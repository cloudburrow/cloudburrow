package run

import (
	"context"
	"errors"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/run/apiv2/runpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Organization policies (#1890), enforced whatever --iam-enforcement, as on
// Google Cloud: gcp.resourceLocations on creating a resource in
// projects/{p}/locations/{l}, and iam.allowedPolicyMemberDomains on
// setIamPolicy. Nothing is refused when no policy is set (or no source is
// in use, as in unit tests).

// checkOrgLocation refuses creating a resource under parent
// (projects/{p}/locations/{l}) in a location gcp.resourceLocations does
// not allow, FAILED_PRECONDITION with Google's documented message.
func checkOrgLocation(ctx context.Context, parent string) error {
	project, location, err := resource.ParseLocation(parent)
	if err != nil {
		return nil
	}
	if err := orgpolicy.CheckLocations(ctx, "projects/"+project, "projects/"+project, location); err != nil {
		return apierror.FailedPrecondition("%s", err.Error())
	}
	return nil
}

// policyMembers are p's members, across its bindings.
func policyMembers(p *iampb.Policy) []string {
	var out []string
	for _, b := range p.GetBindings() {
		out = append(out, b.GetMembers()...)
	}
	return out
}

// storedProto is cur as a policy, nil for none.
func storedProto(cur *iampolicy.Stored) *iampb.Policy {
	if cur == nil {
		return nil
	}
	return cur.Proto()
}

// checkOrgMembers refuses a setIamPolicy on name that adds a member
// iam.allowedPolicyMemberDomains does not permit, FAILED_PRECONDITION
// with Google's message.
func checkOrgMembers(ctx context.Context, name string, before, after *iampb.Policy) error {
	if err := orgpolicy.CheckMembers(ctx, orgpolicy.ProjectOf(name), policyMembers(before), policyMembers(after)); err != nil {
		return apierror.FailedPrecondition("%s", err.Error())
	}
	return nil
}

// Custom constraints on run.googleapis.com/Service (#1891), CREATE and
// UPDATE, over v2 and v1 (which creates and replaces through v2): the
// condition reads the service in "the Cloud Run Admin API v1 field
// specifications" (resource.metadata.annotations,
// resource.spec.template.spec.containers...), the page's examples ("Use
// custom organization policies", Cloud Run, read 2026-11-19), with the
// annotations its examples read, run.googleapis.com/ingress,
// launch-stage, binary-authorization and default-url-disabled, from the
// v2 fields. A violation is FAILED_PRECONDITION "Operation denied by
// custom org policies: [...]".
func checkServiceCustom(ctx context.Context, svc *runpb.Service, method string) error {
	project := orgpolicy.ProjectOf(svc.GetName())
	if project == "" || !orgpolicy.SourceSet() {
		return nil
	}
	v := toV1Service(svc, ksvc{})
	v.Status = nil
	ann := map[string]string{}
	for k, val := range v.Metadata.Annotations {
		ann[k] = val
	}
	switch svc.GetIngress() {
	case runpb.IngressTraffic_INGRESS_TRAFFIC_ALL:
		ann["run.googleapis.com/ingress"] = "all"
	case runpb.IngressTraffic_INGRESS_TRAFFIC_INTERNAL_ONLY:
		ann["run.googleapis.com/ingress"] = "internal"
	case runpb.IngressTraffic_INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER:
		ann["run.googleapis.com/ingress"] = "internal-and-cloud-load-balancing"
	case runpb.IngressTraffic_INGRESS_TRAFFIC_NONE:
		ann["run.googleapis.com/ingress"] = "none"
	}
	if ls := svc.GetLaunchStage(); ls != 0 {
		ann["run.googleapis.com/launch-stage"] = ls.String()
	}
	if ba := svc.GetBinaryAuthorization(); ba.GetUseDefault() {
		ann["run.googleapis.com/binary-authorization"] = "default"
	} else if ba.GetPolicy() != "" {
		ann["run.googleapis.com/binary-authorization"] = ba.GetPolicy()
	}
	if svc.GetDefaultUriDisabled() {
		ann["run.googleapis.com/default-url-disabled"] = "true"
	}
	v.Metadata.Annotations = ann
	err := orgpolicy.CheckCustom(ctx, project, orgpolicy.TypeRunService, method, v)
	var cv *orgpolicy.CustomViolation
	if errors.As(err, &cv) {
		return apierror.FailedPrecondition("%s", cv.Error())
	}
	return nil
}
