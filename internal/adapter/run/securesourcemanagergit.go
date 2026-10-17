package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"os/exec"
	"regexp"
	"strings"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

// A Secure Source Manager repository over git's smart HTTP protocol
// (#1521), served by `git http-backend` from the repository's bare
// directory, at {port}/sourcemanager/git/{project}/{location}/{repo}.git.
//
// A push runs the push triggers of the triggers file, as Google's
// "Connect to Cloud Build" and "Triggers file schema" pages document it:
// .cloudbuild/triggers.yaml on the repository's default branch, a list of
// triggers, each with a name, a project (the repository's by default), a
// configFilePath (.cloudbuild/cloudbuild.yaml by default), an eventType
// (push by default; pull_request triggers never run, as no pull request is
// served), RE2 includedGitRefs and ignoredGitRefs (ignored first; an empty
// filter does not restrict), includedFiles and ignoredFiles over the
// push's changed files, a required serviceAccount, disabled, and
// substitutions. Each pushed ref a trigger accepts runs one build of the
// config file at the pushed commit, with the commit's tree as /workspace
// and the built-in substitutions TRIGGER_NAME, COMMIT_SHA, REVISION_ID,
// SHORT_SHA, REPO_NAME, REPO_FULL_NAME, REF_NAME and
// TRIGGER_BUILD_CONFIG_PATH. The build runs in the repository's location.
//
// CloudBurrow's choices where Google documents none: a ref filter matches
// when it matches either the full ref (refs/heads/main) or its short name
// (main); a trigger with no serviceAccount, or a config that does not
// parse, is skipped and logged rather than failing the push, as the push
// has already landed; the build's source is recorded as a gitSource with
// the repository's URL and the commit.

const ssmGitPrefix = "/sourcemanager/git/"

// IsSourceManagerGitPath reports whether a path is a repository's git URL.
func IsSourceManagerGitPath(p string) bool { return strings.HasPrefix(p, ssmGitPrefix) }

// serveGit serves a git request to a repository, and runs the triggers of
// a push. builds may be nil, when no trigger can run.
func (m *SecureSourceManagerServer) serveGit(builds *CloudBuildServer, w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, ssmGitPrefix)
	parts := strings.SplitN(rest, "/", 4) // project, location, repo.git, rest
	if len(parts) < 3 || !strings.HasSuffix(parts[2], ".git") {
		http.Error(w, "not a repository URL: want "+ssmGitPrefix+"PROJECT/LOCATION/REPOSITORY.git", http.StatusNotFound)
		return
	}
	project, location, id := parts[0], parts[1], strings.TrimSuffix(parts[2], ".git")
	repo, dir, ok := lookupSSMRepo(project, location, id)
	if !ok {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	gitExe, err := exec.LookPath("git")
	if err != nil {
		http.Error(w, "git is not installed on this host", http.StatusNotImplemented)
		return
	}
	push := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack")
	var before map[string]string
	if push {
		before = gitRefs(r.Context(), dir)
	}
	h := &cgi.Handler{
		Path: gitExe,
		Args: []string{"http-backend"},
		Root: ssmGitPrefix + project + "/" + location,
		Env:  []string{"GIT_PROJECT_ROOT=" + strings.TrimSuffix(dir, "/"+id+".git"), "GIT_HTTP_EXPORT_ALL=1"},
	}
	h.ServeHTTP(w, r)
	if !push || builds == nil {
		return
	}
	after := gitRefs(r.Context(), dir)
	ctx := context.WithoutCancel(withCaller(r).Context())
	for ref, sha := range after {
		if before[ref] == sha {
			continue
		}
		m.runPushTriggers(ctx, builds, repo, dir, ref, before[ref], sha)
	}
}

// gitRefs is a repository's refs and the commits they name.
func gitRefs(ctx context.Context, dir string) map[string]string {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "for-each-ref", "--format=%(objectname) %(refname)").Output()
	refs := map[string]string{}
	if err != nil {
		return refs
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if sha, ref, ok := strings.Cut(line, " "); ok {
			refs[ref] = sha
		}
	}
	return refs
}

// ssmTrigger is one entry of the triggers file.
type ssmTrigger struct {
	Name            string            `yaml:"name"`
	Project         string            `yaml:"project"`
	ConfigFilePath  string            `yaml:"configFilePath"`
	EventType       string            `yaml:"eventType"`
	IncludedGitRefs string            `yaml:"includedGitRefs"`
	IgnoredGitRefs  string            `yaml:"ignoredGitRefs"`
	IncludedFiles   string            `yaml:"includedFiles"`
	IgnoredFiles    string            `yaml:"ignoredFiles"`
	ServiceAccount  string            `yaml:"serviceAccount"`
	Disabled        bool              `yaml:"disabled"`
	Substitutions   map[string]string `yaml:"substitutions"`
}

var (
	ssmTriggerNameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	ssmSubstKeyRE    = regexp.MustCompile(`^_[A-Z0-9_]+$`)
	ssmBuiltinKeys   = map[string]bool{"TRIGGER_NAME": true, "COMMIT_SHA": true, "REVISION_ID": true, "SHORT_SHA": true,
		"REPO_NAME": true, "REPO_FULL_NAME": true, "REF_NAME": true, "TRIGGER_BUILD_CONFIG_PATH": true}
)

// ssmTriggersFile is the triggers file's path on the default branch.
const ssmTriggersFile = ".cloudbuild/triggers.yaml"

// ParseSourceManagerTriggers parses a triggers file, refusing what the
// schema does not allow.
func ParseSourceManagerTriggers(b []byte) ([]ssmTrigger, error) {
	var f struct {
		Triggers []ssmTrigger `yaml:"triggers"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && err != io.EOF {
		return nil, fmt.Errorf("%s: %w", ssmTriggersFile, err)
	}
	for i, t := range f.Triggers {
		if !ssmTriggerNameRE.MatchString(t.Name) {
			return nil, fmt.Errorf("%s: trigger %d: name %q must be letters, digits and dashes, not starting or ending with a dash, under 64 characters", ssmTriggersFile, i, t.Name)
		}
		switch t.EventType {
		case "", "push", "pull_request":
		default:
			return nil, fmt.Errorf("%s: trigger %s: eventType %q must be push or pull_request", ssmTriggersFile, t.Name, t.EventType)
		}
		for _, re := range []string{t.IncludedGitRefs, t.IgnoredGitRefs, t.IncludedFiles, t.IgnoredFiles} {
			if _, err := regexp.Compile(re); err != nil {
				return nil, fmt.Errorf("%s: trigger %s: %w", ssmTriggersFile, t.Name, err)
			}
		}
		for k := range t.Substitutions {
			if !ssmSubstKeyRE.MatchString(k) && !ssmBuiltinKeys[k] {
				return nil, fmt.Errorf("%s: trigger %s: substitution %q must start with an underscore and use only upper case letters, digits and underscores, or be a built-in", ssmTriggersFile, t.Name, k)
			}
		}
	}
	return f.Triggers, nil
}

// refMatches reports whether a ref filter matches a ref or its short name.
func refMatches(re, ref string) bool {
	x := regexp.MustCompile(re)
	short := strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
	return x.MatchString(ref) || x.MatchString(short)
}

// accepts reports whether a push trigger runs for a ref and its changed
// files; the ignore filters are checked first.
func (t ssmTrigger) accepts(ref string, files []string) bool {
	if t.Disabled || (t.EventType != "" && t.EventType != "push") {
		return false
	}
	if t.IgnoredGitRefs != "" && refMatches(t.IgnoredGitRefs, ref) {
		return false
	}
	if t.IncludedGitRefs != "" && !refMatches(t.IncludedGitRefs, ref) {
		return false
	}
	if t.IgnoredFiles == "" && t.IncludedFiles == "" {
		return true
	}
	ign, inc := regexp.MustCompile(t.IgnoredFiles), regexp.MustCompile(t.IncludedFiles)
	for _, f := range files {
		if t.IgnoredFiles != "" && ign.MatchString(f) {
			continue
		}
		if t.IncludedFiles == "" || inc.MatchString(f) {
			return true
		}
	}
	return false
}

// changedFiles are the files a push from old to sha changed: every file of
// the commit for a new ref.
func changedFiles(ctx context.Context, dir, old, sha string) []string {
	var out []byte
	var err error
	if old == "" {
		out, err = exec.CommandContext(ctx, "git", "-C", dir, "ls-tree", "-r", "--name-only", sha).Output()
	} else {
		out, err = exec.CommandContext(ctx, "git", "-C", dir, "diff", "--name-only", old, sha).Output()
	}
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// runPushTriggers runs the triggers file's push triggers for one ref.
func (m *SecureSourceManagerServer) runPushTriggers(ctx context.Context, builds *CloudBuildServer, repo *ssmpb.Repository, dir, ref, old, sha string) {
	if strings.Trim(sha, "0") == "" || (!strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/")) {
		return // a deleted ref, or not a branch or tag
	}
	raw, err := exec.CommandContext(ctx, "git", "-C", dir, "show", "HEAD:"+ssmTriggersFile).Output()
	if err != nil {
		return // no triggers file on the default branch
	}
	triggers, err := ParseSourceManagerTriggers(raw)
	if err != nil {
		slog.Warn("securesourcemanager: the triggers file does not parse", "repository", repo.GetName(), "err", err)
		return
	}
	files := changedFiles(ctx, dir, old, sha)
	project, location, _ := resource.ParseLocation(strings.SplitN(repo.GetName(), "/repositories/", 2)[0])
	id := repo.GetName()[strings.LastIndex(repo.GetName(), "/")+1:]
	for _, t := range triggers {
		if !t.accepts(ref, files) {
			continue
		}
		if t.ServiceAccount == "" {
			slog.Warn("securesourcemanager: trigger skipped: serviceAccount is required", "repository", repo.GetName(), "trigger", t.Name)
			continue
		}
		if err := m.runPushTrigger(ctx, builds, repo, dir, project, location, id, ref, sha, t); err != nil {
			slog.Warn("securesourcemanager: trigger's build was refused", "repository", repo.GetName(), "trigger", t.Name, "err", err)
		}
	}
}

func (m *SecureSourceManagerServer) runPushTrigger(ctx context.Context, builds *CloudBuildServer, repo *ssmpb.Repository,
	dir, project, location, id, ref, sha string, t ssmTrigger) error {
	config := t.ConfigFilePath
	if config == "" {
		config = ".cloudbuild/cloudbuild.yaml"
	}
	raw, err := exec.CommandContext(ctx, "git", "-C", dir, "show", sha+":"+config).Output()
	if err != nil {
		return fmt.Errorf("config file %s not found at %s", config, sha)
	}
	b, err := parseBuildConfig(raw)
	if err != nil {
		return fmt.Errorf("config file %s: %w", config, err)
	}
	if t.Project != "" {
		project = t.Project
	}
	short := strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
	subs := map[string]string{
		"COMMIT_SHA": sha, "REVISION_ID": sha, "SHORT_SHA": sha[:7], "REPO_NAME": id,
		"REPO_FULL_NAME": repo.GetName(), "REF_NAME": short, "TRIGGER_BUILD_CONFIG_PATH": config,
	}
	if b.Substitutions == nil {
		b.Substitutions = map[string]string{}
	}
	for k, v := range t.Substitutions {
		if strings.HasPrefix(k, "_") {
			b.Substitutions[k] = v
		} else {
			subs[k] = v
		}
	}
	archive := func(ctx context.Context) (io.ReadCloser, error) {
		out, err := exec.CommandContext(ctx, "git", "-C", dir, "archive", "--format=tgz", sha).Output()
		if err != nil {
			return nil, fmt.Errorf("git archive %s: %w", sha, err)
		}
		return io.NopCloser(bytes.NewReader(out)), nil
	}
	src := &cbpb.Source{Source: &cbpb.Source_GitSource{GitSource: &cbpb.GitSource{Url: m.gitURL(repo.GetName()), Revision: sha}}}
	_, err = builds.createBuild(ctx, project, location, false, b,
		&triggerContext{name: t.Name, subs: subs, archive: archive, source: src})
	return err
}

// parseBuildConfig reads a Cloud Build config file, YAML or JSON.
func parseBuildConfig(raw []byte) (*cbpb.Build, error) {
	var v any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	j, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	b := &cbpb.Build{}
	if err := protojson.Unmarshal(j, b); err != nil {
		return nil, err
	}
	return b, nil
}

// ssmJSONPrefix is where Secure Source Manager's JSON API is served on this
// port: its v1 paths (/v1/projects/*/locations/*/repositories/*) are
// Artifact Registry's too, so gcloud's securesourcemanager override and
// Terraform's secure_source_manager_custom_endpoint carry this prefix.
const ssmJSONPrefix = "/securesourcemanager"

// SourceManagerHandlerOr serves Secure Source Manager's git URLs and its
// JSON API (register adds it and the Operations service it polls), and
// passes every other request to next. builds runs push triggers' builds.
func SourceManagerHandlerOr(m *SecureSourceManagerServer, builds *CloudBuildServer, register func(grpc.ServiceRegistrar), next http.Handler) http.Handler {
	t := &rest.Transcoder{Packages: []string{"google.cloud.securesourcemanager.v1"}, Mixins: ssmIAMMixins}
	register(t)
	iampb.RegisterIAMPolicyServer(t, ssmInstanceIAM{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case IsSourceManagerGitPath(r.URL.Path):
			m.serveGit(builds, w, r)
		case strings.HasPrefix(r.URL.Path, ssmJSONPrefix+"/v1/"):
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimPrefix(r.URL.Path, ssmJSONPrefix)
			r2.URL.RawPath = ""
			t.ServeHTTP(w, r2)
		default:
			next.ServeHTTP(w, r)
		}
	})
}
