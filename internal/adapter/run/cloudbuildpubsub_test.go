package run

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// fakeSubs records the subscriptions a Pub/Sub trigger makes.
type fakeSubs struct {
	mu   sync.Mutex
	subs map[string]PushSubscription
}

func (f *fakeSubs) CreateTopic(context.Context, string, map[string]string) error { return nil }
func (f *fakeSubs) DeleteTopic(context.Context, string) error                    { return nil }
func (f *fakeSubs) CreatePushSubscription(_ context.Context, s PushSubscription) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[s.Name] = s
	return nil
}
func (f *fakeSubs) DeleteSubscription(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.subs, name)
	return nil
}
func (f *fakeSubs) CreateNotification(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (f *fakeSubs) DeleteNotification(context.Context, string, string) error { return nil }

// TestPubsubTrigger (#1506): a pubsubConfig trigger gets a push
// subscription on its topic; a push runs its build with $(body.message.…)
// bindings over the decoded data and attributes; a changed topic moves the
// subscription and a delete removes it.
func TestPubsubTrigger(t *testing.T) {
	name := fmt.Sprintf("ps-%d", time.Now().UnixNano()%1e9)
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &fakeBuilder{got: make(chan BuildSpec, 2)}
	f := &logFetcher{fakeFetcher: fakeFetcher{objects: map[string]string{"b/src.tgz": "archive"}}, logs: map[string]string{}}
	subs := &fakeSubs{subs: map[string]PushSubscription{}}
	cb := s.Builds(f, b).WithTriggerEvents(subs, func() string { return "http://host:1" }).CloudBuild()

	// Without Pub/Sub on the instance it is refused with the reason.
	bare := s.Builds(f, b).CloudBuild()
	tr := &cbpb.BuildTrigger{Name: name, PubsubConfig: &cbpb.PubsubConfig{Topic: "projects/proj-p/topics/t1"},
		Substitutions: map[string]string{"_TAG": "$(body.message.data.tag)", "_BUCKET": "$(body.message.attributes.bucketId)"},
		BuildTemplate: &cbpb.BuildTrigger_Build{Build: &cbpb.Build{Source: cbSource(),
			Steps: []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "-t", "x/y:${_TAG}-${_BUCKET}", "."}}}}}}
	if _, err := bare.CreateBuildTrigger(context.Background(), &cbpb.CreateBuildTriggerRequest{Parent: "projects/proj-p/locations/global", Trigger: tr}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("without Pub/Sub: %v; want FailedPrecondition", err)
	}
	bad := &cbpb.BuildTrigger{Name: name + "-bad", PubsubConfig: &cbpb.PubsubConfig{Topic: "t1"}, BuildTemplate: tr.BuildTemplate}
	if _, err := cb.CreateBuildTrigger(context.Background(), &cbpb.CreateBuildTriggerRequest{Parent: "projects/proj-p/locations/global", Trigger: bad}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a bad topic: %v; want InvalidArgument", err)
	}

	got, err := cb.CreateBuildTrigger(context.Background(), &cbpb.CreateBuildTriggerRequest{Parent: "projects/proj-p/locations/global", Trigger: tr})
	if err != nil {
		t.Fatal(err)
	}
	sub := got.GetPubsubConfig().GetSubscription()
	if sub != "projects/proj-p/subscriptions/gcb-"+got.GetId() || got.GetPubsubConfig().GetState() != cbpb.PubsubConfig_OK {
		t.Fatalf("pubsubConfig = %v", got.GetPubsubConfig())
	}
	ps := subs.subs[sub]
	if ps.Topic != "projects/proj-p/topics/t1" || ps.Endpoint != "http://host:1"+cloudBuildPushPrefix+got.GetResourceName() {
		t.Fatalf("subscription = %+v", ps)
	}

	data := base64.StdEncoding.EncodeToString([]byte(`{"tag":"v7","action":"INSERT"}`))
	env := `{"message":{"data":"` + data + `","attributes":{"bucketId":"bk"},"messageId":"1"},"subscription":"` + sub + `"}`
	w := httptest.NewRecorder()
	if !servePush(cb, w, httptest.NewRequest(http.MethodPost, ps.Endpoint[len("http://host:1"):], strings.NewReader(env))) || w.Code != http.StatusNoContent {
		t.Fatalf("push = %d", w.Code)
	}
	spec := <-b.got
	if spec.LocalImage != localImageFor("x/y:v7-bk") {
		t.Errorf("image = %q; want the message's bindings", spec.LocalImage)
	}

	// A new topic moves the subscription.
	next, err := cb.UpdateBuildTrigger(context.Background(), &cbpb.UpdateBuildTriggerRequest{
		Trigger:    &cbpb.BuildTrigger{ResourceName: got.GetResourceName(), PubsubConfig: &cbpb.PubsubConfig{Topic: "projects/proj-p/topics/t2"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"pubsub_config"}}})
	if err != nil {
		t.Fatal(err)
	}
	if next.GetPubsubConfig().GetSubscription() != sub || subs.subs[sub].Topic != "projects/proj-p/topics/t2" {
		t.Errorf("after update: %v, %+v", next.GetPubsubConfig(), subs.subs[sub])
	}
	if _, err := cb.DeleteBuildTrigger(context.Background(), &cbpb.DeleteBuildTriggerRequest{Name: got.GetResourceName()}); err != nil {
		t.Fatal(err)
	}
	if _, ok := subs.subs[sub]; ok {
		t.Error("the subscription survived its trigger")
	}
	// A push for a deleted trigger is acknowledged.
	w = httptest.NewRecorder()
	servePush(cb, w, httptest.NewRequest(http.MethodPost, cloudBuildPushPrefix+got.GetResourceName(), strings.NewReader(env)))
	if w.Code != http.StatusNoContent {
		t.Errorf("a push for a deleted trigger = %d", w.Code)
	}
}
