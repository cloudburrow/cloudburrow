package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
	"github.com/cloudburrow/cloudburrow/internal/service/datamigration"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// dataMigrationService serves Database Migration Service (#2252) in the
// CLI process, gRPC and JSON on one port. Its destinations are the
// instance's own Cloud SQL instances (through the Cloud SQL Admin API's
// external-primary replicas, in process) and AlloyDB clusters (through
// the AlloyDB API, in process); every check and copy runs from the
// destination's server in the cluster.
type dataMigrationService struct {
	cfg       config.Config
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	// wrapJSON wraps the JSON API (IAM enforcement); nil is none.
	wrapJSON func(http.Handler) http.Handler
	// sql is the Cloud SQL Admin API, nil without Cloud SQL.
	sql *sqladmin.Server
	// alloy is AlloyDB, nil without it.
	alloy *alloyDBService

	server *grpctransport.Server
	api    *datamigration.Server
}

func newDataMigrationService(cfg config.Config) *dataMigrationService {
	if !dataMigrationEnabled(cfg) {
		return nil
	}
	return &dataMigrationService{cfg: cfg}
}

// dataMigrationEnabled is whether `up` binds the Database Migration
// Service port: it is enabled with a destination to migrate to.
func dataMigrationEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceDataMigration) && cfg.Endpoints.DataMigration >= 0 &&
		(serviceEnabled(cfg, config.ServiceCloudSQL) || serviceEnabled(cfg, config.ServiceCloudSQLMySQL) || serviceEnabled(cfg, config.ServiceAlloyDB))
}

func (s *dataMigrationService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *dataMigrationService) Name() string { return "datamigration" }

// API is the server, available after Start.
func (s *dataMigrationService) API() *datamigration.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *dataMigrationService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

func (s *dataMigrationService) Start(ctx context.Context) error {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "datamigration.json")
	}
	b := &dmsBackend{cfg: s.cfg, sql: s.sql, alloy: s.alloy}
	opts := datamigration.Options{Backend: b, CloudSQL: s.sql != nil, AlloyDB: s.alloy != nil, StaticIPs: b.staticIPs}
	api, err := datamigration.NewServer(state, opts)
	if err != nil {
		return err
	}
	s.api = api
	api.Resume()
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.DataMigration)))
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = datamigration.NewRESTHandler(api)
	if s.wrapJSON != nil {
		jsonAPI = s.wrapJSON(jsonAPI)
	}
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

func (s *dataMigrationService) Stop(ctx context.Context) error {
	var errs []error
	if s.server != nil {
		errs = append(errs, s.server.Stop(ctx))
	}
	if s.api != nil {
		s.api.Close()
	}
	return errors.Join(errs...)
}

// dataMigrationResetter forgets the records; the destinations' instances
// and clusters are Cloud SQL's and AlloyDB's to reset.
type dataMigrationResetter struct{ svc *dataMigrationService }

func (r *dataMigrationResetter) Name() string { return "datamigration" }

func (r *dataMigrationResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *dataMigrationResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("the Database Migration Service API has not started")
	}
	return api.ResetProject(project)
}

// useIAMEnforcementDataMigration checks Database Migration Service's
// callers (#2252): each method's documented permission
// (iameval.DataMigration), over gRPC and JSON, with the connection
// profiles', migration jobs' and private connections' own policies.
func useIAMEnforcementDataMigration(e *iameval.Enforcer, s *dataMigrationService) {
	if s == nil || e == nil {
		return
	}
	s.interpose = append(s.interpose, e.Interceptor(iameval.DataMigration))
	s.wrapJSON = jsonEnforcer(e, iameval.DataMigration, datamigration.Mixins())
	e.Policies[iameval.DataMigration.Name] = iameval.GRPCPolicyGetter(func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
		api := s.API()
		if api == nil {
			return nil
		}
		return api.IAMPolicy().GetIamPolicy
	})
}

// dmsBackend is datamigration.Backend over the instance's Cloud SQL and
// AlloyDB.
type dmsBackend struct {
	cfg   config.Config
	sql   *sqladmin.Server
	alloy *alloyDBService
}

var _ datamigration.Backend = (*dmsBackend)(nil)

const dmsPoll = 2 * time.Second

// sourceRep is the name of a Cloud SQL destination's source
// representation instance (UNVERIFIED: Google's console names it after
// the destination; the API does not say how).
// Docs re-read 2026-11-25, still unanswered: https://docs.cloud.google.com/sql/docs/postgres/replication/configure-replication-from-external
func sourceRep(t datamigration.Target) string { return t.ID + "-master" }

// sqlCall calls the Cloud SQL Admin API in process and answers its JSON.
func (b *dmsBackend) sqlCall(ctx context.Context, method, path string, body any) (map[string]any, error) {
	if b.sql == nil {
		return nil, apierror.FailedPrecondition("Cloud SQL is not served by this instance")
	}
	var rd io.Reader
	if body != nil {
		j, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(j)
	}
	req := httptest.NewRequest(method, "http://cloudburrow.internal/v1/"+path, rd).WithContext(ctx)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	b.sql.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK {
		msg := strings.TrimSpace(rec.Body.String())
		if e, ok := out["error"].(map[string]any); ok {
			if m, ok := e["message"].(string); ok {
				msg = m
			}
		}
		return nil, &apierror.Error{Code: httpCode(rec.Code), Message: "Cloud SQL: " + msg}
	}
	return out, nil
}

func httpCode(status int) codes.Code {
	switch status {
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		return codes.AlreadyExists
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotImplemented:
		return codes.Unimplemented
	case http.StatusPreconditionFailed:
		return codes.FailedPrecondition
	}
	return codes.Internal
}

// sqlWait waits for a Cloud SQL operation, failing with its error.
func (b *dmsBackend) sqlWait(ctx context.Context, project string, op map[string]any) error {
	name, _ := op["name"].(string)
	for {
		if op["status"] == "DONE" {
			if e, ok := op["error"].(map[string]any); ok {
				j, _ := json.Marshal(e)
				return fmt.Errorf("Cloud SQL operation %s failed: %s", name, j)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dmsPoll):
		}
		next, err := b.sqlCall(ctx, http.MethodGet, "projects/"+project+"/operations/"+name, nil)
		if err != nil {
			return err
		}
		op = next
	}
}

// sqlOp posts an instance verb and waits for its operation.
func (b *dmsBackend) sqlOp(ctx context.Context, t datamigration.Target, method, path string, body any) error {
	op, err := b.sqlCall(ctx, method, "projects/"+t.Project+"/instances/"+path, body)
	if err != nil {
		return err
	}
	return b.sqlWait(ctx, t.Project, op)
}

// createSourceRep creates t's source representation instance of src,
// replacing one a restart left.
func (b *dmsBackend) createSourceRep(ctx context.Context, t datamigration.Target, src datamigration.Source) error {
	src, err := b.reach(ctx, src)
	if err != nil {
		return err
	}
	hp := net.JoinHostPort(src.Host, strconv.Itoa(src.Port))
	onPrem := map[string]any{"hostPort": hp, "username": src.User, "password": src.Password}
	if src.CACert != "" {
		// A SERVER_ONLY or SERVER_CLIENT profile (#2257): the replica
		// verifies the source's certificate.
		onPrem["caCertificate"] = src.CACert
		if src.ClientCert != "" {
			onPrem["clientCertificate"], onPrem["clientKey"] = src.ClientCert, src.ClientKey
		}
	}
	rep := map[string]any{"name": sourceRep(t), "databaseVersion": t.Version, "instanceType": "ON_PREMISES_INSTANCE", "region": t.Location,
		"onPremisesConfiguration": onPrem}
	if _, err := b.sqlCall(ctx, http.MethodGet, "projects/"+t.Project+"/instances/"+sourceRep(t), nil); err == nil {
		// A restart's replica is created again from the same source.
		_ = b.sqlOp(ctx, t, http.MethodDelete, sourceRep(t), nil)
	}
	if err := b.sqlOp(ctx, t, http.MethodPost, "", rep); err != nil {
		return fmt.Errorf("create the source representation instance %s: %w", sourceRep(t), err)
	}
	return nil
}

func (b *dmsBackend) CreateCloudSQL(ctx context.Context, t datamigration.Target, st *dmspb.CloudSqlSettings, src datamigration.Source) (datamigration.CloudSQLAddresses, error) {
	var none datamigration.CloudSQLAddresses
	if err := b.createSourceRep(ctx, t, src); err != nil {
		return none, err
	}
	settings := map[string]any{"tier": st.GetTier()}
	if l := st.GetUserLabels(); len(l) > 0 {
		settings["userLabels"] = l
	}
	if f := st.GetDatabaseFlags(); len(f) > 0 {
		var flags []map[string]string
		for k, v := range f {
			flags = append(flags, map[string]string{"name": k, "value": v})
		}
		settings["databaseFlags"] = flags
	}
	if n := st.GetDataDiskSizeGb().GetValue(); n > 0 {
		settings["dataDiskSizeGb"] = strconv.FormatInt(n, 10)
	}
	if d := st.GetDataDiskType(); d != dmspb.CloudSqlSettings_SQL_DATA_DISK_TYPE_UNSPECIFIED {
		settings["dataDiskType"] = d.String()
	}
	if e := st.GetEdition(); e != dmspb.CloudSqlSettings_EDITION_UNSPECIFIED {
		settings["edition"] = e.String()
	}
	if ip := st.GetIpConfig(); ip != nil {
		cfg := map[string]any{}
		if ip.GetEnableIpv4() != nil {
			cfg["ipv4Enabled"] = ip.GetEnableIpv4().GetValue()
		}
		if ip.GetPrivateNetwork() != "" {
			cfg["privateNetwork"] = ip.GetPrivateNetwork()
		}
		var nets []map[string]string
		for _, n := range ip.GetAuthorizedNetworks() {
			nets = append(nets, map[string]string{"value": n.GetValue(), "name": n.GetLabel()})
		}
		if len(nets) > 0 {
			cfg["authorizedNetworks"] = nets
		}
		settings["ipConfiguration"] = cfg
	}
	if z := st.GetZone(); z != "" {
		settings["locationPreference"] = map[string]any{"zone": z}
	}
	replica := map[string]any{"name": t.ID, "databaseVersion": t.Version, "masterInstanceName": sourceRep(t), "region": t.Location,
		"instanceType": "READ_REPLICA_INSTANCE", "settings": settings}
	if p := st.GetRootPassword(); p != "" {
		replica["rootPassword"] = p
	}
	if err := b.sqlOp(ctx, t, http.MethodPost, "", replica); err != nil {
		return none, fmt.Errorf("create the destination instance %s: %w", t.ID, err)
	}
	in, err := b.sqlCall(ctx, http.MethodGet, "projects/"+t.Project+"/instances/"+t.ID, nil)
	if err != nil {
		return none, err
	}
	var out datamigration.CloudSQLAddresses
	ips, _ := in["ipAddresses"].([]any)
	for _, a := range ips {
		m, _ := a.(map[string]any)
		ip, _ := m["ipAddress"].(string)
		switch m["type"] {
		case "PRIMARY":
			out.PublicIP = ip
		case "PRIVATE":
			out.PrivateIP = ip
		}
	}
	return out, nil
}

func (b *dmsBackend) CreateAlloyDB(ctx context.Context, t datamigration.Target, st *dmspb.AlloyDbSettings) (string, error) {
	api := b.alloy.API()
	if api == nil {
		return "", apierror.FailedPrecondition("AlloyDB is not served by this instance")
	}
	parent := "projects/" + t.Project + "/locations/" + t.Location
	cl := &alloydbpb.Cluster{Labels: st.GetLabels(),
		InitialUser: &alloydbpb.UserPassword{User: st.GetInitialUser().GetUser(), Password: st.GetInitialUser().GetPassword()}}
	if n := st.GetVpcNetwork(); n != "" {
		cl.NetworkConfig = &alloydbpb.Cluster_NetworkConfig{Network: n}
	}
	if v, ok := alloydbpb.DatabaseVersion_value[t.Version]; ok && t.Version != "" {
		cl.DatabaseVersion = alloydbpb.DatabaseVersion(v)
	}
	op, err := api.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: parent, ClusterId: t.ID, Cluster: cl})
	if err != nil {
		return "", fmt.Errorf("create the AlloyDB cluster %s: %w", t.ID, err)
	}
	if err := b.alloyWait(ctx, api, op.GetName()); err != nil {
		return "", fmt.Errorf("create the AlloyDB cluster %s: %w", t.ID, err)
	}
	ps := st.GetPrimaryInstanceSettings()
	inst := &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, DatabaseFlags: ps.GetDatabaseFlags(), Labels: ps.GetLabels()}
	if c := ps.GetMachineConfig().GetCpuCount(); c > 0 {
		inst.MachineConfig = &alloydbpb.Instance_MachineConfig{CpuCount: c}
	}
	op, err = api.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: parent + "/clusters/" + t.ID, InstanceId: ps.GetId(), Instance: inst})
	if err != nil {
		return "", fmt.Errorf("create the AlloyDB primary instance %s: %w", ps.GetId(), err)
	}
	if err := b.alloyWait(ctx, api, op.GetName()); err != nil {
		return "", fmt.Errorf("create the AlloyDB primary instance %s: %w", ps.GetId(), err)
	}
	c, err := api.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: parent + "/clusters/" + t.ID})
	if err != nil {
		return "", err
	}
	return c.GetDatabaseVersion().String(), nil
}

func (b *dmsBackend) alloyWait(ctx context.Context, api *alloydb.Server, name string) error {
	for {
		op, err := api.Operation(name)
		if err != nil {
			return err
		}
		if op.GetDone() {
			if e := op.GetError(); e != nil {
				return &apierror.Error{Code: codes.Code(e.GetCode()), Message: e.GetMessage()}
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dmsPoll):
		}
	}
}

func (b *dmsBackend) Delete(ctx context.Context, t datamigration.Target) error {
	if !t.CloudSQL {
		return nil
	}
	for _, n := range []string{t.ID, sourceRep(t)} {
		if err := b.sqlOp(ctx, t, http.MethodDelete, n, nil); err != nil && apierror.From(err).Code != codes.NotFound {
			return fmt.Errorf("delete the Cloud SQL instance %s: %w", n, err)
		}
	}
	return nil
}

// exec runs a command in t's server.
func (b *dmsBackend) exec(ctx context.Context, t datamigration.Target) (func(context.Context, io.Reader, io.Writer, ...string) error, error) {
	if t.CloudSQL {
		if b.sql == nil {
			return nil, apierror.FailedPrecondition("Cloud SQL is not served by this instance")
		}
		return nodeExec(b.cfg, components.SQLInstancePrefix+b.sql.ServerOf(t.ID)), nil
	}
	api := b.alloy.API()
	if api == nil {
		return nil, apierror.FailedPrecondition("AlloyDB is not served by this instance")
	}
	list, err := api.ListInstances(ctx, &alloydbpb.ListInstancesRequest{Parent: "projects/" + t.Project + "/locations/" + t.Location + "/clusters/" + t.ID})
	if err != nil {
		return nil, err
	}
	for _, in := range list.GetInstances() {
		if in.GetInstanceType() == alloydbpb.Instance_PRIMARY {
			return nodeExec(b.cfg, alloydb.ServerOf(in.GetName())), nil
		}
	}
	return nil, apierror.FailedPrecondition("the AlloyDB cluster %s has no primary instance", t.ID)
}

// sep separates a row's columns in psql's unaligned output.
const sep = "\x1f"

func parseRows(out string, fieldSep string) [][]string {
	var rows [][]string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if l == "" {
			continue
		}
		rows = append(rows, strings.Split(l, fieldSep))
	}
	return rows
}

var (
	pgAuthRE    = regexp.MustCompile(`password authentication failed|no pg_hba\.conf entry|role "[^"]*" does not exist|authentication failed`)
	pgConnRE    = regexp.MustCompile(`could not connect|could not translate host name|Connection refused|timeout expired|No route to host|Network is unreachable|server closed the connection`)
	mysqlAuthRE = regexp.MustCompile(`Access denied`)
	mysqlConnRE = regexp.MustCompile(`Can't connect|Unknown MySQL server host|Lost connection|timed out`)
)

// classify is a source query's failure as the verification reports it.
func classify(err error, mysql bool) error {
	msg := err.Error()
	auth, conn := pgAuthRE, pgConnRE
	if mysql {
		auth, conn = mysqlAuthRE, mysqlConnRE
	}
	switch {
	case auth.MatchString(msg):
		return &datamigration.ConnError{Auth: true, Msg: msg}
	case conn.MatchString(msg):
		return &datamigration.ConnError{Msg: msg}
	}
	return err
}

func (b *dmsBackend) QuerySource(ctx context.Context, t datamigration.Target, src datamigration.Source, database, query string) ([][]string, error) {
	run, err := b.exec(ctx, t)
	if err != nil {
		return nil, err
	}
	if src, err = b.reach(ctx, src); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	// The password, then the certificates (#2257), on the script's input.
	stdin := strings.NewReader(src.Password + "\n" + tlsStdin(src))
	if src.Engine == dmspb.DatabaseEngine_MYSQL {
		ssl := "PREFERRED"
		if src.RequireSSL {
			ssl = "REQUIRED"
		}
		// A verified profile's certificate is checked against its CA
		// (VERIFY_CA), its client certificate presented.
		args := []string{"sh", "-c", `IFS= read -r MYSQL_PWD; export MYSQL_PWD
` + tlsFiles + `db=""; if [ -n "$5" ]; then db="-D$5"; fi
tls="--ssl-mode=$6"
if [ -n "$ca" ]; then tls="--ssl-mode=VERIFY_CA --ssl-ca=$ca"; fi
if [ -n "$cert" ]; then tls="$tls --ssl-cert=$cert --ssl-key=$key"; fi
mysql -h "$1" -P "$2" -u "$3" -N -B --raw --connect-timeout=10 $tls $db -e "$4"`,
			"sh", src.Host, strconv.Itoa(src.Port), src.User, query, database, ssl}
		if err := run(ctx, stdin, &out, args...); err != nil {
			return nil, classify(err, true)
		}
		return parseRows(out.String(), "\t"), nil
	}
	if database == "" {
		database = "postgres"
	}
	ssl := "prefer"
	if src.RequireSSL {
		ssl = "require"
	}
	conn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=%s connect_timeout=10", pgConnValue(src.Host), src.Port, pgConnValue(src.User), pgConnValue(database), ssl)
	args := []string{"sh", "-c", `IFS= read -r PGPASSWORD; export PGPASSWORD
` + tlsFiles + `conn="$1"
if [ -n "$ca" ]; then conn="$conn sslmode=verify-ca sslrootcert=$ca"; fi
if [ -n "$cert" ]; then conn="$conn sslcert=$cert sslkey=$key"; fi
psql "$conn" -X -v ON_ERROR_STOP=1 -qAt -F "$(printf '\037')" -c "$2"`,
		"sh", conn, query}
	if err := run(ctx, stdin, &out, args...); err != nil {
		return nil, classify(err, false)
	}
	return parseRows(out.String(), sep), nil
}

// pgConnValue quotes a libpq connection string value.
func pgConnValue(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

func (b *dmsBackend) QueryTarget(ctx context.Context, t datamigration.Target, database, query string) ([][]string, error) {
	run, err := b.exec(ctx, t)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if t.MySQL() {
		args := []string{"sh", "-c", mysqlRootEnv + `
db=""; if [ -n "$2" ]; then db="-D$2"; fi
exec mysql $MYSQL_LOCAL -N -B --raw $db -e "$1"`, "sh", query, database}
		if err := run(ctx, nil, &out, args...); err != nil {
			return nil, err
		}
		return parseRows(out.String(), "\t"), nil
	}
	if database == "" {
		database = "postgres"
	}
	args := []string{"sh", "-c", `PGPASSWORD= exec psql -U postgres -d "$1" -X -v ON_ERROR_STOP=1 -qAt -F "$(printf '\037')" -c "$2"`, "sh", database, query}
	if err := run(ctx, nil, &out, args...); err != nil {
		return nil, err
	}
	return parseRows(out.String(), sep), nil
}

func (b *dmsBackend) Migrate(ctx context.Context, t datamigration.Target, src datamigration.Source, m datamigration.Migration) error {
	mode := "ONLINE"
	if !m.Continuous {
		mode = "OFFLINE"
	}
	src, err := b.reach(ctx, src)
	if err != nil {
		return err
	}
	if t.CloudSQL {
		// The job's source as the destination reaches it (a tunnel, a
		// bastion), its certificates, databases, dump file and dump flags
		// (#2257), on the destination replica before its sync.
		if b.sql == nil {
			return apierror.FailedPrecondition("Cloud SQL is not served by this instance")
		}
		if err := b.sql.ConfigureExternal(t.Project, t.ID, func(e *sqladmin.ExternalSource) {
			e.Host, e.Port, e.Instance, e.InstanceUp = src.Host, src.Port, "", false
			e.User, e.Password = src.User, src.Password
			e.CACert, e.ClientCert, e.ClientKey = src.CACert, src.ClientCert, src.ClientKey
			if m.Databases != nil {
				e.Databases = m.Databases
			}
			e.DumpFlags = m.DumpFlags
			if m.DumpPath != "" {
				e.DumpFile, e.LoadDumpAtSync = m.DumpPath, true
			}
		}); err != nil {
			return err
		}
		// Database Migration Service verified the source itself.
		return b.sqlOp(ctx, t, http.MethodPost, t.ID+"/startExternalSync", map[string]any{"syncMode": mode, "skipVerification": true})
	}
	run, err := b.exec(ctx, t)
	if err != nil {
		return err
	}
	// The PostgreSQL external replica's setup (sqlinstances.go), run in
	// the AlloyDB primary: each database's schema by pg_dump, then a
	// subscription to a publication of its tables. An existing cluster's
	// databases of the migrated names are dropped first (#2257).
	demoted := ""
	if t.Existing {
		demoted = " CLOUDSQL_DEMOTED=1"
	}
	env := "export CLOUDSQL_PRIMARY_HOST=" + shellQuote(src.Host) + " CLOUDSQL_PRIMARY_PORT=" + strconv.Itoa(src.Port) +
		" CLOUDSQL_PRIMARY_USER=" + shellQuote(src.User) + " CLOUDSQL_SYNC_MODE=" + mode +
		" CLOUDSQL_DATABASES=" + shellQuote(strings.Join(m.Databases, " ")) + demoted +
		" CLOUDSQL_SYNC_ID=" + fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(t.Project+":"+t.ID))) + "\n" +
		"IFS= read -r CLOUDSQL_REPLICATION_PASSWORD; export CLOUDSQL_REPLICATION_PASSWORD\n" +
		"IFS= read -r l; CLOUDSQL_PRIMARY_SSL_CA=$(printf '%s' \"$l\" | base64 -d)\n" +
		"IFS= read -r l; CLOUDSQL_PRIMARY_SSL_CERT=$(printf '%s' \"$l\" | base64 -d)\n" +
		"IFS= read -r l; CLOUDSQL_PRIMARY_SSL_KEY=$(printf '%s' \"$l\" | base64 -d)\n" +
		"export CLOUDSQL_PRIMARY_SSL_CA CLOUDSQL_PRIMARY_SSL_CERT CLOUDSQL_PRIMARY_SSL_KEY\n"
	script := src.Password + "\n" + tlsStdin(src)
	var out bytes.Buffer
	if err := run(ctx, strings.NewReader(script), &out, "sh", "-c", env+pgExternalSetup); err != nil {
		return classify(err, false)
	}
	return nil
}

func (b *dmsBackend) Pause(ctx context.Context, t datamigration.Target, pause bool) error {
	if t.CloudSQL {
		verb := "/startReplica"
		if pause {
			verb = "/stopReplica"
		}
		return b.sqlOp(ctx, t, http.MethodPost, t.ID+verb, nil)
	}
	act := " ENABLE"
	if pause {
		act = " DISABLE"
	}
	return b.eachSubscription(ctx, t, func(db, sub string) error {
		_, err := b.QueryTarget(ctx, t, db, "ALTER SUBSCRIPTION "+quotePGIdent(sub)+act)
		return err
	})
}

// eachSubscription calls f with each of an AlloyDB target's subscriptions.
func (b *dmsBackend) eachSubscription(ctx context.Context, t datamigration.Target, f func(db, sub string) error) error {
	rows, err := b.QueryTarget(ctx, t, "postgres", `SELECT d.datname, s.subname FROM pg_subscription s JOIN pg_database d ON d.oid = s.subdbid ORDER BY 1, 2`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if len(r) == 2 {
			if err := f(r[0], r[1]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *dmsBackend) Promote(ctx context.Context, t datamigration.Target) error {
	if t.CloudSQL {
		if err := b.sqlOp(ctx, t, http.MethodPost, t.ID+"/promoteReplica", nil); err != nil {
			return err
		}
		if t.Existing {
			// The existing instance is standalone again; the source
			// representation instance its demotion made goes.
			if err := b.sqlOp(ctx, t, http.MethodDelete, sourceRep(t), nil); err != nil && apierror.From(err).Code != codes.NotFound {
				return err
			}
		}
		return nil
	}
	return b.dropSubscriptions(ctx, t)
}

// Existing checks an existing destination (#2257): a Cloud SQL instance
// that is a standalone primary, RUNNABLE, with no replicas, or an AlloyDB
// cluster that is a READY primary cluster with a primary instance.
func (b *dmsBackend) Existing(ctx context.Context, t datamigration.Target) (string, error) {
	if t.CloudSQL {
		in, err := b.sqlCall(ctx, http.MethodGet, "projects/"+t.Project+"/instances/"+t.ID, nil)
		if err != nil {
			return "", err
		}
		v, _ := in["databaseVersion"].(string)
		switch {
		case in["instanceType"] != "CLOUD_SQL_INSTANCE" || in["masterInstanceName"] != nil:
			return "", apierror.InvalidArgument("the Cloud SQL instance %s is not a standalone primary instance", t.ID)
		case in["region"] != t.Location:
			// "The destination instance must be in the same region as the
			// migration job."
			return "", apierror.InvalidArgument("the Cloud SQL instance %s is in %v, not %s", t.ID, in["region"], t.Location)
		case in["state"] != "RUNNABLE":
			return "", apierror.FailedPrecondition("the Cloud SQL instance %s is %v, not RUNNABLE", t.ID, in["state"])
		}
		if r, _ := in["replicaNames"].([]any); len(r) > 0 {
			return "", apierror.FailedPrecondition("the Cloud SQL instance %s has read replicas: an existing destination is standalone", t.ID)
		}
		return v, nil
	}
	api := b.alloy.API()
	if api == nil {
		return "", apierror.FailedPrecondition("AlloyDB is not served by this instance")
	}
	c, err := api.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: "projects/" + t.Project + "/locations/" + t.Location + "/clusters/" + t.ID})
	if err != nil {
		return "", err
	}
	if c.GetClusterType() == alloydbpb.Cluster_SECONDARY || c.GetState() != alloydbpb.Cluster_READY {
		return "", apierror.FailedPrecondition("the AlloyDB cluster %s is not a READY primary cluster", t.ID)
	}
	if _, err := b.exec(ctx, t); err != nil {
		return "", err
	}
	return c.GetDatabaseVersion().String(), nil
}

// Demote makes an existing destination a replica of src (#2257): a Cloud
// SQL instance is demoted (instances.demote) to a replica of a source
// representation instance, waiting for startExternalSync. An AlloyDB
// cluster has no demotion of its own: its migration subscribes it to the
// source, its databases of the migrated names dropped first.
func (b *dmsBackend) Demote(ctx context.Context, t datamigration.Target, src datamigration.Source) error {
	if !t.CloudSQL {
		return nil
	}
	if err := b.createSourceRep(ctx, t, src); err != nil {
		return err
	}
	return b.sqlOp(ctx, t, http.MethodPost, t.ID+"/demote", map[string]any{
		"demoteContext": map[string]any{"sourceRepresentativeInstanceName": sourceRep(t)}})
}

// dropSubscriptions drops an AlloyDB target's subscriptions, and with them
// their slots on the source; one whose source cannot be reached is
// dropped without its slot.
func (b *dmsBackend) dropSubscriptions(ctx context.Context, t datamigration.Target) error {
	return b.eachSubscription(ctx, t, func(db, sub string) error {
		id := quotePGIdent(sub)
		if _, err := b.QueryTarget(ctx, t, db, "DROP SUBSCRIPTION "+id); err != nil {
			for _, st := range []string{"ALTER SUBSCRIPTION " + id + " DISABLE", "ALTER SUBSCRIPTION " + id + " SET (slot_name = NONE)", "DROP SUBSCRIPTION " + id} {
				if _, err := b.QueryTarget(ctx, t, db, st); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (b *dmsBackend) Reset(ctx context.Context, t datamigration.Target, src datamigration.Source, databases []string) error {
	if t.CloudSQL {
		if !t.Existing {
			return nil
		}
		// An existing instance (#2257): promoted, its migrated databases
		// dropped, and demoted again.
		if err := b.Promote(ctx, t); err != nil && !strings.Contains(err.Error(), "not a read replica") {
			return err
		}
		for _, db := range databases {
			q := "DROP DATABASE IF EXISTS " + quotePGIdent(db) + " WITH (FORCE)"
			dbIn := "template1"
			if t.MySQL() {
				q, dbIn = "DROP DATABASE IF EXISTS `"+strings.ReplaceAll(db, "`", "``")+"`", ""
			} else if db == "postgres" {
				continue
			}
			if _, err := b.QueryTarget(ctx, t, dbIn, q); err != nil {
				return err
			}
		}
		return b.Demote(ctx, t, src)
	}
	if err := b.dropSubscriptions(ctx, t); err != nil {
		return err
	}
	for _, db := range databases {
		if db == "postgres" {
			// The cluster's own postgres database stays; what was migrated
			// into it goes.
			rows, err := b.QueryTarget(ctx, t, db, `SELECT nspname FROM pg_namespace WHERE nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
			  AND nspname NOT LIKE 'pg\_%' AND nspname NOT LIKE 'alloydb%' AND nspname NOT LIKE 'google%'`)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if _, err := b.QueryTarget(ctx, t, db, "DROP SCHEMA "+quotePGIdent(r[0])+" CASCADE"); err != nil {
					return err
				}
			}
			if _, err := b.QueryTarget(ctx, t, db, "CREATE SCHEMA IF NOT EXISTS public"); err != nil {
				return err
			}
			continue
		}
		if _, err := b.QueryTarget(ctx, t, "template1", "DROP DATABASE IF EXISTS "+quotePGIdent(db)+" WITH (FORCE)"); err != nil {
			return err
		}
	}
	run, err := b.exec(ctx, t)
	if err != nil {
		return err
	}
	return run(ctx, nil, io.Discard, "sh", "-c", `rm -f "$(dirname "$PGDATA")/cloudsql-external-synced"`)
}

// staticIPs is the address the destinations' servers reach a source
// from: the instance's cluster node, whose address their connections
// leave by.
func (b *dmsBackend) staticIPs() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}",
		b.cfg.ClusterName()+"-control-plane").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}
