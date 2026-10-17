//go:build compat

package compat

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// postgresImage is the instance's own PostgreSQL image, which the kind node
// already holds: it has psql and a shell, which is all the fixtures need.
const postgresImage = "docker.io/library/postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24"

// runCloudSQLConn is the connection name of the instance's PostgreSQL, or a
// skip when Cloud SQL is not on this instance.
func runCloudSQLConn(t *testing.T, h *Harness) string {
	t.Helper()
	// CI's run shard exports RUN_SQLADMIN, so the Cloud SQL suite does not
	// run there a second time; an instance with every service needs only
	// the ordinary variables.
	if strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_RUN_SQLADMIN")) == "" &&
		(strings.TrimSpace(os.Getenv(EnvSQLAdmin)) == "" || strings.TrimSpace(os.Getenv(EnvCloudSQL)) == "") {
		t.Skip("Cloud SQL is not enabled on this instance; run with --services run,cloudsql")
	}
	// The Admin API serves either engine: an instance with only Cloud SQL
	// for MySQL has no postgres instance, and skips (#1304).
	svc, err := sqladmin.NewService(h.Context(), option.WithoutAuthentication(),
		option.WithEndpoint("http://"+runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_SQLADMIN", EnvSQLAdmin)+"/"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Instances.Get(h.Project(), "postgres").Do(); httpCode(err) == http.StatusNotFound {
		t.Skip("Cloud SQL for PostgreSQL is not enabled on this instance; run with --services run,cloudsql")
	} else if err != nil {
		t.Fatalf("the postgres instance: %v", err)
	}
	return h.Project() + ":us-central1:postgres"
}

func cloudSQLVolume(name string, conns ...string) *runpb.Volume {
	return &runpb.Volume{Name: name, VolumeType: &runpb.Volume_CloudSqlInstance{
		CloudSqlInstance: &runpb.CloudSqlInstance{Instances: conns}}}
}

// cloudSQLServeScript answers each HTTP request with what `SELECT 1` returns
// through the Unix socket the Cloud SQL volume holds.
const cloudSQLServeScript = `cat > /tmp/h <<'EOF'
#!/bin/sh
while read -r l; do l=$(printf %s "$l" | tr -d '\r'); [ -z "$l" ] && break; done
r=$(psql -h "/cloudsql/$CONN" -U cloudburrow -d cloudburrow -tAc 'SELECT 1' 2>&1)
printf 'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' "${#r}" "$r"
EOF
chmod +x /tmp/h
while true; do nc -l -p 8080 -e /tmp/h; done`

// cloudSQLSelectOne waits for a service to answer 1, what SELECT 1 returned
// through /cloudsql/<conn>.
func cloudSQLSelectOne(t *testing.T, uri, conn string) {
	t.Helper()
	base, host := ingress(t), hostOf(t, uri)
	var code int
	var body string
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		code, body = httpGet(t, base, host, "/")
		if code == http.StatusOK && strings.TrimSpace(body) == "1" {
			return
		}
	}
	t.Fatalf("SELECT 1 through /cloudsql/%s: %d %q", conn, code, body)
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService
//
// TestRunServiceCloudSQLVolume (#1346): a service with a Cloud SQL volume
// gets, as on Cloud Run, a Unix socket at /cloudsql/<connection name>,
// served by the Cloud SQL Auth Proxy beside it; its container connects to
// PostgreSQL through /cloudsql/<conn>/.s.PGSQL.5432 and returns SELECT 1
// over HTTP. The volume reads back as set; an unknown instance is refused.
func TestRunServiceCloudSQLVolume(t *testing.T) {
	h := New(t)
	conn := runCloudSQLConn(t, h)
	c := runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	id := "compat-cloudsql"
	name := runParent(h) + "/services/" + id
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{
			Volumes: []*runpb.Volume{cloudSQLVolume("cloudsql", conn)},
			Containers: []*runpb.Container{{Image: postgresImage, Command: []string{"sh", "-c"}, Args: []string{cloudSQLServeScript},
				Env:          []*runpb.EnvVar{{Name: "CONN", Values: &runpb.EnvVar_Value{Value: conn}}},
				VolumeMounts: []*runpb.VolumeMount{{Name: "cloudsql", MountPath: "/cloudsql"}}}},
		}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = c.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: name})
	})
	svc, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("waiting for the service: %v", err)
	}
	got, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if v := got.GetTemplate().GetVolumes(); len(v) != 1 || len(v[0].GetCloudSqlInstance().GetInstances()) != 1 ||
		v[0].GetCloudSqlInstance().GetInstances()[0] != conn {
		t.Errorf("volumes read back as %v", v)
	}
	cloudSQLSelectOne(t, svc.GetUri(), conn)

	_, err = c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: "compat-cloudsql-unknown",
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{
			Volumes:    []*runpb.Volume{cloudSQLVolume("cloudsql", h.Project()+":us-central1:no-such-db")},
			Containers: []*runpb.Container{{Image: helloDigest}}}}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("an unknown Cloud SQL instance = %v; want InvalidArgument", err)
	}
}

// covers: google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/RunJob
//
// TestRunJobCloudSQLVolume (#1346): a job's task connects to PostgreSQL
// through its Cloud SQL volume's socket and exits 0 only when SELECT 1
// returns 1; the Auth Proxy beside it does not hold the task open.
func TestRunJobCloudSQLVolume(t *testing.T) {
	h := New(t)
	conn := runCloudSQLConn(t, h)
	jc, _ := runJobClients(t, h)
	ctx := h.Context()
	id := "compat-job-cloudsql"
	name := runParent(h) + "/jobs/" + id
	job := shellJob(`for i in $(seq 60); do [ -S "/cloudsql/$CONN/.s.PGSQL.5432" ] && break; sleep 1; done
[ "$(psql -h "/cloudsql/$CONN" -U cloudburrow -d cloudburrow -tAc 'SELECT 1')" = 1 ]`, 1, map[string]string{"CONN": conn})
	tt := job.Template.Template
	tt.Containers[0].Image = postgresImage
	tt.Volumes = []*runpb.Volume{cloudSQLVolume("cloudsql", conn)}
	tt.Containers[0].VolumeMounts = []*runpb.VolumeMount{{Name: "cloudsql", MountPath: "/cloudsql"}}
	op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id, Job: job})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(h.Context(), &runpb.DeleteJobRequest{Name: name}) })
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: name})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if _, err := rop.Wait(ctx); err != nil {
		t.Fatalf("the task did not reach PostgreSQL through /cloudsql/%s: %v", conn, err)
	}
}
