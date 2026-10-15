package run

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// WorkerPools (#1300): google.cloud.run.v2.WorkerPools over an apps/v1
// Deployment (deployments.go).
//
// A worker pool is a service with no ingress and a fixed instance count:
// scaling.manualInstanceCount is the Deployment's replicas. Each change to
// its template is a new revision, named {pool}-{generation}-{suffix} as
// Cloud Run names one, and each revision is the ReplicaSet the Deployment
// creates for that template, so a retired revision is a ReplicaSet scaled to
// zero. The whole pool always runs its latest revision: instanceSplits
// other than 100% to the latest are refused.

const (
	kindWorkerPool = "worker-pool"
	revisionGenKey = "revision-generation"
)

// defaultWorkerPoolInstances is the instance count a pool is given when
// scaling.manualInstanceCount is not set.
const defaultWorkerPoolInstances = 1

// WorkerPoolsServer serves google.cloud.run.v2.WorkerPools. Its IAM methods
// store a policy without enforcing it (iam.go), as for services and jobs.
type WorkerPoolsServer struct {
	runpb.UnimplementedWorkerPoolsServer
	s *Server
}

// WorkerPools returns the WorkerPools service over the same adapter.
func (s *Server) WorkerPools() *WorkerPoolsServer { return &WorkerPoolsServer{s: s} }

// Register adds the WorkerPools service to a gRPC server or a JSON transcoder.
func (p *WorkerPoolsServer) Register(g grpc.ServiceRegistrar) { runpb.RegisterWorkerPoolsServer(g, p) }

// parseWorkerPoolName splits projects/{p}/locations/{l}/workerPools/{id}.
func parseWorkerPoolName(name string) (workload, error) {
	n, err := resource.Parse(name)
	if err != nil || n.Collection != "workerPools" || n.ParentCollection != "" {
		return workload{}, apierror.InvalidArgument("%q is not a Cloud Run worker pool name", name)
	}
	return workload{kind: kindWorkerPool, id: n.ID, name: name}, nil
}

// unsupportedWorkerPool refuses what a Deployment does not render.
func unsupportedWorkerPool(wp *runpb.WorkerPool) error {
	var gaps []string
	add := func(set bool, field, why string) {
		if set {
			gaps = append(gaps, field+": "+why)
		}
	}
	add(wp.GetBinaryAuthorization() != nil, "binaryAuthorization", "not enforced locally")
	add(len(wp.GetCustomAudiences()) > 0, "customAudiences", "a worker pool receives no requests")
	for _, sp := range wp.GetInstanceSplits() {
		latest := sp.GetType() == runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_LATEST
		add(sp.GetPercent() != 100 || !latest && sp.GetRevision() != wp.GetTemplate().GetRevision(),
			"instanceSplits", "every instance runs the latest revision; a split across revisions is not mapped")
	}
	t := wp.GetTemplate()
	gaps = append(gaps, podTemplateGaps("template", noExecEnv{t})...)
	add(t.GetServiceMesh() != nil, "template.serviceMesh", "no service mesh exists locally")
	add(t.GpuZonalRedundancyDisabled != nil, "template.gpuZonalRedundancyDisabled", "there is one kind node and no GPU")
	add(t.GetEncryptionKeyRevocationAction() != runpb.EncryptionKeyRevocationAction_ENCRYPTION_KEY_REVOCATION_ACTION_UNSPECIFIED ||
		t.GetEncryptionKeyShutdownDuration() != nil, "template.encryptionKeyRevocationAction", "no encryption key is used")
	for _, c := range t.GetContainers() {
		add(len(c.GetPorts()) > 0, "container.ports", "a worker pool has no ingress")
	}
	if len(gaps) > 0 {
		return apierror.Unimplemented("unsupported Cloud Run configuration: %s", strings.Join(gaps, "; "))
	}
	return nil
}

// checkWorkerPool validates a worker pool as given.
func checkWorkerPool(wp *runpb.WorkerPool) error {
	if err := unsupportedWorkerPool(wp); err != nil {
		return err
	}
	t := wp.GetTemplate()
	if len(t.GetContainers()) == 0 {
		return apierror.InvalidArgument("worker pool requires template.containers")
	}
	if err := checkVolumes(noExecEnv{t}); err != nil {
		return err
	}
	for field, m := range map[string]map[string]string{"labels": wp.GetLabels(), "annotations": wp.GetAnnotations(),
		"template.labels": t.GetLabels(), "template.annotations": t.GetAnnotations()} {
		if err := validateMetadata(field, m); err != nil {
			return err
		}
	}
	if c := wp.GetScaling().GetManualInstanceCount(); c < 0 {
		return apierror.InvalidArgument("scaling.manualInstanceCount must not be negative")
	}
	return nil
}

// revisionSuffix is three random characters, as Cloud Run ends a revision
// name.
func revisionSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 3)
	for i := range b {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(b)
}

// templateKey is a template without its revision name, to tell whether an
// update changed what runs.
func templateKey(t *runpb.WorkerPoolRevisionTemplate) string {
	c := proto.Clone(t).(*runpb.WorkerPoolRevisionTemplate)
	c.Revision = ""
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	return string(raw)
}

// workerPoolSpec is what is stored: the fields a caller sets, the
// generation, the default instance count and the latest revision.
func workerPoolSpec(wp *runpb.WorkerPool, generation int64, revision string) *runpb.WorkerPool {
	scaling := &runpb.WorkerPoolScaling{}
	if wp.GetScaling() != nil {
		scaling = proto.Clone(wp.GetScaling()).(*runpb.WorkerPoolScaling)
	}
	if scaling.ManualInstanceCount == nil {
		n := int32(defaultWorkerPoolInstances)
		scaling.ManualInstanceCount = &n
	}
	return &runpb.WorkerPool{
		Name: wp.GetName(), Description: wp.GetDescription(), Labels: wp.GetLabels(), Annotations: wp.GetAnnotations(),
		Client: wp.GetClient(), ClientVersion: wp.GetClientVersion(), LaunchStage: wp.GetLaunchStage(),
		Template: proto.Clone(wp.GetTemplate()).(*runpb.WorkerPoolRevisionTemplate), Scaling: scaling,
		InstanceSplits: wp.GetInstanceSplits(), Generation: generation, UpdateTime: timestamppb.Now(),
		LatestCreatedRevision: revision,
	}
}

// renderWorkerPool renders a pool's ConfigMap and Deployment.
// revGen is the generation the latest revision was created at, which a
// rescale leaves alone so the revision's ReplicaSet is kept.
func (s *Server) renderWorkerPool(w workload, spec *runpb.WorkerPool, revGen int64, resourceVersion string) (string, string, error) {
	cm, err := w.configMap(spec, s.kn.namespace(), s.instance, resourceVersion,
		map[string]string{revisionGenKey: strconv.FormatInt(revGen, 10)})
	if err != nil {
		return "", "", err
	}
	revID := spec.GetLatestCreatedRevision()[strings.LastIndex(spec.GetLatestCreatedRevision(), "/")+1:]
	t := spec.GetTemplate()
	dep, err := s.deployment(w, podSpec{containers: t.GetContainers(), volumes: t.GetVolumes(), revision: revID,
		revisionName: spec.GetLatestCreatedRevision(), revisionSpec: t, generation: revGen},
		spec.GetScaling().GetManualInstanceCount())
	return cm, dep, err
}

// workerPoolOut renders a stored pool with its output-only fields.
func workerPoolOut(cm kconfigMap, d kdeployment, found bool) (*runpb.WorkerPool, error) {
	var wp runpb.WorkerPool
	if err := decodeSpec(cm, &wp); err != nil {
		return nil, err
	}
	wp.Uid = cm.Metadata.UID
	wp.Etag = cm.Metadata.ResourceVersion
	wp.CreateTime = timestamppb.New(cm.Metadata.CreationTimestamp)
	if wp.UpdateTime == nil {
		wp.UpdateTime = wp.CreateTime
	}
	wp.ObservedGeneration = wp.Generation
	cond, reconciling := deploymentConditions(d, found, wp.GetUpdateTime().AsTime())
	wp.TerminalCondition = cond
	wp.Reconciling = reconciling
	if cond.State == runpb.Condition_CONDITION_SUCCEEDED {
		wp.LatestReadyRevision = wp.LatestCreatedRevision
	}
	wp.InstanceSplitStatuses = []*runpb.InstanceSplitStatus{{
		Type: runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_LATEST, Percent: 100}}
	if len(wp.InstanceSplits) == 0 {
		wp.InstanceSplits = []*runpb.InstanceSplit{{
			Type: runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_LATEST, Percent: 100}}
	}
	return &wp, nil
}

func (s *Server) readWorkerPool(ctx context.Context, w workload, cm kconfigMap) (*runpb.WorkerPool, error) {
	d, found, err := s.loadDeployment(ctx, w)
	if err != nil {
		return nil, err
	}
	return workerPoolOut(cm, d, found)
}

// CreateWorkerPool stores a pool and starts its instances. The operation
// completes once the pool is written; GetWorkerPool reports the instances
// coming up as reconciling.
func (p *WorkerPoolsServer) CreateWorkerPool(ctx context.Context, req *runpb.CreateWorkerPoolRequest) (*longrunningpb.Operation, error) {
	s := p.s
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	id := req.GetWorkerPoolId()
	if err := validateJobID(id); err != nil {
		return nil, apierror.InvalidArgument("%s", strings.ReplaceAll(apierror.From(err).Message, "job", "worker pool"))
	}
	if req.GetWorkerPool() == nil {
		return nil, apierror.InvalidArgument("workerPool is required")
	}
	wp := proto.Clone(req.GetWorkerPool()).(*runpb.WorkerPool)
	wp.Name = req.GetParent() + "/workerPools/" + id
	w := workload{kind: kindWorkerPool, id: id, name: wp.Name}
	if wp.Template == nil {
		wp.Template = &runpb.WorkerPoolRevisionTemplate{}
	}
	if err := checkWorkerPool(wp); err != nil {
		return nil, err
	}
	rev := wp.GetTemplate().GetRevision()
	if rev == "" {
		rev = fmt.Sprintf("%s-%05d-%s", id, 1, revisionSuffix())
	}
	spec := workerPoolSpec(wp, 1, wp.Name+"/revisions/"+rev)
	cmManifest, depManifest, err := s.renderWorkerPool(w, spec, 1, "")
	if err != nil {
		return nil, err
	}
	if err := s.ensureAbsent(ctx, w); err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return s.doneOperation(req.GetParent(), wp.Name, spec)
	}
	if err := s.writeWorkload(ctx, w, cmManifest, depManifest, "create"); err != nil {
		return nil, err
	}
	cm, err := s.loadSpec(ctx, w)
	if err != nil {
		return nil, apierror.Internal(err, "read back worker pool %s", wp.Name)
	}
	out, err := s.readWorkerPool(ctx, w, cm)
	if err != nil {
		return nil, err
	}
	return s.doneOperation(req.GetParent(), wp.Name, out)
}

// GetWorkerPool reads a pool.
func (p *WorkerPoolsServer) GetWorkerPool(ctx context.Context, req *runpb.GetWorkerPoolRequest) (*runpb.WorkerPool, error) {
	w, err := parseWorkerPoolName(req.GetName())
	if err != nil {
		return nil, err
	}
	cm, err := p.s.loadSpec(ctx, w)
	if err != nil {
		return nil, err
	}
	return p.s.readWorkerPool(ctx, w, cm)
}

// ListWorkerPools lists the pools under a parent, newest first.
func (p *WorkerPoolsServer) ListWorkerPools(ctx context.Context, req *runpb.ListWorkerPoolsRequest) (*runpb.ListWorkerPoolsResponse, error) {
	s := p.s
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	cms, err := s.specsOfKind(ctx, kindWorkerPool)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*runpb.WorkerPool{}
	keys := []string{}
	prefix := req.GetParent() + "/workerPools/"
	for _, cm := range cms {
		name := cm.Metadata.Annotations[annRunName]
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		w := workload{kind: kindWorkerPool, id: strings.TrimPrefix(name, prefix), name: name}
		wp, err := s.readWorkerPool(ctx, w, cm)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%019d/%s", math.MaxInt64-cm.Metadata.CreationTimestamp.UnixNano(), name)
		byKey[key] = wp
		keys = append(keys, key)
	}
	page, next, err := paging.Page("run-worker-pools:"+req.GetParent(), keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	resp := &runpb.ListWorkerPoolsResponse{NextPageToken: next}
	for _, k := range page {
		resp.WorkerPools = append(resp.WorkerPools, byKey[k])
	}
	return resp, nil
}

// UpdateWorkerPool replaces a pool's configuration. A changed template is
// a new revision; a changed instance count alone rescales the current one.
// update_mask is not read: the pool is replaced whole, as UpdateJob
// replaces a job.
func (p *WorkerPoolsServer) UpdateWorkerPool(ctx context.Context, req *runpb.UpdateWorkerPoolRequest) (*longrunningpb.Operation, error) {
	s := p.s
	if req.GetWorkerPool() == nil {
		return nil, apierror.InvalidArgument("workerPool is required")
	}
	name := req.GetWorkerPool().GetName()
	w, err := parseWorkerPoolName(name)
	if err != nil {
		return nil, err
	}
	parent := name[:strings.LastIndex(name, "/workerPools/")]
	if len(req.GetUpdateMask().GetPaths()) > 0 {
		return nil, apierror.Unimplemented("updateMask is not mapped for a worker pool; send the whole worker pool")
	}
	cm, err := s.loadSpec(ctx, w)
	if err != nil {
		if apierror.From(err).Code == codes.NotFound && req.GetAllowMissing() {
			return p.CreateWorkerPool(ctx, &runpb.CreateWorkerPoolRequest{Parent: parent, WorkerPoolId: w.id,
				WorkerPool: req.GetWorkerPool(), ValidateOnly: req.GetValidateOnly()})
		}
		return nil, err
	}
	if err := checkResourceEtag(req.GetWorkerPool().GetEtag(), cm.Metadata.ResourceVersion, "worker pool"); err != nil {
		return nil, err
	}
	var prev runpb.WorkerPool
	if err := decodeSpec(cm, &prev); err != nil {
		return nil, err
	}
	wp := proto.Clone(req.GetWorkerPool()).(*runpb.WorkerPool)
	if wp.Template == nil {
		wp.Template = &runpb.WorkerPoolRevisionTemplate{}
	}
	if err := checkWorkerPool(wp); err != nil {
		return nil, err
	}
	gen := prev.GetGeneration() + 1
	revision := prev.GetLatestCreatedRevision()
	prevRev := revision[strings.LastIndex(revision, "/")+1:]
	newRev := wp.GetTemplate().GetRevision()
	switch changed := templateKey(wp.GetTemplate()) != templateKey(prev.GetTemplate()); {
	case newRev != "" && newRev != prevRev:
		revision = name + "/revisions/" + newRev
	case changed && newRev != "":
		return nil, apierror.AlreadyExists("revision %s already exists; give the changed template a new revision name", newRev)
	case changed:
		revision = fmt.Sprintf("%s/revisions/%s-%05d-%s", name, w.id, gen, revisionSuffix())
	}
	revGen := gen
	if revision == prev.GetLatestCreatedRevision() {
		if n, err := strconv.ParseInt(cm.Data[revisionGenKey], 10, 64); err == nil {
			revGen = n
		}
	}
	spec := workerPoolSpec(wp, gen, revision)
	cmManifest, depManifest, err := s.renderWorkerPool(w, spec, revGen, cm.Metadata.ResourceVersion)
	if err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return s.doneOperation(parent, name, spec)
	}
	if err := s.writeWorkload(ctx, w, cmManifest, depManifest, "replace"); err != nil {
		return nil, err
	}
	updated, err := s.loadSpec(ctx, w)
	if err != nil {
		return nil, apierror.Internal(err, "read back worker pool %s", name)
	}
	out, err := s.readWorkerPool(ctx, w, updated)
	if err != nil {
		return nil, err
	}
	return s.doneOperation(parent, name, out)
}

// DeleteWorkerPool removes a pool, its revisions and its instances.
func (p *WorkerPoolsServer) DeleteWorkerPool(ctx context.Context, req *runpb.DeleteWorkerPoolRequest) (*longrunningpb.Operation, error) {
	s := p.s
	w, err := parseWorkerPoolName(req.GetName())
	if err != nil {
		return nil, err
	}
	parent := req.GetName()[:strings.LastIndex(req.GetName(), "/workerPools/")]
	cm, err := s.loadSpec(ctx, w)
	if err != nil {
		return nil, err
	}
	if err := checkResourceEtag(req.GetEtag(), cm.Metadata.ResourceVersion, "worker pool"); err != nil {
		return nil, err
	}
	out, err := s.readWorkerPool(ctx, w, cm)
	if err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return s.doneOperation(parent, req.GetName(), out)
	}
	if err := s.deleteWorkload(ctx, w); err != nil {
		return nil, err
	}
	return s.doneOperation(parent, req.GetName(), out)
}

// workerPoolOwner finds the ConfigMap behind a worker pool, for its policy.
func (s *Server) workerPoolOwner(ctx context.Context, name string) (iamOwner, error) {
	w, err := parseWorkerPoolName(name)
	if err != nil {
		return iamOwner{}, err
	}
	cm, err := s.loadSpec(ctx, w)
	if err != nil {
		return iamOwner{}, err
	}
	return iamOwner{kind: "workerpool", id: w.id, name: name, apiVersion: "v1", objKind: "ConfigMap",
		objName: cm.Metadata.Name, uid: cm.Metadata.UID}, nil
}

// GetIamPolicy is a pool's stored policy; nothing enforces it.
func (p *WorkerPoolsServer) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	o, err := p.s.workerPoolOwner(ctx, req.GetResource())
	if err != nil {
		return nil, err
	}
	return p.s.getPolicy(ctx, o, req)
}

// SetIamPolicy stores a pool's policy with a new etag; a stale etag is
// ABORTED.
func (p *WorkerPoolsServer) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	o, err := p.s.workerPoolOwner(ctx, req.GetResource())
	if err != nil {
		return nil, err
	}
	return p.s.setPolicy(ctx, o, req)
}

// TestIamPermissions returns every requested permission on a pool.
func (p *WorkerPoolsServer) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	_, err := p.s.workerPoolOwner(ctx, req.GetResource())
	if err != nil && apierror.From(err).Code == codes.InvalidArgument {
		return nil, err
	}
	return testPermissions(err, req)
}

// Worker pool revisions, served by the Revisions service under
// .../workerPools/{pool}/revisions/{rev}: one ReplicaSet each.

// isWorkerPoolPath reports whether a revision name or parent is a pool's.
func isWorkerPoolPath(name string) bool { return strings.Contains(name, "/workerPools/") }

func (s *Server) workerPoolRevisions(ctx context.Context, w workload) ([]kreplicaSet, error) {
	return listObjects[kreplicaSet](ctx, s.kn, resReplicaSet, w.selector())
}

// fromReplicaSet maps a pool's ReplicaSet onto run.v2.Revision.
func fromReplicaSet(rs kreplicaSet, latest string) (*runpb.Revision, error) {
	ann := rs.Spec.Template.Metadata.Annotations
	var t runpb.WorkerPoolRevisionTemplate
	if err := decodeSpec(kconfigMap{Metadata: rs.Metadata, Data: map[string]string{specDataKey: ann[annRevisionSpec]}}, &t); err != nil {
		return nil, err
	}
	gen, _ := strconv.ParseInt(ann[annRevisionGen], 10, 64)
	name := ann[annRunName]
	rev := &runpb.Revision{
		Name: name, Uid: rs.Metadata.UID, Generation: gen, Labels: t.GetLabels(), Annotations: t.GetAnnotations(),
		CreateTime: timestamppb.New(rs.Metadata.CreationTimestamp), UpdateTime: timestamppb.New(rs.Metadata.CreationTimestamp),
		VpcAccess: t.GetVpcAccess(), ServiceAccount: t.GetServiceAccount(), Containers: t.GetContainers(),
		Volumes: t.GetVolumes(), NodeSelector: t.GetNodeSelector(), ObservedGeneration: gen,
		Etag: rs.Metadata.ResourceVersion,
	}
	want := int32(0)
	if rs.Spec.Replicas != nil {
		want = *rs.Spec.Replicas
	}
	ready := &runpb.Condition{Type: "Ready", LastTransitionTime: rev.CreateTime}
	active := &runpb.Condition{Type: "Active", LastTransitionTime: rev.CreateTime}
	switch {
	case name != latest:
		ready.State = runpb.Condition_CONDITION_SUCCEEDED
		active.State = runpb.Condition_CONDITION_FAILED
		active.Reasons = &runpb.Condition_RevisionReason_{RevisionReason: runpb.Condition_RETIRED}
		active.Message = "Revision retired."
	case rs.Status.ReadyReplicas >= want:
		ready.State = runpb.Condition_CONDITION_SUCCEEDED
		active.State = runpb.Condition_CONDITION_SUCCEEDED
	default:
		ready.State = runpb.Condition_CONDITION_RECONCILING
		active.State = runpb.Condition_CONDITION_RECONCILING
		rev.Reconciling = true
	}
	rev.Conditions = []*runpb.Condition{ready, active}
	return rev, nil
}

func (s *Server) loadWorkerPoolForRevisions(ctx context.Context, poolName string) (workload, string, error) {
	w, err := parseWorkerPoolName(poolName)
	if err != nil {
		return workload{}, "", err
	}
	cm, err := s.loadSpec(ctx, w)
	if err != nil {
		return workload{}, "", err
	}
	var wp runpb.WorkerPool
	if err := decodeSpec(cm, &wp); err != nil {
		return workload{}, "", err
	}
	return w, wp.GetLatestCreatedRevision(), nil
}

func (s *Server) workerPoolRevision(ctx context.Context, name string) (*runpb.Revision, workload, kreplicaSet, string, error) {
	i := strings.LastIndex(name, "/revisions/")
	if i < 0 {
		return nil, workload{}, kreplicaSet{}, "", apierror.InvalidArgument("%q is not a Cloud Run revision name", name)
	}
	w, latest, err := s.loadWorkerPoolForRevisions(ctx, name[:i])
	if err != nil {
		return nil, workload{}, kreplicaSet{}, "", err
	}
	items, err := s.workerPoolRevisions(ctx, w)
	if err != nil {
		return nil, workload{}, kreplicaSet{}, "", err
	}
	for _, rs := range items {
		if rs.Spec.Template.Metadata.Annotations[annRunName] == name {
			rev, err := fromReplicaSet(rs, latest)
			return rev, w, rs, latest, err
		}
	}
	return nil, workload{}, kreplicaSet{}, "", apierror.NotFound("revision %s not found", name)
}

func (s *Server) listWorkerPoolRevisions(ctx context.Context, req *runpb.ListRevisionsRequest) (*runpb.ListRevisionsResponse, error) {
	w, latest, err := s.loadWorkerPoolForRevisions(ctx, req.GetParent())
	if err != nil {
		return nil, err
	}
	items, err := s.workerPoolRevisions(ctx, w)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*runpb.Revision{}
	keys := []string{}
	for _, rs := range items {
		rev, err := fromReplicaSet(rs, latest)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%019d/%s", math.MaxInt64-rev.GetGeneration(), rev.GetName())
		byKey[key] = rev
		keys = append(keys, key)
	}
	page, next, err := paging.Page("run-revisions:"+req.GetParent(), keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	resp := &runpb.ListRevisionsResponse{NextPageToken: next}
	for _, k := range page {
		resp.Revisions = append(resp.Revisions, byKey[k])
	}
	return resp, nil
}

// deleteWorkerPoolRevision removes a retired revision; the one the pool
// runs is refused, as a serving service revision is.
func (s *Server) deleteWorkerPoolRevision(ctx context.Context, req *runpb.DeleteRevisionRequest) (*longrunningpb.Operation, error) {
	rev, _, rs, latest, err := s.workerPoolRevision(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	pool := req.GetName()[:strings.LastIndex(req.GetName(), "/revisions/")]
	if rev.GetName() == latest {
		return nil, apierror.FailedPrecondition(
			"revision %s is the one worker pool %s runs and cannot be deleted; deploy another revision first", rev.GetName(), pool)
	}
	if !req.GetValidateOnly() {
		if err := s.kn.deleteObject(ctx, resReplicaSet, rs.Metadata.Name); err != nil {
			return nil, err
		}
	}
	op := s.ops.Create(pool, req.GetName())
	_ = s.ops.Succeed(op.Name, rev)
	return s.toProtoOperation(op.Name)
}
