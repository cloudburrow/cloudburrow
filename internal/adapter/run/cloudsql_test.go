package run

import (
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var testCloudSQL = &CloudSQLConfig{Endpoint: "http://cloudburrow-host.cloudburrow.svc.cluster.local:9020",
	Region: "us-central1", Instances: []string{"postgres", "mysql"}}

func cloudSQLVolume(names ...string) *runpb.Volume {
	return &runpb.Volume{Name: "cloudsql", VolumeType: &runpb.Volume_CloudSqlInstance{
		CloudSqlInstance: &runpb.CloudSqlInstance{Instances: names}}}
}

// A service's Cloud SQL volume is an emptyDir the Auth Proxy, a second
// container running Google's image, writes its sockets into; the serving
// container names its port, as Knative requires beside a sidecar.
func TestCloudSQLVolumeRendersTheAuthProxy(t *testing.T) {
	svc := &runpb.Service{Name: "projects/proj-1/locations/us-central1/services/web", Template: &runpb.RevisionTemplate{
		Volumes: []*runpb.Volume{cloudSQLVolume("proj-1:us-central1:postgres", "proj-1:us-central1:mysql")},
		Containers: []*runpb.Container{{Image: "example.com/app:1",
			VolumeMounts: []*runpb.VolumeMount{{Name: "cloudsql", MountPath: "/cloudsql"}}}}}}
	injected := []injectedEnv{{name: "GCE_METADATA_HOST", value: "cloudburrow-host.cloudburrow.svc.cluster.local:8085"},
		{name: "STORAGE_EMULATOR_HOST", value: "x"}}
	m, err := renderService(svc, "cloudburrow", "inst", nil, injected, testCloudSQL)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- name: cloudsql\n          emptyDir: {}\n",
		"            - containerPort: 8080\n",
		"- name: cloud-sql-proxy-cloudsql\n          image: " + CloudSQLProxyImage,
		`args: ["--unix-socket", "/cloudsql", "--sqladmin-api-endpoint", "http://cloudburrow-host.cloudburrow.svc.cluster.local:9020", "--exit-zero-on-sigterm", "proj-1:us-central1:postgres", "proj-1:us-central1:mysql"]`,
		"- name: GCE_METADATA_HOST\n",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "restartPolicy: Always") {
		t.Errorf("a service's proxy is an ordinary container:\n%s", m)
	}
	if i := strings.Index(m, "cloud-sql-proxy-cloudsql"); strings.Contains(m[i:], "STORAGE_EMULATOR_HOST") {
		t.Errorf("the proxy is given only the metadata server and project:\n%s", m[i:])
	}
}

// A job's proxy is a native sidecar, so the task completes when its own
// container exits.
func TestCloudSQLVolumeInAJobIsANativeSidecar(t *testing.T) {
	job := &runpb.Job{Name: "projects/proj-1/locations/us-central1/jobs/j"}
	tt := &runpb.TaskTemplate{Volumes: []*runpb.Volume{cloudSQLVolume("proj-1:us-central1:postgres")},
		Containers: []*runpb.Container{{Image: "example.com/app:1"}}}
	m, err := renderExecution(job, "j", "j-abc", tt, 1, "cloudburrow", "inst", nil, nil, testCloudSQL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m, "      initContainers:\n        - name: cloud-sql-proxy-cloudsql\n") ||
		!strings.Contains(m, "restartPolicy: Always") {
		t.Errorf("no native sidecar:\n%s", m)
	}
}

func TestCloudSQLVolumeRefusals(t *testing.T) {
	for _, c := range []struct {
		name  string
		names []string
		sql   *CloudSQLConfig
		code  codes.Code
		want  string
	}{
		{"unknown instance", []string{"proj-1:us-central1:db"}, testCloudSQL, codes.InvalidArgument, `Cloud SQL instance "proj-1:us-central1:db" does not exist`},
		{"other region", []string{"proj-1:europe-west1:postgres"}, testCloudSQL, codes.InvalidArgument, "does not exist"},
		{"malformed", []string{"postgres"}, testCloudSQL, codes.InvalidArgument, "project:region:instance"},
		{"none", nil, testCloudSQL, codes.InvalidArgument, "at least one"},
		{"twice", []string{"proj-1:us-central1:postgres", "proj-1:us-central1:postgres"}, testCloudSQL, codes.InvalidArgument, "twice"},
		{"not enabled", []string{"proj-1:us-central1:postgres"}, nil, codes.FailedPrecondition, "is enabled on this instance"},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := &runpb.Service{Name: "projects/proj-1/locations/us-central1/services/web", Template: &runpb.RevisionTemplate{
				Volumes: []*runpb.Volume{cloudSQLVolume(c.names...)}, Containers: []*runpb.Container{{Image: "example.com/app:1"}}}}
			_, err := renderService(svc, "cloudburrow", "inst", nil, nil, c.sql)
			if status.Code(err) != c.code || !strings.Contains(err.Error(), c.want) {
				t.Errorf("= %v; want %v naming %q", err, c.code, c.want)
			}
		})
	}
}
