//go:build browser

package browser

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// envRemoteCompute names the compute host of an instance started with `up
// --compute` (#2209), as ssh://user@host:port; the test skips without it.
const envRemoteCompute = "CLOUDBURROW_TEST_REMOTE_COMPUTE"

// TestRemoteComputeConsole (#2209): on an instance whose compute runs on a
// remote host, the Instance page's Compute card says so: the host, its
// size and system, a live connection; Reconnect, with the admin token,
// drops the SSH connection and shows the new one. WCAG: every control is
// named and every text meets contrast; keyboard: Reconnect is reached by
// Tab and pressed with Enter.
func TestRemoteComputeConsole(t *testing.T) {
	target := strings.TrimSpace(os.Getenv(envRemoteCompute))
	if target == "" {
		t.Skipf("%s is not set: the instance's compute is this machine's Docker", envRemoteCompute)
	}
	token := strings.TrimSpace(os.Getenv(envAdminToken))
	if token == "" {
		t.Fatalf("%s is not set; Reconnect needs the instance's admin token", envAdminToken)
	}
	code, body := consoleDo(t, http.MethodGet, "/api/instance/compute", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/instance/compute = %d: %s", code, body)
	}
	var view struct {
		Location   string
		Target     string
		Connected  bool
		Reconnects int
		Size       string
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatal(err)
	}
	if view.Location != "ssh" || view.Target != target || !view.Connected || view.Size == "" {
		t.Fatalf("the console's compute = %+v, want a connected ssh host %s with its size", view, target)
	}
	// Reconnect is the admin API's to allow: without the token it is
	// refused, and nothing is dropped.
	if code, _ := consoleDo(t, http.MethodPost, "/api/instance/compute/reconnect", ""); code != http.StatusUnauthorized {
		t.Fatalf("a reconnect without the admin token = %d, want 401", code)
	}

	p := open(t)
	p.navigate("/instance")
	p.waitFor(`document.querySelector("#instance-token-form #instance-token") !== null`)
	p.forgive() // the page's first read is refused with 401, on purpose
	p.run(chromedp.SendKeys(chromedp.CSS(`#instance-token`), token), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector("#instance-compute-target") !== null`)
	text := func(id string) string {
		var s string
		p.eval(`(document.getElementById("`+id+`") || {}).textContent || ""`, &s)
		return s
	}
	if got := text("instance-compute-location"); got != "Remote host over SSH" {
		t.Errorf("Location = %q", got)
	}
	if got := text("instance-compute-target"); got != target {
		t.Errorf("Runs on = %q, want %q", got, target)
	}
	if got := text("instance-compute-size"); !strings.Contains(got, "CPUs") || !strings.Contains(got, "GiB memory") {
		t.Errorf("Size = %q, want the host's CPUs and memory", got)
	}
	if got := text("instance-compute-system"); !strings.HasPrefix(got, "Linux") || !strings.Contains(got, "Docker ") {
		t.Errorf("System = %q, want Linux and its Docker version", got)
	}
	if got := text("instance-compute-state"); got != "Connected" {
		t.Errorf("Connection = %q", got)
	}
	if got := text("instance-compute-ports"); got == "none" || got == "" {
		t.Errorf("Forwarded ports = %q, want at least the API server's and the ingress", got)
	}
	before := text("instance-compute-reconnects")

	p.clickExpr(`document.querySelector("#instance-compute-reconnect")`)
	p.waitForWithin(`(document.getElementById("instance-compute-result") || {}).textContent === "Reconnected."`, 2*time.Minute)
	if got := text("instance-compute-reconnects"); got == before {
		t.Errorf("Reconnects stayed %q after Reconnect", got)
	}
	if got := text("instance-compute-state"); got != "Connected" {
		t.Errorf("Connection after Reconnect = %q", got)
	}

	t.Run("WCAG", func(t *testing.T) {
		for _, u := range p.unnamedControls() {
			t.Errorf("the Instance page: an unnamed %s", u)
		}
		for _, c := range p.contrastFailures() {
			t.Errorf("the Instance page: %s", c)
		}
		var labelled bool
		p.eval(`(() => { const c = document.getElementById("instance-compute-card");
			const h = document.getElementById(c.getAttribute("aria-labelledby")); return !!h && h.textContent === "Compute"; })()`, &labelled)
		if !labelled {
			t.Error("the Compute card is not labelled by its heading")
		}
	})

	t.Run("keyboard", func(t *testing.T) {
		mid := text("instance-compute-reconnects")
		p.tabTo(`a.id === "instance-compute-reconnect"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitForWithin(`(document.getElementById("instance-compute-reconnects") || {}).textContent !== "`+mid+`"`, 2*time.Minute)
	})
}
