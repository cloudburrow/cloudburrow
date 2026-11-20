package run

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A pull request's comments and reviews (#1927), as Google's API reference
// and "Work with issues and pull requests" describe them. A comment is one
// of three kinds: a review (Approve, Comment or Request changes, with an
// optional summary body, pinned to the head commit it reviewed), a general
// comment, or a code comment on a line of a changed file (a positive line
// on the new side of the diff, a negative one on the old side), which may
// reply to another code comment and so joins its conversation. Comments
// are numbered by the server from 1 under their pull request. Batch create
// takes exactly one review and at most 100 code comments, all or none: the
// web interface's "Start review" then "Review". A conversation (a code
// comment and its replies) is resolved or unresolved as a whole: the names
// sent must cover the thread unless auto_fill is set.
//
// Who wrote a comment is not part of Google's resource, but it decides
// what Google's web interface allows: the author of a pull request cannot
// approve it or request changes on it ("If you are commenting on your own
// pull request, you can't approve or request changes to your pull
// request"), and only a comment's author may change or delete it. The
// author is the caller of the request: the account its bearer token was
// minted for, or the instance's own account for a request with none.
//
// A branch rule's minimum_reviews_count and minimum_approvals_count count
// the accounts whose latest Approve or Request changes review is an
// approval (of the head commit, unless allow_stale_reviews), as the
// branch protection overview defines reviewers and approvers: a reviewer
// holds roles/securesourcemanager.repoWriter, an approver
// roles/securesourcemanager.repoAdmin, or repoWriter and
// repoPullRequestApprover, on the repository; the instance's own account
// counts as both, as a project owner does. The roles are read from the
// repository's and its instance's IAM policies.

// ssmCaller resolves a request's caller (#1927).
var ssmCaller struct {
	mu sync.Mutex
	// resolve names the account a bearer token was minted for.
	resolve func(bearer string) (string, bool)
	// owner is the instance's own account.
	owner string
}

// SetSourceManagerCaller tells Secure Source Manager how to name a
// request's caller from its bearer token, and the instance's own account,
// whom a request with no token (or the owner's token) is served as.
func SetSourceManagerCaller(resolve func(bearer string) (string, bool), owner string) {
	ssmCaller.mu.Lock()
	defer ssmCaller.mu.Unlock()
	ssmCaller.resolve, ssmCaller.owner = resolve, strings.ToLower(owner)
}

// ssmOwnerAccount is the account a request with no caller is served as.
const ssmOwnerAccount = "owner"

// ssmCallerOf is the account a request came from: lower-cased, or
// ssmOwnerAccount for the instance's own account.
func ssmCallerOf(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	bearer := ""
	for _, v := range md.Get("authorization") {
		if k, t, ok := strings.Cut(v, " "); ok && strings.EqualFold(k, "bearer") {
			bearer = strings.TrimSpace(t)
		}
	}
	ssmCaller.mu.Lock()
	resolve, owner := ssmCaller.resolve, ssmCaller.owner
	ssmCaller.mu.Unlock()
	if bearer == "" || resolve == nil {
		return ssmOwnerAccount
	}
	email, ok := resolve(bearer)
	email = strings.ToLower(email)
	if !ok || email == "" || email == owner {
		return ssmOwnerAccount
	}
	return email
}

// ssmCallerEmail is the account's email for payloads: the owner's address
// for the instance's own account.
func ssmCallerEmail(account string) string {
	if account != ssmOwnerAccount {
		return account
	}
	ssmCaller.mu.Lock()
	defer ssmCaller.mu.Unlock()
	if ssmCaller.owner != "" {
		return ssmCaller.owner
	}
	return "owner@cloudburrow.localhost"
}

// ssmRolesLocked reports whether an account counts as a reviewer and as an
// approver on a repository.
func ssmRolesLocked(repo, account string) (reviewer, approver bool) {
	if account == ssmOwnerAccount {
		return true, true
	}
	roles := map[string]bool{}
	members := map[string]bool{"user:" + account: true, "serviceaccount:" + account: true, "allusers": true, "allauthenticatedusers": true}
	for _, res := range []string{repo, ssmRepoInstanceLocked(repo)} {
		p, ok := ssmState.policies[res]
		if !ok {
			continue
		}
		for _, b := range p.Bindings {
			if b.Condition != nil {
				continue // a conditional grant is not assumed to hold
			}
			for _, m := range b.Members {
				if members[strings.ToLower(m)] {
					roles[b.Role] = true
				}
			}
		}
	}
	admin := roles["roles/securesourcemanager.repoAdmin"] || roles["roles/owner"]
	writer := admin || roles["roles/securesourcemanager.repoWriter"] || roles["roles/editor"]
	return writer, admin || (writer && roles["roles/securesourcemanager.repoPullRequestApprover"])
}

// ssmRepoInstanceLocked is the instance a repository is on.
func ssmRepoInstanceLocked(repo string) string {
	if r, ok := ssmState.repos[repo]; ok {
		return r.GetInstance()
	}
	return ""
}

func ssmCommentName(name string) (pull string, err error) {
	pull, _, err = ssmChildName(name, "pullRequestComments")
	if err != nil {
		return "", err
	}
	if _, _, err := ssmChildName(pull, "pullRequests"); err != nil || ssmRepoOf(pull) == "" {
		return "", apierror.InvalidArgument("invalid name %q: want projects/*/locations/*/repositories/*/pullRequests/*/pullRequestComments/*", name)
	}
	return pull, nil
}

func ssmCommentLocked(name string) (*ssmpb.PullRequestComment, error) {
	if _, err := ssmCommentName(name); err != nil {
		return nil, err
	}
	c, ok := ssmState.prComments[name]
	if !ok {
		return nil, apierror.NotFound("pull request comment %q not found", name)
	}
	return c, nil
}

// prChangedFiles are the files a pull request changes, from the merge base
// of its branches to its head.
func prChangedFiles(ctx context.Context, dir string, pr *ssmpb.PullRequest) map[string]bool {
	pr = withTips(ctx, dir, pr)
	base, head := pr.GetBase().GetSha(), pr.GetHead().GetSha()
	if pr.GetState() == ssmpb.PullRequest_MERGED {
		if out, err := ssmGit(ctx, dir, "rev-parse", "--verify", "--quiet", base+"^1"); err == nil && base != head {
			base = strings.TrimSpace(string(out))
		}
	}
	out := map[string]bool{}
	mb, err := ssmGit(ctx, dir, "merge-base", base, head)
	if err != nil {
		return out
	}
	raw, err := ssmGit(ctx, dir, "diff", "--name-only", "-z", "--no-renames", strings.TrimSpace(string(mb)), head)
	if err != nil {
		return out
	}
	for _, f := range strings.Split(string(raw), "\x00") {
		if f != "" {
			out[f] = true
		}
	}
	return out
}

// prepareCommentLocked checks a new comment and fills its output fields:
// head is the pull request's head commit, files the files it changes.
func prepareCommentLocked(pr *ssmpb.PullRequest, in *ssmpb.PullRequestComment, author, head string, files map[string]bool) (*ssmpb.PullRequestComment, error) {
	c := proto.Clone(in).(*ssmpb.PullRequestComment)
	if c == nil {
		return nil, apierror.InvalidArgument("pull_request_comment is required")
	}
	switch d := c.GetCommentDetail().(type) {
	case *ssmpb.PullRequestComment_Review_:
		r := d.Review
		switch r.GetActionType() {
		case ssmpb.PullRequestComment_Review_COMMENT:
		case ssmpb.PullRequestComment_Review_APPROVED, ssmpb.PullRequestComment_Review_CHANGE_REQUESTED:
			if pr.GetState() != ssmpb.PullRequest_OPEN {
				return nil, apierror.FailedPrecondition("pull request %q is %s: it can no longer be approved or have changes requested", pr.GetName(), strings.ToLower(pr.GetState().String()))
			}
			if a := ssmState.authors[pr.GetName()]; a != "" && a == author {
				return nil, apierror.FailedPrecondition("you cannot approve or request changes on your own pull request")
			}
		default:
			return nil, apierror.InvalidArgument("pull_request_comment.review.action_type is required: COMMENT, CHANGE_REQUESTED or APPROVED")
		}
		r.EffectiveCommitSha = head
	case *ssmpb.PullRequestComment_Comment_:
		if strings.TrimSpace(d.Comment.GetBody()) == "" {
			return nil, apierror.InvalidArgument("pull_request_comment.comment.body is required")
		}
	case *ssmpb.PullRequestComment_Code_:
		code := d.Code
		if strings.TrimSpace(code.GetBody()) == "" {
			return nil, apierror.InvalidArgument("pull_request_comment.code.body is required")
		}
		code.Resolved, code.EffectiveRootComment = false, ""
		if code.GetReply() != "" {
			parent, ok := ssmState.prComments[code.GetReply()]
			if !ok || !strings.HasPrefix(code.GetReply(), pr.GetName()+"/pullRequestComments/") || parent.GetCode() == nil {
				return nil, apierror.InvalidArgument("pull_request_comment.code.reply %q is not a code comment of %s", code.GetReply(), pr.GetName())
			}
			root := parent.GetCode().GetEffectiveRootComment()
			if root == "" {
				root = parent.GetName()
			}
			code.EffectiveRootComment, code.Position = root, proto.Clone(parent.GetCode().GetPosition()).(*ssmpb.PullRequestComment_Position)
			code.Resolved = parent.GetCode().GetResolved()
		} else {
			pos := code.GetPosition()
			if pos.GetPath() == "" || pos.GetLine() == 0 {
				return nil, apierror.InvalidArgument("pull_request_comment.code.position.path and a non-zero position.line are required")
			}
			if !files[pos.GetPath()] {
				return nil, apierror.InvalidArgument("pull_request_comment.code.position.path %q is not a file the pull request changes", pos.GetPath())
			}
		}
		code.EffectiveCommitSha = head
	default:
		return nil, apierror.InvalidArgument("pull_request_comment needs one of review, comment or code")
	}
	return c, nil
}

// addCommentLocked stores a prepared comment under its pull request.
func addCommentLocked(pull string, c *ssmpb.PullRequestComment, author string) *ssmpb.PullRequestComment {
	now := timestamppb.Now()
	c.Name = pull + "/pullRequestComments/" + strconv.Itoa(nextSSMNumberLocked(pull, "pullRequestComments"))
	c.CreateTime, c.UpdateTime = now, now
	ssmState.prComments[c.Name] = c
	ssmState.authors[c.Name] = author
	return proto.Clone(c).(*ssmpb.PullRequestComment)
}

// commentTarget reads a pull request for a new comment: the pull request,
// its head commit and the files it changes.
func (m *SecureSourceManagerServer) commentTarget(ctx context.Context, pull string) (*ssmpb.PullRequest, string, map[string]bool, error) {
	ssmState.mu.Lock()
	pr, dir, err := ssmPullLocked(pull)
	if err != nil {
		ssmState.mu.Unlock()
		return nil, "", nil, err
	}
	pr = proto.Clone(pr).(*ssmpb.PullRequest)
	ssmState.mu.Unlock()
	tipped := withTips(ctx, dir, pr)
	return pr, tipped.GetHead().GetSha(), prChangedFiles(ctx, dir, pr), nil
}

// CreatePullRequestComment adds a review, a general comment or a code
// comment to a pull request.
func (m *SecureSourceManagerServer) CreatePullRequestComment(ctx context.Context, req *ssmpb.CreatePullRequestCommentRequest) (*longrunningpb.Operation, error) {
	author := ssmCallerOf(ctx)
	pr, head, files, err := m.commentTarget(ctx, req.GetParent())
	if err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	if _, ok := ssmState.pulls[pr.GetName()]; !ok {
		ssmState.mu.Unlock()
		return nil, apierror.NotFound("pull request %q not found", pr.GetName())
	}
	c, err := prepareCommentLocked(pr, req.GetPullRequestComment(), author, head, files)
	if err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	out := addCommentLocked(pr.GetName(), c, author)
	saveSSMLocked()
	ssmState.mu.Unlock()
	if out.GetComment() != nil {
		m.deliverPullComment(ctx, pr.GetName(), out, "created", author)
	}
	return m.done(ssmRepoOf(pr.GetName()), out.GetName(), "create", out)
}

// BatchCreatePullRequestComments submits a review: exactly one review and
// up to 100 code comments, created together or not at all.
func (m *SecureSourceManagerServer) BatchCreatePullRequestComments(ctx context.Context, req *ssmpb.BatchCreatePullRequestCommentsRequest) (*longrunningpb.Operation, error) {
	reviews, codes := 0, 0
	for i, r := range req.GetRequests() {
		if r.GetParent() != "" && r.GetParent() != req.GetParent() {
			return nil, apierror.InvalidArgument("requests[%d].parent %q is not %q", i, r.GetParent(), req.GetParent())
		}
		switch r.GetPullRequestComment().GetCommentDetail().(type) {
		case *ssmpb.PullRequestComment_Review_:
			reviews++
		case *ssmpb.PullRequestComment_Code_:
			codes++
		default:
			return nil, apierror.InvalidArgument("requests[%d]: a batch holds one review and code comments only", i)
		}
	}
	if reviews != 1 {
		return nil, apierror.InvalidArgument("requests must hold exactly one review, not %d", reviews)
	}
	if codes > 100 {
		return nil, apierror.InvalidArgument("requests hold %d code comments: at most 100", codes)
	}
	author := ssmCallerOf(ctx)
	pr, head, files, err := m.commentTarget(ctx, req.GetParent())
	if err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	if _, ok := ssmState.pulls[pr.GetName()]; !ok {
		ssmState.mu.Unlock()
		return nil, apierror.NotFound("pull request %q not found", pr.GetName())
	}
	prepared := make([]*ssmpb.PullRequestComment, 0, len(req.GetRequests()))
	for _, r := range req.GetRequests() {
		c, err := prepareCommentLocked(pr, r.GetPullRequestComment(), author, head, files)
		if err != nil {
			ssmState.mu.Unlock()
			return nil, err
		}
		prepared = append(prepared, c)
	}
	// The code comments first, then the review that sums them up.
	resp := &ssmpb.BatchCreatePullRequestCommentsResponse{}
	var review *ssmpb.PullRequestComment
	for _, c := range prepared {
		if c.GetReview() != nil {
			review = c
			continue
		}
		resp.PullRequestComments = append(resp.PullRequestComments, addCommentLocked(pr.GetName(), c, author))
	}
	resp.PullRequestComments = append(resp.PullRequestComments, addCommentLocked(pr.GetName(), review, author))
	saveSSMLocked()
	ssmState.mu.Unlock()
	return m.done(ssmRepoOf(pr.GetName()), pr.GetName(), "create", resp)
}

// GetPullRequestComment reads a pull request comment.
func (m *SecureSourceManagerServer) GetPullRequestComment(_ context.Context, req *ssmpb.GetPullRequestCommentRequest) (*ssmpb.PullRequestComment, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	c, err := ssmCommentLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(c).(*ssmpb.PullRequestComment), nil
}

// ListPullRequestComments lists a pull request's comments, oldest first.
func (m *SecureSourceManagerServer) ListPullRequestComments(_ context.Context, req *ssmpb.ListPullRequestCommentsRequest) (*ssmpb.ListPullRequestCommentsResponse, error) {
	ssmState.mu.Lock()
	if _, _, err := ssmPullLocked(req.GetParent()); err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	var all []*ssmpb.PullRequestComment
	for _, k := range sortedByNumber(ssmState.prComments, req.GetParent(), "pullRequestComments") {
		all = append(all, proto.Clone(ssmState.prComments[k]).(*ssmpb.PullRequestComment))
	}
	ssmState.mu.Unlock()
	page, next, err := ssmPage(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &ssmpb.ListPullRequestCommentsResponse{PullRequestComments: page, NextPageToken: next}, nil
}

// checkAuthorLocked refuses a change to a comment by anyone but its author
// or the instance's own account.
func checkAuthorLocked(name, caller string) error {
	if a := ssmState.authors[name]; caller != ssmOwnerAccount && a != "" && a != caller {
		return &apierror.Error{Code: codes.PermissionDenied, Message: fmt.Sprintf("only the author of %q can change or delete it", name), Reason: "forbidden"}
	}
	return nil
}

// UpdatePullRequestComment changes a comment's body, the one field Google
// documents as updatable.
func (m *SecureSourceManagerServer) UpdatePullRequestComment(ctx context.Context, req *ssmpb.UpdatePullRequestCommentRequest) (*longrunningpb.Operation, error) {
	in := req.GetPullRequestComment()
	for _, p := range req.GetUpdateMask().GetPaths() {
		if p != "body" && p != "*" {
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated: the updatable field is body", p)
		}
	}
	caller := ssmCallerOf(ctx)
	ssmState.mu.Lock()
	cur, err := ssmCommentLocked(in.GetName())
	if err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	if err := checkAuthorLocked(cur.GetName(), caller); err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	body := in.GetReview().GetBody() + in.GetComment().GetBody() + in.GetCode().GetBody()
	next := proto.Clone(cur).(*ssmpb.PullRequestComment)
	switch {
	case next.GetReview() != nil:
		next.GetReview().Body = body
	case next.GetComment() != nil:
		next.GetComment().Body = body
	case next.GetCode() != nil:
		next.GetCode().Body = body
	}
	if strings.TrimSpace(body) == "" && next.GetReview() == nil {
		ssmState.mu.Unlock()
		return nil, apierror.InvalidArgument("the comment's body is required")
	}
	next.UpdateTime = timestamppb.Now()
	ssmState.prComments[cur.GetName()] = next
	saveSSMLocked()
	out := proto.Clone(next).(*ssmpb.PullRequestComment)
	ssmState.mu.Unlock()
	pull, _ := ssmCommentName(out.GetName())
	if out.GetComment() != nil {
		m.deliverPullComment(ctx, pull, out, "edited", caller)
	}
	return m.done(ssmRepoOf(out.GetName()), out.GetName(), "update", out)
}

// DeletePullRequestComment removes a comment.
func (m *SecureSourceManagerServer) DeletePullRequestComment(ctx context.Context, req *ssmpb.DeletePullRequestCommentRequest) (*longrunningpb.Operation, error) {
	caller := ssmCallerOf(ctx)
	ssmState.mu.Lock()
	cur, err := ssmCommentLocked(req.GetName())
	if err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	if err := checkAuthorLocked(cur.GetName(), caller); err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	delete(ssmState.prComments, cur.GetName())
	delete(ssmState.authors, cur.GetName())
	saveSSMLocked()
	ssmState.mu.Unlock()
	pull, _ := ssmCommentName(cur.GetName())
	if cur.GetComment() != nil {
		m.deliverPullComment(ctx, pull, cur, "deleted", caller)
	}
	return m.done(ssmRepoOf(cur.GetName()), cur.GetName(), "delete", &emptypb.Empty{})
}

// ResolvePullRequestComments resolves code comment conversations.
func (m *SecureSourceManagerServer) ResolvePullRequestComments(_ context.Context, req *ssmpb.ResolvePullRequestCommentsRequest) (*longrunningpb.Operation, error) {
	out, err := m.setResolved(req.GetParent(), req.GetNames(), req.GetAutoFill(), true)
	if err != nil {
		return nil, err
	}
	return m.done(ssmRepoOf(req.GetParent()), req.GetParent(), "update", &ssmpb.ResolvePullRequestCommentsResponse{PullRequestComments: out})
}

// UnresolvePullRequestComments reopens code comment conversations.
func (m *SecureSourceManagerServer) UnresolvePullRequestComments(_ context.Context, req *ssmpb.UnresolvePullRequestCommentsRequest) (*longrunningpb.Operation, error) {
	out, err := m.setResolved(req.GetParent(), req.GetNames(), req.GetAutoFill(), false)
	if err != nil {
		return nil, err
	}
	return m.done(ssmRepoOf(req.GetParent()), req.GetParent(), "update", &ssmpb.UnresolvePullRequestCommentsResponse{PullRequestComments: out})
}

// threadRoot is a code comment's conversation: its root comment's name.
func threadRoot(c *ssmpb.PullRequestComment) string {
	if r := c.GetCode().GetEffectiveRootComment(); r != "" {
		return r
	}
	return c.GetName()
}

func (m *SecureSourceManagerServer) setResolved(parent string, names []string, autoFill, resolved bool) ([]*ssmpb.PullRequestComment, error) {
	if len(names) == 0 {
		return nil, apierror.InvalidArgument("names is required")
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if _, _, err := ssmPullLocked(parent); err != nil {
		return nil, err
	}
	root := ""
	given := map[string]bool{}
	for _, n := range names {
		c, err := ssmCommentLocked(n)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(n, parent+"/pullRequestComments/") {
			return nil, apierror.InvalidArgument("%q is not a comment of %s", n, parent)
		}
		if c.GetCode() == nil {
			return nil, apierror.InvalidArgument("%q is not a code comment: only code comments are resolved", n)
		}
		if root != "" && threadRoot(c) != root {
			return nil, apierror.InvalidArgument("names must all be comments of one conversation")
		}
		root = threadRoot(c)
		given[n] = true
	}
	var thread []string
	for _, k := range sortedByNumber(ssmState.prComments, parent, "pullRequestComments") {
		if c := ssmState.prComments[k]; c.GetCode() != nil && threadRoot(c) == root {
			thread = append(thread, k)
		}
	}
	if !autoFill {
		for _, k := range thread {
			if !given[k] {
				return nil, apierror.InvalidArgument("%q is in the same conversation and is not named: name every comment of the conversation, or set auto_fill", k)
			}
		}
	}
	now := timestamppb.Now()
	var out []*ssmpb.PullRequestComment
	for _, k := range thread {
		c := ssmState.prComments[k]
		c.GetCode().Resolved, c.UpdateTime = resolved, now
		out = append(out, proto.Clone(c).(*ssmpb.PullRequestComment))
	}
	saveSSMLocked()
	return out, nil
}

// reviewState is what a pull request's reviews and conversations come to
// at its head commit.
type reviewState struct {
	reviewers, approvers int
	changesRequested     []string // accounts whose latest review requests changes
	unresolved           int      // unresolved code comment conversations
}

// reviewStateLocked tallies a pull request's reviews: each account's
// latest Approve or Request changes review counts, an approval only when
// it reviewed head (or stale reviews are allowed).
func reviewStateLocked(pull, head string, allowStale bool) reviewState {
	latest := map[string]*ssmpb.PullRequestComment_Review{}
	var order []string
	roots := map[string]bool{}
	var st reviewState
	for _, k := range sortedByNumber(ssmState.prComments, pull, "pullRequestComments") {
		c := ssmState.prComments[k]
		if r := c.GetReview(); r != nil && r.GetActionType() != ssmpb.PullRequestComment_Review_COMMENT {
			a := ssmState.authors[k]
			if _, seen := latest[a]; !seen {
				order = append(order, a)
			}
			latest[a] = r
		}
		if code := c.GetCode(); code != nil && !code.GetResolved() {
			roots[threadRoot(c)] = true
		}
	}
	st.unresolved = len(roots)
	repo := ssmRepoOf(pull)
	for _, a := range order {
		r := latest[a]
		switch r.GetActionType() {
		case ssmpb.PullRequestComment_Review_CHANGE_REQUESTED:
			st.changesRequested = append(st.changesRequested, a)
		case ssmpb.PullRequestComment_Review_APPROVED:
			if !allowStale && r.GetEffectiveCommitSha() != head {
				continue
			}
			rev, appr := ssmRolesLocked(repo, a)
			if rev {
				st.reviewers++
			}
			if appr {
				st.approvers++
			}
		}
	}
	return st
}
