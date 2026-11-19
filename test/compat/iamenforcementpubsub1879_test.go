//go:build compat

package compat

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const pubsubDenied = "User not authorized to perform this action."

func wantPubSubDenied(t *testing.T, what string, err error) {
	t.Helper()
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied || st.Message() != pubsubDenied {
		t.Fatalf("%s = %v; want PERMISSION_DENIED %q", what, err, pubsubDenied)
	}
}

// pubsubIAM is the google.iam.v1.IAMPolicy client on Pub/Sub's port, as
// token ("" the instance's own account).
func pubsubIAM(t *testing.T, endpoint, token string) iampb.IAMPolicyClient {
	t.Helper()
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearerCreds(token)))
	}
	conn, err := grpc.NewClient(stripScheme(endpoint), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return iampb.NewIAMPolicyClient(conn)
}

// covers: google.pubsub.v1.Publisher/Publish, google.pubsub.v1.Publisher/GetTopic, google.pubsub.v1.Publisher/ListTopics, google.pubsub.v1.Subscriber/Pull, google.pubsub.v1.Subscriber/Acknowledge, google.pubsub.v1.Subscriber/Seek, google.pubsub.v1.Subscriber/CreateSnapshot, google.pubsub.v1.Subscriber/StreamingPull
// covers: google.iam.v1.IAMPolicy/GetIamPolicy, google.iam.v1.IAMPolicy/SetIamPolicy, google.iam.v1.IAMPolicy/TestIamPermissions
//
// TestIAMEnforcementPubSub (#1879), with the official Pub/Sub client (v2)
// over gRPC and a minted token: Publish without a role is
// PERMISSION_DENIED "User not authorized to perform this action.";
// roles/pubsub.publisher on the topic's policy lets it publish and
// TestIamPermissions answers only pubsub.topics.publish of what is asked;
// the revoke refuses again; allUsers on the topic lets it publish; Pull,
// Acknowledge, StreamingPull and Seek need roles/pubsub.subscriber on the
// subscription; Viewer on the project is inherited by GetTopic and
// ListTopics, and does not publish; CreateSnapshot needs
// pubsub.snapshots.create on the project (Editor); the subscription's
// policy cannot be read without getIamPolicy.
func TestIAMEnforcementPubSub(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvPubSub)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iepb-")
	owner, err := pubsub.NewClient(ctx, project, grpcAs(stripScheme(endpoint), "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	topic := "projects/" + project + "/topics/enforced"
	sub := "projects/" + project + "/subscriptions/enforced"
	if _, err := owner.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatalf("CreateTopic as the instance's account: %v", err)
	}
	t.Cleanup(func() { _ = owner.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: topic}) })
	if _, err := owner.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	t.Cleanup(func() {
		_ = owner.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: sub})
	})
	ownerIAM := pubsubIAM(t, endpoint, "")
	p := newIAMPrincipal(t, h, project, "iepb")
	as, err := pubsub.NewClient(ctx, project, grpcAs(stripScheme(endpoint), p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	asIAM := pubsubIAM(t, endpoint, p.token)
	publish := func() error {
		_, err := as.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic, Messages: []*pubsubpb.PubsubMessage{{Data: []byte("m")}}})
		return err
	}
	bind := func(resource, role, member string) func() {
		return bindOn(t, ownerIAM.GetIamPolicy, ownerIAM.SetIamPolicy, resource, &iampb.Binding{Role: role, Members: []string{member}})
	}
	me := "serviceAccount:" + p.email

	wantPubSubDenied(t, "Publish without a role", publish())
	undo := bind(topic, "roles/pubsub.publisher", me)
	waitGranted(t, "Publish as a publisher on the topic", publish)
	held, err := asIAM.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: topic,
		Permissions: []string{"pubsub.topics.publish", "pubsub.topics.delete"}})
	if err != nil || len(held.GetPermissions()) != 1 || held.GetPermissions()[0] != "pubsub.topics.publish" {
		t.Errorf("TestIamPermissions as a publisher = %v, %v; want only pubsub.topics.publish", held.GetPermissions(), err)
	}
	// The publisher client (batching) goes the same way.
	if _, err := as.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte("b")}).Get(ctx); err != nil {
		t.Errorf("Publisher.Publish as a publisher: %v", err)
	}
	undo()
	wantPubSubDenied(t, "Publish after the revoke", publish())
	undo = bind(topic, "roles/pubsub.publisher", "allUsers")
	waitGranted(t, "Publish with allUsers on the topic", publish)
	undo()
	wantPubSubDenied(t, "Publish after allUsers is gone", publish())

	pull := func() error {
		_, err := as.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10, ReturnImmediately: true})
		return err
	}
	wantPubSubDenied(t, "Pull without a role", pull())
	_, err = asIAM.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: sub})
	wantPubSubDenied(t, "GetIamPolicy on the subscription without a role", err)
	undo = bind(sub, "roles/pubsub.subscriber", me)
	waitGranted(t, "Pull as a subscriber", pull)
	resp, err := as.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	var ids []string
	for _, m := range resp.GetReceivedMessages() {
		ids = append(ids, m.GetAckId())
	}
	if len(ids) > 0 {
		if err := as.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: ids}); err != nil {
			t.Errorf("Acknowledge as a subscriber: %v", err)
		}
	}
	if _, err := as.SubscriptionAdminClient.Seek(ctx, &pubsubpb.SeekRequest{Subscription: sub,
		Target: &pubsubpb.SeekRequest_Time{Time: timestamppb.New(time.Now())}}); err != nil {
		t.Errorf("Seek as a subscriber: %v", err)
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err = as.Subscriber(sub).Receive(rctx, func(_ context.Context, m *pubsub.Message) { m.Ack() })
	cancel()
	if status.Code(err) == codes.PermissionDenied {
		t.Errorf("StreamingPull as a subscriber: %v", err)
	}
	undo()
	wantPubSubDenied(t, "Pull after the revoke", pull())
	rctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	err = as.Subscriber(sub).Receive(rctx, func(_ context.Context, m *pubsub.Message) { m.Ack() })
	cancel()
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("StreamingPull after the revoke = %v; want PERMISSION_DENIED", err)
	}

	// Viewer on the project, inherited.
	_, err = as.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
	wantPubSubDenied(t, "GetTopic without a role", err)
	p.grant("roles/pubsub.viewer")
	waitGranted(t, "GetTopic as a viewer on the project", func() error {
		_, err := as.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
		return err
	})
	if _, err := as.TopicAdminClient.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: "projects/" + project}).Next(); err != nil {
		t.Errorf("ListTopics as a viewer: %v", err)
	}
	wantPubSubDenied(t, "Publish as a viewer", publish())
	p.revoke("roles/pubsub.viewer")

	// Snapshots.
	snap := "projects/" + project + "/snapshots/enforced"
	createSnap := func() error {
		_, err := as.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: snap, Subscription: sub})
		return err
	}
	wantPubSubDenied(t, "CreateSnapshot without a role", createSnap())
	p.grant("roles/pubsub.editor")
	waitGranted(t, "CreateSnapshot as an editor", createSnap)
	t.Cleanup(func() {
		_ = owner.SubscriptionAdminClient.DeleteSnapshot(ctx, &pubsubpb.DeleteSnapshotRequest{Snapshot: snap})
	})
	if _, err := as.SubscriptionAdminClient.Seek(ctx, &pubsubpb.SeekRequest{Subscription: sub,
		Target: &pubsubpb.SeekRequest_Snapshot{Snapshot: snap}}); err != nil {
		t.Errorf("Seek to the snapshot as an editor: %v", err)
	}
	p.revoke("roles/pubsub.editor")
	_, err = as.SubscriptionAdminClient.Seek(ctx, &pubsubpb.SeekRequest{Subscription: sub, Target: &pubsubpb.SeekRequest_Snapshot{Snapshot: snap}})
	wantPubSubDenied(t, "Seek after the revoke", err)
	if _, err := owner.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic, Messages: []*pubsubpb.PubsubMessage{{Data: []byte("o")}}}); err != nil {
		t.Errorf("Publish as the instance's account: %v", err)
	}
}
