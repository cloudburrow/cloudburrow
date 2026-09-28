//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	apiv1 "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// staleAck pulls a message from sub, waits past its 10s deadline for
// its redelivery under a new ack ID, and acknowledges the first ack ID,
// returning that call's error, and acknowledges the new one.
func staleAck(t *testing.T, h *Harness, c *pubsub.Client, topicName, sub string) error {
	t.Helper()
	ctx := h.Context()
	if _, err := c.Publisher(topicName).Publish(ctx, &pubsub.Message{Data: []byte("x")}).Get(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 1})
	if err != nil || len(first.GetReceivedMessages()) != 1 {
		t.Fatalf("Pull = %v, %v", first, err)
	}
	m1 := first.GetReceivedMessages()[0]
	var m2 *pubsubpb.ReceivedMessage
	for start := time.Now(); m2 == nil && time.Since(start) < 40*time.Second; {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, _ := c.SubscriptionAdminClient.Pull(pctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 1, ReturnImmediately: true})
		cancel()
		if len(resp.GetReceivedMessages()) > 0 {
			m2 = resp.GetReceivedMessages()[0]
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if m2 == nil {
		t.Fatal("not redelivered within 40s of a 10s deadline")
	}
	stale := c.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: []string{m1.GetAckId()}})
	if err := c.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: []string{m2.GetAckId()}}); err != nil {
		t.Errorf("acknowledging the current ack ID = %v", err)
	}
	if stale != nil {
		var info *errdetails.ErrorInfo
		for _, d := range status.Convert(stale).Details() {
			if i, ok := d.(*errdetails.ErrorInfo); ok {
				info = i
			}
		}
		if info == nil || info.GetMetadata()[m1.GetAckId()] != "PERMANENT_FAILURE_INVALID_ACK_ID" {
			t.Errorf("the refusal's ErrorInfo is %v; want the stale ack ID marked PERMANENT_FAILURE_INVALID_ACK_ID", info)
		}
	}
	return stale
}

// TestPubSubExactlyOnceCanBeChanged (#880): UpdateSubscription turns
// exactly-once delivery on for a subscription created without it, and the
// emulator then acts on it: a stale ack ID is refused INVALID_ARGUMENT with
// PERMANENT_FAILURE_INVALID_ACK_ID. Turned off again, it reads back false and
// the same stale acknowledgement is accepted, as on any subscription without
// it. This is what makes Edit subscription's exactly-once field editable.
// covers: google.pubsub.v1.Subscriber/UpdateSubscription
func TestPubSubExactlyOnceCanBeChanged(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	topicName := topic(t, h, c, "eod-change-topic")
	sub := subscription(t, h, c, "eod-change", topicName)
	set := func(on bool) {
		t.Helper()
		got, err := c.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
			Subscription: &pubsubpb.Subscription{Name: sub, EnableExactlyOnceDelivery: on},
			UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"enable_exactly_once_delivery"}}})
		if err != nil || got.GetEnableExactlyOnceDelivery() != on {
			t.Fatalf("UpdateSubscription exactly-once %v = %v, %v", on, got.GetEnableExactlyOnceDelivery(), err)
		}
		read, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
		if err != nil || read.GetEnableExactlyOnceDelivery() != on {
			t.Fatalf("GetSubscription after turning exactly-once %v reads %v, %v", on, read.GetEnableExactlyOnceDelivery(), err)
		}
	}
	set(true)
	if err := staleAck(t, h, c, topicName, sub); status.Code(err) != codes.InvalidArgument {
		t.Errorf("with exactly-once turned on, a stale ack ID = %v; want INVALID_ARGUMENT", err)
	}
	set(false)
	if err := staleAck(t, h, c, topicName, sub); err != nil {
		t.Errorf("with exactly-once turned off, a stale ack ID = %v; want it accepted", err)
	}
}

// TestPubSubRefusesExactlyOnceWithPush (#880): the emulator accepts
// exactly-once delivery on a push subscription; Google does not ("Push and
// export subscriptions don't support exactly-once delivery"), and nor does
// CloudBurrow: CreateSubscription with both, UpdateSubscription giving an
// exactly-once subscription a push endpoint or a push subscription
// exactly-once, and ModifyPushConfig on an exactly-once subscription are
// refused INVALID_ARGUMENT and change nothing. A seed file takes
// enableExactlyOnceDelivery on a pull subscription and refuses it with a push
// endpoint, seeding nothing.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Subscriber/ModifyPushConfig
// unverified: google.pubsub.v1.Subscriber/CreateSubscription INVALID_ARGUMENT: exactly-once delivery with a push endpoint (Google documents the rule, not the code)
func TestPubSubRefusesExactlyOnceWithPush(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	topicName := topic(t, h, c, "eod-push-topic")
	name := func(id string) string { return fmt.Sprintf("projects/%s/subscriptions/%s", h.Project(), id) }
	push := &pubsubpb.PushConfig{PushEndpoint: "http://127.0.0.1:9/push"}
	for _, id := range []string{"eod-both", "eod-pull", "eod-pushed"} {
		n := name(id)
		t.Cleanup(func() {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: n})
		})
	}
	_, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: name("eod-both"), Topic: topicName,
		EnableExactlyOnceDelivery: true, PushConfig: push})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateSubscription with exactly-once and a push endpoint = %v; want INVALID_ARGUMENT", err)
	}
	if _, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name("eod-both")}); status.Code(err) != codes.NotFound {
		t.Errorf("refused, and then reads %v", err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: name("eod-pull"), Topic: topicName,
		EnableExactlyOnceDelivery: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: name("eod-pushed"), Topic: topicName,
		PushConfig: push}); err != nil {
		t.Fatal(err)
	}
	update := func(s *pubsubpb.Subscription, path string) error {
		_, err := c.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
			Subscription: s, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{path}}})
		return err
	}
	if err := update(&pubsubpb.Subscription{Name: name("eod-pull"), PushConfig: push}, "push_config"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a push endpoint for an exactly-once subscription = %v; want INVALID_ARGUMENT", err)
	}
	if err := update(&pubsubpb.Subscription{Name: name("eod-pushed"), EnableExactlyOnceDelivery: true},
		"enable_exactly_once_delivery"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("exactly-once for a push subscription = %v; want INVALID_ARGUMENT", err)
	}
	if err := c.SubscriptionAdminClient.ModifyPushConfig(ctx, &pubsubpb.ModifyPushConfigRequest{
		Subscription: name("eod-pull"), PushConfig: push}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ModifyPushConfig on an exactly-once subscription = %v; want INVALID_ARGUMENT", err)
	}
	for id, eod := range map[string]bool{"eod-pull": true, "eod-pushed": false} {
		s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name(id)})
		if err != nil || s.GetEnableExactlyOnceDelivery() != eod || (s.GetPushConfig().GetPushEndpoint() == "") != eod {
			t.Errorf("%s after the refusals: %v, %v", id, s, err)
		}
	}

	// The seed file, through the admin API.
	control := os.Getenv(EnvControl)
	if control == "" {
		return
	}
	seed := func(withPush bool) (int, string) {
		pc := ""
		if withPush {
			pc = `, "pushConfig": {"pushEndpoint": "http://127.0.0.1:9/push"}`
		}
		return adminSeed(t, control, fmt.Sprintf(`{"components": {"pubsub": {"subscriptions": [
			{"name": "projects/%[1]s/subscriptions/eod-seeded", "topic": %[2]q, "enableExactlyOnceDelivery": true%[3]s}]}}}`,
			h.Project(), topicName, pc))
	}
	t.Cleanup(func() {
		_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: name("eod-seeded")})
	})
	if code, body := seed(true); code != http.StatusBadRequest || !strings.Contains(body, "pull subscriptions only") {
		t.Errorf("seeding exactly-once with a push endpoint = %d %s; want 400", code, body)
	}
	if code, body := seed(false); code != http.StatusOK {
		t.Fatalf("seeding exactly-once = %d %s", code, body)
	}
	if s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name("eod-seeded")}); err != nil ||
		!s.GetEnableExactlyOnceDelivery() {
		t.Errorf("the seeded subscription reads %v, %v", s, err)
	}
}

// storageObjectText reads an object, or "" when it is not there.
func storageObjectText(ctx context.Context, sc *storage.Client, bucket, name string) string {
	r, err := sc.Bucket(bucket).Object(name).NewReader(ctx)
	if err != nil {
		return ""
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// TestPubSubPushSubscriptionExpiresWithoutSuccessfulPushes (#880): Google
// counts "successful pushes" as a subscription's activity. The emulator
// makes the pushes, and CloudBurrow's front relays them, so it sees each
// answer: a push subscription whose endpoint accepts its pushes (the builtin
// storage server's upload endpoint, in the cluster, which stores each push's
// body as an object) is kept, and one whose endpoint refuses them (an upload
// to a bucket that does not exist, 404) expires once idle for its ttl. The
// endpoint receives the emulator's push envelope unchanged, and every
// client reads the real endpoint back, never the relay's.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Subscriber/ListSubscriptions
func TestPubSubPushSubscriptionExpiresWithoutSuccessfulPushes(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	c := pubsubClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bucket := h.Project() + "-pushed"
	if err := sc.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		it := sc.Bucket(bucket).Objects(context.Background(), nil)
		for o, err := it.Next(); err == nil; o, err = it.Next() {
			_ = sc.Bucket(bucket).Object(o.Name).Delete(context.Background())
		}
		_ = sc.Bucket(bucket).Delete(context.Background())
	})
	topicName := topic(t, h, c, "push-expiry-topic")
	upload := func(b string) string {
		return "http://storage:4443/upload/storage/v1/b/" + b + "/o?uploadType=media&name=pushed"
	}
	name := func(id string) string {
		return fmt.Sprintf("projects/%s/subscriptions/push-expiry-%s", h.Project(), id)
	}
	endpoints := map[string]string{"accepted": upload(bucket), "refused": upload(h.Project() + "-absent")}
	for id, ep := range endpoints {
		n := name(id)
		got, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: n, Topic: topicName,
			ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: days(1)}, MessageRetentionDuration: days(1),
			PushConfig: &pubsubpb.PushConfig{PushEndpoint: ep}})
		if err != nil || got.GetPushConfig().GetPushEndpoint() != ep {
			t.Fatalf("CreateSubscription %s = %v, %v; want the endpoint read back as given", n, got.GetPushConfig(), err)
		}
		t.Cleanup(func() {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: n})
		})
	}
	if got, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name("accepted")}); err != nil ||
		got.GetPushConfig().GetPushEndpoint() != endpoints["accepted"] {
		t.Errorf("GetSubscription reads %v, %v; want the real endpoint", got.GetPushConfig(), err)
	}

	// publish sends a message and waits for the accepting endpoint to store
	// its push.
	publish := func(data string) {
		t.Helper()
		if _, err := c.Publisher(topicName).Publish(ctx, &pubsub.Message{Data: []byte(data)}).Get(ctx); err != nil {
			t.Fatal(err)
		}
		want := base64.StdEncoding.EncodeToString([]byte(data))
		for start := time.Now(); time.Since(start) < 60*time.Second; time.Sleep(time.Second) {
			body := storageObjectText(ctx, sc, bucket, "pushed")
			if !strings.Contains(body, want) {
				continue
			}
			var envelope struct {
				Message struct {
					Data string `json:"data"`
				} `json:"message"`
				Subscription string `json:"subscription"`
			}
			if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Message.Data != want ||
				envelope.Subscription != name("accepted") {
				t.Errorf("the endpoint received %s (%v); want the push envelope for %s", body, err, name("accepted"))
			}
			return
		}
		t.Fatalf("the push of %q did not reach the storage server within 60s", data)
	}
	publish("first")
	advancePubSubClock(t, h, 20*time.Hour)
	publish("second")
	advancePubSubClock(t, h, 5*time.Hour)
	got := listedSubscriptions(t, h, c)
	for id, want := range map[string]bool{"accepted": true, "refused": false} {
		if got[name(id)] != want {
			t.Errorf("25h after creation, the push subscription whose endpoint %s its pushes is listed = %v, want %v", id, got[name(id)], want)
		}
	}
	for n := range got {
		s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: n})
		if err == nil && strings.Contains(s.GetPushConfig().GetPushEndpoint(), "/push/") {
			t.Errorf("%s reads the relay's endpoint %s", n, s.GetPushConfig().GetPushEndpoint())
		}
	}
}

// TestPubSubStateSaveAndLoad (#880): `cloudburrow state save` captures
// Pub/Sub, in a project CloudBurrow's registry never heard of: a schema with
// two revisions, a topic that validates against it, subscriptions with their
// settings (exactly-once, filter, retry and dead-letter policies, expiration,
// a push endpoint) and a snapshot. After a reset and `state load`, each
// reads back as it was, the messages are gone (they are not kept), and a
// subscription's expiration clock resumes where it was saved: saved 30h
// into a 48h ttl, it is kept 17h after the load and gone 2h later.
// covers: google.pubsub.v1.SchemaService/CreateSchema, google.pubsub.v1.SchemaService/CommitSchema, google.pubsub.v1.SchemaService/ListSchemaRevisions, google.pubsub.v1.Subscriber/CreateSnapshot, google.pubsub.v1.Subscriber/ListSnapshots
func TestPubSubStateSaveAndLoad(t *testing.T) {
	h := New(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	c := pubsubClient(t, h)
	ctx := h.Context()
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(cli, append(args, flags...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("cloudburrow %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	sch, err := apiv1.NewSchemaClient(ctx, option.WithEndpoint(h.Endpoint(EnvPubSub)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer sch.Close()
	project := "projects/" + h.Project()
	t.Cleanup(func() {
		out, _ := exec.Command(cli, append([]string{"status", "--format", "json"}, flags...)...).Output()
		var st struct {
			ControlURL string `json:"control_url"`
		}
		if json.Unmarshal(out, &st) == nil && st.ControlURL != "" {
			adminReset(t, strings.TrimPrefix(st.ControlURL, "http://"), "service=pubsub&project="+h.Project())
		}
		_ = sch.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: project + "/schemas/state-schema"})
	})

	def := func(field string) string {
		return `{"type":"record","name":"Order","fields":[{"name":"` + field + `","type":"string"}]}`
	}
	schemaName := project + "/schemas/state-schema"
	if _, err := sch.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: project, SchemaId: "state-schema",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: def("id")}}); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	if _, err := sch.CommitSchema(ctx, &pubsubpb.CommitSchemaRequest{Name: schemaName,
		Schema: &pubsubpb.Schema{Name: schemaName, Type: pubsubpb.Schema_AVRO, Definition: def("sku")}}); err != nil {
		t.Fatalf("CommitSchema: %v", err)
	}
	typed, plain, dead := project+"/topics/state-typed", project+"/topics/state-plain", project+"/topics/state-dead"
	topics := []*pubsubpb.Topic{
		{Name: typed, SchemaSettings: &pubsubpb.SchemaSettings{Schema: schemaName, Encoding: pubsubpb.Encoding_JSON}},
		{Name: plain, Labels: map[string]string{"team": "orders"}, MessageRetentionDuration: durationpb.New(2 * time.Hour)},
		{Name: dead},
	}
	for _, tp := range topics {
		if _, err := c.TopicAdminClient.CreateTopic(ctx, tp); err != nil {
			t.Fatalf("CreateTopic %s: %v", tp.Name, err)
		}
	}
	subs := []*pubsubpb.Subscription{
		{Name: project + "/subscriptions/state-once", Topic: plain, AckDeadlineSeconds: 30, EnableExactlyOnceDelivery: true,
			Filter: `attributes.kind = "paid"`, RetainAckedMessages: true,
			ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: days(2)}, MessageRetentionDuration: days(1),
			RetryPolicy:      &pubsubpb.RetryPolicy{MinimumBackoff: durationpb.New(5 * time.Second), MaximumBackoff: durationpb.New(time.Minute)},
			DeadLetterPolicy: &pubsubpb.DeadLetterPolicy{DeadLetterTopic: dead, MaxDeliveryAttempts: 7}},
		{Name: project + "/subscriptions/state-push", Topic: typed,
			PushConfig: &pubsubpb.PushConfig{PushEndpoint: "http://worker.example/push", Attributes: map[string]string{"x-goog-version": "v1"}}},
		{Name: project + "/subscriptions/state-plain", Topic: plain, ExpirationPolicy: &pubsubpb.ExpirationPolicy{}},
	}
	for _, s := range subs {
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, s); err != nil {
			t.Fatalf("CreateSubscription %s: %v", s.Name, err)
		}
	}
	snapshot := project + "/snapshots/state-snap"
	if _, err := c.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: snapshot, Subscription: subs[2].Name}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, err := c.Publisher(plain).Publish(ctx, &pubsub.Message{Data: []byte("not kept"),
		Attributes: map[string]string{"kind": "paid"}}).Get(ctx); err != nil {
		t.Fatal(err)
	}
	// Listed, which is not activity, so the clocks stay as they are.
	readAll := func() map[string]*pubsubpb.Subscription {
		t.Helper()
		out := map[string]*pubsubpb.Subscription{}
		it := c.SubscriptionAdminClient.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: project})
		for s, err := it.Next(); err == nil; s, err = it.Next() {
			out[s.GetName()] = s
		}
		return out
	}
	before := readAll()
	advancePubSubClock(t, h, 30*time.Hour)

	file := filepath.Join(t.TempDir(), "state.tar.gz")
	if saved := run("state", "save", file); !strings.Contains(saved, "captured:     pubsub") {
		t.Fatalf("Pub/Sub was not captured:\n%s", saved)
	}
	out, _ := exec.Command(cli, append([]string{"status", "--format", "json"}, flags...)...).Output()
	var st struct {
		ControlURL string `json:"control_url"`
	}
	_ = json.Unmarshal(out, &st)
	control := strings.TrimPrefix(st.ControlURL, "http://")
	if code, body := adminReset(t, control, "service=pubsub&project="+h.Project()); code != 200 {
		t.Fatalf("reset: %d %s", code, body)
	}
	if len(readAll()) != 0 {
		t.Fatal("subscriptions survived the reset; the test would prove nothing")
	}
	if loaded := run("state", "load", file); !strings.Contains(loaded, "pubsub") {
		t.Fatalf("state load output:\n%s", loaded)
	}

	after := readAll()
	for _, s := range subs {
		if !proto.Equal(before[s.Name], after[s.Name]) {
			t.Errorf("%s came back as\n%v\nwant\n%v", s.Name, after[s.Name], before[s.Name])
		}
	}
	for _, tp := range topics {
		got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tp.Name})
		if err != nil || got.GetSchemaSettings().GetSchema() != tp.GetSchemaSettings().GetSchema() ||
			got.GetSchemaSettings().GetEncoding() != tp.GetSchemaSettings().GetEncoding() ||
			got.GetLabels()["team"] != tp.GetLabels()["team"] ||
			got.GetMessageRetentionDuration().AsDuration() != tp.GetMessageRetentionDuration().AsDuration() {
			t.Errorf("%s came back as %v, %v", tp.Name, got, err)
		}
	}
	var defs []string
	it := sch.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: schemaName, View: pubsubpb.SchemaView_FULL})
	for r, err := it.Next(); err == nil; r, err = it.Next() {
		defs = append(defs, r.GetDefinition())
	}
	if len(defs) != 2 || !strings.Contains(strings.Join(defs, " "), `"id"`) || !strings.Contains(strings.Join(defs, " "), `"sku"`) {
		t.Errorf("the schema came back with revisions %v; want both", defs)
	}
	if latest, err := sch.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: schemaName, View: pubsubpb.SchemaView_FULL}); err != nil ||
		latest.GetDefinition() != def("sku") {
		t.Errorf("the schema's latest revision is %v, %v; want the second", latest, err)
	}
	if n, err := c.SubscriptionAdminClient.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: snapshot}); err != nil || n.GetTopic() != plain {
		t.Errorf("the snapshot came back as %v, %v", n, err)
	}

	// The clock: saved at 30h idle of a 48h ttl.
	advancePubSubClock(t, h, 17*time.Hour)
	if _, ok := readAll()[subs[0].Name]; !ok {
		t.Fatalf("%s expired 47h into a 48h ttl", subs[0].Name)
	}
	advancePubSubClock(t, h, 2*time.Hour)
	left := readAll()
	if _, ok := left[subs[0].Name]; ok {
		t.Errorf("%s is still there 49h into a 48h ttl", subs[0].Name)
	}
	if _, ok := left[subs[2].Name]; !ok {
		t.Errorf("%s, which never expires, is gone", subs[2].Name)
	}

	// The message published before the save is not kept.
	resp, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: subs[2].Name, MaxMessages: 1, ReturnImmediately: true})
	if err != nil || len(resp.GetReceivedMessages()) != 0 {
		t.Errorf("Pull after the load = %v, %v; want no message (messages are not kept)", resp.GetReceivedMessages(), err)
	}
}
