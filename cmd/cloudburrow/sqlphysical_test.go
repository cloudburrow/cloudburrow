package main

import (
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// A physical sync's Job (#1570) runs the XtraBackup image of the replica's
// major version on the replica's volume and, when it takes the backup, the
// source's, read-only; the restored replica starts with the init file and
// sets up replication from the backup's GTIDs.
func TestPhysicalSyncJobAndSetup(t *testing.T) {
	spec := sqladmin.Spec{Name: "rep", DatabaseVersion: "MYSQL_8_0", MasterInstanceName: "src-rep", RootPassword: `it's\x`,
		ExternalSyncStarted: true,
		External:            &sqladmin.ExternalSource{Host: "sqlinst-src", Port: 3306, User: "migrator", Password: "pw", Instance: "src", Physical: true}}
	m := physicalJob("cloudburrow", "w", "sqlxb-rep", spec, true)
	for _, want := range []string{"kind: Job", components.XtraBackup80Image, "claimName: sqlinst-rep-data", "claimName: sqlinst-src-data",
		"readOnly: true", "name: CLOUDSQL_TAKE_BACKUP", "backoffLimit: 0"} {
		if !strings.Contains(m, want) {
			t.Errorf("the job manifest has no %q:\n%s", want, m)
		}
	}
	if m := physicalJob("cloudburrow", "w", "sqlxb-rep", spec, false); strings.Contains(m, "sqlinst-src-data") || strings.Contains(m, "name: CLOUDSQL_TAKE_BACKUP") {
		t.Errorf("a job restoring an uploaded backup mounts the source:\n%s", m)
	}
	if sql := physicalInitSQL(`it's\x`); !strings.Contains(sql, `IDENTIFIED BY 'it''s\\x'`) || !strings.HasPrefix(sql, "SET sql_log_bin = 0;") {
		t.Errorf("init SQL:\n%s", sql)
	}
	if replicaSetup(spec) != mysqlPhysicalSetup {
		t.Error("a physical replica's setup is not mysqlPhysicalSetup")
	}
	p := &sqlProvisioner{persistent: true}
	if strings.Contains(strings.Join(p.backend(spec).Args, " "), "--init-file") {
		t.Error("the init file is passed before the backup is restored")
	}
	spec.External.PhysicalRestored = true
	if !strings.Contains(strings.Join(p.backend(spec).Args, " "), "--init-file=/var/lib/mysql/"+physicalInitFile) {
		t.Errorf("a restored replica's args: %v", p.backend(spec).Args)
	}
	if why := (&sqlProvisioner{}).PhysicalUnsupported(spec); !strings.Contains(why, "--mode persistent") {
		t.Errorf("an ephemeral instance's physical sync: %q", why)
	}
	spec.DatabaseVersion = "MYSQL_5_7"
	if why := p.PhysicalUnsupported(spec); !strings.Contains(why, "XtraBackup") {
		t.Errorf("MYSQL_5_7: %q", why)
	}
}
