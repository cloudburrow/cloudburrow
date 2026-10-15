//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
)

// TestConsoleBigtableInstancePicker (#1360).
//
// The Bigtable screen on an instance made with the official
// InstanceAdminClient, chosen with ?instance= as the console's picker sends
// it. The listing's picker offers the default instance, cloudburrow, and
// the created one. Create table makes a table the admin client on that
// instance lists, with its family, and the default instance does not;
// Add column family, Write cell, Append value and Delete rows by prefix
// change what the clients on that instance read; the table's page lists the
// row and both families; Delete table removes it from that instance. An
// instance that does not exist is refused rather than served empty. The
// instance's page on the instances screen has a Tables tab linking each
// table to the Bigtable screen with the instance chosen. The changes go
// through the console, so only the clients' reads are claimed.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/ListTables, google.bigtable.admin.v2.BigtableTableAdmin/GetTable, google.bigtable.v2.Bigtable/ReadRows
func TestConsoleBigtableInstancePicker(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h)
	addr := consoleAddr(t, h)
	t.Setenv("BIGTABLE_EMULATOR_HOST", h.Endpoint(EnvBigtable))
	ctx := h.Context()
	project := h.Project()

	id := fmt.Sprintf("cb-pick-%d", time.Now().UnixNano()%1e8)
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "Picker instance",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	admin, err := bigtable.NewAdminClient(ctx, project, id)
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	defer admin.Close()
	client, err := bigtable.NewClient(ctx, project, id)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()
	defAdmin, err := bigtable.NewAdminClient(ctx, project, "cloudburrow")
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	defer defAdmin.Close()

	scoped := "project=" + url.QueryEscape(project) + "&instance=" + id
	table := fmt.Sprintf("picker-%d", time.Now().UnixNano()%1e8)

	var l struct {
		Unavailable string
		Note        string
		Items       []struct {
			Name   string
			Fields map[string]string
		}
		Scope *struct {
			Key, Selected, Default string
			Options                []struct{ ID, Label string }
		}
	}
	list := func(q string) {
		t.Helper()
		code, out := consoleDo(t, addr, http.MethodGet, "/api/resources/bigtable?"+q, "")
		l.Items, l.Scope = nil, nil
		if code != http.StatusOK || json.Unmarshal([]byte(out), &l) != nil {
			t.Fatalf("console listing ?%s = %d: %s", q, code, out)
		}
	}
	list(scoped)
	if l.Unavailable != "" || l.Scope == nil || l.Scope.Key != "instance" || l.Scope.Selected != id || l.Scope.Default != "cloudburrow" {
		t.Fatalf("the created instance's listing = %+v", l)
	}
	var ids []string
	for _, o := range l.Scope.Options {
		ids = append(ids, o.ID)
	}
	if len(ids) < 2 || ids[0] != "cloudburrow" || !strings.Contains(strings.Join(ids, ","), id) {
		t.Errorf("the picker offers %v, want cloudburrow and %s", ids, id)
	}

	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/bigtable?"+scoped,
		fmt.Sprintf(`{"table":%q,"families":"cf"}`, table)); code != http.StatusOK {
		t.Fatalf("console Create table = %d: %s", code, out)
	}
	t.Cleanup(func() { _ = admin.DeleteTable(h.Context(), table) })
	if tables, err := admin.Tables(ctx); err != nil || strings.Join(tables, ",") != table {
		t.Fatalf("after Create table, %s lists %v, %v", id, tables, err)
	}
	if tables, err := defAdmin.Tables(ctx); err != nil || strings.Contains(strings.Join(tables, ","), table) {
		t.Errorf("after Create table in %s, the default instance lists %v, %v", id, tables, err)
	}
	list(scoped)
	if len(l.Items) != 1 || l.Items[0].Name != table || l.Items[0].Fields["Column families"] != "cf" {
		t.Errorf("the listing after Create table = %+v", l.Items)
	}

	act := func(path []string, action string, values map[string]string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
		if code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/bigtable?"+scoped, string(body)); code != http.StatusOK {
			t.Fatalf("%s at %v = %d: %s", action, path, code, out)
		}
	}
	act([]string{table}, "addfamily", map[string]string{"family": "meta"})
	act([]string{table}, "writecell", map[string]string{"row": "a1", "family": "cf", "qualifier": "v", "value": "ab"})
	act([]string{table}, "writecell", map[string]string{"row": "b1", "family": "meta", "qualifier": "v", "value": "x"})
	act([]string{table, "a1", "cf:v"}, "append", map[string]string{"value": "cd"})
	act([]string{table}, "deleteprefix", map[string]string{"prefix": "b"})
	info, err := admin.TableInfo(ctx, table)
	if err != nil || len(info.Families) != 2 {
		t.Fatalf("after Add column family, TableInfo = %+v, %v", info, err)
	}
	var keys []string
	if err := client.Open(table).ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		keys = append(keys, r.Key())
		if r.Key() == "a1" && (len(r["cf"]) == 0 || string(r["cf"][0].Value) != "abcd") {
			t.Errorf("a1 reads %+v, want cf:v abcd", r)
		}
		return true
	}); err != nil {
		t.Fatalf("ReadRows: %v", err)
	}
	if strings.Join(keys, ",") != "a1" {
		t.Errorf("after Delete rows by prefix b, ReadRows reads %v, want a1", keys)
	}

	q := url.Values{"project": {project}, "instance": {id}, "name": {table}}
	code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/bigtable?"+q.Encode(), "")
	if code != http.StatusOK || !strings.Contains(out, `"name":"a1"`) || !strings.Contains(out, `"name":"meta"`) {
		t.Errorf("the table's page = %d: %s", code, out)
	}

	consoleBigtableInstance(t, addr, project, id)
	q = url.Values{"project": {project}, "name": {id}}
	_, out = consoleDo(t, addr, http.MethodGet, "/api/detail/bigtable-instances?"+q.Encode(), "")
	var page struct {
		Sections []struct {
			ID      string
			Listing struct{ Items []struct{ Name, Link string } }
		}
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("decode the instance's page: %v: %s", err, out)
	}
	want := fmt.Sprintf("/bigtable/%s?instance=%s&project=%s", table, id, url.QueryEscape(project))
	var link string
	for _, s := range page.Sections {
		if s.ID == "tables" {
			for _, it := range s.Listing.Items {
				if it.Name == table {
					link = it.Link
				}
			}
		}
	}
	if link != want {
		t.Errorf("the instance's Tables tab links %s to %q, want %q: %s", table, link, want, out)
	}

	if code, out := consoleDo(t, addr, http.MethodDelete, "/api/resources/bigtable?"+scoped+"&name="+table, ""); code/100 != 2 {
		t.Fatalf("console Delete table = %d: %s", code, out)
	}
	if tables, err := admin.Tables(ctx); err != nil || len(tables) != 0 {
		t.Errorf("after Delete table, %s lists %v, %v", id, tables, err)
	}

	list("project=" + url.QueryEscape(project) + "&instance=no-such-inst")
	if !strings.Contains(l.Unavailable, `no Bigtable instance "no-such-inst"`) {
		t.Errorf("a missing instance's listing = %+v", l)
	}
}
