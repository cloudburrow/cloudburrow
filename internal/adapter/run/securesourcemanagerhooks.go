package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A repository's webhooks (#1927), as Google's "Webhooks overview" and
// "Set up webhooks" pages describe them. A hook POSTs a JSON event to its
// target URI, with sensitive_query_string appended as the URI's query
// (TARGET_URL?SENSITIVE_QUERY_STRING), on the events it names: PUSH (a
// push to the repository, or a merged pull request moving its base
// branch, filtered by push_option.branch_filter, a gobwas/glob pattern
// where "" and "*" report every branch), PULL_REQUEST (a pull request
// opened, closed, reopened or edited; a merge is "closed" with merged
// set) and PULL_REQUEST_COMMENT (a general comment added, edited or
// deleted). A disabled hook sends nothing. The request carries the
// headers Google documents (Content-Type: application/json,
// X-SecureSourceManager-Delivery: a fresh UUID, X-SecureSourceManager-Event:
// push, pull_request or pull_request_comment, and an empty
// X-SecureSourceManager-Signature) and the payloads of the overview's
// examples, field for field. The sensitive query string is never read
// back: a read shows a placeholder in its place, and an update that sends
// the placeholder keeps the stored one.
//
// Delivery is from this host, so a hook may target a local URL (a
// localhost listener, a Cloud Run service's local URL); a target on a
// Google domain is never called, since nothing here reaches Google. Each
// hook keeps its last deliveries, request and response, for the console's
// Recent deliveries and Test delivery. Google documents no retries; one
// attempt is made, with a 10-second timeout.

// ssmQueryPlaceholder stands in for a hook's sensitive query string on
// reads.
const ssmQueryPlaceholder = "****************"

// ssmHookEvents are the X-SecureSourceManager-Event header's values.
var ssmHookEvents = map[ssmpb.Hook_HookEventType]string{ssmpb.Hook_PUSH: "push", ssmpb.Hook_PULL_REQUEST: "pull_request",
	ssmpb.Hook_PULL_REQUEST_COMMENT: "pull_request_comment"}

// SourceManagerDelivery is one request a hook sent, for the console.
type SourceManagerDelivery struct {
	ID, Event, URL    string
	Status            int
	Err               string
	Time              time.Time
	Request, Response string
}

var ssmDeliveries = struct {
	mu  sync.Mutex
	log map[string][]SourceManagerDelivery
}{log: map[string][]SourceManagerDelivery{}}

const ssmKeptDeliveries = 20

// SourceManagerHookDeliveries are a hook's recent deliveries, newest first.
func SourceManagerHookDeliveries(hook string) []SourceManagerDelivery {
	ssmDeliveries.mu.Lock()
	defer ssmDeliveries.mu.Unlock()
	return append([]SourceManagerDelivery(nil), ssmDeliveries.log[hook]...)
}

func deleteDeliveries(prefix string) {
	ssmDeliveries.mu.Lock()
	defer ssmDeliveries.mu.Unlock()
	for k := range ssmDeliveries.log {
		if strings.HasPrefix(k, prefix) {
			delete(ssmDeliveries.log, k)
		}
	}
}

// ssmGlobRE compiles a gobwas/glob pattern without separators: * any run of
// characters, ? one, [abc] / [a-z] / [!abc] a class, {a,b} alternatives,
// \x a literal x.
func ssmGlobRE(p string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	depth := 0
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '\\':
			if i+1 >= len(p) {
				return nil, fmt.Errorf("a trailing backslash")
			}
			i++
			b.WriteString(regexp.QuoteMeta(string(p[i])))
		case '[':
			j := strings.IndexByte(p[i:], ']')
			if j < 0 {
				return nil, fmt.Errorf("an unclosed [")
			}
			class := p[i+1 : i+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += j
		case '{':
			depth++
			b.WriteString("(?:")
		case '}':
			if depth == 0 {
				return nil, fmt.Errorf("an unmatched }")
			}
			depth--
			b.WriteString(")")
		case ',':
			if depth > 0 {
				b.WriteString("|")
			} else {
				b.WriteString(",")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("an unclosed {")
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// googleHost reports whether a host is Google's, which nothing here calls
// (internal/egress).
func googleHost(host string) bool { return egress.GoogleHost(host) }

func checkHook(h *ssmpb.Hook) error {
	u, err := url.Parse(h.GetTargetUri())
	if h.GetTargetUri() == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return apierror.InvalidArgument("hook.target_uri %q must be an http or https URL", h.GetTargetUri())
	}
	seen := map[ssmpb.Hook_HookEventType]bool{}
	for _, e := range h.GetEvents() {
		if _, ok := ssmHookEvents[e]; !ok {
			return apierror.InvalidArgument("hook.events: %s is not an event: PUSH, PULL_REQUEST or PULL_REQUEST_COMMENT", e)
		}
		if seen[e] {
			return apierror.InvalidArgument("hook.events names %s twice", e)
		}
		seen[e] = true
	}
	if f := h.GetPushOption().GetBranchFilter(); f != "" {
		if _, err := ssmGlobRE(f); err != nil {
			return apierror.InvalidArgument("hook.push_option.branch_filter %q is not a glob pattern: %v", f, err)
		}
	}
	return nil
}

// hookOut is a hook as reads return it: its sensitive query string hidden.
func hookOut(h *ssmpb.Hook) *ssmpb.Hook {
	out := proto.Clone(h).(*ssmpb.Hook)
	if ssmState.hookSecrets[h.GetName()] != "" {
		out.SensitiveQueryString = ssmQueryPlaceholder
	}
	return out
}

// CreateHook adds a webhook to a repository.
func (m *SecureSourceManagerServer) CreateHook(_ context.Context, req *ssmpb.CreateHookRequest) (*longrunningpb.Operation, error) {
	if !ssmRuleIDRE.MatchString(req.GetHookId()) {
		return nil, apierror.InvalidArgument("invalid hook_id %q: lower case letters, digits and hyphens, starting with a letter and ending with a letter or digit, at most 63", req.GetHookId())
	}
	h := proto.Clone(req.GetHook()).(*ssmpb.Hook)
	if h == nil {
		return nil, apierror.InvalidArgument("hook is required")
	}
	if err := checkHook(h); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if _, _, err := ssmRepoLocked(req.GetParent()); err != nil {
		return nil, err
	}
	h.Name = req.GetParent() + "/hooks/" + req.GetHookId()
	if _, ok := ssmState.hooks[h.Name]; ok {
		return nil, apierror.AlreadyExists("hook %q already exists", h.Name)
	}
	now := timestamppb.Now()
	h.Uid, h.CreateTime, h.UpdateTime = uuid.NewString(), now, now
	if q := h.GetSensitiveQueryString(); q != "" {
		ssmState.hookSecrets[h.Name] = strings.TrimPrefix(q, "?")
	}
	h.SensitiveQueryString = ""
	ssmState.hooks[h.Name] = h
	saveSSMLocked()
	return m.done(req.GetParent(), h.Name, "create", hookOut(h))
}

func ssmHookLocked(name string) (*ssmpb.Hook, error) {
	parent, id, ok := strings.Cut(name, "/hooks/")
	if !ok || ssmRepoOf(parent) != parent || !ssmRuleIDRE.MatchString(id) {
		return nil, apierror.InvalidArgument("invalid name %q: want projects/*/locations/*/repositories/*/hooks/*", name)
	}
	h, ok := ssmState.hooks[name]
	if !ok {
		return nil, apierror.NotFound("hook %q not found", name)
	}
	return h, nil
}

// GetHook reads a hook.
func (m *SecureSourceManagerServer) GetHook(_ context.Context, req *ssmpb.GetHookRequest) (*ssmpb.Hook, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	h, err := ssmHookLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return hookOut(h), nil
}

// ListHooks lists a repository's hooks, by ID.
func (m *SecureSourceManagerServer) ListHooks(_ context.Context, req *ssmpb.ListHooksRequest) (*ssmpb.ListHooksResponse, error) {
	ssmState.mu.Lock()
	if _, _, err := ssmRepoLocked(req.GetParent()); err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	var all []*ssmpb.Hook
	for _, k := range sortedKeys(ssmState.hooks) {
		if strings.HasPrefix(k, req.GetParent()+"/hooks/") {
			all = append(all, hookOut(ssmState.hooks[k]))
		}
	}
	ssmState.mu.Unlock()
	page, next, err := ssmPage(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &ssmpb.ListHooksResponse{Hooks: page, NextPageToken: next}, nil
}

var ssmHookFields = []string{"target_uri", "disabled", "events", "push_option", "sensitive_query_string"}

// UpdateHook changes the fields its mask names ("*" replaces them all;
// with no mask, the fields that are set).
func (m *SecureSourceManagerServer) UpdateHook(_ context.Context, req *ssmpb.UpdateHookRequest) (*longrunningpb.Operation, error) {
	in := req.GetHook()
	paths := req.GetUpdateMask().GetPaths()
	for i, p := range paths {
		if p == "push_option.branch_filter" {
			paths[i] = "push_option"
		}
	}
	paths, err := ssmMaskPaths(paths, ssmHookFields, func(f string) bool {
		switch f {
		case "target_uri":
			return in.GetTargetUri() != ""
		case "disabled":
			return in.GetDisabled()
		case "events":
			return len(in.GetEvents()) > 0
		case "push_option":
			return in.GetPushOption() != nil
		}
		return in.GetSensitiveQueryString() != ""
	})
	if err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	cur, err := ssmHookLocked(in.GetName())
	if err != nil {
		return nil, err
	}
	next := proto.Clone(cur).(*ssmpb.Hook)
	secret, hasSecret := ssmState.hookSecrets[cur.GetName()], false
	for _, p := range paths {
		switch p {
		case "target_uri":
			next.TargetUri = in.GetTargetUri()
		case "disabled":
			next.Disabled = in.GetDisabled()
		case "events":
			next.Events = in.GetEvents()
		case "push_option":
			next.PushOption = in.GetPushOption()
		case "sensitive_query_string":
			if q := in.GetSensitiveQueryString(); q != ssmQueryPlaceholder {
				secret, hasSecret = strings.TrimPrefix(q, "?"), true
			}
		}
	}
	if err := checkHook(next); err != nil {
		return nil, err
	}
	next.UpdateTime = timestamppb.Now()
	ssmState.hooks[cur.GetName()] = next
	if hasSecret {
		if secret == "" {
			delete(ssmState.hookSecrets, cur.GetName())
		} else {
			ssmState.hookSecrets[cur.GetName()] = secret
		}
	}
	saveSSMLocked()
	return m.done(ssmRepoOf(cur.GetName()), cur.GetName(), "update", hookOut(next))
}

// DeleteHook removes a hook.
func (m *SecureSourceManagerServer) DeleteHook(_ context.Context, req *ssmpb.DeleteHookRequest) (*longrunningpb.Operation, error) {
	ssmState.mu.Lock()
	h, err := ssmHookLocked(req.GetName())
	if err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	delete(ssmState.hooks, h.GetName())
	delete(ssmState.hookSecrets, h.GetName())
	saveSSMLocked()
	ssmState.mu.Unlock()
	deleteDeliveries(h.GetName())
	return m.done(ssmRepoOf(h.GetName()), h.GetName(), "delete", &emptypb.Empty{})
}

// ssmTarget is one hook a delivery goes to.
type ssmTarget struct {
	name, url string
}

// hookTargets are the enabled hooks of a repository that take an event;
// branch is a push's branch ("" for other events and for tags).
func hookTargets(repo string, ev ssmpb.Hook_HookEventType, branch string) []ssmTarget {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	var out []ssmTarget
	for _, k := range sortedKeys(ssmState.hooks) {
		h := ssmState.hooks[k]
		if !strings.HasPrefix(k, repo+"/hooks/") || h.GetDisabled() {
			continue
		}
		takes := false
		for _, e := range h.GetEvents() {
			takes = takes || e == ev
		}
		if !takes {
			continue
		}
		if f := h.GetPushOption().GetBranchFilter(); ev == ssmpb.Hook_PUSH && branch != "" && f != "" && f != "*" {
			if re, err := ssmGlobRE(f); err != nil || !re.MatchString(branch) {
				continue
			}
		}
		out = append(out, ssmTarget{name: k, url: hookURL(h.GetTargetUri(), ssmState.hookSecrets[k])})
	}
	return out
}

// hookURL is a hook's request URL: its target with the sensitive query
// string as (or added to) its query.
func hookURL(target, query string) string {
	if query == "" {
		return target
	}
	if strings.Contains(target, "?") {
		return target + "&" + query
	}
	return target + "?" + query
}

// ssmHookClient sends through the local-only guard (#2015), so a redirect
// to a Google host is refused too.
var ssmHookClient = egress.Client(&http.Client{Timeout: 10 * time.Second})

// deliver sends one event to one hook and records it.
func deliver(ctx context.Context, t ssmTarget, event string, payload any) SourceManagerDelivery {
	body, _ := json.MarshalIndent(payload, "", "  ")
	d := SourceManagerDelivery{ID: uuid.NewString(), Event: event, URL: t.url, Time: time.Now()}
	shown := strings.SplitN(t.url, "?", 2)[0]
	d.Request = "POST " + shown + "\nContent-Type: application/json\nX-SecureSourceManager-Delivery: " + d.ID +
		"\nX-SecureSourceManager-Event: " + event + "\nX-SecureSourceManager-Signature: \n\n" + string(body)
	d.URL = shown
	defer func() {
		ssmDeliveries.mu.Lock()
		log := append([]SourceManagerDelivery{d}, ssmDeliveries.log[t.name]...)
		if len(log) > ssmKeptDeliveries {
			log = log[:ssmKeptDeliveries]
		}
		ssmDeliveries.log[t.name] = log
		ssmDeliveries.mu.Unlock()
	}()
	u, err := url.Parse(t.url)
	if err != nil {
		d.Err = err.Error()
		return d
	}
	if googleHost(u.Hostname()) {
		d.Err = "not delivered: " + u.Hostname() + " is Google's, and this instance never calls Google"
		return d
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		d.Err = err.Error()
		return d
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-SecureSourceManager-Delivery", d.ID)
	req.Header.Set("X-SecureSourceManager-Event", event)
	req.Header["X-Securesourcemanager-Signature"] = []string{""}
	resp, err := ssmHookClient.Do(req)
	if err != nil {
		d.Err = err.Error()
		return d
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	d.Status = resp.StatusCode
	d.Response = resp.Status + "\n\n" + string(rb)
	return d
}

// deliverAll sends an event to every hook that takes it, in the
// background: the request that caused it does not wait.
func deliverAll(ctx context.Context, targets []ssmTarget, event string, payload any) {
	if len(targets) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		for _, t := range targets {
			if d := deliver(ctx, t, event, payload); d.Err != "" {
				slog.Warn("securesourcemanager: webhook delivery failed", "hook", t.name, "err", d.Err)
			}
		}
	}()
}

// SourceManagerTestDelivery sends a hook a placeholder push event, as the
// web interface's Test delivery does, and waits for the answer.
func SourceManagerTestDelivery(ctx context.Context, hook string) error {
	ssmState.mu.Lock()
	h, err := ssmHookLocked(hook)
	if err != nil {
		ssmState.mu.Unlock()
		return err
	}
	t := ssmTarget{name: h.GetName(), url: hookURL(h.GetTargetUri(), ssmState.hookSecrets[h.GetName()])}
	repo := ssmRepoOf(h.GetName())
	_, dir, rerr := ssmRepoLocked(repo)
	ssmState.mu.Unlock()
	if rerr != nil {
		return rerr
	}
	branch := ""
	if out, err := ssmGit(ctx, dir, "symbolic-ref", "--short", "HEAD"); err == nil {
		branch = strings.TrimSpace(string(out))
	}
	sha := branchSHA(ctx, dir, branch)
	payload := pushPayload(ctx, repo, dir, "refs/heads/"+branch, sha, sha, ssmOwnerAccount)
	d := deliver(ctx, t, "push", payload)
	if d.Err != "" {
		return status.Error(codes.Unavailable, "test delivery failed: "+d.Err)
	}
	return nil
}

// ssmID is a stable positive number for a name, for the payloads' ids.
func ssmID(name string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum32()&0x7fffffff) + 1
}

func ssmStamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ssmUserPayload is an account as the payloads show it.
func ssmUserPayload(account string) map[string]any {
	email := ssmCallerEmail(account)
	name := email
	if i := strings.Index(email, "@"); i > 0 {
		name = email[:i]
	}
	return map[string]any{"id": ssmID("user/" + email), "login": email, "full_name": name, "email": email, "avatar_url": "",
		"language": "", "is_admin": false, "last_login": "0001-01-01T00:00:00Z", "created": "0001-01-01T00:00:00Z",
		"restricted": false, "username": email}
}

// ssmHTMLURL is a repository's web address: its git URL without .git.
func ssmHTMLURL(r *ssmpb.Repository) string {
	return strings.TrimSuffix(r.GetUris().GetGitHttps(), ".git")
}

// repoPayload is a repository as the payloads show it.
func repoPayload(ctx context.Context, name, dir string) map[string]any {
	ssmState.mu.Lock()
	r := proto.Clone(ssmState.repos[name]).(*ssmpb.Repository)
	open := 0
	for k, pr := range ssmState.pulls {
		if ssmRepoOf(k) == name && pr.GetState() == ssmpb.PullRequest_OPEN {
			open++
		}
	}
	openIssues := 0
	for k, is := range ssmState.issues {
		if ssmRepoOf(k) == name && is.GetState() == ssmpb.Issue_OPEN {
			openIssues++
		}
	}
	ssmState.mu.Unlock()
	parts := strings.Split(name, "/")
	project, id := parts[1], parts[5]
	branch, empty := "main", true
	if out, err := ssmGit(ctx, dir, "symbolic-ref", "--short", "HEAD"); err == nil {
		branch = strings.TrimSpace(string(out))
	}
	if out, err := ssmGit(ctx, dir, "for-each-ref", "--count=1", "refs/heads/"); err == nil && len(bytes.TrimSpace(out)) > 0 {
		empty = false
	}
	html := ssmHTMLURL(r)
	return map[string]any{
		"id": ssmID(name),
		"owner": map[string]any{"id": ssmID("projects/" + project), "login": project, "full_name": "", "email": "", "avatar_url": "",
			"language": "", "is_admin": false, "last_login": "0001-01-01T00:00:00Z", "created": ssmStamp(r.GetCreateTime().AsTime()),
			"restricted": false, "username": project},
		"name": id, "full_name": project + "/" + id, "description": r.GetDescription(), "empty": empty, "private": false,
		"fork": false, "template": false, "parent": nil, "mirror": false, "size": 0, "html_url": html, "ssh_url": "",
		"clone_url": r.GetUris().GetGitHttps(), "original_url": "", "website": "", "stars_count": 0, "forks_count": 0,
		"watchers_count": 0, "open_issues_count": openIssues, "open_pr_counter": open, "release_counter": 0,
		"default_branch": branch, "archived": false, "created_at": ssmStamp(r.GetCreateTime().AsTime()),
		"updated_at":       ssmStamp(r.GetUpdateTime().AsTime()),
		"permissions":      map[string]any{"admin": true, "push": true, "pull": true},
		"has_issues":       true,
		"internal_tracker": map[string]any{"enable_time_tracker": false, "allow_only_contributors_to_track_time": true, "enable_issue_dependencies": true},
		"has_wiki":         false, "has_pull_requests": true, "has_projects": false, "ignore_whitespace_conflicts": false,
		"allow_merge_commits": true, "allow_rebase": false, "allow_rebase_explicit": false, "allow_squash_merge": false,
		"avatar_url": "", "internal": false, "mirror_interval": "",
		"uris": map[string]any{"api": r.GetUris().GetApi(), "html": r.GetUris().GetHtml(), "git_https": r.GetUris().GetGitHttps()},
	}
}

// pushPayload is a push event: the commits from before to after, newest
// last, at most 20.
func pushPayload(ctx context.Context, repo, dir, ref, before, after, pusher string) map[string]any {
	rp := repoPayload(ctx, repo, dir)
	html, _ := rp["html_url"].(string)
	zero := strings.Repeat("0", 40)
	if before == "" {
		before = zero
	}
	commits := []map[string]any{}
	if strings.Trim(after, "0") != "" {
		rng := after
		if strings.Trim(before, "0") != "" {
			rng = before + ".." + after
		}
		out, err := ssmGit(ctx, dir, "log", "--reverse", "--max-count=20", "--format=%H%x00%an%x00%ae%x00%cn%x00%ce%x00%aI%x00%B%x01", rng)
		if err == nil {
			for _, rec := range strings.Split(string(out), "\x01") {
				f := strings.SplitN(strings.TrimLeft(rec, "\n"), "\x00", 7)
				if len(f) < 7 {
					continue
				}
				added, removed, modified := ssmCommitFiles(ctx, dir, f[0])
				ts, _ := time.Parse(time.RFC3339, f[5])
				commits = append(commits, map[string]any{"id": f[0], "message": f[6], "url": html + "/commit/" + f[0],
					"author":       map[string]any{"name": f[1], "email": f[2], "username": f[2]},
					"committer":    map[string]any{"name": f[3], "email": f[4], "username": f[4]},
					"verification": nil, "timestamp": ssmStamp(ts), "added": added, "removed": removed, "modified": modified})
			}
		}
	}
	return map[string]any{"secret": "", "ref": ref, "before": before, "after": after,
		"compare_url": html + "/compare/" + before + "..." + after, "commits": commits, "head_commit": nil,
		"repository": rp, "pusher": ssmUserPayload(pusher), "sender": ssmUserPayload(pusher)}
}

// ssmCommitFiles are the files a commit added, removed and modified.
func ssmCommitFiles(ctx context.Context, dir, sha string) (added, removed, modified []string) {
	added, removed, modified = []string{}, []string{}, []string{}
	out, err := ssmGit(ctx, dir, "diff-tree", "--no-commit-id", "--name-status", "-r", "--root", "--no-renames", "-z", sha)
	if err != nil {
		return
	}
	f := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		switch f[i] {
		case "A":
			added = append(added, f[i+1])
		case "D":
			removed = append(removed, f[i+1])
		default:
			modified = append(modified, f[i+1])
		}
	}
	return
}

// deliverPush sends a push event for one ref to the hooks that take it.
func (m *SecureSourceManagerServer) deliverPush(ctx context.Context, repo, dir, ref, before, after, pusher string) {
	branch := ""
	if strings.HasPrefix(ref, "refs/heads/") {
		branch = strings.TrimPrefix(ref, "refs/heads/")
	}
	targets := hookTargets(repo, ssmpb.Hook_PUSH, branch)
	if len(targets) == 0 {
		return
	}
	deliverAll(ctx, targets, "push", pushPayload(ctx, repo, dir, ref, before, after, pusher))
}

// pullPayload is a pull request as the payloads show it.
func pullPayload(ctx context.Context, dir string, pr *ssmpb.PullRequest, rp map[string]any) map[string]any {
	html, _ := rp["html_url"].(string)
	n, _ := strconv.Atoi(lastSegment(pr.GetName()))
	tipped := withTips(ctx, dir, pr)
	base, head := tipped.GetBase().GetSha(), tipped.GetHead().GetSha()
	mergeBase := ""
	if pr.GetState() != ssmpb.PullRequest_MERGED {
		if out, err := ssmGit(ctx, dir, "merge-base", base, head); err == nil {
			mergeBase = strings.TrimSpace(string(out))
		}
	}
	ssmState.mu.Lock()
	author := ssmState.authors[pr.GetName()]
	comments := 0
	for k, c := range ssmState.prComments {
		if strings.HasPrefix(k, pr.GetName()+"/") && c.GetComment() != nil {
			comments++
		}
	}
	ssmState.mu.Unlock()
	merged := pr.GetState() == ssmpb.PullRequest_MERGED
	state := "open"
	if pr.GetState() != ssmpb.PullRequest_OPEN {
		state = "closed"
	}
	var mergedAt, mergeSHA, mergedBy, closedAt any
	if merged {
		mergedAt, mergeSHA = ssmStamp(pr.GetCloseTime().AsTime()), base
	}
	if pr.GetCloseTime() != nil {
		closedAt = ssmStamp(pr.GetCloseTime().AsTime())
	}
	branch := func(ref, sha string) map[string]any {
		return map[string]any{"label": ref, "ref": ref, "sha": sha, "repo_id": rp["id"], "repo": rp}
	}
	return map[string]any{"id": ssmID(pr.GetName()), "url": html + "/pulls/" + strconv.Itoa(n), "number": n,
		"user": ssmUserPayload(orOwner(author)), "title": pr.GetTitle(), "body": pr.GetBody(), "labels": []any{}, "milestone": nil,
		"assignee": nil, "assignees": nil, "state": state, "is_locked": false, "comments": comments,
		"html_url": html + "/pulls/" + strconv.Itoa(n), "diff_url": html + "/pulls/" + strconv.Itoa(n) + ".diff",
		"patch_url": html + "/pulls/" + strconv.Itoa(n) + ".patch", "mergeable": !merged && pr.GetState() == ssmpb.PullRequest_OPEN,
		"merged": merged, "merged_at": mergedAt, "merge_commit_sha": mergeSHA, "merged_by": mergedBy,
		"base": branch(pr.GetBase().GetRef(), base), "head": branch(pr.GetHead().GetRef(), head), "merge_base": mergeBase,
		"due_date": nil, "created_at": ssmStamp(pr.GetCreateTime().AsTime()), "updated_at": ssmStamp(pr.GetUpdateTime().AsTime()),
		"closed_at": closedAt}
}

func orOwner(account string) string {
	if account == "" {
		return ssmOwnerAccount
	}
	return account
}

// deliverPull sends a pull_request event: opened, closed, reopened or
// edited.
func (m *SecureSourceManagerServer) deliverPull(ctx context.Context, pr *ssmpb.PullRequest, action, sender string) {
	repo := ssmRepoOf(pr.GetName())
	targets := hookTargets(repo, ssmpb.Hook_PULL_REQUEST, "")
	if len(targets) == 0 {
		return
	}
	_, dir, err := ssmRepoDir(repo)
	if err != nil {
		return
	}
	rp := repoPayload(ctx, repo, dir)
	n, _ := strconv.Atoi(lastSegment(pr.GetName()))
	deliverAll(ctx, targets, "pull_request", map[string]any{"secret": "", "action": action, "number": n,
		"pull_request": pullPayload(ctx, dir, pr, rp), "repository": rp, "sender": ssmUserPayload(sender), "review": nil})
}

// deliverPullComment sends a pull_request_comment event for a general
// comment: created, edited or deleted.
func (m *SecureSourceManagerServer) deliverPullComment(ctx context.Context, pull string, c *ssmpb.PullRequestComment, action, sender string) {
	repo := ssmRepoOf(pull)
	targets := hookTargets(repo, ssmpb.Hook_PULL_REQUEST_COMMENT, "")
	if len(targets) == 0 {
		return
	}
	_, dir, err := ssmRepoDir(repo)
	if err != nil {
		return
	}
	ssmState.mu.Lock()
	pr, ok := ssmState.pulls[pull]
	if ok {
		pr = proto.Clone(pr).(*ssmpb.PullRequest)
	}
	author := ssmState.authors[c.GetName()]
	ssmState.mu.Unlock()
	if !ok {
		return
	}
	rp := repoPayload(ctx, repo, dir)
	pp := pullPayload(ctx, dir, pr, rp)
	html, _ := rp["html_url"].(string)
	n, _ := strconv.Atoi(lastSegment(pull))
	cid, _ := strconv.Atoi(lastSegment(c.GetName()))
	prURL := html + "/pulls/" + strconv.Itoa(n)
	parts := strings.Split(repo, "/")
	issue := map[string]any{"id": pp["id"], "url": rp["uris"].(map[string]any)["api"].(string) + "/issues/" + strconv.Itoa(n),
		"html_url": prURL, "number": n, "user": pp["user"], "original_author": "", "original_author_id": 0,
		"title": pr.GetTitle(), "body": pr.GetBody(), "ref": "", "labels": []any{}, "milestone": nil, "assignee": nil,
		"assignees": nil, "state": pp["state"], "is_locked": false, "comments": pp["comments"], "created_at": pp["created_at"],
		"updated_at": pp["updated_at"], "closed_at": pp["closed_at"], "due_date": nil,
		"pull_request": map[string]any{"merged": pp["merged"], "merged_at": pp["merged_at"]},
		"repository":   map[string]any{"id": rp["id"], "name": parts[5], "owner": parts[1], "full_name": parts[1] + "/" + parts[5]},
		"etag":         ssmEtag(pr.GetUpdateTime())}
	comment := map[string]any{"id": cid, "html_url": prURL + "#issuecomment-" + strconv.Itoa(cid), "pull_request_url": prURL,
		"issue_url": "", "user": ssmUserPayload(orOwner(author)), "original_author": "", "original_author_id": 0,
		"body": c.GetComment().GetBody(), "created_at": ssmStamp(c.GetCreateTime().AsTime()), "updated_at": ssmStamp(c.GetUpdateTime().AsTime())}
	deliverAll(ctx, targets, "pull_request_comment", map[string]any{"secret": "", "action": action, "issue": issue,
		"comment": comment, "repository": rp, "sender": ssmUserPayload(sender), "is_pull": true})
}
