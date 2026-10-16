package run

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Cloud Run v1 jobs, namespaces.jobs (#1349): list, get, create and delete,
// mapped onto the v2 Jobs server as v1 services map onto v2 services, so
// the two versions store one job and refuse the same configuration. The
// path is /apis/run.googleapis.com/v1/namespaces/{project}/jobs[/{name}],
// and the region is the {region}- host's or the cloud.googleapis.com/location
// label, as for services. replaceJob, run, executions and tasks stay 501.

// v1JobParent is a jobs request's v2 parent; every call needs a region.
func v1JobParent(sc v1Scope, label string) (string, error) {
	loc := sc.location
	if loc == "" {
		loc = label
	}
	if loc == "" {
		return "", apierror.InvalidArgument("no region: call a regional endpoint, or set the " + v1LocationLabel + " label")
	}
	if label != "" && label != loc {
		return "", apierror.InvalidArgument("label %s=%s does not match the request's region %s", v1LocationLabel, label, loc)
	}
	return fmt.Sprintf("projects/%s/locations/%s", sc.project, loc), nil
}

func (s *Server) v1ListJobs(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	sel, err := labelSelector(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	parent, err := v1JobParent(sc, "")
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	resp, err := s.Jobs().ListJobs(r.Context(), &runpb.ListJobsRequest{Parent: parent, PageSize: 1000})
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	byName := map[string]*runv1.Job{}
	var names []string
	for _, j := range resp.GetJobs() {
		v := toV1Job(j)
		if selected(v.Metadata.Labels, sel) {
			byName[j.GetName()] = v
			names = append(names, j.GetName())
		}
	}
	page, next, err := v1Page(r, "run-v1-jobs:"+parent, names)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	out := &runv1.ListJobsResponse{ApiVersion: "run.googleapis.com/v1", Kind: "JobsList",
		Metadata: &runv1.ListMeta{Continue: next}, Items: []*runv1.Job{}}
	for _, n := range page {
		out.Items = append(out.Items, byName[n])
	}
	v1Write(w, nil, out)
}

func (s *Server) v1GetJob(w http.ResponseWriter, r *http.Request) {
	id, err := resourceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	parent, err := v1JobParent(scopeOf(r), "")
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	j, err := s.Jobs().GetJob(r.Context(), &runpb.GetJobRequest{Name: parent + "/jobs/" + id})
	v1Write(w, err, toV1Job(j))
}

func (s *Server) v1CreateJob(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	var in runv1.Job
	if err := v1DecodeJob(r, &in); err != nil {
		v1Write(w, err, nil)
		return
	}
	if in.Metadata == nil || in.Metadata.Name == "" {
		v1Write(w, apierror.InvalidArgument("metadata.name is required"), nil)
		return
	}
	if ns := in.Metadata.Namespace; ns != "" && ns != sc.project {
		v1Write(w, apierror.InvalidArgument("metadata.namespace %q does not match the request's namespace %q", ns, sc.project), nil)
		return
	}
	parent, err := v1JobParent(sc, in.Metadata.Labels[v1LocationLabel])
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	job, err := fromV1Job(&in)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	validateOnly := r.URL.Query().Get("dryRun") == "all"
	if _, err := s.Jobs().CreateJob(r.Context(), &runpb.CreateJobRequest{Parent: parent, JobId: in.Metadata.Name, Job: job,
		ValidateOnly: validateOnly}); err != nil {
		v1Write(w, err, nil)
		return
	}
	if validateOnly {
		job.Name = parent + "/jobs/" + in.Metadata.Name
		v1Write(w, nil, toV1Job(job))
		return
	}
	got, err := s.Jobs().GetJob(r.Context(), &runpb.GetJobRequest{Name: parent + "/jobs/" + in.Metadata.Name})
	v1Write(w, err, toV1Job(got))
}

func (s *Server) v1DeleteJob(w http.ResponseWriter, r *http.Request) {
	id, err := resourceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	parent, err := v1JobParent(scopeOf(r), "")
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	j, err := s.Jobs().GetJob(r.Context(), &runpb.GetJobRequest{Name: parent + "/jobs/" + id})
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	if _, err := s.Jobs().DeleteJob(r.Context(), &runpb.DeleteJobRequest{Name: j.GetName(),
		ValidateOnly: r.URL.Query().Get("dryRun") == "all"}); err != nil {
		v1Write(w, err, nil)
		return
	}
	v1Write(w, nil, &runv1.Status{Status: "Success",
		Details: &runv1.StatusDetails{Name: id, Kind: "jobs", Group: "run.googleapis.com", Uid: j.GetUid()}})
}

// fromV1Job converts a v1 job to v2's shape. The task template's
// containers, volumes, service account and timeout go through the service
// mapping, so they are refused or accepted alike.
func fromV1Job(in *runv1.Job) (*runpb.Job, error) {
	if in.Spec == nil || in.Spec.Template == nil || in.Spec.Template.Spec == nil ||
		in.Spec.Template.Spec.Template == nil || in.Spec.Template.Spec.Template.Spec == nil {
		return nil, apierror.InvalidArgument("spec.template.spec.template.spec.containers is required")
	}
	es := in.Spec.Template.Spec
	ts := es.Template.Spec
	var gaps []string
	gap := func(b bool, field string) {
		if b {
			gaps = append(gaps, field)
		}
	}
	// The containers, through the service mapping, which takes a revision
	// template's shape.
	svc, err := fromV1Service(&runv1.Service{Spec: &runv1.ServiceSpec{Template: &runv1.RevisionTemplate{Spec: &runv1.RevisionSpec{
		Containers: ts.Containers, Volumes: ts.Volumes, ServiceAccountName: ts.ServiceAccountName}}}}, "")
	if err != nil {
		return nil, err
	}
	tt := &runpb.TaskTemplate{Containers: svc.GetTemplate().GetContainers(), Volumes: svc.GetTemplate().GetVolumes(),
		ServiceAccount: ts.ServiceAccountName}
	// maxRetries is omitted when 0 by the generated clients, so 0 is
	// v2's default (3) rather than no retries.
	if ts.MaxRetries > 0 {
		tt.Retries = &runpb.TaskTemplate_MaxRetries{MaxRetries: int32(ts.MaxRetries)}
	}
	if ts.TimeoutSeconds > 0 {
		tt.Timeout = durationpb.New(time.Duration(ts.TimeoutSeconds) * time.Second)
	}
	job := &runpb.Job{Template: &runpb.ExecutionTemplate{Parallelism: int32(es.Parallelism), TaskCount: int32(es.TaskCount), Template: tt}}
	if m := in.Metadata; m != nil {
		labels := map[string]string{}
		for k, v := range m.Labels {
			if k != v1LocationLabel {
				labels[k] = v
			}
		}
		if len(labels) > 0 {
			job.Labels = labels
		}
		ann := map[string]string{}
		for k, v := range m.Annotations {
			switch k {
			case v1ClientName:
				job.Client = v
			case v1ClientVersion:
				job.ClientVersion = v
			default:
				gap(strings.HasPrefix(k, "run.googleapis.com/"), "metadata.annotations["+k+"]")
				ann[k] = v
			}
		}
		if len(ann) > 0 {
			job.Annotations = ann
		}
		gap(m.GenerateName != "", "metadata.generateName")
	}
	if tm := in.Spec.Template.Metadata; tm != nil {
		job.Template.Labels = tm.Labels
		ann := map[string]string{}
		for k, v := range tm.Annotations {
			switch k {
			case v1ClientName, v1ClientVersion:
				// Which tool wrote the template: informational, as on a
				// revision template.
			case v1CloudSQLInstances:
				vols, err := fromV1CloudSQL("spec.template.metadata.annotations["+k+"]", v, tt.Volumes, tt.Containers)
				if err != nil {
					return nil, err
				}
				tt.Volumes = vols
			default:
				gap(strings.HasPrefix(k, "run.googleapis.com/"), "spec.template.metadata.annotations["+k+"]")
				ann[k] = v
			}
		}
		if len(ann) > 0 {
			job.Template.Annotations = ann
		}
	}
	if len(gaps) > 0 {
		return nil, apierror.Unimplemented("unsupported Cloud Run v1 configuration: %s: not mapped", strings.Join(gaps, ", "))
	}
	return job, nil
}

// toV1Job renders a v2 job in v1's shape; nil for nil.
func toV1Job(j *runpb.Job) *runv1.Job {
	if j == nil {
		return nil
	}
	project, location := projectOf(j.GetName()), locationOf(j.GetName())
	_, id, _ := strings.Cut(j.GetName(), "/jobs/")
	labels := map[string]string{v1LocationLabel: location}
	for k, v := range j.GetLabels() {
		labels[k] = v
	}
	ann := map[string]string{}
	for k, v := range j.GetAnnotations() {
		ann[k] = v
	}
	if j.GetClient() != "" {
		ann[v1ClientName] = j.GetClient()
	}
	if j.GetClientVersion() != "" {
		ann[v1ClientVersion] = j.GetClientVersion()
	}
	if len(ann) == 0 {
		ann = nil
	}
	meta := &runv1.ObjectMeta{Name: id, Namespace: project, Labels: labels, Annotations: ann,
		Generation: j.GetGeneration(), Uid: j.GetUid(), ResourceVersion: j.GetEtag(), SelfLink: "/apis/run.googleapis.com/v1/namespaces/" + project + "/jobs/" + id}
	if t := j.GetCreateTime(); t != nil {
		meta.CreationTimestamp = t.AsTime().UTC().Format(time.RFC3339)
	}
	et := j.GetTemplate()
	tt := et.GetTemplate()
	ts := &runv1.TaskSpec{Containers: toV1Containers(tt.GetContainers(), tt.GetVolumes()), Volumes: toV1Volumes(tt.GetVolumes()),
		ServiceAccountName: tt.GetServiceAccount(), TimeoutSeconds: tt.GetTimeout().GetSeconds()}
	if r, ok := tt.GetRetries().(*runpb.TaskTemplate_MaxRetries); ok {
		ts.MaxRetries = int64(r.MaxRetries)
	}
	out := &runv1.Job{ApiVersion: "run.googleapis.com/v1", Kind: "Job", Metadata: meta,
		Spec: &runv1.JobSpec{Template: &runv1.ExecutionTemplateSpec{
			Metadata: &runv1.ObjectMeta{Labels: et.GetLabels(), Annotations: toV1CloudSQL(cloneAnn(et.GetAnnotations()), tt.GetVolumes())},
			Spec: &runv1.ExecutionSpec{Parallelism: int64(et.GetParallelism()), TaskCount: int64(et.GetTaskCount()),
				Template: &runv1.TaskTemplateSpec{Spec: ts}}}},
		Status: &runv1.JobStatus{ObservedGeneration: j.GetObservedGeneration(), ExecutionCount: int64(j.GetExecutionCount())}}
	if c := j.GetTerminalCondition(); c != nil {
		out.Status.Conditions = []*runv1.GoogleCloudRunV1Condition{v1Condition("Ready", c)}
	}
	if e := j.GetLatestCreatedExecution(); e != nil {
		_, eid, _ := strings.Cut(e.GetName(), "/executions/")
		out.Status.LatestCreatedExecution = &runv1.ExecutionReference{Name: eid}
		if t := e.GetCreateTime(); t != nil {
			out.Status.LatestCreatedExecution.CreationTimestamp = t.AsTime().UTC().Format(time.RFC3339)
		}
	}
	return out
}

// v1Condition renders a v2 condition as v1's, of the given type.
func v1Condition(typ string, c *runpb.Condition) *runv1.GoogleCloudRunV1Condition {
	status := "Unknown"
	switch c.GetState() {
	case runpb.Condition_CONDITION_SUCCEEDED:
		status = "True"
	case runpb.Condition_CONDITION_FAILED:
		status = "False"
	}
	out := &runv1.GoogleCloudRunV1Condition{Type: typ, Status: status, Message: c.GetMessage()}
	if t := c.GetLastTransitionTime(); t != nil {
		out.LastTransitionTime = t.AsTime().UTC().Format(time.RFC3339)
	}
	return out
}

// cloneAnn copies annotations, so adding to them leaves the job's alone.
func cloneAnn(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
