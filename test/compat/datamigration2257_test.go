//go:build compat

package compat

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// The #2257 tests: what Database Migration Service served at 501 after
// #2252, each through a real migration. Sources are containers; a source
// only a bastion reaches is on a Docker network of its own, which the
// kind network (and so the destinations' servers) has no route to.

// dmsTLS is a CA and the server and client certificates it signed.
type dmsTLS struct {
	ca, serverCert, serverKey, clientCert, clientKey string
}

func dmsNewTLS(t *testing.T, clientCN string) dmsTLS {
	t.Helper()
	key := func() (*ecdsa.PrivateKey, string) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, _ := x509.MarshalPKCS8PrivateKey(k)
		return k, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	}
	caKey, _ := key()
	caT := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dms-test-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caT, caT, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	sign := func(cn string, serial int64, usage x509.ExtKeyUsage) (string, string) {
		k, kp := key()
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour),
			NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), kp
	}
	out := dmsTLS{ca: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))}
	out.serverCert, out.serverKey = sign("dms-source", 2, x509.ExtKeyUsageServerAuth)
	out.clientCert, out.clientKey = sign(clientCN, 3, x509.ExtKeyUsageClientAuth)
	return out
}

// dmsNetwork is a Docker network of the test's own, which the kind
// network has no route to.
func dmsNetwork(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("cb-dms-iso-%d", time.Now().UnixNano()%1e9)
	if out, err := exec.Command("docker", "network", "create", name).CombinedOutput(); err != nil {
		t.Fatalf("docker network create: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", name).Run() })
	return name
}

func dmsContainerIP(t *testing.T, name, network string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{(index .NetworkSettings.Networks \""+network+"\").IPAddress}}", name).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("the address of %s on %s: %v %s", name, network, err, out)
	}
	return strings.TrimSpace(string(out))
}

// dmsSourceOpts are a source container's settings beyond startDMSSource's.
type dmsSourceOpts struct {
	network string // "" is kind
	gtidOff bool
	tls     *dmsTLS
}

// startDMSSourceWith is startDMSSource on opts' network, with the source's
// certificates (its TLS on) and, for MySQL, gtid_mode OFF.
func startDMSSourceWith(t *testing.T, mysql bool, opts dmsSourceOpts) *dmsSource {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("needs docker")
	}
	network := opts.network
	if network == "" {
		network = "kind"
	}
	name := fmt.Sprintf("cb-dms-src-%d", time.Now().UnixNano()%1e9)
	args := []string{"create", "--name", name, "--network", network}
	var prep, server string
	if mysql {
		gtid := "--gtid-mode=ON --enforce-gtid-consistency=ON"
		if opts.gtidOff {
			gtid = "--gtid-mode=OFF"
		}
		server = "docker-entrypoint.sh mysqld --server-id=78 --log-bin=mysql-bin --binlog-format=ROW " + gtid
		if opts.tls != nil {
			prep = "mkdir -p /etc/dmstls && cp /dmstls/* /etc/dmstls/ && chown -R mysql:mysql /etc/dmstls && chmod 600 /etc/dmstls/server-key.pem && "
			server += " --ssl-ca=/etc/dmstls/ca.pem --ssl-cert=/etc/dmstls/server-cert.pem --ssl-key=/etc/dmstls/server-key.pem"
		}
		args = append(args, "-e", "MYSQL_ROOT_PASSWORD="+dmsSourcePass, "--entrypoint", "sh", dmsMySQLImage, "-c", prep+"exec "+server)
	} else {
		server = "docker-entrypoint.sh postgres -c wal_level=logical -c max_replication_slots=10 -c max_wal_senders=10 -c max_worker_processes=16"
		if opts.tls != nil {
			prep = "mkdir -p /etc/dmstls && cp /dmstls/* /etc/dmstls/ && chown -R postgres:postgres /etc/dmstls && chmod 600 /etc/dmstls/server-key.pem && "
			server += " -c ssl=on -c ssl_cert_file=/etc/dmstls/server-cert.pem -c ssl_key_file=/etc/dmstls/server-key.pem -c ssl_ca_file=/etc/dmstls/ca.pem"
		}
		args = append(args, "-e", "POSTGRES_PASSWORD="+dmsSourcePass, "--entrypoint", "sh", dmsPostgresImage, "-c", prep+"exec "+server)
	}
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker create the source: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	if opts.tls != nil {
		dir := t.TempDir()
		for f, v := range map[string]string{"ca.pem": opts.tls.ca, "server-cert.pem": opts.tls.serverCert, "server-key.pem": opts.tls.serverKey} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if out, err := exec.Command("docker", "cp", dir+"/.", name+":/dmstls").CombinedOutput(); err != nil {
			t.Fatalf("docker cp the certificates: %v: %s", err, out)
		}
	}
	if out, err := exec.Command("docker", "start", name).CombinedOutput(); err != nil {
		t.Fatalf("docker start the source: %v: %s", err, out)
	}
	s := &dmsSource{name: name, mysql: mysql, ip: dmsContainerIP(t, name, network)}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if _, err := s.exec("", "SELECT 1"); err == nil {
			break
		} else if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", "--tail", "30", name).CombinedOutput()
			t.Fatalf("the source did not start: %v\n%s", err, logs)
		}
		time.Sleep(time.Second)
	}
	return s
}

// dmsSSHBinary is testdata/dmsssh built for the Docker daemon's Linux.
func dmsSSHBinary(t *testing.T) string {
	t.Helper()
	arch := runtime.GOARCH
	if out, err := exec.Command("docker", "version", "-f", "{{.Server.Arch}}").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		arch = strings.TrimSpace(string(out))
	}
	bin := filepath.Join(t.TempDir(), "dmsssh")
	build := exec.Command("go", "build", "-o", bin, "./testdata/dmsssh")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build dmsssh: %v\n%s", err, out)
	}
	return bin
}

// dmsRunSSH runs dmsssh in a container on networks (the first its own),
// from the PostgreSQL source image already pulled, and answers its name.
func dmsRunSSH(t *testing.T, bin string, networks []string, args ...string) string {
	t.Helper()
	name := fmt.Sprintf("cb-dms-ssh-%d", time.Now().UnixNano()%1e9)
	create := append([]string{"create", "--name", name, "--network", networks[0], "--entrypoint", "/dmsssh", dmsPostgresImage}, args...)
	if out, err := exec.Command("docker", create...).CombinedOutput(); err != nil {
		t.Fatalf("docker create %s: %v: %s", name, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	if out, err := exec.Command("docker", "cp", bin, name+":/dmsssh").CombinedOutput(); err != nil {
		t.Fatalf("docker cp dmsssh: %v: %s", err, out)
	}
	for _, n := range networks[1:] {
		if out, err := exec.Command("docker", "network", "connect", n, name).CombinedOutput(); err != nil {
			t.Fatalf("docker network connect %s: %v: %s", n, err, out)
		}
	}
	if out, err := exec.Command("docker", "start", name).CombinedOutput(); err != nil {
		t.Fatalf("docker start %s: %v: %s", name, err, out)
	}
	return name
}

func dmsGcloud(t *testing.T, h *Harness) (func(a ...string) string, func(a ...string) (string, error), string) {
	g := newGcloudSession(t, h)
	try := func(a ...string) (string, error) {
		return g.run(nil, append(append([]string{"database-migration"}, a...), "--region", "us-central1", "--project", g.project, "--quiet")...)
	}
	run := func(a ...string) string {
		t.Helper()
		out, err := try(a...)
		if err != nil {
			t.Fatalf("gcloud database-migration %s: %v\n%s", strings.Join(a, " "), err, out)
		}
		return out
	}
	return run, try, g.project
}

// TestDataMigrationMySQLExistingInstanceNoGTID (#2257), through gcloud:
// a continuous migration into an existing Cloud SQL for MySQL instance
// (gcloud's `connection-profiles create mysql --cloudsql-instance
// --role=DESTINATION`) from a MySQL source whose gtid_mode is OFF, which
// replicates by binary log file and position; the job selects one of the
// source's two databases (--databases-filter) and dumps with extra
// mysqldump flags (--dump-flags). A start before the demotion fails;
// `migration-jobs demote-destination` makes the instance a replica of
// the source; the selected database's rows and a row written after reach
// it, the other database does not; promote leaves it standalone and
// writable, and deleting the job leaves the instance.
// covers: google.cloud.clouddms.v1.DataMigrationService/CreateConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/CreateMigrationJob, google.cloud.clouddms.v1.DataMigrationService/DemoteDestination, google.cloud.clouddms.v1.DataMigrationService/VerifyMigrationJob, google.cloud.clouddms.v1.DataMigrationService/StartMigrationJob, google.cloud.clouddms.v1.DataMigrationService/PromoteMigrationJob, google.cloud.clouddms.v1.DataMigrationService/DeleteMigrationJob
func TestDataMigrationMySQLExistingInstanceNoGTID(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	c := dmsClient(t, h)
	ctx := dmsCtx(t)
	run, try, project := dmsGcloud(t, h)
	svc := sqlAdminService(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	src := startDMSSourceWith(t, true, dmsSourceOpts{gtidOff: true})
	if g, err := src.exec("", "SELECT @@GLOBAL.gtid_mode"); err != nil || g != "OFF" {
		t.Fatalf("the source's gtid_mode: %q %v", g, err)
	}
	src.mustExec(t, "", "CREATE DATABASE shop", "CREATE DATABASE other")
	src.mustExec(t, "shop", "CREATE TABLE orders (id int AUTO_INCREMENT PRIMARY KEY, item varchar(64) NOT NULL) ENGINE=InnoDB",
		"INSERT INTO orders (item) VALUES ('a'), ('b'), ('c')")
	src.mustExec(t, "other", "CREATE TABLE t (id int PRIMARY KEY) ENGINE=InnoDB", "INSERT INTO t VALUES (1)")

	dest := fmt.Sprintf("dms-exist-%d", n)
	createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: dest, DatabaseVersion: "MYSQL_8_0", Region: "us-central1",
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	srcID, dstID, jobID := fmt.Sprintf("ex-src-%d", n), fmt.Sprintf("ex-dst-%d", n), fmt.Sprintf("ex-job-%d", n)
	job := parent + "/migrationJobs/" + jobID
	t.Cleanup(func() {
		ctx := context.Background()
		if op, err := c.DeleteMigrationJob(ctx, &dmspb.DeleteMigrationJobRequest{Name: job}); err == nil {
			_ = op.Wait(ctx)
		}
		for _, p := range []string{dstID, srcID} {
			if op, err := c.DeleteConnectionProfile(ctx, &dmspb.DeleteConnectionProfileRequest{Name: parent + "/connectionProfiles/" + p}); err == nil {
				_ = op.Wait(ctx)
			}
		}
	})
	run("connection-profiles", "create", "mysql", srcID, "--host", src.ip, "--port", "3306", "--username", "root", "--password", dmsSourcePass, "--no-async")
	run("connection-profiles", "create", "mysql", dstID, "--cloudsql-instance", dest, "--role", "DESTINATION", "--no-async")
	if out := run("connection-profiles", "describe", dstID, "--format", "value(state,mysql.cloudSqlId,role)"); !strings.Contains(out, "READY") ||
		!strings.Contains(out, dest) || !strings.Contains(out, "DESTINATION") {
		t.Errorf("the existing destination's profile: %s", out)
	}
	// A profile of an instance that does not exist is refused.
	if out, err := try("connection-profiles", "create", "mysql", dstID+"-x", "--cloudsql-instance", "no-such-instance", "--role", "DESTINATION", "--no-async"); err == nil {
		t.Errorf("a profile of a missing instance was created: %s", out)
	}
	run("migration-jobs", "create", jobID, "--source", srcID, "--destination", dstID, "--type", "CONTINUOUS",
		"--databases-filter", "shop", "--dump-flags", "max-allowed-packet=64M,net-buffer-length=16384", "--no-async")
	j := dmsJobState(t, ctx, c, job)
	if fl := j.GetDumpFlags().GetDumpFlags(); len(fl) != 2 {
		t.Errorf("the job's dump flags: %v", fl)
	}
	// Not demoted yet, the job does not start.
	if out, err := try("migration-jobs", "start", jobID); err == nil {
		time.Sleep(3 * time.Second)
		if j := dmsJobState(t, ctx, c, job); j.GetState() == dmspb.MigrationJob_RUNNING {
			t.Fatalf("a job of an undemoted destination started: %s", out)
		}
	}
	run("migration-jobs", "demote-destination", jobID)
	eventually(t, "the destination demoted", func() error {
		in, err := svc.Instances.Get(project, dest).Do()
		if err != nil {
			return err
		}
		if in.MasterInstanceName == "" || in.State != "RUNNABLE" {
			return fmt.Errorf("%s: master %q, %s", dest, in.MasterInstanceName, in.State)
		}
		return nil
	})
	run("migration-jobs", "verify", jobID)
	run("migration-jobs", "start", jobID)
	dmsWaitFor(t, ctx, c, job, "the CDC phase", inCDC)

	db := mysqlOpen(t, h, project+":us-central1:"+dest, "root", "Root-pass1", "mysql")
	count := func(q string) (int, error) {
		var k int
		err := db.QueryRowContext(ctx, q).Scan(&k)
		return k, err
	}
	eventually(t, "the selected database's rows", func() error {
		if k, err := count("SELECT count(*) FROM shop.orders"); err != nil || k != 3 {
			return fmt.Errorf("%d rows, %v", k, err)
		}
		return nil
	})
	if k, _ := count("SELECT count(*) FROM information_schema.schemata WHERE schema_name = 'other'"); k != 0 {
		t.Errorf("the database the job did not select was migrated")
	}
	src.mustExec(t, "shop", "INSERT INTO orders (item) VALUES ('after-start')")
	eventually(t, "the row replicated by binary log position", func() error {
		if k, err := count("SELECT count(*) FROM shop.orders"); err != nil || k != 4 {
			return fmt.Errorf("%d rows, %v", k, err)
		}
		return nil
	})
	run("migration-jobs", "promote", jobID)
	dmsWaitFor(t, ctx, c, job, "COMPLETED", func(j *dmspb.MigrationJob) bool { return j.GetState() == dmspb.MigrationJob_COMPLETED })
	in, err := svc.Instances.Get(project, dest).Do()
	if err != nil || in.MasterInstanceName != "" || in.InstanceType != "CLOUD_SQL_INSTANCE" {
		t.Fatalf("the promoted instance: %+v %v", in, err)
	}
	eventually(t, "a write on the promoted instance", func() error {
		_, err := db.ExecContext(ctx, "INSERT INTO shop.orders (item) VALUES ('on-destination')")
		return err
	})
	run("migration-jobs", "delete", jobID)
	eventually(t, "the job deleted", func() error {
		if _, err := c.GetMigrationJob(ctx, &dmspb.GetMigrationJobRequest{Name: job}); err == nil {
			return fmt.Errorf("%s still exists", job)
		}
		return nil
	})
	if _, err := svc.Instances.Get(project, dest).Do(); err != nil {
		t.Errorf("the existing instance after the job's delete: %v", err)
	}
}

// TestDataMigrationPostgresExistingAlloyDBObjects (#2257), through
// gcloud: a continuous migration of one of a PostgreSQL source's
// databases (--databases-filter) into an existing AlloyDB cluster (the
// profile's --alloydb-cluster); `migration-jobs fetch-source-objects`
// lists the source's databases as the job's objects, the unselected one
// NOT_SELECTED, and `objects list` and `objects lookup` read them;
// `demote-destination`, then the rows and a row written after reach the
// cluster, read through AlloyDB's ExecuteSql; the object is then RUNNING;
// promote ends replication.
// covers: google.cloud.clouddms.v1.DataMigrationService/CreateConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/CreateMigrationJob, google.cloud.clouddms.v1.DataMigrationService/FetchSourceObjects, google.cloud.clouddms.v1.DataMigrationService/ListMigrationJobObjects, google.cloud.clouddms.v1.DataMigrationService/LookupMigrationJobObject, google.cloud.clouddms.v1.DataMigrationService/DemoteDestination, google.cloud.clouddms.v1.DataMigrationService/StartMigrationJob, google.cloud.clouddms.v1.DataMigrationService/PromoteMigrationJob
func TestDataMigrationPostgresExistingAlloyDBObjects(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvAlloyDB) == "" {
		t.Skip("needs --services alloydb")
	}
	c := dmsClient(t, h)
	adb := alloyDBClients(t, h)["grpc"]
	ctx := dmsCtx(t)
	run, try, project := dmsGcloud(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	src := startDMSSource(t, false)
	dmsPostgresSeed(t, src)
	src.mustExec(t, "", "CREATE DATABASE skipped")

	cluster := fmt.Sprintf("dms-exadb-%d", n)
	cop, err := adb.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: parent, ClusterId: cluster, Cluster: &alloydbpb.Cluster{
		InitialUser:   &alloydbpb.UserPassword{User: "postgres", Password: "Adb-pass1"},
		NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: "projects/" + project + "/global/networks/default"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := adb.DeleteCluster(context.Background(), &alloydbpb.DeleteClusterRequest{Name: parent + "/clusters/" + cluster, Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	iop, err := adb.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: parent + "/clusters/" + cluster, InstanceId: "primary",
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iop.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	srcID, dstID, jobID := fmt.Sprintf("exa-src-%d", n), fmt.Sprintf("exa-dst-%d", n), fmt.Sprintf("exa-job-%d", n)
	job := parent + "/migrationJobs/" + jobID
	t.Cleanup(func() {
		ctx := context.Background()
		if op, err := c.DeleteMigrationJob(ctx, &dmspb.DeleteMigrationJobRequest{Name: job}); err == nil {
			_ = op.Wait(ctx)
		}
		for _, p := range []string{dstID, srcID} {
			if op, err := c.DeleteConnectionProfile(ctx, &dmspb.DeleteConnectionProfileRequest{Name: parent + "/connectionProfiles/" + p}); err == nil {
				_ = op.Wait(ctx)
			}
		}
	})
	run("connection-profiles", "create", "postgresql", srcID, "--host", src.ip, "--port", "5432", "--username", "postgres", "--password", dmsSourcePass, "--no-async")
	run("connection-profiles", "create", "postgresql", dstID, "--alloydb-cluster", cluster, "--role", "DESTINATION", "--no-async")
	run("migration-jobs", "create", jobID, "--source", srcID, "--destination", dstID, "--type", "CONTINUOUS", "--use-postgres-native",
		"--databases-filter", "shop", "--no-async")

	// Output is combined with gcloud's progress lines; the values are the
	// tab-separated ones.
	out := run("migration-jobs", "fetch-source-objects", jobID, "--format", "value(sourceObject.database,state)")
	states := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Split(strings.TrimSpace(l), "\t"); len(f) == 2 {
			states[f[0]] = f[1]
		}
	}
	if states["shop"] != "NOT_STARTED" || states["skipped"] != "NOT_SELECTED" || states["postgres"] != "NOT_SELECTED" {
		t.Errorf("fetch-source-objects: %v\n%s", states, out)
	}
	if out := run("objects", "list", "--migration-job", jobID, "--format", "value(sourceObject.database,state)"); !strings.Contains(out, "shop") || !strings.Contains(out, "skipped") {
		t.Errorf("objects list: %s", out)
	}
	if out := run("objects", "lookup", "--migration-job", jobID, "--database", "shop", "--format", "value(name,state)"); !strings.Contains(out, job+"/objects/") || !strings.Contains(out, "NOT_STARTED") {
		t.Errorf("objects lookup: %s", out)
	}
	run("migration-jobs", "demote-destination", jobID)
	// The demotion's operation runs beside the request; start once it is
	// done (a start before it fails as not demoted).
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, err := try("migration-jobs", "start", jobID, "--skip-validation")
		if err == nil {
			break
		}
		if time.Now().After(deadline) || !strings.Contains(out, "demote") {
			t.Fatalf("start: %v\n%s", err, out)
		}
		time.Sleep(2 * time.Second)
	}
	dmsWaitFor(t, ctx, c, job, "the CDC phase", inCDC)
	primary := parent + "/clusters/" + cluster + "/instances/primary"
	sqlq := func(db, q string) (string, error) {
		res, err := adb.ExecuteSql(ctx, &alloydbpb.ExecuteSqlRequest{Instance: primary, Database: db, User: "postgres",
			UserCredential: &alloydbpb.ExecuteSqlRequest_Password{Password: "Adb-pass1"}, SqlStatement: q})
		if err != nil {
			return "", err
		}
		if len(res.GetSqlResults()) != 1 || len(res.GetSqlResults()[0].GetRows()) != 1 {
			return "", fmt.Errorf("unexpected answer %v", res)
		}
		return res.GetSqlResults()[0].GetRows()[0].GetValues()[0].GetValue(), nil
	}
	if k, err := sqlq("shop", "SELECT count(*) FROM orders"); err != nil || k != "50" {
		t.Fatalf("the full dump on the existing cluster: %s rows, %v", k, err)
	}
	if k, err := sqlq("postgres", "SELECT count(*) FROM pg_database WHERE datname = 'skipped'"); err != nil || k != "0" {
		t.Errorf("the unselected database: %s %v", k, err)
	}
	src.mustExec(t, "shop", "INSERT INTO orders (item) VALUES ('after-start')")
	eventually(t, "the CDC row on the existing cluster", func() error {
		if k, err := sqlq("shop", "SELECT count(*) FROM orders"); err != nil || k != "51" {
			return fmt.Errorf("%s rows, %v", k, err)
		}
		return nil
	})
	if out := run("objects", "lookup", "--migration-job", jobID, "--database", "shop", "--format", "value(state,phase)"); !strings.Contains(out, "RUNNING") {
		t.Errorf("the running job's object: %s", out)
	}
	run("migration-jobs", "promote", jobID)
	dmsWaitFor(t, ctx, c, job, "COMPLETED", func(j *dmspb.MigrationJob) bool { return j.GetState() == dmspb.MigrationJob_COMPLETED })
	if cl, err := adb.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: parent + "/clusters/" + cluster}); err != nil || cl.GetState() != alloydbpb.Cluster_READY {
		t.Errorf("the existing cluster after promote: %v %v", cl.GetState(), err)
	}
}

// TestDataMigrationPostgresVerifiedSSLForwardSSH (#2257), through gcloud:
// a PostgreSQL source with TLS on a Docker network the destinations'
// servers have no route to, reached through a bastion (an SSH server on
// both networks) by the profile's forward SSH tunnel
// (--forward-ssh-hostname, --forward-ssh-username, --forward-ssh-password)
// and verified against its CA (--ssl-type SERVER_ONLY --ca-certificate):
// the job verifies and runs, its rows and a row written after reach a new
// Cloud SQL for PostgreSQL destination; a profile with another CA fails
// verification with CONNECTION_FAILURE, and one without the tunnel cannot
// reach the source.
// covers: google.cloud.clouddms.v1.DataMigrationService/CreateConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/CreateMigrationJob, google.cloud.clouddms.v1.DataMigrationService/VerifyMigrationJob, google.cloud.clouddms.v1.DataMigrationService/StartMigrationJob, google.cloud.clouddms.v1.DataMigrationService/UpdateMigrationJob, google.cloud.clouddms.v1.DataMigrationService/DeleteMigrationJob, google.cloud.clouddms.v1.DataMigrationService/DeleteConnectionProfile
func TestDataMigrationPostgresVerifiedSSLForwardSSH(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	c := dmsClient(t, h)
	ctx := dmsCtx(t)
	run, _, project := dmsGcloud(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	iso := dmsNetwork(t)
	certs := dmsNewTLS(t, "postgres")
	src := startDMSSourceWith(t, false, dmsSourceOpts{network: iso, tls: &certs})
	dmsPostgresSeed(t, src)
	bastion := dmsRunSSH(t, dmsSSHBinary(t), []string{"kind", iso}, "server", "-listen", ":22", "-user", "tunnel", "-password", "Ssh-pass1")
	bastionIP := dmsContainerIP(t, bastion, "kind")

	srcID, dstID, jobID := fmt.Sprintf("ssh-src-%d", n), fmt.Sprintf("ssh-dst-%d", n), fmt.Sprintf("ssh-job-%d", n)
	badID, badJob := fmt.Sprintf("ssh-bad-%d", n), fmt.Sprintf("ssh-badjob-%d", n)
	job := parent + "/migrationJobs/" + jobID
	t.Cleanup(func() {
		ctx := context.Background()
		for _, j := range []string{jobID, badJob} {
			if op, err := c.DeleteMigrationJob(ctx, &dmspb.DeleteMigrationJobRequest{Name: parent + "/migrationJobs/" + j, Force: true}); err == nil {
				_ = op.Wait(ctx)
			}
		}
		for _, p := range []string{badID, srcID} {
			if op, err := c.DeleteConnectionProfile(ctx, &dmspb.DeleteConnectionProfileRequest{Name: parent + "/connectionProfiles/" + p}); err == nil {
				_ = op.Wait(ctx)
			}
		}
	})
	profile := func(id, ca string) {
		run("connection-profiles", "create", "postgresql", id, "--host", src.ip, "--port", "5432", "--username", "postgres", "--password", dmsSourcePass,
			"--ssl-type", "SERVER_ONLY", "--ca-certificate", ca,
			"--forward-ssh-hostname", bastionIP, "--forward-ssh-username", "tunnel", "--forward-ssh-password", "Ssh-pass1", "--no-async")
	}
	profile(srcID, certs.ca)
	if out := run("connection-profiles", "describe", srcID, "--format", "json"); strings.Contains(out, "Ssh-pass1") || strings.Contains(out, "BEGIN CERTIFICATE") ||
		!strings.Contains(out, bastionIP) {
		t.Errorf("the profile answers its input-only secrets, or not its tunnel: %s", out)
	}
	run("connection-profiles", "create", "cloudsql", dstID, "--source-id", srcID, "--database-version", "POSTGRES_15",
		"--tier", "db-custom-1-3840", "--root-password", pgRootPassword, "--no-async")
	run("migration-jobs", "create", jobID, "--source", srcID, "--destination", dstID, "--type", "CONTINUOUS", "--use-postgres-native", "--no-async")
	vop, err := c.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: job})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vop.Wait(ctx); err != nil {
		t.Fatalf("verify through the tunnel with the source's CA: %v %v", err, dmsVerificationCodes(t, ctx, c, vop.Name()))
	}
	run("migration-jobs", "start", jobID)
	dmsWaitFor(t, ctx, c, job, "the CDC phase", inCDC)
	shop := sqlEngine{}.open(t, h, project+":us-central1:"+dstID, "postgres", pgRootPassword, "shop")
	count := func() (int, error) {
		var k int
		err := shop.QueryRowContext(ctx, "SELECT count(*) FROM orders").Scan(&k)
		return k, err
	}
	if k, err := count(); err != nil || k != 50 {
		t.Fatalf("the full dump through the tunnel: %d rows, %v", k, err)
	}
	src.mustExec(t, "shop", "INSERT INTO orders (item) VALUES ('after-start')")
	eventually(t, "the CDC row through the tunnel", func() error {
		if k, err := count(); err != nil || k != 51 {
			return fmt.Errorf("%d rows, %v", k, err)
		}
		return nil
	})
	if ssl, err := src.exec("shop", "SELECT count(*) FROM pg_stat_ssl s JOIN pg_stat_activity a USING (pid) WHERE s.ssl AND a.backend_type = 'walsender'"); err != nil || ssl == "0" {
		t.Errorf("the replication connection is not TLS: %s %v", ssl, err)
	}

	// Another CA: the source's certificate does not verify.
	other := dmsNewTLS(t, "postgres")
	profile(badID, other.ca)
	dmsCreateProfile(t, ctx, c, parent, badID+"-d", &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Cloudsql{
		Cloudsql: &dmspb.CloudSqlConnectionProfile{Settings: &dmspb.CloudSqlSettings{SourceId: parent + "/connectionProfiles/" + badID,
			DatabaseVersion: dmspb.CloudSqlSettings_POSTGRES_15, Tier: "db-custom-1-3840", RootPassword: pgRootPassword}}}})
	run("migration-jobs", "create", badJob, "--source", badID, "--destination", badID+"-d", "--type", "CONTINUOUS", "--use-postgres-native", "--no-async")
	bop, err := c.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: parent + "/migrationJobs/" + badJob})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bop.Wait(ctx); err == nil {
		t.Fatal("a source verified against another CA")
	}
	if codes := dmsVerificationCodes(t, ctx, c, bop.Name()); !codes[dmspb.MigrationJobVerificationError_CONNECTION_FAILURE] {
		t.Errorf("verify against another CA: %v", codes)
	}
}

// TestDataMigrationMySQLClientCertReverseSSH (#2257), through the
// official Go client: a MySQL source on a Docker network of its own whose
// migration user REQUIRE X509 (SERVER_CLIENT: the source's certificate
// verified against the CA, the client certificate presented), reached by
// the job's reverse SSH connectivity: the source's side opens a remote
// forward to a bastion (as `ssh -R` does), and the destination connects to
// the bastion's vmIp:vmPort. Verification passes, the rows and a row
// written after reach a new Cloud SQL for MySQL destination over TLS.
// covers: google.cloud.clouddms.v1.DataMigrationService/CreateConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/CreateMigrationJob, google.cloud.clouddms.v1.DataMigrationService/VerifyMigrationJob, google.cloud.clouddms.v1.DataMigrationService/StartMigrationJob, google.cloud.clouddms.v1.DataMigrationService/PromoteMigrationJob, google.cloud.clouddms.v1.DataMigrationService/DeleteMigrationJob
func TestDataMigrationMySQLClientCertReverseSSH(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	c := dmsClient(t, h)
	ctx := dmsCtx(t)
	project := h.Project()
	parent := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	iso := dmsNetwork(t)
	certs := dmsNewTLS(t, "migrator")
	src := startDMSSourceWith(t, true, dmsSourceOpts{network: iso, tls: &certs})
	src.mustExec(t, "", "CREATE DATABASE shop",
		"CREATE USER 'migrator'@'%' IDENTIFIED BY 'Mig-pass1' REQUIRE X509",
		"GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, RELOAD, EXECUTE, REPLICATION SLAVE, REPLICATION CLIENT, PROCESS ON *.* TO 'migrator'@'%'")
	src.mustExec(t, "shop", "CREATE TABLE orders (id int AUTO_INCREMENT PRIMARY KEY, item varchar(64) NOT NULL) ENGINE=InnoDB",
		"INSERT INTO orders (item) VALUES ('a'), ('b'), ('c'), ('d')")
	bin := dmsSSHBinary(t)
	bastion := dmsRunSSH(t, bin, []string{"kind", iso}, "server", "-listen", ":22", "-user", "tunnel", "-password", "Ssh-pass1")
	bastionKind, bastionIso := dmsContainerIP(t, bastion, "kind"), dmsContainerIP(t, bastion, iso)
	const vmPort = 23306
	dmsRunSSH(t, bin, []string{iso}, "reverse", "-ssh", bastionIso+":22", "-user", "tunnel", "-password", "Ssh-pass1",
		"-remote", fmt.Sprintf("0.0.0.0:%d", vmPort), "-target", src.ip+":3306")

	srcID, dstID, jobID := fmt.Sprintf("rv-src-%d", n), fmt.Sprintf("rv-dst-%d", n), fmt.Sprintf("rv-job-%d", n)
	job := parent + "/migrationJobs/" + jobID
	t.Cleanup(func() {
		ctx := context.Background()
		if op, err := c.DeleteMigrationJob(ctx, &dmspb.DeleteMigrationJobRequest{Name: job, Force: true}); err == nil {
			_ = op.Wait(ctx)
		}
		if op, err := c.DeleteConnectionProfile(ctx, &dmspb.DeleteConnectionProfileRequest{Name: parent + "/connectionProfiles/" + srcID}); err == nil {
			_ = op.Wait(ctx)
		}
	})
	// The profile names the source as its own network knows it; the job's
	// reverse SSH connectivity is how the destination reaches it.
	dmsCreateProfile(t, ctx, c, parent, srcID, &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Mysql{Mysql: &dmspb.MySqlConnectionProfile{
		Host: src.ip, Port: 3306, Username: "migrator", Password: "Mig-pass1",
		Ssl: &dmspb.SslConfig{Type: dmspb.SslConfig_SERVER_CLIENT, CaCertificate: certs.ca, ClientCertificate: certs.clientCert, ClientKey: certs.clientKey}}}})
	got, err := c.GetConnectionProfile(ctx, &dmspb.GetConnectionProfileRequest{Name: parent + "/connectionProfiles/" + srcID})
	if err != nil || got.GetMysql().GetSsl().GetClientKey() != "" || got.GetMysql().GetSsl().GetType() != dmspb.SslConfig_SERVER_CLIENT {
		t.Fatalf("the profile's SSL: %v %v", got.GetMysql().GetSsl(), err)
	}
	dmsCreateProfile(t, ctx, c, parent, dstID, &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Cloudsql{
		Cloudsql: &dmspb.CloudSqlConnectionProfile{Settings: &dmspb.CloudSqlSettings{SourceId: parent + "/connectionProfiles/" + srcID,
			DatabaseVersion: dmspb.CloudSqlSettings_MYSQL_8_0, Tier: "db-custom-1-3840", RootPassword: "Root-pass1"}}}})
	jop, err := c.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: jobID, MigrationJob: &dmspb.MigrationJob{
		Type: dmspb.MigrationJob_CONTINUOUS, Source: parent + "/connectionProfiles/" + srcID, Destination: parent + "/connectionProfiles/" + dstID,
		Connectivity: &dmspb.MigrationJob_ReverseSshConnectivity{ReverseSshConnectivity: &dmspb.ReverseSshConnectivity{
			VmIp: bastionKind, VmPort: vmPort, Vm: "bastion", Vpc: "projects/" + project + "/global/networks/default"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if j, err := jop.Wait(ctx); err != nil || j.GetReverseSshConnectivity().GetVmPort() != vmPort {
		t.Fatalf("create the job: %v %v", j, err)
	}
	vop, err := c.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: job})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vop.Wait(ctx); err != nil {
		t.Fatalf("verify through the reverse tunnel with a client certificate: %v %v", err, dmsVerificationCodes(t, ctx, c, vop.Name()))
	}
	sop, err := c.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: job})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sop.Wait(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	dmsWaitFor(t, ctx, c, job, "the CDC phase", inCDC)
	shop := mysqlOpen(t, h, project+":us-central1:"+dstID, "root", "Root-pass1", "shop")
	count := func() (int, error) {
		var k int
		err := shop.QueryRowContext(ctx, "SELECT count(*) FROM orders").Scan(&k)
		return k, err
	}
	eventually(t, "the dump", func() error {
		if k, err := count(); err != nil || k != 4 {
			return fmt.Errorf("%d rows, %v", k, err)
		}
		return nil
	})
	src.mustExec(t, "shop", "INSERT INTO orders (item) VALUES ('after-start')")
	eventually(t, "the replicated row", func() error {
		if k, err := count(); err != nil || k != 5 {
			return fmt.Errorf("%d rows, %v", k, err)
		}
		return nil
	})
	// The replication connection is the migrator's, over TLS.
	if s, err := src.exec("", "SELECT count(*) FROM performance_schema.threads t JOIN performance_schema.status_by_thread s USING (thread_id) "+
		"WHERE t.processlist_user = 'migrator' AND t.processlist_command LIKE 'Binlog Dump%' AND s.variable_name = 'Ssl_version' AND s.variable_value <> ''"); err != nil || s == "0" {
		t.Errorf("the replication connection over TLS: %s %v", s, err)
	}
	pop, err := c.PromoteMigrationJob(ctx, &dmspb.PromoteMigrationJobRequest{Name: job})
	if err != nil {
		t.Fatal(err)
	}
	if j, err := pop.Wait(ctx); err != nil || j.GetState() != dmspb.MigrationJob_COMPLETED {
		t.Fatalf("promote: %v %v", j, err)
	}
}

// TestDataMigrationMySQLDumpPath (#2257), through the official Go client:
// a continuous MySQL job whose dumpPath is a mysqldump file in the
// instance's Cloud Storage (written with --set-gtid-purged=ON, as Google's
// manual dump guide asks): the destination loads that file, not a dump of
// the source (a table only the file has arrives), and replicates from the
// file's GTIDs, so a row written after reaches it; a job with both
// dumpPath and dumpFlags is refused.
// covers: google.cloud.clouddms.v1.DataMigrationService/CreateConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/CreateMigrationJob, google.cloud.clouddms.v1.DataMigrationService/VerifyMigrationJob, google.cloud.clouddms.v1.DataMigrationService/StartMigrationJob, google.cloud.clouddms.v1.DataMigrationService/DeleteMigrationJob
func TestDataMigrationMySQLDumpPath(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	c := dmsClient(t, h)
	ctx := dmsCtx(t)
	project := h.Project()
	parent := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	bh := sqlTransferBucket(t, h, "dmsdump")
	src := startDMSSource(t, true)
	src.mustExec(t, "", "CREATE DATABASE shop")
	src.mustExec(t, "shop", "CREATE TABLE orders (id int AUTO_INCREMENT PRIMARY KEY, item varchar(64) NOT NULL) ENGINE=InnoDB",
		"INSERT INTO orders (item) VALUES ('a'), ('b')",
		"CREATE TABLE from_file (id int PRIMARY KEY) ENGINE=InnoDB", "INSERT INTO from_file VALUES (7)")
	dump, err := exec.Command("docker", "exec", "-e", "MYSQL_PWD="+dmsSourcePass, src.name, "mysqldump", "-h127.0.0.1", "-uroot",
		"--single-transaction", "--set-gtid-purged=ON", "--databases", "shop").Output()
	if err != nil {
		t.Fatalf("mysqldump: %v", err)
	}
	sqlWriteObject(t, h, bh, "shop.sql", string(dump))
	// The table the file has and the source no longer does: dropped
	// without a GTID, so nothing replicates its drop.
	src.mustExec(t, "shop", "SET SESSION sql_log_bin = 0; DROP TABLE from_file")

	srcID, dstID, jobID := fmt.Sprintf("dp-src-%d", n), fmt.Sprintf("dp-dst-%d", n), fmt.Sprintf("dp-job-%d", n)
	job := parent + "/migrationJobs/" + jobID
	t.Cleanup(func() {
		ctx := context.Background()
		if op, err := c.DeleteMigrationJob(ctx, &dmspb.DeleteMigrationJobRequest{Name: job, Force: true}); err == nil {
			_ = op.Wait(ctx)
		}
		if op, err := c.DeleteConnectionProfile(ctx, &dmspb.DeleteConnectionProfileRequest{Name: parent + "/connectionProfiles/" + srcID}); err == nil {
			_ = op.Wait(ctx)
		}
	})
	dmsCreateProfile(t, ctx, c, parent, srcID, dmsSourceProfile(src))
	dmsCreateProfile(t, ctx, c, parent, dstID, &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Cloudsql{
		Cloudsql: &dmspb.CloudSqlConnectionProfile{Settings: &dmspb.CloudSqlSettings{SourceId: parent + "/connectionProfiles/" + srcID,
			DatabaseVersion: dmspb.CloudSqlSettings_MYSQL_8_0, Tier: "db-custom-1-3840", RootPassword: "Root-pass1"}}}})
	path := "gs://" + bh.BucketName() + "/shop.sql"
	if _, err := c.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: jobID + "-x", MigrationJob: &dmspb.MigrationJob{
		Type: dmspb.MigrationJob_CONTINUOUS, Source: parent + "/connectionProfiles/" + srcID, Destination: parent + "/connectionProfiles/" + dstID,
		DumpPath: path, DumpFlags: &dmspb.MigrationJob_DumpFlags{DumpFlags: []*dmspb.MigrationJob_DumpFlag{{Name: "add-locks"}}}}}); err == nil {
		t.Fatal("a job with both dumpPath and dumpFlags was created")
	}
	jop, err := c.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: jobID, MigrationJob: &dmspb.MigrationJob{
		Type: dmspb.MigrationJob_CONTINUOUS, Source: parent + "/connectionProfiles/" + srcID, Destination: parent + "/connectionProfiles/" + dstID, DumpPath: path}})
	if err != nil {
		t.Fatal(err)
	}
	if j, err := jop.Wait(ctx); err != nil || j.GetDumpPath() != path {
		t.Fatalf("create the job: %v %v", j, err)
	}
	vop, err := c.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: job})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vop.Wait(ctx); err != nil {
		t.Fatalf("verify: %v %v", err, dmsVerificationCodes(t, ctx, c, vop.Name()))
	}
	sop, err := c.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: job})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sop.Wait(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	dmsWaitFor(t, ctx, c, job, "the CDC phase", inCDC)
	shop := mysqlOpen(t, h, project+":us-central1:"+dstID, "root", "Root-pass1", "shop")
	eventually(t, "the dump file's table", func() error {
		var k int
		if err := shop.QueryRowContext(ctx, "SELECT id FROM from_file").Scan(&k); err != nil || k != 7 {
			return fmt.Errorf("from_file: %d %v", k, err)
		}
		return nil
	})
	src.mustExec(t, "shop", "INSERT INTO orders (item) VALUES ('after-the-dump')")
	eventually(t, "the row written after the dump", func() error {
		var k int
		if err := shop.QueryRowContext(ctx, "SELECT count(*) FROM orders").Scan(&k); err != nil || k != 3 {
			return fmt.Errorf("%d rows, %v", k, err)
		}
		return nil
	})
}
