package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// The tests run the AWS SDK against blobfork in front of Floci, with a second
// Floci holding the real bucket. BLOBFORK_TEST_FLOCI and
// BLOBFORK_TEST_FLOCI_LAB name running ones (both checking signatures, with
// the keys test/test); otherwise the tests start them with Docker.

const flociImage = "floci/floci:2.4.0"

var (
	floci, flociLab string
	sequence        atomic.Int64

	testCredentials = aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}
)

func TestMain(m *testing.M) {
	floci = os.Getenv("BLOBFORK_TEST_FLOCI")
	flociLab = os.Getenv("BLOBFORK_TEST_FLOCI_LAB")

	if floci == "" || flociLab == "" {
		var stops []func()

		for _, target := range []*string{&floci, &flociLab} {
			endpoint, stop, err := startFloci()
			if err != nil {
				fmt.Fprintln(os.Stderr, "blobfork tests need Floci:", err)
				os.Exit(1)
			}

			*target = endpoint
			stops = append(stops, stop)
		}

		code := m.Run()

		for _, stop := range stops {
			stop()
		}

		os.Exit(code)
	}

	os.Exit(m.Run())
}

func startFloci() (string, func(), error) {
	out, err := exec.Command(
		"docker", "run", "-d",
		"-p", "127.0.0.1::4566",
		"-e", "FLOCI_AUTH_VALIDATE_SIGNATURES=true",
		"-e", "FLOCI_STORAGE_MODE=memory",
		flociImage,
	).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("docker run: %w: %s", err, out)
	}

	id := strings.TrimSpace(string(out))
	stop := func() { exec.Command("docker", "rm", "-f", id).Run() }

	address, err := dockerPort(id, "4566/tcp")
	if err != nil {
		stop()

		return "", nil, err
	}

	return "http://" + address, stop, nil
}

func client(endpoint string, credentials aws.Credentials) *awss3.Client {
	return awss3.New(awss3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return credentials, nil }),
	})
}

// env is one test's fork: the app's bucket through blobfork, and the real
// one in lab.
type env struct {
	bucket string
	fork   *Fork
	proxy  *httptest.Server
	app    *awss3.Client
	lab    *awss3.Client
}

func setup(t *testing.T) *env {
	return setupMany(t, 1)[0]
}

// setupMany forks n buckets through one blobfork, an env each.
func setupMany(t *testing.T, n int) []*env {
	t.Helper()

	ctx := t.Context()

	lab := client(flociLab, testCredentials)

	buckets := map[string]Layer{}
	envs := make([]*env, n)

	for i := range envs {
		bucket := fmt.Sprintf("files-%d-%d", time.Now().UnixNano()%1e6, sequence.Add(1))

		// Floci may still be starting.
		deadline := time.Now().Add(time.Minute)

		for {
			_, err := lab.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)})
			if err == nil {
				break
			}

			if time.Now().After(deadline) {
				t.Fatal(err)
			}

			time.Sleep(500 * time.Millisecond)
		}

		buckets[bucket] = &Bucket{
			Endpoint:    flociLab,
			Name:        bucket,
			Region:      "us-east-1",
			PathStyle:   true,
			Credentials: testCredentials,
		}

		envs[i] = &env{bucket: bucket, lab: lab}
	}

	fork, err := New(ctx, Config{
		Floci:       floci,
		Credentials: testCredentials,
		Region:      "us-east-1",
		Buckets:     buckets,
		Log:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(fork)
	t.Cleanup(proxy.Close)

	for _, e := range envs {
		e.fork, e.proxy, e.app = fork, proxy, client(proxy.URL, testCredentials)
	}

	return envs
}

// seed writes the real bucket's objects, then has the test check that it's
// never written after.
func (e *env) seed(t *testing.T, objects map[string]string) {
	t.Helper()

	for key, body := range objects {
		_, err := e.lab.PutObject(t.Context(), &awss3.PutObjectInput{
			Bucket:      aws.String(e.bucket),
			Key:         aws.String(key),
			Body:        strings.NewReader(body),
			ContentType: aws.String("text/plain"),
			Metadata:    map[string]string{"origin": "lab"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	before := e.labState(t)

	t.Cleanup(func() {
		after := e.labState(t)

		if !maps.Equal(before, after) {
			t.Errorf("the real bucket changed:\nbefore %v\nafter  %v", before, after)
		}
	})
}

// labState is the real bucket's objects and their ETags.
func (e *env) labState(t *testing.T) map[string]string {
	state := map[string]string{}

	pager := awss3.NewListObjectsV2Paginator(e.lab, &awss3.ListObjectsV2Input{Bucket: aws.String(e.bucket)})

	for pager.HasMorePages() {
		page, err := pager.NextPage(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		for _, object := range page.Contents {
			state[*object.Key] = *object.ETag
		}
	}

	return state
}

func (e *env) read(t *testing.T, key string) (string, error) {
	t.Helper()

	res, err := e.app.GetObject(t.Context(), &awss3.GetObjectInput{Bucket: aws.String(e.bucket), Key: aws.String(key)})
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)

	return string(body), err
}

func (e *env) write(t *testing.T, key, body string) {
	t.Helper()

	_, err := e.app.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: aws.String(e.bucket), Key: aws.String(key), Body: strings.NewReader(body)})
	if err != nil {
		t.Fatalf("write %s: %v", key, err)
	}
}

func (e *env) remove(t *testing.T, key string) {
	t.Helper()

	_, err := e.app.DeleteObject(t.Context(), &awss3.DeleteObjectInput{Bucket: aws.String(e.bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("delete %s: %v", key, err)
	}
}

func notFound(err error) bool {
	var noKey *types.NoSuchKey
	var missing *types.NotFound

	return errors.As(err, &noKey) || errors.As(err, &missing)
}

func errorCodeOf(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}

	return ""
}

// Reads the fork can't answer come from the real bucket: bodies, ranges,
// headers and metadata; an object in neither is missing.
func TestReadsFallThrough(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"invoices/1.pdf": "0123456789", "a b+c (1).txt": "spaces"})

	ctx := t.Context()

	if body, err := e.read(t, "invoices/1.pdf"); err != nil || body != "0123456789" {
		t.Fatalf("read: %q %v", body, err)
	}

	if body, err := e.read(t, "a b+c (1).txt"); err != nil || body != "spaces" {
		t.Fatalf("read an escaped key: %q %v", body, err)
	}

	res, err := e.app.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(e.bucket), Key: aws.String("invoices/1.pdf"), Range: aws.String("bytes=2-4")})
	if err != nil {
		t.Fatal(err)
	}

	part, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if string(part) != "234" || aws.ToString(res.ContentRange) != "bytes 2-4/10" {
		t.Fatalf("range: %q %v", part, aws.ToString(res.ContentRange))
	}

	head, err := e.app.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(e.bucket), Key: aws.String("invoices/1.pdf")})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToInt64(head.ContentLength) != 10 || aws.ToString(head.ContentType) != "text/plain" || head.Metadata["origin"] != "lab" {
		t.Fatalf("head: %d %s %v", aws.ToInt64(head.ContentLength), aws.ToString(head.ContentType), head.Metadata)
	}

	if _, err := e.read(t, "nowhere.txt"); !notFound(err) {
		t.Fatalf("missing: %v", err)
	}

	_, err = e.app.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(e.bucket), Key: aws.String("nowhere.txt")})
	if !notFound(err) {
		t.Fatalf("missing head: %v", err)
	}
}

// Without the app's key there's nothing to read, from either layer.
func TestReadsNeedTheKey(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"secret.txt": "lab's"})

	for _, path := range []string{"/" + e.bucket + "/secret.txt", "/" + e.bucket + "?list-type=2"} {
		res, err := http.Get(e.proxy.URL + path)
		if err != nil {
			t.Fatal(err)
		}

		res.Body.Close()

		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("unsigned %s: %d", path, res.StatusCode)
		}
	}

	wrong := client(e.proxy.URL, aws.Credentials{AccessKeyID: "test", SecretAccessKey: "wrong"})

	_, err := wrong.GetObject(t.Context(), &awss3.GetObjectInput{Bucket: aws.String(e.bucket), Key: aws.String("secret.txt")})
	if errorCodeOf(err) != "SignatureDoesNotMatch" {
		t.Fatalf("wrong secret: %v", err)
	}
}

// A presigned URL the app makes, as for a browser's download, reads an
// object only in the real bucket.
func TestPresignedFallsThrough(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"photo.jpg": "jpeg"})

	presigned, err := awss3.NewPresignClient(e.app).PresignGetObject(t.Context(), &awss3.GetObjectInput{Bucket: aws.String(e.bucket), Key: aws.String("photo.jpg")})
	if err != nil {
		t.Fatal(err)
	}

	res, err := http.Get(presigned.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusOK || string(body) != "jpeg" {
		t.Fatalf("presigned read: %d %s", res.StatusCode, body)
	}
}

// Writes stay in the fork: new objects, and the real bucket's overwritten.
func TestWritesStayInFork(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"a.txt": "lab's"})

	e.write(t, "a.txt", "the fork's")
	e.write(t, "b.txt", "new")

	if body, _ := e.read(t, "a.txt"); body != "the fork's" {
		t.Fatalf("overwritten: %q", body)
	}

	if body, _ := e.read(t, "b.txt"); body != "new" {
		t.Fatalf("new: %q", body)
	}
}

// Deletes, one at a time or many at once, hide the real bucket's objects
// until the app writes them again.
func TestDeletesHide(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"only-lab.txt": "1", "both.txt": "2", "dir/x": "3", "dir/y": "4", "many/1": "5", "many/2": "6"})

	ctx := t.Context()

	e.write(t, "both.txt", "the fork's")

	for _, key := range []string{"only-lab.txt", "both.txt"} {
		e.remove(t, key)

		if _, err := e.read(t, key); !notFound(err) {
			t.Fatalf("read deleted %s: %v", key, err)
		}

		_, err := e.app.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(e.bucket), Key: aws.String(key)})
		if !notFound(err) {
			t.Fatalf("head of deleted %s: %v", key, err)
		}
	}

	_, err := e.app.DeleteObjects(ctx, &awss3.DeleteObjectsInput{
		Bucket: aws.String(e.bucket),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("many/1")}, {Key: aws.String("many/2")}, {Key: aws.String("nowhere")}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"many/1", "many/2"} {
		if _, err := e.read(t, key); !notFound(err) {
			t.Fatalf("read %s deleted with DeleteObjects: %v", key, err)
		}
	}

	e.write(t, "only-lab.txt", "back")

	if body, _ := e.read(t, "only-lab.txt"); body != "back" {
		t.Fatalf("written again: %q", body)
	}

	// Markers outlast blobfork: a new one over the same Floci reads them.
	again, err := New(ctx, e.fork.Config)
	if err != nil {
		t.Fatal(err)
	}

	if !again.buckets[e.bucket].markers.has("both.txt") || !again.buckets[e.bucket].markers.has("many/2") {
		t.Fatal("the markers weren't kept")
	}

	// A prefix whose objects are all deleted is gone from the listing.
	e.remove(t, "dir/x")
	e.remove(t, "dir/y")

	keys, prefixes := listV2(t, e.app, e.bucket, "", "/", 0)

	if !slices.Equal(keys, []string{"only-lab.txt"}) || len(prefixes) != 0 {
		t.Fatalf("listing after deletes: %v %v", keys, prefixes)
	}
}

// Copies whose source is only in the real bucket copy it up first.
func TestCopyFromLower(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"template.docx": "template", "part.bin": strings.Repeat("p", 5<<20)})

	ctx := t.Context()

	_, err := e.app.CopyObject(ctx, &awss3.CopyObjectInput{
		Bucket:     aws.String(e.bucket),
		Key:        aws.String("copy.docx"),
		CopySource: aws.String(e.bucket + "/template.docx"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if body, err := e.read(t, "copy.docx"); err != nil || body != "template" {
		t.Fatalf("copy: %q %v", body, err)
	}

	// The source was copied up, with lab's headers and metadata.
	head, err := client(floci, testCredentials).HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(e.bucket), Key: aws.String("template.docx")})
	if err != nil {
		t.Fatalf("not copied up: %v", err)
	}

	if aws.ToString(head.ContentType) != "text/plain" || head.Metadata["origin"] != "lab" {
		t.Fatalf("copied up without its headers: %s %v", aws.ToString(head.ContentType), head.Metadata)
	}

	// UploadPartCopy too.
	upload, err := e.app.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Bucket: aws.String(e.bucket), Key: aws.String("joined.bin")})
	if err != nil {
		t.Fatal(err)
	}

	part, err := e.app.UploadPartCopy(ctx, &awss3.UploadPartCopyInput{
		Bucket:     aws.String(e.bucket),
		Key:        aws.String("joined.bin"),
		UploadId:   upload.UploadId,
		PartNumber: aws.Int32(1),
		CopySource: aws.String(e.bucket + "/part.bin"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.app.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:          aws.String(e.bucket),
		Key:             aws.String("joined.bin"),
		UploadId:        upload.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: part.CopyPartResult.ETag, PartNumber: aws.Int32(1)}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if body, err := e.read(t, "joined.bin"); err != nil || len(body) != 5<<20 {
		t.Fatalf("part copy: %d bytes, %v", len(body), err)
	}
}

// Writes that change an object in place copy it up first.
func TestWriteInPlace(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"a.txt": "lab's"})

	ctx := t.Context()

	_, err := e.app.PutObjectTagging(ctx, &awss3.PutObjectTaggingInput{
		Bucket:  aws.String(e.bucket),
		Key:     aws.String("a.txt"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("seen"), Value: aws.String("yes")}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := e.app.GetObjectTagging(ctx, &awss3.GetObjectTaggingInput{Bucket: aws.String(e.bucket), Key: aws.String("a.txt")})
	if err != nil || len(tags.TagSet) != 1 {
		t.Fatalf("tags: %v %v", tags, err)
	}

	if body, _ := e.read(t, "a.txt"); body != "lab's" {
		t.Fatalf("body after copy-up: %q", body)
	}
}

// Several buckets fork through one blobfork, each over its own real one, and
// a copy from one to another copies its source up in its own.
func TestSeveralBuckets(t *testing.T) {
	envs := setupMany(t, 2)
	files, images := envs[0], envs[1]

	files.seed(t, map[string]string{"a.txt": "files'", "shared.txt": "in files"})
	images.seed(t, map[string]string{"logo.png": "images'", "shared.txt": "in images"})

	ctx := t.Context()

	for _, check := range []struct {
		e         *env
		key, body string
	}{
		{files, "a.txt", "files'"},
		{files, "shared.txt", "in files"},
		{images, "logo.png", "images'"},
		{images, "shared.txt", "in images"},
	} {
		if body, err := check.e.read(t, check.key); err != nil || body != check.body {
			t.Fatalf("%s in %s: %q %v", check.key, check.e.bucket, body, err)
		}
	}

	if _, err := files.read(t, "logo.png"); !notFound(err) {
		t.Fatalf("another bucket's object: %v", err)
	}

	files.remove(t, "shared.txt")

	if body, _ := images.read(t, "shared.txt"); body != "in images" {
		t.Fatalf("a delete in one bucket hid another's: %q", body)
	}

	if keys, _ := listV2(t, files.app, files.bucket, "", "", 0); !slices.Equal(keys, []string{"a.txt"}) {
		t.Fatalf("files: %v", keys)
	}

	if keys, _ := listV2(t, images.app, images.bucket, "", "", 0); !slices.Equal(keys, []string{"logo.png", "shared.txt"}) {
		t.Fatalf("images: %v", keys)
	}

	_, err := files.app.CopyObject(ctx, &awss3.CopyObjectInput{
		Bucket:     aws.String(files.bucket),
		Key:        aws.String("logo-copy.png"),
		CopySource: aws.String(images.bucket + "/logo.png"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if body, err := files.read(t, "logo-copy.png"); err != nil || body != "images'" {
		t.Fatalf("copy across buckets: %q %v", body, err)
	}

	_, err = client(floci, testCredentials).HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(images.bucket), Key: aws.String("logo.png")})
	if err != nil {
		t.Fatalf("the source wasn't copied up in its own bucket: %v", err)
	}
}

// ListBuckets leaves the hidden one out.
func TestListBucketsHides(t *testing.T) {
	e := setup(t)

	res, err := e.app.ListBuckets(t.Context(), &awss3.ListBucketsInput{})
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, bucket := range res.Buckets {
		names = append(names, *bucket.Name)
	}

	if slices.Contains(names, HiddenBucket) || !slices.Contains(names, e.bucket) {
		t.Fatalf("buckets: %v", names)
	}
}

// Listings merge both layers by name, the fork's winning and deleted objects
// left out, and page through the merge without repeats or gaps, whatever the
// page size, in both versions of ListObjects. Random writes and deletes
// against a model of what's there.
func TestListingsMerge(t *testing.T) {
	for seed := range uint64(4) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { testListingsMerge(t, seed) })
	}
}

func testListingsMerge(t *testing.T, seed uint64) {
	e := setup(t)

	ctx := t.Context()
	random := rand.New(rand.NewPCG(seed, 2))

	segments := []string{"a", "b", "c", "d"}

	randomKey := func() string {
		depth := 1 + random.IntN(3)
		parts := make([]string, depth)

		for i := range parts {
			parts[i] = segments[random.IntN(len(segments))]
		}

		return strings.Join(parts, "/") + fmt.Sprint(random.IntN(3))
	}

	lab := map[string]string{}
	for range 40 {
		lab[randomKey()] = "lab"
	}

	e.seed(t, lab)

	visible := map[string]bool{}
	for key := range lab {
		visible[key] = true
	}

	for range 60 {
		key := randomKey()

		if random.IntN(2) == 0 {
			e.write(t, key, "fork")
			visible[key] = true

			continue
		}

		e.remove(t, key)
		delete(visible, key)
	}

	want := slices.Sorted(maps.Keys(visible))

	for _, size := range []int{0, 1, 2, 3, 7, 1000} {
		if got, _ := listV2(t, e.app, e.bucket, "", "", size); !slices.Equal(got, want) {
			t.Fatalf("flat V2, pages of %d:\ngot  %v\nwant %v", size, got, want)
		}

		if got, _ := listV1(t, e.app, e.bucket, "", "", size); !slices.Equal(got, want) {
			t.Fatalf("flat V1, pages of %d:\ngot  %v\nwant %v", size, got, want)
		}
	}

	for _, prefix := range []string{"", "a/", "b/c/"} {
		wantKeys, wantPrefixes := hierarchy(want, prefix)

		for _, size := range []int{0, 1, 2, 5} {
			keys, prefixes := listV2(t, e.app, e.bucket, prefix, "/", size)

			if !slices.Equal(keys, wantKeys) || !slices.Equal(prefixes, wantPrefixes) {
				t.Fatalf("hierarchy V2 %q, pages of %d:\ngot  %v %v\nwant %v %v", prefix, size, keys, prefixes, wantKeys, wantPrefixes)
			}

			keys, prefixes = listV1(t, e.app, e.bucket, prefix, "/", size)

			if !slices.Equal(keys, wantKeys) || !slices.Equal(prefixes, wantPrefixes) {
				t.Fatalf("hierarchy V1 %q, pages of %d:\ngot  %v %v\nwant %v %v", prefix, size, keys, prefixes, wantKeys, wantPrefixes)
			}
		}
	}

	// The fork's copy is the one listed: its size is the fork's body's.
	page, err := e.app.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String(e.bucket)})
	if err != nil {
		t.Fatal(err)
	}

	for _, object := range page.Contents {
		body, _ := e.read(t, *object.Key)

		if int64(len(body)) != aws.ToInt64(object.Size) {
			t.Fatalf("%s listed from the wrong layer: size %d, body %q", *object.Key, aws.ToInt64(object.Size), body)
		}
	}
}

// A continuation token that isn't blobfork's is turned down.
func TestListingBadToken(t *testing.T) {
	e := setup(t)

	_, err := e.app.ListObjectsV2(t.Context(), &awss3.ListObjectsV2Input{Bucket: aws.String(e.bucket), ContinuationToken: aws.String("someone-else's")})
	if errorCodeOf(err) != "InvalidArgument" {
		t.Fatalf("bad token: %v", err)
	}
}

func listV2(t *testing.T, app *awss3.Client, bucket, prefix, delimiter string, size int) (keys, prefixes []string) {
	t.Helper()

	input := &awss3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)}

	if delimiter != "" {
		input.Delimiter = aws.String(delimiter)
	}

	if size > 0 {
		input.MaxKeys = aws.Int32(int32(size))
	}

	pager := awss3.NewListObjectsV2Paginator(app, input)

	for pager.HasMorePages() {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		if size > 0 && int(aws.ToInt32(page.KeyCount)) > size {
			t.Fatalf("page of %d for %d", aws.ToInt32(page.KeyCount), size)
		}

		for _, object := range page.Contents {
			keys = append(keys, *object.Key)
		}

		for _, p := range page.CommonPrefixes {
			prefixes = append(prefixes, *p.Prefix)
		}
	}

	return keys, prefixes
}

func listV1(t *testing.T, app *awss3.Client, bucket, prefix, delimiter string, size int) (keys, prefixes []string) {
	t.Helper()

	input := &awss3.ListObjectsInput{Bucket: aws.String(bucket), Prefix: aws.String(prefix)}

	if delimiter != "" {
		input.Delimiter = aws.String(delimiter)
	}

	if size > 0 {
		input.MaxKeys = aws.Int32(int32(size))
	}

	for {
		page, err := app.ListObjects(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}

		for _, object := range page.Contents {
			keys = append(keys, *object.Key)
		}

		for _, p := range page.CommonPrefixes {
			prefixes = append(prefixes, *p.Prefix)
		}

		if !aws.ToBool(page.IsTruncated) {
			return keys, prefixes
		}

		input.Marker = page.NextMarker
	}
}

// hierarchy is what a listing with the delimiter "/" has under a prefix.
func hierarchy(names []string, prefix string) (keys, prefixes []string) {
	seen := map[string]bool{}

	for _, name := range names {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}

		i := strings.Index(rest, "/")
		if i < 0 {
			keys = append(keys, name)

			continue
		}

		if p := prefix + rest[:i+1]; !seen[p] {
			seen[p] = true
			prefixes = append(prefixes, p)
		}
	}

	return keys, prefixes
}

// dockerPort is the address a container's port is published at, waiting a
// little for it; a container that stopped instead says why, in its logs.
func dockerPort(id, port string) (string, error) {
	deadline := time.Now().Add(10 * time.Second)

	for {
		out, err := exec.Command("docker", "port", id, port).Output()
		if err == nil {
			return strings.TrimSpace(strings.Split(string(out), "\n")[0]), nil
		}

		if time.Now().After(deadline) {
			state, _ := exec.Command("docker", "inspect", "-f", "{{.State.Status}} (exit {{.State.ExitCode}})", id).CombinedOutput()
			logs, _ := exec.Command("docker", "logs", "--tail", "40", id).CombinedOutput()

			return "", fmt.Errorf("docker port %s: %w; the container is %s; its logs:\n%s", port, err, strings.TrimSpace(string(state)), logs)
		}

		time.Sleep(200 * time.Millisecond)
	}
}
