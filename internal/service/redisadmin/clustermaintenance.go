package redisadmin

// Maintenance events of Memorystore for Redis Cluster clusters and
// Memorystore for Valkey instances (#1524), on the design of the Redis
// instances' (maintenanceevents.go): an event is scheduled locally,
// through CloudBurrow's control endpoint MaintenanceControlPath (not a
// Memorystore API) or the console's Schedule maintenance, standing in for
// the update Google schedules, and from then on behaves as Google
// documents it (docs.cloud.google.com/memorystore/docs/cluster/ and
// /valkey/ about-maintenance and find-and-set-maintenance-windows, and
// gcloud redis clusters / memorystore instances reschedule-maintenance,
// read 2026-11-18):
//
//   - "The duration of the maintenance window is 1 hour", on the policy's
//     day at its UTC start hour: an event with no start given starts in
//     the first window at least a week away (notifications come "at least
//     one week before it's scheduled"), or a week on, to the hour, with no
//     policy. maintenance_schedule is start_time and end_time, an hour on.
//   - "You can reschedule maintenance within 14 days of the originally
//     scheduled time": SPECIFIC_TIME ("Custom day and time") between the
//     original time and 14 days after; IMMEDIATE ("Update now") applies it
//     now. Neither page limits how many times.
//   - "If there's less than an hour remaining before the current scheduled
//     maintenance time", it is not rescheduled: FAILED_PRECONDITION.
//   - The update is "a gradual deployment and create-before-destroy
//     lifecycle strategy": shard by shard, node by node, "a new replica is
//     added first. If the node is a primary, a coordinated failover
//     follows, then the old replica is removed"; a shard without replicas
//     too. The data is kept ("without any loss of data"). "Cluster Mode
//     Disabled instances undergo a failover, which resets their
//     connections": the primary and reader endpoints then reach the new
//     primary and replicas.
//
// UNVERIFIED (re-read 2026-11-18, the pages above, which give none): the
// code and wording of the errors for nothing scheduled, a reschedule
// within the hour and a time out of bounds; the operation's verb; that
// the state stays ACTIVE while the update runs (the update is "designed to
// have a zero-downtime impact", and neither State enum has MAINTENANCE);
// that the update leaves no operation of its own; the default window with
// no policy (Google's is "10 PM to 6 AM" in the cluster's time zone,
// which the API does not carry).
//
// Unlike Google's Private Service Connect endpoints, which "aren't
// affected by maintenance", a Redis cluster's or a CLUSTER mode Valkey
// instance's discovery endpoint is its first primary's host address here,
// so it moves with the replaced node.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const (
	clusterMaintenanceDeadline = 14 * 24 * time.Hour
	clusterMaintenanceEvery    = 5 * time.Second
)

// clusterMaintenanceDeadline is the original time and 14 days.
func (m *scheduledMaintenance) clusterDeadline() time.Time {
	return m.Original.Add(clusterMaintenanceDeadline)
}

// weeklyWindow is a policy's first weekly window: its day, 1 (Monday) to
// 7 (Sunday), and its UTC start.
type weeklyWindow struct {
	day                 int32
	hours, mins, second int32
}

// window is r's maintenance window, if its policy has one. Call with mu
// held.
func (c *clusters) window(r *clusterRecord) (weeklyWindow, bool) {
	if c.valkey {
		for _, w := range renderValkeyBase(r).GetMaintenancePolicy().GetWeeklyMaintenanceWindow() {
			st := w.GetStartTime()
			return weeklyWindow{int32(w.GetDay()), st.GetHours(), st.GetMinutes(), st.GetSeconds()}, true
		}
		return weeklyWindow{}, false
	}
	for _, w := range c.render(r).GetMaintenancePolicy().GetWeeklyMaintenanceWindow() {
		st := w.GetStartTime()
		return weeklyWindow{int32(w.GetDay()), st.GetHours(), st.GetMinutes(), st.GetSeconds()}, true
	}
	return weeklyWindow{}, false
}

// next is w's first start at or after t.
func (w weeklyWindow) next(t time.Time) time.Time {
	t = t.UTC()
	c := time.Date(t.Year(), t.Month(), t.Day(), int(w.hours), int(w.mins), int(w.second), 0, time.UTC)
	want := time.Weekday(int(w.day) % 7)
	for i := 0; i < 15; i++ {
		if !c.Before(t) && c.Weekday() == want {
			break
		}
		c = c.AddDate(0, 0, 1)
	}
	return c
}

// what names c's resources, in messages.
func (c *clusters) what() string {
	if c.valkey {
		return "instance"
	}
	return "cluster"
}

// scheduleMaintenance schedules an event on name, as Memorystore would:
// CloudBurrow's own call. A zero start is the first window a week away.
func (c *clusters) scheduleMaintenance(name string, start time.Time) (any, error) {
	c.mu.Lock()
	r, ok := c.byName[name]
	if !ok {
		c.mu.Unlock()
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	now := c.now().UTC()
	if start.IsZero() {
		t := now.Add(maintenanceLeadTime)
		if w, ok := c.window(r); ok {
			start = w.next(t)
		} else if start = t.Truncate(time.Hour); start.Before(t) {
			start = start.Add(time.Hour)
		}
	}
	prev := r.Scheduled
	r.Scheduled = &scheduledMaintenance{Original: start.UTC(), Start: start.UTC()}
	if err := c.save(r); err != nil {
		r.Scheduled = prev
		c.mu.Unlock()
		return nil, apierror.Internal(err, "keep %s %s", c.what(), name)
	}
	out := c.output(r)
	c.mu.Unlock()
	if !start.After(now) {
		go c.maintenanceDue()
	}
	return out, nil
}

// cancelMaintenance removes name's event: CloudBurrow's own call.
func (c *clusters) cancelMaintenance(name string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byName[name]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	prev := r.Scheduled
	r.Scheduled = nil
	if err := c.save(r); err != nil {
		r.Scheduled = prev
		return nil, apierror.Internal(err, "keep %s %s", c.what(), name)
	}
	return c.output(r), nil
}

// reschedule moves r's event, which c.mu holds: now (immediate) or to at.
// It returns the operation, finished, or the error; mu is released.
func (c *clusters) reschedule(r *clusterRecord, immediate bool, at *timestamppb.Timestamp, verb string) (string, error) {
	name := r.Name
	now := c.now().UTC()
	m := r.Scheduled
	if m == nil {
		c.mu.Unlock()
		return "", apierror.FailedPrecondition("%s %s has no scheduled maintenance to reschedule", c.what(), name)
	}
	if !now.Before(m.Start.Add(-maintenanceNotice)) {
		c.mu.Unlock()
		return "", apierror.FailedPrecondition("%s %s's maintenance starts at %s: maintenance is not rescheduled when less than an hour remains before it",
			c.what(), name, m.Start.Format(time.RFC3339))
	}
	next := *m
	if immediate {
		next.Start = now
	} else {
		t := at.AsTime().UTC()
		if t.Before(m.Original) || t.After(m.clusterDeadline()) {
			c.mu.Unlock()
			return "", apierror.InvalidArgument("schedule_time %s is not within 14 days of the originally scheduled maintenance time, %s to %s",
				t.Format(time.RFC3339), m.Original.Format(time.RFC3339), m.clusterDeadline().Format(time.RFC3339))
		}
		if t.Before(now) {
			c.mu.Unlock()
			return "", apierror.InvalidArgument("schedule_time %s is in the past", t.Format(time.RFC3339))
		}
		next.Start = t
		next.Reschedules++
	}
	r.Scheduled = &next
	if err := c.save(r); err != nil {
		r.Scheduled = m
		c.mu.Unlock()
		return "", apierror.Internal(err, "keep %s %s", c.what(), name)
	}
	op := c.newOperation(name, verb)
	out := c.output(r)
	c.mu.Unlock()
	c.finish(op, out, nil)
	if immediate {
		go c.maintenanceDue()
	}
	return op, nil
}

// checkReschedule checks a reschedule request's type and time: 1 is
// IMMEDIATE and 3 SPECIFIC_TIME in both APIs.
func checkReschedule(typ int32, at *timestamppb.Timestamp) error {
	switch typ {
	case 1:
		if at != nil {
			return apierror.InvalidArgument("schedule_time is given only with reschedule_type SPECIFIC_TIME")
		}
	case 3:
		if at == nil {
			return apierror.InvalidArgument("schedule_time is required with reschedule_type SPECIFIC_TIME")
		}
		if err := at.CheckValid(); err != nil {
			return apierror.InvalidArgument("schedule_time: %v", err)
		}
	default:
		return apierror.InvalidArgument("reschedule_type is required: IMMEDIATE or SPECIFIC_TIME")
	}
	return nil
}

// RescheduleClusterMaintenance moves a cluster's scheduled maintenance.
func (s *ClusterServer) RescheduleClusterMaintenance(ctx context.Context, req *clusterpb.RescheduleClusterMaintenanceRequest) (*longrunningpb.Operation, error) {
	if err := checkReschedule(int32(req.GetRescheduleType()), req.GetScheduleTime()); err != nil {
		return nil, err
	}
	c, r, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	op, err := c.reschedule(r, req.GetRescheduleType() == clusterpb.RescheduleClusterMaintenanceRequest_IMMEDIATE, req.GetScheduleTime(), "rescheduleClusterMaintenance")
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}

// RescheduleMaintenance moves a Valkey instance's scheduled maintenance.
func (s *MemorystoreServer) RescheduleMaintenance(ctx context.Context, req *memorystorepb.RescheduleMaintenanceRequest) (*longrunningpb.Operation, error) {
	if err := checkReschedule(int32(req.GetRescheduleType()), req.GetScheduleTime()); err != nil {
		return nil, err
	}
	c, r, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	op, err := c.reschedule(r, req.GetRescheduleType() == memorystorepb.RescheduleMaintenanceRequest_IMMEDIATE, req.GetScheduleTime(), "rescheduleMaintenance")
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}

// maintenanceLoop starts due events until ctx ends.
func (c *clusters) maintenanceLoop(ctx context.Context) {
	t := time.NewTicker(clusterMaintenanceEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.maintenanceDue()
	}
}

// maintenanceDue starts every event whose time has come on an ACTIVE
// cluster with no operation in progress; the others wait until it is.
func (c *clusters) maintenanceDue() {
	c.mu.Lock()
	now := c.now().UTC()
	var due []string
	for name, r := range c.byName {
		if _, pending := c.busy[name]; pending || r.Scheduled == nil || r.Scheduled.Start.After(now) ||
			r.state != clusterpb.Cluster_ACTIVE || r.suspended || len(r.Nodes) == 0 {
			continue
		}
		prev := r.Scheduled
		r.Scheduled = nil
		if err := c.save(r); err != nil {
			r.Scheduled = prev
			log.Printf("memorystore: %s: keep the maintenance's start: %v", name, err)
			continue
		}
		c.busy[name] = maintenanceRestarted
		due = append(due, name)
	}
	c.mu.Unlock()
	for _, name := range due {
		go c.maintain(name)
	}
}

// maintain applies name's maintenance update, claimed by maintenanceDue:
// every node is replaced, shard by shard, create-before-destroy.
func (c *clusters) maintain(name string) {
	defer func() {
		c.mu.Lock()
		delete(c.busy, name)
		c.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(c.bg, clusterWait)
	defer cancel()
	c.mu.Lock()
	r, ok := c.byName[name]
	if !ok {
		c.mu.Unlock()
		return
	}
	snap := *r
	snap.Nodes = append([]*clusterNode(nil), r.Nodes...)
	c.mu.Unlock()
	// Replicas first, so a shard's primary fails over once to a new node.
	var order []*clusterNode
	for sh := 0; sh < int(snap.ShardCount); sh++ {
		for _, primary := range []bool{false, true} {
			for _, n := range snap.Nodes {
				if n.Shard == sh && n.Primary == primary {
					order = append(order, n)
				}
			}
		}
	}
	for _, old := range order {
		if err := c.replaceNode(ctx, &snap, old); err != nil {
			log.Printf("memorystore: %s: maintenance: replace %s: %v", name, old.Server, err)
			c.keepNodes(name, &snap)
			return
		}
		c.keepNodes(name, &snap)
		c.removeNodes([]*clusterNode{old})
	}
	log.Printf("memorystore: %s: maintenance applied: %d nodes replaced", name, len(order))
	if c.api != nil && c.api.events != nil {
		c.api.events("memorystore", "maintenance", name, map[string]string{"nodes": fmt.Sprint(len(order))})
	}
}

// keepNodes makes snap's nodes name's, and keeps them.
func (c *clusters) keepNodes(name string, snap *clusterRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byName[name]
	if !ok {
		return
	}
	r.Nodes, r.NextNode = append([]*clusterNode(nil), snap.Nodes...), snap.NextNode
	if err := c.save(r); err != nil {
		log.Printf("memorystore: %s: keep the nodes: %v", name, err)
	}
	if e := c.eps[name]; e != nil {
		e.labelled = ""
	}
	c.openEndpoints(r)
}

// replaceNode adds a new node to old's shard, replicating its primary;
// fails a primary over to it; and removes old from snap's nodes.
func (c *clusters) replaceNode(ctx context.Context, snap *clusterRecord, old *clusterNode) error {
	m := c.newNode(snap, old.Shard, false)
	if err := c.startNodes(ctx, snap, []*clusterNode{m}); err != nil {
		snap.Nodes = snap.Nodes[:len(snap.Nodes)-1]
		c.removeNodes([]*clusterNode{m})
		return err
	}
	if !snap.standalone() {
		if err := c.meet(ctx, snap, snap.firstPrimary(), []*clusterNode{m}); err != nil {
			return err
		}
	}
	if err := c.replicate(ctx, snap, []*clusterNode{m}, snap.Nodes); err != nil {
		return err
	}
	if err := c.settle(ctx, snap, snap.Nodes); err != nil {
		return err
	}
	if old.Primary {
		if err := c.handOver(ctx, snap, old, m); err != nil {
			return err
		}
	}
	var keep []*clusterNode
	for _, n := range snap.Nodes {
		if n != old {
			keep = append(keep, n)
		}
	}
	return c.forget(ctx, snap, keep, []*clusterNode{old})
}

// handOver is a coordinated failover from the primary old to its replica
// m: CLUSTER FAILOVER, or for a CLUSTER_DISABLED instance, once m has
// every write, REPLICAOF NO ONE on m and REPLICAOF m on the others.
func (c *clusters) handOver(ctx context.Context, snap *clusterRecord, old, m *clusterNode) error {
	if snap.standalone() {
		replicas := 0
		for _, n := range snap.Nodes {
			if !n.Primary {
				replicas++
			}
		}
		// Writes stop while the replicas catch up, as a coordinated
		// failover pauses them.
		_ = c.do(ctx, snap, old, "CLIENT", "PAUSE", 10000, "WRITE")
		_ = c.do(ctx, snap, old, "WAIT", replicas, 5000)
		if err := (nodeReplicator{c, snap, m}).ReplicaOf(ctx, "", 0); err != nil {
			_ = c.do(ctx, snap, old, "CLIENT", "UNPAUSE")
			return err
		}
		host, port := c.nodeHost(m), m.port
		for _, n := range snap.Nodes {
			if n != m {
				if err := (nodeReplicator{c, snap, n}).ReplicaOf(ctx, host, port); err != nil {
					return err
				}
			}
		}
		_ = c.do(ctx, snap, old, "CLIENT", "UNPAUSE")
	} else if err := c.do(ctx, snap, m, "CLUSTER", "FAILOVER"); err != nil {
		return err
	}
	if err := c.until(ctx, "the new node "+m.Server+" serves as the primary", func() bool {
		cl := c.dial(m.addr, snap.password(), snap.TransitCA)
		defer cl.Close()
		info, err := cl.Do(ctx, "INFO", "replication")
		return err == nil && strings.Contains(fmt.Sprint(info), "role:master")
	}); err != nil {
		return err
	}
	c.mu.Lock()
	old.Primary, m.Primary = false, true
	c.mu.Unlock()
	return c.settle(ctx, snap, snap.Nodes)
}

// ControlClusterMaintenance schedules an event on a Redis cluster or a
// Valkey instance, or with cancel removes it: CloudBurrow's own call
// (MaintenanceControlPath), not a Memorystore API. A zero start is the
// first window a week away.
func (s *Server) ControlClusterMaintenance(name string, start time.Time, cancel bool) (any, error) {
	c := s.clusters
	if strings.Contains(name, "/instances/") {
		c = s.valkey
	}
	if c == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	if cancel {
		return c.cancelMaintenance(name)
	}
	return c.scheduleMaintenance(name, start)
}
