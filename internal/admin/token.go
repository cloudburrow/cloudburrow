package admin

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// The admin token (#553). The control port binds loopback, and that was
// taken to keep cluster workloads out of /admin. On Docker Desktop it does
// not: a pod reaches the host's loopback through host.docker.internal, so
// any workload under test could reset, seed, export or fault the instance.
// `up` therefore mints a token per instance, keeps it in the state
// directory where only the developer can read it, and every /admin route
// requires it. Health and readiness stay open: they reveal nothing and
// change nothing.

// RequireToken makes every admin route require `Authorization: Bearer
// <token>`. An empty token leaves the routes open, for tests that build the
// API in process.
func (a *API) RequireToken(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = token
}

// authorized wraps a handler so a request without the token is refused
// before the handler runs, with 401 and a body that says where the token is.
func (a *API) authorized(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		token := a.token
		a.mu.Unlock()
		if token != "" {
			got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="cloudburrow admin"`)
				writeJSON(w, http.StatusUnauthorized, map[string]string{
					"error": "the admin API needs the instance's admin token: send `Authorization: Bearer <token>`, " +
						"where the token is the admin-token file in the instance's state directory",
				})
				return
			}
		}
		h(w, r)
	}
}
