package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// pubsubConsole serves a console whose Pub/Sub screen talks to an in-process
// pstest server, and returns it with an SDK client of the same server.
func pubsubConsole(t *testing.T, project string) (*httptest.Server, *pubsub.Client) {
	t.Helper()
	fake := pstest.NewServer()
	t.Cleanup(func() { _ = fake.Close() })
	c, err := pubsub.NewClient(context.Background(), project,
		option.WithEndpoint(fake.Addr),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, pubsubProvider{endpoint: fake.Addr}).Handler())
	t.Cleanup(srv.Close)
	return srv, c
}

func pubsubAct(t *testing.T, srv *httptest.Server, project string, path []string, action string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Path": path, "Action": action})
	resp, err := http.Post(srv.URL+"/api/actions/pubsub?project="+project, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestATopicPageDeletesItsSubscriptionRows covers #595 (PR 2).
//
// Each subscription row on a topic page offers Delete, addressed as [topic,
// subscription]; performing it removes the subscription through the official
// client, so the SDK's GetSubscription answers NOT_FOUND. A subscription of
// another topic or another project cannot be reached through the page.
func TestATopicPageDeletesItsSubscriptionRows(t *testing.T) {
	const project = "p1"
	srv, ps := pubsubConsole(t, project)
	ctx := context.Background()

	topic := "projects/p1/topics/orders"
	other := "projects/p1/topics/other"
	sub := "projects/p1/subscriptions/orders-sub"
	otherSub := "projects/p1/subscriptions/other-sub"
	for _, tp := range []string{topic, other} {
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: tp}); err != nil {
			t.Fatal(err)
		}
	}
	for s, tp := range map[string]string{sub: topic, otherSub: other} {
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: s, Topic: tp}); err != nil {
			t.Fatal(err)
		}
	}

	// The topic page's row carries the action the route accepts.
	resp, err := http.Get(srv.URL + "/api/detail/pubsub?project=" + project + "&name=" + url.QueryEscape(topic))
	if err != nil {
		t.Fatal(err)
	}
	var detail console.Detail
	err = json.NewDecoder(resp.Body).Decode(&detail)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Sections) != 1 || len(detail.Sections[0].Listing.Items) != 1 {
		t.Fatalf("topic page sections = %+v, want one subscription row", detail.Sections)
	}
	row := detail.Sections[0].Listing.Items[0]
	if row.Name != sub || len(row.Actions) != 1 || row.Actions[0].ID != actDeleteSubscription || !row.Actions[0].Destructive {
		t.Fatalf("subscription row = %+v, want %s with one destructive %s", row, sub, actDeleteSubscription)
	}
	for _, a := range detail.Actions {
		if a.ID == actDeleteSubscription {
			t.Error("the topic page itself offers a subscription delete")
		}
	}

	// A subscription of another topic is refused through this topic's page,
	// and survives.
	if code, out := pubsubAct(t, srv, project, []string{topic, otherSub}, actDeleteSubscription); code != http.StatusBadRequest ||
		!strings.Contains(out, "is not a subscription of") {
		t.Errorf("deleting another topic's subscription = %d %s, want 400 naming the mismatch", code, out)
	}
	if _, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: otherSub}); err != nil {
		t.Errorf("another topic's subscription was touched: %v", err)
	}
	// Another project's subscription name is refused before any call.
	if code, out := pubsubAct(t, srv, project, []string{topic, "projects/p2/subscriptions/x"}, actDeleteSubscription); code != http.StatusBadRequest ||
		!strings.Contains(out, "is not a subscription of project p1") {
		t.Errorf("deleting another project's subscription = %d %s, want 400", code, out)
	}
	// A topic-level action is not accepted on a subscription row.
	if code, _ := pubsubAct(t, srv, project, []string{topic, sub}, actPublish); code != http.StatusBadRequest {
		t.Errorf("publish on a subscription row = %d, want 400", code)
	}

	// The delete itself.
	if code, out := pubsubAct(t, srv, project, []string{topic, sub}, actDeleteSubscription); code != http.StatusOK {
		t.Fatalf("delete = %d %s", code, out)
	}
	_, err = ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if status.Code(err) != codes.NotFound {
		t.Errorf("GetSubscription after a console delete = %v, want NOT_FOUND", err)
	}
	// Deleting it again reports the backend's NOT_FOUND rather than success.
	if code, out := pubsubAct(t, srv, project, []string{topic, sub}, actDeleteSubscription); code != http.StatusBadRequest ||
		!strings.Contains(strings.ReplaceAll(strings.ToLower(out), " ", ""), "notfound") {
		t.Errorf("a second delete = %d %s, want 400 with the backend's not found", code, out)
	}

	// The path has actions but no page, and the page that says so offers none.
	resp, err = http.Get(srv.URL + "/api/detail/pubsub?project=" + project +
		"&name=" + url.QueryEscape(topic) + "&name=" + url.QueryEscape(otherSub))
	if err != nil {
		t.Fatal(err)
	}
	var deeper console.Detail
	_ = json.NewDecoder(resp.Body).Decode(&deeper)
	_ = resp.Body.Close()
	if deeper.Unavailable == "" || len(deeper.Actions) != 0 {
		t.Errorf("a subscription path's page = %+v, want unavailable with no actions", deeper)
	}
}
