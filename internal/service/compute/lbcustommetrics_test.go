package compute

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	orcav3 "github.com/cncf/xds/go/xds/data/orca/v3"
	"google.golang.org/protobuf/proto"
)

func TestParseORCAFormats(t *testing.T) {
	bin, _ := proto.Marshal(&orcav3.OrcaLoadReport{CpuUtilization: 0.3, RpsFractional: 10, NamedMetrics: map[string]float64{"orders": 0.4}})
	for name, h := range map[string]http.Header{
		"text": {"Endpoint-Load-Metrics": {"TEXT cpu_utilization=0.3, mem_utilization=0.8, rps_fractional=10.0, eps=1, named_metrics.orders=0.4"}},
		"json": {"Endpoint-Load-Metrics-Json": {`JSON {"cpu_utilization": 0.3, "mem_utilization": 0.8, "rps_fractional": 10.0, "eps": 1, "named_metrics": {"orders": 0.4}}`}},
		"bin":  {"Endpoint-Load-Metrics-Bin": {base64.StdEncoding.EncodeToString(bin)}},
		"BIN":  {"Endpoint-Load-Metrics": {"BIN " + base64.StdEncoding.EncodeToString(bin)}},
	} {
		r, ok := parseORCA(h)
		if !ok || r.cpu != 0.3 || r.rps != 10 || r.named["orders"] != 0.4 {
			t.Errorf("%s: %+v %v", name, r, ok)
		}
		if v, ok := r.value("orders"); !ok || v != 0.4 {
			t.Errorf("%s: value(orders) = %v %v", name, v, ok)
		}
		if v, ok := r.value("orca.cpu_utilization"); !ok || v != 0.3 {
			t.Errorf("%s: value(orca.cpu_utilization) = %v %v", name, v, ok)
		}
	}
	if _, ok := parseORCA(http.Header{}); ok {
		t.Error("no header: a report")
	}
}

// reporting is a backend that answers with its name and an ORCA report.
func reporting(t *testing.T, name, report string) (string, int) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if report != "" {
			w.Header().Set("Endpoint-Load-Metrics", report)
		}
		_, _ = io.WriteString(w, name)
	}))
	t.Cleanup(srv.Close)
	h, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	var port int
	_, _ = fmt.Sscan(p, &port)
	return h, port
}

func customMetricsFixture(t *testing.T) *Server {
	s, err := New(Options{VMs: fakeVMs{}, LB: &fakeLB{vms: map[string]string{}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	lbCall(t, s, "POST", "global/healthChecks", obj{"name": "hc", "type": "TCP", "tcpHealthCheck": obj{"portSpecification": "USE_SERVING_PORT"},
		"checkIntervalSec": 1, "timeoutSec": 1, "healthyThreshold": 1})
	return s
}

func hybridNEG(t *testing.T, s *Server, zone, name string, eps ...[2]any) string {
	z := "zones/" + zone + "/"
	lbCall(t, s, "POST", z+"networkEndpointGroups", obj{"name": name, "networkEndpointType": "NON_GCP_PRIVATE_IP_PORT",
		"network": "global/networks/default", "defaultPort": 80})
	var l []any
	for _, e := range eps {
		l = append(l, obj{"ipAddress": e[0], "port": e[1]})
	}
	lbCall(t, s, "POST", z+"networkEndpointGroups/"+name+"/attachNetworkEndpoints", obj{"networkEndpoints": l})
	return z + "networkEndpointGroups/" + name
}

func lbFront(t *testing.T, s *Server, svc string) string {
	lbCall(t, s, "POST", "global/urlMaps", obj{"name": svc, "defaultService": "global/backendServices/" + svc})
	lbCall(t, s, "POST", "global/targetHttpProxies", obj{"name": svc, "urlMap": "global/urlMaps/" + svc})
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": svc, "target": "global/targetHttpProxies/" + svc,
		"portRange": "80", "loadBalancingScheme": "EXTERNAL_MANAGED"})
	return "http://" + str(lbCall(t, s, "GET", "global/forwardingRules/"+svc, nil), "cloudburrowAddress")
}

// TestCustomMetricsBalancingMode: the backend over its maxUtilization
// gets no traffic once its report is in, while the other has room.
func TestCustomMetricsBalancingMode(t *testing.T) {
	s := customMetricsFixture(t)
	ha, pa := reporting(t, "busy", "TEXT named_metrics.orders=0.9")
	hb, pb := reporting(t, "idle", "TEXT named_metrics.orders=0.1")
	a := hybridNEG(t, s, "us-central1-a", "a", [2]any{ha, pa})
	b := hybridNEG(t, s, "us-central1-b", "b", [2]any{hb, pb})
	cm := []any{obj{"name": "orders", "maxUtilization": 0.5}}
	lbCall(t, s, "POST", "global/backendServices", obj{"name": "cm", "protocol": "HTTP", "loadBalancingScheme": "EXTERNAL_MANAGED",
		"healthChecks": []any{"global/healthChecks/hc"},
		"backends": []any{obj{"group": a, "balancingMode": "CUSTOM_METRICS", "customMetrics": cm},
			obj{"group": b, "balancingMode": "CUSTOM_METRICS", "customMetrics": cm}}})
	bs := lbCall(t, s, "GET", "global/backendServices/cm", nil)
	be := objOf(listOf(bs["backends"])[0])
	if be["balancingMode"] != "CUSTOM_METRICS" || numOr(objOf(listOf(be["customMetrics"])[0]), "maxUtilization", 0) != 0.5 {
		t.Fatalf("backend: %v", be)
	}
	u := lbFront(t, s, "cm")
	c := &http.Client{}
	var busy int
	waitFor(t, "both backends healthy and reporting", func() bool {
		seen := map[string]bool{}
		for i := 0; i < 20; i++ {
			resp, err := c.Get(u + "/")
			if err != nil {
				return false
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			seen[string(body)] = true
		}
		return seen["busy"] || seen["idle"]
	})
	for i := 0; i < 50; i++ {
		_, body, _ := get(t, c, u+"/")
		if body == "busy" {
			busy++
		}
	}
	if busy > 0 {
		t.Fatalf("the backend over maxUtilization got %d of 50 requests", busy)
	}
}

// TestWeightedRoundRobinByORCA: endpoints are weighed by rps over
// utilization.
func TestWeightedRoundRobinByORCA(t *testing.T) {
	s := customMetricsFixture(t)
	h1, p1 := reporting(t, "hot", "TEXT application_utilization=0.9, rps_fractional=10")
	h2, p2 := reporting(t, "cool", "TEXT application_utilization=0.1, rps_fractional=10")
	g := hybridNEG(t, s, "us-central1-a", "w", [2]any{h1, p1}, [2]any{h2, p2})
	lbCall(t, s, "POST", "global/backendServices", obj{"name": "wrr", "protocol": "HTTP", "loadBalancingScheme": "EXTERNAL_MANAGED",
		"localityLbPolicy": "WEIGHTED_ROUND_ROBIN", "healthChecks": []any{"global/healthChecks/hc"},
		"customMetrics": []any{obj{"name": "orca.application_utilization", "dryRun": false}},
		"backends":      []any{obj{"group": g, "balancingMode": "RATE", "maxRatePerEndpoint": 1000}}})
	u := lbFront(t, s, "wrr")
	c := &http.Client{}
	got := map[string]int{}
	waitFor(t, "both endpoints answering", func() bool {
		resp, err := c.Get(u + "/")
		if err != nil {
			return false
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got[string(body)]++
		return got["hot"] > 0 && got["cool"] > 0
	})
	got = map[string]int{}
	for i := 0; i < 300; i++ {
		_, body, _ := get(t, c, u+"/")
		got[body]++
	}
	// Weights 10/0.9 and 10/0.1: about 10% and 90%.
	if got["cool"] < 220 || got["hot"] == 0 {
		t.Fatalf("distribution: %v", got)
	}
}

func TestCustomMetricsValidation(t *testing.T) {
	s := customMetricsFixture(t)
	g := hybridNEG(t, s, "us-central1-a", "v", [2]any{"10.1.2.3", 80})
	for _, tc := range []struct {
		body obj
		want string
	}{
		{obj{"backends": []any{obj{"group": g, "balancingMode": "CUSTOM_METRICS"}}}, "needs at least one custom metric"},
		{obj{"backends": []any{obj{"group": g, "balancingMode": "CUSTOM_METRICS", "customMetrics": []any{obj{"name": "Bad"}}}}}, "regex"},
		{obj{"backends": []any{obj{"group": g, "balancingMode": "CUSTOM_METRICS", "customMetrics": []any{obj{"name": "x", "maxUtilization": 1.5}}}}}, "between 0.0 and 1.0"},
		{obj{"backends": []any{obj{"group": g, "balancingMode": "CUSTOM_METRICS", "customMetrics": []any{obj{"name": "x"}}}}}, "maxUtilization"},
		{obj{"backends": []any{obj{"group": g, "balancingMode": "CUSTOM_METRICS", "customMetrics": []any{
			obj{"name": "a", "maxUtilization": 0.5}, obj{"name": "b", "maxUtilization": 0.5}, obj{"name": "c", "maxUtilization": 0.5}}}}}, "At most 2"},
		{obj{"backends": []any{obj{"group": g, "balancingMode": "RATE", "maxRatePerEndpoint": 1, "customMetrics": []any{obj{"name": "x", "maxUtilization": 0.5}}}}}, "need the CUSTOM_METRICS"},
		{obj{"loadBalancingScheme": "EXTERNAL", "backends": []any{obj{"group": g, "balancingMode": "CUSTOM_METRICS", "customMetrics": []any{obj{"name": "x", "maxUtilization": 0.5}}}}}, "Application Load Balancers"},
		{obj{"customMetrics": []any{obj{"name": "x", "maxUtilization": 0.5}}}, "no maxUtilization"},
	} {
		b := obj{"name": "bad", "protocol": "HTTP", "loadBalancingScheme": "EXTERNAL_MANAGED", "healthChecks": []any{"global/healthChecks/hc"}}
		for k, v := range tc.body {
			b[k] = v
		}
		_, err := vmCall(t, s, "POST", "global/backendServices", jsonBody(b))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.body, err, tc.want)
		}
	}
	// Dry-run metrics: three in all, two used, and no maxUtilization needed.
	lbCall(t, s, "POST", "global/backendServices", obj{"name": "dry", "protocol": "HTTP", "loadBalancingScheme": "EXTERNAL_MANAGED",
		"healthChecks": []any{"global/healthChecks/hc"},
		"backends": []any{obj{"group": g, "balancingMode": "CUSTOM_METRICS", "customMetrics": []any{
			obj{"name": "a", "maxUtilization": 0.5}, obj{"name": "b", "maxUtilization": 0.5}, obj{"name": "c", "dryRun": true}}}}})
}
