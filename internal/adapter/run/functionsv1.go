package run

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// FunctionsV1Server serves google.cloud.functions.v1.CloudFunctionsService
// (#1428), the API of 1st-gen functions, over the v2 machinery: a v1
// CloudFunction is stored as a v2 Function whose environment is GEN_1, so the
// v2 API lists and reads it as Google's does, and it is built and served as a
// 2nd-gen function is, through Google's buildpacks and a Cloud Run service.
// The CloudFunction as the caller gave it is kept beside it, so a read
// answers the v1 shape: its httpsTrigger or legacy eventTrigger, its
// sourceArchiveUrl or sourceUploadUrl, and the v1 status.
type FunctionsV1Server struct {
	fv1.UnimplementedCloudFunctionsServiceServer
	f *FunctionsServer
}

// V1 returns the Cloud Functions v1 API over f.
func (f *FunctionsServer) V1() *FunctionsV1Server { return &FunctionsV1Server{f: f} }

// Register adds CloudFunctionsService to a gRPC server or the transcoder.
// Its IAM methods are its own RPCs; Locations and Operations are the
// mixins the v2 API and Cloud Run already register on the same port.
func (v *FunctionsV1Server) Register(g grpc.ServiceRegistrar) {
	fv1.RegisterCloudFunctionsServiceServer(g, v)
}

// gen1Account is the account a 1st-gen function runs as by default: the
// App Engine default service account, as Google documents.
func gen1Account(project string) string { return project + "@appspot.gserviceaccount.com" }

// v1EventTypes maps the legacy event types of 1st gen, both spellings, to
// the CloudEvent type of #1414's delivery.
var v1EventTypes = map[string]string{
	"google.pubsub.topic.publish":                     pubsubPublished,
	"providers/cloud.pubsub/eventTypes/topic.publish": pubsubPublished,
	"google.storage.object.finalize":                  storageEventPrefix + "finalized",
	"google.storage.object.delete":                    storageEventPrefix + "deleted",
	"google.storage.object.archive":                   storageEventPrefix + "archived",
	"google.storage.object.metadataUpdate":            storageEventPrefix + "metadataUpdated",
}

// v2Trigger is the v2 event trigger a v1 one is delivered through.
func v2Trigger(project string, et *fv1.EventTrigger) (*functionspb.EventTrigger, error) {
	typ, ok := v1EventTypes[et.GetEventType()]
	if !ok {
		return nil, apierror.Unimplemented("eventTrigger.eventType %q is not supported: Pub/Sub's google.pubsub.topic.publish and Cloud Storage's "+
			"google.storage.object.finalize, delete, archive and metadataUpdate are (the legacy providers/cloud.storage/eventTypes/object.change "+
			"and Firestore, Firebase and other sources are not served)", et.GetEventType())
	}
	out := &functionspb.EventTrigger{EventType: typ, ServiceAccountEmail: gen1Account(project),
		RetryPolicy: functionspb.EventTrigger_RETRY_POLICY_DO_NOT_RETRY}
	if et.GetFailurePolicy().GetRetry() != nil {
		out.RetryPolicy = functionspb.EventTrigger_RETRY_POLICY_RETRY
	}
	res := et.GetResource()
	if res == "" {
		return nil, apierror.InvalidArgument("eventTrigger.resource is required")
	}
	if typ == pubsubPublished {
		if !strings.HasPrefix(res, "projects/") {
			res = "projects/" + project + "/topics/" + res
		}
		out.PubsubTopic = res
		return out, nil
	}
	bucket := res
	if b, ok := strings.CutPrefix(res, "projects/_/buckets/"); ok {
		bucket = b
	} else if strings.Contains(res, "/") {
		return nil, apierror.InvalidArgument("eventTrigger.resource %q must be projects/_/buckets/{bucket}", res)
	}
	out.EventFilters = []*functionspb.EventFilter{{Attribute: "bucket", Value: bucket}}
	return out, nil
}

// unsupportedV1 refuses what 1st gen offers and this instance cannot honour.
func unsupportedV1(in *fv1.CloudFunction) error {
	var fields []string
	add := func(set bool, field string) {
		if set {
			fields = append(fields, field)
		}
	}
	add(in.GetSourceRepository() != nil, "sourceRepository")
	add(in.GetNetwork() != "", "network")
	add(in.GetVpcConnector() != "", "vpcConnector")
	add(len(in.GetSecretEnvironmentVariables()) > 0, "secretEnvironmentVariables")
	add(len(in.GetSecretVolumes()) > 0, "secretVolumes")
	add(in.GetKmsKeyName() != "", "kmsKeyName")
	add(in.GetBuildWorkerPool() != "", "buildWorkerPool")
	add(in.GetDockerRepository() != "", "dockerRepository")
	add(in.GetDockerRegistry() == fv1.CloudFunction_CONTAINER_REGISTRY, "dockerRegistry CONTAINER_REGISTRY")
	add(in.GetBuildServiceAccount() != "", "buildServiceAccount")
	if len(fields) > 0 {
		return apierror.Unimplemented("function field %s is not supported on this instance", strings.Join(fields, ", "))
	}
	return nil
}

// toV2 is the v2 function a v1 one is built and served as.
func toV2(name string, in *fv1.CloudFunction) (*functionspb.Function, error) {
	project := strings.Split(name, "/")[1]
	src := &functionspb.StorageSource{}
	switch {
	case in.GetSourceUploadUrl() != "":
		src.SourceUploadUrl = in.GetSourceUploadUrl()
	case in.GetSourceArchiveUrl() != "":
		b, o, ok := strings.Cut(strings.TrimPrefix(in.GetSourceArchiveUrl(), "gs://"), "/")
		if !strings.HasPrefix(in.GetSourceArchiveUrl(), "gs://") || !ok || b == "" || o == "" {
			return nil, apierror.InvalidArgument("sourceArchiveUrl %q must be gs://{bucket}/{object}", in.GetSourceArchiveUrl())
		}
		src.Bucket, src.Object = b, o
	default:
		return nil, apierror.InvalidArgument("one of sourceArchiveUrl, sourceUploadUrl or sourceRepository is required")
	}
	if in.GetHttpsTrigger() == nil && in.GetEventTrigger() == nil {
		return nil, apierror.InvalidArgument("one of httpsTrigger or eventTrigger is required")
	}
	if in.GetEntryPoint() == "" {
		// Google defaults the entry point to the function's name.
		in.EntryPoint = name[strings.LastIndex(name, "/")+1:]
	}
	mb := in.GetAvailableMemoryMb()
	if mb == 0 {
		mb = 256
	}
	timeout := int32(60)
	if d := in.GetTimeout(); d != nil {
		if d.AsDuration() <= 0 || d.AsDuration() > 540*time.Second {
			return nil, apierror.InvalidArgument("timeout must be between 1s and 540s for a 1st gen function")
		}
		timeout = int32(d.AsDuration() / time.Second)
	}
	fn := &functionspb.Function{
		Name: name, Description: in.GetDescription(), Labels: in.GetLabels(), Environment: functionspb.Environment_GEN_1,
		BuildConfig: &functionspb.BuildConfig{Runtime: in.GetRuntime(), EntryPoint: in.GetEntryPoint(),
			EnvironmentVariables: in.GetBuildEnvironmentVariables(),
			Source:               &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: src}}},
		ServiceConfig: &functionspb.ServiceConfig{AvailableMemory: fmt.Sprintf("%dM", mb), TimeoutSeconds: timeout,
			EnvironmentVariables: in.GetEnvironmentVariables(), MaxInstanceCount: in.GetMaxInstances(),
			MinInstanceCount: in.GetMinInstances(), ServiceAccountEmail: in.GetServiceAccountEmail(),
			IngressSettings: functionspb.ServiceConfig_IngressSettings(in.GetIngressSettings())},
	}
	if fn.ServiceConfig.ServiceAccountEmail == "" {
		fn.ServiceConfig.ServiceAccountEmail = gen1Account(project)
	}
	if et := in.GetEventTrigger(); et != nil {
		t, err := v2Trigger(project, et)
		if err != nil {
			return nil, err
		}
		t.ServiceAccountEmail = fn.ServiceConfig.ServiceAccountEmail
		fn.EventTrigger = t
	}
	return fn, nil
}

// v1Status is a v2 state in v1's terms: a failed deployment is OFFLINE.
func v1Status(s functionspb.Function_State) fv1.CloudFunctionStatus {
	switch s {
	case functionspb.Function_ACTIVE:
		return fv1.CloudFunctionStatus_ACTIVE
	case functionspb.Function_DEPLOYING:
		return fv1.CloudFunctionStatus_DEPLOY_IN_PROGRESS
	case functionspb.Function_DELETING:
		return fv1.CloudFunctionStatus_DELETE_IN_PROGRESS
	case functionspb.Function_FAILED:
		return fv1.CloudFunctionStatus_OFFLINE
	}
	return fv1.CloudFunctionStatus_UNKNOWN
}

// v1Of answers the v1 shape of a stored 1st-gen function: the caller's
// CloudFunction, with Google's defaults and the outputs filled. The caller
// holds functions.mu.
func v1Of(sf *storedFunction) *fv1.CloudFunction {
	out := proto.Clone(sf.v1).(*fv1.CloudFunction)
	fn := sf.fn
	out.Name, out.Status, out.UpdateTime, out.VersionId = fn.GetName(), v1Status(fn.GetState()), fn.GetUpdateTime(), sf.version
	sc := fn.GetServiceConfig()
	out.EntryPoint = fn.GetBuildConfig().GetEntryPoint()
	if out.AvailableMemoryMb == 0 {
		out.AvailableMemoryMb = 256
	}
	if out.Timeout == nil {
		out.Timeout = durationpb.New(time.Duration(sc.GetTimeoutSeconds()) * time.Second)
	}
	out.ServiceAccountEmail = sc.GetServiceAccountEmail()
	if out.IngressSettings == fv1.CloudFunction_INGRESS_SETTINGS_UNSPECIFIED {
		out.IngressSettings = fv1.CloudFunction_ALLOW_ALL
	}
	if out.DockerRegistry == fv1.CloudFunction_DOCKER_REGISTRY_UNSPECIFIED {
		out.DockerRegistry = fv1.CloudFunction_ARTIFACT_REGISTRY
	}
	if b := fn.GetBuildConfig().GetBuild(); b != "" {
		out.BuildName, out.BuildId = b, b[strings.LastIndex(b, "/")+1:]
	}
	if ht := out.GetHttpsTrigger(); ht != nil {
		ht.Url = v1URL(fn.GetName())
		if ht.SecurityLevel == fv1.HttpsTrigger_SECURITY_LEVEL_UNSPECIFIED {
			ht.SecurityLevel = fv1.HttpsTrigger_SECURE_OPTIONAL
		}
	}
	if et := out.GetEventTrigger(); et != nil && et.Service == "" {
		if strings.HasPrefix(et.GetEventType(), "google.pubsub") || strings.HasPrefix(et.GetEventType(), "providers/cloud.pubsub") {
			et.Service = "pubsub.googleapis.com"
		} else {
			et.Service = "storage.googleapis.com"
		}
	}
	return out
}

// v1URL is the URL Google gives a 1st-gen HTTP function,
// https://{region}-{project}.cloudfunctions.net/{name}; it is reported as
// Google reports it, while the function is reached at its Cloud Run
// service's URL (the v2 API's url, and call).
func v1URL(name string) string {
	p := strings.Split(name, "/")
	return fmt.Sprintf("https://%s-%s.cloudfunctions.net/%s", p[3], p[1], p[5])
}

// gen1 reads a stored function through v1, which sees 1st-gen ones only.
// The caller holds functions.mu.
func gen1(name string) (*storedFunction, error) {
	if _, _, err := parseFunctionName(name); err != nil {
		return nil, err
	}
	sf, ok := functions.fns[name]
	if !ok {
		return nil, functionNotFound(name)
	}
	if sf.v1 == nil {
		return nil, apierror.FailedPrecondition("Function %s is a 2nd gen function: manage it through the Cloud Functions v2 API", name)
	}
	return sf, nil
}

// GetFunction reads a 1st-gen function.
func (v *FunctionsV1Server) GetFunction(_ context.Context, req *fv1.GetFunctionRequest) (*fv1.CloudFunction, error) {
	functions.mu.Lock()
	defer functions.mu.Unlock()
	sf, err := gen1(req.GetName())
	if err != nil {
		return nil, err
	}
	if req.GetVersionId() != 0 && req.GetVersionId() != sf.version {
		return nil, apierror.Unimplemented("GetFunction version_id is not supported: only the current version is kept")
	}
	return v1Of(sf), nil
}

// ListFunctions lists a location's 1st-gen functions, or every location's
// for locations/-, by name and paged.
func (v *FunctionsV1Server) ListFunctions(_ context.Context, req *fv1.ListFunctionsRequest) (*fv1.ListFunctionsResponse, error) {
	project, location, err := resource.ParseLocation(req.GetParent())
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	prefix := req.GetParent() + "/functions/"
	if location == "-" {
		prefix = "projects/" + project + "/locations/"
	}
	functions.mu.Lock()
	byName := map[string]*fv1.CloudFunction{}
	var names []string
	for name, sf := range functions.fns {
		if sf.v1 != nil && strings.HasPrefix(name, prefix) {
			byName[name] = v1Of(sf)
			names = append(names, name)
		}
	}
	functions.mu.Unlock()
	sort.Strings(names)
	page, next, err := paging.Page("functions-v1:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	out := &fv1.ListFunctionsResponse{NextPageToken: next}
	for _, n := range page {
		out.Functions = append(out.Functions, byName[n])
	}
	return out, nil
}

// newV1Operation is a v1 operation, named operations/{id} as Google names
// them, with OperationMetadataV1.
func (v *FunctionsV1Server) newV1Operation(name string, typ fv1.OperationType, request proto.Message, version int64) string {
	id := base64.RawURLEncoding.EncodeToString([]byte(strings.TrimPrefix(name, "projects/") + "/" + uuid.NewString()[:8]))
	op := v.f.s.ops.CreateNamed("operations/"+id, name)
	md := &fv1.OperationMetadataV1{Target: name, Type: typ, UpdateTime: timestamppb.Now(), VersionId: version}
	if request != nil {
		md.Request, _ = anypb.New(request)
	}
	_ = v.f.s.ops.SetMetadata(op.Name, md)
	return op.Name
}

// finishV1 is the end of a deploy for a 1st-gen function: its operation's
// metadata names the build, and its response is the v1 CloudFunction. The
// caller holds functions.mu.
func (f *FunctionsServer) finishV1(opName string, sf *storedFunction) proto.Message {
	o, err := f.s.ops.Get(opName)
	if err == nil {
		if md, ok := o.Metadata.(*fv1.OperationMetadataV1); ok {
			md = proto.Clone(md).(*fv1.OperationMetadataV1)
			md.UpdateTime = timestamppb.Now()
			if b := sf.fn.GetBuildConfig().GetBuild(); b != "" {
				md.BuildName, md.BuildId = b, b[strings.LastIndex(b, "/")+1:]
			}
			_ = f.s.ops.SetMetadata(opName, md)
		}
	}
	return v1Of(sf)
}

// prepareV1 checks a v1 function and returns the v2 function it is.
func (v *FunctionsV1Server) prepareV1(name string, in *fv1.CloudFunction) (*functionspb.Function, error) {
	if err := unsupportedV1(in); err != nil {
		return nil, err
	}
	fn, err := toV2(name, in)
	if err != nil {
		return nil, err
	}
	if err := v.f.checkEventTrigger(fn.GetEventTrigger()); err != nil {
		return nil, err
	}
	if in.GetRuntime() == "" {
		return nil, apierror.InvalidArgument("runtime is required")
	}
	if err := checkRuntime(in.GetRuntime()); err != nil {
		return nil, apierror.InvalidArgument("runtime %q is not supported", in.GetRuntime())
	}
	if _, _, _, err := sourceOf(fn.GetBuildConfig()); err != nil {
		return nil, err
	}
	return fn, nil
}

// CreateFunction stores a 1st-gen function as DEPLOY_IN_PROGRESS and
// returns its operation; the build and the service's rollout run behind it.
func (v *FunctionsV1Server) CreateFunction(ctx context.Context, req *fv1.CreateFunctionRequest) (*longrunningpb.Operation, error) {
	if _, _, err := resource.ParseLocation(req.GetLocation()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	in := req.GetFunction()
	if in == nil {
		return nil, apierror.InvalidArgument("function is required")
	}
	parent, id, err := parseFunctionName(in.GetName())
	if err != nil {
		return nil, err
	}
	if parent != req.GetLocation() {
		return nil, apierror.InvalidArgument("function.name %q is not in location %q", in.GetName(), req.GetLocation())
	}
	if err := ValidateServiceID(id); err != nil {
		return nil, apierror.InvalidArgument("invalid function name %q: it must start with a lower case letter and contain only lower case letters, digits and hyphens, at most 63", id)
	}
	spec := proto.Clone(in).(*fv1.CloudFunction)
	fn, err := v.prepareV1(in.GetName(), spec)
	if err != nil {
		return nil, err
	}
	now := timestamppb.Now()
	fn.State, fn.CreateTime, fn.UpdateTime = functionspb.Function_DEPLOYING, now, now
	defaultServiceConfig(fn)
	defaultEventTrigger(fn)
	functions.mu.Lock()
	if _, ok := functions.fns[in.GetName()]; ok {
		functions.mu.Unlock()
		return nil, apierror.AlreadyExists("Function %s already exists", in.GetName())
	}
	functions.fns[in.GetName()] = &storedFunction{fn: fn, v1: spec, version: 1}
	functions.mu.Unlock()
	op := v.newV1Operation(in.GetName(), fv1.OperationType_CREATE_FUNCTION, req, 1)
	go v.f.deploy(context.WithoutCancel(ctx), op, in.GetName(), true, true)
	return v.f.s.toProtoOperation(op)
}

// v1Path is the CloudFunction field a mask path names, either spelling.
func v1Path(msg protoreflect.Message, p string) (protoreflect.FieldDescriptor, string) {
	head, sub, _ := strings.Cut(p, ".")
	fds := msg.Descriptor().Fields()
	fd := fds.ByName(protoreflect.Name(head))
	if fd == nil {
		fd = fds.ByJSONName(head)
	}
	return fd, sub
}

// applyV1Path copies one update mask path from src onto dst.
func applyV1Path(dst, src protoreflect.Message, p string) error {
	fd, sub := v1Path(dst, p)
	if fd == nil {
		return apierror.InvalidArgument("update_mask path %q is not a CloudFunction field", p)
	}
	if sub != "" && fd.Message() != nil && !fd.IsMap() && !fd.IsList() {
		if !dst.Has(fd) {
			dst.Set(fd, dst.NewField(fd))
		}
		s := src.Get(fd).Message()
		if !src.Has(fd) {
			s = dst.Get(fd).Message().Type().New()
		}
		return applyV1Path(dst.Mutable(fd).Message(), s, sub)
	}
	if src.Has(fd) {
		dst.Set(fd, src.Get(fd))
	} else {
		dst.Clear(fd)
	}
	return nil
}

// UpdateFunction applies the update mask, or replaces the function with no
// mask, and redeploys it: a change of its source, runtime, entry point or
// build environment rebuilds it; one of its trigger remakes the trigger; a
// description alone changes nothing that runs. The trigger's kind, HTTP or
// event, cannot change, as on Google.
func (v *FunctionsV1Server) UpdateFunction(ctx context.Context, req *fv1.UpdateFunctionRequest) (*longrunningpb.Operation, error) {
	in := req.GetFunction()
	if in == nil {
		return nil, apierror.InvalidArgument("function is required")
	}
	name := in.GetName()
	paths := req.GetUpdateMask().GetPaths()
	functions.mu.Lock()
	sf, err := gen1(name)
	if err != nil {
		functions.mu.Unlock()
		return nil, err
	}
	if sf.fn.State == functionspb.Function_DEPLOYING {
		functions.mu.Unlock()
		return nil, apierror.FailedPrecondition("An operation on function %s is already in progress", name)
	}
	spec := proto.Clone(sf.v1).(*fv1.CloudFunction)
	build, serve, trig := false, false, false
	if len(paths) == 0 {
		spec = proto.Clone(in).(*fv1.CloudFunction)
		build, serve, trig = true, true, true
	}
	for _, p := range paths {
		head, _, _ := strings.Cut(p, ".")
		switch head {
		case "name", "status", "updateTime", "update_time", "versionId", "version_id", "buildId", "build_id", "buildName", "build_name":
			functions.mu.Unlock()
			return nil, apierror.InvalidArgument("update_mask path %q is output only or immutable", p)
		}
		if err := applyV1Path(spec.ProtoReflect(), in.ProtoReflect(), p); err != nil {
			functions.mu.Unlock()
			return nil, err
		}
		switch head {
		case "description":
		case "sourceArchiveUrl", "source_archive_url", "sourceUploadUrl", "source_upload_url", "sourceRepository", "source_repository",
			"runtime", "entryPoint", "entry_point", "buildEnvironmentVariables", "build_environment_variables":
			build, serve = true, true
		case "eventTrigger", "event_trigger", "httpsTrigger", "https_trigger":
			trig = true
		default:
			serve = true
		}
	}
	spec.Name = name
	if (sf.v1.GetHttpsTrigger() != nil) != (spec.GetHttpsTrigger() != nil) {
		functions.mu.Unlock()
		return nil, apierror.InvalidArgument("The trigger type of function %s cannot be changed between HTTP and event", name)
	}
	fn, err := v.prepareV1(name, spec)
	if err != nil {
		functions.mu.Unlock()
		return nil, err
	}
	fn.CreateTime, fn.UpdateTime = sf.fn.GetCreateTime(), timestamppb.Now()
	defaultServiceConfig(fn)
	defaultEventTrigger(fn)
	fn.BuildConfig.Build, fn.BuildConfig.DockerRegistry = sf.fn.GetBuildConfig().GetBuild(), sf.fn.GetBuildConfig().GetDockerRegistry()
	fn.ServiceConfig.Service, fn.ServiceConfig.Uri = sf.fn.GetServiceConfig().GetService(), sf.fn.GetServiceConfig().GetUri()
	fn.ServiceConfig.Revision, fn.Url = sf.fn.GetServiceConfig().GetRevision(), sf.fn.GetUrl()
	fn.State = sf.fn.GetState()
	if build || serve || trig {
		fn.State = functionspb.Function_DEPLOYING
	}
	sf.fn, sf.v1 = fn, spec
	sf.version++
	version := sf.version
	functions.mu.Unlock()
	op := v.newV1Operation(name, fv1.OperationType_UPDATE_FUNCTION, req, version)
	if !build && !serve && !trig {
		functions.mu.Lock()
		resp := v.f.finishV1(op, sf)
		functions.mu.Unlock()
		_ = v.f.s.ops.Succeed(op, resp)
		return v.f.s.toProtoOperation(op)
	}
	go v.f.deploy(context.WithoutCancel(ctx), op, name, build, serve)
	return v.f.s.toProtoOperation(op)
}

// DeleteFunction deletes a 1st-gen function and its service, as the v2
// API's delete does.
func (v *FunctionsV1Server) DeleteFunction(ctx context.Context, req *fv1.DeleteFunctionRequest) (*longrunningpb.Operation, error) {
	functions.mu.Lock()
	sf, err := gen1(req.GetName())
	functions.mu.Unlock()
	if err != nil {
		return nil, err
	}
	op := v.newV1Operation(req.GetName(), fv1.OperationType_DELETE_FUNCTION, req, sf.version)
	if err := v.f.deleteFunction(ctx, op, req.GetName()); err != nil {
		return nil, err
	}
	return v.f.s.toProtoOperation(op)
}

// GenerateUploadUrl returns a URL on this port that takes the source zip
// with a PUT and no credentials; a create names it as sourceUploadUrl.
func (v *FunctionsV1Server) GenerateUploadUrl(ctx context.Context, req *fv1.GenerateUploadUrlRequest) (*fv1.GenerateUploadUrlResponse, error) {
	resp, err := v.f.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: req.GetParent(), KmsKeyName: req.GetKmsKeyName(),
		Environment: functionspb.Environment_GEN_1})
	if err != nil {
		return nil, err
	}
	return &fv1.GenerateUploadUrlResponse{UploadUrl: resp.GetUploadUrl()}, nil
}

// GenerateDownloadUrl returns a URL on this port that reads the source the
// function was built from.
func (v *FunctionsV1Server) GenerateDownloadUrl(ctx context.Context, req *fv1.GenerateDownloadUrlRequest) (*fv1.GenerateDownloadUrlResponse, error) {
	functions.mu.Lock()
	sf, err := gen1(req.GetName())
	functions.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if req.GetVersionId() != 0 && int64(req.GetVersionId()) != sf.version {
		return nil, apierror.Unimplemented("GenerateDownloadUrl version_id is not supported: only the current version is kept")
	}
	resp, err := v.f.GenerateDownloadUrl(ctx, &functionspb.GenerateDownloadUrlRequest{Name: req.GetName()})
	if err != nil {
		return nil, err
	}
	return &fv1.GenerateDownloadUrlResponse{DownloadUrl: resp.GetDownloadUrl()}, nil
}

// maxCallResult caps the response body a call answers: call is for
// testing, and Google truncates its result too.
const maxCallResult = 100 << 10

// CallFunction invokes an HTTP function with data as its request body,
// through the cluster's ingress, and answers its response body as result,
// or its error as error when it answers an error status. An event
// function is not called: publish to its topic or write to its bucket.
func (v *FunctionsV1Server) CallFunction(ctx context.Context, req *fv1.CallFunctionRequest) (*fv1.CallFunctionResponse, error) {
	functions.mu.Lock()
	sf, err := gen1(req.GetName())
	var fn *functionspb.Function
	if err == nil {
		fn = cloneFunction(sf.fn)
		if sf.v1.GetHttpsTrigger() == nil {
			err = apierror.Unimplemented("call of an event-triggered function is not supported: its events are delivered " +
				"through its trigger, so publish to its topic or write to its bucket")
		}
	}
	functions.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if fn.GetState() != functionspb.Function_ACTIVE {
		return nil, apierror.FailedPrecondition("Function %s is not ACTIVE", req.GetName())
	}
	ingress := v.f.ingress
	if ingress == "" {
		return nil, apierror.FailedPrecondition("no ingress port is published on this machine, so the function cannot be called")
	}
	u, err := url.Parse(fn.GetServiceConfig().GetUri())
	if err != nil || u.Host == "" {
		return nil, apierror.FailedPrecondition("Function %s has no service URL", req.GetName())
	}
	timeout := time.Duration(fn.GetServiceConfig().GetTimeoutSeconds()) * time.Second
	cctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	hr, err := http.NewRequestWithContext(cctx, http.MethodPost, "http://"+ingress+"/", strings.NewReader(req.GetData()))
	if err != nil {
		return nil, apierror.Internal(err, "build the call")
	}
	hr.Host = u.Host
	hr.Header.Set("Content-Type", "text/plain")
	if json.Valid([]byte(req.GetData())) {
		hr.Header.Set("Content-Type", "application/json")
	}
	if v.f.tokens != nil {
		tok, err := v.f.tokens.ServiceAccountIDToken(fn.GetServiceConfig().GetServiceAccountEmail(), strings.TrimSuffix(fn.GetServiceConfig().GetUri(), "/"))
		if err != nil {
			return nil, apierror.Internal(err, "mint the call's token")
		}
		hr.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return nil, &apierror.Error{Code: codes.Unavailable, Message: fmt.Sprintf("call %s: %v", req.GetName(), err)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxCallResult))
	out := &fv1.CallFunctionResponse{ExecutionId: strings.ReplaceAll(uuid.NewString(), "-", "")[:12]}
	if resp.StatusCode >= 400 {
		out.Error = fmt.Sprintf("%d %s: %s", resp.StatusCode, http.StatusText(resp.StatusCode), body)
		return out, nil
	}
	out.Result = string(body)
	return out, nil
}

// SetIamPolicy, GetIamPolicy and TestIamPermissions are the function's
// stored policy (ADR-0006), the one the v2 API reads.
func (v *FunctionsV1Server) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	return functionsIAM{v.f}.SetIamPolicy(ctx, req)
}

func (v *FunctionsV1Server) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return functionsIAM{v.f}.GetIamPolicy(ctx, req)
}

func (v *FunctionsV1Server) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return functionsIAM{v.f}.TestIamPermissions(ctx, req)
}

// listV1Operations is v1's operations.list: name "operations", and a filter
// of project:{p}[,location:{l}][,function:{f}][,latest:true] as Google
// documents it, newest first.
func (s *Server) listV1Operations(req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	if n := req.GetName(); n != "" && n != "operations" {
		return nil, apierror.InvalidArgument("name %q must be \"operations\"", n)
	}
	var project, location, function string
	latest := false
	for _, term := range strings.Split(req.GetFilter(), ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(term), ":")
		if !ok {
			return nil, apierror.InvalidArgument("filter %q must be project:{project}[,location:{location}][,function:{function}][,latest:true]", req.GetFilter())
		}
		switch k {
		case "project":
			project = val
		case "location":
			location = val
		case "function":
			function = val
		case "latest":
			latest = val == "true"
		default:
			return nil, apierror.InvalidArgument("filter term %q is not supported: project, location, function and latest are", term)
		}
	}
	if project == "" {
		return nil, apierror.InvalidArgument("filter must name the project: project:{project}")
	}
	type entry struct {
		p      *longrunningpb.Operation
		target string
		at     time.Time
	}
	var all []entry
	for _, op := range s.ops.List("operations/") {
		parts := strings.Split(op.Target, "/")
		if len(parts) != 6 || parts[1] != project || (location != "" && parts[3] != location) || (function != "" && parts[5] != function) {
			continue
		}
		p, err := s.toProtoOperation(op.Name)
		if err != nil {
			continue
		}
		at := time.Time{}
		if md, ok := op.Metadata.(*fv1.OperationMetadataV1); ok {
			at = md.GetUpdateTime().AsTime()
		}
		all = append(all, entry{p, op.Target, at})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	seen := map[string]bool{}
	byName := map[string]*longrunningpb.Operation{}
	var names []string
	for _, e := range all {
		if latest && seen[e.target] {
			continue
		}
		seen[e.target] = true
		byName[e.p.GetName()] = e.p
		names = append(names, e.p.GetName())
	}
	page, next, err := paging.Page("functions-v1-operations:"+req.GetFilter(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	out := &longrunningpb.ListOperationsResponse{NextPageToken: next}
	for _, n := range page {
		out.Operations = append(out.Operations, byName[n])
	}
	return out, nil
}

// functionsV1Mixins are cloudfunctions_v1.yaml's Operations bindings; its
// Locations binding, /v1/{name=projects/*}/locations, is the Cloud Run v1
// API's path too, which answers it.
var functionsV1Mixins = []*annotations.HttpRule{
	{Selector: "google.longrunning.Operations.GetOperation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=operations/*}"}},
	{Selector: "google.longrunning.Operations.ListOperations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/operations"}},
}

// FunctionsV1Transcoder is the Cloud Functions v1 JSON API's configuration.
func FunctionsV1Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.functions.v1"}, Mixins: functionsV1Mixins, SnakeCaseMasks: true}
}

// isFunctionsV1Path reports whether a path is Cloud Functions v1's: its
// operations, and the functions of a location, which Cloud Run v1 never
// names.
func isFunctionsV1Path(p string) bool {
	if p == "/v1/operations" || strings.HasPrefix(p, "/v1/operations/") {
		return true
	}
	rest, ok := strings.CutPrefix(p, "/v1/projects/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	return len(parts) >= 4 && parts[1] == "locations" && strings.SplitN(parts[3], ":", 2)[0] == "functions"
}
