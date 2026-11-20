//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// secretsSDK is the official Secret Manager client at the instance's
// endpoint, or a skip when the run gave none.
func secretsSDK(t *testing.T) *secretmanager.Client {
	t.Helper()
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envSecrets)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the secrets are read back through the Secret Manager API itself", envSecrets)
	}
	c, err := secretmanager.NewClient(context.Background(), option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestSecretManagerConsoleParity (#1850), in the run shard, which serves
// Secret Manager: the product's menu is Secret Manager and Settings, the
// regional secrets a tab of its page; Create secret is one page in Google's
// sections, whose Secret value is typed or read from Upload file, Set rotation period reveals the
// Rotation period list and Custom its own period, and Version aliases name
// version 1, which the official client then reaches by its alias; the list
// has Google's columns, and a row's Actions menu holds Add new version,
// whose Disable all past versions disables version 1, and Edit; the secret's
// tabs are Overview, Versions, Permissions and Logs, and a version row's
// View secret value shows its value; Create regional secret makes a
// regional secret, listed on Regional secrets only.
func TestSecretManagerConsoleParity(t *testing.T) {
	needService(t, "secrets")
	c := secretsSDK(t)
	ctx := context.Background()
	p := open(t)
	project := uniqueProject(t)
	id := "browser-parity"
	name := "projects/" + project + "/secrets/" + id
	t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &smpb.DeleteSecretRequest{Name: name}) })

	p.navigate("/secrets?project=" + project)
	p.waitFor(`document.querySelectorAll("#product-nav a").length >= 2 && document.querySelector("#view .page-tabs") !== null`)
	var menu string
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim()).join("|")`, &menu)
	if !strings.Contains(menu, "Secret Manager|Settings") {
		t.Errorf("Secret Manager's menu = %q; want Secret Manager, Settings", menu)
	}
	// The regional secrets are a tab of the Secret Manager page (#1850).
	var pageTabs string
	p.eval(`[...document.querySelectorAll("#view .page-tabs .tab")].map((a) => a.textContent.trim()).join("|")`, &pageTabs)
	if pageTabs != "Global secrets|Regional secrets" {
		t.Errorf("Secret Manager's page tabs = %q; want Global secrets, Regional secrets", pageTabs)
	}

	// Create secret: one page, Google's sections, Upload file.
	p.navigate("/secrets/create?project=" + project)
	p.waitFor(`document.querySelector("#view #up-payload") !== null`)
	var sections string
	p.eval(`[...document.querySelectorAll("#view h2, #view h3, #view legend")].map((h) => h.textContent.trim()).join("|")`, &sections)
	for _, s := range []string{"Replication policy", "Encryption", "Rotation", "Expiration", "Labels", "Annotations", "Version aliases", "Delayed destruction"} {
		if !strings.Contains(sections, s) {
			t.Errorf("Create secret has no %q section: %s", s, sections)
		}
	}
	file := filepath.Join(t.TempDir(), "value.txt")
	if err := os.WriteFile(file, []byte("from a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.run(chromedp.SetUploadFiles(chromedp.CSS(`#view #up-payload`), []string{file}))
	p.waitFor(`document.querySelector("#view #f-payload").value === "from a file"`)
	setField(p, "#view", "f-secretId", id)
	var hidden bool
	p.eval(`document.querySelector("#view #f-rotationPreset").closest(".form-row").hidden`, &hidden)
	if !hidden {
		t.Error("the Rotation period list shows before Set rotation period is checked")
	}
	setField(p, "#view", "f-setRotation", "true")
	p.waitFor(`!document.querySelector("#view #f-rotationPreset").closest(".form-row").hidden && ` +
		`document.querySelector("#view #f-rotationPeriod").closest(".form-row").hidden`)
	setField(p, "#view", "f-rotationPreset", "Custom")
	p.waitFor(`!document.querySelector("#view #f-rotationPeriod").closest(".form-row").hidden`)
	setField(p, "#view", "f-setRotation", "false")
	setField(p, "#view", "f-labels", "env=prod")
	setField(p, "#view", "f-versionAliases", "stable=1")
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(`location.pathname === "/secrets"`)

	got, err := c.GetSecret(ctx, &smpb.GetSecretRequest{Name: name})
	if err != nil {
		t.Fatalf("GetSecret of the secret made in the browser: %v", err)
	}
	if got.GetVersionAliases()["stable"] != 1 || got.GetLabels()["env"] != "prod" || got.GetRotation() != nil {
		t.Errorf("GetSecret = aliases %v, labels %v, rotation %v", got.GetVersionAliases(), got.GetLabels(), got.GetRotation())
	}
	if a, err := c.AccessSecretVersion(ctx, &smpb.AccessSecretVersionRequest{Name: name + "/versions/stable"}); err != nil ||
		string(a.GetPayload().GetData()) != "from a file" {
		t.Errorf("AccessSecretVersion(stable) = %v, %v; want the uploaded file", a, err)
	}

	// The list: Google's columns, and the row's Actions.
	p.waitFor(rowReady(name))
	var heads string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).join("|")`, &heads)
	for _, h := range []string{"Location", "Encryption", "Labels", "Created", "Expiration"} {
		if !strings.Contains(heads, h) {
			t.Errorf("the list has no %s column: %s", h, heads)
		}
	}
	menuBtn := fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+name)
	p.run(chromedp.Click(chromedp.CSS(menuBtn)))
	var items string
	p.eval(`[...document.querySelectorAll('.overflow-menu:not([hidden]) [role="menuitem"]')].map((i) => i.textContent.trim()).join("|")`, &items)
	if !strings.Contains(items, "Add new version") || !strings.Contains(items, "Edit") {
		t.Errorf("the row's Actions menu = %q; want Add new version and Edit", items)
	}
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Add new version")
	p.waitFor(`document.querySelector(".modal.is-open #f-payload") !== null && document.querySelector(".modal.is-open #up-payload") !== null`)
	setField(p, ".modal.is-open", "f-payload", "second")
	setField(p, ".modal.is-open", "f-disablePast", "true")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if v, err := c.GetSecretVersion(ctx, &smpb.GetSecretVersionRequest{Name: name + "/versions/1"}); err != nil ||
		v.GetState() != smpb.SecretVersion_DISABLED {
		t.Errorf("version 1 after Disable all past versions = %v, %v", v.GetState(), err)
	}

	// The secret's page: Overview, Versions, Permissions, Logs.
	p.clickText("#view tbody a", name)
	p.waitFor(`document.querySelectorAll("#view .tab-strip .tab").length >= 4`)
	var tabs string
	p.eval(`[...document.querySelectorAll("#view .tab-strip .tab")].map((t) => t.textContent.trim()).join("|")`, &tabs)
	if !strings.HasPrefix(tabs, "Overview|Versions|Permissions|Logs") {
		t.Errorf("the secret's tabs = %q; want Overview, Versions, Permissions, Logs", tabs)
	}
	p.clickText("#view .tab-strip .tab", "Versions")
	p.waitFor(rowReady("2"))
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for 2"))))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "View secret value")
	p.waitFor(`document.querySelector(".modal.is-open .reveal-value") !== null`)
	var value string
	p.eval(`document.querySelector(".modal.is-open .reveal-value").textContent`, &value)
	if value != "second" {
		t.Errorf("View secret value showed %q, want second", value)
	}
	p.clickText(".modal.is-open .modal-actions button", "Done")

	// Settings says what it is.
	p.navigate("/secrets-settings?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Settings")`)

	// Create regional secret, on Regional secrets only.
	rid := "browser-parity-regional"
	rname := "projects/" + project + "/locations/us-central1/secrets/" + rid
	t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &smpb.DeleteSecretRequest{Name: rname}) })
	p.navigate("/secrets-regional/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-location") !== null`)
	setField(p, "#view", "f-secretId", rid)
	setField(p, "#view", "f-location", "us-central1")
	setField(p, "#view", "f-payload", "regional")
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(`location.pathname === "/secrets-regional"`)
	if _, err := c.GetSecret(ctx, &smpb.GetSecretRequest{Name: rname}); err != nil {
		t.Errorf("GetSecret of the regional secret made in the browser: %v", err)
	}
	p.waitFor(rowReady(rname))
	code, body := consoleDo(t, http.MethodGet, "/api/resources/secrets?project="+project, "")
	if code != http.StatusOK || strings.Contains(body, rname) {
		t.Errorf("Secret Manager's list = %d and holds the regional secret: %s", code, body)
	}
}
