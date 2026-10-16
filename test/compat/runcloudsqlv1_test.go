//go:build compat

package compat

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// runCloudSQLMySQLConn is the connection name of the instance's MySQL, or a
// skip when Cloud SQL for MySQL is not on this instance.
func runCloudSQLMySQLConn(t *testing.T, h *Harness) string {
	t.Helper()
	if strings.TrimSpace(os.Getenv(EnvMySQLPassword)) == "" ||
		(strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_RUN_SQLADMIN")) == "" && strings.TrimSpace(os.Getenv(EnvSQLAdmin)) == "") {
		t.Skip("Cloud SQL for MySQL is not enabled on this instance; run with --services run,cloudsql-mysql")
	}
	return h.Project() + ":us-central1:mysql"
}

// cloudSQLMySQLQueryScript writes what SELECT 1 returns through the MySQL
// socket the Cloud SQL volume holds to /shared/r, every two seconds. The
// MySQL image has no HTTP server, so the serving container returns the file.
const cloudSQLMySQLQueryScript = `while true; do
mysql -S "/cloudsql/$CONN" -u cloudburrow -N -e 'SELECT 1' cloudburrow > /shared/r.new 2>&1; mv /shared/r.new /shared/r
sleep 2; done`

// cloudSQLFileServeScript answers each HTTP request with /shared/r.
const cloudSQLFileServeScript = `cat > /tmp/h <<'EOF'
#!/bin/sh
while read -r l; do l=$(printf %s "$l" | tr -d '\r'); [ -z "$l" ] && break; done
r=$(cat /shared/r 2>&1)
printf 'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' "${#r}" "$r"
EOF
chmod +x /tmp/h
while true; do nc -l -p 8080 -e /tmp/h; done`

// covers: google.cloud.run.v2.Services/CreateService
//
// TestRunServiceCloudSQLMySQLVolume (#1346): a Cloud SQL volume for a MySQL
// instance is, as on Cloud Run, a Unix socket at /cloudsql/<connection
// name> itself. A sidecar running the MySQL image's own client queries
// SELECT 1 through it as the cloudburrow user, and the serving container
// returns the answer over HTTP.
func TestRunServiceCloudSQLMySQLVolume(t *testing.T) {
	h := New(t)
	conn := runCloudSQLMySQLConn(t, h)
	c := runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	id := "compat-cloudsql-mysql"
	name := runParent(h) + "/services/" + id
	shared := &runpb.Volume{Name: "shared", VolumeType: &runpb.Volume_EmptyDir{EmptyDir: &runpb.EmptyDirVolumeSource{
		Medium: runpb.EmptyDirVolumeSource_MEMORY, SizeLimit: "1Mi"}}}
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{
			Volumes: []*runpb.Volume{cloudSQLVolume("cloudsql", conn), shared},
			Containers: []*runpb.Container{
				{Name: "web", Image: postgresImage, Command: []string{"sh", "-c"}, Args: []string{cloudSQLFileServeScript},
					Ports:        []*runpb.ContainerPort{{ContainerPort: 8080}},
					VolumeMounts: []*runpb.VolumeMount{{Name: "shared", MountPath: "/shared"}}},
				// The instance's MySQL image, by the name the kind node holds it under.
				{Name: "mysql", Image: "docker.io/library/" + imageConst(t, "CloudSQLMySQLImage"), Command: []string{"bash", "-c"},
					Args: []string{cloudSQLMySQLQueryScript},
					Env: []*runpb.EnvVar{{Name: "CONN", Values: &runpb.EnvVar_Value{Value: conn}},
						{Name: "MYSQL_PWD", Values: &runpb.EnvVar_Value{Value: os.Getenv(EnvMySQLPassword)}}},
					VolumeMounts: []*runpb.VolumeMount{{Name: "cloudsql", MountPath: "/cloudsql"}, {Name: "shared", MountPath: "/shared"}}},
			},
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
	cloudSQLSelectOne(t, svc.GetUri(), conn)
}

// cloudSQLAnnotationFormat is describe's format for the v1 annotation.
const cloudSQLAnnotationFormat = `--format=value(spec.template.metadata.annotations."run.googleapis.com/cloudsql-instances")`

// covers: run.namespaces.services.create, run.namespaces.services.get, run.namespaces.services.replaceService
//
// TestGcloudRunDeployCloudSQL (#1346): `gcloud run deploy
// --add-cloudsql-instances` sends Cloud Run v1's
// run.googleapis.com/cloudsql-instances annotation on the revision
// template, which is the socket at /cloudsql/<connection name>. The service
// answers SELECT 1 through it, describe shows the annotation, the official
// v2 client reads it as the cloudsql volume mounted at /cloudsql, the
// console's edit form prefills its Cloud SQL connections field with it, and
// --remove-cloudsql-instances removes it.
func TestGcloudRunDeployCloudSQL(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	conn := runCloudSQLConn(t, h)
	g := newGcloudSession(t, h)
	id := "gcloud-cloudsql"
	region := "--region=us-central1"
	t.Cleanup(func() { _, _ = g.run(nil, "run", "services", "delete", id, region, "--quiet") })
	out, err := g.run(nil, "run", "deploy", id, "--image="+postgresImage, region, "--no-allow-unauthenticated",
		"--command=sh", "--args=^@^-c@"+cloudSQLServeScript, "--set-env-vars=CONN="+conn,
		"--add-cloudsql-instances="+conn, "--quiet", "--log-http")
	if err != nil {
		t.Fatalf("gcloud run deploy --add-cloudsql-instances: %v\n%s", err, out)
	}
	// Which API and form gcloud sent the instances in.
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "uri: ") || strings.HasPrefix(l, "method: ") {
			t.Logf("--log-http: %.200s", l)
		}
	}
	if !strings.Contains(out, `"run.googleapis.com/cloudsql-instances": "`+conn+`"`) {
		t.Errorf("gcloud did not send the v1 annotation")
	}
	if got := g.must("run", "services", "describe", id, region, cloudSQLAnnotationFormat); strings.TrimSpace(got) != conn {
		t.Errorf("describe shows cloudsql-instances %q; want %q", got, conn)
	}
	c := runClient(t, h)
	svc, err := c.GetService(h.Context(), &runpb.GetServiceRequest{Name: runParent(h) + "/services/" + id})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	tmpl := svc.GetTemplate()
	if v := tmpl.GetVolumes(); len(v) != 1 || v[0].GetName() != "cloudsql" ||
		len(v[0].GetCloudSqlInstance().GetInstances()) != 1 || v[0].GetCloudSqlInstance().GetInstances()[0] != conn {
		t.Errorf("v2 reads the volumes as %v", v)
	}
	if m := tmpl.GetContainers()[0].GetVolumeMounts(); len(m) != 1 || m[0].GetMountPath() != "/cloudsql" {
		t.Errorf("v2 reads the mounts as %v", m)
	}
	cloudSQLSelectOne(t, svc.GetUri(), conn)
	// The console's Cloud SQL connections field is the same volume. Its edit
	// form needs arguments with no whitespace, so a plain image.
	if os.Getenv(EnvConsole) != "" {
		plain := id + "-console"
		t.Cleanup(func() { _, _ = g.run(nil, "run", "services", "delete", plain, region, "--quiet") })
		g.must("run", "deploy", plain, "--image="+helloDigest, region, "--no-allow-unauthenticated",
			"--add-cloudsql-instances="+conn, "--quiet")
		if _, values, _ := consoleRunEditForm(t, consoleAddr(t, h), h.Project(), plain); values["cloudSqlInstances"] != conn {
			t.Errorf("the console's edit form prefills Cloud SQL connections with %q; want %q", values["cloudSqlInstances"], conn)
		}
	}

	g.must("run", "services", "update", id, region, "--remove-cloudsql-instances="+conn, "--quiet")
	if got := g.must("run", "services", "describe", id, region, cloudSQLAnnotationFormat); strings.TrimSpace(got) != "" {
		t.Errorf("after --remove-cloudsql-instances, describe shows %q", got)
	}
}
