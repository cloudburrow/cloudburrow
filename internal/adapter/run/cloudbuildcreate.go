package run

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// CreateBuild and CancelBuild (#1347): a Build whose source is a tar.gz or
// zip in the instance's Cloud Storage. Its docker build, docker push and pack
// steps run through the same local builder as SubmitBuild.
//
// Any other step (#1382) runs as Google's build worker runs it: a container
// on the host's Docker, with the Docker socket mounted, on a user-defined
// network named cloudbuild, with the build's /workspace volume as its working
// directory and a /builder/home volume every step shares. The builder runs
// them when it implements StepRunner; the CLI's does.

// StepRunner runs a build's arbitrary steps as containers on the host's
// Docker (#1382). The ImageBuilder the CLI wires implements it.
type StepRunner interface {
	// OpenWorkspace makes the build's /workspace, a Docker volume holding the
	// unpacked source archive (nil: empty), and the cloudbuild network.
	OpenWorkspace(ctx context.Context, buildID string, archive io.Reader) (BuildWorkspace, error)
}

// BuildWorkspace is one build's /workspace and the volumes its steps share.
type BuildWorkspace interface {
	// RunStep runs one step's container to completion. A non-zero exit is an
	// error naming the status, as Cloud Build's log does.
	RunStep(ctx context.Context, spec StepSpec) error
	// Archive is /workspace as a gzipped tarball, which a docker build or
	// pack step builds: it sees what earlier steps wrote.
	Archive(ctx context.Context) (io.ReadCloser, error)
	// Close removes the build's containers, volumes and, when no other
	// build uses it, the cloudbuild network.
	Close()
}

// StepSpec is one arbitrary step, substituted and validated.
type StepSpec struct {
	// Index is the step's position, for the container's name.
	Index      int
	Image      string
	Entrypoint string
	Args, Env  []string
	// Dir is the working directory relative to /workspace; empty is
	// /workspace itself.
	Dir     string
	Volumes []StepVolume
	// Script is the step's script (#1407): the runner writes it, mode
	// 0755, to ScriptPath in the container before it starts, and
	// Entrypoint and Args run it.
	Script string
	// Secrets are the step's secretEnv variables, name to value (#1407).
	// The runner must pass them without putting a value on a command line
	// or in a log.
	Secrets map[string]string
	Log     io.Writer
}

// ScriptPath is where a step's script is written in its container.
const ScriptPath = "/builder/script"

// StepVolume is a named volume a step mounts at Path; steps naming the same
// volume share it for the build.
type StepVolume struct{ Name, Path string }

// ObjectWriter writes a build log to the instance's Cloud Storage. The
// SourceFetcher the CLI wires implements it; without it, a build's log is
// kept only in its record.
type ObjectWriter interface {
	// Put writes bucket/object; with create, a missing bucket is made in
	// project first.
	Put(ctx context.Context, project, bucket, object string, data []byte, create bool) error
}

// Cloud builder images whose steps are run locally.
var (
	dockerBuilders = map[string]bool{"gcr.io/cloud-builders/docker": true, "docker": true,
		"gcr.io/cloud-builders/docker:latest": true, "docker:latest": true}
	packBuilders = map[string]bool{"gcr.io/k8s-skaffold/pack": true, "gcr.io/k8s-skaffold/pack:latest": true,
		"buildpacksio/pack": true, "buildpacksio/pack:latest": true}
)

// defaultBuildTimeout is Cloud Build's default build timeout, ten minutes.
const defaultBuildTimeout = 10 * time.Minute

type stepKind int

const (
	stepDockerBuild stepKind = iota + 1
	stepDockerPush
	stepPack
	stepContainer
)

// plannedStep is one validated step and what running it does.
type plannedStep struct {
	kind       stepKind
	tags       []string // docker build -t, or pack's image
	push       string   // docker push's image
	dockerfile string   // relative to the source root
	context    string   // relative to the source root
	buildArgs  []string
	env        map[string]string // pack --env
	timeout    time.Duration
	step       StepSpec // stepContainer
	// allowFailure and allowExitCodes (#1407): a failure they allow is
	// the step's FAILURE, not the build's.
	allowFailure   bool
	allowExitCodes []int32
	// secretEnv names the secrets the step gets (stepContainer).
	secretEnv []string
}

// allows reports whether the step's failure err leaves the build going:
// allowExitCodes, when set, takes precedence over allowFailure, and allows
// only an exit with one of its codes.
func (p plannedStep) allows(err error) bool {
	if len(p.allowExitCodes) > 0 {
		var exit *StepExitError
		if !errors.As(err, &exit) {
			return false
		}
		for _, c := range p.allowExitCodes {
			if int(c) == exit.Code {
				return true
			}
		}
		return false
	}
	return p.allowFailure
}

// runningBuilds holds the cancel function of each build CreateBuild started.
var runningBuilds = struct {
	mu     sync.Mutex
	cancel map[string]context.CancelFunc
}{cancel: map[string]context.CancelFunc{}}

var substRE = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// substitute applies Cloud Build's substitutions to s: $VAR and ${VAR} for
// the built-in and user (_NAME) substitutions; $$ is a literal $. A
// variable with no substitution is left as written, as Cloud Build does
// for environment variables a step's shell expands.
func substitute(s string, subs map[string]string) string {
	return substRE.ReplaceAllStringFunc(s, func(m string) string {
		if m == "$$" {
			return "$"
		}
		k := strings.Trim(m, "${}")
		if v, ok := subs[k]; ok {
			return v
		}
		return m
	})
}

// cleanRel validates a path inside /workspace and returns it relative to
// the source root.
func cleanRel(dir, p string) (string, error) {
	if strings.HasPrefix(p, "/workspace/") || p == "/workspace" {
		p = strings.TrimPrefix(strings.TrimPrefix(p, "/workspace"), "/")
	} else if !path.IsAbs(p) {
		p = path.Join(dir, p)
	} else {
		return "", fmt.Errorf("path %q is outside /workspace", p)
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("path %q is outside /workspace", p)
	}
	return c, nil
}

// planStep validates one step and plans it, or refuses it with the reason.
func planStep(i int, st *cbpb.BuildStep, subs map[string]string) (plannedStep, error) {
	refuse := func(format string, a ...any) (plannedStep, error) {
		return plannedStep{}, apierror.Unimplemented("step %d (%s): "+format, append([]any{i, st.GetName()}, a...)...)
	}
	if st.GetScript() != "" && (len(st.GetArgs()) > 0 || st.GetEntrypoint() != "") {
		return plannedStep{}, apierror.InvalidArgument("invalid build: step %d: script cannot be used with args or entrypoint", i)
	}
	for _, c := range st.GetAllowExitCodes() {
		if c < 0 || c > 255 {
			return plannedStep{}, apierror.InvalidArgument("invalid build: step %d: allowExitCodes value %d is not between 0 and 255", i, c)
		}
	}
	p, err := planStepKind(i, st, subs, refuse)
	if err != nil {
		return p, err
	}
	p.allowFailure, p.allowExitCodes = st.GetAllowFailure(), st.GetAllowExitCodes()
	return p, nil
}

// planStepKind plans the step as a local docker build, docker push or pack
// step, or an arbitrary container.
func planStepKind(i int, st *cbpb.BuildStep, subs map[string]string, refuse func(string, ...any) (plannedStep, error)) (plannedStep, error) {
	args := make([]string, len(st.GetArgs()))
	for j, a := range st.GetArgs() {
		args[j] = substitute(a, subs)
	}
	dir := substitute(st.GetDir(), subs)
	if dir != "" {
		c, err := cleanRel("", dir)
		if err != nil {
			return refuse("dir: %v", err)
		}
		dir = c
	}
	p := plannedStep{}
	if t := st.GetTimeout(); t != nil {
		p.timeout = t.AsDuration()
	}
	name := substitute(st.GetName(), subs)
	if (name == "gcr.io/cloud-builders/gcb-internal" || name == "gcr.io/cloud-builders/gcb-internal:latest") &&
		len(args) > 0 && args[0] == "docker" {
		// `gcloud builds submit --tag`'s step: Cloud Build's own builder
		// image running docker, args[0] the command.
		name, args = "gcr.io/cloud-builders/docker", args[1:]
	}
	local := st.GetScript() == "" && (dockerBuilders[name] && (st.GetEntrypoint() == "" || st.GetEntrypoint() == "docker") && len(args) > 0 &&
		(args[0] == "build" || args[0] == "push")) || (packBuilders[name] && (st.GetEntrypoint() == "" || st.GetEntrypoint() == "pack"))
	if local {
		// The local builder runs these steps itself, not in the step's
		// container, so what only that container would see is refused.
		switch {
		case len(st.GetVolumes()) > 0:
			return refuse("volumes are not supported on a docker build, docker push or pack step: the local builder runs it, not the step's container")
		case len(st.GetEnv()) > 0 && !packBuilders[name]:
			return refuse("env is not supported on a docker build or push step: the local builder runs it, not the step's container")
		case len(st.GetSecretEnv()) > 0:
			return refuse("secretEnv is not supported on a docker build, docker push or pack step: the local builder runs it, " +
				"not the step's container; run docker in a step with entrypoint bash, as Cloud Build's examples do")
		}
	}
	switch {
	case !local:
		return planContainer(i, st, name, args, dir, subs)
	case dockerBuilders[name]:
		if len(args) == 0 {
			return refuse("a docker step needs args")
		}
		switch args[0] {
		case "build":
			p.kind = stepDockerBuild
			var positional []string
			for j := 1; j < len(args); j++ {
				a := args[j]
				flag, val, hasVal := strings.Cut(a, "=")
				next := func() (string, bool) {
					if hasVal {
						return val, true
					}
					if j+1 < len(args) {
						j++
						return args[j], true
					}
					return "", false
				}
				switch flag {
				case "-t", "--tag":
					v, ok := next()
					if !ok {
						return refuse("%s needs a value", flag)
					}
					p.tags = append(p.tags, v)
				case "-f", "--file":
					v, ok := next()
					if !ok {
						return refuse("%s needs a value", flag)
					}
					c, err := cleanRel(dir, v)
					if err != nil {
						return refuse("%s: %v", flag, err)
					}
					p.dockerfile = c
				case "--build-arg":
					v, ok := next()
					if !ok {
						return refuse("--build-arg needs a value")
					}
					p.buildArgs = append(p.buildArgs, v)
				case "--network", "--cache-from":
					// The cloudbuild network, and a cache that only
					// speeds the build: the build is the same without.
					if _, ok := next(); !ok {
						return refuse("%s needs a value", flag)
					}
				case "--no-cache", "--pull", "-q", "--quiet":
				default:
					if strings.HasPrefix(a, "-") {
						return refuse("docker build flag %s is not supported", flag)
					}
					positional = append(positional, a)
				}
			}
			if len(positional) != 1 {
				return refuse("docker build needs one context directory, got %q", positional)
			}
			if len(p.tags) == 0 {
				return refuse("docker build needs -t: an untagged image cannot be pushed or deployed")
			}
			c, err := cleanRel(dir, positional[0])
			if err != nil {
				return refuse("context: %v", err)
			}
			p.context = c
			if p.dockerfile == "" {
				p.dockerfile = path.Join(c, "Dockerfile")
			}
		case "push":
			if len(args) != 2 || strings.HasPrefix(args[1], "-") {
				return refuse("docker push needs exactly one image, got %q", args[1:])
			}
			p.kind, p.push = stepDockerPush, args[1]
		}
	case packBuilders[name]:
		if len(args) < 2 || args[0] != "build" || strings.HasPrefix(args[1], "-") {
			return refuse("a pack step must be pack build IMAGE")
		}
		p.kind, p.tags, p.context, p.env = stepPack, []string{args[1]}, dir, map[string]string{}
		for _, e := range st.GetEnv() {
			k, v, _ := strings.Cut(substitute(e, subs), "=")
			p.env[k] = v
		}
		for j := 2; j < len(args); j++ {
			flag, val, hasVal := strings.Cut(args[j], "=")
			next := func() (string, bool) {
				if hasVal {
					return val, true
				}
				if j+1 < len(args) {
					j++
					return args[j], true
				}
				return "", false
			}
			switch flag {
			case "--builder", "-B":
				v, ok := next()
				if !ok || !strings.HasPrefix(v, "gcr.io/buildpacks/builder") {
					return refuse("pack --builder %q is not supported: builds use Google's gcr.io/buildpacks/builder", v)
				}
			case "--network":
				if _, ok := next(); !ok {
					return refuse("--network needs a value")
				}
			case "--env", "-e":
				v, ok := next()
				if !ok {
					return refuse("--env needs a value")
				}
				k, ev, _ := strings.Cut(v, "=")
				p.env[k] = ev
			case "--path", "-p":
				v, ok := next()
				if !ok {
					return refuse("--path needs a value")
				}
				c, err := cleanRel(dir, v)
				if err != nil {
					return refuse("--path: %v", err)
				}
				p.context = c
			case "--publish", "--no-color", "--quiet", "-q", "--trust-builder":
			default:
				return refuse("pack flag %s is not supported", flag)
			}
		}
	}
	return p, nil
}

// reservedStepPaths are the mounts Cloud Build gives every step, which a
// step's own volume may not replace.
var reservedStepPaths = map[string]bool{"/workspace": true, "/builder/home": true, "/var/run/docker.sock": true}

var volumeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// planContainer plans an arbitrary step, run in its own container.
func planContainer(i int, st *cbpb.BuildStep, name string, args []string, dir string, subs map[string]string) (plannedStep, error) {
	if name == "" {
		return plannedStep{}, apierror.InvalidArgument("invalid build: step %d has no name", i)
	}
	env := make([]string, 0, len(st.GetEnv()))
	for _, e := range st.GetEnv() {
		e = substitute(e, subs)
		if k, _, ok := strings.Cut(e, "="); !ok || k == "" {
			return plannedStep{}, apierror.InvalidArgument("invalid build: step %d env %q is not KEY=VALUE", i, e)
		}
		env = append(env, e)
	}
	var vols []StepVolume
	seen := map[string]bool{}
	for _, v := range st.GetVolumes() {
		p := path.Clean(v.GetPath())
		switch {
		case !volumeNameRE.MatchString(v.GetName()):
			return plannedStep{}, apierror.InvalidArgument("invalid build: step %d volume name %q is not valid", i, v.GetName())
		case !path.IsAbs(v.GetPath()):
			return plannedStep{}, apierror.InvalidArgument("invalid build: step %d volume path %q must be absolute", i, v.GetPath())
		case reservedStepPaths[p] || strings.HasPrefix(p, "/workspace/"):
			return plannedStep{}, apierror.InvalidArgument("invalid build: step %d volume path %q is reserved", i, v.GetPath())
		case seen[p] || seen["name:"+v.GetName()]:
			return plannedStep{}, apierror.InvalidArgument("invalid build: step %d mounts volume %q or path %q twice", i, v.GetName(), p)
		}
		seen[p], seen["name:"+v.GetName()] = true, true
		vols = append(vols, StepVolume{Name: v.GetName(), Path: p})
	}
	p := plannedStep{kind: stepContainer, secretEnv: st.GetSecretEnv(), step: StepSpec{Index: i, Image: name,
		Entrypoint: substitute(st.GetEntrypoint(), subs), Args: args, Env: env, Dir: dir, Volumes: vols}}
	if sc := st.GetScript(); sc != "" {
		// Written to a file and run: by its #! line's interpreter, else
		// by /bin/sh, as Cloud Build does. Substitutions are not applied
		// to a script; automapSubstitutions gives them as variables.
		p.step.Script = sc
		if strings.HasPrefix(sc, "#!") {
			p.step.Entrypoint, p.step.Args = ScriptPath, nil
		} else {
			p.step.Entrypoint, p.step.Args = "/bin/sh", []string{ScriptPath}
		}
	}
	if t := st.GetTimeout(); t != nil {
		p.timeout = t.AsDuration()
	}
	return p, nil
}

// unsupportedCreate names the first Build field CreateBuild cannot honour.
func unsupportedCreate(b *cbpb.Build) error {
	var fields []string
	add := func(set bool, f string) {
		if set {
			fields = append(fields, f)
		}
	}
	add(b.GetArtifacts() != nil, "artifacts")
	add(b.GetServiceAccount() != "", "serviceAccount")
	add(b.GetGitConfig() != nil, "gitConfig")
	if o := b.GetOptions(); o != nil {
		add(len(o.GetSourceProvenanceHash()) > 0, "options.sourceProvenanceHash")
		add(o.GetMachineType() != cbpb.BuildOptions_UNSPECIFIED, "options.machineType")
		add(o.GetDiskSizeGb() != 0, "options.diskSizeGb")
		add(o.GetWorkerPool() != "" || o.GetPool() != nil, "options.pool")
		add(len(o.GetVolumes()) > 0, "options.volumes")
		add(o.GetDynamicSubstitutions(), "options.dynamicSubstitutions")
		add(o.GetDefaultLogsBucketBehavior() != cbpb.BuildOptions_DEFAULT_LOGS_BUCKET_BEHAVIOR_UNSPECIFIED, "options.defaultLogsBucketBehavior")
	}
	if len(fields) == 0 {
		return nil
	}
	return apierror.Unimplemented("Build field %s is not supported: it needs Google Cloud Build's own workers; "+
		"builds run locally with Docker or pack", strings.Join(fields, ", "))
}

// CreateBuild starts a build and returns its operation at once, as Cloud
// Build does: QUEUED, then WORKING while its steps run in the order their
// waitFor sets, then SUCCESS, FAILURE, TIMEOUT or CANCELLED.
func (c *CloudBuildServer) CreateBuild(ctx context.Context, req *cbpb.CreateBuildRequest) (*longrunning.Operation, error) {
	project, location := req.GetProjectId(), "global"
	if req.GetParent() != "" {
		p, l, err := resource.ParseLocation(req.GetParent())
		if err != nil {
			return nil, apierror.InvalidArgument("%v", err)
		}
		if project != "" && project != p {
			return nil, apierror.InvalidArgument("project_id %q does not match parent %q", project, req.GetParent())
		}
		project, location = p, l
	}
	if project == "" {
		return nil, apierror.InvalidArgument("project_id is required")
	}
	return c.createBuild(ctx, project, location, req.GetParent() == "", req.GetBuild())
}

// createdBuilds keeps each CreateBuild build's request, by build name, which
// RetryBuild runs again.
var createdBuilds = struct {
	mu    sync.Mutex
	input map[string]*cbpb.Build
}{input: map[string]*cbpb.Build{}}

// Retryable reports whether RetryBuild can rerun the build of that name:
// whether CreateBuild created it.
func Retryable(name string) bool {
	createdBuilds.mu.Lock()
	defer createdBuilds.mu.Unlock()
	_, ok := createdBuilds.input[name]
	return ok
}

// RetryBuild creates a new build, with a new ID, from the request a build
// was created with: its source, steps, substitutions and options, as Cloud
// Build does. A Cloud Storage source without a generation is read again, so
// the new build may see a newer object. A build CreateBuild did not create,
// one SubmitBuild ran for a source deploy or a function, is refused: its
// request was Cloud Run's, not Cloud Build's.
func (c *CloudBuildServer) RetryBuild(ctx context.Context, req *cbpb.RetryBuildRequest) (*longrunning.Operation, error) {
	b, err := c.GetBuild(ctx, &cbpb.GetBuildRequest{Name: req.GetName(), ProjectId: req.GetProjectId(), Id: req.GetId()})
	if err != nil {
		return nil, err
	}
	createdBuilds.mu.Lock()
	in, ok := createdBuilds.input[b.GetName()]
	createdBuilds.mu.Unlock()
	if !ok {
		return nil, apierror.FailedPrecondition("build %s cannot be retried: it was run by Cloud Run's SubmitBuild, "+
			"for a source deploy or a function, not created by CreateBuild; deploy again to rebuild it", b.GetId())
	}
	_, location, _ := resource.ParseLocation(strings.SplitN(b.GetName(), "/builds/", 2)[0])
	return c.createBuild(ctx, b.GetProjectId(), location, req.GetName() == "", proto.Clone(in).(*cbpb.Build))
}

// createBuild validates and starts a build in project and location; global
// names its operation in the global form.
func (c *CloudBuildServer) createBuild(ctx context.Context, project, location string, global bool, in *cbpb.Build) (*longrunning.Operation, error) {
	if in == nil {
		return nil, apierror.InvalidArgument("build is required")
	}
	if len(in.GetSteps()) == 0 {
		return nil, apierror.InvalidArgument("invalid build: build must have at least one step")
	}
	if err := unsupportedCreate(in); err != nil {
		return nil, err
	}
	src := in.GetSource()
	if src != nil && src.GetStorageSource() == nil {
		return nil, apierror.Unimplemented("Build source %T is not supported: only storageSource, an archive in this instance's Cloud Storage, is", src.GetSource())
	}
	if src != nil && (src.GetStorageSource().GetBucket() == "" || src.GetStorageSource().GetObject() == "") {
		return nil, apierror.InvalidArgument("source.storageSource.bucket and source.storageSource.object are required")
	}
	if c.builds == nil || c.builds.builder == nil {
		return nil, apierror.FailedPrecondition("CreateBuild has no local builder on this instance")
	}
	if src != nil && c.builds.fetch == nil {
		return nil, apierror.FailedPrecondition("CreateBuild reads its source from Cloud Storage, which is not enabled on this instance: start it with --services run,storage")
	}
	for k := range in.GetSubstitutions() {
		if !strings.HasPrefix(k, "_") {
			return nil, apierror.InvalidArgument("invalid build: substitution key %q must begin with an underscore", k)
		}
	}
	timeout := defaultBuildTimeout
	if t := in.GetTimeout(); t != nil {
		if t.AsDuration() <= 0 {
			return nil, apierror.InvalidArgument("invalid build: timeout must be positive")
		}
		timeout = t.AsDuration()
	}

	id := uuid.NewString()
	parent := "projects/" + project + "/locations/" + location
	number := strconv.FormatInt(resource.ProjectNumber(project), 10)
	subs := map[string]string{"PROJECT_ID": project, "PROJECT_NUMBER": number, "BUILD_ID": id, "LOCATION": location}
	if s := src.GetStorageSource(); s != nil {
		subs["_SOURCE_BUCKET"] = s.GetBucket()
	}
	for k, v := range in.GetSubstitutions() {
		subs[k] = v
	}
	for _, e := range in.GetOptions().GetEnv() {
		if k, _, ok := strings.Cut(substitute(e, subs), "="); !ok || k == "" {
			return nil, apierror.InvalidArgument("invalid build: options.env %q is not KEY=VALUE", e)
		}
	}
	secretDefs, err := c.planSecrets(in, subs)
	if err != nil {
		return nil, err
	}
	plan := make([]plannedStep, len(in.GetSteps()))
	deps := make([][]int, len(in.GetSteps()))
	ids := map[string]int{}
	containers, firstContainer := false, 0
	for i, st := range in.GetSteps() {
		switch w := st.GetWaitFor(); {
		case len(w) == 0:
			// A step with no waitFor waits for every step before it.
			for j := 0; j < i; j++ {
				deps[i] = append(deps[i], j)
			}
		case len(w) == 1 && w[0] == "-":
			// "-": start when the build starts.
		default:
			for _, name := range w {
				j, ok := ids[name]
				if !ok {
					return nil, apierror.InvalidArgument("invalid build: step %d waits for %q, which is not an earlier step's id", i, name)
				}
				deps[i] = append(deps[i], j)
			}
		}
		if st.GetId() != "" {
			if _, dup := ids[st.GetId()]; dup {
				return nil, apierror.InvalidArgument("invalid build: duplicate step id %q", st.GetId())
			}
			ids[st.GetId()] = i
		}
		p, err := planStep(i, st, subs)
		if err != nil {
			return nil, err
		}
		if p.kind == stepContainer {
			if !containers {
				firstContainer = i
			}
			containers = true
			// The step's own automapSubstitutions, true or false, wins
			// over the build's.
			automap := in.GetOptions().GetAutomapSubstitutions()
			if st.AutomapSubstitutions != nil {
				automap = st.GetAutomapSubstitutions()
			}
			// Docker's last -e wins: the substitutions, then options.env,
			// then the step's env, which Cloud Build lets override both.
			var env []string
			if automap {
				keys := make([]string, 0, len(subs))
				for k := range subs {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					env = append(env, k+"="+subs[k])
				}
			}
			for _, e := range in.GetOptions().GetEnv() {
				env = append(env, substitute(e, subs))
			}
			p.step.Env = append(env, p.step.Env...)
			p.secretEnv = mergeNames(in.GetOptions().GetSecretEnv(), p.secretEnv)
		}
		plan[i] = p
	}
	var runner StepRunner
	if containers {
		// Refused before anything runs (#1382): the step gets the host's
		// Docker socket.
		if err := c.hostStepsRefusal(ctx, firstContainer, plan[firstContainer].step.Image); err != nil {
			return nil, err
		}
		var ok bool
		if runner, ok = c.builds.builder.(StepRunner); !ok {
			return nil, apierror.Unimplemented("this instance's builder runs only docker build, docker push and pack steps; " +
				"an arbitrary step needs the host's Docker, which runs it")
		}
	}
	for i, p := range plan {
		if (p.kind == stepDockerBuild || p.kind == stepPack) && src == nil && !containers {
			return nil, apierror.InvalidArgument("invalid build: step %d builds /workspace, but the build has no source", i)
		}
	}

	b := proto.Clone(in).(*cbpb.Build)
	b.Name, b.Id, b.ProjectId, b.Status = parent+"/builds/"+id, id, project, cbpb.Build_QUEUED
	b.CreateTime, b.StartTime, b.FinishTime, b.Results, b.StatusDetail, b.FailureInfo = timestamppb.Now(), nil, nil, nil, "", nil
	b.Timeout = durationpb.New(timeout)
	if b.QueueTtl == nil {
		b.QueueTtl = durationpb.New(time.Hour)
	}
	if b.Substitutions == nil && len(in.GetSubstitutions()) > 0 {
		b.Substitutions = in.GetSubstitutions()
	}
	for i, st := range b.Steps {
		st.Status, st.Timing, st.PullTiming, st.ExitCode = cbpb.Build_QUEUED, nil, nil, 0
		st.Args = make([]string, len(in.GetSteps()[i].GetArgs()))
		for j, a := range in.GetSteps()[i].GetArgs() {
			st.Args[j] = substitute(a, subs)
		}
	}
	for i, im := range b.Images {
		b.Images[i] = substitute(im, subs)
	}
	logging := b.GetOptions().GetLogging()
	writeLogs := logging != cbpb.BuildOptions_NONE && logging != cbpb.BuildOptions_CLOUD_LOGGING_ONLY
	createBucket := false
	if writeLogs && b.LogsBucket == "" {
		// Cloud Build's default logs bucket, which Google owns; here it is
		// made in the instance's Cloud Storage on the first log.
		b.LogsBucket, createBucket = "gs://"+number+".cloudbuild-logs.googleusercontent.com", true
	}
	if !writeLogs {
		b.LogsBucket = ""
	}
	if BuildLogURLBase != "" {
		b.LogUrl = strings.TrimSuffix(BuildLogURLBase, "/") + "/run/builds?project=" + project + "&location=" + location + "&id=" + id
	}
	cloudBuilds.mu.Lock()
	cloudBuilds.builds[b.Name] = b
	cloudBuilds.mu.Unlock()
	createdBuilds.mu.Lock()
	createdBuilds.input[b.Name] = proto.Clone(in).(*cbpb.Build)
	createdBuilds.mu.Unlock()
	queued := proto.Clone(b).(*cbpb.Build)

	opName := parent + "/operations/" + id
	if global {
		// The global form's operation, operations/build/{project}/{base64 id}.
		opName = "operations/build/" + project + "/" + base64.StdEncoding.EncodeToString([]byte(id))
	}
	s := c.builds.s
	op := s.ops.CreateNamed(opName, b.Name)
	_ = s.ops.SetMetadata(op.Name, &cbpb.BuildOperationMetadata{Build: queued})

	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	runningBuilds.mu.Lock()
	runningBuilds.cancel[b.Name] = cancel
	runningBuilds.mu.Unlock()
	r := &buildRun{c: c, name: b.Name, op: op.Name, project: project, plan: plan, deps: deps, src: src.GetStorageSource(),
		runner: runner, logsBucket: b.LogsBucket, createBucket: createBucket, secretDefs: secretDefs}
	go r.run(bctx, cancel)
	return s.toProtoOperation(op.Name)
}

// buildRun is one CreateBuild build running.
type buildRun struct {
	c            *CloudBuildServer
	name, op     string
	project      string
	plan         []plannedStep
	deps         [][]int // each step's waitFor, as step indexes
	src          *cbpb.StorageSource
	runner       StepRunner     // nil: no arbitrary steps
	ws           BuildWorkspace // the build's /workspace, with runner
	logsBucket   string
	createBucket bool
	mu           sync.Mutex
	log          bytes.Buffer
	built        map[string]string // tag -> local image, under mu
	// secretDefs are the build's secrets (#1407), read at build time into
	// secrets, whose values mask hides in the log; under mu.
	secretDefs map[string]buildSecret
	secrets    map[string]string
	mask       *strings.Replacer
}

// mergeNames is a then b, without repeats.
func mergeNames(a, b []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range append(append([]string(nil), a...), b...) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// stepWriter prefixes each line a step writes, as Cloud Build's log does.
type stepWriter struct {
	r       *buildRun
	prefix  string
	pending []byte
}

func (w *stepWriter) Write(p []byte) (int, error) {
	w.r.mu.Lock()
	defer w.r.mu.Unlock()
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		w.r.log.WriteString(w.prefix)
		w.r.log.WriteString(w.r.masked(string(w.pending[:i+1])))
		w.pending = w.pending[i+1:]
	}
	return len(p), nil
}

func (w *stepWriter) flush() {
	if len(w.pending) > 0 {
		_, _ = w.Write([]byte("\n"))
	}
}

func (r *buildRun) logf(format string, a ...any) {
	r.mu.Lock()
	r.log.WriteString(r.masked(fmt.Sprintf(format, a...)))
	r.mu.Unlock()
}

// buildLogs holds each CreateBuild build's log, for the console.
var buildLogs = struct {
	mu   sync.Mutex
	logs map[string]string
}{logs: map[string]string{}}

// BuildLog is a build's log so far, by its name, and whether it has one.
func BuildLog(name string) (string, bool) {
	buildLogs.mu.Lock()
	defer buildLogs.mu.Unlock()
	l, ok := buildLogs.logs[name]
	return l, ok
}

// flushLog records the log so far and writes it to the logs bucket,
// log-{id}.txt, as Cloud Build does.
func (r *buildRun) flushLog(ctx context.Context) {
	r.mu.Lock()
	data := append([]byte(nil), r.log.Bytes()...)
	r.mu.Unlock()
	buildLogs.mu.Lock()
	buildLogs.logs[r.name] = string(data)
	buildLogs.mu.Unlock()
	w, ok := r.c.builds.fetch.(ObjectWriter)
	if r.logsBucket == "" || !ok {
		return
	}
	bucket, prefix, _ := strings.Cut(strings.TrimPrefix(r.logsBucket, "gs://"), "/")
	id := r.name[strings.LastIndex(r.name, "/")+1:]
	object := path.Join(prefix, "log-"+id+".txt")
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = w.Put(wctx, r.project, bucket, object, data, r.createBucket)
}

func (r *buildRun) update(f func(*cbpb.Build)) *cbpb.Build {
	b := updateCloudBuild(r.name, f)
	_ = r.c.builds.s.ops.SetMetadata(r.op, &cbpb.BuildOperationMetadata{Build: b})
	return b
}

func span(start, end time.Time) *cbpb.TimeSpan {
	return &cbpb.TimeSpan{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)}
}

func (r *buildRun) run(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	defer func() {
		runningBuilds.mu.Lock()
		delete(runningBuilds.cancel, r.name)
		runningBuilds.mu.Unlock()
	}()
	id := r.name[strings.LastIndex(r.name, "/")+1:]
	start := time.Now()
	r.built = map[string]string{}
	r.update(func(b *cbpb.Build) { b.Status, b.StartTime = cbpb.Build_WORKING, timestamppb.New(start) })
	r.logf("starting build %q\n\nFETCHSOURCE\n", id)
	if r.src != nil {
		r.logf("Fetching storage object: gs://%s/%s#%d\n", r.src.GetBucket(), r.src.GetObject(), r.src.GetGeneration())
	}
	var once sync.Once
	failed := func(status cbpb.Build_Status, step int, detail string) {
		once.Do(func() {
			end := time.Now()
			r.logf("ERROR: %s\n", detail)
			r.flushLog(ctx)
			r.update(func(b *cbpb.Build) {
				b.Status, b.FinishTime, b.StatusDetail = status, timestamppb.New(end), detail
				if step >= 0 {
					b.Steps[step].Status = status
					if b.Steps[step].Timing == nil {
						b.Steps[step].Timing = span(end, end)
					} else {
						b.Steps[step].Timing.EndTime = timestamppb.New(end)
					}
				}
				// A step that was running when the build stopped is
				// CANCELLED; one that never started stays QUEUED.
				for i, st := range b.Steps {
					if i != step && st.Status == cbpb.Build_WORKING {
						st.Status = cbpb.Build_CANCELLED
						st.Timing.EndTime = timestamppb.New(end)
					}
				}
				if status == cbpb.Build_FAILURE {
					b.FailureInfo = &cbpb.Build_FailureInfo{Type: cbpb.Build_FailureInfo_USER_BUILD_STEP, Detail: detail}
				}
				b.Timing = map[string]*cbpb.TimeSpan{"BUILD": span(start, end)}
			})
			var err error
			switch status {
			case cbpb.Build_CANCELLED:
				err = &apierror.Error{Code: codes.Canceled, Message: "Build was cancelled"}
			case cbpb.Build_TIMEOUT:
				err = &apierror.Error{Code: codes.DeadlineExceeded, Message: "Build timed out"}
			default:
				err = &apierror.Error{Code: codes.Unknown, Message: "Build failed; check build logs for details"}
			}
			_ = r.c.builds.s.ops.Fail(r.op, err)
		})
	}
	ctxStatus := func() (cbpb.Build_Status, bool) {
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			return cbpb.Build_CANCELLED, true
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return cbpb.Build_TIMEOUT, true
		}
		return 0, false
	}
	if r.runner != nil {
		var archive io.Reader
		if r.src != nil {
			rc, err := r.c.builds.fetch.Fetch(ctx, r.src.GetBucket(), r.src.GetObject(), r.src.GetGeneration())
			if err != nil {
				failed(cbpb.Build_FAILURE, -1, "failed to fetch source: "+firstLine(err.Error()))
				return
			}
			defer rc.Close()
			archive = rc
		}
		ws, err := r.runner.OpenWorkspace(ctx, id, archive)
		if err != nil {
			if st, done := ctxStatus(); done {
				failed(st, -1, "build "+strings.ToLower(st.String()))
				return
			}
			failed(cbpb.Build_FAILURE, -1, "failed to prepare /workspace: "+firstLine(err.Error()))
			return
		}
		r.ws = ws
		defer ws.Close()
	}
	if err := r.resolveSecrets(ctx); err != nil {
		if st, done := ctxStatus(); done {
			failed(st, -1, "build "+strings.ToLower(st.String()))
			return
		}
		failed(cbpb.Build_FAILURE, -1, err.Error())
		return
	}
	r.logf("BUILD\n")
	r.flushLog(ctx)

	// Each step starts once the steps it waits for succeed; the first
	// failure stops the others, as on Cloud Build.
	stepsCtx, stopSteps := context.WithCancel(ctx)
	defer stopSteps()
	done := make([]chan struct{}, len(r.plan))
	ok := make([]bool, len(r.plan))
	for i := range done {
		done[i] = make(chan struct{})
	}
	digests := make([]string, len(r.plan))
	var wg sync.WaitGroup
	for i := range r.plan {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer close(done[i])
			for _, d := range r.deps[i] {
				select {
				case <-done[d]:
				case <-stepsCtx.Done():
					return
				}
				if !ok[d] {
					return
				}
			}
			if stepsCtx.Err() != nil {
				return
			}
			p := r.plan[i]
			label := fmt.Sprintf("Step #%d", i)
			cur := updateCloudBuild(r.name, func(*cbpb.Build) {})
			if sid := cur.GetSteps()[i].GetId(); sid != "" {
				label += fmt.Sprintf(" - %q", sid)
			}
			stepStart := time.Now()
			r.update(func(b *cbpb.Build) {
				b.Steps[i].Status, b.Steps[i].Timing = cbpb.Build_WORKING, &cbpb.TimeSpan{StartTime: timestamppb.New(stepStart)}
			})
			r.logf("Starting %s\n", label)
			sctx, scancel := stepsCtx, context.CancelFunc(func() {})
			if p.timeout > 0 {
				sctx, scancel = context.WithTimeout(stepsCtx, p.timeout)
			}
			w := &stepWriter{r: r, prefix: label + ": "}
			err := r.runStep(sctx, p, w)
			w.flush()
			stepTimedOut := p.timeout > 0 && errors.Is(sctx.Err(), context.DeadlineExceeded) && stepsCtx.Err() == nil
			scancel()
			r.logf("Finished %s\n", label)
			if err != nil && stepsCtx.Err() == nil && !stepTimedOut && p.allows(err) {
				// allowFailure or allowExitCodes: the step is FAILURE
				// with its exit code, and the build goes on.
				_, _ = w.Write([]byte(err.Error() + "\n"))
				stepEnd := time.Now()
				ok[i] = true
				r.update(func(b *cbpb.Build) {
					b.Steps[i].Status, b.Steps[i].Timing.EndTime = cbpb.Build_FAILURE, timestamppb.New(stepEnd)
					var exit *StepExitError
					if errors.As(err, &exit) {
						b.Steps[i].ExitCode = int32(exit.Code)
					}
				})
				r.flushLog(ctx)
				return
			}
			if err != nil {
				defer stopSteps()
				if st, isDone := ctxStatus(); isDone {
					failed(st, i, "build "+strings.ToLower(st.String()))
					return
				}
				if stepsCtx.Err() != nil {
					return // another step failed first
				}
				if stepTimedOut {
					failed(cbpb.Build_TIMEOUT, i, fmt.Sprintf("build step %d %q timed out", i, cur.GetSteps()[i].GetName()))
					return
				}
				_, _ = w.Write([]byte(err.Error() + "\n"))
				var exit *StepExitError
				if errors.As(err, &exit) {
					r.update(func(b *cbpb.Build) { b.Steps[i].ExitCode = int32(exit.Code) })
				}
				failed(cbpb.Build_FAILURE, i, fmt.Sprintf("build step %d %q failed: %v", i, cur.GetSteps()[i].GetName(), firstLine(err.Error())))
				return
			}
			stepEnd := time.Now()
			if len(p.tags) > 0 {
				digests[i] = buildDigest(id + "/" + strconv.Itoa(i))
			}
			ok[i] = true
			r.update(func(b *cbpb.Build) {
				b.Steps[i].Status, b.Steps[i].Timing.EndTime = cbpb.Build_SUCCESS, timestamppb.New(stepEnd)
			})
			r.flushLog(ctx)
		}(i)
	}
	wg.Wait()
	if st, isDone := ctxStatus(); isDone {
		failed(st, -1, "build "+strings.ToLower(st.String()))
		return
	}
	for i := range ok {
		if !ok[i] {
			return // failed has recorded it
		}
	}
	tagDigest := map[string]string{}
	for i, p := range r.plan {
		for _, t := range p.tags {
			tagDigest[tagged(t)] = digests[i]
		}
	}
	buildEnd := time.Now()
	r.logf("PUSH\n")
	cur := updateCloudBuild(r.name, func(*cbpb.Build) {})
	var results []*cbpb.BuiltImage
	for _, im := range cur.GetImages() {
		r.mu.Lock()
		local, found := r.built[tagged(im)]
		r.mu.Unlock()
		if !found {
			failed(cbpb.Build_FAILURE, -1, fmt.Sprintf("failed to push image %q: no step built it", im))
			return
		}
		r.logf("Pushing %s\n%s: digest: %s\n", im, tagged(im), tagDigest[tagged(im)])
		localBuilds.record(untagged(im)+"@"+tagDigest[tagged(im)], local)
		pt := time.Now()
		results = append(results, &cbpb.BuiltImage{Name: tagged(im), Digest: tagDigest[tagged(im)], PushTiming: span(pt, pt)})
	}
	end := time.Now()
	r.logf("DONE\n")
	r.flushLog(ctx)
	final := r.update(func(b *cbpb.Build) {
		b.Status, b.FinishTime = cbpb.Build_SUCCESS, timestamppb.New(end)
		b.Results = &cbpb.Results{Images: results, BuildStepImages: digests}
		b.Timing = map[string]*cbpb.TimeSpan{"BUILD": span(start, buildEnd), "PUSH": span(buildEnd, end)}
	})
	_ = r.c.builds.s.ops.Succeed(r.op, final)
}

// StepExitError is an arbitrary step's non-zero exit, which the build
// records as the step's exitCode.
type StepExitError struct{ Code int }

func (e *StepExitError) Error() string {
	return fmt.Sprintf("step exited with non-zero status: %d", e.Code)
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// runStep runs one planned step.
func (r *buildRun) runStep(ctx context.Context, p plannedStep, w *stepWriter) error {
	switch p.kind {
	case stepContainer:
		spec := p.step
		spec.Log = w
		if len(p.secretEnv) > 0 {
			r.mu.Lock()
			spec.Secrets = make(map[string]string, len(p.secretEnv))
			for _, n := range p.secretEnv {
				spec.Secrets[n] = r.secrets[n]
			}
			r.mu.Unlock()
		}
		return r.ws.RunStep(ctx, spec)
	case stepDockerPush:
		r.mu.Lock()
		local, ok := r.built[tagged(p.push)]
		r.mu.Unlock()
		if !ok {
			return fmt.Errorf("An image does not exist locally with the tag: %s", p.push)
		}
		// The image is loaded into the instance's cluster, where Cloud
		// Run runs it; there is no registry to push it to.
		fmt.Fprintf(w, "The push refers to repository [%s]\n", untagged(p.push))
		fmt.Fprintf(w, "%s: loaded into the cluster as %s\n", tagged(p.push), local)
		return nil
	case stepDockerBuild, stepPack:
		var rc io.ReadCloser
		var err error
		if r.ws != nil {
			// What earlier steps wrote to /workspace is built.
			rc, err = r.ws.Archive(ctx)
		} else {
			rc, err = r.c.builds.fetch.Fetch(ctx, r.src.GetBucket(), r.src.GetObject(), r.src.GetGeneration())
		}
		if err != nil {
			return err
		}
		defer rc.Close()
		spec := BuildSpec{Archive: rc, LocalImage: localImageFor(p.tags[0]), Context: p.context, Log: w}
		if p.kind == stepDockerBuild {
			spec.Kind, spec.Dockerfile, spec.BuildArgs = BuildDocker, p.dockerfile, p.buildArgs
		} else {
			spec.Kind, spec.Env, spec.FunctionTarget = BuildBuildpacks, p.env, p.env["GOOGLE_FUNCTION_TARGET"]
		}
		if err := r.c.builds.builder.Build(ctx, spec); err != nil {
			return err
		}
		r.mu.Lock()
		for _, t := range p.tags {
			r.built[tagged(t)] = spec.LocalImage
			localBuilds.record(t, spec.LocalImage)
		}
		r.mu.Unlock()
		fmt.Fprintf(w, "Successfully tagged %s\n", strings.Join(p.tags, ", "))
		return nil
	}
	return fmt.Errorf("unknown step")
}

// CancelBuild cancels a build that has not finished; a finished build is
// returned as it is.
func (c *CloudBuildServer) CancelBuild(ctx context.Context, req *cbpb.CancelBuildRequest) (*cbpb.Build, error) {
	b, err := c.GetBuild(ctx, &cbpb.GetBuildRequest{Name: req.GetName(), ProjectId: req.GetProjectId(), Id: req.GetId()})
	if err != nil {
		return nil, err
	}
	runningBuilds.mu.Lock()
	cancel, ok := runningBuilds.cancel[b.GetName()]
	runningBuilds.mu.Unlock()
	if !ok {
		return b, nil
	}
	cancel()
	// The build's goroutine records CANCELLED; wait for it briefly so the
	// answer carries it, as Cloud Build's does.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		cur, err := c.GetBuild(ctx, &cbpb.GetBuildRequest{Name: b.GetName()})
		if err == nil && cur.GetStatus() != cbpb.Build_QUEUED && cur.GetStatus() != cbpb.Build_WORKING {
			return cur, nil
		}
	}
	return c.GetBuild(ctx, &cbpb.GetBuildRequest{Name: b.GetName()})
}
