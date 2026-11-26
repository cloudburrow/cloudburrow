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
	m := physicalJob("cloudburrow", "w", "sqlxb-rep", spec, true, "adm")
	for _, want := range []string{"kind: Job", components.XtraBackup80Image, "claimName: sqlinst-rep-data", "claimName: sqlinst-src-data",
		"readOnly: true", "name: CLOUDSQL_TAKE_BACKUP", "backoffLimit: 0"} {
		if !strings.Contains(m, want) {
			t.Errorf("the job manifest has no %q:\n%s", want, m)
		}
	}
	if m := physicalJob("cloudburrow", "w", "sqlxb-rep", spec, false, "adm"); strings.Contains(m, "sqlinst-src-data") || strings.Contains(m, "name: CLOUDSQL_TAKE_BACKUP") {
		t.Errorf("a job restoring an uploaded backup mounts the source:\n%s", m)
	}
	if sql := physicalInitSQL("MYSQL_8_0", `it's\x`, "adm-pw"); !strings.Contains(sql, `IDENTIFIED BY 'it''s\\x'`) || !strings.HasPrefix(sql, "SET sql_log_bin = 0;") ||
		!strings.Contains(sql, `'cloudsqladmin'@'127.0.0.1' IDENTIFIED BY 'adm-pw'`) {
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
	// An ephemeral instance's replica (#1614): served, on its volume (a
	// created server has one in either mode, #1740), started by
	// physicalWrapper, which passes the init file itself, so the restore
	// changes nothing in its pod's template.
	eph := &sqlProvisioner{}
	if why := eph.PhysicalUnsupported(spec); why != "" {
		t.Errorf("an ephemeral instance's physical sync: %q", why)
	}
	for _, restored := range []bool{false, true} {
		spec.External.PhysicalRestored = restored
		b := eph.backend(spec)
		m := b.Manifest("cloudburrow", "w")
		if !b.Persistent || !strings.Contains(m, "claimName: sqlinst-") || !strings.Contains(m, "mountPath: /var/lib/mysql") ||
			strings.Contains(m, "emptyDir") || strings.Contains(strings.Join(b.Args, " "), "--init-file") ||
			len(b.Command) != 4 || b.Command[2] != physicalWrapper {
			t.Errorf("an ephemeral physical replica (restored %v): command %q args %v\n%s", restored, b.Command, b.Args, m)
		}
	}
	other := spec
	other.External = &sqladmin.ExternalSource{Host: "h", Instance: "src"}
	if b := eph.backend(other); len(b.Command) != 0 {
		t.Errorf("an ephemeral logical replica has the physical start: %+v", b.Command)
	}
	for _, want := range []string{"mv " + physicalRestoredDir + " data", `--init-file=/var/lib/mysql/` + physicalInitFile, `exec docker-entrypoint.sh "$@"`} {
		if !strings.Contains(physicalWrapper, want) {
			t.Errorf("physicalWrapper has no %q", want)
		}
	}
	// MySQL 5.7 (#1524): XtraBackup 2.4, which may lack 8.0's
	// --no-server-version-check, and the server's open-files limit.
	spec.DatabaseVersion = "MYSQL_5_7"
	if why := p.PhysicalUnsupported(spec); why != "" {
		t.Errorf("MYSQL_5_7: %q", why)
	}
	if m := physicalJob("cloudburrow", "w", "sqlxb-rep", spec, true, "adm"); !strings.Contains(m, components.XtraBackup24Image) {
		t.Errorf("a MYSQL_5_7 job's image:\n%s", m)
	}
	for _, s := range []string{physicalScript, physicalStreamScript} {
		if !strings.Contains(s, physicalVersionCheck) || strings.Contains(s, "--backup --no-server-version-check") {
			t.Errorf("a backup script passes --no-server-version-check unconditionally:\n%s", s)
		}
	}
	if st := physicalStart("MYSQL_5_7"); !strings.HasPrefix(st, "set -e\nulimit -n 1048576\n") || physicalStart("MYSQL_8_0") != physicalWrapper {
		t.Errorf("physicalStart(MYSQL_5_7):\n%s", st)
	}
	for _, v := range []string{"MYSQL_5_6", "MYSQL_8_4"} {
		spec.DatabaseVersion = v
		if why := p.PhysicalUnsupported(spec); !strings.Contains(why, "XtraBackup") {
			t.Errorf("%s: %q", v, why)
		}
	}
}
