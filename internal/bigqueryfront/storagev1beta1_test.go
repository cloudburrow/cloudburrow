package bigqueryfront

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	beta "cloud.google.com/go/bigquery/storage/apiv1beta1/storagepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestStorageV1beta1 (#2215): v1beta1's BigQueryStorage, as BigQuery to
// Parquet's launcher calls it, reads the same session as v1: its Avro
// schema (the default format) is v1's, its stream's rows are v1's, a
// stream splits and finalizes, and a stream cannot be added.
func TestStorageV1beta1(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, _, _ := storageReadFrontConn(t, ctx, &jobRecords{})
	c, v1 := beta.NewBigQueryStorageClient(conn), storagepb.NewBigQueryReadClient(conn)
	tr := &beta.TableReference{ProjectId: "p-1", DatasetId: "ds", TableId: "t"}
	sess, err := c.CreateReadSession(ctx, &beta.CreateReadSessionRequest{Parent: "projects/p-1", TableReference: tr})
	if err != nil || len(sess.GetStreams()) != 1 || sess.GetAvroSchema().GetSchema() == "" {
		t.Fatalf("CreateReadSession: %v, %v; want one stream and an Avro schema", sess, err)
	}
	if sess.GetTableReference().GetTableId() != "t" {
		t.Errorf("the session's table is %v", sess.GetTableReference())
	}
	want, err := v1.CreateReadSession(ctx, &storagepb.CreateReadSessionRequest{Parent: "projects/p-1",
		ReadSession: &storagepb.ReadSession{Table: "projects/p-1/datasets/ds/tables/t", DataFormat: storagepb.DataFormat_AVRO}})
	if err != nil {
		t.Fatal(err)
	}
	if sess.GetAvroSchema().GetSchema() != want.GetAvroSchema().GetSchema() {
		t.Errorf("the v1beta1 Avro schema\n%s\nis not v1's\n%s", sess.GetAvroSchema().GetSchema(), want.GetAvroSchema().GetSchema())
	}
	read := func(name string, offset int64) (int64, error) {
		rs, err := c.ReadRows(ctx, &beta.ReadRowsRequest{ReadPosition: &beta.StreamPosition{Stream: &beta.Stream{Name: name}, Offset: offset}})
		if err != nil {
			return 0, err
		}
		var n int64
		for {
			r, err := rs.Recv()
			if errors.Is(err, io.EOF) {
				return n, nil
			}
			if err != nil {
				return n, err
			}
			if r.GetAvroRows().GetRowCount() != r.GetRowCount() || len(r.GetAvroRows().GetSerializedBinaryRows()) == 0 && r.GetRowCount() > 0 {
				t.Errorf("an answer's avro_rows %v and row_count %d", r.GetAvroRows(), r.GetRowCount())
			}
			n += r.GetRowCount()
		}
	}
	name := sess.GetStreams()[0].GetName()
	if n, err := read(name, 0); err != nil || n != 2 {
		t.Errorf("ReadRows: %d rows, %v; want the table's 2", n, err)
	}
	if n, err := read(name, 1); err != nil || n != 1 {
		t.Errorf("ReadRows from offset 1: %d rows, %v; want 1", n, err)
	}
	sp, err := c.SplitReadStream(ctx, &beta.SplitReadStreamRequest{OriginalStream: &beta.Stream{Name: name}, Fraction: 0.5})
	if err != nil || sp.GetPrimaryStream() == nil || sp.GetRemainderStream() == nil {
		t.Fatalf("SplitReadStream: %v, %v", sp, err)
	}
	for _, s := range []*beta.Stream{sp.GetPrimaryStream(), sp.GetRemainderStream()} {
		if n, err := read(s.GetName(), 0); err != nil || n != 1 {
			t.Errorf("ReadRows of the split %s: %d rows, %v; want 1", s.GetName(), n, err)
		}
	}
	if _, err := c.FinalizeStream(ctx, &beta.FinalizeStreamRequest{Stream: &beta.Stream{Name: name}}); err != nil {
		t.Errorf("FinalizeStream: %v", err)
	}
	if _, err := c.FinalizeStream(ctx, &beta.FinalizeStreamRequest{Stream: &beta.Stream{Name: "no-such"}}); status.Code(err) != codes.NotFound && status.Code(err) != codes.Unimplemented {
		t.Errorf("FinalizeStream of an unknown stream: %v", err)
	}
	if _, err := c.BatchCreateReadSessionStreams(ctx, &beta.BatchCreateReadSessionStreamsRequest{Session: sess, RequestedStreams: 2}); status.Code(err) != codes.Unimplemented {
		t.Errorf("BatchCreateReadSessionStreams: %v; want UNIMPLEMENTED", err)
	}
	if _, err := c.CreateReadSession(ctx, &beta.CreateReadSessionRequest{Parent: "projects/p-1", TableReference: &beta.TableReference{ProjectId: "p-1"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateReadSession without a table: %v; want INVALID_ARGUMENT", err)
	}
}
