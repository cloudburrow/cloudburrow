package redisadmin

import (
	"context"
	"log"
	"strconv"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
)

// Automatic failover (#1412). Memorystore fails a Standard tier instance
// over on its own when its primary fails: "If the primary instance fails,
// then the instance fails over automatically to a replica ... a healthy
// replica with the least replication lag", dropping the connections to
// the primary endpoint, which then reaches the new primary. Acknowledged
// writes the replicas had not received are lost, as replication is
// asynchronous: there is no data protection mode to choose, so it is
// FailoverInstance's FORCE_DATA_LOSS, the primary being unable to answer.
//
// The health check is a call to the primary's server, every
// healthEvery: INFO replication through its tunnel. The primary has
// failed once it has not answered as a primary (role:master) for
// failAfter checks in a row: its pod is gone or not Ready, or it was
// started again and came back a replica (haNodeSpec). Google documents
// no detection window; the instance is REPAIRING while it fails over, as
// Memorystore reports an automated repair, and READY after.

// healthEvery is how often the primaries are checked; failAfter is how
// many failed checks in a row fail one over.
var (
	healthEvery = time.Second
	failAfter   = 3
)

// autoFailover is busy's marker of an automatic failover: an operation
// no caller started, which holds off the caller's own.
const autoFailover = "automatic failover"

// watch checks every Standard tier instance's primary until ctx ends.
func (c *instances) watch(ctx context.Context, every time.Duration) {
	failed := map[string]int{}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, name := range c.checkPrimaries(ctx, failed, every) {
			delete(failed, name)
			c.autoFailover(ctx, name)
		}
	}
}

// checkPrimaries counts the failed checks of each READY Standard tier
// instance's primary in failed, and returns the instances whose primary
// failed failAfter in a row.
func (c *instances) checkPrimaries(ctx context.Context, failed map[string]int, every time.Duration) []string {
	type target struct{ name, addr, auth string }
	var ts []target
	c.mu.Lock()
	for name, r := range c.byID {
		if _, pending := c.busy[name]; !r.ha() || pending || r.state != redispb.Instance_READY || r.nodeAddrs[r.Primary] == "" {
			delete(failed, name)
			continue
		}
		ts = append(ts, target{name, r.nodeAddrs[r.Primary], r.AuthString})
	}
	c.mu.Unlock()
	var out []string
	for _, tg := range ts {
		healthy := false
		if rp, err := c.replicator(tg.addr, tg.auth); err == nil {
			cctx, cancel := context.WithTimeout(ctx, every)
			st, err := rp.Replication(cctx)
			cancel()
			healthy = err == nil && st.Primary
		}
		if healthy {
			delete(failed, tg.name)
			continue
		}
		if failed[tg.name]++; failed[tg.name] >= failAfter {
			out = append(out, tg.name)
		}
	}
	return out
}

// autoFailover promotes the replica of name with the most of its failed
// primary's data, as FailoverInstance with FORCE_DATA_LOSS does.
func (c *instances) autoFailover(ctx context.Context, name string) {
	rs, err := c.replicaSet()
	if err != nil {
		return
	}
	c.mu.Lock()
	r, ok := c.byID[name]
	if _, pending := c.busy[name]; !ok || pending || r.state != redispb.Instance_READY {
		c.mu.Unlock()
		return
	}
	c.busy[name] = autoFailover
	r.state, r.message = redispb.Instance_REPAIRING, "automatic failover: the primary node-"+strconv.Itoa(r.Primary)+" failed its health checks"
	from := *r
	from.Nodes = append([]int(nil), r.Nodes...)
	addrs := map[int]string{}
	for k, v := range r.nodeAddrs {
		addrs[k] = v
	}
	c.mu.Unlock()
	log.Printf("memorystore: %s: the primary node-%d failed %d health checks; failing over", name, from.Primary, failAfter)
	fctx, cancel := context.WithTimeout(ctx, haWait)
	err = c.failover(fctx, rs, &from, addrs, redispb.FailoverInstanceRequest_FORCE_DATA_LOSS)
	cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.busy, name)
	r, ok = c.byID[name]
	if !ok {
		return
	}
	r.state, r.message = redispb.Instance_READY, ""
	if err != nil {
		// No replica answers either: the next checks try again.
		log.Printf("memorystore: %s: automatic failover: %v", name, err)
		return
	}
	log.Printf("memorystore: %s: automatic failover promoted node-%d", name, r.Primary)
}
