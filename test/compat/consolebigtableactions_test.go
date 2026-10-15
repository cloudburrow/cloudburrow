//go:build compat

package compat

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
)

// TestConsoleBigtableTableAndCellActions.
//
// The table and cell actions of #1198, sent through the console API and each
// read back through the official clients against the emulator. On a table of
// rows a1, a2 and b1, Delete rows by prefix a leaves b1, as the data client's
// ReadRows reads it, and Delete all rows leaves no row and the family cf, as
// the admin client's TableInfo reads it; a prefix left empty is refused, and
// both deletes are offered as confirmed by the table's name. Edit deletion
// protection on makes TableInfo read the table as protected, its form is
// then prefilled on, and the console's Delete table is refused with the
// emulator's message while the table stays; off again, Delete table works.
// Increment by 3 on cf:n, the 64-bit integer 5 written by the data client,
// reads 8, and by -10 reads -2; Append value cd on cf:s reads abcd; Increment
// on cf:s, not 8 bytes, is refused with the emulator's message. Delete cells
// and Delete row, one CheckAndMutateRow each, apply to a column and a row
// that hold a cell and are refused, naming what is missing, for a column or
// a row deleted by the data client since, leaving every other cell. The
// changes go through the console, so only the clients' reads are claimed.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/GetTable, google.bigtable.v2.Bigtable/ReadRows
func TestConsoleBigtableTableAndCellActions(t *testing.T) {
	h := New(t)
	btAddr := h.Endpoint(EnvBigtable)
	addr := consoleAddr(t, h)
	t.Setenv("BIGTABLE_EMULATOR_HOST", btAddr)
	ctx := h.Context()
	project := h.Project()

	admin, err := bigtable.NewAdminClient(ctx, project, "cloudburrow")
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	defer admin.Close()
	client, err := bigtable.NewClient(ctx, project, "cloudburrow")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	table := fmt.Sprintf("console-actions-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx := context.Background()
		_ = admin.UpdateTableWithDeletionProtection(ctx, table, bigtable.Unprotected)
		_ = admin.DeleteTable(ctx, table)
	})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/bigtable?project="+project,
		fmt.Sprintf(`{"table":%q,"families":"cf"}`, table)); code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	tbl := client.Open(table)
	write := func(row string, cells map[string][]byte) {
		t.Helper()
		mut := bigtable.NewMutation()
		for q, v := range cells {
			mut.Set("cf", q, bigtable.Now(), v)
		}
		if err := tbl.Apply(ctx, row, mut); err != nil {
			t.Fatalf("Apply %s: %v", row, err)
		}
	}
	keys := func() string {
		t.Helper()
		var out []string
		if err := tbl.ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
			out = append(out, r.Key())
			return true
		}); err != nil {
			t.Fatalf("ReadRows: %v", err)
		}
		return strings.Join(out, ",")
	}
	latest := func(row string) map[string][]byte {
		t.Helper()
		r, err := tbl.ReadRow(ctx, row, bigtable.RowFilter(bigtable.LatestNFilter(1)))
		if err != nil {
			t.Fatalf("ReadRow %s: %v", row, err)
		}
		out := map[string][]byte{}
		for _, item := range r["cf"] {
			out[item.Column] = item.Value
		}
		return out
	}
	act := func(path []string, action string, values map[string]string) (int, string) {
		t.Helper()
		return bigtableConsoleAct(t, addr, project, path, action, values)
	}
	mustAct := func(path []string, action string, values map[string]string) {
		t.Helper()
		if code, out := act(path, action, values); code != http.StatusOK {
			t.Fatalf("%s on %v = %d: %s", action, path, code, out)
		}
	}
	refused := func(path []string, action string, values map[string]string, want string) {
		t.Helper()
		code, out := act(path, action, values)
		if code == http.StatusOK || !strings.Contains(consoleError(t, out), want) {
			t.Errorf("%s on %v = %d: %s; want a refusal saying %q", action, path, code, out, want)
		}
	}

	// The table's own actions, as the page offers them.
	page := bigtableConsolePage(t, addr, project, table)
	if _, ok := confirmedDelete(page.Actions, "deleteallrows"); !ok {
		t.Errorf("Delete all rows is not a confirmed delete: %+v", page.Actions)
	}
	var protection bigtableAction
	for _, a := range page.Actions {
		if a.ID == "deleteprefix" && (!a.Destructive || a.Confirm == "") {
			t.Errorf("Delete rows by prefix is not confirmed by the table's name: %+v", a)
		}
		if a.ID == "protection" {
			protection = a
		}
	}
	if len(protection.Fields) != 1 || protection.Fields[0].Default != "false" {
		t.Errorf("Edit deletion protection on an unprotected table is %+v, want prefilled off", protection)
	}

	// Delete rows by prefix, then Delete all rows.
	for _, r := range []string{"a1", "a2", "b1"} {
		write(r, map[string][]byte{"v": []byte(r)})
	}
	refused([]string{table}, "deleteprefix", map[string]string{"prefix": ""}, "prefix is required")
	if got := keys(); got != "a1,a2,b1" {
		t.Fatalf("a refused Delete rows by prefix left %s", got)
	}
	mustAct([]string{table}, "deleteprefix", map[string]string{"prefix": "a"})
	if got := keys(); got != "b1" {
		t.Errorf("after Delete rows by prefix a, ReadRows reads %s, want b1", got)
	}
	mustAct([]string{table}, "deleteallrows", nil)
	if got := keys(); got != "" {
		t.Errorf("after Delete all rows, ReadRows reads %s, want no row", got)
	}
	if policy, ok := bigtableFamilyPolicy(t, admin, table, "cf"); !ok {
		t.Errorf("Delete all rows removed the family cf (policy %s)", policy)
	}

	// Edit deletion protection, and Delete table refused while it is on.
	mustAct([]string{table}, "protection", map[string]string{"protected": "true"})
	info, err := admin.TableInfo(ctx, table)
	if err != nil {
		t.Fatalf("TableInfo: %v", err)
	}
	if info.DeletionProtection != bigtable.Protected {
		t.Errorf("after Edit deletion protection on, TableInfo reads %v, want Protected", info.DeletionProtection)
	}
	for _, a := range bigtableConsolePage(t, addr, project, table).Actions {
		if a.ID == "protection" && (len(a.Fields) != 1 || a.Fields[0].Default != "true") {
			t.Errorf("Edit deletion protection on a protected table is %+v, want prefilled on", a)
		}
	}
	code, out := consoleDo(t, addr, http.MethodDelete, "/api/resources/bigtable?project="+project+"&name="+table, "")
	if code == http.StatusOK || !strings.Contains(consoleError(t, out), "protected from deletion") {
		t.Errorf("Delete table while protected = %d: %s; want the emulator's refusal", code, out)
	}
	if _, err := admin.TableInfo(ctx, table); err != nil {
		t.Fatalf("the protected table is gone after a refused delete: %v", err)
	}
	mustAct([]string{table}, "protection", map[string]string{"protected": "false"})
	if info, err := admin.TableInfo(ctx, table); err != nil || info.DeletionProtection != bigtable.Unprotected {
		t.Errorf("after Edit deletion protection off, TableInfo reads %+v, %v; want Unprotected", info, err)
	}

	// Increment and Append value.
	five := make([]byte, 8)
	binary.BigEndian.PutUint64(five, 5)
	write("r1", map[string][]byte{"n": five, "s": []byte("ab")})
	mustAct([]string{table, "r1", "cf:n"}, "increment", map[string]string{"amount": "3"})
	if v := latest("r1")["cf:n"]; len(v) != 8 || int64(binary.BigEndian.Uint64(v)) != 8 {
		t.Errorf("after Increment by 3, ReadRow reads cf:n %v, want 8", v)
	}
	mustAct([]string{table, "r1", "cf:n"}, "increment", map[string]string{"amount": "-10"})
	if v := latest("r1")["cf:n"]; len(v) != 8 || int64(binary.BigEndian.Uint64(v)) != -2 {
		t.Errorf("after Increment by -10, ReadRow reads cf:n %v, want -2", v)
	}
	mustAct([]string{table, "r1", "cf:s"}, "append", map[string]string{"value": "cd"})
	if v := string(latest("r1")["cf:s"]); v != "abcd" {
		t.Errorf("after Append value cd, ReadRow reads cf:s %q, want abcd", v)
	}
	refused([]string{table, "r1", "cf:s"}, "increment", map[string]string{"amount": "1"}, "non-64-bit")
	if v := string(latest("r1")["cf:s"]); v != "abcd" {
		t.Errorf("after a refused Increment, ReadRow reads cf:s %q, want abcd", v)
	}

	// The conditional Delete cells and Delete row.
	write("r1", map[string][]byte{"gone": []byte("x")})
	write("r2", map[string][]byte{"v": []byte("y")})
	mustAct([]string{table, "r1", "cf:s"}, "deletecells", nil)
	if cells := latest("r1"); cells["cf:s"] != nil || cells["cf:n"] == nil || cells["cf:gone"] == nil {
		t.Errorf("after Delete cells cf:s, ReadRow reads r1 %q, want cf:n and cf:gone", cells)
	}
	del := bigtable.NewMutation()
	del.DeleteCellsInColumn("cf", "gone")
	if err := tbl.Apply(ctx, "r1", del); err != nil {
		t.Fatal(err)
	}
	refused([]string{table, "r1", "cf:gone"}, "deletecells", nil, "has no cells in cf:gone")
	if cells := latest("r1"); len(cells) != 1 || cells["cf:n"] == nil {
		t.Errorf("after a refused Delete cells, ReadRow reads r1 %q, want cf:n", cells)
	}
	mustAct([]string{table, "r1"}, "deleterow", nil)
	if got := keys(); got != "r2" {
		t.Errorf("after Delete row r1, ReadRows reads %s, want r2", got)
	}
	refused([]string{table, "r1"}, "deleterow", nil, "no row with key r1")
	if got := keys(); got != "r2" {
		t.Errorf("after a refused Delete row, ReadRows reads %s, want r2", got)
	}

	// Delete table, protection off.
	if code, out := consoleDo(t, addr, http.MethodDelete, "/api/resources/bigtable?project="+project+"&name="+table, ""); code != http.StatusOK {
		t.Errorf("Delete table after protection was turned off = %d: %s", code, out)
	}
}
