package queryfront

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	dspb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// filteringDatastore is an emulator of conjunctions: it serves EQUAL and
// range filters ANDed, ordered by the query's orders and the key, two
// results per batch, and refuses what the emulator refuses (#1888).
type filteringDatastore struct {
	dspb.UnimplementedDatastoreServer
	entities []*dspb.Entity
	calls    int
}

func (f *filteringDatastore) RunQuery(_ context.Context, req *dspb.RunQueryRequest) (*dspb.RunQueryResponse, error) {
	f.calls++
	q := req.GetQuery()
	var conj []*dspb.PropertyFilter
	var walk func(*dspb.Filter) error
	walk = func(fl *dspb.Filter) error {
		if cf := fl.GetCompositeFilter(); cf != nil {
			if cf.GetOp() != dspb.CompositeFilter_AND {
				return status.Error(codes.InvalidArgument, "unsupported composite property operator")
			}
			for _, s := range cf.GetFilters() {
				if err := walk(s); err != nil {
					return err
				}
			}
			return nil
		}
		pf := fl.GetPropertyFilter()
		switch pf.GetOp() {
		case dspb.PropertyFilter_NOT_EQUAL, dspb.PropertyFilter_IN, dspb.PropertyFilter_NOT_IN:
			return status.Error(codes.InvalidArgument, "refused")
		}
		conj = append(conj, pf)
		return nil
	}
	if q.GetFilter() != nil {
		if err := walk(q.GetFilter()); err != nil {
			return nil, err
		}
	}
	type hit struct {
		e     *dspb.Entity
		order []*dspb.Value
	}
	var hits []hit
	for _, e := range f.entities {
		// One value of a property satisfies all of its filters.
		ok := true
		for _, pf := range conj {
			v := e.GetProperties()[pf.GetProperty().GetName()]
			if v == nil {
				ok = false
				break
			}
			vals := []*dspb.Value{v}
			if av := v.GetArrayValue(); av != nil {
				vals = av.GetValues()
			}
			any := false
			for _, x := range vals {
				all := true
				for _, other := range conj {
					if other.GetProperty().GetName() == pf.GetProperty().GetName() {
						all = all && satisfies(x, other)
					}
				}
				any = any || all
			}
			ok = ok && any
		}
		if ok {
			hits = append(hits, hit{e, orderValues(e, q, conj)})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		return comparePositions(q, hits[i].order, hits[j].order, hits[i].e.GetKey(), hits[j].e.GetKey()) < 0
	})
	start := 0
	if c := q.GetStartCursor(); len(c) > 0 {
		fmt.Sscan(string(c), &start)
	}
	end := len(hits)
	if q.Limit != nil && start+int(q.GetLimit().GetValue()) < end {
		end = start + int(q.GetLimit().GetValue())
	}
	more := dspb.QueryResultBatch_NO_MORE_RESULTS
	if end-start > 2 {
		end, more = start+2, dspb.QueryResultBatch_NOT_FINISHED
	}
	b := &dspb.QueryResultBatch{MoreResults: more, EndCursor: []byte(fmt.Sprint(end))}
	for _, h := range hits[start:end] {
		b.EntityResults = append(b.EntityResults, &dspb.EntityResult{Entity: h.e})
	}
	return &dspb.RunQueryResponse{Batch: b}, nil
}

func keyed(id int64, props map[string]*dspb.Value) *dspb.Entity {
	return &dspb.Entity{Key: &dspb.Key{Path: []*dspb.Key_PathElement{{Kind: "K", IdType: &dspb.Key_PathElement_Id{Id: id}}}}, Properties: props}
}

func arrval(vs ...*dspb.Value) *dspb.Value {
	return &dspb.Value{ValueType: &dspb.Value_ArrayValue{ArrayValue: &dspb.ArrayValue{Values: vs}}}
}

func pfilter(name string, op dspb.PropertyFilter_Operator, v *dspb.Value) *dspb.Filter {
	return &dspb.Filter{FilterType: &dspb.Filter_PropertyFilter{PropertyFilter: &dspb.PropertyFilter{
		Property: &dspb.PropertyReference{Name: name}, Op: op, Value: v}}}
}

func composite(op dspb.CompositeFilter_Operator, fs ...*dspb.Filter) *dspb.Filter {
	return &dspb.Filter{FilterType: &dspb.Filter_CompositeFilter{CompositeFilter: &dspb.CompositeFilter{Op: op, Filters: fs}}}
}

func operatorsFront(t *testing.T) (*Datastore, *filteringDatastore) {
	f := &filteringDatastore{}
	for i := int64(1); i <= 6; i++ {
		props := map[string]*dspb.Value{"I": ival(i), "J": ival(10 - i)}
		if i == 6 {
			props["I"] = arrval(ival(2), ival(9)) // an array: 2 and 9
		}
		f.entities = append(f.entities, keyed(i, props))
	}
	f.entities = append(f.entities, keyed(7, map[string]*dspb.Value{"J": ival(0)})) // no I
	return NewDatastore(serve(t, func(s *grpc.Server) { dspb.RegisterDatastoreServer(s, f) })), f
}

func ids(t *testing.T, resp *dspb.RunQueryResponse, err error) string {
	t.Helper()
	return idsOf(t, resp, err)
}

func idsOf(t *testing.T, resp *dspb.RunQueryResponse, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	var out []string
	for _, r := range resp.GetBatch().GetEntityResults() {
		out = append(out, fmt.Sprint(r.GetEntity().GetKey().GetPath()[0].GetId()))
	}
	return strings.Join(out, ",")
}

func TestOperatorsAreServedByMergingConjunctions(t *testing.T) {
	d, _ := operatorsFront(t)
	ctx := context.Background()
	run := func(q *dspb.Query) (*dspb.RunQueryResponse, error) {
		q.Kind = []*dspb.KindExpression{{Name: "K"}}
		return d.RunQuery(ctx, &dspb.RunQueryRequest{ProjectId: "p", QueryType: &dspb.RunQueryRequest_Query{Query: q}})
	}
	asc := []*dspb.PropertyOrder{{Property: &dspb.PropertyReference{Name: "I"}}}
	desc := []*dspb.PropertyOrder{{Property: &dspb.PropertyReference{Name: "I"}, Direction: dspb.PropertyOrder_DESCENDING}}
	for _, c := range []struct {
		name string
		q    *dspb.Query
		want string
	}{
		// 7 has no I: != and NOT_IN match only entities with the property.
		{"!=", &dspb.Query{Filter: pfilter("I", dspb.PropertyFilter_NOT_EQUAL, ival(2))}, "1,3,4,5,6"},
		// Ordered by I, 6 at its smallest value satisfying the filter (9 for != 2).
		{"!= ordered", &dspb.Query{Filter: pfilter("I", dspb.PropertyFilter_NOT_EQUAL, ival(2)), Order: asc}, "1,3,4,5,6"},
		{"!= descending", &dspb.Query{Filter: pfilter("I", dspb.PropertyFilter_NOT_EQUAL, ival(3)), Order: desc}, "6,5,4,2,1"},
		{"IN", &dspb.Query{Filter: pfilter("I", dspb.PropertyFilter_IN, arrval(ival(4), ival(2), ival(5)))}, "2,4,5,6"},
		{"IN ordered", &dspb.Query{Filter: pfilter("I", dspb.PropertyFilter_IN, arrval(ival(4), ival(2), ival(5))), Order: asc}, "2,6,4,5"},
		{"NOT_IN", &dspb.Query{Filter: pfilter("I", dspb.PropertyFilter_NOT_IN, arrval(ival(2), ival(9), ival(4)))}, "1,3,5"},
		{"OR", &dspb.Query{Filter: composite(dspb.CompositeFilter_OR, pfilter("I", dspb.PropertyFilter_EQUAL, ival(1)), pfilter("J", dspb.PropertyFilter_LESS_THAN, ival(5)))}, "1,6,7"},
		{"AND of OR", &dspb.Query{Filter: composite(dspb.CompositeFilter_AND, pfilter("J", dspb.PropertyFilter_GREATER_THAN, ival(5)),
			composite(dspb.CompositeFilter_OR, pfilter("I", dspb.PropertyFilter_EQUAL, ival(1)), pfilter("I", dspb.PropertyFilter_EQUAL, ival(3)), pfilter("I", dspb.PropertyFilter_EQUAL, ival(4))))}, "1,3,4"},
		{"offset and limit", &dspb.Query{Filter: pfilter("I", dspb.PropertyFilter_NOT_EQUAL, ival(2)), Order: asc, Offset: 1, Limit: wrapperspb.Int32(2)}, "3,4"},
	} {
		if got := idsOf2(t)(run(c.q)); got != c.want {
			t.Errorf("%s = %s, want %s", c.name, got, c.want)
		}
	}

	// Cursors resume after the result they were taken at, and the end
	// cursor after the batch.
	q := &dspb.Query{Filter: pfilter("I", dspb.PropertyFilter_NOT_EQUAL, ival(2)), Order: asc, Limit: wrapperspb.Int32(2)}
	resp, err := run(q)
	if got := ids(t, resp, err); got != "1,3" || resp.GetBatch().GetMoreResults() != dspb.QueryResultBatch_MORE_RESULTS_AFTER_LIMIT {
		t.Fatalf("first page = %s, %v", got, resp.GetBatch().GetMoreResults())
	}
	q.StartCursor = resp.GetBatch().GetEndCursor()
	if got := idsOf2(t)(run(q)); got != "4,5" {
		t.Errorf("second page = %s, want 4,5", got)
	}
	q.StartCursor = resp.GetBatch().GetEntityResults()[0].GetCursor()
	q.Limit = nil
	if got := idsOf2(t)(run(q)); got != "3,4,5,6" {
		t.Errorf("from the first result's cursor = %s, want 3,4,5,6", got)
	}
	q.StartCursor, q.EndCursor = nil, resp.GetBatch().GetEndCursor()
	if got := idsOf2(t)(run(q)); got != "1,3" {
		t.Errorf("to the end cursor = %s, want 1,3", got)
	}

	// COUNT, SUM and AVG over them.
	agg, err := d.RunAggregationQuery(ctx, aggReq(&dspb.Query{Kind: []*dspb.KindExpression{{Name: "K"}},
		Filter: pfilter("I", dspb.PropertyFilter_IN, arrval(ival(1), ival(3)))}, count("n", nil), sum("s", "J"), avg("a", "J")))
	got := resultsOf(t, agg, err)
	if got["n"] != "int 2" || got["s"] != "int 16" || got["a"] != "double 8" {
		t.Errorf("aggregation over IN = %v", got)
	}
}

func TestOperatorLimitsAreGooglesErrors(t *testing.T) {
	d, f := operatorsFront(t)
	var thirtyOne []*dspb.Value
	for i := int64(0); i < 31; i++ {
		thirtyOne = append(thirtyOne, ival(i))
	}
	var ors []*dspb.Filter
	for i := int64(0); i < 31; i++ {
		ors = append(ors, pfilter("I", dspb.PropertyFilter_EQUAL, ival(i)))
	}
	for name, fl := range map[string]*dspb.Filter{
		"IN of 31":     pfilter("I", dspb.PropertyFilter_IN, arrval(thirtyOne...)),
		"NOT_IN of 11": pfilter("I", dspb.PropertyFilter_NOT_IN, arrval(thirtyOne[:11]...)),
		"empty IN":     pfilter("I", dspb.PropertyFilter_IN, arrval()),
		"IN no array":  pfilter("I", dspb.PropertyFilter_IN, ival(1)),
		"two !=": composite(dspb.CompositeFilter_AND, pfilter("I", dspb.PropertyFilter_NOT_EQUAL, ival(1)),
			pfilter("J", dspb.PropertyFilter_NOT_EQUAL, ival(1))),
		"!= and NOT_IN": composite(dspb.CompositeFilter_AND, pfilter("I", dspb.PropertyFilter_NOT_EQUAL, ival(1)),
			pfilter("I", dspb.PropertyFilter_NOT_IN, arrval(ival(2)))),
		"31 disjunctions": composite(dspb.CompositeFilter_OR, ors...),
		"IN × IN past 30": composite(dspb.CompositeFilter_AND, pfilter("I", dspb.PropertyFilter_IN, arrval(thirtyOne[:6]...)),
			pfilter("J", dspb.PropertyFilter_IN, arrval(thirtyOne[:6]...))),
		"different ancestors": composite(dspb.CompositeFilter_OR,
			pfilter("__key__", dspb.PropertyFilter_HAS_ANCESTOR, &dspb.Value{ValueType: &dspb.Value_KeyValue{KeyValue: keyed(1, nil).GetKey()}}),
			pfilter("I", dspb.PropertyFilter_EQUAL, ival(1))),
	} {
		f.calls = 0
		_, err := d.RunQuery(context.Background(), &dspb.RunQueryRequest{ProjectId: "p", QueryType: &dspb.RunQueryRequest_Query{
			Query: &dspb.Query{Kind: []*dspb.KindExpression{{Name: "K"}}, Filter: fl}}})
		if status.Code(err) != codes.InvalidArgument || f.calls != 0 {
			t.Errorf("%s: %v after %d emulator calls, want INVALID_ARGUMENT before any", name, err, f.calls)
		}
	}
	// Thirty values are allowed.
	_, err := d.RunQuery(context.Background(), &dspb.RunQueryRequest{ProjectId: "p", QueryType: &dspb.RunQueryRequest_Query{
		Query: &dspb.Query{Kind: []*dspb.KindExpression{{Name: "K"}}, Filter: pfilter("I", dspb.PropertyFilter_IN, arrval(thirtyOne[:30]...))}}})
	if err != nil {
		t.Errorf("IN of 30: %v", err)
	}
}

func TestGQLConditionTree(t *testing.T) {
	for q, want := range map[string]string{
		"SELECT * FROM K WHERE a != 1 AND b IN ARRAY(1, KEY(K, 'x'), 3) OR (c NOT IN @v AND 4 < d) ORDER BY a": "a = 1|b = 1|b = KEY(K, 'x')|b = 3|c IS NULL|d = 4",
		"SELECT * FROM K WHERE a IS NULL OR __key__ HAS ANCESTOR KEY(K, 1)":                                    "a IS NULL|__key__ HAS ANCESTOR KEY(K, 1)",
	} {
		toks := gqlTokens(q)
		p := &gqlParser{q: q, toks: toks, bind: map[string]*dspb.GqlQueryParameter{"v": {ParameterType: &dspb.GqlQueryParameter_Value{Value: arrval(ival(1))}}}}
		stop := len(toks)
		for i := range toks {
			if p.text(i) == "ORDER" {
				stop = i
			}
		}
		for i := range toks {
			if p.text(i) == "WHERE" {
				p.i = i + 1
			}
		}
		p.end = stop
		tree, err := p.expr()
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got := strings.Join(tree.collect(nil), "|"); got != want {
			t.Errorf("%s: items %s, want %s", q, got, want)
		}
	}
	if !gqlNeedsOperators("SELECT * FROM K WHERE a != 1") || gqlNeedsOperators("SELECT * FROM K WHERE a = 'NOT IN'") {
		t.Error("gqlNeedsOperators")
	}
	g := namedBindings(&dspb.GqlQuery{QueryString: "SELECT * FROM K WHERE a = @1 AND b = '@2'", PositionalBindings: []*dspb.GqlQueryParameter{{}}})
	if g.GetQueryString() != "SELECT * FROM K WHERE a = @"+gqlPositionalPrefix+"1 AND b = '@2'" || len(g.GetNamedBindings()) != 1 {
		t.Errorf("namedBindings = %v", g)
	}
}

func idsOf2(t *testing.T) func(*dspb.RunQueryResponse, error) string {
	return func(resp *dspb.RunQueryResponse, err error) string { return idsOf(t, resp, err) }
}
