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
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/mysqlpool"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// sqlMetricsTimeout bounds one sample's reads.
const sqlMetricsTimeout = 20 * time.Second

// monitoringSQL is monitoring.SQLSource over the instance's Cloud SQL.
type monitoringSQL struct {
	srv                            func() *sqladmin.Server
	kubeconfig, namespace, project string
	// logs counts the PostgreSQL poolers' sign-ins and errors (#2163).
	logs *sqlPoolerLogs
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
		pod := ""
		for name, st := range pods {
			rest, ok := strings.CutPrefix(name, prefix)
			if !ok || strings.Count(rest, "-") != 1 {
				continue
			}
			pod = name
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
				for _, l := range ss.Load {
					r.Load = append(r.Load, monitoring.SQLLoad{User: l.User, Database: l.Database, ExecUs: l.ExecUs, IOReadUs: l.IOReadUs, IOWriteUs: l.IOWriteUs})
				}
				for _, w := range ss.Waits {
					r.Waits = append(r.Waits, monitoring.SQLWait{User: w.User, Database: w.Database, LockType: w.LockType, Sessions: w.Sessions})
				}
				if r.DiskUsed == 0 {
					r.DiskUsed = ss.DataBytes
				}
				if in.Pooling {
					m.poolFigures(ctx, srv, in, pod, &r)
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

// poolFigures reads in's managed connection pooling figures into r
// (#2163): MySQL's thread pool's page through the API server's pod proxy,
// PostgreSQL's pooler's SHOW POOLS and SHOW STATS, with the sign-ins and
// errors its log counts.
func (m monitoringSQL) poolFigures(ctx context.Context, srv *sqladmin.Server, in sqladmin.MetricInstance, pod string, r *monitoring.SQLInstance) {
	switch r.Engine {
	case "MYSQL":
		if pod == "" {
			return
		}
		page, err := kubectlRaw(ctx, m.kubeconfig, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%d/proxy/", m.namespace, pod, components.MySQLThreadPoolStatusPort))
		if err != nil {
			return
		}
		st, err := mysqlpool.ParseStatus(page)
		if err != nil {
			return
		}
		r.MySQLPool = &monitoring.SQLMySQLPool{Active: int64(st.Active), Idle: int64(st.Idle), Pending: int64(st.Pending),
			Waits: st.Waits, WaitedUs: st.Waited.Microseconds()}
	case "POSTGRES":
		pools, err := srv.PoolerStats(ctx, in.Project, in.Name)
		if err != nil {
			return
		}
		r.HasPGPool = true
		var counts map[string][2]uint64
		counted := false
		if m.logs != nil && pod != "" {
			counts, counted = m.logs.read(ctx, m.kubeconfig, m.namespace, in.Project+":"+in.Name, pod)
		}
		seen := map[string]bool{}
		for _, pd := range pools {
			seen[pd.Database] = true
			c := counts[pd.Database]
			r.PGPools = append(r.PGPools, monitoring.SQLPGPool{Database: pd.Database, Pools: pd.Pools,
				ClientActive: pd.ClientActive, ClientWaiting: pd.ClientWaiting, ServerActive: pd.ServerActive,
				ServerIdle: pd.ServerIdle, AvgWaitUs: pd.AvgWaitUs, Attempts: c[0], Errors: c[1], Counted: counted})
		}
		// A database signed in to that has no pool (every sign-in
		// failed) has its attempts and errors all the same, and one of the
		// server's no client has used the pooler for reads none.
		for _, db := range sqlPoolDBs(counts) {
			if !seen[db] {
				seen[db] = true
				c := counts[db]
				r.PGPools = append(r.PGPools, monitoring.SQLPGPool{Database: db, Attempts: c[0], Errors: c[1], Counted: true})
			}
		}
		dbs := make([]string, 0, len(r.Backends))
		for db := range r.Backends {
			dbs = append(dbs, db)
		}
		sort.Strings(dbs)
		for _, db := range dbs {
			if !seen[db] {
				r.PGPools = append(r.PGPools, monitoring.SQLPGPool{Database: db, Counted: counted})
			}
		}
	}
}

func sqlPoolDBs(m map[string][2]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PgBouncer's log lines (log_connections and log_pooler_errors, both on
// by default): "C-0x...: DB/USER@ADDR login attempt: db=DB user=USER ..."
// for each client sign-in, and "C-0x...: DB/USER@ADDR pooler error: ..."
// for each error sent to a client as the pooler disconnects it (a failed
// sign-in, a server connection that failed, a timeout, a limit reached).
var (
	pgbouncerAttempt = regexp.MustCompile(`login attempt: db=(\S+) user=`)
	pgbouncerError   = regexp.MustCompile(`C-0x[0-9a-f]+: ([^/\s]+)/\S* pooler error:`)
)

// sqlPoolerLogs counts each instance's pooler sign-ins and errors by
// database, from its pods' logs, each read from where the last read
// ended.
type sqlPoolerLogs struct {
	mu sync.Mutex
	by map[string]*poolerLogCounts
}

type poolerLogCounts struct {
	// last is each pod's last line's time.
	last map[string]time.Time
	// counts are attempts and errors by database.
	counts map[string][2]uint64
}

func newSQLPoolerLogs() *sqlPoolerLogs { return &sqlPoolerLogs{by: map[string]*poolerLogCounts{}} }

// read adds pod's pooler log lines since the last read to instance's
// counts, and answers them; false when the log could not be read.
func (l *sqlPoolerLogs) read(ctx context.Context, kubeconfig, namespace, instance, pod string) (map[string][2]uint64, bool) {
	l.mu.Lock()
	c := l.by[instance]
	if c == nil {
		c = &poolerLogCounts{last: map[string]time.Time{}, counts: map[string][2]uint64{}}
		l.by[instance] = c
	}
	last := c.last[pod]
	l.mu.Unlock()
	args := []string{"logs", pod, "-c", components.PoolerContainer, "--timestamps"}
	if !last.IsZero() {
		// The API takes whole seconds: the lines of last's second again
		// are skipped below.
		args = append(args, "--since-time", last.Truncate(time.Second).Format(time.RFC3339))
	}
	out, err := kubeRunner(kubeconfig, namespace).Do(ctx, "", args...)
	if err != nil {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c.last[pod] = countPoolerLog(out, last, c.counts)
	out2 := make(map[string][2]uint64, len(c.counts))
	for k, v := range c.counts {
		out2[k] = v
	}
	return out2, true
}

// countPoolerLog adds the lines of log (kubectl logs --timestamps) after
// last to counts' attempts and errors by database, and answers the last
// line's time.
func countPoolerLog(log string, last time.Time, counts map[string][2]uint64) time.Time {
	newest := last
	for _, line := range strings.Split(log, "\n") {
		stamp, text, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || !at.After(last) {
			continue
		}
		newest = maxTime(newest, at)
		if m := pgbouncerAttempt.FindStringSubmatch(text); m != nil && m[1] != "pgbouncer" {
			v := counts[m[1]]
			v[0]++
			counts[m[1]] = v
		} else if m := pgbouncerError.FindStringSubmatch(text); m != nil && m[1] != "pgbouncer" && m[1] != "(nodb)" {
			v := counts[m[1]]
			v[1]++
			counts[m[1]] = v
		}
	}
	return newest
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
