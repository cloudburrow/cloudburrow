//go:build compat

package compat

import (
	"fmt"
	"testing"
	"time"

	certificatemanager "cloud.google.com/go/certificatemanager/apiv1"
	cmpb "cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	networksecurity "cloud.google.com/go/networksecurity/apiv1"
	nspb "cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	networkservices "cloud.google.com/go/networkservices/apiv1"
	nsvpb "cloud.google.com/go/networkservices/apiv1/networkservicespb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestComputePortAPIsOverGRPC (#2160): Network Security, Network Services
// and Certificate Manager, which Google serves over gRPC as well as JSON,
// answer their official Go clients' default gRPC transport on the Compute
// Engine port: an address group is created (its operation polled over
// gRPC, from the API that owns it), its items added and listed, and
// deleted; Network Services lists its gateways and answers NOT_FOUND for a
// missing one; Certificate Manager lists its certificates; and a missing
// operation is NOT_FOUND.
func TestComputePortAPIsOverGRPC(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvCompute) // skips without it
	ctx := h.Context()
	opts := traceGRPCOpts(addr)
	project := h.Project()
	loc := "projects/" + project + "/locations/global"

	ag, err := networksecurity.NewAddressGroupClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ag.Close() })
	id := fmt.Sprintf("grpc-ag-%d", time.Now().UnixNano()%1_000_000_000)
	op, err := ag.CreateAddressGroup(ctx, &nspb.CreateAddressGroupRequest{Parent: loc, AddressGroupId: id,
		AddressGroup: &nspb.AddressGroup{Type: nspb.AddressGroup_IPV4, Capacity: 100, Items: []string{"192.0.2.0/24"},
			Purpose: []nspb.AddressGroup_Purpose{nspb.AddressGroup_CLOUD_ARMOR}}})
	if err != nil {
		t.Fatalf("CreateAddressGroup over gRPC: %v", err)
	}
	g, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the create operation over gRPC: %v", err)
	}
	if g.GetName() != loc+"/addressGroups/"+id || g.GetCapacity() != 100 {
		t.Errorf("CreateAddressGroup: %v", g)
	}
	aop, err := ag.AddAddressGroupItems(ctx, &nspb.AddAddressGroupItemsRequest{AddressGroup: g.GetName(), Items: []string{"198.51.100.7"}})
	if err != nil {
		t.Fatal(err)
	}
	if g, err = aop.Wait(ctx); err != nil || len(g.GetItems()) != 2 {
		t.Errorf("AddAddressGroupItems over gRPC: %v %v", g, err)
	}
	found := false
	for it := ag.ListAddressGroups(ctx, &nspb.ListAddressGroupsRequest{Parent: loc}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || x.GetName() == g.GetName()
	}
	if !found {
		t.Errorf("ListAddressGroups over gRPC has no %s", g.GetName())
	}
	dop, err := ag.DeleteAddressGroup(ctx, &nspb.DeleteAddressGroupRequest{Name: g.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.GetAddressGroup(ctx, &nspb.GetAddressGroupRequest{Name: g.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetAddressGroup after delete: %v, want NOT_FOUND", err)
	}
	if _, err := ag.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: loc + "/operations/operation-0-missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("a missing operation: %v, want NOT_FOUND", err)
	}

	nsv, err := networkservices.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nsv.Close() })
	it := nsv.ListGateways(ctx, &nsvpb.ListGatewaysRequest{Parent: "projects/" + project + "/locations/us-central1"})
	if _, err := it.Next(); err != nil && err != iterator.Done {
		t.Errorf("ListGateways over gRPC: %v", err)
	}
	if _, err := nsv.GetGateway(ctx, &nsvpb.GetGatewayRequest{Name: "projects/" + project + "/locations/us-central1/gateways/none"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetGateway of a missing gateway: %v, want NOT_FOUND", err)
	}

	cm, err := certificatemanager.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cm.Close() })
	cit := cm.ListCertificates(ctx, &cmpb.ListCertificatesRequest{Parent: loc})
	if _, err := cit.Next(); err != nil && err != iterator.Done {
		t.Errorf("ListCertificates over gRPC: %v", err)
	}
}
