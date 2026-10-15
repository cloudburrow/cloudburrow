package storage

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// With RelayChannels the server POSTs nothing (#1290): the outbox offers
// each channel's next message, one at a time and in order; a failed report
// holds it back for its retry and a successful one moves to the next.
func TestChannelOutboxRelaysInOrder(t *testing.T) {
	now := time.Now()
	s, err := NewServer(Options{Now: func() time.Time { return now }, RelayChannels: true})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	defer h.Close()
	do := func(method, path, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, h.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	outbox := func() []ChannelDelivery {
		t.Helper()
		resp, err := http.Get(h.URL + ChannelOutboxPath)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var page struct {
			Deliveries []ChannelDelivery `json:"deliveries"`
		}
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&page) != nil {
			t.Fatalf("outbox answered %d", resp.StatusCode)
		}
		return page.Deliveries
	}
	report := func(key, failure string) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"key": key, "error": failure})
		if code, _ := do("POST", ChannelOutboxPath, string(b)); code != http.StatusNoContent {
			t.Fatalf("report %d", code)
		}
	}
	if code, _ := do("POST", "/storage/v1/b?project=p", `{"name":"relayb"}`); code != 200 {
		t.Fatalf("bucket insert %d", code)
	}
	if code, _ := do("POST", "/storage/v1/b/relayb/o/watch", `{"id":"c1","type":"web_hook","address":"http://127.0.0.1:1/hook","token":"tok"}`); code != 200 {
		t.Fatalf("watchAll %d", code)
	}
	if code, _ := do("POST", "/upload/storage/v1/b/relayb/o?uploadType=media&name=a.txt", "hello"); code != 200 {
		t.Fatalf("upload %d", code)
	}

	d := outbox()
	if len(d) != 1 || d[0].Address != "http://127.0.0.1:1/hook" || d[0].Header.Get("X-Goog-Resource-State") != "sync" ||
		d[0].Header.Get("X-Goog-Message-Number") != "1" || d[0].Header.Get("X-Goog-Channel-Token") != "tok" || len(d[0].Body) != 0 {
		t.Fatalf("first offer = %+v; want the sync message alone", d)
	}
	report(d[0].Key, "refused")
	if d := outbox(); len(d) != 0 {
		t.Fatalf("offered %+v during the failed message's retry wait", d)
	}
	now = now.Add(time.Minute)
	if d2 := outbox(); len(d2) != 1 || d2[0].Key != d[0].Key {
		t.Fatalf("after the wait %+v; want the sync message again", d2)
	}
	report(d[0].Key, "")
	d = outbox()
	if len(d) != 1 || d[0].Header.Get("X-Goog-Resource-State") != "exists" || d[0].Header.Get("X-Goog-Message-Number") != "2" ||
		!strings.Contains(string(d[0].Body), `"a.txt"`) || d[0].Header.Get("Content-Type") == "" {
		t.Fatalf("second offer = %+v; want exists with the object", d)
	}
	report(d[0].Key, "")
	if d := outbox(); len(d) != 0 {
		t.Fatalf("offered %+v with nothing pending", d)
	}
	if del, drop := s.ChannelStats(); del != 2 || drop != 0 {
		t.Errorf("stats %d delivered %d dropped; want 2, 0", del, drop)
	}

	// A message that keeps failing is given up on after maxChannelAttempts.
	if code, _ := do("DELETE", "/storage/v1/b/relayb/o/a.txt", ""); code != 204 {
		t.Fatalf("delete %d", code)
	}
	for i := 0; i < maxChannelAttempts; i++ {
		now = now.Add(time.Minute)
		d := outbox()
		if len(d) != 1 || d[0].Header.Get("X-Goog-Resource-State") != "not_exists" {
			t.Fatalf("attempt %d offer = %+v", i, d)
		}
		report(d[0].Key, "refused")
	}
	now = now.Add(time.Minute)
	if d := outbox(); len(d) != 0 {
		t.Fatalf("offered %+v after giving up", d)
	}
	if _, drop := s.ChannelStats(); drop != 1 {
		t.Errorf("dropped %d; want 1", drop)
	}
}

// Without RelayChannels the outbox is refused: the server delivers itself.
func TestChannelOutboxRefusedWhenNotRelaying(t *testing.T) {
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	defer h.Close()
	resp, err := http.Get(h.URL + ChannelOutboxPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("outbox answered %d; want 409", resp.StatusCode)
	}
}

// Channel messages go to local addresses only: CloudBurrow never calls
// outside the developer's machine.
func TestLocalChannelClientRefusesOutsideAddresses(t *testing.T) {
	for ip, want := range map[string]bool{"127.0.0.1": true, "::1": true, "10.1.2.3": true, "172.18.0.1": true,
		"192.168.65.254": true, "169.254.1.1": true, "fd00::1": true, "8.8.8.8": false, "142.250.1.1": false, "2001:4860::1": false} {
		if got := LocalChannelAddress(net.ParseIP(ip)); got != want {
			t.Errorf("LocalChannelAddress(%s) = %v, want %v", ip, got, want)
		}
	}
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	c := LocalChannelClient()
	ctx := context.Background()
	if err := PostChannelDelivery(ctx, c, ChannelDelivery{Address: ok.URL}); err != nil {
		t.Errorf("loopback delivery: %v", err)
	}
	// An address outside is refused before any packet is sent.
	err := PostChannelDelivery(ctx, c, ChannelDelivery{Address: "http://8.8.8.8:9/"})
	if err == nil || !strings.Contains(err.Error(), "not a local address") {
		t.Errorf("outside delivery = %v; want refused as not local", err)
	}
}
