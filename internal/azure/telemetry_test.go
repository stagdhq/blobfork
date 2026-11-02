package azure

import (
	"net/http/httptest"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/stagdhq/blobfork/internal/telemetry"
	"github.com/stagdhq/blobfork/internal/telemetry/telemetrytest"
)

// Each request's span says what blobfork did and what answered, and the
// metrics count reads and bytes from the real container, delete markers,
// merged listings, copy-ups and every request.
func TestTelemetry(t *testing.T) {
	recorder := telemetrytest.Start()

	e := setup(t)
	e.seed(t, map[string]string{"a.txt": "lab's", "b.txt": "lab's", "c.txt": "lab's"})

	traced := httptest.NewServer(telemetry.Handler(e.fork))
	t.Cleanup(traced.Close)

	cred, _ := container.NewSharedKeyCredential("blobfork", forkKey)

	app, err := container.NewClientWithSharedKeyCredential(traced.URL+"/blobfork/"+e.name, cred, nil)
	if err != nil {
		t.Fatal(err)
	}

	e.app = app
	ctx := t.Context()

	if _, err := e.read(t, "a.txt"); err != nil {
		t.Fatal(err)
	}

	if span := recorder.Span("read"); span["blobfork.answered_by"] != "lower" || span["blobfork.container"] != e.name || span["blobfork.front"] != "azure" {
		t.Fatalf("read span: %v", span)
	}

	e.write(t, "new.txt", "the fork's")

	if span := recorder.Span("pass PUT"); span["blobfork.answered_by"] != "fork" {
		t.Fatalf("write span: %v", span)
	}

	if _, err := app.NewBlobClient("b.txt").Delete(ctx, nil); err != nil {
		t.Fatal(err)
	}

	listFlat(t, app, 0)

	if span := recorder.Span("list"); span["blobfork.answered_by"] != "merge" {
		t.Fatalf("list span: %v", span)
	}

	if _, err := app.NewBlobClient("copy.txt").StartCopyFromURL(ctx, app.NewBlobClient("c.txt").URL(), nil); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]int64{
		"blobfork.lower.reads":     1,
		"blobfork.deletes.hidden":  1,
		"blobfork.listings.merged": 1,
		"blobfork.copy_ups":        1,
		"blobfork.copy_ups.bytes":  5,
		"blobfork.lower.bytes":     10,
		"blobfork.deletes.markers": 1,
	} {
		if got := recorder.Count(t, name, e.name); got != want {
			t.Errorf("%s: %d, want %d", name, got, want)
		}
	}

	// Every request was counted and timed, and the real one's answers timed.
	for _, name := range []string{"blobfork.requests", "blobfork.request.duration", "blobfork.lower.duration"} {
		if got := recorder.Count(t, name, e.name); got == 0 {
			t.Errorf("%s: none", name)
		}
	}
}
