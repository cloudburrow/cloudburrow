package redisadmin

import (
	"context"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
)

// The endpoints of a Memorystore for Valkey instance in CLUSTER_DISABLED
// mode, and its automatic failover (#1614). Google documents, for these
// instances: "Connect to the reader endpoint to connect to any of the
// available replicas", and, when the primary fails, "a replica is
// promoted to be the primary"; "Automatic failovers can take time on the
// order of tens of seconds", and "Client connections are likely to be
// reset". It documents no failover method for these instances (the
// memorystore.v1 API has none), so there is none to serve.
//
// Each node is a server of its own, its tunnel bound to its pod, so an
// endpoint that outlives a node is built of three things:
//
//   - in the cluster, a Service per endpoint, the instance's server base
//     and "-primary" or "-reader", selecting the instance's pods by the
//     role label (cloudburrow.dev/memorystore-role primary or replica)
//     each node's pod carries (RoleRouter); kube-proxy spreads the
//     reader Service's connections over the replicas;
//   - on the host, the primary and reader endpoints the instance reports:
//     addresses that stay the same while the nodes behind them change, as
//     a Standard tier Redis instance's do (endpoint.go). The primary
//     endpoint carries each connection to the node the primary Service
//     selects, the reader endpoint spreads connections over the ones the
//     reader Service selects, one each in turn, through the nodes'
//     tunnels, which follow their pods as they restart;
//   - a health check of the primary, as a Standard tier instance's
//     (autofailover.go): INFO replication every healthEvery. The primary
//     has failed once it has not answered as a primary for failAfter
//     checks in a row, or at once when it answers with a replication ID
//     of its own other than the last (its server started again, empty or
//     from its files, so its replicas' stream is gone). The replica with
//     the most of its data (bestReplica) is promoted: REPLICAOF NO ONE,
//     then every other node, the old primary among them, REPLICAOF it at
//     its Service; the roles are labelled again and the primary
//     endpoint's connections are dropped, so a client reconnects to the
//     same address and reaches the new primary. Acknowledged writes the
//     replicas had not received are lost, as replication is asynchronous.
//
// The same check, every tendEvery, makes a node that should be a replica
// and answers as a primary (its pod started again) a replica of the
// primary, and labels each pod with its role again, a new pod having
// none. CLUSTER mode is unchanged: its nodes fail over among themselves.

// RoleRouter is a ClusterRuntime that publishes a CLUSTER_DISABLED
// instance's roles in the cluster.
type RoleRouter interface {
	// NodeHost is where the other nodes reach server in the cluster: its
	// Service's name, which follows its pod.
	NodeHost(server string) string
	// Roles labels the pod of each server of roles with its role,
	// rolePrimary or roleReplica, and applies the instance's primary and
	// reader Services, base+"-primary" and base+"-reader", selecting them
	// on port (the nodes' port is named in their pods, ValkeyPortName).
	Roles(ctx context.Context, base string, roles map[string]string, port int) error
	// RemoveRoles deletes the instance's primary and reader Services.
	RemoveRoles(ctx context.Context, base string) error
}

const (
	rolePrimary = "primary"
	roleReplica = "replica"
	// ValkeyPortName names a CLUSTER_DISABLED node's port in its pod, which
	// the role Services select: each node serves on a port of its own.
	ValkeyPortName = "valkey"
)

// tendEvery is how often the roles are labelled again and a node that
// came back as a primary is made a replica.
var tendEvery = 5 * time.Second

// roleEndpoints are a CLUSTER_DISABLED instance's endpoints on the host.
type roleEndpoints struct {
	base            string
	primary, reader *endpoint
	primaryUp       string
	readerUps       string
	// labelled is the roles last labelled, and when.
	labelled   string
	labelledAt time.Time
}

func (e *roleEndpoints) close() {
	e.primary.close()
	e.reader.close()
}

// ValkeyServerBase is the base of the names of a Memorystore for Valkey
// instance's servers: each node's server is it and "-" and the node's
// number, and in CLUSTER_DISABLED mode its primary and reader Services
// are it and "-primary" and "-reader".
func ValkeyServerBase(name string) string {
	s := clusterServerName(name, 0)
	return s[:strings.LastIndex(s, "-")]
}

// openEndpoints opens r's endpoints, when r is a CLUSTER_DISABLED
// instance whose primary serves, and points them at its nodes: the
// primary endpoint at the primary, dropping its connections when the
// primary changed, the reader endpoint at the replicas. Call with mu held.
func (c *clusters) openEndpoints(r *clusterRecord) {
	if !c.valkey || !r.standalone() || r.state == clusterpb.Cluster_CREATING {
		return
	}
	p := r.firstPrimary()
	if p == nil || p.addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(p.addr)
	if err != nil {
		return
	}
	if c.eps == nil {
		c.eps = map[string]*roleEndpoints{}
	}
	e := c.eps[r.Name]
	if e == nil {
		e = &roleEndpoints{base: ValkeyServerBase(r.Name)}
		if e.primary, err = listen(host); err != nil {
			log.Printf("memorystore: %s: open the primary endpoint: %v", r.Name, err)
			return
		}
		if e.reader, err = listen(host); err != nil {
			e.primary.close()
			log.Printf("memorystore: %s: open the reader endpoint: %v", r.Name, err)
			return
		}
		c.eps[r.Name] = e
	}
	if e.primaryUp != p.addr {
		e.primary.set([]string{p.addr}, e.primaryUp != "")
		e.primaryUp = p.addr
	}
	var ups []string
	for _, n := range r.Nodes {
		if !n.Primary && n.addr != "" {
			ups = append(ups, n.addr)
		}
	}
	if j := strings.Join(ups, ","); j != e.readerUps {
		e.reader.set(ups, false)
		e.readerUps = j
	}
	r.primaryAddr, r.readerAddr = e.primary.Addr(), e.reader.Addr()
}

// nodeReplicator is a node of a cluster as a Replicator.
type nodeReplicator struct {
	c *clusters
	r *clusterRecord
	n *clusterNode
}

func (nr nodeReplicator) ReplicaOf(ctx context.Context, host string, port int) error {
	if host == "" {
		return nr.c.do(ctx, nr.r, nr.n, "REPLICAOF", "NO", "ONE")
	}
	return nr.c.do(ctx, nr.r, nr.n, "REPLICAOF", host, port)
}

func (nr nodeReplicator) Replication(ctx context.Context) (Replication, error) {
	cl := nr.c.dial(nr.n.addr, nr.r.password(), nr.r.TransitCA)
	defer cl.Close()
	v, err := cl.Do(ctx, "INFO", "replication")
	if err != nil {
		return Replication{}, err
	}
	return parseReplication(fmt.Sprint(v)), nil
}

// nodeHost is where the other nodes reach n: its Service, which follows
// its pod, or its pod's IP without a RoleRouter.
func (c *clusters) nodeHost(n *clusterNode) string {
	if rr, ok := c.rt.(RoleRouter); ok {
		return rr.NodeHost(n.Server)
	}
	return n.podIP
}

// watched is whether the health check looks after r: a CLUSTER_DISABLED
// instance that is ACTIVE, with a replica, no operation of its own
// running, and its primary its own (not a secondary of cross-instance
// replication or a migration's target, whose primary replicates another
// server). Call with mu held.
func (c *clusters) watched(r *clusterRecord) bool {
	if _, pending := c.busy[r.Name]; pending || !r.standalone() || r.state != clusterpb.Cluster_ACTIVE || r.suspended {
		return false
	}
	if v := r.Valkey; (v.Migration.migrating() && v.Migration.State != memorystorepb.MigrationConfig_ROLLED_BACK.String()) ||
		v.role() == memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String() {
		return false
	}
	p := r.firstPrimary()
	return p != nil && p.addr != "" && len(r.Nodes) > 1
}

// watchStandalone checks every CLUSTER_DISABLED instance's primary until
// ctx ends.
func (c *clusters) watchStandalone(ctx context.Context, every time.Duration) {
	tend := tendEvery
	failed := map[string]int{}
	ids := map[string]string{}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			for name, e := range c.eps {
				e.close()
				delete(c.eps, name)
			}
			c.mu.Unlock()
			return
		case <-t.C:
		}
		for _, name := range c.checkStandalone(ctx, failed, ids, every, tend) {
			delete(failed, name)
			delete(ids, name)
			c.failoverStandalone(ctx, name)
		}
	}
}

// checkStandalone closes the endpoints of instances that are gone, counts
// the failed checks of each watched instance's primary in failed, tends
// the roles, and returns the instances whose primary failed.
func (c *clusters) checkStandalone(ctx context.Context, failed map[string]int, ids map[string]string, every, tend time.Duration) []string {
	type target struct {
		name string
		rec  clusterRecord
		p    clusterNode
	}
	var ts []target
	c.mu.Lock()
	for name, e := range c.eps {
		if r, ok := c.byName[name]; !ok || !r.standalone() {
			e.close()
			delete(c.eps, name)
			if rr, ok := c.rt.(RoleRouter); ok {
				go func(base string) {
					rctx, cancel := context.WithTimeout(c.bg, time.Minute)
					defer cancel()
					_ = rr.RemoveRoles(rctx, base)
				}(e.base)
			}
		}
	}
	for name, r := range c.byName {
		if !c.watched(r) {
			delete(failed, name)
			delete(ids, name)
			continue
		}
		c.openEndpoints(r)
		ts = append(ts, target{name, *r, *r.firstPrimary()})
	}
	c.mu.Unlock()
	var out []string
	for _, tg := range ts {
		cctx, cancel := context.WithTimeout(ctx, every)
		st, err := nodeReplicator{c, &tg.rec, &tg.p}.Replication(cctx)
		cancel()
		switch {
		case err == nil && st.Primary && (ids[tg.name] == "" || ids[tg.name] == st.ReplID):
			delete(failed, tg.name)
			ids[tg.name] = st.ReplID
			c.tendRoles(ctx, tg.name, tend)
			continue
		case err == nil && st.Primary:
			// Started again: its replicas' stream is gone.
			log.Printf("memorystore: %s: the primary %s started again (replication ID %s, was %s)", tg.name, tg.p.Server, st.ReplID, ids[tg.name])
			failed[tg.name] = failAfter
		default:
			failed[tg.name]++
		}
		if failed[tg.name] >= failAfter {
			out = append(out, tg.name)
		}
	}
	return out
}

// tendRoles, every tend or when the roles changed, makes each node
// of name that should be a replica and answers as a primary a replica of
// the primary, and labels the nodes' pods with their roles.
func (c *clusters) tendRoles(ctx context.Context, name string, tend time.Duration) {
	c.mu.Lock()
	r, ok := c.byName[name]
	e := c.eps[name]
	if !ok || e == nil || !c.watched(r) {
		c.mu.Unlock()
		return
	}
	roles := map[string]string{}
	var keys []string
	for _, n := range r.Nodes {
		roles[n.Server] = roleReplica
		if n.Primary {
			roles[n.Server] = rolePrimary
		}
		keys = append(keys, n.Server+"="+roles[n.Server])
	}
	sort.Strings(keys)
	sig := strings.Join(keys, ",")
	if sig == e.labelled && time.Since(e.labelledAt) < tend {
		c.mu.Unlock()
		return
	}
	e.labelled, e.labelledAt = sig, time.Now()
	rec := *r
	nodes := make([]clusterNode, len(r.Nodes))
	for i, n := range r.Nodes {
		nodes[i] = *n
	}
	p := *r.firstPrimary()
	c.mu.Unlock()
	for i := range nodes {
		n := &nodes[i]
		if n.Primary || n.addr == "" {
			continue
		}
		nr := nodeReplicator{c, &rec, n}
		if st, err := nr.Replication(ctx); err == nil && st.Primary {
			log.Printf("memorystore: %s: %s answers as a primary; making it a replica of %s", name, n.Server, p.Server)
			if err := nr.ReplicaOf(ctx, c.nodeHost(&p), p.port); err != nil {
				log.Printf("memorystore: %s: %s: REPLICAOF: %v", name, n.Server, err)
			}
		}
	}
	if rr, ok := c.rt.(RoleRouter); ok {
		if err := rr.Roles(ctx, e.base, roles, rolePort(&rec)); err != nil {
			log.Printf("memorystore: %s: label the roles: %v", name, err)
			c.mu.Lock()
			e.labelled = ""
			c.mu.Unlock()
		}
	}
}

// rolePort is the port of r's role Services: 6378 with in-transit
// encryption, as Memorystore serves TLS, else 6379.
func rolePort(r *clusterRecord) int {
	if r.TransitCA != "" {
		return TLSPort
	}
	return 6379
}

// failoverStandalone promotes the replica of name with the most of its
// failed primary's data.
func (c *clusters) failoverStandalone(ctx context.Context, name string) {
	c.mu.Lock()
	r, ok := c.byName[name]
	if !ok || !c.watched(r) {
		c.mu.Unlock()
		return
	}
	c.busy[name] = autoFailover
	rec := *r
	nodes := make([]clusterNode, len(r.Nodes))
	old := -1
	var replicas []int
	for i, n := range r.Nodes {
		nodes[i] = *n
		if n.Primary {
			old = i
		} else {
			replicas = append(replicas, i)
		}
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.busy, name)
		c.mu.Unlock()
	}()
	log.Printf("memorystore: %s: the primary %s failed its health checks; failing over", name, nodes[old].Server)
	fctx, cancel := context.WithTimeout(ctx, haWait)
	defer cancel()
	best, _, err := bestReplica(fctx, replicas, func(i int) (Replicator, error) {
		return nodeReplicator{c, &rec, &nodes[i]}, nil
	}, 0, false)
	if err != nil {
		// No replica answers either: the next checks try again.
		log.Printf("memorystore: %s: automatic failover: %v", name, err)
		return
	}
	if err := (nodeReplicator{c, &rec, &nodes[best]}).ReplicaOf(fctx, "", 0); err != nil {
		log.Printf("memorystore: %s: automatic failover: %s: REPLICAOF NO ONE: %v", name, nodes[best].Server, err)
		return
	}
	host, port := c.nodeHost(&nodes[best]), nodes[best].port
	for i := range nodes {
		if i == best {
			continue
		}
		// The old primary, while its pod is gone, follows once it is back
		// (tendRoles).
		if err := (nodeReplicator{c, &rec, &nodes[i]}).ReplicaOf(fctx, host, port); err != nil && i != old {
			log.Printf("memorystore: %s: automatic failover: %s: REPLICAOF: %v", name, nodes[i].Server, err)
		}
	}
	c.mu.Lock()
	if r, ok = c.byName[name]; ok {
		for _, n := range r.Nodes {
			n.Primary = n.Server == nodes[best].Server
		}
		if err := c.save(r); err != nil {
			log.Printf("memorystore: %s: keep the instance: %v", name, err)
		}
		c.openEndpoints(r)
		if e := c.eps[name]; e != nil {
			e.labelled = ""
		}
	}
	c.mu.Unlock()
	log.Printf("memorystore: %s: automatic failover promoted %s", name, nodes[best].Server)
}
