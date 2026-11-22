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
	"strings"
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
	return st, nil
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
		(SELECT COALESCE(SUM(DATA_LENGTH + INDEX_LENGTH), 0) FROM information_schema.TABLES)`).
		Scan(&st.Connections, &st.PeakConnections, &st.MaxConnections, &st.DataBytes); err != nil {
		return SystemStats{}, myError(err)
	}
	return st, nil
}
