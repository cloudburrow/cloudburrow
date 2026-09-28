package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// --- BigQuery -------------------------------------------------------------
//
// Through the official client against the forwarded REST port: datasets,
// their tables, a table's schema and first rows, and a bounded query editor
// (#698). Datasets, tables and rows are created and deleted with the forms in
// consolebigqueryedit.go (#854); the query editor stays read-only, so a
// statement typed there never writes.
//
// No creation, modification or expiry time, and no expiration setting, is
// shown anywhere. The emulator
// writes them in seconds where the API specifies milliseconds (measured,
// #698: a table created today reads back as January 1970 through the official
// client), and a date that is confidently wrong is worse than none.
//
// The emulator serves exactly one project, the instance's default, and answers
// 404 for every other. A screen that passed that 404 through would read as
// "broken", and one that showed an empty table would read as "you have no
// datasets", which is false in real BigQuery and misleading here. So a
// different project gets a prompt that names the one project served.

type bigqueryProvider struct {
	// endpoint is the forwarded REST address, host:port.
	endpoint string
	// project is the one project the emulator serves.
	project string
}

func (bigqueryProvider) ID() string    { return "bigquery" }
func (bigqueryProvider) Title() string { return "BigQuery" }

// onlyProject is the explicit state for a project the emulator does not serve.
func (p bigqueryProvider) onlyProject(project string) string {
	return fmt.Sprintf("The BigQuery emulator serves one project, %q, and no other, so %q "+
		"has nothing to show here. Choose %s in the toolbar.", p.project, project, p.project)
}

// scope reports the prompt a project needs before anything is read, or "".
func (p bigqueryProvider) scope(project string) string {
	switch {
	case project == "":
		return "BigQuery holds datasets per project. Choose one in the toolbar."
	case project != p.project:
		return p.onlyProject(project)
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
	if base.Note == "" {
		base.Note = "In-memory: everything here is gone when the instance restarts."
	}
	return base, nil
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

// Detail opens a dataset (its tables) or a table (its schema, details and
// first rows).
func (p bigqueryProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if prompt := p.scope(project); prompt != "" {
		return console.Detail{Prompt: prompt}, nil
	}
	switch len(path) {
	case 1:
		return p.datasetDetail(ctx, path[0])
	case 2:
		return p.tableDetail(ctx, path[0], path[1])
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

	return console.Detail{
		Summary: []console.Property{
			{Label: "Tables", Value: fmt.Sprint(tables.Total)},
			{Label: "Location", Value: orDash(md.Location)},
		},
		Sections: []console.Section{
			{ID: "tables", Label: "Tables", Listing: tables},
			{
				ID: "details", Label: "Details", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{Heading: "Dataset", Properties: props}},
			},
		},
	}, nil
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
	schema.Total = len(schema.Items)
	if schema.Total == 0 {
		schema.Note = "This table has no schema."
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
	if cl := md.Clustering; cl != nil && len(cl.Fields) > 0 {
		props = append(props, console.Property{Label: "Clustered by", Value: strings.Join(cl.Fields, ", ")})
	}
	if md.ViewQuery != "" {
		props = append(props, console.Property{Label: "View query", Value: md.ViewQuery})
	}
	if len(md.Labels) > 0 {
		props = append(props, console.Property{Label: "Labels", Value: formatLabels(md.Labels)})
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "Dataset", Value: datasetID},
			{Label: "Type", Value: orDash(string(md.Type))},
			{Label: "Rows", Value: fmt.Sprint(md.NumRows)},
			{Label: "Fields", Value: fmt.Sprint(len(md.Schema))},
		},
		Sections: []console.Section{
			{ID: "schema", Label: "Schema", Listing: schema},
			{
				ID: "details", Label: "Details", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{Heading: "Table", Properties: props}},
			},
			p.previewSection(ctx, t, md.Schema),
		},
	}, nil
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
		"before sending it; tables and rows are created with Create table and Insert rows. "+
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
	if project == "" {
		return console.Listing{Prompt: "Choose a project in the toolbar."}, nil
	}
	if project != p.project {
		return console.Listing{}, errors.New(p.onlyProject(project))
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
		return fmt.Errorf("the BigQuery editor is read-only: only a SELECT or WITH … SELECT statement runs here, not %s", first)
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
// matter to readOnlyBigQuery and classifySpannerStatement: identifiers and
// keywords, "(", ";", "@", "{" and "}". Quoted
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
		case c == '(' || c == ';' || c == '@' || c == '{' || c == '}':
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
	_ console.Driller  = bigqueryProvider{}
	_ console.Executor = bigqueryProvider{}
)
