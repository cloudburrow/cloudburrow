//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/datastore"
	"cloud.google.com/go/spanner"
)

// TestPythonDataflowDatabases (#2215): Google's Datastore, Bigtable and
// Spanner connectors in a Python pipeline (testdata/dataflowpy/databases.py)
// write to the instance's emulators: the Python SDK's DataflowRunner
// submits it, it runs on the local runner, and the Go clients read each
// count back from Datastore (entity Word/kiwi), Bigtable (row kiwi, cf:n)
// and Spanner (table Words).
func TestPythonDataflowDatabases(t *testing.T) {
	h, _, c := dataflowEnv(t)
	storageAddr := h.Endpoint(EnvStorage)
	dfAddr := h.Endpoint(EnvDataflow)
	h.Endpoint(EnvDatastore)
	spannerAddr := h.Endpoint(EnvSpanner)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-db-%s", project, suffix)
	dataflowBucket(t, ctx, storageClient(t, h), project, bucket)

	table := "dfwords" + suffix
	_, bt := bigtableTable(t, h, table)
	admin, err := bigtable.NewAdminClient(ctx, project, bigtableInstance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.CreateColumnFamily(ctx, table, "cf"); err != nil {
		t.Fatalf("CreateColumnFamily: %v", err)
	}
	spInstance, spDB := "dataflow-"+suffix, "words"
	db := spannerDatabase(t, ctx, spannerAddr, project, spInstance, spDB, "CREATE TABLE Words (Word STRING(64) NOT NULL, N INT64) PRIMARY KEY (Word)")
	ds := datastoreClient(t, h, project)

	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 80: dfAddr})
	out := runBeamPython(t, ctx, network, project, append([]string{"/flow/databases.py", "--db_project=" + project,
		"--bigtable_instance=" + bigtableInstance, "--bigtable_table=" + table,
		"--spanner_instance=" + spInstance, "--spanner_database=" + spDB},
		dataflowRunnerArgs(project, bucket, "databases-"+suffix)...)...)
	id := jobIDOf(t, out)
	if j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED); j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("the job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
	}

	var e struct{ N int64 }
	if err := ds.Get(ctx, datastore.NameKey("Word", "kiwi", nil), &e); err != nil || e.N != 2 {
		t.Errorf("Datastore Word/kiwi = %+v, %v; want n 2", e, err)
	}
	row, err := bt.Open(table).ReadRow(ctx, "kiwi")
	if err != nil || len(row["cf"]) != 1 || string(row["cf"][0].Value) != "2" {
		t.Errorf("Bigtable row kiwi = %v, %v; want cf:n 2", row, err)
	}
	sp, err := spanner.NewClient(ctx, db, spannerOpts(spannerAddr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	var got []string
	it := sp.Single().Read(ctx, "Words", spanner.AllKeys(), []string{"Word", "N"})
	if err := it.Do(func(r *spanner.Row) error {
		var w string
		var n int64
		if err := r.Columns(&w, &n); err != nil {
			return err
		}
		got = append(got, fmt.Sprintf("%s=%d", w, n))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "fig=1,kiwi=2" {
		t.Errorf("Spanner Words = %v; want fig=1, kiwi=2", got)
	}
}
