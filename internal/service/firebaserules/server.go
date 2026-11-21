// Package firebaserules serves the Firebase Rules API v1
// (firebaserules.googleapis.com, #2048) over the instance's Firestore
// emulator, which evaluates Firestore Security Rules itself: rulesets,
// releases and projects.test, as REST/JSON, which the Go, Node and Python
// client libraries, the Firebase CLI and Terraform's
// google_firebaserules_ruleset and google_firebaserules_release call.
//
// A ruleset is compiled by the emulator: its source is set on a scratch
// project (checkProject), and the emulator's compile errors are the
// ruleset's issues. A release named cloud.firestore (or
// cloud.firestore/DATABASE) sets its ruleset as the project's rules in the
// emulator, which then evaluates every client-SDK call (a Firebase ID
// token, or no credentials) against it; server client libraries bypass
// rules, as they do on Google (the Firestore front sends their calls as the
// emulator's owner). The emulator keeps one set of rules per project, so
// the latest cloud.firestore release of a project is the one in force for
// all its databases.
//
// Until a project has a cloud.firestore release its Firestore stays open,
// as CloudBurrow's Firestore always was (on Google a database created
// without rules is in locked or test mode, which the console chooses);
// deleting the last release puts the open rules back, rather than
// Google's deny-all, so existing clients keep working.
//
// projects.test evaluates each test case by the emulator too, on a fresh
// scratch project: the ruleset is set there, the case's resource is
// written as the owner, and the case's request is made with an unsigned
// Firebase ID token for request.auth (or no credentials for a null auth).
// A 403 is a DENY, any other answer an ALLOW. Function mocks are not
// evaluated locally: such a case fails, saying so.
package firebaserules

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	rules "google.golang.org/api/firebaserules/v1"

	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/sysparams"
)

// FirestoreService is the service a Firestore ruleset declares, and the
// release name prefix that puts a ruleset in force.
const FirestoreService = "cloud.firestore"

// File is one source file of a ruleset. Its fingerprint is the caller's,
// answered only when given, as Google's is (Terraform reads it back).
type File struct {
	Name        string `json:"name,omitempty"`
	Content     string `json:"content,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Source is a ruleset's source: its files and, when given, its language
// (FIREBASE_RULES), which the generated Go client has no field for.
type Source struct {
	Files    []*File `json:"files,omitempty"`
	Language string  `json:"language,omitempty"`
}

// Ruleset is a ruleset as the REST API answers it.
type Ruleset struct {
	Name            string          `json:"name,omitempty"`
	CreateTime      string          `json:"createTime,omitempty"`
	Metadata        *rules.Metadata `json:"metadata,omitempty"`
	Source          *Source         `json:"source,omitempty"`
	AttachmentPoint string          `json:"attachmentPoint,omitempty"`
}

// testRulesetRequest is projects.test's body.
type testRulesetRequest struct {
	Source    *Source          `json:"source,omitempty"`
	TestSuite *rules.TestSuite `json:"testSuite,omitempty"`
}

// stored is the server's state, as its state file keeps it.
type stored struct {
	Rulesets map[string]*Ruleset       `json:"rulesets"`
	Releases map[string]*rules.Release `json:"releases"`
}

// Server is the Firebase Rules API.
type Server struct {
	addr  string
	emu   Emulator
	state string // the state file, "" for memory only
	// Now is the clock.
	Now func() time.Time
	// Observe, if set, is called once per request with the method name and
	// the answer's HTTP status.
	Observe func(method string, code int)

	mu       sync.Mutex
	rulesets map[string]*Ruleset       // by name
	releases map[string]*rules.Release // by name
	// check serializes compiles on the scratch project.
	check sync.Mutex

	srv *http.Server
	ln  net.Listener
}

// NewServer serves at addr once started, over emu. stateDir, when not
// empty, is the directory its rulesets and releases are kept in.
func NewServer(addr string, emu Emulator, stateDir string) *Server {
	s := &Server{addr: addr, emu: emu, Now: time.Now,
		rulesets: map[string]*Ruleset{}, releases: map[string]*rules.Release{}}
	if stateDir != "" {
		s.state = filepath.Join(stateDir, "firebaserules.json")
	}
	return s
}

func (s *Server) Name() string { return "firebaserules" }

// Start loads the state, binds the listener and serves in the background,
// then puts each project's release back in the emulator, which keeps no
// rules across a restart, as soon as it answers.
func (s *Server) Start(ctx context.Context) error {
	if err := s.load(); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("firebaserules: listen on %s: %w", s.addr, err)
	}
	s.ln = ln
	s.srv = &http.Server{Handler: hostguard.Wrap(sysparams.Wrap(s, sysparams.Options{})), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	go s.reapply(context.WithoutCancel(ctx))
	return nil
}

// reapply sets each project's release in the emulator, retrying for a few
// minutes while the emulator starts.
func (s *Server) reapply(ctx context.Context) {
	s.mu.Lock()
	projects := map[string]bool{}
	for name := range s.releases {
		projects[projectOf(name)] = true
	}
	s.mu.Unlock()
	for p := range projects {
		for i := 0; i < 60; i++ {
			if err := s.apply(ctx, p); err == nil {
				break
			}
			time.Sleep(5 * time.Second)
		}
	}
}

// Stop shuts the server down.
func (s *Server) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// Addr is the bound address, empty before Start.
func (s *Server) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *Server) load() error {
	if s.state == "" {
		return nil
	}
	b, err := os.ReadFile(s.state)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("firebaserules: %w", err)
	}
	var st stored
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("firebaserules: %s: %w", s.state, err)
	}
	if st.Rulesets != nil {
		s.rulesets = st.Rulesets
	}
	if st.Releases != nil {
		s.releases = st.Releases
	}
	return nil
}

// saveLocked writes the state file; s.mu is held.
func (s *Server) saveLocked() error {
	if s.state == "" {
		return nil
	}
	b, err := json.Marshal(stored{Rulesets: s.rulesets, Releases: s.releases})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.state), 0o700); err != nil {
		return err
	}
	tmp := s.state + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.state)
}

// ---- errors ----

// apiError is an error with the HTTP status and Google status it answers.
type apiError struct {
	code   int
	status string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func errorf(code int, status, format string, args ...any) error {
	return &apiError{code: code, status: status, msg: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) error {
	return errorf(http.StatusBadRequest, "INVALID_ARGUMENT", format, args...)
}

// ErrorStatus is err's HTTP status and Google status, for the console.
func ErrorStatus(err error) (int, string) {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.code, ae.status
	}
	return http.StatusInternalServerError, "INTERNAL"
}

// ---- routing ----

var (
	rulesetsRe   = regexp.MustCompile(`^/v1/projects/([^/:]+)/rulesets$`)
	rulesetRe    = regexp.MustCompile(`^/v1/(projects/([^/:]+)/rulesets/([^/:]+))$`)
	releasesRe   = regexp.MustCompile(`^/v1/projects/([^/:]+)/releases$`)
	releaseRe    = regexp.MustCompile(`^/v1/(projects/([^/:]+)/releases/(.+?))(:getExecutable)?$`)
	testRe       = regexp.MustCompile(`^/v1/projects/([^/:]+):test$`)
	rulesetTest  = regexp.MustCompile(`^/v1/(projects/([^/:]+)/rulesets/([^/:]+)):test$`)
	issueLineRe  = regexp.MustCompile(`^L(\d+):(\d+)\s+(.*)$`)
	releaseIDRe  = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	servicesDecl = regexp.MustCompile(`(?m)^\s*service\s+([A-Za-z0-9_.]+)`)
)

// ServeHTTP routes /v1/....
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method, out, err := s.route(r)
	if err != nil {
		var ae *apiError
		if !errors.As(err, &ae) {
			ae = &apiError{code: http.StatusInternalServerError, status: "INTERNAL", msg: err.Error()}
		}
		if s.Observe != nil {
			s.Observe(method, ae.code)
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(ae.code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": ae.code, "message": ae.msg, "status": ae.status,
		}})
		return
	}
	if s.Observe != nil {
		s.Observe(method, http.StatusOK)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(out)
}

func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20))
	if err := dec.Decode(v); err != nil && err.Error() != "EOF" {
		return invalid("Invalid JSON payload received. %v", err)
	}
	return nil
}

func (s *Server) route(r *http.Request) (string, any, error) {
	ctx := r.Context()
	p := r.URL.Path
	q := r.URL.Query()
	switch {
	case testRe.MatchString(p) && r.Method == http.MethodPost:
		m := testRe.FindStringSubmatch(p)
		var req testRulesetRequest
		if err := decode(r, &req); err != nil {
			return "projects.test", nil, err
		}
		out, err := s.Test(ctx, m[1], req.Source, req.TestSuite)
		return "projects.test", out, err
	case rulesetTest.MatchString(p) && r.Method == http.MethodPost:
		m := rulesetTest.FindStringSubmatch(p)
		var req testRulesetRequest
		if err := decode(r, &req); err != nil {
			return "projects.test", nil, err
		}
		rs, err := s.GetRuleset(m[1])
		if err != nil {
			return "projects.test", nil, err
		}
		out, err := s.Test(ctx, m[2], rs.Source, req.TestSuite)
		return "projects.test", out, err
	case rulesetsRe.MatchString(p):
		m := rulesetsRe.FindStringSubmatch(p)
		switch r.Method {
		case http.MethodPost:
			var rs Ruleset
			if err := decode(r, &rs); err != nil {
				return "projects.rulesets.create", nil, err
			}
			out, err := s.CreateRuleset(ctx, m[1], rs.Source)
			return "projects.rulesets.create", out, err
		case http.MethodGet:
			out, err := s.listRulesets(m[1], q.Get("pageSize"), q.Get("pageToken"))
			return "projects.rulesets.list", out, err
		}
	case rulesetRe.MatchString(p):
		m := rulesetRe.FindStringSubmatch(p)
		switch r.Method {
		case http.MethodGet:
			out, err := s.GetRuleset(m[1])
			return "projects.rulesets.get", out, err
		case http.MethodDelete:
			err := s.DeleteRuleset(m[1])
			return "projects.rulesets.delete", struct{}{}, err
		}
	case releasesRe.MatchString(p):
		m := releasesRe.FindStringSubmatch(p)
		switch r.Method {
		case http.MethodPost:
			var rel rules.Release
			if err := decode(r, &rel); err != nil {
				return "projects.releases.create", nil, err
			}
			out, err := s.CreateRelease(ctx, m[1], &rel)
			return "projects.releases.create", out, err
		case http.MethodGet:
			out, err := s.listReleases(m[1], q.Get("filter"), q.Get("pageSize"), q.Get("pageToken"))
			return "projects.releases.list", out, err
		}
	case releaseRe.MatchString(p):
		m := releaseRe.FindStringSubmatch(p)
		if m[4] != "" {
			if r.Method != http.MethodGet {
				break
			}
			out, err := s.getExecutable(m[1])
			return "projects.releases.getExecutable", out, err
		}
		switch r.Method {
		case http.MethodGet:
			out, err := s.GetRelease(m[1])
			return "projects.releases.get", out, err
		case http.MethodPatch:
			var req rules.UpdateReleaseRequest
			if err := decode(r, &req); err != nil {
				return "projects.releases.patch", nil, err
			}
			out, err := s.PatchRelease(ctx, m[1], req.Release, req.UpdateMask)
			return "projects.releases.patch", out, err
		case http.MethodDelete:
			err := s.DeleteRelease(ctx, m[1])
			return "projects.releases.delete", struct{}{}, err
		}
	}
	return "", nil, errorf(http.StatusNotFound, "NOT_FOUND", "The requested URL %s was not found on this server.", p)
}

// ---- rulesets ----

func (s *Server) timestamp() string { return s.Now().UTC().Format(time.RFC3339Nano) }

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// cloneSource copies src as given.
func cloneSource(src *Source) *Source {
	out := &Source{Language: src.Language}
	for _, f := range src.Files {
		c := *f
		out.Files = append(out.Files, &c)
	}
	return out
}

// sourceText is the source the emulator compiles: its files, concatenated.
func sourceText(src *Source) string {
	var parts []string
	for _, f := range src.Files {
		parts = append(parts, f.Content)
	}
	return strings.Join(parts, "\n")
}

func checkSource(src *Source) error {
	if src == nil || len(src.Files) == 0 {
		return invalid("Ruleset is missing a source.")
	}
	for _, f := range src.Files {
		if f.Name == "" {
			return invalid("Ruleset source file is missing a name.")
		}
	}
	return nil
}

// Compile compiles src in the emulator, answering its issues.
func (s *Server) Compile(ctx context.Context, src *Source) ([]*rules.Issue, error) {
	s.check.Lock()
	defer s.check.Unlock()
	return s.compileOn(ctx, checkProject, src)
}

func (s *Server) compileOn(ctx context.Context, project string, src *Source) ([]*rules.Issue, error) {
	err := setRules(ctx, s.emu, project, sourceText(src))
	var ce *CompileError
	if errors.As(err, &ce) {
		file := ""
		if len(src.Files) > 0 {
			file = src.Files[0].Name
		}
		return compileIssues(file, ce.Message), nil
	}
	return nil, err
}

// compileIssues reads the emulator's "Error compiling rules:\nL1:33
// Unexpected 'x'." into issues.
func compileIssues(file, msg string) []*rules.Issue {
	var out []*rules.Issue
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimSpace(line)
		if m := issueLineRe.FindStringSubmatch(line); m != nil {
			l, _ := strconv.ParseInt(m[1], 10, 64)
			c, _ := strconv.ParseInt(m[2], 10, 64)
			out = append(out, &rules.Issue{Description: m[3], Severity: "ERROR",
				SourcePosition: &rules.SourcePosition{FileName: file, Line: l, Column: c}})
		}
	}
	if len(out) == 0 {
		out = append(out, &rules.Issue{Description: strings.TrimSpace(msg), Severity: "ERROR",
			SourcePosition: &rules.SourcePosition{FileName: file}})
	}
	return out
}

// CreateRuleset compiles and keeps a ruleset in project.
func (s *Server) CreateRuleset(ctx context.Context, project string, src *Source) (*Ruleset, error) {
	if err := checkSource(src); err != nil {
		return nil, err
	}
	issues, err := s.Compile(ctx, src)
	if err != nil {
		return nil, errorf(http.StatusServiceUnavailable, "UNAVAILABLE", "The Firestore emulator did not compile the ruleset: %v", err)
	}
	if len(issues) > 0 {
		var lines []string
		for _, i := range issues {
			p := i.SourcePosition
			lines = append(lines, fmt.Sprintf("%s:%d:%d: %s", p.FileName, p.Line, p.Column, i.Description))
		}
		return nil, invalid("Syntax error in the ruleset: %s", strings.Join(lines, "; "))
	}
	rs := &Ruleset{Name: "projects/" + project + "/rulesets/" + newID(), CreateTime: s.timestamp(),
		Source: cloneSource(src)}
	for _, m := range servicesDecl.FindAllStringSubmatch(sourceText(src), -1) {
		if rs.Metadata == nil {
			rs.Metadata = &rules.Metadata{}
		}
		rs.Metadata.Services = append(rs.Metadata.Services, m[1])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rulesets[rs.Name] = rs
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return rs, nil
}

// GetRuleset answers a ruleset by name.
func (s *Server) GetRuleset(name string) (*Ruleset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, ok := s.rulesets[name]
	if !ok {
		return nil, errorf(http.StatusNotFound, "NOT_FOUND", "Ruleset %s not found.", name)
	}
	return rs, nil
}

// Rulesets are project's rulesets, newest first.
func (s *Server) Rulesets(project string) []*Ruleset {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Ruleset
	for _, rs := range s.rulesets {
		if projectOf(rs.Name) == project {
			out = append(out, rs)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreateTime != out[j].CreateTime {
			return out[i].CreateTime > out[j].CreateTime
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func page(n int, size, token string) (int, int, error) {
	ps := 10
	if size != "" {
		v, err := strconv.Atoi(size)
		if err != nil || v < 0 {
			return 0, 0, invalid("Invalid page size %q.", size)
		}
		if v > 0 {
			ps = min(v, 100)
		}
	}
	start := 0
	if token != "" {
		b, err := base64.RawURLEncoding.DecodeString(token)
		v, err2 := strconv.Atoi(string(b))
		if err != nil || err2 != nil || v < 0 || v > n {
			return 0, 0, invalid("Invalid page token.")
		}
		start = v
	}
	return start, min(n, start+ps), nil
}

func nextToken(end, n int) string {
	if end >= n {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
}

// listRulesetsResponse is rulesets.list's answer.
type listRulesetsResponse struct {
	Rulesets      []*Ruleset `json:"rulesets,omitempty"`
	NextPageToken string     `json:"nextPageToken,omitempty"`
}

func (s *Server) listRulesets(project, size, token string) (*listRulesetsResponse, error) {
	all := s.Rulesets(project)
	start, end, err := page(len(all), size, token)
	if err != nil {
		return nil, err
	}
	out := &listRulesetsResponse{NextPageToken: nextToken(end, len(all))}
	for _, rs := range all[start:end] {
		// The list omits each ruleset's source, as Google's does.
		out.Rulesets = append(out.Rulesets, &Ruleset{Name: rs.Name, CreateTime: rs.CreateTime, Metadata: rs.Metadata})
	}
	return out, nil
}

// DeleteRuleset deletes a ruleset no release refers to.
func (s *Server) DeleteRuleset(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rulesets[name]; !ok {
		return errorf(http.StatusNotFound, "NOT_FOUND", "Ruleset %s not found.", name)
	}
	for _, rel := range s.releases {
		if rel.RulesetName == name {
			return errorf(http.StatusBadRequest, "FAILED_PRECONDITION",
				"Ruleset %s is referenced by release %s and cannot be deleted.", name, rel.Name)
		}
	}
	delete(s.rulesets, name)
	return s.saveLocked()
}

// ---- releases ----

func projectOf(name string) string {
	rest := strings.TrimPrefix(name, "projects/")
	p, _, _ := strings.Cut(rest, "/")
	return p
}

// releaseID is a release name's ID, after releases/.
func releaseID(name string) string {
	_, id, _ := strings.Cut(name, "/releases/")
	return id
}

// IsFirestore reports whether a release ID puts its ruleset in force for
// Firestore: cloud.firestore, or cloud.firestore/DATABASE.
func IsFirestore(id string) bool {
	return id == FirestoreService || strings.HasPrefix(id, FirestoreService+"/")
}

// CreateRelease creates a release in project.
func (s *Server) CreateRelease(ctx context.Context, project string, rel *rules.Release) (*rules.Release, error) {
	if rel == nil || rel.Name == "" {
		return nil, invalid("Release name is required.")
	}
	if projectOf(rel.Name) != project || !strings.HasPrefix(rel.Name, "projects/"+project+"/releases/") {
		return nil, invalid("Release name %q must be under projects/%s/releases/.", rel.Name, project)
	}
	if id := releaseID(rel.Name); id == "" || !releaseIDRe.MatchString(id) {
		return nil, invalid("Invalid release name %q.", rel.Name)
	}
	if rel.RulesetName == "" {
		return nil, invalid("Release rulesetName is required.")
	}
	s.mu.Lock()
	if _, ok := s.releases[rel.Name]; ok {
		s.mu.Unlock()
		return nil, errorf(http.StatusConflict, "ALREADY_EXISTS", "Release %s already exists.", rel.Name)
	}
	if _, ok := s.rulesets[rel.RulesetName]; !ok || projectOf(rel.RulesetName) != project {
		s.mu.Unlock()
		return nil, errorf(http.StatusNotFound, "NOT_FOUND", "Ruleset %s not found.", rel.RulesetName)
	}
	now := s.timestamp()
	out := &rules.Release{Name: rel.Name, RulesetName: rel.RulesetName, CreateTime: now, UpdateTime: now}
	s.releases[rel.Name] = out
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if IsFirestore(releaseID(rel.Name)) {
		if err := s.apply(ctx, project); err != nil {
			return nil, errorf(http.StatusServiceUnavailable, "UNAVAILABLE", "The release is kept, but the Firestore emulator did not take its rules: %v", err)
		}
	}
	return out, nil
}

// GetRelease answers a release by name.
func (s *Server) GetRelease(name string) (*rules.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rel, ok := s.releases[name]
	if !ok {
		return nil, errorf(http.StatusNotFound, "NOT_FOUND", "Release %s not found.", name)
	}
	return rel, nil
}

// Releases are project's releases, by name.
func (s *Server) Releases(project string) []*rules.Release {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*rules.Release
	for _, rel := range s.releases {
		if projectOf(rel.Name) == project {
			out = append(out, rel)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// releaseFilter reads Google's filter: name, ruleset_name and
// test_suite_name, each = a value, a trailing * matching a prefix,
// combined by AND.
func releaseFilter(filter string) (func(*rules.Release) bool, error) {
	if strings.TrimSpace(filter) == "" {
		return func(*rules.Release) bool { return true }, nil
	}
	type term struct{ field, value string }
	var terms []term
	for _, part := range regexp.MustCompile(`\s+AND\s+`).Split(strings.TrimSpace(filter), -1) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, invalid("Invalid filter %q.", filter)
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch k {
		case "name", "ruleset_name", "rulesetName", "test_suite_name", "testSuiteName":
		default:
			return nil, invalid("Invalid filter field %q.", k)
		}
		terms = append(terms, term{k, v})
	}
	match := func(got, want string) bool {
		if p, ok := strings.CutSuffix(want, "*"); ok {
			return strings.HasPrefix(got, p)
		}
		return got == want
	}
	return func(rel *rules.Release) bool {
		for _, t := range terms {
			switch t.field {
			case "name":
				// A bare ID matches as a relative name.
				if !match(rel.Name, t.value) && !match(releaseID(rel.Name), t.value) {
					return false
				}
			case "ruleset_name", "rulesetName":
				if !match(rel.RulesetName, t.value) && !match(strings.TrimPrefix(rel.RulesetName, "projects/"+projectOf(rel.Name)+"/rulesets/"), t.value) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}, nil
}

func (s *Server) listReleases(project, filter, size, token string) (*rules.ListReleasesResponse, error) {
	keep, err := releaseFilter(filter)
	if err != nil {
		return nil, err
	}
	var all []*rules.Release
	for _, rel := range s.Releases(project) {
		if keep(rel) {
			all = append(all, rel)
		}
	}
	start, end, err := page(len(all), size, token)
	if err != nil {
		return nil, err
	}
	return &rules.ListReleasesResponse{Releases: all[start:end], NextPageToken: nextToken(end, len(all))}, nil
}

// PatchRelease points a release at another ruleset: only rulesetName is
// honored, as Google documents.
func (s *Server) PatchRelease(ctx context.Context, name string, rel *rules.Release, mask string) (*rules.Release, error) {
	if rel == nil {
		return nil, invalid("Release is required.")
	}
	if rel.Name != "" && rel.Name != name {
		return nil, invalid("Release name %q does not match %q.", rel.Name, name)
	}
	if mask != "" {
		for _, f := range strings.Split(mask, ",") {
			switch strings.TrimSpace(f) {
			case "ruleset_name", "rulesetName", "name":
			default:
				return nil, invalid("Invalid update mask field %q.", f)
			}
		}
	}
	project := projectOf(name)
	s.mu.Lock()
	cur, ok := s.releases[name]
	if !ok {
		s.mu.Unlock()
		return nil, errorf(http.StatusNotFound, "NOT_FOUND", "Release %s not found.", name)
	}
	if rel.RulesetName != "" {
		if _, ok := s.rulesets[rel.RulesetName]; !ok || projectOf(rel.RulesetName) != project {
			s.mu.Unlock()
			return nil, errorf(http.StatusNotFound, "NOT_FOUND", "Ruleset %s not found.", rel.RulesetName)
		}
		next := *cur
		next.RulesetName = rel.RulesetName
		next.UpdateTime = s.timestamp()
		cur = &next
		s.releases[name] = cur
	}
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if IsFirestore(releaseID(name)) {
		if err := s.apply(ctx, project); err != nil {
			return nil, errorf(http.StatusServiceUnavailable, "UNAVAILABLE", "The release is kept, but the Firestore emulator did not take its rules: %v", err)
		}
	}
	return cur, nil
}

// DeleteRelease deletes a release; the project's Firestore takes its
// remaining cloud.firestore release's rules, or the open rules.
func (s *Server) DeleteRelease(ctx context.Context, name string) error {
	s.mu.Lock()
	if _, ok := s.releases[name]; !ok {
		s.mu.Unlock()
		return errorf(http.StatusNotFound, "NOT_FOUND", "Release %s not found.", name)
	}
	delete(s.releases, name)
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if IsFirestore(releaseID(name)) {
		return s.apply(ctx, projectOf(name))
	}
	return nil
}

func (s *Server) getExecutable(name string) (*rules.GetReleaseExecutableResponse, error) {
	rel, err := s.GetRelease(name)
	if err != nil {
		return nil, err
	}
	rs, err := s.GetRuleset(rel.RulesetName)
	if err != nil {
		return nil, err
	}
	// The emulator compiles rules in its own process and exposes no
	// executable; the executable answered is the ruleset's source.
	return &rules.GetReleaseExecutableResponse{
		Executable:        base64.StdEncoding.EncodeToString([]byte(sourceText(rs.Source))),
		ExecutableVersion: "FIREBASE_RULES_EXECUTABLE_V2",
		Language:          "FIREBASE_RULES",
		RulesetName:       rs.Name,
		UpdateTime:        rel.UpdateTime,
		SyncTime:          s.timestamp(),
	}, nil
}

// InForce is the release whose rules the emulator evaluates for project:
// its latest-updated cloud.firestore release, nil for none.
func (s *Server) InForce(project string) *rules.Release {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inForceLocked(project)
}

func (s *Server) inForceLocked(project string) *rules.Release {
	var best *rules.Release
	for _, rel := range s.releases {
		if projectOf(rel.Name) != project || !IsFirestore(releaseID(rel.Name)) {
			continue
		}
		if best == nil || rel.UpdateTime > best.UpdateTime || rel.UpdateTime == best.UpdateTime && rel.Name < best.Name {
			best = rel
		}
	}
	return best
}

// apply sets project's rules in the emulator: its release in force, or
// the open rules.
func (s *Server) apply(ctx context.Context, project string) error {
	s.mu.Lock()
	src := OpenRules
	if rel := s.inForceLocked(project); rel != nil {
		if rs, ok := s.rulesets[rel.RulesetName]; ok {
			src = sourceText(rs.Source)
		}
	}
	s.mu.Unlock()
	return setRules(ctx, s.emu, project, src)
}

// ResetProject forgets one project's rulesets and releases, or every
// project's for "", and opens their Firestore again.
func (s *Server) ResetProject(ctx context.Context, project string) error {
	s.mu.Lock()
	touched := map[string]bool{}
	for name := range s.releases {
		if p := projectOf(name); project == "" || p == project {
			touched[p] = true
			delete(s.releases, name)
		}
	}
	for name := range s.rulesets {
		if project == "" || projectOf(name) == project {
			delete(s.rulesets, name)
		}
	}
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	var errs []error
	for p := range touched {
		errs = append(errs, s.apply(ctx, p))
	}
	return errors.Join(errs...)
}
