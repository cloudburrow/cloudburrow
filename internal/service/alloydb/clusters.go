package alloydb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// DefaultVersion is a new cluster's database version when the request
// names none. UNVERIFIED: the API reference says only that "a default
// database version will be used"; gcloud's reference names POSTGRES_16.
const DefaultVersion = alloydbpb.DatabaseVersion_POSTGRES_16

// supportedVersions are the major versions a new cluster may have: those
// Google lists as generally available for new clusters
// (docs.cloud.google.com/alloydb/docs/db-version-policies, read
// 2026-11-23), each served by the PostgreSQL image of that version.
var supportedVersions = map[alloydbpb.DatabaseVersion]bool{
	alloydbpb.DatabaseVersion_POSTGRES_14: true,
	alloydbpb.DatabaseVersion_POSTGRES_15: true,
	alloydbpb.DatabaseVersion_POSTGRES_16: true,
	alloydbpb.DatabaseVersion_POSTGRES_17: true,
}

// defaultRecoveryWindowDays is continuous backup's default window.
const defaultRecoveryWindowDays = 14

// allDays is continuous backup's schedule: every day.
var allDays = []dayofweek.DayOfWeek{dayofweek.DayOfWeek_MONDAY, dayofweek.DayOfWeek_TUESDAY, dayofweek.DayOfWeek_WEDNESDAY,
	dayofweek.DayOfWeek_THURSDAY, dayofweek.DayOfWeek_FRIDAY, dayofweek.DayOfWeek_SATURDAY, dayofweek.DayOfWeek_SUNDAY}

// builtinUser is a cluster's initial user when the request names none.
const builtinUser = "postgres"

// SuperuserRole is the role AlloyDB's built-in users are granted.
const SuperuserRole = "alloydbsuperuser"

func (s *Server) CreateCluster(_ context.Context, req *alloydbpb.CreateClusterRequest) (*longrunningpb.Operation, error) {
	project, location, err := checkParent("parent", req.GetParent(), false)
	if err != nil {
		return nil, err
	}
	_ = project
	if err := checkID("cluster", req.GetClusterId()); err != nil {
		return nil, err
	}
	c := clone(req.GetCluster())
	if c == nil {
		c = &alloydbpb.Cluster{}
	}
	if err := s.prepareCluster(c); err != nil {
		return nil, err
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
	if req.GetValidateOnly() {
		return s.validateOnly(req.GetParent(), "create", name), nil
	}
	password := c.GetInitialUser().GetPassword()
	user := c.GetInitialUser().GetUser()
	s.stampCluster(name, c, location)
	s.clusters[name] = c
	s.passwords[name] = password
	s.addBuiltinUser(name, user)
	op, err := s.startOp(req.GetParent(), "create", name, req.GetRequestId())
	if err != nil {
		return nil, err
	}
	// A cluster has no server of its own: it is ready once recorded.
	c.State = alloydbpb.Cluster_READY
	c.Reconciling = false
	s.finishOp(op.Name, clone(c), nil)
	return clone(s.ops[op.Name]), nil
}

// prepareCluster checks a new cluster's fields and fills their defaults.
func (s *Server) prepareCluster(c *alloydbpb.Cluster) error {
	if c.GetDatabaseVersion() == alloydbpb.DatabaseVersion_DATABASE_VERSION_UNSPECIFIED {
		c.DatabaseVersion = DefaultVersion
	}
	if !supportedVersions[c.GetDatabaseVersion()] {
		return apierror.InvalidArgument("database version %s is not supported for new clusters", c.GetDatabaseVersion())
	}
	network := c.GetNetworkConfig().GetNetwork()
	if network == "" {
		network = c.GetNetwork()
	}
	if network == "" && !c.GetPscConfig().GetPscEnabled() {
		// UNVERIFIED: Google's message. A cluster needs a VPC network
		// for private services access, or Private Service Connect.
		return apierror.InvalidArgument("cluster.network_config.network is required unless Private Service Connect is enabled (cluster.psc_config.psc_enabled)")
	}
	if network != "" {
		if c.NetworkConfig == nil {
			c.NetworkConfig = &alloydbpb.Cluster_NetworkConfig{}
		}
		c.NetworkConfig.Network = network
		c.Network = network
	}
	if cb := c.GetContinuousBackupConfig(); cb != nil && cb.RecoveryWindowDays != 0 &&
		(cb.RecoveryWindowDays < 1 || cb.RecoveryWindowDays > 35) {
		return apierror.InvalidArgument("continuous_backup_config.recovery_window_days must be between 1 and 35")
	}
	if k := c.GetEncryptionConfig().GetKmsKeyName(); k != "" && !kmsKeyRE.MatchString(k) {
		return apierror.InvalidArgument("encryption_config.kms_key_name %q must be projects/{project}/locations/{location}/keyRings/{key_ring}/cryptoKeys/{key}", k)
	}
	return nil
}

// stampCluster gives a new cluster its output fields; the caller holds mu.
func (s *Server) stampCluster(name string, c *alloydbpb.Cluster, location string) {
	now := timestamppb.New(s.now())
	c.Name = name
	c.Uid = newUID()
	c.CreateTime, c.UpdateTime = now, now
	c.State = alloydbpb.Cluster_CREATING
	c.Reconciling = true
	if c.ClusterType == alloydbpb.Cluster_CLUSTER_TYPE_UNSPECIFIED {
		c.ClusterType = alloydbpb.Cluster_PRIMARY
	}
	if c.SubscriptionType == alloydbpb.SubscriptionType_SUBSCRIPTION_TYPE_UNSPECIFIED {
		c.SubscriptionType = alloydbpb.SubscriptionType_STANDARD
	}
	c.InitialUser = nil // INPUT_ONLY
	cb := c.GetContinuousBackupConfig()
	if cb == nil {
		cb = &alloydbpb.ContinuousBackupConfig{}
		c.ContinuousBackupConfig = cb
	}
	if cb.Enabled == nil {
		cb.Enabled = proto.Bool(true)
	}
	if cb.RecoveryWindowDays == 0 {
		cb.RecoveryWindowDays = defaultRecoveryWindowDays
	}
	s.stampContinuous(c)
	c.EncryptionInfo = encryptionInfo(c.GetEncryptionConfig())
	if p := c.GetAutomatedBackupPolicy(); p != nil {
		if p.Location == "" {
			p.Location = location
		}
	}
	if c.GetPscConfig().GetPscEnabled() {
		c.PscConfig.ServiceOwnedProjectNumber = projectNumber(c.Uid)
	}
	c.Etag = etag()
}

// stampContinuous sets continuous_backup_info from the configuration; the
// caller holds mu.
func (s *Server) stampContinuous(c *alloydbpb.Cluster) {
	if !c.GetContinuousBackupConfig().GetEnabled() {
		c.ContinuousBackupInfo = nil
		return
	}
	info := c.GetContinuousBackupInfo()
	if info == nil || info.EnabledTime == nil {
		info = &alloydbpb.ContinuousBackupInfo{EnabledTime: timestamppb.New(s.now()), Schedule: allDays}
	}
	info.EncryptionInfo = encryptionInfo(c.GetContinuousBackupConfig().GetEncryptionConfig())
	c.ContinuousBackupInfo = info
}

func encryptionInfo(cfg *alloydbpb.EncryptionConfig) *alloydbpb.EncryptionInfo {
	if k := cfg.GetKmsKeyName(); k != "" {
		return &alloydbpb.EncryptionInfo{EncryptionType: alloydbpb.EncryptionInfo_CUSTOMER_MANAGED_ENCRYPTION,
			KmsKeyVersions: []string{k + "/cryptoKeyVersions/1"}}
	}
	return &alloydbpb.EncryptionInfo{EncryptionType: alloydbpb.EncryptionInfo_GOOGLE_DEFAULT_ENCRYPTION}
}

// projectNumber is a stable 12-digit number derived from uid, for the
// service-owned project Private Service Connect names.
func projectNumber(uid string) int64 {
	var n int64
	for _, c := range uid {
		if c >= '0' && c <= '9' {
			n = (n*10 + int64(c-'0')) % 1_000_000_000_000
		} else {
			n = (n*7 + int64(c)) % 1_000_000_000_000
		}
	}
	return 100_000_000_000 + n%900_000_000_000
}

// addBuiltinUser records a cluster's initial user; the caller holds mu.
func (s *Server) addBuiltinUser(cluster, user string) {
	if user == "" {
		user = builtinUser
	}
	s.users[cluster+"/users/"+user] = &alloydbpb.User{Name: cluster + "/users/" + user,
		UserType: alloydbpb.User_ALLOYDB_BUILT_IN, DatabaseRoles: []string{SuperuserRole}}
}

func (s *Server) GetCluster(_ context.Context, req *alloydbpb.GetClusterRequest) (*alloydbpb.Cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.cluster(req.GetName())
	if err != nil {
		return nil, err
	}
	return s.clusterView(c), nil
}

// cluster is the named cluster; the caller holds mu.
func (s *Server) cluster(name string) (*alloydbpb.Cluster, error) {
	m := clusterRE.FindStringSubmatch(name)
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/clusters/{cluster}", name)
	}
	c, ok := s.clusters[name]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return c, nil
}

// clusterView is what a read of c answers; the caller holds mu.
func (s *Server) clusterView(c *alloydbpb.Cluster) *alloydbpb.Cluster {
	out := clone(c)
	if info := out.GetContinuousBackupInfo(); info != nil {
		if t, ok := s.earliestRestorable(c.GetName()); ok {
			info.EarliestRestorableTime = timestamppb.New(t)
		}
	}
	return out
}

func (s *Server) ListClusters(_ context.Context, req *alloydbpb.ListClustersRequest) (*alloydbpb.ListClustersResponse, error) {
	if _, _, err := checkParent("parent", req.GetParent(), true); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var all []*alloydbpb.Cluster
	for _, k := range sortedKeys(s.clusters, "") {
		if inLocation(k, req.GetParent()) {
			all = append(all, s.clusterView(s.clusters[k]))
		}
	}
	s.mu.Unlock()
	out, next, err := listPage("alloydb-clusters:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &alloydbpb.ListClustersResponse{Clusters: out, NextPageToken: next}, nil
}

// clusterPaths are the fields UpdateCluster changes, by mask path.
var clusterPaths = map[string]func(dst, src *alloydbpb.Cluster){
	"display_name":             func(d, s *alloydbpb.Cluster) { d.DisplayName = s.DisplayName },
	"labels":                   func(d, s *alloydbpb.Cluster) { d.Labels = s.Labels },
	"annotations":              func(d, s *alloydbpb.Cluster) { d.Annotations = s.Annotations },
	"automated_backup_policy":  func(d, s *alloydbpb.Cluster) { d.AutomatedBackupPolicy = s.AutomatedBackupPolicy },
	"continuous_backup_config": func(d, s *alloydbpb.Cluster) { d.ContinuousBackupConfig = s.ContinuousBackupConfig },
	"maintenance_update_policy": func(d, s *alloydbpb.Cluster) {
		d.MaintenanceUpdatePolicy = s.MaintenanceUpdatePolicy
	},
	"subscription_type": func(d, s *alloydbpb.Cluster) { d.SubscriptionType = s.SubscriptionType },
	"tags":              func(d, s *alloydbpb.Cluster) { d.Tags = s.Tags },
	"network_config.allocated_ip_range": func(d, s *alloydbpb.Cluster) {
		if d.NetworkConfig == nil {
			d.NetworkConfig = &alloydbpb.Cluster_NetworkConfig{}
		}
		d.NetworkConfig.AllocatedIpRange = s.GetNetworkConfig().GetAllocatedIpRange()
	},
}

// maskPaths are the paths of an update mask, snake_case; an empty mask is
// every path in all.
func maskPaths(paths []string, all []string) []string {
	if len(paths) == 0 {
		return all
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, toSnake(p))
	}
	return out
}

func toSnake(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			b.WriteRune(r - 'A' + 'a')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func keys[V any](m map[string]V) []string { return sortedKeys(m, "") }

func (s *Server) UpdateCluster(_ context.Context, req *alloydbpb.UpdateClusterRequest) (*longrunningpb.Operation, error) {
	in := req.GetCluster()
	m := clusterRE.FindStringSubmatch(in.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("cluster.name %q must be projects/{project}/locations/{location}/clusters/{cluster}", in.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	c, ok := s.clusters[in.GetName()]
	if !ok {
		if req.GetAllowMissing() {
			s.mu.Unlock()
			op, err := s.CreateCluster(context.Background(), &alloydbpb.CreateClusterRequest{Parent: "projects/" + m[1] + "/locations/" + m[2],
				ClusterId: m[3], Cluster: in, RequestId: req.GetRequestId(), ValidateOnly: req.GetValidateOnly()})
			s.mu.Lock()
			return op, err
		}
		return nil, apierror.NotFound("Resource '%s' was not found", in.GetName())
	}
	if in.GetEtag() != "" && in.GetEtag() != c.GetEtag() {
		return nil, apierror.Aborted("etag %q does not match the cluster's", in.GetEtag())
	}
	next := clone(c)
	var password string
	for _, p := range maskPaths(req.GetUpdateMask().GetPaths(), keys(clusterPaths)) {
		switch {
		case p == "initial_user" || strings.HasPrefix(p, "initial_user."):
			password = in.GetInitialUser().GetPassword()
			continue
		case strings.HasPrefix(p, "automated_backup_policy."):
			p = "automated_backup_policy"
		case strings.HasPrefix(p, "continuous_backup_config."):
			p = "continuous_backup_config"
		}
		f, ok := clusterPaths[p]
		if !ok {
			return nil, apierror.InvalidArgument("update_mask path %q is not a field UpdateCluster can change", p)
		}
		f(next, in)
	}
	if cb := next.GetContinuousBackupConfig(); cb != nil && cb.RecoveryWindowDays != 0 &&
		(cb.RecoveryWindowDays < 1 || cb.RecoveryWindowDays > 35) {
		return nil, apierror.InvalidArgument("continuous_backup_config.recovery_window_days must be between 1 and 35")
	}
	if req.GetValidateOnly() {
		return s.validateOnly(locationOf(c.GetName()), "update", c.GetName()), nil
	}
	if cb := next.GetContinuousBackupConfig(); cb != nil {
		if cb.Enabled == nil {
			cb.Enabled = proto.Bool(true)
		}
		if cb.RecoveryWindowDays == 0 {
			cb.RecoveryWindowDays = defaultRecoveryWindowDays
		}
	}
	wasPITR := c.GetContinuousBackupConfig().GetEnabled()
	s.stampContinuous(next)
	if p := next.GetAutomatedBackupPolicy(); p != nil && p.Location == "" {
		p.Location = m[2]
	}
	next.UpdateTime = timestamppb.New(s.now())
	next.Etag = etag()
	s.clusters[c.GetName()] = next
	if password != "" {
		s.passwords[c.GetName()] = password
	}
	op, err := s.startOp(locationOf(c.GetName()), "update", c.GetName(), req.GetRequestId())
	if err != nil {
		return nil, err
	}
	// Turning continuous backup on or off changes the primary's WAL
	// archiving, which a restart applies.
	primary := s.primaryOf(c.GetName())
	if primary != nil && wasPITR != next.GetContinuousBackupConfig().GetEnabled() && s.opts.Engine != nil {
		spec, _ := s.specOf(primary)
		pitr := spec.PITR
		s.background(op.Name, func(ctx context.Context) error {
			if err := s.opts.Engine.Start(ctx, spec); err != nil {
				return err
			}
			if pitr {
				return s.takeBase(ctx, c.GetName(), spec.Server, true)
			}
			return nil
		}, func(err error) (proto.Message, error) { return s.clusterView(s.clusters[c.GetName()]), nil })
		return op, nil
	}
	s.finishOp(op.Name, s.clusterView(next), nil)
	return clone(s.ops[op.Name]), nil
}

func locationOf(name string) string {
	parts := strings.SplitN(name, "/", 5)
	if len(parts) < 4 {
		return name
	}
	return strings.Join(parts[:4], "/")
}

// primaryOf is cluster's primary instance, or nil; the caller holds mu.
func (s *Server) primaryOf(cluster string) *alloydbpb.Instance {
	for _, k := range sortedKeys(s.instances, cluster+"/instances/") {
		if in := s.instances[k]; in.GetInstanceType() == alloydbpb.Instance_PRIMARY {
			return in
		}
	}
	return nil
}

func (s *Server) DeleteCluster(_ context.Context, req *alloydbpb.DeleteClusterRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	c, err := s.cluster(req.GetName())
	if err != nil {
		return nil, err
	}
	if req.GetEtag() != "" && req.GetEtag() != c.GetEtag() {
		return nil, apierror.Aborted("etag %q does not match the cluster's", req.GetEtag())
	}
	insts := sortedKeys(s.instances, c.GetName()+"/instances/")
	if len(insts) > 0 && !req.GetForce() {
		// UNVERIFIED: Google's message.
		return nil, apierror.FailedPrecondition("Cluster %s has instances; delete them first, or set force to delete them with the cluster", c.GetName())
	}
	if req.GetValidateOnly() {
		return s.validateOnly(locationOf(c.GetName()), "delete", c.GetName()), nil
	}
	c.State = alloydbpb.Cluster_DELETING
	for _, k := range insts {
		s.instances[k].State = alloydbpb.Instance_DELETING
	}
	op, err := s.startOp(locationOf(c.GetName()), "delete", c.GetName(), req.GetRequestId())
	if err != nil {
		return nil, err
	}
	var servers []string
	for _, k := range insts {
		servers = append(servers, serverName(k))
	}
	name := c.GetName()
	s.background(op.Name, func(ctx context.Context) error {
		if s.opts.Engine == nil {
			return nil
		}
		for _, srv := range servers {
			if err := s.opts.Engine.Remove(ctx, srv); err != nil {
				return err
			}
		}
		return nil
	}, func(err error) (proto.Message, error) {
		if err != nil {
			if c, ok := s.clusters[name]; ok {
				c.State = alloydbpb.Cluster_READY
			}
			return nil, err
		}
		for _, k := range insts {
			delete(s.instances, k)
		}
		for _, k := range sortedKeys(s.users, name+"/users/") {
			delete(s.users, k)
		}
		delete(s.clusters, name)
		delete(s.passwords, name)
		delete(s.lastAuto, name)
		if d := s.restores[name]; d != "" && strings.HasPrefix(d, s.restoreRoot()) {
			_ = os.RemoveAll(d)
		}
		delete(s.restores, name)
		return nil, nil
	})
	return op, nil
}

// restoreRoot is where point-in-time recoveries' dumps are kept.
func (s *Server) restoreRoot() string {
	if s.opts.BackupsDir == "" {
		return ""
	}
	return filepath.Join(s.opts.BackupsDir, "restores")
}

func (s *Server) RestoreCluster(_ context.Context, req *alloydbpb.RestoreClusterRequest) (*longrunningpb.Operation, error) {
	_, location, err := checkParent("parent", req.GetParent(), false)
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
	name := req.GetParent() + "/clusters/" + req.GetClusterId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	if _, ok := s.clusters[name]; ok {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	var (
		srcServer string
		at        time.Time
		dir       string
	)
	switch {
	case req.GetBackupSource() != nil:
		b, ok := s.backups[req.GetBackupSource().GetBackupName()]
		if !ok {
			return nil, apierror.NotFound("Resource '%s' was not found", req.GetBackupSource().GetBackupName())
		}
		if b.GetState() != alloydbpb.Backup_READY {
			return nil, apierror.FailedPrecondition("backup %s is not ready", b.GetName())
		}
		c.DatabaseVersion = b.GetDatabaseVersion()
		c.Source = &alloydbpb.Cluster_BackupSource{BackupSource: &alloydbpb.BackupSource{BackupName: b.GetName(), BackupUid: b.GetUid()}}
		dir = s.backupDir(b)
	case req.GetContinuousBackupSource() != nil:
		src, err := s.cluster(req.GetContinuousBackupSource().GetCluster())
		if err != nil {
			return nil, err
		}
		if !src.GetContinuousBackupConfig().GetEnabled() {
			return nil, apierror.FailedPrecondition("continuous backup is not enabled on cluster %s", src.GetName())
		}
		at = req.GetContinuousBackupSource().GetPointInTime().AsTime()
		earliest, ok := s.earliestRestorable(src.GetName())
		if !ok || at.Before(earliest) || at.After(s.now()) {
			return nil, apierror.InvalidArgument("point_in_time %s is outside cluster %s's recovery window", at.UTC().Format(time.RFC3339Nano), src.GetName())
		}
		p := s.primaryOf(src.GetName())
		if p == nil {
			return nil, apierror.FailedPrecondition("cluster %s has no primary instance to recover", src.GetName())
		}
		srcServer = serverName(p.GetName())
		c.DatabaseVersion = src.GetDatabaseVersion()
		if s.restoreRoot() == "" {
			return nil, apierror.FailedPrecondition("point-in-time recovery is not available: this instance keeps no backups")
		}
		dir = filepath.Join(s.restoreRoot(), randomHex(8))
	default:
		return nil, apierror.InvalidArgument("one of backup_source or continuous_backup_source is required")
	}
	if c.NetworkConfig == nil && c.Network == "" && !c.GetPscConfig().GetPscEnabled() {
		// The source's network, when the request names none. UNVERIFIED.
		if src, ok := s.clusters[req.GetContinuousBackupSource().GetCluster()]; ok {
			c.NetworkConfig = clone(src.GetNetworkConfig())
		}
	}
	if err := s.prepareCluster(c); err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return s.validateOnly(req.GetParent(), "restore", name), nil
	}
	s.stampCluster(name, c, location)
	s.clusters[name] = c
	s.addBuiltinUser(name, "")
	op, err := s.startOp(req.GetParent(), "restore", name, req.GetRequestId())
	if err != nil {
		return nil, err
	}
	s.background(op.Name, func(ctx context.Context) error {
		if srcServer == "" {
			return nil
		}
		if s.opts.Engine == nil {
			return apierror.FailedPrecondition("no engine runs AlloyDB instances here")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return s.opts.Engine.RecoverTo(ctx, srcServer, at, dir)
	}, func(err error) (proto.Message, error) {
		c, ok := s.clusters[name]
		if !ok {
			return nil, err
		}
		if err != nil {
			c.State = alloydbpb.Cluster_FAILED
			c.Reconciling = false
			return nil, err
		}
		s.restores[name] = dir
		c.State = alloydbpb.Cluster_READY
		c.Reconciling = false
		return s.clusterView(c), nil
	})
	return op, nil
}

func (s *Server) UpgradeCluster(_ context.Context, req *alloydbpb.UpgradeClusterRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	c, err := s.cluster(req.GetName())
	if err != nil {
		return nil, err
	}
	if req.GetEtag() != "" && req.GetEtag() != c.GetEtag() {
		return nil, apierror.Aborted("etag %q does not match the cluster's", req.GetEtag())
	}
	if !supportedVersions[req.GetVersion()] {
		return nil, apierror.InvalidArgument("version %s is not a supported database version", req.GetVersion())
	}
	if req.GetVersion() <= c.GetDatabaseVersion() {
		return nil, apierror.InvalidArgument("version %s is not newer than the cluster's %s", req.GetVersion(), c.GetDatabaseVersion())
	}
	if req.GetValidateOnly() {
		return s.validateOnly(locationOf(c.GetName()), "upgrade", c.GetName()), nil
	}
	op, err := s.startOp(locationOf(c.GetName()), "upgrade", c.GetName(), req.GetRequestId())
	if err != nil {
		return nil, err
	}
	name, from := c.GetName(), c.GetDatabaseVersion()
	primary := s.primaryOf(name)
	var pools []*alloydbpb.Instance
	for _, k := range sortedKeys(s.instances, name+"/instances/") {
		if s.instances[k].GetInstanceType() == alloydbpb.Instance_READ_POOL {
			pools = append(pools, s.instances[k])
		}
	}
	c.DatabaseVersion = req.GetVersion()
	c.Reconciling = true
	var primarySpec ServerSpec
	var poolSpecs []ServerSpec
	if primary != nil {
		primarySpec, _ = s.specOf(primary)
		primary.State = alloydbpb.Instance_MAINTENANCE
	}
	for _, p := range pools {
		sp, _ := s.specOf(p)
		poolSpecs = append(poolSpecs, sp)
		p.State = alloydbpb.Instance_MAINTENANCE
	}
	dir := ""
	if s.opts.BackupsDir != "" {
		dir = filepath.Join(s.opts.BackupsDir, "upgrades", randomHex(8))
	}
	s.background(op.Name, func(ctx context.Context) error {
		if primary == nil || s.opts.Engine == nil {
			return nil
		}
		if dir == "" {
			return apierror.FailedPrecondition("this instance keeps no backups, which an upgrade's copy needs")
		}
		defer os.RemoveAll(dir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		// pg_upgrade's outcome, by a dump of the old server loaded into a
		// new one of the new version: the read pools are rebuilt from it.
		e := s.opts.Engine
		if err := e.Dump(ctx, primarySpec.Server, dir); err != nil {
			return err
		}
		for _, sp := range poolSpecs {
			if err := e.Remove(ctx, sp.Server); err != nil {
				return err
			}
		}
		if err := e.Remove(ctx, primarySpec.Server); err != nil {
			return err
		}
		if err := e.Start(ctx, primarySpec); err != nil {
			return err
		}
		if err := e.Load(ctx, primarySpec.Server, dir); err != nil {
			return err
		}
		if err := s.setupPrimary(ctx, name, primarySpec.Server); err != nil {
			return err
		}
		if primarySpec.PITR {
			if err := s.takeBase(ctx, name, primarySpec.Server, true); err != nil {
				return err
			}
		}
		for _, sp := range poolSpecs {
			if err := e.Start(ctx, sp); err != nil {
				return err
			}
		}
		return nil
	}, func(err error) (proto.Message, error) {
		c, ok := s.clusters[name]
		if !ok {
			return nil, err
		}
		c.Reconciling = false
		if err != nil {
			c.DatabaseVersion = from
		}
		for _, k := range sortedKeys(s.instances, name+"/instances/") {
			if err != nil {
				s.instances[k].State = alloydbpb.Instance_FAILED
			} else {
				s.instances[k].State = alloydbpb.Instance_READY
			}
		}
		c.UpdateTime = timestamppb.New(s.now())
		c.Etag = etag()
		if err != nil {
			return nil, err
		}
		return &alloydbpb.UpgradeClusterResponse{Status: alloydbpb.UpgradeClusterResponse_SUCCESS,
			Message: "Cluster upgraded from " + from.String() + " to " + c.GetDatabaseVersion().String()}, nil
	})
	return op, nil
}

func (s *Server) ExportCluster(context.Context, *alloydbpb.ExportClusterRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented("ExportCluster is not implemented yet in CloudBurrow (#2191): exports to Cloud Storage are not served")
}

func (s *Server) ImportCluster(context.Context, *alloydbpb.ImportClusterRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented("ImportCluster is not implemented yet in CloudBurrow (#2191): imports from Cloud Storage are not served")
}

func (s *Server) CreateSecondaryCluster(context.Context, *alloydbpb.CreateSecondaryClusterRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented("CreateSecondaryCluster is not implemented yet in CloudBurrow (#2191): cross-region replication is not served")
}

func (s *Server) PromoteCluster(context.Context, *alloydbpb.PromoteClusterRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented("PromoteCluster is not implemented yet in CloudBurrow (#2191): there are no secondary clusters")
}

func (s *Server) SwitchoverCluster(context.Context, *alloydbpb.SwitchoverClusterRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented("SwitchoverCluster is not implemented yet in CloudBurrow (#2191): there are no secondary clusters")
}

func (s *Server) CreateSecondaryInstance(context.Context, *alloydbpb.CreateSecondaryInstanceRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented("CreateSecondaryInstance is not implemented yet in CloudBurrow (#2191): there are no secondary clusters")
}
