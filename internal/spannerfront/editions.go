package spannerfront

// Editions, default backup schedules and the managed autoscaler's
// configuration (#1949), as Google documents them:
//
//   - https://docs.cloud.google.com/spanner/docs/editions-overview: an
//     instance is STANDARD, ENTERPRISE or ENTERPRISE_PLUS. Enterprise adds
//     custom read-only replicas, Spanner Graph, full-text and vector search,
//     the managed autoscaler and incremental backups; Enterprise Plus adds
//     dual- and multi-region configurations and geo-partitioning. An
//     instance can be upgraded at any time; "You must stop using the
//     higher-tier edition features in order to downgrade", so a downgrade
//     is FAILED_PRECONDITION, naming the feature, while the instance uses
//     one. A feature the instance's edition does not include is refused
//     FAILED_PRECONDITION where it is asked for.
//   - default_backup_schedule_type: "Default backup schedules are
//     automatically enabled for all new instances" (AUTOMATIC, unless the
//     instance is a free instance, which has no backups); each database
//     created afterwards gets a schedule of full backups every 24 hours,
//     kept 7 days, which "takes 24 hours to become active". Databases that
//     existed before AUTOMATIC was set get none.
//   - https://docs.cloud.google.com/spanner/docs/managed-autoscaler: limits
//     of at least 1 node (1000 processing units), the maximum at least the
//     minimum and at most 10 times it, in one unit; a high-priority and a
//     total CPU target from 10% to 90% (at least one, the high-priority
//     one below the total one), and a storage target from 10% to 99%.
//     There is no load locally, so an autoscaled instance reports its
//     compute capacity at the configured minimum, as Google's autoscaler
//     leaves an idle instance.
//
// The emulator has no editions: the front keeps the edition, the default
// backup schedule type and the autoscaling configuration in the instance's
// changes (instances.go), which state.go persists, and lays them over the
// emulator's instance. An instance created before #1949 has none of them
// recorded and reports EDITION_UNSPECIFIED, as Google's legacy instances
// do; it is gated as a Standard edition instance.

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Paths the front keeps for an instance beside its UpdateInstance changes.
const (
	pathEdition     = "edition"
	pathAutoscaling = "autoscaling_config"
	pathDefaultBkup = "default_backup_schedule_type"
)

// defaultScheduleID is the ID of the schedule AUTOMATIC gives a new
// database. Google does not document the ID it uses.
const defaultScheduleID = "default-backup-schedule"

// editionRank orders the editions; EDITION_UNSPECIFIED (a legacy
// instance) ranks as STANDARD.
func editionRank(e instancepb.Instance_Edition) int {
	switch e {
	case instancepb.Instance_ENTERPRISE:
		return 2
	case instancepb.Instance_ENTERPRISE_PLUS:
		return 3
	}
	return 1
}

func editionName(e instancepb.Instance_Edition) string {
	if e == instancepb.Instance_EDITION_UNSPECIFIED {
		e = instancepb.Instance_STANDARD
	}
	return e.String()
}

// featureNeeds is the error for feature, which edition min includes and
// the instance's edition has does not.
func featureNeeds(method, feature string, min, has instancepb.Instance_Edition) error {
	return status.Errorf(codes.FailedPrecondition, "%s: %s requires the %s edition or higher; the instance is %s edition: upgrade it with UpdateInstance (edition) first.",
		method, feature, min, editionName(has))
}

// editionLocked is the edition recorded for instance. s.mu is held.
func (s *Instances) editionLocked(instance string) instancepb.Instance_Edition {
	if c, ok := s.changes[instance]; ok && c.paths[pathEdition] {
		return c.inst.GetEdition()
	}
	return instancepb.Instance_EDITION_UNSPECIFIED
}

func (s *Instances) edition(instance string) instancepb.Instance_Edition {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.editionLocked(instance)
}

// configEdition is the lowest edition an instance in the configuration
// name may have: Enterprise Plus for a multi-region one (or a custom one
// based on it), Enterprise for a custom one with read-only replicas, and
// Standard for any other.
func (s *Instances) configEdition(name string) instancepb.Instance_Edition {
	id := name[strings.LastIndex(name, "/")+1:]
	if _, ok := multiRegions[id]; ok {
		return instancepb.Instance_ENTERPRISE_PLUS
	}
	s.mu.Lock()
	c := s.configs[name]
	s.mu.Unlock()
	if c == nil {
		return instancepb.Instance_STANDARD
	}
	if base := c.GetBaseConfig(); base != "" && base != name && s.configEdition(base) == instancepb.Instance_ENTERPRISE_PLUS {
		return instancepb.Instance_ENTERPRISE_PLUS
	}
	for _, r := range c.GetReplicas() {
		if r.GetType() == instancepb.ReplicaInfo_READ_ONLY {
			return instancepb.Instance_ENTERPRISE
		}
	}
	return instancepb.Instance_STANDARD
}

// configFeature names what makes configuration name need its edition.
func configFeature(e instancepb.Instance_Edition) string {
	if e == instancepb.Instance_ENTERPRISE_PLUS {
		return "a dual-region or multi-region configuration"
	}
	return "a configuration with custom read-only replicas"
}

// checkAutoscaling validates an autoscaling configuration as the managed
// autoscaler documents it.
func checkAutoscaling(method string, ac *instancepb.AutoscalingConfig) error {
	bad := func(format string, a ...any) error {
		return status.Errorf(codes.InvalidArgument, "Invalid %s request: autoscaling_config.%s", method, fmt.Sprintf(format, a...))
	}
	l := ac.GetAutoscalingLimits()
	if l == nil || l.GetMinLimit() == nil || l.GetMaxLimit() == nil {
		return bad("autoscaling_limits needs a minimum and a maximum, in nodes or in processing units.")
	}
	_, minNodes := l.GetMinLimit().(*instancepb.AutoscalingConfig_AutoscalingLimits_MinNodes)
	_, maxNodes := l.GetMaxLimit().(*instancepb.AutoscalingConfig_AutoscalingLimits_MaxNodes)
	if minNodes != maxNodes {
		return bad("autoscaling_limits: the minimum and the maximum must both be nodes or both be processing units.")
	}
	lo, hi := l.GetMinProcessingUnits(), l.GetMaxProcessingUnits()
	if minNodes {
		lo, hi = l.GetMinNodes()*1000, l.GetMaxNodes()*1000
	}
	if lo < 1000 {
		return bad("autoscaling_limits: the minimum must be 1 node (1000 processing units) or greater.")
	}
	if !minNodes && (lo%1000 != 0 || hi%1000 != 0) {
		return bad("autoscaling_limits: processing units must be a multiple of 1000.")
	}
	if hi < lo {
		return bad("autoscaling_limits: the maximum must be greater than or equal to the minimum.")
	}
	if hi > 10*lo {
		return bad("autoscaling_limits: the maximum can't be more than 10 times the minimum.")
	}
	t := ac.GetAutoscalingTargets()
	hp, total, st := t.GetHighPriorityCpuUtilizationPercent(), t.GetTotalCpuUtilizationPercent(), t.GetStorageUtilizationPercent()
	if hp == 0 && total == 0 {
		return bad("autoscaling_targets needs a high-priority or a total CPU utilization target.")
	}
	if hp != 0 && (hp < 10 || hp > 90) {
		return bad("autoscaling_targets.high_priority_cpu_utilization_percent must be from 10 to 90.")
	}
	if total != 0 && (total < 10 || total > 90) {
		return bad("autoscaling_targets.total_cpu_utilization_percent must be from 10 to 90.")
	}
	if hp != 0 && total != 0 && hp >= total {
		return bad("autoscaling_targets: the high-priority CPU utilization target has to be smaller than the total CPU utilization target.")
	}
	if st < 10 || st > 99 {
		return bad("autoscaling_targets.storage_utilization_percent must be from 10 to 99.")
	}
	for i, o := range ac.GetAsymmetricAutoscalingOptions() {
		if o.GetReplicaSelection().GetLocation() == "" {
			return bad("asymmetric_autoscaling_options[%d].replica_selection.location is required.", i)
		}
		ov := o.GetOverrides()
		if ol := ov.GetAutoscalingLimits(); ol != nil {
			if err := checkAutoscaling(method, &instancepb.AutoscalingConfig{AutoscalingLimits: ol,
				AutoscalingTargets: &instancepb.AutoscalingConfig_AutoscalingTargets{HighPriorityCpuUtilizationPercent: 50, StorageUtilizationPercent: 50}}); err != nil {
				return err
			}
		}
		if p := ov.GetAutoscalingTargetHighPriorityCpuUtilizationPercent(); p != 0 && (p < 10 || p > 90) {
			return bad("asymmetric_autoscaling_options[%d]: the high-priority CPU target must be from 10 to 90.", i)
		}
	}
	return nil
}

// checkAsymmetric checks that each asymmetric option names a read-only
// replica of the configuration config: "You can't use asymmetric
// autoscaling with read-write and witness replicas."
func (s *Instances) checkAsymmetric(ctx context.Context, method, config string, ac *instancepb.AutoscalingConfig) error {
	if len(ac.GetAsymmetricAutoscalingOptions()) == 0 {
		return nil
	}
	c, err := s.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: config})
	if err != nil {
		return err
	}
	ro := map[string]bool{}
	for _, r := range c.GetReplicas() {
		if r.GetType() == instancepb.ReplicaInfo_READ_ONLY {
			ro[r.GetLocation()] = true
		}
	}
	for _, o := range ac.GetAsymmetricAutoscalingOptions() {
		if loc := o.GetReplicaSelection().GetLocation(); !ro[loc] {
			return status.Errorf(codes.InvalidArgument, "Invalid %s request: asymmetric autoscaling option %s is not a read-only replica of %s: asymmetric autoscaling can't be used with read-write and witness replicas.", method, loc, config)
		}
	}
	return nil
}

// autoscaledCapacity sets dst's compute capacity to ac's minimum: the
// capacity an idle autoscaled instance has.
func autoscaledCapacity(dst *instancepb.Instance, ac *instancepb.AutoscalingConfig) {
	l := ac.GetAutoscalingLimits()
	if n := l.GetMinNodes(); n > 0 {
		dst.NodeCount, dst.ProcessingUnits = n, n*1000
		return
	}
	pu := l.GetMinProcessingUnits()
	dst.NodeCount, dst.ProcessingUnits = pu/1000, pu
}

// mergeAutoscaling is cur with the fields of in that paths
// ("autoscaling_config.autoscaling_limits.max_nodes") name.
func mergeAutoscaling(cur, in *instancepb.AutoscalingConfig, paths []string) (*instancepb.AutoscalingConfig, error) {
	out := &instancepb.AutoscalingConfig{}
	if cur != nil {
		out = proto.Clone(cur).(*instancepb.AutoscalingConfig)
	}
	if in == nil {
		in = &instancepb.AutoscalingConfig{}
	}
	for _, p := range paths {
		parts := strings.Split(strings.TrimPrefix(p, pathAutoscaling+"."), ".")
		dst, src := out.ProtoReflect(), in.ProtoReflect()
		for i, part := range parts {
			fd := dst.Descriptor().Fields().ByName(protoreflect.Name(part))
			if fd == nil {
				return nil, status.Errorf(codes.InvalidArgument, "Invalid UpdateInstance request: field_mask path %q is not a field of Instance that can be updated.", p)
			}
			if i == len(parts)-1 {
				if src.Has(fd) {
					dst.Set(fd, src.Get(fd))
				} else {
					dst.Clear(fd)
				}
				break
			}
			if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
				return nil, status.Errorf(codes.InvalidArgument, "Invalid UpdateInstance request: field_mask path %q is not a field of Instance that can be updated.", p)
			}
			dst, src = dst.Mutable(fd).Message(), src.Get(fd).Message()
		}
	}
	return out, nil
}

// applyEditions lays the edition fields of src named in paths over dst
// (apply, instances.go).
func applyEditions(dst, src *instancepb.Instance, paths map[string]bool) {
	if paths[pathEdition] {
		dst.Edition = src.Edition
	}
	if paths[pathDefaultBkup] {
		dst.DefaultBackupScheduleType = src.DefaultBackupScheduleType
	}
	if paths[pathAutoscaling] {
		dst.AutoscalingConfig = src.AutoscalingConfig
		if src.AutoscalingConfig != nil {
			autoscaledCapacity(dst, src.AutoscalingConfig)
		}
	}
}

// prepareCreate checks the edition, default backup schedule type and
// autoscaling configuration of a CreateInstance, fills Google's defaults,
// and returns the request the emulator is given (without them, and with
// an autoscaled instance's capacity at its minimum) and the change the
// front records once the emulator has created the instance.
func (s *Instances) prepareCreate(ctx context.Context, req *instancepb.CreateInstanceRequest) (*instancepb.CreateInstanceRequest, *change, error) {
	const method = "CreateInstance"
	in := req.GetInstance()
	rec := &instancepb.Instance{Edition: in.GetEdition(), DefaultBackupScheduleType: in.GetDefaultBackupScheduleType(),
		AutoscalingConfig: in.GetAutoscalingConfig()}
	need := s.configEdition(in.GetConfig())
	if rec.Edition == instancepb.Instance_EDITION_UNSPECIFIED {
		// The lowest edition the configuration allows.
		rec.Edition = need
	}
	if editionRank(rec.Edition) < editionRank(need) {
		return nil, nil, featureNeeds(method, configFeature(need), need, rec.Edition)
	}
	free := in.GetInstanceType() == instancepb.Instance_FREE_INSTANCE
	switch rec.DefaultBackupScheduleType {
	case instancepb.Instance_DEFAULT_BACKUP_SCHEDULE_TYPE_UNSPECIFIED:
		rec.DefaultBackupScheduleType = instancepb.Instance_AUTOMATIC
		if free {
			rec.DefaultBackupScheduleType = instancepb.Instance_NONE
		}
	case instancepb.Instance_AUTOMATIC:
		if free {
			return nil, nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: default_backup_schedule_type AUTOMATIC isn't permitted for free instances, as backups and backup schedules aren't supported for free instances.", method)
		}
	}
	fwd := proto.Clone(req).(*instancepb.CreateInstanceRequest)
	fi := fwd.GetInstance()
	if fi == nil {
		fi = &instancepb.Instance{}
		fwd.Instance = fi
	}
	fi.Edition, fi.DefaultBackupScheduleType, fi.AutoscalingConfig = instancepb.Instance_EDITION_UNSPECIFIED, instancepb.Instance_DEFAULT_BACKUP_SCHEDULE_TYPE_UNSPECIFIED, nil
	paths := map[string]bool{pathEdition: true, pathDefaultBkup: true}
	if ac := rec.AutoscalingConfig; ac != nil {
		if editionRank(rec.Edition) < editionRank(instancepb.Instance_ENTERPRISE) {
			return nil, nil, featureNeeds(method, "the managed autoscaler", instancepb.Instance_ENTERPRISE, rec.Edition)
		}
		if in.GetNodeCount() != 0 || in.GetProcessingUnits() != 0 {
			return nil, nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: node_count and processing_units can't be set with autoscaling_config: the autoscaler sets them.", method)
		}
		if err := checkAutoscaling(method, ac); err != nil {
			return nil, nil, err
		}
		if err := s.checkAsymmetric(ctx, method, in.GetConfig(), ac); err != nil {
			return nil, nil, err
		}
		autoscaledCapacity(fi, ac)
		// The emulator takes one of the two.
		fi.NodeCount = 0
		paths[pathAutoscaling] = true
	}
	return fwd, &change{inst: rec, paths: paths}, nil
}

// created records c for the instance name, and lays it over a done
// operation's instance.
func (s *Instances) created(name string, c *change, op *longrunningpb.Operation) {
	s.mu.Lock()
	c.at = s.now()
	s.changes[name] = c
	s.mu.Unlock()
	if r := op.GetResponse(); r != nil {
		inst := &instancepb.Instance{}
		if r.UnmarshalTo(inst) == nil {
			applyEditions(inst, c.inst, c.paths)
			if a, err := anypb.New(inst); err == nil {
				op.Result = &longrunningpb.Operation_Response{Response: a}
			}
		}
	}
}

// checkEditionUpdate checks an UpdateInstance's edition and autoscaling
// paths against cur, the instance as it is (with the front's changes),
// and fills want's capacity from an autoscaling configuration.
func (s *Instances) checkEditionUpdate(ctx context.Context, cur, want *instancepb.Instance, paths map[string]bool) error {
	const method = "UpdateInstance"
	ed := cur.GetEdition()
	if paths[pathEdition] {
		ed = want.GetEdition()
	}
	ac := cur.GetAutoscalingConfig()
	if paths[pathAutoscaling] {
		ac = want.GetAutoscalingConfig()
	}
	if (paths["node_count"] || paths["processing_units"]) && ac != nil {
		return status.Errorf(codes.InvalidArgument, "Invalid %s request: node_count and processing_units can't be set while autoscaling_config is set: clear autoscaling_config to scale manually.", method)
	}
	if paths[pathAutoscaling] && ac != nil {
		if err := checkAutoscaling(method, ac); err != nil {
			return err
		}
		if err := s.checkAsymmetric(ctx, method, cur.GetConfig(), ac); err != nil {
			return err
		}
	}
	if paths[pathAutoscaling] && ac == nil && !paths["node_count"] && !paths["processing_units"] {
		// Back to manual scaling at the capacity the autoscaler left.
		want.NodeCount, want.ProcessingUnits = cur.GetNodeCount(), cur.GetProcessingUnits()
		paths["processing_units"] = true
	}
	if paths[pathDefaultBkup] && want.GetDefaultBackupScheduleType() == instancepb.Instance_AUTOMATIC &&
		cur.GetInstanceType() == instancepb.Instance_FREE_INSTANCE {
		return status.Errorf(codes.InvalidArgument, "Invalid %s request: default_backup_schedule_type AUTOMATIC isn't permitted for free instances.", method)
	}
	if ac != nil && editionRank(ed) < editionRank(instancepb.Instance_ENTERPRISE) {
		if paths[pathEdition] && editionRank(ed) < editionRank(cur.GetEdition()) {
			return downgradeRefused(cur.GetEdition(), ed, "the managed autoscaler: switch the instance to manual scaling")
		}
		return featureNeeds(method, "the managed autoscaler", instancepb.Instance_ENTERPRISE, ed)
	}
	if !paths[pathEdition] || editionRank(ed) >= editionRank(cur.GetEdition()) {
		return nil
	}
	// A downgrade: "You must stop using the higher-tier edition features."
	if need := s.configEdition(cur.GetConfig()); editionRank(ed) < editionRank(need) {
		return downgradeRefused(cur.GetEdition(), ed, configFeature(need)+": move the instance to a regional configuration")
	}
	if editionRank(ed) < editionRank(instancepb.Instance_ENTERPRISE_PLUS) {
		parts, err := s.emu.ListInstancePartitions(ctx, &instancepb.ListInstancePartitionsRequest{Parent: cur.GetName()})
		if err == nil && len(parts.GetInstancePartitions()) > 0 {
			return downgradeRefused(cur.GetEdition(), ed, "geo-partitioning: delete all partitions")
		}
	}
	if editionRank(ed) < editionRank(instancepb.Instance_ENTERPRISE) {
		if why := s.enterpriseInUse(ctx, cur.GetName()); why != "" {
			return downgradeRefused(cur.GetEdition(), ed, why)
		}
	}
	return nil
}

func downgradeRefused(from, to instancepb.Instance_Edition, feature string) error {
	return status.Errorf(codes.FailedPrecondition, "UpdateInstance: the instance can't be downgraded from the %s edition to the %s edition while it uses %s. You must stop using the higher-tier edition features in order to downgrade.",
		editionName(from), editionName(to), feature)
}

// enterpriseDDL are the schema objects only the Enterprise editions have,
// with what to do before a downgrade.
var enterpriseDDL = []struct {
	re           *regexp.Regexp
	feature, fix string
}{
	{regexp.MustCompile(`(?is)^\s*CREATE\s+(OR\s+REPLACE\s+)?PROPERTY\s+GRAPH\b`), "Spanner Graph", "delete all property graph schemas"},
	{regexp.MustCompile(`(?is)^\s*CREATE\s+SEARCH\s+INDEX\b`), "full-text search", "delete all search indexes"},
	{regexp.MustCompile(`(?is)^\s*CREATE\s+VECTOR\s+INDEX\b`), "vector search", "delete all vector indexes"},
}

// enterpriseInUse names the Enterprise feature instance uses, "" for none:
// incremental backup schedules, or a property graph, search index or
// vector index in a database's schema.
func (s *Instances) enterpriseInUse(ctx context.Context, instance string) string {
	s.mu.Lock()
	for name, sc := range s.schedules {
		if strings.HasPrefix(name, instance+"/") && sc.pb.GetIncrementalBackupSpec() != nil {
			s.mu.Unlock()
			return "incremental backups: delete the incremental backup schedule " + name
		}
	}
	s.mu.Unlock()
	token := ""
	for {
		resp, err := s.emuDB.ListDatabases(ctx, &databasepb.ListDatabasesRequest{Parent: instance, PageToken: token})
		if err != nil {
			return ""
		}
		for _, db := range resp.GetDatabases() {
			ddl, err := s.emuDB.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{Database: db.GetName()})
			if err != nil {
				continue
			}
			for _, st := range ddl.GetStatements() {
				for _, e := range enterpriseDDL {
					if e.re.MatchString(st) {
						return e.feature + " in " + db.GetName() + ": " + e.fix
					}
				}
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			return ""
		}
	}
}

// checkEditionDDL refuses, in an instance below the Enterprise edition, a
// statement creating what only the Enterprise editions have.
func (s *Instances) checkEditionDDL(method, instance string, stmts []string) error {
	ed := s.edition(instance)
	if editionRank(ed) >= editionRank(instancepb.Instance_ENTERPRISE) {
		return nil
	}
	for _, st := range stmts {
		for _, e := range enterpriseDDL {
			if e.re.MatchString(st) {
				return featureNeeds(method, e.feature, instancepb.Instance_ENTERPRISE, ed)
			}
		}
	}
	return nil
}

// addDefaultSchedule gives database, new in an instance whose default
// backup schedule type is AUTOMATIC, Google's default schedule: a full
// backup every 24 hours, kept 7 days, its first a day after the database
// was created.
func (s *Instances) addDefaultSchedule(database string) {
	m := databaseNameRe.FindStringSubmatch(database)
	if m == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.changes[m[1]]
	if !ok || !c.paths[pathDefaultBkup] || c.inst.GetDefaultBackupScheduleType() != instancepb.Instance_AUTOMATIC {
		return
	}
	now := s.now()
	text := fmt.Sprintf("%d %d * * *", now.Minute(), now.Hour())
	sc, err := cronOf(text)
	if err != nil {
		return
	}
	pb := &databasepb.BackupSchedule{Name: database + "/backupSchedules/" + defaultScheduleID,
		Spec:              &databasepb.BackupScheduleSpec{ScheduleSpec: &databasepb.BackupScheduleSpec_CronSpec{CronSpec: &databasepb.CrontabSpec{Text: text}}},
		RetentionDuration: durationpb.New(7 * 24 * 60 * 60 * 1e9),
		BackupTypeSpec:    &databasepb.BackupSchedule_FullBackupSpec{FullBackupSpec: &databasepb.FullBackupSpec{}}}
	s.normalize(pb)
	s.schedules[pb.Name] = &schedule{pb: pb, cron: sc, last: now}
}
