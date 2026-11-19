package run

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// customSource holds custom constraints enforced everywhere.
type customSource []orgpolicy.CustomConstraint

func (customSource) Effective(context.Context, string, string) (orgpolicy.Effective, error) {
	return orgpolicy.Effective{}, nil
}

func (s customSource) Customs(_ context.Context, _, resourceType, _ string) ([]orgpolicy.CustomConstraint, error) {
	if resourceType != orgpolicy.TypeRunService {
		return nil, nil
	}
	return s, nil
}

// TestCustomConstraintsOnRunServices (#1891): the Cloud Run page's
// examples, read over the service's v1 shape: ALLOW ingress internal
// refuses a service open to all, DENY a container on a public image.
func TestCustomConstraintsOnRunServices(t *testing.T) {
	on := orgpolicy.AsResponse(orgpolicy.Effective{Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{
		{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}}}}})
	orgpolicy.Use(customSource{
		{Name: "custom.ingressInternal", ActionType: "ALLOW", Description: "Require ingress to be set to internal.", Effective: on,
			Condition: "resource.metadata.annotations['run.googleapis.com/ingress'] == 'internal'"},
		{Name: "custom.noDockerHub", ActionType: "DENY", Description: "No Docker Hub images.", Effective: on,
			Condition: "resource.spec.template.spec.containers.exists(c, c.image.startsWith('docker.io/'))"},
	})
	t.Cleanup(func() { orgpolicy.Use(nil) })
	ctx := context.Background()
	svc := func(ingress runpb.IngressTraffic, image string) *runpb.Service {
		return &runpb.Service{Name: "projects/p/locations/us-central1/services/s", Ingress: ingress,
			Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: image}}}}
	}
	if err := checkServiceCustom(ctx, svc(runpb.IngressTraffic_INGRESS_TRAFFIC_INTERNAL_ONLY, "us-docker.pkg.dev/p/r/i"), orgpolicy.MethodCreate); err != nil {
		t.Errorf("an internal service: %v", err)
	}
	err := checkServiceCustom(ctx, svc(runpb.IngressTraffic_INGRESS_TRAFFIC_ALL, "us-docker.pkg.dev/p/r/i"), orgpolicy.MethodCreate)
	want := `Operation denied by custom org policies: ["customConstraints/custom.ingressInternal":"Require ingress to be set to internal."]`
	if apierror.From(err).Code != codes.FailedPrecondition || !strings.Contains(err.Error(), want) {
		t.Errorf("a service open to all = %v, want FAILED_PRECONDITION %s", err, want)
	}
	err = checkServiceCustom(ctx, svc(runpb.IngressTraffic_INGRESS_TRAFFIC_INTERNAL_ONLY, "docker.io/library/nginx"), orgpolicy.MethodUpdate)
	if err == nil || !strings.Contains(err.Error(), "custom.noDockerHub") {
		t.Errorf("a Docker Hub image = %v", err)
	}
}
