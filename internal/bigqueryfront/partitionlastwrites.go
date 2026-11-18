package bigqueryfront

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// A partition's last write, however long ago (#1458).
//
// BigQuery: PARTITIONS' last_modified_time is "the most recent time that
// data was written to the partition", TABLE_STORAGE's
// storage_last_modified_time "the most recent time that data was written
// to the table", and a partition (or a table that is not partitioned) "not
// modified for 90 consecutive days" is long-term storage
// (https://cloud.google.com/bigquery/docs/information-schema-partitions,
// https://cloud.google.com/bigquery/docs/information-schema-table-storage,
// https://cloud.google.com/bigquery/pricing#storage). The engine records
// every write to a table (DML, MERGE, TRUNCATE TABLE, tabledata.insertAll,
// loads, copies, query destinations, a partition's delete) in the table's
// change table (googlesqlite patch 0036), seven days of them, and since
// googlesqlite patch 0090 moves each record the change table lets go of
// (its own seven days, or the front's time travel window,
// changehistoryretention.go) to the table's kept table, in the database,
// so it is there after a restart. A partition's last write is the latest
// record of the two whose row is in it. The kept table is compacted, when
// read, to the latest record of each partition but those still in the
// fail-safe period, which TABLE_STORAGE's fail_safe_physical_bytes counts
// (keptCompact). A table whose change table let go of records before patch
// 0090 has lost them: where no record of a partition is left, its last
// write is unknown (NULL).

// keptTableSuffix ends the name of a table's kept table in the engine
// (googlesqlite patch 0090).
const keptTableSuffix = "$kept" + changeTableSuffix

// failSafeHours is BigQuery's fail-safe period, after the time travel
// window: "BigQuery retains deleted data for an additional seven days"
// (https://cloud.google.com/bigquery/docs/time-travel#fail-safe).
const failSafeHours = 7 * 24

// keptWrites is the subquery of the records of table's kept table, as
// changeWrites is of its change table, or "" when it has none (a table with
// no change table, or an engine before googlesqlite patch 0090).
func (f front) keptWrites(r *http.Request, project, dataset, table string, partitionTime bool) string {
	path := quotePath([]string{project, dataset, table + keptTableSuffix})
	cols := "*"
	if partitionTime {
		cols = "*, _PARTITIONTIME"
	}
	if _, msg := f.scalar(r, "SELECT COUNT(*) FROM (SELECT "+cols+" FROM "+path+" WHERE FALSE)"); msg != "" {
		return ""
	}
	return "(SELECT " + cols + " FROM " + path + ")"
}

// allWrites is the subquery (pid, t, k) of every write record of table the
// engine holds, its change table's and its kept table's: the partition of
// the row written (pid, an expression of the table's columns), the time
// and the _CHANGE_TYPE; or "" when it holds none.
func (f front) allWrites(r *http.Request, project, dataset, table string, pm partitionMeta, pid string) string {
	var parts []string
	for _, src := range []string{f.changeWrites(r, project, dataset, table, pm.ingestion()), f.keptWrites(r, project, dataset, table, pm.ingestion())} {
		if src != "" {
			parts = append(parts, "SELECT "+pid+" AS pid, _CHANGE_TIMESTAMP AS t, _CHANGE_TYPE AS k FROM "+src)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	q := parts[0]
	for _, p := range parts[1:] {
		q += " UNION ALL " + p
	}
	return "(" + q + ")"
}

// lastWrites is the subquery (pid, t) of each partition's last write of
// table (above), pid NULL for a table that is not partitioned, or "" when
// the engine holds no record of its writes. It compacts the kept table
// first (keptCompact).
func (f front) lastWrites(r *http.Request, project, dataset, table string, pm partitionMeta, pid string, hours int64) string {
	f.keptCompact(r, project, dataset, table, pm, pid, hours)
	all := f.allWrites(r, project, dataset, table, pm, pid)
	if all == "" {
		return ""
	}
	return "(SELECT pid, MAX(t) AS t FROM " + all + " GROUP BY 1)"
}

// keptCompact deletes, of table's kept table, every record but the latest
// of each partition (by pid) and those within the time travel window and
// the fail-safe period after it (hours of the window, then
// failSafeHours), whose deleted rows TABLE_STORAGE counts. It runs only
// when there is such a record, and a failure leaves the table as it is:
// the reads are the same either way.
func (f front) keptCompact(r *http.Request, project, dataset, table string, pm partitionMeta, pid string, hours int64) {
	kept := f.keptWrites(r, project, dataset, table, pm.ingestion())
	if kept == "" {
		return
	}
	path := quotePath([]string{project, dataset, table + keptTableSuffix})
	before := f.now().Add(-time.Duration(hours+failSafeHours) * time.Hour).UnixMicro()
	old := fmt.Sprintf("googlesqlite_change_micros < %d", before)
	ranked := "(SELECT rowid AS cloudburrow_order, googlesqlite_change_micros AS cloudburrow_micros, ROW_NUMBER() OVER (PARTITION BY " + pid +
		" ORDER BY googlesqlite_change_micros DESC, rowid DESC) AS cloudburrow_rank FROM " + path + ")"
	if n, ok := f.scalarInt(r, "SELECT COUNT(*) FROM "+ranked+" WHERE cloudburrow_rank > 1 AND cloudburrow_micros < "+strconv.FormatInt(before, 10)); !ok || n == 0 {
		return
	}
	_ = f.runDML(r, "DELETE FROM "+path+" WHERE "+old+" AND rowid IN (SELECT cloudburrow_order FROM "+ranked+" WHERE cloudburrow_rank > 1)")
}

// tierBytes are TABLE_STORAGE's active_logical_bytes and
// long_term_logical_bytes of a table made 90 days ago or more, as SQL: the
// logical bytes (bytes, an expression of a row's columns) of its
// partitions written in the last 90 days, and of those not, by each
// partition's last write (lastWrites); both NULL when a partition holding
// rows has no write the engine still holds (above).
func (f front) tierBytes(r *http.Request, project, dataset, table string, pm partitionMeta, bytes string, hours int64) (active, long string) {
	id, ok := partitionIDExpr(pm)
	if !ok || bytes == "" {
		return "NULL", "NULL"
	}
	pid := id
	if pid == "" {
		pid = "CAST(NULL AS STRING)"
	}
	last := f.lastWrites(r, project, dataset, table, pm, pid, hours)
	if last == "" {
		return "NULL", "NULL"
	}
	path := quotePath([]string{project, dataset, table})
	cut := "TIMESTAMP_MICROS(" + strconv.FormatInt(f.now().Add(-90*24*time.Hour).UnixMicro(), 10) + ")"
	got, msg := f.scalars(r, "SELECT IFNULL(SUM(IF(w.t > "+cut+", p.b, 0)), 0), IFNULL(SUM(IF(w.t <= "+cut+", p.b, 0)), 0), COUNTIF(w.t IS NULL) FROM "+
		"(SELECT "+pid+" AS pid, IFNULL(SUM("+bytes+"), 0) AS b FROM "+path+" GROUP BY 1) AS p LEFT JOIN "+last+
		" AS w ON p.pid = w.pid OR p.pid IS NULL AND w.pid IS NULL", 3)
	if msg != "" || got[2] > 0 {
		return "NULL", "NULL"
	}
	return strconv.FormatInt(got[0], 10), strconv.FormatInt(got[1], 10)
}

// failSafeBytesExpr is the SQL, over allWrites, of the logical bytes of a
// table's rows deleted or replaced after the time travel window's last
// hours and within the fail-safe period before it: "deleted or changed
// data retained after the time-travel window"
// (https://cloud.google.com/bigquery/docs/information-schema-table-storage).
func failSafeBytesExpr(bytes string, hours int64) string {
	h := strconv.FormatInt(hours, 10)
	return "IFNULL(SUM(IF(_CHANGE_TYPE = 'DELETE' AND _CHANGE_TIMESTAMP < TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL " + h +
		" HOUR) AND _CHANGE_TIMESTAMP >= TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL " + strconv.FormatInt(hours+failSafeHours, 10) +
		" HOUR), " + bytes + ", 0)), 0)"
}
