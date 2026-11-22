//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// TestSharedVPCThroughTheGoClient2140 (#2140), the official compute/apiv1
// ProjectsRESTClient over projects made in the local organization by
// Resource Manager's official v3 client: enableXpnHost makes a host
// (xpnProjectStatus HOST), a top-level project outside the organization is
// refused, enableXpnResource attaches a service project, a second host is
// refused for it ("A service project can only attach to one host
// project"), getXpnHost, getXpnResources and listXpnHosts answer them, the
// service project reserves an internal address in the host's subnet while
// an unattached project is refused ("Cross project referencing"),
// detaching is refused while the address uses the subnet and disabling the
// host while a project is attached; then both are undone.
// covers: compute.projects.enableXpnHost, compute.projects.disableXpnHost, compute.projects.enableXpnResource, compute.projects.disableXpnResource, compute.projects.getXpnHost, compute.projects.getXpnResources, compute.projects.listXpnHosts
func TestSharedVPCThroughTheGoClient2140(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	_, _, pc := foldersClients(t, h) // skips without Resource Manager
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	project := func(prefix, parent string) string {
		t.Helper()
		id := v1ID(h, prefix)
		op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id, Parent: parent}})
		if err != nil {
			t.Fatalf("CreateProject %s: %v", id, err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatalf("CreateProject %s: %v", id, err)
		}
		t.Cleanup(func() {
			if op, err := pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + id}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		})
		return id
	}
	host, svc, host2, other := project("xh-", localOrg), project("xs-", localOrg), project("xh2-", localOrg), project("xo-", localOrg)
	loner := project("xl-", "")

	projects, err := computeapi.NewProjectsRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projects.Close() })
	addrs, _ := computeapi.NewAddressesRESTClient(ctx, opts...)
	wait := func(op *computeapi.Operation, err error) error {
		t.Helper()
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	enable := func(p string) error {
		return wait(projects.EnableXpnHost(ctx, &computepb.EnableXpnHostProjectRequest{Project: p}))
	}
	res := func(id string) *computepb.XpnResourceId {
		return &computepb.XpnResourceId{Id: proto.String(id), Type: proto.String("PROJECT")}
	}
	attach := func(host, id string) error {
		return wait(projects.EnableXpnResource(ctx, &computepb.EnableXpnResourceProjectRequest{Project: host,
			ProjectsEnableXpnResourceRequestResource: &computepb.ProjectsEnableXpnResourceRequest{XpnResource: res(id)}}))
	}
	detach := func(host, id string) error {
		return wait(projects.DisableXpnResource(ctx, &computepb.DisableXpnResourceProjectRequest{Project: host,
			ProjectsDisableXpnResourceRequestResource: &computepb.ProjectsDisableXpnResourceRequest{XpnResource: res(id)}}))
	}
	disable := func(p string) error {
		return wait(projects.DisableXpnHost(ctx, &computepb.DisableXpnHostProjectRequest{Project: p}))
	}

	if err := enable(loner); computeCode(err) != 400 {
		t.Fatalf("a project outside the organization as a host: %v", err)
	}
	for _, p := range []string{host, host2} {
		if err := enable(p); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = disable(p) })
	}
	if p, err := projects.Get(ctx, &computepb.GetProjectRequest{Project: host}); err != nil || p.GetXpnProjectStatus() != "HOST" {
		t.Fatalf("host's xpnProjectStatus = %q, %v", p.GetXpnProjectStatus(), err)
	}
	if err := attach(host, svc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = detach(host, svc) })
	if err := attach(host2, svc); computeCode(err) != 400 || !strings.Contains(err.Error(), "one host project") {
		t.Fatalf("a second host for the service project: %v", err)
	}
	if err := attach(host, host2); computeCode(err) != 400 {
		t.Fatalf("a host as a service project: %v", err)
	}
	if err := attach(host, loner); computeCode(err) != 400 {
		t.Fatalf("a service project outside the organization: %v", err)
	}
	if got, err := projects.GetXpnHost(ctx, &computepb.GetXpnHostProjectRequest{Project: svc}); err != nil || got.GetName() != host {
		t.Fatalf("getXpnHost = %q, %v", got.GetName(), err)
	}
	var ids []string
	it := projects.GetXpnResources(ctx, &computepb.GetXpnResourcesProjectsRequest{Project: host})
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
	if strings.Join(ids, ",") != svc+":PROJECT" {
		t.Fatalf("getXpnResources = %v", ids)
	}
	seen := map[string]bool{}
	hit := projects.ListXpnHosts(ctx, &computepb.ListXpnHostsProjectsRequest{Project: svc,
		ProjectsListXpnHostsRequestResource: &computepb.ProjectsListXpnHostsRequest{Organization: proto.String(localOrg)}})
	for {
		p, err := hit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[p.GetName()] = true
	}
	if !seen[host] || !seen[host2] || seen[svc] {
		t.Fatalf("listXpnHosts = %v", seen)
	}

	// The service project uses the host's default subnet.
	shared := "projects/" + host + "/regions/us-central1/subnetworks/default"
	reserve := func(p, name string) error {
		return wait(addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: "us-central1", AddressResource: &computepb.Address{
			Name: proto.String(name), AddressType: proto.String("INTERNAL"), Subnetwork: proto.String(shared)}}))
	}
	if err := reserve(svc, "shared-ip"); err != nil {
		t.Fatal(err)
	}
	a, err := addrs.Get(ctx, &computepb.GetAddressRequest{Project: svc, Region: "us-central1", Address: "shared-ip"})
	if err != nil || !strings.HasSuffix(a.GetNetwork(), "projects/"+host+"/global/networks/default") || !strings.HasPrefix(a.GetAddress(), "10.128.0.") {
		t.Fatalf("shared address = %v, %v", a, err)
	}
	if err := reserve(other, "x"); computeCode(err) != 400 || !strings.Contains(err.Error(), "Cross project referencing") {
		t.Fatalf("an unattached project on the host subnet: %v", err)
	}
	if err := detach(host, svc); computeCode(err) != 400 {
		t.Fatalf("detach while the address uses the subnet: %v", err)
	}
	if err := disable(host); computeCode(err) != 400 {
		t.Fatalf("disable a host with an attached project: %v", err)
	}
	if err := wait(addrs.Delete(ctx, &computepb.DeleteAddressRequest{Project: svc, Region: "us-central1", Address: "shared-ip"})); err != nil {
		t.Fatal(err)
	}
	if err := detach(host, svc); err != nil {
		t.Fatal(err)
	}
	if err := disable(host); err != nil {
		t.Fatal(err)
	}
}
