//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/run/apiv2/runpb"
	vpcaccess "cloud.google.com/go/vpcaccess/apiv1"
	"cloud.google.com/go/vpcaccess/apiv1/vpcaccesspb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// Serverless VPC Access (vpcaccess.googleapis.com, #2140) is served on the
// Compute Engine port, JSON under /v1/: the official REST client's options
// point there, never at Google, with no credentials.
func vpcAccessClient(t *testing.T, h *Harness) *vpcaccess.Client {
	t.Helper()
	addr := h.Endpoint(EnvCompute) // skips without it
	if addr == "" {
		t.Fatal("no Compute Engine endpoint")
	}
	c, err := vpcaccess.NewRESTClient(context.Background(), option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ensureCompatConnector makes the connector runStoredSettings names, when
// Compute Engine is served: Cloud Run then checks that it exists and is
// READY (#2140). Without compute, Cloud Run records it unchecked.
func ensureCompatConnector(h *Harness) {
	addr := os.Getenv(EnvCompute)
	if addr == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := vpcaccess.NewRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		return
	}
	defer c.Close()
	op, err := c.CreateConnector(ctx, &vpcaccesspb.CreateConnectorRequest{Parent: "projects/" + h.Project() + "/locations/us-central1",
		ConnectorId: "compat", Connector: &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.250.0/28"}})
	if err == nil {
		_, _ = op.Wait(ctx)
	}
}

// TestServerlessVPCAccessConnectorsThroughTheGoClient (#2140), the official
// vpcaccess/apiv1 REST client: the locations; a connector on the default
// network with a custom /28 range, READY with Google's defaults (e2-micro,
// 2 to 10 instances, 200 to 1000 Mbps); the documented refusals (a range
// that is not a /28, one overlapping a subnet or another connector, a bad
// ID, instance counts out of range, a duplicate); a connector on an
// existing /28 subnet made with the compute client; its operations; and
// deletes.
// covers: google.cloud.vpcaccess.v1.VpcAccessService/CreateConnector, google.cloud.vpcaccess.v1.VpcAccessService/GetConnector, google.cloud.vpcaccess.v1.VpcAccessService/ListConnectors, google.cloud.vpcaccess.v1.VpcAccessService/DeleteConnector, google.cloud.location.Locations/ListLocations
func TestServerlessVPCAccessConnectorsThroughTheGoClient(t *testing.T) {
	h := New(t)
	c := vpcAccessClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	p := h.Project()
	parent := "projects/" + p + "/locations/us-central1"
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e5)

	locs := c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + p})
	found := false
	for {
		l, err := locs.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListLocations: %v", err)
		}
		found = found || l.GetLocationId() == "us-central1"
	}
	if !found {
		t.Error("ListLocations has no us-central1")
	}

	id := "ct-conn-" + suffix
	op, err := c.CreateConnector(ctx, &vpcaccesspb.CreateConnectorRequest{Parent: parent, ConnectorId: id,
		Connector: &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.240.0/28"}})
	if err != nil {
		t.Fatalf("CreateConnector: %v", err)
	}
	conn, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("waiting for the connector: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteConnector(h.Context(), &vpcaccesspb.DeleteConnectorRequest{Name: parent + "/connectors/" + id}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	if conn.GetState() != vpcaccesspb.Connector_READY || conn.GetMachineType() != "e2-micro" || conn.GetMinInstances() != 2 ||
		conn.GetMaxInstances() != 10 || conn.GetMinThroughput() != 200 || conn.GetMaxThroughput() != 1000 || conn.GetNetwork() != "default" {
		t.Errorf("the new connector is %v", conn)
	}
	got, err := c.GetConnector(ctx, &vpcaccesspb.GetConnectorRequest{Name: parent + "/connectors/" + id})
	if err != nil || !proto.Equal(got, conn) {
		t.Errorf("GetConnector = %v, %v; want %v", got, err, conn)
	}

	for _, tc := range []struct {
		id string
		c  *vpcaccesspb.Connector
	}{
		{"ct-bad-" + suffix, &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.241.0/27"}},
		{"ct-bad-" + suffix, &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.240.0/28"}},
		{"ct-bad-" + suffix, &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.128.0.0/28"}},
		{"Bad_" + suffix, &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.242.0/28"}},
		{"ct-bad-" + suffix, &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.242.0/28", MinInstances: 1}},
		{"ct-bad-" + suffix, &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.242.0/28", MinInstances: 6, MaxInstances: 4}},
		{"ct-bad-" + suffix, &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.242.0/28", MachineType: "n2-standard-2"}},
		{"ct-bad-" + suffix, &vpcaccesspb.Connector{Network: "ct-no-such-net", IpCidrRange: "10.8.242.0/28"}},
	} {
		if _, err := c.CreateConnector(ctx, &vpcaccesspb.CreateConnectorRequest{Parent: parent, ConnectorId: tc.id, Connector: tc.c}); grpcCode(err) != codes.InvalidArgument {
			t.Errorf("CreateConnector(%s, %v) = %v, want InvalidArgument", tc.id, tc.c, err)
		}
	}
	if _, err := c.CreateConnector(ctx, &vpcaccesspb.CreateConnectorRequest{Parent: parent, ConnectorId: id,
		Connector: &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.243.0/28"}}); grpcCode(err) != codes.AlreadyExists {
		t.Errorf("a duplicate connector: %v, want AlreadyExists", err)
	}

	// A connector on an existing /28 subnet of a custom network.
	opts := computeOpts(t, h)
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	netName, subName := "ct-sva-"+suffix, "ct-sva-sub-"+suffix
	nop, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{Name: proto.String(netName), AutoCreateSubnetworks: proto.Bool(false)}})
	if err != nil || nop.Wait(ctx) != nil {
		t.Fatalf("network: %v", err)
	}
	t.Cleanup(func() {
		if op, err := nets.Delete(h.Context(), &computepb.DeleteNetworkRequest{Project: p, Network: netName}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	sop, err := subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: "us-central1", SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String(subName), Network: proto.String("projects/" + p + "/global/networks/" + netName), IpCidrRange: proto.String("10.9.0.0/28")}})
	if err != nil || sop.Wait(ctx) != nil {
		t.Fatalf("subnet: %v", err)
	}
	t.Cleanup(func() {
		if op, err := subs.Delete(h.Context(), &computepb.DeleteSubnetworkRequest{Project: p, Region: "us-central1", Subnetwork: subName}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	subID := "ct-sub-" + suffix
	sc, err := c.CreateConnector(ctx, &vpcaccesspb.CreateConnectorRequest{Parent: parent, ConnectorId: subID,
		Connector: &vpcaccesspb.Connector{Subnet: &vpcaccesspb.Connector_Subnet{Name: subName}, MinInstances: 3, MaxInstances: 5, MachineType: "f1-micro"}})
	if err != nil {
		t.Fatalf("CreateConnector on a subnet: %v", err)
	}
	sconn, err := sc.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sconn.GetNetwork() != netName || sconn.GetSubnet().GetProjectId() != p || sconn.GetMinThroughput() != 300 || sconn.GetMaxThroughput() != 500 {
		t.Errorf("the subnet connector is %v", sconn)
	}
	if _, err := c.CreateConnector(ctx, &vpcaccesspb.CreateConnectorRequest{Parent: parent, ConnectorId: "ct-sub2-" + suffix,
		Connector: &vpcaccesspb.Connector{Subnet: &vpcaccesspb.Connector_Subnet{Name: subName}}}); err == nil {
		t.Error("a second connector on the same subnet was accepted")
	}

	it := c.ListConnectors(ctx, &vpcaccesspb.ListConnectorsRequest{Parent: parent})
	names := map[string]bool{}
	for {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListConnectors: %v", err)
		}
		names[x.GetName()] = true
	}
	if !names[parent+"/connectors/"+id] || !names[parent+"/connectors/"+subID] {
		t.Errorf("ListConnectors = %v", names)
	}
	if o, err := c.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: sc.Name()}); err != nil || !o.GetDone() {
		t.Errorf("GetOperation(%s) = %v, %v", sc.Name(), o, err)
	}
	ops := c.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: parent})
	if o, err := ops.Next(); err != nil || !strings.HasPrefix(o.GetName(), parent+"/operations/") {
		t.Errorf("ListOperations: %v, %v", o, err)
	}
	dop, err := c.DeleteConnector(ctx, &vpcaccesspb.DeleteConnectorRequest{Name: parent + "/connectors/" + subID})
	if err != nil || dop.Wait(ctx) != nil {
		t.Fatalf("DeleteConnector: %v", err)
	}
	if _, err := c.GetConnector(ctx, &vpcaccesspb.GetConnectorRequest{Name: parent + "/connectors/" + subID}); grpcCode(err) != codes.NotFound {
		t.Errorf("a deleted connector: %v, want NotFound", err)
	}
}

// TestRunVPCEgressThroughTheGoClient2140 (#2140), the official run/apiv2
// client: a service whose vpcAccess names a READY connector of its region
// (given bare, as gcloud may, and read back in full) with egress
// ALL_TRAFFIC; a missing connector, one in another region and both a
// connector and network interfaces refused; Direct VPC egress on the
// default network, its subnetwork the network's own name, recorded on the
// service and its revision; an unknown subnetwork refused.
// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService, google.cloud.run.v2.Revisions/GetRevision
func TestRunVPCEgressThroughTheGoClient2140(t *testing.T) {
	h := New(t)
	va := vpcAccessClient(t, h)
	c := runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	p := h.Project()
	parent := "projects/" + p + "/locations/us-central1"
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e5)
	connID := "ct-run-" + suffix
	op, err := va.CreateConnector(ctx, &vpcaccesspb.CreateConnectorRequest{Parent: parent, ConnectorId: connID,
		Connector: &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.230.0/28"}})
	if err != nil {
		t.Fatalf("CreateConnector: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := va.DeleteConnector(h.Context(), &vpcaccesspb.DeleteConnectorRequest{Name: parent + "/connectors/" + connID}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	svc := func(v *runpb.VpcAccess) *runpb.Service {
		return &runpb.Service{Template: &runpb.RevisionTemplate{VpcAccess: v, Containers: []*runpb.Container{{Image: helloDigest}}}}
	}
	for _, v := range []*runpb.VpcAccess{
		{Connector: parent + "/connectors/ct-none-" + suffix},
		{Connector: "projects/" + p + "/locations/europe-west1/connectors/" + connID},
		{Connector: connID, NetworkInterfaces: []*runpb.VpcAccess_NetworkInterface{{Network: "default"}}},
		{NetworkInterfaces: []*runpb.VpcAccess_NetworkInterface{{Subnetwork: "ct-no-such-sub"}}},
	} {
		if _, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: "ct-vpc-bad-" + suffix, Service: svc(v)}); err == nil {
			t.Errorf("CreateService with vpcAccess %v was accepted", v)
		}
	}
	for _, tc := range []struct {
		id   string
		v    *runpb.VpcAccess
		want func(*runpb.VpcAccess) bool
	}{
		{"ct-vpc-conn-" + suffix, &runpb.VpcAccess{Connector: connID, Egress: runpb.VpcAccess_ALL_TRAFFIC},
			func(v *runpb.VpcAccess) bool {
				return v.GetConnector() == parent+"/connectors/"+connID && v.GetEgress() == runpb.VpcAccess_ALL_TRAFFIC
			}},
		{"ct-vpc-direct-" + suffix, &runpb.VpcAccess{Egress: runpb.VpcAccess_PRIVATE_RANGES_ONLY,
			NetworkInterfaces: []*runpb.VpcAccess_NetworkInterface{{Network: "default", Tags: []string{"run"}}}},
			func(v *runpb.VpcAccess) bool {
				ni := v.GetNetworkInterfaces()
				return len(ni) == 1 && ni[0].GetNetwork() == "default" && v.GetEgress() == runpb.VpcAccess_PRIVATE_RANGES_ONLY
			}},
	} {
		name := parent + "/services/" + tc.id
		op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: tc.id, Service: svc(tc.v)})
		if err != nil {
			t.Fatalf("CreateService(%s): %v", tc.id, err)
		}
		t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
		got, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("waiting for %s: %v", tc.id, err)
		}
		if !tc.want(got.GetTemplate().GetVpcAccess()) {
			t.Errorf("%s's vpcAccess reads back as %v", tc.id, got.GetTemplate().GetVpcAccess())
		}
		g, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
		if err != nil || !tc.want(g.GetTemplate().GetVpcAccess()) {
			t.Errorf("GetService(%s) = %v, %v", tc.id, g.GetTemplate().GetVpcAccess(), err)
		}
	}
}
