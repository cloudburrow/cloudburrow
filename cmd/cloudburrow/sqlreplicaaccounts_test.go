package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// fakeMySQLClients writes mysql, mysqladmin and mysqldump stand-ins to dir:
// a call with -h primary is the primary's, which has the account
// 'appuser'@'%' at version; any other is the replica's. Statements read on
// standard input are appended to dir/applied.sql.
func fakeMySQLClients(t *testing.T, dir, version string) {
	t.Helper()
	mysql := `#!/bin/sh
args="$*"
case "$args" in *"-h primary"*) primary=1 ;; *) primary= ;; esac
case "$args" in
  *"SHOW REPLICA STATUS"*) exit 0 ;;
  *"SELECT 1"*) echo 1 ;;
  *"SELECT VERSION()"*) echo "` + version + `" ;;
  *"SELECT schema_name"*) echo app ;;
  *"SELECT CONCAT('CREATE USER '"*) [ -n "$primary" ] && echo "CREATE USER 'appuser'@'%' IDENTIFIED BY PASSWORD '*ABC';" ;;
  *"SELECT CONCAT('DROP USER"*) [ -z "$primary" ] && echo "DROP USER 'root'@'%';" ;;
  *"SELECT CONCAT(QUOTE(user)"*) [ -n "$primary" ] && echo "'appuser'@'%'" ;;
  *"SHOW CREATE USER"*) echo "CREATE USER 'appuser'@'%' IDENTIFIED WITH 'mysql_native_password' AS '*ABC'" ;;
  *"SHOW GRANTS"*) case "` + version + `" in
      5.6*) echo "GRANT SELECT ON *.* TO 'appuser'@'%' IDENTIFIED BY PASSWORD <secret>" ;;
      *) echo "GRANT SELECT ON *.* TO 'appuser'@'%'" ;;
    esac ;;
  *"SELECT COUNT(*)"*) echo 0 ;;
  *" -e "*|*" -e"*) : ;;
  *) [ -z "$primary" ] && cat >> "` + dir + `/applied.sql" ;;
esac
exit 0
`
	for name, body := range map[string]string{"mysql": mysql, "mysqladmin": "#!/bin/sh\nexit 0\n", "mysqldump": "#!/bin/sh\necho 'CREATE TABLE t (id INT);'\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// A replica of a Cloud SQL MySQL instance has the primary's users, the
// ones made before the replica too (main run 45126544176: a blue-green
// deployment's green instance had none of the blue one's, so after the
// switchover the blue name and its clone refused 'appuser'): the setup
// copies them, with the binary log off, after dropping the replica's own,
// on MySQL 5.7 and on 5.6, whose account statements end in an exit that
// must not end the setup, and whose SHOW GRANTS, read through the
// replication user, which lacks SUPER, masks the password as <secret>
// (main run 45147981577).
func TestMySQLReplicaSetupCopiesThePrimarysAccounts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the setup is a POSIX shell script")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	spec := sqladmin.Spec{Name: "rep", DatabaseVersion: "MYSQL_5_7", MasterInstanceName: "src"}
	script := replicaSetup(spec)
	if script != mysqlReplicaSetup {
		t.Fatal("a read replica's setup is not mysqlReplicaSetup")
	}
	for _, version := range []string{"5.7.44-log", "5.6.51-log"} {
		dir := t.TempDir()
		fakeMySQLClients(t, dir, version)
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CLOUDSQL_PRIMARY_HOST=primary",
			"CLOUDSQL_REPLICATION_PASSWORD=pw", "MYSQL_ROOT_PASSWORD=root")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: the setup failed: %v\n%s", version, err, out)
		}
		b, err := os.ReadFile(filepath.Join(dir, "applied.sql"))
		if err != nil {
			t.Fatal(err)
		}
		applied := string(b)
		create := "CREATE USER IF NOT EXISTS 'appuser'@'%'"
		if strings.HasPrefix(version, "5.6") {
			create = "CREATE USER 'appuser'@'%' IDENTIFIED BY PASSWORD"
		}
		for _, want := range []string{"SET SESSION sql_log_bin = 0;", "DROP USER 'root'@'%';", create, "GRANT SELECT ON *.* TO 'appuser'@'%';"} {
			if !strings.Contains(applied, want) {
				t.Errorf("%s: the replica was not given %q:\n%s", version, want, applied)
			}
		}
		if i, j := strings.Index(applied, "sql_log_bin = 0"), strings.Index(applied, "appuser"); i < 0 || j < i {
			t.Errorf("%s: an account statement came before the binary log was turned off:\n%s", version, applied)
		}
	}

	// An external source's replica gets its databases only, as before.
	dir := t.TempDir()
	fakeMySQLClients(t, dir, "5.7.44-log")
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CLOUDSQL_PRIMARY_HOST=primary",
		"CLOUDSQL_PRIMARY_USER=migrator", "CLOUDSQL_REPLICATION_PASSWORD=pw")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external: the setup failed: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "applied.sql")); strings.Contains(string(b), "appuser") {
		t.Errorf("an external source's replica was given its accounts:\n%s", b)
	}
}
