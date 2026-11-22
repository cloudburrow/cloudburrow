package redisadmin

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
)

// Migration into a CLUSTER mode instance (#1577), as Google's "Migrate
// workloads" guide (https://docs.cloud.google.com/memorystore/docs/valkey/migrate-workloads)
// documents it: "the cluster mode of the target instance must match the
// cluster mode of the source instance", and "the number of shards on the
// target instance must be identical to the number of shards on the source
// instance" (the replicas may differ). StartMigration's
// self_managed_source names one node of the source cluster; the source's
// shards are read from it (CLUSTER SLOTS, or this CloudBurrow instance's
// own record of a Valkey instance or Redis cluster that node belongs to,
// whose nodes are reached at their host addresses), the target's slots are
// moved to match the source's shards, and each target shard is fed by a
// replication link from the source shard of the same slots
// (crossclusterlink.go): its data is copied, then its replication stream
// applied, continuously, while the target refuses its clients' writes, as
// a migration's target "allows only reads". FinishMigration waits until
// each link has applied its source shard's offset at the time of the call
// (unless force), stops the links and lets the target take writes. A
// source with AUTH or TLS is not served, as in CLUSTER_DISABLED mode: the
// source config has no credentials, and Google's guide has AUTH and TLS
// off on the source.

// migrationLink is the links map's source key for a migration.
const migrationLink = "migration"

// clusterSource reads the shards of the cluster whose node is at ip:port:
// each primary's host address and its slot ranges.
func (s *Server) clusterSource(ctx context.Context, ip string, port int) (map[string][][2]int64, error) {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	var dial ClusterDialer
	for _, c := range []*clusters{s.clusters, s.valkey} {
		if c == nil {
			continue
		}
		dial = c.dial
		c.mu.Lock()
		var nodes []*clusterNode
		for _, r := range c.byName {
			if r.standalone() {
				continue
			}
			for _, n := range r.Nodes {
				if n.addr == addr {
					nodes = append([]*clusterNode(nil), r.Nodes...)
				}
			}
		}
		c.mu.Unlock()
		if nodes != nil {
			// A Valkey instance or Redis cluster of this CloudBurrow
			// instance: its nodes announce addresses of the cluster to
			// in-cluster clients, so they are reached at their host ones.
			return c.primaryRanges(ctx, nodes, "", "")
		}
	}
	if dial == nil {
		return nil, fmt.Errorf("no cluster runtime")
	}
	cl := dial(addr, "", "")
	defer cl.Close()
	v, err := cl.Do(ctx, "CLUSTER", "SLOTS")
	if err != nil {
		return nil, fmt.Errorf("CLUSTER SLOTS on %s: %w", addr, err)
	}
	out := map[string][][2]int64{}
	rows, _ := v.([]any)
	for _, e := range rows {
		row, _ := e.([]any)
		if len(row) < 3 {
			continue
		}
		owner, _ := row[2].([]any)
		if len(owner) < 2 {
			continue
		}
		host := fmt.Sprint(owner[0])
		if host == "" {
			host = ip // a node that announces no address is the one asked
		}
		p, _ := strconv.Atoi(fmt.Sprint(owner[1]))
		lo, _ := strconv.ParseInt(fmt.Sprint(row[0]), 10, 64)
		hi, _ := strconv.ParseInt(fmt.Sprint(row[1]), 10, 64)
		a := net.JoinHostPort(host, strconv.Itoa(p))
		out[a] = append(out[a], [2]int64{lo, hi})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the source at %s serves no slots: it is not in cluster mode", addr)
	}
	for a := range out {
		sort.Slice(out[a], func(i, j int) bool { return out[a][i][0] < out[a][j][0] })
	}
	return out, nil
}

// slotGroups are the slot ranges of each shard of m, the shards in order
// of their first slot.
func slotGroups(m map[string][][2]int64) [][][2]int64 {
	var out [][][2]int64
	for _, rs := range m {
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0][0] < out[j][0][0] })
	return out
}

// alignSlots moves r's slots, keys and all, so that each shard of r holds
// the slots of src's shard of the same order, unless it does already.
// r's nodes take its admin's writes (MIGRATE).
func (c *clusters) alignSlots(ctx context.Context, r *clusterRecord, src map[string][][2]int64) error {
	mine, err := c.primaryRanges(ctx, r.Nodes, r.password(), r.TransitCA)
	if err != nil {
		return err
	}
	groups := slotGroups(src)
	if fmt.Sprint(slotGroups(mine)) == fmt.Sprint(groups) {
		return nil
	}
	primaries := primariesOf(r.Nodes)
	if len(primaries) != len(groups) {
		return fmt.Errorf("%s has %d shards and its source %d", r.Name, len(primaries), len(groups))
	}
	want := map[int]*clusterNode{}
	for i, g := range groups {
		for _, rg := range g {
			for s := rg[0]; s <= rg[1]; s++ {
				want[int(s)] = primaries[i]
			}
		}
	}
	if len(want) != totalSlots {
		return fmt.Errorf("the source serves %d of the %d slots", len(want), totalSlots)
	}
	if err := c.placeSlots(ctx, r, primaries, want); err != nil {
		return err
	}
	return c.settle(ctx, r, r.Nodes)
}

// alignSecondary gives r, a CLUSTER mode secondary (a snapshot), its
// primary's slot layout when it differs (a primary whose slots a
// migration placed, alignSlots): its writes are let through while its
// slots move, and refused again by the link that follows.
func (c *clusters) alignSecondary(ctx context.Context, r *clusterRecord, pnodes []*clusterNode, ppass, pca string) error {
	src, err := c.primaryRanges(ctx, pnodes, ppass, pca)
	if err != nil {
		return err
	}
	mine, err := c.primaryRanges(ctx, r.Nodes, r.password(), r.TransitCA)
	if err != nil {
		return err
	}
	if fmt.Sprint(slotGroups(mine)) == fmt.Sprint(slotGroups(src)) {
		return nil
	}
	open := *r
	open.dropCIR()
	if err := c.setWrites(ctx, &open, open.Nodes, false); err != nil {
		return err
	}
	return c.alignSlots(ctx, &open, src)
}

// migrateCluster starts a migration into r, a CLUSTER mode instance (a
// snapshot), from the cluster whose node is at ip:port, and waits until
// every shard is copied and streaming.
func (c *clusters) migrateCluster(ctx context.Context, r *clusterRecord, ip string, port int) error {
	src, err := c.api.clusterSource(ctx, ip, port)
	if err != nil {
		return fmt.Errorf("read the source's shards: %w", err)
	}
	if len(src) != int(r.ShardCount) {
		return fmt.Errorf("the source has %d shards and the instance %d: the target's shard count must be the source's", len(src), r.ShardCount)
	}
	if err := c.alignSlots(ctx, r, src); err != nil {
		return fmt.Errorf("place the instance's slots as the source's: %w", err)
	}
	c.mu.Lock()
	if live := c.byName[r.Name]; live != nil && live.linkPassword() == "" {
		live.setLinkPassword(newUID() + newUID())
		_ = c.save(live)
	}
	c.mu.Unlock()
	if err := c.setWrites(ctx, r, r.Nodes, true); err != nil {
		return fmt.Errorf("refuse the instance's writes: %w", err)
	}
	name := r.Name
	source := net.JoinHostPort(ip, strconv.Itoa(port))
	return c.startLink(ctx, name, migrationLink, func(ctx context.Context) ([][2]shardEnd, error) {
		from, err := c.api.clusterSource(ctx, ip, port)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		x := c.byName[name]
		if x == nil {
			c.mu.Unlock()
			return nil, fmt.Errorf("instance %s is gone", name)
		}
		nodes := append([]*clusterNode(nil), x.Nodes...)
		pass, ca, lp := x.password(), x.TransitCA, x.linkPassword()
		c.mu.Unlock()
		dst, err := c.primaryRanges(ctx, nodes, pass, ca)
		if err != nil {
			return nil, err
		}
		return pairRanges(from, dst, shardEnd{}, shardEnd{user: crrUser, password: lp, ca: ca}, "the source at "+source, name)
	})
}

// linkCaughtUp waits until each of r's links has applied its source
// shard's replication offset at the time of the call.
func (c *clusters) linkCaughtUp(ctx context.Context, r string) error {
	c.mu.Lock()
	l := c.links[r]
	c.mu.Unlock()
	if l == nil {
		return fmt.Errorf("instance %s replicates nothing", r)
	}
	l.mu.Lock()
	srcs := append([]shardEnd(nil), l.srcs...)
	l.mu.Unlock()
	want := make([]int64, len(srcs))
	for i, e := range srcs {
		cl := c.dial(e.addr, e.password, e.ca)
		info, err := cl.Do(ctx, "INFO", "replication")
		cl.Close()
		if err != nil {
			return fmt.Errorf("read the source's replication offset at %s: %w (FinishMigration with force skips the check)", e.addr, err)
		}
		want[i], _ = strconv.ParseInt(clusterInfoField(fmt.Sprint(info), "master_repl_offset"), 10, 64)
	}
	return c.until(ctx, "the instance to catch up with the source", func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		if len(l.offs) != len(want) {
			return false
		}
		for i := range want {
			if l.srcs[i].addr != srcs[i].addr || l.offs[i] < want[i] {
				return false
			}
		}
		return true
	})
}

// migrationTarget is whether r is a CLUSTER mode instance being migrated
// into, whose clients are refused writes.
func (r *clusterRecord) migrationTarget() bool {
	if r.Valkey == nil || r.standalone() || r.Valkey.Migration == nil {
		return false
	}
	st := r.Valkey.Migration.State
	return st == "" || st == "REPLICATION_ESTABLISHED"
}
