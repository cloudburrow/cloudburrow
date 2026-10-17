package sqladmin

// External primaries (#1501): replication from a MySQL server outside
// Cloud SQL, as Google documents it for a managed import
// (docs.cloud.google.com/sql/docs/mysql/replication/configure-replication-from-external).
// A source representation instance (instanceType ON_PREMISES_INSTANCE) is a
// record of the outside server, its onPremisesConfiguration; no server runs
// for it. A replica created with masterInstanceName naming it runs as a
// MySQL server of its own, idle until verifyExternalSyncSettings checks the
// source and startExternalSync dumps the source's databases into it and
// starts GTID replication from it; promoteReplica ends the migration.

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// OnPremises is a source representation instance's onPremisesConfiguration.
// Password and ClientKey are never answered.
type OnPremises struct {
	HostPort          string `json:"hostPort"`
	Username          string `json:"username,omitempty"`
	Password          string `json:"password,omitempty"`
	DumpFilePath      string `json:"dumpFilePath,omitempty"`
	CaCertificate     string `json:"caCertificate,omitempty"`
	ClientCertificate string `json:"clientCertificate,omitempty"`
	ClientKey         string `json:"clientKey,omitempty"`
	// SelectedObjects are the databases a PostgreSQL source's sync
	// migrates (#1524); none is every database but the templates.
	SelectedObjects []SelectedObject `json:"selectedObjects,omitempty"`
}

// SelectedObject is one of onPremisesConfiguration.selectedObjects.
type SelectedObject struct {
	Database string `json:"database"`
}

// SourceRep is a source representation instance.
type SourceRep struct {
	Name            string     `json:"name"`
	Project         string     `json:"project"`
	DatabaseVersion string     `json:"databaseVersion"`
	Region          string     `json:"region,omitempty"`
	CreateTime      string     `json:"createTime"`
	OnPremises      OnPremises `json:"onPremisesConfiguration"`
}

// ExternalSource is the outside server an external replica replicates
// from: Host and Port as the replica's server dials them, and the
// replication user. Instance, when set, is the CloudBurrow instance whose
// address hostPort is, which the replica's server reaches by its Service
// in the cluster instead (InstanceUp: a server `up` runs).
type ExternalSource struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	Password   string `json:"password,omitempty"`
	Instance   string `json:"instance,omitempty"`
	InstanceUp bool   `json:"instanceUp,omitempty"`
	// Databases are a PostgreSQL source's selectedObjects (#1524).
	Databases []string `json:"databases,omitempty"`
	// DumpFile is the dump in Cloud Storage a MySQL replica is seeded
	// from when it is created (#1524): its source's or its own
	// dumpFilePath, written with the source's GTIDs as purged.
	DumpFile string `json:"dumpFile,omitempty"`
	// SyncMode is startExternalSync's: "" (ONLINE), a dump and then
	// replication, or OFFLINE, the dump alone (#1524).
	SyncMode string `json:"syncMode,omitempty"`
}

// instanceServicePrefix and upMySQLService are the Services a created
// instance's server and the MySQL server `up` runs are reached by in the
// cluster (internal/components' SQLInstancePrefix and cloudsql-mysql).
const (
	instanceServicePrefix = "sqlinst-"
	upMySQLService        = "cloudsql-mysql"
)

// upPostgresService is the Service of the PostgreSQL server `up` runs.
const upPostgresService = "cloudsql"

func (w *instances) sourcesPath() string {
	if w.path == "" {
		return ""
	}
	return strings.TrimSuffix(w.path, filepath.Ext(w.path)) + "-sources.json"
}

// restoreSources reads the recorded source representation instances.
// Called with emu held.
func (s *Server) restoreSourcesLocked() {
	w := s.writes
	p := w.sourcesPath()
	if p == "" {
		return
	}
	var reps []SourceRep
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &reps)
	}
	for i := range reps {
		if s.engineLocked(reps[i].Name) == nil {
			w.sources[reps[i].Name] = &reps[i]
		}
	}
}

func (s *Server) saveSourcesLocked() error {
	w := s.writes
	p := w.sourcesPath()
	if p == "" {
		return nil
	}
	reps := []SourceRep{}
	for _, r := range w.sources {
		reps = append(reps, *r)
	}
	sort.Slice(reps, func(i, j int) bool { return reps[i].Name < reps[j].Name })
	b, err := json.MarshalIndent(reps, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// sourceNamed is the source representation instance name in project.
func (s *Server) sourceNamed(project, name string) *SourceRep {
	s.emu.RLock()
	defer s.emu.RUnlock()
	if s.writes == nil {
		return nil
	}
	if r := s.writes.sources[name]; r != nil && r.Project == project {
		return r
	}
	return nil
}

// insertSource is instances.insert of a source representation instance.
func (s *Server) insertSource(base, project string, b instanceBody) (any, error) {
	if b.InstanceType != "" && b.InstanceType != "ON_PREMISES_INSTANCE" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: an instance with onPremisesConfiguration must have instanceType ON_PREMISES_INSTANCE.")
	}
	if b.OnPremisesConfiguration == nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: an ON_PREMISES_INSTANCE needs onPremisesConfiguration.")
	}
	if b.MasterInstanceName != "" || b.ReplicaConfiguration != nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: a source representation instance has no masterInstanceName or replicaConfiguration.")
	}
	var op OnPremises
	if err := json.Unmarshal(*b.OnPremisesConfiguration, &op); err != nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid JSON payload received: onPremisesConfiguration: %v", err)
	}
	if b.DatabaseVersion == "" {
		return nil, Errorf(http.StatusBadRequest, "Missing parameter: databaseVersion.")
	}
	switch {
	case strings.HasPrefix(b.DatabaseVersion, "POSTGRES_"):
		if op.DumpFilePath != "" {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: dumpFilePath is for a MySQL source; a PostgreSQL source is seeded by a managed import.")
		}
	case strings.HasPrefix(b.DatabaseVersion, "SQLSERVER_"):
		return nil, unimplemented("databaseVersion "+b.DatabaseVersion, sqlServerVersionWhy)
	case !strings.HasPrefix(b.DatabaseVersion, "MYSQL_"):
		return nil, Errorf(http.StatusBadRequest, "Invalid request: Invalid value for databaseVersion: %s.", b.DatabaseVersion)
	}
	if op.DumpFilePath != "" {
		if _, _, err := parseGCSURI(op.DumpFilePath); err != nil {
			return nil, err
		}
	}
	if _, _, err := hostPort(op.HostPort); err != nil {
		return nil, err
	}
	rep := &SourceRep{Name: b.Name, Project: project, DatabaseVersion: b.DatabaseVersion, OnPremises: op,
		CreateTime: time.Now().UTC().Format(time.RFC3339Nano)}
	if b.Region != "" {
		if !regionRE.MatchString(b.Region) {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: Invalid value for region: %s.", b.Region)
		}
		if b.Region != Region {
			rep.Region = b.Region
		}
	}
	s.emu.Lock()
	if s.engineLocked(b.Name) != nil || s.writes.sources[b.Name] != nil {
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "The Cloud SQL instance already exists.")
	}
	s.writes.sources[b.Name] = rep
	err := s.saveSourcesLocked()
	s.emu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("sqladmin: keep instance %s: %w", b.Name, err)
	}
	return s.done(base, project, namedEngine(b.Name), "CREATE", b.Name), nil
}

func hostPort(v string) (string, int, error) {
	h, p, err := net.SplitHostPort(v)
	if err == nil {
		var n int
		n, err = strconv.Atoi(p)
		if err == nil && (n <= 0 || n > 65535 || h == "") {
			err = fmt.Errorf("bad port or host")
		}
		if err == nil {
			return h, n, nil
		}
	}
	return "", 0, Errorf(http.StatusBadRequest, "Invalid request: onPremisesConfiguration.hostPort %q is not HOST:PORT.", v)
}

// sourceJSON is a source representation instance as Cloud SQL answers it.
func (s *Server) sourceJSON(base string, r *SourceRep) map[string]any {
	region := Region
	if r.Region != "" {
		region = r.Region
	}
	op := map[string]any{"kind": "sql#onPremisesConfiguration", "hostPort": r.OnPremises.HostPort}
	for k, v := range map[string]string{"username": r.OnPremises.Username, "caCertificate": r.OnPremises.CaCertificate,
		"clientCertificate": r.OnPremises.ClientCertificate, "dumpFilePath": r.OnPremises.DumpFilePath} {
		if v != "" {
			op[k] = v
		}
	}
	if len(r.OnPremises.SelectedObjects) > 0 {
		op["selectedObjects"] = r.OnPremises.SelectedObjects
	}
	in := map[string]any{
		"kind": "sql#instance", "name": r.Name, "project": r.Project, "databaseVersion": r.DatabaseVersion,
		"state": "RUNNABLE", "region": region, "backendType": "EXTERNAL", "instanceType": "ON_PREMISES_INSTANCE",
		"createTime": r.CreateTime, "selfLink": base + "projects/" + r.Project + "/instances/" + r.Name,
		"onPremisesConfiguration": op,
	}
	s.emu.RLock()
	rs := s.replicaNamesLocked(r.Project, r.Name)
	s.emu.RUnlock()
	if len(rs) > 0 {
		in["replicaNames"] = rs
	}
	return in
}

// sourceRoute answers the methods on a source representation instance:
// get and delete; it has no server for the rest.
func (s *Server) sourceRoute(r *http.Request, base, project string, rep *SourceRep, rest []string, verb string) (string, any, error) {
	if len(rest) == 2 && verb == "" {
		switch r.Method {
		case http.MethodGet:
			return "sql.instances.get", s.sourceJSON(base, rep), nil
		case http.MethodDelete:
			s.emu.Lock()
			if rs := s.replicaNamesLocked(project, rep.Name); len(rs) > 0 {
				s.emu.Unlock()
				return "sql.instances.delete", nil, Errorf(http.StatusBadRequest,
					"The instance has read replicas (%s). Delete or promote the replicas first.", strings.Join(rs, ", "))
			}
			delete(s.writes.sources, rep.Name)
			err := s.saveSourcesLocked()
			s.emu.Unlock()
			if err != nil {
				return "sql.instances.delete", nil, err
			}
			return "sql.instances.delete", s.done(base, project, namedEngine(rep.Name), "DELETE", rep.Name), nil
		case http.MethodPatch, http.MethodPut:
			return "sql.instances.patch", nil, Errorf(http.StatusBadRequest,
				"Invalid request: a source representation instance (ON_PREMISES_INSTANCE) is not patched; delete it and create it again.")
		}
	}
	what := verb
	if what == "" && len(rest) > 2 {
		what = rest[2]
	}
	return "sql.instances." + what, nil, Errorf(http.StatusBadRequest,
		"Invalid request: %s is not supported for a source representation instance (ON_PREMISES_INSTANCE): it describes a server outside Cloud SQL.", what)
}

// prepareExternalReplica fills a replica's spec from the source
// representation instance it replicates from.
func (s *Server) prepareExternalReplica(b instanceBody, rep *SourceRep, spec *Spec) error {
	if spec.DatabaseVersion == "" {
		spec.DatabaseVersion = rep.DatabaseVersion
	}
	pg := strings.HasPrefix(rep.DatabaseVersion, "POSTGRES_")
	if pg != strings.HasPrefix(spec.DatabaseVersion, "POSTGRES_") {
		return Errorf(http.StatusBadRequest, "Invalid request: the replica's databaseVersion %s is not the engine of its source, %s.",
			spec.DatabaseVersion, rep.DatabaseVersion)
	}
	if !SupportedVersions[spec.DatabaseVersion] {
		return unimplemented("databaseVersion "+spec.DatabaseVersion, unservedVersionWhy(spec.DatabaseVersion))
	}
	if b.InstanceType != "" && b.InstanceType != "READ_REPLICA_INSTANCE" {
		return Errorf(http.StatusBadRequest, "Invalid request: an instance with masterInstanceName must have instanceType READ_REPLICA_INSTANCE.")
	}
	user, pass, dump := rep.OnPremises.Username, rep.OnPremises.Password, rep.OnPremises.DumpFilePath
	if b.ReplicaConfiguration != nil {
		var rc struct {
			FailoverTarget bool `json:"failoverTarget"`
			Mysql          *struct {
				Username     string `json:"username"`
				Password     string `json:"password"`
				DumpFilePath string `json:"dumpFilePath"`
			} `json:"mysqlReplicaConfiguration"`
		}
		_ = json.Unmarshal(*b.ReplicaConfiguration, &rc)
		if rc.FailoverTarget {
			return legacyFailoverErr()
		}
		if m := rc.Mysql; m != nil {
			if m.DumpFilePath != "" {
				if pg {
					return Errorf(http.StatusBadRequest, "Invalid request: mysqlReplicaConfiguration is for a replica of a MySQL source.")
				}
				if _, _, err := parseGCSURI(m.DumpFilePath); err != nil {
					return err
				}
				dump = m.DumpFilePath
			}
			if m.Username != "" {
				user, pass = m.Username, m.Password
			}
		}
	}
	if user == "" {
		return Errorf(http.StatusBadRequest, "Invalid request: the replication user is missing: set onPremisesConfiguration.username "+
			"on %s or replicaConfiguration.mysqlReplicaConfiguration.username.", rep.Name)
	}
	host, port, err := hostPort(rep.OnPremises.HostPort)
	if err != nil {
		return err
	}
	if dump != "" && s.transfers == nil {
		return s.noTransfers("a replica seeded from dumpFilePath")
	}
	ext := &ExternalSource{Host: host, Port: port, User: user, Password: pass, DumpFile: dump}
	for _, o := range rep.OnPremises.SelectedObjects {
		if o.Database != "" {
			ext.Databases = append(ext.Databases, o.Database)
		}
	}
	upService, enginePort, enginePrefix := upMySQLService, 3306, "MYSQL_"
	if pg {
		upService, enginePort, enginePrefix = upPostgresService, 5432, "POSTGRES_"
	}
	// hostPort may be another CloudBurrow instance's: the address of its
	// tunnel on this machine, or its Service in the cluster (sqlinst-NAME,
	// or cloudsql-mysql for the server `up` runs). The replica's server
	// reaches it by its Service, and verification by its tunnel.
	short := strings.SplitN(host, ".", 2)[0]
	s.emu.RLock()
	for _, e := range s.engines {
		if e.Addr() == rep.OnPremises.HostPort || (short == upService && port == enginePort && strings.HasPrefix(e.DatabaseVersion(), enginePrefix)) {
			ext.Instance, ext.InstanceUp = e.Instance(), true
		}
	}
	for n, c := range s.writes.byName {
		if c.engine.Addr() == rep.OnPremises.HostPort || (short == instanceServicePrefix+n && port == enginePort) {
			ext.Instance, ext.InstanceUp = n, false
		}
	}
	s.emu.RUnlock()
	spec.MasterInstanceName = rep.Name
	spec.External = ext
	if b.Region == "" {
		spec.Region = rep.Region
	}
	if spec.Tier == "" {
		spec.Tier = "db-custom-1-3840"
	}
	return nil
}

// sourceAddr is the address this process reaches ext at: its
// CloudBurrow instance's tunnel, or its hostPort.
func (s *Server) sourceAddr(ext *ExternalSource) string {
	if ext.Instance != "" {
		if e := s.engine(ext.Instance); e != nil && e.Addr() != "" {
			return e.Addr()
		}
	}
	return net.JoinHostPort(ext.Host, strconv.Itoa(ext.Port))
}

// syncError is one sql#externalSyncSettingError.
func syncError(typ, detail string) map[string]any {
	return map[string]any{"kind": "sql#externalSyncSettingError", "type": typ, "detail": detail}
}

// verifyExternalSync checks the source an external replica would
// replicate from, as verifyExternalSyncSettings does: that it is reachable
// with the replication user, has the privileges a managed import needs,
// and logs GTID transactions in row format.
// An OFFLINE sync (#1524) only dumps: the binary log and the replication
// privileges are not checked.
func verifyExternalSync(ctx context.Context, spec Spec, addr string, started bool, offline bool) []any {
	errs := []any{}
	ext := spec.External
	if started {
		errs = append(errs, syncError("REPLICA_ALREADY_SETUP", "replication to the replica has already been started"))
		return errs
	}
	if !spec.MySQL() {
		return verifyPostgresSync(ctx, spec, addr, offline)
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
	var logBin, serverID int
	if err := db.QueryRowContext(ctx, "SELECT @@GLOBAL.version, @@GLOBAL.gtid_mode, @@GLOBAL.binlog_format, @@GLOBAL.log_bin, @@GLOBAL.server_id").
		Scan(&version, &gtid, &format, &logBin, &serverID); err != nil {
		return append(errs, syncError("CONNECTION_FAILURE", fmt.Sprintf("connect to %s as %s: %v", cfg.Addr, ext.User, err)))
	}
	if want := strings.ReplaceAll(strings.TrimPrefix(spec.DatabaseVersion, "MYSQL_"), "_", "."); !strings.HasPrefix(version, want+".") && version != want {
		errs = append(errs, syncError("INCOMPATIBLE_DATABASE_VERSION",
			fmt.Sprintf("the source runs MySQL %s and the replica %s; they must be the same major version", version, spec.DatabaseVersion)))
	}
	if offline {
		logBin, gtid, format = 1, "ON", "ROW"
	}
	if logBin != 1 {
		errs = append(errs, syncError("BINLOG_NOT_ENABLED", "the source's binary log is off (log_bin)"))
	}
	if gtid != "ON" {
		errs = append(errs, syncError("UNSUPPORTED_GTID_MODE", "the source's gtid_mode is "+gtid+"; a managed import needs ON"))
	}
	if format != "ROW" {
		errs = append(errs, syncError("UNSUPPORTED_BINLOG_FORMAT", "the source's binlog_format is "+format+"; it must be ROW"))
	}
	rows, err := db.QueryContext(ctx, "SHOW GRANTS")
	if err != nil {
		return append(errs, syncError("INSUFFICIENT_PRIVILEGE", err.Error()))
	}
	defer rows.Close()
	var grants strings.Builder
	for rows.Next() {
		var g string
		_ = rows.Scan(&g)
		grants.WriteString(strings.ToUpper(g) + "\n")
	}
	all := strings.Contains(grants.String(), "GRANT ALL PRIVILEGES ON *.*")
	need := []string{"REPLICATION SLAVE", "REPLICATION CLIENT", "SELECT", "SHOW VIEW", "RELOAD", "TRIGGER"}
	if offline {
		need = need[2:]
	}
	for _, p := range need {
		if !all && !strings.Contains(grants.String(), p) {
			errs = append(errs, syncError("INSUFFICIENT_PRIVILEGE", "the replication user "+ext.User+" lacks "+p+" ON *.*"))
		}
	}
	return errs
}

// externalReplica is the created external replica name, or why a sync
// method does not apply to it.
func (s *Server) externalReplica(project, name, what string) (*created, error) {
	c, err := s.createdFor(project, name, what)
	if err != nil {
		return nil, err
	}
	s.emu.RLock()
	defer s.emu.RUnlock()
	if c.spec.External == nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: %s needs a replica of a source representation instance (ON_PREMISES_INSTANCE); "+
			"%s is not one.", what, name)
	}
	if c.state != "RUNNABLE" {
		return nil, Errorf(http.StatusConflict, "Operation failed because the instance is %s.", c.state)
	}
	return c, nil
}

type syncRequest struct {
	SyncMode         string `json:"syncMode"`
	SkipVerification bool   `json:"skipVerification"`
	MigrationType    string `json:"migrationType"`
	VerifyConn       bool   `json:"verifyConnectionOnly"`
	SyncParallel     string `json:"syncParallelLevel"`
}

func decodeSync(r *http.Request) (syncRequest, error) {
	var req syncRequest
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return req, Errorf(http.StatusBadRequest, "Invalid JSON payload received: %v", err)
		}
	}
	switch req.SyncMode {
	case "", "EXTERNAL_SYNC_MODE_UNSPECIFIED", "ONLINE":
	case "OFFLINE":
		// A dump and load, with no replication after (#1524).
	default:
		return req, Errorf(http.StatusBadRequest, "Invalid request: Invalid value for syncMode: %s.", req.SyncMode)
	}
	if req.MigrationType == "PHYSICAL" {
		return req, unimplemented("migrationType PHYSICAL", "a physical migration restores an XtraBackup physical backup from Cloud Storage; "+
			"LOGICAL, a dump of the source's databases, is served")
	}
	return req, nil
}

// verifyExternalSyncSettings is instances.verifyExternalSyncSettings.
func (s *Server) verifyExternalSyncSettings(r *http.Request, project, name string) (any, error) {
	req, err := decodeSync(r)
	if err != nil {
		return nil, err
	}
	c, err := s.externalReplica(project, name, "verifyExternalSyncSettings")
	if err != nil {
		return nil, err
	}
	s.emu.RLock()
	spec := c.spec
	s.emu.RUnlock()
	errs := verifyExternalSync(r.Context(), spec, s.sourceAddr(spec.External), spec.ExternalSyncStarted, req.SyncMode == "OFFLINE")
	if req.VerifyConn {
		var conn []any
		for _, e := range errs {
			if e.(map[string]any)["type"] == "CONNECTION_FAILURE" {
				conn = append(conn, e)
			}
		}
		errs = conn
	}
	out := map[string]any{"kind": "sql#migrationSettingErrorList"}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	return out, nil
}

// startExternalSync is instances.startExternalSync: the replica's server
// is given the source's databases and replicates from it.
func (s *Server) startExternalSync(r *http.Request, base, project, name string) (any, error) {
	req, err := decodeSync(r)
	if err != nil {
		return nil, err
	}
	c, err := s.externalReplica(project, name, "startExternalSync")
	if err != nil {
		return nil, err
	}
	s.emu.Lock()
	spec := c.spec
	s.emu.Unlock()
	if spec.ExternalSyncStarted {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: replication to %s has already been started.", name)
	}
	if !req.SkipVerification {
		if errs := verifyExternalSync(r.Context(), spec, s.sourceAddr(spec.External), false, req.SyncMode == "OFFLINE"); len(errs) > 0 {
			var d []string
			for _, e := range errs {
				m := e.(map[string]any)
				d = append(d, fmt.Sprintf("%s: %s", m["type"], m["detail"]))
			}
			return nil, Errorf(http.StatusBadRequest, "Invalid request: the source fails verification: %s.", strings.Join(d, "; "))
		}
	}
	s.emu.Lock()
	if c.state != "RUNNABLE" || c.spec.ExternalSyncStarted {
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "Operation failed because another operation was already in progress.")
	}
	c.spec.ExternalSyncStarted = true
	ext := *c.spec.External
	ext.SyncMode = ""
	if req.SyncMode == "OFFLINE" {
		ext.SyncMode = "OFFLINE"
	}
	c.spec.External = &ext
	c.state = "MAINTENANCE"
	err = s.saveLocked()
	s.emu.Unlock()
	if err != nil {
		return nil, err
	}
	op := s.pending(base, project, name, "START_EXTERNAL_SYNC")
	go s.bringUp(name, op["name"].(string))
	return op, nil
}

// LogicalDecodingFlags are the Cloud SQL flags Google documents for a
// Cloud SQL for PostgreSQL instance that is an external primary's source
// (#1524): cloudsql.logical_decoding sets the server's wal_level to
// logical; cloudsql.enable_pglogical, which loads pglogical, is recorded,
// as the sync uses PostgreSQL's built-in logical replication instead
// (components.CloudSQLInstanceBackend).
var LogicalDecodingFlags = map[string]bool{"cloudsql.logical_decoding": true, "cloudsql.enable_pglogical": true}

// seedExternal loads a new MySQL external replica's dumpFilePath (#1524),
// as Google documents seeding from a dump file: the replica's GTIDs are
// reset, the dump, written by mysqldump --set-gtid-purged=ON, is loaded
// and sets them as the source's at the dump, and the replica then
// connects to the source and replicates from there, with no
// startExternalSync.
func (s *Server) seedExternal(ctx context.Context, name string, spec Spec) error {
	ext := spec.External
	if ext == nil || ext.DumpFile == "" || spec.ExternalSyncStarted || !spec.MySQL() {
		return nil
	}
	s.emu.RLock()
	c := s.writes.byName[name]
	s.emu.RUnlock()
	if c == nil || s.transfers == nil {
		return s.noTransfers("a replica seeded from dumpFilePath")
	}
	bucket, object, err := parseGCSURI(ext.DumpFile)
	if err != nil {
		return err
	}
	data, err := s.transfers.obj.Read(ctx, bucket, object)
	if err != nil {
		return fmt.Errorf("read the dump file %s: %w", ext.DumpFile, err)
	}
	var rd io.Reader = bytes.NewReader(data)
	if strings.HasSuffix(object, ".gz") {
		zr, err := gzip.NewReader(rd)
		if err != nil {
			return Errorf(http.StatusBadRequest, "%s ends in .gz but is not gzip: %v", ext.DumpFile, err)
		}
		rd = zr
	}
	reset := "RESET BINARY LOGS AND GTIDS;\n"
	if strings.HasPrefix(spec.DatabaseVersion, "MYSQL_8_0") {
		reset = "RESET MASTER;\n"
	}
	if err := s.transfers.t.ImportSQL(ctx, s.target(c.engine), "", "", io.MultiReader(strings.NewReader(reset), rd)); err != nil {
		return fmt.Errorf("load the dump file %s: %w", ext.DumpFile, err)
	}
	s.emu.Lock()
	c.spec.ExternalSyncStarted = true
	next := c.spec
	err = s.saveLocked()
	s.emu.Unlock()
	if err != nil {
		return err
	}
	return s.writes.prov.Start(ctx, next)
}
