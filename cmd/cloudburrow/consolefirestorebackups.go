package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// A database's exports, imports, bulk deletes, backups, restores and
// clones (#1500), through the official Firestore Admin client: Export
// documents, Import documents, Bulk delete documents and Clone database on
// its page, and its Backups tab, with Restore and Delete backup on a
// backup's row.

// firestoreBackupsSegment is the second segment of a backup's action path.
const firestoreBackupsSegment = "backups"

const databaseIDPattern = `^[a-z][a-z0-9\-]{2,61}[a-z0-9]$`

func collectionIDsField(help string) console.Field {
	return console.Field{Name: "collectionIds", Label: "Collection IDs (optional)", Type: "text",
		Help: help + " Comma-separated collection group IDs; empty for every collection."}
}

// firestoreTransferActions are the database page's export, import, bulk delete and
// clone actions.
func firestoreTransferActions() []console.Action {
	return []console.Action{
		{ID: "exportdocuments", Label: "Export", Fields: []console.Field{
			{Name: "outputUriPrefix", Label: "Destination", Type: "text", Required: true, Pattern: `^gs://.+`, Browse: "storage",
				Help: "gs://BUCKET[/PATH] in this instance's Cloud Storage; a bucket alone gets a folder named after the time."},
			collectionIDsField("The collections to export."),
			// Google's "Choose the state of your database to export":
			// "Export from an earlier point in time" and its snapshot time,
			// for a database with PITR (#1845, #1942;
			// https://docs.cloud.google.com/firestore/native/docs/manage-data/export-import,
			// read 2026-11-20).
			{Name: "snapshot", Label: "Export from an earlier point in time", Type: "checkbox", Default: "false",
				Section: "Choose the state of your database to export",
				Help:    "Needs point-in-time recovery, enabled on the Disaster recovery tab."},
			{Name: "snapshotTime", Label: "Snapshot time (UTC)", Type: "datetime-local", Required: true, ShowWhen: "snapshot",
				Section: "Choose the state of your database to export",
				Help:    "A whole minute within the past 7 days, not earlier than the database's earliest version time."},
		}},
		{ID: "importdocuments", Label: "Import", Fields: []console.Field{
			{Name: "inputUriPrefix", Label: "Export location", Type: "text", Required: true, Pattern: `^gs://.+/.+`, Browse: "storage",
				Help: "The gs:// folder of an export (its output URI prefix)."},
			collectionIDsField("The collections to import."),
		}, Confirm: "Imported documents overwrite the documents of the same names."},
		{ID: "bulkdelete", Label: "Bulk delete documents", Destructive: true, Fields: []console.Field{
			collectionIDsField("The collections to delete."),
		}, Confirm: "Every document of the named collection groups, or of the whole database, is deleted."},
		cloneAction(),
	}
}

// cloneAction is Google's Clone and its "Create a clone" dialog
// (https://docs.cloud.google.com/firestore/native/docs/manage-databases,
// read 2026-11-20): the clone's ID, and in Clone from the point in time it
// is cloned from, at the minute: any minute of the past 7 days with
// point-in-time recovery, of the past hour without (#1942).
func cloneAction() console.Action {
	return console.Action{ID: "clonedatabase", Label: "Clone", Fields: []console.Field{
		{Name: "databaseId", Label: "Give the clone an ID", Type: "text", Required: true, Pattern: databaseIDPattern,
			Help: "The new database's ID, which no database of the project has."},
		{Name: "cloneFrom", Label: "Clone from", Type: "datetime-local", Required: true,
			Default: time.Now().UTC().Truncate(time.Minute).Format("2006-01-02T15:04"),
			Help: "A point in time, UTC, at the minute: within the past 7 days with point-in-time recovery, " +
				"the past hour without, and not earlier than the database's earliest version time."},
	}}
}

// pointInTimeMinute reads a form's point in time (an RFC 3339 or
// datetime-local time, read as UTC) as the whole minute it is in, as
// Google's console picks it; empty is the current minute.
func pointInTimeMinute(v, what string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Now().UTC().Truncate(time.Minute), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC().Truncate(time.Minute), nil
		}
	}
	return time.Time{}, fmt.Errorf("the %s must be a date and time, not %q", what, v)
}

func firestoreBackupActions() []console.Action {
	return []console.Action{
		{ID: "restorebackup", Label: "Restore", Fields: []console.Field{
			{Name: "databaseId", Label: "New database ID", Type: "text", Required: true, Pattern: databaseIDPattern},
		}},
		{ID: "deletebackup", Label: "Delete backup", Destructive: true},
	}
}

func collectionIDs(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// databaseBackups are the backups of the database db, in every location.
func databaseBackups(ctx context.Context, c *admin.FirestoreAdminClient, db string) ([]*adminpb.Backup, error) {
	project := strings.SplitN(db, "/databases/", 2)[0]
	resp, err := c.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: project + "/locations/-"})
	if err != nil {
		return nil, err
	}
	var out []*adminpb.Backup
	for _, b := range resp.GetBackups() {
		if b.GetDatabase() == db {
			out = append(out, b)
		}
	}
	return out, nil
}

// backupNamed is db's backup whose ID is id.
func backupNamed(ctx context.Context, c *admin.FirestoreAdminClient, db, id string) (*adminpb.Backup, error) {
	bs, err := databaseBackups(ctx, c, db)
	if err != nil {
		return nil, err
	}
	for _, b := range bs {
		if lastSegment(b.GetName()) == id {
			return b, nil
		}
	}
	return nil, fmt.Errorf("no backup %s of %s", id, db)
}

// backups is a database's Backups tab.
func (p firestoreDatabasesProvider) backups(ctx context.Context, c *admin.FirestoreAdminClient, db, id string) console.Listing {
	list := console.Listing{Columns: []string{"Location", "State", "Snapshot", "Expires", "Documents", "Size"}, NameColumn: "Backup ID", Noun: "backups"}
	bs, err := databaseBackups(ctx, c, db)
	if err != nil {
		list.Unavailable = "listing backups: " + err.Error()
		return list
	}
	for _, b := range bs {
		parts := strings.Split(b.GetName(), "/")
		list.Items = append(list.Items, console.Resource{Name: lastSegment(b.GetName()), Fields: map[string]string{
			"Location":  parts[3],
			"State":     enumWord(b.GetState(), ""),
			"Snapshot":  b.GetSnapshotTime().AsTime().Format("2006-01-02 15:04:05 MST"),
			"Expires":   b.GetExpireTime().AsTime().Format("2006-01-02 15:04:05 MST"),
			"Documents": fmt.Sprint(b.GetStats().GetDocumentCount()),
			"Size":      fmt.Sprintf("%d B", b.GetStats().GetSizeBytes()),
		}, Actions: firestoreBackupActions(), ActsOn: []string{id, firestoreBackupsSegment, lastSegment(b.GetName())}})
	}
	list.Total = len(list.Items)
	list.Note = "Backup schedules take the backups: a schedule takes its first when it is created, then one each day or week. " +
		"A backup is a copy of the documents, kept by the instance until it expires."
	return list
}

// actTransfer performs an export, import, bulk delete, clone or backup
// action on db. ok is false for an action that is not one.
func actTransfer(ctx context.Context, c *admin.FirestoreAdminClient, db string, path []string, action string, values map[string]string) (bool, error) {
	project := strings.SplitN(db, "/databases/", 2)[0]
	switch {
	case action == "exportdocuments" && len(path) == 1:
		req := &adminpb.ExportDocumentsRequest{Name: db,
			OutputUriPrefix: strings.TrimSpace(values["outputUriPrefix"]), CollectionIds: collectionIDs(values["collectionIds"])}
		if values["snapshot"] == "true" {
			if strings.TrimSpace(values["snapshotTime"]) == "" {
				return true, errors.New("a snapshot time is required to export from an earlier point in time")
			}
			at, err := pointInTimeMinute(values["snapshotTime"], "snapshot time")
			if err != nil {
				return true, err
			}
			req.SnapshotTime = timestamppb.New(at)
		}
		op, err := c.ExportDocuments(ctx, req)
		if err == nil {
			_, err = op.Wait(ctx)
		}
		return true, err
	case action == "importdocuments" && len(path) == 1:
		op, err := c.ImportDocuments(ctx, &adminpb.ImportDocumentsRequest{Name: db,
			InputUriPrefix: strings.TrimSpace(values["inputUriPrefix"]), CollectionIds: collectionIDs(values["collectionIds"])})
		if err == nil {
			err = op.Wait(ctx)
		}
		return true, err
	case action == "bulkdelete" && len(path) == 1:
		op, err := c.BulkDeleteDocuments(ctx, &adminpb.BulkDeleteDocumentsRequest{Name: db, CollectionIds: collectionIDs(values["collectionIds"])})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		return true, err
	case action == "clonedatabase" && len(path) == 1:
		// Clone from: the minute picked (#1942).
		at, err := pointInTimeMinute(values["cloneFrom"], "point in time to clone from")
		if err != nil {
			return true, err
		}
		op, err := c.CloneDatabase(ctx, &adminpb.CloneDatabaseRequest{Parent: project, DatabaseId: strings.TrimSpace(values["databaseId"]),
			PitrSnapshot: &adminpb.PitrSnapshot{Database: db, SnapshotTime: timestamppb.New(at)}})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		return true, err
	case len(path) == 3 && path[1] == firestoreBackupsSegment && (action == "restorebackup" || action == "deletebackup"):
		b, err := backupNamed(ctx, c, db, path[2])
		if err != nil {
			return true, err
		}
		if action == "deletebackup" {
			return true, c.DeleteBackup(ctx, &adminpb.DeleteBackupRequest{Name: b.GetName()})
		}
		id := strings.TrimSpace(values["databaseId"])
		if id == "" {
			return true, errors.New("a new database ID is required")
		}
		op, err := c.RestoreDatabase(ctx, &adminpb.RestoreDatabaseRequest{Parent: project, DatabaseId: id, Backup: b.GetName()})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		return true, err
	}
	return false, nil
}
