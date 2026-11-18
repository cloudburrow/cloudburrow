//go:build compat

package compat

import (
	"os"
	"strings"
	"testing"
)

// TestCloudSQLHighAvailabilityBeforePostgres13 (#1524): what
// TestCloudSQLHighAvailability does for PostgreSQL 17, on 9.6, 11 and 12,
// through the official Go client and connector: a REGIONAL instance whose
// standby (recovery.conf before 12) is synchronous, a read replica,
// instances.failover promoting the standby (pg_ctl promote before 12,
// which has no pg_promote()) with every committed row kept, the old
// primary rebuilt as the new standby, the replica following the new
// timeline, and ZONAL removing the standby. WAL is kept with
// wal_keep_segments before 13.
// covers: sql.instances.insert, sql.instances.failover, sql.instances.patch
func TestCloudSQLHighAvailabilityBeforePostgres13(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	for _, version := range []string{"POSTGRES_9_6", "POSTGRES_11", "POSTGRES_12"} {
		e := haPostgres
		e.version = version
		e.short = "pg" + strings.ReplaceAll(strings.TrimPrefix(version, "POSTGRES_"), "_", "")
		t.Run(version, func(t *testing.T) {
			h := h.For(t)
			testHighAvailability(t, h, e)
		})
	}
}
