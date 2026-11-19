package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	dsadmin "cloud.google.com/go/datastore/admin/apiv1"
	dsadminpb "cloud.google.com/go/datastore/admin/apiv1/adminpb"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/metadata"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The Datastore page as Google's console shows a Datastore mode database
// (#1846): beside its entities, Query by GQL on a kind's query tab, the
// database's Indexes, and Import/Export, whose Export and Import run the
// Datastore Admin API the Datastore front serves (#1884). Labels are
// Google's (docs.cloud.google.com/datastore/docs/store-query-data and
// export-import-entities).

// datastoreIndexesSegment addresses the Indexes page. A kind that begins and
// ends with two underscores is Datastore's own, so no kind can be it.
const datastoreIndexesSegment = "__indexes__"

// The query tab's two modes, as Google's Query by kind and Query by GQL.
const (
	datastoreQueryByKind = "Query by kind"
	datastoreQueryByGQL  = "Query by GQL"
)

// Import/Export's Namespace and Kind choices for everything.
const (
	datastoreAllNamespaces = "All Namespaces"
	datastoreAllKinds      = "All Kinds"
)

// datastoreAdmin opens the Datastore Admin API of the Datastore front, and
// ctx with the routing header naming the page's database, which the front
// reads it from.
func (p datastoreProvider) datastoreAdmin(ctx context.Context, project string) (*dsadmin.DatastoreAdminClient, context.Context, error) {
	c, err := dsadmin.NewDatastoreAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return nil, ctx, fmt.Errorf("cannot reach the Datastore Admin API: %w", err)
	}
	params := "project_id=" + url.QueryEscape(project)
	if db := datastoreDatabaseID(ctx); db != "" {
		params += "&database_id=" + url.QueryEscape(db)
	}
	return c, metadata.AppendToOutgoingContext(ctx, "x-goog-request-params", params), nil
}

// datastorePageHref is a route of the Datastore page in the request's
// project and database.
func datastorePageHref(ctx context.Context, project string, path ...string) string {
	q := url.Values{"project": {project}}
	if db := datastoreDatabaseID(ctx); db != "" {
		q.Set(firestoreDatabaseKey, db)
	}
	segs := make([]string, len(path))
	for i, s := range path {
		segs[i] = url.PathEscape(s)
	}
	return "/datastore/" + strings.Join(segs, "/") + "?" + q.Encode()
}

// ListActions implements console.ListActor: Import and Export, as Google's
// Import/Export page has them, and the database's Indexes.
func (p datastoreProvider) ListActions(ctx context.Context, project string) []console.Action {
	if project == "" {
		return nil
	}
	nss, kinds := []string{datastoreAllNamespaces, firestoreDefaultDatabase}, []string{datastoreAllKinds}
	if c, err := p.client(ctx, project); err == nil {
		if names, err := datastoreNamespaces(ctx, c); err == nil {
			nss = append(nss, names...)
		}
		seen := map[string]bool{}
		for _, ns := range nss[1:] {
			if ns == firestoreDefaultDatabase {
				ns = ""
			}
			if ks, err := datastoreKinds(ctx, c, ns); err == nil {
				for _, k := range ks {
					seen[k] = true
				}
			}
		}
		for k := range seen {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds[1:])
		_ = c.Close()
	}
	nsField := console.Field{Name: "namespace", Label: "Namespace", Type: "select", Options: nss, Default: datastoreAllNamespaces,
		Help: "One namespace, or All Namespaces; (default) is the default namespace."}
	kindField := console.Field{Name: "kind", Label: "Kind", Type: "select", Options: kinds, Default: datastoreAllKinds,
		Help: "One kind, or All Kinds."}
	return []console.Action{
		{ID: "indexes", Label: "Indexes", Href: datastorePageHref(ctx, project, datastoreIndexesSegment)},
		{ID: "export", Label: "Export", Fields: []console.Field{nsField, kindField,
			{Name: "destination", Label: "Destination", Type: "text", Required: true, Browse: "storage",
				Pattern: `^gs://[a-z0-9][a-z0-9_.\-]*[a-z0-9](/.*)?$`,
				Help:    "The Cloud Storage bucket, and optionally a folder in it, the export is written to: gs://BUCKET[/PATH]."}}},
		{ID: "import", Label: "Import", Fields: []console.Field{
			{Name: "file", Label: "File", Type: "text", Required: true, Browse: "storage",
				Pattern: `^gs://.+\.overall_export_metadata$`,
				Help:    "An export's .overall_export_metadata file, gs://BUCKET/PATH/NAME.overall_export_metadata. Not an .export_metadata file."},
			nsField, kindField}},
	}
}

// datastoreEntityFilter is Import/Export's Namespace and Kind as the
// Datastore Admin API's entity filter.
func datastoreEntityFilter(values map[string]string) *dsadminpb.EntityFilter {
	f := &dsadminpb.EntityFilter{}
	switch ns := values["namespace"]; ns {
	case "", datastoreAllNamespaces:
	case firestoreDefaultDatabase:
		f.NamespaceIds = []string{""}
	default:
		f.NamespaceIds = []string{ns}
	}
	if k := values["kind"]; k != "" && k != datastoreAllKinds {
		f.Kinds = []string{k}
	}
	return f
}

// datastoreTransfer runs Export or Import and says what it did, as the
// alert Google's Import/Export page shows.
func (p datastoreProvider) datastoreTransfer(ctx context.Context, project, action string, values map[string]string) (*console.Listing, error) {
	if project == "" {
		return nil, errors.New("choose a project first")
	}
	c, actx, err := p.datastoreAdmin(ctx, project)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	out := &console.Listing{Columns: []string{"Value"}, NameColumn: "Field", Noun: "fields"}
	add := func(name, value string) {
		out.Items = append(out.Items, console.Resource{Name: name, Fields: map[string]string{"Value": value}})
	}
	switch action {
	case "export":
		dest := strings.TrimSpace(values["destination"])
		if !strings.HasPrefix(dest, "gs://") {
			return nil, errors.New("the destination is a Cloud Storage bucket: gs://BUCKET[/PATH]")
		}
		op, err := c.ExportEntities(actx, &dsadminpb.ExportEntitiesRequest{ProjectId: project,
			EntityFilter: datastoreEntityFilter(values), OutputUrlPrefix: dest})
		if err != nil {
			return nil, err
		}
		resp, err := op.Wait(actx)
		if err != nil {
			return nil, err
		}
		meta, _ := op.Metadata()
		add("Export", "Completed")
		add("Output URL", resp.GetOutputUrl())
		add("Entities", strconv.FormatInt(meta.GetProgressEntities().GetWorkCompleted(), 10))
	case "import":
		file := strings.TrimSpace(values["file"])
		if !strings.HasSuffix(file, ".overall_export_metadata") {
			return nil, errors.New("choose an export's .overall_export_metadata file, not an .export_metadata file")
		}
		op, err := c.ImportEntities(actx, &dsadminpb.ImportEntitiesRequest{ProjectId: project,
			EntityFilter: datastoreEntityFilter(values), InputUrl: file})
		if err != nil {
			return nil, err
		}
		if err := op.Wait(actx); err != nil {
			return nil, err
		}
		meta, _ := op.Metadata()
		add("Import", "Completed")
		add("File", file)
		add("Entities", strconv.FormatInt(meta.GetProgressEntities().GetWorkCompleted(), 10))
	}
	out.Total = len(out.Items)
	return out, nil
}

// ActAtResult implements console.ResultActor: Export and Import answer with
// what they did; every other action is ActAt's.
func (p datastoreProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) == 0 && (action == "export" || action == "import") {
		return p.datastoreTransfer(ctx, project, action, values)
	}
	if len(path) == 1 && path[0] == datastoreIndexesSegment && action == "deleteindex" {
		return nil, p.deleteDatastoreIndex(ctx, project, values["index"])
	}
	return nil, p.ActAt(ctx, project, path, action, values)
}

// datastoreIndexProperties is an index's properties as Google's index.yaml
// writes them: name and direction, in order.
func datastoreIndexProperties(ix *dsadminpb.Index) string {
	var out []string
	for _, pr := range ix.GetProperties() {
		dir := "Ascending"
		if pr.GetDirection() == dsadminpb.Index_DESCENDING {
			dir = "Descending"
		}
		out = append(out, pr.GetName()+" ("+dir+")")
	}
	return strings.Join(out, ", ")
}

// datastoreIndexState is an index's state as the console words it.
func datastoreIndexState(s dsadminpb.Index_State) string {
	switch s {
	case dsadminpb.Index_READY:
		return "Serving"
	case dsadminpb.Index_CREATING:
		return "Building"
	case dsadminpb.Index_DELETING:
		return "Deleting"
	case dsadminpb.Index_ERROR:
		return "Error"
	}
	return "Unknown"
}

// indexesDetail is the database's Indexes: its composite indexes, from the
// Datastore Admin API.
func (p datastoreProvider) indexesDetail(ctx context.Context, project string) (console.Detail, error) {
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	list := console.Listing{Columns: []string{"Kind", "Properties", "Ancestor", "Status"}, NameColumn: "Index ID", Noun: "indexes"}
	c, actx, err := p.datastoreAdmin(ctx, project)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer c.Close()
	it := c.ListIndexes(actx, &dsadminpb.ListIndexesRequest{ProjectId: project})
	for {
		ix, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return console.Detail{Unavailable: "listing indexes: " + err.Error()}, nil
		}
		anc := "None"
		if ix.GetAncestor() == dsadminpb.Index_ALL_ANCESTORS {
			anc = "All ancestors"
		}
		list.Items = append(list.Items, console.Resource{Name: ix.GetIndexId(), Fields: map[string]string{
			"Kind": ix.GetKind(), "Properties": datastoreIndexProperties(ix), "Ancestor": anc,
			"Status": datastoreIndexState(ix.GetState())}})
	}
	list.Total = len(list.Items)
	list.Note = "Composite indexes, which a query on more than one property needs. Datastore indexes every " +
		"property on its own unless it is excluded from indexes. Composite indexes are created from an " +
		"index.yaml file with gcloud datastore indexes create, or through the Datastore Admin API."
	d := singleSection("indexes", "Indexes", list,
		[]console.Property{{Label: "Database", Value: datastoreDatabaseIn(ctx)}})
	d.Title = "Indexes"
	return d, nil
}

// deleteDatastoreIndex deletes a composite index by its ID.
func (p datastoreProvider) deleteDatastoreIndex(ctx context.Context, project, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("an index ID is required")
	}
	c, actx, err := p.datastoreAdmin(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	op, err := c.DeleteIndex(actx, &dsadminpb.DeleteIndexRequest{ProjectId: project, IndexId: strings.TrimSpace(id)})
	if err != nil {
		return err
	}
	_, err = op.Wait(actx)
	return err
}

// datastoreGQLFields are the query tab's Query by GQL controls.
func datastoreGQLFields(kind string) []console.Field {
	def := ""
	if kind != "" {
		def = "SELECT * FROM `" + strings.ReplaceAll(kind, "`", "``") + "`"
	}
	return []console.Field{{Name: "gql", Label: "GQL query", Type: "textarea", Default: def,
		ShowWhen: "queryBy=" + datastoreQueryByGQL,
		Help: "A GQL query in this namespace, such as SELECT * FROM Task WHERE done = false ORDER BY priority DESC LIMIT 10, " +
			"or an aggregation, such as SELECT COUNT(*) FROM Task or AGGREGATE SUM(hours) OVER (SELECT * FROM Task)."}}
}

// datastoreGQLAggregation matches GQL that is an aggregation query.
var datastoreGQLAggregation = regexp.MustCompile(`(?is)^\s*(AGGREGATE\s|SELECT\s+(COUNT|COUNT_UP_TO|SUM|AVG)\s*\()`)

// buildGQL runs a Query by GQL in the namespace: entities, or an
// aggregation's values.
func (p datastoreProvider) buildGQL(ctx context.Context, c datastorepb.DatastoreClient, project string, scope datastoreScope, gql string) (console.Listing, error) {
	gql = strings.TrimSpace(gql)
	if gql == "" {
		return console.Listing{}, errors.New("a GQL query is required")
	}
	part := &datastorepb.PartitionId{ProjectId: project, NamespaceId: scope.ns}
	q := &datastorepb.GqlQuery{QueryString: gql, AllowLiterals: true}
	if datastoreGQLAggregation.MatchString(gql) {
		resp, err := c.RunAggregationQuery(ctx, &datastorepb.RunAggregationQueryRequest{ProjectId: project, PartitionId: part,
			QueryType: &datastorepb.RunAggregationQueryRequest_GqlQuery{GqlQuery: q}})
		if err != nil {
			return console.Listing{}, err
		}
		out := console.Listing{Columns: []string{"Value"}, NameColumn: "Aggregation", Noun: "aggregations"}
		for _, r := range resp.GetBatch().GetAggregationResults() {
			names := make([]string, 0, len(r.GetAggregateProperties()))
			for n := range r.GetAggregateProperties() {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				out.Items = append(out.Items, console.Resource{Name: n,
					Fields: map[string]string{"Value": datastoreAggregateValue(r.GetAggregateProperties()[n])}})
			}
		}
		out.Total = len(out.Items)
		return out, nil
	}
	out := console.Listing{Columns: append([]string{"Kind"}, datastoreEntityColumns...), Noun: "entities",
		NameColumn: datastoreNameIDColumn, RowsOpenable: true}
	resp, err := c.RunQuery(ctx, &datastorepb.RunQueryRequest{ProjectId: project, PartitionId: part,
		QueryType: &datastorepb.RunQueryRequest_GqlQuery{GqlQuery: q}})
	if err != nil {
		return console.Listing{}, err
	}
	for _, r := range resp.GetBatch().GetEntityResults() {
		if len(out.Items) >= detailLimit {
			break
		}
		k, props := datastoreEntityProperties(r.GetEntity(), homeOf(ctx, project))
		row := datastoreEntityRow(scope, k.Kind, k, datastorePropertiesCell(props))
		row.Fields["Kind"] = k.Kind
		out.Items = append(out.Items, row)
	}
	out.Total = len(out.Items)
	return out, nil
}

// datastoreAggregateValue renders an aggregation's value.
func datastoreAggregateValue(v *datastorepb.Value) string {
	switch x := v.GetValueType().(type) {
	case *datastorepb.Value_IntegerValue:
		return strconv.FormatInt(x.IntegerValue, 10)
	case *datastorepb.Value_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *datastorepb.Value_NullValue:
		return "null"
	}
	return fmt.Sprint(v)
}
