package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The streaming buffer of an ingestion-time partitioned table (#1273).
//
// BigQuery documents that "recently streamed rows to an ingestion time
// partitioned table temporarily have a NULL value for the _PARTITIONTIME
// pseudocolumn", that "newly arriving data is temporarily placed in the
// __UNPARTITIONED__ partition while in the streaming buffer", that it
// "assigns the final non-NULL value of the _PARTITIONTIME column in the
// background", typically within a few minutes, with no SLA, and that
// tables.get answers a streamingBuffer section while there are such rows,
// numRows "excluding any data in the streaming buffer"
// (https://cloud.google.com/bigquery/docs/streaming-data-into-bigquery,
// https://cloud.google.com/bigquery/docs/reference/rest/v2/tables#Table).
// For a daily partitioned table, tabledata.insertAll of `t$YYYYMMDD`
// streams into that partition, within 31 days in the past and 16 days in
// the future of today (UTC); a decorator of an hour-, month- or
// year-partitioned table is not supported.
//
// Measured first, after the engine kept each row's ingestion time (#1277):
// a streamed row read its partition's _PARTITIONTIME at once.
//
// So the front streams a row of an ingestion-time partitioned table into a
// scratch table of the table's schema (the emulator's insertAll, with its
// checks and insertErrors), then moves it into the table with
// _PARTITIONTIME NULL, and keeps a buffer window per table: the partition
// its rows go to (the decorator's, or the time of the first row truncated
// to the table's unit), the time of its oldest row and how many it holds.
// The window flushes streamBufferFor after its oldest row, before the
// front answers any later request: one UPDATE gives every row of the table
// whose _PARTITIONTIME is NULL the window's partition time. A table holds
// one window at most: a row streamed for another partition (another
// decorator, or the clock past the unit) flushes the window first, which
// BigQuery's "no SLA" allows. The windows are kept in the front's state
// directory, so a restart of the front loses none; the emulator's restart
// empties them with the tables.

// The streaming buffer of a table partitioned by a column or an integer
// range (#1273). BigQuery documents that "when you stream data to a
// time-partitioned table, each partition has a streaming buffer", that a
// streamed row of a table partitioned by a DATE, DATETIME or TIMESTAMP
// column "is initially placed in the __UNPARTITIONED__ partition", that
// for a daily partitioned table "only data outside of the date range
// (last 7 days to future 3 days) is extracted to the UNPARTITIONED
// partition", and that a value of the column more than 10 years in the
// past or 1 year in the future is rejected
// (https://cloud.google.com/bigquery/docs/streaming-data-into-bigquery).
// Such a table has no column that tells a buffered row from the others,
// so the engine gives it the pseudo-column _PARTITION_BUFFERED (googlesqlite
// patch 0029, bigquery-emulator patch 0016), which no query of the client
// names: TRUE on a buffered row of __UNPARTITIONED__, FALSE on one
// buffered in its own partition, NULL on every other row. The rows are
// read by queries at once, as BigQuery reads them; a partition decorator
// reads __UNPARTITIONED__ with the TRUE ones and the other partitions
// without them (partitionFilter); tables.get leaves them out of numRows
// and answers streamingBuffer; a copy leaves them out; and an UPDATE,
// DELETE, MERGE or TRUNCATE TABLE that would change one fails, as
// "rows that were recently written ... can't be modified with data
// manipulation language" (https://cloud.google.com/bigquery/docs/data-manipulation-language).
// The window flushes as an ingestion-time table's does, giving every row
// NULL. A table made before the engine had the column streams as it did,
// its rows partitioned at once. An integer-range partitioned table's rows
// go to __UNPARTITIONED__ too: BigQuery's documentation does not set them
// apart.

// DML and rows streamed in the last 30 minutes (#1370). BigQuery documents
// that "rows that were recently written using the BigQuery Storage Write
// API (REST)" (tabledata.insertAll, "previously known as the legacy
// tabledata.insertAll method") "can't be modified" by UPDATE, DELETE,
// MERGE or TRUNCATE: "the recent writes are those that occurred within the
// last 30 minutes. All other rows in the table remain modifiable"
// (https://cloud.google.com/bigquery/docs/data-manipulation-language), on
// any table, partitioned or not, and apart from the streaming buffer's
// flush. So every table made by tables.insert has the engine pseudo-column
// _PARTITION_STREAMED TIMESTAMP (googlesqlite patch 0030, bigquery-emulator
// patch 0018), NULL on every row but a streamed one, whose time the front
// gives it: every insertAll of a table goes through a scratch table as an
// ingestion-time partitioned table's does (streamIngestion). The front
// keeps each table's last stream time (recent), so DML of a table not
// streamed to in 30 minutes reads nothing more, and bufferedChange counts
// the rows the statement would change among those streamed within
// dmlLockFor of the clock. A table made by a CREATE TABLE statement, or
// before the engine had the column, has none: its streamed rows are held
// only while in the buffer, as before. The clock is the front's, which
// the CloudBurrow extension POST /cloudburrow/bigquery/v2/clock:advance
// moves forward (serveClock), so a test passes the 30 minutes at once.

// streamedAt is the engine's pseudo-column of the time a row was streamed
// (above).
const streamedAt = "_PARTITION_STREAMED"

// dmlLockFor is how long DML refuses a streamed row (above).
const dmlLockFor = 30 * time.Minute

// bufferedRows is the engine's pseudo-column of a table partitioned by a
// column that marks the rows of its streaming buffer (above).
const bufferedRows = "_PARTITION_BUFFERED"

// streamBufferFor is how long a streamed row of an ingestion-time
// partitioned table stays in the buffer: BigQuery gives "typically a few
// minutes" and no SLA.
const streamBufferFor = 20 * time.Second

// streamWindow is the streaming buffer of one table.
type streamWindow struct {
	Base    string    `json:"base"` // the front's base, for the flush's reads
	Dataset string    `json:"dataset"`
	Table   string    `json:"table"`
	Unit    string    `json:"unit"`   // DAY, HOUR, MONTH or YEAR
	Target  time.Time `json:"target"` // the partition's start
	Oldest  time.Time `json:"oldest"`
	Rows    int64     `json:"rows"`
	// Column is whether the table is partitioned by a column or an
	// integer range, whose buffered rows _PARTITION_BUFFERED marks.
	Column bool `json:"column,omitempty"`
}

// cond is the condition of the rows of w's table that are in its
// streaming buffer.
func (w streamWindow) cond() string {
	if w.Column {
		return bufferedRows + " IS NOT NULL"
	}
	return "_PARTITIONTIME IS NULL"
}

// streamBuffers are the tables' streaming buffers.
type streamBuffers struct {
	mu   sync.Mutex
	wins map[string]*streamWindow
	now  func() time.Time
	// offset is how far the clock was advanced (serveClock), in
	// nanoseconds.
	offset atomic.Int64
	// stamp is options.streamTimes.
	stamp bool
	// recent is each table's last stream time, by streamKey (above).
	recent map[string]time.Time
	path   string
	logf   func(string, ...any)
}

func streamBuffersStateFile(dir string) string { return filepath.Join(dir, "streambuffers.json") }

func streamKey(project, dataset, table string) string {
	return project + "\x00" + dataset + "\x00" + table
}

func (s *streamBuffers) clock() time.Time {
	if s == nil {
		return time.Now().UTC()
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	return now().Add(time.Duration(s.offset.Load())).UTC()
}

// streamedRecently returns the condition of the rows of the table that
// DML refuses as streamed within dmlLockFor, or "" when it was not
// streamed to in that time (above).
func (s *streamBuffers) streamedRecently(project, dataset, table string) string {
	if s == nil {
		return ""
	}
	now := s.clock()
	s.mu.Lock()
	last, ok := s.recent[streamKey(project, dataset, table)]
	s.mu.Unlock()
	if !ok || !now.Before(last.Add(dmlLockFor)) {
		return ""
	}
	return streamedAt + " > TIMESTAMP '" + now.Add(-dmlLockFor).Format(streamStamp) + "'"
}

// streamStamp is the form of a stream time in SQL.
const streamStamp = "2006-01-02 15:04:05.000000+00"

// streamed records that the table was streamed to at t.
func (s *streamBuffers) streamed(project, dataset, table string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recent == nil {
		s.recent = map[string]time.Time{}
	}
	s.recent[streamKey(project, dataset, table)] = t
	s.saveLocked()
}

// recentFile is where the stream times are kept, beside the windows.
func (s *streamBuffers) recentFile() string {
	return strings.TrimSuffix(s.path, ".json") + "-recent.json"
}

// serveClock serves the CloudBurrow extension that moves the front's
// clock forward (above), POST /cloudburrow/bigquery/v2/clock:advance
// with {"seconds": n}, n not negative (0 reads the clock). It answers
// {"time": RFC 3339}. Windows due by then flush before the next request.
// The engine's clock follows (#1466): engine, when not nil, is sent the
// offset (bigquery-emulator patch 0034, googlesqlite patch 0044), so that
// CURRENT_TIMESTAMP() and the times of a table's change history, which
// time travel, APPENDS and CHANGES read, are the front's clock too.
func (s *streamBuffers) serveClock(w http.ResponseWriter, r *http.Request, engine http.Handler) bool {
	if r.URL.Path != "/cloudburrow/bigquery/v2/clock:advance" {
		return false
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid", "clock:advance takes POST")
		return true
	}
	var body struct {
		Seconds float64 `json:"seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Seconds < 0 {
		writeError(w, http.StatusBadRequest, "invalid", `the body must be {"seconds": n}, n not negative: the clock only moves forward`)
		return true
	}
	s.offset.Add(int64(body.Seconds * float64(time.Second)))
	if engine != nil {
		if msg := syncEngineClock(r, engine, s.offset.Load()); msg != "" {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: the front's clock moved, but the engine's did not: "+msg)
			return true
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"time": s.clock().Format(time.RFC3339Nano)})
	return true
}

// keep restores the windows from path and from then on keeps them there.
func (s *streamBuffers) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	var saved []streamWindow
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the streaming buffers it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the streaming buffers it kept", path)
		saved = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range saved {
		w := saved[i]
		if s.wins == nil {
			s.wins = map[string]*streamWindow{}
		}
		s.wins[streamKey(projectOf(w.Base), w.Dataset, w.Table)] = &w
	}
	s.path, s.logf = path, logf
	if b, err := os.ReadFile(s.recentFile()); err == nil {
		var recent map[string]time.Time
		if json.Unmarshal(b, &recent) == nil {
			s.recent = recent
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	s.saveLocked()
}

func (s *streamBuffers) saveLocked() {
	if s == nil || s.path == "" {
		return
	}
	saved := make([]streamWindow, 0, len(s.wins))
	for _, w := range s.wins {
		saved = append(saved, *w)
	}
	sort.Slice(saved, func(a, b int) bool { return saved[a].Oldest.Before(saved[b].Oldest) })
	b, _ := json.Marshal(saved)
	now := s.clock()
	for k, t := range s.recent {
		if !now.Before(t.Add(dmlLockFor)) {
			delete(s.recent, k)
		}
	}
	rb, _ := json.Marshal(s.recent)
	for _, f := range []struct {
		path string
		b    []byte
	}{{s.path, b}, {s.recentFile(), rb}} {
		tmp := f.path + ".tmp"
		if err := os.WriteFile(tmp, f.b, 0o600); err != nil {
			s.logf("bigquery front: %v", err)
			return
		}
		if err := os.Rename(tmp, f.path); err != nil {
			s.logf("bigquery front: %v", err)
		}
	}
}

func (s *streamBuffers) reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wins, s.recent = nil, nil
	s.saveLocked()
}

// window returns a copy of the table's window, if it has one.
func (s *streamBuffers) window(project, dataset, table string) (streamWindow, bool) {
	if s == nil {
		return streamWindow{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.wins[streamKey(project, dataset, table)]
	if !ok {
		return streamWindow{}, false
	}
	return *w, true
}

// drop forgets the table's window: the table was deleted or replaced.
func (s *streamBuffers) drop(project, dataset, table string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.wins[streamKey(project, dataset, table)]
	_, rok := s.recent[streamKey(project, dataset, table)]
	if ok || rok {
		delete(s.wins, streamKey(project, dataset, table))
		delete(s.recent, streamKey(project, dataset, table))
		s.saveLocked()
	}
}

// flushDue flushes every window older than streamBufferFor (above).
func (s *streamBuffers) flushDue(next http.Handler, r *http.Request) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if len(s.wins) == 0 {
		s.mu.Unlock()
		return
	}
	now := s.clock()
	var due []streamWindow
	for k, w := range s.wins {
		if !now.Before(w.Oldest.Add(streamBufferFor)) {
			due = append(due, *w)
			delete(s.wins, k)
		}
	}
	if len(due) > 0 {
		s.saveLocked()
	}
	s.mu.Unlock()
	for _, w := range due {
		if msg := (front{next: next, base: w.Base}).flushWindow(r, w); msg != "" && s.logf != nil {
			s.logf("bigquery front: could not flush the streaming buffer of %s.%s: %s", w.Dataset, w.Table, msg)
		}
	}
}

// flushWindow gives the buffered rows of w's table their partition time.
func (f front) flushWindow(r *http.Request, w streamWindow) string {
	if w.Column {
		return f.runDML(r, "UPDATE "+quotePath([]string{projectOf(f.base), w.Dataset, w.Table})+" SET "+bufferedRows+
			" = NULL WHERE "+bufferedRows+" IS NOT NULL")
	}
	t := "TIMESTAMP '" + w.Target.Format(sqlTimestamp) + "'"
	set := "_PARTITIONTIME = " + t
	if w.Unit == "DAY" {
		set += ", _PARTITIONDATE = DATE(" + t + ")"
	}
	return f.runDML(r, "UPDATE "+quotePath([]string{projectOf(f.base), w.Dataset, w.Table})+" SET "+set+
		" WHERE _PARTITIONTIME IS NULL")
}

// streamTarget is the partition a row streamed now into the
// ingestion-time partitioned table m goes to: the decorator's, dec, or
// now's. A decorator BigQuery does not take is the status and message.
func streamTarget(m partitionMeta, dec, name string, now time.Time) (time.Time, int, string) {
	unit := m.ingestionUnit()
	if dec == "" {
		switch unit {
		case "HOUR":
			return now.Truncate(time.Hour), 0, ""
		case "MONTH":
			return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), 0, ""
		case "YEAR":
			return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC), 0, ""
		}
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), 0, ""
	}
	if unit != "DAY" {
		// "Streaming using a partition decorator is only supported for
		// daily partitioned tables."
		return time.Time{}, http.StatusBadRequest, "Streaming to a partition decorator is supported only for daily " +
			"partitioned tables: " + name // UNVERIFIED wording
	}
	from, _, ok := partitionBounds("DAY", dec)
	if !ok {
		return time.Time{}, http.StatusBadRequest, "Invalid partition decorator " + name // UNVERIFIED wording
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if from.Before(today.AddDate(0, 0, -31)) || from.After(today.AddDate(0, 0, 16)) {
		// "You can stream to partitions within the last 31 days in the
		// past and 16 days in the future relative to the current date."
		return time.Time{}, http.StatusBadRequest, "The partition " + name + " is outside the allowed bounds: " +
			"you can stream to partitions within the last 31 days in the past and 16 days in the future " +
			"relative to the current date. Use a load or query job to write to it." // UNVERIFIED wording
	}
	return from, 0, ""
}

// streamIngestion carries out tabledata.insertAll of out, the request's
// body as it is to be forwarded, into the ingestion-time partitioned
// table base (with m) of dataset, partition dec ("" for now's), and
// answers rec as the emulator would have (above). It reports false when
// it answered w with a refusal.
func (f front) streamIngestion(w http.ResponseWriter, r *http.Request, dataset, base, dec string, m partitionMeta,
	out []byte, rec *recorder) bool {
	project := projectOf(f.base)
	name := project + ":" + dataset + "." + base
	if dec != "" {
		name += "$" + dec
	}
	now := f.buffers.clock()
	plain := m.TimePartitioning == nil && m.RangePartitioning == nil
	column := !m.ingestion()
	var target time.Time
	if !column && !plain {
		var code int
		var msg string
		if target, code, msg = streamTarget(m, dec, name, now); code != 0 {
			writeError(w, code, reasonOf(code, "invalid"), msg)
			return false
		}
	}
	// The table rounds its NUMERIC and BIGNUMERIC values, the scratch
	// table does not (#1401, decimalrows.go).
	schema, err := json.Marshal(scratchSchema(m.Schema.Fields))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", err.Error())
		return false
	}
	scratch := tableRef{ProjectID: project, DatasetID: dataset, TableID: scratchTable()}
	if why := f.makeTable(r, scratch, schema); why != "" {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not stream the rows: "+why)
		return false
	}
	defer f.send(r, http.MethodDelete, tablePath(scratch.DatasetID, scratch.TableID), nil)
	status, got := f.send(r, http.MethodPost, tablePath(scratch.DatasetID, scratch.TableID)+"/insertAll", out)
	rec.status = status
	rec.body.Reset()
	rec.body.Write([]byte(strings.ReplaceAll(string(got), scratch.TableID, base)))
	rec.Header().Set("Content-Type", "application/json")
	if status != http.StatusOK {
		return true
	}
	n, why := f.scalar(r, "SELECT COUNT(*) FROM "+quotePath([]string{project, dataset, scratch.TableID}))
	if why != "" || n == 0 {
		return true
	}
	cols := make([]string, len(m.Schema.Fields))
	for i, fl := range m.Schema.Fields {
		cols[i] = quoteName(fl.Name)
	}
	list := strings.Join(cols, ", ")
	pcols, pvals, unit := ", "+bufferedRows, ", "+bufferedMark(m, now), ""
	if plain {
		pcols, pvals = "", ""
	} else if !column {
		pcols, pvals, unit = ", _PARTITIONTIME", ", CAST(NULL AS TIMESTAMP)", m.ingestionUnit()
	}
	switch {
	case !column && unit == "DAY":
		pcols, pvals = pcols+", _PARTITIONDATE", pvals+", CAST(NULL AS DATE)"
	}
	// A window for another partition flushes first (above).
	key := streamKey(project, dataset, base)
	f.buffers.mu.Lock()
	old, had := f.buffers.wins[key]
	var flush *streamWindow
	if !plain && had && !old.Target.Equal(target) {
		flush = old
		delete(f.buffers.wins, key)
		had = false
	}
	f.buffers.mu.Unlock()
	if flush != nil {
		if why := f.flushWindow(r, *flush); why != "" {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not flush the streaming buffer: "+why)
			return false
		}
	}
	insert := func(pcols, pvals string) string {
		return f.runDML(r, "INSERT INTO "+quotePath([]string{project, dataset, base})+" ("+list+pcols+") SELECT "+list+
			pvals+" FROM "+quotePath([]string{project, dataset, scratch.TableID}))
	}
	// The time of the rows, which DML reads (#1370, above).
	stamp := ", TIMESTAMP '" + now.Format(streamStamp) + "'"
	why = insert(pcols+", "+streamedAt, pvals+stamp)
	stamped := why == ""
	if why != "" && strings.Contains(why, streamedAt) {
		// A table with no stream time (above): its rows are held only
		// while in the buffer.
		why = insert(pcols, pvals)
	}
	if why != "" && column && !plain && strings.Contains(why, bufferedRows) {
		// A table made before the engine marked the buffer (above): its
		// rows are partitioned at once, as they were.
		why = insert("", "")
		if why == "" {
			if stamped {
				f.buffers.streamed(project, dataset, base, now)
			}
			return true
		}
	}
	if why != "" && strings.Contains(why, "OUT_OF_RANGE") {
		// A value the table's NUMERIC or BIGNUMERIC column refuses
		// (#1401): the client's error, not one to retry.
		writeError(w, http.StatusBadRequest, "invalid", why)
		return false
	}
	if why != "" {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not stream the rows: "+why)
		return false
	}
	if stamped {
		f.buffers.streamed(project, dataset, base, now)
	}
	if plain {
		return true
	}
	f.buffers.mu.Lock()
	if f.buffers.wins == nil {
		f.buffers.wins = map[string]*streamWindow{}
	}
	if cur, ok := f.buffers.wins[key]; ok && cur.Target.Equal(target) {
		cur.Rows += n
	} else {
		f.buffers.wins[key] = &streamWindow{Base: f.base, Dataset: dataset, Table: base, Unit: unit,
			Target: target, Oldest: now, Rows: n, Column: column}
	}
	f.buffers.saveLocked()
	f.buffers.mu.Unlock()
	return true
}

// withStreamingBuffer adds the streamingBuffer section of tables.get to
// meta, the table's resource, while the table has buffered rows, and
// leaves them out of numRows (above).
func (f front) withStreamingBuffer(r *http.Request, dataset, table string, meta map[string]any) bool {
	win, ok := f.buffers.window(projectOf(f.base), dataset, table)
	if !ok {
		return false
	}
	n, why := f.scalar(r, "SELECT COUNT(*) FROM "+quotePath([]string{projectOf(f.base), dataset, table})+
		" WHERE "+win.cond())
	if why != "" || n == 0 {
		return false
	}
	meta["streamingBuffer"] = map[string]any{
		"estimatedRows":   strconv.FormatInt(n, 10),
		"oldestEntryTime": strconv.FormatInt(win.Oldest.UnixMilli(), 10),
	}
	if s, ok := meta["numRows"].(string); ok {
		if total, err := strconv.ParseInt(s, 10, 64); err == nil && total >= n {
			meta["numRows"] = strconv.FormatInt(total-n, 10)
		}
	}
	return true
}

// bufferedMark is the SQL value of _PARTITION_BUFFERED for a row streamed
// now into the table m, partitioned by a column, read from the row's
// columns: FALSE when its partition is a day's from 7 days ago to 3 days
// ahead, which keeps it in its own partition's buffer; TRUE, in
// __UNPARTITIONED__, otherwise (above).
func bufferedMark(m partitionMeta, now time.Time) string {
	tp := m.TimePartitioning
	if tp == nil || tp.Field == "" || tp.Type != "" && !strings.EqualFold(tp.Type, "DAY") {
		return "TRUE"
	}
	expr := quoteName(tp.Field)
	for _, fl := range m.Schema.Fields {
		if strings.EqualFold(fl.Name, tp.Field) && !strings.EqualFold(fl.Type, "TIMESTAMP") {
			expr = "TIMESTAMP(" + expr + ")"
		}
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return "IF(" + expr + " >= TIMESTAMP '" + today.AddDate(0, 0, -7).Format(sqlTimestamp) + "' AND " + expr +
		" < TIMESTAMP '" + today.AddDate(0, 0, 4).Format(sqlTimestamp) + "', FALSE, TRUE)"
}

// streamBounds checks the value v of the partitioning column of a row
// streamed now into a table partitioned by a DATE, DATETIME or TIMESTAMP
// column: "between 10 years in the past and 1 year in the future. Data
// outside this range is rejected" (above). It returns the row error's
// message, or "" when the value is in bounds or is not one it reads.
func streamBounds(m partitionMeta, v any, now time.Time) string {
	tp := m.TimePartitioning
	if tp == nil || tp.Field == "" {
		return ""
	}
	var t time.Time
	switch x := v.(type) {
	case string:
		ok := false
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999",
			"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999 MST", "2006-01-02"} {
			if p, err := time.Parse(layout, x); err == nil {
				t, ok = p, true
				break
			}
		}
		if !ok {
			return ""
		}
	case json.Number:
		sec, err := x.Float64()
		if err != nil {
			return ""
		}
		t = time.Unix(0, int64(sec*1e9))
	default:
		return ""
	}
	if t.Before(now.AddDate(-10, 0, 0)) || t.After(now.AddDate(1, 0, 0)) {
		return "Value " + fmt.Sprint(v) + " for field " + tp.Field + " is outside the allowed bounds: you can stream " +
			"data between 10 years in the past and 1 year in the future." // UNVERIFIED wording
	}
	return ""
}

// plainTable is whether got, a table's tables.get answer, is a table (not
// a view or an external table).
func plainTable(got []byte) bool {
	var t struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(got, &t) == nil && (t.Type == "" || t.Type == "TABLE")
}

// syncEngineClock sets the engine's clock offset to offset nanoseconds
// (serveClock), or says why it could not.
func syncEngineClock(r *http.Request, engine http.Handler, offset int64) string {
	body, err := json.Marshal(map[string]int64{"offsetNanos": offset})
	if err != nil {
		return err.Error()
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/cloudburrow/clock", bytes.NewReader(body))
	if err != nil {
		return err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Host = r.Host
	req.RemoteAddr = r.RemoteAddr
	rec := newRecorder()
	engine.ServeHTTP(rec, req)
	if rec.status != 0 && rec.status != http.StatusOK {
		return fmt.Sprintf("%d %s", rec.status, strings.TrimSpace(rec.body.String()))
	}
	return ""
}
