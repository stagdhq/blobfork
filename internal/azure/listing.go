package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"slices"
	"strconv"
	"strings"
)

// merge is one page of List Blobs over both layers.
//
// Both layers are asked the same and answer in name order, so the merge
// walks them side by side: a name in both is the fork's, a lower blob a
// marker hides is left out, and a prefix only the lower layer has is left out
// when every blob under it is deleted.
//
// Azure's markers are opaque, and List Blobs can't start after a name, so
// blobfork's marker holds each layer's (the one its unread entries came
// from) and the last name answered. The next page asks each layer again from
// its marker and skips the names up to that one: in order, with no repeats or
// gaps, though the layers' pages don't line up.
type merge struct {
	c *forked

	prefix    string
	delimiter string
	include   string
	version   string
	max       int
}

// cursorState is where a layer's listing stands, in blobfork's marker.
type cursorState struct {
	Marker string `json:"m,omitempty"`
	Done   bool   `json:"d,omitempty"`
}

type markerState struct {
	Upper cursorState `json:"u"`
	Lower cursorState `json:"l"`
	After string      `json:"a"`
}

const markerPrefix = "bf1."

var errBadMarker = errors.New("not a blobfork marker")

func decodeMarker(raw string) (markerState, error) {
	var state markerState

	data, ok := strings.CutPrefix(raw, markerPrefix)
	if !ok {
		return state, errBadMarker
	}

	decoded, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		return state, errBadMarker
	}

	if err := json.Unmarshal(decoded, &state); err != nil {
		return state, errBadMarker
	}

	return state, nil
}

func encodeMarker(state markerState) string {
	data, _ := json.Marshal(state)

	return markerPrefix + base64.RawURLEncoding.EncodeToString(data)
}

// Snapshots, versions and soft-deleted blobs repeat a name, which the merge
// by name can't page through: left out of both layers' listings. Uncommitted
// blobs are the fork's own.
var (
	includeNeither = map[string]bool{"snapshots": true, "versions": true, "deleted": true, "deletedwithversions": true}
	includeUpper   = map[string]bool{"uncommittedblobs": true}
)

func include(raw string, lower bool) string {
	var kept []string

	for item := range strings.SplitSeq(raw, ",") {
		item = strings.TrimSpace(item)

		if item == "" || includeNeither[strings.ToLower(item)] || lower && includeUpper[strings.ToLower(item)] {
			continue
		}

		kept = append(kept, item)
	}

	return strings.Join(kept, ",")
}

// answer is the page's body, in Azure's shape: head is Azurite's answer to
// the app's own request, for the parts that aren't entries.
func (m *merge) answer(ctx context.Context, head *enumeration, marker string, hasMax bool) ([]byte, error) {
	var state markerState

	if marker != "" {
		var err error

		state, err = decodeMarker(marker)
		if err != nil {
			return nil, err
		}
	}

	upper := m.cursor(m.c.upper, state.Upper, state.After, false)
	lower := m.cursor(m.c.lower, state.Lower, state.After, true)

	var out []Entry

	for len(out) < m.max {
		u, err := upper.peek(ctx)
		if err != nil {
			return nil, err
		}

		l, err := lower.peek(ctx)
		if err != nil {
			return nil, err
		}

		if u == nil && l == nil {
			break
		}

		switch {
		case l == nil || u != nil && u.Name < l.Name:
			upper.pos++
			out = append(out, *u)

		case u == nil || l.Name < u.Name:
			lower.pos++

			visible, err := m.visible(ctx, l)
			if err != nil {
				return nil, err
			}

			if visible {
				out = append(out, *l)
			}

		default:
			upper.pos++
			lower.pos++
			out = append(out, *u)
		}
	}

	next := ""

	if len(out) > 0 {
		u, err := upper.peek(ctx)
		if err != nil {
			return nil, err
		}

		l, err := lower.peek(ctx)
		if err != nil {
			return nil, err
		}

		if u != nil || l != nil {
			next = encodeMarker(markerState{
				Upper: upper.state(u == nil),
				Lower: lower.state(l == nil),
				After: out[len(out)-1].Name,
			})
		}
	}

	var b bytes.Buffer

	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><EnumerationResults ServiceEndpoint="`)
	xml.EscapeText(&b, []byte(head.ServiceEndpoint))
	b.WriteString(`" ContainerName="`)
	xml.EscapeText(&b, []byte(head.ContainerName))
	b.WriteString(`">`)

	element(&b, "Prefix", m.prefix)
	element(&b, "Marker", marker)

	if hasMax {
		element(&b, "MaxResults", strconv.Itoa(m.max))
	}

	element(&b, "Delimiter", m.delimiter)

	b.WriteString("<Blobs>")

	for _, entry := range out {
		b.Write(entry.XML)
	}

	b.WriteString("</Blobs>")

	if next == "" {
		b.WriteString("<NextMarker />")
	} else {
		element(&b, "NextMarker", next)
	}

	b.WriteString("</EnumerationResults>")

	return b.Bytes(), nil
}

func element(b *bytes.Buffer, name, value string) {
	if value == "" {
		return
	}

	b.WriteString("<" + name + ">")
	xml.EscapeText(b, []byte(value))
	b.WriteString("</" + name + ">")
}

// visible says whether a lower entry shows: a blob unless a marker hides it,
// a prefix unless every blob under it is deleted.
func (m *merge) visible(ctx context.Context, entry *Entry) (bool, error) {
	if !entry.Prefix {
		return !m.c.markers.has(entry.Name), nil
	}

	if !m.c.markers.under(entry.Name) {
		return true, nil
	}

	marker := ""

	for {
		page, err := m.c.lower.List(ctx, ListQuery{
			Prefix:     entry.Name,
			MaxResults: 1000,
			Marker:     marker,
			Version:    m.version,
		})
		if err != nil {
			return false, err
		}

		for _, blob := range page.Entries {
			if !m.c.markers.has(blob.Name) {
				return true, nil
			}
		}

		if page.NextMarker == "" {
			return false, nil
		}

		marker = page.NextMarker
	}
}

func (m *merge) cursor(layer Layer, state cursorState, after string, lower bool) *cursor {
	return &cursor{
		layer: layer,
		query: ListQuery{
			Prefix:     m.prefix,
			Delimiter:  m.delimiter,
			Include:    include(m.include, lower),
			MaxResults: m.max,
			Version:    m.version,
		},
		marker: state.Marker,
		done:   state.Done,
		after:  after,
	}
}

// cursor walks one layer's listing, a page at a time, skipping the names up
// to after.
type cursor struct {
	layer Layer
	query ListQuery
	after string

	// marker is where the page in hand came from.
	marker string
	done   bool

	page   []Entry
	next   string
	pos    int
	loaded bool
}

// peek is the next entry, nil at the end.
func (c *cursor) peek(ctx context.Context) (*Entry, error) {
	for !c.done {
		if !c.loaded {
			c.query.Marker = c.marker

			page, err := c.layer.List(ctx, c.query)
			if err != nil {
				return nil, err
			}

			c.page = c.page[:0]

			for _, entry := range page.Entries {
				if c.after == "" || entry.Name > c.after {
					c.page = append(c.page, entry)
				}
			}

			// A page covers a range of names, but not always in order:
			// Azurite puts a page's prefixes before its blobs.
			slices.SortStableFunc(c.page, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })

			c.next = page.NextMarker
			c.pos = 0
			c.loaded = true
		}

		if c.pos < len(c.page) {
			return &c.page[c.pos], nil
		}

		if c.next == "" {
			c.done = true

			break
		}

		c.marker = c.next
		c.loaded = false
	}

	return nil, nil
}

// state is where the next page starts: from the page in hand, its read
// entries skipped by name.
func (c *cursor) state(ended bool) cursorState {
	if ended {
		return cursorState{Done: true}
	}

	return cursorState{Marker: c.marker}
}
