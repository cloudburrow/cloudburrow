//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// consoleFaultsDo is one request to the console's fault endpoints, with the
// admin token when token is true, as the fault screen sends it (#800).
func consoleFaultsDo(t *testing.T, addr, method, path, body string, token bool) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token {
		req.Header.Set("Authorization", "Bearer "+adminToken(t))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestConsoleFaultRuleFailsTheSDKCall (#800): a rule added through the
// console's fault endpoints is held by /admin/faults and makes the official
// Secret Manager client's next matching call fail with the rule's code; the
// console lists the fault it injected; deleting the rule from the console
// removes it from /admin/faults and the call gets the ordinary answer again.
// Without the admin token, the console's endpoints refuse the same requests
// with 401 and add nothing (#553). The rule is scoped to a project no other
// test uses, so it cannot fault a concurrent test's call.
func TestConsoleFaultRuleFailsTheSDKCall(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	control := h.Endpoint(EnvControl)
	// First, so a shard that does not serve Secret Manager skips before a
	// rule for it is refused.
	sc := secretsClient(t, h)
	if adminToken(t) == "" {
		t.Skipf("%s is not set and no instance directory is known; the console's fault endpoints need the token", EnvAdminToken)
	}
	const project = "console-faulted"
	rule := `{"service":"secretmanager","method":"GetSecret","project":"` + project + `","code":"PERMISSION_DENIED"}`
	listed := func() string {
		t.Helper()
		_, body := adminDo(t, http.MethodGet, "http://"+control+"/admin/faults", "", nil)
		return body
	}

	if code, body := consoleFaultsDo(t, addr, http.MethodPost, "/api/faults", rule, false); code != http.StatusUnauthorized {
		t.Fatalf("POST /api/faults without the admin token = %d %s; want 401", code, body)
	}
	if body := listed(); strings.Contains(body, `"project":"`+project+`"`) {
		t.Fatalf("a refused console request added a rule: %s", body)
	}

	code, body := consoleFaultsDo(t, addr, http.MethodPost, "/api/faults", rule, true)
	var created struct{ ID string }
	if err := json.Unmarshal([]byte(body), &created); err != nil || code != http.StatusCreated || created.ID == "" {
		t.Fatalf("POST /api/faults with the admin token = %d %s", code, body)
	}
	deleted := false
	t.Cleanup(func() {
		if !deleted {
			adminDo(t, http.MethodDelete, "http://"+control+"/admin/faults?id="+url.QueryEscape(created.ID), "", nil)
		}
	})
	if body := listed(); !strings.Contains(body, `"id":"`+created.ID+`"`) {
		t.Fatalf("GET /admin/faults does not list the console's rule %s: %s", created.ID, body)
	}

	name := "projects/" + project + "/secrets/absent"
	_, err := sc.GetSecret(h.Context(), &secretmanagerpb.GetSecretRequest{Name: name})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "injected fault (rule "+created.ID+")") {
		t.Fatalf("GetSecret under the console's PERMISSION_DENIED rule = %v", err)
	}

	code, body = consoleFaultsDo(t, addr, http.MethodGet, "/api/faults", "", true)
	var page struct {
		Faults []struct {
			ID       string
			Injected int
		}
		Recent []struct{ Rule, Code string }
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil || code != http.StatusOK {
		t.Fatalf("GET /api/faults = %d %s", code, body)
	}
	injected, recorded := 0, false
	for _, f := range page.Faults {
		if f.ID == created.ID {
			injected = f.Injected
		}
	}
	for _, e := range page.Recent {
		if e.Rule == created.ID && e.Code == "PERMISSION_DENIED" {
			recorded = true
		}
	}
	if injected < 1 || !recorded {
		t.Errorf("the console shows rule %s with %d injected and recent fault %v: %s", created.ID, injected, recorded, body)
	}

	if code, body := consoleFaultsDo(t, addr, http.MethodDelete, "/api/faults?id="+url.QueryEscape(created.ID), "", false); code != http.StatusUnauthorized {
		t.Errorf("DELETE /api/faults without the admin token = %d %s; want 401", code, body)
	}
	if code, body := consoleFaultsDo(t, addr, http.MethodDelete, "/api/faults?id="+url.QueryEscape(created.ID), "", true); code != http.StatusOK {
		t.Fatalf("DELETE /api/faults?id=%s = %d %s", created.ID, code, body)
	}
	deleted = true
	if body := listed(); strings.Contains(body, `"id":"`+created.ID+`"`) {
		t.Errorf("GET /admin/faults still lists %s after the console's delete: %s", created.ID, body)
	}
	if _, err := sc.GetSecret(h.Context(), &secretmanagerpb.GetSecretRequest{Name: name}); status.Code(err) == codes.PermissionDenied {
		t.Errorf("GetSecret after the console deleted the rule = %v; want the ordinary answer", err)
	}
}
