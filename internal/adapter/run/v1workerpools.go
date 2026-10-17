package run

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/genproto/googleapis/api"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Cloud Run v1 worker pools, namespaces.workerpools (#1546): list, get,
// create, replaceWorkerPool and delete, at
// /apis/run.googleapis.com/v1/namespaces/{project}/workerpools[/{name}],
// mapped onto the v2 WorkerPools server, as v1 services are onto v2's
// Services: the revision template goes through the service mapping, the
// instance splits map one to one, and the pool's manual instance count is
// its run.googleapis.com/manualInstanceCount annotation. The pools' v1 IAM
// methods were served already (#1533, v1global.go).

const v1ManualInstanceCount = "run.googleapis.com/manualInstanceCount"

func (s *Server) v1WorkerPoolName(r *http.Request) (string, error) {
	id, err := resourceName(r)
	if err != nil {
		return "", err
	}
	parent, err := v1JobParent(scopeOf(r), "")
	if err != nil {
		return "", err
	}
	return parent + "/workerPools/" + id, nil
}

func (s *Server) v1ListWorkerPools(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	sel, err := labelSelector(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	locs, err := s.v1KindLocations(r.Context(), kindWorkerPool, sc)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	byName := map[string]*runv1.WorkerPool{}
	var names []string
	for _, loc := range locs {
		resp, err := s.WorkerPools().ListWorkerPools(r.Context(), &runpb.ListWorkerPoolsRequest{Parent: "projects/" + sc.project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			v1Write(w, err, nil)
			return
		}
		for _, wp := range resp.GetWorkerPools() {
			if v := toV1WorkerPool(wp); selected(v.Metadata, sel) {
				byName[wp.GetName()] = v
				names = append(names, wp.GetName())
			}
		}
	}
	page, next, err := v1Page(r, "run-v1-workerpools:projects/"+sc.project+"/locations/"+sc.location, names)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	out := &runv1.ListWorkerPoolsResponse{ApiVersion: "run.googleapis.com/v1", Kind: "WorkerPoolList",
		Metadata: &runv1.ListMeta{Continue: next}, Items: []*runv1.WorkerPool{}}
	for _, n := range page {
		out.Items = append(out.Items, byName[n])
	}
	v1Write(w, nil, out)
}

func (s *Server) v1GetWorkerPool(w http.ResponseWriter, r *http.Request) {
	name, err := s.v1WorkerPoolName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	wp, err := s.WorkerPools().GetWorkerPool(r.Context(), &runpb.GetWorkerPoolRequest{Name: name})
	v1Write(w, err, toV1WorkerPool(wp))
}

func v1DecodeWorkerPool(r *http.Request, sc v1Scope) (*runv1.WorkerPool, error) {
	var in runv1.WorkerPool
	if err := v1DecodeDroppingSandboxes(r, &in, "spec", "template", "spec"); err != nil {
		return nil, err
	}
	if in.Metadata == nil || in.Metadata.Name == "" {
		return nil, apierror.InvalidArgument("metadata.name is required")
	}
	if ns := in.Metadata.Namespace; ns != "" && ns != sc.project {
		return nil, apierror.InvalidArgument("metadata.namespace %q does not match the request's namespace %q", ns, sc.project)
	}
	return &in, nil
}

func (s *Server) v1CreateWorkerPool(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	in, err := v1DecodeWorkerPool(r, sc)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	parent, err := v1JobParent(sc, in.Metadata.Labels[v1LocationLabel])
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	wp, err := fromV1WorkerPool(in)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	validateOnly := r.URL.Query().Get("dryRun") == "all"
	if _, err := s.WorkerPools().CreateWorkerPool(r.Context(), &runpb.CreateWorkerPoolRequest{Parent: parent, WorkerPoolId: in.Metadata.Name,
		WorkerPool: wp, ValidateOnly: validateOnly}); err != nil {
		v1Write(w, err, nil)
		return
	}
	name := parent + "/workerPools/" + in.Metadata.Name
	if validateOnly {
		wp.Name = name
		v1Write(w, nil, toV1WorkerPool(wp))
		return
	}
	got, err := s.WorkerPools().GetWorkerPool(r.Context(), &runpb.GetWorkerPoolRequest{Name: name})
	v1Write(w, err, toV1WorkerPool(got))
}

// v1ReplaceWorkerPool is namespaces.workerpools.replaceWorkerPool: v2's
// UpdateWorkerPool with the whole pool.
func (s *Server) v1ReplaceWorkerPool(w http.ResponseWriter, r *http.Request) {
	name, err := s.v1WorkerPoolName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	in, err := v1DecodeWorkerPool(r, scopeOf(r))
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	if !strings.HasSuffix(name, "/workerPools/"+in.Metadata.Name) {
		v1Write(w, apierror.InvalidArgument("metadata.name %q does not match the request's worker pool", in.Metadata.Name), nil)
		return
	}
	if l := in.Metadata.Labels[v1LocationLabel]; l != "" && l != locationOf(name) {
		v1Write(w, apierror.InvalidArgument("label %s=%s does not match the request's region %s", v1LocationLabel, l, locationOf(name)), nil)
		return
	}
	wp, err := fromV1WorkerPool(in)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	wp.Name, wp.Etag = name, in.Metadata.ResourceVersion
	validateOnly := r.URL.Query().Get("dryRun") == "all"
	if _, err := s.WorkerPools().UpdateWorkerPool(r.Context(), &runpb.UpdateWorkerPoolRequest{WorkerPool: wp, ValidateOnly: validateOnly}); err != nil {
		v1Write(w, err, nil)
		return
	}
	if validateOnly {
		v1Write(w, nil, toV1WorkerPool(wp))
		return
	}
	got, err := s.WorkerPools().GetWorkerPool(r.Context(), &runpb.GetWorkerPoolRequest{Name: name})
	v1Write(w, err, toV1WorkerPool(got))
}

func (s *Server) v1DeleteWorkerPool(w http.ResponseWriter, r *http.Request) {
	name, err := s.v1WorkerPoolName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	wp, err := s.WorkerPools().GetWorkerPool(r.Context(), &runpb.GetWorkerPoolRequest{Name: name})
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	if _, err := s.WorkerPools().DeleteWorkerPool(r.Context(), &runpb.DeleteWorkerPoolRequest{Name: name,
		ValidateOnly: r.URL.Query().Get("dryRun") == "all"}); err != nil {
		v1Write(w, err, nil)
		return
	}
	_, id, _ := strings.Cut(name, "/workerPools/")
	v1Write(w, nil, &runv1.Status{Status: "Success",
		Details: &runv1.StatusDetails{Name: id, Kind: "workerpools", Group: "run.googleapis.com", Uid: wp.GetUid()}})
}

// fromV1WorkerPool converts a v1 worker pool to v2's shape.
func fromV1WorkerPool(in *runv1.WorkerPool) (*runpb.WorkerPool, error) {
	if in.Spec == nil || in.Spec.Template == nil || in.Spec.Template.Spec == nil {
		return nil, apierror.InvalidArgument("spec.template.spec.containers is required")
	}
	var gaps []string
	gap := func(b bool, field string) {
		if b {
			gaps = append(gaps, field)
		}
	}
	// The revision template through the service mapping; what a worker
	// pool's revision has no field for (serving settings) is a gap.
	svc, err := fromV1Service(&runv1.Service{Spec: &runv1.ServiceSpec{Template: in.Spec.Template}}, "")
	if err != nil {
		return nil, err
	}
	t := svc.GetTemplate()
	gap(t.GetScaling() != nil, "spec.template.metadata.annotations[autoscaling.knative.dev/*]")
	gap(t.GetMaxInstanceRequestConcurrency() > 0, "spec.template.spec.containerConcurrency")
	gap(t.GetSessionAffinity(), "spec.template.metadata.annotations[run.googleapis.com/sessionAffinity]")
	gap(t.GetExecutionEnvironment() != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_UNSPECIFIED, "spec.template.metadata.annotations[run.googleapis.com/execution-environment]")
	gap(t.GetHealthCheckDisabled(), "spec.template.metadata.annotations[run.googleapis.com/health-check-disabled]")
	if ts := in.Spec.Template.Spec.TimeoutSeconds; ts > 0 {
		gap(true, "spec.template.spec.timeoutSeconds")
	}
	wp := &runpb.WorkerPool{Template: &runpb.WorkerPoolRevisionTemplate{Revision: t.GetRevision(), Labels: t.GetLabels(),
		Annotations: t.GetAnnotations(), VpcAccess: t.GetVpcAccess(), ServiceAccount: t.GetServiceAccount(),
		Containers: t.GetContainers(), Volumes: t.GetVolumes(), EncryptionKey: t.GetEncryptionKey(), NodeSelector: t.GetNodeSelector()}}
	for _, sp := range in.Spec.InstanceSplits {
		x := &runpb.InstanceSplit{Percent: int32(sp.Percent)}
		switch {
		case sp.LatestRevision && sp.RevisionName == "":
			x.Type = runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_LATEST
		case !sp.LatestRevision && sp.RevisionName != "":
			x.Type, x.Revision = runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_REVISION, sp.RevisionName
		default:
			return nil, apierror.InvalidArgument("spec.instanceSplits: each split names a revisionName or sets latestRevision")
		}
		wp.InstanceSplits = append(wp.InstanceSplits, x)
	}
	m := in.Metadata
	labels := map[string]string{}
	for k, v := range m.Labels {
		if k != v1LocationLabel {
			labels[k] = v
		}
	}
	if len(labels) > 0 {
		wp.Labels = labels
	}
	ann := map[string]string{}
	for k, v := range m.Annotations {
		switch k {
		case v1ClientName:
			wp.Client = v
		case v1ClientVersion:
			wp.ClientVersion = v
		case v1LaunchStage:
			st, ok := api.LaunchStage_value[v]
			if !ok {
				return nil, apierror.InvalidArgument("metadata.annotations[%s]: %q is not a launch stage", k, v)
			}
			wp.LaunchStage = api.LaunchStage(st)
		case v1ManualInstanceCount:
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return nil, apierror.InvalidArgument("metadata.annotations[%s]: %q is not an instance count", k, v)
			}
			c := int32(n)
			wp.Scaling = &runpb.WorkerPoolScaling{ManualInstanceCount: &c}
		default:
			gap(strings.HasPrefix(k, "run.googleapis.com/"), "metadata.annotations["+k+"]")
			ann[k] = v
		}
	}
	if len(ann) > 0 {
		wp.Annotations = ann
	}
	gap(m.GenerateName != "", "metadata.generateName")
	if len(gaps) > 0 {
		return nil, apierror.Unimplemented("unsupported Cloud Run v1 configuration: %s: not mapped", strings.Join(gaps, ", "))
	}
	return wp, nil
}

// lastSegment is a resource name's ID.
func lastSegment(name string) string { return name[strings.LastIndex(name, "/")+1:] }

// toV1Splits renders v2 instance splits as v1's.
func toV1Splits[T interface {
	GetType() runpb.InstanceSplitAllocationType
	GetRevision() string
	GetPercent() int32
}](in []T) []*runv1.InstanceSplit {
	var out []*runv1.InstanceSplit
	for _, x := range in {
		sp := &runv1.InstanceSplit{Percent: int64(x.GetPercent())}
		if x.GetType() == runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_LATEST {
			sp.LatestRevision = true
		} else {
			sp.RevisionName = lastSegment(x.GetRevision())
		}
		out = append(out, sp)
	}
	return out
}

// toV1WorkerPool renders a v2 worker pool in v1's shape; nil for nil.
func toV1WorkerPool(wp *runpb.WorkerPool) *runv1.WorkerPool {
	if wp == nil {
		return nil
	}
	project, location := projectOf(wp.GetName()), locationOf(wp.GetName())
	id := lastSegment(wp.GetName())
	labels := map[string]string{v1LocationLabel: location}
	for k, v := range wp.GetLabels() {
		labels[k] = v
	}
	ann := cloneAnn(wp.GetAnnotations())
	if ann == nil {
		ann = map[string]string{}
	}
	if wp.GetClient() != "" {
		ann[v1ClientName] = wp.GetClient()
	}
	if wp.GetClientVersion() != "" {
		ann[v1ClientVersion] = wp.GetClientVersion()
	}
	if st := wp.GetLaunchStage(); st != api.LaunchStage_LAUNCH_STAGE_UNSPECIFIED {
		ann[v1LaunchStage] = st.String()
	}
	if c := wp.GetScaling().ManualInstanceCount; c != nil {
		ann[v1ManualInstanceCount] = strconv.Itoa(int(*c))
	}
	if len(ann) == 0 {
		ann = nil
	}
	meta := &runv1.ObjectMeta{Name: id, Namespace: project, Labels: labels, Annotations: ann, Generation: wp.GetGeneration(),
		Uid: wp.GetUid(), ResourceVersion: wp.GetEtag(), SelfLink: "/apis/run.googleapis.com/v1/namespaces/" + project + "/workerpools/" + id}
	if t := wp.GetCreateTime(); t != nil {
		meta.CreationTimestamp = t.AsTime().UTC().Format(time.RFC3339)
	}
	t := wp.GetTemplate()
	tann := toV1CloudSQL(cloneAnn(t.GetAnnotations()), t.GetVolumes())
	tmpl := &runv1.RevisionTemplate{Metadata: &runv1.ObjectMeta{Name: t.GetRevision(), Labels: t.GetLabels(), Annotations: tann},
		Spec: &runv1.RevisionSpec{Containers: toV1Containers(t.GetContainers(), t.GetVolumes()), Volumes: toV1Volumes(t.GetVolumes()),
			ServiceAccountName: t.GetServiceAccount()}}
	out := &runv1.WorkerPool{ApiVersion: "run.googleapis.com/v1", Kind: "WorkerPool", Metadata: meta,
		Spec:   &runv1.WorkerPoolSpec{Template: tmpl, InstanceSplits: toV1Splits(wp.GetInstanceSplits())},
		Status: &runv1.WorkerPoolStatus{ObservedGeneration: wp.GetObservedGeneration(), InstanceSplits: toV1Splits(wp.GetInstanceSplitStatuses())}}
	if r := wp.GetLatestCreatedRevision(); r != "" {
		out.Status.LatestCreatedRevisionName = lastSegment(r)
	}
	if r := wp.GetLatestReadyRevision(); r != "" {
		out.Status.LatestReadyRevisionName = lastSegment(r)
	}
	if c := wp.GetTerminalCondition(); c != nil {
		out.Status.Conditions = append(out.Status.Conditions, v1Condition("Ready", c))
	}
	for _, c := range wp.GetConditions() {
		out.Status.Conditions = append(out.Status.Conditions, v1Condition(c.GetType(), c))
	}
	return out
}
