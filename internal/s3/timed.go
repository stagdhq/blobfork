package s3

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/stagdhq/blobfork/internal/telemetry"
)

// timed is a lower layer whose requests are timed (blobfork.lower.duration),
// and whose bodies read for the app or a copy-up are counted
// (blobfork.lower.bytes).
type timed struct {
	Layer
	bucket string
}

func (l timed) Object(ctx context.Context, method, key string, query url.Values, header http.Header) (*http.Response, error) {
	started := time.Now()

	res, err := l.Layer.Object(ctx, method, key, query, header)

	operation := "read"
	if method == http.MethodHead {
		operation = "head"
	}

	telemetry.Lower(ctx, "s3", l.bucket, operation, started)

	if err == nil && method == http.MethodGet && res.ContentLength > 0 && res.StatusCode/100 == 2 {
		telemetry.LowerBytes.Add(ctx, res.ContentLength, telemetry.Attrs("s3", l.bucket))
	}

	return res, err
}

func (l timed) List(ctx context.Context, q ListQuery) (*Page, error) {
	started := time.Now()

	page, err := l.Layer.List(ctx, q)

	telemetry.Lower(ctx, "s3", l.bucket, "list", started)

	return page, err
}
