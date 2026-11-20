package run

import (
	"context"
	"strconv"
	"strings"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A repository's issues and their comments (#1856), as Google's API
// reference describes them: an issue is numbered by the server, from 1 in
// each repository, and is OPEN until closed (close_time set, and cleared
// when it is reopened); a comment is numbered from 1 under its issue.
// ListIssues' filter takes `state=OPEN` or `state=CLOSED` (Google
// documents no filter grammar for it, so any other filter is
// UNIMPLEMENTED); an issue's etag, when sent, must be current.

// CreateIssue opens an issue on a repository.
func (m *SecureSourceManagerServer) CreateIssue(_ context.Context, req *ssmpb.CreateIssueRequest) (*longrunningpb.Operation, error) {
	in := req.GetIssue()
	if strings.TrimSpace(in.GetTitle()) == "" {
		return nil, apierror.InvalidArgument("issue.title is required")
	}
	ssmState.mu.Lock()
	if _, _, err := ssmRepoLocked(req.GetParent()); err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	now := timestamppb.Now()
	n := nextSSMNumberLocked(req.GetParent(), "issues")
	is := &ssmpb.Issue{Name: req.GetParent() + "/issues/" + strconv.Itoa(n), Title: in.GetTitle(), Body: in.GetBody(),
		State: ssmpb.Issue_OPEN, CreateTime: now, UpdateTime: now, Etag: ssmEtag(now)}
	ssmState.issues[is.Name] = is
	out := proto.Clone(is)
	saveSSMLocked()
	ssmState.mu.Unlock()
	return m.done(req.GetParent(), is.Name, "create", out)
}

func ssmIssueLocked(name string) (*ssmpb.Issue, error) {
	if _, _, err := ssmChildName(name, "issues"); err != nil {
		return nil, err
	}
	is, ok := ssmState.issues[name]
	if !ok {
		return nil, apierror.NotFound("issue %q not found", name)
	}
	return is, nil
}

// GetIssue reads an issue.
func (m *SecureSourceManagerServer) GetIssue(_ context.Context, req *ssmpb.GetIssueRequest) (*ssmpb.Issue, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	is, err := ssmIssueLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(is).(*ssmpb.Issue), nil
}

// ListIssues lists a repository's issues, by number.
func (m *SecureSourceManagerServer) ListIssues(_ context.Context, req *ssmpb.ListIssuesRequest) (*ssmpb.ListIssuesResponse, error) {
	var zero *ssmpb.Issue
	f, err := aipfilter.Compile(req.GetFilter(), zero.ProtoReflect().Descriptor())
	if err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if _, _, err := ssmRepoLocked(req.GetParent()); err != nil {
		return nil, err
	}
	var all []*ssmpb.Issue
	for _, k := range sortedByNumber(ssmState.issues, req.GetParent(), "issues") {
		if is := ssmState.issues[k]; f.Match(is) {
			all = append(all, proto.Clone(is).(*ssmpb.Issue))
		}
	}
	page, next, err := ssmPage(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &ssmpb.ListIssuesResponse{Issues: page, NextPageToken: next}, nil
}

// UpdateIssue changes an issue's title or body.
func (m *SecureSourceManagerServer) UpdateIssue(_ context.Context, req *ssmpb.UpdateIssueRequest) (*longrunningpb.Operation, error) {
	in := req.GetIssue()
	paths, err := ssmMaskPaths(req.GetUpdateMask().GetPaths(), []string{"title", "body"}, func(f string) bool {
		return (f == "title" && in.GetTitle() != "") || (f == "body" && in.GetBody() != "")
	})
	if err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	is, err := ssmIssueLocked(in.GetName())
	if err != nil {
		return nil, err
	}
	if err := checkSSMEtag(in.GetEtag(), is.GetEtag(), is.GetName()); err != nil {
		return nil, err
	}
	next := proto.Clone(is).(*ssmpb.Issue)
	for _, p := range paths {
		switch p {
		case "title":
			if strings.TrimSpace(in.GetTitle()) == "" {
				return nil, apierror.InvalidArgument("issue.title is required")
			}
			next.Title = in.GetTitle()
		case "body":
			next.Body = in.GetBody()
		}
	}
	next.UpdateTime = timestamppb.Now()
	next.Etag = ssmEtag(next.UpdateTime)
	ssmState.issues[is.GetName()] = next
	saveSSMLocked()
	return m.done(ssmRepoOf(is.GetName()), is.GetName(), "update", proto.Clone(next))
}

// DeleteIssue deletes an issue and its comments.
func (m *SecureSourceManagerServer) DeleteIssue(_ context.Context, req *ssmpb.DeleteIssueRequest) (*longrunningpb.Operation, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	is, err := ssmIssueLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := checkSSMEtag(req.GetEtag(), is.GetEtag(), is.GetName()); err != nil {
		return nil, err
	}
	delete(ssmState.issues, is.GetName())
	deletePrefix(ssmState.issueComments, is.GetName()+"/")
	deletePrefix(ssmState.counters, is.GetName()+"/")
	saveSSMLocked()
	return m.done(ssmRepoOf(is.GetName()), is.GetName(), "delete", &emptypb.Empty{})
}

// setIssueState opens or closes an issue.
func (m *SecureSourceManagerServer) setIssueState(name, etag string, st ssmpb.Issue_State, verb string) (*longrunningpb.Operation, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	is, err := ssmIssueLocked(name)
	if err != nil {
		return nil, err
	}
	if err := checkSSMEtag(etag, is.GetEtag(), name); err != nil {
		return nil, err
	}
	if is.GetState() != st {
		now := timestamppb.Now()
		is.State, is.UpdateTime, is.Etag = st, now, ssmEtag(now)
		is.CloseTime = nil
		if st == ssmpb.Issue_CLOSED {
			is.CloseTime = now
		}
		saveSSMLocked()
	}
	return m.done(ssmRepoOf(name), name, verb, proto.Clone(is))
}

// OpenIssue reopens a closed issue.
func (m *SecureSourceManagerServer) OpenIssue(_ context.Context, req *ssmpb.OpenIssueRequest) (*longrunningpb.Operation, error) {
	return m.setIssueState(req.GetName(), req.GetEtag(), ssmpb.Issue_OPEN, "update")
}

// CloseIssue closes an issue.
func (m *SecureSourceManagerServer) CloseIssue(_ context.Context, req *ssmpb.CloseIssueRequest) (*longrunningpb.Operation, error) {
	return m.setIssueState(req.GetName(), req.GetEtag(), ssmpb.Issue_CLOSED, "update")
}

// CreateIssueComment comments on an issue.
func (m *SecureSourceManagerServer) CreateIssueComment(_ context.Context, req *ssmpb.CreateIssueCommentRequest) (*longrunningpb.Operation, error) {
	if strings.TrimSpace(req.GetIssueComment().GetBody()) == "" {
		return nil, apierror.InvalidArgument("issue_comment.body is required")
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if _, err := ssmIssueLocked(req.GetParent()); err != nil {
		return nil, err
	}
	now := timestamppb.Now()
	c := &ssmpb.IssueComment{Name: req.GetParent() + "/issueComments/" + strconv.Itoa(nextSSMNumberLocked(req.GetParent(), "issueComments")),
		Body: req.GetIssueComment().GetBody(), CreateTime: now, UpdateTime: now}
	ssmState.issueComments[c.Name] = c
	saveSSMLocked()
	return m.done(ssmRepoOf(c.Name), c.Name, "create", proto.Clone(c))
}

func ssmIssueCommentLocked(name string) (*ssmpb.IssueComment, error) {
	if _, _, err := ssmChildName(name, "issueComments"); err != nil {
		return nil, err
	}
	c, ok := ssmState.issueComments[name]
	if !ok {
		return nil, apierror.NotFound("issue comment %q not found", name)
	}
	return c, nil
}

// GetIssueComment reads a comment.
func (m *SecureSourceManagerServer) GetIssueComment(_ context.Context, req *ssmpb.GetIssueCommentRequest) (*ssmpb.IssueComment, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	c, err := ssmIssueCommentLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(c).(*ssmpb.IssueComment), nil
}

// ListIssueComments lists an issue's comments, oldest first.
func (m *SecureSourceManagerServer) ListIssueComments(_ context.Context, req *ssmpb.ListIssueCommentsRequest) (*ssmpb.ListIssueCommentsResponse, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if _, err := ssmIssueLocked(req.GetParent()); err != nil {
		return nil, err
	}
	var all []*ssmpb.IssueComment
	for _, k := range sortedByNumber(ssmState.issueComments, req.GetParent(), "issueComments") {
		all = append(all, proto.Clone(ssmState.issueComments[k]).(*ssmpb.IssueComment))
	}
	page, next, err := ssmPage(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &ssmpb.ListIssueCommentsResponse{IssueComments: page, NextPageToken: next}, nil
}

// UpdateIssueComment changes a comment's body.
func (m *SecureSourceManagerServer) UpdateIssueComment(_ context.Context, req *ssmpb.UpdateIssueCommentRequest) (*longrunningpb.Operation, error) {
	in := req.GetIssueComment()
	paths, err := ssmMaskPaths(req.GetUpdateMask().GetPaths(), []string{"body"}, func(string) bool { return in.GetBody() != "" })
	if err != nil {
		return nil, err
	}
	if len(paths) > 0 && strings.TrimSpace(in.GetBody()) == "" {
		return nil, apierror.InvalidArgument("issue_comment.body is required")
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	c, err := ssmIssueCommentLocked(in.GetName())
	if err != nil {
		return nil, err
	}
	if len(paths) > 0 {
		c.Body, c.UpdateTime = in.GetBody(), timestamppb.Now()
		saveSSMLocked()
	}
	return m.done(ssmRepoOf(c.GetName()), c.GetName(), "update", proto.Clone(c))
}

// DeleteIssueComment deletes a comment.
func (m *SecureSourceManagerServer) DeleteIssueComment(_ context.Context, req *ssmpb.DeleteIssueCommentRequest) (*longrunningpb.Operation, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	c, err := ssmIssueCommentLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	delete(ssmState.issueComments, c.GetName())
	saveSSMLocked()
	return m.done(ssmRepoOf(c.GetName()), c.GetName(), "delete", &emptypb.Empty{})
}
