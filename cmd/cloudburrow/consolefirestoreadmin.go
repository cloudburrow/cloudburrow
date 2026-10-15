package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// firestoreDatabasesProvider is Firestore's second screen (#1331): the
// project's databases, each with its composite indexes and field overrides,
// read through the Firestore Admin API the Firestore pod's call front serves
// (internal/firestoreadmin). Create database and Delete database go through
// the same API.
//
// Indexes and field overrides are stored and READY at once, and change
// nothing about queries: the emulator answers any query without an index and
// deletes no expired document. The pages say so.
type firestoreDatabasesProvider struct{ endpoint string }

func (firestoreDatabasesProvider) ID() string    { return "firestore-databases" }
func (firestoreDatabasesProvider) Title() string { return "Firestore databases" }

func (p firestoreDatabasesProvider) client(ctx context.Context) (*admin.FirestoreAdminClient, error) {
	c, err := admin.NewFirestoreAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the Firestore Admin API: %w", err)
	}
	return c, nil
}

func dbID(name string) string { return name[strings.LastIndex(name, "/")+1:] }

// enumWord is an enum's value as Google's console words it: "OPTIMISTIC"
// as Optimistic, "DELETE_PROTECTION_ENABLED" as Enabled.
func enumWord(v fmt.Stringer, prefix string) string {
	s := strings.TrimPrefix(v.String(), prefix)
	s = strings.ReplaceAll(strings.ToLower(s), "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (p firestoreDatabasesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	base := console.Listing{Columns: []string{"Type", "Location", "Concurrency", "Delete protection"},
		Noun: "databases", NameColumn: "Database", RowsOpenable: true}
	if project == "" {
		base.Prompt = "Firestore holds databases per project. Choose one in the toolbar."
		return base, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		base.Unavailable = err.Error()
		return base, nil
	}
	defer c.Close()
	resp, err := c.ListDatabases(ctx, &adminpb.ListDatabasesRequest{Parent: "projects/" + project})
	if err != nil {
		base.Unavailable = "listing databases: " + err.Error()
		return base, nil
	}
	for _, db := range resp.Databases {
		base.Items = append(base.Items, console.Resource{Name: dbID(db.Name), Fields: map[string]string{
			"Type":              databaseType(db.Type),
			"Location":          db.LocationId,
			"Concurrency":       enumWord(db.ConcurrencyMode, ""),
			"Delete protection": enumWord(db.DeleteProtectionState, "DELETE_PROTECTION_"),
		}})
	}
	base.Total = len(base.Items)
	base.Note = "The Firestore screen browses (default)'s collections. Every database is served by the one emulator, " +
		"each with its own documents; deleting a database deletes its documents."
	return base, nil
}

func databaseType(t adminpb.Database_DatabaseType) string {
	switch t {
	case adminpb.Database_FIRESTORE_NATIVE:
		return "Native"
	case adminpb.Database_DATASTORE_MODE:
		return "Datastore mode"
	}
	return t.String()
}

// Detail is a database's page: its settings, and its Indexes and Field
// overrides tabs.
func (p firestoreDatabasesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer c.Close()
	name := "projects/" + project + "/databases/" + path[0]
	db, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "reading the database: " + err.Error()}, nil
	}
	summary := []console.Property{
		{Label: "Type", Value: databaseType(db.Type)},
		{Label: "Location", Value: db.LocationId},
		{Label: "Edition", Value: enumWord(db.DatabaseEdition, "")},
		{Label: "Concurrency mode", Value: enumWord(db.ConcurrencyMode, "")},
		{Label: "Point-in-time recovery", Value: enumWord(db.PointInTimeRecoveryEnablement, "POINT_IN_TIME_RECOVERY_")},
		{Label: "Version retention", Value: db.VersionRetentionPeriod.AsDuration().String()},
		{Label: "App Engine integration", Value: enumWord(db.AppEngineIntegrationMode, "")},
		{Label: "Delete protection", Value: enumWord(db.DeleteProtectionState, "DELETE_PROTECTION_")},
		{Label: "UID", Value: db.Uid},
		{Label: "Created", Value: db.CreateTime.AsTime().Format("2006-01-02 15:04:05 MST")},
	}
	return console.Detail{Summary: summary, Sections: []console.Section{
		{ID: "indexes", Label: "Indexes", Listing: p.indexes(ctx, c, name)},
		{ID: "fields", Label: "Field overrides", Listing: p.fields(ctx, c, name)},
	}}, nil
}

// indexes is a database's Indexes tab: its composite indexes, in every
// collection group.
func (p firestoreDatabasesProvider) indexes(ctx context.Context, c *admin.FirestoreAdminClient, db string) console.Listing {
	list := console.Listing{Columns: []string{"Collection group", "Fields", "Query scope", "State"}, NameColumn: "Index ID", Noun: "indexes"}
	it := c.ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: db + "/collectionGroups/-"})
	for {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			list.Unavailable = "listing indexes: " + err.Error()
			return list
		}
		parts := strings.Split(x.Name, "/")
		list.Items = append(list.Items, console.Resource{Name: parts[len(parts)-1], Fields: map[string]string{
			"Collection group": parts[len(parts)-3],
			"Fields":           indexFields(x.Fields),
			"Query scope":      enumWord(x.QueryScope, ""),
			"State":            enumWord(x.State, ""),
		}})
	}
	list.Total = len(list.Items)
	list.Note = "Indexes are stored and READY at once, as the Admin API returns them, and change nothing about " +
		"queries: the emulator answers any query without an index."
	return list
}

func indexFields(fields []*adminpb.Index_IndexField) string {
	var out []string
	for _, f := range fields {
		mode := ""
		switch {
		case f.GetOrder() != 0:
			mode = enumWord(f.GetOrder(), "")
		case f.GetArrayConfig() != 0:
			mode = "Array contains"
		case f.GetVectorConfig() != nil:
			mode = fmt.Sprintf("Vector (%d)", f.GetVectorConfig().GetDimension())
		}
		out = append(out, f.FieldPath+" "+mode)
	}
	return strings.Join(out, ", ")
}

// fields is a database's Field overrides tab: the fields with index
// settings or a TTL policy of their own.
func (p firestoreDatabasesProvider) fields(ctx context.Context, c *admin.FirestoreAdminClient, db string) console.Listing {
	list := console.Listing{Columns: []string{"Collection group", "Indexes", "TTL"}, NameColumn: "Field", Noun: "field overrides"}
	seen := map[string]*adminpb.Field{}
	for _, filter := range []string{"indexConfig.usesAncestorConfig:false", "ttlConfig:*"} {
		it := c.ListFields(ctx, &adminpb.ListFieldsRequest{Parent: db + "/collectionGroups/-", Filter: filter})
		for {
			f, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				list.Unavailable = "listing field overrides: " + err.Error()
				return list
			}
			seen[f.Name] = f
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := seen[n]
		parts := strings.Split(n, "/")
		indexes := "Inherited"
		if !f.GetIndexConfig().GetUsesAncestorConfig() {
			var modes []string
			for _, x := range f.GetIndexConfig().GetIndexes() {
				modes = append(modes, strings.TrimPrefix(indexFields(x.Fields), parts[len(parts)-1]+" "))
			}
			indexes = "Exempted (none)"
			if len(modes) > 0 {
				indexes = strings.Join(modes, ", ")
			}
		}
		ttl := "None"
		if f.GetTtlConfig() != nil {
			ttl = enumWord(f.GetTtlConfig().GetState(), "")
		}
		list.Items = append(list.Items, console.Resource{Name: parts[len(parts)-1], Fields: map[string]string{
			"Collection group": parts[len(parts)-3], "Indexes": indexes, "TTL": ttl,
		}})
	}
	list.Total = len(list.Items)
	list.Note = "Field overrides are stored as the Admin API returns them and change nothing: the emulator needs " +
		"no index and deletes no expired document."
	return list
}

// CreateForm implements console.Creator: Create database.
func (firestoreDatabasesProvider) CreateForm() (string, []console.Field) {
	return "Create database", []console.Field{
		{Name: "database", Label: "Database ID", Type: "text", Required: true,
			Help:    "4 to 63 lowercase letters, numbers and hyphens, starting with a letter and ending with a letter or number.",
			Pattern: `^[a-z][a-z0-9-]{2,61}[a-z0-9]$`},
		{Name: "location", Label: "Location", Type: "text", Required: true, Default: "nam5",
			Help: "A Firestore location, such as nam5, eur3 or us-east1."},
		{Name: "concurrency", Label: "Concurrency mode", Type: "select", Default: "PESSIMISTIC",
			Options: []string{"PESSIMISTIC", "OPTIMISTIC"}},
	}
}

// Create implements console.Creator: a Native mode database.
func (p firestoreDatabasesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", fmt.Errorf("choose a project first")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return "", err
	}
	defer c.Close()
	mode := adminpb.Database_ConcurrencyMode(adminpb.Database_ConcurrencyMode_value[values["concurrency"]])
	op, err := c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/" + project,
		DatabaseId: strings.TrimSpace(values["database"]),
		Database: &adminpb.Database{LocationId: strings.TrimSpace(values["location"]),
			Type: adminpb.Database_FIRESTORE_NATIVE, ConcurrencyMode: mode}})
	if err != nil {
		return "", fmt.Errorf("creating the database: %w", err)
	}
	db, err := op.Wait(ctx)
	if err != nil {
		return "", fmt.Errorf("creating the database: %w", err)
	}
	return dbID(db.Name), nil
}

// Delete implements console.Deleter: the database and every document in it.
func (p firestoreDatabasesProvider) Delete(ctx context.Context, project, name string) error {
	if project == "" {
		return fmt.Errorf("choose a project first")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	op, err := c.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: "projects/" + project + "/databases/" + name})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	return err
}
