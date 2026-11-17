package main

import (
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// A created Cloud SQL instance's server keeps its data on a volume in
// either mode (#1740): a flag, tier or edition change, instances.restart
// and a failover replace the server's pod, and Google keeps the data
// across them. Each server, and an HA standby, owns its claim, which
// Remove deletes with it; the Deployment recreates its pod, so the
// ReadWriteOnce volume is never wanted by two pods at once.
func TestSQLServerVolumeInEitherMode(t *testing.T) {
	specs := []sqladmin.Spec{
		{Name: "pg", DatabaseVersion: "POSTGRES_16", Flags: []sqladmin.Flag{{Name: "max_connections", Value: "50"}}},
		{Name: "my", DatabaseVersion: "MYSQL_8_0", RootPassword: "pw", Flags: []sqladmin.Flag{{Name: "max_connections", Value: "50"}}},
		{Name: "rep", DatabaseVersion: "POSTGRES_16", MasterInstanceName: "pg"},
	}
	for _, persistent := range []bool{false, true} {
		p := &sqlProvisioner{persistent: persistent}
		for _, spec := range specs {
			b := p.backend(spec)
			m := b.Manifest("cloudburrow", "w")
			claim := "sqlinst-" + spec.Name + "-data"
			for _, want := range []string{"kind: PersistentVolumeClaim\nmetadata:\n  name: " + claim, "claimName: " + claim, "type: Recreate"} {
				if !strings.Contains(m, want) {
					t.Errorf("persistent %v, %s: no %q in\n%s", persistent, spec.Name, want, m)
				}
			}
			if strings.Contains(m, "emptyDir") {
				t.Errorf("persistent %v, %s: an emptyDir in\n%s", persistent, spec.Name, m)
			}
		}
	}
}
