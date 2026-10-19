//go:build browser

package browser

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// The Bigtable table and cell actions of #1198, and Firestore's Count only,
// in the emulators shard, whose instance serves Bigtable and Firestore. Each
// change made in the browser is read back with the official Go client
// against the emulator itself.

// envBigtable is the Bigtable emulator's address, which the emulators
// shard's browser step exports.
const envBigtable = "CLOUDBURROW_TEST_BIGTABLE"

// bigtableInstanceID is the one instance the console addresses.
const bigtableInstanceID = "cloudburrow"

// bigtableClients are the official admin and data clients against the
// emulator, and a table of their own with one family, cf, removed when the
// test ends (unprotected first, in case the test left it protected).
func bigtableClients(t *testing.T, project string) (*bigtable.AdminClient, *bigtable.Client, string) {
	t.Helper()
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envBigtable)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the emulator itself", envBigtable)
	}
	t.Setenv("BIGTABLE_EMULATOR_HOST", addr)
	ctx := context.Background()
	admin, err := bigtable.NewAdminClient(ctx, project, bigtableInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	client, err := bigtable.NewClient(ctx, project, bigtableInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	table := fmt.Sprintf("browser-%d", time.Now().UnixNano()%1e12)
	if err := admin.CreateTable(ctx, table); err != nil {
		t.Fatalf("CreateTable with the official client: %v", err)
	}
	if err := admin.CreateColumnFamily(ctx, table, "cf"); err != nil {
		t.Fatalf("CreateColumnFamily with the official client: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = admin.UpdateTableWithDeletionProtection(ctx, table, bigtable.Unprotected)
		_ = admin.DeleteTable(ctx, table)
		_ = client.Close()
		_ = admin.Close()
	})
	return admin, client, table
}

// bigtableRowKeys are the keys of every row in table, read with the data
// client.
func bigtableRowKeys(t *testing.T, client *bigtable.Client, table string) []string {
	t.Helper()
	var keys []string
	if err := client.Open(table).ReadRows(context.Background(), bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		keys = append(keys, r.Key())
		return true
	}); err != nil {
		t.Fatalf("ReadRows with the official client: %v", err)
	}
	return keys
}

// bigtableLatest is the latest value of each column of a row, read with the
// data client.
func bigtableLatest(t *testing.T, client *bigtable.Client, table, row string) map[string][]byte {
	t.Helper()
	r, err := client.Open(table).ReadRow(context.Background(), row, bigtable.RowFilter(bigtable.LatestNFilter(1)))
	if err != nil {
		t.Fatalf("ReadRow with the official client: %v", err)
	}
	out := map[string][]byte{}
	for _, items := range r {
		for _, item := range items {
			out[item.Column] = item.Value
		}
	}
	return out
}

func bigtableWrite(t *testing.T, client *bigtable.Client, table, row string, cells map[string][]byte) {
	t.Helper()
	mut := bigtable.NewMutation()
	for col, v := range cells {
		mut.Set("cf", strings.TrimPrefix(col, "cf:"), bigtable.Now(), v)
	}
	if err := client.Open(table).Apply(context.Background(), row, mut); err != nil {
		t.Fatalf("Apply to %s with the official client: %v", row, err)
	}
}

// confirmByName types word into the open confirmation and presses Enter.
func (p *tab) confirmByName(word string) {
	p.t.Helper()
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #confirm-input`), word), chromedp.KeyEvent(kb.Enter))
}

// modalRefusal waits for an open dialog's error and returns its text.
func (p *tab) modalRefusal() string {
	p.t.Helper()
	p.waitFor(`[...document.querySelectorAll(".modal .form-error")].some((e) => !e.hidden && e.textContent)`)
	var text string
	p.eval(`[...document.querySelectorAll(".modal .form-error")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &text)
	return text
}

// cellMenu opens a cell's row menu on a row's page and picks an item.
func (p *tab) cellMenu(column, item string) {
	p.t.Helper()
	sel := fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+column)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, sel))
	p.run(chromedp.Click(chromedp.CSS(sel)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, item)
}

// TestBigtableDeleteRowsByPrefixAndAllRowsThroughTheBrowser (#1198). A
// table holds rows a1, a2 and b1, written with the official client. On the
// table's page, Delete rows by prefix with the prefix a asks for the table's
// name back before anything is sent, and then leaves b1 alone, as the data
// client's ReadRows reads it. Delete all rows, confirmed by the table's name,
// leaves no row and the column family cf, as the admin client's TableInfo
// reads it.
func TestBigtableDeleteRowsByPrefixAndAllRowsThroughTheBrowser(t *testing.T) {
	needService(t, "bigtable")
	project := uniqueProject(t)
	admin, client, table := bigtableClients(t, project)
	p := open(t)
	q := "?project=" + project
	for _, r := range []string{"a1", "a2", "b1"} {
		bigtableWrite(t, client, table, r, map[string][]byte{"cf:v": []byte(r)})
	}

	p.navigate("/bigtable/" + table + q)
	p.clickText("#view .page-actions button", "Delete rows by prefix")
	p.waitFor(`document.querySelector(".modal #f-prefix") !== null`)
	p.setField(".modal #f-prefix", "a")
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal #confirm-input") !== null`)
	if sent := p.sent(http.MethodPost, "/api/actions/bigtable"); len(sent) != 0 {
		t.Fatalf("Delete rows by prefix was sent before it was confirmed: %v", sent)
	}
	p.confirmByName(table)
	p.waitFor(`document.querySelector(".modal") === null`)
	if keys := bigtableRowKeys(t, client, table); strings.Join(keys, ",") != "b1" {
		t.Fatalf("after Delete rows by prefix a, ReadRows reads %v, want b1", keys)
	}

	p.clickText("#view .page-actions button", "Delete all rows")
	p.confirmByName(table)
	p.waitFor(`document.querySelector(".modal") === null`)
	if keys := bigtableRowKeys(t, client, table); len(keys) != 0 {
		t.Errorf("after Delete all rows, ReadRows reads %v, want no row", keys)
	}
	info, err := admin.TableInfo(context.Background(), table)
	if err != nil {
		t.Fatalf("TableInfo: %v", err)
	}
	if len(info.Families) != 1 || info.Families[0] != "cf" {
		t.Errorf("after Delete all rows, the table's families are %v, want cf", info.Families)
	}
}

// TestBigtableDeletionProtectionRefusesDeleteTableThroughTheBrowser (#1198).
// Edit deletion protection on a table's page opens unchecked; checked and
// saved, the admin client's TableInfo reads the table as protected, and the
// form opens checked. The table's Delete on the Bigtable list, confirmed by
// name, is then refused with the emulator's message, and the table is still
// there.
func TestBigtableDeletionProtectionRefusesDeleteTableThroughTheBrowser(t *testing.T) {
	needService(t, "bigtable")
	project := uniqueProject(t)
	admin, _, table := bigtableClients(t, project)
	p := open(t)
	q := "?project=" + project
	ctx := context.Background()

	openForm := func() bool {
		p.navigate("/bigtable/" + table + q)
		p.clickText("#view .page-actions button", "Edit deletion protection")
		p.waitFor(`document.querySelector(".modal #f-protected") !== null`)
		var checked bool
		p.eval(`document.querySelector(".modal #f-protected").checked`, &checked)
		return checked
	}
	if openForm() {
		t.Error("Edit deletion protection opens checked on an unprotected table")
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal #f-protected`)))
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	info, err := admin.TableInfo(ctx, table)
	if err != nil {
		t.Fatalf("TableInfo: %v", err)
	}
	if info.DeletionProtection != bigtable.Protected {
		t.Fatalf("after Edit deletion protection, TableInfo reads protection %v, want Protected", info.DeletionProtection)
	}
	if !openForm() {
		t.Error("Edit deletion protection opens unchecked on a protected table")
	}
	p.clickText(".modal button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)

	p.navigate("/bigtable" + q)
	sel := fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+table)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, sel))
	p.run(chromedp.Click(chromedp.CSS(sel)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Delete")
	p.confirmByName(table)
	if refusal := p.modalRefusal(); !strings.Contains(refusal, "protected from deletion") {
		t.Errorf("Delete of a protected table was answered %q, want the emulator's refusal", refusal)
	}
	p.forgive()
	if _, err := admin.TableInfo(ctx, table); err != nil {
		t.Errorf("the protected table is gone after a refused Delete: %v", err)
	}
}

// TestBigtableIncrementAndAppendValueThroughTheBrowser (#1198). A row holds
// cf:n, the 64-bit big-endian integer 5, and cf:s, the text ab, written with
// the official client. On the row's page, Increment on cf:n, prefilled 1,
// by 3 makes the data client read 8; Append value cd on cf:s makes it read
// abcd; Increment on cf:s, which is not 8 bytes, is refused with the
// emulator's message and leaves it abcd. The page shows cf:n as hex with its
// int64, and cf:s as its text (#1214).
func TestBigtableIncrementAndAppendValueThroughTheBrowser(t *testing.T) {
	needService(t, "bigtable")
	project := uniqueProject(t)
	_, client, table := bigtableClients(t, project)
	p := open(t)
	q := "?project=" + project
	five := make([]byte, 8)
	binary.BigEndian.PutUint64(five, 5)
	bigtableWrite(t, client, table, "r1", map[string][]byte{"cf:n": five, "cf:s": []byte("ab")})

	p.navigate("/bigtable/" + table + "/r1" + q)
	p.cellMenu("cf:n", "Increment")
	p.waitFor(`document.querySelector(".modal #f-amount") !== null`)
	var prefill string
	p.eval(`document.querySelector(".modal #f-amount").value`, &prefill)
	if prefill != "1" {
		t.Errorf("Increment's amount is prefilled %q, want 1", prefill)
	}
	p.setField(".modal #f-amount", "3")
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if v := bigtableLatest(t, client, table, "r1")["cf:n"]; len(v) != 8 || binary.BigEndian.Uint64(v) != 8 {
		t.Errorf("after Increment by 3, the data client reads cf:n %v, want the integer 8", v)
	}
	// The counter's 8 bytes are not text, so the page shows them as hex and
	// as the big-endian int64 they encode (#1214).
	p.waitFor(`document.body.innerText.includes("0x0000000000000008 (int64: 8)")`)

	p.cellMenu("cf:s", "Append value")
	p.waitFor(`document.querySelector(".modal #f-value") !== null`)
	p.setField(".modal #f-value", "cd")
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if v := string(bigtableLatest(t, client, table, "r1")["cf:s"]); v != "abcd" {
		t.Errorf("after Append value cd, the data client reads cf:s %q, want abcd", v)
	}
	p.waitFor(`[...document.querySelectorAll("td")].some(td => td.innerText.trim() === "abcd")`)

	p.cellMenu("cf:s", "Increment")
	p.waitFor(`document.querySelector(".modal #f-amount") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
	if refusal := p.modalRefusal(); !strings.Contains(refusal, "non-64-bit") {
		t.Errorf("Increment on a text cell was answered %q, want the emulator's refusal", refusal)
	}
	p.forgive()
	if v := string(bigtableLatest(t, client, table, "r1")["cf:s"]); v != "abcd" {
		t.Errorf("after a refused Increment, the data client reads cf:s %q, want abcd", v)
	}
}

// TestBigtableConditionalDeleteRowAndCellsThroughTheBrowser (#1198). Delete
// cells and Delete row are each one CheckAndMutateRow, applied only when the
// column or the row holds a cell. On r1's page, Delete cells on cf:a,
// confirmed by its name, leaves cf:b alone; the cf:b column is then deleted
// with the official client behind the page's back, and its Delete cells is
// refused, saying the row has no cells in it. Delete row on r1, confirmed by
// its key, returns to the table and the data client reads no r1; r2's page,
// whose row is deleted with the client behind its back, has its Delete row
// refused, saying there is no such row.
func TestBigtableConditionalDeleteRowAndCellsThroughTheBrowser(t *testing.T) {
	needService(t, "bigtable")
	project := uniqueProject(t)
	_, client, table := bigtableClients(t, project)
	p := open(t)
	q := "?project=" + project
	ctx := context.Background()
	bigtableWrite(t, client, table, "r1", map[string][]byte{"cf:a": []byte("1"), "cf:b": []byte("2"), "cf:c": []byte("3")})
	bigtableWrite(t, client, table, "r2", map[string][]byte{"cf:a": []byte("1")})

	p.navigate("/bigtable/" + table + "/r1" + q)
	p.cellMenu("cf:a", "Delete cells")
	p.confirmByName("cf:a")
	p.waitFor(`document.querySelector(".modal") === null`)
	if cells := bigtableLatest(t, client, table, "r1"); len(cells) != 2 || cells["cf:a"] != nil {
		t.Fatalf("after Delete cells on cf:a, the data client reads r1 %q, want cf:b and cf:c", cells)
	}

	p.waitFor(`document.querySelector('button[aria-label="Actions for cf:b"]') !== null`)
	del := bigtable.NewMutation()
	del.DeleteCellsInColumn("cf", "b")
	if err := client.Open(table).Apply(ctx, "r1", del); err != nil {
		t.Fatal(err)
	}
	p.cellMenu("cf:b", "Delete cells")
	p.confirmByName("cf:b")
	if refusal := p.modalRefusal(); !strings.Contains(refusal, "has no cells in cf:b") {
		t.Errorf("Delete cells on a column deleted since was answered %q, want a refusal naming it", refusal)
	}
	p.forgive()
	p.clickText(".modal button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if cells := bigtableLatest(t, client, table, "r1"); len(cells) != 1 || cells["cf:c"] == nil {
		t.Errorf("after a refused Delete cells, the data client reads r1 %q, want cf:c", cells)
	}

	p.navigate("/bigtable/" + table + "/r1" + q)
	p.clickText("#view .page-actions button", "Delete row")
	p.confirmByName("r1")
	p.waitFor(`document.querySelector(".modal") === null && location.pathname === "/bigtable/` + table + `"`)
	if keys := bigtableRowKeys(t, client, table); strings.Join(keys, ",") != "r2" {
		t.Errorf("after Delete row r1, ReadRows reads %v, want r2", keys)
	}

	p.navigate("/bigtable/" + table + "/r2" + q)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Delete row")`)
	gone := bigtable.NewMutation()
	gone.DeleteRow()
	if err := client.Open(table).Apply(ctx, "r2", gone); err != nil {
		t.Fatal(err)
	}
	p.clickText("#view .page-actions button", "Delete row")
	p.confirmByName("r2")
	if refusal := p.modalRefusal(); !strings.Contains(refusal, "no row with key r2") {
		t.Errorf("Delete row on a row deleted since was answered %q, want a refusal naming it", refusal)
	}
	p.forgive()
}

// TestFirestoreCountOnlyThroughTheBrowser (#1198). A collection holds three
// documents, ages 10, 20 and 30. In its Query tab, age >= 20 with Count only
// checked shows one row, count(*), with Firestore's count 2, where the same
// query unchecked lists the two documents; Count only with no filter counts
// all three, and the Limit of 1 does not apply to it.
func TestFirestoreCountOnlyThroughTheBrowser(t *testing.T) {
	needService(t, "firestore")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	for i, age := range []string{"10", "20", "30"} {
		if code, body := consoleDo(t, http.MethodPost, "/api/resources/firestore"+q,
			fmt.Sprintf(`{"collection":"people","documentId":"p%d","field":"age","type":"number","value":%q}`, i, age)); code != http.StatusOK {
			t.Fatalf("add a document through the console API = %d: %s", code, body)
		}
	}

	p.navigate("/firestore/people" + q)
	p.waitFor(`document.querySelector("#tab-query") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-query`)))
	p.waitFor(`document.querySelector(".query-form #f-count") !== null`)
	results := func() map[string]string {
		var rows map[string]string
		p.eval(`Object.fromEntries([...document.querySelectorAll(".query-results tbody tr")].map((r) => {
			const cells = [...r.querySelectorAll("td")].map((c) => c.textContent.trim()).filter((s) => s);
			return [cells[0], cells.slice(1).join(" ")];
		}))`, &rows)
		return rows
	}
	runQuery := func(wantRows int) map[string]string {
		p.eval(`(() => { document.querySelectorAll(".query-results").forEach((r) => r.replaceChildren()); return true; })()`, nil)
		p.run(chromedp.Click(chromedp.CSS(`.query-form button[type="submit"]`)))
		p.waitFor(fmt.Sprintf(`document.querySelectorAll(".query-results tbody tr").length === %d`, wantRows))
		return results()
	}

	p.setField(".query-form #f-field", "age")
	p.setField(".query-form #f-op", ">=")
	p.setField(".query-form #f-value", "20")
	if rows := runQuery(2); len(rows) != 2 {
		t.Errorf("age >= 20 lists %v, want p1 and p2", rows)
	}
	p.run(chromedp.Click(chromedp.CSS(`.query-form #f-count`)))
	if rows := runQuery(1); rows["count(*)"] != "2" {
		t.Errorf("age >= 20 with Count only shows %v, want count(*) 2", rows)
	}
	p.setField(".query-form #f-field", "")
	p.setField(".query-form #f-value", "")
	p.setField(".query-form #f-limit", "1")
	if rows := runQuery(1); rows["count(*)"] != "3" {
		t.Errorf("Count only with no filter and Limit 1 shows %v, want count(*) 3", rows)
	}
}
