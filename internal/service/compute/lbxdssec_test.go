package compute

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	xdscreds "google.golang.org/grpc/credentials/xds"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/xds"
)

// fakeXDSNetSec is Network Security and Network Services with one client
// TLS policy, one server TLS policy (mTLS) and one GRPC_SERVER endpoint
// policy taking servers labelled app=hello on port.
type fakeXDSNetSec struct {
	fakeNetSec
	port string
}

func (f *fakeXDSNetSec) XDSClientTLS(name string) (XDSClientTLS, bool) {
	if name != "projects/p/locations/global/clientTlsPolicies/client-mtls" {
		return XDSClientTLS{}, false
	}
	return XDSClientTLS{CertInstance: XDSCertProviderInstance, CAInstances: []string{XDSCertProviderInstance}}, true
}

func (f *fakeXDSNetSec) XDSServerTLS(name string) (XDSServerTLS, bool) {
	if name != "projects/p/locations/global/serverTlsPolicies/server-mtls" {
		return XDSServerTLS{}, false
	}
	return XDSServerTLS{CertInstance: XDSCertProviderInstance, MTLS: true, CAInstances: []string{XDSCertProviderInstance}}, true
}

func (f *fakeXDSNetSec) XDSEndpointPolicies(project string) []XDSEndpointPolicy {
	if project != "p" {
		return nil
	}
	return []XDSEndpointPolicy{{Name: "projects/p/locations/global/endpointPolicies/ep", Type: "GRPC_SERVER", MatchAll: true,
		Labels: map[string]string{"app": "hello"}, Ports: []string{f.port}, ServerTLSPolicy: "projects/p/locations/global/serverTlsPolicies/server-mtls"}}
}

// TestProxylessGRPCMutualTLSOverXDSOverTLS (#2160): the xDS API reached
// over TLS (the bootstrap's tls channel credentials), a backend service's
// client TLS policy delivered to the client and an endpoint policy's
// server TLS policy to an xDS-enabled server, both with the
// google_cloud_private_spiffe file_watcher credentials, so the call is
// mutual TLS with the workload's SPIFFE identity; a client without xDS
// credentials is refused by the server.
func TestProxylessGRPCMutualTLSOverXDSOverTLS(t *testing.T) {
	dir := t.TempDir()
	ns := &fakeXDSNetSec{}
	s, err := New(Options{VMs: fakeVMs{}, NetSec: ns, XDSCredentialDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	// The xDS API over TLS, as the Compute Engine port takes it.
	cfg, err := s.XDSServerTLS(nil)
	if err != nil {
		t.Fatal(err)
	}
	xs := httptest.NewUnstartedServer(s)
	xs.TLS = cfg
	xs.EnableHTTP2 = true
	xs.StartTLS()
	t.Cleanup(xs.Close)
	xaddr := strings.TrimPrefix(xs.URL, "https://")

	// The bootstrap, from the port's own endpoint.
	bootstrap := func(meta string) []byte {
		u := xs.URL + XDSBootstrapPath + "?project=p&network=default"
		if meta != "" {
			u += "&node-metadata=" + meta
		}
		resp, err := xs.Client().Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
			t.Fatal(err)
		}
		srv := b["xds_servers"].([]any)[0].(map[string]any)
		if creds := srv["channel_creds"].([]any)[0].(map[string]any); creds["type"] != "tls" {
			t.Fatalf("channel_creds: %v", creds)
		}
		cp := b["certificate_providers"].(map[string]any)[XDSCertProviderInstance].(map[string]any)
		if cp["plugin_name"] != "file_watcher" {
			t.Fatalf("certificate_providers: %v", cp)
		}
		srv["server_uri"] = xaddr
		out, _ := json.Marshal(b)
		return out
	}

	// The backend: an xDS-enabled gRPC server labelled app=hello.
	gl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gport := gl.Addr().(*net.TCPAddr).Port
	ns.port = strconv.Itoa(gport)
	screds, err := xdscreds.NewServerCredentials(xdscreds.ServerOptions{FallbackCreds: insecure.NewCredentials()})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var peers []string
	gs, err := xds.NewGRPCServer(grpc.Creds(screds), xds.BootstrapContentsForTesting(bootstrap("app=hello")),
		grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if p, ok := peer.FromContext(ctx); ok {
				if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.PeerCertificates) > 0 {
					mu.Lock()
					for _, u := range ti.State.PeerCertificates[0].URIs {
						peers = append(peers, u.String())
					}
					mu.Unlock()
				}
			}
			return h(ctx, req)
		}))
	if err != nil {
		t.Fatal(err)
	}
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(gs, hs)
	go func() { _ = gs.Serve(gl) }()
	t.Cleanup(gs.Stop)

	z := "zones/us-central1-a/"
	lbCall(t, s, "POST", z+"networkEndpointGroups", obj{"name": "grpc", "networkEndpointType": "NON_GCP_PRIVATE_IP_PORT",
		"network": "global/networks/default", "defaultPort": gport})
	lbCall(t, s, "POST", z+"networkEndpointGroups/grpc/attachNetworkEndpoints", obj{"networkEndpoints": []any{
		obj{"ipAddress": "127.0.0.1", "port": gport}}})
	lbCall(t, s, "POST", "global/healthChecks", obj{"name": "thc", "type": "TCP", "tcpHealthCheck": obj{"portSpecification": "USE_SERVING_PORT"},
		"checkIntervalSec": 1, "timeoutSec": 1, "healthyThreshold": 1})
	lbCall(t, s, "POST", "global/backendServices", obj{"name": "grpc", "protocol": "GRPC", "loadBalancingScheme": "INTERNAL_SELF_MANAGED",
		"healthChecks": []any{"global/healthChecks/thc"},
		"securitySettings": obj{"clientTlsPolicy": "projects/p/locations/global/clientTlsPolicies/client-mtls",
			"subjectAltNames": []any{xdsWorkloadID("p")}},
		"backends": []any{obj{"group": z + "networkEndpointGroups/grpc", "balancingMode": "RATE", "maxRatePerEndpoint": 100}}})
	lbCall(t, s, "POST", "global/urlMaps", obj{"name": "grpc", "defaultService": "global/backendServices/grpc"})
	lbCall(t, s, "POST", "global/targetGrpcProxies", obj{"name": "grpc", "urlMap": "global/urlMaps/grpc", "validateForProxyless": true})
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": "grpc", "target": "global/targetGrpcProxies/grpc",
		"portRange": "8000", "loadBalancingScheme": "INTERNAL_SELF_MANAGED", "network": "global/networks/default"})

	rb, err := xds.NewXDSResolverWithConfigForTesting(bootstrap(""))
	if err != nil {
		t.Fatal(err)
	}
	ccreds, err := xdscreds.NewClientCredentials(xdscreds.ClientOptions{FallbackCreds: insecure.NewCredentials()})
	if err != nil {
		t.Fatal(err)
	}
	cc, err := grpc.NewClient("xds:///hello.example:8000", grpc.WithTransportCredentials(ccreds), grpc.WithResolvers(rb))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	var last error
	waitFor(t, "a mutual TLS call through xDS over TLS", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, last = healthpb.NewHealthClient(cc).Check(ctx, &healthpb.HealthCheckRequest{})
		return last == nil
	})
	if last != nil {
		t.Fatalf("Check: %v", last)
	}
	mu.Lock()
	got := append([]string(nil), peers...)
	mu.Unlock()
	if len(got) == 0 || got[0] != xdsWorkloadID("p") {
		t.Fatalf("the server saw client certificates %v, want %s", got, xdsWorkloadID("p"))
	}
	// A plaintext client straight to the server is refused: the endpoint
	// policy requires mutual TLS.
	pc, err := grpc.NewClient(gl.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := healthpb.NewHealthClient(pc).Check(ctx, &healthpb.HealthCheckRequest{}); err == nil {
		t.Fatal("a plaintext call to the mTLS server succeeded")
	}
}

// TestXDSBootstrapInsecureStays: without a credential directory, or with
// creds=insecure, the bootstrap is the plaintext one.
func TestXDSBootstrapInsecureStays(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{}, XDSCredentialDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"?project=p&creds=insecure", ""} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest("GET", XDSBootstrapPath+q, nil))
		if !strings.Contains(rec.Body.String(), `"type":"insecure"`) || strings.Contains(rec.Body.String(), "certificate_providers") {
			t.Errorf("%s: %s", q, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", XDSBootstrapPath+"?project=p&node-metadata=app=hello&instance-ip=10.0.0.5", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"tls"`) || !strings.Contains(rec.Body.String(), `"app":"hello"`) ||
		!strings.Contains(rec.Body.String(), `"INSTANCE_IP":"10.0.0.5"`) || !strings.Contains(rec.Body.String(), "grpc/server?xds.resource.listening_address=%s") {
		t.Fatalf("TLS bootstrap: %d %s", rec.Code, rec.Body.String())
	}
}
