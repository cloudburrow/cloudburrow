package run

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"google.golang.org/protobuf/proto"
)

// Cloud Build triggers on a Developer Connect repository (#1852): the
// trigger Cloud Run's "Connect repo" makes, which runs its build on each
// push to a branch of a repository linked through Developer Connect.
//
// Google's BuildTrigger names such a repository in
// developerConnectEventConfig.gitRepositoryLink, a field the Cloud Build
// client libraries this port is built on do not carry; it is accepted here
// in repositoryEventConfig, whose repository is then the link's name
// (projects/P/locations/L/connections/C/gitRepositoryLinks/K) and whose
// push filter is the branch or tag RE2 (invertRegex reversing it), as
// Google's push filter is. The link must name a Secure Source Manager
// repository of this instance (developerconnect.go): a push to it
// (securesourcemanagergit.go) runs every enabled trigger whose filter
// matches the pushed branch or tag, with the commit's tree as /workspace
// and the repository's built-in substitutions (COMMIT_SHA, SHORT_SHA,
// REVISION_ID, REPO_NAME, REPO_FULL_NAME, REF_NAME, BRANCH_NAME or
// TAG_NAME); its includedFiles, ignoredFiles and filter narrow the pushes
// that build (#1955, cloudbuildtriggerfilter.go). RunBuildTrigger runs one
// at the head of source.branchName (or tagName, or commitSha), else of the
// repository's default branch, unfiltered.
// Pull request filters are refused (#1854): this instance's Secure Source
// Manager serves no pull requests (CreatePullRequest, MergePullRequest and
// the other pull request methods are UNIMPLEMENTED), so no pull request
// event can ever happen.

var devConnectLinkRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/connections/[^/]+/gitRepositoryLinks/[^/]+$`)

// devConnectEventError is why a repositoryEventConfig cannot run here, or
// nil when it names a Developer Connect link with a push filter.
func devConnectEventError(rc *cbpb.RepositoryEventConfig) error {
	if !devConnectLinkRE.MatchString(rc.GetRepository()) {
		return apierror.Unimplemented("BuildTrigger field repositoryEventConfig is supported only for a Developer Connect "+
			"git repository link (projects/PROJECT/locations/LOCATION/connections/CONNECTION/gitRepositoryLinks/LINK) "+
			"of this instance's Secure Source Manager; %q is not one", rc.GetRepository())
	}
	if rc.GetPullRequest() != nil {
		return apierror.Unimplemented("BuildTrigger field repositoryEventConfig.pullRequest is not supported: this instance's Secure Source Manager serves no pull requests (its pull request methods are UNIMPLEMENTED)")
	}
	push := rc.GetPush()
	if push == nil || (push.GetBranch() == "" && push.GetTag() == "") {
		return apierror.InvalidArgument("invalid trigger: repositoryEventConfig needs a push filter with a branch or tag regex")
	}
	for _, re := range []string{push.GetBranch(), push.GetTag()} {
		if _, err := regexp.Compile(re); err != nil {
			return apierror.InvalidArgument("invalid trigger: push filter %q is not RE2: %v", re, err)
		}
	}
	if _, err := devConnectLinkRepo(rc.GetRepository()); err != nil {
		return apierror.FailedPrecondition("invalid trigger: %v", err)
	}
	return nil
}

// pushFilterAccepts reports whether a push filter matches a pushed ref.
func pushFilterAccepts(f *cbpb.PushFilter, ref string) bool {
	var re string
	var short string
	switch {
	case f.GetBranch() != "" && strings.HasPrefix(ref, "refs/heads/"):
		re, short = f.GetBranch(), strings.TrimPrefix(ref, "refs/heads/")
	case f.GetTag() != "" && strings.HasPrefix(ref, "refs/tags/"):
		re, short = f.GetTag(), strings.TrimPrefix(ref, "refs/tags/")
	default:
		return false
	}
	return regexp.MustCompile(re).MatchString(short) != f.GetInvertRegex()
}

// devConnectTriggers are the stored triggers on links to repo.
func devConnectTriggers(repo string) []*cbpb.BuildTrigger {
	triggerStore.mu.Lock()
	var out []*cbpb.BuildTrigger
	for _, n := range sortedKeys(triggerStore.triggers) {
		t := triggerStore.triggers[n]
		if rc := t.GetRepositoryEventConfig(); rc != nil && devConnectLinkRE.MatchString(rc.GetRepository()) {
			out = append(out, proto.Clone(t).(*cbpb.BuildTrigger))
		}
	}
	triggerStore.mu.Unlock()
	var mine []*cbpb.BuildTrigger
	for _, t := range out {
		if named, err := devConnectLinkRepo(t.GetRepositoryEventConfig().GetRepository()); err == nil && named == repo {
			mine = append(mine, t)
		}
	}
	return mine
}

// runDevConnectPushTriggers runs the triggers on links to a repository for
// one pushed ref, from old (empty for a new ref) to sha: each whose push
// filter accepts the ref, whose includedFiles and ignoredFiles accept the
// push's changed files, and whose filter accepts its substitutions
// (#1955, cloudbuildtriggerfilter.go).
func runDevConnectPushTriggers(ctx context.Context, builds *CloudBuildServer, repo, dir, ref, old, sha string) {
	var files []string
	filesRead := false
	for _, t := range devConnectTriggers(repo) {
		if t.GetDisabled() || !pushFilterAccepts(t.GetRepositoryEventConfig().GetPush(), ref) {
			continue
		}
		if len(t.GetIncludedFiles()) > 0 || len(t.GetIgnoredFiles()) > 0 {
			if !filesRead {
				files, filesRead = changedFiles(ctx, dir, old, sha), true
			}
			if !triggerFilesAccept(t, files) {
				continue
			}
		}
		if t.GetFilter() != "" {
			p, _, _ := resource.ParseLocation(strings.SplitN(t.GetResourceName(), "/triggers/", 2)[0])
			subs := map[string]string{}
			for k, v := range t.GetSubstitutions() {
				subs[k] = v
			}
			for k, v := range devConnectBuiltins(t, repo, ref, sha) {
				subs[k] = v
			}
			subs["PROJECT_ID"] = p
			if ok, err := triggerFilterAccepts(t, subs); err != nil || !ok {
				continue
			}
		}
		if _, err := builds.runDevConnectTrigger(ctx, t, dir, ref, sha, nil); err != nil {
			slog.Warn("cloudbuild: trigger's build was refused", "repository", repo, "trigger", t.GetName(), "err", err)
		}
	}
}

// runDevConnectTrigger runs a trigger's build at a commit of its
// repository.
func (c *CloudBuildServer) runDevConnectTrigger(ctx context.Context, t *cbpb.BuildTrigger, dir, ref, sha string, extra map[string]string) (*longrunning.Operation, error) {
	link := t.GetRepositoryEventConfig().GetRepository()
	repo, err := devConnectLinkRepo(link)
	if err != nil {
		return nil, apierror.FailedPrecondition("%v", err)
	}
	cloneURI := ""
	ssmState.mu.Lock()
	if l, ok := dcState.links[link]; ok {
		cloneURI = l.GetCloneUri()
	}
	ssmState.mu.Unlock()
	b, configPath, err := triggerRepoBuild(ctx, t, dir, sha)
	if err != nil {
		return nil, err
	}
	if b.Substitutions == nil {
		b.Substitutions = map[string]string{}
	}
	for k, v := range t.GetSubstitutions() {
		b.Substitutions[k] = v
	}
	for k, v := range extra {
		b.Substitutions[k] = v
	}
	subs := devConnectBuiltins(t, repo, ref, sha)
	subs["TRIGGER_BUILD_CONFIG_PATH"] = configPath
	archive := func(ctx context.Context) (io.ReadCloser, error) {
		out, err := exec.CommandContext(ctx, "git", "-C", dir, "archive", "--format=tgz", sha).Output()
		if err != nil {
			return nil, fmt.Errorf("git archive %s: %w", sha, err)
		}
		return io.NopCloser(bytes.NewReader(out)), nil
	}
	src := &cbpb.Source{Source: &cbpb.Source_GitSource{GitSource: &cbpb.GitSource{Url: cloneURI, Revision: sha}}}
	triggerStore.mu.Lock()
	approval := triggerApprovals[t.GetResourceName()]
	triggerStore.mu.Unlock()
	// The trigger's account, never the build config's (#1854).
	b.ServiceAccount = t.GetServiceAccount()
	p, l, _ := resource.ParseLocation(strings.SplitN(t.GetResourceName(), "/triggers/", 2)[0])
	return c.createBuild(ctx, p, l, l == "global", b,
		&triggerContext{id: t.GetId(), name: t.GetName(), approval: approval, subs: subs, archive: archive, source: src})
}

// devConnectBuiltins are a repository build's built-in substitutions.
func devConnectBuiltins(t *cbpb.BuildTrigger, repo, ref, sha string) map[string]string {
	id := repo[strings.LastIndex(repo, "/")+1:]
	subs := map[string]string{"TRIGGER_NAME": t.GetName(), "COMMIT_SHA": sha, "REVISION_ID": sha, "SHORT_SHA": sha[:7],
		"REPO_NAME": id, "REPO_FULL_NAME": repo}
	if short, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		subs["BRANCH_NAME"], subs["REF_NAME"] = short, short
	} else if short, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		subs["TAG_NAME"], subs["REF_NAME"] = short, short
	}
	return subs
}

// runDevConnectManual is RunBuildTrigger of a trigger on a link: its build
// at the head of the source's branch or tag, or at its commit, else at the
// repository's default branch.
func (c *CloudBuildServer) runDevConnectManual(ctx context.Context, t *cbpb.BuildTrigger, source *cbpb.RepoSource) (*longrunning.Operation, error) {
	repo, err := devConnectLinkRepo(t.GetRepositoryEventConfig().GetRepository())
	if err != nil {
		return nil, apierror.FailedPrecondition("%v", err)
	}
	project, location, _ := resource.ParseLocation(strings.SplitN(repo, "/repositories/", 2)[0])
	_, dir, ok := lookupSSMRepo(project, location, repo[strings.LastIndex(repo, "/")+1:])
	if !ok {
		return nil, apierror.FailedPrecondition("repository %s not found", repo)
	}
	ref := "HEAD"
	switch {
	case source.GetBranchName() != "":
		ref = "refs/heads/" + source.GetBranchName()
	case source.GetTagName() != "":
		ref = "refs/tags/" + source.GetTagName()
	case source.GetCommitSha() != "":
		ref = source.GetCommitSha()
	}
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Output()
	sha := strings.TrimSpace(string(out))
	if err != nil || sha == "" {
		return nil, apierror.FailedPrecondition("repository %s has no commit at %s", repo, strings.TrimPrefix(ref, "refs/heads/"))
	}
	if ref == "HEAD" {
		if sym, err := exec.CommandContext(ctx, "git", "-C", dir, "symbolic-ref", "HEAD").Output(); err == nil {
			ref = strings.TrimSpace(string(sym))
		}
	}
	return c.runDevConnectTrigger(ctx, t, dir, ref, sha, source.GetSubstitutions())
}

// onDevConnectLink reports whether a trigger's events come from a Developer
// Connect git repository link, whose commits this instance holds.
func onDevConnectLink(t *cbpb.BuildTrigger) bool {
	rc := t.GetRepositoryEventConfig()
	return rc != nil && devConnectLinkRE.MatchString(rc.GetRepository())
}

// triggerRepoBuild is the build a trigger runs at a commit (#1854): its
// inline build; its filename, the Cloud Build config file read from the
// commit; or, autodetected as Google does, cloudbuild.yaml or
// cloudbuild.json at the repository's root, else its Dockerfile, built and
// pushed as gcr.io/$PROJECT_ID/$REPO_NAME:$COMMIT_SHA. configPath is
// TRIGGER_BUILD_CONFIG_PATH.
func triggerRepoBuild(ctx context.Context, t *cbpb.BuildTrigger, dir, sha string) (*cbpb.Build, string, error) {
	if b := t.GetBuild(); b != nil {
		return proto.Clone(b).(*cbpb.Build), "", nil
	}
	show := func(path string) ([]byte, bool) {
		out, err := exec.CommandContext(ctx, "git", "-C", dir, "show", sha+":"+path).Output()
		return out, err == nil
	}
	parse := func(path string, raw []byte) (*cbpb.Build, string, error) {
		b, err := parseBuildConfig(raw)
		if err != nil {
			return nil, "", apierror.InvalidArgument("build config file %s: %v", path, err)
		}
		if err := unsupportedCreate(b); err != nil {
			return nil, "", err
		}
		return b, path, nil
	}
	if f := strings.TrimPrefix(t.GetFilename(), "/"); f != "" {
		raw, ok := show(f)
		if !ok {
			return nil, "", apierror.FailedPrecondition("build config file %s not found at commit %s", f, sha)
		}
		return parse(f, raw)
	}
	for _, f := range []string{"cloudbuild.yaml", "cloudbuild.json"} {
		if raw, ok := show(f); ok {
			return parse(f, raw)
		}
	}
	if _, ok := show("Dockerfile"); ok {
		image := "gcr.io/$PROJECT_ID/$REPO_NAME:$COMMIT_SHA"
		return &cbpb.Build{Steps: []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "-t", image, "."}}},
			Images: []string{image}}, "", nil
	}
	return nil, "", apierror.FailedPrecondition("autodetect found no cloudbuild.yaml, cloudbuild.json or Dockerfile at the root of commit %s", sha)
}

// ParseBuildConfig reads a Cloud Build config file, YAML or JSON, as a
// trigger's inline build (the console's Inline configuration, #1854).
func ParseBuildConfig(raw []byte) (*cbpb.Build, error) { return parseBuildConfig(raw) }
