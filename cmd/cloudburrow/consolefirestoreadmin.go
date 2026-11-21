package main

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// firestoreDatabasesProvider is Firestore's second screen (#1331): the
// project's databases, each with its composite indexes and field overrides,
// read through the Firestore Admin API the Firestore pod's call front serves
// (internal/firestoreadmin). Create database and Delete database go through
// the same API, and so do Edit database, Create index, Delete index and
// the field override forms (#1343, consolefirestoreadminedit.go).
//
// Indexes and field overrides are stored and READY at once, and change
// nothing about queries: the emulator answers any query without an index and
// deletes no expired document. The pages say so.
type firestoreDatabasesProvider struct {
	endpoint string
	// mongo is Firestore with MongoDB compatibility's host address
	// (#1703), shown in an Enterprise database's connection string, and
	// mongoCA its CA certificate's file (#1738).
	mongo, mongoCA string
	// projects is Resource Manager, whose project IAM policy Add User
	// grants the new user's role in (#1845, consolefirestoresecurity.go).
	projects *resourcemanager.Registry
	// rules is the Firebase Rules API's host address (#2048), or "".
	rules func() string
}

// mongoConnectionString is the connection string Google documents for
// SCRAM ("Authenticate and connect to a database"), for an Enterprise
// edition database on this instance's endpoint: TLS, as Google's, trusting
// the instance's CA (tlsCAFile, #1738).
func (p firestoreDatabasesProvider) mongoConnectionString(user, dbID string) string {
	if p.mongo == "" {
		return ""
	}
	cs := "mongodb://" + user + ":PASSWORD@" + p.mongo + "/" + dbID + "?loadBalanced=true&authMechanism=SCRAM-SHA-256&tls=true&retryWrites=false"
	if p.mongoCA != "" {
		cs += "&tlsCAFile=" + url.QueryEscape(p.mongoCA)
	}
	return cs
}

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
	base := console.Listing{Columns: []string{"Mode", "Edition", "Location", "Scheduled backups", "Concurrency", "Delete protection"},
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
			"Mode":              databaseType(db.Type),
			"Edition":           enumWord(db.DatabaseEdition, ""),
			"Location":          db.LocationId,
			"Scheduled backups": scheduledBackups(ctx, c, db.Name),
			"Concurrency":       enumWord(db.ConcurrencyMode, ""),
			"Delete protection": enumWord(db.DeleteProtectionState, "DELETE_PROTECTION_"),
		},
			// Clone in the row's View more, as Google's (#1845).
			Actions: databaseRowActions(project, db), ActsOn: []string{dbID(db.Name)}})
	}
	base.Total = len(base.Items)
	base.Note = "Data browses any database, chosen in its Database picker; each database's Query builder runs against its own documents. Every database is served by the one emulator, " +
		"each with its own documents; deleting a database deletes its documents."
	return base, nil
}

// databaseRowActions are a database row's View more: Clone, and, for a
// Datastore mode database, Datastore Studio, its entities on the Datastore
// page, which is where Google's Databases page opens one (#1846).
func databaseRowActions(project string, db *adminpb.Database) []console.Action {
	acts := []console.Action{cloneAction()}
	if db.GetType() == adminpb.Database_DATASTORE_MODE {
		q := url.Values{"project": {project}}
		if id := dbID(db.GetName()); id != firestoreDefaultDatabase {
			q.Set(firestoreDatabaseKey, id)
		}
		acts = append([]console.Action{{ID: "datastore-studio", Label: "Datastore Studio", Href: "/datastore?" + q.Encode()}}, acts...)
	}
	return acts
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
	// Google's database menu, in its order (#1845): Query builder (of
	// Firestore Studio), Indexes (Composite, then Single field with its
	// exemptions), Import/Export, Disaster recovery, Time-to-live.
	acts := firestoreTabActions()
	sections := []console.Section{
		{ID: "query", Label: "Query builder", Kind: console.KindQuery},
		// One Indexes tab holding Google's Manual (composite and vector
		// indexes) and Automatic (single-field settings and exemptions) tabs
		// (https://docs.cloud.google.com/firestore/native/docs/query-data/indexing).
		{ID: "indexes", Label: "Manual", Group: "Indexes", Listing: p.indexes(ctx, c, name, path[0]), Actions: acts["indexes"]},
		{ID: "fields", Label: "Automatic", Group: "Indexes", Listing: p.fields(ctx, c, name, path[0]), Actions: acts["fields"]},
		{ID: "importexport", Label: "Import/Export", Listing: p.importExportJobs(ctx, c, name), Actions: acts["importexport"],
			Note: "Export copies the database's documents, every collection group or the ones named, to a gs:// folder of this " +
				"instance's Cloud Storage; Import reads such a folder back. Each runs as a long-running operation, listed here."},
		// Google's Disaster recovery page: the schedules described, Edit,
		// and the Backups table (#1845).
		p.disasterRecovery(ctx, c, name, path[0]),
		{ID: "ttl", Label: "Time-to-live", Listing: p.ttlPolicies(ctx, c, name, path[0]), Actions: acts["ttl"]},
		// Security (rules), Usage and Query insights (#2048,
		// consolefirestorerules.go).
		p.rulesSection(ctx, project, path[0]),
		p.usageSection(ctx, project, path[0]),
		p.insightsSection(ctx, project, path[0]),
	}
	// Users (Security) and pipelines are Enterprise edition features
	// (#1500, consolefirestorepipeline.go).
	if db.DatabaseEdition == adminpb.Database_ENTERPRISE {
		// Google's base connection string, which the Explorer panel shows
		// (Connect using MongoDB tools); a user's own, with SCRAM, is in
		// Add user's dialog.
		if cs := p.baseConnectionString(path[0]); cs != "" {
			summary = append(summary, console.Property{Label: "Connection string", Value: cs})
		}
		// Google's Security page of an Enterprise database: its users,
		// with Add User (#1845, round 3).
		sections = append(sections, console.Section{ID: "usercreds", Label: "Security",
			Listing: p.userCreds(ctx, c, project, name, path[0]), Actions: []console.Action{addUserAction()}})
	}
	return console.Detail{Summary: summary, Sections: sections}, nil
}

// indexes is a database's Indexes tab: its composite indexes, in every
// collection group.
func (p firestoreDatabasesProvider) indexes(ctx context.Context, c *admin.FirestoreAdminClient, db, id string) console.Listing {
	list := console.Listing{Columns: []string{"Collection group", "Fields", "Query scope", "API scope", "State"}, NameColumn: "Index ID", Noun: "indexes"}
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
			"API scope":        apiScopeWord(x),
			"State":            enumWord(x.State, ""),
		},
			// Delete index (#1343), addressed by the collection group and ID.
			Actions: indexActions(),
			ActsOn:  []string{id, firestoreIndexesSegment, parts[len(parts)-3], parts[len(parts)-1]},
		})
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
		case f.GetSearchConfig().GetTextSpec() != nil:
			mode = "Text"
		}
		out = append(out, f.FieldPath+" "+mode)
	}
	return strings.Join(out, ", ")
}

// apiScopeWord is an index's API scope as the console shows it, with a
// text index's language (#1738).
func apiScopeWord(x *adminpb.Index) string {
	w := "Any API"
	if x.GetApiScope() == adminpb.Index_MONGODB_COMPATIBLE_API {
		w = "MongoDB compatible API"
	}
	if l := x.GetSearchIndexOptions().GetTextLanguage(); l != "" {
		w += ", text language " + l
	}
	return w
}

// fields is a database's Field overrides tab: the fields with index
// settings or a TTL policy of their own.
func (p firestoreDatabasesProvider) fields(ctx context.Context, c *admin.FirestoreAdminClient, db, id string) console.Listing {
	list := console.Listing{Columns: []string{"Collection group", "Indexes", "TTL"}, NameColumn: "Field", Noun: "exemptions"}
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
		},
			// Edit field override and Delete field override (#1343).
			Actions: fieldActions(f),
			ActsOn:  []string{id, firestoreFieldsSegment, parts[len(parts)-3], parts[len(parts)-1]},
		})
	}
	list.Total = len(list.Items)
	list.Note = "Field overrides are stored as the Admin API returns them and change nothing: the emulator needs " +
		"no index and deletes no expired document."
	return list
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
	req, err := createDatabaseRequest(project, values)
	if err != nil {
		return "", err
	}
	op, err := c.CreateDatabase(ctx, req)
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
