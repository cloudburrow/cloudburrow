package redisadmin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Memorystore for Valkey (#1519), google.cloud.memorystore.v1, which
// Google serves on memorystore.googleapis.com. Its gRPC service shares the
// admin port with CloudRedis and CloudRedisCluster; its JSON API, whose
// paths are the same as CloudRedis's (/v1/projects/*/locations/*/instances),
// is served under /memorystore on that port (MemorystoreRESTPrefix).
//
// An instance is built as a Redis cluster is (cluster.go), of the same
// image: in CLUSTER mode (the default) shard_count primaries, each with
// replica_count replicas, every node a cluster-mode Valkey server, the
// slots spread over the primaries; in CLUSTER_DISABLED mode one primary and
// replica_count replicas, plain Valkey servers kept in step by REPLICAOF.
// Shards are added and removed with their slots and keys moved, replicas
// started or removed. endpoints and the deprecated psc_auto_connections and
// discovery_endpoints report the nodes' host addresses: the discovery
// endpoint (the first primary) in CLUSTER mode, the primary and a reader
// (the first replica) in CLUSTER_DISABLED mode, as endpoints when they
// were asked as endpoints, as psc_auto_connections when asked so.
//
// In-transit encryption (SERVER_AUTHENTICATION, #1549) is a Redis
// cluster's (cluster.go): a CA of the instance's own, which
// GetCertificateAuthority returns as managed_server_ca, every node serving
// TLS only. IAM_AUTH takes any AUTH string (ADR-0006: IAM is not enforced).

// MemorystoreRESTPrefix is the path the JSON API of memorystore.googleapis.com
// is served under on the admin port: a client's endpoint is
// http://HOST:PORT/memorystore.
const MemorystoreRESTPrefix = "/memorystore"

const valkeyPrefix = "valkey/"

// DefaultValkeyVersion is the engine_version an instance runs when none
// is asked: the image is Valkey 8.1 (components.MemorystoreImage),
// Memorystore's VALKEY_8_0 line. VALKEY_7_2 runs Valkey 7.2 and
// VALKEY_9_0 Valkey 9.0 (#1557).
const DefaultValkeyVersion = "VALKEY_8_0"

// valkeyNodeSizes are the GB a node of each node_type holds, as Memorystore
// for Valkey documents (node specification).
var valkeyNodeSizes = map[memorystorepb.Instance_NodeType]float64{
	memorystorepb.Instance_SHARED_CORE_NANO: 1.4,
	memorystorepb.Instance_STANDARD_SMALL:   6.5,
	memorystorepb.Instance_HIGHMEM_MEDIUM:   13,
	memorystorepb.Instance_HIGHMEM_XLARGE:   58,
}

// valkeyFields are what a Valkey instance has beyond a Redis cluster's
// record. The protos are kept as protojson.
type valkeyFields struct {
	Mode          string    `json:"mode"`
	NodeType      string    `json:"nodeType"`
	EngineVersion string    `json:"engineVersion"`
	AuthMode      string    `json:"authMode"`
	Updated       time.Time `json:"updated"`
	// Persistence is the persistence_config, protojson; "" is DISABLED.
	Persistence string `json:"persistence,omitempty"`
	// Networks are the PSC auto connections asked for, project and
	// network each, from endpoints or psc_auto_connections.
	Networks []valkeyNetwork `json:"networks,omitempty"`
	// Endpoints is whether they were asked as endpoints (true) or as the
	// deprecated psc_auto_connections.
	Endpoints        bool   `json:"endpoints,omitempty"`
	ZoneDistribution string `json:"zoneDistribution,omitempty"`
	Maintenance      string `json:"maintenance,omitempty"`
	// CIR is cross-instance replication, Migration the migration
	// (crossinstance.go); nil for none.
	CIR       *cirConfig `json:"cir,omitempty"`
	Migration *migration `json:"migration,omitempty"`
}

type valkeyNetwork struct {
	Project string `json:"project"`
	Network string `json:"network"`
}

func (v *valkeyFields) nodeType() memorystorepb.Instance_NodeType {
	return memorystorepb.Instance_NodeType(memorystorepb.Instance_NodeType_value[v.NodeType])
}

func (v *valkeyFields) nodeGB() float64 { return valkeyNodeSizes[v.nodeType()] }

func (v *valkeyFields) persistence() *memorystorepb.PersistenceConfig {
	p := &memorystorepb.PersistenceConfig{Mode: memorystorepb.PersistenceConfig_DISABLED}
	if v.Persistence != "" {
		_ = protojson.Unmarshal([]byte(v.Persistence), p)
	}
	return p
}

// rdbPeriods are the seconds of each RDB snapshot period.
var rdbPeriods = map[memorystorepb.PersistenceConfig_RDBConfig_SnapshotPeriod]int{
	memorystorepb.PersistenceConfig_RDBConfig_ONE_HOUR:          3600,
	memorystorepb.PersistenceConfig_RDBConfig_SIX_HOURS:         6 * 3600,
	memorystorepb.PersistenceConfig_RDBConfig_TWELVE_HOURS:      12 * 3600,
	memorystorepb.PersistenceConfig_RDBConfig_TWENTY_FOUR_HOURS: 24 * 3600,
}

// persistenceSettings are the server settings of the persistence_config:
// RDB snapshots at the period (save N 1), or the append-only file with its
// fsync, or neither.
func (v *valkeyFields) persistenceSettings() [][2]string {
	p := v.persistence()
	switch p.GetMode() {
	case memorystorepb.PersistenceConfig_RDB:
		period := rdbPeriods[p.GetRdbConfig().GetRdbSnapshotPeriod()]
		if period == 0 {
			period = 3600
		}
		return [][2]string{{"appendonly", "no"}, {"save", fmt.Sprintf("%d 1", period)}}
	case memorystorepb.PersistenceConfig_AOF:
		fsync := "everysec"
		switch p.GetAofConfig().GetAppendFsync() {
		case memorystorepb.PersistenceConfig_AOFConfig_ALWAYS:
			fsync = "always"
		case memorystorepb.PersistenceConfig_AOFConfig_NEVER:
			fsync = "no"
		}
		return [][2]string{{"save", ""}, {"appendonly", "yes"}, {"appendfsync", fsync}}
	}
	return [][2]string{{"save", ""}, {"appendonly", "no"}}
}

func checkPersistence(p *memorystorepb.PersistenceConfig) error {
	if p == nil {
		return nil
	}
	switch p.GetMode() {
	case memorystorepb.PersistenceConfig_PERSISTENCE_MODE_UNSPECIFIED, memorystorepb.PersistenceConfig_DISABLED,
		memorystorepb.PersistenceConfig_AOF:
	case memorystorepb.PersistenceConfig_RDB:
		if per := p.GetRdbConfig().GetRdbSnapshotPeriod(); per != memorystorepb.PersistenceConfig_RDBConfig_SNAPSHOT_PERIOD_UNSPECIFIED && rdbPeriods[per] == 0 {
			return apierror.InvalidArgument("persistence_config.rdb_config.rdb_snapshot_period %s is not a snapshot period", per)
		}
	default:
		return apierror.InvalidArgument("persistence_config.mode %s is not a persistence mode", p.GetMode())
	}
	return nil
}

func protoJSON(m proto.Message) string {
	if m == nil || !m.ProtoReflect().IsValid() {
		return ""
	}
	b, err := protojson.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// MemorystoreServer implements memorystorepb.MemorystoreServer.
type MemorystoreServer struct {
	memorystorepb.UnimplementedMemorystoreServer
	api *Server
}

// NewMemorystoreServer returns the Memorystore for Valkey API of api, whose
// instances EnableValkey enables.
func NewMemorystoreServer(api *Server) *MemorystoreServer { return &MemorystoreServer{api: api} }

// Register adds the service to a gRPC server or to the JSON transcoder.
func (s *MemorystoreServer) Register(g grpc.ServiceRegistrar) {
	memorystorepb.RegisterMemorystoreServer(g, s)
}

// EnableValkey makes Memorystore for Valkey's CreateInstance and the rest
// work over rt, keeping the instances in db, their operations beside the
// Redis instances' (EnableCreate first). ctx bounds the nodes' lives.
func (s *Server) EnableValkey(ctx context.Context, rt ClusterRuntime, dial ClusterDialer, db store.Store) {
	if s.created == nil {
		panic("redisadmin: EnableValkey before EnableCreate")
	}
	s.valkey = &clusters{valkey: true, rt: rt, dial: dial, db: db, api: s, now: time.Now, bg: ctx,
		byName: map[string]*clusterRecord{}, busy: map[string]string{}}
	go s.valkey.autoBackups(ctx)
}

func (s *MemorystoreServer) instances() (*clusters, error) {
	if s.api.valkey == nil {
		return nil, apierror.Unimplemented("Memorystore for Valkey is not served by this instance")
	}
	return s.api.valkey, nil
}

// find returns the instance name, with c.mu held when err is nil.
func (s *MemorystoreServer) find(name string) (*clusters, *clusterRecord, error) {
	if instanceRE.FindStringSubmatch(name) == nil {
		return nil, nil, apierror.InvalidArgument("name must be projects/{project}/locations/{location}/instances/{instance}, not %q", name)
	}
	c, err := s.instances()
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	r, ok := c.byName[name]
	if !ok {
		c.mu.Unlock()
		return nil, nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return c, r, nil
}

// renderValkey is r as Google shapes an instance. Call with mu held.
func renderValkey(r *clusterRecord) *memorystorepb.Instance {
	v := r.Valkey
	m := nodesOwnerRE.FindStringSubmatch(r.Name)
	mode := memorystorepb.Instance_CLUSTER
	if r.standalone() {
		mode = memorystorepb.Instance_CLUSTER_DISABLED
	}
	out := &memorystorepb.Instance{
		Name:                      r.Name,
		Uid:                       r.UID,
		CreateTime:                timestamppb.New(r.Created),
		UpdateTime:                timestamppb.New(v.Updated),
		Labels:                    copyMap(r.Labels),
		State:                     memorystorepb.Instance_State(memorystorepb.Instance_State_value[r.state.String()]),
		ReplicaCount:              proto.Int32(r.ReplicaCount),
		AuthorizationMode:         memorystorepb.Instance_AuthorizationMode(memorystorepb.Instance_AuthorizationMode_value[v.AuthMode]),
		TransitEncryptionMode:     memorystorepb.Instance_TRANSIT_ENCRYPTION_DISABLED,
		ShardCount:                r.ShardCount,
		NodeType:                  v.nodeType(),
		PersistenceConfig:         v.persistence(),
		EngineVersion:             v.EngineVersion,
		EngineConfigs:             copyMap(r.RedisConfigs),
		NodeConfig:                &memorystorepb.NodeConfig{SizeGb: v.nodeGB()},
		ZoneDistributionConfig:    &memorystorepb.ZoneDistributionConfig{Mode: memorystorepb.ZoneDistributionConfig_MULTI_ZONE},
		DeletionProtectionEnabled: proto.Bool(r.DeletionProtect),
		Mode:                      mode,
		SatisfiesPzs:              proto.Bool(false),
		SatisfiesPzi:              proto.Bool(false),
	}
	if r.TransitCA != "" {
		out.TransitEncryptionMode = memorystorepb.Instance_SERVER_AUTHENTICATION
	}
	if r.KmsKey != "" {
		out.KmsKey = proto.String(r.KmsKey)
		out.EncryptionInfo = convert(r.encryptionInfo(), &memorystorepb.EncryptionInfo{})
	}
	renderCIR(r, out)
	if r.AutoBackup != "" {
		ab := &clusterpb.AutomatedBackupConfig{}
		if protojson.Unmarshal([]byte(r.AutoBackup), ab) == nil {
			out.AutomatedBackupConfig = convert(ab, &memorystorepb.AutomatedBackupConfig{})
		}
	}
	if col := collectionName(r); col != "" && r.state != clusterpb.Cluster_CREATING {
		out.BackupCollection = proto.String(col)
	}
	switch {
	case r.Source != "":
		out.ImportSources = &memorystorepb.Instance_ManagedBackupSource_{ManagedBackupSource: &memorystorepb.Instance_ManagedBackupSource{Backup: r.Source}}
	case len(r.SourceURIs) > 0:
		out.ImportSources = &memorystorepb.Instance_GcsSource{GcsSource: &memorystorepb.Instance_GcsBackupSource{Uris: r.SourceURIs}}
	}
	if pc := out.PersistenceConfig; pc.GetMode() == memorystorepb.PersistenceConfig_RDB {
		// Google returns the time snapshots are counted from: the
		// create time, unless one was asked.
		if pc.RdbConfig == nil {
			pc.RdbConfig = &memorystorepb.PersistenceConfig_RDBConfig{RdbSnapshotPeriod: memorystorepb.PersistenceConfig_RDBConfig_ONE_HOUR}
		}
		if pc.RdbConfig.RdbSnapshotStartTime == nil {
			pc.RdbConfig.RdbSnapshotStartTime = timestamppb.New(r.Created)
		}
	}
	if v.ZoneDistribution != "" {
		z := &memorystorepb.ZoneDistributionConfig{}
		if protojson.Unmarshal([]byte(v.ZoneDistribution), z) == nil {
			out.ZoneDistributionConfig = z
		}
	}
	if v.Maintenance != "" {
		mp := &memorystorepb.MaintenancePolicy{}
		if protojson.Unmarshal([]byte(v.Maintenance), mp) == nil {
			out.MaintenancePolicy = mp
		}
	}
	if r.state == clusterpb.Cluster_UPDATING {
		ui := &memorystorepb.Instance_StateInfo_UpdateInfo{}
		if r.pendingShards > 0 {
			ui.TargetShardCount = proto.Int32(r.pendingShards)
		}
		if r.pendingReplicas >= 0 {
			ui.TargetReplicaCount = proto.Int32(r.pendingReplicas)
		}
		out.StateInfo = &memorystorepb.Instance_StateInfo{Info: &memorystorepb.Instance_StateInfo_UpdateInfo_{UpdateInfo: ui}}
	}
	if r.state == clusterpb.Cluster_CREATING {
		return out
	}
	type conn struct {
		kind memorystorepb.ConnectionType
		node *clusterNode
	}
	var conns []conn
	if mode == memorystorepb.Instance_CLUSTER {
		conns = append(conns, conn{memorystorepb.ConnectionType_CONNECTION_TYPE_DISCOVERY, r.firstPrimary()})
	} else {
		conns = append(conns, conn{memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY, r.firstPrimary()})
		for _, n := range r.Nodes {
			if !n.Primary {
				conns = append(conns, conn{memorystorepb.ConnectionType_CONNECTION_TYPE_READER, n})
				break
			}
		}
	}
	if first := r.firstPrimary(); mode == memorystorepb.Instance_CLUSTER && first != nil && first.addr != "" {
		host, port := splitAddr(first.addr, "", 0)
		ep := &memorystorepb.DiscoveryEndpoint{Address: host, Port: port}
		if len(v.Networks) > 0 {
			ep.Network = v.Networks[0].Network
		}
		out.DiscoveryEndpoints = []*memorystorepb.DiscoveryEndpoint{ep}
	}
	for i, net := range v.Networks {
		ie := &memorystorepb.Instance_InstanceEndpoint{}
		for j, cn := range conns {
			if cn.node == nil || cn.node.addr == "" {
				continue
			}
			host, port := splitAddr(cn.node.addr, "", 0)
			id := fmt.Sprintf("%s-psc-%d-%d", r.UID[:8], i, j)
			pac := &memorystorepb.PscAutoConnection{
				Ports:               &memorystorepb.PscAutoConnection_Port{Port: port},
				PscConnectionId:     id,
				IpAddress:           host,
				ForwardingRule:      fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/regions/%s/forwardingRules/%s", net.Project, m[2], id),
				ProjectId:           net.Project,
				Network:             net.Network,
				ServiceAttachment:   fmt.Sprintf("projects/%s/regions/%s/serviceAttachments/%s-%d", m[1], m[2], m[3], j),
				PscConnectionStatus: memorystorepb.PscConnectionStatus_ACTIVE,
				ConnectionType:      cn.kind,
			}
			// Each in the form it was asked in: the Terraform provider reads
			// psc_auto_connections back as desired_psc_auto_connections and
			// endpoints as desired_auto_created_endpoints, each forcing a
			// replacement when it appears unasked.
			if !v.Endpoints {
				out.PscAutoConnections = append(out.PscAutoConnections, pac)
				continue
			}
			ie.Connections = append(ie.Connections, &memorystorepb.Instance_ConnectionDetail{
				Connection: &memorystorepb.Instance_ConnectionDetail_PscAutoConnection{PscAutoConnection: pac}})
		}
		if v.Endpoints {
			out.Endpoints = append(out.Endpoints, ie)
		}
	}
	for j, cn := range conns {
		out.PscAttachmentDetails = append(out.PscAttachmentDetails, &memorystorepb.PscAttachmentDetail{
			ServiceAttachment: fmt.Sprintf("projects/%s/regions/%s/serviceAttachments/%s-%d", m[1], m[2], m[3], j),
			ConnectionType:    cn.kind,
		})
	}
	return out
}

// ListInstances lists a project's instances in one region, or in every
// region ("locations/-"), in name order and paged.
func (s *MemorystoreServer) ListInstances(ctx context.Context, req *memorystorepb.ListInstancesRequest) (*memorystorepb.ListInstancesResponse, error) {
	m := parentRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent must be projects/{project}/locations/{location}, not %q", req.GetParent())
	}
	if req.GetPageSize() < 0 {
		return nil, apierror.InvalidArgument("page_size must not be negative")
	}
	if req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Unimplemented("filter and order_by are not served for Memorystore instances here")
	}
	resp := &memorystorepb.ListInstancesResponse{}
	c := s.api.valkey
	if c == nil {
		return resp, nil
	}
	c.mu.Lock()
	byName := map[string]*memorystorepb.Instance{}
	for n, r := range c.byName {
		k := instanceRE.FindStringSubmatch(n)
		if k[1] == m[1] && (m[2] == "-" || k[2] == m[2]) {
			byName[n] = renderValkey(r)
		}
	}
	c.mu.Unlock()
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	page, next, err := paging.Page("memorystore-instances:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token %q", req.GetPageToken())
	}
	resp.NextPageToken = next
	for _, n := range page {
		resp.Instances = append(resp.Instances, byName[n])
	}
	return resp, nil
}

// GetInstance describes an instance.
func (s *MemorystoreServer) GetInstance(ctx context.Context, req *memorystorepb.GetInstanceRequest) (*memorystorepb.Instance, error) {
	c, r, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	return renderValkey(r), nil
}

// GetCertificateAuthority returns an instance's CA, as managed_server_ca;
// an instance without in-transit encryption has none.
func (s *MemorystoreServer) GetCertificateAuthority(ctx context.Context, req *memorystorepb.GetCertificateAuthorityRequest) (*memorystorepb.CertificateAuthority, error) {
	// gRPC names the CA (.../certificateAuthority); the JSON API of
	// memorystore.googleapis.com names the instance.
	name := strings.TrimSuffix(req.GetName(), "/certificateAuthority")
	c, r, err := s.find(name)
	if err != nil {
		return nil, err
	}
	ca := r.TransitCA
	c.mu.Unlock()
	if ca == "" {
		return nil, apierror.FailedPrecondition("instance %s has no certificate authority: its transit_encryption_mode is TRANSIT_ENCRYPTION_DISABLED", name)
	}
	return &memorystorepb.CertificateAuthority{Name: name,
		ServerCa: &memorystorepb.CertificateAuthority_ManagedServerCa{ManagedServerCa: &memorystorepb.CertificateAuthority_ManagedCertificateAuthority{
			CaCerts: []*memorystorepb.CertificateAuthority_ManagedCertificateAuthority_CertChain{{Certificates: []string{ca}}}}}}, nil
}

// valkeyConfigs are the engine_configs an instance takes, as Memorystore
// for Valkey documents them; each is set on every node.
var valkeyConfigs = map[string]bool{
	"maxmemory-policy": true, "maxmemory-clients": true, "notify-keyspace-events": true,
	"slowlog-log-slower-than": true, "maxclients": true, "lfu-log-factor": true, "lfu-decay-time": true,
	"maxmemory-samples": true, "stream-node-max-bytes": true, "stream-node-max-entries": true, "activedefrag": true,
}

func checkValkeyConfigs(cfg map[string]string) error {
	for k, v := range cfg {
		if !valkeyConfigs[k] {
			return apierror.InvalidArgument("engine_configs %q is not a configuration Memorystore for Valkey supports", k)
		}
		if k == "maxmemory-policy" && !policies[v] {
			return apierror.InvalidArgument("engine_configs %s=%q is not an eviction policy", k, v)
		}
	}
	return nil
}

func checkEngineVersion(v string) error {
	if _, ok := valkeyVersions[v]; ok || v == "" {
		return nil
	}
	return apierror.InvalidArgument("engine_version %q is not a Memorystore for Valkey engine version", v)
}

func checkValkeyNodeType(nt memorystorepb.Instance_NodeType) error {
	if _, ok := valkeyNodeSizes[nt]; ok {
		return nil
	}
	if _, known := memorystorepb.Instance_NodeType_name[int32(nt)]; known && nt != memorystorepb.Instance_NODE_TYPE_UNSPECIFIED {
		return apierror.Unimplemented("node_type %s is not served here: CloudBurrow sizes SHARED_CORE_NANO, STANDARD_SMALL, HIGHMEM_MEDIUM and HIGHMEM_XLARGE nodes", nt)
	}
	return apierror.InvalidArgument("node_type %s is not a Memorystore for Valkey node type", nt)
}

// networks reads the PSC auto connections asked for, from endpoints (and
// whether they were) or psc_auto_connections.
func networks(in *memorystorepb.Instance) ([]valkeyNetwork, bool, error) {
	var out []valkeyNetwork
	add := func(p *memorystorepb.PscAutoConnection) error {
		if p.GetNetwork() == "" || p.GetProjectId() == "" {
			return apierror.InvalidArgument("a PSC auto connection needs project_id and network")
		}
		for _, n := range out {
			if n.Network == p.GetNetwork() {
				return nil
			}
		}
		out = append(out, valkeyNetwork{Project: p.GetProjectId(), Network: p.GetNetwork()})
		return nil
	}
	for _, e := range in.GetEndpoints() {
		for _, cn := range e.GetConnections() {
			if cn.GetPscConnection() != nil {
				return nil, false, apierror.Unimplemented("user-created PSC connections (endpoints.connections.psc_connection) are not served here: use psc_auto_connection")
			}
			if err := add(cn.GetPscAutoConnection()); err != nil {
				return nil, false, err
			}
		}
	}
	if len(out) > 0 {
		return out, true, nil
	}
	for _, p := range in.GetPscAutoConnections() {
		if err := add(p); err != nil {
			return nil, false, err
		}
	}
	return out, false, nil
}

func checkValkeyShape(mode memorystorepb.Instance_Mode, shards, replicas int32) error {
	if mode == memorystorepb.Instance_CLUSTER_DISABLED && shards != 1 {
		return apierror.InvalidArgument("shard_count must be 1 for an instance in CLUSTER_DISABLED mode, not %d", shards)
	}
	return checkShape(shards, replicas)
}

// CreateInstance runs an instance of the shape asked and returns the
// operation that completes once it serves.
func (s *MemorystoreServer) CreateInstance(ctx context.Context, req *memorystorepb.CreateInstanceRequest) (*longrunningpb.Operation, error) {
	c, err := s.instances()
	if err != nil {
		return nil, err
	}
	m := parentRE.FindStringSubmatch(req.GetParent())
	if m == nil || m[2] == "-" {
		return nil, apierror.InvalidArgument("parent must be projects/{project}/locations/{location}, not %q", req.GetParent())
	}
	id := req.GetInstanceId()
	if !clusterIDRE.MatchString(id) {
		return nil, apierror.InvalidArgument("instance_id %q must be 4 to 63 lowercase letters, digits or hyphens, start with a letter and not end with a hyphen", id)
	}
	in := req.GetInstance()
	if in == nil {
		return nil, apierror.InvalidArgument("instance is required")
	}
	mode := in.GetMode()
	switch mode {
	case memorystorepb.Instance_MODE_UNSPECIFIED:
		mode = memorystorepb.Instance_CLUSTER
	case memorystorepb.Instance_CLUSTER, memorystorepb.Instance_CLUSTER_DISABLED:
	case memorystorepb.Instance_STANDALONE:
		return nil, apierror.InvalidArgument("mode STANDALONE is deprecated: use CLUSTER_DISABLED")
	default:
		return nil, apierror.InvalidArgument("mode %s is not an instance mode", mode)
	}
	shards := in.GetShardCount()
	if shards == 0 {
		if mode == memorystorepb.Instance_CLUSTER {
			return nil, apierror.InvalidArgument("instance.shard_count is required")
		}
		shards = 1
	}
	if err := checkValkeyShape(mode, shards, in.GetReplicaCount()); err != nil {
		return nil, err
	}
	nt := in.GetNodeType()
	if nt == memorystorepb.Instance_NODE_TYPE_UNSPECIFIED {
		nt = memorystorepb.Instance_HIGHMEM_MEDIUM
	}
	if err := checkValkeyNodeType(nt); err != nil {
		return nil, err
	}
	version := in.GetEngineVersion()
	if err := checkEngineVersion(version); err != nil {
		return nil, err
	}
	if version == "" {
		version = DefaultValkeyVersion
	}
	auth := in.GetAuthorizationMode()
	switch auth {
	case memorystorepb.Instance_AUTHORIZATION_MODE_UNSPECIFIED:
		auth = memorystorepb.Instance_AUTH_DISABLED
	case memorystorepb.Instance_AUTH_DISABLED:
	case memorystorepb.Instance_IAM_AUTH:
		// A client's AUTH is its access token. IAM is not enforced here
		// (ADR-0006), so the nodes take any AUTH string (docs/memorystore.md).
	case memorystorepb.Instance_TOKEN_AUTH:
		// Token auth users and their tokens (clusterauth.go).
	default:
		return nil, apierror.InvalidArgument("authorization_mode %s is not an authorization mode", auth)
	}
	tlsOn := false
	switch tm := in.GetTransitEncryptionMode(); tm {
	case memorystorepb.Instance_TRANSIT_ENCRYPTION_MODE_UNSPECIFIED, memorystorepb.Instance_TRANSIT_ENCRYPTION_DISABLED:
	case memorystorepb.Instance_SERVER_AUTHENTICATION:
		tlsOn = true
	default:
		return nil, apierror.InvalidArgument("transit_encryption_mode %s is not a transit encryption mode", tm)
	}
	if in.GetServerCaPool() != "" ||
		(in.ServerCaMode != nil && in.GetServerCaMode() != memorystorepb.Instance_SERVER_CA_MODE_UNSPECIFIED) {
		return nil, apierror.Unimplemented("server CA modes are not served for a Memorystore for Valkey instance here")
	}
	primary, err := cirPrimary(in, mode, tlsOn)
	if err != nil {
		return nil, err
	}
	ab := convert(in.GetAutomatedBackupConfig(), &clusterpb.AutomatedBackupConfig{})
	if in.GetAutomatedBackupConfig() == nil {
		ab = nil
	}
	if err := checkAutoBackup(ab); err != nil {
		return nil, err
	}
	if err := checkValkeyConfigs(in.GetEngineConfigs()); err != nil {
		return nil, err
	}
	if err := checkPersistence(in.GetPersistenceConfig()); err != nil {
		return nil, err
	}
	nets, asEndpoints, err := networks(in)
	if err != nil {
		return nil, err
	}
	name := req.GetParent() + "/instances/" + id
	now := c.now()
	r := &clusterRecord{Name: name, UID: newUID(), Created: now, ShardCount: shards, ReplicaCount: in.GetReplicaCount(),
		RedisConfigs: copyMap(in.GetEngineConfigs()), DeletionProtect: in.GetDeletionProtectionEnabled(),
		Labels: copyMap(in.GetLabels()), state: clusterpb.Cluster_CREATING, pendingReplicas: -1,
		Valkey: &valkeyFields{Mode: mode.String(), NodeType: nt.String(), EngineVersion: version, AuthMode: auth.String(),
			Updated: now, Persistence: protoJSON(in.GetPersistenceConfig()), Networks: nets, Endpoints: asEndpoints,
			ZoneDistribution: protoJSON(in.GetZoneDistributionConfig()), Maintenance: protoJSON(in.GetMaintenancePolicy())}}
	if auth == memorystorepb.Instance_TOKEN_AUTH {
		r.AdminPassword = newUID() + newUID()
	}
	r.AutoBackup = protoJSON(ab)
	if key := in.GetKmsKey(); key != "" {
		if err := c.newClusterKey(ctx, r, key, m[2]); err != nil {
			return nil, err
		}
	}
	switch src := in.GetImportSources().(type) {
	case *memorystorepb.Instance_GcsSource:
		r.SourceURIs = src.GcsSource.GetUris()
		r.restore, err = c.readGCS(ctx, r.SourceURIs, "instance.gcs_source.uris")
	case *memorystorepb.Instance_ManagedBackupSource_:
		r.Source = src.ManagedBackupSource.GetBackup()
		r.restore, err = c.readBackup(r.Source, "instance.managed_backup_source.backup")
	}
	if err != nil {
		return nil, err
	}
	if tlsOn {
		if r.TransitCA, r.TransitCAKey, err = newCA(id, now); err != nil {
			return nil, apierror.Internal(err, "create the CA of instance %s", name)
		}
	}
	for sh := 0; sh < int(r.ShardCount); sh++ {
		for k := 0; k <= int(r.ReplicaCount); k++ {
			r.Nodes = append(r.Nodes, &clusterNode{Server: clusterServerName(name, r.NextNode), Shard: sh, Primary: k == 0})
			r.NextNode++
		}
	}
	c.mu.Lock()
	if _, ok := c.byName[name]; ok {
		c.mu.Unlock()
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	if primary != "" {
		if err := c.checkPrimary(r, primary); err != nil {
			c.mu.Unlock()
			return nil, err
		}
	}
	if err := c.save(r); err != nil {
		c.mu.Unlock()
		return nil, apierror.Internal(err, "keep instance %s", name)
	}
	c.byName[name] = r
	op := c.newOperation(name, "create")
	c.busy[name] = op
	c.mu.Unlock()
	go c.create(name, op)
	return s.api.operation(op)
}

// UpdateInstance changes shard_count, replica_count, node_type,
// engine_configs, persistence_config, labels, deletion protection, the
// endpoints, zone distribution and maintenance policy.
func (s *MemorystoreServer) UpdateInstance(ctx context.Context, req *memorystorepb.UpdateInstanceRequest) (*longrunningpb.Operation, error) {
	in := req.GetInstance()
	if in == nil {
		return nil, apierror.InvalidArgument("instance is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	set := map[string]bool{}
	for _, p := range paths {
		if head, _, ok := strings.Cut(p, "."); ok {
			// gcloud names a message's fields (#1557):
			// cross_instance_replication_config.instance_role and the like.
			switch head {
			case "cross_instance_replication_config", "crossInstanceReplicationConfig",
				"persistence_config", "persistenceConfig", "automated_backup_config", "automatedBackupConfig":
				p = head
			}
		}
		switch p {
		case "shard_count", "shardCount":
			set["shard_count"] = true
		case "replica_count", "replicaCount":
			set["replica_count"] = true
		case "node_type", "nodeType":
			set["node_type"] = true
		case "engine_configs", "engineConfigs":
			set["engine_configs"] = true
		case "engine_version", "engineVersion":
			set["engine_version"] = true
		case "persistence_config", "persistenceConfig":
			set["persistence_config"] = true
		case "labels":
			set["labels"] = true
		case "deletion_protection_enabled", "deletionProtectionEnabled":
			set["deletion_protection_enabled"] = true
		case "endpoints":
			set["endpoints"] = true
		case "zone_distribution_config", "zoneDistributionConfig":
			set["zone_distribution_config"] = true
		case "maintenance_policy", "maintenancePolicy":
			set["maintenance_policy"] = true
		case "automated_backup_config", "automatedBackupConfig":
			set["automated_backup_config"] = true
		case "cross_instance_replication_config", "crossInstanceReplicationConfig":
			set["cross_instance_replication_config"] = true
		case "authorization_mode", "authorizationMode", "transit_encryption_mode", "transitEncryptionMode", "mode",
			"psc_auto_connections", "pscAutoConnections":
			return nil, apierror.InvalidArgument("update_mask path %q is an immutable field of Instance", p)
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not an updatable field of Instance", p)
		}
	}
	c, r, err := s.find(in.GetName())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	if _, pending := c.busy[r.Name]; pending || r.state != clusterpb.Cluster_ACTIVE {
		return nil, apierror.FailedPrecondition("instance %s is %s: an instance is updated only when ACTIVE", r.Name, r.state)
	}
	if r.Valkey.Migration.migrating() {
		return nil, apierror.FailedPrecondition("instance %s is migrating: an instance is updated once FinishMigration completes", r.Name)
	}
	if set["cross_instance_replication_config"] {
		if len(set) > 1 {
			return nil, apierror.InvalidArgument("cross_instance_replication_config is updated alone, not with other fields")
		}
		return s.updateCIR(c, r, in.GetCrossInstanceReplicationConfig())
	}
	next := *r
	vf := *r.Valkey
	next.Valkey = &vf
	if set["shard_count"] {
		next.ShardCount = in.GetShardCount()
	}
	if set["replica_count"] {
		next.ReplicaCount = in.GetReplicaCount()
	}
	mode := memorystorepb.Instance_Mode(memorystorepb.Instance_Mode_value[vf.Mode])
	if err := checkValkeyShape(mode, next.ShardCount, next.ReplicaCount); err != nil {
		return nil, err
	}
	if next.ShardCount != r.ShardCount && next.ReplicaCount != r.ReplicaCount {
		return nil, apierror.InvalidArgument("shard_count and replica_count cannot be changed in one update")
	}
	if set["node_type"] {
		if err := checkValkeyNodeType(in.GetNodeType()); err != nil {
			return nil, err
		}
		vf.NodeType = in.GetNodeType().String()
	}
	upgradeTo := ""
	if set["engine_version"] {
		if v := in.GetEngineVersion(); v != vf.EngineVersion {
			if err := checkEngineVersion(v); err != nil {
				return nil, err
			}
			if valkeyVersions[v] <= valkeyVersions[vf.EngineVersion] {
				return nil, apierror.InvalidArgument("engine_version %q is not a later version than %s: an instance is only upgraded", v, vf.EngineVersion)
			}
			if _, ok := c.rt.(Shell); !ok {
				return nil, apierror.Unimplemented("engine upgrades are not supported by this instance's servers")
			}
			upgradeTo = v
		}
	}
	if set["engine_configs"] {
		if err := checkValkeyConfigs(in.GetEngineConfigs()); err != nil {
			return nil, err
		}
		next.RedisConfigs = copyMap(in.GetEngineConfigs())
	}
	if set["persistence_config"] {
		if err := checkPersistence(in.GetPersistenceConfig()); err != nil {
			return nil, err
		}
		vf.Persistence = protoJSON(in.GetPersistenceConfig())
	}
	if set["labels"] {
		next.Labels = copyMap(in.GetLabels())
	}
	if set["deletion_protection_enabled"] {
		next.DeletionProtect = in.GetDeletionProtectionEnabled()
	}
	if set["endpoints"] {
		nets, _, err := networks(&memorystorepb.Instance{Endpoints: in.GetEndpoints()})
		if err != nil {
			return nil, err
		}
		vf.Networks, vf.Endpoints = nets, true
	}
	if set["zone_distribution_config"] {
		vf.ZoneDistribution = protoJSON(in.GetZoneDistributionConfig())
	}
	if set["maintenance_policy"] {
		vf.Maintenance = protoJSON(in.GetMaintenancePolicy())
	}
	if set["automated_backup_config"] {
		next.AutoBackup = ""
		if in.GetAutomatedBackupConfig() != nil {
			ab := convert(in.GetAutomatedBackupConfig(), &clusterpb.AutomatedBackupConfig{})
			if err := checkAutoBackup(ab); err != nil {
				return nil, err
			}
			next.AutoBackup = protoJSON(ab)
		}
	}
	if set["node_type"] || set["engine_configs"] || set["persistence_config"] {
		settings := serverConfigs(next.RedisConfigs)
		for k := range serverConfigs(r.RedisConfigs) {
			if _, kept := settings[k]; !kept {
				if d, ok := configDefaults[k]; ok {
					settings[k] = d
				} else if d, ok := valkeyConfigDefaults[k]; ok {
					settings[k] = d
				}
			}
		}
		settings["maxmemory"] = strconv.FormatInt(next.maxMemory(), 10)
		var pairs [][2]string
		for k, v := range settings {
			pairs = append(pairs, [2]string{k, v})
		}
		pairs = append(pairs, vf.persistenceSettings()...)
		for _, n := range r.Nodes {
			for _, kv := range pairs {
				if err := c.do(ctx, r, n, "CONFIG", "SET", kv[0], kv[1]); err != nil {
					return nil, apierror.InvalidArgument("the server refused the change: %v", err)
				}
			}
		}
	}
	vf.Updated = c.now()
	reshape := next.ShardCount != r.ShardCount || next.ReplicaCount != r.ReplicaCount
	shards, replicas := next.ShardCount, next.ReplicaCount
	next.ShardCount, next.ReplicaCount = r.ShardCount, r.ReplicaCount // until the nodes are
	if err := c.save(&next); err != nil {
		return nil, apierror.Internal(err, "keep instance %s", r.Name)
	}
	*r = next
	op := c.newOperation(r.Name, "update")
	if upgradeTo != "" && reshape {
		return nil, apierror.InvalidArgument("engine_version and the instance's shape cannot be changed in one update")
	}
	if upgradeTo != "" {
		r.state, r.message = clusterpb.Cluster_UPDATING, "upgrading the engine to "+upgradeTo
		c.busy[r.Name] = op
		go c.upgrade(r.Name, op, upgradeTo)
		return s.api.operation(op)
	}
	if !reshape {
		c.finish(op, renderValkey(r), nil)
		return s.api.operation(op)
	}
	r.state, r.message = clusterpb.Cluster_UPDATING, "changing the instance's shape"
	r.pendingShards, r.pendingReplicas = 0, -1
	if shards != r.ShardCount {
		r.pendingShards = shards
	}
	if replicas != r.ReplicaCount {
		r.pendingReplicas = replicas
	}
	c.busy[r.Name] = op
	go c.reshape(r.Name, op, shards, replicas)
	return s.api.operation(op)
}

// valkeyConfigDefaults are the values an engine config goes back to when
// it is removed, for those configDefaults has not: Valkey's own.
var valkeyConfigDefaults = map[string]string{
	"maxmemory-clients": "0", "slowlog-log-slower-than": "10000", "maxclients": "10000", "maxmemory-samples": "5",
}

// DeleteInstance removes an instance: its nodes and their data.
func (s *MemorystoreServer) DeleteInstance(ctx context.Context, req *memorystorepb.DeleteInstanceRequest) (*longrunningpb.Operation, error) {
	c, r, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	if _, pending := c.busy[r.Name]; pending {
		return nil, apierror.FailedPrecondition("instance %s has an operation in progress", r.Name)
	}
	if r.DeletionProtect {
		return nil, apierror.FailedPrecondition("instance %s has deletion protection enabled: disable it first", r.Name)
	}
	if len(c.secondariesOf(r.Name)) > 0 {
		return nil, apierror.FailedPrecondition("instance %s is the primary of cross-instance replication: detach its secondary instances first", r.Name)
	}
	if r.Valkey.role() == memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String() && r.Valkey.CIR.Primary != nil {
		if p := c.byName[r.Valkey.CIR.Primary.Name]; p != nil {
			var rest []*clusterRecord
			for _, x := range c.secondariesOf(p.Name) {
				if x != r {
					rest = append(rest, x)
				}
			}
			c.setMembership(p, rest)
		}
	}
	r.state = clusterpb.Cluster_DELETING
	op := c.newOperation(r.Name, "delete")
	c.busy[r.Name] = op
	nodes := append([]*clusterNode(nil), r.Nodes...)
	name := r.Name
	go func() {
		c.removeNodes(nodes)
		c.mu.Lock()
		delete(c.busy, name)
		delete(c.byName, name)
		var err error
		if c.db != nil {
			if derr := c.db.Delete(valkeyPrefix + clusterKey(name)); derr != nil && !errors.Is(derr, store.ErrNotFound) {
				err = derr
			}
		}
		c.mu.Unlock()
		c.finish(op, &emptypb.Empty{}, err)
	}()
	return s.api.operation(op)
}
