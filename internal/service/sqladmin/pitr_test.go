package sqladmin

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// pitrFake is a fakeDumper that keeps bases, by the time each was taken.
type pitrFake struct {
	*fakeDumper
	mu     sync.Mutex
	bases  []time.Time
	failed error
}

func (f *pitrFake) PITRBase(context.Context, DumpTarget) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed != nil {
		return f.failed
	}
	f.bases = append(f.bases, time.Now())
	return nil
}

func (f *pitrFake) RecoveryWindow(context.Context, DumpTarget) (time.Time, time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bases) == 0 {
		return time.Time{}, time.Time{}, false, nil
	}
	return f.bases[0], time.Now(), true, nil
}

func (f *pitrFake) Prune(_ context.Context, _ DumpTarget, before time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	keep := 0
	for i, b := range f.bases {
		if !b.After(before) {
			keep = i
		}
	}
	f.bases = f.bases[keep:]
	return nil
}

func (f *pitrFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bases)
}

// TestPITRRetentionAndFailedBase (#1330): the retention settings are
// checked against Cloud SQL's ranges and returned; a backup whose base
// fails is FAILED with the error; advancing the retention clock past
// transactionLogRetentionDays prunes the older bases and moves the
// earliest recovery time forward.
func TestPITRRetentionAndFailedBase(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer("", newFake())
	srv.EnableInstanceWrites(newFakeProv(), filepath.Join(dir, "instances.json"))
	d := &pitrFake{fakeDumper: &fakeDumper{data: map[string]string{}}}
	if err := srv.EnableBackups(d, filepath.Join(dir, "backups"), false); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	insert := func(name, edition string, bc *sqladmin.BackupConfiguration) (*sqladmin.Operation, error) {
		return svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17",
			Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", Edition: edition, BackupConfiguration: bc}}).Do()
	}
	for _, bad := range []struct {
		edition string
		bc      *sqladmin.BackupConfiguration
	}{
		{"ENTERPRISE", &sqladmin.BackupConfiguration{TransactionLogRetentionDays: 8}},
		{"ENTERPRISE_PLUS", &sqladmin.BackupConfiguration{TransactionLogRetentionDays: 36}},
		{"", &sqladmin.BackupConfiguration{BackupRetentionSettings: &sqladmin.BackupRetentionSettings{RetainedBackups: 366}}},
		{"", &sqladmin.BackupConfiguration{BackupRetentionSettings: &sqladmin.BackupRetentionSettings{RetentionUnit: "DAYS", RetainedBackups: 3}}},
	} {
		if _, err := insert("bad", bad.edition, bad.bc); err == nil || !strings.Contains(err.Error(), "400") {
			t.Errorf("insert with %+v: %v, want 400", bad.bc, err)
		}
	}

	op, err := insert("pit", "ENTERPRISE", &sqladmin.BackupConfiguration{Enabled: true, PointInTimeRecoveryEnabled: true,
		TransactionLogRetentionDays: 1, BackupRetentionSettings: &sqladmin.BackupRetentionSettings{RetentionUnit: "COUNT", RetainedBackups: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil {
		t.Fatalf("insert: %+v", got.Error)
	}
	in, err := svc.Instances.Get("proj", "pit").Do()
	if err != nil || in.Settings.BackupConfiguration.TransactionLogRetentionDays != 1 ||
		in.Settings.BackupConfiguration.BackupRetentionSettings.RetainedBackups != 2 {
		t.Fatalf("get: %v %+v", err, in.Settings.BackupConfiguration)
	}
	if d.count() != 1 {
		t.Fatalf("bases after create: %d, want 1", d.count())
	}
	first, err := svc.Projects.Instances.GetLatestRecoveryTime("proj", "pit").Do()
	if err != nil {
		t.Fatal(err)
	}

	// A base that fails fails the backup.
	d.mu.Lock()
	d.failed = errors.New("pg_basebackup: disk full")
	d.mu.Unlock()
	op, err = svc.BackupRuns.Insert("proj", "pit", &sqladmin.BackupRun{}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error == nil {
		t.Errorf("the backup's operation succeeded with a failed base")
	}
	runs, err := svc.BackupRuns.List("proj", "pit").Do()
	if err != nil || len(runs.Items) != 1 || runs.Items[0].Status != "FAILED" || runs.Items[0].Error == nil ||
		!strings.Contains(runs.Items[0].Error.Message, "disk full") {
		t.Fatalf("the backup run with a failed base: %v %+v", err, runs)
	}
	d.mu.Lock()
	d.failed = nil
	d.mu.Unlock()
	op, _ = svc.BackupRuns.Insert("proj", "pit", &sqladmin.BackupRun{}).Do()
	if got := wait(t, svc, "proj", op); got.Error != nil || d.count() != 2 {
		t.Fatalf("backup: %+v, bases %d", got.Error, d.count())
	}

	// A day later, only the newest base is kept, and the window starts a
	// day before the clock.
	time.Sleep(10 * time.Millisecond)
	res, err := http.Post(ts.URL+ClockPath, "application/json", bytes.NewBufferString(`{"duration":"86400s"}`))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("advance the clock: %v %v", err, res)
	}
	res.Body.Close()
	if d.count() != 1 {
		t.Errorf("bases after a day: %d, want 1", d.count())
	}
	win, err := svc.Projects.Instances.GetLatestRecoveryTime("proj", "pit").Do()
	if err != nil {
		t.Fatal(err)
	}
	e0, _ := time.Parse(time.RFC3339Nano, first.EarliestRecoveryTime)
	e1, _ := time.Parse(time.RFC3339Nano, win.EarliestRecoveryTime)
	l1, _ := time.Parse(time.RFC3339Nano, win.LatestRecoveryTime)
	if !e1.After(e0) || e1.After(l1) {
		t.Errorf("window after a day: %s to %s, earliest was %s", e1, l1, e0)
	}
	if _, err := svc.Instances.Clone("proj", "pit", &sqladmin.InstancesCloneRequest{CloneContext: &sqladmin.CloneContext{
		DestinationInstanceName: "pit-old", PointInTime: e0.Format(time.RFC3339Nano)}}).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("a clone to a time past retention: %v, want 400", err)
	}
	res, _ = http.Post(ts.URL+ClockPath, "application/json", bytes.NewBufferString(`{"duration":"-1s"}`))
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("moving the clock back: %d, want 400", res.StatusCode)
	}
	res.Body.Close()

	// High availability stays 501, with why.
	if _, err := svc.Instances.Failover("proj", "pit", &sqladmin.InstancesFailoverRequest{}).Do(); err == nil ||
		!strings.Contains(err.Error(), "501") || !strings.Contains(err.Error(), "synchronously") {
		t.Errorf("failover: %v, want 501 saying why", err)
	}
	if _, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "ha", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", AvailabilityType: "REGIONAL"}}).Do(); err == nil ||
		!strings.Contains(err.Error(), "501") || !strings.Contains(err.Error(), "second zone") {
		t.Errorf("REGIONAL: %v, want 501 saying why", err)
	}
}

func TestRetentionFloor(t *testing.T) {
	s := NewServer("")
	day := 24 * time.Hour
	if f := s.retentionFloor(7 * day); time.Since(f) < 7*day-time.Minute || time.Since(f) > 7*day+time.Minute {
		t.Errorf("floor with no advance: %s ago, want 7 days", time.Since(f))
	}
	before := time.Now()
	if _, err := s.Advance(day); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	// A day's retention: what was there at the advance is a day old.
	if f := s.retentionFloor(day); f.Before(before) || f.After(after) {
		t.Errorf("one day's floor after a day's advance: %s, want the advance (%s to %s)", f, before, after)
	}
	// Seven days': six more days of the past are kept.
	if d := before.Sub(s.retentionFloor(7 * day)); d < 6*day-time.Minute || d > 6*day+time.Minute {
		t.Errorf("seven days' floor after a day's advance: %s before the advance, want 6 days", d)
	}
	if got := s.Clock(); got.Sub(time.Now()) < day-time.Minute {
		t.Errorf("Clock: %s, want a day ahead", got)
	}
}
