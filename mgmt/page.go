package mgmt

import (
	"encoding/base64"
	"encoding/json"
	"errors"
)

// Pagination (/v1): every list is ordered by a stable key and a page is
// "the first Limit rows after the key", so concurrent inserts never shift
// or duplicate rows across pages. Keys are (created_at, id) for tenants,
// hosts, deployments and objects, and (ts, chain, seq) for audit. The
// cursor the API hands out is the base64url JSON of the last row's key.

// PageDefaultLimit and PageMaxLimit are the /v1 list defaults.
const (
	PageDefaultLimit = 50
	PageMaxLimit     = 500
)

// Page is a store list window. A zero Page means "everything".
type Page struct {
	Limit int      // 0 = unlimited
	After *PageKey // exclusive
}

// PageKey is the sort key of a row.
type PageKey struct {
	Created int64  `json:"c,omitempty"` // created_at (unix ms)
	ID      string `json:"i,omitempty"`
	TS      string `json:"t,omitempty"` // audit: ts
	Chain   string `json:"h,omitempty"` // audit: chain
	Seq     int64  `json:"q,omitempty"` // audit: seq
}

// ErrBadCursor is returned by DecodeCursor.
var ErrBadCursor = errors.New("bad_cursor")

// EncodeCursor turns a key into an opaque cursor.
func EncodeCursor(k PageKey) string {
	b, _ := json.Marshal(k)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses a cursor ("" → nil).
func DecodeCursor(s string) (*PageKey, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrBadCursor
	}
	var k PageKey
	if err := json.Unmarshal(b, &k); err != nil || (k.ID == "" && k.TS == "") {
		return nil, ErrBadCursor
	}
	return &k, nil
}

// ClampLimit applies the API defaults: 0 → default, >max → max.
func ClampLimit(n int) int {
	switch {
	case n <= 0:
		return PageDefaultLimit
	case n > PageMaxLimit:
		return PageMaxLimit
	}
	return n
}

// pageOf trims rows fetched with Limit+1 and returns the next cursor.
func pageOf[T any](rows []T, limit int, key func(T) PageKey) ([]T, *string) {
	if limit <= 0 || len(rows) <= limit {
		return rows, nil
	}
	rows = rows[:limit]
	c := EncodeCursor(key(rows[len(rows)-1]))
	return rows, &c
}
