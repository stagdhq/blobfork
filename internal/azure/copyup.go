package azure

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"

	"github.com/stagdhq/blobfork/internal/telemetry"
)

// copyUp copies a lower blob into the fork, unless the fork has it or a
// marker hides it: its body, content headers and metadata. Block blobs only.
//
// It doesn't overwrite: a write the app made meanwhile wins.
func (f *Fork) copyUp(ctx context.Context, c *forked, name string) error {
	if c.markers.has(name) {
		return nil
	}

	client := c.client.NewBlockBlobClient(name)

	_, err := client.GetProperties(ctx, nil)
	if err == nil {
		return nil
	}

	if !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return fmt.Errorf("copy up %s: %w", name, err)
	}

	res, err := c.lower.Blob(ctx, http.MethodGet, name, "", http.Header{})
	if err != nil {
		return fmt.Errorf("copy up %s: %w", name, err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotFound {
		return nil
	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("copy up %s: lower layer: %s", name, res.Status)
	}

	if res.Header.Get("X-Ms-Blob-Type") != "BlockBlob" {
		return nil
	}

	headers := &blob.HTTPHeaders{
		BlobContentType:        header(res, "Content-Type"),
		BlobContentEncoding:    header(res, "Content-Encoding"),
		BlobContentLanguage:    header(res, "Content-Language"),
		BlobContentDisposition: header(res, "Content-Disposition"),
		BlobCacheControl:       header(res, "Cache-Control"),
	}

	if sum, err := base64.StdEncoding.DecodeString(res.Header.Get("Content-Md5")); err == nil && len(sum) > 0 {
		headers.BlobContentMD5 = sum
	}

	metadata := map[string]*string{}

	for key, values := range res.Header {
		if name, ok := strings.CutPrefix(strings.ToLower(key), "x-ms-meta-"); ok && len(values) > 0 {
			metadata[name] = to.Ptr(values[0])
		}
	}

	_, err = client.UploadStream(ctx, res.Body, &blockblob.UploadStreamOptions{
		HTTPHeaders: headers,
		Metadata:    metadata,
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETagAny)},
		},
	})

	if bloberror.HasCode(err, bloberror.BlobAlreadyExists, bloberror.ConditionNotMet) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("copy up %s: %w", name, err)
	}

	f.Log.Debug("blobfork: copied up", "container", c.name, "blob", name)
	telemetry.CopyUps.Add(ctx, 1, telemetry.Attrs("azure", c.name))
	telemetry.CopyUpBytes.Add(ctx, max(res.ContentLength, 0), telemetry.Attrs("azure", c.name))

	return nil
}

func header(res *http.Response, key string) *string {
	value := res.Header.Get(key)
	if value == "" {
		return nil
	}

	return &value
}
