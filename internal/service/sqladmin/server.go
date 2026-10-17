// Package sqladmin serves the Cloud SQL Admin API (sqladmin v1 and v1beta4,
// #1301) over the PostgreSQL and MySQL servers CloudBurrow runs.
//
// Google publishes no Cloud SQL emulator, so there is nothing to wrap: each
// running server is described as one Cloud SQL instance, and the databases
// and users methods run CREATE, ALTER and DROP in that server, so what they
// report is what the server holds. Nothing here keeps its own copy of a
// database or a user.
//
// What cannot be faithful is refused with 501 and says why: creating,
// patching, restarting or deleting an instance (that would mean starting or
// stopping a server) and IAM database authentication. The connector path
// (connectSettings, generateEphemeralCert and the TLS listener on 3307) is
// in connector.go (#1330).
package sqladmin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Region is the region every instance reports. CloudBurrow has no regions;
// one constant keeps connection names stable.
const Region = "us-central1"

// Database is one database, as an engine reports it.
type Database struct {
	Name, Charset, Collation string
}

// User is one user, as an engine reports it. Host is MySQL's; PostgreSQL's
// is empty, as Cloud SQL's is.
type User struct {
	Name, Host string
}

// Engine is one running server.
type Engine interface {
	// Instance is the instance's name in the API.
	Instance() string
	// DatabaseVersion is the Cloud SQL enum, such as POSTGRES_17.
	DatabaseVersion() string
	// InstalledVersion is the server's own version as Cloud SQL spells
	// databaseInstalledVersion, such as POSTGRES_17_11, read from the server.
	InstalledVersion(ctx context.Context) (string, error)
	// Addr is the host address the server is reached at, empty when its
	// tunnel is not up.
	Addr() string
	ListDatabases(ctx context.Context) ([]Database, error)
	CreateDatabase(ctx context.Context, d Database) error
	UpdateDatabase(ctx context.Context, d Database) error
	DeleteDatabase(ctx context.Context, name string) error
	ListUsers(ctx context.Context) ([]User, error)
	CreateUser(ctx context.Context, u User, password string) error
	UpdateUser(ctx context.Context, u User, password string) error
	DeleteUser(ctx context.Context, u User) error
}

// apiError is an error with the HTTP status and Google status it answers.
type apiError struct {
	code   int
	status string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

// Errorf is an error the server answers with code and the matching status.
func Errorf(code int, format string, args ...any) error {
	st := map[int]string{
		http.StatusBadRequest: "INVALID_ARGUMENT", http.StatusNotFound: "NOT_FOUND",
		http.StatusConflict: "ALREADY_EXISTS", http.StatusNotImplemented: "UNIMPLEMENTED",
		http.StatusPreconditionFailed: "FAILED_PRECONDITION", http.StatusServiceUnavailable: "UNAVAILABLE",
	}[code]
	if st == "" {
		st = "UNKNOWN"
	}
	return &apiError{code: code, status: st, msg: fmt.Sprintf(format, args...)}
}

// Server is the Admin API's HTTP server.
type Server struct {
	addr    string
	engines []Engine

	mu  sync.Mutex
	ops map[string]map[string]any // by operation name
	// opOrder is creation order, for operations.list.
	opOrder []string

	srv *http.Server
	ln  net.Listener
	// conn is the connector path, nil unless EnableConnector was called.
	conn *connector
	// writes are the created instances, nil unless EnableInstanceWrites
	// was called; emu guards them.
	emu    sync.RWMutex
	writes *instances
	// backups are the backup runs, nil unless EnableBackups was called.
	backups *backupStore
	// backupdr resolves Backup and DR names (#1433), nil unless
	// SetBackupDR was called; emu guards it.
	backupdr BackupDR
	// transfers are imports and exports, nil unless EnableTransfers was
	// called (#1380).
	transfers *transfers
	// certs are the instances' client certificates (sslCerts, #1380).
	certs *sslCertStore
	// secrets reads executeSql's passwordSecretVersion (#1462), nil
	// unless SetSecretAccessor was called; emu guards it.
	secrets  SecretAccessor
	stopAuto chan struct{}
	// advances are the retention clock's Advances (#1330), oldest first.
	clockMu  sync.Mutex
	advances []clockAdvance
	// Observe, if set, is called once per request with the method name and
	// the answer's status.
	Observe func(method string, code int)
}

// NewServer serves engines at addr once started.
func NewServer(addr string, engines ...Engine) *Server {
	return &Server{addr: addr, engines: engines, ops: map[string]map[string]any{}}
}

func (s *Server) Name() string { return "sqladmin" }

// Start binds the listener and serves in the background.
func (s *Server) Start(context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("sqladmin: listen on %s: %w", s.addr, err)
	}
	s.ln = ln
	if s.conn != nil {
		s.conn.start(s)
	}
	if s.writes != nil {
		s.restore()
	}
	if s.backups != nil {
		s.stopAuto = make(chan struct{})
		go s.automate(s.stopAuto)
	}
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

// Stop shuts the server down.
func (s *Server) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	if s.conn != nil {
		s.conn.stop()
	}
	if s.stopAuto != nil {
		close(s.stopAuto)
		s.stopAuto = nil
	}
	if s.writes != nil {
		if c, ok := s.writes.prov.(interface{ Close() }); ok {
			c.Close()
		}
	}
	return s.srv.Shutdown(ctx)
}

// Addr is the bound address, empty before Start.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// engine is the server named name: one `up` runs, or a created
// instance's.
func (s *Server) engine(name string) Engine {
	s.emu.RLock()
	defer s.emu.RUnlock()
	return s.engineLocked(name)
}

func (s *Server) engineLocked(name string) Engine {
	for _, e := range s.engines {
		if e.Instance() == name {
			return e
		}
	}
	if s.writes != nil {
		if c := s.writes.byName[name]; c != nil {
			return c.engine
		}
	}
	return nil
}

// createdNamed is the created instance named name in project, nil when
// there is none.
func (s *Server) createdNamed(project, name string) *created {
	s.emu.RLock()
	defer s.emu.RUnlock()
	if s.writes == nil {
		return nil
	}
	if c := s.writes.byName[name]; c != nil && c.spec.Project == project {
		return c
	}
	return nil
}

// ServeHTTP routes /v1/... and /sql/v1beta4/..., which differ only in prefix.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == ClockPath {
		out, err := s.serveClock(r)
		if err != nil {
			s.fail(w, "", err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	var prefix string
	switch {
	case strings.HasPrefix(path, "/v1/"):
		prefix = "/v1/"
	case strings.HasPrefix(path, "/sql/v1beta4/"):
		prefix = "/sql/v1beta4/"
	default:
		s.fail(w, "", Errorf(http.StatusNotFound, "%s is not a Cloud SQL Admin API path", path))
		return
	}
	base := "http://" + r.Host + prefix
	parts := strings.Split(strings.Trim(strings.TrimPrefix(path, prefix), "/"), "/")
	method, out, err := s.route(r, base, parts)
	if err != nil {
		s.fail(w, method, err)
		return
	}
	if s.Observe != nil {
		s.Observe(method, http.StatusOK)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) fail(w http.ResponseWriter, method string, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = &apiError{code: http.StatusBadRequest, status: "FAILED_PRECONDITION", msg: err.Error()}
	}
	if s.Observe != nil {
		s.Observe(method, ae.code)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(ae.code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": ae.code, "message": ae.msg, "status": ae.status,
		"errors": []map[string]any{{"domain": "global", "reason": strings.ToLower(ae.status), "message": ae.msg}},
	}})
}

func unimplemented(what, why string) error {
	return Errorf(http.StatusNotImplemented, "CloudBurrow does not implement %s: %s", what, why)
}

const instanceWhy = "this server was started without instance writes (docs/cloudsql.md)"

// The reasons the instance methods CloudBurrow cannot serve faithfully are
// refused (#1462): each names what is missing.
const (
	externalPrimaryWhy = "demoting makes a Cloud SQL instance a replica of an external primary, a server outside Cloud SQL, which a migration back out of Cloud SQL " +
		"replicates to; CloudBurrow serves the other direction, a replica of a source representation instance (ON_PREMISES_INSTANCE) " +
		"synced with startExternalSync (or seeded from dumpFilePath) and promoted with promoteReplica, but does not turn a primary into a replica: " +
		"Google's custom import demotes a promoted instance and then sets its replication up with Cloud SQL's mysql.* stored procedures, " +
		"which a MySQL server's own mysql schema does not take (docs/cloudsql.md)"
	sqlServerWhy = "it is a SQL Server feature, and CloudBurrow runs PostgreSQL and MySQL servers only"
	diskWhy      = "an instance's storage is its server's volume in the cluster, not a provisioned Compute Engine persistent disk, " +
		"so it has no provisioned size (settings.dataDiskSizeGb) to shrink or reset"
	BlueGreenWhy = "a blue-green deployment's switchover moves the blue instance's write endpoint (its IP address and DNS name) to the green instance, " +
		"which needs Google-managed networking; CloudBurrow's instances are reached by connection name through the connector. " +
		"Read replicas and promoteReplica are served (docs/cloudsql.md)"
	ResolveWhy = "resolving a DNS name needs the instance's Cloud DNS records (the dnsNames of Private Service Connect and write endpoints), " +
		"Google-managed networking that is not served; instances are reached by connection name through connectSettings"
)

// verbWhy is why an instance method other than restart is not served.
func verbWhy(verb string) string {
	switch verb {
	case "demoteMaster", "demote":
		return externalPrimaryWhy
	case "switchover":
		return "switchover swaps the primary with a cross-region disaster recovery replica of an Enterprise Plus replication cluster, " +
			"which needs a second region, and CloudBurrow has one (" + Region + "); promote a read replica with promoteReplica, " +
			"or fail over a REGIONAL instance with instances.failover (docs/cloudsql.md)"
	case "reencrypt":
		return "an instance's data is not encrypted with a customer-managed Cloud KMS key (diskEncryptionConfiguration is not applied to its server's volume), " +
			"so there is no key version to re-encrypt it with"
	case "acquireSsrsLease", "releaseSsrsLease":
		return "SQL Server Reporting Services leases: " + sqlServerWhy
	case "listEntraIdCertificates", "addEntraIdCertificate", "rotateEntraIdCertificate":
		return "Microsoft Entra ID authentication: " + sqlServerWhy
	case "listServerCertificates", "addServerCertificate", "rotateServerCertificate":
		return "per-instance server certificates are for instances whose serverCaMode is GOOGLE_MANAGED_CAS_CA or CUSTOMER_MANAGED_CAS_CA, " +
			"issued by Certificate Authority Service, which is not served; instances here are GOOGLE_MANAGED_INTERNAL_CA, " +
			"whose server CA is rotated with addServerCa and rotateServerCa"
	case "getDiskShrinkConfig", "performDiskShrink", "resetReplicaSize":
		return diskWhy
	case "rescheduleMaintenance":
		return "CloudBurrow's servers have no Google-managed maintenance: no maintenance event is ever scheduled (scheduledMaintenance is never set), " +
			"so there is none to reschedule"
	}
	return "beneath an instance only databases, users, backupRuns, connectSettings, restart, restoreBackup and clone are served; " +
		"see docs/cloudsql.md for what is served"
}

func (s *Server) route(r *http.Request, base string, p []string) (string, any, error) {
	if len(p) == 1 && p[0] == "flags" && r.Method == http.MethodGet {
		out, err := listFlags(r.URL.Query().Get("databaseVersion"))
		return "sql.flags.list", out, err
	}
	if len(p) == 3 && p[0] == "projects" && p[2] == "tiers" && r.Method == http.MethodGet {
		return "sql.tiers.list", listTiers(), nil
	}
	if len(p) == 2 && p[0] == "projects" && strings.HasSuffix(p[1], ":pointInTimeRestore") &&
		strings.TrimSuffix(p[1], ":pointInTimeRestore") != "" && r.Method == http.MethodPost {
		out, err := s.pointInTimeRestore(r, base, strings.TrimSuffix(p[1], ":pointInTimeRestore"))
		return "sql.instances.pointInTimeRestore", out, err
	}
	if len(p) == 4 && p[0] == "locations" && p[2] == "dns" && strings.HasSuffix(p[3], ":resolveConnectSettings") {
		return "sql.connect.resolve", nil, unimplemented("connect.resolve", ResolveWhy)
	}
	if len(p) >= 5 && p[0] == "projects" && p[2] == "locations" && p[4] == "blueGreenDeployments" {
		return "sql.blueGreenDeployments", nil, unimplemented("blueGreenDeployments", BlueGreenWhy)
	}
	if len(p) < 2 || p[0] != "projects" || p[1] == "" || strings.Contains(p[1], ":") || len(p) == 2 {
		// flags and locations.
		return "", nil, unimplemented(r.URL.Path, "only instances, databases, users and operations are served")
	}
	project := p[1]
	rest := p[2:]
	ctx := r.Context()
	switch rest[0] {
	case "operations":
		switch {
		case len(rest) == 1 && r.Method == http.MethodGet:
			return "sql.operations.list", s.listOperations(project, r.URL.Query().Get("instance")), nil
		case len(rest) == 2 && r.Method == http.MethodGet:
			op, err := s.operation(project, rest[1])
			return "sql.operations.get", op, err
		case len(rest) == 3 && rest[2] == "cancel" && r.Method == http.MethodPost:
			out, err := s.cancelOperation(project, rest[1])
			return "sql.operations.cancel", out, err
		}
		return "sql.operations", nil, Errorf(http.StatusNotFound, "%s is not a Cloud SQL Admin API path", r.URL.Path)
	case "backups":
		return s.projectBackups(r, base, project, rest[1:])
	case "instances":
	default:
		return "", nil, unimplemented("projects/"+project+"/"+rest[0], "only instances, databases, users and operations are served")
	}
	if len(rest) == 1 {
		switch r.Method {
		case http.MethodGet:
			out, err := s.listInstances(ctx, base, project)
			return "sql.instances.list", out, err
		case http.MethodPost:
			out, err := s.insertInstance(r, base, project)
			return "sql.instances.insert", out, err
		}
		return "sql.instances", nil, Errorf(http.StatusNotFound, "method %s is not served on instances", r.Method)
	}
	name, verb, _ := strings.Cut(rest[1], ":")
	if rep := s.sourceNamed(project, name); rep != nil {
		return s.sourceRoute(r, base, project, rep, rest, verb)
	}
	e := s.engine(name)
	c := s.createdNamed(project, name)
	if c == nil && e != nil && s.writes != nil {
		s.emu.RLock()
		if s.writes.byName[name] != nil {
			e = nil // another project's
		}
		s.emu.RUnlock()
	}
	if e == nil {
		return "sql.instances.get", nil, Errorf(http.StatusNotFound, "The Cloud SQL instance does not exist. "+
			"CloudBurrow's instances are %s.", s.instanceNames())
	}
	if verb == "generateEphemeralCert" {
		if r.Method != http.MethodPost {
			return "sql.connect.generateEphemeral", nil, Errorf(http.StatusNotFound, "method %s is not served on generateEphemeralCert", r.Method)
		}
		out, err := s.generateEphemeralCert(r, project, e)
		return "sql.connect.generateEphemeral", out, err
	}
	if len(rest) == 3 && r.Method == http.MethodPost && (rest[2] == "restoreBackup" || rest[2] == "clone" ||
		rest[2] == "promoteReplica" || rest[2] == "stopReplica" || rest[2] == "startReplica" || rest[2] == "failover") {
		verb = rest[2]
	}
	if len(rest) == 3 && rest[2] == "getLatestRecoveryTime" && r.Method == http.MethodGet {
		out, err := s.latestRecoveryTime(project, e)
		return "sql.projects.instances.getLatestRecoveryTime", out, err
	}
	switch verb {
	case "promoteReplica", "stopReplica", "startReplica":
		if r.Method != http.MethodPost {
			break
		}
		out, err := s.replicaVerb(base, project, name, verb)
		return "sql.instances." + verb, out, err
	case "failover":
		out, err := s.failover(r, base, project, name)
		return "sql.instances.failover", out, err
	case "restoreBackup":
		out, err := s.restoreBackup(r, base, project, e)
		return "sql.instances.restoreBackup", out, err
	case "clone":
		out, err := s.cloneInstance(r, base, project, e)
		return "sql.instances.clone", out, err
	}
	if verb != "" {
		return "sql.instances." + verb, nil, unimplemented("instances."+verb, verbWhy(verb))
	}
	if len(rest) == 2 {
		switch r.Method {
		case http.MethodGet:
			if c != nil {
				out, err := s.createdJSON(ctx, base, c)
				return "sql.instances.get", out, err
			}
			out, err := s.instance(ctx, base, project, e)
			return "sql.instances.get", out, err
		case http.MethodPatch:
			out, err := s.patchInstance(r, base, project, name, false)
			return "sql.instances.patch", out, err
		case http.MethodPut:
			out, err := s.patchInstance(r, base, project, name, true)
			return "sql.instances.update", out, err
		case http.MethodDelete:
			out, err := s.deleteInstance(r, base, project, name)
			return "sql.instances.delete", out, err
		}
		return "sql.instances", nil, Errorf(http.StatusNotFound, "method %s is not served on an instance", r.Method)
	}
	switch rest[2] {
	case "databases":
		return s.databases(r, base, project, e, rest[3:])
	case "users":
		return s.users(r, base, project, e, rest[3:])
	case "backupRuns":
		return s.backupRuns(r, base, project, e, rest[3:])
	case "sslCerts":
		return s.sslCerts(r, base, project, e, rest[3:])
	case "listServerCas":
		if len(rest) == 3 && r.Method == http.MethodGet {
			out, err := s.listServerCas(project, e)
			return "sql.instances.listServerCas", out, err
		}
	case "addServerCa":
		if len(rest) == 3 && r.Method == http.MethodPost {
			out, err := s.addServerCa(base, project, e)
			return "sql.instances.addServerCa", out, err
		}
	case "rotateServerCa":
		if len(rest) == 3 && r.Method == http.MethodPost {
			out, err := s.rotateServerCa(r, base, project, e)
			return "sql.instances.rotateServerCa", out, err
		}
	case "createEphemeral":
		if len(rest) == 3 && r.Method == http.MethodPost {
			out, err := s.generateEphemeralCert(r, project, e)
			if m, ok := out.(map[string]any); ok && err == nil {
				out = m["ephemeralCert"]
			}
			return "sql.sslCerts.createEphemeral", out, err
		}
	case "resetSslConfig":
		if len(rest) == 3 && r.Method == http.MethodPost {
			out, err := s.resetSslConfig(base, project, e)
			return "sql.instances.resetSslConfig", out, err
		}
	case "export":
		if len(rest) == 3 && r.Method == http.MethodPost {
			out, err := s.exportInstance(r, base, project, e)
			return "sql.instances.export", out, err
		}
	case "import":
		if len(rest) == 3 && r.Method == http.MethodPost {
			out, err := s.importInstance(r, base, project, e)
			return "sql.instances.import", out, err
		}
	case "connectSettings":
		if len(rest) != 3 || r.Method != http.MethodGet {
			return "sql.connect.get", nil, Errorf(http.StatusNotFound, "%s is not a Cloud SQL Admin API path", r.URL.Path)
		}
		out, err := s.connectSettings(r, project, e)
		return "sql.connect.get", out, err
	case "executeSql":
		if len(rest) == 3 && r.Method == http.MethodPost {
			out, err := s.executeSQL(r, project, e)
			return "sql.instances.executeSql", out, err
		}
	case "truncateLog":
		if len(rest) == 3 && r.Method == http.MethodPost {
			out, err := s.truncateLog(r, base, project, e)
			return "sql.instances.truncateLog", out, err
		}
	case "preCheckMajorVersionUpgrade":
		if len(rest) == 3 && r.Method == http.MethodPost {
			out, err := s.preCheckMajorVersionUpgrade(r, base, project, e)
			return "sql.instances.preCheckMajorVersionUpgrade", out, err
		}
	case "restart":
		if len(rest) == 3 && r.Method == http.MethodPost {
			out, err := s.restartInstance(base, project, name)
			return "sql.instances.restart", out, err
		}
	case "verifyExternalSyncSettings":
		if len(rest) == 3 && r.Method == http.MethodPost {
			out, err := s.verifyExternalSyncSettings(r, project, name)
			return "sql.projects.instances.verifyExternalSyncSettings", out, err
		}
	case "startExternalSync":
		if len(rest) == 3 && r.Method == http.MethodPost {
			out, err := s.startExternalSync(r, base, project, name)
			return "sql.projects.instances.startExternalSync", out, err
		}
	}
	return "sql.instances." + rest[2], nil, unimplemented("instances/{instance}/"+rest[2], verbWhy(rest[2]))
}

func (s *Server) instanceNames() string {
	var n []string
	for _, e := range s.engines {
		n = append(n, e.Instance())
	}
	if s.writes != nil {
		n = append(n, "the ones created with instances.insert")
	}
	if len(n) == 0 {
		return "none"
	}
	return strings.Join(n, " and ")
}

func (s *Server) listInstances(ctx context.Context, base, project string) (any, error) {
	items := []any{}
	for _, e := range s.engines {
		in, err := s.instance(ctx, base, project, e)
		if err != nil {
			return nil, err
		}
		items = append(items, in)
	}
	var cs []*created
	s.emu.RLock()
	if s.writes != nil {
		for _, c := range s.writes.byName {
			if c.spec.Project == project {
				cs = append(cs, c)
			}
		}
	}
	s.emu.RUnlock()
	var reps []*SourceRep
	s.emu.RLock()
	if s.writes != nil {
		for _, r := range s.writes.sources {
			if r.Project == project {
				reps = append(reps, r)
			}
		}
	}
	s.emu.RUnlock()
	sort.Slice(cs, func(i, j int) bool { return cs[i].spec.Name < cs[j].spec.Name })
	sort.Slice(reps, func(i, j int) bool { return reps[i].Name < reps[j].Name })
	for _, c := range cs {
		in, err := s.createdJSON(ctx, base, c)
		if err != nil {
			return nil, err
		}
		items = append(items, in)
	}
	for _, r := range reps {
		items = append(items, s.sourceJSON(base, r))
	}
	return map[string]any{"kind": "sql#instancesList", "items": items}, nil
}

func (s *Server) instance(ctx context.Context, base, project string, e Engine) (map[string]any, error) {
	state := "RUNNABLE"
	installed, err := e.InstalledVersion(ctx)
	if err != nil {
		// The server is not answering: Cloud SQL's word for an instance that
		// is not serving is UNKNOWN_STATE, not an error from the API.
		state = "UNKNOWN_STATE"
	}
	host := "127.0.0.1"
	if h, _, err := net.SplitHostPort(e.Addr()); err == nil && h != "" {
		host = h
	}
	in := map[string]any{
		"kind":            "sql#instance",
		"name":            e.Instance(),
		"project":         project,
		"connectionName":  project + ":" + Region + ":" + e.Instance(),
		"databaseVersion": e.DatabaseVersion(),
		"state":           state,
		"region":          Region,
		"gceZone":         Region + "-a",
		"backendType":     "SECOND_GEN",
		"instanceType":    "CLOUD_SQL_INSTANCE",
		"selfLink":        base + "projects/" + project + "/instances/" + e.Instance(),
		"ipAddresses":     []any{map[string]any{"type": "PRIMARY", "ipAddress": host}},
		"settings": map[string]any{
			"kind":             "sql#settings",
			"tier":             "db-custom-1-3840",
			"activationPolicy": "ALWAYS",
			"availabilityType": "ZONAL",
			"pricingPlan":      "PER_USE",
			"settingsVersion":  "1",
			"ipConfiguration":  map[string]any{"ipv4Enabled": true},
			"backupConfiguration": map[string]any{
				"kind": "sql#backupConfiguration", "enabled": false,
			},
		},
	}
	if installed != "" {
		in["databaseInstalledVersion"] = installed
	}
	s.emu.RLock()
	rs := s.replicaNamesLocked(project, e.Instance())
	s.emu.RUnlock()
	if len(rs) > 0 {
		in["replicaNames"] = rs
	}
	return in, nil
}

func (s *Server) databases(r *http.Request, base, project string, e Engine, rest []string) (string, any, error) {
	ctx := r.Context()
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			dbs, err := e.ListDatabases(ctx)
			if err != nil {
				return "sql.databases.list", nil, err
			}
			items := []any{}
			for _, d := range dbs {
				items = append(items, databaseJSON(base, project, e, d))
			}
			return "sql.databases.list", map[string]any{"kind": "sql#databasesList", "items": items}, nil
		case http.MethodPost:
			var d struct{ Name, Charset, Collation string }
			if err := decode(r, &d); err != nil {
				return "sql.databases.insert", nil, err
			}
			if d.Name == "" {
				return "sql.databases.insert", nil, Errorf(http.StatusBadRequest, "Missing parameter: name.")
			}
			if err := e.CreateDatabase(ctx, Database{Name: d.Name, Charset: d.Charset, Collation: d.Collation}); err != nil {
				return "sql.databases.insert", nil, err
			}
			return "sql.databases.insert", s.done(base, project, e, "CREATE_DATABASE", d.Name), nil
		}
		return "sql.databases", nil, Errorf(http.StatusNotFound, "method %s is not served on databases", r.Method)
	}
	name := rest[0]
	find := func() (Database, error) {
		dbs, err := e.ListDatabases(ctx)
		if err != nil {
			return Database{}, err
		}
		for _, d := range dbs {
			if d.Name == name {
				return d, nil
			}
		}
		return Database{}, Errorf(http.StatusNotFound, "Not Found: database %s does not exist in instance %s.", name, e.Instance())
	}
	switch r.Method {
	case http.MethodGet:
		d, err := find()
		if err != nil {
			return "sql.databases.get", nil, err
		}
		return "sql.databases.get", databaseJSON(base, project, e, d), nil
	case http.MethodPatch, http.MethodPut:
		method := "sql.databases.patch"
		if r.Method == http.MethodPut {
			method = "sql.databases.update"
		}
		cur, err := find()
		if err != nil {
			return method, nil, err
		}
		var d struct{ Name, Charset, Collation string }
		if err := decode(r, &d); err != nil {
			return method, nil, err
		}
		if d.Name != "" && d.Name != name {
			return method, nil, Errorf(http.StatusBadRequest, "a database cannot be renamed: the body names %s, the path %s", d.Name, name)
		}
		next := cur
		if d.Charset != "" {
			next.Charset = d.Charset
		}
		if d.Collation != "" {
			next.Collation = d.Collation
		}
		if next != cur {
			if err := e.UpdateDatabase(ctx, next); err != nil {
				return method, nil, err
			}
		}
		return method, s.done(base, project, e, "UPDATE_DATABASE", name), nil
	case http.MethodDelete:
		if _, err := find(); err != nil {
			return "sql.databases.delete", nil, err
		}
		if err := e.DeleteDatabase(ctx, name); err != nil {
			return "sql.databases.delete", nil, err
		}
		return "sql.databases.delete", s.done(base, project, e, "DELETE_DATABASE", name), nil
	}
	return "sql.databases", nil, Errorf(http.StatusNotFound, "method %s is not served on a database", r.Method)
}

func databaseJSON(base, project string, e Engine, d Database) map[string]any {
	return map[string]any{
		"kind": "sql#database", "name": d.Name, "charset": d.Charset, "collation": d.Collation,
		"instance": e.Instance(), "project": project,
		"selfLink": base + "projects/" + project + "/instances/" + e.Instance() + "/databases/" + d.Name,
	}
}

func (s *Server) users(r *http.Request, base, project string, e Engine, rest []string) (string, any, error) {
	ctx := r.Context()
	q := r.URL.Query()
	find := func(name, host string) (User, error) {
		us, err := e.ListUsers(ctx)
		if err != nil {
			return User{}, err
		}
		for _, u := range us {
			if u.Name == name && (u.Host == host || (host == "" && u.Host == "%")) {
				return u, nil
			}
		}
		return User{}, Errorf(http.StatusNotFound, "Not Found: user %s does not exist in instance %s.", name, e.Instance())
	}
	if len(rest) == 1 && r.Method == http.MethodGet {
		u, err := find(rest[0], q.Get("host"))
		if err != nil {
			return "sql.users.get", nil, err
		}
		return "sql.users.get", userJSON(project, e, u), nil
	}
	if len(rest) != 0 {
		return "sql.users", nil, Errorf(http.StatusNotFound, "%s is not a Cloud SQL Admin API path", r.URL.Path)
	}
	switch r.Method {
	case http.MethodGet:
		us, err := e.ListUsers(ctx)
		if err != nil {
			return "sql.users.list", nil, err
		}
		items := []any{}
		for _, u := range us {
			items = append(items, userJSON(project, e, u))
		}
		return "sql.users.list", map[string]any{"kind": "sql#usersList", "items": items}, nil
	case http.MethodPost:
		var u struct {
			Name, Host, Password, Type string
		}
		if err := decode(r, &u); err != nil {
			return "sql.users.insert", nil, err
		}
		if u.Name == "" {
			return "sql.users.insert", nil, Errorf(http.StatusBadRequest, "Missing parameter: name.")
		}
		if u.Type != "" && u.Type != "BUILT_IN" {
			return "sql.users.insert", nil, unimplemented("users of type "+u.Type,
				iamWhy+", so only BUILT_IN users are served")
		}
		if err := e.CreateUser(ctx, User{Name: u.Name, Host: u.Host}, u.Password); err != nil {
			return "sql.users.insert", nil, err
		}
		return "sql.users.insert", s.done(base, project, e, "CREATE_USER", u.Name), nil
	case http.MethodPut:
		var u struct{ Name, Host, Password string }
		if err := decode(r, &u); err != nil {
			return "sql.users.update", nil, err
		}
		name, host := q.Get("name"), q.Get("host")
		if name == "" {
			name, host = u.Name, u.Host
		}
		cur, err := find(name, host)
		if err != nil {
			return "sql.users.update", nil, err
		}
		if err := e.UpdateUser(ctx, cur, u.Password); err != nil {
			return "sql.users.update", nil, err
		}
		return "sql.users.update", s.done(base, project, e, "UPDATE_USER", name), nil
	case http.MethodDelete:
		cur, err := find(q.Get("name"), q.Get("host"))
		if err != nil {
			return "sql.users.delete", nil, err
		}
		if err := e.DeleteUser(ctx, cur); err != nil {
			return "sql.users.delete", nil, err
		}
		return "sql.users.delete", s.done(base, project, e, "DELETE_USER", cur.Name), nil
	}
	return "sql.users", nil, Errorf(http.StatusNotFound, "method %s is not served on users", r.Method)
}

func userJSON(project string, e Engine, u User) map[string]any {
	m := map[string]any{"kind": "sql#user", "name": u.Name, "instance": e.Instance(), "project": project,
		"type": "BUILT_IN"}
	if u.Host != "" {
		m["host"] = u.Host
	}
	return m
}

func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return Errorf(http.StatusBadRequest, "Invalid JSON payload received: %v", err)
	}
	return nil
}

// done records an operation that has already finished: every method here
// runs its statement before it answers, so the operation is DONE when it is
// returned, and operations.get answers the same thing afterwards.
func (s *Server) done(base, project string, e Engine, opType, target string) map[string]any {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	id = id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
	now := time.Now().UTC().Format(time.RFC3339Nano)
	op := map[string]any{
		"kind": "sql#operation", "name": id, "operationType": opType, "status": "DONE",
		"targetId": e.Instance(), "targetProject": project,
		"targetLink": base + "projects/" + project + "/instances/" + e.Instance(),
		"selfLink":   base + "projects/" + project + "/operations/" + id,
		"insertTime": now, "startTime": now, "endTime": now, "user": "cloudburrow@localhost",
	}
	s.mu.Lock()
	s.ops[id] = op
	s.opOrder = append(s.opOrder, id)
	s.mu.Unlock()
	return op
}

func (s *Server) operation(project, name string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[name]
	if !ok || op["targetProject"] != project {
		return nil, Errorf(http.StatusNotFound, "The Cloud SQL operation %s does not exist in project %s.", name, project)
	}
	return copyOp(op), nil
}

func (s *Server) listOperations(project, instance string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := []any{}
	// Newest first, as Cloud SQL lists them.
	for i := len(s.opOrder) - 1; i >= 0; i-- {
		op := s.ops[s.opOrder[i]]
		if op["targetProject"] != project || (instance != "" && op["targetId"] != instance) {
			continue
		}
		items = append(items, copyOp(op))
	}
	return map[string]any{"kind": "sql#operationsList", "items": items}
}
