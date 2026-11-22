package main

// Cloud SQL's metrics in Cloud Monitoring (#2141) are read from each
// instance's server: its pod's CPU time, memory (the working set, and the
// usage with the page cache), network counters and data volume from the
// kubelet's /stats/summary (through the API server's node proxy, as the
// console's cluster panel reads it), and the server's own figures through
// sqladmin's System insights (connections, by database on PostgreSQL;
// MySQL's statements; PostgreSQL's transactions). The tier gives the
// reserved cores and the memory quota, settings.dataDiskSizeGb the disk
// quota.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// sqlMetricsTimeout bounds one sample's reads.
const sqlMetricsTimeout = 20 * time.Second

// monitoringSQL is monitoring.SQLSource over the instance's Cloud SQL.
type monitoringSQL struct {
	srv                            func() *sqladmin.Server
	kubeconfig, namespace, project string
}

// sqlPodStats are one pod's kubelet readings.
type sqlPodStats struct {
	cpuSeconds         float64
	working, usage     int64
	rx, tx             uint64
	volumeUsed         int64
	hasVolume, running bool
}

// pods reads every node's summary, the namespace's pods by name.
func (m monitoringSQL) pods(ctx context.Context) map[string]sqlPodStats {
	out := map[string]sqlPodStats{}
	raw, err := kubectlJSON(ctx, m.kubeconfig, "", "nodes")
	if err != nil {
		return out
	}
	var nodes struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &nodes) != nil {
		return out
	}
	for _, n := range nodes.Items {
		sum, err := nodeUsage(ctx, m.kubeconfig, n.Metadata.Name)
		if err != nil {
			continue
		}
		for _, p := range sum.Pods {
			if p.PodRef.Namespace != m.namespace {
				continue
			}
			st := sqlPodStats{cpuSeconds: float64(p.CPU.UsageCoreNanoSeconds) / 1e9, working: p.Memory.WorkingSetBytes,
				usage: p.Memory.UsageBytes, rx: p.Network.RxBytes, tx: p.Network.TxBytes, running: true}
			for _, v := range p.Volume {
				if v.PVCRef != nil {
					st.volumeUsed += v.UsedBytes
					st.hasVolume = true
				}
			}
			out[p.PodRef.Name] = st
		}
	}
	return out
}

// sqlDeployment is the Deployment an instance's server runs as.
func sqlDeployment(in sqladmin.MetricInstance) string {
	if in.Server == "" {
		// `up`'s own server is its service's.
		if strings.HasPrefix(in.DatabaseVersion, "MYSQL_") {
			return string(config.ServiceCloudSQLMySQL)
		}
		return string(config.ServiceCloudSQL)
	}
	return components.SQLInstancePrefix + in.Server
}

// sqlEngineOf is the engine monitoring.SQLInstance names.
func sqlEngineOf(version string) string {
	switch {
	case strings.HasPrefix(version, "MYSQL_"):
		return "MYSQL"
	case strings.HasPrefix(version, "SQLSERVER_"):
		return "SQLSERVER"
	}
	return "POSTGRES"
}

func (m monitoringSQL) SQLInstances() []monitoring.SQLInstance {
	srv := m.srv()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sqlMetricsTimeout)
	defer cancel()
	all := srv.MetricInstances(m.project)
	if len(all) == 0 {
		return nil
	}
	pods := m.pods(ctx)
	var out []monitoring.SQLInstance
	for _, in := range all {
		r := monitoring.SQLInstance{Project: in.Project, Instance: in.Name, Region: in.Region, Engine: sqlEngineOf(in.DatabaseVersion),
			ReservedCores: in.Cores, MemoryQuota: in.RAM, DiskQuota: in.DiskBytes}
		// A Deployment's pods are DEPLOYMENT-<replica set hash>-<id>; a
		// REGIONAL instance's standby is another Deployment, -b, and not
		// the serving server.
		prefix := sqlDeployment(in) + "-"
		for name, st := range pods {
			rest, ok := strings.CutPrefix(name, prefix)
			if !ok || strings.Count(rest, "-") != 1 {
				continue
			}
			r.HasPod = true
			r.CPUSeconds += st.cpuSeconds
			r.MemoryUsage += st.working
			r.MemoryTotal += st.usage
			r.RxBytes += st.rx
			r.TxBytes += st.tx
			if st.hasVolume {
				r.DiskUsed += st.volumeUsed
			}
		}
		if in.State == "RUNNABLE" && !in.Stopped {
			if ss, err := srv.SystemInsights(ctx, in.Project, in.Name); err == nil {
				r.Up, r.HasServer = true, true
				r.Connections, r.Backends, r.Queries = ss.Connections, ss.Backends, ss.Queries
				r.Commits, r.Rollbacks = ss.Commits, ss.Rollbacks
				if r.DiskUsed == 0 {
					r.DiskUsed = ss.DataBytes
				}
			} else if r.HasPod {
				// SQL Server keeps no System insights here: its pod
				// running is its server up.
				r.Up = r.Engine == "SQLSERVER"
			}
		}
		out = append(out, r)
	}
	return out
}
