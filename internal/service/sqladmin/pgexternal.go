package sqladmin

// A PostgreSQL external primary (#1524), as Google documents it
// (docs.cloud.google.com/sql/docs/postgres/replication/configure-replication-from-external):
// a source representation instance records the outside server, a replica
// of it runs a PostgreSQL server of its own, verifyExternalSyncSettings
// checks the source's settings, startExternalSync copies the source's
// databases (every one but the templates, or its selectedObjects) into the
// replica and goes on replicating them, and promoteReplica ends the
// migration. Cloud SQL does this with pglogical; CloudBurrow's images have
// no pglogical, so the replica subscribes to a publication of each
// database by PostgreSQL's built-in logical replication, which needs the
// same wal_level, slots, WAL senders and worker processes of the source.

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// sourceDatabasesQuery lists the databases a sync migrates by default.
const sourceDatabasesQuery = `SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate
  AND datname NOT IN ('template0', 'template1', 'cloudsqladmin', 'rdsadmin') ORDER BY datname`

func pgSourceURL(ext *ExternalSource, addr, database string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(ext.User, ext.Password), Host: addr, Path: "/" + database,
		RawQuery: "sslmode=prefer&connect_timeout=10"}
	return u.String()
}

// verifyPostgresSync is verifyExternalSyncSettings of a PostgreSQL
// source: reachable as the replication user, a major version the replica
// can take, wal_level logical, enough replication slots, WAL senders and
// worker processes for one subscription per database, and a user with
// REPLICATION that may publish every table (a superuser, or their owner
// with CREATE on the database).
func verifyPostgresSync(ctx context.Context, spec Spec, addr string, offline bool) []any {
	errs := []any{}
	ext := spec.External
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, pgSourceURL(ext, addr, "postgres"))
	if err != nil {
		return append(errs, syncError("CONNECTION_FAILURE", fmt.Sprintf("connect to %s as %s: %v", addr, ext.User, pgError(err))))
	}
	defer c.Close(context.Background())
	var num, walLevel, slots, senders, workers string
	var super, repl bool
	if err := c.QueryRow(ctx, `SELECT current_setting('server_version_num'), current_setting('wal_level'),
	  current_setting('max_replication_slots'), current_setting('max_wal_senders'), current_setting('max_worker_processes'),
	  rolsuper, rolreplication FROM pg_roles WHERE rolname = current_user`).Scan(&num, &walLevel, &slots, &senders, &workers, &super, &repl); err != nil {
		return append(errs, syncError("CONNECTION_FAILURE", pgError(err).Error()))
	}
	dbs := ext.Databases
	if len(dbs) == 0 {
		rows, err := c.Query(ctx, sourceDatabasesQuery)
		if err == nil {
			dbs, err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
		if err != nil {
			return append(errs, syncError("CONNECTION_FAILURE", pgError(err).Error()))
		}
	}
	srcMajor := pgMajor(num)
	if want := pgVersionMajor(spec.DatabaseVersion); srcMajor > want || srcMajor < 10 {
		errs = append(errs, syncError("INCOMPATIBLE_DATABASE_VERSION",
			fmt.Sprintf("the source runs PostgreSQL %d and the replica %s; the replica's major version must be the source's or later, and the source 10 or later",
				srcMajor, spec.DatabaseVersion)))
	}
	if !offline {
		if walLevel != "logical" {
			errs = append(errs, syncError("INVALID_WAL_LEVEL", "the source's wal_level is "+walLevel+"; it must be logical "+
				"(on a Cloud SQL source, the flag cloudsql.logical_decoding=on)"))
		}
		n := len(dbs)
		for _, x := range []struct {
			typ, name, v string
		}{{"INSUFFICIENT_MAX_REPLICATION_SLOTS", "max_replication_slots", slots}, {"INSUFFICIENT_MAX_WAL_SENDERS", "max_wal_senders", senders},
			{"INSUFFICIENT_MAX_WORKER_PROCESSES", "max_worker_processes", workers}} {
			if v, _ := strconv.Atoi(x.v); v < n {
				errs = append(errs, syncError(x.typ, fmt.Sprintf("the source's %s is %s; the sync needs one for each of its %d databases", x.name, x.v, n)))
			}
		}
		if !super && !repl {
			errs = append(errs, syncError("INSUFFICIENT_PRIVILEGE", "the replication user "+ext.User+" has no REPLICATION attribute"))
		}
	}
	for _, db := range dbs {
		dc, err := pgx.Connect(ctx, pgSourceURL(ext, addr, db))
		if err != nil {
			errs = append(errs, syncError("CONNECTION_FAILURE", fmt.Sprintf("connect to the database %s: %v", db, pgError(err))))
			continue
		}
		var notOwned, unreadable int
		var create bool
		err = dc.QueryRow(ctx, `SELECT has_database_privilege(current_database(), 'CREATE'), count(*) FILTER (WHERE NOT pg_has_role(c.relowner, 'USAGE')),
		  count(*) FILTER (WHERE NOT has_table_privilege(c.oid, 'SELECT'))
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_%'`).
			Scan(&create, &notOwned, &unreadable)
		dc.Close(context.Background())
		switch {
		case err != nil:
			errs = append(errs, syncError("INSUFFICIENT_PRIVILEGE", fmt.Sprintf("the database %s: %v", db, pgError(err))))
		case unreadable > 0:
			errs = append(errs, syncError("INSUFFICIENT_PRIVILEGE", fmt.Sprintf("the replication user %s cannot SELECT %d tables of the database %s", ext.User, unreadable, db)))
		case !offline && !create:
			errs = append(errs, syncError("INSUFFICIENT_PRIVILEGE", fmt.Sprintf("the replication user %s has no CREATE on the database %s, "+
				"so it cannot create the publication the replica subscribes to", ext.User, db)))
		case !offline && !super && notOwned > 0:
			errs = append(errs, syncError("INSUFFICIENT_PRIVILEGE", fmt.Sprintf("the replication user %s neither is a superuser nor owns %d tables of the database %s, "+
				"so it cannot publish them", ext.User, notOwned, db)))
		}
	}
	return errs
}

func pgMajor(num string) int {
	n, _ := strconv.Atoi(num)
	if n >= 100000 {
		return n / 10000
	}
	return n / 100 // 9.6 is 90600: 906
}

// pgVersionMajor is POSTGRES_17's 17.
func pgVersionMajor(v string) int {
	n, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(v, "POSTGRES_"), "_", 2)[0])
	return n
}

// subscriptions are a logical replica's subscriptions, by database.
func (p *Postgres) subscriptions(ctx context.Context) (map[string][]string, error) {
	c, err := p.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close(context.Background())
	rows, err := c.Query(ctx, `SELECT d.datname, s.subname FROM pg_subscription s JOIN pg_database d ON d.oid = s.subdbid ORDER BY 1, 2`)
	if err != nil {
		return nil, pgError(err)
	}
	out := map[string][]string{}
	for rows.Next() {
		var d, n string
		if err := rows.Scan(&d, &n); err != nil {
			return nil, pgError(err)
		}
		out[d] = append(out[d], n)
	}
	if err := rows.Err(); err != nil {
		return nil, pgError(err)
	}
	return out, nil
}

// inDatabase runs stmts in database as the superuser.
func (p *Postgres) inDatabase(ctx context.Context, database string, stmts ...string) error {
	q := *p
	q.Database = database
	c, err := q.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	for _, st := range stmts {
		if _, err := c.Exec(ctx, st); err != nil {
			return pgError(err)
		}
	}
	return nil
}

// promoteLogical drops a logical replica's subscriptions, and with them
// their slots on the source; a source that cannot be reached keeps its
// slots, as a subscription whose slot is NONE drops without it.
func (p *Postgres) promoteLogical(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	subs, err := p.subscriptions(ctx)
	if err != nil {
		return err
	}
	for db, ns := range subs {
		for _, n := range ns {
			id := pgx.Identifier{n}.Sanitize()
			if err := p.inDatabase(ctx, db, "DROP SUBSCRIPTION "+id); err != nil {
				if err := p.inDatabase(ctx, db, "ALTER SUBSCRIPTION "+id+" DISABLE", "ALTER SUBSCRIPTION "+id+" SET (slot_name = NONE)",
					"DROP SUBSCRIPTION "+id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// pauseLogical disables or enables a logical replica's subscriptions.
func (p *Postgres) pauseLogical(ctx context.Context, pause bool) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	subs, err := p.subscriptions(ctx)
	if err != nil {
		return err
	}
	verb := " ENABLE"
	if pause {
		verb = " DISABLE"
	}
	for db, ns := range subs {
		for _, n := range ns {
			if err := p.inDatabase(ctx, db, "ALTER SUBSCRIPTION "+pgx.Identifier{n}.Sanitize()+verb); err != nil {
				return err
			}
		}
	}
	return nil
}
