package s3

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// merge is one page of ListObjects (V1 or V2) over both layers.
//
// Both layers are asked the same and answer in name order, so the merge
// walks them side by side: a name in both is the fork's, a lower object a
// marker hides is left out, and a prefix only the lower layer has is left out
// when every object under it is deleted.
//
// S3 can start a listing after a name (start-after), so blobfork's
// continuation token is only the last name it answered with: the next page
// asks each layer to start after it. A V1 listing's marker is that name
// already.
type merge struct {
	fork *Fork

	v1         bool
	bucket     string
	prefix     string
	delimiter  string
	encoding   string
	fetchOwner bool
	attributes string
	max        int
}

const tokenPrefix = "bf1."

var errBadToken = errors.New("not a blobfork continuation token")

func decodeToken(raw string) (string, error) {
	data, ok := strings.CutPrefix(raw, tokenPrefix)
	if !ok {
		return "", errBadToken
	}

	after, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		return "", errBadToken
	}

	return string(after), nil
}

func encodeToken(after string) string {
	return tokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(after))
}

// answer is the page's body, in S3's shape, for the app's query.
func (m *merge) answer(ctx context.Context, query url.Values) ([]byte, error) {
	after := query.Get("start-after")

	if m.v1 {
		after = query.Get("marker")
	}

	if token := query.Get("continuation-token"); token != "" && !m.v1 {
		var err error

		after, err = decodeToken(token)
		if err != nil {
			return nil, err
		}
	}

	upper := m.cursor(m.fork.upper, after)
	lower := m.cursor(m.fork.Lower, after)

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

	truncated := false

	if len(out) > 0 {
		u, err := upper.peek(ctx)
		if err != nil {
			return nil, err
		}

		l, err := lower.peek(ctx)
		if err != nil {
			return nil, err
		}

		truncated = u != nil || l != nil
	}

	var b bytes.Buffer

	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)

	element(&b, "Name", m.bucket, true)
	element(&b, "Prefix", m.echo(m.prefix), true)

	if m.v1 {
		element(&b, "Marker", m.echo(query.Get("marker")), true)

		if truncated {
			element(&b, "NextMarker", m.echo(out[len(out)-1].Name), true)
		}
	} else {
		element(&b, "StartAfter", m.echo(query.Get("start-after")), false)
		element(&b, "ContinuationToken", query.Get("continuation-token"), false)
		element(&b, "KeyCount", strconv.Itoa(len(out)), true)

		if truncated {
			element(&b, "NextContinuationToken", encodeToken(out[len(out)-1].Name), true)
		}
	}

	element(&b, "MaxKeys", strconv.Itoa(m.max), true)
	element(&b, "Delimiter", m.echo(m.delimiter), false)
	element(&b, "EncodingType", m.encoding, false)
	element(&b, "IsTruncated", strconv.FormatBool(truncated), true)

	// S3 lists the objects, then the prefixes.
	for _, entry := range out {
		if !entry.Prefix {
			b.Write(entry.XML)
		}
	}

	for _, entry := range out {
		if entry.Prefix {
			b.Write(entry.XML)
		}
	}

	b.WriteString("</ListBucketResult>")

	return b.Bytes(), nil
}

// echo is a name as the answer has it: URL-encoded with encoding-type=url.
func (m *merge) echo(name string) string {
	if m.encoding == "url" {
		return url.QueryEscape(name)
	}

	return name
}

func element(b *bytes.Buffer, name, value string, always bool) {
	if value == "" && !always {
		return
	}

	b.WriteString("<" + name + ">")
	xml.EscapeText(b, []byte(value))
	b.WriteString("</" + name + ">")
}

// visible says whether a lower entry shows: an object unless a marker hides
// it, a prefix unless every object under it is deleted.
func (m *merge) visible(ctx context.Context, entry *Entry) (bool, error) {
	if !entry.Prefix {
		return !m.fork.markers.has(entry.Name), nil
	}

	if !m.fork.markers.under(entry.Name) {
		return true, nil
	}

	token := ""

	for {
		page, err := m.fork.Lower.List(ctx, ListQuery{Prefix: entry.Name, EncodingType: "url", Token: token})
		if err != nil {
			return false, err
		}

		for _, object := range page.Entries {
			if !m.fork.markers.has(object.Name) {
				return true, nil
			}
		}

		if page.Next == "" {
			return false, nil
		}

		token = page.Next
	}
}

func (m *merge) cursor(layer Layer, after string) *cursor {
	return &cursor{
		layer: layer,
		query: ListQuery{
			Prefix:       m.prefix,
			Delimiter:    m.delimiter,
			EncodingType: m.encoding,
			FetchOwner:   m.fetchOwner,
			MaxKeys:      m.max,
			StartAfter:   after,
			Attributes:   m.attributes,
		},
		after: after,
	}
}

// cursor walks one layer's listing, a page at a time, skipping the names up
// to after: a prefix the last page ended with comes again.
type cursor struct {
	layer Layer
	query ListQuery
	after string

	page   []Entry
	pos    int
	next   string
	loaded bool
	done   bool
}

// peek is the next entry, nil at the end.
func (c *cursor) peek(ctx context.Context) (*Entry, error) {
	for !c.done {
		if !c.loaded {
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

			// S3 answers the objects, then the prefixes.
			slices.SortStableFunc(c.page, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })

			c.next = page.Next
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

		c.query.Token = c.next
		c.loaded = false
	}

	return nil, nil
}
