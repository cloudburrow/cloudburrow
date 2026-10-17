package redisadmin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/lro"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Spec is what a created instance's server is started with.
type Spec struct {
	// Server names the server: the Deployment and Service, unique per
	// instance resource name (ServerName).
	Server string
	// MaxMemory is its maxmemory, in bytes.
	MaxMemory int64
	// Password is its AUTH string; "" runs it without AUTH.
	Password string
	// Configs are further server settings, by Valkey's own names
	// (maxmemory-policy, notify-keyspace-events, ...).
	Configs map[string]string
	// Version is the redis_version the server runs (#1386): its image.
	Version string
	// RDBSeconds, above 0, is RDB persistence (#1384): a snapshot once
	// that many seconds have passed with a change, in place of the
	// CloudBurrow instance's mode's own.
	RDBSeconds int
	// ReplicaOf, host:port, starts the server as a replica of it: a
	// Standard tier node (#1412), which a failover or the automatic
	// failover makes a primary.
	ReplicaOf string
	// TransitCA and TransitCAKey, PEM, are the instance's CA with
	// in-transit encryption (#1413): the server serves TLS only, on
	// TLSPort, with a certificate of it (ServerTLS). "" for none.
	TransitCA, TransitCAKey string
	// TransitTrust, PEM, are every CA server_ca_certs lists: TransitCA
	// and the ones a rotation replaced, which still are valid (#1463).
	TransitTrust string
	// Dir is a Redis Cluster or Valkey node's data directory on its
	// volume in persistent mode (#1557): one per formation, so a node
	// formed again never loads what a previous formation left. "" is the
	// mode's own.
	Dir string
}

// Provisioner runs the server of an instance CreateInstance made.
type Provisioner interface {
	// Provision runs spec's server, or runs it again with spec after a
	// restart of CloudBurrow, and returns once it serves, with the
	// address the host reaches it at.
	Provision(ctx context.Context, spec Spec) (addr string, err error)
	// Remove stops the server and deletes its data.
	Remove(ctx context.Context, server string) error
}

// DataPlane is a created instance's running server.
type DataPlane interface {
	Backend
	// ConfigSet changes settings of the running server (CONFIG SET).
	ConfigSet(ctx context.Context, settings map[string]string) error
	// Save snapshots the data (BGSAVE) and returns the RDB file's path.
	Save(ctx context.Context) (string, error)
	// Load replaces the data with the one of the server at host:port.
	Load(ctx context.Context, host string, port int) error
}

// Dialer reaches the server at addr with an AUTH string ("" for none),
// over TLS trusting the CA caPEM, or plain TCP for "" (#1413).
type Dialer func(addr, password, caPEM string) DataPlane

// ServerName is the name of an instance's server: "redis-" and its ID,
// with a hash of the whole resource name, so an instance of the same ID in
// another project or region has its own, and within Kubernetes' 63.
func ServerName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "redis-" + name[strings.LastIndex(name, "/")+1:] + "-" + hex.EncodeToString(sum[:4])
}

// ServerPrefix starts every created instance's server name.
const ServerPrefix = "redis-"

// record is a created instance, as it is kept.
type record struct {
	Name         string            `json:"name"`
	DisplayName  string            `json:"displayName,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	LocationID   string            `json:"locationId"`
	MemorySizeGB int32             `json:"memorySizeGb"`
	RedisConfigs map[string]string `json:"redisConfigs,omitempty"`
	AuthString   string            `json:"authString,omitempty"`
	Created      time.Time         `json:"created"`
	Server       string            `json:"server"`

	// Version is redis_version; "" is DefaultRedisVersion (#1386).
	Version string `json:"version,omitempty"`
	// Maintenance is maintenance_policy, as protojson; "" for none (#1386).
	Maintenance string `json:"maintenance,omitempty"`
	// Persistence is persistence_config, as last set; nil when never set.
	Persistence *persistence `json:"persistence,omitempty"`

	// Standard tier (#1385): Tier is "STANDARD_HA", "" for BASIC; Nodes
	// are the node numbers, Primary the primary's.
	Tier                  string `json:"tier,omitempty"`
	AlternativeLocationID string `json:"alternativeLocationId,omitempty"`
	ReplicaCount          int32  `json:"replicaCount,omitempty"`
	ReadReplicas          bool   `json:"readReplicas,omitempty"`
	Nodes                 []int  `json:"nodes,omitempty"`
	Primary               int    `json:"primary,omitempty"`

	// TransitCA and TransitCAKey are the instance's CA, PEM, with
	// transit_encryption_mode SERVER_AUTHENTICATION (#1413).
	TransitCA    string `json:"transitCa,omitempty"`
	TransitCAKey string `json:"transitCaKey,omitempty"`
	// PreviousCAs are the CAs a server CA rotation replaced, PEM, still
	// listed in server_ca_certs until they expire (#1463).
	PreviousCAs string `json:"previousCas,omitempty"`

	// CMEK (#1463): CustomerManagedKey is customer_managed_key, WrappedKey
	// the data key it wrapped, and Sealed the secrets above sealed with
	// the data key, which then are not kept in the clear.
	CustomerManagedKey string `json:"customerManagedKey,omitempty"`
	WrappedKey         []byte `json:"wrappedKey,omitempty"`
	Sealed             []byte `json:"sealed,omitempty"`
	dek                []byte
	suspended          bool

	// Not kept: what the running server is now.
	state   redispb.Instance_State
	message string
	// addr is the server's address: the primary's, of a Standard tier
	// instance, whose nodes are at nodeAddrs and whose clients connect
	// to primaryEP and readEP.
	addr      string
	nodeAddrs map[int]string
	primaryEP *endpoint
	readEP    *endpoint
}

func (r *record) spec() Spec {
	return Spec{Server: r.Server, MaxMemory: maxMemory(r.MemorySizeGB, r.RedisConfigs),
		Password: r.AuthString, Configs: serverConfigs(r.RedisConfigs), RDBSeconds: r.Persistence.seconds(), Version: r.version(),
		TransitCA: r.TransitCA, TransitCAKey: r.TransitCAKey, TransitTrust: r.caPEM()}
}

// binary is the prefix of the server's programs: valkey-server, ...
func (r *record) binary() string {
	if r.version() == DefaultRedisVersion {
		return "valkey"
	}
	return "redis"
}

// instances are the instances CreateInstance made.
type instances struct {
	prov Provisioner
	dial Dialer
	db   store.Store
	ops  *lro.Store
	now  func() time.Time
	// bg is what provisioning runs under: the server's life, not a call's.
	bg context.Context

	mu   sync.Mutex
	byID map[string]*record // by resource name
	busy map[string]string  // resource name -> pending operation
	// cancels cancel the imports and exports in progress, by operation.
	cancels map[string]context.CancelFunc
	// objs is this instance's Cloud Storage, for import and export; nil
	// when it is not enabled.
	objs Objects
	// kms is Cloud KMS, for customer_managed_key; nil when it is not
	// enabled (#1463).
	kms KeyService
	// aof is whether a server keeps an append-only file when it has no
	// RDB persistence: the CloudBurrow instance's persistent mode.
	aof bool
}

// PersistentMode says the CloudBurrow instance is persistent, so a server
// without RDB persistence keeps an append-only file (#1384).
func (s *Server) PersistentMode(on bool) {
	if s.created != nil {
		s.created.aof = on
	}
}

// EnableCreate makes CreateInstance, UpdateInstance and DeleteInstance
// work, over p, keeping the instances in db (a durable store keeps them
// across a restart of CloudBurrow, whose servers Restore runs again).
// ctx bounds provisioning; operations is where their operations are.
func (s *Server) EnableCreate(ctx context.Context, p Provisioner, dial Dialer, db store.Store, operations *lro.Store) {
	if operations == nil {
		operations = lro.NewStore(nil)
	}
	s.created = &instances{prov: p, dial: dial, db: db, ops: operations, now: time.Now, bg: ctx,
		byID: map[string]*record{}, busy: map[string]string{}, cancels: map[string]context.CancelFunc{}}
	go s.created.watch(ctx, healthEvery)
}

// Operations is the operations store, nil before EnableCreate.
func (s *Server) Operations() *lro.Store {
	if s.created == nil {
		return nil
	}
	return s.created.ops
}

// Servers are the server names of every instance kept, which a caller
// tidying the cluster must keep.
func (s *Server) Servers() []string {
	if s.created == nil {
		return nil
	}
	s.created.mu.Lock()
	defer s.created.mu.Unlock()
	var out []string
	for _, r := range s.created.byID {
		out = append(out, r.servers()...)
	}
	sort.Strings(out)
	return out
}

const recordPrefix = "instances/"

// Restore reads the kept instances and runs their servers again, each in
// the background: REPAIRING until it serves.
func (s *Server) Restore() error {
	c := s.created
	if c == nil || c.db == nil {
		return nil
	}
	keys, err := c.db.List(recordPrefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		b, err := c.db.Get(k)
		if err != nil {
			return err
		}
		r := &record{}
		if err := json.Unmarshal(b, r); err != nil {
			return fmt.Errorf("Memorystore instance %s: %w", k, err)
		}
		r.state, r.message = redispb.Instance_REPAIRING, "CloudBurrow is starting the instance's server again"
		if r.CustomerManagedKey != "" {
			// The key must unwrap the data key before the server runs.
			dek, err := c.unwrap(c.bg, r.CustomerManagedKey, r.WrappedKey)
			if err == nil {
				err = r.unseal(dek)
			}
			if err != nil {
				c.mu.Lock()
				c.byID[r.Name] = r
				r.suspended = true
				r.state, r.message = redispb.Instance_STATE_UNSPECIFIED, "the instance is suspended: its customer-managed key "+r.CustomerManagedKey+" cannot be used: "+err.Error()
				c.mu.Unlock()
				continue
			}
		}
		c.mu.Lock()
		c.byID[r.Name] = r
		c.mu.Unlock()
		go c.run(r.Name, "", r.spec())
	}
	return nil
}

func (c *instances) save(r *record) error {
	if c.db == nil {
		return nil
	}
	k, err := r.kept()
	if err != nil {
		return err
	}
	b, err := json.Marshal(k)
	if err != nil {
		return err
	}
	return c.db.Put(recordPrefix+r.Server, b)
}

// run provisions name's server, then marks it READY and completes op, or
// FAILED with why.
func (c *instances) run(name, op string, spec Spec) {
	c.mu.Lock()
	var snap record
	if r, ok := c.byID[name]; ok {
		snap = *r
	}
	c.mu.Unlock()
	var addr string
	var err error
	if snap.ha() {
		err = c.startHA(c.bg, snap) // which keeps the addresses
	} else {
		addr, err = c.prov.Provision(c.bg, spec)
	}
	c.mu.Lock()
	r, ok := c.byID[name]
	if ok && err != nil && op != "" {
		// A create that failed leaves no instance, as Google's does.
		delete(c.byID, name)
		if c.db != nil {
			_ = c.db.Delete(recordPrefix + r.Server)
		}
		ok = false
		r.closeEndpoints()
		gone := *r
		go func() { _ = c.removeServers(c.bg, gone) }()
	}
	if ok {
		if err != nil {
			r.state, r.message = redispb.Instance_STATE_UNSPECIFIED, "the server did not start: "+err.Error()
		} else {
			r.state, r.message = redispb.Instance_READY, ""
			if !r.ha() {
				r.addr = addr
			}
		}
	}
	delete(c.busy, name)
	var inst *redispb.Instance
	if ok {
		inst = c.render(r)
	}
	c.mu.Unlock()
	if op == "" {
		return
	}
	if err != nil {
		_ = c.ops.Fail(op, &apierror.Error{Code: codes.Internal, Message: "the instance's server did not start: " + err.Error(), Cause: err})
		return
	}
	c.finish(op, inst)
}

// finish completes an operation with its response, and stamps its end.
func (c *instances) finish(op string, response proto.Message) {
	if got, err := c.ops.Get(op); err == nil {
		if md, ok := got.Metadata.(*redispb.OperationMetadata); ok {
			md = proto.Clone(md).(*redispb.OperationMetadata)
			md.EndTime = timestamppb.New(c.now())
			_ = c.ops.SetMetadata(op, md)
		}
	}
	_ = c.ops.Succeed(op, response)
}

func (c *instances) exists(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.byID[name]
	return ok
}

func (c *instances) list(ctx context.Context, project, region string) []*redispb.Instance {
	c.mu.Lock()
	var rs []*record
	for _, r := range c.byID {
		m := instanceRE.FindStringSubmatch(r.Name)
		if m[1] == project && (region == "-" || m[2] == region) {
			rs = append(rs, r)
		}
	}
	out := make([]*redispb.Instance, 0, len(rs))
	for _, r := range rs {
		out = append(out, c.render(r))
	}
	c.mu.Unlock()
	return out
}

func (c *instances) get(ctx context.Context, name string) (*redispb.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byID[name]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return c.render(r), nil
}

func (c *instances) authString(name string) (*redispb.InstanceAuthString, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byID[name]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return &redispb.InstanceAuthString{AuthString: r.AuthString}, nil
}

// render is r as Google shapes an instance. Call with mu held.
func (c *instances) render(r *record) *redispb.Instance {
	host, port := splitAddr(r.addr, "", 0)
	region := instanceRE.FindStringSubmatch(r.Name)[2]
	inst := &redispb.Instance{
		Name:                  r.Name,
		DisplayName:           r.DisplayName,
		Labels:                copyMap(r.Labels),
		LocationId:            r.LocationID,
		CurrentLocationId:     r.LocationID,
		RedisVersion:          r.version(),
		Host:                  host,
		Port:                  port,
		CreateTime:            timestamppb.New(r.Created),
		State:                 r.state,
		StatusMessage:         r.message,
		RedisConfigs:          copyMap(r.RedisConfigs),
		Tier:                  redispb.Instance_BASIC,
		MemorySizeGb:          r.MemorySizeGB,
		ConnectMode:           redispb.Instance_DIRECT_PEERING,
		AuthEnabled:           r.AuthString != "",
		TransitEncryptionMode: redispb.Instance_DISABLED,
		ServerCaCerts:         r.serverCACerts(),
		Nodes:                 []*redispb.NodeInfo{{Id: "node-0", Zone: r.LocationID}},
		PersistenceConfig:     r.Persistence.render(c.now()),
		MaintenancePolicy:     renderPolicy(r.Maintenance),
	}
	if r.tlsOn() {
		inst.TransitEncryptionMode = redispb.Instance_SERVER_AUTHENTICATION
	}
	r.renderHA(inst)
	r.renderCMEK(inst)
	if region == "" {
		inst.LocationId = ""
	}
	return inst
}

func copyMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// DefaultRedisVersion is the one version a created instance runs: what
// the Valkey image reports (INFO redis_version 7.2.4).
const DefaultRedisVersion = "REDIS_7_2"

// idRE is Google's rule for an instance ID: 1 to 40 lowercase letters,
// digits and hyphens, starting with a letter, not ending with a hyphen.
var idRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)

// newOperation starts an operation on target, with Google's metadata.
func (c *instances) newOperation(target, verb string) string {
	m := instanceRE.FindStringSubmatch(target)
	op := c.ops.Create("projects/"+m[1]+"/locations/"+m[2], target)
	_ = c.ops.SetMetadata(op.Name, &redispb.OperationMetadata{CreateTime: timestamppb.New(c.now()),
		Target: target, Verb: verb, ApiVersion: "v1"})
	return op.Name
}

// CreateInstance runs a Valkey server for the instance and returns the
// operation that completes once it serves.
func (s *Server) CreateInstance(ctx context.Context, req *redispb.CreateInstanceRequest) (*longrunningpb.Operation, error) {
	c := s.created
	if c == nil {
		return nil, apierror.Unimplemented("CreateInstance is not served: only the instance `up` runs exists")
	}
	m := parentRE.FindStringSubmatch(req.GetParent())
	if m == nil || m[2] == "-" {
		return nil, apierror.InvalidArgument("parent must be projects/{project}/locations/{location}, not %q", req.GetParent())
	}
	id := req.GetInstanceId()
	if !idRE.MatchString(id) {
		return nil, apierror.InvalidArgument("instance_id %q must be 1 to 40 lowercase letters, digits or hyphens, start with a letter and not end with a hyphen", id)
	}
	in := req.GetInstance()
	if in == nil {
		return nil, apierror.InvalidArgument("instance is required")
	}
	if err := checkCreate(in); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/instances/" + id
	if name == s.inst.Name() {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	loc := in.GetLocationId()
	if loc == "" {
		loc = m[2] + "-a"
	}
	r := &record{Name: name, DisplayName: in.GetDisplayName(), Labels: copyMap(in.GetLabels()), LocationID: loc,
		MemorySizeGB: in.GetMemorySizeGb(), RedisConfigs: copyMap(in.GetRedisConfigs()), Created: c.now(),
		Server: ServerName(name), state: redispb.Instance_CREATING, Version: in.GetRedisVersion()}
	if mp := in.GetMaintenancePolicy(); mp != nil {
		kept, err := maintenancePolicy(mp, c.now(), c.now())
		if err != nil {
			return nil, err
		}
		r.Maintenance = kept
	}
	if err := standard(in, r); err != nil {
		return nil, err
	}
	if r.ha() {
		if _, err := c.replicaSet(); err != nil {
			return nil, err
		}
	}
	if p := in.GetPersistenceConfig(); p != nil {
		pc, err := newPersistence(p, nil, c.now())
		if err != nil {
			return nil, err
		}
		r.Persistence = pc
	}
	if in.GetTransitEncryptionMode() == redispb.Instance_SERVER_AUTHENTICATION {
		cert, key, err := newCA(name, c.now())
		if err != nil {
			return nil, apierror.Internal(err, "generate the instance's CA")
		}
		r.TransitCA, r.TransitCAKey = cert, key
	}
	if key := in.GetCustomerManagedKey(); key != "" {
		if err := checkKey(key, m[2]); err != nil {
			return nil, err
		}
		r.CustomerManagedKey = key
		if err := c.newDataKey(ctx, r); err != nil {
			return nil, err
		}
	}
	if in.GetAuthEnabled() {
		pw, err := newAuthString()
		if err != nil {
			return nil, apierror.Internal(err, "generate the AUTH string")
		}
		r.AuthString = pw
	}
	c.mu.Lock()
	if _, ok := c.byID[name]; ok {
		c.mu.Unlock()
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	if err := c.save(r); err != nil {
		c.mu.Unlock()
		return nil, apierror.Internal(err, "keep instance %s", name)
	}
	c.byID[name] = r
	op := c.newOperation(name, "create")
	c.busy[name] = op
	c.mu.Unlock()
	go c.run(name, op, r.spec())
	return s.operation(op)
}

// checkCreate refuses what CreateInstance cannot run faithfully, and what
// Google refuses.
func checkCreate(in *redispb.Instance) error {
	switch in.GetTier() {
	case redispb.Instance_BASIC:
	case redispb.Instance_STANDARD_HA:
	default:
		return apierror.InvalidArgument("tier is required: BASIC or STANDARD_HA")
	}
	if in.GetMemorySizeGb() < 1 || in.GetMemorySizeGb() > 300 {
		return apierror.InvalidArgument("memory_size_gb must be between 1 and 300, not %d", in.GetMemorySizeGb())
	}
	if err := checkVersion(in.GetRedisVersion()); err != nil {
		return err
	}
	return checkConfigs(in.GetRedisConfigs(), in.GetMemorySizeGb())
}

// newAuthString is a fresh AUTH string, shaped as Google's: a UUID.
func newAuthString() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// UpdateInstance changes a created instance's display_name, labels,
// memory_size_gb and redis_configs, the size and configs on the running
// server (CONFIG SET), and the next start of it too.
func (s *Server) UpdateInstance(ctx context.Context, req *redispb.UpdateInstanceRequest) (*longrunningpb.Operation, error) {
	in := req.GetInstance()
	if in == nil {
		return nil, apierror.InvalidArgument("instance is required")
	}
	isDefault, err := s.find(in.GetName())
	if err != nil {
		return nil, err
	}
	if isDefault {
		return nil, apierror.FailedPrecondition("instance %s is the server `cloudburrow up` runs, which cannot be updated; create an instance to change one", in.GetName())
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	set := map[string]bool{}
	for _, p := range paths {
		switch p {
		case "display_name", "displayName":
			set["display_name"] = true
		case "labels":
			set["labels"] = true
		case "memory_size_gb", "memorySizeGb":
			set["memory_size_gb"] = true
		case "redis_configs", "redisConfigs", "redis_config", "redisConfig":
			set["redis_configs"] = true
		case "persistence_config", "persistenceConfig":
			set["persistence_config"] = true
		case "maintenance_policy", "maintenancePolicy":
			set["maintenance_policy"] = true
		case "auth_enabled", "authEnabled":
			set["auth_enabled"] = true
		case "replica_count", "replicaCount":
			set["replica_count"] = true
		case "read_replicas_mode", "readReplicasMode":
			set["read_replicas_mode"] = true
		case "secondary_ip_range", "secondaryIpRange":
			// The /28 range read replicas need: there are no VPC
			// addresses here, so it is accepted and not kept.
			set["secondary_ip_range"] = true
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not an updatable field of Instance", p)
		}
	}
	c := s.created
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.byID[in.GetName()]
	if _, pending := c.busy[r.Name]; pending || r.state != redispb.Instance_READY {
		return nil, apierror.FailedPrecondition("instance %s is %s: an instance is updated only when READY", r.Name, r.state)
	}
	next := *r
	if set["display_name"] {
		next.DisplayName = in.GetDisplayName()
	}
	if set["labels"] {
		next.Labels = copyMap(in.GetLabels())
	}
	if set["memory_size_gb"] {
		if in.GetMemorySizeGb() < 1 || in.GetMemorySizeGb() > 300 {
			return nil, apierror.InvalidArgument("memory_size_gb must be between 1 and 300 for a BASIC instance, not %d", in.GetMemorySizeGb())
		}
		next.MemorySizeGB = in.GetMemorySizeGb()
	}
	if set["redis_configs"] {
		next.RedisConfigs = copyMap(in.GetRedisConfigs())
	}
	if err := checkConfigs(next.RedisConfigs, next.MemorySizeGB); err != nil {
		return nil, err
	}
	rescale := false
	if set["replica_count"] || set["read_replicas_mode"] || (r.ReadReplicas && set["memory_size_gb"]) {
		if !r.ha() {
			return nil, apierror.InvalidArgument("replica_count and read_replicas_mode apply only to a STANDARD_HA instance")
		}
		if set["read_replicas_mode"] {
			switch in.GetReadReplicasMode() {
			case redispb.Instance_READ_REPLICAS_ENABLED:
				next.ReadReplicas = true
			default:
				if r.ReadReplicas {
					return nil, apierror.InvalidArgument("read replicas cannot be disabled once they are enabled")
				}
			}
		}
		if set["replica_count"] {
			next.ReplicaCount = in.GetReplicaCount()
		}
		if next.ReadReplicas && !r.ReadReplicas && !set["replica_count"] {
			next.ReplicaCount = 0 // enabling read replicas: their default count
		}
		if err := checkReplicas(next.ReadReplicas, &next.ReplicaCount, next.MemorySizeGB); err != nil {
			return nil, err
		}
		rescale = next.ReplicaCount != r.ReplicaCount
	}
	if set["memory_size_gb"] || set["redis_configs"] {
		// The running server first: a value it refuses changes nothing.
		settings := serverConfigs(next.RedisConfigs)
		for k := range serverConfigs(r.RedisConfigs) {
			if _, kept := settings[k]; !kept {
				settings[k] = configDefaults[k] // a removed config goes back to its default
			}
		}
		settings["maxmemory"] = strconv.FormatInt(maxMemory(next.MemorySizeGB, next.RedisConfigs), 10)
		if err := c.configAll(ctx, r, settings); err != nil {
			return nil, apierror.InvalidArgument("the server refused the change: %v", err)
		}
	}
	if set["maintenance_policy"] {
		next.Maintenance = ""
		if mp := in.GetMaintenancePolicy(); mp != nil {
			kept, err := maintenancePolicy(mp, policyCreated(r.Maintenance, c.now()), c.now())
			if err != nil {
				return nil, err
			}
			next.Maintenance = kept
		}
	}
	if set["auth_enabled"] && in.GetAuthEnabled() != (r.AuthString != "") {
		// AUTH on gets a fresh string, as Memorystore's does; off clears it.
		pw := ""
		if in.GetAuthEnabled() {
			var err error
			if pw, err = newAuthString(); err != nil {
				return nil, apierror.Internal(err, "generate the AUTH string")
			}
		}
		settings := map[string]string{"requirepass": pw}
		if r.ha() {
			settings["masterauth"] = pw // the replicas authenticate to the primary with it
		}
		if err := c.configAll(ctx, r, settings); err != nil {
			return nil, apierror.InvalidArgument("the server refused the change: %v", err)
		}
		next.AuthString = pw
	}
	if set["persistence_config"] {
		pc, err := newPersistence(in.GetPersistenceConfig(), r.Persistence, c.now())
		if err != nil {
			return nil, err
		}
		if err := c.persist(ctx, &next, pc); err != nil {
			return nil, err
		}
		next.Persistence = pc
	}
	if err := c.save(&next); err != nil {
		return nil, apierror.Internal(err, "keep instance %s", r.Name)
	}
	enabled := next.ReadReplicas != r.ReadReplicas
	*r = next
	if enabled {
		if err := c.points(r, false); err != nil {
			return nil, apierror.Internal(err, "open the read endpoint of %s", r.Name)
		}
	}
	op := c.newOperation(r.Name, "update")
	if rescale {
		// Nodes are started or removed: the instance is UPDATING until
		// they are.
		r.state = redispb.Instance_UPDATING
		c.busy[r.Name] = op
		go func() {
			ctx, cancel := context.WithTimeout(c.bg, haWait)
			defer cancel()
			c.done(r.Name, op, redispb.Instance_READY, c.scale(ctx, r.Name))
		}()
		return s.operation(op)
	}
	c.finish(op, c.render(r))
	return s.operation(op)
}

// DeleteInstance removes a created instance: its server and its data.
func (s *Server) DeleteInstance(ctx context.Context, req *redispb.DeleteInstanceRequest) (*longrunningpb.Operation, error) {
	isDefault, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	if isDefault {
		return nil, apierror.FailedPrecondition("instance %s is the server `cloudburrow up` runs, which cannot be deleted; run up without memorystore in --services to have none", req.GetName())
	}
	c := s.created
	c.mu.Lock()
	r := c.byID[req.GetName()]
	if _, pending := c.busy[r.Name]; pending {
		c.mu.Unlock()
		return nil, apierror.FailedPrecondition("instance %s has an operation in progress", r.Name)
	}
	r.state = redispb.Instance_DELETING
	op := c.newOperation(r.Name, "delete")
	c.busy[r.Name] = op
	c.mu.Unlock()
	gone := *r
	go func() {
		err := c.removeServers(c.bg, gone)
		c.mu.Lock()
		delete(c.busy, r.Name)
		if err == nil {
			if c.db != nil {
				err = c.db.Delete(recordPrefix + r.Server)
				if errors.Is(err, store.ErrNotFound) {
					err = nil
				}
			}
			delete(c.byID, r.Name)
			r.closeEndpoints()
		} else {
			r.state, r.message = redispb.Instance_READY, "delete failed: "+err.Error()
		}
		c.mu.Unlock()
		if err != nil {
			_ = c.ops.Fail(op, apierror.Internal(err, "delete instance %s", r.Name))
			return
		}
		c.finish(op, &emptypb.Empty{})
	}()
	return s.operation(op)
}
