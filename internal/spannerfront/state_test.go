package spannerfront

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// TestStateRoundTripsThroughItsDirectory (#1514): everything the front
// keeps, a backup's rows among it, is written to its state directory and
// read back by a new front on it, as after a restart of the front's
// container; a reset clears it, and the copies with it.
func TestStateRoundTripsThroughItsDirectory(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newFront := func() *Instances {
		s, err := New("127.0.0.1:1")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.KeepState(ctx, dir, t.Logf); err != nil {
			t.Fatal(err)
		}
		return s
	}
	a := newFront()
	db := "projects/p/instances/i/databases/d"
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	rows := []*structpb.ListValue{{Values: []*structpb.Value{structpb.NewStringValue("1"), structpb.NewStringValue("one")}}}
	pb := &databasepb.BackupSchedule{Name: db + "/backupSchedules/daily", RetentionDuration: durationpb.New(48 * time.Hour),
		Spec: &databasepb.BackupScheduleSpec{ScheduleSpec: &databasepb.BackupScheduleSpec_CronSpec{CronSpec: &databasepb.CrontabSpec{Text: "0 2 * * *"}}}}
	c, err := checkSchedule("test", pb)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.backups["projects/p/instances/i/backups/b"] = &backup{pb: &databasepb.Backup{Name: "projects/p/instances/i/backups/b", Database: db},
		snap: &snapshot{dialect: databasepb.DatabaseDialect_GOOGLE_STANDARD_SQL, ddl: []string{"CREATE TABLE t (k STRING(10)) PRIMARY KEY (k)"}, size: 9,
			tables: []tableRows{{name: "t", columns: []string{"k", "v"}, rows: rows}}}}
	a.restored["projects/p/instances/i/databases/r"] = &databasepb.RestoreInfo{SourceType: databasepb.RestoreSourceType_BACKUP}
	a.schedules[pb.Name] = &schedule{pb: pb, cron: c, last: at}
	a.roleStmts[db] = []string{"CREATE ROLE reader"}
	a.protected[db] = true
	a.policies[db] = &iampolicy.Stored{}
	a.configs["projects/p/instanceConfigs/custom-c"] = &instancepb.InstanceConfig{Name: "projects/p/instanceConfigs/custom-c", DisplayName: "custom"}
	a.changes["projects/p/instances/i"] = &change{inst: &instancepb.Instance{Name: "projects/p/instances/i", DisplayName: "renamed"}, paths: map[string]bool{"display_name": true}, at: at}
	a.ops["projects/p/instances/i/operations/o"] = &longrunningpb.Operation{Name: "projects/p/instances/i/operations/o", Done: true}
	a.opOrder = []string{"projects/p/instances/i/operations/o"}
	a.mu.Unlock()
	if err := a.save(); err != nil {
		t.Fatal(err)
	}

	b := newFront()
	b.mu.Lock()
	got := b.backups["projects/p/instances/i/backups/b"]
	sc := b.schedules[pb.Name]
	b.mu.Unlock()
	if got == nil || got.snap == nil || len(got.snap.tables) != 1 || got.snap.tables[0].name != "t" ||
		!proto.Equal(got.snap.tables[0].rows[0], rows[0]) || got.snap.ddl[0] != "CREATE TABLE t (k STRING(10)) PRIMARY KEY (k)" || got.snap.size != 9 {
		t.Fatalf("the backup after the restart = %+v", got)
	}
	if sc == nil || !sc.last.Equal(at) || sc.cron.Next(at).IsZero() {
		t.Errorf("the schedule after the restart = %+v", sc)
	}
	if !b.protected[db] || b.roleStmts[db][0] != "CREATE ROLE reader" || b.configs["projects/p/instanceConfigs/custom-c"].GetDisplayName() != "custom" ||
		b.changes["projects/p/instances/i"].inst.GetDisplayName() != "renamed" || !b.ops[b.opOrder[0]].GetDone() || b.policies[db] == nil ||
		b.restored["projects/p/instances/i/databases/r"] == nil {
		t.Errorf("the records after the restart: protected %v, roles %v, configs %v, changes %v, ops %v, policies %v, restored %v",
			b.protected, b.roleStmts, b.configs, b.changes, b.ops, b.policies, b.restored)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, frontstate.ResetPath, nil)
	if !b.Claims(req) {
		t.Fatal("the front does not claim the reset path")
	}
	b.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "copies")); len(left) != 0 {
		t.Errorf("%d copies left after the reset", len(left))
	}
	if c := newFront(); len(c.backups) != 0 || len(c.schedules) != 0 || len(c.configs) != 0 {
		t.Errorf("after the reset a new front holds %d backups, %d schedules, %d configs", len(c.backups), len(c.schedules), len(c.configs))
	}
}
