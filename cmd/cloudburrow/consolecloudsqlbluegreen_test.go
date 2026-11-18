package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sqladminapi "google.golang.org/api/sqladmin/v1"

	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// TestCloudSQLConsoleBlueGreen (#1614): a MySQL 8.0 primary's page offers
// Create blue-green deployment, with MySQL 8.4 as the green version, which
// calls blueGreenDeployments.create in the instance's region; an instance
// in a deployment shows its role and the deployment's state with its
// errorDetail, and offers Switch over and Delete, which call switchover
// and delete; after the switchover Delete sends deleteOldSource. A
// PostgreSQL instance has no blue-green actions.
func TestCloudSQLConsoleBlueGreen(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls[r.Method+" "+r.URL.Path] = string(b) + "?" + r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		inst := func(name, version string, info map[string]any) {
			m := map[string]any{"name": name, "state": "RUNNABLE", "databaseVersion": version, "region": "us-central1",
				"instanceType": "CLOUD_SQL_INSTANCE", "settings": map[string]any{"tier": "t"}}
			if info != nil {
				m["deploymentInfo"] = info
			}
			_ = json.NewEncoder(w).Encode(m)
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/blue"):
			inst("blue", "MYSQL_8_0", nil)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/pg"):
			inst("pg", "POSTGRES_17", nil)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/green"):
			inst("green", "MYSQL_8_4", map[string]any{"deploymentId": "bg1", "state": "PRE_SWITCHOVER",
				"target": map[string]any{"sourceId": map[string]any{"name": "blue"}}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/after"):
			inst("after", "MYSQL_8_4", map[string]any{"deploymentId": "bg1", "state": "POST_SWITCHOVER",
				"source": map[string]any{"targetId": map[string]any{"name": "after-green"}}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blueGreenDeployments/bg1"):
			_, _ = io.WriteString(w, `{"name":"x","state":"SWITCHOVER_NOT_READY","errorDetail":"Replication is stopped"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{}`)
		default:
			_, _ = io.WriteString(w, `{"kind":"sql#operation","name":"op","status":"DONE"}`)
		}
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"MYSQL_8_0"}}
	ctx := context.Background()
	page := func(name string) (map[string]string, map[string]bool) {
		d, err := p.Detail(ctx, "proj", []string{name})
		if err != nil {
			t.Fatal(err)
		}
		sum, acts := map[string]string{}, map[string]bool{}
		for _, pr := range d.Summary {
			sum[pr.Label] = pr.Value
		}
		for _, a := range d.Actions {
			acts[a.ID] = true
		}
		return sum, acts
	}
	if _, acts := page("blue"); !acts["blue-green-create"] || acts["blue-green-switchover"] {
		t.Errorf("the blue instance's actions: %v", acts)
	}
	if _, acts := page("pg"); acts["blue-green-create"] {
		t.Errorf("a PostgreSQL instance offers a blue-green deployment: %v", acts)
	}
	if err := p.ActAt(ctx, "proj", []string{"blue"}, "blue-green-create",
		map[string]string{"deploymentId": "bg1", "databaseVersion": "MYSQL_8_4", "description": "upgrade"}); err != nil {
		t.Fatal(err)
	}
	if b := calls["POST /v1/projects/proj/locations/us-central1/blueGreenDeployments"]; !strings.Contains(b, `"sourceInstance":"projects/proj/instances/blue"`) ||
		!strings.Contains(b, `"databaseVersion":"MYSQL_8_4"`) || !strings.Contains(b, "blueGreenDeploymentId=bg1") {
		t.Errorf("create: %s", b)
	}
	sum, acts := page("green")
	if !strings.Contains(sum["Blue-green deployment"], "green (target), blue blue") ||
		sum["Blue-green deployment state"] != "SWITCHOVER_NOT_READY: Replication is stopped" ||
		!acts["blue-green-switchover"] || !acts["blue-green-delete"] || acts["blue-green-create"] {
		t.Errorf("the green instance's page: %v %v", sum, acts)
	}
	if err := p.ActAt(ctx, "proj", []string{"green"}, "blue-green-switchover", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := calls["POST /v1/projects/proj/locations/us-central1/blueGreenDeployments/bg1:switchover"]; !ok {
		t.Errorf("switchover not called: %v", calls)
	}
	if _, acts := page("after"); acts["blue-green-switchover"] || !acts["blue-green-delete"] {
		t.Errorf("after the switchover: %v", acts)
	}
	if err := p.ActAt(ctx, "proj", []string{"after"}, "blue-green-delete", map[string]string{"deleteOldSource": "true"}); err != nil {
		t.Fatal(err)
	}
	if q := calls["DELETE /v1/projects/proj/locations/us-central1/blueGreenDeployments/bg1"]; !strings.Contains(q, "deleteOldSource=true") {
		t.Errorf("delete: %q", q)
	}
}

// TestCloudSQLConsoleRegionalBlueGreenAndDR (#1614): a REGIONAL MySQL
// primary's Create blue-green deployment says its green instance is
// high-availability too, and a PostgreSQL DR replica's Switchover says the
// primary's other read replicas stay its replicas, rebuilt.
func TestCloudSQLConsoleRegionalBlueGreenAndDR(t *testing.T) {
	acts := blueGreenActions(&sqladminapi.DatabaseInstance{Name: "blue", DatabaseVersion: "MYSQL_8_0",
		Settings: &sqladminapi.Settings{AvailabilityType: "REGIONAL"}})
	if len(acts) != 1 || acts[0].ID != "blue-green-create" || !strings.Contains(acts[0].Fields[0].Help, "high-availability (REGIONAL)") {
		t.Errorf("a REGIONAL blue instance's actions: %+v", acts)
	}
	acts = blueGreenActions(&sqladminapi.DatabaseInstance{Name: "blue", DatabaseVersion: "MYSQL_8_0", Settings: &sqladminapi.Settings{}})
	if len(acts) != 1 || strings.Contains(acts[0].Fields[0].Help, "REGIONAL") {
		t.Errorf("a ZONAL blue instance's actions: %+v", acts)
	}
	acts = drActions(&sqladminapi.DatabaseInstance{Name: "r", DatabaseVersion: "POSTGRES_17", MasterInstanceName: "p:prim",
		ReplicationCluster: &sqladminapi.ReplicationCluster{DrReplica: true}})
	if len(acts) != 1 || acts[0].ID != "switchover" || !strings.Contains(acts[0].Confirm, "other read replicas stay its replicas") {
		t.Errorf("a PostgreSQL DR replica's actions: %+v", acts)
	}
}

// TestRegionalReplicaStandbyKeepsItsServer (#1614): a REGIONAL read
// replica's standby (a green instance's) is the same server once the
// instance is a primary, so a blue-green switchover and a failover after
// it do not restart it, which on an ephemeral instance would lose its data.
func TestRegionalReplicaStandbyKeepsItsServer(t *testing.T) {
	p := &sqlProvisioner{}
	green := sqladmin.Spec{Name: "blue-green-1", DatabaseVersion: "MYSQL_8_0", AvailabilityType: "REGIONAL", MasterInstanceName: "blue",
		ReplicationPassword: "r", HAPassword: "h", HAEpochs: [2]int{0, 1}}
	promoted := green
	promoted.MasterInstanceName, promoted.ReplicationPassword = "", ""
	if a, b := p.nodeBackend(green, 1).Manifest("ns", "i"), p.nodeBackend(promoted, 1).Manifest("ns", "i"); a != b {
		t.Errorf("the standby's server changes when the replica becomes a primary:\n%s\n---\n%s", a, b)
	}
	if strings.Join(p.nodeBackend(green, 0).Args, " ") == strings.Join(p.nodeBackend(promoted, 0).Args, " ") {
		t.Error("the replica's primary server is not a replica")
	}
}
