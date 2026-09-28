package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/console"
	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// pstestPublisher delivers the storage server's notifications to a pstest
// server through the official Pub/Sub client, as the storage server's own
// emulator publisher does.
type pstestPublisher struct{ c *pubsub.Client }

func (p pstestPublisher) Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
	pb := p.c.Publisher(topic)
	defer pb.Stop()
	_, err := pb.Publish(ctx, &pubsub.Message{Data: data, Attributes: attrs}).Get(ctx)
	return err
}

// notifyRig is a console over an in-process storage server and a pstest
// Pub/Sub, with the official clients of both.
type notifyRig struct {
	p   storageProvider
	srv *gcsbuiltin.Server
	con *httptest.Server
	sc  *storage.Client
	ps  *pubsub.Client
}

// newNotifyRig builds one. withPublisher starts the storage server with a
// Pub/Sub to deliver to; consolePubSub tells the console where Pub/Sub is.
func newNotifyRig(t *testing.T, withPublisher, consolePubSub bool) notifyRig {
	t.Helper()
	ctx := context.Background()
	fake := pstest.NewServer()
	t.Cleanup(func() { _ = fake.Close() })
	ps, err := pubsub.NewClient(ctx, "p", option.WithEndpoint(fake.Addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	opts := gcsbuiltin.Options{}
	if withPublisher {
		opts.Publisher = pstestPublisher{ps}
	}
	srv, err := gcsbuiltin.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv)
	t.Cleanup(h.Close)
	p := storageProvider{endpoint: strings.TrimPrefix(h.URL, "http://")}
	if consolePubSub {
		p.pubsub = fake.Addr
	}
	if _, err := p.Create(ctx, "p", map[string]string{"name": "notes"}); err != nil {
		t.Fatal(err)
	}
	sc, err := p.storageClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	con := httptest.NewServer(console.New("127.0.0.1:0", nil, p).Handler())
	t.Cleanup(con.Close)
	return notifyRig{p: p, srv: srv, con: con, sc: sc, ps: ps}
}

// act posts an action through the console's route, which checks it against
// the page's DetailActions first.
func (r notifyRig) act(t *testing.T, path []string, action string, values map[string]string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
	resp, err := http.Post(r.con.URL+"/api/actions/storage?project=p", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func (r notifyRig) topic(t *testing.T, id string) string {
	t.Helper()
	name := "projects/p/topics/" + id
	if _, err := r.ps.TopicAdminClient.CreateTopic(context.Background(), &pubsubpb.Topic{Name: name}); err != nil {
		t.Fatal(err)
	}
	return name
}

func notificationsTab(t *testing.T, d console.Detail) console.Section {
	t.Helper()
	for _, s := range d.Sections {
		if s.ID == "notifications" {
			return s
		}
	}
	t.Fatalf("the bucket's page has no Notifications tab: %+v", d.Sections)
	return console.Section{}
}

func hasAction(actions []console.Action, id string) bool {
	for _, a := range actions {
		if a.ID == id {
			return true
		}
	}
	return false
}

// A notification created through the console's route is what the official
// client's BucketHandle.Notifications lists, an upload afterwards publishes a
// message the official Pub/Sub client pulls from the topic, and a delete from
// its row leaves the client's list without it (#791).
func TestStorageNotificationsThroughTheAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newNotifyRig(t, true, true)
	topic := r.topic(t, "uploads")
	r.topic(t, "audit")
	sub, err := r.ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: "projects/p/subscriptions/uploads-sub", Topic: topic})
	if err != nil {
		t.Fatal(err)
	}

	d, err := r.p.Detail(ctx, "p", []string{"notes"})
	if err != nil {
		t.Fatal(err)
	}
	if tab := notificationsTab(t, d); len(tab.Listing.Items) != 0 || tab.Unavailable != "" {
		t.Errorf("a new bucket's Notifications tab reads %+v", tab)
	}
	create := actionByID(t, d.Actions, "createnotification")
	if got := create.Fields[0].Options; strings.Join(got, ",") != "projects/p/topics/audit,projects/p/topics/uploads" {
		t.Errorf("the topic picker offers %v, want the project's two topics", got)
	}
	if !hasAction(r.p.DetailActions(ctx, "p", []string{"notes"}), "createnotification") {
		t.Error("the route's DetailActions do not offer Create notification")
	}
	if hasAction(r.p.DetailActions(ctx, "p", []string{"notes", "folder"}), "createnotification") {
		t.Error("a folder's page offers Create notification")
	}
	for _, s := range d.Sections {
		for _, a := range s.Listing.SelectActions {
			if a.ID == "createnotification" {
				t.Error("Create notification is offered as an action on checked objects")
			}
		}
	}

	values := formValues(create)
	values["topic"] = topic
	values["event_"+storage.ObjectFinalizeEvent] = "true"
	values["prefix"] = "in/"
	values["attributes"] = `{"team":"blue"}`
	if code, body := r.act(t, []string{"notes"}, "createnotification", values); code != http.StatusOK {
		t.Fatalf("create notification = %d: %s", code, body)
	}
	all, err := r.sc.Bucket("notes").Notifications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("the client lists %d notifications, want 1: %v", len(all), all)
	}
	var n *storage.Notification
	for _, v := range all {
		n = v
	}
	if n.TopicProjectID != "p" || n.TopicID != "uploads" || n.PayloadFormat != storage.JSONPayload ||
		n.ObjectNamePrefix != "in/" || n.CustomAttributes["team"] != "blue" ||
		strings.Join(n.EventTypes, ",") != storage.ObjectFinalizeEvent {
		t.Errorf("the client reads the notification as %+v", n)
	}

	// Its row, and its page from notifications.get.
	d, _ = r.p.Detail(ctx, "p", []string{"notes"})
	rows := notificationsTab(t, d).Listing.Items
	if len(rows) != 1 || rows[0].Name != "notificationConfigs/"+n.ID || rows[0].Fields["Topic"] != topic ||
		rows[0].Fields["Event types"] != storage.ObjectFinalizeEvent || rows[0].Fields["Custom attributes"] != "team=blue" ||
		!hasAction(rows[0].Actions, "deletenotification") {
		t.Errorf("the Notifications tab lists %+v", rows)
	}
	page, _ := r.p.Detail(ctx, "p", rows[0].Opens)
	if page.Unavailable != "" || len(page.Sections) != 1 {
		t.Fatalf("the notification's page reads %+v", page)
	}
	props := map[string]string{}
	for _, g := range page.Sections[0].Groups {
		for _, pr := range g.Properties {
			props[g.Heading+"/"+pr.Label] = pr.Value
		}
	}
	if props["Delivery/Topic"] != topic || props["Delivery/Object name prefix"] != "in/" ||
		props["Delivery/Payload format"] != "JSON_API_V1" || props["Custom attributes/team"] != "blue" {
		t.Errorf("the notification's page reads %v", props)
	}

	// An upload in the prefix is delivered to the topic.
	putObject(t, r.sc, "notes", "out/skip.txt", "no", nil)
	putObject(t, r.sc, "notes", "in/a.txt", "yes", nil)
	if left, err := r.srv.DispatchNotifications(ctx); err != nil || left != 0 {
		t.Fatalf("dispatch left %d (%v)", left, err)
	}
	var mu sync.Mutex
	var got []*pubsub.Message
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = r.ps.Subscriber(sub.GetName()).Receive(rctx, func(_ context.Context, m *pubsub.Message) {
		m.Ack()
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		cancel()
	})
	if len(got) != 1 || got[0].Attributes["objectId"] != "in/a.txt" || got[0].Attributes["team"] != "blue" ||
		got[0].Attributes["eventType"] != storage.ObjectFinalizeEvent {
		t.Fatalf("the subscription received %v", got)
	}

	// A topic the picker did not offer, and a value the API refuses.
	values["topic"] = "projects/p/topics/missing"
	if code, body := r.act(t, []string{"notes"}, "createnotification", values); code == http.StatusOK || !strings.Contains(body, "not one of project p") {
		t.Errorf("a topic not offered = %d: %s", code, body)
	}
	values["topic"] = topic
	many := map[string]string{}
	for i := 0; i < 11; i++ {
		many[fmt.Sprintf("k%d", i)] = "v"
	}
	raw, _ := json.Marshal(many)
	values["attributes"] = string(raw)
	if code, body := r.act(t, []string{"notes"}, "createnotification", values); code == http.StatusOK || !strings.Contains(body, "at most 10 custom attributes") {
		t.Errorf("eleven custom attributes = %d: %s, want the API's refusal", code, body)
	}

	// Delete, from the row: the path its page has.
	if code, body := r.act(t, rows[0].Opens, "deletenotification", nil); code != http.StatusOK {
		t.Fatalf("delete = %d: %s", code, body)
	}
	if all, err := r.sc.Bucket("notes").Notifications(ctx); err != nil || len(all) != 0 {
		t.Errorf("after the console's delete the client lists %v (%v)", all, err)
	}
	if gone, _ := r.p.Detail(ctx, "p", rows[0].Opens); !strings.Contains(gone.Unavailable, "no notification configuration") {
		t.Errorf("a deleted notification's page reads %+v", gone)
	}
	if code, _ := r.act(t, rows[0].Opens, "deletenotification", nil); code == http.StatusOK {
		t.Error("a second delete of the same notification was accepted")
	}
}

// With no Pub/Sub, Create notification is not offered, the tab says why in
// the storage server's own 501 message, the route refuses it, and what the
// bucket already has is still listed and can be deleted (#791).
func TestStorageNotificationsWithoutPubSub(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newNotifyRig(t, false, false)

	d, err := r.p.Detail(ctx, "p", []string{"notes"})
	if err != nil {
		t.Fatal(err)
	}
	if hasAction(d.Actions, "createnotification") || hasAction(r.p.DetailActions(ctx, "p", []string{"notes"}), "createnotification") {
		t.Error("Create notification is offered with no Pub/Sub")
	}
	tab := notificationsTab(t, d)
	if !strings.Contains(tab.Listing.Note, "501") ||
		!strings.Contains(tab.Listing.Note, "notifications need a Pub/Sub emulator to deliver to") {
		t.Errorf("the tab's note is %q, want the server's 501 message", tab.Listing.Note)
	}
	if code, body := r.act(t, []string{"notes"}, "createnotification", map[string]string{"topic": "projects/p/topics/t"}); code == http.StatusOK || !strings.Contains(body, "not available") {
		t.Errorf("create with no Pub/Sub = %d: %s", code, body)
	}
	if all, err := r.sc.Bucket("notes").Notifications(ctx); err != nil || len(all) != 0 {
		t.Errorf("the probe left %v (%v)", all, err)
	}
}

// A console that knows Pub/Sub over a storage server that does not deliver:
// the create is refused with the server's own 501 message, on the form. And a
// project with no topic has nothing to publish to, so nothing is offered.
func TestStorageNotificationRefusalsAreTheServers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newNotifyRig(t, false, true)
	d, _ := r.p.Detail(ctx, "p", []string{"notes"})
	if hasAction(d.Actions, "createnotification") {
		t.Error("Create notification is offered in a project with no topics")
	}
	if note := notificationsTab(t, d).Listing.Note; !strings.Contains(note, "has no Pub/Sub topics") {
		t.Errorf("the tab's note is %q", note)
	}

	topic := r.topic(t, "t")
	d, _ = r.p.Detail(ctx, "p", []string{"notes"})
	values := formValues(actionByID(t, d.Actions, "createnotification"))
	values["topic"] = topic
	code, body := r.act(t, []string{"notes"}, "createnotification", values)
	if code == http.StatusOK || !strings.Contains(body, "501") || !strings.Contains(body, "notifications need a Pub/Sub emulator") {
		t.Errorf("create against a server with no Pub/Sub = %d: %s, want its 501", code, body)
	}
}
