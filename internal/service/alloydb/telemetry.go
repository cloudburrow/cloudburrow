package alloydb

// Query insights' per-execution readings (#2253), from each instance's own
// PostgreSQL server, where #2251 had only pg_stat_statements' totals:
//
//   - the server's log, which every server writes as csvlog
//     (TelemetryArgs) and the collector reads back over SQL
//     (pg_ls_logdir, pg_read_binary_file): each line is written to Cloud
//     Logging as alloydb.googleapis.com/postgres.log on the instance's
//     alloydb.googleapis.com/Instance (System insights' Log errors card
//     counts them), and each statement's duration line
//     (log_min_duration_statement = 0), with its query_id, user, database
//     and client address, is one execution's latency: the latency
//     distributions per query, per application tag (sqlcommenter comments
//     in the statement's text, docs.cloud.google.com/alloydb/docs/
//     using-query-insights#adding-tags-to-sql-queries) and in aggregate;
//   - auto_explain, sampling plans with EXPLAIN ANALYZE at the rate that
//     keeps to the instance's query_plans_per_minute, the plans kept no
//     more than that many a minute;
//   - pg_stat_activity, sampled every second: the wait events of the
//     processes doing work (wait_count, wait_time), and each query's lock
//     waits (lock_time per query, per tag and in aggregate), which a 1 s
//     sample measures to within that second.
//
// The collector's own statements are not logged (its connections set
// log_min_duration_statement and auto_explain.log_min_duration to -1),
// and the lines of CloudBurrow's management role are left out, as
// Google's pages leave out AlloyDB's own.

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"github.com/jackc/pgx/v5"
)

// The server's log, as TelemetryArgs has every server write it: csvlog
// in $PGDATA/log, a file a minute (postgresql-MM.csv), each overwritten
// an hour later, so an hour of lines is kept on the volume.
const (
	telemetryLogDir  = "log"
	telemetryLogFile = "postgresql-%M.log"
	// telemetryReadMax bounds one file's read in one pass.
	telemetryReadMax = 8 << 20
	// telemetryChunk is one pg_read_binary_file call's length.
	telemetryChunk = 1 << 20
)

// The collector's cadence: pg_stat_activity every second, the log every
// five, and the plan sampling rate once a minute.
const (
	waitSampleEvery = time.Second
	logReadEvery    = 5
	planTuneEvery   = 60
)

// Bounds on what one instance keeps.
const (
	maxLatencyKeys = 5000
	maxPlanSamples = 200
)

// LatencyBuckets are the latency distributions' buckets, in microseconds:
// exponential, growth factor 1.4 from 1 us, 64 finite buckets (to about
// 36 minutes). UNVERIFIED: Google publishes no bucket options for the
// insights latencies; these are CloudBurrow's. Docs re-read 2026-11-25,
// still unanswered: https://docs.cloud.google.com/monitoring/api/metrics_gcp_a_b.
var LatencyBuckets = struct {
	Count         int
	Growth, Scale float64
}{64, 1.4, 1}

// TelemetryArgs are the postgres arguments that have a server write the
// log the collector reads, appended to args (a server's command line,
// whose last setting of a name wins): its shared_preload_libraries keep
// what args preload and add auto_explain. auto_explain.log_min_duration
// and sample_rate are left to the collector, which sets them with ALTER
// SYSTEM (an argument would override that).
func TelemetryArgs(args []string) []string {
	preload := []string{"pg_stat_statements"}
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-c" {
			continue
		}
		if v, ok := strings.CutPrefix(args[i+1], "shared_preload_libraries="); ok {
			preload = nil
			for _, l := range strings.Split(strings.Trim(v, `'"`), ",") {
				if l = strings.TrimSpace(l); l != "" {
					preload = append(preload, l)
				}
			}
		}
	}
	for _, need := range []string{"pg_stat_statements", "auto_explain"} {
		have := false
		for _, l := range preload {
			have = have || l == need
		}
		if !have {
			preload = append(preload, need)
		}
	}
	out := append([]string{}, args...)
	if len(out) == 0 {
		out = []string{"postgres"}
	}
	for _, kv := range []string{
		"shared_preload_libraries=" + strings.Join(preload, ","),
		"logging_collector=on", "log_destination=csvlog", "log_directory=" + telemetryLogDir,
		"log_filename=" + telemetryLogFile, "log_rotation_age=1min", "log_rotation_size=0", "log_truncate_on_rotation=on",
		"log_timezone=UTC", "log_min_duration_statement=0", "compute_query_id=on",
		"auto_explain.log_format=json", "auto_explain.log_analyze=on", "auto_explain.log_buffers=on",
		"auto_explain.log_timing=on", "auto_explain.log_nested_statements=off",
	} {
		out = append(out, "-c", kv)
	}
	return out
}

// Tags are a statement's application tags, the sqlcommenter keys Query
// insights keeps (others are dropped).
type Tags struct {
	Action, Application, Controller, DBDriver, Framework, Route string
}

// Hash is the tags' tag_hash: a hash of their string.
func (t Tags) Hash() string {
	h := fnv.New64a()
	h.Write([]byte(t.String()))
	return fmt.Sprintf("%016x", h.Sum64())
}

// String is the tags as sqlcommenter writes them, sorted by key.
func (t Tags) String() string {
	var parts []string
	for _, kv := range [][2]string{{"action", t.Action}, {"application", t.Application}, {"controller", t.Controller},
		{"db_driver", t.DBDriver}, {"framework", t.Framework}, {"route", t.Route}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"='"+url.QueryEscape(kv[1])+"'")
		}
	}
	return strings.Join(parts, ",")
}

func (t Tags) empty() bool { return t == Tags{} }

var commentRE = regexp.MustCompile(`(?s)/\*(.*?)\*/`)
var commentPairRE = regexp.MustCompile(`([A-Za-z0-9_%.\-]+)\s*=\s*'((?:[^'\\]|\\.)*)'`)

// ParseTags reads the sqlcommenter tags in a statement's comments
// (google.github.io/sqlcommenter/spec: key='value' pairs, separated by
// commas, keys and values URL-encoded): action, application, controller,
// db_driver (or "db driver"), framework and route. ok is whether any was.
func ParseTags(statement string) (Tags, bool) {
	var t Tags
	for _, m := range commentRE.FindAllStringSubmatch(statement, -1) {
		for _, p := range commentPairRE.FindAllStringSubmatch(m[1], -1) {
			k, err := url.QueryUnescape(p[1])
			if err != nil {
				k = p[1]
			}
			raw := strings.ReplaceAll(p[2], `\'`, `'`)
			v, err := url.QueryUnescape(raw)
			if err != nil {
				v = raw
			}
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "action":
				t.Action = v
			case "application":
				t.Application = v
			case "controller":
				t.Controller = v
			case "db_driver", "db driver":
				t.DBDriver = v
			case "framework":
				t.Framework = v
			case "route":
				t.Route = v
			}
		}
	}
	return t, !t.empty()
}

// Histogram is a cumulative latency distribution over LatencyBuckets, in
// microseconds: Buckets has LatencyBuckets.Count+2 counts (underflow,
// the finite buckets, overflow).
type Histogram struct {
	Count      int64
	Sum, SumSq float64
	Buckets    []int64
}

func (h *Histogram) add(us float64) {
	if h.Buckets == nil {
		h.Buckets = make([]int64, LatencyBuckets.Count+2)
	}
	h.Count++
	h.Sum += us
	h.SumSq += us * us
	h.Buckets[latencyBucket(us)]++
}

// latencyBucket is us's bucket index: 0 below Scale, i for
// [Scale*G^(i-1), Scale*G^i), Count+1 beyond.
func latencyBucket(us float64) int {
	if us < LatencyBuckets.Scale {
		return 0
	}
	i := int(math.Floor(math.Log(us/LatencyBuckets.Scale)/math.Log(LatencyBuckets.Growth))) + 1
	if i > LatencyBuckets.Count+1 {
		i = LatencyBuckets.Count + 1
	}
	return i
}

func (h Histogram) clone() Histogram {
	h.Buckets = append([]int64(nil), h.Buckets...)
	return h
}

// LatencyBound is bucket i's upper bound in microseconds (+Inf for the
// overflow).
func LatencyBound(i int) float64 {
	if i > LatencyBuckets.Count {
		return math.Inf(1)
	}
	return LatencyBuckets.Scale * math.Pow(LatencyBuckets.Growth, float64(i))
}

// Percentile is q's latency in microseconds, interpolated within its
// bucket, from bucket counts over LatencyBuckets; 0 for none.
func Percentile(counts []int64, q float64) float64 {
	var total int64
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	rank := q * float64(total)
	var seen float64
	for i, c := range counts {
		if c == 0 {
			continue
		}
		if seen+float64(c) >= rank {
			lo := 0.0
			if i > 0 {
				lo = LatencyBound(i - 1)
			}
			hi := LatencyBound(i)
			if math.IsInf(hi, 1) {
				return lo
			}
			return lo + (hi-lo)*(rank-seen)/float64(c)
		}
		seen += float64(c)
	}
	return LatencyBound(len(counts) - 2)
}

// LatencyStat is one key's latency distribution since the collector
// started reading the server: per (database, user, client address), and
// per query (QueryID) or per tag (TagHash) when set.
type LatencyStat struct {
	Database, User, Client string
	QueryID                int64
	TagHash                string
	Hist                   Histogram
}

// LockStat is the time, in microseconds, one key's backends were seen
// waiting on a lock of one type (lw, hw, buffer_pin), by query and tag.
type LockStat struct {
	Database, User, Client, LockType string
	QueryID                          int64
	TagHash                          string
	Us                               int64
}

// WaitEventStat is one wait event's waits seen (a process found waiting
// on it at a sample where it was not at the last) and their time.
type WaitEventStat struct {
	Type, Name string
	Count      int64
	Us         float64
}

// PlanSample is one execution's plan, sampled by auto_explain.
type PlanSample struct {
	At                     time.Time
	Database, User, Client string
	QueryID                int64
	DurationMs             float64
	// Plan is EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)'s output.
	Plan json.RawMessage
}

// LogLine is one line of a server's log, for Cloud Logging.
type LogLine struct {
	// Instance is the instance's full name; Project, Location, Cluster and
	// InstanceID its parts.
	Instance, Project, Location, Cluster, InstanceID string
	At                                               time.Time
	// Severity is Cloud Logging's (DEBUG, INFO, NOTICE, WARNING, ERROR,
	// ALERT).
	Severity string
	// Text is the line as PostgreSQL's stderr log would print it, with
	// Cloud SQL's log_line_prefix, and its DETAIL, HINT, CONTEXT and
	// STATEMENT lines after it.
	Text string
	// Fields are the csvlog's columns, by name.
	Fields map[string]string
}

// Telemetry is one instance's readings since the collector started
// reading its server: cumulative, so a reader takes differences.
type Telemetry struct {
	// Reading is whether the collector reads the server.
	Reading bool
	// LogsRead is whether its log was read (its server writes csvlog).
	LogsRead                    bool
	Aggregate, PerQuery, PerTag []LatencyStat
	Locks                       []LockStat
	Waits                       []WaitEventStat
	// TagValues are the tags by hash; Queries the normalized queries by
	// query_id, as pg_stat_statements names them.
	TagValues map[string]Tags
	Queries   map[int64]string
	// QueryStringLength is the instance's query_string_length.
	QueryStringLength int
}

// telemetryState is the collector's state for one server.
type telemetryState struct {
	mu       sync.Mutex
	instance MetricInstance
	conn     *pgx.Conn
	busy     bool
	ticks    int
	// files are the log files read, by name.
	files map[string]*logFile
	// primed is whether the first listing set the offsets (the lines
	// written before the collector started are not read).
	primed  bool
	pending map[string]float64 // a session's bind time, before its execute
	agg     map[latKey]*Histogram
	perQ    map[latKey]*Histogram
	perT    map[latKey]*Histogram
	tags    map[string]Tags
	queries map[int64]string
	locks   map[lockKey]int64
	waits   map[[2]string]*WaitEventStat
	// lastWait is each process's wait event at the last sample.
	lastWait  map[int32][2]string
	lastAt    time.Time
	execs     int64 // executions logged since the last plan tuning
	planMin   time.Time
	planCount int
	plans     []PlanSample
	rate      float64 // auto_explain.sample_rate last set; -1 for off
	logsRead  bool
}

type latKey struct {
	db, user, client string
	query            int64
	tag              string
}

type lockKey struct {
	db, user, client, lock string
	query                  int64
	tag                    string
}

type logFile struct {
	offset int64
	head   []byte
}

func newTelemetryState(mi MetricInstance) *telemetryState {
	return &telemetryState{instance: mi, files: map[string]*logFile{}, pending: map[string]float64{},
		agg: map[latKey]*Histogram{}, perQ: map[latKey]*Histogram{}, perT: map[latKey]*Histogram{}, tags: map[string]Tags{},
		queries: map[int64]string{}, locks: map[lockKey]int64{}, waits: map[[2]string]*WaitEventStat{},
		lastWait: map[int32][2]string{}, rate: -2}
}

// SetLogSink has the collector hand every server log line to sink.
func (s *Server) SetLogSink(sink func(LogLine)) {
	s.telMu.Lock()
	defer s.telMu.Unlock()
	s.logSink = sink
}

// RunTelemetry reads every serving instance's server until stop closes.
func (s *Server) RunTelemetry(stop <-chan struct{}) {
	t := time.NewTicker(waitSampleEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			s.telMu.Lock()
			for _, st := range s.tel {
				st.close()
			}
			s.telMu.Unlock()
			return
		case <-t.C:
			s.telemetryTick()
		}
	}
}

func (st *telemetryState) close() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.conn != nil {
		_ = st.conn.Close(context.Background())
		st.conn = nil
	}
}

func (s *Server) telemetryTick() {
	if s.opts.Engine == nil {
		return
	}
	seen := map[string]bool{}
	for _, mi := range s.MetricInstances("") {
		if !mi.Serving {
			continue
		}
		seen[mi.Name] = true
		s.telMu.Lock()
		if s.tel == nil {
			s.tel = map[string]*telemetryState{}
		}
		st := s.tel[mi.Name]
		if st == nil || st.instance.Server != mi.Server {
			if st != nil {
				st.close()
			}
			st = newTelemetryState(mi)
			s.tel[mi.Name] = st
		}
		s.telMu.Unlock()
		st.mu.Lock()
		if st.busy {
			st.mu.Unlock()
			continue
		}
		st.busy = true
		st.instance = mi
		st.mu.Unlock()
		go func(st *telemetryState) {
			s.collect(st)
			st.mu.Lock()
			st.busy = false
			st.mu.Unlock()
		}(st)
	}
	s.telMu.Lock()
	for k, st := range s.tel {
		if !seen[k] {
			st.close()
			delete(s.tel, k)
		}
	}
	s.telMu.Unlock()
}

// telemetryConn connects as the management role with the collector's own
// statements left out of the server's log.
func (s *Server) telemetryConn(ctx context.Context, server string) (*pgx.Conn, error) {
	if s.opts.Engine == nil {
		return nil, fmt.Errorf("no engine")
	}
	addr := s.opts.Engine.Addr(server)
	if addr == "" {
		return nil, fmt.Errorf("the server does not answer")
	}
	cfg, err := pgx.ParseConfig("postgres://" + addr + "/postgres?sslmode=disable&connect_timeout=5")
	if err != nil {
		return nil, err
	}
	cfg.User, cfg.Password = builtinUser, ""
	if s.opts.AdminUser != "" && s.opts.AdminPassword != "" {
		cfg.User, cfg.Password = s.opts.AdminUser, s.opts.AdminPassword
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["application_name"] = insightsMarker
	cfg.RuntimeParams["log_min_duration_statement"] = "-1"
	cfg.RuntimeParams["log_statement"] = "none"
	cfg.RuntimeParams["auto_explain.log_min_duration"] = "-1"
	return pgx.ConnectConfig(ctx, cfg)
}

// collect is one tick for one server.
func (s *Server) collect(st *telemetryState) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	st.mu.Lock()
	conn := st.conn
	mi := st.instance
	st.ticks++
	tick := st.ticks
	st.mu.Unlock()
	if conn == nil || conn.IsClosed() {
		c, err := s.telemetryConn(ctx, mi.Server)
		if err != nil {
			return
		}
		conn = c
		st.mu.Lock()
		st.conn = c
		st.mu.Unlock()
	}
	fail := func(err error) {
		if err != nil && (conn.IsClosed() || ctx.Err() != nil) {
			st.close()
		}
	}
	fail(s.sampleActivity(ctx, conn, st))
	if tick%logReadEvery == 1 || logReadEvery == 1 {
		fail(s.readLog(ctx, conn, st))
	}
	if tick%planTuneEvery == 2 {
		s.tunePlans(ctx, conn, st)
		s.readQueryTexts(ctx, conn, st)
	}
}

// ---- pg_stat_activity ----

// activityRow is one process at a sample.
type activityRow struct {
	PID                    int32
	Backend, State         string
	Database, User, Client string
	QueryID                int64
	WaitType, WaitName     string
	Query                  string
}

func (s *Server) sampleActivity(ctx context.Context, c *pgx.Conn, st *telemetryState) error {
	admin := s.opts.AdminUser
	rows, err := c.Query(ctx, `SELECT /* `+insightsMarker+` */ pid, COALESCE(backend_type, ''), COALESCE(state, ''),
		COALESCE(datname, ''), COALESCE(usename, ''), COALESCE(host(client_addr), ''), COALESCE(query_id, 0),
		COALESCE(wait_event_type, ''), COALESCE(wait_event, ''), COALESCE(query, '')
		FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND COALESCE(usename, '') <> $1`, admin)
	if err != nil {
		return err
	}
	var out []activityRow
	for rows.Next() {
		var r activityRow
		if rows.Scan(&r.PID, &r.Backend, &r.State, &r.Database, &r.User, &r.Client, &r.QueryID, &r.WaitType, &r.WaitName, &r.Query) == nil {
			out = append(out, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.observeActivity(out, s.now())
	return nil
}

// lockTypeOf is Query insights' lock_type of a wait event type: lw
// (LWLock), hw (Lock, heavyweight) or buffer_pin, "" for no lock.
func lockTypeOf(waitType string) string {
	switch waitType {
	case "LWLock":
		return "lw"
	case "Lock":
		return "hw"
	case "BufferPin":
		return "buffer_pin"
	}
	return ""
}

// observeActivity adds one sample: the time since the last one, to each
// waiting process's wait event and to each lock wait. A client backend
// counts while it is active; another process unless it waits in its main
// loop (wait_event_type Activity). UNVERIFIED: which processes Google's
// wait_count and wait_time count; idle sessions, all waiting on
// ClientRead, would swamp them (docs re-read 2026-11-25, still
// unanswered: "processes waited", https://docs.cloud.google.com/monitoring/api/metrics_gcp_a_b). The caller holds st.mu.
func (st *telemetryState) observeActivity(rows []activityRow, now time.Time) {
	elapsed := waitSampleEvery
	if !st.lastAt.IsZero() {
		elapsed = now.Sub(st.lastAt)
		if elapsed <= 0 || elapsed > 10*waitSampleEvery {
			// A gap (a server restart, a stalled read): charge a sample's
			// worth, not the gap.
			elapsed = waitSampleEvery
		}
	}
	st.lastAt = now
	us := elapsed.Microseconds()
	cur := map[int32][2]string{}
	q := st.instance.Insights
	for _, r := range rows {
		client := r.Backend == "client backend"
		if client && r.State != "active" || r.WaitType == "" || !client && r.WaitType == "Activity" {
			continue
		}
		ev := [2]string{r.WaitType, r.WaitName}
		cur[r.PID] = ev
		w := st.waits[ev]
		if w == nil {
			w = &WaitEventStat{Type: ev[0], Name: ev[1]}
			st.waits[ev] = w
		}
		if st.lastWait[r.PID] != ev {
			w.Count++
		}
		w.Us += float64(us)
		lt := lockTypeOf(r.WaitType)
		if !client || lt == "" {
			continue
		}
		addr := r.Client
		if !q.GetRecordClientAddress() {
			addr = ""
		}
		tag := ""
		if q.GetRecordApplicationTags() {
			if t, ok := ParseTags(r.Query); ok {
				tag = t.Hash()
				st.tags[tag] = t
			}
		}
		k := lockKey{db: r.Database, user: r.User, client: addr, lock: lt, query: r.QueryID, tag: tag}
		if _, ok := st.locks[k]; ok || len(st.locks) < maxLatencyKeys {
			st.locks[k] += us
		}
	}
	st.lastWait = cur
}

// ---- the server's log ----

// readLog reads what the server's log files gained since the last read.
func (s *Server) readLog(ctx context.Context, c *pgx.Conn, st *telemetryState) error {
	rows, err := c.Query(ctx, `SELECT /* `+insightsMarker+` */ name, size FROM pg_ls_logdir() WHERE name LIKE '%.csv' ORDER BY modification, name`)
	if err != nil {
		return err
	}
	type entry struct {
		name string
		size int64
	}
	var files []entry
	for rows.Next() {
		var e entry
		if rows.Scan(&e.name, &e.size) == nil {
			files = append(files, e)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	st.mu.Lock()
	primed := st.primed
	st.primed = true
	st.mu.Unlock()
	for _, f := range files {
		head, err := readChunk(ctx, c, f.name, 0, 64)
		if err != nil {
			continue
		}
		st.mu.Lock()
		lf := st.files[f.name]
		if lf == nil {
			lf = &logFile{}
			if !primed {
				// Lines written before the collector started are not read.
				lf.offset = f.size
			}
			st.files[f.name] = lf
		}
		if f.size < lf.offset || len(lf.head) > 0 && !bytes.HasPrefix(head, lf.head) && !bytes.HasPrefix(lf.head, head) {
			// The file was overwritten (log_truncate_on_rotation): read it
			// from its start.
			lf.offset = 0
		}
		lf.head = head
		offset := lf.offset
		st.mu.Unlock()
		if f.size <= offset {
			continue
		}
		var buf []byte
		for read := int64(0); offset+read < f.size && read < telemetryReadMax; {
			n := min(int64(telemetryChunk), f.size-offset-read)
			b, err := readChunk(ctx, c, f.name, offset+read, n)
			if err != nil || len(b) == 0 {
				break
			}
			buf = append(buf, b...)
			read += int64(len(b))
		}
		recs, used := splitCSVRecords(buf)
		st.mu.Lock()
		lf.offset = offset + int64(used)
		st.logsRead = true
		var lines []LogLine
		for _, rec := range recs {
			if l, ok := st.observeRecord(rec, s.opts.AdminUser); ok {
				lines = append(lines, l)
			}
		}
		st.mu.Unlock()
		s.telMu.Lock()
		sink := s.logSink
		s.telMu.Unlock()
		if sink != nil {
			for _, l := range lines {
				sink(l)
			}
		}
	}
	st.mu.Lock()
	// Forget the files the server removed.
	for name := range st.files {
		found := false
		for _, f := range files {
			found = found || f.name == name
		}
		if !found {
			delete(st.files, name)
		}
	}
	st.mu.Unlock()
	return nil
}

func readChunk(ctx context.Context, c *pgx.Conn, name string, offset, n int64) ([]byte, error) {
	var b []byte
	err := c.QueryRow(ctx, `SELECT /* `+insightsMarker+` */ pg_read_binary_file($1, $2, $3, true)`,
		telemetryLogDir+"/"+name, offset, n).Scan(&b)
	return b, err
}

// splitCSVRecords splits buf into its complete csvlog records (a record
// ends at a newline outside quotes), and says how many bytes they took.
func splitCSVRecords(buf []byte) ([][]string, int) {
	var out [][]string
	start, quoted := 0, false
	for i, b := range buf {
		switch b {
		case '"':
			quoted = !quoted
		case '\n':
			if quoted {
				continue
			}
			r := csv.NewReader(bytes.NewReader(buf[start : i+1]))
			r.FieldsPerRecord = -1
			r.LazyQuotes = true
			if rec, err := r.Read(); err == nil {
				out = append(out, rec)
			}
			start = i + 1
		}
	}
	return out, start
}

// The csvlog columns (PostgreSQL 14 to 17).
const (
	csvTime = iota
	csvUser
	csvDatabase
	csvPID
	csvFrom
	csvSession
	csvLine
	csvTag
	csvSessionStart
	csvVXID
	csvXID
	csvSeverity
	csvState
	csvMessage
	csvDetail
	csvHint
	csvInternalQuery
	csvInternalPos
	csvContext
	csvQuery
	csvQueryPos
	csvLocation
	csvApplication
	csvBackend
	csvLeader
	csvQueryID
	csvColumns
)

var csvNames = []string{"log_time", "user_name", "database_name", "process_id", "connection_from", "session_id", "session_line_num",
	"command_tag", "session_start_time", "virtual_transaction_id", "transaction_id", "error_severity", "sql_state_code", "message",
	"detail", "hint", "internal_query", "internal_query_pos", "context", "query", "query_pos", "location", "application_name",
	"backend_type", "leader_pid", "query_id"}

var durationRE = regexp.MustCompile(`(?s)^duration: ([0-9.]+) ms  (statement|execute [^:]*|bind [^:]*|parse [^:]*|plan):\s?(.*)$`)

// severityOf is Cloud Logging's severity of a PostgreSQL message level.
// Google lists INFO, NOTICE, WARNING, ERROR and ALERT for postgres.log
// (docs.cloud.google.com/logging/docs/api/platform-logs, re-read 2026-11-25); UNVERIFIED:
// FATAL and PANIC are ALERT here, DEBUG levels DEBUG (still unanswered).
func severityOf(level string) string {
	switch {
	case strings.HasPrefix(level, "DEBUG"):
		return "DEBUG"
	case level == "LOG" || level == "INFO":
		return "INFO"
	case level == "NOTICE", level == "WARNING", level == "ERROR":
		return level
	case level == "FATAL" || level == "PANIC":
		return "ALERT"
	}
	return "DEFAULT"
}

// observeRecord takes one csvlog record: an execution's latency, a plan
// sample, and the line Cloud Logging is given (ok false for none: the
// management role's lines, and the duration and plan lines the server
// writes for the collector, which the instance's own
// log_min_duration_statement flag would not have it write). The caller
// holds st.mu.
func (st *telemetryState) observeRecord(rec []string, admin string) (LogLine, bool) {
	if len(rec) < csvQueryID {
		return LogLine{}, false
	}
	for len(rec) < csvColumns {
		rec = append(rec, "")
	}
	if admin != "" && rec[csvUser] == admin || rec[csvApplication] == insightsMarker {
		return LogLine{}, false
	}
	at, err := time.Parse("2006-01-02 15:04:05.000 MST", rec[csvTime])
	if err != nil {
		at = time.Now()
	}
	at = at.UTC()
	queryID, _ := strconv.ParseInt(rec[csvQueryID], 10, 64)
	q := st.instance.Insights
	client := ""
	if q.GetRecordClientAddress() {
		client = clientHost(rec[csvFrom])
	}
	ship := true
	if m := durationRE.FindStringSubmatch(rec[csvMessage]); m != nil && rec[csvSeverity] == "LOG" {
		ms, _ := strconv.ParseFloat(m[1], 64)
		kind, _, _ := strings.Cut(m[2], " ")
		session := rec[csvSession]
		switch kind {
		case "bind":
			st.pending[session] += ms
		case "statement", "execute":
			total := ms + st.pending[session]
			delete(st.pending, session)
			st.observeExecution(rec[csvDatabase], rec[csvUser], client, queryID, m[3], total*1000)
		case "plan":
			st.observePlan(at, rec[csvDatabase], rec[csvUser], client, queryID, ms, m[3])
		}
		ship = kind != "plan" && userLogsDuration(st.instance.Flags, ms)
	}
	if !ship {
		return LogLine{}, false
	}
	return st.logLine(rec, at), true
}

// userLogsDuration is whether the instance's log_min_duration_statement
// flag logs a statement of ms (-1, the default, logs none).
func userLogsDuration(flags map[string]string, ms float64) bool {
	v, ok := flags["log_min_duration_statement"]
	if !ok {
		return false
	}
	n, err := strconv.ParseFloat(v, 64)
	return err == nil && n >= 0 && ms >= n
}

// clientHost is a connection_from's host ("" for the socket).
func clientHost(from string) string {
	if from == "" || from == "[local]" {
		return ""
	}
	if i := strings.LastIndex(from, ":"); i > 0 {
		return strings.Trim(from[:i], "[]")
	}
	return from
}

func (st *telemetryState) logLine(rec []string, at time.Time) LogLine {
	mi := st.instance
	text := fmt.Sprintf("%s [%s]: [%s-1] db=%s,user=%s %s:  %s", rec[csvTime], rec[csvPID], rec[csvLine], rec[csvDatabase], rec[csvUser],
		rec[csvSeverity], rec[csvMessage])
	for _, extra := range [][2]string{{"DETAIL", rec[csvDetail]}, {"HINT", rec[csvHint]}, {"CONTEXT", rec[csvContext]}, {"STATEMENT", rec[csvQuery]}} {
		if extra[1] != "" {
			text += "\n" + extra[0] + ":  " + extra[1]
		}
	}
	fields := map[string]string{}
	for i, name := range csvNames {
		if i < len(rec) && rec[i] != "" {
			fields[name] = rec[i]
		}
	}
	return LogLine{Instance: mi.Name, Project: mi.Project, Location: mi.Location, Cluster: mi.Cluster, InstanceID: mi.Instance,
		At: at, Severity: severityOf(rec[csvSeverity]), Text: text, Fields: fields}
}

// observeExecution adds one execution's latency, in microseconds. The
// caller holds st.mu.
func (st *telemetryState) observeExecution(db, user, client string, queryID int64, statement string, us float64) {
	st.execs++
	add := func(m map[latKey]*Histogram, k latKey) {
		h := m[k]
		if h == nil {
			if len(m) >= maxLatencyKeys {
				return
			}
			h = &Histogram{}
			m[k] = h
		}
		h.add(us)
	}
	add(st.agg, latKey{db: db, user: user, client: client})
	if queryID != 0 {
		add(st.perQ, latKey{db: db, user: user, client: client, query: queryID})
	}
	if st.instance.Insights.GetRecordApplicationTags() {
		if t, ok := ParseTags(statement); ok {
			h := t.Hash()
			st.tags[h] = t
			add(st.perT, latKey{db: db, user: user, client: client, tag: h})
		}
	}
}

// observePlan keeps a sampled plan, no more than query_plans_per_minute a
// minute. The caller holds st.mu.
func (st *telemetryState) observePlan(at time.Time, db, user, client string, queryID int64, ms float64, body string) {
	limit := planLimit(st.instance.Insights)
	if limit == 0 {
		return
	}
	minute := at.Truncate(time.Minute)
	if !minute.Equal(st.planMin) {
		st.planMin, st.planCount = minute, 0
	}
	if st.planCount >= limit {
		return
	}
	var plan json.RawMessage
	if json.Unmarshal([]byte(body), &plan) != nil {
		return
	}
	st.planCount++
	st.plans = append(st.plans, PlanSample{At: at, Database: db, User: user, Client: client, QueryID: queryID, DurationMs: ms, Plan: plan})
	if len(st.plans) > maxPlanSamples {
		st.plans = st.plans[len(st.plans)-maxPlanSamples:]
	}
}

// planLimit is the instance's query_plans_per_minute (5 by default).
func planLimit(q *alloydbpb.Instance_QueryInsightsInstanceConfig) int {
	if q == nil || q.QueryPlansPerMinute == nil {
		return 5
	}
	return int(q.GetQueryPlansPerMinute())
}

// tunePlans sets auto_explain's sampling: off without plan sampling, else
// the rate at which the last minute's executions would have given twice
// query_plans_per_minute plans (the cap keeps that many), at most 1.
func (s *Server) tunePlans(ctx context.Context, c *pgx.Conn, st *telemetryState) {
	st.mu.Lock()
	limit := planLimit(st.instance.Insights)
	execs := st.execs
	st.execs = 0
	prev := st.rate
	st.mu.Unlock()
	rate := -1.0
	if limit > 0 {
		rate = 1
		if execs > 0 {
			rate = math.Min(1, math.Max(0.0001, float64(2*limit)/float64(execs)))
		}
	}
	if prev != -2 && (rate < 0) == (prev < 0) && (rate < 0 || math.Abs(rate-prev) <= prev*0.1) {
		return
	}
	var err error
	if rate < 0 {
		_, err = c.Exec(ctx, "ALTER SYSTEM SET auto_explain.log_min_duration = -1")
	} else {
		if _, err = c.Exec(ctx, "ALTER SYSTEM SET auto_explain.log_min_duration = 0"); err == nil {
			_, err = c.Exec(ctx, fmt.Sprintf("ALTER SYSTEM SET auto_explain.sample_rate = %g", rate))
		}
	}
	if err == nil {
		_, err = c.Exec(ctx, "SELECT /* "+insightsMarker+" */ pg_reload_conf()")
	}
	if err != nil {
		s.logf("AlloyDB: sampling the query plans of %s: %v", st.instance.Name, err)
		return
	}
	st.mu.Lock()
	st.rate = rate
	st.mu.Unlock()
}

// readQueryTexts keeps the normalized queries' texts by query_id.
func (s *Server) readQueryTexts(ctx context.Context, c *pgx.Conn, st *telemetryState) {
	rows, err := c.Query(ctx, `SELECT /* `+insightsMarker+` */ queryid, query FROM pg_stat_statements WHERE queryid IS NOT NULL`)
	if err != nil {
		return
	}
	defer rows.Close()
	m := map[int64]string{}
	for rows.Next() {
		var id int64
		var q string
		if rows.Scan(&id, &q) == nil && !strings.Contains(q, insightsMarker) {
			m[id] = q
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, q := range m {
		if _, ok := st.queries[id]; ok || len(st.queries) < maxLatencyKeys {
			st.queries[id] = q
		}
	}
}

// ---- reads ----

func (s *Server) telemetryOf(name string) *telemetryState {
	s.telMu.Lock()
	defer s.telMu.Unlock()
	return s.tel[name]
}

// TelemetryOf is instance name's readings so far.
func (s *Server) TelemetryOf(name string) Telemetry {
	st := s.telemetryOf(name)
	if st == nil {
		return Telemetry{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	out := Telemetry{Reading: true, LogsRead: st.logsRead, TagValues: map[string]Tags{}, Queries: map[int64]string{},
		QueryStringLength: int(st.instance.Insights.GetQueryStringLength())}
	if out.QueryStringLength == 0 {
		out.QueryStringLength = 1024
	}
	conv := func(m map[latKey]*Histogram) []LatencyStat {
		var o []LatencyStat
		for k, h := range m {
			o = append(o, LatencyStat{Database: k.db, User: k.user, Client: k.client, QueryID: k.query, TagHash: k.tag, Hist: h.clone()})
		}
		sort.Slice(o, func(i, j int) bool {
			a, b := o[i], o[j]
			return fmt.Sprint(a.Database, a.User, a.Client, a.QueryID, a.TagHash) < fmt.Sprint(b.Database, b.User, b.Client, b.QueryID, b.TagHash)
		})
		return o
	}
	out.Aggregate, out.PerQuery, out.PerTag = conv(st.agg), conv(st.perQ), conv(st.perT)
	for k, us := range st.locks {
		out.Locks = append(out.Locks, LockStat{Database: k.db, User: k.user, Client: k.client, LockType: k.lock, QueryID: k.query, TagHash: k.tag, Us: us})
	}
	for _, w := range st.waits {
		out.Waits = append(out.Waits, *w)
	}
	sort.Slice(out.Waits, func(i, j int) bool {
		return out.Waits[i].Type+"/"+out.Waits[i].Name < out.Waits[j].Type+"/"+out.Waits[j].Name
	})
	for k, v := range st.tags {
		out.TagValues[k] = v
	}
	for k, v := range st.queries {
		out.Queries[k] = v
	}
	return out
}

// QueryPlans are instance name's sampled plans of query queryID (every
// query's when 0), newest first.
func (s *Server) QueryPlans(name string, queryID int64) []PlanSample {
	st := s.telemetryOf(name)
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []PlanSample
	for i := len(st.plans) - 1; i >= 0; i-- {
		if p := st.plans[i]; queryID == 0 || p.QueryID == queryID {
			out = append(out, p)
		}
	}
	return out
}

// QueryHash is a query_id as the perquery metrics' query_hash label.
func QueryHash(queryID int64) string { return strconv.FormatUint(uint64(queryID), 10) }
