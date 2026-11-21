//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	scheduler "cloud.google.com/go/scheduler/apiv1"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	workflows "cloud.google.com/go/workflows/apiv1"
	"cloud.google.com/go/workflows/apiv1/workflowspb"
	executions "cloud.google.com/go/workflows/executions/apiv1"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
	"github.com/googleapis/gax-go/v2/apierror"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/api/workflowexecutions/v1"
)

// ownerTokenAs are gRPC client options for endpoint calling with an access
// token of the instance's own account (the ADC fixture's), which is the
// project owner.
func ownerTokenAs(t *testing.T, endpoint string) []option.ClientOption {
	t.Helper()
	_, source := instanceAccount(t)
	tok, err := source.Token()
	if err != nil {
		t.Fatalf("the instance account's token: %v", err)
	}
	return grpcAs(endpoint, tok.AccessToken)
}

// TestIAMEnforcementScheduler (#1879), with the official Cloud Scheduler
// client over gRPC and REST and a token minted through IAM Credentials:
// without a role, CreateJob, GetJob and RunJob are PERMISSION_DENIED with
// Cloud Scheduler's message (Cloud Tasks' wording, no ErrorInfo); Cloud
// Scheduler Viewer on the project lets GetJob and ListJobs through but not
// RunJob or PauseJob; Job Runner adds RunJob; the instance's own account
// (no token, and its own minted token) does everything.
func TestIAMEnforcementScheduler(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvScheduler)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iesc-")
	parent := "projects/" + project + "/locations/us-central1"
	owner, err := scheduler.NewCloudSchedulerClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	newJob := func(c *scheduler.CloudSchedulerClient, id string) (*schedulerpb.Job, error) {
		return c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
			Name: parent + "/jobs/" + id, Schedule: "0 9 * * 1",
			Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: "http://127.0.0.1:1/never", HttpMethod: schedulerpb.HttpMethod_POST}}}})
	}
	job, err := newJob(owner, "enforced")
	if err != nil {
		t.Fatalf("CreateJob as the owner: %v", err)
	}
	t.Cleanup(func() { _ = owner.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: job.GetName()}) })

	p := newIAMPrincipal(t, h, project, "iesc")
	as, err := scheduler.NewCloudSchedulerClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	denied := func(perm, resource string) string {
		return `The principal (user or service account) lacks IAM permission "` + perm + `" for the resource "` + resource + `" (or the resource may not exist).`
	}
	_, err = newJob(as, "by-principal")
	wantDenied(t, "CreateJob without a role", err, denied("cloudscheduler.jobs.create", parent), "cloudscheduler.jobs.create", false)
	get := func() error {
		_, err := as.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job.GetName()})
		return err
	}
	wantDenied(t, "GetJob without a role", get(), denied("cloudscheduler.jobs.get", job.GetName()), "cloudscheduler.jobs.get", false)
	run := func() error {
		_, err := as.RunJob(ctx, &schedulerpb.RunJobRequest{Name: job.GetName()})
		return err
	}
	wantDenied(t, "RunJob without a role", run(), denied("cloudscheduler.jobs.run", job.GetName()), "cloudscheduler.jobs.run", false)

	// Over JSON, the same denial in Google's error envelope.
	rc, err := scheduler.NewCloudSchedulerRESTClient(ctx, option.WithEndpoint("http://"+strings.TrimPrefix(endpoint, "http://")),
		option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token})}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	_, err = rc.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job.GetName()})
	var ae *apierror.APIError
	if !errors.As(err, &ae) || ae.HTTPCode() != http.StatusForbidden || !strings.Contains(err.Error(), denied("cloudscheduler.jobs.get", job.GetName())) {
		t.Fatalf("GetJob over JSON without a role = %v; want 403 with Cloud Scheduler's message", err)
	}

	p.grant("roles/cloudscheduler.viewer")
	t.Cleanup(func() { p.revoke("roles/cloudscheduler.viewer") })
	waitGranted(t, "GetJob as a Cloud Scheduler Viewer", get)
	waitGranted(t, "GetJob over JSON as a Cloud Scheduler Viewer", func() error {
		_, err := rc.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job.GetName()})
		return err
	})
	if _, err := as.ListJobs(ctx, &schedulerpb.ListJobsRequest{Parent: parent}).Next(); err != nil {
		t.Fatalf("ListJobs as a Cloud Scheduler Viewer: %v", err)
	}
	wantDenied(t, "RunJob as a Cloud Scheduler Viewer", run(), denied("cloudscheduler.jobs.run", job.GetName()), "cloudscheduler.jobs.run", false)
	_, err = as.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: job.GetName()})
	wantDenied(t, "PauseJob as a Cloud Scheduler Viewer", err, denied("cloudscheduler.jobs.pause", job.GetName()), "cloudscheduler.jobs.pause", false)

	p.grant("roles/cloudscheduler.jobRunner")
	t.Cleanup(func() { p.revoke("roles/cloudscheduler.jobRunner") })
	waitGranted(t, "RunJob as a Cloud Scheduler Job Runner", run)
	_, err = as.ResumeJob(ctx, &schedulerpb.ResumeJobRequest{Name: job.GetName()})
	wantDenied(t, "ResumeJob as a Job Runner", err, denied("cloudscheduler.jobs.enable", job.GetName()), "cloudscheduler.jobs.enable", false)

	// The instance's own account, with its own token, is the owner.
	mine, err := scheduler.NewCloudSchedulerClient(ctx, ownerTokenAs(t, endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mine.Close() })
	if _, err := mine.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: job.GetName()}); err != nil {
		t.Fatalf("PauseJob as the instance's account: %v", err)
	}
	if _, err := mine.ResumeJob(ctx, &schedulerpb.ResumeJobRequest{Name: job.GetName()}); err != nil {
		t.Fatalf("ResumeJob as the instance's account: %v", err)
	}
}

// TestIAMEnforcementWorkflows (#1879), with the official Workflows and
// Workflow Executions clients over gRPC, the Workflow Executions REST
// client and a minted token: without a role, GetWorkflow and
// CreateExecution are PERMISSION_DENIED with "Permission 'P' denied on
// resource 'R' (or it may not exist)." and ErrorInfo
// IAM_PERMISSION_DENIED; Workflows Invoker on the project runs an
// execution and reads it, but cannot read the workflow or deploy one;
// step entries over JSON are 403 until Workflows Viewer; the instance's
// own account (no token, and its own minted token) deploys and runs.
func TestIAMEnforcementWorkflows(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvWorkflows)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iewf-")
	parent := "projects/" + project + "/locations/us-central1"
	owner, err := workflows.NewClient(ctx, ownerTokenAs(t, endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	src := "main:\n  steps:\n    - done:\n        return: 42\n"
	deploy := func(c *workflows.Client, id string) (*workflowspb.Workflow, error) {
		op, err := c.CreateWorkflow(ctx, &workflowspb.CreateWorkflowRequest{Parent: parent, WorkflowId: id,
			Workflow: &workflowspb.Workflow{SourceCode: &workflowspb.Workflow_SourceContents{SourceContents: src}}})
		if err != nil {
			return nil, err
		}
		return op.Wait(ctx)
	}
	wf, err := deploy(owner, "enforced")
	if err != nil {
		t.Fatalf("CreateWorkflow as the instance's account: %v", err)
	}
	t.Cleanup(func() {
		if op, err := owner.DeleteWorkflow(context.Background(), &workflowspb.DeleteWorkflowRequest{Name: wf.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	exOwner, err := executions.NewClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exOwner.Close() })
	if _, err := exOwner.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: wf.GetName()}); err != nil {
		t.Fatalf("CreateExecution with no token: %v", err)
	}

	p := newIAMPrincipal(t, h, project, "iewf")
	wc, err := workflows.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Close() })
	ec, err := executions.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	denied := func(perm, resource string) string {
		return "Permission '" + perm + "' denied on resource '" + resource + "' (or it may not exist)."
	}
	_, err = wc.GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: wf.GetName()})
	wantDenied(t, "GetWorkflow without a role", err, denied("workflows.workflows.get", wf.GetName()), "workflows.workflows.get", true)
	var ex *executionspb.Execution
	execute := func() error {
		var err error
		ex, err = ec.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: wf.GetName()})
		return err
	}
	wantDenied(t, "CreateExecution without a role", execute(), denied("workflows.executions.create", wf.GetName()), "workflows.executions.create", true)

	p.grant("roles/workflows.invoker")
	t.Cleanup(func() { p.revoke("roles/workflows.invoker") })
	waitGranted(t, "CreateExecution as a Workflows Invoker", execute)
	if _, err := ec.GetExecution(ctx, &executionspb.GetExecutionRequest{Name: ex.GetName()}); err != nil {
		t.Fatalf("GetExecution as a Workflows Invoker: %v", err)
	}
	_, err = wc.GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: wf.GetName()})
	wantDenied(t, "GetWorkflow as a Workflows Invoker", err, denied("workflows.workflows.get", wf.GetName()), "workflows.workflows.get", true)
	_, err = deploy(wc, "by-invoker")
	wantDenied(t, "CreateWorkflow as a Workflows Invoker", err, denied("workflows.workflows.create", parent), "workflows.workflows.create", true)

	// Step entries are served over JSON alone: with the Invoker role taken
	// back, listing them is refused until Workflows Viewer is granted.
	rest, err := workflowexecutions.NewService(ctx, option.WithEndpoint("http://"+strings.TrimPrefix(endpoint, "http://")+"/"),
		option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token})}}))
	if err != nil {
		t.Fatal(err)
	}
	p.revoke("roles/workflows.invoker")
	steps := func() error {
		_, err := rest.Projects.Locations.Workflows.Executions.StepEntries.List(ex.GetName()).Context(ctx).Do()
		return err
	}
	if err := steps(); err == nil || !strings.Contains(err.Error(), "403") ||
		!strings.Contains(err.Error(), denied("workflows.stepEntries.list", ex.GetName())) {
		t.Fatalf("stepEntries.list without a role = %v; want 403 %q", err, denied("workflows.stepEntries.list", ex.GetName()))
	}
	p.grant("roles/workflows.viewer")
	t.Cleanup(func() { p.revoke("roles/workflows.viewer") })
	waitGranted(t, "stepEntries.list as a Workflows Viewer", steps)
	waitGranted(t, "GetWorkflow as a Workflows Viewer", func() error {
		_, err := wc.GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: wf.GetName()})
		return err
	})
	_, err = ec.CancelExecution(ctx, &executionspb.CancelExecutionRequest{Name: ex.GetName()})
	wantDenied(t, "CancelExecution as a Workflows Viewer", err, denied("workflows.executions.cancel", ex.GetName()), "workflows.executions.cancel", true)
}
