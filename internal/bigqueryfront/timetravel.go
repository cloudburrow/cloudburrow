package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Time travel: FOR SYSTEM_TIME AS OF a past time (#1460).
//
// Measured first through the front: a past time inside the time travel
// window was 501, the emulator kept no history. BigQuery documents
// (https://cloud.google.com/bigquery/docs/time-travel, read 2026-10-16)
// that a query reads "changed or deleted data from any point within the
// time travel window", the dataset's maxTimeTravelHours (two to seven
// days, seven by default) before now, and no earlier than the table's
// creation; partitionreads.go's systemTime checks the window and fails a
// time before it as BigQuery documents.
//
// The engine records every change of every table (#1418, googlesqlite
// patch 0036) in its change table, with the time of its transaction, for
// seven days, and (googlesqlite patch 0041) the changed row's rowid, the
// order of the changes, and a START row of when the change table was
// made. The table as it was at a time T is then, of each rowid, the last
// change at or before T, when it is not a DELETE: the front writes the
// read of the table as that subquery of its change table and runs the
// query so; the job keeps the statement the client sent.
//
// Where the change table cannot answer, the read stays 501, each with its
// reason:
//   - a table whose change table started after T (a table made before
//     googlesqlite patch 0036); an ALTER TABLE that rebuilds the table and
//     CREATE OR REPLACE TABLE carry the history on (#1466,
//     changehistorycarry.go), the rows a rebuild copied marked CARRY and
//     the old ones ended by an END, which this read takes as a delete;
//   - a change table made before patch 0041, which has no row identities;
//   - an ingestion-time partitioned table whose change table was made
//     before googlesqlite patch 0050, which keeps each change's
//     _PARTITIONTIME (#1458);
//   - a query that reads _PARTITIONTIME or _PARTITIONDATE of a past time,
//     has a * and names the time by a query parameter (below).
//
// An ingestion-time partitioned table's past rows have their
// _PARTITIONTIME (and, by day, _PARTITIONDATE) when the query names them
// (#1458). A query that names them and has a * in a select list reads
// the past rows from a scratch table (scratch.go) the front fills with
// them first, partitioned as the table is, each row in its partition of
// then, so that * gives the table's columns and not the pseudo-columns,
// as of the table itself; the scratch table is deleted once the query is
// answered. A snapshot, clone or copy of a past time of such a table
// (CREATE SNAPSHOT TABLE, CREATE TABLE ... CLONE, a copy job of
// table@time) is made partitioned as the table is, with each row's
// _PARTITIONTIME of then (#1458): its subquery gives each row's
// _PARTITIONTIME as _cb_pt, which insertSelect writes (copyjob.go). A partition decorator with FOR SYSTEM_TIME AS OF reads the
// partition's rows of then: those of the past rows the decorator's
// condition holds of (partitionRows); the streaming buffer of then is not
// kept, so a row in it then reads in its partition.
//
// The table is read with its columns of now; a column the change table
// does not have (it is made again with the table's new columns) leaves
// the read 501 as above.

// pastPartitionTime names the column of a copy's past rows that holds
// each row's _PARTITIONTIME (pastRows).
const pastPartitionTime = "_cb_pt"

// historyRead returns the subquery that reads the table of tr, with m, as
// it was at past (microseconds since the epoch), the value of expr, or the
// status and message of why it cannot (above). ref names the table in a
// message. The subquery keeps expr, which may name a query parameter the
// engine fails a query for leaving unused.
// A scratch table it fills is added to scratches, for the caller to
// delete.
func (f front) historyRead(r *http.Request, tr tableRead, m partitionMeta, ref string, past int64, expr string,
	scratches *[]string) (string, int, string) {
	notBuilt := func(why string) (string, int, string) {
		return "", http.StatusNotImplemented, "Not implemented here: FOR SYSTEM_TIME AS OF a past time of " + ref + ": " + why +
			" (docs/compatibility.md). Nothing was run."
	}
	where := ""
	if tr.decorator != "" {
		var code int
		var msg string
		if where, code, msg = partitionRows(m, tr.decorator, ref+"$"+tr.decorator); code != 0 {
			return "", code, msg
		}
	}
	pseudo := false
	if m.ingestion() {
		for _, t := range tr.stmt {
			pseudo = pseudo || t.is("_PARTITIONTIME") || t.is("_PARTITIONDATE")
		}
		if pseudo && hasSelectStar(tr.stmt) && strings.Contains(expr, "@") {
			return notBuilt("CloudBurrow does not read _PARTITIONTIME or _PARTITIONDATE of a past time named by a query " +
				"parameter in a query whose select list has a *")
		}
	}
	var text string
	var code int
	var msg string
	if pseudo && m.ingestion() && hasSelectStar(tr.stmt) {
		text, code, msg = f.pastScratch(r, tr, m, ref, past, where, scratches)
	} else {
		text, code, msg = f.pastRows(r, tr.dataset, tr.table, m, ref, past, expr, true, pseudo, where)
	}
	if code != 0 {
		return "", code, msg
	}
	if n := tr.next; n >= len(tr.stmt) || !tr.stmt[n].is("AS") && (tr.stmt[n].kind != tokWord && tr.stmt[n].kind != tokQuoted ||
		tr.stmt[n].kind == tokWord && reservedWords[strings.ToUpper(tr.stmt[n].text)] || tr.stmt[n].is("FOR")) {
		text += " AS " + quotePath([]string{tr.table})
	}
	return text, 0, ""
}

// hasSelectStar reports whether stmt has a select list's * (SELECT *,
// x.*), not COUNT(*) or a product.
func hasSelectStar(stmt []token) bool {
	for i, t := range stmt {
		if t.kind != tokWord && t.kind != tokQuoted && t.text == "*" && i > 0 {
			p := stmt[i-1]
			if p.is("SELECT") || p.is("DISTINCT") || p.is("ALL") || p.text == "," || p.text == "." {
				return true
			}
		}
	}
	return false
}

// pastScratch writes the rows of the ingestion-time partitioned table of
// tr, with m, as it was at past into a scratch table partitioned as it
// is, each row in its partition of then, and returns the scratch table's
// path to read in its place (above).
func (f front) pastScratch(r *http.Request, tr tableRead, m partitionMeta, ref string, past int64, where string,
	scratches *[]string) (string, int, string) {
	sub, code, msg := f.pastRows(r, tr.dataset, tr.table, m, ref, past, "TRUE", false, false, where)
	if code != 0 {
		return "", code, msg
	}
	schema, err := json.Marshal(m.Schema)
	if err != nil {
		return "", http.StatusInternalServerError, err.Error()
	}
	scratch := tableRef{DatasetID: resultsDataset, TableID: strings.Replace(scratchTable(), "_replace_", "_past_", 1)}
	if why := f.makeTableWith(r, scratch, schema, map[string]any{"timePartitioning": m.TimePartitioning}); why != "" {
		return "", http.StatusInternalServerError, "cloudburrow: could not hold the past rows of " + ref + ": " + why
	}
	*scratches = append(*scratches, scratch.TableID)
	if why := f.insertSelect(r, scratch, []copyTable{{ref: scratch, fields: m.Schema.Fields, from: sub,
		partitionTime: m.TimePartitioning}}); why != "" {
		return "", http.StatusInternalServerError, "cloudburrow: could not hold the past rows of " + ref + ": " + why
	}
	return quotePath([]string{projectOf(f.base), resultsDataset, scratch.TableID}), 0, ""
}

// historySubquery is historyRead's subquery of dataset.table, without an
// alias; a CREATE SNAPSHOT TABLE or CLONE and a copy job of table@time
// read a past time through it too (#1460, cloneddl.go, dropped.go). Of an
// ingestion-time partitioned table, it gives each row's _PARTITIONTIME as
// _cb_pt too (#1458), and the caller sets its copyTable's partitionTime.
func (f front) historySubquery(r *http.Request, dataset, table string, m partitionMeta, ref string, past int64, expr string) (string, int, string) {
	return f.pastRows(r, dataset, table, m, ref, past, expr, false, false, "")
}

// pastRows is historySubquery. inQuery is a query's read; pseudo gives
// its subquery an ingestion-time partitioned table's _PARTITIONTIME (and,
// by day, _PARTITIONDATE) columns, and any other read of one is given
// _PARTITIONTIME as _cb_pt (#1458); where, when not "", is a partition
// decorator's condition the rows must hold (#1458).
func (f front) pastRows(r *http.Request, dataset, table string, m partitionMeta, ref string, past int64, expr string,
	inQuery, pseudo bool, where string) (string, int, string) {
	notBuilt := func(why string) (string, int, string) {
		return "", http.StatusNotImplemented, "Not implemented here: FOR SYSTEM_TIME AS OF a past time of " + ref + ": " + why +
			" (docs/compatibility.md). Nothing was run."
	}
	ingestion := m.ingestion()
	project := projectOf(f.base)
	var cols []string
	for _, fl := range m.Schema.Fields {
		cols = append(cols, quotePath([]string{fl.Name}))
	}
	if len(cols) == 0 {
		return notBuilt("the table has no columns")
	}
	list := strings.Join(cols, ", ")
	out := list
	inner := list
	if ingestion {
		// The change table keeps each change's _PARTITIONTIME
		// (googlesqlite patch 0050).
		inner += ", _PARTITIONTIME"
		if !inQuery {
			out += ", _PARTITIONTIME AS " + pastPartitionTime
		} else if pseudo {
			out += ", _PARTITIONTIME"
			if m.ingestionUnit() == "DAY" {
				out += ", DATE(_PARTITIONTIME) AS _PARTITIONDATE"
			}
		}
	}
	changes := quotePath([]string{project, dataset, table + changeTableSuffix})
	legacy := false
	body, err := json.Marshal(queryOptions{UseLegacySQL: &legacy, Query: fmt.Sprintf(
		"SELECT COUNTIF(_CHANGE_TYPE = 'START' AND googlesqlite_change_micros > %d), COUNT(googlesqlite_change_rowid) + COUNT(rowid) "+
			"FROM (SELECT %s, _CHANGE_TYPE, googlesqlite_change_micros, googlesqlite_change_rowid, rowid FROM %s)", past, inner, changes)})
	if err != nil {
		return "", http.StatusInternalServerError, err.Error()
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	if status != http.StatusOK && ingestion {
		return notBuilt("CloudBurrow's history of the table keeps no _PARTITIONTIME (its change table was made before " +
			"CloudBurrow kept it), or none it can read at that time")
	}
	if status != http.StatusOK {
		return notBuilt("CloudBurrow keeps no history of the table that it can read at that time; it records a table's " +
			"changes from when it was made")
	}
	var res struct {
		Rows []struct {
			F []struct {
				V *string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if json.Unmarshal(got, &res) != nil || len(res.Rows) != 1 || len(res.Rows[0].F) != 2 || res.Rows[0].F[0].V == nil {
		return "", http.StatusInternalServerError, "cloudburrow: could not read the history of " + ref
	}
	if *res.Rows[0].F[0].V != "0" {
		return notBuilt("CloudBurrow's history of the table starts after that time (a table made before " +
			"CloudBurrow recorded changes)")
	}
	if where != "" {
		where = " AND " + where
	}
	text := fmt.Sprintf("(SELECT %s FROM (SELECT %s, _CHANGE_TYPE AS _cloudburrow_change, "+
		"ROW_NUMBER() OVER (PARTITION BY googlesqlite_change_rowid ORDER BY rowid DESC) AS _cloudburrow_last "+
		"FROM %s WHERE _CHANGE_TYPE != 'START' AND googlesqlite_change_micros <= %d AND (%s) IS NOT NULL) "+
		"WHERE _cloudburrow_last = 1 AND _cloudburrow_change NOT IN ('DELETE', '"+changeEnd+"')%s)", out, inner, changes, past, expr, where)
	return text, 0, ""
}
