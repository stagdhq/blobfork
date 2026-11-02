package azure

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
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
)

// Config is what a fork needs: Azurite, its account, and the containers to
// fork with the layers under them.
type Config struct {
	// Azurite is Azurite's blob endpoint, without the account, e.g.
	// http://127.0.0.1:10010.
	Azurite string

	// Account and Key are the account in Azurite the app uses.
	Account string
	Key     string

	// Containers are the forked containers by their names in Azurite, usually
	// the real ones', each with the real container under it.
	Containers map[string]Layer

	Log *slog.Logger
}

// Fork is the proxy: requests pass through to Azurite, and those on a forked
// container that the fork can't answer alone are answered with the lower
// layer.
type Fork struct {
	Config

	containers map[string]*forked
	hidden     *container.Client
	proxy      *httputil.ReverseProxy
}

// forked is one container blobfork forks: the fork's, in Azurite, and the
// real one under it.
type forked struct {
	name  string
	lower Layer

	// upper is the fork's, listed with a SAS of blobfork's own: the app's
	// request is signed for the app's marker, not the fork's.
	upper  *SASLayer
	client *container.Client

	markers *markers
}

// HiddenContainer holds the delete markers, where the app's listings don't
// see them. blobfork leaves it out of List Containers.
const HiddenContainer = "blobfork-deleted"

// New makes a fork: it waits for Azurite, makes the forked containers and the
// hidden one if they aren't there, and reads the delete markers.
func New(ctx context.Context, cfg Config) (*Fork, error) {
	target, err := url.Parse(strings.TrimRight(cfg.Azurite, "/"))
	if err != nil {
		return nil, fmt.Errorf("azurite: %w", err)
	}

	cred, err := container.NewSharedKeyCredential(cfg.Account, cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("account key: %w", err)
	}

	if len(cfg.Containers) == 0 {
		return nil, errors.New("no container to fork")
	}

	account := target.String() + "/" + cfg.Account

	hidden, err := container.NewClientWithSharedKeyCredential(account+"/"+HiddenContainer, cred, nil)
	if err != nil {
		return nil, err
	}

	f := &Fork{
		Config:     cfg,
		containers: map[string]*forked{},
		hidden:     hidden,
	}

	for name, lower := range cfg.Containers {
		if name == HiddenContainer {
			return nil, fmt.Errorf("%s is blobfork's own container", name)
		}

		client, err := container.NewClientWithSharedKeyCredential(account+"/"+name, cred, nil)
		if err != nil {
			return nil, err
		}

		f.containers[name] = &forked{
			name:   name,
			lower:  lower,
			client: client,
			upper: &SASLayer{
				URL:    account + "/" + name,
				Client: http.DefaultClient,
				SAS: func() string {
					values := sas.BlobSignatureValues{
						Protocol:      sas.ProtocolHTTPSandHTTP,
						ExpiryTime:    time.Now().Add(time.Hour).UTC(),
						Permissions:   (&sas.ContainerPermissions{Read: true, List: true}).String(),
						ContainerName: name,
					}

					query, err := values.SignWithSharedKey(cred)
					if err != nil {
						panic(err) // only with no expiry or permissions, both set above
					}

					return query.Encode()
				},
			},
		}
	}

	f.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)

			// Azurite writes the app's host into listings and copy sources.
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

// start makes the containers, waiting for Azurite to come up, and reads the
// markers.
func (f *Fork) start(ctx context.Context) error {
	deadline := time.Now().Add(time.Minute)

	for {
		_, err := f.hidden.Create(ctx, nil)
		if err == nil || bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
			break
		}

		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("azurite: %w", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-time.After(500 * time.Millisecond):
		}
	}

	for name, c := range f.containers {
		_, err := c.client.Create(ctx, nil)
		if err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
			return fmt.Errorf("container %s: %w", name, err)
		}

		c.markers, err = loadMarkers(ctx, f.hidden, name)
		if err != nil {
			return fmt.Errorf("delete markers of %s: %w", name, err)
		}

		f.Log.Info("blobfork: ready", "container", name, "deleted", c.markers.count())
	}

	return nil
}

// What blobfork steps in for.
const (
	routePass = iota
	routeRead
	routeDelete
	routeList
	routeListContainers
)

type route struct {
	kind int
	c    *forked
	name string
	comp string
}

type routeKey struct{}

// writesInPlace are the comps that change a blob that's there: a blob only
// in the lower layer is copied up first.
var writesInPlace = map[string]bool{
	"metadata":             true,
	"properties":           true,
	"lease":                true,
	"snapshot":             true,
	"tags":                 true,
	"tier":                 true,
	"expiry":               true,
	"immutabilityPolicies": true,
	"legalhold":            true,
}

func (f *Fork) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt := f.route(r)

	if r.Method == http.MethodPut && rt.name != "" {
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

// route says what a request is, for a forked container; anything else
// passes.
func (f *Fork) route(r *http.Request) route {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if parts[0] != f.Account {
		return route{}
	}

	query := r.URL.Query()
	comp := query.Get("comp")

	if len(parts) == 1 || len(parts) == 2 && parts[1] == "" {
		if r.Method == http.MethodGet && comp == "list" {
			return route{kind: routeListContainers}
		}

		return route{}
	}

	c, ok := f.containers[parts[1]]
	if !ok {
		return route{}
	}

	if len(parts) == 2 || parts[2] == "" {
		if r.Method == http.MethodGet && comp == "list" && query.Get("restype") == "container" {
			return route{kind: routeList, c: c}
		}

		return route{}
	}

	rt := route{c: c, name: parts[2], comp: comp}

	// Snapshots and versions are the fork's own.
	if query.Has("snapshot") || query.Has("versionid") {
		return route{}
	}

	switch {
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && (comp == "" || comp == "metadata"):
		rt.kind = routeRead

	case r.Method == http.MethodDelete && comp == "":
		rt.kind = routeDelete
	}

	return rt
}

// copyUpFor copies a lower blob up before a write that needs it in the fork:
// the source of a copy, or the blob a write changes in place.
func (f *Fork) copyUpFor(r *http.Request, rt route) error {
	if source := r.Header.Get("X-Ms-Copy-Source"); source != "" {
		c, name, ok := f.sourceName(source)
		if !ok {
			return nil
		}

		return f.copyUp(r.Context(), c, name)
	}

	if writesInPlace[rt.comp] {
		return f.copyUp(r.Context(), rt.c, rt.name)
	}

	return nil
}

// sourceName is the blob a copy source names, if it's in a forked container:
// path style (/<account>/<container>/<blob>) as blobfork serves it, or the
// account's own host (<account>.blob…/<container>/<blob>).
func (f *Fork) sourceName(source string) (*forked, string, bool) {
	u, err := url.Parse(source)
	if err != nil {
		return nil, "", false
	}

	parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 3)

	if len(parts) == 3 && parts[0] == f.Account && parts[2] != "" {
		c, ok := f.containers[parts[1]]

		return c, parts[2], ok
	}

	if len(parts) >= 2 && strings.HasPrefix(u.Hostname(), f.Account+".") {
		c, ok := f.containers[parts[0]]
		name := strings.Join(parts[1:], "/")

		return c, name, ok && name != ""
	}

	return nil, "", false
}

// modify steps in on Azurite's answer, where the lower layer has a say.
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

	case routeList:
		return f.list(res, rt)

	case routeListContainers:
		return f.listContainers(res)
	}

	return nil
}

// read answers a read the fork can't from the lower layer, unless a marker
// hides it. Azurite answering 404 rather than 403 means the app may read.
func (f *Fork) read(res *http.Response, rt route) error {
	if !blobNotFound(res) || rt.c.markers.has(rt.name) {
		return nil
	}

	req := res.Request

	lower, err := rt.c.lower.Blob(req.Context(), req.Method, rt.name, rt.comp, req.Header)
	if err != nil {
		return err
	}

	res.Body.Close()
	replace(res, lower)

	return nil
}

// delete leaves a marker where the lower layer has the blob, so it stays
// deleted; Azurite has deleted the fork's own copy, if there was one.
func (f *Fork) delete(res *http.Response, rt route) error {
	ctx := res.Request.Context()

	switch {
	case res.StatusCode == http.StatusAccepted:
		if rt.c.markers.has(rt.name) {
			return nil
		}

		below, err := rt.c.inLower(ctx, rt.name)
		if err != nil || !below {
			return err
		}

		return rt.c.markers.add(ctx, rt.name)

	case blobNotFound(res):
		if rt.c.markers.has(rt.name) {
			return nil
		}

		below, err := rt.c.inLower(ctx, rt.name)
		if err != nil || !below {
			return err
		}

		if err := rt.c.markers.add(ctx, rt.name); err != nil {
			return err
		}

		res.Body.Close()

		res.StatusCode = http.StatusAccepted
		res.Status = "202 Accepted"
		res.Body = http.NoBody
		res.ContentLength = 0

		res.Header.Del("X-Ms-Error-Code")
		res.Header.Del("Content-Type")
		res.Header.Set("Content-Length", "0")
		res.Header.Set("X-Ms-Delete-Type-Permanent", "true")
	}

	return nil
}

// list answers List Blobs with both layers merged, once Azurite has said the
// app may list.
func (f *Fork) list(res *http.Response, rt route) error {
	if res.StatusCode != http.StatusOK {
		return nil
	}

	probe, err := io.ReadAll(res.Body)
	res.Body.Close()

	if err != nil {
		return err
	}

	head, err := parseList(probe)
	if err != nil {
		return fmt.Errorf("azurite's listing: %w", err)
	}

	req := res.Request
	query := req.URL.Query()

	max := 5000
	if raw := query.Get("maxresults"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err == nil && n > 0 && n < max {
			max = n
		}
	}

	m := &merge{
		c:         rt.c,
		prefix:    query.Get("prefix"),
		delimiter: query.Get("delimiter"),
		include:   query.Get("include"),
		version:   req.Header.Get("X-Ms-Version"),
		max:       max,
	}

	body, err := m.answer(req.Context(), head, query.Get("marker"), query.Has("maxresults"))
	if errors.Is(err, errBadMarker) {
		answer(res, http.StatusBadRequest, "OutOfRangeInput", "The marker isn't one blobfork gave.")

		return nil
	}

	if err != nil {
		return err
	}

	res.Body = io.NopCloser(bytes.NewReader(body))
	res.ContentLength = int64(len(body))
	res.Header.Set("Content-Length", strconv.Itoa(len(body)))

	return nil
}

// listContainers leaves the hidden container out of List Containers.
func (f *Fork) listContainers(res *http.Response) error {
	if res.StatusCode != http.StatusOK {
		return nil
	}

	body, err := io.ReadAll(res.Body)
	res.Body.Close()

	if err != nil {
		return err
	}

	start := bytes.Index(body, []byte("<Container><Name>"+HiddenContainer+"</Name>"))
	if start >= 0 {
		end := bytes.Index(body[start:], []byte("</Container>"))
		if end >= 0 {
			body = append(body[:start:start], body[start+end+len("</Container>"):]...)
		}
	}

	res.Body = io.NopCloser(bytes.NewReader(body))
	res.ContentLength = int64(len(body))
	res.Header.Set("Content-Length", strconv.Itoa(len(body)))

	return nil
}

// inLower says whether the lower layer has a blob.
func (c *forked) inLower(ctx context.Context, name string) (bool, error) {
	res, err := c.lower.Blob(ctx, http.MethodHead, name, "", http.Header{})
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

// fail answers an error of blobfork's own as Azure would, 500 InternalError.
func (f *Fork) fail(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}

	f.Log.Error("blobfork: request failed", "method", r.Method, "path", r.URL.Path, "err", err)

	body := errorBody("InternalError", "blobfork: "+err.Error())

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("X-Ms-Error-Code", "InternalError")
	w.WriteHeader(http.StatusInternalServerError)
	w.Write(body)
}

func blobNotFound(res *http.Response) bool {
	return res.StatusCode == http.StatusNotFound && res.Header.Get("X-Ms-Error-Code") == string(bloberror.BlobNotFound)
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

// answer makes res an error of blobfork's own, in Azure's shape.
func answer(res *http.Response, status int, code, message string) {
	body := errorBody(code, message)

	res.StatusCode = status
	res.Status = fmt.Sprintf("%d %s", status, http.StatusText(status))
	res.Body = io.NopCloser(bytes.NewReader(body))
	res.ContentLength = int64(len(body))

	res.Header.Set("Content-Type", "application/xml")
	res.Header.Set("Content-Length", strconv.Itoa(len(body)))
	res.Header.Set("X-Ms-Error-Code", code)
}

func errorBody(code, message string) []byte {
	var b bytes.Buffer

	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><Error><Code>`)
	xml.EscapeText(&b, []byte(code))
	b.WriteString("</Code><Message>")
	xml.EscapeText(&b, []byte(message))
	b.WriteString("</Message></Error>")

	return b.Bytes()
}
