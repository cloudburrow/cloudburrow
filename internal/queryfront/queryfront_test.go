package queryfront

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dspb "cloud.google.com/go/datastore/apiv1/datastorepb"
	fspb "cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// fakeDatastore serves RunQuery over entities, two per batch.
type fakeDatastore struct {
	dspb.UnimplementedDatastoreServer
	entities []*dspb.Entity
	calls    int
}

func (f *fakeDatastore) RunQuery(_ context.Context, req *dspb.RunQueryRequest) (*dspb.RunQueryResponse, error) {
	f.calls++
	q := req.GetQuery()
	start := 0
	if c := q.GetStartCursor(); len(c) > 0 {
		fmt.Sscan(string(c), &start)
	}
	end := len(f.entities)
	if q.Limit != nil && start+int(q.GetLimit().GetValue()) < end {
		end = start + int(q.GetLimit().GetValue())
	}
	more := dspb.QueryResultBatch_NO_MORE_RESULTS
	if end-start > 2 {
		end, more = start+2, dspb.QueryResultBatch_NOT_FINISHED
	}
	b := &dspb.QueryResultBatch{MoreResults: more, EndCursor: []byte(fmt.Sprint(end))}
	for _, e := range f.entities[start:end] {
		b.EntityResults = append(b.EntityResults, &dspb.EntityResult{Entity: e})
	}
	resp := &dspb.RunQueryResponse{Batch: b}
	if req.GetReadOptions().GetNewTransaction() != nil {
		resp.Transaction = []byte("tx")
	}
	return resp, nil
}

func serve(t *testing.T, register func(*grpc.Server)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	register(s)
	go func() { _ = s.Serve(l) }()
	t.Cleanup(s.Stop)
	return l.Addr().String()
}

func ival(i int64) *dspb.Value {
	return &dspb.Value{ValueType: &dspb.Value_IntegerValue{IntegerValue: i}}
}
func dval(d float64) *dspb.Value {
	return &dspb.Value{ValueType: &dspb.Value_DoubleValue{DoubleValue: d}}
}
func ent(props map[string]*dspb.Value) *dspb.Entity {
	return &dspb.Entity{Properties: props}
}

func count(alias string, upTo *int64) *dspb.AggregationQuery_Aggregation {
	c := &dspb.AggregationQuery_Aggregation_Count{}
	if upTo != nil {
		c.UpTo = wrapperspb.Int64(*upTo)
	}
	return &dspb.AggregationQuery_Aggregation{Alias: alias, Operator: &dspb.AggregationQuery_Aggregation_Count_{Count: c}}
}
func sum(alias, p string) *dspb.AggregationQuery_Aggregation {
	return &dspb.AggregationQuery_Aggregation{Alias: alias, Operator: &dspb.AggregationQuery_Aggregation_Sum_{
		Sum: &dspb.AggregationQuery_Aggregation_Sum{Property: &dspb.PropertyReference{Name: p}}}}
}
func avg(alias, p string) *dspb.AggregationQuery_Aggregation {
	return &dspb.AggregationQuery_Aggregation{Alias: alias, Operator: &dspb.AggregationQuery_Aggregation_Avg_{
		Avg: &dspb.AggregationQuery_Aggregation_Avg{Property: &dspb.PropertyReference{Name: p}}}}
}

func aggReq(q *dspb.Query, aggs ...*dspb.AggregationQuery_Aggregation) *dspb.RunAggregationQueryRequest {
	return &dspb.RunAggregationQueryRequest{ProjectId: "p", QueryType: &dspb.RunAggregationQueryRequest_AggregationQuery{
		AggregationQuery: &dspb.AggregationQuery{QueryType: &dspb.AggregationQuery_NestedQuery{NestedQuery: q}, Aggregations: aggs}}}
}

func newDatastore(t *testing.T, ents ...*dspb.Entity) (*Datastore, *fakeDatastore) {
	f := &fakeDatastore{entities: ents}
	return NewDatastore(serve(t, func(s *grpc.Server) { dspb.RegisterDatastoreServer(s, f) })), f
}

func results(t *testing.T) func(*dspb.RunAggregationQueryResponse, error) map[string]string {
	return func(resp *dspb.RunAggregationQueryResponse, err error) map[string]string {
		return resultsOf(t, resp, err)
	}
}

func resultsOf(t *testing.T, resp *dspb.RunAggregationQueryResponse, err error) map[string]string {
	t.Helper()
	if err != nil {
		t.Fatalf("RunAggregationQuery: %v", err)
	}
	out := map[string]string{}
	for k, v := range resp.GetBatch().GetAggregationResults()[0].GetAggregateProperties() {
		switch x := v.GetValueType().(type) {
		case *dspb.Value_IntegerValue:
			out[k] = fmt.Sprintf("int %d", x.IntegerValue)
		case *dspb.Value_DoubleValue:
			out[k] = fmt.Sprintf("double %g", x.DoubleValue)
		case *dspb.Value_NullValue:
			out[k] = "null"
		}
	}
	if resp.GetBatch().GetReadTime() == nil || resp.GetBatch().GetMoreResults() != dspb.QueryResultBatch_NO_MORE_RESULTS {
		t.Errorf("batch = %v, want a read time and NO_MORE_RESULTS", resp.GetBatch())
	}
	return out
}

func TestAggregationAcrossBatches(t *testing.T) {
	str := &dspb.Value{ValueType: &dspb.Value_StringValue{StringValue: "x"}}
	null := &dspb.Value{ValueType: &dspb.Value_NullValue{}}
	unindexed := ival(1000)
	unindexed.ExcludeFromIndexes = true
	d, f := newDatastore(t,
		ent(map[string]*dspb.Value{"i": ival(1), "v": dval(1.5), "s": str}),
		ent(map[string]*dspb.Value{"i": ival(2), "v": ival(2)}),
		ent(map[string]*dspb.Value{"i": str, "v": null}),
		ent(map[string]*dspb.Value{"i": unindexed}),
		ent(map[string]*dspb.Value{"e": {ValueType: &dspb.Value_EntityValue{EntityValue: ent(map[string]*dspb.Value{"n": ival(7)})}}}),
	)
	three := int64(3)
	got := results(t)(d.RunAggregationQuery(context.Background(), aggReq(&dspb.Query{},
		count("", nil), count("upto", &three), sum("", "i"), avg("mean", "v"), sum("deep", "e.n"))))
	want := map[string]string{"property_1": "int 5", "upto": "int 3", "property_2": "int 3", "mean": "double 1.75", "deep": "int 7"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if f.calls != 3 {
		t.Errorf("RunQuery calls = %d, want 3 (every batch)", f.calls)
	}
}

func TestAggregationEmptyAndNonNumeric(t *testing.T) {
	d, _ := newDatastore(t, ent(map[string]*dspb.Value{"w": {ValueType: &dspb.Value_StringValue{StringValue: "s"}}}))
	got := results(t)(d.RunAggregationQuery(context.Background(), aggReq(&dspb.Query{}, sum("s", "w"), avg("a", "w"))))
	if got["s"] != "int 0" || got["a"] != "null" {
		t.Errorf("sum, avg over non-numeric = %v, want int 0 and null", got)
	}
}

func TestAggregationOverflowNaNAndLimit(t *testing.T) {
	d, _ := newDatastore(t,
		ent(map[string]*dspb.Value{"i": ival(math.MaxInt64), "n": dval(1)}),
		ent(map[string]*dspb.Value{"i": ival(10), "n": dval(math.NaN())}),
		ent(map[string]*dspb.Value{"i": ival(1), "n": dval(2)}),
	)
	got := results(t)(d.RunAggregationQuery(context.Background(), aggReq(&dspb.Query{}, sum("i", "i"), sum("n", "n"), avg("an", "n"))))
	if !strings.HasPrefix(got["i"], "double 9.22") || got["n"] != "double NaN" || got["an"] != "double NaN" {
		t.Errorf("overflow and NaN = %v", got)
	}
	got = results(t)(d.RunAggregationQuery(context.Background(), aggReq(&dspb.Query{Limit: wrapperspb.Int32(1)}, sum("i", "i"), count("c", nil))))
	if got["i"] != fmt.Sprintf("int %d", int64(math.MaxInt64)) || got["c"] != "int 1" {
		t.Errorf("with limit 1 = %v", got)
	}
}

func TestAggregationRefusals(t *testing.T) {
	d, _ := newDatastore(t)
	ctx := context.Background()
	neg := int64(-1)
	for name, c := range map[string]struct {
		req  *dspb.RunAggregationQueryRequest
		code codes.Code
	}{
		"key":       {aggReq(&dspb.Query{}, sum("", "__key__")), codes.InvalidArgument},
		"none":      {aggReq(&dspb.Query{}), codes.InvalidArgument},
		"six":       {aggReq(&dspb.Query{}, count("a", nil), count("b", nil), count("c", nil), count("d", nil), count("e", nil), count("f", nil)), codes.InvalidArgument},
		"duplicate": {aggReq(&dspb.Query{}, count("a", nil), count("a", nil)), codes.InvalidArgument},
		"negative":  {aggReq(&dspb.Query{}, count("a", &neg)), codes.InvalidArgument},
		"gql":       {&dspb.RunAggregationQueryRequest{QueryType: &dspb.RunAggregationQueryRequest_GqlQuery{GqlQuery: &dspb.GqlQuery{QueryString: "SELECT COUNT(*) FROM k"}}}, codes.Unimplemented},
	} {
		if _, err := d.RunAggregationQuery(ctx, c.req); status.Code(err) != c.code {
			t.Errorf("%s: err = %v, want %v", name, err, c.code)
		}
	}
}

func TestAggregationTransactionAndREST(t *testing.T) {
	d, _ := newDatastore(t, ent(map[string]*dspb.Value{"i": ival(4)}))
	req := aggReq(&dspb.Query{}, sum("s", "i"))
	req.ReadOptions = &dspb.ReadOptions{ConsistencyType: &dspb.ReadOptions_NewTransaction{NewTransaction: &dspb.TransactionOptions{}}}
	resp, err := d.RunAggregationQuery(context.Background(), req)
	if err != nil || string(resp.GetTransaction()) != "tx" {
		t.Fatalf("new transaction: %v, %v; want the emulator's transaction", resp, err)
	}

	body, _ := protojson.Marshal(aggReq(&dspb.Query{}, count("n", nil)))
	r := httptest.NewRequest(http.MethodPost, "/v1/projects/p:runAggregationQuery", strings.NewReader(string(body)))
	if !d.Claims(r) {
		t.Fatal("REST runAggregationQuery not claimed")
	}
	w := httptest.NewRecorder()
	d.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"n":{"integerValue":"1"}`) {
		t.Errorf("REST = %d %s", w.Code, w.Body)
	}
	if d.Claims(httptest.NewRequest(http.MethodPost, "/v1/projects/p:runQuery", nil)) {
		t.Error("runQuery claimed")
	}
	w = httptest.NewRecorder()
	gql := httptest.NewRequest(http.MethodPost, "/v1/projects/p:runAggregationQuery", strings.NewReader(`{"gqlQuery":{"queryString":"x"}}`))
	d.ServeHTTP(w, gql)
	if w.Code != 501 {
		t.Errorf("REST GQL aggregation = %d, want 501", w.Code)
	}
}

// fakeFirestore serves RunQuery over document names.
type fakeFirestore struct {
	fspb.UnimplementedFirestoreServer
	names []string
	got   *fspb.RunQueryRequest
}

func (f *fakeFirestore) RunQuery(req *fspb.RunQueryRequest, s fspb.Firestore_RunQueryServer) error {
	f.got = req
	for _, n := range f.names {
		if err := s.Send(&fspb.RunQueryResponse{Document: &fspb.Document{Name: n}}); err != nil {
			return err
		}
	}
	return s.Send(&fspb.RunQueryResponse{}) // a progress-only response
}

func TestSplitPoints(t *testing.T) {
	for _, c := range []struct {
		n     int
		count int64
		want  string
	}{
		{10, 3, "[2 5 7]"}, {10, 1, "[5]"}, {3, 10, "[1 2]"}, {1, 5, "[]"}, {0, 5, "[]"}, {4, 3, "[1 2 3]"},
	} {
		if got := fmt.Sprint(splitPoints(c.n, c.count)); got != c.want {
			t.Errorf("splitPoints(%d, %d) = %s, want %s", c.n, c.count, got, c.want)
		}
	}
}

func partReq(count int64, size int32, tok string) *fspb.PartitionQueryRequest {
	return &fspb.PartitionQueryRequest{
		Parent: "projects/p/databases/(default)/documents", PartitionCount: count, PageSize: size, PageToken: tok,
		QueryType: &fspb.PartitionQueryRequest_StructuredQuery{StructuredQuery: &fspb.StructuredQuery{
			From:    []*fspb.StructuredQuery_CollectionSelector{{CollectionId: "c", AllDescendants: true}},
			OrderBy: []*fspb.StructuredQuery_Order{{Field: &fspb.StructuredQuery_FieldReference{FieldPath: "__name__"}, Direction: fspb.StructuredQuery_ASCENDING}},
		}},
	}
}

func TestPartitionQueryPages(t *testing.T) {
	f := &fakeFirestore{}
	for i := 0; i < 10; i++ {
		f.names = append(f.names, fmt.Sprintf("projects/p/databases/(default)/documents/c/d%d", i))
	}
	fs := NewFirestore(serve(t, func(s *grpc.Server) { fspb.RegisterFirestoreServer(s, f) }))
	ctx := context.Background()
	var all []string
	tok := ""
	for page := 0; ; page++ {
		resp, err := fs.PartitionQuery(ctx, partReq(3, 2, tok))
		if err != nil {
			t.Fatalf("PartitionQuery: %v", err)
		}
		for _, c := range resp.GetPartitions() {
			all = append(all, c.GetValues()[0].GetReferenceValue()[len("projects/p/databases/(default)/documents/"):])
		}
		if tok = resp.GetNextPageToken(); tok == "" {
			break
		}
		if page > 3 {
			t.Fatal("paging does not end")
		}
	}
	if fmt.Sprint(all) != "[c/d2 c/d5 c/d7]" {
		t.Errorf("cursors = %v, want [c/d2 c/d5 c/d7]", all)
	}
	if sel := f.got.GetStructuredQuery().GetSelect().GetFields(); len(sel) != 1 || sel[0].GetFieldPath() != "__name__" {
		t.Errorf("emulator query selects %v, want __name__ alone", sel)
	}
}

func TestPartitionQueryRefusals(t *testing.T) {
	fs := NewFirestore(serve(t, func(s *grpc.Server) { fspb.RegisterFirestoreServer(s, &fakeFirestore{}) }))
	ctx := context.Background()
	notGroup := partReq(2, 0, "")
	notGroup.GetStructuredQuery().From[0].AllDescendants = false
	filtered := partReq(2, 0, "")
	filtered.GetStructuredQuery().Limit = wrapperspb.Int32(3)
	desc := partReq(2, 0, "")
	desc.GetStructuredQuery().OrderBy[0].Direction = fspb.StructuredQuery_DESCENDING
	for name, req := range map[string]*fspb.PartitionQueryRequest{
		"zero": partReq(0, 0, ""), "token": partReq(2, 0, "bogus!"), "not a group": notGroup, "limit": filtered, "desc": desc,
	} {
		if _, err := fs.PartitionQuery(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: err = %v, want InvalidArgument", name, err)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/projects/p/databases/(default)/documents:partitionQuery", strings.NewReader(`{"structuredQuery":{"from":[{"collectionId":"c","allDescendants":true}]},"partitionCount":"2"}`))
	if !fs.Claims(r) {
		t.Fatal("REST partitionQuery not claimed")
	}
	w := httptest.NewRecorder()
	fs.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("REST = %d %s", w.Code, w.Body)
	}
	if fs.Claims(httptest.NewRequest(http.MethodPost, "/v1/projects/p/databases/(default)/documents:runQuery", nil)) {
		t.Error("runQuery claimed")
	}
}
