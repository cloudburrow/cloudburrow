package components

import (
	"strings"
	"testing"
)

// TestPoolerHBATrustsOnlyTheStatsUserOnLoopback pins the pooler's
// pg_hba-style rules (#2163): trust is for PoolerStatsUser alone, on the
// pooler's pgbouncer console and from the pod's loopback; that user is
// refused anywhere else; and every other sign-in is md5 (SCRAM where the
// server keeps SCRAM), as Cloud SQL's managed connection pooling asks for
// the user's password. No rule trusts any other user or address.
func TestPoolerHBATrustsOnlyTheStatsUserOnLoopback(t *testing.T) {
	_, rest, ok := strings.Cut(poolerScript, `cat > "$d/hba.conf" <<EOF`+"\n")
	if !ok {
		t.Fatal("the pooler script writes no hba.conf")
	}
	body, _, ok := strings.Cut(rest, "\nEOF\n")
	if !ok {
		t.Fatal("hba.conf's here-document has no end")
	}
	stats := "${CLOUDSQL_POOLER_STATS_USER}"
	want := []string{
		"host pgbouncer " + stats + " 127.0.0.1/32 trust",
		"host pgbouncer " + stats + " ::1/128 trust",
		"host all " + stats + " 0.0.0.0/0 reject",
		"host all " + stats + " ::/0 reject",
		"host all all 0.0.0.0/0 md5",
		"host all all ::/0 md5",
	}
	if got := strings.Split(body, "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("hba.conf:\n%s\nwant:\n%s", body, strings.Join(want, "\n"))
	}
	for _, l := range strings.Split(body, "\n") {
		f := strings.Fields(l)
		if f[len(f)-1] == "trust" && (f[1] != "pgbouncer" || f[2] != stats || (f[3] != "127.0.0.1/32" && f[3] != "::1/128")) {
			t.Errorf("rule %q trusts beyond the stats user on loopback", l)
		}
	}
	for _, s := range []string{"auth_type = hba\n", "stats_users = " + stats + "\n", "unix_socket_dir =\n"} {
		if !strings.Contains(poolerScript, s) {
			t.Errorf("pgbouncer.ini lacks %q", s)
		}
	}
	if strings.Contains(poolerScript, "admin_users") {
		t.Error("pgbouncer.ini names admin_users: the stats user must stay read-only")
	}
	if c := CloudSQLPooler(6432, nil); c.Env["CLOUDSQL_POOLER_STATS_USER"] != PoolerStatsUser {
		t.Errorf("the sidecar's stats user is %q, want %q", c.Env["CLOUDSQL_POOLER_STATS_USER"], PoolerStatsUser)
	}
}

// TestAlloyDBConnectorPoolerTrustsOnlyLoopback pins AlloyDB's two poolers
// (#2191): the direct one is Cloud SQL's, password-checked; the
// connectors' trusts only the pod's loopback, where the CLI's
// port-forward from the connectors' proxy arrives, and refuses every other
// address. The replacement must really happen: a no-op would leave the
// connectors' clients, which send no password, refused.
func TestAlloyDBConnectorPoolerTrustsOnlyLoopback(t *testing.T) {
	if !strings.Contains(poolerScript, poolerHBA) {
		t.Fatal("poolerHBA is not the pooler script's hba.conf")
	}
	cs := AlloyDBPoolers(nil)
	if len(cs) != 2 {
		t.Fatalf("%d poolers, want 2", len(cs))
	}
	direct, proxied := cs[0], cs[1]
	if direct.Port != AlloyDBPoolerPort || direct.Command[2] != poolerScript {
		t.Errorf("the direct pooler is not Cloud SQL's on %d", AlloyDBPoolerPort)
	}
	if proxied.Port != AlloyDBConnectorPoolerPort {
		t.Errorf("the connectors' pooler is on %d, want %d", proxied.Port, AlloyDBConnectorPoolerPort)
	}
	script := proxied.Command[2]
	if strings.Contains(script, poolerHBA) || !strings.Contains(script, connectorPoolerHBA) {
		t.Fatalf("the connectors' pooler does not use connectorPoolerHBA:\n%s", script)
	}
	for _, l := range strings.Split(strings.TrimSpace(connectorPoolerHBA), "\n") {
		f := strings.Fields(l)
		if f[len(f)-1] == "trust" && f[3] != "127.0.0.1/32" && f[3] != "::1/128" {
			t.Errorf("rule %q trusts beyond loopback", l)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(connectorPoolerHBA), "host all all ::/0 reject") {
		t.Error("the connectors' hba does not end refusing every other address")
	}
}
