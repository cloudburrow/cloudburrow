package sqladmin

// Query insights and System insights read from the instance's own server
// (#2141). Google's console draws them from Cloud SQL's telemetry in Cloud
// Monitoring (https://docs.cloud.google.com/sql/docs/postgres/using-query-insights,
// https://docs.cloud.google.com/sql/docs/postgres/use-system-insights);
// CloudBurrow keeps no such series, so the console reads the server's own
// statistics: pg_stat_statements on PostgreSQL, which Cloud SQL preloads
// (CREATE EXTENSION pg_stat_statements needs no flag there), and
// performance_schema's statement digests on MySQL. They are the totals
// since the server started, not a chosen time range.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// QueryStat is one normalized query's totals.
type QueryStat struct {
	Query    string
	Database string
	// User is the role that ran it; MySQL's digests do not keep it.
	User string
	// Calls is Times called.
	Calls int64
	// TotalMs and MeanMs are the total and average execution times.
	TotalMs float64
	MeanMs  float64
	// Rows is the rows returned (or affected) over every call.
	Rows int64
}

// SystemStats is a server's current figures for System insights.
type SystemStats struct {
	// Connections is the client connections now open; PeakConnections the
	// most open at once since the server started, where the server keeps
	// it (MySQL's Max_used_connections), else Connections.
	Connections, PeakConnections, MaxConnections int64
	// TransactionIDUtilization is the oldest database's transaction ID age
	// over the 2^31 that wraparound allows, in percent; PostgreSQL only,
	// -1 elsewhere.
	TransactionIDUtilization float64
	// DataBytes is the size of the server's databases.
	DataBytes int64
	// Backends are PostgreSQL's client connections by database; nil
	// elsewhere (#2141, postgresql/num_backends).
	Backends map[string]int64
	// Commits and Rollbacks are PostgreSQL's transactions by database,
	// as pg_stat_database counts them.
	Commits, Rollbacks map[string]uint64
	// Queries is MySQL's Queries status: statements executed since it
	// started (mysql/queries).
	Queries uint64
	// Load is PostgreSQL's statement time by user and database, from
	// pg_stat_statements; Waits its backends waiting on a lock now, from
	// pg_stat_activity (Query insights' database load, #2141). Nil
	// elsewhere, or before pg_stat_statements is made.
	Load  []LoadStat
	Waits []WaitStat
}

// LoadStat is one user's statement time in one database, in
// microseconds: execution, and reading and writing blocks (counted only
// with the track_io_timing flag on, as PostgreSQL counts them).
type LoadStat struct {
	User, Database              string
	ExecUs, IOReadUs, IOWriteUs int64
}

// WaitStat is the backends of one user in one database waiting on a lock
// of one type: lw (LWLock), hw (Lock) or buffer_pin (BufferPin).
type WaitStat struct {
	User, Database, LockType string
	Sessions                 int64
}

// insightsEngine is an engine that reads its own statistics.
type insightsEngine interface {
	QueryStats(ctx context.Context) ([]QueryStat, error)
	SystemStats(ctx context.Context) (SystemStats, error)
}

// insightsOf is instance name's engine in project when it reads its own
// statistics.
func (s *Server) insightsOf(project, name string) (insightsEngine, error) {
	e := s.engine(name)
	if e != nil && s.createdNamed(project, name) == nil && s.writes != nil {
		s.emu.RLock()
		if s.writes.byName[name] != nil {
			e = nil // another project's
		}
		s.emu.RUnlock()
	}
	if e == nil {
		return nil, Errorf(http.StatusNotFound, "The Cloud SQL instance does not exist.")
	}
	ie, ok := e.(insightsEngine)
	if !ok {
		return nil, errors.New("this instance's server keeps no statement statistics")
	}
	return ie, nil
}

// QueryInsights are instance name's top queries by total execution time,
// at most 100.
func (s *Server) QueryInsights(ctx context.Context, project, name string) ([]QueryStat, error) {
	ie, err := s.insightsOf(project, name)
	if err != nil {
		return nil, err
	}
	return ie.QueryStats(ctx)
}

// SystemInsights are instance name's current server figures.
func (s *Server) SystemInsights(ctx context.Context, project, name string) (SystemStats, error) {
	ie, err := s.insightsOf(project, name)
	if err != nil {
		return SystemStats{}, err
	}
	return ie.SystemStats(ctx)
}

const topQueries = 100

// QueryStats reads pg_stat_statements, made in the server's first
// database if it is not there yet. Its own reads, and SystemStats', are
// left out, as Google's page leaves out Cloud SQL's own.
func (p *Postgres) QueryStats(ctx context.Context) ([]QueryStat, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close(context.Background())
	if _, err := c.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		return nil, pgError(err)
	}
	var num int
	if err := c.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&num); err != nil {
		return nil, pgError(err)
	}
	// PostgreSQL 13 split total_time into planning and execution.
	total, mean := "s.total_exec_time", "s.mean_exec_time"
	if num < 130000 {
		total, mean = "s.total_time", "s.mean_time"
	}
	rows, err := c.Query(ctx, `SELECT s.query, d.datname, r.rolname, s.calls, `+total+`, `+mean+`, s.rows
		FROM pg_stat_statements s JOIN pg_database d ON d.oid = s.dbid JOIN pg_roles r ON r.oid = s.userid
		WHERE s.query NOT LIKE '%pg_stat_statements%' AND s.query NOT LIKE '%cloudburrow-insights%'
		ORDER BY `+total+` DESC LIMIT $1`, topQueries)
	if err != nil {
		if strings.Contains(err.Error(), "shared_preload_libraries") {
			return nil, Errorf(http.StatusPreconditionFailed, "this server was started before CloudBurrow preloaded "+
				"pg_stat_statements; restart the instance to collect its queries")
		}
		return nil, pgError(err)
	}
	defer rows.Close()
	var out []QueryStat
	for rows.Next() {
		var q QueryStat
		if err := rows.Scan(&q.Query, &q.Database, &q.User, &q.Calls, &q.TotalMs, &q.MeanMs, &q.Rows); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, pgError(err)
	}
	return out, nil
}

// SystemStats reads pg_stat_activity, max_connections, the databases'
// transaction ID ages and sizes.
func (p *Postgres) SystemStats(ctx context.Context) (SystemStats, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return SystemStats{}, err
	}
	defer c.Close(context.Background())
	var st SystemStats
	if err := c.QueryRow(ctx, `SELECT /* cloudburrow-insights */ (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'),
		current_setting('max_connections')::bigint,
		(SELECT COALESCE(max(age(datfrozenxid)), 0) FROM pg_database)::float8 * 100 / 2147483648,
		(SELECT COALESCE(sum(pg_database_size(oid)), 0) FROM pg_database)::bigint`).
		Scan(&st.Connections, &st.MaxConnections, &st.TransactionIDUtilization, &st.DataBytes); err != nil {
		return SystemStats{}, pgError(err)
	}
	st.PeakConnections = st.Connections
	st.Backends, st.Commits, st.Rollbacks = map[string]int64{}, map[string]uint64{}, map[string]uint64{}
	rows, err := c.Query(ctx, `SELECT /* cloudburrow-insights */ d.datname, d.xact_commit, d.xact_rollback,
		(SELECT count(*) FROM pg_stat_activity a WHERE a.datid = d.datid AND a.backend_type = 'client backend')
		FROM pg_stat_database d WHERE d.datname IS NOT NULL AND d.datname NOT IN ('template0', 'template1')`)
	if err != nil {
		return SystemStats{}, pgError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var db string
		var commit, rollback, n int64
		if err := rows.Scan(&db, &commit, &rollback, &n); err != nil {
			return SystemStats{}, err
		}
		st.Backends[db], st.Commits[db], st.Rollbacks[db] = n, uint64(commit), uint64(rollback)
	}
	if err := rows.Err(); err != nil {
		return SystemStats{}, pgError(err)
	}
	rows.Close()
	st.Load, st.Waits = pgLoad(ctx, c)
	return st, nil
}

// pgLoad reads the server's statement time by user and database and its
// backends waiting on locks; none where pg_stat_statements is not made
// (QueryStats makes it) or cannot be read.
func pgLoad(ctx context.Context, c *pgx.Conn) ([]LoadStat, []WaitStat) {
	var num int
	var has bool
	if err := c.QueryRow(ctx, `SELECT /* cloudburrow-insights */ current_setting('server_version_num')::int,
		EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements')`).Scan(&num, &has); err != nil {
		return nil, nil
	}
	var load []LoadStat
	if has {
		exec, read, write := "s.total_exec_time", "s.blk_read_time", "s.blk_write_time"
		switch {
		case num < 130000:
			exec = "s.total_time"
		case num >= 170000:
			read, write = "s.shared_blk_read_time + s.local_blk_read_time + s.temp_blk_read_time",
				"s.shared_blk_write_time + s.local_blk_write_time + s.temp_blk_write_time"
		}
		rows, err := c.Query(ctx, `SELECT /* cloudburrow-insights */ r.rolname, d.datname,
			(sum(`+exec+`) * 1000)::bigint, (sum(`+read+`) * 1000)::bigint, (sum(`+write+`) * 1000)::bigint
			FROM pg_stat_statements s JOIN pg_database d ON d.oid = s.dbid JOIN pg_roles r ON r.oid = s.userid
			WHERE s.query NOT LIKE '%cloudburrow-insights%' AND s.query NOT LIKE '%pg_stat_statements%'
			GROUP BY r.rolname, d.datname ORDER BY 1, 2`)
		if err == nil {
			for rows.Next() {
				var l LoadStat
				if rows.Scan(&l.User, &l.Database, &l.ExecUs, &l.IOReadUs, &l.IOWriteUs) == nil {
					load = append(load, l)
				}
			}
			rows.Close()
		}
	}
	var waits []WaitStat
	rows, err := c.Query(ctx, `SELECT /* cloudburrow-insights */ COALESCE(usename, ''), COALESCE(datname, ''),
		CASE wait_event_type WHEN 'LWLock' THEN 'lw' WHEN 'Lock' THEN 'hw' ELSE 'buffer_pin' END, count(*)
		FROM pg_stat_activity WHERE state = 'active' AND backend_type = 'client backend'
		AND wait_event_type IN ('LWLock', 'Lock', 'BufferPin') AND pid <> pg_backend_pid()
		GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`)
	if err == nil {
		for rows.Next() {
			var w WaitStat
			if rows.Scan(&w.User, &w.Database, &w.LockType, &w.Sessions) == nil {
				waits = append(waits, w)
			}
		}
		rows.Close()
	}
	return load, waits
}

// QueryStats reads performance_schema's statement digests, whose timers
// are in picoseconds.
func (m *MySQL) QueryStats(ctx context.Context) ([]QueryStat, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT DIGEST_TEXT, COALESCE(SCHEMA_NAME, ''), COUNT_STAR,
		SUM_TIMER_WAIT / 1000000000, AVG_TIMER_WAIT / 1000000000, SUM_ROWS_SENT + SUM_ROWS_AFFECTED
		FROM performance_schema.events_statements_summary_by_digest
		WHERE DIGEST_TEXT IS NOT NULL AND DIGEST_TEXT NOT LIKE '%performance_schema%'
		ORDER BY SUM_TIMER_WAIT DESC LIMIT ?`, topQueries)
	if err != nil {
		return nil, myError(err)
	}
	defer rows.Close()
	var out []QueryStat
	for rows.Next() {
		var q QueryStat
		if err := rows.Scan(&q.Query, &q.Database, &q.Calls, &q.TotalMs, &q.MeanMs, &q.Rows); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, myError(err)
	}
	return out, nil
}

// SystemStats reads Threads_connected, Max_used_connections,
// max_connections and the schemas' sizes.
func (m *MySQL) SystemStats(ctx context.Context) (SystemStats, error) {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return SystemStats{}, err
	}
	defer db.Close()
	st := SystemStats{TransactionIDUtilization: -1}
	if err := db.QueryRowContext(ctx, `SELECT
		(SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Threads_connected'),
		(SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Max_used_connections'),
		@@max_connections,
		(SELECT COALESCE(SUM(DATA_LENGTH + INDEX_LENGTH), 0) FROM information_schema.TABLES),
		(SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Queries')`).
		Scan(&st.Connections, &st.PeakConnections, &st.MaxConnections, &st.DataBytes, &st.Queries); err != nil {
		return SystemStats{}, myError(err)
	}
	return st, nil
}

// MetricInstance is an instance Cloud Monitoring's Cloud SQL metrics are
// read for (#2141).
type MetricInstance struct {
	Project, Name, Region, DatabaseVersion, Tier string
	// Server is its server's name in the cluster.
	Server string
	// State is its Cloud SQL state; Stopped whether its owner stopped it.
	State   string
	Stopped bool
	// Cores and RAM are the tier's; DiskBytes the data disk's size.
	Cores     float64
	RAM       int64
	DiskBytes int64
}

// MetricInstances are the instances created through the Admin API, in
// every project, then the servers `up` runs, in project (theirs is every
// project's; their Server is "", the cluster's name for them being the
// service's).
func (s *Server) MetricInstances(project string) []MetricInstance {
	var out []MetricInstance
	for _, e := range s.engines {
		cores, ram := TierShape("db-custom-1-3840")
		out = append(out, MetricInstance{Project: project, Name: e.Instance(), Region: Region, DatabaseVersion: e.DatabaseVersion(),
			Tier: "db-custom-1-3840", State: "RUNNABLE", Cores: cores, RAM: ram, DiskBytes: minDiskGB << 30})
	}
	s.emu.RLock()
	defer s.emu.RUnlock()
	if s.writes == nil {
		return out
	}
	for _, c := range s.writes.byName {
		sp := c.spec
		cores, ram := TierShape(sp.Tier)
		out = append(out, MetricInstance{Project: sp.Project, Name: sp.Name, Region: sp.region(), DatabaseVersion: sp.DatabaseVersion,
			Tier: sp.Tier, Server: sp.ServerName(), State: c.state, Stopped: sp.Stopped(), Cores: cores, RAM: ram,
			DiskBytes: diskSizeGB(sp) << 30})
	}
	return out
}

// TierShape is a tier's vCPUs and memory: db-custom-CPUS-MEMORY_MIB, a
// documented tier's RAM, the shared-core tiers' fractions (db-f1-micro
// 0.2 vCPU, db-g1-small 0.5, as Google's machine series overview gives
// them) and db-perf-optimized-N-CPUS.
func TierShape(tier string) (cores float64, ram int64) {
	for _, t := range documentedTiers {
		if t.Tier == tier {
			ram = t.RAM
		}
	}
	switch tier {
	case "db-f1-micro":
		return 0.2, ram
	case "db-g1-small":
		return 0.5, ram
	}
	parts := strings.Split(tier, "-")
	if strings.HasPrefix(tier, "db-custom-") && len(parts) == 4 {
		c, err1 := strconv.ParseFloat(parts[2], 64)
		m, err2 := strconv.ParseInt(parts[3], 10, 64)
		if err1 == nil && err2 == nil {
			return c, m << 20
		}
	}
	if strings.HasPrefix(tier, "db-perf-optimized-N-") {
		if c, err := strconv.ParseFloat(parts[len(parts)-1], 64); err == nil {
			return c, ram
		}
	}
	return 1, ram
}
