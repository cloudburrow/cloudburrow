package sqladmin

// The managed connection pooler's figures (#2163), which Cloud Monitoring's
// per_database/conn_pool metrics are read from: PgBouncer's SHOW POOLS
// (each pool's clients active and waiting, its server connections active
// and idle) and SHOW STATS (each database's average wait), on its own
// pgbouncer database, as components.PoolerStatsUser.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/cloudburrow/cloudburrow/internal/components"
)

// PoolDatabase is one database's pools now.
type PoolDatabase struct {
	Database string
	// Pools is the pools of the database: one a user.
	Pools int64
	// ClientActive are the clients linked to a server connection or idle
	// with none needed; ClientWaiting those waiting for one.
	ClientActive, ClientWaiting int64
	// ServerActive are the server connections linked to a client;
	// ServerIdle those unused (idle, or used and awaiting a check).
	ServerActive, ServerIdle int64
	// AvgWaitUs is the clients' average wait for a server connection over
	// the pooler's last stats period, in microseconds.
	AvgWaitUs int64
}

// ErrNoPooler is PoolStats on an instance whose pooler is not running.
var ErrNoPooler = errors.New("the instance runs no managed connection pooler")

// PoolStats reads the pooler's pools and stats by database.
func (p *Postgres) PoolStats(ctx context.Context) ([]PoolDatabase, error) {
	addr := p.PoolerAddr()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, ErrNoPooler
	}
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	cfg, err := pgx.ParseConfig(fmt.Sprintf("postgres://%s@%s:%s/pgbouncer?sslmode=disable", components.PoolerStatsUser, host, port))
	if err != nil {
		return nil, err
	}
	// The pooler's console speaks the simple query protocol alone.
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, Errorf(http.StatusServiceUnavailable, "cannot reach the connection pooler: %v", err)
	}
	defer c.Close(context.Background())
	pools, err := showRows(ctx, c, "SHOW POOLS")
	if err != nil {
		return nil, err
	}
	stats, err := showRows(ctx, c, "SHOW STATS")
	if err != nil {
		return nil, err
	}
	return poolDatabases(pools, stats), nil
}

// poolDatabases sums SHOW POOLS' rows by database, with SHOW STATS'
// avg_wait_time; the pooler's own database is left out.
func poolDatabases(pools, stats []map[string]string) []PoolDatabase {
	num := func(r map[string]string, k string) int64 {
		n, _ := strconv.ParseInt(r[k], 10, 64)
		return n
	}
	byDB := map[string]*PoolDatabase{}
	var order []string
	get := func(db string) *PoolDatabase {
		if d := byDB[db]; d != nil {
			return d
		}
		d := &PoolDatabase{Database: db}
		byDB[db] = d
		order = append(order, db)
		return d
	}
	for _, r := range pools {
		db := r["database"]
		if db == "" || db == "pgbouncer" {
			continue
		}
		d := get(db)
		d.Pools++
		d.ClientActive += num(r, "cl_active")
		d.ClientWaiting += num(r, "cl_waiting")
		d.ServerActive += num(r, "sv_active")
		d.ServerIdle += num(r, "sv_idle") + num(r, "sv_used")
	}
	for _, r := range stats {
		if d := byDB[r["database"]]; d != nil {
			d.AvgWaitUs = num(r, "avg_wait_time")
		}
	}
	out := make([]PoolDatabase, 0, len(order))
	for _, db := range order {
		out = append(out, *byDB[db])
	}
	return out
}

// showRows runs one of the console's SHOW commands, each row by column.
func showRows(ctx context.Context, c *pgx.Conn, cmd string) ([]map[string]string, error) {
	rows, err := c.Query(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("the pooler's %s: %w", cmd, err)
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		fields := rows.FieldDescriptions()
		r := map[string]string{}
		for i, v := range rows.RawValues() {
			if i < len(fields) && v != nil {
				r[fields[i].Name] = string(v)
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the pooler's %s: %w", cmd, err)
	}
	return out, nil
}

// PoolerStats are instance name's pooler figures, by database.
func (s *Server) PoolerStats(ctx context.Context, project, name string) ([]PoolDatabase, error) {
	if _, err := s.insightsOf(project, name); err != nil {
		return nil, err
	}
	pg, ok := s.engine(name).(*Postgres)
	if !ok {
		return nil, ErrNoPooler
	}
	return pg.PoolStats(ctx)
}
