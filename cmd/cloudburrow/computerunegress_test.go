package main

import (
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// TestRunEgressPodsAndGateways (#2140): Cloud Run's running pods with
// cloudburrow.dev/vpc-egress are read with their vpcAccess, gateway pods
// are told apart, a connector's and a Direct VPC egress setting's gateways
// are keyed by what they carry, and the nodes' script routes each pod's
// traffic to its gateway's table.
func TestRunEgressPodsAndGateways(t *testing.T) {
	list := `{"items":[
 {"metadata":{"name":"a","annotations":{"cloudburrow.dev/vpc-egress":"{\"name\":\"projects/p/locations/us-central1/services/s\",\"vpcAccess\":{\"connector\":\"projects/p/locations/us-central1/connectors/c\"}}"}},"status":{"phase":"Running","podIP":"10.244.0.20"}},
 {"metadata":{"name":"b","annotations":{"cloudburrow.dev/vpc-egress":"{\"name\":\"projects/p/locations/us-central1/services/d\",\"vpcAccess\":{\"egress\":\"ALL_TRAFFIC\",\"networkInterfaces\":[{\"network\":\"default\",\"tags\":[\"web\",\"api\"]}]}}"}},"status":{"phase":"Running","podIP":"10.244.0.21"}},
 {"metadata":{"name":"pending","annotations":{"cloudburrow.dev/vpc-egress":"{\"name\":\"projects/p/locations/us-central1/services/s\",\"vpcAccess\":{\"connector\":\"c\"}}"}},"status":{"phase":"Pending"}},
 {"metadata":{"name":"plain"},"status":{"phase":"Running","podIP":"10.244.0.22"}},
 {"metadata":{"name":"cb-gw-1","labels":{"cloudburrow.dev/run-gateway":"gwx"}},"status":{"phase":"Running","podIP":"10.244.0.30"}}]}`
	pods, gw := runEgressPods(list)
	if len(pods) != 2 || pods[0].ip != "10.244.0.20" || pods[1].access.GetEgress() != runpb.VpcAccess_ALL_TRAFFIC || !gw["cb-gw-1"] {
		t.Fatalf("pods = %+v, gateways = %v", pods, gw)
	}
	conn := func(name string) (string, string, string, string, bool) {
		return "projects/p/global/networks/default", "10.8.0.0/28", "", "us-central1", name == "projects/p/locations/us-central1/connectors/c"
	}
	g, ok := runGateway(pods[0], conn)
	if !ok || !g.Connector || g.Key != "connector:projects/p/locations/us-central1/connectors/c" || g.Range != "10.8.0.0/28" {
		t.Fatalf("connector gateway = %+v", g)
	}
	d, ok := runGateway(pods[1], conn)
	if !ok || d.Connector || d.Subnetwork != "projects/p/regions/us-central1/subnetworks/default" || !d.AllTraffic ||
		d.Key != "direct:projects/p/regions/us-central1/subnetworks/default:api,web:all" {
		t.Fatalf("direct gateway = %+v", d)
	}
	script := runEgressScript(pods, map[string]string{"10.244.0.20": g.Key, "10.244.0.21": d.Key},
		[]compute.GatewayState{{Gateway: g, Pod: "10.244.0.30"}, {Gateway: d, Pod: "10.244.0.31"}},
		func(key string) []string { return []string{"10.128.0.0/20"} })
	tg, td := runEgressTableOf(g.Key), runEgressTableOf(d.Key)
	for _, want := range []string{
		"ip route replace 10.128.0.0/20 via 10.244.0.30 dev $d table " + strconv.Itoa(tg) + " onlink",
		"ip route replace throw 10.96.0.0/12 table " + strconv.Itoa(tg),
		"ip route replace default via 10.244.0.31 dev $d table " + strconv.Itoa(td) + " onlink",
		"ip rule add from 10.244.0.20/32 lookup " + strconv.Itoa(tg),
		"ip rule add from 10.244.0.21/32 lookup " + strconv.Itoa(td),
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "default via 10.244.0.30") {
		t.Errorf("PRIVATE_RANGES_ONLY routes everything:\n%s", script)
	}
}
