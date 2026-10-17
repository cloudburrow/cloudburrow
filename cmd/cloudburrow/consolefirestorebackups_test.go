package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/callfront"
	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/fstest"
)

// TestConsoleFirestoreTransferAndBackups (#1500): Export documents, Bulk
// delete documents, Import documents and Clone database on a database's
// page, and the Backups tab with Restore and Delete backup on a backup's
// row, against the call front's Admin API in front of an in-memory
// emulator and Cloud Storage.
func TestConsoleFirestoreTransferAndBackups(t *testing.T) {
	docs, docsAddr, gcs, gcsURL := fstest.Serve(t)
	a := firestoreadmin.New(docsAddr)
	a.SetStorage(gcsURL)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := callfront.Server(callfront.HandlerWith(docsAddr, &calllog.Ring{}, t.Logf, a), t.Logf)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	p := firestoreDatabasesProvider{endpoint: l.Addr().String()}
	ctx := context.Background()
	db := "projects/p/databases/(default)"
	docs.Seed(db)

	want := map[string]bool{"exportdocuments": true, "importdocuments": true, "bulkdelete": true, "clonedatabase": true}
	for _, act := range p.DetailActions(ctx, "p", []string{"(default)"}) {
		delete(want, act.ID)
	}
	if len(want) != 0 {
		t.Fatalf("a database's page lacks %v", want)
	}
	at := []string{"(default)"}
	if err := p.ActAt(ctx, "p", at, "exportdocuments", map[string]string{"outputUriPrefix": "gs://exports/console"}); err != nil {
		t.Fatalf("Export documents: %v", err)
	}
	if _, ok := gcs.Objects["console/console.overall_export_metadata"]; !ok {
		t.Errorf("the export wrote no overall metadata: %v", gcs.Objects)
	}
	if err := p.ActAt(ctx, "p", at, "bulkdelete", map[string]string{"collectionIds": "cities, users"}); err != nil {
		t.Fatalf("Bulk delete documents: %v", err)
	}
	if got := strings.Join(docs.Names(db), ","); got != "cities/sf/landmarks/bridge,orphans/missing/kids/k1" {
		t.Errorf("after Bulk delete: %s", got)
	}
	if err := p.ActAt(ctx, "p", at, "importdocuments", map[string]string{"inputUriPrefix": "gs://exports/console"}); err != nil {
		t.Fatalf("Import documents: %v", err)
	}
	if n := len(docs.Names(db)); n != 5 {
		t.Errorf("after Import: %d documents, want 5", n)
	}
	if err := p.ActAt(ctx, "p", at, "clonedatabase", map[string]string{"databaseId": "copy"}); err != nil {
		t.Fatalf("Clone database: %v", err)
	}
	if n := len(docs.Names("projects/p/databases/copy")); n != 5 {
		t.Errorf("the clone has %d documents, want 5", n)
	}

	c, err := p.client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db, BackupSchedule: &adminpb.BackupSchedule{
		Retention: durationpb.New(24 * time.Hour), Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}}}); err != nil {
		t.Fatal(err)
	}
	d, err := p.Detail(ctx, "p", at)
	if err != nil || len(d.Sections) != 4 || d.Sections[3].ID != "backups" || len(d.Sections[3].Listing.Items) != 1 {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	row := d.Sections[3].Listing.Items[0]
	if row.Fields["State"] != "Ready" || row.Fields["Documents"] != "5" || row.Fields["Location"] != "nam5" {
		t.Errorf("the backup row = %+v", row.Fields)
	}
	if acts := p.DetailActions(ctx, "p", row.ActsOn); len(acts) != 2 {
		t.Errorf("a backup's actions = %+v", acts)
	}
	if err := p.ActAt(ctx, "p", row.ActsOn, "restorebackup", map[string]string{"databaseId": "restored"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n := len(docs.Names("projects/p/databases/restored")); n != 5 {
		t.Errorf("the restored database has %d documents, want 5", n)
	}
	if err := p.ActAt(ctx, "p", row.ActsOn, "deletebackup", nil); err != nil {
		t.Fatalf("Delete backup: %v", err)
	}
	if d, _ := p.Detail(ctx, "p", at); len(d.Sections[3].Listing.Items) != 0 {
		t.Errorf("backups after Delete backup = %+v", d.Sections[3].Listing.Items)
	}
}
