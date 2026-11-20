package firebaserules

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	rules "google.golang.org/api/firebaserules/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// fakeEmulator compiles any source without "BAD" and, for a call that is
// not the owner's, denies when the project's rules say "if false" and the
// caller's uid is not "alice".
type fakeEmulator struct {
	mu    sync.Mutex
	rules map[string]string
	calls []string
}

func (f *fakeEmulator) Do(_ context.Context, method, path string, h http.Header, body []byte) (int, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method+" "+path)
	if strings.HasPrefix(path, "/emulator/v1/projects/") && strings.HasSuffix(path, ":securityRules") {
		p := strings.TrimSuffix(strings.TrimPrefix(path, "/emulator/v1/projects/"), ":securityRules")
		var b struct {
			Rules struct {
				Files []struct{ Content string } `json:"files"`
			} `json:"rules"`
		}
		_ = json.Unmarshal(body, &b)
		src := b.Rules.Files[0].Content
		if strings.Contains(src, "BAD") {
			return 400, []byte(`{"error":{"code":400,"message":"Error compiling rules:\nL1:33 Unexpected 'x'.","status":"INVALID_ARGUMENT"}}`), nil
		}
		f.rules[p] = src
		return 200, []byte("{}"), nil
	}
	if strings.HasPrefix(path, "/emulator/") {
		return 200, []byte("{}"), nil
	}
	if h.Get("Authorization") == "Bearer owner" {
		return 200, []byte("{}"), nil
	}
	p := strings.TrimPrefix(path, "/v1/projects/")
	p, _, _ = strings.Cut(p, "/")
	if strings.Contains(f.rules[p], "if false") {
		uid := ""
		if tok, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer "); ok {
			parts := strings.Split(tok, ".")
			b, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var c map[string]any
			_ = json.Unmarshal(b, &c)
			uid, _ = c["sub"].(string)
		}
		if uid != "alice" {
			return 403, []byte(`{"error":{"code":403,"message":"\nfalse for 'get' @ L4","status":"PERMISSION_DENIED"}}`), nil
		}
	}
	return 404, []byte(`{"error":{"code":404,"status":"NOT_FOUND"}}`), nil
}

const locked = `rules_version = '2';
service cloud.firestore {
  match /databases/{database}/documents {
    match /{document=**} { allow read, write: if false; }
  }
}`

func newTestClient(t *testing.T) (*rules.Service, *fakeEmulator, *Server) {
	t.Helper()
	emu := &fakeEmulator{rules: map[string]string{}}
	s := NewServer("127.0.0.1:0", emu, t.TempDir())
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c, err := rules.NewService(context.Background(), option.WithEndpoint(srv.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return c, emu, s
}

func TestRulesetsReleasesAndTest(t *testing.T) {
	ctx := context.Background()
	c, emu, s := newTestClient(t)
	src := &rules.Source{Files: []*rules.File{{Name: "firestore.rules", Content: locked}}}
	rs, err := c.Projects.Rulesets.Create("projects/p", &rules.Ruleset{Source: src}).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rs.Name, "projects/p/rulesets/") || rs.CreateTime == "" || rs.Metadata == nil || rs.Metadata.Services[0] != "cloud.firestore" {
		t.Fatalf("ruleset = %+v", rs)
	}
	_, err = c.Projects.Rulesets.Create("projects/p", &rules.Ruleset{Source: &rules.Source{Files: []*rules.File{{Name: "f", Content: "BAD"}}}}).Do()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != 400 {
		t.Fatalf("bad source: %v", err)
	}
	if _, ok := emu.rules["p"]; ok {
		t.Fatal("a ruleset alone changed the project's rules")
	}
	rel, err := c.Projects.Releases.Create("projects/p", &rules.Release{Name: "projects/p/releases/cloud.firestore", RulesetName: rs.Name}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if emu.rules["p"] != locked || rel.UpdateTime == "" {
		t.Fatalf("release did not set the rules: %q", emu.rules["p"])
	}
	if _, err := c.Projects.Releases.Create("projects/p", &rules.Release{Name: rel.Name, RulesetName: rs.Name}).Do(); !errors.As(err, &ge) || ge.Code != 409 {
		t.Fatalf("duplicate release: %v", err)
	}
	list, err := c.Projects.Releases.List("projects/p").Filter("name=projects/p/releases/cloud.*").Do()
	if err != nil || len(list.Releases) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	rl, err := c.Projects.Rulesets.List("projects/p").Do()
	if err != nil || len(rl.Rulesets) != 1 || rl.Rulesets[0].Source != nil {
		t.Fatalf("rulesets list = %+v, %v", rl, err)
	}
	if _, err := c.Projects.Releases.GetExecutable(rel.Name).Do(); err != nil {
		t.Fatal(err)
	}
	if err := func() error { _, err := c.Projects.Rulesets.Delete(rs.Name).Do(); return err }(); !errors.As(err, &ge) || ge.Code != 400 {
		t.Fatalf("delete of a released ruleset: %v", err)
	}

	res, err := c.Projects.Test("projects/p", &rules.TestRulesetRequest{Source: src, TestSuite: &rules.TestSuite{TestCases: []*rules.TestCase{
		{Expectation: "DENY", Request: map[string]any{"method": "get", "path": "/databases/(default)/documents/users/bob"}},
		{Expectation: "ALLOW", Request: map[string]any{"method": "get", "path": "/databases/(default)/documents/users/alice", "auth": map[string]any{"uid": "alice"}}},
		{Expectation: "ALLOW", Request: map[string]any{"method": "get", "path": "/databases/(default)/documents/users/bob", "auth": map[string]any{"uid": "bob"}}},
	}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, r := range res.TestResults {
		got = append(got, r.State)
	}
	if strings.Join(got, ",") != "SUCCESS,SUCCESS,FAILURE" {
		t.Fatalf("results = %v (%+v)", got, res.TestResults[2])
	}
	bad, err := c.Projects.Test("projects/p", &rules.TestRulesetRequest{Source: &rules.Source{Files: []*rules.File{{Name: "f.rules", Content: "BAD"}}}}).Do()
	if err != nil || len(bad.Issues) != 1 || bad.Issues[0].SourcePosition.Line != 1 || bad.Issues[0].SourcePosition.Column != 33 {
		t.Fatalf("issues = %+v, %v", bad, err)
	}

	if _, err := c.Projects.Releases.Delete(rel.Name).Do(); err != nil {
		t.Fatal(err)
	}
	if emu.rules["p"] != OpenRules {
		t.Fatalf("deleting the release left %q", emu.rules["p"])
	}
	if _, err := c.Projects.Rulesets.Delete(rs.Name).Do(); err != nil {
		t.Fatal(err)
	}
	// The state file brings releases back.
	if _, err := s.CreateRuleset(ctx, "q", &Source{Files: []*File{{Name: "firestore.rules", Content: locked}}}); err != nil {
		t.Fatal(err)
	}
	s2 := NewServer("127.0.0.1:0", emu, "")
	s2.state = s.state
	if err := s2.load(); err != nil || len(s2.Rulesets("q")) != 1 {
		t.Fatalf("reload: %v %d", err, len(s2.Rulesets("q")))
	}
}

func TestIDTokenAndValues(t *testing.T) {
	tok := idToken("x", map[string]any{"uid": "u1", "token": map[string]any{"email": "a@b"}}, time.Unix(0, 0))
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[2] != "" {
		t.Fatalf("token = %q", tok)
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c map[string]any
	_ = json.Unmarshal(b, &c)
	if c["sub"] != "u1" || c["email"] != "a@b" || c["iss"] != "https://securetoken.google.com/x" || c["firebase"] == nil {
		t.Fatalf("claims = %v", c)
	}
	v, _ := json.Marshal(fields(map[string]any{"n": 3.0, "d": 1.5, "s": "x", "m": map[string]any{"b": true}, "a": []any{nil}}))
	want := `{"a":{"arrayValue":{"values":[{"nullValue":null}]}},"d":{"doubleValue":1.5},"m":{"mapValue":{"fields":{"b":{"booleanValue":true}}}},"n":{"integerValue":"3"},"s":{"stringValue":"x"}}`
	if string(v) != want {
		t.Fatalf("fields = %s", v)
	}
	if d, db, err := documentPath("/databases/(default)/documents/a/b"); err != nil || d != "a/b" || db != "(default)" {
		t.Fatalf("path = %q %q %v", d, db, err)
	}
}

// TestRulesetReadsBackAsSent: Terraform's provider plans clean only when a
// ruleset reads back as it sent it: no fingerprint it did not give, and
// the source's language kept.
func TestRulesetReadsBackAsSent(t *testing.T) {
	emu := &fakeEmulator{rules: map[string]string{}}
	srv := httptest.NewServer(NewServer("127.0.0.1:0", emu, ""))
	defer srv.Close()
	body := `{"source":{"files":[{"name":"firestore.rules","content":"service cloud.firestore {}"}],"language":"FIREBASE_RULES"}}`
	resp, err := http.Post(srv.URL+"/v1/projects/p/rulesets", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	resp, err = http.Get(srv.URL + "/v1/" + created["name"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Source struct {
			Files    []map[string]any `json:"files"`
			Language string           `json:"language"`
		} `json:"source"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Source.Language != "FIREBASE_RULES" || len(got.Source.Files) != 1 {
		t.Fatalf("source = %+v", got.Source)
	}
	if _, ok := got.Source.Files[0]["fingerprint"]; ok {
		t.Fatalf("a fingerprint the caller did not give: %v", got.Source.Files[0])
	}
}
