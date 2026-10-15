package sqladmin

// Point-in-time recovery (#1330). A created instance with
// settings.backupConfiguration.pointInTimeRecoveryEnabled (PostgreSQL) or
// binaryLogEnabled (MySQL) keeps the engine's own transaction log beside
// its data: PostgreSQL archives its write-ahead log, MySQL keeps its
// binary log. Each base, a pg_basebackup or a mysqldump taken at the
// binary log position it is consistent with, is taken when the instance
// starts and with each of its backups. A clone to cloneContext.pointInTime
// starts from the newest base taken before that time and replays the log
// to it: PostgreSQL to recovery_target_time in a scratch server, MySQL up
// to the first transaction whose commit timestamp is later, both to the
// microsecond. See docs/cloudsql.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// PITRDumper is a Dumper that can recover a server to a point in time.
type PITRDumper interface {
	// PITRBase takes a base that a recovery can start from.
	PITRBase(ctx context.Context, t DumpTarget) error
	// RecoveryWindow is the earliest and latest times the target can be
	// recovered to; ok is false when it has no base yet.
	RecoveryWindow(ctx context.Context, t DumpTarget) (earliest, latest time.Time, ok bool, err error)
	// Prune drops the bases older than the newest one complete by before,
	// and the log only they needed, as the retention window passes them.
	Prune(ctx context.Context, t DumpTarget, before time.Time) error
	// CopyAt loads src's databases as they were at `at` into dst, a
	// created instance's fresh server, using dir as scratch space.
	CopyAt(ctx context.Context, src, dst DumpTarget, at time.Time, dir string) error
}

// PITREnabled is whether spec keeps its transaction log for point-in-time
// recovery: pointInTimeRecoveryEnabled for PostgreSQL, binaryLogEnabled
// for MySQL, as Cloud SQL names them.
func (s Spec) PITREnabled() bool {
	bc, _ := s.Settings["backupConfiguration"].(map[string]any)
	key := "pointInTimeRecoveryEnabled"
	if s.MySQL() {
		key = "binaryLogEnabled"
	}
	on, _ := bc[key].(bool)
	return on
}

func (s *Server) pitr() PITRDumper {
	if s.backups == nil {
		return nil
	}
	p, _ := s.backups.dumper.(PITRDumper)
	return p
}

// pitrSpec is a created instance's spec when it keeps its log.
func (s *Server) pitrSpec(name string) (Spec, bool) {
	s.emu.RLock()
	defer s.emu.RUnlock()
	if s.writes == nil {
		return Spec{}, false
	}
	c := s.writes.byName[name]
	if c == nil || !c.spec.PITREnabled() {
		return Spec{}, false
	}
	return c.spec, true
}

// takePITRBase takes a base for a created instance that keeps its log; it
// is a no-op for any other. Errors are the operation's caller's to report.
func (s *Server) takePITRBase(name string) error {
	p := s.pitr()
	spec, ok := s.pitrSpec(name)
	if p == nil || !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	t := DumpTarget{Instance: name, MySQL: spec.MySQL(), Created: true}
	if err := p.PITRBase(ctx, t); err != nil {
		return err
	}
	_ = p.Prune(ctx, t, s.retentionStart(spec))
	return nil
}

// pitrUnavailable is why an instance cannot be recovered to a time, or
// nil when it can.
func (s *Server) pitrUnavailable(e Engine) error {
	if s.pitr() == nil {
		return unimplemented("point-in-time recovery", "this server was started without a backup store (docs/cloudsql.md)")
	}
	if _, ok := s.pitrSpec(e.Instance()); !ok {
		t := s.target(e)
		key := "pointInTimeRecoveryEnabled"
		if t.MySQL {
			key = "binaryLogEnabled"
		}
		why := ""
		if !t.Created {
			why = "; it is the server `cloudburrow up` runs, which keeps no transaction log: create an instance with it enabled"
		}
		return Errorf(http.StatusBadRequest, "Invalid request: point-in-time recovery is not enabled on instance %s "+
			"(settings.backupConfiguration.%s)%s.", e.Instance(), key, why)
	}
	return nil
}

// latestRecoveryTime is projects.instances.getLatestRecoveryTime.
func (s *Server) latestRecoveryTime(project string, e Engine) (any, error) {
	if err := s.pitrUnavailable(e); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()
	earliest, latest, ok, err := s.recoveryWindow(ctx, e)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: instance %s has no base backup to recover from yet.", e.Instance())
	}
	return map[string]any{"kind": "sql#getLatestRecoveryTime",
		"earliestRecoveryTime": earliest.UTC().Format(time.RFC3339Nano),
		"latestRecoveryTime":   latest.UTC().Format(time.RFC3339Nano)}, nil
}

// checkPointInTime parses a clone's pointInTime and checks it is within
// the source's recovery window.
func (s *Server) checkPointInTime(e Engine, raw string) (time.Time, error) {
	if err := s.pitrUnavailable(e); err != nil {
		return time.Time{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, Errorf(http.StatusBadRequest, "Invalid request: cloneContext.pointInTime %q is not an RFC 3339 time.", raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()
	earliest, latest, ok, err := s.recoveryWindow(ctx, e)
	if err != nil {
		return time.Time{}, err
	}
	if !ok || at.Before(earliest) || at.After(latest) {
		window := "none yet"
		if ok {
			window = earliest.UTC().Format(time.RFC3339Nano) + " to " + latest.UTC().Format(time.RFC3339Nano)
		}
		return time.Time{}, Errorf(http.StatusBadRequest, "Invalid request: the point in time %s is outside instance %s's "+
			"recovery window (%s).", at.UTC().Format(time.RFC3339Nano), e.Instance(), window)
	}
	return at, nil
}

// copyAt is copyData to a point in time.
func (s *Server) copyAt(src, dst DumpTarget, at time.Time) error {
	dir, err := os.MkdirTemp(s.backups.dir, "pitr-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 2*startTimeout)
	defer cancel()
	if err := s.pitr().CopyAt(ctx, src, dst, at, dir); err != nil {
		return fmt.Errorf("recover %s to %s: %w", src.Instance, at.UTC().Format(time.RFC3339Nano), err)
	}
	return nil
}

// Retention (#1330). Cloud SQL keeps transactionLogRetentionDays of
// transaction log, 1 to 7 days on the Enterprise edition (7 by default)
// and 1 to 35 on Enterprise Plus (14 by default), and the earliest time an
// instance can be recovered to moves forward as older log is dropped.
// Here the sweep (each minute, after each base, and after the clock is
// advanced) drops every base older than the newest one complete by the
// window's start, and the WAL or binary log only those needed.

// Clock is the retention clock: the time now, plus every Advance.
func (s *Server) Clock() time.Time {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	now := time.Now()
	for _, a := range s.advances {
		now = now.Add(a.by)
	}
	return now
}

// clockAdvance is one Advance: at a real time, by how much.
type clockAdvance struct {
	at time.Time
	by time.Duration
}

// Advance ages the log already kept by d, as if d had passed, sweeps, and
// returns the retention clock's new time. It is a CloudBurrow extension
// (ClockPath), for tests that cannot wait days. The servers' own clocks,
// which their log is stamped by and a pointInTime is read on, do not move:
// what was written before the advance is d older, what is written after
// ages from when it was written.
func (s *Server) Advance(d time.Duration) (time.Time, error) {
	if d < 0 {
		return time.Time{}, Errorf(http.StatusBadRequest, "Invalid request: the clock only moves forward: %s.", d)
	}
	s.clockMu.Lock()
	s.advances = append(s.advances, clockAdvance{time.Now(), d})
	s.clockMu.Unlock()
	s.sweepRetention()
	return s.Clock(), nil
}

// retentionFloor is the earliest server time whose log is no older than
// keep, now: a record written at T is now-T old, plus every advance made
// after T.
func (s *Server) retentionFloor(keep time.Duration) time.Time {
	s.clockMu.Lock()
	advs := append([]clockAdvance(nil), s.advances...)
	s.clockMu.Unlock()
	now := time.Now()
	var lower time.Time
	for j := 0; j <= len(advs); j++ {
		// Records in [lower, upper] are aged by the advances from j on.
		var aged time.Duration
		for _, a := range advs[j:] {
			aged += a.by
		}
		upper := now
		if j < len(advs) {
			upper = advs[j].at
		}
		t := now.Add(aged - keep)
		if !lower.IsZero() && t.Before(lower) {
			t = lower
		}
		if !t.After(upper) {
			return t
		}
		lower = upper
	}
	return now
}

// ClockPath advances the retention clock: POST {"duration": "86400s"}
// answers {"now": RFC 3339}.
const ClockPath = "/cloudburrow/v1/clock:advance"

func (s *Server) serveClock(r *http.Request) (any, error) {
	if r.Method != http.MethodPost {
		return nil, Errorf(http.StatusNotFound, "method %s is not served on %s", r.Method, ClockPath)
	}
	var body struct {
		Duration string `json:"duration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: %v.", err)
	}
	d, err := parseSeconds(body.Duration)
	if err != nil {
		return nil, err
	}
	now, err := s.Advance(d)
	if err != nil {
		return nil, err
	}
	return map[string]any{"now": now.UTC().Format(time.RFC3339Nano)}, nil
}

// parseSeconds parses a protobuf Duration's JSON form, such as "86400s".
func parseSeconds(v string) (time.Duration, error) {
	f, err := strconv.ParseFloat(strings.TrimSuffix(v, "s"), 64)
	if err != nil || !strings.HasSuffix(v, "s") {
		return 0, Errorf(http.StatusBadRequest, "Invalid request: duration %q is not seconds, such as \"86400s\".", v)
	}
	return time.Duration(f * float64(time.Second)), nil
}

// isEnterprisePlus is whether settings name the Enterprise Plus edition.
func isEnterprisePlus(settings map[string]any) bool {
	ed, _ := settings["edition"].(string)
	return ed == "ENTERPRISE_PLUS"
}

// transactionLogDays is spec's transactionLogRetentionDays, or Cloud SQL's
// default for its edition.
func transactionLogDays(spec Spec) int {
	bc, _ := spec.Settings["backupConfiguration"].(map[string]any)
	if n := intSetting(bc["transactionLogRetentionDays"]); n > 0 {
		return n
	}
	if isEnterprisePlus(spec.Settings) {
		return 14
	}
	return 7
}

func intSetting(v any) int {
	switch v := v.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

// retentionStart is the start of spec's retention window, on the
// servers' clock.
func (s *Server) retentionStart(spec Spec) time.Time {
	return s.retentionFloor(time.Duration(transactionLogDays(spec)) * 24 * time.Hour)
}

// recoveryWindow is the dumper's window, starting no earlier than the
// retention window does.
func (s *Server) recoveryWindow(ctx context.Context, e Engine) (time.Time, time.Time, bool, error) {
	earliest, latest, ok, err := s.pitr().RecoveryWindow(ctx, s.target(e))
	if err != nil || !ok {
		return earliest, latest, ok, err
	}
	if spec, on := s.pitrSpec(e.Instance()); on {
		if start := s.retentionStart(spec); start.After(earliest) {
			earliest = start
		}
	}
	if earliest.After(latest) {
		earliest = latest
	}
	return earliest, latest, true, nil
}

// sweepRetention prunes each created instance that keeps its log to its
// retention window.
func (s *Server) sweepRetention() {
	p := s.pitr()
	if p == nil {
		return
	}
	type due struct {
		t     DumpTarget
		start time.Time
	}
	var dues []due
	s.emu.RLock()
	if s.writes != nil {
		for name, c := range s.writes.byName {
			if c.spec.PITREnabled() && c.state == "RUNNABLE" {
				dues = append(dues, due{DumpTarget{Instance: name, MySQL: c.spec.MySQL(), Created: true}, s.retentionStart(c.spec)})
			}
		}
	}
	s.emu.RUnlock()
	for _, d := range dues {
		ctx, cancel := context.WithTimeout(context.Background(), engineTimeout)
		_ = p.Prune(ctx, d.t, d.start)
		cancel()
	}
}

// retentionInvalid checks a backupConfiguration's retention settings
// against Cloud SQL's ranges; edition is the instance's, "" when unknown.
func retentionInvalid(bc map[string]any, edition string) error {
	if v, ok := bc["transactionLogRetentionDays"]; ok && v != nil {
		max := 35
		if edition == "ENTERPRISE" {
			max = 7
		}
		if n := intSetting(v); n < 1 || n > max {
			return Errorf(http.StatusBadRequest, "Invalid request: backupConfiguration.transactionLogRetentionDays %v is not "+
				"between 1 and %d (7 on the Enterprise edition, 35 on Enterprise Plus).", v, max)
		}
	}
	rs, _ := bc["backupRetentionSettings"].(map[string]any)
	if u, _ := rs["retentionUnit"].(string); u != "" && u != "COUNT" {
		return Errorf(http.StatusBadRequest, "Invalid request: backupRetentionSettings.retentionUnit %q is not COUNT.", u)
	}
	if v, ok := rs["retainedBackups"]; ok && v != nil {
		if n := intSetting(v); n < 1 || n > 365 {
			return Errorf(http.StatusBadRequest, "Invalid request: backupRetentionSettings.retainedBackups %v is not between 1 and 365.", v)
		}
	}
	return nil
}
