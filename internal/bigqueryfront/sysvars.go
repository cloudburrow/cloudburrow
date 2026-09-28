package bigqueryfront

import "strings"

// @@dataset_id in a query with a default dataset (#1137).
//
// In BigQuery @@dataset_id is "the ID of the default dataset in the
// current project" (https://cloud.google.com/bigquery/docs/reference/system-variables),
// and a query's default dataset is the same thing: "Setting the system
// variable @@dataset_id achieves the same behavior"
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationQuery.FIELDS.default_dataset).
// The emulator does not set the variable from the default dataset:
// measured through the front, SELECT @@dataset_id with the default dataset
// m1129 read NULL, where a script that ran SET @@dataset_id = 'm1129'
// first read m1129.
//
// So the front sends each @@dataset_id of a query with a default dataset
// as that dataset's ID, a string literal, unless the query sets the
// variable itself (SET @@dataset_id), which the emulator then reads. A
// CREATE FUNCTION, PROCEDURE or VIEW statement's is left as it is, as its
// body is read when it is called. With no default dataset the variable
// is NULL, in BigQuery too, and nothing is changed.

// datasetIDVariable returns sql with each @@dataset_id (above) written as
// dataset's ID, and whether it changed any.
func datasetIDVariable(sql, dataset string) (string, bool) {
	if dataset == "" || !strings.Contains(strings.ToLower(sql), "dataset_id") {
		return sql, false
	}
	toks, ok := lex(sql)
	if !ok {
		return sql, false
	}
	isVar := func(stmt []token, i int) bool {
		return i >= 2 && stmt[i].is("dataset_id") && stmt[i-1].punct("@") && stmt[i-2].punct("@") &&
			stmt[i-1].pos == stmt[i-2].end && stmt[i].pos == stmt[i-1].end
	}
	for i := range toks {
		if isVar(toks, i) && i >= 3 && toks[i-3].is("SET") {
			return sql, false
		}
	}
	type span struct{ pos, end int }
	var spans []span
	for _, stmt := range splitStatements(toks) {
		body, _, _ := stripControlFlow(stmt)
		if definesBody(body) {
			continue
		}
		for i := range stmt {
			if isVar(stmt, i) {
				spans = append(spans, span{stmt[i-2].pos, stmt[i].end})
			}
		}
	}
	if len(spans) == 0 {
		return sql, false
	}
	lit := "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(dataset) + "'"
	var b strings.Builder
	last := 0
	for _, s := range spans {
		b.WriteString(sql[last:s.pos])
		b.WriteString(lit)
		last = s.end
	}
	b.WriteString(sql[last:])
	return b.String(), true
}
