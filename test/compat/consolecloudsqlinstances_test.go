//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestConsoleCloudSQLInstancesCreateRestartAndDelete (#1330): the Cloud
// SQL instances page creates an instance through the Admin API, which the
// official client then reads RUNNABLE once its server serves; Restart and
// Delete act on it, deletion protection refuses Delete with the API's
// words, and the instance `up` runs is refused.
func TestConsoleCloudSQLInstancesCreateRestartAndDelete(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	addr := consoleAddr(t, h)
	svc := sqlAdminService(t, h)
	project := h.Project()
	name := fmt.Sprintf("console-pg-%d", time.Now().UnixNano()%1e6)
	running := func() {
		t.Helper()
		for i := 0; i < 120; i++ {
			in, err := svc.Instances.Get(project, name).Do()
			if err == nil && in.State == "RUNNABLE" {
				return
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("%s never became RUNNABLE", name)
	}
	del := func() (int, string) {
		return consoleDo(t, addr, http.MethodDelete, "/api/resources/cloudsql-instances?project="+project+"&name="+name, "")
	}
	code, out := consoleCreate(t, addr, "cloudsql-instances", project, map[string]string{
		"name": name, "databaseVersion": "POSTGRES_17", "tier": "db-custom-1-3840", "deletionProtection": "true"})
	if code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	t.Cleanup(func() {
		if op, err := svc.Instances.Patch(project, name, sqlProtection(false)).Do(); err == nil {
			waitSQLOp(t, svc, project, op)
			_, _ = svc.Instances.Delete(project, name).Do()
		}
	})
	running()
	if code, out := del(); code == http.StatusOK || !strings.Contains(out, "protected") {
		t.Errorf("delete of a protected instance = %d: %s", code, out)
	}
	code, out = consoleDo(t, addr, http.MethodGet, "/api/resources/cloudsql-instances?project="+project, "")
	if code != http.StatusOK || !strings.Contains(out, name) || !strings.Contains(out, project+":us-central1:"+name) {
		t.Errorf("list = %d: %s", code, out)
	}

	code, out = consoleDo(t, addr, http.MethodPost, "/api/actions/cloudsql-instances?project="+project,
		fmt.Sprintf(`{"Name":%q,"Action":"restart"}`, name))
	if code != http.StatusOK {
		t.Fatalf("restart = %d: %s", code, out)
	}
	time.Sleep(2 * time.Second)
	running()

	// Lift the protection through the API, as gcloud would, then delete.
	op, err := svc.Instances.Patch(project, name, sqlProtection(false)).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	if code, out := del(); code != http.StatusOK {
		t.Fatalf("delete = %d: %s", code, out)
	}
	for i := 0; i < 120; i++ {
		if _, err := svc.Instances.Get(project, name).Do(); err != nil {
			wantHTTP(t, "get after the console's delete", err, http.StatusNotFound)
			break
		}
		time.Sleep(time.Second)
	}

	code, out = consoleDo(t, addr, http.MethodDelete, "/api/resources/cloudsql-instances?project="+project+"&name=postgres", "")
	if code == http.StatusOK || !strings.Contains(out, "cloudburrow up") {
		t.Errorf("delete of the instance up runs = %d: %s", code, out)
	}
}

// TestConsoleCloudSQLInstanceEditAndBackups (#1330): on a created
// instance's page, Edit instance changes the tier, labels, a database flag,
// deletion protection, the automated backups' start time, retained backups
// and transaction log retention days (one out of range refused) in one
// instances.patch, which the official client (sqladmin/v1) reads back; the
// Backups tab's Create backup takes a backup the client lists, Restore
// loads it back over a changed row, Delete backup removes it, and Clone
// makes a new instance holding the same row.
func TestConsoleCloudSQLInstanceEditAndBackups(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	addr := consoleAddr(t, h)
	svc := sqlAdminService(t, h)
	project := h.Project()
	name := fmt.Sprintf("console-bk-%d", time.Now().UnixNano()%1e6)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, n := range []string{name, name + "-cl"} {
			if op, err := svc.Instances.Patch(project, n, sqlProtection(false)).Do(); err == nil {
				waitSQLOp(t, svc, project, op)
				if d, err := svc.Instances.Delete(project, n).Do(); err == nil {
					waitSQLOp(t, svc, project, d)
				}
			}
		}
	})
	waitSQLOp(t, svc, project, op)
	running := func(n string) {
		t.Helper()
		for i := 0; i < 180; i++ {
			if in, err := svc.Instances.Get(project, n).Do(); err == nil && in.State == "RUNNABLE" {
				return
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("%s never became RUNNABLE", n)
	}

	code, out := consoleEdit(t, addr, "cloudsql-instances", project, []string{name}, map[string]string{
		"tier": "db-custom-2-7680", "labels": "env=console,team=a", "flags": "max_connections=44",
		"deletionProtection": "true", "backupsEnabled": "true", "backupStartTime": "04:30",
		"retainedBackups": "5", "logRetentionDays": "3"})
	if code != http.StatusOK {
		t.Fatalf("edit = %d: %s", code, out)
	}
	time.Sleep(2 * time.Second)
	running(name)
	in, err := svc.Instances.Get(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if st := in.Settings; st.Tier != "db-custom-2-7680" || st.UserLabels["env"] != "console" || st.UserLabels["team"] != "a" ||
		len(st.DatabaseFlags) != 1 || st.DatabaseFlags[0].Value != "44" || !st.DeletionProtectionEnabled ||
		!st.BackupConfiguration.Enabled || st.BackupConfiguration.StartTime != "04:30" ||
		st.BackupConfiguration.TransactionLogRetentionDays != 3 || st.BackupConfiguration.BackupRetentionSettings == nil ||
		st.BackupConfiguration.BackupRetentionSettings.RetainedBackups != 5 {
		t.Errorf("after the console's edit: %+v %+v", st, st.BackupConfiguration)
	}
	if code, out := consoleEdit(t, addr, "cloudsql-instances", project, []string{name}, map[string]string{
		"tier": "db-custom-2-7680", "deletionProtection": "true", "backupsEnabled": "true", "logRetentionDays": "36"}); code == http.StatusOK ||
		!strings.Contains(out, "transactionLogRetentionDays") {
		t.Errorf("an edit to 36 days of log = %d: %s; want the API's refusal", code, out)
	}
	if code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/cloudsql-instances?project="+project+"&name=postgres", ""); code != http.StatusOK ||
		strings.Contains(out, `"edit"`) {
		t.Errorf("the page of the instance up runs offers Edit instance = %d: %s", code, out)
	}

	c := sqlBackupCase{table: "cbk_rows", register: pgRegister, dsn: func(_, conn string) string {
		return fmt.Sprintf("host=%s user=postgres dbname=postgres sslmode=disable", conn)
	}}
	conn := project + ":us-central1:" + name
	sqlBackupRows(t, h, c, conn, "CREATE TABLE cbk_rows (id INT PRIMARY KEY, name TEXT)", "INSERT INTO cbk_rows VALUES (1, 'a')")
	act := func(action string, values map[string]string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": []string{name}, "Action": action, "Values": values})
		if code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/cloudsql-instances?project="+project, string(body)); code != http.StatusOK {
			t.Fatalf("console %s = %d: %s", action, code, out)
		}
	}
	act("create-backup", map[string]string{"description": "from the console"})
	var id int64
	for i := 0; i < 120 && id == 0; i++ {
		l, err := svc.BackupRuns.List(project, name).Do()
		if err == nil && len(l.Items) > 0 && l.Items[0].Status == "SUCCESSFUL" && l.Items[0].Description == "from the console" {
			id = l.Items[0].Id
		}
		time.Sleep(time.Second)
	}
	if id == 0 {
		t.Fatal("the console's backup never finished")
	}
	if code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/cloudsql-instances?project="+project+"&name="+name, ""); code != http.StatusOK ||
		!strings.Contains(out, fmt.Sprint(id)) || !strings.Contains(out, "restore-backup") {
		t.Errorf("the Backups tab = %d: %s", code, out)
	}
	sqlBackupRows(t, h, c, conn, "UPDATE cbk_rows SET name = 'z'")
	act("restore-backup", map[string]string{"backup": fmt.Sprint(id)})
	var got []string
	for i := 0; i < 120; i++ {
		time.Sleep(time.Second)
		if in, err := svc.Instances.Get(project, name).Do(); err != nil || in.State != "RUNNABLE" {
			continue
		}
		if got = sqlBackupRows(t, h, c, conn); len(got) == 1 && got[0] == "1=a" {
			break
		}
	}
	if len(got) != 1 || got[0] != "1=a" {
		t.Errorf("rows after the console's restore: %v", got)
	}
	act("clone", map[string]string{"destination": name + "-cl"})
	time.Sleep(2 * time.Second)
	running(name + "-cl")
	if got := sqlBackupRows(t, h, c, project+":us-central1:"+name+"-cl"); len(got) != 1 || got[0] != "1=a" {
		t.Errorf("rows in the console's clone: %v", got)
	}
	act("delete-backup", map[string]string{"backup": fmt.Sprint(id)})
	_, err = svc.BackupRuns.Get(project, name, id).Do()
	wantHTTP(t, "backupRuns.get after the console's delete", err, http.StatusNotFound)
}

// TestConsoleCloudSQLReadReplica (#1330): Create read replica on a
// primary's page makes a replica the official client reads as one; the
// replica's page shows its primary and offers Promote, Stop replication
// and Create read replica, a cascading replica (#1381); Stop replication and then Start replication are read back as
// databaseReplicationEnabled, and Promote leaves a standalone primary.
func TestConsoleCloudSQLReadReplica(t *testing.T) {
	consoleCloudSQLReadReplica(t, "POSTGRES_17", "console-rp-")
}

// TestConsoleCloudSQLReadReplicaPostgres96 (#1524): the same on a
// PostgreSQL 9.6 instance, whose replica is promoted with pg_ctl promote.
// covers: sql.instances.insert, sql.instances.promoteReplica, sql.instances.stopReplica, sql.instances.startReplica
func TestConsoleCloudSQLReadReplicaPostgres96(t *testing.T) {
	consoleCloudSQLReadReplica(t, "POSTGRES_9_6", "console-rp96-")
}

func consoleCloudSQLReadReplica(t *testing.T, version, prefix string) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	addr := consoleAddr(t, h)
	svc := sqlAdminService(t, h)
	project := h.Project()
	name := fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()%1e6)
	replica := name + "-r"
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: version,
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, n := range []string{replica, name} {
			if d, err := svc.Instances.Delete(project, n).Do(); err == nil {
				waitSQLOp(t, svc, project, d)
			}
		}
	})
	waitSQLOp(t, svc, project, op)
	act := func(on, action string, values map[string]string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": []string{on}, "Action": action, "Values": values})
		if code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/cloudsql-instances?project="+project, string(body)); code != http.StatusOK {
			t.Fatalf("console %s = %d: %s", action, code, out)
		}
	}
	get := func(want func(*sqladmin.DatabaseInstance) bool) *sqladmin.DatabaseInstance {
		t.Helper()
		var in *sqladmin.DatabaseInstance
		for i := 0; i < 180; i++ {
			if in, err = svc.Instances.Get(project, replica).Do(); err == nil && in.State == "RUNNABLE" && want(in) {
				return in
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("replica: %+v %v", in, err)
		return nil
	}
	// The page in Google's layout (#1860): Promote replica beside the
	// title, Create read replica and Stop and Start replication on the
	// Replicas page, the primary and the replicas on the Overview's
	// Replication card and in the Replicas list.
	if d := sqlInstancePageOf(t, addr, project, name); !d.offers("replicas", "create-replica") || d.offers("", "promote-replica") {
		t.Errorf("a primary's page offers %+v, its Replicas page %+v", d.Actions, d.section("replicas").Actions)
	}
	act(name, "create-replica", map[string]string{"replica": replica})
	get(func(in *sqladmin.DatabaseInstance) bool {
		return in.InstanceType == "READ_REPLICA_INSTANCE" && in.MasterInstanceName == project+":"+name
	})
	d := sqlInstancePageOf(t, addr, project, replica)
	if !d.offers("", "promote-replica") || !d.offers("replicas", "stop-replica") || !d.offers("replicas", "create-replica") ||
		d.property("Replication", "Primary instance") != project+":"+name || d.role(name) != "Primary" {
		t.Errorf("the replica's page: %+v", d)
	}
	if p := sqlInstancePageOf(t, addr, project, name); p.property("Replication", "Read replicas") != replica || p.role(replica) != "Read replica" {
		t.Errorf("the primary's page lists replicas %q, its Replicas page %+v", p.property("Replication", "Read replicas"), p.section("replicas").Listing.Items)
	}
	act(replica, "stop-replica", nil)
	get(func(in *sqladmin.DatabaseInstance) bool { return !in.Settings.DatabaseReplicationEnabled })
	act(replica, "start-replica", nil)
	get(func(in *sqladmin.DatabaseInstance) bool { return in.Settings.DatabaseReplicationEnabled })
	act(replica, "promote-replica", nil)
	get(func(in *sqladmin.DatabaseInstance) bool {
		return in.InstanceType == "CLOUD_SQL_INSTANCE" && in.MasterInstanceName == ""
	})
}

// TestConsoleCloudSQLPointInTimeRestore (#1330): the Create instance form's
// Point-in-time recovery makes an instance with pointInTimeRecoveryEnabled,
// as the official client (sqladmin/v1) reads it; its page shows the
// recovery window, and its Point-in-time restore action makes a new
// instance holding only the rows written by the time given.
func TestConsoleCloudSQLPointInTimeRestore(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	addr := consoleAddr(t, h)
	svc := sqlAdminService(t, h)
	project := h.Project()
	name := fmt.Sprintf("console-pitr-%d", time.Now().UnixNano()%1e6)
	code, out := consoleCreate(t, addr, "cloudsql-instances", project, map[string]string{
		"name": name, "databaseVersion": "POSTGRES_17", "tier": "db-custom-1-3840", "pitr": "true"})
	if code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	t.Cleanup(func() {
		for _, n := range []string{name, name + "-pit"} {
			if d, err := svc.Instances.Delete(project, n).Do(); err == nil {
				waitSQLOp(t, svc, project, d)
			}
		}
	})
	running := func(n string) {
		t.Helper()
		for i := 0; i < 180; i++ {
			if in, err := svc.Instances.Get(project, n).Do(); err == nil && in.State == "RUNNABLE" {
				return
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("%s never became RUNNABLE", n)
	}
	running(name)
	in, err := svc.Instances.Get(project, name).Do()
	if err != nil || in.Settings.BackupConfiguration == nil || !in.Settings.BackupConfiguration.PointInTimeRecoveryEnabled {
		t.Fatalf("the console's instance: %v %+v", err, in)
	}
	waitRecoverable(t, svc, project, name)
	if code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/cloudsql-instances?project="+project+"&name="+name, ""); code != http.StatusOK ||
		!strings.Contains(out, "pitr-restore") || !strings.Contains(out, "Earliest recovery time") {
		t.Errorf("the instance's page = %d: %s", code, out)
	}
	c := sqlBackupCase{version: "POSTGRES_17", table: "cpitr_rows", register: pgRegister, dsn: func(_, conn string) string {
		return fmt.Sprintf("host=%s user=postgres dbname=postgres sslmode=disable", conn)
	}}
	conn := project + ":us-central1:" + name
	sqlBackupRows(t, h, c, conn, "CREATE TABLE cpitr_rows (id INT PRIMARY KEY, name TEXT)", "INSERT INTO cpitr_rows VALUES (1, 'a')")
	at := sqlServerTime(t, h, c, conn)
	sqlBackupRows(t, h, c, conn, "INSERT INTO cpitr_rows VALUES (2, 'b')")
	body, _ := json.Marshal(map[string]any{"Path": []string{name}, "Action": "pitr-restore",
		"Values": map[string]string{"destination": name + "-pit", "pointInTime": at.Format(time.RFC3339Nano)}})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/cloudsql-instances?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("console pitr-restore = %d: %s", code, out)
	}
	time.Sleep(2 * time.Second)
	running(name + "-pit")
	if rows := sqlBackupRows(t, h, c, project+":us-central1:"+name+"-pit"); len(rows) != 1 || rows[0] != "1=a" {
		t.Errorf("rows in the console's point-in-time restore: %v, want [1=a]", rows)
	}
}

// TestConsoleCloudSQLVersionsAndUpgrade (#1381): the Create instance
// form's version picker lists every served version of the engines this
// instance runs, PostgreSQL 9.6 to 12 among them (#1462), and a
// POSTGRES_12 instance is created from it; an instance created from it as POSTGRES_15 in
// europe-west1 is read by the official client (sqladmin/v1) with that
// version and region, its page offers Upgrade with the newer versions, and
// Upgrade to POSTGRES_16 is read back as the new version, the server
// reporting POSTGRES_16_15. The page of the newest version offers none.
func TestConsoleCloudSQLVersionsAndUpgrade(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	addr := consoleAddr(t, h)
	svc := sqlAdminService(t, h)
	project := h.Project()
	code, body := consoleDo(t, addr, http.MethodGet, "/api/services", "")
	if code != http.StatusOK {
		t.Fatalf("services = %d", code)
	}
	var listing struct {
		Services []struct {
			ID     string
			Create json.RawMessage
		}
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatal(err)
	}
	var picker []string
	for _, s := range listing.Services {
		var form struct {
			Fields []struct {
				Name    string
				Options []string
			}
		}
		// Google's create page picks the engine first, then its version
		// (#1860): PostgreSQL's versions in databaseVersion, MySQL's in
		// mysqlVersion.
		if s.ID == "cloudsql-instances" && json.Unmarshal(s.Create, &form) == nil {
			for _, name := range []string{"databaseVersion", "mysqlVersion"} {
				for _, f := range form.Fields {
					if f.Name == name {
						picker = append(picker, f.Options...)
					}
				}
			}
		}
	}
	want := []string{"POSTGRES_9_6", "POSTGRES_10", "POSTGRES_11", "POSTGRES_12", "POSTGRES_13", "POSTGRES_14", "POSTGRES_15", "POSTGRES_16", "POSTGRES_17", "POSTGRES_18"}
	if os.Getenv(EnvMySQL) != "" {
		want = append(want, "MYSQL_5_6", "MYSQL_5_7", "MYSQL_8_0", "MYSQL_8_4")
	}
	if strings.Join(picker, ",") != strings.Join(want, ",") {
		t.Errorf("the version picker lists %v, want %v", picker, want)
	}
	// PostgreSQL 12, served since #1462, is created from the picker too.
	old := fmt.Sprintf("console-pg12-%d", time.Now().UnixNano()%1e6)
	if code, out := consoleCreate(t, addr, "cloudsql-instances", project, map[string]string{
		"name": old, "databaseVersion": "POSTGRES_12", "tier": "db-custom-1-3840"}); code != http.StatusOK {
		t.Fatalf("console create of POSTGRES_12 = %d: %s", code, out)
	}
	t.Cleanup(func() {
		if op, err := svc.Instances.Delete(project, old).Do(); err == nil {
			waitSQLOp(t, svc, project, op)
		}
	})
	for i := 0; ; i++ {
		in, err := svc.Instances.Get(project, old).Do()
		if err == nil && in.State == "RUNNABLE" && in.DatabaseInstalledVersion == "POSTGRES_12_22" {
			break
		}
		if i == 240 {
			t.Fatalf("the POSTGRES_12 instance: %+v %v", in, err)
		}
		time.Sleep(time.Second)
	}
	name := fmt.Sprintf("console-v-%d", time.Now().UnixNano()%1e6)
	code, out := consoleCreate(t, addr, "cloudsql-instances", project, map[string]string{
		"name": name, "databaseVersion": "POSTGRES_15", "tier": "db-custom-1-3840", "region": "europe-west1"})
	if code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	t.Cleanup(func() {
		if op, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQLOp(t, svc, project, op)
		}
	})
	get := func(want func(*sqladmin.DatabaseInstance) bool) *sqladmin.DatabaseInstance {
		t.Helper()
		var in *sqladmin.DatabaseInstance
		var err error
		for i := 0; i < 240; i++ {
			if in, err = svc.Instances.Get(project, name).Do(); err == nil && in.State == "RUNNABLE" && want(in) {
				return in
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("instance: %+v %v", in, err)
		return nil
	}
	in := get(func(in *sqladmin.DatabaseInstance) bool { return true })
	if in.DatabaseVersion != "POSTGRES_15" || in.Region != "europe-west1" || in.ConnectionName != project+":europe-west1:"+name {
		t.Errorf("created: version %s region %s connection %s", in.DatabaseVersion, in.Region, in.ConnectionName)
	}
	if d := consoleDetailOf(t, addr, "cloudsql-instances", project, name); !d.offers("upgrade") {
		t.Errorf("the page of a POSTGRES_15 instance offers %+v", d.Actions)
	}
	body2, _ := json.Marshal(map[string]any{"Path": []string{name}, "Action": "upgrade", "Values": map[string]string{"databaseVersion": "POSTGRES_16"}})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/cloudsql-instances?project="+project, string(body2)); code != http.StatusOK {
		t.Fatalf("console upgrade = %d: %s", code, out)
	}
	get(func(in *sqladmin.DatabaseInstance) bool {
		return in.DatabaseVersion == "POSTGRES_16" && in.DatabaseInstalledVersion == "POSTGRES_16_15"
	})
	if d := consoleDetailOf(t, addr, "cloudsql-instances", project, "postgres"); d.offers("upgrade") {
		t.Errorf("the page of the server up runs offers Upgrade")
	}
}

// TestConsoleCloudSQLHighAvailabilityPostgres11 (#1524): Create instance
// with Zonal availability REGIONAL makes a PostgreSQL 11 instance with a
// standby in a second zone, as the official client reads it; its page
// offers Failover, which swaps the zones and keeps the instance RUNNABLE.
// covers: sql.instances.insert, sql.instances.failover
func TestConsoleCloudSQLHighAvailabilityPostgres11(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	addr := consoleAddr(t, h)
	svc := sqlAdminService(t, h)
	project := h.Project()
	name := fmt.Sprintf("console-ha11-%d", time.Now().UnixNano()%1e6)
	code, out := consoleCreate(t, addr, "cloudsql-instances", project, map[string]string{
		"name": name, "databaseVersion": "POSTGRES_11", "tier": "db-custom-1-3840", "availabilityType": "REGIONAL"})
	if code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQLOp(t, svc, project, d)
		}
	})
	get := func(want func(*sqladmin.DatabaseInstance) bool) *sqladmin.DatabaseInstance {
		t.Helper()
		var in *sqladmin.DatabaseInstance
		var err error
		for i := 0; i < 240; i++ {
			if in, err = svc.Instances.Get(project, name).Do(); err == nil && in.State == "RUNNABLE" && want(in) {
				return in
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("instance: %+v %v", in, err)
		return nil
	}
	in := get(func(in *sqladmin.DatabaseInstance) bool {
		return in.Settings.AvailabilityType == "REGIONAL" && in.SecondaryGceZone != "" && in.SecondaryGceZone != in.GceZone
	})
	if d := consoleDetailOf(t, addr, "cloudsql-instances", project, name); !d.offers("failover") {
		t.Fatalf("a REGIONAL instance's page offers %+v", d.Actions)
	}
	body, _ := json.Marshal(map[string]any{"Path": []string{name}, "Action": "failover", "Values": map[string]string{}})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/cloudsql-instances?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("console failover = %d: %s", code, out)
	}
	get(func(got *sqladmin.DatabaseInstance) bool {
		return got.GceZone == in.SecondaryGceZone && got.SecondaryGceZone == in.GceZone
	})
}

// sqlInstancePage is a Cloud SQL instance's console page in Google's layout
// (#1860), as much of it as these tests read: the actions beside the title,
// and each menu page's actions, property cards and list.
type sqlInstancePage struct {
	Actions  []struct{ ID string }
	Sections []sqlPageSection
}

// sqlPageSection is one menu page of it.
type sqlPageSection struct {
	ID      string
	Actions []struct{ ID string }
	Groups  []struct {
		Heading    string
		Properties []struct{ Label, Value string }
	}
	Listing struct {
		Items []struct {
			Name   string
			Fields map[string]string
		}
	}
}

// sqlInstancePageOf reads an instance's page through the console's detail
// route.
func sqlInstancePageOf(t *testing.T, addr, project, name string) sqlInstancePage {
	t.Helper()
	var d sqlInstancePage
	consoleJSON(t, addr, http.MethodGet, "/api/detail/cloudsql-instances?"+url.Values{"project": {project}, "name": {name}}.Encode(), "", &d)
	return d
}

// section is the menu page id, or a zero one.
func (d sqlInstancePage) section(id string) sqlPageSection {
	for _, s := range d.Sections {
		if s.ID == id {
			return s
		}
	}
	return sqlPageSection{}
}

// offers reports whether the menu page on ("" for beside the title) offers
// the action id.
func (d sqlInstancePage) offers(on, id string) bool {
	acts := d.Actions
	if on != "" {
		acts = d.section(on).Actions
	}
	for _, a := range acts {
		if a.ID == id {
			return true
		}
	}
	return false
}

// property is the value of label on the Overview's card heading.
func (d sqlInstancePage) property(heading, label string) string {
	for _, g := range d.section("overview").Groups {
		if g.Heading != heading {
			continue
		}
		for _, p := range g.Properties {
			if p.Label == label {
				return p.Value
			}
		}
	}
	return ""
}

// role is the Role the Replicas list gives the instance name.
func (d sqlInstancePage) role(name string) string {
	for _, it := range d.section("replicas").Listing.Items {
		if it.Name == name {
			return it.Fields["Role"]
		}
	}
	return ""
}
