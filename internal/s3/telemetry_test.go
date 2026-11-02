package s3

import (
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/stagdhq/blobfork/internal/telemetry"
	"github.com/stagdhq/blobfork/internal/telemetry/telemetrytest"
)

// Each request's span says what blobfork did and what answered, and the
// metrics count reads and bytes from the real bucket, delete markers, merged
// listings, copy-ups and every request.
func TestTelemetry(t *testing.T) {
	recorder := telemetrytest.Start()

	e := setup(t)
	e.seed(t, map[string]string{"a.txt": "lab's", "b.txt": "lab's", "c.txt": "lab's"})

	traced := httptest.NewServer(telemetry.Handler(e.fork))
	t.Cleanup(traced.Close)

	e.app = client(traced.URL, testCredentials)
	ctx := t.Context()

	if _, err := e.read(t, "a.txt"); err != nil {
		t.Fatal(err)
	}

	if span := recorder.Span("read"); span["blobfork.answered_by"] != "lower" || span["blobfork.container"] != e.bucket || span["blobfork.front"] != "s3" {
		t.Fatalf("read span: %v", span)
	}

	e.write(t, "new.txt", "the fork's")

	if span := recorder.Span("pass PUT"); span["blobfork.answered_by"] != "fork" {
		t.Fatalf("write span: %v", span)
	}

	e.remove(t, "b.txt")

	listV2(t, e.app, e.bucket, "", "", 0)

	if span := recorder.Span("list"); span["blobfork.answered_by"] != "merge" {
		t.Fatalf("list span: %v", span)
	}

	_, err := e.app.CopyObject(ctx, &awss3.CopyObjectInput{
		Bucket:     aws.String(e.bucket),
		Key:        aws.String("copy.txt"),
		CopySource: aws.String(e.bucket + "/c.txt"),
	})
	if err != nil {
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
		if got := recorder.Count(t, name, e.bucket); got != want {
			t.Errorf("%s: %d, want %d", name, got, want)
		}
	}

	// Every request was counted and timed, and the real one's answers timed.
	for _, name := range []string{"blobfork.requests", "blobfork.request.duration", "blobfork.lower.duration"} {
		if got := recorder.Count(t, name, e.bucket); got == 0 {
			t.Errorf("%s: none", name)
		}
	}
}
