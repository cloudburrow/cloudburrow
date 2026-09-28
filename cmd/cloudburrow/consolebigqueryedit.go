package main

// BigQuery writes: datasets, tables and rows (#854).
//
// The screen was read-only (#698), for a reason that stopped being true: the
// Spanner editor runs DML (#798) and Cloud SQL creates and drops databases
// (#699). Everything the emulator serves is now reachable from the console
// (#782): Create dataset and Delete dataset (datasets.insert, datasets.delete
// with deleteContents), Create table with a schema (tables.insert), Delete
// table (tables.delete) and Insert rows (tabledata.insertAll), each through the
// official client against the forwarded REST port. The query editor stays
// read-only; a statement that writes is not how this console changes data.
//
// The emulator is more permissive than BigQuery, and in ways that damage data
// (measured against goccy/bigquery-emulator, #854): it accepts a hyphenated
// dataset ID and a row missing a REQUIRED value, stores "x" as a NUMERIC 0,
// inserts the good rows of a batch with a bad one, and answers a duplicate
// dataset or column with a 500 the client retries until its deadline. The
// console used to apply BigQuery's rules itself. Since #861 the validating
// front (internal/bigqueryfront) stands in the forwarded REST port this
// client uses, and refuses all of that as BigQuery does, so the console no
// longer repeats those checks (#874): it sends what the form holds and shows
// the API's refusal in the API's words (bigqueryRefusal). What stays here is
// what no API refuses: the one-project precondition, the forms' own patterns
// (checked in the browser before anything is sent), the rows' JSON, and the
// shaping of each value into the form insertAll's JSON takes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The forms' patterns, which the browser checks as each field is typed. The
// API (the front) checks the same rules, and the length limits the patterns
// leave out.
const (
	// bigqueryDatasetIDPattern is BigQuery's: letters, digits and
	// underscores, at most 1,024.
	bigqueryDatasetIDPattern = `^[A-Za-z0-9_]+$`
	// bigqueryTableIDPattern is BigQuery's: letters, marks, numbers,
	// connectors, dashes and spaces, again at most 1,024 characters.
	bigqueryTableIDPattern = `^[\p{L}\p{M}\p{N}\p{Pc}\p{Pd}\p{Zs}]+$`
	// bigqueryFieldNamePattern is the column name rule every BigQuery table
	// accepts: a letter or underscore, then letters, digits or underscores,
	// at most 300. BigQuery's flexible column names allow more; this console
	// offers the portable rule rather than one the emulator may read
	// differently.
	bigqueryFieldNamePattern = `^[A-Za-z_][A-Za-z0-9_]{0,299}$`
	// bigqueryInsertLimit bounds one Insert rows: a form is for a handful of
	// rows, and a load of thousands belongs in a client.
	bigqueryInsertLimit = 500
)

// bigqueryColumnTypes are the types Create table offers, in the order
// BigQuery's own schema editor lists them. A RECORD holds nested fields,
// which the editor lists under it (#874).
var bigqueryColumnTypes = []string{
	"STRING", "BYTES", "INTEGER", "FLOAT", "NUMERIC", "BIGNUMERIC", "BOOLEAN",
	"TIMESTAMP", "DATE", "TIME", "DATETIME", "GEOGRAPHY", "JSON", "RECORD",
}

// bigqueryRecordDepth is how deep RECORDs nest: "a schema cannot contain
// more than 15 levels of nested RECORD types"
// (https://cloud.google.com/bigquery/docs/nested-repeated). The editor
// offers no RECORD below it.
const bigqueryRecordDepth = 15

// CreateForm implements console.Creator: Create dataset.
func (bigqueryProvider) CreateForm() (string, []console.Field) {
	return "Create dataset", []console.Field{
		{Name: "datasetId", Label: "Dataset ID", Type: "text", Required: true, Pattern: bigqueryDatasetIDPattern,
			Help: "Letters, numbers and underscores, at most 1,024."},
		{Name: "location", Label: "Location", Type: "text",
			Help: "Optional, such as US or EU. Stored and shown; the emulator runs every dataset in one place."},
		{Name: "description", Label: "Description", Type: "textarea"},
		{Name: "labels", Label: "Labels", Type: "map", Help: "One key=value per line."},
	}
}

// Create implements console.Creator.
func (p bigqueryProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if err := p.writable(project); err != nil {
		return "", err
	}
	id := strings.TrimSpace(values["datasetId"])
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", fmt.Errorf("labels: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return "", err
	}
	defer c.Close()
	ds := c.Dataset(id)
	// An invalid ID is the front's 400 and an existing dataset its 409
	// "Already Exists", at once, as BigQuery answers them (#861).
	md := &bigquery.DatasetMetadata{
		Location:    strings.TrimSpace(values["location"]),
		Description: values["description"],
	}
	if len(labels) > 0 {
		md.Labels = labels
	}
	if err := ds.Create(ctx, md); err != nil {
		return "", bigqueryRefusal(err)
	}
	return id, nil
}

// Delete implements console.Deleter for a dataset row: the dataset and every
// table in it, which is what BigQuery's console deletes too. The confirmation
// names the tables (DELETE_DETAIL in console.js).
func (p bigqueryProvider) Delete(ctx context.Context, project, name string) error {
	if err := p.writable(project); err != nil {
		return err
	}
	return p.deleteDataset(ctx, name)
}

func (p bigqueryProvider) deleteDataset(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	// A dataset already gone is the emulator's own 404 (measured, #854).
	return c.Dataset(id).DeleteWithContents(ctx)
}

// writable is the precondition every write shares: the one project the
// emulator serves.
func (p bigqueryProvider) writable(project string) error {
	switch {
	case project == "":
		return errors.New("choose a project first")
	case project != p.project:
		return errors.New(p.onlyProject(project))
	}
	return nil
}

// bigqueryRefusal is an API error in the API's own words: the message of a
// googleapi.Error without the "googleapi: Error 400:" envelope and the
// reason after it, which say nothing a person acts on. Any other error is
// returned as it is.
func bigqueryRefusal(err error) error {
	var e *googleapi.Error
	if errors.As(err, &e) && e.Message != "" {
		return errors.New(e.Message)
	}
	return err
}

// DetailActions offers Create table, Load from Cloud Storage and Delete
// dataset on a dataset, and Insert rows, Load from Cloud Storage, Export to
// Cloud Storage and Delete table on a table (the jobs are #993's,
// consolebigqueryjobs.go).
func (p bigqueryProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.writable(project) != nil {
		return nil
	}
	switch len(path) {
	case 1:
		return []console.Action{
			{ID: "createtable", Label: "Create table", Fields: bigqueryTableFields()},
			{ID: "load", Label: "Load from Cloud Storage", Fields: bigqueryLoadFields(true)},
			{ID: "deletedataset", Label: "Delete dataset", Destructive: true, Leaves: true,
				Confirm: "Every table in the dataset, and every row in them, is deleted with it."},
		}
	case 2:
		return []console.Action{
			{ID: "insertrows", Label: "Insert rows", Fields: p.insertFields(ctx, path[0], path[1])},
			{ID: "load", Label: "Load from Cloud Storage", Fields: bigqueryLoadFields(false)},
			{ID: "export", Label: "Export to Cloud Storage", Fields: bigqueryExportFields()},
			{ID: "deletetable", Label: "Delete table", Destructive: true, Leaves: true},
		}
	}
	return nil
}

// bigqueryTableFields are Create table's inputs.
func bigqueryTableFields() []console.Field {
	return []console.Field{
		{Name: "tableId", Label: "Table ID", Type: "text", Required: true, Pattern: bigqueryTableIDPattern,
			Help: "Letters, numbers, underscores, dashes and spaces, at most 1,024."},
		{Name: "description", Label: "Description", Type: "textarea"},
		{Name: "schema", Label: "Schema", Type: "schema", Required: true, Options: bigqueryColumnTypes,
			Pattern: bigqueryFieldNamePattern,
			Help: "Each field's name, type and mode. A name is a letter or underscore, then letters, digits " +
				"or underscores, and no two at one level may differ only in case. REQUIRED refuses a row " +
				"without the value; REPEATED holds an array. A RECORD holds nested fields, added under it."},
	}
}

// insertFields is Insert rows' one input, whose help names the table's own
// columns, so the form says what a row must look like.
func (p bigqueryProvider) insertFields(ctx context.Context, datasetID, tableID string) []console.Field {
	help := "One JSON object per line, or a JSON array of objects, keyed by field name. "
	if schema, err := p.tableSchema(ctx, datasetID, tableID); err == nil && len(schema) > 0 {
		cols := make([]string, len(schema))
		for i, f := range schema {
			cols[i] = f.Name + " " + describeColumn(f)
		}
		help += "This table's fields: " + strings.Join(cols, ", ") + ". "
	}
	help += "A value is written as its type reads it: an INTEGER or FLOAT as a number, a NUMERIC as a " +
		"number or a string, a BOOLEAN as true or false, a TIMESTAMP as RFC 3339 (2026-09-27T15:04:05Z), " +
		"a DATE as 2026-09-27, a TIME as 15:04:05, a DATETIME as 2026-09-27T15:04:05, BYTES as base64, a " +
		"JSON column as any JSON value, a REPEATED field as an array and a RECORD as an object of its " +
		"nested fields. Every row is checked before any is written, so a refused insert writes nothing. " +
		fmt.Sprintf("At most %d rows.", bigqueryInsertLimit)
	return []console.Field{{Name: "rows", Label: "Rows", Type: "textarea", Required: true, Help: help}}
}

func describeColumn(f *bigquery.FieldSchema) string {
	s := string(f.Type)
	if len(f.Schema) > 0 {
		// A RECORD's own fields, so the help says what its object holds.
		inner := make([]string, len(f.Schema))
		for i, c := range f.Schema {
			inner[i] = c.Name + " " + describeColumn(c)
		}
		s += " of " + strings.Join(inner, ", ")
	}
	switch {
	case f.Repeated:
		s += " REPEATED"
	case f.Required:
		s += " REQUIRED"
	}
	return "(" + s + ")"
}

func (p bigqueryProvider) tableSchema(ctx context.Context, datasetID, tableID string) (bigquery.Schema, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	md, err := c.Dataset(datasetID).Table(tableID).Metadata(ctx)
	if err != nil {
		return nil, err
	}
	return md.Schema, nil
}

// ActAt implements console.PathActor.
func (p bigqueryProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if err := p.writable(project); err != nil {
		return err
	}
	switch {
	case action == "createtable" && len(path) == 1:
		return p.createTable(ctx, path[0], values)
	case action == "deletedataset" && len(path) == 1:
		return p.deleteDataset(ctx, path[0])
	case action == "insertrows" && len(path) == 2:
		_, err := p.insertRows(ctx, path[0], path[1], values["rows"])
		return err
	case action == "deletetable" && len(path) == 2:
		ctx, cancel := context.WithTimeout(ctx, dbTimeout)
		defer cancel()
		c, err := p.client(ctx)
		if err != nil {
			return err
		}
		defer c.Close()
		return bigqueryRefusal(c.Dataset(path[0]).Table(path[1]).Delete(ctx))
	}
	return fmt.Errorf("unknown action %q", action)
}

// schemaColumn is one row of the schema editor, as the form submits it; a
// RECORD's nested rows are its fields.
type schemaColumn struct {
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Mode   string         `json:"mode"`
	Fields []schemaColumn `json:"fields,omitempty"`
}

// parseSchemaField reads the schema editor's value: a JSON array of name,
// type, mode and, for a RECORD, its fields. The rules a schema must follow
// — names, duplicates at one level, types, modes, a RECORD with no fields —
// are the API's (the front's, #861) and are not repeated here.
func parseSchemaField(raw string) (bigquery.Schema, error) {
	var cols []schemaColumn
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("a table needs at least one field")
	}
	if err := json.Unmarshal([]byte(raw), &cols); err != nil {
		return nil, fmt.Errorf("the schema is a JSON array of fields with a name, type and mode: %v", err)
	}
	if len(cols) == 0 {
		return nil, errors.New("a table needs at least one field")
	}
	return schemaFromColumns(cols), nil
}

func schemaFromColumns(cols []schemaColumn) bigquery.Schema {
	schema := make(bigquery.Schema, 0, len(cols))
	for _, col := range cols {
		mode := strings.ToUpper(strings.TrimSpace(col.Mode))
		f := &bigquery.FieldSchema{
			Name:     strings.TrimSpace(col.Name),
			Type:     bigquery.FieldType(strings.ToUpper(strings.TrimSpace(col.Type))),
			Required: mode == "REQUIRED", Repeated: mode == "REPEATED",
		}
		if len(col.Fields) > 0 {
			f.Schema = schemaFromColumns(col.Fields)
		}
		schema = append(schema, f)
	}
	return schema
}

func (p bigqueryProvider) createTable(ctx context.Context, datasetID string, values map[string]string) error {
	id := strings.TrimSpace(values["tableId"])
	schema, err := parseSchemaField(values["schema"])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	// An invalid ID or schema is the front's 400; a duplicate table is the
	// emulator's own 409.
	return bigqueryRefusal(c.Dataset(datasetID).Table(id).Create(ctx, &bigquery.TableMetadata{
		Schema: schema, Description: values["description"],
	}))
}

// jsonRow is one checked row, sent as it stands.
type jsonRow map[string]bigquery.Value

// Save implements bigquery.ValueSaver. No insert ID: the client makes one.
func (r jsonRow) Save() (map[string]bigquery.Value, string, error) { return r, "", nil }

// insertRows shapes every row for the table's schema and streams them in
// with one tabledata.insertAll, which the front checks row by row: without
// skipInvalidRows, one invalid row means none is written (#861). It
// returns how many were inserted.
func (p bigqueryProvider) insertRows(ctx context.Context, datasetID, tableID, raw string) (int, error) {
	objects, err := decodeRows(raw)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	t := c.Dataset(datasetID).Table(tableID)
	md, err := t.Metadata(ctx)
	if err != nil {
		return 0, bigqueryRefusal(err)
	}
	if len(md.Schema) == 0 {
		return 0, errors.New("this table has no schema, so it has no field a row could set")
	}
	rows := make([]jsonRow, len(objects))
	for i, obj := range objects {
		rows[i] = bigqueryRow(md.Schema, obj)
	}
	if err := t.Inserter().Put(ctx, rows); err != nil {
		return 0, insertError(err)
	}
	return len(rows), nil
}

// insertError is a PutMultiError as the rows the form numbered, each with
// the API's message for it. A row reported only as "stopped" was valid and
// not written because another was not; it is left out, so what is shown is
// what to fix.
func insertError(err error) error {
	var multi bigquery.PutMultiError
	if !errors.As(err, &multi) {
		return bigqueryRefusal(err)
	}
	parts := make([]string, 0, len(multi))
	for _, re := range multi {
		msgs := make([]string, 0, len(re.Errors))
		for _, e := range re.Errors {
			var be *bigquery.Error
			switch {
			case errors.As(e, &be) && be.Reason == "stopped":
				continue
			case errors.As(e, &be) && be.Message != "":
				msgs = append(msgs, be.Message)
			default:
				msgs = append(msgs, e.Error())
			}
		}
		if len(msgs) > 0 {
			parts = append(parts, fmt.Sprintf("row %d: %s", re.RowIndex+1, strings.Join(msgs, "; ")))
		}
	}
	if len(parts) == 0 {
		return err
	}
	return errors.New(strings.Join(parts, "\n"))
}

// decodeRows reads the form's rows: a JSON array of objects, or one object
// per line.
func decodeRows(raw string) ([]map[string]any, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("at least one row is required")
	}
	var out []map[string]any
	decodeOne := func(text string, where string) (map[string]any, error) {
		dec := json.NewDecoder(strings.NewReader(text))
		dec.UseNumber()
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			return nil, fmt.Errorf("%s is not a JSON object: %v", where, err)
		}
		if dec.More() {
			return nil, fmt.Errorf("%s holds more than one JSON value; put each row on its own line", where)
		}
		if obj == nil {
			return nil, fmt.Errorf("%s is null, not a row", where)
		}
		return obj, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		var arr []json.RawMessage
		if err := dec.Decode(&arr); err != nil {
			return nil, fmt.Errorf("the rows are not a JSON array of objects: %v", err)
		}
		if dec.More() {
			return nil, errors.New("the rows are one JSON array; nothing may follow it")
		}
		for i, r := range arr {
			obj, err := decodeOne(string(r), fmt.Sprintf("row %d", i+1))
			if err != nil {
				return nil, err
			}
			out = append(out, obj)
		}
	} else {
		for i, line := range strings.Split(trimmed, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			obj, err := decodeOne(line, fmt.Sprintf("line %d", i+1))
			if err != nil {
				return nil, err
			}
			out = append(out, obj)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("at least one row is required")
	}
	if len(out) > bigqueryInsertLimit {
		return nil, fmt.Errorf("%d rows is more than the %d one insert takes here", len(out), bigqueryInsertLimit)
	}
	return out, nil
}

// bigqueryRow shapes one decoded row for insertAll: a field is keyed by its
// column's own name, and each value is put in the form insertAll's JSON
// takes for its column's type (bigqueryValue). Nothing is refused here: a
// field the table does not have, a missing REQUIRED value and a value that
// does not convert are sent as written, and the front refuses the row with
// BigQuery's reason, naming the field (#861).
func bigqueryRow(schema bigquery.Schema, obj map[string]any) jsonRow {
	byName := make(map[string]*bigquery.FieldSchema, len(schema))
	for _, f := range schema {
		byName[strings.ToLower(f.Name)] = f
	}
	row := jsonRow{}
	for k, v := range obj {
		f, ok := byName[strings.ToLower(k)]
		if !ok || v == nil {
			row[k] = v
			continue
		}
		row[f.Name] = bigqueryFieldValue(f, v)
	}
	return row
}

// bigqueryFieldValue is one field's value: for a REPEATED field, each
// element of its array shaped as the column's type.
func bigqueryFieldValue(f *bigquery.FieldSchema, v any) bigquery.Value {
	arr, isArray := v.([]any)
	if !f.Repeated || !isArray {
		return bigqueryValue(f, v)
	}
	out := make([]bigquery.Value, len(arr))
	for i, e := range arr {
		out[i] = bigqueryValue(f, e)
	}
	return out
}

// bigqueryValue shapes one value the way the console has always sent it
// (#854): an INTEGER written as a string is sent as the number it holds, a
// NUMERIC or BIGNUMERIC written as a number as its exact decimal text, a
// JSON column's value as its JSON text, and a RECORD's object field by
// field. Anything else, and a value that does not parse, is sent as it was
// written.
func bigqueryValue(f *bigquery.FieldSchema, v any) bigquery.Value {
	if v == nil {
		return nil
	}
	switch f.Type {
	case bigquery.IntegerFieldType:
		if s, ok := v.(string); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				return n
			}
		}
	case bigquery.NumericFieldType, bigquery.BigNumericFieldType:
		if n, ok := v.(json.Number); ok {
			return n.String()
		}
	case bigquery.JSONFieldType:
		// The value is the JSON itself; insertAll carries a JSON column as
		// its text.
		if encoded, err := json.Marshal(v); err == nil {
			return string(encoded)
		}
	case bigquery.RecordFieldType:
		if obj, ok := v.(map[string]any); ok {
			return map[string]bigquery.Value(bigqueryRow(f.Schema, obj))
		}
	}
	return v
}

var (
	_ console.Creator   = bigqueryProvider{}
	_ console.Deleter   = bigqueryProvider{}
	_ console.PathActor = bigqueryProvider{}
)
