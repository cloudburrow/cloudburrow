package bigtablefront

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/bigtable/bttest"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

// serveKept serves a front for the emulator at emu that keeps its state in
// dir; stop ends it, as a restart of the front's container does.
func serveKept(t *testing.T, emu, dir string) (addr string, stop func()) {
	t.Helper()
	f, err := New(emu, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := f.KeepState(ctx, dir); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.Serve(ctx, l) }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
		_ = f.Close()
	}
	t.Cleanup(stop)
	return l.Addr().String(), stop
}

// TestFrontKeepsItsStateAcrossARestart (#1514): an instance, a backup, a
// snapshot, a deleted table, an authorized view, a logical view and a
// table's IAM policy, made through one front, are read back through a new
// one on the same state directory, the emulator still running: the backup
// restores, and the deleted table undeletes, with their cells. A reset
// clears them, and the directory with them.
func TestFrontKeepsItsStateAcrossARestart(t *testing.T) {
	emu, err := bttest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(emu.Close)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	addr, stop := serveKept(t, emu.Addr, dir)
	opt := option.WithGRPCConn(dial(t, addr))
	iac, err := bigtable.NewInstanceAdminClient(ctx, "p", opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "kept-i", DisplayName: "kept instance",
		ClusterId: "kept-c", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	ac, err := bigtable.NewAdminClient(ctx, "p", "kept-i", opt)
	if err != nil {
		t.Fatal(err)
	}
	c, err := bigtable.NewClient(ctx, "p", "kept-i", opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"src", "gone"} {
		if err := ac.CreateTable(ctx, tbl); err != nil {
			t.Fatal(err)
		}
		if err := ac.CreateColumnFamily(ctx, tbl, "f"); err != nil {
			t.Fatal(err)
		}
		m := bigtable.NewMutation()
		m.Set("f", "q", bigtable.Timestamp(1000), []byte("v-"+tbl))
		if err := c.Open(tbl).Apply(ctx, "r", m); err != nil {
			t.Fatal(err)
		}
	}
	if err := ac.CreateBackup(ctx, "src", "kept-c", "b1", time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if err := ac.SnapshotTable(ctx, "src", "kept-c", "s1", 0); err != nil {
		t.Fatalf("SnapshotTable: %v", err)
	}
	if err := ac.DeleteTable(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	ta := ac.TableAdminClientV2()
	parent := "projects/p/instances/kept-i"
	if _, err := ta.CreateAuthorizedView(ctx, &adminpb.CreateAuthorizedViewRequest{Parent: parent + "/tables/src", AuthorizedViewId: "av",
		AuthorizedView: &adminpb.AuthorizedView{AuthorizedView: &adminpb.AuthorizedView_SubsetView_{SubsetView: &adminpb.AuthorizedView_SubsetView{RowPrefixes: [][]byte{[]byte("r")}}}}}); err != nil {
		t.Fatalf("CreateAuthorizedView: %v", err)
	}
	if _, err := iac.InstanceAdminClientV2().CreateLogicalView(ctx, &adminpb.CreateLogicalViewRequest{Parent: parent, LogicalViewId: "lv",
		LogicalView: &adminpb.LogicalView{Query: "SELECT _key FROM src"}}); err != nil {
		t.Fatalf("CreateLogicalView: %v", err)
	}
	if _, err := ta.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: parent + "/tables/src", Policy: &iampb.Policy{Bindings: []*iampb.Binding{{Role: "roles/bigtable.reader", Members: []string{"user:a@example.com"}}}}}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	stop()

	addr2, _ := serveKept(t, emu.Addr, dir)
	opt2 := option.WithGRPCConn(dial(t, addr2))
	iac2, err := bigtable.NewInstanceAdminClient(ctx, "p", opt2)
	if err != nil {
		t.Fatal(err)
	}
	if in, err := iac2.InstanceInfo(ctx, "kept-i"); err != nil || in.DisplayName != "kept instance" {
		t.Errorf("InstanceInfo after the restart = %+v, %v", in, err)
	}
	ac2, err := bigtable.NewAdminClient(ctx, "p", "kept-i", opt2)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := bigtable.NewClient(ctx, "p", "kept-i", opt2)
	if err != nil {
		t.Fatal(err)
	}
	if err := ac2.RestoreTable(ctx, "restored", "kept-c", "b1"); err != nil {
		t.Fatalf("RestoreTable after the restart: %v", err)
	}
	if got := rowsOf(t, ctx, c2.Open("restored")); got["r/f:q"] != "v-src" {
		t.Errorf("the restored rows = %v", got)
	}
	if s, err := ac2.SnapshotInfo(ctx, "kept-c", "s1"); err != nil || s.SourceTable != "src" {
		t.Errorf("SnapshotInfo after the restart = %+v, %v", s, err)
	}
	op, err := ac2.TableAdminClientV2().UndeleteTable(ctx, &adminpb.UndeleteTableRequest{Name: parent + "/tables/gone"})
	if err != nil {
		t.Fatalf("UndeleteTable after the restart: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, ctx, c2.Open("gone")); got["r/f:q"] != "v-gone" {
		t.Errorf("the undeleted rows = %v", got)
	}
	ta2 := ac2.TableAdminClientV2()
	if _, err := ta2.GetAuthorizedView(ctx, &adminpb.GetAuthorizedViewRequest{Name: parent + "/tables/src/authorizedViews/av"}); err != nil {
		t.Errorf("GetAuthorizedView after the restart: %v", err)
	}
	if lv, err := iac2.InstanceAdminClientV2().GetLogicalView(ctx, &adminpb.GetLogicalViewRequest{Name: parent + "/logicalViews/lv"}); err != nil || lv.GetQuery() != "SELECT _key FROM src" {
		t.Errorf("GetLogicalView after the restart = %v, %v", lv, err)
	}
	if p, err := ta2.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: parent + "/tables/src"}); err != nil || len(p.GetBindings()) != 1 {
		t.Errorf("GetIamPolicy after the restart = %v, %v", p, err)
	}

	resp, err := http.Post("http://"+addr2+frontstate.ResetPath, "", nil)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset = %v, %v", resp, err)
	}
	_ = resp.Body.Close()
	if _, err := iac2.InstanceInfo(ctx, "kept-i"); status.Code(err) != codes.NotFound {
		t.Errorf("InstanceInfo after the reset = %v, want NotFound", err)
	}
	copies, _ := os.ReadDir(filepath.Join(dir, "copies"))
	if len(copies) != 0 {
		t.Errorf("%d copies left after the reset", len(copies))
	}
}
