package azure

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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
)

// The tests run the Azure SDK against blobfork in front of Azurite, with a
// second account in the same Azurite (lab) holding the real container.
// BLOBFORK_TEST_AZURITE names a running Azurite with both accounts
// (AZURITE_ACCOUNTS="blobfork:<forkKey>;lab:<labKey>"); otherwise the tests
// start one with Docker.

const (
	azuriteImage = "mcr.microsoft.com/azure-storage/azurite:3.35.0"

	forkKey = "Zm9yay1rZXktZm9yLWJsb2Jmb3JrLXRlc3RzLW5vdC1hLXNlY3JldC0wMDAwMDAwMDA="
	labKey  = "bGFiLWtleS1mb3ItYmxvYmZvcmstdGVzdHMtbm90LWEtc2VjcmV0LTAwMDAwMDAwMDA="
)

var (
	azurite  string
	sequence atomic.Int64
)

func TestMain(m *testing.M) {
	azurite = os.Getenv("BLOBFORK_TEST_AZURITE")

	if azurite == "" {
		stop, err := startAzurite()
		if err != nil {
			fmt.Fprintln(os.Stderr, "blobfork tests need Azurite:", err)
			os.Exit(1)
		}

		code := m.Run()

		stop()
		os.Exit(code)
	}

	os.Exit(m.Run())
}

func startAzurite() (func(), error) {
	out, err := exec.Command(
		"docker", "run", "-d",
		"-p", "127.0.0.1::10000",
		"-e", "AZURITE_ACCOUNTS=blobfork:"+forkKey+";lab:"+labKey,
		azuriteImage,
		"azurite-blob", "--blobHost", "0.0.0.0", "--inMemoryPersistence", "--skipApiVersionCheck", "--disableProductStyleUrl",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("docker run: %w: %s", err, stderr(err))
	}

	// Only the container's ID is on stdout; a pull's progress isn't.
	id := strings.TrimSpace(string(out))
	stop := func() { exec.Command("docker", "rm", "-f", id).Run() }

	address, err := dockerPort(id, "10000/tcp")
	if err != nil {
		stop()

		return nil, err
	}

	azurite = "http://" + address

	return stop, nil
}

// env is one test's fork: the app's container through blobfork, and the
// real one in lab.
type env struct {
	name  string
	fork  *Fork
	proxy *httptest.Server
	app   *container.Client
	lab   *container.Client
	labs  *SASLayer
}

func setup(t *testing.T) *env {
	return setupMany(t, 1)[0]
}

// setupMany forks n containers through one blobfork, an env each.
func setupMany(t *testing.T, n int) []*env {
	t.Helper()

	ctx := t.Context()

	labCred, _ := container.NewSharedKeyCredential("lab", labKey)
	forkCred, _ := container.NewSharedKeyCredential("blobfork", forkKey)

	containers := map[string]Layer{}
	envs := make([]*env, n)

	for i := range envs {
		name := fmt.Sprintf("files-%d-%d", time.Now().UnixNano()%1e6, sequence.Add(1))

		lab, err := container.NewClientWithSharedKeyCredential(azurite+"/lab/"+name, labCred, nil)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := lab.Create(ctx, nil); err != nil {
			t.Fatal(err)
		}

		query, err := sas.BlobSignatureValues{
			Protocol:      sas.ProtocolHTTPSandHTTP,
			ExpiryTime:    time.Now().Add(time.Hour).UTC(),
			Permissions:   (&sas.ContainerPermissions{Read: true, List: true}).String(),
			ContainerName: name,
		}.SignWithSharedKey(labCred)
		if err != nil {
			t.Fatal(err)
		}

		lower, err := NewSASLayer(azurite + "/lab/" + name + "?" + query.Encode())
		if err != nil {
			t.Fatal(err)
		}

		containers[lower.Container()] = lower
		envs[i] = &env{name: name, lab: lab, labs: lower}
	}

	fork, err := New(ctx, Config{
		Azurite:    azurite,
		Account:    "blobfork",
		Key:        forkKey,
		Containers: containers,
		Log:        slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(fork)
	t.Cleanup(proxy.Close)

	for _, e := range envs {
		app, err := container.NewClientWithSharedKeyCredential(proxy.URL+"/blobfork/"+e.name, forkCred, nil)
		if err != nil {
			t.Fatal(err)
		}

		e.fork, e.proxy, e.app = fork, proxy, app
	}

	return envs
}

// seed writes the real container's blobs, then has the test check that it's
// never written after.
func (e *env) seed(t *testing.T, blobs map[string]string) {
	t.Helper()

	for name, body := range blobs {
		_, err := e.lab.NewBlockBlobClient(name).UploadBuffer(t.Context(), []byte(body), &blockblob.UploadBufferOptions{
			HTTPHeaders: &blob.HTTPHeaders{BlobContentType: to.Ptr("text/plain")},
			Metadata:    map[string]*string{"origin": to.Ptr("lab")},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	before := e.labState(t)

	t.Cleanup(func() {
		after := e.labState(t)

		if !maps.Equal(before, after) {
			t.Errorf("the real container changed:\nbefore %v\nafter  %v", before, after)
		}
	})
}

// labState is the real container's blobs and their ETags.
func (e *env) labState(t *testing.T) map[string]string {
	state := map[string]string{}

	pager := e.lab.NewListBlobsFlatPager(nil)

	for pager.More() {
		page, err := pager.NextPage(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		for _, item := range page.Segment.BlobItems {
			state[*item.Name] = string(*item.Properties.ETag)
		}
	}

	return state
}

func (e *env) read(t *testing.T, name string) (string, error) {
	t.Helper()

	res, err := e.app.NewBlobClient(name).DownloadStream(t.Context(), nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)

	return string(body), err
}

func (e *env) write(t *testing.T, name, body string) {
	t.Helper()

	if _, err := e.app.NewBlockBlobClient(name).UploadBuffer(t.Context(), []byte(body), nil); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// Reads the fork can't answer come from the real container: bodies, ranges,
// properties and metadata; a blob in neither is missing.
func TestReadsFallThrough(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"invoices/1.pdf": "0123456789", "a b+c.txt": "spaces"})

	ctx := t.Context()

	if body, err := e.read(t, "invoices/1.pdf"); err != nil || body != "0123456789" {
		t.Fatalf("read: %q %v", body, err)
	}

	if body, err := e.read(t, "a b+c.txt"); err != nil || body != "spaces" {
		t.Fatalf("read an escaped name: %q %v", body, err)
	}

	res, err := e.app.NewBlobClient("invoices/1.pdf").DownloadStream(ctx, &blob.DownloadStreamOptions{
		Range: blob.HTTPRange{Offset: 2, Count: 3},
	})
	if err != nil {
		t.Fatal(err)
	}

	part, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if string(part) != "234" || res.ContentRange == nil || *res.ContentRange != "bytes 2-4/10" {
		t.Fatalf("range: %q %v", part, res.ContentRange)
	}

	props, err := e.app.NewBlobClient("invoices/1.pdf").GetProperties(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	if *props.ContentLength != 10 || *props.ContentType != "text/plain" || props.Metadata["Origin"] == nil && props.Metadata["origin"] == nil {
		t.Fatalf("properties: %d %s %v", *props.ContentLength, *props.ContentType, props.Metadata)
	}

	_, err = e.read(t, "nowhere.txt")
	if !bloberror.HasCode(err, bloberror.BlobNotFound) {
		t.Fatalf("missing: %v", err)
	}

	_, err = e.app.NewBlobClient("nowhere.txt").GetProperties(ctx, nil)
	if !bloberror.HasCode(err, bloberror.BlobNotFound) {
		t.Fatalf("missing properties: %v", err)
	}
}

// Without the app's key there's nothing to read, from either layer.
func TestReadsNeedTheKey(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"secret.txt": "lab's"})

	res, err := http.Get(e.proxy.URL + "/blobfork/" + e.name + "/secret.txt")
	if err != nil {
		t.Fatal(err)
	}

	res.Body.Close()

	if res.StatusCode == http.StatusOK {
		t.Fatal("read without a key")
	}

	res, err = http.Get(e.proxy.URL + "/blobfork/" + e.name + "?restype=container&comp=list")
	if err != nil {
		t.Fatal(err)
	}

	res.Body.Close()

	if res.StatusCode == http.StatusOK {
		t.Fatal("listed without a key")
	}
}

// A SAS URL the app signs, as for a browser's download, reads a blob only in
// the real container.
func TestSASFallsThrough(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"photo.jpg": "jpeg"})

	url, err := e.app.NewBlobClient("photo.jpg").GetSASURL(sas.BlobPermissions{Read: true}, time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}

	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusOK || string(body) != "jpeg" {
		t.Fatalf("SAS read: %d %s", res.StatusCode, body)
	}
}

// Writes stay in the fork: new blobs, and the real container's overwritten.
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

// Deletes hide the real container's blob, until the app writes it again.
func TestDeletesHide(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"only-lab.txt": "1", "both.txt": "2", "dir/x": "3", "dir/y": "4"})

	ctx := t.Context()

	e.write(t, "both.txt", "the fork's")

	for _, name := range []string{"only-lab.txt", "both.txt"} {
		if _, err := e.app.NewBlobClient(name).Delete(ctx, nil); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}

		if _, err := e.read(t, name); !bloberror.HasCode(err, bloberror.BlobNotFound) {
			t.Fatalf("read deleted %s: %v", name, err)
		}

		if _, err := e.app.NewBlobClient(name).GetProperties(ctx, nil); !bloberror.HasCode(err, bloberror.BlobNotFound) {
			t.Fatalf("properties of deleted %s: %v", name, err)
		}

		if _, err := e.app.NewBlobClient(name).Delete(ctx, nil); !bloberror.HasCode(err, bloberror.BlobNotFound) {
			t.Fatalf("delete deleted %s: %v", name, err)
		}
	}

	if _, err := e.app.NewBlobClient("nowhere").Delete(ctx, nil); !bloberror.HasCode(err, bloberror.BlobNotFound) {
		t.Fatalf("delete missing: %v", err)
	}

	e.write(t, "only-lab.txt", "back")

	if body, _ := e.read(t, "only-lab.txt"); body != "back" {
		t.Fatalf("written again: %q", body)
	}

	// Markers outlast blobfork: a new one over the same Azurite reads them.
	again, err := New(ctx, e.fork.Config)
	if err != nil {
		t.Fatal(err)
	}

	if !again.containers[e.name].markers.has("both.txt") {
		t.Fatal("the marker wasn't kept")
	}

	// A prefix whose blobs are all deleted is gone from the listing.
	for _, name := range []string{"dir/x", "dir/y"} {
		if _, err := e.app.NewBlobClient(name).Delete(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}

	blobs, prefixes := listHierarchy(t, e.app, "", 0)

	if !slices.Equal(blobs, []string{"only-lab.txt"}) || len(prefixes) != 0 {
		t.Fatalf("listing after deletes: %v %v", blobs, prefixes)
	}
}

// Copies whose source is only in the real container copy it up first.
func TestCopyFromLower(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"template.docx": "template", "part.bin": "part"})

	ctx := t.Context()

	source := e.app.NewBlobClient("template.docx").URL()

	res, err := e.app.NewBlobClient("copy.docx").StartCopyFromURL(ctx, source, nil)
	if err != nil {
		t.Fatal(err)
	}

	for status := *res.CopyStatus; status == blob.CopyStatusTypePending; {
		time.Sleep(100 * time.Millisecond)

		props, err := e.app.NewBlobClient("copy.docx").GetProperties(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}

		status = *props.CopyStatus
	}

	if body, err := e.read(t, "copy.docx"); err != nil || body != "template" {
		t.Fatalf("copy: %q %v", body, err)
	}

	// The source was copied up, so it's the fork's now, with lab's metadata.
	props, err := e.fork.containers[e.name].client.NewBlobClient("template.docx").GetProperties(ctx, nil)
	if err != nil {
		t.Fatalf("not copied up: %v", err)
	}

	if *props.ContentType != "text/plain" || len(props.Metadata) != 1 {
		t.Fatalf("copied up without its properties: %s %v", *props.ContentType, props.Metadata)
	}

	// Put Block From URL copies its source up too, though Azurite 3.35 doesn't
	// do the rest (501).
	sourceSAS, err := e.app.NewBlobClient("part.bin").GetSASURL(sas.BlobPermissions{Read: true}, time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.app.NewBlockBlobClient("blocks.bin").StageBlockFromURL(ctx, "YmxvY2stMQ==", sourceSAS, nil)
	if err != nil && !bloberror.HasCode(err, "APINotImplemented") {
		t.Fatalf("Put Block From URL: %v", err)
	}

	if _, err := e.fork.containers[e.name].client.NewBlobClient("part.bin").GetProperties(ctx, nil); err != nil {
		t.Fatalf("Put Block From URL's source not copied up: %v", err)
	}
}

// Writes that change a blob in place copy it up first.
func TestWriteInPlace(t *testing.T) {
	e := setup(t)
	e.seed(t, map[string]string{"a.txt": "lab's"})

	ctx := t.Context()

	_, err := e.app.NewBlobClient("a.txt").SetMetadata(ctx, map[string]*string{"seen": to.Ptr("yes")}, nil)
	if err != nil {
		t.Fatal(err)
	}

	props, err := e.app.NewBlobClient("a.txt").GetProperties(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	if props.Metadata["Seen"] == nil && props.Metadata["seen"] == nil {
		t.Fatalf("metadata: %v", props.Metadata)
	}

	if body, _ := e.read(t, "a.txt"); body != "lab's" {
		t.Fatalf("body after copy-up: %q", body)
	}
}

// Several containers fork through one blobfork, each over its own real one,
// and a copy from one to another copies its source up in its own.
func TestSeveralContainers(t *testing.T) {
	envs := setupMany(t, 2)
	files, images := envs[0], envs[1]

	files.seed(t, map[string]string{"a.txt": "files'", "shared.txt": "in files"})
	images.seed(t, map[string]string{"logo.png": "images'", "shared.txt": "in images"})

	ctx := t.Context()

	for _, check := range []struct {
		e          *env
		name, body string
	}{
		{files, "a.txt", "files'"},
		{files, "shared.txt", "in files"},
		{images, "logo.png", "images'"},
		{images, "shared.txt", "in images"},
	} {
		if body, err := check.e.read(t, check.name); err != nil || body != check.body {
			t.Fatalf("%s in %s: %q %v", check.name, check.e.name, body, err)
		}
	}

	if _, err := files.read(t, "logo.png"); !bloberror.HasCode(err, bloberror.BlobNotFound) {
		t.Fatalf("another container's blob: %v", err)
	}

	if _, err := files.app.NewBlobClient("shared.txt").Delete(ctx, nil); err != nil {
		t.Fatal(err)
	}

	if body, _ := images.read(t, "shared.txt"); body != "in images" {
		t.Fatalf("a delete in one container hid another's: %q", body)
	}

	if blobs := listFlat(t, files.app, 0); !slices.Equal(blobs, []string{"a.txt"}) {
		t.Fatalf("files: %v", blobs)
	}

	if blobs := listFlat(t, images.app, 0); !slices.Equal(blobs, []string{"logo.png", "shared.txt"}) {
		t.Fatalf("images: %v", blobs)
	}

	_, err := files.app.NewBlobClient("logo-copy.png").StartCopyFromURL(ctx, images.app.NewBlobClient("logo.png").URL(), nil)
	if err != nil {
		t.Fatal(err)
	}

	if body, err := files.read(t, "logo-copy.png"); err != nil || body != "images'" {
		t.Fatalf("copy across containers: %q %v", body, err)
	}

	if _, err := images.fork.containers[images.name].client.NewBlobClient("logo.png").GetProperties(ctx, nil); err != nil {
		t.Fatalf("the source wasn't copied up in its own container: %v", err)
	}
}

// List Containers leaves the hidden one out.
func TestListContainersHides(t *testing.T) {
	e := setup(t)

	cred, _ := service.NewSharedKeyCredential("blobfork", forkKey)

	svc, err := service.NewClientWithSharedKeyCredential(e.proxy.URL+"/blobfork", cred, nil)
	if err != nil {
		t.Fatal(err)
	}

	var names []string

	pager := svc.NewListContainersPager(nil)

	for pager.More() {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		for _, item := range page.ContainerItems {
			names = append(names, *item.Name)
		}
	}

	if slices.Contains(names, HiddenContainer) || !slices.Contains(names, e.name) {
		t.Fatalf("containers: %v", names)
	}
}

// Listings merge both layers by name, the fork's winning and deleted blobs
// left out, and page through the merge without repeats or gaps, whatever the
// page size. Random writes and deletes against a model of what's there.
func TestListingsMerge(t *testing.T) {
	for seed := range uint64(5) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { testListingsMerge(t, seed) })
	}
}

func testListingsMerge(t *testing.T, seed uint64) {
	e := setup(t)

	ctx := t.Context()
	random := rand.New(rand.NewPCG(seed, 2))

	segments := []string{"a", "b", "c", "d"}

	randomName := func() string {
		depth := 1 + random.IntN(3)
		parts := make([]string, depth)

		for i := range parts {
			parts[i] = segments[random.IntN(len(segments))]
		}

		return strings.Join(parts, "/") + fmt.Sprint(random.IntN(3))
	}

	lab := map[string]string{}
	for range 40 {
		lab[randomName()] = "lab"
	}

	e.seed(t, lab)

	visible := map[string]bool{}
	for name := range lab {
		visible[name] = true
	}

	for range 60 {
		name := randomName()

		if random.IntN(2) == 0 {
			e.write(t, name, "fork")
			visible[name] = true

			continue
		}

		_, err := e.app.NewBlobClient(name).Delete(ctx, nil)

		switch {
		case visible[name] && err != nil:
			t.Fatalf("delete %s: %v", name, err)

		case !visible[name] && !bloberror.HasCode(err, bloberror.BlobNotFound):
			t.Fatalf("delete missing %s: %v", name, err)
		}

		delete(visible, name)
	}

	want := slices.Sorted(maps.Keys(visible))

	for _, size := range []int{0, 1, 2, 3, 7, 1000} {
		got := listFlat(t, e.app, size)

		if !slices.Equal(got, want) {
			t.Fatalf("flat, pages of %d:\ngot  %v\nwant %v", size, got, want)
		}
	}

	for _, prefix := range []string{"", "a/", "b/c/"} {
		wantBlobs, wantPrefixes := hierarchy(want, prefix)

		for _, size := range []int{0, 1, 2, 5} {
			blobs, prefixes := listHierarchy(t, e.app, prefix, size)

			if !slices.Equal(blobs, wantBlobs) || !slices.Equal(prefixes, wantPrefixes) {
				t.Fatalf("hierarchy %q, pages of %d:\ngot  %v %v\nwant %v %v", prefix, size, blobs, prefixes, wantBlobs, wantPrefixes)
			}
		}
	}

	// The fork's copy is the one listed.
	pager := e.app.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Include: container.ListBlobsInclude{Metadata: true}})

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}

		for _, item := range page.Segment.BlobItems {
			fromLab := item.Metadata["origin"] != nil || item.Metadata["Origin"] != nil

			if body, _ := e.read(t, *item.Name); fromLab != (body == "lab") {
				t.Fatalf("%s listed from the wrong layer: metadata %v, body %q", *item.Name, item.Metadata, body)
			}
		}
	}
}

// A marker that isn't blobfork's is turned down.
func TestListingBadMarker(t *testing.T) {
	e := setup(t)

	pager := e.app.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Marker: to.Ptr("someone-else's")})

	_, err := pager.NextPage(t.Context())
	if !bloberror.HasCode(err, bloberror.OutOfRangeInput) {
		t.Fatalf("bad marker: %v", err)
	}
}

func listFlat(t *testing.T, app *container.Client, size int) []string {
	t.Helper()

	options := &container.ListBlobsFlatOptions{}
	if size > 0 {
		options.MaxResults = to.Ptr(int32(size))
	}

	var names []string

	pager := app.NewListBlobsFlatPager(options)

	for pager.More() {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		if size > 0 && len(page.Segment.BlobItems) > size {
			t.Fatalf("page of %d for %d", len(page.Segment.BlobItems), size)
		}

		for _, item := range page.Segment.BlobItems {
			names = append(names, *item.Name)
		}
	}

	return names
}

func listHierarchy(t *testing.T, app *container.Client, prefix string, size int) (blobs, prefixes []string) {
	t.Helper()

	options := &container.ListBlobsHierarchyOptions{Prefix: to.Ptr(prefix)}
	if size > 0 {
		options.MaxResults = to.Ptr(int32(size))
	}

	pager := app.NewListBlobsHierarchyPager("/", options)

	for pager.More() {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		for _, item := range page.Segment.BlobItems {
			blobs = append(blobs, *item.Name)
		}

		for _, item := range page.Segment.BlobPrefixes {
			prefixes = append(prefixes, *item.Name)
		}
	}

	return blobs, prefixes
}

// hierarchy is what a listing with the delimiter "/" has under a prefix.
func hierarchy(names []string, prefix string) (blobs, prefixes []string) {
	seen := map[string]bool{}

	for _, name := range names {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}

		i := strings.Index(rest, "/")
		if i < 0 {
			blobs = append(blobs, name)

			continue
		}

		if p := prefix + rest[:i+1]; !seen[p] {
			seen[p] = true
			prefixes = append(prefixes, p)
		}
	}

	return blobs, prefixes
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

func stderr(err error) []byte {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.Stderr
	}

	return nil
}
