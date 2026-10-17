package bigtablefront

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"cloud.google.com/go/bigtable/bttest"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// describeChange is one DataChange's mutations in a short form.
func describeChange(dc *bigtablepb.ReadChangeStreamResponse_DataChange) string {
	var parts []string
	for _, ch := range dc.GetChunks() {
		switch m := ch.GetMutation().GetMutation().(type) {
		case *bigtablepb.Mutation_SetCell_:
			parts = append(parts, fmt.Sprintf("set %s:%s=%q", m.SetCell.GetFamilyName(), m.SetCell.GetColumnQualifier(), m.SetCell.GetValue()))
		case *bigtablepb.Mutation_DeleteFromColumn_:
			parts = append(parts, fmt.Sprintf("delcol %s:%s", m.DeleteFromColumn.GetFamilyName(), m.DeleteFromColumn.GetColumnQualifier()))
		case *bigtablepb.Mutation_DeleteFromFamily_:
			parts = append(parts, "delfam "+m.DeleteFromFamily.GetFamilyName())
		default:
			parts = append(parts, fmt.Sprintf("%T", m))
		}
	}
	return string(dc.GetRowKey()) + ": " + strings.Join(parts, ", ")
}

// readStream reads a change stream to its end, returning its records.
func readStream(t *testing.T, d bigtablepb.BigtableClient, ctx context.Context, req *bigtablepb.ReadChangeStreamRequest) ([]*bigtablepb.ReadChangeStreamResponse, error) {
	t.Helper()
	s, err := d.ReadChangeStream(ctx, req)
	if err != nil {
		return nil, err
	}
	var out []*bigtablepb.ReadChangeStreamResponse
	for {
		r, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
}

func dataChanges(rs []*bigtablepb.ReadChangeStreamResponse) []*bigtablepb.ReadChangeStreamResponse_DataChange {
	var out []*bigtablepb.ReadChangeStreamResponse_DataChange
	for _, r := range rs {
		if dc := r.GetDataChange(); dc != nil {
			out = append(out, dc)
		}
	}
	return out
}

func changeStreamClients(t *testing.T, addr string) (context.Context, *bigtable.AdminClient, *bigtable.Client, bigtablepb.BigtableClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	conn := dial(t, addr)
	ac, err := bigtable.NewAdminClient(ctx, "p", "i", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	c, err := bigtable.NewClient(ctx, "p", "i", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	return ctx, ac, c, bigtablepb.NewBigtableClient(conn)
}

// TestChangeStreamRecordsTheWrites (#1523): the writes to a table with a
// change stream are read back from it, as Google's change stream records
// them, through the official clients, and a read resumes from a
// continuation token.
func TestChangeStreamRecordsTheWrites(t *testing.T) {
	addr := serve(t)
	ctx, ac, c, d := changeStreamClients(t, addr)
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "cs", ChangeStreamRetention: 36 * time.Hour,
		Families: map[string]bigtable.GCPolicy{"f": bigtable.NoGcPolicy(), "g": bigtable.NoGcPolicy()}}); err != nil {
		t.Fatalf("CreateTable with a change stream: %v", err)
	}
	if ti, err := ac.TableInfo(ctx, "cs"); err != nil || ti.ChangeStreamRetention != 36*time.Hour {
		t.Fatalf("TableInfo = %+v, %v; want a retention of 36h", ti, err)
	}
	tbl := c.Open("cs")
	start := time.Now()
	m := bigtable.NewMutation()
	m.Set("f", "q", bigtable.ServerTime, []byte("v1"))
	if err := tbl.Apply(ctx, "r1", m); err != nil {
		t.Fatal(err)
	}
	m1, m2 := bigtable.NewMutation(), bigtable.NewMutation()
	m1.Set("f", "q", bigtable.Timestamp(5000), []byte("a"))
	m2.Set("g", "x", bigtable.Timestamp(5000), []byte("b"))
	if errs, err := tbl.ApplyBulk(ctx, []string{"r2", "r3"}, []*bigtable.Mutation{m1, m2}); err != nil || errs != nil {
		t.Fatalf("ApplyBulk: %v %v", err, errs)
	}
	del := bigtable.NewMutation()
	del.DeleteCellsInColumn("f", "q")
	if err := tbl.Apply(ctx, "r2", del); err != nil {
		t.Fatal(err)
	}
	row := bigtable.NewMutation()
	row.DeleteRow()
	if err := tbl.Apply(ctx, "r3", row); err != nil {
		t.Fatal(err)
	}
	rmw := bigtable.NewReadModifyWrite()
	rmw.AppendValue("f", "q", []byte("+"))
	if _, err := tbl.ApplyReadModifyWrite(ctx, "r1", rmw); err != nil {
		t.Fatal(err)
	}
	yes, no := bigtable.NewMutation(), bigtable.NewMutation()
	yes.Set("f", "c", bigtable.Timestamp(7000), []byte("matched"))
	no.Set("f", "c", bigtable.Timestamp(7000), []byte("not matched"))
	if err := tbl.Apply(ctx, "r1", bigtable.NewCondMutation(bigtable.FamilyFilter("f"), yes, no)); err != nil {
		t.Fatal(err)
	}
	// DropRowRange is not recorded.
	if err := ac.DropRowRange(ctx, "cs", "zz"); err != nil {
		t.Fatal(err)
	}

	name := "projects/p/instances/i/tables/cs"
	ps, err := d.GenerateInitialChangeStreamPartitions(ctx, &bigtablepb.GenerateInitialChangeStreamPartitionsRequest{TableName: name})
	if err != nil {
		t.Fatal(err)
	}
	var parts []*bigtablepb.StreamPartition
	for {
		p, err := ps.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("GenerateInitialChangeStreamPartitions: %v", err)
		}
		parts = append(parts, p.GetPartition())
	}
	if len(parts) != 1 || !isWholeTable(parts[0]) {
		t.Fatalf("partitions = %v, want the whole table", parts)
	}
	end := time.Now()
	rs, err := readStream(t, d, ctx, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: parts[0],
		StartFrom: &bigtablepb.ReadChangeStreamRequest_StartTime{StartTime: timestamppb.New(start)},
		EndTime:   timestamppb.New(end)})
	if err != nil {
		t.Fatalf("ReadChangeStream: %v", err)
	}
	dcs := dataChanges(rs)
	var got []string
	var last time.Time
	for _, dc := range dcs {
		got = append(got, describeChange(dc))
		at := dc.GetCommitTimestamp().AsTime()
		if !at.After(last) || !dc.GetDone() || dc.GetToken() == "" || dc.GetEstimatedLowWatermark() == nil || dc.GetType() != bigtablepb.ReadChangeStreamResponse_DataChange_USER {
			t.Errorf("record %s: commit %v after %v, done %v, token %q, watermark %v, type %v", describeChange(dc), at, last, dc.GetDone(), dc.GetToken(), dc.GetEstimatedLowWatermark(), dc.GetType())
		}
		last = at
	}
	want := []string{
		`r1: set f:q="v1"`,
		`r2: set f:q="a"`,
		`r3: set g:x="b"`,
		`r2: delcol f:q`,
		`r3: delfam f, delfam g`,
		`r1: set f:q="v1+"`,
		`r1: set f:c="matched"`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the change stream holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The server-timestamped cell is logged with its stored timestamp.
	r, err := tbl.ReadRow(ctx, "r1", bigtable.RowFilter(bigtable.ColumnFilter("q")))
	if err != nil {
		t.Fatal(err)
	}
	checkStampedCells(t, dcs[0], dcs[5], r["f"])

	// Resumed from the third record's token: the rest.
	rs, err = readStream(t, d, ctx, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: parts[0],
		StartFrom: &bigtablepb.ReadChangeStreamRequest_ContinuationTokens{ContinuationTokens: &bigtablepb.StreamContinuationTokens{
			Tokens: []*bigtablepb.StreamContinuationToken{{Partition: parts[0], Token: dcs[2].GetToken()}}}},
		EndTime: timestamppb.New(end)})
	if err != nil {
		t.Fatalf("ReadChangeStream from a token: %v", err)
	}
	if n := len(dataChanges(rs)); n != len(want)-3 {
		t.Errorf("resumed from the third record: %d records, want %d", n, len(want)-3)
	}

	// Caught up, a stream sends heartbeats with the token to resume from.
	// Bounded by ctx, not by a few seconds a loaded machine can overrun.
	hctx, hcancel := context.WithCancel(ctx)
	defer hcancel()
	s, err := d.ReadChangeStream(hctx, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: parts[0],
		StartFrom: &bigtablepb.ReadChangeStreamRequest_ContinuationTokens{ContinuationTokens: &bigtablepb.StreamContinuationTokens{
			Tokens: []*bigtablepb.StreamContinuationToken{{Partition: parts[0], Token: dcs[len(dcs)-1].GetToken()}}}},
		HeartbeatDuration: durationpb.New(100 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	hb, err := s.Recv()
	if err != nil || hb.GetHeartbeat() == nil || hb.GetHeartbeat().GetContinuationToken().GetToken() != dcs[len(dcs)-1].GetToken() ||
		hb.GetHeartbeat().GetEstimatedLowWatermark().AsTime().Before(last) {
		t.Fatalf("a caught-up stream's first message = %v, %v; want a heartbeat", hb, err)
	}
	// A write reaches the open stream.
	live := bigtable.NewMutation()
	live.Set("f", "q", bigtable.Timestamp(9000), []byte("live"))
	if err := tbl.Apply(ctx, "r9", live); err != nil {
		t.Fatal(err)
	}
	for {
		msg, err := s.Recv()
		if err != nil {
			t.Fatalf("waiting for the live write: %v", err)
		}
		if dc := msg.GetDataChange(); dc != nil {
			if got := describeChange(dc); got != `r9: set f:q="live"` {
				t.Errorf("the live record = %s", got)
			}
			break
		}
	}
	hcancel()

	// Another partition is closed with the table's.
	rs, err = readStream(t, d, ctx, &bigtablepb.ReadChangeStreamRequest{TableName: name,
		Partition: &bigtablepb.StreamPartition{RowRange: &bigtablepb.RowRange{EndKey: &bigtablepb.RowRange_EndKeyOpen{EndKeyOpen: []byte("m")}}}})
	if err != nil || len(rs) != 1 || rs[0].GetCloseStream() == nil || len(rs[0].GetCloseStream().GetNewPartitions()) != 1 ||
		!isWholeTable(rs[0].GetCloseStream().GetNewPartitions()[0]) || len(rs[0].GetCloseStream().GetContinuationTokens()) != 1 {
		t.Errorf("a read of another partition = %v, %v; want a CloseStream with the table's partition", rs, err)
	}
}

// checkStampedCells checks the logged timestamps of a server-timestamped
// SetCell (set) and a later ReadModifyWriteRow (rmw) of the same column
// against the column's stored cells, newest first. The append is stamped
// max(now, the cell's timestamp) in milliseconds, so when both land in the
// same millisecond it replaces the cell rather than adding one (#1575): the
// column then holds a single cell, at the timestamp both records carry.
func checkStampedCells(t *testing.T, set, rmw *bigtablepb.ReadChangeStreamResponse_DataChange, cells []bigtable.ReadItem) {
	t.Helper()
	setTS := set.GetChunks()[0].GetMutation().GetSetCell().GetTimestampMicros()
	rmwCell := rmw.GetChunks()[0].GetMutation().GetSetCell()
	if setTS <= 0 || len(cells) == 0 {
		t.Fatalf("the logged timestamp %d, the stored cells %v", setTS, cells)
	}
	if got := int64(cells[len(cells)-1].Timestamp); got != setTS {
		t.Errorf("the server-timestamped cell is logged at %d, stored at %d (cells %v)", setTS, got, cells)
	}
	if got := int64(cells[0].Timestamp); got != rmwCell.GetTimestampMicros() || string(cells[0].Value) != string(rmwCell.GetValue()) {
		t.Errorf("the ReadModifyWriteRow cell is logged as %d=%q, stored as %v", rmwCell.GetTimestampMicros(), rmwCell.GetValue(), cells[0])
	}
	if want := 2; rmwCell.GetTimestampMicros() == setTS {
		want = 1
		if len(cells) != want {
			t.Errorf("the append in the set's millisecond: %d cells %v, want %d", len(cells), cells, want)
		}
	} else if len(cells) != want {
		t.Errorf("%d cells %v, want %d", len(cells), cells, want)
	}
}

// TestChangeStreamAppendInTheSameMillisecond (#1575): a ReadModifyWriteRow
// in the millisecond of the server-timestamped SetCell before it replaces
// that cell, and the change stream's two records still replay to what is
// stored. The front's clock is pinned ahead of the emulator's, so the
// append always lands on the set's timestamp; TestChangeStreamRecordsTheWrites
// failed about once in 70 runs when the two fell in one millisecond.
func TestChangeStreamAppendInTheSameMillisecond(t *testing.T) {
	addr, f := serveFront(t)
	pinned := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	f.changes.now = func() time.Time { return pinned }
	ctx, ac, c, d := changeStreamClients(t, addr)
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "cs", ChangeStreamRetention: 24 * time.Hour,
		Families: map[string]bigtable.GCPolicy{"f": bigtable.NoGcPolicy()}}); err != nil {
		t.Fatal(err)
	}
	tbl := c.Open("cs")
	m := bigtable.NewMutation()
	m.Set("f", "q", bigtable.ServerTime, []byte("v1"))
	if err := tbl.Apply(ctx, "r1", m); err != nil {
		t.Fatal(err)
	}
	rmw := bigtable.NewReadModifyWrite()
	rmw.AppendValue("f", "q", []byte("+"))
	if _, err := tbl.ApplyReadModifyWrite(ctx, "r1", rmw); err != nil {
		t.Fatal(err)
	}
	name := "projects/p/instances/i/tables/cs"
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s, err := d.ReadChangeStream(hctx, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: wholeTable(),
		StartFrom: &bigtablepb.ReadChangeStreamRequest_ContinuationTokens{ContinuationTokens: &bigtablepb.StreamContinuationTokens{
			Tokens: []*bigtablepb.StreamContinuationToken{{Partition: wholeTable(), Token: encodeToken(0)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var dcs []*bigtablepb.ReadChangeStreamResponse_DataChange
	for len(dcs) < 2 {
		msg, err := s.Recv()
		if err != nil {
			t.Fatalf("ReadChangeStream: %v", err)
		}
		if dc := msg.GetDataChange(); dc != nil {
			dcs = append(dcs, dc)
		}
	}
	if got := describeChange(dcs[0]) + "; " + describeChange(dcs[1]); got != `r1: set f:q="v1"; r1: set f:q="v1+"` {
		t.Fatalf("the change stream holds %s", got)
	}
	r, err := tbl.ReadRow(ctx, "r1", bigtable.RowFilter(bigtable.ColumnFilter("q")))
	if err != nil {
		t.Fatal(err)
	}
	if setTS := dcs[0].GetChunks()[0].GetMutation().GetSetCell().GetTimestampMicros(); setTS != pinned.UnixMicro() ||
		dcs[1].GetChunks()[0].GetMutation().GetSetCell().GetTimestampMicros() != setTS {
		t.Fatalf("the records are stamped %v and %v, want both at the pinned %d", dcs[0], dcs[1], pinned.UnixMicro())
	}
	checkStampedCells(t, dcs[0], dcs[1], r["f"])
}

// TestChangeStreamChunksLargeValues: a SetCell value larger than a chunk is
// sent in chunks, the DataChange's header on the first message and its
// token on the last.
func TestChangeStreamChunksLargeValues(t *testing.T) {
	old := maxChunkBytes
	maxChunkBytes = 4
	t.Cleanup(func() { maxChunkBytes = old })
	addr := serve(t)
	ctx, ac, c, d := changeStreamClients(t, addr)
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "big", ChangeStreamRetention: 24 * time.Hour,
		Families: map[string]bigtable.GCPolicy{"f": bigtable.NoGcPolicy()}}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	m := bigtable.NewMutation()
	m.Set("f", "q", bigtable.Timestamp(1000), []byte("0123456789"))
	if err := c.Open("big").Apply(ctx, "k", m); err != nil {
		t.Fatal(err)
	}
	rs, err := readStream(t, d, ctx, &bigtablepb.ReadChangeStreamRequest{TableName: "projects/p/instances/i/tables/big", Partition: wholeTable(),
		StartFrom: &bigtablepb.ReadChangeStreamRequest_StartTime{StartTime: timestamppb.New(start)},
		EndTime:   timestamppb.Now()})
	if err != nil {
		t.Fatal(err)
	}
	dcs := dataChanges(rs)
	if len(dcs) != 3 {
		t.Fatalf("%d messages, want 3", len(dcs))
	}
	var value []byte
	for i, dc := range dcs {
		first, lastMsg := i == 0, i == len(dcs)-1
		if (dc.GetRowKey() != nil) != first || dc.GetDone() != lastMsg || (dc.GetToken() != "") != lastMsg {
			t.Errorf("message %d: row %q, done %v, token %q", i, dc.GetRowKey(), dc.GetDone(), dc.GetToken())
		}
		ch := dc.GetChunks()[0]
		if ch.GetChunkInfo().GetChunkedValueSize() != 10 || int(ch.GetChunkInfo().GetChunkedValueOffset()) != len(value) || ch.GetChunkInfo().GetLastChunk() != lastMsg {
			t.Errorf("message %d: chunk info %v", i, ch.GetChunkInfo())
		}
		value = append(value, ch.GetMutation().GetSetCell().GetValue()...)
	}
	if string(value) != "0123456789" {
		t.Errorf("the chunks join to %q", value)
	}
}

// TestChangeStreamConfiguration: UpdateTable changes the retention and
// disables the stream, which a read is then refused, and keeps the table's
// deletion protection; a retention outside 1 to 7 days is refused; a
// table without a stream refuses its reads.
func TestChangeStreamConfiguration(t *testing.T) {
	addr := serve(t)
	ctx, ac, _, d := changeStreamClients(t, addr)
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "cfg", DeletionProtection: bigtable.Protected}); err != nil {
		t.Fatal(err)
	}
	name := "projects/p/instances/i/tables/cfg"
	if _, err := readStream(t, d, ctx, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: wholeTable()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a read without a change stream: %v, want FailedPrecondition", err)
	}
	if err := ac.UpdateTableWithChangeStream(ctx, "cfg", 72*time.Hour); err != nil {
		t.Fatalf("UpdateTableWithChangeStream: %v", err)
	}
	ti, err := ac.TableInfo(ctx, "cfg")
	if err != nil || ti.ChangeStreamRetention != 72*time.Hour || ti.DeletionProtection != bigtable.Protected {
		t.Fatalf("TableInfo = %+v, %v", ti, err)
	}
	if err := ac.UpdateTableWithChangeStream(ctx, "cfg", 8*24*time.Hour); status.Code(errors.Unwrap(err)) != codes.InvalidArgument && !strings.Contains(fmt.Sprint(err), "between 1 and 7 days") {
		t.Errorf("a retention of 8 days: %v, want InvalidArgument", err)
	}
	if err := ac.UpdateTableDisableChangeStream(ctx, "cfg"); err != nil {
		t.Fatalf("UpdateTableDisableChangeStream: %v", err)
	}
	if ti, err := ac.TableInfo(ctx, "cfg"); err != nil || ti.ChangeStreamRetention != nil {
		t.Errorf("TableInfo after disabling = %+v, %v", ti, err)
	}
	if _, err := readStream(t, d, ctx, &bigtablepb.ReadChangeStreamRequest{TableName: name, Partition: wholeTable()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a read after disabling: %v, want FailedPrecondition", err)
	}
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "short", ChangeStreamRetention: time.Hour}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateTable with a retention of an hour: %v, want InvalidArgument", err)
	}
}

// TestChangeLogSurvivesARestart: the change log, full segments and the
// rest, is read back through a new front on the same state directory.
func TestChangeLogSurvivesARestart(t *testing.T) {
	emu, err := bttest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(emu.Close)
	dir := t.TempDir()
	addr, stop := serveKept(t, emu.Addr, dir)
	ctx, ac, c, _ := changeStreamClients(t, addr)
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "kept", ChangeStreamRetention: 24 * time.Hour,
		Families: map[string]bigtable.GCPolicy{"f": bigtable.NoGcPolicy()}}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	n := segmentRecords + 10
	for i := 0; i < n; i++ {
		m := bigtable.NewMutation()
		m.Set("f", "q", bigtable.Timestamp(1000), []byte(fmt.Sprint(i)))
		if err := c.Open("kept").Apply(ctx, fmt.Sprintf("r%03d", i), m); err != nil {
			t.Fatal(err)
		}
	}
	stop()
	// The stopped front's last flush runs as its context ends.
	time.Sleep(200 * time.Millisecond)

	addr, _ = serveKept(t, emu.Addr, dir)
	ctx, ac, _, d := changeStreamClients(t, addr)
	if ti, err := ac.TableInfo(ctx, "kept"); err != nil || ti.ChangeStreamRetention != 24*time.Hour {
		t.Fatalf("TableInfo after the restart = %+v, %v", ti, err)
	}
	rs, err := readStream(t, d, ctx, &bigtablepb.ReadChangeStreamRequest{TableName: "projects/p/instances/i/tables/kept", Partition: wholeTable(),
		StartFrom: &bigtablepb.ReadChangeStreamRequest_StartTime{StartTime: timestamppb.New(start)},
		EndTime:   timestamppb.Now()})
	if err != nil {
		t.Fatal(err)
	}
	dcs := dataChanges(rs)
	if len(dcs) != n || string(dcs[n-1].GetRowKey()) != fmt.Sprintf("r%03d", n-1) {
		t.Fatalf("after the restart: %d records, want %d", len(dcs), n)
	}
	// Writes after the restart continue the log.
	m := bigtable.NewMutation()
	m.Set("f", "q", bigtable.Timestamp(1000), []byte("after"))
	c2, err := bigtable.NewClient(ctx, "p", "i", option.WithGRPCConn(dial(t, addr)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.Open("kept").Apply(ctx, "later", m); err != nil {
		t.Fatal(err)
	}
	rs, err = readStream(t, d, ctx, &bigtablepb.ReadChangeStreamRequest{TableName: "projects/p/instances/i/tables/kept", Partition: wholeTable(),
		StartFrom: &bigtablepb.ReadChangeStreamRequest_ContinuationTokens{ContinuationTokens: &bigtablepb.StreamContinuationTokens{
			Tokens: []*bigtablepb.StreamContinuationToken{{Partition: wholeTable(), Token: dcs[n-1].GetToken()}}}},
		EndTime: timestamppb.Now()})
	if err != nil || len(dataChanges(rs)) != 1 || string(dataChanges(rs)[0].GetRowKey()) != "later" {
		t.Fatalf("resumed after the restart: %v, %v", rs, err)
	}
}
