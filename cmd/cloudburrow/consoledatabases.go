package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
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
	"google.golang.org/genproto/googleapis/type/latlng"
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

// summariseFrom pulls one resource's own properties out of the listing the
// provider already produces.
//
// The values are the ones the list row showed, read from the provider's own
// API rather than remembered by the browser, so opening a resource by deep
// link shows the same properties as clicking through to it. A resource the
// listing does not know about yields nothing, and the screen then renders no
// card rather than an empty one.
func summariseFrom(list console.Listing, name string) []console.Property {
	for _, item := range list.Items {
		if item.Name != name {
			continue
		}
		out := make([]console.Property, 0, len(list.Columns))
		// In the listing's own column order, with the listing's own labels:
		// a property that is called one thing on the list and another on the
		// detail screen is two facts as far as the reader is concerned.
		for _, c := range list.Columns {
			if v := item.Fields[c]; v != "" {
				out = append(out, console.Property{Label: c, Value: v})
			}
		}
		if item.Status != "" {
			out = append(out, console.Property{Label: "Status", Value: item.Status})
		}
		return out
	}
	return nil
}

// singleSection wraps a provider's contents listing as a one-tab detail page.
//
// One section renders no tab strip, so a provider that has nothing else to
// show costs the reader nothing.
func singleSection(id, label string, list console.Listing, summary []console.Property) console.Detail {
	return console.Detail{
		Summary:     summary,
		Sections:    []console.Section{{ID: id, Label: label, Listing: list}},
		Unavailable: list.Unavailable,
		Prompt:      list.Prompt,
	}
}

// Detail lists the documents in a Firestore collection.
// Detail implements console.Driller for a Firestore collection.
func (p firestoreProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	// Two levels: a collection, and one of its documents. Anything deeper is
	// refused rather than silently collapsed onto the same page.
	if len(path) == 2 {
		return p.documentDetail(ctx, project, path[0], path[1])
	}
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	name := path[0]
	list, err := p.contents(ctx, project, name)
	if err != nil {
		return console.Detail{}, err
	}
	// The properties come from the same List the screen above was built from,
	// so the detail page cannot disagree with the row that led to it.
	var summary []console.Property
	if list.Prompt == "" {
		if parent, err := p.List(ctx, project); err == nil {
			summary = summariseFrom(parent, name)
		}
	}
	return singleSection("documents", "Documents", list, summary), nil
}

func (p firestoreProvider) contents(ctx context.Context, project, name string) (console.Listing, error) {
	out := console.Listing{
		Columns: []string{"Fields"}, Noun: "documents", NameColumn: "Document",
		// A document's fields were flattened into one cell and truncated at 80
		// characters, so the answer to "did my application write what I
		// expected" was "probably, the beginning of it looks right". Each
		// document now opens.
		RowsOpenable: true,
	}
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
// Detail implements console.Driller for a Datastore kind.
func (p datastoreProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	// Two levels: a kind, and one of its entities. Anything deeper is refused
	// rather than silently collapsed onto the same page.
	if len(path) == 2 {
		return p.entityDetail(ctx, project, path[0], path[1])
	}
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	name := path[0]
	list, err := p.contents(ctx, project, name)
	if err != nil {
		return console.Detail{}, err
	}
	// The properties come from the same List the screen above was built from,
	// so the detail page cannot disagree with the row that led to it.
	var summary []console.Property
	if list.Prompt == "" {
		if parent, err := p.List(ctx, project); err == nil {
			summary = summariseFrom(parent, name)
		}
	}
	return singleSection("entities", "Entities", list, summary), nil
}

func (p datastoreProvider) contents(ctx context.Context, project, name string) (console.Listing, error) {
	out := console.Listing{
		Columns: []string{"Properties"}, Noun: "entities", NameColumn: "Key",
		// An entity's properties were one truncated cell. Each entity now opens.
		RowsOpenable: true,
	}
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
// Detail implements console.Driller for a Bigtable table.
func (p bigtableProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	// Two levels: a table, and one of its rows. Anything deeper is refused
	// rather than silently collapsed onto the same page.
	if len(path) == 2 {
		return p.rowDetail(ctx, project, path[0], path[1])
	}
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	name := path[0]
	list, err := p.contents(ctx, project, name)
	if err != nil {
		return console.Detail{}, err
	}
	// The properties come from the same List the screen above was built from,
	// so the detail page cannot disagree with the row that led to it.
	var summary []console.Property
	if list.Prompt == "" {
		if parent, err := p.List(ctx, project); err == nil {
			summary = summariseFrom(parent, name)
		}
	}
	d := console.Detail{
		Summary: summary,
		Sections: []console.Section{
			{ID: "rows", Label: "Rows", Listing: list},
			p.familiesSection(ctx, project, name),
		},
		Unavailable: list.Unavailable,
		Prompt:      list.Prompt,
	}
	return d, nil
}

// familiesSection is a table's column families and their garbage-collection
// policies.
//
// A Bigtable table's schema *is* its column families. The rows listing showed
// cells and never said which families exist, so a family created by an
// application and one that was never created looked the same — both absent from
// a table with no data in them.
func (p bigtableProvider) familiesSection(ctx context.Context, project, table string) console.Section {
	out := console.Listing{
		Columns:    []string{"Garbage collection"},
		NameColumn: "Column family",
		Noun:       "column families",
	}
	sec := console.Section{ID: "families", Label: "Schema", Listing: out}
	if project == "" {
		return sec
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	admin, err := bigtable.NewAdminClient(ctx, project, bigtableInstance, localOpts(p.endpoint)...)
	if err != nil {
		out.Unavailable = "cannot reach the Bigtable admin API: " + err.Error()
		sec.Listing = out
		return sec
	}
	defer admin.Close()

	info, err := admin.TableInfo(ctx, table)
	if err != nil {
		out.Unavailable = "cannot read the table: " + err.Error()
		sec.Listing = out
		return sec
	}
	for _, f := range info.FamilyInfos {
		policy := f.GCPolicy
		if strings.TrimSpace(policy) == "" {
			// An empty policy means cells are kept forever, which is not the
			// same fact as "no policy configured" and reads very differently.
			policy = "never expires"
		}
		out.Items = append(out.Items, console.Resource{
			Name:   f.Name,
			Fields: map[string]string{"Garbage collection": policy},
		})
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	if out.Total == 0 && out.Unavailable == "" {
		out.Note = "This table has no column families, so nothing can be written " +
			"to it: a Bigtable write names the family it goes into."
	}
	sec.Listing = out
	return sec
}

func (p bigtableProvider) contents(ctx context.Context, project, name string) (console.Listing, error) {
	out := console.Listing{
		Columns: []string{"Cells"}, Noun: "rows", NameColumn: "Row key",
		// A row's cells were one truncated cell of their own. Each row now opens.
		RowsOpenable: true,
	}
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
// Detail implements console.Driller for a Spanner database.
func (p spannerProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	// One level: the row on the list screen. Anything deeper is refused
	// rather than silently collapsed onto the same page.
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	name := path[0]
	list, err := p.contents(ctx, project, name)
	if err != nil {
		return console.Detail{}, err
	}
	// The properties come from the same List the screen above was built from,
	// so the detail page cannot disagree with the row that led to it.
	var summary []console.Property
	if list.Prompt == "" {
		if parent, err := p.List(ctx, project); err == nil {
			summary = summariseFrom(parent, name)
		}
	}
	return singleSection("tables", "Tables", list, summary), nil
}

func (p spannerProvider) contents(ctx context.Context, project, name string) (console.Listing, error) {
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

// documentDetail is one Firestore document, field by field.
//
// The collection listing renders every field into one cell and truncates it at
// eighty characters, which is enough to recognise a document and not enough to
// check one. This is the whole document: each field with its own value and the
// type Firestore stored it as, because "42" and "\"42\"" are different writes and
// a flattened cell shows them identically.
func (p firestoreProvider) documentDetail(ctx context.Context, project, collection, id string) (console.Detail, error) {
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	opts := append(localOpts(p.endpoint),
		option.WithGRPCDialOption(grpc.WithPerRPCCredentials(emulatorOwner{})))
	c, err := firestore.NewClient(ctx, project, opts...)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Firestore: " + err.Error()}, nil
	}
	defer c.Close()

	snap, err := c.Collection(collection).Doc(id).Get(ctx)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the document: " + err.Error()}, nil
	}

	fields := console.Listing{
		Columns:    []string{"Type", "Value"},
		NameColumn: "Field",
		Noun:       "fields",
	}
	data := snap.Data()
	for _, key := range sortedAnyKeys(data) {
		fields.Items = append(fields.Items, console.Resource{
			Name: key,
			Fields: map[string]string{
				"Type": firestoreType(data[key]),
				// Not truncated. This page is the reason the listing's cell
				// could be.
				"Value": renderValue(data[key]),
			},
		})
	}
	fields.Total = len(fields.Items)
	if fields.Total == 0 {
		fields.Note = "This document has no fields. In Firestore that is a real " +
			"state: a document can exist as a parent of subcollections."
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "Collection", Value: collection},
			{Label: "Document ID", Value: snap.Ref.ID},
			{Label: "Path", Value: snap.Ref.Path},
			{Label: "Fields", Value: fmt.Sprint(fields.Total)},
			{Label: "Created", Value: snap.CreateTime.Format(time.RFC3339)},
			{Label: "Updated", Value: snap.UpdateTime.Format(time.RFC3339)},
		},
		Sections: []console.Section{{ID: "fields", Label: "Fields", Listing: fields}},
	}, nil
}

// QueryForm is the Firestore query builder.
//
// Firestore has no query language a console can offer, so the real console
// builds queries from controls: a field, an operator, a value. A textarea here
// would mean inventing a syntax, and a syntax nobody else accepts is worse than
// no query at all.
func (p firestoreProvider) QueryForm(path []string) (string, []console.Field) {
	return "Run query", []console.Field{
		{Name: "field", Label: "Field", Type: "text",
			Help: "The field to filter on. Leave the filter empty to list in order."},
		{Name: "op", Label: "Operator", Type: "text", Default: "==",
			Help:    `One of ==, !=, <, <=, >, >=, in, array-contains.`,
			Pattern: `^(==|!=|<|<=|>|>=|in|array-contains)$`},
		{Name: "value", Label: "Value", Type: "text",
			Help: "Compared as a number when it parses as one, as a boolean for " +
				"true and false, and as a string otherwise — which is what the " +
				"stored type has to match."},
		{Name: "orderBy", Label: "Order by", Type: "text",
			Help: "Optional field name."},
		{Name: "descending", Label: "Descending", Type: "checkbox"},
		{Name: "limit", Label: "Limit", Type: "text", Default: "50",
			Pattern: `^[0-9]{1,4}$`,
			Help:    fmt.Sprintf("At most %d, which is where every listing on this console stops.", detailLimit)},
	}
}

// Build runs the query the form describes.
func (p firestoreProvider) Build(ctx context.Context, project string, path []string, values map[string]string) (console.Listing, error) {
	out := console.Listing{
		Columns: []string{"Fields"}, Noun: "documents", NameColumn: "Document",
		RowsOpenable: true,
	}
	if project == "" {
		out.Prompt = "Choose a project in the toolbar."
		return out, nil
	}
	if len(path) != 1 {
		return console.Listing{}, fmt.Errorf("a Firestore query runs against a collection")
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

	q := c.Collection(path[0]).Query
	if field := strings.TrimSpace(values["field"]); field != "" {
		op := strings.TrimSpace(values["op"])
		if op == "" {
			op = "=="
		}
		value, err := typedValue(values["value"], op)
		if err != nil {
			return console.Listing{}, err
		}
		q = q.Where(field, op, value)
	}
	if order := strings.TrimSpace(values["orderBy"]); order != "" {
		dir := firestore.Asc
		if values["descending"] == "true" {
			dir = firestore.Desc
		}
		q = q.OrderBy(order, dir)
	}
	q = q.Limit(queryLimit(values["limit"]))

	it := q.Documents(ctx)
	defer it.Stop()
	for {
		doc, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			// Firestore's own message, which for a missing composite index
			// names the index that needs creating. Replacing it with "query
			// failed" would throw away the fix.
			return console.Listing{}, err
		}
		out.Items = append(out.Items, console.Resource{
			Name:   doc.Ref.ID,
			Fields: map[string]string{"Fields": flatten(doc.Data())},
			Opens:  []string{path[0], doc.Ref.ID},
		})
	}
	out.Total = len(out.Items)
	return out, nil
}

// typedValue reads a form value as the type the comparison needs.
//
// Firestore compares by stored type, so "42" as a string never matches a number
// 42. Guessing from the text is what the real console does too, and getting it
// wrong silently returns nothing — so the help text says what the rule is.
func typedValue(raw, op string) (any, error) {
	raw = strings.TrimSpace(raw)
	if op == "in" {
		// "in" takes a list. Split on commas, each element typed on its own.
		parts := splitAndTrim(raw)
		if len(parts) == 0 {
			return nil, fmt.Errorf(`the "in" operator needs at least one value`)
		}
		out := make([]any, 0, len(parts))
		for _, part := range parts {
			v, err := typedValue(part, "==")
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	switch raw {
	case "":
		return nil, fmt.Errorf("a filter on a field needs a value")
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if n, err := strconv.ParseFloat(raw, 64); err == nil {
		return n, nil
	}
	return raw, nil
}

// queryLimit bounds a query the way every listing on this console is bounded.
//
// A development database is small, but "small" is not something the console
// gets to assume: a query with no limit against a table someone loaded a
// million rows into would hang the page rather than answer anything.
func queryLimit(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 50
	}
	if n > detailLimit {
		return detailLimit
	}
	return n
}

// firestoreType names the type Firestore stored a value as.
//
// "42" and 42 are different writes, and a flattened cell shows them
// identically — which is how a field that was meant to be a number and went in
// as a string stays invisible until a query returns nothing.
func firestoreType(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case int64, float64:
		return "number"
	case string:
		return "string"
	case time.Time:
		return "timestamp"
	case []byte:
		return "bytes"
	case []any:
		return fmt.Sprintf("array (%d)", len(t))
	case map[string]any:
		return fmt.Sprintf("map (%d)", len(t))
	case *latlng.LatLng:
		return "geopoint"
	case *firestore.DocumentRef:
		return "reference"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// renderValue is a value written out in full.
func renderValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case time.Time:
		return t.Format(time.RFC3339Nano)
	case []byte:
		return fmt.Sprintf("%d bytes", len(t))
	case *firestore.DocumentRef:
		return t.Path
	case []any, map[string]any:
		// Nested structures as JSON rather than Go's %v: a map printed as
		// map[a:1] is not something anyone can compare with what they wrote.
		if encoded, err := json.Marshal(t); err == nil {
			return string(encoded)
		}
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// flatten renders a document's fields for a table cell.
func flatten(data map[string]any) string {
	parts := make([]string, 0, len(data))
	for _, k := range sortedAnyKeys(data) {
		parts = append(parts, k+": "+summarise(data[k]))
	}
	return strings.Join(parts, ", ")
}

// sortedAnyKeys returns a map's keys in order.
func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// entityDetail is one Datastore entity, property by property.
func (p datastoreProvider) entityDetail(ctx context.Context, project, kind, id string) (console.Detail, error) {
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	c, err := datastore.NewClient(ctx, project, localOpts(p.endpoint)...)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Datastore: " + err.Error()}, nil
	}
	defer c.Close()

	// The listing renders a numeric key as "id=123" because that is what
	// distinguishes it from a name. The same convention is read back here, so a
	// row and the page it opens address the same entity.
	key, err := datastoreKey(kind, id)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}

	var props datastore.PropertyList
	if err := c.Get(ctx, key, &props); err != nil {
		return console.Detail{Unavailable: "cannot read the entity: " + err.Error()}, nil
	}

	fields := console.Listing{
		Columns:    []string{"Type", "Indexed", "Value"},
		NameColumn: "Property",
		Noun:       "properties",
	}
	sort.SliceStable(props, func(a, b int) bool { return props[a].Name < props[b].Name })
	for _, prop := range props {
		fields.Items = append(fields.Items, console.Resource{
			Name: prop.Name,
			Fields: map[string]string{
				"Type": fmt.Sprintf("%T", prop.Value),
				// NoIndex inverted, because "indexed" is what a query needs and
				// the double negative is where a reader loses the thread.
				"Indexed": yesNo(!prop.NoIndex),
				"Value":   renderValue(prop.Value),
			},
		})
	}
	fields.Total = len(fields.Items)

	return console.Detail{
		Summary: []console.Property{
			{Label: "Kind", Value: kind},
			{Label: "Key", Value: id},
			{Label: "Namespace", Value: orDash(key.Namespace)},
			{Label: "Properties", Value: fmt.Sprint(fields.Total)},
		},
		Sections: []console.Section{{ID: "properties", Label: "Properties", Listing: fields}},
	}, nil
}

// datastoreKey reads back the key the listing rendered.
func datastoreKey(kind, id string) (*datastore.Key, error) {
	if numeric, ok := strings.CutPrefix(id, "id="); ok {
		n, err := strconv.ParseInt(numeric, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a numeric entity id", numeric)
		}
		return datastore.IDKey(kind, n, nil), nil
	}
	return datastore.NameKey(kind, id, nil), nil
}

// QueryForm is the Datastore query builder.
//
// The kind page listed every entity in insertion order with no way to narrow
// it, so a kind with two hundred entities was a wall and the two-hundred-and-
// first was unreachable.
func (p datastoreProvider) QueryForm(path []string) (string, []console.Field) {
	return "Run query", []console.Field{
		{Name: "property", Label: "Property", Type: "text",
			Help: "The property to filter on. Leave empty to list in order."},
		{Name: "op", Label: "Operator", Type: "text", Default: "=",
			Help:    "One of =, <, <=, >, >=.",
			Pattern: `^(=|<|<=|>|>=)$`},
		{Name: "value", Label: "Value", Type: "text",
			Help: "Compared as a number when it parses as one, as a boolean for " +
				"true and false, and as a string otherwise."},
		{Name: "orderBy", Label: "Order by", Type: "text",
			Help: "Optional property name. Datastore requires an index for an " +
				"ordering it has not been given one for, and says which."},
		{Name: "descending", Label: "Descending", Type: "checkbox"},
		{Name: "limit", Label: "Limit", Type: "text", Default: "50",
			Pattern: `^[0-9]{1,4}$`,
			Help:    fmt.Sprintf("At most %d.", detailLimit)},
	}
}

func (p datastoreProvider) Build(ctx context.Context, project string, path []string, values map[string]string) (console.Listing, error) {
	out := console.Listing{
		Columns: []string{"Properties"}, Noun: "entities", NameColumn: "Key",
		RowsOpenable: true,
	}
	if project == "" {
		out.Prompt = "Choose a project in the toolbar."
		return out, nil
	}
	if len(path) != 1 {
		return console.Listing{}, fmt.Errorf("a Datastore query runs against a kind")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	c, err := datastore.NewClient(ctx, project, localOpts(p.endpoint)...)
	if err != nil {
		out.Unavailable = "cannot reach Datastore: " + err.Error()
		return out, nil
	}
	defer c.Close()

	q := datastore.NewQuery(path[0])
	if prop := strings.TrimSpace(values["property"]); prop != "" {
		op := strings.TrimSpace(values["op"])
		if op == "" {
			op = "="
		}
		value, err := typedValue(values["value"], "==")
		if err != nil {
			return console.Listing{}, err
		}
		q = q.FilterField(prop, op, value)
	}
	if order := strings.TrimSpace(values["orderBy"]); order != "" {
		if values["descending"] == "true" {
			order = "-" + order
		}
		q = q.Order(order)
	}
	q = q.Limit(queryLimit(values["limit"]))

	var entities []datastore.PropertyList
	keys, err := c.GetAll(ctx, q, &entities)
	if err != nil {
		// Datastore's own message, which for a missing index names the index.
		return console.Listing{}, err
	}
	for i, k := range keys {
		var parts []string
		if i < len(entities) {
			props := entities[i]
			sort.SliceStable(props, func(a, b int) bool { return props[a].Name < props[b].Name })
			for _, prop := range props {
				parts = append(parts, prop.Name+": "+summarise(prop.Value))
			}
		}
		id := k.Name
		if id == "" {
			id = fmt.Sprintf("id=%d", k.ID)
		}
		out.Items = append(out.Items, console.Resource{
			Name:   id,
			Fields: map[string]string{"Properties": strings.Join(parts, ", ")},
			Opens:  []string{path[0], id},
		})
	}
	out.Total = len(out.Items)
	return out, nil
}

// rowDetail is one Bigtable row, cell by cell.
func (p bigtableProvider) rowDetail(ctx context.Context, project, table, rowKey string) (console.Detail, error) {
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	c, err := bigtable.NewClient(ctx, project, bigtableInstance, localOpts(p.endpoint)...)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Bigtable: " + err.Error()}, nil
	}
	defer c.Close()

	row, err := c.Open(table).ReadRow(ctx, rowKey)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the row: " + err.Error()}, nil
	}
	if len(row) == 0 {
		return console.Detail{Unavailable: "no row with key " + rowKey + " in " + table}, nil
	}

	cells := console.Listing{
		Columns:    []string{"Family", "Timestamp", "Value"},
		NameColumn: "Column",
		Noun:       "cells",
	}
	// Every version of every cell, not only the latest. Bigtable keeps versions,
	// and a page that showed one per column would be answering a different
	// question than the one a Bigtable user is asking.
	for _, family := range sortedFamilies(row) {
		for _, item := range row[family] {
			cells.Items = append(cells.Items, console.Resource{
				Name: item.Column,
				Fields: map[string]string{
					"Family":    family,
					"Timestamp": item.Timestamp.Time().Format(time.RFC3339Nano),
					"Value":     string(item.Value),
				},
			})
		}
	}
	cells.Total = len(cells.Items)

	return console.Detail{
		Summary: []console.Property{
			{Label: "Table", Value: table},
			{Label: "Row key", Value: rowKey},
			{Label: "Column families", Value: fmt.Sprint(len(row))},
			{Label: "Cells", Value: fmt.Sprint(cells.Total)},
		},
		Sections: []console.Section{{ID: "cells", Label: "Cells", Listing: cells}},
	}, nil
}

func sortedFamilies(row bigtable.Row) []string {
	out := make([]string, 0, len(row))
	for family := range row {
		out = append(out, family)
	}
	sort.Strings(out)
	return out
}

// QueryForm is the Bigtable row-range reader.
//
// Bigtable has one query: a row-key range, optionally narrowed to a column
// family. That is the whole surface, and reading rows by prefix is the operation
// a Bigtable user performs constantly and could not perform here at all.
func (p bigtableProvider) QueryForm(path []string) (string, []console.Field) {
	return "Read rows", []console.Field{
		{Name: "prefix", Label: "Row key prefix", Type: "text",
			Help: "Reads every row whose key starts with this. Leave empty with a " +
				"start and end to read an explicit range instead."},
		{Name: "start", Label: "Start key", Type: "text",
			Help: "Inclusive. Ignored when a prefix is given."},
		{Name: "end", Label: "End key", Type: "text",
			Help: "Exclusive. Ignored when a prefix is given."},
		{Name: "family", Label: "Column family", Type: "text",
			Help: "Optional. Restricts the cells read, not the rows returned."},
		{Name: "limit", Label: "Limit", Type: "text", Default: "50",
			Pattern: `^[0-9]{1,4}$`,
			Help:    fmt.Sprintf("At most %d.", detailLimit)},
	}
}

func (p bigtableProvider) Build(ctx context.Context, project string, path []string, values map[string]string) (console.Listing, error) {
	out := console.Listing{
		Columns: []string{"Cells"}, Noun: "rows", NameColumn: "Row key",
		RowsOpenable: true,
	}
	if project == "" {
		out.Prompt = "Choose a project in the toolbar."
		return out, nil
	}
	if len(path) != 1 {
		return console.Listing{}, fmt.Errorf("a Bigtable read runs against a table")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	c, err := bigtable.NewClient(ctx, project, bigtableInstance, localOpts(p.endpoint)...)
	if err != nil {
		out.Unavailable = "cannot reach Bigtable: " + err.Error()
		return out, nil
	}
	defer c.Close()

	// A prefix wins over a range, because giving both is ambiguous and silently
	// honouring one would make the other look broken.
	var rowSet bigtable.RowSet
	switch prefix := strings.TrimSpace(values["prefix"]); {
	case prefix != "":
		rowSet = bigtable.PrefixRange(prefix)
	default:
		start, end := strings.TrimSpace(values["start"]), strings.TrimSpace(values["end"])
		if start == "" && end == "" {
			rowSet = bigtable.InfiniteRange("")
		} else {
			rowSet = bigtable.NewRange(start, end)
		}
	}

	var opts []bigtable.ReadOption
	limit := queryLimit(values["limit"])
	opts = append(opts, bigtable.LimitRows(int64(limit)))
	if family := strings.TrimSpace(values["family"]); family != "" {
		opts = append(opts, bigtable.RowFilter(bigtable.FamilyFilter(family)))
	}

	err = c.Open(path[0]).ReadRows(ctx, rowSet, func(row bigtable.Row) bool {
		var parts []string
		for _, family := range sortedFamilies(row) {
			for _, item := range row[family] {
				parts = append(parts, item.Column+": "+summarise(string(item.Value)))
			}
		}
		out.Items = append(out.Items, console.Resource{
			Name:   row.Key(),
			Fields: map[string]string{"Cells": strings.Join(parts, ", ")},
			Opens:  []string{path[0], row.Key()},
		})
		return len(out.Items) < limit
	}, opts...)
	if err != nil {
		return console.Listing{}, err
	}
	out.Total = len(out.Items)
	return out, nil
}
