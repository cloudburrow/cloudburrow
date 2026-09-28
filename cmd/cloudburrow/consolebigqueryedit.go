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
// (measured against goccy/bigquery-emulator, #854), so the console enforces
// the API's own rules before anything is sent:
//
//   - It accepts a dataset ID with a hyphen, a table ID with "!", a field name
//     with a space, and a row missing a REQUIRED field, all of which BigQuery
//     refuses. The forms carry BigQuery's patterns and the row check refuses a
//     missing required value.
//   - It answers a duplicate dataset, a duplicate column and a value of the
//     wrong type with HTTP 500, which the client retries until its deadline,
//     so the refusal would arrive as a timeout. A duplicate dataset is looked
//     up first, a duplicate column refused on the form, and every value
//     checked against its column's type.
//   - It stores an unparseable element of a REPEATED INTEGER, and the table
//     then cannot be read at all ("failed to scan rows"), and it stores "x" as
//     a NUMERIC 0. Every value, repeated elements included, is parsed as its
//     column's type before the insert is sent.
//   - Given several rows with one bad one, it inserts the good rows, where
//     BigQuery inserts none. Every row is checked before any is sent, so a
//     refused insert writes nothing.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	// bigqueryDatasetIDPattern is BigQuery's: letters, digits and
	// underscores. Its limit of 1,024 is checked apart, because Go's regexp
	// refuses a repeat count over 1,000.
	bigqueryDatasetIDPattern = `^[A-Za-z0-9_]+$`
	// bigqueryTableIDPattern is BigQuery's: letters, marks, numbers,
	// connectors, dashes and spaces, again at most 1,024 characters.
	bigqueryTableIDPattern = `^[\p{L}\p{M}\p{N}\p{Pc}\p{Pd}\p{Zs}]+$`
	// bigqueryIDLimit is the longest dataset or table ID.
	bigqueryIDLimit = 1024
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

var (
	bigqueryDatasetID = regexp.MustCompile(bigqueryDatasetIDPattern)
	bigqueryTableID   = regexp.MustCompile(bigqueryTableIDPattern)
	bigqueryFieldName = regexp.MustCompile(bigqueryFieldNamePattern)
)

// bigqueryColumnTypes are the types Create table offers, in the order
// BigQuery's own schema editor lists them. RECORD is not offered: a nested
// schema needs an editor of its own, and a table with RECORD columns created
// by a client still takes rows here.
var bigqueryColumnTypes = []string{
	"STRING", "BYTES", "INTEGER", "FLOAT", "NUMERIC", "BIGNUMERIC", "BOOLEAN",
	"TIMESTAMP", "DATE", "TIME", "DATETIME", "GEOGRAPHY", "JSON",
}

var bigqueryColumnModes = []string{"NULLABLE", "REQUIRED", "REPEATED"}

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
	if !bigqueryDatasetID.MatchString(id) || len(id) > bigqueryIDLimit {
		return "", fmt.Errorf("%q is not a dataset ID: letters, numbers and underscores, at most 1,024", id)
	}
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
	// The emulator answers a duplicate with a 500, which the client retries
	// until its deadline; BigQuery answers 409. Looked up first, so the
	// refusal is immediate and says what it is.
	if _, err := ds.Metadata(ctx); err == nil {
		return "", fmt.Errorf("already exists: dataset %s.%s", p.project, id)
	} else if !isBigQueryNotFound(err) {
		return "", err
	}
	md := &bigquery.DatasetMetadata{
		Location:    strings.TrimSpace(values["location"]),
		Description: values["description"],
	}
	if len(labels) > 0 {
		md.Labels = labels
	}
	if err := ds.Create(ctx, md); err != nil {
		return "", err
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

func isBigQueryNotFound(err error) bool {
	var e *googleapi.Error
	return errors.As(err, &e) && e.Code == 404
}

// DetailActions offers Create table and Delete dataset on a dataset, and
// Insert rows and Delete table on a table.
func (p bigqueryProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.writable(project) != nil {
		return nil
	}
	switch len(path) {
	case 1:
		return []console.Action{
			{ID: "createtable", Label: "Create table", Fields: bigqueryTableFields()},
			{ID: "deletedataset", Label: "Delete dataset", Destructive: true, Leaves: true,
				Confirm: "Every table in the dataset, and every row in them, is deleted with it."},
		}
	case 2:
		return []console.Action{
			{ID: "insertrows", Label: "Insert rows", Fields: p.insertFields(ctx, path[0], path[1])},
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
				"or underscores, and no two may differ only in case. REQUIRED refuses a row without the " +
				"value; REPEATED holds an array."},
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
		"JSON column as any JSON value, a REPEATED field as an array and a RECORD as an object. Every row " +
		fmt.Sprintf("is checked before any is sent, so a refused insert writes nothing. At most %d rows.", bigqueryInsertLimit)
	return []console.Field{{Name: "rows", Label: "Rows", Type: "textarea", Required: true, Help: help}}
}

func describeColumn(f *bigquery.FieldSchema) string {
	s := string(f.Type)
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
		return c.Dataset(path[0]).Table(path[1]).Delete(ctx)
	}
	return fmt.Errorf("unknown action %q", action)
}

// schemaColumn is one row of the schema editor, as the form submits it.
type schemaColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Mode string `json:"mode"`
}

// parseSchemaField reads the schema editor's value: a JSON array of name,
// type and mode.
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
	seen := map[string]bool{}
	var schema bigquery.Schema
	for i, col := range cols {
		name := strings.TrimSpace(col.Name)
		if !bigqueryFieldName.MatchString(name) {
			return nil, fmt.Errorf("field %d: %q is not a field name: a letter or underscore, then letters, digits or underscores, at most 300", i+1, name)
		}
		// Column names are case-insensitive in BigQuery; the emulator
		// answers a duplicate with a 500 the client retries until its
		// deadline.
		if seen[strings.ToLower(name)] {
			return nil, fmt.Errorf("field %q is named twice: field names are case-insensitive", name)
		}
		seen[strings.ToLower(name)] = true
		typ := strings.ToUpper(strings.TrimSpace(col.Type))
		if !oneOf(bigqueryColumnTypes, typ) {
			return nil, fmt.Errorf("field %q: type %q is not one of %s", name, col.Type, strings.Join(bigqueryColumnTypes, ", "))
		}
		mode := strings.ToUpper(strings.TrimSpace(col.Mode))
		if mode == "" {
			mode = "NULLABLE"
		}
		if !oneOf(bigqueryColumnModes, mode) {
			return nil, fmt.Errorf("field %q: mode %q is not one of NULLABLE, REQUIRED or REPEATED", name, col.Mode)
		}
		schema = append(schema, &bigquery.FieldSchema{
			Name: name, Type: bigquery.FieldType(typ),
			Required: mode == "REQUIRED", Repeated: mode == "REPEATED",
		})
	}
	return schema, nil
}

func oneOf(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (p bigqueryProvider) createTable(ctx context.Context, datasetID string, values map[string]string) error {
	id := strings.TrimSpace(values["tableId"])
	if !bigqueryTableID.MatchString(id) || len([]rune(id)) > bigqueryIDLimit {
		return fmt.Errorf("%q is not a table ID: letters, numbers, underscores, dashes and spaces, at most 1,024", id)
	}
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
	// A duplicate table is the emulator's own 409.
	return c.Dataset(datasetID).Table(id).Create(ctx, &bigquery.TableMetadata{
		Schema: schema, Description: values["description"],
	})
}

// jsonRow is one checked row, sent as it stands.
type jsonRow map[string]bigquery.Value

// Save implements bigquery.ValueSaver. No insert ID: the client makes one.
func (r jsonRow) Save() (map[string]bigquery.Value, string, error) { return r, "", nil }

// insertRows checks every row against the table's schema and then streams
// them in with one tabledata.insertAll. It returns how many were inserted.
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
		return 0, err
	}
	if len(md.Schema) == 0 {
		return 0, errors.New("this table has no schema, so it has no field a row could set")
	}
	rows := make([]jsonRow, len(objects))
	for i, obj := range objects {
		row, err := bigqueryRow(md.Schema, obj)
		if err != nil {
			return 0, fmt.Errorf("row %d: %w", i+1, err)
		}
		rows[i] = row
	}
	if err := t.Inserter().Put(ctx, rows); err != nil {
		return 0, insertError(err)
	}
	return len(rows), nil
}

// insertError is a PutMultiError as the rows the form numbered.
func insertError(err error) error {
	var multi bigquery.PutMultiError
	if !errors.As(err, &multi) {
		return err
	}
	parts := make([]string, 0, len(multi))
	for _, re := range multi {
		msgs := make([]string, 0, len(re.Errors))
		for _, e := range re.Errors {
			msgs = append(msgs, e.Error())
		}
		parts = append(parts, fmt.Sprintf("row %d: %s", re.RowIndex+1, strings.Join(msgs, "; ")))
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

// bigqueryRow checks one decoded row against a schema and returns the values
// to send: every field named must exist, every REQUIRED field must be set, and
// every value must parse as its column's type.
func bigqueryRow(schema bigquery.Schema, obj map[string]any) (jsonRow, error) {
	byName := make(map[string]*bigquery.FieldSchema, len(schema))
	for _, f := range schema {
		byName[strings.ToLower(f.Name)] = f
	}
	row := jsonRow{}
	for k, v := range obj {
		f, ok := byName[strings.ToLower(k)]
		if !ok {
			names := make([]string, len(schema))
			for i, f := range schema {
				names[i] = f.Name
			}
			return nil, fmt.Errorf("no such field: %s (this table's fields are %s)", k, strings.Join(names, ", "))
		}
		if v == nil {
			continue
		}
		out, err := bigqueryFieldValue(f, v)
		if err != nil {
			return nil, err
		}
		row[f.Name] = out
	}
	for _, f := range schema {
		if f.Required {
			if _, ok := row[f.Name]; !ok {
				return nil, fmt.Errorf("%s is REQUIRED and has no value", f.Name)
			}
		}
	}
	return row, nil
}

// bigqueryFieldValue is one field's value: an array for a REPEATED field,
// each element its column's type.
func bigqueryFieldValue(f *bigquery.FieldSchema, v any) (bigquery.Value, error) {
	if !f.Repeated {
		return bigqueryScalar(f, v)
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is REPEATED: its value is a JSON array", f.Name)
	}
	out := make([]bigquery.Value, len(arr))
	for i, e := range arr {
		if e == nil {
			return nil, fmt.Errorf("%s[%d] is null: an array in BigQuery holds no NULL", f.Name, i)
		}
		c, err := bigqueryScalar(f, e)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", f.Name, i, err)
		}
		out[i] = c
	}
	return out, nil
}

// bigqueryTimeLayouts are the forms a DATE, TIME and DATETIME take in the
// JSON BigQuery reads.
var bigqueryTimeLayouts = map[bigquery.FieldType][]string{
	bigquery.DateFieldType:     {"2006-01-02"},
	bigquery.TimeFieldType:     {"15:04:05.999999999", "15:04:05", "15:04"},
	bigquery.DateTimeFieldType: {"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"},
}

// bigqueryScalar parses one value as its column's type, and returns it in the
// form insertAll's JSON takes.
func bigqueryScalar(f *bigquery.FieldSchema, v any) (bigquery.Value, error) {
	text, isString := v.(string)
	num, isNumber := v.(json.Number)
	want := func(what string) error {
		shown, _ := json.Marshal(v)
		return fmt.Errorf("%s is %s: %s is %s", f.Name, f.Type, shown, what)
	}
	switch f.Type {
	case bigquery.StringFieldType, bigquery.GeographyFieldType:
		// GEOGRAPHY is sent as the WKT or GeoJSON text it is written in; the
		// emulator does not check it, and neither does this console.
		if !isString {
			return nil, want("not a JSON string")
		}
		return text, nil
	case bigquery.IntegerFieldType:
		s := text
		if isNumber {
			s = num.String()
		} else if !isString {
			return nil, want("not an integer")
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, want("not a 64-bit integer")
		}
		return n, nil
	case bigquery.FloatFieldType:
		s := text
		if isNumber {
			s = num.String()
		} else if !isString {
			return nil, want("not a number")
		}
		x, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, want("not a number")
		}
		if isString {
			// "NaN" and "Infinity" are FLOAT values JSON cannot hold as
			// numbers; BigQuery reads them as strings, so they are sent as
			// the string they were written as.
			return strings.TrimSpace(s), nil
		}
		return x, nil
	case bigquery.NumericFieldType, bigquery.BigNumericFieldType:
		s := text
		if isNumber {
			s = num.String()
		} else if !isString {
			return nil, want("not a number")
		}
		s = strings.TrimSpace(s)
		// A decimal, never a fraction: big.Rat would accept "1/3".
		if strings.Contains(s, "/") {
			return nil, want("not a decimal number")
		}
		if _, ok := new(big.Rat).SetString(s); !ok {
			return nil, want("not a decimal number")
		}
		return s, nil
	case bigquery.BooleanFieldType:
		if b, ok := v.(bool); ok {
			return b, nil
		}
		return nil, want("not true or false")
	case bigquery.TimestampFieldType:
		if !isString {
			return nil, want("not an RFC 3339 time such as 2026-09-27T15:04:05Z")
		}
		if _, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text)); err != nil {
			return nil, want("not an RFC 3339 time such as 2026-09-27T15:04:05Z")
		}
		return strings.TrimSpace(text), nil
	case bigquery.DateFieldType, bigquery.TimeFieldType, bigquery.DateTimeFieldType:
		example := map[bigquery.FieldType]string{
			bigquery.DateFieldType: "2026-09-27", bigquery.TimeFieldType: "15:04:05",
			bigquery.DateTimeFieldType: "2026-09-27T15:04:05",
		}[f.Type]
		if !isString {
			return nil, want("not written like " + example)
		}
		for _, layout := range bigqueryTimeLayouts[f.Type] {
			if _, err := time.Parse(layout, strings.TrimSpace(text)); err == nil {
				return strings.TrimSpace(text), nil
			}
		}
		return nil, want("not written like " + example)
	case bigquery.BytesFieldType:
		if !isString {
			return nil, want("not a base64 string")
		}
		if _, err := base64.StdEncoding.DecodeString(text); err != nil {
			return nil, want("not base64")
		}
		return text, nil
	case bigquery.JSONFieldType:
		// The value is the JSON itself; insertAll carries a JSON column as
		// its text.
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil, want("not JSON")
		}
		return string(encoded), nil
	case bigquery.RecordFieldType:
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, want("not a JSON object")
		}
		nested, err := bigqueryRow(f.Schema, obj)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		return map[string]bigquery.Value(nested), nil
	}
	return nil, fmt.Errorf("%s is %s, which this console does not write", f.Name, f.Type)
}

var (
	_ console.Creator   = bigqueryProvider{}
	_ console.Deleter   = bigqueryProvider{}
	_ console.PathActor = bigqueryProvider{}
)
