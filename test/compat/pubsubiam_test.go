//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// Pub/Sub IAM (#1367, ADR-0006): the Pub/Sub front stores the policies of
// topics, subscriptions, snapshots and schemas and enforces none. These
// tests drive it with the official clients, gcloud, Terraform and the
// console, and assert nothing about access.

// pubsubIAMHandle is the IAM handle the Pub/Sub client's topic.IAM() and
// sub.IAM() return (cloud.google.com/go/pubsub v1 builds it with
// iam.InternalNewHandle on its connection; pubsub/v2 has the admin
// clients' methods instead), on a connection to the Pub/Sub port.
func pubsubIAMHandle(t *testing.T, h *Harness, resource string) *iam.Handle {
	t.Helper()
	pubsubClient(t, h) // skips without CLOUDBURROW_TEST_PUBSUB
	conn, err := grpc.NewClient(h.Endpoint(EnvPubSub), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return iam.InternalNewHandle(conn, resource)
}

// TestPubSubIAMHandle: the iam handle on a topic and a subscription. A
// policy set reads back with a new etag, a stale etag is ABORTED, a
// conditional binding is UNIMPLEMENTED, TestPermissions answers the asked
// permissions Pub/Sub defines on the resource, and deleting a topic or a
// subscription deletes its policy.
// covers: pubsub.projects.topics.getIamPolicy, pubsub.projects.topics.setIamPolicy, pubsub.projects.topics.testIamPermissions, pubsub.projects.subscriptions.getIamPolicy, pubsub.projects.subscriptions.setIamPolicy, pubsub.projects.subscriptions.testIamPermissions
func TestPubSubIAMHandle(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	topicName := topic(t, h, c, "iam-handle-topic")
	sub := subscription(t, h, c, "iam-handle-sub", topicName)
	member := "serviceAccount:app@" + h.Project() + ".iam.gserviceaccount.com"

	for _, tc := range []struct {
		name, role string
		perms      []string
		want       []string
	}{
		{topicName, "roles/pubsub.publisher",
			[]string{"pubsub.topics.publish", "pubsub.subscriptions.consume", "storage.objects.get", "pubsub.topics.get"},
			[]string{"pubsub.topics.publish", "pubsub.topics.get"}},
		{sub, "roles/pubsub.subscriber",
			[]string{"pubsub.subscriptions.consume", "pubsub.topics.publish"},
			[]string{"pubsub.subscriptions.consume"}},
	} {
		hd := pubsubIAMHandle(t, h, tc.name)
		empty, err := hd.Policy(ctx)
		if err != nil || len(empty.Roles()) != 0 {
			t.Fatalf("%s: Policy on a new resource = %v, %v; want the empty policy", tc.name, empty, err)
		}
		emptyEtag := append([]byte(nil), empty.InternalProto.GetEtag()...)
		empty.Add(member, iam.RoleName(tc.role))
		if err := hd.SetPolicy(ctx, empty); err != nil {
			t.Fatalf("%s: SetPolicy: %v", tc.name, err)
		}
		got, err := hd.Policy(ctx)
		if err != nil || !got.HasRole(member, iam.RoleName(tc.role)) {
			t.Errorf("%s: read back %v, %v; want %s holding %s", tc.name, got, err, member, tc.role)
		}
		if string(got.InternalProto.GetEtag()) == string(emptyEtag) {
			t.Errorf("%s: the etag did not change on a set", tc.name)
		}
		stale := &iam.Policy{InternalProto: &iampb.Policy{Etag: emptyEtag}}
		if err := hd.SetPolicy(ctx, stale); status.Code(err) != codes.Aborted {
			t.Errorf("%s: SetPolicy with a stale etag: %v, want ABORTED", tc.name, err)
		}
		cond := &iam.Policy3{Bindings: []*iampb.Binding{{Role: tc.role, Members: []string{member},
			Condition: &expr.Expr{Title: "later", Expression: `request.time > timestamp("2030-01-01T00:00:00Z")`}}}}
		if err := hd.V3().SetPolicy(ctx, cond); status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "condition") {
			t.Errorf("%s: a conditional binding: %v, want UNIMPLEMENTED naming the condition", tc.name, err)
		}
		held, err := hd.TestPermissions(ctx, tc.perms)
		if err != nil || strings.Join(held, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: TestPermissions(%v) = %v, %v; want %v", tc.name, tc.perms, held, err, tc.want)
		}
	}

	// The admin clients' methods are the same calls.
	p, err := c.TopicAdminClient.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: topicName})
	if err != nil || len(p.GetBindings()) != 1 {
		t.Errorf("TopicAdminClient.GetIamPolicy = %v, %v; want the one binding", p, err)
	}
	if _, err := c.SubscriptionAdminClient.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + h.Project() + "/subscriptions/none"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicy on a missing subscription: %v, want NOT_FOUND", err)
	}

	// Deleting the resources deletes their policies: made again, they have none.
	if err := c.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: sub}); err != nil {
		t.Fatal(err)
	}
	if err := c.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: topicName}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topicName}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{topicName, sub} {
		p, err := pubsubIAMHandle(t, h, name).Policy(ctx)
		if err != nil || len(p.Roles()) != 0 {
			t.Errorf("%s made again: %v, %v; want the empty policy", name, p.Roles(), err)
		}
	}
}

// TestPubSubIAMOnASnapshotAndASchema: the same methods on a snapshot and a
// schema, through the official clients.
// covers: pubsub.projects.snapshots.getIamPolicy, pubsub.projects.snapshots.setIamPolicy, pubsub.projects.snapshots.testIamPermissions, pubsub.projects.schemas.getIamPolicy, pubsub.projects.schemas.setIamPolicy, pubsub.projects.schemas.testIamPermissions
func TestPubSubIAMOnASnapshotAndASchema(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	topicName := topic(t, h, c, "iam-snap-topic")
	sub := subscription(t, h, c, "iam-snap-sub", topicName)
	snap := "projects/" + h.Project() + "/snapshots/iam-snap"
	if _, err := c.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: snap, Subscription: sub}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSnapshot(context.Background(), &pubsubpb.DeleteSnapshotRequest{Snapshot: snap})
	})
	sc := schemaClient(t, h)
	schema, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + h.Project(), SchemaId: "iam-schema",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: `{"type":"record","name":"R","fields":[{"name":"a","type":"string"}]}`}})
	if err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: schema.GetName()})
	})

	for _, tc := range []struct{ name, perm string }{
		{snap, "pubsub.snapshots.seek"},
		{schema.GetName(), "pubsub.schemas.attach"},
	} {
		set, err := sc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: tc.name, Policy: &iampb.Policy{
			Bindings: []*iampb.Binding{{Role: "roles/pubsub.viewer", Members: []string{"user:ada@example.com"}}}}})
		if err != nil {
			t.Fatalf("SetIamPolicy %s: %v", tc.name, err)
		}
		got, err := c.SubscriptionAdminClient.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: tc.name})
		if err != nil || len(got.GetBindings()) != 1 || string(got.GetEtag()) != string(set.GetEtag()) {
			t.Errorf("%s: read back %v, %v", tc.name, got, err)
		}
		tp, err := sc.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: tc.name, Permissions: []string{tc.perm, "pubsub.topics.publish"}})
		if err != nil || strings.Join(tp.GetPermissions(), ",") != tc.perm {
			t.Errorf("%s: TestIamPermissions = %v, %v; want [%s]", tc.name, tp.GetPermissions(), err, tc.perm)
		}
	}
	if err := c.SubscriptionAdminClient.DeleteSnapshot(ctx, &pubsubpb.DeleteSnapshotRequest{Snapshot: snap}); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: snap}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicy on a deleted snapshot: %v, want NOT_FOUND", err)
	}
}

// TestGcloudPubSubIAM: `gcloud pubsub topics add-iam-policy-binding` and
// `get-iam-policy`, and the subscription ones, through the configuration
// gcloud-setup writes, over the front's REST API; the official client reads
// back what gcloud set. gcloud read-modify-writes with the etag, so a
// second add keeps the first.
// covers: pubsub.projects.topics.getIamPolicy, pubsub.projects.topics.setIamPolicy, pubsub.projects.subscriptions.getIamPolicy, pubsub.projects.subscriptions.setIamPolicy
func TestGcloudPubSubIAM(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	p := "--project=" + h.Project()
	topicName := topic(t, h, c, "gcloud-iam-topic")
	sub := subscription(t, h, c, "gcloud-iam-sub", topicName)
	const dev, ops = "user:dev@example.com", "group:ops@example.com"

	g.must("pubsub", "topics", "add-iam-policy-binding", "gcloud-iam-topic", p, "--member="+dev, "--role=roles/pubsub.publisher")
	g.must("pubsub", "topics", "add-iam-policy-binding", "gcloud-iam-topic", p, "--member="+ops, "--role=roles/pubsub.viewer")
	g.must("pubsub", "subscriptions", "add-iam-policy-binding", "gcloud-iam-sub", p, "--member="+dev, "--role=roles/pubsub.subscriber")

	roles := func(get func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error), name string) map[string]string {
		t.Helper()
		pol, err := get(ctx, &iampb.GetIamPolicyRequest{Resource: name})
		if err != nil {
			t.Fatalf("GetIamPolicy %s: %v", name, err)
		}
		out := map[string]string{}
		for _, b := range pol.GetBindings() {
			out[b.GetRole()] = strings.Join(b.GetMembers(), ",")
		}
		return out
	}
	topicGet := func(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
		return c.TopicAdminClient.GetIamPolicy(ctx, r)
	}
	subGet := func(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
		return c.SubscriptionAdminClient.GetIamPolicy(ctx, r)
	}
	if r := roles(topicGet, topicName); fmt.Sprint(r) != fmt.Sprint(map[string]string{"roles/pubsub.publisher": dev, "roles/pubsub.viewer": ops}) {
		t.Errorf("the topic's policy = %v; want publisher → %s and viewer → %s", r, dev, ops)
	}
	if r := roles(subGet, sub); r["roles/pubsub.subscriber"] != dev || len(r) != 1 {
		t.Errorf("the subscription's policy = %v; want subscriber → %s", r, dev)
	}

	var got struct {
		Etag     string `json:"etag"`
		Bindings []struct {
			Role    string   `json:"role"`
			Members []string `json:"members"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal([]byte(g.must("pubsub", "topics", "get-iam-policy", "gcloud-iam-topic", p, "--format=json")), &got); err != nil {
		t.Fatalf("get-iam-policy is not JSON: %v", err)
	}
	if got.Etag == "" || len(got.Bindings) != 2 {
		t.Errorf("topics get-iam-policy = %+v; want both bindings and an etag", got)
	}
	g.must("pubsub", "topics", "remove-iam-policy-binding", "gcloud-iam-topic", p, "--member="+ops, "--role=roles/pubsub.viewer")
	if r := roles(topicGet, topicName); len(r) != 1 || r["roles/pubsub.publisher"] != dev {
		t.Errorf("after remove-iam-policy-binding the topic's policy = %v", r)
	}
	if out := g.must("pubsub", "subscriptions", "get-iam-policy", "gcloud-iam-sub", p, "--format=value(bindings.role)"); !strings.Contains(out, "roles/pubsub.subscriber") {
		t.Errorf("subscriptions get-iam-policy = %q", out)
	}
}

// TestTerraformPubSubIAM (#1367, #1304): google_pubsub_topic_iam_member,
// _binding and _policy, google_pubsub_subscription_iam_* and
// google_pubsub_schema_iam_*, through
// `cloudburrow terraform` with egress blocked: applied, a plan with no
// diff, changed (member and binding, then none, then a policy, then none)
// and destroyed, each read back with the official client.
// covers: google.pubsub.v1.Publisher/CreateTopic, google.pubsub.v1.Publisher/GetTopic, google.pubsub.v1.Publisher/DeleteTopic, google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Subscriber/DeleteSubscription, pubsub.projects.topics.getIamPolicy, pubsub.projects.topics.setIamPolicy, pubsub.projects.subscriptions.getIamPolicy, pubsub.projects.subscriptions.setIamPolicy, pubsub.projects.schemas.getIamPolicy, pubsub.projects.schemas.setIamPolicy, google.pubsub.v1.SchemaService/CreateSchema, google.pubsub.v1.SchemaService/GetSchema, google.pubsub.v1.SchemaService/DeleteSchema
func TestTerraformPubSubIAM(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h) // skips without CLOUDBURROW_TEST_PUBSUB
	m := newTFModule(t)
	ctx := h.Context()
	project := h.Project()
	topicName := "projects/" + project + "/topics/tf-iam-topic"
	sub := "projects/" + project + "/subscriptions/tf-iam-sub"
	sc := schemaClient(t, h)

	base := fmt.Sprintf(`
resource "google_pubsub_topic" "t" {
  project = %[1]q
  name    = "tf-iam-topic"
}
resource "google_pubsub_subscription" "s" {
  project = %[1]q
  name    = "tf-iam-sub"
  topic   = google_pubsub_topic.t.id
}
resource "google_pubsub_schema" "sc" {
  project    = %[1]q
  name       = "tf-iam-schema"
  type       = "AVRO"
  definition = jsonencode({ type = "record", name = "R", fields = [{ name = "a", type = "string" }] })
}
`, project)
	read := func(get func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error), name string) func() map[string][]string {
		return func() map[string][]string {
			t.Helper()
			p, err := get(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				t.Fatalf("GetIamPolicy %s: %v", name, err)
			}
			out := map[string][]string{}
			for _, b := range p.GetBindings() {
				out[b.GetRole()] = b.GetMembers()
			}
			return out
		}
	}
	targets := []tfIAMTarget{
		{kind: "google_pubsub_topic", label: "t", ref: map[string]string{"project": "google_pubsub_topic.t.project", "topic": "google_pubsub_topic.t.name"},
			read: read(func(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
				return c.TopicAdminClient.GetIamPolicy(ctx, r)
			}, topicName)},
		{kind: "google_pubsub_subscription", label: "s", ref: map[string]string{"project": "google_pubsub_subscription.s.project", "subscription": "google_pubsub_subscription.s.name"},
			read: read(func(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
				return c.SubscriptionAdminClient.GetIamPolicy(ctx, r)
			}, sub)},
		{kind: "google_pubsub_schema", label: "sc", ref: map[string]string{"project": "google_pubsub_schema.sc.project", "schema": "google_pubsub_schema.sc.name"},
			read: read(func(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
				return sc.GetIamPolicy(ctx, r)
			}, "projects/"+project+"/schemas/tf-iam-schema")},
	}
	runTFIAMPhases(t, m, base, [2]string{"roles/pubsub.publisher", "roles/pubsub.viewer"}, targets)

	if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topicName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTopic after destroy: %v; want NotFound", err)
	}
	if _, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSubscription after destroy: %v; want NotFound", err)
	}
}

// TestConsolePermissionsOnATopicAndASubscription: the Permissions tab on a
// topic's page and a subscription's, read back with the iam handle.
// covers: pubsub.projects.topics.getIamPolicy, pubsub.projects.topics.setIamPolicy, pubsub.projects.topics.testIamPermissions, pubsub.projects.subscriptions.getIamPolicy, pubsub.projects.subscriptions.setIamPolicy, pubsub.projects.subscriptions.testIamPermissions
func TestConsolePermissionsOnATopicAndASubscription(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	topicName := topic(t, h, c, "console-perms-topic")
	sub := subscription(t, h, c, "console-perms-sub", topicName)
	addr := consoleAddr(t, h)
	consolePermissionsRoundTrip(t, h, addr, "pubsub", []string{topicName}, handleOf(pubsubIAMHandle(t, h, topicName)),
		"roles/pubsub.publisher", "pubsub.topics.publish")
	consolePermissionsRoundTrip(t, h, addr, "pubsub-subscriptions", []string{sub}, handleOf(pubsubIAMHandle(t, h, sub)),
		"roles/pubsub.subscriber", "pubsub.subscriptions.consume")
}
