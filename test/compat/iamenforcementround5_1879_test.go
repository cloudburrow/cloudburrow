//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	eventarc "cloud.google.com/go/eventarc/apiv1"
	"cloud.google.com/go/eventarc/apiv1/eventarcpb"
	functionsv1 "cloud.google.com/go/functions/apiv1"
	functionsv1pb "cloud.google.com/go/functions/apiv1/functionspb"
	functions "cloud.google.com/go/functions/apiv2"
	"cloud.google.com/go/functions/apiv2/functionspb"
	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	memorystore "cloud.google.com/go/memorystore/apiv1"
	"cloud.google.com/go/memorystore/apiv1/memorystorepb"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	redis "cloud.google.com/go/redis/apiv1"
	"cloud.google.com/go/redis/apiv1/redispb"
	cluster "cloud.google.com/go/redis/cluster/apiv1"
	"cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	scheduler "cloud.google.com/go/scheduler/apiv1"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	workflows "cloud.google.com/go/workflows/apiv1"
	"cloud.google.com/go/workflows/apiv1/workflowspb"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	logrest "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newerDenied is the denial of Google's newer APIs, "Permission 'P'
// denied on resource 'R' (or it may not exist).".
func newerDenied(perm, resource string) string {
	return "Permission '" + perm + "' denied on resource '" + resource + "' (or it may not exist)."
}

// wantNotDenied fails unless err is nil or a code other than
// PERMISSION_DENIED: the call passed its IAM check.
func wantNotDenied(t *testing.T, what string, err error) {
	t.Helper()
	for i := 0; i < 10 && status.Code(err) == codes.PermissionDenied; i++ {
		time.Sleep(200 * time.Millisecond)
	}
	if status.Code(err) == codes.PermissionDenied {
		t.Fatalf("%s = %v; want it past its IAM check", what, err)
	}
}

// TestIAMEnforcementActAs (#1879), with the official Cloud Scheduler,
// Workflows and Cloud Tasks clients and minted tokens: a principal with
// each service's admin or enqueuer role, but no
// iam.serviceAccounts.actAs on a service account, cannot create a
// Scheduler job whose OIDC token is the account's, deploy a workflow that
// runs as it, or enqueue a task whose OIDC token is the account's, each
// refused with the service's own wording on
// projects/P/serviceAccounts/E; Service Account User on the account (its
// own policy) lets all three through; the instance's own account needs no
// grant.
func TestIAMEnforcementActAs(t *testing.T) {
	h := New(t)
	schedEndpoint := h.Endpoint(EnvScheduler)
	wfEndpoint := h.Endpoint(EnvWorkflows)
	tasksEndpoint := h.Endpoint(EnvTasks)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ieaa-")
	parent := "projects/" + project + "/locations/us-central1"
	target := newServiceAccount(t, h, project, "runas")
	saResource := "projects/" + project + "/serviceAccounts/" + target.GetEmail()
	p := newIAMPrincipal(t, h, project, "ieaa", "roles/cloudscheduler.admin", "roles/workflows.editor", "roles/cloudtasks.enqueuer")

	sc, err := scheduler.NewCloudSchedulerClient(ctx, grpcAs(schedEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	owner, err := scheduler.NewCloudSchedulerClient(ctx, grpcAs(schedEndpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	n := 0
	job := func(c *scheduler.CloudSchedulerClient) error {
		n++
		j, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
			Name: fmt.Sprintf("%s/jobs/actas-%d", parent, n), Schedule: "0 9 * * 1",
			Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: "http://127.0.0.1:1/never",
				AuthorizationHeader: &schedulerpb.HttpTarget_OidcToken{OidcToken: &schedulerpb.OidcToken{ServiceAccountEmail: target.GetEmail()}}}}}})
		if err == nil {
			t.Cleanup(func() { _ = owner.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: j.GetName()}) })
		}
		return err
	}
	wantDenied(t, "CreateJob without actAs", job(sc),
		`The principal (user or service account) lacks IAM permission "iam.serviceAccounts.actAs" for the resource "`+saResource+`" (or the resource may not exist).`,
		"iam.serviceAccounts.actAs", false)
	if err := job(owner); err != nil {
		t.Fatalf("CreateJob as the instance's account: %v", err)
	}

	wc, err := workflows.NewClient(ctx, grpcAs(wfEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Close() })
	deploy := func() error {
		n++
		op, err := wc.CreateWorkflow(ctx, &workflowspb.CreateWorkflowRequest{Parent: parent, WorkflowId: fmt.Sprintf("actas-%d", n),
			Workflow: &workflowspb.Workflow{ServiceAccount: target.GetEmail(),
				SourceCode: &workflowspb.Workflow_SourceContents{SourceContents: "main:\n  steps:\n    - done:\n        return: 1\n"}}})
		if err != nil {
			return err
		}
		wf, err := op.Wait(ctx)
		if err == nil {
			t.Cleanup(func() {
				if op, err := wc.DeleteWorkflow(context.Background(), &workflowspb.DeleteWorkflowRequest{Name: wf.GetName()}); err == nil {
					_ = op.Wait(context.Background())
				}
			})
		}
		return err
	}
	wantDenied(t, "CreateWorkflow without actAs", deploy(), newerDenied("iam.serviceAccounts.actAs", saResource), "iam.serviceAccounts.actAs", true)

	tasksOwner, err := cloudtasks.NewClient(ctx, grpcAs(tasksEndpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tasksOwner.Close() })
	q, err := tasksOwner.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{Parent: parent, Queue: &cloudtaskspb.Queue{Name: parent + "/queues/actas"}})
	if err != nil {
		t.Fatalf("CreateQueue as the owner: %v", err)
	}
	t.Cleanup(func() {
		_ = tasksOwner.DeleteQueue(context.Background(), &cloudtaskspb.DeleteQueueRequest{Name: q.GetName()})
	})
	_, _ = tasksOwner.PauseQueue(ctx, &cloudtaskspb.PauseQueueRequest{Name: q.GetName()})
	tc, err := cloudtasks.NewClient(ctx, grpcAs(tasksEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	enqueue := func() error {
		_, err := tc.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{Parent: q.GetName(), Task: &cloudtaskspb.Task{
			MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{Url: "http://127.0.0.1:1/never",
				AuthorizationHeader: &cloudtaskspb.HttpRequest_OidcToken{OidcToken: &cloudtaskspb.OidcToken{ServiceAccountEmail: target.GetEmail()}}}}}})
		return err
	}
	wantDenied(t, "CreateTask without actAs", enqueue(),
		`The principal (user or service account) lacks IAM permission "iam.serviceAccounts.actAs" for the resource "`+saResource+`" (or the resource may not exist).`,
		"iam.serviceAccounts.actAs", false)

	grantOn(t, h, target.GetName(), "roles/iam.serviceAccountUser", "serviceAccount:"+p.email)
	waitGranted(t, "CreateJob with actAs", func() error { return job(sc) })
	waitGranted(t, "CreateWorkflow with actAs", deploy)
	waitGranted(t, "CreateTask with actAs", enqueue)
}

// TestIAMEnforcementRunActAs (#1879), with the official Cloud Run v2,
// Cloud Functions v2 and v1 and Eventarc clients: a Cloud Run service, a
// function (either generation) or an Eventarc trigger whose service
// account the caller may not act as is refused, Cloud Run and Cloud
// Functions with their troubleshooting pages' wording, before anything is
// deployed; with Service
// Account User on the account the same requests pass the check (and fail
// on their deliberately invalid IDs, so nothing is deployed).
func TestIAMEnforcementRunActAs(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvRun)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iera-")
	parent := "projects/" + project + "/locations/us-central1"
	target := newServiceAccount(t, h, project, "runid")
	p := newIAMPrincipal(t, h, project, "iera", "roles/run.admin", "roles/cloudfunctions.admin", "roles/eventarc.admin")
	saResource := "projects/" + project + "/serviceAccounts/" + target.GetEmail()
	rc, err := run.NewServicesClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	fc, err := functions.NewFunctionClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fc.Close() })
	service := func() error {
		_, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: "Not_A_Valid_ID", Service: &runpb.Service{
			Template: &runpb.RevisionTemplate{ServiceAccount: target.GetEmail(),
				Containers: []*runpb.Container{{Image: "127.0.0.1:1/none/never:latest"}}}}})
		return err
	}
	function := func() error {
		_, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: "Not_A_Valid_ID", Function: &functionspb.Function{
			ServiceConfig: &functionspb.ServiceConfig{ServiceAccountEmail: target.GetEmail()}}})
		return err
	}
	ec, err := eventarc.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	trigger := func() error {
		_, err := ec.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: parent, TriggerId: "Not_A_Valid_ID", Trigger: &eventarcpb.Trigger{
			ServiceAccount: target.GetEmail(),
			EventFilters:   []*eventarcpb.EventFilter{{Attribute: "type", Value: "google.cloud.pubsub.topic.v1.messagePublished"}},
			Destination:    &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudRun{CloudRun: &eventarcpb.CloudRun{Service: "never", Region: "us-central1"}}}}})
		return err
	}
	f1, err := functionsv1.NewCloudFunctionsClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f1.Close() })
	gen1 := func() error {
		_, err := f1.CreateFunction(ctx, &functionsv1pb.CreateFunctionRequest{Location: parent, Function: &functionsv1pb.CloudFunction{
			Name: parent + "/functions/Not_A_Valid_ID", ServiceAccountEmail: target.GetEmail()}})
		return err
	}
	wantDenied(t, "CreateTrigger without actAs", trigger(), newerDenied("iam.serviceAccounts.actAs", saResource), "iam.serviceAccounts.actAs", true)
	err = gen1()
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied ||
		st.Message() != "Caller is missing permission 'iam.serviceaccounts.actAs' on service account projects/-/serviceAccounts/"+target.GetEmail()+"." {
		t.Fatalf("1st gen CreateFunction without actAs = %v", err)
	}
	wantDenied(t, "CreateService without actAs", service(),
		"Permission 'iam.serviceaccounts.actAs' denied on service account "+target.GetEmail(), "iam.serviceAccounts.actAs", true)
	err = function()
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied ||
		st.Message() != "Caller is missing permission 'iam.serviceaccounts.actAs' on service account projects/-/serviceAccounts/"+target.GetEmail()+"." {
		t.Fatalf("CreateFunction without actAs = %v", err)
	}
	grantOn(t, h, target.GetName(), "roles/iam.serviceAccountUser", "serviceAccount:"+p.email)
	wantNotDenied(t, "CreateService with actAs", service())
	wantNotDenied(t, "CreateFunction with actAs", function())
	wantNotDenied(t, "CreateTrigger with actAs", trigger())
	wantNotDenied(t, "1st gen CreateFunction with actAs", gen1())
	owner, err := run.NewServicesClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	_, err = owner.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: "Not_A_Valid_ID", Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{ServiceAccount: target.GetEmail(), Containers: []*runpb.Container{{Image: "127.0.0.1:1/none/never:latest"}}}}})
	wantNotDenied(t, "CreateService as the instance's account", err)
}

// TestIAMEnforcementLogging (#1879), with the official Cloud Logging
// clients over gRPC and the REST client: without a role, ListLogEntries,
// WriteLogEntries and ListSinks are PERMISSION_DENIED (over JSON too);
// Logs Viewer lets the reads through but not a write or CreateSink; Logs
// Writer adds the write; the instance's own account does everything.
func TestIAMEnforcementLogging(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvLogging)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ielg-")
	pr := "projects/" + project
	logName := pr + "/logs/enforced"
	p := newIAMPrincipal(t, h, project, "ielg")
	lc, err := logging.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lc.Close() })
	cc, err := logging.NewConfigClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	write := func(c *logging.Client) error {
		_, err := c.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: logName,
			Resource: &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}},
			Entries:  []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "enforced"}}}})
		return err
	}
	list := func() error {
		_, err := lc.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{pr}}).Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		return err
	}
	sinks := func() error {
		_, err := cc.ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: pr}).Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		return err
	}
	denied := func(perm string) string { return "Permission '" + perm + "' denied on resource (or it may not exist)." }
	wantDenied(t, "WriteLogEntries without a role", write(lc), denied("logging.logEntries.create"), "logging.logEntries.create", true)
	wantDenied(t, "ListLogEntries without a role", list(), denied("logging.logEntries.list"), "logging.logEntries.list", true)
	wantDenied(t, "ListSinks without a role", sinks(), denied("logging.sinks.list"), "logging.sinks.list", true)
	rest, err := logrest.NewService(ctx, option.WithEndpoint("http://"+strings.TrimPrefix(endpoint, "http://")+"/"),
		option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token})}}))
	if err != nil {
		t.Fatal(err)
	}
	restList := func() error {
		_, err := rest.Entries.List(&logrest.ListLogEntriesRequest{ResourceNames: []string{pr}}).Context(ctx).Do()
		return err
	}
	if err := restList(); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), denied("logging.logEntries.list")) {
		t.Fatalf("entries.list over JSON without a role = %v; want 403", err)
	}

	p.grant("roles/logging.viewer")
	t.Cleanup(func() { p.revoke("roles/logging.viewer") })
	waitGranted(t, "ListLogEntries as a Logs Viewer", list)
	waitGranted(t, "entries.list over JSON as a Logs Viewer", restList)
	waitGranted(t, "ListSinks as a Logs Viewer", sinks)
	wantDenied(t, "WriteLogEntries as a Logs Viewer", write(lc), denied("logging.logEntries.create"), "logging.logEntries.create", true)
	_, err = cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: pr, Sink: &loggingpb.LogSink{Name: "enforced",
		Destination: "storage.googleapis.com/never-" + project}})
	wantDenied(t, "CreateSink as a Logs Viewer", err, denied("logging.sinks.create"), "logging.sinks.create", true)
	p.grant("roles/logging.logWriter")
	t.Cleanup(func() { p.revoke("roles/logging.logWriter") })
	waitGranted(t, "WriteLogEntries as a Logs Writer", func() error { return write(lc) })

	mine, err := logging.NewClient(ctx, ownerTokenAs(t, endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mine.Close() })
	if err := write(mine); err != nil {
		t.Fatalf("WriteLogEntries as the instance's account: %v", err)
	}
}

// TestIAMEnforcementMonitoring (#1879), with the official Cloud
// Monitoring clients: without a role, GetGroup and ListAlertPolicies are
// PERMISSION_DENIED; Monitoring Viewer lets them through but not
// DeleteGroup; the instance's own account creates and deletes.
func TestIAMEnforcementMonitoring(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvMonitoring)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iemo-")
	pr := "projects/" + project
	owner, err := monitoring.NewGroupClient(ctx, ownerTokenAs(t, endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	g, err := owner.CreateGroup(ctx, &monitoringpb.CreateGroupRequest{Name: pr, Group: &monitoringpb.Group{DisplayName: "enforced",
		Filter: `resource.type = "gce_instance"`}})
	if err != nil {
		t.Fatalf("CreateGroup as the instance's account: %v", err)
	}
	p := newIAMPrincipal(t, h, project, "iemo")
	gc, err := monitoring.NewGroupClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gc.Close() })
	ac, err := monitoring.NewAlertPolicyClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	get := func() error {
		_, err := gc.GetGroup(ctx, &monitoringpb.GetGroupRequest{Name: g.GetName()})
		return err
	}
	alerts := func() error {
		_, err := ac.ListAlertPolicies(ctx, &monitoringpb.ListAlertPoliciesRequest{Name: pr}).Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		return err
	}
	wantDenied(t, "GetGroup without a role", get(), newerDenied("monitoring.groups.get", g.GetName()), "monitoring.groups.get", true)
	wantDenied(t, "ListAlertPolicies without a role", alerts(), newerDenied("monitoring.alertPolicies.list", pr), "monitoring.alertPolicies.list", true)
	p.grant("roles/monitoring.viewer")
	t.Cleanup(func() { p.revoke("roles/monitoring.viewer") })
	waitGranted(t, "GetGroup as a Monitoring Viewer", get)
	waitGranted(t, "ListAlertPolicies as a Monitoring Viewer", alerts)
	err = gc.DeleteGroup(ctx, &monitoringpb.DeleteGroupRequest{Name: g.GetName()})
	wantDenied(t, "DeleteGroup as a Monitoring Viewer", err, newerDenied("monitoring.groups.delete", g.GetName()), "monitoring.groups.delete", true)
	if err := owner.DeleteGroup(ctx, &monitoringpb.DeleteGroupRequest{Name: g.GetName()}); err != nil {
		t.Fatalf("DeleteGroup as the instance's account: %v", err)
	}
}

// TestIAMEnforcementMemorystore (#1879), with the official Memorystore
// for Redis, Redis Cluster and Valkey clients on Memorystore's one port:
// without a role, each API's list is PERMISSION_DENIED with its own
// permission; Redis Viewer lets Redis' and Redis Cluster's lists and
// reads through (a missing instance is then NOT_FOUND) but not
// CreateInstance, nor Valkey's list, which Memorystore Viewer adds; the
// instance's own account lists all three.
func TestIAMEnforcementMemorystore(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvMemorystoreAdmin)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iems-")
	parent := "projects/" + project + "/locations/us-central1"
	p := newIAMPrincipal(t, h, project, "iems")
	clients := func(token string) (*redis.CloudRedisClient, *cluster.CloudRedisClusterClient, *memorystore.Client) {
		rc, err := redis.NewCloudRedisClient(ctx, grpcAs(endpoint, token)...)
		if err != nil {
			t.Fatal(err)
		}
		cc, err := cluster.NewCloudRedisClusterClient(ctx, grpcAs(endpoint, token)...)
		if err != nil {
			t.Fatal(err)
		}
		vc, err := memorystore.NewClient(ctx, grpcAs(endpoint, token)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rc.Close(); _ = cc.Close(); _ = vc.Close() })
		return rc, cc, vc
	}
	done := func(err error) error {
		if errors.Is(err, iterator.Done) {
			return nil
		}
		return err
	}
	rc, cc, vc := clients(p.token)
	redisList := func() error {
		_, err := rc.ListInstances(ctx, &redispb.ListInstancesRequest{Parent: parent}).Next()
		return done(err)
	}
	clusterList := func() error {
		_, err := cc.ListClusters(ctx, &clusterpb.ListClustersRequest{Parent: parent}).Next()
		return done(err)
	}
	valkeyList := func() error {
		_, err := vc.ListInstances(ctx, &memorystorepb.ListInstancesRequest{Parent: parent}).Next()
		return done(err)
	}
	wantDenied(t, "Redis ListInstances without a role", redisList(), newerDenied("redis.instances.list", parent), "redis.instances.list", true)
	wantDenied(t, "ListClusters without a role", clusterList(), newerDenied("redis.clusters.list", parent), "redis.clusters.list", true)
	wantDenied(t, "Valkey ListInstances without a role", valkeyList(), newerDenied("memorystore.instances.list", parent), "memorystore.instances.list", true)

	p.grant("roles/redis.viewer")
	t.Cleanup(func() { p.revoke("roles/redis.viewer") })
	waitGranted(t, "Redis ListInstances as a Redis Viewer", redisList)
	waitGranted(t, "ListClusters as a Redis Viewer", clusterList)
	_, err := rc.GetInstance(ctx, &redispb.GetInstanceRequest{Name: parent + "/instances/missing"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetInstance of a missing instance as a Redis Viewer = %v; want NOT_FOUND", err)
	}
	_, err = rc.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "enforced",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}})
	wantDenied(t, "Redis CreateInstance as a Redis Viewer", err, newerDenied("redis.instances.create", parent), "redis.instances.create", true)
	wantDenied(t, "Valkey ListInstances as a Redis Viewer", valkeyList(), newerDenied("memorystore.instances.list", parent), "memorystore.instances.list", true)
	p.grant("roles/memorystore.viewer")
	t.Cleanup(func() { p.revoke("roles/memorystore.viewer") })
	waitGranted(t, "Valkey ListInstances as a Memorystore Viewer", valkeyList)

	orc, occ, ovc := clients("")
	if _, err := orc.ListInstances(ctx, &redispb.ListInstancesRequest{Parent: parent}).Next(); done(err) != nil {
		t.Fatalf("Redis ListInstances as the instance's account: %v", err)
	}
	if _, err := occ.ListClusters(ctx, &clusterpb.ListClustersRequest{Parent: parent}).Next(); done(err) != nil {
		t.Fatalf("ListClusters as the instance's account: %v", err)
	}
	if _, err := ovc.ListInstances(ctx, &memorystorepb.ListInstancesRequest{Parent: parent}).Next(); done(err) != nil {
		t.Fatalf("Valkey ListInstances as the instance's account: %v", err)
	}
}

// TestIAMEnforcementCloudSQL (#1879), with the official
// google.golang.org/api/sqladmin/v1 client and minted tokens: without a
// role, instances.list is 403 notAuthorized with Cloud SQL's message;
// Cloud SQL Viewer on the project lists, and reads a missing instance and
// its databases (404, past the check), but cannot restart it; Cloud SQL
// Editor can (404); the instance's own account lists.
func TestIAMEnforcementCloudSQL(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvSQLAdmin)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iesq-")
	p := newIAMPrincipal(t, h, project, "iesq")
	as := func(token string) *sqladmin.Service {
		opts := []option.ClientOption{option.WithEndpoint("http://" + strings.TrimPrefix(endpoint, "http://") + "/")}
		if token == "" {
			opts = append(opts, option.WithoutAuthentication())
		} else {
			opts = append(opts, option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})}}))
		}
		svc, err := sqladmin.NewService(ctx, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	svc := as(p.token)
	code := func(err error) int {
		var ge *googleapi.Error
		if errors.As(err, &ge) {
			return ge.Code
		}
		if err != nil {
			return -1
		}
		return 200
	}
	wantForbidden := func(what string, err error) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusForbidden || ge.Message != "The client isn't authorized to make this request." ||
			len(ge.Errors) != 1 || ge.Errors[0].Reason != "notAuthorized" {
			t.Fatalf("%s = %v; want 403 notAuthorized", what, err)
		}
	}
	list := func() error { _, err := svc.Instances.List(project).Context(ctx).Do(); return err }
	wantForbidden("instances.list without a role", list())
	restart := func() error { _, err := svc.Instances.Restart(project, "missing").Context(ctx).Do(); return err }
	wantForbidden("instances.restart without a role", restart())

	p.grant("roles/cloudsql.viewer")
	t.Cleanup(func() { p.revoke("roles/cloudsql.viewer") })
	waitGranted(t, "instances.list as a Cloud SQL Viewer", list)
	if _, err := svc.Instances.Get(project, "missing").Context(ctx).Do(); code(err) != http.StatusNotFound {
		t.Fatalf("instances.get of a missing instance as a Cloud SQL Viewer = %v; want 404", err)
	}
	if _, err := svc.Databases.List(project, "missing").Context(ctx).Do(); code(err) == http.StatusForbidden || code(err) == -1 {
		t.Fatalf("databases.list as a Cloud SQL Viewer = %v; want it past the check", err)
	}
	wantForbidden("instances.restart as a Cloud SQL Viewer", restart())
	p.grant("roles/cloudsql.editor")
	t.Cleanup(func() { p.revoke("roles/cloudsql.editor") })
	for i := 0; i < 10 && code(restart()) == http.StatusForbidden; i++ {
		time.Sleep(200 * time.Millisecond)
	}
	if err := restart(); code(err) == http.StatusForbidden || code(err) == -1 {
		t.Fatalf("instances.restart as a Cloud SQL Editor = %v; want it past the check", err)
	}
	if _, err := as("").Instances.List(project).Context(ctx).Do(); err != nil {
		t.Fatalf("instances.list as the instance's account: %v", err)
	}
}
