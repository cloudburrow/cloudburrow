package run

import (
	"context"
	"fmt"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/lro"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
)

// Server serves google.cloud.run.v2.Services by translating to Knative.
//
// Embedding UnimplementedServicesServer means an unmapped method returns
// Unimplemented rather than a stub that returns plausible empty success.
type Server struct {
	runpb.UnimplementedServicesServer
	kn  *Knative
	ops *lro.Store
	// instance labels everything this adapter creates.
	instance string
	// readyTimeout bounds how long a create waits for the revision.
	readyTimeout time.Duration
	// secrets resolves secretKeyRef environment variables. Nil when Secret
	// Manager is not enabled, in which case a reference is refused rather
	// than silently dropped.
	secrets SecretResolver
}

// WithSecrets attaches a secret resolver, enabling secretKeyRef environment
// variables on deployed revisions.
func (s *Server) WithSecrets(r SecretResolver) *Server {
	s.secrets = r
	return s
}

// NewServer returns a Cloud Run adapter.
func NewServer(kn *Knative, instance string, readyTimeout time.Duration) *Server {
	if readyTimeout <= 0 {
		readyTimeout = 5 * time.Minute
	}
	return &Server{kn: kn, ops: lro.NewStore(time.Now), instance: instance, readyTimeout: readyTimeout}
}

// Register adds this service to a gRPC server.
func (s *Server) Register(g *grpc.Server) { runpb.RegisterServicesServer(g, s) }

// CreateService deploys a Knative Service and returns a long-running operation.
//
// Cloud Run returns an operation here, so a client polls until the revision is
// ready. Returning a completed operation immediately would tell the caller the
// container was serving before it had started.
func (s *Server) CreateService(ctx context.Context, req *runpb.CreateServiceRequest) (*longrunningpb.Operation, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	id := req.GetServiceId()
	if err := ValidateServiceID(id); err != nil {
		return nil, err
	}
	svc := req.GetService()
	if svc == nil {
		return nil, apierror.InvalidArgument("service is required")
	}
	name := fmt.Sprintf("%s/services/%s", req.GetParent(), id)
	svc.Name = name

	if _, err := s.kn.Get(ctx, id); err == nil {
		return nil, apierror.AlreadyExists("service %s already exists", name)
	}

	manifest, err := ToKnative(svc, s.kn.Namespace, s.instance, s.secrets)
	if err != nil {
		return nil, err
	}
	if err := s.kn.Apply(ctx, manifest); err != nil {
		return nil, err
	}

	op := s.ops.Create(req.GetParent(), name)
	go s.awaitReady(context.WithoutCancel(ctx), op.Name, id, req.GetParent(), 0)
	return s.toProtoOperation(op.Name)
}

// UpdateService replaces a service's configuration, which Knative turns into
// a new revision (#300).
//
// The whole service is replaced, as `gcloud run deploy` and Terraform send
// it. An update_mask naming a subset is refused rather than honoured partly:
// merging a mask onto a Knative template field by field is not implemented,
// and applying the full service while claiming to have applied a subset
// would change fields the caller meant to leave alone.
//
// The operation completes when Knative has reconciled the new generation and
// its revision is ready, not when the old revision still reports Ready.
func (s *Server) UpdateService(ctx context.Context, req *runpb.UpdateServiceRequest) (*longrunningpb.Operation, error) {
	svc := req.GetService()
	if svc == nil {
		return nil, apierror.InvalidArgument("service is required")
	}
	id, err := ServiceID(svc.GetName())
	if err != nil {
		return nil, err
	}
	if paths := req.GetUpdateMask().GetPaths(); len(paths) > 0 {
		return nil, apierror.Unimplemented(
			"update_mask %v is not supported: send the full service, which replaces its configuration", paths)
	}
	parent := strings.TrimSuffix(svc.GetName(), "/services/"+id)
	existing, err := s.kn.Get(ctx, id)
	if err != nil {
		if !req.GetAllowMissing() {
			return nil, apierror.NotFound("service %s not found", svc.GetName())
		}
		return s.CreateService(ctx, &runpb.CreateServiceRequest{
			Parent: parent, ServiceId: id, Service: svc, ValidateOnly: req.GetValidateOnly()})
	}
	if existing.Metadata.Labels["cloudburrow.dev/owned"] != "true" {
		return nil, apierror.FailedPrecondition(
			"service %s was not created by CloudBurrow and will not be updated", svc.GetName())
	}
	manifest, err := ToKnative(svc, s.kn.Namespace, s.instance, s.secrets)
	if err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		op := s.ops.Create(parent, svc.GetName())
		_ = s.ops.Succeed(op.Name, FromKnative(existing, parent))
		return s.toProtoOperation(op.Name)
	}
	if err := s.kn.Apply(ctx, manifest); err != nil {
		return nil, err
	}
	applied, err := s.kn.Get(ctx, id)
	if err != nil {
		return nil, apierror.Internal(err, "read back service %s", id)
	}
	op := s.ops.Create(parent, svc.GetName())
	go s.awaitReady(context.WithoutCancel(ctx), op.Name, id, parent, applied.Metadata.Generation)
	return s.toProtoOperation(op.Name)
}

// awaitReady polls Knative until the service is ready or the bound elapses.
//
// This is bounded polling, not an injected clock: Knative is an external
// component whose clock we cannot advance (architecture §9).
//
// minGeneration, when set, is the spec generation that must have been
// reconciled first: after an update, Ready=True can still describe the
// previous revision until Knative observes the new spec.
func (s *Server) awaitReady(ctx context.Context, opName, id, parent string, minGeneration int64) {
	deadline := time.Now().Add(s.readyTimeout)
	for time.Now().Before(deadline) {
		k, err := s.kn.Get(ctx, id)
		// Only once the new spec is observed do the conditions describe it.
		if err == nil && k.Status.ObservedGeneration >= minGeneration {
			ready, reason := k.Ready()
			// After an update, Ready with an older latest-ready revision is
			// the old revision still serving, not the new one being ready.
			current := minGeneration == 0 || k.Status.LatestReadyRevisionName == k.Status.LatestCreatedRevisionName
			if ready && current {
				_ = s.ops.Succeed(opName, FromKnative(k, parent))
				return
			}
			if reason != "" {
				_ = s.ops.Fail(opName, apierror.FailedPrecondition("revision failed: %s", reason))
				return
			}
			if minGeneration > 0 {
				if msg := k.latestCreatedFailure(); msg != "" {
					_ = s.ops.Fail(opName, apierror.FailedPrecondition("revision failed: %s", msg))
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	_ = s.ops.Fail(opName, apierror.FailedPrecondition(
		"service %s did not become ready within %s", id, s.readyTimeout))
}

// toProtoOperation renders a tracked operation.
func (s *Server) toProtoOperation(name string) (*longrunningpb.Operation, error) {
	op, err := s.ops.Get(name)
	if err != nil {
		return nil, apierror.NotFound("operation %s not found", name)
	}
	out := &longrunningpb.Operation{Name: op.Name, Done: op.Done()}
	if op.State == lro.StateFailed && op.Error != nil {
		e := apierror.From(op.Error)
		out.Result = &longrunningpb.Operation_Error{Error: &rpcstatus.Status{Code: int32(e.Code), Message: e.Message}}
		return out, nil
	}
	if op.State == lro.StateSucceeded {
		if svc, ok := op.Response.(*runpb.Service); ok {
			any, err := anypb.New(svc)
			if err == nil {
				out.Result = &longrunningpb.Operation_Response{Response: any}
			}
		}
	}
	return out, nil
}

// GetService reads a service.
func (s *Server) GetService(ctx context.Context, req *runpb.GetServiceRequest) (*runpb.Service, error) {
	id, err := ServiceID(req.GetName())
	if err != nil {
		return nil, err
	}
	k, err := s.kn.Get(ctx, id)
	if err != nil {
		return nil, apierror.NotFound("service %s not found", req.GetName())
	}
	parent := strings.TrimSuffix(req.GetName(), "/services/"+id)
	return FromKnative(k, parent), nil
}

// ListServices lists services under a parent.
func (s *Server) ListServices(ctx context.Context, req *runpb.ListServicesRequest) (*runpb.ListServicesResponse, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	items, err := s.kn.List(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]*runpb.Service{}
	names := make([]string, 0, len(items))
	for _, k := range items {
		svc := FromKnative(k, req.GetParent())
		byName[svc.GetName()] = svc
		names = append(names, svc.GetName())
	}
	page, next, err := paging.Page("run:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	resp := &runpb.ListServicesResponse{NextPageToken: next}
	for _, n := range page {
		resp.Services = append(resp.Services, byName[n])
	}
	return resp, nil
}

// DeleteService removes a service.
func (s *Server) DeleteService(ctx context.Context, req *runpb.DeleteServiceRequest) (*longrunningpb.Operation, error) {
	id, err := ServiceID(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.kn.Delete(ctx, id); err != nil {
		return nil, err
	}
	parent := strings.TrimSuffix(req.GetName(), "/services/"+id)
	op := s.ops.Create(parent, req.GetName())
	_ = s.ops.Succeed(op.Name, &runpb.Service{Name: req.GetName()})
	return s.toProtoOperation(op.Name)
}

// Operation returns a tracked operation, so a client can poll a create.
func (s *Server) Operation(name string) (*longrunningpb.Operation, error) {
	return s.toProtoOperation(name)
}
