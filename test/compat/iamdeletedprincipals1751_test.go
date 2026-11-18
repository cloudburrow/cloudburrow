//go:build compat

package compat

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	logging "google.golang.org/api/logging/v2"
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

// covers: google.iam.admin.v1.IAM/DeleteServiceAccount, google.iam.admin.v1.IAM/UndeleteServiceAccount
//
// TestIAMDeletedServiceAccountInEveryServicesPolicies (#1751): a deleted
// account's bindings read deleted:serviceAccount:EMAIL?uid=ID, and
// serviceAccount:EMAIL again after undelete, in the allow policies every
// other service keeps on its own resources, each read and written through
// that service's official client: a Cloud Storage bucket (the storage
// server, another process), a Pub/Sub topic, subscription and schema, a
// Bigtable instance, a Spanner instance and a BigQuery table (their
// fronts, other processes, which the host reaches through their tunnels),
// a Logging view, a Cloud Run job and an Artifact Registry repository;
// and a Pub/Sub schema and a Bigtable table.
// Another member of the same binding is kept. (Purge, 30 days after the
// delete, drops the member: unit-tested, TestMemberRewriteHooks.)
func TestIAMDeletedServiceAccountInEveryServicesPolicies(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvPubSub)
	h.Endpoint(EnvBigtable)
	h.Endpoint(EnvSpanner)
	h.Endpoint(EnvBigQuery)
	h.Endpoint(EnvLogging)
	h.Endpoint(EnvRun)
	adm := iamAdmin(t, h)
	ctx := h.Context()
	project := iamProject(t, h, "dse-")
	victim := newServiceAccount(t, h, project, "every-svc")
	live := "serviceAccount:" + victim.GetEmail()
	deleted := "deleted:serviceAccount:" + victim.GetEmail() + "?uid=" + victim.GetUniqueId()
	const role = "roles/viewer"
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e8)
	res := map[string]policyRW{}

	// Cloud Storage.
	sc := storageClient(t, h)
	bucket := sc.Bucket("dse-" + suffix)
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("bucket Create: %v", err)
	}
	t.Cleanup(func() { _ = bucket.Delete(context.Background()) })
	res["bucket"] = handleRW(ctx, bucket.IAM())

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
