package sqladmin

// Physical external syncs (#1570): startExternalSync with migrationType
// PHYSICAL, as Google documents a MySQL migration from a physical backup
// (docs.cloud.google.com/sql/docs/mysql/replication/custom-import-replication-from-external
// and the migrationType of SyncExternalSettingsRequest). The replica's data
// directory is replaced by a Percona XtraBackup backup of the source,
// prepared and copied back, and the replica then replicates from the source
// by GTID auto-positioning from the backup's GTID set
// (xtrabackup_binlog_info). The backup is the folder of files XtraBackup
// wrote, uploaded to Cloud Storage and named by the source representation
// instance's dumpFilePath; with none, and a source that is a CloudBurrow
// instance's server, the sync takes the backup itself.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// PhysicalBackup is the backup a physical sync restores.
type PhysicalBackup struct {
	// Files are the backup's files, by their path in its folder, which
	// Read returns. None means the sync takes the backup of the source.
	Files []string
	Read  func(ctx context.Context, file string) ([]byte, error)
}

// PhysicalSyncer is a Provisioner that restores physical backups.
type PhysicalSyncer interface {
	// PhysicalUnsupported is why spec's server cannot be given a physical
	// backup, or "" when it can.
	PhysicalUnsupported(spec Spec) string
	// PhysicalRestore replaces spec's server's data directory with the
	// prepared backup, and leaves the backup's GTID set for the
	// replication setup Start then runs (spec.External.PhysicalRestored).
	PhysicalRestore(ctx context.Context, spec Spec, b PhysicalBackup) error
}

// physicalFiles are the files every XtraBackup backup holds: they say what
// was backed up, to what log sequence number, and at which GTIDs.
var physicalFiles = []string{"xtrabackup_checkpoints", "xtrabackup_info", "xtrabackup_binlog_info"}

// physicalFolder is the backup folder dumpFilePath names, without the
// trailing slash, or "" when the sync takes the backup.
func physicalFolder(ext *ExternalSource) string {
	return strings.TrimSuffix(ext.DumpFile, "/")
}

// isPhysicalBackup says whether the dumpFilePath uri is the folder of an
// XtraBackup backup rather than a dump file: what a replica created with
// it waits for startExternalSync PHYSICAL to restore, instead of loading.
func (s *Server) isPhysicalBackup(ctx context.Context, uri string) bool {
	if s.transfers == nil || uri == "" {
		return false
	}
	bucket, object, err := parseGCSURI(strings.TrimSuffix(uri, "/") + "/x")
	if err != nil {
		return false
	}
	prefix := strings.TrimSuffix(object, "x")
	names, err := s.transfers.obj.List(ctx, bucket, prefix+"xtrabackup_checkpoints")
	return err == nil && len(names) > 0
}

// physicalBackup lists the backup folder ext's dumpFilePath names, and
// says what is wrong with it as sql#externalSyncSettingError values.
func (s *Server) physicalBackup(ctx context.Context, ext *ExternalSource) (PhysicalBackup, []any) {
	folder := physicalFolder(ext)
	if folder == "" {
		return PhysicalBackup{}, nil
	}
	if s.transfers == nil {
		return PhysicalBackup{}, []any{syncError("INSUFFICIENT_GCS_PERMISSIONS",
			"this server was started without the instance's Cloud Storage, so the backup in "+ext.DumpFile+" cannot be read")}
	}
	bucket, object, err := parseGCSURI(folder + "/x")
	if err != nil {
		return PhysicalBackup{}, []any{syncError("INVALID_FILE_INFO", "dumpFilePath "+ext.DumpFile+" is not gs://BUCKET/FOLDER")}
	}
	prefix := strings.TrimSuffix(object, "x")
	names, err := s.transfers.obj.List(ctx, bucket, prefix)
	if err != nil {
		return PhysicalBackup{}, []any{syncError("INSUFFICIENT_GCS_PERMISSIONS", fmt.Sprintf("list %s: %v", ext.DumpFile, err))}
	}
	b := PhysicalBackup{Read: func(ctx context.Context, file string) ([]byte, error) {
		return s.transfers.obj.Read(ctx, bucket, prefix+file)
	}}
	have := map[string]bool{}
	for _, n := range names {
		rel := strings.TrimPrefix(n, prefix)
		if rel == "" || strings.HasSuffix(rel, "/") {
			continue
		}
		b.Files = append(b.Files, rel)
		have[rel] = true
	}
	var errs []any
	for _, f := range physicalFiles {
		if !have[f] {
			errs = append(errs, syncError("INVALID_FILE_INFO",
				fmt.Sprintf("%s has no %s: dumpFilePath must name the Cloud Storage folder of a Percona XtraBackup backup of the source, "+
					"the files `xtrabackup --backup --target-dir` wrote", ext.DumpFile, f)))
		}
	}
	for _, f := range b.Files {
		if strings.HasSuffix(f, ".xbstream") || strings.HasSuffix(f, ".qp") {
			errs = append(errs, syncError("INVALID_FILE_INFO",
				fmt.Sprintf("%s holds %s: extract an xbstream archive, and decompress a qpress backup, before uploading the folder", ext.DumpFile, f)))
		}
	}
	return b, errs
}

// physicalInfoVersion is the server_version xtrabackup_info records.
var physicalInfoVersion = regexp.MustCompile(`(?m)^server_version\s*=\s*(\S+)`)

// versionMajor is "8.0" of "8.0.46" or "8.0.46-37", and of MYSQL_8_0.
func versionMajor(v string) string {
	v = strings.ReplaceAll(strings.TrimPrefix(v, "MYSQL_"), "_", ".")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// versionPatch is the patch number of "8.0.46-37", or -1.
func versionPatch(v string) int {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 3 {
		return -1
	}
	digits := strings.FieldsFunc(parts[2], func(r rune) bool { return r < '0' || r > '9' })
	if len(digits) == 0 {
		return -1
	}
	n, err := strconv.Atoi(digits[0])
	if err != nil {
		return -1
	}
	return n
}

// verifyPhysicalSync checks a physical sync of spec's replica from the
// source at addr, as verifyExternalSyncSettings does with migrationType
// PHYSICAL: the engine, the source's version, binary log and GTIDs, the
// replication user's privileges (and, when the sync takes the backup,
// XtraBackup's), the settings a physical copy cannot change, and the
// backup's files. installed is the replica's server version, "" if unknown.
func (s *Server) verifyPhysicalSync(ctx context.Context, spec Spec, addr, installed string, started, offline bool) []any {
	errs := []any{}
	ext := spec.External
	if started {
		return append(errs, syncError("REPLICA_ALREADY_SETUP", "replication to the replica has already been started"))
	}
	if !spec.MySQL() {
		return append(errs, syncError("UNSUPPORTED_MIGRATION_TYPE",
			"migrationType PHYSICAL is for a MySQL source; a PostgreSQL source is migrated LOGICAL"))
	}
	if offline {
		errs = append(errs, syncError("UNSUPPORTED_MIGRATION_TYPE",
			"migrationType PHYSICAL restores a backup and then replicates from the source; it has no OFFLINE syncMode"))
	}
	backup, ferrs := s.physicalBackup(ctx, ext)
	errs = append(errs, ferrs...)
	take := physicalFolder(ext) == ""
	if take && ext.Instance == "" {
		errs = append(errs, syncError("INVALID_FILE_INFO",
			"the source representation instance has no dumpFilePath: set it to the Cloud Storage folder of a Percona XtraBackup backup of the source "+
				"(CloudBurrow takes the backup itself only of a CloudBurrow instance's server)"))
	}
	if len(ferrs) == 0 && !take && backup.Read != nil {
		if info, err := backup.Read(ctx, "xtrabackup_info"); err == nil {
			if m := physicalInfoVersion.FindSubmatch(info); m != nil && versionMajor(string(m[1])) != versionMajor(spec.DatabaseVersion) {
				errs = append(errs, syncError("INCOMPATIBLE_DATABASE_VERSION",
					fmt.Sprintf("the backup is of MySQL %s and the replica is %s; they must be the same major version", m[1], spec.DatabaseVersion)))
			}
		}
	}

	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net = ext.User, ext.Password, "tcp"
	cfg.Addr = addr
	cfg.Timeout = 10 * time.Second
	cfg.AllowNativePasswords = true
	cfg.TLSConfig = "preferred"
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return append(errs, syncError("CONNECTION_FAILURE", err.Error()))
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var version, gtid, format string
	var logBin, pageSize, lowerCase int
	if err := db.QueryRowContext(ctx, "SELECT @@GLOBAL.version, @@GLOBAL.gtid_mode, @@GLOBAL.binlog_format, @@GLOBAL.log_bin, "+
		"@@GLOBAL.innodb_page_size, @@GLOBAL.lower_case_table_names").Scan(&version, &gtid, &format, &logBin, &pageSize, &lowerCase); err != nil {
		return append(errs, syncError("CONNECTION_FAILURE", fmt.Sprintf("connect to %s as %s: %v", cfg.Addr, ext.User, err)))
	}
	if versionMajor(version) != versionMajor(spec.DatabaseVersion) {
		errs = append(errs, syncError("INCOMPATIBLE_DATABASE_VERSION",
			fmt.Sprintf("the source runs MySQL %s and the replica %s; they must be the same major version", version, spec.DatabaseVersion)))
	} else if installed != "" && versionPatch(version) > versionPatch(installed) && versionPatch(installed) >= 0 {
		// The replica's server opens the source's data files: it cannot
		// be older than the server that wrote them.
		errs = append(errs, syncError("INCOMPATIBLE_DATABASE_MINOR_VERSION",
			fmt.Sprintf("the source runs MySQL %s and the replica's server %s; a physical copy needs a replica at least as new as its source", version, installed)))
	}
	if logBin != 1 {
		errs = append(errs, syncError("BINLOG_NOT_ENABLED", "the source's binary log is off (log_bin)"))
	}
	if gtid != "ON" {
		errs = append(errs, syncError("UNSUPPORTED_GTID_MODE", "the source's gtid_mode is "+gtid+"; a physical migration replicates by GTID and needs ON"))
	}
	if format != "ROW" {
		errs = append(errs, syncError("UNSUPPORTED_BINLOG_FORMAT", "the source's binlog_format is "+format+"; it must be ROW"))
	}
	if pageSize != 16384 {
		errs = append(errs, syncError("UNSUPPORTED_DATABASE_SETTINGS",
			fmt.Sprintf("the source's innodb_page_size is %d; a physical copy keeps it, and a Cloud SQL server's is 16384", pageSize)))
	}
	if lowerCase != 0 {
		errs = append(errs, syncError("UNSUPPORTED_DATABASE_SETTINGS",
			fmt.Sprintf("the source's lower_case_table_names is %d; a physical copy keeps it, and the replica's is 0", lowerCase)))
	}
	if rows, err := db.QueryContext(ctx, "SELECT CONCAT(table_schema, '.', table_name), engine FROM information_schema.tables "+
		"WHERE table_type = 'BASE TABLE' AND engine <> 'InnoDB' AND table_schema NOT IN ('mysql','sys','information_schema','performance_schema') "+
		"ORDER BY 1 LIMIT 20"); err == nil {
		for rows.Next() {
			var t, engine string
			if rows.Scan(&t, &engine) == nil {
				errs = append(errs, syncError("UNSUPPORTED_STORAGE_ENGINE",
					fmt.Sprintf("table %s uses %s; a physical migration copies InnoDB tables only", t, engine)))
			}
		}
		rows.Close()
	}

	rows, err := db.QueryContext(ctx, "SHOW GRANTS")
	if err != nil {
		return append(errs, syncError("INSUFFICIENT_PRIVILEGE", err.Error()))
	}
	var grants strings.Builder
	for rows.Next() {
		var g string
		_ = rows.Scan(&g)
		grants.WriteString(strings.ToUpper(g) + "\n")
	}
	rows.Close()
	all := strings.Contains(grants.String(), "GRANT ALL PRIVILEGES ON *.*")
	// SELECT, so verification sees every table's engine.
	need := []string{"REPLICATION SLAVE", "REPLICATION CLIENT", "SELECT"}
	if take {
		// What XtraBackup needs of the user it backs the source up as.
		need = append(need, "RELOAD", "PROCESS", "BACKUP_ADMIN")
	}
	for _, p := range need {
		if !all && !strings.Contains(grants.String(), p) {
			errs = append(errs, syncError("INSUFFICIENT_PRIVILEGE", "the replication user "+ext.User+" lacks "+p+" ON *.*"))
		}
	}
	if take {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM performance_schema.log_status").Scan(&n); err != nil {
			errs = append(errs, syncError("INSUFFICIENT_PRIVILEGE",
				"the replication user "+ext.User+" cannot read performance_schema.log_status, which XtraBackup reads: grant it SELECT on it"))
		}
	}
	return errs
}

// physicalUnsupported is why the replica cannot be given a physical
// backup here, as a 501, or nil.
func (s *Server) physicalUnsupported(spec Spec) error {
	ps, ok := s.writes.prov.(PhysicalSyncer)
	if !ok {
		return unimplemented("migrationType PHYSICAL", "this server's instances cannot be given a physical backup")
	}
	if why := ps.PhysicalUnsupported(spec); why != "" {
		return unimplemented("migrationType PHYSICAL", why)
	}
	if physicalFolder(spec.External) != "" && s.transfers == nil {
		return s.noTransfers("a physical sync from dumpFilePath")
	}
	return nil
}

// restorePhysical restores the replica's physical backup once, before
// Start sets up its replication from the backup's GTIDs.
func (s *Server) restorePhysical(ctx context.Context, name string, spec Spec) (Spec, error) {
	ext := spec.External
	if ext == nil || !ext.Physical || ext.PhysicalRestored {
		return spec, nil
	}
	ps, ok := s.writes.prov.(PhysicalSyncer)
	if !ok {
		return spec, unimplemented("migrationType PHYSICAL", "this server's instances cannot be given a physical backup")
	}
	backup, errs := s.physicalBackup(ctx, ext)
	if len(errs) > 0 {
		m := errs[0].(map[string]any)
		return spec, Errorf(http.StatusBadRequest, "%s: %s", m["type"], m["detail"])
	}
	if err := ps.PhysicalRestore(ctx, spec, backup); err != nil {
		return spec, fmt.Errorf("restore the physical backup: %w", err)
	}
	s.emu.Lock()
	c := s.writes.byName[name]
	if c == nil {
		s.emu.Unlock()
		return spec, fmt.Errorf("instance %s is gone", name)
	}
	next := *c.spec.External
	next.PhysicalRestored = true
	c.spec.External = &next
	spec = c.spec
	err := s.saveLocked()
	s.emu.Unlock()
	return spec, err
}

// installedVersion is c's MySQL server's version, 8.0.46, or "" when it
// does not answer.
func (s *Server) installedVersion(ctx context.Context, c *created) string {
	s.emu.RLock()
	e := c.engine
	s.emu.RUnlock()
	if e == nil {
		return ""
	}
	v, err := e.InstalledVersion(ctx)
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(strings.TrimPrefix(v, "MYSQL_"), "_", ".") // 8.0.46
}
