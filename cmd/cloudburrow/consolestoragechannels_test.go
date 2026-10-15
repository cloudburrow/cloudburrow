package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// TestConsoleStorageChannels: a channel objects.watchAll registered is
// listed on its bucket's Channels tab, and Stop on its row ends it, so the
// tab is empty afterwards (#1281).
func TestConsoleStorageChannels(t *testing.T) {
	ctx := context.Background()
	srv, err := gcsbuiltin.NewServer(gcsbuiltin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv)
	t.Cleanup(h.Close)
	p := storageProvider{endpoint: strings.TrimPrefix(h.URL, "http://")}
	if _, err := p.Create(ctx, "p", map[string]string{"name": "watched"}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.URL+"/storage/v1/b/watched/o/watch", "application/json",
		strings.NewReader(`{"id":"console-ch","type":"web_hook","address":"http://127.0.0.1:9/hook"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("watchAll: %v %v", resp, err)
	}
	resp.Body.Close()
	tab := func() []string {
		d, err := p.Detail(ctx, "p", []string{"watched"})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range d.Sections {
			if s.ID == "channels" {
				if s.Unavailable != "" {
					t.Fatalf("Channels tab unavailable: %s", s.Unavailable)
				}
				var names []string
				for _, it := range s.Listing.Items {
					if it.Fields["Channel ID"] != "console-ch" || it.Fields["Address"] != "http://127.0.0.1:9/hook" {
						t.Errorf("row %+v", it.Fields)
					}
					names = append(names, it.Name)
				}
				return names
			}
		}
		t.Fatal("the bucket's page has no Channels tab")
		return nil
	}
	rows := tab()
	if len(rows) != 1 {
		t.Fatalf("Channels tab rows %v, want one", rows)
	}
	path := []string{"watched", rows[0]}
	if a := p.DetailActions(ctx, "p", path); len(a) != 1 || a[0].ID != "stopchannel" {
		t.Fatalf("actions at %v = %v", path, a)
	}
	if err := p.ActAt(ctx, "p", path, "stopchannel", nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if rows := tab(); len(rows) != 0 {
		t.Errorf("after Stop the tab lists %v", rows)
	}
	if err := p.ActAt(ctx, "p", path, "stopchannel", nil); err == nil {
		t.Error("a second Stop succeeded")
	}
}
