package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/bttest"

	"github.com/cloudburrow/cloudburrow/internal/bigtablefront"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// bigtablePickerConsole is the Bigtable and Bigtable instances screens over
// the Bigtable front in front of the official in-memory server, as the
// instance wires them, with the front's address.
func bigtablePickerConsole(t *testing.T) (*httptest.Server, string) {
	t.Helper()
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
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = f.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	addr := l.Addr().String()
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil,
		bigtableProvider{endpoint: addr}, bigtableInstancesProvider{endpoint: addr}).Handler())
	t.Cleanup(srv.Close)
	return srv, addr
}

// pickerDo sends one console request, as the page does with ?instance= on it.
func pickerDo(t *testing.T, srv *httptest.Server, method, path string, body any) (int, string) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestConsoleBigtableInstancePicker (#1360): the Bigtable screen offers the
// default instance and the ones the admin API lists; with ?instance= its
// Create table, listing, table page, rows, column families and table and
// cell actions all address that instance, read back through the official
// clients on it, and leave the default instance alone; an instance that does
// not exist is refused, not served empty; and the instance's page on the
// instances screen links to its tables.
func TestConsoleBigtableInstancePicker(t *testing.T) {
	srv, addr := bigtablePickerConsole(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	iac, err := bigtable.NewInstanceAdminClient(ctx, "demo", localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer iac.Close()
	const inst = "archive-inst"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: inst, DisplayName: "Archive",
		ClusterId: inst + "-c1", Zone: "us-central1-b", NumNodes: 1}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	admin, err := bigtable.NewAdminClient(ctx, "demo", inst, localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	data, err := bigtable.NewClient(ctx, "demo", inst, localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	defAdmin, err := bigtable.NewAdminClient(ctx, "demo", bigtableInstance, localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer defAdmin.Close()

	scoped := "project=demo&instance=" + inst
	list := func(q string) console.Listing {
		t.Helper()
		code, out := pickerDo(t, srv, http.MethodGet, "/api/resources/bigtable?"+q, nil)
		var l console.Listing
		if code != http.StatusOK || json.Unmarshal([]byte(out), &l) != nil {
			t.Fatalf("listing ?%s = %d: %s", q, code, out)
		}
		return l
	}

	// The service says which parameter carries the instance.
	code, out := pickerDo(t, srv, http.MethodGet, "/api/services", nil)
	var services struct {
		Services []struct{ ID, Scope string }
	}
	if code != http.StatusOK || json.Unmarshal([]byte(out), &services) != nil || len(services.Services) != 2 ||
		services.Services[0].ID != "bigtable" || services.Services[0].Scope != "instance" || services.Services[1].Scope != "" {
		t.Errorf("services = %d: %s", code, out)
	}

	// The picker: the default instance and the created one, the chosen one selected.
	l := list("project=demo")
	if l.Scope == nil || l.Scope.Key != "instance" || l.Scope.Selected != bigtableInstance || l.Scope.Default != bigtableInstance ||
		len(l.Scope.Options) != 2 || l.Scope.Options[0].ID != bigtableInstance || l.Scope.Options[1].ID != inst ||
		l.Scope.Options[1].Label != inst+" (Archive)" {
		t.Fatalf("the default listing's scope = %+v", l.Scope)
	}
	if l = list(scoped); l.Scope == nil || l.Scope.Selected != inst || l.Unavailable != "" || len(l.Items) != 0 {
		t.Fatalf("the created instance's listing = %+v", l)
	}

	// Create table in the chosen instance.
	if code, out := pickerDo(t, srv, http.MethodPost, "/api/resources/bigtable?"+scoped,
		map[string]string{"table": "events", "families": "cf"}); code/100 != 2 {
		t.Fatalf("Create table = %d: %s", code, out)
	}
	if tables, err := admin.Tables(ctx); err != nil || strings.Join(tables, ",") != "events" {
		t.Fatalf("the created instance's tables = %v, %v", tables, err)
	}
	if tables, err := defAdmin.Tables(ctx); err != nil || len(tables) != 0 {
		t.Errorf("the default instance's tables = %v, %v, want none", tables, err)
	}
	if l = list(scoped); len(l.Items) != 1 || l.Items[0].Name != "events" || l.Items[0].Fields["Column families"] != "cf" ||
		!strings.Contains(l.Note, `Instance "`+inst+`"`) {
		t.Errorf("the listing after Create table = %+v", l)
	}
	if l = list("project=demo"); len(l.Items) != 0 {
		t.Errorf("the default listing after Create table = %+v", l.Items)
	}

	act := func(path []string, action string, values map[string]string) {
		t.Helper()
		if code, out := pickerDo(t, srv, http.MethodPost, "/api/actions/bigtable?"+scoped,
			map[string]any{"Path": path, "Action": action, "Values": values}); code != http.StatusOK {
			t.Fatalf("%s at %v = %d: %s", action, path, code, out)
		}
	}
	act([]string{"events"}, "addfamily", map[string]string{"family": "meta"})
	act([]string{"events"}, "writecell", map[string]string{"row": "r1", "family": "cf", "qualifier": "q", "value": "hello"})
	act([]string{"events"}, "writecell", map[string]string{"row": "r2", "family": "meta", "qualifier": "q", "value": "x"})
	act([]string{"events", "r1", "cf:q"}, "append", map[string]string{"value": "!"})
	act([]string{"events", "r2"}, "deleterow", nil)
	info, err := admin.TableInfo(ctx, "events")
	if err != nil || len(info.FamilyInfos) != 2 {
		t.Fatalf("TableInfo = %+v, %v", info, err)
	}
	row, err := data.Open("events").ReadRow(ctx, "r1", bigtable.RowFilter(bigtable.LatestNFilter(1)))
	if err != nil || len(row["cf"]) != 1 || string(row["cf"][0].Value) != "hello!" {
		t.Fatalf("row r1 = %+v, %v", row, err)
	}
	if row, err := data.Open("events").ReadRow(ctx, "r2"); err != nil || len(row) != 0 {
		t.Errorf("row r2 after Delete row = %+v, %v", row, err)
	}

	// The table's page, a row's page, the query form and paging read the
	// chosen instance.
	detail := func(path ...string) console.Detail {
		t.Helper()
		q, _ := url.ParseQuery(scoped)
		for _, p := range path {
			q.Add("name", p)
		}
		code, out := pickerDo(t, srv, http.MethodGet, "/api/detail/bigtable?"+q.Encode(), nil)
		var d console.Detail
		if code != http.StatusOK || json.Unmarshal([]byte(out), &d) != nil {
			t.Fatalf("detail %v = %d: %s", path, code, out)
		}
		return d
	}
	d := detail("events")
	if d.Unavailable != "" || len(d.Sections) < 2 || len(d.Sections[0].Listing.Items) != 1 ||
		d.Sections[0].Listing.Items[0].Name != "r1" || len(d.Sections[1].Listing.Items) != 2 {
		t.Fatalf("the table's page = %+v", d)
	}
	if d := detail("events", "r1"); d.Unavailable != "" || !strings.Contains(mustJSON(t, d), "hello!") {
		t.Errorf("the row's page = %+v", d)
	}
	if code, out := pickerDo(t, srv, http.MethodPost, "/api/query/bigtable?"+scoped,
		map[string]any{"Path": []string{"events"}, "Values": map[string]string{"prefix": "r"}}); code != http.StatusOK || !strings.Contains(out, "r1") {
		t.Errorf("a read of the table = %d: %s", code, out)
	}

	// Delete table removes it from the chosen instance.
	if code, out := pickerDo(t, srv, http.MethodDelete, "/api/resources/bigtable?"+scoped+"&name=events", nil); code/100 != 2 {
		t.Fatalf("Delete table = %d: %s", code, out)
	}
	if tables, err := admin.Tables(ctx); err != nil || len(tables) != 0 {
		t.Errorf("the created instance's tables after Delete = %v, %v", tables, err)
	}

	// An instance that does not exist is refused, and the picker still
	// offers the ones that do.
	l = list("project=demo&instance=nowhere-inst")
	if !strings.Contains(l.Unavailable, `no Bigtable instance "nowhere-inst"`) || l.Scope == nil || len(l.Scope.Options) != 3 {
		t.Errorf("a missing instance's listing = %+v", l)
	}
	if code, out := pickerDo(t, srv, http.MethodPost, "/api/resources/bigtable?project=demo&instance=nowhere-inst",
		map[string]string{"table": "ghost"}); code/100 == 2 || !strings.Contains(out, "no Bigtable instance") {
		t.Errorf("Create table in a missing instance = %d: %s", code, out)
	}

	// The instance's page has a Tables tab linking to its tables on the
	// Bigtable screen, with the instance chosen.
	scopedCtx := console.WithScopeValue(ctx, bigtableScopeKey, inst)
	if _, err := (bigtableProvider{endpoint: addr}).Create(scopedCtx, "demo",
		map[string]string{"table": "audit", "families": "cf"}); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"project": {"demo"}, "name": {inst}}
	code, out = pickerDo(t, srv, http.MethodGet, "/api/detail/bigtable-instances?"+q.Encode(), nil)
	var id console.Detail
	if code != http.StatusOK || json.Unmarshal([]byte(out), &id) != nil {
		t.Fatalf("the instance's page = %d: %s", code, out)
	}
	var tab *console.Section
	for i := range id.Sections {
		if id.Sections[i].ID == "tables" {
			tab = &id.Sections[i]
		}
	}
	if tab == nil || len(tab.Listing.Items) != 1 || tab.Listing.Items[0].Name != "audit" ||
		tab.Listing.Items[0].Link != "/bigtable/audit?instance="+inst+"&project=demo" {
		t.Errorf("the instance's Tables tab = %+v", tab)
	}
	if got := bigtableTablesHref("demo", bigtableInstance, "t"); got != "/bigtable/t?project=demo" {
		t.Errorf("the default instance's table link = %q", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
