package redisadmin

import (
	"context"
	"fmt"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Cross-region replication of Memorystore for Redis Cluster (#1577), the
// Redis Cluster API's cross_cluster_replication_config, as Google
// documents it (https://docs.cloud.google.com/memorystore/docs/cluster/about-cross-region-replication
// and working-with-cross-region-replication): a secondary is created with
// cluster_role SECONDARY and primary_cluster, and copies its primary's
// shard count, node type, authorization and in-transit encryption modes,
// redis_configs and persistence (replica count, zones, maintenance,
// deletion protection and backups are its own); a primary has up to two
// secondaries, and a secondary is never a primary of its own. Switchover
// and detach are UpdateCluster of cross_cluster_replication_config, as
// gcloud redis clusters switchover, detach and detach-secondaries send
// them: a secondary updated to PRIMARY with every other member as its
// secondary_clusters, a secondary updated to NONE, and a primary updated
// to fewer secondary_clusters, or to NONE. Every member's membership lists
// the primary and the secondaries.
//
// The replication is the one Memorystore for Valkey's CLUSTER mode
// instances use (crossclusterlink.go): a link per shard copies the
// primary's shard into the secondary's shard of the same slots and applies
// its replication stream, and the secondary's clients are refused writes.
//
// A change of shard_count (and, for a Valkey instance, engine_version) is
// made on the primary, and Google scales the secondaries to match it
// ("Memorystore for Redis Cluster syncs these changes with the secondary
// clusters automatically"): the links stop, the primary and then each
// secondary are reshaped (or upgraded), and the links start again, each
// copying its primary shard anew into the secondary's shard of the same
// slots (scaleGroup). Node type, redis_configs and persistence set on a
// Redis cluster primary are set on its secondaries too; on a secondary
// these are refused, as they are the primary's.

// maxSecondaries is how many secondaries a primary has at most: "You can
// have up to two secondary clusters."
const maxSecondaries = 2

// cir is r's cross-instance (Valkey) or cross-cluster (Redis) replication
// config, nil for none.
func (r *clusterRecord) cir() *cirConfig {
	if r.Valkey != nil {
		return r.Valkey.CIR
	}
	return r.CCR
}

func (r *clusterRecord) setCIR(x *cirConfig) {
	if r.Valkey != nil {
		r.Valkey.CIR = x
		return
	}
	r.CCR = x
}

// cirRole is r's role: NONE, PRIMARY or SECONDARY.
func (r *clusterRecord) cirRole() string {
	if x := r.cir(); x != nil && x.Role != "" {
		return x.Role
	}
	return "NONE"
}

func (r *clusterRecord) linkPassword() string {
	if r.Valkey != nil {
		return r.Valkey.LinkPassword
	}
	return r.CCRLinkPassword
}

func (r *clusterRecord) setLinkPassword(p string) {
	if r.Valkey != nil {
		r.Valkey.LinkPassword = p
		return
	}
	r.CCRLinkPassword = p
}

// dropCIR leaves r, a snapshot, with no replication config, so its nodes
// take every user's writes (crrUsers); the record it was taken of keeps
// its own.
func (r *clusterRecord) dropCIR() {
	if r.Valkey != nil {
		v := *r.Valkey
		v.CIR = nil
		r.Valkey = &v
		return
	}
	r.CCR = nil
}

func remoteCluster(x remoteInstance) *clusterpb.CrossClusterReplicationConfig_RemoteCluster {
	return &clusterpb.CrossClusterReplicationConfig_RemoteCluster{Cluster: x.Name, Uid: x.UID}
}

// renderCCR sets out's cross_cluster_replication_config.
func renderCCR(r *clusterRecord, out *clusterpb.Cluster) {
	cfg := r.CCR
	if cfg == nil || cfg.Role == "" {
		return
	}
	c := &clusterpb.CrossClusterReplicationConfig{
		ClusterRole: clusterpb.CrossClusterReplicationConfig_ClusterRole(clusterpb.CrossClusterReplicationConfig_ClusterRole_value[cfg.Role]),
		UpdateTime:  timestamppb.New(cfg.Updated),
		Membership:  &clusterpb.CrossClusterReplicationConfig_Membership{},
	}
	var secs []*clusterpb.CrossClusterReplicationConfig_RemoteCluster
	for _, s := range cfg.Secondaries {
		secs = append(secs, remoteCluster(s))
	}
	c.Membership.SecondaryClusters = secs
	if cfg.Role == "PRIMARY" {
		c.SecondaryClusters = secs
		c.Membership.PrimaryCluster = remoteCluster(remoteInstance{r.Name, r.UID})
	} else if cfg.Primary != nil {
		c.PrimaryCluster = remoteCluster(*cfg.Primary)
		c.Membership.PrimaryCluster = remoteCluster(*cfg.Primary)
	}
	out.CrossClusterReplicationConfig = c
}

// ccrSecondary reads a create's cross_cluster_replication_config: for a
// secondary, in with its primary's shard count, node type, authorization
// and in-transit encryption modes, redis_configs and persistence, which a
// secondary copies, and the primary's name; else in and "".
func (c *clusters) ccrSecondary(in *clusterpb.Cluster) (*clusterpb.Cluster, string, error) {
	cfg := in.GetCrossClusterReplicationConfig()
	switch cfg.GetClusterRole() {
	case clusterpb.CrossClusterReplicationConfig_CLUSTER_ROLE_UNSPECIFIED, clusterpb.CrossClusterReplicationConfig_NONE:
		if cfg.GetPrimaryCluster() != nil || len(cfg.GetSecondaryClusters()) > 0 {
			return nil, "", apierror.InvalidArgument("cross_cluster_replication_config.primary_cluster and secondary_clusters are set only with cluster_role SECONDARY")
		}
		return in, "", nil
	case clusterpb.CrossClusterReplicationConfig_PRIMARY:
		return nil, "", apierror.InvalidArgument("cross_cluster_replication_config.cluster_role PRIMARY is not set at create: a cluster becomes a primary when a secondary is created with it as primary_cluster")
	case clusterpb.CrossClusterReplicationConfig_SECONDARY:
	default:
		return nil, "", apierror.InvalidArgument("cross_cluster_replication_config.cluster_role %s is not a cluster role", cfg.GetClusterRole())
	}
	if len(cfg.GetSecondaryClusters()) > 0 {
		return nil, "", apierror.InvalidArgument("cross_cluster_replication_config.secondary_clusters is set only on a primary")
	}
	name := cfg.GetPrimaryCluster().GetCluster()
	if clusterRE.FindStringSubmatch(name) == nil {
		return nil, "", apierror.InvalidArgument("cross_cluster_replication_config.primary_cluster.cluster must be projects/{project}/locations/{location}/clusters/{cluster}, not %q", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.byName[name]
	if p == nil {
		return nil, "", apierror.NotFound("primary cluster %s was not found", name)
	}
	pc := c.render(p)
	out := proto.Clone(in).(*clusterpb.Cluster)
	out.ShardCount = proto.Int32(p.ShardCount)
	out.NodeType = pc.GetNodeType()
	out.AuthorizationMode = pc.GetAuthorizationMode()
	out.TransitEncryptionMode = pc.GetTransitEncryptionMode()
	out.RedisConfigs = copyMap(p.RedisConfigs)
	out.PersistenceConfig = pc.GetPersistenceConfig()
	return out, name, nil
}

// checkPrimaryCluster checks that cluster name can be r's primary, and
// makes r its secondary. Call with mu held.
func (c *clusters) checkPrimaryCluster(r *clusterRecord, name string) error {
	p := c.byName[name]
	switch {
	case p == nil:
		return apierror.NotFound("primary cluster %s was not found", name)
	case p.state != clusterpb.Cluster_ACTIVE:
		return apierror.FailedPrecondition("primary cluster %s is %s, not ACTIVE", name, p.state)
	case c.busy[name] != "":
		return apierror.FailedPrecondition("primary cluster %s has an operation in progress", name)
	case p.cirRole() == "SECONDARY":
		// "Memorystore for Redis Cluster doesn't support cascading
		// replicas."
		return apierror.FailedPrecondition("cluster %s is itself a secondary: a secondary's primary_cluster must be a primary", name)
	case len(c.secondariesOf(name)) >= maxSecondaries:
		return apierror.FailedPrecondition("primary cluster %s has %d secondary clusters already, the most a primary has", name, maxSecondaries)
	case p.ShardCount != r.ShardCount:
		return apierror.FailedPrecondition("primary cluster %s's shard_count changed during the create", name)
	}
	r.CCRLinkPassword = newUID() + newUID()
	r.setCIR(&cirConfig{Role: "SECONDARY", Primary: &remoteInstance{p.Name, p.UID}, Updated: c.now(),
		PeerCAs: trustOf(append(c.membersOf(p), r))})
	return nil
}

// updateCCR is an UpdateCluster of cross_cluster_replication_config: a
// switchover or a detach (updateReplication). CLUSTER_ROLE_UNSPECIFIED is
// NONE, as the API documents it.
func (s *ClusterServer) updateCCR(c *clusters, r *clusterRecord, want *clusterpb.CrossClusterReplicationConfig) (*longrunningpb.Operation, error) {
	role := want.GetClusterRole()
	if role == clusterpb.CrossClusterReplicationConfig_CLUSTER_ROLE_UNSPECIFIED {
		role = clusterpb.CrossClusterReplicationConfig_NONE
	}
	var asked []string
	for _, x := range want.GetSecondaryClusters() {
		asked = append(asked, x.GetCluster())
	}
	return c.updateReplication(s.api, r, role.String(), asked)
}

// replicaGroup is what a change of r's shard count or engine version
// scales with it: none for a member of no replication or in
// CLUSTER_DISABLED mode, r's secondaries for a primary; a secondary's is
// refused, as its primary's change is copied to it. Call with mu held.
func (c *clusters) replicaGroup(r *clusterRecord, what string) ([]*clusterRecord, error) {
	if r.standalone() {
		return nil, nil
	}
	w := c.words()
	switch r.cirRole() {
	case "SECONDARY":
		p := ""
		if r.cir().Primary != nil {
			p = r.cir().Primary.Name
		}
		return nil, apierror.FailedPrecondition("%s of secondary %s %s is its primary's: change it on primary %s %s, whose secondaries are scaled with it", what, w.kind, r.Name, w.kind, p)
	case "PRIMARY":
		secs := c.secondariesOf(r.Name)
		if err := c.checkIdle(secs); err != nil {
			return nil, err
		}
		return secs, nil
	}
	return nil, nil
}

// launchGroup starts scaleGroup on r and its secondaries, secs, and
// returns op's operation. Call with mu held.
func (c *clusters) launchGroup(api *Server, r *clusterRecord, op string, secs []*clusterRecord, shards int32, version string) (*longrunningpb.Operation, error) {
	all := append([]*clusterRecord{r}, secs...)
	marked := c.markBusy(all, op)
	var names []string
	for _, x := range all {
		x.state = clusterpb.Cluster_UPDATING
		if version != "" {
			x.message = "upgrading the engine to " + version
		} else {
			x.message, x.pendingShards = "changing the shard count with the primary", shards
		}
		if x != r {
			names = append(names, x.Name)
		}
	}
	return api.launch(op, func() { c.scaleGroup(r.Name, op, marked, names, shards, version) })
}

// scaleGroup reshapes (shards) or upgrades (version) primary name and then
// each of its secondaries, secs, with their replication links stopped,
// starts the links again, and completes op.
func (c *clusters) scaleGroup(name, op string, marked, secs []string, shards int32, version string) {
	ctx, cancel := context.WithTimeout(c.bg, time.Duration(2+len(secs))*clusterWait)
	defer cancel()
	for _, s := range secs {
		c.crossClusterUnlink(s)
	}
	replicas := func(n string) int32 {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.byName[n].ReplicaCount
	}
	var err error
	for i, n := range append([]string{name}, secs...) {
		if err != nil {
			break
		}
		if version != "" {
			err = c.upgradeNodes(ctx, n, version)
		} else {
			// A secondary's replica count is its own.
			err = c.reshapeNodes(ctx, n, shards, replicas(n), i > 0)
		}
	}
	// The links start again whatever happened, each copying its primary
	// shard anew.
	for _, s := range secs {
		if lerr := c.linkSecondary(ctx, s); lerr != nil && err == nil {
			err = fmt.Errorf("replicate %s into %s again: %w", name, s, lerr)
		}
	}
	c.mu.Lock()
	c.clearBusy(marked, op)
	for _, n := range append([]string{name}, secs...) {
		if x := c.byName[n]; x != nil {
			x.state, x.message, x.pendingShards, x.pendingReplicas = clusterpb.Cluster_ACTIVE, "", 0, -1
		}
	}
	out := c.output(c.byName[name])
	c.mu.Unlock()
	c.finish(op, out, err)
}

// detachForDelete takes r, a secondary being deleted, out of its
// primary's membership; a primary with secondaries is not deleted. Call
// with mu held.
func (c *clusters) detachForDelete(r *clusterRecord) error {
	w := c.words()
	if len(c.secondariesOf(r.Name)) > 0 {
		return apierror.FailedPrecondition("%s %s is the primary of %s: detach or delete its secondaries first", w.kind, r.Name, w.replication)
	}
	if r.cirRole() == "SECONDARY" && r.cir().Primary != nil {
		if p := c.byName[r.cir().Primary.Name]; p != nil {
			var rest []*clusterRecord
			for _, x := range c.secondariesOf(p.Name) {
				if x != r {
					rest = append(rest, x)
				}
			}
			c.setMembership(p, rest)
		}
	}
	return nil
}

// propagateSettings gives r's secondaries the node type, redis_configs
// and persistence a Redis cluster primary was just updated to, on their
// records and running nodes, as Google syncs them. Call with mu held.
func (c *clusters) propagateSettings(ctx context.Context, r *clusterRecord, settings map[string]string) error {
	for _, s := range c.secondariesOf(r.Name) {
		s.NodeType, s.RedisConfigs, s.Persistence = r.NodeType, copyMap(r.RedisConfigs), r.Persistence
		if err := c.save(s); err != nil {
			return err
		}
		for _, n := range s.Nodes {
			for k, v := range settings {
				if err := c.do(ctx, s, n, "CONFIG", "SET", k, v); err != nil {
					return fmt.Errorf("secondary cluster %s: %w", s.Name, err)
				}
			}
		}
	}
	return nil
}
