package main

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestConsoleSecretTopics (#1259): Create secret and Edit secret take the
// Pub/Sub notification topics, the page shows them, and each change is
// published to them.
func TestConsoleSecretTopics(t *testing.T) {
	p := secretsFixture(t)
	ctx := context.Background()
	var mu sync.Mutex
	var events []string
	p.svc.Store().SetPublisher(func(_ context.Context, topic string, _ []byte, attrs map[string]string) error {
		if !strings.HasSuffix(topic, "/ok") && !strings.HasSuffix(topic, "/also") {
			return status.Error(codes.NotFound, "Topic not found")
		}
		mu.Lock()
		defer mu.Unlock()
		events = append(events, topic+" "+attrs["eventType"])
		return nil
	})
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "a", "payload": "x",
		"topics": "projects/demo/topics/ok"}); err != nil {
		t.Fatal(err)
	}
	d, err := p.Detail(ctx, "demo", []string{"projects/demo/secrets/a"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detailText(d), "projects/demo/topics/ok") {
		t.Error("the page does not show the topic")
	}
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "b", "payload": "x",
		"topics": "projects/demo/topics/missing"}); err == nil {
		t.Error("a topic that does not exist was accepted")
	}
	if err := p.Edit(ctx, "demo", []string{"projects/demo/secrets/a"}, map[string]string{
		"topics": "projects/demo/topics/ok\nprojects/demo/topics/also"}); err != nil {
		t.Fatal(err)
	}
	a, _ := p.svc.Store().GetSecret("demo", "a")
	if !slices.Equal(a.Topics, []string{"projects/demo/topics/ok", "projects/demo/topics/also"}) {
		t.Errorf("topics after Edit = %v", a.Topics)
	}
	want := []string{
		"projects/demo/topics/ok TOPIC_CONFIGURED", "projects/demo/topics/ok SECRET_CREATE",
		"projects/demo/topics/ok SECRET_VERSION_ADD",
		"projects/demo/topics/ok TOPIC_CONFIGURED", "projects/demo/topics/also TOPIC_CONFIGURED",
		"projects/demo/topics/ok SECRET_UPDATE", "projects/demo/topics/also SECRET_UPDATE",
	}
	mu.Lock()
	got := slices.Clone(events)
	mu.Unlock()
	if !slices.Equal(got, want) {
		t.Errorf("events = %v\nwant %v", got, want)
	}
	if err := p.Edit(ctx, "demo", []string{"projects/demo/secrets/a"}, map[string]string{"topics": ""}); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Detail(ctx, "demo", []string{"projects/demo/secrets/a"})
	if a, _ = p.svc.Store().GetSecret("demo", "a"); len(a.Topics) != 0 || !strings.Contains(detailText(d), "None") {
		t.Errorf("clearing the topics left %v", a.Topics)
	}
}
