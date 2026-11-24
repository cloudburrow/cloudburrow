//go:build browser

package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov/awsfake"
	"github.com/cloudburrow/cloudburrow/internal/byoc/azureprov"
	"github.com/cloudburrow/cloudburrow/internal/byoc/azureprov/azurefake"
	"github.com/cloudburrow/cloudburrow/internal/byoc/keychain"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Bring your own cloud (#2209): Settings > Compute's Connect AWS and Connect
// Azure, the Create VM wizard with its price, the progress view, and
// Compute capacity's size slider, Start, Stop and Delete, on a console
// served in process. The clouds are the local fakes awsfake and azurefake,
// which speak AWS's and Azure's APIs; the keychain is in memory. Nothing
// leaves the machine.

const byocToken = "byoc-browser-test-token"

type byocConsole struct {
	p    *tab
	aws  *awsfake.Server
	az   *azurefake.Server
	keys *keychain.Memory
}

func openByoc(t *testing.T) *byocConsole {
	t.Helper()
	b := &byocConsole{aws: awsfake.New(), az: azurefake.New(), keys: keychain.NewMemory()}
	t.Cleanup(b.aws.Close)
	t.Cleanup(b.az.Close)
	var once sync.Mutex
	m, err := byoc.NewManager(byoc.Options{Dir: t.TempDir(), Keychain: b.keys,
		Drivers:  []byoc.Driver{awsprov.NewDriver(b.aws.Options()), azureprov.New(b.az.Options())},
		IPLookup: func(context.Context) (string, error) { return "203.0.113.77", nil },
		OpenBrowser: func(u string) error {
			once.Lock()
			defer once.Unlock()
			if strings.HasPrefix(u, b.az.URL()) {
				// Microsoft's page: the user signs in, the page redirects.
				go func() { _ = b.az.OpenBrowser(u) }()
			}
			return nil
		},
		Location: func() (float64, float64, bool) { return 47.6, -122.3, true }})
	if err != nil {
		t.Fatal(err)
	}
	s := console.New("", func(context.Context) console.Status { return console.Status{} })
	s.SetCompute(m, "byoc-demo", func(_ context.Context, authz string) error {
		if authz != "Bearer "+byocToken {
			return &console.InstanceRefusal{Status: http.StatusUnauthorized, Message: "missing or wrong admin token"}
		}
		return nil
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	b.p = openAt(t, strings.TrimPrefix(srv.URL, "http://"))
	t.Cleanup(b.p.forgive)
	return b
}

// signIn opens Settings > Compute and gives the admin token.
func (b *byocConsole) signIn() {
	p := b.p
	p.navigate("/settings/compute")
	p.waitFor(`document.getElementById("compute-token") !== null`)
	p.setField("#compute-token", byocToken)
	p.eval(`document.getElementById("compute-token-form").requestSubmit(), true`, nil)
	p.waitFor(`document.getElementById("compute-aws-card") !== null && document.getElementById("compute-azure-card") !== null`)
}

func (b *byocConsole) connectAWS() {
	p := b.p
	p.setField("#compute-aws-start-url", "https://d-1234567890.awsapps.com/start")
	p.setField("#compute-aws-sso-region", "us-east-1")
	p.run(chromedp.Click(chromedp.CSS(`#compute-aws-signin`)))
	p.waitFor(`document.getElementById("compute-aws-code") !== null && document.getElementById("compute-aws-code").textContent.length > 0`)
	b.aws.Approve()
	p.waitFor(`document.getElementById("compute-aws-choose-form") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#compute-aws-choose`)))
	p.waitFor(`document.querySelector('#compute-aws-connections [data-connection="aws-` + awsfake.DefaultAccount + `"]') !== null`)
}

func (b *byocConsole) connectAzure() {
	p := b.p
	p.run(chromedp.Click(chromedp.CSS(`#compute-azure-signin`)))
	p.waitFor(`document.getElementById("compute-azure-choose-form") !== null`)
	// The second subscription: the startup's credits.
	p.clickExpr(`document.querySelector('#compute-azure-choose-form input[value="` + azurefake.Subscription2 + `"]')`)
	p.run(chromedp.Click(chromedp.CSS(`#compute-azure-choose`)))
	p.waitFor(`document.querySelector('#compute-azure-connections [data-connection="azure-` + azurefake.Subscription2 + `"]') !== null`)
}

func (b *byocConsole) waitOp() (failed string) {
	p := b.p
	p.waitForWithin(`location.pathname === "/settings/compute/operation" && (document.getElementById("compute-op-done") !== null || document.getElementById("compute-op-failed") !== null)`, time.Minute)
	p.eval(`(document.getElementById("compute-op-failed") || {}).textContent || ""`, &failed)
	return failed
}

func TestByocConnectCreateScaleDelete(t *testing.T) {
	b := openByoc(t)
	p := b.p
	b.signIn()
	var none bool
	p.eval(`document.getElementById("compute-create").disabled`, &none)
	if !none {
		t.Error("Create VM is enabled with no cloud connected")
	}
	b.connectAWS()
	b.connectAzure()
	if n := b.keys.Names(); len(n) != 2 {
		t.Fatalf("keychain items %v", n)
	}

	// Create VM: region (the closest first), size presets with their
	// prices, auto-stop, the permission check and the Monthly estimate.
	p.run(chromedp.Click(chromedp.CSS(`#compute-create`)))
	p.waitFor(`location.pathname === "/settings/compute/create"`)
	p.waitFor(`document.querySelectorAll("#compute-sizes input[name=compute-size]").length === 4 && document.getElementById("compute-permissions-ok") !== null`)
	var form struct {
		Region, Monthly, Hourly, Sizes, Instance string
	}
	p.eval(`({ Region: document.getElementById("compute-region").value,
		Monthly: document.getElementById("compute-price-monthly").textContent,
		Hourly: document.getElementById("compute-price-hourly").textContent,
		Sizes: [...document.querySelectorAll("#compute-sizes .compute-size")].map((n) => n.textContent).join("|"),
		Instance: document.getElementById("compute-instance").value })`, &form)
	if form.Region != "us-west-2" || !strings.HasPrefix(form.Monthly, "$") || !strings.Contains(form.Hourly, "hourly") ||
		!strings.Contains(form.Sizes, "m7i.xlarge") || !strings.Contains(form.Sizes, "g6.xlarge") || !strings.Contains(form.Sizes, "NVIDIA L4") || form.Instance != "byoc-demo" {
		t.Fatalf("the wizard reads %+v", form)
	}
	p.clickExpr(`document.querySelector('#compute-sizes input[value="medium"]')`)
	var monthly string
	p.eval(`document.getElementById("compute-price-monthly").textContent`, &monthly)
	if monthly == form.Monthly {
		t.Errorf("the estimate did not follow the size: %s", monthly)
	}
	p.setField("#compute-autostop", "30")
	p.run(chromedp.Click(chromedp.CSS(`#compute-create-submit`)))
	if failed := b.waitOp(); failed != "" {
		t.Fatalf("create failed: %s", failed)
	}
	var steps string
	p.eval(`[...document.querySelectorAll("#compute-events li")].map((li) => li.dataset.step + ":" + li.dataset.status).join(",")`, &steps)
	for _, s := range []string{"permissions:done", "address:done", "key:done", "network", "vm", "boot"} {
		if !strings.Contains(steps, s) {
			t.Errorf("progress lacks %s: %s", s, steps)
		}
	}

	// Compute capacity: the slider to Large, by keyboard, then Resize.
	p.run(chromedp.Click(chromedp.CSS(`#compute-op-capacity`)))
	p.waitFor(`location.pathname === "/compute-capacity" && document.getElementById("capacity-byoc-demo") !== null`)
	p.waitFor(`!document.getElementById("capacity-byoc-demo-size").disabled`)
	var card struct{ Type, Price, State, Value string }
	p.eval(`({ Type: document.getElementById("capacity-byoc-demo-type").textContent, Price: document.getElementById("capacity-byoc-demo-price").textContent,
		State: document.getElementById("capacity-byoc-demo-state").textContent, Value: document.getElementById("capacity-byoc-demo-size-value").textContent })`, &card)
	if card.Type != "m7i.4xlarge" || !strings.Contains(card.Price, "/hour") || card.State != "Running" || !strings.Contains(card.Value, "Medium") {
		t.Fatalf("the capacity card reads %+v", card)
	}
	p.run(chromedp.Focus(chromedp.CSS(`#capacity-byoc-demo-size`)), chromedp.KeyEvent(kb.ArrowRight))
	p.waitFor(`document.getElementById("capacity-byoc-demo-size-value").textContent.includes("m7i.8xlarge") && !document.getElementById("capacity-byoc-demo-resize").disabled`)
	p.run(chromedp.Click(chromedp.CSS(`#capacity-byoc-demo-resize`)))
	p.waitForWithin(`document.getElementById("capacity-byoc-demo-type") && document.getElementById("capacity-byoc-demo-type").textContent === "m7i.8xlarge"`, time.Minute)

	// Stop, then Start.
	p.waitFor(`!document.getElementById("capacity-byoc-demo-stop").disabled`)
	p.run(chromedp.Click(chromedp.CSS(`#capacity-byoc-demo-stop`)))
	p.waitForWithin(`document.getElementById("capacity-byoc-demo-state") && document.getElementById("capacity-byoc-demo-state").textContent === "Stopped"`, time.Minute)
	p.run(chromedp.Click(chromedp.CSS(`#capacity-byoc-demo-start`)))
	p.waitForWithin(`document.getElementById("capacity-byoc-demo-state") && document.getElementById("capacity-byoc-demo-state").textContent === "Running"`, time.Minute)

	// Resources lists what carries the tag.
	p.run(chromedp.Click(chromedp.CSS(`#capacity-byoc-demo-resources-button`)))
	p.waitFor(`document.querySelectorAll("#capacity-byoc-demo-resources tbody tr").length >= 5`)

	t.Run("WCAG", func(t *testing.T) {
		for _, path := range []string{"/compute-capacity", "/settings/compute", "/settings/compute/create"} {
			p.navigate(path)
			p.waitFor(`document.querySelector("#view h1") !== null && document.querySelector("#view .skeleton") === null`)
			if path == "/settings/compute/create" {
				p.waitFor(`document.querySelectorAll("#compute-sizes input").length === 4`)
			}
			if path == "/compute-capacity" {
				p.waitFor(`document.getElementById("capacity-byoc-demo-size") !== null && !document.getElementById("capacity-byoc-demo-size").disabled`)
			}
			for _, u := range p.unnamedControls() {
				t.Errorf("%s: an unnamed %s", path, u)
			}
			for _, c := range p.contrastFailures() {
				t.Errorf("%s: %s", path, c)
			}
		}
	})

	t.Run("keyboard", func(t *testing.T) {
		p.run(reducedMotion())
		p.navigate("/compute-capacity")
		p.waitFor(`document.getElementById("capacity-byoc-demo-size") !== null && !document.getElementById("capacity-byoc-demo-size").disabled`)
		for _, f := range p.keyboardFailures() {
			t.Errorf("Compute capacity: %s", f)
		}
		// The slider moves by keyboard alone, and Resize follows it.
		p.navigate("/compute-capacity")
		p.waitFor(`document.getElementById("capacity-byoc-demo-size") !== null && !document.getElementById("capacity-byoc-demo-size").disabled`)
		p.tabTo(`a.id === "capacity-byoc-demo-size"`)
		p.run(chromedp.KeyEvent(kb.ArrowLeft))
		p.waitFor(`document.getElementById("capacity-byoc-demo-size-value").textContent.includes("Medium") && !document.getElementById("capacity-byoc-demo-resize").disabled`)
		p.navigate("/settings/compute")
		p.waitFor(`document.getElementById("compute-aws-card") !== null`)
		for _, f := range p.keyboardFailures() {
			t.Errorf("Settings > Compute: %s", f)
		}
		// The access-key form opens from its button, by keyboard.
		p.tabTo(`a.id === "compute-aws-key-toggle"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.activeElement.id === "compute-aws-key-id"`)
	})

	// Delete: typed confirmation, then everything tagged is gone.
	p.navigate("/compute-capacity")
	p.waitFor(`document.getElementById("capacity-byoc-demo-delete") !== null && !document.getElementById("capacity-byoc-demo-delete").disabled`)
	p.run(chromedp.Click(chromedp.CSS(`#capacity-byoc-demo-delete`)))
	p.confirmByName("byoc-demo")
	p.waitForWithin(`location.pathname === "/compute-capacity" && document.getElementById("capacity-byoc-demo") === null && document.querySelector("#view .state") !== null`, time.Minute)
	if live := b.aws.Live(); len(live) != 0 {
		t.Fatalf("Delete left %+v", live)
	}

	// Disconnect removes the keychain item.
	p.navigate("/settings/compute")
	p.waitFor(`document.querySelector('#compute-aws-connections [data-connection="aws-` + awsfake.DefaultAccount + `"] button') !== null`)
	p.clickExpr(`document.querySelector('#compute-aws-connections [data-connection="aws-` + awsfake.DefaultAccount + `"] button')`)
	p.confirmByName(awsfake.DefaultAccount)
	p.waitFor(`document.getElementById("compute-aws-none") !== null`)
	if n := b.keys.Names(); len(n) != 1 || !strings.HasPrefix(n[0], "connection/azure-") {
		t.Fatalf("keychain items after Disconnect %v", n)
	}
}

func TestByocFailuresAreShownInPlainWords(t *testing.T) {
	b := openByoc(t)
	p := b.p
	b.signIn()
	b.connectAzure()

	// A missing permission: the wizard says which, before Create.
	b.az.Deny("Microsoft.Network/networkSecurityGroups/write")
	p.navigate("/settings/compute/create")
	p.waitFor(`document.getElementById("compute-permissions-missing") !== null`)
	var missing string
	p.eval(`document.getElementById("compute-permissions-missing").textContent`, &missing)
	if !strings.Contains(missing, "Microsoft.Network/networkSecurityGroups/write") {
		t.Fatalf("the missing permission reads %q", missing)
	}

	// Quota exceeded on the VM: the progress view says so, and that
	// nothing was left behind.
	b2 := openByoc(t)
	b2.signIn()
	b2.connectAzure()
	b2.az.FailNext("Microsoft.Compute/virtualMachines/write", 409, "OperationNotAllowed",
		"Operation could not be completed as it results in exceeding approved standardDSv5Family Cores quota.")
	p = b2.p
	p.navigate("/settings/compute/create")
	p.waitFor(`document.getElementById("compute-permissions-ok") !== null && !document.getElementById("compute-create-submit").disabled`)
	p.run(chromedp.Click(chromedp.CSS(`#compute-create-submit`)))
	failed := b2.waitOp()
	if !strings.Contains(failed, "quota") || !strings.Contains(failed, "Nothing was left behind") {
		t.Fatalf("the failure reads %q", failed)
	}
	var rolled bool
	p.eval(`[...document.querySelectorAll("#compute-events li")].some((li) => li.dataset.status === "rolled-back")`, &rolled)
	if !rolled {
		t.Error("the progress view shows no rollback")
	}
	if ids := b2.az.IDs(); len(ids) != 0 {
		t.Fatalf("the failed create left %v", ids)
	}

	t.Run("WCAG", func(t *testing.T) {
		for _, u := range p.unnamedControls() {
			t.Errorf("the progress view: an unnamed %s", u)
		}
		for _, c := range p.contrastFailures() {
			t.Errorf("the progress view: %s", c)
		}
	})
}

// TestByocSettingsLinks: Settings and utilities reaches both pages.
func TestByocSettingsLinks(t *testing.T) {
	b := openByoc(t)
	p := b.p
	p.navigate("/")
	p.run(chromedp.Click(chromedp.CSS(`#settings`)))
	p.waitFor(`document.getElementById("settings-panel").classList.contains("is-open") && document.getElementById("settings-compute-link").getClientRects().length > 0`)
	p.run(chromedp.Click(chromedp.CSS(`#settings-compute-link`)))
	p.waitFor(`location.pathname === "/settings/compute" && document.getElementById("compute-token") !== null`)
	// The panel closes as the page opens (its exit animation aside).
	p.waitFor(`!document.getElementById("settings-panel").classList.contains("is-open")`)
	p.waitFor(`document.getElementById("settings").getAttribute("aria-expanded") === "false"`)
}
