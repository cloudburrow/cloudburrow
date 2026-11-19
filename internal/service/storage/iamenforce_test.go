package storage

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// enforceSource is a hierarchy with one project and no policies.
type enforceSource struct{}

func (enforceSource) Ancestry(p string) []string                    { return []string{"projects/" + p} }
func (enforceSource) Policy(string) *iampolicy.Stored               { return nil }
func (enforceSource) CustomRolePermissions(string) ([]string, bool) { return nil, false }
func (enforceSource) Principals(email string) []string              { return []string{iameval.Member(email)} }

const enforcedSA = "reader@p.iam.gserviceaccount.com"

func newEnforcedServer(t *testing.T) *httptest.Server {
	t.Helper()
	e := &iameval.Enforcer{Eval: &iameval.Evaluator{Source: enforceSource{}}, On: func() bool { return true },
		CallerOf: func(b string) iameval.Caller {
			if b == "tok-reader" {
				return iameval.Caller{Email: enforcedSA}
			}
			return iameval.Caller{Invalid: true}
		}}
	host := httptest.NewServer(http.HandlerFunc(e.ServeCheck))
	t.Cleanup(host.Close)
	hc := &http.Client{}
	t.Cleanup(hc.CloseIdleConnections)
	s, err := NewServer(Options{Hosts: []string{"storage.localhost"}, IAMCheck: &iamcheck.Client{Base: host.URL, Client: hc}})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	return h
}

func callAs(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestStorageIAMEnforcement: with a check host, a caller without a role
// is refused with Google's text over JSON and XML; the bucket's
// objectViewer binding lets it read; allUsers does too; an object ACL
// grants only without uniform bucket-level access; a token the host did
// not mint is 401; no token is the instance's own account.
func TestStorageIAMEnforcement(t *testing.T) {
	h := newEnforcedServer(t)
	if code, b := callAs(t, "POST", h.URL+"/storage/v1/b?project=p", "", `{"name":"enf"}`); code != 200 {
		t.Fatalf("insert bucket as the owner: %d %s", code, b)
	}
	if code, b := callAs(t, "POST", h.URL+"/upload/storage/v1/b/enf/o?uploadType=media&name=a.txt", "", "hello"); code != 200 {
		t.Fatalf("upload as the owner: %d %s", code, b)
	}
	want := enforcedSA + " does not have storage.objects.get access to the Google Cloud Storage object. Permission 'storage.objects.get' denied on resource (or it may not exist)."
	code, b := callAs(t, "GET", h.URL+"/storage/v1/b/enf/o/a.txt", "tok-reader", "")
	if code != 403 || !strings.Contains(b, want) || !strings.Contains(b, `"forbidden"`) {
		t.Fatalf("objects.get without a role = %d %s; want 403 %q", code, b, want)
	}
	code, b = callAs(t, "GET", h.URL+"/enf/a.txt", "tok-reader", "")
	if code != 403 || !strings.Contains(b, "<Code>AccessDenied</Code>") || !strings.Contains(b, "<Details>"+xmlEscape(want)+"</Details>") {
		t.Fatalf("XML GET without a role = %d %s", code, b)
	}
	if code, b := callAs(t, "GET", h.URL+"/storage/v1/b/enf/o", "tok-reader", ""); code != 403 || !strings.Contains(b, "storage.objects.list access to the Google Cloud Storage bucket") {
		t.Fatalf("objects.list without a role = %d %s", code, b)
	}
	if code, b := callAs(t, "GET", h.URL+"/storage/v1/b?project=p", "tok-reader", ""); code != 403 || !strings.Contains(b, "storage.buckets.list access to the Google Cloud project") {
		t.Fatalf("buckets.list without a role = %d %s", code, b)
	}
	if code, _ := callAs(t, "GET", h.URL+"/storage/v1/b/enf/o/a.txt", "forged", ""); code != 401 {
		t.Fatalf("objects.get with a forged token = %d; want 401", code)
	}
	// An object ACL grants without uniform bucket-level access.
	if code, b := callAs(t, "POST", h.URL+"/storage/v1/b/enf/o/a.txt/acl", "", `{"entity":"user-`+enforcedSA+`","role":"READER"}`); code != 200 {
		t.Fatalf("objectAccessControls.insert: %d %s", code, b)
	}
	if code, b := callAs(t, "GET", h.URL+"/enf/a.txt", "tok-reader", ""); code != 200 || b != "hello" {
		t.Fatalf("XML GET with an object ACL = %d %s", code, b)
	}
	if code, b := callAs(t, "PATCH", h.URL+"/storage/v1/b/enf", "", `{"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}}`); code != 200 {
		t.Fatalf("enable uniform bucket-level access: %d %s", code, b)
	}
	if code, _ := callAs(t, "GET", h.URL+"/enf/a.txt", "tok-reader", ""); code != 403 {
		t.Fatalf("XML GET with an object ACL under uniform access = %d; want 403", code)
	}
	for _, member := range []string{"serviceAccount:" + enforcedSA, "allUsers"} {
		pol := `{"bindings":[{"role":"roles/storage.objectViewer","members":["` + member + `"]}]}`
		if code, b := callAs(t, "PUT", h.URL+"/storage/v1/b/enf/iam", "", pol); code != 200 {
			t.Fatalf("setIamPolicy: %d %s", code, b)
		}
		if code, b := callAs(t, "GET", h.URL+"/download/storage/v1/b/enf/o/a.txt?alt=media", "tok-reader", ""); code != 200 || b != "hello" {
			t.Fatalf("download with objectViewer for %s = %d %s", member, code, b)
		}
		if code, b := callAs(t, "GET", h.URL+"/storage/v1/b/enf/iam/testPermissions?permissions=storage.objects.get&permissions=storage.objects.delete", "tok-reader", ""); code != 200 ||
			!strings.Contains(b, "storage.objects.get") || strings.Contains(b, "storage.objects.delete") {
			t.Fatalf("testIamPermissions as objectViewer = %d %s", code, b)
		}
		if code, _ := callAs(t, "DELETE", h.URL+"/storage/v1/b/enf/o/a.txt", "tok-reader", ""); code != 403 {
			t.Fatalf("objects.delete as objectViewer = %d; want 403", code)
		}
	}
	if code, b := callAs(t, "PUT", h.URL+"/storage/v1/b/enf/iam", "", `{"bindings":[]}`); code != 200 {
		t.Fatalf("setIamPolicy: %d %s", code, b)
	}
	if code, _ := callAs(t, "GET", h.URL+"/storage/v1/b/enf/o/a.txt", "tok-reader", ""); code != 403 {
		t.Fatalf("objects.get after the revoke = %d; want 403", code)
	}
}
