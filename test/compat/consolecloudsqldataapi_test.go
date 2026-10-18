//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestConsoleCloudSQLDataAPIAndProjectBackups (#1462): the Cloud SQL
// page's actions, each checked through the official client. On a created
// PostgreSQL instance with the Data API allowed, Run SQL answers the rows
// executeSql returns and Pre-check upgrade its findings; Delete with
// final backup deletes it and leaves a FINAL backup, which Edit project
// backup changes and Delete project backup deletes from the `postgres`
// server's page; Restore project backup restores a backup by its name;
// Truncate log is offered on MySQL.
// covers: sql.instances.executeSql, sql.instances.preCheckMajorVersionUpgrade, sql.instances.truncateLog, sql.instances.delete, sql.instances.restoreBackup, sql.Backups.CreateBackup, sql.Backups.ListBackups, sql.Backups.GetBackup, sql.Backups.UpdateBackup, sql.Backups.DeleteBackup
func TestConsoleCloudSQLDataAPIAndProjectBackups(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envCloudSQLSecrets, EnvSecrets)
	if os.Getenv(EnvCloudSQL) == "" || os.Getenv(EnvSecrets) == "" {
		t.Skip("needs --services cloudsql,secretmanager")
	}
	sqlShardSecrets(t, h) // the password's secret version
	addr := consoleAddr(t, h)
	svc := sqlAdminService(t, h)
	project := h.Project()
	name := fmt.Sprintf("cdata-pg-%d", time.Now().UnixNano()%1e6)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17",
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", DataApiAccess: "ALLOW_DATA_API"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	gone := false
	defer func() {
		if !gone {
			if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
				waitSQLOp(t, svc, project, d)
			}
		}
	}()
	waitSQLOp(t, svc, project, op)
	op, err = svc.Users.Insert(project, name, &sqladmin.User{Name: "consoleapi", Password: "Console-pass1"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	sm := secretsClient(t, h)
	parent := "projects/" + project
	secretID := name + "-pw"
	if _, err := sm.CreateSecret(h.Context(), &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: secretID,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: parent + "/secrets/" + secretID})
	}()
	ver, err := sm.AddSecretVersion(h.Context(), &secretmanagerpb.AddSecretVersionRequest{Parent: parent + "/secrets/" + secretID,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("Console-pass1")}})
	if err != nil {
		t.Fatal(err)
	}

	detail := func(inst string) string {
		t.Helper()
		code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/cloudsql-instances?project="+project+"&name="+inst, "")
		if code != http.StatusOK {
			t.Fatalf("detail %s = %d: %.300s", inst, code, out)
		}
		return out
	}
	act := func(inst, action string, values map[string]string) string {
		t.Helper()
		code, out := consoleAct(t, addr, "cloudsql-instances", project, []string{inst}, action, values)
		if code != http.StatusOK {
			t.Fatalf("console %s on %s = %d: %s", action, inst, code, out)
		}
		return out
	}
	page := detail(name)
	for _, id := range []string{"run-sql", "precheck-upgrade", "delete-final-backup"} {
		if !strings.Contains(page, `"id":"`+id+`"`) {
			t.Fatalf("%s's page offers no %s: %.500s", name, id, page)
		}
	}
	out := act(name, "run-sql", map[string]string{"database": "postgres", "user": "consoleapi", "passwordSecretVersion": ver.Name,
		"sql": "SELECT 'console-' || n AS v FROM generate_series(1, 2) AS n", "rowLimit": "10"})
	if !strings.Contains(out, "console-1") || !strings.Contains(out, "console-2") {
		t.Errorf("Run SQL: %.500s", out)
	}
	if out := act(name, "precheck-upgrade", map[string]string{"databaseVersion": "POSTGRES_18"}); !strings.Contains(out, "Nothing stops the upgrade") {
		t.Errorf("Pre-check upgrade: %.500s", out)
	}
	if os.Getenv(EnvMySQL) != "" {
		if !strings.Contains(detail("mysql"), `"id":"truncate-log"`) {
			t.Error("the MySQL server's page offers no Truncate log")
		}
		act("mysql", "truncate-log", map[string]string{"logType": "MYSQL_SLOW_TABLE"})
	}

	act(name, "delete-final-backup", map[string]string{"description": "console-final-" + name, "ttlDays": "5"})
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if _, err := svc.Instances.Get(project, name).Do(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Delete with final backup did not delete the instance")
		}
		time.Sleep(time.Second)
	}
	gone = true
	list, err := svc.Backups.ListBackups(parent).Filter("type=FINAL").Do()
	if err != nil {
		t.Fatal(err)
	}
	var final string
	for _, b := range list.Backups {
		if b.Instance == name && b.Description == "console-final-"+name {
			final = b.Name
		}
	}
	if final == "" {
		t.Fatalf("no FINAL backup of %s: %+v", name, list.Backups)
	}
	if !strings.Contains(detail("postgres"), final) {
		t.Fatalf("the postgres page does not offer %s", final)
	}
	act("postgres", "edit-project-backup", map[string]string{"backup": final, "description": "edited", "ttlDays": "9"})
	if b, err := svc.Backups.GetBackup(final).Do(); err != nil || b.Description != "edited" || b.TtlDays != 9 {
		t.Errorf("after Edit project backup: %v %+v", err, b)
	}
	act("postgres", "delete-project-backup", map[string]string{"backup": final})
	if _, err := svc.Backups.GetBackup(final).Do(); err == nil {
		t.Error("Delete project backup left the backup")
	}

	// Restore project backup, of the postgres server's own backup.
	op, err = svc.Backups.CreateBackup(parent, &sqladmin.Backup{Instance: "postgres", Description: "console-restore"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	list, err = svc.Backups.ListBackups(parent).Filter(`instance="postgres"`).Do()
	if err != nil {
		t.Fatal(err)
	}
	var own string
	for _, b := range list.Backups {
		if b.Description == "console-restore" {
			own = b.Name
		}
	}
	if own == "" {
		t.Fatal("CreateBackup made no backup")
	}
	defer func() { _, _ = svc.Backups.DeleteBackup(own).Do() }()
	act("postgres", "restore-project-backup", map[string]string{"backup": own})
	ops, err := svc.Operations.List(project).Instance("postgres").Do()
	if err != nil || len(ops.Items) == 0 || ops.Items[0].OperationType != "RESTORE_VOLUME" {
		t.Fatalf("Restore project backup's operation: %v %+v", err, ops)
	}
	waitSQLOp(t, svc, project, ops.Items[0])
}
