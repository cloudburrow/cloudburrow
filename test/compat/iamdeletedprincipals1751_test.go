//go:build compat

package compat

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/iam"
	admin "cloud.google.com/go/iam/admin/apiv1"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	bqv2 "google.golang.org/api/bigquery/v2"
	logging "google.golang.org/api/logging/v2"
	storagev1 "google.golang.org/api/storage/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// policyRW reads and writes one resource's allow policy through its
// service's official client.
type policyRW struct {
	get func() (*iampb.Policy, error)
	set func(*iampb.Policy) error
}

// handleRW is a policyRW over a client's iam.Handle.
func handleRW(ctx context.Context, h *iam.Handle) policyRW {
	return policyRW{
		get: func() (*iampb.Policy, error) {
			p, err := h.Policy(ctx)
			if err != nil {
				return nil, err
			}
			return p.InternalProto, nil
		},
		set: func(p *iampb.Policy) error { return h.SetPolicy(ctx, &iam.Policy{InternalProto: p}) },
	}
}

// deletedAccountFixture is a service account in a project of its own,
// whose deletion the tests below watch in other services' policies, the
// IAM admin client, and a suffix for the resources' names.
func deletedAccountFixture(t *testing.T, h *Harness) (*admin.IamClient, *adminpb.ServiceAccount, string) {
	t.Helper()
	adm := iamAdmin(t, h)
	project := iamProject(t, h, "dse-")
	return adm, newServiceAccount(t, h, project, "every-svc"), fmt.Sprint(time.Now().UnixNano() % 1e8)
}

// deletedAccountRoundTrip grants victim and another member roles/viewer on
// every resource of res, through each service's official client, and
// checks that each binding reads deleted:serviceAccount:EMAIL?uid=ID after
// the delete and serviceAccount:EMAIL again after undelete, the other
// member kept. Each of also runs after each of those checks, with the
// phase's name and whether the account is deleted then.
func deletedAccountRoundTrip(t *testing.T, h *Harness, adm *admin.IamClient, victim *adminpb.ServiceAccount, res map[string]policyRW, also ...func(when string, isDeleted bool)) {
	t.Helper()
	ctx := h.Context()
	live := "serviceAccount:" + victim.GetEmail()
	deleted := "deleted:serviceAccount:" + victim.GetEmail() + "?uid=" + victim.GetUniqueId()
	const role = "roles/viewer"
	for name, rw := range res {
		p, err := rw.get()
		if err != nil {
			t.Fatalf("%s GetIamPolicy: %v", name, err)
		}
		pol := &iam.Policy{InternalProto: p}
		pol.Add(live, role)
		pol.Add("user:keep@example.com", role)
		if err := rw.set(pol.InternalProto); err != nil {
			t.Fatalf("%s SetIamPolicy: %v", name, err)
		}
	}
	check := func(when, want string) {
		t.Helper()
		defer func() {
			for _, f := range also {
				f(when, want == deleted)
			}
		}()
		for name, rw := range res {
			p, err := rw.get()
			if err != nil {
				t.Fatalf("%s GetIamPolicy: %v", name, err)
			}
			members := (&iam.Policy{InternalProto: p}).Members(role)
			if !slices.Contains(members, want) || !slices.Contains(members, "user:keep@example.com") || len(members) != 2 {
				t.Errorf("%s, %s: members %q, want %q and user:keep@example.com", when, name, members, want)
			}
		}
	}
	check("before delete", live)
	if err := adm.DeleteServiceAccount(ctx, &adminpb.DeleteServiceAccountRequest{Name: victim.GetName()}); err != nil {
		t.Fatal(err)
	}
	check("after delete", deleted)
	if _, err := adm.UndeleteServiceAccount(ctx, &adminpb.UndeleteServiceAccountRequest{Name: "projects/-/serviceAccounts/" + victim.GetUniqueId()}); err != nil {
		t.Fatal(err)
	}
	check("after undelete", live)
}

// covers: google.iam.admin.v1.IAM/DeleteServiceAccount, google.iam.admin.v1.IAM/UndeleteServiceAccount
//
// TestIAMDeletedServiceAccountInEveryServicesPolicies (#1751): a deleted
// account's bindings read deleted:serviceAccount:EMAIL?uid=ID, and
// serviceAccount:EMAIL again after undelete, in the allow policies the
// other services keep on their own resources, each read and written
// through that service's official client: here a Cloud Storage bucket and
// managed folder (the storage server, another process), a Pub/Sub topic,
// subscription, snapshot and schema, and a Logging view; Bigtable, Spanner
// and BigQuery in TestIAMDeletedServiceAccountInEmulatorFrontsPolicies,
// and Cloud Run, Artifact Registry and Secure Source Manager in
// TestIAMDeletedServiceAccountInRunShardPolicies, so each runs in the CI
// shard that serves its services (#1761), and a BigQuery connection in
// TestIAMDeletedServiceAccountInConnectionPolicies (#1795). Another member
// of the same binding is kept. (Purge, 30 days after the delete, drops the
// member: unit-tested, TestMemberRewriteHooks and
// TestConnectionsRewriteMember.)
func TestIAMDeletedServiceAccountInEveryServicesPolicies(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvPubSub)
	// The storage shard serves Logging under the name Cloud Tasks' logs
	// test reads, so the Logging suite stays in served.
	shardEndpoint(h, "CLOUDBURROW_TEST_TASKS_LOGGING", EnvLogging)
	adm, victim, suffix := deletedAccountFixture(t, h)
	ctx := h.Context()
	res := map[string]policyRW{}

	// Cloud Storage.
	sc := storageClient(t, h)
	bucket := sc.Bucket("dse-" + suffix)
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("bucket Create: %v", err)
	}
	t.Cleanup(func() { _ = bucket.Delete(context.Background()) })
	res["bucket"] = handleRW(ctx, bucket.IAM())

	// A managed folder, over the JSON API.
	sj := storageJSON(t, h)
	mfb := managedFolderBucket(t, h, sc, sj).BucketName()
	if _, err := sj.ManagedFolders.Insert(mfb, &storagev1.ManagedFolder{Name: "dse-" + suffix + "/"}).Context(ctx).Do(); err != nil {
		t.Fatalf("managedFolders.insert: %v", err)
	}
	mf := "dse-" + suffix + "/"
	res["managed folder"] = policyRW{
		get: func() (*iampb.Policy, error) {
			p, err := sj.ManagedFolders.GetIamPolicy(mfb, mf).Context(ctx).Do()
			if err != nil {
				return nil, err
			}
			out := &iampb.Policy{Version: int32(p.Version), Etag: []byte(p.Etag)}
			for _, b := range p.Bindings {
				out.Bindings = append(out.Bindings, &iampb.Binding{Role: b.Role, Members: b.Members})
			}
			return out, nil
		},
		set: func(p *iampb.Policy) error {
			in := &storagev1.Policy{Etag: string(p.GetEtag())}
			for _, b := range p.GetBindings() {
				in.Bindings = append(in.Bindings, &storagev1.PolicyBindings{Role: b.GetRole(), Members: b.GetMembers()})
			}
			_, err := sj.ManagedFolders.SetIamPolicy(mfb, mf, in).Context(ctx).Do()
			return err
		},
	}

	// Pub/Sub.
	pc := pubsubClient(t, h)
	topic, err := pc.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: "projects/" + h.Project() + "/topics/dse-" + suffix})
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	t.Cleanup(func() {
		_ = pc.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: topic.GetName()})
	})
	sub, err := pc.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: "projects/" + h.Project() + "/subscriptions/dse-" + suffix, Topic: topic.GetName()})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	t.Cleanup(func() {
		_ = pc.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: sub.GetName()})
	})
	res["topic"] = policyRW{
		get: func() (*iampb.Policy, error) {
			return pc.TopicAdminClient.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: topic.GetName()})
		},
		set: func(p *iampb.Policy) error {
			_, err := pc.TopicAdminClient.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: topic.GetName(), Policy: p})
			return err
		},
	}
	res["subscription"] = policyRW{
		get: func() (*iampb.Policy, error) {
			return pc.SubscriptionAdminClient.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: sub.GetName()})
		},
		set: func(p *iampb.Policy) error {
			_, err := pc.SubscriptionAdminClient.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: sub.GetName(), Policy: p})
			return err
		},
	}

	snap, err := pc.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: "projects/" + h.Project() + "/snapshots/dse-" + suffix, Subscription: sub.GetName()})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	t.Cleanup(func() {
		_ = pc.SubscriptionAdminClient.DeleteSnapshot(context.Background(), &pubsubpb.DeleteSnapshotRequest{Snapshot: snap.GetName()})
	})
	res["snapshot"] = policyRW{
		get: func() (*iampb.Policy, error) {
			return pc.SubscriptionAdminClient.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: snap.GetName()})
		},
		set: func(p *iampb.Policy) error {
			_, err := pc.SubscriptionAdminClient.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: snap.GetName(), Policy: p})
			return err
		},
	}
	sch := schemaClient(t, h)
	schema, err := sch.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + h.Project(), SchemaId: "dse-" + suffix,
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: avroSchemaV1}})
	if err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	t.Cleanup(func() {
		_ = sch.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: schema.GetName()})
	})
	res["schema"] = policyRW{
		get: func() (*iampb.Policy, error) {
			return sch.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: schema.GetName()})
		},
		set: func(p *iampb.Policy) error {
			_, err := sch.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: schema.GetName(), Policy: p})
			return err
		},
	}

	// Logging, over its JSON API (the IAMPolicy mixin is not in the
	// logging protos).
	views := loggingJSONService(t, h).Projects.Locations.Buckets.Views
	view := loggingView(t, h, "dse-"+suffix)
	res["log view"] = policyRW{
		get: func() (*iampb.Policy, error) {
			p, err := views.GetIamPolicy(view, &logging.GetIamPolicyRequest{}).Context(ctx).Do()
			if err != nil {
				return nil, err
			}
			out := &iampb.Policy{Version: int32(p.Version), Etag: []byte(p.Etag)}
			for _, b := range p.Bindings {
				out.Bindings = append(out.Bindings, &iampb.Binding{Role: b.Role, Members: b.Members})
			}
			return out, nil
		},
		set: func(p *iampb.Policy) error {
			in := &logging.Policy{Etag: string(p.GetEtag())}
			for _, b := range p.GetBindings() {
				in.Bindings = append(in.Bindings, &logging.Binding{Role: b.GetRole(), Members: b.GetMembers()})
			}
			_, err := views.SetIamPolicy(view, &logging.SetIamPolicyRequest{Policy: in}).Context(ctx).Do()
			return err
		},
	}

	deletedAccountRoundTrip(t, h, adm, victim, res)
}

// covers: google.iam.admin.v1.IAM/DeleteServiceAccount, google.iam.admin.v1.IAM/UndeleteServiceAccount
//
// TestIAMDeletedServiceAccountInEmulatorFrontsPolicies (#1751): as
// TestIAMDeletedServiceAccountInEveryServicesPolicies, for a Bigtable
// instance, table, authorized view and backup, a Spanner instance,
// database and backup, and a BigQuery table and routine (their fronts,
// other processes, which the host reaches through their tunnels), in the
// emulators shard. A BigQuery dataset's access list names the account as
// userByEmail, iamMember deleted:serviceAccount:EMAIL?uid=ID once it is
// deleted (the shape is UNVERIFIED, re-read 2026-11-17:
// internal/bigqueryfront/datasetaccessrewrite.go) and userByEmail again
// after undelete; another entry stays.
func TestIAMDeletedServiceAccountInEmulatorFrontsPolicies(t *testing.T) {
	h := New(t)
	shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	h.Endpoint(EnvBigtable)
	h.Endpoint(EnvSpanner)
	h.Endpoint(EnvBigQuery)
	adm, victim, suffix := deletedAccountFixture(t, h)
	ctx := h.Context()
	res := map[string]policyRW{}

	// Bigtable.
	btAdmin, _ := bigtableTable(t, h, "dse-"+suffix)
	res["bigtable table"] = handleRW(ctx, btAdmin.TableIAM("dse-"+suffix))
	iac := bigtableInstanceAdmin(t, h)
	btID := "dse-bt-" + suffix
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: btID, DisplayName: "dse bigtable", ClusterId: btID + "-c", Zone: "europe-west1-b", NumNodes: 1}); err != nil {
		t.Fatalf("Bigtable CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(context.Background(), btID) })
	res["bigtable instance"] = handleRW(ctx, iac.InstanceIAM(btID))

	// Spanner.
	sp, err := instance.NewInstanceAdminClient(ctx, spannerOpts(h.Endpoint(EnvSpanner))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	spID := "dse-sp-" + suffix
	op, err := sp.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: "projects/" + h.Project(), InstanceId: spID,
		Instance: &instancepb.Instance{Config: "projects/" + h.Project() + "/instanceConfigs/emulator-config", DisplayName: "dse", NodeCount: 1}})
	if err != nil {
		t.Fatalf("Spanner CreateInstance: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	spName := "projects/" + h.Project() + "/instances/" + spID
	t.Cleanup(func() { _ = sp.DeleteInstance(context.Background(), &instancepb.DeleteInstanceRequest{Name: spName}) })
	res["spanner instance"] = policyRW{
		get: func() (*iampb.Policy, error) {
			return sp.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: spName})
		},
		set: func(p *iampb.Policy) error {
			_, err := sp.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: spName, Policy: p})
			return err
		},
	}

	// A Spanner database.
	dbName := spannerDatabase(t, ctx, h.Endpoint(EnvSpanner), h.Project(), spID, "dse", "CREATE TABLE t (id INT64) PRIMARY KEY (id)")
	dba, err := database.NewDatabaseAdminClient(ctx, spannerOpts(h.Endpoint(EnvSpanner))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dba.Close() })
	res["spanner database"] = policyRW{
		get: func() (*iampb.Policy, error) {
			return dba.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: dbName})
		},
		set: func(p *iampb.Policy) error {
			_, err := dba.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: dbName, Policy: p})
			return err
		},
	}

	// A Spanner backup of that database.
	bop, err := dba.CreateBackup(ctx, &databasepb.CreateBackupRequest{Parent: spName, BackupId: "dse-bk",
		Backup: &databasepb.Backup{Database: dbName, ExpireTime: timestamppb.New(time.Now().Add(48 * time.Hour))}})
	if err != nil {
		t.Fatalf("Spanner CreateBackup: %v", err)
	}
	spBackup, err := bop.Wait(ctx)
	if err != nil {
		t.Fatalf("Spanner CreateBackup.Wait: %v", err)
	}
	t.Cleanup(func() {
		_ = dba.DeleteBackup(context.Background(), &databasepb.DeleteBackupRequest{Name: spBackup.GetName()})
	})
	res["spanner backup"] = policyRW{
		get: func() (*iampb.Policy, error) {
			return dba.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: spBackup.GetName()})
		},
		set: func(p *iampb.Policy) error {
			_, err := dba.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: spBackup.GetName(), Policy: p})
			return err
		},
	}

	// BigQuery.
	bq, bqProject := bigqueryClient(t, h)
	ds := bq.DatasetInProject(bqProject, "dse_"+suffix)
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatalf("dataset Create: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(context.Background()) })
	tbl := ds.Table("t")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatalf("table Create: %v", err)
	}
	res["bigquery table"] = handleRW(ctx, tbl.IAM())

	// A Bigtable authorized view and backup of the table.
	sv := &bigtable.SubsetViewConf{}
	sv.AddRowPrefix([]byte("eu#"))
	if err := btAdmin.CreateAuthorizedView(ctx, &bigtable.AuthorizedViewConf{TableID: "dse-" + suffix, AuthorizedViewID: "v", AuthorizedView: sv}); err != nil {
		t.Fatalf("CreateAuthorizedView: %v", err)
	}
	res["bigtable authorized view"] = handleRW(ctx, btAdmin.AuthorizedViewIAM("dse-"+suffix, "v"))
	btBackup := "dse-bk-" + suffix
	if err := btAdmin.CreateBackup(ctx, "dse-"+suffix, "c1", btBackup, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("Bigtable CreateBackup: %v", err)
	}
	t.Cleanup(func() { _ = btAdmin.DeleteBackup(context.Background(), "c1", btBackup) })
	res["bigtable backup"] = handleRW(ctx, btAdmin.BackupIAM("c1", btBackup))

	// A BigQuery routine, through the generated REST client (the Go client
	// has no routine IAM call).
	if _, err := bq.Query("CREATE FUNCTION `" + bqProject + ".dse_" + suffix + ".plus_one`(x INT64) AS (x + 1)").Read(ctx); err != nil {
		t.Fatalf("CREATE FUNCTION: %v", err)
	}
	bqs := bigqueryService(t, h)
	routine := "projects/" + bqProject + "/datasets/dse_" + suffix + "/routines/plus_one"
	res["bigquery routine"] = policyRW{
		get: func() (*iampb.Policy, error) {
			p, err := bqs.Routines.GetIamPolicy(routine, &bqv2.GetIamPolicyRequest{}).Context(ctx).Do()
			if err != nil {
				return nil, err
			}
			out := &iampb.Policy{Version: int32(p.Version), Etag: []byte(p.Etag)}
			for _, b := range p.Bindings {
				out.Bindings = append(out.Bindings, &iampb.Binding{Role: b.Role, Members: b.Members})
			}
			return out, nil
		},
		set: func(p *iampb.Policy) error {
			in := &bqv2.Policy{Etag: string(p.GetEtag())}
			for _, b := range p.GetBindings() {
				in.Bindings = append(in.Bindings, &bqv2.Binding{Role: b.GetRole(), Members: b.GetMembers()})
			}
			_, err := bqs.Routines.SetIamPolicy(routine, &bqv2.SetIamPolicyRequest{Policy: in}).Context(ctx).Do()
			return err
		},
	}

	// The dataset's own access list: the account as userByEmail (the
	// datasets reference's field for serviceAccount:EMAIL), beside another
	// entry that stays.
	md, err := ds.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ds.Update(ctx, bigquery.DatasetMetadataToUpdate{Access: append(md.Access,
		&bigquery.AccessEntry{Role: bigquery.ReaderRole, EntityType: bigquery.UserEmailEntity, Entity: victim.GetEmail()},
		&bigquery.AccessEntry{Role: bigquery.ReaderRole, EntityType: bigquery.UserEmailEntity, Entity: "keep@example.com"})}, md.ETag); err != nil {
		t.Fatalf("dataset Update: %v", err)
	}
	checkAccess := func(when string, wantType bigquery.EntityType, want string) {
		t.Helper()
		md, err := ds.Metadata(ctx)
		if err != nil {
			t.Fatalf("%s: dataset Metadata: %v", when, err)
		}
		var found, kept, stale bool
		for _, a := range md.Access {
			switch {
			case a.EntityType == wantType && a.Entity == want && a.Role == bigquery.ReaderRole:
				found = true
			case a.EntityType == bigquery.UserEmailEntity && a.Entity == "keep@example.com":
				kept = true
			case strings.Contains(a.Entity, victim.GetEmail()):
				stale = true
			}
		}
		if !found || !kept || stale {
			var got []string
			for _, a := range md.Access {
				got = append(got, fmt.Sprintf("%s %d %s", a.Role, a.EntityType, a.Entity))
			}
			t.Errorf("%s: dataset access %q, want READER %d %s and keep@example.com", when, got, wantType, want)
		}
	}

	deletedAccountRoundTrip(t, h, adm, victim, res, func(when string, isDeleted bool) {
		if isDeleted {
			checkAccess(when, bigquery.IAMMemberEntity, "deleted:serviceAccount:"+victim.GetEmail()+"?uid="+victim.GetUniqueId())
		} else {
			checkAccess(when, bigquery.UserEmailEntity, victim.GetEmail())
		}
	})
}

// covers: google.iam.admin.v1.IAM/DeleteServiceAccount, google.iam.admin.v1.IAM/UndeleteServiceAccount
//
// TestIAMDeletedServiceAccountInRunShardPolicies (#1751): as
// TestIAMDeletedServiceAccountInEveryServicesPolicies, for a Cloud Run
// job, service and worker pool, an Artifact Registry repository and a
// Secure Source Manager instance and repository, in the run shard.
func TestIAMDeletedServiceAccountInRunShardPolicies(t *testing.T) {
	h := New(t)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_RESOURCEMANAGER", EnvResourceManager)
	h.Endpoint(EnvRun)
	adm, victim, suffix := deletedAccountFixture(t, h)
	ctx := h.Context()
	res := map[string]policyRW{}

	// Cloud Run and Artifact Registry.
	jobs, err := run.NewJobsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jobs.Close() })
	jop, err := jobs.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: "dse-" + suffix, Job: &runpb.Job{
		Template: &runpb.ExecutionTemplate{Template: &runpb.TaskTemplate{Containers: []*runpb.Container{{Image: "us-docker.pkg.dev/cloudrun/container/job:latest"}}}}}})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	job, err := jop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateJob.Wait: %v", err)
	}
	t.Cleanup(func() {
		if op, err := jobs.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: job.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	res["run job"] = policyRW{
		get: func() (*iampb.Policy, error) {
			return jobs.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: job.GetName()})
		},
		set: func(p *iampb.Policy) error {
			_, err := jobs.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: job.GetName(), Policy: p})
			return err
		},
	}
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ar.Close() })
	rop, err := ar.CreateRepository(ctx, &artifactregistrypb.CreateRepositoryRequest{Parent: runParent(h), RepositoryId: "dse-" + suffix,
		Repository: &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateRepository.Wait: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ar.DeleteRepository(context.Background(), &artifactregistrypb.DeleteRepositoryRequest{Name: repo.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	res["repository"] = policyRW{
		get: func() (*iampb.Policy, error) {
			return ar.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: repo.GetName()})
		},
		set: func(p *iampb.Policy) error {
			_, err := ar.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: repo.GetName(), Policy: p})
			return err
		},
	}

	// Secure Source Manager.
	ssm := ssmClient(t, h)
	iop, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: runParent(h), InstanceId: "dse-" + suffix})
	if err != nil {
		t.Fatalf("SSM CreateInstance: %v", err)
	}
	inst, err := iop.Wait(ctx)
	if err != nil {
		t.Fatalf("SSM CreateInstance.Wait: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ssm.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: inst.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	srop, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: runParent(h), RepositoryId: "r-" + suffix,
		Repository: &ssmpb.Repository{Instance: inst.GetName()}})
	if err != nil {
		t.Fatalf("SSM CreateRepository: %v", err)
	}
	srepo, err := srop.Wait(ctx)
	if err != nil {
		t.Fatalf("SSM CreateRepository.Wait: %v", err)
	}
	res["ssm instance"] = policyRW{
		get: func() (*iampb.Policy, error) {
			return ssm.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: inst.GetName()})
		},
		set: func(p *iampb.Policy) error {
			_, err := ssm.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: inst.GetName(), Policy: p})
			return err
		},
	}
	res["ssm repository"] = policyRW{
		get: func() (*iampb.Policy, error) {
			return ssm.GetIamPolicyRepo(ctx, &iampb.GetIamPolicyRequest{Resource: srepo.GetName()})
		},
		set: func(p *iampb.Policy) error {
			_, err := ssm.SetIamPolicyRepo(ctx, &iampb.SetIamPolicyRequest{Resource: srepo.GetName(), Policy: p})
			return err
		},
	}

	// A Cloud Run service and worker pool.
	services, err := run.NewServicesClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = services.Close() })
	sop, err := services.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: "dse-" + suffix,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: "ghcr.io/knative/helloworld-go:latest"}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	svc, err := sop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateService.Wait: %v", err)
	}
	t.Cleanup(func() {
		_, _ = services.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: svc.GetName()})
	})
	res["run service"] = policyRW{
		get: func() (*iampb.Policy, error) {
			return services.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: svc.GetName()})
		},
		set: func(p *iampb.Policy) error {
			_, err := services.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: svc.GetName(), Policy: p})
			return err
		},
	}
	pools, err := run.NewWorkerPoolsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	one := int32(1)
	pop, err := pools.CreateWorkerPool(ctx, &runpb.CreateWorkerPoolRequest{Parent: runParent(h), WorkerPoolId: "dse-" + suffix,
		WorkerPool: &runpb.WorkerPool{Template: &runpb.WorkerPoolRevisionTemplate{Containers: sleeper("dse")},
			Scaling: &runpb.WorkerPoolScaling{ManualInstanceCount: &one}}})
	if err != nil {
		t.Fatalf("CreateWorkerPool: %v", err)
	}
	pool, err := pop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateWorkerPool.Wait: %v", err)
	}
	t.Cleanup(func() {
		if op, err := pools.DeleteWorkerPool(context.Background(), &runpb.DeleteWorkerPoolRequest{Name: pool.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	res["run worker pool"] = policyRW{
		get: func() (*iampb.Policy, error) {
			return pools.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: pool.GetName()})
		},
		set: func(p *iampb.Policy) error {
			_, err := pools.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: pool.GetName(), Policy: p})
			return err
		},
	}

	deletedAccountRoundTrip(t, h, adm, victim, res)
}

// covers: google.iam.admin.v1.IAM/DeleteServiceAccount, google.iam.admin.v1.IAM/UndeleteServiceAccount, bigqueryconnection.projects.locations.connections.getIamPolicy
//
// TestIAMDeletedServiceAccountInConnectionPolicies (#1795): as
// TestIAMDeletedServiceAccountInEveryServicesPolicies, for a BigQuery
// Connection API connection's allow policy (the BigQuery front, another
// process), read and written through the official
// cloud.google.com/go/bigquery/connection/apiv1 client, in the shard that
// serves BigQuery: deleted:serviceAccount:EMAIL?uid=ID after the delete,
// serviceAccount:EMAIL again after undelete, the binding's other member
// kept.
func TestIAMDeletedServiceAccountInConnectionPolicies(t *testing.T) {
	h := New(t)
	shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	cc, project := remoteConnectionClient(t, h)
	adm, victim, suffix := deletedAccountFixture(t, h)
	ctx := h.Context()
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us",
		ConnectionId: "dse_" + strings.ReplaceAll(suffix, "-", "_"),
		Connection:   &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() {
		_ = cc.DeleteConnection(context.Background(), &connectionpb.DeleteConnectionRequest{Name: cn.GetName()})
	})
	deletedAccountRoundTrip(t, h, adm, victim, map[string]policyRW{"bigquery connection": {
		get: func() (*iampb.Policy, error) {
			return cc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: cn.GetName()})
		},
		set: func(p *iampb.Policy) error {
			_, err := cc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: cn.GetName(), Policy: p})
			return err
		},
	}})
}
