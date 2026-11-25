package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/mailsink"
)

func TestMailPageAPI(t *testing.T) {
	c := New("127.0.0.1:0", nil)
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}
	if code, _ := call("GET", "/api/mail", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("no sink = %d", code)
	}
	sink := mailsink.New()
	if err := sink.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	c.SetMail(sink)
	if err := sink.Send(context.Background(), "monitoring", "a@cloudburrow.localhost", []string{"ops@example.com"}, "Disk full", "Incident 1 opened.\n"); err != nil {
		t.Fatal(err)
	}
	code, list := call("GET", "/api/mail", "")
	msgs, _ := list["messages"].([]any)
	if code != 200 || list["smtp"] != sink.Addr() || len(msgs) != 1 {
		t.Fatalf("list = %d %v", code, list)
	}
	id := msgs[0].(map[string]any)["id"].(string)
	if code, m := call("GET", "/api/mail/"+id, ""); code != 200 || m["subject"] != "Disk full" || !strings.Contains(m["text"].(string), "Incident 1 opened.") {
		t.Fatalf("message = %d %v", code, m)
	}
	if code, _ := call("DELETE", "/api/mail", ""); code != http.StatusBadRequest {
		t.Fatalf("DELETE with no id = %d", code)
	}
	if code, _ := call("DELETE", "/api/mail?id="+id, ""); code != 200 || len(sink.Messages()) != 0 {
		t.Fatalf("DELETE = %d", code)
	}
	if code, _ := call("GET", "/api/mail/"+id, ""); code != http.StatusNotFound {
		t.Fatalf("deleted message = %d", code)
	}
}

func TestClocksAPI(t *testing.T) {
	c := New("127.0.0.1:0", nil)
	var moved time.Duration
	c.SetClocks([]Clock{{ID: "agent-platform", Title: "Agent Platform clock", Advance: func(d time.Duration) (time.Time, error) {
		moved += d
		return time.Now().Add(moved), nil
	}}})
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	post := func(id, body string) int {
		resp, err := http.Post(srv.URL+"/api/clocks/"+id, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for body, want := range map[string]int{`{"duration":"1d2h"}`: 200, `{"duration":"0s"}`: 400, `{"duration":"-1h"}`: 400, `{"duration":"soon"}`: 400, `nope`: 400} {
		if got := post("agent-platform", body); got != want {
			t.Errorf("%s = %d, want %d", body, got, want)
		}
	}
	if moved != 26*time.Hour {
		t.Errorf("moved %v", moved)
	}
	if post("gke", `{"duration":"1h"}`) != http.StatusNotFound {
		t.Error("an unknown clock was advanced")
	}
	resp, err := http.Get(srv.URL + "/api/clocks")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(b), `"agent-platform"`) {
		t.Errorf("clocks = %s", b)
	}
	if _, err := ParseAdvance("400d"); err == nil {
		t.Error("400d was accepted")
	}
}
