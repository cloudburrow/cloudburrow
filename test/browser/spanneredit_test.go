//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	database "cloud.google.com/go/spanner/admin/database/apiv1"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestSpannerEditInstanceAndDropProtectionThroughThePage (#1461), in the
// emulators shard. An instance's page has Edit instance, prefilled with its
// display name and processing units; saved with new ones, the official
// admin client's GetInstance reads them, and a capacity Google refuses is
// refused on the form with the API's message. A database's page has Drop
// protection, unchecked on a new database; checked and saved, GetDatabase
// reads enable_drop_protection on, and cleared again, off.
func TestSpannerEditInstanceAndDropProtectionThroughThePage(t *testing.T) {
	needService(t, "spanner")
	addr := strings.TrimSpace(os.Getenv(envSpanner))
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the Admin API itself", envSpanner)
	}
	ctx := context.Background()
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	instAdmin, err := instance.NewInstanceAdminClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instAdmin.Close() })
	dbAdmin, err := database.NewDatabaseAdminClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbAdmin.Close() })

	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	inst := fmt.Sprintf("browser-edit-%d", time.Now().UnixNano()%1e8)
	const db = "editdb"
	body, _ := json.Marshal(map[string]string{"instance": inst, "database": db,
		"ddl": "CREATE TABLE T (Id INT64 NOT NULL) PRIMARY KEY (Id)"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/spanner"+q, string(body)); code != http.StatusOK {
		t.Fatalf("create a Spanner database through the console API = %d: %s", code, out)
	}
	instName := "projects/" + project + "/instances/" + inst
	dbName := instName + "/databases/" + db
	t.Cleanup(func() {
		// A failed run can leave the database protected, which refuses the
		// instance's deletion.
		if op, err := dbAdmin.UpdateDatabase(context.Background(), &databasepb.UpdateDatabaseRequest{
			Database:   &databasepb.Database{Name: dbName},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"enable_drop_protection"}}}); err == nil {
			_, _ = op.Wait(context.Background())
		}
		b, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "dropinstance"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(b))
	})
	before, err := instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: instName})
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}

	// Edit instance, prefilled from the instance.
	p.navigate("/spanner" + q)
	p.clickText("#view tbody a", inst)
	openEdit := func() {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", "Edit instance")
		p.waitFor(`document.querySelector(".modal.is-open #f-processingUnits") !== null`)
	}
	openEdit()
	var form struct{ DisplayName, ProcessingUnits string }
	p.eval(`({ DisplayName: document.querySelector(".modal.is-open #f-displayName").value,
		ProcessingUnits: document.querySelector(".modal.is-open #f-processingUnits").value })`, &form)
	if form.DisplayName != before.GetDisplayName() || form.ProcessingUnits != fmt.Sprint(before.GetProcessingUnits()) {
		t.Errorf("Edit instance opens with %+v; want the instance's display name %q and %d processing units",
			form, before.GetDisplayName(), before.GetProcessingUnits())
	}
	p.setField(".modal.is-open #f-displayName", "edited in browser")
	p.setField(".modal.is-open #f-processingUnits", "300")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	got, err := instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: instName})
	if err != nil || got.GetDisplayName() != "edited in browser" || got.GetProcessingUnits() != 300 {
		t.Errorf("after Edit instance, GetInstance = %v, %v; want display name %q and 300 processing units", got, err, "edited in browser")
	}

	// 1500 is not a multiple of 1000: the API's refusal is shown on the form.
	openEdit()
	p.setField(".modal.is-open #f-processingUnits", "1500")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	if refusal := p.modalRefusal(); !strings.Contains(refusal, "processing_units") {
		t.Errorf("1500 processing units is refused with %q; want the API's message about processing_units", refusal)
	}
	p.forgive() // the refusal's 400, provoked on purpose
	p.clickText(".modal.is-open .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)

	// Drop protection on the database's page.
	setProtection := func(want bool) {
		t.Helper()
		p.navigate("/spanner" + q)
		p.clickText("#view tbody a", inst)
		p.clickText("#view tbody a", db)
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", "Drop protection")
		p.waitFor(`document.querySelector(".modal.is-open #f-enabled") !== null`)
		var checked bool
		p.eval(`document.querySelector(".modal.is-open #f-enabled").checked`, &checked)
		if checked == want {
			t.Fatalf("Drop protection opens with the box checked=%v, before setting it to %v", checked, want)
		}
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-enabled`)))
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
		p.waitFor(`document.querySelector(".modal") === null`)
		d, err := dbAdmin.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: dbName})
		if err != nil || d.GetEnableDropProtection() != want {
			t.Fatalf("after Drop protection set to %v, GetDatabase = %v, %v", want, d, err)
		}
	}
	setProtection(true)
	setProtection(false)
}

// TestSpannerCreateInstanceThroughTheListScreen (#1491), in the emulators
// shard. An instance was made only as a side effect of Create database:
// Create instance on the Spanner list screen makes one the official admin
// client's GetInstance reads with the form's display name and processing
// units, and the list shows it.
func TestSpannerCreateInstanceThroughTheListScreen(t *testing.T) {
	needService(t, "spanner")
	addr := strings.TrimSpace(os.Getenv(envSpanner))
	if addr == "" {
		t.Skipf("%s is not set: the instance is read back through the Admin API itself", envSpanner)
	}
	ctx := context.Background()
	instAdmin, err := instance.NewInstanceAdminClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instAdmin.Close() })
	project := uniqueProject(t)
	q := "?project=" + project
	inst := fmt.Sprintf("browser-new-%d", time.Now().UnixNano()%1e8)
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "dropinstance"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(b))
	})

	// Google's Create instance page (#1859): Select an edition, Name your
	// instance (the ID made from the name), Configure your instance,
	// Allocate compute capacity, Backups, ending in Create.
	p := open(t)
	p.navigate("/spanner" + q)
	p.clickText("#view button", "Create instance")
	p.waitFor(`location.pathname === "/spanner/create" && document.querySelector("#view #f-displayName") !== null`)
	var steps []string
	p.eval(`[...document.querySelectorAll(".create-step-title")].map((s) => s.textContent)`, &steps)
	want := []string{"Select an edition", "Name your instance", "Configure your instance", "Allocate compute capacity", "Backups"}
	if len(steps) < len(want) {
		t.Fatalf("the Create instance page's steps are %v; want %v first", steps, want)
	}
	for i, w := range want {
		if !strings.Contains(steps[i], w) {
			t.Errorf("step %d is %q; want %q", i+1, steps[i], w)
		}
	}
	var disabled []string
	p.eval(`[...document.querySelectorAll("#view input[type=radio]:disabled, #view input[type=checkbox]:disabled")].map((r) => r.value || r.id)`, &disabled)
	for _, w := range []string{"Dual-region"} {
		if !slices.Contains(disabled, w) {
			t.Errorf("%s is not drawn disabled (not available locally); disabled are %v", w, disabled)
		}
	}
	// Compare editions opens Google's comparison, and Continue folds a step
	// and opens the next.
	p.run(chromedp.Click(chromedp.CSS(`#view #f-edition-dialog`)))
	p.waitFor(`(document.querySelector(".modal.is-open .field-dialog-table") || {}).textContent?.includes("Enterprise Plus")`)
	var rows int
	p.eval(`document.querySelectorAll(".modal.is-open .field-dialog-table tbody tr").length`, &rows)
	if rows < 8 {
		t.Errorf("Compare editions has %d rows; want Google's comparison (8 and Locally)", rows)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open .modal-actions button`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.eval(`(() => { document.querySelector("#view .create-step .create-step-continue button").click(); return true })()`, new(bool))
	p.waitFor(`document.querySelector("#view .create-step").classList.contains("is-folded") && document.activeElement && document.activeElement.id === "f-displayName"`)
	setField(p, "#view", "f-displayName", "Made in browser")
	setField(p, "#view", "f-instance", inst)
	p.run(chromedp.Click(chromedp.CSS(`#view #f-unit-1`)))
	setField(p, "#view", "f-processingUnits", "300")
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname !== "/spanner/create"`)
	got, err := instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: "projects/" + project + "/instances/" + inst})
	if err != nil || got.GetDisplayName() != "Made in browser" || got.GetProcessingUnits() != 300 ||
		got.GetConfig() != "projects/"+project+"/instanceConfigs/regional-us-central1" {
		t.Fatalf("GetInstance after Create instance = %v, %v; want the page's name, 300 processing units and regional-us-central1", got, err)
	}
	p.navigate("/spanner" + q)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) && r.textContent.includes("300 processing units"))`, inst))
}
