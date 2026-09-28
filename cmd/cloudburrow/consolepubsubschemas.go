package main

// Pub/Sub schemas (#788).
//
// The whole SchemaService is verified with the official client
// (TestPubSubSchemaService), and the console had no schema screen at all.
// This adds one: a Schemas page under Pub/Sub that lists a project's schemas,
// opens each one (its definition and its revisions), creates one, commits a
// new revision, rolls back to a revision, deletes a revision, deletes the
// schema, and tests a message against it. Every call is the official
// apiv1.SchemaClient's against the emulator, the call an application makes,
// so a refusal is the emulator's own and is shown as it came.
//
// What the pinned emulator does, measured against it and asserted by
// TestConsolePubSubSchemas:
//   - Protocol Buffer schemas are refused: ValidateSchema, CreateSchema and
//     CommitSchema of type PROTOCOL_BUFFER answer UNIMPLEMENTED "Protocol
//     buffer support not implemented in emulator". The forms therefore offer
//     Avro only, and say why.
//   - A definition that is not Avro is refused INVALID_ARGUMENT "Could not
//     parse schema definition", by ValidateSchema as by CreateSchema and
//     CommitSchema; the console asks ValidateSchema first, so nothing is
//     created from a definition it refuses.
//   - The last revision cannot be deleted: DeleteSchemaRevision answers
//     INVALID_ARGUMENT "Cannot delete last revision. Please use
//     DeleteSchema." Delete revision is offered only while there are two or
//     more.
//   - RollbackSchema to a revision commits a copy of it as a new latest
//     revision. Offered on every revision but the latest, where it would
//     copy what is already current.
//   - Deleting a schema a topic uses is allowed, and the topic's schema
//     settings then name "_deleted-schema_".

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	actCommitSchema         = "commit-revision"
	actTestMessage          = "test-message"
	actDeleteSchema         = "delete-schema"
	actRollbackSchema       = "rollback"
	actDeleteSchemaRevision = "delete-revision"
)

// pubsubProtobufRefusal is the emulator's answer to a Protocol Buffer schema,
// word for word, as TestConsolePubSubSchemas asserts it.
const pubsubProtobufRefusal = "Protocol buffer support not implemented in emulator"

// pubsubSchemaAvroOnly is why the forms take an Avro definition and offer no
// type.
const pubsubSchemaAvroOnly = "Avro only: this emulator refuses Protocol Buffer schemas as UNIMPLEMENTED (\"" +
	pubsubProtobufRefusal + "\")."

// deletedSchema is the schema Pub/Sub reports for a topic whose schema has
// been deleted.
const deletedSchema = "_deleted-schema_"

// schemaIDPattern is the resource ID rule Pub/Sub applies to a schema, as to
// topics and subscriptions: 3-255 characters, starting with a letter. The
// emulator refuses a shorter one as an invalid name.
const schemaIDPattern = `^[A-Za-z][A-Za-z0-9._~%+\-]{2,254}$`

// pubsubSchemasProvider is the Pub/Sub Schemas screen (#788).
type pubsubSchemasProvider struct{ endpoint string }

func (pubsubSchemasProvider) ID() string    { return "pubsub-schemas" }
func (pubsubSchemasProvider) Title() string { return "Pub/Sub schemas" }

// client is the official generated SchemaClient against the emulator, dialled
// the way the admin clients are.
func (p pubsubSchemasProvider) client(ctx context.Context) (*vkit.SchemaClient, error) {
	return vkit.NewSchemaClient(ctx,
		option.WithEndpoint(p.endpoint),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
}

func schemaPrefix(project string) string { return "projects/" + project + "/schemas/" }

func schemaOf(project, name string) error {
	if project == "" || !strings.HasPrefix(name, schemaPrefix(project)) || strings.Contains(name, "@") {
		return fmt.Errorf("%s is not a schema of project %s", name, project)
	}
	return nil
}

// schemaType is the type column: the enum's own name, which is what the API
// and gcloud print.
func schemaType(s *pubsubpb.Schema) string { return s.GetType().String() }

func schemaRevisionTime(s *pubsubpb.Schema) string {
	if s.GetRevisionCreateTime() == nil {
		return "—"
	}
	return s.GetRevisionCreateTime().AsTime().UTC().Format(time.RFC3339)
}

func (p pubsubSchemasProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{
		Columns:    []string{"Type", "Latest revision", "Revision created"},
		NameColumn: "Schema",
		Noun:       "schemas",
	}
	if project == "" {
		out.Prompt = "Pub/Sub lists schemas per project. Choose one in the toolbar."
		return out, nil
	}
	c, err := p.client(ctx)
	if err != nil {
		return console.Listing{}, fmt.Errorf("connect to Pub/Sub: %w", err)
	}
	defer func() { _ = c.Close() }()

	it := c.ListSchemas(ctx, &pubsubpb.ListSchemasRequest{Parent: "projects/" + project, View: pubsubpb.SchemaView_BASIC})
	for {
		s, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return console.Listing{}, fmt.Errorf("list schemas: %w", err)
		}
		if !strings.HasPrefix(s.GetName(), schemaPrefix(project)) {
			continue
		}
		out.Items = append(out.Items, console.Resource{Name: s.GetName(), Fields: map[string]string{
			"Type":             schemaType(s),
			"Latest revision":  orDash(s.GetRevisionId()),
			"Revision created": schemaRevisionTime(s),
		}})
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	return out, nil
}

// CreateForm implements console.Creator: a schema ID and an Avro definition.
func (pubsubSchemasProvider) CreateForm() (string, []console.Field) {
	return "Create schema", []console.Field{
		{
			Name: "name", Label: "Schema ID", Type: "text", Required: true, Pattern: schemaIDPattern,
			Help: "3-255 characters, starting with a letter.",
		},
		{
			Name: "definition", Label: "Definition", Type: "textarea", Required: true,
			Help: "An Avro schema, in JSON. " + pubsubSchemaAvroOnly +
				" The definition is checked with ValidateSchema first, and one it refuses is not created.",
		},
	}
}

// validateSchema asks the API whether a definition is one it accepts. Its
// refusal is returned as it came, so the form shows the API's own message.
func validateSchema(ctx context.Context, c *vkit.SchemaClient, project string, typ pubsubpb.Schema_Type, definition string) error {
	_, err := c.ValidateSchema(ctx, &pubsubpb.ValidateSchemaRequest{
		Parent: "projects/" + project,
		Schema: &pubsubpb.Schema{Type: typ, Definition: definition},
	})
	return err
}

// Create implements console.Creator: ValidateSchema, then CreateSchema.
func (p pubsubSchemasProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before creating a schema")
	}
	id := strings.TrimSpace(values["name"])
	if id == "" {
		return "", errors.New("schema ID is required")
	}
	c, err := p.client(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()

	definition := values["definition"]
	if err := validateSchema(ctx, c, project, pubsubpb.Schema_AVRO, definition); err != nil {
		return "", err
	}
	s, err := c.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{
		Parent: "projects/" + project, SchemaId: id,
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: definition},
	})
	if err != nil {
		return "", err
	}
	return s.GetName(), nil
}

// Delete implements console.Deleter through DeleteSchema. A schema of another
// project is refused before any call.
func (p pubsubSchemasProvider) Delete(ctx context.Context, project, name string) error {
	if err := schemaOf(project, name); err != nil {
		return err
	}
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.DeleteSchema(ctx, &pubsubpb.DeleteSchemaRequest{Name: name})
}

// schemaRevisions lists a schema's revisions, newest first.
func schemaRevisions(ctx context.Context, c *vkit.SchemaClient, name string) ([]*pubsubpb.Schema, error) {
	var out []*pubsubpb.Schema
	it := c.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	for {
		s, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].GetRevisionCreateTime().AsTime().After(out[j].GetRevisionCreateTime().AsTime())
	})
	return out, nil
}

// revisionActions are what one revision row offers: Rollback on every
// revision but the latest, and Delete revision while there is more than one.
// The same list is put on the row by Detail and returned by DetailActions for
// the row's path, so the row offers exactly what the action route accepts.
func revisionActions(latest string, count int, revision string) []console.Action {
	var out []console.Action
	if revision != latest {
		out = append(out, console.Action{ID: actRollbackSchema, Label: "Roll back to this revision"})
	}
	if count > 1 {
		out = append(out, console.Action{ID: actDeleteSchemaRevision, Label: "Delete revision", Destructive: true})
	}
	return out
}

// Detail implements console.Driller for one schema: its latest definition and
// its revisions.
func (p pubsubSchemasProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		// A revision row has actions at [schema, revision] but no page there.
		// Empty rather than nil, so the server does not attach the row's
		// actions to a page that says it cannot be opened.
		d := console.DeeperThan(1, path)
		d.Actions = []console.Action{}
		return d, nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	name := path[0]
	if err := schemaOf(project, name); err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	c, err := p.client(ctx)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Pub/Sub: " + err.Error()}, nil
	}
	defer func() { _ = c.Close() }()

	s, err := c.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_FULL})
	if err != nil {
		return console.Detail{}, err
	}

	revs := console.Listing{
		Columns: []string{"Created", "Definition"}, NameColumn: "Revision", Noun: "revisions", AlwaysStatus: true,
	}
	list, err := schemaRevisions(ctx, c, name)
	if err != nil {
		revs.Unavailable = "listing revisions: " + err.Error()
	}
	for _, r := range list {
		row := console.Resource{
			Name: r.GetRevisionId(),
			Fields: map[string]string{
				"Created":    schemaRevisionTime(r),
				"Definition": r.GetDefinition(),
			},
			Actions: revisionActions(s.GetRevisionId(), len(list), r.GetRevisionId()),
		}
		if r.GetRevisionId() == s.GetRevisionId() {
			row.Status = "Latest"
		}
		revs.Items = append(revs.Items, row)
	}
	revs.Total = len(revs.Items)
	if revs.Total == 1 {
		revs.Note = "The only revision cannot be deleted: delete the schema instead."
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "Schema", Value: s.GetName()},
			{Label: "Type", Value: schemaType(s)},
			{Label: "Latest revision", Value: orDash(s.GetRevisionId())},
			{Label: "Revision created", Value: schemaRevisionTime(s)},
		},
		Sections: []console.Section{
			{ID: "definition", Label: "Definition", Kind: console.KindText, Text: s.GetDefinition()},
			{ID: "revisions", Label: "Revisions", Listing: revs},
		},
	}, nil
}

// DetailActions implements console.PathActor: on a schema's page, Commit
// revision, Test message and Delete schema; on a revision row, Rollback and
// Delete revision.
func (p pubsubSchemasProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) < 1 || len(path) > 2 || schemaOf(project, path[0]) != nil {
		return nil
	}
	c, err := p.client(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close() }()
	s, err := c.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: path[0], View: pubsubpb.SchemaView_FULL})
	if err != nil {
		return nil
	}

	if len(path) == 2 {
		list, err := schemaRevisions(ctx, c, path[0])
		if err != nil {
			return nil
		}
		for _, r := range list {
			if r.GetRevisionId() == path[1] {
				return revisionActions(s.GetRevisionId(), len(list), path[1])
			}
		}
		return nil
	}

	return []console.Action{
		{ID: actCommitSchema, Label: "Commit revision", Fields: []console.Field{{
			Name: "definition", Label: "Definition", Type: "textarea", Required: true, Default: s.GetDefinition(),
			Help: "The new revision's Avro definition, prefilled with the latest. It becomes the latest revision; " +
				"the others are kept. Checked with ValidateSchema first, and one it refuses is not committed.",
		}}},
		{ID: actTestMessage, Label: "Test message", Fields: []console.Field{
			{
				Name: "encoding", Label: "Encoding", Type: "text", Required: true, Default: "JSON",
				Pattern: `^(JSON|BINARY)$`,
				Help:    "JSON, or BINARY with the message given in base64.",
			},
			{
				Name: "message", Label: "Message", Type: "textarea", Required: true,
				Help: "Checked with ValidateMessage against the latest revision. Nothing is published.",
			},
		}},
		{ID: actDeleteSchema, Label: "Delete schema", Destructive: true, Leaves: true},
	}
}

// ActAt implements console.PathActor.
func (p pubsubSchemasProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, values)
	return err
}

// ActAtResult implements console.ResultActor: Test message answers with its
// verdict; the others with nothing.
func (p pubsubSchemasProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) < 1 || len(path) > 2 {
		return nil, errors.New("Pub/Sub schema actions apply to a schema or one of its revisions")
	}
	name := path[0]
	if err := schemaOf(project, name); err != nil {
		return nil, err
	}
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()

	if len(path) == 2 {
		switch action {
		case actRollbackSchema:
			_, err := c.RollbackSchema(ctx, &pubsubpb.RollbackSchemaRequest{Name: name, RevisionId: path[1]})
			return nil, err
		case actDeleteSchemaRevision:
			_, err := c.DeleteSchemaRevision(ctx, &pubsubpb.DeleteSchemaRevisionRequest{Name: name + "@" + path[1]})
			return nil, err
		default:
			return nil, fmt.Errorf("unknown action %q on a schema revision", action)
		}
	}

	switch action {
	case actCommitSchema:
		cur, err := c.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_BASIC})
		if err != nil {
			return nil, err
		}
		definition := values["definition"]
		if err := validateSchema(ctx, c, project, cur.GetType(), definition); err != nil {
			return nil, err
		}
		_, err = c.CommitSchema(ctx, &pubsubpb.CommitSchemaRequest{Name: name,
			Schema: &pubsubpb.Schema{Name: name, Type: cur.GetType(), Definition: definition}})
		return nil, err

	case actTestMessage:
		var enc pubsubpb.Encoding
		var msg []byte
		switch strings.TrimSpace(values["encoding"]) {
		case "JSON":
			enc, msg = pubsubpb.Encoding_JSON, []byte(values["message"])
		case "BINARY":
			enc = pubsubpb.Encoding_BINARY
			b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(values["message"]))
			if err != nil {
				return nil, fmt.Errorf("message: a BINARY message is given in base64: %w", err)
			}
			msg = b
		default:
			return nil, errors.New("encoding: JSON or BINARY")
		}
		cur, err := c.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_BASIC})
		if err != nil {
			return nil, err
		}
		if _, err := c.ValidateMessage(ctx, &pubsubpb.ValidateMessageRequest{
			Parent: "projects/" + project, SchemaSpec: &pubsubpb.ValidateMessageRequest_Name{Name: name},
			Message: msg, Encoding: enc,
		}); err != nil {
			return nil, err
		}
		return &console.Listing{
			Columns: []string{"Revision", "Encoding"}, NameColumn: "Verdict", Noun: "verdicts",
			Items: []console.Resource{{Name: "Valid", Fields: map[string]string{
				"Revision": cur.GetRevisionId(), "Encoding": enc.String(),
			}}},
			Total: 1,
			Note:  "ValidateMessage accepted the message against " + name + ". Nothing was published.",
		}, nil

	case actDeleteSchema:
		return nil, c.DeleteSchema(ctx, &pubsubpb.DeleteSchemaRequest{Name: name})

	default:
		return nil, fmt.Errorf("unknown action %q on a schema", action)
	}
}

var (
	_ console.Driller     = pubsubSchemasProvider{}
	_ console.Creator     = pubsubSchemasProvider{}
	_ console.Deleter     = pubsubSchemasProvider{}
	_ console.ResultActor = pubsubSchemasProvider{}
)
