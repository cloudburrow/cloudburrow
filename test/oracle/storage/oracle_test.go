//go:build oracle

// Package storageoracle is the Cloud Storage differential oracle (#497). It
// sends the same HTTP exchanges to the builtin server and to Google's
// storage-testbench, pinned by image digest in dependencies.json, and compares
// the status, selected headers and success bodies after normalising the
// values each server sets for itself (times, generations, etags, IDs, hosts).
//
// Agreement with the testbench is NOT an observation of Google, and never
// removes an UNVERIFIED annotation: the docs win. The oracle is never pointed
// at live Google. It runs only in CI (scripts/oracle-storage.sh), on a
// loopback with no network.
package storageoracle

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// EnvTestbench is the testbench's base URL, set by scripts/oracle-storage.sh.
const EnvTestbench = "CLOUDBURROW_ORACLE_TESTBENCH"

const project = "oracle-project"

// target is one server under comparison.
type target struct {
	name string
	base string
	c    *http.Client
}

// targets returns the testbench and a fresh in-process builtin server.
func targets(t *testing.T) (tb, cb *target) {
	t.Helper()
	base := os.Getenv(EnvTestbench)
	if base == "" {
		t.Fatalf("%s is unset: the oracle runs only through scripts/oracle-storage.sh", EnvTestbench)
	}
	c := &http.Client{
		Timeout: 30 * time.Second,
		// A redirect is an observation, not something to follow.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	waitReady(t, c, base)
	s, err := storage.NewServer(storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	return &target{"testbench", strings.TrimRight(base, "/"), c}, &target{"builtin", h.URL, c}
}

// waitReady polls the testbench's root, which answers "OK" once serving.
func waitReady(t *testing.T, c *http.Client, base string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		resp, err := c.Get(base + "/")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 && strings.TrimSpace(string(b)) == "OK" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("testbench at %s not ready: %v", base, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// req is one HTTP request, made the same way on each server. url is a path
// and query; a full URL (a resumable session URI) is used as is.
type req struct {
	method, url string
	header      map[string]string
	body        []byte
}

// observed is one exchange's normalised result, flattened to path → value.
type observed map[string]string

// do sends r and returns the response with its body.
func (tg *target) do(t *testing.T, r req) (*http.Response, []byte) {
	t.Helper()
	u := r.url
	if !strings.HasPrefix(u, "http") {
		u = tg.base + u
	}
	hr, err := http.NewRequest(r.method, u, bytes.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range r.header {
		hr.Header.Set(k, v)
	}
	if r.body != nil && hr.Header.Get("Content-Type") == "" {
		hr.Header.Set("Content-Type", "application/json")
	}
	resp, err := tg.c.Do(hr)
	if err != nil {
		t.Fatalf("%s %s %s: %v", tg.name, r.method, u, err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

// comparedHeaders are the response headers the wire protocol turns on. A
// header absent from one server and present on the other is a difference.
var comparedHeaders = []string{
	"Accept-Ranges",
	"Content-Range",
	"Location",
	"Range",
	"X-Goog-Generation",
	"X-Goog-Hash",
	"X-Goog-Metageneration",
	"X-Goog-Storage-Class",
	"X-Goog-Stored-Content-Encoding",
	"X-Goog-Stored-Content-Length",
	"X-Http-Status-Code-Override",
}

// observe normalises one response. Of an error only the status is
// compared (see notCompared). A media
// body is compared byte for byte; a JSON body field by field.
func (tg *target) observe(resp *http.Response, body []byte) observed {
	o := observed{"status": fmt.Sprint(resp.StatusCode)}
	if resp.StatusCode >= 400 {
		return o
	}
	for _, h := range comparedHeaders {
		if v := resp.Header.Get(h); v != "" {
			o["header:"+h] = tg.normalizeString(h, v)
		}
	}
	if len(body) == 0 {
		return o
	}
	var v any
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") && json.Unmarshal(body, &v) == nil {
		flatten(o, "body", tg.normalize("", v))
		return o
	}
	o["media"] = fmt.Sprintf("%d bytes %q", len(body), truncate(body))
	return o
}

func truncate(b []byte) []byte {
	if len(b) > 32 {
		return b[:32]
	}
	return b
}

// volatile are fields each server sets for itself: their presence is
// compared, never their value.
var volatile = map[string]bool{
	"etag": true, "generation": true, "id": true, "projectNumber": true,
	"rewriteToken": true, "timeCreated": true, "timeFinalized": true,
	"timeStorageClassUpdated": true, "updated": true,
	// Header values.
	"X-Goog-Generation": true,
}

var (
	generationRE = regexp.MustCompile(`generation=[0-9]+`)
	uploadIDRE   = regexp.MustCompile(`upload_id=[^&]+`)
)

// normalizeString rewrites a server's own host and IDs out of s.
func (tg *target) normalizeString(key, s string) string {
	if volatile[key] {
		return "<set>"
	}
	s = strings.ReplaceAll(s, tg.base, "<host>")
	s = generationRE.ReplaceAllString(s, "generation=<gen>")
	return uploadIDRE.ReplaceAllString(s, "upload_id=<id>")
}

func (tg *target) normalize(key string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			if key == "metadata" && isTestbenchMetadata(k) {
				continue
			}
			out[k] = tg.normalize(k, e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = tg.normalize(key, e)
		}
		return out
	case string:
		return tg.normalizeString(key, x)
	}
	return v
}

// isTestbenchMetadata reports a custom-metadata key the testbench adds to
// every object to record how it was uploaded (gcs/upload.py sets
// x_emulator_upload and friends). It is the testbench's bookkeeping, not
// Cloud Storage behaviour.
func isTestbenchMetadata(k string) bool {
	return strings.HasPrefix(k, "x_emulator_") || strings.HasPrefix(k, "x_testbench_")
}

func flatten(o observed, path string, v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			flatten(o, path+"."+k, e)
		}
	case []any:
		for i, e := range x {
			flatten(o, fmt.Sprintf("%s[%d]", path, i), e)
		}
	default:
		b, _ := json.Marshal(x)
		o[path] = string(b)
	}
}

// diff returns "<step> <path>" for every path whose value differs.
func diff(step string, tb, cb observed) map[string]string {
	out := map[string]string{}
	for k := range union(tb, cb) {
		a, aok := tb[k]
		b, bok := cb[k]
		switch {
		case !aok:
			out[step+" "+k] = fmt.Sprintf("testbench: absent, builtin: %s", b)
		case !bok:
			out[step+" "+k] = fmt.Sprintf("testbench: %s, builtin: absent", a)
		case a != b:
			out[step+" "+k] = fmt.Sprintf("testbench: %s, builtin: %s", a, b)
		}
	}
	return out
}

func union(a, b observed) map[string]bool {
	u := map[string]bool{}
	for k := range a {
		u[k] = true
	}
	for k := range b {
		u[k] = true
	}
	return u
}

// oracle runs steps on both servers and collects the differences.
type oracle struct {
	t      *testing.T
	tb, cb *target
	diffs  map[string]string
}

func newOracle(t *testing.T) *oracle {
	tb, cb := targets(t)
	return &oracle{t: t, tb: tb, cb: cb, diffs: map[string]string{}}
}

// step sends r to both servers, records the differences under name and
// returns both raw responses for a follow-up step.
func (o *oracle) step(name string, r req) (tbResp, cbResp *http.Response, tbBody, cbBody []byte) {
	o.t.Helper()
	tbResp, tbBody = o.tb.do(o.t, r)
	cbResp, cbBody = o.cb.do(o.t, r)
	for k, v := range diff(name, o.tb.observe(tbResp, tbBody), o.cb.observe(cbResp, cbBody)) {
		o.diffs[k] = v
	}
	return
}

// stepEach is step for a request that differs per server, such as a
// session URI or a rewrite token each server issued.
func (o *oracle) stepEach(name string, tbReq, cbReq req) (tbResp, cbResp *http.Response, tbBody, cbBody []byte) {
	o.t.Helper()
	tbResp, tbBody = o.tb.do(o.t, tbReq)
	cbResp, cbBody = o.cb.do(o.t, cbReq)
	for k, v := range diff(name, o.tb.observe(tbResp, tbBody), o.cb.observe(cbResp, cbBody)) {
		o.diffs[k] = v
	}
	return
}

// setup sends r to both servers and requires success from each; nothing
// is compared.
func (o *oracle) setup(r req) {
	o.t.Helper()
	for _, tg := range []*target{o.tb, o.cb} {
		if resp, b := tg.do(o.t, r); resp.StatusCode >= 300 {
			o.t.Fatalf("setup on %s: %s %s: %d %s", tg.name, r.method, r.url, resp.StatusCode, b)
		}
	}
}

// check fails for every difference no allowlist entry explains. Entries
// that explained something are recorded for TestMain's staleness check.
func (o *oracle) check() {
	o.t.Helper()
	for _, msg := range unexplained(o.diffs, allowlist, matched) {
		o.t.Error(msg)
	}
}

// matched records the allowlist entries that explained a difference in
// this run.
var matched = map[string]bool{}

// TestMain fails a full run in which an allowlist entry explained nothing,
// so the allowlist cannot silently grow stale. A run narrowed with -run
// skips the check, because it does not run every step.
func TestMain(m *testing.M) {
	flag.Parse()
	code := m.Run()
	if code == 0 && os.Getenv(EnvTestbench) != "" && flag.Lookup("test.run").Value.String() == "" {
		for _, msg := range stale(allowlist, matched) {
			fmt.Println(msg)
			code = 1
		}
	}
	os.Exit(code)
}

// unexplained returns a message for each difference no entry of allow
// explains, and adds each entry that explained one to used.
func unexplained(diffs, allow map[string]string, used map[string]bool) []string {
	var out []string
	for k, v := range diffs {
		e, ok := explain(k, allow)
		if !ok {
			out = append(out, fmt.Sprintf("unlisted difference %q: %s", k, v))
			continue
		}
		used[e] = true
	}
	sort.Strings(out)
	return out
}

// stale returns a message for each entry of allow not in used.
func stale(allow map[string]string, used map[string]bool) []string {
	var out []string
	for e := range allow {
		if !used[e] {
			out = append(out, fmt.Sprintf("allowlist entry %q explained no difference; remove it", e))
		}
	}
	sort.Strings(out)
	return out
}

// explain returns the allowlist entry that explains the difference key,
// "<step> <path>". An entry is "<step> <path>" or "* <path>" (any step),
// and its path covers the path itself and everything under it: "body.acl"
// covers "body.acl[0].role" but not "body.aclx".
func explain(key string, allow map[string]string) (string, bool) {
	i := strings.LastIndex(key, " ")
	step, path := key[:i], key[i+1:]
	for p := path; ; {
		for _, s := range []string{step, "*"} {
			if _, ok := allow[s+" "+p]; ok {
				return s + " " + p, true
			}
		}
		j := strings.LastIndexAny(p, ".[")
		if j <= 0 {
			return "", false
		}
		p = p[:j]
	}
}

func jsonBody(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// field reads a top-level string field of a JSON body.
func field(t *testing.T, body []byte, k string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode %s: %v in %s", k, err, body)
	}
	s, _ := m[k].(string)
	return s
}

func createBucket(o *oracle, name string) {
	o.setup(req{method: "POST", url: "/storage/v1/b?project=" + project, body: jsonBody(map[string]any{"name": name})})
}

// payload returns n deterministic bytes.
func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}
