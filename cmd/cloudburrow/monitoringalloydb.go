package main

// AlloyDB's metrics in Cloud Monitoring (#2251) are read from each
// instance's server: its pod's CPU time and memory from the kubelet's
// /stats/summary (as Cloud SQL's are, monitoringsql.go) and the server's
// own figures through alloydb.Server.InstanceStats, and the collector's
// per-execution readings through alloydb.Server.TelemetryOf (#2253). A
// cluster's storage is what its primary stores: its databases and its WAL
// files; its last backups are its READY backups'. An instance whose node
// is its server (a primary, a read pool of one node) has node/* too, its
// node_id its pod's name.

import (
	"context"
	"strings"
	"unicode/utf8"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"

	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// alloyDBMachineBytes is an AlloyDB machine's memory a vCPU: Google's
// machines have 8 GB a vCPU (2 vCPU, 16 GB and up).
const alloyDBMachineBytes = 8 << 30

// monitoringAlloyDB is monitoring.AlloyDBSource over the instance's
// AlloyDB: project's clusters, or every project's when it is "".
type monitoringAlloyDB struct {
	srv                            func() *alloydb.Server
	kubeconfig, namespace, project string
}

func (m monitoringAlloyDB) AlloyDBSample() ([]monitoring.AlloyDBCluster, []monitoring.AlloyDBInstance) {
	srv := m.srv()
	if srv == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sqlMetricsTimeout)
	defer cancel()
	all := srv.MetricInstances(m.project)
	clusters := srv.MetricClusters(m.project)
	if len(all) == 0 && len(clusters) == 0 {
		return nil, nil
	}
	pods := monitoringSQL{kubeconfig: m.kubeconfig, namespace: m.namespace}.pods(ctx)
	storage := map[string]int64{}
	var out []monitoring.AlloyDBInstance
	for _, mi := range all {
		r := monitoring.AlloyDBInstance{Project: mi.Project, Location: mi.Location, Cluster: mi.Cluster, Instance: mi.Instance,
			ReadPool: mi.Type == alloydbpb.Instance_READ_POOL, VCPUs: int64(mi.CPUs), Nodes: int64(mi.Nodes),
			MemoryMachine: int64(mi.CPUs) * alloyDBMachineBytes}
		// The server's Deployment is its name: its pods are
		// SERVER-<replica set hash>-<id>.
		var podNames []string
		for name, st := range pods {
			rest, ok := strings.CutPrefix(name, mi.Server+"-")
			if !ok || strings.Count(rest, "-") != 1 {
				continue
			}
			r.HasPod = true
			r.CPUSeconds += st.cpuSeconds
			r.MemoryUsed += st.working
			r.RxBytes += int64(st.rx)
			r.TxBytes += int64(st.tx)
			podNames = append(podNames, name)
		}
		if len(podNames) == 1 && mi.Nodes == 1 {
			// One pod, the instance's one node.
			r.NodeID, r.HasNet = podNames[0], true
		}
		if mi.Serving {
			if st, err := srv.InstanceStats(ctx, mi.Name); err == nil {
				r.Up, r.HasServer = true, true
				r.MaxConnections, r.ByState, r.ByApplication = st.MaxConnections, st.ByState, st.ByApplication
				r.XIDUtilization, r.OldestAge, r.Replica, r.ReplayLagMs = st.XIDUtilization, st.OldestAge, st.Replica, st.ReplayLagMs
				var size int64
				for _, d := range st.Databases {
					size += d.SizeBytes
					r.Databases = append(r.Databases, monitoring.AlloyDBDatabase{Name: d.Name, Backends: d.Backends, Commits: d.Commits,
						Rollbacks: d.Rollbacks, BlksHit: d.BlksHit, BlksRead: d.BlksRead, Returned: d.Returned, Fetched: d.Fetched,
						Inserted: d.Inserted, Updated: d.Updated, Deleted: d.Deleted, TempFiles: d.TempFiles, TempBytes: d.TempBytes,
						Deadlocks: d.Deadlocks, Sessions: d.Sessions, Live: d.Live, Dead: d.Dead, HasTuples: d.HasTuples})
				}
				if !r.ReadPool && st.HasWAL {
					storage[mi.Project+"/"+mi.Location+"/"+mi.Cluster] = size + st.WALBytes
				}
				for _, q := range st.Queries {
					r.Queries = append(r.Queries, monitoring.AlloyDBQuery{QueryHash: alloydb.QueryHash(q.QueryID),
						QueryString: truncateQuery(q.Query, queryLength(mi)), User: q.User, Database: q.Database, ExecUs: q.ExecUs,
						IOReadUs: q.IOReadUs, IOWriteUs: q.IOWriteUs, Rows: q.Rows, SharedHit: q.SharedHit, SharedRead: q.SharedRead})
				}
				r.HasLoad = st.HasLoad
				for _, l := range st.Load {
					r.Load = append(r.Load, monitoring.AlloyDBLoad{User: l.User, Database: l.Database, ExecUs: l.ExecUs,
						IOReadUs: l.IOReadUs, IOWriteUs: l.IOWriteUs, Rows: l.Rows, SharedHit: l.SharedHit, SharedRead: l.SharedRead})
				}
				for _, s := range st.Statements {
					r.Statements = append(r.Statements, monitoring.AlloyDBStatementCount{Database: s.Database, Operation: s.Operation, Calls: s.Calls})
				}
				for _, w := range st.Waits {
					r.Waits = append(r.Waits, monitoring.AlloyDBWait{User: w.User, Database: w.Database, LockType: w.LockType, Sessions: w.Sessions})
				}
			}
		}
		if mi.Serving {
			alloyDBTelemetryInto(&r, srv.TelemetryOf(mi.Name))
		}
		out = append(out, r)
	}
	var cs []monitoring.AlloyDBCluster
	for _, c := range clusters {
		mc := monitoring.AlloyDBCluster{Project: c.Project, Location: c.Location, Cluster: c.Cluster, LastBackups: c.LastBackups}
		mc.StorageBytes, mc.HasStorage = storage[c.Project+"/"+c.Location+"/"+c.Cluster]
		cs = append(cs, mc)
	}
	return cs, out
}

// queryLength is the instance's query_string_length (1024 by default).
func queryLength(mi alloydb.MetricInstance) int {
	if n := int(mi.Insights.GetQueryStringLength()); n > 0 {
		return n
	}
	return 1024
}

// truncateQuery is q's first n bytes, as Query insights keeps them,
// without splitting a character.
func truncateQuery(q string, n int) string {
	if len(q) <= n {
		return q
	}
	for n > 0 && !utf8.RuneStart(q[n]) {
		n--
	}
	return q[:n]
}

// alloyDBTelemetryInto copies the collector's readings into r.
func alloyDBTelemetryInto(r *monitoring.AlloyDBInstance, t alloydb.Telemetry) {
	if !t.Reading {
		return
	}
	r.HasTelemetry = true
	tags := func(h string) monitoring.AlloyDBTags {
		v := t.TagValues[h]
		return monitoring.AlloyDBTags{Action: v.Action, Application: v.Application, Controller: v.Controller, DBDriver: v.DBDriver,
			Framework: v.Framework, Route: v.Route}
	}
	hist := func(h alloydb.Histogram) monitoring.AlloyDBHistogram {
		return monitoring.AlloyDBHistogram{Count: h.Count, Sum: h.Sum, SumSq: h.SumSq, Buckets: h.Buckets}
	}
	query := func(id int64) (string, string) {
		if id == 0 {
			return "", ""
		}
		return alloydb.QueryHash(id), truncateQuery(t.Queries[id], t.QueryStringLength)
	}
	for _, group := range [][]alloydb.LatencyStat{t.Aggregate, t.PerQuery, t.PerTag} {
		for _, l := range group {
			qh, qs := query(l.QueryID)
			r.Latencies = append(r.Latencies, monitoring.AlloyDBLatency{Database: l.Database, User: l.User, Client: l.Client,
				QueryHash: qh, QueryString: qs, TagHash: l.TagHash, Tags: tags(l.TagHash), Hist: hist(l.Hist)})
		}
	}
	for _, l := range t.Locks {
		qh, qs := query(l.QueryID)
		r.Locks = append(r.Locks, monitoring.AlloyDBLockWait{Database: l.Database, User: l.User, Client: l.Client, LockType: l.LockType,
			QueryHash: qh, QueryString: qs, TagHash: l.TagHash, Tags: tags(l.TagHash), Us: l.Us})
	}
	for _, w := range t.Waits {
		r.WaitEvents = append(r.WaitEvents, monitoring.AlloyDBWaitEvent{Type: w.Type, Name: w.Name, Count: w.Count, Us: w.Us})
	}
}

// The latency distributions' buckets are the collector's.
func init() { monitoring.AlloyDBLatencyBuckets = alloydb.LatencyBuckets }
