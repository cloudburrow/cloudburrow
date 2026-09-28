package bigqueryfront

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// A restart of the front, given a state directory, keeps its streams, their
// offsets, and the rows a PENDING stream holds until its commit and a
// BUFFERED one until its flush (#1115); without one, it keeps none.
func TestStorageWriteStreamsOutliveAFrontRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "storage-write")
	tables := &fakeTables{schemas: map[string]string{"ds.t": writeTestFields, "ds.u": writeTestFields}, rows: map[string][]map[string]any{}}
	c, stop := serveWriteFront(t, ctx, tables, &writeOptions{stateDir: dir})
	create := func(c storagepb.BigQueryWriteClient, table string, typ storagepb.WriteStream_Type) string {
		ws, err := c.CreateWriteStream(ctx, &storagepb.CreateWriteStreamRequest{Parent: "projects/p-1/datasets/ds/tables/" + table,
			WriteStream: &storagepb.WriteStream{Type: typ}})
		if err != nil {
			t.Fatalf("CreateWriteStream %s: %v", typ, err)
		}
		return ws.GetName()
	}
	appendAt := func(c storagepb.BigQueryWriteClient, stream string, off int64, ids ...int64) {
		t.Helper()
		var rows [][]byte
		for _, id := range ids {
			rows = append(rows, testRow(t, id, nil))
		}
		resp, err := openAppend(t, ctx, c).send(rowsReq(stream, true, wrapperspb.Int64(off), rows...))
		if err != nil || resp.GetError() != nil || resp.GetAppendResult().GetOffset().GetValue() != off {
			t.Fatalf("append %v at %d to %s: %v, %v", ids, off, stream, resp, err)
		}
	}
	pending := create(c, "t", storagepb.WriteStream_PENDING)
	buffered := create(c, "u", storagepb.WriteStream_BUFFERED)
	committed := create(c, "u", storagepb.WriteStream_COMMITTED)
	appendAt(c, pending, 0, 1, 2)
	appendAt(c, buffered, 0, 11, 12, 13)
	appendAt(c, committed, 0, 21)
	if _, err := c.FlushRows(ctx, &storagepb.FlushRowsRequest{WriteStream: buffered, Offset: wrapperspb.Int64(0)}); err != nil {
		t.Fatalf("FlushRows: %v", err)
	}
	stop()
	// An append that failed between its rows and its state leaves bytes
	// past the state's; a restore ignores them.
	f, err := os.OpenFile(filepath.Join(dir, streamID(pending)+".rows"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"i":"99"}` + "\n" + `{"i":`)
	_ = f.Close()

	c, _ = serveWriteFront(t, ctx, tables, &writeOptions{stateDir: dir})
	for _, name := range []string{pending, buffered, committed} {
		if _, err := c.GetWriteStream(ctx, &storagepb.GetWriteStreamRequest{Name: name}); err != nil {
			t.Errorf("GetWriteStream %s after the restart: %v", name, err)
		}
	}
	appendAt(c, pending, 2, 3)
	appendAt(c, committed, 1, 22)
	if got := tables.ids("ds.t"); len(got) != 0 {
		t.Errorf("ds.t before the commit: %v", got)
	}
	if f, err := c.FinalizeWriteStream(ctx, &storagepb.FinalizeWriteStreamRequest{Name: pending}); err != nil || f.GetRowCount() != 3 {
		t.Fatalf("Finalize: %v, %v", f, err)
	}
	if r, err := c.BatchCommitWriteStreams(ctx, &storagepb.BatchCommitWriteStreamsRequest{Parent: "projects/p-1/datasets/ds/tables/t",
		WriteStreams: []string{pending}}); err != nil || len(r.GetStreamErrors()) > 0 {
		t.Fatalf("BatchCommitWriteStreams: %v, %v", r, err)
	}
	if got := tables.ids("ds.t"); !reflect.DeepEqual(got, []string{"1", "2", "3"}) {
		t.Errorf("ds.t after the commit: %v, want 1 2 3", got)
	}
	if _, err := os.Stat(filepath.Join(dir, streamID(pending)+".rows")); !os.IsNotExist(err) {
		t.Errorf("the committed stream's rows file is still there: %v", err)
	}
	if _, err := c.FlushRows(ctx, &storagepb.FlushRowsRequest{WriteStream: buffered, Offset: wrapperspb.Int64(2)}); err != nil {
		t.Fatalf("FlushRows after the restart: %v", err)
	}
	if got := tables.ids("ds.u"); !reflect.DeepEqual(got, []string{"21", "11", "22", "12", "13"}) {
		t.Errorf("ds.u: %v, want 21 11 22 12 13", got)
	}

	// Without a state directory a restart keeps nothing.
	c, stop = serveWriteFront(t, ctx, tables, nil)
	mem := create(c, "t", storagepb.WriteStream_PENDING)
	stop()
	c, _ = serveWriteFront(t, ctx, tables, nil)
	if _, err := c.GetWriteStream(ctx, &storagepb.GetWriteStreamRequest{Name: mem}); status.Code(err) != codes.NotFound {
		t.Errorf("GetWriteStream after a restart without state: %v, want NOT_FOUND", err)
	}
}

// A state file that cannot be read leaves its stream out, and the rest in.
func TestStorageWriteStateSkipsAnUnreadableStream(t *testing.T) {
	dir := t.TempDir()
	w := newStorageWrite(&fakeTables{})
	var logged []string
	w.logf = func(f string, a ...any) { logged = append(logged, f) }
	if err := os.WriteFile(filepath.Join(dir, "cbbad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := &writeStream{name: "projects/p/datasets/d/tables/t/streams/cbgood", typ: storagepb.WriteStream_COMMITTED, rows: 4}
	w.dir = dir
	if err := w.saveStream(good); err != nil {
		t.Fatal(err)
	}
	w2 := newStorageWrite(&fakeTables{})
	w2.logf = w.logf
	if err := w2.keepStreams(dir); err != nil {
		t.Fatal(err)
	}
	if st := w2.streams[good.name]; st == nil || st.rows != 4 || len(w2.streams) != 1 {
		t.Errorf("restored %v, want the good stream with 4 rows alone", w2.streams)
	}
	if len(logged) == 0 {
		t.Error("the unreadable stream was not logged")
	}
}
