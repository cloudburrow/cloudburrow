package redisadmin

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Standard tier (#1385). A STANDARD_HA instance is a primary and its
// replicas, each a server of its own (a Deployment and Service per node,
// named ServerName and "-n" and the node's number), kept in step by the
// servers' own asynchronous replication (REPLICAOF). The instance's host
// and port are an endpoint on the host that carries every connection to
// the current primary, and the read endpoint, with read replicas enabled,
// spreads connections over the replicas. In the cluster the instance's
// Service (ServerName) selects the current primary's pod.
//
// FailoverInstance promotes the replica with the most of the primary's
// data: REPLICAOF NO ONE on it, then REPLICAOF it on every other node, the
// old primary among them; the instance's Service and endpoint follow, and
// the connections the endpoint carried are dropped, so clients reconnect
// to the same address and reach the new primary. LIMITED_DATA_LOSS, the
// default, aborts when the greatest replication offset delta between the
// primary and a replica is 30 MB or more, as Memorystore documents;
// FORCE_DATA_LOSS does not check.

// Replication is what a server says of its part in replication.
type Replication struct {
	// Primary is role:master.
	Primary bool
	// Offset is master_repl_offset of a primary, slave_repl_offset of a
	// replica: how much of the primary's stream it has.
	Offset int64
	// LinkUp is a replica's master_link_status:up.
	LinkUp bool
}

// Replicator is a server that replicates: a DataPlane of a Standard tier
// node.
type Replicator interface {
	// ReplicaOf makes the server a replica of host:port, or, with host "",
	// a primary (REPLICAOF NO ONE).
	ReplicaOf(ctx context.Context, host string, port int) error
	// Replication reads INFO replication.
	Replication(ctx context.Context) (Replication, error)
}

// ReplicaSet is a Provisioner that runs Standard tier instances: their
// nodes reach one another inside the cluster, and the instance's Service
// selects its primary.
type ReplicaSet interface {
	// ClusterAddr is where the other servers reach server.
	ClusterAddr(server string) (host string, port int)
	// Route points the Service service at the pod of node's server, on
	// port (0 for the default).
	Route(ctx context.Context, service, node string, port int) error
}

// limitedDataLoss is LIMITED_DATA_LOSS's bound on the replication offset
// delta between the primary and a replica: 30 MB.
const limitedDataLoss = 30 << 20

// haWait bounds the wait for the replicas to sync, after a create.
var haWait = 3 * time.Minute

const tierHA = "STANDARD_HA"

func (r *record) ha() bool { return r.Tier == tierHA }

// nodeServer is the server of r's node n.
func nodeServer(server string, n int) string { return server + "-n" + strconv.Itoa(n) }

// dataServer is the server that has the instance's data: the primary's.
func (r *record) dataServer() string {
	if r.ha() {
		return nodeServer(r.Server, r.Primary)
	}
	return r.Server
}

// nodeSpec is the spec of r's node n: the instance's server, which
// authenticates to the primary with the instance's AUTH string.
func (r *record) nodeSpec(n int) Spec {
	s := r.spec()
	s.Server = nodeServer(r.Server, n)
	if s.Password != "" {
		cfg := map[string]string{}
		for k, v := range s.Configs {
			cfg[k] = v
		}
		cfg["masterauth"] = s.Password
		s.Configs = cfg
	}
	return s
}

// haNodeSpec is nodeSpec, starting as a replica of the instance's
// Service (#1412): a node whose pod restarts, the primary's among them,
// comes back as a replica, which refuses its own replicas a sync until it
// is linked to a primary, so a primary that restarts empty never empties
// its replicas; it follows whichever node the Service selects.
func (r *record) haNodeSpec(rs ReplicaSet, n int) Spec {
	s := r.nodeSpec(n)
	host, port := r.clusterAddr(rs, r.Server)
	s.ReplicaOf = net.JoinHostPort(host, strconv.Itoa(port))
	return s
}

// servers are every server of r.
func (r *record) servers() []string {
	if !r.ha() {
		return []string{r.Server}
	}
	out := make([]string, 0, len(r.Nodes))
	for _, n := range r.Nodes {
		out = append(out, nodeServer(r.Server, n))
	}
	return out
}

// zoneOf is the zone of r's node n: the first in location_id, the second
// in alternative_location_id, and the others in the region's zones in
// turn, as Memorystore places nodes beyond the first two itself.
func (r *record) zoneOf(n int) string {
	switch n {
	case 0:
		return r.LocationID
	case 1:
		return r.AlternativeLocationID
	}
	return regionOf(r.LocationID) + "-" + string("abc"[n%3])
}

func regionOf(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

// altZone is a zone of loc's region other than loc: b after a, else a.
func altZone(loc string) string {
	if strings.HasSuffix(loc, "-a") {
		return regionOf(loc) + "-b"
	}
	return regionOf(loc) + "-a"
}

// replicas are r's nodes other than the primary, in order.
func (r *record) replicas() []int {
	var out []int
	for _, n := range r.Nodes {
		if n != r.Primary {
			out = append(out, n)
		}
	}
	return out
}

// standard checks and fills in the Standard tier fields of a new instance,
// as Memorystore does: replica_count is 1 without read replicas, 1 to 5
// (default 2) with them, and read replicas need nodes of 5 GB or more.
func standard(in *redispb.Instance, r *record) error {
	if in.GetTier() != redispb.Instance_STANDARD_HA {
		if in.GetReplicaCount() != 0 {
			return apierror.InvalidArgument("replica_count must be 0 for a BASIC instance, not %d", in.GetReplicaCount())
		}
		if in.GetReadReplicasMode() == redispb.Instance_READ_REPLICAS_ENABLED {
			return apierror.InvalidArgument("read_replicas_mode READ_REPLICAS_ENABLED is not valid for a BASIC instance")
		}
		if in.GetAlternativeLocationId() != "" {
			return apierror.InvalidArgument("alternative_location_id applies only to a STANDARD_HA instance")
		}
		return nil
	}
	r.Tier = tierHA
	r.ReadReplicas = in.GetReadReplicasMode() == redispb.Instance_READ_REPLICAS_ENABLED
	count := in.GetReplicaCount()
	if err := checkReplicas(r.ReadReplicas, &count, r.MemorySizeGB); err != nil {
		return err
	}
	r.ReplicaCount = count
	r.AlternativeLocationID = in.GetAlternativeLocationId()
	if r.AlternativeLocationID == "" {
		r.AlternativeLocationID = altZone(r.LocationID)
	}
	if r.AlternativeLocationID == r.LocationID {
		return apierror.InvalidArgument("alternative_location_id must be a zone other than location_id %s", r.LocationID)
	}
	if regionOf(r.AlternativeLocationID) != regionOf(r.LocationID) {
		return apierror.InvalidArgument("alternative_location_id %s is not in the instance's region", r.AlternativeLocationID)
	}
	r.Nodes = nil
	for n := 0; n <= int(count); n++ {
		r.Nodes = append(r.Nodes, n)
	}
	r.Primary = 0
	return nil
}

// checkReplicas checks a replica count, 0 taking its default.
func checkReplicas(readReplicas bool, count *int32, sizeGB int32) error {
	if !readReplicas {
		if *count == 0 {
			*count = 1
		}
		if *count != 1 {
			return apierror.InvalidArgument("replica_count must be 1 for a STANDARD_HA instance without read replicas, not %d; set read_replicas_mode READ_REPLICAS_ENABLED for more", *count)
		}
		return nil
	}
	if *count == 0 {
		*count = 2
	}
	if *count < 1 || *count > 5 {
		return apierror.InvalidArgument("replica_count must be between 1 and 5 with read replicas enabled, not %d", *count)
	}
	if sizeGB < 5 {
		return apierror.InvalidArgument("read replicas need an instance of 5 GB or more, not %d GB", sizeGB)
	}
	return nil
}

// renderHA adds r's Standard tier fields to inst. Call with mu held.
func (r *record) renderHA(inst *redispb.Instance) {
	if !r.ha() {
		return
	}
	inst.Tier = redispb.Instance_STANDARD_HA
	inst.ReplicaCount = r.ReplicaCount
	inst.AlternativeLocationId = r.AlternativeLocationID
	inst.CurrentLocationId = r.zoneOf(r.Primary)
	inst.ReadReplicasMode = redispb.Instance_READ_REPLICAS_DISABLED
	if r.ReadReplicas {
		inst.ReadReplicasMode = redispb.Instance_READ_REPLICAS_ENABLED
	}
	inst.Host, inst.Port = "", 0
	if r.primaryEP != nil {
		inst.Host, inst.Port = splitAddr(r.primaryEP.Addr(), "", 0)
	}
	if r.ReadReplicas && r.readEP != nil {
		inst.ReadEndpoint, inst.ReadEndpointPort = splitAddr(r.readEP.Addr(), "", 0)
	}
	nodes := append([]int(nil), r.Nodes...)
	sort.Ints(nodes)
	inst.Nodes = nil
	for _, n := range nodes {
		inst.Nodes = append(inst.Nodes, &redispb.NodeInfo{Id: "node-" + strconv.Itoa(n), Zone: r.zoneOf(n)})
	}
}

// replicator is the server at addr, as a Replicator.
func (c *instances) replicator(addr, auth, ca string) (Replicator, error) {
	rp, ok := c.dial(addr, auth, ca).(Replicator)
	if !ok {
		return nil, apierror.Unimplemented("this instance's servers do not replicate")
	}
	return rp, nil
}

func (c *instances) replicaSet() (ReplicaSet, error) {
	rs, ok := c.prov.(ReplicaSet)
	if !ok {
		return nil, apierror.Unimplemented("tier STANDARD_HA is not supported by this instance's servers")
	}
	return rs, nil
}

// startHA runs every node of from, makes from.Primary the primary and the
// others its replicas, waits for them to sync, and points the instance's
// Service and endpoints at them.
func (c *instances) startHA(ctx context.Context, from record) error {
	rs, err := c.replicaSet()
	if err != nil {
		return err
	}
	addrs := map[int]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var first error
	for _, n := range from.Nodes {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			addr, err := c.prov.Provision(ctx, from.haNodeSpec(rs, n))
			mu.Lock()
			defer mu.Unlock()
			if err != nil && first == nil {
				first = fmt.Errorf("node-%d: %w", n, err)
			}
			addrs[n] = addr
		}(n)
	}
	wg.Wait()
	if first != nil {
		return first
	}
	if err := c.wire(ctx, rs, &from, addrs, from.Primary, true); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byID[from.Name]
	if !ok {
		return fmt.Errorf("instance %s was deleted", from.Name)
	}
	r.nodeAddrs, r.addr = addrs, addrs[r.Primary]
	return c.points(r, false)
}

// wire makes node primary the primary of r's nodes, at addrs, and the
// others its replicas, and routes the instance's Service to it; with wait
// it returns once every replica has synced.
func (c *instances) wire(ctx context.Context, rs ReplicaSet, r *record, addrs map[int]string, primary int, wait bool) error {
	p, err := c.replicator(addrs[primary], r.AuthString, r.caPEM())
	if err != nil {
		return err
	}
	if err := p.ReplicaOf(ctx, "", 0); err != nil {
		return fmt.Errorf("node-%d: REPLICAOF NO ONE: %w", primary, err)
	}
	host, port := r.clusterAddr(rs, nodeServer(r.Server, primary))
	var reps []Replicator
	for _, n := range r.Nodes {
		if n == primary {
			continue
		}
		rp, err := c.replicator(addrs[n], r.AuthString, r.caPEM())
		if err != nil {
			return err
		}
		if err := rp.ReplicaOf(ctx, host, port); err != nil {
			if !wait {
				continue // a failed old primary: it follows once it is back
			}
			return fmt.Errorf("node-%d: REPLICAOF: %w", n, err)
		}
		reps = append(reps, rp)
	}
	if err := rs.Route(ctx, r.Server, nodeServer(r.Server, primary), r.servicePort()); err != nil {
		return fmt.Errorf("route the instance's Service: %w", err)
	}
	if !wait {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, haWait)
	defer cancel()
	for _, rp := range reps {
		for {
			st, err := rp.Replication(ctx)
			if err == nil && st.LinkUp {
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("a replica did not sync with the primary: %w", ctx.Err())
			case <-time.After(savePoll):
			}
		}
	}
	return nil
}

// points points r's endpoints at its nodes: the primary endpoint at the
// primary, the read endpoint, with read replicas, at the replicas; drop
// drops the connections they carried. Call with mu held.
func (c *instances) points(r *record, drop bool) error {
	addr := r.nodeAddrs[r.Primary]
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("the primary's address %q: %w", addr, err)
	}
	if r.primaryEP == nil {
		if r.primaryEP, err = listen(host); err != nil {
			return err
		}
	}
	r.primaryEP.set([]string{addr}, drop)
	if !r.ReadReplicas {
		r.readEP.close()
		r.readEP = nil
		return nil
	}
	if r.readEP == nil {
		if r.readEP, err = listen(host); err != nil {
			return err
		}
	}
	var ups []string
	for _, n := range r.replicas() {
		if a := r.nodeAddrs[n]; a != "" {
			ups = append(ups, a)
		}
	}
	r.readEP.set(ups, drop)
	return nil
}

// closeEndpoints closes r's endpoints. Call with mu held.
func (r *record) closeEndpoints() {
	r.primaryEP.close()
	r.readEP.close()
	r.primaryEP, r.readEP = nil, nil
}

// removeServers removes every server of r, and its Service.
func (c *instances) removeServers(ctx context.Context, r record) error {
	var first error
	for _, s := range r.servers() {
		if err := c.prov.Remove(ctx, s); err != nil && first == nil {
			first = err
		}
	}
	if r.ha() {
		// The instance's Service, which selected the primary.
		if err := c.prov.Remove(ctx, r.Server); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// configAll changes settings on every server of r (CONFIG SET).
func (c *instances) configAll(ctx context.Context, r *record, settings map[string]string) error {
	if !r.ha() {
		return c.dial(r.addr, r.AuthString, r.caPEM()).ConfigSet(ctx, settings)
	}
	for _, n := range r.Nodes {
		if err := c.dial(r.nodeAddrs[n], r.AuthString, r.caPEM()).ConfigSet(ctx, settings); err != nil {
			return fmt.Errorf("node-%d: %w", n, err)
		}
	}
	return nil
}

// FailoverInstance promotes a replica of a STANDARD_HA instance to its
// primary.
func (s *Server) FailoverInstance(ctx context.Context, req *redispb.FailoverInstanceRequest) (*longrunningpb.Operation, error) {
	isDefault, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	if isDefault {
		return nil, apierror.FailedPrecondition("instance %s is a BASIC tier instance: only a STANDARD_HA instance fails over", req.GetName())
	}
	mode := req.GetDataProtectionMode()
	switch mode {
	case redispb.FailoverInstanceRequest_DATA_PROTECTION_MODE_UNSPECIFIED:
		mode = redispb.FailoverInstanceRequest_LIMITED_DATA_LOSS
	case redispb.FailoverInstanceRequest_LIMITED_DATA_LOSS, redispb.FailoverInstanceRequest_FORCE_DATA_LOSS:
	default:
		return nil, apierror.InvalidArgument("data_protection_mode %v is not LIMITED_DATA_LOSS or FORCE_DATA_LOSS", mode)
	}
	c := s.created
	rs, err := c.replicaSet()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	r := c.byID[req.GetName()]
	if !r.ha() {
		c.mu.Unlock()
		return nil, apierror.FailedPrecondition("instance %s is a BASIC tier instance: only a STANDARD_HA instance fails over", r.Name)
	}
	op, err := c.claim(r, "failover")
	if err == nil {
		r.state = redispb.Instance_FAILING_OVER
	}
	from := *r
	from.Nodes = append([]int(nil), r.Nodes...)
	addrs := map[int]string{}
	for k, v := range r.nodeAddrs {
		addrs[k] = v
	}
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.bg, haWait)
		defer cancel()
		c.done(from.Name, op, redispb.Instance_READY, c.failover(ctx, rs, &from, addrs, mode))
	}()
	return s.operation(op)
}

// failover promotes the replica of from that has the most of the
// primary's data.
func (c *instances) failover(ctx context.Context, rs ReplicaSet, from *record, addrs map[int]string, mode redispb.FailoverInstanceRequest_DataProtectionMode) error {
	limited := mode == redispb.FailoverInstanceRequest_LIMITED_DATA_LOSS
	var primaryOffset int64
	primaryUp := false
	if p, err := c.replicator(addrs[from.Primary], from.AuthString, from.caPEM()); err != nil {
		return err
	} else if st, err := p.Replication(ctx); err == nil {
		primaryOffset, primaryUp = st.Offset, true
	}
	if limited && !primaryUp {
		return apierror.FailedPrecondition("failover aborted: the primary does not answer, so its offset delta to the replicas cannot be checked; use FORCE_DATA_LOSS")
	}
	best, bestOffset, worst := -1, int64(-1), int64(0)
	for _, n := range from.replicas() {
		rp, err := c.replicator(addrs[n], from.AuthString, from.caPEM())
		if err != nil {
			return err
		}
		st, err := rp.Replication(ctx)
		if err != nil || (limited && !st.LinkUp) {
			if limited {
				return apierror.FailedPrecondition("failover aborted: node-%d is not replicating from the primary, so its offset delta cannot be checked; use FORCE_DATA_LOSS", n)
			}
			continue
		}
		if d := primaryOffset - st.Offset; d > worst {
			worst = d
		}
		if st.Offset > bestOffset {
			best, bestOffset = n, st.Offset
		}
	}
	if best < 0 {
		return apierror.FailedPrecondition("failover aborted: no replica answers")
	}
	if limited && worst >= limitedDataLoss {
		return apierror.FailedPrecondition("failover aborted: the greatest offset delta between the primary and a replica is %d bytes, 30 MB or more (LIMITED_DATA_LOSS); use FORCE_DATA_LOSS to fail over anyway", worst)
	}
	if err := c.wire(ctx, rs, from, addrs, best, false); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byID[from.Name]
	if !ok {
		return fmt.Errorf("instance %s was deleted", from.Name)
	}
	r.Primary, r.addr = best, addrs[best]
	if err := c.save(r); err != nil {
		return apierror.Internal(err, "keep instance %s", r.Name)
	}
	return c.points(r, true)
}

// scale runs the nodes r's replica_count adds, as replicas of its
// primary, or removes the replicas it drops: the highest numbered.
func (c *instances) scale(ctx context.Context, name string) error {
	rs, err := c.replicaSet()
	if err != nil {
		return err
	}
	c.mu.Lock()
	r, ok := c.byID[name]
	if !ok {
		c.mu.Unlock()
		return fmt.Errorf("instance %s was deleted", name)
	}
	from := *r
	from.Nodes = append([]int(nil), r.Nodes...)
	want := int(r.ReplicaCount)
	have := len(r.replicas())
	var drop []int
	if have > want {
		reps := r.replicas()
		drop = reps[want:]
		keep := map[int]bool{}
		for _, n := range drop {
			keep[n] = true
		}
		var nodes []int
		for _, n := range r.Nodes {
			if !keep[n] {
				nodes = append(nodes, n)
			}
		}
		r.Nodes = nodes
		_ = c.save(r)
		_ = c.points(r, false)
	}
	c.mu.Unlock()
	for _, n := range drop {
		if err := c.prov.Remove(ctx, nodeServer(from.Server, n)); err != nil {
			return err
		}
	}
	if have >= want {
		return nil
	}
	host, port := from.clusterAddr(rs, nodeServer(from.Server, from.Primary))
	next := 0
	for _, n := range from.Nodes {
		if n >= next {
			next = n + 1
		}
	}
	for i := have; i < want; i, next = i+1, next+1 {
		addr, err := c.prov.Provision(ctx, from.haNodeSpec(rs, next))
		if err != nil {
			return fmt.Errorf("node-%d: %w", next, err)
		}
		rp, err := c.replicator(addr, from.AuthString, from.caPEM())
		if err != nil {
			return err
		}
		if err := rp.ReplicaOf(ctx, host, port); err != nil {
			return fmt.Errorf("node-%d: REPLICAOF: %w", next, err)
		}
		for {
			st, err := rp.Replication(ctx)
			if err == nil && st.LinkUp {
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("node-%d did not sync with the primary: %w", next, ctx.Err())
			case <-time.After(savePoll):
			}
		}
		c.mu.Lock()
		if r, ok := c.byID[name]; ok {
			r.Nodes = append(append([]int(nil), r.Nodes...), next)
			addrs := map[int]string{next: addr}
			for k, v := range r.nodeAddrs {
				addrs[k] = v
			}
			r.nodeAddrs = addrs
			_ = c.save(r)
			_ = c.points(r, false)
		}
		c.mu.Unlock()
	}
	return nil
}
