package sqladmin

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Every version Cloud SQL lists that an official, maintained image serves
// is served (#1381), and an upgrade goes only forward within an engine.
func TestServedVersionsAndUpgrades(t *testing.T) {
	for _, v := range []string{"POSTGRES_9_6", "POSTGRES_10", "POSTGRES_11", "POSTGRES_12", "POSTGRES_13", "POSTGRES_14", "POSTGRES_15", "POSTGRES_16", "POSTGRES_17", "POSTGRES_18", "MYSQL_8_0", "MYSQL_8_4"} {
		if !SupportedVersions[v] {
			t.Errorf("%s is not served", v)
		}
	}
	for v, why := range map[string]string{"MYSQL_5_7": "linux/amd64 only", "MYSQL_5_6": "linux/amd64 only",
		"SQLSERVER_2022_STANDARD": "linux/amd64 only", "MYSQL_8_0_31": "MYSQL_8_0", "POSTGRES_8_4": "POSTGRES_9_6"} {
		if SupportedVersions[v] || !strings.Contains(unservedVersionWhy(v), why) {
			t.Errorf("%s: served %v, why %q, want it to say %q", v, SupportedVersions[v], unservedVersionWhy(v), why)
		}
	}
	got := upgradable("POSTGRES_16")
	if len(got) != 2 || got[0].(map[string]any)["name"] != "POSTGRES_17" || got[1].(map[string]any)["displayName"] != "PostgreSQL 18" {
		t.Errorf("upgradable(POSTGRES_16) = %v", got)
	}
	if got := upgradable("MYSQL_8_4"); len(got) != 0 {
		t.Errorf("upgradable(MYSQL_8_4) = %v, want none", got)
	}
	s := &Server{writes: &instances{byName: map[string]*created{}}}
	c := &created{spec: Spec{Name: "a", Project: "p", DatabaseVersion: "POSTGRES_17"}}
	for v, want := range map[string]string{
		"POSTGRES_13": "400", "MYSQL_8_4": "400", "POSTGRES_12": "400", "POSTGRES_8_4": "501", "BOGUS": "400",
	} {
		err := s.checkUpgrade(c, v, false)
		if err == nil || !strings.Contains(errStatus(err), want) {
			t.Errorf("upgrade to %s: %v, want %s", v, err, want)
		}
	}
}

func errStatus(err error) string {
	if ae, ok := err.(*apiError); ok {
		return fmt.Sprint(ae.status, " ", ae.code)
	}
	return err.Error()
}

// pgBefore12 is PostgreSQL 9.6 to 11 (#1462), whose standbys and
// recoveries use recovery.conf (#1524).
func TestPGBefore12(t *testing.T) {
	for v, want := range map[string]bool{"POSTGRES_9_6": true, "POSTGRES_10": true, "POSTGRES_11": true, "POSTGRES_12": false,
		"POSTGRES_17": false, "MYSQL_8_0": false} {
		if got := pgBefore12(v); got != want {
			t.Errorf("pgBefore12(%s) = %v", v, got)
		}
	}
}

// A MySQL read replica is upgraded on its own, before its primary, as
// Cloud SQL documents; a PostgreSQL one is 501, since physical
// replication does not cross major versions (#1462).
func TestReplicaUpgradeOnItsOwn(t *testing.T) {
	s := &Server{writes: &instances{byName: map[string]*created{}, prov: upgraderProv{newFakeProv()}}}
	for _, c := range []*created{
		{spec: Spec{Name: "my", Project: "p", DatabaseVersion: "MYSQL_8_0"}},
		{spec: Spec{Name: "myr", Project: "p", DatabaseVersion: "MYSQL_8_0", MasterInstanceName: "my"}},
		{spec: Spec{Name: "pg", Project: "p", DatabaseVersion: "POSTGRES_16"}},
		{spec: Spec{Name: "pgr", Project: "p", DatabaseVersion: "POSTGRES_16", MasterInstanceName: "pg"}},
	} {
		s.writes.byName[c.spec.Name] = c
	}
	if err := s.checkUpgrade(s.writes.byName["myr"], "MYSQL_8_4", false); err != nil {
		t.Errorf("upgrading a MySQL replica: %v", err)
	}
	// A PostgreSQL replica too (#1524): it then waits for its primary.
	if err := s.checkUpgrade(s.writes.byName["pgr"], "POSTGRES_17", false); err != nil {
		t.Errorf("upgrading a PostgreSQL replica: %v", err)
	}
	if err := s.awaitPrimary("p", "pgr", "POSTGRES_17", false); err != nil {
		t.Fatalf("awaitPrimary: %v", err)
	}
	if r := s.writes.byName["pgr"]; !r.spec.AwaitingPrimary || r.spec.DatabaseVersion != "POSTGRES_17" || r.state != "RUNNABLE" {
		t.Errorf("the PostgreSQL replica after its upgrade: %+v %s", r.spec, r.state)
	}
	if _, err := s.replicaVerb("", "p", "pgr", "promoteReplica"); err == nil || !strings.Contains(errStatus(err), "501") {
		t.Errorf("promoting a replica waiting for its primary: %v, want 501", err)
	}
	// Its primary is not upgraded to an older version than the replica's:
	// that would roll the replica back.
	for _, include := range []bool{false, true} {
		if err := s.checkUpgrade(s.writes.byName["pg"], "POSTGRES_16", include); err == nil || !strings.Contains(errStatus(err), "400") {
			t.Errorf("upgrading the primary below its replica (include %v): %v, want 400", include, err)
		}
	}
	// Its primary is no longer behind it.
	if err := s.checkUpgrade(s.writes.byName["pg"], "POSTGRES_17", false); err != nil {
		t.Errorf("upgrading the PostgreSQL primary after its replica: %v", err)
	}
	// An external replica is not upgraded in place.
	s.writes.byName["ext"] = &created{spec: Spec{Name: "ext", Project: "p", DatabaseVersion: "POSTGRES_16", MasterInstanceName: "src",
		External: &ExternalSource{}}}
	if err := s.checkUpgrade(s.writes.byName["ext"], "POSTGRES_17", false); err == nil || !strings.Contains(errStatus(err), "400") {
		t.Errorf("upgrading an external replica: %v, want 400", err)
	}
	// The primary's replica is behind: 400 without includeReplicas.
	if err := s.checkUpgrade(s.writes.byName["my"], "MYSQL_8_4", false); err == nil || !strings.Contains(errStatus(err), "400") {
		t.Errorf("upgrading the primary first: %v, want 400", err)
	}
	s.writes.byName["myr"].spec.DatabaseVersion = "MYSQL_8_4"
	if err := s.checkUpgrade(s.writes.byName["my"], "MYSQL_8_4", false); err != nil {
		t.Errorf("upgrading the primary after its replica: %v", err)
	}
}

// upgraderProv is a fake provisioner that can upgrade.
type upgraderProv struct{ *fakeProv }

func (upgraderProv) Upgrade(context.Context, Spec, Spec) error { return nil }

// legacyProv is a provisioner whose node runs, or does not run, MySQL
// 5.7's linux/amd64 image.
type legacyProv struct {
	*fakeProv
	why string
}

func (p legacyProv) LegacyUnrunnable(string) string { return p.why }

// legacyUpgrader is a legacyProv that can upgrade.
type legacyUpgrader struct{ legacyProv }

func (legacyUpgrader) Upgrade(context.Context, Spec, Spec) error { return nil }

// MYSQL_5_7 and MYSQL_5_6 are served where the node runs their images
// (#1524), with their clones and major version upgrades, and their
// replicas, high availability and point-in-time recovery are 501.
func TestLegacyMySQLServed(t *testing.T) {
	runs := &Server{writes: &instances{prov: legacyProv{}, byName: map[string]*created{}}}
	if ok, why := runs.versionServed("MYSQL_5_7"); !ok {
		t.Errorf("MYSQL_5_7 on a node that runs it: %q", why)
	}
	if ok, why := runs.versionServed("MYSQL_5_6"); !ok {
		t.Errorf("MYSQL_5_6 on a node that runs it: %q", why)
	}
	if ok, why := runs.versionServed("MYSQL_5_5"); ok || why == "" {
		t.Errorf("MYSQL_5_5: %v %q", ok, why)
	}
	cannot := &Server{writes: &instances{prov: legacyProv{why: "the node is aarch64"}, byName: map[string]*created{}}}
	if ok, why := cannot.versionServed("MYSQL_5_7"); ok || why != "the node is aarch64" {
		t.Errorf("MYSQL_5_7 on a node that cannot run it: %v %q", ok, why)
	}
	plain := &Server{writes: &instances{prov: &fakeProv{}, byName: map[string]*created{}}}
	if ok, why := plain.versionServed("MYSQL_5_7"); ok || !strings.Contains(why, "linux/amd64 only") {
		t.Errorf("MYSQL_5_7 with no LegacyRunner: %v %q", ok, why)
	}
	base := Spec{Name: "a", DatabaseVersion: "MYSQL_5_7"}
	if err := legacyRefusal(base); err != nil {
		t.Errorf("a standalone ZONAL MYSQL_5_7 instance: %v", err)
	}
	// Read replicas, REGIONAL and external replicas run in 5.7's and
	// 5.6's statements (#1524); a physical sync is XtraBackup 8.0's.
	replica, regional, external, physical := base, base, base, base
	replica.MasterInstanceName, regional.AvailabilityType = "p", "REGIONAL"
	external.External = &ExternalSource{}
	physical.External = &ExternalSource{Physical: true}
	for _, sp := range []Spec{replica, regional, external} {
		if err := legacyRefusal(sp); err != nil {
			t.Errorf("%+v: %v", sp, err)
		}
	}
	if err := legacyRefusal(physical); err == nil || !strings.Contains(errStatus(err), "501") {
		t.Errorf("a MYSQL_5_7 physical external sync: %v, want 501", err)
	}
	// A major version upgrade goes one version at a time: 5.6 to 5.7, 5.7
	// to 8.0, where the node runs the images.
	up := &Server{writes: &instances{prov: legacyUpgrader{legacyProv{}}, byName: map[string]*created{}}}
	for _, tc := range []struct{ from, to, want string }{
		{"MYSQL_5_7", "MYSQL_8_0", ""}, {"MYSQL_5_6", "MYSQL_5_7", ""}, {"MYSQL_5_6", "MYSQL_8_0", "400"},
		{"MYSQL_5_7", "MYSQL_8_4", "400"}, {"MYSQL_8_0", "MYSQL_5_7", "400"}, {"MYSQL_5_7", "MYSQL_5_6", "400"},
	} {
		err := up.checkUpgrade(&created{spec: Spec{Name: "a", DatabaseVersion: tc.from}}, tc.to, false)
		if (tc.want == "" && err != nil) || (tc.want != "" && (err == nil || !strings.Contains(errStatus(err), tc.want))) {
			t.Errorf("upgrading %s to %s: %v, want %q", tc.from, tc.to, err, tc.want)
		}
	}
	noAMD := &Server{writes: &instances{prov: legacyUpgrader{legacyProv{why: "the node is aarch64"}}, byName: map[string]*created{}}}
	if err := noAMD.checkUpgrade(&created{spec: Spec{Name: "a", DatabaseVersion: "MYSQL_5_6"}}, "MYSQL_5_7", false); err == nil ||
		!strings.Contains(errStatus(err), "501") {
		t.Errorf("upgrading MYSQL_5_6 to MYSQL_5_7 where the node cannot run it: %v, want 501", err)
	}
	for v, want := range map[string]string{"MYSQL_5_6": "MYSQL_5_7", "MYSQL_5_7": "MYSQL_8_0", "MYSQL_8_0": "MYSQL_8_4"} {
		if got := upgradable(v); len(got) != 1 || got[0].(map[string]any)["name"] != want {
			t.Errorf("upgradable(%s) = %v, want %s only", v, got, want)
		}
	}
	if err := up.copyRefusal("MYSQL_5_6", "instances.clone"); err != nil {
		t.Errorf("cloning a MYSQL_5_6 instance: %v", err)
	}
	if err := noAMD.copyRefusal("MYSQL_5_7", "instances.clone"); err == nil || !strings.Contains(errStatus(err), "501") {
		t.Errorf("cloning a MYSQL_5_7 instance where the node cannot run it: %v, want 501", err)
	}
	if err := legacyRefusal(Spec{Name: "b", DatabaseVersion: "MYSQL_5_6", MasterInstanceName: "p"}); err != nil {
		t.Errorf("a MYSQL_5_6 read replica: %v", err)
	}
	for _, v := range []string{"MYSQL_5_6", "MYSQL_5_7"} {
		if err := limitedUnimplemented(v, "a blue-green deployment"); err != nil {
			t.Errorf("limitedUnimplemented(%s): %v", v, err)
		}
	}
	if err := mysql56BlueGreenErr("a"); !strings.Contains(errStatus(err), "400") {
		t.Errorf("a MYSQL_5_6 blue-green deployment: %v, want 400", err)
	}
}

// MySQL 8.0's replication statements in 5.7's and 5.6's names (#1524).
func TestMySQLDialect(t *testing.T) {
	const in = "CHANGE REPLICATION SOURCE TO SOURCE_HOST = 'h', SOURCE_PORT = 3306, SOURCE_AUTO_POSITION = 1, SOURCE_SSL = 1; " +
		"START REPLICA; SET PERSIST super_read_only = ON; START REPLICA UNTIL SOURCE_LOG_FILE = 'f', SOURCE_LOG_POS = 4; " +
		"SET PERSIST rpl_semi_sync_source_wait_point = 'AFTER_SYNC'; SET PERSIST rpl_semi_sync_source_wait_for_replica_count = 1; " +
		"SET PERSIST rpl_semi_sync_source_enabled = ON; INSTALL PLUGIN rpl_semi_sync_replica SONAME 'semisync_replica.so'; " +
		"sed -n 's/^ *Relay_Source_Log_File: //p'; echo $CLOUDSQL_SOURCE_HOST; SHOW REPLICA STATUS; " +
		"SELECT VARIABLE_VALUE FROM performance_schema.global_status"
	if got := MySQLDialect("MYSQL_8_0_41", in); got != in {
		t.Errorf("8.0: %q", got)
	}
	want57 := "CHANGE MASTER TO MASTER_HOST = 'h', MASTER_PORT = 3306, MASTER_AUTO_POSITION = 1, MASTER_SSL = 1; " +
		"START SLAVE; SET GLOBAL super_read_only = ON; START SLAVE UNTIL MASTER_LOG_FILE = 'f', MASTER_LOG_POS = 4; " +
		"SET GLOBAL rpl_semi_sync_master_wait_point = 'AFTER_SYNC'; SET GLOBAL rpl_semi_sync_master_wait_for_slave_count = 1; " +
		"SET GLOBAL rpl_semi_sync_master_enabled = ON; INSTALL PLUGIN rpl_semi_sync_slave SONAME 'semisync_slave.so'; " +
		"sed -n 's/^ *Relay_Master_Log_File: //p'; echo $CLOUDSQL_SOURCE_HOST; SHOW SLAVE STATUS; " +
		"SELECT VARIABLE_VALUE FROM performance_schema.global_status"
	if got := MySQLDialect("MYSQL_5_7", in); got != want57 {
		t.Errorf("5.7:\n got %q\nwant %q", got, want57)
	}
	want56 := "CHANGE MASTER TO MASTER_HOST = 'h', MASTER_PORT = 3306, MASTER_AUTO_POSITION = 1, MASTER_SSL = 0; " +
		"START SLAVE; SET GLOBAL read_only = ON; START SLAVE UNTIL MASTER_LOG_FILE = 'f', MASTER_LOG_POS = 4; " +
		"SET GLOBAL rpl_semi_sync_master_enabled = ON; INSTALL PLUGIN rpl_semi_sync_slave SONAME 'semisync_slave.so'; " +
		"sed -n 's/^ *Relay_Master_Log_File: //p'; echo $CLOUDSQL_SOURCE_HOST; SHOW SLAVE STATUS; " +
		"SELECT VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS"
	if got := MySQLDialect("MYSQL_5_6", in); got != want56 {
		t.Errorf("5.6:\n got %q\nwant %q", got, want56)
	}
	if got := mysqlUpsertUser("MYSQL_5_6", "u", "p", "REPLICATION SLAVE"); len(got) != 1 || !strings.HasPrefix(got[0], "GRANT REPLICATION SLAVE ON *.* TO 'u'@'%' IDENTIFIED BY ") {
		t.Errorf("5.6 user: %q", got)
	}
}
