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

// TestSubnetIAMAndListUsableThroughTheGoClient (#2140): a host subnet's
// policy set, read back with its etag and version, a stale etag refused,
// testIamPermissions answering what was asked (enforcement narrows it),
// and listUsable listing a service project's own subnets and its host's,
// across regions.
func TestSubnetIAMAndListUsableThroughTheGoClient(t *testing.T) {
	ctx := context.Background()
	s, opts := newTestServer(t, Options{VMs: fakeVMs{}, ProjectOrganization: func(p string) (string, bool) { return "organizations/1", true }})
	projects, err := computeapi.NewProjectsRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	subnets, err := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	networks, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err == nil {
			err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	wait(networks.Insert(ctx, &computepb.InsertNetworkRequest{Project: "host", NetworkResource: &computepb.Network{
		Name: proto.String("shared"), AutoCreateSubnetworks: proto.Bool(false)}}))
	for _, sn := range []struct{ region, name, cidr string }{{"us-central1", "a", "10.10.0.0/24"}, {"europe-west1", "b", "10.20.0.0/24"}} {
		wait(subnets.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: "host", Region: sn.region, SubnetworkResource: &computepb.Subnetwork{
			Name: proto.String(sn.name), Network: proto.String("global/networks/shared"), IpCidrRange: proto.String(sn.cidr)}}))
	}
	wait(projects.EnableXpnHost(ctx, &computepb.EnableXpnHostProjectRequest{Project: "host"}))
	wait(projects.EnableXpnResource(ctx, &computepb.EnableXpnResourceProjectRequest{Project: "host",
		ProjectsEnableXpnResourceRequestResource: &computepb.ProjectsEnableXpnResourceRequest{XpnResource: &computepb.XpnResourceId{
			Id: proto.String("svc"), Type: proto.String("PROJECT")}}}))

	p, err := subnets.GetIamPolicy(ctx, &computepb.GetIamPolicySubnetworkRequest{Project: "host", Region: "us-central1", Resource: "a"})
	if err != nil || len(p.GetBindings()) != 0 || p.GetEtag() == "" {
		t.Fatalf("empty policy: %v %v", p, err)
	}
	empty := p.GetEtag()
	set, err := subnets.SetIamPolicy(ctx, &computepb.SetIamPolicySubnetworkRequest{Project: "host", Region: "us-central1", Resource: "a",
		RegionSetPolicyRequestResource: &computepb.RegionSetPolicyRequest{Policy: &computepb.Policy{Etag: proto.String(empty), Bindings: []*computepb.Binding{
			{Role: proto.String("roles/compute.networkUser"), Members: []string{"user:dev@example.com"}}}}}})
	if err != nil || len(set.GetBindings()) != 1 || set.GetEtag() == empty {
		t.Fatalf("setIamPolicy: %v %v", set, err)
	}
	got, err := subnets.GetIamPolicy(ctx, &computepb.GetIamPolicySubnetworkRequest{Project: "host", Region: "us-central1", Resource: "a",
		OptionsRequestedPolicyVersion: proto.Int32(3)})
	if err != nil || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != "user:dev@example.com" || got.GetEtag() != set.GetEtag() {
		t.Fatalf("getIamPolicy: %v %v", got, err)
	}
	if pol := s.Policy("projects/host/regions/us-central1/subnetworks/a"); pol == nil || len(pol.Bindings) != 1 {
		t.Fatalf("Policy: %v", pol)
	}
	if _, err := subnets.SetIamPolicy(ctx, &computepb.SetIamPolicySubnetworkRequest{Project: "host", Region: "us-central1", Resource: "a",
		RegionSetPolicyRequestResource: &computepb.RegionSetPolicyRequest{Policy: &computepb.Policy{Etag: proto.String(empty)}}}); code(err) != 409 {
		t.Fatalf("a stale etag: %v", err)
	}
	if _, err := subnets.GetIamPolicy(ctx, &computepb.GetIamPolicySubnetworkRequest{Project: "host", Region: "us-central1", Resource: "nope"}); code(err) != 404 {
		t.Fatalf("no such subnet: %v", err)
	}
	tp, err := subnets.TestIamPermissions(ctx, &computepb.TestIamPermissionsSubnetworkRequest{Project: "host", Region: "us-central1", Resource: "a",
		TestPermissionsRequestResource: &computepb.TestPermissionsRequest{Permissions: []string{"compute.subnetworks.use"}}})
	if err != nil || len(tp.GetPermissions()) != 1 {
		t.Fatalf("testIamPermissions: %v %v", tp, err)
	}
	if _, err := subnets.TestIamPermissions(ctx, &computepb.TestIamPermissionsSubnetworkRequest{Project: "host", Region: "us-central1", Resource: "a",
		TestPermissionsRequestResource: &computepb.TestPermissionsRequest{Permissions: []string{"compute.*"}}}); code(err) != 400 {
		t.Fatalf("a wildcard: %v", err)
	}

	usable := func(project, svc string) map[string]*computepb.UsableSubnetwork {
		t.Helper()
		req := &computepb.ListUsableSubnetworksRequest{Project: project}
		if svc != "" {
			req.ServiceProject = proto.String(svc)
		}
		out := map[string]*computepb.UsableSubnetwork{}
		it := subnets.ListUsable(ctx, req)
		for {
			u, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			out[u.GetSubnetwork()[strings.Index(u.GetSubnetwork(), "projects/"):]] = u
		}
		return out
	}
	got2 := usable("svc", "")
	a := "projects/host/regions/us-central1/subnetworks/a"
	if u := got2[a]; u == nil || u.GetIpCidrRange() != "10.10.0.0/24" || !strings.HasSuffix(u.GetNetwork(), "/projects/host/global/networks/shared") {
		t.Fatalf("listUsable of the service project: %v", got2)
	}
	if got2["projects/host/regions/europe-west1/subnetworks/b"] == nil {
		t.Fatalf("listUsable misses the other region: %v", got2)
	}
	if got := usable("host", "svc"); got[a] == nil {
		t.Fatalf("listUsable with serviceProject: %v", got)
	}
	if _, err := subnets.ListUsable(ctx, &computepb.ListUsableSubnetworksRequest{Project: "host", ServiceProject: proto.String("stranger")}).Next(); code(err) != 400 {
		t.Fatalf("serviceProject not attached: %v", err)
	}
}
