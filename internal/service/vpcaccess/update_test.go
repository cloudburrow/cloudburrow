package vpcaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/vpcaccess/apiv1/vpcaccesspb"
)

func TestUpdateConnectorRules(t *testing.T) {
	s := newTest(t, "")
	if err := create(s, "web", &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.0.0/28", MinInstances: 3, MaxInstances: 5}); err != nil {
		t.Fatal(err)
	}
	name := "projects/p/locations/us-central1/connectors/web"
	ctx := context.Background()
	bad := []struct {
		c    *vpcaccesspb.Connector
		mask []string
		want string
	}{
		{&vpcaccesspb.Connector{MinInstances: 2}, []string{"min_instances"}, "Decreasing"},
		{&vpcaccesspb.Connector{MaxInstances: 4}, []string{"maxInstances"}, "Decreasing"},
		{&vpcaccesspb.Connector{MaxInstances: 11}, []string{"max_instances"}, "between 3 and 10"},
		{&vpcaccesspb.Connector{MinInstances: 5}, []string{"min_instances"}, "lower than max_instances"},
		{&vpcaccesspb.Connector{MachineType: "n1-standard-1"}, []string{"machine_type"}, "machine_type"},
		{&vpcaccesspb.Connector{Network: "other"}, []string{"network"}, "cannot be updated"},
		{&vpcaccesspb.Connector{IpCidrRange: "10.8.1.0/28"}, []string{"ip_cidr_range"}, "cannot be updated"},
	}
	for _, b := range bad {
		if _, err := s.UpdateConnector(ctx, name, b.c, b.mask); err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%v %v: %v, want %q", b.c, b.mask, err, b.want)
		}
	}
	if _, err := s.UpdateConnector(ctx, "projects/p/locations/us-central1/connectors/none", &vpcaccesspb.Connector{MaxInstances: 6}, nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing: %v", err)
	}
	op, err := s.UpdateConnector(ctx, name, &vpcaccesspb.Connector{MinInstances: 4, MaxInstances: 8, MachineType: "e2-standard-4", Network: "ignored"},
		[]string{"min_instances", "max_instances", "machine_type"})
	if err != nil || !op.GetDone() {
		t.Fatalf("update: %v %v", op, err)
	}
	c, _ := s.Connector(name)
	if c.GetMinInstances() != 4 || c.GetMaxInstances() != 8 || c.GetMachineType() != "e2-standard-4" || c.GetMinThroughput() != 400 || c.GetMaxThroughput() != 800 || c.GetNetwork() != "default" {
		t.Fatalf("after update: %v", c)
	}
	// No mask: the fields the body sets.
	if _, err := s.UpdateConnector(ctx, name, &vpcaccesspb.Connector{MaxInstances: 10}, nil); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Connector(name)
	if c.GetMinInstances() != 4 || c.GetMaxInstances() != 10 || c.GetMachineType() != "e2-standard-4" {
		t.Fatalf("after maskless update: %v", c)
	}
}

func TestUpdateConnectorOverJSON(t *testing.T) {
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
	if code, m := do("POST", "/v1/projects/p/locations/us-central1/connectors?connectorId=web", `{"network":"default","ipCidrRange":"10.8.0.0/28"}`); code != http.StatusOK {
		t.Fatalf("create: %d %v", code, m)
	}
	code, op := do("PATCH", "/v1/projects/p/locations/us-central1/connectors/web?updateMask=minInstances,machineType", `{"minInstances":3,"machineType":"f1-micro"}`)
	if code != http.StatusOK || op["done"] != true {
		t.Fatalf("patch: %d %v", code, op)
	}
	resp, _ := op["response"].(map[string]any)
	if resp["minInstances"] != float64(3) || resp["machineType"] != "f1-micro" {
		t.Fatalf("patch response: %v", resp)
	}
	md, _ := op["metadata"].(map[string]any)
	if !strings.HasSuffix(md["method"].(string), "UpdateConnector") {
		t.Fatalf("metadata: %v", md)
	}
	if code, m := do("PATCH", "/v1/projects/p/locations/us-central1/connectors/web?updateMask=maxInstances", `{"maxInstances":4}`); code != http.StatusBadRequest {
		t.Fatalf("decrease: %d %v", code, m)
	}
	if code, m := do("PATCH", "/v1/projects/p/locations/us-central1/connectors/web", `{"bogus":1}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %v", code, m)
	}
	if code, _ := do("PATCH", "/v1/projects/p/locations/us-central1/connectors/none?updateMask=maxInstances", `{"maxInstances":10}`); code != http.StatusNotFound {
		t.Fatalf("missing: %d", code)
	}
}

func TestConnectorNetworkUsers(t *testing.T) {
	s := newTest(t, "")
	if err := create(s, "web", &vpcaccesspb.Connector{Network: "default", IpCidrRange: "10.8.0.0/28"}); err != nil {
		t.Fatal(err)
	}
	if err := create(s, "sub", &vpcaccesspb.Connector{Subnet: &vpcaccesspb.Connector_Subnet{Name: "conn"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.NetworkUsers("projects/p/global/networks/default"); len(got) != 1 || got[0] != "projects/p/locations/us-central1/connectors/web" {
		t.Fatalf("network users: %v", got)
	}
	if got := s.NetworkUsers("projects/p/regions/us-central1/subnetworks/conn"); len(got) != 1 || got[0] != "projects/p/locations/us-central1/connectors/sub" {
		t.Fatalf("subnet users: %v", got)
	}
	if got := s.NetworkUsers("projects/q/global/networks/default"); len(got) != 0 {
		t.Fatalf("other project: %v", got)
	}
	if _, err := s.DeleteConnector(context.Background(), &vpcaccesspb.DeleteConnectorRequest{Name: "projects/p/locations/us-central1/connectors/web"}); err != nil {
		t.Fatal(err)
	}
	if got := s.NetworkUsers("projects/p/global/networks/default"); len(got) != 0 {
		t.Fatalf("after delete: %v", got)
	}
}
