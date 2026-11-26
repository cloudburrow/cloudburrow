package bigqueryfront

// The Storage API's v1beta1 surface (google.cloud.bigquery.storage.v1beta1.
// BigQueryStorage, #2215), which Google still serves and clients still
// call: Google's DataflowTemplates' BigQuery to Parquet launcher reads its
// table's schema with v1beta1's BigQueryStorageClient before it submits
// the job. Each call is the v1 call of the same meaning (storageread.go),
// its request and answer translated, so a v1beta1 session reads the same
// rows, in the same schema, as a v1 session of the table:
//
//   - CreateReadSession: table_reference and table_modifiers name the
//     table, read_options its columns and rows, format AVRO (the default,
//     "Currently default to Avro", storage.proto) or ARROW; the session's
//     streams are v1's (one).
//   - ReadRows: read_position's stream from its offset; each answer is
//     v1's rows (avro_rows or arrow_record_batch) and row_count.
//   - SplitReadStream: original_stream at fraction, primary_stream and
//     remainder_stream (none when the stream cannot be split there).
//   - FinalizeStream: a stream's rows are fixed when its session is made,
//     so it ends where it would have; the stream must be one the front
//     knows.
//   - BatchCreateReadSessionStreams: a session's streams are the one its
//     table's read is made of, so more cannot be added: UNIMPLEMENTED.
//
// (storage.proto of google/cloud/bigquery/storage/v1beta1, read
// 2026-11-25: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/bigquery/storage/v1beta1/storage.proto)

import (
	"context"
	"io"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	beta "cloud.google.com/go/bigquery/storage/apiv1beta1/storagepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	betaService                = "/google.cloud.bigquery.storage.v1beta1.BigQueryStorage/"
	betaCreateReadSession      = betaService + "CreateReadSession"
	betaReadRows               = betaService + "ReadRows"
	betaSplitReadStream        = betaService + "SplitReadStream"
	betaFinalizeStream         = betaService + "FinalizeStream"
	betaBatchCreateReadStreams = betaService + "BatchCreateReadSessionStreams"
)

// beta serves one v1beta1 call (above).
func (s *storageRead) beta(ss grpc.ServerStream, method string, open func() (grpc.ClientStream, error)) error {
	var in rawFrame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	switch method {
	case betaCreateReadSession:
		return s.betaCreateSession(ss, in, open)
	case betaReadRows:
		var req beta.ReadRowsRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			return status.Errorf(codes.InvalidArgument, "decode ReadRowsRequest: %v", err)
		}
		name := req.GetReadPosition().GetStream().GetName()
		if name == "" {
			return status.Error(codes.InvalidArgument, "read_position.stream.name is required")
		}
		st, err := s.streamOf(name)
		if err != nil {
			return err
		}
		return s.serveRows(ss.Context(), &betaRows{ServerStream: ss}, st, req.GetReadPosition().GetOffset())
	case betaSplitReadStream:
		var req beta.SplitReadStreamRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			return status.Errorf(codes.InvalidArgument, "decode SplitReadStreamRequest: %v", err)
		}
		v1, err := proto.Marshal(&storagepb.SplitReadStreamRequest{Name: req.GetOriginalStream().GetName(), Fraction: float64(req.GetFraction())})
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		out := &innerStream{ctx: ss.Context(), in: v1}
		if err := s.splitStream(ss.Context(), out); err != nil {
			return err
		}
		var resp storagepb.SplitReadStreamResponse
		if err := proto.Unmarshal(out.out, &resp); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		answer := &beta.SplitReadStreamResponse{}
		if n := resp.GetPrimaryStream().GetName(); n != "" {
			answer.PrimaryStream = &beta.Stream{Name: n}
		}
		if n := resp.GetRemainderStream().GetName(); n != "" {
			answer.RemainderStream = &beta.Stream{Name: n}
		}
		return ss.SendMsg(answer)
	case betaFinalizeStream:
		var req beta.FinalizeStreamRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			return status.Errorf(codes.InvalidArgument, "decode FinalizeStreamRequest: %v", err)
		}
		if req.GetStream().GetName() == "" {
			return status.Error(codes.InvalidArgument, "stream.name is required")
		}
		if _, err := s.streamOf(req.GetStream().GetName()); err != nil {
			return err
		}
		return ss.SendMsg(&emptypb.Empty{})
	case betaBatchCreateReadStreams:
		return status.Error(codes.Unimplemented, "Not implemented here: BatchCreateReadSessionStreams. A CloudBurrow read session "+
			"has one stream, its table's whole read, so no stream can be added to it; read the session's stream, or split it "+
			"with SplitReadStream. Nothing was changed.")
	}
	return status.Errorf(codes.Unimplemented, "Not implemented here: %s.", method)
}

// betaCreateSession is v1beta1's CreateReadSession, made as v1's.
func (s *storageRead) betaCreateSession(ss grpc.ServerStream, in rawFrame, open func() (grpc.ClientStream, error)) error {
	var req beta.CreateReadSessionRequest
	if err := proto.Unmarshal(in, &req); err != nil {
		return status.Errorf(codes.InvalidArgument, "decode CreateReadSessionRequest: %v", err)
	}
	tr := req.GetTableReference()
	if tr.GetProjectId() == "" || tr.GetDatasetId() == "" || tr.GetTableId() == "" {
		return status.Error(codes.InvalidArgument, "table_reference must name a project_id, a dataset_id and a table_id")
	}
	if req.GetParent() == "" {
		return status.Error(codes.InvalidArgument, "parent is required")
	}
	format := storagepb.DataFormat_AVRO
	switch req.GetFormat() {
	case beta.DataFormat_DATA_FORMAT_UNSPECIFIED, beta.DataFormat_AVRO:
	case beta.DataFormat_ARROW:
		format = storagepb.DataFormat_ARROW
	default:
		return status.Errorf(codes.InvalidArgument, "format %s: give AVRO or ARROW", req.GetFormat())
	}
	table := "projects/" + tr.GetProjectId() + "/datasets/" + tr.GetDatasetId() + "/tables/" + tr.GetTableId()
	sess := &storagepb.ReadSession{Table: table, DataFormat: format}
	if ro := req.GetReadOptions(); ro != nil {
		sess.ReadOptions = &storagepb.ReadSession_TableReadOptions{SelectedFields: ro.GetSelectedFields(), RowRestriction: ro.GetRowRestriction()}
	}
	if t := req.GetTableModifiers().GetSnapshotTime(); t != nil {
		sess.TableModifiers = &storagepb.ReadSession_TableModifiers{SnapshotTime: t}
	}
	v1, err := proto.Marshal(&storagepb.CreateReadSessionRequest{Parent: req.GetParent(), ReadSession: sess, MaxStreamCount: 1})
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	out := &innerStream{ctx: ss.Context(), in: v1}
	if err := s.createSession(out, open); err != nil {
		return err
	}
	var got storagepb.ReadSession
	if err := proto.Unmarshal(out.out, &got); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	answer := &beta.ReadSession{Name: got.GetName(), ExpireTime: got.GetExpireTime(), TableReference: tr,
		TableModifiers: req.GetTableModifiers(), ShardingStrategy: req.GetShardingStrategy()}
	if a := got.GetAvroSchema(); a != nil {
		answer.Schema = &beta.ReadSession_AvroSchema{AvroSchema: &beta.AvroSchema{Schema: a.GetSchema()}}
	}
	if a := got.GetArrowSchema(); a != nil {
		answer.Schema = &beta.ReadSession_ArrowSchema{ArrowSchema: &beta.ArrowSchema{SerializedSchema: a.GetSerializedSchema()}}
	}
	for _, st := range got.GetStreams() {
		answer.Streams = append(answer.Streams, &beta.Stream{Name: st.GetName()})
	}
	return ss.SendMsg(answer)
}

// betaRows is a v1beta1 ReadRows call's stream, to which serveRows's v1
// answers are sent as v1beta1's.
type betaRows struct{ grpc.ServerStream }

func (b *betaRows) SendMsg(m any) error {
	r, ok := m.(*storagepb.ReadRowsResponse)
	if !ok {
		return b.ServerStream.SendMsg(m)
	}
	out := &beta.ReadRowsResponse{RowCount: r.GetRowCount()}
	if a := r.GetAvroRows(); a != nil {
		out.Rows = &beta.ReadRowsResponse_AvroRows{AvroRows: &beta.AvroRows{SerializedBinaryRows: a.GetSerializedBinaryRows(), RowCount: r.GetRowCount()}}
	}
	if a := r.GetArrowRecordBatch(); a != nil {
		out.Rows = &beta.ReadRowsResponse_ArrowRecordBatch{ArrowRecordBatch: &beta.ArrowRecordBatch{SerializedRecordBatch: a.GetSerializedRecordBatch(), RowCount: r.GetRowCount()}}
	}
	if a := r.GetAvroSchema(); a != nil {
		out.Schema = &beta.ReadRowsResponse_AvroSchema{AvroSchema: &beta.AvroSchema{Schema: a.GetSchema()}}
	}
	if a := r.GetArrowSchema(); a != nil {
		out.Schema = &beta.ReadRowsResponse_ArrowSchema{ArrowSchema: &beta.ArrowSchema{SerializedSchema: a.GetSerializedSchema()}}
	}
	return b.ServerStream.SendMsg(out)
}

// innerStream is a v1 call made within the front: its one request in,
// its one answer out.
type innerStream struct {
	ctx      context.Context
	in, out  []byte
	received bool
}

func (s *innerStream) SetHeader(metadata.MD) error  { return nil }
func (s *innerStream) SendHeader(metadata.MD) error { return nil }
func (s *innerStream) SetTrailer(metadata.MD)       {}
func (s *innerStream) Context() context.Context     { return s.ctx }

func (s *innerStream) SendMsg(m any) error {
	switch v := m.(type) {
	case *rawFrame:
		s.out = append([]byte(nil), *v...)
	case proto.Message:
		b, err := proto.Marshal(v)
		if err != nil {
			return err
		}
		s.out = b
	}
	return nil
}

func (s *innerStream) RecvMsg(m any) error {
	if s.received {
		return io.EOF
	}
	s.received = true
	if f, ok := m.(*rawFrame); ok {
		*f = append((*f)[:0], s.in...)
		return nil
	}
	if p, ok := m.(proto.Message); ok {
		return proto.Unmarshal(s.in, p)
	}
	return status.Error(codes.Internal, "bigqueryfront: an inner call's request type")
}
