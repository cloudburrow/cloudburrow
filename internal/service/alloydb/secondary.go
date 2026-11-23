package alloydb

// Cross-region replication (#2191), as Google documents it
// (docs.cloud.google.com/alloydb/docs/cross-region-replication/about-cross-region-replication
// and work-with-cross-region-replication, and the REST reference of
// clusters.createsecondary, .promote and .switchover, read 2026-11-23):
//
//   - CreateSecondaryCluster makes a cluster of type SECONDARY whose
//     secondary_config names its primary cluster, which lists it in
//     primary_config.secondary_cluster_names; a primary has at most 5
//     secondaries; another region is recommended, not required; the
//     database version is the primary's, and the backup settings are the
//     primary's unless the request gives its own;
//   - its one SECONDARY instance (CreateSecondaryInstance) replicates the
//     primary cluster's primary instance and takes no writes; read pools
//     may be added to a secondary cluster; read pools are not copied;
//   - PromoteCluster ends replication and makes the secondary a standalone
//     PRIMARY cluster, its secondary instance the primary; the former
//     primary cluster goes on unaffected;
//   - SwitchoverCluster swaps the roles with no data lost: the secondary
//     becomes the primary, the former primary a secondary of it, and the
//     other secondaries follow the new primary.
//
// Here a secondary instance is a streaming replica (in the same
// Kubernetes cluster, wherever its region) of the primary instance's
// PostgreSQL server; a promotion is pg_promote. A switchover stops the
// former primary's server, which hands its standbys every WAL record
// first (a clean shutdown waits for its WAL senders), promotes the
// secondary, and rebuilds the former primary (and the other secondaries,
// and the former primary cluster's read pools) as replicas of the new
// primary.

import (
	"context"
	"fmt"
	"slices"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// maxSecondaries is how many secondary clusters a primary cluster has at
// most (about-cross-region-replication).
const maxSecondaries = 5

func (s *Server) CreateSecondaryCluster(_ context.Context, req *alloydbpb.CreateSecondaryClusterRequest) (*longrunningpb.Operation, error) {
	project, location, err := checkParent("parent", req.GetParent(), false)
	if err != nil {
		return nil, err
	}
	if err := checkID("cluster", req.GetClusterId()); err != nil {
		return nil, err
	}
	c := clone(req.GetCluster())
	if c == nil {
		c = &alloydbpb.Cluster{}
	}
	primaryName := c.GetSecondaryConfig().GetPrimaryClusterName()
	if primaryName == "" {
		return nil, apierror.InvalidArgument("cluster.secondary_config.primary_cluster_name is required")
	}
	name := req.GetParent() + "/clusters/" + req.GetClusterId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	if _, ok := s.clusters[name]; ok {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	p, err := s.cluster(primaryName)
	if err != nil {
		return nil, err
	}
	if p.GetClusterType() != alloydbpb.Cluster_PRIMARY {
		return nil, apierror.InvalidArgument("cluster %s is not a PRIMARY cluster", primaryName)
	}
	if p.GetState() != alloydbpb.Cluster_READY {
		return nil, apierror.FailedPrecondition("invalid cluster state %s", p.GetState())
	}
	if n := len(p.GetPrimaryConfig().GetSecondaryClusterNames()); n >= maxSecondaries {
		return nil, apierror.FailedPrecondition("cluster %s already has %d secondary clusters, the most a primary cluster can have", primaryName, n)
	}
	if v := c.GetDatabaseVersion(); v != alloydbpb.DatabaseVersion_DATABASE_VERSION_UNSPECIFIED && v != p.GetDatabaseVersion() {
		return nil, apierror.InvalidArgument("cluster.database_version %s must be the primary cluster's, %s", v, p.GetDatabaseVersion())
	}
	c.DatabaseVersion = p.GetDatabaseVersion()
	if c.GetAutomatedBackupPolicy() == nil && p.GetAutomatedBackupPolicy() != nil {
		c.AutomatedBackupPolicy = clone(p.GetAutomatedBackupPolicy())
		c.AutomatedBackupPolicy.Location = ""
	}
	if c.GetContinuousBackupConfig() == nil && p.GetContinuousBackupConfig() != nil {
		c.ContinuousBackupConfig = clone(p.GetContinuousBackupConfig())
	}
	if c.GetInitialUser() != nil {
		return nil, apierror.InvalidArgument("cluster.initial_user cannot be set on a secondary cluster: its users are the primary cluster's")
	}
	if err := s.prepareCluster(project, c); err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return s.validateOnly(req.GetParent(), "create", name), nil
	}
	s.stampCluster(name, c, location)
	c.ClusterType = alloydbpb.Cluster_SECONDARY
	c.SecondaryConfig = &alloydbpb.Cluster_SecondaryConfig{PrimaryClusterName: primaryName}
	c.PrimaryConfig = nil
	c.State = alloydbpb.Cluster_READY
	c.Reconciling = false
	s.clusters[name] = c
	s.passwords[name] = s.passwords[primaryName]
	// The users are the primary's: its roles replicate.
	for _, u := range s.usersOf(primaryName) {
		u.Name = name + "/users/" + lastSegment(u.GetName())
		s.users[u.Name] = u
	}
	if p.PrimaryConfig == nil {
		p.PrimaryConfig = &alloydbpb.Cluster_PrimaryConfig{}
	}
	p.PrimaryConfig.SecondaryClusterNames = append(p.PrimaryConfig.SecondaryClusterNames, name)
	return s.doneOp(req.GetParent(), "create", name, req.GetRequestId(), clone(c))
}

func lastSegment(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' {
			return name[i+1:]
		}
	}
	return name
}

func (s *Server) CreateSecondaryInstance(_ context.Context, req *alloydbpb.CreateSecondaryInstanceRequest) (*longrunningpb.Operation, error) {
	in := clone(req.GetInstance())
	if in == nil {
		in = &alloydbpb.Instance{}
	}
	if t := in.GetInstanceType(); t != alloydbpb.Instance_INSTANCE_TYPE_UNSPECIFIED && t != alloydbpb.Instance_SECONDARY {
		return nil, apierror.InvalidArgument("instance.instance_type must be SECONDARY")
	}
	in.InstanceType = alloydbpb.Instance_SECONDARY
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createInstance(&alloydbpb.CreateInstanceRequest{Parent: req.GetParent(), InstanceId: req.GetInstanceId(),
		Instance: in, RequestId: req.GetRequestId(), ValidateOnly: req.GetValidateOnly()})
}

// secondaryOf checks that name is a SECONDARY cluster with a READY
// secondary instance, and answers them with its primary cluster; the
// caller holds mu.
func (s *Server) secondaryOf(name string) (c, p *alloydbpb.Cluster, in *alloydbpb.Instance, err error) {
	c, err = s.cluster(name)
	if err != nil {
		return nil, nil, nil, err
	}
	if c.GetClusterType() != alloydbpb.Cluster_SECONDARY {
		return nil, nil, nil, apierror.FailedPrecondition("cluster %s is not a SECONDARY cluster", name)
	}
	if c.GetState() != alloydbpb.Cluster_READY {
		return nil, nil, nil, apierror.FailedPrecondition("invalid cluster state %s", c.GetState())
	}
	in = s.primaryOf(name)
	if in == nil || in.GetState() != alloydbpb.Instance_READY {
		return nil, nil, nil, apierror.FailedPrecondition("cluster %s has no READY secondary instance", name)
	}
	p = s.clusters[c.GetSecondaryConfig().GetPrimaryClusterName()]
	return c, p, in, nil
}

// unlinkSecondary removes secondary from its primary's list; the caller
// holds mu.
func (s *Server) unlinkSecondary(primary, secondary string) {
	p, ok := s.clusters[primary]
	if !ok || p.GetPrimaryConfig() == nil {
		return
	}
	p.PrimaryConfig.SecondaryClusterNames = slices.DeleteFunc(p.PrimaryConfig.SecondaryClusterNames, func(n string) bool { return n == secondary })
}

func (s *Server) PromoteCluster(_ context.Context, req *alloydbpb.PromoteClusterRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	c, _, in, err := s.secondaryOf(req.GetName())
	if err != nil {
		return nil, err
	}
	if c.GetEtag() != "" && req.GetEtag() != "" && req.GetEtag() != c.GetEtag() {
		return nil, apierror.Aborted("etag %q does not match the cluster's", req.GetEtag())
	}
	if req.GetValidateOnly() {
		return s.validateOnly(locationOf(c.GetName()), "promote", c.GetName()), nil
	}
	op, err := s.startOp(locationOf(c.GetName()), "promote", c.GetName(), req.GetRequestId())
	if err != nil {
		return nil, err
	}
	name, inst := c.GetName(), in.GetName()
	c.State = alloydbpb.Cluster_PROMOTING
	c.Reconciling = true
	in.State = alloydbpb.Instance_MAINTENANCE
	server := serverName(inst)
	// The spec the promoted server keeps: a primary's.
	next := clone(in)
	next.InstanceType = alloydbpb.Instance_PRIMARY
	spec := s.primarySpec(next, c)
	s.background(op.Name, func(ctx context.Context) error {
		if s.opts.Engine == nil {
			return nil
		}
		if err := s.opts.Engine.Promote(ctx, server); err != nil {
			return err
		}
		return s.opts.Engine.Start(ctx, spec)
	}, func(err error) (proto.Message, error) {
		c, ok := s.clusters[name]
		if !ok {
			return nil, err
		}
		c.Reconciling = false
		if err != nil {
			c.State = alloydbpb.Cluster_READY
			if in, ok := s.instances[inst]; ok {
				in.State = alloydbpb.Instance_FAILED
			}
			return nil, err
		}
		s.makePrimary(c, inst)
		s.unlinkPrimaryOf(c)
		c.SecondaryConfig = nil
		c.State = alloydbpb.Cluster_READY
		s.afterPromotion(name, spec)
		return s.clusterView(c), nil
	})
	return op, nil
}

// primarySpec is in's server's spec as cluster c's primary, which a
// promotion makes it; the caller holds mu.
func (s *Server) primarySpec(in *alloydbpb.Instance, c *alloydbpb.Cluster) ServerSpec {
	spec := ServerSpec{Server: serverName(in.GetName()), Version: c.GetDatabaseVersion().String(), Flags: map[string]string{},
		Password: s.passwords[c.GetName()], PITR: c.GetContinuousBackupConfig().GetEnabled(), Pooler: poolerOf(in)}
	for k, v := range in.GetDatabaseFlags() {
		spec.Flags[k] = v
	}
	return spec
}

// makePrimary records cluster c as a PRIMARY cluster whose primary is
// instance inst; the caller holds mu.
func (s *Server) makePrimary(c *alloydbpb.Cluster, inst string) {
	c.ClusterType = alloydbpb.Cluster_PRIMARY
	c.UpdateTime = timestamppb.New(s.now())
	if in, ok := s.instances[inst]; ok {
		in.InstanceType = alloydbpb.Instance_PRIMARY
		in.State = alloydbpb.Instance_READY
		if in.AvailabilityType == alloydbpb.Instance_AVAILABILITY_TYPE_UNSPECIFIED {
			in.AvailabilityType = alloydbpb.Instance_REGIONAL
		}
		in.UpdateTime = c.UpdateTime
		s.stampAddresses(in, c)
	}
	delete(s.epochs, inst)
}

// unlinkPrimaryOf removes secondary c from its primary's list; the
// caller holds mu.
func (s *Server) unlinkPrimaryOf(c *alloydbpb.Cluster) {
	s.unlinkSecondary(c.GetSecondaryConfig().GetPrimaryClusterName(), c.GetName())
}

// afterPromotion starts continuous backup on cluster's new primary, in the
// background; the caller holds mu.
func (s *Server) afterPromotion(cluster string, spec ServerSpec) {
	delete(s.bases, cluster)
	if !spec.PITR || s.opts.Engine == nil {
		return
	}
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.takeBase(ctx, cluster, spec.Server, true); err != nil {
			s.logf("alloydb: the continuous backup base of %s's new primary: %v", cluster, err)
		}
	}()
}

func (s *Server) SwitchoverCluster(_ context.Context, req *alloydbpb.SwitchoverClusterRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	c, p, in, err := s.secondaryOf(req.GetName())
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, apierror.FailedPrecondition("the primary cluster of %s no longer exists", c.GetName())
	}
	old := s.primaryOf(p.GetName())
	if p.GetState() != alloydbpb.Cluster_READY || old == nil || old.GetState() != alloydbpb.Instance_READY {
		// Both regions must be online and the instances healthy.
		return nil, apierror.FailedPrecondition("the primary cluster %s has no READY primary instance; a switchover needs both clusters healthy (promote the secondary instead)", p.GetName())
	}
	if req.GetValidateOnly() {
		return s.validateOnly(locationOf(c.GetName()), "switchover", c.GetName()), nil
	}
	op, err := s.startOp(locationOf(c.GetName()), "switchover", c.GetName(), req.GetRequestId())
	if err != nil {
		return nil, err
	}
	newName, oldName := c.GetName(), p.GetName()
	newInst, oldInst := in.GetName(), old.GetName()
	c.State, p.State = alloydbpb.Cluster_PROMOTING, alloydbpb.Cluster_MAINTENANCE
	c.Reconciling, p.Reconciling = true, true
	in.State, old.State = alloydbpb.Instance_MAINTENANCE, alloydbpb.Instance_MAINTENANCE
	epoch := s.now().UnixNano()

	// The new primary's spec, and the specs of the servers that follow it:
	// the former primary, as a secondary instance; the other secondaries;
	// and the former primary cluster's read pools, which follow it.
	promoted := clone(in)
	promoted.InstanceType = alloydbpb.Instance_PRIMARY
	newSpec := s.primarySpec(promoted, c)
	others := slices.DeleteFunc(slices.Clone(p.GetPrimaryConfig().GetSecondaryClusterNames()), func(n string) bool { return n == newName })
	type follower struct {
		inst string
		spec ServerSpec
	}
	var followers []follower
	follow := func(inst *alloydbpb.Instance, source string, sourceFlags map[string]string) {
		cl := s.clusters[instanceRE.FindStringSubmatch(inst.GetName())[1]]
		sp := s.primarySpec(inst, cl)
		sp.ReplicaOf, sp.PITR, sp.Epoch, sp.Pooler = source, false, epoch, nil
		if inst.GetInstanceType() == alloydbpb.Instance_READ_POOL {
			sp.Pooler = poolerOf(inst)
		}
		floorFlags(sp.Flags, sourceFlags)
		followers = append(followers, follower{inst.GetName(), sp})
	}
	oldAsSecondary := clone(old)
	oldAsSecondary.InstanceType = alloydbpb.Instance_SECONDARY
	follow(oldAsSecondary, newSpec.Server, in.GetDatabaseFlags())
	for _, k := range sortedKeys(s.instances, oldName+"/instances/") {
		if r := s.instances[k]; r.GetInstanceType() == alloydbpb.Instance_READ_POOL {
			follow(r, serverName(oldInst), old.GetDatabaseFlags())
		}
	}
	for _, o := range others {
		if si := s.primaryOf(o); si != nil {
			follow(si, newSpec.Server, in.GetDatabaseFlags())
		}
	}
	oldServer := serverName(oldInst)
	s.background(op.Name, func(ctx context.Context) error {
		e := s.opts.Engine
		if e == nil {
			return nil
		}
		// A clean shutdown sends the standbys every WAL record first, so
		// nothing written to the former primary is lost.
		if err := e.Stop(ctx, oldServer); err != nil {
			return fmt.Errorf("stop the primary: %w", err)
		}
		if err := e.Promote(ctx, newSpec.Server); err != nil {
			return fmt.Errorf("promote the secondary: %w", err)
		}
		if err := e.Start(ctx, newSpec); err != nil {
			return err
		}
		for _, f := range followers {
			if err := e.Start(ctx, f.spec); err != nil {
				return fmt.Errorf("rebuild %s as a replica of the new primary: %w", f.inst, err)
			}
		}
		return nil
	}, func(err error) (proto.Message, error) {
		c, ok1 := s.clusters[newName]
		p, ok2 := s.clusters[oldName]
		if !ok1 || !ok2 {
			return nil, err
		}
		c.Reconciling, p.Reconciling = false, false
		c.State, p.State = alloydbpb.Cluster_READY, alloydbpb.Cluster_READY
		if err != nil {
			for _, n := range []string{newInst, oldInst} {
				if in, ok := s.instances[n]; ok {
					in.State = alloydbpb.Instance_FAILED
				}
			}
			return nil, err
		}
		// The roles swap.
		secondaries := append([]string{oldName}, others...)
		s.makePrimary(c, newInst)
		c.SecondaryConfig = nil
		c.PrimaryConfig = &alloydbpb.Cluster_PrimaryConfig{SecondaryClusterNames: secondaries}
		p.ClusterType = alloydbpb.Cluster_SECONDARY
		p.PrimaryConfig = nil
		p.SecondaryConfig = &alloydbpb.Cluster_SecondaryConfig{PrimaryClusterName: newName}
		p.UpdateTime = c.UpdateTime
		delete(s.bases, oldName)
		for _, o := range others {
			if oc, ok := s.clusters[o]; ok {
				oc.SecondaryConfig = &alloydbpb.Cluster_SecondaryConfig{PrimaryClusterName: newName}
			}
		}
		if in, ok := s.instances[oldInst]; ok {
			in.InstanceType = alloydbpb.Instance_SECONDARY
			in.State = alloydbpb.Instance_READY
			in.UpdateTime = c.UpdateTime
			s.stampAddresses(in, p)
		}
		for _, f := range followers {
			s.epochs[f.inst] = epoch
		}
		s.afterPromotion(newName, newSpec)
		return s.clusterView(c), nil
	})
	return op, nil
}
