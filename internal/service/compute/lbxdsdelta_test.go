package compute

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestDeltaXDS drives the incremental protocol with Envoy's generated ADS
// client: a wildcard cluster subscription, a listener and endpoints by
// name, an ACK that needs no answer, a change pushed as only the changed
// resource, a removal, and a reconnect with the versions held.
func TestDeltaXDS(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	z := "zones/us-central1-a/"
	lbCall(t, s, "POST", z+"networkEndpointGroups", obj{"name": "grpc", "networkEndpointType": "NON_GCP_PRIVATE_IP_PORT",
		"network": "global/networks/default", "defaultPort": 9000})
	lbCall(t, s, "POST", z+"networkEndpointGroups/grpc/attachNetworkEndpoints", obj{"networkEndpoints": []any{
		obj{"ipAddress": "10.0.0.7", "port": 9000}}})
	lbCall(t, s, "POST", "global/healthChecks", obj{"name": "ghc", "type": "GRPC", "grpcHealthCheck": obj{"portSpecification": "USE_SERVING_PORT"}})
	for _, n := range []string{"grpc", "grpc2"} {
		lbCall(t, s, "POST", "global/backendServices", obj{"name": n, "protocol": "GRPC", "loadBalancingScheme": "INTERNAL_SELF_MANAGED",
			"healthChecks": []any{"global/healthChecks/ghc"},
			"backends":     []any{obj{"group": z + "networkEndpointGroups/grpc", "balancingMode": "RATE", "maxRatePerEndpoint": 100}}})
	}
	lbCall(t, s, "POST", "global/urlMaps", obj{"name": "grpc", "defaultService": "global/backendServices/grpc",
		"hostRules":    []any{obj{"hosts": []any{"health.example"}, "pathMatcher": "pm"}},
		"pathMatchers": []any{obj{"name": "pm", "defaultService": "global/backendServices/grpc"}}})
	lbCall(t, s, "POST", "global/targetGrpcProxies", obj{"name": "grpc", "urlMap": "global/urlMaps/grpc", "validateForProxyless": true})
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": "grpc", "target": "global/targetGrpcProxies/grpc",
		"portRange": "8000", "loadBalancingScheme": "INTERNAL_SELF_MANAGED", "network": "global/networks/default"})

	xl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var protos http.Protocols
	protos.SetUnencryptedHTTP2(true)
	xs := &http.Server{Handler: s.XDSHandler(), Protocols: &protos}
	go func() { _ = xs.Serve(xl) }()
	t.Cleanup(func() { _ = xs.Close() })
	cc, err := grpc.NewClient(xl.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	node := &corev3.Node{Id: "projects/p/networks/default/nodes/delta"}
	stream, err := discoveryv3.NewAggregatedDiscoveryServiceClient(cc).DeltaAggregatedResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recv := func() *discoveryv3.DeltaDiscoveryResponse {
		t.Helper()
		r, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	names := func(r *discoveryv3.DeltaDiscoveryResponse) map[string]string {
		m := map[string]string{}
		for _, res := range r.GetResources() {
			m[res.GetName()] = res.GetVersion()
		}
		return m
	}

	// The wildcard: every INTERNAL_SELF_MANAGED backend service.
	if err := stream.Send(&discoveryv3.DeltaDiscoveryRequest{Node: node, TypeUrl: typeCluster}); err != nil {
		t.Fatal(err)
	}
	r := recv()
	if r.GetTypeUrl() != typeCluster || len(r.GetResources()) != 2 {
		t.Fatalf("wildcard CDS: %v", r)
	}
	var c clusterv3.Cluster
	if err := r.GetResources()[0].GetResource().UnmarshalTo(&c); err != nil || c.GetName() != r.GetResources()[0].GetName() {
		t.Fatalf("cluster: %v %v", err, c.GetName())
	}
	held := names(r)
	if err := stream.Send(&discoveryv3.DeltaDiscoveryRequest{TypeUrl: typeCluster, ResponseNonce: r.GetNonce()}); err != nil {
		t.Fatal(err)
	}

	// A listener by name, and one that does not exist: reported removed.
	if err := stream.Send(&discoveryv3.DeltaDiscoveryRequest{TypeUrl: typeListener,
		ResourceNamesSubscribe: []string{"health.example:8000", "nothing.example:9"}}); err != nil {
		t.Fatal(err)
	}
	r = recv()
	if r.GetTypeUrl() != typeListener || len(r.GetResources()) != 1 || r.GetResources()[0].GetName() != "health.example:8000" ||
		len(r.GetRemovedResources()) != 1 || r.GetRemovedResources()[0] != "nothing.example:9" {
		t.Fatalf("LDS: %v", r)
	}
	if err := stream.Send(&discoveryv3.DeltaDiscoveryRequest{TypeUrl: typeListener, ResponseNonce: r.GetNonce()}); err != nil {
		t.Fatal(err)
	}

	// A change to one backend service: only its cluster is pushed.
	lbCall(t, s, "PATCH", "global/backendServices/grpc2", obj{"localityLbPolicy": "LEAST_REQUEST"})
	r = recv()
	if r.GetTypeUrl() != typeCluster || len(r.GetResources()) != 1 {
		t.Fatalf("pushed CDS change: %v", r)
	}
	changed := r.GetResources()[0]
	if held[changed.GetName()] == "" || held[changed.GetName()] == changed.GetVersion() {
		t.Fatalf("changed cluster %s: version %s, held %v", changed.GetName(), changed.GetVersion(), held)
	}
	held[changed.GetName()] = changed.GetVersion()
	if err := stream.Send(&discoveryv3.DeltaDiscoveryRequest{TypeUrl: typeCluster, ResponseNonce: r.GetNonce()}); err != nil {
		t.Fatal(err)
	}

	// A deleted backend service: named in removed_resources.
	lbCall(t, s, "DELETE", "global/backendServices/grpc2", nil)
	r = recv()
	if r.GetTypeUrl() != typeCluster || len(r.GetResources()) != 0 || len(r.GetRemovedResources()) != 1 ||
		r.GetRemovedResources()[0] != changed.GetName() {
		t.Fatalf("removal: %v", r)
	}
	delete(held, changed.GetName())
	_ = stream.CloseSend()
	cancel()

	// A reconnect with the versions held: nothing to send for clusters
	// until one changes; endpoints by name arrive.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	stream, err = discoveryv3.NewAggregatedDiscoveryServiceClient(cc).DeltaAggregatedResources(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&discoveryv3.DeltaDiscoveryRequest{Node: node, TypeUrl: typeCluster, InitialResourceVersions: held,
		ResourceNamesSubscribe: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	var cla string
	for n := range held {
		cla = n
	}
	if err := stream.Send(&discoveryv3.DeltaDiscoveryRequest{TypeUrl: typeEndpoint, ResourceNamesSubscribe: []string{cla}}); err != nil {
		t.Fatal(err)
	}
	r = recv()
	if r.GetTypeUrl() != typeEndpoint || len(r.GetResources()) != 1 || r.GetResources()[0].GetName() != cla {
		t.Fatalf("after reconnect, want only EDS (clusters are current): %v", r)
	}
	// Unsubscribing drops the name; nothing more is pushed for it.
	if err := stream.Send(&discoveryv3.DeltaDiscoveryRequest{TypeUrl: typeEndpoint, ResourceNamesUnsubscribe: []string{cla}}); err != nil {
		t.Fatal(err)
	}
}
