package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

func mysqlConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServiceCloudSQLMySQL}
	return cfg
}

// The passwords are generated once and kept: MySQL writes them into its
// data directory, so a new pair on the next start would lock a persistent
// instance's application out of its own data.
func TestMySQLCredentialsAreGeneratedOnceAndPrivate(t *testing.T) {
	cfg := mysqlConfig(t)
	first, err := loadOrCreateMySQLCredentials(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Password) != 24 || len(first.RootPassword) != 24 || first.Password == first.RootPassword {
		t.Fatalf("credentials = %+v", first)
	}
	again, _ := loadOrCreateMySQLCredentials(cfg)
	if again != first {
		t.Error("the credentials changed between calls")
	}
	fi, err := os.Stat(mysqlCredentialsPath(cfg))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("credentials file mode = %v (%v), want 0600", fi.Mode(), err)
	}
}

func TestTheMySQLManifestCarriesTheInstanceCredentials(t *testing.T) {
	creds := components.MySQLCredentials{Password: "app-secret", RootPassword: "root-secret"}
	m := components.CloudSQLMySQLBackend(true, creds).Manifest("cb-ns", "inst")
	for _, want := range []string{"app-secret", "root-secret", "MYSQL_DATABASE", "containerPort: 3306",
		"PersistentVolumeClaim", "mountPath: /var/lib/mysql", "--datadir=/var/lib/mysql/data", "sha256:"} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	if strings.Contains(components.CloudSQLMySQLBackend(false, creds).Manifest("cb-ns", "inst"), "PersistentVolumeClaim") {
		t.Error("an ephemeral MySQL was given a volume")
	}
}

func TestEnvAndStatusShowTheMySQLEndpoint(t *testing.T) {
	cfg := mysqlConfig(t)
	creds, _ := loadOrCreateMySQLCredentials(cfg)
	vars := map[string]string{}
	for _, v := range envVars(cfg, "dev-project", "") {
		vars[v.Name] = v.Value
	}
	for name, want := range map[string]string{
		"MYSQL_HOST": "127.0.0.1", "MYSQL_PORT": "9017", "MYSQL_USER": "cloudburrow",
		"MYSQL_PASSWORD": creds.Password, "MYSQL_DATABASE": "cloudburrow",
	} {
		if vars[name] != want {
			t.Errorf("%s = %q, want %q", name, vars[name], want)
		}
	}

	var out bytes.Buffer
	printConfiguredEndpoints(&out, cfg)
	s := out.String()
	for _, want := range []string{"cloudsql-mysql", "127.0.0.1:9017", "not the Cloud SQL Admin API",
		"user cloudburrow, database cloudburrow", mysqlCredentialsPath(cfg)} {
		if !strings.Contains(s, want) {
			t.Errorf("status endpoints lack %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, creds.Password) {
		t.Error("status printed the MySQL password")
	}
}

func TestMySQLIdentifierQuoting(t *testing.T) {
	if got := quoteMySQLIdent("we`ird"); got != "`we``ird`" {
		t.Errorf("quoteMySQLIdent = %s", got)
	}
}
