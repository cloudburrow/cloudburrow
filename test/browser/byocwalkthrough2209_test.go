//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov/awsfake"
	"github.com/cloudburrow/cloudburrow/internal/byoc/azureprov/azurefake"
)

// The first-run walk-through of bring your own cloud (#2209): the whole
// no-DevOps flow, in the browser, on a real instance whose clouds are the
// local fakes (test/tools/byocfakes, CLOUDBURROW_BYOC_FAKES) and whose VMs
// boot as the Docker-in-Docker sshd stand-ins. Every click is the user's:
// connect AWS (IAM Identity Center's device sign-in) and Azure (Microsoft's
// sign-in), create a VM in each, move the instance onto the AWS VM, deploy a
// Cloud Run service there, move the instance from AWS to Azure with it,
// scale, stop and start the Azure VM, move the instance back to this
// machine, and delete both VMs, nothing left in either cloud. Nothing
// leaves the machine: the fakes and the stand-ins are local.
//
//	CLOUDBURROW_TEST_BYOC_WALKTHROUGH   the fakes file the instance runs with
//	CLOUDBURROW_TEST_BYOC_INSTANCE_DIR  the instance's state directory (the
//	                                    admin token is read from it after each
//	                                    restart, never printed)
//	CLOUDBURROW_TEST_REMOTE_STANDIN     the stand-in the AWS VM boots as
//	CLOUDBURROW_TEST_REMOTE_STANDIN_2   the stand-in the Azure VM boots as
//	CLOUDBURROW_TEST_CLUSTER            the instance's kind cluster
const (
	envWalkThrough     = "CLOUDBURROW_TEST_BYOC_WALKTHROUGH"
	envWalkInstanceDir = "CLOUDBURROW_TEST_BYOC_INSTANCE_DIR"
	envStandInA        = "CLOUDBURROW_TEST_REMOTE_STANDIN"
	envStandInB        = "CLOUDBURROW_TEST_REMOTE_STANDIN_2"
	envWalkCluster     = "CLOUDBURROW_TEST_CLUSTER"
)

// walk is the walk-through's state.
type walk struct {
	t                  *testing.T
	p                  *tab
	dir, cluster       string
	standA, standB     string
	fakes              map[string]string
	instance, vmA, vmB string
}

func (w *walk) token() string {
	w.t.Helper()
	b, err := os.ReadFile(filepath.Join(w.dir, "admin-token"))
	if err != nil {
		w.t.Fatalf("the instance's admin token: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// signIn gives Settings > Compute the instance's current admin token (a
// move restarts the instance, with a new one).
func (w *walk) signIn() {
	p := w.p
	p.navigate("/settings/compute")
	p.waitFor(`document.getElementById("compute-token") !== null || document.getElementById("compute-aws-card") !== null`)
	var asked bool
	p.eval(`document.getElementById("compute-token") !== null`, &asked)
	if asked {
		p.setField("#compute-token", w.token())
		p.eval(`document.getElementById("compute-token-form").requestSubmit(), true`, nil)
	}
	p.waitFor(`document.getElementById("compute-aws-card") !== null && document.getElementById("compute-azure-card") !== null`)
}

// fake calls the fakes' control endpoint: what the user does on AWS's or
// Microsoft's own page.
func (w *walk) fake(path string) {
	w.t.Helper()
	u := strings.TrimSuffix(w.fakes["open_url"], "/open") + path
	res, err := http.Post(u, "application/json", strings.NewReader("{}"))
	if err != nil {
		w.t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		w.t.Fatalf("%s: %s", path, res.Status)
	}
}

func (w *walk) create(conn, name string) {
	w.t.Helper()
	p := w.p
	p.navigate("/settings/compute/create")
	p.waitFor(`document.getElementById("compute-connection") !== null && document.querySelectorAll("#compute-sizes input[name=compute-size]").length === 4`)
	p.eval(fmt.Sprintf(`(() => { const s = document.getElementById("compute-connection"); s.value = %q; s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, conn), nil)
	p.waitFor(`document.getElementById("compute-permissions-ok") !== null && document.querySelectorAll("#compute-sizes input[name=compute-size]").length === 4`)
	var price string
	p.eval(`document.getElementById("compute-price-hourly").textContent`, &price)
	if !strings.Contains(price, "$") {
		w.t.Errorf("the wizard shows no hourly price before Create for %s: %q", conn, price)
	}
	p.setField("#compute-instance", name)
	p.clickExpr(`document.querySelector('#compute-sizes input[value="small"]')`)
	p.setField("#compute-autostop", "60")
	p.run(chromedp.Click(chromedp.CSS(`#compute-create-submit`)))
	p.waitForWithin(`location.pathname === "/settings/compute/operation" && (document.getElementById("compute-op-done") !== null || document.getElementById("compute-op-failed") !== null)`, 3*time.Minute)
	var failed, steps string
	p.eval(`(document.getElementById("compute-op-failed") || {}).textContent || ""`, &failed)
	if failed != "" {
		w.t.Fatalf("Create %s failed: %s", name, failed)
	}
	p.eval(`[...document.querySelectorAll("#compute-events li")].map((li) => li.dataset.step + ":" + li.dataset.status).join(",")`, &steps)
	for _, s := range []string{"permissions:done", "key:done", "vm", "remote:done"} {
		if !strings.Contains(steps, s) {
			w.t.Errorf("Create %s's progress lacks %s: %s", name, s, steps)
		}
	}
}

// moveStatus is the instance's move.json, as the console reads it.
type walkMove struct {
	To          string `json:"to"`
	Done        bool   `json:"done"`
	Error       string `json:"error"`
	Interrupted bool   `json:"interrupted"`
	Steps       []struct {
		Step string `json:"step"`
	} `json:"steps"`
}

func (w *walk) moveFile() walkMove {
	var st walkMove
	if b, err := os.ReadFile(filepath.Join(w.dir, "move.json")); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// awaitMove waits out a move the page started: the instance (and its
// console) stops, its state is carried, and it starts again; then the
// page, signed in again, shows the outcome.
func (w *walk) awaitMove(to, cardSteps string) {
	w.t.Helper()
	// The page shows the progress while the console is up.
	w.p.waitForWithin(fmt.Sprintf(`document.querySelectorAll("#%s li").length > 0`, cardSteps), 2*time.Minute)
	deadline := time.Now().Add(40 * time.Minute)
	var st walkMove
	for time.Now().Before(deadline) {
		if st = w.moveFile(); st.To == to && st.Done {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !st.Done || st.Error != "" || st.Interrupted {
		w.t.Fatalf("the move to %s: %+v", to, st)
	}
	var steps []string
	for _, s := range st.Steps {
		steps = append(steps, s.Step)
	}
	for _, want := range []string{"stop", "connect", "image", "start", "verify", "cleanup", "done"} {
		if !strings.Contains(strings.Join(steps, ","), want) {
			w.t.Errorf("the move to %s lacks step %s: %v", to, want, steps)
		}
	}
	// The console is back: its address answers.
	addr := strings.TrimPrefix(w.p.origin, "http://")
	for i := 0; i < 120; i++ {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			break
		}
		time.Sleep(time.Second)
	}
	w.signIn()
	w.p.navigate("/compute-capacity")
	where := "This machine's Docker"
	if to != "local" {
		where = "VM " + to
	}
	w.p.waitForWithin(fmt.Sprintf(`document.getElementById("capacity-instance-where") !== null && document.getElementById("capacity-instance-where").textContent === %q`, where), time.Minute)
}

// nodeOn reports whether the instance's kind node runs in the stand-in
// (or, with "", on this machine's Docker).
func (w *walk) nodeOn(standIn string) bool {
	args := []string{"inspect", "--format", "{{.State.Running}}", w.cluster + "-control-plane"}
	if standIn != "" {
		args = append([]string{"exec", standIn, "docker"}, args...)
	}
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST=", "DOCKER_CONTEXT=")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// revisionOn waits for a Cloud Run revision's container on the node in the
// stand-in.
func (w *walk) revisionOn(standIn string) bool {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		cmd := exec.Command("docker", "exec", standIn, "docker", "exec", w.cluster+"-control-plane", "crictl", "ps", "--name", "user-container", "-q")
		cmd.Env = append(os.Environ(), "DOCKER_HOST=", "DOCKER_CONTEXT=")
		if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
			return true
		}
		time.Sleep(5 * time.Second)
	}
	return false
}

func (w *walk) clickWhenEnabled(id string) {
	w.t.Helper()
	w.p.waitForWithin(fmt.Sprintf(`document.getElementById(%q) !== null && !document.getElementById(%q).disabled`, id, id), 2*time.Minute)
	w.p.run(chromedp.Click(chromedp.CSS("#" + id)))
}

func (w *walk) confirm() {
	w.t.Helper()
	w.p.waitFor(`document.querySelector(".modal button[type=submit]") !== null`)
	w.p.run(chromedp.Click(chromedp.CSS(`.modal button[type=submit]`)))
}

func (w *walk) waitState(vm, state string) {
	w.t.Helper()
	w.p.waitForWithin(fmt.Sprintf(`document.getElementById("capacity-%s-state") !== null && document.getElementById("capacity-%s-state").textContent === %q`, vm, vm, state), 3*time.Minute)
}

func TestByocFirstRunWalkThrough(t *testing.T) {
	fakesPath := os.Getenv(envWalkThrough)
	if fakesPath == "" {
		t.Skipf("%s names no fakes file: the walk-through needs an instance started with CLOUDBURROW_BYOC_FAKES and the stand-ins", envWalkThrough)
	}
	w := &walk{t: t, dir: os.Getenv(envWalkInstanceDir), cluster: os.Getenv(envWalkCluster), standA: os.Getenv(envStandInA), standB: os.Getenv(envStandInB)}
	if w.dir == "" || w.cluster == "" || w.standA == "" || w.standB == "" {
		t.Fatalf("%s, %s, %s and %s must all be set", envWalkInstanceDir, envWalkCluster, envStandInA, envStandInB)
	}
	b, err := os.ReadFile(fakesPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &w.fakes); err != nil {
		t.Fatal(err)
	}
	w.instance = filepath.Base(w.dir)
	w.vmA, w.vmB = w.instance+"-aws", w.instance+"-az"
	if !w.nodeOn("") {
		t.Fatalf("the instance's node %s-control-plane is not on this machine's Docker to start from", w.cluster)
	}
	w.p = openWithin(t, 150*time.Minute)
	p := w.p
	// The console restarts with each move; the page's requests during it
	// fail, as the page says.
	p.expected = append(p.expected, "Failed to fetch", "ERR_CONNECTION_REFUSED", "ERR_EMPTY_RESPONSE", "401", "503")
	t.Cleanup(p.forgive)

	// 1. Connect: AWS through IAM Identity Center's device sign-in, Azure
	// through Microsoft's sign-in, both in the browser.
	w.signIn()
	p.setField("#compute-aws-start-url", "https://d-1234567890.awsapps.com/start")
	p.setField("#compute-aws-sso-region", "us-east-1")
	p.run(chromedp.Click(chromedp.CSS(`#compute-aws-signin`)))
	p.waitFor(`document.getElementById("compute-aws-code") !== null && document.getElementById("compute-aws-code").textContent.length > 0`)
	w.fake("/approve") // the user confirms the code on AWS's page
	p.waitForWithin(`document.getElementById("compute-aws-choose-form") !== null`, time.Minute)
	p.run(chromedp.Click(chromedp.CSS(`#compute-aws-choose`)))
	p.waitFor(`document.querySelector('#compute-aws-connections [data-connection="aws-` + awsfake.DefaultAccount + `"]') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#compute-azure-signin`)))
	p.waitForWithin(`document.getElementById("compute-azure-choose-form") !== null`, time.Minute)
	p.clickExpr(`document.querySelector('#compute-azure-choose-form input[value="` + azurefake.Subscription2 + `"]')`)
	p.run(chromedp.Click(chromedp.CSS(`#compute-azure-choose`)))
	p.waitFor(`document.querySelector('#compute-azure-connections [data-connection="azure-` + azurefake.Subscription2 + `"]') !== null`)

	// 2. Create a VM in each cloud, the price shown first.
	w.create("aws-"+awsfake.DefaultAccount, w.vmA)
	w.create("azure-"+azurefake.Subscription2, w.vmB)

	// 3. Move the instance onto the AWS VM.
	p.navigate("/compute-capacity")
	p.waitFor(`document.getElementById("capacity-instance-where") !== null && document.getElementById("capacity-instance-where").textContent === "This machine's Docker"`)
	w.clickWhenEnabled("capacity-" + w.vmA + "-move")
	w.confirm()
	w.awaitMove(w.vmA, "capacity-"+w.vmA+"-move-steps")
	if !w.nodeOn(w.standA) || w.nodeOn("") {
		t.Fatal("after the move to the AWS VM the node is not on its stand-in alone")
	}

	// 4. A Cloud Run service, deployed from the console, runs on the VM.
	project := uniqueProject(t)
	service := fmt.Sprintf("walk-%d", time.Now().UnixNano()%1e6)
	p.navigate("/run/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-image") !== null && document.querySelector("#view #f-name") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`#view #f-image`), "ghcr.io/knative/helloworld-go:latest"),
		chromedp.SendKeys(chromedp.CSS(`#view #f-name`), service))
	p.clickText("#view .is-radio label", "Allow public access")
	p.clickText(`#view button[type="submit"]`, "Create")
	p.waitForWithin(`location.pathname === "/run"`, 6*time.Minute)
	p.waitForWithin(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, service), time.Minute)
	if !w.revisionOn(w.standA) {
		t.Fatal("the Cloud Run revision's container is not on the AWS VM's node")
	}

	// 5. From the AWS VM to the Azure VM: across clouds, the service with it.
	w.clickWhenEnabled("capacity-" + w.vmB + "-move")
	var detail string
	p.waitFor(`document.querySelector(".modal") !== null`)
	p.eval(`document.querySelector(".modal").textContent`, &detail)
	if !strings.Contains(detail, "stops on "+w.vmA) || !strings.Contains(detail, "Nothing is removed from "+w.vmA) {
		t.Errorf("the VM-to-VM move's dialog reads %q", detail)
	}
	w.confirm()
	w.awaitMove(w.vmB, "capacity-"+w.vmB+"-move-steps")
	if !w.nodeOn(w.standB) || w.nodeOn(w.standA) {
		t.Fatal("after the move to the Azure VM the node is not on its stand-in alone")
	}
	if !w.revisionOn(w.standB) {
		t.Fatal("the Cloud Run revision did not come back on the Azure VM's node")
	}
	p.navigate("/run?project=" + project)
	p.waitForWithin(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, service), time.Minute)

	// 6. Scale the Azure VM, the price difference shown; stop and start it.
	p.navigate("/compute-capacity")
	sizeID := "capacity-" + w.vmB + "-size"
	p.waitForWithin(fmt.Sprintf(`!document.getElementById(%q).disabled`, sizeID), time.Minute)
	p.run(chromedp.Focus(chromedp.CSS("#"+sizeID)), chromedp.KeyEvent(kb.ArrowRight))
	var value string
	p.eval(fmt.Sprintf(`document.getElementById(%q).textContent`, sizeID+"-value"), &value)
	if !strings.Contains(value, "Medium") || !strings.Contains(value, "+$") {
		t.Errorf("the slider at Medium reads %q; want the size and the price difference", value)
	}
	w.clickWhenEnabled("capacity-" + w.vmB + "-resize")
	p.waitForWithin(fmt.Sprintf(`document.getElementById("capacity-%s-type") && document.getElementById("capacity-%s-type").textContent === "Standard_D16s_v5"`, w.vmB, w.vmB), 3*time.Minute)
	w.clickWhenEnabled("capacity-" + w.vmB + "-stop")
	w.waitState(w.vmB, "Stopped")
	w.clickWhenEnabled("capacity-" + w.vmB + "-start")
	w.waitState(w.vmB, "Running")

	t.Run("WCAG", func(t *testing.T) {
		p.navigate("/compute-capacity")
		p.waitFor(`document.getElementById("capacity-instance-move-back") !== null`)
		for _, u := range p.unnamedControls() {
			t.Errorf("an unnamed %s", u)
		}
		for _, c := range p.contrastFailures() {
			t.Errorf("%s", c)
		}
		p.run(chromedp.Click(chromedp.CSS(`#capacity-instance-move-back`)))
		p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") !== null`)
		for _, u := range p.unnamedControls() {
			t.Errorf("the Move back dialog: an unnamed %s", u)
		}
		p.run(chromedp.KeyEvent(kb.Escape))
		p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") === null`)
	})
	t.Run("keyboard", func(t *testing.T) {
		p.run(reducedMotion())
		p.navigate("/compute-capacity")
		p.waitFor(`document.getElementById("capacity-instance-move-back") !== null`)
		for _, f := range p.keyboardFailures() {
			t.Errorf("Compute capacity: %s", f)
		}
		// Move back opens by keyboard with focus on Cancel.
		p.tabTo(`a.id === "capacity-instance-move-back"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") !== null && document.activeElement.textContent === "Cancel"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") === null`)
	})

	// 7. Back to this machine, the service with it.
	p.navigate("/compute-capacity")
	w.clickWhenEnabled("capacity-instance-move-back")
	w.confirm()
	w.awaitMove("local", "capacity-instance-move-steps")
	if !w.nodeOn("") || w.nodeOn(w.standB) {
		t.Fatal("after the move back the node is not on this machine's Docker alone")
	}

	// 8. Delete both VMs: nothing tagged is left in either cloud.
	for _, vm := range []string{w.vmA, w.vmB} {
		w.clickWhenEnabled("capacity-" + vm + "-delete")
		p.waitFor(`document.getElementById("confirm-input") !== null`)
		p.setField("#confirm-input", vm)
		w.confirm()
		p.waitForWithin(fmt.Sprintf(`document.getElementById("capacity-%s") === null`, vm), 3*time.Minute)
	}
	res, err := http.Get(w.fakes["live_url"])
	if err != nil {
		t.Fatal(err)
	}
	var live []string
	_ = json.NewDecoder(res.Body).Decode(&live)
	res.Body.Close()
	for _, l := range live {
		if strings.Contains(l, "instance") || strings.Contains(strings.ToLower(l), "virtualmachines") {
			t.Errorf("left in a cloud after Delete: %s", l)
		}
	}
}
