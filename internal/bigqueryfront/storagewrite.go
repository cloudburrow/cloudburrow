package bigqueryfront

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// The Storage Write API (#1102), served by the front on the Storage Read
// API's port (the Service's 9060): the emulator's Storage Write API is
// never called.
//
// Why. Measured against the pinned emulator (v0.8.1) through the front,
// which passed the Write API through, with the official Go clients
// (cloud.google.com/go/bigquery/storage/managedwriter and the raw
// apiv1 BigQueryWriteClient):
//
//   - Five requests panicked its gRPC handler (server/storage_handler.go),
//     which ends the process: Kubernetes restarted the container and every
//     dataset, table and job of the instance was gone, as the emulator
//     keeps them in memory (#1095). They were AppendRows naming no
//     write_stream when no stream exists (nil status, :497); a FIXED64
//     field ("type mismatch: cannot convert uint64 to float", :644);
//     FlushRows with no offset (:809) and FlushRows past the stream's rows
//     ("slice bounds out of range", :819), both of which the client
//     retries, so the retry found the stream gone; and CreateReadSession of
//     a table in a project the emulator does not have (getTableMetadata,
//     :919, which storageread.go now refuses first).
//   - A request after the first on an AppendRows connection, which the
//     managed writer sends without write_stream (its simplexOptimizer),
//     was written to whichever stream the emulator's map gave first:
//     measured, rows appended to a COMMITTED stream of one table landed in
//     another table's default stream.
//   - The offsets: an append's AppendResult.offset was the request's
//     offset plus its row count, not the append's first row; an offset
//     already written was accepted again; the default stream reported
//     offsets.
//   - The values: a NUMERIC in BigDecimalByteStringEncoder's bytes was
//     stored as 0, a DATETIME or TIME in CivilTimeEncoder's packed int64
//     as another value ("6483-06-17T05:36:51.664"), a BOOL as 1.
//   - The commit semantics, read in its source: FinalizeWriteStream of a
//     COMMITTED stream counts 0 rows; BatchCommitWriteStreams commits a
//     stream that is not finalized and commits it again when asked again;
//     FlushRows writes every row up to the offset each time.
//
// So the front keeps each write stream itself and writes its rows to the
// table through the REST front's tabledata.insertAll (front.go), whose
// checks and value fixes every streamed row then has. The semantics are
// storage.proto's (googleapis/google/cloud/bigquery/storage/v1):
//
//   - CreateWriteStream: a COMMITTED, PENDING or BUFFERED stream of an
//     existing table; any other type is INVALID_ARGUMENT, a missing table
//     NOT_FOUND. The table's schema is returned.
//   - GetWriteStream: a stream the front made, or a table's "_default"
//     stream (COMMITTED); the schema only in the FULL view.
//   - AppendRows: write_stream and proto_rows.writer_schema are required
//     in the first request of a connection and may be left out of the
//     rest, which use the connection's. Rows are proto_rows only
//     (arrow_rows is UNIMPLEMENTED). An offset must be the stream's next
//     row: ALREADY_EXISTS below it, OUT_OF_RANGE above it, and no offset
//     is allowed on the default stream. The result's offset is the append's
//     first row; the default stream's has none. A row that does not decode
//     or convert fails the whole append with INVALID_ARGUMENT and its
//     row_errors. A COMMITTED stream's rows are in the table when the
//     append is answered; a PENDING stream's at BatchCommitWriteStreams; a
//     BUFFERED stream's at FlushRows. An append to a finalized stream is
//     INVALID_ARGUMENT (STREAM_FINALIZED). missing_value_interpretations of
//     DEFAULT_VALUE is UNIMPLEMENTED: column defaults are not applied here.
//   - FinalizeWriteStream: row_count is the rows appended; the default
//     stream cannot be finalized.
//   - BatchCommitWriteStreams: every stream must be a finalized PENDING
//     stream of the parent table, not yet committed; otherwise its
//     stream_errors are returned and none is committed.
//   - FlushRows: a BUFFERED stream's rows up to and including offset are
//     written, once each; an offset past the rows appended is OUT_OF_RANGE.
//
// A stream lives in the front's memory: after the front restarts, a stream
// made before is NOT_FOUND (the default stream is always there). Rows are
// written as a streamed insert is, so what the emulator cannot store from
// insertAll (a RECORD in a RECORD with a REPEATED one among them,
// unstorableNesting) is UNIMPLEMENTED here too.

const writeService = "/google.cloud.bigquery.storage.v1.BigQueryWrite/"

// insertAllBatch bounds the JSON of the rows one insertAll carries, below
// the REST front's request limit.
const insertAllBatch = 4 << 20

// storageWrite is the Storage Write front.
type storageWrite struct {
	// rest is the REST front (Wrap's handler): tables.get and
	// tabledata.insertAll go through it.
	rest http.Handler
	now  func() time.Time

	mu      sync.Mutex
	streams map[string]*writeStream
	order   []string
}

// writeStream is a write stream the front made, or a table's default
// stream.
type writeStream struct {
	name    string
	table   readTable
	typ     storagepb.WriteStream_Type
	isDef   bool
	created time.Time

	mu sync.Mutex
	// rows is the number of rows appended: the stream's next offset.
	rows int64
	// held are the rows appended and not yet in the table (PENDING until
	// committed, BUFFERED until flushed), from offset rows-len(held).
	held      []json.RawMessage
	finalized bool
	committed time.Time
}

func newStorageWrite(rest http.Handler) *storageWrite {
	return &storageWrite{rest: rest, now: time.Now, streams: map[string]*writeStream{}}
}

// handle serves one call of the Write API. A panic is the call's
// INTERNAL error: it never ends the front's process, which keeps the
// streams.
func (w *storageWrite) handle(ss grpc.ServerStream, method string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = status.Errorf(codes.Internal, "CloudBurrow could not serve %s: %v", method, p)
		}
	}()
	if w == nil || w.rest == nil {
		return status.Error(codes.Unimplemented, "Not implemented here: the Storage Write API is not served by this front.")
	}
	ctx := ss.Context()
	switch strings.TrimPrefix(method, writeService) {
	case "AppendRows":
		return w.appendRows(ss)
	case "CreateWriteStream":
		var req storagepb.CreateWriteStreamRequest
		if err := recvRequest(ss, &req); err != nil {
			return err
		}
		resp, err := w.createStream(ctx, &req)
		return reply(ss, resp, err)
	case "GetWriteStream":
		var req storagepb.GetWriteStreamRequest
		if err := recvRequest(ss, &req); err != nil {
			return err
		}
		resp, err := w.getStream(ctx, &req)
		return reply(ss, resp, err)
	case "FinalizeWriteStream":
		var req storagepb.FinalizeWriteStreamRequest
		if err := recvRequest(ss, &req); err != nil {
			return err
		}
		resp, err := w.finalize(ctx, &req)
		return reply(ss, resp, err)
	case "BatchCommitWriteStreams":
		var req storagepb.BatchCommitWriteStreamsRequest
		if err := recvRequest(ss, &req); err != nil {
			return err
		}
		resp, err := w.commit(ctx, &req)
		return reply(ss, resp, err)
	case "FlushRows":
		var req storagepb.FlushRowsRequest
		if err := recvRequest(ss, &req); err != nil {
			return err
		}
		resp, err := w.flush(ctx, &req)
		return reply(ss, resp, err)
	}
	return status.Errorf(codes.Unimplemented, "Not implemented here: %s", method)
}

// recvRequest reads the one request of a unary call into m.
func recvRequest(ss grpc.ServerStream, m proto.Message) error {
	var in rawFrame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	if err := proto.Unmarshal(in, m); err != nil {
		return status.Errorf(codes.InvalidArgument, "decode %s: %v", m.ProtoReflect().Descriptor().Name(), err)
	}
	return nil
}

// reply sends a unary call's answer, or returns its error.
func reply(ss grpc.ServerStream, m proto.Message, err error) error {
	if err != nil {
		return err
	}
	return ss.SendMsg(m)
}

// parseStreamName reads projects/{p}/datasets/{d}/tables/{t}/streams/{s}.
func parseStreamName(name string) (readTable, string, bool) {
	i := strings.LastIndex(name, "/streams/")
	if i < 0 {
		return readTable{}, "", false
	}
	t, ok := parseReadTable(name[:i])
	id := name[i+len("/streams/"):]
	if !ok || id == "" || strings.Contains(id, "/") {
		return readTable{}, "", false
	}
	return t, id, true
}

func (t readTable) path() string {
	return "projects/" + t.project + "/datasets/" + t.dataset + "/tables/" + t.table
}

// tableFields reads t's schema through the REST front: NOT_FOUND when
// there is no such table.
func (w *storageWrite) tableFields(ctx context.Context, t readTable) ([]field, error) {
	code, body := w.call(ctx, http.MethodGet, t, "", nil)
	if code != http.StatusOK {
		return nil, restStatus(code, body, "the table "+t.path())
	}
	var meta struct {
		Schema tableSchema `json:"schema"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, status.Errorf(codes.Internal, "read the table %s: %v", t.path(), err)
	}
	return meta.Schema.Fields, nil
}

// call sends a request of the REST front for the table t (tables.get, or
// suffix "/insertAll") and returns its status and body.
func (w *storageWrite) call(ctx context.Context, method string, t readTable, suffix string, body []byte) (int, []byte) {
	p := "/bigquery/v2/projects/" + url.PathEscape(t.project) + "/datasets/" + url.PathEscape(t.dataset) +
		"/tables/" + url.PathEscape(t.table) + suffix
	r, err := http.NewRequestWithContext(ctx, method, "http://bigquery"+p, bytes.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, []byte(err.Error())
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	rec := newRecorder()
	w.rest.ServeHTTP(rec, r)
	code := rec.status
	if code == 0 {
		code = http.StatusOK
	}
	return code, rec.body.Bytes()
}

// restStatus is the gRPC status of a REST answer that is not 200.
func restStatus(code int, body []byte, what string) error {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	c := codes.Unavailable
	switch code {
	case http.StatusBadRequest:
		c = codes.InvalidArgument
	case http.StatusNotFound:
		c = codes.NotFound
	case http.StatusConflict:
		c = codes.AlreadyExists
	case http.StatusNotImplemented:
		c = codes.Unimplemented
	case http.StatusInternalServerError:
		c = codes.Internal
	}
	return status.Errorf(c, "%s: %s", what, msg)
}

// storageSchema writes a REST TableSchema's fields as the Storage API's.
func storageSchema(fields []field) *storagepb.TableSchema {
	return &storagepb.TableSchema{Fields: storageFields(fields)}
}

func storageFields(fields []field) []*storagepb.TableFieldSchema {
	out := make([]*storagepb.TableFieldSchema, 0, len(fields))
	for _, f := range fields {
		typ := map[string]storagepb.TableFieldSchema_Type{
			"STRING": storagepb.TableFieldSchema_STRING, "INTEGER": storagepb.TableFieldSchema_INT64,
			"FLOAT": storagepb.TableFieldSchema_DOUBLE, "RECORD": storagepb.TableFieldSchema_STRUCT,
			"BYTES": storagepb.TableFieldSchema_BYTES, "BOOLEAN": storagepb.TableFieldSchema_BOOL,
			"TIMESTAMP": storagepb.TableFieldSchema_TIMESTAMP, "DATE": storagepb.TableFieldSchema_DATE,
			"TIME": storagepb.TableFieldSchema_TIME, "DATETIME": storagepb.TableFieldSchema_DATETIME,
			"GEOGRAPHY": storagepb.TableFieldSchema_GEOGRAPHY, "NUMERIC": storagepb.TableFieldSchema_NUMERIC,
			"BIGNUMERIC": storagepb.TableFieldSchema_BIGNUMERIC, "JSON": storagepb.TableFieldSchema_JSON,
			"INTERVAL": storagepb.TableFieldSchema_INTERVAL, "RANGE": storagepb.TableFieldSchema_RANGE,
		}[legacyType(f.Type)]
		mode := map[string]storagepb.TableFieldSchema_Mode{
			"NULLABLE": storagepb.TableFieldSchema_NULLABLE, "REQUIRED": storagepb.TableFieldSchema_REQUIRED,
			"REPEATED": storagepb.TableFieldSchema_REPEATED,
		}[modeOf(f)]
		out = append(out, &storagepb.TableFieldSchema{Name: f.Name, Type: typ, Mode: mode, Fields: storageFields(f.Fields)})
	}
	return out
}

func newStreamID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "cb" + hex.EncodeToString(b)
}

func (w *storageWrite) createStream(ctx context.Context, req *storagepb.CreateWriteStreamRequest) (*storagepb.WriteStream, error) {
	t, ok := parseReadTable(req.GetParent())
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "parent %q is not projects/{project}/datasets/{dataset}/tables/{table}", req.GetParent())
	}
	switch typ := req.GetWriteStream().GetType(); typ {
	case storagepb.WriteStream_COMMITTED, storagepb.WriteStream_PENDING, storagepb.WriteStream_BUFFERED:
	default:
		return nil, status.Errorf(codes.InvalidArgument, "write_stream.type %s: give COMMITTED, PENDING or BUFFERED", typ)
	}
	if m := req.GetWriteStream().GetWriteMode(); m != storagepb.WriteStream_WRITE_MODE_UNSPECIFIED && m != storagepb.WriteStream_INSERT {
		return nil, status.Errorf(codes.InvalidArgument, "write_stream.write_mode %s: only INSERT is a write mode", m)
	}
	fields, err := w.tableFields(ctx, t)
	if err != nil {
		return nil, err
	}
	st := &writeStream{
		name:    t.path() + "/streams/" + newStreamID(),
		table:   t,
		typ:     req.GetWriteStream().GetType(),
		created: w.now(),
	}
	w.remember(st)
	return st.resource(fields, true), nil
}

// resource is the stream as WriteStream, with the table's schema when full.
func (st *writeStream) resource(fields []field, full bool) *storagepb.WriteStream {
	st.mu.Lock()
	defer st.mu.Unlock()
	ws := &storagepb.WriteStream{Name: st.name, Type: st.typ, WriteMode: storagepb.WriteStream_INSERT}
	if !st.isDef {
		ws.CreateTime = timestamppb.New(st.created)
	}
	// "If the stream is of COMMITTED type, then it will have a
	// commit_time same as create_time" (stream.proto).
	switch {
	case st.typ == storagepb.WriteStream_COMMITTED && !st.isDef:
		ws.CommitTime = timestamppb.New(st.created)
	case !st.committed.IsZero():
		ws.CommitTime = timestamppb.New(st.committed)
	}
	if full {
		ws.TableSchema = storageSchema(fields)
	}
	return ws
}

func (w *storageWrite) remember(st *writeStream) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.streams[st.name]; !ok {
		w.order = append(w.order, st.name)
	}
	w.streams[st.name] = st
	if len(w.order) >= maxStreams {
		drop := w.order[:len(w.order)/2]
		for _, n := range drop {
			delete(w.streams, n)
		}
		w.order = append([]string(nil), w.order[len(drop):]...)
	}
}

// stream finds the stream name: one the front made, or a table's
// "_default" stream when the table exists.
func (w *storageWrite) stream(ctx context.Context, name string) (*writeStream, error) {
	t, id, ok := parseStreamName(name)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "write stream %q is not projects/{project}/datasets/{dataset}/tables/{table}/streams/{stream}", name)
	}
	w.mu.Lock()
	st, found := w.streams[name]
	w.mu.Unlock()
	if found {
		return st, nil
	}
	if id != "_default" {
		return nil, status.Errorf(codes.NotFound, "Requested entity was not found: write stream %s. CloudBurrow keeps the streams "+
			"it made in the BigQuery front's memory: a stream made before the front last started is gone.", name)
	}
	if _, err := w.tableFields(ctx, t); err != nil {
		return nil, err
	}
	st = &writeStream{name: name, table: t, typ: storagepb.WriteStream_COMMITTED, isDef: true, created: w.now()}
	w.mu.Lock()
	if prev, ok := w.streams[name]; ok {
		st = prev
	}
	w.mu.Unlock()
	w.remember(st)
	return st, nil
}

func (w *storageWrite) getStream(ctx context.Context, req *storagepb.GetWriteStreamRequest) (*storagepb.WriteStream, error) {
	st, err := w.stream(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	full := req.GetView() == storagepb.WriteStreamView_FULL
	var fields []field
	if full {
		if fields, err = w.tableFields(ctx, st.table); err != nil {
			return nil, err
		}
	}
	return st.resource(fields, full), nil
}

func (w *storageWrite) finalize(ctx context.Context, req *storagepb.FinalizeWriteStreamRequest) (*storagepb.FinalizeWriteStreamResponse, error) {
	st, err := w.stream(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	if st.isDef {
		return nil, status.Errorf(codes.InvalidArgument, "The _default stream cannot be finalized: %s", st.name)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.finalized = true
	return &storagepb.FinalizeWriteStreamResponse{RowCount: st.rows}, nil
}

func storageError(code storagepb.StorageError_StorageErrorCode, entity, format string, args ...any) *storagepb.StorageError {
	return &storagepb.StorageError{Code: code, Entity: entity, ErrorMessage: fmt.Sprintf(format, args...)}
}

func (w *storageWrite) commit(ctx context.Context, req *storagepb.BatchCommitWriteStreamsRequest) (*storagepb.BatchCommitWriteStreamsResponse, error) {
	t, ok := parseReadTable(req.GetParent())
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "parent %q is not projects/{project}/datasets/{dataset}/tables/{table}", req.GetParent())
	}
	if len(req.GetWriteStreams()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "write_streams is required")
	}
	var (
		streams []*writeStream
		errs    []*storagepb.StorageError
		seen    = map[string]bool{}
	)
	for _, name := range req.GetWriteStreams() {
		if seen[name] {
			continue
		}
		seen[name] = true
		w.mu.Lock()
		st, found := w.streams[name]
		w.mu.Unlock()
		if !found || st.isDef {
			errs = append(errs, storageError(storagepb.StorageError_STREAM_NOT_FOUND, name, "Stream is not found: %s", name))
			continue
		}
		if st.table != t {
			errs = append(errs, storageError(storagepb.StorageError_INVALID_STREAM_STATE, name, "Stream %s is not of the table %s", name, t.path()))
			continue
		}
		if st.typ != storagepb.WriteStream_PENDING {
			errs = append(errs, storageError(storagepb.StorageError_INVALID_STREAM_TYPE, name, "Stream %s is of type %s: only a PENDING stream is committed", name, st.typ))
			continue
		}
		streams = append(streams, st)
	}
	sort.Slice(streams, func(a, b int) bool { return streams[a].name < streams[b].name })
	for _, st := range streams {
		st.mu.Lock()
		defer st.mu.Unlock()
		switch {
		case !st.committed.IsZero():
			errs = append(errs, storageError(storagepb.StorageError_STREAM_ALREADY_COMMITTED, st.name, "Stream is already committed: %s", st.name))
		case !st.finalized:
			errs = append(errs, storageError(storagepb.StorageError_INVALID_STREAM_STATE, st.name, "Stream is not finalized: %s", st.name))
		}
	}
	if len(errs) > 0 {
		// "If non empty, certain streams have errors and ZERO stream is
		// committed due to atomicity guarantee" (storage.proto).
		return &storagepb.BatchCommitWriteStreamsResponse{StreamErrors: errs}, nil
	}
	// Every stream's rows go to the table together, in the order of the
	// request's streams.
	var rows []json.RawMessage
	for _, name := range req.GetWriteStreams() {
		for _, st := range streams {
			if st.name == name {
				rows = append(rows, st.held...)
			}
		}
	}
	if err := w.insert(ctx, t, rows); err != nil {
		return nil, err
	}
	now := w.now()
	for _, st := range streams {
		st.held, st.committed = nil, now
	}
	return &storagepb.BatchCommitWriteStreamsResponse{CommitTime: timestamppb.New(now)}, nil
}

func (w *storageWrite) flush(ctx context.Context, req *storagepb.FlushRowsRequest) (*storagepb.FlushRowsResponse, error) {
	st, err := w.stream(ctx, req.GetWriteStream())
	if err != nil {
		return nil, err
	}
	if st.typ != storagepb.WriteStream_BUFFERED || st.isDef {
		return nil, status.Errorf(codes.InvalidArgument, "FlushRows is for a BUFFERED stream; %s is %s", st.name, st.typ)
	}
	if req.GetOffset() == nil {
		return nil, status.Error(codes.InvalidArgument, "offset is required: the row to flush the stream up to, and including")
	}
	off := req.GetOffset().GetValue()
	st.mu.Lock()
	defer st.mu.Unlock()
	if off < 0 || off >= st.rows {
		return nil, status.Errorf(codes.OutOfRange, "offset %d is not a row of the stream %s, which has %d rows", off, st.name, st.rows)
	}
	flushed := st.rows - int64(len(st.held))
	if off >= flushed {
		n := off - flushed + 1
		if err := w.insert(ctx, st.table, st.held[:n]); err != nil {
			return nil, err
		}
		st.held = append([]json.RawMessage(nil), st.held[n:]...)
	}
	return &storagepb.FlushRowsResponse{Offset: off}, nil
}

// insert writes rows to t with tabledata.insertAll of the REST front, in
// batches of at most insertAllBatch bytes.
func (w *storageWrite) insert(ctx context.Context, t readTable, rows []json.RawMessage) error {
	for len(rows) > 0 {
		n, size := 0, 0
		for n < len(rows) && (n == 0 || size+len(rows[n]) <= insertAllBatch) {
			size += len(rows[n]) + 16
			n++
		}
		if err := w.insertBatch(ctx, t, rows[:n]); err != nil {
			return err
		}
		rows = rows[n:]
	}
	return nil
}

func (w *storageWrite) insertBatch(ctx context.Context, t readTable, rows []json.RawMessage) error {
	type insertRow struct {
		JSON json.RawMessage `json:"json"`
	}
	req := struct {
		Rows []insertRow `json:"rows"`
	}{}
	for _, r := range rows {
		req.Rows = append(req.Rows, insertRow{JSON: r})
	}
	body, err := json.Marshal(req)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	code, got := w.call(ctx, http.MethodPost, t, "/insertAll", body)
	if code != http.StatusOK {
		return restStatus(code, got, "writing the rows to "+t.path())
	}
	var resp insertResponse
	if err := json.Unmarshal(got, &resp); err != nil {
		return status.Errorf(codes.Internal, "writing the rows to %s: %v", t.path(), err)
	}
	if len(resp.InsertErrors) > 0 {
		var msgs []string
		for _, e := range resp.InsertErrors {
			for _, re := range e.Errors {
				if re.Reason != "stopped" {
					msgs = append(msgs, fmt.Sprintf("row %d: %s", e.Index, re.Message))
				}
			}
		}
		return status.Errorf(codes.InvalidArgument, "writing the rows to %s: %s", t.path(), strings.Join(msgs, "; "))
	}
	return nil
}

// appendConn is what an AppendRows connection carries from one request to
// the next: its stream and writer schema, and the rows' converters.
type appendConn struct {
	stream string
	schema *descriptorpb.DescriptorProto
	convs  map[string]convResult
}

type convResult struct {
	conv   *rowConv
	fields []field
	err    error
}

// appendRows serves an AppendRows connection: one answer to each request,
// in order. A request the connection cannot be read past (no stream, no
// writer schema, no rows) ends it; an append that fails is answered with
// its error, and the connection goes on.
func (w *storageWrite) appendRows(ss grpc.ServerStream) error {
	c := &appendConn{convs: map[string]convResult{}}
	for {
		var in rawFrame
		if err := ss.RecvMsg(&in); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var req storagepb.AppendRowsRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			return status.Errorf(codes.InvalidArgument, "decode AppendRowsRequest: %v", err)
		}
		resp, err := w.appendOne(ss.Context(), c, &req)
		if err != nil {
			return err
		}
		if err := ss.SendMsg(resp); err != nil {
			return err
		}
	}
}

// appendFailed is an append's answer with its error.
func appendFailed(stream string, err error, details ...protoadapt.MessageV1) *storagepb.AppendRowsResponse {
	s := status.Convert(err)
	if len(details) > 0 {
		if d, derr := s.WithDetails(details...); derr == nil {
			s = d
		}
	}
	return &storagepb.AppendRowsResponse{WriteStream: stream, Response: &storagepb.AppendRowsResponse_Error{Error: s.Proto()}}
}

func (w *storageWrite) appendOne(ctx context.Context, c *appendConn, req *storagepb.AppendRowsRequest) (*storagepb.AppendRowsResponse, error) {
	name := req.GetWriteStream()
	if name == "" {
		if name = c.stream; name == "" {
			return nil, status.Error(codes.InvalidArgument, "write_stream is required in the first request of an AppendRows connection")
		}
	}
	pr := req.GetProtoRows()
	if pr == nil {
		if req.GetArrowRows() != nil {
			return nil, status.Error(codes.Unimplemented, "Not implemented here: AppendRows with arrow_rows. Nothing was written. "+
				"Send the rows as proto_rows.")
		}
		return nil, status.Error(codes.InvalidArgument, "proto_rows is required: the rows to append")
	}
	if ws := pr.GetWriterSchema(); ws != nil {
		if ws.GetProtoDescriptor() == nil {
			return nil, status.Error(codes.InvalidArgument, "proto_rows.writer_schema.proto_descriptor is required")
		}
		if c.schema == nil || !proto.Equal(c.schema, ws.GetProtoDescriptor()) {
			c.schema, c.convs = ws.GetProtoDescriptor(), map[string]convResult{}
		}
	} else if c.schema == nil {
		return nil, status.Error(codes.InvalidArgument, "proto_rows.writer_schema is required in the first request of an AppendRows connection")
	}
	st, err := w.stream(ctx, name)
	if err != nil {
		return nil, err
	}
	c.stream = name

	for col, mvi := range req.GetMissingValueInterpretations() {
		if mvi == storagepb.AppendRowsRequest_DEFAULT_VALUE {
			return appendFailed(name, status.Errorf(codes.Unimplemented, "Not implemented here: missing_value_interpretations "+
				"DEFAULT_VALUE (column %s). CloudBurrow does not apply column default values. Nothing was written.", col)), nil
		}
	}
	if req.GetDefaultMissingValueInterpretation() == storagepb.AppendRowsRequest_DEFAULT_VALUE {
		return appendFailed(name, status.Error(codes.Unimplemented, "Not implemented here: default_missing_value_interpretation "+
			"DEFAULT_VALUE. CloudBurrow does not apply column default values. Nothing was written.")), nil
	}

	cr, ok := c.convs[name]
	if !ok {
		cr.fields, cr.err = w.tableFields(ctx, st.table)
		if cr.err == nil {
			var md, derr = writerDescriptor(c.schema)
			if derr != nil {
				cr.err = derr
			} else {
				cr.conv, cr.err = newRowConv(md, cr.fields, "")
			}
		}
		c.convs[name] = cr
	}
	if cr.err != nil {
		return appendFailed(name, cr.err), nil
	}

	rows, rowErrs, err := convertRows(cr.conv, cr.fields, pr.GetRows().GetSerializedRows())
	if err != nil {
		return appendFailed(name, err), nil
	}
	if len(rowErrs) > 0 {
		resp := appendFailed(name, status.Error(codes.InvalidArgument,
			"Errors found while processing rows. Please refer to the row_errors field for details. The list may not be complete "+
				"because of the size limitations. Entity: "+name))
		resp.RowErrors = rowErrs
		return resp, nil
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.finalized || !st.committed.IsZero() {
		return appendFailed(name, status.Errorf(codes.InvalidArgument, "Stream is finalized: %s", name),
			protoadapt.MessageV1Of(storageError(storagepb.StorageError_STREAM_FINALIZED, name, "Stream is finalized"))), nil
	}
	if off := req.GetOffset(); off != nil {
		if st.isDef {
			return appendFailed(name, status.Errorf(codes.InvalidArgument, "an offset cannot be given for the _default stream: %s", name)), nil
		}
		switch v := off.GetValue(); {
		case v < st.rows:
			return appendFailed(name, status.Errorf(codes.AlreadyExists, "The offset is within stream, expected offset %d, received %d", st.rows, v),
				protoadapt.MessageV1Of(storageError(storagepb.StorageError_OFFSET_ALREADY_EXISTS, name, "offset %d already exists", v))), nil
		case v > st.rows:
			return appendFailed(name, status.Errorf(codes.OutOfRange, "The offset is beyond stream, expected offset %d, received %d", st.rows, v),
				protoadapt.MessageV1Of(storageError(storagepb.StorageError_OFFSET_OUT_OF_RANGE, name, "offset %d is beyond the stream's end", v))), nil
		}
	}
	start := st.rows
	if st.typ == storagepb.WriteStream_COMMITTED {
		if err := w.insert(ctx, st.table, rows); err != nil {
			return appendFailed(name, err), nil
		}
	} else {
		st.held = append(st.held, rows...)
	}
	st.rows += int64(len(rows))
	result := &storagepb.AppendRowsResponse_AppendResult{}
	if !st.isDef {
		// "The row offset at which the last append occurred. The offset
		// will not be set if appending using default streams."
		result.Offset = wrapperspb.Int64(start)
	}
	return &storagepb.AppendRowsResponse{WriteStream: name, Response: &storagepb.AppendRowsResponse_AppendResult_{AppendResult: result}}, nil
}

// convertRows writes each serialized row as insertAll's JSON, checked as
// insertAll checks a row, so that a PENDING or BUFFERED stream's rows fail
// when appended, as BigQuery's do. A row that fails is a row error; a
// value the emulator cannot store is an UNIMPLEMENTED error.
func convertRows(rc *rowConv, fields []field, serialized [][]byte) ([]json.RawMessage, []*storagepb.RowError, error) {
	rows := make([]json.RawMessage, 0, len(serialized))
	var rowErrs []*storagepb.RowError
	fail := func(i int, format string, args ...any) {
		rowErrs = append(rowErrs, &storagepb.RowError{Index: int64(i), Code: storagepb.RowError_FIELDS_ERROR, Message: fmt.Sprintf(format, args...)})
	}
	for i, b := range serialized {
		obj, err := rc.row(b)
		if err != nil {
			fail(i, "%v", err)
			continue
		}
		raw, err := json.Marshal(obj)
		if err != nil {
			fail(i, "%v", err)
			continue
		}
		var check map[string]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&check); err != nil {
			fail(i, "%v", err)
			continue
		}
		if loc := unstorableNesting(fields, check, "", false, false); loc != "" {
			return nil, nil, status.Errorf(codes.Unimplemented, "Not implemented here: the row at index %d holds a value in %s, a "+
				"RECORD nested in a RECORD with a REPEATED one among them. BigQuery accepts it, but the emulator behind CloudBurrow "+
				"cannot read a table back once such a value is streamed into it. Nothing was written.", i, loc)
		}
		if _, errs := checkRow(fields, check, false); len(errs) > 0 {
			var msgs []string
			for _, e := range errs {
				msgs = append(msgs, e.Message)
			}
			fail(i, "%s", strings.Join(msgs, "; "))
			continue
		}
		if _, p := fixValues(fields, check, fmt.Sprintf("the row at index %d", i), ""); p != nil {
			c := codes.InvalidArgument
			if p.code == http.StatusNotImplemented {
				c = codes.Unimplemented
			}
			return nil, nil, status.Error(c, p.msg)
		}
		rows = append(rows, raw)
	}
	return rows, rowErrs, nil
}
