package components

import (
	"strings"
	"testing"
)

// TestCloudSQLReplicaRebuild (#1524): a PostgreSQL replica created as one
// takes a base backup only into an empty data directory; a former primary
// a switchover made a replica (Epoch > 0) replaces its data with one
// whenever the epoch its volume records differs, and records the epoch.
func TestCloudSQLReplicaRebuild(t *testing.T) {
	b := CloudSQLInstanceBackend("p", "POSTGRES_17", true, "pw", nil)
	fresh := CloudSQLReplicaBackend(b, false, SQLReplicaSource{Host: "sqlinst-r", Password: "x"})
	script := strings.Join(fresh.Command, " ")
	if !strings.Contains(script, `[ ! -s "$PGDATA/PG_VERSION" ]`) || strings.Contains(script, "cloudsql-rebuild-epoch") {
		t.Errorf("a new replica's script: %s", script)
	}
	rebuilt := CloudSQLReplicaBackend(b, false, SQLReplicaSource{Host: "sqlinst-r", Password: "x", Epoch: 2})
	script = strings.Join(rebuilt.Command, " ")
	// (pgAuthPrelude, after it, reads PG_VERSION to know an initialised
	// data directory, #2208; the rebuild's own test is the epoch's.)
	if rebuilt.Env["CLOUDSQL_REBUILD_EPOCH"] != "2" || strings.Contains(script, `[ ! -s "$PGDATA/PG_VERSION" ]`) ||
		strings.Count(script, "cloudsql-rebuild-epoch") != 2 || !strings.Contains(script, "pg_basebackup") {
		t.Errorf("a rebuilt replica: env %v script %s", rebuilt.Env["CLOUDSQL_REBUILD_EPOCH"], script)
	}
	if b.Env["CLOUDSQL_REBUILD_EPOCH"] != "" {
		t.Error("the primary's environment was changed")
	}
}
