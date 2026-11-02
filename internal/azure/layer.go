// Package azure is blobfork's Azure front: a proxy in front of Azurite that
// forks one container, with the real container as a read-only layer under it.
package azure

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/stagdhq/blobfork/internal/telemetry"
)

// Layer is a container blobfork reads from: the real one under the fork (the
// lower layer), or the fork itself in Azurite, when blobfork lists it.
//
// The lower layer is read with a SAS of its own for now (SASLayer). In a
// Stagd preview it will be read through Stagd, so the SAS stays there: another
// Layer, on an API Stagd doesn't have yet.
type Layer interface {
	// Blob reads a blob as Get Blob (GET) or Get Blob Properties (HEAD) do,
	// with the range and conditions in header; comp is "" or "metadata". The
	// answer is Azure's, headers and all, and a missing blob is a 404.
	Blob(ctx context.Context, method, name, comp string, header http.Header) (*http.Response, error)

	// List reads one page of List Blobs.
	List(ctx context.Context, q ListQuery) (*Page, error)
}

// ListQuery is what List Blobs is asked: the app's prefix, delimiter and
// include, and the layer's own marker.
type ListQuery struct {
	Prefix     string
	Delimiter  string
	Include    string
	MaxResults int
	Marker     string

	// Version is the x-ms-version to ask with, the app's, so the entries
	// have the shape its SDK expects.
	Version string
}

// Page is one page of a listing, in name order.
type Page struct {
	Entries    []Entry
	NextMarker string
}

// Entry is a Blob or a BlobPrefix of a listing, its XML kept as the layer
// answered, to be passed on unchanged.
type Entry struct {
	Name   string
	Prefix bool
	XML    []byte
}

// headersToLower are the request headers a read passes to the lower layer:
// the range and the conditions, nothing that authorizes.
var headersToLower = []string{
	"Range",
	"X-Ms-Range",
	"X-Ms-Range-Get-Content-Md5",
	"X-Ms-Range-Get-Content-Crc64",
	"If-Match",
	"If-None-Match",
	"If-Modified-Since",
	"If-Unmodified-Since",
	"X-Ms-Version",
}

// SASLayer is a container read with a SAS: the real one with a read-only SAS
// (BLOBFORK_FROM), or the fork in Azurite with one blobfork signs itself.
type SASLayer struct {
	// URL is the container's, without the SAS.
	URL string

	// SAS is the SAS's query string, asked for on every request.
	SAS func() string

	Client *http.Client
}

// NewSASLayer reads a container's SAS URL, as BLOBFORK_FROM has it.
func NewSASLayer(raw string) (*SASLayer, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}

	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("not a container URL: %s", u.Redacted())
	}

	if u.Query().Get("sig") == "" {
		return nil, fmt.Errorf("no SAS in the container URL (sig=…)")
	}

	sas := u.RawQuery

	u.RawQuery = ""
	u.Fragment = ""

	return &SASLayer{
		URL:    strings.TrimRight(u.String(), "/"),
		SAS:    func() string { return sas },
		Client: telemetry.Client,
	}, nil
}

// Container is the container's name, the last part of its URL.
func (l *SASLayer) Container() string {
	return l.URL[strings.LastIndex(l.URL, "/")+1:]
}

func (l *SASLayer) Blob(ctx context.Context, method, name, comp string, header http.Header) (*http.Response, error) {
	target := l.URL + "/" + escapeName(name) + "?" + l.SAS()
	if comp != "" {
		target += "&comp=" + url.QueryEscape(comp)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}

	for _, key := range headersToLower {
		if value := header.Get(key); value != "" {
			req.Header.Set(key, value)
		}
	}

	return l.Client.Do(req)
}

func (l *SASLayer) List(ctx context.Context, q ListQuery) (*Page, error) {
	query := url.Values{}
	query.Set("restype", "container")
	query.Set("comp", "list")

	if q.Prefix != "" {
		query.Set("prefix", q.Prefix)
	}

	if q.Delimiter != "" {
		query.Set("delimiter", q.Delimiter)
	}

	if q.Include != "" {
		query.Set("include", q.Include)
	}

	if q.MaxResults > 0 {
		query.Set("maxresults", strconv.Itoa(q.MaxResults))
	}

	if q.Marker != "" {
		query.Set("marker", q.Marker)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.URL+"?"+query.Encode()+"&"+l.SAS(), nil)
	if err != nil {
		return nil, err
	}

	if q.Version != "" {
		req.Header.Set("X-Ms-Version", q.Version)
	}

	res, err := l.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list %s: %s %s", l.URL, res.Status, res.Header.Get("X-Ms-Error-Code"))
	}

	list, err := parseList(body)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", l.URL, err)
	}

	return list.page()
}

// escapeName escapes a blob's name for its URL, keeping the slashes.
func escapeName(name string) string {
	return (&url.URL{Path: name}).EscapedPath()
}

// enumeration is a List Blobs answer, its entries kept as they came.
type enumeration struct {
	XMLName         xml.Name `xml:"EnumerationResults"`
	ServiceEndpoint string   `xml:"ServiceEndpoint,attr"`
	ContainerName   string   `xml:"ContainerName,attr"`

	Blobs struct {
		Items []rawElement `xml:",any"`
	} `xml:"Blobs"`

	NextMarker string `xml:"NextMarker"`
}

type rawElement struct {
	XMLName xml.Name
	Inner   []byte `xml:",innerxml"`
}

func parseList(body []byte) (*enumeration, error) {
	var list enumeration

	if err := xml.Unmarshal(body, &list); err != nil {
		return nil, err
	}

	return &list, nil
}

func (e *enumeration) page() (*Page, error) {
	page := &Page{NextMarker: e.NextMarker}

	for _, item := range e.Blobs.Items {
		tag := item.XMLName.Local

		raw := []byte("<" + tag + ">" + string(item.Inner) + "</" + tag + ">")

		var named struct {
			Name struct {
				Encoded bool   `xml:"Encoded,attr"`
				Value   string `xml:",chardata"`
			} `xml:"Name"`
		}

		if err := xml.Unmarshal(raw, &named); err != nil {
			return nil, err
		}

		// A name with characters XML can't hold comes URL-encoded.
		name := named.Name.Value
		if named.Name.Encoded {
			decoded, err := url.PathUnescape(name)
			if err != nil {
				return nil, err
			}

			name = decoded
		}

		page.Entries = append(page.Entries, Entry{Name: name, Prefix: tag == "BlobPrefix", XML: raw})
	}

	return page, nil
}
