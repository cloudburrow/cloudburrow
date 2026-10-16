package run

import (
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runv1 "google.golang.org/api/run/v1"
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

// v1's run.googleapis.com/cloudsql-instances annotation, as gcloud run
// deploy --add-cloudsql-instances writes it, is the Cloud SQL volume named
// cloudsql mounted at /cloudsql in every container, and reads back as the
// annotation, with no v1 volume or mount for it.
func TestV1CloudSQLAnnotationIsTheVolume(t *testing.T) {
	const conns = "p:us-central1:postgres, p:us-central1:mysql"
	in := &runv1.Service{Metadata: &runv1.ObjectMeta{Name: "web"}, Spec: &runv1.ServiceSpec{Template: &runv1.RevisionTemplate{
		Metadata: &runv1.ObjectMeta{Annotations: map[string]string{v1CloudSQLInstances: conns}},
		Spec:     &runv1.RevisionSpec{Containers: []*runv1.Container{{Image: "example.com/app:1"}}}}}}
	svc, err := fromV1Service(in, "projects/p/locations/us-central1/services/web")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := svc.GetTemplate()
	if v := tmpl.GetVolumes(); len(v) != 1 || v[0].GetName() != "cloudsql" ||
		strings.Join(v[0].GetCloudSqlInstance().GetInstances(), ",") != "p:us-central1:postgres,p:us-central1:mysql" {
		t.Errorf("volumes = %v", v)
	}
	if m := tmpl.GetContainers()[0].GetVolumeMounts(); len(m) != 1 || m[0].GetName() != "cloudsql" || m[0].GetMountPath() != "/cloudsql" {
		t.Errorf("mounts = %v", m)
	}
	if len(tmpl.GetAnnotations()) != 0 {
		t.Errorf("the annotation is kept as well: %v", tmpl.GetAnnotations())
	}
	out := toV1Template(svc)
	if got := out.Metadata.Annotations[v1CloudSQLInstances]; got != "p:us-central1:postgres,p:us-central1:mysql" {
		t.Errorf("annotation read back as %q", got)
	}
	if len(out.Spec.Volumes) != 0 || len(out.Spec.Containers[0].VolumeMounts) != 0 {
		t.Errorf("v1 shows a volume or mount for it: %v %v", out.Spec.Volumes, out.Spec.Containers[0].VolumeMounts)
	}

	// A v2 volume at another path keeps its layout through a v1 replace
	// that names the same instances, and is dropped when they are removed.
	prior := &runpb.RevisionTemplate{Volumes: []*runpb.Volume{{Name: "db", VolumeType: &runpb.Volume_CloudSqlInstance{
		CloudSqlInstance: &runpb.CloudSqlInstance{Instances: []string{"p:us-central1:mysql", "p:us-central1:postgres"}}}}},
		Containers: []*runpb.Container{{VolumeMounts: []*runpb.VolumeMount{{Name: "db", MountPath: "/sql"}}}}}
	keepCloudSQLLayout(tmpl, prior)
	if v, m := tmpl.GetVolumes(), tmpl.GetContainers()[0].GetVolumeMounts(); len(v) != 1 || v[0].GetName() != "db" ||
		len(m) != 1 || m[0].GetMountPath() != "/sql" {
		t.Errorf("layout after replace = %v %v", v, m)
	}
	in.Spec.Template.Metadata.Annotations = nil
	svc, err = fromV1Service(in, svc.GetName())
	if err != nil {
		t.Fatal(err)
	}
	keepCloudSQLLayout(svc.GetTemplate(), prior)
	if len(svc.GetTemplate().GetVolumes()) != 0 {
		t.Errorf("removing the annotation kept %v", svc.GetTemplate().GetVolumes())
	}

	// A volume already named cloudsql is refused.
	in.Spec.Template.Metadata.Annotations = map[string]string{v1CloudSQLInstances: "p:us-central1:postgres"}
	in.Spec.Template.Spec.Volumes = []*runv1.Volume{{Name: "cloudsql", EmptyDir: &runv1.EmptyDirVolumeSource{}}}
	if _, err := fromV1Service(in, svc.GetName()); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a cloudsql volume beside the annotation = %v", err)
	}
}

// A v1 job's execution template annotation is the task's volume.
func TestV1JobCloudSQLAnnotation(t *testing.T) {
	in := &runv1.Job{Spec: &runv1.JobSpec{Template: &runv1.ExecutionTemplateSpec{
		Metadata: &runv1.ObjectMeta{Annotations: map[string]string{v1CloudSQLInstances: "p:us-central1:postgres"}},
		Spec: &runv1.ExecutionSpec{Template: &runv1.TaskTemplateSpec{Spec: &runv1.TaskSpec{
			Containers: []*runv1.Container{{Image: "example.com/app:1"}}}}}}}}
	job, err := fromV1Job(in)
	if err != nil {
		t.Fatal(err)
	}
	tt := job.GetTemplate().GetTemplate()
	if len(tt.GetVolumes()) != 1 || tt.GetContainers()[0].GetVolumeMounts()[0].GetMountPath() != "/cloudsql" {
		t.Fatalf("task template = %v", tt)
	}
	job.Name = "projects/p/locations/us-central1/jobs/j"
	if got := toV1Job(job).Spec.Template.Metadata.Annotations[v1CloudSQLInstances]; got != "p:us-central1:postgres" {
		t.Errorf("annotation read back as %q", got)
	}
}
