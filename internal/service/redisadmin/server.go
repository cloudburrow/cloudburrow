// Package redisadmin serves the Memorystore for Redis admin API,
// google.cloud.redis.v1.CloudRedis (#1302), over the one Valkey server
// `--services memorystore` runs.
//
// There is exactly one instance, because exactly one server runs: it is
// described, not invented. Its host and port are the address an application
// on the host connects to, its redis_version and memory_size_gb are what the
// server itself reports (INFO and CONFIG GET maxmemory), and its state is
// READY only while the server answers. Nothing here can start a second
// server, resize this one, or change its version, so every method that would
// is UNIMPLEMENTED rather than a plausible stub.
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

// ListInstances lists the one instance in its project, in its region or in
// every region ("locations/-", as Google accepts). Any other project or
// region has none. The one page is the whole answer, so a page token is
// one this server never issued.
func (s *Server) ListInstances(ctx context.Context, req *redispb.ListInstancesRequest) (*redispb.ListInstancesResponse, error) {
	m := parentRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent must be projects/{project}/locations/{location}, not %q", req.GetParent())
	}
	if req.GetPageSize() < 0 {
		return nil, apierror.InvalidArgument("page_size must not be negative")
	}
	if req.GetPageToken() != "" {
		return nil, apierror.InvalidArgument("invalid page_token %q", req.GetPageToken())
	}
	if m[1] != s.inst.Project || (m[2] != Region && m[2] != "-") {
		return &redispb.ListInstancesResponse{}, nil
	}
	inst, err := s.describe(ctx)
	if err != nil {
		return nil, err
	}
	return &redispb.ListInstancesResponse{Instances: []*redispb.Instance{inst}}, nil
}

// GetInstance describes the instance.
func (s *Server) GetInstance(ctx context.Context, req *redispb.GetInstanceRequest) (*redispb.Instance, error) {
	if err := s.find(req.GetName()); err != nil {
		return nil, err
	}
	return s.describe(ctx)
}

// GetInstanceAuthString refuses: the server has no AUTH (docs/memorystore.md),
// so there is no string to return, and auth_enabled says so.
func (s *Server) GetInstanceAuthString(ctx context.Context, req *redispb.GetInstanceAuthStringRequest) (*redispb.InstanceAuthString, error) {
	if err := s.find(req.GetName()); err != nil {
		return nil, err
	}
	return nil, apierror.FailedPrecondition("AUTH is not enabled for instance %s", req.GetName())
}

// find checks name is the one instance.
func (s *Server) find(name string) error {
	m := instanceRE.FindStringSubmatch(name)
	if m == nil {
		return apierror.InvalidArgument("name must be projects/{project}/locations/{location}/instances/{instance}, not %q", name)
	}
	if name != s.inst.Name() {
		return apierror.NotFound("Resource '%s' was not found", name)
	}
	return nil
}

// describe reads the server and returns the instance as Google shapes it.
func (s *Server) describe(ctx context.Context) (*redispb.Instance, error) {
	info, err := s.backend.Info(ctx)
	if err != nil {
		return nil, &apierror.Error{Code: codes.Unavailable, Message: "the Memorystore server is not reachable: " + err.Error(), Cause: err}
	}
	host, port := s.inst.Host, s.inst.Port
	if s.inst.Address != nil {
		if h, p, err := net.SplitHostPort(s.inst.Address()); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				host, port = h, int32(n)
			}
		}
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
