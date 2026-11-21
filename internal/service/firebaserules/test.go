package firebaserules

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	rules "google.golang.org/api/firebaserules/v1"
)

// clientHeader marks a call as a Firebase client SDK's, which the
// Firestore front passes to the emulator to be evaluated against rules
// (a call with no credentials and no such mark is a server library's).
const clientHeader = "gl-rest/ fire/cloudburrow"

// Test evaluates suite against src in project, by the emulator: Google's
// projects.test. With no test cases it only compiles src.
func (s *Server) Test(ctx context.Context, project string, src *Source, suite *rules.TestSuite) (*rules.TestRulesetResponse, error) {
	if err := checkSource(src); err != nil {
		return nil, err
	}
	var cases []*rules.TestCase
	if suite != nil {
		cases = suite.TestCases
	}
	for i, c := range cases {
		switch c.Expectation {
		case "ALLOW", "DENY":
		default:
			return nil, invalid("testSuite.testCases[%d].expectation must be ALLOW or DENY.", i)
		}
		if err := checkMocks(i, c.FunctionMocks); err != nil {
			return nil, err
		}
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	scratch := "cloudburrow-rules-test-" + hex.EncodeToString(b)
	issues, err := s.compileOn(ctx, scratch, src)
	if err != nil {
		return nil, errorf(http.StatusServiceUnavailable, "UNAVAILABLE", "The Firestore emulator did not compile the ruleset: %v", err)
	}
	out := &rules.TestRulesetResponse{Issues: issues}
	if len(issues) > 0 || len(cases) == 0 {
		return out, nil
	}
	defer clearProject(context.WithoutCancel(ctx), s.emu, scratch)
	text := sourceText(src)
	inForce := text
	for _, c := range cases {
		// The case's source: its service function calls answered by its
		// function mocks (mocks.go).
		mocked, err := mockedSource(text, c.FunctionMocks)
		if err != nil {
			return nil, err
		}
		if mocked != inForce {
			if err := setRules(ctx, s.emu, scratch, mocked); err != nil {
				out.TestResults = append(out.TestResults, failed("The function mocks could not be applied: "+err.Error()))
				inForce = ""
				continue
			}
			inForce = mocked
		}
		out.TestResults = append(out.TestResults, s.runCase(ctx, scratch, c))
	}
	return out, nil
}

// testRequest is a test case's request, as Firestore's rules see it.
type testRequest struct {
	Method string         `json:"method"`
	Path   string         `json:"path"`
	Auth   map[string]any `json:"auth"`
	// Resource is the request's resource: its data is what a create or an
	// update writes.
	Resource *struct {
		Data map[string]any `json:"data"`
	} `json:"resource"`
}

func failed(msg string) *rules.TestResult {
	return &rules.TestResult{State: "FAILURE", DebugMessages: []string{msg}}
}

// runCase evaluates one case on scratch, whose documents it clears first.
func (s *Server) runCase(ctx context.Context, scratch string, c *rules.TestCase) *rules.TestResult {
	raw, _ := json.Marshal(c.Request)
	var req testRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return failed("The request is not a Firestore request: " + err.Error())
	}
	doc, db, err := documentPath(req.Path)
	if err != nil {
		return failed(err.Error())
	}
	clearProject(ctx, s.emu, scratch)
	base := "/v1/projects/" + scratch + "/databases/" + url.PathEscape(db) + "/documents"
	if c.Resource != nil {
		rb, _ := json.Marshal(c.Resource)
		var res struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rb, &res)
		if docPath(doc) {
			body, _ := json.Marshal(map[string]any{"fields": fields(res.Data)})
			code, b, err := s.emu.Do(ctx, http.MethodPatch, base+"/"+doc, ownerHeader(), body)
			if err != nil || code/100 != 2 {
				return failed(fmt.Sprintf("The resource could not be written: %v %s", err, googleError(b)))
			}
		}
	}
	h := http.Header{"X-Goog-Api-Client": {clientHeader}}
	if req.Auth != nil {
		h.Set("Authorization", "Bearer "+idToken(scratch, req.Auth, s.Now()))
	}
	var data map[string]any
	if req.Resource != nil {
		data = req.Resource.Data
	}
	body, _ := json.Marshal(map[string]any{"fields": fields(data)})
	var method, path string
	switch strings.ToLower(req.Method) {
	case "get":
		method, path = http.MethodGet, base+"/"+doc
		body = nil
	case "list":
		coll := doc
		if docPath(doc) {
			coll = doc[:strings.LastIndex(doc, "/")]
		}
		method, path = http.MethodGet, base+"/"+coll
		body = nil
	case "create":
		method, path = http.MethodPatch, base+"/"+doc+"?currentDocument.exists=false"
	case "update":
		method, path = http.MethodPatch, base+"/"+doc+"?currentDocument.exists=true"
	case "delete":
		method, path = http.MethodDelete, base+"/"+doc
		body = nil
	default:
		return failed(fmt.Sprintf("Unsupported request method %q: Firestore's are get, list, create, update and delete.", req.Method))
	}
	if (method == http.MethodPatch || method == http.MethodDelete || req.Method == "get") && !docPath(doc) {
		return failed(fmt.Sprintf("%s names a collection, not a document.", req.Path))
	}
	code, b, err := s.emu.Do(ctx, method, path, h, body)
	if err != nil {
		return failed("The Firestore emulator did not answer: " + err.Error())
	}
	got := "ALLOW"
	var msgs []string
	switch {
	case code == http.StatusForbidden:
		got = "DENY"
		if m := strings.TrimSpace(googleError(b)); m != "" {
			msgs = append(msgs, m)
		}
	case code/100 == 2, code == http.StatusNotFound, code == http.StatusConflict,
		code == http.StatusBadRequest && strings.Contains(string(b), "FAILED_PRECONDITION"):
		// The rules allowed the call; the document's state answered it.
	default:
		return failed(fmt.Sprintf("The Firestore emulator answered %d: %s", code, googleError(b)))
	}
	r := &rules.TestResult{State: "SUCCESS", DebugMessages: msgs}
	if got != c.Expectation {
		r.State = "FAILURE"
		r.DebugMessages = append([]string{fmt.Sprintf("Expected %s, but the request was %s.", c.Expectation, map[string]string{"ALLOW": "allowed", "DENY": "denied"}[got])}, msgs...)
	}
	return r
}

// documentPath reads a request path, /databases/(default)/documents/a/b,
// into the document's path under documents/ and the database.
func documentPath(p string) (string, string, error) {
	p = strings.Trim(p, "/")
	if strings.HasPrefix(p, "projects/") {
		parts := strings.SplitN(p, "/", 3)
		if len(parts) < 3 {
			return "", "", fmt.Errorf("invalid request path %q", p)
		}
		p = parts[2]
	}
	parts := strings.SplitN(p, "/", 4)
	if len(parts) < 4 || parts[0] != "databases" || parts[2] != "documents" || parts[3] == "" {
		return "", "", fmt.Errorf("the request path %q is not /databases/{database}/documents/{path}", p)
	}
	return parts[3], parts[1], nil
}

// docPath reports whether a path under documents/ names a document (an
// even number of segments).
func docPath(p string) bool { return len(strings.Split(p, "/"))%2 == 0 }

// idToken is an unsigned Firebase ID token for auth ({uid, token}), which
// the emulator accepts as Firebase Authentication's.
func idToken(project string, auth map[string]any, now time.Time) string {
	claims := map[string]any{}
	if tok, ok := auth["token"].(map[string]any); ok {
		for k, v := range tok {
			claims[k] = v
		}
	}
	uid, _ := auth["uid"].(string)
	if uid == "" {
		uid, _ = claims["sub"].(string)
	}
	claims["iss"] = "https://securetoken.google.com/" + project
	claims["aud"] = project
	claims["sub"] = uid
	claims["user_id"] = uid
	claims["iat"] = now.Unix()
	claims["auth_time"] = now.Unix()
	claims["exp"] = now.Add(time.Hour).Unix()
	fb, _ := claims["firebase"].(map[string]any)
	if fb == nil {
		fb = map[string]any{}
	}
	if _, ok := fb["sign_in_provider"]; !ok {
		fb["sign_in_provider"] = "custom"
	}
	if _, ok := fb["identities"]; !ok {
		fb["identities"] = map[string]any{}
	}
	claims["firebase"] = fb
	head, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(head) + "." + enc.EncodeToString(body) + "."
}

// fields is data as Firestore REST fields.
func fields(data map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range data {
		out[k] = value(v)
	}
	return out
}

func value(v any) map[string]any {
	switch x := v.(type) {
	case nil:
		return map[string]any{"nullValue": nil}
	case bool:
		return map[string]any{"booleanValue": x}
	case string:
		return map[string]any{"stringValue": x}
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return map[string]any{"integerValue": strconv.FormatInt(int64(x), 10)}
		}
		return map[string]any{"doubleValue": x}
	case []any:
		vals := []any{}
		for _, e := range x {
			vals = append(vals, value(e))
		}
		return map[string]any{"arrayValue": map[string]any{"values": vals}}
	case map[string]any:
		return map[string]any{"mapValue": map[string]any{"fields": fields(x)}}
	}
	return map[string]any{"stringValue": fmt.Sprint(v)}
}
