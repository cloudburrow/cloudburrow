package alloydb

import (
	"math"
	"strings"
	"testing"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"google.golang.org/protobuf/proto"
)

// csvlog lines as a PostgreSQL 16 server writes them (#2253): a simple
// query's plan and duration, an extended query's parse, bind, execute
// and plan, and an error.
const testCSVLog = `2026-11-26 00:20:16.049 UTC,"app","shop",67,"10.0.0.7:5544",6b077b40.43,3,"SELECT",2026-11-26 00:20:16 UTC,3/3,0,LOG,00000,"duration: 0.019 ms  plan:
{
  ""Query Text"": ""/*action='list',application='shop',route='%2Fitems'*/ select count(*) from t where id < 50"",
  ""Plan"": {
    ""Node Type"": ""Aggregate"",
    ""Actual Total Time"": 0.012
  }
}",,,,,,,,,"psql","client backend",,2898794813361217301
2026-11-26 00:20:16.049 UTC,"app","shop",67,"10.0.0.7:5544",6b077b40.43,4,"SELECT",2026-11-26 00:20:16 UTC,3/0,0,LOG,00000,"duration: 0.747 ms  statement: /*action='list',application='shop',route='%2Fitems'*/ select count(*) from t where id < 50",,,,,,,,,"psql","client backend",,2898794813361217301
2026-11-26 00:20:31.685 UTC,"app","shop",97,"10.0.0.8:62272",6b077b4f.61,1,"PARSE",2026-11-26 00:20:31 UTC,3/10,0,LOG,00000,"duration: 0.970 ms  parse stmtcache_1: /*db_driver='pgx'*/ SELECT count(*) FROM t WHERE id < $1",,,,,,,,,"","client backend",,3751096229874491004
2026-11-26 00:20:31.685 UTC,"app","shop",97,"10.0.0.8:62272",6b077b4f.61,2,"BIND",2026-11-26 00:20:31 UTC,3/11,0,LOG,00000,"duration: 0.564 ms  bind stmtcache_1: /*db_driver='pgx'*/ SELECT count(*) FROM t WHERE id < $1","parameters: $1 = '30'",,,,,,,,"","client backend",,3751096229874491004
2026-11-26 00:20:31.686 UTC,"app","shop",97,"10.0.0.8:62272",6b077b4f.61,3,"SELECT",2026-11-26 00:20:31 UTC,3/11,0,LOG,00000,"duration: 0.040 ms  execute stmtcache_1: /*db_driver='pgx'*/ SELECT count(*) FROM t WHERE id < $1","parameters: $1 = '30'",,,,,,,,"","client backend",,3751096229874491004
2026-11-26 00:20:31.686 UTC,"app","shop",97,"10.0.0.8:62272",6b077b4f.61,5,"SELECT",2026-11-26 00:20:31 UTC,3/12,0,ERROR,42P01,"relation ""nosuchtable"" does not exist",,,,,,"SELECT * FROM nosuchtable",15,,"","client backend",,0
2026-11-26 00:20:32.000 UTC,"alloydbadmin","postgres",98,"127.0.0.1:1",6b077b4f.62,1,"SELECT",2026-11-26 00:20:31 UTC,3/12,0,ERROR,42P01,"relation ""x"" does not exist",,,,,,"SELECT * FROM x",15,,"","client backend",,0
2026-11-26 00:20:33.000 UTC,"app","shop",99,"10.0.0.9:1",6b077b4f.63,1,"SELECT",2026-11-26 00:20:3`

func TestSplitCSVRecordsKeepsThePartialLast(t *testing.T) {
	recs, used := splitCSVRecords([]byte(testCSVLog))
	if len(recs) != 7 {
		t.Fatalf("records = %d, want 7", len(recs))
	}
	if !strings.HasPrefix(testCSVLog[used:], "2026-11-26 00:20:33.000") {
		t.Fatalf("the partial last record was consumed: rest %q", testCSVLog[used:])
	}
	if got := recs[0][csvMessage]; !strings.Contains(got, `"Node Type": "Aggregate"`) {
		t.Fatalf("multi-line message = %q", got)
	}
	if recs[0][csvQueryID] != "2898794813361217301" || recs[0][csvFrom] != "10.0.0.7:5544" {
		t.Fatalf("query_id, connection_from = %q, %q", recs[0][csvQueryID], recs[0][csvFrom])
	}
}

func testState(cfg *alloydbpb.Instance_QueryInsightsInstanceConfig) *telemetryState {
	return newTelemetryState(MetricInstance{Name: "projects/p/locations/us-central1/clusters/c/instances/i", Project: "p",
		Location: "us-central1", Cluster: "c", Instance: "i", Insights: cfg, Flags: map[string]string{}})
}

func defaultInsights() *alloydbpb.Instance_QueryInsightsInstanceConfig {
	return &alloydbpb.Instance_QueryInsightsInstanceConfig{RecordApplicationTags: proto.Bool(true), RecordClientAddress: proto.Bool(true),
		QueryStringLength: 1024, QueryPlansPerMinute: proto.Uint32(5)}
}

func TestObserveRecordLatenciesTagsPlansAndLines(t *testing.T) {
	st := testState(defaultInsights())
	recs, _ := splitCSVRecords([]byte(testCSVLog))
	var lines []LogLine
	for _, r := range recs {
		if l, ok := st.observeRecord(r, "alloydbadmin"); ok {
			lines = append(lines, l)
		}
	}
	// The duration and plan lines are the collector's (the instance has no
	// log_min_duration_statement flag), and the management role's error is
	// left out: only the user's error is a line.
	if len(lines) != 1 || lines[0].Severity != "ERROR" || !strings.Contains(lines[0].Text, `ERROR:  relation "nosuchtable" does not exist`) ||
		!strings.Contains(lines[0].Text, "STATEMENT:  SELECT * FROM nosuchtable") || !strings.Contains(lines[0].Text, "db=shop,user=app") {
		t.Fatalf("lines = %+v", lines)
	}
	if lines[0].InstanceID != "i" || lines[0].Cluster != "c" || lines[0].At.Format(time.RFC3339Nano) != "2026-11-26T00:20:31.686Z" {
		t.Fatalf("line's instance and time = %+v", lines[0])
	}
	// Two executions: the simple query's 0.747 ms, the extended one's bind
	// and execute, 0.604 ms (its parse is not an execution).
	agg := st.agg
	if len(agg) != 2 {
		t.Fatalf("aggregate keys = %v", agg)
	}
	h := agg[latKey{db: "shop", user: "app", client: "10.0.0.8"}]
	if h == nil || h.Count != 1 || math.Abs(h.Sum-604) > 0.01 {
		t.Fatalf("extended execution = %+v", h)
	}
	if q := st.perQ[latKey{db: "shop", user: "app", client: "10.0.0.7", query: 2898794813361217301}]; q == nil || q.Count != 1 || math.Abs(q.Sum-747) > 0.01 {
		t.Fatalf("per query = %+v", q)
	}
	tags := Tags{Action: "list", Application: "shop", Route: "/items"}
	if q := st.perT[latKey{db: "shop", user: "app", client: "10.0.0.7", tag: tags.Hash()}]; q == nil || q.Count != 1 {
		t.Fatalf("per tag = %+v (tags %v)", st.perT, st.tags)
	}
	if st.tags[tags.Hash()] != tags {
		t.Fatalf("tags = %v", st.tags)
	}
	if len(st.plans) != 1 || st.plans[0].QueryID != 2898794813361217301 || !strings.Contains(string(st.plans[0].Plan), "Aggregate") {
		t.Fatalf("plans = %+v", st.plans)
	}
}

func TestObserveRecordHonoursTheInstanceSettings(t *testing.T) {
	cfg := defaultInsights()
	cfg.RecordClientAddress, cfg.RecordApplicationTags, cfg.QueryPlansPerMinute = proto.Bool(false), proto.Bool(false), proto.Uint32(0)
	st := testState(cfg)
	st.instance.Flags["log_min_duration_statement"] = "0.5"
	recs, _ := splitCSVRecords([]byte(testCSVLog))
	var shipped []string
	for _, r := range recs {
		if l, ok := st.observeRecord(r, "alloydbadmin"); ok {
			shipped = append(shipped, l.Text)
		}
	}
	// log_min_duration_statement 0.5 ms: the 0.747 ms statement, the
	// 0.970 ms parse and the 0.564 ms bind are logged, the 0.040 ms
	// execute is not, nor the plan.
	if len(shipped) != 4 {
		t.Fatalf("shipped = %q", shipped)
	}
	if len(st.perT) != 0 || len(st.plans) != 0 {
		t.Fatalf("tags or plans recorded with them off: %v %v", st.perT, st.plans)
	}
	for k := range st.agg {
		if k.client != "" {
			t.Fatalf("a client address recorded with it off: %+v", k)
		}
	}
}

func TestPlansKeepToTheRateAMinute(t *testing.T) {
	cfg := defaultInsights()
	cfg.QueryPlansPerMinute = proto.Uint32(2)
	st := testState(cfg)
	at := time.Date(2026, 11, 26, 0, 0, 1, 0, time.UTC)
	for i := 0; i < 5; i++ {
		st.observePlan(at.Add(time.Duration(i)*time.Second), "d", "u", "", 1, 1, `{"Plan":{}}`)
	}
	st.observePlan(at.Add(time.Minute), "d", "u", "", 1, 1, `{"Plan":{}}`)
	if len(st.plans) != 3 {
		t.Fatalf("plans = %d, want 2 in the first minute and 1 in the next", len(st.plans))
	}
}

func TestObserveActivityWaitsAndLocks(t *testing.T) {
	st := testState(defaultInsights())
	t0 := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	rows := []activityRow{
		{PID: 1, Backend: "client backend", State: "active", Database: "shop", User: "app", Client: "10.0.0.7", QueryID: 42,
			WaitType: "Lock", WaitName: "transactionid", Query: "UPDATE t SET v = 1 /*application='shop'*/"},
		{PID: 2, Backend: "client backend", State: "idle", WaitType: "Client", WaitName: "ClientRead"},
		{PID: 3, Backend: "walwriter", WaitType: "Activity", WaitName: "WalWriterMain"},
		{PID: 4, Backend: "checkpointer", WaitType: "IO", WaitName: "DataFileSync"},
	}
	st.observeActivity(rows, t0)
	st.observeActivity(rows, t0.Add(time.Second))
	w := st.waits[[2]string{"Lock", "transactionid"}]
	if w == nil || w.Count != 1 || w.Us != 2e6 {
		t.Fatalf("lock wait = %+v (one wait, seen at two samples)", w)
	}
	if st.waits[[2]string{"Client", "ClientRead"}] != nil || st.waits[[2]string{"Activity", "WalWriterMain"}] != nil {
		t.Fatalf("idle waits counted: %v", st.waits)
	}
	if io := st.waits[[2]string{"IO", "DataFileSync"}]; io == nil || io.Count != 1 {
		t.Fatalf("a background process's IO wait = %+v", io)
	}
	tag := Tags{Application: "shop"}.Hash()
	if us := st.locks[lockKey{db: "shop", user: "app", client: "10.0.0.7", lock: "hw", query: 42, tag: tag}]; us != 2e6 {
		t.Fatalf("lock time = %d, locks %v", us, st.locks)
	}
	// The process moves to another wait: a second wait.
	rows[0].WaitType, rows[0].WaitName = "LWLock", "WALWrite"
	st.observeActivity(rows, t0.Add(2*time.Second))
	if st.waits[[2]string{"LWLock", "WALWrite"}].Count != 1 {
		t.Fatalf("waits = %v", st.waits)
	}
}

func TestParseTags(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Tags
		ok   bool
	}{
		{"SELECT 1 /*action='run%20it',controller='index',framework='spring',route='%2Fpolls%201000',application='app',db_driver='pgx'*/",
			Tags{Action: "run it", Controller: "index", Framework: "spring", Route: "/polls 1000", Application: "app", DBDriver: "pgx"}, true},
		{"/*traceparent='00-1-2-01'*/ SELECT 1", Tags{}, false},
		{"SELECT '/*' || 'x'", Tags{}, false},
		{"SELECT 1 /*route='a\\'b'*/", Tags{Route: "a'b"}, true},
	} {
		got, ok := ParseTags(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseTags(%q) = %+v %v, want %+v %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestPercentileWithinBuckets(t *testing.T) {
	var h Histogram
	for i := 0; i < 100; i++ {
		h.add(1000) // 1 ms
	}
	h.add(100000) // 100 ms
	p50 := Percentile(h.Buckets, 0.5)
	if p50 < 1000/LatencyBuckets.Growth || p50 > 1000*LatencyBuckets.Growth {
		t.Fatalf("P50 = %v us, want about 1000", p50)
	}
	if p := Percentile(h.Buckets, 1); p < 100000/LatencyBuckets.Growth || p > 100000*LatencyBuckets.Growth {
		t.Fatalf("P100 = %v us, want about 100000", p)
	}
	if Percentile(make([]int64, 66), 0.99) != 0 {
		t.Fatal("an empty distribution's percentile is not 0")
	}
}

func TestTelemetryArgsKeepThePreloadedLibraries(t *testing.T) {
	args := TelemetryArgs([]string{"postgres", "-c", "shared_preload_libraries=pg_stat_statements", "-c", "shared_preload_libraries=pg_cron,pgaudit"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c shared_preload_libraries=pg_cron,pgaudit,pg_stat_statements,auto_explain") {
		t.Fatalf("args = %v", args)
	}
	for _, want := range []string{"log_destination=csvlog", "logging_collector=on", "log_min_duration_statement=0", "compute_query_id=on"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args lack %s: %v", want, args)
		}
	}
	if strings.Contains(joined, "auto_explain.log_min_duration") || strings.Contains(joined, "auto_explain.sample_rate") {
		t.Fatalf("args set what the collector sets: %v", args)
	}
}

func TestClientHost(t *testing.T) {
	for in, want := range map[string]string{"10.0.0.7:5544": "10.0.0.7", "[local]": "", "::1:4000": "::1", "": ""} {
		if got := clientHost(in); got != want {
			t.Errorf("clientHost(%q) = %q, want %q", in, got, want)
		}
	}
}
