//go:build compat

package compat

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	sqladmin "google.golang.org/api/sqladmin/v1"

	"github.com/cloudburrow/cloudburrow/internal/components"
)

// TestCloudSQLExternalMySQLPhysical (#1570): through the official Go
// client, startExternalSync with migrationType PHYSICAL, as Google
// documents a MySQL migration from a Percona XtraBackup physical backup.
// The source is a CloudBurrow MySQL instance with GTIDs on. Verification
// with migrationType PHYSICAL refuses what a physical copy cannot carry (a
// MyISAM table), the privileges XtraBackup needs that the replication user
// lacks, and a dumpFilePath folder holding no backup. Then two replicas are
// given the source's data physically: one from a backup the sync takes
// itself, and one from a real XtraBackup backup of the source uploaded to
// Cloud Storage, which dumpFilePath names. Each has the source's rows,
// keeps its own root password (the copy carries the source's accounts) and
// its own server UUID, and replicates by GTID a row written on the source
// afterwards.
// covers: sql.instances.insert, sql.instances.get, sql.instances.delete, sql.projects.instances.verifyExternalSyncSettings, sql.projects.instances.startExternalSync, sql.operations.get, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLExternalMySQLPhysical(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	bh := sqlTransferBucket(t, h, "physical")
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set: the uploaded backup is taken in the instance's cluster", envKubeconfig)
	}
	project := h.Project()
	e := sqlEngine{mysql: true}
	n := time.Now().UnixNano() % 1e6
	srcName := fmt.Sprintf("phy-src-%d", n)
	src := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: srcName, DatabaseVersion: "MYSQL_8_4",
		RootPassword: "Src-root1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	db := e.open(t, h, src.ConnectionName, "root", "Src-root1", "mysql")
	for _, q := range []string{"SET PERSIST enforce_gtid_consistency = ON", "SET PERSIST gtid_mode = OFF_PERMISSIVE",
		"SET PERSIST gtid_mode = ON_PERMISSIVE", "SET PERSIST gtid_mode = ON",
		"CREATE DATABASE shop", "CREATE TABLE shop.orders (id int primary key, item varchar(20))",
		"INSERT INTO shop.orders VALUES (1, 'before')",
		"CREATE TABLE shop.legacy (id int primary key) ENGINE=MyISAM",
		"CREATE USER 'migrator'@'%' IDENTIFIED BY 'Repl-pass1'",
		"GRANT REPLICATION SLAVE, REPLICATION CLIENT, SELECT ON *.* TO 'migrator'@'%'"} {
		q := q
		eventually(t, q, func() error { _, err := db.Exec(q); return err })
	}

	source := func(name, dump string) {
		t.Helper()
		op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "MYSQL_8_4",
			InstanceType: "ON_PREMISES_INSTANCE", Region: "us-central1",
			OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: "sqlinst-" + srcName + ":3306",
				Username: "migrator", Password: "Repl-pass1", DumpFilePath: dump}}).Do()
		if err != nil {
			t.Fatalf("insert the source representation instance %s: %v", name, err)
		}
		t.Cleanup(func() {
			if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
				waitSQLOpDone(t, svc, project, d)
			}
		})
		if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
			t.Fatalf("create the source representation instance %s: %+v", name, op.Error.Errors[0])
		}
	}
	replica := func(name, master string) *sqladmin.DatabaseInstance {
		t.Helper()
		return createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: name, MasterInstanceName: master,
			RootPassword: "Root-pass1", Region: "us-central1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	}
	physical := &sqladmin.SqlInstancesVerifyExternalSyncSettingsRequest{SyncMode: "ONLINE", MigrationType: "PHYSICAL"}
	verify := func(name string) *sqladmin.SqlInstancesVerifyExternalSyncSettingsResponse {
		t.Helper()
		v, err := svc.Projects.Instances.VerifyExternalSyncSettings(project, name, physical).Do()
		if err != nil {
			t.Fatalf("verifyExternalSyncSettings PHYSICAL of %s: %v", name, err)
		}
		return v
	}
	start := func(name string) {
		t.Helper()
		op, err := svc.Projects.Instances.StartExternalSync(project, name,
			&sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "ONLINE", MigrationType: "PHYSICAL"}).Do()
		if err != nil {
			t.Fatalf("startExternalSync PHYSICAL of %s: %v", name, err)
		}
		// The XtraBackup image's first pull, the backup and the restore.
		for deadline := time.Now().Add(20 * time.Minute); op.Status != "DONE"; {
			if time.Now().After(deadline) {
				t.Fatalf("startExternalSync PHYSICAL of %s did not finish", name)
			}
			time.Sleep(2 * time.Second)
			if op, err = svc.Operations.Get(project, op.Name).Do(); err != nil {
				t.Fatal(err)
			}
		}
		if op.Error != nil {
			t.Fatalf("startExternalSync PHYSICAL of %s: %+v", name, op.Error.Errors[0])
		}
	}
	var srcUUID string
	if err := db.QueryRow("SELECT @@GLOBAL.server_uuid").Scan(&srcUUID); err != nil {
		t.Fatal(err)
	}
	synced := func(name string, rep *sqladmin.DatabaseInstance, ids ...int) *sql.DB {
		t.Helper()
		// The instance's own root password, not the source's.
		rdb := e.root(t, h, rep.ConnectionName)
		for _, id := range ids {
			id := id
			eventually(t, fmt.Sprintf("order %d on %s", id, name), func() error {
				var item string
				if err := rdb.QueryRow("SELECT item FROM shop.orders WHERE id = ?", id).Scan(&item); err != nil {
					return err
				}
				return nil
			})
		}
		var uuid string
		if err := rdb.QueryRow("SELECT @@GLOBAL.server_uuid").Scan(&uuid); err != nil || uuid == srcUUID {
			t.Errorf("%s's server_uuid = %q, %v; want its own, not the source's %q", name, uuid, err, srcUUID)
		}
		return rdb
	}

	// 1. The sync takes the backup: the replication user needs
	// XtraBackup's privileges, and the MyISAM table cannot be copied.
	takeRep, takeName := fmt.Sprintf("phy-rep-%d", n), fmt.Sprintf("phy-take-%d", n)
	source(takeRep, "")
	take := replica(takeName, takeRep)
	v := verify(takeName)
	for _, want := range []string{"UNSUPPORTED_STORAGE_ENGINE", "INSUFFICIENT_PRIVILEGE"} {
		if !hasSyncError(v, want) {
			t.Errorf("verification PHYSICAL of a source with a MyISAM table and a user without BACKUP_ADMIN: no %s in %s", want, syncErrors(v))
		}
	}
	_, err := svc.Projects.Instances.StartExternalSync(project, takeName,
		&sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "ONLINE", MigrationType: "PHYSICAL"}).Do()
	wantHTTP(t, "startExternalSync PHYSICAL from a source that fails verification", err, 400)
	for _, q := range []string{"ALTER TABLE shop.legacy ENGINE=InnoDB",
		"GRANT RELOAD, PROCESS, BACKUP_ADMIN ON *.* TO 'migrator'@'%'",
		"GRANT SELECT ON performance_schema.log_status TO 'migrator'@'%'"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if v := verify(takeName); len(v.Errors) != 0 {
		t.Fatalf("verification PHYSICAL of a ready source: %s", syncErrors(v))
	}

	// 2. A dumpFilePath folder with only part of a backup (no
	// xtrabackup_info, no xtrabackup_binlog_info) is refused.
	emptyRep, emptyName := fmt.Sprintf("phy-none-%d", n), fmt.Sprintf("phy-nrep-%d", n)
	sqlWriteObject(t, h, bh, "nothing/xtrabackup_checkpoints", "backup_type = full-backuped\n")
	source(emptyRep, "gs://"+bh.BucketName()+"/nothing")
	replica(emptyName, emptyRep)
	if v := verify(emptyName); !hasSyncError(v, "INVALID_FILE_INFO") {
		t.Errorf("verification PHYSICAL of a folder with no backup: %s", syncErrors(v))
	}

	// 3. A real XtraBackup backup of the source, taken in the cluster with
	// the source's volume mounted, as one is taken on a server's host, and
	// uploaded to Cloud Storage.
	uploadXtraBackup(t, h, kubeconfig, bh, "sqlinst-"+srcName, fmt.Sprintf("xb-take-%d", n), "backup")
	gcsRep, gcsName := fmt.Sprintf("phy-gcs-%d", n), fmt.Sprintf("phy-grep-%d", n)
	source(gcsRep, "gs://"+bh.BucketName()+"/backup")
	gcs := replica(gcsName, gcsRep)

	start(takeName)
	start(gcsName)
	takeDB := synced(takeName, take, 1)
	gcsDB := synced(gcsName, gcs, 1)
	if _, err := db.Exec("INSERT INTO shop.orders VALUES (2, 'after')"); err != nil {
		t.Fatal(err)
	}
	synced(takeName, take, 2)
	synced(gcsName, gcs, 2)
	for name, d := range map[string]*sql.DB{takeName: takeDB, gcsName: gcsDB} {
		var engine string
		if err := d.QueryRow("SELECT engine FROM information_schema.tables WHERE table_schema = 'shop' AND table_name = 'legacy'").Scan(&engine); err != nil || engine != "InnoDB" {
			t.Errorf("%s: shop.legacy engine = %q, %v", name, engine, err)
		}
	}
	if v, err := svc.Projects.Instances.VerifyExternalSyncSettings(project, takeName, physical).Do(); err != nil || !hasSyncError(v, "REPLICA_ALREADY_SETUP") {
		t.Errorf("verification after the sync: %+v %v", v, err)
	}
}

// uploadXtraBackup runs XtraBackup in a pod with deployment's volume
// mounted, as `xtrabackup --backup --target-dir` runs on a server's host,
// and uploads every file it wrote to bh under folder.
func uploadXtraBackup(t *testing.T, h *Harness, kubeconfig string, bh *storage.BucketHandle, deployment, pod, folder string) {
	t.Helper()
	kubectl := func(stdin string, args ...string) ([]byte, error) {
		cmd := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", "cloudburrow"}, args...)...)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return out, fmt.Errorf("kubectl %s: %v %s", args[0], err, stderr.String())
		}
		return out, nil
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
spec:
  restartPolicy: Never
  securityContext:
    runAsUser: 0
  containers:
    - name: xtrabackup
      image: %s
      command: ["bash", "-c", "xtrabackup --backup --no-server-version-check --datadir=/var/lib/mysql/data --target-dir=/backup --host=%s --port=3306 --user=migrator --password=Repl-pass1 && touch /done || touch /failed; sleep 3600"]
      volumeMounts:
        - {name: source, mountPath: /var/lib/mysql, readOnly: true}
        - {name: backup, mountPath: /backup}
  volumes:
    - name: source
      persistentVolumeClaim: {claimName: %s-data, readOnly: true}
    - name: backup
      emptyDir: {}
`, pod, components.XtraBackup84Image, deployment, deployment)
	if _, err := kubectl(manifest, "apply", "-f", "-"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = kubectl("", "delete", "pod", pod, "--ignore-not-found", "--wait=false") })
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if _, err := kubectl("", "exec", pod, "--", "test", "-f", "/done"); err == nil {
			break
		}
		_, err := kubectl("", "exec", pod, "--", "test", "-f", "/failed")
		if err == nil || time.Now().After(deadline) {
			logs, _ := kubectl("", "logs", pod, "--tail=20")
			t.Fatalf("the XtraBackup pod did not take its backup: %s", logs)
		}
		time.Sleep(3 * time.Second)
	}
	list, err := kubectl("", "exec", pod, "--", "find", "/backup", "-type", "f")
	if err != nil {
		t.Fatal(err)
	}
	files := strings.Fields(string(list))
	if len(files) == 0 {
		t.Fatal("the backup has no files")
	}
	for _, f := range files {
		data, err := kubectl("", "exec", pod, "--", "cat", f)
		if err != nil {
			t.Fatal(err)
		}
		w := bh.Object(folder + strings.TrimPrefix(f, "/backup")).NewWriter(h.Context())
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func syncErrors(v *sqladmin.SqlInstancesVerifyExternalSyncSettingsResponse) string {
	var out []string
	for _, e := range v.Errors {
		out = append(out, e.Type+": "+e.Detail)
	}
	return strings.Join(out, "; ")
}
