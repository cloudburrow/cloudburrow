package monitoring

// ---- AlloyDB for PostgreSQL: alloydb.googleapis.com/* (#2251) ----
//
// Google's AlloyDB metrics (https://docs.cloud.google.com/monitoring/api/metrics_gcp_a_b,
// the alloydb section, read 2026-11-25), "Sampled every 60 seconds", on
// the alloydb.googleapis.com/Cluster, Instance and Database resources
// (resource_container, location, cluster_id, instance_id, database). They
// are read from an AlloyDBSource: each instance's server, its pod's own
// counters as the kubelet reports them (CPU time, memory) and the
// server's own figures (pg_stat_activity, pg_stat_database,
// pg_stat_statements and the rest; internal/service/alloydb/insights.go).
// A GAUGE is the sample's reading; a DELTA the change since the last
// sample (none across a restart or a statistics reset, whose counters
// start again from zero). The metrics no reading backs (the latency
// distributions, wait events, the ultra-fast cache, node/*, ML
// integration, audit logging) are not served: docs/gaps.md lists them.

import (
	"sort"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The AlloyDB metric types the console reads.
const (
	AlloyDBPrefix          = "alloydb.googleapis.com/"
	AlloyDBStorage         = AlloyDBPrefix + "cluster/storage/usage"
	AlloyDBLastBackup      = AlloyDBPrefix + "cluster/last_backup_timestamp"
	AlloyDBCPUAvg          = AlloyDBPrefix + "instance/cpu/average_utilization"
	AlloyDBCPUMax          = AlloyDBPrefix + "instance/cpu/maximum_utilization"
	AlloyDBVCPUs           = AlloyDBPrefix + "instance/cpu/vcpus"
	AlloyDBMemAvailable    = AlloyDBPrefix + "instance/memory/min_available_memory"
	AlloyDBConnections     = AlloyDBPrefix + "instance/postgres/total_connections"
	AlloyDBConnLimit       = AlloyDBPrefix + "instance/postgres/connections_limit"
	AlloyDBBackendsByState = AlloyDBPrefix + "instance/postgresql/backends_by_state"
	AlloyDBBackendsByApp   = AlloyDBPrefix + "instance/postgresql/backends_for_top_applications"
	AlloyDBTransactions    = AlloyDBPrefix + "instance/postgres/transaction_count"
	AlloyDBCommits         = AlloyDBPrefix + "instance/postgres/commit_count"
	AlloyDBAborts          = AlloyDBPrefix + "instance/postgres/abort_count"
	AlloyDBNodes           = AlloyDBPrefix + "instance/postgres/instances"
	AlloyDBReplicationLag  = AlloyDBPrefix + "instance/postgres/replication/maximum_lag"
	AlloyDBBlksHit         = AlloyDBPrefix + "instance/postgresql/blks_hit"
	AlloyDBBlksRead        = AlloyDBPrefix + "instance/postgresql/blks_read"
	AlloyDBDeadlocks       = AlloyDBPrefix + "instance/postgresql/deadlock_count"
	AlloyDBTuplesWritten   = AlloyDBPrefix + "instance/postgresql/written_tuples_count"
	AlloyDBTuplesInserted  = AlloyDBPrefix + "instance/postgresql/inserted_tuples_count"
	AlloyDBTuplesUpdated   = AlloyDBPrefix + "instance/postgresql/updated_tuples_count"
	AlloyDBTuplesDeleted   = AlloyDBPrefix + "instance/postgresql/deleted_tuples_count"
	AlloyDBTuplesFetched   = AlloyDBPrefix + "instance/postgresql/fetched_tuples_count"
	AlloyDBTuplesReturned  = AlloyDBPrefix + "instance/postgresql/returned_tuples_count"
	AlloyDBTempBytes       = AlloyDBPrefix + "instance/postgresql/temp_bytes_written_count"
	AlloyDBTempFiles       = AlloyDBPrefix + "instance/postgresql/temp_files_written_count"
	AlloyDBNewConnections  = AlloyDBPrefix + "instance/postgresql/new_connections_count"
	AlloyDBOldestXact      = AlloyDBPrefix + "database/postgresql/vacuum/oldest_transaction_age"
	AlloyDBXIDUtilization  = AlloyDBPrefix + "database/postgresql/vacuum/transaction_id_utilization"
	AlloyDBDBBackends      = AlloyDBPrefix + "database/postgresql/backends_for_top_databases"
	AlloyDBDBCommits       = AlloyDBPrefix + "database/postgresql/committed_transactions_for_top_databases"
	AlloyDBDBRollbacks     = AlloyDBPrefix + "database/postgresql/rolledback_transactions_for_top_databases"
	AlloyDBDBTuples        = AlloyDBPrefix + "database/postgresql/tuples"
	AlloyDBExecTime        = AlloyDBPrefix + "database/postgresql/insights/aggregate/execution_time"
	AlloyDBIOTime          = AlloyDBPrefix + "database/postgresql/insights/aggregate/io_time"
	AlloyDBLockTime        = AlloyDBPrefix + "database/postgresql/insights/aggregate/lock_time"
	AlloyDBRowCount        = AlloyDBPrefix + "database/postgresql/insights/aggregate/row_count"
	AlloyDBSharedBlks      = AlloyDBPrefix + "database/postgresql/insights/aggregate/shared_blk_access_count"
	AlloyDBStatements      = AlloyDBPrefix + "database/postgresql/statements_executed_count"

	AlloyDBClusterResource  = "alloydb.googleapis.com/Cluster"
	AlloyDBInstanceResource = "alloydb.googleapis.com/Instance"
	AlloyDBDatabaseResource = "alloydb.googleapis.com/Database"
)

// AlloyDBDatabase is one database's readings: pg_stat_database's
// counters since its statistics were reset, its connections now, and its
// rows by state where HasTuples.
type AlloyDBDatabase struct {
	Name                                          string
	Backends                                      int64
	Commits, Rollbacks, BlksHit, BlksRead         int64
	Returned, Fetched, Inserted, Updated, Deleted int64
	TempFiles, TempBytes, Deadlocks, Sessions     int64
	Live, Dead                                    int64
	HasTuples                                     bool
}

// AlloyDBLoad is one user's statement totals in one database since
// pg_stat_statements was reset; times in microseconds.
type AlloyDBLoad struct {
	User, Database                    string
	ExecUs, IOReadUs, IOWriteUs, Rows int64
	SharedHit, SharedRead             int64
}

// AlloyDBStatementCount is one database's statements of one operation type
// since pg_stat_statements was reset.
type AlloyDBStatementCount struct {
	Database, Operation string
	Calls               int64
}

// AlloyDBWait is the backends of one user in one database seen waiting on
// a lock of one type (lw, hw or buffer_pin) at the sample.
type AlloyDBWait struct {
	User, Database, LockType string
	Sessions                 int64
}

// AlloyDBInstance is one instance's readings at one sample.
type AlloyDBInstance struct {
	Project, Location, Cluster, Instance string
	// ReadPool is whether it is a read pool instance.
	ReadPool bool
	// VCPUs is each node's vCPUs; Nodes the nodes it is configured with.
	VCPUs, Nodes int64
	// Up is whether its server serves.
	Up bool
	// HasPod is whether the pod's counters below were read: its CPU time
	// since it started, its memory in use (the working set) and the
	// machine's memory (8 GB a vCPU, as AlloyDB's machines have).
	HasPod                    bool
	CPUSeconds                float64
	MemoryUsed, MemoryMachine int64
	// HasServer is whether the server's figures below were read.
	HasServer              bool
	MaxConnections         int64
	ByState, ByApplication map[string]int64
	Databases              []AlloyDBDatabase
	XIDUtilization         float64
	OldestAge              map[string]int64
	// Replica is whether the server is a standby, ReplayLagMs its lag.
	Replica     bool
	ReplayLagMs int64
	// HasLoad is whether pg_stat_statements was read into Load and
	// Statements.
	HasLoad    bool
	Load       []AlloyDBLoad
	Statements []AlloyDBStatementCount
	Waits      []AlloyDBWait
}

// AlloyDBCluster is one cluster's readings at one sample: its primary's
// databases' size, and its newest backup of each type.
type AlloyDBCluster struct {
	Project, Location, Cluster string
	StorageBytes               int64
	HasStorage                 bool
	LastBackups                map[string]time.Time
}

// AlloyDBSource reads every AlloyDB cluster and instance now.
type AlloyDBSource interface {
	AlloyDBSample() ([]AlloyDBCluster, []AlloyDBInstance)
}

// AlloyDBBuiltin serves alloydb.googleapis.com metrics from an
// AlloyDBSource, sampled every 60 seconds; a read samples first when the
// last sample is older than minGap.
type AlloyDBBuiltin struct {
	src    AlloyDBSource
	now    func() time.Time
	minGap time.Duration

	mu     sync.Mutex
	lastAt time.Time
	prev   map[string]AlloyDBInstance
	series map[string]*monitoringpb.TimeSeries
}

// NewAlloyDBBuiltin returns AlloyDB's metrics, sampled from src.
func NewAlloyDBBuiltin(src AlloyDBSource, now func() time.Time) *AlloyDBBuiltin {
	return &AlloyDBBuiltin{src: src, now: now, minGap: 10 * time.Second, prev: map[string]AlloyDBInstance{},
		series: map[string]*monitoringpb.TimeSeries{}}
}

// Sample reads every cluster and instance once.
func (b *AlloyDBBuiltin) Sample() { b.sampleAt(b.now()) }

func alloyDBClusterRes(project, location, cluster string) *monitoredres.MonitoredResource {
	return &monitoredres.MonitoredResource{Type: AlloyDBClusterResource, Labels: map[string]string{
		"resource_container": project, "location": location, "cluster_id": cluster}}
}

func alloyDBInstanceRes(in AlloyDBInstance) *monitoredres.MonitoredResource {
	return &monitoredres.MonitoredResource{Type: AlloyDBInstanceResource, Labels: map[string]string{
		"resource_container": in.Project, "location": in.Location, "cluster_id": in.Cluster, "instance_id": in.Instance}}
}

func alloyDBDatabaseRes(in AlloyDBInstance, db string) *monitoredres.MonitoredResource {
	r := alloyDBInstanceRes(in)
	r.Type = AlloyDBDatabaseResource
	r.Labels["database"] = db
	return r
}

// add appends a point to its series; the caller holds mu.
func (b *AlloyDBBuiltin) add(metric string, labels map[string]string, res *monitoredres.MonitoredResource,
	iv *monitoringpb.TimeInterval, v *monitoringpb.TypedValue) {
	d := alloyDBDescriptorOf(metric)
	if d == nil {
		return
	}
	if labels == nil {
		labels = map[string]string{}
	}
	ts := &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: metric, Labels: labels}, Resource: res,
		MetricKind: d.kind, ValueType: d.vt, Unit: d.unit}
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

// alloyDBDBCounters are the per-database DELTAs, each a pg_stat_database
// counter, and the instance-wide metric that sums it ("" for none).
var alloyDBDBCounters = []struct {
	db, instance string
	v            func(d AlloyDBDatabase) int64
}{
	{AlloyDBDBCommits, AlloyDBCommits, func(d AlloyDBDatabase) int64 { return d.Commits }},
	{AlloyDBDBRollbacks, AlloyDBAborts, func(d AlloyDBDatabase) int64 { return d.Rollbacks }},
	{AlloyDBPrefix + "database/postgresql/blks_hit_for_top_databases", AlloyDBBlksHit, func(d AlloyDBDatabase) int64 { return d.BlksHit }},
	{AlloyDBPrefix + "database/postgresql/blks_read_for_top_databases", AlloyDBBlksRead, func(d AlloyDBDatabase) int64 { return d.BlksRead }},
	{AlloyDBPrefix + "database/postgresql/deadlock_count_for_top_databases", AlloyDBDeadlocks, func(d AlloyDBDatabase) int64 { return d.Deadlocks }},
	{AlloyDBPrefix + "database/postgresql/inserted_tuples_count_for_top_databases", AlloyDBTuplesInserted, func(d AlloyDBDatabase) int64 { return d.Inserted }},
	{AlloyDBPrefix + "database/postgresql/updated_tuples_count_for_top_databases", AlloyDBTuplesUpdated, func(d AlloyDBDatabase) int64 { return d.Updated }},
	{AlloyDBPrefix + "database/postgresql/deleted_tuples_count_for_top_databases", AlloyDBTuplesDeleted, func(d AlloyDBDatabase) int64 { return d.Deleted }},
	{AlloyDBPrefix + "database/postgresql/written_tuples_count_for_top_databases", AlloyDBTuplesWritten, func(d AlloyDBDatabase) int64 { return d.Inserted + d.Updated + d.Deleted }},
	{AlloyDBPrefix + "database/postgresql/fetched_tuples_count_for_top_databases", AlloyDBTuplesFetched, func(d AlloyDBDatabase) int64 { return d.Fetched }},
	{AlloyDBPrefix + "database/postgresql/returned_tuples_count_for_top_databases", AlloyDBTuplesReturned, func(d AlloyDBDatabase) int64 { return d.Returned }},
	{AlloyDBPrefix + "database/postgresql/temp_bytes_written_for_top_databases", AlloyDBTempBytes, func(d AlloyDBDatabase) int64 { return d.TempBytes }},
	{AlloyDBPrefix + "database/postgresql/temp_files_written_for_top_databases", AlloyDBTempFiles, func(d AlloyDBDatabase) int64 { return d.TempFiles }},
	{AlloyDBPrefix + "database/postgresql/new_connections_for_top_databases", AlloyDBNewConnections, func(d AlloyDBDatabase) int64 { return d.Sessions }},
}

func (b *AlloyDBBuiltin) sampleAt(now time.Time) {
	clusters, instances := b.src.AlloyDBSample()
	b.mu.Lock()
	defer b.mu.Unlock()
	start := b.lastAt
	if !start.IsZero() && !now.After(start) {
		return
	}
	at := &monitoringpb.TimeInterval{StartTime: timestamppb.New(now), EndTime: timestamppb.New(now)}
	span := &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(now)}
	for _, c := range clusters {
		res := alloyDBClusterRes(c.Project, c.Location, c.Cluster)
		if c.HasStorage {
			b.add(AlloyDBStorage, nil, clone(res), at, int64Value(c.StorageBytes))
		}
		for _, typ := range sqlKeys(c.LastBackups) {
			b.add(AlloyDBLastBackup, map[string]string{"backup_type": typ}, clone(res), at, int64Value(c.LastBackups[typ].UnixMicro()))
		}
	}
	// Each cluster's primary, for its read pools' replication lag.
	primaries := map[string]AlloyDBInstance{}
	for _, in := range instances {
		if !in.ReadPool {
			primaries[in.Project+"/"+in.Location+"/"+in.Cluster] = in
		}
	}
	seen := map[string]bool{}
	elapsed := now.Sub(start)
	for _, in := range instances {
		key := in.Project + "/" + in.Location + "/" + in.Cluster + "/" + in.Instance
		seen[key] = true
		res := alloyDBInstanceRes(in)
		prev, had := b.prev[key]
		up, down := int64(0), in.Nodes
		if in.Up {
			up, down = in.Nodes, 0
		}
		b.add(AlloyDBNodes, map[string]string{"status": "up"}, clone(res), at, int64Value(up))
		b.add(AlloyDBNodes, map[string]string{"status": "down"}, clone(res), at, int64Value(down))
		b.add(AlloyDBVCPUs, nil, clone(res), at, int64Value(in.VCPUs))
		withPrev := had && !start.IsZero() && elapsed > 0
		if in.HasPod {
			if in.MemoryMachine > 0 {
				avail := in.MemoryMachine - in.MemoryUsed
				if avail < 0 {
					avail = 0
				}
				b.add(AlloyDBMemAvailable, nil, clone(res), at, int64Value(avail))
			}
			if withPrev && prev.HasPod && in.CPUSeconds >= prev.CPUSeconds && in.VCPUs > 0 {
				// One server serves the instance: its mean and maximum are
				// the same.
				pct := (in.CPUSeconds - prev.CPUSeconds) / elapsed.Seconds() / float64(in.VCPUs) * 100
				b.add(AlloyDBCPUAvg, nil, clone(res), at, doubleValue(pct))
				b.add(AlloyDBCPUMax, nil, clone(res), at, doubleValue(pct))
			}
		}
		if in.HasServer {
			total := int64(0)
			for _, st := range sqlKeys(in.ByState) {
				total += in.ByState[st]
				b.add(AlloyDBBackendsByState, map[string]string{"state": st}, clone(res), at, int64Value(in.ByState[st]))
			}
			for _, app := range sqlKeys(in.ByApplication) {
				b.add(AlloyDBBackendsByApp, map[string]string{"application_name": app}, clone(res), at, int64Value(in.ByApplication[app]))
			}
			b.add(AlloyDBConnections, nil, clone(res), at, int64Value(total))
			b.add(AlloyDBConnLimit, nil, clone(res), at, int64Value(in.MaxConnections))
			b.add(AlloyDBXIDUtilization, nil, clone(res), at, doubleValue(in.XIDUtilization))
			for _, typ := range sqlKeys(in.OldestAge) {
				b.add(AlloyDBOldestXact, map[string]string{"type": typ}, clone(res), at, int64Value(in.OldestAge[typ]))
			}
			if in.Replica && in.ReadPool {
				if p, ok := primaries[in.Project+"/"+in.Location+"/"+in.Cluster]; ok {
					b.add(AlloyDBReplicationLag, map[string]string{"replica_instance_id": in.Instance}, alloyDBInstanceRes(p), at, int64Value(in.ReplayLagMs))
				}
			}
			for _, d := range in.Databases {
				b.add(AlloyDBDBBackends, nil, alloyDBDatabaseRes(in, d.Name), at, int64Value(d.Backends))
				if d.HasTuples {
					b.add(AlloyDBDBTuples, map[string]string{"state": "live"}, alloyDBDatabaseRes(in, d.Name), at, int64Value(d.Live))
					b.add(AlloyDBDBTuples, map[string]string{"state": "dead"}, alloyDBDatabaseRes(in, d.Name), at, int64Value(d.Dead))
				}
			}
			if withPrev && prev.HasServer {
				b.deltasLocked(in, prev, res, span)
			}
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

// deltasLocked records in's DELTAs over span, against prev's counters;
// a counter that went down (a restart, a reset) records nothing.
func (b *AlloyDBBuiltin) deltasLocked(in, prev AlloyDBInstance, res *monitoredres.MonitoredResource, span *monitoringpb.TimeInterval) {
	old := map[string]AlloyDBDatabase{}
	for _, d := range prev.Databases {
		old[d.Name] = d
	}
	sums := map[string]int64{}
	sumsOK := map[string]bool{}
	var commits, aborts int64
	txOK := true
	for _, c := range alloyDBDBCounters {
		sumsOK[c.instance] = true
	}
	for _, d := range in.Databases {
		o, ok := old[d.Name]
		for _, c := range alloyDBDBCounters {
			if !ok {
				sumsOK[c.instance] = false
				continue
			}
			n := c.v(d) - c.v(o)
			if n < 0 {
				sumsOK[c.instance] = false
				continue
			}
			b.add(c.db, nil, alloyDBDatabaseRes(in, d.Name), span, int64Value(n))
			sums[c.instance] += n
			switch c.instance {
			case AlloyDBCommits:
				commits += n
			case AlloyDBAborts:
				aborts += n
			}
		}
		if !ok || d.Commits < o.Commits || d.Rollbacks < o.Rollbacks {
			txOK = false
		}
	}
	for _, c := range alloyDBDBCounters {
		// The instance-wide tuples written is the sum of the three below.
		if c.instance != "" && c.instance != AlloyDBTuplesWritten && sumsOK[c.instance] {
			b.add(c.instance, nil, clone(res), span, int64Value(sums[c.instance]))
		}
	}
	if sumsOK[AlloyDBTuplesInserted] && sumsOK[AlloyDBTuplesUpdated] && sumsOK[AlloyDBTuplesDeleted] {
		b.add(AlloyDBTuplesWritten, nil, clone(res), span, int64Value(sums[AlloyDBTuplesInserted]+sums[AlloyDBTuplesUpdated]+sums[AlloyDBTuplesDeleted]))
	}
	if txOK {
		b.add(AlloyDBTransactions, nil, clone(res), span, int64Value(commits+aborts))
	}
	if !in.HasLoad || !prev.HasLoad {
		return
	}
	oldLoad := map[[2]string]AlloyDBLoad{}
	for _, l := range prev.Load {
		oldLoad[[2]string{l.User, l.Database}] = l
	}
	for _, l := range in.Load {
		o, ok := oldLoad[[2]string{l.User, l.Database}]
		if !ok {
			// New since the last sample: its totals are this span's.
			o = AlloyDBLoad{}
		}
		if l.ExecUs < o.ExecUs || l.Rows < o.Rows || l.SharedHit < o.SharedHit || l.SharedRead < o.SharedRead ||
			l.IOReadUs < o.IOReadUs || l.IOWriteUs < o.IOWriteUs {
			continue
		}
		dr := alloyDBDatabaseRes(in, l.Database)
		lab := func(extra ...string) map[string]string {
			m := map[string]string{"user": l.User, "client_addr": ""}
			for i := 0; i+1 < len(extra); i += 2 {
				m[extra[i]] = extra[i+1]
			}
			return m
		}
		b.add(AlloyDBExecTime, lab(), clone(dr), span, int64Value(l.ExecUs-o.ExecUs))
		b.add(AlloyDBIOTime, lab("io_type", "read"), clone(dr), span, int64Value(l.IOReadUs-o.IOReadUs))
		b.add(AlloyDBIOTime, lab("io_type", "write"), clone(dr), span, int64Value(l.IOWriteUs-o.IOWriteUs))
		b.add(AlloyDBRowCount, lab(), clone(dr), span, int64Value(l.Rows-o.Rows))
		b.add(AlloyDBSharedBlks, lab("access_type", "hit"), clone(dr), span, int64Value(l.SharedHit-o.SharedHit))
		b.add(AlloyDBSharedBlks, lab("access_type", "miss"), clone(dr), span, int64Value(l.SharedRead-o.SharedRead))
	}
	// Lock time: the backends seen waiting at this sample, times the
	// span: an estimate, where AlloyDB measures it.
	us := span.GetEndTime().AsTime().Sub(span.GetStartTime().AsTime()).Microseconds()
	for _, w := range in.Waits {
		b.add(AlloyDBLockTime, map[string]string{"user": w.User, "client_addr": "", "lock_type": w.LockType},
			alloyDBDatabaseRes(in, w.Database), span, int64Value(w.Sessions*us))
	}
	oldStmt := map[[2]string]int64{}
	for _, s := range prev.Statements {
		oldStmt[[2]string{s.Database, s.Operation}] = s.Calls
	}
	for _, s := range in.Statements {
		n := s.Calls - oldStmt[[2]string{s.Database, s.Operation}]
		if n < 0 {
			continue
		}
		b.add(AlloyDBStatements, map[string]string{"operation_type": s.Operation}, alloyDBDatabaseRes(in, s.Database), span, int64Value(n))
	}
}

// alloyDBDescriptor is one metric as Google publishes it.
type alloyDBDescriptor struct {
	typ, name, desc, unit, resource string
	kind                            metricpb.MetricDescriptor_MetricKind
	vt                              metricpb.MetricDescriptor_ValueType
	labels                          []string
}

const alloyDBSampled = " Sampled every 60 seconds. After sampling, data is not visible for up to 180 seconds."

var alloyDBDescriptors = []alloyDBDescriptor{
	{AlloyDBStorage, "Cluster storage usage", "The total AlloyDB storage in bytes across the entire cluster.", "By", AlloyDBClusterResource, gauge, tInt, nil},
	{AlloyDBLastBackup, "Creation timestamp of the latest backup for the cluster", "Creation timestamp of the latest backup for the cluster.", "us", AlloyDBClusterResource, gauge, tInt, []string{"backup_type"}},
	{AlloyDBCPUAvg, "Mean CPU utilization", "Mean CPU utilization across all currently serving nodes of the instance from 0 to 100.", "10^2.%", AlloyDBInstanceResource, gauge, tDouble, nil},
	{AlloyDBCPUMax, "Maximum CPU utilization", "Maximum CPU utilization across all currently serving nodes of the instance from 0 to 100.", "10^2.%", AlloyDBInstanceResource, gauge, tDouble, nil},
	{AlloyDBVCPUs, "vCPUs allocated per node", "The number of vCPUs allocated to each node in the instance.", "1", AlloyDBInstanceResource, gauge, tInt, nil},
	{AlloyDBMemAvailable, "Minimum available memory", "The minimum available memory across all currently serving nodes of the instance. The available memory is an estimate of memory in bytes available for allocation on the VM including memory that is currently used, but can be potentially freed.", "By", AlloyDBInstanceResource, gauge, tInt, nil},
	{AlloyDBConnections, "Total connections per instance", "The number of active and idle connections to the AlloyDB instance across serving nodes of the instance.", "1", AlloyDBInstanceResource, gauge, tInt, nil},
	{AlloyDBConnLimit, "Limit on connections per node", "The current limit on the number of connections per node of the AlloyDB instance.", "1", AlloyDBInstanceResource, gauge, tInt, nil},
	{AlloyDBBackendsByState, "Number of connections to the instance per connection state", "The current number of connections to the instance grouped by the state: idle, active, idle_in_transaction, idle_in_transaction_aborted, disabled, and fastpath_function_call.", "1", AlloyDBInstanceResource, gauge, tInt, []string{"state"}},
	{AlloyDBBackendsByApp, "Number of connections to the instance per application", "The current number of connections to the AlloyDB instance, grouped by applications for top 500 applications.", "1", AlloyDBInstanceResource, gauge, tInt, []string{"application_name"}},
	{AlloyDBTransactions, "Instance transaction count", "The number of committed and rolled back transactions across all serving nodes of the instance.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBCommits, "Instance commit count", "The number of committed transactions across all serving nodes of the instance.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBAborts, "Instance abort count", "The number of transactions that were rolled back across all serving nodes of the instance.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBNodes, "Number and status of nodes", "The number of nodes in the instance, along with their status, which can be either up or down.", "1", AlloyDBInstanceResource, gauge, tInt, []string{"status"}},
	{AlloyDBReplicationLag, "Maximum replication lag", "The maximum replication time lag calculated across all serving read replicas of the instance. The replication time lag is derived from the replay_lag value.", "ms", AlloyDBInstanceResource, gauge, tInt, []string{"replica_instance_id"}},
	{AlloyDBBlksHit, "Blocks hit", "Number of times Postgres found the requested block in the buffer cache.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBBlksRead, "Blocks miss", "Number of blocks read by Postgres that were not in the buffer cache.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBDeadlocks, "Number of deadlocks", "Number of deadlocks detected in the instance.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBTuplesWritten, "Number of tuples written", "Number of rows written while processing the queries in the instance since the last sample.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBTuplesInserted, "Number of tuples inserted", "Number of rows inserted while processing the queries in the instance since the last sample.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBTuplesUpdated, "Number of tuples updated", "Number of rows updated while processing the queries in the instance since the last sample.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBTuplesDeleted, "Number of tuples deleted", "Number of rows deleted while processing the queries in the instance since the last sample.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBTuplesFetched, "Number of tuples fetched", "Number of rows fetched while processing the queries in the instance since the last sample.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBTuplesReturned, "Number of tuples scanned", "Number of rows scanned while processing the queries in the instance since the last sample.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBTempBytes, "Bytes written to temporary files", "The total amount of data(in bytes) written to temporary files by the queries while performing internal algorithms like join, sort etc.", "By", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBTempFiles, "Temporary files used for writing data", "The number of temporary files used for writing data in the instance while performing internal algorithms like join, sort etc.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBNewConnections, "Number of new connections added to the instance", "The number of new connections added to the instance.", "1", AlloyDBInstanceResource, delta, tInt, nil},
	{AlloyDBOldestXact, "Oldest transaction age", "Current age of the oldest uncommitted transaction. It's measured in the number of transactions that started since the oldest transaction.", "1", AlloyDBInstanceResource, gauge, tInt, []string{"type"}},
	{AlloyDBXIDUtilization, "Transaction ID utilization", "The current percentage of transaction ID space consumed by the AlloyDB instance. It records the number of unvacuumed transactions as a percentage of the 2-billion maximum.", "1", AlloyDBInstanceResource, gauge, tDouble, nil},
	{AlloyDBDBBackends, "Number of connections to the instance per database", "The current number of connections per database to the instance for top 500 dbs.", "1", AlloyDBDatabaseResource, gauge, tInt, nil},
	{AlloyDBDBCommits, "Number of committed transactions per database", "Total number of transactions committed per database for top 500 databases.", "1", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBDBRollbacks, "Number of rolledback transactions per database", "Total number of transactions rolledback per database for top 500 databases.", "1", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBPrefix + "database/postgresql/blks_hit_for_top_databases", "Blocks hit per database", "Number of times Postgres found the requested block in the buffer cache per database for top 500 databases.", "1", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBPrefix + "database/postgresql/blks_read_for_top_databases", "Blocks miss per database", "Number of blocks read by Postgres that were not in the buffer cache per database for top 500 databases.", "1", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBPrefix + "database/postgresql/deadlock_count_for_top_databases", "Number of deadlocks per database", "Total number of deadlocks detected in the instance per database for top 500 dbs.", "1", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBPrefix + "database/postgresql/inserted_tuples_count_for_top_databases", "Number of tuples inserted per database", "The total number of rows inserted per db for top 500 dbs as a result of the queries in the instance.", "1", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBPrefix + "database/postgresql/updated_tuples_count_for_top_databases", "Number of tuples updated per database", "The total number of rows updated per db for top 500 dbs as a result of the queries in the instance.", "1", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBPrefix + "database/postgresql/deleted_tuples_count_for_top_databases", "Number of tuples deleted per database", "The total number of rows deleted per db for top 500 dbs as a result of the queries in the instance.", "1", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBPrefix + "database/postgresql/written_tuples_count_for_top_databases", "Number of tuples written", "The total number of rows written(inserted,updated,deleted) per db for top 500 dbs as a result of the queries in the instance.", "1", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBPrefix + "database/postgresql/fetched_tuples_count_for_top_databases", "Number of tuples fetched per database", "The total number of rows fetched per db for top 500 dbs as a result of the queries in the instance.", "1", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBPrefix + "database/postgresql/returned_tuples_count_for_top_databases", "Number of tuples scanned per database", "The total number of rows scanned per database for top 500 dbs while processing the queries in the instance.", "1", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBPrefix + "database/postgresql/temp_bytes_written_for_top_databases", "Bytes written to temporary files per database", "The total amount of data(in bytes) written to temporary files by the queries per database for top 500 dbs.", "By", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBPrefix + "database/postgresql/temp_files_written_for_top_databases", "Temporary files used for writing data per database", "The number of temporary files used for writing data per database while performing internal algorithms like join, sort etc for top 500 dbs.", "1", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBPrefix + "database/postgresql/new_connections_for_top_databases", "Number of connections added per database", "Total number of new connections added per database for top 500 databases to the instance.", "1", AlloyDBDatabaseResource, delta, tInt, nil},
	{AlloyDBDBTuples, "Number of tuples (rows) in the database.", "Number of tuples (rows) by state per database in the instance. This metric will only be exposed when the number of db's is less than 50.", "1", AlloyDBDatabaseResource, gauge, tInt, []string{"state"}},
	{AlloyDBExecTime, "Execution time", "Accumulated query execution time since the last sample.This is the sum of CPU time, IO wait time, lock wait time, process context switch, and scheduling for all the processes involved in the query execution.", "us{CPU}", AlloyDBDatabaseResource, delta, tInt, []string{"user", "client_addr"}},
	{AlloyDBIOTime, "IO time", "Accumulated IO time since the last sample.", "us", AlloyDBDatabaseResource, delta, tInt, []string{"user", "client_addr", "io_type"}},
	{AlloyDBLockTime, "Lock time", "Accumulated lock wait time since the last sample.", "us", AlloyDBDatabaseResource, delta, tInt, []string{"user", "client_addr", "lock_type"}},
	{AlloyDBRowCount, "Affected rows", "The number of retrieved or affected rows since the last sample.", "1", AlloyDBDatabaseResource, delta, tInt, []string{"user", "client_addr"}},
	{AlloyDBSharedBlks, "Shared blocks cache access", "Shared blocks (regular tables and indexed) accessed by statement execution.", "1", AlloyDBDatabaseResource, delta, tInt, []string{"user", "client_addr", "access_type"}},
	{AlloyDBStatements, "Statements executed count", "Total count of statements executed in the instance per database per operation_type. Only available for instances with Query insights enabled.", "1", AlloyDBDatabaseResource, delta, tInt, []string{"operation_type"}},
}

var alloyDBDescriptorByType = func() map[string]*alloyDBDescriptor {
	m := map[string]*alloyDBDescriptor{}
	for i := range alloyDBDescriptors {
		m[alloyDBDescriptors[i].typ] = &alloyDBDescriptors[i]
	}
	return m
}()

func alloyDBDescriptorOf(t string) *alloyDBDescriptor { return alloyDBDescriptorByType[t] }

func (b *AlloyDBBuiltin) Descriptors(project string) []*metricpb.MetricDescriptor {
	out := make([]*metricpb.MetricDescriptor, 0, len(alloyDBDescriptors))
	for _, d := range alloyDBDescriptors {
		md := &metricpb.MetricDescriptor{Name: descriptorName(project, d.typ), Type: d.typ, MetricKind: d.kind, ValueType: d.vt,
			Unit: d.unit, DisplayName: d.name, Description: d.desc + alloyDBSampled, MonitoredResourceTypes: []string{d.resource}}
		for _, l := range d.labels {
			md.Labels = append(md.Labels, &labelpb.LabelDescriptor{Key: l, ValueType: labelpb.LabelDescriptor_STRING})
		}
		out = append(out, md)
	}
	return out
}

func (b *AlloyDBBuiltin) Series(project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	if alloyDBDescriptorOf(metricType) == nil {
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
		if ts.GetMetric().GetType() != metricType || ts.GetResource().GetLabels()["resource_container"] != project {
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
