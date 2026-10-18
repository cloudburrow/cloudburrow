package redisadmin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"google.golang.org/protobuf/proto"
)

// vkNode is a fake CLUSTER_DISABLED node: a listener that greets each
// connection with its server's name, and its replication state.
type vkNode struct {
	server  string
	ln      net.Listener
	port    int
	primary string // "" when it is a primary, else the host:port it replicates
	replID  string
	offset  int64
	down    bool
}

// vkNodes runs the fake nodes, as a ClusterRuntime and RoleRouter.
type vkNodes struct {
	mu     sync.Mutex
	t      *testing.T
	nodes  map[string]*vkNode
	byAddr map[string]*vkNode
	roles  map[string]string // server -> role, as last labelled
	ids    int
}

func (f *vkNodes) StartClusterNode(_ context.Context, spec Spec) (string, string, int, error) {
	if spec.PortName != ValkeyPortName {
		f.t.Errorf("node %s port name %q, want %q", spec.Server, spec.PortName, ValkeyPortName)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", 0, err
	}
	f.t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(spec.Server + "\n"))
			c.Close()
		}
	}()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids++
	port := ln.Addr().(*net.TCPAddr).Port
	n := &vkNode{server: spec.Server, ln: ln, port: port, replID: "id" + strconv.Itoa(f.ids)}
	f.nodes[spec.Server], f.byAddr[ln.Addr().String()] = n, n
	return ln.Addr().String(), "10.0.0." + strconv.Itoa(f.ids), port, nil
}

func (f *vkNodes) Remove(context.Context, string) error { return nil }

func (f *vkNodes) NodeHost(server string) string { return server + ".svc" }

func (f *vkNodes) Roles(_ context.Context, _ string, roles map[string]string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles = roles
	return nil
}

func (f *vkNodes) RemoveRoles(context.Context, string) error { return nil }

type vkConn struct {
	f    *vkNodes
	addr string
}

func (c vkConn) Do(_ context.Context, args ...any) (any, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	n := c.f.byAddr[c.addr]
	if n == nil || n.down {
		return nil, errors.New("connection refused")
	}
	cmd := strings.ToUpper(fmt.Sprint(args[0]))
	switch {
	case cmd == "REPLICAOF" && strings.EqualFold(fmt.Sprint(args[1]), "NO"):
		n.primary = ""
		c.f.ids++
		n.replID = "id" + strconv.Itoa(c.f.ids)
	case cmd == "REPLICAOF":
		n.primary = fmt.Sprint(args[1]) + ":" + fmt.Sprint(args[2])
	case cmd == "INFO":
		if n.primary == "" {
			return fmt.Sprintf("role:master\r\nmaster_replid:%s\r\nmaster_repl_offset:%d\r\nslave0:ip=x,port=1,state=online\r\nslave1:ip=x,port=1,state=online\r\n", n.replID, n.offset), nil
		}
		return fmt.Sprintf("role:slave\r\nmaster_link_status:up\r\nslave_repl_offset:%d\r\nmaster_replid:%s\r\n", n.offset, n.replID), nil
	}
	return "OK", nil
}

func (c vkConn) Pipe(ctx context.Context, cmds [][]any) ([]any, error) {
	out := make([]any, len(cmds))
	for i, cm := range cmds {
		var err error
		if out[i], err = c.Do(ctx, cm...); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (vkConn) Close() error { return nil }

func vkEndpoint(t *testing.T, inst *memorystorepb.Instance, kind memorystorepb.ConnectionType) (string, int32) {
	t.Helper()
	for _, p := range inst.GetPscAutoConnections() {
		if p.GetConnectionType() == kind {
			return p.GetIpAddress(), p.GetPort()
		}
	}
	t.Fatalf("no %v connection in %v", kind, inst)
	return "", 0
}

// TestValkeyClusterDisabledFailover (#1614): a CLUSTER_DISABLED
// instance's primary endpoint reaches the primary and its reader endpoint
// spreads connections over the replicas, each pod labelled with its role;
// when the primary stops answering, the replica with the most of its data
// is promoted, the others replicate it at its Service, the roles are
// labelled again, and the same primary endpoint reaches the new primary.
func TestValkeyClusterDisabledFailover(t *testing.T) {
	oldEvery, oldTend := healthEvery, tendEvery
	healthEvery, tendEvery = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { healthEvery, tendEvery = oldEvery, oldTend })
	s, h := newHA(t)
	f := &vkNodes{t: t, nodes: map[string]*vkNode{}, byAddr: map[string]*vkNode{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_ = h
	s.EnableValkey(ctx, f, func(addr, _, _ string) ClusterNode { return vkConn{f, addr} }, nil)
	ms := NewMemorystoreServer(s)
	name := haParent + "/instances/vk-one"
	if _, err := ms.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: haParent, InstanceId: "vk-one", Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(2), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: "test-project", Network: "projects/test-project/global/networks/default"}},
	}}); err != nil {
		t.Fatal(err)
	}
	var inst *memorystorepb.Instance
	for i := 0; i < 500; i++ {
		inst, _ = ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
		if inst.GetState() == memorystorepb.Instance_ACTIVE {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if inst.GetState() != memorystorepb.Instance_ACTIVE {
		t.Fatalf("instance %v", inst)
	}
	base := ValkeyServerBase(name)
	n0, n1, n2 := base+"-0", base+"-1", base+"-2"
	ph, pp := vkEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	rh, rp := vkEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_READER)
	if got := greet(t, ph, pp); got != n0 {
		t.Fatalf("the primary endpoint reaches %s, want %s", got, n0)
	}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		seen[greet(t, rh, rp)] = true
	}
	if len(seen) != 2 || !seen[n1] || !seen[n2] {
		t.Fatalf("the reader endpoint reaches %v, want both replicas", seen)
	}
	waitRoles := func(want map[string]string) {
		t.Helper()
		for i := 0; i < 500; i++ {
			f.mu.Lock()
			got := fmt.Sprint(f.roles)
			f.mu.Unlock()
			if got == fmt.Sprint(want) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("roles %v, want %v", f.roles, want)
	}
	waitRoles(map[string]string{n0: rolePrimary, n1: roleReplica, n2: roleReplica})

	f.mu.Lock()
	f.nodes[n2].offset = 10 // node 2 has more of the primary's data
	f.nodes[n0].down = true
	f.mu.Unlock()
	for i := 0; i < 500 && greetOrEmpty(ph, pp) != n2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := greet(t, ph, pp); got != n2 {
		t.Fatalf("after the primary failed the primary endpoint reaches %s, want %s", got, n2)
	}
	after, _ := ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	if h2, p2 := vkEndpoint(t, after, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY); h2 != ph || p2 != pp {
		t.Fatalf("the primary endpoint moved: %s:%d, was %s:%d", h2, p2, ph, pp)
	}
	waitRoles(map[string]string{n0: roleReplica, n1: roleReplica, n2: rolePrimary})
	f.mu.Lock()
	follows := f.nodes[n1].primary
	f.mu.Unlock()
	if want := n2 + ".svc:" + strconv.Itoa(f.nodes[n2].port); follows != want {
		t.Fatalf("node 1 replicates %q, want %q", follows, want)
	}

	// The old primary comes back as a primary of its own (its pod started
	// again): it is made a replica of the new one.
	f.mu.Lock()
	f.nodes[n0].down = false
	f.mu.Unlock()
	for i := 0; i < 500; i++ {
		f.mu.Lock()
		follows = f.nodes[n0].primary
		f.mu.Unlock()
		if follows != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if want := n2 + ".svc:" + strconv.Itoa(f.nodes[n2].port); follows != want {
		t.Fatalf("the old primary replicates %q, want %q", follows, want)
	}
}

// TestValkeyPrimaryStartedAgainFailsOver (#1614): a primary that answers
// with a replication ID of its own other than the last started again,
// its replicas' stream gone: a replica is promoted at once.
func TestValkeyPrimaryStartedAgainFailsOver(t *testing.T) {
	oldEvery, oldTend := healthEvery, tendEvery
	healthEvery, tendEvery = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { healthEvery, tendEvery = oldEvery, oldTend })
	s, _ := newHA(t)
	f := &vkNodes{t: t, nodes: map[string]*vkNode{}, byAddr: map[string]*vkNode{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.EnableValkey(ctx, f, func(addr, _, _ string) ClusterNode { return vkConn{f, addr} }, nil)
	ms := NewMemorystoreServer(s)
	name := haParent + "/instances/vk-two"
	if _, err := ms.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: haParent, InstanceId: "vk-two", Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(1), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: "test-project", Network: "projects/test-project/global/networks/default"}},
	}}); err != nil {
		t.Fatal(err)
	}
	base := ValkeyServerBase(name)
	for i := 0; i < 500; i++ {
		f.mu.Lock()
		labelled := f.roles[base+"-0"] == rolePrimary
		f.mu.Unlock()
		if labelled {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	f.nodes[base+"-0"].replID = "started-again"
	f.mu.Unlock()
	for i := 0; i < 500; i++ {
		f.mu.Lock()
		promoted := f.roles[base+"-1"] == rolePrimary && f.nodes[base+"-0"].primary != ""
		f.mu.Unlock()
		if promoted {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no failover after the primary started again: %v", f.roles)
}

func greetOrEmpty(host string, port int32) string {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))), time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 256)
	n, _ := c.Read(b)
	return strings.TrimSpace(string(b[:n]))
}
