package bigqueryfront

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
}

// streamBuffers are the tables' streaming buffers.
type streamBuffers struct {
	mu   sync.Mutex
	wins map[string]*streamWindow
	now  func() time.Time
	path string
	logf func(string, ...any)
}

func streamBuffersStateFile(dir string) string { return filepath.Join(dir, "streambuffers.json") }

func streamKey(project, dataset, table string) string {
	return project + "\x00" + dataset + "\x00" + table
}

func (s *streamBuffers) clock() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
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
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		s.logf("bigquery front: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		s.logf("bigquery front: %v", err)
	}
}

func (s *streamBuffers) reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wins = nil
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
	if _, ok := s.wins[streamKey(project, dataset, table)]; ok {
		delete(s.wins, streamKey(project, dataset, table))
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
	target, code, msg := streamTarget(m, dec, name, now)
	if code != 0 {
		writeError(w, code, reasonOf(code, "invalid"), msg)
		return false
	}
	schema, err := json.Marshal(m.Schema)
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
	pcols, pvals := ", _PARTITIONTIME", ", CAST(NULL AS TIMESTAMP)"
	if m.ingestionUnit() == "DAY" {
		pcols, pvals = pcols+", _PARTITIONDATE", pvals+", CAST(NULL AS DATE)"
	}
	// A window for another partition flushes first (above).
	key := streamKey(project, dataset, base)
	f.buffers.mu.Lock()
	old, had := f.buffers.wins[key]
	var flush *streamWindow
	if had && !old.Target.Equal(target) {
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
	if why := f.runDML(r, "INSERT INTO "+quotePath([]string{project, dataset, base})+" ("+list+pcols+") SELECT "+list+
		pvals+" FROM "+quotePath([]string{project, dataset, scratch.TableID})); why != "" {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not stream the rows: "+why)
		return false
	}
	f.buffers.mu.Lock()
	if f.buffers.wins == nil {
		f.buffers.wins = map[string]*streamWindow{}
	}
	if cur, ok := f.buffers.wins[key]; ok && cur.Target.Equal(target) {
		cur.Rows += n
	} else {
		f.buffers.wins[key] = &streamWindow{Base: f.base, Dataset: dataset, Table: base, Unit: m.ingestionUnit(),
			Target: target, Oldest: now, Rows: n}
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
		" WHERE _PARTITIONTIME IS NULL")
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
