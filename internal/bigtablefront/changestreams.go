package bigtablefront

// Change streams (#1523). The emulator keeps no log of the mutations it
// applies, so the front keeps one for each table whose change stream is
// enabled (`change_stream_config` on CreateTable or UpdateTable): it
// decodes the MutateRow, MutateRows, CheckAndMutateRow and
// ReadModifyWriteRow calls it forwards to such a table, and logs what the
// emulator applied once it answers OK, as Google's change stream records
// them:
//
//   - a SetCell with the server-assigned timestamp (-1) is given the time
//     before it is forwarded, so the logged cell is the stored one;
//   - DeleteFromRow is a DeleteFromFamily for each column family;
//   - ReadModifyWriteRow is a SetCell of each cell it wrote, with its new
//     value and timestamp;
//   - CheckAndMutateRow logs the mutations of the branch that ran.
//
// DropRowRange is not recorded, as Google documents. Garbage collection
// does run in the emulator, but out of the front's sight: no
// GARBAGE_COLLECTION record is written.
//
// The writes to a table with a change stream are applied one at a time, so
// the log's order is the emulator's; each gets a commit timestamp later
// than the one before. The log is kept for the table's retention period
// with the front's other state (state.go): full segments of
// segmentRecords as copy files, the rest in the records.
//
// The stream has a single partition, the whole row key space, which the
// API allows (a table's partitions follow its tablets). ReadChangeStream
// serves DataChange records, chunking a SetCell value larger than
// maxChunkBytes, heartbeats every heartbeat_duration (5 seconds by
// default), continuation tokens and low watermarks, ends OK past end_time,
// and closes the stream with the whole-table partition for a request on any
// other partition.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

const (
	minRetention = 24 * time.Hour
	maxRetention = 7 * 24 * time.Hour
	// defaultHeartbeat is heartbeat_duration's documented default.
	defaultHeartbeat = 5 * time.Second
	segmentRecords   = 256
	tokenPrefix      = "cb1:"
)

// maxChunkBytes is the largest SetCell value sent in one chunk; a variable
// so the tests can chunk small values.
var maxChunkBytes = 1 << 20

// changeSegment is a full, immutable run of a log's records.
type changeSegment struct {
	recs []*btpb.ReadChangeStreamResponse_DataChange
}

// tableLog is one table's change log.
type tableLog struct {
	retention time.Duration
	since     time.Time // when the change stream was enabled
	segs      []*changeSegment
	tail      []*btpb.ReadChangeStreamResponse_DataChange
	nextSeq   int64 // the seq of the next record; records start at 1
	trimmed   int64 // the last seq dropped by retention
	last      time.Time
	// write serializes the writes to the table, so the log's order is
	// the emulator's.
	write sync.Mutex
	// wake is closed, and replaced, when a record is added or the log
	// goes.
	wake chan struct{}
	gone bool
}

// changeStreams is the change logs, by table name.
type changeStreams struct {
	mu     sync.Mutex
	tables map[string]*tableLog
	now    func() time.Time
}

func newChangeStreams() *changeStreams {
	return &changeStreams{tables: map[string]*tableLog{}, now: time.Now}
}

func recSeq(r *btpb.ReadChangeStreamResponse_DataChange) int64 {
	n, _ := strconv.ParseInt(strings.TrimPrefix(r.GetToken(), tokenPrefix), 10, 64)
	return n
}

func encodeToken(seq int64) string { return tokenPrefix + strconv.FormatInt(seq, 10) }

func decodeToken(tok string) (int64, error) {
	if !strings.HasPrefix(tok, tokenPrefix) {
		return 0, grpcstatus.Errorf(codes.InvalidArgument, "invalid continuation token %q", tok)
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(tok, tokenPrefix), 10, 64)
	if err != nil || n < 0 {
		return 0, grpcstatus.Errorf(codes.InvalidArgument, "invalid continuation token %q", tok)
	}
	return n, nil
}

// checkRetention checks a change_stream_config's retention period.
func checkRetention(c *btapb.ChangeStreamConfig) (time.Duration, error) {
	d := c.GetRetentionPeriod()
	if d == nil {
		return 0, grpcstatus.Error(codes.InvalidArgument, "change_stream_config.retention_period is required")
	}
	if err := d.CheckValid(); err != nil {
		return 0, grpcstatus.Errorf(codes.InvalidArgument, "change_stream_config.retention_period: %v", err)
	}
	r := d.AsDuration()
	if r < minRetention || r > maxRetention {
		return 0, grpcstatus.Errorf(codes.InvalidArgument, "change_stream_config.retention_period %s must be between 1 and 7 days", r)
	}
	return r, nil
}

// enabled reports whether any table has a change stream.
func (c *changeStreams) enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.tables) > 0
}

// config is the table's change_stream_config, or nil.
func (c *changeStreams) config(table string) *btapb.ChangeStreamConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.tables[table]; ok {
		return &btapb.ChangeStreamConfig{RetentionPeriod: durationpb.New(l.retention)}
	}
	return nil
}

// set enables the table's change stream with retention, or changes its
// retention; 0 disables it, dropping its log.
func (c *changeStreams) set(table string, retention time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.tables[table]
	switch {
	case retention == 0:
		if ok {
			c.dropLocked(table)
		}
	case ok:
		l.retention = retention
		c.trimLocked(l)
	default:
		c.tables[table] = &tableLog{retention: retention, since: c.now().UTC().Truncate(time.Microsecond),
			nextSeq: 1, wake: make(chan struct{})}
	}
}

func (c *changeStreams) dropLocked(table string) {
	if l, ok := c.tables[table]; ok {
		l.gone = true
		close(l.wake)
		delete(c.tables, table)
	}
}

// drop forgets the logs of name and of every table under it.
func (c *changeStreams) drop(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for t := range c.tables {
		if t == name || strings.HasPrefix(t, name+"/") {
			c.dropLocked(t)
		}
	}
}

// trimLocked drops the records older than the retention period.
func (c *changeStreams) trimLocked(l *tableLog) {
	cutoff := c.now().Add(-l.retention)
	for len(l.segs) > 0 {
		recs := l.segs[0].recs
		if len(recs) > 0 && !recs[len(recs)-1].GetCommitTimestamp().AsTime().Before(cutoff) {
			break
		}
		if len(recs) > 0 {
			l.trimmed = recSeq(recs[len(recs)-1])
		}
		l.segs = l.segs[1:]
	}
	if len(l.segs) == 0 {
		for len(l.tail) > 0 && l.tail[0].GetCommitTimestamp().AsTime().Before(cutoff) {
			l.trimmed = recSeq(l.tail[0])
			l.tail = l.tail[1:]
		}
	}
}

// append logs one row's change. c.mu is held.
func (c *changeStreams) appendLocked(table string, l *tableLog, row []byte, ms []*btpb.Mutation, cluster string) {
	if len(ms) == 0 {
		return
	}
	at := c.now().UTC().Truncate(time.Microsecond)
	if !at.After(l.last) {
		at = l.last.Add(time.Microsecond)
	}
	l.last = at
	chunks := make([]*btpb.ReadChangeStreamResponse_MutationChunk, len(ms))
	for i, m := range ms {
		chunks[i] = &btpb.ReadChangeStreamResponse_MutationChunk{Mutation: m}
	}
	rec := &btpb.ReadChangeStreamResponse_DataChange{
		Type: btpb.ReadChangeStreamResponse_DataChange_USER, SourceClusterId: cluster,
		RowKey: row, CommitTimestamp: timestamppb.New(at), Chunks: chunks,
		Done: true, Token: encodeToken(l.nextSeq),
	}
	l.nextSeq++
	l.tail = append(l.tail, rec)
	if len(l.tail) >= segmentRecords {
		l.segs = append(l.segs, &changeSegment{recs: l.tail})
		l.tail = nil
	}
	c.trimLocked(l)
	close(l.wake)
	l.wake = make(chan struct{})
}

// --- the data plane ---

// rowChange is one row's mutations, as the change stream records them.
type rowChange struct {
	row []byte
	ms  []*btpb.Mutation
}

// pendingChange is a write forwarded to a table with a change stream: what
// it would log, completed by the emulator's answer.
type pendingChange struct {
	c       *changeStreams
	table   string
	log     *tableLog
	cluster string
	method  string
	rows    []rowChange
	// alt is CheckAndMutateRow's false branch, rows its true one.
	alt       []rowChange
	responses []frame
}

func (p *pendingChange) observe(fr frame) { p.responses = append(p.responses, fr) }

func (p *pendingChange) release() { p.log.write.Unlock() }

// commit logs what the emulator applied, given it answered OK.
func (p *pendingChange) commit() {
	var rows []rowChange
	switch p.method {
	case data + "MutateRow":
		rows = p.rows
	case data + "MutateRows":
		applied := map[int64]bool{}
		for _, fr := range p.responses {
			var resp btpb.MutateRowsResponse
			if proto.Unmarshal(fr, &resp) != nil {
				continue
			}
			for _, e := range resp.GetEntries() {
				if e.GetStatus().GetCode() == int32(codes.OK) {
					applied[e.GetIndex()] = true
				}
			}
		}
		// The log follows the request's order.
		for i, r := range p.rows {
			if applied[int64(i)] {
				rows = append(rows, r)
			}
		}
	case data + "CheckAndMutateRow":
		if len(p.responses) == 0 {
			return
		}
		var resp btpb.CheckAndMutateRowResponse
		if proto.Unmarshal(p.responses[0], &resp) != nil {
			return
		}
		rows = p.alt
		if resp.GetPredicateMatched() {
			rows = p.rows
		}
	case data + "ReadModifyWriteRow":
		if len(p.responses) == 0 {
			return
		}
		var resp btpb.ReadModifyWriteRowResponse
		if proto.Unmarshal(p.responses[0], &resp) != nil {
			return
		}
		var ms []*btpb.Mutation
		for _, fam := range resp.GetRow().GetFamilies() {
			for _, col := range fam.GetColumns() {
				for _, cell := range col.GetCells() {
					ms = append(ms, &btpb.Mutation{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
						FamilyName: fam.GetName(), ColumnQualifier: col.GetQualifier(),
						TimestampMicros: cell.GetTimestampMicros(), Value: cell.GetValue()}}})
				}
			}
		}
		rows = []rowChange{{row: resp.GetRow().GetKey(), ms: ms}}
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	if p.log.gone {
		return
	}
	for _, r := range rows {
		p.c.appendLocked(p.table, p.log, r.row, r.ms, p.cluster)
	}
}

// mutationMethods are the data methods a change stream records.
var mutationMethods = map[string]bool{
	data + "MutateRow": true, data + "MutateRows": true, data + "CheckAndMutateRow": true, data + "ReadModifyWriteRow": true,
}

// prepare decodes a write to a table with a change stream, giving each
// server-timestamped SetCell its time, and returns the request to forward
// with what it would log, holding the table's write lock until release.
// A write to any other table is returned unchanged, with no pending change.
func (f *Front) prepareChange(ctx context.Context, method string, in frame) (frame, *pendingChange, error) {
	if !mutationMethods[method] || !f.changes.enabled() {
		return in, nil, nil
	}
	var req interface {
		proto.Message
		GetTableName() string
	}
	switch method {
	case data + "MutateRow":
		req = &btpb.MutateRowRequest{}
	case data + "MutateRows":
		req = &btpb.MutateRowsRequest{}
	case data + "CheckAndMutateRow":
		req = &btpb.CheckAndMutateRowRequest{}
	case data + "ReadModifyWriteRow":
		req = &btpb.ReadModifyWriteRowRequest{}
	}
	if err := proto.Unmarshal(in, req); err != nil {
		return nil, nil, grpcstatus.Errorf(codes.InvalidArgument, "cannot decode the request: %v", err)
	}
	table := req.GetTableName()
	f.changes.mu.Lock()
	l, ok := f.changes.tables[table]
	f.changes.mu.Unlock()
	if !ok {
		return in, nil, nil
	}
	l.write.Lock()
	p := &pendingChange{c: f.changes, table: table, log: l, method: method, cluster: f.sourceCluster(table)}
	ok = false
	defer func() {
		if !ok {
			l.write.Unlock()
		}
	}()
	// The server's timestamp, in milliseconds, which either granularity
	// takes.
	stamp := f.changes.now().UnixMicro() / 1000 * 1000
	var families []string
	resolve := func(ms []*btpb.Mutation) ([]*btpb.Mutation, error) {
		var out []*btpb.Mutation
		for _, m := range ms {
			switch x := m.GetMutation().(type) {
			case *btpb.Mutation_SetCell_:
				if x.SetCell.GetTimestampMicros() == -1 {
					x.SetCell.TimestampMicros = stamp
				}
				out = append(out, m)
			case *btpb.Mutation_DeleteFromRow_:
				if families == nil {
					t, err := btapb.NewBigtableTableAdminClient(f.upstream).GetTable(ctx, &btapb.GetTableRequest{Name: table, View: btapb.Table_SCHEMA_VIEW})
					if err != nil {
						return nil, err
					}
					families = []string{}
					for name := range t.GetColumnFamilies() {
						families = append(families, name)
					}
					sort.Strings(families)
				}
				for _, fam := range families {
					out = append(out, &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromFamily_{DeleteFromFamily: &btpb.Mutation_DeleteFromFamily{FamilyName: fam}}})
				}
			default:
				out = append(out, m)
			}
		}
		return out, nil
	}
	var err error
	switch r := req.(type) {
	case *btpb.MutateRowRequest:
		var ms []*btpb.Mutation
		if ms, err = resolve(r.GetMutations()); err == nil {
			p.rows = []rowChange{{row: r.GetRowKey(), ms: ms}}
		}
	case *btpb.MutateRowsRequest:
		for _, e := range r.GetEntries() {
			var ms []*btpb.Mutation
			if ms, err = resolve(e.GetMutations()); err != nil {
				break
			}
			p.rows = append(p.rows, rowChange{row: e.GetRowKey(), ms: ms})
		}
	case *btpb.CheckAndMutateRowRequest:
		var tm, fm []*btpb.Mutation
		if tm, err = resolve(r.GetTrueMutations()); err == nil {
			if fm, err = resolve(r.GetFalseMutations()); err == nil {
				p.rows = []rowChange{{row: r.GetRowKey(), ms: tm}}
				p.alt = []rowChange{{row: r.GetRowKey(), ms: fm}}
			}
		}
	}
	if err != nil {
		return nil, nil, err
	}
	out, err := proto.Marshal(req)
	if err != nil {
		return nil, nil, grpcstatus.Errorf(codes.Internal, "encode the request: %v", err)
	}
	ok = true
	return out, p, nil
}

// sourceCluster is the first cluster the front keeps of the table's
// instance, or "" for an instance only the emulator has.
func (f *Front) sourceCluster(table string) string {
	i := strings.Index(table, "/tables/")
	if i < 0 {
		return ""
	}
	f.instances.mu.Lock()
	defer f.instances.mu.Unlock()
	in, ok := f.instances.byNm[table[:i]]
	if !ok {
		return ""
	}
	var ids []string
	for id := range in.clusters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// --- the table admin calls the change stream is configured through ---

func (f *Front) changeStreamMethods() map[string]method {
	return map[string]method{
		admin + "CreateTable": unary(func() *btapb.CreateTableRequest { return &btapb.CreateTableRequest{} }, f.createTable),
		admin + "GetTable":    unary(func() *btapb.GetTableRequest { return &btapb.GetTableRequest{} }, f.getTable),
		admin + "UpdateTable": unary(func() *btapb.UpdateTableRequest { return &btapb.UpdateTableRequest{} }, f.updateTable),
	}
}

func (f *Front) createTable(ctx context.Context, req *btapb.CreateTableRequest) (proto.Message, error) {
	cfg := req.GetTable().GetChangeStreamConfig()
	var retention time.Duration
	if cfg != nil {
		var err error
		if retention, err = checkRetention(cfg); err != nil {
			return nil, err
		}
	}
	t, err := btapb.NewBigtableTableAdminClient(f.upstream).CreateTable(ctx, req)
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		f.changes.set(t.GetName(), retention)
		t.ChangeStreamConfig = f.changes.config(t.GetName())
	}
	return t, nil
}

// withConfig is t with its change_stream_config, in the views that hold
// it.
func (f *Front) withConfig(t *btapb.Table, view btapb.Table_View) *btapb.Table {
	switch view {
	case btapb.Table_VIEW_UNSPECIFIED, btapb.Table_SCHEMA_VIEW, btapb.Table_FULL:
		t.ChangeStreamConfig = f.changes.config(t.GetName())
	}
	return t
}

func (f *Front) getTable(ctx context.Context, req *btapb.GetTableRequest) (proto.Message, error) {
	t, err := btapb.NewBigtableTableAdminClient(f.upstream).GetTable(ctx, req)
	if err != nil {
		return nil, err
	}
	return f.withConfig(t, req.GetView()), nil
}

// updateTable serves the change_stream_config paths of UpdateTable and
// forwards the others to the emulator.
func (f *Front) updateTable(ctx context.Context, req *btapb.UpdateTableRequest) (proto.Message, error) {
	name := req.GetTable().GetName()
	if !tableNameRe.MatchString(name) {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "invalid table name %q: want projects/{project}/instances/{instance}/tables/{table}", name)
	}
	if len(req.GetUpdateMask().GetPaths()) == 0 {
		return nil, grpcstatus.Error(codes.InvalidArgument, "UpdateTableRequest.UpdateMask required for table update")
	}
	var rest []string
	stream, retention := false, time.Duration(0)
	for _, p := range req.GetUpdateMask().GetPaths() {
		switch p {
		case "change_stream_config", "change_stream_config.retention_period":
			stream = true
			if cfg := req.GetTable().GetChangeStreamConfig(); cfg != nil {
				r, err := checkRetention(cfg)
				if err != nil {
					return nil, err
				}
				retention = r
			}
		default:
			rest = append(rest, p)
		}
	}
	c := btapb.NewBigtableTableAdminClient(f.upstream)
	start := timestamppb.Now()
	if len(rest) > 0 {
		fwd := proto.Clone(req).(*btapb.UpdateTableRequest)
		fwd.Table.ChangeStreamConfig = nil
		fwd.UpdateMask = &fieldmaskpb.FieldMask{Paths: rest}
		op, err := c.UpdateTable(ctx, fwd)
		if err != nil {
			return nil, err
		}
		if e := op.GetError(); e != nil {
			return nil, grpcstatus.ErrorProto(e)
		}
	} else if _, err := c.GetTable(ctx, &btapb.GetTableRequest{Name: name, View: btapb.Table_NAME_ONLY}); err != nil {
		return nil, err
	}
	if stream {
		f.changes.set(name, retention)
	}
	t, err := c.GetTable(ctx, &btapb.GetTableRequest{Name: name, View: btapb.Table_SCHEMA_VIEW})
	if err != nil {
		return nil, err
	}
	return f.copies.op(name, &btapb.UpdateTableMetadata{Name: name, StartTime: start, EndTime: timestamppb.Now()},
		f.withConfig(t, btapb.Table_SCHEMA_VIEW))
}

// --- the change stream reads ---

// streamMethods are the server-streaming methods the front serves.
func (f *Front) streamMethods() map[string]func(grpc.ServerStream) error {
	return map[string]func(grpc.ServerStream) error{
		data + "GenerateInitialChangeStreamPartitions": f.generatePartitions,
		data + "ReadChangeStream":                      f.readChangeStream,
	}
}

// wholeTable is the stream's single partition.
func wholeTable() *btpb.StreamPartition {
	return &btpb.StreamPartition{RowRange: &btpb.RowRange{
		StartKey: &btpb.RowRange_StartKeyClosed{StartKeyClosed: []byte{}},
		EndKey:   &btpb.RowRange_EndKeyOpen{EndKeyOpen: []byte{}},
	}}
}

// isWholeTable reports whether p is the whole row key space.
func isWholeTable(p *btpb.StreamPartition) bool {
	r := p.GetRowRange()
	if r == nil {
		return false
	}
	start := r.GetStartKey() == nil || (len(r.GetStartKeyClosed()) == 0 && r.GetStartKeyOpen() == nil)
	end := r.GetEndKey() == nil || (len(r.GetEndKeyOpen()) == 0 && r.GetEndKeyClosed() == nil)
	return start && end
}

// lookupStream returns the table's log, or the error a read of it gets.
func (f *Front) lookupStream(ctx context.Context, table string) (*tableLog, error) {
	if !tableNameRe.MatchString(table) {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "invalid table name %q: want projects/{project}/instances/{instance}/tables/{table}", table)
	}
	if _, err := btapb.NewBigtableTableAdminClient(f.upstream).GetTable(ctx, &btapb.GetTableRequest{Name: table, View: btapb.Table_NAME_ONLY}); err != nil {
		return nil, err
	}
	f.changes.mu.Lock()
	defer f.changes.mu.Unlock()
	l, ok := f.changes.tables[table]
	if !ok {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "Change Stream is not enabled for table %s", table)
	}
	return l, nil
}

func (f *Front) generatePartitions(ss grpc.ServerStream) error {
	var in frame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	var req btpb.GenerateInitialChangeStreamPartitionsRequest
	if err := proto.Unmarshal(in, &req); err != nil {
		return grpcstatus.Errorf(codes.InvalidArgument, "cannot decode the request: %v", err)
	}
	if _, err := f.lookupStream(ss.Context(), req.GetTableName()); err != nil {
		return err
	}
	return ss.SendMsg(&btpb.GenerateInitialChangeStreamPartitionsResponse{Partition: wholeTable()})
}

// readFrom returns the records after seq, up to limit, and the low
// watermark: no record logged later commits before it.
func (c *changeStreams) readFrom(l *tableLog, seq int64, limit int) ([]*btpb.ReadChangeStreamResponse_DataChange, time.Time, <-chan struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*btpb.ReadChangeStreamResponse_DataChange
	add := func(recs []*btpb.ReadChangeStreamResponse_DataChange) {
		i := sort.Search(len(recs), func(i int) bool { return recSeq(recs[i]) > seq })
		for ; i < len(recs) && len(out) < limit; i++ {
			out = append(out, recs[i])
		}
	}
	for _, s := range l.segs {
		if len(out) >= limit {
			break
		}
		if n := len(s.recs); n > 0 && recSeq(s.recs[n-1]) > seq {
			add(s.recs)
		}
	}
	if len(out) < limit {
		add(l.tail)
	}
	mark := c.now().UTC().Truncate(time.Microsecond)
	if mark.Before(l.last) {
		mark = l.last
	}
	return out, mark, l.wake, l.gone
}

// seqAt is the seq a read starting at t resumes after: the last record
// committed before t.
func (c *changeStreams) seqAt(l *tableLog, t time.Time) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	seq := l.trimmed
	for _, recs := range append(segRecs(l.segs), l.tail) {
		for _, r := range recs {
			if !r.GetCommitTimestamp().AsTime().Before(t) {
				return seq
			}
			seq = recSeq(r)
		}
	}
	return seq
}

func segRecs(segs []*changeSegment) [][]*btpb.ReadChangeStreamResponse_DataChange {
	out := make([][]*btpb.ReadChangeStreamResponse_DataChange, len(segs))
	for i, s := range segs {
		out[i] = s.recs
	}
	return out
}

// dataChangeMessages is rec as the stream sends it: one message, or more
// when a SetCell value is chunked, the last done and carrying the token and
// watermark.
func dataChangeMessages(rec *btpb.ReadChangeStreamResponse_DataChange, mark time.Time) []*btpb.ReadChangeStreamResponse {
	var chunks []*btpb.ReadChangeStreamResponse_MutationChunk
	for _, ch := range rec.GetChunks() {
		sc := ch.GetMutation().GetSetCell()
		if sc == nil || len(sc.GetValue()) <= maxChunkBytes {
			chunks = append(chunks, ch)
			continue
		}
		v := sc.GetValue()
		for off := 0; off < len(v); off += maxChunkBytes {
			end := min(off+maxChunkBytes, len(v))
			part := proto.Clone(sc).(*btpb.Mutation_SetCell)
			part.Value = v[off:end]
			chunks = append(chunks, &btpb.ReadChangeStreamResponse_MutationChunk{
				ChunkInfo: &btpb.ReadChangeStreamResponse_MutationChunk_ChunkInfo{
					ChunkedValueSize: int32(len(v)), ChunkedValueOffset: int32(off), LastChunk: end == len(v)},
				Mutation: &btpb.Mutation{Mutation: &btpb.Mutation_SetCell_{SetCell: part}},
			})
		}
	}
	// A message ends after a chunked value's piece, so each piece is a
	// message of its own.
	var groups [][]*btpb.ReadChangeStreamResponse_MutationChunk
	var cur []*btpb.ReadChangeStreamResponse_MutationChunk
	for _, ch := range chunks {
		cur = append(cur, ch)
		if ch.GetChunkInfo() != nil {
			groups = append(groups, cur)
			cur = nil
		}
	}
	if len(cur) > 0 || len(groups) == 0 {
		groups = append(groups, cur)
	}
	out := make([]*btpb.ReadChangeStreamResponse, len(groups))
	for i, g := range groups {
		dc := &btpb.ReadChangeStreamResponse_DataChange{Chunks: g}
		if i == 0 {
			dc.Type, dc.SourceClusterId, dc.RowKey = rec.GetType(), rec.GetSourceClusterId(), rec.GetRowKey()
			dc.CommitTimestamp, dc.Tiebreaker = rec.GetCommitTimestamp(), rec.GetTiebreaker()
		}
		if i == len(groups)-1 {
			dc.Done, dc.Token = true, rec.GetToken()
			dc.EstimatedLowWatermark = timestamppb.New(mark)
		}
		out[i] = &btpb.ReadChangeStreamResponse{StreamRecord: &btpb.ReadChangeStreamResponse_DataChange_{DataChange: dc}}
	}
	return out
}

func (f *Front) readChangeStream(ss grpc.ServerStream) error {
	var in frame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	var req btpb.ReadChangeStreamRequest
	if err := proto.Unmarshal(in, &req); err != nil {
		return grpcstatus.Errorf(codes.InvalidArgument, "cannot decode the request: %v", err)
	}
	ctx := ss.Context()
	l, err := f.lookupStream(ctx, req.GetTableName())
	if err != nil {
		return err
	}
	if req.GetPartition() == nil {
		return grpcstatus.Error(codes.InvalidArgument, "partition is required")
	}
	heartbeat := defaultHeartbeat
	if d := req.GetHeartbeatDuration(); d != nil {
		if heartbeat = d.AsDuration(); heartbeat <= 0 {
			return grpcstatus.Errorf(codes.InvalidArgument, "heartbeat_duration %s must be positive", heartbeat)
		}
	}
	var end time.Time
	if e := req.GetEndTime(); e != nil {
		end = e.AsTime().Truncate(time.Microsecond)
	}
	now := f.changes.now()
	var seq int64
	switch {
	case req.GetContinuationTokens() != nil:
		toks := req.GetContinuationTokens().GetTokens()
		if len(toks) == 0 {
			return grpcstatus.Error(codes.InvalidArgument, "continuation_tokens is empty")
		}
		seq = -1
		for _, t := range toks {
			if !isWholeTable(t.GetPartition()) {
				return grpcstatus.Errorf(codes.InvalidArgument, "a continuation token's partition %v is not one of the table's", t.GetPartition().GetRowRange())
			}
			n, err := decodeToken(t.GetToken())
			if err != nil {
				return err
			}
			if seq < 0 || n < seq {
				seq = n
			}
		}
		f.changes.mu.Lock()
		expired := seq < l.trimmed
		f.changes.mu.Unlock()
		if expired {
			return grpcstatus.Errorf(codes.InvalidArgument, "the continuation token %s is older than the change stream's retention period", encodeToken(seq))
		}
	case req.GetStartTime() != nil:
		st := req.GetStartTime().AsTime().Truncate(time.Microsecond)
		f.changes.mu.Lock()
		earliest := now.Add(-l.retention)
		if l.since.After(earliest) {
			earliest = l.since
		}
		f.changes.mu.Unlock()
		if st.After(now) {
			return grpcstatus.Errorf(codes.InvalidArgument, "start_time %s is in the future", st.Format(time.RFC3339Nano))
		}
		if st.Before(earliest.Truncate(time.Microsecond)) {
			return grpcstatus.Errorf(codes.InvalidArgument, "start_time %s is before the change stream's earliest time %s", st.Format(time.RFC3339Nano), earliest.Format(time.RFC3339Nano))
		}
		seq = f.changes.seqAt(l, st)
	default:
		seq = f.changes.seqAt(l, now)
	}
	if !isWholeTable(req.GetPartition()) {
		// The partitions changed (or never were this one): the client
		// is to read the table's partition from where it was.
		return ss.SendMsg(&btpb.ReadChangeStreamResponse{StreamRecord: &btpb.ReadChangeStreamResponse_CloseStream_{CloseStream: &btpb.ReadChangeStreamResponse_CloseStream{
			Status:             &status.Status{Code: int32(codes.OutOfRange), Message: "the partition is not one of the table's: read the new partitions"},
			ContinuationTokens: []*btpb.StreamContinuationToken{{Partition: wholeTable(), Token: encodeToken(seq)}},
			NewPartitions:      []*btpb.StreamPartition{wholeTable()},
		}}})
	}
	lastSent := time.Now()
	for {
		recs, mark, wake, gone := f.changes.readFrom(l, seq, 64)
		if gone {
			return grpcstatus.Errorf(codes.FailedPrecondition, "the change stream of %s is no longer enabled", req.GetTableName())
		}
		for i, rec := range recs {
			if !end.IsZero() && rec.GetCommitTimestamp().AsTime().After(end) {
				return nil
			}
			m := rec.GetCommitTimestamp().AsTime()
			if i == len(recs)-1 {
				m = mark
			}
			for _, msg := range dataChangeMessages(rec, m) {
				if err := ss.SendMsg(msg); err != nil {
					return err
				}
			}
			seq = recSeq(rec)
			lastSent = time.Now()
		}
		if len(recs) > 0 {
			continue
		}
		if !end.IsZero() && mark.After(end) {
			return nil
		}
		wait := time.Until(lastSent.Add(heartbeat))
		if !end.IsZero() {
			if d := time.Until(end) + time.Microsecond; d < wait {
				wait = d
			}
		}
		timer := time.NewTimer(max(wait, 0))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-wake:
			timer.Stop()
		case <-timer.C:
			if time.Since(lastSent) >= heartbeat {
				_, mark, _, _ := f.changes.readFrom(l, seq, 0)
				if err := ss.SendMsg(&btpb.ReadChangeStreamResponse{StreamRecord: &btpb.ReadChangeStreamResponse_Heartbeat_{Heartbeat: &btpb.ReadChangeStreamResponse_Heartbeat{
					ContinuationToken:     &btpb.StreamContinuationToken{Partition: wholeTable(), Token: encodeToken(seq)},
					EstimatedLowWatermark: timestamppb.New(mark),
				}}}); err != nil {
					return err
				}
				lastSent = time.Now()
			}
		}
	}
}

// --- state ---

type savedChangeLog struct {
	Retention time.Duration `json:"retention"`
	Since     time.Time     `json:"since"`
	NextSeq   int64         `json:"nextSeq"`
	Trimmed   int64         `json:"trimmed,omitempty"`
	Last      time.Time     `json:"last"`
	Segments  []string      `json:"segments,omitempty"`
	Tail      [][]byte      `json:"tail,omitempty"`
}

func changeSegmentKey(table string, s *changeSegment) string {
	return "changes:" + table + ":" + strconv.FormatInt(recSeq(s.recs[0]), 10)
}

func encodeRecs(recs []*btpb.ReadChangeStreamResponse_DataChange) ([][]byte, error) {
	out := make([][]byte, len(recs))
	for i, r := range recs {
		b, err := proto.Marshal(r)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

func decodeRecs(parts [][]byte) ([]*btpb.ReadChangeStreamResponse_DataChange, error) {
	out := make([]*btpb.ReadChangeStreamResponse_DataChange, len(parts))
	for i, b := range parts {
		out[i] = &btpb.ReadChangeStreamResponse_DataChange{}
		if err := proto.Unmarshal(b, out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// snapshot is the logs to save, and their segments as copies.
func (c *changeStreams) snapshot(addCopy func(key string, data any, encode func() ([]byte, error))) (map[string]savedChangeLog, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.tables) == 0 {
		return nil, nil
	}
	out := make(map[string]savedChangeLog, len(c.tables))
	for name, l := range c.tables {
		c.trimLocked(l)
		s := savedChangeLog{Retention: l.retention, Since: l.since, NextSeq: l.nextSeq, Trimmed: l.trimmed, Last: l.last}
		for _, seg := range l.segs {
			key := changeSegmentKey(name, seg)
			s.Segments = append(s.Segments, key)
			recs := seg.recs
			addCopy(key, seg, func() ([]byte, error) {
				parts, err := encodeRecs(recs)
				if err != nil {
					return nil, err
				}
				return frontstate.Pack(parts...), nil
			})
		}
		var err error
		if s.Tail, err = encodeRecs(l.tail); err != nil {
			return nil, err
		}
		out[name] = s
	}
	return out, nil
}

// restore replaces the logs with saved, reading each segment by readCopy.
func (c *changeStreams) restore(saved map[string]savedChangeLog, readCopy func(key string) ([]byte, error), markRead func(key string, data any)) error {
	tables := make(map[string]*tableLog, len(saved))
	for name, s := range saved {
		l := &tableLog{retention: s.Retention, since: s.Since, nextSeq: s.NextSeq, trimmed: s.Trimmed, last: s.Last, wake: make(chan struct{})}
		if l.nextSeq < 1 {
			l.nextSeq = 1
		}
		for _, key := range s.Segments {
			raw, err := readCopy(key)
			if err != nil {
				return fmt.Errorf("the change log segment %s: %w", key, err)
			}
			parts, err := frontstate.Unpack(raw)
			if err != nil {
				return fmt.Errorf("the change log segment %s: %w", key, err)
			}
			recs, err := decodeRecs(parts)
			if err != nil {
				return fmt.Errorf("the change log segment %s: %w", key, err)
			}
			if len(recs) == 0 {
				continue
			}
			seg := &changeSegment{recs: recs}
			markRead(key, seg)
			l.segs = append(l.segs, seg)
		}
		var err error
		if l.tail, err = decodeRecs(s.Tail); err != nil {
			return fmt.Errorf("the change log of %s: %w", name, err)
		}
		tables[name] = l
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for name := range c.tables {
		c.dropLocked(name)
	}
	c.tables = tables
	return nil
}

// reset forgets every log.
func (c *changeStreams) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name := range c.tables {
		c.dropLocked(name)
	}
}
