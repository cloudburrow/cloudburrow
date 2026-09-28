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

	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	testAvroV1 = `{"type":"record","name":"Burrow","fields":[{"name":"name","type":"string"}]}`
	testAvroV2 = `{"type":"record","name":"Burrow","fields":[{"name":"name","type":"string"},{"name":"depth","type":"int","default":0}]}`
)

// schemasConsole serves a console with the Schemas screen against a pstest
// server, which serves the SchemaService, and returns it with the official
// SchemaClient of the same server.
func schemasConsole(t *testing.T, opts ...pstest.ServerReactorOption) (*httptest.Server, *vkit.SchemaClient) {
	t.Helper()
	ps := pstest.NewServer(opts...)
	t.Cleanup(func() { _ = ps.Close() })
	p := pubsubSchemasProvider{endpoint: ps.Addr}
	c, err := p.client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, p).Handler())
	t.Cleanup(srv.Close)
	return srv, c
}

func consolePost(t *testing.T, u string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(u, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func schemaAct(t *testing.T, srv *httptest.Server, project string, path []string, action string, values map[string]string) (int, string) {
	t.Helper()
	return consolePost(t, srv.URL+"/api/actions/pubsub-schemas?project="+project,
		map[string]any{"Path": path, "Action": action, "Values": values})
}

func schemaDetail(t *testing.T, srv *httptest.Server, project, name string) console.Detail {
	t.Helper()
	var d console.Detail
	getJSON200(t, srv.URL+"/api/detail/pubsub-schemas?project="+project+"&name="+url.QueryEscape(name), &d)
	return d
}

func sdkRevisions(t *testing.T, c *vkit.SchemaClient, name string) map[string]string {
	t.Helper()
	out := map[string]string{}
	it := c.ListSchemaRevisions(context.Background(), &pubsubpb.ListSchemaRevisionsRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	for {
		s, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[s.GetRevisionId()] = s.GetDefinition()
	}
}

func revisionRows(d console.Detail) map[string]console.Resource {
	out := map[string]console.Resource{}
	for _, s := range d.Sections {
		if s.ID == "revisions" {
			for _, it := range s.Listing.Items {
				out[it.Name] = it
			}
		}
	}
	return out
}

func actionIDs(as []console.Action) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

// The Schemas screen creates a schema the official SchemaClient reads back,
// lists and opens it (definition, revisions), commits a revision that the
// client reads as the latest, rolls back so the chosen revision's definition
// is the latest, deletes a revision, tests a message, and deletes the schema,
// each through the official client. Rollback is not offered on the latest
// revision, nor Delete revision on the only one; another project's schema is
// refused (#788).
func TestTheSchemasScreenCreatesCommitsRollsBackAndDeletes(t *testing.T) {
	ctx := context.Background()
	const project = "schema-proj"
	srv, c := schemasConsole(t)
	name := "projects/" + project + "/schemas/burrow"

	var services struct {
		Services []struct {
			ID             string
			Detail, Delete bool
			Create         *struct{ Label string }
		}
	}
	getJSON200(t, srv.URL+"/api/services", &services)
	advertised := false
	for _, s := range services.Services {
		if s.ID == "pubsub-schemas" {
			advertised = s.Detail && s.Delete && s.Create != nil && s.Create.Label == "Create schema"
		}
	}
	if !advertised {
		t.Fatalf("pubsub-schemas is not advertised with detail, delete and Create schema: %+v", services)
	}
	label, fields := pubsubSchemasProvider{}.CreateForm()
	if label != "Create schema" || len(fields) != 2 || !strings.Contains(fields[1].Help, pubsubProtobufRefusal) {
		t.Errorf("the create form = %q %+v; want an ID and a definition, saying Protocol Buffer is refused", label, fields)
	}

	if code, out := consolePost(t, srv.URL+"/api/resources/pubsub-schemas?project="+project,
		map[string]string{"name": "burrow", "definition": testAvroV1}); code != http.StatusOK || !strings.Contains(out, name) {
		t.Fatalf("console create = %d: %s", code, out)
	}
	got, err := c.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	if err != nil || got.GetDefinition() != testAvroV1 || got.GetType() != pubsubpb.Schema_AVRO {
		t.Fatalf("the official client's GetSchema = %v (%v); want the console's Avro schema", got, err)
	}
	v1 := got.GetRevisionId()

	var listing console.Listing
	getJSON200(t, srv.URL+"/api/resources/pubsub-schemas?project="+project, &listing)
	if len(listing.Items) != 1 || listing.Items[0].Name != name || listing.Items[0].Fields["Type"] != "AVRO" ||
		listing.Items[0].Fields["Latest revision"] != v1 {
		t.Fatalf("schemas listing = %+v; want %s, AVRO, at %s", listing.Items, name, v1)
	}

	d := schemaDetail(t, srv, project, name)
	if len(d.Sections) != 2 || d.Sections[0].ID != "definition" || d.Sections[0].Text != testAvroV1 {
		t.Fatalf("schema page sections = %+v; want the definition first", d.Sections)
	}
	rows := revisionRows(d)
	if len(rows) != 1 || rows[v1].Status != "Latest" || len(rows[v1].Actions) != 0 {
		t.Errorf("one revision's rows = %+v; want it latest, with no rollback and no delete", rows)
	}
	if ids := strings.Join(actionIDs(d.Actions), ","); ids != "commit-revision,test-message,delete-schema" {
		t.Fatalf("schema page actions = %s", ids)
	}
	if d.Actions[0].Fields[0].Default != testAvroV1 {
		t.Errorf("Commit revision is prefilled with %q, want the latest definition", d.Actions[0].Fields[0].Default)
	}
	if del := d.Actions[2]; !del.Destructive || !del.Leaves {
		t.Errorf("Delete schema = %+v; want it destructive, leaving the page", del)
	}
	if code, out := schemaAct(t, srv, project, []string{name, v1}, actDeleteSchemaRevision, nil); code != http.StatusBadRequest ||
		!strings.Contains(out, "is not available") {
		t.Errorf("deleting the only revision = %d %s; want it refused as not offered", code, out)
	}

	// Commit: the client reads the new revision as the latest.
	if code, out := schemaAct(t, srv, project, []string{name}, actCommitSchema, map[string]string{"definition": testAvroV2}); code != http.StatusOK {
		t.Fatalf("Commit revision = %d: %s", code, out)
	}
	got, _ = c.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	v2 := got.GetRevisionId()
	if got.GetDefinition() != testAvroV2 || v2 == v1 {
		t.Fatalf("after the commit GetSchema = %v; want v2 as a new latest revision", got)
	}
	rows = revisionRows(schemaDetail(t, srv, project, name))
	if ids := strings.Join(actionIDs(rows[v1].Actions), ","); ids != "rollback,delete-revision" {
		t.Errorf("the older revision offers %s, want rollback,delete-revision", ids)
	}
	if ids := strings.Join(actionIDs(rows[v2].Actions), ","); ids != "delete-revision" || rows[v2].Status != "Latest" {
		t.Errorf("the latest revision offers %s (%q), want only delete-revision", ids, rows[v2].Status)
	}
	if code, _ := schemaAct(t, srv, project, []string{name, v2}, actRollbackSchema, nil); code != http.StatusBadRequest {
		t.Errorf("rollback to the latest revision = %d; want it refused as not offered", code)
	}

	// Rollback: v1's definition is the latest again, as a new revision.
	if code, out := schemaAct(t, srv, project, []string{name, v1}, actRollbackSchema, nil); code != http.StatusOK {
		t.Fatalf("Rollback = %d: %s", code, out)
	}
	got, _ = c.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	if got.GetDefinition() != testAvroV1 || got.GetRevisionId() == v1 || got.GetRevisionId() == v2 {
		t.Errorf("after the rollback GetSchema = %v; want v1's definition as a new latest revision", got)
	}

	// Delete revision: v2 is gone from the client's revisions.
	if code, out := schemaAct(t, srv, project, []string{name, v2}, actDeleteSchemaRevision, nil); code != http.StatusOK {
		t.Fatalf("Delete revision = %d: %s", code, out)
	}
	if revs := sdkRevisions(t, c, name); len(revs) != 2 || revs[v2] != "" {
		t.Errorf("after the delete the client lists %v; want two revisions without %s", revs, v2)
	}

	// Test message: the verdict, and nothing else.
	code, out := schemaAct(t, srv, project, []string{name}, actTestMessage, map[string]string{"encoding": "JSON", "message": `{"name":"x"}`})
	var res struct{ Result console.Listing }
	_ = json.Unmarshal([]byte(out), &res)
	if code != http.StatusOK || len(res.Result.Items) != 1 || res.Result.Items[0].Name != "Valid" {
		t.Errorf("Test message = %d %s; want the verdict Valid", code, out)
	}
	if code, out := schemaAct(t, srv, project, []string{name}, actTestMessage, map[string]string{"encoding": "BINARY", "message": "not base64!"}); code != http.StatusBadRequest ||
		!strings.Contains(out, "base64") {
		t.Errorf("a BINARY message that is not base64 = %d %s; want 400 naming base64", code, out)
	}

	// Another project's schema is not reached.
	other := "projects/elsewhere/schemas/burrow"
	if code, _ := consoleDelete(t, srv, "pubsub-schemas", project, other); code != http.StatusBadRequest {
		t.Errorf("deleting another project's schema = %d; want 400", code)
	}
	if code, _ := schemaAct(t, srv, "elsewhere", []string{name}, actDeleteSchema, nil); code != http.StatusBadRequest {
		t.Errorf("an action on another project's schema = %d; want 400", code)
	}

	// Delete schema from its page: NOT_FOUND through the client.
	if code, out := schemaAct(t, srv, project, []string{name}, actDeleteSchema, nil); code != http.StatusOK {
		t.Fatalf("Delete schema = %d: %s", code, out)
	}
	if _, err := c.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("after Delete schema GetSchema = %v; want NOT_FOUND", err)
	}

	// And from the list row, through DeleteSchema.
	if _, err := c.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + project, SchemaId: "again",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: testAvroV1}}); err != nil {
		t.Fatal(err)
	}
	if code, out := consoleDelete(t, srv, "pubsub-schemas", project, "projects/"+project+"/schemas/again"); code != http.StatusOK {
		t.Fatalf("console delete = %d: %s", code, out)
	}
	var none console.Listing
	getJSON200(t, srv.URL+"/api/resources/pubsub-schemas?project="+project, &none)
	if len(none.Items) != 0 {
		t.Errorf("after the deletes the screen lists %+v", none.Items)
	}
}

// What ValidateSchema refuses is not created and not committed, and the form
// is answered with the API's own message; so is a message ValidateMessage
// refuses. pstest accepts any non-empty definition, so the refusals are
// injected with the emulator's own messages (#788).
func TestSchemaFormsShowTheAPIsRefusal(t *testing.T) {
	ctx := context.Background()
	const project = "schema-proj"
	srv, c := schemasConsole(t,
		pstest.WithErrorInjection("ValidateSchema", codes.InvalidArgument, "Could not parse schema definition"),
		pstest.WithErrorInjection("ValidateMessage", codes.InvalidArgument, "Could not parse JSON Avro message"))

	code, out := consolePost(t, srv.URL+"/api/resources/pubsub-schemas?project="+project,
		map[string]string{"name": "burrow", "definition": `{"type":"record"`})
	if code != http.StatusBadRequest || !strings.Contains(out, "InvalidArgument: Could not parse schema definition") {
		t.Errorf("creating an invalid definition = %d %s; want 400 with ValidateSchema's message", code, out)
	}
	it := c.ListSchemas(ctx, &pubsubpb.ListSchemasRequest{Parent: "projects/" + project})
	if s, err := it.Next(); err != iterator.Done {
		t.Errorf("a definition ValidateSchema refused was created: %v %v", s, err)
	}

	name := "projects/" + project + "/schemas/burrow"
	if _, err := c.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + project, SchemaId: "burrow",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: testAvroV1}}); err != nil {
		t.Fatal(err)
	}
	if code, out := schemaAct(t, srv, project, []string{name}, actCommitSchema, map[string]string{"definition": "{"}); code != http.StatusBadRequest ||
		!strings.Contains(out, "Could not parse schema definition") {
		t.Errorf("committing an invalid definition = %d %s; want 400 with ValidateSchema's message", code, out)
	}
	if revs := sdkRevisions(t, c, name); len(revs) != 1 {
		t.Errorf("a definition ValidateSchema refused was committed: %v", revs)
	}
	if code, out := schemaAct(t, srv, project, []string{name}, actTestMessage, map[string]string{"encoding": "JSON", "message": `{"nope":1}`}); code != http.StatusBadRequest ||
		!strings.Contains(out, "Could not parse JSON Avro message") {
		t.Errorf("testing an invalid message = %d %s; want 400 with ValidateMessage's message", code, out)
	}
}
