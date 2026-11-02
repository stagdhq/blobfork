// Package s3 is blobfork's S3 front: a proxy in front of Floci that forks one
// bucket, with the real bucket as a read-only layer under it.
package s3

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// Layer is a bucket blobfork reads from: the real one under the fork (the
// lower layer), or the fork itself in Floci, when blobfork lists it.
//
// The lower layer is read with credentials of its own for now (Bucket). In a
// Stagd preview it will be read through Stagd, so the credentials stay there:
// another Layer, on an API Stagd doesn't have yet.
type Layer interface {
	// Object reads an object as GetObject (GET) or HeadObject (HEAD) do, with
	// the range and conditions in header and the response-* overrides in
	// query. The answer is S3's, headers and all, and a missing object is a
	// 404.
	Object(ctx context.Context, method, key string, query url.Values, header http.Header) (*http.Response, error)

	// List reads one page of ListObjectsV2.
	List(ctx context.Context, q ListQuery) (*Page, error)
}

// ListQuery is what ListObjectsV2 is asked: the app's prefix, delimiter and
// options, and where the page starts.
type ListQuery struct {
	Prefix       string
	Delimiter    string
	EncodingType string
	FetchOwner   bool
	MaxKeys      int

	// StartAfter starts the listing after a name; Token, a page the layer
	// gave, wins over it.
	StartAfter string
	Token      string

	// Attributes is x-amz-optional-object-attributes, the app's.
	Attributes string
}

// Page is one page of a listing, its objects and prefixes in name order.
type Page struct {
	Entries []Entry
	Next    string
}

// Entry is a Contents or a CommonPrefixes of a listing, its XML kept as the
// layer answered, to be passed on unchanged.
type Entry struct {
	Name   string
	Prefix bool
	XML    []byte
}

// headersToLower are the request headers a read passes to the lower layer:
// the range and the conditions, nothing that authorizes.
var headersToLower = []string{
	"Range",
	"If-Match",
	"If-None-Match",
	"If-Modified-Since",
	"If-Unmodified-Since",
}

// Bucket is a bucket read and written with SigV4 credentials: the real one,
// with read-only credentials of its own, or the fork in Floci.
type Bucket struct {
	// Endpoint is the S3 endpoint, e.g. https://s3.eu-north-1.amazonaws.com,
	// https://<account>.r2.cloudflarestorage.com or http://127.0.0.1:4567.
	Endpoint string

	Name   string
	Region string

	// PathStyle addresses the bucket in the path (<endpoint>/<bucket>/<key>)
	// rather than the host (<bucket>.<endpoint host>/<key>).
	PathStyle bool

	Credentials aws.Credentials

	Client *http.Client

	signer *v4.Signer
}

// unsignedPayload is what blobfork signs a body as: S3 takes it over TLS
// and plain HTTP alike, and the body needn't be read twice.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// Do sends a request for the bucket (key "") or one of its objects, signed.
func (b *Bucket) Do(ctx context.Context, method, key string, query url.Values, header http.Header, body io.Reader, size int64) (*http.Response, error) {
	if b.signer == nil {
		// S3 signs the path as it's sent, escaped once.
		b.signer = v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	}

	target, err := url.Parse(strings.TrimRight(b.Endpoint, "/"))
	if err != nil {
		return nil, err
	}

	path := "/"
	if b.PathStyle {
		path += b.Name + "/"
	} else {
		target.Host = b.Name + "." + target.Host
	}

	path += key

	target.Path = path
	target.RawPath = escapePath(path)
	target.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20")

	// An empty body is sent as none, not chunked.
	if body != nil && size == 0 {
		body = http.NoBody
	}

	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}

	for key, values := range header {
		req.Header[key] = values
	}

	if body != nil {
		req.ContentLength = size
	}

	req.Header.Set("X-Amz-Content-Sha256", unsignedPayload)

	err = b.signer.SignHTTP(ctx, b.Credentials, req, unsignedPayload, "s3", b.Region, time.Now())
	if err != nil {
		return nil, err
	}

	client := b.Client
	if client == nil {
		client = http.DefaultClient
	}

	return client.Do(req)
}

func (b *Bucket) Object(ctx context.Context, method, key string, query url.Values, header http.Header) (*http.Response, error) {
	forward := http.Header{}

	for _, name := range headersToLower {
		if value := header.Get(name); value != "" {
			forward.Set(name, value)
		}
	}

	return b.Do(ctx, method, key, query, forward, nil, 0)
}

func (b *Bucket) List(ctx context.Context, q ListQuery) (*Page, error) {
	query := url.Values{}
	query.Set("list-type", "2")

	if q.Prefix != "" {
		query.Set("prefix", q.Prefix)
	}

	if q.Delimiter != "" {
		query.Set("delimiter", q.Delimiter)
	}

	if q.EncodingType != "" {
		query.Set("encoding-type", q.EncodingType)
	}

	if q.FetchOwner {
		query.Set("fetch-owner", "true")
	}

	if q.MaxKeys > 0 {
		query.Set("max-keys", strconv.Itoa(q.MaxKeys))
	}

	if q.Token != "" {
		query.Set("continuation-token", q.Token)
	} else if q.StartAfter != "" {
		query.Set("start-after", q.StartAfter)
	}

	header := http.Header{}
	if q.Attributes != "" {
		header.Set("X-Amz-Optional-Object-Attributes", q.Attributes)
	}

	res, err := b.Do(ctx, http.MethodGet, "", query, header, nil, 0)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list %s: %s %s", b.Name, res.Status, errorCode(body))
	}

	list, err := parseList(body)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", b.Name, err)
	}

	return list.page()
}

// escapePath escapes a path as SigV4 has it: everything but the unreserved
// characters and the slashes.
func escapePath(path string) string {
	var b strings.Builder

	for i := 0; i < len(path); i++ {
		c := path[i]

		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || strings.IndexByte("-._~/", c) >= 0 {
			b.WriteByte(c)

			continue
		}

		fmt.Fprintf(&b, "%%%02X", c)
	}

	return b.String()
}

// listResult is a ListObjects (V1 or V2) answer, its entries kept as they
// came.
type listResult struct {
	XMLName xml.Name `xml:"ListBucketResult"`

	Contents       []rawElement `xml:"Contents"`
	CommonPrefixes []rawElement `xml:"CommonPrefixes"`

	EncodingType          string `xml:"EncodingType"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

type rawElement struct {
	Inner []byte `xml:",innerxml"`
}

func parseList(body []byte) (*listResult, error) {
	var list listResult

	if err := xml.Unmarshal(body, &list); err != nil {
		return nil, err
	}

	return &list, nil
}

func (l *listResult) page() (*Page, error) {
	page := &Page{}

	if l.IsTruncated {
		page.Next = l.NextContinuationToken
	}

	add := func(items []rawElement, tag, field string) error {
		for _, item := range items {
			raw := []byte("<" + tag + ">" + string(item.Inner) + "</" + tag + ">")

			var named struct {
				Key    string `xml:"Key"`
				Prefix string `xml:"Prefix"`
			}

			if err := xml.Unmarshal(raw, &named); err != nil {
				return err
			}

			name := named.Key
			if field == "Prefix" {
				name = named.Prefix
			}

			// With encoding-type=url, names come URL-encoded.
			if l.EncodingType == "url" {
				decoded, err := url.QueryUnescape(name)
				if err != nil {
					return err
				}

				name = decoded
			}

			page.Entries = append(page.Entries, Entry{Name: name, Prefix: tag == "CommonPrefixes", XML: raw})
		}

		return nil
	}

	if err := add(l.Contents, "Contents", "Key"); err != nil {
		return nil, err
	}

	if err := add(l.CommonPrefixes, "CommonPrefixes", "Prefix"); err != nil {
		return nil, err
	}

	return page, nil
}

// errorCode is the Code of an S3 error body, if it is one.
func errorCode(body []byte) string {
	var e struct {
		Code string `xml:"Code"`
	}

	xml.Unmarshal(body, &e)

	return e.Code
}
