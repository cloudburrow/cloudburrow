package alloydb

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// takeBase takes a continuous backup base of cluster's primary server and
// records its time, the earliest the cluster can be recovered to, unless
// an earlier base of the same primary is recorded.
func (s *Server) takeBase(ctx context.Context, cluster, server string, fresh bool) error {
	if err := s.opts.Engine.PITRBase(ctx, server); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.bases[cluster]; !ok || fresh {
		// A base is complete at the next whole second (cmd/cloudburrow's
		// sqlpitr.go names it so).
		s.bases[cluster] = s.now().Truncate(time.Second).Add(time.Second)
	}
	return s.save()
}

// earliestRestorable is the earliest time cluster can be recovered to: its
// first base, or the start of its recovery window, whichever is later;
// the caller holds mu.
func (s *Server) earliestRestorable(cluster string) (time.Time, bool) {
	c, ok := s.clusters[cluster]
	if !ok || !c.GetContinuousBackupConfig().GetEnabled() {
		return time.Time{}, false
	}
	base, ok := s.bases[cluster]
	if !ok {
		return time.Time{}, false
	}
	window := s.now().Add(-time.Duration(c.GetContinuousBackupConfig().GetRecoveryWindowDays()) * 24 * time.Hour)
	if window.After(base) {
		return window, true
	}
	return base, true
}

// backupDir is where a backup's dump is kept.
func (s *Server) backupDir(b *alloydbpb.Backup) string {
	if s.opts.BackupsDir == "" || b.GetUid() == "" {
		return ""
	}
	return filepath.Join(s.opts.BackupsDir, b.GetUid())
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, e := d.Info(); e == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

func (s *Server) CreateBackup(_ context.Context, req *alloydbpb.CreateBackupRequest) (*longrunningpb.Operation, error) {
	if _, _, err := checkParent("parent", req.GetParent(), false); err != nil {
		return nil, err
	}
	if err := checkID("backup", req.GetBackupId()); err != nil {
		return nil, err
	}
	b := clone(req.GetBackup())
	if b == nil {
		b = &alloydbpb.Backup{}
	}
	if b.GetType() == alloydbpb.Backup_TYPE_UNSPECIFIED {
		b.Type = alloydbpb.Backup_ON_DEMAND
	}
	if b.GetType() != alloydbpb.Backup_ON_DEMAND {
		return nil, apierror.InvalidArgument("backup.type must be ON_DEMAND: AUTOMATED and CONTINUOUS backups are made by AlloyDB")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	return s.createBackup(req.GetParent()+"/backups/"+req.GetBackupId(), b, req.GetRequestId(), req.GetValidateOnly())
}

// createBackup records backup b of its cluster and dumps the primary; the
// caller holds mu.
func (s *Server) createBackup(name string, b *alloydbpb.Backup, requestID string, validate bool) (*longrunningpb.Operation, error) {
	if _, ok := s.backups[name]; ok {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	c, err := s.cluster(b.GetClusterName())
	if err != nil {
		if b.GetClusterName() == "" {
			return nil, apierror.InvalidArgument("backup.cluster_name is required")
		}
		return nil, err
	}
	if locationOf(c.GetName()) != locationOf(name) {
		return nil, apierror.InvalidArgument("backup %s must be in its cluster's location", name)
	}
	server, err := s.primaryServer(c.GetName())
	if err != nil {
		return nil, err
	}
	if s.opts.BackupsDir == "" {
		return nil, apierror.FailedPrecondition("this instance keeps no backups")
	}
	if validate {
		return s.validateOnly(locationOf(name), "create", name), nil
	}
	now := timestamppb.New(s.now())
	b.Name, b.Uid = name, newUID()
	b.CreateTime, b.UpdateTime = now, now
	b.State = alloydbpb.Backup_CREATING
	b.Reconciling = true
	b.ClusterUid = c.GetUid()
	b.DatabaseVersion = c.GetDatabaseVersion()
	if b.EncryptionConfig == nil && c.GetAutomatedBackupPolicy().GetEncryptionConfig() != nil && b.GetType() == alloydbpb.Backup_AUTOMATED {
		b.EncryptionConfig = clone(c.GetAutomatedBackupPolicy().GetEncryptionConfig())
	}
	b.EncryptionInfo = encryptionInfo(b.GetEncryptionConfig())
	b.Etag = etag()
	s.backups[name] = b
	op, err := s.startOp(locationOf(name), "create", name, requestID)
	if err != nil {
		return nil, err
	}
	dir := s.backupDir(b)
	users := s.usersOf(b.GetClusterName())
	s.background(op.Name, func(ctx context.Context) error {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := writeUsers(dir, users); err != nil {
			return err
		}
		return s.opts.Engine.Dump(ctx, server, dir)
	}, func(err error) (proto.Message, error) {
		b, ok := s.backups[name]
		if !ok {
			return nil, err
		}
		b.Reconciling = false
		if err != nil {
			b.State = alloydbpb.Backup_FAILED
			return nil, err
		}
		b.State = alloydbpb.Backup_READY
		b.CreateCompletionTime = timestamppb.New(s.now())
		b.SizeBytes = dirSize(dir)
		return clone(b), nil
	})
	return op, nil
}

func (s *Server) GetBackup(_ context.Context, req *alloydbpb.GetBackupRequest) (*alloydbpb.Backup, error) {
	if backupRE.FindStringSubmatch(req.GetName()) == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/backups/{backup}", req.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.backups[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	return clone(b), nil
}

func (s *Server) ListBackups(_ context.Context, req *alloydbpb.ListBackupsRequest) (*alloydbpb.ListBackupsResponse, error) {
	if _, _, err := checkParent("parent", req.GetParent(), true); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var all []*alloydbpb.Backup
	for _, k := range sortedKeys(s.backups, "") {
		if inLocation(k, req.GetParent()) {
			all = append(all, clone(s.backups[k]))
		}
	}
	s.mu.Unlock()
	out, next, err := listPage("alloydb-backups:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &alloydbpb.ListBackupsResponse{Backups: out, NextPageToken: next}, nil
}

var backupPaths = map[string]func(d, s *alloydbpb.Backup){
	"display_name": func(d, s *alloydbpb.Backup) { d.DisplayName = s.DisplayName },
	"labels":       func(d, s *alloydbpb.Backup) { d.Labels = s.Labels },
	"annotations":  func(d, s *alloydbpb.Backup) { d.Annotations = s.Annotations },
	"description":  func(d, s *alloydbpb.Backup) { d.Description = s.Description },
	"tags":         func(d, s *alloydbpb.Backup) { d.Tags = s.Tags },
}

func (s *Server) UpdateBackup(ctx context.Context, req *alloydbpb.UpdateBackupRequest) (*longrunningpb.Operation, error) {
	in := req.GetBackup()
	m := backupRE.FindStringSubmatch(in.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("backup.name %q must be projects/{project}/locations/{location}/backups/{backup}", in.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	cur, ok := s.backups[in.GetName()]
	if !ok {
		if req.GetAllowMissing() {
			b := clone(in)
			if b.GetType() == alloydbpb.Backup_TYPE_UNSPECIFIED {
				b.Type = alloydbpb.Backup_ON_DEMAND
			}
			return s.createBackup(in.GetName(), b, req.GetRequestId(), req.GetValidateOnly())
		}
		return nil, apierror.NotFound("Resource '%s' was not found", in.GetName())
	}
	if in.GetEtag() != "" && in.GetEtag() != cur.GetEtag() {
		return nil, apierror.Aborted("etag %q does not match the backup's", in.GetEtag())
	}
	next := clone(cur)
	for _, p := range maskPaths(req.GetUpdateMask().GetPaths(), keys(backupPaths)) {
		f, ok := backupPaths[p]
		if !ok {
			return nil, apierror.InvalidArgument("update_mask path %q is not a field UpdateBackup can change", p)
		}
		f(next, in)
	}
	if req.GetValidateOnly() {
		return s.validateOnly(locationOf(cur.GetName()), "update", cur.GetName()), nil
	}
	next.UpdateTime = timestamppb.New(s.now())
	next.Etag = etag()
	s.backups[cur.GetName()] = next
	return s.doneOp(locationOf(cur.GetName()), "update", cur.GetName(), req.GetRequestId(), clone(next))
}

func (s *Server) DeleteBackup(_ context.Context, req *alloydbpb.DeleteBackupRequest) (*longrunningpb.Operation, error) {
	if backupRE.FindStringSubmatch(req.GetName()) == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/backups/{backup}", req.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	b, ok := s.backups[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	if req.GetEtag() != "" && req.GetEtag() != b.GetEtag() {
		return nil, apierror.Aborted("etag %q does not match the backup's", req.GetEtag())
	}
	if b.GetState() == alloydbpb.Backup_CREATING {
		return nil, apierror.FailedPrecondition("backup %s is still being created", b.GetName())
	}
	if req.GetValidateOnly() {
		return s.validateOnly(locationOf(b.GetName()), "delete", b.GetName()), nil
	}
	if d := s.backupDir(b); d != "" {
		_ = os.RemoveAll(d)
	}
	delete(s.backups, b.GetName())
	for k, d := range s.restores {
		if d == s.backupDir(b) {
			delete(s.restores, k)
		}
	}
	return s.doneOp(locationOf(b.GetName()), "delete", b.GetName(), req.GetRequestId(), nil)
}

// RunAutomatedBackups takes the automated backups that are due at now, by
// each cluster's automated_backup_policy (weekly_schedule's days and start
// times, in UTC), and deletes those its retention no longer keeps. `up`
// calls it each minute.
func (s *Server) RunAutomatedBackups(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range sortedKeys(s.clusters, "") {
		c := s.clusters[name]
		p := c.GetAutomatedBackupPolicy()
		if p == nil || !p.GetEnabled() {
			continue
		}
		s.expireAutomated(c, now)
		due, ok := lastDue(p.GetWeeklySchedule(), now)
		if !ok || !due.After(s.lastAuto[name]) || now.Sub(due) > time.Hour {
			continue
		}
		if _, err := s.primaryServer(name); err != nil {
			continue
		}
		m := clusterRE.FindStringSubmatch(name)
		loc := p.GetLocation()
		if loc == "" {
			loc = m[2]
		}
		id := m[3] + "-" + now.UTC().Format("20060102-150405")
		b := &alloydbpb.Backup{Type: alloydbpb.Backup_AUTOMATED, ClusterName: name, Labels: p.GetLabels()}
		if r := p.GetTimeBasedRetention().GetRetentionPeriod(); r != nil {
			b.ExpiryTime = timestamppb.New(now.Add(r.AsDuration()))
		}
		s.lastAuto[name] = due
		if _, err := s.createBackup("projects/"+m[1]+"/locations/"+loc+"/backups/"+id, b, "", false); err != nil {
			s.logf("alloydb: automated backup of %s: %v", name, err)
		}
	}
}

// expireAutomated deletes c's automated backups past their retention;
// the caller holds mu.
func (s *Server) expireAutomated(c *alloydbpb.Cluster, now time.Time) {
	p := c.GetAutomatedBackupPolicy()
	var auto []*alloydbpb.Backup
	for _, k := range sortedKeys(s.backups, "") {
		b := s.backups[k]
		if b.GetType() == alloydbpb.Backup_AUTOMATED && b.GetClusterName() == c.GetName() && b.GetState() == alloydbpb.Backup_READY {
			auto = append(auto, b)
		}
	}
	keep := len(auto)
	if n := int(p.GetQuantityBasedRetention().GetCount()); n > 0 && n < keep {
		keep = n
	}
	// Newest last: the backups' IDs end in their times.
	for i, b := range auto {
		expired := i < len(auto)-keep || (b.GetExpiryTime() != nil && now.After(b.GetExpiryTime().AsTime()))
		if !expired {
			continue
		}
		if d := s.backupDir(b); d != "" {
			_ = os.RemoveAll(d)
		}
		delete(s.backups, b.GetName())
	}
}

// lastDue is the latest scheduled start at or before now.
func lastDue(w *alloydbpb.AutomatedBackupPolicy_WeeklySchedule, now time.Time) (time.Time, bool) {
	if w == nil || len(w.GetStartTimes()) == 0 {
		return time.Time{}, false
	}
	days := map[time.Weekday]bool{}
	for _, d := range w.GetDaysOfWeek() {
		if d == dayofweek.DayOfWeek_SUNDAY {
			days[time.Sunday] = true
		} else if d != dayofweek.DayOfWeek_DAY_OF_WEEK_UNSPECIFIED {
			days[time.Weekday(d)] = true
		}
	}
	if len(days) == 0 {
		for d := time.Sunday; d <= time.Saturday; d++ {
			days[d] = true
		}
	}
	now = now.UTC()
	var best time.Time
	for back := 0; back < 8; back++ {
		day := now.AddDate(0, 0, -back)
		if !days[day.Weekday()] {
			continue
		}
		for _, t := range w.GetStartTimes() {
			at := time.Date(day.Year(), day.Month(), day.Day(), int(t.GetHours()), int(t.GetMinutes()), 0, 0, time.UTC)
			if !at.After(now) && at.After(best) {
				best = at
			}
		}
	}
	return best, !best.IsZero()
}

// backupsOfCluster is a cluster's backups, for the console.
func (s *Server) backupsOfCluster(cluster string) []*alloydbpb.Backup {
	var out []*alloydbpb.Backup
	for _, k := range sortedKeys(s.backups, "") {
		if s.backups[k].GetClusterName() == cluster {
			out = append(out, clone(s.backups[k]))
		}
	}
	return out
}

// usersFile keeps, beside a dump, the users of the cluster it was taken
// from: a dump holds no roles, and a restored cluster's primary creates
// them before it loads the data, whose grants name them. Google's backups
// are of the whole cluster, users included.
const usersFile = "alloydb-users.json"

// usersOf are cluster's users; the caller holds mu.
func (s *Server) usersOf(cluster string) []*alloydbpb.User {
	var out []*alloydbpb.User
	for _, k := range sortedKeys(s.users, cluster+"/users/") {
		out = append(out, clone(s.users[k]))
	}
	return out
}

func writeUsers(dir string, users []*alloydbpb.User) error {
	var out []json.RawMessage
	for _, u := range users {
		b, err := protojson.Marshal(u)
		if err != nil {
			return err
		}
		out = append(out, b)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, usersFile), b, 0o600)
}

// readUsers are the users written beside a dump, none when there is no
// such file (a dump from before it was kept).
func readUsers(dir string) []*alloydbpb.User {
	b, err := os.ReadFile(filepath.Join(dir, usersFile))
	if err != nil {
		return nil
	}
	var raw []json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	var out []*alloydbpb.User
	for _, r := range raw {
		u := &alloydbpb.User{}
		if protojson.Unmarshal(r, u) == nil {
			out = append(out, u)
		}
	}
	return out
}
