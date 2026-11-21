package run

import (
	"encoding/json"
	"strings"
	"testing"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// TestServiceRecordReadsBack (#2049): creator, last_modifier and
// build_config are written as the service's annotations and read back.
func TestServiceRecordReadsBack(t *testing.T) {
	svc := &runpb.Service{Creator: "a@example.com", LastModifier: "b@example.com",
		BuildConfig: &runpb.BuildConfig{FunctionTarget: "helloHttp", SourceLocation: "gs://b/o.zip",
			BaseImage: BaseImage("us-central1", "nodejs22"), EnableAutomaticUpdates: true}}
	var b strings.Builder
	renderRecord(&b, svc)
	ann := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), ": ")
		var s string
		if err := json.Unmarshal([]byte(v), &s); err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		ann[k] = s
	}
	var got runpb.Service
	readRecord(ann, &got)
	if got.GetCreator() != "a@example.com" || got.GetLastModifier() != "b@example.com" ||
		got.GetBuildConfig().GetFunctionTarget() != "helloHttp" || !got.GetBuildConfig().GetEnableAutomaticUpdates() ||
		RuntimeOfBaseImage(got.GetBuildConfig().GetBaseImage()) != "nodejs22" {
		t.Errorf("read back %v", &got)
	}
	if err := checkBuildConfig(&runpb.BuildConfig{SourceLocation: "https://x"}); err == nil {
		t.Error("a source_location that is not gs:// was accepted")
	}
}

// TestPlaceholderRunsTheGatewaysEnvoy (#2049): Google's placeholder image
// is never rendered as itself, runs the gateway's Envoy by digest once that
// is known, and reads back as Google's image without the Envoy command.
func TestPlaceholderRunsTheGatewaysEnvoy(t *testing.T) {
	placeholder.mu.Lock()
	saved := placeholder.ref
	placeholder.ref = ""
	placeholder.mu.Unlock()
	t.Cleanup(func() { placeholder.mu.Lock(); placeholder.ref = saved; placeholder.mu.Unlock() })
	c := []*runpb.Container{{Image: PlaceholderImage}}
	var b strings.Builder
	if err := renderContainers(&b, c, containerRender{}); err == nil {
		t.Fatal("the placeholder rendered before the gateway's image was known")
	}
	placeholder.mu.Lock()
	placeholder.ref = "docker.io/envoyproxy/envoy@sha256:" + strings.Repeat("a", 64)
	placeholder.mu.Unlock()
	b.Reset()
	if err := renderContainers(&b, c, containerRender{}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "envoyproxy/envoy@sha256:") || !strings.Contains(out, "/usr/local/bin/envoy") ||
		!strings.Contains(out, "--config-yaml") || strings.Contains(out, "us-docker.pkg.dev") {
		t.Errorf("rendered %s", out)
	}
	if built := builtImagesOf(c); built[placeholderRef()] != PlaceholderImage {
		t.Errorf("built images = %v", built)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(placeholderEnvoy(8080)), &cfg); err != nil {
		t.Errorf("the Envoy bootstrap is not JSON: %v", err)
	}
}

// TestFunctionDeployKeepsRunSettings (#2049): a function's deploy keeps
// what Cloud Run set on its service, and records Run's build_config.
func TestFunctionDeployKeepsRunSettings(t *testing.T) {
	fn := &functionspb.Function{Name: "projects/p/locations/us-central1/functions/f",
		BuildConfig: &functionspb.BuildConfig{Runtime: "python312", EntryPoint: "hello_http",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: &functionspb.StorageSource{Bucket: "b", Object: "o.zip"}}}},
		ServiceConfig: &functionspb.ServiceConfig{SecretVolumes: []*functionspb.SecretVolume{{MountPath: "/s", Secret: "k"}}}}
	existing := &runpb.Service{Description: "kept", Template: &runpb.RevisionTemplate{
		ExecutionEnvironment: runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2,
		VpcAccess:            &runpb.VpcAccess{Connector: "projects/p/locations/us-central1/connectors/c"},
		Volumes: []*runpb.Volume{{Name: "mem", VolumeType: &runpb.Volume_EmptyDir{EmptyDir: &runpb.EmptyDirVolumeSource{}}},
			{Name: "secret-volume-0", VolumeType: &runpb.Volume_Secret{Secret: &runpb.SecretVolumeSource{Secret: "old"}}}},
		Containers: []*runpb.Container{{Ports: []*runpb.ContainerPort{{Name: "h2c", ContainerPort: 8080}},
			VolumeMounts: []*runpb.VolumeMount{{Name: "mem", MountPath: "/cache"}, {Name: "secret-volume-0", MountPath: "/old"}}}}}}
	svc := FunctionService(fn, "img")
	keepRunSettings(svc, existing)
	tm := svc.GetTemplate()
	if tm.GetExecutionEnvironment() != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2 || tm.GetVpcAccess().GetConnector() == "" ||
		svc.GetDescription() != "kept" || tm.GetContainers()[0].GetPorts()[0].GetName() != "h2c" {
		t.Errorf("settings not kept: %v", svc)
	}
	var names []string
	for _, v := range tm.GetVolumes() {
		names = append(names, v.GetName())
	}
	if strings.Join(names, ",") != "secret-volume-0,mem" {
		t.Errorf("volumes = %v; want the function's secret volume made again and the in-memory one kept", names)
	}
	if v := tm.GetVolumes()[0].GetSecret().GetSecret(); !strings.HasSuffix(v, "/secrets/k") && v != "k" {
		t.Errorf("the function's secret volume = %q", v)
	}
	bc := svc.GetBuildConfig()
	if bc.GetFunctionTarget() != "hello_http" || bc.GetSourceLocation() != "gs://b/o.zip" || !bc.GetEnableAutomaticUpdates() ||
		bc.GetBaseImage() != "us-central1-docker.pkg.dev/serverless-runtimes/google-22-full/runtimes/python312" {
		t.Errorf("build_config = %v", bc)
	}
	c := cloneFunction(fn)
	defaultUpdatePolicy(c)
	if c.GetBuildConfig().GetAutomaticUpdatePolicy() == nil {
		t.Error("automatic updates are not the default policy")
	}
}
