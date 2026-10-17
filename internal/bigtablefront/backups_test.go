package bigtablefront

import (
	"context"
	"sort"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func rowsOf(t *testing.T, ctx context.Context, tbl bigtable.TableAPI) map[string]string {
	t.Helper()
	got := map[string]string{}
	if err := tbl.ReadRows(ctx, bigtable.InfiniteRange(""), func(r bigtable.Row) bool {
		for _, items := range r {
			for _, it := range items {
				got[r.Key()+"/"+it.Column] = string(it.Value)
			}
		}
		return true
	}); err != nil {
		t.Fatalf("ReadRows: %v", err)
	}
	return got
}

// TestBackupsAndSnapshots backs a table up, restores and copies the
// backup, and snapshots the table, through the official client: the rows
// come back in the new tables.
func TestBackupsAndSnapshots(t *testing.T) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opt := option.WithGRPCConn(dial(t, addr))
	ac, err := bigtable.NewAdminClient(ctx, "p", "i", opt)
	if err != nil {
		t.Fatal(err)
	}
	c, err := bigtable.NewClient(ctx, "p", "i", opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "src"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateColumnFamily(ctx, "src", "f"); err != nil {
		t.Fatal(err)
	}
	tbl := c.Open("src")
	for _, k := range []string{"a", "b"} {
		m := bigtable.NewMutation()
		m.Set("f", "q", bigtable.Timestamp(1000), []byte("v-"+k))
		if err := tbl.Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}
	want := rowsOf(t, ctx, tbl)

	expire := time.Now().Add(48 * time.Hour)
	if err := ac.CreateBackup(ctx, "src", "c", "b1", expire); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if err := ac.CreateBackup(ctx, "src", "c", "b1", expire); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second CreateBackup = %v, want AlreadyExists", err)
	}
	if err := ac.CreateBackup(ctx, "src", "c", "soon", time.Now().Add(time.Hour)); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an hour's expiry = %v, want InvalidArgument", err)
	}
	info, err := ac.BackupInfo(ctx, "c", "b1")
	if err != nil || info.SourceTable != "src" || info.State != "READY" || info.SizeBytes == 0 {
		t.Errorf("BackupInfo = %+v, %v", info, err)
	}
	if err := ac.UpdateBackup(ctx, "c", "b1", expire.Add(time.Hour)); err != nil {
		t.Errorf("UpdateBackup: %v", err)
	}
	if err := ac.RestoreTable(ctx, "restored", "c", "b1"); err != nil {
		t.Fatalf("RestoreTable: %v", err)
	}
	if got := rowsOf(t, ctx, c.Open("restored")); len(got) != 2 || got["a/f:q"] != want["a/f:q"] || got["b/f:q"] != "v-b" {
		t.Errorf("the restored rows = %v, want %v", got, want)
	}
	if err := ac.CopyBackup(ctx, "c", "b1", "p", "i", "c2", "b2", expire); err != nil {
		t.Fatalf("CopyBackup: %v", err)
	}
	var names []string
	it := ac.Backups(ctx, "-")
	for {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("Backups: %v", err)
		}
		names = append(names, b.Name)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "b1" || names[1] != "b2" {
		t.Errorf("Backups(-) = %v", names)
	}
	if err := ac.DeleteTable(ctx, "src"); err != nil {
		t.Fatal(err)
	}
	// The backup outlives its table.
	if err := ac.RestoreTable(ctx, "again", "c2", "b2"); err != nil {
		t.Fatalf("RestoreTable from the copy: %v", err)
	}
	if got := rowsOf(t, ctx, c.Open("again")); len(got) != 2 {
		t.Errorf("rows restored from the copy = %v", got)
	}
	if err := ac.DeleteBackup(ctx, "c", "b1"); err != nil {
		t.Errorf("DeleteBackup: %v", err)
	}
	if _, err := ac.BackupInfo(ctx, "c", "b1"); status.Code(err) != codes.NotFound {
		t.Errorf("BackupInfo after delete = %v, want NotFound", err)
	}

	if err := ac.SnapshotTable(ctx, "restored", "c", "s1", 0); err != nil {
		t.Fatalf("SnapshotTable: %v", err)
	}
	if s, err := ac.SnapshotInfo(ctx, "c", "s1"); err != nil || s.SourceTable != "restored" {
		t.Errorf("SnapshotInfo = %+v, %v", s, err)
	}
	if err := ac.CreateTableFromSnapshot(ctx, "fromsnap", "c", "s1"); err != nil {
		t.Fatalf("CreateTableFromSnapshot: %v", err)
	}
	if got := rowsOf(t, ctx, c.Open("fromsnap")); len(got) != 2 {
		t.Errorf("rows from the snapshot = %v", got)
	}
	sit := ac.Snapshots(ctx, "c")
	if s, err := sit.Next(); err != nil || s.Name != "s1" {
		t.Errorf("Snapshots = %+v, %v", s, err)
	}
	if err := ac.DeleteSnapshot(ctx, "c", "s1"); err != nil {
		t.Errorf("DeleteSnapshot: %v", err)
	}
}

// TestAuthorizedViews manages a view and reads and writes through it: only
// the view's rows and columns are seen, and a write outside it is refused.
func TestAuthorizedViews(t *testing.T) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opt := option.WithGRPCConn(dial(t, addr))
	ac, err := bigtable.NewAdminClient(ctx, "p", "i", opt)
	if err != nil {
		t.Fatal(err)
	}
	c, err := bigtable.NewClient(ctx, "p", "i", opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"pub", "priv"} {
		if err := ac.CreateColumnFamily(ctx, "t", f); err != nil {
			t.Fatal(err)
		}
	}
	tbl := c.Open("t")
	for _, k := range []string{"user#1", "user#2", "admin#1"} {
		m := bigtable.NewMutation()
		m.Set("pub", "name", bigtable.Now(), []byte(k))
		m.Set("pub", "secret", bigtable.Now(), []byte("x"))
		m.Set("priv", "ssn", bigtable.Now(), []byte("y"))
		if err := tbl.Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}
	sv := &bigtable.SubsetViewConf{}
	sv.AddRowPrefix([]byte("user#"))
	sv.AddFamilySubsetQualifier("pub", []byte("name"))
	if err := ac.CreateAuthorizedView(ctx, &bigtable.AuthorizedViewConf{TableID: "t", AuthorizedViewID: "users", AuthorizedView: sv}); err != nil {
		t.Fatalf("CreateAuthorizedView: %v", err)
	}
	if names, err := ac.AuthorizedViews(ctx, "t"); err != nil || len(names) != 1 || names[0] != "users" {
		t.Errorf("AuthorizedViews = %v, %v", names, err)
	}
	info, err := ac.AuthorizedViewInfo(ctx, "t", "users")
	if err != nil {
		t.Fatalf("AuthorizedViewInfo: %v", err)
	}
	if got := info.AuthorizedView.(*bigtable.SubsetViewInfo); len(got.RowPrefixes) != 1 {
		t.Errorf("AuthorizedViewInfo = %+v", got)
	}
	view := c.OpenAuthorizedView("t", "users")
	got := rowsOf(t, ctx, view)
	if len(got) != 2 || got["user#1/pub:name"] != "user#1" || got["user#2/pub:name"] != "user#2" {
		t.Errorf("rows through the view = %v", got)
	}
	m := bigtable.NewMutation()
	m.Set("pub", "name", bigtable.Now(), []byte("new"))
	if err := view.Apply(ctx, "user#3", m); err != nil {
		t.Errorf("a write inside the view: %v", err)
	}
	if err := view.Apply(ctx, "admin#2", m); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a write outside the view's rows = %v, want PermissionDenied", err)
	}
	bad := bigtable.NewMutation()
	bad.Set("priv", "ssn", bigtable.Now(), []byte("z"))
	if err := view.Apply(ctx, "user#1", bad); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a write outside the view's columns = %v, want PermissionDenied", err)
	}

	sv2 := &bigtable.SubsetViewConf{}
	sv2.AddRowPrefix([]byte(""))
	sv2.AddFamilySubsetQualifierPrefix("priv", []byte(""))
	if err := ac.UpdateAuthorizedView(ctx, bigtable.UpdateAuthorizedViewConf{AuthorizedViewConf: bigtable.AuthorizedViewConf{
		TableID: "t", AuthorizedViewID: "users", AuthorizedView: sv2, DeletionProtection: bigtable.Protected}}); err != nil {
		t.Fatalf("UpdateAuthorizedView: %v", err)
	}
	if got := rowsOf(t, ctx, view); len(got) != 3 || got["admin#1/priv:ssn"] != "y" {
		t.Errorf("rows through the updated view = %v", got)
	}
	if err := ac.DeleteAuthorizedView(ctx, "t", "users"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("deleting a protected view = %v, want FailedPrecondition", err)
	}
	if err := ac.UpdateAuthorizedView(ctx, bigtable.UpdateAuthorizedViewConf{AuthorizedViewConf: bigtable.AuthorizedViewConf{
		TableID: "t", AuthorizedViewID: "users", DeletionProtection: bigtable.Unprotected}}); err != nil {
		t.Fatalf("UpdateAuthorizedView: %v", err)
	}
	if err := ac.DeleteAuthorizedView(ctx, "t", "users"); err != nil {
		t.Errorf("DeleteAuthorizedView: %v", err)
	}
	if _, err := ac.AuthorizedViewInfo(ctx, "t", "users"); status.Code(err) != codes.NotFound {
		t.Errorf("AuthorizedViewInfo after delete = %v, want NotFound", err)
	}
}
