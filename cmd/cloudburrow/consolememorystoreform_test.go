package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// memorystoreAllProps are a page's properties over every tab.
func memorystoreAllProps(d console.Detail) []console.Property {
	var out []console.Property
	for _, s := range d.Sections {
		for _, g := range s.Groups {
			out = append(out, g.Properties...)
		}
	}
	return out
}

// TestMemorystoreCreateFormsFollowGoogle (#1862): the three create pages
// carry Google's fields in Google's order, Security is one choice that
// holds TLS on, and Valkey 9.1 and the CA modes other than Per-instance
// are drawn as not available locally.
func TestMemorystoreCreateFormsFollowGoogle(t *testing.T) {
	names := func(fs []console.Field) string {
		var out []string
		for _, f := range fs {
			out = append(out, f.Name)
		}
		return strings.Join(out, ",")
	}
	byName := func(fs []console.Field, n string) console.Field {
		for _, f := range fs {
			if f.Name == n {
				return f
			}
		}
		t.Fatalf("no field %s", n)
		return console.Field{}
	}
	_, redis := (memorystoreProvider{}).CreateForm()
	if got, want := names(redis), "id,displayName,tier,region,zone,redisVersion,memorySizeGb,authEnabled,transitEncryption,authorizedNetwork,connectMode,reservedIpRange,redisConfigs,"+
		"readReplicas,replicaCount,customerManagedKey,labels"; got != want {
		t.Errorf("Create a Redis instance fields = %s, want %s", got, want)
	}
	if f := byName(redis, "tier"); f.OptionLabels["STANDARD_HA"] != "Standard" || f.Type != "radio" {
		t.Errorf("Tier = %+v, want Basic / Standard", f)
	}
	// docs.cloud.google.com/memorystore/docs/redis/create-instance-console
	// ends in "Click Create instance." (#1933).
	if (memorystoreProvider{}).CreateSubmit() != "Create instance" {
		t.Error("Create a Redis instance does not end in Create instance")
	}
	_, cluster := (redisClusterProvider{}).CreateForm()
	if got, want := names(cluster), "id,region,zoneDistribution,zone,nodeType,shardCount,replicaCount,network,security,transitEncryption,caMode,caPool,"+
		"encryption,kmsKey,aclPolicy,persistence,autoBackup,backupRetention,backupHour,maintenance,maintenanceDay,maintenanceHour,redisConfigs,restoreBackup"; got != want {
		t.Errorf("Create cluster fields = %s, want %s", got, want)
	}
	_, valkey := (valkeyProvider{}).CreateForm()
	if got, want := names(valkey), "id,region,zoneDistribution,zone,mode,nodeType,shardCount,replicaCount,network,security,transitEncryption,caMode,caPool,"+
		"encryption,kmsKey,aclPolicy,persistence,autoBackup,backupRetention,backupHour,maintenance,maintenanceDay,maintenanceHour,engineVersion,"+
		"engineConfigs,restoreBackup,primaryInstance"; got != want {
		t.Errorf("Create instance (Valkey) fields = %s, want %s", got, want)
	}
	for _, fs := range [][]console.Field{cluster, valkey} {
		if f := byName(fs, "replicaCount"); f.Default != "1" {
			t.Errorf("Replicas default %q, want Google's 1", f.Default)
		}
		if f := byName(fs, "transitEncryption"); f.CheckedWhen != "security=IAM_AUTH|TOKEN_AUTH" {
			t.Errorf("TLS is not held by Security: %q", f.CheckedWhen)
		}
		if f := byName(fs, "caMode"); len(f.UnavailableOptions) != 0 || len(f.Options) != 3 {
			t.Errorf("every CA mode is served (#1907, #1918): %+v", f)
		}
		if f := byName(fs, "caPool"); f.ShowWhen != "caMode=CUSTOMER_MANAGED" {
			t.Errorf("CA pool shows when %q", f.ShowWhen)
		}
		if f := byName(fs, "backupRetention"); f.Default != "35" {
			t.Errorf("Days to retain backups default %q, want 35", f.Default)
		}
	}
	if f := byName(valkey, "engineVersion"); f.UnavailableOptions["VALKEY_9_1"] == "" || f.OptionLabels["VALKEY_9_0"] != "Valkey 9.0" {
		t.Errorf("Choose version = %+v", f)
	}
	for _, tc := range []struct {
		values map[string]string
		mode   int32
		pool   string
	}{
		{map[string]string{"caMode": "SHARED"}, 0, ""},
		{map[string]string{"transitEncryption": "true", "caMode": "PER_INSTANCE"}, 0, ""},
		{map[string]string{"transitEncryption": "true", "caMode": "SHARED"}, 2, ""},
		{map[string]string{"security": "IAM_AUTH", "caMode": "CUSTOMER_MANAGED", "caPool": " projects/p/locations/us-central1/caPools/x "}, 3, "projects/p/locations/us-central1/caPools/x"},
	} {
		if mode, pool := memorystoreCAMode(tc.values); mode != tc.mode || pool != tc.pool {
			t.Errorf("memorystoreCAMode(%v) = %d, %q; want %d, %q", tc.values, mode, pool, tc.mode, tc.pool)
		}
	}
	for k, want := range map[string]bool{"IAM_AUTH": true, "TOKEN_AUTH": true, "DISABLED": false} {
		if memorystoreTLS(map[string]string{"security": k}) != want {
			t.Errorf("security %s: TLS = %v", k, !want)
		}
	}
	if on, h, r, err := memorystoreAutoBackup(map[string]string{"autoBackup": "true", "backupRetention": "7", "backupHour": "3"}); !on || h != 3 || r != 7*24*time.Hour || err != nil {
		t.Errorf("automated backups = %v %d %v %v", on, h, r, err)
	}
	if _, _, _, err := memorystoreAutoBackup(map[string]string{"autoBackup": "true", "backupRetention": "366"}); err == nil {
		t.Error("366 days to retain backups accepted")
	}
}

// TestValkeyCreateKeepsZonesBackupsAndMaintenance (#1862): Create instance's
// Single zone, Automated daily backups and Custom maintenance reach
// CreateInstance, and the page's tabs show them.
func TestValkeyCreateKeepsZonesBackupsAndMaintenance(t *testing.T) {
	ctx := context.Background()
	srv := valkeyTestServer(t)
	vp := valkeyProvider{server: func() *redisadmin.Server { return srv }}
	if _, err := vp.Create(ctx, "p", map[string]string{"id": "vk-google", "region": redisadmin.Region, "mode": "CLUSTER", "shardCount": "1",
		"replicaCount": "0", "nodeType": "SHARED_CORE_NANO", "zoneDistribution": "SINGLE_ZONE", "zone": redisadmin.Region + "-b",
		"autoBackup": "true", "backupRetention": "7", "backupHour": "4", "maintenance": "CUSTOM", "maintenanceDay": "TUESDAY", "maintenanceHour": "5",
		"security": "DISABLED", "encryption": "GOOGLE", "kmsKey": "ignored", "persistence": "DISABLED"}); err != nil {
		t.Fatalf("Create instance: %v", err)
	}
	d, err := vp.Detail(ctx, "p", []string{"vk-google"})
	if err != nil {
		t.Fatal(err)
	}
	var tabs []string
	for _, s := range d.Sections {
		tabs = append(tabs, s.ID)
	}
	if got := strings.Join(tabs, ","); !strings.HasPrefix(got, "overview,configuration,connections,") || !strings.HasSuffix(got, ",maintenance,monitoring") {
		t.Errorf("tabs = %s; want Overview, Configuration, Connections, ..., Maintenance, Monitoring", got)
	}
	props := map[string]string{}
	for _, pr := range memorystoreAllProps(d) {
		props[pr.Label] = pr.Value
	}
	if props["Zonal availability"] != "Single zone "+redisadmin.Region+"-b" || props["Automated backups"] != "Daily at 04:00 - 05:00 UTC, kept 7 days" ||
		props["Customer-managed key"] != "—" || props["Node type"] != "shared-core-nano" {
		t.Errorf("page = %v", props)
	}
	if mon := sectionByID(d, "monitoring"); mon == nil || !strings.HasPrefix(mon.Unavailable, "Not available locally") {
		t.Errorf("Monitoring tab = %+v", mon)
	}
	if !strings.Contains(strings.Join(func() []string {
		var out []string
		for _, p := range sectionByID(d, "maintenance").Groups[0].Properties {
			out = append(out, p.Value)
		}
		return out
	}(), " "), "TUESDAY") {
		t.Errorf("Maintenance tab = %+v", sectionByID(d, "maintenance"))
	}
	if d.Edit == nil || d.Edit.Label != "Edit" || d.Edit.Submit != "Save" {
		t.Errorf("Edit = %+v", d.Edit)
	}
}

// valkeyTestServer is a Memorystore admin API over fake Valkey nodes.
func valkeyTestServer(t *testing.T) *redisadmin.Server {
	t.Helper()
	srv := redisadmin.NewServer(redisadmin.Instance{Project: "p", Host: "127.0.0.1", Port: 9016, Created: time.Unix(100, 0)}, valkeyNoBackend{})
	srv.EnableCreate(context.Background(), valkeyNoProv{}, func(string, string, string) redisadmin.DataPlane { return nil }, nil, nil)
	nodes := &fakeValkeyNodes{running: map[string]string{}}
	srv.EnableValkey(context.Background(), nodes, func(addr, _, _ string) redisadmin.ClusterNode { return fakeValkeyNode{nodes, addr} }, nil)
	return srv
}
