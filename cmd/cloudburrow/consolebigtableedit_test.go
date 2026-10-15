package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/bttest"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// bigtableConsole is the console serving the Bigtable screen over the
// official in-memory server, with one table holding family "cf", and the
// official clients reading the same server.
func bigtableConsole(t *testing.T, table string) (*httptest.Server, *bigtable.AdminClient, *bigtable.Client) {
	t.Helper()
	ctx := context.Background()
	bt, err := bttest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bt.Close)
	p := bigtableProvider{endpoint: bt.Addr}
	if _, err := p.Create(ctx, "demo", map[string]string{"table": table, "families": "cf"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, p).Handler())
	t.Cleanup(srv.Close)
	admin, err := bigtable.NewAdminClient(ctx, "demo", bigtableInstance, localOpts(bt.Addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	client, err := bigtable.NewClient(ctx, "demo", bigtableInstance, localOpts(bt.Addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return srv, admin, client
}

// bigtableAct performs one action through the console's action route, as the
// page sends it.
func bigtableAct(t *testing.T, srv *httptest.Server, path []string, action string, values map[string]string) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/api/actions/bigtable?project=demo", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// bigtableDetail reads a page as the browser does.
func bigtableDetail(t *testing.T, srv *httptest.Server, path ...string) console.Detail {
	t.Helper()
	q := url.Values{"project": {"demo"}}
	for _, s := range path {
		q.Add("name", s)
	}
	resp, err := http.Get(srv.URL + "/api/detail/bigtable?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d console.Detail
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func familyPolicy(t *testing.T, admin *bigtable.AdminClient, table, family string) (bigtable.GCPolicy, bool) {
	t.Helper()
	info, err := admin.TableInfo(context.Background(), table)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range info.FamilyInfos {
		if f.Name == family {
			return f.FullGCPolicy, true
		}
	}
	return nil, false
}

// TestBigtableFamilyAndRowActionsThroughTheOfficialClients (#797).
//
// Every action the table, row and cell pages offer, performed through the
// console's action route against the official in-memory server, and read
// back through the official admin and data clients: a family added with a
// GC policy is returned by TableInfo with it, its policy edited, and deleted;
// a cell written is read by ReadRow with its value and timestamp; Delete
// cells and Delete row remove what they name. A refusal is the server's own
// message, and a delete of nothing is refused rather than reported done.
func TestBigtableFamilyAndRowActionsThroughTheOfficialClients(t *testing.T) {
	const table = "edits"
	srv, admin, client := bigtableConsole(t, table)
	ctx := context.Background()
	tbl := client.Open(table)

	// Add column family, with both limits and only-when-both.
	code, out := bigtableAct(t, srv, []string{table}, "addfamily",
		map[string]string{"family": "stats", "maxVersions": "3", "maxAge": "7d", "both": "true"})
	if code != http.StatusOK {
		t.Fatalf("addfamily = %d: %s", code, out)
	}
	policy, ok := familyPolicy(t, admin, table, "stats")
	if !ok {
		t.Fatal("TableInfo does not return the family the console added")
	}
	if got, want := policy.String(), "(versions() > 3 && age() > 7d)"; got != want {
		t.Errorf("the added family's policy = %s, want %s", got, want)
	}

	// Its row in the Schema section offers the edit prefilled from it, and
	// addresses it apart from the rows.
	var familyRow *console.Resource
	for _, s := range bigtableDetail(t, srv, table).Sections {
		for i := range s.Listing.Items {
			if s.ID == "families" && s.Listing.Items[i].Name == "stats" {
				familyRow = &s.Listing.Items[i]
			}
		}
	}
	if familyRow == nil {
		t.Fatal("the Schema section has no row for the added family")
	}
	if strings.Join(familyRow.ActsOn, "/") != table+"/families/stats" {
		t.Errorf("the family row's actions address %v", familyRow.ActsOn)
	}
	if ids := strings.Join(actionIDs(familyRow.Actions), ","); ids != "editgc,deletefamily" {
		t.Errorf("the family row offers %s", ids)
	}
	defaults := map[string]string{}
	for _, f := range familyRow.Actions[0].Fields {
		defaults[f.Name] = f.Default
	}
	if defaults["maxVersions"] != "3" || defaults["maxAge"] != "7d" || defaults["both"] != "true" {
		t.Errorf("Edit GC policy is prefilled with %v", defaults)
	}

	// Edit GC policy: saved as prefilled, the policy is unchanged; then one
	// limit only.
	if code, out := bigtableAct(t, srv, familyTarget(table, "stats"), "editgc", defaults); code != http.StatusOK {
		t.Fatalf("editgc as prefilled = %d: %s", code, out)
	}
	if policy, _ := familyPolicy(t, admin, table, "stats"); policy.String() != "(versions() > 3 && age() > 7d)" {
		t.Errorf("saving the edit unchanged changed the policy to %s", policy)
	}
	if code, out := bigtableAct(t, srv, familyTarget(table, "stats"), "editgc",
		map[string]string{"maxVersions": "1"}); code != http.StatusOK {
		t.Fatalf("editgc = %d: %s", code, out)
	}
	if policy, _ := familyPolicy(t, admin, table, "stats"); policy.String() != "versions() > 1" {
		t.Errorf("the edited policy = %s, want versions() > 1", policy)
	}

	// A family that exists is the server's own ALREADY_EXISTS.
	code, out = bigtableAct(t, srv, []string{table}, "addfamily", map[string]string{"family": "stats"})
	if code != http.StatusBadRequest || !strings.Contains(out, "already exists") {
		t.Errorf("adding an existing family = %d: %s", code, out)
	}

	// Write cell from the table's page, at a given timestamp.
	stamp := time.Date(2026, 9, 27, 12, 0, 0, 123000000, time.UTC)
	code, out = bigtableAct(t, srv, []string{table}, "writecell", map[string]string{
		"row": "r1", "family": "cf", "qualifier": "name", "value": "Ada", "timestamp": stamp.Format(time.RFC3339Nano)})
	if code != http.StatusOK {
		t.Fatalf("writecell = %d: %s", code, out)
	}
	// And from the row's page, which names its own row.
	if code, out := bigtableAct(t, srv, []string{table, "r1"}, "writecell",
		map[string]string{"family": "stats", "qualifier": "visits", "value": "3"}); code != http.StatusOK {
		t.Fatalf("writecell on the row = %d: %s", code, out)
	}
	row, err := tbl.ReadRow(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(row["cf"]) != 1 || string(row["cf"][0].Value) != "Ada" || !row["cf"][0].Timestamp.Time().Equal(stamp) {
		t.Errorf("ReadRow cf = %+v, want Ada at %s", row["cf"], stamp)
	}
	if len(row["stats"]) != 1 || row["stats"][0].Column != "stats:visits" || string(row["stats"][0].Value) != "3" {
		t.Errorf("ReadRow stats = %+v", row["stats"])
	}

	// A family that does not exist is refused with the server's own message;
	// a timestamp finer than a millisecond, which Bigtable refuses and bttest
	// truncates, before it is sent.
	code, out = bigtableAct(t, srv, []string{table}, "writecell",
		map[string]string{"row": "r1", "family": "nope", "value": "x"})
	if code != http.StatusBadRequest || !strings.Contains(out, "nope") {
		t.Errorf("a write to an unknown family = %d: %s", code, out)
	}
	code, out = bigtableAct(t, srv, []string{table}, "writecell",
		map[string]string{"row": "r1", "family": "cf", "value": "x", "timestamp": "2026-09-27T12:00:00.000001Z"})
	if code != http.StatusBadRequest || !strings.Contains(out, "finer than a millisecond") {
		t.Errorf("a microsecond timestamp = %d: %s", code, out)
	}

	// The row page's cells each offer Delete cells, at [table, row, column].
	rowPage := bigtableDetail(t, srv, table, "r1")
	if ids := strings.Join(actionIDs(rowPage.Actions), ","); ids != "writecell,deleterow" {
		t.Errorf("the row page offers %s", ids)
	}
	for _, a := range rowPage.Actions {
		if a.ID == "deleterow" && (!a.Destructive || !a.Leaves) {
			t.Errorf("Delete row is %+v; it must be confirmed and leave the page", a)
		}
	}
	for _, c := range rowPage.Sections[0].Listing.Items {
		if ids := strings.Join(actionIDs(c.Actions), ","); ids != "increment,append,deletecells" {
			t.Errorf("cell %s offers %s", c.Name, ids)
		}
	}
	if code, out := bigtableAct(t, srv, []string{table, "r1", "stats:visits"}, "deletecells", nil); code != http.StatusOK {
		t.Fatalf("deletecells = %d: %s", code, out)
	}
	if row, _ := tbl.ReadRow(ctx, "r1"); len(row["stats"]) != 0 || len(row["cf"]) != 1 {
		t.Errorf("after Delete cells the row holds %+v", row)
	}
	code, out = bigtableAct(t, srv, []string{table, "r1", "stats:visits"}, "deletecells", nil)
	if code != http.StatusBadRequest || !strings.Contains(out, "no cells") {
		t.Errorf("deleting cells already gone = %d: %s", code, out)
	}

	// Delete row, and again.
	if code, out := bigtableAct(t, srv, []string{table, "r1"}, "deleterow", nil); code != http.StatusOK {
		t.Fatalf("deleterow = %d: %s", code, out)
	}
	if row, err := tbl.ReadRow(ctx, "r1"); err != nil || len(row) != 0 {
		t.Errorf("ReadRow after Delete row = %v, %v", row, err)
	}
	code, out = bigtableAct(t, srv, []string{table, "r1"}, "deleterow", nil)
	if code != http.StatusBadRequest || !strings.Contains(out, "no row") {
		t.Errorf("deleting a row already gone = %d: %s", code, out)
	}

	// Delete column family.
	if code, out := bigtableAct(t, srv, familyTarget(table, "stats"), "deletefamily", nil); code != http.StatusOK {
		t.Fatalf("deletefamily = %d: %s", code, out)
	}
	if _, ok := familyPolicy(t, admin, table, "stats"); ok {
		t.Error("TableInfo still returns the family the console deleted")
	}
	// Gone, it is no longer offered.
	code, out = bigtableAct(t, srv, familyTarget(table, "stats"), "deletefamily", nil)
	if code != http.StatusBadRequest || !strings.Contains(out, "not available") {
		t.Errorf("deleting a family already gone = %d: %s", code, out)
	}
}

// TestBigtableActionsAreOfferedWhereTheyApply (#797).
//
// A table with no column family is offered Add column family and no write,
// because a write names a family and could only be refused. A row's page and
// a family path never offer each other's actions, even when a row key is a
// family's name.
func TestBigtableActionsAreOfferedWhereTheyApply(t *testing.T) {
	const table = "offers"
	srv, admin, _ := bigtableConsole(t, table)
	ctx := context.Background()

	if err := admin.DeleteColumnFamily(ctx, table, "cf"); err != nil {
		t.Fatal(err)
	}
	if ids := strings.Join(actionIDs(bigtableDetail(t, srv, table).Actions), ","); ids != "addfamily,deleteprefix,deleteallrows,protection" {
		t.Errorf("a table without families offers %s, want no write", ids)
	}
	code, out := bigtableAct(t, srv, []string{table}, "writecell", map[string]string{"row": "r", "family": "cf"})
	if code != http.StatusBadRequest || !strings.Contains(out, "not available") {
		t.Errorf("a write to a table without families = %d: %s", code, out)
	}
	if code, out := bigtableAct(t, srv, []string{table}, "addfamily", map[string]string{"family": "cf"}); code != http.StatusOK {
		t.Fatalf("addfamily = %d: %s", code, out)
	}
	if ids := strings.Join(actionIDs(bigtableDetail(t, srv, table).Actions), ","); ids != "addfamily,writecell,deleteprefix,deleteallrows,protection" {
		t.Errorf("a table with a family offers %s", ids)
	}
	// Both row deletes ask for the table's name back: Delete all rows as a
	// destructive action with no form, Delete rows by prefix by its Confirm.
	for _, a := range bigtableDetail(t, srv, table).Actions {
		if (a.ID == "deleteprefix" && (!a.Destructive || a.Confirm == "")) || (a.ID == "deleteallrows" && (!a.Destructive || len(a.Fields) != 0)) {
			t.Errorf("%s is not confirmed by the table's name: %+v", a.ID, a)
		}
	}

	// A row keyed like the family: its page offers row actions, and a family
	// action on its path is refused.
	code, out = bigtableAct(t, srv, []string{table, "cf"}, "deletefamily", nil)
	if code != http.StatusBadRequest || !strings.Contains(out, "not available") {
		t.Errorf("a family action on a row path = %d: %s", code, out)
	}
	if _, ok := familyPolicy(t, admin, table, "cf"); !ok {
		t.Error("a family action on a row path deleted the family")
	}
	code, out = bigtableAct(t, srv, []string{table, "families", "cf"}, "deleterow", nil)
	if code != http.StatusBadRequest || !strings.Contains(out, "not available") {
		t.Errorf("a row action on a family path = %d: %s", code, out)
	}
}

// TestGCPolicyFormRoundTripsWhatItCanHold (#797).
//
// The GC fields are prefilled in the form they are parsed from, so saving
// them unchanged writes the same policy; a policy they cannot express is
// offered no edit rather than one that would save a different policy.
func TestGCPolicyFormRoundTripsWhatItCanHold(t *testing.T) {
	for _, p := range []bigtable.GCPolicy{
		bigtable.NoGcPolicy(),
		bigtable.MaxVersionsPolicy(1),
		bigtable.MaxAgePolicy(36 * time.Hour),
		bigtable.MaxAgePolicy(90 * time.Minute),
		bigtable.UnionPolicy(bigtable.MaxVersionsPolicy(2), bigtable.MaxAgePolicy(7*24*time.Hour)),
		bigtable.IntersectionPolicy(bigtable.MaxAgePolicy(time.Hour), bigtable.MaxVersionsPolicy(5)),
	} {
		versions, age, both, ok := formatGCPolicy(p)
		if !ok {
			t.Errorf("%s offers no edit", p)
			continue
		}
		back, err := parseGCPolicy(map[string]string{"maxVersions": versions, "maxAge": age, "both": map[bool]string{true: "true"}[both]})
		if err != nil {
			t.Errorf("%s: the form's own values are refused: %v", p, err)
			continue
		}
		// The form writes versions first; the policy's own order is not
		// what it means.
		if normalisePolicy(back.String()) != normalisePolicy(p.String()) {
			t.Errorf("%s came back as %s", p, back)
		}
	}
	for _, p := range []bigtable.GCPolicy{
		bigtable.MaxAgePolicy(90 * time.Second),
		bigtable.UnionPolicy(bigtable.MaxVersionsPolicy(1), bigtable.MaxVersionsPolicy(2)),
		bigtable.UnionPolicy(bigtable.IntersectionPolicy(bigtable.MaxVersionsPolicy(1), bigtable.MaxAgePolicy(time.Hour)),
			bigtable.MaxVersionsPolicy(3)),
		bigtable.UnionPolicy(bigtable.MaxVersionsPolicy(1), bigtable.MaxAgePolicy(time.Hour), bigtable.MaxAgePolicy(2*time.Hour)),
	} {
		if versions, age, both, ok := formatGCPolicy(p); ok {
			t.Errorf("%s offers an edit as %q %q %v, which would change it", p, versions, age, both)
		}
	}
	for _, bad := range []map[string]string{{"maxVersions": "0"}, {"maxAge": "1w"}, {"maxAge": "30s"}, {"maxAge": "0d"}} {
		if _, err := parseGCPolicy(bad); err == nil {
			t.Errorf("%v is accepted", bad)
		}
	}
}

// normalisePolicy orders a two-part policy's parts.
func normalisePolicy(s string) string {
	for _, sep := range []string{" && ", " || "} {
		if parts := strings.Split(strings.Trim(s, "()"), sep); len(parts) == 2 {
			if parts[0] > parts[1] {
				parts[0], parts[1] = parts[1], parts[0]
			}
			return strings.Join(parts, sep)
		}
	}
	return s
}

// TestBigtableTableAndCellActionsThroughTheOfficialClients (#1198).
//
// Delete rows by prefix and Delete all rows (DropRowRange) delete what they
// name and nothing else; Edit deletion protection (UpdateTable) is prefilled
// from the table and read back by TableInfo; Increment and Append value
// (ReadModifyWriteRow) change the cell's latest value; Delete row and Delete
// cells (CheckAndMutateRow) refuse a row or a column that holds nothing.
func TestBigtableTableAndCellActionsThroughTheOfficialClients(t *testing.T) {
	const table = "upkeep"
	srv, admin, client := bigtableConsole(t, table)
	ctx := context.Background()
	tbl := client.Open(table)
	write := func(row, col, value string) {
		t.Helper()
		mut := bigtable.NewMutation()
		mut.Set("cf", col, bigtable.Now(), []byte(value))
		if err := tbl.Apply(ctx, row, mut); err != nil {
			t.Fatal(err)
		}
	}
	keys := func() string {
		t.Helper()
		var out []string
		if err := tbl.ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
			out = append(out, r.Key())
			return true
		}); err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, ",")
	}
	for _, k := range []string{"user#1", "user#2", "order#1"} {
		write(k, "v", "x")
	}

	// Delete rows by prefix needs a prefix, and deletes only those rows.
	if code, out := bigtableAct(t, srv, []string{table}, "deleteprefix", map[string]string{"prefix": ""}); code != http.StatusBadRequest || !strings.Contains(out, "prefix is required") {
		t.Errorf("deleteprefix with no prefix = %d: %s", code, out)
	}
	if code, out := bigtableAct(t, srv, []string{table}, "deleteprefix", map[string]string{"prefix": "user#"}); code != http.StatusOK {
		t.Fatalf("deleteprefix = %d: %s", code, out)
	}
	if got := keys(); got != "order#1" {
		t.Errorf("after Delete rows by prefix the table holds %q, want order#1", got)
	}

	// Increment and Append value on a cell, read back by ReadRow.
	counter := make([]byte, 8)
	counter[7] = 5
	mut := bigtable.NewMutation()
	mut.Set("cf", "n", bigtable.Now(), counter)
	if err := tbl.Apply(ctx, "order#1", mut); err != nil {
		t.Fatal(err)
	}
	if code, out := bigtableAct(t, srv, []string{table, "order#1", "cf:n"}, "increment", map[string]string{"amount": "-2"}); code != http.StatusOK {
		t.Fatalf("increment = %d: %s", code, out)
	}
	if code, out := bigtableAct(t, srv, []string{table, "order#1", "cf:v"}, "append", map[string]string{"value": "yz"}); code != http.StatusOK {
		t.Fatalf("append = %d: %s", code, out)
	}
	row, err := tbl.ReadRow(ctx, "order#1", bigtable.RowFilter(bigtable.LatestNFilter(1)))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range row["cf"] {
		switch item.Column {
		case "cf:n":
			if want := []byte{0, 0, 0, 0, 0, 0, 0, 3}; string(item.Value) != string(want) {
				t.Errorf("after Increment by -2 the counter is %v, want %v", item.Value, want)
			}
		case "cf:v":
			if string(item.Value) != "xyz" {
				t.Errorf("after Append value the cell is %q, want xyz", item.Value)
			}
		}
	}
	if code, out := bigtableAct(t, srv, []string{table, "order#1", "cf:n"}, "increment", map[string]string{"amount": "1.5"}); code != http.StatusBadRequest || !strings.Contains(out, "whole number") {
		t.Errorf("a fractional increment = %d: %s", code, out)
	}

	// A column whose name is a pattern matches only itself.
	write("order#1", "a.c", "x")
	if code, out := bigtableAct(t, srv, []string{table, "order#1", "cf:a.*"}, "deletecells", nil); code != http.StatusBadRequest || !strings.Contains(out, "no cells") {
		t.Errorf("deletecells on a column holding nothing = %d: %s", code, out)
	}
	if row, _ := tbl.ReadRow(ctx, "order#1", bigtable.RowFilter(bigtable.LatestNFilter(1))); len(row["cf"]) != 3 {
		t.Errorf("a refused Delete cells changed the row to %+v", row)
	}

	// Edit deletion protection, prefilled; a protected table is not deleted.
	protection := func() string {
		for _, a := range bigtableDetail(t, srv, table).Actions {
			if a.ID == "protection" {
				return a.Fields[0].Default
			}
		}
		return "absent"
	}
	if got := protection(); got != "false" {
		t.Errorf("Edit deletion protection is prefilled with %s, want false", got)
	}
	if code, out := bigtableAct(t, srv, []string{table}, "protection", map[string]string{"protected": "true"}); code != http.StatusOK {
		t.Fatalf("protection on = %d: %s", code, out)
	}
	if info, err := admin.TableInfo(ctx, table); err != nil || info.DeletionProtection != bigtable.Protected {
		t.Errorf("TableInfo after protection on = %+v, %v", info, err)
	}
	if got := protection(); got != "true" {
		t.Errorf("Edit deletion protection is prefilled with %s, want true", got)
	}
	if code, out := bigtableAct(t, srv, []string{table}, "protection", map[string]string{"protected": "false"}); code != http.StatusOK {
		t.Fatalf("protection off = %d: %s", code, out)
	}
	if info, err := admin.TableInfo(ctx, table); err != nil || info.DeletionProtection != bigtable.Unprotected {
		t.Errorf("TableInfo after protection off = %+v, %v", info, err)
	}

	// Delete all rows.
	if code, out := bigtableAct(t, srv, []string{table}, "deleteallrows", nil); code != http.StatusOK {
		t.Fatalf("deleteallrows = %d: %s", code, out)
	}
	if got := keys(); got != "" {
		t.Errorf("after Delete all rows the table holds %q", got)
	}
	if _, ok := familyPolicy(t, admin, table, "cf"); !ok {
		t.Error("Delete all rows deleted the column family")
	}
}

// TestBigtableCellValue: a cell's bytes read as text when they are printable
// UTF-8 and as hex otherwise, and 8 bytes also read as their big-endian
// int64 (#1214).
func TestBigtableCellValue(t *testing.T) {
	for _, tc := range []struct {
		in   []byte
		want string
	}{
		{[]byte("Ada"), "Ada"},
		{[]byte(""), ""},
		{[]byte("line one\nline two"), "line one\nline two"},
		{[]byte("héllo"), "héllo"},
		{[]byte{0, 0, 0, 0, 0, 0, 0, 3}, "0x0000000000000003 (int64: 3)"},
		{[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe}, "0xfffffffffffffffe (int64: -2)"},
		{[]byte("abcdefgh"), "abcdefgh (int64: 7017280452245743464)"},
		{[]byte{0xc3, 0x28}, "0xc328"},
		{[]byte{0x01, 0x02}, "0x0102"},
	} {
		if got := bigtableCellValue(tc.in); got != tc.want {
			t.Errorf("bigtableCellValue(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
