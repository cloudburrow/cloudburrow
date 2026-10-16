package sqladmin

import (
	"fmt"
	"strings"
	"testing"
)

// Every version Cloud SQL lists that an official, maintained image serves
// is served (#1381), and an upgrade goes only forward within an engine.
func TestServedVersionsAndUpgrades(t *testing.T) {
	for _, v := range []string{"POSTGRES_13", "POSTGRES_14", "POSTGRES_15", "POSTGRES_16", "POSTGRES_17", "POSTGRES_18", "MYSQL_8_0", "MYSQL_8_4"} {
		if !SupportedVersions[v] {
			t.Errorf("%s is not served", v)
		}
	}
	if SupportedVersions["MYSQL_5_7"] || !strings.Contains(unservedVersionWhy("MYSQL_5_7"), "no longer maintained") {
		t.Errorf("MYSQL_5_7: served %v, why %q", SupportedVersions["MYSQL_5_7"], unservedVersionWhy("MYSQL_5_7"))
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
		"POSTGRES_13": "400", "MYSQL_8_4": "400", "POSTGRES_12": "501", "BOGUS": "400",
	} {
		err := s.checkUpgrade(c, v)
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
