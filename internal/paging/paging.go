// Package paging provides pagination primitives with deterministic ordering.
//
// Invalid page tokens are rejected rather than silently treated as a first
// page: silently restarting a listing makes a client loop forever without ever
// reporting an error.
package paging

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ErrInvalidToken means a page token was not produced by this service.
var ErrInvalidToken = errors.New("invalid page token")

// DefaultPageSize is used when a caller asks for none.
const DefaultPageSize = 100

// MaxPageSize caps what a caller can request.
const MaxPageSize = 1000

// token is the opaque cursor. It carries the listing identity so a token from
// one listing cannot be replayed against another and silently skip results.
type token struct {
	// After is the last key returned. Listing resumes strictly after it.
	After string `json:"a"`
	// Scope identifies the listing (collection plus filter), so a token from a
	// different listing is rejected instead of quietly misapplied.
	Scope string `json:"s"`
}

// EncodeToken builds an opaque page token.
func EncodeToken(scope, after string) string {
	b, err := json.Marshal(token{After: after, Scope: scope})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeToken parses a page token, verifying it belongs to this listing.
func DecodeToken(scope, s string) (after string, err error) {
	if s == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("%w: not valid base64", ErrInvalidToken)
	}
	var t token
	if err := json.Unmarshal(b, &t); err != nil {
		return "", fmt.Errorf("%w: not a recognised token", ErrInvalidToken)
	}
	if t.Scope != scope {
		return "", fmt.Errorf("%w: token belongs to a different listing", ErrInvalidToken)
	}
	return t.After, nil
}

// ClampPageSize applies the default and the maximum.
func ClampPageSize(requested int) int {
	switch {
	case requested <= 0:
		return DefaultPageSize
	case requested > MaxPageSize:
		return MaxPageSize
	default:
		return requested
	}
}

// Page selects one page from a set of keys.
//
// Keys are sorted before slicing, so the order does not depend on map
// iteration or insertion order. Without that, the same listing could return
// the same item twice across pages, or skip one entirely.
func Page(scope string, keys []string, pageToken string, pageSize int) (page []string, nextToken string, err error) {
	after, err := DecodeToken(scope, pageToken)
	if err != nil {
		return nil, "", err
	}
	size := ClampPageSize(pageSize)

	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)

	start := 0
	if after != "" {
		// Resume strictly after the cursor, so a deleted cursor item does not
		// cause the page to restart.
		start = sort.SearchStrings(sorted, after)
		if start < len(sorted) && sorted[start] == after {
			start++
		}
	}
	if start >= len(sorted) {
		return nil, "", nil
	}

	end := start + size
	if end > len(sorted) {
		end = len(sorted)
	}
	page = sorted[start:end]

	if end < len(sorted) {
		nextToken = EncodeToken(scope, page[len(page)-1])
	}
	return page, nextToken, nil
}
