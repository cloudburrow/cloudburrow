package run

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func fullService() *runpb.Service {
	return &runpb.Service{Name: svcParent, Description: `the "hello" app`,
		Labels: map[string]string{"team": "a"}, Annotations: map[string]string{"example.com/owner": "z"},
		Template: &runpb.RevisionTemplate{Labels: map[string]string{"rev": "1"}, Annotations: map[string]string{"example.com/note": "b"},
			Scaling: &runpb.RevisionScaling{MinInstanceCount: 1, MaxInstanceCount: 3},
			Containers: []*runpb.Container{{Image: "ghcr.io/x/y:v1", WorkingDir: "/app",
				Ports: []*runpb.ContainerPort{{ContainerPort: 8080}},
				StartupProbe: &runpb.Probe{PeriodSeconds: 2, FailureThreshold: 30, ProbeType: &runpb.Probe_HttpGet{HttpGet: &runpb.HTTPGetAction{
					Path: "/healthz", HttpHeaders: []*runpb.HTTPHeader{{Name: "X-A", Value: "1"}}}}},
				LivenessProbe: &runpb.Probe{TimeoutSeconds: 3, ProbeType: &runpb.Probe_Grpc{Grpc: &runpb.GRPCAction{Service: "health"}}},
				Env:           []*runpb.EnvVar{{Name: "A", Values: &runpb.EnvVar_Value{Value: "b"}}}}}}}
}

// Every populated field ToKnative does not render is refused by name
// rather than dropped (#581).
func TestDroppedFieldsAreRefusedByName(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*runpb.Service){
		"ingress":                  func(s *runpb.Service) { s.Ingress = runpb.IngressTraffic_INGRESS_TRAFFIC_INTERNAL_ONLY },
		"customAudiences":          func(s *runpb.Service) { s.CustomAudiences = []string{"x"} },
		"scaling":                  func(s *runpb.Service) { s.Scaling = &runpb.ServiceScaling{MinInstanceCount: 2} },
		"iapEnabled":               func(s *runpb.Service) { s.IapEnabled = true },
		"template.revision":        func(s *runpb.Service) { s.Template.Revision = "hello-v2" },
		"template.nodeSelector":    func(s *runpb.Service) { s.Template.NodeSelector = &runpb.NodeSelector{Accelerator: "nvidia-l4"} },
		"container.dependsOn":      func(s *runpb.Service) { s.Template.Containers[0].DependsOn = []string{"db"} },
		"container.readinessProbe": func(s *runpb.Service) { s.Template.Containers[0].ReadinessProbe = &runpb.Probe{} },
		"container.startupProbe port": func(s *runpb.Service) {
			s.Template.Containers[0].StartupProbe = &runpb.Probe{ProbeType: &runpb.Probe_TcpSocket{TcpSocket: &runpb.TCPSocketAction{Port: 9090}}}
		},
	}
	for field, mutate := range cases {
		svc := fullService()
		mutate(svc)
		_, err := ToKnative(svc, "default", "inst", nil)
		if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: %v; want Unimplemented naming it", field, err)
		}
	}
	svc := fullService()
	svc.Ingress = runpb.IngressTraffic_INGRESS_TRAFFIC_ALL
	if _, err := ToKnative(svc, "default", "inst", nil); err != nil {
		t.Errorf("ingress ALL is what the adapter does, and is accepted: %v", err)
	}
}

// Keys in the namespaces Cloud Run v2 reserves, and the adapter's own, are
// refused as Google refuses them.
func TestReservedMetadataKeysAreRefused(t *testing.T) {
	t.Parallel()
	for _, mutate := range []func(*runpb.Service){
		func(s *runpb.Service) { s.Labels["run.googleapis.com/x"] = "1" },
		func(s *runpb.Service) { s.Annotations["autoscaling.knative.dev/min-scale"] = "9" },
		func(s *runpb.Service) { s.Template.Annotations["cloudburrow.dev/managed"] = "false" },
		func(s *runpb.Service) { s.Template.Labels["serving.knative.dev/service"] = "other" },
	} {
		svc := fullService()
		mutate(svc)
		if _, err := ToKnative(svc, "default", "inst", nil); status.Code(err) != codes.InvalidArgument {
			t.Errorf("a reserved key = %v; want InvalidArgument", err)
		}
	}
}

// What ToKnative renders reads back through FromKnative as it was set:
// probes, working directory, labels, annotations, description, scaling and a
// secretKeyRef env var. The annotations are taken from the rendered manifest
// itself, so the two sides cannot drift.
func TestNewFieldsRoundTrip(t *testing.T) {
	t.Parallel()
	svc := fullService()
	svc.Template.Containers[0].Env = append(svc.Template.Containers[0].Env, &runpb.EnvVar{Name: "TOKEN",
		Values: &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{SecretKeyRef: &runpb.SecretKeySelector{Secret: "api-token", Version: "2"}}}})
	m, err := ToKnative(svc, "default", "inst", fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`workingDir: "/app"`, "startupProbe:\n            httpGet:\n              port: 8080\n              path: \"/healthz\"",
		"livenessProbe:\n            grpc:\n              port: 8080\n              service: \"health\"", "timeoutSeconds: 3", "failureThreshold: 30"} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	ann := func(key string) string {
		t.Helper()
		match := regexp.MustCompile(regexp.QuoteMeta(key) + `: ("(?:[^"\\]|\\.)*")`).FindStringSubmatch(m)
		if match == nil {
			t.Fatalf("manifest has no %s:\n%s", key, m)
		}
		v, err := strconv.Unquote(match[1])
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	k := ksvc{}
	k.Metadata.Name, k.Metadata.UID, k.Metadata.ResourceVersion = "hello", "u-1", "42"
	k.Metadata.CreationTimestamp = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	k.Metadata.Annotations = map[string]string{"cloudburrow.dev/cloud-run-name": svcParent,
		annServiceLabels: ann(annServiceLabels), annServiceAnnotations: ann(annServiceAnnotations), annDescription: ann(annDescription)}
	k.Spec.Template.Metadata.Annotations = map[string]string{annTemplateLabels: ann(annTemplateLabels),
		annTemplateAnnotation: ann(annTemplateAnnotation), annSecretEnv: ann(annSecretEnv),
		"autoscaling.knative.dev/min-scale": "1", "autoscaling.knative.dev/max-scale": "3"}
	// The container as kubectl returns it for this manifest.
	container := `{"image":"ghcr.io/x/y:v1","workingDir":"/app",
	  "startupProbe":{"httpGet":{"path":"/healthz","httpHeaders":[{"name":"X-A","value":"1"}]},"periodSeconds":2,"failureThreshold":30},
	  "livenessProbe":{"grpc":{"port":8080,"service":"health"},"timeoutSeconds":3},
	  "env":[{"name":"A","value":"b"},{"name":"TOKEN","valueFrom":{"secretKeyRef":{"name":"k8s-secret","key":"2"}}}]}`
	if err := json.Unmarshal([]byte(`{"spec":{"template":{"spec":{"containers":[`+container+`]}}}}`), &k); err != nil {
		t.Fatal(err)
	}
	// Unmarshal over k reset the template annotations it did not mention;
	// set them again.
	k.Spec.Template.Metadata.Annotations = map[string]string{annTemplateLabels: ann(annTemplateLabels),
		annTemplateAnnotation: ann(annTemplateAnnotation), annSecretEnv: ann(annSecretEnv),
		"autoscaling.knative.dev/min-scale": "1", "autoscaling.knative.dev/max-scale": "3"}

	got := FromKnative(k, "projects/demo-project/locations/us-central1")
	if got.GetEtag() != "42" || got.GetUid() != "u-1" || got.GetCreateTime() == nil || got.GetUpdateTime() == nil {
		t.Errorf("etag %q uid %q create %v update %v; want all set", got.GetEtag(), got.GetUid(), got.GetCreateTime(), got.GetUpdateTime())
	}
	if got.GetDescription() != svc.GetDescription() || got.GetLabels()["team"] != "a" || got.GetAnnotations()["example.com/owner"] != "z" {
		t.Errorf("service metadata read back as %q %v %v", got.GetDescription(), got.GetLabels(), got.GetAnnotations())
	}
	tm := got.GetTemplate()
	if tm.GetLabels()["rev"] != "1" || tm.GetAnnotations()["example.com/note"] != "b" {
		t.Errorf("template metadata read back as %v %v", tm.GetLabels(), tm.GetAnnotations())
	}
	if tm.GetScaling().GetMinInstanceCount() != 1 || tm.GetScaling().GetMaxInstanceCount() != 3 {
		t.Errorf("scaling read back as %v", tm.GetScaling())
	}
	c := tm.GetContainers()[0]
	if c.GetWorkingDir() != "/app" || c.GetStartupProbe().GetHttpGet().GetPath() != "/healthz" ||
		c.GetStartupProbe().GetFailureThreshold() != 30 || c.GetLivenessProbe().GetGrpc().GetService() != "health" {
		t.Errorf("container read back as %v", c)
	}
	var token *runpb.EnvVar
	for _, e := range c.GetEnv() {
		if e.GetName() == "TOKEN" {
			token = e
		}
	}
	if ref := token.GetValueSource().GetSecretKeyRef(); ref.GetSecret() != "api-token" || ref.GetVersion() != "2" {
		t.Errorf("TOKEN read back as %v; want the Secret Manager reference that was set", token)
	}
}

type fakeSecrets struct{}

func (fakeSecrets) ResolveSecretRef(_, secret, version string) (string, string, error) {
	return "k8s-secret", version, nil
}

// validate_only applies nothing on create, update-with-allow-missing and
// delete; a stale etag is ABORTED on update and delete (#581).
func TestValidateOnlyAndEtags(t *testing.T) {
	t.Parallel()
	s, r := updateServer()
	ctx := context.Background()
	svc := updated("ghcr.io/x/y:v2", "t")

	op, err := s.CreateService(ctx, &runpb.CreateServiceRequest{Parent: "projects/demo-project/locations/us-central1",
		ServiceId: "fresh", Service: &runpb.Service{Template: svc.Template}, ValidateOnly: true})
	if err != nil || !op.GetDone() {
		t.Fatalf("validate-only create = %v %v; want a done operation", op, err)
	}
	if len(r.applied) != 0 {
		t.Errorf("a validate-only create applied %d manifests", len(r.applied))
	}
	missing := &runpb.Service{Name: "projects/demo-project/locations/us-central1/services/absent", Template: svc.Template}
	if _, err := s.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: missing, AllowMissing: true, ValidateOnly: true}); err != nil {
		t.Fatal(err)
	}
	if len(r.applied) != 0 {
		t.Errorf("a validate-only update of a missing service applied %d manifests", len(r.applied))
	}
	if _, err := s.DeleteService(ctx, &runpb.DeleteServiceRequest{Name: svcParent, ValidateOnly: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls {
		if strings.Contains(c, " delete ") {
			t.Errorf("a validate-only delete ran %q", c)
		}
	}
	stale := updated("ghcr.io/x/y:v2", "t")
	stale.Etag = `"999"`
	if _, err := s.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: stale}); status.Code(err) != codes.Aborted {
		t.Errorf("update with a stale etag = %v; want Aborted", err)
	}
	if _, err := s.DeleteService(ctx, &runpb.DeleteServiceRequest{Name: svcParent, Etag: `"999"`}); status.Code(err) != codes.Aborted {
		t.Errorf("delete with a stale etag = %v; want Aborted", err)
	}
}
