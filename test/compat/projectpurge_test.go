//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// purgeExtra is one more service's resource the purge test makes in the
// project, when the instance serves that service.
type purgeExtra struct {
	kind   string
	exists func() bool
}

// TestAPurgedProjectsIDStartsEmpty (#1537). Google frees a purged project's
// ID, and a project created again under it has none of the old one's state.
// Through the official clients: a project gets an org policy, an IAM
// binding, a lien, a tag binding and a project tag key, a disabled API (every
// API the instance serves starts ENABLED, so a change from that is what a
// project keeps) and a secret, and, where the instance serves them, a topic,
// a queue, a key ring, a Scheduler job, a Logging exclusion and a bucket.
// Deleted and undeleted it keeps all of it; deleted and purged
// (POST /admin/purge, in place of the 30 days) and created again with the
// same ID, none of it reads back.
func TestAPurgedProjectsIDStartsEmpty(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvServiceUsage)
	h.Endpoint(EnvSecrets)
	ctx := h.Context()
	served := func(env string) bool {
		if strings.TrimSpace(os.Getenv(env)) == "" {
			return false
		}
		h.Endpoint(env)
		return true
	}
	usePubSub, useTasks, useKMS := served(EnvPubSub), served(EnvTasks), served(EnvKMS)
	useScheduler, useLogging, useStorage := served(EnvScheduler), served(EnvLogging), served(EnvStorage)

	pc, err := resourcemanager.NewProjectsClient(ctx, rmOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	v1 := rmV1(t, h)
	tags := newTagClients(t, h)
	su := serviceUsageClient(t, h)
	sc := secretsClient(t, h)
	id := v1ID(h, "pg-")
	parent := "projects/" + id
	const boolC = "constraints/compute.requireOsLogin"
	const api = "secretmanager.googleapis.com"

	create := func() *rmpb.Project {
		t.Helper()
		op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id}})
		if err != nil {
			t.Fatalf("CreateProject %s: %v", id, err)
		}
		p, err := op.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := create()
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: parent})
	})

	// An organization tag, bound to the project, and a tag key of its own.
	kop, err := tags.keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{
		Parent: localOrg, ShortName: folderLabel(h, "pg")}})
	if err != nil {
		t.Fatal(err)
	}
	orgKey, err := kop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	vop, err := tags.values.CreateTagValue(ctx, &rmpb.CreateTagValueRequest{TagValue: &rmpb.TagValue{
		Parent: orgKey.GetName(), ShortName: "on"}})
	if err != nil {
		t.Fatal(err)
	}
	orgValue, err := vop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		if op, err := tags.values.DeleteTagValue(bg, &rmpb.DeleteTagValueRequest{Name: orgValue.GetName()}); err == nil {
			_, _ = op.Wait(bg)
		}
		if op, err := tags.keys.DeleteTagKey(bg, &rmpb.DeleteTagKeyRequest{Name: orgKey.GetName()}); err == nil {
			_, _ = op.Wait(bg)
		}
	})
	target := crm + p.GetName()
	bop, err := tags.bindings.CreateTagBinding(ctx, &rmpb.CreateTagBindingRequest{TagBinding: &rmpb.TagBinding{
		Parent: target, TagValue: orgValue.GetName()}})
	if err != nil {
		t.Fatalf("CreateTagBinding: %v", err)
	}
	if _, err := bop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	pkop, err := tags.keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: parent, ShortName: "team"}})
	if err != nil {
		t.Fatalf("CreateTagKey under the project: %v", err)
	}
	if _, err := pkop.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	// IAM, org policy, lien, Service Usage, a secret.
	if _, err := pc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: parent, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/viewer", Members: []string{"user:purge@example.com"}}}}}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	if _, err := v1.Projects.SetOrgPolicy(parent, &crmv1.SetOrgPolicyRequest{Policy: &crmv1.OrgPolicy{
		Constraint: boolC, BooleanPolicy: &crmv1.BooleanPolicy{Enforced: true}}}).Context(ctx).Do(); err != nil {
		t.Fatalf("setOrgPolicy: %v", err)
	}
	if _, err := v1.Liens.Create(&crmv1.Lien{Parent: parent, Restrictions: []string{"resourcemanager.projects.update"},
		Reason: "purge test", Origin: "compat"}).Context(ctx).Do(); err != nil {
		t.Fatalf("liens.create: %v", err)
	}
	if _, err := su.Services.Disable(parent+"/services/"+api, nil).Context(ctx).Do(); err != nil {
		t.Fatalf("services.disable: %v", err)
	}
	secret := parent + "/secrets/purged"
	if _, err := sc.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "purged",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: secret}) })

	notFound := func(kind string, err error) bool {
		t.Helper()
		switch status.Code(err) {
		case codes.OK:
			return true
		case codes.NotFound:
			return false
		}
		t.Fatalf("reading the %s: %v", kind, err)
		return false
	}
	var extras []purgeExtra
	if usePubSub {
		ps := pubsubClient(t, h)
		topic := parent + "/topics/purged"
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
			t.Fatalf("CreateTopic: %v", err)
		}
		t.Cleanup(func() {
			_ = ps.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: topic})
		})
		extras = append(extras, purgeExtra{"topic", func() bool {
			_, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
			return notFound("topic", err)
		}})
	}
	if useTasks {
		tc := tasksClient(t, h)
		q := parent + "/locations/us-central1/queues/purged"
		if _, err := tc.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{Parent: parent + "/locations/us-central1",
			Queue: &cloudtaskspb.Queue{Name: q}}); err != nil {
			t.Fatalf("CreateQueue: %v", err)
		}
		t.Cleanup(func() { _ = tc.DeleteQueue(context.Background(), &cloudtaskspb.DeleteQueueRequest{Name: q}) })
		extras = append(extras, purgeExtra{"queue", func() bool {
			_, err := tc.GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: q})
			return notFound("queue", err)
		}})
	}
	if useKMS {
		kc := kmsClients(t, h)["grpc"]
		ring := parent + "/locations/global/keyRings/purged"
		if _, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent + "/locations/global",
			KeyRingId: "purged", KeyRing: &kmspb.KeyRing{}}); err != nil {
			t.Fatalf("CreateKeyRing: %v", err)
		}
		extras = append(extras, purgeExtra{"key ring", func() bool {
			_, err := kc.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring})
			return notFound("key ring", err)
		}})
	}
	if useScheduler {
		jc := schedulerClient(t, h)
		job := parent + "/locations/us-central1/jobs/purged"
		if _, err := jc.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent + "/locations/us-central1",
			Job: &schedulerpb.Job{Name: job, Schedule: "0 0 1 1 *", Target: &schedulerpb.Job_HttpTarget{
				HttpTarget: &schedulerpb.HttpTarget{Uri: "http://127.0.0.1:9/purged"}}}}); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		t.Cleanup(func() { _ = jc.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: job}) })
		extras = append(extras, purgeExtra{"Scheduler job", func() bool {
			_, err := jc.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job})
			return notFound("Scheduler job", err)
		}})
	}
	if useLogging {
		lc := loggingConfigClient(t, h)
		ex := parent + "/exclusions/purged"
		if _, err := lc.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: parent,
			Exclusion: &loggingpb.LogExclusion{Name: "purged", Filter: "severity<ERROR"}}); err != nil {
			t.Fatalf("CreateExclusion: %v", err)
		}
		t.Cleanup(func() { _ = lc.DeleteExclusion(context.Background(), &loggingpb.DeleteExclusionRequest{Name: ex}) })
		extras = append(extras, purgeExtra{"Logging exclusion", func() bool {
			_, err := lc.GetExclusion(ctx, &loggingpb.GetExclusionRequest{Name: ex})
			return notFound("Logging exclusion", err)
		}})
	}
	if useStorage {
		bc := storageClient(t, h)
		b := bc.Bucket(id + "-purged")
		if err := b.Create(ctx, id, nil); err != nil {
			t.Fatalf("create bucket: %v", err)
		}
		t.Cleanup(func() { emptyAndDelete(context.Background(), b) })
		extras = append(extras, purgeExtra{"bucket", func() bool {
			_, err := b.Attrs(ctx)
			return err == nil
		}})
	}

	// What reads back: every piece that is there.
	left := func() []string {
		t.Helper()
		var out []string
		if pol, err := pc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: parent}); err != nil {
			t.Fatalf("GetIamPolicy: %v", err)
		} else if len(pol.GetBindings()) > 0 {
			out = append(out, "IAM binding")
		}
		if op, err := v1.Projects.GetOrgPolicy(parent, &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); err != nil {
			t.Fatalf("getOrgPolicy: %v", err)
		} else if op.BooleanPolicy != nil {
			out = append(out, "org policy")
		}
		if ls, err := v1.Liens.List().Parent(parent).Context(ctx).Do(); err != nil {
			t.Fatalf("liens.list: %v", err)
		} else if len(ls.Liens) > 0 {
			out = append(out, "lien")
		}
		if b, err := tags.bindings.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: target}).Next(); err == nil && b != nil {
			out = append(out, "tag binding")
		}
		if k, err := tags.keys.ListTagKeys(ctx, &rmpb.ListTagKeysRequest{Parent: parent}).Next(); err == nil && k != nil {
			out = append(out, "project tag key")
		}
		if serviceState(t, h, su, id, api) != "ENABLED" {
			out = append(out, "disabled API")
		}
		_, err := sc.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: secret})
		if notFound("secret", err) {
			out = append(out, "secret")
		}
		for _, e := range extras {
			if e.exists() {
				out = append(out, e.kind)
			}
		}
		return out
	}
	all := len(left())
	if want := 7 + len(extras); all != want {
		t.Fatalf("set up %d pieces of state, want %d: %v", all, want, left())
	}

	// Deleted and undeleted: everything is kept, as on Google.
	if _, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: parent}); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if code, body := adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, "not-"+id), "", nil); code != http.StatusNotFound {
		t.Errorf("purging a project that does not exist = %d %s, want 404", code, body)
	}
	if _, err := pc.UndeleteProject(ctx, &rmpb.UndeleteProjectRequest{Name: parent}); err != nil {
		t.Fatalf("UndeleteProject: %v", err)
	}
	if code, body := adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, id), "", nil); code != http.StatusBadRequest {
		t.Errorf("purging an ACTIVE project = %d %s, want 400 (FAILED_PRECONDITION)", code, body)
	}
	if got := left(); len(got) != all {
		t.Errorf("after undelete only %v read back, want all %d", got, all)
	}

	// Deleted and purged, and made again under the ID.
	if _, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: parent}); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if code, body := adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, id), "", nil); code != http.StatusOK {
		t.Fatalf("POST /admin/purge = %d %s", code, body)
	}
	if _, err := pc.GetProject(ctx, &rmpb.GetProjectRequest{Name: parent}); status.Code(err) != codes.NotFound {
		t.Errorf("GetProject after the purge = %v, want NotFound", err)
	}
	create()
	if got := left(); len(got) != 0 {
		t.Errorf("a project created again under a purged ID reads the old project's %v", got)
	}
}
