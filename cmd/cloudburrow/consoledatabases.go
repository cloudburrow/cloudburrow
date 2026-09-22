package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/datastore"
	"cloud.google.com/go/firestore"
	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/identity-wael/cloudburrow/internal/console"
)

// The four opt-in databases had working backends and no screens: they were
// reachable by an SDK and invisible in the console, which is the kind of gap a
// developer reads as "not implemented".
//
// Each screen shows the top level of that database's hierarchy — the thing you
// would look for first to confirm your application wrote what you expected.
// None of them offers create or delete: these are Google's emulators and the
// shape of a write differs enough per product that an untested form would be
// the working-looking control the parity specification forbids.

// dbTimeout bounds a screen's read. An emulator that is slow to answer should
// produce an error a developer can see, not a page that hangs.
const dbTimeout = 15 * time.Second

func localOpts(endpoint string) []option.ClientOption {
	return []option.ClientOption{
		option.WithEndpoint(endpoint),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	}
}

// --- Firestore ------------------------------------------------------------

type firestoreProvider struct{ endpoint string }

func (firestoreProvider) ID() string    { return "firestore" }
func (firestoreProvider) Title() string { return "Firestore" }

func (p firestoreProvider) List(ctx context.Context, project string) (console.Listing, error) {
	base := console.Listing{Columns: []string{"Documents"}, Noun: "collections", NameColumn: "Collection"}
	if project == "" {
		base.Prompt = "Firestore holds collections per project. Choose one in the toolbar."
		return base, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	// Listing collections is a metadata operation, and the emulator refuses
	// those without an owner token: "Metadata operations require admin
	// authentication". Setting FIRESTORE_EMULATOR_HOST is what normally
	// arranges that, but an environment variable is process-wide and this
	// console runs inside the same process as everything else. The token is
	// sent per call instead.
	opts := append(localOpts(p.endpoint),
		option.WithGRPCDialOption(grpc.WithPerRPCCredentials(emulatorOwner{})))
	c, err := firestore.NewClient(ctx, project, opts...)
	if err != nil {
		base.Unavailable = "cannot reach Firestore: " + err.Error()
		return base, nil
	}
	defer c.Close()

	it := c.Collections(ctx)
	var items []console.Resource
	for {
		col, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			base.Unavailable = "listing collections: " + err.Error()
			return base, nil
		}
		// Counting is a read of every document, so it is bounded and the
		// column says when it stopped counting rather than reporting a total
		// it did not establish.
		count, more := countDocuments(ctx, col)
		shown := fmt.Sprintf("%d", count)
		if more {
			shown = fmt.Sprintf("%d+", count)
		}
		items = append(items, console.Resource{
			Name:   col.ID,
			Fields: map[string]string{"Documents": shown},
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	base.Items, base.Total = items, len(items)
	base.Note = "Document counts stop at 100; a collection with more shows a trailing plus. " +
		"Collections are not created or deleted here: one exists because a document is in it, " +
		"so the buttons would really be creating and deleting documents."
	return base, nil
}

const documentCountCap = 100

func countDocuments(ctx context.Context, col *firestore.CollectionRef) (int, bool) {
	it := col.Limit(documentCountCap + 1).Documents(ctx)
	defer it.Stop()
	n := 0
	for {
		if _, err := it.Next(); err != nil {
			break
		}
		n++
		if n > documentCountCap {
			return documentCountCap, true
		}
	}
	return n, false
}

// --- Datastore ------------------------------------------------------------

type datastoreProvider struct{ endpoint string }

func (datastoreProvider) ID() string    { return "datastore" }
func (datastoreProvider) Title() string { return "Datastore" }

func (p datastoreProvider) List(ctx context.Context, project string) (console.Listing, error) {
	base := console.Listing{Columns: []string{}, Noun: "kinds", NameColumn: "Kind"}
	if project == "" {
		base.Prompt = "Datastore holds entities per project. Choose one in the toolbar."
		return base, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	c, err := datastore.NewClient(ctx, project, localOpts(p.endpoint)...)
	if err != nil {
		base.Unavailable = "cannot reach Datastore: " + err.Error()
		return base, nil
	}
	defer c.Close()

	// __kind__ is Datastore's own metadata query: the list of kinds is a
	// query rather than an API call, which is why this looks unlike the
	// others.
	keys, err := c.GetAll(ctx, datastore.NewQuery("__kind__").KeysOnly(), nil)
	if err != nil {
		base.Unavailable = "listing kinds: " + err.Error()
		return base, nil
	}
	items := make([]console.Resource, 0, len(keys))
	for _, k := range keys {
		if strings.HasPrefix(k.Name, "__") {
			continue // Datastore's own metadata kinds
		}
		items = append(items, console.Resource{Name: k.Name})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	base.Items, base.Total = items, len(items)
	base.Note = "Kinds are not created or deleted here: a kind exists because an entity has it, " +
		"so the buttons would really be creating and deleting entities."
	return base, nil
}

// --- Bigtable -------------------------------------------------------------

type bigtableProvider struct{ endpoint string }

func (bigtableProvider) ID() string    { return "bigtable" }
func (bigtableProvider) Title() string { return "Bigtable" }

// bigtableInstance is the instance the console reads.
//
// The emulator does not implement instance administration, so there is no list
// to choose from: it serves whatever instance a client asks for. The console
// reads the one CloudBurrow's own tests use and says so, rather than
// presenting an instance picker that would have nothing behind it.
const bigtableInstance = "cloudburrow"

func (p bigtableProvider) List(ctx context.Context, project string) (console.Listing, error) {
	base := console.Listing{Columns: []string{"Column families"}, Noun: "tables", NameColumn: "Table"}
	if project == "" {
		base.Prompt = "Bigtable holds tables per project. Choose one in the toolbar."
		return base, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	admin, err := bigtable.NewAdminClient(ctx, project, bigtableInstance, localOpts(p.endpoint)...)
	if err != nil {
		base.Unavailable = "cannot reach Bigtable: " + err.Error()
		return base, nil
	}
	defer admin.Close()

	tables, err := admin.Tables(ctx)
	if err != nil {
		base.Unavailable = "listing tables: " + err.Error()
		return base, nil
	}
	sort.Strings(tables)

	items := make([]console.Resource, 0, len(tables))
	for _, t := range tables {
		families := "—"
		if info, err := admin.TableInfo(ctx, t); err == nil {
			names := make([]string, 0, len(info.FamilyInfos))
			for _, f := range info.FamilyInfos {
				names = append(names, f.Name)
			}
			sort.Strings(names)
			if len(names) > 0 {
				families = strings.Join(names, ", ")
			}
		}
		items = append(items, console.Resource{
			Name:   t,
			Fields: map[string]string{"Column families": families},
		})
	}
	base.Items, base.Total = items, len(items)
	base.Note = fmt.Sprintf("Instance %q. The emulator implements no instance administration, "+
		"so there is no instance list to choose from.", bigtableInstance)
	return base, nil
}

// --- Spanner --------------------------------------------------------------

type spannerProvider struct{ endpoint string }

func (spannerProvider) ID() string    { return "spanner" }
func (spannerProvider) Title() string { return "Spanner" }

func (p spannerProvider) List(ctx context.Context, project string) (console.Listing, error) {
	// No "State" column: the state is carried as the row's status, and a
	// column repeating it put the same value on the screen twice.
	base := console.Listing{Columns: []string{"Instance"}, Noun: "databases", NameColumn: "Database"}
	if project == "" {
		base.Prompt = "Spanner holds databases per project. Choose one in the toolbar."
		return base, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		base.Unavailable = "cannot reach Spanner: " + err.Error()
		return base, nil
	}
	defer instAdmin.Close()

	dbAdmin, err := database.NewDatabaseAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		base.Unavailable = "cannot reach Spanner: " + err.Error()
		return base, nil
	}
	defer dbAdmin.Close()

	// Databases live under instances, so both levels are walked and the
	// instance travels with each row: two databases called "main" under
	// different instances are different databases.
	instances := instAdmin.ListInstances(ctx, &instancepb.ListInstancesRequest{
		Parent: "projects/" + project,
	})
	var items []console.Resource
	for {
		inst, err := instances.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			base.Unavailable = "listing instances: " + err.Error()
			return base, nil
		}
		instanceID := lastSegment(inst.GetName())

		dbs := dbAdmin.ListDatabases(ctx, &databasepb.ListDatabasesRequest{Parent: inst.GetName()})
		for {
			db, err := dbs.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				base.Unavailable = "listing databases: " + err.Error()
				return base, nil
			}
			items = append(items, console.Resource{
				Name:   lastSegment(db.GetName()),
				Status: db.GetState().String(),
				Fields: map[string]string{"Instance": instanceID},
			})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	base.Items, base.Total = items, len(items)
	base.Note = "In-memory: everything here is gone when the instance restarts."
	return base, nil
}

// emulatorOwner sends the owner token the Firestore emulator expects.
//
// It authenticates nothing: the emulator accepts the literal string and grants
// admin access to whoever presents it, which is why it is safe here and would
// be catastrophic anywhere else. RequireTransportSecurity is false because the
// endpoint is a local plaintext tunnel, and a credential that insisted on TLS
// would simply refuse to be sent.
type emulatorOwner struct{}

func (emulatorOwner) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer owner"}, nil
}

func (emulatorOwner) RequireTransportSecurity() bool { return false }

// --- what is inside one row ------------------------------------------------
//
// A list of collections answers "what is there". Opening one answers "did my
// application write what I expected", which is the question someone opens a
// database console to settle.
//
// Every detail view is bounded. These are development databases and a screen
// that tried to render a large table would hang the page rather than answer
// anything, so each stops at a documented limit and says so.

const detailLimit = 200

func truncatedNote(shown int, what string) string {
	if shown < detailLimit {
		return ""
	}
	return fmt.Sprintf("Showing the first %d %s. There may be more.", detailLimit, what)
}

// summarise renders a value briefly enough for a table cell.
func summarise(v any) string {
	s := fmt.Sprintf("%v", v)
	if len(s) > 80 {
		return s[:77] + "…"
	}
	return s
}

// Detail lists the documents in a Firestore collection.
func (p firestoreProvider) Detail(ctx context.Context, project, name string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Fields"}, Noun: "documents", NameColumn: "Document"}
	if project == "" {
		out.Prompt = "Choose a project in the toolbar."
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	opts := append(localOpts(p.endpoint),
		option.WithGRPCDialOption(grpc.WithPerRPCCredentials(emulatorOwner{})))
	c, err := firestore.NewClient(ctx, project, opts...)
	if err != nil {
		out.Unavailable = "cannot reach Firestore: " + err.Error()
		return out, nil
	}
	defer c.Close()

	it := c.Collection(name).Limit(detailLimit).Documents(ctx)
	defer it.Stop()
	var items []console.Resource
	for {
		doc, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			out.Unavailable = "reading documents: " + err.Error()
			return out, nil
		}
		// The field names and values, not a document count: the point of
		// opening a document is to see what is in it.
		data := doc.Data()
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+": "+summarise(data[k]))
		}
		items = append(items, console.Resource{
			Name:   doc.Ref.ID,
			Fields: map[string]string{"Fields": strings.Join(parts, ", ")},
		})
	}
	out.Items, out.Total = items, len(items)
	out.Note = truncatedNote(len(items), "documents")
	return out, nil
}

// Detail lists the entities of a Datastore kind.
func (p datastoreProvider) Detail(ctx context.Context, project, name string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Properties"}, Noun: "entities", NameColumn: "Key"}
	if project == "" {
		out.Prompt = "Choose a project in the toolbar."
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	c, err := datastore.NewClient(ctx, project, localOpts(p.endpoint)...)
	if err != nil {
		out.Unavailable = "cannot reach Datastore: " + err.Error()
		return out, nil
	}
	defer c.Close()

	// PropertyList keeps this generic: the console has no Go type for a
	// developer's entities and inventing one would only fit the ones it
	// guessed right.
	var entities []datastore.PropertyList
	keys, err := c.GetAll(ctx, datastore.NewQuery(name).Limit(detailLimit), &entities)
	if err != nil {
		out.Unavailable = "reading entities: " + err.Error()
		return out, nil
	}

	items := make([]console.Resource, 0, len(keys))
	for i, k := range keys {
		var parts []string
		if i < len(entities) {
			props := entities[i]
			sort.Slice(props, func(a, b int) bool { return props[a].Name < props[b].Name })
			for _, prop := range props {
				parts = append(parts, prop.Name+": "+summarise(prop.Value))
			}
		}
		id := k.Name
		if id == "" {
			id = fmt.Sprintf("id=%d", k.ID)
		}
		items = append(items, console.Resource{
			Name:   id,
			Fields: map[string]string{"Properties": strings.Join(parts, ", ")},
		})
	}
	out.Items, out.Total = items, len(items)
	out.Note = truncatedNote(len(items), "entities")
	return out, nil
}

// Detail lists the rows of a Bigtable table.
func (p bigtableProvider) Detail(ctx context.Context, project, name string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Cells"}, Noun: "rows", NameColumn: "Row key"}
	if project == "" {
		out.Prompt = "Choose a project in the toolbar."
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	c, err := bigtable.NewClient(ctx, project, bigtableInstance, localOpts(p.endpoint)...)
	if err != nil {
		out.Unavailable = "cannot reach Bigtable: " + err.Error()
		return out, nil
	}
	defer c.Close()

	var items []console.Resource
	err = c.Open(name).ReadRows(ctx, bigtable.InfiniteRange(""), func(row bigtable.Row) bool {
		var parts []string
		families := make([]string, 0, len(row))
		for family := range row {
			families = append(families, family)
		}
		sort.Strings(families)
		for _, family := range families {
			for _, item := range row[family] {
				parts = append(parts, item.Column+": "+summarise(string(item.Value)))
			}
		}
		items = append(items, console.Resource{
			Name:   row.Key(),
			Fields: map[string]string{"Cells": strings.Join(parts, ", ")},
		})
		return len(items) < detailLimit
	})
	if err != nil {
		out.Unavailable = "reading rows: " + err.Error()
		return out, nil
	}
	out.Items, out.Total = items, len(items)
	out.Note = truncatedNote(len(items), "rows")
	return out, nil
}

// Detail lists the tables in a Spanner database.
func (p spannerProvider) Detail(ctx context.Context, project, name string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Instance", "Columns"}, Noun: "tables", NameColumn: "Table"}
	if project == "" {
		out.Prompt = "Choose a project in the toolbar."
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	// A database name is unique only within its instance, so the instance is
	// found rather than assumed: two instances may each hold a "main".
	instanceID, err := p.instanceOf(ctx, project, name)
	if err != nil {
		out.Unavailable = err.Error()
		return out, nil
	}

	dbName := fmt.Sprintf("projects/%s/instances/%s/databases/%s", project, instanceID, name)
	c, err := spanner.NewClient(ctx, dbName, localOpts(p.endpoint)...)
	if err != nil {
		out.Unavailable = "cannot open the database: " + err.Error()
		return out, nil
	}
	defer c.Close()

	// INFORMATION_SCHEMA is Spanner's own catalogue, so this asks the
	// database what it holds rather than keeping a second record of it.
	stmt := spanner.Statement{SQL: `
		SELECT t.TABLE_NAME, COUNT(c.COLUMN_NAME)
		FROM INFORMATION_SCHEMA.TABLES t
		LEFT JOIN INFORMATION_SCHEMA.COLUMNS c
		  ON c.TABLE_NAME = t.TABLE_NAME AND c.TABLE_SCHEMA = t.TABLE_SCHEMA
		WHERE t.TABLE_SCHEMA = ''
		GROUP BY t.TABLE_NAME
		ORDER BY t.TABLE_NAME`}
	iter := c.Single().Query(ctx, stmt)
	defer iter.Stop()

	var items []console.Resource
	for {
		row, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			out.Unavailable = "reading the schema: " + err.Error()
			return out, nil
		}
		var table string
		var columns int64
		if err := row.Columns(&table, &columns); err != nil {
			out.Unavailable = "decoding the schema: " + err.Error()
			return out, nil
		}
		items = append(items, console.Resource{
			Name: table,
			Fields: map[string]string{
				"Instance": instanceID,
				"Columns":  fmt.Sprintf("%d", columns),
			},
		})
	}
	out.Items, out.Total = items, len(items)
	return out, nil
}

// instanceOf finds which instance holds a database.
func (p spannerProvider) instanceOf(ctx context.Context, project, database string) (string, error) {
	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return "", fmt.Errorf("cannot reach Spanner: %w", err)
	}
	defer instAdmin.Close()
	dbAdmin, err := database2AdminClient(ctx, p.endpoint)
	if err != nil {
		return "", err
	}
	defer dbAdmin.Close()

	instances := instAdmin.ListInstances(ctx, &instancepb.ListInstancesRequest{Parent: "projects/" + project})
	for {
		inst, err := instances.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return "", fmt.Errorf("listing instances: %w", err)
		}
		dbs := dbAdmin.ListDatabases(ctx, &databasepb.ListDatabasesRequest{Parent: inst.GetName()})
		for {
			db, err := dbs.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return "", fmt.Errorf("listing databases: %w", err)
			}
			if lastSegment(db.GetName()) == database {
				return lastSegment(inst.GetName()), nil
			}
		}
	}
	return "", fmt.Errorf("database %q was not found in any instance", database)
}

func database2AdminClient(ctx context.Context, endpoint string) (*database.DatabaseAdminClient, error) {
	c, err := database.NewDatabaseAdminClient(ctx, localOpts(endpoint)...)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Spanner: %w", err)
	}
	return c, nil
}

// --- writes ---------------------------------------------------------------
//
// Only where the product has a real administrative operation for it.
//
// Bigtable tables and Spanner databases are first-class resources with create
// and drop APIs, so the console offers them. Firestore collections and
// Datastore kinds are not: a collection exists because a document is in it and
// stops existing when the last one goes. A "create collection" button would
// actually be creating a document under a name the user did not choose, and a
// "delete collection" would be a bounded, non-atomic loop that could stop
// halfway and leave the thing it claimed to remove. Neither is offered, and
// the screens say why rather than leaving the absence unexplained.

// CreateForm implements console.Creator for Bigtable.
func (bigtableProvider) CreateForm() (string, []console.Field) {
	return "Create table", []console.Field{
		{
			Name: "table", Label: "Table ID", Type: "text", Required: true,
			Help:    "Letters, digits, hyphens and underscores.",
			Pattern: `^[A-Za-z0-9][A-Za-z0-9_\-]{0,49}$`,
		},
		{
			Name: "families", Label: "Column families", Type: "text",
			Help:    "Comma-separated. A table with no column family can hold nothing, so one is created if this is empty.",
			Default: "cf1",
		},
	}
}

// Create implements console.Creator for Bigtable.
func (p bigtableProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", fmt.Errorf("choose a project first")
	}
	table := strings.TrimSpace(values["table"])
	if table == "" {
		return "", fmt.Errorf("a table ID is required")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	admin, err := bigtable.NewAdminClient(ctx, project, bigtableInstance, localOpts(p.endpoint)...)
	if err != nil {
		return "", fmt.Errorf("cannot reach Bigtable: %w", err)
	}
	defer admin.Close()

	if err := admin.CreateTable(ctx, table); err != nil {
		return "", fmt.Errorf("creating the table: %w", err)
	}
	families := splitAndTrim(values["families"])
	if len(families) == 0 {
		families = []string{"cf1"}
	}
	for _, f := range families {
		if err := admin.CreateColumnFamily(ctx, table, f); err != nil {
			// The table exists but is unusable without a family, so the
			// half-made resource is removed rather than left behind.
			_ = admin.DeleteTable(ctx, table)
			return "", fmt.Errorf("creating column family %q: %w", f, err)
		}
	}
	return table, nil
}

// Delete implements console.Deleter for Bigtable.
func (p bigtableProvider) Delete(ctx context.Context, project, name string) error {
	if project == "" {
		return fmt.Errorf("choose a project first")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	admin, err := bigtable.NewAdminClient(ctx, project, bigtableInstance, localOpts(p.endpoint)...)
	if err != nil {
		return fmt.Errorf("cannot reach Bigtable: %w", err)
	}
	defer admin.Close()
	return admin.DeleteTable(ctx, name)
}

// CreateForm implements console.Creator for Spanner.
func (spannerProvider) CreateForm() (string, []console.Field) {
	return "Create database", []console.Field{
		{
			Name: "instance", Label: "Instance ID", Type: "text", Required: true,
			Help:    "Created if it does not exist — the emulator has no instance list to choose from.",
			Default: "main",
			Pattern: `^[a-z][a-z0-9\-]{1,62}[a-z0-9]$`,
		},
		{
			Name: "database", Label: "Database ID", Type: "text", Required: true,
			Help:    "Lowercase letters, digits and hyphens.",
			Pattern: `^[a-z][a-z0-9\-_]{1,28}[a-z0-9]$`,
		},
		{
			// A textarea, not an input: a CREATE TABLE statement past one
			// narrow table does not fit on one line, and a single-line box
			// makes the user edit what they cannot see.
			Name: "ddl", Label: "First table (DDL)", Type: "textarea",
			Help: "Optional. One CREATE TABLE statement; the database is created empty without it.",
		},
	}
}

// CreateOnPage implements console.PageCreator.
//
// Three fields would otherwise be a dialog, but one of them is a DDL
// statement: the dialog gives it 440px and no room to grow.
func (spannerProvider) CreateOnPage() bool { return true }

// Create implements console.Creator for Spanner.
func (p spannerProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", fmt.Errorf("choose a project first")
	}
	instanceID := strings.TrimSpace(values["instance"])
	dbID := strings.TrimSpace(values["database"])
	if instanceID == "" || dbID == "" {
		return "", fmt.Errorf("an instance ID and a database ID are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*dbTimeout)
	defer cancel()

	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return "", fmt.Errorf("cannot reach Spanner: %w", err)
	}
	defer instAdmin.Close()

	// A database needs an instance and the emulator offers no instance
	// administration screen to make one in, so it is created here when
	// absent. The form says so; a silent side effect would be worse.
	if _, err := instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{
		Name: fmt.Sprintf("projects/%s/instances/%s", project, instanceID),
	}); err != nil {
		op, err := instAdmin.CreateInstance(ctx, &instancepb.CreateInstanceRequest{
			Parent:     "projects/" + project,
			InstanceId: instanceID,
			Instance: &instancepb.Instance{
				Config:      fmt.Sprintf("projects/%s/instanceConfigs/emulator-config", project),
				DisplayName: instanceID,
				NodeCount:   1,
			},
		})
		if err != nil {
			return "", fmt.Errorf("creating instance %q: %w", instanceID, err)
		}
		if _, err := op.Wait(ctx); err != nil {
			return "", fmt.Errorf("waiting for instance %q: %w", instanceID, err)
		}
	}

	dbAdmin, err := database.NewDatabaseAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return "", fmt.Errorf("cannot reach Spanner: %w", err)
	}
	defer dbAdmin.Close()

	req := &databasepb.CreateDatabaseRequest{
		Parent:          fmt.Sprintf("projects/%s/instances/%s", project, instanceID),
		CreateStatement: fmt.Sprintf("CREATE DATABASE `%s`", dbID),
	}
	if ddl := strings.TrimSpace(values["ddl"]); ddl != "" {
		req.ExtraStatements = []string{ddl}
	}
	op, err := dbAdmin.CreateDatabase(ctx, req)
	if err != nil {
		return "", fmt.Errorf("creating the database: %w", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		return "", fmt.Errorf("waiting for the database: %w", err)
	}
	return dbID, nil
}

// Delete implements console.Deleter for Spanner.
func (p spannerProvider) Delete(ctx context.Context, project, name string) error {
	if project == "" {
		return fmt.Errorf("choose a project first")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	instanceID, err := p.instanceOf(ctx, project, name)
	if err != nil {
		return err
	}
	dbAdmin, err := database.NewDatabaseAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return fmt.Errorf("cannot reach Spanner: %w", err)
	}
	defer dbAdmin.Close()

	return dbAdmin.DropDatabase(ctx, &databasepb.DropDatabaseRequest{
		Database: fmt.Sprintf("projects/%s/instances/%s/databases/%s", project, instanceID, name),
	})
}

// splitAndTrim splits a comma-separated field, dropping blanks.
func splitAndTrim(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

var (
	_ console.Creator = bigtableProvider{}
	_ console.Deleter = bigtableProvider{}
	_ console.Creator = spannerProvider{}
	_ console.Deleter = spannerProvider{}
	_ console.Driller = firestoreProvider{}
	_ console.Driller = datastoreProvider{}
	_ console.Driller = bigtableProvider{}
	_ console.Driller = spannerProvider{}
)
