package redisadmin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Memorystore for Redis Cluster (#1519), google.cloud.redis.cluster.v1,
// served on redis.googleapis.com beside CloudRedis, as Google serves both.
//
// A cluster is a real Redis Cluster: shard_count primaries, each with
// replica_count replicas, every node a cluster-mode server of the image a
// Memorystore instance runs (Valkey), a Deployment and Service of its own.
// The nodes gossip on their pod addresses; each serves its clients on a
// port that is the same in the cluster and on the host, and announces the
// host's address to clients (cluster-announce-client-ipv4), so a cluster
// client on the host follows CLUSTER SLOTS and MOVED to the right node.
// discovery_endpoints is the first primary's address.
//
// The slots are spread evenly over the primaries at create; a change of
// shard_count adds or removes primaries and moves slots, keys and all, with
// SETSLOT IMPORTING / MIGRATING and MIGRATE, as redis-cli --cluster reshard
// does; a change of replica_count adds or forgets replicas. node_type sizes
// each node's maxmemory, and redis_configs are set on every node. What has
// no local counterpart is kept and returned (psc_configs, zone distribution,
// persistence, maintenance), or refused.
//
// In-transit encryption (SERVER_AUTHENTICATION, #1549) gives the cluster a
// CA of its own (tls.go) and every node a server certificate of it, issued
// each time the node starts: the nodes serve TLS only on their client port,
// gossip (tls-cluster) and replicate (tls-replication) over TLS, and
// GetClusterCertificateAuthority returns the CA as managed_server_ca. With
// AUTH_MODE_IAM_AUTH a client sends an access token as its AUTH string;
// the instance's IAM is not enforced (ADR-0006), so the nodes' default user
// has no password and takes any AUTH, the token CloudBurrow's credentials
// issued among them.

// ClusterRuntime runs the nodes of a cluster.
type ClusterRuntime interface {
	// StartClusterNode runs spec's server in cluster mode, serving on a
	// port that is the same in the cluster and on the host, and returns
	// once it serves: the host's address of it, its pod's IP and the port.
	StartClusterNode(ctx context.Context, spec Spec) (hostAddr, podIP string, port int, err error)
	// Remove stops a node's server and deletes its data.
	Remove(ctx context.Context, server string) error
}

// ClusterNode is a node's server as a cluster client reaches it.
type ClusterNode interface {
	// Do runs a command on the node.
	Do(ctx context.Context, args ...any) (any, error)
	// Pipe runs commands on the node in one round trip, and returns each
	// one's reply, or the first error.
	Pipe(ctx context.Context, cmds [][]any) ([]any, error)
	// Close closes the connection.
	Close() error
}

// ClusterDialer reaches a node's server at addr, over TLS trusting caPEM
// when it is not "".
type ClusterDialer func(addr, password, caPEM string) ClusterNode

// clusterBusPort is the port every node gossips on: fixed, since a client
// port near 65535 has no room for the default of the port plus 10000.
const clusterBusPort = 16379

// totalSlots is Redis Cluster's hash slot count.
const totalSlots = 16384

// nodeSizes are the GB a node of each node_type holds, as Memorystore
// documents.
var nodeSizes = map[clusterpb.NodeType]float64{
	clusterpb.NodeType_REDIS_SHARED_CORE_NANO: 1.4,
	clusterpb.NodeType_REDIS_STANDARD_SMALL:   6.5,
	clusterpb.NodeType_REDIS_HIGHMEM_MEDIUM:   13,
	clusterpb.NodeType_REDIS_HIGHMEM_XLARGE:   58,
}

// clusterNode is one node of a cluster, as it is kept.
type clusterNode struct {
	Server  string `json:"server"`
	Shard   int    `json:"shard"`
	Primary bool   `json:"primary"`
	// Not kept: where the running server is.
	addr  string
	podIP string
	port  int
	id    string
}

// clusterRecord is a cluster, as it is kept.
type clusterRecord struct {
	Name             string            `json:"name"`
	UID              string            `json:"uid"`
	Created          time.Time         `json:"created"`
	ShardCount       int32             `json:"shardCount"`
	ReplicaCount     int32             `json:"replicaCount"`
	NodeType         string            `json:"nodeType"`
	AuthMode         string            `json:"authMode"`
	RedisConfigs     map[string]string `json:"redisConfigs,omitempty"`
	DeletionProtect  bool              `json:"deletionProtection,omitempty"`
	PscConfigs       []string          `json:"pscConfigs,omitempty"`
	Persistence      string            `json:"persistence,omitempty"`
	ZoneDistribution string            `json:"zoneDistribution,omitempty"`
	Maintenance      string            `json:"maintenance,omitempty"`
	Nodes            []*clusterNode    `json:"nodes"`
	NextNode         int               `json:"nextNode"`
	// Labels are the REST Cluster\'s labels, which the Go proto has not
	// (clusterlabels.go).
	Labels map[string]string `json:"labels,omitempty"`
	// Valkey is a Memorystore for Valkey instance's (memorystore.go):
	// nil for a Redis cluster.
	Valkey *valkeyFields `json:"valkey,omitempty"`
	// TransitCA and TransitCAKey, PEM, are the cluster's CA with in-transit
	// encryption (#1549); "" for none.
	TransitCA    string `json:"transitCa,omitempty"`
	TransitCAKey string `json:"transitCaKey,omitempty"`
	// AdminPassword is the nodes' default user's password with token auth
	// (clusterauth.go): only the admin plane's; "" for none.
	AdminPassword string `json:"adminPassword,omitempty"`
	// TokenUsers are the token auth users, by username.
	TokenUsers map[string]*tokenUser `json:"tokenUsers,omitempty"`
	// AclPolicy is the attached ACL policy's name (aclpolicy.go), "" for none.
	AclPolicy string `json:"aclPolicy,omitempty"`
	// AutoBackup is the automated_backup_config, protojson; LastAutoBackup
	// the time of the last automated backup (backups.go).
	AutoBackup     string    `json:"autoBackup,omitempty"`
	LastAutoBackup time.Time `json:"lastAutoBackup,omitempty"`
	// Source is the backup or gs:// files the cluster was created from.
	Source     string   `json:"source,omitempty"`
	SourceURIs []string `json:"sourceUris,omitempty"`
	// CMEK (#1557, clustercmek.go): KmsKey is kms_key, WrappedKey the data
	// key it wrapped, Sealed the secrets sealed with the data key, and
	// SuspendedData a suspended cluster's data, each primary's RDB file
	// sealed with it.
	KmsKey        string   `json:"kmsKey,omitempty"`
	WrappedKey    []byte   `json:"wrappedKey,omitempty"`
	Sealed        []byte   `json:"sealed,omitempty"`
	SuspendedData [][]byte `json:"suspendedData,omitempty"`
	dek           []byte
	suspended     bool
	// KeyState is the key's kms_key_primary_state when it cannot be used,
	// KeyChecked when it was last checked.
	KeyState   string    `json:"-"`
	KeyChecked time.Time `json:"-"`
	// DataGen is the formation the nodes' data directories are of
	// (restart.go): 1 at create, one more each time CloudBurrow forms the
	// nodes again after a restart; 0 for a record kept before formations.
	DataGen int `json:"dataGen,omitempty"`
	// restore is the RDB files a create loads once the nodes serve.
	restore         [][]byte
	state           clusterpb.Cluster_State
	message         string
	pendingShards   int32
	pendingReplicas int32
}

// clusters are the clusters CreateCluster made.
type clusters struct {
	// valkey is Memorystore for Valkey's instances (memorystore.go),
	// false for Redis clusters.
	valkey bool
	rt     ClusterRuntime
	dial   ClusterDialer
	db     store.Store
	api    *Server
	now    func() time.Time
	bg     context.Context

	mu     sync.Mutex
	byName map[string]*clusterRecord
	busy   map[string]string
	// backups, collections and files (without a store) are the backups
	// (backups.go), read once.
	backups     map[string]*backupRecord
	collections map[string]*collectionRecord
	files       map[string][]byte
	// policies are the ACL policies (aclpolicy.go), read once.
	aclOnce  sync.Once
	policies *aclPolicies
}

// ClusterServer implements clusterpb.CloudRedisClusterServer.
type ClusterServer struct {
	clusterpb.UnimplementedCloudRedisClusterServer
	api *Server
}

// NewClusterServer returns the Redis Cluster API of api, whose clusters
// EnableClusters enables.
func NewClusterServer(api *Server) *ClusterServer { return &ClusterServer{api: api} }

// Register adds the service to a gRPC server or to the JSON transcoder.
func (s *ClusterServer) Register(g grpc.ServiceRegistrar) {
	clusterpb.RegisterCloudRedisClusterServer(g, s)
}

// EnableClusters makes CreateCluster and the rest work over rt, keeping the
// clusters in db, their operations beside the instances' (EnableCreate
// first). ctx bounds the nodes' lives.
func (s *Server) EnableClusters(ctx context.Context, rt ClusterRuntime, dial ClusterDialer, db store.Store) {
	if s.created == nil {
		panic("redisadmin: EnableClusters before EnableCreate")
	}
	s.clusters = &clusters{rt: rt, dial: dial, db: db, api: s, now: time.Now, bg: ctx,
		byName: map[string]*clusterRecord{}, busy: map[string]string{}}
	go s.clusters.autoBackups(ctx)
}

const clusterPrefix = "clusters/"

// prefix is where c's records are kept.
func (c *clusters) prefix() string {
	if c.valkey {
		return valkeyPrefix
	}
	return clusterPrefix
}

// output is r as its API returns it. Call with mu held.
func (c *clusters) output(r *clusterRecord) proto.Message {
	if c.valkey {
		return renderValkey(r)
	}
	return c.render(r)
}

var clusterRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/clusters/([^/]+)$`)

// nodesOwnerRE matches a Redis cluster's or a Valkey instance's name.
var nodesOwnerRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/(?:clusters|instances)/([^/]+)$`)

// clusterIDRE is Google's rule for a cluster ID: 4 to 63 lowercase
// letters, digits and hyphens, starting with a letter, not ending with a
// hyphen.
var clusterIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{2,61}[a-z0-9]$`)

func (c *clusters) save(r *clusterRecord) error {
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
	return c.db.Put(c.prefix()+clusterKey(r.Name), b)
}

func clusterKey(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:8])
}

// clusterServerName is node n's server: "redis-" and the cluster's ID (cut
// to fit), a hash of its whole name, and "-c" and n.
func clusterServerName(name string, n int) string {
	id := nodesOwnerRE.FindStringSubmatch(name)[3]
	if len(id) > 36 {
		id = id[:36]
	}
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%sc%s-%s-%d", ServerPrefix, trimHyphen(id), hex.EncodeToString(sum[:4]), n)
}

func trimHyphen(s string) string {
	for len(s) > 0 && s[len(s)-1] == '-' {
		s = s[:len(s)-1]
	}
	return s
}

// ClusterServers are the servers of every cluster's nodes, which a caller
// tidying the cluster must keep.
func (s *Server) ClusterServers() []string {
	var out []string
	for _, c := range []*clusters{s.clusters, s.valkey} {
		if c == nil {
			continue
		}
		c.mu.Lock()
		for _, r := range c.byName {
			for _, n := range r.Nodes {
				out = append(out, n.Server)
			}
		}
		c.mu.Unlock()
	}
	sort.Strings(out)
	return out
}

// RestoreClusters reads the kept clusters and forms each again, in the
// background: its nodes start empty, as a cluster's do after a restart
// without persistence.
func (s *Server) RestoreClusters() error {
	if err := s.clusters.restore(); err != nil {
		return err
	}
	return s.valkey.restore()
}

func (c *clusters) restore() error {
	if c == nil || c.db == nil {
		return nil
	}
	keys, err := c.db.List(c.prefix())
	if err != nil {
		return err
	}
	for _, k := range keys {
		b, err := c.db.Get(k)
		if err != nil {
			return err
		}
		r := &clusterRecord{}
		if err := json.Unmarshal(b, r); err != nil {
			return fmt.Errorf("Redis cluster %s: %w", k, err)
		}
		r.state, r.message = clusterpb.Cluster_CREATING, "CloudBurrow is forming the cluster again"
		r.pendingReplicas = -1
		c.mu.Lock()
		c.byName[r.Name] = r
		c.mu.Unlock()
		go c.create(r.Name, "")
	}
	return nil
}

func (s *ClusterServer) clusters() (*clusters, error) {
	if s.api.clusters == nil {
		return nil, apierror.Unimplemented("Memorystore for Redis Cluster is not served by this instance")
	}
	return s.api.clusters, nil
}

func (c *clusters) newOperation(target, verb string) string {
	m := nodesOwnerRE.FindStringSubmatch(target)
	ops := c.api.created.ops
	op := ops.Create("projects/"+m[1]+"/locations/"+m[2], target)
	if c.valkey {
		_ = ops.SetMetadata(op.Name, &memorystorepb.OperationMetadata{CreateTime: timestamppb.New(c.now()),
			Target: target, Verb: verb, ApiVersion: "v1"})
		return op.Name
	}
	_ = ops.SetMetadata(op.Name, &clusterpb.OperationMetadata{CreateTime: timestamppb.New(c.now()),
		Target: target, Verb: verb, ApiVersion: "v1"})
	return op.Name
}

func (c *clusters) finish(op string, response proto.Message, err error) {
	ops := c.api.created.ops
	if got, gerr := ops.Get(op); gerr == nil {
		if md, ok := got.Metadata.(*clusterpb.OperationMetadata); ok {
			md = proto.Clone(md).(*clusterpb.OperationMetadata)
			md.EndTime = timestamppb.New(c.now())
			_ = ops.SetMetadata(op, md)
		}
		if md, ok := got.Metadata.(*memorystorepb.OperationMetadata); ok {
			md = proto.Clone(md).(*memorystorepb.OperationMetadata)
			md.EndTime = timestamppb.New(c.now())
			_ = ops.SetMetadata(op, md)
		}
	}
	if err != nil {
		_ = ops.Fail(op, &apierror.Error{Code: codes.Internal, Message: err.Error(), Cause: err})
		return
	}
	_ = ops.Succeed(op, response)
}

// ListClusters lists a project's clusters in one region, or in every
// region ("locations/-"), in name order and paged.
func (s *ClusterServer) ListClusters(ctx context.Context, req *clusterpb.ListClustersRequest) (*clusterpb.ListClustersResponse, error) {
	m := parentRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent must be projects/{project}/locations/{location}, not %q", req.GetParent())
	}
	if req.GetPageSize() < 0 {
		return nil, apierror.InvalidArgument("page_size must not be negative")
	}
	resp := &clusterpb.ListClustersResponse{}
	c := s.api.clusters
	if c == nil {
		return resp, nil
	}
	c.mu.Lock()
	byName := map[string]*clusterpb.Cluster{}
	for n, r := range c.byName {
		k := clusterRE.FindStringSubmatch(n)
		if k[1] == m[1] && (m[2] == "-" || k[2] == m[2]) {
			byName[n] = c.render(r)
		}
	}
	c.mu.Unlock()
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	page, next, err := paging.Page("redis-clusters:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token %q", req.GetPageToken())
	}
	resp.NextPageToken = next
	for _, n := range page {
		resp.Clusters = append(resp.Clusters, byName[n])
	}
	return resp, nil
}

// GetCluster describes a cluster.
func (s *ClusterServer) GetCluster(ctx context.Context, req *clusterpb.GetClusterRequest) (*clusterpb.Cluster, error) {
	c, r, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	return c.render(r), nil
}

// find returns the cluster name, with c.mu held when err is nil.
func (s *ClusterServer) find(name string) (*clusters, *clusterRecord, error) {
	if clusterRE.FindStringSubmatch(name) == nil {
		return nil, nil, apierror.InvalidArgument("name must be projects/{project}/locations/{location}/clusters/{cluster}, not %q", name)
	}
	c, err := s.clusters()
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

// GetClusterCertificateAuthority returns a cluster's CA, as
// managed_server_ca, which its clients trust; a cluster without in-transit
// encryption has none.
func (s *ClusterServer) GetClusterCertificateAuthority(ctx context.Context, req *clusterpb.GetClusterCertificateAuthorityRequest) (*clusterpb.CertificateAuthority, error) {
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
		return nil, apierror.FailedPrecondition("cluster %s has no certificate authority: its transit_encryption_mode is TRANSIT_ENCRYPTION_MODE_DISABLED", name)
	}
	return &clusterpb.CertificateAuthority{Name: name + "/certificateAuthority",
		ServerCa: &clusterpb.CertificateAuthority_ManagedServerCa{ManagedServerCa: &clusterpb.CertificateAuthority_ManagedCertificateAuthority{
			CaCerts: []*clusterpb.CertificateAuthority_ManagedCertificateAuthority_CertChain{{Certificates: []string{ca}}}}}}, nil
}

// render is r as Google shapes a cluster. Call with mu held.
func (c *clusters) render(r *clusterRecord) *clusterpb.Cluster {
	nt := clusterpb.NodeType(clusterpb.NodeType_value[r.NodeType])
	size := math.Round(nodeSizes[nt]*float64(r.ShardCount)*10) / 10
	sizeGB := int32(math.Ceil(size))
	out := &clusterpb.Cluster{
		Name:                      r.Name,
		Uid:                       r.UID,
		CreateTime:                timestamppb.New(r.Created),
		State:                     r.state,
		ShardCount:                proto.Int32(r.ShardCount),
		ReplicaCount:              proto.Int32(r.ReplicaCount),
		NodeType:                  nt,
		SizeGb:                    proto.Int32(sizeGB),
		PreciseSizeGb:             proto.Float64(size),
		AuthorizationMode:         clusterpb.AuthorizationMode(clusterpb.AuthorizationMode_value[r.AuthMode]),
		TransitEncryptionMode:     clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_DISABLED,
		RedisConfigs:              copyMap(r.RedisConfigs),
		DeletionProtectionEnabled: proto.Bool(r.DeletionProtect),
		ZoneDistributionConfig:    &clusterpb.ZoneDistributionConfig{Mode: clusterpb.ZoneDistributionConfig_MULTI_ZONE},
		PersistenceConfig:         &clusterpb.ClusterPersistenceConfig{Mode: clusterpb.ClusterPersistenceConfig_DISABLED},
	}
	if r.TransitCA != "" {
		out.TransitEncryptionMode = clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION
	}
	if r.KmsKey != "" {
		out.KmsKey = proto.String(r.KmsKey)
		out.EncryptionInfo = r.encryptionInfo()
	}
	if r.AutoBackup != "" {
		ab := &clusterpb.AutomatedBackupConfig{}
		if protojson.Unmarshal([]byte(r.AutoBackup), ab) == nil {
			out.AutomatedBackupConfig = ab
		}
	}
	if col := collectionName(r); col != "" && r.state != clusterpb.Cluster_CREATING {
		out.BackupCollection = proto.String(col)
	}
	switch {
	case r.Source != "":
		out.ImportSources = &clusterpb.Cluster_ManagedBackupSource_{ManagedBackupSource: &clusterpb.Cluster_ManagedBackupSource{Backup: r.Source}}
	case len(r.SourceURIs) > 0:
		out.ImportSources = &clusterpb.Cluster_GcsSource{GcsSource: &clusterpb.Cluster_GcsBackupSource{Uris: r.SourceURIs}}
	}
	if r.AclPolicy != "" {
		out.AclPolicy = r.AclPolicy
		out.AclPolicyInfo = c.policyInfo(r)
	}
	if r.ZoneDistribution != "" {
		z := &clusterpb.ZoneDistributionConfig{}
		if json.Unmarshal([]byte(r.ZoneDistribution), z) == nil {
			out.ZoneDistributionConfig = z
		}
	}
	if r.Persistence != "" {
		p := &clusterpb.ClusterPersistenceConfig{}
		if json.Unmarshal([]byte(r.Persistence), p) == nil {
			out.PersistenceConfig = p
		}
	}
	if r.Maintenance != "" {
		mp := &clusterpb.ClusterMaintenancePolicy{}
		if json.Unmarshal([]byte(r.Maintenance), mp) == nil {
			out.MaintenancePolicy = mp
		}
	}
	if r.state != clusterpb.Cluster_ACTIVE && r.message != "" {
		out.StateInfo = &clusterpb.Cluster_StateInfo{Info: &clusterpb.Cluster_StateInfo_UpdateInfo_{
			UpdateInfo: &clusterpb.Cluster_StateInfo_UpdateInfo{}}}
		if r.pendingShards > 0 {
			out.StateInfo.GetUpdateInfo().TargetShardCount = proto.Int32(r.pendingShards)
		}
		if r.pendingReplicas >= 0 && r.state == clusterpb.Cluster_UPDATING {
			out.StateInfo.GetUpdateInfo().TargetReplicaCount = proto.Int32(r.pendingReplicas)
		}
	}
	m := clusterRE.FindStringSubmatch(r.Name)
	for i, net := range r.PscConfigs {
		out.PscConfigs = append(out.PscConfigs, &clusterpb.PscConfig{Network: net})
		if first := r.firstPrimary(); first != nil && first.addr != "" {
			host, port := splitAddr(first.addr, "", 0)
			out.PscConnections = append(out.PscConnections, &clusterpb.PscConnection{
				PscConnectionId:     fmt.Sprintf("%s-psc-%d", r.UID[:8], i),
				Address:             host,
				ForwardingRule:      fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/regions/%s/forwardingRules/%s-psc-%d", m[1], m[2], m[3], i),
				ProjectId:           m[1],
				Network:             net,
				PscConnectionStatus: clusterpb.PscConnectionStatus_PSC_CONNECTION_STATUS_ACTIVE,
				ConnectionType:      clusterpb.ConnectionType_CONNECTION_TYPE_DISCOVERY,
			})
			_ = port
		}
	}
	if first := r.firstPrimary(); first != nil && first.addr != "" && r.state != clusterpb.Cluster_CREATING {
		host, port := splitAddr(first.addr, "", 0)
		ep := &clusterpb.DiscoveryEndpoint{Address: host, Port: port}
		if len(r.PscConfigs) > 0 {
			ep.PscConfig = &clusterpb.PscConfig{Network: r.PscConfigs[0]}
		}
		out.DiscoveryEndpoints = []*clusterpb.DiscoveryEndpoint{ep}
	}
	return out
}

func (r *clusterRecord) firstPrimary() *clusterNode {
	for _, n := range r.Nodes {
		if n.Primary && n.Shard == 0 {
			return n
		}
	}
	for _, n := range r.Nodes {
		if n.Primary {
			return n
		}
	}
	return nil
}

// password is the nodes' default user's: "" but with token auth.
func (r *clusterRecord) password() string { return r.AdminPassword }

// standalone is whether r's nodes run without cluster mode: a Valkey
// instance in CLUSTER_DISABLED mode, a primary and its replicas.
func (r *clusterRecord) standalone() bool {
	return r.Valkey != nil && r.Valkey.Mode == "CLUSTER_DISABLED"
}

// maxMemory is a node's maxmemory, in bytes.
func (r *clusterRecord) maxMemory() int64 {
	if r.Valkey != nil {
		return int64(r.Valkey.nodeGB() * (1 << 30))
	}
	gb := nodeSizes[clusterpb.NodeType(clusterpb.NodeType_value[r.NodeType])]
	return int64(gb * (1 << 30))
}

func (r *clusterRecord) spec(server string) Spec {
	cfg := serverConfigs(r.RedisConfigs)
	tls := func(s Spec) Spec {
		s.TransitCA, s.TransitCAKey, s.TransitTrust = r.TransitCA, r.TransitCAKey, r.TransitCA
		if r.AdminPassword != "" {
			// The default user's password; the replicas sync with it.
			s.Password = r.AdminPassword
			s.Configs["masterauth"] = r.AdminPassword
		}
		return s
	}
	// The persistence_config's settings, on the formation's directory
	// (restart.go).
	for _, kv := range r.persistenceSettings() {
		cfg[kv[0]] = kv[1]
	}
	if r.standalone() {
		return tls(Spec{Server: server, MaxMemory: r.maxMemory(), Configs: cfg, Version: r.engineVersion(), Dir: dataDir(r.DataGen)})
	}
	if r.TransitCA != "" {
		// The cluster bus, and MIGRATE between nodes, over TLS too.
		cfg["tls-cluster"] = "yes"
	}
	cfg["cluster-enabled"] = "yes"
	cfg["cluster-port"] = strconv.Itoa(clusterBusPort)
	cfg["cluster-config-file"] = "/tmp/nodes.conf"
	cfg["cluster-node-timeout"] = "5000"
	cfg["cluster-require-full-coverage"] = "yes"
	return tls(Spec{Server: server, MaxMemory: r.maxMemory(), Configs: cfg, Version: r.engineVersion(), Dir: dataDir(r.DataGen)})
}

// engineVersion is the Valkey version r's nodes run (#1557): a Valkey
// instance's engine_version, VALKEY_8_0 for a Redis cluster.
func (r *clusterRecord) engineVersion() string {
	if r.Valkey != nil && r.Valkey.EngineVersion != "" {
		return r.Valkey.EngineVersion
	}
	return DefaultValkeyVersion
}

// clusterConfigs are the redis_configs a cluster takes, as Memorystore
// documents them for Redis Cluster.
var clusterConfigs = map[string]bool{
	"maxmemory-policy": true, "maxmemory-clients": true, "notify-keyspace-events": true,
	"slowlog-log-slower-than": true, "maxclients": true, "lfu-log-factor": true, "lfu-decay-time": true,
	"maxmemory-samples": true,
}

func checkClusterConfigs(cfg map[string]string) error {
	for k := range cfg {
		if !clusterConfigs[k] {
			return apierror.InvalidArgument("redis_configs %q is not a configuration Memorystore for Redis Cluster supports", k)
		}
	}
	return nil
}

func checkShape(shards, replicas int32) error {
	if shards < 1 || shards > 250 {
		return apierror.InvalidArgument("shard_count must be between 1 and 250, not %d", shards)
	}
	if replicas < 0 || replicas > 5 {
		return apierror.InvalidArgument("replica_count must be between 0 and 5, not %d", replicas)
	}
	return nil
}

// CreateCluster forms a Redis Cluster of the shape asked and returns the
// operation that completes once every slot is served.
func (s *ClusterServer) CreateCluster(ctx context.Context, req *clusterpb.CreateClusterRequest) (*longrunningpb.Operation, error) {
	c, err := s.clusters()
	if err != nil {
		return nil, err
	}
	m := parentRE.FindStringSubmatch(req.GetParent())
	if m == nil || m[2] == "-" {
		return nil, apierror.InvalidArgument("parent must be projects/{project}/locations/{location}, not %q", req.GetParent())
	}
	id := req.GetClusterId()
	if !clusterIDRE.MatchString(id) {
		return nil, apierror.InvalidArgument("cluster_id %q must be 4 to 63 lowercase letters, digits or hyphens, start with a letter and not end with a hyphen", id)
	}
	in := req.GetCluster()
	if in == nil {
		return nil, apierror.InvalidArgument("cluster is required")
	}
	if in.ShardCount == nil {
		return nil, apierror.InvalidArgument("cluster.shard_count is required")
	}
	if err := checkShape(in.GetShardCount(), in.GetReplicaCount()); err != nil {
		return nil, err
	}
	nt := in.GetNodeType()
	if nt == clusterpb.NodeType_NODE_TYPE_UNSPECIFIED {
		nt = clusterpb.NodeType_REDIS_HIGHMEM_MEDIUM
	}
	if _, ok := nodeSizes[nt]; !ok {
		return nil, apierror.InvalidArgument("node_type %s is not a Memorystore for Redis Cluster node type", nt)
	}
	auth := in.GetAuthorizationMode()
	switch auth {
	case clusterpb.AuthorizationMode_AUTH_MODE_UNSPECIFIED:
		auth = clusterpb.AuthorizationMode_AUTH_MODE_DISABLED
	case clusterpb.AuthorizationMode_AUTH_MODE_DISABLED:
	case clusterpb.AuthorizationMode_AUTH_MODE_IAM_AUTH:
		// A client's AUTH is its access token. IAM is not enforced here
		// (ADR-0006), so the nodes take any AUTH string (docs/memorystore.md).
	case clusterpb.AuthorizationMode_AUTH_MODE_TOKEN_AUTH:
		// Token auth users and their tokens (clusterauth.go).
	default:
		return nil, apierror.InvalidArgument("authorization_mode %s is not an authorization mode", auth)
	}
	tlsOn := false
	switch tm := in.GetTransitEncryptionMode(); tm {
	case clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_UNSPECIFIED, clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_DISABLED:
	case clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION:
		tlsOn = true
	default:
		return nil, apierror.InvalidArgument("transit_encryption_mode %s is not a transit encryption mode", tm)
	}
	if err := checkCrossCluster(in.GetCrossClusterReplicationConfig() != nil); err != nil {
		return nil, err
	}
	if err := checkAutoBackup(in.GetAutomatedBackupConfig()); err != nil {
		return nil, err
	}
	if err := checkClusterConfigs(in.GetRedisConfigs()); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/clusters/" + id
	r := &clusterRecord{Name: name, UID: newUID(), Created: c.now(), ShardCount: in.GetShardCount(), ReplicaCount: in.GetReplicaCount(),
		NodeType: nt.String(), AuthMode: auth.String(), RedisConfigs: copyMap(in.GetRedisConfigs()),
		DeletionProtect: in.GetDeletionProtectionEnabled(), state: clusterpb.Cluster_CREATING, pendingReplicas: -1}
	if auth == clusterpb.AuthorizationMode_AUTH_MODE_TOKEN_AUTH {
		r.AdminPassword = newUID() + newUID()
	}
	r.AutoBackup = protoJSON(in.GetAutomatedBackupConfig())
	if key := in.GetKmsKey(); key != "" {
		if err := c.newClusterKey(ctx, r, key, m[2]); err != nil {
			return nil, err
		}
	}
	if r.AclPolicy, err = c.attachPolicy(ctx, in.GetAclPolicy(), req.GetParent()); err != nil {
		return nil, err
	}
	switch src := in.GetImportSources().(type) {
	case *clusterpb.Cluster_GcsSource:
		r.SourceURIs = src.GcsSource.GetUris()
		r.restore, err = c.readGCS(ctx, r.SourceURIs, "cluster.gcs_source.uris")
	case *clusterpb.Cluster_ManagedBackupSource_:
		r.Source = src.ManagedBackupSource.GetBackup()
		r.restore, err = c.readBackup(r.Source, "cluster.managed_backup_source.backup")
	}
	if err != nil {
		return nil, err
	}
	if tlsOn {
		if r.TransitCA, r.TransitCAKey, err = newCA(id, c.now()); err != nil {
			return nil, apierror.Internal(err, "create the CA of cluster %s", name)
		}
	}
	for _, p := range in.GetPscConfigs() {
		r.PscConfigs = append(r.PscConfigs, p.GetNetwork())
	}
	keepJSON(&r.Persistence, in.GetPersistenceConfig())
	keepJSON(&r.ZoneDistribution, in.GetZoneDistributionConfig())
	keepJSON(&r.Maintenance, in.GetMaintenancePolicy())
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
	if err := c.save(r); err != nil {
		c.mu.Unlock()
		return nil, apierror.Internal(err, "keep cluster %s", name)
	}
	c.byName[name] = r
	op := c.newOperation(name, "create")
	c.busy[name] = op
	c.mu.Unlock()
	go c.create(name, op)
	return s.api.operation(op)
}

func keepJSON(dst *string, m proto.Message) {
	if m == nil || !m.ProtoReflect().IsValid() {
		return
	}
	if b, err := json.Marshal(m); err == nil {
		*dst = string(b)
	}
}

func newUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// clusterWait bounds forming or changing a cluster.
var clusterWait = 6 * time.Minute

// create starts every node and forms the cluster; op, when not "", is
// completed with it.
func (c *clusters) create(name, op string) {
	ctx, cancel := context.WithTimeout(c.bg, clusterWait)
	defer cancel()
	c.mu.Lock()
	r := c.byName[name]
	key, wrapped, dek := r.KmsKey, r.WrappedKey, r.dek
	c.mu.Unlock()
	if key != "" && dek == nil {
		// Formed again after a restart: its secrets are unsealed with the
		// data key, or it stays suspended until the key can be used.
		dek, err := c.unwrapKey(ctx, key, wrapped)
		c.mu.Lock()
		if err == nil {
			err = r.unsealWith(dek)
		}
		if err != nil {
			r.suspended, r.KeyState = true, keyState(err)
			r.state, r.message = clusterpb.Cluster_STATE_UNSPECIFIED, "suspended: its kms_key "+key+" cannot be used: "+err.Error()
			delete(c.busy, name)
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
	}
	c.mu.Lock()
	// Formed again after a restart: the nodes start empty in the next
	// formation's directory, and with persistence the previous one's
	// data is loaded into them (restart.go).
	prev, mode := "", ""
	if op == "" {
		prev, mode = dataDir(r.DataGen), r.persistenceMode()
		r.DataGen++
	} else if r.DataGen == 0 {
		r.DataGen = 1
	}
	_ = c.save(r)
	nodes := append([]*clusterNode(nil), r.Nodes...)
	snap := *r
	c.mu.Unlock()
	err := c.startNodes(ctx, &snap, nodes)
	if err == nil {
		err = c.form(ctx, &snap, nodes)
	}
	if err == nil && prev != "" && mode != "" {
		if err = c.reloadData(ctx, &snap, prev, mode); err != nil {
			err = fmt.Errorf("load the data kept before the restart: %w", err)
		}
	}
	if err == nil && prev != "" {
		c.dropDir(ctx, nodes, prev)
	}
	if err == nil && op != "" && len(snap.restore) > 0 {
		// Created from a backup or gs:// RDB files (backups.go).
		if err = c.load(ctx, &snap, snap.restore); err != nil {
			err = fmt.Errorf("restore the data: %w", err)
		}
	}
	if err == nil {
		// A secondary of cross-instance replication (crossinstance.go).
		err = c.linkSecondary(ctx, name)
	}
	c.mu.Lock()
	r.restore = nil
	if err == nil && op != "" {
		c.addCollection(r)
	}
	delete(c.busy, name)
	if err != nil {
		if op != "" {
			delete(c.byName, name)
			if c.db != nil {
				_ = c.db.Delete(c.prefix() + clusterKey(name))
			}
			go c.removeNodes(nodes)
		} else {
			r.state, r.message = clusterpb.Cluster_STATE_UNSPECIFIED, "the cluster did not form: "+err.Error()
		}
	} else {
		r.state, r.message = clusterpb.Cluster_ACTIVE, ""
	}
	var out proto.Message
	if err == nil {
		out = c.output(r)
	}
	c.mu.Unlock()
	if op != "" {
		if err != nil {
			err = fmt.Errorf("the cluster did not form: %w", err)
		}
		c.finish(op, out, err)
	}
}

// startNodes runs nodes' servers, all at once, and fills in where they are.
func (c *clusters) startNodes(ctx context.Context, r *clusterRecord, nodes []*clusterNode) error {
	errs := make([]error, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			addr, ip, port, err := c.rt.StartClusterNode(ctx, r.spec(n.Server))
			if err != nil {
				errs[i] = fmt.Errorf("node %s: %w", n.Server, err)
				return
			}
			c.mu.Lock()
			n.addr, n.podIP, n.port = addr, ip, port
			c.mu.Unlock()
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	{
		// Persistence (restart.go) is set on the running server too, as an
		// update sets it.
		for _, n := range nodes {
			for _, kv := range r.persistenceSettings() {
				if err := c.do(ctx, r, n, "CONFIG", "SET", kv[0], kv[1]); err != nil {
					return err
				}
			}
		}
	}
	c.mu.Lock()
	users := c.aclUsers(r)
	c.mu.Unlock()
	if len(users) > 0 {
		// Token auth users and ACL policy users, on every node (clusterauth.go).
		if err := c.syncUsers(ctx, r, users, nodes); err != nil {
			return err
		}
	}
	if r.standalone() {
		return nil
	}
	for _, n := range nodes {
		cl := c.dial(n.addr, r.password(), r.TransitCA)
		host, _ := splitAddr(n.addr, "", 0)
		_, err := cl.Do(ctx, "CONFIG", "SET", "cluster-announce-client-ipv4", host)
		if err != nil && strings.Contains(err.Error(), "nknown option") {
			// Valkey 7.2 (#1557) has no cluster-announce-client-ipv4:
			// clients are given the host's address as the node's hostname.
			_, err = cl.Do(ctx, "CONFIG", "SET", "cluster-announce-hostname", host)
			if err == nil {
				_, err = cl.Do(ctx, "CONFIG", "SET", "cluster-preferred-endpoint-type", "hostname")
			}
		}
		if err == nil {
			var id any
			id, err = cl.Do(ctx, "CLUSTER", "MYID")
			c.mu.Lock()
			n.id = fmt.Sprint(id)
			c.mu.Unlock()
		}
		cl.Close()
		if err != nil {
			return fmt.Errorf("node %s: %w", n.Server, err)
		}
	}
	return nil
}

// slotRanges spreads the slots over shards primaries evenly.
func slotRanges(shards int) [][2]int {
	out := make([][2]int, shards)
	lo := 0
	for i := 0; i < shards; i++ {
		hi := (i+1)*totalSlots/shards - 1
		out[i] = [2]int{lo, hi}
		lo = hi + 1
	}
	return out
}

// form joins nodes, all new, into one cluster: slots to the primaries,
// MEET from the first, replicas to their shard's primary.
func (c *clusters) form(ctx context.Context, r *clusterRecord, nodes []*clusterNode) error {
	if r.standalone() {
		if err := c.replicate(ctx, r, nodes, nodes); err != nil {
			return err
		}
		return c.settle(ctx, r, nodes)
	}
	ranges := slotRanges(int(r.ShardCount))
	for _, n := range nodes {
		if n.Primary {
			rg := ranges[n.Shard]
			if err := c.do(ctx, r, n, "CLUSTER", "ADDSLOTSRANGE", rg[0], rg[1]); err != nil {
				return err
			}
		}
	}
	if err := c.meet(ctx, r, nodes[0], nodes[1:]); err != nil {
		return err
	}
	if err := c.replicate(ctx, r, nodes, nodes); err != nil {
		return err
	}
	return c.settle(ctx, r, nodes)
}

func (c *clusters) do(ctx context.Context, r *clusterRecord, n *clusterNode, args ...any) error {
	cl := c.dial(n.addr, r.password(), r.TransitCA)
	defer cl.Close()
	if _, err := cl.Do(ctx, args...); err != nil {
		return fmt.Errorf("%v on node %s: %w", args, n.Server, err)
	}
	return nil
}

// meet introduces every node of others to via, then waits until every one
// knows them all by their IDs. cluster_known_nodes alone is not enough: it
// counts a node still in its handshake, under a random ID, so a CLUSTER
// REPLICATE sent then fails "ERR Unknown node" (#1568).
func (c *clusters) meet(ctx context.Context, r *clusterRecord, via *clusterNode, others []*clusterNode) error {
	for _, n := range others {
		if err := c.do(ctx, r, via, "CLUSTER", "MEET", n.podIP, n.port, clusterBusPort); err != nil {
			return err
		}
	}
	ids := make([]string, 0, len(r.Nodes))
	for _, n := range r.Nodes {
		if n.id != "" {
			ids = append(ids, n.id)
		}
	}
	return c.until(ctx, "every node to know the others", func() bool {
		want := len(r.Nodes)
		for _, n := range r.Nodes {
			cl := c.dial(n.addr, r.password(), r.TransitCA)
			got := known(ctx, cl, "cluster_known_nodes")
			ok := got == strconv.Itoa(want)
			if ok {
				v, err := cl.Do(ctx, "CLUSTER", "NODES")
				ok = err == nil && knowsAll(fmt.Sprint(v), ids)
			}
			cl.Close()
			if !ok {
				return false
			}
		}
		return true
	})
}

// knowsAll reports whether a CLUSTER NODES reply lists every one of ids
// with its handshake done: a line of its own whose flags name neither
// handshake nor noaddr.
func knowsAll(nodes string, ids []string) bool {
	done := map[string]bool{}
	for _, line := range strings.Split(nodes, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		flags := "," + f[2] + ","
		if !strings.Contains(flags, ",handshake,") && !strings.Contains(flags, ",noaddr,") {
			done[f[0]] = true
		}
	}
	for _, id := range ids {
		if !done[id] {
			return false
		}
	}
	return true
}

// replicate makes each replica of these a replica of its shard's primary,
// found among all.
func (c *clusters) replicate(ctx context.Context, r *clusterRecord, these, all []*clusterNode) error {
	primary := map[int]*clusterNode{}
	for _, n := range all {
		if n.Primary {
			primary[n.Shard] = n
		}
	}
	for _, n := range these {
		if n.Primary {
			continue
		}
		p := primary[n.Shard]
		if p == nil {
			return fmt.Errorf("shard %d has no primary", n.Shard)
		}
		if r.standalone() {
			if err := c.do(ctx, r, n, "REPLICAOF", p.podIP, p.port); err != nil {
				return err
			}
			continue
		}
		if err := c.do(ctx, r, n, "CLUSTER", "REPLICATE", p.id); err != nil {
			return err
		}
	}
	return nil
}

// settle waits until every node says the cluster is ok, and every replica
// is linked to its primary.
func (c *clusters) settle(ctx context.Context, r *clusterRecord, nodes []*clusterNode) error {
	return c.until(ctx, "cluster_state:ok on every node", func() bool {
		for _, n := range r.Nodes {
			cl := c.dial(n.addr, r.password(), r.TransitCA)
			ok := r.standalone() || known(ctx, cl, "cluster_state") == "ok"
			if ok && !n.Primary {
				info, err := cl.Do(ctx, "INFO", "replication")
				ok = err == nil && clusterInfoField(fmt.Sprint(info), "master_link_status") == "up"
			}
			cl.Close()
			if !ok {
				return false
			}
		}
		return true
	})
}

// known is a CLUSTER INFO field.
func known(ctx context.Context, cl ClusterNode, field string) string {
	v, err := cl.Do(ctx, "CLUSTER", "INFO")
	if err != nil {
		return ""
	}
	return clusterInfoField(fmt.Sprint(v), field)
}

func clusterInfoField(info, field string) string {
	for _, line := range strings.Split(info, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == field {
			return v
		}
	}
	return ""
}

func (c *clusters) until(ctx context.Context, what string, cond func() bool) error {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		if cond() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", what, ctx.Err())
		case <-t.C:
		}
	}
}

// moveSlots moves slots, keys and all, from src to dst: IMPORTING on dst,
// MIGRATING on src, MIGRATE of every key, then NODE on both, as
// redis-cli --cluster reshard does, each step pipelined over the slots.
//
// Valkey 8 replicates each CLUSTER SETSLOT to the node's replicas and
// holds the client until they acknowledge it, so one connection's pipeline
// of thousands of SETSLOTs is one replica round trip per slot: more than a
// minute on a loaded CI runner (run 39038657629). The SETSLOT steps are
// spread over setslotConns connections instead, whose blocked commands one
// acknowledgement releases together.
func (c *clusters) moveSlots(ctx context.Context, r *clusterRecord, src, dst *clusterNode, slots []int) error {
	s := c.dial(src.addr, r.password(), r.TransitCA)
	defer s.Close()
	each := func(f func(slot int) []any) [][]any {
		out := make([][]any, len(slots))
		for i, slot := range slots {
			out[i] = f(slot)
		}
		return out
	}
	if err := c.setslots(ctx, r, dst, each(func(slot int) []any { return []any{"CLUSTER", "SETSLOT", slot, "IMPORTING", src.id} })); err != nil {
		return fmt.Errorf("importing on %s: %w", dst.Server, err)
	}
	if err := c.setslots(ctx, r, src, each(func(slot int) []any { return []any{"CLUSTER", "SETSLOT", slot, "MIGRATING", dst.id} })); err != nil {
		return fmt.Errorf("migrating on %s: %w", src.Server, err)
	}
	counts, err := s.Pipe(ctx, each(func(slot int) []any { return []any{"CLUSTER", "COUNTKEYSINSLOT", slot} }))
	if err != nil {
		return err
	}
	for i, slot := range slots {
		if n, _ := toInt(counts[i]); n == 0 {
			continue
		}
		for {
			v, err := s.Do(ctx, "CLUSTER", "GETKEYSINSLOT", slot, 100)
			if err != nil {
				return err
			}
			keys, _ := v.([]any)
			if len(keys) == 0 {
				break
			}
			args := []any{"MIGRATE", dst.podIP, dst.port, "", 0, 5000, "REPLACE"}
			if pw := r.password(); pw != "" {
				args = append(args, "AUTH", pw)
			}
			args = append(args, "KEYS")
			args = append(args, keys...)
			if _, err := s.Do(ctx, args...); err != nil {
				return fmt.Errorf("migrate slot %d: %w", slot, err)
			}
		}
	}
	node := each(func(slot int) []any { return []any{"CLUSTER", "SETSLOT", slot, "NODE", dst.id} })
	if err := c.setslots(ctx, r, dst, node); err != nil {
		return fmt.Errorf("slots to %s: %w", dst.Server, err)
	}
	if err := c.setslots(ctx, r, src, node); err != nil {
		return fmt.Errorf("slots off %s: %w", src.Server, err)
	}
	return nil
}

// setslotConns is how many connections setslots spreads its commands over.
const setslotConns = 32

// setslots runs cmds, CLUSTER SETSLOTs of distinct slots, on n, pipelined
// over up to setslotConns connections at once (see moveSlots); their
// order across slots does not matter.
func (c *clusters) setslots(ctx context.Context, r *clusterRecord, n *clusterNode, cmds [][]any) error {
	conns := min(setslotConns, len(cmds))
	errs := make([]error, conns)
	var wg sync.WaitGroup
	for i := range conns {
		lo, hi := i*len(cmds)/conns, (i+1)*len(cmds)/conns
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl := c.dial(n.addr, r.password(), r.TransitCA)
			defer cl.Close()
			_, errs[i] = cl.Pipe(ctx, cmds[lo:hi])
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// ownedSlots reads which primary serves each slot, by node ID, from via.
func (c *clusters) ownedSlots(ctx context.Context, r *clusterRecord, via *clusterNode) (map[string][]int, error) {
	cl := c.dial(via.addr, r.password(), r.TransitCA)
	defer cl.Close()
	v, err := cl.Do(ctx, "CLUSTER", "SHARDS")
	if err != nil {
		return nil, err
	}
	out := map[string][]int{}
	shards, _ := v.([]any)
	for _, sh := range shards {
		fields := pairs(sh)
		var slots []int
		if rs, ok := fields["slots"].([]any); ok {
			for i := 0; i+1 < len(rs); i += 2 {
				lo, _ := toInt(rs[i])
				hi, _ := toInt(rs[i+1])
				for s := lo; s <= hi; s++ {
					slots = append(slots, s)
				}
			}
		}
		nodes, _ := fields["nodes"].([]any)
		for _, n := range nodes {
			nf := pairs(n)
			if fmt.Sprint(nf["role"]) == "master" {
				out[fmt.Sprint(nf["id"])] = slots
			}
		}
	}
	return out, nil
}

// pairs reads a RESP map or flat key-value list.
func pairs(v any) map[string]any {
	out := map[string]any{}
	switch t := v.(type) {
	case map[any]any:
		for k, val := range t {
			out[fmt.Sprint(k)] = val
		}
	case []any:
		for i := 0; i+1 < len(t); i += 2 {
			out[fmt.Sprint(t[i])] = t[i+1]
		}
	}
	return out
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case int64:
		return int(t), true
	case int:
		return t, true
	case string:
		n, err := strconv.Atoi(t)
		return n, err == nil
	}
	return 0, false
}

// UpdateCluster changes shard_count, replica_count, node_type,
// redis_configs, deletion_protection_enabled, psc_configs,
// persistence_config, zone distribution and maintenance policy.
func (s *ClusterServer) UpdateCluster(ctx context.Context, req *clusterpb.UpdateClusterRequest) (*longrunningpb.Operation, error) {
	in := req.GetCluster()
	if in == nil {
		return nil, apierror.InvalidArgument("cluster is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	set := map[string]bool{}
	for _, p := range paths {
		switch p {
		case "shard_count", "shardCount":
			set["shard_count"] = true
		case "replica_count", "replicaCount":
			set["replica_count"] = true
		case "node_type", "nodeType":
			set["node_type"] = true
		case "redis_configs", "redisConfigs":
			set["redis_configs"] = true
		case "deletion_protection_enabled", "deletionProtectionEnabled":
			set["deletion_protection_enabled"] = true
		case "psc_configs", "pscConfigs":
			set["psc_configs"] = true
		case "persistence_config", "persistenceConfig":
			set["persistence_config"] = true
		case "zone_distribution_config", "zoneDistributionConfig":
			set["zone_distribution_config"] = true
		case "maintenance_policy", "maintenancePolicy":
			set["maintenance_policy"] = true
		case "automated_backup_config", "automatedBackupConfig":
			set["automated_backup_config"] = true
		case "acl_policy", "aclPolicy":
			set["acl_policy"] = true
		case "cross_cluster_replication_config", "crossClusterReplicationConfig":
			return nil, checkCrossCluster(true)
		case "labels":
			// Set by ClusterLabels, beside the transcoder.
		case "authorization_mode", "authorizationMode", "transit_encryption_mode", "transitEncryptionMode":
			if p == "transit_encryption_mode" || p == "transitEncryptionMode" {
				return nil, apierror.InvalidArgument("transit_encryption_mode cannot be changed once a cluster is created")
			}
			set["authorization_mode"] = true
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not an updatable field of Cluster", p)
		}
	}
	c, r, err := s.find(in.GetName())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	if _, pending := c.busy[r.Name]; pending || r.state != clusterpb.Cluster_ACTIVE {
		return nil, apierror.FailedPrecondition("cluster %s is %s: a cluster is updated only when ACTIVE", r.Name, r.state)
	}
	next := *r
	if set["shard_count"] {
		next.ShardCount = in.GetShardCount()
	}
	if set["replica_count"] {
		next.ReplicaCount = in.GetReplicaCount()
	}
	if err := checkShape(next.ShardCount, next.ReplicaCount); err != nil {
		return nil, err
	}
	if next.ShardCount != r.ShardCount && next.ReplicaCount != r.ReplicaCount {
		return nil, apierror.InvalidArgument("shard_count and replica_count cannot be changed in one update")
	}
	if set["node_type"] {
		nt := in.GetNodeType()
		if _, ok := nodeSizes[nt]; !ok {
			return nil, apierror.InvalidArgument("node_type %s is not a Memorystore for Redis Cluster node type", nt)
		}
		next.NodeType = nt.String()
	}
	if set["redis_configs"] {
		if err := checkClusterConfigs(in.GetRedisConfigs()); err != nil {
			return nil, err
		}
		next.RedisConfigs = copyMap(in.GetRedisConfigs())
	}
	if set["deletion_protection_enabled"] {
		next.DeletionProtect = in.GetDeletionProtectionEnabled()
	}
	if set["psc_configs"] {
		next.PscConfigs = nil
		for _, p := range in.GetPscConfigs() {
			next.PscConfigs = append(next.PscConfigs, p.GetNetwork())
		}
	}
	if set["persistence_config"] {
		next.Persistence = ""
		keepJSON(&next.Persistence, in.GetPersistenceConfig())
	}
	if set["zone_distribution_config"] {
		next.ZoneDistribution = ""
		keepJSON(&next.ZoneDistribution, in.GetZoneDistributionConfig())
	}
	if set["maintenance_policy"] {
		next.Maintenance = ""
		keepJSON(&next.Maintenance, in.GetMaintenancePolicy())
	}
	if set["authorization_mode"] {
		switch a := in.GetAuthorizationMode(); {
		case a.String() == r.AuthMode:
		case a == clusterpb.AuthorizationMode_AUTH_MODE_TOKEN_AUTH || r.AuthMode == clusterpb.AuthorizationMode_AUTH_MODE_TOKEN_AUTH.String():
			return nil, apierror.Unimplemented("authorization_mode cannot be changed to or from AUTH_MODE_TOKEN_AUTH here: the nodes' default user's password is set when they start")
		case a == clusterpb.AuthorizationMode_AUTH_MODE_DISABLED, a == clusterpb.AuthorizationMode_AUTH_MODE_IAM_AUTH:
			next.AuthMode = a.String()
		default:
			return nil, apierror.InvalidArgument("authorization_mode %s is not an authorization mode", a)
		}
	}
	if set["automated_backup_config"] {
		if err := checkAutoBackup(in.GetAutomatedBackupConfig()); err != nil {
			return nil, err
		}
		next.AutoBackup = protoJSON(in.GetAutomatedBackupConfig())
	}
	syncACL := false
	if set["acl_policy"] && in.GetAclPolicy() != r.AclPolicy {
		m := clusterRE.FindStringSubmatch(r.Name)
		if next.AclPolicy, err = c.attachPolicy(ctx, in.GetAclPolicy(), "projects/"+m[1]+"/locations/"+m[2]); err != nil {
			return nil, err
		}
		syncACL = true
	}
	if set["node_type"] || set["redis_configs"] || set["persistence_config"] {
		settings := serverConfigs(next.RedisConfigs)
		for k := range serverConfigs(r.RedisConfigs) {
			if _, kept := settings[k]; !kept {
				settings[k] = configDefaults[k]
			}
		}
		settings["maxmemory"] = strconv.FormatInt(next.maxMemory(), 10)
		// persistence_config's settings (restart.go).
		for _, kv := range next.persistenceSettings() {
			settings[kv[0]] = kv[1]
		}
		for _, n := range r.Nodes {
			for k, v := range settings {
				if err := c.do(ctx, r, n, "CONFIG", "SET", k, v); err != nil {
					return nil, apierror.InvalidArgument("the server refused the change: %v", err)
				}
			}
		}
	}
	reshape := next.ShardCount != r.ShardCount || next.ReplicaCount != r.ReplicaCount
	shards, replicas := next.ShardCount, next.ReplicaCount
	next.ShardCount, next.ReplicaCount = r.ShardCount, r.ReplicaCount // until the nodes are
	if err := c.save(&next); err != nil {
		return nil, apierror.Internal(err, "keep cluster %s", r.Name)
	}
	*r = next
	op := c.newOperation(r.Name, "update")
	if syncACL && !reshape {
		// The policy's users on every node (aclpolicy.go), then done.
		c.busy[r.Name] = op
		snap, users := *r, c.aclUsers(r)
		nodes := append([]*clusterNode(nil), r.Nodes...)
		go func() {
			ctx, cancel := context.WithTimeout(c.bg, clusterWait)
			defer cancel()
			err := c.syncUsers(ctx, &snap, users, nodes)
			c.mu.Lock()
			delete(c.busy, snap.Name)
			out := c.render(r)
			c.mu.Unlock()
			c.finish(op, out, err)
		}()
		return s.api.operation(op)
	}
	if !reshape {
		c.finish(op, c.render(r), nil)
		return s.api.operation(op)
	}
	r.state, r.message = clusterpb.Cluster_UPDATING, "changing the cluster's shape"
	r.pendingShards, r.pendingReplicas = shards, replicas
	c.busy[r.Name] = op
	go c.reshape(r.Name, op, shards, replicas)
	return s.api.operation(op)
}

// reshape adds or removes shards or replicas, then completes op.
func (c *clusters) reshape(name, op string, shards, replicas int32) {
	ctx, cancel := context.WithTimeout(c.bg, clusterWait)
	defer cancel()
	c.mu.Lock()
	r := c.byName[name]
	snap := *r
	snap.Nodes = append([]*clusterNode(nil), r.Nodes...)
	c.mu.Unlock()
	var err error
	var removed []*clusterNode
	switch {
	case shards > snap.ShardCount:
		err = c.addShards(ctx, &snap, int(shards))
	case shards < snap.ShardCount:
		removed, err = c.removeShards(ctx, &snap, int(shards))
	case replicas > snap.ReplicaCount:
		err = c.addReplicas(ctx, &snap, int(replicas))
	case replicas < snap.ReplicaCount:
		removed, err = c.removeReplicas(ctx, &snap, int(replicas))
	}
	c.mu.Lock()
	delete(c.busy, name)
	r.pendingShards, r.pendingReplicas = 0, -1
	if err == nil {
		r.Nodes, r.NextNode, r.ShardCount, r.ReplicaCount = snap.Nodes, snap.NextNode, shards, replicas
		err = c.save(r)
	}
	r.state, r.message = clusterpb.Cluster_ACTIVE, ""
	out := c.output(r)
	c.mu.Unlock()
	if removed != nil && err == nil {
		c.removeNodes(removed)
	}
	if err != nil {
		err = fmt.Errorf("the cluster's shape did not change: %w", err)
	}
	c.finish(op, out, err)
}

// newNode adds a node to r, of shard.
func (c *clusters) newNode(r *clusterRecord, shard int, primary bool) *clusterNode {
	n := &clusterNode{Server: clusterServerName(r.Name, r.NextNode), Shard: shard, Primary: primary}
	r.NextNode++
	r.Nodes = append(r.Nodes, n)
	return n
}

func (c *clusters) addShards(ctx context.Context, r *clusterRecord, shards int) error {
	old := append([]*clusterNode(nil), r.Nodes...)
	var added []*clusterNode
	for sh := int(r.ShardCount); sh < shards; sh++ {
		for k := 0; k <= int(r.ReplicaCount); k++ {
			added = append(added, c.newNode(r, sh, k == 0))
		}
	}
	if err := c.startNodes(ctx, r, added); err != nil {
		c.removeNodes(added)
		return err
	}
	if err := c.meet(ctx, r, old[0], added); err != nil {
		return err
	}
	if err := c.replicate(ctx, r, added, r.Nodes); err != nil {
		return err
	}
	// The new primaries must know who serves every slot before they import
	// any: a node answers RESTORE-ASKING for a slot it sees unassigned
	// with CLUSTERDOWN.
	if err := c.settle(ctx, r, r.Nodes); err != nil {
		return err
	}
	return c.rebalance(ctx, r, shards)
}

// rebalance moves slots so the first shards primaries hold them evenly,
// and the others none.
func (c *clusters) rebalance(ctx context.Context, r *clusterRecord, shards int) error {
	var primaries []*clusterNode
	for _, n := range r.Nodes {
		if n.Primary {
			primaries = append(primaries, n)
		}
	}
	sort.Slice(primaries, func(i, j int) bool { return primaries[i].Shard < primaries[j].Shard })
	owned, err := c.ownedSlots(ctx, r, primaries[0])
	if err != nil {
		return err
	}
	byID := map[string]*clusterNode{}
	for _, p := range primaries {
		byID[p.id] = p
	}
	ranges := slotRanges(shards)
	want := map[int]*clusterNode{}
	for i, rg := range ranges {
		for s := rg[0]; s <= rg[1]; s++ {
			want[s] = primaries[i]
		}
	}
	type move struct{ src, dst *clusterNode }
	moves := map[move][]int{}
	for id, slots := range owned {
		src := byID[id]
		if src == nil {
			continue
		}
		for _, s := range slots {
			if dst := want[s]; dst != src {
				moves[move{src, dst}] = append(moves[move{src, dst}], s)
			}
		}
	}
	for m, slots := range moves {
		if err := c.moveSlots(ctx, r, m.src, m.dst, slots); err != nil {
			return err
		}
	}
	return c.settle(ctx, r, r.Nodes)
}

func (c *clusters) removeShards(ctx context.Context, r *clusterRecord, shards int) ([]*clusterNode, error) {
	if err := c.rebalance(ctx, r, shards); err != nil {
		return nil, err
	}
	var keep, gone []*clusterNode
	for _, n := range r.Nodes {
		if n.Shard < shards {
			keep = append(keep, n)
		} else {
			gone = append(gone, n)
		}
	}
	if err := c.forget(ctx, r, keep, gone); err != nil {
		return nil, err
	}
	r.Nodes = keep
	return gone, nil
}

func (c *clusters) addReplicas(ctx context.Context, r *clusterRecord, replicas int) error {
	var added []*clusterNode
	for sh := 0; sh < int(r.ShardCount); sh++ {
		for k := int(r.ReplicaCount); k < replicas; k++ {
			added = append(added, c.newNode(r, sh, false))
		}
	}
	if err := c.startNodes(ctx, r, added); err != nil {
		c.removeNodes(added)
		return err
	}
	if !r.standalone() {
		if err := c.meet(ctx, r, r.firstPrimary(), added); err != nil {
			return err
		}
	}
	if err := c.replicate(ctx, r, added, r.Nodes); err != nil {
		return err
	}
	return c.settle(ctx, r, r.Nodes)
}

func (c *clusters) removeReplicas(ctx context.Context, r *clusterRecord, replicas int) ([]*clusterNode, error) {
	count := map[int]int{}
	var keep, gone []*clusterNode
	for _, n := range r.Nodes {
		if n.Primary {
			keep = append(keep, n)
			continue
		}
		if count[n.Shard] < replicas {
			count[n.Shard]++
			keep = append(keep, n)
		} else {
			gone = append(gone, n)
		}
	}
	if err := c.forget(ctx, r, keep, gone); err != nil {
		return nil, err
	}
	r.Nodes = keep
	return gone, nil
}

// forget removes gone from the cluster: CLUSTER RESET on each, so it stops
// gossiping, and FORGET on every node kept.
func (c *clusters) forget(ctx context.Context, r *clusterRecord, keep, gone []*clusterNode) error {
	if r.standalone() {
		r.Nodes = keep
		return nil
	}
	for _, g := range gone {
		_ = c.do(ctx, r, g, "CLUSTER", "RESET", "HARD")
	}
	for _, k := range keep {
		for _, g := range gone {
			if err := c.do(ctx, r, k, "CLUSTER", "FORGET", g.id); err != nil {
				return err
			}
		}
	}
	r.Nodes = keep
	return c.settle(ctx, r, keep)
}

func (c *clusters) removeNodes(nodes []*clusterNode) {
	ctx, cancel := context.WithTimeout(c.bg, clusterWait)
	defer cancel()
	for _, n := range nodes {
		_ = c.rt.Remove(ctx, n.Server)
	}
}

// DeleteCluster removes a cluster: its nodes and their data.
func (s *ClusterServer) DeleteCluster(ctx context.Context, req *clusterpb.DeleteClusterRequest) (*longrunningpb.Operation, error) {
	c, r, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	if _, pending := c.busy[r.Name]; pending {
		return nil, apierror.FailedPrecondition("cluster %s has an operation in progress", r.Name)
	}
	if r.DeletionProtect {
		return nil, apierror.FailedPrecondition("cluster %s has deletion protection enabled: disable it first", r.Name)
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
			if derr := c.db.Delete(c.prefix() + clusterKey(name)); derr != nil && !errors.Is(derr, store.ErrNotFound) {
				err = derr
			}
		}
		c.mu.Unlock()
		c.finish(op, &emptypb.Empty{}, err)
	}()
	return s.api.operation(op)
}
