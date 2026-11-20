//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"github.com/chromedp/chromedp"
)

// TestBigtableRowKeySchema1900 (#1900). Create table's Define row key
// schema reveals the Encoding, the Delimiter and a row per segment; the
// table it creates carries the schema the admin client reads, Bigtable
// Studio's Query editor reads the key by its segments, and Edit table
// deletes the schema.
func TestBigtableRowKeySchema1900(t *testing.T) {
	needService(t, "bigtable-instances")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envBigtable)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the admin API itself", envBigtable)
	}
	t.Setenv("BIGTABLE_EMULATOR_HOST", addr)
	ctx := context.Background()
	project := uniqueProject(t)
	iac, err := bigtable.NewInstanceAdminClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.Close() })
	id := fmt.Sprintf("br-rks-%d", time.Now().UnixNano()%1e8)
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "Row keys",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(context.Background(), id) })
	admin, err := bigtable.NewAdminClient(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	p := open(t)
	q := "?project=" + project
	p.navigate("/bigtable-instances/" + id + q)
	p.waitFor(`document.querySelector("#tab-tables") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-tables`)))
	p.clickText("#view #section-actions-tables button", "Create table")
	p.waitFor(`document.querySelector(".modal.is-open #f-table") !== null`)
	// The schema's fields are hidden until Define row key schema is checked.
	p.waitFor(`document.querySelector(".modal.is-open #f-rowKeyEncoding").closest("[hidden]") !== null`)
	setField(p, ".modal.is-open", "f-table", "sales")
	setField(p, ".modal.is-open", "f-families", `[{"family":"product","type":"Standard"}]`)
	setField(p, ".modal.is-open", "f-rowKeySchema", "true")
	p.waitFor(`document.querySelector(".modal.is-open #f-rowKeyEncoding").closest("[hidden]") === null &&
		document.querySelector(".modal.is-open #f-rowKeyDelimiter").value === "#"`)
	// Segments: user_id BYTES and purchase_date STRING.
	p.clickText(".modal.is-open button", "Add row key segment")
	p.eval(`(() => {
		const rows = [...document.querySelector(".modal.is-open #f-rowKeySegments").closest("fieldset").querySelectorAll(".group-row")];
		const set = (r, name, type) => {
			const [seg] = r.querySelectorAll("input[type=text]");
			const [typ] = r.querySelectorAll("select");
			seg.value = name; seg.dispatchEvent(new Event("input"));
			typ.value = type; typ.dispatchEvent(new Event("change"));
		};
		set(rows[0], "user_id", "BYTES");
		set(rows[1], "purchase_date", "STRING");
		return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	info, err := admin.TableInfo(ctx, "sales")
	if err != nil || info.RowKeySchema == nil || len(info.RowKeySchema.Fields) != 2 ||
		info.RowKeySchema.Fields[0].FieldName != "user_id" || info.RowKeySchema.Fields[1].FieldName != "purchase_date" {
		t.Fatalf("after Create table, TableInfo = %+v, %v", info, err)
	}
	if enc, ok := info.RowKeySchema.Encoding.(bigtable.StructDelimitedBytesEncoding); !ok || string(enc.Delimiter) != "#" {
		t.Errorf("the schema's encoding = %#v, want DelimitedBytes #", info.RowKeySchema.Encoding)
	}

	client, err := bigtable.NewClient(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	for _, k := range []string{"user1#2025-05-20", "user2#2025-05-24"} {
		m := bigtable.NewMutation()
		m.Set("product", "product_name", bigtable.Now(), []byte("p-"+k))
		if err := client.Open("sales").Apply(ctx, k, m); err != nil {
			t.Fatalf("Apply to %s: %v", k, err)
		}
	}
	p.navigate("/bigtable-instances/" + id + q)
	p.waitFor(`document.querySelector("#tab-studio") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-studio`)))
	// The Studio draws its mode tabs again once its Explorer has loaded
	// (loadExplorer's loadTab), so a Query editor button found before then
	// is gone by the click ("Could not find node with given id"): wait for
	// the table in the builder's list, then click the button drawn after.
	p.waitFor(`[...document.querySelectorAll("#bigtable-builder-table option")].some((o) => o.value === "sales")`)
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-mode-editor`)))
	p.waitFor(`document.querySelector("#bigtable-editor") !== null`)
	setField(p, "#view", "bigtable-editor", `SELECT purchase_date FROM sales WHERE user_id = b"user2"`)
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-editor-run`)))
	p.waitFor(`[...document.querySelectorAll("#bigtable-results-table td")].some((c) => c.textContent === "2025-05-24")`)

	// Edit table deletes the schema.
	p.navigate("/bigtable-instances/" + id + q)
	p.waitFor(`document.querySelector("#tab-tables") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-tables`)))
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for sales"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Edit")
	p.waitFor(`document.querySelector(".modal.is-open #f-deleteRowKeySchema") !== null &&
		document.querySelector(".modal.is-open").textContent.includes("user_id BYTES, purchase_date STRING")`)
	setField(p, ".modal.is-open", "f-deleteRowKeySchema", "true")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if info, err := admin.TableInfo(ctx, "sales"); err != nil || info.RowKeySchema != nil {
		t.Errorf("after Edit table's Delete row key schema, TableInfo = %+v, %v", info, err)
	}
}
