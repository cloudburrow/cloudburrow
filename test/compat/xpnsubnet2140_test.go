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

// TestSharedVPCIndividualSubnetsThroughTheGoClient2140 (#2140), Shared
// VPC's Individual subnets (subnet-level permissions) through the official
// compute/apiv1 SubnetworksRESTClient: a host project's subnet starts with
// the empty policy (an etag, no bindings), SetIamPolicy grants
// roles/compute.networkUser on it, GetIamPolicy (requested version 3)
// returns it with the new etag, a stale etag is refused (409, as IAM's Policy concurrency documents),
// TestIamPermissions answers for the instance's own account, and
// ListUsable lists the host's subnets for its attached service project
// (and, with serviceProject, on the host) across regions.
// covers: compute.subnetworks.getIamPolicy, compute.subnetworks.setIamPolicy, compute.subnetworks.testIamPermissions, compute.subnetworks.listUsable
func TestSharedVPCIndividualSubnetsThroughTheGoClient2140(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	_, _, pc := foldersClients(t, h) // skips without Resource Manager
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	project := func(prefix string) string {
		t.Helper()
		id := v1ID(h, prefix)
		op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id, Parent: localOrg}})
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
	host, svc := project("xih-"), project("xis-")
	projects, err := computeapi.NewProjectsRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projects.Close() })
	subnets, err := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subnets.Close() })
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err == nil {
			err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	res := &computepb.XpnResourceId{Id: proto.String(svc), Type: proto.String("PROJECT")}
	wait(projects.EnableXpnHost(ctx, &computepb.EnableXpnHostProjectRequest{Project: host}))
	t.Cleanup(func() {
		_, _ = projects.DisableXpnHost(context.Background(), &computepb.DisableXpnHostProjectRequest{Project: host})
	})
	wait(projects.EnableXpnResource(ctx, &computepb.EnableXpnResourceProjectRequest{Project: host,
		ProjectsEnableXpnResourceRequestResource: &computepb.ProjectsEnableXpnResourceRequest{XpnResource: res}}))
	t.Cleanup(func() {
		_, _ = projects.DisableXpnResource(context.Background(), &computepb.DisableXpnResourceProjectRequest{Project: host,
			ProjectsDisableXpnResourceRequestResource: &computepb.ProjectsDisableXpnResourceRequest{XpnResource: res}})
	})

	const region = "us-central1"
	p, err := subnets.GetIamPolicy(ctx, &computepb.GetIamPolicySubnetworkRequest{Project: host, Region: region, Resource: "default"})
	if err != nil || len(p.GetBindings()) != 0 || p.GetEtag() == "" {
		t.Fatalf("the default subnet's policy: %v %v", p, err)
	}
	empty := p.GetEtag()
	member := "user:dev-" + svc + "@example.com"
	set, err := subnets.SetIamPolicy(ctx, &computepb.SetIamPolicySubnetworkRequest{Project: host, Region: region, Resource: "default",
		RegionSetPolicyRequestResource: &computepb.RegionSetPolicyRequest{Policy: &computepb.Policy{Etag: proto.String(empty),
			Bindings: []*computepb.Binding{{Role: proto.String("roles/compute.networkUser"), Members: []string{member}}}}}})
	if err != nil || len(set.GetBindings()) != 1 || set.GetEtag() == empty {
		t.Fatalf("SetIamPolicy: %v %v", set, err)
	}
	got, err := subnets.GetIamPolicy(ctx, &computepb.GetIamPolicySubnetworkRequest{Project: host, Region: region, Resource: "default",
		OptionsRequestedPolicyVersion: proto.Int32(3)})
	if err != nil || got.GetEtag() != set.GetEtag() || len(got.GetBindings()) != 1 ||
		got.GetBindings()[0].GetRole() != "roles/compute.networkUser" || got.GetBindings()[0].GetMembers()[0] != member {
		t.Fatalf("GetIamPolicy: %v %v", got, err)
	}
	if _, err := subnets.SetIamPolicy(ctx, &computepb.SetIamPolicySubnetworkRequest{Project: host, Region: region, Resource: "default",
		RegionSetPolicyRequestResource: &computepb.RegionSetPolicyRequest{Policy: &computepb.Policy{Etag: proto.String(empty)}}}); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("a stale etag: %v", err)
	}
	tp, err := subnets.TestIamPermissions(ctx, &computepb.TestIamPermissionsSubnetworkRequest{Project: host, Region: region, Resource: "default",
		TestPermissionsRequestResource: &computepb.TestPermissionsRequest{Permissions: []string{"compute.subnetworks.use", "compute.subnetworks.get"}}})
	if err != nil || len(tp.GetPermissions()) != 2 {
		t.Fatalf("TestIamPermissions as the instance's account: %v %v", tp, err)
	}

	usable := func(project, serviceProject string) map[string]bool {
		t.Helper()
		req := &computepb.ListUsableSubnetworksRequest{Project: project}
		if serviceProject != "" {
			req.ServiceProject = proto.String(serviceProject)
		}
		out := map[string]bool{}
		it := subnets.ListUsable(ctx, req)
		for {
			u, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListUsable %s: %v", project, err)
			}
			link := u.GetSubnetwork()
			if i := strings.Index(link, "projects/"); i >= 0 {
				out[link[i:]] = u.GetIpCidrRange() != "" && u.GetNetwork() != ""
			}
		}
		return out
	}
	want := "projects/" + host + "/regions/" + region + "/subnetworks/default"
	other := "projects/" + host + "/regions/europe-west1/subnetworks/default"
	if got := usable(svc, ""); !got[want] || !got[other] {
		t.Fatalf("ListUsable of the service project misses the host's subnets: %v", got)
	}
	if got := usable(host, svc); !got[want] {
		t.Fatalf("ListUsable with serviceProject: %v", got)
	}
}
