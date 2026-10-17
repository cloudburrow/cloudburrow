//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get
//
// TestBqCopySnapshotCloneAndRestore (#1002): bq cp --snapshot --expiration,
// bq cp --clone and bq cp --restore, through the real bq CLI (the harness's
// isolated configuration, behind egressGuard), send copy jobs with
// operationType SNAPSHOT, CLONE and RESTORE (and destinationExpirationTime),
// and the official Go client reads each result back: a SNAPSHOT of the base
// with its snapshotDefinition and an expiration about an hour away, a
// writable clone with its cloneDefinition, and a standard table restored from
// the snapshot with its rows.
func TestBqCopySnapshotCloneAndRestore(t *testing.T) {
	h := New(t)
	endpoint := strings.TrimSuffix(h.Endpoint(EnvBigQuery), "/")
	c, project := bigqueryClient(t, h)
	b := newBqSession(t, nil, "--api", endpoint, "--project_id", project, bqToken)
	ctx := h.Context()
	// bq cp reads the dataset's location before it copies.
	ds := c.Dataset(strings.ReplaceAll(h.Project(), "-", "_") + "_bqcp")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(ctx) })
	d := ds.DatasetID
	if err := ds.Table("base").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "INSERT INTO `"+d+".base` VALUES (1), (2), (3)", false); err != nil {
		t.Fatal(err)
	}

	b.must("cp", "--snapshot", "--expiration=3600", d+".base", d+".snap")
	md, err := ds.Table("snap").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.Type != bigquery.Snapshot || md.SnapshotDefinition == nil || md.SnapshotDefinition.BaseTableReference.TableID != "base" {
		t.Errorf("bq cp --snapshot made type %q, %+v", md.Type, md.SnapshotDefinition)
	}
	if want := advanceBigQueryClock(t, h, 0).Add(time.Hour); md.ExpirationTime.Before(want.Add(-5*time.Minute)) || md.ExpirationTime.After(want.Add(5*time.Minute)) {
		t.Errorf("bq cp --expiration=3600: the snapshot expires %v, want about %v", md.ExpirationTime, want)
	}

	b.must("cp", "--clone", d+".base", d+".cl")
	if md, err := ds.Table("cl").Metadata(ctx); err != nil || md.Type != bigquery.RegularTable || md.CloneDefinition == nil {
		t.Errorf("bq cp --clone made %+v, %v", md, err)
	}
	if err := bqRun(ctx, c, "INSERT INTO `"+d+".cl` VALUES (4)", false); err != nil {
		t.Errorf("INSERT into the clone: %v", err)
	}

	b.must("cp", "--restore", d+".snap", d+".restored")
	if md, err := ds.Table("restored").Metadata(ctx); err != nil || md.Type != bigquery.RegularTable || md.SnapshotDefinition != nil {
		t.Errorf("bq cp --restore made %+v, %v", md, err)
	}
	for table, want := range map[string]string{"restored": "[[3]]", "cl": "[[4]]", "base": "[[3]]"} {
		if got := fmt.Sprint(rows(t, ctx, c, "SELECT COUNT(*) FROM `"+d+"."+table+"`")); got != want {
			t.Errorf("%s holds %s rows, want %s", table, got, want)
		}
	}
}
