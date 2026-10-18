package bigqueryfront

import (
	"strings"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Missing values of the Storage Write API (#1391).
//
// "Missing values are fields present in user schema but missing in rows"
// (storage.proto, AppendRowsRequest.missing_value_interpretations): a
// proto row's field with no value (and no default in the writer schema),
// an Arrow row's NULL. NULL_VALUE, and a field neither map nor default
// names, writes NULL; DEFAULT_VALUE writes "column default value if
// declared in the table schema, NULL otherwise". A key of the map is a
// top-level column ("Currently, field name can only be top-level column
// name, can't be a struct field path like 'foo.bar'"), matched without
// regard to case as columns are; a key naming no column is ignored.
// default_missing_value_interpretation "is applied to all requests from
// that point forward, until a subsequent AppendRowsRequest sets it to a
// different value", and the map overrides it for its columns. A
// MISSING_VALUE_INTERPRETATION_UNSPECIFIED in the map is INVALID_ARGUMENT
// ("Requests with this value will be rejected").
//
// A column written its default is left out of the row given to
// tabledata.insertAll, and the engine writes the column's DEFAULT for a
// row that leaves it out (engine patches bigquery-emulator 0014 and
// googlesqlite 0027, columndefaults.go), as it does for a column the
// writer schema lacks. An Arrow NULL taken as missing is UNVERIFIED
// against BigQuery.
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/bigquery/storage/v1/storage.proto

// missingValues are the columns, by lower-case name, whose missing values
// are their defaults; nil: none.
type missingValues struct {
	all     bool
	columns map[string]bool
	nulls   map[string]bool
}

// missingInterpretations is the interpretation of one request: the
// connection's default, def, and the request's map.
func missingInterpretations(def storagepb.AppendRowsRequest_MissingValueInterpretation,
	m map[string]storagepb.AppendRowsRequest_MissingValueInterpretation) (*missingValues, error) {
	mv := &missingValues{all: def == storagepb.AppendRowsRequest_DEFAULT_VALUE}
	for col, i := range m {
		switch i {
		case storagepb.AppendRowsRequest_DEFAULT_VALUE:
			if mv.columns == nil {
				mv.columns = map[string]bool{}
			}
			mv.columns[strings.ToLower(col)] = true
		case storagepb.AppendRowsRequest_NULL_VALUE:
			if mv.nulls == nil {
				mv.nulls = map[string]bool{}
			}
			mv.nulls[strings.ToLower(col)] = true
		default:
			return nil, status.Errorf(codes.InvalidArgument, "Invalid missing_value_interpretations: the column %q has "+
				"MISSING_VALUE_INTERPRETATION_UNSPECIFIED. Nothing was written.", col)
		}
	}
	if !mv.all && len(mv.columns) == 0 {
		return nil, nil
	}
	return mv, nil
}

// defaults reports whether a missing value of the column is its default.
func (mv *missingValues) defaults(col string) bool {
	if mv == nil {
		return false
	}
	c := strings.ToLower(col)
	if mv.columns[c] {
		return true
	}
	return mv.all && !mv.nulls[c]
}

// apply leaves out of obj, a row's insertAll object, each NULL of a
// column that is written its default: a top-level, not REPEATED column
// with a defaultValueExpression (one without is NULL either way).
func (mv *missingValues) apply(fields []field, obj map[string]any) map[string]any {
	if mv == nil || obj == nil {
		return obj
	}
	for _, f := range fields {
		if f.DefaultValueExpression == "" || strings.EqualFold(f.Mode, "REPEATED") || !mv.defaults(f.Name) {
			continue
		}
		for k, v := range obj {
			if v == nil && strings.EqualFold(k, f.Name) {
				delete(obj, k)
			}
		}
	}
	return obj
}
