package main

// AlloyDB's metrics in Cloud Monitoring (#2251) are read from each
// instance's server: its pod's CPU time and memory from the kubelet's
// /stats/summary (as Cloud SQL's are, monitoringsql.go) and the server's
// own figures through alloydb.Server.InstanceStats. A cluster's storage is
// its primary's databases' size; its last backups are its READY backups'.

import (
	"context"
	"strings"

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
		for name, st := range pods {
			rest, ok := strings.CutPrefix(name, mi.Server+"-")
			if !ok || strings.Count(rest, "-") != 1 {
				continue
			}
			r.HasPod = true
			r.CPUSeconds += st.cpuSeconds
			r.MemoryUsed += st.working
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
				if !r.ReadPool {
					storage[mi.Project+"/"+mi.Location+"/"+mi.Cluster] = size
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
