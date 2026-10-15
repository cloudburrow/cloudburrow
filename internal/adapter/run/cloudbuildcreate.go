package run

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"regexp"
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
// zip in the instance's Cloud Storage and whose steps are docker build,
// docker push and pack, run through the same local builder as SubmitBuild.
//
// Arbitrary step containers are refused: Cloud Build runs every step on one
// VM with the host's Docker socket, the cloudbuild network and a /workspace
// volume each step mounts in turn, and a step's container may itself call
// docker. A step run as a Kubernetes Job in the instance's cluster would see
// none of that (no docker daemon, no cloudbuild network, a /workspace that
// lives only as long as a PersistentVolume shared across nodes), so the
// builds that work on Google would fail or differ here. They are refused,
// naming the step, rather than run differently.

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
	switch {
	case len(st.GetSecretEnv()) > 0:
		return refuse("secretEnv is not supported: secrets come from Secret Manager, which Cloud Build reads on Google")
	case len(st.GetVolumes()) > 0:
		return refuse("volumes are not supported: only /workspace is shared between steps here")
	case st.GetScript() != "":
		return refuse("script runs in the step's container, which is not run here; only docker build, docker push and pack steps are")
	case st.GetAllowFailure() || len(st.GetAllowExitCodes()) > 0:
		return refuse("allowFailure and allowExitCodes are not supported")
	case len(st.GetEnv()) > 0 && !packBuilders[st.GetName()]:
		return refuse("env is not supported on a docker step: the step's container, where it would be set, is not run here")
	}
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
	name := st.GetName()
	if (name == "gcr.io/cloud-builders/gcb-internal" || name == "gcr.io/cloud-builders/gcb-internal:latest") &&
		len(args) > 0 && args[0] == "docker" {
		// `gcloud builds submit --tag`'s step: Cloud Build's own builder
		// image running docker, args[0] the command.
		name, args = "gcr.io/cloud-builders/docker", args[1:]
	}
	switch {
	case dockerBuilders[name] && (st.GetEntrypoint() == "" || st.GetEntrypoint() == "docker"):
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
		default:
			return refuse("docker %s is not supported: only docker build and docker push run here", args[0])
		}
	case packBuilders[name] && (st.GetEntrypoint() == "" || st.GetEntrypoint() == "pack"):
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
	default:
		return refuse("only docker build, docker push (%s) and pack (%s) steps run here; "+
			"an arbitrary step's container would run without the Docker socket, the cloudbuild network "+
			"and the /workspace volume Cloud Build gives it", "gcr.io/cloud-builders/docker", "gcr.io/k8s-skaffold/pack")
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
	add(len(b.GetSecrets()) > 0, "secrets")
	add(b.GetAvailableSecrets() != nil, "availableSecrets")
	add(b.GetArtifacts() != nil, "artifacts")
	add(b.GetServiceAccount() != "", "serviceAccount")
	add(b.GetGitConfig() != nil, "gitConfig")
	if o := b.GetOptions(); o != nil {
		add(len(o.GetSourceProvenanceHash()) > 0, "options.sourceProvenanceHash")
		add(o.GetMachineType() != cbpb.BuildOptions_UNSPECIFIED, "options.machineType")
		add(o.GetDiskSizeGb() != 0, "options.diskSizeGb")
		add(o.GetWorkerPool() != "" || o.GetPool() != nil, "options.pool")
		add(len(o.GetEnv()) > 0, "options.env")
		add(len(o.GetSecretEnv()) > 0, "options.secretEnv")
		add(len(o.GetVolumes()) > 0, "options.volumes")
		add(o.GetDynamicSubstitutions(), "options.dynamicSubstitutions")
		add(o.GetDefaultLogsBucketBehavior() != cbpb.BuildOptions_DEFAULT_LOGS_BUCKET_BEHAVIOR_UNSPECIFIED, "options.defaultLogsBucketBehavior")
	}
	if len(fields) == 0 {
		return nil
	}
	return apierror.Unimplemented("Build field %s is not supported: it needs Google Cloud Build's own workers or "+
		"Secret Manager; builds run locally with Docker or pack", strings.Join(fields, ", "))
}

// CreateBuild starts a build and returns its operation at once, as Cloud
// Build does: QUEUED, then WORKING while its steps run in order, then
// SUCCESS, FAILURE, TIMEOUT or CANCELLED.
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
	in := req.GetBuild()
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
	plan := make([]plannedStep, len(in.GetSteps()))
	ids := map[string]bool{}
	for i, st := range in.GetSteps() {
		for _, w := range st.GetWaitFor() {
			// Steps run one after another, which satisfies any waitFor
			// naming an earlier step, and "-" (start at once).
			if w != "-" && !ids[w] {
				return nil, apierror.InvalidArgument("invalid build: step %d waits for %q, which is not an earlier step's id", i, w)
			}
		}
		if st.GetId() != "" {
			if ids[st.GetId()] {
				return nil, apierror.InvalidArgument("invalid build: duplicate step id %q", st.GetId())
			}
			ids[st.GetId()] = true
		}
		p, err := planStep(i, st, subs)
		if err != nil {
			return nil, err
		}
		if (p.kind == stepDockerBuild || p.kind == stepPack) && src == nil {
			return nil, apierror.InvalidArgument("invalid build: step %d builds /workspace, but the build has no source", i)
		}
		plan[i] = p
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
		st.Status, st.Timing, st.PullTiming = cbpb.Build_QUEUED, nil, nil
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
	queued := proto.Clone(b).(*cbpb.Build)

	opName := parent + "/operations/" + id
	if req.GetParent() == "" {
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
	r := &buildRun{c: c, name: b.Name, op: op.Name, project: project, plan: plan, src: src.GetStorageSource(),
		logsBucket: b.LogsBucket, createBucket: createBucket}
	go r.run(bctx, cancel)
	return s.toProtoOperation(op.Name)
}

// buildRun is one CreateBuild build running.
type buildRun struct {
	c            *CloudBuildServer
	name, op     string
	project      string
	plan         []plannedStep
	src          *cbpb.StorageSource
	logsBucket   string
	createBucket bool
	mu           sync.Mutex
	log          bytes.Buffer
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
		w.r.log.Write(w.pending[:i+1])
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
	fmt.Fprintf(&r.log, format, a...)
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
	r.update(func(b *cbpb.Build) { b.Status, b.StartTime = cbpb.Build_WORKING, timestamppb.New(start) })
	r.logf("starting build %q\n\nFETCHSOURCE\n", id)
	if r.src != nil {
		r.logf("Fetching storage object: gs://%s/%s#%d\n", r.src.GetBucket(), r.src.GetObject(), r.src.GetGeneration())
	}
	r.logf("BUILD\n")
	r.flushLog(ctx)
	built := map[string]string{} // tag -> local image
	digests := map[string]string{}
	var stepImages []string
	failed := func(status cbpb.Build_Status, step int, detail string) {
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
	for i, p := range r.plan {
		if st, done := ctxStatus(); done {
			failed(st, -1, "build "+strings.ToLower(st.String()))
			return
		}
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
		sctx, scancel := ctx, context.CancelFunc(func() {})
		if p.timeout > 0 {
			sctx, scancel = context.WithTimeout(ctx, p.timeout)
		}
		w := &stepWriter{r: r, prefix: label + ": "}
		err := r.runStep(sctx, p, w, built)
		w.flush()
		stepTimedOut := p.timeout > 0 && errors.Is(sctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		scancel()
		if err != nil {
			r.logf("Finished %s\n", label)
			if st, done := ctxStatus(); done {
				failed(st, i, "build "+strings.ToLower(st.String()))
				return
			}
			if stepTimedOut {
				failed(cbpb.Build_TIMEOUT, i, fmt.Sprintf("build step %d %q timed out", i, cur.GetSteps()[i].GetName()))
				return
			}
			w.prefix = label + ": "
			_, _ = w.Write([]byte(err.Error() + "\n"))
			failed(cbpb.Build_FAILURE, i, fmt.Sprintf("build step %d %q failed: %v", i, cur.GetSteps()[i].GetName(), firstLine(err.Error())))
			return
		}
		stepEnd := time.Now()
		digest := ""
		if len(p.tags) > 0 {
			digest = buildDigest(id + "/" + strconv.Itoa(i))
			for _, t := range p.tags {
				digests[tagged(t)] = digest
			}
		}
		stepImages = append(stepImages, digest)
		r.logf("Finished %s\n", label)
		r.update(func(b *cbpb.Build) {
			b.Steps[i].Status, b.Steps[i].Timing.EndTime = cbpb.Build_SUCCESS, timestamppb.New(stepEnd)
		})
		r.flushLog(ctx)
	}
	buildEnd := time.Now()
	r.logf("PUSH\n")
	cur := updateCloudBuild(r.name, func(*cbpb.Build) {})
	var results []*cbpb.BuiltImage
	for _, im := range cur.GetImages() {
		local, ok := built[tagged(im)]
		if !ok {
			failed(cbpb.Build_FAILURE, -1, fmt.Sprintf("failed to push image %q: no step built it", im))
			return
		}
		r.logf("Pushing %s\n%s: digest: %s\n", im, tagged(im), digests[tagged(im)])
		localBuilds.record(untagged(im)+"@"+digests[tagged(im)], local)
		pt := time.Now()
		results = append(results, &cbpb.BuiltImage{Name: tagged(im), Digest: digests[tagged(im)], PushTiming: span(pt, pt)})
	}
	end := time.Now()
	r.logf("DONE\n")
	r.flushLog(ctx)
	done := r.update(func(b *cbpb.Build) {
		b.Status, b.FinishTime = cbpb.Build_SUCCESS, timestamppb.New(end)
		b.Results = &cbpb.Results{Images: results, BuildStepImages: stepImages}
		b.Timing = map[string]*cbpb.TimeSpan{"BUILD": span(start, buildEnd), "PUSH": span(buildEnd, end)}
	})
	_ = r.c.builds.s.ops.Succeed(r.op, done)
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// runStep runs one planned step; built maps each tag a step built to its
// local image.
func (r *buildRun) runStep(ctx context.Context, p plannedStep, w *stepWriter, built map[string]string) error {
	switch p.kind {
	case stepDockerPush:
		local, ok := built[tagged(p.push)]
		if !ok {
			return fmt.Errorf("An image does not exist locally with the tag: %s", p.push)
		}
		// The image is loaded into the instance's cluster, where Cloud
		// Run runs it; there is no registry to push it to.
		fmt.Fprintf(w, "The push refers to repository [%s]\n", untagged(p.push))
		fmt.Fprintf(w, "%s: loaded into the cluster as %s\n", tagged(p.push), local)
		return nil
	case stepDockerBuild, stepPack:
		rc, err := r.c.builds.fetch.Fetch(ctx, r.src.GetBucket(), r.src.GetObject(), r.src.GetGeneration())
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
		for _, t := range p.tags {
			built[tagged(t)] = spec.LocalImage
			localBuilds.record(t, spec.LocalImage)
		}
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
