package run

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// Remote repositories (#1546). Google documents another Artifact Registry
// repository as a remote repository's upstream:
// remote_repository_config.common_repository.uri, the upstream's
// projects/{project}/locations/{location}/repositories/{repository}, a
// standard repository of the same format. Such a remote repository is
// served here for every format whose remote mode Google documents with an
// Artifact Registry upstream (DOCKER, MAVEN, NPM and PYTHON): a read the
// remote repository cannot answer from what it has cached is answered from
// the upstream, and what was read is kept in the remote repository, which
// then lists it as its own package, version and file, as Google's caches
// it. A remote repository is read-only. Every other upstream (Docker Hub,
// PyPI, npmjs, Maven Central, Debian, a custom URL, or a repository this
// instance does not hold) is outside the instance and refused: egress is
// off, and the instance makes no calls off the machine.

// remoteFormats are the formats whose remote repositories are served.
var remoteFormats = map[arpb.Repository_Format]bool{
	arpb.Repository_DOCKER: true, arpb.Repository_MAVEN: true, arpb.Repository_NPM: true, arpb.Repository_PYTHON: true,
}

// pkgDevRE is a repository's registry host and path,
// {location}-{format}.pkg.dev/{project}/{repository}.
var pkgDevRE = regexp.MustCompile(`^([a-z0-9-]+)-(docker|python|npm|maven)\.pkg\.(?:dev|localhost)/([^/]+)/([^/]+)/?$`)

// upstreamName is the repository name a common_repository URI names:
// projects/*/locations/*/repositories/*, with or without the API's
// https://artifactregistry.googleapis.com/v1/ in front, or the
// repository's https://{location}-{format}.pkg.dev/{project}/{repository}.
func upstreamName(uri string) (string, bool) {
	u := strings.TrimSpace(uri)
	for _, p := range []string{"https://", "http://"} {
		u = strings.TrimPrefix(u, p)
	}
	u = strings.TrimPrefix(u, "artifactregistry.googleapis.com/v1/")
	if strings.HasPrefix(u, "projects/") {
		u = strings.TrimSuffix(u, "/")
		if _, _, err := parseRepositoryName(u); err != nil {
			return "", false
		}
		return u, true
	}
	if m := pkgDevRE.FindStringSubmatch(strings.ToLower(u)); m != nil {
		return "projects/" + m[3] + "/locations/" + m[1] + "/repositories/" + m[4], true
	}
	return "", false
}

// remoteUpstream is a remote repository's upstream repository name, or ""
// when r is not a remote repository over one of this instance's.
func remoteUpstream(r *arpb.Repository) string {
	if r.GetMode() != arpb.Repository_REMOTE_REPOSITORY {
		return ""
	}
	n, _ := upstreamName(r.GetRemoteRepositoryConfig().GetCommonRepository().GetUri())
	return n
}

const outsideUpstream = "this instance's remote repositories pull only from another Artifact Registry repository it holds, " +
	"named by remote_repository_config.common_repository.uri (projects/{project}/locations/{location}/repositories/{repository}), " +
	"as Google documents; egress is off, and the instance makes no calls off the machine"

// checkRemote accepts a remote repository whose upstream is a standard
// repository of its format on this instance.
func checkRemote(r *arpb.Repository) error {
	rc := r.GetRemoteRepositoryConfig()
	if rc == nil {
		return apierror.InvalidArgument("a remote repository needs remote_repository_config")
	}
	if !remoteFormats[r.GetFormat()] {
		return apierror.Unimplemented("remote %v repositories are not supported: Google documents an Artifact Registry upstream for DOCKER, MAVEN, NPM and PYTHON remote repositories only, "+
			"and every other upstream is outside this instance: %s", r.GetFormat(), outsideUpstream)
	}
	common, ok := rc.GetRemoteSource().(*arpb.RemoteRepositoryConfig_CommonRepository)
	if !ok {
		what := "no upstream"
		if rc.GetRemoteSource() != nil {
			what = "a public or custom upstream"
		}
		return apierror.Unimplemented("remote repository with %s is not supported: it is outside this instance: %s", what, outsideUpstream)
	}
	name, ok := upstreamName(common.CommonRepository.GetUri())
	if !ok {
		return apierror.Unimplemented("upstream %q is outside this instance: %s", common.CommonRepository.GetUri(), outsideUpstream)
	}
	repositories.mu.Lock()
	up, exists := repositories.repos[name]
	repositories.mu.Unlock()
	if !exists {
		if rc.GetDisableUpstreamValidation() {
			return nil
		}
		return apierror.InvalidArgument("upstream %s is not a repository of this instance: %s", name, outsideUpstream)
	}
	if up.GetFormat() != r.GetFormat() || up.GetMode() != arpb.Repository_STANDARD_REPOSITORY {
		return apierror.InvalidArgument("upstream %s must be a standard %v repository: Artifact Registry upstream repositories must be standard mode repositories", name, r.GetFormat())
	}
	return nil
}

// lookupRegistryUpstream is the Docker registry's Remote: the upstream a
// remote DOCKER repository pulls through from.
func lookupRegistryUpstream(repo registry.Repo) (registry.Repo, bool) {
	r, ok := repoFormat("projects/" + repo.Project + "/locations/" + repo.Location + "/repositories/" + repo.Repository)
	if !ok || r.GetMode() != arpb.Repository_REMOTE_REPOSITORY {
		return registry.Repo{}, false
	}
	up := remoteUpstream(r)
	if up == "" {
		return registry.Repo{}, true
	}
	return repoOf(up), true
}

// readRepos are the repositories a read searches: the repository; a
// remote one's cache, then its upstream; or a virtual one's upstreams by
// descending priority, each remote one followed by its own upstream.
func (p *pkgRequest) readRepos() []string {
	switch p.repo.GetMode() {
	case arpb.Repository_REMOTE_REPOSITORY:
		return withUpstream(p.repo)
	case arpb.Repository_VIRTUAL_REPOSITORY:
	default:
		return []string{p.repo.GetName()}
	}
	ups := append([]*arpb.UpstreamPolicy(nil), p.repo.GetVirtualRepositoryConfig().GetUpstreamPolicies()...)
	sort.SliceStable(ups, func(i, j int) bool { return ups[i].GetPriority() > ups[j].GetPriority() })
	var out []string
	for _, u := range ups {
		if r, ok := repoFormat(u.GetRepository()); ok && r.GetMode() == arpb.Repository_REMOTE_REPOSITORY {
			out = append(out, withUpstream(r)...)
			continue
		}
		out = append(out, u.GetRepository())
	}
	return out
}

func withUpstream(r *arpb.Repository) []string {
	out := []string{r.GetName()}
	if up := remoteUpstream(r); up != "" {
		out = append(out, up)
	}
	return out
}

func (p *pkgRequest) writable(w http.ResponseWriter) bool {
	switch p.repo.GetMode() {
	case arpb.Repository_VIRTUAL_REPOSITORY:
		http.Error(w, "a virtual repository is read-only: upload to one of its upstream repositories", http.StatusMethodNotAllowed)
		return false
	case arpb.Repository_REMOTE_REPOSITORY:
		http.Error(w, "a remote repository is read-only: it caches what it pulls from its upstream, "+remoteUpstream(p.repo)+"; upload there", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// serveFile serves a file a read found in repo, first keeping it in each
// remote repository the read went through to reach repo.
func (p *pkgRequest) serveFile(w http.ResponseWriter, r *http.Request, repo string, f fmtFile, contentType string) {
	p.cacheThrough(repo, f)
	serveBlob(w, r, f, contentType)
}

// cacheThrough keeps f, read from upstream repository from, in the remote
// repositories of this request whose upstream it is: the request's own
// repository, or a virtual one's remote upstreams. The content is the
// upstream's blob, shared by digest; npm's version document comes too.
func (p *pkgRequest) cacheThrough(from string, f fmtFile) {
	var remotes []*arpb.Repository
	switch p.repo.GetMode() {
	case arpb.Repository_REMOTE_REPOSITORY:
		remotes = append(remotes, p.repo)
	case arpb.Repository_VIRTUAL_REPOSITORY:
		for _, u := range p.repo.GetVirtualRepositoryConfig().GetUpstreamPolicies() {
			if r, ok := repoFormat(u.GetRepository()); ok && r.GetMode() == arpb.Repository_REMOTE_REPOSITORY {
				remotes = append(remotes, r)
			}
		}
	}
	for _, rr := range remotes {
		if remoteUpstream(rr) == from && from != rr.GetName() {
			cacheFile(rr.GetName(), from, f)
		}
	}
}

// cacheFile adds f to a remote repository's index unless it holds a file
// of that ID already.
func cacheFile(remote, from string, f fmtFile) {
	fmtState.mu.Lock()
	defer fmtState.mu.Unlock()
	r := fmtRepoLocked(remote)
	for _, x := range r.Files {
		if x.ID == f.ID {
			return
		}
	}
	c := f
	c.Uploaded = time.Now().UTC()
	r.Files = append(r.Files, &c)
	if src := fmtState.repos[from]; src != nil {
		if doc, ok := src.Docs[f.Package][f.Version]; ok {
			if r.Docs == nil {
				r.Docs = map[string]map[string]json.RawMessage{}
			}
			if r.Docs[f.Package] == nil {
				r.Docs[f.Package] = map[string]json.RawMessage{}
			}
			r.Docs[f.Package][f.Version] = doc
		}
	}
	saveFormatsLocked()
}

// repositoryPath is a repository create or patch call's path.
var repositoryPath = regexp.MustCompile(`^/v1/projects/[^/]+/locations/[^/]+/repositories(/[^/:]+)?$`)

// takeRemoteConfigExtras drops, from a repository's JSON, the
// remoteRepositoryConfig fields Google's API has and the published proto
// this instance is built on does not, which gcloud sends on every remote
// repository create, enableIngestionAttestation when false, and Terraform
// does, noCache on a REMOTE_REPOSITORY. Otherwise (an attestation, a
// connector repository, or serviceDirectoryConfig, a private network's
// upstream) the call is refused, as nothing here builds them.
func takeRemoteConfigExtras(r *http.Request) (*http.Request, error) {
	if (r.Method != http.MethodPost && r.Method != http.MethodPatch) || !repositoryPath.MatchString(r.URL.Path) || r.Body == nil {
		return r, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		return r, apierror.InvalidArgument("the request body could not be read")
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return r, nil // the transcoder reports it
	}
	var rc map[string]json.RawMessage
	if json.Unmarshal(body["remoteRepositoryConfig"], &rc) != nil || rc == nil {
		return r, nil
	}
	if v, ok := rc["enableIngestionAttestation"]; ok {
		if string(bytes.TrimSpace(v)) == "true" {
			return r, apierror.Unimplemented("remoteRepositoryConfig.enableIngestionAttestation is not supported: no attestation is made locally")
		}
		delete(rc, "enableIngestionAttestation")
	}
	if _, ok := rc["noCache"]; ok {
		// Terraform sends it on every remote repository; it only marks a
		// CONNECTOR_REPOSITORY, a mode the proto does not have.
		if string(bytes.TrimSpace(body["mode"])) != `"REMOTE_REPOSITORY"` {
			return r, apierror.Unimplemented("remoteRepositoryConfig.noCache (a connector repository) is not supported: %s", outsideUpstream)
		}
		delete(rc, "noCache")
	}
	if _, ok := rc["serviceDirectoryConfig"]; ok {
		return r, apierror.Unimplemented("remoteRepositoryConfig.serviceDirectoryConfig is not supported: %s", outsideUpstream)
	}
	body["remoteRepositoryConfig"], _ = json.Marshal(rc)
	raw, _ = json.Marshal(body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	r.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	return r, nil
}
