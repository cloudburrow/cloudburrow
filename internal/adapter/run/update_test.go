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
)

// applyRunner answers get ksvc with a fixed service and records applies.
type applyRunner struct {
	scriptedRunner
	applied []string
}

func (a *applyRunner) Run(ctx context.Context, stdin, name string, args ...string) (string, error) {
	if strings.Contains(strings.Join(args, " "), " apply -f -") {
		a.applied = append(a.applied, stdin)
		return "", nil
	}
	return a.scriptedRunner.Run(ctx, stdin, name, args...)
}

func updateServer() (*Server, *applyRunner) {
	r := &applyRunner{scriptedRunner: scriptedRunner{answers: map[string]string{
		"get ksvc hello": `{"metadata":{"name":"hello","generation":2,"labels":{"cloudburrow.dev/owned":"true"}},
		  "status":{"observedGeneration":1,"latestReadyRevisionName":"hello-00001","latestCreatedRevisionName":"hello-00001"}}`,
	}}}
	return NewServer(&Knative{Kubeconfig: "k", Namespace: "default", Runner: r}, "inst", time.Second), r
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
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"template.containers"}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("update with a mask = %v, want Unimplemented", err)
	}
	missing := updated(img, "v2")
	missing.Name = "projects/demo-project/locations/us-central1/services/absent"
	if _, err := s.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: missing}); status.Code(err) != codes.NotFound {
		t.Errorf("update of a missing service = %v, want NotFound", err)
	}
	for _, traffic := range [][]*runpb.TrafficTarget{
		{{Type: runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST, Percent: 50}},
		{{Type: runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION, Revision: "hello-00001", Percent: 100}},
		{{Type: runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST, Percent: 50},
			{Type: runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION, Revision: "hello-00001", Percent: 50}},
	} {
		svc := updated(img, "v2")
		svc.Traffic = traffic
		if _, err := s.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: svc}); status.Code(err) != codes.Unimplemented ||
			!strings.Contains(err.Error(), "traffic") {
			t.Errorf("update with traffic %v = %v, want Unimplemented naming traffic", traffic, err)
		}
	}
	// 100% to latest is what the adapter does anyway, so it is accepted.
	svc := updated(img, "v2")
	svc.Traffic = []*runpb.TrafficTarget{{Type: runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST, Percent: 100}}
	if _, err := s.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: svc}); err != nil {
		t.Errorf("update with 100%% to latest = %v", err)
	}
	if len(r.applied) != 1 {
		t.Errorf("refused updates reached the cluster: %d applies", len(r.applied))
	}
}
