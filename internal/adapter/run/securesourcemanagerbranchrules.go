package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A repository's branch rules (#1856), as Google's "Configure branch
// protection" page describes them: a rule's branch filter
// (include_pattern) is one branch's name or `.*` for every branch, full
// regular expressions not being supported; each enabled filter is unique
// in the repository; a rule naming the branch takes precedence over the
// `.*` rule, and the two are not combined. Require a pull request before
// merging blocks direct pushes to the branch (a pre-receive hook refuses
// them), and cannot be used with `.*`. The rest is checked when a pull
// request is merged: linear history (a fast-forward, securesourcemanagerpulls.go);
// the required numbers of approving reviewers and approvers, conversation
// resolution (securesourcemanagerreviews.go) and required status checks
// (the pull request's trigger builds, securesourcemanagerpullevents.go),
// #1927, and code owner approval, from the base branch's CODEOWNERS files
// (securesourcemanagercodeowners.go, #1928).

var ssmRuleIDRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validBranchFilter checks a rule's include_pattern.
func validBranchFilter(ctx context.Context, p string) error {
	if p == ".*" {
		return nil
	}
	if p == "" || exec.CommandContext(ctx, "git", "check-ref-format", "--branch", p).Run() != nil || strings.ContainsAny(p, `*?[]()|+^$\`) {
		return apierror.InvalidArgument("invalid include_pattern %q: a branch name, or .* for every branch (full regular expressions are not supported)", p)
	}
	return nil
}

func checkBranchRule(ctx context.Context, r *ssmpb.BranchRule) error {
	if err := validBranchFilter(ctx, r.GetIncludePattern()); err != nil {
		return err
	}
	if r.GetRequirePullRequest() && r.GetIncludePattern() == ".*" {
		return apierror.InvalidArgument("require_pull_request cannot be used with the .* branch filter")
	}
	if r.GetMinimumReviewsCount() < 0 || r.GetMinimumApprovalsCount() < 0 {
		return apierror.InvalidArgument("minimum_reviews_count and minimum_approvals_count must not be negative")
	}
	for _, c := range r.GetRequiredStatusChecks() {
		if strings.TrimSpace(c.GetContext()) == "" {
			return apierror.InvalidArgument("required_status_checks[].context is required")
		}
	}
	return nil
}

// duplicateFilterLocked refuses a second enabled rule with one filter.
func duplicateFilterLocked(repo string, r *ssmpb.BranchRule) error {
	if r.GetDisabled() {
		return nil
	}
	for name, o := range ssmState.rules {
		if name != r.GetName() && ssmRepoOf(name) == repo && !o.GetDisabled() && o.GetIncludePattern() == r.GetIncludePattern() {
			return apierror.AlreadyExists("branch rule %q already has the enabled branch filter %q", lastSegment(name), r.GetIncludePattern())
		}
	}
	return nil
}

// writeProtectedLocked writes the branches the pre-receive hook refuses
// direct pushes to, and the branch filters whose rules require code owner
// approval, whose CODEOWNERS files it checks.
func writeProtectedLocked(repo string) error {
	_, dir, err := ssmRepoLocked(repo)
	if err != nil {
		return err
	}
	var names, owners []string
	for name, r := range ssmState.rules {
		if ssmRepoOf(name) != repo || r.GetDisabled() {
			continue
		}
		if r.GetRequirePullRequest() {
			names = append(names, r.GetIncludePattern())
		}
		owners = append(owners, r.GetIncludePattern()+"\t"+strconv.FormatBool(r.GetRequireCodeOwnerApproval()))
	}
	sort.Strings(names)
	sort.Strings(owners)
	if err := installSSMPreReceive(dir); err != nil { // repositories made before #1856 have none
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ssmCodeOwnersBranchesFile), []byte(strings.Join(owners, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ssmProtectedFile), []byte(strings.Join(names, "\n")+"\n"), 0o600)
}

// effectiveBranchRuleLocked is the enabled rule that applies to a branch:
// the one naming it, else the .* one, else nil.
func effectiveBranchRuleLocked(repo, branch string) *ssmpb.BranchRule {
	var wild *ssmpb.BranchRule
	for name, r := range ssmState.rules {
		if ssmRepoOf(name) != repo || r.GetDisabled() {
			continue
		}
		if r.GetIncludePattern() == branch {
			return r
		}
		if r.GetIncludePattern() == ".*" {
			wild = r
		}
	}
	return wild
}

// ruleAllowsMergeLocked refuses a merge of a pull request at head into
// branch that its branch rule does not allow yet (#1927): too few
// approving reviewers or approvers, unresolved conversations or change
// requests, required status checks that have not passed, or (#1928) code
// owners who have not approved, or CODEOWNERS files the merge would bring
// with lines that do not parse. base is the base branch's commit.
func ruleAllowsMergeLocked(ctx context.Context, dir string, r *ssmpb.BranchRule, pr *ssmpb.PullRequest, base, head, branch string) error {
	if r == nil {
		return nil
	}
	pull := pr.GetName()
	id := lastSegment(r.GetName())
	st := reviewStateLocked(pull, head, r.GetAllowStaleReviews())
	if n := r.GetMinimumReviewsCount(); n > 0 && int32(st.reviewers) < n {
		return apierror.FailedPrecondition("branch rule %q on %s requires %d approving reviewers of %s, and it has %d", id, branch, n, shortSHA(head), st.reviewers)
	}
	if n := r.GetMinimumApprovalsCount(); n > 0 && int32(st.approvers) < n {
		return apierror.FailedPrecondition("branch rule %q on %s requires %d approvals of %s, and it has %d", id, branch, n, shortSHA(head), st.approvers)
	}
	if r.GetRequireCodeOwnerApproval() {
		from := base
		if mb, err := ssmGit(ctx, dir, "merge-base", base, head); err == nil {
			from = strings.TrimSpace(string(mb))
		}
		if bad := codeOwnersPresubmit(ctx, dir, from, head); len(bad) > 0 {
			return apierror.FailedPrecondition("branch rule %q on %s requires code owner review, and the pull request's CODEOWNERS has a syntax error: %s", id, branch, strings.Join(bad, "; "))
		}
		changed := prChangedFiles(ctx, dir, pr)
		co, err := codeOwnerStatus(ctx, dir, branch, base, changed, approvedEmailsLocked(pull, head, r.GetAllowStaleReviews()))
		if err != nil {
			return apierror.Internal(err, "CODEOWNERS")
		}
		for _, q := range co.Requirements {
			if q.Satisfied {
				continue
			}
			sec := ""
			if q.Section != "" {
				sec = " in section [" + q.Section + "]"
			}
			return apierror.FailedPrecondition("branch rule %q on %s requires code owner approval of %s: %d approval(s) of %s are needed%s, and %d approved %s",
				id, branch, strings.Join(q.Files, ", "), q.Approvals, strings.Join(q.Owners, ", "), sec, len(q.ApprovedBy), shortSHA(head))
		}
	}
	if r.GetRequireCommentsResolved() {
		if st.unresolved > 0 {
			return apierror.FailedPrecondition("branch rule %q on %s requires conversation resolution: %d conversations are unresolved", id, branch, st.unresolved)
		}
		if len(st.changesRequested) > 0 {
			return apierror.FailedPrecondition("branch rule %q on %s requires conversation resolution: changes were requested by %s", id, branch, strings.Join(st.changesRequested, ", "))
		}
	}
	return statusChecksLocked(r, pull, head, branch)
}

// CreateBranchRule adds a branch rule to a repository.
func (m *SecureSourceManagerServer) CreateBranchRule(ctx context.Context, req *ssmpb.CreateBranchRuleRequest) (*longrunningpb.Operation, error) {
	if !ssmRuleIDRE.MatchString(req.GetBranchRuleId()) {
		return nil, apierror.InvalidArgument("invalid branch_rule_id %q: lower case letters, digits and hyphens, starting with a letter and ending with a letter or digit, at most 63", req.GetBranchRuleId())
	}
	r := proto.Clone(req.GetBranchRule()).(*ssmpb.BranchRule)
	if r == nil {
		r = &ssmpb.BranchRule{}
	}
	if err := checkBranchRule(ctx, r); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if _, _, err := ssmRepoLocked(req.GetParent()); err != nil {
		return nil, err
	}
	r.Name = req.GetParent() + "/branchRules/" + req.GetBranchRuleId()
	if _, ok := ssmState.rules[r.Name]; ok {
		return nil, apierror.AlreadyExists("branch rule %q already exists", r.Name)
	}
	if err := duplicateFilterLocked(req.GetParent(), r); err != nil {
		return nil, err
	}
	now := timestamppb.Now()
	r.Uid, r.CreateTime, r.UpdateTime, r.Etag = uuid.NewString(), now, now, ssmEtag(now)
	ssmState.rules[r.Name] = r
	if err := writeProtectedLocked(req.GetParent()); err != nil {
		delete(ssmState.rules, r.Name)
		return nil, apierror.Internal(err, "protected branches")
	}
	saveSSMLocked()
	return m.done(req.GetParent(), r.Name, "create", proto.Clone(r))
}

func ssmRuleLocked(name string) (*ssmpb.BranchRule, error) {
	parent, id, ok := strings.Cut(name, "/branchRules/")
	if !ok || ssmRepoOf(parent) != parent || !ssmRuleIDRE.MatchString(id) {
		return nil, apierror.InvalidArgument("invalid name %q: want projects/*/locations/*/repositories/*/branchRules/*", name)
	}
	r, ok := ssmState.rules[name]
	if !ok {
		return nil, apierror.NotFound("branch rule %q not found", name)
	}
	return r, nil
}

// GetBranchRule reads a branch rule.
func (m *SecureSourceManagerServer) GetBranchRule(_ context.Context, req *ssmpb.GetBranchRuleRequest) (*ssmpb.BranchRule, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	r, err := ssmRuleLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(r).(*ssmpb.BranchRule), nil
}

// ListBranchRules lists a repository's branch rules, by ID.
func (m *SecureSourceManagerServer) ListBranchRules(_ context.Context, req *ssmpb.ListBranchRulesRequest) (*ssmpb.ListBranchRulesResponse, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if _, _, err := ssmRepoLocked(req.GetParent()); err != nil {
		return nil, err
	}
	var all []*ssmpb.BranchRule
	for _, k := range sortedKeys(ssmState.rules) {
		if strings.HasPrefix(k, req.GetParent()+"/branchRules/") {
			all = append(all, proto.Clone(ssmState.rules[k]).(*ssmpb.BranchRule))
		}
	}
	page, next, err := ssmPage(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &ssmpb.ListBranchRulesResponse{BranchRules: page, NextPageToken: next}, nil
}

var ssmRuleFields = []string{"annotations", "include_pattern", "disabled", "require_pull_request", "minimum_reviews_count",
	"minimum_approvals_count", "require_code_owner_approval", "require_comments_resolved", "allow_stale_reviews",
	"require_linear_history", "required_status_checks"}

// UpdateBranchRule changes a branch rule's fields the mask names.
func (m *SecureSourceManagerServer) UpdateBranchRule(ctx context.Context, req *ssmpb.UpdateBranchRuleRequest) (*longrunningpb.Operation, error) {
	in := req.GetBranchRule()
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		// The resource sent replaces the rule's settings (Google documents
		// no implied mask for this method; booleans cannot be told unset).
		paths = []string{"*"}
	}
	paths, err := ssmMaskPaths(paths, ssmRuleFields, nil)
	if err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	cur, err := ssmRuleLocked(in.GetName())
	if err != nil {
		return nil, err
	}
	if err := checkSSMEtag(in.GetEtag(), cur.GetEtag(), cur.GetName()); err != nil {
		return nil, err
	}
	next := proto.Clone(cur).(*ssmpb.BranchRule)
	for _, p := range paths {
		switch p {
		case "annotations":
			next.Annotations = in.GetAnnotations()
		case "include_pattern":
			next.IncludePattern = in.GetIncludePattern()
		case "disabled":
			next.Disabled = in.GetDisabled()
		case "require_pull_request":
			next.RequirePullRequest = in.GetRequirePullRequest()
		case "minimum_reviews_count":
			next.MinimumReviewsCount = in.GetMinimumReviewsCount()
		case "minimum_approvals_count":
			next.MinimumApprovalsCount = in.GetMinimumApprovalsCount()
		case "require_code_owner_approval":
			next.RequireCodeOwnerApproval = in.GetRequireCodeOwnerApproval()
		case "require_comments_resolved":
			next.RequireCommentsResolved = in.GetRequireCommentsResolved()
		case "allow_stale_reviews":
			next.AllowStaleReviews = in.GetAllowStaleReviews()
		case "require_linear_history":
			next.RequireLinearHistory = in.GetRequireLinearHistory()
		case "required_status_checks":
			next.RequiredStatusChecks = in.GetRequiredStatusChecks()
		}
	}
	if err := checkBranchRule(ctx, next); err != nil {
		return nil, err
	}
	repo := ssmRepoOf(cur.GetName())
	if err := duplicateFilterLocked(repo, next); err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return m.done(repo, cur.GetName(), "update", next)
	}
	next.UpdateTime = timestamppb.Now()
	next.Etag = ssmEtag(next.UpdateTime)
	ssmState.rules[cur.GetName()] = next
	if err := writeProtectedLocked(repo); err != nil {
		ssmState.rules[cur.GetName()] = cur
		return nil, apierror.Internal(err, "protected branches")
	}
	saveSSMLocked()
	return m.done(repo, cur.GetName(), "update", proto.Clone(next))
}

// DeleteBranchRule removes a branch rule.
func (m *SecureSourceManagerServer) DeleteBranchRule(_ context.Context, req *ssmpb.DeleteBranchRuleRequest) (*longrunningpb.Operation, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	r, err := ssmRuleLocked(req.GetName())
	if status.Code(err) == codes.NotFound && req.GetAllowMissing() {
		return m.done(ssmRepoOf(req.GetName()), req.GetName(), "delete", &emptypb.Empty{})
	}
	if err != nil {
		return nil, err
	}
	repo := ssmRepoOf(r.GetName())
	delete(ssmState.rules, r.GetName())
	if err := writeProtectedLocked(repo); err != nil {
		ssmState.rules[r.GetName()] = r
		return nil, apierror.Internal(err, "protected branches")
	}
	saveSSMLocked()
	return m.done(repo, r.GetName(), "delete", &emptypb.Empty{})
}
