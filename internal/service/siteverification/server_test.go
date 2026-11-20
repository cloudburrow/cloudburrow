package siteverification

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func call(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestVerifyALocalSite(t *testing.T) {
	s := NewServer(store.NewMemory(), "dev@example.com")
	var file string
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if file != "" && r.URL.Path == "/"+file {
			_, _ = w.Write([]byte("google-site-verification: " + file))
			return
		}
		http.NotFound(w, r)
	}))
	defer site.Close()
	url := site.URL + "/"
	body := `{"site":{"type":"SITE","identifier":"` + url + `"},"verificationMethod":"FILE"}`
	code, tok := call(t, s, http.MethodPost, Prefix+"token", body)
	if code != 200 || !strings.HasPrefix(tok["token"].(string), "google") || tok["method"] != "FILE" {
		t.Fatalf("getToken = %d %v", code, tok)
	}
	res := `{"site":{"type":"SITE","identifier":"` + url + `"}}`
	if code, out := call(t, s, http.MethodPost, Prefix+"webResource?verificationMethod=FILE", res); code != 400 {
		t.Fatalf("insert before the file is served = %d %v", code, out)
	}
	file = tok["token"].(string)
	code, r := call(t, s, http.MethodPost, Prefix+"webResource?verificationMethod=FILE", res)
	if code != 200 || r["id"] == "" {
		t.Fatalf("insert = %d %v", code, r)
	}
	id := r["id"].(string)
	if code, l := call(t, s, http.MethodGet, Prefix+"webResource", ""); code != 200 || len(l["items"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, l)
	}
	esc := strings.ReplaceAll(id, "%", "%25")
	if code, g := call(t, s, http.MethodGet, Prefix+"webResource/"+esc, ""); code != 200 || g["id"] != id {
		t.Fatalf("get = %d %v", code, g)
	}
	if code, _ := call(t, s, http.MethodPut, Prefix+"webResource/"+esc, `{"owners":["other@example.com"]}`); code != 400 {
		t.Errorf("update removing the caller = %d", code)
	}
	if code, u := call(t, s, http.MethodPut, Prefix+"webResource/"+esc, `{"owners":["dev@example.com","other@example.com"]}`); code != 200 || len(u["owners"].([]any)) != 2 {
		t.Errorf("update = %d %v", code, u)
	}
	if code, _ := call(t, s, http.MethodDelete, Prefix+"webResource/"+esc, ""); code != 204 {
		t.Errorf("delete = %d", code)
	}
	if code, _ := call(t, s, http.MethodGet, Prefix+"webResource/"+esc, ""); code != 404 {
		t.Errorf("get after delete = %d", code)
	}
	all, _ := s.Resources()
	if len(all) != 1 || all[0].Owners[0] != "other@example.com" {
		t.Errorf("the other owner keeps it: %+v", all)
	}
}

func TestVerificationNeverLeavesTheMachine(t *testing.T) {
	s := NewServer(store.NewMemory(), "dev@example.com")
	ctx := context.Background()
	if _, err := s.Insert(ctx, "dev@example.com", Resource{Site: Site{Type: "INET_DOMAIN", Identifier: "example.com"}}, "DNS_TXT"); err == nil ||
		!strings.Contains(err.Error(), "public DNS") {
		t.Errorf("DNS_TXT = %v", err)
	}
	// 192.0.2.1 is TEST-NET-1: public, so never dialed.
	if _, err := s.Insert(ctx, "dev@example.com", Resource{Site: Site{Type: "SITE", Identifier: "http://192.0.2.1/"}}, "META"); err == nil ||
		!strings.Contains(err.Error(), "off the machine") {
		t.Errorf("a public site = %v", err)
	}
	if _, err := s.GetToken("dev@example.com", Site{Type: "SITE", Identifier: "http://x/"}, "DNS_TXT"); err == nil {
		t.Errorf("DNS_TXT for a SITE was accepted")
	}
}
