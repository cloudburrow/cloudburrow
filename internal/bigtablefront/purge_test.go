package bigtablefront

import (
	"context"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/bttest"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

// TestResetProjectDropsOnlyThatProject (#1540): ResetPath?project= deletes
// that project's instances and their tables, and leaves another project's.
func TestResetProjectDropsOnlyThatProject(t *testing.T) {
	emu, err := bttest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(emu.Close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	addr, _ := serveKept(t, emu.Addr, t.TempDir())
	opt := option.WithGRPCConn(dial(t, addr))
	admins := map[string]*bigtable.AdminClient{}
	for _, p := range []string{"gone", "kept"} {
		iac, err := bigtable.NewInstanceAdminClient(ctx, p, opt)
		if err != nil {
			t.Fatal(err)
		}
		if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "inst-1", DisplayName: "inst-1",
			ClusterId: "clus-1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
			t.Fatalf("CreateInstance in %s: %v", p, err)
		}
		ac, err := bigtable.NewAdminClient(ctx, p, "inst-1", opt)
		if err != nil {
			t.Fatal(err)
		}
		if err := ac.CreateTable(ctx, "t"); err != nil {
			t.Fatal(err)
		}
		admins[p] = ac
	}

	resp, err := http.Post("http://"+addr+frontstate.ResetPath+"?project=gone", "", nil)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset = %v, %v", resp, err)
	}
	_ = resp.Body.Close()

	for p, want := range map[string]codes.Code{"gone": codes.NotFound, "kept": codes.OK} {
		iac, err := bigtable.NewInstanceAdminClient(ctx, p, opt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := iac.InstanceInfo(ctx, "inst-1"); status.Code(err) != want {
			t.Errorf("InstanceInfo in %s after the reset = %v, want %v", p, err, want)
		}
	}
	// The emulator's tables went with the instance: one created again
	// under the same name starts empty.
	tables, err := admins["kept"].Tables(ctx)
	if err != nil || len(tables) != 1 {
		t.Errorf("the kept project's tables = %v, %v", tables, err)
	}
	conn := dial(t, emu.Addr)
	direct, err := bigtable.NewAdminClient(ctx, "gone", "inst-1", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	if tables, err := direct.Tables(ctx); err != nil || len(tables) != 0 {
		t.Errorf("the purged project's tables in the emulator = %v, %v", tables, err)
	}
}

// TestResetProjectDropsTablesOfInstancesItNeverMade (#1550): a table
// created under an instance ID the instance admin never made, which the
// emulator accepts, goes with its project too, though the front was
// restarted in between, and another project's such table stays.
func TestResetProjectDropsTablesOfInstancesItNeverMade(t *testing.T) {
	emu, err := bttest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(emu.Close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	addr, stop := serveKept(t, emu.Addr, dir)
	for _, p := range []string{"gone", "kept"} {
		opt := option.WithGRPCConn(dial(t, addr))
		ac, err := bigtable.NewAdminClient(ctx, p, "never-made", opt)
		if err != nil {
			t.Fatal(err)
		}
		if err := ac.CreateTable(ctx, "t"); err != nil {
			t.Fatalf("CreateTable in %s: %v", p, err)
		}
		c, err := bigtable.NewClient(ctx, p, "never-made", opt)
		if err != nil {
			t.Fatal(err)
		}
		if err := ac.CreateColumnFamily(ctx, "t", "f"); err != nil {
			t.Fatal(err)
		}
		m := bigtable.NewMutation()
		m.Set("f", "c", bigtable.Now(), []byte("v"))
		if err := c.Open("t").Apply(ctx, "row", m); err != nil {
			t.Fatal(err)
		}
	}
	stop()
	addr, _ = serveKept(t, emu.Addr, dir)

	resp, err := http.Post("http://"+addr+frontstate.ResetPath+"?project=gone", "", nil)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset = %v, %v", resp, err)
	}
	_ = resp.Body.Close()

	conn := dial(t, emu.Addr)
	for p, want := range map[string]int{"gone": 0, "kept": 1} {
		direct, err := bigtable.NewAdminClient(ctx, p, "never-made", option.WithGRPCConn(conn))
		if err != nil {
			t.Fatal(err)
		}
		if tables, err := direct.Tables(ctx); err != nil || len(tables) != want {
			t.Errorf("%s's tables in the emulator after the reset = %v, %v; want %d", p, tables, err, want)
		}
	}
}

func TestSeenInstanceOf(t *testing.T) {
	for in, want := range map[string]string{
		"projects/p/instances/i/tables/t":   "projects/p/instances/i",
		"projects/p/instances/i":            "projects/p/instances/i",
		"projects/p/instances/-":            "",
		"projects/p":                        "",
		"operations/projects/p/instances/i": "",
	} {
		if got := seenInstanceOf(in); got != want {
			t.Errorf("seenInstanceOf(%q) = %q, want %q", in, got, want)
		}
	}
}
