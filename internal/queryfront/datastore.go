package queryfront

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const datastoreAggregation = "/google.datastore.v1.Datastore/RunAggregationQuery"

// maxAggregations is Google's limit: "a minimum of one and maximum of five
// aggregations per query".
const maxAggregations = 5

var datastoreAggregationREST = regexp.MustCompile(`^/v1/projects/([^/:]+):runAggregationQuery$`)

// Datastore answers RunAggregationQuery in front of the Datastore emulator.
type Datastore struct {
	pb.UnimplementedDatastoreServer
	up   upstream
	grpc *grpc.Server
	// client overrides the emulator's client (tests).
	client pb.DatastoreClient
}

// NewDatastore returns the front for the Datastore emulator at emulator
// (host:port).
func NewDatastore(emulator string) *Datastore {
	d := &Datastore{up: upstream{addr: emulator}}
	d.grpc = grpc.NewServer()
	pb.RegisterDatastoreServer(d.grpc, d)
	return d
}

// Claims reports whether r is a RunAggregationQuery call, over gRPC or REST.
func (d *Datastore) Claims(r *http.Request) bool {
	if isGRPC(r) {
		return r.URL.Path == datastoreAggregation
	}
	return r.Method == http.MethodPost && datastoreAggregationREST.MatchString(r.URL.Path)
}

// ServeHTTP answers a call Claims claimed.
func (d *Datastore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isGRPC(r) {
		d.grpc.ServeHTTP(w, r)
		return
	}
	req := &pb.RunAggregationQueryRequest{}
	if err := readJSON(r, req); err != nil {
		writeError(w, err)
		return
	}
	req.ProjectId = datastoreAggregationREST.FindStringSubmatch(r.URL.Path)[1]
	resp, err := d.runAggregation(forwardHTTP(r.Context(), r), req)
	writeReply(w, resp, err)
}

func (d *Datastore) emulator() (pb.DatastoreClient, error) {
	if d.client != nil {
		return d.client, nil
	}
	conn, err := d.up.get()
	if err != nil {
		return nil, err
	}
	return pb.NewDatastoreClient(conn), nil
}

// RunAggregationQuery runs the nested query through the emulator and
// aggregates its results.
func (d *Datastore) RunAggregationQuery(ctx context.Context, req *pb.RunAggregationQueryRequest) (*pb.RunAggregationQueryResponse, error) {
	return d.runAggregation(ctx, req)
}

// aggregate is one requested aggregation, with its resolved alias.
type aggregate struct {
	alias string
	count bool
	upTo  int64 // -1: unbounded
	avg   bool
	prop  string
	acc   numeric
	n     int64 // entities counted
}

// numeric accumulates SUM and AVG as Google documents them: integers sum
// as a 64-bit integer until one is a double or the sum overflows, then as
// a double; NaN and infinities follow IEEE-754.
type numeric struct {
	ints     int64
	overflow bool
	intsF    float64 // the integers, as a double, for an overflowed sum
	doubles  float64
	anyDbl   bool
	values   int64
}

func (a *numeric) add(v *pb.Value) {
	switch x := v.GetValueType().(type) {
	case *pb.Value_IntegerValue:
		a.values++
		a.intsF += float64(x.IntegerValue)
		if !a.overflow {
			s := a.ints + x.IntegerValue
			if (x.IntegerValue > 0 && s < a.ints) || (x.IntegerValue < 0 && s > a.ints) {
				a.overflow = true
			} else {
				a.ints = s
			}
		}
	case *pb.Value_DoubleValue:
		a.values++
		a.anyDbl = true
		a.doubles += x.DoubleValue
	}
}

func (a *numeric) double() float64 {
	ints := float64(a.ints)
	if a.overflow {
		ints = a.intsF
	}
	return ints + a.doubles // NaN and infinities carry through
}

func (a *aggregate) result() *pb.Value {
	switch {
	case a.count:
		return &pb.Value{ValueType: &pb.Value_IntegerValue{IntegerValue: a.n}}
	case a.avg:
		if a.acc.values == 0 {
			return &pb.Value{ValueType: &pb.Value_NullValue{NullValue: structpb.NullValue_NULL_VALUE}}
		}
		return &pb.Value{ValueType: &pb.Value_DoubleValue{DoubleValue: a.acc.double() / float64(a.acc.values)}}
	default:
		if !a.acc.anyDbl && !a.acc.overflow {
			return &pb.Value{ValueType: &pb.Value_IntegerValue{IntegerValue: a.acc.ints}}
		}
		return &pb.Value{ValueType: &pb.Value_DoubleValue{DoubleValue: a.acc.double()}}
	}
}

// aggregates validates the request's aggregations and resolves their
// aliases: an unnamed one is `property_<n>`, n counting the unnamed ones.
func aggregates(in []*pb.AggregationQuery_Aggregation) ([]*aggregate, error) {
	if len(in) == 0 || len(in) > maxAggregations {
		return nil, status.Errorf(codes.InvalidArgument, "an aggregation query must have between 1 and %d aggregations, it has %d", maxAggregations, len(in))
	}
	taken := map[string]bool{}
	for _, a := range in {
		if a.GetAlias() == "" {
			continue
		}
		if taken[a.GetAlias()] {
			return nil, status.Errorf(codes.InvalidArgument, "duplicate aggregation alias %q", a.GetAlias())
		}
		if strings.HasPrefix(a.GetAlias(), "__") && strings.HasSuffix(a.GetAlias(), "__") {
			return nil, status.Errorf(codes.InvalidArgument, "the aggregation alias %q is reserved", a.GetAlias())
		}
		taken[a.GetAlias()] = true
	}
	next := 0
	out := make([]*aggregate, 0, len(in))
	for _, a := range in {
		g := &aggregate{alias: a.GetAlias(), upTo: -1}
		if g.alias == "" {
			for {
				next++
				g.alias = fmt.Sprintf("property_%d", next)
				if !taken[g.alias] {
					break
				}
			}
			taken[g.alias] = true
		}
		var prop *pb.PropertyReference
		switch op := a.GetOperator().(type) {
		case *pb.AggregationQuery_Aggregation_Count_:
			g.count = true
			if u := op.Count.GetUpTo(); u != nil {
				if u.GetValue() < 0 {
					return nil, status.Errorf(codes.InvalidArgument, "COUNT up_to must be non-negative, got %d", u.GetValue())
				}
				g.upTo = u.GetValue()
			}
		case *pb.AggregationQuery_Aggregation_Sum_:
			prop = op.Sum.GetProperty()
		case *pb.AggregationQuery_Aggregation_Avg_:
			g.avg = true
			prop = op.Avg.GetProperty()
		default:
			return nil, status.Error(codes.InvalidArgument, "an aggregation must be COUNT, SUM or AVG")
		}
		if !g.count {
			g.prop = prop.GetName()
			if g.prop == "" {
				return nil, status.Error(codes.InvalidArgument, "SUM and AVG need a property")
			}
			if g.prop == "__key__" {
				return nil, status.Errorf(codes.InvalidArgument, "Aggregations are not supported for the property: %s", g.prop)
			}
		}
		out = append(out, g)
	}
	return out, nil
}

// property is the value of name in e: a property of that name, or, for a
// dotted name, a path through embedded entities.
func property(e *pb.Entity, name string) *pb.Value {
	if v, ok := e.GetProperties()[name]; ok {
		return v
	}
	head, rest, ok := strings.Cut(name, ".")
	if !ok {
		return nil
	}
	v, ok := e.GetProperties()[head]
	if !ok || v.GetEntityValue() == nil {
		return nil
	}
	return property(v.GetEntityValue(), rest)
}

func (d *Datastore) runAggregation(ctx context.Context, req *pb.RunAggregationQueryRequest) (*pb.RunAggregationQueryResponse, error) {
	if req.GetGqlQuery() != nil {
		return nil, status.Error(codes.Unimplemented, "a GQL aggregation query is not served here: the Datastore emulator does not serve RunAggregationQuery, and CloudBurrow answers a structured aggregation_query alone; send the aggregation as aggregation_query")
	}
	if req.GetExplainOptions() != nil {
		return nil, status.Error(codes.Unimplemented, "explain_options are not served here: the Datastore emulator does not plan queries, so there are no query plan or execution statistics to return")
	}
	aq := req.GetAggregationQuery()
	nested := aq.GetNestedQuery()
	if nested == nil {
		return nil, status.Error(codes.InvalidArgument, "an aggregation query needs a nested_query")
	}
	aggs, err := aggregates(aq.GetAggregations())
	if err != nil {
		return nil, err
	}
	client, err := d.emulator()
	if err != nil {
		return nil, err
	}
	ctx = forward(ctx)

	// COUNT alone needs the keys alone; it stops at its largest bound.
	countOnly, bound := true, int64(0)
	for _, g := range aggs {
		if !g.count {
			countOnly = false
		}
		if g.upTo < 0 {
			bound = -1
		} else if bound >= 0 && g.upTo > bound {
			bound = g.upTo
		}
	}
	if !countOnly {
		bound = -1
	}
	q := proto.Clone(nested).(*pb.Query)
	if countOnly && len(q.GetProjection()) == 0 && len(q.GetDistinctOn()) == 0 {
		q.Projection = []*pb.Projection{{Property: &pb.PropertyReference{Name: "__key__"}}}
	}

	ro := req.GetReadOptions()
	var txn []byte
	var readTime *timestamppb.Timestamp
	var seen int64
	for {
		if bound >= 0 && seen >= bound {
			break
		}
		resp, err := client.RunQuery(ctx, &pb.RunQueryRequest{
			ProjectId: req.GetProjectId(), DatabaseId: req.GetDatabaseId(), PartitionId: req.GetPartitionId(),
			ReadOptions: ro, QueryType: &pb.RunQueryRequest_Query{Query: q},
		})
		if err != nil {
			return nil, err
		}
		if t := resp.GetTransaction(); len(t) > 0 {
			txn = t
			ro = &pb.ReadOptions{ConsistencyType: &pb.ReadOptions_Transaction{Transaction: t}}
		}
		b := resp.GetBatch()
		if b.GetReadTime() != nil {
			readTime = b.GetReadTime()
		}
		for _, er := range b.GetEntityResults() {
			seen++
			for _, g := range aggs {
				if g.count {
					if g.upTo < 0 || g.n < g.upTo {
						g.n++
					}
					continue
				}
				v := property(er.GetEntity(), g.prop)
				// Aggregations are answered from the indexes, which an
				// unindexed value is not in.
				if v == nil || v.GetExcludeFromIndexes() {
					continue
				}
				g.acc.add(v)
			}
		}
		if b.GetMoreResults() != pb.QueryResultBatch_NOT_FINISHED {
			break
		}
		if len(b.GetEntityResults()) == 0 && b.GetSkippedResults() == 0 {
			return nil, status.Error(codes.Internal, "the emulator's query made no progress")
		}
		q = proto.Clone(q).(*pb.Query)
		q.StartCursor = b.GetEndCursor()
		q.Offset = max(0, q.GetOffset()-b.GetSkippedResults())
		if q.Limit != nil {
			left := q.GetLimit().GetValue() - int32(len(b.GetEntityResults()))
			if left <= 0 {
				break
			}
			q.Limit.Value = left
		}
	}
	if readTime == nil {
		readTime = timestamppb.Now()
	}
	props := make(map[string]*pb.Value, len(aggs))
	for _, g := range aggs {
		props[g.alias] = g.result()
	}
	out := &pb.RunAggregationQueryResponse{
		Batch: &pb.AggregationResultBatch{
			AggregationResults: []*pb.AggregationResult{{AggregateProperties: props}},
			MoreResults:        pb.QueryResultBatch_NO_MORE_RESULTS,
			ReadTime:           readTime,
		},
	}
	if req.GetReadOptions().GetNewTransaction() != nil {
		out.Transaction = txn
	}
	return out, nil
}
