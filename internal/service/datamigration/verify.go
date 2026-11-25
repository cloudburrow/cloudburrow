package datamigration

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
)

// The checks a verification runs, each from the destination's server, as
// Google documents a source's configuration
// (docs.cloud.google.com/database-migration/docs/postgresql-to-alloydb/
// configure-source-database and mysql/configure-source-database, read
// 2026-11-25). Each failure is a MigrationJobVerificationError of the
// code the reference names for it; where none fits (a privilege, the
// binary log) it is ERROR_RDBMS (UNVERIFIED: Google publishes no table of
// which code each check answers).

type code = dmspb.MigrationJobVerificationError_ErrorCode

const (
	errConnection    = dmspb.MigrationJobVerificationError_CONNECTION_FAILURE
	errAuth          = dmspb.MigrationJobVerificationError_AUTHENTICATION_FAILURE
	errVersion       = dmspb.MigrationJobVerificationError_VERSION_INCOMPATIBILITY
	errTypes         = dmspb.MigrationJobVerificationError_CONNECTION_PROFILE_TYPES_INCOMPATIBILITY
	errNoPglogical   = dmspb.MigrationJobVerificationError_NO_PGLOGICAL_INSTALLED
	errWalLevel      = dmspb.MigrationJobVerificationError_INVALID_WAL_LEVEL
	errPreload       = dmspb.MigrationJobVerificationError_INVALID_SHARED_PRELOAD_LIBRARY
	errSlots         = dmspb.MigrationJobVerificationError_INSUFFICIENT_MAX_REPLICATION_SLOTS
	errSenders       = dmspb.MigrationJobVerificationError_INSUFFICIENT_MAX_WAL_SENDERS
	errWorkers       = dmspb.MigrationJobVerificationError_INSUFFICIENT_MAX_WORKER_PROCESSES
	errGTID          = dmspb.MigrationJobVerificationError_UNSUPPORTED_GTID_MODE
	errTableDef      = dmspb.MigrationJobVerificationError_UNSUPPORTED_TABLE_DEFINITION
	errLimitedTables = dmspb.MigrationJobVerificationError_TABLES_WITH_LIMITED_SUPPORT
	errRDBMS         = dmspb.MigrationJobVerificationError_ERROR_RDBMS
	errConflicting   = dmspb.MigrationJobVerificationError_EXISTING_CONFLICTING_DATABASES
)

// verify runs a job's checks; fresh is a job not started yet, whose
// destination must not hold the source's databases already.
func (s *Server) verify(ctx context.Context, jc jobContext, fresh bool) []*dmspb.MigrationJobVerificationError {
	srcEngine := jc.src.Engine
	dstMySQL := jc.target.MySQL()
	if (srcEngine == dmspb.DatabaseEngine_MYSQL) != dstMySQL || (!jc.target.CloudSQL && srcEngine != dmspb.DatabaseEngine_POSTGRESQL) {
		return []*dmspb.MigrationJobVerificationError{verificationError(errTypes,
			fmt.Sprintf("The source (%s) and the destination (%s) are not a supported pair", srcEngine, jc.target.Version),
			"A MySQL source migrates to Cloud SQL for MySQL; a PostgreSQL source to Cloud SQL for PostgreSQL or AlloyDB for PostgreSQL.")}
	}
	if srcEngine == dmspb.DatabaseEngine_MYSQL {
		return s.verifyMySQL(ctx, jc, fresh)
	}
	return s.verifyPostgres(ctx, jc, fresh)
}

// connFailure is a query's error as a verification error.
func connFailure(what string, err error) *dmspb.MigrationJobVerificationError {
	var ce *ConnError
	if errors.As(err, &ce) {
		if ce.Auth {
			return verificationError(errAuth, "The destination could not sign in to the source: "+what, ce.Msg)
		}
		return verificationError(errConnection, "The destination could not reach the source: "+what, ce.Msg)
	}
	return verificationError(errRDBMS, "The source answered an error: "+what, err.Error())
}

func (s *Server) verifyPostgres(ctx context.Context, jc jobContext, fresh bool) []*dmspb.MigrationJobVerificationError {
	var out []*dmspb.MigrationJobVerificationError
	b, t, src := s.opts.Backend, jc.target, jc.src
	rows, err := b.QuerySource(ctx, t, src, "postgres", `SELECT current_setting('server_version_num'), current_setting('wal_level'),
	  current_setting('max_replication_slots'), current_setting('max_wal_senders'), current_setting('max_worker_processes'),
	  current_setting('shared_preload_libraries'), r.rolsuper, r.rolreplication FROM pg_roles r WHERE r.rolname = current_user`)
	if err != nil {
		return append(out, connFailure(fmt.Sprintf("%s:%d as %s (database postgres)", src.Host, src.Port, src.User), err))
	}
	if len(rows) != 1 || len(rows[0]) != 8 {
		return append(out, verificationError(errRDBMS, "The source's settings could not be read", fmt.Sprint(rows)))
	}
	r := rows[0]
	num, _ := strconv.Atoi(r[0])
	srcMajor := num / 10000
	if num < 100000 {
		srcMajor = num / 100
	}
	dstMajor, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(t.Version, "POSTGRES_"), "_", 2)[0])
	if dstMajor != 0 && srcMajor > dstMajor {
		out = append(out, verificationError(errVersion,
			fmt.Sprintf("The source runs PostgreSQL %d, newer than the destination's %s", srcMajor, t.Version),
			"The destination's major version must be the source's or later."))
	}
	if r[1] != "logical" {
		out = append(out, verificationError(errWalLevel, "The source database 'wal_level' configuration must be equal to 'logical'.",
			"wal_level is "+r[1]+"; set it to logical and restart the server."))
	}
	native := nativeLogical(jc.job)
	if !native && !hasLibrary(r[5], "pglogical") {
		out = append(out, verificationError(errPreload, "The source's shared_preload_libraries must include pglogical.",
			"shared_preload_libraries is '"+r[5]+"'; add pglogical and restart the server, or migrate with native logical replication (postgresHomogeneousConfig.isNativeLogical)."))
	}
	if r[6] != "t" && r[7] != "t" {
		out = append(out, verificationError(errRDBMS, "The migration user "+src.User+" has no REPLICATION attribute.",
			"ALTER USER "+src.User+" WITH REPLICATION;"))
	}
	dbs, err := s.sourceDatabases(ctx, jc)
	if err != nil {
		return append(out, connFailure("list the source's databases", err))
	}
	if !hasDatabase(dbs, "postgres") {
		out = append(out, verificationError(errRDBMS, "The source has no postgres database.", "Database Migration Service needs the default postgres database."))
	}
	n := len(dbs)
	for _, x := range []struct {
		c    code
		name string
		v    string
	}{{errSlots, "max_replication_slots", r[2]}, {errSenders, "max_wal_senders", r[3]}, {errWorkers, "max_worker_processes", r[4]}} {
		if v, _ := strconv.Atoi(x.v); v < n {
			out = append(out, verificationError(x.c, fmt.Sprintf("The source's %s is %s; the migration needs at least %d, one for each database.", x.name, x.v, n),
				"Raise "+x.name+" and restart the server."))
		}
	}
	for _, db := range dbs {
		rows, err := b.QuerySource(ctx, t, src, db, `SELECT (SELECT count(*) FROM pg_extension WHERE extname = 'pglogical'),
		  has_database_privilege(current_database(), 'CREATE'),
		  count(*) FILTER (WHERE NOT pg_has_role(c.relowner, 'USAGE')),
		  count(*) FILTER (WHERE NOT has_table_privilege(c.oid, 'SELECT')),
		  coalesce(string_agg(format('%I.%I', n.nspname, c.relname), ', ') FILTER (WHERE c.relreplident IN ('d', 'n')
		    AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary)), ''),
		  (SELECT rolsuper FROM pg_roles WHERE rolname = current_user)
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pglogical')
		    AND n.nspname NOT LIKE 'pg\_%'`)
		if err != nil {
			out = append(out, connFailure("the database "+db, err))
			continue
		}
		if len(rows) != 1 || len(rows[0]) != 6 {
			out = append(out, verificationError(errRDBMS, "The database "+db+"'s tables could not be read", fmt.Sprint(rows)))
			continue
		}
		d := rows[0]
		if !native && d[0] == "0" {
			out = append(out, verificationError(errNoPglogical, "The pglogical extension is not installed in the database "+db+".",
				"CREATE EXTENSION IF NOT EXISTS pglogical; in every database but template0 and template1."))
		}
		if d[3] != "0" {
			out = append(out, verificationError(errRDBMS, fmt.Sprintf("The migration user %s cannot SELECT %s tables of the database %s.", src.User, d[3], db),
				"GRANT SELECT ON ALL TABLES IN SCHEMA ... TO "+src.User+";"))
		}
		if d[5] != "t" && (d[1] != "t" || d[2] != "0") {
			// CloudBurrow's own requirement: the copy publishes each
			// database's tables with PostgreSQL's logical replication,
			// which takes CREATE on the database and the tables' ownership.
			out = append(out, verificationError(errRDBMS, fmt.Sprintf("The migration user %s cannot publish the tables of the database %s.", src.User, db),
				"CloudBurrow replicates with PostgreSQL's built-in logical replication, which needs CREATE on the database and the tables owned by the migration user (or a superuser) (docs/gaps.md)."))
		}
		if d[4] != "" {
			out = append(out, verificationError(errLimitedTables, "Tables of the database "+db+" have no primary key: "+d[4],
				"Add a primary key, or set REPLICA IDENTITY FULL or USING INDEX, so their updates and deletes replicate (and the source keeps taking them)."))
		}
	}
	if fresh {
		out = append(out, s.conflicting(ctx, t, dbs)...)
	}
	return out
}

// conflicting reports the source's databases the destination already
// holds with tables of their own.
func (s *Server) conflicting(ctx context.Context, t Target, dbs []string) []*dmspb.MigrationJobVerificationError {
	rows, err := s.opts.Backend.QueryTarget(ctx, t, "postgres", `SELECT datname FROM pg_database WHERE NOT datistemplate`)
	if err != nil {
		return []*dmspb.MigrationJobVerificationError{verificationError(errRDBMS, "The destination's databases could not be read", err.Error())}
	}
	exists := map[string]bool{}
	for _, r := range rows {
		if len(r) > 0 {
			exists[r[0]] = true
		}
	}
	var out []*dmspb.MigrationJobVerificationError
	for _, db := range dbs {
		if !exists[db] {
			continue
		}
		rows, err := s.opts.Backend.QueryTarget(ctx, t, db, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_%'`)
		if err == nil && len(rows) == 1 && len(rows[0]) == 1 && rows[0][0] != "0" {
			out = append(out, verificationError(errConflicting, "The destination already has the database "+db+" with tables.",
				"Drop it from the destination, or migrate to an empty destination."))
		}
	}
	return out
}

func hasLibrary(list, lib string) bool {
	for _, l := range strings.Split(list, ",") {
		if strings.Trim(strings.TrimSpace(l), `"'`) == lib {
			return true
		}
	}
	return false
}

func hasDatabase(dbs []string, db string) bool {
	for _, d := range dbs {
		if d == db {
			return true
		}
	}
	return false
}

// mysqlSystem are the schemas a MySQL migration leaves out.
const mysqlSystem = `('mysql', 'sys', 'information_schema', 'performance_schema')`

func (s *Server) verifyMySQL(ctx context.Context, jc jobContext, fresh bool) []*dmspb.MigrationJobVerificationError {
	var out []*dmspb.MigrationJobVerificationError
	b, t, src := s.opts.Backend, jc.target, jc.src
	continuous := jc.job.GetType() == dmspb.MigrationJob_CONTINUOUS
	rows, err := b.QuerySource(ctx, t, src, "", `SELECT @@GLOBAL.version, @@GLOBAL.gtid_mode, @@GLOBAL.binlog_format, @@GLOBAL.log_bin, @@GLOBAL.server_id`)
	if err != nil {
		return append(out, connFailure(fmt.Sprintf("%s:%d as %s", src.Host, src.Port, src.User), err))
	}
	if len(rows) != 1 || len(rows[0]) != 5 {
		return append(out, verificationError(errRDBMS, "The source's settings could not be read", fmt.Sprint(rows)))
	}
	r := rows[0]
	if !mysqlAtMost(r[0], t.Version) {
		out = append(out, verificationError(errVersion, fmt.Sprintf("The source runs MySQL %s, newer than the destination's %s", r[0], t.Version),
			"The destination's version must be the source's or later."))
	}
	gtid := strings.ToUpper(r[1])
	switch {
	case gtid != "ON" && gtid != "OFF":
		// "GTID_MODE: Must be ON or OFF ... no other value is supported."
		out = append(out, verificationError(errGTID, "The source's gtid_mode is "+r[1]+"; it must be ON or OFF.", "Set gtid_mode to ON or OFF."))
	case continuous && gtid != "ON":
		out = append(out, verificationError(errGTID, "The source's gtid_mode is OFF; CloudBurrow's continuous MySQL migration replicates by GTID.",
			"Set gtid_mode=ON and enforce_gtid_consistency=ON on the source, or run a ONE_TIME job (docs/gaps.md)."))
	}
	if continuous {
		if r[3] != "1" {
			out = append(out, verificationError(errRDBMS, "The source's binary log is off (log_bin).", "Enable binary logging on the source."))
		}
		if strings.ToUpper(r[2]) != "ROW" {
			out = append(out, verificationError(errRDBMS, "The source's binlog_format is "+r[2]+"; it must be ROW.", "SET GLOBAL binlog_format = 'ROW';"))
		}
	}
	grants, err := b.QuerySource(ctx, t, src, "", "SHOW GRANTS")
	if err != nil {
		out = append(out, connFailure("read the migration user's grants", err))
	} else {
		var all strings.Builder
		for _, g := range grants {
			all.WriteString(strings.ToUpper(strings.Join(g, " ")) + "\n")
		}
		every := strings.Contains(all.String(), "GRANT ALL PRIVILEGES ON *.*")
		need := []string{"SELECT", "SHOW VIEW", "TRIGGER"}
		if gtid == "ON" {
			need = append(need, "RELOAD")
		}
		if continuous {
			need = append(need, "REPLICATION SLAVE", "REPLICATION CLIENT")
		}
		for _, p := range need {
			if !every && !strings.Contains(all.String(), p) {
				out = append(out, verificationError(errRDBMS, "The migration user "+src.User+" lacks "+p+" ON *.*.",
					"GRANT "+p+" ON *.* TO '"+src.User+"'@'%';"))
			}
		}
	}
	tables, err := b.QuerySource(ctx, t, src, "", `SELECT CONCAT(table_schema, '.', table_name, ' (', engine, ')') FROM information_schema.tables
	  WHERE table_type = 'BASE TABLE' AND engine <> 'InnoDB' AND table_schema NOT IN `+mysqlSystem)
	if err == nil && len(tables) > 0 {
		var names []string
		for _, tr := range tables {
			names = append(names, strings.Join(tr, " "))
		}
		out = append(out, verificationError(errTableDef, "Tables of the source do not use InnoDB: "+strings.Join(names, ", "),
			"All tables, except those in system databases, should use InnoDB."))
	}
	if fresh {
		schemas, err := b.QuerySource(ctx, t, src, "", `SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN `+mysqlSystem)
		if err == nil {
			for _, sc := range schemas {
				if len(sc) == 0 {
					continue
				}
				got, err := b.QueryTarget(ctx, t, "", fmt.Sprintf("SELECT count(*) FROM information_schema.tables WHERE table_schema = %s", quoteLiteral(sc[0])))
				if err == nil && len(got) == 1 && len(got[0]) == 1 && got[0][0] != "0" {
					out = append(out, verificationError(errConflicting, "The destination already has the database "+sc[0]+" with tables.",
						"Drop it from the destination, or migrate to an empty destination."))
				}
			}
		}
	}
	return out
}

// mysqlAtMost reports whether a source's version (8.0.36, 8.4.3-log) is
// at most a destination's (MYSQL_8_4).
func mysqlAtMost(version, dst string) bool {
	parts := strings.SplitN(strings.TrimPrefix(dst, "MYSQL_"), "_", 3)
	if len(parts) < 2 {
		return true
	}
	dmaj, _ := strconv.Atoi(parts[0])
	dmin, _ := strconv.Atoi(parts[1])
	v := strings.SplitN(version, ".", 3)
	if len(v) < 2 {
		return true
	}
	smaj, _ := strconv.Atoi(v[0])
	smin, _ := strconv.Atoi(v[1])
	return smaj < dmaj || smaj == dmaj && smin <= dmin
}
