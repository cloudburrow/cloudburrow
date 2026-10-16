package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
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

var _ sqladmin.PITRDumper = (*sqlDumper)(nil)

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
` + mysqlRootEnv + `R=` + mysqlPITRDir + `
`

const pgPITRPrelude = `set -e
R=` + pgPITRDir + `
asuser=$(command -v su-exec || command -v gosu)
`

func (d *sqlDumper) PITRBase(ctx context.Context, t sqladmin.DumpTarget) error {
	defer pitrLock(d.deployment(t))()
	if t.MySQL {
		return d.sh(ctx, t, nil, io.Discard, mysqlPITRPrelude+`mkdir -p "$R/base"
dbs=$(mysql $MYSQL_LOCAL -N -B -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('mysql','sys','information_schema','performance_schema')")
if [ -n "$dbs" ]; then
  mysqldump $MYSQL_LOCAL --single-transaction --source-data=2 --set-gtid-purged=OFF --routines --triggers --events --no-tablespaces --databases $dbs > "$R/base/.tmp"
else
  mysql $MYSQL_LOCAL -N -B -e 'SHOW BINARY LOG STATUS' | awk '{printf "-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE='"'"'%s'"'"', SOURCE_LOG_POS=%s;\n", $1, $2}' > "$R/base/.tmp"
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
	// The clock to the microsecond: busybox's date has no %N.
	clock := `date -u +%s.%N`
	if !t.MySQL {
		clock = `psql -U postgres -d template1 -AtX -c "SELECT to_char(extract(epoch FROM clock_timestamp()), 'FM9999999999.000000')"`
	}
	var out bytes.Buffer
	if err := d.sh(ctx, t, nil, &out, clock+`; ls "$1" 2>/dev/null || true`, dir); err != nil {
		return nil, time.Time{}, err
	}
	lines := strings.Fields(out.String())
	if len(lines) == 0 {
		return nil, time.Time{}, fmt.Errorf("no clock reading from the server")
	}
	now, err := parseEpoch(lines[0])
	if err != nil {
		return nil, time.Time{}, err
	}
	var ids []int64
	for _, l := range lines[1:] {
		if n, err := strconv.ParseInt(strings.TrimSuffix(l, ".sql"), 10, 64); err == nil {
			ids = append(ids, n)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, now, nil
}

// parseEpoch parses seconds since the epoch with a fraction, exactly.
func parseEpoch(s string) (time.Time, error) {
	secs, frac, _ := strings.Cut(s, ".")
	sec, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("the server's clock %q: %w", s, err)
	}
	frac = (frac + "000000000")[:9]
	ns, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("the server's clock %q: %w", s, err)
	}
	return time.Unix(sec, ns), nil
}

func (d *sqlDumper) RecoveryWindow(ctx context.Context, t sqladmin.DumpTarget) (time.Time, time.Time, bool, error) {
	ids, now, err := d.bases(ctx, t)
	if err != nil || len(ids) == 0 {
		return time.Time{}, time.Time{}, false, err
	}
	return time.Unix(ids[0], 0), now, true, nil
}

// Prune drops the bases older than the newest one complete by before, and
// the log only they needed: PostgreSQL's archived WAL segments before the
// kept base's start (pg_archivecleanup, with its backup_label), MySQL's
// binary logs before the kept base's file (PURGE BINARY LOGS TO).
func (d *sqlDumper) Prune(ctx context.Context, t sqladmin.DumpTarget, before time.Time) error {
	defer pitrLock(d.deployment(t))()
	ids, _, err := d.bases(ctx, t)
	if err != nil {
		return err
	}
	keep, ok := baseFor(ids, before)
	if !ok || keep == ids[0] {
		return nil
	}
	id := strconv.FormatInt(keep, 10)
	if t.MySQL {
		return d.sh(ctx, t, nil, io.Discard, mysqlPITRPrelude+`cd "$R/base"
for b in *.sql; do n=${b%.sql}; [ "$n" -lt "$1" ] 2>/dev/null && rm -f "$b"; done
f=$(sed -n "s/.*SOURCE_LOG_FILE='\([^']*\)'.*/\1/p" "$1.sql" | head -1)
[ -z "$f" ] || mysql -uroot -e "PURGE BINARY LOGS TO '$f'"`, id)
	}
	return d.sh(ctx, t, nil, io.Discard, pgPITRPrelude+`cd "$R/base"
for b in *; do [ "$b" -lt "$1" ] 2>/dev/null && rm -rf "$b"; done
seg=$(sed -n 's/^START WAL LOCATION: .*(file \([0-9A-F]*\))$/\1/p' "$1/backup_label")
[ -z "$seg" ] || pg_archivecleanup "$R/wal" "$seg"`, id)
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

// mysqlPITRUser is the replication user a MySQL recovery's clone
// replicates the source's binary log as, for the length of the recovery.
const mysqlPITRUser = "cloudsqlpitr"

// mysqlRecoverySource readies the source for a recovery: the replication
// user, whose statements commit after the target, and a binary log
// rotation, so every transaction up to now is in a closed file. It prints
// the base's binary log file and position, then each binary log's name.
const mysqlRecoverySource = mysqlPITRPrelude + `base="$R/base/$1.sql"
mysql $MYSQL_LOCAL -e "CREATE USER IF NOT EXISTS '` + mysqlPITRUser + `'@'%' IDENTIFIED BY '$2'; ALTER USER '` + mysqlPITRUser + `'@'%' IDENTIFIED BY '$2'; GRANT REPLICATION SLAVE ON *.* TO '` + mysqlPITRUser + `'@'%'; FLUSH BINARY LOGS"
sed -n "s/.*SOURCE_LOG_FILE='\([^']*\)', SOURCE_LOG_POS=\([0-9]*\).*/\1 \2/p" "$base" | head -1
dirname "$(mysql $MYSQL_LOCAL -N -B -e 'SELECT @@log_bin_basename')"
mysql $MYSQL_LOCAL -N -B -e 'SHOW BINARY LOGS' | awk '{print $1}'`

// mysqlRecoveryReplay replicates the source's binary log into the clone
// from the base's position to the stop position, with MySQL's own
// applier, and waits for it to stop there.
const mysqlRecoveryReplay = `set -e
` + mysqlRootEnv + `q() { mysql $MYSQL_LOCAL -N -B -e "$1"; }
q "SET GLOBAL server_id = $8"
q "CHANGE REPLICATION SOURCE TO SOURCE_HOST = '$1', SOURCE_PORT = $2, SOURCE_USER = '` + mysqlPITRUser + `', SOURCE_PASSWORD = '$3', SOURCE_LOG_FILE = '$4', SOURCE_LOG_POS = $5, SOURCE_SSL = 1, SOURCE_AUTO_POSITION = 0"
q "START REPLICA UNTIL SOURCE_LOG_FILE = '$6', SOURCE_LOG_POS = $7"
i=0
while :; do
  st=$(mysql $MYSQL_LOCAL -e 'SHOW REPLICA STATUS\G')
  sql=$(echo "$st" | sed -n 's/^ *Replica_SQL_Running: //p')
  err=$(echo "$st" | sed -n 's/^ *Last_Error: //p')
  ioerr=$(echo "$st" | sed -n 's/^ *Last_IO_Error: //p')
  [ -z "$err" ] || { q "STOP REPLICA; RESET REPLICA ALL"; echo "replay: $err" >&2; exit 1; }
  if [ "$sql" = No ]; then
    file=$(echo "$st" | sed -n 's/^ *Relay_Source_Log_File: //p'); pos=$(echo "$st" | sed -n 's/^ *Exec_Source_Log_Pos: //p')
    q "STOP REPLICA; RESET REPLICA ALL"
    [ "$file" = "$6" ] && [ "$pos" = "$7" ] || { echo "replay stopped at $file:$pos, not $6:$7 $ioerr" >&2; exit 1; }
    exit 0
  fi
  i=$((i+1)); [ $i -lt 600 ] || { q "STOP REPLICA; RESET REPLICA ALL"; echo "replay did not reach $6:$7: $ioerr" >&2; exit 1; }
  sleep 0.5
done`

func (d *sqlDumper) mysqlCopyAt(ctx context.Context, src, dst sqladmin.DumpTarget, at time.Time, id int64, dir string) error {
	if !src.Created {
		return fmt.Errorf("only a created instance keeps its binary log for recovery")
	}
	password := randomHex(12)
	defer func() {
		_ = d.sh(context.Background(), src, nil, io.Discard, mysqlPITRPrelude+`mysql $MYSQL_LOCAL -e "DROP USER IF EXISTS '`+mysqlPITRUser+`'@'%'"`)
	}()
	var info bytes.Buffer
	if err := d.sh(ctx, src, nil, &info, mysqlRecoverySource, strconv.FormatInt(id, 10), password); err != nil {
		return fmt.Errorf("ready the source: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(info.String()), "\n")
	if len(lines) < 3 {
		return fmt.Errorf("ready the source: %q", info.String())
	}
	baseFile, basePosS, _ := strings.Cut(lines[0], " ")
	basePos, err := strconv.ParseInt(basePosS, 10, 64)
	if err != nil {
		return fmt.Errorf("the base's binary log position %q: %w", lines[0], err)
	}
	logDir, logs := lines[1], lines[2:]
	stopFile, stopPos, err := d.mysqlStopAt(ctx, src, logDir, logs, baseFile, basePos, at, dir)
	if err != nil {
		return err
	}
	// The base, then the transactions up to the target.
	baseDump := filepath.Join(dir, "base.sql")
	if err := d.toFile(ctx, src, baseDump, mysqlPITRPrelude+`cat "$R/base/$1.sql"`, strconv.FormatInt(id, 10)); err != nil {
		return err
	}
	in, err := os.Open(baseDump)
	if err != nil {
		return err
	}
	err = d.sh(ctx, dst, in, io.Discard, mysqlRootEnv+`exec mysql $MYSQL_LOCAL`)
	_ = in.Close()
	if err != nil {
		return fmt.Errorf("load the base into %s: %w", dst.Instance, err)
	}
	if stopFile == baseFile && stopPos == basePos {
		return nil // nothing committed between the base and the target
	}
	serverID := 2 + crc32.ChecksumIEEE([]byte(dst.Instance))%1000000
	if err := d.sh(ctx, dst, nil, io.Discard, mysqlRecoveryReplay, d.deployment(src), strconv.Itoa(components.CloudSQLMySQLPort),
		password, baseFile, strconv.FormatInt(basePos, 10), stopFile, strconv.FormatInt(stopPos, 10), fmt.Sprint(serverID)); err != nil {
		return fmt.Errorf("replay the binary log into %s: %w", dst.Instance, err)
	}
	return nil
}

// mysqlStopAt reads the source's binary logs from the base's position on
// and returns where the first transaction committed after at starts.
func (d *sqlDumper) mysqlStopAt(ctx context.Context, src sqladmin.DumpTarget, logDir string, logs []string,
	baseFile string, basePos int64, at time.Time, dir string) (string, int64, error) {
	started := false
	for _, name := range logs {
		if name == baseFile {
			started = true
		}
		if !started {
			continue
		}
		from := int64(4)
		if name == baseFile {
			from = basePos
		}
		path := filepath.Join(dir, "binlog")
		if err := d.toFile(ctx, src, path, `exec cat "$1/$2"`, logDir, name); err != nil {
			return "", 0, fmt.Errorf("read the binary log %s: %w", name, err)
		}
		raw, err := os.ReadFile(path)
		_ = os.Remove(path)
		if err != nil {
			return "", 0, err
		}
		pos, found, err := firstCommitAfter(raw, from, at)
		if err != nil {
			return "", 0, fmt.Errorf("the binary log %s: %w", name, err)
		}
		if found {
			return name, pos, nil
		}
	}
	if !started {
		return "", 0, fmt.Errorf("the binary log %s of the base is gone", baseFile)
	}
	return "", 0, fmt.Errorf("no transaction after %s is in the binary log", at.UTC().Format(time.RFC3339Nano))
}

// Binary log event types that start a transaction (MySQL's
// binlog_event.h): each carries the transaction's commit timestamp.
const (
	binlogGTIDEvent          = 33
	binlogAnonymousGTIDEvent = 34
	binlogHeaderLen          = 19
)

// firstCommitAfter scans a binary log file from offset from and returns
// the offset of the first (anonymous) GTID event whose
// immediate_commit_timestamp, in microseconds, is after at.
func firstCommitAfter(raw []byte, from int64, at time.Time) (int64, bool, error) {
	if len(raw) < 4 || string(raw[:4]) != "\xfebin" {
		return 0, false, fmt.Errorf("not a binary log")
	}
	target := at.UnixMicro()
	off := from
	for off+binlogHeaderLen <= int64(len(raw)) {
		h := raw[off:]
		typ := h[4]
		size := int64(binary.LittleEndian.Uint32(h[9:13]))
		if size < binlogHeaderLen || off+size > int64(len(raw)) {
			break // the event being written
		}
		if typ == binlogGTIDEvent || typ == binlogAnonymousGTIDEvent {
			// flags 1, sid 16, gno 8, logical timestamp type 1,
			// last_committed 8, sequence_number 8, then the commit time
			// in 7 bytes, whose top bit says the original follows.
			body := h[binlogHeaderLen:size]
			const at = 1 + 16 + 8 + 1 + 8 + 8
			if len(body) < at+7 {
				return 0, false, fmt.Errorf("a GTID event at %d has no commit timestamp", off)
			}
			var b [8]byte
			copy(b[:7], body[at:at+7])
			ts := int64(binary.LittleEndian.Uint64(b[:]) &^ (1 << 55))
			if ts > target {
				return off, true, nil
			}
		}
		off += size
	}
	return 0, false, nil
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
