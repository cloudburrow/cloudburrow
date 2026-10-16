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

// PostgreSQL 9.6 to 11 serve a primary, and refuse high availability,
// read replicas and point-in-time recovery, which need PostgreSQL 12
// (#1462); turning either on for 12 and later is not refused.
func TestPGBefore12Refusals(t *testing.T) {
	for v, want := range map[string]bool{"POSTGRES_9_6": true, "POSTGRES_10": true, "POSTGRES_11": true, "POSTGRES_12": false,
		"POSTGRES_17": false, "MYSQL_8_0": false} {
		if got := pgBefore12(v); got != want {
			t.Errorf("pgBefore12(%s) = %v", v, got)
		}
		ha := Spec{DatabaseVersion: v, AvailabilityType: "REGIONAL"}
		if err := ha.pgBefore12Unsupported(nil); (err != nil) != want || (err != nil && !strings.Contains(errStatus(err), "501")) {
			t.Errorf("REGIONAL %s: %v", v, err)
		}
		if err := ha.pgBefore12Unsupported(&ha); err != nil {
			t.Errorf("REGIONAL kept on %s: %v", v, err)
		}
	}
	if err := pgBefore12Why("POSTGRES_10", "a read replica"); err == nil || !strings.Contains(err.Error(), "pg_promote") {
		t.Errorf("a POSTGRES_10 replica: %v", err)
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
	if err := s.checkUpgrade(s.writes.byName["pgr"], "POSTGRES_17", false); err == nil || !strings.Contains(errStatus(err), "501") {
		t.Errorf("upgrading a PostgreSQL replica: %v, want 501", err)
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
