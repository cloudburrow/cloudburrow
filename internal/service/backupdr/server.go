// Package backupdr serves the Backup and DR Service API v1
// (backupdr.googleapis.com, #1433) as far as Cloud SQL's integration uses
// it: backup vaults, backup plans with their rules, backup plan
// associations of a Cloud SQL instance, the data sources they make in the
// plan's vault, and the backups taken of them.
//
// A backup is a real one: a Cloud SQL backup run of the instance (#1330,
// #1380), its databases dumped with the engine's own tools and kept by the
// Cloud SQL Admin API marked as this service's, so it outlives the instance
// as a vault's backup does. Cloud SQL restores it (instances.restoreBackup
// with backupdrBackup), and instances.pointInTimeRestore recovers the data
// source's instance to a time through instances.clone's path; both resolve
// the names here through Resolver.
//
// Other workload types (Compute Engine instances, disks, AlloyDB,
// Filestore) need servers CloudBurrow does not run, and are refused with
// that reason, as backups.restore, whose targets are all Compute Engine
// instances and disks, is.
//
// Every mutation is a long-running operation (google.longrunning), answered
// done or running as the work is, and operations.get answers it
// afterwards. docs/backupdr.md is the user's guide.
package backupdr

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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CloudSQLResourceType is the resourceType of a Cloud SQL instance.
const CloudSQLResourceType = "sqladmin.googleapis.com/Instance"

// Instance is what the service records of a Cloud SQL instance.
type Instance struct {
	DatabaseVersion string
	Tier            string
	CreateTime      string
	Region          string
}

// Run is a backup run's state, as Cloud SQL reports it.
type Run struct {
	Status  string // RUNNING, SUCCESSFUL or FAILED
	Error   string
	EndTime string
}

// CloudSQL is the Cloud SQL Admin API this service backs up.
type CloudSQL interface {
	// Instance is project's instance, if it is served.
	Instance(project, instance string) (Instance, bool)
	// Backup starts a backup of the instance and returns its run's ID and
	// a channel closed when it is taken.
	Backup(project, instance, description string) (string, <-chan struct{}, error)
	// Run is a backup run's state.
	Run(id string) (Run, bool)
	// DeleteRun deletes a backup run.
	DeleteRun(id string) error
}

// apiError is an error with its HTTP status and Google status.
type apiError struct {
	code   int
	status string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func errorf(code int, status, format string, args ...any) error {
	return &apiError{code: code, status: status, msg: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) error {
	return errorf(http.StatusBadRequest, "INVALID_ARGUMENT", format, args...)
}

func notFound(format string, args ...any) error {
	return errorf(http.StatusNotFound, "NOT_FOUND", format, args...)
}

func precondition(format string, args ...any) error {
	return errorf(http.StatusBadRequest, "FAILED_PRECONDITION", format, args...)
}

func unimplemented(what, why string) error {
	return errorf(http.StatusNotImplemented, "UNIMPLEMENTED", "%s is not implemented in CloudBurrow: %s", what, why)
}

// Server serves the API over HTTP.
type Server struct {
	addr  string
	sql   CloudSQL
	state string // the records' file; "" keeps them in memory

	mu   sync.Mutex
	st   state
	ops  map[string]map[string]any
	stop chan struct{}

	srv *http.Server
	ln  net.Listener
	// Observe, if set, is called once per request with the method name and
	// the answer's HTTP status.
	Observe func(method string, code int)
	// now is the clock, for tests.
	now func() time.Time
}

// state is every record, as it is kept.
type state struct {
	Vaults       map[string]*vault       `json:"vaults"`
	Plans        map[string]*plan        `json:"plans"`
	Associations map[string]*association `json:"associations"`
	DataSources  map[string]*dataSource  `json:"dataSources"`
	Backups      map[string]*backup      `json:"backups"`
}

type vault struct {
	Name, Description, CreateTime, UpdateTime, UID, MinRetention, AccessRestriction string
	Labels                                                                          map[string]string
}

type rule struct {
	RuleID          string         `json:"ruleId"`
	RetentionDays   int64          `json:"backupRetentionDays"`
	Schedule        map[string]any `json:"standardSchedule"`
	lastBackupState string
}

type plan struct {
	Name, Description, Vault, ResourceType, CreateTime, UpdateTime string
	LogRetentionDays                                               int64
	Rules                                                          []rule
	Labels                                                         map[string]string
	// Revision is the current revision's number: v0 at creation, one
	// more per patch. Revisions are every revision's snapshot, oldest
	// first.
	Revision  int
	Revisions []planRevision `json:",omitempty"`
}

// planRevision is one revision of a plan: the plan as it was.
type planRevision struct {
	ID, CreateTime string
	Snapshot       map[string]any
}

type association struct {
	Name, Plan, Resource, ResourceType, DataSource, CreateTime, UpdateTime string
	// LastBackup is by rule ID: the last backup's state and time.
	LastBackup map[string]lastBackup
}

type lastBackup struct {
	State, Time, Error string
}

type dataSource struct {
	Name, Project, Instance, Association, CreateTime, UpdateTime string
	Info                                                         Instance
	Labels                                                       map[string]string `json:",omitempty"`
}

type backup struct {
	Name, DataSource, RunID, RuleID, Plan, CreateTime, ExpireTime, Description string
	Labels                                                                     map[string]string
	// Revision is the plan revision the backup was taken under, "" for v0.
	// EnforcedEnd is enforcedRetentionEndTime when a patch set it apart
	// from ExpireTime; UpdateTime is the last patch's.
	Revision, EnforcedEnd, UpdateTime string `json:",omitempty"`
}

// NewServer serves at addr once started, backing up through sql. With a
// state file the records are kept there and survive a restart.
func NewServer(addr string, sql CloudSQL, stateFile string) *Server {
	s := &Server{addr: addr, sql: sql, state: stateFile, ops: map[string]map[string]any{}, now: time.Now}
	s.st = state{Vaults: map[string]*vault{}, Plans: map[string]*plan{}, Associations: map[string]*association{},
		DataSources: map[string]*dataSource{}, Backups: map[string]*backup{}}
	if stateFile != "" {
		if b, err := os.ReadFile(stateFile); err == nil {
			_ = json.Unmarshal(b, &s.st)
		}
	}
	return s
}

func (s *Server) Name() string { return "backupdr" }

// Start binds the listener, serves in the background and takes each
// rule's scheduled backups.
func (s *Server) Start(context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("backupdr: listen on %s: %w", s.addr, err)
	}
	s.ln = ln
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	s.stop = make(chan struct{})
	go func() { _ = s.srv.Serve(ln) }()
	go s.schedule(s.stop)
	return nil
}

// Stop shuts the server down.
func (s *Server) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	close(s.stop)
	return s.srv.Shutdown(ctx)
}

// Addr is the bound address, empty before Start.
func (s *Server) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// saveLocked writes the records, with s.mu held.
func (s *Server) saveLocked() error {
	if s.state == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.state), 0o700); err != nil {
		return err
	}
	tmp := s.state + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.state)
}

// ServeHTTP routes /v1/....
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method, out, err := s.route(r)
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
		ae = &apiError{code: http.StatusInternalServerError, status: "INTERNAL", msg: err.Error()}
	}
	if s.Observe != nil {
		s.Observe(method, ae.code)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(ae.code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": ae.code, "message": ae.msg, "status": ae.status,
	}})
}

var idRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

func checkID(field, id string) error {
	if id == "" {
		return invalid("%s is required.", field)
	}
	if !idRE.MatchString(id) {
		return invalid("%s %q must be 1 to 63 lowercase letters, digits and hyphens, start with a letter and not end with a hyphen.", field, id)
	}
	return nil
}

// param is a query parameter, by its JSON name or its proto field name
// (backupPlanId or backup_plan_id): gRPC transcoding accepts both, and the
// Terraform provider sends the second.
func param(r *http.Request, name string) string {
	q := r.URL.Query()
	if v := q.Get(name); v != "" {
		return v
	}
	var b strings.Builder
	for _, c := range name {
		if c >= 'A' && c <= 'Z' {
			b.WriteByte('_')
			c += 'a' - 'A'
		}
		b.WriteRune(c)
	}
	return q.Get(b.String())
}

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return invalid("Invalid JSON payload received. %v", err)
	}
	return nil
}

func (s *Server) stamp() string { return s.now().UTC().Format(time.RFC3339Nano) }

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// route dispatches a request: v1/projects/{p}/locations/{l}/... with a
// custom verb after a colon in the last segment.
func (s *Server) route(r *http.Request) (string, any, error) {
	path, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	if !ok {
		return "", nil, notFound("%s is not a Backup and DR API v1 path", r.URL.Path)
	}
	verb := ""
	if i := strings.LastIndex(path, ":"); i > strings.LastIndex(path, "/") {
		path, verb = path[:i], path[i+1:]
	}
	p := strings.Split(strings.TrimSuffix(path, "/"), "/")
	if len(p) == 5 && (p[0] == "folders" || p[0] == "organizations") && p[2] == "locations" && p[4] == "resourceBackupConfigs" && verb == "fetch" {
		return "backupdr." + p[0] + ".locations.resourceBackupConfigs.fetch", nil, unimplemented(p[0]+".locations.resourceBackupConfigs.fetch", hierarchyWhy)
	}
	if len(p) < 2 || p[0] != "projects" || p[1] == "" {
		return "", nil, notFound("%s is not a Backup and DR API v1 path", r.URL.Path)
	}
	if len(p) == 3 && p[2] == "locations" && r.Method == http.MethodGet {
		return "backupdr.projects.locations.list", s.listLocations(p[1]), nil
	}
	if len(p) < 4 || p[2] != "locations" || p[3] == "" {
		return "", nil, notFound("%s is not a Backup and DR API v1 path", r.URL.Path)
	}
	loc := strings.Join(p[:4], "/")
	if len(p) == 4 && verb == "" && r.Method == http.MethodGet {
		return "backupdr.projects.locations.get", locationJSON(p[1], p[3]), nil
	}
	rest := p[4:]
	if len(rest) == 0 && verb == "msComplianceMetadata" {
		return "backupdr.projects.locations.managementServers.msComplianceMetadata", nil,
			unimplemented("managementServers.msComplianceMetadata", managementServerWhy)
	}
	if len(rest) == 0 {
		return "", nil, unimplemented("projects.locations."+verb, otherWhy)
	}
	if m, why := refused(rest, verb); why != "" {
		return m, nil, unimplemented(strings.TrimPrefix(m, "backupdr.projects.locations."), why)
	}
	get, list, post, del := r.Method == http.MethodGet, r.Method == http.MethodGet, r.Method == http.MethodPost, r.Method == http.MethodDelete
	patch := r.Method == http.MethodPatch
	switch rest[0] {
	case "operations":
		switch {
		case len(rest) == 1 && list:
			return "backupdr.projects.locations.operations.list", s.listOperations(loc), nil
		case len(rest) == 2 && get && verb == "":
			out, err := s.operation(loc + "/operations/" + rest[1])
			return "backupdr.projects.locations.operations.get", out, err
		case len(rest) == 2 && post && verb == "cancel":
			out, err := s.cancelOperation(loc + "/operations/" + rest[1])
			return "backupdr.projects.locations.operations.cancel", out, err
		case len(rest) == 2 && del && verb == "":
			out, err := s.deleteOperation(loc + "/operations/" + rest[1])
			return "backupdr.projects.locations.operations.delete", out, err
		}
	case "dataSourceReferences":
		switch {
		case len(rest) == 1 && list && verb == "":
			return "backupdr.projects.locations.dataSourceReferences.list", s.listReferences(loc), nil
		case len(rest) == 1 && get && verb == "fetchForResourceType":
			out, err := s.fetchReferences(r, loc)
			return "backupdr.projects.locations.dataSourceReferences.fetchForResourceType", out, err
		case len(rest) == 2 && get && verb == "":
			out, err := s.getReference(loc + "/dataSourceReferences/" + rest[1])
			return "backupdr.projects.locations.dataSourceReferences.get", out, err
		}
	case "backupVaults":
		switch {
		case len(rest) == 1 && list && verb == "":
			return "backupdr.projects.locations.backupVaults.list", s.listVaults(loc), nil
		case len(rest) == 1 && get && verb == "fetchUsable":
			return "backupdr.projects.locations.backupVaults.fetchUsable", s.listVaults(loc), nil
		case len(rest) == 2 && patch && verb == "":
			out, err := s.patchVault(r, loc+"/backupVaults/"+rest[1])
			return "backupdr.projects.locations.backupVaults.patch", out, err
		case len(rest) == 2 && post && verb == "testIamPermissions":
			out, err := s.testVaultPermissions(r, loc+"/backupVaults/"+rest[1])
			return "backupdr.projects.locations.backupVaults.testIamPermissions", out, err
		case len(rest) == 4 && rest[2] == "dataSources" && patch && verb == "":
			out, err := s.patchDataSource(r, strings.Join(p, "/"))
			return "backupdr.projects.locations.backupVaults.dataSources.patch", out, err
		case len(rest) == 5 && rest[2] == "dataSources" && rest[4] == "backups" && get && verb == "fetchForResourceType":
			out, err := s.fetchBackups(r, strings.Join(p[:len(p)-1], "/"))
			return "backupdr.projects.locations.backupVaults.dataSources.backups.fetchForResourceType", out, err
		case len(rest) == 6 && rest[2] == "dataSources" && rest[4] == "backups" && patch && verb == "":
			out, err := s.patchBackup(r, strings.Join(p, "/"))
			return "backupdr.projects.locations.backupVaults.dataSources.backups.patch", out, err
		case len(rest) == 1 && post && verb == "":
			out, err := s.createVault(r, loc)
			return "backupdr.projects.locations.backupVaults.create", out, err
		case len(rest) == 2 && get && verb == "":
			out, err := s.getVault(loc + "/backupVaults/" + rest[1])
			return "backupdr.projects.locations.backupVaults.get", out, err
		case len(rest) == 2 && del && verb == "":
			out, err := s.deleteVault(r, loc+"/backupVaults/"+rest[1])
			return "backupdr.projects.locations.backupVaults.delete", out, err
		case len(rest) == 3 && rest[2] == "dataSources" && list && verb == "":
			out, err := s.listDataSources(loc + "/backupVaults/" + rest[1])
			return "backupdr.projects.locations.backupVaults.dataSources.list", out, err
		case len(rest) == 4 && rest[2] == "dataSources" && get && verb == "":
			out, err := s.getDataSource(strings.Join(p, "/"))
			return "backupdr.projects.locations.backupVaults.dataSources.get", out, err
		case len(rest) == 5 && rest[2] == "dataSources" && rest[4] == "backups" && list && verb == "":
			out, err := s.listBackups(strings.Join(p[:len(p)-1], "/"))
			return "backupdr.projects.locations.backupVaults.dataSources.backups.list", out, err
		case len(rest) == 6 && rest[2] == "dataSources" && rest[4] == "backups" && get && verb == "":
			out, err := s.getBackup(strings.Join(p, "/"))
			return "backupdr.projects.locations.backupVaults.dataSources.backups.get", out, err
		case len(rest) == 6 && rest[2] == "dataSources" && rest[4] == "backups" && del && verb == "":
			out, err := s.deleteBackup(strings.Join(p, "/"))
			return "backupdr.projects.locations.backupVaults.dataSources.backups.delete", out, err
		case len(rest) == 6 && rest[2] == "dataSources" && rest[4] == "backups" && post && verb == "restore":
			out, err := s.restoreBackup(r, strings.Join(p, "/"))
			return "backupdr.projects.locations.backupVaults.dataSources.backups.restore", out, err
		}
	case "backupPlans":
		switch {
		case len(rest) == 1 && list && verb == "":
			return "backupdr.projects.locations.backupPlans.list", s.listPlans(loc), nil
		case len(rest) == 1 && post && verb == "":
			out, err := s.createPlan(r, loc)
			return "backupdr.projects.locations.backupPlans.create", out, err
		case len(rest) == 2 && get && verb == "":
			out, err := s.getPlan(loc + "/backupPlans/" + rest[1])
			return "backupdr.projects.locations.backupPlans.get", out, err
		case len(rest) == 2 && del && verb == "":
			out, err := s.deletePlan(loc + "/backupPlans/" + rest[1])
			return "backupdr.projects.locations.backupPlans.delete", out, err
		case len(rest) == 2 && patch && verb == "":
			out, err := s.patchPlan(r, loc+"/backupPlans/"+rest[1])
			return "backupdr.projects.locations.backupPlans.patch", out, err
		case len(rest) == 3 && rest[2] == "revisions" && list && verb == "":
			out, err := s.listRevisions(loc + "/backupPlans/" + rest[1])
			return "backupdr.projects.locations.backupPlans.revisions.list", out, err
		case len(rest) == 4 && rest[2] == "revisions" && get && verb == "":
			out, err := s.getRevision(loc+"/backupPlans/"+rest[1], rest[3])
			return "backupdr.projects.locations.backupPlans.revisions.get", out, err
		}
	case "backupPlanAssociations":
		switch {
		case len(rest) == 1 && list && verb == "":
			return "backupdr.projects.locations.backupPlanAssociations.list", s.listAssociations(loc), nil
		case len(rest) == 1 && post && verb == "":
			out, err := s.createAssociation(r, loc)
			return "backupdr.projects.locations.backupPlanAssociations.create", out, err
		case len(rest) == 1 && get && verb == "fetchForResourceType":
			out, err := s.fetchAssociations(r, loc)
			return "backupdr.projects.locations.backupPlanAssociations.fetchForResourceType", out, err
		case len(rest) == 2 && patch && verb == "":
			out, err := s.patchAssociation(r, loc+"/backupPlanAssociations/"+rest[1])
			return "backupdr.projects.locations.backupPlanAssociations.patch", out, err
		case len(rest) == 2 && get && verb == "":
			out, err := s.getAssociation(loc + "/backupPlanAssociations/" + rest[1])
			return "backupdr.projects.locations.backupPlanAssociations.get", out, err
		case len(rest) == 2 && del && verb == "":
			out, err := s.deleteAssociation(loc + "/backupPlanAssociations/" + rest[1])
			return "backupdr.projects.locations.backupPlanAssociations.delete", out, err
		case len(rest) == 2 && post && verb == "triggerBackup":
			out, err := s.triggerBackup(r, loc+"/backupPlanAssociations/"+rest[1])
			return "backupdr.projects.locations.backupPlanAssociations.triggerBackup", out, err
		}
	}
	what := strings.Join(rest, "/")
	if verb != "" {
		what += ":" + verb
	}
	return "backupdr." + rest[0], nil, unimplemented(r.Method+" "+what, otherWhy)
}

// refused is the method a path names and why it is refused, when it is
// one CloudBurrow cannot serve faithfully; why is "" otherwise.
func refused(rest []string, verb string) (string, string) {
	const pre = "backupdr.projects.locations."
	switch {
	case rest[0] == "managementServers":
		m := map[string]string{"getIamPolicy": "getIamPolicy", "setIamPolicy": "setIamPolicy", "testIamPermissions": "testIamPermissions"}[verb]
		if m == "" {
			return pre + "managementServers", managementServerWhy
		}
		return pre + "managementServers." + m, managementServerWhy
	case len(rest) == 1 && rest[0] == "trial":
		if verb == "" {
			return pre + "getTrial", trialWhy
		}
		return pre + "trial." + verb, trialWhy
	case len(rest) == 1 && rest[0] == "resourceBackupConfigs":
		return pre + "resourceBackupConfigs." + map[bool]string{true: "fetch", false: "list"}[verb == "fetch"], inventoryWhy
	case len(rest) == 1 && rest[0] == "serviceConfig" && verb == "initialize":
		return pre + "serviceConfig.initialize", initializeWhy
	case len(rest) == 4 && rest[0] == "backupVaults" && rest[2] == "dataSources":
		switch verb {
		case "initiateBackup", "finalizeBackup", "abandonBackup", "fetchAccessToken", "setInternalStatus", "remove":
			return pre + "backupVaults.dataSources." + verb, applianceWhy
		}
	}
	return "", ""
}

// otherWhy is why a method outside Cloud SQL's integration is refused.
const otherWhy = "CloudBurrow serves the Backup and DR Service for Cloud SQL instances: backup vaults, backup plans, " +
	"backup plan associations, data sources, backups and operations (docs/backupdr.md); management servers, " +
	"backup appliances and the other workload types (Compute Engine, disks, AlloyDB, Filestore) are not served"

func locationJSON(project, loc string) map[string]any {
	return map[string]any{"name": "projects/" + project + "/locations/" + loc, "locationId": loc, "displayName": loc}
}

// Locations are the regions the service lists.
var Locations = []string{"us-central1", "us-east1", "us-west1", "europe-west1", "asia-east1"}

func (s *Server) listLocations(project string) any {
	out := []any{}
	for _, l := range Locations {
		out = append(out, locationJSON(project, l))
	}
	return map[string]any{"locations": out}
}

// --- operations -------------------------------------------------------

func (s *Server) newOp(loc, verb, target string) (string, map[string]any) {
	name := loc + "/operations/operation-" + strconv.FormatInt(s.now().UnixNano(), 10) + "-" + newID()[:12]
	now := s.stamp()
	op := map[string]any{"name": name, "done": false, "metadata": map[string]any{
		"@type": "type.googleapis.com/google.cloud.backupdr.v1.OperationMetadata", "createTime": now,
		"target": target, "verb": verb, "apiVersion": "v1", "requestedCancellation": false}}
	return name, op
}

// opDone records an operation finished with resp (a resource, or nil for
// google.protobuf.Empty) or err.
func (s *Server) opDone(loc, verb, target string, resp map[string]any, typ string, err error) map[string]any {
	name, op := s.newOp(loc, verb, target)
	s.finishOp(op, resp, typ, err)
	s.mu.Lock()
	s.ops[name] = op
	cp := copyMap(op)
	s.mu.Unlock()
	return cp
}

func (s *Server) finishOp(op map[string]any, resp map[string]any, typ string, err error) {
	op["done"] = true
	op["metadata"].(map[string]any)["endTime"] = s.stamp()
	if err != nil {
		code := 13
		var ae *apiError
		if errors.As(err, &ae) {
			code = grpcCode(ae.code)
		}
		op["error"] = map[string]any{"code": code, "message": err.Error()}
		return
	}
	if resp == nil {
		resp = map[string]any{}
		typ = "google.protobuf.Empty"
	}
	r := copyMap(resp)
	r["@type"] = "type.googleapis.com/" + typ
	op["response"] = r
}

func grpcCode(httpCode int) int {
	switch httpCode {
	case http.StatusBadRequest:
		return 3
	case http.StatusNotFound:
		return 5
	case http.StatusConflict:
		return 6
	case http.StatusNotImplemented:
		return 12
	}
	return 13
}

func copyMap(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func (s *Server) operation(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[name]
	if !ok {
		return nil, notFound("Operation %s does not exist.", name)
	}
	return copyMap(op), nil
}

func (s *Server) listOperations(loc string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for n := range s.ops {
		if strings.HasPrefix(n, loc+"/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := []any{}
	for _, n := range names {
		out = append(out, copyMap(s.ops[n]))
	}
	return map[string]any{"operations": out}
}

// --- vaults -----------------------------------------------------------

func (s *Server) vaultJSON(v *vault) map[string]any {
	count := 0
	for _, b := range s.st.Backups {
		if strings.HasPrefix(b.Name, v.Name+"/") {
			count++
		}
	}
	deletable := true
	for _, d := range s.st.DataSources {
		if strings.HasPrefix(d.Name, v.Name+"/") {
			deletable = false
		}
	}
	out := map[string]any{"name": v.Name, "createTime": v.CreateTime, "updateTime": v.UpdateTime,
		"state": "ACTIVE", "uid": v.UID, "deletable": deletable, "backupCount": strconv.Itoa(count),
		"backupMinimumEnforcedRetentionDuration": v.MinRetention, "etag": strconv.Quote(v.UpdateTime),
		"serviceAccount":    "service-backupdr@cloudburrow.iam.gserviceaccount.com",
		"accessRestriction": v.AccessRestriction, "totalStoredBytes": "0"}
	if v.Description != "" {
		out["description"] = v.Description
	}
	if len(v.Labels) > 0 {
		out["labels"] = v.Labels
	}
	return out
}

func (s *Server) createVault(r *http.Request, loc string) (any, error) {
	id := param(r, "backupVaultId")
	if err := checkID("backupVaultId", id); err != nil {
		return nil, err
	}
	var body struct {
		Description       string            `json:"description"`
		Labels            map[string]string `json:"labels"`
		MinRetention      string            `json:"backupMinimumEnforcedRetentionDuration"`
		AccessRestriction string            `json:"accessRestriction"`
		EncryptionConfig  json.RawMessage   `json:"encryptionConfig"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.MinRetention == "" {
		return nil, invalid("backupVault.backupMinimumEnforcedRetentionDuration is required.")
	}
	if d, err := parseDuration(body.MinRetention); err != nil || d < 0 {
		return nil, invalid("backupVault.backupMinimumEnforcedRetentionDuration %q is not a duration such as \"100000s\".", body.MinRetention)
	}
	if len(body.EncryptionConfig) > 0 && string(body.EncryptionConfig) != "null" && string(body.EncryptionConfig) != "{}" {
		return nil, unimplemented("backupVault.encryptionConfig", "a customer-managed encryption key needs Cloud KMS to wrap the vault's backups, "+
			"and CloudBurrow's Backup and DR Service does not use Cloud KMS: a vault's backups are Cloud SQL backup runs, unencrypted at rest (docs/backupdr.md)")
	}
	switch body.AccessRestriction {
	case "":
		body.AccessRestriction = "WITHIN_ORGANIZATION"
	case "WITHIN_PROJECT", "WITHIN_ORGANIZATION", "UNRESTRICTED", "WITHIN_ORG_BUT_UNRESTRICTED_FOR_BA":
	default:
		return nil, invalid("backupVault.accessRestriction %q is not one of WITHIN_PROJECT, WITHIN_ORGANIZATION, UNRESTRICTED.", body.AccessRestriction)
	}
	name := loc + "/backupVaults/" + id
	s.mu.Lock()
	if s.st.Vaults[name] != nil {
		s.mu.Unlock()
		return nil, errorf(http.StatusConflict, "ALREADY_EXISTS", "Backup vault %s already exists.", name)
	}
	now := s.stamp()
	v := &vault{Name: name, Description: body.Description, Labels: body.Labels, CreateTime: now, UpdateTime: now,
		UID: newID(), MinRetention: body.MinRetention, AccessRestriction: body.AccessRestriction}
	s.st.Vaults[name] = v
	err := s.saveLocked()
	out := s.vaultJSON(v)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.opDone(loc, "create", name, out, "google.cloud.backupdr.v1.BackupVault", nil), nil
}

func (s *Server) getVault(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.st.Vaults[name]
	if v == nil {
		return nil, notFound("Backup vault %s does not exist.", name)
	}
	return s.vaultJSON(v), nil
}

func (s *Server) listVaults(loc string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []any{}
	for _, n := range sortedKeys(s.st.Vaults) {
		if under(n, loc, "/backupVaults/") {
			out = append(out, s.vaultJSON(s.st.Vaults[n]))
		}
	}
	return map[string]any{"backupVaults": out}
}

// under is whether name is a direct child of kind ("/backupVaults/") under
// parent, whose segments may be "-" for every one.
func under(name, parent, kind string) bool {
	pre, id, ok := strings.Cut(name, kind)
	return ok && id != "" && !strings.Contains(id, "/") && match(parent, pre)
}

// match is whether name has pattern's segments, "-" matching any.
func match(pattern, name string) bool {
	p, n := strings.Split(pattern, "/"), strings.Split(name, "/")
	if len(p) != len(n) {
		return false
	}
	for i := range p {
		if p[i] != n[i] && p[i] != "-" {
			return false
		}
	}
	return true
}

func (s *Server) deleteVault(r *http.Request, name string) (any, error) {
	force := param(r, "force") == "true"
	// ignoreInactiveDatasources lets a vault go with the data sources no
	// association protects any more, and their backups.
	inactive := param(r, "ignoreInactiveDatasources") == "true"
	loc := name[:strings.Index(name, "/backupVaults/")]
	s.mu.Lock()
	v := s.st.Vaults[name]
	if v == nil {
		s.mu.Unlock()
		if param(r, "allowMissing") == "true" {
			return s.opDone(loc, "delete", name, nil, "", nil), nil
		}
		return nil, notFound("Backup vault %s does not exist.", name)
	}
	for _, p := range s.st.Plans {
		if p.Vault == name && param(r, "ignoreBackupPlanReferences") != "true" {
			s.mu.Unlock()
			return nil, precondition("Backup vault %s is used by backup plan %s; delete the plan first.", name, p.Name)
		}
	}
	var runs []string
	for n, d := range s.st.DataSources {
		if !strings.HasPrefix(n, name+"/") {
			continue
		}
		if !force && !(inactive && s.st.Associations[d.Association] == nil) {
			s.mu.Unlock()
			return nil, precondition("Backup vault %s holds data source %s; delete its backups, or pass force=true.", name, d.Name)
		}
		for bn, b := range s.st.Backups {
			if b.DataSource == n {
				runs = append(runs, b.RunID)
				delete(s.st.Backups, bn)
			}
		}
		delete(s.st.DataSources, n)
	}
	delete(s.st.Vaults, name)
	err := s.saveLocked()
	s.mu.Unlock()
	for _, id := range runs {
		_ = s.sql.DeleteRun(id)
	}
	if err != nil {
		return nil, err
	}
	return s.opDone(loc, "delete", name, nil, "", nil), nil
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseDuration parses a google.protobuf.Duration's JSON ("86400s").
func parseDuration(s string) (time.Duration, error) {
	if !strings.HasSuffix(s, "s") {
		return 0, errors.New("no unit")
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(f * float64(time.Second)), nil
}
