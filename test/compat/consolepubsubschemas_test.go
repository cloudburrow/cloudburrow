//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// schemaPage is the part of a schema's console page this test reads.
type schemaPage struct {
	Sections []struct {
		ID, Text string
		Listing  struct {
			Items []struct {
				Name, Status string
				Actions      []struct{ ID string }
			}
		}
	}
	Actions []struct {
		ID     string
		Fields []struct{ Name, Default, Help string }
	}
}

func consoleSchemaPage(t *testing.T, addr, project, name string) schemaPage {
	t.Helper()
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/pubsub-schemas?project="+project+"&name="+url.QueryEscape(name), "")
	if code != http.StatusOK {
		t.Fatalf("console detail of %s = %d: %s", name, code, body)
	}
	var p schemaPage
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("decode the schema page: %v: %s", err, body)
	}
	return p
}

// revisionRowActions returns each revision row's action IDs, by revision.
func (p schemaPage) revisionRowActions() map[string]string {
	out := map[string]string{}
	for _, s := range p.Sections {
		if s.ID != "revisions" {
			continue
		}
		for _, it := range s.Listing.Items {
			var ids []string
			for _, a := range it.Actions {
				ids = append(ids, a.ID)
			}
			out[it.Name] = strings.Join(ids, ",")
		}
	}
	return out
}

// consoleRefusal is a console error body's message.
func consoleRefusal(body string) string {
	var r struct{ Error string }
	_ = json.Unmarshal([]byte(body), &r)
	return r.Error
}

// clientRefusal is how the console words a gRPC refusal: the code, then the
// message, exactly as the official client received them.
func clientRefusal(err error) string {
	st, _ := status.FromError(err)
	return st.Code().String() + ": " + st.Message()
}

// TestConsolePubSubSchemas (#788): a schema created on the console's Schemas
// screen is read back by the official SchemaClient; a revision committed from
// the console is the client's latest; a console rollback makes the chosen
// revision's definition the latest; a revision deleted from the console is no
// longer listed; a console delete leaves GetSchema NOT_FOUND. An invalid
// definition, on create and on commit, is refused with exactly ValidateSchema's
// message as the client receives it, and nothing is created or committed; an
// invalid test message is refused with exactly ValidateMessage's. The forms
// offer Avro only and Delete revision only while there are two revisions: this
// test asserts that the emulator still refuses Protocol Buffer schemas and the
// last revision's delete, with the messages the console quotes, so an
// emulator that starts accepting them fails here and they are then offered. A
// topic's page shows its schema settings, and after the schema is deleted from
// the console, the "_deleted-schema_" the emulator reports for it.
func TestConsolePubSubSchemas(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	sc := schemaClient(t, h)
	ps := pubsubClient(t, h)
	ctx := h.Context()
	project := h.Project()
	parent := "projects/" + project
	name := parent + "/schemas/console-schema"
	t.Cleanup(func() { _ = sc.DeleteSchema(context.Background(), &pubsubpb.DeleteSchemaRequest{Name: name}) })

	// What the form leaves out, still what the emulator does: Protocol Buffer
	// schemas are refused, with the message the form's help quotes.
	const protobuf = "syntax = \"proto3\";\nmessage Burrow {\n  string name = 1;\n}\n"
	_, err := sc.ValidateSchema(ctx, &pubsubpb.ValidateSchemaRequest{Parent: parent,
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: protobuf}})
	wantRefused(t, "ValidateSchema(PROTOCOL_BUFFER)", err, codes.Unimplemented, "Protocol buffer support not implemented in emulator")
	code, body := consoleDo(t, addr, http.MethodGet, "/api/services", "")
	if code != http.StatusOK || !strings.Contains(body, `"label":"Create schema"`) ||
		!strings.Contains(body, "Protocol buffer support not implemented in emulator") {
		t.Fatalf("the console's services = %d %s; want pubsub-schemas' Create schema saying Protocol Buffer is refused", code, body)
	}

	// An invalid definition: ValidateSchema's own refusal, and nothing made.
	const invalid = `{"type":"record"`
	_, sdkErr := sc.ValidateSchema(ctx, &pubsubpb.ValidateSchemaRequest{Parent: parent,
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: invalid}})
	if sdkErr == nil {
		t.Fatal("the emulator's ValidateSchema accepted a malformed definition")
	}
	create := func(definition string) (int, string) {
		b, _ := json.Marshal(map[string]string{"name": "console-schema", "definition": definition})
		return consoleDo(t, addr, http.MethodPost, "/api/resources/pubsub-schemas?project="+project, string(b))
	}
	if code, body := create(invalid); code != http.StatusBadRequest || consoleRefusal(body) != clientRefusal(sdkErr) {
		t.Errorf("console create of an invalid definition = %d %s; want 400 with the client's own %q", code, body, clientRefusal(sdkErr))
	}
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Fatalf("a definition ValidateSchema refused was created: GetSchema = %v", err)
	}

	// A valid one: the client reads it back and lists it.
	if code, body := create(avroSchemaV1); code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, body)
	}
	got, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	if err != nil || got.GetType() != pubsubpb.Schema_AVRO || got.GetDefinition() != avroSchemaV1 {
		t.Fatalf("GetSchema of the console's schema = %v (%v); want its Avro definition", got, err)
	}
	v1 := got.GetRevisionId()
	var listed []string
	for _, s := range drain(t, "ListSchemas", sc.ListSchemas(ctx, &pubsubpb.ListSchemasRequest{Parent: parent}).Next) {
		listed = append(listed, s.GetName())
	}
	if !contains(listed, name) {
		t.Errorf("the client's ListSchemas = %v; want the console's %s", listed, name)
	}
	if code, body := consoleDo(t, addr, http.MethodGet, "/api/resources/pubsub-schemas?project="+project, ""); code != http.StatusOK ||
		!strings.Contains(body, name) || !strings.Contains(body, v1) {
		t.Errorf("the console's Schemas screen = %d %s; want %s at %s", code, body, name, v1)
	}

	// The last revision cannot be deleted, so the page does not offer it.
	_, err = sc.DeleteSchemaRevision(ctx, &pubsubpb.DeleteSchemaRevisionRequest{Name: name + "@" + v1})
	wantRefused(t, "DeleteSchemaRevision(the only revision)", err, codes.InvalidArgument, "Cannot delete last revision. Please use DeleteSchema.")
	if acts := consoleSchemaPage(t, addr, project, name).revisionRowActions(); len(acts) != 1 || acts[v1] != "" {
		t.Errorf("the only revision offers %v; want no action", acts)
	}

	act := func(path []string, action string, values map[string]string) (int, string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
		return consoleDo(t, addr, http.MethodPost, "/api/actions/pubsub-schemas?project="+project, string(b))
	}

	// Commit: an invalid definition is refused as ValidateSchema refuses it,
	// and a valid one is the client's latest revision.
	if code, body := act([]string{name}, "commit-revision", map[string]string{"definition": invalid}); code != http.StatusBadRequest ||
		consoleRefusal(body) != clientRefusal(sdkErr) {
		t.Errorf("console commit of an invalid definition = %d %s; want 400 with the client's own %q", code, body, clientRefusal(sdkErr))
	}
	if revs := drain(t, "ListSchemaRevisions", sc.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: name}).Next); len(revs) != 1 {
		t.Fatalf("a definition ValidateSchema refused was committed: %d revisions", len(revs))
	}
	if code, body := act([]string{name}, "commit-revision", map[string]string{"definition": avroSchemaV2}); code != http.StatusOK {
		t.Fatalf("console Commit revision = %d: %s", code, body)
	}
	got, err = sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	if err != nil || got.GetDefinition() != avroSchemaV2 || got.GetRevisionId() == v1 {
		t.Fatalf("after the console's commit GetSchema = %v (%v); want v2 as a new latest revision", got, err)
	}
	v2 := got.GetRevisionId()
	if acts := consoleSchemaPage(t, addr, project, name).revisionRowActions(); acts[v1] != "rollback,delete-revision" || acts[v2] != "delete-revision" {
		t.Errorf("revision rows offer %v; want rollback and delete on %s, delete only on the latest %s", acts, v1, v2)
	}

	// Test message: against the latest (v2), a conforming message is valid,
	// and one ValidateMessage refuses is refused with its own message.
	const conforming = `{"name":"x","depth":1}`
	if code, body := act([]string{name}, "test-message", map[string]string{"encoding": "JSON", "message": conforming}); code != http.StatusOK ||
		!strings.Contains(body, `"name":"Valid"`) || !strings.Contains(body, v2) {
		t.Errorf("console Test message of a conforming message = %d %s; want Valid against %s", code, body, v2)
	}
	_, vmErr := sc.ValidateMessage(ctx, &pubsubpb.ValidateMessageRequest{Parent: parent,
		SchemaSpec: &pubsubpb.ValidateMessageRequest_Name{Name: name}, Message: []byte(`{"nope":1}`), Encoding: pubsubpb.Encoding_JSON})
	if vmErr == nil {
		t.Fatal("the emulator's ValidateMessage accepted a non-conforming message")
	}
	if code, body := act([]string{name}, "test-message", map[string]string{"encoding": "JSON", "message": `{"nope":1}`}); code != http.StatusBadRequest ||
		consoleRefusal(body) != clientRefusal(vmErr) {
		t.Errorf("console Test message of a non-conforming message = %d %s; want 400 with the client's own %q", code, body, clientRefusal(vmErr))
	}

	// Rollback to v1: its definition is the client's latest, as a new revision.
	if code, body := act([]string{name, v1}, "rollback", nil); code != http.StatusOK {
		t.Fatalf("console Rollback = %d: %s", code, body)
	}
	got, err = sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	if err != nil || got.GetDefinition() != avroSchemaV1 || got.GetRevisionId() == v1 || got.GetRevisionId() == v2 {
		t.Errorf("after the console's rollback GetSchema = %v (%v); want v1's definition as a new latest revision", got, err)
	}

	// Delete revision v2: the client no longer lists it.
	if code, body := act([]string{name, v2}, "delete-revision", nil); code != http.StatusOK {
		t.Fatalf("console Delete revision = %d: %s", code, body)
	}
	var revs []string
	for _, s := range drain(t, "ListSchemaRevisions", sc.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: name}).Next) {
		revs = append(revs, s.GetRevisionId())
	}
	if contains(revs, v2) || len(revs) != 2 {
		t.Errorf("after the console's delete the client lists revisions %v; want two, without %s", revs, v2)
	}

	// A topic that uses the schema: its page shows the settings.
	tp := parent + "/topics/console-schema-topic"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: tp,
		SchemaSettings: &pubsubpb.SchemaSettings{Schema: name, Encoding: pubsubpb.Encoding_JSON}}); err != nil {
		t.Fatalf("CreateTopic with schema settings: %v", err)
	}
	t.Cleanup(func() {
		_ = ps.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: tp})
	})
	topicPage := func() string {
		code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/pubsub?project="+project+"&name="+url.QueryEscape(tp), "")
		if code != http.StatusOK {
			t.Fatalf("console topic page = %d: %s", code, body)
		}
		return body
	}
	if body := topicPage(); !strings.Contains(body, `{"label":"Schema","value":"`+name+`"}`) ||
		!strings.Contains(body, `{"label":"Schema encoding","value":"JSON"}`) {
		t.Errorf("the topic page does not show its schema settings: %s", body)
	}

	// Delete schema, from its page: NOT_FOUND through the client, and the
	// topic's settings name the deleted schema.
	if code, body := act([]string{name}, "delete-schema", nil); code != http.StatusOK {
		t.Fatalf("console Delete schema = %d: %s", code, body)
	}
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("after the console's delete GetSchema = %v, want NOT_FOUND", err)
	}
	if got, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tp}); err != nil ||
		got.GetSchemaSettings().GetSchema() != "_deleted-schema_" {
		t.Errorf("after the schema's delete GetTopic = %v (%v); the delete confirmation says its settings name _deleted-schema_", got, err)
	}
	if body := topicPage(); !strings.Contains(body, "_deleted-schema_ (the schema was deleted)") {
		t.Errorf("the topic page does not say its schema was deleted: %s", body)
	}

	// And from the list row, through DeleteSchema.
	if code, body := create(avroSchemaV1); code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, body)
	}
	if code, body := consoleDo(t, addr, http.MethodDelete, "/api/resources/pubsub-schemas?project="+project+"&name="+url.QueryEscape(name), ""); code != http.StatusOK {
		t.Fatalf("console delete of %s = %d: %s", name, code, body)
	}
	if _, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("after the console's row delete GetSchema = %v, want NOT_FOUND", err)
	}
}
