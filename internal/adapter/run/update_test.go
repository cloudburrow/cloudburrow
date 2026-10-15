package run

import (
	"context"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// applyRunner answers get ksvc with a fixed service and records applies.
type applyRunner struct {
	scriptedRunner
	applied []string
	// calls is every kubectl invocation, so a test can prove one never ran.
	calls []string
}

func (a *applyRunner) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	a.calls = append(a.calls, strings.Join(args, " "))
	if strings.Contains(strings.Join(args, " "), " apply -f -") {
		a.applied = append(a.applied, stdin)
		return "", nil
	}
	return a.scriptedRunner.Run(ctx, stdin, args...)
}

func updateServer() (*Server, *applyRunner) {
	r := &applyRunner{scriptedRunner: scriptedRunner{answers: map[string]string{
		"get ksvc hello": `{"metadata":{"name":"hello","generation":2,"labels":{"cloudburrow.dev/owned":"true"}},
		  "status":{"observedGeneration":1,"latestReadyRevisionName":"hello-00001","latestCreatedRevisionName":"hello-00001"}}`,
	}}}
	return NewServer(&Knative{Kube: k8s.NewWith(r, "k", "", "default")}, "inst", time.Second), r
}

func updated(image, target string) *runpb.Service {
	return &runpb.Service{
		Name: svcParent,
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{
			Image: image, Env: []*runpb.EnvVar{{Name: "TARGET", Values: &runpb.EnvVar_Value{Value: target}}}}}},
	}
}

func TestUpdateServiceAppliesANewTemplate(t *testing.T) {
	s, r := updateServer()
	op, err := s.UpdateService(context.Background(), &runpb.UpdateServiceRequest{
		Service: updated("ghcr.io/knative/helloworld-go@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "v2")})
	if err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	// Not done at once: the new revision is not ready, and Knative has not
	// yet observed generation 2, so the old revision's Ready does not count.
	if op.GetDone() {
		t.Error("the update's operation is done before Knative observed the new generation")
	}
	if len(r.applied) != 1 || !strings.Contains(r.applied[0], "v2") || !strings.Contains(r.applied[0], "helloworld-go@sha256:") {
		t.Errorf("applied = %v", r.applied)
	}
}

func TestUpdateServiceRefusals(t *testing.T) {
	s, r := updateServer()
	ctx := context.Background()
	img := "ghcr.io/knative/helloworld-go@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	if _, err := s.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: updated(img, "v2"),
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"template.nonsense"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("update with an unknown mask path = %v, want InvalidArgument", err)
	}
	for _, path := range []string{"uri", "template.containers.image"} {
		if _, err := s.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: updated(img, "v2"),
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{path}}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("update with mask %q = %v, want InvalidArgument", path, err)
		}
	}
	missing := updated(img, "v2")
	missing.Name = "projects/demo-project/locations/us-central1/services/absent"
	if _, err := s.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: missing}); status.Code(err) != codes.NotFound {
		t.Errorf("update of a missing service = %v, want NotFound", err)
	}
	// Traffic is mapped (#1224); what cannot be routed is the caller's
	// mistake, refused before anything is applied.
	latest := runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST
	revision := runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION
	for _, traffic := range [][]*runpb.TrafficTarget{
		{{Type: latest, Percent: 50}},
		{{Type: revision, Revision: "hello-00009", Percent: 100}},
		{{Type: latest, Percent: 50}, {Type: revision, Revision: "hello-00009", Percent: 50}},
		{{Type: revision, Percent: 100}},
		{{Type: latest, Percent: 100, Tag: "Bad_Tag"}},
	} {
		svc := updated(img, "v2")
		svc.Traffic = traffic
		if _, err := s.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: svc}); status.Code(err) != codes.InvalidArgument ||
			!strings.Contains(err.Error(), "traffic") {
			t.Errorf("update with traffic %v = %v, want InvalidArgument naming traffic", traffic, err)
		}
	}
	// 100% to latest is Knative's default, and is accepted.
	svc := updated(img, "v2")
	svc.Traffic = []*runpb.TrafficTarget{{Type: runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST, Percent: 100}}
	if _, err := s.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: svc}); err != nil {
		t.Errorf("update with 100%% to latest = %v", err)
	}
	if len(r.applied) != 1 {
		t.Errorf("refused updates reached the cluster: %d applies", len(r.applied))
	}
}

// A mask names the fields to change; the rest are kept as read back from
// Knative (#1260).
func TestUpdateServiceWithAMaskChangesOnlyTheNamedFields(t *testing.T) {
	r := &applyRunner{scriptedRunner: scriptedRunner{answers: map[string]string{
		"get ksvc hello": `{"metadata":{"name":"hello","generation":2,"labels":{"cloudburrow.dev/owned":"true"},
		  "annotations":{"cloudburrow.dev/cloud-run-labels":"{\"team\":\"a\"}"}},
		  "spec":{"template":{"metadata":{"annotations":{"autoscaling.knative.dev/max-scale":"3"}},
		    "spec":{"containers":[{"image":"ghcr.io/knative/helloworld-go:v1","command":["/srv"],"args":["--x"],
		      "resources":{"limits":{"memory":"256Mi"}},
		      "env":[{"name":"TARGET","value":"v1"}]}]}}},
		  "status":{"observedGeneration":2,"latestReadyRevisionName":"hello-00001","latestCreatedRevisionName":"hello-00001"}}`,
	}}}
	s := NewServer(&Knative{Kube: k8s.NewWith(r, "k", "", "default")}, "inst", time.Second)
	in := &runpb.Service{Name: svcParent, Labels: map[string]string{"team": "b"},
		Template: &runpb.RevisionTemplate{Scaling: &runpb.RevisionScaling{MaxInstanceCount: 9}}}
	if _, err := s.UpdateService(context.Background(), &runpb.UpdateServiceRequest{Service: in,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}); err != nil {
		t.Fatalf("UpdateService with mask labels: %v", err)
	}
	if len(r.applied) != 1 {
		t.Fatalf("applied = %d manifests, want 1", len(r.applied))
	}
	m := r.applied[0]
	for _, want := range []string{`\"team\":\"b\"`, "helloworld-go:v1", `"/srv"`, `"--x"`, `memory: "256Mi"`,
		`value: "v1"`, `max-scale: "3"`} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %s:\n%s", want, m)
		}
	}
	if strings.Contains(m, `max-scale: "9"`) {
		t.Errorf("template.scaling changed though the mask named only labels:\n%s", m)
	}
}
