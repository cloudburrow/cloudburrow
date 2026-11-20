//go:build compat

package compat

// Pub/Sub export subscriptions under IAM enforcement (#1998), through the
// official Pub/Sub, BigQuery, Cloud Storage and IAM clients: the account a
// BigQuery or Cloud Storage subscription writes as, the Pub/Sub service
// agent or its service_account_email, needs the roles Google documents
// (https://docs.cloud.google.com/pubsub/docs/create-bigquery-subscription,
// https://docs.cloud.google.com/pubsub/docs/create-cloudstorage-subscription):
// without them CreateSubscription is refused with the troubleshooting
// pages' text, a subscription whose account loses one reads
// PERMISSION_DENIED with its messages kept, and it is ACTIVE again once
// the role is back; a service_account_email needs actAs of its caller.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pubsubAgent1998 is the Pub/Sub service agent of project.
func pubsubAgent1998(t *testing.T, h *Harness, project string) string {
	t.Helper()
	p, err := rmV1(t, h).Projects.Get(project).Context(h.Context()).Do()
	if err != nil {
		t.Fatalf("projects.get %s: %v", project, err)
	}
	return fmt.Sprintf("service-%d@gcp-sa-pubsub.iam.gserviceaccount.com", p.ProjectNumber)
}

// handleRole adds (or removes) role for member on an IAM handle.
func handleRole(t *testing.T, h *Harness, hd *iam.Handle, member, role string, add bool) {
	t.Helper()
	p, err := hd.Policy(h.Context())
	if err != nil {
		t.Fatalf("getIamPolicy: %v", err)
	}
	if add {
		p.Add(member, iam.RoleName(role))
	} else {
		p.Remove(member, iam.RoleName(role))
	}
	if err := hd.SetPolicy(h.Context(), p); err != nil {
		t.Fatalf("setIamPolicy %s %s: %v", role, member, err)
	}
}

func wantCreateRefused(t *testing.T, what string, err error, code codes.Code, msg string) {
	t.Helper()
	if st, _ := status.FromError(err); st.Code() != code || !strings.Contains(st.Message(), msg) {
		t.Fatalf("%s = %v; want %s containing %q", what, err, code, msg)
	}
}

// createEventually retries a CreateSubscription a grant has just allowed.
func createEventually(t *testing.T, h *Harness, ps *pubsub.Client, s *pubsubpb.Subscription) {
	t.Helper()
	var err error
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		if _, err = ps.SubscriptionAdminClient.CreateSubscription(h.Context(), s); err == nil {
			t.Cleanup(func() {
				_ = ps.SubscriptionAdminClient.DeleteSubscription(h.Context(), &pubsubpb.DeleteSubscriptionRequest{Subscription: s.GetName()})
			})
			return
		}
	}
	t.Fatalf("CreateSubscription %s after the grant: %v", s.GetName(), err)
}

func newTopic1998(t *testing.T, h *Harness, ps *pubsub.Client, project, id string) string {
	t.Helper()
	topic := "projects/" + project + "/topics/" + id
	if _, err := ps.TopicAdminClient.CreateTopic(h.Context(), &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.TopicAdminClient.DeleteTopic(h.Context(), &pubsubpb.DeleteTopicRequest{Topic: topic}) })
	return topic
}

// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Publisher/Publish
//
// TestIAMEnforcementPubSubBigQueryServiceAgentRoles (#1998): a BigQuery
// subscription written by the Pub/Sub service agent is refused while the
// agent lacks BigQuery Data Editor on the table ("Service account
// service-N@gcp-sa-pubsub.iam.gserviceaccount.com is missing permissions
// required to write to the BigQuery table: bigquery.tables.get,
// bigquery.tables.updateData."), created once the table's policy grants
// it, PERMISSION_DENIED when the role is removed, its message kept, and
// ACTIVE again, the message written, when it is granted back.
func TestIAMEnforcementPubSubBigQueryServiceAgentRoles(t *testing.T) {
	h := New(t)
	bq, bqProject := bigqueryClient(t, h)
	ps, _ := exportClients(t, h)
	h.Endpoint(EnvResourceManager)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "psbq-")
	agent := "serviceAccount:" + pubsubAgent1998(t, h, project)
	ds := bqDataset1874(t, h, bq, "agent1998", "")
	tb := ds.Table("t")
	if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: dataSchema1874}); err != nil {
		t.Fatal(err)
	}
	topic := newTopic1998(t, h, ps, project, "bq-agent")
	s := &pubsubpb.Subscription{Name: "projects/" + project + "/subscriptions/bq-agent", Topic: topic,
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: bqProject + "." + ds.DatasetID + ".t"}}
	_, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, s)
	wantCreateRefused(t, "CreateSubscription without BigQuery Data Editor", err, codes.FailedPrecondition,
		"Service account "+strings.TrimPrefix(agent, "serviceAccount:")+" is missing permissions required to write to the BigQuery table: bigquery.tables.get, bigquery.tables.updateData.")

	handleRole(t, h, tb.IAM(), agent, "roles/bigquery.dataEditor", true)
	createEventually(t, h, ps, s)
	waitExportState(t, h, ps, s.GetName(), "ACTIVE")

	handleRole(t, h, tb.IAM(), agent, "roles/bigquery.dataEditor", false)
	waitExportState(t, h, ps, s.GetName(), "PERMISSION_DENIED")
	publish1874(t, h, ps, topic, "while-denied")
	time.Sleep(4 * time.Second)
	if rows := bqRows1874(t, h, tb); len(rows) != 0 {
		t.Fatalf("rows written while the agent lacks the role: %v", rows)
	}
	handleRole(t, h, tb.IAM(), agent, "roles/bigquery.dataEditor", true)
	waitExportState(t, h, ps, s.GetName(), "ACTIVE")
	eventually1874(t, "the message written once the role is back", 60*time.Second, func() bool {
		rows := bqRows1874(t, h, tb)
		return len(rows) == 1 && rows[0]["data"] == "while-denied"
	})
}

// storageBucket1998 is a bucket of project, removed at the end.
func storageBucket1998(t *testing.T, h *Harness, sc *storage.Client, project, prefix string) *storage.BucketHandle {
	t.Helper()
	name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1e9)
	b := sc.Bucket(name)
	if err := b.Create(h.Context(), project, nil); err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
	t.Cleanup(func() {
		it := b.Objects(h.Context(), nil)
		for o, err := it.Next(); err == nil; o, err = it.Next() {
			_ = b.Object(o.Name).Delete(h.Context())
		}
		_ = b.Delete(h.Context())
	})
	return b
}

func storageRefused1998(bucket, account string) string {
	return "Cloud Pub/Sub did not have the necessary permissions configured to access the provided bucket " + bucket +
		" (or the bucket may not exist). Please verify that the service account " + account +
		" was granted the Storage Legacy Bucket Reader and Storage Object Creator roles for the provided bucket."
}

// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription
//
// TestIAMEnforcementPubSubStorageServiceAgentRoles (#1998): a Cloud
// Storage subscription is refused while the Pub/Sub service agent lacks
// Storage Object Creator and Storage Legacy Bucket Reader on the bucket,
// with the troubleshooting page's "Cloud Pub/Sub did not have the
// necessary permissions configured to access the provided bucket ...";
// created once the bucket's policy grants both; PERMISSION_DENIED when
// Storage Object Creator is removed, and ACTIVE once it is back.
func TestIAMEnforcementPubSubStorageServiceAgentRoles(t *testing.T) {
	h := New(t)
	ps, sc := exportClients(t, h)
	h.Endpoint(EnvResourceManager)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "psgcs-")
	agentEmail := pubsubAgent1998(t, h, project)
	agent := "serviceAccount:" + agentEmail
	b := storageBucket1998(t, h, sc, project, "ps1998-agent")
	topic := newTopic1998(t, h, ps, project, "gcs-agent")
	s := &pubsubpb.Subscription{Name: "projects/" + project + "/subscriptions/gcs-agent", Topic: topic,
		CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: b.BucketName()}}
	_, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, s)
	wantCreateRefused(t, "CreateSubscription without the bucket roles", err, codes.FailedPrecondition, storageRefused1998(b.BucketName(), agentEmail))
	// Storage Legacy Bucket Reader alone is not enough.
	handleRole(t, h, b.IAM(), agent, "roles/storage.legacyBucketReader", true)
	time.Sleep(3 * time.Second)
	_, err = ps.SubscriptionAdminClient.CreateSubscription(ctx, s)
	wantCreateRefused(t, "CreateSubscription with Storage Legacy Bucket Reader only", err, codes.FailedPrecondition, storageRefused1998(b.BucketName(), agentEmail))

	handleRole(t, h, b.IAM(), agent, "roles/storage.objectCreator", true)
	createEventually(t, h, ps, s)
	waitExportState(t, h, ps, s.GetName(), "ACTIVE")
	handleRole(t, h, b.IAM(), agent, "roles/storage.objectCreator", false)
	waitExportState(t, h, ps, s.GetName(), "PERMISSION_DENIED")
	handleRole(t, h, b.IAM(), agent, "roles/storage.objectCreator", true)
	waitExportState(t, h, ps, s.GetName(), "ACTIVE")
}

// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription
//
// TestIAMEnforcementPubSubExportServiceAccountActAs (#1998): a caller with
// Pub/Sub Editor on the project cannot create a subscription that writes
// as a service_account_email it lacks iam.serviceAccounts.actAs on
// (PERMISSION_DENIED); with Service Account User on the account, the
// create still needs the Pub/Sub service agent's
// iam.serviceAccounts.getAccessToken on it (Service Account Token
// Creator), then the account's own bucket roles; with all three it is
// created, ACTIVE, and the service agent's own roles are not needed.
func TestIAMEnforcementPubSubExportServiceAccountActAs(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envSinkPubSub, EnvPubSub)
	ownerPS, sc := exportClients(t, h)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "psact-")
	agent := "serviceAccount:" + pubsubAgent1998(t, h, project)
	p := newIAMPrincipal(t, h, project, "psact")
	p.grant("roles/pubsub.editor")
	as, err := pubsub.NewClient(ctx, project, grpcAs(stripScheme(endpoint), p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	writer := newServiceAccount(t, h, project, "pswriter")
	b := storageBucket1998(t, h, sc, project, "ps1998-actas")
	topic := newTopic1998(t, h, ownerPS, project, "gcs-actas")
	s := &pubsubpb.Subscription{Name: "projects/" + project + "/subscriptions/gcs-actas", Topic: topic,
		CloudStorageConfig: &pubsubpb.CloudStorageConfig{Bucket: b.BucketName(), ServiceAccountEmail: writer.GetEmail()}}
	var lastErr error
	for i := 0; i < 20; i++ {
		// The editor grant reaches the front's check at once; retry a
		// moment for the token's first use.
		if _, lastErr = as.SubscriptionAdminClient.CreateSubscription(ctx, s); status.Code(lastErr) == codes.PermissionDenied &&
			strings.Contains(status.Convert(lastErr).Message(), "iam.serviceAccounts.actAs") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	wantCreateRefused(t, "CreateSubscription without actAs", lastErr, codes.PermissionDenied,
		"Permission 'iam.serviceAccounts.actAs' denied on service account "+writer.GetEmail())

	grantOn(t, h, writer.GetName(), "roles/iam.serviceAccountUser", "serviceAccount:"+p.email)
	time.Sleep(time.Second)
	_, err = as.SubscriptionAdminClient.CreateSubscription(ctx, s)
	wantCreateRefused(t, "CreateSubscription without the agent's getAccessToken", err, codes.FailedPrecondition, "iam.serviceAccounts.getAccessToken")

	grantOn(t, h, writer.GetName(), "roles/iam.serviceAccountTokenCreator", agent)
	time.Sleep(3 * time.Second)
	_, err = as.SubscriptionAdminClient.CreateSubscription(ctx, s)
	wantCreateRefused(t, "CreateSubscription without the account's bucket roles", err, codes.FailedPrecondition, storageRefused1998(b.BucketName(), writer.GetEmail()))

	member := "serviceAccount:" + writer.GetEmail()
	handleRole(t, h, b.IAM(), member, "roles/storage.legacyBucketReader", true)
	handleRole(t, h, b.IAM(), member, "roles/storage.objectCreator", true)
	createEventually(t, h, as, s)
	waitExportState(t, h, ownerPS, s.GetName(), "ACTIVE")
}
