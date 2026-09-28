package main

// Cloud Run jobs and executions (#785).
//
// The adapter serves google.cloud.run.v2.Jobs and Executions (#582), and the
// console's Cloud Run screen listed services only, so a job could be created,
// run and cancelled from an SDK and from nothing a person could click. This is
// the Jobs page of the Cloud Run product: /run/jobs, a job's page with its
// executions and configuration, and an execution's page with its task counts,
// conditions and its pods' logs.
//
// Every read and write goes through the adapter's own Jobs and Executions API,
// the same endpoint an SDK dials, so the console cannot show a job the API
// would not return or accept a configuration the API refuses. Only the Logs
// tab reads the cluster: Cloud Run has no log method, and Google's console
// reads Cloud Logging for the same tab.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	runclient "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// runJobsProvider is the Jobs page of Cloud Run.
type runJobsProvider struct {
	// runAddr is the adapter's bound address, asked on each call as the
	// services screen asks it (#646).
	runAddr        func() string
	defaultProject string
	region         string
	// kubeconfig and namespace are where an execution's pods are read from,
	// for its Logs tab only.
	kubeconfig, namespace string
}

func (runJobsProvider) ID() string    { return "run-jobs" }
func (runJobsProvider) Title() string { return "Cloud Run jobs" }

// runJobNameImmutable is the job name field's help and the refusal a change
// to it gets.
const runJobNameImmutable = "A job name cannot be changed after it is created."

// runJobNotOffered names what this page leaves out, and why: each is
// something the adapter refuses or does not serve (docs/coverage/run.md), so
// a control for it could only fail.
const runJobNotOffered = "Not offered, because the Cloud Run adapter refuses or does not serve them: " +
	"permissions (the Jobs IAM methods return UNIMPLEMENTED), service accounts, VPC access, " +
	"volumes, binary authorization, and triggers."

// runTasksNotOffered is the execution page's account of its task list.
const runTasksNotOffered = "Not offered: google.cloud.run.v2.Tasks (GetTask, ListTasks) is not served " +
	"by this instance. The counts above are the execution's own."

func (p runJobsProvider) endpoint() string {
	if p.runAddr == nil {
		return ""
	}
	return p.runAddr()
}

func (p runJobsProvider) location() string {
	if p.region != "" {
		return p.region
	}
	return "us-central1"
}

func (p runJobsProvider) project(project string) string {
	if project == "" {
		return p.defaultProject
	}
	return project
}

func (p runJobsProvider) parent(project string) string {
	return fmt.Sprintf("projects/%s/locations/%s", project, p.location())
}

func (p runJobsProvider) jobName(project, job string) string {
	if strings.HasPrefix(job, "projects/") {
		return job
	}
	return p.parent(project) + "/jobs/" + job
}

func (p runJobsProvider) executionName(project, job, execution string) string {
	if strings.HasPrefix(execution, "projects/") {
		return execution
	}
	return p.jobName(project, job) + "/executions/" + execution
}

// clients dials the adapter as an SDK would.
func (p runJobsProvider) clients(ctx context.Context) (*runclient.JobsClient, *runclient.ExecutionsClient, error) {
	if p.endpoint() == "" {
		return nil, nil, errors.New("the Cloud Run adapter is not running")
	}
	opts := []option.ClientOption{
		option.WithEndpoint(p.endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	}
	jc, err := runclient.NewJobsClient(ctx, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to Cloud Run: %w", err)
	}
	xc, err := runclient.NewExecutionsClient(ctx, opts...)
	if err != nil {
		_ = jc.Close()
		return nil, nil, fmt.Errorf("connect to Cloud Run: %w", err)
	}
	return jc, xc, nil
}

// withClients runs fn with both clients and closes them after.
func (p runJobsProvider) withClients(ctx context.Context, fn func(*runclient.JobsClient, *runclient.ExecutionsClient) error) error {
	jc, xc, err := p.clients(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = jc.Close(); _ = xc.Close() }()
	return fn(jc, xc)
}

// apiMessage is the API's own words for a failure, with its code and without
// the gRPC envelope, as the console's handlers report one.
func apiMessage(err error) string {
	if st, ok := status.FromError(err); ok && st.Code() != codes.OK && st.Code() != codes.Unknown {
		return fmt.Sprintf("%s: %s", st.Code(), st.Message())
	}
	return err.Error()
}

// --- the jobs list ------------------------------------------------------

var runJobColumns = []string{"Last execution", "Last executed", "Executions", "Image", "Age"}

func (p runJobsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: runJobColumns, NameColumn: "Job", Noun: "jobs", AlwaysStatus: true}
	project = p.project(project)
	if project == "" {
		out.Prompt = "Choose a project to list its Cloud Run jobs."
		return out, nil
	}
	err := p.withClients(ctx, func(jc *runclient.JobsClient, _ *runclient.ExecutionsClient) error {
		it := jc.ListJobs(ctx, &runpb.ListJobsRequest{Parent: p.parent(project)})
		for {
			job, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out.Items = append(out.Items, jobRow(job))
		}
	})
	if err != nil {
		out.Unavailable = apiMessage(err)
		return out, nil
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	return out, nil
}

// jobRow is one row of the jobs list.
func jobRow(job *runpb.Job) console.Resource {
	status, last, when := "Not executed", "—", "—"
	if ref := job.GetLatestCreatedExecution(); ref != nil {
		status = completionWord(ref.GetCompletionStatus())
		last = lastSegment(ref.GetName())
		when = stampAge(ref.GetCreateTime())
	}
	return console.Resource{
		Name: lastSegment(job.GetName()), Status: status,
		Fields: map[string]string{
			"Last execution": last,
			"Last executed":  when,
			"Executions":     fmt.Sprint(job.GetExecutionCount()),
			"Image":          orDash(jobImage(job)),
			"Age":            stampAge(job.GetCreateTime()),
		},
		Actions: runJobActions(),
	}
}

func jobImage(job *runpb.Job) string {
	if cs := job.GetTemplate().GetTemplate().GetContainers(); len(cs) > 0 {
		return cs[0].GetImage()
	}
	return ""
}

// completionWord is an execution reference's status as the list shows it.
func completionWord(s runpb.ExecutionReference_CompletionStatus) string {
	switch s {
	case runpb.ExecutionReference_EXECUTION_SUCCEEDED:
		return "Succeeded"
	case runpb.ExecutionReference_EXECUTION_FAILED:
		return "Failed"
	case runpb.ExecutionReference_EXECUTION_RUNNING:
		return "Running"
	case runpb.ExecutionReference_EXECUTION_PENDING:
		return "Pending"
	case runpb.ExecutionReference_EXECUTION_CANCELLED:
		return "Cancelled"
	}
	return "Unknown"
}

// stampAge is a protobuf timestamp's age as the other lists show one.
func stampAge(ts *timestamppb.Timestamp) string {
	if ts == nil || !ts.IsValid() || ts.AsTime().IsZero() {
		return "—"
	}
	return orDash(shortAge(ts.AsTime().UTC().Format(time.RFC3339)))
}

// stampText is a protobuf timestamp as text, or "" when unset.
func stampText(ts *timestamppb.Timestamp) string {
	if ts == nil || !ts.IsValid() || ts.AsTime().IsZero() || ts.AsTime().Unix() <= 0 {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}

// --- actions --------------------------------------------------------------

func runJobActions() []console.Action {
	return []console.Action{{ID: "execute", Label: "Execute"}}
}

// Actions implements console.Actor: a job row's Execute.
func (runJobsProvider) Actions(console.Resource) []console.Action { return runJobActions() }

// Act implements console.Actor.
func (p runJobsProvider) Act(ctx context.Context, project, name, action string) error {
	if action != "execute" {
		return fmt.Errorf("unknown action %q", action)
	}
	_, err := p.execute(ctx, project, name)
	return err
}

// execute starts an execution and returns its name. It does not wait for it:
// an execution runs for as long as its tasks do, and its page is where it is
// followed.
func (p runJobsProvider) execute(ctx context.Context, project, job string) (string, error) {
	return p.executeWith(ctx, project, job, nil)
}

// executeWith is execute with RunJob's overrides, which apply to this
// execution only.
func (p runJobsProvider) executeWith(ctx context.Context, project, job string, o *runpb.RunJobRequest_Overrides) (string, error) {
	project = p.project(project)
	if project == "" {
		return "", errors.New("choose a project before executing a job")
	}
	var name string
	err := p.withClients(ctx, func(jc *runclient.JobsClient, _ *runclient.ExecutionsClient) error {
		op, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: p.jobName(project, job), Overrides: o})
		if err != nil {
			return err
		}
		if md, err := op.Metadata(); err == nil && md != nil {
			name = md.GetName()
		}
		return nil
	})
	return name, err
}

// executionActions is what an execution offers in its state: Cancel while it
// runs, Delete once it has finished. The API refuses a cancel of a finished
// execution, so the button is not drawn there.
func executionActions(e *runpb.Execution) []console.Action {
	if e.GetCompletionTime() == nil {
		if executionStatus(e) == "Cancelling" {
			return nil
		}
		return []console.Action{{ID: "cancel", Label: "Cancel"}}
	}
	return []console.Action{{ID: "delete", Label: "Delete", Destructive: true}}
}

// DetailActions implements console.PathActor: a job's Execute, and an
// execution's Cancel or Delete.
func (p runJobsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	switch len(path) {
	case 1:
		return append(runJobActions(), executeOverridesAction())
	case 2:
		var actions []console.Action
		_ = p.withClients(ctx, func(_ *runclient.JobsClient, xc *runclient.ExecutionsClient) error {
			e, err := xc.GetExecution(ctx, &runpb.GetExecutionRequest{Name: p.executionName(p.project(project), path[0], path[1])})
			if err != nil {
				return err
			}
			actions = executionActions(e)
			return nil
		})
		return actions
	}
	return nil
}

// ActAt implements console.PathActor.
func (p runJobsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	project = p.project(project)
	switch {
	case len(path) == 1 && action == "execute":
		_, err := p.execute(ctx, project, path[0])
		return err
	case len(path) == 1 && action == actExecuteOverrides:
		o, err := runJobOverrides(values)
		if err != nil {
			return err
		}
		_, err = p.executeWith(ctx, project, path[0], o)
		return err
	case len(path) == 2 && action == "cancel":
		return p.withClients(ctx, func(_ *runclient.JobsClient, xc *runclient.ExecutionsClient) error {
			// Not waited on. The cancel is applied when the API accepts it —
			// the execution's running tasks are being stopped — and the
			// operation completes only when the last pod has gone, which can
			// outlast the console's own budget. The page reads the execution
			// as Cancelling until then.
			_, err := xc.CancelExecution(ctx, &runpb.CancelExecutionRequest{Name: p.executionName(project, path[0], path[1])})
			return err
		})
	case len(path) == 2 && action == "delete":
		return p.withClients(ctx, func(_ *runclient.JobsClient, xc *runclient.ExecutionsClient) error {
			op, err := xc.DeleteExecution(ctx, &runpb.DeleteExecutionRequest{Name: p.executionName(project, path[0], path[1])})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		})
	}
	return fmt.Errorf("unknown action %q", action)
}

// Delete implements console.Deleter: DeleteJob, which removes the job and
// every execution of it.
func (p runJobsProvider) Delete(ctx context.Context, project, name string) error {
	project = p.project(project)
	if project == "" {
		return errors.New("choose a project before deleting a job")
	}
	return p.withClients(ctx, func(jc *runclient.JobsClient, _ *runclient.ExecutionsClient) error {
		op, err := jc.DeleteJob(ctx, &runpb.DeleteJobRequest{Name: p.jobName(project, name)})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

// --- create and edit ------------------------------------------------------

// CreateForm is every field the adapter maps from a job's template onto a
// batch Job, and nothing it refuses (a port, a probe, a service account,
// VPC access, volumes): those are named in the last field's help rather
// than drawn as controls that fail.
func (runJobsProvider) CreateForm() (string, []console.Field) {
	return "Create job", []console.Field{
		{
			Name: "name", Label: "Job name", Type: "text", Required: true,
			Help:    "Lowercase letters, numbers and hyphens, starting with a letter; at most 63 characters.",
			Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`,
			Section: "Job settings",
		},
		runLabelsField("Job settings"),
		{
			Name: "image", Label: "Container image URL", Type: "text", Required: true,
			Help: "A tagged image, such as docker.io/library/busybox:1.36. A locally built one " +
				"is rewritten to dev.local/ and never pulled; an untagged reference is refused.",
			Section: "Container",
		},
		{
			Name: "command", Label: "Container command", Type: "text",
			Help:    "Optional. Overrides the image's entrypoint. Quoted as a shell would: sh -c",
			Section: "Container",
		},
		{
			Name: "args", Label: "Container arguments", Type: "text",
			Help:    `Optional. Quoted as a shell would: 'echo "hello, world"'`,
			Section: "Container",
		},
		{
			Name: "env", Label: "Environment variables", Type: "map",
			Help:    "Optional. One KEY=value per line.",
			Section: "Container",
		},
		runSecretEnvField("Container"),
		{
			Name: "taskCount", Label: "Number of tasks", Type: "text", Default: "1",
			Help:    "How many tasks each execution runs. Each task gets its own CLOUD_RUN_TASK_INDEX.",
			Pattern: `^[0-9]{1,5}$`,
			Section: "Tasks",
		},
		{
			Name: "parallelism", Label: "Parallelism", Type: "text",
			Help:    "Optional. How many tasks run at once; blank runs as many as there are.",
			Pattern: `^[0-9]{1,5}$`,
			Section: "Tasks",
		},
		{
			Name: "maxRetries", Label: "Maximum retries per failed task", Type: "text", Default: "3",
			Pattern: `^[0-9]{1,2}$`,
			Section: "Tasks",
		},
		{
			Name: "timeout", Label: "Task timeout (seconds)", Type: "text", Default: "600",
			Help:    "How long one attempt of a task may run before it is stopped.",
			Pattern: `^[0-9]{1,6}$`,
			Section: "Tasks",
		},
		{
			Name: "cpu", Label: "CPU limit", Type: "text",
			Help:    `Optional, as Kubernetes quantities: "1", "500m".`,
			Pattern: `^[0-9]+(\.[0-9]+)?m?$`,
			Section: "Resources",
		},
		{
			Name: "memory", Label: "Memory limit", Type: "text",
			Help:    `Optional, as Kubernetes quantities: "512Mi", "1Gi". ` + runJobNotOffered,
			Pattern: `^[0-9]+(Ki|Mi|Gi|K|M|G)?$`,
			Section: "Resources",
		},
	}
}

// CreateOnPage implements console.PageCreator: thirteen fields in four groups.
func (runJobsProvider) CreateOnPage() bool { return true }

// Create implements console.Creator through CreateJob. The operation
// completes at once: a job is configuration, and runs nothing until it is
// executed.
func (p runJobsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	project = p.project(project)
	if project == "" {
		return "", errors.New("choose a project before creating a job")
	}
	id := strings.TrimSpace(values["name"])
	if id == "" {
		return "", errors.New("a job name is required")
	}
	et, err := runJobFormTemplate(values)
	if err != nil {
		return "", err
	}
	labels, _, err := runLabels(values)
	if err != nil {
		return "", err
	}
	var name string
	err = p.withClients(ctx, func(jc *runclient.JobsClient, _ *runclient.ExecutionsClient) error {
		op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{
			Parent: p.parent(project), JobId: id, Job: &runpb.Job{Labels: labels, Template: et},
		})
		if err != nil {
			return err
		}
		job, err := op.Wait(ctx)
		if err != nil {
			return err
		}
		name = job.GetName()
		return nil
	})
	return name, err
}

// runJobFormTemplate builds the execution template the form describes.
// Shared by Create and Edit, so the two cannot map a field differently.
func runJobFormTemplate(values map[string]string) (*runpb.ExecutionTemplate, error) {
	image := strings.TrimSpace(values["image"])
	if image == "" {
		return nil, errors.New("a container image is required")
	}
	c := &runpb.Container{Image: image}
	var err error
	if c.Command, err = shellSplit(values["command"]); err != nil {
		return nil, fmt.Errorf("container command: %w", err)
	}
	if c.Args, err = shellSplit(values["args"]); err != nil {
		return nil, fmt.Errorf("container arguments: %w", err)
	}
	env, err := console.ParseMap(values["env"])
	if err != nil {
		return nil, fmt.Errorf("environment variables: %w", err)
	}
	for _, k := range sortedKeys(env) {
		c.Env = append(c.Env, &runpb.EnvVar{Name: k, Values: &runpb.EnvVar_Value{Value: env[k]}})
	}
	if err := withSecretEnv(c, values["secretEnv"]); err != nil {
		return nil, err
	}
	limits := map[string]string{}
	if v := strings.TrimSpace(values["cpu"]); v != "" {
		limits["cpu"] = v
	}
	if v := strings.TrimSpace(values["memory"]); v != "" {
		limits["memory"] = v
	}
	if len(limits) > 0 {
		c.Resources = &runpb.ResourceRequirements{Limits: limits}
	}

	tasks, err := optionalInt(values["taskCount"], "number of tasks")
	if err != nil {
		return nil, err
	}
	if tasks == 0 {
		tasks = 1
	}
	parallelism, err := optionalInt(values["parallelism"], "parallelism")
	if err != nil {
		return nil, err
	}
	tt := &runpb.TaskTemplate{Containers: []*runpb.Container{c}}
	if v := strings.TrimSpace(values["maxRetries"]); v != "" {
		n, err := optionalInt(v, "maximum retries")
		if err != nil {
			return nil, err
		}
		tt.Retries = &runpb.TaskTemplate_MaxRetries{MaxRetries: int32(n)}
	}
	timeout, err := optionalInt(values["timeout"], "task timeout")
	if err != nil {
		return nil, err
	}
	if timeout > 0 {
		tt.Timeout = durationpb.New(time.Duration(timeout) * time.Second)
	}
	return &runpb.ExecutionTemplate{TaskCount: int32(tasks), Parallelism: int32(parallelism), Template: tt}, nil
}

// Edit implements console.Editor through UpdateJob. The job is read through
// the API first and only the form's fields are replaced on it, because
// UpdateJob replaces the whole configuration: annotations and a working
// directory, which the form does not show, are kept.
func (p runJobsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a job can be edited: an execution runs the configuration it was started with")
	}
	if v, ok := values["name"]; ok && strings.TrimSpace(v) != path[0] {
		return errors.New(runJobNameImmutable)
	}
	project = p.project(project)
	if project == "" {
		return errors.New("choose a project before editing a job")
	}
	form, err := runJobFormTemplate(values)
	if err != nil {
		return err
	}
	return p.withClients(ctx, func(jc *runclient.JobsClient, _ *runclient.ExecutionsClient) error {
		job, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: p.jobName(project, path[0])})
		if err != nil {
			return err
		}
		_, sentSecrets := values["secretEnv"]
		if err := applyRunJobForm(job, form, !sentSecrets); err != nil {
			return err
		}
		if labels, ok, err := runLabels(values); err != nil {
			return err
		} else if ok {
			job.Labels = labels
		}
		op, err := jc.UpdateJob(ctx, &runpb.UpdateJobRequest{Job: job})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

// applyRunJobForm replaces the form's fields on a job read from the API.
// keepSecrets is a request without the secretEnv field, whose secret-backed
// variables are kept.
func applyRunJobForm(job *runpb.Job, form *runpb.ExecutionTemplate, keepSecrets bool) error {
	et := job.GetTemplate()
	tt := et.GetTemplate()
	if len(tt.GetContainers()) != 1 {
		return fmt.Errorf("this job runs %d containers, and the form edits exactly one", len(tt.GetContainers()))
	}
	cur, next := tt.Containers[0], form.Template.Containers[0]
	cur.Image, cur.Command, cur.Args = next.GetImage(), next.GetCommand(), next.GetArgs()
	if cur.Resources == nil {
		cur.Resources = &runpb.ResourceRequirements{}
	}
	if cur.Resources.Limits == nil {
		cur.Resources.Limits = map[string]string{}
	}
	for _, k := range []string{"cpu", "memory"} {
		if v, ok := next.GetResources().GetLimits()[k]; ok {
			cur.Resources.Limits[k] = v
		} else {
			delete(cur.Resources.Limits, k)
		}
	}
	// The variables are the form's, plain and secret-backed: both are on it,
	// prefilled, so one removed there is removed.
	if err := formEnv(cur, next.GetEnv(), keepSecrets, "change it through the Cloud Run API"); err != nil {
		return err
	}

	et.TaskCount, et.Parallelism = form.GetTaskCount(), form.GetParallelism()
	if form.Template.Retries != nil {
		tt.Retries = form.Template.Retries
	}
	if form.Template.Timeout != nil {
		tt.Timeout = form.Template.Timeout
	}
	return nil
}

// runJobEditForm is the create form prefilled from the job, with the name
// shown and refused.
func runJobEditForm(job *runpb.Job) *console.EditForm {
	tt := job.GetTemplate().GetTemplate()
	if len(tt.GetContainers()) != 1 {
		return nil
	}
	c := tt.Containers[0]
	env := map[string]string{}
	for _, e := range c.GetEnv() {
		if e.GetValues() != nil && e.GetValueSource() == nil {
			env[e.GetName()] = e.GetValue()
		}
	}
	secretEnv, unknown := secretEnvLines(c.GetEnv())
	prefill := map[string]string{
		"name": lastSegment(job.GetName()), "image": c.GetImage(),
		"command": shellJoin(c.GetCommand()), "args": shellJoin(c.GetArgs()),
		"env":        console.FormatMap(env),
		"secretEnv":  secretEnv,
		"labels":     console.FormatMap(job.GetLabels()),
		"taskCount":  fmt.Sprint(job.GetTemplate().GetTaskCount()),
		"maxRetries": fmt.Sprint(tt.GetMaxRetries()),
		"cpu":        c.GetResources().GetLimits()["cpu"],
		"memory":     c.GetResources().GetLimits()["memory"],
	}
	if n := job.GetTemplate().GetParallelism(); n > 0 {
		prefill["parallelism"] = fmt.Sprint(n)
	}
	if t := tt.GetTimeout(); t != nil {
		prefill["timeout"] = fmt.Sprint(int64(t.AsDuration().Seconds()))
	}
	_, fields := runJobsProvider{}.CreateForm()
	for i := range fields {
		fields[i].Default = prefill[fields[i].Name]
		if fields[i].Name == "name" {
			fields[i].Immutable = true
			fields[i].Help = runJobNameImmutable
		}
	}
	note := "Saved with UpdateJob. The next execution runs the new configuration; executions " +
		"already started keep the one they were started with."
	if len(unknown) > 0 {
		note += " Variables with neither a value nor a secret (" + strings.Join(unknown, ", ") +
			") cannot be shown, so the form cannot be saved while they are there."
	}
	return &console.EditForm{Label: "Edit job", Fields: fields, Note: note}
}

// --- detail pages -------------------------------------------------------

// Detail implements console.Driller: a job, or one of its executions.
func (p runJobsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	project = p.project(project)
	if project == "" {
		return console.Detail{Prompt: "Choose a project to open its Cloud Run jobs."}, nil
	}
	var d console.Detail
	err := p.withClients(ctx, func(jc *runclient.JobsClient, xc *runclient.ExecutionsClient) error {
		if len(path) == 2 {
			e, err := xc.GetExecution(ctx, &runpb.GetExecutionRequest{Name: p.executionName(project, path[0], path[1])})
			if err != nil {
				return err
			}
			d = p.executionDetail(ctx, e)
			return nil
		}
		job, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: p.jobName(project, path[0])})
		if err != nil {
			return err
		}
		d = runJobDetail(job, p.executions(ctx, xc, job.GetName()))
		return nil
	})
	if err != nil {
		return console.Detail{Unavailable: apiMessage(err)}, nil
	}
	return d, nil
}

func runJobDetail(job *runpb.Job, executions console.Listing) console.Detail {
	status, latest := "Not executed", ""
	if ref := job.GetLatestCreatedExecution(); ref != nil {
		status, latest = completionWord(ref.GetCompletionStatus()), lastSegment(ref.GetName())
	}
	summary := stored(
		console.Property{Label: "Status", Value: status},
		console.Property{Label: "Region", Value: regionOf(job.GetName())},
		console.Property{Label: "Latest execution", Value: unsetAs(latest, "None: the job has not been executed")},
		console.Property{Label: "Executions", Value: fmt.Sprint(job.GetExecutionCount())},
		console.Property{Label: "Generation", Value: fmt.Sprint(job.GetGeneration())},
		console.Property{Label: "Created", Value: stampText(job.GetCreateTime())},
		console.Property{Label: "Updated", Value: stampText(job.GetUpdateTime())},
	)
	return console.Detail{
		Summary: summary,
		Sections: []console.Section{
			{ID: "executions", Label: "Executions", Listing: executions},
			runJobConfiguration(job),
		},
		Edit: runJobEditForm(job),
	}
}

// regionOf reads the location out of a resource name.
func regionOf(name string) string {
	parts := strings.Split(name, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "locations" {
			return parts[i+1]
		}
	}
	return ""
}

// executions is a job's Executions tab, newest first as the API lists them.
func (p runJobsProvider) executions(ctx context.Context, xc *runclient.ExecutionsClient, job string) console.Listing {
	out := console.Listing{
		Columns:    []string{"Tasks", "Running", "Succeeded", "Failed", "Cancelled", "Created", "Finished"},
		NameColumn: "Execution", Noun: "executions", AlwaysStatus: true, RowsOpenable: true,
	}
	it := xc.ListExecutions(ctx, &runpb.ListExecutionsRequest{Parent: job})
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			out.Unavailable = apiMessage(err)
			return out
		}
		out.Items = append(out.Items, console.Resource{
			Name: lastSegment(e.GetName()), Status: executionStatus(e),
			Fields: map[string]string{
				"Tasks":     fmt.Sprintf("%d/%d completed", e.GetSucceededCount()+e.GetFailedCount()+e.GetCancelledCount(), e.GetTaskCount()),
				"Running":   fmt.Sprint(e.GetRunningCount()),
				"Succeeded": fmt.Sprint(e.GetSucceededCount()),
				"Failed":    fmt.Sprint(e.GetFailedCount()),
				"Cancelled": fmt.Sprint(e.GetCancelledCount()),
				"Created":   stampAge(e.GetCreateTime()),
				"Finished":  stampAge(e.GetCompletionTime()),
			},
			Actions: executionActions(e),
		})
	}
	if len(out.Items) == 0 {
		out.Note = "This job has not been executed. Execute starts one execution of its tasks."
	}
	out.Total = len(out.Items)
	return out
}

// completedCondition is an execution's Completed condition, which carries
// its outcome and, for a failure, the failing task's own message.
func completedCondition(e *runpb.Execution) *runpb.Condition {
	for _, c := range e.GetConditions() {
		if c.GetType() == "Completed" {
			return c
		}
	}
	return nil
}

// executionStatus is one word for where an execution is.
func executionStatus(e *runpb.Execution) string {
	c := completedCondition(e)
	switch {
	case c.GetState() == runpb.Condition_CONDITION_SUCCEEDED:
		return "Succeeded"
	case c.GetState() == runpb.Condition_CONDITION_FAILED && c.GetExecutionReason() == runpb.Condition_CANCELLED:
		return "Cancelled"
	case c.GetState() == runpb.Condition_CONDITION_FAILED:
		return "Failed"
	case c.GetExecutionReason() == runpb.Condition_CANCELLING:
		return "Cancelling"
	case e.GetRunningCount() > 0:
		return "Running"
	}
	return "Pending"
}

// conditionState is a condition's state in the words the table uses.
func conditionState(s runpb.Condition_State) string {
	switch s {
	case runpb.Condition_CONDITION_SUCCEEDED:
		return "Succeeded"
	case runpb.Condition_CONDITION_FAILED:
		return "Failed"
	case runpb.Condition_CONDITION_PENDING:
		return "Pending"
	case runpb.Condition_CONDITION_RECONCILING:
		return "Reconciling"
	}
	return "Unknown"
}

func (p runJobsProvider) executionDetail(ctx context.Context, e *runpb.Execution) console.Detail {
	status := executionStatus(e)
	summary := stored(
		console.Property{Label: "Status", Value: status},
		console.Property{Label: "Job", Value: lastSegment(e.GetJob())},
		console.Property{Label: "Tasks", Value: fmt.Sprint(e.GetTaskCount())},
		console.Property{Label: "Parallelism", Value: fmt.Sprint(e.GetParallelism())},
		console.Property{Label: "Running", Value: fmt.Sprint(e.GetRunningCount())},
		console.Property{Label: "Succeeded", Value: fmt.Sprint(e.GetSucceededCount())},
		console.Property{Label: "Failed", Value: fmt.Sprint(e.GetFailedCount())},
		console.Property{Label: "Cancelled", Value: fmt.Sprint(e.GetCancelledCount())},
		console.Property{Label: "Retried", Value: fmt.Sprint(e.GetRetriedCount())},
		console.Property{Label: "Created", Value: stampText(e.GetCreateTime())},
		console.Property{Label: "Started", Value: unsetAs(stampText(e.GetStartTime()), "Not started yet")},
		console.Property{Label: "Finished", Value: unsetAs(stampText(e.GetCompletionTime()), "Not finished")},
	)
	if c := completedCondition(e); c.GetState() == runpb.Condition_CONDITION_FAILED && c.GetMessage() != "" {
		summary = append(summary, console.Property{Label: "Failure", Value: c.GetMessage()})
	}
	summary = append(summary, console.Property{Label: "Task list", Value: runTasksNotOffered})

	conditions := console.Listing{
		Columns: []string{"State", "Reason", "Message", "Changed"}, NameColumn: "Condition",
		Noun: "conditions", AlwaysStatus: true,
	}
	for _, c := range e.GetConditions() {
		reason := ""
		if r := c.GetExecutionReason(); r != runpb.Condition_EXECUTION_REASON_UNDEFINED {
			reason = r.String()
		} else if r := c.GetReason(); r != runpb.Condition_COMMON_REASON_UNDEFINED {
			reason = r.String()
		}
		conditions.Items = append(conditions.Items, console.Resource{
			Name: c.GetType(), Status: conditionState(c.GetState()),
			Fields: map[string]string{
				"State": conditionState(c.GetState()), "Reason": orDash(reason),
				"Message": orDash(c.GetMessage()), "Changed": orDash(stampText(c.GetLastTransitionTime())),
			},
		})
	}
	conditions.Total = len(conditions.Items)

	return console.Detail{
		Summary: summary,
		Sections: []console.Section{
			{ID: "conditions", Label: "Conditions", Listing: conditions},
			executionConfiguration(e),
			p.executionLogs(ctx, e),
		},
	}
}

// runJobConfiguration is a job's settings, grouped as the form groups them.
func runJobConfiguration(job *runpb.Job) console.Section {
	et := job.GetTemplate()
	tasks := stored(
		console.Property{Label: "Number of tasks", Value: fmt.Sprint(et.GetTaskCount())},
		console.Property{Label: "Parallelism", Value: parallelismText(et.GetParallelism())},
		console.Property{Label: "Maximum retries per failed task", Value: fmt.Sprint(et.GetTemplate().GetMaxRetries())},
		console.Property{Label: "Task timeout", Value: timeoutText(et.GetTemplate().GetTimeout())},
	)
	groups := []console.PropertyGroup{{Heading: "Tasks", Properties: tasks}}
	groups = append(groups, containerGroups(et.GetTemplate().GetContainers())...)
	if len(job.GetLabels()) > 0 {
		groups = append(groups, console.PropertyGroup{Heading: "Labels", Properties: labelPairs(job.GetLabels())})
	}
	return console.Section{
		ID: "configuration", Label: "Configuration", Kind: console.KindProperties, Groups: groups,
		Note: "Changed with Edit job, which saves the whole configuration through UpdateJob. " + runJobNotOffered,
	}
}

// executionConfiguration is what one execution ran with, which is the job's
// configuration when it was started, not necessarily now.
func executionConfiguration(e *runpb.Execution) console.Section {
	tasks := stored(
		console.Property{Label: "Number of tasks", Value: fmt.Sprint(e.GetTaskCount())},
		console.Property{Label: "Parallelism", Value: fmt.Sprint(e.GetParallelism())},
		console.Property{Label: "Maximum retries per failed task", Value: fmt.Sprint(e.GetTemplate().GetMaxRetries())},
		console.Property{Label: "Task timeout", Value: timeoutText(e.GetTemplate().GetTimeout())},
	)
	groups := []console.PropertyGroup{{Heading: "Tasks", Properties: tasks}}
	groups = append(groups, containerGroups(e.GetTemplate().GetContainers())...)
	return console.Section{
		ID: "configuration", Label: "Configuration", Kind: console.KindProperties, Groups: groups,
		Note: "The configuration this execution was started with. Editing the job changes the next execution, not this one.",
	}
}

func parallelismText(n int32) string {
	if n <= 0 {
		return "All tasks at once"
	}
	return fmt.Sprint(n)
}

func timeoutText(d *durationpb.Duration) string {
	if d == nil {
		return "Cloud Run's default"
	}
	return fmt.Sprintf("%d seconds", int64(d.AsDuration().Seconds()))
}

func labelPairs(m map[string]string) []console.Property {
	var out []console.Property
	for _, k := range sortedKeys(m) {
		out = append(out, console.Property{Label: k, Value: unsetAs(m[k], "(empty value)")})
	}
	return out
}

// containerGroups is one group per container, as the service page draws them.
func containerGroups(containers []*runpb.Container) []console.PropertyGroup {
	var groups []console.PropertyGroup
	for _, c := range containers {
		heading := "Container"
		if c.GetName() != "" {
			heading += " " + c.GetName()
		}
		props := stored(
			console.Property{Label: "Image", Value: c.GetImage()},
			console.Property{Label: "Command", Value: shellJoin(c.GetCommand())},
			console.Property{Label: "Arguments", Value: shellJoin(c.GetArgs())},
			console.Property{Label: "Working directory", Value: c.GetWorkingDir()},
		)
		for _, pair := range sortedPairs(c.GetResources().GetLimits()) {
			props = append(props, console.Property{Label: "Limit " + pair.Label, Value: pair.Value})
		}
		for _, e := range c.GetEnv() {
			if src := e.GetValueSource().GetSecretKeyRef(); src != nil {
				props = append(props, console.Property{Label: "Env " + e.GetName(),
					Value: "from secret " + src.GetSecret() + " version " + unsetAs(src.GetVersion(), "latest")})
				continue
			}
			props = append(props, console.Property{Label: "Env " + e.GetName(), Value: unsetAs(e.GetValue(), "(empty value)")})
		}
		groups = append(groups, console.PropertyGroup{Heading: heading, Properties: props})
	}
	return groups
}

// --- the Logs tab -----------------------------------------------------------

// runLogTail is how many lines of each task's pod the Logs tab shows.
const runLogTail = 200

// executionLogs is the execution's Logs tab: each of its pods' output, read
// with kubectl logs, newest pod first.
//
// Read from the cluster, because the Cloud Run API has no log method: Google's
// console reads Cloud Logging for this tab. The console's own log follower
// does not serve here, since a task that finishes between two of its sweeps
// is never followed.
func (p runJobsProvider) executionLogs(ctx context.Context, e *runpb.Execution) console.Section {
	sec := console.Section{ID: "logs", Label: "Logs", Kind: console.KindText,
		Note: fmt.Sprintf("The last %d lines of each task's pod, read from the cluster with kubectl logs.", runLogTail)}
	execID := lastSegment(e.GetName())
	raw, err := kubectlSelected(ctx, p.kubeconfig, p.namespace, "pods", "batch.kubernetes.io/job-name="+execID)
	if err != nil {
		sec.Unavailable = "cannot read the execution's pods: " + err.Error()
		return sec
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				CreationTimestamp string            `json:"creationTimestamp"`
				Annotations       map[string]string `json:"annotations"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		sec.Unavailable = "decode the execution's pods: " + err.Error()
		return sec
	}
	if len(list.Items) == 0 {
		sec.Text = "No pods: the execution's tasks have not started, or their pods were removed with it."
		return sec
	}
	sort.SliceStable(list.Items, func(i, j int) bool {
		return list.Items[i].Metadata.CreationTimestamp > list.Items[j].Metadata.CreationTimestamp
	})
	var b strings.Builder
	for _, pod := range list.Items {
		task := unsetAs(pod.Metadata.Annotations["batch.kubernetes.io/job-completion-index"], "?")
		fmt.Fprintf(&b, "── Task %s · pod %s · %s ──\n", task, pod.Metadata.Name, unsetAs(pod.Status.Phase, "Unknown"))
		out, err := kubeRunner(p.kubeconfig, "").Do(ctx, "", "-n", p.namespace, "logs", pod.Metadata.Name,
			"--all-containers", "--tail", fmt.Sprint(runLogTail))
		switch {
		case err != nil:
			fmt.Fprintf(&b, "(logs unavailable: %v)\n", kubectlCause(err))
		case strings.TrimSpace(out) == "":
			b.WriteString("(no output)\n")
		default:
			b.WriteString(strings.TrimRight(out, "\n") + "\n")
		}
		b.WriteString("\n")
	}
	sec.Text = strings.TrimRight(b.String(), "\n")
	return sec
}

// --- quoting ----------------------------------------------------------------

// shellSplit splits a command line the way a POSIX shell splits words: on
// unquoted whitespace, with '…' taken literally, "…" allowing \" and \\, and
// a backslash escaping the next character outside quotes. A job's arguments
// are routinely a script (sh -c 'echo "$X"'), which a split on spaces alone
// cannot hold.
func shellSplit(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	quote := rune(0)
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch {
			case r == '"':
				quote = 0
			case r == '\\' && i+1 < len(runes) && (runes[i+1] == '"' || runes[i+1] == '\\'):
				i++
				cur.WriteRune(runes[i])
			default:
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == '\\':
			if i+1 >= len(runes) {
				return nil, errors.New("a trailing backslash escapes nothing")
			}
			i++
			cur.WriteRune(runes[i])
			inWord = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

// shellJoin is the inverse of shellSplit: each word bare when it needs no
// quoting, else in single quotes.
func shellJoin(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		if w != "" && strings.Trim(w, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
			out[i] = w
			continue
		}
		out[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

var (
	_ console.Driller     = runJobsProvider{}
	_ console.PageCreator = runJobsProvider{}
	_ console.Deleter     = runJobsProvider{}
	_ console.Editor      = runJobsProvider{}
	_ console.Actor       = runJobsProvider{}
	_ console.PathActor   = runJobsProvider{}
)
