// Package alloydb serves AlloyDB for PostgreSQL (alloydb.googleapis.com
// v1, #2191): clusters, instances (a primary and read pools), backups
// (on-demand, automated and continuous, with point-in-time recovery),
// users, databases, supported database flags, connection information and
// the client certificates the AlloyDB connectors and the Auth Proxy ask
// for, with the server-side proxy those connectors dial on port 5433.
//
// Each instance is a PostgreSQL server of the cluster's major version
// that an Engine runs (in the instance's Kubernetes cluster, as Cloud
// SQL's created instances are): a read pool is a streaming replica of its
// cluster's primary. AlloyDB Omni, Google's own image of the engine, is
// not used: its use is governed by licence terms
// (cloud.google.com/terms/alloydb-omni-marketplace-terms) that CloudBurrow
// cannot accept for the user, so the engine is the PostgreSQL of the same
// major version, and docs/alloydb.md lists what differs.
//
// As Google documents it (docs.cloud.google.com/alloydb/docs, read
// 2026-11-23):
//
//   - a cluster holds one primary instance and any number of read pool
//     instances; a read pool has 1 to 20 nodes (instance-overview);
//   - cluster, instance and backup IDs are RFC 1035 labels: lower-case
//     letters, digits and hyphens, starting with a letter, at most 63
//     characters (the proto and gcloud's reference);
//   - a cluster with instances is deleted only with force, which deletes
//     them too (DeleteClusterRequest.force);
//   - continuous backup and recovery is on by default with a 14-day
//     window, 1 to 35 days (ContinuousBackupConfig);
//   - restoring a backup or a point in time makes a new cluster, without
//     instances, whose primary has the restored data (backup/restore).
//
// What the docs leave unsaid is marked UNVERIFIED where it is decided.
// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/reference/rest.)
package alloydb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
)

// ServerSpec is one instance's PostgreSQL server, as the Engine runs it.
type ServerSpec struct {
	// Server names the server: a DNS label, unique in the CloudBurrow
	// instance (serverName).
	Server string
	// Version is the cluster's database version, such as POSTGRES_16.
	Version string
	// Flags are the instance's database flags, by PostgreSQL's names.
	Flags map[string]string
	// ReplicaOf is the primary's Server for a read pool, "" for a primary.
	ReplicaOf string
	// PITR archives the server's WAL, for continuous backup.
	PITR bool
	// Password is the postgres user's password (the cluster's initial
	// user), "" for none.
	Password string
	// Epoch, for a replica, rebuilds its data from ReplicaOf when it
	// differs from the one it was last built at: a switchover's former
	// primary, and the replicas that follow it.
	Epoch int64
	// Pooler, when not nil, runs the managed connection pooler beside the
	// server, with these PgBouncer settings.
	Pooler map[string]string
}

// Engine runs the instances' PostgreSQL servers. Every method may take
// minutes; none is called with the server's lock held.
type Engine interface {
	// Start runs spec's server, or applies spec to it, and returns once it
	// answers.
	Start(ctx context.Context, spec ServerSpec) error
	// Stop shuts the server down, keeping its data.
	Stop(ctx context.Context, server string) error
	// Restart restarts the server.
	Restart(ctx context.Context, server string) error
	// Remove deletes the server and its data.
	Remove(ctx context.Context, server string) error
	// Addr is the host address the server's PostgreSQL answers on, "" while
	// it does not serve.
	Addr(server string) string
	// Dump writes the server's databases into dir; Load replaces them with
	// those dumped in dir.
	Dump(ctx context.Context, server, dir string) error
	Load(ctx context.Context, server, dir string) error
	// PITRBase takes a base backup for point-in-time recovery: the
	// server can be recovered to any time after it.
	PITRBase(ctx context.Context, server string) error
	// RecoverTo dumps, into dir, the server's databases as they were at at.
	RecoverTo(ctx context.Context, server string, at time.Time, dir string) error
	// Promote ends a replica's recovery: it then takes writes. Start, with
	// its spec's ReplicaOf "", follows to keep it so.
	Promote(ctx context.Context, server string) error
	// PoolerAddr is the host address the server's managed connection
	// pooler answers on (the connectors' pool when connectors), "" while
	// it has none.
	PoolerAddr(server string, connectors bool) string
}

// Options are the server's collaborators.
type Options struct {
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Engine runs the servers; nil refuses instances with FAILED_PRECONDITION.
	Engine Engine
	// BackupsDir keeps the backups' dumps; "" keeps none (backups are
	// refused).
	BackupsDir string
	// ConnectorHost is the address the connectors dial (port 5433), which
	// GetConnectionInfo returns as each instance's IP address.
	ConnectorHost string
	// IAMToken resolves an OAuth 2.0 access token minted by this instance
	// to its principal's email; nil knows none.
	IAMToken func(token string) (email string, ok bool)
	// InstanceAddresses are the addresses instances take for their own,
	// in order (DefaultInstanceAddresses); those the host does not have
	// are skipped. With none, every instance has ConnectorHost.
	InstanceAddresses []string
	// Ports, for tests, move the instance addresses' ports.
	Ports *ListenPorts
	// Transfers and Objects run ExportCluster and ImportCluster; nil
	// refuses them with FAILED_PRECONDITION.
	Transfers Transfers
	Objects   Objects
	// Logf logs background failures; nil discards them.
	Logf func(format string, args ...any)
}

// Server serves google.cloud.alloydb.v1.AlloyDBAdmin.
type Server struct {
	alloydbpb.UnimplementedAlloyDBAdminServer

	opts  Options
	state string

	mu        sync.Mutex
	clusters  map[string]*alloydbpb.Cluster
	instances map[string]*alloydbpb.Instance
	backups   map[string]*alloydbpb.Backup
	users     map[string]*alloydbpb.User
	ops       map[string]*longrunningpb.Operation
	// requests are the operations of the request IDs already seen.
	requests map[string]string
	// restores are the dump directories a restored cluster's primary is
	// loaded from when it is created, by cluster name.
	restores map[string]string
	// passwords are the clusters' initial users' passwords, by cluster.
	passwords map[string]string
	// versions are the API versions (v1alpha, v1beta) an operation was
	// started through, by operation name; v1 is not recorded.
	versions map[string]string
	// lastAuto is each cluster's last automated backup time.
	lastAuto map[string]time.Time
	// epochs are the rebuild epochs of replicas a switchover rebuilt, by
	// instance name (ServerSpec.Epoch).
	epochs map[string]int64
	// bases are the times of each cluster's first continuous backup base
	// on its current primary: the earliest it can be recovered to.
	bases map[string]time.Time

	ca *authority
	// conn is the listeners, once ServeConnector runs.
	conn *Connector
	// work tracks the background operations, for Close.
	work sync.WaitGroup
}

// NewServer loads the records in state ("" keeps them in memory) and the
// connector's certificate authority from caPath ("" makes one in memory).
func NewServer(state, caPath string, opts Options) (*Server, error) {
	s := &Server{opts: opts, state: state}
	s.clear()
	if state != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	ca, err := loadOrCreateAuthority(caPath)
	if err != nil {
		return nil, fmt.Errorf("alloydb: the connectors' certificate authority: %w", err)
	}
	s.ca = ca
	return s, nil
}

func (s *Server) clear() {
	s.clusters = map[string]*alloydbpb.Cluster{}
	s.instances = map[string]*alloydbpb.Instance{}
	s.backups = map[string]*alloydbpb.Backup{}
	s.users = map[string]*alloydbpb.User{}
	s.ops = map[string]*longrunningpb.Operation{}
	s.requests = map[string]string{}
	s.restores = map[string]string{}
	s.passwords = map[string]string{}
	s.versions = map[string]string{}
	s.lastAuto = map[string]time.Time{}
	s.bases = map[string]time.Time{}
	s.epochs = map[string]int64{}
}

// Wait waits for the background operations started so far.
func (s *Server) Wait() { s.work.Wait() }

func (s *Server) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

func (s *Server) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

var (
	projectRE  = regexp.MustCompile(`^projects/([^/]+)$`)
	parentRE   = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	clusterRE  = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/clusters/([^/]+)$`)
	instanceRE = regexp.MustCompile(`^(projects/([^/]+)/locations/([^/]+)/clusters/([^/]+))/instances/([^/]+)$`)
	userRE     = regexp.MustCompile(`^(projects/([^/]+)/locations/([^/]+)/clusters/([^/]+))/users/([^/]+)$`)
	backupRE   = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/backups/([^/]+)$`)
	// idRE is a cluster's, instance's or backup's ID (RFC 1035).
	idRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// Locations are the regions AlloyDB answers ListLocations with: Google's
// regions (internal/locations). AlloyDB's own list is a subset that the
// locations page names; UNVERIFIED which regions it leaves out.
// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/reference/rest.)
func Locations() []string { return locations.Regions }

func validLocation(loc string) bool {
	for _, r := range Locations() {
		if r == loc {
			return true
		}
	}
	return false
}

// checkParent is a parent's project and location; "-" (every location) is
// accepted where any is.
func checkParent(field, parent string, any bool) (string, string, error) {
	m := parentRE.FindStringSubmatch(parent)
	if m == nil {
		return "", "", apierror.InvalidArgument("%s %q must be projects/{project}/locations/{location}", field, parent)
	}
	if m[2] == "-" && any {
		return m[1], m[2], nil
	}
	if !validLocation(m[2]) {
		return "", "", apierror.InvalidArgument("location %q is not an AlloyDB location", m[2])
	}
	return m[1], m[2], nil
}

func checkID(kind, id string) error {
	if !idRE.MatchString(id) {
		return apierror.InvalidArgument("%s ID %q must be 1 to 63 lower-case letters, digits or hyphens, starting with a letter and not ending with a hyphen", kind, id)
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newUID() string {
	b := randomHex(16)
	return b[:8] + "-" + b[8:12] + "-" + b[12:16] + "-" + b[16:20] + "-" + b[20:32]
}

func clone[T proto.Message](m T) T { return proto.Clone(m).(T) }

func etag() string { return randomHex(8) }

// serverName is an instance's server: "alloydb-" and a hash of its full
// name, a DNS label of 20 characters, whatever the IDs' lengths.
func serverName(instance string) string {
	h := sha256.Sum256([]byte(instance))
	return "alloydb-" + hex.EncodeToString(h[:6])
}

// ServerOf is instance's server name (the Engine's), for the console.
func ServerOf(instance string) string { return serverName(instance) }

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

// inLocation is whether name is in parent, where parent's location may be
// "-".
func inLocation(name, parent string) bool {
	if strings.HasSuffix(parent, "/locations/-") {
		project := strings.TrimSuffix(parent, "/locations/-")
		return strings.HasPrefix(name, project+"/locations/")
	}
	return strings.HasPrefix(name, parent+"/")
}

// ---- operations ----

// startOp records a running operation in location for target; the caller
// holds mu.
func (s *Server) startOp(location, verb, target, requestID string) (*longrunningpb.Operation, error) {
	now := timestamppb.New(s.now())
	md, err := anypb.New(&alloydbpb.OperationMetadata{CreateTime: now, Target: target, Verb: verb, ApiVersion: "v1"})
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	op := &longrunningpb.Operation{Name: location + "/operations/operation-" + randomHex(12), Metadata: md}
	s.ops[op.Name] = op
	if requestID != "" {
		s.requests[requestID] = op.Name
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(op), nil
}

// finishOp ends operation name with resp, or with err; the caller holds
// mu.
func (s *Server) finishOp(name string, resp proto.Message, err error) {
	op, ok := s.ops[name]
	if !ok {
		return
	}
	var md alloydbpb.OperationMetadata
	if op.GetMetadata().UnmarshalTo(&md) == nil {
		md.EndTime = timestamppb.New(s.now())
		if a, e := anypb.New(&md); e == nil {
			op.Metadata = a
		}
	}
	op.Done = true
	if err != nil {
		op.Result = &longrunningpb.Operation_Error{Error: apierror.From(err).GRPCStatus().Proto()}
	} else {
		if resp == nil {
			resp = &alloydbpb.OperationMetadata{}
		}
		a, e := anypb.New(resp)
		if e != nil {
			op.Result = &longrunningpb.Operation_Error{Error: apierror.From(e).GRPCStatus().Proto()}
		} else {
			op.Result = &longrunningpb.Operation_Response{Response: a}
		}
	}
	if e := s.save(); e != nil {
		s.logf("alloydb: saving the state: %v", e)
	}
}

// replay is the operation an earlier request with requestID returned, so a
// retried request is not applied twice (AIP-155); the caller holds mu.
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

// background runs work for operation op outside the lock; finish is
// called with the lock held, with work's error, and returns the
// operation's response.
func (s *Server) background(op string, work func(ctx context.Context) error, finish func(err error) (proto.Message, error)) {
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		err := work(ctx)
		s.mu.Lock()
		defer s.mu.Unlock()
		resp, ferr := finish(err)
		if err == nil {
			err = ferr
		}
		if err != nil {
			s.logf("alloydb: operation %s: %v", op, err)
		}
		s.finishOp(op, resp, err)
	}()
}

// doneOp records an operation that is done at once, with resp; the caller
// holds mu.
func (s *Server) doneOp(location, verb, target, requestID string, resp proto.Message) (*longrunningpb.Operation, error) {
	op, err := s.startOp(location, verb, target, requestID)
	if err != nil {
		return nil, err
	}
	s.finishOp(op.Name, resp, nil)
	return clone(s.ops[op.Name]), nil
}

// validateOnly is the answer to a request with validate_only set: an
// operation that is done, with no response, which is not recorded.
// UNVERIFIED: what Google's operation for a validated request holds.
// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/reference/rest.)
func (s *Server) validateOnly(location, verb, target string) *longrunningpb.Operation {
	now := timestamppb.New(s.now())
	md, _ := anypb.New(&alloydbpb.OperationMetadata{CreateTime: now, EndTime: now, Target: target, Verb: verb, ApiVersion: "v1"})
	return &longrunningpb.Operation{Name: location + "/operations/operation-" + randomHex(12), Done: true, Metadata: md}
}

// ---- persistence ----

type diskState struct {
	Clusters  []json.RawMessage    `json:"clusters,omitempty"`
	Instances []json.RawMessage    `json:"instances,omitempty"`
	Backups   []json.RawMessage    `json:"backups,omitempty"`
	Users     []json.RawMessage    `json:"users,omitempty"`
	Ops       []json.RawMessage    `json:"ops,omitempty"`
	Requests  map[string]string    `json:"requests,omitempty"`
	Restores  map[string]string    `json:"restores,omitempty"`
	Passwords map[string]string    `json:"passwords,omitempty"`
	Versions  map[string]string    `json:"versions,omitempty"`
	LastAuto  map[string]time.Time `json:"lastAuto,omitempty"`
	Bases     map[string]time.Time `json:"bases,omitempty"`
	Epochs    map[string]int64     `json:"epochs,omitempty"`
}

func marshalAll[T proto.Message](m map[string]T) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for _, k := range sortedKeys(m, "") {
		b, err := protojson.Marshal(m[k])
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
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, m); err != nil {
			return err
		}
		into[name(m)] = m
	}
	return nil
}

// save writes the records; the caller holds mu.
func (s *Server) save() error {
	if s.state == "" {
		return nil
	}
	var st diskState
	var err error
	if st.Clusters, err = marshalAll(s.clusters); err != nil {
		return err
	}
	if st.Instances, err = marshalAll(s.instances); err != nil {
		return err
	}
	if st.Backups, err = marshalAll(s.backups); err != nil {
		return err
	}
	if st.Users, err = marshalAll(s.users); err != nil {
		return err
	}
	if st.Ops, err = marshalAll(s.ops); err != nil {
		return err
	}
	st.Requests, st.Restores, st.Passwords, st.Versions, st.LastAuto, st.Bases = s.requests, s.restores, s.passwords, s.versions, s.lastAuto, s.bases
	st.Epochs = s.epochs
	b, err := json.MarshalIndent(st, "", "  ")
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

func (s *Server) load() error {
	b, err := os.ReadFile(s.state)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st diskState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("alloydb state %s: %w", s.state, err)
	}
	errs := []error{
		unmarshalAll(st.Clusters, func() *alloydbpb.Cluster { return &alloydbpb.Cluster{} }, (*alloydbpb.Cluster).GetName, s.clusters),
		unmarshalAll(st.Instances, func() *alloydbpb.Instance { return &alloydbpb.Instance{} }, (*alloydbpb.Instance).GetName, s.instances),
		unmarshalAll(st.Backups, func() *alloydbpb.Backup { return &alloydbpb.Backup{} }, (*alloydbpb.Backup).GetName, s.backups),
		unmarshalAll(st.Users, func() *alloydbpb.User { return &alloydbpb.User{} }, (*alloydbpb.User).GetName, s.users),
		unmarshalAll(st.Ops, func() *longrunningpb.Operation { return &longrunningpb.Operation{} }, (*longrunningpb.Operation).GetName, s.ops),
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("alloydb state %s: %w", s.state, err)
	}
	for k, v := range st.Requests {
		s.requests[k] = v
	}
	for k, v := range st.Restores {
		s.restores[k] = v
	}
	for k, v := range st.Passwords {
		s.passwords[k] = v
	}
	for k, v := range st.Versions {
		s.versions[k] = v
	}
	for k, v := range st.LastAuto {
		s.lastAuto[k] = v
	}
	for k, v := range st.Bases {
		s.bases[k] = v
	}
	for k, v := range st.Epochs {
		s.epochs[k] = v
	}
	// An operation a restart interrupted ends in error, and its resource
	// is left as it was recorded. UNVERIFIED: Google's operations survive
	// its restarts.
	// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/reference/rest.)
	for name, op := range s.ops {
		if !op.GetDone() {
			s.finishOp(name, nil, apierror.Aborted("the operation was interrupted by a restart of CloudBurrow"))
		}
	}
	return nil
}

// Servers are the servers of every recorded instance, which an `up`
// keeps; the Engine prunes the others.
func (s *Server) Servers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for name := range s.instances {
		out = append(out, serverName(name))
	}
	sort.Strings(out)
	return out
}

// Resume starts the servers of the recorded instances that should run, as
// an `up` of a persistent CloudBurrow instance does.
func (s *Server) Resume(ctx context.Context) {
	s.mu.Lock()
	type job struct {
		name, cluster string
		spec          ServerSpec
	}
	var primaries, secondaries, pools []job
	for _, name := range sortedKeys(s.instances, "") {
		in := s.instances[name]
		if in.GetActivationPolicy() == alloydbpb.Instance_NEVER {
			continue
		}
		spec, err := s.specOf(in)
		if err != nil {
			continue
		}
		cluster := instanceRE.FindStringSubmatch(name)[1]
		switch in.GetInstanceType() {
		case alloydbpb.Instance_READ_POOL:
			pools = append(pools, job{name, cluster, spec})
		case alloydbpb.Instance_SECONDARY:
			secondaries = append(secondaries, job{name, cluster, spec})
		default:
			primaries = append(primaries, job{name, cluster, spec})
		}
	}
	s.mu.Unlock()
	if s.opts.Engine == nil {
		return
	}
	for _, j := range append(append(primaries, secondaries...), pools...) {
		if err := s.opts.Engine.Start(ctx, j.spec); err != nil {
			s.logf("alloydb: resuming instance %s: %v", j.name, err)
			continue
		}
		if !j.spec.PITR || j.spec.ReplicaOf != "" {
			continue
		}
		if err := s.takeBase(ctx, j.cluster, j.spec.Server, false); err != nil {
			s.logf("alloydb: the continuous backup base of %s: %v", j.name, err)
		}
	}
}

// ResetProject deletes project's records and servers; "" deletes
// everything.
func (s *Server) ResetProject(ctx context.Context, project string) error {
	s.mu.Lock()
	prefix := "projects/" + project + "/"
	if project == "" {
		prefix = ""
	}
	var servers []string
	for k := range s.instances {
		if strings.HasPrefix(k, prefix) {
			servers = append(servers, serverName(k))
			s.releaseAddress(s.instances[k])
			delete(s.instances, k)
		}
	}
	for _, m := range []map[string]*alloydbpb.Cluster{s.clusters} {
		for k := range m {
			if strings.HasPrefix(k, prefix) {
				delete(m, k)
				delete(s.restores, k)
				delete(s.passwords, k)
				delete(s.lastAuto, k)
				delete(s.bases, k)
			}
		}
	}
	var dirs []string
	for k := range s.backups {
		if strings.HasPrefix(k, prefix) {
			dirs = append(dirs, s.backupDir(s.backups[k]))
			delete(s.backups, k)
		}
	}
	for k := range s.users {
		if strings.HasPrefix(k, prefix) {
			delete(s.users, k)
		}
	}
	for k := range s.ops {
		if strings.HasPrefix(k, prefix) {
			delete(s.ops, k)
			delete(s.versions, k)
		}
	}
	err := s.save()
	s.mu.Unlock()
	for _, d := range dirs {
		if d != "" {
			_ = os.RemoveAll(d)
		}
	}
	if s.opts.Engine != nil {
		for _, srv := range servers {
			if e := s.opts.Engine.Remove(ctx, srv); e != nil {
				err = errors.Join(err, e)
			}
		}
	}
	return err
}

// Operation returns a copy of the operation called name, for the console,
// which waits on what it started.
func (s *Server) Operation(name string) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[name]
	if !ok {
		return nil, apierror.NotFound("operation %q was not found", name)
	}
	return clone(op), nil
}
