package run

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// IAM enforcement of the Cloud Run v1 API (#1879). The v1 (Knative-shaped)
// handlers serve each request through the v2 methods, called in process
// rather than over gRPC, so the v2 interceptors never see them: each v1
// handler instead asks V1Authorize about the v2 request it makes (or, for
// a read the v2 methods do not serve, the v2 request it stands for), with
// the v1 request's Authorization header as the call's incoming metadata.
// The v2 table's permissions and iam.serviceAccounts.actAs then apply to
// both APIs alike.

// V1Authorizer checks the v1 API's requests; a nil func checks none.
type V1Authorizer struct {
	// Method checks a v2 request a v1 request makes, by its gRPC full
	// method; nil when it may run.
	Method func(ctx context.Context, fullMethod string, req proto.Message) error
	// Permission checks a permission on a resource, for a v1 method no
	// v2 method stands for (routes, configurations, operations).
	Permission func(ctx context.Context, permission, resource string) error
}

var v1Authorizer atomic.Pointer[V1Authorizer]

// UseV1Authorize makes the v1 API check its requests with a (nil: none).
func UseV1Authorize(a *V1Authorizer) { v1Authorizer.Store(a) }

var (
	v2MethodsOnce sync.Once
	v2Methods     map[protoreflect.FullName]string
)

// v2Method is the v2 full method whose request is req's type, "" for none.
func v2Method(req proto.Message) string {
	v2MethodsOnce.Do(func() {
		v2Methods = map[protoreflect.FullName]string{}
		for _, fd := range []protoreflect.FileDescriptor{runpb.File_google_cloud_run_v2_service_proto, runpb.File_google_cloud_run_v2_revision_proto,
			runpb.File_google_cloud_run_v2_job_proto, runpb.File_google_cloud_run_v2_execution_proto, runpb.File_google_cloud_run_v2_task_proto,
			runpb.File_google_cloud_run_v2_worker_pool_proto, runpb.File_google_cloud_run_v2_instance_proto} {
			for i := 0; i < fd.Services().Len(); i++ {
				sd := fd.Services().Get(i)
				for j := 0; j < sd.Methods().Len(); j++ {
					md := sd.Methods().Get(j)
					v2Methods[md.Input().FullName()] = "/" + string(sd.FullName()) + "/" + string(md.Name())
				}
			}
		}
	})
	return v2Methods[req.ProtoReflect().Descriptor().FullName()]
}

// v1Check asks the authorizer about req, a v2 request; nil when none is
// set or it may run.
func v1Check(ctx context.Context, req proto.Message) error {
	return v1CheckAs(ctx, v2Method(req), req)
}

// v1CheckAs is v1Check of a request checked as method (an IAMPolicy
// request, which every v2 service shares).
func v1CheckAs(ctx context.Context, method string, req proto.Message) error {
	a := v1Authorizer.Load()
	if a == nil || a.Method == nil || method == "" {
		return nil
	}
	return a.Method(ctx, method, req)
}

// v1CheckPermission asks the authorizer about permission on resource.
func v1CheckPermission(ctx context.Context, permission, resource string) error {
	a := v1Authorizer.Load()
	if a == nil || a.Permission == nil {
		return nil
	}
	return a.Permission(ctx, permission, resource)
}

// v1Call checks req, then calls f with it.
func v1Call[Req proto.Message, Resp any](ctx context.Context, f func(context.Context, Req) (Resp, error), req Req) (Resp, error) {
	if err := v1Check(ctx, req); err != nil {
		var zero Resp
		return zero, err
	}
	return f(ctx, req)
}

// v1Context is a v1 request's context with its Authorization header as
// incoming gRPC metadata, which the authorizer reads the caller from.
func v1Context(r *http.Request) *http.Request {
	if v1Authorizer.Load() == nil {
		return r
	}
	md := metadata.MD{}
	if a := r.Header.Get("Authorization"); a != "" {
		md.Set("authorization", a)
	}
	if old, ok := metadata.FromIncomingContext(r.Context()); ok {
		md = metadata.Join(old, md)
	}
	return r.WithContext(metadata.NewIncomingContext(r.Context(), md))
}

// parent is "projects/P/locations/L" for a v1 scope, L "-" when the
// request names no region.
func (sc v1Scope) parent() string {
	loc := sc.location
	if loc == "" {
		loc = "-"
	}
	return "projects/" + sc.project + "/locations/" + loc
}

// v1Named is the v2 name a v1 read is checked on: found, the resource's
// name as read, or, for one not found, the scope's collection's id (the
// check comes first, so a denied caller learns nothing of it).
func v1Named(found string, sc v1Scope, collection, id string) string {
	if found != "" {
		return found
	}
	return sc.parent() + "/" + collection + "/" + id
}

// v1CheckIAM checks a v1 getIamPolicy or setIamPolicy (verb) on name, of
// collection (services, jobs, workerpools or instances), as its v2
// IAMPolicy method is checked; v2's Instances has none, so an instance's
// is run.instances.getIamPolicy or setIamPolicy. testIamPermissions
// answers as it did.
func v1CheckIAM(ctx context.Context, collection, verb, name string) error {
	method := map[string]string{"getIamPolicy": "GetIamPolicy", "setIamPolicy": "SetIamPolicy"}[verb]
	if method == "" {
		return nil
	}
	if collection == "instances" {
		return v1CheckPermission(ctx, "run.instances."+verb, name)
	}
	svc := map[string]string{"services": "Services", "jobs": "Jobs", "workerpools": "WorkerPools"}[collection]
	if svc == "" {
		return nil
	}
	var req proto.Message = &iampb.GetIamPolicyRequest{Resource: name}
	if method == "SetIamPolicy" {
		req = &iampb.SetIamPolicyRequest{Resource: name}
	}
	return v1CheckAs(ctx, "/google.cloud.run.v2."+svc+"/"+method, req)
}
