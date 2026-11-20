package pubsubfront

import (
	"context"
	"slices"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// #2000: an UpdateTopic of message_storage_policy is applied by the front;
// Get, List and the update's answer name the new policy, a zone or a
// multi-region is refused, the export check reads it, and a delete forgets
// it.
func TestUpdateTopicStoragePolicy(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	name := "projects/" + project + "/topics/stored"
	pub := fx.client.TopicAdminClient
	if _, err := pub.CreateTopic(ctx, &pubsubpb.Topic{Name: name,
		MessageStoragePolicy: &pubsubpb.MessageStoragePolicy{AllowedPersistenceRegions: []string{"us-east1"}, EnforceInTransit: true}}); err != nil {
		t.Fatal(err)
	}
	mask := &fieldmaskpb.FieldMask{Paths: []string{"message_storage_policy"}}
	want := []string{"us-central1", "europe-west1"}
	got, err := pub.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{Topic: &pubsubpb.Topic{Name: name,
		MessageStoragePolicy: &pubsubpb.MessageStoragePolicy{AllowedPersistenceRegions: want, EnforceInTransit: true}}, UpdateMask: mask})
	if err != nil || !slices.Equal(got.GetMessageStoragePolicy().GetAllowedPersistenceRegions(), want) {
		t.Fatalf("UpdateTopic(message_storage_policy) = %v, %v; want %v", got.GetMessageStoragePolicy(), err, want)
	}
	read, err := pub.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name})
	if err != nil || !slices.Equal(read.GetMessageStoragePolicy().GetAllowedPersistenceRegions(), want) || !read.GetMessageStoragePolicy().GetEnforceInTransit() {
		t.Errorf("GetTopic = %v, %v; want %v enforced", read.GetMessageStoragePolicy(), err, want)
	}
	it := pub.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: "projects/" + project})
	for tp, err := it.Next(); err == nil; tp, err = it.Next() {
		if tp.GetName() == name && !slices.Equal(tp.GetMessageStoragePolicy().GetAllowedPersistenceRegions(), want) {
			t.Errorf("ListTopics lists %v, want %v", tp.GetMessageStoragePolicy(), want)
		}
	}
	if !fx.front.inTransitAllowed(name, "us-central1") || fx.front.inTransitAllowed(name, "us-east1") {
		t.Errorf("the export check does not read the new regions")
	}
	for _, bad := range []string{"us", "us-central1-a", "nam4"} {
		_, err := pub.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{Topic: &pubsubpb.Topic{Name: name,
			MessageStoragePolicy: &pubsubpb.MessageStoragePolicy{AllowedPersistenceRegions: []string{bad}}}, UpdateMask: mask})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("a policy naming %q: %v, want INVALID_ARGUMENT", bad, err)
		}
	}
	cleared, err := pub.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{Topic: &pubsubpb.Topic{Name: name}, UpdateMask: mask})
	if err != nil || cleared.GetMessageStoragePolicy() != nil {
		t.Errorf("an update with no policy = %v, %v; want none", cleared.GetMessageStoragePolicy(), err)
	}
	if !fx.front.inTransitAllowed(name, "asia-east1") {
		t.Errorf("a cleared policy still restricts the exports")
	}
	if err := pub.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: name}); err != nil {
		t.Fatal(err)
	}
	fx.front.mu.Lock()
	_, kept := fx.front.storagePolicies[name]
	fx.front.mu.Unlock()
	if kept {
		t.Errorf("a deleted topic's policy is still kept")
	}
}

// The kept policy survives a restart of the front with its state file.
func TestStoragePolicyState(t *testing.T) {
	f, err := New("127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	name := "projects/" + project + "/topics/kept"
	f.statePath = t.TempDir() + "/state.json"
	f.setStoragePolicy(name, &pubsubpb.MessageStoragePolicy{AllowedPersistenceRegions: []string{"us-west1"}, EnforceInTransit: true})
	g, err := New("127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.KeepState(f.statePath); err != nil {
		t.Fatal(err)
	}
	p := g.topicStoragePolicy(name)
	if !slices.Equal(p.GetAllowedPersistenceRegions(), []string{"us-west1"}) || !p.GetEnforceInTransit() {
		t.Errorf("restored policy = %v", p)
	}
}
