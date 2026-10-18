package redisadmin

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
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
// detached. With in-transit encryption (SERVER_AUTHENTICATION, #1563),
// which a secondary takes from its primary, each member keeps its own CA
// and its nodes trust every member's (cirConfig.PeerCAs, given to a node
// at its start and to the running ones by CONFIG SET tls-ca-cert-file
// before a switchover), so they replicate over TLS (tls-replication)
// whichever member is the primary. In CLUSTER mode it keeps its 501: a
// cluster-mode node refuses REPLICAOF ("REPLICAOF not allowed in cluster
// mode", measured by TestMemorystoreValkeyClusterUpgradeKeepsWrites), and a
// cluster-mode replica follows only a primary of its own cluster (CLUSTER
// REPLICATE takes a node ID its gossip knows), so another cluster's shards
// cannot be replicated, even with the same slot layout.
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
// not served: self_managed_source has no credentials (rechecked against
// googleapis' memorystore/v1 proto, #1563), as Google's guide asks for
// AUTH and TLS to be turned off on the source. Migration into CLUSTER mode
// keeps its 501: Google's replicates each source shard into the target's
// shard of the same slots, continuously until FinishMigration, which a
// cluster-mode node cannot do (it refuses REPLICAOF); a keyspace scan
// would copy the keys once and lose the writes made during the migration.
//
// An engine_version update to a later version (VALKEY_7_2, VALKEY_8_0,
// VALKEY_9_0) is a rolling upgrade (#1563): new nodes of the new version's
// image join as replicas of the old ones, the first of each shard is
// promoted once synced (CLUSTER FAILOVER in CLUSTER mode; in CLUSTER_
// DISABLED mode the old primary's writes are paused until the new node has
// its offset, and it then replicates the new one), and the old nodes are
// removed: every write the old nodes acknowledged is kept.

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
	// PeerCAs, PEM, are the members' CAs with in-transit encryption,
	// which each member's nodes trust besides their own.
	PeerCAs string `json:"peerCas,omitempty"`
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
func cirPrimary(in *memorystorepb.Instance, mode memorystorepb.Instance_Mode) (string, error) {
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
	case (p.TransitCA != "") != (r.TransitCA != ""):
		return apierror.InvalidArgument("a secondary's transit_encryption_mode must be its primary's: primary instance %s has %s", name, transitMode(p))
	case p.Valkey.AuthMode != r.Valkey.AuthMode:
		return apierror.InvalidArgument("a secondary's authorization_mode must be its primary's, %s", p.Valkey.AuthMode)
	case p.Valkey.Migration.migrating():
		return apierror.FailedPrecondition("primary instance %s is migrating", name)
	}
	if r.TransitCA != "" {
		if _, ok := c.rt.(Shell); !ok {
			return apierror.Unimplemented("cross-instance replication with in-transit encryption is not supported by this instance's servers")
		}
	}
	r.Valkey.CIR = &cirConfig{Role: memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String(),
		Primary: &remoteInstance{p.Name, p.UID}, Updated: c.now(), PeerCAs: trustOf(append(c.membersOf(p), r))}
	return nil
}

func transitMode(r *clusterRecord) memorystorepb.Instance_TransitEncryptionMode {
	if r.TransitCA != "" {
		return memorystorepb.Instance_SERVER_AUTHENTICATION
	}
	return memorystorepb.Instance_TRANSIT_ENCRYPTION_DISABLED
}

// membersOf is p, a primary, and its secondaries. Call with mu held.
func (c *clusters) membersOf(p *clusterRecord) []*clusterRecord {
	return append([]*clusterRecord{p}, c.secondariesOf(p.Name)...)
}

// trustOf is the CAs of every member of rs with in-transit encryption, one
// PEM bundle: what each member's nodes trust, so a node replicates over TLS
// from whichever member is the primary, before and after a switchover.
func trustOf(rs []*clusterRecord) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, x := range rs {
		if x.TransitCA != "" && !seen[x.TransitCA] {
			seen[x.TransitCA] = true
			b.WriteString(x.TransitCA)
		}
	}
	return b.String()
}

// peerCAs is what r's nodes trust besides r's own CA: the CAs of its
// cross-instance replication's members.
func (r *clusterRecord) peerCAs() string {
	if r.Valkey == nil || r.Valkey.CIR == nil {
		return ""
	}
	return r.Valkey.CIR.PeerCAs
}

// trustFile is where a node's CA bundle with its peers' CAs is written.
const trustFile = "/tmp/tls/trust.crt"

// shareTrust makes every node of rs trust bundle, every member's CA, on
// its running server (CONFIG SET tls-ca-cert-file); a node started later
// is given it by spec. A no-op without in-transit encryption.
func (c *clusters) shareTrust(ctx context.Context, rs []*clusterRecord, bundle string) error {
	if bundle == "" {
		return nil
	}
	sh, ok := c.rt.(Shell)
	if !ok {
		return fmt.Errorf("the servers cannot be given their peers' CAs")
	}
	for _, r := range rs {
		if r.TransitCA == "" {
			continue
		}
		for _, n := range r.Nodes {
			if out, err := sh.Exec(ctx, n.Server, nil, "sh", "-c", `printf '%s' "$1" > `+trustFile, "sh", bundle); err != nil {
				return fmt.Errorf("write node %s's CAs: %v: %s", n.Server, err, out)
			}
			if err := c.do(ctx, r, n, "CONFIG", "SET", "tls-ca-cert-file", trustFile); err != nil {
				return err
			}
		}
	}
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
	trust := trustOf(append([]*clusterRecord{p}, secs...))
	if len(secs) == 0 {
		p.Valkey.CIR = nil
	} else {
		p.Valkey.CIR = &cirConfig{Role: memorystorepb.CrossInstanceReplicationConfig_PRIMARY.String(), Secondaries: rs, Updated: now, PeerCAs: trust}
	}
	_ = c.save(p)
	for _, s := range secs {
		s.Valkey.CIR = &cirConfig{Role: memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String(),
			Primary: &remoteInstance{p.Name, p.UID}, Secondaries: rs, Updated: now, PeerCAs: trust}
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
	src := c.sourceOf(p)
	snap := *r
	snap.Nodes = append([]*clusterNode(nil), r.Nodes...)
	c.mu.Unlock()
	if src.host == "" {
		return fmt.Errorf("primary instance %s has no running primary", p.Name)
	}
	if err := c.publish(ctx, src); err != nil {
		return fmt.Errorf("publish primary instance %s's roles: %w", p.Name, err)
	}
	if err := c.linkTo(ctx, &snap, src.host, src.port, src.password); err != nil {
		return fmt.Errorf("replicate primary instance %s: %w", p.Name, err)
	}
	c.mu.Lock()
	if live := c.byName[p.Name]; live != nil {
		c.setMembership(live, c.secondariesOf(p.Name))
	}
	c.mu.Unlock()
	return nil
}

// cirSource is where a secondary of cross-instance replication reaches
// its primary instance's primary in the cluster.
type cirSource struct {
	host     string
	port     int
	password string
	// base and roles, with a RoleRouter, are the primary instance's role
	// Services and its nodes' roles, published before a secondary links.
	base  string
	roles map[string]string
}

// sourceOf is where a secondary replicates p (#1614): with a RoleRouter
// and a replica, p's primary Service (base+"-primary"), which selects
// whichever node is p's primary, so the secondary follows p's automatic
// failover (valkeyfailover.go) and needs no relinking; with a RoleRouter
// alone, the primary node's own Service, which follows its pod; else its
// pod's IP. The nodes' certificates cover each of these Services' names.
// Call with mu held.
func (c *clusters) sourceOf(p *clusterRecord) cirSource {
	pp := p.firstPrimary()
	if pp == nil || pp.podIP == "" {
		return cirSource{}
	}
	src := cirSource{host: pp.podIP, port: pp.port, password: p.password()}
	rr, ok := c.rt.(RoleRouter)
	switch {
	case !ok:
	case len(p.Nodes) > 1:
		src.base, src.roles = ValkeyServerBase(p.Name), rolesOf(p)
		src.host, src.port = rr.NodeHost(src.base+"-primary"), rolePort(p)
	default:
		src.host = rr.NodeHost(pp.Server)
	}
	return src
}

// publish labels the source's roles and applies its role Services, so the
// primary Service selects the primary before a secondary links to it; the
// health check keeps them (tendRoles).
func (c *clusters) publish(ctx context.Context, src cirSource) error {
	rr, ok := c.rt.(RoleRouter)
	if !ok || src.roles == nil {
		return nil
	}
	return rr.Roles(ctx, src.base, src.roles, src.port)
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
	src := c.sourceOf(primary)
	var linked []*clusterRecord
	for _, x := range st.link {
		linked = append(linked, snaps[x])
	}
	trust := trustOf(append([]*clusterRecord{primary}, members...))
	go func() {
		ctx, cancel := context.WithTimeout(c.bg, clusterWait)
		defer cancel()
		// Over TLS, the members that will replicate the new primary trust
		// its CA first.
		err := c.shareTrust(ctx, append([]*clusterRecord{snaps[primary]}, linked...), trust)
		if err == nil && len(st.link) > 0 {
			// A former secondary, never watched, has no role Services yet.
			err = c.publish(ctx, src)
		}
		for _, x := range st.unlink {
			if err != nil {
				break
			}
			err = c.unlink(ctx, snaps[x])
		}
		for _, x := range st.link {
			if err != nil {
				break
			}
			err = c.linkTo(ctx, snaps[x], src.host, src.port, src.password)
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
			if r.primaryAddr == addr {
				// A CLUSTER_DISABLED instance's primary endpoint
				// (valkeyfailover.go): its primary now.
				if p := r.firstPrimary(); p != nil && p.podIP != "" {
					h, pt := c.nodeHost(p), p.port
					c.mu.Unlock()
					return h, pt
				}
			}
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
		return nil, apierror.Unimplemented("migration into an instance in CLUSTER mode is not served here: each source shard would have to replicate to a shard of the instance, and a cluster-mode node refuses REPLICAOF (CLUSTER_DISABLED instances are served)")
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

// upgrade moves instance name to version as a rolling upgrade does: new
// nodes of the version's image join as replicas of the old ones, each
// shard's first new node is promoted once it has caught up, and the old
// nodes, which serve and replicate every write until then, are removed.
func (c *clusters) upgrade(name, op, version string) {
	ctx, cancel := context.WithTimeout(c.bg, 2*clusterWait)
	defer cancel()
	c.mu.Lock()
	r := c.byName[name]
	snap := *r
	vf := *r.Valkey
	vf.EngineVersion = version
	snap.Valkey = &vf
	old := append([]*clusterNode(nil), r.Nodes...)
	snap.Nodes = append([]*clusterNode(nil), old...)
	var fresh []*clusterNode
	for sh := 0; sh < int(snap.ShardCount); sh++ {
		for k := 0; k <= int(snap.ReplicaCount); k++ {
			fresh = append(fresh, c.newNode(&snap, sh, false))
		}
	}
	c.mu.Unlock()
	err := c.roll(ctx, &snap, old, fresh)
	c.mu.Lock()
	delete(c.busy, name)
	r.state, r.message = clusterpb.Cluster_ACTIVE, ""
	if err == nil {
		v := *r.Valkey
		v.EngineVersion, v.Updated = version, c.now()
		r.Valkey, r.Nodes, r.NextNode = &v, fresh, snap.NextNode
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
		c.removeNodes(fresh)
		err = fmt.Errorf("the engine upgrade did not complete: %w", err)
	}
	c.finish(op, out, err)
}

// roll moves r's data from its old nodes onto fresh ones: fresh replicate
// old, the first of each shard is promoted when it has caught up with its
// shard's primary, and the old nodes leave. r.Nodes are old and fresh;
// fresh's Primary is set on the promoted ones.
func (c *clusters) roll(ctx context.Context, r *clusterRecord, old, fresh []*clusterNode) error {
	if err := c.startNodes(ctx, r, fresh); err != nil {
		return err
	}
	if !r.standalone() {
		if err := c.meet(ctx, r, r.firstPrimaryOf(old), fresh); err != nil {
			return err
		}
	}
	if err := c.replicate(ctx, r, fresh, old); err != nil {
		return err
	}
	if err := c.settle(ctx, r, r.Nodes); err != nil {
		return err
	}
	primary := map[int]*clusterNode{}
	for _, n := range old {
		if n.Primary {
			primary[n.Shard] = n
		}
	}
	lead := map[int]*clusterNode{}
	for _, n := range fresh {
		if lead[n.Shard] == nil {
			lead[n.Shard] = n
		}
	}
	for sh, n := range lead {
		if err := c.promote(ctx, r, primary[sh], n); err != nil {
			return fmt.Errorf("promote shard %d's new node: %w", sh, err)
		}
		c.mu.Lock()
		n.Primary = true
		c.mu.Unlock()
	}
	if err := c.replicate(ctx, r, fresh, fresh); err != nil {
		return err
	}
	return c.forget(ctx, r, fresh, old)
}

// firstPrimaryOf is the primary of the lowest shard among nodes.
func (r *clusterRecord) firstPrimaryOf(nodes []*clusterNode) *clusterNode {
	ps := primariesOf(nodes)
	if len(ps) == 0 {
		return nodes[0]
	}
	return ps[0]
}

// promote makes n, a replica of old, its shard's primary without losing a
// write: in CLUSTER mode a CLUSTER FAILOVER, which pauses old's clients
// until n has its offset; in CLUSTER_DISABLED mode old's writes are paused
// (CLIENT PAUSE WRITE) until n has caught up, n stops replicating, and old
// becomes its replica, so a write sent to old after it is refused
// (READONLY) rather than lost.
func (c *clusters) promote(ctx context.Context, r *clusterRecord, old, n *clusterNode) error {
	if old == nil {
		return fmt.Errorf("node %s's shard has no primary", n.Server)
	}
	role := func(x *clusterNode) string {
		cl := c.dial(x.addr, r.password(), r.TransitCA)
		defer cl.Close()
		info, err := cl.Do(ctx, "INFO", "replication")
		if err != nil {
			return ""
		}
		return clusterInfoField(fmt.Sprint(info), "role")
	}
	if !r.standalone() {
		if err := c.do(ctx, r, n, "CLUSTER", "FAILOVER"); err != nil {
			return err
		}
		return c.until(ctx, "node "+n.Server+" to take over its shard", func() bool {
			return role(n) == "master" && role(old) == "slave"
		})
	}
	if err := c.do(ctx, r, old, "CLIENT", "PAUSE", int(clusterWait/time.Millisecond), "WRITE"); err != nil {
		return err
	}
	defer func() { _ = c.do(context.WithoutCancel(ctx), r, old, "CLIENT", "UNPAUSE") }()
	offset := func(x *clusterNode, field string) int64 {
		cl := c.dial(x.addr, r.password(), r.TransitCA)
		defer cl.Close()
		info, err := cl.Do(ctx, "INFO", "replication")
		if err != nil {
			return -1
		}
		v, _ := strconv.ParseInt(clusterInfoField(fmt.Sprint(info), field), 10, 64)
		return v
	}
	want := offset(old, "master_repl_offset")
	if err := c.until(ctx, "node "+n.Server+" to catch up with its shard's primary", func() bool {
		return offset(n, "slave_repl_offset") >= want
	}); err != nil {
		return err
	}
	if err := c.do(ctx, r, n, "REPLICAOF", "NO", "ONE"); err != nil {
		return err
	}
	return c.do(ctx, r, old, "REPLICAOF", n.podIP, n.port)
}
