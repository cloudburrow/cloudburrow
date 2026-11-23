//go:build compat

package compat

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	networksecurity "cloud.google.com/go/networksecurity/apiv1"
	"cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	xdscreds "google.golang.org/grpc/credentials/xds"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/xds"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestDryRunCustomMetricInCloudMonitoring (#2160): a backend of balancing
// mode CUSTOM_METRICS with a dry-run metric "queue" beside the metric it
// balances on, made with the official compute/apiv1 client; the value its
// origin reports in its ORCA header is written to Cloud Monitoring as
// network.googleapis.com/loadbalancer/backend/lb_custom_metric, which the
// official monitoring/apiv3 client lists with dry_run "true".
func TestDryRunCustomMetricInCloudMonitoring(t *testing.T) {
	h := New(t)
	mc := monitoringClient(t, h) // skips without Cloud Monitoring
	l := newLB2160(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Endpoint-Load-Metrics", "TEXT named_metrics.orders=0.2, named_metrics.queue=0.6")
		_, _ = io.WriteString(w, "ok")
	}))
	op, err := l.bss.Patch(l.ctx, &computepb.PatchBackendServiceRequest{Project: l.project, BackendService: l.n("app"),
		BackendServiceResource: &computepb.BackendService{Backends: []*computepb.Backend{
			{Group: proto.String("zones/us-central1-a/networkEndpointGroups/" + l.n("origin")), BalancingMode: proto.String("CUSTOM_METRICS"),
				CustomMetrics: []*computepb.BackendCustomMetric{{Name: proto.String("orders"), MaxUtilization: proto.Float32(0.8)},
					{Name: proto.String("queue"), DryRun: proto.Bool(true)}}}}}})
	l.do("CUSTOM_METRICS with a dry-run metric", op, err)
	filter := `metric.type = "network.googleapis.com/loadbalancer/backend/lb_custom_metric" AND metric.labels.dry_run = "true" AND ` +
		`metric.labels.custom_metric_name = "queue" AND metric.labels.backend_name = "` + l.n("origin") + `"`
	var found *monitoringpb.TimeSeries
	waitFor(t, "the dry-run metric in Cloud Monitoring", 4*time.Minute, func() bool {
		_, _, _ = l.get("/", nil)
		now := time.Now()
		it := mc.ListTimeSeries(l.ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + l.project, Filter: filter,
			Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(now.Add(-time.Hour)), EndTime: timestamppb.New(now)}})
		ts, err := it.Next()
		if err != nil {
			if err != iterator.Done {
				t.Logf("ListTimeSeries: %v", err)
			}
			return false
		}
		found = ts
		return true
	})
	if found.GetResource().GetType() != "gce_backend_service" || len(found.GetPoints()) == 0 ||
		found.GetPoints()[0].GetValue().GetDoubleValue() != 0.6 || found.GetMetric().GetLabels()["backend_scope"] != "us-central1-a" {
		t.Fatalf("series: %v", found)
	}
}

// covers: compute.backendServices.insert
//
// TestProxylessGRPCClientTLSOverXDSOverTLS (#2160), as
// service-mesh/docs/service-routing/security-proxyless-setup sets a client
// up: the bootstrap from the Compute Engine port names the xDS API over
// TLS (tls channel credentials) and the google_cloud_private_spiffe
// file_watcher certificate provider; a client TLS policy made with the
// official networksecurity/apiv1 client names it, and a backend service
// made with compute/apiv1 names the policy and the server's SPIFFE ID in
// securitySettings. A google.golang.org/grpc/xds client with xDS
// credentials then calls a server that requires a client certificate
// from the workload CA, over mutual TLS with the workload's identity.
func TestProxylessGRPCClientTLSOverXDSOverTLS(t *testing.T) {
	h := New(t)
	l := newLB2160(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	ctx, opts := l.ctx, computeOpts(t, h)

	// The bootstrap, as GRPC_XDS_BOOTSTRAP would hold it.
	resp, err := http.Get("http://" + l.compute + "/cloudburrow/xds/bootstrap?project=" + l.project + "&network=default")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var boot struct {
		XDSServers []struct {
			ServerURI    string `json:"server_uri"`
			ChannelCreds []struct {
				Type   string            `json:"type"`
				Config map[string]string `json:"config"`
			} `json:"channel_creds"`
		} `json:"xds_servers"`
		CertificateProviders map[string]struct {
			PluginName string            `json:"plugin_name"`
			Config     map[string]string `json:"config"`
		} `json:"certificate_providers"`
	}
	if err := json.Unmarshal(raw, &boot); err != nil || len(boot.XDSServers) != 1 || boot.XDSServers[0].ChannelCreds[0].Type != "tls" {
		t.Fatalf("bootstrap: %s %v", raw, err)
	}
	cp, ok := boot.CertificateProviders["google_cloud_private_spiffe"]
	if !ok || cp.PluginName != "file_watcher" {
		t.Fatalf("certificate_providers: %s", raw)
	}

	// The server: plain TLS with the workload's credentials, requiring a
	// client certificate the workload CA signed.
	kp, err := tls.LoadX509KeyPair(cp.Config["certificate_file"], cp.Config["private_key_file"])
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(cp.Config["ca_certificate_file"])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	spiffe := kp.Leaf.URIs[0].String()
	var peerID string
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{kp}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert})),
		grpc.UnaryInterceptor(func(c context.Context, req any, info *grpc.UnaryServerInfo, hd grpc.UnaryHandler) (any, error) {
			if p, ok := peer.FromContext(c); ok {
				if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.PeerCertificates) > 0 && len(ti.State.PeerCertificates[0].URIs) > 0 {
					peerID = ti.State.PeerCertificates[0].URIs[0].String()
				}
			}
			return hd(c, req)
		}))
	hs := health.NewServer()
	healthpb.RegisterHealthServer(gs, hs)
	gl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(gl) }()
	t.Cleanup(gs.Stop)
	gport := gl.Addr().(*net.TCPAddr).Port

	ns, err := networksecurity.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	inst := &networksecuritypb.CertificateProviderInstance{PluginInstance: "google_cloud_private_spiffe"}
	cop, err := ns.CreateClientTlsPolicy(ctx, &networksecuritypb.CreateClientTlsPolicyRequest{Parent: "projects/" + l.project + "/locations/global",
		ClientTlsPolicyId: l.n("client-mtls"), ClientTlsPolicy: &networksecuritypb.ClientTlsPolicy{
			ClientCertificate:  &networksecuritypb.CertificateProvider{Type: &networksecuritypb.CertificateProvider_CertificateProviderInstance{CertificateProviderInstance: inst}},
			ServerValidationCa: []*networksecuritypb.ValidationCA{{Type: &networksecuritypb.ValidationCA_CertificateProviderInstance{CertificateProviderInstance: inst}}}}})
	if err != nil {
		t.Fatal(err)
	}
	ctp, err := cop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}

	negs, _ := computeapi.NewNetworkEndpointGroupsRESTClient(ctx, opts...)
	hcs, _ := computeapi.NewHealthChecksRESTClient(ctx, opts...)
	tgps, _ := computeapi.NewTargetGrpcProxiesRESTClient(ctx, opts...)
	gfrs, _ := computeapi.NewGlobalForwardingRulesRESTClient(ctx, opts...)
	zone := "us-central1-a"
	op, err := negs.Insert(ctx, &computepb.InsertNetworkEndpointGroupRequest{Project: l.project, Zone: zone,
		NetworkEndpointGroupResource: &computepb.NetworkEndpointGroup{Name: proto.String(l.n("gsrv")), NetworkEndpointType: proto.String("NON_GCP_PRIVATE_IP_PORT"),
			Network: proto.String("global/networks/default"), DefaultPort: proto.Int32(int32(gport))}})
	l.do("the gRPC server's group", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return negs.Delete(c, &computepb.DeleteNetworkEndpointGroupRequest{Project: l.project, Zone: zone, NetworkEndpointGroup: l.n("gsrv")})
	})
	op, err = negs.AttachNetworkEndpoints(ctx, &computepb.AttachNetworkEndpointsNetworkEndpointGroupRequest{Project: l.project, Zone: zone,
		NetworkEndpointGroup: l.n("gsrv"), NetworkEndpointGroupsAttachEndpointsRequestResource: &computepb.NetworkEndpointGroupsAttachEndpointsRequest{
			NetworkEndpoints: []*computepb.NetworkEndpoint{{IpAddress: proto.String("127.0.0.1"), Port: proto.Int32(int32(gport))}}}})
	l.do("the gRPC server", op, err)
	op, err = hcs.Insert(ctx, &computepb.InsertHealthCheckRequest{Project: l.project, HealthCheckResource: &computepb.HealthCheck{
		Name: proto.String(l.n("thc")), Type: proto.String("TCP"), CheckIntervalSec: proto.Int32(1), TimeoutSec: proto.Int32(1), HealthyThreshold: proto.Int32(1),
		TcpHealthCheck: &computepb.TCPHealthCheck{PortSpecification: proto.String("USE_SERVING_PORT")}}})
	l.do("the TCP health check", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return hcs.Delete(c, &computepb.DeleteHealthCheckRequest{Project: l.project, HealthCheck: l.n("thc")})
	})
	op, err = l.bss.Insert(ctx, &computepb.InsertBackendServiceRequest{Project: l.project, BackendServiceResource: &computepb.BackendService{
		Name: proto.String(l.n("gsec")), Protocol: proto.String("GRPC"), LoadBalancingScheme: proto.String("INTERNAL_SELF_MANAGED"),
		HealthChecks: []string{"global/healthChecks/" + l.n("thc")},
		SecuritySettings: &computepb.SecuritySettings{ClientTlsPolicy: proto.String("//networksecurity.googleapis.com/" + ctp.GetName()),
			SubjectAltNames: []string{spiffe}},
		Backends: []*computepb.Backend{{Group: proto.String("zones/" + zone + "/networkEndpointGroups/" + l.n("gsrv")),
			BalancingMode: proto.String("RATE"), MaxRatePerEndpoint: proto.Float32(100)}}}})
	l.do("the backend service with a client TLS policy", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return l.bss.Delete(c, &computepb.DeleteBackendServiceRequest{Project: l.project, BackendService: l.n("gsec")})
	})
	op, err = l.ums.Insert(ctx, &computepb.InsertUrlMapRequest{Project: l.project, UrlMapResource: &computepb.UrlMap{
		Name: proto.String(l.n("gsmap")), DefaultService: proto.String("global/backendServices/" + l.n("gsec"))}})
	l.do("the URL map", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return l.ums.Delete(c, &computepb.DeleteUrlMapRequest{Project: l.project, UrlMap: l.n("gsmap")})
	})
	op, err = tgps.Insert(ctx, &computepb.InsertTargetGrpcProxyRequest{Project: l.project, TargetGrpcProxyResource: &computepb.TargetGrpcProxy{
		Name: proto.String(l.n("gsproxy")), UrlMap: proto.String("global/urlMaps/" + l.n("gsmap")), ValidateForProxyless: proto.Bool(true)}})
	l.do("the target gRPC proxy", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return tgps.Delete(c, &computepb.DeleteTargetGrpcProxyRequest{Project: l.project, TargetGrpcProxy: l.n("gsproxy")})
	})
	port := 30000 + time.Now().Nanosecond()%9000
	op, err = gfrs.Insert(ctx, &computepb.InsertGlobalForwardingRuleRequest{Project: l.project, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(l.n("gsrule")), Target: proto.String("global/targetGrpcProxies/" + l.n("gsproxy")), PortRange: proto.String(strconv.Itoa(port)),
		LoadBalancingScheme: proto.String("INTERNAL_SELF_MANAGED"), IPAddress: proto.String("0.0.0.0"), Network: proto.String("global/networks/default")}})
	l.do("the forwarding rule", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return gfrs.Delete(c, &computepb.DeleteGlobalForwardingRuleRequest{Project: l.project, ForwardingRule: l.n("gsrule")})
	})

	rb, err := xds.NewXDSResolverWithConfigForTesting(raw)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := xdscreds.NewClientCredentials(xdscreds.ClientOptions{FallbackCreds: insecure.NewCredentials()})
	if err != nil {
		t.Fatal(err)
	}
	cc, err := grpc.NewClient("xds:///"+l.n("secure")+".example:"+strconv.Itoa(port), grpc.WithTransportCredentials(creds), grpc.WithResolvers(rb))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	var last error
	waitFor(t, "a mutual TLS call through xDS over TLS", 2*time.Minute, func() bool {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, last = healthpb.NewHealthClient(cc).Check(c, &healthpb.HealthCheckRequest{})
		return last == nil
	})
	if peerID != spiffe {
		t.Fatalf("the server saw client identity %q, want %q (last error %v)", peerID, spiffe, last)
	}
}
