// Package datamigration serves the Database Migration Service API
// (datamigration.googleapis.com v1, #2252): connection profiles, migration
// jobs that copy a MySQL or PostgreSQL source into a Cloud SQL or AlloyDB
// destination of this instance and go on replicating it, private
// connections, and the operations, locations and IAM mixins.
//
// As Google documents it (docs.cloud.google.com/database-migration/docs,
// read 2026-11-25: reference/rest, reference/rest/v1/projects.locations.
// connectionProfiles and .migrationJobs with their methods' pages,
// postgresql-to-alloydb/configure-source-database, mysql/configure-source-
// database, postgres/migration-job-statuses, postgres/create-migration-job,
// about-quick-start-migrations):
//
//   - a connection profile describes a source (mysql, postgresql: host,
//     port, username, an input-only password answered as passwordSet) or a
//     destination; a cloudsql profile's settings create its Cloud SQL
//     instance, a replica of the source its sourceId names, and an alloydb
//     profile's settings create its cluster and primary instance (settings
//     are immutable);
//   - a migration job (ONE_TIME or CONTINUOUS) moves the source profile's
//     databases into the destination profile's; it is NOT_STARTED once
//     created, STARTING, then RUNNING in the FULL_DUMP phase and, for a
//     continuous one, the CDC phase; stop (STOPPING, STOPPED) and resume
//     (RESUMING) pause the CDC phase; promote (PROMOTE_IN_PROGRESS) stops
//     replication and makes the destination standalone, COMPLETED; a
//     one-time job is COMPLETED after its full dump; restart starts a
//     stopped or failed job again from scratch; verify checks that "the
//     destination can reach the source" and that its settings are met,
//     with MigrationJobVerificationError codes;
//   - a PostgreSQL source has wal_level logical and enough replication
//     slots, WAL senders and worker processes for one per database; a
//     standard migration also needs pglogical (in shared_preload_libraries
//     and as an extension of every database), a quick-start (native
//     logical, postgresHomogeneousConfig.isNativeLogical) none; every
//     database but template0 and template1 is migrated;
//   - deleting a migration job deletes its Cloud SQL destination's profile,
//     and with force the replica instance too.
//
// Here the destination is this instance's own Cloud SQL or AlloyDB server
// and the data really moves: the Backend runs every check and every copy
// from the destination's server, which reaches the source as the profile
// names it (a host on the instance's network, or one it can route to).
// Cloud SQL destinations replicate through the Cloud SQL Admin API's
// external-primary replicas (a source representation instance and a
// replica of it: GTID replication for MySQL, PostgreSQL's built-in logical
// replication for PostgreSQL); AlloyDB destinations subscribe the primary
// to a publication of each source database. Neither runs pglogical, which
// CloudBurrow's images do not have: a standard PostgreSQL migration still
// checks for it, as Google's does. What is not served answers
// UNIMPLEMENTED and is listed in docs/gaps.md.
//
// What the docs leave unsaid is marked UNVERIFIED where it is decided.
// Docs re-read 2026-11-25, still unanswered: https://docs.cloud.google.com/database-migration/docs/reference/rest
package datamigration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/locations"
)

// Source is a source database as a destination's server reaches it.
type Source struct {
	// Engine is MYSQL or POSTGRESQL.
	Engine dmspb.DatabaseEngine
	Host   string
	Port   int
	User   string
	// Password is the profile's input-only password.
	Password string
	// RequireSSL is a profile whose SSL type is REQUIRED: the connection
	// is encrypted, its certificate not verified.
	RequireSSL bool
}

// Target is a destination database server of this instance.
type Target struct {
	// CloudSQL is a Cloud SQL instance; otherwise an AlloyDB cluster.
	CloudSQL bool
	// Project, Location and ID name it: the Cloud SQL instance ID or the
	// AlloyDB cluster ID.
	Project, Location, ID string
	// Version is its database version: MYSQL_8_0, POSTGRES_16, ...
	Version string
}

// MySQL reports whether the target is a MySQL server.
func (t Target) MySQL() bool { return strings.HasPrefix(t.Version, "MYSQL_") }

// CloudSQLAddresses are a created Cloud SQL destination's addresses.
type CloudSQLAddresses struct {
	PublicIP, PrivateIP string
}

// ConnError is a failure to reach or sign in to a source, as the Backend
// reports it: Auth is a sign-in the source refused.
type ConnError struct {
	Auth bool
	Msg  string
}

func (e *ConnError) Error() string { return e.Msg }

// Backend runs the destinations and moves the data. Every method that
// touches the source does so from the target's server.
type Backend interface {
	// CreateCloudSQL creates the Cloud SQL instance of a destination
	// profile: a replica of src, through a source representation
	// instance, that waits for Migrate. It returns once the instance runs.
	CreateCloudSQL(ctx context.Context, t Target, s *dmspb.CloudSqlSettings, src Source) (CloudSQLAddresses, error)
	// CreateAlloyDB creates an AlloyDB destination's cluster and its
	// primary instance, and returns once the primary runs, answering the
	// cluster's database version (POSTGRES_16, ...).
	CreateAlloyDB(ctx context.Context, t Target, s *dmspb.AlloyDbSettings) (string, error)
	// Delete deletes a Cloud SQL destination's instance and its source
	// representation instance.
	Delete(ctx context.Context, t Target) error
	// QuerySource runs a read-only query against src's database (""
	// is the engine's default) from t's server, and answers its rows.
	QuerySource(ctx context.Context, t Target, src Source, database, query string) ([][]string, error)
	// QueryTarget runs a statement in t's database as its administrator
	// and answers its rows.
	QueryTarget(ctx context.Context, t Target, database, query string) ([][]string, error)
	// Migrate copies src's databases (a PostgreSQL source's, or every one
	// of a MySQL source's) into t and, when continuous, goes on
	// replicating them. A MySQL target returns once its dump is loaded
	// (and, continuous, replication from the dump's GTID position runs);
	// a PostgreSQL target once its subscriptions are created, their
	// initial copy then followed in pg_subscription_rel.
	Migrate(ctx context.Context, t Target, src Source, databases []string, continuous bool) error
	// Pause stops (or, with pause false, restarts) t's replication.
	Pause(ctx context.Context, t Target, pause bool) error
	// Promote ends t's replication: it is then a standalone database.
	Promote(ctx context.Context, t Target) error
	// Reset undoes an AlloyDB target's migration (its subscriptions and the
	// migrated databases), so Migrate can run again.
	Reset(ctx context.Context, t Target, databases []string) error
}

// Options are the server's collaborators.
type Options struct {
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Backend runs the destinations; nil refuses every destination with
	// FAILED_PRECONDITION.
	Backend Backend
	// CloudSQL and AlloyDB are whether the instance serves those
	// destinations.
	CloudSQL, AlloyDB bool
	// CloudSQLVersions are the Cloud SQL database versions the instance
	// serves (POSTGRES_16, MYSQL_8_4, ...); nil accepts any.
	CloudSQLVersions func() []string
	// Network reports whether a VPC network (projects/P/global/networks/N)
	// exists; nil checks none.
	Network func(name string) bool
	// StaticIPs are the addresses the destinations' servers connect to a
	// source from; nil answers fetchStaticIps UNIMPLEMENTED.
	StaticIPs func() []string
	// Poll is how often a running job's copy is checked; 0 is 2 seconds.
	Poll time.Duration
}

// Server serves google.cloud.clouddms.v1.DataMigrationService.
type Server struct {
	dmspb.UnimplementedDataMigrationServiceServer

	opts  Options
	state string

	mu       sync.Mutex
	profiles map[string]*dmspb.ConnectionProfile
	jobs     map[string]*dmspb.MigrationJob
	privates map[string]*dmspb.PrivateConnection
	ops      map[string]*longrunningpb.Operation
	policies map[string]*iampolicy.Stored
	requests map[string]string
	// started is when each job last entered RUNNING, for its duration.
	started map[string]time.Time
	// cancel stops each job's runner.
	cancel map[string]context.CancelFunc
	work   sync.WaitGroup
}

// NewServer loads the records in state ("" keeps them in memory).
func NewServer(state string, opts Options) (*Server, error) {
	s := &Server{opts: opts, state: state}
	s.clear()
	if state != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Server) clear() {
	s.profiles = map[string]*dmspb.ConnectionProfile{}
	s.jobs = map[string]*dmspb.MigrationJob{}
	s.privates = map[string]*dmspb.PrivateConnection{}
	s.ops = map[string]*longrunningpb.Operation{}
	s.policies = map[string]*iampolicy.Stored{}
	s.requests = map[string]string{}
	s.started = map[string]time.Time{}
	if s.cancel == nil {
		s.cancel = map[string]context.CancelFunc{}
	}
}

// Wait waits for the server's background work.
func (s *Server) Wait() { s.work.Wait() }

// Close stops every job's runner and waits for them.
func (s *Server) Close() {
	s.mu.Lock()
	for _, c := range s.cancel {
		c()
	}
	s.mu.Unlock()
	s.work.Wait()
}

func (s *Server) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

func (s *Server) poll() time.Duration {
	if s.opts.Poll > 0 {
		return s.opts.Poll
	}
	return 2 * time.Second
}

var (
	projectRE = regexp.MustCompile(`^projects/([^/]+)$`)
	parentRE  = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	profileRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/connectionProfiles/([^/]+)$`)
	jobRE     = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/migrationJobs/([^/]+)$`)
	privateRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/privateConnections/([^/]+)$`)
	opRE      = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/operations/([^/]+)$`)
	// idRE is a resource ID: lower-case letters, digits and hyphens,
	// starting with a letter, at most 63 (UNVERIFIED: the reference gives
	// no rule; the console asks for this, and a Cloud SQL destination's
	// instance takes its profile's ID).
	// Docs re-read 2026-11-25, still unanswered: https://docs.cloud.google.com/database-migration/docs/reference/rest/v1/projects.locations.migrationJobs/create
	idRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// requestRE is a request ID: "letters (a-z, A-Z), numbers (0-9),
	// underscores (_), and hyphens (-)", at most 40 (the create pages).
	requestRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
)

// Locations are the regions Database Migration Service runs in.
func Locations() []string { return locations.Regions }

func validLocation(loc string) bool {
	for _, r := range locations.Regions {
		if r == loc {
			return true
		}
	}
	return false
}

// checkParent is a parent's project and location.
func checkParent(field, parent string) (string, string, error) {
	m := parentRE.FindStringSubmatch(parent)
	if m == nil {
		return "", "", apierror.InvalidArgument("%s %q must be projects/{project}/locations/{location}", field, parent)
	}
	if !validLocation(m[2]) {
		return "", "", apierror.InvalidArgument("location %q is not a Database Migration Service location", m[2])
	}
	return m[1], m[2], nil
}

func checkID(kind, id string) error {
	if id == "" {
		return apierror.InvalidArgument("%s_id is required", kind)
	}
	if !idRE.MatchString(id) {
		return apierror.InvalidArgument("%s_id %q must be 1 to 63 lower-case letters, digits or hyphens, starting with a letter and not ending with a hyphen", kind, id)
	}
	return nil
}

func checkRequestID(id string) error {
	if id != "" && !requestRE.MatchString(id) {
		return apierror.InvalidArgument("request_id %q may contain only letters, numbers, underscores and hyphens, at most 40 characters", id)
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func clone[T proto.Message](m T) T { return proto.Clone(m).(T) }

func ts(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

// startOp records a pending operation for target in location; the caller
// holds mu.
func (s *Server) startOp(location, verb, target, requestID string) (*longrunningpb.Operation, error) {
	md, err := anypb.New(&dmspb.OperationMetadata{CreateTime: ts(s.now()), Target: target, Verb: verb, ApiVersion: "v1"})
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	op := &longrunningpb.Operation{Name: location + "/operations/operation-" + randomHex(16), Metadata: md}
	s.ops[op.Name] = op
	if requestID != "" {
		s.requests[requestID] = op.Name
	}
	return op, nil
}

// finishOp ends op with resp or with err; the caller holds mu.
func (s *Server) finishOp(name string, resp proto.Message, err error) {
	op := s.ops[name]
	if op == nil {
		return
	}
	var md dmspb.OperationMetadata
	if op.GetMetadata() != nil && op.GetMetadata().UnmarshalTo(&md) == nil {
		md.EndTime = ts(s.now())
		if a, e := anypb.New(&md); e == nil {
			op.Metadata = a
		}
	}
	op.Done = true
	if err != nil {
		op.Result = &longrunningpb.Operation_Error{Error: statusOf(err)}
		return
	}
	if resp != nil {
		if a, e := anypb.New(resp); e == nil {
			op.Result = &longrunningpb.Operation_Response{Response: a}
		}
	}
}

// done records a finished operation; the caller holds mu.
func (s *Server) done(location, verb, target, requestID string, resp proto.Message) (*longrunningpb.Operation, error) {
	op, err := s.startOp(location, verb, target, requestID)
	if err != nil {
		return nil, err
	}
	s.finishOp(op.Name, resp, nil)
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(s.ops[op.Name]), nil
}

// statusOf is err as a google.rpc.Status, its details kept.
func statusOf(err error) *spb.Status {
	e := apierror.From(err)
	return &spb.Status{Code: int32(e.Code), Message: e.Message, Details: e.Details}
}

// replay is the operation an earlier request with requestID returned, so
// a retried request is not applied twice; the caller holds mu.
func (s *Server) replay(requestID string) (*longrunningpb.Operation, bool) {
	if requestID == "" {
		return nil, false
	}
	if name, ok := s.requests[requestID]; ok {
		if op, ok := s.ops[name]; ok {
			return clone(op), true
		}
	}
	return nil, false
}

// background runs f beside the request, holding the server's wait group.
func (s *Server) background(f func()) {
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		f()
	}()
}

func sortedKeys[V any](m map[string]V, prefix string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func listPage[T proto.Message](scope, filter, orderBy string, items []T, token string, size int32) ([]T, string, error) {
	out, next, err := aipfilter.ListPage(scope+"|"+filter+"|"+orderBy, filter, orderBy, items, token, int(size))
	if err != nil {
		return nil, "", apierror.InvalidArgument("%v", err)
	}
	return out, next, nil
}

func unimplemented(what, why string) error {
	return apierror.Unimplemented("%s is not implemented in CloudBurrow: %s", what, why)
}

func failedPrecondition(format string, a ...any) error {
	return &apierror.Error{Code: codes.FailedPrecondition, Message: fmt.Sprintf(format, a...), Reason: "conditionNotMet"}
}

// ResetProject deletes project's records; "" deletes everything. The
// destinations' instances and clusters are their services' to reset.
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	prefix := "projects/" + project + "/"
	for k, c := range s.cancel {
		if project == "" || strings.HasPrefix(k, prefix) {
			c()
			delete(s.cancel, k)
		}
	}
	if project == "" {
		s.clear()
		err := s.save()
		s.mu.Unlock()
		return err
	}
	for _, m := range []map[string]*dmspb.ConnectionProfile{s.profiles} {
		for k := range m {
			if strings.HasPrefix(k, prefix) {
				delete(m, k)
				delete(s.policies, k)
			}
		}
	}
	for k := range s.jobs {
		if strings.HasPrefix(k, prefix) {
			delete(s.jobs, k)
			delete(s.policies, k)
			delete(s.started, k)
		}
	}
	for k := range s.privates {
		if strings.HasPrefix(k, prefix) {
			delete(s.privates, k)
			delete(s.policies, k)
		}
	}
	for k := range s.ops {
		if strings.HasPrefix(k, prefix) {
			delete(s.ops, k)
		}
	}
	err := s.save()
	s.mu.Unlock()
	return err
}

// ---- state ----

type diskState struct {
	Profiles []json.RawMessage           `json:"connectionProfiles,omitempty"`
	Jobs     []json.RawMessage           `json:"migrationJobs,omitempty"`
	Privates []json.RawMessage           `json:"privateConnections,omitempty"`
	Ops      []json.RawMessage           `json:"operations,omitempty"`
	Policies map[string]iampolicy.Stored `json:"policies,omitempty"`
	Requests map[string]string           `json:"requests,omitempty"`
	Started  map[string]time.Time        `json:"started,omitempty"`
}

func marshalAll[T proto.Message](m map[string]T) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for _, k := range sortedKeys(m, "") {
		b, err := overlay.Marshal(m[k])
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func unmarshalAll[T proto.Message](raw []json.RawMessage, mk func() T, name func(T) string, into map[string]T) error {
	for _, b := range raw {
		m := mk()
		if err := overlay.Unmarshal(b, m); err != nil {
			return err
		}
		into[name(m)] = m
	}
	return nil
}

// save writes the records; the caller holds mu. Passwords are kept, as
// Google keeps a profile's to connect with, in a file only the user reads.
func (s *Server) save() error {
	if s.state == "" {
		return nil
	}
	var st diskState
	var err error
	if st.Profiles, err = marshalAll(s.profiles); err != nil {
		return err
	}
	if st.Jobs, err = marshalAll(s.jobs); err != nil {
		return err
	}
	if st.Privates, err = marshalAll(s.privates); err != nil {
		return err
	}
	if st.Ops, err = marshalAll(s.ops); err != nil {
		return err
	}
	st.Policies = map[string]iampolicy.Stored{}
	for k, p := range s.policies {
		if p != nil {
			st.Policies[k] = *p
		}
	}
	st.Requests, st.Started = s.requests, s.started
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.state), 0o755); err != nil {
		return err
	}
	tmp := s.state + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.state)
}

func (s *Server) load() error {
	b, err := os.ReadFile(s.state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var st diskState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("datamigration: %s: %w", s.state, err)
	}
	if err := unmarshalAll(st.Profiles, func() *dmspb.ConnectionProfile { return &dmspb.ConnectionProfile{} },
		(*dmspb.ConnectionProfile).GetName, s.profiles); err != nil {
		return err
	}
	if err := unmarshalAll(st.Jobs, func() *dmspb.MigrationJob { return &dmspb.MigrationJob{} },
		(*dmspb.MigrationJob).GetName, s.jobs); err != nil {
		return err
	}
	if err := unmarshalAll(st.Privates, func() *dmspb.PrivateConnection { return &dmspb.PrivateConnection{} },
		(*dmspb.PrivateConnection).GetName, s.privates); err != nil {
		return err
	}
	if err := unmarshalAll(st.Ops, func() *longrunningpb.Operation { return &longrunningpb.Operation{} },
		(*longrunningpb.Operation).GetName, s.ops); err != nil {
		return err
	}
	for k, p := range st.Policies {
		p := p
		s.policies[k] = &p
	}
	if st.Requests != nil {
		s.requests = st.Requests
	}
	if st.Started != nil {
		s.started = st.Started
	}
	return nil
}
