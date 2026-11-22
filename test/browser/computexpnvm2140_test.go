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
)

// TestCreateInstanceOnSharedSubnetThroughTheBrowser2140 (#2140), in the
// compute shard: in a Shared VPC service project, Create an instance's
// Networking offers "Networks shared with me (from host project: H)"
// (vpc/docs/provisioning-shared-vpc "Create an instance in a service
// project"); an instance made on the host's default subnet of us-central1
// is counted by the host's Attached projects as a resource on its shared
// subnets, then deleted from the row's menu.
func TestCreateInstanceOnSharedSubnetThroughTheBrowser2140(t *testing.T) {
	needService(t, "compute-instances")
	host, service, act := sharedVPCProjects(t)
	const vm = "browser-shared-vm"
	t.Cleanup(func() {
		act(service, "compute-instances", "delete", []string{"us-central1-a", vm}, nil)
		time.Sleep(5 * time.Second)
	})

	p := open(t)
	p.navigate("/compute/instances?project=" + url.QueryEscape(service))
	p.clickText("#view button", "Create instance")
	p.waitFor(`document.querySelector("#f-networkSource") !== null`)
	var label string
	p.eval(`document.querySelector('label[for="f-networkSource-1"]').textContent`, &label)
	if want := "Networks shared with me (from host project: " + host + ")"; label != want {
		t.Fatalf("Network interfaces' second choice = %q, want %q", label, want)
	}
	p.clickExpr(`document.querySelector("#f-networkSource-1")`)
	p.waitFor(`!document.querySelector("#f-sharedSubnetwork").closest(".form-row").hidden && document.querySelector("#f-network").disabled`)
	p.submitForm(map[string]string{"name": vm, "machineType": "e2-small", "image": "debian-cloud/debian-12",
		"sharedSubnetwork": "us-central1/default", "externalIP": "None"}, "name")
	p.waitFor(`location.pathname === "/compute/instances" && [...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("` + vm + `"))`)

	deadline := time.Now().Add(5 * time.Minute)
	for {
		_, body := consoleDo(t, http.MethodGet, "/api/resources/vpc-sharedvpc?project="+url.QueryEscape(host), "")
		if strings.Contains(body, `"Resources on shared subnets":"1"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the host's Attached projects never counted the instance: %s", body)
		}
		time.Sleep(2 * time.Second)
	}
	vmRowMenu(p, vm, "Delete", map[string]string{"noGracefulShutdown": "false"}, "")
}

// sharedVPCProjects registers a host and a service project in the
// organization (Shared VPC is for an organization's projects), sets up
// Shared VPC on the host with the service project attached through the
// console API, and undoes it all when the test ends (after the test's own
// cleanups). act performs a console action.
func sharedVPCProjects(t *testing.T) (host, service string, act func(project, screen, action string, path []string, values map[string]string) (int, string)) {
	t.Helper()
	n := time.Now().UnixNano() % 1e9
	host, service = fmt.Sprintf("cb-bxh-%d", n), fmt.Sprintf("cb-bxs-%d", n)
	act = func(project, screen, action string, path []string, values map[string]string) (int, string) {
		b, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
		return consoleDo(t, http.MethodPost, "/api/actions/"+screen+"?project="+url.QueryEscape(project), string(b))
	}
	for _, id := range []string{host, service} {
		if code, body := consoleDo(t, http.MethodPost, "/api/resources/projects", `{"projectId":"`+id+`","parent":"organizations/100000000001"}`); code != http.StatusOK {
			t.Fatalf("register project %s in the organization = %d: %s", id, code, body)
		}
	}
	t.Cleanup(func() {
		act(host, "vpc-sharedvpc", "detach", []string{service}, nil)
		act(host, "vpc-sharedvpc", "disable", []string{}, nil)
		for _, id := range []string{service, host} {
			consoleDo(t, http.MethodDelete, "/api/resources/projects?name="+id, "")
		}
	})
	if code, body := act(host, "vpc-sharedvpc", "setup", []string{}, map[string]string{"sharing": "all", "projects": service}); code != http.StatusOK {
		t.Fatalf("Set up Shared VPC through the console API = %d: %s", code, body)
	}
	return host, service, act
}

// TestCreateInstanceInServiceProjectMeetsWCAG2140 (#2140): Create an
// instance in a Shared VPC service project, with its Network interfaces
// choice, meets WCAG (contrast in both themes, keyboard, names, motion,
// reflow).
func TestCreateInstanceInServiceProjectMeetsWCAG2140(t *testing.T) {
	needService(t, "compute-instances")
	_, service, _ := sharedVPCProjects(t)
	auditScreens(t, []a11yPage{screen("create instance in a service project", "/compute/instances/create", service)})
}
