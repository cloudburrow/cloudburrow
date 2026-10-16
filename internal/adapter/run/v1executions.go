package run

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Cloud Run v1 jobs' replaceJob and run, and namespaces.executions and
// namespaces.tasks (#1379), mapped onto the v2 Jobs, Executions and Tasks
// servers as v1 create, get, list and delete are (#1349): an execution run
// with v1 is the v2 execution, and the console lists it with the rest.
//
// An execution and a task are addressed by ID in the namespace, at the
// region's {region}- host: executions/{execution} and tasks/{task}. Their
// labels are Google's, run.googleapis.com/job on an execution and
// run.googleapis.com/execution and run.googleapis.com/runningState on a
// task, which is what gcloud's list selectors match.

const (
	v1JobLabel          = "run.googleapis.com/job"
	v1ExecutionLabel    = "run.googleapis.com/execution"
	v1RunningStateLabel = "run.googleapis.com/runningState"
)

// v1ExecutionName is a v1 execution request's v2 execution name: the
// execution's batch Job, owned by the adapter and in the request's region.
// method, when set, is the custom method the {name} segment must end in.
func (s *Server) v1ExecutionName(r *http.Request, method string) (string, error) {
	id := r.PathValue("name")
	if method != "" {
		var ok bool
		if id, ok = strings.CutSuffix(id, ":"+method); !ok {
			return "", apierror.Unimplemented("%s %s is not served by the Cloud Run v1 API locally", r.Method, r.URL.Path)
		}
	}
	if strings.Contains(id, ":") {
		return "", apierror.Unimplemented("%s %s is not served by the Cloud Run v1 API locally", r.Method, r.URL.Path)
	}
	parent, err := v1JobParent(scopeOf(r), "")
	if err != nil {
		return "", err
	}
	return s.v1ExecutionByID(r.Context(), parent, id)
}

func (s *Server) v1ExecutionByID(ctx context.Context, parent, id string) (string, error) {
	var j kjob
	if err := s.kn.getObject(ctx, &j, resBatchJob, id, "execution "+id); err != nil {
		if apierror.From(err).Code == codes.NotFound {
			return "", apierror.NotFound("executions.run.googleapis.com %q not found", id)
		}
		return "", err
	}
	name := j.Metadata.Annotations[annRunName]
	if j.Metadata.Labels[labelOwned] != "true" || !strings.HasPrefix(name, parent+"/jobs/") ||
		!strings.HasSuffix(name, "/executions/"+id) {
		return "", apierror.NotFound("executions.run.googleapis.com %q not found", id)
	}
	return name, nil
}

func (s *Server) v1ReplaceJob(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	id, err := resourceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	var in runv1.Job
	if err := v1DecodeJob(r, &in); err != nil {
		v1Write(w, err, nil)
		return
	}
	if in.Metadata == nil || in.Metadata.Name != id {
		v1Write(w, apierror.InvalidArgument("metadata.name must be the job's name, %q", id), nil)
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
	job.Name = parent + "/jobs/" + id
	job.Etag = in.Metadata.ResourceVersion
	delay := in.Spec.Template.Spec.DelayExecution
	validateOnly := r.URL.Query().Get("dryRun") == "all"
	op, err := s.Jobs().UpdateJob(r.Context(), &runpb.UpdateJobRequest{Job: job, ValidateOnly: validateOnly})
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	if validateOnly {
		var out runpb.Job
		if err := op.GetResponse().UnmarshalTo(&out); err != nil {
			v1Write(w, apierror.Internal(err, "decode job %s", job.Name), nil)
			return
		}
		v1Write(w, nil, v1JobOf(&out, delay))
		return
	}
	// The v2 update rewrote the job's ConfigMap without the flag.
	if err := s.setV1JobDelay(r.Context(), job.Name, delay); err != nil {
		v1Write(w, err, nil)
		return
	}
	s.v1WriteJob(w, r, job.Name)
}

// v1RunJob is POST jobs/{name}:run. It answers with the new execution as
// it is when created; gcloud's --wait polls executions.get for Completed.
func (s *Server) v1RunJob(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("name"), ":run")
	if !ok || strings.Contains(id, ":") {
		v1Write(w, apierror.Unimplemented("%s %s is not served by the Cloud Run v1 API locally", r.Method, r.URL.Path), nil)
		return
	}
	parent, err := v1JobParent(scopeOf(r), "")
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	var in runv1.RunJobRequest
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		v1Write(w, apierror.InvalidArgument("request body: %v", err), nil)
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			v1Write(w, apierror.InvalidArgument("malformed request body: %v", err), nil)
			return
		}
	}
	overrides, err := fromV1Overrides(in.Overrides)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	jobName := parent + "/jobs/" + id
	// An execution is delayed when the run asks for it or the job's
	// template does (v1delay.go, #1450).
	delay := in.Overrides != nil && in.Overrides.DelayExecution
	if !delay {
		if delay, err = s.v1JobDelay(r.Context(), jobName); err != nil {
			v1Write(w, err, nil)
			return
		}
	}
	op, err := s.Jobs().RunJob(r.Context(), &runpb.RunJobRequest{Name: jobName, Overrides: overrides})
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	var exec runpb.Execution
	if err := op.GetMetadata().UnmarshalTo(&exec); err != nil {
		v1Write(w, apierror.Internal(err, "decode the execution of job %s", id), nil)
		return
	}
	if delay {
		_, execID, _ := strings.Cut(exec.GetName(), "/executions/")
		if err := s.setV1Delay(r.Context(), resBatchJob, execID, true); err != nil {
			v1Write(w, err, nil)
			return
		}
	}
	v1Write(w, nil, v1ExecutionOf(&exec, delay))
}

// fromV1Overrides converts v1's execution overrides to v2's.
func fromV1Overrides(in *runv1.Overrides) (*runpb.RunJobRequest_Overrides, error) {
	if in == nil {
		return nil, nil
	}
	// delayExecution has no v2 field; v1RunJob stores it beside the
	// execution (v1delay.go, #1450).
	var gaps []string
	gap := func(b bool, field string) {
		if b {
			gaps = append(gaps, "overrides.containerOverrides."+strings.TrimPrefix(field, "container."))
		}
	}
	out := &runpb.RunJobRequest_Overrides{TaskCount: int32(in.TaskCount)}
	if in.TimeoutSeconds > 0 {
		out.Timeout = durationpb.New(time.Duration(in.TimeoutSeconds) * time.Second)
	} else if in.TimeoutSeconds < 0 {
		return nil, apierror.InvalidArgument("overrides.timeoutSeconds must be positive")
	}
	for _, co := range in.ContainerOverrides {
		o := &runpb.RunJobRequest_Overrides_ContainerOverride{Name: co.Name, Args: co.Args, ClearArgs: co.ClearArgs}
		for _, e := range co.Env {
			o.Env = append(o.Env, fromV1Env(e, gap))
		}
		out.ContainerOverrides = append(out.ContainerOverrides, o)
	}
	if len(gaps) > 0 {
		return nil, apierror.Unimplemented("unsupported Cloud Run v1 configuration: %s: not mapped", strings.Join(gaps, ", "))
	}
	return out, nil
}

func (s *Server) v1GetExecution(w http.ResponseWriter, r *http.Request) {
	name, err := s.v1ExecutionName(r, "")
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	s.v1WriteExecution(w, r, name)
}

// v1WriteExecution answers with an execution, its delayExecution too.
func (s *Server) v1WriteExecution(w http.ResponseWriter, r *http.Request, name string) {
	e, err := s.Executions().GetExecution(r.Context(), &runpb.GetExecutionRequest{Name: name})
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	delay, err := s.v1ExecutionDelay(r.Context(), name)
	v1Write(w, err, v1ExecutionOf(e, delay))
}

func (s *Server) v1DeleteExecution(w http.ResponseWriter, r *http.Request) {
	name, err := s.v1ExecutionName(r, "")
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	e, err := s.Executions().GetExecution(r.Context(), &runpb.GetExecutionRequest{Name: name})
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	if _, err := s.Executions().DeleteExecution(r.Context(), &runpb.DeleteExecutionRequest{Name: name,
		ValidateOnly: r.URL.Query().Get("dryRun") == "all"}); err != nil {
		v1Write(w, err, nil)
		return
	}
	_, id, _ := strings.Cut(name, "/executions/")
	v1Write(w, nil, &runv1.Status{Status: "Success",
		Details: &runv1.StatusDetails{Name: id, Kind: "executions", Group: "run.googleapis.com", Uid: e.GetUid()}})
}

// v1CancelExecution is POST executions/{name}:cancel. It answers with the
// execution once its cancel is under way; it reads Cancelled when its pods
// are gone.
func (s *Server) v1CancelExecution(w http.ResponseWriter, r *http.Request) {
	name, err := s.v1ExecutionName(r, "cancel")
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	if _, err := s.Executions().CancelExecution(r.Context(), &runpb.CancelExecutionRequest{Name: name}); err != nil {
		v1Write(w, err, nil)
		return
	}
	s.v1WriteExecution(w, r, name)
}

// v1Executions is the region's executions, newest first: one job's, when
// job is set (none if it does not exist), or every job's.
func (s *Server) v1Executions(ctx context.Context, parent, job string) ([]*runpb.Execution, error) {
	jobs := []string{parent + "/jobs/" + job}
	if job == "" {
		jobs = nil
		resp, err := s.Jobs().ListJobs(ctx, &runpb.ListJobsRequest{Parent: parent, PageSize: 1000})
		if err != nil {
			return nil, err
		}
		for _, j := range resp.GetJobs() {
			jobs = append(jobs, j.GetName())
		}
	}
	var out []*runpb.Execution
	for _, j := range jobs {
		token := ""
		for {
			resp, err := s.Executions().ListExecutions(ctx, &runpb.ListExecutionsRequest{Parent: j, PageSize: 1000, PageToken: token})
			if err != nil {
				if apierror.From(err).Code == codes.NotFound {
					break
				}
				return nil, err
			}
			out = append(out, resp.GetExecutions()...)
			if token = resp.GetNextPageToken(); token == "" {
				break
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		ta, tb := out[a].GetCreateTime().AsTime(), out[b].GetCreateTime().AsTime()
		if !ta.Equal(tb) {
			return ta.After(tb)
		}
		return out[a].GetName() > out[b].GetName()
	})
	return out, nil
}

func (s *Server) v1ListExecutions(w http.ResponseWriter, r *http.Request) {
	sel, err := labelSelector(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	parent, err := v1JobParent(scopeOf(r), "")
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	execs, err := s.v1Executions(r.Context(), parent, sel.only(v1JobLabel))
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	delays, err := s.v1ExecutionDelays(r.Context())
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	byName := map[string]*runv1.Execution{}
	var names []string
	for i, e := range execs {
		v := v1ExecutionOf(e, delays[e.GetName()])
		if selected(v.Metadata, sel) {
			// Paging sorts its keys, so the key keeps the order.
			key := strconv.Itoa(100000000+i) + "/" + v.Metadata.Name
			byName[key] = v
			names = append(names, key)
		}
	}
	page, next, err := v1Page(r, "run-v1-executions:"+parent, names)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	out := &runv1.ListExecutionsResponse{ApiVersion: "run.googleapis.com/v1", Kind: "ExecutionsList",
		Metadata: &runv1.ListMeta{Continue: next}, Items: []*runv1.Execution{}}
	for _, n := range page {
		out.Items = append(out.Items, byName[n])
	}
	v1Write(w, nil, out)
}

// v1TaskExecution splits a task ID, {execution}-task{index}.
func v1TaskExecution(id string) (string, int, bool) {
	i := strings.LastIndex(id, "-task")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(id[i+len("-task"):])
	if err != nil || n < 0 || strconv.Itoa(n) != id[i+len("-task"):] {
		return "", 0, false
	}
	return id[:i], n, true
}

func (s *Server) v1GetTask(w http.ResponseWriter, r *http.Request) {
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
	execID, _, ok := v1TaskExecution(id)
	if !ok {
		v1Write(w, apierror.NotFound("tasks.run.googleapis.com %q not found", id), nil)
		return
	}
	exec, err := s.v1ExecutionByID(r.Context(), parent, execID)
	if err != nil {
		if apierror.From(err).Code == codes.NotFound {
			err = apierror.NotFound("tasks.run.googleapis.com %q not found", id)
		}
		v1Write(w, err, nil)
		return
	}
	t, err := s.Tasks().GetTask(r.Context(), &runpb.GetTaskRequest{Name: exec + "/tasks/" + id})
	v1Write(w, err, toV1Task(t))
}

func (s *Server) v1ListTasks(w http.ResponseWriter, r *http.Request) {
	sel, err := labelSelector(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	parent, err := v1JobParent(scopeOf(r), "")
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	var execs []string
	if id := sel.only(v1ExecutionLabel); id != "" {
		name, err := s.v1ExecutionByID(r.Context(), parent, id)
		if err != nil && apierror.From(err).Code != codes.NotFound {
			v1Write(w, err, nil)
			return
		}
		if err == nil {
			execs = append(execs, name)
		}
	} else {
		all, err := s.v1Executions(r.Context(), parent, sel.only(v1JobLabel))
		if err != nil {
			v1Write(w, err, nil)
			return
		}
		for _, e := range all {
			execs = append(execs, e.GetName())
		}
	}
	byName := map[string]*runv1.Task{}
	var names []string
	for i, e := range execs {
		token := ""
		for {
			resp, err := s.Tasks().ListTasks(r.Context(), &runpb.ListTasksRequest{Parent: e, PageSize: 1000, PageToken: token})
			if err != nil {
				if apierror.From(err).Code == codes.NotFound {
					break
				}
				v1Write(w, err, nil)
				return
			}
			for _, t := range resp.GetTasks() {
				v := toV1Task(t)
				if selected(v.Metadata, sel) {
					key := strconv.Itoa(100000000+i) + "/" + strconv.Itoa(100000000+int(t.GetIndex()))
					byName[key] = v
					names = append(names, key)
				}
			}
			if token = resp.GetNextPageToken(); token == "" {
				break
			}
		}
	}
	page, next, err := v1Page(r, "run-v1-tasks:"+parent, names)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	out := &runv1.ListTasksResponse{ApiVersion: "run.googleapis.com/v1", Kind: "TasksList",
		Metadata: &runv1.ListMeta{Continue: next}, Items: []*runv1.Task{}}
	for _, n := range page {
		out.Items = append(out.Items, byName[n])
	}
	v1Write(w, nil, out)
}

// v1Time is a timestamp in v1's RFC 3339 form; "" for none.
func v1Time(t *timestamppb.Timestamp) string {
	if t == nil {
		return ""
	}
	return t.AsTime().UTC().Format(time.RFC3339)
}

// v1Reason is a v2 condition's reason in v1's CamelCase.
func v1Reason(c *runpb.Condition) string {
	switch c.GetExecutionReason() {
	case runpb.Condition_NON_ZERO_EXIT_CODE:
		return "NonZeroExitCode"
	case runpb.Condition_CANCELLED:
		return "Cancelled"
	case runpb.Condition_CANCELLING:
		return "Cancelling"
	}
	return ""
}

// v1RunConditions renders an execution's or a task's Completed and Started,
// with ResourcesAvailable before Started, as gcloud's stages expect.
func v1RunConditions(cs []*runpb.Condition) []*runv1.GoogleCloudRunV1Condition {
	var out []*runv1.GoogleCloudRunV1Condition
	for _, c := range cs {
		v := v1Condition(c.GetType(), c)
		v.Reason = v1Reason(c)
		out = append(out, v)
		if c.GetType() == "Started" {
			ra := &runv1.GoogleCloudRunV1Condition{Type: "ResourcesAvailable", Status: "Unknown"}
			if v.Status == "True" {
				ra.Status, ra.LastTransitionTime = "True", v.LastTransitionTime
			}
			out = append(out, ra)
		}
	}
	return out
}

// v1RunMeta is an execution's or a task's metadata: its ID in the project's
// namespace, the region and Google's labels, and its etag as the
// resourceVersion.
func v1RunMeta(name, collection, uid, etag string, generation int64, created *timestamppb.Timestamp,
	labels, annotations map[string]string) *runv1.ObjectMeta {
	project := projectOf(name)
	id := name[strings.LastIndex(name, "/")+1:]
	l := map[string]string{v1LocationLabel: locationOf(name)}
	for k, v := range labels {
		l[k] = v
	}
	return &runv1.ObjectMeta{Name: id, Namespace: project, Labels: l, Annotations: cloneAnn(annotations),
		Uid: uid, ResourceVersion: etag, Generation: generation, CreationTimestamp: v1Time(created),
		SelfLink: "/apis/run.googleapis.com/v1/namespaces/" + project + "/" + collection + "/" + id}
}

// v1ExecutionOf renders a v2 execution in v1's shape, with its
// delayExecution, which v2 does not hold (v1delay.go); nil for nil.
func v1ExecutionOf(e *runpb.Execution, delay bool) *runv1.Execution {
	if e == nil {
		return nil
	}
	_, job, _ := strings.Cut(e.GetJob(), "/jobs/")
	labels := map[string]string{v1JobLabel: job}
	for k, v := range e.GetLabels() {
		labels[k] = v
	}
	tt := e.GetTemplate()
	return &runv1.Execution{ApiVersion: "run.googleapis.com/v1", Kind: "Execution",
		Metadata: v1RunMeta(e.GetName(), "executions", e.GetUid(), e.GetEtag(), e.GetGeneration(), e.GetCreateTime(),
			labels, toV1CloudSQL(cloneAnn(e.GetAnnotations()), tt.GetVolumes())),
		Spec: &runv1.ExecutionSpec{Parallelism: int64(e.GetParallelism()), TaskCount: int64(e.GetTaskCount()), DelayExecution: delay,
			Template: &runv1.TaskTemplateSpec{Spec: toV1TaskSpec(tt)}},
		Status: &runv1.ExecutionStatus{ObservedGeneration: e.GetObservedGeneration(), Conditions: v1RunConditions(e.GetConditions()),
			StartTime: v1Time(e.GetStartTime()), CompletionTime: v1Time(e.GetCompletionTime()),
			RunningCount: int64(e.GetRunningCount()), SucceededCount: int64(e.GetSucceededCount()), FailedCount: int64(e.GetFailedCount()),
			CancelledCount: int64(e.GetCancelledCount()), RetriedCount: int64(e.GetRetriedCount()), LogUri: e.GetLogUri()}}
}

// v1RunningState is a task's run.googleapis.com/runningState label.
func v1RunningState(t *runpb.Task) string {
	for _, c := range t.GetConditions() {
		if c.GetType() != "Completed" {
			continue
		}
		switch c.GetState() {
		case runpb.Condition_CONDITION_SUCCEEDED:
			return "Succeeded"
		case runpb.Condition_CONDITION_FAILED:
			if c.GetExecutionReason() == runpb.Condition_CANCELLED {
				return "Cancelled"
			}
			return "Failed"
		}
	}
	if t.GetStartTime() != nil {
		return "Running"
	}
	return "Pending"
}

// toV1Task renders a v2 task in v1's shape; nil for nil.
func toV1Task(t *runpb.Task) *runv1.Task {
	if t == nil {
		return nil
	}
	_, job, _ := strings.Cut(t.GetJob(), "/jobs/")
	_, exec, _ := strings.Cut(t.GetExecution(), "/executions/")
	labels := map[string]string{v1JobLabel: job, v1ExecutionLabel: exec, v1RunningStateLabel: v1RunningState(t)}
	for k, v := range t.GetLabels() {
		labels[k] = v
	}
	spec := toV1TaskSpec(&runpb.TaskTemplate{Containers: t.GetContainers(), Volumes: t.GetVolumes(),
		ServiceAccount: t.GetServiceAccount(), Timeout: t.GetTimeout(),
		Retries: &runpb.TaskTemplate_MaxRetries{MaxRetries: t.GetMaxRetries()}})
	st := &runv1.TaskStatus{Index: int64(t.GetIndex()), Retried: int64(t.GetRetried()), ObservedGeneration: t.GetObservedGeneration(),
		StartTime: v1Time(t.GetStartTime()), CompletionTime: v1Time(t.GetCompletionTime()),
		Conditions: v1RunConditions(t.GetConditions()), LogUri: t.GetLogUri()}
	if res := t.GetLastAttemptResult(); res != nil {
		st.LastAttemptResult = &runv1.TaskAttemptResult{ExitCode: int64(res.GetExitCode()), TermSignal: int64(res.GetTermSignal()),
			Status: &runv1.GoogleRpcStatus{Code: int64(res.GetStatus().GetCode()), Message: res.GetStatus().GetMessage()}}
	}
	return &runv1.Task{ApiVersion: "run.googleapis.com/v1", Kind: "Task",
		Metadata: v1RunMeta(t.GetName(), "tasks", t.GetUid(), t.GetEtag(), t.GetGeneration(), t.GetCreateTime(),
			labels, t.GetAnnotations()),
		Spec: spec, Status: st}
}
