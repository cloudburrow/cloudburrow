package firestoreadmin

// Client calls before any Rules release (#2050).
//
// On Google, a database with no released Security Rules denies every
// mobile and web client call, while server client libraries, which rules
// never bind, still pass. The emulator instead allows everything in a
// project it holds no rules for. The CLI's Firebase Rules API puts a
// project's release in force, or the deny-all rules once its last release
// is deleted (internal/service/firebaserules), by PUT
// /emulator/v1/projects/P:securityRules through this front; the front
// notes each project whose rules were set that way. When a client call
// (one ClientAuth did not send as the owner) names a project the front has
// seen no rules for, the front first sets that project's rules to
// DenyRules, so the emulator itself denies the call, in each transport,
// as PERMISSION_DENIED. The front keeps each project's rules as last set,
// and sets them all again when the emulator restarts empty (a compaction's
// JVM restart, documents.go's restore), which keeps no rules.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DenyRules deny every client call, as Google does with no release.
const DenyRules = `rules_version = '2';
service cloud.firestore {
  match /databases/{database}/documents {
    match /{document=**} {
      allow read, write: if false;
    }
  }
}
`

// rulesGate is the projects whose rules are set in the emulator.
type rulesGate struct {
	a *Admin
	// mu orders the CLI's rules and the front's own deny-all, so the
	// front never overwrites a release it saw set.
	mu sync.Mutex
	// set is each project's rules request as last set.
	set map[string][]byte
}

var (
	rulesPutPath  = regexp.MustCompile(`^/emulator/v1/projects/([^/:]+):securityRules$`)
	restProject   = regexp.MustCompile(`^/v1/projects/([^/]+)/`)
	headerProject = regexp.MustCompile(`projects(?:/|%2F|%2f)([^/%&,\s]+)`)
)

// RulesGate sets deny-all rules for a project a client calls before any
// release. It goes inside ClientAuth, which marks server calls as the
// owner.
func (a *Admin) RulesGate(next http.Handler) http.Handler {
	g := &rulesGate{a: a, set: map[string][]byte{}}
	a.rulesGate.Store(g)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m := rulesPutPath.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodPut {
			p, _ := url.PathUnescape(m[1])
			body, err := io.ReadAll(io.LimitReader(r.Body, maxCapture))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			g.mu.Lock()
			defer g.mu.Unlock()
			sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
			next.ServeHTTP(sw, r)
			if sw.code/100 == 2 {
				g.set[p] = body
			}
			return
		}
		if isDocumentsCall(r) && r.Header.Get("Authorization") != ownerToken {
			if p := callProject(r); p != "" {
				g.ensure(r.Context(), p)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ensure sets project's rules to DenyRules unless rules were set for it.
func (g *rulesGate) ensure(ctx context.Context, project string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.set[project] != nil || g.a.emulator == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"rules": map[string]any{
		"files": []map[string]string{{"name": "firestore.rules", "content": DenyRules}}}})
	if err := g.put(ctx, project, body); err != nil {
		if g.a.logf != nil {
			g.a.logf("firestore: could not set the deny-all rules of %s: %v", project, err)
		}
		return
	}
	g.set[project] = body
}

// put sets project's rules in the emulator, as the owner.
func (g *rulesGate) put(ctx context.Context, project string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+g.a.emulator+"/emulator/v1/projects/"+url.PathEscape(project)+":securityRules", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", ownerToken)
	resp, err := g.a.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// restoreRules sets every project's rules again in an emulator that
// started empty.
func (a *Admin) restoreRules(ctx context.Context) {
	g := a.rulesGate.Load()
	if g == nil || a.emulator == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for p, body := range g.set {
		if err := g.put(ctx, p, body); err != nil && a.logf != nil {
			a.logf("firestore: could not set the rules of %s again: %v", p, err)
		}
	}
}

// callProject is the project a documents call names: in its REST path,
// the gRPC routing headers, or the WebChannel's database parameter.
func callProject(r *http.Request) string {
	if m := restProject.FindStringSubmatch(r.URL.Path); m != nil {
		return m[1]
	}
	for _, v := range []string{r.Header.Get("Google-Cloud-Resource-Prefix"), r.Header.Get("X-Goog-Request-Params"), r.URL.Query().Get("database")} {
		if m := headerProject.FindStringSubmatch(v); m != nil {
			p, _ := url.PathUnescape(m[1])
			return strings.TrimSpace(p)
		}
	}
	return ""
}

// statusWriter keeps an answer's status code.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
