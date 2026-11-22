package compute

import (
	"context"
	"strings"
	"testing"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// TestSharedVPCThroughTheGoClient (#2140): a host project, two service
// projects attached, one refused for being outside the organization and
// one for having another host already; getXpnHost, getXpnResources and
// listXpnHosts; a service project's VM on the host's subnet, on the host
// network's segment and firewall rules; a VM of an unattached project
// refused there; detaching refused while the VM uses the subnet, and
// disabling the host refused while projects are attached.
func TestSharedVPCThroughTheGoClient(t *testing.T) {
	ctx := context.Background()
	orgs := map[string]string{"host": "organizations/1", "svc": "organizations/1", "svc2": "organizations/1", "host2": "organizations/1",
		"loner": "", "stranger": "organizations/1"}
	s, opts := newTestServer(t, Options{VMs: fakeVMs{}, ProjectOrganization: func(p string) (string, bool) {
		o, ok := orgs[p]
		return o, ok
	}})
	projects, err := computeapi.NewProjectsRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	wait := func(op *computeapi.Operation, err error) error {
		t.Helper()
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	enableHost := func(p string) error {
		return wait(projects.EnableXpnHost(ctx, &computepb.EnableXpnHostProjectRequest{Project: p}))
	}
	attach := func(host, svc string) error {
		return wait(projects.EnableXpnResource(ctx, &computepb.EnableXpnResourceProjectRequest{Project: host,
			ProjectsEnableXpnResourceRequestResource: &computepb.ProjectsEnableXpnResourceRequest{XpnResource: &computepb.XpnResourceId{
				Id: proto.String(svc), Type: proto.String("PROJECT")}}}))
	}
	detach := func(host, svc string) error {
		return wait(projects.DisableXpnResource(ctx, &computepb.DisableXpnResourceProjectRequest{Project: host,
			ProjectsDisableXpnResourceRequestResource: &computepb.ProjectsDisableXpnResourceRequest{XpnResource: &computepb.XpnResourceId{
				Id: proto.String(svc), Type: proto.String("PROJECT")}}}))
	}
	if err := attach("host", "svc"); code(err) != 400 {
		t.Fatalf("attach before the host is enabled: %v", err)
	}
	if err := enableHost("loner"); code(err) != 400 || !strings.Contains(err.Error(), "organization") {
		t.Fatalf("a host outside an organization: %v", err)
	}
	if err := enableHost("host"); err != nil {
		t.Fatal(err)
	}
	if p, err := projects.Get(ctx, &computepb.GetProjectRequest{Project: "host"}); err != nil || p.GetXpnProjectStatus() != "HOST" {
		t.Fatalf("host project: %v %v", p.GetXpnProjectStatus(), err)
	}
	for _, svc := range []string{"svc", "svc2"} {
		if err := attach("host", svc); err != nil {
			t.Fatal(err)
		}
	}
	if err := attach("host", "loner"); code(err) != 400 {
		t.Fatalf("a service project outside the organization: %v", err)
	}
	if err := attach("host", "host"); code(err) != 400 {
		t.Fatalf("the host as its own service project: %v", err)
	}
	if err := enableHost("host2"); err != nil {
		t.Fatal(err)
	}
	if err := attach("host2", "svc"); code(err) != 400 || !strings.Contains(err.Error(), "one host project") {
		t.Fatalf("a second host: %v", err)
	}
	if err := attach("host2", "host"); code(err) != 400 {
		t.Fatalf("a host as a service project: %v", err)
	}
	if err := enableHost("svc"); code(err) != 400 {
		t.Fatalf("a service project as a host: %v", err)
	}
	if h, err := projects.GetXpnHost(ctx, &computepb.GetXpnHostProjectRequest{Project: "svc"}); err != nil || h.GetName() != "host" {
		t.Fatalf("getXpnHost: %v %v", h, err)
	}
	if h, err := projects.GetXpnHost(ctx, &computepb.GetXpnHostProjectRequest{Project: "stranger"}); err != nil || h.GetName() != "" {
		t.Fatalf("getXpnHost of an unattached project: %v %v", h, err)
	}
	var ids []string
	it := projects.GetXpnResources(ctx, &computepb.GetXpnResourcesProjectsRequest{Project: "host"})
	for {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.GetId()+":"+r.GetType())
	}
	if strings.Join(ids, ",") != "svc:PROJECT,svc2:PROJECT" {
		t.Fatalf("getXpnResources = %v", ids)
	}
	var hosts []string
	hit := projects.ListXpnHosts(ctx, &computepb.ListXpnHostsProjectsRequest{Project: "svc",
		ProjectsListXpnHostsRequestResource: &computepb.ProjectsListXpnHostsRequest{Organization: proto.String("organizations/1")}})
	for {
		p, err := hit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		hosts = append(hosts, p.GetName())
	}
	if strings.Join(hosts, ",") != "host,host2" {
		t.Fatalf("listXpnHosts = %v", hosts)
	}

	// A service project's VM on the host's subnet.
	z := "zones/us-central1-a/"
	vm := func(project, name string) error {
		_, err := s.Call("POST", project, z+"instances", nil, obj{"name": name, "machineType": z + "machineTypes/e2-small",
			"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
			"networkInterfaces": []any{obj{"subnetwork": "projects/host/regions/us-central1/subnetworks/default"}}})
		return err
	}
	if err := vm("svc", "shared-vm"); err != nil {
		t.Fatal(err)
	}
	inst, _ := s.Call("GET", "svc", z+"instances/shared-vm", nil, nil)
	nic := inst["networkInterfaces"].([]any)[0].(map[string]any)
	if nic["network"] != "projects/host/global/networks/default" || !strings.HasPrefix(str(nic, "networkIP"), "10.128.0.") {
		t.Fatalf("shared VM's interface = %v", nic)
	}
	if err := vm("stranger", "x"); err == nil || !strings.Contains(err.Error(), "Cross project referencing") {
		t.Fatalf("an unattached project's VM on the host subnet: %v", err)
	}
	// The VM is on the host network's segment, under the host's rules.
	s.mu.Lock()
	text, ok := s.vpcConfigLocked(s.res["projects/svc/zones/us-central1-a/instances/shared-vm"], "10.244.0.9")
	s.mu.Unlock()
	if !ok || !strings.Contains(text, "default-allow-internal") && !strings.Contains(text, "implied deny ingress") {
		t.Fatalf("shared VM's network config: %q", text)
	}
	if err := detach("host", "svc"); code(err) != 400 || !strings.Contains(err.Error(), "shared-vm") {
		t.Fatalf("detach while a VM uses the subnet: %v", err)
	}
	if err := wait(projects.DisableXpnHost(ctx, &computepb.DisableXpnHostProjectRequest{Project: "host"})); code(err) != 400 {
		t.Fatalf("disable a host with service projects: %v", err)
	}
	if err := detach("host", "svc2"); err != nil {
		t.Fatal(err)
	}
	if err := detach("host", "svc2"); code(err) != 400 {
		t.Fatalf("detach twice: %v", err)
	}
	if err := wait(projects.DisableXpnHost(ctx, &computepb.DisableXpnHostProjectRequest{Project: "host2"})); err != nil {
		t.Fatal(err)
	}
}
