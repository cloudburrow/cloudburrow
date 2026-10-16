package redisadmin

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// haNode is a fake Standard tier node: a listener that greets each
// connection with its server's name, and its replication state.
type haNode struct {
	server  string
	ln      net.Listener
	primary string // "" when it is a primary, else the host it replicates
	offset  int64
	linkUp  bool
	down    bool // does not answer
}

// haCluster runs the fake nodes, as a Provisioner and ReplicaSet.
type haCluster struct {
	mu     sync.Mutex
	nodes  map[string]*haNode // by server
	byAddr map[string]*haNode
	routes map[string]string // service -> node
	gone   []string
	t      *testing.T
	// primaryOffset is the primary's offset, set on every new primary.
	primaryOffset int64
}

func newHACluster(t *testing.T) *haCluster {
	return &haCluster{nodes: map[string]*haNode{}, byAddr: map[string]*haNode{}, routes: map[string]string{}, t: t}
}

func (h *haCluster) Provision(_ context.Context, spec Spec) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n, ok := h.nodes[spec.Server]; ok {
		return n.ln.Addr().String(), nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	h.t.Cleanup(func() { ln.Close() })
	n := &haNode{server: spec.Server, ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(spec.Server + "\n"))
			go func() { _, _ = bufio.NewReader(c).ReadString('\n'); c.Close() }()
		}
	}()
	h.nodes[spec.Server] = n
	h.byAddr[ln.Addr().String()] = n
	return ln.Addr().String(), nil
}

func (h *haCluster) Remove(_ context.Context, server string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gone = append(h.gone, server)
	if n, ok := h.nodes[server]; ok {
		n.ln.Close()
		delete(h.byAddr, n.ln.Addr().String())
		delete(h.nodes, server)
	}
	return nil
}

func (h *haCluster) ClusterAddr(server string) (string, int) { return server + ".svc", 6379 }

func (h *haCluster) Route(_ context.Context, service, node string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.routes[service] = node
	return nil
}

// haData is a node's data plane.
type haData struct {
	h    *haCluster
	addr string
}

func (d haData) node() *haNode {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	return d.h.byAddr[d.addr]
}

func (d haData) Info(context.Context) (ServerInfo, error) { return ServerInfo{Version: "7.2.4"}, nil }
func (d haData) ConfigSet(context.Context, map[string]string) error {
	return nil
}
func (d haData) Save(context.Context) (string, error)    { return "", nil }
func (d haData) Load(context.Context, string, int) error { return nil }
func (d haData) ReplicaOf(_ context.Context, host string, _ int) error {
	n := d.node()
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if n.down {
		return errors.New("down")
	}
	n.primary = host
	n.linkUp = host != ""
	n.offset = d.h.primaryOffset
	return nil
}

func (d haData) Replication(context.Context) (Replication, error) {
	n := d.node()
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if n.down {
		return Replication{}, errors.New("down")
	}
	if n.primary == "" {
		return Replication{Primary: true, Offset: d.h.primaryOffset}, nil
	}
	return Replication{Offset: n.offset, LinkUp: n.linkUp}, nil
}

func newHA(t *testing.T) (*Server, *haCluster) {
	t.Helper()
	s := newTestServer(nil)
	h := newHACluster(t)
	s.EnableCreate(context.Background(), h, func(addr, _ string) DataPlane { return haData{h: h, addr: addr} }, nil, nil)
	return s, h
}

// greet is the server name the node at addr greets with.
func greet(t *testing.T, host string, port int32) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

func waitHA(t *testing.T, s *Server, op *longrunningpb.Operation) *longrunningpb.Operation {
	t.Helper()
	for i := 0; i < 200; i++ {
		got, err := s.Operation(op.GetName())
		if err != nil {
			t.Fatal(err)
		}
		if got.GetDone() {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("operation %s not done", op.GetName())
	return nil
}

const haParent = "projects/test-project/locations/us-central1"

func createHA(t *testing.T, s *Server, id string, in *redispb.Instance) *redispb.Instance {
	t.Helper()
	op, err := s.CreateInstance(context.Background(), &redispb.CreateInstanceRequest{Parent: haParent, InstanceId: id, Instance: in})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitHA(t, s, op); got.GetError() != nil {
		t.Fatalf("create: %v", got.GetError())
	}
	inst, err := s.GetInstance(context.Background(), &redispb.GetInstanceRequest{Name: haParent + "/instances/" + id})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestStandardTierWithReadReplicasAndFailover(t *testing.T) {
	s, h := newHA(t)
	inst := createHA(t, s, "ha", &redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 5,
		ReadReplicasMode: redispb.Instance_READ_REPLICAS_ENABLED})
	name := haParent + "/instances/ha"
	srv := ServerName(name)
	if inst.GetReplicaCount() != 2 || len(inst.GetNodes()) != 3 {
		t.Fatalf("replica_count %d nodes %v, want the default 2 replicas and 3 nodes", inst.GetReplicaCount(), inst.GetNodes())
	}
	if inst.GetCurrentLocationId() != "us-central1-a" || inst.GetAlternativeLocationId() != "us-central1-b" {
		t.Fatalf("current %s alternative %s", inst.GetCurrentLocationId(), inst.GetAlternativeLocationId())
	}
	if got := greet(t, inst.GetHost(), inst.GetPort()); got != srv+"-n0" {
		t.Fatalf("the primary endpoint reached %s, want node 0", got)
	}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		seen[greet(t, inst.GetReadEndpoint(), inst.GetReadEndpointPort())] = true
	}
	if !seen[srv+"-n1"] || !seen[srv+"-n2"] || seen[srv+"-n0"] {
		t.Fatalf("the read endpoint reached %v, want both replicas and not the primary", seen)
	}
	if h.routes[srv] != srv+"-n0" {
		t.Fatalf("the instance's Service selects %q", h.routes[srv])
	}

	op, err := s.FailoverInstance(context.Background(), &redispb.FailoverInstanceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitHA(t, s, op); got.GetError() != nil {
		t.Fatalf("failover: %v", got.GetError())
	}
	after, _ := s.GetInstance(context.Background(), &redispb.GetInstanceRequest{Name: name})
	if after.GetHost() != inst.GetHost() || after.GetPort() != inst.GetPort() {
		t.Fatalf("the endpoint moved: %s:%d", after.GetHost(), after.GetPort())
	}
	primary := greet(t, after.GetHost(), after.GetPort())
	if primary == srv+"-n0" || h.routes[srv] != primary {
		t.Fatalf("after failover the endpoint reaches %s and the Service %s", primary, h.routes[srv])
	}
	if after.GetCurrentLocationId() == "us-central1-a" {
		t.Fatalf("current_location_id is still the old primary's zone")
	}
	h.mu.Lock()
	oldPrimary := h.nodes[srv+"-n0"].primary
	h.mu.Unlock()
	if oldPrimary != primary+".svc" {
		t.Fatalf("the old primary replicates %q, want the new one", oldPrimary)
	}

	// replica_count down to 1, then read replicas cannot be disabled.
	op, err = s.UpdateInstance(context.Background(), &redispb.UpdateInstanceRequest{
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"replica_count"}},
		Instance:   &redispb.Instance{Name: name, ReplicaCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitHA(t, s, op); got.GetError() != nil {
		t.Fatalf("scale: %v", got.GetError())
	}
	scaled, _ := s.GetInstance(context.Background(), &redispb.GetInstanceRequest{Name: name})
	if scaled.GetReplicaCount() != 1 || len(scaled.GetNodes()) != 2 {
		t.Fatalf("after scaling: %d replicas, nodes %v", scaled.GetReplicaCount(), scaled.GetNodes())
	}
	_, err = s.UpdateInstance(context.Background(), &redispb.UpdateInstanceRequest{
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"read_replicas_mode"}},
		Instance:   &redispb.Instance{Name: name, ReadReplicasMode: redispb.Instance_READ_REPLICAS_DISABLED}})
	if apierror.From(err).Code != codes.InvalidArgument {
		t.Fatalf("disabling read replicas: %v", err)
	}

	dop, err := s.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	waitHA(t, s, dop)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.nodes) != 0 {
		t.Fatalf("nodes left after delete: %v", h.nodes)
	}
}

func TestLimitedDataLossRefusesALaggingReplica(t *testing.T) {
	s, h := newHA(t)
	createHA(t, s, "lag", &redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 1})
	name := haParent + "/instances/lag"
	h.mu.Lock()
	h.primaryOffset = limitedDataLoss + 1 // the replica stays at 0
	h.mu.Unlock()
	op, err := s.FailoverInstance(context.Background(), &redispb.FailoverInstanceRequest{Name: name,
		DataProtectionMode: redispb.FailoverInstanceRequest_LIMITED_DATA_LOSS})
	if err != nil {
		t.Fatal(err)
	}
	got := waitHA(t, s, op)
	if got.GetError().GetCode() != int32(codes.FailedPrecondition) || !strings.Contains(got.GetError().GetMessage(), "30 MB") {
		t.Fatalf("LIMITED_DATA_LOSS with a lagging replica: %v", got.GetError())
	}
	inst, _ := s.GetInstance(context.Background(), &redispb.GetInstanceRequest{Name: name})
	if inst.GetCurrentLocationId() != "us-central1-a" || inst.GetState() != redispb.Instance_READY {
		t.Fatalf("after a refused failover: %s %s", inst.GetCurrentLocationId(), inst.GetState())
	}
	op, err = s.FailoverInstance(context.Background(), &redispb.FailoverInstanceRequest{Name: name,
		DataProtectionMode: redispb.FailoverInstanceRequest_FORCE_DATA_LOSS})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitHA(t, s, op); got.GetError() != nil {
		t.Fatalf("FORCE_DATA_LOSS: %v", got.GetError())
	}
	inst, _ = s.GetInstance(context.Background(), &redispb.GetInstanceRequest{Name: name})
	if inst.GetCurrentLocationId() != "us-central1-b" {
		t.Fatalf("after a forced failover current_location_id is %s", inst.GetCurrentLocationId())
	}
}

func TestStandardTierChecks(t *testing.T) {
	s, _ := newHA(t)
	for _, tc := range []struct {
		in   *redispb.Instance
		want codes.Code
	}{
		{&redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 1, ReplicaCount: 2}, codes.InvalidArgument},
		{&redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 1, ReadReplicasMode: redispb.Instance_READ_REPLICAS_ENABLED}, codes.InvalidArgument},
		{&redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 5, ReplicaCount: 6, ReadReplicasMode: redispb.Instance_READ_REPLICAS_ENABLED}, codes.InvalidArgument},
		{&redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, ReplicaCount: 1}, codes.InvalidArgument},
		{&redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 1, LocationId: "us-central1-a", AlternativeLocationId: "us-central1-a"}, codes.InvalidArgument},
	} {
		_, err := s.CreateInstance(context.Background(), &redispb.CreateInstanceRequest{Parent: haParent, InstanceId: "bad", Instance: tc.in})
		if apierror.From(err).Code != tc.want {
			t.Errorf("%v: %v, want %v", tc.in, err, tc.want)
		}
	}
	basic := createHA(t, s, "basic", &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1})
	if _, err := s.FailoverInstance(context.Background(), &redispb.FailoverInstanceRequest{Name: basic.GetName()}); apierror.From(err).Code != codes.FailedPrecondition {
		t.Fatalf("failover of BASIC: %v", err)
	}
}

func TestParseReplication(t *testing.T) {
	st := parseReplication("# Replication\r\nrole:slave\r\nmaster_link_status:up\r\nslave_repl_offset:420\r\nmaster_repl_offset:420\r\n")
	if st.Primary || !st.LinkUp || st.Offset != 420 {
		t.Fatalf("%+v", st)
	}
	if st := parseReplication("role:master\nmaster_repl_offset:99\n"); !st.Primary || st.Offset != 99 {
		t.Fatalf("%+v", st)
	}
}

// TestAutomaticFailover (#1412): a STANDARD_HA instance whose primary
// stops answering fails over on its own, after failAfter failed health
// checks: the replica is promoted, the endpoint stays at the same address
// and reaches it, the Service selects it, and the instance is READY; a
// BASIC instance is never failed over.
func TestAutomaticFailover(t *testing.T) {
	old := healthEvery
	healthEvery = 10 * time.Millisecond
	t.Cleanup(func() { healthEvery = old })
	s, h := newHA(t)
	inst := createHA(t, s, "auto", &redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 1})
	name := haParent + "/instances/auto"
	srv := ServerName(name)
	h.mu.Lock()
	h.nodes[srv+"-n0"].down = true
	h.mu.Unlock()
	var after *redispb.Instance
	for i := 0; i < 300; i++ {
		after, _ = s.GetInstance(context.Background(), &redispb.GetInstanceRequest{Name: name})
		if after.GetCurrentLocationId() == "us-central1-b" && after.GetState() == redispb.Instance_READY {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after.GetCurrentLocationId() != "us-central1-b" || after.GetState() != redispb.Instance_READY {
		t.Fatalf("after the primary failed: %s %s", after.GetCurrentLocationId(), after.GetState())
	}
	if after.GetHost() != inst.GetHost() || after.GetPort() != inst.GetPort() {
		t.Fatalf("the endpoint moved: %s:%d", after.GetHost(), after.GetPort())
	}
	if got := greet(t, after.GetHost(), after.GetPort()); got != srv+"-n1" {
		t.Fatalf("the endpoint reaches %s, want node 1", got)
	}
	h.mu.Lock()
	route := h.routes[srv]
	h.mu.Unlock()
	if route != srv+"-n1" {
		t.Fatalf("the Service selects %s", route)
	}
}
