package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Change history and multi-statement transactions (#1503).
//
// BigQuery: "You can't use the CHANGES function on tables that have had
// multi-statement transactions committed to them within the requested
// time window"
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/time-series-functions,
// read 2026-10-17). The front applies a transaction's DML at its COMMIT
// as one request (transactions.go), so it knows the tables the
// transaction wrote: noteTransaction keeps, for each, the change time of
// the commit (the engine's, the same for every row the transaction
// wrote, googlesqlite patch 0036) in the table's settings
// (tablesettings.json), and changeReads refuses a CHANGES range that
// holds one (committedIn). APPENDS has no such limit, and reads them.

// noteTransaction keeps the commit of kept, a transaction's DML statements
// just committed, on each table they write (above).
func (f front) noteTransaction(r *http.Request, q queryOptions, kept []string) {
	if f.tables == nil {
		return
	}
	project := projectOf(f.base)
	seen := map[string]bool{}
	for _, text := range kept {
		toks, ok := lex(text)
		if !ok {
			continue
		}
		parts := writtenTable(toks)
		if len(parts) == 1 && defaultDatasetOf(q) != "" {
			parts = []string{defaultDatasetOf(q), parts[0]}
		}
		dataset, table, named := resolve(parts, project)
		if !named || seen[dataset+"."+table] {
			continue // a TEMP table, or another project's
		}
		seen[dataset+"."+table] = true
		st, got := f.get(r, tablePath(dataset, table))
		var meta map[string]any
		if st != http.StatusOK || json.Unmarshal(got, &meta) != nil {
			continue
		}
		at, ok := f.scalarInt(r, "SELECT MAX(googlesqlite_change_micros) FROM "+
			quotePath([]string{project, dataset, table + changeTableSuffix})+" WHERE _CHANGE_TYPE != 'START'")
		if !ok {
			continue // no change table: no history to read
		}
		f.tables.addTransaction(streamKey(project, dataset, table), str(meta, "creationTime"), at)
	}
}

// committedIn is the change time of a multi-statement transaction s
// keeps in [start, stop), and true; false when it keeps none there.
func committedIn(s tableSetting, start, stop int64) (int64, bool) {
	for _, at := range s.Transactions {
		if at >= start && at < stop {
			return at, true
		}
	}
	return 0, false
}

// committedRefused is CHANGES's refusal of a range of ref holding the
// transaction committed at at (wording CloudBurrow's, UNVERIFIED).
func committedRefused(ref string, at int64) string {
	return fmt.Sprintf("CHANGES cannot be used over a time range in which a multi-statement transaction was committed to "+
		"the table %s (committed at %s); choose a range that leaves it out", ref, micros(at))
}

// scalarInt runs sql, a query of one INT64 value, on the emulator; false
// when it fails or the value is NULL.
func (f front) scalarInt(r *http.Request, sql string) (int64, bool) {
	legacy := false
	body, err := json.Marshal(queryOptions{UseLegacySQL: &legacy, Query: sql})
	if err != nil {
		return 0, false
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	if status != http.StatusOK {
		return 0, false
	}
	var res struct {
		Rows []struct {
			F []struct {
				V *string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if json.Unmarshal(got, &res) != nil || len(res.Rows) != 1 || len(res.Rows[0].F) != 1 || res.Rows[0].F[0].V == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(*res.Rows[0].F[0].V), 10, 64)
	return n, err == nil
}
