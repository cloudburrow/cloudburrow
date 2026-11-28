package monitoring

// AlloyDB's per-execution metrics (#2253): the insights latency
// distributions (aggregate, per query, per tag), the per-query and per-tag
// execution, IO, row, block and lock figures, the instance's wait events,
// and node/* on the alloydb.googleapis.com/InstanceNode and NodeDatabase
// resources, for the instances whose nodes are each a server (a primary,
// a read pool of one node): one node, its node_id its server's pod.
// Their readings come from internal/service/alloydb/telemetry.go (the
// server's log, auto_explain, pg_stat_activity sampled every second) and
// pg_stat_statements per query.

import (
	"strings"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/distribution"
	"google.golang.org/genproto/googleapis/api/monitoredres"
)

// The metric types the console reads.
const (
	AlloyDBLatencies       = AlloyDBPrefix + "database/postgresql/insights/aggregate/latencies"
	AlloyDBQueryLatencies  = AlloyDBPrefix + "database/postgresql/insights/perquery/latencies"
	AlloyDBQueryExecTime   = AlloyDBPrefix + "database/postgresql/insights/perquery/execution_time"
	AlloyDBQueryIOTime     = AlloyDBPrefix + "database/postgresql/insights/perquery/io_time"
	AlloyDBQueryLockTime   = AlloyDBPrefix + "database/postgresql/insights/perquery/lock_time"
	AlloyDBQueryRowCount   = AlloyDBPrefix + "database/postgresql/insights/perquery/row_count"
	AlloyDBQuerySharedBlks = AlloyDBPrefix + "database/postgresql/insights/perquery/shared_blk_access_count"
	AlloyDBTagLatencies    = AlloyDBPrefix + "database/postgresql/insights/pertag/latencies"
	AlloyDBTagExecTime     = AlloyDBPrefix + "database/postgresql/insights/pertag/execution_time"
	AlloyDBTagLockTime     = AlloyDBPrefix + "database/postgresql/insights/pertag/lock_time"
	AlloyDBWaitCount       = AlloyDBPrefix + "instance/postgresql/wait_count"
	AlloyDBWaitTime        = AlloyDBPrefix + "instance/postgresql/wait_time"
	AlloyDBNodeCPU         = AlloyDBPrefix + "node/cpu/usage_time"
	AlloyDBNodeMemory      = AlloyDBPrefix + "node/memory/available_memory"
	AlloyDBNodeUptime      = AlloyDBPrefix + "node/postgres/uptime"
	AlloyDBNodeBackends    = AlloyDBPrefix + "node/postgres/backends"
	AlloyDBNodeReceived    = AlloyDBPrefix + "node/network/received_bytes_count"
	AlloyDBNodeSent        = AlloyDBPrefix + "node/network/sent_bytes_count"
	AlloyDBNodeReplayLag   = AlloyDBPrefix + "node/postgres/replay_lag"
	AlloyDBNodeResource    = "alloydb.googleapis.com/InstanceNode"
	AlloyDBNodeDBResource  = "alloydb.googleapis.com/NodeDatabase"
	alloyDBTagLabels       = "action,application,controller,db_driver,framework,route,tag_hash"
)

// AlloyDBLatencyBuckets are the latency distributions' bucket options,
// the alloydb package's LatencyBuckets (cmd sets them).
var AlloyDBLatencyBuckets = struct {
	Count         int
	Growth, Scale float64
}{64, 1.4, 1}

// AlloyDBHistogram is a cumulative latency distribution in microseconds
// over AlloyDBLatencyBuckets (Count+2 bucket counts).
type AlloyDBHistogram struct {
	Count      int64
	Sum, SumSq float64
	Buckets    []int64
}

// AlloyDBTags are one application tag set.
type AlloyDBTags struct {
	Action, Application, Controller, DBDriver, Framework, Route string
}

// AlloyDBLatency is one key's cumulative latency distribution: in
// aggregate, per query (QueryHash) or per tag (TagHash).
type AlloyDBLatency struct {
	Database, User, Client string
	QueryHash, QueryString string
	TagHash                string
	Tags                   AlloyDBTags
	Hist                   AlloyDBHistogram
}

// AlloyDBLockWait is one key's cumulative lock wait, in microseconds.
type AlloyDBLockWait struct {
	Database, User, Client, LockType string
	QueryHash, QueryString           string
	TagHash                          string
	Tags                             AlloyDBTags
	Us                               int64
}

// AlloyDBWaitEvent is one wait event's cumulative waits and time.
type AlloyDBWaitEvent struct {
	Type, Name string
	Count      int64
	Us         float64
}

// AlloyDBQuery is one normalized query's pg_stat_statements totals for
// one user in one database; times in microseconds.
type AlloyDBQuery struct {
	QueryHash, QueryString, User, Database string
	ExecUs, IOReadUs, IOWriteUs, Rows      int64
	SharedHit, SharedRead                  int64
}

func init() {
	dist := metricDist
	lab := func(base string, extra ...string) []string {
		out := strings.Split(base, ",")
		return append(out, extra...)
	}
	q := "querystring,user,client_addr,query_hash"
	t := "user,client_addr," + alloyDBTagLabels
	add := []alloyDBDescriptor{
		{AlloyDBLatencies, "Latency", "Query latency distribution.", "us", AlloyDBDatabaseResource, delta, dist, []string{"user", "client_addr"}},
		{AlloyDBQueryLatencies, "Latency per query", "Query latency distribution.", "us", AlloyDBDatabaseResource, delta, dist, lab(q)},
		{AlloyDBQueryExecTime, "Execution time per query", "Accumulated execution times per user per database per query.This is the sum of CPU time, IO wait time, lock wait time, process context switch, and scheduling for all the processes involved in the query execution.", "us{CPU}", AlloyDBDatabaseResource, delta, tInt, lab(q)},
		{AlloyDBQueryIOTime, "IO time per query", "Accumulated IO time since the last sample.", "us", AlloyDBDatabaseResource, delta, tInt, lab(q, "io_type")},
		{AlloyDBQueryLockTime, "Lock time per query", "Accumulated lock wait time since the last sample.", "us", AlloyDBDatabaseResource, delta, tInt, lab(q, "lock_type")},
		{AlloyDBQueryRowCount, "Affected rows per query", "The number of retrieved or affected rows since the last sample.", "1", AlloyDBDatabaseResource, delta, tInt, lab(q)},
		{AlloyDBQuerySharedBlks, "Shared blocks cache access per query", "Shared blocks (regular tables and indexed) accesed by statement execution.", "1", AlloyDBDatabaseResource, delta, tInt, lab(q, "access_type")},
		{AlloyDBTagLatencies, "Latency per tag", "Query latency distribution.", "us", AlloyDBDatabaseResource, delta, dist, lab(t)},
		{AlloyDBTagExecTime, "Execution time per tag", "Accumulated execution times since the last sample.This is the sum of CPU time, IO wait time, lock wait time, process context switch, and scheduling for all the processes involved in the query execution.", "us{CPU}", AlloyDBDatabaseResource, delta, tInt, lab(t)},
		{AlloyDBTagLockTime, "Lock time per tag", "Accumulated lock wait time since the last sample.", "us", AlloyDBDatabaseResource, delta, tInt, lab(t, "lock_type")},
		{AlloyDBWaitCount, "Wait count", "Total number of times processes waited for each wait event in the instance.", "1", AlloyDBInstanceResource, delta, tInt, []string{"wait_event_type", "wait_event_name"}},
		{AlloyDBWaitTime, "Wait time", "Total elapsed wait time for each wait event in the instance.", "us", AlloyDBInstanceResource, delta, tDouble, []string{"wait_event_type", "wait_event_name"}},
		// node/*: one node of an instance whose nodes are servers.
		{AlloyDBNodeCPU, "Node CPU usage", "Rate of CPU utilization in the node.", "10^2.%", AlloyDBNodeResource, gauge, tDouble, nil},
		{AlloyDBNodeMemory, "Available memory", "An estimate of memory in bytes available for allocation on the VM including memory that is currently used, but can be potentially freed.", "By", AlloyDBNodeResource, gauge, tInt, nil},
		{AlloyDBNodeReceived, "Received bytes", "Received bytes over the network, since the last sample.", "By", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBNodeSent, "Sent bytes", "Sent bytes over the network, since the last sample.", "By", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBNodeBackends, "Total connections per node", "The total number of connections across all databases for this node.", "1", AlloyDBNodeResource, gauge, tInt, nil},
		{AlloyDBPrefix + "node/postgres/backends_by_state", "Number of connections to the node per connection state", "The current number of connections to the node grouped by the state: idle, active, idle_in_transaction, idle_in_transaction_aborted, disabled, and fastpath_function_call.", "1", AlloyDBNodeResource, gauge, tInt, []string{"state"}},
		{AlloyDBPrefix + "node/postgres/backends_for_top_applications", "Number of connections to the node per application", "The current number of connections to the AlloyDB node, grouped by applications for top 500 applications.", "1", AlloyDBNodeResource, gauge, tInt, []string{"application_name"}},
		{AlloyDBPrefix + "node/postgres/blks_hit", "Blocks hit", "Number of times Postgres found the requested block in the buffer cache.", "1", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/postgres/blks_read", "Blocks miss", "Number of blocks read by Postgres that were not in the buffer cache.", "1", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/postgres/deadlock_count", "Number of deadlocks", "Number of deadlocks detected in the node.", "1", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/postgres/deleted_tuples_count", "Number of tuples deleted", "Number of rows deleted while processing the queries in the node since the last sample.", "1", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/postgres/fetched_tuples_count", "Number of tuples fetched", "Number of rows fetched while processing the queries in the node since the last sample.", "1", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/postgres/inserted_tuples_count", "Number of tuples inserted", "Number of rows inserted while processing the queries in the node since the last sample.", "1", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/postgres/new_connections_count", "Number of new connections added to the node", "The number of new connections added to the node.", "1", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBNodeReplayLag, "Node replication lag", "The lag in milliseconds for the individual node, comes from replay_lag in pg_stat_replication.", "ms", AlloyDBNodeResource, gauge, tInt, nil},
		{AlloyDBPrefix + "node/postgres/returned_tuples_count", "Number of tuples scanned", "Number of rows scanned while processing the queries in the node since the last sample.", "1", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/postgres/temp_bytes_written_count", "Bytes written to temporary files", "The total amount of data (in bytes) written to temporary files by the queries while performing internal algorithms like join, sort etc.", "By", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/postgres/temp_files_written_count", "Temporary files used for writing data", "The number of temporary files used for writing data in the node while performing internal algorithms like join, sort etc.", "1", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/postgres/transaction_count", "Node transaction count", "The number of committed and rolled back transactions in the node since the last sample.", "1", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/postgres/updated_tuples_count", "Number of tuples updated", "Number of rows updated while processing the queries in the node since the last sample.", "1", AlloyDBNodeResource, delta, tInt, nil},
		{AlloyDBNodeUptime, "Node database availability", "Rate of database availability in the node.", "1", AlloyDBNodeResource, gauge, tDouble, nil},
		{AlloyDBPrefix + "node/postgres/wait_count", "Node Wait count", "Total number of times processes waited for each wait event in the node.", "1", AlloyDBNodeResource, delta, tInt, []string{"wait_event_type", "wait_event_name"}},
		{AlloyDBPrefix + "node/postgres/wait_time", "Node Wait time", "Total elapsed wait time for each wait event in the node.", "us", AlloyDBNodeResource, delta, tDouble, []string{"wait_event_type", "wait_event_name"}},
		{AlloyDBPrefix + "node/database/postgresql/backends_for_top_databases", "Number of connections to the node for each database", "The current number of connections to the node for each database in the top 500 databases.", "1", AlloyDBNodeDBResource, gauge, tInt, nil},
		{AlloyDBPrefix + "node/database/postgresql/blks_hit_for_top_databases", "Blocks hit for each database", "The number of times Postgres found the requested block in the buffer cache for each database in the top 500 databases.", "1", AlloyDBNodeDBResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/database/postgresql/blks_read_for_top_databases", "Blocks miss for each database", "The number of blocks read by Postgres that were not in the buffer cache for each database in the top 500 databases.", "1", AlloyDBNodeDBResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/database/postgresql/committed_transactions_for_top_databases", "Number of committed transactions for each database", "The total number of transactions committed for each database in the top 500 databases.", "1", AlloyDBNodeDBResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/database/postgresql/deadlock_count_for_top_databases", "Number of deadlocks for each database", "The total number of deadlocks detected in the node for each database in the top 500 databases.", "1", AlloyDBNodeDBResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/database/postgresql/fetched_tuples_count_for_top_databases", "Number of rows fetched for each database", "The total number of rows fetched for database in the top 500 databases as a result of the queries in the node.", "1", AlloyDBNodeDBResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/database/postgresql/insights/aggregate/execution_time", "Execution time", "The accumulated query execution time for each database since the last sample. This is the sum of CPU time, IO wait time, lock wait time, process context switch, and scheduling for all the processes involved in the query execution.", "us{CPU}", AlloyDBNodeDBResource, delta, tInt, []string{"user", "client_addr"}},
		{AlloyDBPrefix + "node/database/postgresql/insights/aggregate/io_time", "IO time", "The accumulated IO time since the last sample for each database in the top 500 databases.", "us", AlloyDBNodeDBResource, delta, tInt, []string{"user", "client_addr", "io_type"}},
		{AlloyDBPrefix + "node/database/postgresql/insights/aggregate/latencies", "Latency", "Query latency distribution.", "us", AlloyDBNodeDBResource, delta, dist, []string{"user", "client_addr"}},
		{AlloyDBPrefix + "node/database/postgresql/new_connections_for_top_databases", "Number of connections added for each database", "The total number of new connections added for each database in the top 500 databases.", "1", AlloyDBNodeDBResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/database/postgresql/returned_tuples_count_for_top_databases", "Number of rows scanned for each database", "The total number of rows scanned for each database in the top 500 databases while processing the queries in the node.", "1", AlloyDBNodeDBResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/database/postgresql/rolledback_transactions_for_top_databases", "Number of rolled back transactions for each database", "The total number of transactions rolled back for each database in the top 500 databases.", "1", AlloyDBNodeDBResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/database/postgresql/temp_bytes_written_for_top_databases", "Bytes written to temporary files for each database", "The total amount of data (in bytes) written to temporary files by the queries for each database in the top 500 databases.", "By", AlloyDBNodeDBResource, delta, tInt, nil},
		{AlloyDBPrefix + "node/database/postgresql/temp_files_written_for_top_databases", "Temporary files used for writing data for each database", "The number of temporary files used for writing data for each database in the top 500 databases while performing internal algorithms such as join, sort, and so on.", "1", AlloyDBNodeDBResource, delta, tInt, nil},
	}
	alloyDBDescriptors = append(alloyDBDescriptors, add...)
	for i := range alloyDBDescriptors {
		alloyDBDescriptorByType[alloyDBDescriptors[i].typ] = &alloyDBDescriptors[i]
	}
}

// metricDist is the DISTRIBUTION value type.
var metricDist = tDist

// alloyDBNodeMetric is the node/* metric an instance metric is mirrored
// to for an instance whose node is its server ("" for none).
func alloyDBNodeMetric(metric string) string {
	rest, ok := strings.CutPrefix(metric, AlloyDBPrefix)
	if !ok {
		return ""
	}
	var n string
	switch rest {
	case "instance/cpu/maximum_utilization":
		n = "node/cpu/usage_time"
	case "instance/memory/min_available_memory":
		n = "node/memory/available_memory"
	case "instance/postgres/total_connections":
		n = "node/postgres/backends"
	default:
		switch {
		case strings.HasPrefix(rest, "instance/postgresql/"):
			n = "node/postgres/" + strings.TrimPrefix(rest, "instance/postgresql/")
		case strings.HasPrefix(rest, "instance/postgres/"):
			n = "node/postgres/" + strings.TrimPrefix(rest, "instance/postgres/")
		case strings.HasPrefix(rest, "database/postgresql/"):
			n = "node/" + rest
		}
	}
	if n == "" || alloyDBDescriptorOf(AlloyDBPrefix+n) == nil || n == "node/postgres/replay_lag" {
		return ""
	}
	return AlloyDBPrefix + n
}

// alloyDBNodeRes is res, an Instance or Database resource, as its node's.
func alloyDBNodeRes(res *monitoredres.MonitoredResource, node string) *monitoredres.MonitoredResource {
	r := clone(res)
	switch r.Type {
	case AlloyDBInstanceResource:
		r.Type = AlloyDBNodeResource
	case AlloyDBDatabaseResource:
		r.Type = AlloyDBNodeDBResource
	default:
		return nil
	}
	r.Labels["node_id"] = node
	return r
}

// histDelta is the distribution of the executions between prev and cur
// (nil when none or the counts went down).
func histDelta(cur, prev AlloyDBHistogram) *distribution.Distribution {
	n := cur.Count - prev.Count
	if n <= 0 || len(cur.Buckets) == 0 {
		return nil
	}
	counts := make([]int64, len(cur.Buckets))
	for i := range cur.Buckets {
		c := cur.Buckets[i]
		if i < len(prev.Buckets) {
			c -= prev.Buckets[i]
		}
		if c < 0 {
			return nil
		}
		counts[i] = c
	}
	sum, sq := cur.Sum-prev.Sum, cur.SumSq-prev.SumSq
	mean := sum / float64(n)
	ssd := sq - sum*mean
	if ssd < 0 {
		ssd = 0
	}
	return &distribution.Distribution{Count: n, Mean: mean, SumOfSquaredDeviation: ssd,
		BucketOptions: &distribution.Distribution_BucketOptions{Options: &distribution.Distribution_BucketOptions_ExponentialBuckets{
			ExponentialBuckets: &distribution.Distribution_BucketOptions_Exponential{NumFiniteBuckets: int32(AlloyDBLatencyBuckets.Count),
				GrowthFactor: AlloyDBLatencyBuckets.Growth, Scale: AlloyDBLatencyBuckets.Scale}}},
		BucketCounts: counts}
}

func tagLabels(m map[string]string, t AlloyDBTags, hash string) map[string]string {
	m["action"], m["application"], m["controller"] = t.Action, t.Application, t.Controller
	m["db_driver"], m["framework"], m["route"], m["tag_hash"] = t.DBDriver, t.Framework, t.Route, hash
	return m
}

// telemetryLocked records in's per-execution DELTAs over span against
// prev's cumulative readings: the latency distributions, the per-tag
// execution and lock time, per-query lock time, and the wait events. The
// caller holds mu.
func (b *AlloyDBBuiltin) telemetryLocked(in, prev AlloyDBInstance, res *monitoredres.MonitoredResource, span *monitoringpb.TimeInterval) {
	if !in.HasTelemetry || !prev.HasTelemetry {
		return
	}
	key := func(l AlloyDBLatency) string {
		return l.Database + "\x00" + l.User + "\x00" + l.Client + "\x00" + l.QueryHash + "\x00" + l.TagHash
	}
	old := map[string]AlloyDBHistogram{}
	for _, l := range prev.Latencies {
		old[key(l)] = l.Hist
	}
	for _, l := range in.Latencies {
		d := histDelta(l.Hist, old[key(l)])
		if d == nil {
			continue
		}
		dr := alloyDBDatabaseRes(in, l.Database)
		labels := map[string]string{"user": l.User, "client_addr": l.Client}
		switch {
		case l.QueryHash != "":
			labels["querystring"], labels["query_hash"] = l.QueryString, l.QueryHash
			b.add(AlloyDBQueryLatencies, labels, dr, span, distValue(d))
		case l.TagHash != "":
			b.add(AlloyDBTagLatencies, tagLabels(labels, l.Tags, l.TagHash), dr, span, distValue(d))
			exec := map[string]string{"user": l.User, "client_addr": l.Client}
			b.add(AlloyDBTagExecTime, tagLabels(exec, l.Tags, l.TagHash), clone(dr), span, int64Value(int64(d.Count)*int64(d.Mean+0.5)))
		default:
			b.add(AlloyDBLatencies, labels, dr, span, distValue(d))
		}
	}
	lkey := func(l AlloyDBLockWait) string {
		return l.Database + "\x00" + l.User + "\x00" + l.Client + "\x00" + l.LockType + "\x00" + l.QueryHash + "\x00" + l.TagHash
	}
	oldLock := map[string]int64{}
	for _, l := range prev.Locks {
		oldLock[lkey(l)] = l.Us
	}
	type aggKey struct{ db, user, client, lock string }
	agg := map[aggKey]int64{}
	tags := map[string]AlloyDBLockWait{}
	tagUs := map[string]int64{}
	queries := map[string]AlloyDBLockWait{}
	queryUs := map[string]int64{}
	for _, l := range in.Locks {
		n := l.Us - oldLock[lkey(l)]
		if n <= 0 {
			continue
		}
		agg[aggKey{l.Database, l.User, l.Client, l.LockType}] += n
		if l.QueryHash != "" {
			k := l.Database + "\x00" + l.User + "\x00" + l.Client + "\x00" + l.LockType + "\x00" + l.QueryHash
			queries[k], queryUs[k] = l, queryUs[k]+n
		}
		if l.TagHash != "" {
			k := l.Database + "\x00" + l.User + "\x00" + l.Client + "\x00" + l.LockType + "\x00" + l.TagHash
			tags[k], tagUs[k] = l, tagUs[k]+n
		}
	}
	for k, n := range agg {
		b.add(AlloyDBLockTime, map[string]string{"user": k.user, "client_addr": k.client, "lock_type": k.lock},
			alloyDBDatabaseRes(in, k.db), span, int64Value(n))
	}
	for k, l := range queries {
		b.add(AlloyDBQueryLockTime, map[string]string{"user": l.User, "client_addr": l.Client, "lock_type": l.LockType,
			"querystring": l.QueryString, "query_hash": l.QueryHash}, alloyDBDatabaseRes(in, l.Database), span, int64Value(queryUs[k]))
	}
	for k, l := range tags {
		b.add(AlloyDBTagLockTime, tagLabels(map[string]string{"user": l.User, "client_addr": l.Client, "lock_type": l.LockType}, l.Tags, l.TagHash),
			alloyDBDatabaseRes(in, l.Database), span, int64Value(tagUs[k]))
	}
	oldWait := map[[2]string]AlloyDBWaitEvent{}
	for _, w := range prev.WaitEvents {
		oldWait[[2]string{w.Type, w.Name}] = w
	}
	for _, w := range in.WaitEvents {
		o := oldWait[[2]string{w.Type, w.Name}]
		if w.Count < o.Count || w.Us < o.Us || w.Us == o.Us {
			continue
		}
		lab := map[string]string{"wait_event_type": w.Type, "wait_event_name": w.Name}
		b.add(AlloyDBWaitCount, lab, clone(res), span, int64Value(w.Count-o.Count))
		b.add(AlloyDBWaitTime, map[string]string{"wait_event_type": w.Type, "wait_event_name": w.Name}, clone(res), span, doubleValue(w.Us-o.Us))
	}
}

// queriesLocked records in's per-query pg_stat_statements DELTAs over
// span. pg_stat_statements does not record the client, so client_addr is
// "". The caller holds mu.
func (b *AlloyDBBuiltin) queriesLocked(in, prev AlloyDBInstance, span *monitoringpb.TimeInterval) {
	key := func(q AlloyDBQuery) string { return q.Database + "\x00" + q.User + "\x00" + q.QueryHash }
	old := map[string]AlloyDBQuery{}
	for _, q := range prev.Queries {
		old[key(q)] = q
	}
	for _, q := range in.Queries {
		o, ok := old[key(q)]
		if !ok {
			// Not among the previous sample's top queries: no baseline.
			continue
		}
		if q.ExecUs < o.ExecUs || q.Rows < o.Rows || q.IOReadUs < o.IOReadUs || q.IOWriteUs < o.IOWriteUs ||
			q.SharedHit < o.SharedHit || q.SharedRead < o.SharedRead {
			continue
		}
		dr := alloyDBDatabaseRes(in, q.Database)
		lab := func(extra ...string) map[string]string {
			m := map[string]string{"user": q.User, "client_addr": "", "querystring": q.QueryString, "query_hash": q.QueryHash}
			for i := 0; i+1 < len(extra); i += 2 {
				m[extra[i]] = extra[i+1]
			}
			return m
		}
		b.add(AlloyDBQueryExecTime, lab(), clone(dr), span, int64Value(q.ExecUs-o.ExecUs))
		b.add(AlloyDBQueryIOTime, lab("io_type", "read"), clone(dr), span, int64Value(q.IOReadUs-o.IOReadUs))
		b.add(AlloyDBQueryIOTime, lab("io_type", "write"), clone(dr), span, int64Value(q.IOWriteUs-o.IOWriteUs))
		b.add(AlloyDBQueryRowCount, lab(), clone(dr), span, int64Value(q.Rows-o.Rows))
		b.add(AlloyDBQuerySharedBlks, lab("access_type", "hit"), clone(dr), span, int64Value(q.SharedHit-o.SharedHit))
		b.add(AlloyDBQuerySharedBlks, lab("access_type", "miss"), clone(dr), span, int64Value(q.SharedRead-o.SharedRead))
	}
}
