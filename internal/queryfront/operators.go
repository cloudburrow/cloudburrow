package queryfront

// Datastore's NOT_EQUAL, IN and NOT_IN property filters and OR composite
// filters (#1888), which the Datastore emulator refuses (measured
// 2026-11-19 through RunQuery, structured and GQL alike: IN with two values
// "Filter has 2 properties, expected 1", != an UNKNOWN with no message,
// NOT_IN "Unrecognized PropertyFilter.op: NOT_IN", OR "unsupported
// composite property operator"), and Google serves
// (https://cloud.google.com/datastore/docs/concepts/queries#filters).
//
// The front answers such a query from queries the emulator serves. Its
// filter is put in disjunctive normal form, each conjunction a query the
// emulator runs:
//
//   - p IN [a, b] is p = a OR p = b ("combine up to 30 equality (==)
//     clauses on the same property with a logical OR");
//   - p != v is p < v OR p > v, the two range queries Google's != is, so
//     only entities where p exists and is indexed match, an array matching
//     when any of its values does;
//   - p NOT_IN [a, b] (a < b) is p < a OR (p > a AND p < b) OR p > b:
//     "up to 10 not-equal (!=) clauses on the same property with a logical
//     AND", each satisfied by a single value of p.
//
// The conjunctions' results are merged in the query's order (the property
// orders, then the key), each entity once, at the place of the smallest
// value that satisfied the filter for an ascending order ("For ascending
// order, the smallest value that satisfies the filter is used for
// ordering") and the largest for a descending one; then the query's offset
// and limit are applied. Every result's cursor, and the batch's end and
// skipped cursors, are the front's: the position in that order, so a query
// resumed at one returns what comes after it, whatever was written since.
// COUNT, SUM and AVG (datastore.go) run over the merged results.
//
// Google's limits, each INVALID_ARGUMENT:
//   - an IN of at most 30 values and a NOT_IN of at most 10, each a
//     non-empty array;
//   - one NOT_EQUAL or NOT_IN in a query ("Only a single NOT_IN or != is
//     allowed per query");
//   - at most 30 disjunctions in disjunctive normal form, an IN counting a
//     disjunction for each of its values (Firestore's documented limit,
//     https://cloud.google.com/firestore/docs/query-data/queries#limits_on_or_queries;
//     UNVERIFIED for Datastore mode (re-read 2026-11-19), whose pages
//     name none: https://cloud.google.com/datastore/docs/concepts/queries);
//   - every disjunction with the same ancestor filter ("All evaluated
//     disjunctions must have the same ancestor filter").
//
// The emulator keeps its own rules for each conjunction: one inequality
// property, the first sort order on it ("The first sort property must be
// the same as the property to which the inequality filter is applied"), as
// Google's GQL reference documents for != and NOT IN too. The wording of
// the front's own errors is UNVERIFIED (re-read 2026-11-19): Google's
// pages quote none (https://cloud.google.com/datastore/docs/concepts/queries).

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Google's limits (above).
const (
	maxInValues      = 30
	maxNotInValues   = 10
	maxDisjunctions  = 30
	cursorMagic      = "cloudburrow-query-1:"
	maxMergedResults = 1 << 20
)

// needsOperators reports whether q has a filter the emulator does not
// serve: NOT_EQUAL, NOT_IN, IN, or an OR.
func needsOperators(q *pb.Query) bool {
	var walk func(f *pb.Filter) bool
	walk = func(f *pb.Filter) bool {
		if cf := f.GetCompositeFilter(); cf != nil {
			if cf.GetOp() == pb.CompositeFilter_OR {
				return true
			}
			for _, s := range cf.GetFilters() {
				if walk(s) {
					return true
				}
			}
			return false
		}
		switch f.GetPropertyFilter().GetOp() {
		case pb.PropertyFilter_NOT_EQUAL, pb.PropertyFilter_IN, pb.PropertyFilter_NOT_IN:
			return true
		}
		return false
	}
	return q != nil && q.GetFilter() != nil && walk(q.GetFilter())
}

// conjunction is one disjunction of a filter's normal form: property
// filters the emulator serves, ANDed.
type conjunction []*pb.PropertyFilter

// validateOperators checks q's filter against Google's limits.
func validateOperators(q *pb.Query) error {
	negations := 0
	var walk func(f *pb.Filter) error
	walk = func(f *pb.Filter) error {
		if cf := f.GetCompositeFilter(); cf != nil {
			if len(cf.GetFilters()) == 0 {
				return status.Error(codes.InvalidArgument, "A composite filter must have at least one sub-filter.")
			}
			for _, s := range cf.GetFilters() {
				if err := walk(s); err != nil {
					return err
				}
			}
			return nil
		}
		pf := f.GetPropertyFilter()
		switch pf.GetOp() {
		case pb.PropertyFilter_NOT_EQUAL:
			negations++
		case pb.PropertyFilter_IN, pb.PropertyFilter_NOT_IN:
			name, limit := "IN", maxInValues
			if pf.GetOp() == pb.PropertyFilter_NOT_IN {
				name, limit = "NOT_IN", maxNotInValues
				negations++
			}
			av := pf.GetValue().GetArrayValue()
			if av == nil {
				return status.Errorf(codes.InvalidArgument, "A filter with operator %s requires an array value.", name)
			}
			if n := len(av.GetValues()); n == 0 {
				return status.Errorf(codes.InvalidArgument, "A filter with operator %s requires a non-empty array value.", name)
			} else if n > limit {
				return status.Errorf(codes.InvalidArgument, "A filter with operator %s supports at most %d values, it has %d.", name, limit, n)
			}
			for _, v := range av.GetValues() {
				if v.GetArrayValue() != nil {
					return status.Errorf(codes.InvalidArgument, "A filter with operator %s cannot have an array within its array value.", name)
				}
			}
		}
		if negations > 1 {
			return status.Error(codes.InvalidArgument, "Only a single NOT_IN or NOT_EQUAL filter is allowed per query.")
		}
		return nil
	}
	if err := walk(q.GetFilter()); err != nil {
		return err
	}
	if n := disjunctions(q.GetFilter()); n > maxDisjunctions {
		return status.Errorf(codes.InvalidArgument, "The query has %d disjunctions in disjunctive normal form, more than the maximum of %d.", n, maxDisjunctions)
	}
	return nil
}

// disjunctions counts f's disjunctions in disjunctive normal form, an IN a
// disjunction per value; NOT_EQUAL and NOT_IN are conjunctions. Saturates
// past the limit.
func disjunctions(f *pb.Filter) int {
	if cf := f.GetCompositeFilter(); cf != nil {
		if cf.GetOp() == pb.CompositeFilter_OR {
			n := 0
			for _, s := range cf.GetFilters() {
				n += disjunctions(s)
				if n > maxDisjunctions {
					return n
				}
			}
			return n
		}
		n := 1
		for _, s := range cf.GetFilters() {
			n *= disjunctions(s)
			if n > maxDisjunctions {
				return n
			}
		}
		return n
	}
	if pf := f.GetPropertyFilter(); pf.GetOp() == pb.PropertyFilter_IN {
		return max(1, len(pf.GetValue().GetArrayValue().GetValues()))
	}
	return 1
}

// normalForm is f in disjunctive normal form, its NOT_EQUAL, IN and
// NOT_IN expanded (above).
func normalForm(f *pb.Filter) []conjunction {
	if f == nil {
		return []conjunction{nil}
	}
	if cf := f.GetCompositeFilter(); cf != nil {
		if cf.GetOp() == pb.CompositeFilter_OR {
			var out []conjunction
			for _, s := range cf.GetFilters() {
				out = append(out, normalForm(s)...)
			}
			return out
		}
		out := []conjunction{nil}
		for _, s := range cf.GetFilters() {
			var next []conjunction
			for _, a := range out {
				for _, b := range normalForm(s) {
					c := append(append(conjunction{}, a...), b...)
					next = append(next, c)
				}
			}
			out = next
		}
		return out
	}
	pf := f.GetPropertyFilter()
	with := func(op pb.PropertyFilter_Operator, v *pb.Value) *pb.PropertyFilter {
		return &pb.PropertyFilter{Property: pf.GetProperty(), Op: op, Value: v}
	}
	switch pf.GetOp() {
	case pb.PropertyFilter_IN:
		var out []conjunction
		for _, v := range distinctValues(pf.GetValue().GetArrayValue().GetValues()) {
			out = append(out, conjunction{with(pb.PropertyFilter_EQUAL, v)})
		}
		return out
	case pb.PropertyFilter_NOT_EQUAL:
		return []conjunction{{with(pb.PropertyFilter_LESS_THAN, pf.GetValue())}, {with(pb.PropertyFilter_GREATER_THAN, pf.GetValue())}}
	case pb.PropertyFilter_NOT_IN:
		vs := distinctValues(pf.GetValue().GetArrayValue().GetValues())
		out := []conjunction{{with(pb.PropertyFilter_LESS_THAN, vs[0])}}
		for i := 1; i < len(vs); i++ {
			out = append(out, conjunction{with(pb.PropertyFilter_GREATER_THAN, vs[i-1]), with(pb.PropertyFilter_LESS_THAN, vs[i])})
		}
		return append(out, conjunction{with(pb.PropertyFilter_GREATER_THAN, vs[len(vs)-1])})
	}
	return []conjunction{{pf}}
}

// distinctValues is vs sorted in Datastore's order, each value once.
func distinctValues(vs []*pb.Value) []*pb.Value {
	out := append([]*pb.Value(nil), vs...)
	sort.SliceStable(out, func(i, j int) bool { return compareValues(out[i], out[j]) < 0 })
	n := 0
	for i, v := range out {
		if i == 0 || compareValues(out[n-1], v) != 0 {
			out[n] = v
			n++
		}
	}
	return out[:n]
}

// ancestorOf is a conjunction's ancestor filter, as bytes, "" for none.
func ancestorOf(c conjunction) string {
	for _, f := range c {
		if f.GetOp() == pb.PropertyFilter_HAS_ANCESTOR {
			b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(f.GetValue())
			return "a" + string(b)
		}
	}
	return ""
}

// valueRank is a value's type in Datastore's value type ordering
// (https://cloud.google.com/datastore/docs/concepts/entities#value_type_ordering):
// null, integers and dates and times, booleans, byte strings, strings,
// doubles, geographical points, keys; then arrays and entities, which are
// not ordered.
func valueRank(v *pb.Value) int {
	switch v.GetValueType().(type) {
	case *pb.Value_NullValue, nil:
		return 0
	case *pb.Value_IntegerValue, *pb.Value_TimestampValue:
		return 1
	case *pb.Value_BooleanValue:
		return 2
	case *pb.Value_BlobValue:
		return 3
	case *pb.Value_StringValue:
		return 4
	case *pb.Value_DoubleValue:
		return 5
	case *pb.Value_GeoPointValue:
		return 6
	case *pb.Value_KeyValue:
		return 7
	}
	return 8
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpFloat(a, b float64) int {
	switch {
	case math.IsNaN(a) && math.IsNaN(b):
		return 0
	case math.IsNaN(a):
		return -1
	case math.IsNaN(b):
		return 1
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// fixedPoint is an integer, or a timestamp in microseconds, which
// Datastore orders together.
func fixedPoint(v *pb.Value) int64 {
	if t := v.GetTimestampValue(); t != nil {
		return t.GetSeconds()*1e6 + int64(t.GetNanos())/1e3
	}
	return v.GetIntegerValue()
}

// compareValues orders two values as Datastore does.
func compareValues(a, b *pb.Value) int {
	if c := cmpInt(int64(valueRank(a)), int64(valueRank(b))); c != 0 {
		return c
	}
	switch valueRank(a) {
	case 1:
		return cmpInt(fixedPoint(a), fixedPoint(b))
	case 2:
		x, y := a.GetBooleanValue(), b.GetBooleanValue()
		if x == y {
			return 0
		}
		if !x {
			return -1
		}
		return 1
	case 3:
		return bytes.Compare(a.GetBlobValue(), b.GetBlobValue())
	case 4:
		return strings.Compare(a.GetStringValue(), b.GetStringValue())
	case 5:
		return cmpFloat(a.GetDoubleValue(), b.GetDoubleValue())
	case 6:
		if c := cmpFloat(a.GetGeoPointValue().GetLatitude(), b.GetGeoPointValue().GetLatitude()); c != 0 {
			return c
		}
		return cmpFloat(a.GetGeoPointValue().GetLongitude(), b.GetGeoPointValue().GetLongitude())
	case 7:
		return compareKeys(a.GetKeyValue(), b.GetKeyValue())
	}
	return 0
}

// compareKeys orders keys by their paths, element by element: kind, then
// an ID before a name, IDs numerically and names as strings; an ancestor
// before its descendants.
func compareKeys(a, b *pb.Key) int {
	if c := strings.Compare(a.GetPartitionId().GetNamespaceId(), b.GetPartitionId().GetNamespaceId()); c != 0 {
		return c
	}
	pa, pb_ := a.GetPath(), b.GetPath()
	for i := 0; i < len(pa) && i < len(pb_); i++ {
		x, y := pa[i], pb_[i]
		if c := strings.Compare(x.GetKind(), y.GetKind()); c != 0 {
			return c
		}
		_, xName := x.GetIdType().(*pb.Key_PathElement_Name)
		_, yName := y.GetIdType().(*pb.Key_PathElement_Name)
		switch {
		case xName && !yName:
			return 1
		case !xName && yName:
			return -1
		case xName:
			if c := strings.Compare(x.GetName(), y.GetName()); c != 0 {
				return c
			}
		default:
			if c := cmpInt(x.GetId(), y.GetId()); c != 0 {
				return c
			}
		}
	}
	return cmpInt(int64(len(pa)), int64(len(pb_)))
}

// satisfies reports whether value v satisfies property filter f.
func satisfies(v *pb.Value, f *pb.PropertyFilter) bool {
	c := compareValues(v, f.GetValue())
	switch f.GetOp() {
	case pb.PropertyFilter_EQUAL:
		return c == 0
	case pb.PropertyFilter_LESS_THAN:
		return c < 0
	case pb.PropertyFilter_LESS_THAN_OR_EQUAL:
		return c <= 0
	case pb.PropertyFilter_GREATER_THAN:
		return c > 0
	case pb.PropertyFilter_GREATER_THAN_OR_EQUAL:
		return c >= 0
	}
	return true
}

// merged is one result of a merged query, at its position: the values it
// is ordered by, then its key.
type merged struct {
	result *pb.EntityResult
	order  []*pb.Value
	ident  string // the key, and a projection's values
}

// orderValues are the values entity e of conjunction c is ordered by in q:
// for each order, of the values of its property that satisfy c's filters
// on it, the smallest for an ascending order and the largest for a
// descending one.
func orderValues(e *pb.Entity, q *pb.Query, c conjunction) []*pb.Value {
	out := make([]*pb.Value, 0, len(q.GetOrder()))
	for _, o := range q.GetOrder() {
		name := o.GetProperty().GetName()
		if name == "__key__" {
			out = append(out, &pb.Value{ValueType: &pb.Value_KeyValue{KeyValue: e.GetKey()}})
			continue
		}
		var vals []*pb.Value
		if v := property(e, name); v != nil {
			if av := v.GetArrayValue(); av != nil {
				vals = av.GetValues()
			} else {
				vals = []*pb.Value{v}
			}
		}
		var best *pb.Value
		desc := o.GetDirection() == pb.PropertyOrder_DESCENDING
	values:
		for _, v := range vals {
			for _, f := range c {
				if f.GetProperty().GetName() == name && !satisfies(v, f) {
					continue values
				}
			}
			if best == nil || (!desc && compareValues(v, best) < 0) || (desc && compareValues(v, best) > 0) {
				best = v
			}
		}
		if best == nil {
			best = &pb.Value{ValueType: &pb.Value_NullValue{}}
		}
		out = append(out, best)
	}
	return out
}

// comparePositions orders two positions in q's order.
func comparePositions(q *pb.Query, a, b []*pb.Value, ka, kb *pb.Key) int {
	for i, o := range q.GetOrder() {
		if i >= len(a) || i >= len(b) {
			break
		}
		c := compareValues(a[i], b[i])
		if o.GetDirection() == pb.PropertyOrder_DESCENDING {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return compareKeys(ka, kb)
}

// encodeCursor is the front's cursor at a position.
func encodeCursor(order []*pb.Value, key *pb.Key) []byte {
	props := make(map[string]*pb.Value, len(order))
	for i, v := range order {
		props[fmt.Sprintf("o%d", i)] = v
	}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(&pb.Entity{Key: key, Properties: props})
	return append([]byte(cursorMagic), b...)
}

// decodeCursor is the position of a cursor of the front's.
func decodeCursor(b []byte, orders int) ([]*pb.Value, *pb.Key, error) {
	rest, ok := bytes.CutPrefix(b, []byte(cursorMagic))
	e := &pb.Entity{}
	if !ok || proto.Unmarshal(rest, e) != nil || len(e.GetProperties()) != orders || e.GetKey() == nil {
		return nil, nil, status.Error(codes.InvalidArgument, "Invalid query cursor: it is not a cursor of this query.")
	}
	out := make([]*pb.Value, orders)
	for i := range out {
		v, ok := e.GetProperties()[fmt.Sprintf("o%d", i)]
		if !ok {
			return nil, nil, status.Error(codes.InvalidArgument, "Invalid query cursor: it is not a cursor of this query.")
		}
		out[i] = v
	}
	return out, e.GetKey(), nil
}

// keysOnly reports whether q projects the key alone.
func keysOnly(q *pb.Query) bool {
	p := q.GetProjection()
	return len(p) == 1 && p[0].GetProperty().GetName() == "__key__"
}

// runOperators answers a RunQuery whose structured query has a filter the
// emulator does not serve (above).
func (d *Datastore) runOperators(ctx context.Context, client pb.DatastoreClient, req *pb.RunQueryRequest) (*pb.RunQueryResponse, error) {
	q := req.GetQuery()
	if err := validateOperators(q); err != nil {
		return nil, err
	}
	conjs := normalForm(q.GetFilter())
	for _, c := range conjs[1:] {
		if ancestorOf(c) != ancestorOf(conjs[0]) {
			return nil, status.Error(codes.InvalidArgument, "All evaluated disjunctions must have the same ancestor filter.")
		}
	}
	var start, end []*pb.Value
	var startKey, endKey *pb.Key
	var err error
	if len(q.GetStartCursor()) > 0 {
		if start, startKey, err = decodeCursor(q.GetStartCursor(), len(q.GetOrder())); err != nil {
			return nil, err
		}
	}
	if len(q.GetEndCursor()) > 0 {
		if end, endKey, err = decodeCursor(q.GetEndCursor(), len(q.GetOrder())); err != nil {
			return nil, err
		}
	}
	offset := int64(q.GetOffset())
	limit := int64(-1)
	if q.GetLimit() != nil {
		limit = int64(q.GetLimit().GetValue())
		if limit < 0 {
			return nil, status.Error(codes.InvalidArgument, "The query limit must not be negative.")
		}
	}
	// Each conjunction needs at most offset+limit results, when no
	// cursor skips some of them first.
	perQuery := int64(-1)
	if limit >= 0 && start == nil {
		perQuery = offset + limit
	}
	// The emulator's projection, unless it is the key alone: the front
	// needs the ordered properties, and projects keys itself.
	full := keysOnly(q)

	ro := req.GetReadOptions()
	var txn []byte
	var readTime = (*pb.QueryResultBatch)(nil)
	best := map[string]*merged{}
	for _, c := range conjs {
		sub := proto.Clone(q).(*pb.Query)
		sub.StartCursor, sub.EndCursor, sub.Offset, sub.Limit = nil, nil, 0, nil
		if full {
			sub.Projection = nil
		}
		switch len(c) {
		case 0:
			sub.Filter = nil
		case 1:
			sub.Filter = &pb.Filter{FilterType: &pb.Filter_PropertyFilter{PropertyFilter: c[0]}}
		default:
			fs := make([]*pb.Filter, len(c))
			for i, f := range c {
				fs[i] = &pb.Filter{FilterType: &pb.Filter_PropertyFilter{PropertyFilter: f}}
			}
			sub.Filter = &pb.Filter{FilterType: &pb.Filter_CompositeFilter{CompositeFilter: &pb.CompositeFilter{Op: pb.CompositeFilter_AND, Filters: fs}}}
		}
		if perQuery >= 0 {
			sub.Limit = wrapperspb.Int32(int32(min(perQuery, math.MaxInt32)))
		}
		var got int64
		for {
			resp, err := client.RunQuery(ctx, &pb.RunQueryRequest{ProjectId: req.GetProjectId(), DatabaseId: req.GetDatabaseId(),
				PartitionId: req.GetPartitionId(), ReadOptions: ro, QueryType: &pb.RunQueryRequest_Query{Query: sub}})
			if err != nil {
				return nil, err
			}
			if t := resp.GetTransaction(); len(t) > 0 {
				txn = t
				ro = &pb.ReadOptions{ConsistencyType: &pb.ReadOptions_Transaction{Transaction: t}}
			}
			b := resp.GetBatch()
			readTime = b
			for _, er := range b.GetEntityResults() {
				got++
				e := er.GetEntity()
				m := &merged{result: er, order: orderValues(e, q, c)}
				m.ident = identity(e, sub)
				if start != nil && comparePositions(q, m.order, start, e.GetKey(), startKey) <= 0 {
					continue
				}
				if end != nil && comparePositions(q, m.order, end, e.GetKey(), endKey) > 0 {
					continue
				}
				if old, ok := best[m.ident]; !ok || comparePositions(q, m.order, old.order, e.GetKey(), old.result.GetEntity().GetKey()) < 0 {
					best[m.ident] = m
				}
				if len(best) > maxMergedResults {
					return nil, status.Error(codes.ResourceExhausted, "the query matches too many entities for the Datastore emulator's front to merge")
				}
			}
			if b.GetMoreResults() != pb.QueryResultBatch_NOT_FINISHED || (perQuery >= 0 && got >= perQuery) {
				break
			}
			if len(b.GetEntityResults()) == 0 {
				return nil, status.Error(codes.Internal, "the emulator's query made no progress")
			}
			sub = proto.Clone(sub).(*pb.Query)
			sub.StartCursor = b.GetEndCursor()
			if sub.Limit != nil {
				sub.Limit.Value = int32(min(perQuery-got, math.MaxInt32))
			}
		}
	}
	all := make([]*merged, 0, len(best))
	for _, m := range best {
		all = append(all, m)
	}
	sort.Slice(all, func(i, j int) bool {
		c := comparePositions(q, all[i].order, all[j].order, all[i].result.GetEntity().GetKey(), all[j].result.GetEntity().GetKey())
		if c == 0 {
			return all[i].ident < all[j].ident
		}
		return c < 0
	})
	if len(q.GetDistinctOn()) > 0 {
		all = distinct(all, q)
	}
	batch := &pb.QueryResultBatch{EntityResultType: resultType(q), MoreResults: pb.QueryResultBatch_NO_MORE_RESULTS}
	if readTime != nil {
		batch.ReadTime = readTime.GetReadTime()
		batch.SnapshotVersion = readTime.GetSnapshotVersion()
	}
	cursorOf := func(m *merged) []byte { return encodeCursor(m.order, m.result.GetEntity().GetKey()) }
	skipped := min(offset, int64(len(all)))
	batch.SkippedResults = int32(skipped)
	if skipped > 0 {
		batch.SkippedCursor = cursorOf(all[skipped-1])
	}
	rest := all[skipped:]
	if limit >= 0 && int64(len(rest)) > limit {
		rest = rest[:limit]
		batch.MoreResults = pb.QueryResultBatch_MORE_RESULTS_AFTER_LIMIT
	} else if limit >= 0 && int64(len(rest)) == limit && limit > 0 {
		batch.MoreResults = pb.QueryResultBatch_MORE_RESULTS_AFTER_LIMIT
	}
	for _, m := range rest {
		er := proto.Clone(m.result).(*pb.EntityResult)
		if full {
			er.Entity = &pb.Entity{Key: er.GetEntity().GetKey()}
		}
		er.Cursor = cursorOf(m)
		batch.EntityResults = append(batch.EntityResults, er)
	}
	switch {
	case len(rest) > 0:
		batch.EndCursor = cursorOf(rest[len(rest)-1])
	case skipped > 0:
		batch.EndCursor = batch.SkippedCursor
	default:
		batch.EndCursor = q.GetStartCursor()
	}
	out := &pb.RunQueryResponse{Batch: batch}
	if req.GetReadOptions().GetNewTransaction() != nil {
		out.Transaction = txn
	}
	return out, nil
}

// identity is what tells one result of q from another: its key, and, for
// a projection, its projected values (one result for each value of an
// array).
func identity(e *pb.Entity, q *pb.Query) string {
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(e.GetKey())
	if len(q.GetProjection()) == 0 {
		return string(b)
	}
	var s strings.Builder
	s.Write(b)
	for _, p := range q.GetProjection() {
		v, _ := proto.MarshalOptions{Deterministic: true}.Marshal(e.GetProperties()[p.GetProperty().GetName()])
		s.WriteString("\x00")
		s.Write(v)
	}
	return s.String()
}

// distinct keeps the first result of each combination of q's distinct_on
// properties' values.
func distinct(all []*merged, q *pb.Query) []*merged {
	seen := map[string]bool{}
	out := all[:0]
	for _, m := range all {
		var s strings.Builder
		for _, p := range q.GetDistinctOn() {
			v, _ := proto.MarshalOptions{Deterministic: true}.Marshal(m.result.GetEntity().GetProperties()[p.GetName()])
			s.Write(v)
			s.WriteString("\x00")
		}
		if !seen[s.String()] {
			seen[s.String()] = true
			out = append(out, m)
		}
	}
	return out
}
