package main

import (
	"context"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

func valkeyPageProps(t *testing.T, p valkeyProvider, id string) (console.Detail, map[string]string) {
	t.Helper()
	d, err := p.Detail(context.Background(), "p", []string{id})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("Detail %s = %+v, %v", id, d, err)
	}
	props := map[string]string{}
	for _, pr := range d.Sections[0].Groups[0].Properties {
		props[pr.Label] = pr.Value
	}
	return d, props
}

// TestConsoleValkeyReplicationMigrationVersions (#1557): Create instance
// makes a secondary of another (Secondary of), at an engine version, with
// persistence; the page shows the role, the primary, the membership and
// the engine version, and the primary's page its secondary; a secondary's
// page offers Switchover and Detach, and Detach leaves the replication;
// Start migration and Finish migration move an instance through MIGRATING
// back to ACTIVE; Edit offers the engine versions.
func TestConsoleValkeyReplicationMigrationVersions(t *testing.T) {
	srv := redisadmin.NewServer(redisadmin.Instance{Project: "p", Host: "127.0.0.1", Port: 9016, Created: time.Unix(100, 0)}, valkeyNoBackend{})
	srv.EnableCreate(context.Background(), valkeyNoProv{}, func(string, string, string) redisadmin.DataPlane { return nil }, nil, nil)
	nodes := &fakeValkeyNodes{running: map[string]string{}}
	srv.EnableValkey(context.Background(), nodes, func(addr, _, _ string) redisadmin.ClusterNode { return fakeValkeyNode{nodes, addr} }, nil)
	p := valkeyProvider{server: func() *redisadmin.Server { return srv }}
	ctx := context.Background()
	base := map[string]string{"region": redisadmin.Region, "mode": "CLUSTER_DISABLED", "shardCount": "1", "replicaCount": "0",
		"nodeType": "SHARED_CORE_NANO", "network": "projects/p/global/networks/default"}
	create := func(id string, extra map[string]string) {
		t.Helper()
		v := map[string]string{"id": id}
		for k, x := range base {
			v[k] = x
		}
		for k, x := range extra {
			v[k] = x
		}
		if _, err := p.Create(ctx, "p", v); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	primary := "projects/p/locations/" + redisadmin.Region + "/instances/vk-pri"
	create("vk-pri", map[string]string{"engineVersion": "VALKEY_7_2", "persistence": "AOF"})
	create("vk-sec", map[string]string{"primaryInstance": primary})
	_, props := valkeyPageProps(t, p, "vk-sec")
	if props["Cross-instance replication"] != "SECONDARY" || props["Primary instance"] != primary ||
		props["Replication membership"] == "—" || props["Encryption"] == "" {
		t.Errorf("secondary's page shows %v", props)
	}
	_, props = valkeyPageProps(t, p, "vk-pri")
	if props["Cross-instance replication"] != "PRIMARY" || props["Engine version"] != "VALKEY_7_2" || props["Persistence"] != "AOF" ||
		props["Secondary instances"] == "—" {
		t.Errorf("primary's page shows %v", props)
	}
	acts := p.DetailActions(ctx, "p", []string{"vk-sec"})
	if !msHasAction(acts, "switchover") || !msHasAction(acts, "detach-replication") || msHasAction(acts, "start-migration") {
		t.Errorf("secondary's actions = %v", acts)
	}
	if err := p.ActAt(ctx, "p", []string{"vk-sec"}, "detach-replication", nil); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if _, props = valkeyPageProps(t, p, "vk-sec"); props["Cross-instance replication"] != "NONE" {
		t.Errorf("after Detach the page shows %v", props)
	}

	if acts := p.DetailActions(ctx, "p", []string{"vk-sec"}); !msHasAction(acts, "start-migration") {
		t.Errorf("an instance with no replication: actions = %v", acts)
	}
	if err := p.ActAt(ctx, "p", []string{"vk-sec"}, "start-migration", map[string]string{"ip": "127.0.0.1", "port": "6390"}); err != nil {
		t.Fatalf("Start migration: %v", err)
	}
	if _, props = valkeyPageProps(t, p, "vk-sec"); props["State"] != "MIGRATING" || props["Migration source"] != "127.0.0.1:6390" {
		t.Errorf("migrating: page shows %v", props)
	}
	if acts := p.DetailActions(ctx, "p", []string{"vk-sec"}); !msHasAction(acts, "finish-migration") {
		t.Errorf("migrating: actions = %v", acts)
	}
	if err := p.ActAt(ctx, "p", []string{"vk-sec"}, "finish-migration", nil); err != nil {
		t.Fatalf("Finish migration: %v", err)
	}
	if _, props = valkeyPageProps(t, p, "vk-sec"); props["State"] != "ACTIVE" || props["Migration"] != "MIGRATED" {
		t.Errorf("migrated: page shows %v", props)
	}

	// Edit's Engine version (an upgrade is TestConsoleMemorystore1557 in
	// the browser, on real nodes).
	d, _ := valkeyPageProps(t, p, "vk-pri")
	found := false
	for _, f := range d.Edit.Fields {
		if f.Name == "engineVersion" && f.Default == "VALKEY_7_2" && len(f.Options) == 3 {
			found = true
		}
	}
	if !found {
		t.Errorf("Edit has no Engine version field: %+v", d.Edit.Fields)
	}
}

// TestConsoleValkeySecondaryWithTLS (#1563): Create instance makes a
// secondary with in-transit encryption of a primary with it (Enable
// in-transit encryption and Secondary of), and the page shows both; a
// secondary without the primary's in-transit encryption is refused.
func TestConsoleValkeySecondaryWithTLS(t *testing.T) {
	srv := redisadmin.NewServer(redisadmin.Instance{Project: "p", Host: "127.0.0.1", Port: 9016, Created: time.Unix(100, 0)}, valkeyNoBackend{})
	srv.EnableCreate(context.Background(), valkeyNoProv{}, func(string, string, string) redisadmin.DataPlane { return nil }, nil, nil)
	nodes := &fakeValkeyNodes{running: map[string]string{}}
	srv.EnableValkey(context.Background(), nodes, func(addr, _, _ string) redisadmin.ClusterNode { return fakeValkeyNode{nodes, addr} }, nil)
	p := valkeyProvider{server: func() *redisadmin.Server { return srv }}
	ctx := context.Background()
	create := func(id string, extra map[string]string) error {
		v := map[string]string{"id": id, "region": redisadmin.Region, "mode": "CLUSTER_DISABLED", "shardCount": "1", "replicaCount": "0",
			"nodeType": "SHARED_CORE_NANO", "network": "projects/p/global/networks/default"}
		for k, x := range extra {
			v[k] = x
		}
		_, err := p.Create(ctx, "p", v)
		return err
	}
	primary := "projects/p/locations/" + redisadmin.Region + "/instances/vk-tls-pri"
	if err := create("vk-tls-pri", map[string]string{"transitEncryption": "true"}); err != nil {
		t.Fatalf("Create the primary: %v", err)
	}
	if err := create("vk-plain-sec", map[string]string{"primaryInstance": primary}); err == nil {
		t.Error("Create of a secondary without the primary's in-transit encryption succeeded")
	}
	if err := create("vk-tls-sec", map[string]string{"transitEncryption": "true", "primaryInstance": primary}); err != nil {
		t.Fatalf("Create the secondary: %v", err)
	}
	_, props := valkeyPageProps(t, p, "vk-tls-sec")
	if props["Cross-instance replication"] != "SECONDARY" || props["Primary instance"] != primary ||
		props["In-transit encryption"] != "SERVER_AUTHENTICATION" {
		t.Errorf("secondary's page shows %v", props)
	}
}
