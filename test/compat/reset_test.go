//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func adminReset(t *testing.T, control, query string) (int, string) {
	t.Helper()
	return adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/reset?%s", control, query), "", nil)
}

// seedProject creates one queue, one topic with a subscription, and one secret
// in the harness's project, through the official clients. It returns their
// names.
func seedProject(t *testing.T, h *Harness) (queueName, topicName, subName, secretName string) {
	t.Helper()
	queueName = queue(t, h, tasksClient(t, h), "reset-q")

	pc := pubsubClient(t, h)
	topicName = topic(t, h, pc, "reset-topic")
	subName = fmt.Sprintf("projects/%s/subscriptions/reset-sub", h.Project())
	if _, err := pc.SubscriptionAdminClient.CreateSubscription(h.Context(),
		&pubsubpb.Subscription{Name: subName, Topic: topicName}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	sc := secretsClient(t, h)
	sec, err := sc.CreateSecret(h.Context(), &secretmanagerpb.CreateSecretRequest{
		Parent: secretsParent(h), SecretId: "reset-secret",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteSecret(h.Context(), &secretmanagerpb.DeleteSecretRequest{Name: sec.Name}) })
	return queueName, topicName, subName, sec.Name
}

// present reports which of a project's seeded resources the official clients
// can still read, failing on any error other than NOT_FOUND.
func present(t *testing.T, h *Harness, queueName, topicName, subName, secretName string) []string {
	t.Helper()
	var found []string
	check := func(kind string, err error) {
		t.Helper()
		switch status.Code(err) {
		case codes.OK:
			found = append(found, kind)
		case codes.NotFound:
		default:
			t.Fatalf("reading the %s: %v", kind, err)
		}
	}
	_, err := tasksClient(t, h).GetQueue(h.Context(), &cloudtaskspb.GetQueueRequest{Name: queueName})
	check("queue", err)
	pc := pubsubClient(t, h)
	_, err = pc.TopicAdminClient.GetTopic(h.Context(), &pubsubpb.GetTopicRequest{Topic: topicName})
	check("topic", err)
	_, err = pc.SubscriptionAdminClient.GetSubscription(h.Context(), &pubsubpb.GetSubscriptionRequest{Subscription: subName})
	check("subscription", err)
	_, err = secretsClient(t, h).GetSecret(h.Context(), &secretmanagerpb.GetSecretRequest{Name: secretName})
	check("secret", err)
	return found
}

// TestAProjectResetClearsOnlyThatProject.
//
// /admin/reset used to clear Cloud Tasks and nothing else (#274). Resetting one
// project must now clear its queues, topics, subscriptions and secrets, as the
// official clients see them, and leave a second project's untouched.
func TestAProjectResetClearsOnlyThatProject(t *testing.T) {
	target, bystander := New(t), New(t)
	control := target.Endpoint(EnvControl)
	if target.Project() == bystander.Project() {
		t.Fatal("the two harnesses share a project; the isolation check would prove nothing")
	}

	tq, tt, ts, tsec := seedProject(t, target)
	bq, bt, bs, bsec := seedProject(t, bystander)

	code, body := adminReset(t, control, "service=tasks,pubsub,secretmanager&project="+target.Project())
	if code != http.StatusOK {
		t.Fatalf("reset: %d %s", code, body)
	}

	if left := present(t, target, tq, tt, ts, tsec); len(left) != 0 {
		t.Errorf("after resetting %s its %v survived", target.Project(), left)
	}
	if kept := present(t, bystander, bq, bt, bs, bsec); strings.Join(kept, ",") != "queue,topic,subscription,secret" {
		t.Errorf("resetting %s removed another project's state: only %v remain", target.Project(), kept)
	}
}

// TestAProjectResetIncludingStorage (#510): the storage server records each
// bucket's project, so a project-scoped reset clears that project's buckets
// and leaves another project's alone.
func TestAProjectResetIncludingStorage(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	q, tp, sub, sec := seedProject(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	mine := sc.Bucket(h.Project() + "-reset-mine")
	if err := mine.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}

	other := New(t)
	theirs := sc.Bucket(other.Project() + "-reset-theirs")
	if err := theirs.Create(ctx, other.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(context.Background(), theirs) })
	// Named services: an instance may also run ones that cannot reset by
	// project (Cloud SQL for MySQL, Scheduler, Logging), which would refuse
	// the whole request.
	if code, body := adminReset(t, control, "service=storage,tasks,pubsub,secretmanager&project="+h.Project()); code != http.StatusOK {
		t.Fatalf("a project reset including storage = %d %s", code, body)
	}
	if _, err := mine.Attrs(ctx); err == nil {
		t.Error("the reset project's bucket survived")
	}
	if kept := present(t, h, q, tp, sub, sec); len(kept) != 0 {
		t.Errorf("the project reset left %v behind", kept)
	}
	if _, err := theirs.Attrs(ctx); err != nil {
		t.Errorf("another project's bucket was removed: %v", err)
	}
}
