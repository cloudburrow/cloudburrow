package redisadmin

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
)

// Automatic failover in cluster mode (#1910): a Redis cluster's, and a
// Memorystore for Valkey instance's with cluster mode enabled. Google
// documents that when a shard's primary fails, "a replica is promoted to
// be the new primary" automatically, within seconds, and that the failed
// node is replaced, so the shard keeps its replica count
// (https://docs.cloud.google.com/memorystore/docs/cluster/ha-and-replicas,
// https://docs.cloud.google.com/memorystore/docs/valkey/ha-and-replicas).
//
// Valkey's own cluster failover cannot be relied on for this: a replica is
// promoted only with the votes of a majority of the primaries, so a
// cluster of one or two shards never fails over on its own. So each node
// is checked every healthEvery, CLUSTER MYID and ROLE through its tunnel,
// which follows its pod:
//
//   - a primary that has not answered failAfter checks in a row, or that
//     answers with an ID other than its own (its pod started again, empty,
//     with a new identity: a node's cluster configuration is not kept),
//     has failed. Unless one of its shard's replicas already answers as
//     the primary (Valkey failed it over itself), the replica with the
//     greatest replication offset is promoted with CLUSTER FAILOVER
//     TAKEOVER, which needs no votes; writes it had not received are lost,
//     as Google documents of asynchronous replication. The record's roles
//     follow, so discovery_endpoints (the first shard's primary) moves to
//     the promoted node.
//   - a node that answers with a new ID rejoins: its settings are set
//     again (startNodes), its old identity is forgotten on every other
//     node, it MEETs the cluster, and it is made a replica of its shard's
//     primary (emptied first: it syncs from the primary). A shard with no
//     replica keeps the node as its primary, which takes the slots its old
//     identity had, with what it loaded from its persistence.
//
// A node's own ID is the one it gave when CloudBurrow started it
// (startNodes asks CLUSTER MYID, and the checks begin only once every node
// has one), so a primary lost before the first check of its cluster, as
// a pod deleted the moment the cluster is ACTIVE, is found failed as any
// other: the checks do not wait for it to answer as itself first, which
// it never would again.
//
// Roles that changed without CloudBurrow (Valkey's own failover, a
// CLUSTER FAILOVER sent by a client) are taken into the record as found.

// nodeHealth is what the checks learned of one node.
type nodeHealth struct {
	// failed is how many checks in a row the node has not answered.
	failed int
	// reborn is the node's new ID, when it answered with one.
	reborn string
}

// watchClusters checks every cluster-mode cluster of c every every.
func (c *clusters) watchClusters(ctx context.Context, every time.Duration) {
	hs := map[string]map[string]*nodeHealth{}
	// running are the clusters being checked or repaired: each in a
	// goroutine of its own, so a cluster whose nodes time out never
	// delays another's checks.
	running := map[string]bool{}
	done := make(chan string, 16)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case name := <-done:
			// What the checks learned is kept: a repair that did not finish
			// is tried again at the next check.
			delete(running, name)
			continue
		case <-t.C:
		}
		for _, name := range c.clusterModeNames() {
			if running[name] {
				continue
			}
			if hs[name] == nil {
				hs[name] = map[string]*nodeHealth{}
			}
			running[name] = true
			go func(name string, h map[string]*nodeHealth) {
				defer func() { done <- name }()
				if c.checkCluster(ctx, name, h, every) {
					c.repairCluster(ctx, name, h)
				}
			}(name, hs[name])
		}
		for name := range hs {
			if _, ok := c.lookup(name); !ok && !running[name] {
				delete(hs, name)
			}
		}
	}
}

func (c *clusters) lookup(name string) (*clusterRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byName[name]
	return r, ok
}

// clusterModeNames are the clusters to check: ACTIVE, in cluster mode,
// with no operation in progress.
func (c *clusters) clusterModeNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for name, r := range c.byName {
		if r.standalone() || r.suspended || c.busy[name] != "" || r.state != clusterpb.Cluster_ACTIVE {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// snapshot copies r and its nodes (c.mu held).
func snapshotNodes(r *clusterRecord) (clusterRecord, []clusterNode) {
	snap := *r
	nodes := make([]clusterNode, len(r.Nodes))
	for i, n := range r.Nodes {
		nodes[i] = *n
	}
	return snap, nodes
}

// probe asks a node its ID and role.
func (c *clusters) probe(ctx context.Context, r *clusterRecord, n clusterNode, timeout time.Duration) (id string, primary bool, offset int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cl := c.dial(n.addr, r.password(), r.TransitCA)
	defer cl.Close()
	out, err := cl.Pipe(ctx, [][]any{{"CLUSTER", "MYID"}, {"ROLE"}})
	if err != nil {
		return "", false, 0, err
	}
	role, _ := out[1].([]any)
	if len(role) == 0 {
		return "", false, 0, fmt.Errorf("ROLE: %v", out[1])
	}
	switch fmt.Sprint(role[0]) {
	case "master":
		if len(role) > 1 {
			offset, _ = strconv.ParseInt(fmt.Sprint(role[1]), 10, 64)
		}
		return fmt.Sprint(out[0]), true, offset, nil
	default:
		if len(role) > 4 {
			offset, _ = strconv.ParseInt(fmt.Sprint(role[4]), 10, 64)
		}
		return fmt.Sprint(out[0]), false, offset, nil
	}
}

// checkCluster checks name's nodes once, takes roles that changed into the
// record, and reports whether a repair is due: a failed primary or a node
// that started again.
func (c *clusters) checkCluster(ctx context.Context, name string, hs map[string]*nodeHealth, every time.Duration) bool {
	c.mu.Lock()
	r, ok := c.byName[name]
	if !ok {
		c.mu.Unlock()
		return false
	}
	snap, nodes := snapshotNodes(r)
	c.mu.Unlock()
	roles := map[string]bool{}
	due := false
	timeout := 2 * every
	if timeout < 2*time.Second {
		timeout = 2 * time.Second
	}
	for _, n := range nodes {
		if n.id == "" || n.addr == "" {
			return false // not formed yet
		}
		h := hs[n.Server]
		if h == nil {
			h = &nodeHealth{}
			hs[n.Server] = h
		}
		id, primary, _, err := c.probe(ctx, &snap, n, timeout)
		if (err != nil || id != n.id) && h.failed == 0 && h.reborn == "" {
			log.Printf("memorystore: %s: node %s: check: id %q (kept %q), %v", name, n.Server, id, n.id, err)
		}
		switch {
		case err != nil:
			h.failed++
		case id != n.id:
			h.failed, h.reborn = failAfter, id
		default:
			h.failed, h.reborn = 0, ""
			roles[n.Server] = primary
		}
		if h.reborn != "" || (n.Primary && h.failed >= failAfter) {
			due = true
		}
	}
	// Roles changed elsewhere: a healthy node's own word wins.
	changed := false
	c.mu.Lock()
	if r, ok := c.byName[name]; ok && c.busy[name] == "" {
		for _, n := range r.Nodes {
			if p, ok := roles[n.Server]; ok && p != n.Primary {
				n.Primary, changed = p, true
			}
		}
		if changed {
			_ = c.save(r)
		}
	}
	c.mu.Unlock()
	if changed {
		log.Printf("memorystore: %s: node roles changed in the cluster; the record follows", name)
	}
	return due
}

// repairCluster fails over the shards whose primary failed and makes the
// nodes that started again rejoin (mu not held).
func (c *clusters) repairCluster(ctx context.Context, name string, hs map[string]*nodeHealth) {
	ctx, cancel := context.WithTimeout(ctx, clusterWait)
	defer cancel()
	c.mu.Lock()
	r, ok := c.byName[name]
	if !ok || c.busy[name] != "" {
		c.mu.Unlock()
		return
	}
	c.busy[name] = "repair"
	snap := *r
	snap.Nodes = append([]*clusterNode(nil), r.Nodes...)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.busy[name] == "repair" {
			delete(c.busy, name)
		}
		c.mu.Unlock()
	}()
	failed := func(n *clusterNode) bool {
		h := hs[n.Server]
		return h != nil && (h.failed >= failAfter || h.reborn != "")
	}
	// Failover: each shard whose primary failed, to its best replica.
	for sh := 0; sh < int(snap.ShardCount); sh++ {
		var primary *clusterNode
		var replicas []*clusterNode
		for _, n := range snap.Nodes {
			if n.Shard != sh {
				continue
			}
			if n.Primary {
				primary = n
			} else if !failed(n) {
				replicas = append(replicas, n)
			}
		}
		if primary == nil || !failed(primary) || len(replicas) == 0 {
			continue
		}
		if err := c.takeOver(ctx, &snap, primary, replicas); err != nil {
			log.Printf("memorystore: %s: automatic failover of shard %d: %v", name, sh, err)
			return
		}
	}
	c.keepRoles(name, &snap)
	// Rejoin: every node that started again with a new identity.
	for _, n := range snap.Nodes {
		h := hs[n.Server]
		if h == nil || h.reborn == "" {
			continue
		}
		if err := c.rejoin(ctx, &snap, n); err != nil {
			log.Printf("memorystore: %s: node %s rejoining the cluster: %v", name, n.Server, err)
			c.keepRoles(name, &snap)
			return
		}
		log.Printf("memorystore: %s: node %s started again and rejoined the cluster", name, n.Server)
		c.keepRoles(name, &snap)
	}
	if c.api != nil && c.api.events != nil {
		c.api.events("memorystore", "repair", name, nil)
	}
}

// takeOver promotes the replica of replicas with the greatest replication
// offset in place of the failed primary old, unless one already serves as
// the primary.
func (c *clusters) takeOver(ctx context.Context, snap *clusterRecord, old *clusterNode, replicas []*clusterNode) error {
	var best *clusterNode
	var bestOff int64 = -1
	var errs []string
	for _, n := range replicas {
		_, primary, off, err := c.probe(ctx, snap, *n, 2*time.Second)
		if err != nil {
			errs = append(errs, n.Server+": "+err.Error())
			continue
		}
		if primary {
			best, bestOff = n, 1<<62
			break
		}
		if best == nil || off > bestOff {
			best, bestOff = n, off
		}
	}
	if best == nil {
		return fmt.Errorf("no replica of shard %d answers: %s", old.Shard, strings.Join(errs, "; "))
	}
	if bestOff != 1<<62 {
		log.Printf("memorystore: %s: the primary %s of shard %d failed; promoting %s", snap.Name, old.Server, old.Shard, best.Server)
		if err := c.do(ctx, snap, best, "CLUSTER", "FAILOVER", "TAKEOVER"); err != nil {
			return err
		}
	}
	if err := c.until(ctx, "the replica "+best.Server+" serves as the primary", func() bool {
		_, primary, _, err := c.probe(ctx, snap, *best, 2*time.Second)
		return err == nil && primary
	}); err != nil {
		return err
	}
	// The record's nodes are shared with snap's: flip the roles under mu.
	c.mu.Lock()
	old.Primary, best.Primary = false, true
	c.mu.Unlock()
	return nil
}

// rejoin brings n, started again with a new identity, back into the
// cluster: as a replica of its shard's primary, or as the primary of a
// shard that has no other node.
func (c *clusters) rejoin(ctx context.Context, snap *clusterRecord, n *clusterNode) error {
	oldID := n.id
	var via, primary *clusterNode
	for _, m := range snap.Nodes {
		if m == n {
			continue
		}
		if via == nil {
			via = m
		}
		if m.Shard == n.Shard && m.Primary {
			primary = m
		}
	}
	if via == nil {
		return fmt.Errorf("no other node")
	}
	var slots []int
	if primary == nil {
		owned, err := c.ownedSlots(ctx, snap, via)
		if err != nil {
			return err
		}
		slots = owned[oldID]
	}
	// The node's settings again, and its new ID, address and pod IP.
	if err := c.startNodes(ctx, snap, []*clusterNode{n}); err != nil {
		return err
	}
	others := make([]*clusterNode, 0, len(snap.Nodes)-1)
	for _, m := range snap.Nodes {
		if m != n {
			others = append(others, m)
		}
	}
	for _, m := range others {
		if err := c.do(ctx, snap, m, "CLUSTER", "FORGET", oldID); err != nil && !strings.Contains(err.Error(), "nknown node") {
			return err
		}
	}
	if primary != nil {
		if err := c.do(ctx, snap, n, "FLUSHALL"); err != nil {
			return err
		}
	}
	if err := c.meet(ctx, snap, via, []*clusterNode{n}); err != nil {
		return err
	}
	if primary != nil {
		c.mu.Lock()
		n.Primary = false
		c.mu.Unlock()
		if err := c.replicate(ctx, snap, []*clusterNode{n}, snap.Nodes); err != nil {
			return err
		}
	} else {
		c.mu.Lock()
		n.Primary = true
		c.mu.Unlock()
		for _, rg := range slotRuns(slots) {
			if err := c.do(ctx, snap, n, "CLUSTER", "ADDSLOTSRANGE", rg[0], rg[1]); err != nil {
				return err
			}
		}
		_ = c.do(ctx, snap, n, "CLUSTER", "BUMPEPOCH")
	}
	return c.settle(ctx, snap, snap.Nodes)
}

// slotRuns are slots, sorted, as ranges.
func slotRuns(slots []int) [][2]int {
	s := append([]int(nil), slots...)
	sort.Ints(s)
	var out [][2]int
	for _, x := range s {
		if k := len(out); k > 0 && out[k-1][1] == x-1 {
			out[k-1][1] = x
			continue
		}
		out = append(out, [2]int{x, x})
	}
	return out
}

// keepRoles saves name's record with snap's roles, which share its nodes.
func (c *clusters) keepRoles(name string, snap *clusterRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byName[name]
	if !ok {
		return
	}
	if err := c.save(r); err != nil {
		log.Printf("memorystore: %s: keep the roles: %v", name, err)
	}
	_ = snap
}
