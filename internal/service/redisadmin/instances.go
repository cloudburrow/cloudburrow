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
	// RDBSeconds, above 0, is RDB persistence (#1384): a snapshot once
	// that many seconds have passed with a change, in place of the
	// CloudBurrow instance's mode's own.
	RDBSeconds int
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

// Dialer reaches the server at addr with an AUTH string ("" for none).
type Dialer func(addr, password string) DataPlane

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

	// Persistence is persistence_config, as last set; nil when never set.
	Persistence *persistence `json:"persistence,omitempty"`

	// Not kept: what the running server is now.
	state   redispb.Instance_State
	message string
	addr    string
}

func (r *record) spec() Spec {
	return Spec{Server: r.Server, MaxMemory: maxMemory(r.MemorySizeGB, r.RedisConfigs),
		Password: r.AuthString, Configs: serverConfigs(r.RedisConfigs), RDBSeconds: r.Persistence.seconds()}
}

// binary is the prefix of the server's programs: valkey-server, ...
func (r *record) binary() string { return "valkey" }

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
	// objs is this instance's Cloud Storage, for import and export; nil
	// when it is not enabled.
	objs Objects
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
		byID: map[string]*record{}, busy: map[string]string{}}
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
		out = append(out, r.Server)
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
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return c.db.Put(recordPrefix+r.Server, b)
}

// run provisions name's server, then marks it READY and completes op, or
// FAILED with why.
func (c *instances) run(name, op string, spec Spec) {
	addr, err := c.prov.Provision(c.bg, spec)
	c.mu.Lock()
	r, ok := c.byID[name]
	if ok && err != nil && op != "" {
		// A create that failed leaves no instance, as Google's does.
		delete(c.byID, name)
		if c.db != nil {
			_ = c.db.Delete(recordPrefix + r.Server)
		}
		ok = false
		go func() { _ = c.prov.Remove(c.bg, spec.Server) }()
	}
	if ok {
		if err != nil {
			r.state, r.message = redispb.Instance_STATE_UNSPECIFIED, "the server did not start: "+err.Error()
		} else {
			r.state, r.message, r.addr = redispb.Instance_READY, "", addr
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
		RedisVersion:          DefaultRedisVersion,
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
		Nodes:                 []*redispb.NodeInfo{{Id: "node-0", Zone: r.LocationID}},
		PersistenceConfig:     r.Persistence.render(c.now()),
	}
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
		Server: ServerName(name), state: redispb.Instance_CREATING}
	if p := in.GetPersistenceConfig(); p != nil {
		pc, err := newPersistence(p, nil, c.now())
		if err != nil {
			return nil, err
		}
		r.Persistence = pc
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
		return apierror.Unimplemented("tier STANDARD_HA is not supported: CloudBurrow runs one server per instance, with no replica or failover; use BASIC")
	default:
		return apierror.InvalidArgument("tier is required: BASIC or STANDARD_HA")
	}
	if in.GetMemorySizeGb() < 1 || in.GetMemorySizeGb() > 300 {
		return apierror.InvalidArgument("memory_size_gb must be between 1 and 300 for a BASIC instance, not %d", in.GetMemorySizeGb())
	}
	if v := in.GetRedisVersion(); v != "" && v != DefaultRedisVersion {
		return apierror.Unimplemented("redis_version %s is not supported: CloudBurrow's server is Valkey, which is %s", v, DefaultRedisVersion)
	}
	if in.GetTransitEncryptionMode() == redispb.Instance_SERVER_AUTHENTICATION {
		return apierror.Unimplemented("transit_encryption_mode SERVER_AUTHENTICATION is not supported: the server speaks plain TCP")
	}
	if in.GetReplicaCount() > 0 || in.GetReadReplicasMode() == redispb.Instance_READ_REPLICAS_ENABLED {
		return apierror.Unimplemented("read replicas are not supported: CloudBurrow runs one server per instance")
	}
	if in.GetCustomerManagedKey() != "" {
		return apierror.Unimplemented("customer_managed_key is not supported")
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
		case "replica_count", "replicaCount", "read_replicas_mode", "readReplicasMode",
			"maintenance_policy", "maintenancePolicy", "auth_enabled", "authEnabled", "secondary_ip_range", "secondaryIpRange":
			return nil, apierror.Unimplemented("updating %s is not supported", p)
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
	if set["memory_size_gb"] || set["redis_configs"] {
		// The running server first: a value it refuses changes nothing.
		settings := serverConfigs(next.RedisConfigs)
		for k := range serverConfigs(r.RedisConfigs) {
			if _, kept := settings[k]; !kept {
				settings[k] = configDefaults[k] // a removed config goes back to its default
			}
		}
		settings["maxmemory"] = strconv.FormatInt(maxMemory(next.MemorySizeGB, next.RedisConfigs), 10)
		if err := c.dial(r.addr, r.AuthString).ConfigSet(ctx, settings); err != nil {
			return nil, apierror.InvalidArgument("the server refused the change: %v", err)
		}
	}
	if set["persistence_config"] {
		pc, err := newPersistence(in.GetPersistenceConfig(), r.Persistence, c.now())
		if err != nil {
			return nil, err
		}
		if err := c.persist(ctx, r, pc); err != nil {
			return nil, err
		}
		next.Persistence = pc
	}
	if err := c.save(&next); err != nil {
		return nil, apierror.Internal(err, "keep instance %s", r.Name)
	}
	*r = next
	op := c.newOperation(r.Name, "update")
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
	go func() {
		err := c.prov.Remove(c.bg, r.Server)
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
