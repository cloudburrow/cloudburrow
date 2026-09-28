//go:build compat

package compat

import (
	"reflect"
	"testing"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"cloud.google.com/go/bigquery/storage/managedwriter"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestBigQueryStorageWriteStreamsOutliveAFrontRestart (#1115): the front
// keeps its Storage Write streams, and the rows a PENDING or BUFFERED
// stream holds, on the pod's emptyDir, so a restart of the front's
// container alone, the emulator still running, loses none of them. With
// the managed writer: rows appended to a PENDING stream and to a BUFFERED
// one before the restart, and to a COMMITTED one, whose offset is kept;
// after it, each stream is found by name, takes its next rows at the next
// offset, and the PENDING stream's commit and the BUFFERED stream's flush
// write every row once. Until #1115 the streams lived in the front's
// memory alone, which #1112 recorded: after a restart each was NOT_FOUND
// and its held rows were gone (as unit
// TestStorageWriteStreamsOutliveAFrontRestart shows of a front given no
// state directory).
func TestBigQueryStorageWriteStreamsOutliveAFrontRestart(t *testing.T) {
	h := New(t)
	c, project, tables := writeTables(t, h)
	m, dp := writerSchema(t, h, tables[0])
	mw := managedWriter(t, h, project)
	open := func(tbl int, opts ...managedwriter.WriterOption) *managedwriter.ManagedStream {
		t.Helper()
		ms, err := mw.NewManagedStream(h.Context(), append([]managedwriter.WriterOption{
			managedwriter.WithDestinationTable(writeTablePath(tables[tbl])), managedwriter.WithSchemaDescriptor(dp)}, opts...)...)
		if err != nil {
			t.Fatalf("NewManagedStream: %v", err)
		}
		t.Cleanup(func() { _ = ms.Close() })
		return ms
	}
	appendAt := func(ms *managedwriter.ManagedStream, off int64, ids ...int64) {
		t.Helper()
		var rows [][]byte
		for _, id := range ids {
			rows = append(rows, idRow(t, m, id))
		}
		got, err := appendResult(t, h, ms, rows, managedwriter.WithOffset(off))
		if status.Code(err) == codes.Unavailable {
			// A connection made just after the restart sometimes ends
			// EOF (#1136): once more, as a client retries it; an offset
			// already written means the first attempt landed.
			t.Logf("append %v at %d to %s: %v; retrying once (#1136)", ids, off, ms.StreamName(), err)
			got, err = appendResult(t, h, ms, rows, managedwriter.WithOffset(off))
			if status.Code(err) == codes.AlreadyExists {
				got, err = off, nil
			}
		}
		if err != nil || got != off {
			t.Fatalf("append %v at %d to %s: offset %d, %v", ids, off, ms.StreamName(), got, err)
		}
	}
	pending := open(0, managedwriter.WithType(managedwriter.PendingStream))
	buffered := open(1, managedwriter.WithType(managedwriter.BufferedStream))
	committed := open(1, managedwriter.WithType(managedwriter.CommittedStream))
	appendAt(pending, 0, 1, 2)
	appendAt(buffered, 0, 11, 12, 13)
	appendAt(committed, 0, 21)
	if _, err := buffered.FlushRows(h.Context(), 0); err != nil {
		t.Fatalf("FlushRows(0): %v", err)
	}

	restartFront(t, "bigquery", "bigquery")

	for _, ms := range []*managedwriter.ManagedStream{pending, buffered, committed} {
		if _, err := mw.GetWriteStream(h.Context(), &storagepb.GetWriteStreamRequest{Name: ms.StreamName()}); err != nil {
			t.Errorf("GetWriteStream %s after the restart: %v", ms.StreamName(), err)
		}
	}
	// The streams are opened again by name, from a new client, as a
	// writer that restarted with the front would: the old client's
	// connections ended with the front's process.
	mw = managedWriter(t, h, project)
	pending2 := open(0, managedwriter.WithStreamName(pending.StreamName()))
	committed2 := open(1, managedwriter.WithStreamName(committed.StreamName()))
	buffered2 := open(1, managedwriter.WithStreamName(buffered.StreamName()))
	appendAt(pending2, 2, 3)
	appendAt(committed2, 1, 22)
	read := func(tbl int) []string {
		return readRows(t, c.Dataset(tables[tbl].DatasetID).Table(tables[tbl].TableID).Read(h.Context()), "Table.Read")
	}
	if got := read(0); len(got) != 0 {
		t.Errorf("rows of the PENDING stream's table before the commit: %q", got)
	}
	if n, err := pending2.Finalize(h.Context()); err != nil || n != 3 {
		t.Fatalf("Finalize: %d rows, %v; want 3", n, err)
	}
	resp, err := mw.BatchCommitWriteStreams(h.Context(), &storagepb.BatchCommitWriteStreamsRequest{
		Parent: writeTablePath(tables[0]), WriteStreams: []string{pending.StreamName()}})
	if err != nil || len(resp.GetStreamErrors()) != 0 {
		t.Fatalf("BatchCommitWriteStreams: %v, %v", resp, err)
	}
	if got, want := read(0), []string{idRowRead("1"), idRowRead("2"), idRowRead("3")}; !reflect.DeepEqual(got, want) {
		t.Errorf("the PENDING stream's rows: %q, want %q", got, want)
	}
	if _, err := buffered2.FlushRows(h.Context(), 2); err != nil {
		t.Fatalf("FlushRows(2) after the restart: %v", err)
	}
	want := []string{idRowRead("11"), idRowRead("12"), idRowRead("13"), idRowRead("21"), idRowRead("22")}
	if got := read(1); !reflect.DeepEqual(got, want) {
		t.Errorf("the BUFFERED and COMMITTED streams' rows: %q, want %q", got, want)
	}
}
