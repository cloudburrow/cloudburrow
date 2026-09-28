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
	"cloud.google.com/go/datastore/apiv1/datastorepb"
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The four opt-in databases had working backends and no screens: they were
// reachable by an SDK and invisible in the console, which is the kind of gap a
// developer reads as "not implemented".
//
// Each walks its product's own hierarchy down to the level that answers "did my
// application write what I expected": a Firestore document's fields, a Datastore
// entity's properties, a Bigtable row's cells, a Spanner table's columns and
// indexes. Firestore, Datastore and Bigtable get a query form rather than a
// statement box, because none of them has a query language a console could
// offer and inventing a syntax would be worse than offering nothing.
//
// Bigtable tables and Spanner databases and instances are created and dropped
// through their admin APIs. Firestore collections and Datastore kinds are not
// first-class — a collection exists because a document is in it — so what is
// created and deleted is a document or an entity (consoledocedit.go, #796):
// "Start collection" and "Create entity" on the list make the first one.
// Every read goes through a client constructed with the selected project, so a
// screen cannot show or touch another project's data: the scoping is in the
// client rather than in a filter applied afterwards.

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
		// Counting is a read of every document name, so it is bounded and
		// the column says when it stopped counting rather than reporting a
		// total it did not establish.
		items = append(items, console.Resource{
			Name:   col.ID,
			Fields: map[string]string{"Documents": countDocuments(ctx, col)},
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	base.Items, base.Total = items, len(items)
	base.Note = firestoreCountNote
	return base, nil
}

// firestoreCountNote is the collections list's note on its Documents column.
const firestoreCountNote = "Document counts stop at 100; a collection with more shows a trailing plus. " +
	"A count is of the documents the collection's page lists, so it includes a document that does not " +
	"exist but has subcollections, which that page shows in italics. " +
	"A collection exists because a document is in it: Start collection adds its first document, " +
	"and it goes when its last document is deleted."

const documentCountCap = 100

// countDocuments is a collection's Documents cell: how many documents its
// page lists, up to documentCountCap, then "100+".
//
// It counts what ListDocuments with show_missing returns — the client's
// DocumentRefs, which is how the collection's page is read (documentsPage) —
// rather than a query's results, so a document that does not exist but has
// subcollections is counted as it is listed (#882): a count that left it out
// disagreed with the page it summarises. A read that fails is "—", not a
// count of what was read before it failed.
func countDocuments(ctx context.Context, col *firestore.CollectionRef) string {
	it := col.DocumentRefs(ctx)
	n := 0
	for {
		_, err := it.Next()
		if err == iterator.Done {
			return strconv.Itoa(n)
		}
		if err != nil {
			return "—"
		}
		n++
		if n > documentCountCap {
			return strconv.Itoa(documentCountCap) + "+"
		}
	}
}

// --- Datastore ------------------------------------------------------------

type datastoreProvider struct{ endpoint string }

func (datastoreProvider) ID() string    { return "datastore" }
func (datastoreProvider) Title() string { return "Datastore" }

func (p datastoreProvider) List(ctx context.Context, project string) (console.Listing, error) {
	// Every namespace's kinds (#854): a kind in a namespace other than the
	// default is a different kind, and a screen that listed only the default
	// namespace hid it. The Namespace column is the picker: its filter narrows
	// the list to one namespace, and a row opens the kind in its own.
	base := console.Listing{Columns: []string{"Namespace"}, Noun: "kinds", NameColumn: "Kind"}
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

	namespaces, err := datastoreNamespaces(ctx, c)
	if err != nil {
		base.Unavailable = "listing namespaces: " + err.Error()
		return base, nil
	}
	var items []console.Resource
	for _, ns := range append([]string{""}, namespaces...) {
		kinds, err := datastoreKinds(ctx, c, ns)
		if err != nil {
			base.Unavailable = "listing kinds: " + err.Error()
			return base, nil
		}
		for _, kind := range kinds {
			item := console.Resource{Name: kind, Fields: map[string]string{"Namespace": namespaceLabel(ns)}}
			if ns != "" {
				item.Opens = datastoreScope{ns: ns, namespaced: true}.at(kind)
			}
			items = append(items, item)
		}
	}
	base.Items, base.Total = items, len(items)
	base.Note = "A kind exists because an entity has it: Create entity makes its first entity, " +
		"and it goes when its last entity is deleted. Kinds in every namespace are listed, the default first; " +
		"filter on a namespace's name to see only its kinds."
	return base, nil
}

// datastoreNamespaces are the project's namespaces other than the default,
// sorted, from Datastore's own __namespace__ metadata query.
func datastoreNamespaces(ctx context.Context, c *datastore.Client) ([]string, error) {
	keys, err := c.GetAll(ctx, datastore.NewQuery("__namespace__").KeysOnly(), nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, k := range keys {
		// The default namespace is the key with ID 1 and no name.
		if k.Name != "" {
			out = append(out, k.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// datastoreKinds are a namespace's kinds, sorted, from __kind__. The list of
// kinds is a query rather than an API call, which is why this looks unlike
// the other screens.
func datastoreKinds(ctx context.Context, c *datastore.Client, ns string) ([]string, error) {
	keys, err := c.GetAll(ctx, datastore.NewQuery("__kind__").Namespace(ns).KeysOnly(), nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, k := range keys {
		if strings.HasPrefix(k.Name, "__") {
			continue // Datastore's own metadata kinds
		}
		out = append(out, k.Name)
	}
	sort.Strings(out)
	return out, nil
}

func namespaceLabel(ns string) string {
	if ns == "" {
		return "(default)"
	}
	return ns
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
	// Instances, not databases.
	//
	// Spanner's hierarchy is project → instance → database → table, and this
	// screen used to flatten the first two: it listed databases with the
	// instance as a column, so two databases named "main" under different
	// instances were two rows called "main". Worse, opening one had to search
	// every instance for a database with that name and took whichever it found
	// first — so with a duplicate, the console opened an arbitrary one.
	base := console.Listing{
		Columns: []string{"Databases", "Nodes", "Configuration"},
		Noun:    "instances", NameColumn: "Instance",
		AlwaysStatus: true,
		RowsOpenable: true,
	}
	if project == "" {
		base.Prompt = "Spanner holds instances per project. Choose one in the toolbar."
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
		// The database count, because "an instance exists" and "an instance
		// holds something" are different facts and only the second is useful.
		count := "—"
		if n, err := p.countDatabases(ctx, dbAdmin, inst.GetName()); err == nil {
			count = fmt.Sprint(n)
		}
		items = append(items, console.Resource{
			Name:   lastSegment(inst.GetName()),
			Status: inst.GetState().String(),
			Fields: map[string]string{
				"Databases":     count,
				"Nodes":         fmt.Sprint(inst.GetNodeCount()),
				"Configuration": lastSegment(inst.GetConfig()),
			},
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	base.Items, base.Total = items, len(items)
	base.Note = "In-memory: everything here is gone when the instance restarts."
	return base, nil
}

func (p spannerProvider) countDatabases(ctx context.Context, dbAdmin *database.DatabaseAdminClient, instanceName string) (int, error) {
	dbs := dbAdmin.ListDatabases(ctx, &databasepb.ListDatabasesRequest{Parent: instanceName})
	n := 0
	for {
		if _, err := dbs.Next(); err == iterator.Done {
			return n, nil
		} else if err != nil {
			return 0, err
		}
		n++
	}
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
	// Three levels: a collection, one of its documents, and one of its fields.
	// Anything deeper is refused rather than silently collapsed onto the same
	// page.
	//
	// A subcollection (#854) is addressed by its whole path in the first
	// segment — users/alice/orders — so a document in it is
	// [users/alice/orders, o1] and a field of that document one more, and the
	// three levels hold at any depth. A Firestore ID has no slash, so a first
	// segment with one is always a collection path and never an ID.
	var (
		d   console.Detail
		err error
	)
	switch {
	case len(path) == 2:
		d, err = p.documentDetail(ctx, project, path[0], path[1])
	case len(path) == 3:
		d, err = p.fieldDetail(ctx, project, path[0], path[1], path[2])
	case len(path) > 3:
		return console.DeeperThan(3, path), nil
	default:
		d, err = p.collectionDetail(ctx, project, path[0])
	}
	if err == nil && d.Unavailable == "" && d.Prompt == "" {
		d.Trail = firestoreTrail(path[0], path[1:]...)
	}
	return d, err
}

func (p firestoreProvider) collectionDetail(ctx context.Context, project, name string) (console.Detail, error) {
	list, err := p.contents(ctx, project, name)
	if err != nil {
		return console.Detail{}, err
	}
	// The properties come from the same List the screen above was built from,
	// so the detail page cannot disagree with the row that led to it. A
	// subcollection is on no list screen; its page names its parent.
	var summary []console.Property
	if i := strings.LastIndex(name, "/"); i >= 0 {
		summary = []console.Property{{Label: "Parent document", Value: name[:i]}}
	} else if list.Prompt == "" {
		if parent, err := p.List(ctx, project); err == nil {
			summary = summariseFrom(parent, name)
		}
	}
	return singleSection("documents", "Documents", list, summary), nil
}

func (p firestoreProvider) contents(ctx context.Context, project, name string) (console.Listing, error) {
	return p.documentsPage(ctx, project, name, "")
}

// documentsPage reads one page of a collection's documents.
//
// The page is ListDocuments with show_missing, through the client's
// DocumentRefs, rather than a query: a query returns only documents that
// exist, and a document that does not exist but has subcollections was left
// off the list, so its subcollections could be reached only by typing their
// path (#875). Google's console lists such a document in italics, and so
// does this one. The cursor is ListDocuments' own page token, which is the
// API's mechanism for exactly this; the documents come back in name order.
//
// DocumentRefs reads names only, so the documents that exist are then read
// in one batch for their fields.
func (p firestoreProvider) documentsPage(ctx context.Context, project, name, after string) (console.Listing, error) {
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

	col, err := firestoreCollection(c, name)
	if err != nil {
		out.Unavailable = err.Error()
		return out, nil
	}
	var refs []*firestore.DocumentRef
	next, err := iterator.NewPager(col.DocumentRefs(ctx), detailLimit, after).NextPage(&refs)
	if err != nil {
		out.Unavailable = "reading documents: " + err.Error()
		return out, nil
	}
	var snaps []*firestore.DocumentSnapshot
	if len(refs) > 0 {
		if snaps, err = c.GetAll(ctx, refs); err != nil {
			out.Unavailable = "reading documents: " + err.Error()
			return out, nil
		}
	}
	items := make([]console.Resource, 0, len(snaps))
	for _, snap := range snaps {
		if !snap.Exists() {
			// Listed by show_missing and not there when read: a document
			// that does not exist but has subcollections.
			items = append(items, console.Resource{
				Name: snap.Ref.ID, Absent: true,
				Fields: map[string]string{"Fields": firestoreMissingNote},
			})
			continue
		}
		// The field names and values, not a document count: the point of
		// opening a document is to see what is in it.
		items = append(items, console.Resource{
			Name:   snap.Ref.ID,
			Fields: map[string]string{"Fields": flatten(snap.Data())},
		})
	}
	// A page token means the server may have more. It can be issued for a
	// page that happened to end the collection, so the next page can be
	// empty — which is the honest answer to "was that the end".
	if next != "" {
		out.More = true
		out.Cursor = next
	}
	out.Items, out.Total = items, len(items)
	for _, it := range items {
		if it.Absent {
			out.Note = "A document in italics does not exist: it has no fields, and is listed because it has " +
				"subcollections, which open from its page."
			break
		}
	}
	return out, nil
}

// firestoreMissingNote is the Fields cell of a document that does not exist
// but has subcollections.
const firestoreMissingNote = "no fields — has subcollections"

// Page implements console.Pager for a collection's documents.
func (p firestoreProvider) Page(ctx context.Context, project string, path []string, cursor string) (console.Listing, error) {
	if len(path) != 1 {
		return console.Listing{}, fmt.Errorf("only a collection's document list can be paged")
	}
	return p.documentsPage(ctx, project, path[0], cursor)
}

// Detail lists the entities of a Datastore kind.
// Detail implements console.Driller for a Datastore kind.
func (p datastoreProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	// Four levels: a kind, one of its entities, one of its properties, and a
	// value inside an array or embedded entity property (#905). Anything
	// deeper is refused rather than silently collapsed onto the same
	// page. A namespace other than the default comes first, as two segments
	// (datastoreScope), and a child entity is addressed by its whole key path
	// (datastoreEntityKey), so the three levels hold in every namespace and
	// at every depth of ancestry (#854).
	scope, rest := parseDatastorePath(path)
	var (
		d   console.Detail
		err error
	)
	switch {
	case scope.index:
		d, err = p.namespacesDetail(ctx, project)
	case len(rest) == 0:
		d, err = p.namespaceDetail(ctx, project, scope)
	case len(rest) == 1:
		d, err = p.kindDetail(ctx, project, scope, rest[0])
	case len(rest) == 2:
		d, err = p.entityDetail(ctx, project, scope, rest[0], rest[1])
	case len(rest) == 3:
		d, err = p.propertyDetail(ctx, project, scope, rest[0], rest[1], rest[2])
	case len(rest) == 4:
		// A value inside an array or embedded entity property (#905).
		d, err = p.elementDetail(ctx, project, scope, rest[0], rest[1], rest[2], rest[3])
	default:
		return console.DeeperThan(len(path)-len(rest)+4, path), nil
	}
	if err == nil && d.Unavailable == "" && d.Prompt == "" {
		d.Trail = scope.trail(rest...)
		// An entity's page is addressed by its encoded key (#875); its
		// heading and crumb name it by its Name/ID, or a child by its key
		// path in the same rendering (datastoreEntityHeading, #885).
		if len(rest) >= 2 {
			labels := append([]string{}, rest...)
			labels[1] = datastoreEntityLabel(project, scope.ns, rest[0], rest[1])
			// A value inside a property is addressed by its steps as JSON,
			// and named as its row names it (#905).
			if len(rest) == 4 {
				if steps, err := parseDatastoreElement(rest[3]); err == nil {
					labels[3] = datastoreElementLabel(rest[2], steps)
					d.Title = labels[3]
				}
			}
			if labels[1] != rest[1] || len(rest) == 4 {
				d.Trail = scope.labelledTrail(rest, labels)
				if len(rest) == 2 {
					d.Title = labels[1]
				}
			}
		}
	}
	return d, err
}

func (p datastoreProvider) kindDetail(ctx context.Context, project string, scope datastoreScope, kind string) (console.Detail, error) {
	list, err := p.entitiesPage(ctx, project, scope, kind, "")
	if err != nil {
		return console.Detail{}, err
	}
	summary := []console.Property{{Label: "Namespace", Value: namespaceLabel(scope.ns)}}
	return singleSection("entities", "Entities", list, summary), nil
}

// namespacesDetail lists the namespaces other than the default, whose kinds
// are the Datastore screen itself.
func (p datastoreProvider) namespacesDetail(ctx context.Context, project string) (console.Detail, error) {
	list := console.Listing{Columns: []string{"Kinds"}, NameColumn: "Namespace", Noun: "namespaces", RowsOpenable: true}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer c.Close()
	namespaces, err := datastoreNamespaces(ctx, c)
	if err != nil {
		return console.Detail{Unavailable: "listing namespaces: " + err.Error()}, nil
	}
	for _, ns := range namespaces {
		kinds, err := datastoreKinds(ctx, c, ns)
		shown := "—"
		if err == nil {
			shown = fmt.Sprint(len(kinds))
		}
		list.Items = append(list.Items, console.Resource{Name: ns, Fields: map[string]string{"Kinds": shown}})
	}
	list.Total = len(list.Items)
	list.Note = "The default namespace's kinds are the Datastore screen itself. A namespace exists because " +
		"an entity is in it: Create entity with a namespace makes its first."
	return singleSection("namespaces", "Namespaces", list, nil), nil
}

// namespaceDetail lists one namespace's kinds.
func (p datastoreProvider) namespaceDetail(ctx context.Context, project string, scope datastoreScope) (console.Detail, error) {
	list := console.Listing{Columns: []string{}, NameColumn: "Kind", Noun: "kinds", RowsOpenable: true}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer c.Close()
	kinds, err := datastoreKinds(ctx, c, scope.ns)
	if err != nil {
		return console.Detail{Unavailable: "listing kinds: " + err.Error()}, nil
	}
	for _, k := range kinds {
		list.Items = append(list.Items, console.Resource{Name: k})
	}
	list.Total = len(list.Items)
	if list.Total == 0 {
		list.Note = "Namespace " + scope.ns + " holds no entity. Create entity makes its first."
	}
	return singleSection("kinds", "Kinds", list,
		[]console.Property{{Label: "Namespace", Value: scope.ns}, {Label: "Kinds", Value: fmt.Sprint(list.Total)}}), nil
}

// entitiesPage reads one page of a kind's entities.
//
// The cursor is Datastore's own query cursor, which is what Datastore gives for
// exactly this and is stable across pages in a way an offset is not.
func (p datastoreProvider) entitiesPage(ctx context.Context, project string, scope datastoreScope, name, after string) (console.Listing, error) {
	out := console.Listing{
		Columns: datastoreEntityColumns, Noun: "entities", NameColumn: datastoreNameIDColumn,
		// An entity's properties were one truncated cell. Each entity now opens.
		RowsOpenable: true,
	}
	if project == "" {
		out.Prompt = "Choose a project in the toolbar."
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	// Through the v1 API's RunQuery, as the entity's own page reads it
	// (#904): the Go client's Key drops a key value's project and database,
	// so a key to another project's Order 7 read as this project's.
	c, done, err := p.rawDatastore()
	if err != nil {
		out.Unavailable = err.Error()
		return out, nil
	}
	defer done()

	var start []byte
	if after != "" {
		// The client's own cursor encoding, so a link issued before #904
		// still pages on.
		if start, err = decodeDatastoreCursor(after); err != nil {
			return console.Listing{}, fmt.Errorf("not a cursor this screen issued: %w", err)
		}
	}
	res, err := runEntityQuery(ctx, c, project, scope.ns,
		&datastorepb.Query{Kind: []*datastorepb.KindExpression{{Name: name}}}, detailLimit, start)
	if err != nil {
		out.Unavailable = "reading entities: " + err.Error()
		return out, nil
	}
	var items []console.Resource
	for _, e := range res.Entities {
		// An entity's page is addressed by its encoded key, which tells
		// every entity from every other (#875); its row names it by Name/ID
		// and its parent, which tell the rows apart too (#882, #885).
		k, props := datastoreEntityProperties(e, project)
		items = append(items, datastoreEntityRow(scope, name, k, datastorePropertiesCell(props)))
	}
	out.Items, out.Total = items, len(items)
	// A full page means there may be more. Datastore has no cheap way to know
	// without reading one further, and its cursor is valid whether or not
	// anything follows it — so a full page offers the cursor and an empty next
	// page is the honest answer to "was that the end".
	if len(items) == detailLimit && len(res.Cursor) > 0 {
		out.More = true
		out.Cursor = encodeDatastoreCursor(res.Cursor)
	}
	return out, nil
}

// datastoreEntityColumns are a kind's entity listing's columns, the kind
// page's and the query builder's alike.
var datastoreEntityColumns = []string{"Parent", "Properties"}

// datastoreRootParent is the Parent cell of a root entity.
const datastoreRootParent = "none (root entity)"

// datastoreEntityRow is one entity's row in a kind's listing: named in the
// Name/ID column as Google's console names it (datastoreNameID), opening its
// page by its encoded key, with its parent's key path in the Parent column,
// or datastoreRootParent.
//
// A Key cell holding a root's name, or a child's key path, could not tell a
// root entity named like a key path — Order "Customer/alice/Order/x" — from
// the child that path names (#882), nor a root named "id=7" from the numeric
// ID 7 (#885). As in Google's console, the Name/ID cell says which it is —
// name=id=7, id=7 — and the Parent column carries the ancestry, so every two
// entities' rows differ where they are listed and not only on their pages.
func datastoreEntityRow(scope datastoreScope, kind string, k *datastore.Key, props []string) console.Resource {
	parent := datastoreRootParent
	if k.Parent != nil {
		parent = datastoreKeyPathLabel(k.Parent)
	}
	return console.Resource{
		Name:   datastoreNameID(k),
		Fields: map[string]string{"Parent": parent, "Properties": strings.Join(props, ", ")},
		Opens:  scope.at(kind, datastoreEntityAddress(k)),
	}
}

// Page implements console.Pager for a kind's entities.
func (p datastoreProvider) Page(ctx context.Context, project string, path []string, cursor string) (console.Listing, error) {
	scope, rest := parseDatastorePath(path)
	if scope.index || len(rest) != 1 {
		return console.Listing{}, fmt.Errorf("only a kind's entity list can be paged")
	}
	return p.entitiesPage(ctx, project, scope, rest[0], cursor)
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
			// Edit GC policy and Delete column family (#797), addressed apart
			// from the rows beside them: a row key can be a family's name.
			Actions: familyActions(f),
			ActsOn:  familyTarget(table, f.Name),
		})
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	if out.Total == 0 && out.Unavailable == "" {
		out.Note = "This table has no column families, so nothing can be written " +
			"to it: a Bigtable write names the family it goes into. Add column family creates one."
	} else if out.Unavailable == "" {
		out.Note = "A GC policy set through the API that this form cannot express — nested, " +
			"or an age that is not a whole number of minutes — is shown and offered no edit."
	}
	sec.Listing = out
	return sec
}

func (p bigtableProvider) contents(ctx context.Context, project, name string) (console.Listing, error) {
	return p.rowsPage(ctx, project, name, "")
}

// rowsPage reads one page of a table's rows.
//
// The cursor is the last row key on the previous page, and the next read starts
// just after it. Bigtable's row ranges are half-open on the start, so resuming is
// exactly what InfiniteRange over the successor key does — no extra state and
// nothing to invalidate.
func (p bigtableProvider) rowsPage(ctx context.Context, project, name, after string) (console.Listing, error) {
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

	// The zero byte appended is the smallest key greater than `after`, so the
	// range starts at the first row strictly after the last one shown.
	start := ""
	if after != "" {
		start = after + "\x00"
	}

	var items []console.Resource
	err = c.Open(name).ReadRows(ctx, bigtable.InfiniteRange(start), func(row bigtable.Row) bool {
		var parts []string
		for _, family := range sortedFamilies(row) {
			for _, item := range row[family] {
				parts = append(parts, item.Column+": "+summarise(string(item.Value)))
			}
		}
		items = append(items, console.Resource{
			Name:   row.Key(),
			Fields: map[string]string{"Cells": strings.Join(parts, ", ")},
		})
		return len(items) <= detailLimit
	})
	if err != nil {
		out.Unavailable = "reading rows: " + err.Error()
		return out, nil
	}
	if len(items) > detailLimit {
		items = items[:detailLimit]
		out.More = true
		out.Cursor = items[len(items)-1].Name
	}
	out.Items, out.Total = items, len(items)
	return out, nil
}

// Page implements console.Pager for a table's rows.
func (p bigtableProvider) Page(ctx context.Context, project string, path []string, cursor string) (console.Listing, error) {
	if len(path) != 1 {
		return console.Listing{}, fmt.Errorf("only a table's row list can be paged")
	}
	return p.rowsPage(ctx, project, path[0], cursor)
}

// Detail implements console.Driller for the Spanner hierarchy.
//
// Three levels, matching Spanner's own: an instance holds databases, a database
// holds tables, a table has columns and indexes. The path carries the instance,
// so nothing has to search every instance for a database by name — which is what
// the previous flat listing forced, and which picked an arbitrary one when two
// instances each held a database of the same name.
func (p spannerProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	switch len(path) {
	case 1:
		return p.instanceDetail(ctx, project, path[0])
	case 2:
		return p.databaseDetail(ctx, project, path[0], path[1])
	case 3:
		return p.spannerTableDetail(ctx, project, path[0], path[1], path[2])
	}
	return console.DeeperThan(3, path), nil
}

// instanceDetail lists an instance's databases.
func (p spannerProvider) instanceDetail(ctx context.Context, project, instanceID string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Spanner: " + err.Error()}, nil
	}
	defer instAdmin.Close()

	name := fmt.Sprintf("projects/%s/instances/%s", project, instanceID)
	inst, err := instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the instance: " + err.Error()}, nil
	}

	dbAdmin, err := database.NewDatabaseAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Spanner: " + err.Error()}, nil
	}
	defer dbAdmin.Close()

	databases := console.Listing{
		Columns: []string{"Dialect", "Created"}, NameColumn: "Database", Noun: "databases",
		AlwaysStatus: true,
		RowsOpenable: true,
	}
	dbs := dbAdmin.ListDatabases(ctx, &databasepb.ListDatabasesRequest{Parent: name})
	for {
		db, err := dbs.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			databases.Unavailable = "listing databases: " + err.Error()
			break
		}
		created := "—"
		if t := db.GetCreateTime(); t != nil {
			created = t.AsTime().Format(time.RFC3339)
		}
		databases.Items = append(databases.Items, console.Resource{
			Name:   lastSegment(db.GetName()),
			Status: db.GetState().String(),
			Fields: map[string]string{
				"Dialect": db.GetDatabaseDialect().String(),
				"Created": created,
			},
		})
	}
	sort.SliceStable(databases.Items, func(i, j int) bool {
		return databases.Items[i].Name < databases.Items[j].Name
	})
	databases.Total = len(databases.Items)

	return console.Detail{
		Summary: []console.Property{
			{Label: "State", Value: inst.GetState().String()},
			{Label: "Databases", Value: fmt.Sprint(databases.Total)},
			{Label: "Nodes", Value: fmt.Sprint(inst.GetNodeCount())},
			{Label: "Configuration", Value: lastSegment(inst.GetConfig())},
		},
		Sections: []console.Section{
			{ID: "databases", Label: "Databases", Listing: databases},
			{
				ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{
					Heading: "Instance",
					Properties: []console.Property{
						{Label: "Resource name", Value: inst.GetName()},
						{Label: "Display name", Value: inst.GetDisplayName()},
						{Label: "Configuration", Value: inst.GetConfig()},
						{Label: "Node count", Value: fmt.Sprint(inst.GetNodeCount())},
						{Label: "Processing units", Value: fmt.Sprint(inst.GetProcessingUnits())},
						{Label: "State", Value: inst.GetState().String()},
					},
				}},
				Note: "Read-only. The emulator accepts an instance's node count and " +
					"does nothing with it: there is one process serving everything, " +
					"so capacity is not a setting that has an effect here.",
			},
		},
	}, nil
}

// databaseDetail is a database's tables, its DDL and its query editor.
func (p spannerProvider) databaseDetail(ctx context.Context, project, instanceID, dbID string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	dbName := fmt.Sprintf("projects/%s/instances/%s/databases/%s", project, instanceID, dbID)

	dbAdmin, err := database.NewDatabaseAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Spanner: " + err.Error()}, nil
	}
	defer dbAdmin.Close()

	db, err := dbAdmin.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: dbName})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the database: " + err.Error()}, nil
	}

	tables := console.Listing{
		Columns: []string{"Columns", "Indexes", "Parent"}, NameColumn: "Table", Noun: "tables",
		RowsOpenable: true,
	}
	c, err := spanner.NewClient(ctx, dbName, localOpts(p.endpoint)...)
	if err != nil {
		tables.Unavailable = "cannot open the database: " + err.Error()
	} else {
		defer c.Close()
		// INFORMATION_SCHEMA is Spanner's own catalogue, so this asks the
		// database what it holds rather than keeping a second record of it.
		// PARENT_TABLE_NAME is here because an interleaved table's parent is
		// part of its identity, not a detail.
		stmt := spanner.Statement{SQL: `
			SELECT t.TABLE_NAME,
			       (SELECT COUNT(1) FROM INFORMATION_SCHEMA.COLUMNS c
			          WHERE c.TABLE_NAME = t.TABLE_NAME AND c.TABLE_SCHEMA = t.TABLE_SCHEMA),
			       (SELECT COUNT(1) FROM INFORMATION_SCHEMA.INDEXES i
			          WHERE i.TABLE_NAME = t.TABLE_NAME AND i.TABLE_SCHEMA = t.TABLE_SCHEMA),
			       IFNULL(t.PARENT_TABLE_NAME, '')
			FROM INFORMATION_SCHEMA.TABLES t
			WHERE t.TABLE_SCHEMA = ''
			ORDER BY t.TABLE_NAME`}
		iter := c.Single().Query(ctx, stmt)
		defer iter.Stop()
		for {
			row, err := iter.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				tables.Unavailable = "reading the schema: " + err.Error()
				break
			}
			var table, parent string
			var columns, indexes int64
			if err := row.Columns(&table, &columns, &indexes, &parent); err != nil {
				tables.Unavailable = "decoding the schema: " + err.Error()
				break
			}
			tables.Items = append(tables.Items, console.Resource{
				Name: table,
				Fields: map[string]string{
					"Columns": fmt.Sprint(columns),
					"Indexes": fmt.Sprint(indexes),
					"Parent":  orDash(parent),
				},
			})
		}
		tables.Total = len(tables.Items)
	}

	// The whole schema as Spanner states it. A table list says what exists; the
	// DDL says how it was defined — the constraints, the interleaving and the
	// key order that a per-table view has to reassemble from the catalogue.
	ddl := console.Section{ID: "ddl", Label: "DDL", Kind: console.KindText}
	if statements, err := dbAdmin.GetDatabaseDdl(ctx,
		&databasepb.GetDatabaseDdlRequest{Database: dbName}); err != nil {
		ddl.Unavailable = "cannot read the schema: " + err.Error()
	} else if len(statements.GetStatements()) == 0 {
		ddl.Note = "This database has no schema yet."
	} else {
		ddl.Text = strings.Join(statements.GetStatements(), ";\n\n") + ";"
	}

	created := "—"
	if t := db.GetCreateTime(); t != nil {
		created = t.AsTime().Format(time.RFC3339)
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "State", Value: db.GetState().String()},
			{Label: "Instance", Value: instanceID},
			{Label: "Tables", Value: fmt.Sprint(tables.Total)},
			{Label: "Dialect", Value: db.GetDatabaseDialect().String()},
			{Label: "Created", Value: created},
		},
		Sections: []console.Section{
			{ID: "tables", Label: "Tables", Listing: tables},
			ddl,
			{
				ID: "properties", Label: "Properties", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{
					Heading: "Database",
					Properties: []console.Property{
						{Label: "Resource name", Value: db.GetName()},
						{Label: "State", Value: db.GetState().String()},
						{Label: "Dialect", Value: db.GetDatabaseDialect().String()},
						{Label: "Version retention", Value: orDash(db.GetVersionRetentionPeriod())},
						{Label: "Created", Value: created},
					},
				}},
			},
		},
	}, nil
}

// spannerTableDetail is one table's columns and indexes.
func (p spannerProvider) spannerTableDetail(ctx context.Context, project, instanceID, dbID, table string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	dbName := fmt.Sprintf("projects/%s/instances/%s/databases/%s", project, instanceID, dbID)
	c, err := spanner.NewClient(ctx, dbName, localOpts(p.endpoint)...)
	if err != nil {
		return console.Detail{Unavailable: "cannot open the database: " + err.Error()}, nil
	}
	defer c.Close()

	columns := console.Listing{
		Columns:    []string{"Type", "Nullable", "Key", "Generated"},
		NameColumn: "Column",
		Noun:       "columns",
	}
	// The table name is a parameter, never interpolated: it arrives from a URL
	// segment, and a name pasted into SQL is an injection point whatever the
	// catalogue is.
	colStmt := spanner.Statement{
		SQL: `SELECT c.COLUMN_NAME, c.SPANNER_TYPE, c.IS_NULLABLE,
		             IFNULL(k.ORDINAL_POSITION, 0), IFNULL(c.IS_GENERATED, 'NEVER')
		      FROM INFORMATION_SCHEMA.COLUMNS c
		      LEFT JOIN INFORMATION_SCHEMA.INDEX_COLUMNS k
		        ON k.TABLE_NAME = c.TABLE_NAME AND k.COLUMN_NAME = c.COLUMN_NAME
		           AND k.INDEX_NAME = 'PRIMARY_KEY'
		      WHERE c.TABLE_SCHEMA = '' AND c.TABLE_NAME = @table
		      ORDER BY c.ORDINAL_POSITION`,
		Params: map[string]any{"table": table},
	}
	iter := c.Single().Query(ctx, colStmt)
	for {
		row, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			columns.Unavailable = "reading columns: " + err.Error()
			break
		}
		var name, spannerType, nullable, generated string
		var keyPosition int64
		if err := row.Columns(&name, &spannerType, &nullable, &keyPosition, &generated); err != nil {
			columns.Unavailable = "decoding columns: " + err.Error()
			break
		}
		key := "—"
		if keyPosition > 0 {
			// The position matters: a composite key's order decides how rows
			// are distributed, and "part of the key" alone does not say it.
			key = fmt.Sprintf("PK %d", keyPosition)
		}
		columns.Items = append(columns.Items, console.Resource{
			Name: name,
			Fields: map[string]string{
				"Type": spannerType, "Nullable": nullable,
				"Key": key, "Generated": generated,
			},
		})
	}
	iter.Stop()
	columns.Total = len(columns.Items)

	indexes := console.Listing{
		Columns:    []string{"Type", "Unique", "State", "Columns"},
		NameColumn: "Index",
		Noun:       "indexes",
	}
	idxStmt := spanner.Statement{
		SQL: `SELECT i.INDEX_NAME, i.INDEX_TYPE, i.IS_UNIQUE, i.INDEX_STATE,
		             STRING_AGG(ic.COLUMN_NAME, ', ' ORDER BY ic.ORDINAL_POSITION)
		      FROM INFORMATION_SCHEMA.INDEXES i
		      LEFT JOIN INFORMATION_SCHEMA.INDEX_COLUMNS ic
		        ON ic.TABLE_NAME = i.TABLE_NAME AND ic.INDEX_NAME = i.INDEX_NAME
		      WHERE i.TABLE_SCHEMA = '' AND i.TABLE_NAME = @table
		      GROUP BY i.INDEX_NAME, i.INDEX_TYPE, i.IS_UNIQUE, i.INDEX_STATE
		      ORDER BY i.INDEX_NAME`,
		Params: map[string]any{"table": table},
	}
	idx := c.Single().Query(ctx, idxStmt)
	for {
		row, err := idx.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			indexes.Unavailable = "reading indexes: " + err.Error()
			break
		}
		var name, indexType, cols string
		var state spanner.NullString
		var unique bool
		if err := row.Columns(&name, &indexType, &unique, &state, &cols); err != nil {
			indexes.Unavailable = "decoding indexes: " + err.Error()
			break
		}
		indexes.Items = append(indexes.Items, console.Resource{
			Name: name,
			Fields: map[string]string{
				"Type": indexType, "Unique": yesNo(unique),
				// PRIMARY_KEY has no state; a backfilling index does, and it is
				// the difference between an index that works and one that will.
				"State": orDash(state.StringVal), "Columns": cols,
			},
		})
	}
	idx.Stop()
	indexes.Total = len(indexes.Items)

	return console.Detail{
		Summary: []console.Property{
			{Label: "Database", Value: dbID},
			{Label: "Instance", Value: instanceID},
			{Label: "Columns", Value: fmt.Sprint(columns.Total)},
			{Label: "Indexes", Value: fmt.Sprint(indexes.Total)},
		},
		Sections: []console.Section{
			{ID: "columns", Label: "Columns", Listing: columns},
			{ID: "indexes", Label: "Indexes", Listing: indexes},
		},
	}, nil
}

// QueryHint is the Spanner Studio's contract with the user.
func (spannerProvider) QueryHint() string {
	return "Read-only SQL against this database. Every statement runs in a " +
		"read-only transaction, so a write is refused by Spanner itself; an " +
		"INSERT, UPDATE or DELETE is stopped before it is sent, with the reason. " +
		"Switch to Read-write to change data."
}

// Query runs a read-only statement against one database.
//
// Read-only is enforced by the transaction, not by inspecting the text. A
// console that decided what a statement did by looking at it would be wrong
// about the first statement nobody thought of, and the cost of being wrong is
// an unintended write. The classifier only adds an earlier, clearer refusal
// for what it recognises as DML or DDL (#798); everything else still reaches
// the read-only transaction, which has the last word.
func (p spannerProvider) Query(ctx context.Context, project string, path []string, statement string) (console.Listing, error) {
	if project == "" {
		return console.Listing{Prompt: "Choose a project in the toolbar."}, nil
	}
	if len(path) < 2 {
		return console.Listing{}, fmt.Errorf(
			"a Spanner query runs against a database: open one from its instance")
	}
	if err := readOnlySpanner(statement); err != nil {
		return console.Listing{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	dbName := fmt.Sprintf("projects/%s/instances/%s/databases/%s", project, path[0], path[1])
	c, err := spanner.NewClient(ctx, dbName, localOpts(p.endpoint)...)
	if err != nil {
		return console.Listing{}, fmt.Errorf("cannot open the database: %w", err)
	}
	defer c.Close()

	// Single() is a read-only snapshot transaction. A DML statement inside one
	// is refused by Spanner.
	iter := c.Single().Query(ctx, spanner.Statement{SQL: statement})
	defer iter.Stop()

	out := console.Listing{Noun: "rows"}
	for {
		row, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			// Spanner's own message, which names the syntax error's position.
			return console.Listing{}, err
		}
		if out.Columns == nil {
			// The column names come from the result, not from the statement:
			// an expression without an alias gets whatever name Spanner gives
			// it, and guessing would label the wrong column.
			names := row.ColumnNames()
			if len(names) == 0 {
				return console.Listing{}, fmt.Errorf("the statement returned no columns")
			}
			out.NameColumn = names[0]
			out.Columns = names[1:]
		}
		fields := map[string]string{}
		var first string
		for i, name := range row.ColumnNames() {
			var value spanner.GenericColumnValue
			if err := row.Column(i, &value); err != nil {
				return console.Listing{}, fmt.Errorf("reading column %s: %w", name, err)
			}
			rendered := renderSpannerValue(value)
			if i == 0 {
				first = rendered
				continue
			}
			fields[name] = rendered
		}
		out.Items = append(out.Items, console.Resource{Name: first, Fields: fields})
		if len(out.Items) >= detailLimit {
			out.Note = truncatedNote(len(out.Items), "rows")
			break
		}
	}
	if out.Columns == nil {
		out.Columns = []string{}
		out.NameColumn = "Result"
	}
	out.Total = len(out.Items)
	return out, nil
}

// renderSpannerValue prints a result cell.
//
// A NULL renders as an em dash, which is how every other listing in this console
// shows an absent value — and distinguishes it from the empty string, which is a
// different answer.
func renderSpannerValue(v spanner.GenericColumnValue) string {
	if _, ok := v.Value.GetKind().(*structpb.Value_NullValue); ok {
		return "—"
	}
	switch kind := v.Value.GetKind().(type) {
	case *structpb.Value_StringValue:
		return kind.StringValue
	case *structpb.Value_BoolValue:
		return fmt.Sprint(kind.BoolValue)
	case *structpb.Value_NumberValue:
		return strconv.FormatFloat(kind.NumberValue, 'g', -1, 64)
	default:
		// A struct or a list. JSON, because Go's %v on a protobuf value is
		// unreadable and this pane exists to be read.
		if encoded, err := v.Value.MarshalJSON(); err == nil {
			return string(encoded)
		}
		return v.Value.String()
	}
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
			Help: "An existing instance, or a new one — it is created when absent, " +
				"because a database cannot exist without one. The Spanner screen " +
				"lists the instances this project already has.",
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

// DetailActions offers the drops, at the level each one belongs to.
//
// The list screen shows instances now, so a name-addressed Deleter would have
// meant "delete this instance" — which drops every database inside it. Dropping
// a database belongs on the database's own page, where the thing being dropped
// is the thing being looked at.
func (p spannerProvider) DetailActions(_ context.Context, project string, path []string) []console.Action {
	if project == "" {
		return nil
	}
	switch len(path) {
	case 1:
		return []console.Action{{
			ID: "dropinstance", Label: "Delete instance", Destructive: true,
		}}
	case 2:
		return []console.Action{{
			ID: "dropdatabase", Label: "Drop database", Destructive: true,
		}}
	}
	return nil
}

func (p spannerProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	if project == "" {
		return fmt.Errorf("choose a project first")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	switch action {
	case "dropdatabase":
		dbAdmin, err := database.NewDatabaseAdminClient(ctx, localOpts(p.endpoint)...)
		if err != nil {
			return fmt.Errorf("cannot reach Spanner: %w", err)
		}
		defer dbAdmin.Close()
		return dbAdmin.DropDatabase(ctx, &databasepb.DropDatabaseRequest{
			Database: fmt.Sprintf("projects/%s/instances/%s/databases/%s",
				project, path[0], path[1]),
		})
	case "dropinstance":
		instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
		if err != nil {
			return fmt.Errorf("cannot reach Spanner: %w", err)
		}
		defer instAdmin.Close()
		// Spanner deletes the instance's databases with it. Stated in the error
		// nowhere and in the confirmation everywhere: the client's confirm
		// dialog names the instance, and this is the operation that takes the
		// databases too.
		return instAdmin.DeleteInstance(ctx, &instancepb.DeleteInstanceRequest{
			Name: fmt.Sprintf("projects/%s/instances/%s", project, path[0]),
		})
	}
	return fmt.Errorf("unknown action %q", action)
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
	_ console.Driller = firestoreProvider{}
	_ console.Driller = datastoreProvider{}
	_ console.Driller = bigtableProvider{}
	_ console.Driller = spannerProvider{}
	// Spanner drops through path-addressed actions rather than a Deleter: the
	// list screen shows instances, and deleting one takes its databases with it.
	_ console.PathActor       = spannerProvider{}
	_ console.Executor        = spannerProvider{}
	_ console.StatementWriter = spannerProvider{}
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

	col, err := firestoreCollection(c, collection)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	ref := col.Doc(id)
	if ref == nil {
		return console.Detail{Unavailable: fmt.Sprintf("%q is not a document ID: an ID has no slash", id)}, nil
	}
	snap, err := ref.Get(ctx)
	if status.Code(err) == codes.NotFound {
		return missingDocumentDetail(ctx, ref, collection, id, err), nil
	}
	if err != nil {
		return console.Detail{Unavailable: "cannot read the document: " + err.Error()}, nil
	}

	fields := console.Listing{
		Columns:    []string{"Type", "Value"},
		NameColumn: "Field",
		Noun:       "fields",
		// A field opens to its own page, which is where it is edited and
		// deleted (#796).
		RowsOpenable: true,
	}
	data := snap.Data()
	for _, key := range sortedAnyKeys(data) {
		fields.Items = append(fields.Items, console.Resource{
			Name: key,
			Fields: map[string]string{
				"Type": firestoreType(data[key]),
				// Not truncated. This page is the reason the listing's cell
				// could be.
				"Value": renderFirestoreValue(data[key]),
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
		Sections: []console.Section{
			{ID: "fields", Label: "Fields", Listing: fields},
			firestoreSubcollections(ctx, snap.Ref, collection+"/"+id),
		},
	}, nil
}

// missingDocumentDetail is the page of a document that does not exist: the
// italic row a collection lists for a document with no fields and
// subcollections (#875). Its page is its Collections tab, so what is under
// it opens from here as from any document's page. A path with nothing under
// it either is the read's own NOT_FOUND.
func missingDocumentDetail(ctx context.Context, ref *firestore.DocumentRef, collection, id string, notFound error) console.Detail {
	subs := firestoreSubcollections(ctx, ref, collection+"/"+id)
	if subs.Unavailable != "" || subs.Listing.Total == 0 {
		return console.Detail{Unavailable: "cannot read the document: " + notFound.Error()}
	}
	fields := console.Listing{Columns: []string{"Type", "Value"}, NameColumn: "Field", Noun: "fields",
		Note: "This document does not exist: it has no fields, and is listed because it has subcollections. " +
			"Add field creates it with that field."}
	return console.Detail{
		Summary: []console.Property{
			{Label: "Collection", Value: collection},
			{Label: "Document ID", Value: id},
			{Label: "Path", Value: ref.Path},
			{Label: "Exists", Value: "No — " + firestoreMissingNote},
		},
		Sections: []console.Section{
			{ID: "fields", Label: "Fields", Listing: fields},
			subs,
		},
	}
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

	col, err := firestoreCollection(c, path[0])
	if err != nil {
		return console.Listing{}, err
	}
	q := col.Query
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

// entityDetail is one Datastore entity, property by property, and its child
// entities.
func (p datastoreProvider) entityDetail(ctx context.Context, project string, scope datastoreScope, kind, id string) (console.Detail, error) {
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

	// A row opens its entity's page by the entity's encoded key (#875),
	// which is read back here, so a row and the page it opens address the
	// same entity; a link in the listing's older form, a name, id=123 or a
	// key path, still reads back too.
	key, err := datastoreEntityKey(project, scope.ns, kind, id)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}

	// Through the v1 API, as Add, Edit and Delete property write it back, so
	// a key value in another project or database is shown as one (#893).
	props, err := p.readEntity(ctx, project, key)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the entity: " + err.Error()}, nil
	}

	fields := console.Listing{
		Columns:    []string{"Type", "Indexed", "Value"},
		NameColumn: "Property",
		Noun:       "properties",
		// A property opens to its own page, which is where it is edited and
		// deleted (#796).
		RowsOpenable: true,
	}
	sort.SliceStable(props, func(a, b int) bool { return props[a].Name < props[b].Name })
	for _, prop := range props {
		fields.Items = append(fields.Items, console.Resource{
			Name: prop.Name,
			Fields: map[string]string{
				// The type as the edit form names it, not Go's: "*datastore.Key"
				// is this console's implementation, "key" is Datastore's.
				"Type": datastoreType(prop.Value),
				// NoIndex inverted, because "indexed" is what a query needs and
				// the double negative is where a reader loses the thread.
				"Indexed": yesNo(!prop.NoIndex),
				"Value":   renderDatastoreValue(prop.Value, prop.NoIndex),
			},
		})
	}
	fields.Total = len(fields.Items)

	summary := []console.Property{
		{Label: "Kind", Value: kind},
		{Label: datastoreNameIDColumn, Value: datastoreNameID(key)},
		{Label: "Key path", Value: datastoreKeyPathLabel(key)},
	}
	if key.Parent != nil {
		summary = append(summary, console.Property{Label: "Parent", Value: datastoreKeyPathLabel(key.Parent)})
	}
	summary = append(summary,
		console.Property{Label: "Namespace", Value: namespaceLabel(key.Namespace)},
		console.Property{Label: "Properties", Value: fmt.Sprint(fields.Total)})

	return console.Detail{
		Summary: summary,
		Sections: []console.Section{
			{ID: "properties", Label: "Properties", Listing: fields},
			datastoreChildren(ctx, c, scope, key),
		},
	}, nil
}

// datastoreDescendantScan bounds the ancestor query a Children tab reads: it
// returns every descendant, grandchildren included, and the children are
// picked out of them.
const datastoreDescendantScan = 1000

// datastoreChildren is an entity's Children tab: the entities whose parent
// is its key, from a kindless ancestor query, each opening to its own page.
func datastoreChildren(ctx context.Context, c *datastore.Client, scope datastoreScope, key *datastore.Key) console.Section {
	sec := console.Section{ID: "children", Label: "Children"}
	list := console.Listing{Columns: []string{"Kind"}, NameColumn: datastoreNameIDColumn, Noun: "child entities"}
	keys, err := c.GetAll(ctx, datastore.NewQuery("").Namespace(key.Namespace).Ancestor(key).
		KeysOnly().Limit(datastoreDescendantScan), nil)
	if err != nil {
		sec.Unavailable = "reading child entities: " + err.Error()
		return sec
	}
	for _, k := range keys {
		if k.Parent == nil || !k.Parent.Equal(key) {
			continue // the entity itself, or a grandchild
		}
		list.Items = append(list.Items, console.Resource{
			Name: datastoreNameID(k), Fields: map[string]string{"Kind": k.Kind},
			Opens: scope.at(k.Kind, datastoreEntityAddress(k)),
		})
		if len(list.Items) >= detailLimit {
			break
		}
	}
	list.Total = len(list.Items)
	switch {
	case len(keys) >= datastoreDescendantScan:
		list.Note = fmt.Sprintf("Read from the first %d descendants; there may be more children.", datastoreDescendantScan)
	case len(list.Items) >= detailLimit:
		list.Note = truncatedNote(len(list.Items), "child entities")
	case list.Total == 0:
		list.Note = "This entity has no child entities. Create child entity makes one with this entity as its parent."
	}
	sec.Listing = list
	return sec
}

// datastoreKey reads back the key the listing rendered for a root entity in
// the default namespace.
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
	// A namespace's own pages list kinds, not entities: there is nothing on
	// them to query.
	if scope, rest := parseDatastorePath(path); scope.index || (scope.namespaced && len(rest) == 0) {
		return "", nil
	}
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
		Columns: datastoreEntityColumns, Noun: "entities", NameColumn: datastoreNameIDColumn,
		RowsOpenable: true,
	}
	if project == "" {
		out.Prompt = "Choose a project in the toolbar."
		return out, nil
	}
	scope, rest := parseDatastorePath(path)
	if scope.index || len(rest) != 1 {
		return console.Listing{}, fmt.Errorf("a Datastore query runs against a kind")
	}
	kind := rest[0]
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	// Through the v1 API's RunQuery, as the kind's listing and the entity's
	// page read it, so a key value renders the same on all three (#904).
	c, done, err := p.rawDatastore()
	if err != nil {
		out.Unavailable = err.Error()
		return out, nil
	}
	defer done()

	q := &datastorepb.Query{Kind: []*datastorepb.KindExpression{{Name: kind}}}
	if prop := strings.TrimSpace(values["property"]); prop != "" {
		op := strings.TrimSpace(values["op"])
		if op == "" {
			op = "="
		}
		value, err := typedValue(values["value"], "==")
		if err != nil {
			return console.Listing{}, err
		}
		if q.Filter, err = datastoreFilterPB(prop, op, value); err != nil {
			return console.Listing{}, err
		}
	}
	if order := strings.TrimSpace(values["orderBy"]); order != "" {
		if values["descending"] == "true" {
			order = "-" + order
		}
		o, err := datastoreOrderPB(order)
		if err != nil {
			return console.Listing{}, err
		}
		q.Order = []*datastorepb.PropertyOrder{o}
	}

	res, err := runEntityQuery(ctx, c, project, scope.ns, q, queryLimit(values["limit"]), nil)
	if err != nil {
		// Datastore's own message, which for a missing index names the index.
		return console.Listing{}, err
	}
	for _, e := range res.Entities {
		k, props := datastoreEntityProperties(e, project)
		out.Items = append(out.Items, datastoreEntityRow(scope, kind, k, datastorePropertiesCell(props)))
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
				// Every version of the column, through MutateRow (#797).
				Actions: []console.Action{{ID: "deletecells", Label: "Delete cells", Destructive: true}},
			})
		}
	}
	cells.Total = len(cells.Items)
	cells.Note = "Delete cells removes every version of that column in this row. " +
		"When the last cell goes, so does the row."

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
