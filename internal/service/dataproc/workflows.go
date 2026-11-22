package dataproc

// Workflow templates (#2162): projects.regions.workflowTemplates and
// projects.locations.workflowTemplates, as workflow_templates.proto and
// the workflows guide document them
// (https://docs.cloud.google.com/dataproc/docs/concepts/workflows/overview,
// .../use-workflows and .../workflow-parameters). Instantiating one runs
// its DAG of jobs on a managed cluster (created first, deleted at the
// end) or on an existing cluster its labels select; the operation's
// metadata is a WorkflowMetadata with the workflow's graph.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

func fieldMask(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }

var (
	workflowRE = regexp.MustCompile(`^projects/([^/]+)/(regions|locations)/([^/]+)/workflowTemplates/([^/]+)$`)
	// managedNameRE: "The name must contain only lower-case letters
	// (a-z), numbers (0-9), and hyphens (-). Must begin with a letter.
	// Cannot begin or end with hyphen. Must consist of between 2 and 35
	// characters."
	managedNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,33}[a-z0-9]$`)
	// paramNameRE: "The parameter name ... must contain only capital
	// letters (A-Z), numbers (0-9), and underscores (_), and must not
	// start with a number. The maximum length is 40 characters."
	paramNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,39}$`)
)

func workflowKeyOf(name string) string {
	m := workflowRE.FindStringSubmatch(name)
	if m == nil {
		return name
	}
	return "projects/" + m[1] + "/regions/" + m[3] + "/workflowTemplates/" + m[4]
}

// randomSuffix is n lower-case letters and digits.
func randomSuffix(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func orderedJobType(j *dataprocpb.OrderedJob) *dataprocpb.Job {
	out := &dataprocpb.Job{}
	switch t := j.GetJobType().(type) {
	case *dataprocpb.OrderedJob_HadoopJob:
		out.TypeJob = &dataprocpb.Job_HadoopJob{HadoopJob: t.HadoopJob}
	case *dataprocpb.OrderedJob_SparkJob:
		out.TypeJob = &dataprocpb.Job_SparkJob{SparkJob: t.SparkJob}
	case *dataprocpb.OrderedJob_PysparkJob:
		out.TypeJob = &dataprocpb.Job_PysparkJob{PysparkJob: t.PysparkJob}
	case *dataprocpb.OrderedJob_HiveJob:
		out.TypeJob = &dataprocpb.Job_HiveJob{HiveJob: t.HiveJob}
	case *dataprocpb.OrderedJob_PigJob:
		out.TypeJob = &dataprocpb.Job_PigJob{PigJob: t.PigJob}
	case *dataprocpb.OrderedJob_SparkRJob:
		out.TypeJob = &dataprocpb.Job_SparkRJob{SparkRJob: t.SparkRJob}
	case *dataprocpb.OrderedJob_SparkSqlJob:
		out.TypeJob = &dataprocpb.Job_SparkSqlJob{SparkSqlJob: t.SparkSqlJob}
	case *dataprocpb.OrderedJob_PrestoJob:
		out.TypeJob = &dataprocpb.Job_PrestoJob{PrestoJob: t.PrestoJob}
	case *dataprocpb.OrderedJob_TrinoJob:
		out.TypeJob = &dataprocpb.Job_TrinoJob{TrinoJob: t.TrinoJob}
	case *dataprocpb.OrderedJob_FlinkJob:
		out.TypeJob = &dataprocpb.Job_FlinkJob{FlinkJob: t.FlinkJob}
	}
	return out
}

// checkWorkflow checks a template as the reference documents its fields.
func (s *Server) checkWorkflow(t *dataprocpb.WorkflowTemplate, inline bool) error {
	if !inline && !policyIDRE.MatchString(t.GetId()) {
		return apierror.InvalidArgument("Workflow template id %q must be 3 to 50 letters, numbers, underscores and hyphens, not beginning or ending with an underscore or hyphen", t.GetId())
	}
	if len(t.GetLabels()) > maxLabels {
		return apierror.InvalidArgument("No more than %d labels can be associated with a template", maxLabels)
	}
	switch p := t.GetPlacement().GetPlacement().(type) {
	case *dataprocpb.WorkflowTemplatePlacement_ManagedCluster:
		mc := p.ManagedCluster
		if !managedNameRE.MatchString(mc.GetClusterName()) {
			return apierror.InvalidArgument("placement.managed_cluster.cluster_name %q must be 2 to 35 lower-case letters, numbers and hyphens, beginning with a letter and not ending with a hyphen", mc.GetClusterName())
		}
		if mc.GetConfig() == nil {
			return apierror.InvalidArgument("placement.managed_cluster.config is required")
		}
	case *dataprocpb.WorkflowTemplatePlacement_ClusterSelector:
		if len(p.ClusterSelector.GetClusterLabels()) == 0 {
			return apierror.InvalidArgument("placement.cluster_selector.cluster_labels is required")
		}
	default:
		return apierror.InvalidArgument("placement is required: managed_cluster or cluster_selector")
	}
	if len(t.GetJobs()) == 0 {
		return apierror.InvalidArgument("jobs is required: a template has at least one job")
	}
	steps := map[string]bool{}
	for _, j := range t.GetJobs() {
		if !policyIDRE.MatchString(j.GetStepId()) {
			return apierror.InvalidArgument("Step id %q must be 3 to 50 letters, numbers, underscores and hyphens, not beginning or ending with an underscore or hyphen", j.GetStepId())
		}
		if steps[j.GetStepId()] {
			return apierror.InvalidArgument("Step id %q must be unique among the template's jobs", j.GetStepId())
		}
		steps[j.GetStepId()] = true
		if j.GetJobType() == nil {
			return apierror.InvalidArgument("Step %q needs a job", j.GetStepId())
		}
		if len(j.GetLabels()) > maxLabels {
			return apierror.InvalidArgument("No more than %d labels can be associated with a job", maxLabels)
		}
		if err := checkScheduling(j.GetScheduling()); err != nil {
			return err
		}
	}
	for _, j := range t.GetJobs() {
		for _, p := range j.GetPrerequisiteStepIds() {
			if !steps[p] {
				return apierror.InvalidArgument("Step %q's prerequisite %q is not a step of the template", j.GetStepId(), p)
			}
		}
	}
	if cyc := cycle(t.GetJobs()); cyc != "" {
		return apierror.InvalidArgument("The template's jobs are not a directed acyclic graph: step %q depends on itself", cyc)
	}
	if d := t.GetDagTimeout(); d != nil && (d.AsDuration() < 10*time.Minute || d.AsDuration() > 24*time.Hour) {
		return apierror.InvalidArgument("dag_timeout must be from 10 minutes (\"600s\") to 24 hours (\"86400s\")")
	}
	names := map[string]bool{}
	for _, p := range t.GetParameters() {
		if !paramNameRE.MatchString(p.GetName()) {
			return apierror.InvalidArgument("Parameter name %q must contain only capital letters, numbers and underscores, not start with a number, and be at most 40 characters", p.GetName())
		}
		if names[p.GetName()] {
			return apierror.InvalidArgument("Parameter name %q is not unique", p.GetName())
		}
		names[p.GetName()] = true
		if len(p.GetFields()) == 0 {
			return apierror.InvalidArgument("Parameter %q needs at least one field", p.GetName())
		}
		for _, re := range p.GetValidation().GetRegex().GetRegexes() {
			if _, err := regexp.Compile(re); err != nil {
				return apierror.InvalidArgument("Parameter %q's regex %q: %v", p.GetName(), re, err)
			}
		}
	}
	// Every parameter's fields must name a field of the template.
	if _, err := substitute(t, nil, true); err != nil {
		return err
	}
	return nil
}

// cycle is a step that depends on itself through its prerequisites, or "".
func cycle(jobs []*dataprocpb.OrderedJob) string {
	pre := map[string][]string{}
	for _, j := range jobs {
		pre[j.GetStepId()] = j.GetPrerequisiteStepIds()
	}
	state := map[string]int{}
	var visit func(string) string
	visit = func(n string) string {
		switch state[n] {
		case 1:
			return n
		case 2:
			return ""
		}
		state[n] = 1
		for _, p := range pre[n] {
			if c := visit(p); c != "" {
				return c
			}
		}
		state[n] = 2
		return ""
	}
	for _, j := range jobs {
		if c := visit(j.GetStepId()); c != "" {
			return c
		}
	}
	return ""
}

// pathTokenRE is one token of a parameter's field path: a field name,
// a ['key'] selector (a map's key, or the step of jobs) or an [N] index.
var pathTokenRE = regexp.MustCompile(`^(?:\.?([A-Za-z_][A-Za-z0-9_]*)|\['([^']*)'\]|\[(\d+)\])`)

type pathToken struct {
	field, key string
	index      int // -1 when not an index
}

func parsePath(path string) ([]pathToken, error) {
	var out []pathToken
	for rest := path; rest != ""; {
		m := pathTokenRE.FindStringSubmatch(rest)
		if m == nil {
			return nil, fmt.Errorf("%q is not a field path", path)
		}
		t := pathToken{field: m[1], key: m[2], index: -1}
		if m[3] != "" {
			t.index, _ = strconv.Atoi(m[3])
		}
		if t.field == "" && m[2] == "" && m[3] == "" {
			t.key = "" // ['']
		}
		out = append(out, t)
		rest = rest[len(m[0]):]
	}
	if len(out) == 0 || out[0].field == "" {
		return nil, fmt.Errorf("%q is not a field path", path)
	}
	return out, nil
}

// setPath puts value at path in doc, the template as JSON. A map's key
// may be new; a field of a message is made when absent; a list's index
// and a step must exist (check, which only checks the path, lets an
// index be out of range).
func setPath(doc map[string]any, path, value string, check bool) error {
	toks, err := parsePath(path)
	if err != nil {
		return err
	}
	var cur any = doc
	for i, t := range toks {
		last := i == len(toks)-1
		switch {
		case t.field != "":
			obj, ok := cur.(map[string]any)
			if !ok {
				return fmt.Errorf("%q: %s is not a field of a message", path, t.field)
			}
			key, found := lookup(obj, t.field)
			if last {
				obj[key] = value
				return nil
			}
			if !found || obj[key] == nil {
				if toks[i+1].index >= 0 {
					if check {
						return nil
					}
					return fmt.Errorf("%q: %s has no element %d", path, t.field, toks[i+1].index)
				}
				obj[key] = map[string]any{}
			}
			cur = obj[key]
		case t.index >= 0:
			list, ok := cur.([]any)
			if !ok || t.index >= len(list) {
				if check {
					return nil
				}
				return fmt.Errorf("%q: index %d is out of range", path, t.index)
			}
			if last {
				list[t.index] = value
				return nil
			}
			cur = list[t.index]
		default:
			if list, ok := cur.([]any); ok {
				// jobs['step-id']
				var step any
				for _, j := range list {
					if jm, ok := j.(map[string]any); ok && jm["stepId"] == t.key {
						step = jm
					}
				}
				if step == nil || last {
					return fmt.Errorf("%q: no step %q", path, t.key)
				}
				cur = step
				continue
			}
			m, ok := cur.(map[string]any)
			if !ok {
				return fmt.Errorf("%q: [%q] selects from no map", path, t.key)
			}
			if last {
				m[t.key] = value
				return nil
			}
			if m[t.key] == nil {
				m[t.key] = map[string]any{}
			}
			cur = m[t.key]
		}
	}
	return nil
}

func (s *Server) CreateWorkflowTemplate(_ context.Context, req *dataprocpb.CreateWorkflowTemplateRequest) (*dataprocpb.WorkflowTemplate, error) {
	m := regionRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/regions/{region} or projects/{project}/locations/{location}", req.GetParent())
	}
	if !validLocation(m[3]) {
		return nil, apierror.InvalidArgument("Region %q is not a Dataproc region", m[3])
	}
	if req.GetTemplate() == nil {
		return nil, apierror.InvalidArgument("template is required")
	}
	t := clone(req.GetTemplate())
	if t.GetVersion() != 0 {
		return nil, apierror.InvalidArgument("template.version must be left blank for a CreateWorkflowTemplate request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWorkflow(t, false); err != nil {
		return nil, err
	}
	t.Name = req.GetParent() + "/workflowTemplates/" + t.GetId()
	key := workflowKeyOf(t.Name)
	if _, ok := s.workflows[key]; ok {
		return nil, apierror.AlreadyExists("Already exists: Workflow template %s", t.Name)
	}
	now := timestamppb.New(s.now())
	t.Version, t.CreateTime, t.UpdateTime = 1, now, now
	s.workflows[key] = []*dataprocpb.WorkflowTemplate{t}
	s.save()
	return clone(t), nil
}

func (s *Server) workflowLocked(name string, version int32) (*dataprocpb.WorkflowTemplate, error) {
	key := workflowKeyOf(name)
	if !workflowRE.MatchString(key) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/regions/{region}/workflowTemplates/{template}", name)
	}
	versions, ok := s.workflows[key]
	if !ok || len(versions) == 0 {
		return nil, apierror.NotFound("Not found: Workflow template %s", name)
	}
	if version == 0 {
		return versions[len(versions)-1], nil
	}
	for _, v := range versions {
		if v.GetVersion() == version {
			return v, nil
		}
	}
	return nil, apierror.NotFound("Not found: Workflow template %s version %d", name, version)
}

func (s *Server) GetWorkflowTemplate(_ context.Context, req *dataprocpb.GetWorkflowTemplateRequest) (*dataprocpb.WorkflowTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.workflowLocked(req.GetName(), req.GetVersion())
	if err != nil {
		return nil, err
	}
	return clone(t), nil
}

// UpdateWorkflowTemplate: "The updated template must contain version
// that matches the current server version."
func (s *Server) UpdateWorkflowTemplate(_ context.Context, req *dataprocpb.UpdateWorkflowTemplateRequest) (*dataprocpb.WorkflowTemplate, error) {
	if req.GetTemplate() == nil {
		return nil, apierror.InvalidArgument("template is required")
	}
	t := clone(req.GetTemplate())
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.workflowLocked(t.GetName(), 0)
	if err != nil {
		return nil, err
	}
	if t.GetVersion() != cur.GetVersion() {
		return nil, apierror.FailedPrecondition("Template version %d does not match the current version %d", t.GetVersion(), cur.GetVersion())
	}
	if t.Id == "" {
		t.Id = cur.Id
	}
	if err := s.checkWorkflow(t, false); err != nil {
		return nil, err
	}
	t.Name, t.CreateTime, t.UpdateTime, t.Version = cur.Name, cur.CreateTime, timestamppb.New(s.now()), cur.Version+1
	key := workflowKeyOf(cur.Name)
	s.workflows[key] = append(s.workflows[key], t)
	s.save()
	return clone(t), nil
}

func (s *Server) ListWorkflowTemplates(_ context.Context, req *dataprocpb.ListWorkflowTemplatesRequest) (*dataprocpb.ListWorkflowTemplatesResponse, error) {
	m := regionRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/regions/{region} or projects/{project}/locations/{location}", req.GetParent())
	}
	prefix := "projects/" + m[1] + "/regions/" + m[3] + "/workflowTemplates/"
	s.mu.Lock()
	var all []*dataprocpb.WorkflowTemplate
	for _, k := range sortedKeys(s.workflows) {
		if v := s.workflows[k]; strings.HasPrefix(k, prefix) && len(v) > 0 {
			all = append(all, clone(v[len(v)-1]))
		}
	}
	s.mu.Unlock()
	out, next, err := aipfilter.Page("dataproc-workflows:"+prefix, all, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &dataprocpb.ListWorkflowTemplatesResponse{Templates: out, NextPageToken: next}, nil
}

// DeleteWorkflowTemplate: "Deletes a workflow template. It does not
// cancel in-progress workflows." A version given must be the current one.
func (s *Server) DeleteWorkflowTemplate(_ context.Context, req *dataprocpb.DeleteWorkflowTemplateRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.workflowLocked(req.GetName(), 0)
	if err != nil {
		return nil, err
	}
	if v := req.GetVersion(); v != 0 && v != cur.GetVersion() {
		return nil, apierror.FailedPrecondition("Template version %d does not match the current version %d", v, cur.GetVersion())
	}
	delete(s.workflows, workflowKeyOf(cur.Name))
	s.save()
	return &emptypb.Empty{}, nil
}

func (s *Server) InstantiateWorkflowTemplate(ctx context.Context, req *dataprocpb.InstantiateWorkflowTemplateRequest) (*longrunningpb.Operation, error) {
	if r := req.GetRequestId(); r != "" && !requestIDRE.MatchString(r) {
		return nil, apierror.InvalidArgument("request_id %q must be at most 40 letters, digits, underscores and hyphens", r)
	}
	s.mu.Lock()
	t, err := s.workflowLocked(req.GetName(), 0)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	// "If set, the workflow will be instantiated only if the current
	// version of the workflow template has the supplied version."
	if v := req.GetVersion(); v != 0 && v != t.GetVersion() {
		s.mu.Unlock()
		return nil, apierror.FailedPrecondition("Template version %d does not match the current version %d", v, t.GetVersion())
	}
	t = clone(t)
	s.mu.Unlock()
	return s.instantiate(t, req.GetParameters(), req.GetRequestId())
}

func (s *Server) InstantiateInlineWorkflowTemplate(ctx context.Context, req *dataprocpb.InstantiateInlineWorkflowTemplateRequest) (*longrunningpb.Operation, error) {
	m := regionRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/regions/{region} or projects/{project}/locations/{location}", req.GetParent())
	}
	if !validLocation(m[3]) {
		return nil, apierror.InvalidArgument("Region %q is not a Dataproc region", m[3])
	}
	if req.GetTemplate() == nil {
		return nil, apierror.InvalidArgument("template is required")
	}
	if r := req.GetRequestId(); r != "" && !requestIDRE.MatchString(r) {
		return nil, apierror.InvalidArgument("request_id %q must be at most 40 letters, digits, underscores and hyphens", r)
	}
	t := clone(req.GetTemplate())
	s.mu.Lock()
	err := s.checkWorkflow(t, true)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if len(t.GetParameters()) > 0 {
		return nil, apierror.InvalidArgument("An inline template cannot have parameters")
	}
	id := t.GetId()
	if id == "" {
		id = newUUID()
	}
	t.Name = "projects/" + m[1] + "/regions/" + m[3] + "/workflowTemplates/" + id
	return s.instantiate(t, nil, req.GetRequestId())
}

// instantiate starts a workflow of t with the parameters' values.
func (s *Server) instantiate(t *dataprocpb.WorkflowTemplate, params map[string]string, requestID string) (*longrunningpb.Operation, error) {
	for k, v := range params {
		found := false
		for _, p := range t.GetParameters() {
			if p.GetName() == k {
				found = true
				if err := validParam(p, v); err != nil {
					return nil, err
				}
			}
		}
		if !found {
			return nil, apierror.InvalidArgument("Parameter %q is not a parameter of the template", k)
		}
		// "Values may not exceed 1000 characters."
		if len(v) > 1000 {
			return nil, apierror.InvalidArgument("The value of parameter %q exceeds 1000 characters", k)
		}
	}
	for _, p := range t.GetParameters() {
		if _, ok := params[p.GetName()]; !ok {
			return nil, apierror.InvalidArgument("Parameter %q requires a value: values for parameters must be provided when the template is instantiated", p.GetName())
		}
	}
	run, err := substitute(t, params, false)
	if err != nil {
		return nil, err
	}
	m := workflowRE.FindStringSubmatch(workflowKeyOf(t.GetName()))
	project, region := m[1], m[3]
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, status.Error(codes.Unavailable, "the instance is stopping")
	}
	rkey := project + "/" + region + "|workflow|" + requestID
	if requestID != "" {
		if op, ok := s.ops[s.requests[rkey]]; ok {
			return clone(op), nil
		}
	}
	md := &dataprocpb.WorkflowMetadata{Template: t.GetName(), Version: t.GetVersion(), State: dataprocpb.WorkflowMetadata_PENDING,
		Parameters: params, StartTime: timestamppb.New(s.now()), DagTimeout: t.GetDagTimeout(), Graph: &dataprocpb.WorkflowGraph{}}
	for _, j := range run.GetJobs() {
		st := dataprocpb.WorkflowNode_BLOCKED
		if len(j.GetPrerequisiteStepIds()) == 0 {
			st = dataprocpb.WorkflowNode_RUNNABLE
		}
		md.Graph.Nodes = append(md.Graph.Nodes, &dataprocpb.WorkflowNode{StepId: j.GetStepId(), PrerequisiteStepIds: j.GetPrerequisiteStepIds(), State: st})
	}
	op := s.newOpLocked(opName(project, region), md)
	if requestID != "" {
		s.requests[rkey] = op.Name
	}
	s.save()
	s.wg.Add(1)
	go s.runWorkflow(op.Name, project, region, run)
	return clone(op), nil
}

func validParam(p *dataprocpb.TemplateParameter, v string) error {
	if rx := p.GetValidation().GetRegex(); rx != nil {
		for _, re := range rx.GetRegexes() {
			if r, err := regexp.Compile("^(?:" + re + ")$"); err == nil && r.MatchString(v) {
				return nil
			}
		}
		return apierror.InvalidArgument("The value %q of parameter %q matches none of its regexes", v, p.GetName())
	}
	if vals := p.GetValidation().GetValues(); vals != nil {
		for _, x := range vals.GetValues() {
			if x == v {
				return nil
			}
		}
		return apierror.InvalidArgument("The value %q of parameter %q is not one of its values", v, p.GetName())
	}
	return nil
}

// updateWorkflowLocked rewrites the operation's WorkflowMetadata with f.
func (s *Server) updateWorkflowLocked(opName string, f func(*dataprocpb.WorkflowMetadata)) {
	op := s.ops[opName]
	if op == nil {
		return
	}
	var md dataprocpb.WorkflowMetadata
	if op.GetMetadata().UnmarshalTo(&md) != nil {
		return
	}
	f(&md)
	op.Metadata, _ = anyOf(&md)
}

// runWorkflow runs the workflow: its cluster, its jobs in their order,
// then its managed cluster's deletion.
func (s *Server) runWorkflow(opName, project, region string, t *dataprocpb.WorkflowTemplate) {
	defer s.wg.Done()
	instance := newUUID()
	labels := map[string]string{}
	for k, v := range t.GetLabels() {
		labels[k] = v
	}
	tid := lastSegment(t.GetName())
	labels["goog-dataproc-workflow-template-id"], labels["goog-dataproc-workflow-instance-id"] = tid, instance
	fail := func(msg string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.updateWorkflowLocked(opName, func(md *dataprocpb.WorkflowMetadata) {
			md.State, md.EndTime = dataprocpb.WorkflowMetadata_DONE, timestamppb.New(s.now())
		})
		s.finishOpLocked(opName, nil, status.Error(codes.Unknown, msg))
		s.save()
	}
	var clusterName string
	managed := t.GetPlacement().GetManagedCluster()
	s.mu.Lock()
	s.updateWorkflowLocked(opName, func(md *dataprocpb.WorkflowMetadata) { md.State = dataprocpb.WorkflowMetadata_RUNNING })
	if managed != nil {
		c := &dataprocpb.Cluster{ProjectId: project, ClusterName: managed.GetClusterName() + "-" + randomSuffix(13), Config: clone(managed.GetConfig()), Labels: map[string]string{}}
		for k, v := range managed.GetLabels() {
			c.Labels[k] = v
		}
		for k, v := range labels {
			c.Labels[k] = v
		}
		if err := s.checkCluster(project, region, c); err != nil {
			s.mu.Unlock()
			fail("The workflow's managed cluster cannot be created: " + err.Error())
			return
		}
		cop, err := s.createClusterLocked(context.Background(), project, region, c, "", "")
		if err != nil {
			s.mu.Unlock()
			fail(err.Error())
			return
		}
		clusterName = c.ClusterName
		s.updateWorkflowLocked(opName, func(md *dataprocpb.WorkflowMetadata) {
			md.ClusterName, md.ClusterUuid = c.ClusterName, c.ClusterUuid
			md.CreateCluster = &dataprocpb.ClusterOperation{OperationId: cop.Name}
		})
		s.save()
		s.mu.Unlock()
		if err := s.waitCluster(project, region, clusterName, dataprocpb.ClusterStatus_RUNNING); err != nil {
			s.mu.Lock()
			s.updateWorkflowLocked(opName, func(md *dataprocpb.WorkflowMetadata) {
				md.CreateCluster.Done, md.CreateCluster.Error = true, err.Error()
			})
			s.mu.Unlock()
			s.endWorkflow(opName, project, region, clusterName, true, "The workflow's managed cluster failed: "+err.Error())
			return
		}
		s.mu.Lock()
		s.updateWorkflowLocked(opName, func(md *dataprocpb.WorkflowMetadata) { md.CreateCluster.Done = true })
		s.mu.Unlock()
	} else {
		c, err := s.placeLocked(project, region, &dataprocpb.JobPlacement{ClusterLabels: t.GetPlacement().GetClusterSelector().GetClusterLabels()})
		if err != nil {
			s.mu.Unlock()
			fail("No cluster matches the workflow's cluster selector: " + err.Error())
			return
		}
		clusterName = c.ClusterName
		s.updateWorkflowLocked(opName, func(md *dataprocpb.WorkflowMetadata) { md.ClusterName, md.ClusterUuid = c.ClusterName, c.ClusterUuid })
		s.save()
		s.mu.Unlock()
	}
	failed := s.runDAG(opName, project, region, clusterName, instance, labels, t)
	s.endWorkflow(opName, project, region, clusterName, managed != nil, failed)
}

// waitCluster waits for the cluster to reach st, or fail.
func (s *Server) waitCluster(project, region, name string, st dataprocpb.ClusterStatus_State) error {
	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		c := s.clusters[clusterKey(project, region, name)]
		var cur dataprocpb.ClusterStatus_State
		var detail string
		if c != nil {
			cur, detail = c.GetStatus().GetState(), c.GetStatus().GetDetail()
		}
		closed := s.closed
		s.mu.Unlock()
		switch {
		case c == nil:
			return fmt.Errorf("cluster %s was deleted", name)
		case cur == st:
			return nil
		case cur == dataprocpb.ClusterStatus_ERROR:
			return fmt.Errorf("cluster %s is in state ERROR: %s", name, detail)
		case closed:
			return fmt.Errorf("the instance is stopping")
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("cluster %s did not reach %s in 30 minutes", name, st)
}

// runDAG submits each step once its prerequisites are done, and returns
// the workflow's failure ("" when every step succeeded). A failed step
// fails the workflow: the steps still running are cancelled and the rest
// never run.
func (s *Server) runDAG(opName, project, region, cluster, instance string, labels map[string]string, t *dataprocpb.WorkflowTemplate) string {
	jobIDs := map[string]string{}
	state := map[string]dataprocpb.WorkflowNode_NodeState{}
	for _, j := range t.GetJobs() {
		state[j.GetStepId()] = dataprocpb.WorkflowNode_BLOCKED
	}
	var deadline time.Time
	if d := t.GetDagTimeout(); d != nil {
		deadline = s.now().Add(d.AsDuration())
		s.mu.Lock()
		s.updateWorkflowLocked(opName, func(md *dataprocpb.WorkflowMetadata) { md.DagStartTime = timestamppb.New(s.now()) })
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.updateWorkflowLocked(opName, func(md *dataprocpb.WorkflowMetadata) { md.DagEndTime = timestamppb.New(s.now()) })
			s.mu.Unlock()
		}()
	}
	failure := ""
	cancelAll := func() {
		for step, id := range jobIDs {
			if state[step] == dataprocpb.WorkflowNode_RUNNING {
				_, _ = s.CancelJob(context.Background(), &dataprocpb.CancelJobRequest{ProjectId: project, Region: region, JobId: id})
			}
		}
	}
	for {
		progress := false
		for _, j := range t.GetJobs() {
			step := j.GetStepId()
			if state[step] != dataprocpb.WorkflowNode_BLOCKED && state[step] != dataprocpb.WorkflowNode_RUNNABLE {
				continue
			}
			ready := true
			for _, p := range j.GetPrerequisiteStepIds() {
				ready = ready && state[p] == dataprocpb.WorkflowNode_COMPLETED
			}
			if !ready || failure != "" {
				continue
			}
			job := orderedJobType(j)
			job.Reference = &dataprocpb.JobReference{ProjectId: project, JobId: step + "-" + randomSuffix(13)}
			job.Placement = &dataprocpb.JobPlacement{ClusterName: cluster}
			job.Scheduling = j.GetScheduling()
			job.Labels = map[string]string{}
			for k, v := range labels {
				job.Labels[k] = v
			}
			for k, v := range j.GetLabels() {
				job.Labels[k] = v
			}
			job.Labels["goog-dataproc-workflow-step-id"] = step
			if _, err := s.SubmitJob(context.Background(), &dataprocpb.SubmitJobRequest{ProjectId: project, Region: region, Job: job}); err != nil {
				state[step] = dataprocpb.WorkflowNode_FAILED
				failure = fmt.Sprintf("Step %s could not be submitted: %v", step, err)
				s.setNode(opName, step, "", state[step], err.Error())
				cancelAll()
				continue
			}
			jobIDs[step] = job.Reference.JobId
			state[step] = dataprocpb.WorkflowNode_RUNNING
			s.setNode(opName, step, job.Reference.JobId, state[step], "")
			progress = true
		}
		running := 0
		for step, id := range jobIDs {
			if state[step] != dataprocpb.WorkflowNode_RUNNING {
				continue
			}
			s.mu.Lock()
			jb := s.jobs[jobKey(project, region, id)]
			var st dataprocpb.JobStatus_State
			var details string
			if jb != nil {
				st, details = jb.GetStatus().GetState(), jb.GetStatus().GetDetails()
			}
			s.mu.Unlock()
			switch {
			case jb == nil:
				state[step] = dataprocpb.WorkflowNode_FAILED
				details = "the job was deleted"
			case st == dataprocpb.JobStatus_DONE:
				state[step] = dataprocpb.WorkflowNode_COMPLETED
			case st == dataprocpb.JobStatus_ERROR || st == dataprocpb.JobStatus_CANCELLED:
				state[step] = dataprocpb.WorkflowNode_FAILED
				if details == "" {
					details = "Job " + strings.ToLower(st.String())
				}
			default:
				running++
				continue
			}
			progress = true
			s.setNode(opName, step, id, state[step], details)
			if state[step] == dataprocpb.WorkflowNode_FAILED && failure == "" {
				failure = fmt.Sprintf("Step %s (job %s) failed: %s", step, id, details)
				cancelAll()
			}
		}
		if !deadline.IsZero() && s.now().After(deadline) && failure == "" {
			failure = "The workflow ran past its dag_timeout: its remaining jobs were cancelled"
			cancelAll()
		}
		pending := 0
		for _, st := range state {
			if st == dataprocpb.WorkflowNode_BLOCKED || st == dataprocpb.WorkflowNode_RUNNABLE {
				pending++
			}
		}
		if running == 0 && (pending == 0 || failure != "") {
			return failure
		}
		s.mu.Lock()
		closed, cancelled := s.closed, s.wfCancel[opName]
		s.mu.Unlock()
		if closed {
			return "The instance stopped while the workflow ran"
		}
		if cancelled && failure == "" {
			failure = "The workflow was cancelled"
			cancelAll()
		}
		if !progress {
			time.Sleep(time.Second)
		}
	}
}

func (s *Server) setNode(opName, step, jobID string, st dataprocpb.WorkflowNode_NodeState, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateWorkflowLocked(opName, func(md *dataprocpb.WorkflowMetadata) {
		for _, n := range md.GetGraph().GetNodes() {
			if n.StepId == step {
				n.State, n.Error = st, errMsg
				if jobID != "" {
					n.JobId = jobID
				}
			}
		}
		// Steps whose prerequisites are done become RUNNABLE.
		done := map[string]bool{}
		for _, n := range md.GetGraph().GetNodes() {
			done[n.StepId] = n.State == dataprocpb.WorkflowNode_COMPLETED
		}
		for _, n := range md.GetGraph().GetNodes() {
			if n.State != dataprocpb.WorkflowNode_BLOCKED {
				continue
			}
			ready := true
			for _, p := range n.PrerequisiteStepIds {
				ready = ready && done[p]
			}
			if ready {
				n.State = dataprocpb.WorkflowNode_RUNNABLE
			}
		}
	})
	s.save()
}

// endWorkflow deletes the managed cluster, then ends the operation:
// Empty, or the workflow's failure.
func (s *Server) endWorkflow(opName, project, region, cluster string, managed bool, failure string) {
	if managed && cluster != "" {
		s.mu.Lock()
		c := s.clusters[clusterKey(project, region, cluster)]
		var dop *longrunningpb.Operation
		if c != nil && c.GetStatus().GetState() != dataprocpb.ClusterStatus_DELETING {
			dop = s.deleteClusterLocked(project, region, c)
			s.updateWorkflowLocked(opName, func(md *dataprocpb.WorkflowMetadata) {
				md.DeleteCluster = &dataprocpb.ClusterOperation{OperationId: dop.Name}
			})
		}
		s.save()
		s.mu.Unlock()
		if dop != nil {
			deadline := time.Now().Add(10 * time.Minute)
			for time.Now().Before(deadline) {
				s.mu.Lock()
				done := s.ops[dop.Name] == nil || s.ops[dop.Name].Done
				s.mu.Unlock()
				if done {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			s.mu.Lock()
			s.updateWorkflowLocked(opName, func(md *dataprocpb.WorkflowMetadata) { md.DeleteCluster.Done = true })
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateWorkflowLocked(opName, func(md *dataprocpb.WorkflowMetadata) {
		md.State, md.EndTime = dataprocpb.WorkflowMetadata_DONE, timestamppb.New(s.now())
	})
	if failure != "" {
		s.finishOpLocked(opName, nil, status.Error(codes.Unknown, failure))
	} else {
		s.finishOpLocked(opName, &emptypb.Empty{}, nil)
	}
	s.save()
}

// substitute returns t with values put at each parameter's fields
// ("jobs['step'].sparkJob.args[0]", "placement.managedCluster.clusterName",
// "jobs['step'].pysparkJob.properties['key']"...). check only checks
// that every field path reaches a field.
func substitute(t *dataprocpb.WorkflowTemplate, values map[string]string, check bool) (*dataprocpb.WorkflowTemplate, error) {
	if len(t.GetParameters()) == 0 {
		return clone(t), nil
	}
	raw, err := protojson.Marshal(t)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	for _, p := range t.GetParameters() {
		v, ok := values[p.GetName()]
		if check {
			v, ok = "x", true
		}
		if !ok {
			continue
		}
		for _, f := range p.GetFields() {
			if err := setPath(doc, f, v, check); err != nil {
				return nil, apierror.InvalidArgument("Parameter %q's field %q: %v", p.GetName(), f, err)
			}
		}
	}
	if check {
		return nil, nil
	}
	b, _ := json.Marshal(doc)
	var out dataprocpb.WorkflowTemplate
	if err := protojson.Unmarshal(b, &out); err != nil {
		return nil, apierror.InvalidArgument("The parameters' values do not fit their fields: %v", err)
	}
	return &out, nil
}

// lookup is m's key k, matched case-insensitively (the docs write
// pySparkJob for the pysparkJob field).
func lookup(m map[string]any, k string) (string, bool) {
	if _, ok := m[k]; ok {
		return k, true
	}
	for key := range m {
		if strings.EqualFold(key, k) {
			return key, true
		}
	}
	return k, false
}
