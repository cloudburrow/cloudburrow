package pubsubfront

import (
	"context"
	"encoding/base64"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const (
	protoV1 = `syntax = "proto3"; message Burrow { string name = 1; }`
	protoV2 = `syntax = "proto3"; message Burrow { string name = 1; int32 depth = 2; }`
	proto2  = `syntax = "proto2"; message Burrow { required string name = 1; }`
)

// The Schema service of a PROTOCOL_BUFFER schema, which the emulator
// refuses, is the front's: validate, create, get, list, validate a message,
// commit, list revisions, roll back, delete a revision and delete.
func TestProtobufSchemaService(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	sc := pubsubpb.NewSchemaServiceClient(fx.conn)
	parent := "projects/" + project
	name := parent + "/schemas/burrow"

	if _, err := sc.ValidateSchema(ctx, &pubsubpb.ValidateSchemaRequest{Parent: parent,
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: protoV1}}); err != nil {
		t.Fatalf("ValidateSchema: %v", err)
	}
	for _, bad := range []string{`message A { string a = 1; } message B { string b = 1; }`, `syntax = "proto3"; import "x.proto"; message A {}`, `message {`} {
		if _, err := sc.ValidateSchema(ctx, &pubsubpb.ValidateSchemaRequest{Parent: parent,
			Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: bad}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("ValidateSchema(%q) = %v, want INVALID_ARGUMENT", bad, err)
		}
	}
	v1, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: "burrow",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: protoV1}})
	if err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	if v1.GetName() != name || v1.GetRevisionId() == "" || v1.GetRevisionCreateTime() == nil {
		t.Errorf("CreateSchema = %v", v1)
	}
	if _, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: "burrow",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: protoV1}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second CreateSchema = %v, want ALREADY_EXISTS", err)
	}
	got, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name})
	if err != nil || got.GetDefinition() != protoV1 || got.GetType() != pubsubpb.Schema_PROTOCOL_BUFFER {
		t.Errorf("GetSchema = %v, %v", got, err)
	}
	if b, _ := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_BASIC}); b.GetDefinition() != "" {
		t.Errorf("GetSchema(BASIC) = %v, want no definition", b)
	}
	l, err := sc.ListSchemas(ctx, &pubsubpb.ListSchemasRequest{Parent: parent})
	if err != nil || len(l.GetSchemas()) != 1 || l.GetSchemas()[0].GetName() != name {
		t.Errorf("ListSchemas = %v, %v", l, err)
	}

	msg := func(data string, enc pubsubpb.Encoding) error {
		_, err := sc.ValidateMessage(ctx, &pubsubpb.ValidateMessageRequest{Parent: parent,
			SchemaSpec: &pubsubpb.ValidateMessageRequest_Name{Name: name}, Message: []byte(data), Encoding: enc})
		return err
	}
	if err := msg(`{"name":"x"}`, pubsubpb.Encoding_JSON); err != nil {
		t.Errorf("ValidateMessage(conforming JSON): %v", err)
	}
	if err := msg(`{"nope":1}`, pubsubpb.Encoding_JSON); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ValidateMessage(non-conforming JSON) = %v, want INVALID_ARGUMENT", err)
	}
	if err := msg("\x0a\x01x", pubsubpb.Encoding_BINARY); err != nil {
		t.Errorf("ValidateMessage(conforming BINARY): %v", err)
	}
	if err := msg("\xff\xff", pubsubpb.Encoding_BINARY); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ValidateMessage(garbage BINARY) = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := sc.ValidateMessage(ctx, &pubsubpb.ValidateMessageRequest{Parent: parent,
		SchemaSpec: &pubsubpb.ValidateMessageRequest_Schema{Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: proto2}},
		Message:    []byte(`{}`), Encoding: pubsubpb.Encoding_JSON}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ValidateMessage(proto2 missing a required field) = %v, want INVALID_ARGUMENT", err)
	}

	v2, err := sc.CommitSchema(ctx, &pubsubpb.CommitSchemaRequest{Name: name,
		Schema: &pubsubpb.Schema{Name: name, Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: protoV2}})
	if err != nil || v2.GetRevisionId() == v1.GetRevisionId() {
		t.Fatalf("CommitSchema = %v, %v", v2, err)
	}
	revs, err := sc.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: name})
	if err != nil || len(revs.GetSchemas()) != 2 || revs.GetSchemas()[0].GetRevisionId() != v2.GetRevisionId() {
		t.Errorf("ListSchemaRevisions = %v, %v", revs, err)
	}
	rolled, err := sc.RollbackSchema(ctx, &pubsubpb.RollbackSchemaRequest{Name: name, RevisionId: v1.GetRevisionId()})
	if err != nil || rolled.GetDefinition() != protoV1 {
		t.Errorf("RollbackSchema = %v, %v", rolled, err)
	}
	if _, err := sc.DeleteSchemaRevision(ctx, &pubsubpb.DeleteSchemaRevisionRequest{Name: name + "@" + v2.GetRevisionId()}); err != nil {
		t.Errorf("DeleteSchemaRevision: %v", err)
	}
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name + "@" + v2.GetRevisionId()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSchema of the deleted revision = %v, want NOT_FOUND", err)
	}
	if _, err := sc.DeleteSchema(ctx, &pubsubpb.DeleteSchemaRequest{Name: name}); err != nil {
		t.Fatalf("DeleteSchema: %v", err)
	}
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSchema after delete = %v, want NOT_FOUND", err)
	}
}

// A topic bound to a front-kept schema is created without the binding in
// the emulator, reads back with it, and refuses a message that does not
// conform, in its encoding; an update of the binding is UNIMPLEMENTED.
func TestProtobufSchemaTopic(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	sc := pubsubpb.NewSchemaServiceClient(fx.conn)
	parent := "projects/" + project
	name := parent + "/schemas/burrow"
	if _, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: "burrow",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: protoV1}}); err != nil {
		t.Fatal(err)
	}
	for _, enc := range []pubsubpb.Encoding{pubsubpb.Encoding_JSON, pubsubpb.Encoding_BINARY} {
		topic := parent + "/topics/t-" + strings.ToLower(enc.String())
		created, err := fx.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic,
			SchemaSettings: &pubsubpb.SchemaSettings{Schema: name, Encoding: enc}})
		if err != nil {
			t.Fatalf("CreateTopic(%s): %v", enc, err)
		}
		if created.GetSchemaSettings().GetSchema() != name {
			t.Errorf("CreateTopic answered %v", created)
		}
		got, err := fx.client.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
		if err != nil || got.GetSchemaSettings().GetSchema() != name || got.GetSchemaSettings().GetEncoding() != enc {
			t.Errorf("GetTopic = %v, %v", got, err)
		}
		good, bad := []byte(`{"name":"x"}`), []byte(`{"nope":1}`)
		if enc == pubsubpb.Encoding_BINARY {
			good, bad = []byte("\x0a\x01x"), []byte("\xff\xff")
		}
		if _, err := fx.client.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
			Messages: []*pubsubpb.PubsubMessage{{Data: good}}}); err != nil {
			t.Errorf("Publish(conforming %s): %v", enc, err)
		}
		_, err = fx.client.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
			Messages: []*pubsubpb.PubsubMessage{{Data: bad}}})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "Message failed schema validation") {
			t.Errorf("Publish(non-conforming %s) = %v, want INVALID_ARGUMENT", enc, err)
		}
		if _, err := fx.client.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
			Topic:      &pubsubpb.Topic{Name: topic},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"schema_settings"}}}); status.Code(err) != codes.Unimplemented {
			t.Errorf("UpdateTopic(schema_settings) = %v, want UNIMPLEMENTED", err)
		}
	}
	// The state file keeps the schema and the bindings.
	path := filepath.Join(t.TempDir(), "state.json")
	if err := fx.front.KeepState(path); err != nil {
		t.Fatal(err)
	}
	g, err := New("127.0.0.1:1", func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.KeepState(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.keptSchema(name); !ok {
		t.Error("the schema was not restored")
	}
	if _, ok := g.topicSchema(parent + "/topics/t-json"); !ok {
		t.Error("the binding was not restored")
	}
	if _, err := sc.DeleteSchema(ctx, &pubsubpb.DeleteSchemaRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	got, err := fx.client.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: parent + "/topics/t-json"})
	if err != nil || got.GetSchemaSettings().GetSchema() != deletedSchemaName {
		t.Errorf("GetTopic after the schema is deleted = %v, %v", got, err)
	}
}

// Over REST: create, validate a message and publish to a bound topic, as
// gcloud does.
func TestRESTProtobufSchema(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	parent := "projects/" + project
	code, body := fx.rest(t, http.MethodPost, "/v1/"+parent+"/schemas:validate",
		`{"schema":{"type":"PROTOCOL_BUFFER","definition":"syntax = \"proto3\"; message M { string a = 1; }"}}`)
	if code != http.StatusOK {
		t.Fatalf(":validate = %d %v", code, body)
	}
	code, body = fx.rest(t, http.MethodPost, "/v1/"+parent+"/schemas?schemaId=mmm",
		`{"type":"PROTOCOL_BUFFER","definition":"syntax = \"proto3\"; message M { string a = 1; }"}`)
	if code != http.StatusOK || body["name"] != parent+"/schemas/mmm" || body["revisionId"] == "" {
		t.Fatalf("create = %d %v", code, body)
	}
	if code, body := fx.rest(t, http.MethodGet, "/v1/"+parent+"/schemas/mmm", ""); code != http.StatusOK || body["type"] != "PROTOCOL_BUFFER" {
		t.Errorf("get = %d %v", code, body)
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	if code, body := fx.rest(t, http.MethodPost, "/v1/"+parent+"/schemas:validateMessage",
		`{"name":"`+parent+`/schemas/mmm","message":"`+b64(`{"a":"x"}`)+`","encoding":"JSON"}`); code != http.StatusOK {
		t.Errorf("validateMessage(conforming) = %d %v", code, body)
	}
	if code, _ := fx.rest(t, http.MethodPost, "/v1/"+parent+"/schemas:validateMessage",
		`{"name":"`+parent+`/schemas/mmm","message":"`+b64(`{"b":1}`)+`","encoding":"JSON"}`); code != http.StatusBadRequest {
		t.Errorf("validateMessage(non-conforming) = %d, want 400", code)
	}
	up.answer(http.MethodPut, "/v1/"+parent+"/topics/t", `{"name":"`+parent+`/topics/t"}`)
	code, body = fx.rest(t, http.MethodPut, "/v1/"+parent+"/topics/t",
		`{"schemaSettings":{"schema":"`+parent+`/schemas/mmm","encoding":"JSON"}}`)
	if code != http.StatusOK || body["schemaSettings"] == nil {
		t.Errorf("create the topic = %d %v", code, body)
	}
	for _, c := range up.calls() {
		if c.method == http.MethodPut && strings.Contains(string(c.body), "schema") {
			t.Errorf("the emulator saw the binding: %s", c.body)
		}
	}
	if code, _ := fx.rest(t, http.MethodPost, "/v1/"+parent+"/topics/t:publish",
		`{"messages":[{"data":"`+b64(`{"b":1}`)+`"}]}`); code != http.StatusBadRequest {
		t.Errorf("publish(non-conforming) = %d, want 400", code)
	}
	if code, _ := fx.rest(t, http.MethodPost, "/v1/"+parent+"/topics/t:publish",
		`{"messages":[{"data":"`+b64(`{"a":"x"}`)+`"}]}`); code != http.StatusOK {
		t.Errorf("publish(conforming) = %d, want it forwarded", code)
	}
	if code, _ := fx.rest(t, http.MethodDelete, "/v1/"+parent+"/schemas/mmm", ""); code != http.StatusOK {
		t.Errorf("delete = %d", code)
	}
}
