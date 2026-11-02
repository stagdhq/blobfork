package s3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/stagdhq/blobfork/internal/telemetry"
)

// copyHeaders are the object's headers a copy-up keeps, besides its metadata
// (x-amz-meta-*).
var copyHeaders = []string{
	"Content-Type",
	"Content-Encoding",
	"Content-Language",
	"Content-Disposition",
	"Cache-Control",
	"Expires",
	"X-Amz-Website-Redirect-Location",
}

// copyUp copies a lower object into the fork, unless the fork has it or a
// marker hides it: its body, content headers and metadata.
//
// It doesn't overwrite (If-None-Match: *): a write the app made meanwhile
// wins.
func (f *Fork) copyUp(ctx context.Context, b *forked, key string) error {
	if b.markers.has(key) {
		return nil
	}

	head, err := b.upper.Do(ctx, http.MethodHead, key, nil, nil, nil, 0)
	if err != nil {
		return fmt.Errorf("copy up %s: %w", key, err)
	}

	head.Body.Close()

	if head.StatusCode == http.StatusOK {
		return nil
	}

	if head.StatusCode != http.StatusNotFound {
		return fmt.Errorf("copy up %s: fork: %s", key, head.Status)
	}

	res, err := b.lower.Object(ctx, http.MethodGet, key, nil, http.Header{})
	if err != nil {
		return fmt.Errorf("copy up %s: %w", key, err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotFound {
		return nil
	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("copy up %s: lower layer: %s", key, res.Status)
	}

	header := http.Header{}
	header.Set("If-None-Match", "*")

	for _, name := range copyHeaders {
		if value := res.Header.Get(name); value != "" {
			header.Set(name, value)
		}
	}

	for name, values := range res.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") {
			header[name] = values
		}
	}

	put, err := b.upper.Do(ctx, http.MethodPut, key, nil, header, res.Body, res.ContentLength)
	if err != nil {
		return fmt.Errorf("copy up %s: %w", key, err)
	}
	defer put.Body.Close()

	body, _ := io.ReadAll(put.Body)

	switch {
	case put.StatusCode == http.StatusOK:
		f.Log.Debug("blobfork: copied up", "bucket", b.name, "key", key)
		telemetry.CopyUps.Add(ctx, 1, telemetry.Attrs("s3", b.name))
		telemetry.CopyUpBytes.Add(ctx, max(res.ContentLength, 0), telemetry.Attrs("s3", b.name))

		return nil

	case put.StatusCode == http.StatusPreconditionFailed || errorCode(body) == "ConditionalRequestConflict":
		return nil
	}

	return fmt.Errorf("copy up %s: %s %s", key, put.Status, errorCode(body))
}
