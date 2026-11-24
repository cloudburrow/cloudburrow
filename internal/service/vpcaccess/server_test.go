package vpcaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/vpcaccess/apiv1/vpcaccesspb"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// fakeNets is one project's network "default" with the subnet
// "default" (10.128.0.0/20) and the /28 subnet "conn" in us-central1, a VM
// on the /28 "busy".
type fakeNets struct{}

func (fakeNets) HasNetwork(ref, project string) bool {
	return strings.HasSuffix(ref, "/networks/default")
}

func (fakeNets) Conflict(network string, n *net.IPNet) string {
	d, _ := compute.ParseCIDR("10.128.0.0/20")
	if compute.Overlaps(n, d) {
		return "subnetwork default in region us-central1 (10.128.0.0/20)"
	}
	return ""
}

func (fakeNets) SubnetRange(ref, project, region string) (string, string, bool) {
	switch lastSeg(ref) {
	case "conn":
		return "projects/p/global/networks/default", "10.9.0.0/28", true
	case "busy":
		return "projects/p/global/networks/default", "10.9.1.0/28", true
	case "default":
		return "projects/p/global/networks/default", "10.128.0.0/20", true
	}
	return "", "", false
}

func (fakeNets) SubnetUsers(rel string) []string {
	if lastSeg(rel) == "busy" {
		return []string{"projects/p/zones/us-central1-a/instances/vm"}
	}
	return nil
}

func newTest(t *testing.T, state string) *Server {
	t.Helper()
	s, err := New(Options{State: state, Networks: fakeNets{}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func create(s *Server, id string, c *vpcaccesspb.Connector) error {
	_, err := s.CreateConnector(context.Background(), &vpcaccesspb.CreateConnectorRequest{
		Parent: "projects/p/locations/us-central1", ConnectorId: id, Connector: c})
	return err
}

func TestConnectorValidation(t *testing.T) {
	s := newTest(t, "")
	ok := func(c *vpcaccesspb.Connector) *vpcaccesspb.Connector { return c }
	for _, tc := range []struct {
		id   string
		c    *vpcaccesspb.Connector
		want string
	}{
		{"Bad", ok(&vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.0.0/28"}), "must follow the pattern"},
		{"a-very-long-connector-name-x", ok(&vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.0.0/28"}), "must follow the pattern"},
		{"c1", ok(&vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.0.0/27"}), "must be a /28"},
		{"c1", ok(&vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.0.1/28"}), "first of the range"},
		{"c1", ok(&vpcaccesspb.Connector{Network: "default", IpCidrRange: "8.8.8.0/28"}), "private ranges"},
		{"c1", ok(&vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.128.0.0/28"}), "conflicts with subnetwork default"},
		{"c1", ok(&vpcaccesspb.Connector{Network: "nope", IpCidrRange: "10.8.0.0/28"}), "Network 'nope' was not found"},
		{"c1", ok(&vpcaccesspb.Connector{Network: "default"}), "ip_cidr_range must be set"},
		{"c1", ok(&vpcaccesspb.Connector{}), "Either network and ip_cidr_range, or subnet"},
		{"c1", ok(&vpcaccesspb.Connector{Subnet: &vpcaccesspb.Connector_Subnet{Name: "default"}}), "must have a /28"},
		{"c1", ok(&vpcaccesspb.Connector{Subnet: &vpcaccesspb.Connector_Subnet{Name: "busy"}}), "a connector's subnet must be its own"},
		{"c1", ok(&vpcaccesspb.Connector{Subnet: &vpcaccesspb.Connector_Subnet{Name: "gone"}}), "was not found in region"},
		{"c1", ok(&vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.0.0/28", MinInstances: 1}), "'min_instances': 1"},
		{"c1", ok(&vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.0.0/28", MaxInstances: 11}), "'max_instances': 11"},
		{"c1", ok(&vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.0.0/28", MinInstances: 5, MaxInstances: 5}), "must be lower than"},
		{"c1", ok(&vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.0.0/28", MachineType: "n1-standard-1"}), "machine_type"},
		{"c1", ok(&vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.0.0/28", MinThroughput: 250}), "min_throughput"},
	} {
		err := create(s, tc.id, tc.c)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("create %s %v: %v, want %q", tc.id, tc.c, err, tc.want)
		}
	}
}

func TestConnectorLifecycleAndDefaults(t *testing.T) {
	state := filepath.Join(t.TempDir(), "vpcaccess.json")
	s := newTest(t, state)
	if err := create(s, "conn-a", &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.0.0/28"}); err != nil {
		t.Fatal(err)
	}
	c, ok := s.Connector("projects/p/locations/us-central1/connectors/conn-a")
	if !ok || c.GetState() != vpcaccesspb.Connector_READY || c.GetMachineType() != "e2-micro" || c.GetMinInstances() != 2 ||
		c.GetMaxInstances() != 10 || c.GetMinThroughput() != 200 || c.GetMaxThroughput() != 1000 {
		t.Fatalf("defaults: %v", c)
	}
	// An overlapping range of another connector is refused, as is the
	// same ID.
	if err := create(s, "conn-b", &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.0.0/28"}); err == nil || !strings.Contains(err.Error(), "connector 'conn-a'") {
		t.Fatalf("overlap: %v", err)
	}
	if err := create(s, "conn-a", &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.1.0/28"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate: %v", err)
	}
	// A subnet connector takes the subnet's network; throughput sizes it.
	if err := create(s, "conn-s", &vpcaccesspb.Connector{Subnet: &vpcaccesspb.Connector_Subnet{Name: "conn"}, MinThroughput: 300, MaxThroughput: 700, MachineType: "f1-micro"}); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Connector("projects/p/locations/us-central1/connectors/conn-s")
	if cs.GetNetwork() != "default" || cs.GetSubnet().GetProjectId() != "p" || cs.GetMinInstances() != 3 || cs.GetMaxInstances() != 7 {
		t.Fatalf("subnet connector: %v", cs)
	}
	if err := create(s, "conn-t", &vpcaccesspb.Connector{Subnet: &vpcaccesspb.Connector_Subnet{Name: "conn"}}); err == nil || !strings.Contains(err.Error(), "already used by connector") {
		t.Fatalf("subnet reuse: %v", err)
	}
	// It survives a restart.
	s2 := newTest(t, state)
	l, err := s2.ListConnectors(context.Background(), &vpcaccesspb.ListConnectorsRequest{Parent: "projects/p/locations/us-central1"})
	if err != nil || len(l.GetConnectors()) != 2 {
		t.Fatalf("list after restart: %v %v", l, err)
	}
	op, err := s2.DeleteConnector(context.Background(), &vpcaccesspb.DeleteConnectorRequest{Name: "projects/p/locations/us-central1/connectors/conn-a"})
	if err != nil || !op.GetDone() {
		t.Fatalf("delete: %v %v", op, err)
	}
	if _, ok := s2.Connector("projects/p/locations/us-central1/connectors/conn-a"); ok {
		t.Fatal("deleted connector still there")
	}
}

// TestConnectorsOverJSON drives the REST binding the Go REST client and
// gcloud use: create answers a done operation with the connector, get
// and list read it, and the operation and the locations are served.
func TestConnectorsOverJSON(t *testing.T) {
	s := newTest(t, "")
	h := NewRESTHandler(s)
	do := func(method, path, body string) (int, map[string]any) {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var m map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		return w.Code, m
	}
	code, op := do("POST", "/v1/projects/p/locations/us-central1/connectors?connectorId=web", `{"network":"default","ipCidrRange":"10.8.0.0/28"}`)
	if code != http.StatusOK || op["done"] != true {
		t.Fatalf("create: %d %v", code, op)
	}
	resp, _ := op["response"].(map[string]any)
	if resp["state"] != "READY" || !strings.Contains(resp["@type"].(string), "vpcaccess.v1.Connector") {
		t.Fatalf("create response: %v", resp)
	}
	if code, m := do("GET", "/v1/"+op["name"].(string), ""); code != http.StatusOK || m["done"] != true {
		t.Fatalf("get operation: %d %v", code, m)
	}
	if code, m := do("GET", "/v1/projects/p/locations/us-central1/connectors", ""); code != http.StatusOK || len(m["connectors"].([]any)) != 1 {
		t.Fatalf("list: %d %v", code, m)
	}
	if code, m := do("GET", "/v1/projects/p/locations", ""); code != http.StatusOK || len(m["locations"].([]any)) == 0 {
		t.Fatalf("locations: %d %v", code, m)
	}
	if code, _ := do("GET", "/v1/projects/p/locations/us-central1/connectors/none", ""); code != http.StatusNotFound {
		t.Fatalf("missing: %d", code)
	}
	if !Handles("/v1/projects/p/locations/us-central1/connectors") || Handles("/compute/v1/projects/p/global/networks") {
		t.Fatal("Handles")
	}
}
