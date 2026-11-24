//go:build compat

package compat

import (
	"errors"
	"fmt"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	networkconnectivity "cloud.google.com/go/networkconnectivity/apiv1"
	ncpb "cloud.google.com/go/networkconnectivity/apiv1/networkconnectivitypb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	ncrest "google.golang.org/api/networkconnectivity/v1"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// ncClient is the official Network Connectivity gRPC client, on the
// Compute Engine port, where the API is served (#2141).
func ncClient(t *testing.T, h *Harness) *networkconnectivity.CrossNetworkAutomationClient {
	t.Helper()
	addr := sqlComputeEndpoint(h) // skips without Compute Engine
	g, err := networkconnectivity.NewCrossNetworkAutomationClient(h.Context(), option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// ncNetwork makes a custom mode network with one subnet in us-central1.
func ncNetwork(t *testing.T, h *Harness, name, cidr string) (network, subnet string) {
	t.Helper()
	opts := sqlComputeOpts(t, h)
	ctx := h.Context()
	p := h.Project()
	nets, _ := computeapi.NewNetworksRESTClient(ctx, opts...)
	subs, err := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	op, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(name), AutoCreateSubnetworks: proto.Bool(false)}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	network = "projects/" + p + "/global/networks/" + name
	op, err = subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: "us-central1", SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String(name + "-s"), Network: proto.String(network), IpCidrRange: proto.String(cidr)}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	subnet = "projects/" + p + "/regions/us-central1/subnetworks/" + name + "-s"
	t.Cleanup(func() {
		if op, err := subs.Delete(ctx, &computepb.DeleteSubnetworkRequest{Project: p, Region: "us-central1", Subnetwork: name + "-s"}); err == nil {
			_ = op.Wait(ctx)
		}
		if op, err := nets.Delete(ctx, &computepb.DeleteNetworkRequest{Project: p, Network: name}); err == nil {
			_ = op.Wait(ctx)
		}
	})
	return network, subnet
}

// TestNetworkConnectivityServiceConnectionPolicies (#2141), through the
// official networkconnectivity/apiv1 clients over gRPC and REST: a policy
// on a network's subnet (one per network, region and service class, its
// network and service class not updatable, its limit updatable, a stale
// etag refused), a producer's service connection map whose consumer has
// that policy (an endpoint made in the policy's subnet, listed on the map
// and the policy, and the policy then not deletable), the service class
// the map made (not deletable while the map names it), and a service
// connection token, each operation waited on.
// covers: google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/CreateServiceConnectionPolicy, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/GetServiceConnectionPolicy, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/ListServiceConnectionPolicies, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/UpdateServiceConnectionPolicy, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/DeleteServiceConnectionPolicy, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/CreateServiceConnectionMap, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/GetServiceConnectionMap, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/ListServiceConnectionMaps, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/UpdateServiceConnectionMap, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/DeleteServiceConnectionMap, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/GetServiceClass, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/ListServiceClasses, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/UpdateServiceClass, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/DeleteServiceClass, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/CreateServiceConnectionToken, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/GetServiceConnectionToken, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/ListServiceConnectionTokens, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/DeleteServiceConnectionToken
func TestNetworkConnectivityServiceConnectionPolicies(t *testing.T) {
	h := New(t)
	c := ncClient(t, h)
	ctx := h.Context()
	p := h.Project()
	parent := "projects/" + p + "/locations/us-central1"
	for i, tr := range []string{"grpc"} {
		t.Run(tr, func(t *testing.T) {
			h := h.For(t)
			suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
			network, subnet := ncNetwork(t, h, "ct-nc-"+tr+"-"+suffix, fmt.Sprintf("10.%d.0.0/28", 150+i))
			one := int64(1)
			cls := "ct-class-" + tr + "-" + suffix
			pol := &ncpb.ServiceConnectionPolicy{Network: network, ServiceClass: cls, Description: "compat",
				PscConfig: &ncpb.ServiceConnectionPolicy_PscConfig{Subnetworks: []string{subnet}, Limit: &one}}
			op, err := c.CreateServiceConnectionPolicy(ctx, &ncpb.CreateServiceConnectionPolicyRequest{Parent: parent, ServiceConnectionPolicyId: "pol-" + suffix,
				ServiceConnectionPolicy: pol})
			if err != nil {
				t.Fatal(err)
			}
			got, err := op.Wait(ctx)
			if err != nil || got.GetInfrastructure() != ncpb.Infrastructure_PSC || got.GetEtag() == "" {
				t.Fatalf("create: %v, %v", got, err)
			}
			if _, err := c.CreateServiceConnectionPolicy(ctx, &ncpb.CreateServiceConnectionPolicyRequest{Parent: parent, ServiceConnectionPolicyId: "dup-" + suffix,
				ServiceConnectionPolicy: pol}); status.Code(err) != codes.AlreadyExists {
				t.Errorf("a second policy for the network and class: %v; want ALREADY_EXISTS", err)
			}
			listed := false
			it := c.ListServiceConnectionPolicies(ctx, &ncpb.ListServiceConnectionPoliciesRequest{Parent: parent})
			for {
				x, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				listed = listed || x.GetName() == got.GetName()
			}
			if !listed {
				t.Error("the policy is not listed")
			}
			if _, err := c.UpdateServiceConnectionPolicy(ctx, &ncpb.UpdateServiceConnectionPolicyRequest{ServiceConnectionPolicy: &ncpb.ServiceConnectionPolicy{
				Name: got.GetName(), ServiceClass: "other"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"service_class"}}}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("update service_class: %v; want INVALID_ARGUMENT", err)
			}

			// A producer's map, whose consumer is the network: an endpoint in
			// the policy's subnet.
			sa := "projects/" + p + "/regions/us-central1/serviceAttachments/ct-sa-" + suffix
			mop, err := c.CreateServiceConnectionMap(ctx, &ncpb.CreateServiceConnectionMapRequest{Parent: parent, ServiceConnectionMapId: "map-" + suffix,
				ServiceConnectionMap: &ncpb.ServiceConnectionMap{ServiceClass: cls,
					ProducerPscConfigs: []*ncpb.ServiceConnectionMap_ProducerPscConfig{{ServiceAttachmentUri: sa}},
					ConsumerPscConfigs: []*ncpb.ServiceConnectionMap_ConsumerPscConfig{{Network: network, ConsumerInstanceProject: "projects/" + p}}}})
			if err != nil {
				t.Fatal(err)
			}
			m, err := mop.Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if m.GetServiceClassUri() != parent+"/serviceClasses/"+cls || len(m.GetConsumerPscConfigs()) != 1 {
				t.Fatalf("map: %v", m)
			}
			// The map's attachment is no producer's here, so the endpoint
			// cannot be made: FAILED, with its error.
			if cs := m.GetConsumerPscConnections(); len(cs) != 1 || cs[0].GetState() != ncpb.ServiceConnectionMap_ConsumerPscConnection_FAILED ||
				cs[0].GetErrorInfo().GetReason() == "" || m.GetConsumerPscConfigs()[0].GetState() != ncpb.ServiceConnectionMap_ConsumerPscConfig_VALID {
				t.Errorf("connections to an attachment nobody publishes: %v", m.GetConsumerPscConnections())
			}
			if _, err := c.GetServiceConnectionMap(ctx, &ncpb.GetServiceConnectionMapRequest{Name: m.GetName()}); err != nil {
				t.Error(err)
			}
			mit := c.ListServiceConnectionMaps(ctx, &ncpb.ListServiceConnectionMapsRequest{Parent: parent})
			n := 0
			for {
				x, err := mit.Next()
				if err != nil {
					break
				}
				if x.GetName() == m.GetName() {
					n++
				}
			}
			if n != 1 {
				t.Errorf("the map is listed %d times", n)
			}
			uop, err := c.UpdateServiceConnectionMap(ctx, &ncpb.UpdateServiceConnectionMapRequest{ServiceConnectionMap: &ncpb.ServiceConnectionMap{
				Name: m.GetName(), Description: "updated"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
			if err != nil {
				t.Fatal(err)
			}
			if um, err := uop.Wait(ctx); err != nil || um.GetDescription() != "updated" {
				t.Errorf("update the map: %v, %v", um, err)
			}
			// The service class the map made.
			cl, err := c.GetServiceClass(ctx, &ncpb.GetServiceClassRequest{Name: m.GetServiceClassUri()})
			if err != nil || cl.GetServiceClass() != cls {
				t.Fatalf("service class: %v, %v", cl, err)
			}
			cit := c.ListServiceClasses(ctx, &ncpb.ListServiceClassesRequest{Parent: parent})
			if x, err := cit.Next(); err != nil || x.GetName() == "" {
				t.Errorf("list classes: %v, %v", x, err)
			}
			cop, err := c.UpdateServiceClass(ctx, &ncpb.UpdateServiceClassRequest{ServiceClass: &ncpb.ServiceClass{Name: cl.GetName(), Description: "mine"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
			if err != nil {
				t.Fatal(err)
			}
			if x, err := cop.Wait(ctx); err != nil || x.GetDescription() != "mine" {
				t.Errorf("update the class: %v, %v", x, err)
			}
			if _, err := c.DeleteServiceClass(ctx, &ncpb.DeleteServiceClassRequest{Name: cl.GetName()}); status.Code(err) != codes.FailedPrecondition {
				t.Errorf("delete a class a map names: %v; want FAILED_PRECONDITION", err)
			}
			dop, err := c.DeleteServiceConnectionMap(ctx, &ncpb.DeleteServiceConnectionMapRequest{Name: m.GetName()})
			if err != nil {
				t.Fatal(err)
			}
			if err := dop.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if op, err := c.DeleteServiceClass(ctx, &ncpb.DeleteServiceClassRequest{Name: cl.GetName()}); err != nil {
				t.Errorf("delete the class: %v", err)
			} else if err := op.Wait(ctx); err != nil {
				t.Error(err)
			}

			// A token on the network.
			top, err := c.CreateServiceConnectionToken(ctx, &ncpb.CreateServiceConnectionTokenRequest{Parent: parent, ServiceConnectionTokenId: "tok-" + suffix,
				ServiceConnectionToken: &ncpb.ServiceConnectionToken{Network: network}})
			if err != nil {
				t.Fatal(err)
			}
			tok, err := top.Wait(ctx)
			if err != nil || tok.GetToken() == "" || tok.GetExpireTime() == nil {
				t.Fatalf("token: %v, %v", tok, err)
			}
			if _, err := c.GetServiceConnectionToken(ctx, &ncpb.GetServiceConnectionTokenRequest{Name: tok.GetName()}); err != nil {
				t.Error(err)
			}
			if x, err := c.ListServiceConnectionTokens(ctx, &ncpb.ListServiceConnectionTokensRequest{Parent: parent}).Next(); err != nil || x.GetName() == "" {
				t.Errorf("list tokens: %v, %v", x, err)
			}
			if op, err := c.DeleteServiceConnectionToken(ctx, &ncpb.DeleteServiceConnectionTokenRequest{Name: tok.GetName()}); err != nil {
				t.Error(err)
			} else if err := op.Wait(ctx); err != nil {
				t.Error(err)
			}

			// The policy: its limit updated, a stale etag refused, deleted.
			stale := "stale"
			if _, err := c.DeleteServiceConnectionPolicy(ctx, &ncpb.DeleteServiceConnectionPolicyRequest{Name: got.GetName(), Etag: &stale}); status.Code(err) != codes.Aborted {
				t.Errorf("delete with a stale etag: %v; want ABORTED", err)
			}
			two := int64(2)
			uop2, err := c.UpdateServiceConnectionPolicy(ctx, &ncpb.UpdateServiceConnectionPolicyRequest{ServiceConnectionPolicy: &ncpb.ServiceConnectionPolicy{
				Name: got.GetName(), PscConfig: &ncpb.ServiceConnectionPolicy_PscConfig{Limit: &two}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"psc_config.limit"}}})
			if err != nil {
				t.Fatal(err)
			}
			if up, err := uop2.Wait(ctx); err != nil || up.GetPscConfig().GetLimit() != 2 || len(up.GetPscConfig().GetSubnetworks()) != 1 {
				t.Errorf("update the limit: %v, %v", up, err)
			}
			pop, err := c.DeleteServiceConnectionPolicy(ctx, &ncpb.DeleteServiceConnectionPolicyRequest{Name: got.GetName()})
			if err != nil {
				t.Fatal(err)
			}
			if err := pop.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := c.GetServiceConnectionPolicy(ctx, &ncpb.GetServiceConnectionPolicyRequest{Name: got.GetName()}); status.Code(err) != codes.NotFound {
				t.Errorf("get after delete: %v; want NOT_FOUND", err)
			}
		})
	}
}

// TestCloudSQLPSCAutoConnections (#2141), with Cloud SQL and Compute
// Engine on one instance, through the official clients (sqladmin/v1,
// networkconnectivity/apiv1, compute/apiv1): an instance with Private
// Service Connect asking for automatic endpoints in two networks, one
// with a google-cloud-sql service connection policy and one without,
// answers the first's endpoint (an address of the policy's subnet,
// ACTIVE, VALID) and the second CONNECTION_POLICY_MISSING until a policy
// is made for it; each endpoint is a forwarding rule ACCEPTED by the
// instance's attachment, not deletable directly, listed on its policy,
// and gone with the instance, which then lets the policy be deleted.
func TestCloudSQLPSCAutoConnections(t *testing.T) {
	h := New(t)
	c := ncClient(t, h)
	svc := sqlAdminService(t, h)
	ctx := h.Context()
	p := h.Project()
	opts := sqlComputeOpts(t, h)
	rules, err := computeapi.NewForwardingRulesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	parent := "projects/" + p + "/locations/us-central1"
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	netA, subA := ncNetwork(t, h, "ct-auto-a-"+suffix, "10.160.0.0/28")
	netB, subB := ncNetwork(t, h, "ct-auto-b-"+suffix, "10.161.0.0/28")
	mkPolicy := func(id, network, subnet string) string {
		t.Helper()
		op, err := c.CreateServiceConnectionPolicy(ctx, &ncpb.CreateServiceConnectionPolicyRequest{Parent: parent, ServiceConnectionPolicyId: id,
			ServiceConnectionPolicy: &ncpb.ServiceConnectionPolicy{Network: network, ServiceClass: "google-cloud-sql",
				PscConfig: &ncpb.ServiceConnectionPolicy_PscConfig{Subnetworks: []string{subnet}}}})
		if err != nil {
			t.Fatal(err)
		}
		pol, err := op.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if op, err := c.DeleteServiceConnectionPolicy(ctx, &ncpb.DeleteServiceConnectionPolicyRequest{Name: pol.GetName()}); err == nil {
				_ = op.Wait(ctx)
			}
		})
		return pol.GetName()
	}
	polA := mkPolicy("ct-sql-a-"+suffix, netA, subA)

	name := "ct-auto-" + suffix
	op, err := svc.Instances.Insert(p, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", RootPassword: "Root-pass1", Region: "us-central1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: false, ForceSendFields: []string{"Ipv4Enabled"},
			PscConfig: &sqladmin.PscConfig{PscEnabled: true, AllowedConsumerProjects: []string{p},
				PscAutoConnections: []*sqladmin.PscAutoConnectionConfig{{ConsumerNetwork: netA, ConsumerProject: p}, {ConsumerNetwork: netB, ConsumerProject: p}}}}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	deleted := false
	t.Cleanup(func() {
		if deleted {
			return
		}
		if d, err := svc.Instances.Delete(p, name).Do(); err == nil {
			waitSQLOp(t, svc, p, d)
		}
	})
	waitSQLOp(t, svc, p, op)
	auto := func() []*sqladmin.PscAutoConnectionConfig {
		t.Helper()
		in, err := svc.Instances.Get(p, name).Do()
		if err != nil {
			t.Fatal(err)
		}
		return in.Settings.IpConfiguration.PscConfig.PscAutoConnections
	}
	got := auto()
	if len(got) != 2 {
		t.Fatalf("pscAutoConnections: %+v", got)
	}
	a, b := got[0], got[1]
	if a.ConsumerNetwork != netA || a.Status != "ACTIVE" || a.ConsumerNetworkStatus != "VALID" || a.IpAddress != "10.160.0.2" {
		t.Errorf("the endpoint in %s: %+v; want ACTIVE, VALID, 10.160.0.2", netA, a)
	}
	if b.ConsumerNetworkStatus != "CONNECTION_POLICY_MISSING" || b.IpAddress != "" {
		t.Errorf("the endpoint in %s, which has no policy: %+v", netB, b)
	}
	// The policy lists the endpoint, a forwarding rule the attachment
	// accepts, which is not deleted directly.
	pol, err := c.GetServiceConnectionPolicy(ctx, &ncpb.GetServiceConnectionPolicyRequest{Name: polA})
	if err != nil || len(pol.GetPscConnections()) != 1 || pol.GetPscConnections()[0].GetConsumerAddress() != a.IpAddress {
		t.Fatalf("policy connections: %v, %v", pol.GetPscConnections(), err)
	}
	fr := pol.GetPscConnections()[0].GetConsumerForwardingRule()
	frName := fr[len("projects/"+p+"/regions/us-central1/forwardingRules/"):]
	rule, err := rules.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: "us-central1", ForwardingRule: frName})
	if err != nil || rule.GetPscConnectionStatus() != "ACCEPTED" || rule.GetIPAddress() != a.IpAddress || rule.GetSubnetwork() == "" {
		t.Fatalf("the endpoint's forwarding rule: %v, %v", rule, err)
	}
	if _, err := rules.Delete(ctx, &computepb.DeleteForwardingRuleRequest{Project: p, Region: "us-central1", ForwardingRule: frName}); computeCode(err) != 400 {
		t.Errorf("deleting an automated endpoint: %v; want 400", err)
	}
	if _, err := c.DeleteServiceConnectionPolicy(ctx, &ncpb.DeleteServiceConnectionPolicyRequest{Name: polA}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("deleting a policy with a connection: %v; want FAILED_PRECONDITION", err)
	}
	// A policy for the second network: its endpoint is made.
	mkPolicy("ct-sql-b-"+suffix, netB, subB)
	if b := auto()[1]; b.Status != "ACTIVE" || b.ConsumerNetworkStatus != "VALID" || b.IpAddress != "10.161.0.2" {
		t.Errorf("after the second network's policy: %+v", b)
	}
	// The instance's deletion takes its endpoints, and the policy goes.
	d, err := svc.Instances.Delete(p, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, p, d)
	deleted = true
	if _, err := rules.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: "us-central1", ForwardingRule: frName}); computeCode(err) != 404 {
		t.Errorf("the endpoint after the instance's deletion: %v; want 404", err)
	}
	dop, err := c.DeleteServiceConnectionPolicy(ctx, &ncpb.DeleteServiceConnectionPolicyRequest{Name: polA})
	if err != nil {
		t.Fatalf("delete the policy: %v", err)
	}
	_ = dop.Wait(ctx)
}

// TestNetworkConnectivityPoliciesThroughTheRESTClient (#2141), the
// official discovery client (networkconnectivity/v1), JSON on the Compute
// Engine port: a policy created, read, listed, patched (its limit) and
// deleted, each operation read back done, and a second policy for the
// network and class refused with 409.
// The REST methods are the gRPC ones' JSON bindings, the page's rows.
// covers: google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/CreateServiceConnectionPolicy, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/GetServiceConnectionPolicy, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/ListServiceConnectionPolicies, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/UpdateServiceConnectionPolicy, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/DeleteServiceConnectionPolicy
func TestNetworkConnectivityPoliciesThroughTheRESTClient(t *testing.T) {
	h := New(t)
	addr := sqlComputeEndpoint(h)
	svc, err := ncrest.NewService(h.Context(), option.WithEndpoint("http://"+addr+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	p := h.Project()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	network, subnet := ncNetwork(t, h, "ct-ncr-"+suffix, "10.152.0.0/28")
	parent := "projects/" + p + "/locations/us-central1"
	pols := svc.Projects.Locations.ServiceConnectionPolicies
	pol := &ncrest.ServiceConnectionPolicy{Network: network, ServiceClass: "ct-rest-" + suffix,
		PscConfig: &ncrest.PscConfig{Subnetworks: []string{subnet}, Limit: 3, ForceSendFields: []string{"Limit"}}}
	op, err := pols.Create(parent, pol).ServiceConnectionPolicyId("pol-" + suffix).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Projects.Locations.Operations.Get(op.Name).Do(); err != nil || !got.Done {
		t.Fatalf("the create operation: %v, %v", got, err)
	}
	name := parent + "/serviceConnectionPolicies/pol-" + suffix
	got, err := pols.Get(name).Do()
	if err != nil || got.Infrastructure != "PSC" || got.PscConfig.Limit != 3 || got.Etag == "" {
		t.Fatalf("get: %+v, %v", got, err)
	}
	if _, err := pols.Create(parent, pol).ServiceConnectionPolicyId("dup-" + suffix).Do(); googleCode(err) != 409 {
		t.Errorf("a second policy for the network and class: %v; want 409", err)
	}
	ls, err := pols.List(parent).Do()
	if err != nil || len(ls.ServiceConnectionPolicies) == 0 {
		t.Fatalf("list: %v, %v", ls, err)
	}
	if _, err := pols.Patch(name, &ncrest.ServiceConnectionPolicy{PscConfig: &ncrest.PscConfig{Limit: 5}}).UpdateMask("pscConfig.limit").Do(); err != nil {
		t.Fatal(err)
	}
	if got, _ := pols.Get(name).Do(); got == nil || got.PscConfig.Limit != 5 {
		t.Errorf("after patch: %+v", got)
	}
	if _, err := pols.Delete(name).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := pols.Get(name).Do(); googleCode(err) != 404 {
		t.Errorf("get after delete: %v; want 404", err)
	}
}

func googleCode(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}
