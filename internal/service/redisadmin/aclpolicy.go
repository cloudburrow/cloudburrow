package redisadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// ACL policies (#1549), google.cloud.redis.cluster.v1's aclPolicies: a
// policy is a list of rules, a username and a Valkey ACL rule each
// ("on >password ~app:* +@read"), attached to clusters by their
// acl_policy. Every rule is a Valkey ACL user on every node of each
// cluster it is attached to (ACL SETUSER <username> reset <rule>), set
// when a node starts and again on every node when the policy is updated
// or attached; users a policy no longer has are deleted (ACL DELUSER).
// Each update is a revision of the policy, which GetAclPolicyRevision and
// ListAclPolicyRevisions return. Memorystore for Valkey has no ACL
// policies in its API.

type aclRule struct {
	Username string `json:"username"`
	Rule     string `json:"rule"`
}

type aclRevision struct {
	Number  int64     `json:"number"`
	Rules   []aclRule `json:"rules"`
	Created time.Time `json:"created"`
}

type aclPolicyRecord struct {
	Name      string        `json:"name"`
	Rules     []aclRule     `json:"rules"`
	Created   time.Time     `json:"created"`
	Updated   time.Time     `json:"updated"`
	Revisions []aclRevision `json:"revisions"`
}

// aclPolicies are a Redis cluster API's ACL policies. Lock order: a
// clusters' mu before pmu, never the other way.
type aclPolicies struct {
	pmu    sync.Mutex
	byName map[string]*aclPolicyRecord
}

const aclPolicyPrefix = "aclpolicies/"

var aclPolicyRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/aclPolicies/([^/]+)$`)
var aclRevisionRE = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/aclPolicies/[^/]+)/revisions/([0-9]+)$`)

func (c *clusters) acl() *aclPolicies {
	c.aclOnce.Do(func() {
		c.policies = &aclPolicies{byName: map[string]*aclPolicyRecord{}}
		if c.db == nil || c.valkey {
			return
		}
		keys, _ := c.db.List(aclPolicyPrefix)
		for _, k := range keys {
			b, err := c.db.Get(k)
			p := &aclPolicyRecord{}
			if err == nil && json.Unmarshal(b, p) == nil {
				c.policies.byName[p.Name] = p
			}
		}
	})
	return c.policies
}

func (c *clusters) savePolicy(p *aclPolicyRecord) error {
	if c.db == nil {
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return c.db.Put(aclPolicyPrefix+clusterKey(p.Name), b)
}

// policyUsers are the ACL users of r's policy. Call with mu held.
func (c *clusters) policyUsers(r *clusterRecord) map[string][]any {
	if r.AclPolicy == "" || c.valkey {
		return nil
	}
	a := c.acl()
	a.pmu.Lock()
	defer a.pmu.Unlock()
	p := a.byName[r.AclPolicy]
	if p == nil {
		return nil
	}
	out := map[string][]any{}
	for _, rl := range p.Rules {
		args := []any{"reset"}
		for _, f := range strings.Fields(rl.Rule) {
			args = append(args, f)
		}
		out[rl.Username] = args
	}
	return out
}

// attachPolicy checks name, a policy in parent's location, or "".
func (c *clusters) attachPolicy(_ context.Context, name, parent string) (string, error) {
	if name == "" {
		return "", nil
	}
	if c.valkey {
		return "", apierror.InvalidArgument("Memorystore for Valkey has no ACL policies")
	}
	if !strings.HasPrefix(name, parent+"/aclPolicies/") {
		return "", apierror.InvalidArgument("acl_policy %q must be a policy of %s", name, parent)
	}
	a := c.acl()
	a.pmu.Lock()
	defer a.pmu.Unlock()
	if a.byName[name] == nil {
		return "", apierror.NotFound("Resource '%s' was not found", name)
	}
	return name, nil
}

// policyInfo is the attached policy's applied revision. Call with mu held.
func (c *clusters) policyInfo(r *clusterRecord) *clusterpb.AclPolicyInfo {
	a := c.acl()
	a.pmu.Lock()
	defer a.pmu.Unlock()
	p := a.byName[r.AclPolicy]
	if p == nil || len(p.Revisions) == 0 {
		return nil
	}
	last := p.Revisions[len(p.Revisions)-1]
	st := clusterpb.AclPolicyRevisionStatus_APPLIED
	if _, pending := c.busy[r.Name]; pending {
		st = clusterpb.AclPolicyRevisionStatus_APPLYING
	}
	rev := fmt.Sprintf("%s/revisions/%d", p.Name, last.Number)
	return &clusterpb.AclPolicyInfo{AppliedAclPolicy: p.Name, AppliedAclPolicyRevision: rev, AppliedAclPolicyRevisionNumber: last.Number,
		AclPolicyRevisionStatuses: []*clusterpb.AclPolicyRevisionStatus{{AclPolicyRevision: rev, AclPolicyRevisionNumber: last.Number, State: st}}}
}

func checkRules(rules []*clusterpb.AclRule) ([]aclRule, error) {
	seen := map[string]bool{}
	var out []aclRule
	for _, rl := range rules {
		if err := checkACLUser("rules.username", rl.GetUsername()); err != nil {
			return nil, err
		}
		if seen[rl.GetUsername()] {
			return nil, apierror.InvalidArgument("rules name user %q twice", rl.GetUsername())
		}
		seen[rl.GetUsername()] = true
		if strings.TrimSpace(rl.GetRule()) == "" {
			return nil, apierror.InvalidArgument("rules.rule of user %q is required", rl.GetUsername())
		}
		out = append(out, aclRule{Username: rl.GetUsername(), Rule: rl.GetRule()})
	}
	return out, nil
}

// renderPolicy is p as Google shapes it, with the clusters attached.
func (c *clusters) renderPolicy(p *aclPolicyRecord, attached []string, state clusterpb.AclPolicy_State) *clusterpb.AclPolicy {
	out := &clusterpb.AclPolicy{Name: p.Name, State: state, CreateTime: timestamppb.New(p.Created), UpdateTime: timestamppb.New(p.Updated),
		Etag: proto.String(strconv.Itoa(len(p.Revisions)))}
	for _, rl := range p.Rules {
		out.Rules = append(out.Rules, &clusterpb.AclRule{Username: rl.Username, Rule: rl.Rule})
	}
	for _, cl := range attached {
		out.ClusterAclPolicyAttachments = append(out.ClusterAclPolicyAttachments, &clusterpb.ClusterAclPolicyAttachment{Cluster: cl})
	}
	return out
}

// attachedTo are the clusters p is attached to. Call with mu held, not pmu.
func (c *clusters) attachedTo(name string) []string {
	var out []string
	for n, r := range c.byName {
		if r.AclPolicy == name {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (s *ClusterServer) policies() (*clusters, *aclPolicies, error) {
	c, err := s.clusters()
	if err != nil {
		return nil, nil, err
	}
	return c, c.acl(), nil
}

// CreateAclPolicy makes a policy, its first revision.
func (s *ClusterServer) CreateAclPolicy(ctx context.Context, req *clusterpb.CreateAclPolicyRequest) (*clusterpb.AclPolicy, error) {
	c, a, err := s.policies()
	if err != nil {
		return nil, err
	}
	if m := parentRE.FindStringSubmatch(req.GetParent()); m == nil || m[2] == "-" {
		return nil, apierror.InvalidArgument("parent must be projects/{project}/locations/{location}, not %q", req.GetParent())
	}
	if !clusterIDRE.MatchString(req.GetAclPolicyId()) {
		return nil, apierror.InvalidArgument("acl_policy_id %q must be 4 to 63 lowercase letters, digits or hyphens, start with a letter and not end with a hyphen", req.GetAclPolicyId())
	}
	rules, err := checkRules(req.GetAclPolicy().GetRules())
	if err != nil {
		return nil, err
	}
	now := c.now()
	p := &aclPolicyRecord{Name: req.GetParent() + "/aclPolicies/" + req.GetAclPolicyId(), Rules: rules, Created: now, Updated: now,
		Revisions: []aclRevision{{Number: 1, Rules: rules, Created: now}}}
	a.pmu.Lock()
	defer a.pmu.Unlock()
	if a.byName[p.Name] != nil {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", p.Name)
	}
	if err := c.savePolicy(p); err != nil {
		return nil, apierror.Internal(err, "keep ACL policy %s", p.Name)
	}
	a.byName[p.Name] = p
	return c.renderPolicy(p, nil, clusterpb.AclPolicy_ACTIVE), nil
}

func (s *ClusterServer) findPolicy(name string) (*clusters, *aclPolicyRecord, []string, error) {
	if aclPolicyRE.FindStringSubmatch(name) == nil {
		return nil, nil, nil, apierror.InvalidArgument("name must be projects/{project}/locations/{location}/aclPolicies/{acl_policy}, not %q", name)
	}
	c, a, err := s.policies()
	if err != nil {
		return nil, nil, nil, err
	}
	c.mu.Lock()
	attached := c.attachedTo(name)
	c.mu.Unlock()
	a.pmu.Lock()
	p := a.byName[name]
	a.pmu.Unlock()
	if p == nil {
		return nil, nil, nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return c, p, attached, nil
}

// GetAclPolicy describes a policy.
func (s *ClusterServer) GetAclPolicy(ctx context.Context, req *clusterpb.GetAclPolicyRequest) (*clusterpb.AclPolicy, error) {
	c, p, attached, err := s.findPolicy(req.GetName())
	if err != nil {
		return nil, err
	}
	a := c.acl()
	a.pmu.Lock()
	defer a.pmu.Unlock()
	return c.renderPolicy(p, attached, clusterpb.AclPolicy_ACTIVE), nil
}

// ListAclPolicies lists a location's policies, in name order and paged.
func (s *ClusterServer) ListAclPolicies(ctx context.Context, req *clusterpb.ListAclPoliciesRequest) (*clusterpb.ListAclPoliciesResponse, error) {
	m := parentRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent must be projects/{project}/locations/{location}, not %q", req.GetParent())
	}
	c, a, err := s.policies()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	attached := map[string][]string{}
	for n, r := range c.byName {
		if r.AclPolicy != "" {
			attached[r.AclPolicy] = append(attached[r.AclPolicy], n)
		}
	}
	c.mu.Unlock()
	a.pmu.Lock()
	defer a.pmu.Unlock()
	var names []string
	for n := range a.byName {
		k := aclPolicyRE.FindStringSubmatch(n)
		if k[1] == m[1] && (m[2] == "-" || k[2] == m[2]) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	page, next, err := paging.Page("acl-policies:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token %q", req.GetPageToken())
	}
	resp := &clusterpb.ListAclPoliciesResponse{NextPageToken: next}
	for _, n := range page {
		at := attached[n]
		sort.Strings(at)
		resp.AclPolicies = append(resp.AclPolicies, c.renderPolicy(a.byName[n], at, clusterpb.AclPolicy_ACTIVE))
	}
	return resp, nil
}

// UpdateAclPolicy replaces a policy's rules, a new revision, and sets them
// on every node of each cluster it is attached to.
func (s *ClusterServer) UpdateAclPolicy(ctx context.Context, req *clusterpb.UpdateAclPolicyRequest) (*longrunningpb.Operation, error) {
	for _, p := range req.GetUpdateMask().GetPaths() {
		if p != "rules" {
			return nil, apierror.InvalidArgument("update_mask path %q is not an updatable field of AclPolicy", p)
		}
	}
	c, p, attached, err := s.findPolicy(req.GetAclPolicy().GetName())
	if err != nil {
		return nil, err
	}
	rules, err := checkRules(req.GetAclPolicy().GetRules())
	if err != nil {
		return nil, err
	}
	if e := req.GetAclPolicy().GetEtag(); e != "" && e != strconv.Itoa(len(p.Revisions)) {
		return nil, apierror.Aborted("etag %q is not the policy's current etag", e)
	}
	c.mu.Lock()
	for _, n := range attached {
		if _, pending := c.busy[n]; pending {
			c.mu.Unlock()
			return nil, apierror.FailedPrecondition("cluster %s, which the policy is attached to, has an operation in progress", n)
		}
	}
	a := c.acl()
	a.pmu.Lock()
	next := *p
	next.Rules, next.Updated = rules, c.now()
	next.Revisions = append(append([]aclRevision(nil), p.Revisions...), aclRevision{Number: int64(len(p.Revisions) + 1), Rules: rules, Created: next.Updated})
	if err := c.savePolicy(&next); err != nil {
		a.pmu.Unlock()
		c.mu.Unlock()
		return nil, apierror.Internal(err, "keep ACL policy %s", p.Name)
	}
	a.byName[p.Name] = &next
	a.pmu.Unlock()
	m := aclPolicyRE.FindStringSubmatch(p.Name)
	ops := c.api.created.ops
	op := ops.Create("projects/"+m[1]+"/locations/"+m[2], p.Name)
	_ = ops.SetMetadata(op.Name, &clusterpb.OperationMetadata{CreateTime: timestamppb.New(c.now()), Target: p.Name, Verb: "update", ApiVersion: "v1"})
	type job struct {
		snap  clusterRecord
		users map[string][]any
		nodes []*clusterNode
	}
	var jobs []job
	for _, n := range attached {
		r := c.byName[n]
		c.busy[n] = op.Name
		jobs = append(jobs, job{*r, c.aclUsers(r), append([]*clusterNode(nil), r.Nodes...)})
	}
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(c.bg, clusterWait)
		defer cancel()
		var err error
		for _, j := range jobs {
			if e := c.syncUsers(ctx, &j.snap, j.users, j.nodes); e != nil && err == nil {
				err = fmt.Errorf("cluster %s: %w", j.snap.Name, e)
			}
		}
		c.mu.Lock()
		for _, n := range attached {
			delete(c.busy, n)
		}
		a.pmu.Lock()
		out := c.renderPolicy(&next, attached, clusterpb.AclPolicy_ACTIVE)
		a.pmu.Unlock()
		c.mu.Unlock()
		c.finish(op.Name, out, err)
	}()
	return s.api.operation(op.Name)
}

// DeleteAclPolicy deletes a policy no cluster is attached to.
func (s *ClusterServer) DeleteAclPolicy(ctx context.Context, req *clusterpb.DeleteAclPolicyRequest) (*longrunningpb.Operation, error) {
	c, p, attached, err := s.findPolicy(req.GetName())
	if err != nil {
		return nil, err
	}
	if len(attached) > 0 {
		return nil, apierror.FailedPrecondition("ACL policy %s is attached to %s: detach it first", p.Name, strings.Join(attached, ", "))
	}
	if e := req.GetEtag(); e != "" && e != strconv.Itoa(len(p.Revisions)) {
		return nil, apierror.Aborted("etag %q is not the policy's current etag", e)
	}
	a := c.acl()
	a.pmu.Lock()
	delete(a.byName, p.Name)
	if c.db != nil {
		_ = c.db.Delete(aclPolicyPrefix + clusterKey(p.Name))
	}
	a.pmu.Unlock()
	m := aclPolicyRE.FindStringSubmatch(p.Name)
	ops := c.api.created.ops
	op := ops.Create("projects/"+m[1]+"/locations/"+m[2], p.Name)
	_ = ops.SetMetadata(op.Name, &clusterpb.OperationMetadata{CreateTime: timestamppb.New(c.now()), Target: p.Name, Verb: "delete", ApiVersion: "v1"})
	c.finish(op.Name, &emptypb.Empty{}, nil)
	return s.api.operation(op.Name)
}

func (c *clusters) renderRevision(p *aclPolicyRecord, rev aclRevision, attached []string) *clusterpb.AclPolicyRevision {
	snap := *p
	snap.Rules = rev.Rules
	out := &clusterpb.AclPolicyRevision{Name: fmt.Sprintf("%s/revisions/%d", p.Name, rev.Number), RevisionNumber: rev.Number,
		Snapshot: c.renderPolicy(&snap, nil, clusterpb.AclPolicy_ACTIVE), CreateTime: timestamppb.New(rev.Created)}
	if rev.Number == int64(len(p.Revisions)) {
		out.AttachedClusters = attached
	}
	return out
}

// GetAclPolicyRevision is one revision of a policy.
func (s *ClusterServer) GetAclPolicyRevision(ctx context.Context, req *clusterpb.GetAclPolicyRevisionRequest) (*clusterpb.AclPolicyRevision, error) {
	m := aclRevisionRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name must be projects/{project}/locations/{location}/aclPolicies/{acl_policy}/revisions/{revision}, not %q", req.GetName())
	}
	c, p, attached, err := s.findPolicy(m[1])
	if err != nil {
		return nil, err
	}
	n, _ := strconv.ParseInt(m[2], 10, 64)
	if n < 1 || n > int64(len(p.Revisions)) {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	return c.renderRevision(p, p.Revisions[n-1], attached), nil
}

// ListAclPolicyRevisions lists a policy's revisions, newest first.
func (s *ClusterServer) ListAclPolicyRevisions(ctx context.Context, req *clusterpb.ListAclPolicyRevisionsRequest) (*clusterpb.ListAclPolicyRevisionsResponse, error) {
	c, p, attached, err := s.findPolicy(req.GetParent())
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(p.Revisions))
	for i := range p.Revisions {
		ids[i] = strconv.Itoa(len(p.Revisions) - i)
	}
	page, next, err := paging.Page("acl-policy-revisions:"+p.Name, ids, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token %q", req.GetPageToken())
	}
	resp := &clusterpb.ListAclPolicyRevisionsResponse{NextPageToken: next}
	for _, id := range page {
		n, _ := strconv.Atoi(id)
		resp.AclPolicyRevisions = append(resp.AclPolicyRevisions, c.renderRevision(p, p.Revisions[n-1], attached))
	}
	return resp, nil
}
