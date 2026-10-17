package redisadmin

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Cross-instance replication, migration and engine upgrades of a
// Memorystore for Valkey instance (#1557), as Google documents them.
//
// Cross-instance replication is served for CLUSTER_DISABLED instances: a
// secondary is created with cross_instance_replication_config.instance_role
// SECONDARY and primary_instance, and its primary node is then a replica
// (REPLICAOF) of the primary instance's primary node, its own replicas
// chained to it, so the secondary serves reads of the primary's data and
// refuses writes (READONLY). UpdateInstance of the config switches over
// (a secondary updated to PRIMARY, with every other member as its
// secondary_instances: it stops replicating, and the others replicate
// from it) and detaches (a secondary updated to NONE stops replicating; a
// primary updated to NONE, or to fewer secondary_instances, detaches
// them). A primary with secondaries is not deleted until they are
// detached. In CLUSTER mode it keeps its 501: a cluster-mode replica
// follows only a primary of its own cluster (CLUSTER REPLICATE takes a node
// ID its gossip knows), so another cluster's shards cannot be replicated.
//
// Migration (StartMigration, FinishMigration) from a self-managed Valkey or
// Redis server into a CLUSTER_DISABLED instance is the same replication:
// the instance's primary replicates the source at self_managed_source's
// ip_address and port (MIGRATING; migration_config.state
// REPLICATION_ESTABLISHED once linked), and FinishMigration, once the
// instance's offset has caught up with the source's (unless force), stops
// replicating (MIGRATED, ACTIVE). A source that is another server of this
// CloudBurrow instance (a Memorystore for Redis instance, a node of a
// Valkey instance or Redis cluster) is named by the host address its
// clients use, which is reached inside the cluster at its own address;
// any other address is replicated from as it is. A source with AUTH is
// not served: self_managed_source has no credentials.
//
// An engine_version update to a later version (VALKEY_7_2, VALKEY_8_0,
// VALKEY_9_0) takes an RDB snapshot of each shard's primary, forms new
// nodes of the new version's image, loads the snapshots into them, and
// only then removes the old nodes, which serve until then: writes made
// during the upgrade, after the snapshot, are not carried over.

// remoteInstance is a member of cross-instance replication.
type remoteInstance struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}

// cirConfig is an instance's cross_instance_replication_config.
type cirConfig struct {
	Role    string          `json:"role"`
	Primary *remoteInstance `json:"primary,omitempty"`
	// Secondaries are every secondary of the primary: a primary's
	// secondary_instances, and the membership of each.
	Secondaries []remoteInstance `json:"secondaries,omitempty"`
	Updated     time.Time        `json:"updated"`
}

// migration is an instance's migration_config.
type migration struct {
	IP                string `json:"ip"`
	Port              int32  `json:"port"`
	NetworkAttachment string `json:"networkAttachment"`
	// State is "" while replication is being established, then
	// REPLICATION_ESTABLISHED, MIGRATED.
	State string `json:"state,omitempty"`
	Force bool   `json:"force,omitempty"`
	// host and port are where the nodes reach the source.
	host string
	port int
}

func (m *migration) migrating() bool {
	return m != nil && m.State != memorystorepb.MigrationConfig_MIGRATED.String()
}

func (v *valkeyFields) role() string {
	if v.CIR == nil || v.CIR.Role == "" {
		return memorystorepb.CrossInstanceReplicationConfig_NONE.String()
	}
	return v.CIR.Role
}

func renderRemote(x remoteInstance) *memorystorepb.CrossInstanceReplicationConfig_RemoteInstance {
	return &memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{Instance: x.Name, Uid: x.UID}
}

// renderCIR sets out's cross_instance_replication_config and
// migration_config.
func renderCIR(r *clusterRecord, out *memorystorepb.Instance) {
	v := r.Valkey
	if cfg := v.CIR; cfg != nil && cfg.Role != "" {
		c := &memorystorepb.CrossInstanceReplicationConfig{
			InstanceRole: memorystorepb.CrossInstanceReplicationConfig_InstanceRole(memorystorepb.CrossInstanceReplicationConfig_InstanceRole_value[cfg.Role]),
			UpdateTime:   timestamppb.New(cfg.Updated),
			Membership:   &memorystorepb.CrossInstanceReplicationConfig_Membership{},
		}
		var secs []*memorystorepb.CrossInstanceReplicationConfig_RemoteInstance
		for _, s := range cfg.Secondaries {
			secs = append(secs, renderRemote(s))
		}
		c.Membership.SecondaryInstances = secs
		if cfg.Role == memorystorepb.CrossInstanceReplicationConfig_PRIMARY.String() {
			c.SecondaryInstances = secs
			c.Membership.PrimaryInstance = renderRemote(remoteInstance{r.Name, r.UID})
		} else if cfg.Primary != nil {
			c.PrimaryInstance = renderRemote(*cfg.Primary)
			c.Membership.PrimaryInstance = renderRemote(*cfg.Primary)
		}
		out.CrossInstanceReplicationConfig = c
	}
	if m := v.Migration; m != nil {
		st := memorystorepb.MigrationConfig_STATE_UNSPECIFIED
		if m.State != "" {
			st = memorystorepb.MigrationConfig_State(memorystorepb.MigrationConfig_State_value[m.State])
		}
		out.MigrationConfig = &memorystorepb.MigrationConfig{State: st, ForceFinishMigration: m.Force,
			Source: &memorystorepb.MigrationConfig_SelfManagedSource{SelfManagedSource: &memorystorepb.SelfManagedSource{
				IpAddress: m.IP, Port: m.Port, NetworkAttachment: m.NetworkAttachment}}}
		if m.migrating() {
			out.State = memorystorepb.Instance_MIGRATING
		}
	}
}

// cirPrimary is the primary instance a create asks to be a secondary of,
// "" for none.
func cirPrimary(in *memorystorepb.Instance, mode memorystorepb.Instance_Mode, tlsOn bool) (string, error) {
	cfg := in.GetCrossInstanceReplicationConfig()
	switch cfg.GetInstanceRole() {
	case memorystorepb.CrossInstanceReplicationConfig_INSTANCE_ROLE_UNSPECIFIED, memorystorepb.CrossInstanceReplicationConfig_NONE:
		return "", nil
	case memorystorepb.CrossInstanceReplicationConfig_PRIMARY:
		return "", apierror.InvalidArgument("cross_instance_replication_config.instance_role PRIMARY is not set at create: an instance becomes a primary when a secondary is created with it as primary_instance")
	case memorystorepb.CrossInstanceReplicationConfig_SECONDARY:
	default:
		return "", apierror.InvalidArgument("cross_instance_replication_config.instance_role %s is not an instance role", cfg.GetInstanceRole())
	}
	if mode == memorystorepb.Instance_CLUSTER {
		return "", checkCrossCluster(true)
	}
	if tlsOn {
		return "", apierror.Unimplemented("cross-instance replication with in-transit encryption is not served here: a secondary's nodes would have to trust the primary's CA (CLUSTER_DISABLED instances without in-transit encryption are served)")
	}
	p := cfg.GetPrimaryInstance().GetInstance()
	if instanceRE.FindStringSubmatch(p) == nil {
		return "", apierror.InvalidArgument("cross_instance_replication_config.primary_instance.instance must be projects/{project}/locations/{location}/instances/{instance}, not %q", p)
	}
	return p, nil
}

// checkPrimary checks that p can be r's primary. Call with mu held.
func (c *clusters) checkPrimary(r *clusterRecord, name string) error {
	p := c.byName[name]
	if p == nil {
		return apierror.NotFound("primary instance %s was not found", name)
	}
	switch {
	case !p.standalone():
		return apierror.FailedPrecondition("primary instance %s is in CLUSTER mode: a secondary replicates a primary of its own mode, and only CLUSTER_DISABLED instances are served", name)
	case p.state != clusterpb.Cluster_ACTIVE:
		return apierror.FailedPrecondition("primary instance %s is %s, not ACTIVE", name, p.state)
	case p.Valkey.role() == memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String():
		return apierror.FailedPrecondition("instance %s is itself a secondary: a secondary's primary_instance must be a primary", name)
	case p.TransitCA != "":
		return apierror.Unimplemented("cross-instance replication with in-transit encryption is not served here: primary instance %s has it", name)
	case p.Valkey.AuthMode != r.Valkey.AuthMode:
		return apierror.InvalidArgument("a secondary's authorization_mode must be its primary's, %s", p.Valkey.AuthMode)
	case p.Valkey.Migration.migrating():
		return apierror.FailedPrecondition("primary instance %s is migrating", name)
	}
	r.Valkey.CIR = &cirConfig{Role: memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String(),
		Primary: &remoteInstance{p.Name, p.UID}, Updated: c.now()}
	return nil
}

// setMembership makes p the primary of secs, and saves them all. Call with
// mu held.
func (c *clusters) setMembership(p *clusterRecord, secs []*clusterRecord) {
	now := c.now()
	var rs []remoteInstance
	for _, s := range secs {
		rs = append(rs, remoteInstance{s.Name, s.UID})
	}
	if len(secs) == 0 {
		p.Valkey.CIR = nil
	} else {
		p.Valkey.CIR = &cirConfig{Role: memorystorepb.CrossInstanceReplicationConfig_PRIMARY.String(), Secondaries: rs, Updated: now}
	}
	_ = c.save(p)
	for _, s := range secs {
		s.Valkey.CIR = &cirConfig{Role: memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String(),
			Primary: &remoteInstance{p.Name, p.UID}, Secondaries: rs, Updated: now}
		_ = c.save(s)
	}
}

// secondariesOf are p's secondaries. Call with mu held.
func (c *clusters) secondariesOf(p string) []*clusterRecord {
	var out []*clusterRecord
	for _, r := range c.byName {
		if r.Valkey != nil && r.Valkey.CIR != nil && r.Valkey.CIR.Primary != nil && r.Valkey.CIR.Primary.Name == p &&
			r.Valkey.role() == memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String() {
			out = append(out, r)
		}
	}
	sortRecords(out)
	return out
}

func sortRecords(rs []*clusterRecord) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].Name < rs[j-1].Name; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

// linkTo makes r's primary node a replica of host:port, which takes
// password, and waits until it is linked.
func (c *clusters) linkTo(ctx context.Context, r *clusterRecord, host string, port int, password string) error {
	n := r.firstPrimary()
	if n == nil || n.addr == "" {
		return fmt.Errorf("instance %s has no running primary", r.Name)
	}
	if err := c.do(ctx, r, n, "CONFIG", "SET", "masterauth", password); err != nil {
		return err
	}
	if err := c.do(ctx, r, n, "REPLICAOF", host, port); err != nil {
		return err
	}
	return c.until(ctx, "the replication link to "+net.JoinHostPort(host, strconv.Itoa(port)), func() bool {
		cl := c.dial(n.addr, r.password(), r.TransitCA)
		defer cl.Close()
		info, err := cl.Do(ctx, "INFO", "replication")
		return err == nil && clusterInfoField(fmt.Sprint(info), "master_link_status") == "up"
	})
}

// unlink makes r's primary node a primary again.
func (c *clusters) unlink(ctx context.Context, r *clusterRecord) error {
	n := r.firstPrimary()
	if n == nil || n.addr == "" {
		return nil
	}
	if err := c.do(ctx, r, n, "REPLICAOF", "NO", "ONE"); err != nil {
		return err
	}
	return c.do(ctx, r, n, "CONFIG", "SET", "masterauth", r.password())
}

// linkSecondary links instance name, when it is a secondary, to its
// primary, and records the membership.
func (c *clusters) linkSecondary(ctx context.Context, name string) error {
	c.mu.Lock()
	r := c.byName[name]
	if r == nil || r.Valkey == nil || r.Valkey.role() != memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String() || r.Valkey.CIR.Primary == nil {
		c.mu.Unlock()
		return nil
	}
	p := c.byName[r.Valkey.CIR.Primary.Name]
	if p == nil {
		c.mu.Unlock()
		return fmt.Errorf("primary instance %s was not found", r.Valkey.CIR.Primary.Name)
	}
	pp := p.firstPrimary()
	host, port, pw := pp.podIP, pp.port, p.password()
	snap := *r
	snap.Nodes = append([]*clusterNode(nil), r.Nodes...)
	c.mu.Unlock()
	if host == "" {
		return fmt.Errorf("primary instance %s has no running primary", p.Name)
	}
	if err := c.linkTo(ctx, &snap, host, port, pw); err != nil {
		return fmt.Errorf("replicate primary instance %s: %w", p.Name, err)
	}
	c.mu.Lock()
	if live := c.byName[p.Name]; live != nil {
		c.setMembership(live, c.secondariesOf(p.Name))
	}
	c.mu.Unlock()
	return nil
}

// updateCIR is an UpdateInstance of cross_instance_replication_config:
// a switchover or a detach. Call with mu held; r is ACTIVE and idle.
func (s *MemorystoreServer) updateCIR(c *clusters, r *clusterRecord, want *memorystorepb.CrossInstanceReplicationConfig) (*longrunningpb.Operation, error) {
	role := want.GetInstanceRole()
	if role == memorystorepb.CrossInstanceReplicationConfig_INSTANCE_ROLE_UNSPECIFIED {
		return nil, apierror.InvalidArgument("cross_instance_replication_config.instance_role is required")
	}
	cur := r.Valkey.role()
	if !r.standalone() {
		return nil, checkCrossCluster(true)
	}
	type step struct {
		unlink []*clusterRecord
		link   []*clusterRecord // to the new primary
	}
	var st step
	var primary *clusterRecord
	var members []*clusterRecord
	asked := map[string]bool{}
	for _, x := range want.GetSecondaryInstances() {
		asked[x.GetInstance()] = true
	}
	switch {
	case cur == role.String() && role == memorystorepb.CrossInstanceReplicationConfig_NONE:
		op := c.newOperation(r.Name, "update")
		c.finish(op, renderValkey(r), nil)
		return s.api.operation(op)
	case cur == memorystorepb.CrossInstanceReplicationConfig_NONE.String():
		return nil, apierror.InvalidArgument("instance %s is not in cross-instance replication: a secondary is created with instance_role SECONDARY and its primary_instance", r.Name)
	case cur == memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String() && role == memorystorepb.CrossInstanceReplicationConfig_NONE:
		// Detach this secondary.
		st.unlink = []*clusterRecord{r}
		primary = c.byName[r.Valkey.CIR.Primary.Name]
		for _, x := range c.secondariesOf(r.Valkey.CIR.Primary.Name) {
			if x != r {
				members = append(members, x)
			}
		}
	case cur == memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String() && role == memorystorepb.CrossInstanceReplicationConfig_PRIMARY:
		// Switchover: every other member becomes this one's secondary.
		old := c.byName[r.Valkey.CIR.Primary.Name]
		if old == nil {
			return nil, apierror.FailedPrecondition("primary instance %s was not found", r.Valkey.CIR.Primary.Name)
		}
		others := []*clusterRecord{old}
		for _, x := range c.secondariesOf(old.Name) {
			if x != r {
				others = append(others, x)
			}
		}
		if len(asked) != len(others) {
			return nil, apierror.InvalidArgument("a switchover's secondary_instances must be every other member of the replication: the former primary %s and the other secondaries", old.Name)
		}
		for _, x := range others {
			if !asked[x.Name] {
				return nil, apierror.InvalidArgument("a switchover's secondary_instances must include %s", x.Name)
			}
		}
		st.unlink, st.link, primary, members = []*clusterRecord{r}, others, r, others
	case cur == memorystorepb.CrossInstanceReplicationConfig_PRIMARY.String() && role == memorystorepb.CrossInstanceReplicationConfig_NONE:
		st.unlink, primary = c.secondariesOf(r.Name), r
	case cur == memorystorepb.CrossInstanceReplicationConfig_PRIMARY.String() && role == memorystorepb.CrossInstanceReplicationConfig_PRIMARY:
		primary = r
		for _, x := range c.secondariesOf(r.Name) {
			if asked[x.Name] {
				members = append(members, x)
				delete(asked, x.Name)
			} else {
				st.unlink = append(st.unlink, x)
			}
		}
		for n := range asked {
			return nil, apierror.InvalidArgument("instance %s is not a secondary of %s: a secondary is added by creating it with instance_role SECONDARY", n, r.Name)
		}
	default:
		return nil, apierror.InvalidArgument("instance %s, a %s, cannot become %s: a primary is made by a switchover of one of its secondaries", r.Name, cur, role)
	}
	if primary == nil {
		return nil, apierror.FailedPrecondition("the primary of instance %s was not found", r.Name)
	}
	all := append(append([]*clusterRecord{primary}, st.unlink...), members...)
	for _, x := range all {
		if _, pending := c.busy[x.Name]; pending && x != r {
			return nil, apierror.FailedPrecondition("instance %s has an operation in progress", x.Name)
		}
		if x.state != clusterpb.Cluster_ACTIVE {
			return nil, apierror.FailedPrecondition("instance %s is %s, not ACTIVE", x.Name, x.state)
		}
	}
	op := c.newOperation(r.Name, "update")
	c.busy[r.Name] = op
	r.state = clusterpb.Cluster_UPDATING
	snaps := map[*clusterRecord]*clusterRecord{}
	for _, x := range all {
		sn := *x
		sn.Nodes = append([]*clusterNode(nil), x.Nodes...)
		snaps[x] = &sn
	}
	pp := primary.firstPrimary()
	host, port, pw := pp.podIP, pp.port, primary.password()
	go func() {
		ctx, cancel := context.WithTimeout(c.bg, clusterWait)
		defer cancel()
		var err error
		for _, x := range st.unlink {
			if err = c.unlink(ctx, snaps[x]); err != nil {
				break
			}
		}
		for _, x := range st.link {
			if err != nil {
				break
			}
			err = c.linkTo(ctx, snaps[x], host, port, pw)
		}
		c.mu.Lock()
		delete(c.busy, r.Name)
		r.state = clusterpb.Cluster_ACTIVE
		if err == nil {
			for _, x := range st.unlink {
				if x != primary {
					x.Valkey.CIR = nil
					_ = c.save(x)
				}
			}
			c.setMembership(primary, members)
		}
		out := renderValkey(r)
		c.mu.Unlock()
		c.finish(op, out, err)
	}()
	return s.api.operation(op)
}

// sourceAddr is where the nodes reach a migration's source at ip:port: a
// server of this CloudBurrow instance at its own address inside the
// cluster, any other as it is.
func (s *Server) sourceAddr(ip string, port int) (string, int) {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	for _, c := range []*clusters{s.clusters, s.valkey} {
		if c == nil {
			continue
		}
		c.mu.Lock()
		for _, r := range c.byName {
			for _, n := range r.Nodes {
				if n.addr == addr && n.podIP != "" {
					h, p := n.podIP, n.port
					c.mu.Unlock()
					return h, p
				}
			}
		}
		c.mu.Unlock()
	}
	if s.created != nil {
		if rs, ok := s.created.prov.(ReplicaSet); ok {
			s.created.mu.Lock()
			defer s.created.mu.Unlock()
			for _, r := range s.created.byID {
				if r.addr == addr && r.TransitCA == "" {
					return rs.ClusterAddr(r.Server)
				}
			}
		}
	}
	return ip, port
}

var networkAttachmentRE = regexp.MustCompile(`^projects/[^/]+/regions/[^/]+/networkAttachments/[^/]+$`)

// StartMigration begins a migration from a self-managed server into a
// CLUSTER_DISABLED instance: its primary replicates the source.
func (s *MemorystoreServer) StartMigration(ctx context.Context, req *memorystorepb.StartMigrationRequest) (*longrunningpb.Operation, error) {
	src := req.GetSelfManagedSource()
	if src == nil {
		return nil, apierror.InvalidArgument("self_managed_source is required")
	}
	if net.ParseIP(src.GetIpAddress()) == nil {
		return nil, apierror.InvalidArgument("self_managed_source.ip_address %q is not an IP address", src.GetIpAddress())
	}
	if src.GetPort() < 1 || src.GetPort() > 65535 {
		return nil, apierror.InvalidArgument("self_managed_source.port must be between 1 and 65535, not %d", src.GetPort())
	}
	if !networkAttachmentRE.MatchString(src.GetNetworkAttachment()) {
		return nil, apierror.InvalidArgument("self_managed_source.network_attachment must be projects/{project}/regions/{region}/networkAttachments/{network_attachment}, not %q", src.GetNetworkAttachment())
	}
	if _, err := s.instances(); err != nil {
		return nil, err
	}
	host, port := s.api.sourceAddr(src.GetIpAddress(), int(src.GetPort()))
	c, r, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	switch {
	case !r.standalone():
		return nil, apierror.Unimplemented("migration into an instance in CLUSTER mode is not served here: each source shard would have to replicate to a shard of the instance, whose cluster-mode replicas follow only primaries of their own cluster (CLUSTER_DISABLED instances are served)")
	case r.Valkey.Migration.migrating():
		return nil, apierror.FailedPrecondition("instance %s is already migrating", r.Name)
	case r.Valkey.role() != memorystorepb.CrossInstanceReplicationConfig_NONE.String():
		return nil, apierror.FailedPrecondition("instance %s is in cross-instance replication: detach it first", r.Name)
	case r.TransitCA != "":
		return nil, apierror.Unimplemented("migration into an instance with in-transit encryption is not served here: its primary would replicate the source over TLS")
	}
	if _, pending := c.busy[r.Name]; pending || r.state != clusterpb.Cluster_ACTIVE {
		return nil, apierror.FailedPrecondition("instance %s is %s or has an operation in progress", r.Name, r.state)
	}
	vf := *r.Valkey
	vf.Migration = &migration{IP: src.GetIpAddress(), Port: src.GetPort(), NetworkAttachment: src.GetNetworkAttachment(), host: host, port: port}
	r.Valkey = &vf
	_ = c.save(r)
	op := c.newOperation(r.Name, "startMigration")
	c.busy[r.Name] = op
	snap := *r
	snap.Nodes = append([]*clusterNode(nil), r.Nodes...)
	go func() {
		ctx, cancel := context.WithTimeout(c.bg, clusterWait)
		defer cancel()
		err := c.linkTo(ctx, &snap, host, port, "")
		c.mu.Lock()
		delete(c.busy, snap.Name)
		if err != nil {
			vf.Migration.State = memorystorepb.MigrationConfig_ROLLED_BACK.String()
			_ = c.unlink(ctx, &snap)
			err = fmt.Errorf("replicate the source %s: %w", net.JoinHostPort(vf.Migration.IP, strconv.Itoa(int(vf.Migration.Port))), err)
		} else {
			vf.Migration.State = memorystorepb.MigrationConfig_REPLICATION_ESTABLISHED.String()
		}
		_ = c.save(r)
		out := renderValkey(r)
		c.mu.Unlock()
		c.finish(op, out, err)
	}()
	return s.api.operation(op)
}

// FinishMigration stops replicating the source once the instance has
// caught up with it (unless force), and the instance is its own.
func (s *MemorystoreServer) FinishMigration(ctx context.Context, req *memorystorepb.FinishMigrationRequest) (*longrunningpb.Operation, error) {
	c, r, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	m := r.Valkey.Migration
	if m == nil || m.State != memorystorepb.MigrationConfig_REPLICATION_ESTABLISHED.String() {
		return nil, apierror.FailedPrecondition("instance %s has no migration whose replication is established", r.Name)
	}
	if _, pending := c.busy[r.Name]; pending {
		return nil, apierror.FailedPrecondition("instance %s has an operation in progress", r.Name)
	}
	m.Force = req.GetForce()
	op := c.newOperation(r.Name, "finishMigration")
	c.busy[r.Name] = op
	snap := *r
	snap.Nodes = append([]*clusterNode(nil), r.Nodes...)
	force := req.GetForce()
	go func() {
		ctx, cancel := context.WithTimeout(c.bg, clusterWait)
		defer cancel()
		var err error
		if !force {
			err = c.caughtUp(ctx, &snap, net.JoinHostPort(m.IP, strconv.Itoa(int(m.Port))))
		}
		if err == nil {
			err = c.unlink(ctx, &snap)
		}
		c.mu.Lock()
		delete(c.busy, snap.Name)
		if err == nil {
			m.State = memorystorepb.MigrationConfig_MIGRATED.String()
			_ = c.save(r)
		}
		out := renderValkey(r)
		c.mu.Unlock()
		c.finish(op, out, err)
	}()
	return s.api.operation(op)
}

// caughtUp waits until r's primary's replication offset reaches the
// source's at the time of the call.
func (c *clusters) caughtUp(ctx context.Context, r *clusterRecord, source string) error {
	src := c.dial(source, "", "")
	info, err := src.Do(ctx, "INFO", "replication")
	src.Close()
	if err != nil {
		return apierror.FailedPrecondition("read the source's replication offset at %s: %v (FinishMigration with force skips the check)", source, err)
	}
	want, _ := strconv.ParseInt(clusterInfoField(fmt.Sprint(info), "master_repl_offset"), 10, 64)
	n := r.firstPrimary()
	return c.until(ctx, "the instance to catch up with the source", func() bool {
		cl := c.dial(n.addr, r.password(), r.TransitCA)
		defer cl.Close()
		v, err := cl.Do(ctx, "INFO", "replication")
		if err != nil {
			return false
		}
		got, _ := strconv.ParseInt(clusterInfoField(fmt.Sprint(v), "slave_repl_offset"), 10, 64)
		return got >= want
	})
}

// valkeyVersions orders the engine versions.
var valkeyVersions = map[string]int{"VALKEY_7_2": 1, "VALKEY_8_0": 2, "VALKEY_9_0": 3}

// upgrade moves instance name to version: new nodes of its image, loaded
// with a snapshot of the old ones, which are removed once they serve.
func (c *clusters) upgrade(name, op, version string) {
	ctx, cancel := context.WithTimeout(c.bg, 2*clusterWait)
	defer cancel()
	c.mu.Lock()
	r := c.byName[name]
	snap := *r
	vf := *r.Valkey
	snap.Valkey = &vf
	old := append([]*clusterNode(nil), r.Nodes...)
	c.mu.Unlock()
	var files [][]byte
	var err error
	for _, n := range primariesOf(old) {
		var d []byte
		if d, err = c.snapshotNode(ctx, &snap, n); err != nil {
			err = fmt.Errorf("snapshot shard %d: %w", n.Shard, err)
			break
		}
		files = append(files, d)
	}
	if err == nil {
		vf.EngineVersion = version
		err = c.rebuild(ctx, &snap, files)
	}
	c.mu.Lock()
	delete(c.busy, name)
	r.state, r.message = clusterpb.Cluster_ACTIVE, ""
	if err == nil {
		v := *r.Valkey
		v.EngineVersion, v.Updated = version, c.now()
		r.Valkey, r.Nodes, r.NextNode = &v, snap.Nodes, snap.NextNode
		err = c.save(r)
	}
	secs := c.secondariesOf(name)
	out := renderValkey(r)
	c.mu.Unlock()
	if err == nil {
		c.removeNodes(old)
		// Cross-instance replication follows the new nodes.
		if lerr := c.linkSecondary(ctx, name); lerr != nil {
			err = lerr
		}
		for _, s := range secs {
			if lerr := c.linkSecondary(ctx, s.Name); lerr != nil && err == nil {
				err = lerr
			}
		}
	} else {
		err = fmt.Errorf("the engine upgrade did not complete: %w", err)
	}
	c.finish(op, out, err)
}
