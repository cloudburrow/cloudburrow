package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/bttest"

	"github.com/cloudburrow/cloudburrow/internal/bigtablefront"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestBigtableConsoleEditsTheChangeStream (#1523): a table page's Edit
// change stream, against the Bigtable front, which keeps the change log,
// enables the stream with a retention in days, is prefilled with it, refuses
// a retention Bigtable does not take, and disables it with 0.
func TestBigtableConsoleEditsTheChangeStream(t *testing.T) {
	ctx := context.Background()
	emu, err := bttest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(emu.Close)
	f, err := bigtablefront.New(emu.Addr, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = f.Serve(fctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	p := bigtableProvider{endpoint: l.Addr().String()}
	const table = "streamed"
	if _, err := p.Create(ctx, "demo", map[string]string{"table": table, "families": "cf"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, p).Handler())
	t.Cleanup(srv.Close)
	admin, err := bigtable.NewAdminClient(ctx, "demo", bigtableInstance, localOpts(p.endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	prefilled := func() string {
		fields, ok := actionFields(bigtableDetail(t, srv, table).Actions, "changestream")
		if !ok {
			return "absent"
		}
		return fields["retention"]
	}
	if got := prefilled(); got != "0" {
		t.Errorf("Edit change stream is prefilled with %s, want 0", got)
	}
	if code, out := bigtableAct(t, srv, []string{table}, "changestream", map[string]string{"retention": "3"}); code != http.StatusOK {
		t.Fatalf("changestream 3 = %d: %s", code, out)
	}
	if info, err := admin.TableInfo(ctx, table); err != nil || info.ChangeStreamRetention != 72*time.Hour {
		t.Errorf("TableInfo after enabling = %+v, %v", info, err)
	}
	if got := prefilled(); got != "3" {
		t.Errorf("Edit change stream is prefilled with %s, want 3", got)
	}
	// A write is listed in the table's Change stream section.
	client, err := bigtable.NewClient(ctx, "demo", bigtableInstance, localOpts(p.endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	m := bigtable.NewMutation()
	m.Set("cf", "q", bigtable.Timestamp(1000), []byte("streamed value"))
	if err := client.Open(table).Apply(ctx, "row-1", m); err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, sec := range bigtableDetail(t, srv, table).Sections {
		if sec.ID == "changestream" {
			if sec.Listing.Unavailable != "" {
				t.Errorf("the Change stream section is unavailable: %s", sec.Listing.Unavailable)
			}
			for _, it := range sec.Listing.Items {
				listed = append(listed, it.Name+" "+it.Fields["Mutations"])
			}
		}
	}
	if len(listed) != 1 || listed[0] != "row-1 SetCell cf:q = streamed value" {
		t.Errorf("the Change stream section lists %q", listed)
	}
	if code, _ := bigtableAct(t, srv, []string{table}, "changestream", map[string]string{"retention": "9"}); code == http.StatusOK {
		t.Errorf("a retention of 9 days was accepted")
	}
	if code, out := bigtableAct(t, srv, []string{table}, "changestream", map[string]string{"retention": "36h"}); code != http.StatusOK {
		t.Fatalf("changestream 36h = %d: %s", code, out)
	}
	if got := prefilled(); got != "36h0m0s" {
		t.Errorf("Edit change stream is prefilled with %s, want 36h0m0s", got)
	}
	if code, out := bigtableAct(t, srv, []string{table}, "changestream", map[string]string{"retention": "0"}); code != http.StatusOK {
		t.Fatalf("changestream 0 = %d: %s", code, out)
	}
	if info, err := admin.TableInfo(ctx, table); err != nil || info.ChangeStreamRetention != nil {
		t.Errorf("TableInfo after disabling = %+v, %v", info, err)
	}
}
