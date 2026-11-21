//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/chromedp/chromedp"
)

// TestSecretManagerConsoleRound3 (#1850), in the run shard, which serves
// Secret Manager beside Pub/Sub and Cloud KMS: Create secret's Secret type
// (Preview) and its Starting on date and time picker, both read back
// through the official client; the Overview's Secret type; Edit secret as
// a page ending in Update secret; the Versions tab's checkboxes with
// Disable, Enable and Destroy, confirmed as Google's dialogs are (Disable
// selected versions with nothing typed, Destroy selected versions with the
// secret ID typed back); a row's View secret value shown in a dialog with
// Done on the secret's page; and Create regional secret in a multi-region,
// on the Secret Manager page's Regional secrets tab.
func TestSecretManagerConsoleRound3(t *testing.T) {
	needService(t, "secrets")
	needService(t, "pubsub")
	c := secretsSDK(t)
	ctx := context.Background()
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	topic := "projects/" + project + "/topics/browser-r3"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/pubsub"+q, `{"name":"browser-r3"}`); code != http.StatusOK {
		t.Fatalf("create a topic through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/pubsub"+q+"&name="+url.QueryEscape(topic), "") })
	id := "browser-r3"
	name := "projects/" + project + "/secrets/" + id
	t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &smpb.DeleteSecretRequest{Name: name}) })

	// Create secret: Secret type, then a rotation Starting on, picked.
	p.navigate("/secrets/create" + q)
	p.waitFor(`document.querySelector("#view #f-secretType") !== null`)
	var types string
	p.eval(`[...document.querySelector("#view #f-secretType").options].map((o) => o.textContent).join("|")`, &types)
	if types != "None|Other DB credentials|Access key|Certificate|Other" {
		t.Errorf("Secret type offers %q", types)
	}
	setField(p, "#view", "f-secretId", id)
	setField(p, "#view", "f-secretType", "CERTIFICATE")
	setField(p, "#view", "f-payload", "first")
	setField(p, "#view", "f-topics", topic)
	setField(p, "#view", "f-setRotation", "true")
	setField(p, "#view", "f-rotationPreset", "90 days")
	var picker struct{ Type, Local, Instant string }
	p.eval(`(() => {
		const f = document.querySelector("#view #f-nextRotationTime");
		const d = new Date(Date.now() + 3 * 24 * 3600 * 1000); d.setSeconds(0, 0);
		const pad = (n) => String(n).padStart(2, "0");
		const local = d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + "T" + pad(d.getHours()) + ":" + pad(d.getMinutes());
		f.value = local; f.dispatchEvent(new Event("input", { bubbles: true })); f.dispatchEvent(new Event("change", { bubbles: true }));
		return { Type: f.type, Local: local, Instant: d.toISOString() };
	})()`, &picker)
	if picker.Type != "datetime-local" {
		t.Errorf("Starting on is an input of type %q; want a date and time picker", picker.Type)
	}
	p.clickText(`#view button[type="submit"]`, "Create secret")
	p.waitFor(`location.pathname === "/secrets"`)
	got, err := c.GetSecret(ctx, &smpb.GetSecretRequest{Name: name})
	if err != nil {
		t.Fatalf("GetSecret of the secret made in the browser: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, picker.Instant)
	if got.GetSecretType() != smpb.Secret_CERTIFICATE || !got.GetRotation().GetNextRotationTime().AsTime().Equal(want) ||
		got.GetRotation().GetRotationPeriod().AsDuration() != 90*24*time.Hour {
		t.Errorf("GetSecret = type %v, rotation %v; want CERTIFICATE, next %s every 90 days", got.GetSecretType(), got.GetRotation(), want)
	}
	for _, v := range []string{"second", "third"} {
		if _, err := c.AddSecretVersion(ctx, &smpb.AddSecretVersionRequest{Parent: name,
			Payload: &smpb.SecretPayload{Data: []byte(v)}}); err != nil {
			t.Fatal(err)
		}
	}

	// The Overview names the type; Edit secret is a page.
	p.navigate("/secrets/" + id + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("Certificate")`)
	p.clickText("#view .page-actions button", "Edit secret")
	p.waitFor(`location.search.includes("edit=1") && document.querySelector("#view form.create-page #f-labels") !== null`)
	var edit struct{ Heading, Local, Submit string }
	p.eval(`({ Heading: document.querySelector("#view h1").textContent.trim(),
		Local: document.querySelector("#view #f-nextRotationTime").value,
		Submit: document.querySelector('#view form.create-page button[type="submit"]').textContent.trim() })`, &edit)
	if edit.Heading != "Edit secret" || edit.Local != picker.Local || edit.Submit != "Update secret" {
		t.Errorf("Edit secret page = %+v; want Edit secret, Starting on %s, Update secret", edit, picker.Local)
	}
	setField(p, "#view", "f-labels", "team=core")
	p.clickText(`#view form.create-page button[type="submit"]`, "Update secret")
	p.waitFor(`!location.search.includes("edit=1") && document.querySelector("#view .tab-strip") !== null`)
	if got, err := c.GetSecret(ctx, &smpb.GetSecretRequest{Name: name}); err != nil || got.GetLabels()["team"] != "core" ||
		!got.GetRotation().GetNextRotationTime().AsTime().Equal(want) {
		t.Errorf("after Update secret: labels %v, rotation %v, %v", got.GetLabels(), got.GetRotation(), err)
	}

	// Versions: check 1 and 2, Disable, confirmed with Disable selected versions.
	p.clickText("#view .tab-strip .tab", "Versions")
	p.waitFor(rowReady("3"))
	check := func(n string) {
		p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`#view input[aria-label=%q]`, "Select "+n))))
	}
	check("1")
	check("2")
	p.clickText("#view button.secondary", "Disable")
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm]") !== null`)
	var dlg struct{ Title, Button string }
	p.eval(`({ Title: document.querySelector(".modal.is-open h2").textContent.trim(),
		Button: document.querySelector('.modal.is-open button[type="submit"]').textContent.trim() })`, &dlg)
	if dlg.Button != "Disable selected versions" {
		t.Errorf("Disable's confirmation = %+v; want its button Disable selected versions", dlg)
	}
	p.clickText(".modal.is-open button", "Disable selected versions")
	p.waitFor(`document.querySelector(".modal") === null`)
	for _, n := range []string{"1", "2"} {
		if v, err := c.GetSecretVersion(ctx, &smpb.GetSecretVersionRequest{Name: name + "/versions/" + n}); err != nil ||
			v.GetState() != smpb.SecretVersion_DISABLED {
			t.Errorf("version %s after Disable selected versions = %v, %v", n, v.GetState(), err)
		}
	}

	// Destroy version 2 from its row: the secret ID typed back.
	p.waitFor(rowReady("2"))
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for 2"))))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Destroy")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	var asks string
	p.eval(`document.querySelector('.modal.is-open label[for="confirm-input"] strong').textContent`, &asks)
	if asks != id {
		t.Errorf("Destroy asks for %q to be typed; want the secret ID %s", asks, id)
	}
	setField(p, ".modal.is-open", "confirm-input", id)
	p.clickText(".modal.is-open button", "Destroy selected versions")
	p.waitFor(`document.querySelector(".modal") === null`)
	if v, err := c.GetSecretVersion(ctx, &smpb.GetSecretVersionRequest{Name: name + "/versions/2"}); err != nil ||
		v.GetState() != smpb.SecretVersion_DESTROYED {
		t.Errorf("version 2 after Destroy selected versions = %v, %v", v.GetState(), err)
	}

	// View secret value in a dialog, on the secret's page.
	p.waitFor(rowReady("3"))
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for 3"))))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "View secret value")
	p.waitFor(`document.querySelector(".modal.is-open .reveal-value") !== null`)
	var shown struct{ Value, Path string }
	p.eval(`({ Value: document.querySelector(".modal.is-open .reveal-value").textContent, Path: location.pathname })`, &shown)
	if shown.Value != "third" || shown.Path != "/secrets/"+id {
		t.Errorf("View secret value showed %q at %s; want third, on the secret's page", shown.Value, shown.Path)
	}
	p.clickText(".modal.is-open .modal-actions button", "Done")
	p.waitFor(`document.querySelector(".modal") === null`)

	// Create regional secret in a multi-region, from the Regional secrets tab.
	rid := "browser-r3-multi"
	rname := "projects/" + project + "/locations/eu/secrets/" + rid
	t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &smpb.DeleteSecretRequest{Name: rname}) })
	p.navigate("/secrets" + q)
	p.clickText("#view .page-tabs .tab", "Regional secrets")
	p.waitFor(`location.pathname === "/secrets-regional"`)
	p.navigate("/secrets-regional/create" + q)
	p.waitFor(`document.querySelector("#view #f-locationType") !== null`)
	var regionalTypes string
	p.eval(`[...document.querySelector("#view #f-secretType").options].map((o) => o.textContent).join("|")`, &regionalTypes)
	if !strings.HasPrefix(regionalTypes, "None|Cloud SQL DB credentials|") {
		t.Errorf("Create regional secret's Secret type offers %q", regionalTypes)
	}
	setField(p, "#view", "f-secretId", rid)
	setField(p, "#view", "f-payload", "regional")
	setField(p, "#view", "f-locationType", "multi-region")
	p.waitFor(`!document.querySelector("#view #f-multiRegion").closest(".form-row").hidden && ` +
		`document.querySelector("#view #f-location").closest(".form-row").hidden`)
	setField(p, "#view", "f-multiRegion", "eu")
	p.clickText(`#view button[type="submit"]`, "Create regional secret")
	p.waitFor(`location.pathname === "/secrets-regional"`)
	if _, err := c.GetSecret(ctx, &smpb.GetSecretRequest{Name: rname}); err != nil {
		t.Errorf("GetSecret of the multi-region secret made in the browser: %v", err)
	}
}

// TestSecretManagerRound3ScreensMeetWCAG (#1850): the Secret Manager page's
// Global and Regional secrets tabs, Create secret and Create regional
// secret with their Secret type and Location, the secret's Versions tab
// with its checkboxes, the Disable selected versions and Destroy selected
// versions confirmations, the View secret value dialog, and the Edit secret
// page pass the console's WCAG 2.1 AA audit.
func TestSecretManagerRound3ScreensMeetWCAG(t *testing.T) {
	needService(t, "secrets")
	project := uniqueProject(t)
	fixture(t, "secrets", project, `{"secretId":"a11y-r3","payload":"x"}`, "a11y-r3")
	q := "?project=" + url.QueryEscape(project)
	versions := "/secrets/a11y-r3" + q + "&tab=versions"
	openRow := `document.querySelector('#view button[aria-label="Actions for 1"]').click();`
	pages := []a11yPage{
		{"global secrets tab", "/secrets" + q, rowReady("a11y-r3"), ""},
		{"regional secrets tab", "/secrets-regional" + q, settled, ""},
		{"create secret page", "/secrets/create" + q, `document.querySelector("#view #f-secretType") !== null`, ""},
		{"create regional secret page", "/secrets-regional/create" + q, `document.querySelector("#view #f-locationType") !== null`, ""},
		{"versions tab", versions, rowReady("1"), ""},
		{"edit secret page", "/secrets/a11y-r3" + q + "&edit=1", `document.querySelector("#view form.create-page #f-labels") !== null`, ""},
		{"disable selected versions dialog", versions, rowReady("1"),
			`document.querySelector('#view input[aria-label="Select 1"]').click();
			[...document.querySelectorAll("#view button.secondary")].find((b) => b.textContent.trim() === "Disable").click()`},
		{"destroy selected versions dialog", versions, rowReady("1"), openRow +
			`[...document.querySelectorAll(".overflow-menu:not([hidden]) [role=menuitem]")].find((b) => b.textContent.trim() === "Destroy").click()`},
		{"view secret value dialog", versions, rowReady("1"), openRow +
			`[...document.querySelectorAll(".overflow-menu:not([hidden]) [role=menuitem]")].find((b) => b.textContent.trim() === "View secret value").click()`},
	}
	auditScreens(t, pages)
}
