package authplatform

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Paths the metadata server serves this package's endpoints at: Google's
// authorization endpoint's (accounts.google.com/o/oauth2/v2/auth, and the
// older /o/oauth2/auth) and the revocation endpoint's
// (oauth2.googleapis.com/revoke). The token endpoint is the metadata
// server's /token, which hands this package the requests of its clients.
const (
	AuthorizePath   = "/o/oauth2/v2/auth"
	AuthorizePathV1 = "/o/oauth2/auth"
	RevokePath      = "/revoke"
)

// Endpoints serves the authorization and revocation endpoints over a
// Store, signing the instance's user in.
type Endpoints struct {
	Store *Store
	// User is the account a local sign-in is: the instance's user.
	User string

	mu      sync.Mutex
	pending map[string]pendingAuth
}

type pendingAuth struct {
	req     *AuthRequest
	expires time.Time
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .Err}}Error{{else}}Sign in to {{.App}}{{end}} - CloudBurrow</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;margin:0;background:#f1f3f4;color:#202124}
main{max-width:30rem;margin:3rem auto;background:#fff;border:1px solid #dadce0;border-radius:8px;padding:2rem}
h1{font-size:1.5rem;font-weight:400;margin:0 0 1rem}
p,li{line-height:1.5}
.err{color:#a50e0e}
.actions{display:flex;gap:1rem;justify-content:flex-end;margin-top:2rem}
button{font:inherit;padding:.5rem 1.5rem;border-radius:4px;border:1px solid #dadce0;background:#fff;color:#1a73e8;cursor:pointer}
button.primary{background:#1a73e8;color:#fff;border-color:#1a73e8}
button:focus-visible{outline:3px solid #174ea6;outline-offset:2px}
.mono{font-family:ui-monospace,monospace;word-break:break-all}
</style></head><body><main>
{{if .Err}}<h1>Access blocked: {{.App}}</h1>
<p class="err" id="oauth-error"><strong>Error {{.Code}}: {{.OAuth}}</strong></p><p>{{.Err}}</p>
{{else}}<h1 id="oauth-title">{{.App}} wants to access your account</h1>
<p>Signed in as <strong id="oauth-user">{{.User}}</strong>, the user of this CloudBurrow instance.</p>
<p>This will allow {{.App}} to:</p><ul id="oauth-scopes">{{range .Scopes}}<li class="mono">{{.}}</li>{{end}}</ul>
<p>Support: {{.Support}}{{if .Privacy}} · <a href="{{.Privacy}}">Privacy Policy</a>{{end}}{{if .Terms}} · <a href="{{.Terms}}">Terms of Service</a>{{end}}</p>
<form method="post" action="{{.Action}}"><input type="hidden" name="nonce" value="{{.Nonce}}">
<div class="actions"><button type="submit" name="decision" value="deny" id="oauth-cancel">Cancel</button>
<button type="submit" name="decision" value="allow" class="primary" id="oauth-allow">Allow</button></div></form>
{{end}}</main></body></html>`))

type pageData struct {
	App, User, Support, Privacy, Terms, Action, Nonce string
	Scopes                                            []string
	Err, OAuth                                        string
	Code                                              int
}

func (e *Endpoints) page(w http.ResponseWriter, code int, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(code)
	_ = consentPage.Execute(w, d)
}

// ServeHTTP is the authorization endpoint (GET: the consent screen; POST:
// its Allow or Cancel) and the revocation endpoint.
func (e *Endpoints) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == RevokePath {
		e.revoke(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		a, err := e.Store.CheckAuthorization(r.URL.Query(), e.User)
		if err != nil {
			ae, ok := err.(*Error)
			if !ok {
				ae = errorf(500, "server_error", "%v", err)
			}
			e.page(w, ae.Code, pageData{App: "this app", Err: ae.Detail, OAuth: ae.OAuth, Code: ae.Code})
			return
		}
		nonce := randomString(alnum, 32)
		e.mu.Lock()
		if e.pending == nil {
			e.pending = map[string]pendingAuth{}
		}
		for k, p := range e.pending {
			if time.Now().After(p.expires) {
				delete(e.pending, k)
			}
		}
		e.pending[nonce] = pendingAuth{req: a, expires: time.Now().Add(10 * time.Minute)}
		e.mu.Unlock()
		e.page(w, http.StatusOK, pageData{App: a.Brand.AppName, User: e.User, Support: a.Brand.SupportEmail,
			Privacy: a.Brand.PrivacyPolicy, Terms: a.Brand.TermsOfService, Action: r.URL.Path, Nonce: nonce,
			Scopes: strings.Fields(a.Scope)})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "malformed form", http.StatusBadRequest)
			return
		}
		e.mu.Lock()
		p, ok := e.pending[r.PostForm.Get("nonce")]
		delete(e.pending, r.PostForm.Get("nonce"))
		e.mu.Unlock()
		if !ok || time.Now().After(p.expires) {
			e.page(w, http.StatusBadRequest, pageData{App: "this app", Err: "The sign-in request expired. Start again from the app.", OAuth: "invalid_request", Code: 400})
			return
		}
		to := Deny(p.req)
		if r.PostForm.Get("decision") == "allow" {
			to = e.Store.Approve(p.req, e.User)
		}
		http.Redirect(w, r, to, http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (e *Endpoints) revoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if !e.Store.Revoke(r.Form.Get("token")) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_token", "error_description": "Token expired or revoked"})
		return
	}
	_, _ = w.Write([]byte("{}"))
}
