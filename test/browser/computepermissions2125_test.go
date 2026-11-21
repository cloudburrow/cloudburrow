//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestComputePermissionsThroughTheBrowser (#2125), in a shard whose
// instance serves the compute service: Google's Permissions panel on
// Compute Engine's resources. On VM instances, checking a row and clicking
// Permissions opens the info panel's Permissions tab, whose Add principal
// writes the instance's own allow policy; on Disks and Snapshots, checking a
// row opens the info panel, whose Permissions tab does the same; a subnet's
// page has a Permissions tab with Grant access. Each change is read back
// through the console's policy read, and each state meets WCAG.
func TestComputePermissionsThroughTheBrowser(t *testing.T) {
	needService(t, "compute-instances")
	needService(t, "vpc-networks")
	project := uniqueProject(t)
	q := "?project=" + project
	const zone, vm, disk, snap, net, sub = "us-central1-a", "perm-vm", "perm-disk", "perm-snap", "perm-net", "perm-sub"
	create := func(service string, values map[string]string) {
		t.Helper()
		b, _ := json.Marshal(values)
		if code, body := consoleDo(t, http.MethodPost, "/api/resources/"+service+q, string(b)); code != http.StatusOK {
			t.Fatalf("create %s %v = %d: %s", service, values, code, body)
		}
	}
	act := func(service string, path []string, action string, values map[string]string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
		if code, body := consoleDo(t, http.MethodPost, "/api/actions/"+service+q, string(b)); code != http.StatusOK {
			t.Fatalf("%s %s on %v = %d: %s", service, action, path, code, body)
		}
	}
	status := func(service, name string) string {
		code, body := consoleDo(t, http.MethodGet, "/api/resources/"+service+q, "")
		if code != http.StatusOK {
			return ""
		}
		var l struct {
			Items []struct{ Name, Status string }
		}
		_ = json.Unmarshal([]byte(body), &l)
		for _, i := range l.Items {
			if i.Name == name {
				return i.Status
			}
		}
		return ""
	}
	waitStatus := func(service, name, want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Minute)
		for status(service, name) != want {
			if time.Now().After(deadline) {
				t.Fatalf("%s %s never became %s", service, name, want)
			}
			time.Sleep(2 * time.Second)
		}
	}
	policy := func(service string, path []string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"Path": path, "Read": map[string]any{}})
		code, body := consoleDo(t, http.MethodPost, "/api/permissions/"+service+q, string(b))
		if code != http.StatusOK {
			t.Fatalf("read %s %v's policy = %d: %s", service, path, code, body)
		}
		return body
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{zone, vm}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instances"+q, string(b))
		consoleDo(t, http.MethodDelete, "/api/resources/compute-snapshots"+q+"&name="+snap, "")
		time.Sleep(5 * time.Second)
		b, _ = json.Marshal(map[string]any{"Path": []string{zone, disk}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-disks"+q, string(b))
	})

	create("compute-instances", map[string]string{"name": vm, "zone": zone, "machineType": "e2-small", "image": "debian-cloud/debian-12"})
	create("compute-disks", map[string]string{"name": disk, "zone": zone, "source": "Blank disk", "type": "pd-balanced", "sizeGb": "10"})
	waitStatus("compute-disks", disk, "READY")
	act("compute-disks", []string{zone, disk}, "snapshot", map[string]string{"name": snap})
	waitStatus("compute-snapshots", snap, "READY")
	create("vpc-networks", map[string]string{"name": net, "subnetMode": "Custom",
		"subnetName": sub, "subnetRegion": "us-central1", "subnetRange": "10.9.0.0/24"})

	p := open(t)
	wcag := func(what string) {
		for _, u := range p.unnamedControls() {
			t.Errorf("%s: an unnamed %s", what, u)
		}
		for _, f := range p.contrastFailures() {
			t.Errorf("%s: %s", what, f)
		}
	}
	// addPrincipal fills the open Add principal form and saves it.
	addPrincipal := func(member, role string) {
		p.waitFor(`document.querySelector(".modal.is-open #f-members") !== null`)
		setField(p, ".modal.is-open", "f-members", member)
		setField(p, ".modal.is-open", "f-role", role)
		// Names only: the dialog is still fading in, so its colours are
		// not yet its own; the panel behind it is checked once it closes.
		for _, u := range p.unnamedControls() {
			t.Errorf("Add principal: an unnamed %s", u)
		}
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
		p.waitFor(`document.querySelector(".modal.is-open") === null`)
	}

	// VM instances: check the row, then Permissions.
	p.navigate("/compute/instances" + q)
	p.waitFor(rowReady(vm))
	var disabled bool
	p.eval(`document.querySelector("#list-permissions").disabled`, &disabled)
	if !disabled {
		t.Error("Permissions is enabled with no instance checked")
	}
	p.run(chromedp.Click(chromedp.CSS(`#view input[aria-label="Select ` + vm + `"]`)))
	p.clickText("#view button", "Permissions")
	p.waitFor(`document.querySelector("#info-tab-permissions[aria-selected=true]") !== null && document.querySelector("#info-add-principal") !== null`)
	wcag("VM instances Permissions")
	p.run(chromedp.Click(chromedp.CSS(`#info-add-principal`)))
	addPrincipal("user:vm-admin@example.com", "roles/compute.instanceAdmin.v1")
	if got := policy("compute-instances", []string{zone, vm}); !strings.Contains(got, "user:vm-admin@example.com") {
		t.Errorf("after Add principal the instance's policy reads %s", got)
	}
	p.waitFor(`document.querySelector("#info-permissions") !== null && document.querySelector("#info-permissions").textContent.includes("vm-admin@example.com")`)

	// Disks and Snapshots: check the row, the info panel's Permissions tab.
	for _, c := range []struct {
		page, service, name, member, role string
		path                              []string
	}{
		{"/compute/disks", "compute-disks", disk, "user:disk@example.com", "roles/compute.storageAdmin", []string{zone, disk}},
		{"/compute/snapshots", "compute-snapshots", snap, "user:snap@example.com", "roles/compute.storageAdmin", []string{snap}},
	} {
		p.navigate(c.page + q)
		p.waitFor(rowReady(c.name))
		p.run(chromedp.Click(chromedp.CSS(`#view input[aria-label="Select ` + c.name + `"]`)))
		p.waitFor(`document.querySelector("#info-tab-permissions") !== null`)
		p.run(chromedp.Click(chromedp.CSS(`#info-tab-permissions`)))
		p.waitFor(`document.querySelector("#info-add-principal") !== null`)
		wcag(c.page + " info panel Permissions")
		p.run(chromedp.Click(chromedp.CSS(`#info-add-principal`)))
		addPrincipal(c.member, c.role)
		if got := policy(c.service, c.path); !strings.Contains(got, c.member) {
			t.Errorf("after Add principal %s's policy reads %s", c.name, got)
		}
		p.waitFor(fmt.Sprintf(`document.querySelector("#info-permissions") !== null && document.querySelector("#info-permissions").textContent.includes(%q)`,
			strings.TrimPrefix(c.member, "user:")))
		// Remove principal takes it out again.
		p.clickText("#info-permissions button", "Remove principal")
		p.waitFor(`document.querySelector(".modal.is-open") !== null`)
		p.eval(`(() => { const f = document.querySelector(".modal.is-open input[type=text]"); if (f) { f.value = `+
			fmt.Sprintf("%q", c.member)+`; f.dispatchEvent(new Event("input", { bubbles: true })); } return true; })()`, nil)
		p.clickText(".modal.is-open button", "Remove")
		p.waitFor(`document.querySelector(".modal.is-open") === null`)
		deadline := time.Now().Add(30 * time.Second)
		for strings.Contains(policy(c.service, c.path), c.member) {
			if time.Now().After(deadline) {
				t.Fatalf("Remove principal left %s on %s", c.member, c.name)
			}
			time.Sleep(time.Second)
		}
	}

	// A subnet's page: its Permissions tab.
	p.navigate("/networking/networks/list/" + url.PathEscape(net) + "/subnets/us-central1/" + url.PathEscape(sub) + q)
	p.clickText("#view .tab-strip .tab", "Permissions")
	p.waitFor(`document.querySelector("#permissions-note") !== null`)
	wcag("Subnet Permissions")
	p.clickText("#view #grant-access", "Grant access")
	addPrincipal("serviceAccount:builder@"+project+".iam.gserviceaccount.com", "roles/compute.networkUser")
	if got := policy("vpc-networks", []string{net, "subnets", "us-central1", sub}); !strings.Contains(got, "builder@") ||
		!strings.Contains(got, "roles/compute.networkUser") {
		t.Errorf("after Grant access the subnet's policy reads %s", got)
	}
}
