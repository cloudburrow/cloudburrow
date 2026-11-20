package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Firestore's pages as Google's console draws them (#1845): Create a
// Firestore database as one page, a database's Query builder,
// Time-to-live page and Google's labels on its Indexes, Import/Export and
// Disaster recovery tabs. Sources, read 2026-11-19:
// https://docs.cloud.google.com/firestore/native/docs/manage-databases,
// https://docs.cloud.google.com/firestore/native/docs/locations,
// https://docs.cloud.google.com/firestore/native/docs/ttl,
// https://docs.cloud.google.com/firestore/native/docs/using-console.

// firestoreTTLSegment is the second segment of a TTL policy's action path.
const firestoreTTLSegment = "ttl"

const (
	fsLocationRegion      = "region"
	fsLocationMultiRegion = "multi-region"
	fsModeNative          = "native"
	fsModeDatastore       = "datastore"
	fsRulesRestrictive    = "restrictive"
	fsRulesOpen           = "open"
	fsEncryptionGoogle    = "google"
	fsEncryptionKMS       = "kms"
)

// firestoreMultiRegions and firestoreRegions are Google's Firestore
// locations, in the order of the locations page.
var firestoreMultiRegions = [][2]string{
	{"eur3", "Europe"}, {"nam5", "United States (Central)"}, {"nam7", "United States (Central and East)"},
}

var firestoreRegions = [][2]string{
	{"us-west1", "Oregon"}, {"us-west2", "Los Angeles"}, {"us-west3", "Salt Lake City"}, {"us-west4", "Las Vegas"},
	{"us-central1", "Iowa"}, {"northamerica-northeast1", "Montréal"}, {"northamerica-northeast2", "Toronto"},
	{"northamerica-south1", "Queretaro"}, {"us-east1", "South Carolina"}, {"us-east4", "Northern Virginia"},
	{"us-east5", "Columbus"}, {"us-south1", "Dallas"}, {"southamerica-west1", "Santiago"},
	{"southamerica-east1", "São Paulo"}, {"europe-west2", "London"}, {"europe-west1", "Belgium"},
	{"europe-west4", "Netherlands"}, {"europe-west8", "Milan"}, {"europe-southwest1", "Madrid"},
	{"europe-west9", "Paris"}, {"europe-west12", "Turin"}, {"europe-west10", "Berlin"},
	{"europe-west3", "Frankfurt"}, {"europe-north1", "Finland"}, {"europe-north2", "Stockholm"},
	{"europe-central2", "Warsaw"}, {"europe-west6", "Zürich"}, {"me-central1", "Doha"},
	{"me-central2", "Dammam"}, {"me-west1", "Tel Aviv"}, {"asia-south1", "Mumbai"}, {"asia-south2", "Delhi"},
	{"asia-southeast1", "Singapore"}, {"asia-southeast2", "Jakarta"}, {"asia-southeast3", "Bangkok"},
	{"asia-east2", "Hong Kong"}, {"asia-east1", "Taiwan"}, {"asia-northeast1", "Tokyo"},
	{"asia-northeast2", "Osaka"}, {"asia-northeast3", "Seoul"}, {"australia-southeast1", "Sydney"},
	{"australia-southeast2", "Melbourne"}, {"africa-south1", "Johannesburg"},
}

func locationChoices(list [][2]string) ([]string, map[string]string) {
	ids := make([]string, 0, len(list))
	labels := map[string]string{}
	for _, l := range list {
		ids = append(ids, l[0])
		labels[l[0]] = l[0] + " (" + l[1] + ")"
	}
	return ids, labels
}

func knownLocation(list [][2]string, id string) bool {
	for _, l := range list {
		if l[0] == id {
			return true
		}
	}
	return false
}

const (
	fsNoRulesAPI = "Not available locally: CloudBurrow serves no Firebase Security Rules API, so no rules are " +
		"written. Server client libraries ignore rules, as on Google's, and that is what reaches this database."
)

// kmsKeyPattern is a Cloud KMS key's resource name, as cmek_config takes it;
// the slash is escaped in its classes, as the browser compiles it with the v
// flag.
const kmsKeyPattern = `projects/[^\/]+/locations/[^\/]+/keyRings/[^\/]+/cryptoKeys/[^\/]+`

// CreateForm implements console.Creator: Google's Create a Firestore
// database, in its order.
func (firestoreDatabasesProvider) CreateForm() (string, []console.Field) {
	regions, regionLabels := locationChoices(firestoreRegions)
	multi, multiLabels := locationChoices(firestoreMultiRegions)
	return "Create a Firestore database", []console.Field{
		{Name: "database", Label: "Database ID", Type: "text", Required: true, Pattern: databaseIDPattern,
			Help: "Lowercase letters, numbers and hyphens, 4 to 63 characters, starting with a letter and ending with a letter or number."},
		{Name: "edition", Label: "Edition", Type: "radio", Options: []string{"STANDARD", "ENTERPRISE"}, Default: "STANDARD",
			OptionLabels: map[string]string{"STANDARD": "Standard edition", "ENTERPRISE": "Enterprise edition"},
			Help:         "Enterprise edition runs pipelines and MongoDB compatibility. It cannot be changed later."},
		{Name: "mode", Label: "Data access mode", Type: "radio", Options: []string{fsModeNative, fsModeDatastore}, Default: fsModeNative,
			OptionLabels: map[string]string{fsModeNative: "Firestore Native", fsModeDatastore: "Datastore mode"},
			// A Datastore mode database is served through the Datastore
			// API (#1880), and browsed on the Datastore page.
			Help: "Determines which API and client libraries you can use. A Datastore mode database is Standard edition, served through the Datastore API."},
		{Name: "rules", Label: "Security rules", Type: "radio", Options: []string{fsRulesRestrictive, fsRulesOpen}, Default: fsRulesRestrictive,
			OptionLabels:       map[string]string{fsRulesRestrictive: "Restrictive", fsRulesOpen: "Open"},
			UnavailableOptions: map[string]string{fsRulesOpen: fsNoRulesAPI},
			Help:               "Select Restrictive if you don't plan to use the Firebase mobile and web SDKs."},
		{Name: "locationType", Label: "Location type", Type: "radio", Options: []string{fsLocationRegion, fsLocationMultiRegion},
			Default: fsLocationMultiRegion, OptionLabels: map[string]string{fsLocationRegion: "Region", fsLocationMultiRegion: "Multi-region"},
			Help: "Where the database's data is stored. It cannot be changed later."},
		{Name: "multiRegion", Label: "Multi-region", Type: "select", Options: multi, OptionLabels: multiLabels, Default: "nam5",
			ShowWhen: "locationType=" + fsLocationMultiRegion},
		{Name: "region", Label: "Region", Type: "select", Options: regions, OptionLabels: regionLabels, Default: "us-central1",
			ShowWhen: "locationType=" + fsLocationRegion},
		{Name: "encryption", Label: "Encryption options", Type: "radio", Section: "Encryption options",
			Options: []string{fsEncryptionGoogle, fsEncryptionKMS}, Default: fsEncryptionGoogle,
			OptionLabels: map[string]string{fsEncryptionGoogle: "Google-managed encryption key", fsEncryptionKMS: "Cloud KMS key"}},
		// The key, checked against this instance's Cloud KMS (#1881).
		{Name: "kmsKey", Label: "Cloud KMS key", Type: "text", Section: "Encryption options", Pattern: kmsKeyPattern,
			Browse: "kms", ShowWhen: "encryption=" + fsEncryptionKMS,
			Help: "A symmetric key in the database's location: Cloud KMS's us for nam5, europe for eur3, the same region otherwise. " +
				"Disabling or destroying its version makes the database's data inaccessible until it is enabled again."},
		{Name: "pitr", Label: "Enable point-in-time recovery (PITR)", Type: "checkbox", Default: "false", Section: "Disaster recovery",
			Help: "Keeps every version of the documents for 7 days. Stored and returned; the local Firestore reads no earlier version."},
		{Name: "deleteProtection", Label: "Enable delete protection", Type: "checkbox", Default: "false", Section: "Disaster recovery",
			Help: "The database cannot be deleted until delete protection is disabled."},
	}
}

func (firestoreDatabasesProvider) CreateOnPage() bool   { return true }
func (firestoreDatabasesProvider) CreateTitle() string  { return "Create a Firestore database" }
func (firestoreDatabasesProvider) CreateSubmit() string { return "Create Database" }

// createDatabase is the CreateDatabase request the create page's values make.
func createDatabaseRequest(project string, values map[string]string) (*adminpb.CreateDatabaseRequest, error) {
	db := &adminpb.Database{Type: adminpb.Database_FIRESTORE_NATIVE, DatabaseEdition: adminpb.Database_STANDARD}
	switch values["edition"] {
	case "", "STANDARD":
	case "ENTERPRISE":
		db.DatabaseEdition = adminpb.Database_ENTERPRISE
	default:
		return nil, fmt.Errorf("edition %q is not STANDARD or ENTERPRISE", values["edition"])
	}
	switch values["mode"] {
	case "", fsModeNative:
	case fsModeDatastore:
		if db.DatabaseEdition == adminpb.Database_ENTERPRISE {
			return nil, errors.New("a Datastore mode database is Standard edition: choose Standard edition, or Firestore Native")
		}
		db.Type = adminpb.Database_DATASTORE_MODE
	default:
		return nil, fmt.Errorf("data access mode %q is not native or datastore", values["mode"])
	}
	switch values["rules"] {
	case "", fsRulesRestrictive:
	case fsRulesOpen:
		return nil, errors.New(fsNoRulesAPI)
	default:
		return nil, fmt.Errorf("security rules %q are not restrictive or open", values["rules"])
	}
	switch values["encryption"] {
	case "", fsEncryptionGoogle:
	case fsEncryptionKMS:
		key := strings.TrimSpace(values["kmsKey"])
		if key == "" {
			return nil, errors.New("choose the Cloud KMS key to encrypt the database with")
		}
		db.CmekConfig = &adminpb.Database_CmekConfig{KmsKeyName: key}
	default:
		return nil, fmt.Errorf("encryption %q is not google or kms", values["encryption"])
	}
	// The old form's single Location field is still read, for a client of
	// the create route that sends it.
	loc := strings.TrimSpace(values["location"])
	switch values["locationType"] {
	case fsLocationRegion:
		loc = strings.TrimSpace(values["region"])
		if !knownLocation(firestoreRegions, loc) {
			return nil, fmt.Errorf("region %q is not a Firestore region", loc)
		}
	case fsLocationMultiRegion:
		loc = strings.TrimSpace(values["multiRegion"])
		if !knownLocation(firestoreMultiRegions, loc) {
			return nil, fmt.Errorf("multi-region %q is not a Firestore multi-region", loc)
		}
	case "":
		if loc == "" {
			loc = "nam5"
		}
	default:
		return nil, fmt.Errorf("location type %q is not region or multi-region", values["locationType"])
	}
	db.LocationId = loc
	if c := values["concurrency"]; c != "" {
		mode, ok := adminpb.Database_ConcurrencyMode_value[c]
		if !ok {
			return nil, fmt.Errorf("concurrency mode %q is not PESSIMISTIC or OPTIMISTIC", c)
		}
		db.ConcurrencyMode = adminpb.Database_ConcurrencyMode(mode)
	}
	if values["pitr"] == "true" {
		db.PointInTimeRecoveryEnablement = adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED
	}
	if values["deleteProtection"] == "true" {
		db.DeleteProtectionState = adminpb.Database_DELETE_PROTECTION_ENABLED
	}
	return &adminpb.CreateDatabaseRequest{Parent: "projects/" + project,
		DatabaseId: strings.TrimSpace(values["database"]), Database: db}, nil
}

// scheduledBackups is the databases list's Scheduled backups column:
// the database's schedules' recurrences, or None.
func scheduledBackups(ctx context.Context, c *admin.FirestoreAdminClient, db string) string {
	resp, err := c.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: db})
	if err != nil {
		return "Unknown"
	}
	var out []string
	for _, s := range resp.GetBackupSchedules() {
		switch {
		case s.GetDailyRecurrence() != nil:
			out = append(out, "Daily")
		case s.GetWeeklyRecurrence() != nil:
			out = append(out, "Weekly")
		}
	}
	if len(out) == 0 {
		return "None"
	}
	return strings.Join(out, ", ")
}

// ttlPolicies is a database's Time-to-live tab: the TTL policies table,
// each with its status and a Delete.
func (p firestoreDatabasesProvider) ttlPolicies(ctx context.Context, c *admin.FirestoreAdminClient, db, id string) console.Listing {
	list := console.Listing{Columns: []string{"Collection group", "Timestamp field", "Expiration offset", "Status"},
		NameColumn: "Policy", Noun: "TTL policies"}
	it := c.ListFields(ctx, &adminpb.ListFieldsRequest{Parent: db + "/collectionGroups/-", Filter: "ttlConfig:*"})
	for {
		f, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			list.Unavailable = "listing TTL policies: " + err.Error()
			return list
		}
		parts := strings.Split(f.Name, "/")
		cg, field := parts[len(parts)-3], parts[len(parts)-1]
		list.Items = append(list.Items, console.Resource{Name: cg + "." + field, Fields: map[string]string{
			"Collection group":  cg,
			"Timestamp field":   field,
			"Expiration offset": offsetWords(f.GetTtlConfig().GetExpirationOffset()),
			"Status":            enumWord(f.GetTtlConfig().GetState(), ""),
		},
			Actions: ttlActions(),
			ActsOn:  []string{id, firestoreTTLSegment, cg, field},
		})
	}
	list.Total = len(list.Items)
	list.Note = "TTL policies are stored and returned ACTIVE, as the Admin API returns them; the local Firestore " +
		"deletes no expired document."
	return list
}

func offsetWords(d *durationpb.Duration) string {
	if d == nil || d.AsDuration() == 0 {
		return "0"
	}
	v := d.AsDuration()
	for _, u := range []struct {
		n    string
		size time.Duration
	}{{"days", 24 * time.Hour}, {"hours", time.Hour}, {"minutes", time.Minute}} {
		if v%u.size == 0 {
			return strconv.FormatInt(int64(v/u.size), 10) + " " + u.n
		}
	}
	return strconv.FormatInt(int64(v/time.Second), 10) + " seconds"
}

func ttlActions() []console.Action {
	return []console.Action{{ID: "deletettl", Label: "Delete", Destructive: true,
		Confirm: "The TTL policy is removed. The field's index settings are kept."}}
}

func createTTLAction() console.Action {
	return console.Action{ID: "createttl", Label: "Create Policy", Fields: []console.Field{
		{Name: "collectionGroup", Label: "Collection group name", Type: "text", Required: true, Pattern: firestoreCollectionPattern},
		{Name: "field", Label: "Timestamp field name", Type: "text", Required: true},
		{Name: "offset", Label: "Expiration offset (optional)", Type: "text", Default: "0", Pattern: `^[0-9]{0,9}$`},
		{Name: "offsetUnit", Label: "Unit", Type: "select", Options: []string{"days", "hours", "minutes", "seconds"}, Default: "days"},
	}}
}

func ttlOffset(values map[string]string) (*durationpb.Duration, error) {
	raw := strings.TrimSpace(values["offset"])
	if raw == "" || raw == "0" {
		return nil, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("expiration offset %q is not a whole number", raw)
	}
	unit := map[string]time.Duration{"days": 24 * time.Hour, "hours": time.Hour, "minutes": time.Minute, "seconds": time.Second}[values["offsetUnit"]]
	if unit == 0 {
		return nil, fmt.Errorf("unit %q is not days, hours, minutes or seconds", values["offsetUnit"])
	}
	return durationpb.New(time.Duration(n) * unit), nil
}

// actTTL runs Create Policy and a policy's Delete. ok is false for any
// other action.
func actTTL(ctx context.Context, c *admin.FirestoreAdminClient, db string, path []string, action string, values map[string]string) (bool, error) {
	mask := &fieldmaskpb.FieldMask{Paths: []string{"ttl_config"}}
	switch {
	case action == "createttl" && len(path) == 1:
		cg, field := strings.TrimSpace(values["collectionGroup"]), strings.TrimSpace(values["field"])
		if cg == "" || strings.Contains(cg, "/") || field == "" {
			return true, errors.New("a collection group name and a timestamp field name are required")
		}
		off, err := ttlOffset(values)
		if err != nil {
			return true, err
		}
		name := db + "/collectionGroups/" + cg + "/fields/" + field
		cur, err := c.GetField(ctx, &adminpb.GetFieldRequest{Name: name})
		if err != nil {
			return true, err
		}
		if cur.GetTtlConfig() != nil {
			return true, fmt.Errorf("%s in %s already has a TTL policy", field, cg)
		}
		return true, updateField(ctx, c, &adminpb.UpdateFieldRequest{UpdateMask: mask,
			Field: &adminpb.Field{Name: name, TtlConfig: &adminpb.Field_TtlConfig{ExpirationOffset: off}}})
	case action == "deletettl" && len(path) == 4 && path[1] == firestoreTTLSegment:
		return true, updateField(ctx, c, &adminpb.UpdateFieldRequest{UpdateMask: mask,
			Field: &adminpb.Field{Name: db + "/collectionGroups/" + path[2] + "/fields/" + path[3]}})
	}
	return false, nil
}

// The Query builder (Google's Firestore Studio tab): a scope, WHERE and
// ORDER BY clauses, a LIMIT and an aggregation, run against the
// database's documents.

var firestoreQueryOps = []string{"==", "!=", "<", "<=", ">", ">=", "array-contains", "array-contains-any", "in", "not-in"}

// QueryForm implements console.Builder on a database's page only.
func (firestoreDatabasesProvider) QueryForm(path []string) (string, []console.Field) {
	if len(path) != 1 {
		return "", nil
	}
	return "Run", []console.Field{
		{Name: "scope", Label: "Query scope", Type: "radio", Options: []string{"collection", "collectionGroup"}, Default: "collection",
			OptionLabels: map[string]string{"collection": "Collection", "collectionGroup": "Collection group"}},
		{Name: "collection", Label: "Collection path", Type: "text", ShowWhen: "scope=collection",
			Help: "A collection ID, or a subcollection's path, such as users/alice/orders."},
		{Name: "group", Label: "Collection group", Type: "text", ShowWhen: "scope=collectionGroup", Pattern: `^[^\/]*$`,
			Help: "Every collection with this ID, at any depth."},
		{Name: "where", Label: "WHERE", Type: console.GroupFieldType, AddLabel: "Add WHERE clause", Group: []console.Field{
			{Name: "field", Label: "Field", Type: "text"},
			{Name: "op", Label: "Operator", Type: "select", Options: firestoreQueryOps, Default: "=="},
			{Name: "type", Label: "Value type", Type: "select", Options: []string{"string", "number", "boolean", "null", "timestamp"}, Default: "string"},
			{Name: "value", Label: "Value", Type: "text"},
		}},
		{Name: "orderBy", Label: "ORDER BY", Type: console.GroupFieldType, AddLabel: "Add ORDER BY clause", Group: []console.Field{
			{Name: "field", Label: "Field", Type: "text"},
			{Name: "direction", Label: "Direction", Type: "select", Options: []string{"ascending", "descending"}, Default: "ascending"},
		}},
		{Name: "limit", Label: "LIMIT", Type: "text", Default: "50", Pattern: `^[0-9]{0,4}$`,
			Help: fmt.Sprintf("At most %d, where every listing on this console stops.", detailLimit)},
		{Name: "aggregation", Label: "Aggregation", Type: "select", Options: []string{"", "count", "sum", "avg"}, Default: "",
			OptionLabels: map[string]string{"": "None", "count": "COUNT", "sum": "SUM", "avg": "AVG"}},
		{Name: "aggField", Label: "Aggregated field", Type: "text", ShowWhen: "aggregation=sum|avg"},
	}
}

// queryValue is a WHERE clause's value of its chosen type; a list for the
// operators that take one, comma-separated.
func queryValue(kind, raw, op string) (any, error) {
	if op == "in" || op == "not-in" || op == "array-contains-any" {
		var out []any
		for _, part := range strings.Split(raw, ",") {
			v, err := queryValue(kind, strings.TrimSpace(part), "==")
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	switch kind {
	case "", "string":
		return raw, nil
	case "number":
		if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return i, nil
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", raw)
		}
		return f, nil
	case "boolean":
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("%q is not true or false", raw)
		}
		return b, nil
	case "null":
		return nil, nil
	case "timestamp":
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return nil, fmt.Errorf("%q is not an RFC 3339 timestamp, such as 2026-01-02T15:04:05Z", raw)
		}
		return t, nil
	}
	return nil, fmt.Errorf("value type %q is not string, number, boolean, null or timestamp", kind)
}

// firestoreBuilderQuery is the query the Query builder's values describe.
func firestoreBuilderQuery(c *firestore.Client, values map[string]string) (firestore.Query, error) {
	var q firestore.Query
	if values["scope"] == "collectionGroup" {
		id := strings.TrimSpace(values["group"])
		if id == "" || strings.Contains(id, "/") {
			return q, errors.New("a collection group is a collection ID, without a slash")
		}
		q = c.CollectionGroup(id).Query
	} else {
		col, err := firestoreCollection(c, strings.Trim(strings.TrimSpace(values["collection"]), "/"))
		if err != nil || strings.TrimSpace(values["collection"]) == "" {
			return q, errors.New("a collection path is required: a collection ID, or a subcollection's path")
		}
		q = col.Query
	}
	where, err := console.ParseGroup(values["where"])
	if err != nil {
		return q, fmt.Errorf("WHERE: %w", err)
	}
	for _, w := range where {
		field, op := strings.TrimSpace(w["field"]), w["op"]
		if field == "" && strings.TrimSpace(w["value"]) == "" {
			continue // a row added and left empty
		}
		if field == "" {
			return q, errors.New("each WHERE clause needs a field")
		}
		if op == "" {
			op = "=="
		}
		v, err := queryValue(w["type"], w["value"], op)
		if err != nil {
			return q, fmt.Errorf("WHERE %s: %w", field, err)
		}
		q = q.Where(field, op, v)
	}
	order, err := console.ParseGroup(values["orderBy"])
	if err != nil {
		return q, fmt.Errorf("ORDER BY: %w", err)
	}
	for _, o := range order {
		field := strings.TrimSpace(o["field"])
		if field == "" {
			continue
		}
		dir := firestore.Asc
		if o["direction"] == "descending" {
			dir = firestore.Desc
		}
		q = q.OrderBy(field, dir)
	}
	return q.Limit(queryLimit(values["limit"])), nil
}

// Build implements console.Builder: the Query builder's Run.
func (p firestoreDatabasesProvider) Build(ctx context.Context, project string, path []string, values map[string]string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Path", "Fields"}, Noun: "documents", NameColumn: "Document"}
	if project == "" {
		out.Prompt = "Choose a project in the toolbar."
		return out, nil
	}
	if len(path) != 1 {
		return console.Listing{}, errors.New("the Query builder runs against a database")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := firestore.NewClientWithDatabase(ctx, project, path[0], localOpts(p.endpoint)...)
	if err != nil {
		out.Unavailable = "cannot reach Firestore: " + err.Error()
		return out, nil
	}
	defer c.Close()
	q, err := firestoreBuilderQuery(c, values)
	if err != nil {
		return console.Listing{}, err
	}
	switch agg := values["aggregation"]; agg {
	case "":
	case "count", "sum", "avg":
		aq := q.NewAggregationQuery()
		field := strings.TrimSpace(values["aggField"])
		switch agg {
		case "count":
			aq = aq.WithCount("count")
		case "sum", "avg":
			if field == "" {
				return console.Listing{}, fmt.Errorf("%s needs the aggregated field", strings.ToUpper(agg))
			}
			if agg == "sum" {
				aq = aq.WithSum(field, agg)
			} else {
				aq = aq.WithAvg(field, agg)
			}
		}
		res, err := aq.Get(ctx)
		if err != nil {
			return console.Listing{}, err
		}
		v := res[agg]
		return console.Listing{Columns: []string{"Result"}, NameColumn: "Aggregation", Noun: "results", Total: 1,
			Items: []console.Resource{{Name: strings.ToUpper(agg), Fields: map[string]string{"Result": renderAggregate(v)}}}}, nil
	default:
		return console.Listing{}, fmt.Errorf("aggregation %q is not count, sum or avg", agg)
	}
	it := q.Documents(ctx)
	defer it.Stop()
	for {
		doc, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return console.Listing{}, err
		}
		rel := strings.SplitN(doc.Ref.Path, "/documents/", 2)
		out.Items = append(out.Items, console.Resource{Name: doc.Ref.ID,
			Fields: map[string]string{"Path": rel[len(rel)-1], "Fields": flatten(doc.Data())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// renderAggregate is an aggregation's result as text: the client returns
// each as a protobuf Value.
func renderAggregate(v any) string {
	pv, ok := v.(*firestorepb.Value)
	if !ok {
		return fmt.Sprint(v)
	}
	switch x := pv.GetValueType().(type) {
	case *firestorepb.Value_IntegerValue:
		return strconv.FormatInt(x.IntegerValue, 10)
	case *firestorepb.Value_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *firestorepb.Value_NullValue:
		return "null"
	}
	return pv.String()
}

var _ console.Builder = firestoreDatabasesProvider{}
