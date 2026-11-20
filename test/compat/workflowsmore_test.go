//go:build compat

package compat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/logging/logadmin"
	scheduler "cloud.google.com/go/scheduler/apiv1"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/workflows/apiv1/workflowspb"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/api/workflowexecutions/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// deployWith creates a workflow with extra fields and waits for it.
func (w *workflowsAPI) deployWith(t *testing.T, h *Harness, id string, wf *workflowspb.Workflow) (*workflowspb.Workflow, error) {
	t.Helper()
	ctx := h.Context()
	op, err := w.wf.CreateWorkflow(ctx, &workflowspb.CreateWorkflowRequest{Parent: workflowsParent(h), WorkflowId: id, Workflow: wf})
	if err != nil {
		return nil, err
	}
	return op.Wait(ctx)
}

func workflowSource(src string) *workflowspb.Workflow_SourceContents {
	return &workflowspb.Workflow_SourceContents{SourceContents: src}
}

// covers: google.cloud.workflows.v1.Workflows/CreateWorkflow, google.cloud.workflows.executions.v1.Executions/CreateExecution, google.cloud.workflows.executions.v1.Executions/GetExecution
//
// TestWorkflowsCallLogsInCloudLogging (#1996): a workflow deployed with
// call_log_level LOG_ALL_CALLS writes an EngineCallLog entry for each call
// step's start and return, and for a raised exception, to Cloud Logging,
// beside the ExecutionsSystemLog entries of the execution's start and end;
// an execution's own LOG_ERRORS_ONLY overrides it (only the exception), and
// LOG_NONE writes no call log. Each is read back with the official Logging
// client (logadmin), filtered on the execution's label.
func TestWorkflowsCallLogsInCloudLogging(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h)
	logAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_LOGGING", EnvLogging)
	ctx := h.Context()
	la, err := logadmin.NewClient(ctx, "projects/"+h.Project(), option.WithEndpoint(logAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer la.Close()
	src := `main:
  steps:
    - double:
        call: twice
        args: {x: 21}
        result: r
    - upper:
        call: text.to_upper
        args: {source: "burrow"}
        result: u
    - guarded:
        try:
          call: text.to_upper
          args: {source: 7}
        except:
          as: e
          steps:
            - note:
                assign:
                  - caught: ${e.tags[0]}
    - done:
        return: {r: "${r}", u: "${u}", caught: "${caught}"}
twice:
  params: [x]
  steps:
    - r:
        return: ${x * 2}
`
	wf, err := w.deployWith(t, h, "calllogs", &workflowspb.Workflow{CallLogLevel: workflowspb.Workflow_LOG_ALL_CALLS, SourceCode: workflowSource(src)})
	if err != nil {
		t.Fatal(err)
	}
	if wf.GetCallLogLevel() != workflowspb.Workflow_LOG_ALL_CALLS {
		t.Errorf("call_log_level %v", wf.GetCallLogLevel())
	}
	entries := func(ex *executionspb.Execution, want int) []map[string]any {
		t.Helper()
		id := ex.GetName()[strings.LastIndex(ex.GetName(), "/")+1:]
		filter := fmt.Sprintf(`logName="projects/%s/logs/workflows.googleapis.com%%2Fexecutions_system_log" AND labels."workflows.googleapis.com/execution_id"="%s"`, h.Project(), id)
		var out []map[string]any
		deadline := time.Now().Add(30 * time.Second)
		for {
			out = nil
			it := la.Entries(ctx, logadmin.Filter(filter))
			for {
				e, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatalf("Entries: %v", err)
				}
				if e.Resource.GetType() != "workflows.googleapis.com/Workflow" || e.Resource.GetLabels()["workflow_id"] != "calllogs" {
					t.Errorf("resource %v", e.Resource)
				}
				st, ok := e.Payload.(*structpb.Struct)
				if !ok {
					t.Fatalf("payload %T", e.Payload)
				}
				out = append(out, st.AsMap())
			}
			if len(out) >= want || time.Now().After(deadline) {
				return out
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	summary := func(es []map[string]any) string {
		var parts []string
		for _, e := range es {
			switch e["@type"] {
			case "type.googleapis.com/google.cloud.workflows.type.EngineCallLog":
				parts = append(parts, fmt.Sprint(e["state"], ":", e["step"], ":", e["callee"]))
			case "type.googleapis.com/google.cloud.workflows.type.ExecutionsSystemLog":
				parts = append(parts, fmt.Sprint("system:", e["state"]))
			default:
				t.Errorf("entry %v", e)
			}
		}
		return strings.Join(parts, " ")
	}

	ex := w.run(t, h, wf.GetName(), "", time.Minute)
	if ex.GetState() != executionspb.Execution_SUCCEEDED || ex.GetResult() != `{"caught":"TypeError","r":42,"u":"BURROW"}` {
		t.Fatalf("execution %v", ex)
	}
	if ex.GetCallLogLevel() != executionspb.Execution_LOG_ALL_CALLS {
		t.Errorf("the execution's level %v: the workflow's applies", ex.GetCallLogLevel())
	}
	all := entries(ex, 8)
	if got, want := summary(all), "system:ACTIVE BEGUN:double:twice SUCCEEDED:double:twice BEGUN:upper:text.to_upper SUCCEEDED:upper:text.to_upper BEGUN:guarded:text.to_upper EXCEPTION_RAISED:guarded:text.to_upper system:SUCCEEDED"; got != want {
		t.Errorf("LOG_ALL_CALLS entries\n got %s\nwant %s", got, want)
	}
	for _, e := range all {
		switch e["state"] {
		case "BEGUN":
			if e["step"] == "double" && fmt.Sprint(e["begun"]) != "map[namedArgs:map[x:21]]" {
				t.Errorf("begun %v", e["begun"])
			}
		case "SUCCEEDED":
			if s, _ := e["succeeded"].(map[string]any); e["step"] == "double" && (s["response"] != "42" || s["callStartTime"] == nil) {
				t.Errorf("succeeded %v", e["succeeded"])
			}
			if e["executionId"] == "" || e["activityTime"] == nil {
				t.Errorf("entry %v", e)
			}
		case "EXCEPTION_RAISED":
			r, _ := e["exceptionRaised"].(map[string]any)
			if !strings.Contains(fmt.Sprint(r["exception"]), "TypeError") {
				t.Errorf("exceptionRaised %v", r)
			}
		}
	}

	// The execution's level takes precedence over the workflow's.
	errorsOnly, err := w.ex.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: wf.GetName(),
		Execution: &executionspb.Execution{CallLogLevel: executionspb.Execution_LOG_ERRORS_ONLY}})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.await(t, h, errorsOnly.GetName(), time.Minute); got.GetState() != executionspb.Execution_SUCCEEDED {
		t.Fatalf("LOG_ERRORS_ONLY execution %v", got)
	}
	if got, want := summary(entries(errorsOnly, 3)), "system:ACTIVE EXCEPTION_RAISED:guarded:text.to_upper system:SUCCEEDED"; got != want {
		t.Errorf("LOG_ERRORS_ONLY entries\n got %s\nwant %s", got, want)
	}
	none, err := w.ex.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: wf.GetName(),
		Execution: &executionspb.Execution{CallLogLevel: executionspb.Execution_LOG_NONE}})
	if err != nil {
		t.Fatal(err)
	}
	w.await(t, h, none.GetName(), time.Minute)
	if got, want := summary(entries(none, 2)), "system:ACTIVE system:SUCCEEDED"; got != want {
		t.Errorf("LOG_NONE entries\n got %s\nwant %s", got, want)
	}
}

// covers: google.cloud.workflows.v1.Workflows/CreateWorkflow, google.cloud.workflows.executions.v1.Executions/CreateExecution, google.cloud.workflows.executions.v1.Executions/GetExecution, google.cloud.workflows.executions.v1.Executions/ListExecutions
//
// TestWorkflowsExperimentalExecutions (#1996): experimental.executions.run
// starts an execution of another workflow and returns its result;
// experimental.executions.map starts one per argument and returns their
// results in order; a child that fails raises ExecutionError; the
// executions connector's run helper does as run does. Each child is a
// real execution the official client lists.
func TestWorkflowsExperimentalExecutions(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h)
	ctx := h.Context()
	w.deploy(t, h, "child", `main:
  params: [n]
  steps:
    - check:
        switch:
          - condition: ${n == 0}
            raise: "zero"
    - r:
        return: ${n * 10}
`)
	parent := w.deploy(t, h, "parent", `main:
  steps:
    - one:
        call: experimental.executions.run
        args:
          workflow_id: child
          argument: 4
        result: a
    - many:
        call: experimental.executions.map
        args:
          workflow_id: child
          arguments: [1, 2, 3]
        result: b
    - helper:
        call: googleapis.workflowexecutions.v1.projects.locations.workflows.executions.run
        args:
          workflow_id: child
          argument: 5
        result: c
    - bad:
        try:
          call: experimental.executions.run
          args:
            workflow_id: child
            argument: 0
        except:
          as: e
          steps:
            - keep:
                assign:
                  - tag: ${e.tags[0]}
    - r:
        return: {a: "${a}", b: "${b}", c: "${c}", tag: "${tag}"}
`)
	ex := w.run(t, h, parent.GetName(), "", 2*time.Minute)
	if ex.GetState() != executionspb.Execution_SUCCEEDED || ex.GetResult() != `{"a":40,"b":[10,20,30],"c":50,"tag":"ExecutionError"}` {
		t.Fatalf("parent execution %v", ex)
	}
	it := w.ex.ListExecutions(ctx, &executionspb.ListExecutionsRequest{Parent: workflowsParent(h) + "/workflows/child"})
	states := map[executionspb.Execution_State]int{}
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		states[e.GetState()]++
	}
	if states[executionspb.Execution_SUCCEEDED] != 5 || states[executionspb.Execution_FAILED] != 1 {
		t.Errorf("child executions %v, want 5 SUCCEEDED and 1 FAILED", states)
	}
}

// covers: google.cloud.workflows.v1.Workflows/CreateWorkflow, google.cloud.workflows.v1.Workflows/UpdateWorkflow, google.cloud.workflows.executions.v1.Executions/CreateExecution, google.cloud.workflows.executions.v1.Executions/GetExecution, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion
//
// TestWorkflowsCustomerManagedKey (#1996): a workflow deployed with
// crypto_key_name, a key of the instance's Cloud KMS, records the key
// version; with the version disabled (the official KMS client) a new
// execution is refused and an active one FAILS with the key's error, as
// use-cmek documents; enabled again, executions run. A key in another
// region is INVALID_ARGUMENT, and a missing key PERMISSION_DENIED.
func TestWorkflowsCustomerManagedKey(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h)
	ctx := h.Context()
	kc, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_KMS", EnvKMS)),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer kc.Close()
	ring, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/us-central1", KeyRingId: "wf"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "wf",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	src := "main:\n  params: [s]\n  steps:\n    - wait:\n        call: sys.sleep\n        args:\n          seconds: ${s}\n    - r:\n        return: done\n"
	if _, err := w.deployWith(t, h, "otherregion", &workflowspb.Workflow{CryptoKeyName: "projects/" + h.Project() + "/locations/europe-west1/keyRings/wf/cryptoKeys/wf",
		SourceCode: workflowSource(src)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a key in another region: %v", err)
	}
	if _, err := w.deployWith(t, h, "nokey", &workflowspb.Workflow{CryptoKeyName: ring.GetName() + "/cryptoKeys/missing",
		SourceCode: workflowSource(src)}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a missing key: %v", err)
	}
	wf, err := w.deployWith(t, h, "sealed", &workflowspb.Workflow{CryptoKeyName: key.GetName(), SourceCode: workflowSource(src)})
	if err != nil {
		t.Fatal(err)
	}
	if wf.GetCryptoKeyName() != key.GetName() || wf.GetCryptoKeyVersion() != key.GetName()+"/cryptoKeyVersions/1" {
		t.Errorf("workflow key %q version %q", wf.GetCryptoKeyName(), wf.GetCryptoKeyVersion())
	}
	if got := w.run(t, h, wf.GetName(), "0", time.Minute); got.GetState() != executionspb.Execution_SUCCEEDED {
		t.Fatalf("with the key enabled: %v", got)
	}
	active, err := w.ex.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: wf.GetName(), Execution: &executionspb.Execution{Argument: "120"}})
	if err != nil {
		t.Fatal(err)
	}
	setState := func(st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key.GetName() + "/cryptoKeyVersions/1", State: st},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatalf("UpdateCryptoKeyVersion %v: %v", st, err)
		}
	}
	setState(kmspb.CryptoKeyVersion_DISABLED)
	_, err = w.ex.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: wf.GetName(), Execution: &executionspb.Execution{Argument: "0"}})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "Key version is not enabled") {
		t.Errorf("a new execution with the key version disabled: %v", err)
	}
	got := w.await(t, h, active.GetName(), 30*time.Second)
	if got.GetState() != executionspb.Execution_FAILED || !strings.Contains(got.GetError().GetContext(), "Key version is not enabled") {
		t.Errorf("the active execution with its key disabled: %v", got)
	}
	setState(kmspb.CryptoKeyVersion_ENABLED)
	if got := w.run(t, h, wf.GetName(), "0", time.Minute); got.GetState() != executionspb.Execution_SUCCEEDED {
		t.Errorf("enabled again: %v", got)
	}
	// A new key is a new revision encrypted with it.
	key2, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "wf2",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	op, err := w.wf.UpdateWorkflow(ctx, &workflowspb.UpdateWorkflowRequest{Workflow: &workflowspb.Workflow{Name: wf.GetName(), CryptoKeyName: key2.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"crypto_key_name"}}})
	if err != nil {
		t.Fatal(err)
	}
	wf2, err := op.Wait(ctx)
	if err != nil || !strings.HasPrefix(wf2.GetRevisionId(), "000002-") || wf2.GetCryptoKeyVersion() != key2.GetName()+"/cryptoKeyVersions/1" {
		t.Errorf("new key: %v %v", wf2, err)
	}
}

// covers: google.cloud.workflows.v1.Workflows/CreateWorkflow
//
// TestWorkflowsExportDeleteHistoryAndPubsubTrigger (#1996), with the
// official REST client (google.golang.org/api/workflowexecutions/v1),
// as the gRPC API has none of these methods:
// projects.locations.workflows.triggerPubsubExecution starts an execution
// whose argument is the Pub/Sub message as a CloudEvent;
// executions.exportData returns its data, its step entries among them; and
// executions.deleteExecutionHistory deletes its step entries, which
// stepEntries.list then shows.
func TestWorkflowsExportDeleteHistoryAndPubsubTrigger(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h)
	ctx := h.Context()
	wf := w.deploy(t, h, "fromtopic", `main:
  params: [event]
  steps:
    - decode:
        assign:
          - text: ${text.decode(base64.decode(event.data.message.data))}
    - r:
        return: {text: "${text}", type: "${event.type}", attr: "${event.data.message.attributes.k}"}
`)
	sub := "projects/" + h.Project() + "/subscriptions/wf-sub"
	ex, err := w.rest.Projects.Locations.Workflows.TriggerPubsubExecution(wf.GetName(), &workflowexecutions.TriggerPubsubExecutionRequest{
		Subscription: sub, GCPCloudEventsMode: "CUSTOM_PUBSUB_EVENTS",
		Message: &workflowexecutions.PubsubMessage{Data: base64.StdEncoding.EncodeToString([]byte("hello")), Attributes: map[string]string{"k": "v"},
			MessageId: "m-1", PublishTime: time.Now().UTC().Format(time.RFC3339)}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("triggerPubsubExecution: %v", err)
	}
	got := w.await(t, h, ex.Name, time.Minute)
	if got.GetState() != executionspb.Execution_SUCCEEDED || got.GetResult() != `{"attr":"v","text":"hello","type":"google.cloud.pubsub.topic.v1.messagePublished"}` {
		t.Fatalf("execution %v", got)
	}
	data, err := w.rest.Projects.Locations.Workflows.Executions.ExportData(ex.Name).Context(ctx).Do()
	if err != nil {
		t.Fatalf("exportData: %v", err)
	}
	var exported struct {
		StepEntries []map[string]any `json:"stepEntries"`
	}
	if err := json.Unmarshal([]byte(data.Data), &exported); err != nil || len(exported.StepEntries) != 2 {
		t.Errorf("exportData %q: %v", data.Data, err)
	}
	steps, err := w.rest.Projects.Locations.Workflows.Executions.StepEntries.List(ex.Name).Context(ctx).Do()
	if err != nil || len(steps.StepEntries) != 2 {
		t.Fatalf("stepEntries before: %v %v", steps, err)
	}
	if _, err := w.rest.Projects.Locations.Workflows.Executions.DeleteExecutionHistory(ex.Name, &workflowexecutions.DeleteExecutionHistoryRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("deleteExecutionHistory: %v", err)
	}
	steps, err = w.rest.Projects.Locations.Workflows.Executions.StepEntries.List(ex.Name).Context(ctx).Do()
	if err != nil || len(steps.StepEntries) != 0 {
		t.Errorf("stepEntries after deleteExecutionHistory: %v %v", steps, err)
	}
	// The execution itself remains.
	if again, err := w.ex.GetExecution(ctx, &executionspb.GetExecutionRequest{Name: ex.Name}); err != nil || again.GetState() != executionspb.Execution_SUCCEEDED {
		t.Errorf("after deleteExecutionHistory: %v %v", again, err)
	}
	if _, err := w.rest.Projects.Locations.Workflows.TriggerPubsubExecution(workflowsParent(h)+"/workflows/missing",
		&workflowexecutions.TriggerPubsubExecutionRequest{Subscription: sub, Message: &workflowexecutions.PubsubMessage{Data: "aGk="}}).Context(ctx).Do(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("triggerPubsubExecution of a missing workflow: %v", err)
	}
}

// covers: google.cloud.workflows.v1.Workflows/CreateWorkflow, google.cloud.workflows.executions.v1.Executions/CreateExecution, google.cloud.workflows.executions.v1.Executions/GetExecution
//
// TestWorkflowsConnectorsReadBack (#1996): a workflow calls the Secret
// Manager, Cloud Tasks, Cloud Logging, Firestore, Cloud KMS, Cloud
// Scheduler, Workflows, Workflow Executions and Cloud Run connectors of
// this instance; each effect is read back with the service's official
// client.
func TestWorkflowsConnectorsReadBack(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h)
	ctx := h.Context()
	sm := secretsClient(t, h)
	tc := tasksClient(t, h)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_FIRESTORE", EnvFirestore)
	fs := firestoreClient(t, h)
	kmsAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_KMS", EnvKMS)
	schedAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_SCHEDULER", EnvScheduler)
	logAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_LOGGING", EnvLogging)
	h.Endpoint(EnvRun)
	grpcOpts := func(addr string) []option.ClientOption {
		return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	}
	kc, err := kms.NewKeyManagementClient(ctx, grpcOpts(kmsAddr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer kc.Close()
	sc, err := scheduler.NewCloudSchedulerClient(ctx, grpcOpts(schedAddr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	la, err := logadmin.NewClient(ctx, "projects/"+h.Project(), grpcOpts(logAddr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer la.Close()
	p := h.Project()
	loc := "projects/" + p + "/locations/us-central1"
	ring, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "conn"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "conn",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	w.deploy(t, h, "target", "main:\n  steps:\n    - r:\n        return: from-target\n")
	src := `main:
  steps:
    - secret:
        call: googleapis.secretmanager.v1.projects.secrets.create
        args:
          parent: projects/` + p + `
          secretId: conn-secret
          body:
            replication:
              automatic: {}
    - version:
        call: googleapis.secretmanager.v1.projects.secrets.addVersion
        args:
          parent: projects/` + p + `/secrets/conn-secret
          body:
            payload:
              data: ${base64.encode(text.encode("s3cret"))}
    - access:
        call: googleapis.secretmanager.v1.projects.secrets.versions.accessString
        args:
          secret_id: conn-secret
          project_id: ` + p + `
        result: secretText
    - queue:
        call: googleapis.cloudtasks.v2.projects.locations.queues.create
        args:
          parent: ` + loc + `
          body:
            name: ` + loc + `/queues/conn-queue
    - task:
        call: googleapis.cloudtasks.v2.projects.locations.queues.tasks.create
        args:
          parent: ` + loc + `/queues/conn-queue
          body:
            task:
              scheduleTime: "2099-01-01T00:00:00Z"
              httpRequest:
                url: http://127.0.0.1:9/never
    - log:
        call: googleapis.logging.v2.entries.write
        args:
          body:
            logName: projects/` + p + `/logs/conn-log
            resource:
              type: global
            entries:
              - textPayload: written-by-a-connector
    - doc:
        call: googleapis.firestore.v1.projects.databases.documents.createDocument
        args:
          parent: projects/` + p + `/databases/(default)/documents
          collectionId: conn
          documentId: d1
          body:
            fields:
              v:
                stringValue: from-workflow
    - getdoc:
        call: googleapis.firestore.v1.projects.databases.documents.get
        args:
          name: projects/` + p + `/databases/(default)/documents/conn/d1
        result: doc
    - enc:
        call: googleapis.cloudkms.v1.projects.locations.keyRings.cryptoKeys.encrypt
        args:
          name: ` + key.GetName() + `
          body:
            plaintext: ${base64.encode(text.encode("kms-plain"))}
        result: sealed
    - dec:
        call: googleapis.cloudkms.v1.projects.locations.keyRings.cryptoKeys.decrypt
        args:
          name: ` + key.GetName() + `
          body:
            ciphertext: ${sealed.ciphertext}
        result: opened
    - job:
        call: googleapis.cloudscheduler.v1.projects.locations.jobs.create
        args:
          parent: ` + loc + `
          body:
            name: ` + loc + `/jobs/conn-job
            schedule: "0 0 1 1 *"
            httpTarget:
              uri: http://127.0.0.1:9/never
    - getjob:
        call: googleapis.cloudscheduler.v1.projects.locations.jobs.get
        args:
          name: ` + loc + `/jobs/conn-job
        result: job
    - wfs:
        call: googleapis.workflows.v1.projects.locations.workflows.list
        args:
          parent: ` + loc + `
        result: wfs
    - wfget:
        call: googleapis.workflows.v1.projects.locations.workflows.get
        args:
          name: ` + loc + `/workflows/target
        result: wfget
    - start:
        call: googleapis.workflowexecutions.v1.projects.locations.workflows.executions.create
        args:
          parent: ` + loc + `/workflows/target
          body: {}
        result: started
    - getex:
        call: googleapis.workflowexecutions.v1.projects.locations.workflows.executions.get
        args:
          name: ${started.name}
        result: gotex
    - services:
        call: googleapis.run.v2.projects.locations.services.list
        args:
          parent: ` + loc + `
        result: services
    - r:
        return:
          secret: ${secretText}
          doc: ${doc.fields.v.stringValue}
          kms: ${text.decode(base64.decode(opened.plaintext))}
          job: ${job.schedule}
          wfs: ${len(wfs.workflows)}
          wf: ${wfget.state}
          execution: ${started.name}
          servicesType: ${get_type(services)}
`
	wf := w.deploy(t, h, "connectors", src)
	ex := w.run(t, h, wf.GetName(), "", 2*time.Minute)
	if ex.GetState() != executionspb.Execution_SUCCEEDED {
		t.Fatalf("execution %v", ex)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(ex.GetResult()), &res); err != nil {
		t.Fatal(err)
	}
	if res["secret"] != "s3cret" || res["doc"] != "from-workflow" || res["kms"] != "kms-plain" || res["job"] != "0 0 1 1 *" ||
		res["wfs"] != float64(2) || res["wf"] != "ACTIVE" || res["servicesType"] != "map" {
		t.Errorf("result %s", ex.GetResult())
	}
	// Read back with each official client.
	if v, err := sm.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: "projects/" + p + "/secrets/conn-secret/versions/latest"}); err != nil || string(v.GetPayload().GetData()) != "s3cret" {
		t.Errorf("Secret Manager: %v %v", v, err)
	}
	tasks := tc.ListTasks(ctx, &cloudtaskspb.ListTasksRequest{Parent: loc + "/queues/conn-queue"})
	n := 0
	for {
		task, err := tasks.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		if task.GetHttpRequest().GetUrl() != "http://127.0.0.1:9/never" {
			t.Errorf("task %v", task)
		}
		n++
	}
	if n != 1 {
		t.Errorf("%d tasks", n)
	}
	snap, err := fs.Collection("conn").Doc("d1").Get(ctx)
	if err != nil || snap.Data()["v"] != "from-workflow" {
		t.Errorf("Firestore: %v %v", snap, err)
	}
	if j, err := sc.GetJob(ctx, &schedulerpb.GetJobRequest{Name: loc + "/jobs/conn-job"}); err != nil || j.GetSchedule() != "0 0 1 1 *" {
		t.Errorf("Cloud Scheduler: %v %v", j, err)
	}
	started, _ := res["execution"].(string)
	if got := w.await(t, h, started, time.Minute); got.GetState() != executionspb.Execution_SUCCEEDED || got.GetResult() != `"from-target"` {
		t.Errorf("the execution the connector started: %v", got)
	}
	found := false
	deadline := time.Now().Add(20 * time.Second)
	for !found && time.Now().Before(deadline) {
		it := la.Entries(ctx, logadmin.Filter(`logName="projects/`+p+`/logs/conn-log"`))
		for {
			e, err := it.Next()
			if err != nil {
				break
			}
			if e.Payload == "written-by-a-connector" {
				found = true
			}
		}
		if !found {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !found {
		t.Error("Cloud Logging: the connector's entry was not read back")
	}
	_ = cloudtasks.NewClient
	_ = http.MethodGet
}

// The restart probe (#1996): an execution running when the instance stops
// carries on when it starts again.
const (
	envWorkflowsProbe  = "CLOUDBURROW_TEST_WORKFLOWS_PROBE"
	envWorkflowsExpect = "CLOUDBURROW_TEST_WORKFLOWS_EXPECT"
)

type workflowsProbe struct {
	Execution string    `json:"execution"`
	Callback  string    `json:"callback"`
	SetUpAt   time.Time `json:"setUpAt"`
}

const workflowsProbeProject = "workflows-restart-probe"

// TestWorkflowsRestartSetup starts an execution that notes sys.now and a
// uuid, makes a callback endpoint and waits on it, and leaves the
// execution's and the callback's names for TestWorkflowsAcrossRestart.
func TestWorkflowsRestartSetup(t *testing.T) {
	path := os.Getenv(envWorkflowsProbe)
	if path == "" {
		t.Skipf("%s is not set: CI runs this alone, just before stop", envWorkflowsProbe)
	}
	h := New(t)
	w := workflowsClients(t, h)
	ctx := h.Context()
	parent := "projects/" + workflowsProbeProject + "/locations/us-central1"
	op, err := w.wf.CreateWorkflow(ctx, &workflowspb.CreateWorkflowRequest{Parent: parent, WorkflowId: "restart-" + fmt.Sprint(time.Now().Unix()),
		Workflow: &workflowspb.Workflow{SourceCode: workflowSource(`main:
  steps:
    - before:
        assign:
          - t0: ${sys.now()}
          - id: ${uuid.generate()}
    - cb:
        call: events.create_callback_endpoint
        result: cb
    - nap:
        call: sys.sleep
        args:
          seconds: 1
    - wait:
        call: events.await_callback
        args:
          callback: ${cb}
          timeout: 3600
        result: req
    - r:
        return: {t0: "${t0}", id: "${id}", body: "${req.http_request.body}"}
`)}})
	if err != nil {
		t.Fatal(err)
	}
	wf, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ex, err := w.ex.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: wf.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	var cb string
	deadline := time.Now().Add(30 * time.Second)
	for cb == "" && time.Now().Before(deadline) {
		l, err := w.rest.Projects.Locations.Workflows.Executions.Callbacks.List(ex.GetName()).Context(ctx).Do()
		if err != nil {
			t.Fatalf("callbacks.list: %v", err)
		}
		if len(l.Callbacks) == 1 && l.Callbacks[0].Waiters == 1 {
			cb = l.Callbacks[0].Name
		} else {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if cb == "" {
		t.Fatal("the execution did not reach its await_callback")
	}
	b, _ := json.Marshal(workflowsProbe{Execution: ex.GetName(), Callback: cb, SetUpAt: time.Now()})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// covers: google.cloud.workflows.executions.v1.Executions/GetExecution
//
// TestWorkflowsAcrossRestart (#1996): after the instance stopped and
// started again, the execution TestWorkflowsRestartSetup left is still
// ACTIVE, waiting on the same callback endpoint, and a request to it ends
// the execution with the request's body; sys.now and uuid.generate kept
// the values they had before the restart. In an ephemeral instance the
// execution is gone.
func TestWorkflowsAcrossRestart(t *testing.T) {
	expect := os.Getenv(envWorkflowsExpect)
	if expect == "" {
		t.Skipf("%s is not set: this runs only after CI restarts the instance", envWorkflowsExpect)
	}
	b, err := os.ReadFile(os.Getenv(envWorkflowsProbe))
	if err != nil {
		t.Fatalf("the probe TestWorkflowsRestartSetup left: %v", err)
	}
	var p workflowsProbe
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	h := New(t)
	w := workflowsClients(t, h)
	ctx := h.Context()
	ex, err := w.ex.GetExecution(ctx, &executionspb.GetExecutionRequest{Name: p.Execution})
	if expect == "absent" {
		if status.Code(err) != codes.NotFound {
			t.Errorf("ephemeral: GetExecution %v %v, want NOT_FOUND", ex, err)
		}
		return
	}
	if err != nil || ex.GetState() != executionspb.Execution_ACTIVE {
		t.Fatalf("after the restart: %v %v, want ACTIVE", ex, err)
	}
	var l *workflowexecutions.ListCallbacksResponse
	deadline := time.Now().Add(30 * time.Second)
	for {
		l, err = w.rest.Projects.Locations.Workflows.Executions.Callbacks.List(p.Execution).Context(ctx).Do()
		if err != nil {
			t.Fatalf("callbacks.list: %v", err)
		}
		if (len(l.Callbacks) == 1 && l.Callbacks[0].Waiters == 1) || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(l.Callbacks) != 1 || l.Callbacks[0].Name != p.Callback {
		t.Fatalf("callbacks after the restart %+v, want %s", l.Callbacks, p.Callback)
	}
	resp, err := http.Post("http://"+w.addr+"/v1/"+p.Callback, "application/json", strings.NewReader(`"after-restart"`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback POST: %s", resp.Status)
	}
	got := w.await(t, h, p.Execution, time.Minute)
	if got.GetState() != executionspb.Execution_SUCCEEDED {
		t.Fatalf("execution %v", got)
	}
	var res struct {
		T0   float64 `json:"t0"`
		ID   string  `json:"id"`
		Body string  `json:"body"`
	}
	if err := json.Unmarshal([]byte(got.GetResult()), &res); err != nil {
		t.Fatal(err)
	}
	if res.Body != "after-restart" || res.ID == "" || time.Unix(0, int64(res.T0*1e9)).After(p.SetUpAt) {
		t.Errorf("result %s: sys.now must be the value from before the restart (%v)", got.GetResult(), p.SetUpAt)
	}
}
