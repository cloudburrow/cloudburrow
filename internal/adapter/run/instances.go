package run

import (
	"context"
	"fmt"
	"math"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Instances (#1299): google.cloud.run.v2.Instances over an apps/v1
// Deployment of one replica (deployments.go).
//
// A standalone instance is one container group with no revisions and no
// autoscaling: a Deployment whose replicas is 1 while it runs and 0 once
// StopInstance stops it. Whether it is stopped is kept on its stored spec,
// so it reads back the same after a restart of the adapter.
//
// An instance has a URL, as Instance.urls documents ("All URLs serving
// traffic for this Instance"), and is reached the way a service is: through
// the Knative gateway, by a Knative networking Ingress (KIngress) whose host
// is <id>.instance.<namespace>.<domain>, over a ClusterIP Service on the
// pods' serving port (instanceroute.go). As with a service, only
// INGRESS_TRAFFIC_ALL is mapped, and invoker IAM is stored, not enforced.

const (
	kindInstance = "instance"
	// annStopped marks a stored instance StopInstance stopped.
	annStopped = "cloudburrow.dev/run-stopped"
)

// InstancesServer serves google.cloud.run.v2.Instances.
type InstancesServer struct {
	runpb.UnimplementedInstancesServer
	s *Server
}

// Instances returns the Instances service over the same adapter.
func (s *Server) Instances() *InstancesServer { return &InstancesServer{s: s} }

// Register adds the Instances service to a gRPC server or a JSON transcoder.
func (i *InstancesServer) Register(g grpc.ServiceRegistrar) { runpb.RegisterInstancesServer(g, i) }

func parseInstanceName(name string) (workload, string, error) {
	n, err := resource.Parse(name)
	if err != nil || n.Collection != "instances" || n.ParentCollection != "" {
		return workload{}, "", apierror.InvalidArgument("%q is not a Cloud Run instance name", name)
	}
	return workload{kind: kindInstance, id: n.ID, name: name}, n.LocationName(), nil
}

// checkInstance refuses what one Deployment pod does not render.
func checkInstance(in *runpb.Instance) error {
	var gaps []string
	add := func(set bool, field, why string) {
		if set {
			gaps = append(gaps, field+": "+why)
		}
	}
	add(in.GetBinaryAuthorization() != nil, "binaryAuthorization", "not enforced locally")
	ingress := in.GetIngress()
	add(ingress != runpb.IngressTraffic_INGRESS_TRAFFIC_UNSPECIFIED && ingress != runpb.IngressTraffic_INGRESS_TRAFFIC_ALL, "ingress",
		"only INGRESS_TRAFFIC_ALL is mapped; every instance is reachable from the host and the cluster")
	add(in.GetIapEnabled(), "iapEnabled", "no Identity-Aware Proxy exists locally")
	add(in.GpuZonalRedundancyDisabled != nil, "gpuZonalRedundancyDisabled", "there is one kind node and no GPU")
	add(in.GetEncryptionKeyRevocationAction() != runpb.EncryptionKeyRevocationAction_ENCRYPTION_KEY_REVOCATION_ACTION_UNSPECIFIED ||
		in.GetEncryptionKeyShutdownDuration() != nil, "encryptionKeyRevocationAction", "no encryption key is used")
	gaps = append(gaps, podTemplateGaps("instance", noExecEnv{in})...)
	gaps = append(gaps, storedSettingGaps("instance", noExecEnv{in})...)
	if len(gaps) > 0 {
		return apierror.Unimplemented("unsupported Cloud Run configuration: %s", strings.Join(gaps, "; "))
	}
	if len(in.GetContainers()) == 0 {
		return apierror.InvalidArgument("instance requires containers")
	}
	if err := checkVolumes(noExecEnv{in}); err != nil {
		return err
	}
	for field, m := range map[string]map[string]string{"labels": in.GetLabels(), "annotations": in.GetAnnotations()} {
		if err := validateMetadata(field, m); err != nil {
			return err
		}
	}
	return nil
}

// instanceSpec is what is stored: the fields a caller sets and the
// generation.
func instanceSpec(in *runpb.Instance, generation int64) *runpb.Instance {
	return &runpb.Instance{
		Name: in.GetName(), Description: in.GetDescription(), Labels: in.GetLabels(), Annotations: in.GetAnnotations(),
		Client: in.GetClient(), ClientVersion: in.GetClientVersion(), LaunchStage: in.GetLaunchStage(),
		ServiceAccount: in.GetServiceAccount(), Containers: in.GetContainers(), Volumes: in.GetVolumes(),
		Ingress: in.GetIngress(), InvokerIamDisabled: in.GetInvokerIamDisabled(),
		Generation: generation, UpdateTime: timestamppb.Now(),
	}
}

// writeInstance writes an instance's spec and its Deployment, running or
// stopped.
func (s *Server) writeInstance(ctx context.Context, w workload, spec *runpb.Instance, stopped bool, verb, resourceVersion string) error {
	stored := proto.Clone(spec).(*runpb.Instance)
	if stopped {
		if stored.Annotations == nil {
			stored.Annotations = map[string]string{}
		}
		stored.Annotations[annStopped] = "true"
	} else {
		delete(stored.Annotations, annStopped)
	}
	cm, err := w.configMap(stored, s.kn.namespace(), s.instance, resourceVersion, nil)
	if err != nil {
		return err
	}
	replicas := int32(1)
	if stopped {
		replicas = 0
	}
	containers, port := servingContainers(spec.GetContainers())
	dep, err := s.deployment(w, podSpec{containers: containers, volumes: spec.GetVolumes()}, replicas)
	if err != nil {
		return err
	}
	if err := s.writeWorkload(ctx, w, cm, dep, verb); err != nil {
		return err
	}
	return s.applyInstanceRoute(ctx, w, port)
}

// readInstance renders a stored instance with its output-only fields.
func (s *Server) readInstance(ctx context.Context, w workload, cm kconfigMap) (*runpb.Instance, bool, error) {
	var in runpb.Instance
	if err := decodeSpec(cm, &in); err != nil {
		return nil, false, err
	}
	stopped := in.Annotations[annStopped] == "true"
	delete(in.Annotations, annStopped)
	if len(in.Annotations) == 0 {
		in.Annotations = nil
	}
	d, found, err := s.loadDeployment(ctx, w)
	if err != nil {
		return nil, false, err
	}
	in.Uid = cm.Metadata.UID
	in.Etag = cm.Metadata.ResourceVersion
	in.CreateTime = timestamppb.New(cm.Metadata.CreationTimestamp)
	if in.UpdateTime == nil {
		in.UpdateTime = in.CreateTime
	}
	in.ObservedGeneration = in.Generation
	cond, reconciling := deploymentConditions(d, found, in.GetUpdateTime().AsTime())
	if stopped && cond.State == runpb.Condition_CONDITION_SUCCEEDED {
		cond.Message = "the instance is stopped"
	}
	if in.Ingress == runpb.IngressTraffic_INGRESS_TRAFFIC_UNSPECIFIED {
		in.Ingress = runpb.IngressTraffic_INGRESS_TRAFFIC_ALL
	}
	if _, port := servingContainers(in.GetContainers()); port != 0 {
		in.Urls = []string{"http://" + s.instanceHost(w)}
	}
	if !stopped && cond.State == runpb.Condition_CONDITION_SUCCEEDED {
		if ready, msg := s.instanceRouteReady(ctx, w); !ready {
			cond.State, cond.Message, reconciling = runpb.Condition_CONDITION_RECONCILING, msg, true
		}
	}
	in.TerminalCondition, in.Reconciling = cond, reconciling
	return &in, stopped, nil
}

// CreateInstance stores an instance and starts it.
func (i *InstancesServer) CreateInstance(ctx context.Context, req *runpb.CreateInstanceRequest) (*longrunningpb.Operation, error) {
	s := i.s
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	id := req.GetInstanceId()
	if err := validateJobID(id); err != nil {
		return nil, apierror.InvalidArgument("%s", strings.ReplaceAll(apierror.From(err).Message, "job", "instance"))
	}
	if req.GetInstance() == nil {
		return nil, apierror.InvalidArgument("instance is required")
	}
	in := proto.Clone(req.GetInstance()).(*runpb.Instance)
	in.Name = req.GetParent() + "/instances/" + id
	if err := checkInstance(in); err != nil {
		return nil, err
	}
	w := workload{kind: kindInstance, id: id, name: in.Name}
	spec := instanceSpec(in, 1)
	// Render once before anything is written, so an image or secret the pod
	// could not use is refused on create.
	containers, _ := servingContainers(spec.GetContainers())
	if _, err := s.deployment(w, podSpec{containers: containers, volumes: spec.GetVolumes()}, 1); err != nil {
		return nil, err
	}
	if err := s.ensureAbsent(ctx, w); err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return s.doneOperation(req.GetParent(), in.Name, spec)
	}
	if err := s.writeInstance(ctx, w, spec, false, "create", ""); err != nil {
		return nil, err
	}
	return s.instanceOperation(ctx, w, req.GetParent())
}

func (s *Server) instanceOperation(ctx context.Context, w workload, parent string) (*longrunningpb.Operation, error) {
	return s.workloadOperation(ctx, parent, w, func(ctx context.Context, cm kconfigMap) (proto.Message, *runpb.Condition, error) {
		in, _, err := s.readInstance(ctx, w, cm)
		if err != nil {
			return nil, nil, err
		}
		return in, in.GetTerminalCondition(), nil
	})
}

// GetInstance reads an instance.
func (i *InstancesServer) GetInstance(ctx context.Context, req *runpb.GetInstanceRequest) (*runpb.Instance, error) {
	w, _, err := parseInstanceName(req.GetName())
	if err != nil {
		return nil, err
	}
	cm, err := i.s.loadSpec(ctx, w)
	if err != nil {
		return nil, err
	}
	out, _, err := i.s.readInstance(ctx, w, cm)
	return out, err
}

// ListInstances lists the instances under a parent, newest first.
func (i *InstancesServer) ListInstances(ctx context.Context, req *runpb.ListInstancesRequest) (*runpb.ListInstancesResponse, error) {
	s := i.s
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if req.GetShowDeleted() {
		return nil, apierror.Unimplemented("showDeleted: a deleted instance is not kept")
	}
	cms, err := s.specsOfKind(ctx, kindInstance)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*runpb.Instance{}
	keys := []string{}
	prefix := req.GetParent() + "/instances/"
	for _, cm := range cms {
		name := cm.Metadata.Annotations[annRunName]
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		w := workload{kind: kindInstance, id: strings.TrimPrefix(name, prefix), name: name}
		in, _, err := s.readInstance(ctx, w, cm)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%019d/%s", math.MaxInt64-cm.Metadata.CreationTimestamp.UnixNano(), name)
		byKey[key] = in
		keys = append(keys, key)
	}
	page, next, err := paging.Page("run-instances:"+req.GetParent(), keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	resp := &runpb.ListInstancesResponse{NextPageToken: next}
	for _, k := range page {
		resp.Instances = append(resp.Instances, byKey[k])
	}
	return resp, nil
}

// DeleteInstance removes an instance and its pod.
func (i *InstancesServer) DeleteInstance(ctx context.Context, req *runpb.DeleteInstanceRequest) (*longrunningpb.Operation, error) {
	s := i.s
	w, parent, err := parseInstanceName(req.GetName())
	if err != nil {
		return nil, err
	}
	cm, err := s.loadSpec(ctx, w)
	if err != nil {
		return nil, err
	}
	if err := checkResourceEtag(req.GetEtag(), cm.Metadata.ResourceVersion, "instance"); err != nil {
		return nil, err
	}
	out, _, err := s.readInstance(ctx, w, cm)
	if err != nil {
		return nil, err
	}
	if !req.GetValidateOnly() {
		if err := s.deleteWorkload(ctx, w); err != nil {
			return nil, err
		}
		out.DeleteTime = timestamppb.Now()
	}
	return s.doneOperation(parent, req.GetName(), out)
}

// setStopped starts or stops an instance: its Deployment scaled to one or
// to zero.
func (i *InstancesServer) setStopped(ctx context.Context, name, etag string, validateOnly, stop bool) (*longrunningpb.Operation, error) {
	s := i.s
	w, parent, err := parseInstanceName(name)
	if err != nil {
		return nil, err
	}
	cm, err := s.loadSpec(ctx, w)
	if err != nil {
		return nil, err
	}
	if err := checkResourceEtag(etag, cm.Metadata.ResourceVersion, "instance"); err != nil {
		return nil, err
	}
	in, _, err := s.readInstance(ctx, w, cm)
	if err != nil {
		return nil, err
	}
	if validateOnly {
		return s.doneOperation(parent, name, in)
	}
	spec := instanceSpec(in, in.GetGeneration())
	if err := s.writeInstance(ctx, w, spec, stop, "replace", cm.Metadata.ResourceVersion); err != nil {
		return nil, err
	}
	return s.instanceOperation(ctx, w, parent)
}

// StopInstance stops an instance's container; it can be started again.
func (i *InstancesServer) StopInstance(ctx context.Context, req *runpb.StopInstanceRequest) (*longrunningpb.Operation, error) {
	return i.setStopped(ctx, req.GetName(), req.GetEtag(), req.GetValidateOnly(), true)
}

// StartInstance starts a stopped instance.
func (i *InstancesServer) StartInstance(ctx context.Context, req *runpb.StartInstanceRequest) (*longrunningpb.Operation, error) {
	return i.setStopped(ctx, req.GetName(), req.GetEtag(), req.GetValidateOnly(), false)
}
