package storage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type hookCall struct {
	h    http.Header
	body string
}

// A channel gets its sync message, then exists and not_exists with the
// object as the body, in number order; channels.stop ends it, and an
// expired channel gets nothing.
func TestChannelLifecycle(t *testing.T) {
	var mu sync.Mutex
	var got []hookCall
	fail := true
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if fail { // the first delivery fails, and is retried
			fail = false
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		got = append(got, hookCall{r.Header.Clone(), string(b)})
	}))
	defer hook.Close()
	now := time.Now()
	s, err := NewServer(Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	defer h.Close()
	post := func(path, body string) (int, map[string]any) {
		resp, err := http.Post(h.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := post("/storage/v1/b?project=p", `{"name":"chb"}`); code != 200 {
		t.Fatalf("bucket insert %d", code)
	}
	code, ch := post("/storage/v1/b/chb/o/watch", `{"id":"c1","type":"web_hook","address":"`+hook.URL+`","token":"tok"}`)
	if code != 200 || ch["kind"] != "api#channel" || ch["id"] != "c1" || ch["resourceId"] == "" || ch["token"] != "tok" {
		t.Fatalf("watchAll = %d %v", code, ch)
	}
	if code, _ := post("/storage/v1/b/chb/o/watch", `{"id":"c1","type":"web_hook","address":"`+hook.URL+`"}`); code != 400 {
		t.Errorf("a second channel c1 = %d, want 400", code)
	}
	if code, _ := post("/upload/storage/v1/b/chb/o?uploadType=media&name=a.txt", "hi"); code != 200 {
		t.Fatalf("upload %d", code)
	}
	req, _ := http.NewRequest(http.MethodDelete, h.URL+"/storage/v1/b/chb/o/a.txt", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 204 {
		t.Fatalf("delete: %v %v", resp, err)
	}
	ctx := context.Background()
	if left := s.DispatchChannels(ctx); left != 3 {
		t.Errorf("after a failed sync, %d left, want 3 (the rest wait behind it)", left)
	}
	if left := s.DispatchChannels(ctx); left != 0 {
		t.Errorf("%d left", left)
	}
	mu.Lock()
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	for i, want := range []string{"sync", "exists", "not_exists"} {
		hd := got[i].h
		if hd.Get("X-Goog-Resource-State") != want || hd.Get("X-Goog-Message-Number") != []string{"1", "2", "3"}[i] ||
			hd.Get("X-Goog-Channel-ID") != "c1" || hd.Get("X-Goog-Channel-Token") != "tok" ||
			hd.Get("X-Goog-Resource-ID") != ch["resourceId"] || !strings.HasSuffix(hd.Get("X-Goog-Resource-URI"), "/storage/v1/b/chb/o") {
			t.Errorf("message %d headers %v", i, hd)
		}
		if i > 0 && !strings.Contains(got[i].body, `"name":"a.txt"`) {
			t.Errorf("message %d body %s", i, got[i].body)
		}
	}
	if got[0].body != "" {
		t.Errorf("sync body %q", got[0].body)
	}
	got = nil
	mu.Unlock()

	resp, err := http.Get(h.URL + ChannelsPath + "?bucket=chb")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"c1"`) {
		t.Errorf("channels listing %s", b)
	}
	if code, _ := post("/storage/v1/channels/stop", `{"id":"c1","resourceId":"nope"}`); code != 404 {
		t.Errorf("stop unknown = %d", code)
	}
	if code, _ := post("/storage/v1/channels/stop", `{"id":"c1","resourceId":"`+ch["resourceId"].(string)+`"}`); code != 204 {
		t.Errorf("stop = %d", code)
	}
	post("/upload/storage/v1/b/chb/o?uploadType=media&name=b.txt", "hi")
	// An expiring channel, then past its expiration.
	exp := now.Add(time.Minute).UnixMilli()
	code, ch2 := post("/storage/v1/b/chb/o/watch", `{"id":"c2","type":"web_hook","address":"`+hook.URL+`","expiration":"`+itoa(exp)+`"}`)
	if code != 200 || ch2["expiration"] != itoa(exp) {
		t.Fatalf("watchAll with expiration = %d %v", code, ch2)
	}
	s.DispatchChannels(ctx)
	now = now.Add(2 * time.Minute)
	post("/upload/storage/v1/b/chb/o?uploadType=media&name=c.txt", "hi")
	s.DispatchChannels(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].h.Get("X-Goog-Channel-ID") != "c2" || got[0].h.Get("X-Goog-Resource-State") != "sync" ||
		got[0].h.Get("X-Goog-Channel-Expiration") == "" {
		for _, c := range got {
			t.Logf("%v", c.h)
		}
		t.Errorf("after stop and expiry got %d messages, want c2's sync alone", len(got))
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
