//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Protocol Buffer schemas and DetachSubscription (#1406), which the
// emulator refuses UNIMPLEMENTED and CloudBurrow's Pub/Sub front serves
// (internal/pubsubfront: protobufschemas.go, detach.go).

const (
	protoSchemaV1 = "syntax = \"proto3\";\nmessage Burrow {\n  string name = 1;\n}\n"
	protoSchemaV2 = "syntax = \"proto3\";\nmessage Burrow {\n  string name = 1;\n  int32 depth = 2;\n}\n"
	proto2Schema  = "syntax = \"proto2\";\nmessage Burrow {\n  required string name = 1;\n}\n"
)

// TestPubSubProtobufSchemaService drives every SchemaService RPC with a
// PROTOCOL_BUFFER schema through the official generated client: validate
// (a definition with two top-level types or an import is INVALID_ARGUMENT),
// create, get, list, validate a message in JSON and BINARY, a proto2
// definition's required field, commit, list revisions, roll back, delete a
// revision (never the last) and delete.
// covers: google.pubsub.v1.SchemaService/ValidateSchema, google.pubsub.v1.SchemaService/CreateSchema, google.pubsub.v1.SchemaService/GetSchema, google.pubsub.v1.SchemaService/ListSchemas, google.pubsub.v1.SchemaService/ValidateMessage
// covers: google.pubsub.v1.SchemaService/CommitSchema, google.pubsub.v1.SchemaService/ListSchemaRevisions, google.pubsub.v1.SchemaService/RollbackSchema, google.pubsub.v1.SchemaService/DeleteSchemaRevision, google.pubsub.v1.SchemaService/DeleteSchema
func TestPubSubProtobufSchemaService(t *testing.T) {
	h := New(t)
	sc := schemaClient(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project()
	name := parent + "/schemas/burrow-proto"

	if _, err := sc.ValidateSchema(ctx, &pubsubpb.ValidateSchemaRequest{Parent: parent,
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: protoSchemaV1}}); err != nil {
		t.Fatalf("ValidateSchema(PROTOCOL_BUFFER): %v", err)
	}
	for _, bad := range []string{
		"syntax = \"proto3\";\nmessage A { string a = 1; }\nmessage B { string b = 1; }\n",
		"syntax = \"proto3\";\nimport \"other.proto\";\nmessage A { string a = 1; }\n",
		"message {",
	} {
		if _, err := sc.ValidateSchema(ctx, &pubsubpb.ValidateSchemaRequest{Parent: parent,
			Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: bad}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("ValidateSchema(%q) = %v, want INVALID_ARGUMENT", bad, err)
		}
	}

	v1, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: "burrow-proto",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: protoSchemaV1}})
	if err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: name}) })
	if v1.GetName() != name || v1.GetRevisionId() == "" || v1.GetType() != pubsubpb.Schema_PROTOCOL_BUFFER {
		t.Fatalf("CreateSchema = %v", v1)
	}
	got, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	if err != nil || got.GetDefinition() != protoSchemaV1 {
		t.Errorf("GetSchema = %v, %v", got, err)
	}
	var listed []string
	for _, s := range drain(t, "ListSchemas", sc.ListSchemas(ctx, &pubsubpb.ListSchemasRequest{Parent: parent}).Next) {
		listed = append(listed, s.GetName())
	}
	if !contains(listed, name) {
		t.Errorf("ListSchemas = %v, want it to include %s", listed, name)
	}

	validate := func(spec *pubsubpb.ValidateMessageRequest, msg string, enc pubsubpb.Encoding) error {
		spec.Parent, spec.Message, spec.Encoding = parent, []byte(msg), enc
		_, err := sc.ValidateMessage(ctx, spec)
		return err
	}
	byName := func() *pubsubpb.ValidateMessageRequest {
		return &pubsubpb.ValidateMessageRequest{SchemaSpec: &pubsubpb.ValidateMessageRequest_Name{Name: name}}
	}
	if err := validate(byName(), `{"name":"x"}`, pubsubpb.Encoding_JSON); err != nil {
		t.Errorf("ValidateMessage(conforming JSON): %v", err)
	}
	if err := validate(byName(), `{"nope":1}`, pubsubpb.Encoding_JSON); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ValidateMessage(non-conforming JSON) = %v, want INVALID_ARGUMENT", err)
	}
	if err := validate(byName(), "\x0a\x01x", pubsubpb.Encoding_BINARY); err != nil {
		t.Errorf("ValidateMessage(conforming BINARY): %v", err)
	}
	if err := validate(byName(), "\xff\xff\xff", pubsubpb.Encoding_BINARY); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ValidateMessage(non-conforming BINARY) = %v, want INVALID_ARGUMENT", err)
	}
	inline := &pubsubpb.ValidateMessageRequest{SchemaSpec: &pubsubpb.ValidateMessageRequest_Schema{
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: proto2Schema}}}
	if err := validate(inline, `{}`, pubsubpb.Encoding_JSON); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ValidateMessage(proto2, a required field missing) = %v, want INVALID_ARGUMENT", err)
	}

	v2, err := sc.CommitSchema(ctx, &pubsubpb.CommitSchemaRequest{Name: name,
		Schema: &pubsubpb.Schema{Name: name, Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: protoSchemaV2}})
	if err != nil || v2.GetRevisionId() == v1.GetRevisionId() {
		t.Fatalf("CommitSchema = %v, %v", v2, err)
	}
	var revs []string
	for _, s := range drain(t, "ListSchemaRevisions", sc.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: name}).Next) {
		revs = append(revs, s.GetRevisionId())
	}
	if !contains(revs, v1.GetRevisionId()) || !contains(revs, v2.GetRevisionId()) {
		t.Errorf("ListSchemaRevisions = %v, want %s and %s", revs, v1.GetRevisionId(), v2.GetRevisionId())
	}
	rolled, err := sc.RollbackSchema(ctx, &pubsubpb.RollbackSchemaRequest{Name: name, RevisionId: v1.GetRevisionId()})
	if err != nil || rolled.GetDefinition() != protoSchemaV1 {
		t.Errorf("RollbackSchema = %v, %v", rolled, err)
	}
	if _, err := sc.DeleteSchemaRevision(ctx, &pubsubpb.DeleteSchemaRevisionRequest{Name: name + "@" + v2.GetRevisionId()}); err != nil {
		t.Errorf("DeleteSchemaRevision: %v", err)
	}
	if err := sc.DeleteSchema(ctx, &pubsubpb.DeleteSchemaRequest{Name: name}); err != nil {
		t.Fatalf("DeleteSchema: %v", err)
	}
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSchema after DeleteSchema = %v, want NOT_FOUND", err)
	}
}

// TestPubSubProtobufSchemaTopic binds topics to a PROTOCOL_BUFFER schema,
// one JSON and one BINARY, and publishes through the official client's
// Publisher: a conforming message is delivered, a non-conforming one refused
// INVALID_ARGUMENT, as Google documents; the topic reads back its binding.
// An UpdateTopic of schema_settings rebinds it, clears it and binds it again
// (#1446), each binding the one Publish checks.
// covers: google.pubsub.v1.Publisher/CreateTopic, google.pubsub.v1.Publisher/GetTopic, google.pubsub.v1.Publisher/Publish, google.pubsub.v1.Publisher/UpdateTopic
func TestPubSubProtobufSchemaTopic(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	sc := schemaClient(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project()
	name := parent + "/schemas/burrow-proto-topic"
	if _, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: "burrow-proto-topic",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: protoSchemaV1}}); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: name}) })

	for _, enc := range []pubsubpb.Encoding{pubsubpb.Encoding_JSON, pubsubpb.Encoding_BINARY} {
		id := "proto-topic-" + strings.ToLower(enc.String())
		tn := parent + "/topics/" + id
		if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: tn,
			SchemaSettings: &pubsubpb.SchemaSettings{Schema: name, Encoding: enc}}); err != nil {
			t.Fatalf("CreateTopic(%s): %v", enc, err)
		}
		t.Cleanup(func() {
			_ = c.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: tn})
		})
		sub := subscription(t, h, c, id+"-sub", tn)
		got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn})
		if err != nil || got.GetSchemaSettings().GetSchema() != name || got.GetSchemaSettings().GetEncoding() != enc {
			t.Errorf("GetTopic = %v, %v; want bound to %s in %s", got, err, name, enc)
		}
		good, bad := []byte(`{"name":"burrow"}`), []byte(`{"nope":1}`)
		if enc == pubsubpb.Encoding_BINARY {
			good, bad = []byte("\x0a\x06burrow"), []byte("\xff\xff\xff")
		}
		pub := c.Publisher(tn)
		if _, err := pub.Publish(ctx, &pubsub.Message{Data: good}).Get(ctx); err != nil {
			t.Errorf("Publish(conforming %s): %v", enc, err)
		}
		_, err = pub.Publish(ctx, &pubsub.Message{Data: bad}).Get(ctx)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "Message failed schema validation") {
			t.Errorf("Publish(non-conforming %s) = %v, want INVALID_ARGUMENT naming schema validation", enc, err)
		}
		pub.Stop()
		var pulled []string
		for i := 0; i < 20 && len(pulled) == 0; i++ {
			resp, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
			if err != nil {
				t.Fatalf("Pull: %v", err)
			}
			for _, m := range resp.GetReceivedMessages() {
				pulled = append(pulled, string(m.GetMessage().GetData()))
			}
		}
		if len(pulled) != 1 || pulled[0] != string(good) {
			t.Errorf("pulled %q, want only the conforming message", pulled)
		}
	}

	// UpdateTopic of schema_settings (#1446): the JSON topic is bound
	// BINARY from the latest revision on, then cleared, then bound again,
	// and each binding is the one Publish checks.
	tn := parent + "/topics/proto-topic-json"
	latest, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	publish := func(data []byte) error {
		_, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: tn, Messages: []*pubsubpb.PubsubMessage{{Data: data}}})
		return err
	}
	upd, err := c.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic: &pubsubpb.Topic{Name: tn, SchemaSettings: &pubsubpb.SchemaSettings{Schema: name,
			Encoding: pubsubpb.Encoding_BINARY, FirstRevisionId: latest.GetRevisionId()}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"schema_settings"}}})
	if err != nil || upd.GetSchemaSettings().GetEncoding() != pubsubpb.Encoding_BINARY ||
		upd.GetSchemaSettings().GetFirstRevisionId() != latest.GetRevisionId() {
		t.Fatalf("UpdateTopic(schema_settings, BINARY) = %v, %v", upd, err)
	}
	if got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn}); err != nil ||
		got.GetSchemaSettings().GetEncoding() != pubsubpb.Encoding_BINARY {
		t.Errorf("GetTopic after the update = %v, %v", got, err)
	}
	if err := publish([]byte(`{"name":"burrow"}`)); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Publish(JSON to the topic now BINARY) = %v, want INVALID_ARGUMENT", err)
	}
	if err := publish([]byte("\x0a\x06burrow")); err != nil {
		t.Errorf("Publish(BINARY) = %v", err)
	}
	if _, err := c.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic: &pubsubpb.Topic{Name: tn, SchemaSettings: &pubsubpb.SchemaSettings{Schema: name,
			Encoding: pubsubpb.Encoding_JSON, LastRevisionId: "deadbeef"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"schema_settings"}}}); status.Code(err) != codes.NotFound {
		t.Errorf("UpdateTopic(schema_settings to an absent revision) = %v, want NOT_FOUND", err)
	}
	cleared, err := c.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{Topic: &pubsubpb.Topic{Name: tn},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"schema_settings"}}})
	if err != nil || cleared.GetSchemaSettings() != nil {
		t.Fatalf("UpdateTopic(clear schema_settings) = %v, %v", cleared, err)
	}
	if err := publish([]byte("anything at all")); err != nil {
		t.Errorf("Publish to the topic with no schema = %v", err)
	}
	if _, err := c.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic:      &pubsubpb.Topic{Name: tn, SchemaSettings: &pubsubpb.SchemaSettings{Schema: name, Encoding: pubsubpb.Encoding_JSON}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"schema_settings"}}}); err != nil {
		t.Fatalf("UpdateTopic(bind again): %v", err)
	}
	if err := publish([]byte(`{"nope":1}`)); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Publish(non-conforming) after binding again = %v, want INVALID_ARGUMENT", err)
	}
}

// TestGcloudPubSubDetachAndProtobufSchemas (#1406): the real gcloud,
// through `cloudburrow gcloud-setup`'s configuration with egress blocked,
// detaches a subscription (`topics detach-subscription`), and creates, validates against and deletes a
// protocol-buffer schema; the official client reads back what it did. Since
// #1446 it also updates, seeks and acknowledges on the detached
// subscription, is refused a snapshot of it, and binds a topic to the
// schema with `topics update --schema`, then clears it.
// covers: google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Subscriber/Seek, google.pubsub.v1.Subscriber/Acknowledge, google.pubsub.v1.Subscriber/CreateSnapshot, google.pubsub.v1.Publisher/UpdateTopic
// covers: google.pubsub.v1.Publisher/DetachSubscription, google.pubsub.v1.SchemaService/CreateSchema, google.pubsub.v1.SchemaService/ValidateSchema, google.pubsub.v1.SchemaService/ValidateMessage, google.pubsub.v1.SchemaService/GetSchema, google.pubsub.v1.SchemaService/DeleteSchema
func TestGcloudPubSubDetachAndProtobufSchemas(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	sc := schemaClient(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	p := "--project=" + h.Project()
	tn := topic(t, h, c, "gcloud-detach-topic")
	sn := subscription(t, h, c, "gcloud-detach-sub", tn)

	g.must("pubsub", "topics", "detach-subscription", "gcloud-detach-sub", p)
	got, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sn})
	if err != nil || !got.GetDetached() {
		t.Errorf("after gcloud topics detach-subscription, GetSubscription = %v, %v; want detached", got, err)
	}
	if out := g.must("pubsub", "subscriptions", "describe", "gcloud-detach-sub", p, "--format=value(detached,topic)"); !strings.Contains(out, "True") || !strings.Contains(out, "_deleted-topic_") {
		t.Errorf("gcloud subscriptions describe = %q; want detached on _deleted-topic_", out)
	}
	out, err := g.run(nil, "pubsub", "subscriptions", "pull", "gcloud-detach-sub", p)
	if err == nil || !strings.Contains(out, "FAILED_PRECONDITION") && !strings.Contains(out, "detached") {
		t.Errorf("gcloud subscriptions pull of a detached subscription = %v:\n%s\nwant a FAILED_PRECONDITION refusal", err, out)
	}
	// The other calls on it (#1446): an update and a seek to a time are
	// applied, a snapshot of it refused FAILED_PRECONDITION.
	g.must("pubsub", "subscriptions", "update", "gcloud-detach-sub", p, "--ack-deadline=42", "--update-labels=env=test")
	if got, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sn}); err != nil ||
		got.GetAckDeadlineSeconds() != 42 || got.GetLabels()["env"] != "test" || !got.GetDetached() {
		t.Errorf("after gcloud subscriptions update, GetSubscription = %v, %v", got, err)
	}
	g.must("pubsub", "subscriptions", "seek", "gcloud-detach-sub", p, "--time=2026-01-01T00:00:00Z")
	g.must("pubsub", "subscriptions", "ack", "gcloud-detach-sub", p, "--ack-ids=some-ack-id")
	if out, err := g.run(nil, "pubsub", "snapshots", "create", "gcloud-detach-snap", p, "--subscription=gcloud-detach-sub"); err == nil ||
		!strings.Contains(out, "FAILED_PRECONDITION") && !strings.Contains(out, "detached") {
		t.Errorf("gcloud snapshots create of a detached subscription = %v:\n%s\nwant a FAILED_PRECONDITION refusal", err, out)
	}

	name := "projects/" + h.Project() + "/schemas/gcloud-proto"
	t.Cleanup(func() { _ = sc.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: name}) })
	g.must("pubsub", "schemas", "validate-schema", p, "--type=protocol-buffer", "--definition="+protoSchemaV1)
	g.must("pubsub", "schemas", "create", "gcloud-proto", p, "--type=protocol-buffer", "--definition="+protoSchemaV1)
	s, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	if err != nil || s.GetType() != pubsubpb.Schema_PROTOCOL_BUFFER || s.GetDefinition() != protoSchemaV1 {
		t.Errorf("the schema gcloud created reads %v, %v", s, err)
	}
	if out := g.must("pubsub", "schemas", "describe", "gcloud-proto", p, "--format=value(type)"); !strings.Contains(out, "PROTOCOL_BUFFER") {
		t.Errorf("gcloud schemas describe = %q", out)
	}
	g.must("pubsub", "schemas", "validate-message", p, "--schema-name=gcloud-proto", "--message-encoding=json", `--message={"name":"x"}`)
	if out, err := g.run(nil, "pubsub", "schemas", "validate-message", p, "--schema-name=gcloud-proto", "--message-encoding=json", `--message={"nope":1}`); err == nil {
		t.Errorf("gcloud schemas validate-message of a non-conforming message succeeded:\n%s", out)
	}
	// A topic bound to it by `topics update`, then cleared (#1446).
	bt := topic(t, h, c, "gcloud-proto-topic")
	g.must("pubsub", "topics", "update", "gcloud-proto-topic", p, "--schema=gcloud-proto", "--message-encoding=json")
	if got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: bt}); err != nil ||
		got.GetSchemaSettings().GetSchema() != name || got.GetSchemaSettings().GetEncoding() != pubsubpb.Encoding_JSON {
		t.Errorf("after gcloud topics update --schema, GetTopic = %v, %v", got, err)
	}
	if out, err := g.run(nil, "pubsub", "topics", "publish", "gcloud-proto-topic", p, `--message={"nope":1}`); err == nil {
		t.Errorf("gcloud topics publish of a non-conforming message succeeded:\n%s", out)
	}
	g.must("pubsub", "topics", "update", "gcloud-proto-topic", p, "--clear-schema-settings")
	if got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: bt}); err != nil || got.GetSchemaSettings() != nil {
		t.Errorf("after gcloud topics update --clear-schema-settings, GetTopic = %v, %v", got, err)
	}
	g.must("pubsub", "topics", "publish", "gcloud-proto-topic", p, `--message={"nope":1}`)
	g.must("pubsub", "schemas", "delete", "gcloud-proto", p, "--quiet")
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSchema after gcloud schemas delete = %v, want NOT_FOUND", err)
	}
}

// TestTerraformPubSubProtobufSchema (#1406): google_pubsub_schema of type
// PROTOCOL_BUFFER, and a google_pubsub_topic bound to it, through
// `cloudburrow terraform` with egress blocked: applied, a plan with no diff,
// read back with the official client, the binding's encoding changed in
// place (#1446), and destroyed.
// covers: google.pubsub.v1.Publisher/UpdateTopic, google.pubsub.v1.SchemaService/CreateSchema, google.pubsub.v1.SchemaService/GetSchema, google.pubsub.v1.SchemaService/DeleteSchema, google.pubsub.v1.Publisher/CreateTopic, google.pubsub.v1.Publisher/GetTopic, google.pubsub.v1.Publisher/DeleteTopic
func TestTerraformPubSubProtobufSchema(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	sc := schemaClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	project := h.Project()
	name := "projects/" + project + "/schemas/tf-proto"
	tn := "projects/" + project + "/topics/tf-proto-topic"

	config := func(encoding string) string {
		return fmt.Sprintf(`
resource "google_pubsub_schema" "s" {
  project    = %[1]q
  name       = "tf-proto"
  type       = "PROTOCOL_BUFFER"
  definition = "syntax = \"proto3\";\nmessage Burrow {\n  string name = 1;\n}\n"
}
resource "google_pubsub_topic" "t" {
  project = %[1]q
  name    = "tf-proto-topic"
  schema_settings {
    schema   = google_pubsub_schema.s.id
    encoding = %[2]q
  }
}
`, project, encoding)
	}
	m.write(config("JSON"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	s, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	if err != nil || s.GetType() != pubsubpb.Schema_PROTOCOL_BUFFER || s.GetDefinition() != protoSchemaV1 {
		t.Errorf("after apply, GetSchema = %v, %v", s, err)
	}
	got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn})
	if err != nil || got.GetSchemaSettings().GetSchema() != name || got.GetSchemaSettings().GetEncoding() != pubsubpb.Encoding_JSON {
		t.Errorf("after apply, GetTopic = %v, %v", got, err)
	}
	m.planClean()
	// The binding's encoding changed in place, an UpdateTopic of
	// schema_settings (#1446).
	m.write(config("BINARY"))
	m.apply()
	got, err = c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn})
	if err != nil || got.GetSchemaSettings().GetSchema() != name || got.GetSchemaSettings().GetEncoding() != pubsubpb.Encoding_BINARY {
		t.Errorf("after the encoding change, GetTopic = %v, %v", got, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSchema after destroy = %v, want NOT_FOUND", err)
	}
	if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTopic after destroy = %v, want NOT_FOUND", err)
	}
}

// TestConsolePubSubDetachAndProtobufSchema (#1406): Detach from topic on a
// subscription's page detaches it, read back with the official client; the
// Schemas screen creates a PROTOCOL_BUFFER schema, read back the same way.
// Since #1446 the detached subscription's page offers Edit subscription,
// which changes its kept configuration, and Edit topic binds a topic to the
// PROTOCOL_BUFFER schema and clears it, each read back by the client.
// covers: google.pubsub.v1.Publisher/DetachSubscription, google.pubsub.v1.SchemaService/CreateSchema, google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Publisher/UpdateTopic
func TestConsolePubSubDetachAndProtobufSchema(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := pubsubClient(t, h)
	sc := schemaClient(t, h)
	ctx := h.Context()
	project := h.Project()
	tn := topic(t, h, c, "console-detach-topic")
	sn := subscription(t, h, c, "console-detach-sub", tn)

	body, _ := json.Marshal(map[string]any{"Path": []string{sn}, "Action": "detach-subscription", "Values": map[string]string{}})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/pubsub-subscriptions?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("console Detach from topic = %d: %s", code, out)
	}
	got, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sn})
	if err != nil || !got.GetDetached() {
		t.Errorf("after the console's Detach, GetSubscription = %v, %v; want detached", got, err)
	}

	name := "projects/" + project + "/schemas/console-proto"
	t.Cleanup(func() { _ = sc.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: name}) })
	body, _ = json.Marshal(map[string]string{"name": "console-proto", "type": "PROTOCOL_BUFFER", "definition": protoSchemaV1})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/pubsub-schemas?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("console create of a PROTOCOL_BUFFER schema = %d: %s", code, out)
	}
	s, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	if err != nil || s.GetType() != pubsubpb.Schema_PROTOCOL_BUFFER {
		t.Errorf("the console's schema reads %v, %v", s, err)
	}

	edit := func(service, path string, v map[string]string) (int, string) {
		body, _ := json.Marshal(map[string]any{"Path": []string{path}, "Values": v})
		return consoleDo(t, addr, http.MethodPatch, "/api/resources/"+service+"?project="+project, string(body))
	}
	// Edit subscription on the detached subscription's page (#1446).
	_, _, values := consolePubSubEditForm(t, addr, "pubsub-subscriptions", project, sn, "Edit subscription")
	values["ackDeadline"] = "42"
	values["labels"] = `{"env":"dev"}`
	if code, out := edit("pubsub-subscriptions", sn, values); code != http.StatusOK {
		t.Fatalf("console Edit subscription of the detached subscription = %d: %s", code, out)
	}
	if got, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sn}); err != nil ||
		got.GetAckDeadlineSeconds() != 42 || got.GetLabels()["env"] != "dev" || !got.GetDetached() {
		t.Errorf("after the console's edit, GetSubscription = %v, %v; want ack deadline 42, env=dev, detached", got, err)
	}
	// Edit topic binds a topic to the PROTOCOL_BUFFER schema, BINARY, then
	// clears it (#1446).
	_, _, values = consolePubSubEditForm(t, addr, "pubsub", project, tn, "Edit topic")
	values["schema"] = "console-proto"
	values["schemaEncoding"] = "BINARY"
	if code, out := edit("pubsub", tn, values); code != http.StatusOK {
		t.Fatalf("console Edit topic to the schema = %d: %s", code, out)
	}
	if got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn}); err != nil ||
		got.GetSchemaSettings().GetSchema() != name || got.GetSchemaSettings().GetEncoding() != pubsubpb.Encoding_BINARY {
		t.Errorf("after the console's Edit topic, GetTopic = %v, %v; want bound BINARY to %s", got, err, name)
	}
	_, _, values = consolePubSubEditForm(t, addr, "pubsub", project, tn, "Edit topic")
	if values["schema"] != name || values["schemaEncoding"] != "BINARY" {
		t.Errorf("Edit topic is prefilled %v; want the binding", values)
	}
	values["schema"] = ""
	if code, out := edit("pubsub", tn, values); code != http.StatusOK {
		t.Fatalf("console Edit topic clearing the schema = %d: %s", code, out)
	}
	if got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn}); err != nil || got.GetSchemaSettings() != nil {
		t.Errorf("after the console cleared the schema, GetTopic = %v, %v", got, err)
	}
}
