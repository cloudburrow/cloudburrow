//go:build compat

package compat

import (
	"errors"
	"io"
	"testing"

	bqstorage "cloud.google.com/go/bigquery/storage/apiv1"
	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestBigQueryStorageReadSplitAndRestart (#1460): SplitReadStream splits a
// stream at a fraction of its rows into a primary stream of its first
// rows and a remainder of the rest, as the Storage Read API reference
// documents; a split of the remainder splits it again; a stream of one
// row "can no longer be split" (an empty response); a fraction outside
// (0, 1) is INVALID_ARGUMENT. After a restart of the front's container,
// the streams made before it, split ones too, read on. Measured first:
// SplitReadStream failed "unimplemented split read stream" (the
// emulator's), and ReadRows of a stream made before the front last
// started was UNIMPLEMENTED.
//
// covers: bigquery.jobs.query
func TestBigQueryStorageReadSplitAndRestart(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvBigQueryStorage)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	if err := bqRun(ctx, c, "CREATE TABLE "+ds.DatasetID+".split AS SELECT n FROM UNNEST(GENERATE_ARRAY(1, 10)) AS n", false); err != nil {
		t.Fatal(err)
	}
	newClient := func() *bqstorage.BigQueryReadClient {
		rc, err := bqstorage.NewBigQueryReadClient(ctx,
			option.WithEndpoint(endpoint), option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
		if err != nil {
			t.Fatalf("NewBigQueryReadClient: %v", err)
		}
		t.Cleanup(func() { _ = rc.Close() })
		return rc
	}
	rc := newClient()
	sess, err := rc.CreateReadSession(ctx, &storagepb.CreateReadSessionRequest{Parent: "projects/" + project, MaxStreamCount: 1,
		ReadSession: &storagepb.ReadSession{Table: "projects/" + project + "/datasets/" + ds.DatasetID + "/tables/split",
			DataFormat: storagepb.DataFormat_AVRO}})
	if err != nil || len(sess.GetStreams()) == 0 {
		t.Fatalf("CreateReadSession: %v, %v", sess, err)
	}
	count := func(rc *bqstorage.BigQueryReadClient, stream string) (int64, error) {
		t.Helper()
		rs, err := rc.ReadRows(ctx, &storagepb.ReadRowsRequest{ReadStream: stream})
		if err != nil {
			return 0, err
		}
		var n int64
		for {
			resp, err := rs.Recv()
			if errors.Is(err, io.EOF) {
				return n, nil
			}
			if err != nil {
				return n, err
			}
			n += resp.GetRowCount()
		}
	}
	split := func(stream string, fraction float64) *storagepb.SplitReadStreamResponse {
		t.Helper()
		resp, err := rc.SplitReadStream(ctx, &storagepb.SplitReadStreamRequest{Name: stream, Fraction: fraction})
		if err != nil {
			t.Fatalf("SplitReadStream(%s, %v): %v", stream, fraction, err)
		}
		return resp
	}
	original := sess.GetStreams()[0].GetName()
	first := split(original, 0.4)
	primary, remainder := first.GetPrimaryStream().GetName(), first.GetRemainderStream().GetName()
	if primary == "" || remainder == "" || primary == original || primary == remainder {
		t.Fatalf("split of %s: %+v", original, first)
	}
	second := split(remainder, 0.5)
	want := map[string]int64{original: 10, primary: 4, remainder: 6,
		second.GetPrimaryStream().GetName(): 3, second.GetRemainderStream().GetName(): 3}
	for stream, n := range want {
		if got, err := count(rc, stream); err != nil || got != n {
			t.Errorf("ReadRows of %s: %d rows, %v; want %d", stream, got, err, n)
		}
	}
	// A stream of one row: no split.
	one := split(split(primary, 0.5).GetPrimaryStream().GetName(), 0.5) // of 2 rows, then 1
	if one.GetPrimaryStream() == nil {
		t.Fatalf("split of 2 rows: %+v", one)
	}
	if last := split(one.GetPrimaryStream().GetName(), 0.5); last.GetPrimaryStream() != nil || last.GetRemainderStream() != nil {
		t.Errorf("split of a stream of one row: %+v, want an empty response", last)
	}
	for _, f := range []float64{0, 1, 1.5} {
		_, err := rc.SplitReadStream(ctx, &storagepb.SplitReadStreamRequest{Name: original, Fraction: f})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("SplitReadStream fraction %v: %v, want INVALID_ARGUMENT", f, err)
		}
	}
	_, err = count(rc, "projects/"+project+"/locations/us/sessions/none/streams/none")
	if status.Code(err) != codes.NotFound {
		t.Errorf("ReadRows of a stream never made: %v, want NOT_FOUND", err)
	}

	restartFront(t, "bigquery", "bigquery")

	rc = newClient()
	for stream, n := range want {
		if got, err := count(rc, stream); err != nil || got != n {
			t.Errorf("after the restart, ReadRows of %s: %d rows, %v; want %d", stream, got, err, n)
		}
	}
}
