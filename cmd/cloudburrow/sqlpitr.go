package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// Point-in-time recovery (#1330) of created instances, with the engines'
// own logs, kept on the server's volume beside its data directory:
//
//   - PostgreSQL archives each WAL segment (archive_command, set by
//     pgPITRArgs) into pgPITRDir/wal, and a base is a pg_basebackup in
//     pgPITRDir/base/<seconds>. A recovery copies the newest base taken
//     before the target into a scratch data directory, and runs a second
//     postgres on it, on a socket only, with restore_command and
//     recovery_target_time; once promoted at the target it is dumped with
//     pg_dump, as a backup is, and loaded into the clone.
//   - MySQL keeps its binary log (on in MySQL 8.4 by default), and a base is
//     a mysqldump --source-data in mysqlPITRDir/base/<seconds>.sql, which
//     records the binary log position it is consistent with. A recovery
//     loads the base into the clone, then mysqlbinlog's replay from that
//     position up to the first transaction whose commit timestamp (the
//     microsecond immediate_commit_timestamp) is after the target.
//
// A base's name is the first whole second after it was complete, by the
// server's clock, which is the earliest time it can recover to.

const (
	pgPITRDir    = "/var/lib/postgresql/data/cloudsql-pitr"
	mysqlPITRDir = "/var/lib/mysql/cloudsql-pitr"
	// pgRecoveryPort is the scratch server's, on its own socket directory.
	pgRecoveryPort = "5499"
)

// pgPITRArgs are the postgres arguments that archive the WAL.
func pgPITRArgs() []string {
	return []string{"-c", "archive_mode=on", "-c",
		"archive_command=mkdir -p " + pgPITRDir + "/wal && test ! -f " + pgPITRDir + "/wal/%f && cp %p " + pgPITRDir + "/wal/%f"}
}

// pitrLocks serialises the bases and recoveries of each server.
var pitrLocks sync.Map

func pitrLock(deployment string) func() {
	m, _ := pitrLocks.LoadOrStore(deployment, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (d *sqlDumper) sh(ctx context.Context, t sqladmin.DumpTarget, stdin io.Reader, stdout io.Writer, script string, args ...string) error {
	argv := append([]string{"sh", "-c", script, "pitr"}, args...)
	return d.exec(d.deployment(t))(ctx, stdin, stdout, argv...)
}

const mysqlPITRPrelude = `set -e
export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-}"
R=` + mysqlPITRDir + `
`

const pgPITRPrelude = `set -e
R=` + pgPITRDir + `
asuser=$(command -v su-exec || command -v gosu)
`

func (d *sqlDumper) PITRBase(ctx context.Context, t sqladmin.DumpTarget) error {
	defer pitrLock(d.deployment(t))()
	if t.MySQL {
		return d.sh(ctx, t, nil, io.Discard, mysqlPITRPrelude+`mkdir -p "$R/base"
dbs=$(mysql -uroot -N -B -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('mysql','sys','information_schema','performance_schema')")
if [ -n "$dbs" ]; then
  mysqldump -uroot --single-transaction --source-data=2 --set-gtid-purged=OFF --routines --triggers --events --no-tablespaces --databases $dbs > "$R/base/.tmp"
else
  mysql -uroot -N -B -e 'SHOW BINARY LOG STATUS' | awk '{printf "-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE='"'"'%s'"'"', SOURCE_LOG_POS=%s;\n", $1, $2}' > "$R/base/.tmp"
fi
grep -q 'SOURCE_LOG_FILE=' "$R/base/.tmp"
mv "$R/base/.tmp" "$R/base/$(( $(date -u +%s) + 1 )).sql"`)
	}
	return d.sh(ctx, t, nil, io.Discard, pgPITRPrelude+`mkdir -p "$R/base" "$R/wal"; chown postgres "$R" "$R/base" "$R/wal"
rm -rf "$R/base/.tmp"
"$asuser" postgres pg_basebackup -U postgres -D "$R/base/.tmp" -X fetch -c fast
mv "$R/base/.tmp" "$R/base/$(( $(date -u +%s) + 1 ))"`)
}

// bases are the target's bases' times, oldest first, and the server's
// clock now.
func (d *sqlDumper) bases(ctx context.Context, t sqladmin.DumpTarget) ([]int64, time.Time, error) {
	dir := pgPITRDir + "/base"
	if t.MySQL {
		dir = mysqlPITRDir + "/base"
	}
	var out bytes.Buffer
	if err := d.sh(ctx, t, nil, &out, `date -u +%s; ls "$1" 2>/dev/null || true`, dir); err != nil {
		return nil, time.Time{}, err
	}
	lines := strings.Fields(out.String())
	if len(lines) == 0 {
		return nil, time.Time{}, fmt.Errorf("no clock reading from the server")
	}
	now, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("the server's clock: %w", err)
	}
	var ids []int64
	for _, l := range lines[1:] {
		if n, err := strconv.ParseInt(strings.TrimSuffix(l, ".sql"), 10, 64); err == nil {
			ids = append(ids, n)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, time.Unix(now, 0), nil
}

func (d *sqlDumper) RecoveryWindow(ctx context.Context, t sqladmin.DumpTarget) (time.Time, time.Time, bool, error) {
	ids, now, err := d.bases(ctx, t)
	if err != nil || len(ids) == 0 {
		return time.Time{}, time.Time{}, false, err
	}
	return time.Unix(ids[0], 0), now, true, nil
}

// baseFor is the newest base complete by at.
func baseFor(ids []int64, at time.Time) (int64, bool) {
	var best int64
	found := false
	for _, id := range ids {
		if !time.Unix(id, 0).After(at) {
			best, found = id, true
		}
	}
	return best, found
}

func (d *sqlDumper) CopyAt(ctx context.Context, src, dst sqladmin.DumpTarget, at time.Time, dir string) error {
	defer pitrLock(d.deployment(src))()
	ids, _, err := d.bases(ctx, src)
	if err != nil {
		return err
	}
	id, ok := baseFor(ids, at)
	if !ok {
		return fmt.Errorf("no base was complete by %s", at.UTC().Format(time.RFC3339Nano))
	}
	if src.MySQL {
		return d.mysqlCopyAt(ctx, src, dst, at, id, dir)
	}
	return d.pgCopyAt(ctx, src, dst, at, id, dir)
}

func (d *sqlDumper) pgCopyAt(ctx context.Context, src, dst sqladmin.DumpTarget, at time.Time, id int64, dir string) error {
	target := at.UTC().Format("2006-01-02 15:04:05.999999") + "+00"
	// A commit after the target ends the replay there, and the WAL switch
	// archives the segment holding it.
	start := pgPITRPrelude + `W="$R/restore"
psql -U postgres -d template1 -AtXq -c 'SELECT txid_current()' >/dev/null
seg=$(psql -U postgres -d template1 -AtX -c 'SELECT pg_walfile_name(pg_switch_wal())')
i=0; until [ -f "$R/wal/$seg" ]; do i=$((i+1)); [ $i -lt 120 ] || { echo "WAL segment $seg was not archived" >&2; exit 1; }; sleep 0.5; done
rm -rf "$W"; cp -a "$R/base/$1" "$W"; rm -f "$W/postmaster.pid"; chown -R postgres "$W"; chmod 700 "$W"
cat >> "$W/postgresql.auto.conf" <<EOF
restore_command = 'cp $R/wal/%f %p'
recovery_target_time = '$2'
recovery_target_action = 'promote'
EOF
touch "$W/recovery.signal"
"$asuser" postgres pg_ctl -D "$W" -l "$W/recovery.log" -w -t 280 -o "-c port=` + pgRecoveryPort + ` -c listen_addresses='' -c unix_socket_directories=$W -c archive_mode=off" start >/dev/null || { tail -5 "$W/recovery.log" >&2; exit 1; }
i=0; until [ "$("$asuser" postgres psql -h "$W" -p ` + pgRecoveryPort + ` -U postgres -d template1 -AtX -c 'SELECT pg_is_in_recovery()')" = f ]; do
  i=$((i+1)); [ $i -lt 280 ] || { tail -5 "$W/recovery.log" >&2; exit 1; }; sleep 1
done`
	stop := pgPITRPrelude + `W="$R/restore"; [ -d "$W" ] && "$asuser" postgres pg_ctl -D "$W" -m fast -w stop >/dev/null 2>&1; rm -rf "$W"`
	defer func() { _ = d.sh(context.Background(), src, nil, io.Discard, stop) }()
	if err := d.sh(ctx, src, nil, io.Discard, start, strconv.FormatInt(id, 10), target); err != nil {
		return err
	}
	exec := d.exec(d.deployment(src))
	scratch := &postgresSnapshotter{superuser: "postgres", bare: true,
		exec: func(ctx context.Context, stdin io.Reader, stdout io.Writer, argv ...string) error {
			return exec(ctx, stdin, stdout, append([]string{"env", "PGHOST=" + pgPITRDir + "/restore", "PGPORT=" + pgRecoveryPort}, argv...)...)
		}}
	if err := scratch.Export(ctx, dirEntries(dir)); err != nil {
		return fmt.Errorf("dump the recovered server: %w", err)
	}
	return d.Load(ctx, dst, dir)
}

// mysqlStop finds where the replay stops: each binary log from the base's
// position on, and the position, in the last one, of the first
// transaction committed after the target ("" when none is).
const mysqlStop = mysqlPITRPrelude + `base="$R/base/$1.sql"; target=$2
file=$(sed -n "s/.*SOURCE_LOG_FILE='\([^']*\)'.*/\1/p" "$base" | head -1)
pos=$(sed -n "s/.*SOURCE_LOG_POS=\([0-9]*\).*/\1/p" "$base" | head -1)
dir=$(dirname "$(mysql -uroot -N -B -e 'SELECT @@log_bin_basename')")
mysql -uroot -N -B -e 'FLUSH BINARY LOGS'
started=
for f in $(mysql -uroot -N -B -e 'SHOW BINARY LOGS' | awk '{print $1}'); do
  [ "$f" = "$file" ] && started=1
  [ -n "$started" ] || continue
  from=4; [ "$f" = "$file" ] && from=$pos
  stop=$(mysqlbinlog --start-position="$from" "$dir/$f" | awk -v t="$target" '
    /^# at [0-9]+/ { at=$3 }
    /immediate_commit_timestamp=/ { match($0, /immediate_commit_timestamp=[0-9]+/); v=substr($0, RSTART+27, RLENGTH-27); if (v+0 > t+0) { print at; exit } }')
  echo "$f $from $stop"
  [ -z "$stop" ] || break
done
[ -n "$started" ] || { echo "the binary log $file of the base is gone" >&2; exit 1; }`

func (d *sqlDumper) mysqlCopyAt(ctx context.Context, src, dst sqladmin.DumpTarget, at time.Time, id int64, dir string) error {
	var plan bytes.Buffer
	if err := d.sh(ctx, src, nil, &plan, mysqlStop, strconv.FormatInt(id, 10), strconv.FormatInt(at.UnixMicro(), 10)); err != nil {
		return fmt.Errorf("read the binary log: %w", err)
	}
	baseFile := filepath.Join(dir, "base.sql")
	if err := d.toFile(ctx, src, baseFile, mysqlPITRPrelude+`cat "$R/base/$1.sql"`, strconv.FormatInt(id, 10)); err != nil {
		return err
	}
	replay := filepath.Join(dir, "replay.sql")
	f, err := os.Create(replay)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(plan.String()), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		args := []string{"--skip-gtids", "--start-position=" + fields[1]}
		if len(fields) == 3 {
			args = append(args, "--stop-position="+fields[2])
		}
		err = d.sh(ctx, src, nil, f, mysqlPITRPrelude+`dir=$(dirname "$(mysql -uroot -N -B -e 'SELECT @@log_bin_basename')")
f=$1; shift; exec mysqlbinlog "$@" "$dir/$f"`, append([]string{fields[0]}, args...)...)
		if err != nil {
			break
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("read the binary log: %w", err)
	}
	for _, p := range []string{baseFile, replay} {
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		err = d.sh(ctx, dst, in, io.Discard, `export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-}"; exec mysql -uroot`)
		_ = in.Close()
		if err != nil {
			return fmt.Errorf("load %s into %s: %w", filepath.Base(p), dst.Instance, err)
		}
	}
	return nil
}

func (d *sqlDumper) toFile(ctx context.Context, t sqladmin.DumpTarget, path, script string, args ...string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	err = d.sh(ctx, t, nil, f, script, args...)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
