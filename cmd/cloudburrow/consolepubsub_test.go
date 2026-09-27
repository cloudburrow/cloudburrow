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

// subscriptionsConsole serves a console with both Pub/Sub screens against an
// in-process pstest server, and returns it with an SDK client of that server.
func subscriptionsConsole(t *testing.T, project string) (*httptest.Server, *pubsub.Client) {
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
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil,
		pubsubProvider{endpoint: fake.Addr}, pubsubSubscriptionsProvider{endpoint: fake.Addr}).Handler())
	t.Cleanup(srv.Close)
	return srv, c
}

func getJSON200(t *testing.T, u string, into any) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s = %d: %s", u, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatal(err)
	}
}

func consoleDelete(t *testing.T, srv *httptest.Server, service, project, name string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/resources/"+service+
		"?project="+project+"&name="+url.QueryEscape(name), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// detailProps flattens a detail page's summary and property sections into
// label → value, keyed by section ID ("" for the summary).
func detailProps(d console.Detail) map[string]map[string]string {
	out := map[string]map[string]string{"": {}}
	for _, p := range d.Summary {
		out[""][p.Label] = p.Value
	}
	for _, s := range d.Sections {
		out[s.ID] = map[string]string{}
		for _, g := range s.Groups {
			for _, p := range g.Properties {
				out[s.ID][p.Label] = p.Value
			}
		}
	}
	return out
}

// TestTheSubscriptionsScreenListsOpensAndDeletes covers #595 (PR 3).
//
// Every subscription of the project is listed through ListSubscriptions with
// its topic, delivery type and ack deadline — including one whose topic was
// deleted, which the topic page cannot reach. Its page shows the push
// configuration and dead-letter policy the SDK set. Deleting it from the
// screen removes it through the official client, so GetSubscription answers
// NOT_FOUND; another project's subscription cannot be reached.
func TestTheSubscriptionsScreenListsOpensAndDeletes(t *testing.T) {
	const project = "p1"
	srv, ps := subscriptionsConsole(t, project)
	ctx := context.Background()

	orders := "projects/p1/topics/orders"
	dead := "projects/p1/topics/orders-dead"
	gone := "projects/p1/topics/gone"
	elsewhere := "projects/p2/topics/elsewhere"
	for _, tp := range []string{orders, dead, gone, elsewhere} {
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: tp}); err != nil {
			t.Fatal(err)
		}
	}
	pull := "projects/p1/subscriptions/orders-pull"
	push := "projects/p1/subscriptions/orders-push"
	orphan := "projects/p1/subscriptions/orphan"
	foreign := "projects/p2/subscriptions/foreign"
	for _, s := range []*pubsubpb.Subscription{
		{Name: pull, Topic: orders, AckDeadlineSeconds: 20,
			DeadLetterPolicy: &pubsubpb.DeadLetterPolicy{DeadLetterTopic: dead, MaxDeliveryAttempts: 7}},
		{Name: push, Topic: orders, AckDeadlineSeconds: 30,
			PushConfig: &pubsubpb.PushConfig{PushEndpoint: "https://example.invalid/push",
				Attributes: map[string]string{"x-goog-version": "v1"}}},
		{Name: orphan, Topic: gone},
		{Name: foreign, Topic: elsewhere},
	} {
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := ps.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: gone}); err != nil {
		t.Fatal(err)
	}

	// The screen is advertised with detail and delete.
	var services struct {
		Services []struct {
			ID     string
			Delete bool
			Detail bool
		}
	}
	getJSON200(t, srv.URL+"/api/services", &services)
	advertised := false
	for _, s := range services.Services {
		advertised = advertised || (s.ID == "pubsub-subscriptions" && s.Delete && s.Detail)
	}
	if !advertised {
		t.Fatalf("pubsub-subscriptions is not advertised with detail and delete: %+v", services)
	}

	// The list: this project's subscriptions only, with their columns.
	var listing console.Listing
	getJSON200(t, srv.URL+"/api/resources/pubsub-subscriptions?project="+project, &listing)
	if strings.Join(listing.Columns, ",") != "Topic,Delivery type,Ack deadline" || listing.NameColumn != "Subscription" {
		t.Errorf("columns = %q named %q", listing.Columns, listing.NameColumn)
	}
	rows := map[string]map[string]string{}
	for _, it := range listing.Items {
		rows[it.Name] = it.Fields
	}
	want := map[string]map[string]string{
		pull:   {"Topic": orders, "Delivery type": "Pull", "Ack deadline": "20s"},
		push:   {"Topic": orders, "Delivery type": "Push", "Ack deadline": "30s"},
		orphan: {"Topic": "(topic deleted)", "Delivery type": "Pull"},
	}
	if len(rows) != len(want) || listing.Total != len(want) {
		t.Errorf("listed %v, want exactly %d subscriptions of %s", rows, len(want), project)
	}
	for name, fields := range want {
		for k, v := range fields {
			if rows[name][k] != v {
				t.Errorf("%s %s = %q, want %q", name, k, rows[name][k], v)
			}
		}
	}

	// With no project, a prompt rather than an error or someone else's rows.
	var none console.Listing
	getJSON200(t, srv.URL+"/api/resources/pubsub-subscriptions", &none)
	if none.Prompt == "" || len(none.Items) != 0 {
		t.Errorf("no project = %+v, want a prompt", none)
	}

	detail := func(name string) console.Detail {
		t.Helper()
		var d console.Detail
		getJSON200(t, srv.URL+"/api/detail/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(name), &d)
		return d
	}

	// The push subscription's page shows its push configuration.
	pd := detail(push)
	if pd.Unavailable != "" {
		t.Fatalf("push subscription page unavailable: %s", pd.Unavailable)
	}
	props := detailProps(pd)
	if props[""]["Topic"] != orders || props[""]["Delivery type"] != "Push" || props[""]["Ack deadline"] != "30s" {
		t.Errorf("push summary = %v", props[""])
	}
	if props["delivery"]["Endpoint"] != "https://example.invalid/push" || props["delivery"]["Attribute x-goog-version"] != "v1" {
		t.Errorf("push delivery = %v", props["delivery"])
	}
	if props["dead-lettering"]["Dead-letter topic"] != "None" {
		t.Errorf("push dead lettering = %v, want None", props["dead-lettering"])
	}

	// The dead-lettered one shows its policy.
	props = detailProps(detail(pull))
	if props["dead-lettering"]["Dead-letter topic"] != dead || props["dead-lettering"]["Maximum delivery attempts"] != "7" {
		t.Errorf("dead-letter policy = %v, want %s after 7", props["dead-lettering"], dead)
	}
	if props["delivery"]["Push endpoint"] == "" {
		t.Errorf("pull delivery = %v", props["delivery"])
	}

	// Another project's subscription has no page here, and nothing deeper.
	if d := detail(foreign); d.Unavailable == "" {
		t.Errorf("another project's subscription opened: %+v", d)
	}
	var deeper console.Detail
	getJSON200(t, srv.URL+"/api/detail/pubsub-subscriptions?project="+project+
		"&name="+url.QueryEscape(pull)+"&name=x", &deeper)
	if deeper.Unavailable == "" {
		t.Errorf("a path below a subscription opened: %+v", deeper)
	}

	// Delete: another project's is refused and survives.
	if code, out := consoleDelete(t, srv, "pubsub-subscriptions", project, foreign); code != http.StatusBadRequest ||
		!strings.Contains(out, "is not a subscription of project p1") {
		t.Errorf("deleting another project's subscription = %d %s", code, out)
	}
	if _, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: foreign}); err != nil {
		t.Errorf("another project's subscription was touched: %v", err)
	}

	// The push subscription and the orphan both go, and the SDK agrees.
	for _, name := range []string{push, orphan} {
		if code, out := consoleDelete(t, srv, "pubsub-subscriptions", project, name); code != http.StatusOK {
			t.Fatalf("delete %s = %d %s", name, code, out)
		}
		_, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
		if status.Code(err) != codes.NotFound {
			t.Errorf("GetSubscription(%s) after a console delete = %v, want NOT_FOUND", name, err)
		}
	}
	// A second delete reports the backend's NOT_FOUND rather than success.
	if code, out := consoleDelete(t, srv, "pubsub-subscriptions", project, push); code != http.StatusBadRequest ||
		!strings.Contains(strings.ReplaceAll(strings.ToLower(out), " ", ""), "notfound") {
		t.Errorf("a second delete = %d %s, want 400 with the backend's not found", code, out)
	}
	var after console.Listing
	getJSON200(t, srv.URL+"/api/resources/pubsub-subscriptions?project="+project, &after)
	if len(after.Items) != 1 || after.Items[0].Name != pull {
		t.Errorf("after the deletes the screen lists %+v, want only %s", after.Items, pull)
	}
}
