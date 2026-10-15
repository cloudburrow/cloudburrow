package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// The relay takes a relaying server's channel messages and POSTs them on
// this machine, in order, with the server's headers and body (#1290).
func TestStorageChannelRelayDeliversOnTheHost(t *testing.T) {
	got := make(chan string, 8)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- r.Header.Get("X-Goog-Resource-State") + " " + r.Header.Get("X-Goog-Message-Number") + " " + string(b)
	}))
	defer hook.Close()
	srv, err := gcsbuiltin.NewServer(gcsbuiltin.Options{RelayChannels: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := httptest.NewServer(srv)
	defer h.Close()
	post := func(path, body string) {
		t.Helper()
		resp, err := http.Post(h.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("POST %s = %d", path, resp.StatusCode)
		}
	}
	post("/storage/v1/b?project=p", `{"name":"relayed"}`)
	post("/storage/v1/b/relayed/o/watch", `{"id":"c","type":"web_hook","address":"`+hook.URL+`/n"}`)

	r := newStorageChannelRelay(nil, io.Discard)
	r.addr = func() string { return strings.TrimPrefix(h.URL, "http://") }
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	post("/upload/storage/v1/b/relayed/o?uploadType=media&name=x.txt", "hi")
	for _, want := range []string{"sync 1 ", "exists 2 "} {
		select {
		case m := <-got:
			if !strings.HasPrefix(m, want) || (want == "exists 2 " && !strings.Contains(m, `"x.txt"`)) {
				t.Fatalf("received %q, want %q…", m, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no %q message within 10s", want)
		}
	}
}
