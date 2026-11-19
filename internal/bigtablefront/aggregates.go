package bigtablefront

// Aggregate column families (#1893): the emulator (bttest) computes only
// Sum. It stores an AddToCell to a Min or Max family as the new value,
// replacing the cell's, keeps an AddToCell to a HyperLogLog++ family as the
// 8-byte input, and refuses a MergeToCell whose input is an Int64. Google
// documents (https://docs.cloud.google.com/bigtable/docs/aggregates and
// Type.Aggregate's reference):
//
//   - Min and Max: "Allowed input: Int64. State: same as input"; the cell's
//     value is replaced with the lower (higher) of the new value and the
//     current one.
//   - HyperLogLog++ (hllpp_unique_count): "Input: Any, or omit for Raw";
//     "The written value is added to a probabilistic set of all values added
//     since the most recent reset", the state is the set's sketch, read
//     with the ZetaSketch library.
//   - "Writes will provide either the input_type or state_type, and reads
//     will always return the state_type": AddToCell gives an input,
//     MergeToCell a state.
//
// So the front computes them. A write naming a Min, Max or HLL++ family has
// each AddToCell and MergeToCell rewritten to a MergeToCell of the cell's
// new state, which the emulator stores as given (its merge for those types
// is a replacement), after reading the cell's state at that timestamp; the
// writes that do this hold aggWrites.mu, so no two interleave. Sum stays
// the emulator's, but a MergeToCell's Int64 input is passed to it as the
// 8-byte big-endian state it takes. The sketches are ZetaSketch's
// HyperLogLog++ (precision 15, sparse precision 20, ZetaSketch's and
// BigQuery's defaults), through github.com/cloudburrow/cloudburrow/third_party/zetasketch, a Go port of
// Google's Java library that serialises the same AggregatorStateProto, so a
// sketch read from a cell merges with BigQuery's and ZetaSketch's.

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/cloudburrow/cloudburrow/third_party/zetasketch"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// aggWrites serialises the writes the front computes aggregates for.
type aggWrites struct {
	mu sync.Mutex
}

type aggKind int

const (
	aggNone aggKind = iota
	aggSum
	aggMin
	aggMax
	aggHLL
)

// aggKindOf is a family's aggregate, aggNone when it is not one.
func aggKindOf(cf *btapb.ColumnFamily) aggKind {
	a := cf.GetValueType().GetAggregateType()
	switch {
	case a == nil:
		return aggNone
	case a.GetSum() != nil:
		return aggSum
	case a.GetMin() != nil:
		return aggMin
	case a.GetMax() != nil:
		return aggMax
	case a.GetHllppUniqueCount() != nil:
		return aggHLL
	}
	return aggNone
}

// hasAggregateWrite says whether any mutation is an AddToCell or a
// MergeToCell.
func hasAggregateWrite(lists ...[]*btpb.Mutation) bool {
	for _, ms := range lists {
		for _, m := range ms {
			switch m.GetMutation().(type) {
			case *btpb.Mutation_AddToCell_, *btpb.Mutation_MergeToCell_:
				return true
			}
		}
	}
	return false
}

// prepareAggregates rewrites a write's AddToCell and MergeToCell mutations
// as this file's comment says. When it rewrites, release is non-nil and
// must be called once the write has returned.
func (f *Front) prepareAggregates(ctx context.Context, method string, in frame) (frame, func(), error) {
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
	default:
		return in, nil, nil
	}
	if err := proto.Unmarshal(in, req); err != nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "cannot decode the request: %v", err)
	}
	var lists [][]*btpb.Mutation
	switch r := req.(type) {
	case *btpb.MutateRowRequest:
		lists = append(lists, r.GetMutations())
	case *btpb.MutateRowsRequest:
		for _, e := range r.GetEntries() {
			lists = append(lists, e.GetMutations())
		}
	case *btpb.CheckAndMutateRowRequest:
		lists = append(lists, r.GetTrueMutations(), r.GetFalseMutations())
	}
	if req.GetTableName() == "" || !hasAggregateWrite(lists...) {
		return in, nil, nil
	}
	table, err := btapb.NewBigtableTableAdminClient(f.upstream).GetTable(ctx,
		&btapb.GetTableRequest{Name: req.GetTableName(), View: btapb.Table_SCHEMA_VIEW})
	if err != nil {
		// The emulator answers the write itself (NOT_FOUND and the like).
		return in, nil, nil //nolint:nilerr // the write's own call reports it
	}
	kinds := map[string]aggKind{}
	computed := false
	for name, cf := range table.GetColumnFamilies() {
		k := aggKindOf(cf)
		kinds[name] = k
		computed = computed || k != aggNone
	}
	if !computed {
		return in, nil, nil
	}
	f.aggs.mu.Lock()
	unlock := true
	defer func() {
		if unlock {
			f.aggs.mu.Unlock()
		}
	}()
	w := &aggWrite{f: f, ctx: ctx, table: req.GetTableName(), kinds: kinds, read: map[aggCell]aggState{}}
	switch r := req.(type) {
	case *btpb.MutateRowRequest:
		r.Mutations, err = w.row(r.GetRowKey(), w.fresh(r.GetRowKey()), r.GetMutations())
	case *btpb.MutateRowsRequest:
		rows := map[string]*aggRow{}
		for _, e := range r.GetEntries() {
			st, ok := rows[string(e.GetRowKey())]
			if !ok {
				st = w.fresh(e.GetRowKey())
				rows[string(e.GetRowKey())] = st
			}
			if e.Mutations, err = w.row(e.GetRowKey(), st, e.GetMutations()); err != nil {
				break
			}
		}
	case *btpb.CheckAndMutateRowRequest:
		// Each branch starts from the row as it is.
		if r.TrueMutations, err = w.row(r.GetRowKey(), w.fresh(r.GetRowKey()), r.GetTrueMutations()); err == nil {
			r.FalseMutations, err = w.row(r.GetRowKey(), w.fresh(r.GetRowKey()), r.GetFalseMutations())
		}
	}
	if err != nil {
		return nil, nil, err
	}
	out, err := proto.Marshal(req)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "encode the request: %v", err)
	}
	unlock = false
	return out, f.aggs.mu.Unlock, nil
}

// aggCell names one cell: a family, a qualifier and a timestamp.
type aggCell struct {
	row, family, qualifier string
	ts                     int64
}

// aggState is a cell's state, or its absence.
type aggState struct {
	value  []byte
	exists bool
}

// aggWrite is one write being rewritten.
type aggWrite struct {
	f     *Front
	ctx   context.Context
	table string
	kinds map[string]aggKind
	// read is the cells as the emulator holds them, read once each.
	read map[aggCell]aggState
}

// aggRow is a row as one mutation list has left it so far: the states it
// wrote, and what it deleted.
type aggRow struct {
	written     map[aggCell]aggState
	rowDeleted  bool
	famsDeleted map[string]bool
	colsDeleted []*btpb.Mutation_DeleteFromColumn
}

func (w *aggWrite) fresh([]byte) *aggRow {
	return &aggRow{written: map[aggCell]aggState{}, famsDeleted: map[string]bool{}}
}

// deleted says whether the list's earlier mutations deleted c.
func (r *aggRow) deleted(c aggCell) bool {
	if r.rowDeleted || r.famsDeleted[c.family] {
		return true
	}
	for _, d := range r.colsDeleted {
		if r.deletedBy(d, c) {
			return true
		}
	}
	return false
}

// state is c's state before the next mutation.
func (w *aggWrite) state(r *aggRow, c aggCell) (aggState, error) {
	if s, ok := r.written[c]; ok {
		return s, nil
	}
	if r.deleted(c) {
		return aggState{}, nil
	}
	if s, ok := w.read[c]; ok {
		return s, nil
	}
	rows, err := readAll(w.ctx, btpb.NewBigtableClient(w.f.upstream), &btpb.ReadRowsRequest{TableName: w.table,
		Rows: &btpb.RowSet{RowKeys: [][]byte{[]byte(c.row)}},
		Filter: &btpb.RowFilter{Filter: &btpb.RowFilter_Chain_{Chain: &btpb.RowFilter_Chain{Filters: []*btpb.RowFilter{
			{Filter: &btpb.RowFilter_ColumnRangeFilter{ColumnRangeFilter: &btpb.ColumnRange{FamilyName: c.family,
				StartQualifier: &btpb.ColumnRange_StartQualifierClosed{StartQualifierClosed: []byte(c.qualifier)},
				EndQualifier:   &btpb.ColumnRange_EndQualifierClosed{EndQualifierClosed: []byte(c.qualifier)}}}},
			{Filter: &btpb.RowFilter_TimestampRangeFilter{TimestampRangeFilter: &btpb.TimestampRange{
				StartTimestampMicros: c.ts, EndTimestampMicros: c.ts + 1}}},
		}}}}})
	if err != nil {
		return aggState{}, err
	}
	var s aggState
	for _, rw := range rows {
		for _, cl := range rw.cells {
			if cl.family == c.family && string(cl.qualifier) == c.qualifier && cl.ts == c.ts {
				s = aggState{value: cl.value, exists: true}
			}
		}
	}
	w.read[c] = s
	return s, nil
}

// row rewrites one row's mutation list.
func (w *aggWrite) row(key []byte, r *aggRow, ms []*btpb.Mutation) ([]*btpb.Mutation, error) {
	out := make([]*btpb.Mutation, 0, len(ms))
	for _, m := range ms {
		switch x := m.GetMutation().(type) {
		case *btpb.Mutation_DeleteFromRow_:
			r.rowDeleted = true
			clear(r.written)
		case *btpb.Mutation_DeleteFromFamily_:
			r.famsDeleted[x.DeleteFromFamily.GetFamilyName()] = true
			for c := range r.written {
				if c.family == x.DeleteFromFamily.GetFamilyName() {
					delete(r.written, c)
				}
			}
		case *btpb.Mutation_DeleteFromColumn_:
			r.colsDeleted = append(r.colsDeleted, x.DeleteFromColumn)
			for c := range r.written {
				if r.deletedBy(x.DeleteFromColumn, c) {
					delete(r.written, c)
				}
			}
		case *btpb.Mutation_SetCell_:
			delete(r.written, aggCell{string(key), x.SetCell.GetFamilyName(), string(x.SetCell.GetColumnQualifier()), x.SetCell.GetTimestampMicros()})
		case *btpb.Mutation_AddToCell_:
			a := x.AddToCell
			nm, err := w.merge(key, r, a.GetFamilyName(), a.GetColumnQualifier(), a.GetTimestamp(), a.GetInput(), false)
			if err != nil {
				return nil, err
			}
			if nm != nil {
				m = nm
			}
		case *btpb.Mutation_MergeToCell_:
			a := x.MergeToCell
			nm, err := w.merge(key, r, a.GetFamilyName(), a.GetColumnQualifier(), a.GetTimestamp(), a.GetInput(), true)
			if err != nil {
				return nil, err
			}
			if nm != nil {
				m = nm
			}
		}
		out = append(out, m)
	}
	return out, nil
}

func (r *aggRow) deletedBy(d *btpb.Mutation_DeleteFromColumn, c aggCell) bool {
	if d.GetFamilyName() != c.family || string(d.GetColumnQualifier()) != c.qualifier {
		return false
	}
	tr := d.GetTimeRange()
	return c.ts >= tr.GetStartTimestampMicros() && (tr.GetEndTimestampMicros() == 0 || c.ts < tr.GetEndTimestampMicros())
}

// merge is one AddToCell (state false) or MergeToCell (state true): the
// MergeToCell of the cell's new state that replaces it, or nil to forward
// the mutation as it is (a family the emulator computes, or none).
func (w *aggWrite) merge(key []byte, r *aggRow, family string, qual, ts, input *btpb.Value, state bool) (*btpb.Mutation, error) {
	kind := w.kinds[family]
	if kind == aggNone || qual.GetKind() == nil || ts.GetKind() == nil {
		return nil, nil
	}
	c := aggCell{string(key), family, string(qual.GetRawValue()), ts.GetRawTimestampMicros()}
	if kind == aggSum {
		// The emulator sums; it takes a state as its 8 bytes alone.
		delete(r.written, c)
		if iv, ok := input.GetKind().(*btpb.Value_IntValue); ok && state {
			return mergeToCell(family, qual, ts, binary.BigEndian.AppendUint64(nil, uint64(iv.IntValue))), nil
		}
		return nil, nil
	}
	cur, err := w.state(r, c)
	if err != nil {
		return nil, err
	}
	what := "AddToCell"
	if state {
		what = "MergeToCell"
	}
	var next []byte
	switch kind {
	case aggMin, aggMax:
		v, err := int64Input(input)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%s to the %s family %q: %v", what, map[aggKind]string{aggMin: "min", aggMax: "max"}[kind], family, err)
		}
		if cur.exists && len(cur.value) == 8 {
			old := int64(binary.BigEndian.Uint64(cur.value))
			if kind == aggMin && old < v || kind == aggMax && old > v {
				v = old
			}
		}
		next = binary.BigEndian.AppendUint64(nil, uint64(v))
	case aggHLL:
		h := zetasketch.NewHLL(nil)
		if cur.exists && len(cur.value) > 0 {
			if err := h.UnmarshalBinary(cur.value); err != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "the cell's HyperLogLog++ state does not decode: %v", err)
			}
		}
		if state {
			b, err := bytesInput(input)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "MergeToCell to the hllpp_unique_count family %q: %v", family, err)
			}
			o := zetasketch.NewHLL(nil)
			if err := o.UnmarshalBinary(b); err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "MergeToCell to the hllpp_unique_count family %q: the input is not a HyperLogLog++ sketch: %v", family, err)
			}
			if err := h.Merge(o); err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "MergeToCell to the hllpp_unique_count family %q: %v", family, err)
			}
		} else {
			v, err := hllInput(input)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "AddToCell to the hllpp_unique_count family %q: %v", family, err)
			}
			h.Add(v)
		}
		if next, err = h.MarshalBinary(); err != nil {
			return nil, status.Errorf(codes.Internal, "encoding the HyperLogLog++ state: %v", err)
		}
	}
	r.written[c] = aggState{value: next, exists: true}
	return mergeToCell(family, qual, ts, next), nil
}

func mergeToCell(family string, qual, ts *btpb.Value, state []byte) *btpb.Mutation {
	return &btpb.Mutation{Mutation: &btpb.Mutation_MergeToCell_{MergeToCell: &btpb.Mutation_MergeToCell{
		FamilyName: family, ColumnQualifier: qual, Timestamp: ts,
		Input: &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: state}},
	}}}
}

// int64Input is an Int64 input or state: an int_value, or its 8-byte
// big-endian raw encoding.
func int64Input(v *btpb.Value) (int64, error) {
	switch k := v.GetKind().(type) {
	case *btpb.Value_IntValue:
		return k.IntValue, nil
	case *btpb.Value_RawValue:
		if len(k.RawValue) == 8 {
			return int64(binary.BigEndian.Uint64(k.RawValue)), nil
		}
		return 0, fmt.Errorf("an Int64 raw value is 8 bytes, big-endian; got %d bytes", len(k.RawValue))
	}
	return 0, fmt.Errorf("the input must be an Int64 value, not %s", valueKind(v))
}

// bytesInput is a raw or bytes value.
func bytesInput(v *btpb.Value) ([]byte, error) {
	switch k := v.GetKind().(type) {
	case *btpb.Value_RawValue:
		return k.RawValue, nil
	case *btpb.Value_BytesValue:
		return k.BytesValue, nil
	}
	return nil, fmt.Errorf("the state must be bytes, not %s", valueKind(v))
}

// hllInput is a value added to a HyperLogLog++ set, hashed as ZetaSketch
// hashes it: an Int64 as a long, bytes and strings as their bytes.
func hllInput(v *btpb.Value) (zetasketch.Value, error) {
	switch k := v.GetKind().(type) {
	case *btpb.Value_IntValue:
		return zetasketch.Uint64Value(uint64(k.IntValue)), nil
	case *btpb.Value_RawValue:
		return zetasketch.BinaryValue(k.RawValue), nil
	case *btpb.Value_BytesValue:
		return zetasketch.BinaryValue(k.BytesValue), nil
	case *btpb.Value_StringValue:
		return zetasketch.StringValue(k.StringValue), nil
	}
	return nil, fmt.Errorf("the input must be an Int64, bytes or a string, not %s", valueKind(v))
}

func valueKind(v *btpb.Value) string {
	if v.GetKind() == nil {
		return "no value"
	}
	return fmt.Sprintf("%T", v.GetKind())
}
