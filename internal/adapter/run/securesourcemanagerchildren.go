package run

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// What a Secure Source Manager repository holds besides its git data
// (#1856): issues and their comments, pull requests, their comments and
// reviews, branch rules and hooks (#1927),
// saved in the instance state file beside the repositories and deleted
// with their repository.

// ssmSavedChildren is the children's part of the state file.
type ssmSavedChildren struct {
	Issues        []json.RawMessage `json:"issues,omitempty"`
	IssueComments []json.RawMessage `json:"issueComments,omitempty"`
	PullRequests  []json.RawMessage `json:"pullRequests,omitempty"`
	BranchRules   []json.RawMessage `json:"branchRules,omitempty"`
	Counters      map[string]int    `json:"counters,omitempty"`
	// #1927: pull request comments, hooks, who made what, and the hooks'
	// sensitive query strings and the pull requests' builds.
	PullRequestComments []json.RawMessage         `json:"pullRequestComments,omitempty"`
	Hooks               []json.RawMessage         `json:"hooks,omitempty"`
	Authors             map[string]string         `json:"authors,omitempty"`
	HookSecrets         map[string]string         `json:"hookSecrets,omitempty"`
	PullBuilds          map[string][]ssmPullBuild `json:"pullBuilds,omitempty"`
	// #2071: labels, milestones, issues' labels, milestone and assignees,
	// pending review comments and hooks' service account authentication.
	Extras *ssmSavedExtras `json:"extras,omitempty"`
}

func ssmMarshalAll[T proto.Message](m map[string]T) []json.RawMessage {
	var out []json.RawMessage
	for _, k := range sortedKeys(m) {
		b, _ := protojson.Marshal(m[k])
		out = append(out, b)
	}
	return out
}

func ssmUnmarshalAll[T proto.Message](raws []json.RawMessage, mk func() T, name func(T) string, into map[string]T) {
	for _, raw := range raws {
		v := mk()
		if protojson.Unmarshal(raw, v) == nil && ssmState.repos[ssmRepoOf(name(v))] != nil {
			into[name(v)] = v
		}
	}
}

func saveSSMChildrenLocked() ssmSavedChildren {
	return ssmSavedChildren{Issues: ssmMarshalAll(ssmState.issues), IssueComments: ssmMarshalAll(ssmState.issueComments),
		PullRequests: ssmMarshalAll(ssmState.pulls), BranchRules: ssmMarshalAll(ssmState.rules), Counters: ssmState.counters,
		PullRequestComments: ssmMarshalAll(ssmState.prComments), Hooks: ssmMarshalAll(ssmState.hooks), Authors: ssmState.authors,
		HookSecrets: ssmState.hookSecrets, PullBuilds: ssmState.prBuilds, Extras: &ssmExtra}
}

func loadSSMChildrenLocked(c ssmSavedChildren) {
	ssmUnmarshalAll(c.Issues, func() *ssmpb.Issue { return &ssmpb.Issue{} }, (*ssmpb.Issue).GetName, ssmState.issues)
	ssmUnmarshalAll(c.IssueComments, func() *ssmpb.IssueComment { return &ssmpb.IssueComment{} }, (*ssmpb.IssueComment).GetName, ssmState.issueComments)
	ssmUnmarshalAll(c.PullRequests, func() *ssmpb.PullRequest { return &ssmpb.PullRequest{} }, (*ssmpb.PullRequest).GetName, ssmState.pulls)
	ssmUnmarshalAll(c.BranchRules, func() *ssmpb.BranchRule { return &ssmpb.BranchRule{} }, (*ssmpb.BranchRule).GetName, ssmState.rules)
	ssmUnmarshalAll(c.PullRequestComments, func() *ssmpb.PullRequestComment { return &ssmpb.PullRequestComment{} },
		(*ssmpb.PullRequestComment).GetName, ssmState.prComments)
	ssmUnmarshalAll(c.Hooks, func() *ssmpb.Hook { return &ssmpb.Hook{} }, (*ssmpb.Hook).GetName, ssmState.hooks)
	for k, v := range c.Authors {
		if ssmState.repos[ssmRepoOf(k)] != nil {
			ssmState.authors[k] = v
		}
	}
	for k, v := range c.HookSecrets {
		if ssmState.hooks[k] != nil {
			ssmState.hookSecrets[k] = v
		}
	}
	for k, v := range c.PullBuilds {
		if ssmState.pulls[k] != nil {
			ssmState.prBuilds[k] = v
		}
	}
	for k, v := range c.Counters {
		if ssmState.repos[ssmRepoOf(k)] != nil {
			ssmState.counters[k] = v
		}
	}
	loadSSMExtrasLocked(c.Extras)
}

// deleteSSMChildrenLocked drops everything a repository holds.
func deleteSSMChildrenLocked(repo string) {
	prefix := repo + "/"
	deletePrefix(ssmState.issues, prefix)
	deletePrefix(ssmState.issueComments, prefix)
	deletePrefix(ssmState.pulls, prefix)
	deletePrefix(ssmState.rules, prefix)
	deletePrefix(ssmState.counters, prefix)
	deletePrefix(ssmState.prComments, prefix)
	deletePrefix(ssmState.hooks, prefix)
	deletePrefix(ssmState.authors, prefix)
	deletePrefix(ssmState.hookSecrets, prefix)
	deletePrefix(ssmState.prBuilds, prefix)
	deleteSSMExtrasLocked(prefix)
	deleteDeliveries(prefix)
}

func deletePrefix[T any](m map[string]T, prefix string) {
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			delete(m, k)
		}
	}
}

// ssmRepoOf is the repository a child's name (or a counter's key) is
// under: its first six segments.
func ssmRepoOf(name string) string {
	parts := strings.SplitN(name, "/", 7)
	if len(parts) < 6 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "repositories" {
		return ""
	}
	return strings.Join(parts[:6], "/")
}

// ssmRepoLocked finds a repository by name, and its git directory.
func ssmRepoLocked(name string) (*ssmpb.Repository, string, error) {
	if ssmRepoOf(name) != name {
		return nil, "", apierror.InvalidArgument("invalid repository %q: want projects/*/locations/*/repositories/*", name)
	}
	r, ok := ssmState.repos[name]
	if !ok {
		return nil, "", apierror.NotFound("repository %q not found", name)
	}
	root, err := ssmRootLocked()
	if err != nil {
		return nil, "", apierror.Internal(err, "repository directory")
	}
	return r, repoDir(root, name), nil
}

// ssmRepoDir is a repository's git directory, for the reads that need no
// lock held while git runs.
func ssmRepoDir(name string) (*ssmpb.Repository, string, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	r, dir, err := ssmRepoLocked(name)
	if err != nil {
		return nil, "", err
	}
	return proto.Clone(r).(*ssmpb.Repository), dir, nil
}

// ssmChildName splits a numbered child's name into its parent and number:
// {parent}/{collection}/{n}.
func ssmChildName(name, collection string) (parent string, n int, err error) {
	i := strings.LastIndex(name, "/"+collection+"/")
	if i < 0 {
		return "", 0, apierror.InvalidArgument("invalid name %q: want .../%s/*", name, collection)
	}
	n, err = strconv.Atoi(name[i+len(collection)+2:])
	if err != nil || n <= 0 {
		return "", 0, apierror.InvalidArgument("invalid name %q: the %s ID is a number", name, collection)
	}
	return name[:i], n, nil
}

// nextSSMNumberLocked gives out the next number under a parent's
// collection, starting at 1; numbers are never reused.
func nextSSMNumberLocked(parent, collection string) int {
	k := parent + "/" + collection
	ssmState.counters[k]++
	return ssmState.counters[k]
}

// sortedByNumber is a collection's names under a parent, by number.
func sortedByNumber[T any](m map[string]T, parent, collection string) []string {
	type kn struct {
		k string
		n int
	}
	var out []kn
	prefix := parent + "/" + collection + "/"
	for k := range m {
		if !strings.HasPrefix(k, prefix) || strings.Contains(k[len(prefix):], "/") {
			continue
		}
		n, _ := strconv.Atoi(k[len(prefix):])
		out = append(out, kn{k, n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].n < out[j].n })
	names := make([]string, len(out))
	for i, v := range out {
		names[i] = v.k
	}
	return names
}

// ssmPage pages a list as Google documents for these methods: 30 by
// default, at most 100.
func ssmPage[T any](items []T, size int32, token string) ([]T, string, error) {
	if size < 0 {
		return nil, "", apierror.InvalidArgument("page_size must not be negative")
	}
	if size == 0 {
		size = 30
	}
	if size > 100 {
		size = 100
	}
	return pageOf(items, size, token)
}

// ssmEtag is a fresh etag for a resource changed at t.
func ssmEtag(t *timestamppb.Timestamp) string { return fmt.Sprintf("%x", t.AsTime().UnixNano()) }

// checkSSMEtag refuses a request whose etag is set and stale (AIP-154).
func checkSSMEtag(given, current, name string) error {
	if given != "" && given != current {
		return apierror.Aborted("etag %q does not match the current etag of %q", given, name)
	}
	return nil
}

// ssmMaskPaths is an update's mask: the given paths, "*" expanded to
// every updatable field, or, when the mask is empty, the fields that are
// set (AIP-134).
func ssmMaskPaths(paths []string, updatable []string, set func(string) bool) ([]string, error) {
	if len(paths) == 0 {
		for _, f := range updatable {
			if set(f) {
				paths = append(paths, f)
			}
		}
		return paths, nil
	}
	var out []string
	for _, p := range paths {
		if p == "*" {
			return updatable, nil
		}
		ok := false
		for _, f := range updatable {
			ok = ok || f == p
		}
		if !ok {
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated: the updatable fields are %s", p, strings.Join(updatable, ", "))
		}
		out = append(out, p)
	}
	return out, nil
}

// ssmPreReceive is the pre-receive hook every repository gets: it refuses
// a push that updates a branch named in the repository's protected
// branches file, which the branch rules write (branch rules requiring a
// pull request; securesourcemanagerbranchrules.go), and has the CloudBurrow
// executable check the CODEOWNERS files a push brings to a branch whose
// rule requires code owner approval (#1928, securesourcemanagercodeowners.go).
const ssmPreReceive = `#!/bin/sh
# CloudBurrow Secure Source Manager: branches whose branch rule requires a
# pull request take no direct push; branches whose rule requires code
# owner review take no CODEOWNERS file with syntax errors.
input="$(cat)"
here="$(dirname "$0")/.."
status=0
protected="$here/` + ssmProtectedFile + `"
if [ -f "$protected" ]; then
	while read -r old new ref; do
		branch="${ref#refs/heads/}"
		[ "$branch" = "$ref" ] && continue
		if grep -Fxq -- "$branch" "$protected"; then
			echo "error: branch $branch is protected: its branch rule requires a pull request before merging" >&2
			status=1
		fi
	done <<END
$input
END
fi
[ $status -eq 0 ] || exit $status
if [ -n "$` + ssmPresubmitEnv + `" ] && grep -q "	true$" "$here/` + ssmCodeOwnersBranchesFile + `" 2>/dev/null; then
	printf '%s\n' "$input" | "$` + ssmPresubmitEnv + `" ` + SourceManagerPresubmitArg + ` "$here" || exit 1
fi
exit 0
`

const ssmProtectedFile = "cloudburrow-protected-branches"

// installSSMPreReceive writes a repository's pre-receive hook.
func installSSMPreReceive(dir string) error {
	hooks := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte(ssmPreReceive), 0o700)
}
