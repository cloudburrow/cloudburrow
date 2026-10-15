package main

// BigQuery routines on a dataset's page (#782): user-defined functions,
// table functions and stored procedures (#1217), which CREATE FUNCTION,
// CREATE TABLE FUNCTION, CREATE PROCEDURE and routines.insert make (the "Table functions" and CREATE FUNCTION rows of
// docs/compatibility.md). The dataset's page lists them (routines.list), a
// routine's page shows its type, language, arguments, return type and body
// (routines.get), and Delete routine drops it (routines.delete), each through
// the official client against the forwarded REST port.
//
// A routine's page is addressed [dataset, "routines", routine]: a table and a
// routine of one dataset can share a name, and a table's page is
// [dataset, table].

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// bigqueryRoutinesSegment is the second segment of a routine's path.
const bigqueryRoutinesSegment = "routines"

// actDeleteRoutine is Delete routine, on a routine's page and its row.
const actDeleteRoutine = "deleteroutine"

// routinePath is the path of a routine's page and of its row's actions.
func routinePath(datasetID, routineID string) []string {
	return []string{datasetID, bigqueryRoutinesSegment, routineID}
}

// isRoutinePath reports whether path is a routine's page.
func isRoutinePath(path []string) bool {
	return len(path) == 3 && path[1] == bigqueryRoutinesSegment
}

// routinesSection lists a dataset's routines, each opening its page and
// offering Delete routine.
func (p bigqueryProvider) routinesSection(ctx context.Context, ds *bigquery.Dataset) console.Section {
	out := console.Listing{
		Columns: []string{"Type", "Language", "Arguments"}, NameColumn: "Routine", Noun: "routines",
	}
	it := ds.Routines(ctx)
	for {
		r, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			out.Unavailable = "listing routines: " + bigqueryRefusal(err).Error()
			break
		}
		fields := map[string]string{"Type": "—", "Language": "—", "Arguments": "—"}
		if md, err := r.Metadata(ctx); err == nil {
			fields["Type"] = routineType(md.Type)
			fields["Language"] = orDash(md.Language)
			fields["Arguments"] = orDash(routineArguments(md.Arguments))
		}
		out.Items = append(out.Items, console.Resource{
			Name: r.RoutineID, Fields: fields,
			Opens:   routinePath(ds.DatasetID, r.RoutineID),
			Actions: []console.Action{deleteRoutineAction()},
			ActsOn:  routinePath(ds.DatasetID, r.RoutineID),
		})
		if len(out.Items) >= detailLimit {
			out.Note = truncatedNote(len(out.Items), "routines")
			break
		}
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	if out.Total == 0 && out.Unavailable == "" {
		out.Note = "This dataset has no routines. CREATE FUNCTION, CREATE TABLE FUNCTION or CREATE PROCEDURE in the query editor's Read-write mode makes one."
	}
	return console.Section{ID: "routines", Label: "Routines", Listing: out}
}

func deleteRoutineAction() console.Action {
	return console.Action{ID: actDeleteRoutine, Label: "Delete routine", Destructive: true, Leaves: true}
}

// routineTrail is a routine page's breadcrumb: its dataset, then itself.
// [dataset, "routines"] is no page, so the trail skips it, and Delete
// routine goes back to the dataset.
func routineTrail(datasetID, routineID string) []console.Crumb {
	return []console.Crumb{{Label: datasetID, Path: []string{datasetID}}, {Label: routineID}}
}

// routineDetail is a routine's page.
func (p bigqueryProvider) routineDetail(ctx context.Context, datasetID, routineID string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	trail := routineTrail(datasetID, routineID)
	c, err := p.client(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error(), Trail: trail}, nil
	}
	defer c.Close()
	md, err := c.Dataset(datasetID).Routine(routineID).Metadata(ctx)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the routine: " + bigqueryRefusal(err).Error(), Trail: trail}, nil
	}

	args := console.Listing{Columns: []string{"Type", "Kind", "Mode"}, NameColumn: "Argument", Noun: "arguments"}
	for i, a := range md.Arguments {
		name := a.Name
		if name == "" {
			name = fmt.Sprintf("(argument %d)", i+1)
		}
		args.Items = append(args.Items, console.Resource{Name: name, Fields: map[string]string{
			"Type": orDash(sqlType(a.DataType)), "Kind": orDash(a.Kind), "Mode": orDash(a.Mode),
		}})
	}
	args.Total = len(args.Items)
	if args.Total == 0 {
		args.Note = "This routine takes no arguments."
	}

	props := []console.Property{
		{Label: "Routine ID", Value: p.project + "." + datasetID + "." + routineID},
		{Label: "Type", Value: routineType(md.Type)},
		{Label: "Language", Value: orDash(md.Language)},
	}
	if ret := sqlType(md.ReturnType); ret != "" {
		props = append(props, console.Property{Label: "Returns", Value: ret})
	}
	if tt := md.ReturnTableType; tt != nil && len(tt.Columns) > 0 {
		props = append(props, console.Property{Label: "Returns table", Value: "TABLE<" + sqlFields(tt.Columns) + ">"})
	}
	if md.DeterminismLevel != "" {
		props = append(props, console.Property{Label: "Determinism", Value: string(md.DeterminismLevel)})
	}
	if md.Description != "" {
		props = append(props, console.Property{Label: "Description", Value: md.Description})
	}
	if len(md.ImportedLibraries) > 0 {
		props = append(props, console.Property{Label: "Imported libraries", Value: strings.Join(md.ImportedLibraries, ", ")})
	}
	if !md.CreationTime.IsZero() {
		props = append(props, console.Property{Label: "Created", Value: md.CreationTime.UTC().Format(time.RFC3339)})
	}
	if !md.LastModifiedTime.IsZero() {
		props = append(props, console.Property{Label: "Last modified", Value: md.LastModifiedTime.UTC().Format(time.RFC3339)})
	}

	body := md.Body
	if strings.TrimSpace(body) == "" {
		body = "(no body)"
	}
	return console.Detail{
		Trail: trail,
		Summary: []console.Property{
			{Label: "Dataset", Value: datasetID},
			{Label: "Type", Value: routineType(md.Type)},
			{Label: "Arguments", Value: fmt.Sprint(len(md.Arguments))},
		},
		Sections: []console.Section{
			{ID: "body", Label: "Body", Kind: console.KindText, Text: body},
			{ID: "arguments", Label: "Arguments", Listing: args},
			{
				ID: "details", Label: "Details", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{Heading: "Routine", Properties: props}},
			},
		},
	}, nil
}

// deleteRoutine drops a routine with routines.delete.
func (p bigqueryProvider) deleteRoutine(ctx context.Context, datasetID, routineID string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return bigqueryRefusal(c.Dataset(datasetID).Routine(routineID).Delete(ctx))
}

// routineType is a routine's type in the words BigQuery's console uses.
func routineType(t string) string {
	switch t {
	case "SCALAR_FUNCTION":
		return "Function"
	case "TABLE_VALUED_FUNCTION":
		return "Table function"
	case "PROCEDURE":
		return "Procedure"
	case "AGGREGATE_FUNCTION":
		return "Aggregate function"
	}
	return orDash(t)
}

// routineArguments is a routine's signature: each argument's name and
// type, after its mode when it is a procedure's OUT or INOUT one.
func routineArguments(args []*bigquery.RoutineArgument) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		mode := ""
		if a.Mode == "OUT" || a.Mode == "INOUT" {
			mode = a.Mode + " "
		}
		parts = append(parts, strings.TrimSpace(mode+a.Name+" "+sqlType(a.DataType)))
	}
	return strings.Join(parts, ", ")
}

// sqlType writes a GoogleSQL type the way DDL spells it: ARRAY<INT64>,
// STRUCT<a INT64, b STRING>. An ANY TYPE argument has no data type, and is
// empty.
func sqlType(t *bigquery.StandardSQLDataType) string {
	if t == nil {
		return ""
	}
	switch {
	case t.ArrayElementType != nil:
		return "ARRAY<" + sqlType(t.ArrayElementType) + ">"
	case t.RangeElementType != nil:
		return "RANGE<" + sqlType(t.RangeElementType) + ">"
	case t.StructType != nil:
		return "STRUCT<" + sqlFields(t.StructType.Fields) + ">"
	}
	return t.TypeKind
}

func sqlFields(fields []*bigquery.StandardSQLField) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, strings.TrimSpace(f.Name+" "+sqlType(f.Type)))
	}
	return strings.Join(parts, ", ")
}

// actEditRoutine is Edit routine, on a routine's page (#1238).
const actEditRoutine = "editroutine"

// editRoutineAction is Edit routine: the body and description, drawn from
// the routine. Its type, language, arguments and return type are kept.
func editRoutineAction(md *bigquery.RoutineMetadata) console.Action {
	return console.Action{ID: actEditRoutine, Label: "Edit routine", Fields: []console.Field{
		{Name: "body", Label: "Body", Type: "textarea", Required: true, Default: md.Body,
			Help: "A SQL function's expression, or a table function's query. The type, language, arguments and return type are kept."},
		{Name: "description", Label: "Description", Type: "text", Default: md.Description},
	}}
}

// routineMetadata reads a routine with routines.get.
func (p bigqueryProvider) routineMetadata(ctx context.Context, datasetID, routineID string) (*bigquery.RoutineMetadata, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.Dataset(datasetID).Routine(routineID).Metadata(ctx)
}

// editRoutine replaces a routine's body and description with routines.update
// (#1238), restating what the form does not change, since routines.update
// replaces the whole routine, and sending the etag it read.
func (p bigqueryProvider) editRoutine(ctx context.Context, datasetID, routineID string, values map[string]string) error {
	body := strings.TrimSpace(values["body"])
	if body == "" {
		return errors.New("a routine needs a body")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	r := c.Dataset(datasetID).Routine(routineID)
	md, err := r.Metadata(ctx)
	if err != nil {
		return bigqueryRefusal(err)
	}
	_, err = r.Update(ctx, &bigquery.RoutineMetadataToUpdate{
		Type: md.Type, Language: md.Language, Arguments: md.Arguments, ReturnType: md.ReturnType,
		ReturnTableType: md.ReturnTableType, DeterminismLevel: md.DeterminismLevel, ImportedLibraries: md.ImportedLibraries,
		Body: body, Description: strings.TrimSpace(values["description"]),
	}, md.ETag)
	return bigqueryRefusal(err)
}
