//go:build compat

package compat

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admin "cloud.google.com/go/iam/admin/apiv1"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/genproto/googleapis/api"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const ccAvro = `{"type":"record","name":"Order","fields":[{"name":"n","type":"int"}]}`

const ccAvroLegacy = `{"type":"record","name":"Order","fields":[{"name":"legacy","type":"int"}]}`

const ccProto = `syntax = "proto3"; message Order { int32 n = 1; }`

// covers: google.pubsub.v1.SchemaService/CreateSchema, google.pubsub.v1.SchemaService/CommitSchema, google.pubsub.v1.SchemaService/RollbackSchema, google.pubsub.v1.Subscriber/CreateSnapshot, google.pubsub.v1.Subscriber/UpdateSnapshot
//
// TestOrgPolicyCustomConstraintsPubSubSchemasSnapshots (#1895), with the
// official Pub/Sub clients: an ALLOW on pubsub.googleapis.com/Schema
// (resource.type == 'AVRO', CREATE and UPDATE) refuses a Protocol Buffer
// schema, FAILED_PRECONDITION "Operation denied by custom org policies:
// [...]", over gRPC and REST, creates an Avro one, and refuses committing
// a Protocol Buffer revision of it; a DENY on the definition (UPDATE)
// refuses a RollbackSchema to a revision it matches, over gRPC and REST
// (#1902); a DENY on
// pubsub.googleapis.com/Snapshot reading resource.topic refuses a
// snapshot of a subscription to the topic it names, and one reading
// resource.expireTime (UPDATE) an update that brings the expiry within
// a day. A condition on a field the page does not list (the schema's
// revisionId) cannot be evaluated, and refuses.
func TestOrgPolicyCustomConstraintsPubSubSchemasSnapshots(t *testing.T) {
	h := New(t)
	ps := pubsubClient(t, h)
	ctx := h.Context()
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	sc, err := vkit.NewSchemaClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	project := orgPolicyProject(t, h, pc, localOrg, "ocs-")
	avro := customName(h, "avroonly")
	adesc := "Schemas must be Avro."
	blocked := customName(h, "noblockedsnap")
	bdesc := "No snapshots of the blocked topic."
	expiry := customName(h, "snapexpiry")
	edesc := "Snapshots must live at least a day."
	createCustom(t, h, c, avro, "pubsub.googleapis.com/Schema", "resource.type == 'AVRO'", ccAllow, adesc, ccCreate, ccUpdate)
	createCustom(t, h, c, blocked, "pubsub.googleapis.com/Snapshot", "resource.topic.endsWith('/cc-blocked')", ccDeny, bdesc, ccCreate)
	day := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	createCustom(t, h, c, expiry, "pubsub.googleapis.com/Snapshot", "resource.expireTime < timestamp('"+day+"')", ccDeny, edesc, ccUpdate)
	for _, n := range []string{avro, blocked, expiry} {
		setOrgPolicy(t, h, c, "projects/"+project, n, opEnforce())
	}
	parent := "projects/" + project
	_, err = sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: "cc-proto",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: ccProto}})
	wantFailedPrecondition(t, "a Protocol Buffer schema", err, customMessage(avro, adesc))
	code, out := pubsubREST(t, h, http.MethodPost, "/v1/"+parent+"/schemas?schemaId=cc-rest",
		`{"type":"PROTOCOL_BUFFER","definition":"syntax = \"proto3\"; message M { int32 n = 1; }"}`)
	if e, _ := out["error"].(map[string]any); code != http.StatusBadRequest || e["status"] != "FAILED_PRECONDITION" || e["message"] != customMessage(avro, adesc) {
		t.Errorf("REST create of a Protocol Buffer schema = %d %v, want 400 FAILED_PRECONDITION", code, out)
	}
	schema := parent + "/schemas/cc-avro"
	if _, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: "cc-avro",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: ccAvro}}); err != nil {
		t.Fatalf("an Avro schema: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: schema}) })
	_, err = sc.CommitSchema(ctx, &pubsubpb.CommitSchemaRequest{Name: schema,
		Schema: &pubsubpb.Schema{Name: schema, Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: ccProto}})
	wantFailedPrecondition(t, "committing a Protocol Buffer revision", err, customMessage(avro, adesc))

	// RollbackSchema (#1902) is an UPDATE read on the revision it copies.
	roll := parent + "/schemas/cc-roll"
	first, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: "cc-roll",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: ccAvroLegacy}})
	if err != nil {
		t.Fatalf("an Avro schema with a legacy field: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: roll}) })
	if _, err := sc.CommitSchema(ctx, &pubsubpb.CommitSchemaRequest{Name: roll,
		Schema: &pubsubpb.Schema{Name: roll, Type: pubsubpb.Schema_AVRO, Definition: ccAvro}}); err != nil {
		t.Fatalf("committing a revision without it: %v", err)
	}
	legacy := customName(h, "nolegacy")
	ldesc := "No legacy fields."
	createCustom(t, h, c, legacy, "pubsub.googleapis.com/Schema", "resource.definition.contains('legacy')", ccDeny, ldesc, ccUpdate)
	setOrgPolicy(t, h, c, "projects/"+project, legacy, opEnforce())
	_, err = sc.RollbackSchema(ctx, &pubsubpb.RollbackSchemaRequest{Name: roll, RevisionId: first.GetRevisionId()})
	wantFailedPrecondition(t, "rolling back to the legacy revision", err, customMessage(legacy, ldesc))
	code, out = pubsubREST(t, h, http.MethodPost, "/v1/"+roll+":rollback", `{"revisionId":"`+first.GetRevisionId()+`"}`)
	if e, _ := out["error"].(map[string]any); code != http.StatusBadRequest || e["message"] != customMessage(legacy, ldesc) {
		t.Errorf("REST rollback to the legacy revision = %d %v, want 400 FAILED_PRECONDITION", code, out)
	}

	// Topics and subscriptions to snapshot.
	sub := func(topicID, subID string) string {
		t.Helper()
		topic := parent + "/topics/" + topicID
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
			t.Fatalf("CreateTopic %s: %v", topic, err)
		}
		t.Cleanup(func() {
			_ = ps.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: topic})
		})
		s := parent + "/subscriptions/" + subID
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: s, Topic: topic}); err != nil {
			t.Fatalf("CreateSubscription %s: %v", s, err)
		}
		t.Cleanup(func() {
			_ = ps.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: s})
		})
		return s
	}
	bad := sub("cc-blocked", "cc-blocked-sub")
	_, err = ps.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: parent + "/snapshots/cc-bad", Subscription: bad})
	wantFailedPrecondition(t, "a snapshot of the blocked topic", err, customMessage(blocked, bdesc))
	code, out = pubsubREST(t, h, http.MethodPut, "/v1/"+parent+"/snapshots/cc-bad-rest", `{"subscription":"`+bad+`"}`)
	if e, _ := out["error"].(map[string]any); code != http.StatusBadRequest || e["message"] != customMessage(blocked, bdesc) {
		t.Errorf("REST create of a snapshot of the blocked topic = %d %v, want 400 FAILED_PRECONDITION", code, out)
	}
	good := sub("cc-open", "cc-open-sub")
	snap := parent + "/snapshots/cc-good"
	if _, err := ps.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: snap, Subscription: good}); err != nil {
		t.Fatalf("a snapshot of another topic: %v", err)
	}
	t.Cleanup(func() {
		_ = ps.SubscriptionAdminClient.DeleteSnapshot(context.Background(), &pubsubpb.DeleteSnapshotRequest{Snapshot: snap})
	})
	_, err = ps.SubscriptionAdminClient.UpdateSnapshot(ctx, &pubsubpb.UpdateSnapshotRequest{
		Snapshot:   &pubsubpb.Snapshot{Name: snap, ExpireTime: timestamppb.New(time.Now().Add(time.Hour))},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}}})
	wantFailedPrecondition(t, "an expiry within the hour", err, customMessage(expiry, edesc))
	if _, err := ps.SubscriptionAdminClient.UpdateSnapshot(ctx, &pubsubpb.UpdateSnapshotRequest{
		Snapshot:   &pubsubpb.Snapshot{Name: snap, ExpireTime: timestamppb.New(time.Now().Add(48 * time.Hour))},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}}}); err != nil {
		t.Errorf("an expiry in two days: %v", err)
	}

	// A field the page does not list is absent: the condition cannot be
	// evaluated, and the schema is refused.
	other := orgPolicyProject(t, h, pc, localOrg, "ocs2-")
	unlisted := customName(h, "unlisted")
	udesc := "Reads an unlisted field."
	createCustom(t, h, c, unlisted, "pubsub.googleapis.com/Schema", "resource.revisionId == ''", ccDeny, udesc, ccCreate)
	setOrgPolicy(t, h, c, "projects/"+other, unlisted, opEnforce())
	_, err = sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + other, SchemaId: "cc-unlisted",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: ccAvro}})
	wantFailedPrecondition(t, "a condition on an unlisted field", err, customMessage(unlisted, udesc))
}

// covers: google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/UpdateJob, google.cloud.run.v2.WorkerPools/CreateWorkerPool, google.cloud.run.v2.WorkerPools/UpdateWorkerPool
//
// TestOrgPolicyCustomConstraintsRunJobsWorkerPools (#1895), with the
// official Cloud Run v2 clients: the Cloud Run page's launch-stage
// example on run.googleapis.com/Job (ALLOW only GA, or no launch stage,
// read from resource.metadata.annotations of the v1 shape) refuses a
// BETA job, FAILED_PRECONDITION "Operation denied by custom org
// policies: [...]", creates one without, and refuses updating it to
// BETA; a memory-limit ALLOW on the job's v1 task containers
// (resource.spec.template.spec.template.spec.containers) refuses a job
// without one. On run.googleapis.com/WorkerPool, the same launch-stage
// example refuses a BETA pool and validates a GA one, and the page's
// memory-limit example reads the pool's containers. (Binary
// Authorization on a worker pool is 501 locally, before any constraint.)
func TestOrgPolicyCustomConstraintsRunJobsWorkerPools(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envRunResourceManager, EnvResourceManager)
	ctx := h.Context()
	jc, _ := runJobClients(t, h)
	pools, err := run.NewWorkerPoolsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "ocj-")
	stage := customName(h, "jobgaonly")
	sdesc := "Only GA jobs."
	memory := customName(h, "jobmemory")
	mdesc := "Job containers must set a memory limit."
	createCustom(t, h, c, stage, "run.googleapis.com/Job",
		"!('run.googleapis.com/launch-stage' in resource.metadata.annotations) || resource.metadata.annotations['run.googleapis.com/launch-stage'] == 'GA'",
		ccAllow, sdesc, ccCreate, ccUpdate)
	createCustom(t, h, c, memory, "run.googleapis.com/Job",
		"resource.spec.template.spec.template.spec.containers.all(container, 'memory' in container.resources.limits)", ccAllow, mdesc, ccCreate, ccUpdate)
	setOrgPolicy(t, h, c, "projects/"+project, stage, opEnforce())
	setOrgPolicy(t, h, c, "projects/"+project, memory, opEnforce())
	parent := "projects/" + project + "/locations/us-central1"
	job := func(ls api.LaunchStage, mem bool) *runpb.Job {
		ctr := &runpb.Container{Image: jobImage, Command: []string{"true"}}
		if mem {
			ctr.Resources = &runpb.ResourceRequirements{Limits: map[string]string{"memory": "512Mi", "cpu": "1"}}
		}
		return &runpb.Job{LaunchStage: ls, Template: &runpb.ExecutionTemplate{Template: &runpb.TaskTemplate{Containers: []*runpb.Container{ctr}}}}
	}
	_, err = jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: parent, JobId: "cc-beta", Job: job(api.LaunchStage_BETA, true)})
	wantFailedPrecondition(t, "a BETA job", err, customMessage(stage, sdesc))
	_, err = jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: parent, JobId: "cc-nomem", Job: job(api.LaunchStage_LAUNCH_STAGE_UNSPECIFIED, false)})
	wantFailedPrecondition(t, "a job without a memory limit", err, customMessage(memory, mdesc))
	op, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: parent, JobId: "cc-ok", Job: job(api.LaunchStage_LAUNCH_STAGE_UNSPECIFIED, true)})
	if err != nil {
		t.Fatalf("an allowed job: %v", err)
	}
	created, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("an allowed job: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: created.GetName()}) })
	beta := job(api.LaunchStage_BETA, true)
	beta.Name = created.GetName()
	_, err = jc.UpdateJob(ctx, &runpb.UpdateJobRequest{Job: beta})
	wantFailedPrecondition(t, "updating the job to BETA", err, customMessage(stage, sdesc))
	ga := job(api.LaunchStage_GA, true)
	ga.Name = created.GetName()
	if _, err := jc.UpdateJob(ctx, &runpb.UpdateJobRequest{Job: ga, ValidateOnly: true}); err != nil {
		t.Errorf("updating the job to GA (validate only): %v", err)
	}

	other := orgPolicyProject(t, h, pc, localOrg, "ocw-")
	poolStage := customName(h, "poolgaonly")
	bdesc := "Only GA worker pools."
	pmem := customName(h, "poolmemory")
	pdesc := "Worker pool containers must set a memory limit."
	createCustom(t, h, c, poolStage, "run.googleapis.com/WorkerPool",
		"!('run.googleapis.com/launch-stage' in resource.metadata.annotations) || resource.metadata.annotations['run.googleapis.com/launch-stage'] == 'GA'",
		ccAllow, bdesc, ccCreate, ccUpdate)
	createCustom(t, h, c, pmem, "run.googleapis.com/WorkerPool",
		"resource.spec.template.spec.containers.all(container, 'memory' in container.resources.limits)", ccAllow, pdesc, ccCreate, ccUpdate)
	setOrgPolicy(t, h, c, "projects/"+other, poolStage, opEnforce())
	setOrgPolicy(t, h, c, "projects/"+other, pmem, opEnforce())
	oparent := "projects/" + other + "/locations/us-central1"
	pool := func(ls api.LaunchStage, mem bool) *runpb.WorkerPool {
		ctr := &runpb.Container{Image: jobImage, Command: []string{"sleep", "3600"}}
		if mem {
			ctr.Resources = &runpb.ResourceRequirements{Limits: map[string]string{"memory": "512Mi", "cpu": "1"}}
		}
		return &runpb.WorkerPool{LaunchStage: ls, Template: &runpb.WorkerPoolRevisionTemplate{Containers: []*runpb.Container{ctr}}}
	}
	_, err = pools.CreateWorkerPool(ctx, &runpb.CreateWorkerPoolRequest{Parent: oparent, WorkerPoolId: "cc-beta", WorkerPool: pool(api.LaunchStage_BETA, true)})
	wantFailedPrecondition(t, "a BETA pool", err, customMessage(poolStage, bdesc))
	_, err = pools.CreateWorkerPool(ctx, &runpb.CreateWorkerPoolRequest{Parent: oparent, WorkerPoolId: "cc-nomem", WorkerPool: pool(api.LaunchStage_GA, false)})
	wantFailedPrecondition(t, "a pool without a memory limit", err, customMessage(pmem, pdesc))
	if _, err := pools.CreateWorkerPool(ctx, &runpb.CreateWorkerPoolRequest{Parent: oparent, WorkerPoolId: "cc-ok", WorkerPool: pool(api.LaunchStage_GA, true),
		ValidateOnly: true}); err != nil {
		t.Errorf("an allowed pool (validate only): %v", err)
	}
}

// covers: google.iam.admin.v1.IAM/CreateServiceAccountKey, google.iam.admin.v1.IAM/UploadServiceAccountKey
//
// TestOrgPolicyCustomConstraintsServiceAccountKeyName (#1895): on
// keys.create, resource.name is the key's whole name,
// projects/PROJECT_ID/serviceAccounts/UNIQUE_ID/keys/KEY_ID (the IAM
// page's Supported resources): an ALLOW requiring that shape lets a key
// be made, under the ID the key then has; the page's "Disable service
// account key upload" example (DENY USER_MANAGED and USER_PROVIDED)
// refuses an upload, FAILED_PRECONDITION "Operation denied by custom org
// policy: [...]", and leaves keys.create alone.
func TestOrgPolicyCustomConstraintsServiceAccountKeyName(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	ep := h.Endpoint(EnvResourceManager)
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(ep))
	project := orgPolicyProject(t, h, pc, localOrg, "ock-")
	adm, err := admin.NewIamClient(ctx, rmOptionsAt(ep)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adm.Close() })
	shape := customName(h, "keyname")
	upload := customName(h, "noupload")
	createCustom(t, h, c, shape, "iam.googleapis.com/ServiceAccountKey",
		"resource.name.matches('^projects/"+project+"/serviceAccounts/[0-9]+/keys/[0-9a-f]{40}$')", ccAllow, "Keys are named in full.", ccCreate)
	udesc := "Disable service account key upload"
	createCustom(t, h, c, upload, "iam.googleapis.com/ServiceAccountKey",
		"resource.keyType == 'USER_MANAGED' && resource.keyOrigin == 'USER_PROVIDED'", ccDeny, udesc, ccCreate)
	setOrgPolicy(t, h, c, "projects/"+project, shape, opEnforce())
	setOrgPolicy(t, h, c, "projects/"+project, upload, opEnforce())
	sa, err := adm.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/" + project, AccountId: "cckeys1"})
	if err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	key, err := adm.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: sa.GetName()})
	if err != nil {
		t.Fatalf("a key named in full: %v", err)
	}
	if id := key.GetName()[strings.LastIndex(key.GetName(), "/")+1:]; len(id) != 40 {
		t.Errorf("the key's ID = %q, want the 40 hex digits the condition read", id)
	}
	_, err = adm.UploadServiceAccountKey(ctx, &adminpb.UploadServiceAccountKeyRequest{Name: sa.GetName(), PublicKeyData: uploadCert(t)})
	want := `Operation denied by custom org policy: ["customConstraints/` + upload + `":"` + udesc + `"]`
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != want {
		t.Errorf("an upload = %v, want FAILED_PRECONDITION %s", err, want)
	}
}

// covers: google.cloud.orgpolicy.v2.OrgPolicy/CreateCustomConstraint
//
// TestGcloudOrgPoliciesCustomConstraintsSchemaJob (#1895): gcloud
// org-policies set-custom-constraint creates constraints on
// pubsub.googleapis.com/Schema and run.googleapis.com/Job from YAML
// files, set-policy enforces them on a project, and the official clients
// are refused what they forbid.
func TestGcloudOrgPoliciesCustomConstraintsSchemaJob(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envRunResourceManager, EnvResourceManager)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	h.Endpoint(EnvMetadata)
	h.Endpoint(EnvRun)
	ps := pubsubClient(t, h)
	_ = ps
	ctx := h.Context()
	sc, err := vkit.NewSchemaClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	jc, _ := runJobClients(t, h)
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "ocgs-")
	g := newGcloudSession(t, h)
	org := strings.TrimPrefix(localOrg, "organizations/")
	dir := t.TempDir()
	set := func(short, resourceType, condition, action, desc string) {
		t.Helper()
		file := filepath.Join(dir, short+".yaml")
		body := "name: " + localOrg + "/customConstraints/" + short + "\nresourceTypes:\n- " + resourceType + "\nmethodTypes:\n- CREATE\n- UPDATE\n" +
			"condition: \"" + condition + "\"\nactionType: " + action + "\ndisplayName: " + short + "\ndescription: " + desc + "\n"
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		g.must("org-policies", "set-custom-constraint", file)
		t.Cleanup(func() {
			_, _ = g.run(nil, "org-policies", "delete-custom-constraint", short, "--organization="+org, "--quiet")
		})
		pfile := filepath.Join(dir, short+"-policy.yaml")
		if err := os.WriteFile(pfile, []byte("name: projects/"+project+"/policies/"+short+"\nspec:\n  rules:\n  - enforce: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		g.must("org-policies", "set-policy", pfile)
	}
	schemaC := customName(h, "gschema")
	jobC := customName(h, "gjob")
	set(schemaC, "pubsub.googleapis.com/Schema", "resource.name.endsWith('-blocked')", "DENY", "No blocked schemas.")
	set(jobC, "run.googleapis.com/Job", "'run.googleapis.com/launch-stage' in resource.metadata.annotations", "DENY", "No launch stage on jobs.")
	out := g.must("org-policies", "describe-custom-constraint", jobC, "--organization="+org, "--format=json")
	if !strings.Contains(out, "run.googleapis.com/Job") {
		t.Errorf("describe-custom-constraint = %s", out)
	}
	_, err = sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + project, SchemaId: "s-blocked",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: ccAvro}})
	wantFailedPrecondition(t, "a schema the gcloud constraint denies", err, customMessage(schemaC, "No blocked schemas."))
	_, err = jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: "projects/" + project + "/locations/us-central1", JobId: "cc-gbeta",
		Job: &runpb.Job{LaunchStage: api.LaunchStage_BETA, Template: &runpb.ExecutionTemplate{Template: &runpb.TaskTemplate{
			Containers: []*runpb.Container{{Image: jobImage}}}}}})
	wantFailedPrecondition(t, "a job the gcloud constraint denies", err, customMessage(jobC, "No launch stage on jobs."))
}
