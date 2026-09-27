package run

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
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

// An invalid name reaching the cluster comes back as an apply failure,
// reported as Internal — which says the fault is ours when it is the
// caller's, and buries the actual rule.
func TestServiceIDIsValidatedBeforeAnythingIsApplied(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		"", "BadName", "-leading", "trailing-", "has_underscore", "has.dot",
		strings.Repeat("a", 50),
	} {
		err := ValidateServiceID(bad)
		if err == nil {
			t.Errorf("ValidateServiceID(%q) accepted it", bad)
			continue
		}
		if code := apierror.From(err).Code; code != codes.InvalidArgument {
			t.Errorf("ValidateServiceID(%q) = %s, want InvalidArgument", bad, code)
		}
	}

	for _, ok := range []string{"a", "my-service", "svc1", strings.Repeat("a", 49)} {
		if err := ValidateServiceID(ok); err != nil {
			t.Errorf("ValidateServiceID(%q) = %v", ok, err)
		}
	}
}

// A rejection is the caller's fault and a failure to apply is ours; reporting
// both the same way makes the distinction useless.
func TestClusterRejectionIsInvalidArgumentNotInternal(t *testing.T) {
	t.Parallel()
	for _, message := range []string{
		`Service "x" is invalid: metadata.name: Invalid value`,
		"admission webhook denied the request",
		"validation failed: spec.template",
	} {
		if !isRejection(message) {
			t.Errorf("%q was not recognised as a rejection", message)
		}
	}
	for _, message := range []string{
		"connection refused", "the server could not find the requested resource",
	} {
		if isRejection(message) {
			t.Errorf("%q was treated as a rejection", message)
		}
	}
}

// TestTimeoutReachesKnative.
//
// Cloud Run's request timeout is Knative's timeoutSeconds. It was being
// dropped, so a service deployed with a ten-minute timeout got Knative's
// default and failed at five with nothing on screen to explain it.
func TestTimeoutReachesKnative(t *testing.T) {
	yaml, err := ToKnative(&runpb.Service{
		Name: parent + "/services/api",
		Template: &runpb.RevisionTemplate{
			Containers: []*runpb.Container{{Image: "example.com/api:v1"}},
			Timeout:    durationpb.New(600 * time.Second),
		},
	}, "cloudburrow", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(yaml, "timeoutSeconds: 600") {
		t.Fatalf("timeout did not reach the manifest:\n%s", yaml)
	}
}

// TestUnmappedTemplateFieldsAreRefused.
//
// Both were dropped in silence. A caller who asked for a second-generation
// execution environment and saw a successful create would believe they got one.
func TestUnmappedTemplateFieldsAreRefused(t *testing.T) {
	for name, tmpl := range map[string]*runpb.RevisionTemplate{
		"executionEnvironment": {
			Containers:           []*runpb.Container{{Image: "example.com/api:v1"}},
			ExecutionEnvironment: runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2,
		},
		"sessionAffinity": {
			Containers:      []*runpb.Container{{Image: "example.com/api:v1"}},
			SessionAffinity: true,
		},
	} {
		err := Unsupported(&runpb.Service{Name: parent + "/services/api", Template: tmpl})
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s: error does not name the field: %v", name, err)
		}
	}
}

// ksvcStore is a kubectl that keeps applied Knative Services, read back
// from the rendered manifest, so a create is followed by a GetService that
// returns what the cluster would hold.
type ksvcStore struct {
	mu      sync.Mutex
	rv      int
	applied map[string]string // name -> JSON
}

func (s *ksvcStore) Run(_ context.Context, stdin string, args ...string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	args = args[4:] // --kubeconfig k -n ns
	switch {
	case args[0] == "apply":
		s.rv++
		name, obj := ksvcFromManifest(stdin, s.rv)
		b, err := json.Marshal(obj)
		if err != nil {
			return "", err
		}
		s.applied[name] = string(b)
		return "", nil
	case args[0] == "get" && args[1] == "ksvc" && args[2] == "-l":
		items := make([]string, 0, len(s.applied))
		for _, j := range s.applied {
			items = append(items, j)
		}
		return `{"items":[` + strings.Join(items, ",") + `]}`, nil
	case args[0] == "get" && args[1] == "ksvc":
		if j, ok := s.applied[args[2]]; ok {
			return j, nil
		}
	}
	return "", errors.New("NotFound")
}

// ksvcFromManifest reads the parts of a rendered Knative Service that
// FromKnative reports on env: the name, the template's annotations, and
// each container's image and env.
func ksvcFromManifest(m string, rv int) (string, map[string]any) {
	var name string
	ann := map[string]string{}
	var containers []map[string]any
	var env []map[string]any
	section := ""
	flush := func() {
		if len(containers) > 0 {
			containers[len(containers)-1]["env"] = env
		}
		env = nil
	}
	for _, line := range strings.Split(m, "\n") {
		unq := func(v string) string {
			if u, err := strconv.Unquote(v); err == nil {
				return u
			}
			return v
		}
		switch {
		case strings.HasPrefix(line, "  name: ") && name == "":
			name = strings.TrimPrefix(line, "  name: ")
		case line == "      annotations:":
			section = "annotations"
		case line == "    spec:":
			section = "spec"
		case section == "annotations" && strings.HasPrefix(line, "        "):
			k, v, _ := strings.Cut(strings.TrimSpace(line), ": ")
			ann[k] = unq(v)
		case strings.HasPrefix(line, "        - image: "):
			flush()
			containers = append(containers, map[string]any{"image": strings.TrimPrefix(line, "        - image: ")})
		case strings.HasPrefix(line, "            - name: "):
			env = append(env, map[string]any{"name": strings.TrimPrefix(line, "            - name: ")})
		case strings.HasPrefix(line, "              value: "):
			env[len(env)-1]["value"] = unq(strings.TrimPrefix(line, "              value: "))
		case strings.HasPrefix(line, "              valueFrom:"):
			env[len(env)-1]["valueFrom"] = map[string]any{}
		}
	}
	flush()
	return name, map[string]any{
		"metadata": map[string]any{"name": name, "resourceVersion": strconv.Itoa(rv), "generation": rv,
			"labels": map[string]string{"cloudburrow.dev/owned": "true"}},
		"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{"annotations": ann},
			"spec":     map[string]any{"containers": containers}}},
	}
}

func envOf(t *testing.T, svc *runpb.Service) []*runpb.EnvVar {
	t.Helper()
	if len(svc.GetTemplate().GetContainers()) != 1 {
		t.Fatalf("service has %d containers, want 1", len(svc.GetTemplate().GetContainers()))
	}
	return svc.GetTemplate().GetContainers()[0].GetEnv()
}

func envNames(env []*runpb.EnvVar) []string {
	var out []string
	for _, e := range env {
		out = append(out, e.GetName()+"="+e.GetValue())
	}
	return out
}

// #576: every revision's pod is given the in-cluster endpoints and its
// project, after the caller's variables and never over one: a caller-set
// STORAGE_EMULATOR_HOST is kept. The Cloud Run API shows exactly the
// caller's env — GetService, ListServices and GetRevision leave the
// injected variables out, as Cloud Run leaves out K_SERVICE and PORT — so a
// declarative client such as Terraform sees no drift.
func TestGetServiceShowsOnlyTheCallersEnvAndThePodHasBoth(t *testing.T) {
	store := &ksvcStore{applied: map[string]string{}}
	pubsub := "pubsub.cloudburrow.svc.cluster.local:8085"
	s := NewServer(&Knative{Kube: k8s.NewWith(store, "k", "", "default")}, "inst", time.Second).
		WithEnvironment(func() map[string]string {
			return map[string]string{
				"STORAGE_EMULATOR_HOST": "http://storage.cloudburrow.svc.cluster.local:4443",
				"PUBSUB_EMULATOR_HOST":  pubsub,
				"GCE_METADATA_HOST":     "cloudburrow-host.cloudburrow.svc.cluster.local:9004",
				// Not yet published: an empty address is never injected.
				"CLOUDBURROW_TASKS_ENDPOINT": "",
			}
		})
	ctx := context.Background()
	c := &runpb.Container{Image: "example.com/app:v1", Env: []*runpb.EnvVar{
		{Name: "TARGET", Values: &runpb.EnvVar_Value{Value: "world"}},
		{Name: "STORAGE_EMULATOR_HOST", Values: &runpb.EnvVar_Value{Value: "http://mine:1"}},
	}}
	if _, err := s.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: "app",
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{c}}}}); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	pod := func() []string {
		t.Helper()
		var k ksvc
		if err := json.Unmarshal([]byte(store.applied["app"]), &k); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range k.Spec.Template.Spec.Containers[0].Env {
			out = append(out, e.Name+"="+e.Value)
		}
		return out
	}
	want := []string{
		"STORAGE_EMULATOR_HOST=http://mine:1",
		"TARGET=world",
		"GCE_METADATA_HOST=cloudburrow-host.cloudburrow.svc.cluster.local:9004",
		"GOOGLE_CLOUD_PROJECT=my-project",
		"PUBSUB_EMULATOR_HOST=" + pubsub,
	}
	if g := pod(); !slices.Equal(g, want) {
		t.Errorf("the pod's env =\n  %v\nwant\n  %v", g, want)
	}
	callers := []string{"STORAGE_EMULATOR_HOST=http://mine:1", "TARGET=world"}
	got, err := s.GetService(ctx, &runpb.GetServiceRequest{Name: parent + "/services/app"})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if g := envNames(envOf(t, got)); !slices.Equal(g, callers) {
		t.Errorf("GetService env = %v, want only the caller's %v", g, callers)
	}
	list, err := s.ListServices(ctx, &runpb.ListServicesRequest{Parent: parent})
	if err != nil || len(list.GetServices()) != 1 {
		t.Fatalf("ListServices = %v, %v", list, err)
	}
	if g := envNames(envOf(t, list.GetServices()[0])); !slices.Equal(g, callers) {
		t.Errorf("ListServices env = %v, want only the caller's %v", g, callers)
	}

	// A read-modify-write sends back only the caller's env, so nothing
	// injected is frozen: the Pub/Sub address that moved is the new one in
	// the pod. A variable the caller now sets under an injected name, to
	// the injected value, is the caller's and reads back.
	pubsub = "pubsub.cloudburrow.svc.cluster.local:9085"
	got.Etag = ""
	got.Template.Containers[0].Env = append(got.Template.Containers[0].Env, &runpb.EnvVar{
		Name: "GCE_METADATA_HOST", Values: &runpb.EnvVar_Value{Value: "cloudburrow-host.cloudburrow.svc.cluster.local:9004"}})
	if _, err := s.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: got}); err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	want = []string{
		"GCE_METADATA_HOST=cloudburrow-host.cloudburrow.svc.cluster.local:9004",
		"STORAGE_EMULATOR_HOST=http://mine:1",
		"TARGET=world",
		"GOOGLE_CLOUD_PROJECT=my-project",
		"PUBSUB_EMULATOR_HOST=pubsub.cloudburrow.svc.cluster.local:9085",
	}
	if g := pod(); !slices.Equal(g, want) {
		t.Errorf("the pod's env after an update =\n  %v\nwant\n  %v", g, want)
	}
	after, err := s.GetService(ctx, &runpb.GetServiceRequest{Name: parent + "/services/app"})
	if err != nil {
		t.Fatal(err)
	}
	callers = []string{"GCE_METADATA_HOST=cloudburrow-host.cloudburrow.svc.cluster.local:9004",
		"STORAGE_EMULATOR_HOST=http://mine:1", "TARGET=world"}
	if g := envNames(envOf(t, after)); !slices.Equal(g, callers) {
		t.Errorf("GetService env after an update = %v, want the caller's %v", g, callers)
	}
}

// GetRevision reads a Knative Revision, which carries its template's
// annotations: the injected variables are left out there too.
func TestRevisionShowsOnlyTheCallersEnv(t *testing.T) {
	var r krev
	if err := json.Unmarshal([]byte(`{"metadata": {"name": "app-00001", "annotations": {
		"cloudburrow.dev/injected-env": "{\"GOOGLE_CLOUD_PROJECT\":\"my-project\"}"}},
		"spec": {"containers": [{"image": "example.com/app:v1", "env": [
			{"name": "TARGET", "value": "world"}, {"name": "GOOGLE_CLOUD_PROJECT", "value": "my-project"}]}]}}`), &r); err != nil {
		t.Fatal(err)
	}
	rev := FromKnativeRevision(r, parent+"/services/app")
	if g := envNames(rev.GetContainers()[0].GetEnv()); !slices.Equal(g, []string{"TARGET=world"}) {
		t.Errorf("revision env = %v, want only TARGET", g)
	}
}

// A job's tasks are given the same environment, after Cloud Run's own task
// variables, and a caller's variable of the same name still wins.
func TestExecutionsAreGivenTheInjectedEnvironment(t *testing.T) {
	spec := &runpb.Job{Name: jobParent + "/jobs/nightly"}
	tt := &runpb.TaskTemplate{Containers: []*runpb.Container{{Image: "example.com/job:v1", Env: []*runpb.EnvVar{
		{Name: "PUBSUB_EMULATOR_HOST", Values: &runpb.EnvVar_Value{Value: "mine:1"}}}}}}
	s := NewServer(&Knative{Kube: k8s.NewWith(newFakeKube(), "k", "", "default")}, "inst", time.Second).
		WithEnvironment(func() map[string]string {
			return map[string]string{"PUBSUB_EMULATOR_HOST": "pubsub.cloudburrow.svc.cluster.local:8085",
				"CLOUDBURROW_SECRETMANAGER_ENDPOINT": "cloudburrow-host.cloudburrow.svc.cluster.local:9003"}
		})
	m, err := renderExecution(spec, "nightly", "nightly-abc", tt, 1, "default", "inst", nil,
		s.injectedFor(projectOf(spec.GetName())))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- name: PUBSUB_EMULATOR_HOST\n              value: \"mine:1\"",
		"- name: CLOUD_RUN_TASK_COUNT\n              value: \"1\"\n" +
			"            - name: CLOUDBURROW_SECRETMANAGER_ENDPOINT\n              value: \"cloudburrow-host.cloudburrow.svc.cluster.local:9003\"\n" +
			"            - name: GOOGLE_CLOUD_PROJECT\n              value: \"demo-project\"\n",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("the batch Job lacks %q:\n%s", want, m)
		}
	}
	if strings.Count(m, "- name: PUBSUB_EMULATOR_HOST") != 1 {
		t.Errorf("PUBSUB_EMULATOR_HOST is rendered more than once, or the caller's lost:\n%s", m)
	}
}
