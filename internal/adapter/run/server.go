package run

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/lro"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
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
	// env is what every revision and job task is given (#576); nil
	// injects only GOOGLE_CLOUD_PROJECT.
	env Environment
	// cloudSQL serves Cloud SQL volumes (#1346); nil refuses them.
	cloudSQL CloudSQL
	// aliases are the metadata server's names for each workload account
	// (#1388).
	aliases metadataAliases
	// volumeMu orders a collection of unused volume objects after the
	// writes of workloads that mount them (volumegc.go).
	volumeMu sync.RWMutex
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

// Register adds this service to a gRPC server or a JSON transcoder.
func (s *Server) Register(g grpc.ServiceRegistrar) { runpb.RegisterServicesServer(g, s) }

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

	if _, err := s.service(ctx, name, id); err == nil {
		return nil, apierror.AlreadyExists("service %s already exists", name)
	}
	// Its Knative Service carries the project and location (#1400), so
	// another project's or region's service of this ID is another object.
	kname := serviceKName(name)
	if k, err := s.kn.Get(ctx, kname); err == nil {
		// Only an older version's service whose ID is this name can hold it.
		holder := k.Metadata.Annotations[annRunName]
		if holder == "" {
			holder = "another service"
		}
		return nil, apierror.FailedPrecondition("the Knative Service %s this service would be created as is held by %s", kname, holder)
	}

	manifest, err := renderService(svc, kname, s.kn.namespace(), s.instance, s.secrets, s.injectedFor(projectOf(name), svc.GetTemplate().GetServiceAccount()), s.cloudSQLConfig())
	if err != nil {
		return nil, err
	}
	if err := s.checkTrafficRevisions(ctx, name, kname, svc.GetTraffic()); err != nil {
		return nil, err
	}
	// validate_only: the request was checked in full, including the
	// mapping, and nothing is applied (#581). Before, the service was
	// deployed anyway.
	if req.GetValidateOnly() {
		op := s.ops.Create(req.GetParent(), name)
		_ = s.ops.Succeed(op.Name, svc)
		return s.toProtoOperation(op.Name)
	}
	if err := s.ensureWorkloadMetadata(ctx, projectOf(svc.GetName()), svc.GetTemplate().GetServiceAccount()); err != nil {
		return nil, err
	}
	release := s.holdVolumes(svc.GetTemplate().GetVolumes())
	defer release()
	if err := s.ensureGCSFuse(ctx, projectOf(svc.GetName()), svc.GetTemplate().GetVolumes()); err != nil {
		return nil, err
	}
	// Cloud Run's K_* names for the revision's pods (#1448, kenv.go).
	manifest = kEnvPolicy(s.kn.namespace()) + kEnvConfigMap(name, kname, s.kn.namespace(), s.instance, "") + manifest
	if err := s.kn.Apply(ctx, manifest); err != nil {
		return nil, err
	}

	op := s.ops.Create(req.GetParent(), name)
	go s.awaitReady(context.WithoutCancel(ctx), op.Name, kname, req.GetParent(), 0)
	return s.toProtoOperation(op.Name)
}

// UpdateService replaces a service's configuration, which Knative turns into
// a new revision (#300).
//
// With no update_mask the whole service is replaced, as `gcloud run deploy`
// and Terraform send it. A mask naming a subset is merged onto the service
// as it is read back from Knative, so only the named fields change (#1260).
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
	parent := strings.TrimSuffix(svc.GetName(), "/services/"+id)
	existing, err := s.service(ctx, svc.GetName(), id)
	if err != nil {
		if !req.GetAllowMissing() {
			return nil, apierror.NotFound("service %s not found", svc.GetName())
		}
		return s.CreateService(ctx, &runpb.CreateServiceRequest{
			Parent: parent, ServiceId: id, Service: svc, ValidateOnly: req.GetValidateOnly()})
	}
	if existing.Metadata.Labels[k8s.OwnedLabel] != k8s.OwnedValue {
		return nil, apierror.FailedPrecondition(
			"service %s was not created by CloudBurrow and will not be updated", svc.GetName())
	}
	if err := checkEtag(svc.GetEtag(), existing); err != nil {
		return nil, err
	}
	if paths := req.GetUpdateMask().GetPaths(); len(paths) > 0 {
		if svc, err = mergeServiceMask(FromKnative(existing, parent), svc, paths); err != nil {
			return nil, err
		}
	}
	// Updated in place, under the name it has: an older version's service
	// keeps its ID as its Knative name (#1400).
	kname := existing.Metadata.Name
	manifest, err := renderService(svc, kname, s.kn.namespace(), s.instance, s.secrets, s.injectedFor(projectOf(svc.GetName()), svc.GetTemplate().GetServiceAccount()), s.cloudSQLConfig())
	if err != nil {
		return nil, err
	}
	if err := s.checkTrafficRevisions(ctx, svc.GetName(), kname, svc.GetTraffic()); err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		op := s.ops.Create(parent, svc.GetName())
		_ = s.ops.Succeed(op.Name, FromKnative(existing, parent))
		return s.toProtoOperation(op.Name)
	}
	if err := s.ensureWorkloadMetadata(ctx, projectOf(svc.GetName()), svc.GetTemplate().GetServiceAccount()); err != nil {
		return nil, err
	}
	release := s.holdVolumes(svc.GetTemplate().GetVolumes())
	defer release()
	if err := s.ensureGCSFuse(ctx, projectOf(svc.GetName()), svc.GetTemplate().GetVolumes()); err != nil {
		return nil, err
	}
	manifest = kEnvPolicy(s.kn.namespace()) +
		kEnvConfigMap(svc.GetName(), kname, s.kn.namespace(), s.instance, existing.Status.LatestCreatedRevisionName) + manifest
	if err := s.kn.Apply(ctx, manifest); err != nil {
		return nil, err
	}
	// A volume the new revision dropped goes once no revision uses it.
	go s.collectVolumes(ctx)
	applied, err := s.kn.Get(ctx, kname)
	if err != nil {
		return nil, apierror.Internal(err, "read back service %s", id)
	}
	op := s.ops.Create(parent, svc.GetName())
	go s.awaitReady(context.WithoutCancel(ctx), op.Name, kname, parent, applied.Metadata.Generation)
	return s.toProtoOperation(op.Name)
}

// awaitReady polls Knative until the service, Knative Service kname, is
// ready or the bound elapses.
//
// This is bounded polling, not an injected clock: Knative is an external
// component whose clock we cannot advance (architecture §9).
//
// minGeneration, when set, is the spec generation that must have been
// reconciled first: after an update, Ready=True can still describe the
// previous revision until Knative observes the new spec.
func (s *Server) awaitReady(ctx context.Context, opName, kname, parent string, minGeneration int64) {
	deadline := time.Now().Add(s.readyTimeout)
	for time.Now().Before(deadline) {
		k, err := s.kn.Get(ctx, kname)
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
	target := kname
	if op, err := s.ops.Get(opName); err == nil && op.Target != "" {
		target = op.Target
	}
	_ = s.ops.Fail(opName, apierror.FailedPrecondition(
		"service %s did not become ready within %s", target, s.readyTimeout))
}

// toProtoOperation renders a tracked operation.
func (s *Server) toProtoOperation(name string) (*longrunningpb.Operation, error) {
	op, err := s.ops.Get(name)
	if err != nil {
		return nil, apierror.NotFound("operation %s not found", name)
	}
	out := &longrunningpb.Operation{Name: op.Name, Done: op.Done()}
	// Metadata while it runs: a RunJob operation carries its execution.
	if md, ok := op.Metadata.(proto.Message); ok {
		if a, err := anypb.New(md); err == nil {
			out.Metadata = a
		}
	}
	if op.State == lro.StateFailed && op.Error != nil {
		e := apierror.From(op.Error)
		out.Result = &longrunningpb.Operation_Error{Error: &rpcstatus.Status{Code: int32(e.Code), Message: e.Message}}
		return out, nil
	}
	if op.State == lro.StateSucceeded {
		// Any resource: a Service, a Revision, a Job or an Execution.
		if res, ok := op.Response.(proto.Message); ok {
			if a, err := anypb.New(res); err == nil {
				out.Result = &longrunningpb.Operation_Response{Response: a}
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
	k, err := s.service(ctx, req.GetName(), id)
	if err != nil {
		return nil, err
	}
	parent := strings.TrimSuffix(req.GetName(), "/services/"+id)
	return FromKnative(k, parent), nil
}

// service reads the Knative Service behind a Cloud Run service name: the
// scoped one (#1400), or one an older version created under the ID alone,
// and only if it is that name's, so another project's or region's service
// of the same ID is NOT_FOUND here, not returned under the wrong name.
// Callers reach Knative by the returned object's own name.
func (s *Server) service(ctx context.Context, name, id string) (ksvc, error) {
	for _, kname := range []string{serviceKName(name), id} {
		k, err := s.kn.Get(ctx, kname)
		if err != nil {
			continue
		}
		if n := k.Metadata.Annotations[annRunName]; n != "" && n != name {
			continue
		}
		return k, nil
	}
	return ksvc{}, apierror.NotFound("service %s not found", name)
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
		// Only the parent's: every project's services are in the one
		// namespace (#1399).
		if !strings.HasPrefix(svc.GetName(), req.GetParent()+"/services/") {
			continue
		}
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
	// The service as it is: for validate_only and etag (#581), and so a
	// delete never removes another project's service of the same ID (#1399).
	existing, err := s.service(ctx, req.GetName(), id)
	if err != nil {
		return nil, err
	}
	if err := checkEtag(req.GetEtag(), existing); err != nil {
		return nil, err
	}
	parent := strings.TrimSuffix(req.GetName(), "/services/"+id)
	if req.GetValidateOnly() {
		op := s.ops.Create(parent, req.GetName())
		_ = s.ops.Succeed(op.Name, &runpb.Service{Name: req.GetName()})
		return s.toProtoOperation(op.Name)
	}
	if err := s.kn.Delete(ctx, existing.Metadata.Name); err != nil {
		return nil, err
	}
	// Its K_* names (kenv.go); a failure leaves only an unread ConfigMap.
	_ = s.kn.Kube.Delete(ctx, "configmap", kEnvConfigMapName(existing.Metadata.Name), true)
	// Its volumes' mount pods and claims, once no other workload uses them.
	s.collectVolumes(ctx)
	op := s.ops.Create(parent, req.GetName())
	_ = s.ops.Succeed(op.Name, &runpb.Service{Name: req.GetName()})
	return s.toProtoOperation(op.Name)
}

// Operation returns a tracked operation, so a client can poll a create.
func (s *Server) Operation(name string) (*longrunningpb.Operation, error) {
	return s.toProtoOperation(name)
}

// checkEtag refuses a stale etag: the service was changed since the caller
// read it. ABORTED, as AIP-154 prescribes for an etag mismatch; not
// measured against Google.
func checkEtag(etag string, existing ksvc) error {
	if etag == "" {
		return nil
	}
	if strings.Trim(etag, `"`) != strings.Trim(existing.Metadata.ResourceVersion, `"`) {
		return apierror.Aborted("etag %s does not match the service's current etag %s; read it again and retry",
			etag, existing.Metadata.ResourceVersion)
	}
	return nil
}
