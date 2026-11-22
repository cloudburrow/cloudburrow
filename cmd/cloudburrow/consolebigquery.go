package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/nbruntime"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// --- BigQuery -------------------------------------------------------------
//
// Through the official client against the forwarded REST port: datasets,
// their tables, a table's schema and first rows, and a bounded query editor
// (#698). Datasets, tables and rows are created, edited and deleted with the
// forms in consolebigqueryedit.go (#854, #994). The query editor reads by
// default, so a statement typed there never writes unless its Read-write mode
// was chosen and the run confirmed (consolebigquerywrite.go, #994).
//
// No creation, modification or expiry time, and no expiration setting, is
// shown anywhere. The emulator
// writes them in seconds where the API specifies milliseconds (measured,
// #698: a table created today reads back as January 1970 through the official
// client), and a date that is confidently wrong is worse than none.
//
// BigQuery serves every project of the instance's Resource Manager (#1764),
// and answers 404 "project P is not found" for any other. A screen that
// passed that 404 through would read as "broken", and one that showed an
// empty table would read as "you have no datasets". So such a project gets
// a prompt that says why.

type bigqueryProvider struct {
	// endpoint is the forwarded REST address, host:port.
	endpoint string
	// project is the project the provider reads: the instance's default,
	// and in each call the one chosen in the toolbar (in).
	project string
	// registry is Resource Manager's, for a dataset's and a table's tags
	// (consolebigquerytags.go, #1780); nil leaves them out.
	registry *resourcemanager.Registry
	// savedQueries is the record of Studio's saved queries in the
	// instance's state directory (consolebigquerysavedqueries.go, #1861).
	savedQueries string
	// notebooks are Studio's notebook runtimes
	// (consolebigquerynotebooks.go, #2094); nil when none can run.
	notebooks *nbruntime.Manager
}

func (bigqueryProvider) ID() string    { return "bigquery" }
func (bigqueryProvider) Title() string { return "BigQuery" }

// in is p reading project, the one chosen in the toolbar (#1764).
func (p bigqueryProvider) in(project string) bigqueryProvider {
	if project != "" {
		p.project = project
	}
	return p
}

// notServed is the explicit state for a project BigQuery does not serve:
// one the instance's Resource Manager does not have ACTIVE.
func notServed(project string) string {
	return fmt.Sprintf("BigQuery serves the projects of the instance's Resource Manager, and %q is not one of them "+
		"(or is not ACTIVE), so it has nothing to show here. Create it in Resource Manager, or choose another in the toolbar.", project)
}

// projectNotFound reports whether err is BigQuery's 404 for a project it
// does not serve.
func projectNotFound(err error, project string) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code == http.StatusNotFound && strings.Contains(ge.Message, "project "+project+" is not found")
}

// scope reports the prompt a project needs before anything is read, or "".
func (p bigqueryProvider) scope(project string) string {
	switch {
	case project == "":
		return "BigQuery holds datasets per project. Choose one in the toolbar."
	}
	return ""
}

func (p bigqueryProvider) client(ctx context.Context) (*bigquery.Client, error) {
	c, err := bigquery.NewClient(ctx, p.project,
		option.WithEndpoint("http://"+p.endpoint), option.WithoutAuthentication())
	if err != nil {
		return nil, fmt.Errorf("cannot reach BigQuery: %w", err)
	}
	return c, nil
}

func (p bigqueryProvider) List(ctx context.Context, project string) (console.Listing, error) {
	p = p.in(project) // #1764: the project chosen in the toolbar
	base := console.Listing{
		Columns: []string{"Tables", "Location", "Description"},
		Noun:    "datasets", NameColumn: "Dataset",
		RowsOpenable: true,
	}
	if prompt := p.scope(project); prompt != "" {
		base.Prompt = prompt
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

	var items []console.Resource
	datasets := c.Datasets(ctx)
	for {
		ds, err := datasets.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if projectNotFound(err, project) {
			base.Prompt = notServed(project)
			return base, nil
		}
		if err != nil {
			base.Unavailable = "listing datasets: " + err.Error()
			return base, nil
		}
		fields := map[string]string{"Tables": "—", "Location": "—", "Description": "—"}
		if md, err := ds.Metadata(ctx); err == nil {
			fields["Location"] = orDash(md.Location)
			fields["Description"] = orDash(md.Description)
		}
		// The table count, because "a dataset exists" and "a dataset holds
		// something" are different facts and only the second is useful.
		if n, err := countTables(ctx, ds); err == nil {
			fields["Tables"] = fmt.Sprint(n)
		}
		items = append(items, console.Resource{Name: ds.DatasetID, Fields: fields})
		if len(items) >= detailLimit {
			base.Note = truncatedNote(len(items), "datasets")
			break
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	base.Items, base.Total = items, len(items)
	base.Summary = p.projectSummary(ctx, project)
	if base.Note == "" {
		base.Note = "In-memory: everything here is gone when the instance restarts."
	}
	return base, nil
}

// projectSummary is what the datasets list says about its project: the
// project and the BigQuery service account projects.getServiceAccount names,
// the account a customer-managed encryption key is granted to (#1270), as
// Cloud Storage's Settings page names its own. A failed read says so in the
// value rather than hiding the datasets.
func (p bigqueryProvider) projectSummary(ctx context.Context, project string) []console.PropertyGroup {
	email := ""
	if svc, err := p.service(ctx); err != nil {
		email = "unavailable: " + err.Error()
	} else if sa, err := svc.Projects.GetServiceAccount(project).Context(ctx).Do(); err != nil {
		email = "unavailable: " + bigqueryRefusal(err).Error()
	} else {
		email = sa.Email
	}
	return []console.PropertyGroup{{Heading: "Project", Properties: []console.Property{
		{Label: "Project ID", Value: project},
		{Label: "BigQuery service account", Value: email},
	}}, p.connectionsSummary(ctx, project)}
}

func countTables(ctx context.Context, ds *bigquery.Dataset) (int, error) {
	tables := ds.Tables(ctx)
	n := 0
	for {
		if _, err := tables.Next(); errors.Is(err, iterator.Done) {
			return n, nil
		} else if err != nil {
			return 0, err
		}
		n++
	}
}

// Detail opens a dataset (its tables and routines), a table (its schema,
// details and first rows) or a routine (consolebigqueryroutines.go).
func (p bigqueryProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	p = p.in(project) // #1764: the project chosen in the toolbar
	if prompt := p.scope(project); prompt != "" {
		return console.Detail{Prompt: prompt}, nil
	}
	if isConnectionPath(path) { // consolebigqueryconnectionpage.go, #1770
		return p.connectionDetail(ctx, project, path[1], path[2])
	}
	d, err := p.detailAt(ctx, path)
	if err == nil && strings.Contains(d.Unavailable, "project "+project+" is not found") {
		return console.Detail{Prompt: notServed(project)}, nil // #1764
	}
	if err == nil {
		d = p.withTags(d, path) // consolebigquerytags.go, #1780
	}
	return d, err
}

func (p bigqueryProvider) detailAt(ctx context.Context, path []string) (console.Detail, error) {
	switch len(path) {
	case 1:
		return p.datasetDetail(ctx, path[0])
	case 2:
		return p.tableDetail(ctx, path[0], path[1])
	case 3:
		if isRoutinePath(path) {
			return p.routineDetail(ctx, path[0], path[2])
		}
		if isModelPath(path) { // #1465, consolebigquerymodels.go
			return p.modelDetail(ctx, path[0], path[2])
		}
	}
	return console.DeeperThan(2, path), nil
}

func (p bigqueryProvider) datasetDetail(ctx context.Context, datasetID string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer c.Close()

	ds := c.Dataset(datasetID)
	md, err := ds.Metadata(ctx)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the dataset: " + err.Error()}, nil
	}

	tables := console.Listing{
		Columns: []string{"Type", "Rows", "Fields"}, NameColumn: "Table", Noun: "tables",
		RowsOpenable: true,
	}
	it := ds.Tables(ctx)
	for {
		t, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			tables.Unavailable = "listing tables: " + err.Error()
			break
		}
		fields := map[string]string{"Type": "—", "Rows": "—", "Fields": "—"}
		if tm, err := t.Metadata(ctx); err == nil {
			fields["Type"] = orDash(string(tm.Type))
			fields["Rows"] = fmt.Sprint(tm.NumRows)
			fields["Fields"] = fmt.Sprint(len(tm.Schema))
		}
		tables.Items = append(tables.Items, console.Resource{Name: t.TableID, Fields: fields})
		if len(tables.Items) >= detailLimit {
			tables.Note = truncatedNote(len(tables.Items), "tables")
			break
		}
	}
	sort.SliceStable(tables.Items, func(i, j int) bool { return tables.Items[i].Name < tables.Items[j].Name })
	tables.Total = len(tables.Items)

	props := []console.Property{
		{Label: "Dataset ID", Value: p.project + "." + datasetID},
		{Label: "Location", Value: orDash(md.Location)},
	}
	if md.Description != "" {
		props = append(props, console.Property{Label: "Description", Value: md.Description})
	}
	if len(md.Labels) > 0 {
		props = append(props, console.Property{Label: "Labels", Value: formatLabels(md.Labels)})
	}
	props = append(props, datasetSettingProps(md)...)

	return console.Detail{
		Summary: []console.Property{
			{Label: "Tables", Value: fmt.Sprint(tables.Total)},
			{Label: "Location", Value: orDash(md.Location)},
		},
		Sections: []console.Section{
			// Google's dataset page opens on Overview, which lists the
			// dataset's tables, routines and models (#1861,
			// https://docs.cloud.google.com/bigquery/docs/bigquery-web-ui):
			// one tab holding the three, each on its own strip inside it.
			// The ids stay "tables", "routines" and "models", which
			// addresses and tests name.
			overviewSection(console.Section{ID: "tables", Label: "Tables", Listing: tables}),
			overviewSection(p.routinesSection(ctx, ds)),
			overviewSection(p.modelsSection(ctx, ds)), // #1465
			{
				ID: "details", Label: "Details", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{Heading: "Dataset", Properties: props}},
			},
		},
	}, nil
}

// overviewSection puts a dataset's listing under its Overview tab.
func overviewSection(s console.Section) console.Section {
	s.Group = "Overview"
	return s
}

func (p bigqueryProvider) tableDetail(ctx context.Context, datasetID, tableID string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer c.Close()

	t := c.Dataset(datasetID).Table(tableID)
	md, err := t.Metadata(ctx)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the table: " + err.Error()}, nil
	}

	schema := console.Listing{
		Columns: []string{"Type", "Mode", "Description"}, NameColumn: "Field", Noun: "fields",
	}
	schema.Items = schemaRows(md.Schema, "")
	schemaNote := ""
	schema.Total = len(schema.Items)
	if schema.Total == 0 {
		schema.Note = "This table has no schema."
	} else if tags, err := p.readGovernanceTags(ctx, datasetID, tableID); err == nil && len(tags) > 0 {
		// Each field's data governance tags (#1456,
		// consolebigquerygovernance.go).
		schema.Columns = append(schema.Columns, governanceTagsColumn)
		for i := range schema.Items {
			schema.Items[i].Fields[governanceTagsColumn] = orDash(tags[schema.Items[i].Name])
		}
		schemaNote = governanceTagsNote
	}

	props := []console.Property{
		{Label: "Table ID", Value: p.project + "." + datasetID + "." + tableID},
		{Label: "Type", Value: orDash(string(md.Type))},
		{Label: "Rows", Value: fmt.Sprint(md.NumRows)},
		{Label: "Size", Value: fmt.Sprintf("%d bytes", md.NumBytes)},
	}
	if md.Description != "" {
		props = append(props, console.Property{Label: "Description", Value: md.Description})
	}
	if tp := md.TimePartitioning; tp != nil {
		v := string(tp.Type)
		if tp.Field != "" {
			v += " on " + tp.Field
		}
		props = append(props, console.Property{Label: "Partitioning", Value: v})
	}
	if rp := md.RangePartitioning; rp != nil && rp.Range != nil {
		// Integer-range partitioning, as CREATE TABLE ... PARTITION BY
		// RANGE_BUCKET makes it (#1272).
		props = append(props, console.Property{Label: "Partitioning", Value: fmt.Sprintf(
			"RANGE_BUCKET on %s: %d to %d, interval %d", rp.Field, rp.Range.Start, rp.Range.End, rp.Range.Interval)})
	}
	if cl := md.Clustering; cl != nil && len(cl.Fields) > 0 {
		props = append(props, console.Property{Label: "Clustered by", Value: strings.Join(cl.Fields, ", ")})
	}
	if md.ViewQuery != "" {
		props = append(props, console.Property{Label: "View query", Value: md.ViewQuery})
	}
	props = append(props, materializedViewProps(md)...) // #1390, consolebigquerymv.go
	props = append(props, objectTableProps(md)...)      // #1783, consolebigqueryobjects.go
	if len(md.Labels) > 0 {
		props = append(props, console.Property{Label: "Labels", Value: formatLabels(md.Labels)})
	}

	groups := []console.PropertyGroup{{Heading: "Table", Properties: props}}
	if !strings.Contains(tableID, "$") {
		// Its analysis rules (#1580, consolebigqueryprivacy.go).
		if g, ok := p.analysisRulesGroup(ctx, c, datasetID, tableID, md.Type == bigquery.ViewTable); ok {
			groups = append(groups, g)
		}
	}

	detail := console.Detail{
		Summary: []console.Property{
			{Label: "Dataset", Value: datasetID},
			{Label: "Type", Value: orDash(string(md.Type))},
			{Label: "Rows", Value: fmt.Sprint(md.NumRows)},
			{Label: "Fields", Value: fmt.Sprint(len(md.Schema))},
		},
		Sections: []console.Section{
			{ID: "schema", Label: "Schema", Listing: schema, Note: schemaNote},
			{
				ID: "details", Label: "Details", Kind: console.KindProperties,
				Groups: groups,
			},
			p.previewSection(ctx, t, md.Schema),
		},
	}
	if isObjectTable(md) { // #1783: tabledata.list does not read an external table
		detail.Sections[2] = p.objectPreviewSection(ctx, c, datasetID, tableID, md.Schema)
		detail.Sections[0].Note = "An object table's schema is fixed: one row per Cloud Storage object."
		return detail, nil
	}
	if base, part, ok := strings.Cut(tableID, "$"); ok {
		// A partition's page (#1273): the table's, by the decorated name,
		// its Preview the partition's rows; it leads back to the table.
		detail.Trail = []console.Crumb{{Label: datasetID, Path: []string{datasetID}},
			{Label: base, Path: []string{datasetID, base}}, {Label: part}}
		return detail, nil
	}
	// Lineage, Data profile and Data quality follow Preview, as on Google's
	// table page (#1861). Each is another service's (the Data Lineage API,
	// Dataplex's profile and quality scans), none of which CloudBurrow runs.
	detail.Sections = append(detail.Sections, bigqueryUnservedTabs()...)
	if sec, ok := p.partitionsSection(ctx, c, datasetID, tableID, md); ok {
		detail.Sections = append(detail.Sections, sec)
	}
	if md.Type != bigquery.ViewTable && md.Type != bigquery.MaterializedView {
		// A view has no row access policies of its own (#1218).
		detail.Sections = append(detail.Sections, p.rowPoliciesSection(ctx, datasetID, tableID))
	}
	return detail, nil
}

// schemaRows flattens a schema, naming a nested field by its dotted path, the
// way the BigQuery console's schema tab indents it.
func schemaRows(s bigquery.Schema, prefix string) []console.Resource {
	var out []console.Resource
	for _, f := range s {
		mode := "NULLABLE"
		switch {
		case f.Repeated:
			mode = "REPEATED"
		case f.Required:
			mode = "REQUIRED"
		}
		out = append(out, console.Resource{
			Name: prefix + f.Name,
			Fields: map[string]string{
				"Type": string(f.Type), "Mode": mode, "Description": orDash(f.Description),
			},
		})
		if len(f.Schema) > 0 {
			out = append(out, schemaRows(f.Schema, prefix+f.Name+".")...)
		}
	}
	return out
}

// previewSection is the table's first rows, read with tabledata.list, which
// is what the real console's Preview tab reads: it runs no query.
func (p bigqueryProvider) previewSection(ctx context.Context, t *bigquery.Table, schema bigquery.Schema) console.Section {
	sec := console.Section{ID: "preview", Label: "Preview"}
	it := t.Read(ctx)
	listing, err := readRows(it, schema)
	if err != nil {
		sec.Unavailable = "reading rows: " + err.Error()
		return sec
	}
	sec.Listing = listing
	return sec
}

// readRows renders at most detailLimit rows of an iterator as a listing whose
// columns are the result's own.
func readRows(it *bigquery.RowIterator, schema bigquery.Schema) (console.Listing, error) {
	out := console.Listing{Noun: "rows"}
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			// The service's own message, which names the syntax error's
			// position.
			return console.Listing{}, err
		}
		if schema == nil {
			schema = it.Schema
		}
		if out.Columns == nil {
			if len(schema) == 0 {
				return console.Listing{}, fmt.Errorf("the result has no columns")
			}
			// The names come from the result's schema, not from the
			// statement: an expression without an alias gets whatever name
			// BigQuery gives it, and guessing would label the wrong column.
			out.NameColumn = schema[0].Name
			for _, f := range schema[1:] {
				out.Columns = append(out.Columns, f.Name)
			}
		}
		item := console.Resource{Fields: map[string]string{}}
		for i, v := range row {
			var f *bigquery.FieldSchema
			if i < len(schema) {
				f = schema[i]
			}
			text := renderBigQueryValue(v, f)
			if i == 0 {
				item.Name = text
				continue
			}
			if f != nil {
				item.Fields[f.Name] = text
			}
		}
		out.Items = append(out.Items, item)
		if len(out.Items) >= detailLimit {
			out.Note = truncatedNote(len(out.Items), "rows")
			break
		}
	}
	if schema == nil {
		schema = it.Schema
	}
	if out.Columns == nil {
		out.Columns = []string{}
		if out.NameColumn == "" {
			if len(schema) > 0 {
				out.NameColumn = schema[0].Name
				for _, f := range schema[1:] {
					out.Columns = append(out.Columns, f.Name)
				}
			} else {
				out.NameColumn = "Result"
			}
		}
	}
	out.Total = len(out.Items)
	return out, nil
}

// renderBigQueryValue prints a result cell.
//
// NULL is an em dash, as everywhere else in this console, so it is not
// mistaken for the empty string. A RECORD or an ARRAY is JSON with the
// schema's field names, because Go's %v of a []bigquery.Value is a list of
// values with the names thrown away.
func renderBigQueryValue(v bigquery.Value, f *bigquery.FieldSchema) string {
	if v == nil {
		return "—"
	}
	switch v.(type) {
	case []bigquery.Value:
		encoded, err := json.Marshal(jsonBigQueryValue(v, f))
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(encoded)
	}
	s, _ := jsonBigQueryValue(v, f).(string)
	return s
}

// jsonBigQueryValue converts a value to what encoding/json renders readably.
// Scalars become their display strings; records become objects keyed by field
// name; arrays become arrays.
func jsonBigQueryValue(v bigquery.Value, f *bigquery.FieldSchema) any {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case []bigquery.Value:
		if f != nil && f.Repeated {
			elem := *f
			elem.Repeated = false
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = jsonBigQueryValue(e, &elem)
			}
			return out
		}
		if f != nil && len(f.Schema) > 0 {
			obj := make(map[string]any, len(x))
			for i, e := range x {
				if i < len(f.Schema) {
					obj[f.Schema[i].Name] = jsonBigQueryValue(e, f.Schema[i])
				}
			}
			return obj
		}
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonBigQueryValue(e, nil)
		}
		return out
	case *big.Rat:
		if f != nil && f.Type == bigquery.BigNumericFieldType {
			return bigquery.BigNumericString(x)
		}
		return bigquery.NumericString(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		return base64.StdEncoding.EncodeToString(x)
	default:
		return fmt.Sprint(x)
	}
}

func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + labels[k]
	}
	return strings.Join(parts, ", ")
}

// QueryHint is the editor's contract with the user.
func (bigqueryProvider) QueryHint() string {
	return fmt.Sprintf("Read-only GoogleSQL: one SELECT (or WITH … SELECT) statement. "+
		"BigQuery has no read-only transaction, so this console refuses anything else "+
		"before sending it; DDL and scripts run in Read-write mode. "+
		"Unqualified table names resolve in this dataset. At most %d "+
		"rows are shown, and a query stops after %s.", detailLimit, dbTimeout)
}

// Query runs one read-only statement with the dataset on the path as the
// default dataset.
//
// Spanner and Cloud SQL enforce read-only with a read-only transaction, so the
// server is the authority. BigQuery has none, and the emulator's dry run
// reports statementType SELECT for a DELETE and a CREATE TABLE alike
// (measured, #698), so it cannot be the authority either. The check is
// therefore lexical, and deliberately an allowlist: the statement must begin
// with SELECT or WITH and be a single statement. In GoogleSQL neither keyword
// can begin a statement that writes — DML and DDL begin with their own verbs —
// so this refuses every write rather than trying to recognise each one.
func (p bigqueryProvider) Query(ctx context.Context, project string, path []string, statement string) (console.Listing, error) {
	p = p.in(project) // #1764: the project chosen in the toolbar
	if project == "" {
		return console.Listing{Prompt: "Choose a project in the toolbar."}, nil
	}
	if len(path) == 0 {
		return console.Listing{}, fmt.Errorf("a BigQuery query runs in a dataset: open one first")
	}
	if err := readOnlyBigQuery(statement); err != nil {
		return console.Listing{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return console.Listing{}, err
	}
	defer c.Close()

	q := c.Query(statement)
	q.DefaultProjectID = p.project
	q.DefaultDatasetID = path[0]
	// Read, with nothing set that forces a job: the client then sends
	// jobs.query, which leaves no job-named dataset behind in the emulator,
	// where jobs.insert creates one per query (measured, #698).
	it, err := q.Read(ctx)
	if err != nil {
		return console.Listing{}, err
	}
	return readRows(it, nil)
}

// readOnlyBigQuery refuses a statement that is not a single SELECT.
//
// It skips comments and quoted text, so a keyword or semicolon inside a string
// literal, a quoted identifier or a comment is not mistaken for a second
// statement.
func readOnlyBigQuery(statement string) error {
	tokens, err := bigQueryTokens(statement)
	if err != nil {
		return err
	}
	// Leading parentheses are allowed: "(SELECT 1) UNION ALL (SELECT 2)".
	i := 0
	for i < len(tokens) && tokens[i] == "(" {
		i++
	}
	if i == len(tokens) {
		return errors.New("a statement is required")
	}
	first := strings.ToUpper(tokens[i])
	if first != "SELECT" && first != "WITH" {
		return fmt.Errorf("the BigQuery editor is read-only: only a SELECT or WITH … SELECT statement runs here, not %s. "+
			"Switch it to Read-write to run DDL or a script", first)
	}
	for j, tok := range tokens {
		if tok == ";" {
			for _, rest := range tokens[j+1:] {
				if rest != ";" {
					return errors.New("the BigQuery editor runs one statement at a time: remove everything after the first semicolon")
				}
			}
			break
		}
	}
	return nil
}

// bigQueryTokens splits a statement into the words and punctuation that
// matter to readOnlyBigQuery, writableBigQuery and classifySpannerStatement:
// identifiers and keywords, "(", ")" (so a clause inside parentheses can be
// told from the statement's own), ";", "@", "{" and "}". Quoted
// text of every GoogleSQL form, and comments, are consumed and dropped.
func bigQueryTokens(s string) ([]string, error) {
	var out []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '#' || (c == '-' && strings.HasPrefix(s[i:], "--")):
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				return out, nil
			}
			i += end + 1
		case c == '/' && strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, errors.New("unterminated comment")
			}
			i += 2 + end + 2
		case c == '\'' || c == '"' || c == '`':
			n, err := skipQuoted(s[i:])
			if err != nil {
				return nil, err
			}
			i += n
		case isStringPrefix(c) && i+1 < len(s):
			// A raw or bytes prefix: r'…', b"…", rb'…', br"…".
			j := i + 1
			if j < len(s) && isStringPrefix(s[j]) && (s[j]|0x20) != (c|0x20) {
				j++
			}
			if j < len(s) && (s[j] == '\'' || s[j] == '"') {
				n, err := skipQuoted(s[j:])
				if err != nil {
					return nil, err
				}
				i = j + n
				continue
			}
			n := wordLen(s[i:])
			out = append(out, s[i:i+n])
			i += n
		case c == '(' || c == ')' || c == ';' || c == '@' || c == '{' || c == '}':
			// "@", "{" and "}" for a Spanner statement hint, "@{…} UPDATE",
			// which classifySpannerStatement steps over.
			out = append(out, string(c))
			i++
		case isWordByte(c):
			n := wordLen(s[i:])
			out = append(out, s[i:i+n])
			i += n
		default:
			i++
		}
	}
	return out, nil
}

func isStringPrefix(c byte) bool { return c == 'r' || c == 'R' || c == 'b' || c == 'B' }

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func wordLen(s string) int {
	n := 0
	for n < len(s) && isWordByte(s[n]) {
		n++
	}
	return n
}

// skipQuoted returns the length of the quoted text at the start of s, which
// begins with its quote character: a single, double or backtick quote, or
// three single or three double quotes for a triple-quoted string. A backslash
// always protects the byte after it from ending the text: a raw string keeps
// the backslash rather than interpreting it, but an escaped quote still does
// not end it, and a quoted identifier takes escapes too.
func skipQuoted(s string) (int, error) {
	q := s[0]
	delim := string(q)
	if q != '`' && strings.HasPrefix(s, strings.Repeat(delim, 3)) {
		delim = strings.Repeat(delim, 3)
	}
	for i := len(delim); i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if strings.HasPrefix(s[i:], delim) {
			return i + len(delim), nil
		}
	}
	return 0, errors.New("unterminated quoted text")
}

var (
	_ console.Driller         = bigqueryProvider{}
	_ console.Executor        = bigqueryProvider{}
	_ console.StatementWriter = bigqueryProvider{}
)

// bigqueryUnservedTabs are the table page's tabs whose service CloudBurrow
// does not run, in Google's place and saying so (#1861).
func bigqueryUnservedTabs() []console.Section {
	return []console.Section{
		{ID: "lineage", Label: "Lineage", Unavailable: "Not available locally: lineage is recorded by the Data Lineage API, " +
			"which CloudBurrow does not run. A table's source jobs are listed in Job history."},
		{ID: "profile", Label: "Data profile", Unavailable: "Not available locally: a data profile is a Dataplex data profile scan, " +
			"which CloudBurrow does not run. Query the table for its column statistics instead."},
		{ID: "quality", Label: "Data quality", Unavailable: "Not available locally: data quality is a Dataplex data quality scan, " +
			"which CloudBurrow does not run."},
	}
}

// datasetSettingProps are the settings Create dataset sets, as Google's
// Details names them (#1861): durations, which the front keeps as sent.
func datasetSettingProps(md *bigquery.DatasetMetadata) []console.Property {
	expiration := "Never"
	if md.DefaultTableExpiration > 0 {
		expiration = fmt.Sprintf("%g days", md.DefaultTableExpiration.Hours()/24)
	}
	travel := "7 days"
	if md.MaxTimeTravel > 0 {
		travel = fmt.Sprintf("%g days", md.MaxTimeTravel.Hours()/24)
	}
	billing := "Logical"
	if md.StorageBillingModel == "PHYSICAL" {
		billing = "Physical"
	}
	return []console.Property{
		{Label: "Default table expiration", Value: expiration},
		{Label: "Default collation", Value: orDash(md.DefaultCollation)},
		{Label: "Case insensitive", Value: fmt.Sprint(md.IsCaseInsensitive)},
		{Label: "Time travel window", Value: travel},
		{Label: "Storage billing model", Value: billing},
	}
}
