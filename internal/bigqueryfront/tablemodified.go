package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// A table's lastModifiedTime after its data changes (#2105).
//
// BigQuery's Table.lastModifiedTime is "the time when this table was last
// modified, in milliseconds since the epoch"
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/tables#Table),
// and a change to its rows is a modification: Dataform's table update
// triggers (v1beta1 WorkflowTriggerConfig) run a workflow when it moves.
// Measured against the pinned emulator through the front on 2026-11-22: an
// INSERT of one row answered numDmlAffectedRows 1 and tables.get gave the
// creation's lastModifiedTime unchanged, as it does after any DML, load,
// streaming insert or Storage Write.
//
// So at each tables.get of a TABLE the front fingerprints its rows (their
// count and two sums of their FARM_FINGERPRINTs, as mvCurrent does for a
// materialized view) and keeps the fingerprint with the table's settings;
// when it differs from the one kept, the rows changed, and lastModifiedTime
// becomes the time of the last lone DML statement the front ran on the
// table since (noteWrite), or else the front's clock now. Never earlier
// than the engine's own. What differs: a change made by another path
// (a script, a load, a streaming insert) is dated when tables.get first
// sees it, not when it was made; and a change that leaves the same rows
// (an insert then its delete) is not seen.

// noteWrite records that a lone DML statement changed dataset.table now.
func (f front) noteWrite(dataset, table string) {
	if f.tables == nil || dataset == "" || table == "" {
		return
	}
	key := streamKey(projectOf(f.base), dataset, table)
	now := f.buffers.clock().UnixMilli()
	f.tables.mu.Lock()
	defer f.tables.mu.Unlock()
	if f.tables.m == nil {
		f.tables.m = map[string]tableSetting{}
	}
	t := f.tables.m[key]
	t.Written = now
	f.tables.m[key] = t
	f.tables.saveLocked()
}

// withModified is a tables.get answer with lastModifiedTime moved to the
// rows' last change (above).
func (f front) withModified(w http.ResponseWriter, r *http.Request, dataset, table string, serve func(http.ResponseWriter)) {
	if f.tables == nil {
		serve(w)
		return
	}
	rec := newRecorder()
	serve(rec)
	var meta map[string]any
	if (rec.status != 0 && rec.status != http.StatusOK) || json.Unmarshal(rec.body.Bytes(), &meta) != nil || meta == nil {
		rec.copyTo(w)
		return
	}
	if typ := str(meta, "type"); typ != "" && typ != "TABLE" {
		rec.copyTo(w)
		return
	}
	def, _ := json.Marshal(map[string]string{"projectId": projectOf(f.base), "datasetId": dataset})
	from := "SELECT * FROM `" + projectOf(f.base) + "`." + quotePath([]string{dataset, table})
	v, msg := f.evalString(r, queryOptions{DefaultDataset: def}, "TO_JSON_STRING((SELECT AS STRUCT COUNT(*) AS n, BIT_XOR(fp) AS x, SUM(MOD(fp, 1000000007)) AS s FROM "+
		"(SELECT FARM_FINGERPRINT(TO_JSON_STRING(t)) AS fp FROM ("+from+") AS t)))")
	if msg != "" || v == nil {
		rec.copyTo(w)
		return
	}
	engine, _ := strconv.ParseInt(str(meta, "lastModifiedTime"), 10, 64)
	key := streamKey(projectOf(f.base), dataset, table)
	created := str(meta, "creationTime")
	f.tables.mu.Lock()
	if f.tables.m == nil {
		f.tables.m = map[string]tableSetting{}
	}
	t := f.tables.m[key]
	if t.Created != created {
		// A table made again starts afresh; a write noted before its
		// first get is kept.
		t = tableSetting{Created: created, Written: t.Written}
	}
	changed := false
	switch {
	case t.Fingerprint == "":
		t.Fingerprint, t.Modified = *v, engine
		if t.Written > engine {
			t.Modified = t.Written
		}
		changed = true
	case t.Fingerprint != *v:
		t.Fingerprint = *v
		t.Modified = f.buffers.clock().UnixMilli()
		if t.Written > 0 && t.Written <= t.Modified {
			t.Modified = t.Written
		}
		changed = true
	}
	t.Written = 0
	f.tables.m[key] = t
	if changed {
		f.tables.saveLocked()
	}
	mod := t.Modified
	f.tables.mu.Unlock()
	if mod <= engine {
		rec.copyTo(w)
		return
	}
	meta["lastModifiedTime"] = strconv.FormatInt(mod, 10)
	writeRecorded(w, rec, meta)
}
