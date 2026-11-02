package azure

import (
	"context"
	"net/http"
	"time"

	"github.com/stagdhq/blobfork/internal/telemetry"
)

// timed is a lower layer whose requests are timed (blobfork.lower.duration),
// and whose bodies read for the app or a copy-up are counted
// (blobfork.lower.bytes).
type timed struct {
	Layer
	container string
}

func (l timed) Blob(ctx context.Context, method, name, comp string, header http.Header) (*http.Response, error) {
	started := time.Now()

	res, err := l.Layer.Blob(ctx, method, name, comp, header)

	operation := "read"
	if method == http.MethodHead {
		operation = "head"
	}

	telemetry.Lower(ctx, "azure", l.container, operation, started)

	if err == nil && method == http.MethodGet && res.ContentLength > 0 && res.StatusCode/100 == 2 {
		telemetry.LowerBytes.Add(ctx, res.ContentLength, telemetry.Attrs("azure", l.container))
	}

	return res, err
}

func (l timed) List(ctx context.Context, q ListQuery) (*Page, error) {
	started := time.Now()

	page, err := l.Layer.List(ctx, q)

	telemetry.Lower(ctx, "azure", l.container, "list", started)

	return page, err
}
