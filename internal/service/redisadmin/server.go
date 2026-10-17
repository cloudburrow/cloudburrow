// Package redisadmin serves the Memorystore for Redis admin API,
// google.cloud.redis.v1.CloudRedis (#1302).
//
// Two kinds of instance are served. The instance `up` runs, the Valkey
// server `--services memorystore` starts, is described, not invented: its
// host and port are the address an application on the host connects to,
// its redis_version and memory_size_gb are what the server itself reports
// (INFO and CONFIG GET maxmemory), and its state is READY only while the
// server answers. It cannot be updated or deleted.
//
// Every other instance is one CreateInstance made: a real Valkey server of
// its own, which a Provisioner runs (a Deployment and Service per instance,
// reached through a tunnel), started with the instance's maxmemory, AUTH
// and redis_configs. Its size and configs are changed on the running
// server with CONFIG SET, and DeleteInstance removes the server and its
// data. Its data is exported to and imported from Cloud Storage RDB files,
// and RDB persistence is its save point (#1384). What no server here can
// do (another Redis version, Standard tier replicas and failover,
// maintenance) is UNIMPLEMENTED rather than a plausible stub.
package redisadmin

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// InstanceID is the one instance's ID: the Kubernetes Service's name, so the
// API and the cluster call it the same thing.
const InstanceID = "memorystore"

// Region is the instance's location, and Zone its location_id. Google
// requires both and there is no real placement here, so they are the
// defaults the rest of CloudBurrow uses.
const (
	Region = "us-central1"
	Zone   = "us-central1-a"
)

// ServerInfo is what the running server reports about itself.
type ServerInfo struct {
	// Version is INFO server's redis_version, such as "7.2.4". Valkey reports
	// the Redis version it is compatible with there.
	Version string
	// MaxMemory is CONFIG GET maxmemory, in bytes; 0 means unlimited.
	MaxMemory int64
}

// Backend reaches the running server.
type Backend interface {
	Info(ctx context.Context) (ServerInfo, error)
}

// Instance is the static part of the one instance's description.
type Instance struct {
	// Project owns the instance: the CloudBurrow instance's default project.
	Project string
	// Host and Port are the address an application on the host connects to.
	Host string
	Port int32
	// Address, when set, is the host:port to report instead, read on each
	// call: the tunnel's, whose port is OS-assigned with
	// --port-memorystore 0.
	Address func() string
	// Created is when the server was started.
	Created time.Time
}

// Name is the instance's resource name.
func (i Instance) Name() string {
	return "projects/" + i.Project + "/locations/" + Region + "/instances/" + InstanceID
}

// Server implements redispb.CloudRedisServer.
type Server struct {
	redispb.UnimplementedCloudRedisServer
	inst    Instance
	backend Backend

	// created are the instances CreateInstance made, when EnableCreate
	// was called; nil otherwise, and CreateInstance is UNIMPLEMENTED.
	created *instances
	// clusters are the Redis clusters CreateCluster made, when
	// EnableClusters was called (#1519); nil otherwise.
	clusters *clusters
	// valkey are the Memorystore for Valkey instances (memorystore.go),
	// when EnableValkey was called (#1519); nil otherwise.
	valkey *clusters
}

// NewServer returns the API over backend, describing inst.
func NewServer(inst Instance, backend Backend) *Server {
	return &Server{inst: inst, backend: backend}
}

// Register adds the service to a gRPC server or to the JSON transcoder.
func (s *Server) Register(g grpc.ServiceRegistrar) { redispb.RegisterCloudRedisServer(g, s) }

var (
	parentRE   = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	instanceRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/instances/([^/]+)$`)
)

// ListInstances lists the instances in a project, in one region or in
// every region ("locations/-", as Google accepts): the instance `up` runs,
// in its project and region, and every instance CreateInstance made there,
// in name order and paged.
func (s *Server) ListInstances(ctx context.Context, req *redispb.ListInstancesRequest) (*redispb.ListInstancesResponse, error) {
	m := parentRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent must be projects/{project}/locations/{location}, not %q", req.GetParent())
	}
	if req.GetPageSize() < 0 {
		return nil, apierror.InvalidArgument("page_size must not be negative")
	}
	byName := map[string]*redispb.Instance{}
	if m[1] == s.inst.Project && (m[2] == Region || m[2] == "-") {
		inst, err := s.describe(ctx)
		if err != nil {
			return nil, err
		}
		byName[inst.GetName()] = inst
	}
	if s.created != nil {
		for _, inst := range s.created.list(ctx, m[1], m[2]) {
			byName[inst.GetName()] = inst
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	page, next, err := paging.Page("redis-instances:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token %q", req.GetPageToken())
	}
	resp := &redispb.ListInstancesResponse{NextPageToken: next}
	for _, n := range page {
		resp.Instances = append(resp.Instances, byName[n])
	}
	return resp, nil
}

// GetInstance describes an instance.
func (s *Server) GetInstance(ctx context.Context, req *redispb.GetInstanceRequest) (*redispb.Instance, error) {
	isDefault, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	if isDefault {
		return s.describe(ctx)
	}
	return s.created.get(ctx, req.GetName())
}

// GetInstanceAuthString returns a created instance's AUTH string. The
// instance `up` runs has no AUTH (docs/memorystore.md), and neither has a
// created instance without auth_enabled: Google then returns an empty
// string, which this does too.
func (s *Server) GetInstanceAuthString(ctx context.Context, req *redispb.GetInstanceAuthStringRequest) (*redispb.InstanceAuthString, error) {
	isDefault, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	if isDefault {
		return nil, apierror.FailedPrecondition("AUTH is not enabled for instance %s", req.GetName())
	}
	return s.created.authString(req.GetName())
}

// find checks name is an instance, and says whether it is the one `up`
// runs.
func (s *Server) find(name string) (isDefault bool, err error) {
	m := instanceRE.FindStringSubmatch(name)
	if m == nil {
		return false, apierror.InvalidArgument("name must be projects/{project}/locations/{location}/instances/{instance}, not %q", name)
	}
	if name == s.inst.Name() {
		return true, nil
	}
	if s.created != nil && s.created.exists(name) {
		return false, nil
	}
	return false, apierror.NotFound("Resource '%s' was not found", name)
}

// describe reads the server and returns the instance as Google shapes it.
func (s *Server) describe(ctx context.Context) (*redispb.Instance, error) {
	info, err := s.backend.Info(ctx)
	if err != nil {
		return nil, &apierror.Error{Code: codes.Unavailable, Message: "the Memorystore server is not reachable: " + err.Error(), Cause: err}
	}
	host, port := s.inst.Host, s.inst.Port
	if s.inst.Address != nil {
		host, port = splitAddr(s.inst.Address(), host, port)
	}
	return &redispb.Instance{
		Name:                  s.inst.Name(),
		LocationId:            Zone,
		CurrentLocationId:     Zone,
		RedisVersion:          RedisVersion(info.Version),
		Host:                  host,
		Port:                  port,
		CreateTime:            timestamppb.New(s.inst.Created),
		State:                 redispb.Instance_READY,
		Tier:                  redispb.Instance_BASIC,
		MemorySizeGb:          MemorySizeGB(info.MaxMemory),
		ConnectMode:           redispb.Instance_DIRECT_PEERING,
		AuthEnabled:           false,
		TransitEncryptionMode: redispb.Instance_DISABLED,
		Nodes:                 []*redispb.NodeInfo{{Id: "node-0", Zone: Zone}},
	}, nil
}

// splitAddr is host:port, or host and port when addr does not parse.
func splitAddr(addr, host string, port int32) (string, int32) {
	if h, p, err := net.SplitHostPort(addr); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			return h, int32(n)
		}
	}
	return host, port
}

// RedisVersion is Memorystore's name for a server version: "7.2.4" is
// REDIS_7_2. A version that does not parse is passed through unchanged
// rather than guessed at.
func RedisVersion(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	if _, err := strconv.Atoi(parts[0]); err != nil {
		return v
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return v
	}
	// Memorystore names every 6.x REDIS_6_X, and the others by major and
	// minor: REDIS_7_2, REDIS_5_0.
	if parts[0] == "6" {
		return "REDIS_6_X"
	}
	return fmt.Sprintf("REDIS_%s_%s", parts[0], parts[1])
}

// MemorySizeGB is maxmemory in whole GiB, rounded up, as Memorystore sizes
// an instance. 0 (unlimited) is reported as 0: there is no configured size.
func MemorySizeGB(maxMemory int64) int32 {
	const gib = 1 << 30
	if maxMemory <= 0 {
		return 0
	}
	return int32((maxMemory + gib - 1) / gib)
}
