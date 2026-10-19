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
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// vkNode is a fake CLUSTER_DISABLED node: a listener that greets each
// connection with its server's name, and its replication state.
type vkNode struct {
	server  string
	ln      net.Listener
	port    int
	primary string // "" when it is a primary, else the host:port it replicates
	replID  string
	runID   string
	offset  int64
	down    bool
	// syncing is a replica still loading its primary's dataset, its link
	// down, never synced since it started.
	syncing bool
}

// vkNodes runs the fake nodes, as a ClusterRuntime and RoleRouter.
type vkNodes struct {
	mu     sync.Mutex
	t      *testing.T
	nodes  map[string]*vkNode
	byAddr map[string]*vkNode
	roles  map[string]string // server -> role, as last labelled
	ids    int
	// hook, when set, runs before each command, outside mu: its error is
	// the command's (it may block, holding an operation where it is).
	hook func(server string, args []any) error
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
	n := &vkNode{server: spec.Server, ln: ln, port: port, replID: "id" + strconv.Itoa(f.ids), runID: "run" + strconv.Itoa(f.ids)}
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
	hook := c.f.hook
	if n := c.f.byAddr[c.addr]; hook != nil && n != nil {
		c.f.mu.Unlock()
		if err := hook(n.server, args); err != nil {
			return nil, err
		}
		c.f.mu.Lock()
	}
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
			// Two replicas online, or as many as there are: an upgrade's
			// primary has its own and the new nodes' (settle).
			slaves, replicas := "", 0
			for _, o := range c.f.nodes {
				if o.primary != "" {
					replicas++
				}
			}
			for i := 0; i < max(2, replicas); i++ {
				slaves += fmt.Sprintf("slave%d:ip=x,port=1,state=online\r\n", i)
			}
			return fmt.Sprintf("role:master\r\nmaster_replid:%s\r\nmaster_repl_offset:%d\r\n%srun_id:%s\r\n", n.replID, n.offset, slaves, n.runID), nil
		}
		host, port, _ := net.SplitHostPort(n.primary)
		link := "master_link_status:up\r\nmaster_sync_in_progress:0\r\n"
		if n.syncing {
			link = "master_link_status:down\r\nmaster_sync_in_progress:1\r\nmaster_link_down_since_seconds:-1\r\n"
		}
		return fmt.Sprintf("role:slave\r\nmaster_host:%s\r\nmaster_port:%s\r\n%sslave_repl_offset:%d\r\nmaster_replid:%s\r\nrun_id:%s\r\n",
			host, port, link, n.offset, n.replID, n.runID), nil
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
// with a run_id and replication ID of its own other than the last
// started again, its replicas' stream gone: a replica is promoted at once.
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
	f.nodes[base+"-0"].replID, f.nodes[base+"-0"].runID = "started-again", "run-again"
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

// TestValkeySecondaryFollowsFailover (#1641): a secondary of cross-
// instance replication, linked to its primary instance's primary Service,
// is re-pointed at the promoted node's own Service when the primary
// instance fails over, since its link to the primary Service can hang on
// the failed pod for repl-timeout; and one a failover could not re-point
// is re-pointed by the next tending of the roles.
func TestValkeySecondaryFollowsFailover(t *testing.T) {
	oldEvery, oldTend := healthEvery, tendEvery
	healthEvery, tendEvery = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { healthEvery, tendEvery = oldEvery, oldTend })
	s, _ := newHA(t)
	f := &vkNodes{t: t, nodes: map[string]*vkNode{}, byAddr: map[string]*vkNode{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.EnableValkey(ctx, f, func(addr, _, _ string) ClusterNode { return vkConn{f, addr} }, nil)
	ms := NewMemorystoreServer(s)
	psc := []*memorystorepb.PscAutoConnection{{ProjectId: "test-project", Network: "projects/test-project/global/networks/default"}}
	create := func(id string, in *memorystorepb.Instance) string {
		t.Helper()
		in.Mode, in.NodeType, in.PscAutoConnections = memorystorepb.Instance_CLUSTER_DISABLED, memorystorepb.Instance_SHARED_CORE_NANO, psc
		if _, err := ms.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: haParent, InstanceId: id, Instance: in}); err != nil {
			t.Fatal(err)
		}
		name := haParent + "/instances/" + id
		for i := 0; i < 500; i++ {
			if inst, _ := ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name}); inst.GetState() == memorystorepb.Instance_ACTIVE {
				return name
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("instance %s is not ACTIVE", id)
		return ""
	}
	pname := create("vk-cirp", &memorystorepb.Instance{ReplicaCount: proto.Int32(1)})
	sname := create("vk-cirs", &memorystorepb.Instance{CrossInstanceReplicationConfig: &memorystorepb.CrossInstanceReplicationConfig{
		InstanceRole:    memorystorepb.CrossInstanceReplicationConfig_SECONDARY,
		PrimaryInstance: &memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{Instance: pname}}})
	pbase, sn := ValkeyServerBase(pname), ValkeyServerBase(sname)+"-0"
	follows := func() string {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.nodes[sn].primary
	}
	waitFollows := func(what, want string) {
		t.Helper()
		for i := 0; i < 500 && follows() != want; i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if got := follows(); got != want {
			t.Fatalf("%s: the secondary replicates %q, want %q", what, got, want)
		}
	}
	waitFollows("linked", pbase+"-primary.svc:6379")

	f.mu.Lock()
	f.nodes[pbase+"-0"].down = true
	promoted := pbase + "-1.svc:" + strconv.Itoa(f.nodes[pbase+"-1"].port)
	f.mu.Unlock()
	waitFollows("after the failover", promoted)

	// A secondary the failover could not reach is re-pointed once it
	// answers.
	f.mu.Lock()
	f.nodes[sn].primary = pbase + "-0.svc:" + strconv.Itoa(f.nodes[pbase+"-0"].port)
	f.mu.Unlock()
	waitFollows("tended", promoted)
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

// TestValkeySuspendedIsNotFailedOver (#1614, CMEK): a CLUSTER_DISABLED
// instance whose customer-managed key cannot be used is not failed over:
// while its key is unusable its primary failing promotes no replica, and
// once it is suspended (its nodes stopped) it reports no endpoints and
// its primary endpoint carries nothing. Resumed, on the same endpoint
// addresses, the endpoints reach the primary and replicas again and the
// roles are labelled again.
func TestValkeySuspendedIsNotFailedOver(t *testing.T) {
	oldEvery, oldTend := healthEvery, tendEvery
	healthEvery, tendEvery = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { healthEvery, tendEvery = oldEvery, oldTend })
	s, _ := newHA(t)
	f := &vkNodes{t: t, nodes: map[string]*vkNode{}, byAddr: map[string]*vkNode{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.EnableValkey(ctx, f, func(addr, _, _ string) ClusterNode { return vkConn{f, addr} }, nil)
	ms := NewMemorystoreServer(s)
	name := haParent + "/instances/vk-cmek"
	if _, err := ms.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: haParent, InstanceId: "vk-cmek", Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(1), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: "test-project", Network: "projects/test-project/global/networks/default"}},
	}}); err != nil {
		t.Fatal(err)
	}
	base := ValkeyServerBase(name)
	n0, n1 := base+"-0", base+"-1"
	rolesAre := func(want map[string]string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return fmt.Sprint(f.roles) == fmt.Sprint(want)
	}
	initial := map[string]string{n0: rolePrimary, n1: roleReplica}
	for i := 0; i < 500 && !rolesAre(initial); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	inst, _ := ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	ph, pp := vkEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	if got := greet(t, ph, pp); got != n0 {
		t.Fatalf("the primary endpoint reaches %s, want %s", got, n0)
	}
	c := s.valkey
	noFailover := func(what string) {
		t.Helper()
		time.Sleep(20 * healthEvery * time.Duration(failAfter))
		c.mu.Lock()
		r := c.byName[name]
		promoted := r.Nodes[1].Primary
		_, busy := c.busy[name]
		c.mu.Unlock()
		f.mu.Lock()
		follows := f.nodes[n1].primary
		f.mu.Unlock()
		if promoted || busy || follows == "" {
			t.Fatalf("%s: the replica was promoted (record %v, busy %v, replicates %q)", what, promoted, busy, follows)
		}
	}

	// The key cannot be used, the suspension not begun: the primary
	// failing promotes no replica.
	c.mu.Lock()
	c.byName[name].KeyState = "DISABLED"
	c.mu.Unlock()
	f.mu.Lock()
	f.nodes[n0].down = true
	f.mu.Unlock()
	noFailover("key unusable")

	// Suspended, its nodes stopped (clustercmek.go suspendCluster).
	c.mu.Lock()
	r := c.byName[name]
	r.suspended, r.state = true, 0
	addrs := map[string]string{}
	for _, n := range r.Nodes {
		addrs[n.Server] = n.addr
		n.addr, n.podIP, n.port = "", "", 0
	}
	c.mu.Unlock()
	noFailover("suspended")
	got, _ := ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	if len(got.GetPscAutoConnections()) != 0 {
		t.Fatalf("a suspended instance reports %v", got.GetPscAutoConnections())
	}
	if g := greetOrEmpty(ph, pp); g != "" {
		t.Fatalf("a suspended instance's primary endpoint reaches %q", g)
	}

	// Resumed (resumeCluster): its endpoints, at the same addresses,
	// reach its nodes, its roles labelled again.
	f.mu.Lock()
	f.nodes[n0].down = false
	f.roles = nil
	f.mu.Unlock()
	c.mu.Lock()
	r.suspended, r.KeyState, r.state = false, "", clusterpb.Cluster_ACTIVE
	for _, n := range r.Nodes {
		n.addr = addrs[n.Server]
		_, p, _ := net.SplitHostPort(n.addr)
		n.port, _ = strconv.Atoi(p)
	}
	if e := c.eps[name]; e != nil {
		e.labelled = ""
	}
	c.openEndpoints(r)
	c.mu.Unlock()
	got, _ = ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	if h, p := vkEndpoint(t, got, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY); h != ph || p != pp {
		t.Fatalf("resumed: the primary endpoint moved to %s:%d, was %s:%d", h, p, ph, pp)
	}
	if g := greet(t, ph, pp); g != n0 {
		t.Fatalf("resumed: the primary endpoint reaches %q, want %s", g, n0)
	}
	rh, rp := vkEndpoint(t, got, memorystorepb.ConnectionType_CONNECTION_TYPE_READER)
	if g := greet(t, rh, rp); g != n1 {
		t.Fatalf("resumed: the reader endpoint reaches %q, want %s", g, n1)
	}
	for i := 0; i < 500 && !rolesAre(initial); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !rolesAre(initial) {
		t.Fatalf("resumed: roles %v, want %v", f.roles, initial)
	}
}

// TestValkeySwitchoverFencesHealthCheck (#1642): while a cross-instance
// switchover re-points its members' nodes, the health check watches none
// of them, so the former primary, whose primary node is being made a
// replica of the new primary, is not failed over; the marks clear when
// the switchover ends, and when it fails. A switchover is refused while
// a member is failing over.
func TestValkeySwitchoverFencesHealthCheck(t *testing.T) {
	oldEvery, oldTend := healthEvery, tendEvery
	healthEvery, tendEvery = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { healthEvery, tendEvery = oldEvery, oldTend })
	s, _ := newHA(t)
	f := &vkNodes{t: t, nodes: map[string]*vkNode{}, byAddr: map[string]*vkNode{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.EnableValkey(ctx, f, func(addr, _, _ string) ClusterNode { return vkConn{f, addr} }, nil)
	ms := NewMemorystoreServer(s)
	c := s.valkey
	psc := []*memorystorepb.PscAutoConnection{{ProjectId: "test-project", Network: "projects/test-project/global/networks/default"}}
	waitActive := func(name string) {
		t.Helper()
		for i := 0; i < 500; i++ {
			if inst, _ := ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name}); inst.GetState() == memorystorepb.Instance_ACTIVE {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("instance %s is not ACTIVE", name)
	}
	create := func(id string, in *memorystorepb.Instance) string {
		t.Helper()
		in.Mode, in.NodeType, in.PscAutoConnections = memorystorepb.Instance_CLUSTER_DISABLED, memorystorepb.Instance_SHARED_CORE_NANO, psc
		in.ReplicaCount = proto.Int32(1)
		if _, err := ms.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: haParent, InstanceId: id, Instance: in}); err != nil {
			t.Fatal(err)
		}
		name := haParent + "/instances/" + id
		waitActive(name)
		return name
	}
	pname := create("vk-swp", &memorystorepb.Instance{})
	sname := create("vk-sws", &memorystorepb.Instance{CrossInstanceReplicationConfig: &memorystorepb.CrossInstanceReplicationConfig{
		InstanceRole:    memorystorepb.CrossInstanceReplicationConfig_SECONDARY,
		PrimaryInstance: &memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{Instance: pname}}})
	pbase, sbase := ValkeyServerBase(pname), ValkeyServerBase(sname)
	for i := 0; i < 500; i++ {
		f.mu.Lock()
		linked := f.nodes[sbase+"-0"].primary != "" && f.roles[pbase+"-0"] == rolePrimary
		f.mu.Unlock()
		if linked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	switchTo := func(primary, secondary string) error {
		_, err := ms.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cross_instance_replication_config"}},
			Instance: &memorystorepb.Instance{Name: primary, CrossInstanceReplicationConfig: &memorystorepb.CrossInstanceReplicationConfig{
				InstanceRole:       memorystorepb.CrossInstanceReplicationConfig_PRIMARY,
				SecondaryInstances: []*memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{{Instance: secondary}}}}})
		return err
	}
	busy := func() (bool, bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, p := c.busy[pname]
		_, s := c.busy[sname]
		return p, s
	}
	idle := func(what string) {
		t.Helper()
		for i := 0; i < 500; i++ {
			if p, s := busy(); !p && !s {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		p, s := busy()
		t.Fatalf("%s: busy marks left: primary %v, secondary %v", what, p, s)
	}

	// A member failing over: the switchover is refused.
	c.mu.Lock()
	c.busy[pname] = autoFailover
	c.mu.Unlock()
	if err := switchTo(sname, pname); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a switchover while the primary instance fails over: %v, want FAILED_PRECONDITION", err)
	}
	c.mu.Lock()
	delete(c.busy, pname)
	c.mu.Unlock()

	// The switchover fails re-pointing the former primary: both marks
	// clear, and the replication is as it was.
	f.mu.Lock()
	f.hook = func(server string, args []any) error {
		if server == pbase+"-0" && strings.EqualFold(fmt.Sprint(args[0]), "REPLICAOF") && !strings.EqualFold(fmt.Sprint(args[1]), "NO") {
			return errors.New("refused")
		}
		return nil
	}
	f.mu.Unlock()
	if err := switchTo(sname, pname); err != nil {
		t.Fatal(err)
	}
	idle("failed switchover")
	c.mu.Lock()
	role := c.byName[pname].Valkey.role()
	c.mu.Unlock()
	if role != memorystorepb.CrossInstanceReplicationConfig_PRIMARY.String() {
		t.Fatalf("after a failed switchover the former primary is %s", role)
	}
	// Re-link the secondary, as the next switchover expects it.
	f.mu.Lock()
	f.hook = nil
	f.nodes[sbase+"-0"].primary = pbase + "-primary.svc:6379"
	f.mu.Unlock()

	// The switchover holds while the former primary's primary node is
	// re-pointed, and meanwhile that node answers as nothing: the former
	// primary is not failed over.
	hold, held := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.mu.Lock()
	f.hook = func(server string, args []any) error {
		if server == pbase+"-0" && strings.EqualFold(fmt.Sprint(args[0]), "REPLICAOF") && !strings.EqualFold(fmt.Sprint(args[1]), "NO") {
			once.Do(func() { close(held) })
			<-hold
		}
		return nil
	}
	f.mu.Unlock()
	if err := switchTo(sname, pname); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the switchover did not re-point the former primary")
	}
	if p, s := busy(); !p || !s {
		t.Fatalf("during the switchover: busy primary %v, secondary %v; want both", p, s)
	}
	f.mu.Lock()
	f.nodes[pbase+"-0"].down = true
	f.mu.Unlock()
	time.Sleep(20 * healthEvery * time.Duration(failAfter))
	c.mu.Lock()
	promoted := c.byName[pname].Nodes[1].Primary
	c.mu.Unlock()
	f.mu.Lock()
	follows := f.nodes[pbase+"-1"].primary
	f.nodes[pbase+"-0"].down = false
	f.mu.Unlock()
	if promoted || follows == "" {
		t.Fatalf("the former primary was failed over during the switchover (record %v, replica replicates %q)", promoted, follows)
	}
	close(hold)
	idle("switchover")
	waitActive(sname)
	c.mu.Lock()
	prole, srole := c.byName[pname].Valkey.role(), c.byName[sname].Valkey.role()
	c.mu.Unlock()
	if prole != memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String() || srole != memorystorepb.CrossInstanceReplicationConfig_PRIMARY.String() {
		t.Fatalf("after the switchover: former primary %s, former secondary %s", prole, srole)
	}
}

// TestValkeySwitchoverCheckLeavesEndpointOpen (#1652): a health check of
// the former primary that began before a switchover and ends after it,
// seeing the primary node answer as a replica of the new primary, does
// not fence the former primary's primary endpoint: the instance is a
// secondary by then, never checked again, so a fence would make its
// primary endpoint close every connection (EOF) for good. Its primary
// endpoint still reaches its primary node.
func TestValkeySwitchoverCheckLeavesEndpointOpen(t *testing.T) {
	oldEvery, oldTend := healthEvery, tendEvery
	healthEvery, tendEvery = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { healthEvery, tendEvery = oldEvery, oldTend })
	s, _ := newHA(t)
	f := &vkNodes{t: t, nodes: map[string]*vkNode{}, byAddr: map[string]*vkNode{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.EnableValkey(ctx, f, func(addr, _, _ string) ClusterNode { return vkConn{f, addr} }, nil)
	ms := NewMemorystoreServer(s)
	c := s.valkey
	psc := []*memorystorepb.PscAutoConnection{{ProjectId: "test-project", Network: "projects/test-project/global/networks/default"}}
	get := func(name string) *memorystorepb.Instance {
		t.Helper()
		for i := 0; i < 500; i++ {
			if inst, _ := ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name}); inst.GetState() == memorystorepb.Instance_ACTIVE {
				return inst
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("instance %s is not ACTIVE", name)
		return nil
	}
	create := func(id string, in *memorystorepb.Instance) *memorystorepb.Instance {
		t.Helper()
		in.Mode, in.NodeType, in.PscAutoConnections = memorystorepb.Instance_CLUSTER_DISABLED, memorystorepb.Instance_SHARED_CORE_NANO, psc
		in.ReplicaCount = proto.Int32(1)
		if _, err := ms.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: haParent, InstanceId: id, Instance: in}); err != nil {
			t.Fatal(err)
		}
		return get(haParent + "/instances/" + id)
	}
	pinst := create("vk-swe-p", &memorystorepb.Instance{})
	pname := pinst.GetName()
	sname := create("vk-swe-s", &memorystorepb.Instance{CrossInstanceReplicationConfig: &memorystorepb.CrossInstanceReplicationConfig{
		InstanceRole:    memorystorepb.CrossInstanceReplicationConfig_SECONDARY,
		PrimaryInstance: &memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{Instance: pname}}}).GetName()
	p0 := ValkeyServerBase(pname) + "-0"
	ph, pp := vkEndpoint(t, pinst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	if got := greet(t, ph, pp); got != p0 {
		t.Fatalf("the primary endpoint reaches %s, want %s", got, p0)
	}

	// The next health check of the primary holds until the switchover is
	// done; it then finds the primary node a replica of the new primary.
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.mu.Lock()
	f.hook = func(server string, args []any) error {
		if server != p0 || !strings.EqualFold(fmt.Sprint(args[0]), "INFO") {
			return nil
		}
		first := false
		once.Do(func() { first = true })
		if first {
			close(held)
			<-release
		}
		return nil
	}
	f.mu.Unlock()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("no health check of the primary")
	}
	if _, err := ms.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cross_instance_replication_config"}},
		Instance: &memorystorepb.Instance{Name: sname, CrossInstanceReplicationConfig: &memorystorepb.CrossInstanceReplicationConfig{
			InstanceRole:       memorystorepb.CrossInstanceReplicationConfig_PRIMARY,
			SecondaryInstances: []*memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{{Instance: pname}}}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		c.mu.Lock()
		role, busy := c.byName[pname].Valkey.role(), c.busy[sname] != ""
		c.mu.Unlock()
		if role == memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String() && !busy {
			break
		}
		if i == 500 {
			t.Fatalf("the former primary is %s after the switchover", role)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	time.Sleep(20 * healthEvery)
	c.mu.Lock()
	fenced := c.eps[pname].fenced
	c.mu.Unlock()
	if got := greetOrEmpty(ph, pp); fenced || got != p0 {
		t.Fatalf("after the switchover the former primary's primary endpoint reaches %q (fenced %v), want %s", got, fenced, p0)
	}

	// A fence left on the secondary by any other way is lifted by the
	// next health check, which finds it unwatched and idle.
	c.mu.Lock()
	c.eps[pname].fenced = true
	c.openEndpoints(c.byName[pname])
	c.mu.Unlock()
	for i := 0; ; i++ {
		got := greetOrEmpty(ph, pp)
		if got == p0 {
			break
		}
		if i == 100 {
			t.Fatalf("a fenced secondary's primary endpoint reaches %q, want %s", got, p0)
		}
		time.Sleep(healthEvery)
	}
}
