//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
)

// TestGcloudDatabaseMigration (#2252): gcloud database-migration, through
// gcloud-setup's datamigration override alone: a PostgreSQL source
// profile (a container on the kind network, without pglogical) and a
// Cloud SQL destination profile; a continuous migration job with
// --use-postgres-native, which needs no pglogical; describe and list;
// start, the rows and a row written after reach the destination; stop,
// resume, promote and delete. The official client reads back what gcloud
// made.
// covers: google.cloud.clouddms.v1.DataMigrationService/CreateConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/GetConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/ListConnectionProfiles, google.cloud.clouddms.v1.DataMigrationService/DeleteConnectionProfile, google.cloud.clouddms.v1.DataMigrationService/CreateMigrationJob, google.cloud.clouddms.v1.DataMigrationService/GetMigrationJob, google.cloud.clouddms.v1.DataMigrationService/ListMigrationJobs, google.cloud.clouddms.v1.DataMigrationService/StartMigrationJob, google.cloud.clouddms.v1.DataMigrationService/StopMigrationJob, google.cloud.clouddms.v1.DataMigrationService/ResumeMigrationJob, google.cloud.clouddms.v1.DataMigrationService/PromoteMigrationJob, google.cloud.clouddms.v1.DataMigrationService/DeleteMigrationJob
func TestGcloudDatabaseMigration(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	c := dmsClient(t, h)
	ctx := dmsCtx(t)
	g := newGcloudSession(t, h)
	run := func(a ...string) string {
		t.Helper()
		out, err := g.run(nil, append(append([]string{"database-migration"}, a...), "--region", "us-central1", "--project", g.project, "--quiet")...)
		if err != nil {
			t.Fatalf("gcloud database-migration %s: %v\n%s", strings.Join(a, " "), err, out)
		}
		return out
	}
	parent := "projects/" + g.project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	srcID, dstID, jobID := fmt.Sprintf("gc-src-%d", n), fmt.Sprintf("gc-dst-%d", n), fmt.Sprintf("gc-job-%d", n)
	src := startDMSSource(t, false)
	dmsPostgresSeed(t, src)
	t.Cleanup(func() {
		ctx := context.Background()
		if op, err := c.DeleteMigrationJob(ctx, &dmspb.DeleteMigrationJobRequest{Name: parent + "/migrationJobs/" + jobID, Force: true}); err == nil {
			_ = op.Wait(ctx)
		}
		for _, p := range []string{dstID, srcID} {
			if op, err := c.DeleteConnectionProfile(ctx, &dmspb.DeleteConnectionProfileRequest{Name: parent + "/connectionProfiles/" + p}); err == nil {
				_ = op.Wait(ctx)
			}
		}
	})

	run("connection-profiles", "create", "postgresql", srcID, "--host", src.ip, "--port", "5432",
		"--username", "postgres", "--password", dmsSourcePass, "--display-name", "gcloud source")
	run("connection-profiles", "create", "cloudsql", dstID, "--source-id", srcID, "--database-version", "POSTGRES_15",
		"--tier", "db-custom-1-3840", "--root-password", pgRootPassword)
	if out := run("connection-profiles", "list", "--format", "value(name)"); !strings.Contains(out, srcID) || !strings.Contains(out, dstID) {
		t.Errorf("gcloud database-migration connection-profiles list: %s", out)
	}
	if out := run("connection-profiles", "describe", srcID, "--format", "value(state,postgresql.host)"); !strings.Contains(out, "READY") || !strings.Contains(out, src.ip) {
		t.Errorf("gcloud database-migration connection-profiles describe: %s", out)
	}

	run("migration-jobs", "create", jobID, "--source", srcID, "--destination", dstID, "--type", "CONTINUOUS", "--use-postgres-native")
	job := parent + "/migrationJobs/" + jobID
	if out := run("migration-jobs", "describe", jobID, "--format", "value(state,type)"); !strings.Contains(out, "NOT_STARTED") || !strings.Contains(out, "CONTINUOUS") {
		t.Errorf("gcloud database-migration migration-jobs describe: %s", out)
	}
	if out := run("migration-jobs", "list", "--format", "value(name)"); !strings.Contains(out, jobID) {
		t.Errorf("gcloud database-migration migration-jobs list: %s", out)
	}
	run("migration-jobs", "start", jobID)
	dmsWaitFor(t, ctx, c, job, "the CDC phase", inCDC)

	shop := sqlEngine{}.open(t, h, g.project+":us-central1:"+dstID, "postgres", pgRootPassword, "shop")
	count := func() (int, error) {
		var k int
		err := shop.QueryRowContext(ctx, "SELECT count(*) FROM orders").Scan(&k)
		return k, err
	}
	if k, err := count(); err != nil || k != 50 {
		t.Fatalf("the native full dump: %d rows, %v", k, err)
	}
	src.mustExec(t, "shop", "INSERT INTO orders (item) VALUES ('after-start')")
	eventually(t, "the CDC row", func() error {
		if k, err := count(); err != nil || k != 51 {
			return fmt.Errorf("%d rows, %v", k, err)
		}
		return nil
	})
	run("migration-jobs", "stop", jobID)
	if j := dmsJobState(t, ctx, c, job); j.GetState() != dmspb.MigrationJob_STOPPED {
		t.Errorf("after gcloud stop: %v", j.GetState())
	}
	run("migration-jobs", "resume", jobID)
	dmsWaitFor(t, ctx, c, job, "the CDC phase again", inCDC)
	run("migration-jobs", "promote", jobID)
	if j := dmsJobState(t, ctx, c, job); j.GetState() != dmspb.MigrationJob_COMPLETED {
		t.Errorf("after gcloud promote: %v", j.GetState())
	}
	run("migration-jobs", "delete", jobID)
	run("connection-profiles", "delete", srcID)
	if out := run("connection-profiles", "list", "--format", "value(name)"); strings.Contains(out, srcID) || strings.Contains(out, dstID) {
		t.Errorf("profiles after gcloud delete: %s", out)
	}
}
