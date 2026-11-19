package spannerfront

import (
	"context"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// Organization policies (#1890), asked of the host (--resourcemanager),
// whatever --iam-enforcement: an instance is created only in a location
// gcp.resourceLocations allows, its configuration's region
// (regional-us-central1 is us-central1) or multi-region (nam4, eur3,
// which the constraint's value groups list), and setIamPolicy refuses a
// member iam.allowedPolicyMemberDomains does not permit. Nothing is
// refused when no policy is set.

// configLocation is the location instance configuration name (
// projects/{p}/instanceConfigs/{id}) is in, "" for one with none (the
// emulator's own).
func (s *Instances) configLocation(name string) string {
	id := name[strings.LastIndex(name, "/")+1:]
	if region, ok := strings.CutPrefix(id, "regional-"); ok {
		return region
	}
	if _, ok := multiRegions[id]; ok {
		return id
	}
	s.mu.Lock()
	c := s.configs[name]
	s.mu.Unlock()
	if c != nil && c.GetBaseConfig() != "" && c.GetBaseConfig() != name {
		return s.configLocation(c.GetBaseConfig())
	}
	return ""
}

// CreateInstance refuses an instance whose configuration's location
// gcp.resourceLocations does not allow, FAILED_PRECONDITION with Google's
// documented message, and passes any other to the emulator.
func (s *Instances) CreateInstance(ctx context.Context, req *instancepb.CreateInstanceRequest) (*longrunningpb.Operation, error) {
	if loc := s.configLocation(req.GetInstance().GetConfig()); loc != "" {
		if err := orgpolicy.CheckLocations(ctx, req.GetParent(), req.GetParent(), loc); err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
	}
	inst := &instancepb.Instance{}
	if req.GetInstance() != nil {
		inst = proto.Clone(req.GetInstance()).(*instancepb.Instance)
	}
	inst.Name = req.GetParent() + "/instances/" + req.GetInstanceId()
	if err := checkCustom(ctx, inst.GetName(), orgpolicy.TypeSpannerInstance, orgpolicy.MethodCreate, inst); err != nil {
		return nil, err
	}
	return s.emu.CreateInstance(ctx, req)
}

func policyMembers(p *iampb.Policy) []string {
	var out []string
	for _, b := range p.GetBindings() {
		out = append(out, b.GetMembers()...)
	}
	return out
}

// checkOrgMembers refuses a setIamPolicy adding a member
// iam.allowedPolicyMemberDomains does not permit.
func checkOrgMembers(ctx context.Context, resource string, before, after *iampb.Policy) error {
	if err := orgpolicy.CheckMembers(ctx, orgpolicy.ProjectOf(resource), policyMembers(before), policyMembers(after)); err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return nil
}
