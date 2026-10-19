package redisadmin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
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
//     (autofailover.go): INFO replication and server every healthEvery.
//     The primary has failed once it has not answered as a primary for
//     failAfter checks in a row, or at once when it answers with a run_id
//     other than the last (its server started again, empty or from its
//     files, so its replicas' stream is gone). A new replication ID with
//     the same run_id is not a failure (#1657): a primary takes one when
//     it is promoted, and again when it gains a replica with no backlog to
//     serve it from, which a promoted node often does a second after the
//     old primary's node rejoins it. The replica with the most of its
//     data among those fit to promote (promotable) is promoted: REPLICAOF
//     NO ONE, then every other node, the old primary among them,
//     REPLICAOF it at its Service; the roles are labelled again and the
//     primary endpoint's connections are dropped, so a client reconnects
//     to the same address and reaches the new primary. From the first
//     failed check until the primary answers again as the primary it was,
//     or a replica is promoted, the primary endpoint carries no
//     connections (fence): a pod started again, empty, comes back faster
//     than three checks, and a write to it would be lost (measured in
//     TestMemorystoreValkeyReaderAndFailover). Acknowledged writes the
//     replicas had not received are lost, as replication is asynchronous.
//
// Google documents only that "the system detects the failure, and elects
// a replica to be the new primary", and that "acknowledged writes may be
// lost" (https://docs.cloud.google.com/memorystore/docs/valkey/ha-and-replicas,
// re-read 2026-10-19); nothing there, nor in
// https://docs.cloud.google.com/memorystore/docs/valkey/about-maintenance
// (re-read the same day), says how the replica is chosen. So the choice
// follows Valkey's own failover, which passes over a replica whose data is
// too old: a replica is promotable (#1657) unless it still loads the
// primary's dataset (master_sync_in_progress:1), has not synced since it
// started (its link down, master_link_down_since_seconds -1: a node that
// rejoined empty), lost the primary well before the primary failed, or
// is limitedDataLoss or more behind the replica with the most. With none
// promotable there is no failover; the checks go on, and a primary that
// started again with none promotable is kept, as no replica has more of
// its data. After a failover, for settleFor, the instance settles: it is
// failed over again only when the new primary stops answering or starts
// again, not when it answers as a replica or (without a run_id) with a
// new replication ID.
//
// A secondary of cross-instance replication of the instance is then
// made a replica of the promoted node at its own Service (followersOf
// says why not through the primary Service).
//
// The same check, every tendEvery, makes a node that should be a replica
// and answers as a primary (its pod started again) a replica of the
// primary, re-points a secondary that replicates neither the primary
// Service nor the primary's own, and labels each pod with its role again,
// a new pod having none. CLUSTER mode is unchanged: its nodes fail over among themselves.

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
	// fenced is whether the primary endpoint carries no connections: the
	// primary failed a check, and until it answers again as the primary
	// it was, or a replica is promoted, a client must not reach it (a pod
	// started again serves empty, as a primary).
	fenced bool
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
	switch {
	case e.fenced && e.primaryUp != "":
		e.primary.set(nil, true)
		e.primaryUp = ""
	case !e.fenced && e.primaryUp != p.addr:
		e.primary.set([]string{p.addr}, true)
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

// Replication reads INFO replication and server, for its run_id.
func (nr nodeReplicator) Replication(ctx context.Context) (Replication, error) {
	cl := nr.c.dial(nr.n.addr, nr.r.password(), nr.r.TransitCA)
	defer cl.Close()
	v, err := cl.Do(ctx, "INFO", "replication", "server")
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
// running, its customer-managed key usable (a suspended instance's nodes
// are stopped on purpose, clustercmek.go: promoting a replica, or
// fencing and relabelling, would fight the suspension), and its primary
// its own (not a secondary of cross-instance replication or a
// migration's target, whose primary replicates another server). Call
// with mu held.
func (c *clusters) watched(r *clusterRecord) bool {
	if _, pending := c.busy[r.Name]; pending || !r.standalone() || r.state != clusterpb.Cluster_ACTIVE ||
		r.suspended || r.KeyState != "" {
		return false
	}
	if v := r.Valkey; (v.Migration.migrating() && v.Migration.State != memorystorepb.MigrationConfig_ROLLED_BACK.String()) ||
		v.role() == memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String() {
		return false
	}
	p := r.firstPrimary()
	return p != nil && p.addr != "" && len(r.Nodes) > 1
}

// settleFor is how long an instance settles after an automatic failover
// (#1657): only a new primary that stops answering or starts again fails
// it over again meanwhile. linkDownSlack is how much longer than its
// primary has been failing a replica's link may have been down for it to
// be promotable: the link drops as the primary's pod goes, a check or two
// before the first failed one.
var (
	settleFor     = 15 * time.Second
	linkDownSlack = 10 * time.Second
)

// primaryHealth is what the health check knows of a watched instance's
// primary.
type primaryHealth struct {
	// server is the primary's; the rest are reset when it changes.
	server string
	// replID and runID are what it last answered with as the primary.
	replID, runID string
	// failed counts its failed checks in a row, since failingSince.
	failed       int
	failingSince time.Time
	// settleUntil ends the settling after the failover that promoted it.
	settleUntil time.Time
	// noneFit is that the last failover found no promotable replica.
	noneFit bool
}

// watchStandalone checks every CLUSTER_DISABLED instance's primary until
// ctx ends.
func (c *clusters) watchStandalone(ctx context.Context, every time.Duration) {
	tend := tendEvery
	hs := map[string]*primaryHealth{}
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
		for _, name := range c.checkStandalone(ctx, hs, every, tend) {
			h := hs[name]
			down := h.failingSince
			if down.IsZero() {
				down = time.Now()
			}
			promoted, err := c.failoverStandalone(ctx, name, time.Since(down))
			switch {
			case err == nil:
				hs[name] = &primaryHealth{server: promoted, settleUntil: time.Now().Add(settleFor)}
			case errors.Is(err, errNoneFit):
				if !h.noneFit {
					log.Printf("memorystore: %s: automatic failover: %v; checking on", name, err)
				}
				h.noneFit = true
			}
		}
	}
}

// checkStandalone closes the endpoints of instances that are gone, checks
// each watched instance's primary, keeping what it learns in hs, tends
// the roles, and returns the instances whose primary failed.
func (c *clusters) checkStandalone(ctx context.Context, hs map[string]*primaryHealth, every, tend time.Duration) []string {
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
			delete(hs, name)
			switch _, pending := c.busy[name]; {
			case r.suspended:
				c.quiesce(name)
			case !pending && r.state == clusterpb.Cluster_ACTIVE && r.KeyState == "":
				// Not watched for good: a secondary, a migration's target
				// or a single node. No check of it will lift a fence a
				// check put up while it was watched (#1652).
				c.unfence(r)
			}
			continue
		}
		c.openEndpoints(r)
		ts = append(ts, target{name, *r, *r.firstPrimary()})
	}
	c.mu.Unlock()
	var out []string
	for _, tg := range ts {
		h := hs[tg.name]
		if h == nil || h.server != tg.p.Server {
			h = &primaryHealth{server: tg.p.Server}
			hs[tg.name] = h
		}
		cctx, cancel := context.WithTimeout(ctx, every)
		st, err := nodeReplicator{c, &tg.rec, &tg.p}.Replication(cctx)
		cancel()
		if healthy := h.observe(st, err, time.Now()); healthy {
			if h.replID != "" && h.replID != st.ReplID {
				log.Printf("memorystore: %s: the primary %s has a new replication ID (%s, was %s) and is the same server; not a failure", tg.name, tg.p.Server, st.ReplID, h.replID)
			}
			h.replID, h.runID = st.ReplID, st.RunID
			h.failed, h.failingSince, h.noneFit = 0, time.Time{}, false
			c.fence(tg.name, false)
			c.tendRoles(ctx, tg.name, tend)
			continue
		} else if err == nil && st.Primary && h.failed >= failAfter && !h.noneFit {
			log.Printf("memorystore: %s: the primary %s started again (run_id %s, was %s; replication ID %s, was %s)", tg.name, tg.p.Server, st.RunID, h.runID, st.ReplID, h.replID)
		}
		c.fence(tg.name, true)
		if h.failed >= failAfter {
			out = append(out, tg.name)
		}
	}
	return out
}

// observe takes one check of the primary, which answered st or err at
// now: whether it is healthy, else counting the failure in h.
func (h *primaryHealth) observe(st Replication, err error, now time.Time) bool {
	settling := now.Before(h.settleUntil)
	if err == nil && st.Primary {
		switch {
		case h.replID == "" && h.runID == "":
			return true // the first check of this primary
		case st.RunID != "" && h.runID != "":
			if st.RunID == h.runID {
				return true // the same server, whatever its replication ID
			}
		case st.ReplID == h.replID || settling:
			// Without a run_id, a new replication ID is taken as a start,
			// except while settling: a promoted node takes one or two.
			return true
		}
		// Started again: its replicas' stream is gone. With no replica
		// fit to promote, it is kept: none has more of its data.
		if h.noneFit {
			return true
		}
		if h.failingSince.IsZero() {
			h.failingSince = now
		}
		h.failed = failAfter
		return false
	}
	if h.failingSince.IsZero() {
		h.failingSince = now
	}
	// Answering as a replica while settling is not a failure: the
	// failover may still be re-pointing it. Not answering is.
	if err != nil || !settling {
		h.failed++
	}
	return false
}

// quiesce points a suspended instance's endpoints at no node, dropping
// their connections, and forgets its roles' labels and fence, so that
// once it resumes, on new nodes, openEndpoints points them at the new
// primary and replicas and tendRoles labels the new pods. The endpoints
// stay open: their addresses outlive the suspension, as they outlive a
// node. Call with mu held.
func (c *clusters) quiesce(name string) {
	e := c.eps[name]
	if e == nil || (e.primaryUp == "" && e.readerUps == "" && !e.fenced && e.labelled == "") {
		return
	}
	e.primary.set(nil, true)
	e.reader.set(nil, true)
	e.primaryUp, e.readerUps, e.fenced, e.labelled = "", "", false, ""
}

// fence stops (on) or resumes name's primary endpoint's connections to
// its primary.
func (c *clusters) fence(name string, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, r := c.eps[name], c.byName[name]
	if e == nil || r == nil || e.fenced == on || (on && !c.watched(r)) {
		// A check that began before r stopped being watched (a switchover
		// began, #1642) does not fence it: nothing would lift the fence of
		// an instance that became a secondary.
		return
	}
	if on {
		log.Printf("memorystore: %s: the primary failed a health check; its primary endpoint carries no connections until it answers again or a replica is promoted", name)
	} else {
		log.Printf("memorystore: %s: the primary answers again; its primary endpoint carries connections", name)
	}
	e.fenced = on
	c.openEndpoints(r)
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
	roles := rolesOf(r)
	var keys []string
	for server, role := range roles {
		keys = append(keys, server+"="+role)
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
	// A secondary of cross-instance replication that a failover could not
	// re-point (its node did not answer) is re-pointed now.
	c.mu.Lock()
	fs := c.followersOf(name)
	src := c.sourceOf(r)
	c.mu.Unlock()
	c.follow(ctx, name, fs, c.nodeHost(&p), p.port, map[string]bool{
		net.JoinHostPort(c.nodeHost(&p), strconv.Itoa(p.port)): true,
		net.JoinHostPort(src.host, strconv.Itoa(src.port)):     true})
	if rr, ok := c.rt.(RoleRouter); ok {
		if err := rr.Roles(ctx, e.base, roles, rolePort(&rec)); err != nil {
			log.Printf("memorystore: %s: label the roles: %v", name, err)
			c.mu.Lock()
			e.labelled = ""
			c.mu.Unlock()
		}
	}
}

// rolesOf is each of r's nodes' role, by its server. Call with mu held.
func rolesOf(r *clusterRecord) map[string]string {
	roles := map[string]string{}
	for _, n := range r.Nodes {
		roles[n.Server] = roleReplica
		if n.Primary {
			roles[n.Server] = rolePrimary
		}
	}
	return roles
}

// rolePort is the port of r's role Services: 6378 with in-transit
// encryption, as Memorystore serves TLS, else 6379.
func rolePort(r *clusterRecord) int {
	if r.TransitCA != "" {
		return TLSPort
	}
	return 6379
}

// errNoneFit is a failover's finding no replica fit to promote.
var errNoneFit = errors.New("no replica is fit to promote")

// promotable is whether a replica, which answered st, may be promoted,
// its primary failing for down, and why not.
func promotable(st Replication, down time.Duration) (bool, string) {
	switch {
	case st.Primary:
		return false, "it answers as a primary"
	case st.SyncInProgress:
		return false, "it is still loading the primary's dataset"
	case st.LinkUp:
		return true, ""
	case st.LinkDownSince < 0:
		return false, "it has not synced with the primary since it started"
	case time.Duration(st.LinkDownSince)*time.Second > down+linkDownSlack:
		return false, fmt.Sprintf("it lost the primary %ds ago, before the primary failed", st.LinkDownSince)
	}
	return true, ""
}

// fittest is the replica of replicas, those of states that answered, with
// the most of the failed primary's data among those promotable: not more
// than limitedDataLoss behind the replica with the most.
func fittest(replicas []int, states map[int]Replication, down time.Duration) (int, error) {
	most := int64(-1)
	for _, i := range replicas {
		if st, ok := states[i]; ok && !st.Primary && st.Offset > most {
			most = st.Offset
		}
	}
	best, bestOffset := -1, int64(-1)
	var why []string
	for _, i := range replicas {
		st, ok := states[i]
		if !ok {
			continue
		}
		fit, reason := promotable(st, down)
		if fit && most-st.Offset >= limitedDataLoss {
			fit, reason = false, fmt.Sprintf("it is %d bytes behind the replica with the most", most-st.Offset)
		}
		if !fit {
			why = append(why, fmt.Sprintf("node %d: %s", i, reason))
			continue
		}
		if st.Offset > bestOffset {
			best, bestOffset = i, st.Offset
		}
	}
	if best < 0 {
		if len(why) == 0 {
			why = []string{"none answers"}
		}
		return -1, fmt.Errorf("%w (%s)", errNoneFit, strings.Join(why, "; "))
	}
	return best, nil
}

// failoverStandalone promotes the fittest replica of name, whose primary
// has been failing for down, and returns its server.
func (c *clusters) failoverStandalone(ctx context.Context, name string, down time.Duration) (string, error) {
	c.mu.Lock()
	r, ok := c.byName[name]
	if !ok || !c.watched(r) {
		c.mu.Unlock()
		return "", fmt.Errorf("instance %s is not watched", name)
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
	fctx, cancel := context.WithTimeout(ctx, haWait)
	defer cancel()
	states := map[int]Replication{}
	for _, i := range replicas {
		qctx, qcancel := context.WithTimeout(fctx, 5*time.Second)
		if st, err := (nodeReplicator{c, &rec, &nodes[i]}).Replication(qctx); err == nil {
			states[i] = st
		}
		qcancel()
	}
	best, err := fittest(replicas, states, down)
	if err != nil {
		// The next checks try again.
		return "", err
	}
	log.Printf("memorystore: %s: the primary %s failed its health checks; failing over", name, nodes[old].Server)
	if err := (nodeReplicator{c, &rec, &nodes[best]}).ReplicaOf(fctx, "", 0); err != nil {
		log.Printf("memorystore: %s: automatic failover: %s: REPLICAOF NO ONE: %v", name, nodes[best].Server, err)
		return "", err
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
		if e := c.eps[name]; e != nil {
			e.labelled, e.fenced = "", false
		}
		c.openEndpoints(r)
	}
	fs := c.followersOf(name)
	var events EventRecorder
	if c.api != nil {
		events = c.api.events
	}
	c.mu.Unlock()
	log.Printf("memorystore: %s: automatic failover promoted %s", name, nodes[best].Server)
	if events != nil {
		events("memorystore", "failover", name, map[string]string{"from": nodes[old].Server, "to": nodes[best].Server})
	}
	// The secondaries of cross-instance replication replicate the promoted
	// node at its own Service (#1641): their link to the primary Service
	// can hang on a connection to the failed pod (see followersOf).
	c.follow(fctx, name, fs, host, port, nil)
	return nodes[best].Server, nil
}

// cirFollower is a secondary of cross-instance replication and its
// primary node, which replicates the primary instance's primary.
type cirFollower struct {
	rec clusterRecord
	n   clusterNode
}

// followersOf are the ACTIVE, idle secondaries of cross-instance
// replication of name, which follow its automatic failover (#1641).
//
// A secondary first replicates name's primary Service, which selects
// whichever node is the primary (sourceOf), but it cannot be left to find
// the promoted node through it: once the primary's pod is gone, and until
// the promoted node's pod is labelled, the Service has no ready endpoint,
// and kube-proxy then sends a connection to the terminating pod's
// endpoint, whose address no longer answers. The secondary's connection
// to it hangs for repl-timeout (60s) before it tries again (measured:
// "Timeout connecting to the PRIMARY" a minute after the failover), and a
// REPLICAOF of the same host and port does not abandon it ("Already
// connected to specified master"). So a failover re-points each secondary
// at the promoted node's own Service, which follows that node's pod, and
// tendRoles re-points one that a failover could not reach. Call with mu
// held.
func (c *clusters) followersOf(name string) []cirFollower {
	var out []cirFollower
	for _, s := range c.secondariesOf(name) {
		if _, pending := c.busy[s.Name]; pending || s.state != clusterpb.Cluster_ACTIVE {
			continue
		}
		if n := s.firstPrimary(); n != nil && n.addr != "" {
			out = append(out, cirFollower{*s, *n})
		}
	}
	return out
}

// follow makes each follower's primary node a replica of host:port,
// unless it replicates one of keep already.
func (c *clusters) follow(ctx context.Context, name string, fs []cirFollower, host string, port int, keep map[string]bool) {
	for _, f := range fs {
		if keep != nil {
			cl := c.dial(f.n.addr, f.rec.password(), f.rec.TransitCA)
			info, err := cl.Do(ctx, "INFO", "replication")
			cl.Close()
			if err != nil {
				continue
			}
			i := fmt.Sprint(info)
			if clusterInfoField(i, "role") != "slave" ||
				keep[net.JoinHostPort(clusterInfoField(i, "master_host"), clusterInfoField(i, "master_port"))] {
				continue
			}
		}
		if err := c.do(ctx, &f.rec, &f.n, "REPLICAOF", host, port); err != nil {
			log.Printf("memorystore: %s: secondary %s: REPLICAOF: %v", name, f.rec.Name, err)
			continue
		}
		log.Printf("memorystore: %s: secondary %s replicates %s", name, f.rec.Name, net.JoinHostPort(host, strconv.Itoa(port)))
	}
}
