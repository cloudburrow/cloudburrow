package redisadmin

import (
	"context"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Memorystore for Valkey's ACL policies (#1578),
// google.cloud.memorystore.v1's aclPolicies: Redis Cluster's (aclpolicy.go)
// over the Valkey instances, an instance's acl_policy attaching one. Each
// rule is a Valkey ACL user on every node of each instance the policy is
// attached to. Requests and answers are converted field by field: the two
// APIs' messages differ only in naming instances where Redis Cluster
// names clusters.

func (s *MemorystoreServer) acls() *aclAPI { return &aclAPI{api: s.api, get: s.instances} }

// renderValkey is r as Google shapes an instance, with its ACL policy.
// Call with mu held.
func (c *clusters) renderValkey(r *clusterRecord) *memorystorepb.Instance {
	out := renderValkeyBase(r)
	if r.AclPolicy != "" {
		out.AclPolicy = proto.String(r.AclPolicy)
		out.AclPolicyInfo = valkeyPolicyInfo(c.policyInfo(r))
	}
	return out
}

func valkeyStatuses(in []*clusterpb.AclPolicyRevisionStatus) []*memorystorepb.AclPolicyRevisionStatus {
	var out []*memorystorepb.AclPolicyRevisionStatus
	for _, st := range in {
		out = append(out, &memorystorepb.AclPolicyRevisionStatus{AclPolicyRevision: st.GetAclPolicyRevision(),
			AclPolicyRevisionNumber: st.GetAclPolicyRevisionNumber(),
			State:                   memorystorepb.AclPolicyRevisionStatus_State(st.GetState()), ErrorMessage: st.GetErrorMessage()})
	}
	return out
}

func valkeyPolicyInfo(in *clusterpb.AclPolicyInfo) *memorystorepb.AclPolicyInfo {
	if in == nil {
		return nil
	}
	return &memorystorepb.AclPolicyInfo{AppliedAclPolicy: in.GetAppliedAclPolicy(), AppliedAclPolicyRevision: in.GetAppliedAclPolicyRevision(),
		AppliedAclPolicyRevisionNumber: in.GetAppliedAclPolicyRevisionNumber(), AclPolicyRevisionStatuses: valkeyStatuses(in.GetAclPolicyRevisionStatuses())}
}

// valkeyPolicy is a policy as google.cloud.memorystore.v1 shapes it.
func valkeyPolicy(in *clusterpb.AclPolicy) *memorystorepb.AclPolicy {
	if in == nil {
		return nil
	}
	out := &memorystorepb.AclPolicy{Name: in.GetName(), State: memorystorepb.AclPolicy_State(in.GetState()), Etag: in.Etag,
		CreateTime: in.GetCreateTime(), UpdateTime: in.GetUpdateTime()}
	for _, rl := range in.GetRules() {
		out.Rules = append(out.Rules, &memorystorepb.AclRule{Username: rl.GetUsername(), Rule: rl.GetRule()})
	}
	for _, at := range in.GetClusterAclPolicyAttachments() {
		out.InstanceAclPolicyAttachments = append(out.InstanceAclPolicyAttachments, &memorystorepb.InstanceAclPolicyAttachment{
			Instance: at.GetCluster(), AclPolicyRevisionStatuses: valkeyStatuses(at.GetAclPolicyRevisionStatuses())})
	}
	return out
}

func valkeyRevision(in *clusterpb.AclPolicyRevision) *memorystorepb.AclPolicyRevision {
	return &memorystorepb.AclPolicyRevision{Name: in.GetName(), RevisionNumber: in.GetRevisionNumber(), Snapshot: valkeyPolicy(in.GetSnapshot()),
		CreateTime: in.GetCreateTime(), AttachedInstances: in.GetAttachedClusters()}
}

// clusterPolicy is a google.cloud.memorystore.v1 policy as aclpolicy.go
// takes it.
func clusterPolicy(in *memorystorepb.AclPolicy) *clusterpb.AclPolicy {
	if in == nil {
		return nil
	}
	out := &clusterpb.AclPolicy{Name: in.GetName(), Etag: in.Etag}
	for _, rl := range in.GetRules() {
		out.Rules = append(out.Rules, &clusterpb.AclRule{Username: rl.GetUsername(), Rule: rl.GetRule()})
	}
	return out
}

// CreateAclPolicy makes a policy, its first revision.
func (s *MemorystoreServer) CreateAclPolicy(ctx context.Context, req *memorystorepb.CreateAclPolicyRequest) (*memorystorepb.AclPolicy, error) {
	if req.GetAclPolicy() == nil {
		return nil, apierror.InvalidArgument("acl_policy is required")
	}
	p, err := s.acls().CreateAclPolicy(ctx, &clusterpb.CreateAclPolicyRequest{Parent: req.GetParent(), AclPolicyId: req.GetAclPolicyId(),
		AclPolicy: clusterPolicy(req.GetAclPolicy()), RequestId: req.GetRequestId()})
	if err != nil {
		return nil, err
	}
	return valkeyPolicy(p), nil
}

// GetAclPolicy describes a policy.
func (s *MemorystoreServer) GetAclPolicy(ctx context.Context, req *memorystorepb.GetAclPolicyRequest) (*memorystorepb.AclPolicy, error) {
	p, err := s.acls().GetAclPolicy(ctx, &clusterpb.GetAclPolicyRequest{Name: req.GetName()})
	if err != nil {
		return nil, err
	}
	return valkeyPolicy(p), nil
}

// ListAclPolicies lists a location's policies, in name order and paged.
func (s *MemorystoreServer) ListAclPolicies(ctx context.Context, req *memorystorepb.ListAclPoliciesRequest) (*memorystorepb.ListAclPoliciesResponse, error) {
	resp, err := s.acls().ListAclPolicies(ctx, &clusterpb.ListAclPoliciesRequest{Parent: req.GetParent(), PageSize: req.GetPageSize(), PageToken: req.GetPageToken()})
	if err != nil {
		return nil, err
	}
	out := &memorystorepb.ListAclPoliciesResponse{NextPageToken: resp.GetNextPageToken()}
	for _, p := range resp.GetAclPolicies() {
		out.AclPolicies = append(out.AclPolicies, valkeyPolicy(p))
	}
	return out, nil
}

// UpdateAclPolicy replaces a policy's rules, a new revision, and sets them
// on every node of each instance it is attached to.
func (s *MemorystoreServer) UpdateAclPolicy(ctx context.Context, req *memorystorepb.UpdateAclPolicyRequest) (*longrunningpb.Operation, error) {
	if req.GetAclPolicy() == nil {
		return nil, apierror.InvalidArgument("acl_policy is required")
	}
	return s.acls().UpdateAclPolicy(ctx, &clusterpb.UpdateAclPolicyRequest{AclPolicy: clusterPolicy(req.GetAclPolicy()),
		UpdateMask: req.GetUpdateMask(), RequestId: req.GetRequestId()})
}

// DeleteAclPolicy deletes a policy no instance is attached to.
func (s *MemorystoreServer) DeleteAclPolicy(ctx context.Context, req *memorystorepb.DeleteAclPolicyRequest) (*longrunningpb.Operation, error) {
	return s.acls().DeleteAclPolicy(ctx, &clusterpb.DeleteAclPolicyRequest{Name: req.GetName(), RequestId: req.GetRequestId(), Etag: req.GetEtag()})
}

// GetAclPolicyRevision is one revision of a policy.
func (s *MemorystoreServer) GetAclPolicyRevision(ctx context.Context, req *memorystorepb.GetAclPolicyRevisionRequest) (*memorystorepb.AclPolicyRevision, error) {
	r, err := s.acls().GetAclPolicyRevision(ctx, &clusterpb.GetAclPolicyRevisionRequest{Name: req.GetName()})
	if err != nil {
		return nil, err
	}
	return valkeyRevision(r), nil
}

// ListAclPolicyRevisions lists a policy's revisions, the newest first.
func (s *MemorystoreServer) ListAclPolicyRevisions(ctx context.Context, req *memorystorepb.ListAclPolicyRevisionsRequest) (*memorystorepb.ListAclPolicyRevisionsResponse, error) {
	resp, err := s.acls().ListAclPolicyRevisions(ctx, &clusterpb.ListAclPolicyRevisionsRequest{Parent: req.GetParent(), PageSize: req.GetPageSize(), PageToken: req.GetPageToken()})
	if err != nil {
		return nil, err
	}
	out := &memorystorepb.ListAclPolicyRevisionsResponse{NextPageToken: resp.GetNextPageToken()}
	for _, r := range resp.GetAclPolicyRevisions() {
		out.AclPolicyRevisions = append(out.AclPolicyRevisions, valkeyRevision(r))
	}
	return out, nil
}
