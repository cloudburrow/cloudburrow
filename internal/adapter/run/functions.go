package run

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/api/annotations"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// FunctionsServer serves google.cloud.functions.v2.FunctionService (#1377)
// as Google builds a 2nd-gen function: a Cloud Build build of the source
// with Google's buildpacks (SubmitBuild's buildpacks path, #1268), then a
// Cloud Run service running the image, whose name service_config.service
// reports. A function is stored in this process, like Artifact Registry's
// repositories; the service is the cluster's Knative Service.
//
// An event trigger is an Eventarc trigger (#1414, eventarc.go), made once
// the service is deployed, which delivers CloudEvents to the service.
type FunctionsServer struct {
	functionspb.UnimplementedFunctionServiceServer
	s      *Server
	builds *BuildsServer
	// base is the Run port's URL, for the upload and download URLs; it is
	// read at each call, since the port can be OS-assigned.
	base func() string
	// events serves event triggers; nil refuses them.
	events *Events
}

// Functions returns the Cloud Functions v2 service, which builds through
// builds and deploys through s. base returns the URL clients reach this
// port at.
func (s *Server) Functions(builds *BuildsServer, base func() string) *FunctionsServer {
	return &FunctionsServer{s: s, builds: builds, base: base}
}

// Register adds FunctionService, its IAM policy mixin and its Locations
// mixin to a gRPC server or the JSON transcoder.
func (f *FunctionsServer) Register(g grpc.ServiceRegistrar) {
	functionspb.RegisterFunctionServiceServer(g, f)
	iampb.RegisterIAMPolicyServer(g, functionsIAM{f})
	locations.Register(g, functionsLocations{})
}

type storedFunction struct {
	fn     *functionspb.Function
	policy *iampolicy.Stored
	// trigger is what the function's event trigger made, nil without one.
	trigger *liveTrigger
}

// functions is process-wide, like repositories.
var functions = struct {
	mu  sync.Mutex
	fns map[string]*storedFunction
}{fns: map[string]*storedFunction{}}

func parseFunctionName(name string) (parent, id string, err error) {
	i := strings.LastIndex(name, "/functions/")
	if i < 0 {
		return "", "", apierror.InvalidArgument("invalid function name %q: want projects/*/locations/*/functions/*", name)
	}
	parent, id = name[:i], name[i+len("/functions/"):]
	if _, _, err := resource.ParseLocation(parent); err != nil || id == "" || strings.Contains(id, "/") {
		return "", "", apierror.InvalidArgument("invalid function name %q: want projects/*/locations/*/functions/*", name)
	}
	return parent, id, nil
}

func cloneTrigger(t *functionspb.EventTrigger) *functionspb.EventTrigger {
	if t == nil {
		return nil
	}
	return proto.Clone(t).(*functionspb.EventTrigger)
}

func cloneFunction(fn *functionspb.Function) *functionspb.Function {
	return proto.Clone(fn).(*functionspb.Function)
}

func functionNotFound(name string) error {
	return apierror.NotFound("Resource '%s' was not found", name)
}

// Functions lists the stored functions of a project, by name, for the
// console. An empty project lists every project's.
func Functions(project string) []*functionspb.Function {
	functions.mu.Lock()
	defer functions.mu.Unlock()
	var out []*functionspb.Function
	for name, sf := range functions.fns {
		if project == "" || strings.HasPrefix(name, "projects/"+project+"/") {
			out = append(out, cloneFunction(sf.fn))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GetFunction reads a function.
func (f *FunctionsServer) GetFunction(_ context.Context, req *functionspb.GetFunctionRequest) (*functionspb.Function, error) {
	if _, _, err := parseFunctionName(req.GetName()); err != nil {
		return nil, err
	}
	if req.GetRevision() != "" {
		return nil, apierror.Unimplemented("GetFunction revision is not supported: only the current function is kept")
	}
	functions.mu.Lock()
	defer functions.mu.Unlock()
	sf, ok := functions.fns[req.GetName()]
	if !ok {
		return nil, functionNotFound(req.GetName())
	}
	return cloneFunction(sf.fn), nil
}

// ListFunctions lists a location's functions, or every location's for
// locations/-, by name.
func (f *FunctionsServer) ListFunctions(_ context.Context, req *functionspb.ListFunctionsRequest) (*functionspb.ListFunctionsResponse, error) {
	project, location, err := resource.ParseLocation(req.GetParent())
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if req.GetPageToken() != "" || req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Unimplemented("ListFunctions page_token, filter and order_by are not supported")
	}
	prefix := req.GetParent() + "/functions/"
	if location == "-" {
		prefix = "projects/" + project + "/locations/"
	}
	out := &functionspb.ListFunctionsResponse{}
	for _, fn := range Functions(project) {
		if strings.HasPrefix(fn.GetName(), prefix) {
			out.Functions = append(out.Functions, fn)
		}
	}
	return out, nil
}

// unsupportedFunction names the first field of a function that this
// instance cannot honour, so it is refused rather than ignored.
func unsupportedFunction(fn *functionspb.Function) error {
	var fields []string
	add := func(set bool, field string) {
		if set {
			fields = append(fields, field)
		}
	}
	bc, sc := fn.GetBuildConfig(), fn.GetServiceConfig()
	add(bc.GetWorkerPool() != "", "build_config.worker_pool")
	add(bc.GetDockerRepository() != "", "build_config.docker_repository")
	add(bc.GetServiceAccount() != "", "build_config.service_account")
	add(bc.GetSource().GetRepoSource() != nil, "build_config.source.repo_source")
	add(bc.GetSource().GetGitUri() != "", "build_config.source.git_uri")
	add(sc.GetVpcConnector() != "", "service_config.vpc_connector")
	add(len(sc.GetSecretEnvironmentVariables()) > 0, "service_config.secret_environment_variables")
	add(len(sc.GetSecretVolumes()) > 0, "service_config.secret_volumes")
	add(sc.GetBinaryAuthorizationPolicy() != "", "service_config.binary_authorization_policy")
	add(fn.GetKmsKeyName() != "", "kms_key_name")
	if e := fn.GetEnvironment(); e == functionspb.Environment_GEN_1 {
		fields = append(fields, "environment GEN_1")
	}
	if len(fields) > 0 {
		return apierror.Unimplemented("function field %s is not supported on this instance", strings.Join(fields, ", "))
	}
	return nil
}

// sourceOf is the archive a function's build reads: build_config's
// storage_source, or the object a GenerateUploadUrl URL names.
func sourceOf(bc *functionspb.BuildConfig) (bucket, object string, generation int64, err error) {
	ss := bc.GetSource().GetStorageSource()
	if ss == nil {
		return "", "", 0, apierror.InvalidArgument("build_config.source.storage_source is required")
	}
	if ss.GetBucket() == "" && ss.GetSourceUploadUrl() != "" {
		if b, o, ok := parseUploadURL(ss.GetSourceUploadUrl()); ok {
			return b, o, 0, nil
		}
		return "", "", 0, apierror.InvalidArgument("build_config.source.storage_source.source_upload_url %q is not an upload URL this instance generated", ss.GetSourceUploadUrl())
	}
	if ss.GetBucket() == "" || ss.GetObject() == "" {
		return "", "", 0, apierror.InvalidArgument("build_config.source.storage_source.bucket and object are required")
	}
	return ss.GetBucket(), ss.GetObject(), ss.GetGeneration(), nil
}

// CreateFunction stores the function as DEPLOYING and returns its
// operation; the build and the service's rollout run behind it, and the
// function becomes ACTIVE, or FAILED with the operation's error, as on
// Google. A function already present is ALREADY_EXISTS at once.
func (f *FunctionsServer) CreateFunction(ctx context.Context, req *functionspb.CreateFunctionRequest) (*longrunningpb.Operation, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	id := req.GetFunctionId()
	in := req.GetFunction()
	if in == nil {
		return nil, apierror.InvalidArgument("function is required")
	}
	if id == "" {
		if _, fid, err := parseFunctionName(in.GetName()); err == nil {
			id = fid
		}
	}
	if err := ValidateServiceID(id); err != nil {
		return nil, apierror.InvalidArgument("invalid function_id %q: it must start with a lower case letter and contain only lower case letters, digits and hyphens, at most 63", id)
	}
	if err := unsupportedFunction(in); err != nil {
		return nil, err
	}
	if err := f.checkEventTrigger(in.GetEventTrigger()); err != nil {
		return nil, err
	}
	if err := checkRuntime(in.GetBuildConfig().GetRuntime()); err != nil {
		return nil, err
	}
	if _, _, _, err := sourceOf(in.GetBuildConfig()); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/functions/" + id
	fn := cloneFunction(in)
	now := timestamppb.Now()
	fn.Name, fn.State, fn.Environment = name, functionspb.Function_DEPLOYING, functionspb.Environment_GEN_2
	fn.CreateTime, fn.UpdateTime, fn.Url = now, now, ""
	if fn.ServiceConfig == nil {
		fn.ServiceConfig = &functionspb.ServiceConfig{}
	}
	defaultServiceConfig(fn)
	defaultEventTrigger(fn)
	if fn.EventTrigger != nil {
		fn.EventTrigger.Trigger = ""
	}
	functions.mu.Lock()
	if _, ok := functions.fns[name]; ok {
		functions.mu.Unlock()
		return nil, apierror.AlreadyExists("Function %s already exists", name)
	}
	functions.fns[name] = &storedFunction{fn: fn}
	functions.mu.Unlock()
	op := f.newOperation(req.GetParent(), name, "create", functionspb.OperationType_CREATE_FUNCTION, in)
	go f.deploy(context.WithoutCancel(ctx), op, name, true, true)
	return f.s.toProtoOperation(op)
}

// defaultServiceConfig fills what Google defaults: 256M of memory, a
// 60-second timeout, 100 instances at most and the compute default
// service account.
func defaultServiceConfig(fn *functionspb.Function) {
	sc := fn.ServiceConfig
	if sc.AvailableMemory == "" {
		sc.AvailableMemory = "256M"
	}
	if sc.TimeoutSeconds == 0 {
		sc.TimeoutSeconds = 60
	}
	if sc.MaxInstanceCount == 0 {
		sc.MaxInstanceCount = 100
	}
	if sc.MaxInstanceRequestConcurrency == 0 {
		sc.MaxInstanceRequestConcurrency = 1
	}
	if sc.ServiceAccountEmail == "" {
		project, _, _ := strings.Cut(strings.TrimPrefix(fn.Name, "projects/"), "/")
		sc.ServiceAccountEmail = fmt.Sprintf("%d-compute@developer.gserviceaccount.com", resource.ProjectNumber(project))
	}
	if sc.IngressSettings == functionspb.ServiceConfig_INGRESS_SETTINGS_UNSPECIFIED {
		sc.IngressSettings = functionspb.ServiceConfig_ALLOW_ALL
	}
	sc.AllTrafficOnLatestRevision = true
}

func (f *FunctionsServer) newOperation(parent, name, verb string, typ functionspb.OperationType, request proto.Message) string {
	op := f.s.ops.CreateNamed(parent+"/operations/operation-"+strconv.FormatInt(time.Now().UnixMilli(), 10)+"-"+uuid.NewString()[:13], name)
	md := &functionspb.OperationMetadata{CreateTime: timestamppb.Now(), Target: name, Verb: verb,
		ApiVersion: "v2", OperationType: typ}
	if request != nil {
		md.RequestResource, _ = anypb.New(request)
	}
	_ = f.s.ops.SetMetadata(op.Name, md)
	return op.Name
}

// setStage updates the operation's metadata: its stages and build.
func (f *FunctionsServer) setStage(opName string, mutate func(md *functionspb.OperationMetadata)) {
	o, err := f.s.ops.Get(opName)
	if err != nil {
		return
	}
	md, ok := o.Metadata.(*functionspb.OperationMetadata)
	if !ok {
		return
	}
	md = proto.Clone(md).(*functionspb.OperationMetadata)
	mutate(md)
	_ = f.s.ops.SetMetadata(opName, md)
}

func stage(name functionspb.Stage_Name, state functionspb.Stage_State, msg string) *functionspb.Stage {
	return &functionspb.Stage{Name: name, State: state, Message: msg}
}

// awaitOp waits for an operation of the adapter's store to finish.
func (f *FunctionsServer) awaitOp(ctx context.Context, name string, limit time.Duration) (any, error) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		o, err := f.s.ops.Get(name)
		if err != nil {
			return nil, err
		}
		if o.Done() {
			if o.Error != nil {
				return nil, o.Error
			}
			return o.Response, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, &apierror.Error{Code: codes.DeadlineExceeded, Message: fmt.Sprintf("operation %s did not finish within %s", name, limit)}
}

// fail marks the function FAILED and the operation failed with err.
func (f *FunctionsServer) fail(opName, name string, err error) {
	msg := apierror.From(err).Message
	functions.mu.Lock()
	if sf, ok := functions.fns[name]; ok {
		sf.fn.State = functionspb.Function_FAILED
		sf.fn.StateMessages = []*functionspb.StateMessage{{Severity: functionspb.StateMessage_ERROR, Type: "DeploymentFailed", Message: msg}}
		sf.fn.UpdateTime = timestamppb.Now()
	}
	functions.mu.Unlock()
	f.setStage(opName, func(md *functionspb.OperationMetadata) {
		md.EndTime, md.StatusDetail = timestamppb.Now(), msg
	})
	_ = f.s.ops.Fail(opName, err)
}

// imageURI is where Google keeps a 2nd-gen function's image: the
// gcf-artifacts repository of the function's region.
func imageURI(name string) string {
	parts := strings.Split(name, "/")
	return fmt.Sprintf("%s-docker.pkg.dev/%s/gcf-artifacts/%s:version_%d", parts[3], parts[1],
		strings.ReplaceAll(parts[5], "_", "--"), time.Now().UnixNano())
}

// deploy builds the function's source (when build) and rolls out its
// service (when serve), then marks it ACTIVE and finishes the operation.
func (f *FunctionsServer) deploy(ctx context.Context, opName, name string, build, serve bool) {
	functions.mu.Lock()
	sf, ok := functions.fns[name]
	if !ok {
		functions.mu.Unlock()
		_ = f.s.ops.Fail(opName, functionNotFound(name))
		return
	}
	fn := cloneFunction(sf.fn)
	functions.mu.Unlock()
	parent, id, _ := parseFunctionName(name)
	project, _, _ := resource.ParseLocation(parent)
	image := ""
	if build {
		f.setStage(opName, func(md *functionspb.OperationMetadata) {
			md.Stages = []*functionspb.Stage{
				stage(functionspb.Stage_BUILD, functionspb.Stage_IN_PROGRESS, "Building the source with Google's buildpacks"),
				stage(functionspb.Stage_SERVICE, functionspb.Stage_NOT_STARTED, ""),
			}
		})
		bucket, object, generation, _ := sourceOf(fn.GetBuildConfig())
		env := map[string]string{}
		for k, v := range fn.GetBuildConfig().GetEnvironmentVariables() {
			env[k] = v
		}
		// Google's builder reads the runtime, as nodejs22 or python312,
		// from GOOGLE_RUNTIME; a value the caller set wins.
		if _, set := env["GOOGLE_RUNTIME"]; !set {
			env["GOOGLE_RUNTIME"] = fn.GetBuildConfig().GetRuntime()
		}
		image = imageURI(name)
		resp, err := f.builds.SubmitBuild(ctx, &runpb.SubmitBuildRequest{
			Parent:   parent,
			Source:   &runpb.SubmitBuildRequest_StorageSource{StorageSource: &runpb.StorageSource{Bucket: bucket, Object: object, Generation: generation}},
			ImageUri: image,
			BuildType: &runpb.SubmitBuildRequest_BuildpackBuild{BuildpackBuild: &runpb.SubmitBuildRequest_BuildpacksBuild{
				FunctionTarget: fn.GetBuildConfig().GetEntryPoint(), EnvironmentVariables: env}},
		})
		if err != nil {
			e := apierror.From(err)
			f.fail(opName, name, &apierror.Error{Code: e.Code, Message: "Build failed: " + e.Message})
			return
		}
		buildID := resp.GetBuildOperation().GetName()[strings.LastIndex(resp.GetBuildOperation().GetName(), "/")+1:]
		buildName := fmt.Sprintf("projects/%d/locations/%s/builds/%s", resource.ProjectNumber(project), strings.Split(parent, "/")[3], buildID)
		f.setStage(opName, func(md *functionspb.OperationMetadata) {
			md.BuildName = buildName
			md.Stages[0].Resource = buildName
		})
		if _, err := f.awaitOp(ctx, resp.GetBuildOperation().GetName(), f.builds.timeout); err != nil {
			e := apierror.From(err)
			f.fail(opName, name, &apierror.Error{Code: e.Code, Message: "Build failed: " + e.Message})
			return
		}
		functions.mu.Lock()
		if sf, ok := functions.fns[name]; ok {
			sf.fn.BuildConfig.Build = buildName
			sf.fn.BuildConfig.DockerRegistry = functionspb.BuildConfig_ARTIFACT_REGISTRY
			fn = cloneFunction(sf.fn)
		}
		functions.mu.Unlock()
		f.setStage(opName, func(md *functionspb.OperationMetadata) {
			md.Stages[0].State, md.Stages[0].Message = functionspb.Stage_COMPLETE, ""
		})
	}
	if serve {
		svcName := parent + "/services/" + id
		f.setStage(opName, func(md *functionspb.OperationMetadata) {
			if len(md.Stages) < 2 {
				md.Stages = []*functionspb.Stage{stage(functionspb.Stage_BUILD, functionspb.Stage_COMPLETE, ""),
					stage(functionspb.Stage_SERVICE, functionspb.Stage_NOT_STARTED, "")}
			}
			md.Stages[1].State, md.Stages[1].Resource = functionspb.Stage_IN_PROGRESS, svcName
		})
		if image == "" {
			// No rebuild: the service keeps the image it runs.
			existing, err := f.s.GetService(ctx, &runpb.GetServiceRequest{Name: svcName})
			if err != nil || len(existing.GetTemplate().GetContainers()) == 0 {
				f.fail(opName, name, apierror.FailedPrecondition("the function has no built image to serve; update its build_config to build it"))
				return
			}
			image = existing.GetTemplate().GetContainers()[0].GetImage()
		}
		svc := functionService(fn, image)
		var op *longrunningpb.Operation
		var err error
		if _, gerr := f.s.GetService(ctx, &runpb.GetServiceRequest{Name: svcName}); gerr == nil {
			svc.Name = svcName
			op, err = f.s.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: svc})
		} else {
			op, err = f.s.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: id, Service: svc})
		}
		if err != nil {
			f.fail(opName, name, err)
			return
		}
		res, err := f.awaitOp(ctx, op.GetName(), f.s.readyTimeout+time.Minute)
		if err != nil {
			f.fail(opName, name, err)
			return
		}
		deployed, _ := res.(*runpb.Service)
		functions.mu.Lock()
		if sf, ok := functions.fns[name]; ok {
			sf.fn.ServiceConfig.Service = svcName
			if deployed != nil {
				sf.fn.ServiceConfig.Uri = deployed.GetUri()
				sf.fn.ServiceConfig.Revision = strings.TrimPrefix(deployed.GetLatestReadyRevision(), svcName+"/revisions/")
				sf.fn.Url = deployed.GetUri()
			}
		}
		functions.mu.Unlock()
		f.setStage(opName, func(md *functionspb.OperationMetadata) {
			md.Stages[1].State = functionspb.Stage_COMPLETE
		})
	}
	// The trigger is made once the service runs, as Google makes it.
	if err := f.syncTrigger(ctx, name); err != nil {
		f.fail(opName, name, err)
		return
	}
	functions.mu.Lock()
	sf, ok = functions.fns[name]
	if !ok {
		functions.mu.Unlock()
		_ = f.s.ops.Fail(opName, functionNotFound(name))
		return
	}
	sf.fn.State, sf.fn.StateMessages, sf.fn.UpdateTime = functionspb.Function_ACTIVE, nil, timestamppb.Now()
	done := cloneFunction(sf.fn)
	functions.mu.Unlock()
	f.setStage(opName, func(md *functionspb.OperationMetadata) { md.EndTime = timestamppb.Now() })
	_ = f.s.ops.Succeed(opName, done)
}

// functionService is the Cloud Run service a function runs as.
func functionService(fn *functionspb.Function, image string) *runpb.Service {
	sc := fn.GetServiceConfig()
	c := &runpb.Container{Image: image}
	keys := make([]string, 0, len(sc.GetEnvironmentVariables()))
	for k := range sc.GetEnvironmentVariables() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c.Env = append(c.Env, &runpb.EnvVar{Name: k, Values: &runpb.EnvVar_Value{Value: sc.GetEnvironmentVariables()[k]}})
	}
	limits := map[string]string{}
	if m := sc.GetAvailableMemory(); m != "" {
		limits["memory"] = memoryQuantity(m)
	}
	if cpu := sc.GetAvailableCpu(); cpu != "" {
		limits["cpu"] = cpu
	}
	if len(limits) > 0 {
		c.Resources = &runpb.ResourceRequirements{Limits: limits}
	}
	t := &runpb.RevisionTemplate{
		Containers:                    []*runpb.Container{c},
		Timeout:                       durationpb.New(time.Duration(sc.GetTimeoutSeconds()) * time.Second),
		ServiceAccount:                sc.GetServiceAccountEmail(),
		MaxInstanceRequestConcurrency: sc.GetMaxInstanceRequestConcurrency(),
		Scaling:                       &runpb.RevisionScaling{MinInstanceCount: sc.GetMinInstanceCount(), MaxInstanceCount: sc.GetMaxInstanceCount()},
	}
	labels := map[string]string{"goog-managed-by": "cloudfunctions"}
	for k, v := range fn.GetLabels() {
		labels[k] = v
	}
	return &runpb.Service{Template: t, Labels: labels}
}

// memoryQuantity turns Cloud Functions' memory ("256M", "1G", "512Mi")
// into the Kubernetes quantity Cloud Run's limits take. Functions' M and G
// are binary, as Google documents: 256M is 256Mi.
func memoryQuantity(m string) string {
	switch {
	case strings.HasSuffix(m, "Mi"), strings.HasSuffix(m, "Gi"):
		return m
	case strings.HasSuffix(m, "M"), strings.HasSuffix(m, "G"):
		return m + "i"
	}
	return m
}

// UpdateFunction applies the update mask, or replaces the function with no
// mask, and redeploys: a change under build_config rebuilds the source, one
// under service_config rolls out a new revision, and description and
// labels change nothing that runs.
func (f *FunctionsServer) UpdateFunction(ctx context.Context, req *functionspb.UpdateFunctionRequest) (*longrunningpb.Operation, error) {
	in := req.GetFunction()
	parent, _, err := parseFunctionName(in.GetName())
	if err != nil {
		return nil, err
	}
	if err := unsupportedFunction(in); err != nil {
		return nil, err
	}
	if err := f.checkEventTrigger(in.GetEventTrigger()); err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	functions.mu.Lock()
	sf, ok := functions.fns[in.GetName()]
	if !ok {
		functions.mu.Unlock()
		return nil, functionNotFound(in.GetName())
	}
	if sf.fn.State == functionspb.Function_DEPLOYING {
		functions.mu.Unlock()
		return nil, apierror.FailedPrecondition("An operation on function %s is already in progress", in.GetName())
	}
	fn := cloneFunction(sf.fn)
	build, serve, trig := false, false, false
	if len(paths) == 0 {
		fn.Description, fn.Labels = in.GetDescription(), in.GetLabels()
		fn.EventTrigger, trig = cloneTrigger(in.GetEventTrigger()), true
		fn.BuildConfig, fn.ServiceConfig = proto.Clone(in.GetBuildConfig()).(*functionspb.BuildConfig), proto.Clone(in.GetServiceConfig()).(*functionspb.ServiceConfig)
		if fn.ServiceConfig == nil {
			fn.ServiceConfig = &functionspb.ServiceConfig{}
		}
		build, serve = true, true
	}
	for _, p := range paths {
		if err := applyFunctionPath(fn, in, p); err != nil {
			functions.mu.Unlock()
			return nil, err
		}
		switch {
		case p == "build_config" || strings.HasPrefix(p, "build_config.") || p == "buildConfig" || strings.HasPrefix(p, "buildConfig."):
			build, serve = true, true
		case p == "service_config" || strings.HasPrefix(p, "service_config.") || p == "serviceConfig" || strings.HasPrefix(p, "serviceConfig."):
			serve = true
		case p == "labels":
			serve = true // the service carries the labels
		case p == "event_trigger" || strings.HasPrefix(p, "event_trigger.") || p == "eventTrigger" || strings.HasPrefix(p, "eventTrigger."):
			trig = true
		}
	}
	if build {
		if err := checkRuntime(fn.GetBuildConfig().GetRuntime()); err != nil {
			functions.mu.Unlock()
			return nil, err
		}
		if _, _, _, err := sourceOf(fn.GetBuildConfig()); err != nil {
			functions.mu.Unlock()
			return nil, err
		}
	}
	defaultServiceConfig(fn)
	defaultEventTrigger(fn)
	// The service a function runs as is the server's to name.
	fn.ServiceConfig.Service, fn.ServiceConfig.Uri = sf.fn.GetServiceConfig().GetService(), sf.fn.GetServiceConfig().GetUri()
	fn.ServiceConfig.Revision = sf.fn.GetServiceConfig().GetRevision()
	fn.UpdateTime = timestamppb.Now()
	if build || serve || trig {
		fn.State = functionspb.Function_DEPLOYING
	}
	sf.fn = fn
	functions.mu.Unlock()
	op := f.newOperation(parent, in.GetName(), "update", functionspb.OperationType_UPDATE_FUNCTION, in)
	if !build && !serve && !trig {
		f.setStage(op, func(md *functionspb.OperationMetadata) { md.EndTime = timestamppb.Now() })
		_ = f.s.ops.Succeed(op, cloneFunction(fn))
		return f.s.toProtoOperation(op)
	}
	go f.deploy(context.WithoutCancel(ctx), op, in.GetName(), build, serve)
	return f.s.toProtoOperation(op)
}

// applyFunctionPath copies one update mask path from in onto fn.
func applyFunctionPath(fn, in *functionspb.Function, p string) error {
	switch p {
	case "description":
		fn.Description = in.GetDescription()
		return nil
	case "labels":
		fn.Labels = in.GetLabels()
		return nil
	case "name", "state", "url", "update_time", "create_time", "environment":
		return apierror.InvalidArgument("update_mask path %q is output only or immutable", p)
	case "event_trigger", "eventTrigger":
		fn.EventTrigger = cloneTrigger(in.GetEventTrigger())
		return nil
	}
	field, sub, _ := strings.Cut(p, ".")
	var dst, src proto.Message
	switch field {
	case "build_config", "buildConfig":
		if fn.BuildConfig == nil {
			fn.BuildConfig = &functionspb.BuildConfig{}
		}
		dst, src = fn.BuildConfig, in.GetBuildConfig()
	case "service_config", "serviceConfig":
		if fn.ServiceConfig == nil {
			fn.ServiceConfig = &functionspb.ServiceConfig{}
		}
		dst, src = fn.ServiceConfig, in.GetServiceConfig()
	case "event_trigger", "eventTrigger":
		// gcloud's --retry and --no-retry send eventTrigger.retryPolicy.
		if fn.EventTrigger == nil {
			fn.EventTrigger = &functionspb.EventTrigger{}
		}
		dst, src = fn.EventTrigger, in.GetEventTrigger()
	default:
		return apierror.InvalidArgument("update_mask path %q is not a Function field", p)
	}
	if !src.ProtoReflect().IsValid() {
		src = dst.ProtoReflect().Type().New().Interface()
	}
	if sub == "" {
		proto.Reset(dst)
		proto.Merge(dst, src)
		return nil
	}
	fd := dst.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(sub))
	if fd == nil {
		fd = dst.ProtoReflect().Descriptor().Fields().ByJSONName(sub)
	}
	if fd == nil {
		return apierror.InvalidArgument("update_mask path %q is not a Function field", p)
	}
	if src.ProtoReflect().Has(fd) {
		dst.ProtoReflect().Set(fd, src.ProtoReflect().Get(fd))
	} else {
		dst.ProtoReflect().Clear(fd)
	}
	return nil
}

// DeleteFunction deletes the function's service and the function. The
// built image stays in the cluster, as Google leaves it in gcf-artifacts.
func (f *FunctionsServer) DeleteFunction(ctx context.Context, req *functionspb.DeleteFunctionRequest) (*longrunningpb.Operation, error) {
	parent, _, err := parseFunctionName(req.GetName())
	if err != nil {
		return nil, err
	}
	functions.mu.Lock()
	sf, ok := functions.fns[req.GetName()]
	if !ok {
		functions.mu.Unlock()
		return nil, functionNotFound(req.GetName())
	}
	if sf.fn.State == functionspb.Function_DEPLOYING {
		functions.mu.Unlock()
		return nil, apierror.FailedPrecondition("An operation on function %s is already in progress", req.GetName())
	}
	sf.fn.State = functionspb.Function_DELETING
	svc := sf.fn.GetServiceConfig().GetService()
	functions.mu.Unlock()
	op := f.newOperation(parent, req.GetName(), "delete", functionspb.OperationType_DELETE_FUNCTION, nil)
	f.teardown(ctx, sf.trigger)
	if svc != "" {
		if _, err := f.s.DeleteService(ctx, &runpb.DeleteServiceRequest{Name: svc}); err != nil && apierror.From(err).Code != codes.NotFound {
			functions.mu.Lock()
			sf.fn.State = functionspb.Function_FAILED
			functions.mu.Unlock()
			_ = f.s.ops.Fail(op, err)
			return f.s.toProtoOperation(op)
		}
	}
	functions.mu.Lock()
	delete(functions.fns, req.GetName())
	functions.mu.Unlock()
	f.setStage(op, func(md *functionspb.OperationMetadata) { md.EndTime = timestamppb.Now() })
	_ = f.s.ops.Succeed(op, &emptypb.Empty{})
	return f.s.toProtoOperation(op)
}

// uploadPrefix and downloadPrefix are the Run port's paths for a
// function's source: an upload URL takes the unauthenticated PUT of a zip,
// as Google's signed URL does, and writes it to the instance's Cloud
// Storage; a download URL reads it back.
const (
	uploadPrefix   = "/cloudburrow/functions/upload/"
	downloadPrefix = "/cloudburrow/functions/download/"
)

// uploadBucket is the bucket a project's uploads go to in a region: the
// gcf-v2-uploads bucket Google's are named after, made on the first upload.
func uploadBucket(project, location string) string {
	return fmt.Sprintf("gcf-v2-uploads-%d-%s", resource.ProjectNumber(project), location)
}

func parseUploadURL(u string) (bucket, object string, ok bool) {
	pu, err := url.Parse(u)
	if err != nil {
		return "", "", false
	}
	rest, ok := strings.CutPrefix(pu.Path, uploadPrefix)
	if !ok {
		return "", "", false
	}
	bucket, object, ok = strings.Cut(rest, "/")
	return bucket, object, ok && bucket != "" && object != ""
}

// GenerateUploadUrl returns a URL on this port that takes the source zip
// with a PUT and no credentials, and the storage_source a create names.
func (f *FunctionsServer) GenerateUploadUrl(_ context.Context, req *functionspb.GenerateUploadUrlRequest) (*functionspb.GenerateUploadUrlResponse, error) {
	project, location, err := resource.ParseLocation(req.GetParent())
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if req.GetKmsKeyName() != "" {
		return nil, apierror.Unimplemented("kms_key_name is not supported")
	}
	if req.GetEnvironment() == functionspb.Environment_GEN_1 {
		return nil, apierror.Unimplemented("GEN_1 uploads are not supported: only 2nd-gen functions are served")
	}
	bucket, object := uploadBucket(project, location), uuid.NewString()+".zip"
	return &functionspb.GenerateUploadUrlResponse{
		UploadUrl:     f.base() + uploadPrefix + bucket + "/" + object,
		StorageSource: &functionspb.StorageSource{Bucket: bucket, Object: object},
	}, nil
}

// GenerateDownloadUrl returns a URL on this port that reads the source the
// function was built from.
func (f *FunctionsServer) GenerateDownloadUrl(_ context.Context, req *functionspb.GenerateDownloadUrlRequest) (*functionspb.GenerateDownloadUrlResponse, error) {
	if _, _, err := parseFunctionName(req.GetName()); err != nil {
		return nil, err
	}
	functions.mu.Lock()
	sf, ok := functions.fns[req.GetName()]
	functions.mu.Unlock()
	if !ok {
		return nil, functionNotFound(req.GetName())
	}
	bucket, object, _, err := sourceOf(sf.fn.GetBuildConfig())
	if err != nil {
		return nil, apierror.FailedPrecondition("the function has no source to download")
	}
	return &functionspb.GenerateDownloadUrlResponse{DownloadUrl: f.base() + downloadPrefix + bucket + "/" + object}, nil
}

// SourceHandler serves the upload and download URLs, passing every other
// request to next.
func (f *FunctionsServer) SourceHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, uploadPrefix) && r.Method == http.MethodPut:
			f.upload(w, r)
		case strings.HasPrefix(r.URL.Path, downloadPrefix) && r.Method == http.MethodGet:
			f.download(w, r)
		case strings.HasPrefix(r.URL.Path, eventarcPushPrefix) && r.Method == http.MethodPost && f.events != nil:
			f.push(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// maxSourceBytes is Google's limit on an uploaded source, 500 MB.
const maxSourceBytes = 500 << 20

func (f *FunctionsServer) upload(w http.ResponseWriter, r *http.Request) {
	bucket, object, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, uploadPrefix), "/")
	ow, can := f.builds.fetch.(ObjectWriter)
	if !ok || !strings.HasPrefix(bucket, "gcf-v2-uploads-") || object == "" {
		http.Error(w, "not an upload URL", http.StatusNotFound)
		return
	}
	if f.builds.fetch == nil || !can {
		http.Error(w, "this instance does not serve Cloud Storage: start it with --services run,storage", http.StatusPreconditionFailed)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxSourceBytes+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(data) > maxSourceBytes {
		http.Error(w, "the source is larger than 500 MB", http.StatusRequestEntityTooLarge)
		return
	}
	project := projectForUploadBucket(bucket)
	if err := ow.Put(r.Context(), project, bucket, object, data, true); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// projectForUploadBucket finds the project whose number an upload bucket
// carries, among the functions' and the default project; the bucket is made
// in it. Any project works for the bucket itself.
func projectForUploadBucket(bucket string) string {
	num := strings.Split(strings.TrimPrefix(bucket, "gcf-v2-uploads-"), "-")[0]
	functions.mu.Lock()
	defer functions.mu.Unlock()
	for name := range functions.fns {
		p := strings.Split(name, "/")[1]
		if strconv.FormatInt(resource.ProjectNumber(p), 10) == num {
			return p
		}
	}
	return num
}

func (f *FunctionsServer) download(w http.ResponseWriter, r *http.Request) {
	bucket, object, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, downloadPrefix), "/")
	if !ok || f.builds.fetch == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rc, err := f.builds.fetch.Fetch(r.Context(), bucket, object, 0)
	if err != nil {
		http.Error(w, err.Error(), apierror.From(err).HTTPStatus())
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/zip")
	_, _ = io.Copy(w, rc)
}

// Runtimes are ListRuntimes' answer: the 2nd-gen runtimes of the six
// languages built here, as Google's runtimes page lists them.
var Runtimes = []struct{ Name, Display string }{
	{"nodejs22", "Node.js 22"}, {"nodejs20", "Node.js 20"}, {"nodejs18", "Node.js 18"},
	{"python313", "Python 3.13"}, {"python312", "Python 3.12"}, {"python311", "Python 3.11"},
	{"python310", "Python 3.10"}, {"python39", "Python 3.9"},
	{"go125", "Go 1.25"}, {"go124", "Go 1.24"}, {"go123", "Go 1.23"}, {"go122", "Go 1.22"}, {"go121", "Go 1.21"},
	{"java21", "Java 21"}, {"java17", "Java 17"}, {"java11", "Java 11"},
	{"ruby34", "Ruby 3.4"}, {"ruby33", "Ruby 3.3"}, {"ruby32", "Ruby 3.2"},
	{"php84", "PHP 8.4"}, {"php83", "PHP 8.3"}, {"php82", "PHP 8.2"},
	{"dotnet8", ".NET 8"}, {"dotnet6", ".NET 6"},
}

// checkRuntime refuses a runtime ListRuntimes does not name, as Google
// refuses one it does not support.
func checkRuntime(runtime string) error {
	if runtime == "" {
		return apierror.InvalidArgument("build_config.runtime is required")
	}
	for _, r := range Runtimes {
		if r.Name == runtime {
			return nil
		}
	}
	return apierror.InvalidArgument("build_config.runtime %q is not supported: ListRuntimes names the runtimes", runtime)
}

// ListRuntimes lists the runtimes a function can be built with.
func (f *FunctionsServer) ListRuntimes(_ context.Context, req *functionspb.ListRuntimesRequest) (*functionspb.ListRuntimesResponse, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if req.GetFilter() != "" {
		return nil, apierror.Unimplemented("ListRuntimes filter is not supported")
	}
	out := &functionspb.ListRuntimesResponse{}
	for _, r := range Runtimes {
		out.Runtimes = append(out.Runtimes, &functionspb.ListRuntimesResponse_Runtime{
			Name: r.Name, DisplayName: r.Display, Stage: functionspb.ListRuntimesResponse_GA,
			Environment: functionspb.Environment_GEN_2})
	}
	return out, nil
}

// functionsIAM is the google.iam.v1.IAMPolicy mixin on functions, stored
// and never enforced (ADR-0006).
type functionsIAM struct{ f *FunctionsServer }

func (functionsIAM) stored(resourceName string) (*storedFunction, error) {
	if _, _, err := parseFunctionName(resourceName); err != nil {
		return nil, err
	}
	sf, ok := functions.fns[resourceName]
	if !ok {
		return nil, functionNotFound(resourceName)
	}
	return sf, nil
}

func (i functionsIAM) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	functions.mu.Lock()
	defer functions.mu.Unlock()
	sf, err := i.stored(req.GetResource())
	if err != nil {
		return nil, err
	}
	return iampolicy.Get(sf.policy, req.GetOptions())
}

func (i functionsIAM) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	functions.mu.Lock()
	defer functions.mu.Unlock()
	sf, err := i.stored(req.GetResource())
	if err != nil {
		return nil, err
	}
	p, err := iampolicy.Set(sf.policy, req)
	if err != nil {
		return nil, err
	}
	sf.policy = &p
	return p.Proto(), nil
}

func (i functionsIAM) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	functions.mu.Lock()
	defer functions.mu.Unlock()
	if _, err := i.stored(req.GetResource()); err != nil {
		return nil, err
	}
	return iampolicy.Permissions(req), nil
}

// functionsLocations answers the Locations mixin with the regions Cloud
// Scheduler's list names; any well-formed location is accepted in a name.
type functionsLocations struct{}

// FunctionRegions are the regions ListLocations answers.
var FunctionRegions = []string{
	"asia-east1", "asia-northeast1", "asia-south1", "asia-southeast1", "australia-southeast1",
	"europe-north1", "europe-west1", "europe-west2", "europe-west3", "europe-west4",
	"northamerica-northeast1", "southamerica-east1",
	"us-central1", "us-east1", "us-east4", "us-west1", "us-west2",
}

func (functionsLocations) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	project, ok := strings.CutPrefix(req.GetName(), "projects/")
	if !ok || project == "" || strings.Contains(project, "/") {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}", req.GetName())
	}
	out := &locationpb.ListLocationsResponse{}
	for _, r := range FunctionRegions {
		out.Locations = append(out.Locations, &locationpb.Location{Name: req.GetName() + "/locations/" + r, LocationId: r, DisplayName: r})
	}
	return out, nil
}

func (functionsLocations) GetLocation(_ context.Context, req *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	_, loc, err := resource.ParseLocation(req.GetName())
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	return &locationpb.Location{Name: req.GetName(), LocationId: loc, DisplayName: loc}, nil
}

// functionsMixins are the bindings cloudfunctions_v2.yaml adds: Locations,
// the IAM policy methods on a function, and Operations at v2, which the
// Cloud Run transcoder already binds over the same store.
var functionsMixins = []*annotations.HttpRule{
	{Selector: "google.cloud.location.Locations.ListLocations",
		Pattern: &annotations.HttpRule_Get{Get: "/v2/{name=projects/*}/locations"}},
	{Selector: "google.iam.v1.IAMPolicy.GetIamPolicy",
		Pattern: &annotations.HttpRule_Get{Get: "/v2/{resource=projects/*/locations/*/functions/*}:getIamPolicy"}},
	{Selector: "google.iam.v1.IAMPolicy.SetIamPolicy",
		Pattern: &annotations.HttpRule_Post{Post: "/v2/{resource=projects/*/locations/*/functions/*}:setIamPolicy"}, Body: "*"},
	{Selector: "google.iam.v1.IAMPolicy.TestIamPermissions",
		Pattern: &annotations.HttpRule_Post{Post: "/v2/{resource=projects/*/locations/*/functions/*}:testIamPermissions"}, Body: "*"},
}

// FunctionsTranscoder is the Cloud Functions v2 JSON API's configuration.
// Its update masks may be snake_case: a redeploy with `gcloud functions
// deploy --gen2` sends updateMask=build_config.source,event_trigger,
// eventTrigger.retryPolicy, both spellings in one mask (#1414).
func FunctionsTranscoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.functions.v2"}, Mixins: functionsMixins, SnakeCaseMasks: true}
}

// FunctionsHandler serves the Cloud Functions v2 JSON API, and the source
// upload and download URLs, on the Run port beside next.
func (f *FunctionsServer) FunctionsHandler(next http.Handler) http.Handler {
	t := FunctionsTranscoder()
	f.Register(t)
	return f.SourceHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isFunctionsPath(r.URL.Path) {
			t.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// isFunctionsPath reports whether a path is Cloud Functions v2's: Cloud
// Run's v2 paths never name functions or runtimes, nor list a project's
// locations.
func isFunctionsPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/v2/projects/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "locations" {
		return true
	}
	if len(parts) >= 4 && parts[1] == "locations" {
		switch strings.SplitN(parts[3], ":", 2)[0] {
		case "functions", "runtimes":
			return true
		}
	}
	return false
}
