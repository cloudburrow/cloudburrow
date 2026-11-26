//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	clouddms "cloud.google.com/go/clouddms/apiv1"
	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// EnvDataMigration is Database Migration Service's endpoint (#2252).
const EnvDataMigration = "CLOUDBURROW_TEST_DATAMIGRATION"

// The sources' images: the instance's own Cloud SQL images, so nothing
// else is pulled (components/optional.go).
const (
	dmsPostgresImage = "postgres:15-alpine@sha256:f7d23353e1b15400d22ebe31189f4d314b87a4c129cc400c8c2d8d4ca127bf81"
	dmsMySQLImage    = "mysql:8.0@sha256:7dcddc01f13bab2f15cde676d44d01f61fc9f99fe7785e86196dfc07d358ae2b"
	dmsSourcePass    = "Src-pass1"
)

func dmsClient(t *testing.T, h *Harness) *clouddms.DataMigrationClient {
	t.Helper()
	c, err := clouddms.NewDataMigrationClient(h.Context(), option.WithEndpoint(h.Endpoint(EnvDataMigration)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func dmsCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// dmsSource is a source database server in a container on the kind
// network, which the destinations' servers reach at its address there.
type dmsSource struct {
	name, ip string
	mysql    bool
}

func startDMSSource(t *testing.T, mysql bool) *dmsSource {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("needs docker")
	}
	name := fmt.Sprintf("cb-dms-src-%d", time.Now().UnixNano()%1e9)
	args := []string{"run", "-d", "--name", name, "--network", "kind"}
	if mysql {
		args = append(args, "-e", "MYSQL_ROOT_PASSWORD="+dmsSourcePass, dmsMySQLImage,
			"--server-id=77", "--log-bin=mysql-bin", "--binlog-format=ROW", "--gtid-mode=ON", "--enforce-gtid-consistency=ON")
	} else {
		args = append(args, "-e", "POSTGRES_PASSWORD="+dmsSourcePass, dmsPostgresImage,
			"-c", "wal_level=logical", "-c", "max_replication_slots=10", "-c", "max_wal_senders=10", "-c", "max_worker_processes=16")
	}
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker run the source: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	s := &dmsSource{name: name, mysql: mysql}
	out, err := exec.Command("docker", "inspect", "-f", "{{(index .NetworkSettings.Networks \"kind\").IPAddress}}", name).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("the source's address on the kind network: %v %s", err, out)
	}
	s.ip = strings.TrimSpace(string(out))
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if _, err := s.exec("", "SELECT 1"); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the source did not start: %v", err)
		}
		time.Sleep(time.Second)
	}
	return s
}

// exec runs a statement on the source over TCP inside its container (the
// entrypoint's first server listens on its socket only).
func (s *dmsSource) exec(db, stmt string) (string, error) {
	var args []string
	if s.mysql {
		args = []string{"exec", "-e", "MYSQL_PWD=" + dmsSourcePass, s.name, "mysql", "-h127.0.0.1", "-uroot", "-N", "-B", "-e", stmt}
		if db != "" {
			args = append(args, db)
		}
	} else {
		if db == "" {
			db = "postgres"
		}
		args = []string{"exec", "-e", "PGPASSWORD=" + dmsSourcePass, s.name, "psql", "-h", "127.0.0.1", "-U", "postgres", "-d", db, "-qAt", "-v", "ON_ERROR_STOP=1", "-c", stmt}
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %v: %s", stmt, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *dmsSource) mustExec(t *testing.T, db string, stmts ...string) {
	t.Helper()
	for _, st := range stmts {
		if _, err := s.exec(db, st); err != nil {
			t.Fatal(err)
		}
	}
}

func dmsSourceProfile(s *dmsSource) *dmspb.ConnectionProfile {
	if s.mysql {
		return &dmspb.ConnectionProfile{DisplayName: "MySQL source", ConnectionProfile: &dmspb.ConnectionProfile_Mysql{Mysql: &dmspb.MySqlConnectionProfile{
			Host: s.ip, Port: 3306, Username: "root", Password: dmsSourcePass}}}
	}
	return &dmspb.ConnectionProfile{DisplayName: "PostgreSQL source", ConnectionProfile: &dmspb.ConnectionProfile_Postgresql{Postgresql: &dmspb.PostgreSqlConnectionProfile{
		Host: s.ip, Port: 5432, Username: "postgres", Password: dmsSourcePass}}}
}

func dmsCreateProfile(t *testing.T, ctx context.Context, c *clouddms.DataMigrationClient, parent, id string, p *dmspb.ConnectionProfile) *dmspb.ConnectionProfile {
	t.Helper()
	op, err := c.CreateConnectionProfile(ctx, &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: id, ConnectionProfile: p})
	if err != nil {
		t.Fatalf("create the connection profile %s: %v", id, err)
	}
	got, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("create the connection profile %s: %v", id, err)
	}
	if got.GetState() != dmspb.ConnectionProfile_READY {
		t.Fatalf("the connection profile %s is %v", id, got.GetState())
	}
	return got
}

// dmsVerificationCodes are the codes of a failed verification's details,
// read from the operation as Google's clients read it.
func dmsVerificationCodes(t *testing.T, ctx context.Context, c *clouddms.DataMigrationClient, opName string) map[dmspb.MigrationJobVerificationError_ErrorCode]bool {
	t.Helper()
	op, err := c.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: opName})
	if err != nil {
		t.Fatal(err)
	}
	out := map[dmspb.MigrationJobVerificationError_ErrorCode]bool{}
	for _, d := range op.GetError().GetDetails() {
		var v dmspb.MigrationJobVerificationError
		if d.UnmarshalTo(&v) == nil {
			out[v.GetErrorCode()] = true
		}
	}
	return out
}

func dmsJobState(t *testing.T, ctx context.Context, c *clouddms.DataMigrationClient, name string) *dmspb.MigrationJob {
	t.Helper()
	j, err := c.GetMigrationJob(ctx, &dmspb.GetMigrationJobRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func dmsWaitFor(t *testing.T, ctx context.Context, c *clouddms.DataMigrationClient, name, what string, ok func(*dmspb.MigrationJob) bool) *dmspb.MigrationJob {
	t.Helper()
	deadline := time.Now().Add(8 * time.Minute)
	for {
		j := dmsJobState(t, ctx, c, name)
		if ok(j) {
			return j
		}
		if j.GetState() == dmspb.MigrationJob_FAILED {
			t.Fatalf("migration job %s failed waiting for %s: %v", name, what, j.GetError())
		}
		if time.Now().After(deadline) {
			t.Fatalf("migration job %s: timed out waiting for %s: %v %v", name, what, j.GetState(), j.GetPhase())
		}
		time.Sleep(2 * time.Second)
	}
}

func inCDC(j *dmspb.MigrationJob) bool {
	return j.GetState() == dmspb.MigrationJob_RUNNING && j.GetPhase() == dmspb.MigrationJob_CDC
}

// dmsPostgresSeed gives a PostgreSQL source a database with rows and a
// sequence a migration must carry.
func dmsPostgresSeed(t *testing.T, src *dmsSource) {
	src.mustExec(t, "", "CREATE DATABASE shop")
	src.mustExec(t, "shop", "CREATE TABLE orders (id serial PRIMARY KEY, item text NOT NULL)",
		"INSERT INTO orders (item) SELECT 'item-' || g FROM generate_series(1, 50) g")
}

// TestDataMigrationPostgresToCloudSQL (#2252): through the official Go
// client, a continuous migration of a PostgreSQL server (a container on
// the kind network) into a Cloud SQL for PostgreSQL destination the
// destination profile creates: the source profile is READY at once, the
// destination's replica instance is created; verification of a source
// without pglogical fails with Google's NO_PGLOGICAL_INSTALLED and
// INVALID_SHARED_PRELOAD_LIBRARY, and of a wrong password with
// AUTHENTICATION_FAILURE; started with skipValidation the job's full dump
// copies the rows and its CDC phase replicates a row written after; Stop
// holds a row back and Resume lets it through; Promote ends replication
// with the source's sequence carried, so the destination takes a row of
// its own; Delete with force removes the profile and the instance.
// covers: google.cloud.clouddms.v1.DataMigrationService/CreateConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/GetConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/ListConnectionProfiles, google.cloud.clouddms.v1.DataMigrationService/UpdateConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/DeleteConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/CreateMigrationJob, google.cloud.clouddms.v1.DataMigrationService/GetMigrationJob, google.cloud.clouddms.v1.DataMigrationService/ListMigrationJobs, google.cloud.clouddms.v1.DataMigrationService/VerifyMigrationJob, google.cloud.clouddms.v1.DataMigrationService/StartMigrationJob, google.cloud.clouddms.v1.DataMigrationService/StopMigrationJob, google.cloud.clouddms.v1.DataMigrationService/ResumeMigrationJob, google.cloud.clouddms.v1.DataMigrationService/PromoteMigrationJob, google.cloud.clouddms.v1.DataMigrationService/DeleteMigrationJob
func TestDataMigrationPostgresToCloudSQL(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	c := dmsClient(t, h)
	ctx := dmsCtx(t)
	project := h.Project()
	parent := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	src := startDMSSource(t, false)
	dmsPostgresSeed(t, src)

	srcID, dstID, jobID := fmt.Sprintf("pg-src-%d", n), fmt.Sprintf("pg-dst-%d", n), fmt.Sprintf("pg-job-%d", n)
	dmsCreateProfile(t, ctx, c, parent, srcID, dmsSourceProfile(src))
	got, err := c.GetConnectionProfile(ctx, &dmspb.GetConnectionProfileRequest{Name: parent + "/connectionProfiles/" + srcID})
	if err != nil || got.GetPostgresql().GetPassword() != "" || !got.GetPostgresql().GetPasswordSet() {
		t.Fatalf("the source profile answers its password: %v %v", got, err)
	}
	dst := dmsCreateProfile(t, ctx, c, parent, dstID, &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Cloudsql{
		Cloudsql: &dmspb.CloudSqlConnectionProfile{Settings: &dmspb.CloudSqlSettings{SourceId: parent + "/connectionProfiles/" + srcID,
			DatabaseVersion: dmspb.CloudSqlSettings_POSTGRES_15, Tier: "db-custom-1-3840", RootPassword: pgRootPassword}}}})
	if dst.GetCloudsql().GetCloudSqlId() != dstID {
		t.Errorf("the destination's Cloud SQL ID: %q", dst.GetCloudsql().GetCloudSqlId())
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if op, err := c.DeleteMigrationJob(ctx, &dmspb.DeleteMigrationJobRequest{Name: parent + "/migrationJobs/" + jobID, Force: true}); err == nil {
			_ = op.Wait(ctx)
		}
		if op, err := c.DeleteConnectionProfile(ctx, &dmspb.DeleteConnectionProfileRequest{Name: parent + "/connectionProfiles/" + srcID}); err == nil {
			_ = op.Wait(ctx)
		}
	})

	job := parent + "/migrationJobs/" + jobID
	jop, err := c.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: jobID, MigrationJob: &dmspb.MigrationJob{
		Type: dmspb.MigrationJob_CONTINUOUS, Source: parent + "/connectionProfiles/" + srcID, Destination: parent + "/connectionProfiles/" + dstID,
		Connectivity: &dmspb.MigrationJob_StaticIpConnectivity{StaticIpConnectivity: &dmspb.StaticIpConnectivity{}}}})
	if err != nil {
		t.Fatal(err)
	}
	if j, err := jop.Wait(ctx); err != nil || j.GetState() != dmspb.MigrationJob_NOT_STARTED {
		t.Fatalf("create the migration job: %v %v", j, err)
	}
	list, err := c.ListMigrationJobs(ctx, &dmspb.ListMigrationJobsRequest{Parent: parent}).Next()
	if err != nil || list == nil {
		t.Fatalf("list the migration jobs: %v", err)
	}

	// The source has no pglogical, which a standard migration needs.
	vop, err := c.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: job})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vop.Wait(ctx); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("verify without pglogical: %v", err)
	}
	if codes := dmsVerificationCodes(t, ctx, c, vop.Name()); !codes[dmspb.MigrationJobVerificationError_NO_PGLOGICAL_INSTALLED] ||
		!codes[dmspb.MigrationJobVerificationError_INVALID_SHARED_PRELOAD_LIBRARY] {
		t.Errorf("verify without pglogical: %v", codes)
	}
	// A wrong password is AUTHENTICATION_FAILURE.
	if op, err := c.UpdateConnectionProfile(ctx, &dmspb.UpdateConnectionProfileRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"postgresql.password"}},
		ConnectionProfile: &dmspb.ConnectionProfile{Name: parent + "/connectionProfiles/" + srcID, ConnectionProfile: &dmspb.ConnectionProfile_Postgresql{
			Postgresql: &dmspb.PostgreSqlConnectionProfile{Password: "wrong"}}}}); err != nil {
		t.Fatal(err)
	} else if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	vop, err = c.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: job})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = vop.Wait(ctx)
	if codes := dmsVerificationCodes(t, ctx, c, vop.Name()); !codes[dmspb.MigrationJobVerificationError_AUTHENTICATION_FAILURE] {
		t.Errorf("verify with a wrong password: %v", codes)
	}
	if op, err := c.UpdateConnectionProfile(ctx, &dmspb.UpdateConnectionProfileRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"postgresql.password"}},
		ConnectionProfile: &dmspb.ConnectionProfile{Name: parent + "/connectionProfiles/" + srcID, ConnectionProfile: &dmspb.ConnectionProfile_Postgresql{
			Postgresql: &dmspb.PostgreSqlConnectionProfile{Password: dmsSourcePass}}}}); err != nil {
		t.Fatal(err)
	} else if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// Resume is not allowed before the job runs.
	if _, err := c.ResumeMigrationJob(ctx, &dmspb.ResumeMigrationJobRequest{Name: job}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("resume a job not started: %v", err)
	}

	sop, err := c.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: job, SkipValidation: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sop.Wait(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	dmsWaitFor(t, ctx, c, job, "the CDC phase", inCDC)

	conn := project + ":us-central1:" + dstID
	shop := sqlEngine{}.open(t, h, conn, "postgres", pgRootPassword, "shop")
	count := func() (int, error) {
		var k int
		err := shop.QueryRowContext(ctx, "SELECT count(*) FROM orders").Scan(&k)
		return k, err
	}
	if k, err := count(); err != nil || k != 50 {
		t.Fatalf("the full dump: %d rows, %v", k, err)
	}
	src.mustExec(t, "shop", "INSERT INTO orders (item) VALUES ('after-start')")
	eventually(t, "the CDC row", func() error {
		if k, err := count(); err != nil || k != 51 {
			return fmt.Errorf("%d rows, %v", k, err)
		}
		return nil
	})

	stop, err := c.StopMigrationJob(ctx, &dmspb.StopMigrationJobRequest{Name: job})
	if err != nil {
		t.Fatal(err)
	}
	if j, err := stop.Wait(ctx); err != nil || j.GetState() != dmspb.MigrationJob_STOPPED {
		t.Fatalf("stop: %v %v", j.GetState(), err)
	}
	src.mustExec(t, "shop", "INSERT INTO orders (item) VALUES ('while-stopped')")
	time.Sleep(8 * time.Second)
	if k, _ := count(); k != 51 {
		t.Errorf("a stopped job replicated: %d rows", k)
	}
	res, err := c.ResumeMigrationJob(ctx, &dmspb.ResumeMigrationJobRequest{Name: job})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Wait(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	eventually(t, "the row written while stopped", func() error {
		if k, err := count(); err != nil || k != 52 {
			return fmt.Errorf("%d rows, %v", k, err)
		}
		return nil
	})

	pop, err := c.PromoteMigrationJob(ctx, &dmspb.PromoteMigrationJobRequest{Name: job})
	if err != nil {
		t.Fatal(err)
	}
	if j, err := pop.Wait(ctx); err != nil || j.GetState() != dmspb.MigrationJob_COMPLETED {
		t.Fatalf("promote: %v %v", j, err)
	}
	// The sequence came with the data: a row of the destination's own
	// takes the next id.
	var id int
	if err := shop.QueryRowContext(ctx, "INSERT INTO orders (item) VALUES ('on-destination') RETURNING id").Scan(&id); err != nil || id != 53 {
		t.Fatalf("a write on the promoted destination: id %d, %v", id, err)
	}
	src.mustExec(t, "shop", "INSERT INTO orders (item) VALUES ('after-promote')")
	time.Sleep(5 * time.Second)
	if k, _ := count(); k != 53 {
		t.Errorf("a promoted destination still replicates: %d rows", k)
	}

	dop, err := c.DeleteMigrationJob(ctx, &dmspb.DeleteMigrationJobRequest{Name: job, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.GetConnectionProfile(ctx, &dmspb.GetConnectionProfileRequest{Name: parent + "/connectionProfiles/" + dstID}); status.Code(err) != codes.NotFound {
		t.Errorf("the destination profile after the job's delete: %v", err)
	}
	svc := sqlAdminService(t, h)
	eventually(t, "the destination instance deleted", func() error {
		if _, err := svc.Instances.Get(project, dstID).Do(); err == nil {
			return fmt.Errorf("%s still exists", dstID)
		}
		return nil
	})
}

// TestDataMigrationPostgresToAlloyDB (#2252): a continuous migration of
// a PostgreSQL server into an AlloyDB cluster the destination profile
// creates (its cluster and primary instance); the rows and a row written
// after reach it, read through AlloyDB's ExecuteSql; Promote ends
// replication.
// covers: google.cloud.clouddms.v1.DataMigrationService/CreateConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/CreateMigrationJob, google.cloud.clouddms.v1.DataMigrationService/StartMigrationJob, google.cloud.clouddms.v1.DataMigrationService/PromoteMigrationJob, google.cloud.clouddms.v1.DataMigrationService/DeleteMigrationJob
func TestDataMigrationPostgresToAlloyDB(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvAlloyDB) == "" {
		t.Skip("needs --services alloydb")
	}
	c := dmsClient(t, h)
	adb := alloyDBClients(t, h)["grpc"]
	ctx := dmsCtx(t)
	project := h.Project()
	parent := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	src := startDMSSource(t, false)
	dmsPostgresSeed(t, src)
	srcID, dstID, jobID := fmt.Sprintf("pga-src-%d", n), fmt.Sprintf("pga-dst-%d", n), fmt.Sprintf("pga-job-%d", n)
	cluster := fmt.Sprintf("dms-adb-%d", n)
	dmsCreateProfile(t, ctx, c, parent, srcID, dmsSourceProfile(src))
	dmsCreateProfile(t, ctx, c, parent, dstID, &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Alloydb{Alloydb: &dmspb.AlloyDbConnectionProfile{
		ClusterId: cluster, Settings: &dmspb.AlloyDbSettings{InitialUser: &dmspb.AlloyDbSettings_UserPassword{User: "postgres", Password: "Adb-pass1"},
			VpcNetwork: "projects/" + project + "/global/networks/default",
			PrimaryInstanceSettings: &dmspb.AlloyDbSettings_PrimaryInstanceSettings{Id: "primary",
				MachineConfig: &dmspb.AlloyDbSettings_PrimaryInstanceSettings_MachineConfig{CpuCount: 2}}}}}})
	t.Cleanup(func() {
		ctx := context.Background()
		if op, err := c.DeleteMigrationJob(ctx, &dmspb.DeleteMigrationJobRequest{Name: parent + "/migrationJobs/" + jobID}); err == nil {
			_ = op.Wait(ctx)
		}
		for _, p := range []string{dstID, srcID} {
			if op, err := c.DeleteConnectionProfile(ctx, &dmspb.DeleteConnectionProfileRequest{Name: parent + "/connectionProfiles/" + p}); err == nil {
				_ = op.Wait(ctx)
			}
		}
		if op, err := adb.DeleteCluster(ctx, &alloydbpb.DeleteClusterRequest{Name: parent + "/clusters/" + cluster, Force: true}); err == nil {
			_ = op.Wait(ctx)
		}
	})
	if cl, err := adb.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: parent + "/clusters/" + cluster}); err != nil {
		t.Fatalf("the destination's AlloyDB cluster: %v", err)
	} else if cl.GetState() != alloydbpb.Cluster_READY {
		t.Errorf("the destination's AlloyDB cluster is %v", cl.GetState())
	}

	job := parent + "/migrationJobs/" + jobID
	jop, err := c.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: jobID, MigrationJob: &dmspb.MigrationJob{
		Type: dmspb.MigrationJob_CONTINUOUS, Source: parent + "/connectionProfiles/" + srcID, Destination: parent + "/connectionProfiles/" + dstID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	sop, err := c.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: job, SkipValidation: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sop.Wait(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	dmsWaitFor(t, ctx, c, job, "the CDC phase", inCDC)
	primary := parent + "/clusters/" + cluster + "/instances/primary"
	count := func() (string, error) {
		res, err := adb.ExecuteSql(ctx, &alloydbpb.ExecuteSqlRequest{Instance: primary, Database: "shop", User: "postgres",
			UserCredential: &alloydbpb.ExecuteSqlRequest_Password{Password: "Adb-pass1"}, SqlStatement: "SELECT count(*) FROM orders"})
		if err != nil {
			return "", err
		}
		if len(res.GetSqlResults()) != 1 || len(res.GetSqlResults()[0].GetRows()) != 1 {
			return "", fmt.Errorf("unexpected answer %v", res)
		}
		return res.GetSqlResults()[0].GetRows()[0].GetValues()[0].GetValue(), nil
	}
	if k, err := count(); err != nil || k != "50" {
		t.Fatalf("the full dump on AlloyDB: %s rows, %v", k, err)
	}
	src.mustExec(t, "shop", "INSERT INTO orders (item) VALUES ('after-start')")
	eventually(t, "the CDC row on AlloyDB", func() error {
		if k, err := count(); err != nil || k != "51" {
			return fmt.Errorf("%s rows, %v", k, err)
		}
		return nil
	})
	pop, err := c.PromoteMigrationJob(ctx, &dmspb.PromoteMigrationJobRequest{Name: job})
	if err != nil {
		t.Fatal(err)
	}
	if j, err := pop.Wait(ctx); err != nil || j.GetState() != dmspb.MigrationJob_COMPLETED {
		t.Fatalf("promote: %v %v", j, err)
	}
	if slots, err := src.exec("shop", "SELECT count(*) FROM pg_replication_slots"); err != nil || slots != "0" {
		t.Errorf("the source's replication slots after promote: %s %v", slots, err)
	}
}

// TestDataMigrationMySQLToCloudSQL (#2252): a continuous migration of a
// MySQL 8.0 server (GTID on, a container on the kind network) into a
// Cloud SQL for MySQL destination: verification passes, the dump copies
// the rows, replication carries a row written after, and Promote leaves
// the destination writable; a ONE_TIME job of a second destination
// copies the rows once and is COMPLETED.
// covers: google.cloud.clouddms.v1.DataMigrationService/CreateConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/CreateMigrationJob, google.cloud.clouddms.v1.DataMigrationService/VerifyMigrationJob, google.cloud.clouddms.v1.DataMigrationService/StartMigrationJob, google.cloud.clouddms.v1.DataMigrationService/PromoteMigrationJob, google.cloud.clouddms.v1.DataMigrationService/DeleteMigrationJob
func TestDataMigrationMySQLToCloudSQL(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	c := dmsClient(t, h)
	ctx := dmsCtx(t)
	project := h.Project()
	parent := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	src := startDMSSource(t, true)
	src.mustExec(t, "", "CREATE DATABASE shop")
	src.mustExec(t, "shop", "CREATE TABLE orders (id int AUTO_INCREMENT PRIMARY KEY, item varchar(64) NOT NULL) ENGINE=InnoDB",
		"INSERT INTO orders (item) VALUES ('a'), ('b'), ('c'), ('d'), ('e')")
	srcID := fmt.Sprintf("my-src-%d", n)
	dmsCreateProfile(t, ctx, c, parent, srcID, dmsSourceProfile(src))
	var jobs []string
	t.Cleanup(func() {
		ctx := context.Background()
		for _, j := range jobs {
			if op, err := c.DeleteMigrationJob(ctx, &dmspb.DeleteMigrationJobRequest{Name: j, Force: true}); err == nil {
				_ = op.Wait(ctx)
			}
		}
		if op, err := c.DeleteConnectionProfile(ctx, &dmspb.DeleteConnectionProfileRequest{Name: parent + "/connectionProfiles/" + srcID}); err == nil {
			_ = op.Wait(ctx)
		}
	})
	run := func(kind dmspb.MigrationJob_Type, suffix string) (string, string) {
		dstID, jobID := fmt.Sprintf("my-dst-%s-%d", suffix, n), fmt.Sprintf("my-job-%s-%d", suffix, n)
		dmsCreateProfile(t, ctx, c, parent, dstID, &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Cloudsql{
			Cloudsql: &dmspb.CloudSqlConnectionProfile{Settings: &dmspb.CloudSqlSettings{SourceId: parent + "/connectionProfiles/" + srcID,
				DatabaseVersion: dmspb.CloudSqlSettings_MYSQL_8_0, Tier: "db-custom-1-3840", RootPassword: "Root-pass1"}}}})
		job := parent + "/migrationJobs/" + jobID
		jobs = append(jobs, job)
		jop, err := c.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: jobID, MigrationJob: &dmspb.MigrationJob{
			Type: kind, Source: parent + "/connectionProfiles/" + srcID, Destination: parent + "/connectionProfiles/" + dstID}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := jop.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		vop, err := c.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: job})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := vop.Wait(ctx); err != nil {
			t.Fatalf("verify a ready MySQL source: %v %v", err, dmsVerificationCodes(t, ctx, c, vop.Name()))
		}
		sop, err := c.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: job})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sop.Wait(ctx); err != nil {
			t.Fatalf("start: %v", err)
		}
		return job, project + ":us-central1:" + dstID
	}

	job, conn := run(dmspb.MigrationJob_CONTINUOUS, "c")
	dmsWaitFor(t, ctx, c, job, "the CDC phase", inCDC)
	shop := mysqlOpen(t, h, conn, "root", "Root-pass1", "shop")
	t.Cleanup(func() { _ = shop.Close() })
	count := func() (int, error) {
		var k int
		err := shop.QueryRowContext(ctx, "SELECT count(*) FROM orders").Scan(&k)
		return k, err
	}
	eventually(t, "the dump on Cloud SQL for MySQL", func() error {
		if k, err := count(); err != nil || k != 5 {
			return fmt.Errorf("%d rows, %v", k, err)
		}
		return nil
	})
	src.mustExec(t, "shop", "INSERT INTO orders (item) VALUES ('after-start')")
	eventually(t, "the replicated row", func() error {
		if k, err := count(); err != nil || k != 6 {
			return fmt.Errorf("%d rows, %v", k, err)
		}
		return nil
	})
	pop, err := c.PromoteMigrationJob(ctx, &dmspb.PromoteMigrationJobRequest{Name: job})
	if err != nil {
		t.Fatal(err)
	}
	if j, err := pop.Wait(ctx); err != nil || j.GetState() != dmspb.MigrationJob_COMPLETED {
		t.Fatalf("promote: %v %v", j, err)
	}
	eventually(t, "a write on the promoted destination", func() error {
		_, err := shop.ExecContext(ctx, "INSERT INTO orders (item) VALUES ('on-destination')")
		return err
	})

	once, onceConn := run(dmspb.MigrationJob_ONE_TIME, "o")
	j := dmsWaitFor(t, ctx, c, once, "COMPLETED", func(j *dmspb.MigrationJob) bool { return j.GetState() == dmspb.MigrationJob_COMPLETED })
	if j.GetEndTime() == nil {
		t.Errorf("a completed one-time job has no end time")
	}
	o := mysqlOpen(t, h, onceConn, "root", "Root-pass1", "shop")
	t.Cleanup(func() { _ = o.Close() })
	var k int
	if err := o.QueryRowContext(ctx, "SELECT count(*) FROM orders").Scan(&k); err != nil || k != 6 {
		t.Fatalf("the one-time copy: %d rows, %v", k, err)
	}
}

// TestDataMigrationUnserved (#2252): what Database Migration Service
// does not run answers UNIMPLEMENTED: conversion workspaces, an Oracle
// source and the SSH script; fetchStaticIps answers the address the
// destinations connect from.
// covers: google.cloud.clouddms.v1.DataMigrationService/FetchStaticIps, google.cloud.clouddms.v1.DataMigrationService/CreatePrivateConnection, google.cloud.clouddms.v1.DataMigrationService/GetPrivateConnection, google.cloud.clouddms.v1.DataMigrationService/ListPrivateConnections, google.cloud.clouddms.v1.DataMigrationService/DeletePrivateConnection
func TestDataMigrationUnserved(t *testing.T) {
	h := New(t)
	c := dmsClient(t, h)
	ctx := dmsCtx(t)
	parent := "projects/" + h.Project() + "/locations/us-central1"
	if _, err := c.ListConversionWorkspaces(ctx, &dmspb.ListConversionWorkspacesRequest{Parent: parent}).Next(); status.Code(err) != codes.Unimplemented {
		t.Errorf("conversion workspaces: %v", err)
	}
	if _, err := c.CreateConnectionProfile(ctx, &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: "ora",
		ConnectionProfile: &dmspb.ConnectionProfile{ConnectionProfile: &dmspb.ConnectionProfile_Oracle{Oracle: &dmspb.OracleConnectionProfile{
			Host: "ora", Port: 1521, Username: "u", Password: "p", DatabaseService: "ORCL"}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("an Oracle profile: %v", err)
	}
	if _, err := c.GenerateSshScript(ctx, &dmspb.GenerateSshScriptRequest{MigrationJob: parent + "/migrationJobs/none", Vm: "bastion"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("generateSshScript: %v", err)
	}
	ips, err := c.FetchStaticIps(ctx, &dmspb.FetchStaticIpsRequest{Name: parent}).Next()
	if err != nil || ips == "" {
		t.Errorf("fetchStaticIps: %q %v", ips, err)
	}
	id := fmt.Sprintf("pc-%d", time.Now().UnixNano()%1e6)
	op, err := c.CreatePrivateConnection(ctx, &dmspb.CreatePrivateConnectionRequest{Parent: parent, PrivateConnectionId: id, PrivateConnection: &dmspb.PrivateConnection{
		Connectivity: &dmspb.PrivateConnection_VpcPeeringConfig{VpcPeeringConfig: &dmspb.VpcPeeringConfig{
			VpcName: "projects/" + h.Project() + "/global/networks/default", Subnet: "10.240.0.0/29"}}}})
	if err != nil {
		t.Fatal(err)
	}
	pc, err := op.Wait(ctx)
	if err != nil || pc.GetState() != dmspb.PrivateConnection_CREATED {
		t.Fatalf("create a private connection: %v %v", pc, err)
	}
	if _, err := c.CreatePrivateConnection(ctx, &dmspb.CreatePrivateConnectionRequest{Parent: parent, PrivateConnectionId: id + "b", PrivateConnection: &dmspb.PrivateConnection{
		Connectivity: &dmspb.PrivateConnection_VpcPeeringConfig{VpcPeeringConfig: &dmspb.VpcPeeringConfig{
			VpcName: "projects/" + h.Project() + "/global/networks/default", Subnet: "10.240.0.0/28"}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a /28 subnet: %v", err)
	}
	if _, err := c.GetPrivateConnection(ctx, &dmspb.GetPrivateConnectionRequest{Name: parent + "/privateConnections/" + id}); err != nil {
		t.Fatal(err)
	}
	dop, err := c.DeletePrivateConnection(ctx, &dmspb.DeletePrivateConnectionRequest{Name: parent + "/privateConnections/" + id})
	if err != nil {
		t.Fatal(err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
