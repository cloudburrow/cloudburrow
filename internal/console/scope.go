package console

import (
	"context"
	"net/http"
	"net/url"
)

// Scoper is a provider whose screens address a scope narrower than the
// project, chosen with a picker on its list screen and carried in the URL as
// ?<ScopeKey>=, the way the project is: Bigtable's tables live in an
// instance (#1360).
//
// The client carries the key on every request it makes for this service, and
// on every link within its screens, so a table's page, its rows and its
// actions all address the instance its list was read from. The provider reads
// the value with ScopeValue and says what it may be with Listing.Scope.
type Scoper interface {
	ScopeKey() string
}

// Scope is a list screen's picker for a Scoper's scope.
type Scope struct {
	// Key is the query parameter the choice travels in.
	Key string `json:"key"`
	// Label names what is being chosen: "Instance".
	Label string `json:"label"`
	// Selected is the option in force.
	Selected string `json:"selected"`
	// Default is the option meant when the URL names none, which the client
	// leaves out of the address rather than writing it in.
	Default string `json:"default,omitempty"`
	// Options are the choices, Selected among them.
	Options []ScopeOption `json:"options"`
}

// ScopeOption is one choice in a Scope.
type ScopeOption struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

type scopeContextKey struct{}

// withScope makes the request's query parameters readable by ScopeValue from
// any context derived from the request's.
func withScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), scopeContextKey{}, r.URL.Query())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ScopeValue is the ?key= of the console request ctx serves, or "" when it
// names none or ctx serves no request.
func ScopeValue(ctx context.Context, key string) string {
	q, _ := ctx.Value(scopeContextKey{}).(url.Values)
	return q.Get(key)
}

// WithScopeValue returns a context in which ScopeValue(key) is value, for a
// caller that drives a provider outside an HTTP request: a test, or a search.
func WithScopeValue(ctx context.Context, key, value string) context.Context {
	q := url.Values{}
	if prev, ok := ctx.Value(scopeContextKey{}).(url.Values); ok {
		for k, v := range prev {
			q[k] = append([]string(nil), v...)
		}
	}
	q.Set(key, value)
	return context.WithValue(ctx, scopeContextKey{}, q)
}
