//go:build compat

package compat

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// covers: compute.backendServices.patch, compute.backendServices.get
//
// TestCustomMetricsBalancingThroughTheGoClient (#2160), the official
// compute/apiv1 REST clients, as
// load-balancing/docs/https/applb-custom-metrics sets it up: two hybrid
// backends of balancing mode CUSTOM_METRICS on the metric "orders" with
// maxUtilization 0.5, whose origins report it in Google's ORCA header
// (endpoint-load-metrics: TEXT named_metrics.orders=...); once the busy
// one has reported 0.9 it gets no more requests through the load
// balancer while the idle one (0.1) has room.
func TestCustomMetricsBalancingThroughTheGoClient(t *testing.T) {
	h := New(t)
	l := newLB2160(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Endpoint-Load-Metrics", "TEXT named_metrics.orders=0.1")
		_, _ = io.WriteString(w, "idle")
	}))
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Endpoint-Load-Metrics", "TEXT named_metrics.orders=0.9")
		_, _ = io.WriteString(w, "busy")
	}))
	t.Cleanup(busy.Close)
	host, port, _ := net.SplitHostPort(busy.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	opts := computeOpts(t, h)
	negs, err := computeapi.NewNetworkEndpointGroupsRESTClient(l.ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	zone := "us-central1-b"
	op, err := negs.Insert(l.ctx, &computepb.InsertNetworkEndpointGroupRequest{Project: l.project, Zone: zone,
		NetworkEndpointGroupResource: &computepb.NetworkEndpointGroup{Name: proto.String(l.n("busy")),
			NetworkEndpointType: proto.String("NON_GCP_PRIVATE_IP_PORT"), Network: proto.String("global/networks/default"),
			DefaultPort: proto.Int32(int32(p))}})
	l.do("the busy group", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return negs.Delete(c, &computepb.DeleteNetworkEndpointGroupRequest{Project: l.project, Zone: zone, NetworkEndpointGroup: l.n("busy")})
	})
	op, err = negs.AttachNetworkEndpoints(l.ctx, &computepb.AttachNetworkEndpointsNetworkEndpointGroupRequest{Project: l.project, Zone: zone,
		NetworkEndpointGroup: l.n("busy"), NetworkEndpointGroupsAttachEndpointsRequestResource: &computepb.NetworkEndpointGroupsAttachEndpointsRequest{
			NetworkEndpoints: []*computepb.NetworkEndpoint{{IpAddress: proto.String(host), Port: proto.Int32(int32(p))}}}})
	l.do("the busy endpoint", op, err)
	metrics := []*computepb.BackendCustomMetric{{Name: proto.String("orders"), MaxUtilization: proto.Float32(0.5)}}
	op, err = l.bss.Patch(l.ctx, &computepb.PatchBackendServiceRequest{Project: l.project, BackendService: l.n("app"),
		BackendServiceResource: &computepb.BackendService{Backends: []*computepb.Backend{
			{Group: proto.String("zones/us-central1-a/networkEndpointGroups/" + l.n("origin")), BalancingMode: proto.String("CUSTOM_METRICS"), CustomMetrics: metrics},
			{Group: proto.String("zones/" + zone + "/networkEndpointGroups/" + l.n("busy")), BalancingMode: proto.String("CUSTOM_METRICS"), CustomMetrics: metrics},
		}}})
	l.do("CUSTOM_METRICS on the backends", op, err)
	bs, err := l.bss.Get(l.ctx, &computepb.GetBackendServiceRequest{Project: l.project, BackendService: l.n("app")})
	if err != nil {
		t.Fatal(err)
	}
	for _, be := range bs.GetBackends() {
		if be.GetBalancingMode() != "CUSTOM_METRICS" || len(be.GetCustomMetrics()) != 1 || be.GetCustomMetrics()[0].GetMaxUtilization() != 0.5 {
			t.Fatalf("backend: %v", be)
		}
	}
	// A backend of CUSTOM_METRICS with a maxRate is refused.
	_, err = l.bss.Patch(l.ctx, &computepb.PatchBackendServiceRequest{Project: l.project, BackendService: l.n("app"),
		BackendServiceResource: &computepb.BackendService{Backends: []*computepb.Backend{
			{Group: proto.String("zones/us-central1-a/networkEndpointGroups/" + l.n("origin")), BalancingMode: proto.String("CUSTOM_METRICS"),
				CustomMetrics: metrics, MaxRate: proto.Int32(10)}}}})
	if err == nil {
		t.Fatal("CUSTOM_METRICS with maxRate: accepted")
	}
	seen := map[string]int{}
	waitFor(t, "both backends reporting", 2*time.Minute, func() bool {
		_, body, _ := l.get("/", nil)
		seen[body]++
		return seen["busy"] > 0 && seen["idle"] > 0
	})
	seen = map[string]int{}
	for i := 0; i < 40; i++ {
		_, body, _ := l.get("/", nil)
		seen[body]++
	}
	if seen["busy"] != 0 || seen["idle"] != 40 {
		t.Fatalf("after the reports: %v (the busy backend is over its maxUtilization)", seen)
	}
}

// covers: compute.targetGrpcProxies.insert, compute.targetGrpcProxies.delete
//
// TestDeltaXDSThroughTheGoClient (#2160): a target gRPC proxy made with
// the official compute/apiv1 client, and its listener, cluster and
// endpoints read over the incremental (delta) Aggregated Discovery Service
// on the Compute Engine port with Envoy's generated client, as an Envoy
// or a delta-capable gRPC client would.
func TestDeltaXDSThroughTheGoClient(t *testing.T) {
	h := New(t)
	l := newLB2160(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	opts := computeOpts(t, h)
	bss, umsc := l.bss, l.ums
	hcs, _ := computeapi.NewHealthChecksRESTClient(l.ctx, opts...)
	tgps, err := computeapi.NewTargetGrpcProxiesRESTClient(l.ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	gfrs, _ := computeapi.NewGlobalForwardingRulesRESTClient(l.ctx, opts...)
	op, err := hcs.Insert(l.ctx, &computepb.InsertHealthCheckRequest{Project: l.project, HealthCheckResource: &computepb.HealthCheck{
		Name: proto.String(l.n("ghc")), Type: proto.String("GRPC"),
		GrpcHealthCheck: &computepb.GRPCHealthCheck{PortSpecification: proto.String("USE_SERVING_PORT")}}})
	l.do("the gRPC health check", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return hcs.Delete(c, &computepb.DeleteHealthCheckRequest{Project: l.project, HealthCheck: l.n("ghc")})
	})
	op, err = bss.Insert(l.ctx, &computepb.InsertBackendServiceRequest{Project: l.project, BackendServiceResource: &computepb.BackendService{
		Name: proto.String(l.n("grpc")), Protocol: proto.String("GRPC"), LoadBalancingScheme: proto.String("INTERNAL_SELF_MANAGED"),
		LocalityLbPolicy: proto.String("WEIGHTED_ROUND_ROBIN"), HealthChecks: []string{"global/healthChecks/" + l.n("ghc")},
		Backends: []*computepb.Backend{{Group: proto.String("zones/us-central1-a/networkEndpointGroups/" + l.n("origin")),
			BalancingMode: proto.String("RATE"), MaxRatePerEndpoint: proto.Float32(100)}}}})
	l.do("the gRPC backend service", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return bss.Delete(c, &computepb.DeleteBackendServiceRequest{Project: l.project, BackendService: l.n("grpc")})
	})
	op, err = umsc.Insert(l.ctx, &computepb.InsertUrlMapRequest{Project: l.project, UrlMapResource: &computepb.UrlMap{
		Name: proto.String(l.n("gmap")), DefaultService: proto.String("global/backendServices/" + l.n("grpc")),
		HostRules:    []*computepb.HostRule{{Hosts: []string{l.n("svc") + ".example"}, PathMatcher: proto.String("pm")}},
		PathMatchers: []*computepb.PathMatcher{{Name: proto.String("pm"), DefaultService: proto.String("global/backendServices/" + l.n("grpc"))}}}})
	l.do("the gRPC URL map", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return umsc.Delete(c, &computepb.DeleteUrlMapRequest{Project: l.project, UrlMap: l.n("gmap")})
	})
	op, err = tgps.Insert(l.ctx, &computepb.InsertTargetGrpcProxyRequest{Project: l.project, TargetGrpcProxyResource: &computepb.TargetGrpcProxy{
		Name: proto.String(l.n("gproxy")), UrlMap: proto.String("global/urlMaps/" + l.n("gmap")), ValidateForProxyless: proto.Bool(true)}})
	l.do("the target gRPC proxy", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return tgps.Delete(c, &computepb.DeleteTargetGrpcProxyRequest{Project: l.project, TargetGrpcProxy: l.n("gproxy")})
	})
	port := 20000 + time.Now().Nanosecond()%10000
	op, err = gfrs.Insert(l.ctx, &computepb.InsertGlobalForwardingRuleRequest{Project: l.project, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(l.n("grule")), Target: proto.String("global/targetGrpcProxies/" + l.n("gproxy")), PortRange: proto.String(strconv.Itoa(port)),
		LoadBalancingScheme: proto.String("INTERNAL_SELF_MANAGED"), IPAddress: proto.String("0.0.0.0"), Network: proto.String("global/networks/default")}})
	l.do("the gRPC forwarding rule", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return gfrs.Delete(c, &computepb.DeleteGlobalForwardingRuleRequest{Project: l.project, ForwardingRule: l.n("grule")})
	})

	cc, err := grpc.NewClient(l.compute, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(l.ctx, time.Minute)
	defer cancel()
	st, err := discoveryv3.NewAggregatedDiscoveryServiceClient(cc).DeltaAggregatedResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listener := fmt.Sprintf("%s.example:%d", l.n("svc"), port)
	if err := st.Send(&discoveryv3.DeltaDiscoveryRequest{TypeUrl: "type.googleapis.com/envoy.config.listener.v3.Listener",
		ResourceNamesSubscribe: []string{listener}}); err != nil {
		t.Fatal(err)
	}
	r, err := st.Recv()
	if err != nil {
		t.Fatalf("delta LDS: %v", err)
	}
	if len(r.GetResources()) != 1 || r.GetResources()[0].GetName() != listener || r.GetResources()[0].GetVersion() == "" {
		t.Fatalf("delta LDS: %v", r)
	}
	// Every cluster, by the wildcard; ours uses weighted round robin.
	if err := st.Send(&discoveryv3.DeltaDiscoveryRequest{TypeUrl: "type.googleapis.com/envoy.config.cluster.v3.Cluster", ResourceNamesSubscribe: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	r, err = st.Recv()
	if err != nil || len(r.GetResources()) == 0 {
		t.Fatalf("delta CDS: %v %v", r, err)
	}
	found := false
	for _, res := range r.GetResources() {
		if strings.Contains(string(res.GetResource().GetValue()), "client_side_weighted_round_robin") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no cluster with client_side_weighted_round_robin: %v", r)
	}
}
