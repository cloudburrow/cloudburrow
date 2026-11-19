package run

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	auditpb "google.golang.org/genproto/googleapis/cloud/audit"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Cleanup policies (#1897), as
// https://docs.cloud.google.com/artifact-registry/docs/repositories/cleanup-policy
// documents them:
//
//   - they run in a background job, "within approximately one day":
//     here every DefaultCleanupInterval, or the instance's
//     --artifact-registry-cleanup-interval;
//   - a Delete policy needs a condition; a Keep policy has a condition or
//     most_recent_versions (keep_count, optional package_name_prefixes:
//     with none, each package's most recent versions are kept);
//   - a condition's fields all hold (AND), and within a list of prefixes
//     any one matches (OR), as the documented delete-prerelease example
//     reads ("30 days after upload if the artifact starts with the string
//     alpha or v0"); tag_state TAGGED, UNTAGGED or ANY, tag_prefixes only
//     with TAGGED; formats without tags are untagged; older_than and
//     newer_than measure from the version's creation;
//   - "When an artifact matches the criteria in both the delete policy and
//     the keep policy, the artifact is kept"; with no Delete policy nothing
//     is deleted;
//   - with immutable tags, "tagged artifacts can't be deleted"; a Docker
//     manifest an image index lists is not deleted directly;
//   - only standard and remote repositories take them, never virtual ones;
//     a repository has at most 10, and at most 300,000 deletions a day
//     (https://docs.cloud.google.com/artifact-registry/quotas);
//   - each run deletes with BatchDeleteVersions, one call per package,
//     recorded as a Data Access audit log entry in
//     projects/{project}/logs/cloudaudit.googleapis.com%2Fdata_access with
//     protoPayload.request.parent, names and validateOnly; in a dry run
//     (cleanup_policy_dry_run) validateOnly is true and nothing is deleted.
//
// The entry's principal is the Artifact Registry service agent, with
// permission artifactregistry.versions.delete, as the page's example shows.

// DefaultCleanupInterval is how often cleanup policies run: about daily.
const DefaultCleanupInterval = 24 * time.Hour

// maxCleanupPolicies and maxCleanupDeletesPerDay are Google's quotas.
const (
	maxCleanupPolicies      = 10
	maxCleanupDeletesPerDay = 300000
)

// LogWriter writes one entry to the instance's Cloud Logging.
type LogWriter func(ctx context.Context, e *loggingpb.LogEntry) error

var cleanupState = struct {
	mu   sync.Mutex
	logs LogWriter
	// deleted counts each repository's deletions on a UTC day.
	day     string
	deleted map[string]int
	// now is the clock; tests set it.
	now func() time.Time
}{deleted: map[string]int{}, now: time.Now}

// UseCleanupLogging sets where cleanup runs write their audit entries.
func UseCleanupLogging(w LogWriter) {
	cleanupState.mu.Lock()
	cleanupState.logs = w
	cleanupState.mu.Unlock()
}

// checkCleanupPolicies validates a repository's cleanup policies.
func checkCleanupPolicies(r *arpb.Repository) error {
	ps := r.GetCleanupPolicies()
	if len(ps) == 0 {
		return nil
	}
	if r.GetMode() == arpb.Repository_VIRTUAL_REPOSITORY {
		return apierror.InvalidArgument("cleanup policies can only be applied to standard and remote repositories, not virtual ones")
	}
	if len(ps) > maxCleanupPolicies {
		return apierror.InvalidArgument("a repository can have at most %d cleanup policies; got %d", maxCleanupPolicies, len(ps))
	}
	for id, p := range ps {
		if p.GetId() != "" && p.GetId() != id {
			return apierror.InvalidArgument("cleanup policy %q has id %q: the id must be its key", id, p.GetId())
		}
		switch p.GetAction() {
		case arpb.CleanupPolicy_DELETE:
			if p.GetCondition() == nil {
				return apierror.InvalidArgument("cleanup policy %q: a DELETE policy needs a condition", id)
			}
		case arpb.CleanupPolicy_KEEP:
			if p.GetCondition() == nil && p.GetMostRecentVersions() == nil {
				return apierror.InvalidArgument("cleanup policy %q: a KEEP policy needs a condition or most_recent_versions", id)
			}
		default:
			return apierror.InvalidArgument("cleanup policy %q: action must be DELETE or KEEP", id)
		}
		if c := p.GetCondition(); c != nil {
			if len(c.GetTagPrefixes()) > 0 && c.GetTagState() != arpb.CleanupPolicyCondition_TAGGED {
				return apierror.InvalidArgument("cleanup policy %q: tag_prefixes need tag_state TAGGED", id)
			}
			if c.GetOlderThan() != nil && c.GetOlderThan().AsDuration() < 0 || c.GetNewerThan() != nil && c.GetNewerThan().AsDuration() < 0 {
				return apierror.InvalidArgument("cleanup policy %q: older_than and newer_than must not be negative", id)
			}
		}
		if m := p.GetMostRecentVersions(); m != nil && m.GetKeepCount() < 0 {
			return apierror.InvalidArgument("cleanup policy %q: keep_count must not be negative", id)
		}
	}
	return nil
}

// cleanupVersion is one version a policy is evaluated against.
type cleanupVersion struct {
	Name, PackageName string
	Package, Version  string
	Tags              []string
	Created           time.Time
	// listed is a Docker manifest an image index of its image lists.
	listed bool
}

// cleanupVersions is every version of a repository.
func cleanupVersions(r *arpb.Repository) []cleanupVersion {
	repoName := r.GetName()
	var out []cleanupVersion
	if r.GetFormat() != arpb.Repository_DOCKER {
		for _, v := range versionsOf(repoName) {
			cv := cleanupVersion{Name: fmtVersionName(repoName, v.Package, v.Version), PackageName: fmtPackageName(repoName, v.Package),
				Package: v.Package, Version: v.Version, Created: v.First}
			for t, ver := range tagsOf(repoName, v.Package) {
				if ver == v.Version {
					cv.Tags = append(cv.Tags, t)
				}
			}
			sort.Strings(cv.Tags)
			out = append(out, cv)
		}
		return out
	}
	st := RegistryStore()
	if st == nil {
		return nil
	}
	repo := repoOf(repoName)
	for _, image := range st.Images(repo) {
		ms, err := st.Manifests(repo, image)
		if err != nil {
			continue
		}
		listed := map[string]bool{}
		for _, m := range ms {
			if m.Doc != nil {
				for _, d := range m.Doc.Manifests {
					listed[d.Digest] = true
				}
			}
		}
		pkgName := repoName + "/packages/" + esc(image)
		for _, m := range ms {
			out = append(out, cleanupVersion{Name: pkgName + "/versions/" + m.Digest, PackageName: pkgName, Package: image,
				Version: m.Digest, Tags: append([]string(nil), m.Tags...), Created: m.Uploaded, listed: listed[m.Digest]})
		}
	}
	return out
}

func anyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// conditionMatches reports whether every field of c holds for v.
func conditionMatches(c *arpb.CleanupPolicyCondition, v cleanupVersion, now time.Time) bool {
	switch c.GetTagState() {
	case arpb.CleanupPolicyCondition_TAGGED:
		if len(v.Tags) == 0 {
			return false
		}
	case arpb.CleanupPolicyCondition_UNTAGGED:
		if len(v.Tags) > 0 {
			return false
		}
	}
	if ps := c.GetTagPrefixes(); len(ps) > 0 {
		ok := false
		for _, t := range v.Tags {
			ok = ok || anyPrefix(t, ps)
		}
		if !ok {
			return false
		}
	}
	if ps := c.GetVersionNamePrefixes(); len(ps) > 0 && !anyPrefix(v.Version, ps) {
		return false
	}
	if ps := c.GetPackageNamePrefixes(); len(ps) > 0 && !anyPrefix(v.Package, ps) {
		return false
	}
	age := now.Sub(v.Created)
	if c.OlderThan != nil && age < c.GetOlderThan().AsDuration() {
		return false
	}
	if c.NewerThan != nil && age >= c.GetNewerThan().AsDuration() {
		return false
	}
	return true
}

// cleanupDeletions is what a repository's policies delete, by package, in
// the order the versions were found.
func cleanupDeletions(r *arpb.Repository, vs []cleanupVersion, now time.Time) map[string][]string {
	var deletes, keeps []*arpb.CleanupPolicy
	ids := make([]string, 0, len(r.GetCleanupPolicies()))
	for id := range r.GetCleanupPolicies() {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := r.GetCleanupPolicies()[id]
		switch p.GetAction() {
		case arpb.CleanupPolicy_DELETE:
			deletes = append(deletes, p)
		case arpb.CleanupPolicy_KEEP:
			keeps = append(keeps, p)
		}
	}
	if len(deletes) == 0 {
		return nil
	}
	kept := map[string]bool{}
	for _, p := range keeps {
		if c := p.GetCondition(); c != nil {
			for _, v := range vs {
				if conditionMatches(c, v, now) {
					kept[v.Name] = true
				}
			}
			continue
		}
		m := p.GetMostRecentVersions()
		byPkg := map[string][]cleanupVersion{}
		for _, v := range vs {
			if len(m.GetPackageNamePrefixes()) == 0 || anyPrefix(v.Package, m.GetPackageNamePrefixes()) {
				byPkg[v.Package] = append(byPkg[v.Package], v)
			}
		}
		for _, pv := range byPkg {
			sort.SliceStable(pv, func(i, j int) bool { return pv[i].Created.After(pv[j].Created) })
			for i := 0; i < len(pv) && i < int(m.GetKeepCount()); i++ {
				kept[pv[i].Name] = true
			}
		}
	}
	immutable := r.GetDockerConfig().GetImmutableTags()
	out := map[string][]string{}
	for _, v := range vs {
		if kept[v.Name] || v.listed || (immutable && len(v.Tags) > 0) {
			continue
		}
		for _, p := range deletes {
			if conditionMatches(p.GetCondition(), v, now) {
				out[v.PackageName] = append(out[v.PackageName], v.Name)
				break
			}
		}
	}
	return out
}

// RunCleanupPolicies runs every repository's cleanup policies once.
func RunCleanupPolicies(ctx context.Context) {
	cleanupState.mu.Lock()
	now := cleanupState.now()
	cleanupState.mu.Unlock()
	for _, r := range Repositories() {
		if len(r.GetCleanupPolicies()) == 0 || r.GetMode() == arpb.Repository_VIRTUAL_REPOSITORY {
			continue
		}
		runRepositoryCleanup(ctx, r, now)
	}
}

func runRepositoryCleanup(ctx context.Context, r *arpb.Repository, now time.Time) {
	dels := cleanupDeletions(r, cleanupVersions(r), now)
	pkgs := make([]string, 0, len(dels))
	for p := range dels {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	dry := r.GetCleanupPolicyDryRun()
	for _, pkg := range pkgs {
		names := dels[pkg]
		if !dry {
			names = takeDeleteQuota(r.GetName(), names, now)
			if len(names) == 0 {
				return
			}
			for _, n := range names {
				if err := deleteVersionNow(n); err != nil {
					slog.Warn("Artifact Registry cleanup policy could not delete a version", "version", n, "err", err)
				}
			}
		}
		writeCleanupAudit(ctx, r.GetName(), pkg, names, dry, now)
	}
}

// takeDeleteQuota returns the part of names the day's quota allows.
func takeDeleteQuota(repoName string, names []string, now time.Time) []string {
	cleanupState.mu.Lock()
	defer cleanupState.mu.Unlock()
	if day := now.UTC().Format("2006-01-02"); day != cleanupState.day {
		cleanupState.day, cleanupState.deleted = day, map[string]int{}
	}
	left := maxCleanupDeletesPerDay - cleanupState.deleted[repoName]
	if left <= 0 {
		return nil
	}
	if len(names) > left {
		names = names[:left]
	}
	cleanupState.deleted[repoName] += len(names)
	return names
}

// cleanupAuditEntry is the Data Access audit entry of one run's
// BatchDeleteVersions call.
func cleanupAuditEntry(repoName, pkg string, names []string, dry bool, now time.Time) (*loggingpb.LogEntry, error) {
	parts := strings.Split(repoName, "/")
	if len(parts) != 6 {
		return nil, fmt.Errorf("invalid repository name %q", repoName)
	}
	project, location := parts[1], parts[3]
	list := make([]any, len(names))
	for i, n := range names {
		list[i] = n
	}
	req, err := structpb.NewStruct(map[string]any{
		"@type":        "type.googleapis.com/google.devtools.artifactregistry.v1.BatchDeleteVersionsRequest",
		"parent":       pkg,
		"names":        list,
		"validateOnly": dry,
	})
	if err != nil {
		return nil, err
	}
	const method = "google.devtools.artifactregistry.v1.ArtifactRegistry.BatchDeleteVersions"
	agent := fmt.Sprintf("service-%d@gcp-sa-artifactregistry.iam.gserviceaccount.com", resource.ProjectNumber(project))
	payload, err := anypb.New(&auditpb.AuditLog{
		ServiceName:        "artifactregistry.googleapis.com",
		MethodName:         method,
		ResourceName:       pkg,
		AuthenticationInfo: &auditpb.AuthenticationInfo{PrincipalEmail: agent},
		AuthorizationInfo: []*auditpb.AuthorizationInfo{{Resource: repoName, Permission: "artifactregistry.versions.delete",
			Granted: true}},
		Request: req,
	})
	if err != nil {
		return nil, err
	}
	return &loggingpb.LogEntry{
		LogName: "projects/" + project + "/logs/cloudaudit.googleapis.com%2Fdata_access",
		Resource: &monitoredres.MonitoredResource{Type: "audited_resource", Labels: map[string]string{
			"project_id": project, "service": "artifactregistry.googleapis.com", "method": method}},
		Timestamp: timestamppb.New(now),
		Labels:    map[string]string{"location": location},
		Payload:   &loggingpb.LogEntry_ProtoPayload{ProtoPayload: payload},
	}, nil
}

func writeCleanupAudit(ctx context.Context, repoName, pkg string, names []string, dry bool, now time.Time) {
	cleanupState.mu.Lock()
	w := cleanupState.logs
	cleanupState.mu.Unlock()
	if w == nil {
		return
	}
	e, err := cleanupAuditEntry(repoName, pkg, names, dry, now)
	if err != nil {
		return
	}
	if err := w(ctx, e); err != nil {
		slog.Warn("Artifact Registry could not log a cleanup policy run", "repository", repoName, "err", err)
	}
}

// StartCleanupPolicies runs the cleanup policies every interval (about
// daily, as Google's background job) until ctx ends.
func StartCleanupPolicies(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultCleanupInterval
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				RunCleanupPolicies(ctx)
			}
		}
	}()
}
