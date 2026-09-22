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
	base.Note = "Document counts stop at 100; a collection with more shows a trailing plus."
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
