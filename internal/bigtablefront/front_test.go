package bigtablefront

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"cloud.google.com/go/bigtable/bttest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/frontready"
)

// serve starts Google's emulator in process with the front before it, and
// returns the front's address.
func serve(t *testing.T) string {
	t.Helper()
	emu, err := bttest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(emu.Close)
	f, err := New(emu.Addr, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return l.Addr().String()
}

func dial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestFrontForwardsWhatTheEmulatorServes drives tables and rows through the
// official clients, unary and streaming calls alike, through the front.
func TestFrontForwardsWhatTheEmulatorServes(t *testing.T) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opt := option.WithGRPCConn(dial(t, addr))
	ac, err := bigtable.NewAdminClient(ctx, "p", "i", opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "t"); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if err := ac.CreateColumnFamily(ctx, "t", "f"); err != nil {
		t.Fatalf("CreateColumnFamily: %v", err)
	}
	c, err := bigtable.NewClient(ctx, "p", "i", option.WithGRPCConn(dial(t, addr)))
	if err != nil {
		t.Fatal(err)
	}
	tbl := c.Open("t")
	keys := []string{"a", "b", "c"}
	muts := make([]*bigtable.Mutation, len(keys))
	for i := range keys {
		muts[i] = bigtable.NewMutation()
		muts[i].Set("f", "q", bigtable.Now(), []byte("v"+keys[i]))
	}
	if errs, err := tbl.ApplyBulk(ctx, keys, muts); err != nil || errs != nil {
		t.Fatalf("ApplyBulk (MutateRows): %v %v", err, errs)
	}
	var got []string
	if err := tbl.ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		got = append(got, r.Key()+"="+string(r["f"][0].Value))
		return true
	}); err != nil {
		t.Fatalf("ReadRows: %v", err)
	}
	if strings.Join(got, ",") != "a=va,b=vb,c=vc" {
		t.Errorf("ReadRows = %v", got)
	}
	tables, err := ac.Tables(ctx)
	if err != nil || len(tables) != 1 || tables[0] != "t" {
		t.Errorf("Tables = %v, %v", tables, err)
	}
	// An error of the emulator's reaches the client as it was sent.
	if _, err := ac.TableInfo(ctx, "missing"); status.Code(err) != codes.NotFound {
		t.Errorf("TableInfo of a missing table: %v, want NotFound", err)
	}
}

// TestFrontRefusesWhatWouldCrashTheEmulator calls each Refused method: the
// front answers UNIMPLEMENTED, and the emulator, which would panic on any
// of them (taking this test's process with it), still has its table.
func TestFrontRefusesWhatWouldCrashTheEmulator(t *testing.T) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := dial(t, addr)
	ac, err := bigtable.NewAdminClient(ctx, "p", "i", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "kept"); err != nil {
		t.Fatal(err)
	}
	a := adminpb.NewBigtableTableAdminClient(conn)
	d := bigtablepb.NewBigtableClient(conn)
	inst := "projects/p/instances/i"
	tbl := inst + "/tables/kept"

	// The change stream reads, the last Refused methods, are served
	// since #1523 (changestreams_test.go).
	if len(Refused) != 0 {
		t.Errorf("Refused = %v, want none", Refused)
	}
	_, _ = d, tbl
	if _, err := ac.TableInfo(ctx, "kept"); err != nil {
		t.Errorf("the table after the refusals: %v", err)
	}

	// UndeleteTable (#1511) brings back a table deleted through the front,
	// with its families and cells, and refuses one never deleted.
	if _, err := a.UndeleteTable(ctx, &adminpb.UndeleteTableRequest{Name: tbl}); status.Code(err) != codes.NotFound {
		t.Errorf("UndeleteTable of a live table = %v, want NotFound", err)
	}
	if err := ac.CreateColumnFamily(ctx, "kept", "cf"); err != nil {
		t.Fatal(err)
	}
	c, err := bigtable.NewClient(ctx, "p", "i", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	m := bigtable.NewMutation()
	m.Set("cf", "q", bigtable.Timestamp(5000), []byte("v"))
	if err := c.Open("kept").Apply(ctx, "r1", m); err != nil {
		t.Fatal(err)
	}
	if err := ac.DeleteTable(ctx, "kept"); err != nil {
		t.Fatal(err)
	}
	op, err := ac.TableAdminClientV2().UndeleteTable(ctx, &adminpb.UndeleteTableRequest{Name: tbl})
	if err != nil {
		t.Fatalf("UndeleteTable: %v", err)
	}
	if got, err := op.Wait(ctx); err != nil || got.GetName() != tbl {
		t.Fatalf("UndeleteTable.Wait = %v, %v", got, err)
	}
	r, err := c.Open("kept").ReadRow(ctx, "r1")
	if err != nil || len(r["cf"]) != 1 || string(r["cf"][0].Value) != "v" || r["cf"][0].Timestamp != 5000 {
		t.Errorf("the undeleted row = %v, %v", r, err)
	}
	if _, err := a.UndeleteTable(ctx, &adminpb.UndeleteTableRequest{Name: tbl}); status.Code(err) != codes.NotFound {
		t.Errorf("UndeleteTable twice = %v, want NotFound", err)
	}
}

// TestFrontAnswersTheEmulatorsReadiness serves frontready.Path, over
// HTTP/1.1 as the kubelet asks, on the gRPC port.
func TestFrontAnswersTheEmulatorsReadiness(t *testing.T) {
	addr := serve(t)
	resp, err := http.Get("http://" + addr + frontready.Path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("readiness: %d %s", resp.StatusCode, body)
	}
}

// #1587: the REST admin API and the reset are HTTP/1.1, and the host's
// tunnel to this port is raw, so the front checks Host and Origin itself.
func TestFrontRefusesForeignHostAndOrigin(t *testing.T) {
	addr := serve(t)
	for _, c := range []struct {
		method, host, origin string
		want                 int
	}{
		{http.MethodGet, "attacker.example:8086", "", http.StatusMisdirectedRequest},
		{http.MethodPost, "127.0.0.1:8086", "https://evil.example", http.StatusForbidden},
		{http.MethodGet, "bigtable.cloudburrow.svc.cluster.local:8086", "https://evil.example", http.StatusForbidden},
		{http.MethodGet, "bigtable.cloudburrow.svc.cluster.local:8086", "", http.StatusOK},
		{http.MethodGet, "127.0.0.1:8086", "", http.StatusOK},
	} {
		req, err := http.NewRequest(c.method, "http://"+addr+"/v2/projects/p/instances/i/tables", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s Host %s, Origin %q: status %d, want %d", c.method, c.host, c.origin, resp.StatusCode, c.want)
		}
	}
}
