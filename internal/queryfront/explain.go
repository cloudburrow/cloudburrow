package queryfront

// Datastore query explain (#1500): explain_options on RunQuery and
// RunAggregationQuery, which the emulator does not serve. The front
// answers them with the ExplainMetrics Google documents:
//
//   - plan_summary.indexes_used: the index Google's planner would use for
//     the query, in Google's shape ({query_scope, properties}): the
//     equality filters' properties, then the other filters' and the
//     orders', then __name__. The emulator itself scans without indexes;
//     this is the index the query needs, not one the emulator read.
//   - with analyze, execution_stats: the results returned, the time the
//     emulator took, the read operations Google bills (one per entity, at
//     least one per query), and debug_stats with Google's keys, counted by
//     the front from the results (the emulator reports no scan counts).
//
// Without analyze the query is planned, not run, and returns no results,
// as on Google. Calls without explain_options pass to the emulator
// unchanged: the front reads a RunQuery's body to see whether it asks for
// an explain, and claims it only then.

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
)

const datastoreRunQuery = "/google.datastore.v1.Datastore/RunQuery"

var datastoreRunQueryREST = regexp.MustCompile(`^/v1/projects/([^/:]+):runQuery$`)

// asksExplain reads r's body, puts it back, and reports whether it is a
// RunQuery request with explain_options.
func asksExplain(r *http.Request) bool {
	orig := r.Body
	b, err := io.ReadAll(io.LimitReader(orig, 64<<20))
	// What was read goes back in front of what was not, so a body past the
	// limit passes whole.
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), orig), orig}
	if err != nil {
		return false
	}
	req := &pb.RunQueryRequest{}
	if isGRPC(r) {
		// One uncompressed gRPC message: a flag byte, a length, the message.
		if len(b) < 5 || b[0] != 0 {
			return false
		}
		n := binary.BigEndian.Uint32(b[1:5])
		if uint64(len(b)-5) < uint64(n) || proto.Unmarshal(b[5:5+n], req) != nil {
			return false
		}
	} else if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, req); err != nil {
		return false
	}
	return req.GetExplainOptions() != nil
}

func (d *Datastore) serveRunQueryREST(w http.ResponseWriter, r *http.Request) {
	req := &pb.RunQueryRequest{}
	if err := readJSON(r, req); err != nil {
		writeError(w, err)
		return
	}
	req.ProjectId = datastoreRunQueryREST.FindStringSubmatch(r.URL.Path)[1]
	resp, err := d.RunQuery(forwardHTTP(r.Context(), r), req)
	writeReply(w, resp, err)
}

// RunQuery answers a query with explain_options; the front claims no other.
func (d *Datastore) RunQuery(ctx context.Context, req *pb.RunQueryRequest) (*pb.RunQueryResponse, error) {
	client, err := d.emulator()
	if err != nil {
		return nil, err
	}
	eo := req.GetExplainOptions()
	inner := proto.Clone(req).(*pb.RunQueryRequest)
	inner.ExplainOptions = nil
	if !eo.GetAnalyze() {
		q := req.GetQuery()
		var parsed *pb.Query
		if req.GetGqlQuery() != nil {
			// The emulator parses the GQL; its results are not returned.
			resp, err := client.RunQuery(forward(ctx), inner)
			if err != nil {
				return nil, err
			}
			q, parsed = resp.GetQuery(), resp.GetQuery()
		}
		return &pb.RunQueryResponse{Query: parsed,
			Batch:          &pb.QueryResultBatch{EntityResultType: resultType(q), MoreResults: pb.QueryResultBatch_NO_MORE_RESULTS},
			ExplainMetrics: &pb.ExplainMetrics{PlanSummary: planSummary(q)}}, nil
	}
	start := time.Now()
	resp, err := client.RunQuery(forward(ctx), inner)
	if err != nil {
		return nil, err
	}
	q := req.GetQuery()
	if req.GetGqlQuery() != nil {
		q = resp.GetQuery()
	}
	resp.ExplainMetrics = &pb.ExplainMetrics{PlanSummary: planSummary(q),
		ExecutionStats: executionStats(int64(len(resp.GetBatch().GetEntityResults())), time.Since(start))}
	return resp, nil
}

// explainAggregation answers an aggregation with explain_options.
func (d *Datastore) explainAggregation(ctx context.Context, req *pb.RunAggregationQueryRequest) (*pb.RunAggregationQueryResponse, error) {
	eo := req.GetExplainOptions()
	inner := proto.Clone(req).(*pb.RunAggregationQueryRequest)
	inner.ExplainOptions = nil
	var parsed *pb.AggregationQuery
	if inner.GetGqlQuery() != nil {
		s, err := d.fromGQL(ctx, inner)
		if err != nil {
			return nil, err
		}
		inner, parsed = s, s.GetAggregationQuery()
	}
	plan := planSummary(inner.GetAggregationQuery().GetNestedQuery())
	if !eo.GetAnalyze() {
		return &pb.RunAggregationQueryResponse{Query: parsed,
			Batch:          &pb.AggregationResultBatch{MoreResults: pb.QueryResultBatch_NO_MORE_RESULTS},
			ExplainMetrics: &pb.ExplainMetrics{PlanSummary: plan}}, nil
	}
	start := time.Now()
	resp, err := d.runAggregation(ctx, inner)
	if err != nil {
		return nil, err
	}
	resp.Query = parsed
	resp.ExplainMetrics = &pb.ExplainMetrics{PlanSummary: plan, ExecutionStats: executionStats(1, time.Since(start))}
	return resp, nil
}

func resultType(q *pb.Query) pb.EntityResult_ResultType {
	switch p := q.GetProjection(); {
	case len(p) == 0:
		return pb.EntityResult_FULL
	case len(p) == 1 && p[0].GetProperty().GetName() == "__key__":
		return pb.EntityResult_KEY_ONLY
	default:
		return pb.EntityResult_PROJECTION
	}
}

// planSummary is the index Google would plan q on, in Google's shape:
// {"query_scope": "Collection", "properties": "(a ASC, __name__ ASC)"}.
func planSummary(q *pb.Query) *pb.PlanSummary {
	scope := "Collection"
	if len(q.GetKind()) == 0 {
		scope = "Collection group"
	}
	var props []string
	seen := map[string]bool{}
	add := func(name, dir string) {
		if name == "__key__" {
			name = "__name__"
		}
		if !seen[name] {
			seen[name] = true
			props = append(props, name+" "+dir)
		}
	}
	var eq, other []string
	var walk func(f *pb.Filter)
	walk = func(f *pb.Filter) {
		if cf := f.GetCompositeFilter(); cf != nil {
			for _, sub := range cf.GetFilters() {
				walk(sub)
			}
			return
		}
		if pf := f.GetPropertyFilter(); pf != nil {
			if pf.GetOp() == pb.PropertyFilter_EQUAL || pf.GetOp() == pb.PropertyFilter_HAS_ANCESTOR {
				if pf.GetOp() == pb.PropertyFilter_EQUAL {
					eq = append(eq, pf.GetProperty().GetName())
				}
			} else {
				other = append(other, pf.GetProperty().GetName())
			}
		}
	}
	if q.GetFilter() != nil {
		walk(q.GetFilter())
	}
	for _, n := range eq {
		add(n, "ASC")
	}
	last := "ASC"
	for _, o := range q.GetOrder() {
		dir := "ASC"
		if o.GetDirection() == pb.PropertyOrder_DESCENDING {
			dir = "DESC"
		}
		add(o.GetProperty().GetName(), dir)
		last = dir
	}
	for _, n := range other {
		add(n, "ASC")
	}
	add("__name__", last)
	s, _ := structpb.NewStruct(map[string]any{"query_scope": scope, "properties": "(" + strings.Join(props, ", ") + ")"})
	return &pb.PlanSummary{IndexesUsed: []*structpb.Struct{s}}
}

// executionStats are the stats of a query that returned n results in d.
func executionStats(n int64, d time.Duration) *pb.ExecutionStats {
	minCost := "0"
	if n == 0 {
		minCost = "1"
	}
	s := strconv.FormatInt(n, 10)
	debug, _ := structpb.NewStruct(map[string]any{
		"documents_scanned":     s,
		"index_entries_scanned": s,
		"billing_details": map[string]any{
			"documents_billable":     s,
			"index_entries_billable": "0",
			"small_ops":              "0",
			"min_query_cost":         minCost,
		},
	})
	return &pb.ExecutionStats{ResultsReturned: n, ExecutionDuration: durationpb.New(d), ReadOperations: max(n, 1), DebugStats: debug}
}
