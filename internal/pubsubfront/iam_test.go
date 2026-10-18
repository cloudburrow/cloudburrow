package pubsubfront

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIAMPolicyOnATopicAndASubscription(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	topic := fx.topic(t, "iam-topic")
	sub := subName("iam-sub")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	ta, sa := fx.client.TopicAdminClient, fx.client.SubscriptionAdminClient
	for _, tc := range []struct {
		name, role string
		get        func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error)
		set        func(context.Context, *iampb.SetIamPolicyRequest) (*iampb.Policy, error)
		test       func(context.Context, *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error)
		perms      []string
		want       string
	}{
		{topic, "roles/pubsub.publisher",
			func(c context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
				return ta.GetIamPolicy(c, r)
			},
			func(c context.Context, r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
				return ta.SetIamPolicy(c, r)
			},
			func(c context.Context, r *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
				return ta.TestIamPermissions(c, r)
			},
			[]string{"pubsub.topics.publish", "storage.objects.get", "pubsub.subscriptions.consume", "pubsub.topics.get"},
			"pubsub.topics.publish,pubsub.topics.get"},
		{sub, "roles/pubsub.subscriber",
			func(c context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
				return sa.GetIamPolicy(c, r)
			},
			func(c context.Context, r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
				return sa.SetIamPolicy(c, r)
			},
			func(c context.Context, r *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
				return sa.TestIamPermissions(c, r)
			},
			[]string{"pubsub.subscriptions.consume", "pubsub.topics.publish"},
			"pubsub.subscriptions.consume"},
	} {
		empty, err := tc.get(ctx, &iampb.GetIamPolicyRequest{Resource: tc.name})
		if err != nil || len(empty.GetBindings()) != 0 || len(empty.GetEtag()) == 0 {
			t.Fatalf("%s: GetIamPolicy on a new resource = %v, %v; want the empty policy with an etag", tc.name, empty, err)
		}
		set, err := tc.set(ctx, &iampb.SetIamPolicyRequest{Resource: tc.name, Policy: &iampb.Policy{Etag: empty.GetEtag(),
			Bindings: []*iampb.Binding{{Role: tc.role, Members: []string{"user:ada@example.com"}}}}})
		if err != nil {
			t.Fatalf("%s: SetIamPolicy: %v", tc.name, err)
		}
		got, err := tc.get(ctx, &iampb.GetIamPolicyRequest{Resource: tc.name})
		if err != nil || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetRole() != tc.role || string(got.GetEtag()) != string(set.GetEtag()) {
			t.Errorf("%s: read back %v, %v; want the binding with the set's etag", tc.name, got, err)
		}
		if _, err := tc.set(ctx, &iampb.SetIamPolicyRequest{Resource: tc.name, Policy: &iampb.Policy{Etag: empty.GetEtag()}}); status.Code(err) != codes.Aborted {
			t.Errorf("%s: a stale etag: %v, want ABORTED", tc.name, err)
		}
		_, err = tc.set(ctx, &iampb.SetIamPolicyRequest{Resource: tc.name, Policy: &iampb.Policy{Version: 3,
			Bindings: []*iampb.Binding{{Role: tc.role, Members: []string{"user:ada@example.com"}, Condition: &expr.Expr{Expression: "true"}}}}})
		if status.Code(err) != codes.Unimplemented {
			t.Errorf("%s: a condition: %v, want UNIMPLEMENTED", tc.name, err)
		}
		tp, err := tc.test(ctx, &iampb.TestIamPermissionsRequest{Resource: tc.name, Permissions: tc.perms})
		if err != nil || strings.Join(tp.GetPermissions(), ",") != tc.want {
			t.Errorf("%s: TestIamPermissions = %v, %v; want %s", tc.name, tp.GetPermissions(), err, tc.want)
		}
	}

	// Deleting the resources drops their policies: made again, they have none.
	if err := sa.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: sub}); err != nil {
		t.Fatal(err)
	}
	if err := ta.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := ta.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: topic}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicy on a deleted topic: %v, want NOT_FOUND", err)
	}
	fx.topic(t, "iam-topic")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{topic, sub} {
		p, err := ta.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
		if err != nil || len(p.GetBindings()) != 0 {
			t.Errorf("%s made again: %v, %v; want the empty policy", name, p, err)
		}
	}
	fx.front.mu.Lock()
	n := len(fx.front.iam)
	fx.front.mu.Unlock()
	if n != 0 {
		t.Errorf("the front keeps %d policies after the deletes", n)
	}
}

// The in-memory Pub/Sub has no snapshots, so the emulator's answer to
// whether one exists is stubbed; the deletes go through the front as calls.
func TestIAMPolicyOnASnapshotAndASchema(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	snap := "projects/" + project + "/snapshots/iam-snap"
	schema := "projects/" + project + "/schemas/iam-schema"
	fx.front.exists = func(_ context.Context, kind, name string) error {
		if name == snap || name == schema {
			return nil
		}
		return status.Errorf(codes.NotFound, "%s not found", name)
	}
	ta := fx.client.TopicAdminClient
	for _, name := range []string{snap, schema} {
		if _, err := ta.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{
			Bindings: []*iampb.Binding{{Role: "roles/pubsub.viewer", Members: []string{"user:ada@example.com"}}}}}); err != nil {
			t.Fatalf("SetIamPolicy %s: %v", name, err)
		}
	}
	tp, err := ta.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: snap, Permissions: []string{"pubsub.snapshots.seek", "pubsub.topics.get"}})
	if err != nil || strings.Join(tp.GetPermissions(), ",") != "pubsub.snapshots.seek" {
		t.Errorf("TestIamPermissions on a snapshot = %v, %v", tp.GetPermissions(), err)
	}
	tp, err = ta.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: schema, Permissions: []string{"pubsub.schemas.attach"}})
	if err != nil || strings.Join(tp.GetPermissions(), ",") != "pubsub.schemas.attach" {
		t.Errorf("TestIamPermissions on a schema = %v, %v", tp.GetPermissions(), err)
	}
	// The deletes fail upstream (no such resources there), so the
	// policies stay; a successful one is what drops a policy.
	_ = fx.client.SubscriptionAdminClient.DeleteSnapshot(ctx, &pubsubpb.DeleteSnapshotRequest{Snapshot: snap})
	fx.front.mu.Lock()
	_, kept := fx.front.iam[snap]
	fx.front.mu.Unlock()
	if !kept {
		t.Errorf("a failed DeleteSnapshot dropped the policy")
	}
	(&observed{f: fx.front, method: subscriber + "DeleteSnapshot", resource: snap}).done(nil)
	(&observed{f: fx.front, method: schemaService + "DeleteSchema", resource: schema}).done(nil)
	fx.front.mu.Lock()
	n := len(fx.front.iam)
	fx.front.mu.Unlock()
	if n != 0 {
		t.Errorf("the front keeps %d policies after the deletes", n)
	}
	if _, err := ta.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project + "/schemas/none"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicy on a missing schema: %v, want NOT_FOUND", err)
	}
	for _, name := range []string{"projects/" + project + "/schemas/none", "projects/" + project + "/topics/none", "projects/" + project + "/subscriptions/none", "projects/" + project + "/snapshots/none"} {
		if tp, err := ta.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: name, Permissions: []string{"pubsub.topics.get", "pubsub.schemas.get"}}); err != nil || len(tp.GetPermissions()) != 0 {
			t.Errorf("TestIamPermissions on missing %s = %v, %v; want no permissions and no error", name, tp.GetPermissions(), err)
		}
	}
	if _, err := ta.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project + "/buckets/b"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetIamPolicy on a name that is not Pub/Sub's: %v, want INVALID_ARGUMENT", err)
	}
}

func TestRESTIAMPolicy(t *testing.T) {
	fx := newFixtureREST(t, newRESTUpstream(t).addr())
	topic := fx.topic(t, "iam-rest")
	code, got := fx.rest(t, "GET", "/v1/"+topic+":getIamPolicy?options.requestedPolicyVersion=3", "")
	if code != 200 || got["etag"] != "ACAB" {
		t.Fatalf("GET :getIamPolicy = %d %v; want 200 with etag ACAB", code, got)
	}
	code, got = fx.rest(t, "POST", "/v1/"+topic+":setIamPolicy",
		`{"policy":{"version":3,"etag":"ACAB","bindings":[{"role":"roles/pubsub.publisher","members":["user:ada@example.com"]}]}}`)
	if code != 200 || got["etag"] == "ACAB" || got["bindings"] == nil {
		t.Fatalf("POST :setIamPolicy = %d %v", code, got)
	}
	if code, got := fx.rest(t, "POST", "/v1/"+topic+":setIamPolicy", `{"policy":{"etag":"ACAB"}}`); code != 409 {
		t.Errorf("a stale etag over REST = %d %v; want 409", code, got)
	}
	code, got = fx.rest(t, "POST", "/v1/"+topic+":getIamPolicy", `{}`)
	if b, _ := got["bindings"].([]any); code != 200 || len(b) != 1 {
		t.Errorf("POST :getIamPolicy = %d %v", code, got)
	}
	code, got = fx.rest(t, "POST", "/v1/"+topic+":testIamPermissions", `{"permissions":["pubsub.topics.publish","x.y.z"]}`)
	if p, _ := got["permissions"].([]any); code != 200 || len(p) != 1 || p[0] != "pubsub.topics.publish" {
		t.Errorf(":testIamPermissions = %d %v", code, got)
	}
	if code, got := fx.rest(t, "GET", "/v1/projects/"+project+"/subscriptions/none:getIamPolicy", ""); code != 404 {
		t.Errorf("a missing subscription = %d %v; want 404", code, got)
	}
	if code, got := fx.rest(t, "POST", "/v1/projects/"+project+"/subscriptions/none:testIamPermissions", `{"permissions":["pubsub.subscriptions.get"]}`); code != 200 || got["permissions"] != nil {
		t.Errorf(":testIamPermissions on a missing subscription = %d %v; want 200 with none", code, got)
	}
	if code, _ := fx.rest(t, "DELETE", "/v1/"+topic, ""); code != 200 {
		t.Fatalf("DELETE the topic = %d", code)
	}
	fx.front.mu.Lock()
	_, kept := fx.front.iam[topic]
	fx.front.mu.Unlock()
	if kept {
		t.Errorf("the topic's policy outlived its REST delete")
	}
}

func TestStateKeepsIAMPolicies(t *testing.T) {
	fake := pstest.NewServer()
	t.Cleanup(func() { _ = fake.Close() })
	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()
	r := runFront(t, fake.Addr, path)
	topic := "projects/" + project + "/topics/iam-state"
	if _, err := r.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	set, err := r.client.TopicAdminClient.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: topic, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/pubsub.publisher", Members: []string{"user:ada@example.com"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	r.stop()
	r = runFront(t, fake.Addr, path)
	got, err := r.client.TopicAdminClient.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: topic})
	if err != nil || len(got.GetBindings()) != 1 || string(got.GetEtag()) != string(set.GetEtag()) {
		t.Errorf("after a restart of the front: %v, %v; want the policy as set", got, err)
	}
}
