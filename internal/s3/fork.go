package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// Config is what a fork needs: Floci, the credentials the app signs with,
// and the bucket to fork with the layer under it.
type Config struct {
	// Floci is Floci's endpoint, e.g. http://127.0.0.1:4567. It must check
	// signatures (FLOCI_AUTH_VALIDATE_SIGNATURES=true): a request Floci
	// answers 404 is one the app may read through.
	Floci string

	// Credentials and Region are what the app signs with, and blobfork too.
	Credentials aws.Credentials
	Region      string

	// Bucket is the forked bucket's name in Floci, usually the real one's.
	Bucket string

	// Lower is the real bucket.
	Lower Layer

	Log *slog.Logger
}

// Fork is the proxy: requests pass through to Floci, and those on the forked
// bucket that the fork can't answer alone are answered with the lower layer.
type Fork struct {
	Config

	upper   *Bucket
	hidden  *Bucket
	markers *markers
	proxy   *httputil.ReverseProxy
}

// HiddenBucket holds the delete markers, where the app's listings don't see
// them. blobfork leaves it out of ListBuckets.
const HiddenBucket = "blobfork-deleted"

// New makes a fork: it waits for Floci, makes the forked bucket and the
// hidden one if they aren't there, and reads the delete markers.
func New(ctx context.Context, cfg Config) (*Fork, error) {
	target, err := url.Parse(strings.TrimRight(cfg.Floci, "/"))
	if err != nil {
		return nil, fmt.Errorf("floci: %w", err)
	}

	bucket := func(name string) *Bucket {
		return &Bucket{
			Endpoint:    target.String(),
			Name:        name,
			Region:      cfg.Region,
			PathStyle:   true,
			Credentials: cfg.Credentials,
		}
	}

	f := &Fork{
		Config: cfg,
		upper:  bucket(cfg.Bucket),
		hidden: bucket(HiddenBucket),
	}

	f.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)

			// SigV4 signs the host: Floci checks the app's.
			r.Out.Host = r.In.Host
		},
		ModifyResponse: f.modify,
		ErrorHandler:   f.fail,
	}

	if err := f.start(ctx); err != nil {
		return nil, err
	}

	return f, nil
}

// start makes both buckets, waiting for Floci to come up, and reads the
// markers.
func (f *Fork) start(ctx context.Context) error {
	deadline := time.Now().Add(time.Minute)

	for {
		err := f.upper.create(ctx)
		if err == nil {
			break
		}

		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("floci: %w", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-time.After(500 * time.Millisecond):
		}
	}

	if err := f.hidden.create(ctx); err != nil {
		return fmt.Errorf("hidden bucket: %w", err)
	}

	var err error

	f.markers, err = loadMarkers(ctx, f.hidden, f.Bucket)
	if err != nil {
		return fmt.Errorf("delete markers: %w", err)
	}

	f.Log.Info("blobfork: ready", "bucket", f.Bucket, "deleted", f.markers.count())

	return nil
}

// create makes the bucket, unless it's there.
func (b *Bucket) create(ctx context.Context) error {
	res, err := b.Do(ctx, http.MethodPut, "", nil, nil, nil, 0)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	body, _ := io.ReadAll(res.Body)

	code := errorCode(body)
	if res.StatusCode == http.StatusOK || code == "BucketAlreadyOwnedByYou" || code == "BucketAlreadyExists" {
		return nil
	}

	return fmt.Errorf("create bucket %s: %s %s", b.Name, res.Status, code)
}

// What blobfork steps in for.
const (
	routePass = iota
	routeRead
	routeDelete
	routeDeleteMany
	routeList
	routeListBuckets
)

type route struct {
	kind int
	key  string

	// v1 is a ListObjects of the first version (marker, not start-after).
	v1 bool

	// keys are a DeleteObjects' keys.
	keys []string
}

type routeKey struct{}

// readQuery is what a read may have in its query and still be a plain read
// of the object: the response overrides, the part, and the signature.
func readQuery(key string) bool {
	return strings.HasPrefix(key, "response-") || strings.HasPrefix(strings.ToLower(key), "x-amz-") || key == "partNumber" || key == "x-id"
}

// listQueryV1 is what ListObjects (the first version) has in its query.
var listQueryV1 = map[string]bool{"prefix": true, "delimiter": true, "marker": true, "max-keys": true, "encoding-type": true}

// writesInPlace are the subresources whose writes change an object that's
// there: an object only in the lower layer is copied up first.
var writesInPlace = []string{"tagging", "acl", "retention", "legal-hold"}

func (f *Fork) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Floci takes unsigned requests; blobfork doesn't, as a request Floci
	// answers is one the app may read through. A browser's preflight and a
	// form upload (which signs its policy) pass.
	if !signed(r) && r.Method != http.MethodOptions && !formUpload(r) {
		f.refuse(w, http.StatusForbidden, "AccessDenied", "blobfork takes requests signed with SigV4.")

		return
	}

	rt := f.route(r)

	if rt.kind == routeDeleteMany {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		r.Body.Close()

		if err != nil {
			f.fail(w, r, err)

			return
		}

		var request struct {
			Objects []struct {
				Key string `xml:"Key"`
			} `xml:"Object"`
		}

		xml.Unmarshal(body, &request)

		for _, object := range request.Objects {
			rt.keys = append(rt.keys, object.Key)
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}

	if r.Method == http.MethodPut && rt.key != "" {
		if err := f.copyUpFor(r, rt); err != nil {
			f.fail(w, r, err)

			return
		}
	}

	if rt.kind != routePass {
		r = r.WithContext(context.WithValue(r.Context(), routeKey{}, rt))
	}

	f.proxy.ServeHTTP(w, r)
}

func signed(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") || r.URL.Query().Get("X-Amz-Signature") != ""
}

func formUpload(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data")
}

// route says what a request is, for the forked bucket (path style); anything
// else passes.
func (f *Fork) route(r *http.Request) route {
	query := r.URL.Query()

	if r.URL.Path == "/" || r.URL.Path == "" {
		if r.Method == http.MethodGet && !query.Has("list-type") {
			return route{kind: routeListBuckets}
		}

		return route{}
	}

	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != f.Bucket {
		return route{}
	}

	if key == "" {
		switch {
		case r.Method == http.MethodGet && query.Get("list-type") == "2":
			return route{kind: routeList}

		case r.Method == http.MethodGet && onlyQuery(query, func(k string) bool { return listQueryV1[k] || readQuery(k) }):
			return route{kind: routeList, v1: true}

		case r.Method == http.MethodPost && query.Has("delete"):
			return route{kind: routeDeleteMany}
		}

		return route{}
	}

	rt := route{key: key}

	switch {
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && onlyQuery(query, readQuery):
		rt.kind = routeRead

	case r.Method == http.MethodDelete && onlyQuery(query, readQuery):
		rt.kind = routeDelete
	}

	return rt
}

func onlyQuery(query url.Values, allowed func(string) bool) bool {
	for key := range query {
		if !allowed(key) {
			return false
		}
	}

	return true
}

// copyUpFor copies a lower object up before a write that needs it in the
// fork: the source of a copy, or the object a write changes in place.
func (f *Fork) copyUpFor(r *http.Request, rt route) error {
	if source := r.Header.Get("X-Amz-Copy-Source"); source != "" {
		key, ok := f.sourceKey(source)
		if !ok {
			return nil
		}

		return f.copyUp(r.Context(), key)
	}

	query := r.URL.Query()

	for _, sub := range writesInPlace {
		if query.Has(sub) {
			return f.copyUp(r.Context(), rt.key)
		}
	}

	return nil
}

// sourceKey is the object x-amz-copy-source names, if it's in the forked
// bucket and not a version of it: "/<bucket>/<key>" or "<bucket>/<key>",
// URL-encoded.
func (f *Fork) sourceKey(source string) (string, bool) {
	path, version, _ := strings.Cut(source, "?")
	if version != "" {
		return "", false
	}

	decoded, err := url.PathUnescape(path)
	if err != nil {
		return "", false
	}

	bucket, key, _ := strings.Cut(strings.TrimPrefix(decoded, "/"), "/")

	return key, bucket == f.Bucket && key != ""
}

// modify steps in on Floci's answer, where the lower layer has a say.
func (f *Fork) modify(res *http.Response) error {
	rt, ok := res.Request.Context().Value(routeKey{}).(route)
	if !ok {
		return nil
	}

	switch rt.kind {
	case routeRead:
		return f.read(res, rt)

	case routeDelete:
		return f.delete(res, rt)

	case routeDeleteMany:
		return f.deleteMany(res, rt)

	case routeList:
		return f.list(res, rt)

	case routeListBuckets:
		return f.listBuckets(res)
	}

	return nil
}

// read answers a read the fork can't from the lower layer, unless a marker
// hides it. Floci checks the signature first, so a 404 means the app may
// read.
func (f *Fork) read(res *http.Response, rt route) error {
	if res.StatusCode != http.StatusNotFound || f.markers.has(rt.key) {
		return nil
	}

	req := res.Request

	// A HEAD's 404 has no body to say why; the bucket is there, so it's
	// the key.
	if req.Method == http.MethodGet {
		body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		res.Body.Close()

		if err != nil {
			return err
		}

		if errorCode(body) != "NoSuchKey" {
			res.Body = io.NopCloser(bytes.NewReader(body))

			return nil
		}
	}

	query := url.Values{}

	for key, values := range req.URL.Query() {
		if strings.HasPrefix(key, "response-") || key == "partNumber" {
			query[key] = values
		}
	}

	lower, err := f.Lower.Object(req.Context(), req.Method, rt.key, query, req.Header)
	if err != nil {
		return err
	}

	res.Body.Close()
	replace(res, lower)

	return nil
}

// delete leaves a marker where the lower layer has the object, so it stays
// deleted. Floci answers 204 either way, as S3 does.
func (f *Fork) delete(res *http.Response, rt route) error {
	if res.StatusCode/100 != 2 {
		return nil
	}

	return f.hide(res.Request.Context(), rt.key)
}

// deleteMany leaves markers for the keys of a DeleteObjects that Floci
// deleted, or had nothing to delete for.
func (f *Fork) deleteMany(res *http.Response, rt route) error {
	if res.StatusCode != http.StatusOK {
		return nil
	}

	body, err := io.ReadAll(res.Body)
	res.Body.Close()

	if err != nil {
		return err
	}

	res.Body = io.NopCloser(bytes.NewReader(body))

	var result struct {
		Errors []struct {
			Key string `xml:"Key"`
		} `xml:"Error"`
	}

	xml.Unmarshal(body, &result)

	failed := map[string]bool{}
	for _, e := range result.Errors {
		failed[e.Key] = true
	}

	ctx := res.Request.Context()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		slots = make(chan struct{}, 16)
	)

	for _, key := range rt.keys {
		if failed[key] {
			continue
		}

		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()

			if err := f.hide(ctx, key); err != nil {
				mu.Lock()
				first = cmpOr(first, err)
				mu.Unlock()
			}
		})
	}

	wg.Wait()

	return first
}

// hide leaves a marker for a key the lower layer has.
func (f *Fork) hide(ctx context.Context, key string) error {
	if f.markers.has(key) {
		return nil
	}

	below, err := f.inLower(ctx, key)
	if err != nil || !below {
		return err
	}

	return f.markers.add(ctx, key)
}

// list answers ListObjects with both layers merged, once Floci has said the
// app may list.
func (f *Fork) list(res *http.Response, rt route) error {
	if res.StatusCode != http.StatusOK {
		return nil
	}

	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	req := res.Request
	query := req.URL.Query()

	max := 1000
	if raw := query.Get("max-keys"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err == nil && n >= 0 && n < max {
			max = n
		}
	}

	m := &merge{
		fork:       f,
		v1:         rt.v1,
		bucket:     f.Bucket,
		prefix:     query.Get("prefix"),
		delimiter:  query.Get("delimiter"),
		encoding:   query.Get("encoding-type"),
		fetchOwner: rt.v1 || query.Get("fetch-owner") == "true",
		attributes: req.Header.Get("X-Amz-Optional-Object-Attributes"),
		max:        max,
	}

	body, err := m.answer(req.Context(), query)
	if errors.Is(err, errBadToken) {
		answer(res, http.StatusBadRequest, "InvalidArgument", "The continuation token provided is incorrect")

		return nil
	}

	if err != nil {
		return err
	}

	res.Body = io.NopCloser(bytes.NewReader(body))
	res.ContentLength = int64(len(body))
	res.Header.Set("Content-Length", strconv.Itoa(len(body)))
	res.Header.Set("Content-Type", "application/xml")

	return nil
}

// listBuckets leaves the hidden bucket out of ListBuckets.
func (f *Fork) listBuckets(res *http.Response) error {
	if res.StatusCode != http.StatusOK {
		return nil
	}

	body, err := io.ReadAll(res.Body)
	res.Body.Close()

	if err != nil {
		return err
	}

	start := bytes.Index(body, []byte("<Bucket><Name>"+HiddenBucket+"</Name>"))
	if start >= 0 {
		end := bytes.Index(body[start:], []byte("</Bucket>"))
		if end >= 0 {
			body = append(body[:start:start], body[start+end+len("</Bucket>"):]...)
		}
	}

	res.Body = io.NopCloser(bytes.NewReader(body))
	res.ContentLength = int64(len(body))
	res.Header.Set("Content-Length", strconv.Itoa(len(body)))

	return nil
}

// inLower says whether the lower layer has an object.
func (f *Fork) inLower(ctx context.Context, key string) (bool, error) {
	res, err := f.Lower.Object(ctx, http.MethodHead, key, nil, http.Header{})
	if err != nil {
		return false, err
	}

	res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK:
		return true, nil

	case http.StatusNotFound:
		return false, nil
	}

	return false, fmt.Errorf("lower layer: %s", res.Status)
}

// fail answers an error of blobfork's own as S3 would, 500 InternalError.
func (f *Fork) fail(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}

	f.Log.Error("blobfork: request failed", "method", r.Method, "path", r.URL.Path, "err", err)

	f.refuse(w, http.StatusInternalServerError, "InternalError", "blobfork: "+err.Error())
}

func (f *Fork) refuse(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	w.Write(errorBody(code, message))
}

// replace makes res the lower layer's answer.
func replace(res, lower *http.Response) {
	header := lower.Header.Clone()

	for _, hop := range []string{"Connection", "Keep-Alive", "Proxy-Connection", "Transfer-Encoding", "Upgrade"} {
		header.Del(hop)
	}

	res.StatusCode = lower.StatusCode
	res.Status = lower.Status
	res.Header = header
	res.Body = lower.Body
	res.ContentLength = lower.ContentLength
}

// answer makes res an error of blobfork's own, in S3's shape.
func answer(res *http.Response, status int, code, message string) {
	body := errorBody(code, message)

	res.StatusCode = status
	res.Status = fmt.Sprintf("%d %s", status, http.StatusText(status))
	res.Body = io.NopCloser(bytes.NewReader(body))
	res.ContentLength = int64(len(body))

	res.Header.Set("Content-Type", "application/xml")
	res.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

func errorBody(code, message string) []byte {
	var b bytes.Buffer

	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>`)
	xml.EscapeText(&b, []byte(code))
	b.WriteString("</Code><Message>")
	xml.EscapeText(&b, []byte(message))
	b.WriteString("</Message></Error>")

	return b.Bytes()
}

func cmpOr(a, b error) error {
	if a != nil {
		return a
	}

	return b
}
