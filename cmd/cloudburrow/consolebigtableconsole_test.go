package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
)

// TestBigtableConsoleParity (#1858): Create instance's steps (edition, 2x
// node scaling, autoscaling with a storage target); the instance page's
// Create table (column families with their type and GC policy, change
// stream, deletion protection), Edit table and Delete; Create backup; and
// Bigtable Studio's explorer, query builder and SQL editor, each read back
// through the official clients.
func TestBigtableConsoleParity(t *testing.T) {
	p, srv, iac := bigtableInstancesConsole(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := p.Create(ctx, "demo", map[string]string{"displayName": "Shop data", "instance": "shop-data",
		"edition": "ENTERPRISE_PLUS", "storage": "SSD", "cluster": "shop-c1", "region": "us-east1", "zone": "us-central1-b",
		"scaling": "MANUAL", "nodes": "1"}); err == nil || !strings.Contains(err.Error(), "not in region") {
		t.Errorf("Create with a zone outside its region: %v", err)
	}
	if _, err := p.Create(ctx, "demo", map[string]string{"displayName": "Shop data", "instance": "shop-data",
		"edition": "ENTERPRISE", "storage": "SSD", "cluster": "shop-c1", "region": "us-east1", "zone": "us-east1-b",
		"scaling": "MANUAL", "nodes": "1", "encryption": "CMEK"}); err == nil || !strings.Contains(err.Error(), "Not available locally") {
		t.Errorf("Create with a Cloud KMS key: %v", err)
	}
	if _, err := p.Create(ctx, "demo", map[string]string{"displayName": "Shop data", "instance": "shop-data",
		"edition": "ENTERPRISE_PLUS", "storage": "SSD", "cluster": "shop-c1", "region": "us-east1", "zone": "us-east1-b",
		"nodeScaling2x": "true", "scaling": "AUTOSCALING", "minNodes": "2", "maxNodes": "6", "cpuTarget": "55", "storageTarget": "3000"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	info, err := iac.InstanceInfo(ctx, "shop-data")
	if err != nil || info.Edition != bigtable.EnterprisePlus || info.DisplayName != "Shop data" {
		t.Fatalf("InstanceInfo = %+v, %v", info, err)
	}
	cl, err := iac.GetCluster(ctx, "shop-data", "shop-c1")
	if err != nil || cl.AutoscalingConfig == nil || cl.AutoscalingConfig.StorageUtilizationPerNode != 3000 || cl.NodeScalingFactor != bigtable.NodeScalingFactor2X {
		t.Fatalf("GetCluster = %+v, %v", cl, err)
	}
	list, _ := p.List(ctx, "demo")
	if len(list.Items) != 1 || list.Items[0].Fields["Edition"] != "Enterprise Plus" || list.Items[0].Fields["Storage type"] != "SSD" {
		t.Errorf("List = %+v", list.Items)
	}

	// Create table on the Tables page.
	if code, out := bigtableInstancesAct(t, srv, []string{"shop-data"}, "createtable", map[string]string{"table": "orders",
		"families":     `[{"family":"cf1","type":"Standard","maxVersions":"3"},{"family":"totals","type":"Sum"},{"family":"old","maxAge":"7d"}]`,
		"changeStream": "true", "retention": "2", "protected": "true"}); code != http.StatusOK {
		t.Fatalf("Create table = %d: %s", code, out)
	}
	admin, err := bigtable.NewAdminClient(ctx, "demo", "shop-data", localOpts(p.endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	ti, err := admin.TableInfo(ctx, "orders")
	if err != nil || len(ti.FamilyInfos) != 3 || ti.DeletionProtection != bigtable.Protected {
		t.Fatalf("TableInfo = %+v, %v", ti, err)
	}
	if d, _ := ti.ChangeStreamRetention.(time.Duration); d != 48*time.Hour {
		t.Errorf("change stream retention = %v", ti.ChangeStreamRetention)
	}
	// Min, Max and HyperLogLog families are made (the front computes them,
	// #1893), each read back as its type.
	if code, out := bigtableInstancesAct(t, srv, []string{"shop-data"}, "createtable", map[string]string{"table": "aggs",
		"families": `[{"family":"m","type":"Max"},{"family":"n","type":"Min"},{"family":"h","type":"HyperLogLog"}]`}); code != http.StatusOK {
		t.Errorf("Create table with Max, Min and HyperLogLog families = %d: %s", code, out)
	}
	if ai, err := admin.TableInfo(ctx, "aggs"); err != nil {
		t.Errorf("TableInfo aggs: %v", err)
	} else {
		got := map[string]string{}
		for _, f := range ai.FamilyInfos {
			got[f.Name] = bigtableFamilyType(f.ValueType)
		}
		if got["m"] != "Max" || got["n"] != "Min" || got["h"] != "HyperLogLog" {
			t.Errorf("the aggregate families read back as %v", got)
		}
	}
	if err := admin.DeleteTable(ctx, "aggs"); err != nil {
		t.Errorf("DeleteTable aggs: %v", err)
	}
	d, err := p.Detail(ctx, "demo", []string{"shop-data"})
	if err != nil {
		t.Fatal(err)
	}
	rows := bigtableSection(d, "tables").Listing.Items
	if len(rows) != 1 || rows[0].Fields["Deletion protection"] != "Enabled" || rows[0].Fields["Change stream"] == "Disabled" {
		t.Fatalf("the Tables page = %+v", rows)
	}
	// Delete is refused while deletion is prevented.
	if code, _ := bigtableInstancesAct(t, srv, []string{"shop-data", "tables", "orders"}, "deletetable", nil); code == http.StatusOK {
		t.Error("Delete of a protected table was accepted")
	}
	// Edit table: drop "old", add "notes", turn the change stream off and
	// enable deletion.
	if code, out := bigtableInstancesAct(t, srv, []string{"shop-data", "tables", "orders"}, "edittable", map[string]string{
		"families":     `[{"family":"cf1","type":"Standard","maxVersions":"1"},{"family":"totals","type":"Sum"},{"family":"notes"}]`,
		"changeStream": "false", "protection": "ENABLE"}); code != http.StatusOK {
		t.Fatalf("Edit table = %d: %s", code, out)
	}
	ti, _ = admin.TableInfo(ctx, "orders")
	names := map[string]string{}
	for _, f := range ti.FamilyInfos {
		names[f.Name] = f.GCPolicy
	}
	if _, ok := names["old"]; ok || names["cf1"] != "versions() > 1" || len(names) != 3 || ti.DeletionProtection == bigtable.Protected {
		t.Errorf("after Edit table, families = %v, protection %v", names, ti.DeletionProtection)
	}

	// Create backup on the Backups page.
	if code, out := bigtableInstancesAct(t, srv, []string{"shop-data"}, "createbackup", map[string]string{
		"table": "orders", "cluster": "shop-c1", "backup": "orders-bk", "days": "3"}); code != http.StatusOK {
		t.Fatalf("Create backup = %d: %s", code, out)
	}
	if b, err := admin.BackupInfo(ctx, "shop-c1", "orders-bk"); err != nil || b.SourceTable != "orders" {
		t.Errorf("BackupInfo = %+v, %v", b, err)
	}

	// Bigtable Studio.
	data, err := newConsoleBigtableClient(ctx, p.endpoint, "demo", "shop-data")
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	for _, k := range []string{"a#1", "a#2", "b#1"} {
		m := bigtable.NewMutation()
		m.Set("cf1", "name", bigtable.Now(), []byte("v-"+k))
		if err := data.Open("orders").Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}
	studio := func(call string, body map[string]any) (*bigtableStudioResult, error) {
		body["instance"] = "shop-data"
		raw, _ := json.Marshal(body)
		out, err := p.StudioCall(ctx, "demo", call, raw)
		if err != nil {
			return nil, err
		}
		r, _ := out.(*bigtableStudioResult)
		return r, nil
	}
	exp, err := p.StudioCall(ctx, "demo", "explorer", json.RawMessage(`{"instance":"shop-data"}`))
	if e, _ := exp.(*bigtableStudioExplorer); err != nil || len(e.Tables) != 1 || len(e.Tables[0].Families) != 3 {
		t.Fatalf("explorer = %+v, %v", exp, err)
	}
	r, err := studio("read", map[string]any{"table": "orders", "prefix": "a#", "family": "cf1", "limit": 10})
	if err != nil || len(r.Rows) != 2 || r.Rows[0][0] != "a#1" || r.Columns[1] != "cf1:name" || len(r.Details[0].Cells) != 1 {
		t.Fatalf("read by prefix = %+v, %v", r, err)
	}
	if r, err := studio("read", map[string]any{"table": "orders", "keys": "b#1, a#2"}); err != nil || len(r.Rows) != 2 {
		t.Errorf("read by keys = %+v, %v", r, err)
	}
	if r, err := studio("read", map[string]any{"table": "orders", "start": "a#2", "end": "b#1"}); err != nil || len(r.Rows) != 1 {
		t.Errorf("read by range = %+v, %v", r, err)
	}
	r, err = studio("sql", map[string]any{"sql": "SELECT _key, CAST(cf1['name'] AS STRING) AS name FROM orders WHERE STARTS_WITH(_key, 'a') ORDER BY _key"})
	if err != nil || len(r.Rows) != 2 || r.Columns[1] != "name" || r.Rows[1][1] != "v-a#2" {
		t.Fatalf("sql = %+v, %v", r, err)
	}
	if _, err := studio("sql", map[string]any{"sql": "SELECT nope FROM missing"}); err == nil {
		t.Error("a query of a missing table was answered")
	}
}

// TestBigtableStudioSavedQueries (#1858): Save query (a name and an
// optional region of Create instance's), a new version on Save, the
// project's list under the Explorer's Queries, Delete query; and the
// Tables page's Status from each cluster's replication state.
func TestBigtableStudioSavedQueries(t *testing.T) {
	p, _, iac := bigtableInstancesConsole(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "saved-inst", DisplayName: "saved-inst",
		ClusterId: "saved-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatal(err)
	}
	call := func(name string, body map[string]any) (any, error) {
		body["instance"] = "saved-inst"
		raw, _ := json.Marshal(body)
		return p.StudioCall(ctx, "demo", name, raw)
	}
	if _, err := call("savequery", map[string]any{"name": " ", "sql": "SELECT 1"}); err == nil {
		t.Error("a saved query without a name was saved")
	}
	if _, err := call("savequery", map[string]any{"name": "q", "region": "atlantis1", "sql": "SELECT 1"}); err == nil || !strings.Contains(err.Error(), "Bigtable region") {
		t.Errorf("a saved query in an unknown region: %v", err)
	}
	out, err := call("savequery", map[string]any{"name": "Daily orders", "region": "us-east1", "sql": "SELECT _key FROM orders"})
	sq, _ := out.(bqSavedQuery)
	if err != nil || sq.ID == "" || sq.Version != 1 || sq.Region != "us-east1" {
		t.Fatalf("savequery = %+v, %v", out, err)
	}
	if _, err := call("savequery", map[string]any{"name": "daily ORDERS", "sql": "SELECT 2"}); err == nil {
		t.Error("a second saved query of the same name was saved")
	}
	out, err = call("savequery", map[string]any{"id": sq.ID, "name": "Daily orders", "sql": "SELECT _key FROM orders LIMIT 5"})
	if v, _ := out.(bqSavedQuery); err != nil || v.Version != 2 || v.Query != "SELECT _key FROM orders LIMIT 5" {
		t.Fatalf("Save of the open query = %+v, %v", out, err)
	}
	out, err = call("savedqueries", map[string]any{})
	m, _ := out.(map[string]any)
	if qs, _ := m["queries"].([]bqSavedQuery); err != nil || len(qs) != 1 || qs[0].Name != "Daily orders" {
		t.Fatalf("savedqueries = %+v, %v", out, err)
	}
	if rs, _ := m["regions"].([]map[string]string); len(rs) == 0 || rs[0]["id"] == "" || rs[0]["label"] == "" {
		t.Errorf("savedqueries regions = %v", m["regions"])
	}
	// Saved queries are the project's: another project lists none.
	raw, _ := json.Marshal(map[string]any{"instance": "saved-inst"})
	if out, err := p.StudioCall(ctx, "other", "savedqueries", raw); err != nil || len(out.(map[string]any)["queries"].([]bqSavedQuery)) != 0 {
		t.Errorf("another project's savedqueries = %+v, %v", out, err)
	}
	if _, err := call("deletequery", map[string]any{"id": sq.ID}); err != nil {
		t.Fatalf("deletequery: %v", err)
	}
	if _, err := call("deletequery", map[string]any{"id": sq.ID}); err == nil {
		t.Error("a deleted saved query was deleted again")
	}

	// The Tables page's Status.
	admin, err := bigtable.NewAdminClient(ctx, "demo", "saved-inst", localOpts(p.endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.CreateTable(ctx, "orders"); err != nil {
		t.Fatal(err)
	}
	d, err := p.Detail(ctx, "demo", []string{"saved-inst"})
	if err != nil {
		t.Fatal(err)
	}
	rows := bigtableSection(d, "tables").Listing.Items
	if len(rows) != 1 || rows[0].Fields["Status"] != "Ready" || rows[0].Fields["Storage utilization"] != "—" {
		t.Errorf("the Tables page = %+v", rows)
	}
}

func TestBigtableTableStatus(t *testing.T) {
	for want, states := range map[string]map[string]*btapb.Table_ClusterState{
		"—":     nil,
		"Ready": {"a": {ReplicationState: btapb.Table_ClusterState_READY}, "b": {ReplicationState: btapb.Table_ClusterState_READY}},
		"a: Ready, b: Initializing": {"b": {ReplicationState: btapb.Table_ClusterState_INITIALIZING},
			"a": {ReplicationState: btapb.Table_ClusterState_READY}},
	} {
		if got := bigtableTableStatus(states); got != want {
			t.Errorf("bigtableTableStatus(%v) = %q, want %q", states, got, want)
		}
	}
}
