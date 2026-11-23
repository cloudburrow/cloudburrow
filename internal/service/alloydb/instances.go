package alloydb

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// cpuCounts are the vCPU counts an instance's machine may have
// (docs.cloud.google.com/alloydb/docs/instance-configure, read 2026-11-23:
// N2 machines of 2 to 128 vCPUs, and C4A machines of 1 vCPU).
var cpuCounts = map[int32]bool{1: true, 2: true, 4: true, 8: true, 16: true, 32: true, 48: true, 64: true, 72: true, 96: true, 128: true}

// maxReadPoolNodes is a read pool's largest node count.
const maxReadPoolNodes = 20

// connectorHost is the address the connectors dial.
func (s *Server) connectorHost() string {
	if s.opts.ConnectorHost != "" {
		return s.opts.ConnectorHost
	}
	return "127.0.0.1"
}

func (s *Server) CreateInstance(ctx context.Context, req *alloydbpb.CreateInstanceRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createInstance(req)
}

// createInstance creates one instance; the caller holds mu.
func (s *Server) createInstance(req *alloydbpb.CreateInstanceRequest) (*longrunningpb.Operation, error) {
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	c, err := s.cluster(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := checkID("instance", req.GetInstanceId()); err != nil {
		return nil, err
	}
	in := clone(req.GetInstance())
	if in == nil {
		in = &alloydbpb.Instance{}
	}
	name := req.GetParent() + "/instances/" + req.GetInstanceId()
	if _, ok := s.instances[name]; ok {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	if c.GetState() != alloydbpb.Cluster_READY {
		return nil, apierror.FailedPrecondition("cluster %s is %s, not READY", c.GetName(), c.GetState())
	}
	primary := s.primaryOf(c.GetName())
	secondary := c.GetClusterType() == alloydbpb.Cluster_SECONDARY
	switch in.GetInstanceType() {
	case alloydbpb.Instance_PRIMARY:
		if secondary {
			// UNVERIFIED: Google's message.
			// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/cross-region-replication/work-with-cross-region-replication.)
			return nil, apierror.InvalidArgument("cluster %s is a SECONDARY cluster: its writable instance is a SECONDARY instance (CreateSecondaryInstance)", c.GetName())
		}
		if primary != nil {
			// UNVERIFIED: Google's message.
			// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/reference/rest.)
			return nil, apierror.AlreadyExists("cluster %s already has a primary instance, %s", c.GetName(), primary.GetName())
		}
		if in.GetReadPoolConfig() != nil {
			return nil, apierror.InvalidArgument("read_pool_config is only for READ_POOL instances")
		}
	case alloydbpb.Instance_READ_POOL:
		if primary == nil {
			return nil, apierror.FailedPrecondition("cluster %s has no primary instance; create it before a read pool", c.GetName())
		}
		n := in.GetReadPoolConfig().GetNodeCount()
		if n < 1 || n > maxReadPoolNodes {
			return nil, apierror.InvalidArgument("read_pool_config.node_count must be between 1 and %d", maxReadPoolNodes)
		}
	case alloydbpb.Instance_SECONDARY:
		if !secondary {
			return nil, apierror.InvalidArgument("cluster %s is a PRIMARY cluster: a SECONDARY instance belongs to a SECONDARY cluster", c.GetName())
		}
		if primary != nil {
			return nil, apierror.AlreadyExists("cluster %s already has a secondary instance, %s", c.GetName(), primary.GetName())
		}
		if in.GetReadPoolConfig() != nil {
			return nil, apierror.InvalidArgument("read_pool_config is only for READ_POOL instances")
		}
		src := s.primaryOf(c.GetSecondaryConfig().GetPrimaryClusterName())
		if src == nil || src.GetState() != alloydbpb.Instance_READY {
			return nil, apierror.FailedPrecondition("the primary cluster %s has no READY primary instance to replicate", c.GetSecondaryConfig().GetPrimaryClusterName())
		}
		if in.GetMachineConfig() == nil {
			// The primary's machine, by default (work-with-cross-region-replication).
			in.MachineConfig = clone(src.GetMachineConfig())
		}
	default:
		return nil, apierror.InvalidArgument("instance.instance_type must be PRIMARY, READ_POOL or SECONDARY")
	}
	if err := checkMachine(in); err != nil {
		return nil, err
	}
	if err := checkPooling(in); err != nil {
		return nil, err
	}
	if in.GetGceZone() != "" && in.GetAvailabilityType() == alloydbpb.Instance_REGIONAL {
		return nil, apierror.InvalidArgument("gce_zone can only be set for ZONAL instances")
	}
	if err := checkFlags(in.GetDatabaseFlags(), c.GetDatabaseVersion()); err != nil {
		return nil, err
	}
	if s.opts.Engine == nil {
		return nil, apierror.FailedPrecondition("AlloyDB instances need the instance's cluster: start CloudBurrow with --services alloydb")
	}
	if req.GetValidateOnly() {
		return s.validateOnly(locationOf(name), "create", name), nil
	}
	s.stampInstance(name, in, c)
	s.instances[name] = in
	op, err := s.startOp(locationOf(name), "create", name, req.GetRequestId())
	if err != nil {
		return nil, err
	}
	spec, err := s.specOf(in)
	if err != nil {
		return nil, err
	}
	cluster := c.GetName()
	restore := s.restores[cluster]
	isPrimary := in.GetInstanceType() == alloydbpb.Instance_PRIMARY
	s.background(op.Name, func(ctx context.Context) error {
		e := s.opts.Engine
		if err := e.Start(ctx, spec); err != nil {
			return err
		}
		if !isPrimary {
			return nil
		}
		// The roles first: the restored data's grants name them.
		if err := s.setupPrimary(ctx, cluster, spec.Server); err != nil {
			return err
		}
		if restore != "" {
			if err := e.Load(ctx, spec.Server, restore); err != nil {
				return fmt.Errorf("load the restored data: %w", err)
			}
		}
		if spec.PITR {
			if err := s.takeBase(ctx, cluster, spec.Server, true); err != nil {
				return fmt.Errorf("the continuous backup base: %w", err)
			}
		}
		return nil
	}, func(err error) (proto.Message, error) {
		in, ok := s.instances[name]
		if !ok {
			return nil, err
		}
		in.Reconciling = false
		if err != nil {
			in.State = alloydbpb.Instance_FAILED
			return nil, err
		}
		in.State = alloydbpb.Instance_READY
		in.UpdateTime = timestamppb.New(s.now())
		if isPrimary && restore != "" {
			delete(s.restores, cluster)
		}
		return clone(in), nil
	})
	return op, nil
}

func checkMachine(in *alloydbpb.Instance) error {
	mc := in.GetMachineConfig()
	if mc == nil {
		mc = &alloydbpb.Instance_MachineConfig{}
		in.MachineConfig = mc
	}
	if mc.CpuCount == 0 && mc.MachineType != "" {
		var n int32
		i := strings.LastIndex(mc.MachineType, "-")
		if _, err := fmt.Sscanf(mc.MachineType[i+1:], "%d", &n); err != nil || i < 0 {
			return apierror.InvalidArgument("machine_config.machine_type %q is not an AlloyDB machine type", mc.MachineType)
		}
		mc.CpuCount = n
	}
	if mc.CpuCount == 0 {
		// UNVERIFIED: the API's default; gcloud requires --cpu-count.
		// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/reference/rest.)
		mc.CpuCount = 2
	}
	if !cpuCounts[mc.CpuCount] {
		return apierror.InvalidArgument("machine_config.cpu_count %d is not supported: use 1, 2, 4, 8, 16, 32, 48, 64, 72, 96 or 128", mc.CpuCount)
	}
	if mc.MachineType == "" {
		if mc.CpuCount == 1 {
			mc.MachineType = "c4a-highmem-1"
		} else {
			mc.MachineType = fmt.Sprintf("n2-highmem-%d", mc.CpuCount)
		}
	}
	return nil
}

// stampInstance gives a new instance its output fields; the caller holds
// mu.
func (s *Server) stampInstance(name string, in *alloydbpb.Instance, c *alloydbpb.Cluster) {
	now := timestamppb.New(s.now())
	in.Name = name
	in.Uid = newUID()
	in.CreateTime, in.UpdateTime = now, now
	in.State = alloydbpb.Instance_CREATING
	in.Reconciling = true
	in.IpAddress, in.PublicIpAddress = "", "" // OUTPUT_ONLY
	if in.AvailabilityType == alloydbpb.Instance_AVAILABILITY_TYPE_UNSPECIFIED && in.GetInstanceType() == alloydbpb.Instance_PRIMARY {
		in.AvailabilityType = alloydbpb.Instance_REGIONAL
	}
	if in.ActivationPolicy == alloydbpb.Instance_ACTIVATION_POLICY_UNSPECIFIED {
		in.ActivationPolicy = alloydbpb.Instance_ALWAYS
	}
	if in.QueryInsightsConfig == nil {
		// Query insights' documented defaults (query-insights-overview).
		in.QueryInsightsConfig = &alloydbpb.Instance_QueryInsightsInstanceConfig{RecordApplicationTags: proto.Bool(true),
			RecordClientAddress: proto.Bool(true), QueryStringLength: 1024, QueryPlansPerMinute: proto.Uint32(5)}
	}
	if in.ClientConnectionConfig == nil {
		in.ClientConnectionConfig = &alloydbpb.Instance_ClientConnectionConfig{}
	}
	if in.ClientConnectionConfig.SslConfig == nil {
		in.ClientConnectionConfig.SslConfig = &alloydbpb.SslConfig{SslMode: alloydbpb.SslConfig_ENCRYPTED_ONLY,
			CaSource: alloydbpb.SslConfig_CA_SOURCE_MANAGED}
	}
	s.stampAddresses(in, c)
	stampPooling(in)
	in.Etag = etag()
}

// stampAddresses sets an instance's addresses and nodes.
func (s *Server) stampAddresses(in *alloydbpb.Instance, c *alloydbpb.Cluster) {
	region := ""
	if m := instanceRE.FindStringSubmatch(in.GetName()); m != nil {
		region = m[3]
	}
	host := s.connectorHost()
	if in.GetIpAddress() == "" && !c.GetPscConfig().GetPscEnabled() {
		in.IpAddress = s.allocateAddress()
	}
	if in.GetIpAddress() != "" {
		host = in.GetIpAddress()
	}
	in.PublicIpAddress = ""
	if in.GetNetworkConfig().GetEnablePublicIp() {
		in.PublicIpAddress = host
	}
	if c.GetPscConfig().GetPscEnabled() {
		if in.PscInstanceConfig == nil {
			in.PscInstanceConfig = &alloydbpb.Instance_PscInstanceConfig{}
		}
		in.PscInstanceConfig.PscDnsName = PSCDNSName(in.GetName())
		in.PscInstanceConfig.ServiceAttachmentLink = fmt.Sprintf("projects/%d/regions/%s/serviceAttachments/alloydb-%s",
			c.GetPscConfig().GetServiceOwnedProjectNumber(), region, strings.ReplaceAll(in.GetUid(), "-", "")[:12])
		in.IpAddress = ""
	}
	zone := in.GetGceZone()
	if zone == "" {
		zone = region + "-a"
	}
	id := func(n int) string {
		return fmt.Sprintf("%s-%s", in.GetName()[strings.LastIndex(in.GetName(), "/")+1:], serverName(in.GetName())[8:12]+fmt.Sprint(n))
	}
	in.Nodes, in.WritableNode = nil, nil
	switch in.GetInstanceType() {
	case alloydbpb.Instance_PRIMARY:
		in.WritableNode = &alloydbpb.Instance_Node{ZoneId: zone, Id: id(0), Ip: host, State: "RUNNING"}
		if in.GetAvailabilityType() == alloydbpb.Instance_REGIONAL {
			standby := region + "-b"
			if zone == standby {
				standby = region + "-a"
			}
			in.Nodes = []*alloydbpb.Instance_Node{{ZoneId: standby, Id: id(1), Ip: host, State: "RUNNING"}}
		}
	case alloydbpb.Instance_READ_POOL:
		zones := []string{region + "-a", region + "-b", region + "-c"}
		for i := int32(0); i < in.GetReadPoolConfig().GetNodeCount(); i++ {
			in.Nodes = append(in.Nodes, &alloydbpb.Instance_Node{ZoneId: zones[int(i)%len(zones)], Id: id(int(i)), Ip: host, State: "RUNNING"})
		}
	}
}

// PSCDNSName is an instance's Private Service Connect DNS name: Google's
// form ({id}.{hash}.{region}.alloydb-psc.goog) under .localhost, which
// resolves to the loopback address with no DNS set-up, so the connectors
// reach this instance's server-side proxy and name the instance in TLS.
func PSCDNSName(instance string) string {
	m := instanceRE.FindStringSubmatch(instance)
	if m == nil {
		return ""
	}
	h := serverName(instance)[8:]
	return m[5] + "-" + h[:8] + "." + h[8:] + "." + m[3] + ".alloydb-psc.localhost"
}

// specOf is an instance's server; the caller holds mu.
// standbyFloorFlags are the settings a standby needs at least as high as
// its primary's
// (postgresql.org/docs/current/hot-standby.html#HOT-STANDBY-ADMIN).
var standbyFloorFlags = []string{"max_connections", "max_prepared_transactions", "max_locks_per_transaction", "max_wal_senders", "max_worker_processes"}

func (s *Server) specOf(in *alloydbpb.Instance) (ServerSpec, error) {
	m := instanceRE.FindStringSubmatch(in.GetName())
	if m == nil {
		return ServerSpec{}, apierror.InvalidArgument("instance %q", in.GetName())
	}
	c, ok := s.clusters[m[1]]
	if !ok {
		return ServerSpec{}, apierror.NotFound("cluster %s", m[1])
	}
	spec := ServerSpec{Server: serverName(in.GetName()), Version: c.GetDatabaseVersion().String(),
		Flags: map[string]string{}, Password: s.passwords[m[1]]}
	for k, v := range in.GetDatabaseFlags() {
		spec.Flags[k] = v
	}
	spec.Epoch = s.epochs[in.GetName()]
	if t := in.GetInstanceType(); t == alloydbpb.Instance_READ_POOL || t == alloydbpb.Instance_SECONDARY {
		source := m[1]
		if t == alloydbpb.Instance_SECONDARY {
			source = c.GetSecondaryConfig().GetPrimaryClusterName()
		}
		p := s.primaryOf(source)
		if p == nil {
			return ServerSpec{}, apierror.FailedPrecondition("cluster %s has no primary instance", source)
		}
		spec.ReplicaOf = serverName(p.GetName())
		// A hot standby refuses to recover with a lower setting of these
		// than its primary's (PostgreSQL's "insufficient parameter
		// settings"), so a read pool's nodes take the primary's where it
		// is higher.
		for _, k := range standbyFloorFlags {
			pv, ok := p.GetDatabaseFlags()[k]
			if !ok {
				continue
			}
			pn, _ := strconv.ParseInt(pv, 10, 64)
			if own, ok := spec.Flags[k]; ok {
				if n, err := strconv.ParseInt(own, 10, 64); err == nil && n >= pn {
					continue
				}
			}
			spec.Flags[k] = pv
		}
	} else {
		spec.PITR = c.GetContinuousBackupConfig().GetEnabled()
	}
	spec.Pooler = poolerOf(in)
	return spec, nil
}

func (s *Server) GetInstance(_ context.Context, req *alloydbpb.GetInstanceRequest) (*alloydbpb.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.instance(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(in), nil
}

// instance is the named instance; the caller holds mu.
func (s *Server) instance(name string) (*alloydbpb.Instance, error) {
	if instanceRE.FindStringSubmatch(name) == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/clusters/{cluster}/instances/{instance}", name)
	}
	in, ok := s.instances[name]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return in, nil
}

func (s *Server) ListInstances(_ context.Context, req *alloydbpb.ListInstancesRequest) (*alloydbpb.ListInstancesResponse, error) {
	parent := req.GetParent()
	m := clusterRE.FindStringSubmatch(parent)
	if m == nil {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}/clusters/{cluster}", parent)
	}
	s.mu.Lock()
	var all []*alloydbpb.Instance
	for _, k := range sortedKeys(s.instances, "") {
		im := instanceRE.FindStringSubmatch(k)
		if im[2] != m[1] || (m[2] != "-" && im[3] != m[2]) || (m[3] != "-" && im[4] != m[3]) {
			continue
		}
		all = append(all, clone(s.instances[k]))
	}
	if m[2] != "-" && m[3] != "-" {
		if _, ok := s.clusters[parent]; !ok {
			s.mu.Unlock()
			return nil, apierror.NotFound("Resource '%s' was not found", parent)
		}
	}
	s.mu.Unlock()
	out, next, err := listPage("alloydb-instances:"+parent, req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &alloydbpb.ListInstancesResponse{Instances: out, NextPageToken: next}, nil
}

// instancePaths are the fields UpdateInstance changes, by mask path, and
// whether a change restarts the server.
var instancePaths = map[string]struct {
	set     func(dst, src *alloydbpb.Instance)
	restart bool
}{
	"display_name":             {func(d, s *alloydbpb.Instance) { d.DisplayName = s.DisplayName }, false},
	"labels":                   {func(d, s *alloydbpb.Instance) { d.Labels = s.Labels }, false},
	"annotations":              {func(d, s *alloydbpb.Instance) { d.Annotations = s.Annotations }, false},
	"database_flags":           {func(d, s *alloydbpb.Instance) { d.DatabaseFlags = s.DatabaseFlags }, true},
	"machine_config":           {func(d, s *alloydbpb.Instance) { d.MachineConfig = s.MachineConfig }, true},
	"read_pool_config":         {func(d, s *alloydbpb.Instance) { d.ReadPoolConfig = s.ReadPoolConfig }, false},
	"availability_type":        {func(d, s *alloydbpb.Instance) { d.AvailabilityType = s.AvailabilityType }, false},
	"query_insights_config":    {func(d, s *alloydbpb.Instance) { d.QueryInsightsConfig = s.QueryInsightsConfig }, false},
	"observability_config":     {func(d, s *alloydbpb.Instance) { d.ObservabilityConfig = s.ObservabilityConfig }, false},
	"client_connection_config": {func(d, s *alloydbpb.Instance) { d.ClientConnectionConfig = s.ClientConnectionConfig }, false},
	"network_config":           {func(d, s *alloydbpb.Instance) { d.NetworkConfig = s.NetworkConfig }, false},
	"activation_policy":        {func(d, s *alloydbpb.Instance) { d.ActivationPolicy = s.ActivationPolicy }, false},
	"psc_instance_config":      {func(d, s *alloydbpb.Instance) { d.PscInstanceConfig = s.PscInstanceConfig }, false},
	"connection_pool_config":   {func(d, s *alloydbpb.Instance) { d.ConnectionPoolConfig = s.ConnectionPoolConfig }, true},
	"gce_zone":                 {func(d, s *alloydbpb.Instance) { d.GceZone = s.GceZone }, false},
}

func (s *Server) UpdateInstance(ctx context.Context, req *alloydbpb.UpdateInstanceRequest) (*longrunningpb.Operation, error) {
	src := req.GetInstance()
	m := instanceRE.FindStringSubmatch(src.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("instance.name %q must be projects/{project}/locations/{location}/clusters/{cluster}/instances/{instance}", src.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	cur, ok := s.instances[src.GetName()]
	if !ok {
		if req.GetAllowMissing() {
			return s.createInstance(&alloydbpb.CreateInstanceRequest{Parent: m[1], InstanceId: m[5], Instance: src,
				RequestId: req.GetRequestId(), ValidateOnly: req.GetValidateOnly()})
		}
		return nil, apierror.NotFound("Resource '%s' was not found", src.GetName())
	}
	if src.GetEtag() != "" && src.GetEtag() != cur.GetEtag() {
		return nil, apierror.Aborted("etag %q does not match the instance's", src.GetEtag())
	}
	if cur.GetState() != alloydbpb.Instance_READY && cur.GetState() != alloydbpb.Instance_STOPPED {
		return nil, apierror.FailedPrecondition("instance %s is %s; an update needs it READY", cur.GetName(), cur.GetState())
	}
	next := clone(cur)
	restart := false
	var paths []string
	for p := range instancePaths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range maskPaths(req.GetUpdateMask().GetPaths(), paths) {
		top, _, _ := strings.Cut(p, ".")
		f, ok := instancePaths[top]
		if !ok {
			return nil, apierror.InvalidArgument("update_mask path %q is not a field UpdateInstance can change", p)
		}
		if top == "machine_config" && strings.Contains(p, ".") {
			if next.MachineConfig == nil {
				next.MachineConfig = &alloydbpb.Instance_MachineConfig{}
			}
			next.MachineConfig.CpuCount = src.GetMachineConfig().GetCpuCount()
			next.MachineConfig.MachineType = src.GetMachineConfig().GetMachineType()
		} else {
			f.set(next, src)
		}
		if f.restart {
			restart = true
		}
	}
	if next.GetInstanceType() == alloydbpb.Instance_READ_POOL {
		if n := next.GetReadPoolConfig().GetNodeCount(); n < 1 || n > maxReadPoolNodes {
			return nil, apierror.InvalidArgument("read_pool_config.node_count must be between 1 and %d", maxReadPoolNodes)
		}
	}
	if next.GetMachineConfig().GetCpuCount() != cur.GetMachineConfig().GetCpuCount() && next.GetMachineConfig().GetMachineType() == cur.GetMachineConfig().GetMachineType() {
		next.MachineConfig.MachineType = ""
	}
	if err := checkMachine(next); err != nil {
		return nil, err
	}
	if err := checkPooling(next); err != nil {
		return nil, err
	}
	stampPooling(next)
	c := s.clusters[m[1]]
	if err := checkFlags(next.GetDatabaseFlags(), c.GetDatabaseVersion()); err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return s.validateOnly(locationOf(cur.GetName()), "update", cur.GetName()), nil
	}
	stop := cur.GetActivationPolicy() != alloydbpb.Instance_NEVER && next.GetActivationPolicy() == alloydbpb.Instance_NEVER
	start := cur.GetActivationPolicy() == alloydbpb.Instance_NEVER && next.GetActivationPolicy() != alloydbpb.Instance_NEVER
	if stop && next.GetInstanceType() == alloydbpb.Instance_PRIMARY {
		for _, k := range sortedKeys(s.instances, m[1]+"/instances/") {
			if p := s.instances[k]; p.GetInstanceType() == alloydbpb.Instance_READ_POOL && p.GetActivationPolicy() != alloydbpb.Instance_NEVER {
				return nil, apierror.FailedPrecondition("stop the cluster's read pool instances before its primary")
			}
		}
	}
	next.UpdateTime = timestamppb.New(s.now())
	next.Etag = etag()
	s.stampAddresses(next, c)
	op, err := s.startOp(locationOf(cur.GetName()), "update", cur.GetName(), req.GetRequestId())
	if err != nil {
		return nil, err
	}
	name := cur.GetName()
	if !restart && !stop && !start {
		s.instances[name] = next
		s.finishOp(op.Name, clone(next), nil)
		return clone(s.ops[op.Name]), nil
	}
	prev := cur
	next.State = alloydbpb.Instance_MAINTENANCE
	if stop {
		next.State = alloydbpb.Instance_STOPPED
	}
	s.instances[name] = next
	spec, err := s.specOf(next)
	if err != nil {
		return nil, err
	}
	s.background(op.Name, func(ctx context.Context) error {
		if s.opts.Engine == nil {
			return nil
		}
		if stop {
			return s.opts.Engine.Stop(ctx, spec.Server)
		}
		return s.opts.Engine.Start(ctx, spec)
	}, func(err error) (proto.Message, error) {
		in, ok := s.instances[name]
		if !ok {
			return nil, err
		}
		if err != nil {
			s.instances[name] = prev
			return nil, err
		}
		if stop {
			in.State = alloydbpb.Instance_STOPPED
		} else {
			in.State = alloydbpb.Instance_READY
		}
		return clone(in), nil
	})
	return op, nil
}

func (s *Server) DeleteInstance(_ context.Context, req *alloydbpb.DeleteInstanceRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	in, err := s.instance(req.GetName())
	if err != nil {
		return nil, err
	}
	if req.GetEtag() != "" && req.GetEtag() != in.GetEtag() {
		return nil, apierror.Aborted("etag %q does not match the instance's", req.GetEtag())
	}
	m := instanceRE.FindStringSubmatch(in.GetName())
	if in.GetInstanceType() == alloydbpb.Instance_SECONDARY {
		// UNVERIFIED: Google's message; Terraform deletes a secondary
		// cluster with its secondary instance by deletion_policy FORCE.
		// (re-read 2026-11-23: registry.terraform.io/providers/hashicorp/google/latest/docs/resources/alloydb_instance.)
		return nil, apierror.FailedPrecondition("instance %s is a secondary instance: delete its secondary cluster with force, or promote the cluster first", in.GetName())
	}
	if in.GetInstanceType() == alloydbpb.Instance_PRIMARY {
		for _, k := range sortedKeys(s.instances, m[1]+"/instances/") {
			if s.instances[k].GetInstanceType() == alloydbpb.Instance_READ_POOL {
				// UNVERIFIED: Google's message.
				// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/reference/rest.)
				return nil, apierror.FailedPrecondition("cluster %s has read pool instances; delete them before its primary", m[1])
			}
		}
	}
	if req.GetValidateOnly() {
		return s.validateOnly(locationOf(in.GetName()), "delete", in.GetName()), nil
	}
	in.State = alloydbpb.Instance_DELETING
	op, err := s.startOp(locationOf(in.GetName()), "delete", in.GetName(), req.GetRequestId())
	if err != nil {
		return nil, err
	}
	name, srv := in.GetName(), serverName(in.GetName())
	s.background(op.Name, func(ctx context.Context) error {
		if s.opts.Engine == nil {
			return nil
		}
		return s.opts.Engine.Remove(ctx, srv)
	}, func(err error) (proto.Message, error) {
		if err != nil {
			if in, ok := s.instances[name]; ok {
				in.State = alloydbpb.Instance_FAILED
			}
			return nil, err
		}
		if in, ok := s.instances[name]; ok {
			s.releaseAddress(in)
		}
		delete(s.instances, name)
		delete(s.epochs, name)
		return nil, nil
	})
	return op, nil
}

func (s *Server) BatchCreateInstances(_ context.Context, req *alloydbpb.BatchCreateInstancesRequest) (*longrunningpb.Operation, error) {
	reqs := req.GetRequests().GetCreateInstanceRequests()
	if len(reqs) == 0 {
		return nil, apierror.InvalidArgument("requests.create_instance_requests must name at least one instance")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	// The primary first, so a read pool in the same batch finds it.
	sorted := append([]*alloydbpb.CreateInstanceRequest{}, reqs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].GetInstance().GetInstanceType() == alloydbpb.Instance_PRIMARY &&
			sorted[j].GetInstance().GetInstanceType() != alloydbpb.Instance_PRIMARY
	})
	var created []string
	for _, r := range sorted {
		if r.GetParent() != "" && r.GetParent() != req.GetParent() {
			return nil, apierror.InvalidArgument("every request's parent must be %s", req.GetParent())
		}
		r = clone(r)
		r.Parent = req.GetParent()
		r.RequestId = ""
		op, err := s.createInstance(r)
		if err != nil {
			return nil, err
		}
		created = append(created, op.GetName())
	}
	op, err := s.startOp(locationOf(req.GetParent()), "create", req.GetParent(), req.GetRequestId())
	if err != nil {
		return nil, err
	}
	s.background(op.Name, func(ctx context.Context) error {
		s.waitOps(ctx, created)
		return nil
	}, func(error) (proto.Message, error) {
		resp := &alloydbpb.BatchCreateInstancesResponse{}
		for _, r := range sorted {
			name := req.GetParent() + "/instances/" + r.GetInstanceId()
			if in, ok := s.instances[name]; ok {
				resp.Instances = append(resp.Instances, clone(in))
			}
		}
		for _, o := range created {
			if e := s.ops[o].GetError(); e != nil {
				return nil, apierror.Internal(fmt.Errorf("%s", e.GetMessage()), "an instance of the batch failed")
			}
		}
		return resp, nil
	})
	return op, nil
}

// waitOps waits until every named operation is done.
func (s *Server) waitOps(ctx context.Context, names []string) {
	for {
		s.mu.Lock()
		all := true
		for _, n := range names {
			if op, ok := s.ops[n]; ok && !op.GetDone() {
				all = false
			}
		}
		s.mu.Unlock()
		if all || ctx.Err() != nil {
			return
		}
		t := timeAfter(200)
		select {
		case <-ctx.Done():
			return
		case <-t:
		}
	}
}

// restartLike runs a restart of an instance's server, for RestartInstance,
// FailoverInstance and InjectFault; swap exchanges a REGIONAL primary's
// nodes.
func (s *Server) restartLike(verb, name, requestID string, validate, swap bool, check func(*alloydbpb.Instance) error) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(requestID); ok {
		return op, nil
	}
	in, err := s.instance(name)
	if err != nil {
		return nil, err
	}
	if in.GetState() != alloydbpb.Instance_READY {
		return nil, apierror.FailedPrecondition("instance %s is %s, not READY", name, in.GetState())
	}
	if check != nil {
		if err := check(in); err != nil {
			return nil, err
		}
	}
	if validate {
		return s.validateOnly(locationOf(name), verb, name), nil
	}
	op, err := s.startOp(locationOf(name), verb, name, requestID)
	if err != nil {
		return nil, err
	}
	in.State = alloydbpb.Instance_MAINTENANCE
	srv := serverName(name)
	s.background(op.Name, func(ctx context.Context) error {
		if s.opts.Engine == nil {
			return nil
		}
		return s.opts.Engine.Restart(ctx, srv)
	}, func(err error) (proto.Message, error) {
		in, ok := s.instances[name]
		if !ok {
			return nil, err
		}
		in.State = alloydbpb.Instance_READY
		if err != nil {
			in.State = alloydbpb.Instance_FAILED
			return nil, err
		}
		if swap && in.WritableNode != nil && len(in.Nodes) > 0 {
			in.WritableNode.ZoneId, in.Nodes[0].ZoneId = in.Nodes[0].ZoneId, in.WritableNode.ZoneId
			in.WritableNode.Id, in.Nodes[0].Id = in.Nodes[0].Id, in.WritableNode.Id
		}
		in.UpdateTime = timestamppb.New(s.now())
		return clone(in), nil
	})
	return op, nil
}

func (s *Server) RestartInstance(_ context.Context, req *alloydbpb.RestartInstanceRequest) (*longrunningpb.Operation, error) {
	return s.restartLike("restart", req.GetName(), req.GetRequestId(), req.GetValidateOnly(), false, func(in *alloydbpb.Instance) error {
		ids := map[string]bool{}
		for _, n := range append([]*alloydbpb.Instance_Node{in.GetWritableNode()}, in.GetNodes()...) {
			ids[n.GetId()] = true
		}
		for _, id := range req.GetNodeIds() {
			if !ids[id] {
				return apierror.InvalidArgument("node_ids: %q is not a node of instance %s", id, in.GetName())
			}
		}
		if len(req.GetNodeIds()) > 0 && in.GetInstanceType() != alloydbpb.Instance_READ_POOL {
			return apierror.InvalidArgument("node_ids may only be given for a READ_POOL instance")
		}
		return nil
	})
}

func (s *Server) FailoverInstance(_ context.Context, req *alloydbpb.FailoverInstanceRequest) (*longrunningpb.Operation, error) {
	return s.restartLike("failover", req.GetName(), req.GetRequestId(), req.GetValidateOnly(), true, func(in *alloydbpb.Instance) error {
		if in.GetInstanceType() != alloydbpb.Instance_PRIMARY || in.GetAvailabilityType() != alloydbpb.Instance_REGIONAL {
			// UNVERIFIED: Google's message.
			// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/reference/rest.)
			return apierror.FailedPrecondition("instance %s is not a REGIONAL primary instance; only those can fail over", in.GetName())
		}
		return nil
	})
}

func (s *Server) InjectFault(_ context.Context, req *alloydbpb.InjectFaultRequest) (*longrunningpb.Operation, error) {
	if req.GetFaultType() != alloydbpb.InjectFaultRequest_STOP_VM {
		return nil, apierror.InvalidArgument("fault_type must be STOP_VM")
	}
	return s.restartLike("injectFault", req.GetName(), req.GetRequestId(), req.GetValidateOnly(), true, func(in *alloydbpb.Instance) error {
		if in.GetInstanceType() != alloydbpb.Instance_PRIMARY {
			return apierror.FailedPrecondition("instance %s is not a primary instance", in.GetName())
		}
		return nil
	})
}

func (s *Server) GetConnectionInfo(_ context.Context, req *alloydbpb.GetConnectionInfoRequest) (*alloydbpb.ConnectionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.instance(req.GetParent())
	if err != nil {
		return nil, err
	}
	if in.GetState() != alloydbpb.Instance_READY {
		return nil, apierror.FailedPrecondition("instance %s is %s, not READY", in.GetName(), in.GetState())
	}
	return &alloydbpb.ConnectionInfo{Name: in.GetName() + "/connectionInfo", IpAddress: in.GetIpAddress(),
		PublicIpAddress: in.GetPublicIpAddress(), InstanceUid: in.GetUid()}, nil
}

// hostOnly is host without a port.
func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
