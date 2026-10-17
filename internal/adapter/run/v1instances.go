package run

import (
	"context"
	"net/http"
	"strings"
	"time"

	iampb "cloud.google.com/go/iam/apiv1/iampb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/genproto/googleapis/api"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Cloud Run v1 instances, namespaces.instances (#1546): list, get, create,
// replaceInstance, delete, start and stop, at
// /apis/run.googleapis.com/v1/namespaces/{project}/instances[/{name}], and
// the IAM methods at projects.locations.instances. They are mapped onto
// the v2 Instances server, as v1 jobs are onto v2's Jobs, so both
// versions store one instance and refuse the same configuration. The
// region is the {region}- host's or the cloud.googleapis.com/location
// label, as for jobs.

// The annotations v1 carries an instance's v2 fields in, as the v1
// discovery document's ObjectMeta lists them for an Instance.
const (
	v1Ingress            = "run.googleapis.com/ingress"
	v1InvokerIAMDisabled = "run.googleapis.com/invoker-iam-disabled"
)

var v1IngressValues = map[string]runpb.IngressTraffic{
	"all":                               runpb.IngressTraffic_INGRESS_TRAFFIC_ALL,
	"internal":                          runpb.IngressTraffic_INGRESS_TRAFFIC_INTERNAL_ONLY,
	"internal-and-cloud-load-balancing": runpb.IngressTraffic_INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER,
	"none":                              runpb.IngressTraffic_INGRESS_TRAFFIC_NONE,
}

func (s *Server) v1InstanceName(r *http.Request) (string, error) {
	id, err := resourceName(r)
	if err != nil {
		return "", err
	}
	parent, err := v1JobParent(scopeOf(r), "")
	if err != nil {
		return "", err
	}
	return parent + "/instances/" + id, nil
}

// v1ListInstances is namespaces.instances.list: a region's instances, or
// at the global endpoint every region's, each with its location label.
func (s *Server) v1ListInstances(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	sel, err := labelSelector(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	locs, err := s.v1KindLocations(r.Context(), kindInstance, sc)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	byName := map[string]*runv1.Instance{}
	var names []string
	for _, loc := range locs {
		resp, err := s.Instances().ListInstances(r.Context(), &runpb.ListInstancesRequest{Parent: "projects/" + sc.project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			v1Write(w, err, nil)
			return
		}
		for _, in := range resp.GetInstances() {
			if v := toV1Instance(in); selected(v.Metadata, sel) {
				byName[in.GetName()] = v
				names = append(names, in.GetName())
			}
		}
	}
	page, next, err := v1Page(r, "run-v1-instances:projects/"+sc.project+"/locations/"+sc.location, names)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	out := &runv1.ListInstancesResponse{ApiVersion: "run.googleapis.com/v1", Kind: "InstancesList",
		Metadata: &runv1.ListMeta{Continue: next}, Items: []*runv1.Instance{}}
	for _, n := range page {
		out.Items = append(out.Items, byName[n])
	}
	v1Write(w, nil, out)
}

// v1KindLocations are the regions a v1 list reads: the request's, or with
// none every region holding a workload of kind in the project.
func (s *Server) v1KindLocations(ctx context.Context, kind string, sc v1Scope) ([]string, error) {
	if sc.location != "" {
		return []string{sc.location}, nil
	}
	cms, err := s.specsOfKind(ctx, kind)
	if err != nil {
		return nil, err
	}
	byName := map[string]kconfigMap{}
	for _, cm := range cms {
		byName[cm.Metadata.Annotations[annRunName]] = cm
	}
	return v1JobLocations(byName, sc.project), nil
}

func (s *Server) v1GetInstance(w http.ResponseWriter, r *http.Request) {
	name, err := s.v1InstanceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	in, err := s.Instances().GetInstance(r.Context(), &runpb.GetInstanceRequest{Name: name})
	v1Write(w, err, toV1Instance(in))
}

// v1DecodeInstance reads a v1 Instance body, and checks its name and
// namespace against the request.
func v1DecodeInstance(r *http.Request, sc v1Scope) (*runv1.Instance, error) {
	var in runv1.Instance
	if err := v1DecodeDroppingSandboxes(r, &in, "spec"); err != nil {
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

func (s *Server) v1CreateInstance(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	in, err := v1DecodeInstance(r, sc)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	parent, err := v1JobParent(sc, in.Metadata.Labels[v1LocationLabel])
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	inst, err := fromV1Instance(in)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	validateOnly := r.URL.Query().Get("dryRun") == "all"
	if _, err := s.Instances().CreateInstance(r.Context(), &runpb.CreateInstanceRequest{Parent: parent, InstanceId: in.Metadata.Name,
		Instance: inst, ValidateOnly: validateOnly}); err != nil {
		v1Write(w, err, nil)
		return
	}
	name := parent + "/instances/" + in.Metadata.Name
	if validateOnly {
		inst.Name = name
		v1Write(w, nil, toV1Instance(inst))
		return
	}
	got, err := s.Instances().GetInstance(r.Context(), &runpb.GetInstanceRequest{Name: name})
	v1Write(w, err, toV1Instance(got))
}

// v1ReplaceInstance is namespaces.instances.replaceInstance: the
// instance's configuration replaced, its running or stopped state kept.
func (s *Server) v1ReplaceInstance(w http.ResponseWriter, r *http.Request) {
	name, err := s.v1InstanceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	in, err := v1DecodeInstance(r, scopeOf(r))
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	if !strings.HasSuffix(name, "/instances/"+in.Metadata.Name) {
		v1Write(w, apierror.InvalidArgument("metadata.name %q does not match the request's instance", in.Metadata.Name), nil)
		return
	}
	if l := in.Metadata.Labels[v1LocationLabel]; l != "" && l != locationOf(name) {
		v1Write(w, apierror.InvalidArgument("label %s=%s does not match the request's region %s", v1LocationLabel, l, locationOf(name)), nil)
		return
	}
	inst, err := fromV1Instance(in)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	inst.Name, inst.Etag = name, in.Metadata.ResourceVersion
	got, err := s.Instances().replaceInstance(r.Context(), inst, r.URL.Query().Get("dryRun") == "all")
	v1Write(w, err, toV1Instance(got))
}

func (s *Server) v1DeleteInstance(w http.ResponseWriter, r *http.Request) {
	name, err := s.v1InstanceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	in, err := s.Instances().GetInstance(r.Context(), &runpb.GetInstanceRequest{Name: name})
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	if _, err := s.Instances().DeleteInstance(r.Context(), &runpb.DeleteInstanceRequest{Name: name,
		ValidateOnly: r.URL.Query().Get("dryRun") == "all"}); err != nil {
		v1Write(w, err, nil)
		return
	}
	_, id, _ := strings.Cut(name, "/instances/")
	v1Write(w, nil, &runv1.Status{Status: "Success",
		Details: &runv1.StatusDetails{Name: id, Kind: "instances", Group: "run.googleapis.com", Uid: in.GetUid()}})
}

// v1StartStopInstance is namespaces.instances.start and stop, the custom
// methods {name}:start and {name}:stop.
func (s *Server) v1StartStopInstance(w http.ResponseWriter, r *http.Request) {
	n := r.PathValue("name")
	id, verb, _ := strings.Cut(n, ":")
	if (verb != "start" && verb != "stop") || id == "" {
		v1Write(w, apierror.Unimplemented("%s %s is not served by the Cloud Run v1 API locally", r.Method, r.URL.Path), nil)
		return
	}
	parent, err := v1JobParent(scopeOf(r), "")
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	var body runv1.StopInstanceRequest // StartInstanceRequest has the same one field
	if r.ContentLength != 0 {
		if err := v1DecodeDroppingSandboxes(r, &body); err != nil {
			v1Write(w, err, nil)
			return
		}
	}
	name := parent + "/instances/" + id
	validateOnly := body.DryRun == "all" || r.URL.Query().Get("dryRun") == "all"
	if _, err := s.Instances().setStopped(r.Context(), name, "", validateOnly, verb == "stop"); err != nil {
		v1Write(w, err, nil)
		return
	}
	got, err := s.Instances().GetInstance(r.Context(), &runpb.GetInstanceRequest{Name: name})
	v1Write(w, err, toV1Instance(got))
}

// fromV1Instance converts a v1 instance to v2's shape. The containers,
// volumes and service account go through the service mapping, so they are
// refused or accepted alike.
func fromV1Instance(in *runv1.Instance) (*runpb.Instance, error) {
	if in.Spec == nil {
		return nil, apierror.InvalidArgument("spec.containers is required")
	}
	var gaps []string
	gap := func(b bool, field string) {
		if b {
			gaps = append(gaps, field)
		}
	}
	svc, err := fromV1Service(&runv1.Service{Spec: &runv1.ServiceSpec{Template: &runv1.RevisionTemplate{Spec: &runv1.RevisionSpec{
		Containers: in.Spec.Containers, Volumes: in.Spec.Volumes, ServiceAccountName: in.Spec.ServiceAccountName}}}}, "")
	if err != nil {
		return nil, err
	}
	out := &runpb.Instance{Containers: svc.GetTemplate().GetContainers(), Volumes: svc.GetTemplate().GetVolumes(),
		ServiceAccount: in.Spec.ServiceAccountName}
	gap(len(in.Spec.NodeSelector) > 0, "spec.nodeSelector")
	gap(in.Spec.RestartPolicy != "" && in.Spec.RestartPolicy != "Always", "spec.restartPolicy")
	m := in.Metadata
	labels := map[string]string{}
	for k, v := range m.Labels {
		if k != v1LocationLabel {
			labels[k] = v
		}
	}
	if len(labels) > 0 {
		out.Labels = labels
	}
	ann := map[string]string{}
	for k, v := range m.Annotations {
		switch k {
		case v1ClientName:
			out.Client = v
		case v1ClientVersion:
			out.ClientVersion = v
		case v1LaunchStage:
			st, ok := api.LaunchStage_value[v]
			if !ok {
				return nil, apierror.InvalidArgument("metadata.annotations[%s]: %q is not a launch stage", k, v)
			}
			out.LaunchStage = api.LaunchStage(st)
		case v1Ingress:
			ing, ok := v1IngressValues[v]
			if !ok {
				return nil, apierror.InvalidArgument("metadata.annotations[%s]: %q is not an ingress setting", k, v)
			}
			out.Ingress = ing
		case v1InvokerIAMDisabled:
			out.InvokerIamDisabled = v == "true"
		case v1CloudSQLInstances:
			vols, err := fromV1CloudSQL("metadata.annotations["+k+"]", v, out.Volumes, out.Containers)
			if err != nil {
				return nil, err
			}
			out.Volumes = vols
		default:
			gap(strings.HasPrefix(k, "run.googleapis.com/"), "metadata.annotations["+k+"]")
			ann[k] = v
		}
	}
	if len(ann) > 0 {
		out.Annotations = ann
	}
	gap(m.GenerateName != "", "metadata.generateName")
	if len(gaps) > 0 {
		return nil, apierror.Unimplemented("unsupported Cloud Run v1 configuration: %s: not mapped", strings.Join(gaps, ", "))
	}
	return out, nil
}

// toV1Instance renders a v2 instance in v1's shape; nil for nil.
func toV1Instance(in *runpb.Instance) *runv1.Instance {
	if in == nil {
		return nil
	}
	project, location := projectOf(in.GetName()), locationOf(in.GetName())
	_, id, _ := strings.Cut(in.GetName(), "/instances/")
	labels := map[string]string{v1LocationLabel: location}
	for k, v := range in.GetLabels() {
		labels[k] = v
	}
	ann := cloneAnn(in.GetAnnotations())
	if ann == nil {
		ann = map[string]string{}
	}
	if in.GetClient() != "" {
		ann[v1ClientName] = in.GetClient()
	}
	if in.GetClientVersion() != "" {
		ann[v1ClientVersion] = in.GetClientVersion()
	}
	if st := in.GetLaunchStage(); st != api.LaunchStage_LAUNCH_STAGE_UNSPECIFIED {
		ann[v1LaunchStage] = st.String()
	}
	for k, v := range v1IngressValues {
		if v == in.GetIngress() && v != runpb.IngressTraffic_INGRESS_TRAFFIC_ALL {
			ann[v1Ingress] = k
		}
	}
	if in.GetInvokerIamDisabled() {
		ann[v1InvokerIAMDisabled] = "true"
	}
	ann = toV1CloudSQL(ann, in.GetVolumes())
	if len(ann) == 0 {
		ann = nil
	}
	meta := &runv1.ObjectMeta{Name: id, Namespace: project, Labels: labels, Annotations: ann, Generation: in.GetGeneration(),
		Uid: in.GetUid(), ResourceVersion: in.GetEtag(), SelfLink: "/apis/run.googleapis.com/v1/namespaces/" + project + "/instances/" + id}
	if t := in.GetCreateTime(); t != nil {
		meta.CreationTimestamp = t.AsTime().UTC().Format(time.RFC3339)
	}
	out := &runv1.Instance{ApiVersion: "run.googleapis.com/v1", Kind: "Instance", Metadata: meta,
		Spec: &runv1.InstanceSpec{Containers: toV1Containers(in.GetContainers(), in.GetVolumes()), Volumes: toV1Volumes(in.GetVolumes()),
			ServiceAccountName: in.GetServiceAccount()},
		Status: &runv1.InstanceStatus{ObservedGeneration: in.GetObservedGeneration(), LogUri: in.GetLogUri(), Urls: in.GetUrls()}}
	if c := in.GetTerminalCondition(); c != nil {
		out.Status.Conditions = append(out.Status.Conditions, v1Condition("Ready", c))
	}
	for _, c := range in.GetConditions() {
		out.Status.Conditions = append(out.Status.Conditions, v1Condition(c.GetType(), c))
	}
	return out
}

// replaceInstance replaces a stored instance's configuration, keeping
// whether it runs; v1's replaceInstance, as v2 has no UpdateInstance. A
// non-empty etag must be the stored one.
func (i *InstancesServer) replaceInstance(ctx context.Context, in *runpb.Instance, validateOnly bool) (*runpb.Instance, error) {
	s := i.s
	w, _, err := parseInstanceName(in.GetName())
	if err != nil {
		return nil, err
	}
	cm, err := s.loadSpec(ctx, &w)
	if err != nil {
		return nil, err
	}
	if err := checkResourceEtag(in.GetEtag(), cm.Metadata.ResourceVersion, "instance"); err != nil {
		return nil, err
	}
	if err := checkInstance(in); err != nil {
		return nil, err
	}
	cur, stopped, err := s.readInstance(ctx, w, cm)
	if err != nil {
		return nil, err
	}
	spec := instanceSpec(in, cur.GetGeneration()+1)
	containers, _ := servingContainers(spec.GetContainers())
	if _, err := s.deployment(w, podSpec{containers: containers, volumes: spec.GetVolumes(), serviceAccount: spec.GetServiceAccount()}, 1); err != nil {
		return nil, err
	}
	if validateOnly {
		spec.CreateTime, spec.Uid = cur.GetCreateTime(), cur.GetUid()
		return spec, nil
	}
	if err := s.writeInstance(ctx, w, spec, stopped, "replace", cm.Metadata.ResourceVersion); err != nil {
		return nil, err
	}
	return i.GetInstance(ctx, &runpb.GetInstanceRequest{Name: in.GetName()})
}

// instanceOwner finds the stored spec behind a Cloud Run instance name,
// for the v1 IAM methods on projects.locations.instances (#1546).
func (s *Server) instanceOwner(ctx context.Context, name string) (iamOwner, error) {
	w, _, err := parseInstanceName(name)
	if err != nil {
		return iamOwner{}, err
	}
	cm, err := s.loadSpec(ctx, &w)
	if err != nil {
		return iamOwner{}, err
	}
	return iamOwner{kind: "instance", id: w.iamKey(), name: name, apiVersion: "v1", objKind: "ConfigMap",
		objName: cm.Metadata.Name, uid: cm.Metadata.UID}, nil
}

// GetIamPolicy is an instance's stored policy, which v1 serves at
// projects.locations.instances.getIamPolicy; nothing enforces it.
func (i *InstancesServer) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	o, err := i.s.instanceOwner(ctx, req.GetResource())
	if err != nil {
		return nil, err
	}
	return i.s.getPolicy(ctx, o, req)
}

// SetIamPolicy stores an instance's policy with a new etag; a stale etag
// is ABORTED.
func (i *InstancesServer) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	o, err := i.s.instanceOwner(ctx, req.GetResource())
	if err != nil {
		return nil, err
	}
	return i.s.setPolicy(ctx, o, req)
}

// TestIamPermissions returns every requested permission on an instance.
func (i *InstancesServer) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	_, err := i.s.instanceOwner(ctx, req.GetResource())
	if err != nil && apierror.From(err).Code == codes.InvalidArgument {
		return nil, err
	}
	return testPermissions(err, req)
}
