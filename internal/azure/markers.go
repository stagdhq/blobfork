package azure

import (
	"context"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/stagdhq/blobfork/internal/telemetry"
)

// markers are the lower blobs the app has deleted: an empty blob each in the
// hidden container (<container>/<name>), read once at start and kept in
// memory, as blobfork is the only one writing them.
//
// A marker hides the lower layer's blob only: a blob the fork has is there,
// marker or not, so writing a deleted name again needs nothing undone.
type markers struct {
	hidden *container.Client
	prefix string

	mu    sync.RWMutex
	names map[string]struct{}
}

func loadMarkers(ctx context.Context, hidden *container.Client, forked string) (*markers, error) {
	m := &markers{
		hidden: hidden,
		prefix: forked + "/",
		names:  map[string]struct{}{},
	}

	pager := hidden.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: to.Ptr(m.prefix)})

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		for _, item := range page.Segment.BlobItems {
			m.names[strings.TrimPrefix(*item.Name, m.prefix)] = struct{}{}
		}
	}

	return m, nil
}

func (m *markers) has(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, ok := m.names[name]

	return ok
}

// under says whether any marker is under a prefix.
func (m *markers) under(prefix string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for name := range m.names {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}

	return false
}

func (m *markers) count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.names)
}

// add writes a marker, then keeps it: one that isn't written isn't kept.
func (m *markers) add(ctx context.Context, name string) error {
	_, err := m.hidden.NewBlockBlobClient(m.prefix+name).UploadBuffer(ctx, []byte{}, nil)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.names[name] = struct{}{}
	m.mu.Unlock()

	telemetry.Hidden.Add(ctx, 1, telemetry.Attrs("azure", strings.TrimSuffix(m.prefix, "/")))

	return nil
}
