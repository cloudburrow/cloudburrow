package alloydb

// Query insights and System insights (#2251) read from each instance's own
// PostgreSQL server: pg_stat_statements (preloaded on every server, as
// AlloyDB has it: CREATE EXTENSION needs no flag), pg_stat_activity,
// pg_stat_database, pg_stat_user_tables, pg_prepared_xacts,
// pg_replication_slots and pg_stat_replication. They feed the
// alloydb.googleapis.com metrics in Cloud Monitoring and the console's
// Query insights page. Google's figures come from AlloyDB's own telemetry
// (docs.cloud.google.com/alloydb/docs/using-query-insights and
// docs.cloud.google.com/alloydb/docs/reference/system-insights-metrics,
// read 2026-11-25); these are the server's own counters, read when asked.

import (
	"context"
	"strings"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"github.com/jackc/pgx/v5"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// insightsMarker is in every statement of the reads below, which leave
// out the statements that carry it, as Google's pages leave out AlloyDB's
// own.
const insightsMarker = "cloudburrow-insights"

// insightsTimeout bounds one instance's reads.
const insightsTimeout = 20 * time.Second

// MetricInstance is one instance, as the metrics name it.
type MetricInstance struct {
	// Name is the instance's full name; Project, Location, Cluster and
	// Instance its parts.
	Name, Project, Location, Cluster, Instance string
	Type                                       alloydbpb.Instance_InstanceType
	// Server is the Engine's server.
	Server string
	// CPUs is the machine's vCPUs, Nodes a read pool's node count (1
	// otherwise).
	CPUs, Nodes int32
	// Serving is whether the instance is READY and not stopped.
	Serving bool
	// Insights is Query insights' configuration.
	Insights *alloydbpb.Instance_QueryInsightsInstanceConfig
}

// MetricCluster is one cluster, with its newest READY backup of each type
// (continuous, automated, on-demand).
type MetricCluster struct {
	Name, Project, Location, Cluster string
	// Primary is its primary (or secondary) instance's server, "" with
	// none serving.
	Primary     string
	LastBackups map[string]time.Time
}

// MetricInstances are project's instances ("" every project's).
func (s *Server) MetricInstances(project string) []MetricInstance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []MetricInstance
	for _, k := range sortedKeys(s.instances, "") {
		in := s.instances[k]
		m := instanceRE.FindStringSubmatch(k)
		if m == nil || project != "" && m[2] != project {
			continue
		}
		mi := MetricInstance{Name: k, Project: m[2], Location: m[3], Cluster: m[4], Instance: m[5], Type: in.GetInstanceType(),
			Server: serverName(k), CPUs: in.GetMachineConfig().GetCpuCount(), Nodes: 1,
			Serving:  in.GetState() == alloydbpb.Instance_READY && in.GetActivationPolicy() != alloydbpb.Instance_NEVER,
			Insights: clone(in.GetQueryInsightsConfig())}
		if in.GetInstanceType() == alloydbpb.Instance_READ_POOL {
			mi.Nodes = in.GetReadPoolConfig().GetNodeCount()
		}
		out = append(out, mi)
	}
	return out
}

// MetricClusters are project's clusters ("" every project's).
func (s *Server) MetricClusters(project string) []MetricCluster {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []MetricCluster
	for _, k := range sortedKeys(s.clusters, "") {
		parts := strings.Split(k, "/")
		if len(parts) != 6 || project != "" && parts[1] != project {
			continue
		}
		mc := MetricCluster{Name: k, Project: parts[1], Location: parts[3], Cluster: parts[5], LastBackups: map[string]time.Time{}}
		if p := s.primaryOf(k); p != nil && p.GetState() == alloydbpb.Instance_READY {
			mc.Primary = serverName(p.GetName())
		}
		for _, b := range s.backups {
			if b.GetClusterName() != k || b.GetState() != alloydbpb.Backup_READY || b.GetCreateTime() == nil {
				continue
			}
			typ := map[alloydbpb.Backup_Type]string{alloydbpb.Backup_ON_DEMAND: "on-demand", alloydbpb.Backup_AUTOMATED: "automated",
				alloydbpb.Backup_CONTINUOUS: "continuous"}[b.GetType()]
			if typ == "" {
				continue
			}
			if at := b.GetCreateTime().AsTime(); at.After(mc.LastBackups[typ]) {
				mc.LastBackups[typ] = at
			}
		}
		out = append(out, mc)
	}
	return out
}

// DatabaseStats are one database's pg_stat_database counters (since its
// statistics were reset) and its connections and size now.
type DatabaseStats struct {
	Name                                          string
	Backends                                      int64
	Commits, Rollbacks, BlksHit, BlksRead         int64
	Returned, Fetched, Inserted, Updated, Deleted int64
	TempFiles, TempBytes, Deadlocks, Sessions     int64
	SizeBytes                                     int64
	// Live and Dead are its tables' rows by state (pg_stat_user_tables),
	// read only on an instance of fewer than 50 databases, as Google
	// exports them; HasTuples is whether they were.
	Live, Dead int64
	HasTuples  bool
}

// LoadStat is one user's statement totals in one database since
// pg_stat_statements was reset: time in microseconds (IO counted with
// the track_io_timing flag on, as PostgreSQL counts it), rows and shared
// blocks.
type LoadStat struct {
	User, Database                     string
	Calls, ExecUs, IOReadUs, IOWriteUs int64
	Rows, SharedHit, SharedRead        int64
}

// StatementCount is one database's statements of one operation type
// (SELECT, UPDATE, INSERT, DELETE, MERGE, UTILITY), by the normalized
// query's first keyword.
type StatementCount struct {
	Database, Operation string
	Calls               int64
}

// WaitStat is the backends of one user in one database waiting on a lock
// of one type, lw (LWLock), hw (Lock) or buffer_pin (BufferPin), now.
type WaitStat struct {
	User, Database, LockType string
	Sessions                 int64
}

// InstanceStats are one instance's server figures now.
type InstanceStats struct {
	MaxConnections int64
	// ByState and ByApplication are the client connections by state and
	// by application_name ("Unknown" for none, as Google groups them).
	ByState, ByApplication map[string]int64
	Databases              []DatabaseStats
	// XIDUtilization is the oldest database's transaction ID age over the
	// 2^31 wraparound allows, as a percent.
	XIDUtilization float64
	// OldestAge is the age, in transactions, of the oldest transaction
	// holding back vacuum by type: running, prepared, replication_slot,
	// replica.
	OldestAge map[string]int64
	// Replica is whether the server is a standby, ReplayLagMs then its
	// replay lag (0 once it has replayed everything it received).
	Replica     bool
	ReplayLagMs int64
	// Load, Statements and Waits are Query insights' readings; HasLoad is
	// whether pg_stat_statements was read.
	Load       []LoadStat
	Statements []StatementCount
	Waits      []WaitStat
	HasLoad    bool
}

// QueryStat is one normalized query's totals since pg_stat_statements was
// reset.
type QueryStat struct {
	Query, Database, User string
	Calls                 int64
	TotalMs, MeanMs       float64
	CPUMs, IOMs           float64
	Rows                  int64
}

// serverOf is instance name's server while it serves.
func (s *Server) serverOf(name string) (string, alloydbpb.Instance_InstanceType, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instances[name]
	if !ok {
		return "", 0, apierror.NotFound("Resource '%s' was not found", name)
	}
	if in.GetState() != alloydbpb.Instance_READY {
		return "", 0, apierror.FailedPrecondition("instance %s is %s", name, in.GetState())
	}
	return serverName(name), in.GetInstanceType(), nil
}

func (s *Server) insightsConn(ctx context.Context, server, database string) (*pgx.Conn, error) {
	return s.pgConn(ctx, server, "", database)
}

// pgVersionNum is the server's server_version_num.
func pgVersionNum(ctx context.Context, c *pgx.Conn) int {
	var n int
	_ = c.QueryRow(ctx, "SELECT /* "+insightsMarker+" */ current_setting('server_version_num')::int").Scan(&n)
	return n
}

// ensureStatements makes pg_stat_statements on a primary; a standby has
// its primary's.
func ensureStatements(ctx context.Context, c *pgx.Conn) {
	var standby bool
	if c.QueryRow(ctx, "SELECT /* "+insightsMarker+" */ pg_is_in_recovery()").Scan(&standby) == nil && !standby {
		_, _ = c.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	}
}

// statementsCols are pg_stat_statements' columns of version num: total
// time, IO read and write time.
func statementsCols(num int) (exec, read, write string) {
	exec, read, write = "s.total_exec_time", "s.blk_read_time", "s.blk_write_time"
	if num >= 170000 {
		read, write = "s.shared_blk_read_time + s.local_blk_read_time + s.temp_blk_read_time",
			"s.shared_blk_write_time + s.local_blk_write_time + s.temp_blk_write_time"
	}
	return
}

// statementsFilter leaves out the reads' own statements and those of
// CloudBurrow's management role.
func (s *Server) statementsFilter() string {
	f := "s.query NOT LIKE '%" + insightsMarker + "%' AND s.query NOT LIKE '%pg_stat_statements%'"
	if s.opts.AdminUser != "" {
		f += " AND r.rolname <> " + quoteLiteral(s.opts.AdminUser)
	}
	return f
}

// QueryInsights are instance name's top 100 normalized queries by total
// execution time.
func (s *Server) QueryInsights(ctx context.Context, name string) ([]QueryStat, error) {
	server, _, err := s.serverOf(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, insightsTimeout)
	defer cancel()
	c, err := s.insightsConn(ctx, server, "postgres")
	if err != nil {
		return nil, err
	}
	defer c.Close(context.Background())
	ensureStatements(ctx, c)
	exec, read, write := statementsCols(pgVersionNum(ctx, c))
	rows, err := c.Query(ctx, `SELECT /* `+insightsMarker+` */ s.query, d.datname, r.rolname, s.calls, `+exec+`, s.mean_exec_time, `+
		read+` + `+write+`, s.rows
		FROM pg_stat_statements s JOIN pg_database d ON d.oid = s.dbid JOIN pg_roles r ON r.oid = s.userid
		WHERE `+s.statementsFilter()+` ORDER BY `+exec+` DESC LIMIT 100`)
	if err != nil {
		if strings.Contains(err.Error(), "shared_preload_libraries") {
			return nil, apierror.FailedPrecondition("this server was started without pg_stat_statements preloaded; restart the instance to collect its queries")
		}
		return nil, pgError(err)
	}
	defer rows.Close()
	var out []QueryStat
	for rows.Next() {
		var q QueryStat
		if err := rows.Scan(&q.Query, &q.Database, &q.User, &q.Calls, &q.TotalMs, &q.MeanMs, &q.IOMs, &q.Rows); err != nil {
			return nil, err
		}
		q.CPUMs = q.TotalMs - q.IOMs
		if q.CPUMs < 0 {
			q.CPUMs = 0
		}
		out = append(out, q)
	}
	return out, pgErrorOrNil(rows.Err())
}

func pgErrorOrNil(err error) error {
	if err == nil {
		return nil
	}
	return pgError(err)
}

// statementOp is the operation type of a normalized query, by its first
// keyword.
func statementOp(q string) string {
	q = strings.TrimSpace(q)
	for strings.HasPrefix(q, "/*") {
		_, rest, ok := strings.Cut(q, "*/")
		if !ok {
			break
		}
		q = strings.TrimSpace(rest)
	}
	word, _, _ := strings.Cut(strings.ToUpper(q), " ")
	word = strings.TrimRight(word, ";(\n\t")
	switch word {
	case "SELECT", "UPDATE", "INSERT", "DELETE", "MERGE":
		return word
	case "WITH", "":
		return "UNKNOWN"
	}
	return "UTILITY"
}

// InstanceStats reads instance name's server figures.
func (s *Server) InstanceStats(ctx context.Context, name string) (InstanceStats, error) {
	server, _, err := s.serverOf(name)
	if err != nil {
		return InstanceStats{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, insightsTimeout)
	defer cancel()
	c, err := s.insightsConn(ctx, server, "postgres")
	if err != nil {
		return InstanceStats{}, err
	}
	defer c.Close(context.Background())
	m := "/* " + insightsMarker + " */ "
	st := InstanceStats{ByState: map[string]int64{}, ByApplication: map[string]int64{}, OldestAge: map[string]int64{}}
	if err := c.QueryRow(ctx, `SELECT `+m+`current_setting('max_connections')::bigint,
		(SELECT COALESCE(max(age(datfrozenxid)), 0) FROM pg_database)::float8 * 100 / 2147483648, pg_is_in_recovery()`).
		Scan(&st.MaxConnections, &st.XIDUtilization, &st.Replica); err != nil {
		return InstanceStats{}, pgError(err)
	}
	rows, err := c.Query(ctx, `SELECT `+m+`COALESCE(state, 'disabled'), COALESCE(NULLIF(application_name, ''), 'Unknown'), count(*)
		FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid() GROUP BY 1, 2`)
	if err != nil {
		return InstanceStats{}, pgError(err)
	}
	for rows.Next() {
		var state, app string
		var n int64
		if rows.Scan(&state, &app, &n) == nil {
			// pg_stat_activity's "idle in transaction (aborted)" is
			// Google's idle_in_transaction_aborted.
			state = strings.NewReplacer(" (aborted)", "_aborted", " ", "_").Replace(state)
			st.ByState[state] += n
			st.ByApplication[app] += n
		}
	}
	rows.Close()
	rows, err = c.Query(ctx, `SELECT `+m+`d.datname, d.xact_commit, d.xact_rollback, d.blks_hit, d.blks_read, d.tup_returned, d.tup_fetched,
		d.tup_inserted, d.tup_updated, d.tup_deleted, d.temp_files, d.temp_bytes, d.deadlocks, COALESCE(d.sessions, 0),
		pg_database_size(d.datid),
		(SELECT count(*) FROM pg_stat_activity a WHERE a.datid = d.datid AND a.backend_type = 'client backend' AND a.pid <> pg_backend_pid())
		FROM pg_stat_database d WHERE d.datname IS NOT NULL AND d.datname NOT IN ('template0', 'template1') ORDER BY 1`)
	if err != nil {
		return InstanceStats{}, pgError(err)
	}
	for rows.Next() {
		var d DatabaseStats
		if rows.Scan(&d.Name, &d.Commits, &d.Rollbacks, &d.BlksHit, &d.BlksRead, &d.Returned, &d.Fetched, &d.Inserted, &d.Updated,
			&d.Deleted, &d.TempFiles, &d.TempBytes, &d.Deadlocks, &d.Sessions, &d.SizeBytes, &d.Backends) == nil {
			st.Databases = append(st.Databases, d)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return InstanceStats{}, pgError(err)
	}
	// The oldest transaction holding vacuum back, by what holds it.
	for typ, q := range map[string]string{
		"running":          "SELECT " + m + "COALESCE(max(age(backend_xid)), 0) FROM pg_stat_activity WHERE backend_xid IS NOT NULL",
		"prepared":         "SELECT " + m + "COALESCE(max(age(transaction)), 0) FROM pg_prepared_xacts",
		"replication_slot": "SELECT " + m + "COALESCE(max(greatest(age(xmin), age(catalog_xmin))), 0) FROM pg_replication_slots",
		"replica":          "SELECT " + m + "COALESCE(max(age(backend_xmin)), 0) FROM pg_stat_replication",
	} {
		var n int64
		if c.QueryRow(ctx, q).Scan(&n) == nil {
			st.OldestAge[typ] = n
		}
	}
	if st.Replica {
		// Caught up, the lag is none; else the time since the last
		// transaction it replayed was committed.
		_ = c.QueryRow(ctx, `SELECT `+m+`CASE WHEN pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn() THEN 0
			ELSE COALESCE((extract(epoch FROM now() - pg_last_xact_replay_timestamp()) * 1000)::bigint, 0) END`).Scan(&st.ReplayLagMs)
	}
	s.readLoad(ctx, c, &st)
	if len(st.Databases) < 50 {
		for i := range st.Databases {
			s.readTuples(ctx, server, &st.Databases[i])
		}
	}
	return st, nil
}

// readLoad reads pg_stat_statements' totals and the lock waits now.
func (s *Server) readLoad(ctx context.Context, c *pgx.Conn, st *InstanceStats) {
	m := "/* " + insightsMarker + " */ "
	ensureStatements(ctx, c)
	exec, read, write := statementsCols(pgVersionNum(ctx, c))
	rows, err := c.Query(ctx, `SELECT `+m+`r.rolname, d.datname, sum(s.calls)::bigint, (sum(`+exec+`) * 1000)::bigint,
		(sum(`+read+`) * 1000)::bigint, (sum(`+write+`) * 1000)::bigint, sum(s.rows)::bigint,
		sum(s.shared_blks_hit)::bigint, sum(s.shared_blks_read)::bigint
		FROM pg_stat_statements s JOIN pg_database d ON d.oid = s.dbid JOIN pg_roles r ON r.oid = s.userid
		WHERE `+s.statementsFilter()+` GROUP BY 1, 2 ORDER BY 1, 2`)
	if err == nil {
		st.HasLoad = true
		for rows.Next() {
			var l LoadStat
			if rows.Scan(&l.User, &l.Database, &l.Calls, &l.ExecUs, &l.IOReadUs, &l.IOWriteUs, &l.Rows, &l.SharedHit, &l.SharedRead) == nil {
				st.Load = append(st.Load, l)
			}
		}
		rows.Close()
		ops := map[[2]string]int64{}
		if rows, err := c.Query(ctx, `SELECT `+m+`d.datname, s.query, s.calls FROM pg_stat_statements s
			JOIN pg_database d ON d.oid = s.dbid JOIN pg_roles r ON r.oid = s.userid WHERE `+s.statementsFilter()); err == nil {
			for rows.Next() {
				var db, q string
				var n int64
				if rows.Scan(&db, &q, &n) == nil {
					ops[[2]string{db, statementOp(q)}] += n
				}
			}
			rows.Close()
		}
		for k, n := range ops {
			st.Statements = append(st.Statements, StatementCount{Database: k[0], Operation: k[1], Calls: n})
		}
	}
	rows, err = c.Query(ctx, `SELECT `+m+`COALESCE(usename, ''), COALESCE(datname, ''),
		CASE wait_event_type WHEN 'LWLock' THEN 'lw' WHEN 'Lock' THEN 'hw' ELSE 'buffer_pin' END, count(*)
		FROM pg_stat_activity WHERE state = 'active' AND backend_type = 'client backend'
		AND wait_event_type IN ('LWLock', 'Lock', 'BufferPin') AND pid <> pg_backend_pid() GROUP BY 1, 2, 3`)
	if err == nil {
		for rows.Next() {
			var w WaitStat
			if rows.Scan(&w.User, &w.Database, &w.LockType, &w.Sessions) == nil {
				st.Waits = append(st.Waits, w)
			}
		}
		rows.Close()
	}
}

// readTuples reads d's tables' live and dead rows, in d itself.
func (s *Server) readTuples(ctx context.Context, server string, d *DatabaseStats) {
	c, err := s.insightsConn(ctx, server, d.Name)
	if err != nil {
		return
	}
	defer c.Close(context.Background())
	if c.QueryRow(ctx, "SELECT /* "+insightsMarker+" */ COALESCE(sum(n_live_tup), 0)::bigint, COALESCE(sum(n_dead_tup), 0)::bigint FROM pg_stat_user_tables").
		Scan(&d.Live, &d.Dead) == nil {
		d.HasTuples = true
	}
}
