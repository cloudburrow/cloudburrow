package run

import (
	"errors"
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const parent = "projects/my-project/locations/us-central1"

func svc(name string, c *runpb.Container) *runpb.Service {
	return &runpb.Service{
		Name:     parent + "/services/" + name,
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{c}},
	}
}

// A locally built image must be rewritten so Knative does not try to resolve
// it against a registry, and must never be pulled.
func TestImageIsLocalisedAndNotPulled(t *testing.T) {
	t.Parallel()
	m, err := ToKnative(svc("app", &runpb.Container{Image: "myapp:v1"}), "cloudburrow", "test", nil)
	if err != nil {
		t.Fatalf("ToKnative: %v", err)
	}
	if !strings.Contains(m, "image: dev.local/myapp:v1") {
		t.Errorf("image was not localised:\n%s", m)
	}
	if !strings.Contains(m, "imagePullPolicy: Never") {
		t.Errorf("a local image must never be pulled:\n%s", m)
	}
}

// A real registry reference is left alone and may be pulled.
func TestRemoteImageIsLeftAlone(t *testing.T) {
	t.Parallel()
	m, err := ToKnative(svc("app", &runpb.Container{Image: "gcr.io/p/app:v1"}), "cloudburrow", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m, "image: gcr.io/p/app:v1") {
		t.Errorf("remote image was rewritten:\n%s", m)
	}
	if !strings.Contains(m, "imagePullPolicy: IfNotPresent") {
		t.Errorf("remote image should be pullable:\n%s", m)
	}
}

// An untagged image is a mutable target and must be refused.
func TestUntaggedImageIsRefused(t *testing.T) {
	t.Parallel()
	_, err := ToKnative(svc("app", &runpb.Container{Image: "myapp"}), "cloudburrow", "test", nil)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("untagged image = %v, want InvalidArgument", status.Code(err))
	}
}

// Configuration we do not map must be reported, never silently dropped: a
// caller who set it would otherwise believe it took effect.
func TestUnsupportedConfigurationIsReported(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*runpb.Service)
		want   string
	}{
		{"service account", func(s *runpb.Service) {
			s.Template.ServiceAccount = "sa@project.iam.gserviceaccount.com"
		}, "serviceAccount"},
		{"vpc access", func(s *runpb.Service) {
			s.Template.VpcAccess = &runpb.VpcAccess{Connector: "conn"}
		}, "vpcAccess"},
		{"volumes", func(s *runpb.Service) {
			s.Template.Volumes = []*runpb.Volume{{Name: "v"}}
		}, "volumes"},
		{"encryption key", func(s *runpb.Service) {
			s.Template.EncryptionKey = "projects/p/locations/l/keyRings/r/cryptoKeys/k"
		}, "encryptionKey"},
		{"traffic splitting", func(s *runpb.Service) {
			s.Traffic = []*runpb.TrafficTarget{{Percent: 50}, {Percent: 50}}
		}, "traffic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := svc("app", &runpb.Container{Image: "gcr.io/p/app:v1"})
			tt.mutate(s)
			_, err := ToKnative(s, "cloudburrow", "test", nil)
			if status.Code(err) != codes.Unimplemented {
				t.Fatalf("%s = %v, want Unimplemented", tt.name, status.Code(err))
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should name %q, got: %v", tt.want, err)
			}
		})
	}
}

// Secret-backed environment needs Secret Manager, which does not exist here.
func TestSecretEnvIsReported(t *testing.T) {
	t.Parallel()
	s := svc("app", &runpb.Container{
		Image: "gcr.io/p/app:v1",
		Env: []*runpb.EnvVar{{
			Name:   "SECRET",
			Values: &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{}},
		}},
	})
	_, err := ToKnative(s, "cloudburrow", "test", nil)
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("secret env = %v, want Unimplemented", status.Code(err))
	}
}

func TestScalingMapsToKnativeAnnotations(t *testing.T) {
	t.Parallel()
	s := svc("app", &runpb.Container{Image: "gcr.io/p/app:v1"})
	s.Template.Scaling = &runpb.RevisionScaling{MinInstanceCount: 1, MaxInstanceCount: 7}
	s.Template.MaxInstanceRequestConcurrency = 42

	m, err := ToKnative(s, "cloudburrow", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`autoscaling.knative.dev/min-scale: "1"`,
		`autoscaling.knative.dev/max-scale: "7"`,
		"containerConcurrency: 42",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q:\n%s", want, m)
		}
	}
}

// Ownership labels must be present, or cleanup could not tell our services
// from ones a developer created by hand.
func TestOwnershipLabelsArePresent(t *testing.T) {
	t.Parallel()
	m, err := ToKnative(svc("app", &runpb.Container{Image: "gcr.io/p/app:v1"}), "cloudburrow", "inst-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`cloudburrow.dev/owned: "true"`,
		`cloudburrow.dev/instance: "inst-1"`,
		`cloudburrow.dev/cloud-run-name: "` + parent + `/services/app"`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q:\n%s", want, m)
		}
	}
}

// Environment order must be deterministic, or the same Service would render
// differently between calls and churn the revision.
func TestEnvOrderIsDeterministic(t *testing.T) {
	t.Parallel()
	s := svc("app", &runpb.Container{
		Image: "gcr.io/p/app:v1",
		Env: []*runpb.EnvVar{
			{Name: "ZULU", Values: &runpb.EnvVar_Value{Value: "z"}},
			{Name: "ALPHA", Values: &runpb.EnvVar_Value{Value: "a"}},
		},
	})
	first, err := ToKnative(s, "cloudburrow", "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(first, "ALPHA") > strings.Index(first, "ZULU") {
		t.Error("env is not sorted; the same Service would render differently between calls")
	}
	for i := 0; i < 5; i++ {
		again, _ := ToKnative(s, "cloudburrow", "t", nil)
		if again != first {
			t.Fatal("ToKnative is not deterministic")
		}
	}
}

func TestServiceIDRejectsBadNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "services/x", parent + "/queues/q", parent + "/services/.."} {
		if _, err := ServiceID(name); status.Code(err) != codes.InvalidArgument {
			t.Errorf("ServiceID(%q) = %v, want InvalidArgument", name, status.Code(err))
		}
	}
}

// Readiness must be reported truthfully, including the failure reason: a
// caller that only saw a URL would treat a failed revision as merely slow.
func TestFromKnativeReportsRealReadiness(t *testing.T) {
	t.Parallel()

	var failed ksvc
	failed.Metadata.Name = "app"
	failed.Status.Conditions = []ksvcCondition{
		{Type: "Ready", Status: "False", Reason: "RevisionFailed", Message: "Unable to fetch image"},
	}

	got := FromKnative(failed, parent)
	if got.GetTerminalCondition().GetState() != runpb.Condition_CONDITION_FAILED {
		t.Errorf("state = %v, want FAILED", got.GetTerminalCondition().GetState())
	}
	if !strings.Contains(got.GetTerminalCondition().GetMessage(), "Unable to fetch image") {
		t.Errorf("failure reason was lost: %q", got.GetTerminalCondition().GetMessage())
	}

	var ok ksvc
	ok.Metadata.Name = "app"
	ok.Status.URL = "http://app.default.example"
	ok.Status.Conditions = failed.Status.Conditions
	ok.Status.Conditions[0].Status = "True"
	if s := FromKnative(ok, parent); s.GetTerminalCondition().GetState() != runpb.Condition_CONDITION_SUCCEEDED {
		t.Errorf("ready service state = %v, want SUCCEEDED", s.GetTerminalCondition().GetState())
	}
	if s := FromKnative(ok, parent); s.GetUri() != "http://app.default.example" {
		t.Errorf("uri = %q", s.GetUri())
	}
}

// A service still reconciling is pending, not failed.
func TestUnknownConditionIsPending(t *testing.T) {
	t.Parallel()
	var k ksvc
	k.Metadata.Name = "app"
	k.Status.Conditions = []ksvcCondition{{Type: "Ready", Status: "Unknown"}}
	if got := FromKnative(k, parent).GetTerminalCondition().GetState(); got != runpb.Condition_CONDITION_PENDING {
		t.Errorf("state = %v, want PENDING while reconciling", got)
	}
}

// A revision that fails to start must report the container's own output. The
// top-level Ready condition says only "does not have any ready Revision",
// which tells a developer nothing about what to fix.
func TestFailedRevisionReportsTheContainerOutput(t *testing.T) {
	t.Parallel()
	var k ksvc
	k.Metadata.Name = "broken"
	k.Metadata.Namespace = "default"
	k.Status.Conditions = []ksvcCondition{
		{
			Type: "ConfigurationsReady", Status: "False", Reason: "RevisionFailed",
			Message: `Revision "broken-00001" failed with message: Container failed with: FAIL_STARTUP is set.`,
		},
		{
			Type: "Ready", Status: "False", Reason: "RevisionMissing",
			Message: `Configuration "broken" does not have any ready Revision.`,
		},
	}

	ready, msg := k.Ready()
	if ready {
		t.Fatal("a failed revision was reported ready")
	}
	if !strings.Contains(msg, "Container failed with") {
		t.Errorf("failure message = %q, want the container's own output", msg)
	}
}

// fakeResolver stands in for Secret Manager.
type fakeResolver struct {
	err error
}

func (f fakeResolver) ResolveSecretRef(project, secret, version string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return "cb-secret-" + project + "-" + secret, "v" + version, nil
}

func TestSecretKeyRefRendersAValueFrom(t *testing.T) {
	t.Parallel()
	s := svc("app", &runpb.Container{
		Image: "gcr.io/p/app:v1",
		Env: []*runpb.EnvVar{{
			Name: "API_KEY",
			Values: &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{
				SecretKeyRef: &runpb.SecretKeySelector{Secret: "api-key", Version: "3"},
			}},
		}},
	})
	// A valid GCP project ID is 6-30 characters; the resource parser enforces
	// that, so a short stand-in would fail before reaching the resolver.
	s.Name = "projects/demo-project/locations/us-central1/services/app"

	m, err := ToKnative(s, "cloudburrow", "test", fakeResolver{})
	if err != nil {
		t.Fatalf("ToKnative: %v", err)
	}
	for _, want := range []string{
		"- name: API_KEY",
		"valueFrom:",
		"secretKeyRef:",
		"name: cb-secret-demo-project-api-key",
		"key: v3",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest is missing %q:\n%s", want, m)
		}
	}
	// The payload must not appear in the manifest: the whole point is that
	// the pod reads it from the cluster, not that we template it in.
	if strings.Contains(m, "value: ") && strings.Contains(m, "API_KEY") &&
		strings.Contains(m, "\n              value:") {
		t.Errorf("a secret env var was rendered as a literal value:\n%s", m)
	}
}

// A container started without an environment variable it asked for fails
// somewhere far from the cause, so a reference must be refused up front.
func TestSecretKeyRefIsRefusedWithoutSecretManager(t *testing.T) {
	t.Parallel()
	s := svc("app", &runpb.Container{
		Image: "gcr.io/p/app:v1",
		Env: []*runpb.EnvVar{{
			Name: "API_KEY",
			Values: &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{
				SecretKeyRef: &runpb.SecretKeySelector{Secret: "api-key"},
			}},
		}},
	})

	_, err := ToKnative(s, "cloudburrow", "test", nil)
	if err == nil {
		t.Fatal("a secret reference was accepted with no resolver")
	}
	if !strings.Contains(err.Error(), "Secret Manager is not enabled") {
		t.Errorf("error should say why: %v", err)
	}
}

func TestSecretKeyRefPropagatesAResolverFailure(t *testing.T) {
	t.Parallel()
	s := svc("app", &runpb.Container{
		Image: "gcr.io/p/app:v1",
		Env: []*runpb.EnvVar{{
			Name: "API_KEY",
			Values: &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{
				SecretKeyRef: &runpb.SecretKeySelector{Secret: "api-key"},
			}},
		}},
	})

	_, err := ToKnative(s, "cloudburrow", "test", fakeResolver{err: errors.New("version is DISABLED")})
	if err == nil {
		t.Fatal("a resolver failure was swallowed")
	}
	if !strings.Contains(err.Error(), "DISABLED") {
		t.Errorf("the cause was lost: %v", err)
	}
}
