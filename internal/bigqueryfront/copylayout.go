package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// A copy job's partitioning and clustering (#1759).
//
// BigQuery's documentation (managing-partitioned-tables, "Copy a
// partitioned table", re-read 2026-11-18,
// https://docs.cloud.google.com/bigquery/docs/managing-partitioned-tables):
//
//   - "Copying a partitioned table to a new destination table": "All of
//     the partitioning information is copied with the table. The new table
//     and the old table will have identical partitions."
//   - "Copying a non-partitioned table into an existing partitioned
//     table": "This operation is only supported for ingestion-time
//     partitioning. BigQuery copies the source data into the partition
//     that represents the current date." "This operation is not supported
//     for time-unit column-partitioned or integer-range partitioned
//     tables": the job fails.
//   - "Copying a partitioned table into another partitioned table": "The
//     partition specifications for the source and destination tables must
//     match": the job fails when they do not.
//   - "Copying a partitioned table into a non-partitioned table": "The
//     destination table remains unpartitioned", whatever the
//     writeDisposition (WRITE_TRUNCATE "uses the schema and table
//     constraints from the source table", JobConfigurationTableCopy, not
//     its partitioning).
//   - "Copying multiple partitioned tables": "the source tables can't
//     contain a mixture of partitioned and non-partitioned tables", and
//     "the partition specifications for all source tables must match the
//     destination table's partition specification";
//     managing-tables: "all source tables must have identical schemas,
//     including any partitioning or clustering".
//   - "Copying a partitioned table that has a clustering specification":
//     "If you copy into a new table, all of the clustering information is
//     copied with the table"; "If you copy into an existing table, then the
//     cluster specifications for the source and destination tables must
//     match": a partitioned, clustered source into any table that exists
//     and is clustered otherwise, or not at all (one not partitioned
//     included), fails the job (#1762).
//   - A new destination's partition expiration: table-snapshots-intro,
//     having said a snapshot does not keep it, "To retain the partition
//     expiration information, copy the table instead": a copy keeps it.
//   - The job's error reason, "invalid": error-messages documents it as
//     "any type of invalid input other than an invalid query".
//
// A table snapshot (operationType SNAPSHOT): "the partition expiration
// information isn't retained in the snapshot. The snapshotted table uses
// the destination dataset's default partition expiration instead"
// (https://docs.cloud.google.com/bigquery/docs/table-snapshots-intro).
//
// UNVERIFIED (docs re-read 2026-11-18, #1762: managing-partitioned-tables,
// managing-tables, partitioned-tables, clustered-tables,
// manage-clustered-tables, bq-cli-reference's bq cp, the copy samples, the
// Job resource's JobConfigurationTableCopy, error-messages,
// table-clones-intro, table-clones-create, table-snapshots-intro,
// table-snapshots-create, table-snapshots-restore,
// customer-managed-encryption, and a search of docs.cloud.google.com for
// "Incompatible table partitioning specification", which found none,
// https://docs.cloud.google.com/bigquery/docs/managing-partitioned-tables):
// none defines "partition specification" or gives the message of a
// mismatch. CloudBurrow compares the partitioning type, its unit, column and
// integer range, and the clustering columns, not partition expiration or
// require_partition_filter, with its own wording. It carries a new
// destination's requirePartitionFilter, which no copy page names. A
// non-partitioned clustered source into a table neither partitioned nor
// clustered is copied, the clustering rule naming partitioned sources only.
// CLONE and RESTORE, which no page gives a layout rule for, make their new
// table the same way (table-clones-intro says only that a clone is used
// "in the same way as you use standard tables", partitioning and
// clustering included). A table's description and labels are not copied by a
// copy job: no copy page names them (DDL's CREATE TABLE COPY inherits "the
// table options metadata", but it is a statement, not a copy job).

// layoutFields are the Table fields of partitioning and clustering that a
// copy carries into a new destination.
var layoutFields = []string{"timePartitioning", "rangePartitioning", "clustering", "requirePartitionFilter"}

// layoutOf is the partitioning and clustering of a tables.get answer, got:
// each of layoutFields it sets, as given.
func layoutOf(got []byte) map[string]json.RawMessage {
	var all map[string]json.RawMessage
	if json.Unmarshal(got, &all) != nil {
		return nil
	}
	out := map[string]json.RawMessage{}
	for _, k := range layoutFields {
		if v, ok := all[k]; ok && len(v) > 0 && string(v) != "null" {
			out[k] = v
		}
	}
	return out
}

// layoutSpec is what of a table's layout two tables must share
// (UNVERIFIED, docs re-read 2026-11-18, above): partitioning and clustering.
type layoutSpec struct {
	partition string // "" when not partitioned
	cluster   string // "" when not clustered
}

// specOf reads layout's partition and cluster specifications, written as
// they are compared and as the job's error names them.
func specOf(layout map[string]json.RawMessage) layoutSpec {
	var s layoutSpec
	var tp struct {
		Type  string `json:"type"`
		Field string `json:"field"`
	}
	var rp struct {
		Field string `json:"field"`
		Range struct {
			Start    json.Number `json:"start"`
			End      json.Number `json:"end"`
			Interval json.Number `json:"interval"`
		} `json:"range"`
	}
	var cl struct {
		Fields []string `json:"fields"`
	}
	switch {
	case len(layout["timePartitioning"]) > 0 && json.Unmarshal(layout["timePartitioning"], &tp) == nil:
		unit := strings.ToUpper(tp.Type)
		if unit == "" {
			unit = "DAY"
		}
		field := tp.Field
		if field == "" {
			field = "_PARTITIONTIME"
		}
		s.partition = fmt.Sprintf("time(type:%s, field:%s)", unit, strings.ToLower(field))
	case len(layout["rangePartitioning"]) > 0 && json.Unmarshal(layout["rangePartitioning"], &rp) == nil:
		s.partition = fmt.Sprintf("range(field:%s, start:%s, end:%s, interval:%s)", strings.ToLower(rp.Field),
			rp.Range.Start, rp.Range.End, rp.Range.Interval)
	}
	if len(layout["clustering"]) > 0 && json.Unmarshal(layout["clustering"], &cl) == nil && len(cl.Fields) > 0 {
		s.cluster = strings.ToLower(strings.Join(cl.Fields, ", "))
	}
	return s
}

func specText(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// layoutMismatch is why a copy of sources, with their layouts, into dest
// (destLayout, nil when dest does not exist) fails (above), or "".
func layoutMismatch(project string, sources []tableRef, layouts []map[string]json.RawMessage, dest tableRef,
	destLayout map[string]json.RawMessage, destExists bool) string {
	first := specOf(layouts[0])
	for i := 1; i < len(sources); i++ {
		s := specOf(layouts[i])
		if (s.partition == "") != (first.partition == "") {
			// UNVERIFIED wording (docs re-read 2026-11-18, above).
			return fmt.Sprintf("Invalid copy job: the source tables can't contain a mixture of partitioned and "+
				"non-partitioned tables, and %s is %s but %s is %s.", tableName(project, sources[0]),
				partitionedWord(first), tableName(project, sources[i]), partitionedWord(s))
		}
		if s != first {
			// UNVERIFIED wording (docs re-read 2026-11-18, above).
			return fmt.Sprintf("Invalid copy job: the source tables must have identical schemas, including any "+
				"partitioning or clustering; %s has partitioning %s and clustering %s, and %s has partitioning %s "+
				"and clustering %s.", tableName(project, sources[0]), specText(first.partition), specText(first.cluster),
				tableName(project, sources[i]), specText(s.partition), specText(s.cluster))
		}
	}
	if !destExists {
		return ""
	}
	d := specOf(destLayout)
	ingestion := strings.HasSuffix(d.partition, "field:_partitiontime)")
	switch {
	case first.partition == "" && d.partition != "" && !ingestion:
		// UNVERIFIED wording (docs re-read 2026-11-18, above).
		return fmt.Sprintf("Incompatible table partitioning specification. Copying a non-partitioned table into "+
			"%s, which is partitioned by %s, is only supported for ingestion-time partitioning.",
			tableName(project, dest), d.partition)
	case first.partition != "" && d.partition != "" && first.partition != d.partition:
		// UNVERIFIED wording (docs re-read 2026-11-18, above).
		return fmt.Sprintf("Incompatible table partitioning specification. Expects partitioning specification %s, "+
			"but input partitioning specification is %s.", d.partition, first.partition)
	case first.cluster != "" && first.cluster != d.cluster && (first.partition != "" || d.partition != "" || d.cluster != ""):
		// A partitioned, clustered source: "the cluster specifications for
		// the source and destination tables must match", into a table not
		// clustered too (#1762). A non-partitioned one into a table neither
		// partitioned nor clustered is not compared: UNVERIFIED (docs
		// re-read 2026-11-18, above), the rule naming partitioned sources
		// only. UNVERIFIED wording.
		return fmt.Sprintf("Incompatible table clustering specification. Expects clustering specification %s, but "+
			"input clustering specification is %s.", specText(d.cluster), first.cluster)
	}
	return ""
}

func partitionedWord(s layoutSpec) string {
	if s.partition == "" {
		return "not partitioned"
	}
	return "partitioned"
}

// newTableLayout is the Table fields that make a new copy destination
// partitioned and clustered as layout, its first source's (above): for
// SNAPSHOT, without the partition expiration, and with the destination
// dataset's defaultPartitionExpirationMs instead when it has one.
func (f front) newTableLayout(r *http.Request, layout map[string]json.RawMessage, snapshot bool, dataset string) map[string]any {
	out := map[string]any{}
	for k, v := range layout {
		var x any
		if json.Unmarshal(v, &x) == nil {
			out[k] = x
		}
	}
	if tp, ok := out["timePartitioning"].(map[string]any); ok && snapshot {
		delete(tp, "expirationMs")
		if status, got := f.get(r, "/datasets/"+url.PathEscape(dataset)); status == http.StatusOK {
			var ds struct {
				Default string `json:"defaultPartitionExpirationMs"`
			}
			if json.Unmarshal(got, &ds) == nil && ds.Default != "" {
				tp["expirationMs"] = ds.Default
			}
		}
	}
	return out
}

// requiresFilter reports whether Table fields extra set
// require_partition_filter, which the front checks reads of (#1390,
// tableoptions.go).
func requiresFilter(extra map[string]any) bool {
	if extra["requirePartitionFilter"] == true {
		return true
	}
	tp, _ := extra["timePartitioning"].(map[string]any)
	return tp != nil && tp["requirePartitionFilter"] == true
}

// makeCopyTable makes a copy job's table ref with schema and Table fields
// extra (makeTableWith), and has the front check reads of it when it
// requires a partition filter.
func (f front) makeCopyTable(r *http.Request, ref tableRef, schema json.RawMessage, extra map[string]any) string {
	if why := f.makeTableWith(r, ref, schema, extra); why != "" {
		return why
	}
	if requiresFilter(extra) {
		f.tables.watchTable(streamKey(projectOf(f.base), ref.DatasetID, ref.TableID))
	}
	return ""
}

// refuseLayout fails a COPY job of srcs (past, the sources read at a past
// time, copyJob) into dest whose partitioning or clustering do not match
// (layoutMismatch), and reports whether it answered w. A copy of or into a
// partition (partitionCopy), or one a table of is not found, is left to the
// rest of the job.
func (f front) refuseLayout(w http.ResponseWriter, r *http.Request, srcs []tableRef, past map[int]copyTable, dest tableRef) bool {
	for _, t := range append(append([]tableRef{}, srcs...), dest) {
		if _, _, ok := decorated(t.TableID); ok {
			return false
		}
	}
	layouts := make([]map[string]json.RawMessage, len(srcs))
	for i, s := range srcs {
		t, ok := past[i]
		if !ok {
			t = f.copyTableMeta(r, s)
		}
		if !t.exists {
			return false
		}
		layouts[i] = t.layout
	}
	d := f.copyTableMeta(r, dest)
	why := layoutMismatch(projectOf(f.base), srcs, layouts, dest, d.layout, d.exists)
	if why == "" {
		return false
	}
	if j, ok := f.startOwnJob(w, r, "a copy job"); ok {
		f.finishOwnJob(w, r, j, "COPY", "copy", nil, &rowError{Reason: "invalid", Message: why})
	}
	return true
}
