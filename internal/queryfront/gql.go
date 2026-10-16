package queryfront

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// A GQL aggregation query (#1461) is answered as a structured one: the
// front splits the aggregations from the query they are over, has the
// emulator parse that query (its RunQuery answers a GQL query with the
// parsed form), and aggregates over it as it does a structured query. Both
// of Google's forms are served:
//
//	AGGREGATE COUNT(*) AS n, SUM(x) OVER (SELECT * FROM Kind WHERE ...)
//	SELECT COUNT(*) AS n FROM Kind WHERE ...
var (
	gqlAggregateRe = regexp.MustCompile(`(?is)^\s*AGGREGATE\s+(.+?)\s+OVER\s*\((.*)\)\s*$`)
	gqlSelectRe    = regexp.MustCompile(`(?is)^\s*SELECT\s+(.+?)\s+(FROM\s.*)$`)
	gqlAggRe       = regexp.MustCompile("(?is)^(COUNT|COUNT_UP_TO|SUM|AVG)\\s*\\(\\s*(.*?)\\s*\\)(?:\\s+AS\\s+(\\w+|`[^`]+`))?$")
)

// splitGQLAggregation is the aggregations of a GQL aggregation query and the
// GQL of the query they are over.
func splitGQLAggregation(q string) ([]*pb.AggregationQuery_Aggregation, string, error) {
	var list, inner string
	if m := gqlAggregateRe.FindStringSubmatch(q); m != nil {
		list, inner = m[1], m[2]
	} else if m := gqlSelectRe.FindStringSubmatch(q); m != nil && gqlAggRe.MatchString(strings.TrimSpace(strings.SplitN(m[1], ",", 2)[0])) {
		list, inner = m[1], "SELECT * "+m[2]
	} else {
		return nil, "", status.Errorf(codes.InvalidArgument, "%q is not a GQL aggregation query: want AGGREGATE <aggregations> OVER (<query>), or SELECT <aggregations> FROM ...", q)
	}
	var out []*pb.AggregationQuery_Aggregation
	for i, part := range strings.Split(list, ",") {
		m := gqlAggRe.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			return nil, "", status.Errorf(codes.InvalidArgument, "%q is not an aggregation: want COUNT(*), COUNT_UP_TO(n), SUM(property) or AVG(property)", strings.TrimSpace(part))
		}
		alias := strings.Trim(m[3], "`")
		if alias == "" {
			alias = fmt.Sprintf("property_%d", i+1)
		}
		arg := strings.Trim(m[2], "`")
		a := &pb.AggregationQuery_Aggregation{Alias: alias}
		switch strings.ToUpper(m[1]) {
		case "COUNT":
			if arg != "*" {
				return nil, "", status.Errorf(codes.InvalidArgument, "COUNT takes *, not %q", arg)
			}
			a.Operator = &pb.AggregationQuery_Aggregation_Count_{Count: &pb.AggregationQuery_Aggregation_Count{}}
		case "COUNT_UP_TO":
			n, err := strconv.ParseInt(arg, 10, 64)
			if err != nil || n <= 0 {
				return nil, "", status.Errorf(codes.InvalidArgument, "COUNT_UP_TO takes a positive integer, not %q", arg)
			}
			a.Operator = &pb.AggregationQuery_Aggregation_Count_{Count: &pb.AggregationQuery_Aggregation_Count{UpTo: wrapperspb.Int64(n)}}
		case "SUM":
			a.Operator = &pb.AggregationQuery_Aggregation_Sum_{Sum: &pb.AggregationQuery_Aggregation_Sum{Property: &pb.PropertyReference{Name: arg}}}
		case "AVG":
			a.Operator = &pb.AggregationQuery_Aggregation_Avg_{Avg: &pb.AggregationQuery_Aggregation_Avg{Property: &pb.PropertyReference{Name: arg}}}
		}
		out = append(out, a)
	}
	return out, inner, nil
}

// fromGQL is req with its GQL aggregation query made structured, the
// nested query parsed by the emulator.
func (d *Datastore) fromGQL(ctx context.Context, req *pb.RunAggregationQueryRequest) (*pb.RunAggregationQueryRequest, error) {
	g := req.GetGqlQuery()
	aggs, inner, err := splitGQLAggregation(g.GetQueryString())
	if err != nil {
		return nil, err
	}
	client, err := d.emulator()
	if err != nil {
		return nil, err
	}
	gq := proto.Clone(g).(*pb.GqlQuery)
	gq.QueryString = inner
	// Parsed outside any transaction: the aggregation runs in the
	// request's own.
	resp, err := client.RunQuery(forward(ctx), &pb.RunQueryRequest{ProjectId: req.GetProjectId(), DatabaseId: req.GetDatabaseId(),
		PartitionId: req.GetPartitionId(), QueryType: &pb.RunQueryRequest_GqlQuery{GqlQuery: gq}})
	if err != nil {
		return nil, err
	}
	if resp.GetQuery() == nil {
		return nil, status.Error(codes.Internal, "the emulator did not return the parsed form of the GQL query")
	}
	out := proto.Clone(req).(*pb.RunAggregationQueryRequest)
	out.QueryType = &pb.RunAggregationQueryRequest_AggregationQuery{AggregationQuery: &pb.AggregationQuery{
		QueryType:    &pb.AggregationQuery_NestedQuery{NestedQuery: resp.GetQuery()},
		Aggregations: aggs,
	}}
	return out, nil
}
