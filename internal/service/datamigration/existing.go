package datamigration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// A migration into an existing destination (#2257), as Google documents
// it (https://docs.cloud.google.com/database-migration/docs/mysql/create-migration-job-existing-instance
// and .../postgres/guide-to-existing-instance, read 2026-11-25): the
// destination profile names a Cloud SQL instance (cloudSqlId) or an
// AlloyDB cluster (alloydbClusterId, or an alloydb profile without
// settings) that is standalone and in the job's region; the job's
// destination is demoted
// (https://docs.cloud.google.com/database-migration/docs/reference/rest/v1/projects.locations.migrationJobs/demoteDestination,
// "Demotes the destination database to become a read replica of the
// source. This is applicable for the following migrations: MySQL to Cloud
// SQL for MySQL, PostgreSQL to Cloud SQL for PostgreSQL, PostgreSQL to
// AlloyDB for PostgreSQL.") before it starts; and the migration then runs
// as into a created destination. The destination "must not contain
// existing user data"; a PostgreSQL destination's databases of the
// migrated names are dropped by its demotion.

// checkExistingDestination validates an existing destination's profile.
func (s *Server) checkExistingDestination(p *dmspb.ConnectionProfile) error {
	var t Target
	t.CloudSQL, t.ID = existingID(p)
	if !instanceIDRE.MatchString(t.ID) {
		return apierror.InvalidArgument("the destination instance or cluster ID %q is not valid", t.ID)
	}
	if m := p.GetMysql(); m != nil && (m.GetHost() != "" || m.GetUsername() != "") && str(p, "role") != "DESTINATION" {
		return apierror.InvalidArgument("connection_profile.mysql: a destination profile of the Cloud SQL instance %s takes no host", t.ID)
	}
	if str(p, "role") == "SOURCE" {
		return apierror.InvalidArgument("connection_profile.role SOURCE: a profile of the Cloud SQL instance or AlloyDB cluster %s is a destination", t.ID)
	}
	return nil
}

// idRE is a Cloud SQL instance or AlloyDB cluster ID.
var instanceIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,97}$`)

// isExisting reports whether a job's destination is an existing one; the
// caller holds mu.
func (s *Server) isExisting(j *dmspb.MigrationJob) bool {
	p := s.profiles[j.GetDestination()]
	return p != nil && existingDestination(p)
}

// demoteDestination is migrationJobs.demoteDestination: an existing
// destination becomes a replica of the job's source, waiting for the
// job's start.
func (s *Server) demoteDestination(name string) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.jobLocked(name)
	if err != nil {
		return nil, err
	}
	if !s.isExisting(j) {
		return nil, failedPrecondition("migration job %s's destination %s was created by Database Migration Service: only an existing destination is demoted", name, j.GetDestination())
	}
	if j.State != dmspb.MigrationJob_NOT_STARTED && j.State != dmspb.MigrationJob_DRAFT {
		return nil, failedPrecondition("migration job %s is %s: its destination is demoted before it starts", name, j.State)
	}
	if s.demoted[name] {
		return nil, failedPrecondition("migration job %s's destination is already demoted", name)
	}
	if s.demoting[name] {
		return nil, failedPrecondition("migration job %s's destination is being demoted", name)
	}
	jc, err := s.contextLocked(j)
	if err != nil {
		return nil, err
	}
	op, err := s.startOp(parentOf(name), "demoteDestination", name, "")
	if err != nil {
		return nil, err
	}
	s.demoting[name] = true
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	opName := op.Name
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), destinationTimeout)
		defer cancel()
		err := s.demote(ctx, jc)
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.demoting, name)
		cur := s.jobs[name]
		switch {
		case cur == nil:
			s.finishOp(opName, nil, apierror.Aborted("migration job %s was deleted", name))
		case err != nil:
			s.finishOp(opName, nil, err)
		default:
			s.demoted[name] = true
			cur.UpdateTime = ts(s.now())
			s.finishOp(opName, s.answer(cur), nil)
		}
		_ = s.save()
	})
	return clone(op), nil
}

// demote checks an existing destination is empty of the source's
// databases (a MySQL one; a PostgreSQL one's are dropped) and demotes it.
func (s *Server) demote(ctx context.Context, jc jobContext) error {
	if jc.target.MySQL() {
		if errs := s.verifyMySQLEmpty(ctx, jc); len(errs) > 0 {
			return verificationFailed(errs)
		}
	}
	return s.opts.Backend.Demote(ctx, jc.target, jc.src)
}

// verifyMySQLEmpty reports the user databases an existing MySQL
// destination holds: "The destination instance must not contain existing
// user data".
func (s *Server) verifyMySQLEmpty(ctx context.Context, jc jobContext) []*dmspb.MigrationJobVerificationError {
	rows, err := s.opts.Backend.QueryTarget(ctx, jc.target, "", `SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN `+mysqlSystem+` ORDER BY 1`)
	if err != nil {
		return []*dmspb.MigrationJobVerificationError{verificationError(errRDBMS, "The destination's databases could not be read", err.Error())}
	}
	var dbs []string
	for _, r := range rows {
		if len(r) > 0 && r[0] != "" {
			dbs = append(dbs, r[0])
		}
	}
	if len(dbs) == 0 {
		return nil
	}
	return []*dmspb.MigrationJobVerificationError{verificationError(errConflicting,
		"The destination instance "+jc.target.ID+" has user databases: "+strings.Join(dbs, ", "),
		"The destination instance must not contain existing user data; drop them, or migrate into another instance.")}
}

// ---- Objects (#2257) ----

// JobObject is a migration job object: a database of the source that
// fetchSourceObjects found
// (https://docs.cloud.google.com/database-migration/docs/reference/rest/v1/projects.locations.migrationJobs.objects,
// read 2026-11-25). Its state follows its job's.
type JobObject struct {
	ID         string    `json:"id"`
	Database   string    `json:"database"`
	CreateTime time.Time `json:"createTime"`
	UpdateTime time.Time `json:"updateTime"`
}

// objectID is a database's object ID (UNVERIFIED: Google's are opaque).
// Docs re-read 2026-11-25, still unanswered: https://docs.cloud.google.com/database-migration/docs/reference/rest/v1/projects.locations.migrationJobs.objects
func objectID(job, db string) string {
	h := sha256.Sum256([]byte(job + "\x00" + db))
	return hex.EncodeToString(h[:8])
}

// sourceObjects is a job's objectsConfig.sourceObjectsConfig.
type sourceObjects struct {
	Excluded []objectConfig `json:"excludedObjectConfigs"`
	Included []objectConfig `json:"objectConfigs"`
	Type     string         `json:"objectsSelectionType"`
}

type objectConfig struct {
	ID objectIdentifier `json:"objectIdentifier"`
}

type objectIdentifier struct {
	Database string `json:"database,omitempty"`
	Schema   string `json:"schema,omitempty"`
	Table    string `json:"table,omitempty"`
	Type     string `json:"type,omitempty"`
}

func objectsOf(j *dmspb.MigrationJob) (sourceObjects, bool) {
	var c struct {
		Source *sourceObjects `json:"sourceObjectsConfig"`
	}
	if !decodeField(j, "objectsConfig", &c) || c.Source == nil {
		return sourceObjects{}, false
	}
	return *c.Source, true
}

// checkObjects validates a job's objectsConfig: databases of a source
// selected (SPECIFIED_OBJECTS) or excluded. Schemas and tables are
// selected only in conversion workspaces' migrations, which are not
// served.
func checkObjects(j *dmspb.MigrationJob) error {
	c, ok := objectsOf(j)
	if !ok {
		return nil
	}
	switch c.Type {
	case "", "OBJECTS_SELECTION_TYPE_UNSPECIFIED", "ALL_OBJECTS":
		if len(c.Included) > 0 {
			return apierror.InvalidArgument("migration_job.objects_config.source_objects_config.object_configs are for objects_selection_type SPECIFIED_OBJECTS")
		}
	case "SPECIFIED_OBJECTS":
		if len(c.Included) == 0 {
			return apierror.InvalidArgument("migration_job.objects_config.source_objects_config.object_configs: SPECIFIED_OBJECTS selects at least one object")
		}
	default:
		return apierror.InvalidArgument("migration_job.objects_config.source_objects_config.objects_selection_type %q is not valid", c.Type)
	}
	for _, o := range append(append([]objectConfig{}, c.Included...), c.Excluded...) {
		switch {
		case o.ID.Type != "" && o.ID.Type != "DATABASE" && o.ID.Type != "MIGRATION_JOB_OBJECT_TYPE_UNSPECIFIED", o.ID.Schema != "", o.ID.Table != "":
			return unimplemented("migration_job.objects_config's schema and table objects",
				"a homogeneous migration selects databases; schemas and tables are selected in conversion workspaces, which are not served (docs/gaps.md)")
		case o.ID.Database == "":
			return apierror.InvalidArgument("migration_job.objects_config: an object identifier's database is required")
		}
	}
	return nil
}

// selected filters a source's databases by the job's objectsConfig; nil
// all is "every database", which a MySQL source's Backend finds.
func selected(j *dmspb.MigrationJob, all []string) []string {
	c, ok := objectsOf(j)
	if !ok {
		return all
	}
	inc, exc := map[string]bool{}, map[string]bool{}
	for _, o := range c.Included {
		inc[o.ID.Database] = true
	}
	for _, o := range c.Excluded {
		exc[o.ID.Database] = true
	}
	var out []string
	if c.Type == "SPECIFIED_OBJECTS" {
		if all == nil {
			for _, o := range c.Included {
				if !exc[o.ID.Database] {
					out = append(out, o.ID.Database)
				}
			}
			sort.Strings(out)
			return out
		}
		for _, d := range all {
			if inc[d] && !exc[d] {
				out = append(out, d)
			}
		}
		return out
	}
	if len(exc) == 0 {
		return all
	}
	for _, d := range all {
		if !exc[d] {
			out = append(out, d)
		}
	}
	return out
}

// isSelected reports whether the job migrates db.
func isSelected(j *dmspb.MigrationJob, db string) bool {
	return len(selected(j, []string{db})) == 1
}

// fetchSourceObjects is migrationJobs.fetchSourceObjects: the source's
// databases, read from the destination's server, become the job's
// objects. "PostgreSQL to Cloud SQL for PostgreSQL, PostgreSQL to AlloyDB
// for PostgreSQL" (the method's reference page, read 2026-11-25).
func (s *Server) fetchSourceObjects(name string) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.jobLocked(name)
	if err != nil {
		return nil, err
	}
	if src := s.profiles[j.GetSource()]; src == nil || src.GetPostgresql() == nil {
		return nil, apierror.InvalidArgument("migration job %s: fetchSourceObjects is for a PostgreSQL source (PostgreSQL to Cloud SQL for PostgreSQL or AlloyDB for PostgreSQL)", name)
	}
	jc, err := s.contextLocked(j)
	if err != nil {
		return nil, err
	}
	op, err := s.startOp(parentOf(name), "fetchSourceObjects", name, "")
	if err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	opName := op.Name
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
		defer cancel()
		dbs, err := s.allSourceDatabases(ctx, jc)
		s.mu.Lock()
		defer s.mu.Unlock()
		cur := s.jobs[name]
		switch {
		case cur == nil:
			s.finishOp(opName, nil, apierror.Aborted("migration job %s was deleted", name))
		case err != nil:
			s.finishOp(opName, nil, connErrorStatus(err))
		default:
			now := s.now()
			prev := map[string]*JobObject{}
			for _, o := range s.objects[name] {
				prev[o.Database] = o
			}
			var next []*JobObject
			for _, db := range dbs {
				if o := prev[db]; o != nil {
					next = append(next, o)
					continue
				}
				next = append(next, &JobObject{ID: objectID(name, db), Database: db, CreateTime: now, UpdateTime: now})
			}
			s.objects[name] = next
			// UNVERIFIED: the operation's response is the job; the
			// reference does not name it.
			// Docs re-read 2026-11-25, still unanswered: https://docs.cloud.google.com/database-migration/docs/reference/rest/v1/projects.locations.migrationJobs/fetchSourceObjects
			s.finishOp(opName, s.answer(cur), nil)
		}
		_ = s.save()
	})
	return clone(op), nil
}

// connErrorStatus is a source query's failure as an operation's error.
func connErrorStatus(err error) error {
	if ce, ok := err.(*ConnError); ok {
		return failedPrecondition("the destination could not reach the source: %s", ce.Msg)
	}
	return err
}

// objectJSON is a JobObject as the API answers it; the caller holds mu.
func (s *Server) objectJSON(j *dmspb.MigrationJob, o *JobObject) map[string]any {
	state, phase := "NOT_STARTED", ""
	switch {
	case !isSelected(j, o.Database):
		state = "NOT_SELECTED"
	case j.State == dmspb.MigrationJob_RUNNING || j.State == dmspb.MigrationJob_STARTING || j.State == dmspb.MigrationJob_RESTARTING ||
		j.State == dmspb.MigrationJob_RESUMING:
		state = "RUNNING"
		if j.Phase != dmspb.MigrationJob_PHASE_UNSPECIFIED {
			phase = j.Phase.String()
		}
	case j.State == dmspb.MigrationJob_STOPPING || j.State == dmspb.MigrationJob_STOPPED:
		state = j.State.String()
	case j.State == dmspb.MigrationJob_COMPLETED:
		state = "COMPLETED"
	case j.State == dmspb.MigrationJob_FAILED:
		state = "FAILED"
	case j.State == dmspb.MigrationJob_DELETING:
		state = "DELETING"
	}
	out := map[string]any{
		"name":         j.GetName() + "/objects/" + o.ID,
		"sourceObject": map[string]any{"database": o.Database, "type": "DATABASE"},
		"state":        state,
		"createTime":   o.CreateTime.UTC().Format(time.RFC3339Nano),
		"updateTime":   j.GetUpdateTime().AsTime().UTC().Format(time.RFC3339Nano),
	}
	if o.UpdateTime.After(j.GetUpdateTime().AsTime()) {
		out["updateTime"] = o.UpdateTime.UTC().Format(time.RFC3339Nano)
	}
	if phase != "" {
		out["phase"] = phase
	}
	if state == "FAILED" && j.GetError() != nil {
		out["error"] = map[string]any{"code": j.GetError().GetCode(), "message": j.GetError().GetMessage()}
	}
	return out
}

var (
	objectRE  = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/migrationJobs/[^/]+)/objects/([^/]+)$`)
	jobPathRE = regexp.MustCompile(`^/v1/(projects/[^/]+/locations/[^/]+/migrationJobs/[^/:]+)(:demoteDestination|:fetchSourceObjects|/objects|/objects:lookup|/objects/[^/:]+)$`)
)

// ServeJobJSON answers the JSON API's methods clouddmspb has no gRPC
// method for, reporting whether r was one: migrationJobs.demoteDestination,
// .fetchSourceObjects and .objects.get, .list and .lookup.
func (s *Server) ServeJobJSON(w http.ResponseWriter, r *http.Request) bool {
	m := jobPathRE.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return false
	}
	job, rest_ := m[1], m[2]
	var out any
	var err error
	switch {
	case rest_ == ":demoteDestination" && r.Method == http.MethodPost:
		var op *longrunningpb.Operation
		if err = emptyBody(r, nil); err == nil {
			if op, err = s.demoteDestination(job); err == nil {
				out = opJSON(op)
			}
		}
	case rest_ == ":fetchSourceObjects" && r.Method == http.MethodGet:
		var op *longrunningpb.Operation
		if op, err = s.fetchSourceObjects(job); err == nil {
			out = opJSON(op)
		}
	case rest_ == "/objects" && r.Method == http.MethodGet:
		out, err = s.listObjects(job, r.URL.Query().Get("pageToken"), r.URL.Query().Get("pageSize"))
	case rest_ == "/objects:lookup" && r.Method == http.MethodPost:
		var body struct {
			ID *objectIdentifier `json:"sourceObjectIdentifier"`
		}
		if err = emptyBody(r, &body); err == nil {
			if body.ID == nil {
				err = apierror.InvalidArgument("source_object_identifier is required")
			} else {
				out, err = s.lookupObject(job, *body.ID)
			}
		}
	case strings.HasPrefix(rest_, "/objects/") && r.Method == http.MethodGet:
		out, err = s.getObject(job + rest_)
	default:
		err = &apierror.Error{Code: 12, Message: fmt.Sprintf("method %s is not allowed on %s", r.Method, r.URL.Path)}
	}
	if err != nil {
		rest.WriteError(w, err)
		return true
	}
	_ = rest.WriteJSON(w, http.StatusOK, out)
	return true
}

// emptyBody decodes r's JSON body into dst, or checks it is empty.
func emptyBody(r *http.Request, dst any) error {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if strings.TrimSpace(string(b)) == "" {
		return nil
	}
	if dst == nil {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return apierror.InvalidArgument("Invalid JSON payload received: %v", err)
		}
		for k := range m {
			return apierror.InvalidArgument("Invalid JSON payload received. Unknown name %q: Cannot find field.", k)
		}
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apierror.InvalidArgument("Invalid JSON payload received: %v", err)
	}
	return nil
}

// opJSON is an operation's JSON, its response's overlay fields kept.
func opJSON(op *longrunningpb.Operation) json.RawMessage {
	b, err := overlay.Marshal(op)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func (s *Server) listObjects(job, token, size string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.jobLocked(job)
	if err != nil {
		return nil, err
	}
	n := 50 // "Maximum number of objects to return. Default is 50."
	if size != "" {
		v, err := strconv.Atoi(size)
		if err != nil || v < 0 {
			return nil, apierror.InvalidArgument("page_size %q is not valid", size)
		}
		if v > 0 {
			n = v
		}
	}
	all := s.objects[job]
	start := 0
	if token != "" {
		v, err := strconv.Atoi(strings.TrimPrefix(token, "o"))
		if err != nil || !strings.HasPrefix(token, "o") || v < 0 || v > len(all) {
			return nil, apierror.InvalidArgument("page_token %q is not valid", token)
		}
		start = v
	}
	end := start + n
	if end > len(all) {
		end = len(all)
	}
	var objs []map[string]any
	for _, o := range all[start:end] {
		objs = append(objs, s.objectJSON(j, o))
	}
	out := map[string]any{}
	if len(objs) > 0 {
		out["migrationJobObjects"] = objs
	}
	if end < len(all) {
		out["nextPageToken"] = "o" + strconv.Itoa(end)
	}
	return out, nil
}

func (s *Server) getObject(name string) (any, error) {
	m := objectRE.FindStringSubmatch(name)
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/migrationJobs/{migration_job}/objects/{object}", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.jobLocked(m[1])
	if err != nil {
		return nil, err
	}
	for _, o := range s.objects[m[1]] {
		if o.ID == m[2] {
			return s.objectJSON(j, o), nil
		}
	}
	return nil, apierror.NotFound("Resource '%s' was not found", name)
}

func (s *Server) lookupObject(job string, id objectIdentifier) (any, error) {
	if id.Database == "" {
		return nil, apierror.InvalidArgument("source_object_identifier.database is required")
	}
	if id.Schema != "" || id.Table != "" || id.Type != "" && id.Type != "DATABASE" {
		return nil, unimplemented("objects.lookup of schemas and tables", "a homogeneous migration's objects are its databases (docs/gaps.md)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.jobLocked(job)
	if err != nil {
		return nil, err
	}
	for _, o := range s.objects[job] {
		if o.Database == id.Database {
			return s.objectJSON(j, o), nil
		}
	}
	return nil, apierror.NotFound("migration job %s has no object for the database %s; fetch the source's objects first (fetchSourceObjects)", job, id.Database)
}

// ---- Dump flags and path (#2257) ----

// dumpFlagRE is a mysqldump option name; dumpValueRE a value the sync's
// shell passes as one word.
var (
	dumpFlagRE  = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	dumpValueRE = regexp.MustCompile(`^[A-Za-z0-9._,:/@%+-]*$`)
)

// managedDumpFlags are the options Database Migration Service's dump sets
// itself, which a job's dumpFlags cannot change: what it dumps, where to,
// as whom, and the replication position it records.
var managedDumpFlags = map[string]bool{
	"databases": true, "all-databases": true, "result-file": true, "host": true, "port": true, "user": true, "password": true,
	"socket": true, "protocol": true, "set-gtid-purged": true, "source-data": true, "master-data": true, "dump-replica": true,
	"dump-slave": true, "tab": true, "xml": true, "defaults-file": true, "defaults-extra-file": true, "login-path": true,
	"ssl-mode": true, "ssl-ca": true, "ssl-cert": true, "ssl-key": true, "plugin-dir": true, "default-auth": true,
}

// dumpFlagsOf is a job's dumpFlags as mysqldump options, checked.
func dumpFlagsOf(j *dmspb.MigrationJob) ([]string, error) {
	var out []string
	for _, f := range j.GetDumpFlags().GetDumpFlags() {
		name := strings.TrimPrefix(f.GetName(), "--")
		switch {
		case !dumpFlagRE.MatchString(name):
			return nil, apierror.InvalidArgument("migration_job.dump_flags: %q is not a mysqldump option name", f.GetName())
		case managedDumpFlags[name]:
			return nil, apierror.InvalidArgument("migration_job.dump_flags: --%s is set by Database Migration Service's dump", name)
		case !dumpValueRE.MatchString(f.GetValue()):
			return nil, apierror.InvalidArgument("migration_job.dump_flags: the value of --%s holds characters a mysqldump option value cannot", name)
		}
		if f.GetValue() == "" {
			out = append(out, "--"+name)
		} else {
			out = append(out, "--"+name+"="+f.GetValue())
		}
	}
	return out, nil
}

// gcsURIRE is a dump path: gs://BUCKET/OBJECT.
var gcsURIRE = regexp.MustCompile(`^gs://[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]/.+$`)

// checkDump validates a job's dumpPath, dumpFlags and dumpType: "The path
// to the dump file in Google Cloud Storage, in the format:
// (gs://[BUCKET_NAME]/[OBJECT_NAME]). This field and the "dump_flags"
// field are mutually exclusive."; dumpFlags "The initial dump flags. This
// field and the "dump_path" field are mutually exclusive."; both MySQL's
// (the migrationJobs reference, read 2026-11-25).
func checkDump(j *dmspb.MigrationJob, mysql bool) error {
	path, flags := j.GetDumpPath(), j.GetDumpFlags().GetDumpFlags()
	if path == "" && len(flags) == 0 {
		return nil
	}
	if !mysql {
		return apierror.InvalidArgument("migration_job.dump_path and dump_flags are for a MySQL source")
	}
	if path != "" && len(flags) > 0 {
		return apierror.InvalidArgument("migration_job.dump_path and dump_flags are mutually exclusive")
	}
	if path != "" && !gcsURIRE.MatchString(path) {
		return apierror.InvalidArgument("migration_job.dump_path %q must be gs://[BUCKET_NAME]/[OBJECT_NAME]", path)
	}
	_, err := dumpFlagsOf(j)
	return err
}
