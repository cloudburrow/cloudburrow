package alloydb

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Backup and DR (#1524). The Backup and DR Service protects a cluster
// through a backup plan association of resource type
// alloydb.googleapis.com/Cluster
// (docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/alloydb/alloydb-backup,
// read 2026-11-24). While it does, "the existing backup and restore
// settings are overwritten by the backup plan"
// (docs.cloud.google.com/alloydb/docs/backup/manage-enhanced-backups): the
// cluster's automated backups and continuous backup are turned off, what
// they were is kept in backupdrInfo.currentWindow, and they come back when
// the association is deleted ("your cluster is now managed under the
// Standard backups option"). A plan with logRetentionDays keeps the
// primary's WAL archived, as continuous backup does, so the data source can
// be recovered to a point in time.
//
// A vault's backup of a cluster is the dump an AlloyDB backup is (each
// database and the users), kept under the run's ID outside the project's
// backups, so ListBackups does not show it and it outlives the cluster;
// with log retention a continuous backup base (pg_basebackup) is taken
// with it. RestoreCluster restores one into a new cluster with
// backupdrBackupSource, or a data source's cluster to a time with
// backupdrPitrSource (the restore guide's gcloud beta alloydb clusters
// restore --backupdr-backup and --backupdr-data-source --point-in-time):
// "Restore the backup to a new cluster", then create its primary, since
// "Backup and DR does not store instance configuration as part of the
// backup"
// (docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/alloydb/alloydb-restore).
//
// Those request fields and Cluster's backupdrInfo and backupdrBackupSource
// are in the REST discovery document (google.golang.org/api/alloydb/v1,
// v0.314.0) and in no published alloydbpb (cloud.google.com/go/alloydb
// through v1.33.1): they are served over REST through the transcoder's
// overlay, as Cloud Monitoring's uptime fields are (#2146).

const (
	fieldBDRBackupSource = "backupdr_backup_source"
	fieldBDRPitrSource   = "backupdr_pitr_source"
	fieldBDRInfo         = "backupdr_info"
	// The overlay's own numbers, never stored as wire bytes.
	numBDRBackupSource = 1000001
	numBDRPitrSource   = 1000002
	numBDRInfo         = 1000003
)

// alloydbOverlay is AlloyDB's v1, v1beta and v1alpha with the Backup and
// DR fields the discovery document publishes.
var alloydbOverlay = rest.MustOverlays(Packages(), addBackupDRFields)

// Packages are the proto packages of the served versions.
func Packages() []string {
	out := make([]string, len(Versions))
	for i, v := range Versions {
		out[i] = "google.cloud.alloydb." + v
	}
	return out
}

func overlayField(name, json string, num int32, typ descriptorpb.FieldDescriptorProto_Type, msg string, repeated bool) *descriptorpb.FieldDescriptorProto {
	f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), JsonName: proto.String(json), Number: proto.Int32(num),
		Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: typ.Enum()}
	if repeated {
		f.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	}
	if msg != "" {
		f.TypeName = proto.String(msg)
	}
	return f
}

func addBackupDRFields(f *descriptorpb.FileDescriptorProto) {
	pkg := "." + f.GetPackage() + "."
	const (
		tMsg  = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
		tStr  = descriptorpb.FieldDescriptorProto_TYPE_STRING
		tBool = descriptorpb.FieldDescriptorProto_TYPE_BOOL
		tI32  = descriptorpb.FieldDescriptorProto_TYPE_INT32
	)
	has := func(m *descriptorpb.DescriptorProto, name string) bool {
		for _, fd := range m.GetField() {
			if fd.GetName() == name {
				return true
			}
		}
		return false
	}
	hasMsg := func(name string) bool {
		for _, m := range f.GetMessageType() {
			if m.GetName() == name {
				return true
			}
		}
		return false
	}
	switch {
	case strings.HasSuffix(f.GetName(), "/resources.proto"):
		add := func(name string, fields ...*descriptorpb.FieldDescriptorProto) {
			if !hasMsg(name) {
				f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{Name: proto.String(name), Field: fields})
			}
		}
		add("BackupDrBackupSource", overlayField("backup", "backup", 1, tStr, "", false))
		add("BackupDrPitrSource", overlayField("data_source", "dataSource", 1, tStr, "", false),
			overlayField("point_in_time", "pointInTime", 2, tMsg, ".google.protobuf.Timestamp", false))
		add("BackupDrEnabledWindow",
			overlayField("automated_backup_previously_enabled", "automatedBackupPreviouslyEnabled", 1, tBool, "", false),
			overlayField("backup_plan_association", "backupPlanAssociation", 2, tStr, "", false),
			overlayField("continuous_backup_previous_recovery_window_days", "continuousBackupPreviousRecoveryWindowDays", 3, tI32, "", false),
			overlayField("continuous_backup_previously_enabled", "continuousBackupPreviouslyEnabled", 4, tBool, "", false),
			overlayField("continuous_backup_previously_enabled_time", "continuousBackupPreviouslyEnabledTime", 5, tMsg, ".google.protobuf.Timestamp", false),
			overlayField("data_source", "dataSource", 6, tStr, "", false),
			overlayField("disabled_time", "disabledTime", 7, tMsg, ".google.protobuf.Timestamp", false),
			overlayField("enabled_time", "enabledTime", 8, tMsg, ".google.protobuf.Timestamp", false),
			overlayField("log_retention_period", "logRetentionPeriod", 9, tMsg, ".google.protobuf.Duration", false))
		add("BackupDrInfo", overlayField("current_window", "currentWindow", 1, tMsg, pkg+"BackupDrEnabledWindow", false),
			overlayField("previous_windows", "previousWindows", 2, tMsg, pkg+"BackupDrEnabledWindow", true))
		for _, m := range f.GetMessageType() {
			if m.GetName() != "Cluster" {
				continue
			}
			if !has(m, fieldBDRBackupSource) {
				m.Field = append(m.Field, overlayField(fieldBDRBackupSource, "backupdrBackupSource", numBDRBackupSource, tMsg, pkg+"BackupDrBackupSource", false))
			}
			if !has(m, fieldBDRInfo) {
				m.Field = append(m.Field, overlayField(fieldBDRInfo, "backupdrInfo", numBDRInfo, tMsg, pkg+"BackupDrInfo", false))
			}
		}
	case strings.HasSuffix(f.GetName(), "/service.proto"):
		for _, m := range f.GetMessageType() {
			if m.GetName() != "RestoreClusterRequest" {
				continue
			}
			if !has(m, fieldBDRBackupSource) {
				m.Field = append(m.Field, overlayField(fieldBDRBackupSource, "backupdrBackupSource", numBDRBackupSource, tMsg, pkg+"BackupDrBackupSource", false))
			}
			if !has(m, fieldBDRPitrSource) {
				m.Field = append(m.Field, overlayField(fieldBDRPitrSource, "backupdrPitrSource", numBDRPitrSource, tMsg, pkg+"BackupDrPitrSource", false))
			}
		}
	}
}

// overlayJSON is an added field of m as JSON, nil when m does not set it.
func overlayJSON(m proto.Message, field protoreflect.Name) map[string]any {
	v, ok := alloydbOverlay.Get(m, field)
	if !ok {
		return nil
	}
	b, err := protojson.MarshalOptions{Resolver: alloydbOverlay}.Marshal(v.Message().Interface())
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// setOverlayJSON sets an added message field of m from JSON (nil clears it).
func setOverlayJSON(m proto.Message, field protoreflect.Name, v any) error {
	d, err := alloydbOverlay.Copy(m)
	if err != nil || d == nil {
		return fmt.Errorf("alloydb: overlay has no %s", m.ProtoReflect().Descriptor().FullName())
	}
	fd := d.ProtoReflect().Descriptor().Fields().ByName(field)
	if fd == nil {
		return fmt.Errorf("alloydb: overlay has no field %s", field)
	}
	if v == nil {
		d.ProtoReflect().Clear(fd)
	} else {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		val := d.ProtoReflect().NewField(fd)
		if err := (protojson.UnmarshalOptions{Resolver: alloydbOverlay}).Unmarshal(b, val.Message().Interface()); err != nil {
			return err
		}
		d.ProtoReflect().Set(fd, val)
	}
	return alloydbOverlay.Back(d, m)
}

// BackupDR is the Backup and DR Service as AlloyDB's restores see it.
type BackupDR interface {
	// AlloyDBBackup is the run a Backup and DR backup of a cluster holds
	// and the cluster it was taken of: an error when it is no such backup,
	// is not ACTIVE, or its vault's key cannot be used.
	AlloyDBBackup(backup string) (runID, cluster string, err error)
	// AlloyDBDataSource is the cluster a data source protects.
	AlloyDBDataSource(dataSource string) (cluster string, err error)
	// RestoreStarted and RestoreFinished log a restore job (#1924).
	RestoreStarted(category, source, restoreProject, restoreResource, recoveryPoint string) string
	RestoreFinished(jobID string, err error)
}

// SetBackupDR serves RestoreCluster's backupdrBackupSource and
// backupdrPitrSource through b; without it they are FAILED_PRECONDITION.
func (s *Server) SetBackupDR(b BackupDR) {
	s.mu.Lock()
	s.bdrAPI = b
	s.mu.Unlock()
}

// bdrWindow is one window of Backup and DR protection of a cluster.
type bdrWindow struct {
	Association, DataSource string
	LogDays                 int64
	Enabled, Disabled       time.Time
	PrevAutomated           bool
	PrevContinuous          bool
	PrevWindowDays          int32
	PrevContinuousTime      time.Time
}

// bdrState is a cluster's Backup and DR protection: the current window
// (nil when it is not protected) and the earlier ones.
type bdrState struct {
	Current  *bdrWindow  `json:",omitempty"`
	Previous []bdrWindow `json:",omitempty"`
}

func windowJSON(w bdrWindow) map[string]any {
	out := map[string]any{"backupPlanAssociation": w.Association, "dataSource": w.DataSource,
		"automatedBackupPreviouslyEnabled": w.PrevAutomated, "continuousBackupPreviouslyEnabled": w.PrevContinuous,
		"enabledTime": w.Enabled.UTC().Format(time.RFC3339Nano)}
	if w.PrevWindowDays > 0 {
		out["continuousBackupPreviousRecoveryWindowDays"] = w.PrevWindowDays
	}
	if !w.PrevContinuousTime.IsZero() {
		out["continuousBackupPreviouslyEnabledTime"] = w.PrevContinuousTime.UTC().Format(time.RFC3339Nano)
	}
	if !w.Disabled.IsZero() {
		out["disabledTime"] = w.Disabled.UTC().Format(time.RFC3339Nano)
	}
	if w.LogDays > 0 {
		out["logRetentionPeriod"] = fmt.Sprintf("%ds", w.LogDays*86400)
	}
	return out
}

// bdrView sets c's backupdrInfo, for an answer; the caller holds mu.
func (s *Server) bdrView(c *alloydbpb.Cluster) {
	st := s.bdr[c.GetName()]
	if st == nil || (st.Current == nil && len(st.Previous) == 0) {
		return
	}
	info := map[string]any{}
	if st.Current != nil {
		info["currentWindow"] = windowJSON(*st.Current)
	}
	if len(st.Previous) > 0 {
		var prev []any
		for _, w := range st.Previous {
			prev = append(prev, windowJSON(w))
		}
		info["previousWindows"] = prev
	}
	if err := setOverlayJSON(c, fieldBDRInfo, info); err != nil {
		s.logf("alloydb: backupdrInfo of %s: %v", c.GetName(), err)
	}
}

// pitrOn is whether c's primary archives its WAL: continuous backup, or a
// Backup and DR plan with log retention; the caller holds mu.
func (s *Server) pitrOn(c *alloydbpb.Cluster) bool {
	if c.GetContinuousBackupConfig().GetEnabled() {
		return true
	}
	st := s.bdr[c.GetName()]
	return st != nil && st.Current != nil && st.Current.LogDays > 0
}

// BackupDRCluster is what the Backup and DR Service records of a cluster.
type BackupDRCluster struct {
	Name, UID, CreateTime, DatabaseVersion string
	// PITRWindows are the point-in-time recovery windows, oldest first:
	// start and end (end "" for the current one) and log retention days.
	PITRWindows []map[string]any
}

// BackupDRCluster is cluster name, for an association.
func (s *Server) BackupDRCluster(name string) (BackupDRCluster, error) {
	if clusterRE.FindStringSubmatch(name) == nil {
		return BackupDRCluster{}, apierror.InvalidArgument("%q is not an AlloyDB cluster: the format is projects/{project}/locations/{location}/clusters/{cluster}", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.cluster(name)
	if err != nil {
		return BackupDRCluster{}, apierror.NotFound("The AlloyDB cluster %s does not exist.", name)
	}
	out := BackupDRCluster{Name: name, UID: c.GetUid(), CreateTime: c.GetCreateTime().AsTime().UTC().Format(time.RFC3339Nano),
		DatabaseVersion: c.GetDatabaseVersion().String()}
	if st := s.bdr[name]; st != nil {
		ws := append([]bdrWindow(nil), st.Previous...)
		if st.Current != nil {
			ws = append(ws, *st.Current)
		}
		for _, w := range ws {
			if w.LogDays == 0 {
				continue
			}
			start, end := s.bdrPITRRange(name, w)
			if start.IsZero() {
				continue
			}
			pw := map[string]any{"startTime": start.UTC().Format(time.RFC3339Nano), "logRetentionDays": fmt.Sprint(w.LogDays)}
			if !w.Disabled.IsZero() {
				pw["endTime"] = end.UTC().Format(time.RFC3339Nano)
			}
			out.PITRWindows = append(out.PITRWindows, pw)
		}
	}
	return out, nil
}

// bdrPITRRange is the span window w lets the cluster be recovered to: from
// its first base and the window's start, and no longer ago than its log
// retention, until it ended (or now); zero when no base was taken. The
// caller holds mu.
func (s *Server) bdrPITRRange(cluster string, w bdrWindow) (time.Time, time.Time) {
	base, ok := s.bases[cluster]
	if !ok {
		return time.Time{}, time.Time{}
	}
	start := base
	if w.Enabled.After(start) {
		start = w.Enabled
	}
	end := s.now()
	if !w.Disabled.IsZero() {
		end = w.Disabled
	}
	if lim := s.now().Add(-time.Duration(w.LogDays) * 24 * time.Hour); lim.After(start) {
		start = lim
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}
	}
	return start, end
}

// ProtectCluster starts Backup and DR protection of cluster name by
// association, whose backups go in dataSource, with logDays of log
// retention: the cluster's own automated backups and continuous backup
// are turned off, and its WAL archived for the data source when logDays is
// set. It answers once the primary runs with that.
func (s *Server) ProtectCluster(ctx context.Context, name, association, dataSource string, logDays int64) error {
	s.mu.Lock()
	c, err := s.cluster(name)
	if err != nil {
		s.mu.Unlock()
		return apierror.NotFound("The AlloyDB cluster %s does not exist.", name)
	}
	if c.GetClusterType() == alloydbpb.Cluster_SECONDARY {
		s.mu.Unlock()
		return apierror.InvalidArgument("The AlloyDB cluster %s is a secondary cluster: it replicates its primary cluster, which a backup plan protects.", name)
	}
	if s.bdr == nil {
		s.bdr = map[string]*bdrState{}
	}
	st := s.bdr[name]
	if st == nil {
		st = &bdrState{}
		s.bdr[name] = st
	}
	was := s.pitrOn(c)
	now := s.now()
	if st.Current != nil && st.Current.Association == association {
		// A plan change (backupPlanAssociations.patch): the same window.
		st.Current.DataSource, st.Current.LogDays = dataSource, logDays
	} else {
		if st.Current != nil {
			old := *st.Current
			old.Disabled = now
			st.Previous = append(st.Previous, old)
		}
		w := &bdrWindow{Association: association, DataSource: dataSource, LogDays: logDays, Enabled: now,
			PrevAutomated: c.GetAutomatedBackupPolicy().GetEnabled(), PrevContinuous: c.GetContinuousBackupConfig().GetEnabled(),
			PrevWindowDays: c.GetContinuousBackupConfig().GetRecoveryWindowDays()}
		if t := c.GetContinuousBackupInfo().GetEnabledTime(); t != nil {
			w.PrevContinuousTime = t.AsTime()
		}
		st.Current = w
		next := clone(c)
		if p := next.GetAutomatedBackupPolicy(); p != nil {
			p.Enabled = proto.Bool(false)
		}
		if next.ContinuousBackupConfig == nil {
			next.ContinuousBackupConfig = &alloydbpb.ContinuousBackupConfig{RecoveryWindowDays: defaultRecoveryWindowDays}
		}
		next.ContinuousBackupConfig.Enabled = proto.Bool(false)
		s.stampContinuous(next)
		next.UpdateTime = timestamppb.New(now)
		s.clusters[name] = next
		c = next
	}
	return s.applyPITR(ctx, c, was)
}

// UnprotectCluster ends Backup and DR protection of cluster name: its
// automated backups and continuous backup are as they were before.
func (s *Server) UnprotectCluster(ctx context.Context, name string) error {
	s.mu.Lock()
	c, err := s.cluster(name)
	st := s.bdr[name]
	if err != nil || st == nil || st.Current == nil {
		s.mu.Unlock()
		return nil
	}
	was := s.pitrOn(c)
	w := *st.Current
	w.Disabled = s.now()
	st.Previous = append(st.Previous, w)
	st.Current = nil
	next := clone(c)
	if p := next.GetAutomatedBackupPolicy(); p != nil {
		p.Enabled = proto.Bool(w.PrevAutomated)
	}
	if next.ContinuousBackupConfig == nil {
		next.ContinuousBackupConfig = &alloydbpb.ContinuousBackupConfig{}
	}
	next.ContinuousBackupConfig.Enabled = proto.Bool(w.PrevContinuous)
	if w.PrevWindowDays > 0 {
		next.ContinuousBackupConfig.RecoveryWindowDays = w.PrevWindowDays
	}
	s.stampContinuous(next)
	next.UpdateTime = timestamppb.New(s.now())
	s.clusters[name] = next
	return s.applyPITR(ctx, next, was)
}

// applyPITR restarts c's primary when its WAL archiving changed from was,
// and takes a base when it is now on; called with mu held, it releases it.
func (s *Server) applyPITR(ctx context.Context, c *alloydbpb.Cluster, was bool) error {
	_ = s.save()
	now := s.pitrOn(c)
	primary := s.primaryOf(c.GetName())
	if primary == nil || was == now || s.opts.Engine == nil {
		s.mu.Unlock()
		return nil
	}
	spec, err := s.specOf(primary)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := s.opts.Engine.Start(ctx, spec); err != nil {
		return err
	}
	if now {
		return s.takeBase(ctx, c.GetName(), spec.Server, !was)
	}
	return nil
}

// bdrDir is where a Backup and DR backup's dump is kept.
func (s *Server) bdrDir(runID string) string {
	if s.opts.BackupsDir == "" || runID == "" || strings.ContainsAny(runID, `/\.`) {
		return ""
	}
	return filepath.Join(s.opts.BackupsDir, "backupdr", runID)
}

// BackupDRBackup takes a vault's backup of cluster name, kept as runID: its
// primary's databases dumped with its users, as an AlloyDB backup is, and a
// new continuous backup base when its WAL is archived for the data source.
// It answers when the backup is taken, with its size and the cluster's
// database version.
func (s *Server) BackupDRBackup(ctx context.Context, name, runID string) (int64, string, error) {
	dir := s.bdrDir(runID)
	if dir == "" {
		return 0, "", apierror.FailedPrecondition("this instance keeps no AlloyDB backups")
	}
	s.mu.Lock()
	c, err := s.cluster(name)
	if err != nil {
		s.mu.Unlock()
		return 0, "", apierror.NotFound("The AlloyDB cluster %s does not exist.", name)
	}
	server, err := s.primaryServer(name)
	if err != nil {
		s.mu.Unlock()
		return 0, "", err
	}
	if s.opts.Engine == nil {
		s.mu.Unlock()
		return 0, "", apierror.FailedPrecondition("no engine runs AlloyDB instances here")
	}
	users := s.usersOf(name)
	version := c.GetDatabaseVersion().String()
	pitr := s.pitrOn(c)
	s.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, "", err
	}
	fail := func(err error) (int64, string, error) {
		_ = os.RemoveAll(dir)
		return 0, "", err
	}
	if err := writeUsers(dir, users); err != nil {
		return fail(err)
	}
	if err := s.opts.Engine.Dump(ctx, server, dir); err != nil {
		return fail(err)
	}
	if err := writeVersion(dir, version); err != nil {
		return fail(err)
	}
	if pitr {
		if err := s.takeBase(ctx, name, server, false); err != nil {
			return fail(fmt.Errorf("the continuous backup base: %w", err))
		}
	}
	return dirSize(dir), version, nil
}

// DeleteBackupDRBackup deletes a vault's backup's dump.
func (s *Server) DeleteBackupDRBackup(runID string) error {
	dir := s.bdrDir(runID)
	if dir == "" {
		return nil
	}
	return os.RemoveAll(dir)
}

// bdrRestoreSource resolves RestoreClusterRequest's Backup and DR source:
// the dump to load (a backup's), or the source cluster's server and the
// time to recover (a data source's). ok is false without either. The
// caller holds mu.
func (s *Server) bdrRestoreSource(req *alloydbpb.RestoreClusterRequest, c *alloydbpb.Cluster) (src restoreSource, ok bool, err error) {
	bs := overlayJSON(req, fieldBDRBackupSource)
	ps := overlayJSON(req, fieldBDRPitrSource)
	if bs == nil && ps == nil {
		return restoreSource{}, false, nil
	}
	if req.GetBackupSource() != nil || req.GetContinuousBackupSource() != nil || (bs != nil && ps != nil) {
		return restoreSource{}, true, apierror.InvalidArgument("only one of backup_source, continuous_backup_source, backupdr_backup_source and backupdr_pitr_source may be set")
	}
	if s.bdrAPI == nil {
		return restoreSource{}, true, apierror.FailedPrecondition("the Backup and DR Service is not served by this instance: run up with backupdr in --services")
	}
	if bs != nil {
		name, _ := bs["backup"].(string)
		if name == "" {
			return restoreSource{}, true, apierror.InvalidArgument("backupdr_backup_source.backup is required")
		}
		runID, cluster, err := s.bdrAPI.AlloyDBBackup(name)
		if err != nil {
			return restoreSource{}, true, err
		}
		dir := s.bdrDir(runID)
		if fi, err := os.Stat(dir); dir == "" || err != nil || !fi.IsDir() {
			return restoreSource{}, true, apierror.FailedPrecondition("the data of Backup and DR backup %s is not on this instance", name)
		}
		version := alloydbpb.DatabaseVersion_DATABASE_VERSION_UNSPECIFIED
		if v := readVersion(dir); v != "" {
			version = alloydbpb.DatabaseVersion(alloydbpb.DatabaseVersion_value[v])
		}
		src = restoreSource{dir: dir, category: "RESTORE", source: name, fromCluster: cluster, version: version}
		return src, true, nil
	}
	dsName, _ := ps["dataSource"].(string)
	pit, _ := ps["pointInTime"].(string)
	switch {
	case dsName == "":
		return restoreSource{}, true, apierror.InvalidArgument("backupdr_pitr_source.data_source is required")
	case pit == "":
		return restoreSource{}, true, apierror.InvalidArgument("backupdr_pitr_source.point_in_time is required")
	}
	at, err := time.Parse(time.RFC3339Nano, pit)
	if err != nil {
		return restoreSource{}, true, apierror.InvalidArgument("backupdr_pitr_source.point_in_time %q is not a time", pit)
	}
	cluster, err := s.bdrAPI.AlloyDBDataSource(dsName)
	if err != nil {
		return restoreSource{}, true, err
	}
	srcC, err := s.cluster(cluster)
	if err != nil {
		return restoreSource{}, true, apierror.FailedPrecondition("the AlloyDB cluster %s that data source %s protects no longer exists here, so its logs cannot be replayed", cluster, dsName)
	}
	inWindow := false
	if st := s.bdr[cluster]; st != nil {
		ws := append([]bdrWindow(nil), st.Previous...)
		if st.Current != nil {
			ws = append(ws, *st.Current)
		}
		for _, w := range ws {
			if w.DataSource != dsName || w.LogDays == 0 {
				continue
			}
			if start, end := s.bdrPITRRange(cluster, w); !start.IsZero() && !at.Before(start) && !at.After(end) {
				inWindow = true
			}
		}
	}
	if !inWindow {
		return restoreSource{}, true, apierror.InvalidArgument("point_in_time %s is outside data source %s's recovery window: enter a target time within this cluster's recovery window", at.UTC().Format(time.RFC3339Nano), dsName)
	}
	p := s.primaryOf(cluster)
	if p == nil {
		return restoreSource{}, true, apierror.FailedPrecondition("cluster %s has no primary instance to recover", cluster)
	}
	if s.restoreRoot() == "" {
		return restoreSource{}, true, apierror.FailedPrecondition("point-in-time recovery is not available: this instance keeps no backups")
	}
	return restoreSource{server: serverName(p.GetName()), at: at, dir: filepath.Join(s.restoreRoot(), randomHex(8)), category: "PITR",
		source: dsName, fromCluster: cluster, version: srcC.GetDatabaseVersion(), recoveryPoint: at.UTC().Format(time.RFC3339Nano)}, true, nil
}

// restoreSource is what RestoreCluster restores from.
type restoreSource struct {
	// dir is a dump to load, or (with server) where the recovery is dumped.
	dir         string
	server      string
	at          time.Time
	fromCluster string
	version     alloydbpb.DatabaseVersion
	// category, source and recoveryPoint are the Backup and DR restore
	// job's.
	category, source, recoveryPoint string
}

// versionFile records a Backup and DR backup's database version beside
// its dump.
const versionFile = "database-version"

func writeVersion(dir, v string) error {
	return os.WriteFile(filepath.Join(dir, versionFile), []byte(v), 0o600)
}

func readVersion(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, versionFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
