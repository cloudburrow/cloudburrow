package monitoring

// ---- Cloud SQL: cloudsql.googleapis.com/database/* (#2141) ----
//
// Google's Cloud SQL metrics (https://docs.cloud.google.com/monitoring/api/metrics_gcp_c,
// the cloudsql section, read 2026-11-22), "Sampled every 60 seconds", on
// the cloudsql_database resource (project_id, database_id
// "PROJECT:INSTANCE", region). They are read from an SQLSource: each
// instance's server, its pod's own counters as the kubelet reports them
// (CPU time, memory, network, its data volume) and the server's own
// figures (its connections, by database on PostgreSQL, and its statement
// and transaction counters). A GAUGE is the sample's reading; a DELTA the
// change since the last sample (none across a restart, whose counters
// start again from zero).

import (
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The Cloud SQL metric types served.
const (
	SQLCPUUtilization = "cloudsql.googleapis.com/database/cpu/utilization"
	SQLCPUUsageTime   = "cloudsql.googleapis.com/database/cpu/usage_time"
	SQLCPUReserved    = "cloudsql.googleapis.com/database/cpu/reserved_cores"
	SQLMemoryUsage    = "cloudsql.googleapis.com/database/memory/usage"
	SQLMemoryTotal    = "cloudsql.googleapis.com/database/memory/total_usage"
	SQLMemoryQuota    = "cloudsql.googleapis.com/database/memory/quota"
	SQLMemoryUtil     = "cloudsql.googleapis.com/database/memory/utilization"
	SQLDiskBytesUsed  = "cloudsql.googleapis.com/database/disk/bytes_used"
	SQLDiskQuota      = "cloudsql.googleapis.com/database/disk/quota"
	SQLDiskUtil       = "cloudsql.googleapis.com/database/disk/utilization"
	SQLConnections    = "cloudsql.googleapis.com/database/network/connections"
	SQLPGBackends     = "cloudsql.googleapis.com/database/postgresql/num_backends"
	SQLReceivedBytes  = "cloudsql.googleapis.com/database/network/received_bytes_count"
	SQLSentBytes      = "cloudsql.googleapis.com/database/network/sent_bytes_count"
	SQLUp             = "cloudsql.googleapis.com/database/up"
	SQLUptime         = "cloudsql.googleapis.com/database/uptime"
	SQLMySQLQueries   = "cloudsql.googleapis.com/database/mysql/queries"
	SQLPGTransactions = "cloudsql.googleapis.com/database/postgresql/transaction_count"

	// Query insights' aggregate metrics (#2141), on the
	// cloudsql_instance_database resource, by user and client_addr:
	// execution_time and io_time (io_type read or write) are
	// pg_stat_statements' totals, since the server's statistics were
	// reset; lock_time (lock_type lw, hw or buffer_pin) is the time the
	// server's backends were seen waiting on LWLock, Lock or BufferPin in
	// pg_stat_activity at each sample, times the time since the last one:
	// an estimate, where Cloud SQL measures it. client_addr is "": the
	// statistics do not keep it.
	SQLPGInsightsExecTime = "cloudsql.googleapis.com/database/postgresql/insights/aggregate/execution_time"
	SQLPGInsightsIOTime   = "cloudsql.googleapis.com/database/postgresql/insights/aggregate/io_time"
	SQLPGInsightsLockTime = "cloudsql.googleapis.com/database/postgresql/insights/aggregate/lock_time"

	// Managed connection pooling (#2163). MySQL's thread pool, on
	// cloudsql_database: its threads by status (active, idle), the
	// connections waiting in its work queue, and the average time a
	// statement waited there over the sample, in milliseconds, all from
	// the pool's own figures (internal/mysqlpool). PostgreSQL's pooler,
	// on cloudsql_instance_database (each database): its client
	// connections by status (active, waiting), its server connections by
	// status (active, idle), its pools and the clients' average wait in
	// microseconds, from PgBouncer's SHOW POOLS and SHOW STATS; and the
	// sign-in attempts and the errors that disconnected a client, counted
	// from the pooler's log (its "login attempt" and "pooler error"
	// lines) since CloudBurrow first read it.
	SQLMySQLPoolThreads   = "cloudsql.googleapis.com/database/mysql/conn_pool/threads"
	SQLMySQLPoolPending   = "cloudsql.googleapis.com/database/mysql/conn_pool/pending_connections"
	SQLMySQLPoolAvgWait   = "cloudsql.googleapis.com/database/mysql/conn_pool/avg_wait_time"
	SQLPoolClientConns    = "cloudsql.googleapis.com/per_database/conn_pool/client_connections"
	SQLPoolClientAttempts = "cloudsql.googleapis.com/per_database/conn_pool/client_connections_attempts_count"
	SQLPoolClientAvgWait  = "cloudsql.googleapis.com/per_database/conn_pool/client_connections_avg_wait_time"
	SQLPoolClientErrors   = "cloudsql.googleapis.com/per_database/conn_pool/client_connections_error_count"
	SQLPoolNumPools       = "cloudsql.googleapis.com/per_database/conn_pool/num_pools"
	SQLPoolServerConns    = "cloudsql.googleapis.com/per_database/conn_pool/server_connections"
)

// SQLMySQLPool is a MySQL thread pool's figures at one sample.
type SQLMySQLPool struct {
	// Active and Idle are its threads; Pending the connections queued
	// for one.
	Active, Idle, Pending int64
	// Waits and WaitedUs are the statements given a thread since the pool
	// started and the microseconds they waited in all.
	Waits, WaitedUs int64
}

// SQLPGPool is one database's pooler figures at one sample.
type SQLPGPool struct {
	Database                    string
	Pools                       int64
	ClientActive, ClientWaiting int64
	ServerActive, ServerIdle    int64
	AvgWaitUs                   int64
	// Attempts and Errors are the sign-ins and the errors that
	// disconnected a client since the counting began; Counted is whether
	// they were.
	Attempts, Errors uint64
	Counted          bool
}

// SQLLoad is one user's statement totals in one database, in
// microseconds.
type SQLLoad struct {
	User, Database              string
	ExecUs, IOReadUs, IOWriteUs int64
}

// SQLWait is the backends of one user in one database seen waiting on a
// lock of one type (lw, hw or buffer_pin) at the sample.
type SQLWait struct {
	User, Database, LockType string
	Sessions                 int64
}

// SQLInstance is one Cloud SQL instance's readings at one sample.
type SQLInstance struct {
	// Project, Instance and Region name the cloudsql_database resource.
	Project, Instance, Region string
	// Engine is "POSTGRES", "MYSQL" or "SQLSERVER".
	Engine string
	// Up is whether the server serves.
	Up bool
	// HasPod is whether the pod's counters below were read.
	HasPod bool
	// CPUSeconds is the server's CPU time since its pod started.
	CPUSeconds float64
	// ReservedCores and MemoryQuota are the tier's.
	ReservedCores float64
	MemoryQuota   int64
	// MemoryUsage excludes the page cache (the working set); MemoryTotal
	// includes it.
	MemoryUsage, MemoryTotal int64
	// RxBytes and TxBytes are the pod's network counters.
	RxBytes, TxBytes uint64
	// DiskUsed is the data in use; DiskQuota the data disk's size.
	DiskUsed, DiskQuota int64
	// HasServer is whether the server's own figures below were read.
	HasServer bool
	// Connections are the client connections (MySQL, SQL Server).
	Connections int64
	// Backends are PostgreSQL's connections, by database.
	Backends map[string]int64
	// Queries is MySQL's statements executed since it started.
	Queries uint64
	// Commits and Rollbacks are PostgreSQL's transactions since its
	// statistics were reset, by database.
	Commits, Rollbacks map[string]uint64
	// Load and Waits are Query insights' readings (PostgreSQL).
	Load  []SQLLoad
	Waits []SQLWait
	// MySQLPool is MySQL's thread pool, nil where none was read; PGPools
	// PostgreSQL's pooler by database, HasPGPool whether it was read.
	MySQLPool *SQLMySQLPool
	PGPools   []SQLPGPool
	HasPGPool bool
}

// SQLSource reads every Cloud SQL instance now.
type SQLSource interface {
	SQLInstances() []SQLInstance
}

// SQLBuiltin serves cloudsql.googleapis.com/database/* from an SQLSource,
// sampled every 60 seconds; a read samples first when the last sample is
// older than minGap.
type SQLBuiltin struct {
	src    SQLSource
	now    func() time.Time
	minGap time.Duration

	mu     sync.Mutex
	lastAt time.Time
	prev   map[string]SQLInstance
	series map[string]*monitoringpb.TimeSeries
	// cumStart is each cumulative insights series' start, and locks the
	// lock time it has accumulated, by series key.
	cumStart map[string]time.Time
	cumLast  map[string]int64
}

// NewSQLBuiltin returns Cloud SQL's metrics, sampled from src.
func NewSQLBuiltin(src SQLSource, now func() time.Time) *SQLBuiltin {
	return &SQLBuiltin{src: src, now: now, minGap: 10 * time.Second, prev: map[string]SQLInstance{},
		series: map[string]*monitoringpb.TimeSeries{}, cumStart: map[string]time.Time{}, cumLast: map[string]int64{}}
}

// Sample reads every instance once.
func (b *SQLBuiltin) Sample() { b.sampleAt(b.now()) }

func sqlResource(in SQLInstance) *monitoredres.MonitoredResource {
	return &monitoredres.MonitoredResource{Type: "cloudsql_database", Labels: map[string]string{
		"project_id": in.Project, "database_id": in.Project + ":" + in.Instance, "region": in.Region}}
}

func doubleValue(f float64) *monitoringpb.TypedValue {
	return &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: f}}
}

func (b *SQLBuiltin) sampleAt(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	start := b.lastAt
	if !start.IsZero() && !now.After(start) {
		return
	}
	at := &monitoringpb.TimeInterval{StartTime: timestamppb.New(now), EndTime: timestamppb.New(now)}
	span := &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(now)}
	seen := map[string]bool{}
	for _, in := range b.src.SQLInstances() {
		key := in.Project + ":" + in.Instance
		seen[key] = true
		res := sqlResource(in)
		pt := func(metric string, labels map[string]string, kind metricpb.MetricDescriptor_MetricKind,
			vt metricpb.MetricDescriptor_ValueType, unit string, iv *monitoringpb.TimeInterval, v *monitoringpb.TypedValue) {
			if labels == nil {
				labels = map[string]string{}
			}
			ts := &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: metric, Labels: labels}, Resource: clone(res),
				MetricKind: kind, ValueType: vt, Unit: unit}
			k := seriesKey(ts)
			if cur := b.series[k]; cur != nil {
				ts = cur
			} else {
				b.series[k] = ts
			}
			ts.Points = append(ts.Points, &monitoringpb.Point{Interval: iv, Value: v})
			if len(ts.Points) > tasksRing {
				ts.Points = ts.Points[len(ts.Points)-tasksRing:]
			}
		}
		up := int64(0)
		if in.Up {
			up = 1
		}
		pt(SQLUp, nil, gauge, tInt, "1", at, int64Value(up))
		prev, had := b.prev[key]
		elapsed := now.Sub(start).Seconds()
		if had && !start.IsZero() && in.Up && prev.Up {
			pt(SQLUptime, nil, delta, tInt, "s", span, int64Value(int64(elapsed+0.5)))
		}
		if in.HasPod {
			if in.ReservedCores > 0 {
				pt(SQLCPUReserved, nil, gauge, tDouble, "1", at, doubleValue(in.ReservedCores))
			}
			if in.MemoryQuota > 0 {
				pt(SQLMemoryQuota, nil, gauge, tInt, "By", at, int64Value(in.MemoryQuota))
				pt(SQLMemoryUtil, nil, gauge, tDouble, "1", at, doubleValue(float64(in.MemoryUsage)/float64(in.MemoryQuota)))
			}
			pt(SQLMemoryUsage, nil, gauge, tInt, "By", at, int64Value(in.MemoryUsage))
			pt(SQLMemoryTotal, nil, gauge, tInt, "By", at, int64Value(in.MemoryTotal))
			if had && prev.HasPod && !start.IsZero() && in.CPUSeconds >= prev.CPUSeconds && elapsed > 0 {
				used := in.CPUSeconds - prev.CPUSeconds
				pt(SQLCPUUsageTime, nil, delta, tDouble, "s{CPU}", span, doubleValue(used))
				if in.ReservedCores > 0 {
					pt(SQLCPUUtilization, nil, gauge, tDouble, "10^2.%", at, doubleValue(used/elapsed/in.ReservedCores))
				}
			}
			if had && prev.HasPod && !start.IsZero() && in.RxBytes >= prev.RxBytes && in.TxBytes >= prev.TxBytes {
				pt(SQLReceivedBytes, nil, delta, tInt, "By", span, int64Value(int64(in.RxBytes-prev.RxBytes)))
				pt(SQLSentBytes, map[string]string{"destination": ""}, delta, tInt, "By", span, int64Value(int64(in.TxBytes-prev.TxBytes)))
			}
		}
		if in.DiskQuota > 0 {
			pt(SQLDiskQuota, nil, gauge, tInt, "By", at, int64Value(in.DiskQuota))
			if in.HasPod || in.HasServer {
				pt(SQLDiskUtil, nil, gauge, tDouble, "1", at, doubleValue(float64(in.DiskUsed)/float64(in.DiskQuota)))
			}
		}
		if in.HasPod || in.HasServer {
			pt(SQLDiskBytesUsed, nil, gauge, tInt, "By", at, int64Value(in.DiskUsed))
		}
		if in.HasServer {
			if in.Engine == "POSTGRES" {
				for _, db := range sqlKeys(in.Backends) {
					pt(SQLPGBackends, map[string]string{"database": db}, gauge, tInt, "1", at, int64Value(in.Backends[db]))
				}
				if had && prev.HasServer && !start.IsZero() {
					for _, db := range sqlKeys(in.Commits) {
						for typ, m := range map[string][2]map[string]uint64{"commit": {in.Commits, prev.Commits}, "rollback": {in.Rollbacks, prev.Rollbacks}} {
							cur, old := m[0][db], m[1][db]
							if _, ok := m[1][db]; ok && cur >= old {
								pt(SQLPGTransactions, map[string]string{"database": db, "transaction_type": typ}, delta, tInt, "1", span, int64Value(int64(cur-old)))
							}
						}
					}
				}
			} else {
				pt(SQLConnections, nil, gauge, tInt, "1", at, int64Value(in.Connections))
			}
			if in.Engine == "MYSQL" && had && prev.HasServer && !start.IsZero() && in.Queries >= prev.Queries {
				pt(SQLMySQLQueries, nil, delta, tInt, "1", span, int64Value(int64(in.Queries-prev.Queries)))
			}
		}
		if in.HasServer && in.Engine == "POSTGRES" {
			b.insightsLocked(in, now, start, had && prev.HasServer)
		}
		if mp := in.MySQLPool; mp != nil {
			for _, st := range []struct {
				status string
				n      int64
			}{{"active", mp.Active}, {"idle", mp.Idle}} {
				pt(SQLMySQLPoolThreads, map[string]string{"status": st.status}, gauge, tInt, "1", at, int64Value(st.n))
			}
			pt(SQLMySQLPoolPending, nil, gauge, tInt, "1", at, int64Value(mp.Pending))
			// The sample's average: the waits since the last sample, or
			// since the pool started at the first.
			waits, waited := mp.Waits, mp.WaitedUs
			if pp := prev.MySQLPool; had && pp != nil && mp.Waits >= pp.Waits && mp.WaitedUs >= pp.WaitedUs {
				waits, waited = mp.Waits-pp.Waits, mp.WaitedUs-pp.WaitedUs
			}
			avg := int64(0)
			if waits > 0 {
				avg = (waited/waits + 500) / 1000
			}
			pt(SQLMySQLPoolAvgWait, nil, gauge, tInt, "ms", at, int64Value(avg))
		}
		if in.HasPGPool {
			b.pgPoolLocked(in, now)
		}
		b.prev[key] = in
	}
	for k := range b.prev {
		if !seen[k] {
			delete(b.prev, k)
		}
	}
	b.lastAt = now
}

// insightsLocked records in's Query insights metrics at now.
func (b *SQLBuiltin) insightsLocked(in SQLInstance, now, last time.Time, hadServer bool) {
	add := func(metric, user, db string, extra [2]string, value int64, accumulate bool) {
		labels := map[string]string{"user": user, "client_addr": ""}
		if extra[0] != "" {
			labels[extra[0]] = extra[1]
		}
		res := &monitoredres.MonitoredResource{Type: "cloudsql_instance_database", Labels: map[string]string{
			"project_id": in.Project, "location": in.Region, "resource_id": in.Project + ":" + in.Instance, "database": db}}
		ts := &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: metric, Labels: labels}, Resource: res,
			MetricKind: cumulative, ValueType: tInt, Unit: "us"}
		if metric == SQLPGInsightsExecTime {
			ts.Unit = "us{CPU}"
		}
		k := seriesKey(ts)
		if cur := b.series[k]; cur != nil {
			ts = cur
		} else {
			b.series[k] = ts
		}
		if accumulate {
			value += b.cumLast[k]
		} else if value < b.cumLast[k] {
			// The statistics were reset: the series starts again.
			delete(b.cumStart, k)
		}
		st, ok := b.cumStart[k]
		if !ok {
			st = now
			if accumulate && !last.IsZero() {
				st = last
			}
			b.cumStart[k] = st
		}
		b.cumLast[k] = value
		ts.Points = append(ts.Points, &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(st),
			EndTime: timestamppb.New(now)}, Value: int64Value(value)})
		if len(ts.Points) > tasksRing {
			ts.Points = ts.Points[len(ts.Points)-tasksRing:]
		}
	}
	for _, l := range in.Load {
		add(SQLPGInsightsExecTime, l.User, l.Database, [2]string{}, l.ExecUs, false)
		add(SQLPGInsightsIOTime, l.User, l.Database, [2]string{"io_type", "read"}, l.IOReadUs, false)
		add(SQLPGInsightsIOTime, l.User, l.Database, [2]string{"io_type", "write"}, l.IOWriteUs, false)
	}
	if !hadServer || last.IsZero() {
		return
	}
	us := now.Sub(last).Microseconds()
	for _, w := range in.Waits {
		add(SQLPGInsightsLockTime, w.User, w.Database, [2]string{"lock_type", w.LockType}, w.Sessions*us, true)
	}
}

// pgPoolLocked records in's pooler metrics at now, each database's on
// the cloudsql_instance_database resource.
func (b *SQLBuiltin) pgPoolLocked(in SQLInstance, now time.Time) {
	at := &monitoringpb.TimeInterval{StartTime: timestamppb.New(now), EndTime: timestamppb.New(now)}
	for _, pd := range in.PGPools {
		res := &monitoredres.MonitoredResource{Type: "cloudsql_instance_database", Labels: map[string]string{
			"project_id": in.Project, "location": in.Region, "resource_id": in.Project + ":" + in.Instance, "database": pd.Database}}
		add := func(metric string, labels map[string]string, kind metricpb.MetricDescriptor_MetricKind, unit string, v int64) {
			if labels == nil {
				labels = map[string]string{}
			}
			ts := &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: metric, Labels: labels}, Resource: clone(res),
				MetricKind: kind, ValueType: tInt, Unit: unit}
			k := seriesKey(ts)
			if cur := b.series[k]; cur != nil {
				ts = cur
			} else {
				b.series[k] = ts
			}
			iv := at
			if kind == cumulative {
				st, ok := b.cumStart[k]
				if !ok || v < b.cumLast[k] {
					st = now
					b.cumStart[k] = st
				}
				b.cumLast[k] = v
				iv = &monitoringpb.TimeInterval{StartTime: timestamppb.New(st), EndTime: timestamppb.New(now)}
			}
			ts.Points = append(ts.Points, &monitoringpb.Point{Interval: iv, Value: int64Value(v)})
			if len(ts.Points) > tasksRing {
				ts.Points = ts.Points[len(ts.Points)-tasksRing:]
			}
		}
		add(SQLPoolClientConns, map[string]string{"status": "active"}, gauge, "1", pd.ClientActive)
		add(SQLPoolClientConns, map[string]string{"status": "waiting"}, gauge, "1", pd.ClientWaiting)
		add(SQLPoolServerConns, map[string]string{"status": "active"}, gauge, "1", pd.ServerActive)
		add(SQLPoolServerConns, map[string]string{"status": "idle"}, gauge, "1", pd.ServerIdle)
		add(SQLPoolNumPools, nil, gauge, "1", pd.Pools)
		add(SQLPoolClientAvgWait, nil, gauge, "us", pd.AvgWaitUs)
		if pd.Counted {
			add(SQLPoolClientAttempts, nil, cumulative, "1", int64(pd.Attempts))
			add(SQLPoolClientErrors, nil, cumulative, "1", int64(pd.Errors))
		}
	}
}

func sqlKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sqlDescriptors are the descriptors, as Google publishes them.
var sqlDescriptors = []struct {
	typ, name, desc, unit string
	kind                  metricpb.MetricDescriptor_MetricKind
	vt                    metricpb.MetricDescriptor_ValueType
	labels                []string
}{
	{SQLMySQLPoolAvgWait, "MySQL Connection Pool Average Wait Time", "Average time spent by the connection on the work queue in milliseconds.", "ms", gauge, tInt, nil},
	{SQLMySQLPoolPending, "MySQL Connection Pool Pending Connections", "Number of connections presently enqueued in the work queue.", "1", gauge, tInt, nil},
	{SQLMySQLPoolThreads, "MySQL Connection Pool Threads", "Number of threads in the connection pool.", "1", gauge, tInt, []string{"status"}},
	{SQLPoolClientConns, "Client connections", "Number of client connections per database grouped by status.", "1", gauge, tInt, []string{"status"}},
	{SQLPoolClientAttempts, "Client connection attempts count", "Number of client connection attempts received per database.", "1", cumulative, tInt, nil},
	{SQLPoolClientAvgWait, "Average wait time", "Average wait time spent by clients waiting for a server connection per database in microseconds.", "us", gauge, tInt, nil},
	{SQLPoolClientErrors, "Error count", "Count of errors that cause client disconnection including auth error, client connection setup error, server connection error and timeout errors.", "1", cumulative, tInt, nil},
	{SQLPoolNumPools, "Number of connection pools", "Number of connection pools per database.", "1", gauge, tInt, nil},
	{SQLPoolServerConns, "Server connections", "Number of server connections per database grouped by status.", "1", gauge, tInt, []string{"status"}},
	{SQLCPUUtilization, "CPU utilization", "Current CPU utilization represented as a percentage of the reserved CPU that is currently in use.", "10^2.%", gauge, tDouble, nil},
	{SQLCPUUsageTime, "CPU usage", "Cumulative CPU usage time in seconds.", "s{CPU}", delta, tDouble, nil},
	{SQLCPUReserved, "CPU reserved cores", "Number of cores reserved for the database.", "1", gauge, tDouble, nil},
	{SQLMemoryUsage, "Memory usage", "RAM usage in bytes. This metric reports the RAM usage of the server, excluding buffer/cache.", "By", gauge, tInt, nil},
	{SQLMemoryTotal, "Total memory usage", "Total RAM usage in bytes. This metric reports the RAM usage of the database process, including the buffer/cache.", "By", gauge, tInt, nil},
	{SQLMemoryQuota, "Memory quota", "Maximum RAM size in bytes.", "By", gauge, tInt, nil},
	{SQLMemoryUtil, "Memory utilization", "Memory utilization represented as a fraction of the memory quota that is currently in use.", "1", gauge, tDouble, nil},
	{SQLDiskBytesUsed, "Bytes used", "Data utilization in bytes.", "By", gauge, tInt, nil},
	{SQLDiskQuota, "Disk quota", "Maximum data disk size in bytes.", "By", gauge, tInt, nil},
	{SQLDiskUtil, "Disk utilization", "The fraction of the disk quota that is currently in use.", "1", gauge, tDouble, nil},
	{SQLConnections, "Cloud SQL Connections", "Number of connections to databases on the instance. Only applicable to MySQL and SQL Server.", "1", gauge, tInt, nil},
	{SQLPGBackends, "PostgreSQL Connections", "Number of connections to the Cloud SQL PostgreSQL instance.", "1", gauge, tInt, []string{"database"}},
	{SQLReceivedBytes, "Received bytes", "Delta count of bytes received through the network.", "By", delta, tInt, nil},
	{SQLSentBytes, "Sent bytes", "Delta count of bytes sent through the network.", "By", delta, tInt, []string{"destination"}},
	{SQLUp, "Server up", "Indicates if the server is up or not.", "1", gauge, tInt, nil},
	{SQLUptime, "Uptime", "Delta count of the time in seconds the instance has been running.", "s", delta, tInt, nil},
	{SQLMySQLQueries, "Queries", "Delta count of statements executed by the server.", "1", delta, tInt, nil},
	{SQLPGTransactions, "Number of transactions", "Delta count of number of transactions.", "1", delta, tInt, []string{"database", "transaction_type"}},
	{SQLPGInsightsExecTime, "Execution time", "Accumulated query execution time per user per database. This is the sum of cpu time, io wait time, lock wait time, process context switch, and scheduling for all the processes involved in the query execution.", "us{CPU}", cumulative, tInt, []string{"user", "client_addr"}},
	{SQLPGInsightsIOTime, "IO time", "Accumulated IO time per user per database.", "us", cumulative, tInt, []string{"user", "client_addr", "io_type"}},
	{SQLPGInsightsLockTime, "Aggregated lock time", "Accumulated lock wait time per user per database.", "us", cumulative, tInt, []string{"user", "client_addr", "lock_type"}},
}

func (b *SQLBuiltin) Descriptors(project string) []*metricpb.MetricDescriptor {
	out := make([]*metricpb.MetricDescriptor, 0, len(sqlDescriptors))
	for _, d := range sqlDescriptors {
		md := &metricpb.MetricDescriptor{Name: descriptorName(project, d.typ), Type: d.typ, MetricKind: d.kind, ValueType: d.vt,
			Unit: d.unit, DisplayName: d.name, Description: d.desc, MonitoredResourceTypes: []string{"cloudsql_database"}}
		if strings.Contains(d.typ, "/insights/") || strings.Contains(d.typ, "/per_database/") {
			md.MonitoredResourceTypes = []string{"cloudsql_instance_database"}
		}
		for _, l := range d.labels {
			md.Labels = append(md.Labels, &labelpb.LabelDescriptor{Key: l, ValueType: labelpb.LabelDescriptor_STRING})
		}
		out = append(out, md)
	}
	return out
}

func (b *SQLBuiltin) Series(project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	known := false
	for _, d := range sqlDescriptors {
		known = known || d.typ == metricType
	}
	if !known {
		return nil, nil
	}
	at := b.now()
	b.mu.Lock()
	stale := b.lastAt.IsZero() || at.Sub(b.lastAt) >= b.minGap
	b.mu.Unlock()
	if stale && !end.Before(at) {
		b.sampleAt(at)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*monitoringpb.TimeSeries
	for _, ts := range b.series {
		if ts.GetMetric().GetType() != metricType || ts.GetResource().GetLabels()["project_id"] != project {
			continue
		}
		c := &monitoringpb.TimeSeries{Metric: clone(ts.Metric), Resource: clone(ts.Resource),
			MetricKind: ts.MetricKind, ValueType: ts.ValueType, Unit: ts.Unit}
		for _, p := range ts.Points {
			e := p.GetInterval().GetEndTime().AsTime()
			if !e.Before(start) && !e.After(end) {
				c.Points = append(c.Points, clone(p))
			}
		}
		if len(c.Points) > 0 {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return seriesKey(out[i]) < seriesKey(out[j]) })
	return out, nil
}
