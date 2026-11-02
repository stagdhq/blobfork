package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// markers are the lower objects the app has deleted: an empty object each in
// the hidden bucket (<bucket>/<key>), read once at start and kept in memory,
// as blobfork is the only one writing them.
//
// A marker hides the lower layer's object only: an object the fork has is
// there, marker or not, so writing a deleted key again needs nothing undone.
type markers struct {
	hidden *Bucket
	prefix string

	mu    sync.RWMutex
	names map[string]struct{}
}

func loadMarkers(ctx context.Context, hidden *Bucket, forked string) (*markers, error) {
	m := &markers{
		hidden: hidden,
		prefix: forked + "/",
		names:  map[string]struct{}{},
	}

	token := ""

	for {
		page, err := hidden.List(ctx, ListQuery{Prefix: m.prefix, EncodingType: "url", Token: token})
		if err != nil {
			return nil, err
		}

		for _, entry := range page.Entries {
			m.names[strings.TrimPrefix(entry.Name, m.prefix)] = struct{}{}
		}

		if page.Next == "" {
			return m, nil
		}

		token = page.Next
	}
}

func (m *markers) has(key string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, ok := m.names[key]

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
func (m *markers) add(ctx context.Context, key string) error {
	res, err := m.hidden.Do(ctx, http.MethodPut, m.prefix+key, nil, nil, bytes.NewReader(nil), 0)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)

		return fmt.Errorf("delete marker for %s: %s %s", key, res.Status, errorCode(body))
	}

	m.mu.Lock()
	m.names[key] = struct{}{}
	m.mu.Unlock()

	return nil
}
