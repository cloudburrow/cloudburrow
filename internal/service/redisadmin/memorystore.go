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

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
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
// endpoint (the first primary) in CLUSTER mode, the primary and reader
// endpoints in CLUSTER_DISABLED mode (valkeyfailover.go: host addresses
// that follow the roles, a replica promoted when the primary fails), as
// endpoints when they were asked as endpoints, as psc_auto_connections
// when asked so.
//
// In-transit encryption (SERVER_AUTHENTICATION, #1549) is a Redis
// cluster's (cluster.go): a CA of the instance's own, which
// GetCertificateAuthority returns as managed_server_ca, every node serving
// TLS only. IAM_AUTH takes an access token this instance minted for a
// principal granted roles/memorystore.dbConnectionUser (iamauth.go, #1614).

// MemorystoreRESTPrefix is the path the JSON API of memorystore.googleapis.com
// is served under on the admin port: a client's endpoint is
// http://HOST:PORT/memorystore.
const MemorystoreRESTPrefix = "/memorystore"

const valkeyPrefix = "valkey/"

// DefaultValkeyVersion is the engine_version an instance runs when none
// is asked: VALKEY_9_1, Valkey 9.1, as Google documents (#2019: "If you
// don't specify a value, then VALKEY_9_1 is the default value"). VALKEY_7_2
// runs Valkey 7.2, VALKEY_8_0 Valkey 8.1 (components.MemorystoreImage) and
// VALKEY_9_0 Valkey 9.0 (#1557).
const DefaultValkeyVersion = "VALKEY_9_1"

// redisClusterEngineVersion is the Valkey line a Memorystore for Redis
// Cluster's nodes run, and the engine_version a Valkey instance stored
// before engine versions were (#1557) keeps: Valkey 8.1.
const redisClusterEngineVersion = "VALKEY_8_0"

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
	// LinkPassword is the password of the user a CLUSTER mode secondary's
	// replication links write as (crossclusterlink.go).
	LinkPassword string `json:"linkPassword,omitempty"`
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
	s.valkey = &clusters{valkey: true, rt: rt, dial: dial, db: db, api: s, now: s.now, bg: ctx,
		byName: map[string]*clusterRecord{}, busy: map[string]string{}}
	go s.valkey.autoBackups(ctx)
	go s.valkey.maintenanceLoop(ctx)
	// Both read here, not in the goroutine: tests swap them between runs.
	go s.valkey.watchStandalone(ctx, healthEvery, tendEvery)
	go s.valkey.watchClusters(ctx, healthEvery)
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
func renderValkeyBase(r *clusterRecord) *memorystorepb.Instance {
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
	ca := &clusterpb.Cluster{}
	r.renderServerCA(ca)
	if ca.ServerCaMode != nil {
		out.ServerCaMode = memorystorepb.Instance_ServerCaMode(ca.GetServerCaMode()).Enum()
	}
	out.ServerCaPool = ca.ServerCaPool
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
	out.ZoneDistributionConfig.ActiveZones = r.activeZones()
	if v.Maintenance != "" {
		mp := &memorystorepb.MaintenancePolicy{}
		if protojson.Unmarshal([]byte(v.Maintenance), mp) == nil {
			out.MaintenancePolicy = mp
		}
	}
	if m := r.Scheduled; m != nil {
		out.MaintenanceSchedule = &memorystorepb.MaintenanceSchedule{StartTime: timestamppb.New(m.Start), EndTime: timestamppb.New(m.Start.Add(maintenanceWindow))}
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
		addr string
	}
	addrOf := func(n *clusterNode) string {
		if n == nil {
			return ""
		}
		return n.addr
	}
	var conns []conn
	if mode == memorystorepb.Instance_CLUSTER {
		conns = append(conns, conn{memorystorepb.ConnectionType_CONNECTION_TYPE_DISCOVERY, addrOf(r.firstPrimary())})
	} else {
		// The primary and reader endpoints (valkeyfailover.go), which
		// outlive a node; a node's own address until they are open; none
		// while the nodes do not serve (a suspended instance's, cmek).
		addr := addrOf(r.firstPrimary())
		if addr != "" && r.primaryAddr != "" {
			addr = r.primaryAddr
		}
		conns = append(conns, conn{memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY, addr})
		for _, n := range r.Nodes {
			if !n.Primary {
				addr := n.addr
				if addr != "" && r.readerAddr != "" {
					addr = r.readerAddr
				}
				conns = append(conns, conn{memorystorepb.ConnectionType_CONNECTION_TYPE_READER, addr})
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
			if cn.addr == "" {
				continue
			}
			host, port := splitAddr(cn.addr, "", 0)
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
			byName[n] = c.renderValkey(r)
		}
	}
	c.mu.Unlock()
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	if req.GetFilter() != "" || req.GetOrderBy() != "" {
		// AIP-160 filter and AIP-132 order_by (#1943).
		all := make([]*memorystorepb.Instance, len(names))
		for i, n := range names {
			all[i] = byName[n]
		}
		items, next, err := aipfilter.ListPage("memorystore-instances:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), int(req.GetPageSize()))
		if err != nil {
			return nil, err
		}
		resp.Instances, resp.NextPageToken = items, next
		return resp, nil
	}
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
	return c.renderValkey(r), nil
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
	ca, pool := r.TransitCA, r.ServerCAPool
	c.mu.Unlock()
	if pool != "" {
		// Google: "Memorystore for Valkey can't retrieve these
		// certificates"; they are the CA pool's, from CA Service.
		return nil, apierror.FailedPrecondition("GetCertificateAuthority not supported for customer-managed CAs")
	}
	if ca == "" {
		return nil, apierror.FailedPrecondition("instance %s has no certificate authority: its transit_encryption_mode is TRANSIT_ENCRYPTION_DISABLED", name)
	}
	return &memorystorepb.CertificateAuthority{Name: name,
		ServerCa: &memorystorepb.CertificateAuthority_ManagedServerCa{ManagedServerCa: &memorystorepb.CertificateAuthority_ManagedCertificateAuthority{
			CaCerts: []*memorystorepb.CertificateAuthority_ManagedCertificateAuthority_CertChain{{Certificates: []string{ca}}}}}}, nil
}

// GetSharedRegionalCertificateAuthority returns the CA of a project's and
// region's SERVER_CA_MODE_GOOGLE_MANAGED_SHARED_CA instances (#1918):
// Google's "single CA certificate bundle" per region, "valid for all
// instances located in a region that you configure to use the shared CA",
// made on first use as Google's exists in every region (clusterca.go).
func (s *MemorystoreServer) GetSharedRegionalCertificateAuthority(ctx context.Context, req *memorystorepb.GetSharedRegionalCertificateAuthorityRequest) (*memorystorepb.SharedRegionalCertificateAuthority, error) {
	c, err := s.instances()
	if err != nil {
		return nil, err
	}
	m := sharedCANameRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name must be projects/{project}/locations/{location}/sharedRegionalCertificateAuthority, not %q", req.GetName())
	}
	ca, _, err := c.sharedCA(m[1], m[2])
	if err != nil {
		return nil, err
	}
	return &memorystorepb.SharedRegionalCertificateAuthority{Name: req.GetName(),
		ServerCa: &memorystorepb.SharedRegionalCertificateAuthority_ManagedServerCa{ManagedServerCa: &memorystorepb.SharedRegionalCertificateAuthority_RegionalManagedCertificateAuthority{
			CaCerts: []*memorystorepb.SharedRegionalCertificateAuthority_RegionalManagedCertificateAuthority_RegionalCertChain{{Certificates: []string{ca}}}}}}, nil
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
		// A client's AUTH is its access token, checked against the
		// instance's issued tokens and the project's policy (iamauth.go).
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
	primary, err := cirPrimary(in, mode)
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
	for _, n := range nets {
		if err := s.api.checkPSCNetworks("psc_auto_connections.network", n.Network); err != nil {
			return nil, err
		}
	}
	if err := valkeyZoneDist(in.GetZoneDistributionConfig()).check(m[2]); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/instances/" + id
	now := c.now()
	mp, err := valkeyMaintenancePolicy(in.GetMaintenancePolicy(), time.Time{}, now)
	if err != nil {
		return nil, err
	}
	r := &clusterRecord{Name: name, UID: newUID(), Created: now, ShardCount: shards, ReplicaCount: in.GetReplicaCount(),
		RedisConfigs: copyMap(in.GetEngineConfigs()), DeletionProtect: in.GetDeletionProtectionEnabled(),
		Labels: copyMap(in.GetLabels()), state: clusterpb.Cluster_CREATING, pendingReplicas: -1,
		Valkey: &valkeyFields{Mode: mode.String(), NodeType: nt.String(), EngineVersion: version, AuthMode: auth.String(),
			Updated: now, Persistence: protoJSON(in.GetPersistenceConfig()), Networks: nets, Endpoints: asEndpoints,
			ZoneDistribution: protoJSON(in.GetZoneDistributionConfig()), Maintenance: protoJSON(mp)}}
	if auth == memorystorepb.Instance_TOKEN_AUTH || auth == memorystorepb.Instance_IAM_AUTH {
		// IAM auth's allowed tokens are the default user's passwords
		// beside this one (iamauth.go).
		r.AdminPassword = newUID() + newUID()
	}
	r.AutoBackup = protoJSON(ab)
	// The ACL policy whose users every node gets (#1578, memorystoreacl.go).
	if r.AclPolicy, err = c.attachPolicy(ctx, in.GetAclPolicy(), req.GetParent()); err != nil {
		return nil, err
	}
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
	// server_ca_mode and server_ca_pool (#1918), as a Redis cluster's
	// (clusterca.go): Google documents the same three modes for
	// Memorystore for Valkey.
	caIn := &clusterpb.Cluster{ServerCaPool: in.ServerCaPool}
	if in.ServerCaMode != nil {
		caIn.ServerCaMode = clusterpb.ServerCaMode(in.GetServerCaMode()).Enum()
	}
	if err := c.serverCA(ctx, r, caIn, m[1], m[2], tlsOn, "instance"); err != nil {
		return nil, err
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
	return s.api.launch(op, func() { c.create(name, op) })
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
		case "acl_policy", "aclPolicy":
			set["acl_policy"] = true
		case "rotate_server_certificate", "rotateServerCertificate":
			// Only a customer-managed CA's are rotated on demand
			// (clustercertrotation.go, #1918); false asks for nothing.
			if in.GetRotateServerCertificate() {
				set["rotate_server_certificate"] = true
			}
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
	if _, pending := c.busy[r.Name]; pending {
		return nil, apierror.FailedPrecondition("instance %s has an operation in progress", r.Name)
	}
	if r.state != clusterpb.Cluster_ACTIVE {
		return nil, apierror.FailedPrecondition("instance %s is %s: an instance is updated only when ACTIVE", r.Name, r.state)
	}
	if r.KeyState != "" && (set["persistence_config"] || set["automated_backup_config"]) {
		return nil, errKeyUnavailable(r)
	}
	rotate := set["rotate_server_certificate"]
	if rotate && (r.ServerCAMode != clusterpb.ServerCaMode_SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA.String() || r.TransitCA == "") {
		return nil, errRotateNotCustomer
	}
	if rotate && len(set) > 1 {
		// Not documented, chosen here as for a Redis cluster: the
		// rotation is an update of its own.
		return nil, apierror.InvalidArgument("rotate_server_certificate is updated alone, not with other fields")
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
	syncACL := false
	if set["acl_policy"] && in.GetAclPolicy() != r.AclPolicy {
		m := instanceRE.FindStringSubmatch(r.Name)
		if next.AclPolicy, err = c.attachPolicy(ctx, in.GetAclPolicy(), "projects/"+m[1]+"/locations/"+m[2]); err != nil {
			return nil, err
		}
		syncACL = true
	}
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
	// In CLUSTER mode, a primary's secondaries are scaled and upgraded
	// with it, and a secondary's shard count and engine version are its
	// primary's (crossclusterapi.go).
	var group []*clusterRecord
	if next.ShardCount != r.ShardCount {
		if group, err = c.replicaGroup(r, "shard_count"); err != nil {
			return nil, err
		}
	}
	if set["engine_version"] && in.GetEngineVersion() != vf.EngineVersion {
		if group, err = c.replicaGroup(r, "engine_version"); err != nil {
			return nil, err
		}
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
	if set["zone_distribution_config"] && !r.zoneDist().same(valkeyZoneDist(in.GetZoneDistributionConfig())) {
		// Immutable, as the API documents it (#1908).
		return nil, apierror.InvalidArgument("zone_distribution_config is immutable")
	}
	if set["maintenance_policy"] {
		var created time.Time
		if kept := renderValkeyBase(r).GetMaintenancePolicy(); kept.GetCreateTime() != nil {
			created = kept.GetCreateTime().AsTime()
		}
		mp, err := valkeyMaintenancePolicy(in.GetMaintenancePolicy(), created, c.now())
		if err != nil {
			return nil, err
		}
		vf.Maintenance = protoJSON(mp)
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
	if syncACL && (reshape || upgradeTo != "") {
		return nil, apierror.Unimplemented("acl_policy and the instance's shape or engine_version cannot be changed in one update")
	}
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
	if len(group) > 0 && (upgradeTo != "" || shards != r.ShardCount) {
		return c.launchGroup(s.api, r, op, group, shards, upgradeTo)
	}
	if upgradeTo != "" {
		r.state, r.message = clusterpb.Cluster_UPDATING, "upgrading the engine to "+upgradeTo
		c.busy[r.Name] = op
		return s.api.launch(op, func() { c.upgrade(r.Name, op, upgradeTo) })
	}
	if rotate {
		// On demand, every node (clustercertrotation.go, #1918).
		c.busy[r.Name] = op
		snap := *r
		snap.Nodes = append([]*clusterNode(nil), r.Nodes...)
		return s.api.launch(op, func() {
			ctx, cancel := context.WithTimeout(c.bg, clusterWait)
			defer cancel()
			err := c.rotateCerts(ctx, &snap)
			c.mu.Lock()
			delete(c.busy, snap.Name)
			out := c.renderValkey(r)
			c.mu.Unlock()
			c.finish(op, out, err)
		})
	}
	if syncACL {
		// The policy's users on every node, and the users of the policy
		// it replaced removed (aclpolicy.go), then done.
		c.busy[r.Name] = op
		snap, users := *r, c.aclUsers(r)
		nodes := append([]*clusterNode(nil), r.Nodes...)
		return s.api.launch(op, func() {
			ctx, cancel := context.WithTimeout(c.bg, clusterWait)
			defer cancel()
			err := c.syncUsers(ctx, &snap, users, nodes)
			c.mu.Lock()
			delete(c.busy, snap.Name)
			out := c.renderValkey(r)
			c.mu.Unlock()
			if err != nil {
				err = fmt.Errorf("the nodes' ACL users did not change: %w", err)
			}
			c.finish(op, out, err)
		})
	}
	if !reshape {
		c.finish(op, c.renderValkey(r), nil)
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
	return s.api.launch(op, func() { c.reshape(r.Name, op, shards, replicas) })
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
	if r.cirRole() == memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String() && r.cir().Primary != nil {
		if p := c.byName[r.cir().Primary.Name]; p != nil {
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
	return s.api.launch(op, func() {
		c.crossClusterUnlink(name)
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
	})
}
