package dataform

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/robfig/cron/v3"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// ---- npm ----

func (s *Server) InstallNpmPackages(ctx context.Context, req *dfpb.InstallNpmPackagesRequest) (*dfpb.InstallNpmPackagesResponse, error) {
	if req.GetPipelineConfig() != nil {
		return nil, apierror.Unimplemented("pipeline_config is not served by CloudBurrow")
	}
	_, dir, err := s.workspace(req.GetWorkspace())
	if err != nil {
		return nil, err
	}
	if s.opts.Runner == nil {
		return nil, apierror.FailedPrecondition("installing npm packages needs Docker, which this instance cannot run")
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return nil, apierror.FailedPrecondition("the workspace has no package.json")
	}
	var env []string
	m := workspaceRE.FindStringSubmatch(req.GetWorkspace())
	s.mu.Lock()
	secretVersion := ""
	if r, ok := s.repos[m[1]]; ok {
		secretVersion = r.GetNpmrcEnvironmentVariablesSecretVersion()
	}
	s.mu.Unlock()
	if secretVersion != "" {
		b, err := s.secret(ctx, secretVersion)
		if err != nil {
			return nil, apierror.FailedPrecondition("npmrc_environment_variables_secret_version %s: %v", secretVersion, err)
		}
		env = parseEnv(b)
	}
	defer s.lock(dir)()
	if err := s.opts.Runner.InstallPackages(ctx, dir, env); err != nil {
		return nil, apierror.FailedPrecondition("npm install failed: %v", err)
	}
	return &dfpb.InstallNpmPackagesResponse{}, nil
}

// parseEnv reads KEY=VALUE lines, the environment variables the .npmrc
// file refers to.
func parseEnv(b []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, _, ok := strings.Cut(line, "="); ok && k != "" && !strings.ContainsAny(k, " \t") {
			out = append(out, line)
		}
	}
	return out
}

// ---- Release configs ----

func checkCron(cronSchedule, tz string) error {
	if cronSchedule != "" {
		if _, err := cron.ParseStandard(cronSchedule); err != nil {
			return apierror.InvalidArgument("cron_schedule %q: %v", cronSchedule, err)
		}
	}
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return apierror.InvalidArgument("time_zone %q is not an IANA time zone", tz)
		}
	}
	return nil
}

func checkCompilationConfig(c *dfpb.CodeCompilationConfig) error {
	if p := c.GetBuiltinAssertionNamePrefix(); p != "" && !assertionPrefixRE.MatchString(p) {
		return apierror.InvalidArgument("code_compilation_config.builtin_assertion_name_prefix %q: letters, digits and underscores only", p)
	}
	if c.GetDefaultNotebookRuntimeOptions() != nil {
		return apierror.Unimplemented("notebooks are not served by CloudBurrow's Dataform")
	}
	if c.GetPipelineConfig() != nil {
		return apierror.Unimplemented("code_compilation_config.pipeline_config is not served by CloudBurrow")
	}
	for k, v := range c.GetVars() {
		if strings.ContainsAny(k, ",=") || strings.Contains(v, ",") {
			return apierror.InvalidArgument("code_compilation_config.vars %q: names may not contain ',' or '=', nor values ','", k)
		}
	}
	return nil
}

func (s *Server) CreateReleaseConfig(_ context.Context, req *dfpb.CreateReleaseConfigRequest) (*dfpb.ReleaseConfig, error) {
	if err := checkID("release_config_id", req.GetReleaseConfigId()); err != nil {
		return nil, err
	}
	rc := clone(req.GetReleaseConfig())
	if rc == nil || rc.GetGitCommitish() == "" {
		return nil, apierror.InvalidArgument("release_config.git_commitish is required")
	}
	if err := checkCron(rc.GetCronSchedule(), rc.GetTimeZone()); err != nil {
		return nil, err
	}
	if err := checkCompilationConfig(rc.GetCodeCompilationConfig()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.repo(req.GetParent()); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/releaseConfigs/" + req.GetReleaseConfigId()
	if _, ok := s.releases[name]; ok {
		return nil, apierror.AlreadyExists("ReleaseConfig %s already exists", name)
	}
	if err := s.checkReleaseCompilation(req.GetParent(), rc.GetReleaseCompilationResult()); err != nil {
		return nil, err
	}
	rc.Name = name
	rc.RecentScheduledReleaseRecords = nil
	rc.InternalMetadata = nil
	s.releases[name] = rc
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(rc), nil
}

func (s *Server) checkReleaseCompilation(repo, name string) error {
	if name == "" {
		return nil
	}
	m := compilationRE.FindStringSubmatch(name)
	if m == nil || m[1] != repo {
		return apierror.InvalidArgument("release_compilation_result %q must be a compilation result of %s", name, repo)
	}
	if _, ok := s.compilations[name]; !ok {
		return apierror.NotFound("CompilationResult %s not found", name)
	}
	return nil
}

func (s *Server) release(name string) (*dfpb.ReleaseConfig, error) {
	if !releaseRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/*/locations/*/repositories/*/releaseConfigs/*", name)
	}
	rc, ok := s.releases[name]
	if !ok {
		return nil, apierror.NotFound("ReleaseConfig %s not found", name)
	}
	return rc, nil
}

func (s *Server) GetReleaseConfig(_ context.Context, req *dfpb.GetReleaseConfigRequest) (*dfpb.ReleaseConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rc, err := s.release(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(rc), nil
}

func (s *Server) ListReleaseConfigs(_ context.Context, req *dfpb.ListReleaseConfigsRequest) (*dfpb.ListReleaseConfigsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.repo(req.GetParent()); err != nil {
		return nil, err
	}
	keys := childrenOf(s.releases, req.GetParent(), "releaseConfigs")
	start, end, next, err := page("rc:"+req.GetParent(), req.GetPageToken(), req.GetPageSize(), len(keys))
	if err != nil {
		return nil, err
	}
	out := &dfpb.ListReleaseConfigsResponse{NextPageToken: next}
	for _, k := range keys[start:end] {
		out.ReleaseConfigs = append(out.ReleaseConfigs, clone(s.releases[k]))
	}
	return out, nil
}

func (s *Server) UpdateReleaseConfig(_ context.Context, req *dfpb.UpdateReleaseConfigRequest) (*dfpb.ReleaseConfig, error) {
	in := req.GetReleaseConfig()
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.release(in.GetName())
	if err != nil {
		return nil, err
	}
	next := clone(cur)
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"git_commitish", "code_compilation_config", "cron_schedule", "time_zone", "release_compilation_result", "disabled"}
	}
	for _, p := range paths {
		switch p {
		case "git_commitish":
			next.GitCommitish = in.GetGitCommitish()
		case "code_compilation_config":
			next.CodeCompilationConfig = clone(in.GetCodeCompilationConfig())
		case "cron_schedule":
			next.CronSchedule = in.GetCronSchedule()
		case "time_zone":
			next.TimeZone = in.GetTimeZone()
		case "release_compilation_result":
			next.ReleaseCompilationResult = in.GetReleaseCompilationResult()
		case "disabled":
			next.Disabled = in.GetDisabled()
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of ReleaseConfig that can be updated", p)
		}
	}
	if next.GetGitCommitish() == "" {
		return nil, apierror.InvalidArgument("release_config.git_commitish is required")
	}
	if err := checkCron(next.GetCronSchedule(), next.GetTimeZone()); err != nil {
		return nil, err
	}
	if err := checkCompilationConfig(next.GetCodeCompilationConfig()); err != nil {
		return nil, err
	}
	if err := s.checkReleaseCompilation(releaseRE.FindStringSubmatch(next.Name)[1], next.GetReleaseCompilationResult()); err != nil {
		return nil, err
	}
	s.releases[next.Name] = next
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(next), nil
}

func (s *Server) DeleteReleaseConfig(_ context.Context, req *dfpb.DeleteReleaseConfigRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.release(req.GetName()); err != nil {
		return nil, err
	}
	for _, w := range s.workflows {
		if w.GetReleaseConfig() == req.GetName() {
			return nil, apierror.FailedPrecondition("ReleaseConfig %s is used by WorkflowConfig %s", req.GetName(), w.GetName())
		}
	}
	delete(s.releases, req.GetName())
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// ---- Compilation results ----

func (s *Server) CreateCompilationResult(ctx context.Context, req *dfpb.CreateCompilationResultRequest) (*dfpb.CompilationResult, error) {
	in := clone(req.GetCompilationResult())
	if in == nil {
		return nil, apierror.InvalidArgument("compilation_result is required")
	}
	return s.compile(ctx, req.GetParent(), in)
}

func (s *Server) CreateCompilationResultLongRunning(ctx context.Context, req *dfpb.CreateCompilationResultRequest) (*longrunningpb.Operation, error) {
	res, err := s.CreateCompilationResult(ctx, req)
	if err != nil {
		return nil, err
	}
	loc := repositoryRE.FindStringSubmatch(req.GetParent())[1]
	s.mu.Lock()
	defer s.mu.Unlock()
	md := &dfpb.CreateCompilationResultMetadata{Id: res.GetName()[strings.LastIndex(res.GetName(), "/")+1:], Target: res.GetName(), State: dfpb.CreateCompilationResultMetadata_SUCCEEDED, CreateCompilationResultRequest: clone(req)}
	return s.done(loc, md, res)
}

// compile builds a compilation result of a git commitish, a workspace or a
// release config, and stores it.
func (s *Server) compile(ctx context.Context, parent string, c *dfpb.CompilationResult) (*dfpb.CompilationResult, error) {
	s.mu.Lock()
	r, err := s.repo(parent)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	repo := clone(r)
	s.mu.Unlock()
	if c.GetGcsRepositorySnapshotMetadata() != nil {
		return nil, apierror.Unimplemented("Cloud Storage repository snapshots are not served by CloudBurrow")
	}
	cfg := clone(c.GetCodeCompilationConfig())
	var dir, sha string
	switch src := c.GetSource().(type) {
	case *dfpb.CompilationResult_GitCommitish:
		if src.GitCommitish == "" {
			return nil, apierror.InvalidArgument("git_commitish must not be empty")
		}
		dir, sha, err = s.snapshot(ctx, parent, src.GitCommitish)
		if err != nil {
			return nil, err
		}
	case *dfpb.CompilationResult_Workspace:
		m := workspaceRE.FindStringSubmatch(src.Workspace)
		if m == nil || m[1] != parent {
			return nil, apierror.InvalidArgument("workspace %q must be a workspace of %s", src.Workspace, parent)
		}
		_, wdir, err := s.workspace(src.Workspace)
		if err != nil {
			return nil, err
		}
		unlock := s.lock(wdir)
		dir, err = s.copyTree(wdir)
		unlock()
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		// A workspace compiles with the repository's workspace overrides
		// unless the request sets its own.
		if o := repo.GetWorkspaceCompilationOverrides(); o != nil {
			if cfg == nil {
				cfg = &dfpb.CodeCompilationConfig{}
			}
			if cfg.DefaultDatabase == "" {
				cfg.DefaultDatabase = o.GetDefaultDatabase()
			}
			if cfg.SchemaSuffix == "" {
				cfg.SchemaSuffix = o.GetSchemaSuffix()
			}
			if cfg.TablePrefix == "" {
				cfg.TablePrefix = o.GetTablePrefix()
			}
		}
	case *dfpb.CompilationResult_ReleaseConfig:
		s.mu.Lock()
		rc, err := s.release(src.ReleaseConfig)
		if err == nil && !strings.HasPrefix(src.ReleaseConfig, parent+"/") {
			err = apierror.InvalidArgument("release_config %q must be a release config of %s", src.ReleaseConfig, parent)
		}
		var rcc *dfpb.ReleaseConfig
		if err == nil {
			rcc = clone(rc)
		}
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if cfg != nil {
			return nil, apierror.InvalidArgument("code_compilation_config must not be set when compiling a release config, which has its own")
		}
		cfg = rcc.GetCodeCompilationConfig()
		dir, sha, err = s.snapshot(ctx, parent, rcc.GetGitCommitish())
		if err != nil {
			return nil, err
		}
	default:
		return nil, apierror.InvalidArgument("compilation_result needs a source: git_commitish, workspace or release_config")
	}
	defer os.RemoveAll(dir)
	if err := checkCompilationConfig(cfg); err != nil {
		return nil, err
	}
	if s.opts.Runner == nil {
		return nil, apierror.FailedPrecondition("compiling needs Docker, which this instance cannot run")
	}
	project := strings.Split(parent, "/")[1]
	out, err := s.opts.Runner.Compile(ctx, dir, compileFlags(cfg))
	if err != nil {
		return nil, apierror.FailedPrecondition("Dataform core could not compile the project: %v", err)
	}
	g, err := parseGraph(out)
	if err != nil {
		return nil, apierror.FailedPrecondition("Dataform core's output: %v", err)
	}
	g.prefixBuiltinAssertions(cfg.GetBuiltinAssertionNamePrefix())
	c.Name = parent + "/compilationResults/" + newUUID()
	c.CodeCompilationConfig = cfg
	c.ResolvedGitCommitSha = sha
	c.DataformCoreVersion = g.DataformCoreVersion
	c.CreateTime = s.now()
	c.DataEncryptionState, c.InternalMetadata = nil, nil
	c.CompilationErrors = g.errors(project)
	rec := &compilation{C: c, Actions: g.actions(project)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.repos[parent]; !ok {
		return nil, apierror.NotFound("Repository %s not found", parent)
	}
	s.compilations[c.Name] = rec
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(c), nil
}

// compileFlags are @dataform/cli's compile flags for a compilation config.
func compileFlags(c *dfpb.CodeCompilationConfig) []string {
	var f []string
	add := func(flag, v string) {
		if v != "" {
			f = append(f, "--"+flag+"="+v)
		}
	}
	add("default-database", c.GetDefaultDatabase())
	add("default-schema", c.GetDefaultSchema())
	add("default-location", c.GetDefaultLocation())
	add("assertion-schema", c.GetAssertionSchema())
	add("database-suffix", c.GetDatabaseSuffix())
	add("schema-suffix", c.GetSchemaSuffix())
	add("table-prefix", c.GetTablePrefix())
	if len(c.GetVars()) > 0 {
		keys := make([]string, 0, len(c.GetVars()))
		for k := range c.GetVars() {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var kv []string
		for _, k := range keys {
			kv = append(kv, k+"="+c.GetVars()[k])
		}
		f = append(f, "--vars="+strings.Join(kv, ","))
	}
	return f
}

func (s *Server) compilationOf(name string) (*compilation, error) {
	if !compilationRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/*/locations/*/repositories/*/compilationResults/*", name)
	}
	c, ok := s.compilations[name]
	if !ok {
		return nil, apierror.NotFound("CompilationResult %s not found", name)
	}
	return c, nil
}

func (s *Server) GetCompilationResult(_ context.Context, req *dfpb.GetCompilationResultRequest) (*dfpb.CompilationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.compilationOf(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(c.C), nil
}

func (s *Server) ListCompilationResults(_ context.Context, req *dfpb.ListCompilationResultsRequest) (*dfpb.ListCompilationResultsResponse, error) {
	s.mu.Lock()
	if _, err := s.repo(req.GetParent()); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var all []*dfpb.CompilationResult
	for _, k := range childrenOf(s.compilations, req.GetParent(), "compilationResults") {
		all = append(all, clone(s.compilations[k].C))
	}
	s.mu.Unlock()
	all, err := filterBy(all, req.GetFilter(), func(c *dfpb.CompilationResult) map[string]string {
		return map[string]string{"name": c.GetName(), "git_commitish": c.GetGitCommitish(), "workspace": c.GetWorkspace(), "release_config": c.GetReleaseConfig()}
	})
	if err != nil {
		return nil, err
	}
	order := req.GetOrderBy()
	if order == "" {
		order = "create_time desc"
	}
	if err := orderBy(all, order, func(c *dfpb.CompilationResult) map[string]string {
		return map[string]string{"name": c.GetName(), "create_time": c.GetCreateTime().AsTime().Format(time.RFC3339Nano)}
	}); err != nil {
		return nil, err
	}
	start, end, next, err := page("cr:"+req.GetParent()+req.GetFilter()+req.GetOrderBy(), req.GetPageToken(), req.GetPageSize(), len(all))
	if err != nil {
		return nil, err
	}
	return &dfpb.ListCompilationResultsResponse{CompilationResults: all[start:end], NextPageToken: next}, nil
}

func (s *Server) QueryCompilationResultActions(_ context.Context, req *dfpb.QueryCompilationResultActionsRequest) (*dfpb.QueryCompilationResultActionsResponse, error) {
	s.mu.Lock()
	c, err := s.compilationOf(req.GetName())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var all []*dfpb.CompilationResultAction
	for _, a := range c.Actions {
		all = append(all, clone(a))
	}
	s.mu.Unlock()
	all, err = filterBy(all, req.GetFilter(), func(a *dfpb.CompilationResultAction) map[string]string {
		return map[string]string{"file_path": a.GetFilePath(), "target.database": a.GetTarget().GetDatabase(), "target.schema": a.GetTarget().GetSchema(), "target.name": a.GetTarget().GetName()}
	})
	if err != nil {
		return nil, err
	}
	start, end, next, err := page("cra:"+req.GetName()+req.GetFilter(), req.GetPageToken(), req.GetPageSize(), len(all))
	if err != nil {
		return nil, err
	}
	return &dfpb.QueryCompilationResultActionsResponse{CompilationResultActions: all[start:end], NextPageToken: next}, nil
}

// ---- Dataform core's compiled graph ----

type dfTarget struct {
	Database string `json:"database"`
	Schema   string `json:"schema"`
	Name     string `json:"name"`
}

func (t *dfTarget) proto(project string) *dfpb.Target {
	if t == nil {
		return nil
	}
	db := t.Database
	if db == "" {
		db = project
	}
	return &dfpb.Target{Database: db, Schema: t.Schema, Name: t.Name}
}

type dfDescriptor struct {
	Description string `json:"description"`
	Columns     []struct {
		Path               []string `json:"path"`
		Description        string   `json:"description"`
		BigqueryPolicyTags []string `json:"bigqueryPolicyTags"`
	} `json:"columns"`
	BigqueryLabels map[string]string `json:"bigqueryLabels"`
}

func (d *dfDescriptor) proto() *dfpb.RelationDescriptor {
	if d == nil {
		return nil
	}
	out := &dfpb.RelationDescriptor{Description: d.Description, BigqueryLabels: d.BigqueryLabels}
	for _, c := range d.Columns {
		out.Columns = append(out.Columns, &dfpb.RelationDescriptor_ColumnDescriptor{Path: c.Path, Description: c.Description, BigqueryPolicyTags: c.BigqueryPolicyTags})
	}
	return out
}

type dfAction struct {
	Type               string        `json:"type"`
	EnumType           string        `json:"enumType"`
	Materialized       bool          `json:"materialized"`
	Target             *dfTarget     `json:"target"`
	CanonicalTarget    *dfTarget     `json:"canonicalTarget"`
	FileName           string        `json:"fileName"`
	Query              string        `json:"query"`
	IncrementalQuery   string        `json:"incrementalQuery"`
	Queries            []string      `json:"queries"`
	HasOutput          bool          `json:"hasOutput"`
	PreOps             []string      `json:"preOps"`
	PostOps            []string      `json:"postOps"`
	IncrementalPreOps  []string      `json:"incrementalPreOps"`
	IncrementalPostOps []string      `json:"incrementalPostOps"`
	Disabled           bool          `json:"disabled"`
	Protected          bool          `json:"protected"`
	UniqueKey          []string      `json:"uniqueKey"`
	Tags               []string      `json:"tags"`
	DependencyTargets  []*dfTarget   `json:"dependencyTargets"`
	ParentAction       *dfTarget     `json:"parentAction"`
	ActionDescriptor   *dfDescriptor `json:"actionDescriptor"`
	BigQuery           *dfBigQuery   `json:"bigquery"`
}

type dfBigQuery struct {
	PartitionBy             string            `json:"partitionBy"`
	ClusterBy               []string          `json:"clusterBy"`
	PartitionExpirationDays int32             `json:"partitionExpirationDays"`
	RequirePartitionFilter  bool              `json:"requirePartitionFilter"`
	UpdatePartitionFilter   string            `json:"updatePartitionFilter"`
	AdditionalOptions       map[string]string `json:"additionalOptions"`
}

type dfGraph struct {
	Tables           []dfAction `json:"tables"`
	Operations       []dfAction `json:"operations"`
	Assertions       []dfAction `json:"assertions"`
	Declarations     []dfAction `json:"declarations"`
	Notebooks        []dfAction `json:"notebooks"`
	DataPreparations []dfAction `json:"dataPreparations"`
	GraphErrors      struct {
		CompilationErrors []struct {
			FileName     string    `json:"fileName"`
			Message      string    `json:"message"`
			Stack        string    `json:"stack"`
			ActionTarget *dfTarget `json:"actionTarget"`
		} `json:"compilationErrors"`
	} `json:"graphErrors"`
	DataformCoreVersion string `json:"dataformCoreVersion"`
}

func parseGraph(b []byte) (*dfGraph, error) {
	b = bytes.TrimSpace(b)
	if i := bytes.IndexByte(b, '{'); i > 0 {
		b = b[i:]
	}
	var g dfGraph
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, fmt.Errorf("not a compiled graph: %w", err)
	}
	return &g, nil
}

// assertionPrefixRE is what a built-in assertion name prefix may hold: it
// becomes part of a BigQuery view name.
var assertionPrefixRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// prefixBuiltinAssertions is code_compilation_config.builtin_assertion_name_prefix,
// "prefix prepended to auto-generated built-in assertion names" (Dataform
// core's WorkflowSettings.builtinAssertionNamePrefix): every assertion
// Dataform core generated from a table's assertions config (those with a
// parent action) is renamed, and the targets that depend on it follow.
// The pinned CLI has no flag for it, so it is applied to the compiled graph.
func (g *dfGraph) prefixBuiltinAssertions(prefix string) {
	if prefix == "" {
		return
	}
	renamed := map[dfTarget]string{}
	for i := range g.Assertions {
		a := &g.Assertions[i]
		if a.ParentAction == nil || a.Target == nil {
			continue
		}
		old := *a.Target
		renamed[old] = prefix + old.Name
		a.Target = &dfTarget{Database: old.Database, Schema: old.Schema, Name: prefix + old.Name}
		if ct := a.CanonicalTarget; ct != nil {
			renamed[*ct] = prefix + ct.Name
			a.CanonicalTarget = &dfTarget{Database: ct.Database, Schema: ct.Schema, Name: prefix + ct.Name}
		}
	}
	if len(renamed) == 0 {
		return
	}
	for _, list := range [][]dfAction{g.Tables, g.Operations, g.Assertions, g.Notebooks, g.DataPreparations} {
		for i := range list {
			for j, t := range list[i].DependencyTargets {
				if t == nil {
					continue
				}
				if n, ok := renamed[*t]; ok {
					list[i].DependencyTargets[j] = &dfTarget{Database: t.Database, Schema: t.Schema, Name: n}
				}
			}
		}
	}
}

func (g *dfGraph) errors(project string) []*dfpb.CompilationResult_CompilationError {
	var out []*dfpb.CompilationResult_CompilationError
	for _, e := range g.GraphErrors.CompilationErrors {
		out = append(out, &dfpb.CompilationResult_CompilationError{Message: e.Message, Stack: e.Stack, Path: e.FileName, ActionTarget: e.ActionTarget.proto(project)})
	}
	return out
}

func targets(ts []*dfTarget, project string) []*dfpb.Target {
	var out []*dfpb.Target
	for _, t := range ts {
		out = append(out, t.proto(project))
	}
	return out
}

func (g *dfGraph) actions(project string) []*dfpb.CompilationResultAction {
	var out []*dfpb.CompilationResultAction
	base := func(a dfAction) *dfpb.CompilationResultAction {
		ct := a.CanonicalTarget
		if ct == nil {
			ct = a.Target
		}
		return &dfpb.CompilationResultAction{Target: a.Target.proto(project), CanonicalTarget: ct.proto(project), FilePath: a.FileName}
	}
	for _, a := range g.Tables {
		x := base(a)
		rel := &dfpb.CompilationResultAction_Relation{
			DependencyTargets: targets(a.DependencyTargets, project), Disabled: a.Disabled, Tags: a.Tags,
			RelationDescriptor: a.ActionDescriptor.proto(), SelectQuery: a.Query, PreOperations: a.PreOps, PostOperations: a.PostOps,
		}
		switch {
		case strings.EqualFold(a.EnumType, "VIEW") || a.Type == "view":
			rel.RelationType = dfpb.CompilationResultAction_Relation_VIEW
			if a.Materialized {
				rel.RelationType = dfpb.CompilationResultAction_Relation_MATERIALIZED_VIEW
			}
		case strings.EqualFold(a.EnumType, "INCREMENTAL") || a.Type == "incremental":
			rel.RelationType = dfpb.CompilationResultAction_Relation_INCREMENTAL_TABLE
			rel.IncrementalTableConfig = &dfpb.CompilationResultAction_Relation_IncrementalTableConfig{
				IncrementalSelectQuery: a.IncrementalQuery, RefreshDisabled: a.Protected, UniqueKeyParts: a.UniqueKey,
				IncrementalPreOperations: a.IncrementalPreOps, IncrementalPostOperations: a.IncrementalPostOps,
			}
		default:
			rel.RelationType = dfpb.CompilationResultAction_Relation_TABLE
		}
		if bq := a.BigQuery; bq != nil {
			rel.PartitionExpression = bq.PartitionBy
			rel.ClusterExpressions = bq.ClusterBy
			rel.PartitionExpirationDays = bq.PartitionExpirationDays
			rel.RequirePartitionFilter = bq.RequirePartitionFilter
			rel.AdditionalOptions = bq.AdditionalOptions
			if rel.IncrementalTableConfig != nil {
				rel.IncrementalTableConfig.UpdatePartitionFilter = bq.UpdatePartitionFilter
			}
		}
		x.CompiledObject = &dfpb.CompilationResultAction_Relation_{Relation: rel}
		out = append(out, x)
	}
	for _, a := range g.Operations {
		x := base(a)
		x.CompiledObject = &dfpb.CompilationResultAction_Operations_{Operations: &dfpb.CompilationResultAction_Operations{
			DependencyTargets: targets(a.DependencyTargets, project), Disabled: a.Disabled, Tags: a.Tags,
			RelationDescriptor: a.ActionDescriptor.proto(), Queries: a.Queries, HasOutput: a.HasOutput,
		}}
		out = append(out, x)
	}
	for _, a := range g.Assertions {
		x := base(a)
		x.CompiledObject = &dfpb.CompilationResultAction_Assertion_{Assertion: &dfpb.CompilationResultAction_Assertion{
			DependencyTargets: targets(a.DependencyTargets, project), ParentAction: a.ParentAction.proto(project), Disabled: a.Disabled, Tags: a.Tags,
			SelectQuery: a.Query, RelationDescriptor: a.ActionDescriptor.proto(),
		}}
		out = append(out, x)
	}
	for _, a := range g.Declarations {
		x := base(a)
		x.CompiledObject = &dfpb.CompilationResultAction_Declaration_{Declaration: &dfpb.CompilationResultAction_Declaration{RelationDescriptor: a.ActionDescriptor.proto()}}
		out = append(out, x)
	}
	for _, a := range g.Notebooks {
		x := base(a)
		x.CompiledObject = &dfpb.CompilationResultAction_Notebook_{Notebook: &dfpb.CompilationResultAction_Notebook{DependencyTargets: targets(a.DependencyTargets, project), Disabled: a.Disabled, Tags: a.Tags}}
		out = append(out, x)
	}
	for _, a := range g.DataPreparations {
		x := base(a)
		x.CompiledObject = &dfpb.CompilationResultAction_DataPreparation_{DataPreparation: &dfpb.CompilationResultAction_DataPreparation{DependencyTargets: targets(a.DependencyTargets, project), Disabled: a.Disabled, Tags: a.Tags}}
		out = append(out, x)
	}
	sort.SliceStable(out, func(i, j int) bool { return targetKey(out[i].GetTarget()) < targetKey(out[j].GetTarget()) })
	return out
}

func targetKey(t *dfpb.Target) string {
	return t.GetDatabase() + "." + t.GetSchema() + "." + t.GetName()
}

// ---- schedules ----

// RunSchedules creates the compilation results and invocations whose cron
// schedules are due, until ctx ends: release configs compile their git
// commitish, workflow configs invoke their release's latest compilation.
func (s *Server) RunSchedules(ctx context.Context, tick time.Duration) {
	last := s.opts.Now()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := s.opts.Now()
		s.fireDue(ctx, last, now)
		s.evaluateTriggers(ctx, now)
		last = now
	}
}

func due(cronSchedule, tz string, from, to time.Time) bool {
	if cronSchedule == "" {
		return false
	}
	spec := cronSchedule
	if tz != "" {
		spec = "CRON_TZ=" + tz + " " + cronSchedule
	}
	sch, err := cron.ParseStandard(spec)
	if err != nil {
		return false
	}
	next := sch.Next(from)
	return !next.After(to)
}

func (s *Server) fireDue(ctx context.Context, from, to time.Time) {
	s.mu.Lock()
	var releases []*dfpb.ReleaseConfig
	for _, k := range sortedKeys(s.releases) {
		rc := s.releases[k]
		if !rc.GetDisabled() && due(rc.GetCronSchedule(), rc.GetTimeZone(), from, to) {
			releases = append(releases, clone(rc))
		}
	}
	var workflows []*dfpb.WorkflowConfig
	for _, k := range sortedKeys(s.workflows) {
		w := s.workflows[k]
		if !w.GetDisabled() && due(w.GetCronSchedule(), w.GetTimeZone(), from, to) {
			workflows = append(workflows, clone(w))
		}
	}
	s.mu.Unlock()
	for _, rc := range releases {
		repo := releaseRE.FindStringSubmatch(rc.GetName())[1]
		// A scheduled release reads the repository's data, as a call does.
		var res *dfpb.CompilationResult
		err := s.checkKey(ctx, repo)
		if err == nil {
			res, err = s.compile(ctx, repo, &dfpb.CompilationResult{Source: &dfpb.CompilationResult_ReleaseConfig{ReleaseConfig: rc.GetName()}})
		}
		rec := &dfpb.ReleaseConfig_ScheduledReleaseRecord{ReleaseTime: timestamppb.New(to)}
		if err != nil {
			rec.Result = &dfpb.ReleaseConfig_ScheduledReleaseRecord_ErrorStatus{ErrorStatus: apierror.From(err).GRPCStatus().Proto()}
		} else {
			rec.Result = &dfpb.ReleaseConfig_ScheduledReleaseRecord_CompilationResult{CompilationResult: res.GetName()}
		}
		s.mu.Lock()
		if cur, ok := s.releases[rc.GetName()]; ok {
			if err == nil {
				cur.ReleaseCompilationResult = res.GetName()
			}
			cur.RecentScheduledReleaseRecords = append([]*dfpb.ReleaseConfig_ScheduledReleaseRecord{rec}, cur.RecentScheduledReleaseRecords...)
			if len(cur.RecentScheduledReleaseRecords) > 10 {
				cur.RecentScheduledReleaseRecords = cur.RecentScheduledReleaseRecords[:10]
			}
			_ = s.save()
		}
		s.mu.Unlock()
	}
	for _, w := range workflows {
		repo := workflowRE.FindStringSubmatch(w.GetName())[1]
		var inv *dfpb.WorkflowInvocation
		err := s.checkKey(ctx, repo)
		if err == nil {
			inv, err = s.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo,
				WorkflowInvocation: &dfpb.WorkflowInvocation{CompilationSource: &dfpb.WorkflowInvocation_WorkflowConfig{WorkflowConfig: w.GetName()}}})
		}
		rec := &dfpb.WorkflowConfig_ScheduledExecutionRecord{ExecutionTime: timestamppb.New(to)}
		if err != nil {
			rec.Result = &dfpb.WorkflowConfig_ScheduledExecutionRecord_ErrorStatus{ErrorStatus: apierror.From(err).GRPCStatus().Proto()}
		} else {
			rec.Result = &dfpb.WorkflowConfig_ScheduledExecutionRecord_WorkflowInvocation{WorkflowInvocation: inv.GetName()}
		}
		s.mu.Lock()
		if cur, ok := s.workflows[w.GetName()]; ok {
			cur.RecentScheduledExecutionRecords = append([]*dfpb.WorkflowConfig_ScheduledExecutionRecord{rec}, cur.RecentScheduledExecutionRecords...)
			if len(cur.RecentScheduledExecutionRecords) > 10 {
				cur.RecentScheduledExecutionRecords = cur.RecentScheduledExecutionRecords[:10]
			}
			_ = s.save()
		}
		s.mu.Unlock()
	}
}
