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
)

// TestCloudSQLConsoleSwitchover (#1614): a MySQL primary's page shows its
// DR replica and offers Designate DR replica among its replicas and Remove
// DR replica, both through instances.patch's replicationCluster; the DR
// replica's page shows its role and offers Switchover, which is refused
// unless the primary's instance ID is typed, and then calls
// instances.switchover with the timeout as dbTimeout.
func TestCloudSQLConsoleSwitchover(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls[r.Method+" "+r.URL.Path] = string(b) + "?" + r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/prim"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "prim", "state": "RUNNABLE", "databaseVersion": "MYSQL_8_4",
				"instanceType": "CLOUD_SQL_INSTANCE", "replicaNames": []string{"dr", "other"},
				"replicationCluster": map[string]any{"failoverDrReplicaName": "proj:dr"}, "settings": map[string]any{"tier": "t"}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances/dr"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "dr", "state": "RUNNABLE", "databaseVersion": "MYSQL_8_4",
				"instanceType": "READ_REPLICA_INSTANCE", "masterInstanceName": "proj:prim",
				"replicationCluster": map[string]any{"drReplica": true}, "settings": map[string]any{"tier": "t", "databaseReplicationEnabled": true}})
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{}`)
		default:
			_, _ = io.WriteString(w, `{"kind":"sql#operation","name":"op","status":"DONE"}`)
		}
	}))
	defer ts.Close()
	p := cloudSQLInstancesProvider{endpoint: strings.TrimPrefix(ts.URL, "http://"), versions: []string{"MYSQL_8_4"}}
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
	sum, acts := page("prim")
	if sum["DR replica"] != "proj:dr" || !acts["designate-dr-replica"] || !acts["clear-dr-replica"] || acts["switchover"] {
		t.Errorf("the primary's page: %v %v", sum, acts)
	}
	sum, acts = page("dr")
	if sum["Disaster recovery role"] != "DR replica of proj:prim" || !acts["switchover"] || acts["designate-dr-replica"] {
		t.Errorf("the DR replica's page: %v %v", sum, acts)
	}
	if err := p.ActAt(ctx, "proj", []string{"prim"}, "designate-dr-replica", map[string]string{"replica": "other"}); err != nil {
		t.Fatal(err)
	}
	if b := calls["PATCH /v1/projects/proj/instances/prim"]; !strings.Contains(b, `"replicationCluster":{"failoverDrReplicaName":"other"}`) {
		t.Errorf("designate: %s", b)
	}
	if err := p.ActAt(ctx, "proj", []string{"prim"}, "clear-dr-replica", nil); err != nil {
		t.Fatal(err)
	}
	if b := calls["PATCH /v1/projects/proj/instances/prim"]; !strings.Contains(b, `"failoverDrReplicaName":""`) {
		t.Errorf("remove: %s", b)
	}
	if err := p.ActAt(ctx, "proj", []string{"dr"}, "switchover", map[string]string{"primary": "wrong", "dbTimeout": "600"}); err == nil {
		t.Error("a switchover confirmed with the wrong primary was sent")
	}
	if _, ok := calls["POST /v1/projects/proj/instances/dr/switchover"]; ok {
		t.Error("switchover called without confirmation")
	}
	if err := p.ActAt(ctx, "proj", []string{"dr"}, "switchover", map[string]string{"primary": "prim", "dbTimeout": "900"}); err != nil {
		t.Fatal(err)
	}
	if q := calls["POST /v1/projects/proj/instances/dr/switchover"]; !strings.Contains(q, "dbTimeout=900s") {
		t.Errorf("switchover: %q", q)
	}
}
