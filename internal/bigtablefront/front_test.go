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
	tbl, cl := inst+"/tables/kept", inst+"/clusters/c"
	backup, bundle := cl+"/backups/b", tbl+"/schemaBundles/s"

	calls := map[string]func() error{
		"CreateBackup": func() error {
			_, err := a.CreateBackup(ctx, &adminpb.CreateBackupRequest{Parent: cl, BackupId: "b", Backup: &adminpb.Backup{SourceTable: tbl}})
			return err
		},
		"CopyBackup": func() error {
			_, err := a.CopyBackup(ctx, &adminpb.CopyBackupRequest{Parent: cl, BackupId: "b2", SourceBackup: backup})
			return err
		},
		"GetBackup": func() error { _, err := a.GetBackup(ctx, &adminpb.GetBackupRequest{Name: backup}); return err },
		"ListBackups": func() error {
			_, err := a.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: cl})
			return err
		},
		"UpdateBackup": func() error {
			_, err := a.UpdateBackup(ctx, &adminpb.UpdateBackupRequest{Backup: &adminpb.Backup{Name: backup}})
			return err
		},
		"DeleteBackup": func() error { _, err := a.DeleteBackup(ctx, &adminpb.DeleteBackupRequest{Name: backup}); return err },
		"RestoreTable": func() error {
			_, err := a.RestoreTable(ctx, &adminpb.RestoreTableRequest{Parent: inst, TableId: "r", Source: &adminpb.RestoreTableRequest_Backup{Backup: backup}})
			return err
		},
		"CreateSchemaBundle": func() error {
			_, err := a.CreateSchemaBundle(ctx, &adminpb.CreateSchemaBundleRequest{Parent: tbl, SchemaBundleId: "s", SchemaBundle: &adminpb.SchemaBundle{}})
			return err
		},
		"GetSchemaBundle": func() error {
			_, err := a.GetSchemaBundle(ctx, &adminpb.GetSchemaBundleRequest{Name: bundle})
			return err
		},
		"ListSchemaBundles": func() error {
			_, err := a.ListSchemaBundles(ctx, &adminpb.ListSchemaBundlesRequest{Parent: tbl})
			return err
		},
		"UpdateSchemaBundle": func() error {
			_, err := a.UpdateSchemaBundle(ctx, &adminpb.UpdateSchemaBundleRequest{SchemaBundle: &adminpb.SchemaBundle{Name: bundle}})
			return err
		},
		"DeleteSchemaBundle": func() error {
			_, err := a.DeleteSchemaBundle(ctx, &adminpb.DeleteSchemaBundleRequest{Name: bundle})
			return err
		},
		"UndeleteTable": func() error {
			_, err := a.UndeleteTable(ctx, &adminpb.UndeleteTableRequest{Name: tbl})
			return err
		},
		"GenerateInitialChangeStreamPartitions": func() error {
			s, err := d.GenerateInitialChangeStreamPartitions(ctx, &bigtablepb.GenerateInitialChangeStreamPartitionsRequest{TableName: tbl})
			if err != nil {
				return err
			}
			_, err = s.Recv()
			return err
		},
		"ReadChangeStream": func() error {
			s, err := d.ReadChangeStream(ctx, &bigtablepb.ReadChangeStreamRequest{TableName: tbl})
			if err != nil {
				return err
			}
			_, err = s.Recv()
			return err
		},
	}
	if len(calls) != len(Refused) {
		t.Fatalf("%d calls for %d refused methods", len(calls), len(Refused))
	}
	for name, call := range calls {
		method := admin + name
		if _, ok := Refused[method]; !ok {
			method = data + name
		}
		want, ok := Refused[method]
		if !ok {
			t.Fatalf("%s is not refused", name)
		}
		err := call()
		if s, _ := status.FromError(err); s.Code() != codes.Unimplemented || s.Message() != want {
			t.Errorf("%s: %v, want Unimplemented %q", name, err, want)
		}
	}
	if _, err := ac.TableInfo(ctx, "kept"); err != nil {
		t.Errorf("the table after the refusals: %v", err)
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
