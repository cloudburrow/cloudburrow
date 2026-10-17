package bigtablefront

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SQL (#1499, #1511): PrepareQuery and ExecuteQuery, GoogleSQL for
// Bigtable over the emulator's rows. The statement (sqlparse.go) is
// SELECT [DISTINCT] items [FROM table-or-view [(temporal arguments)]]
// [WHERE] [GROUP BY] [ORDER BY _key] [LIMIT], with the operators,
// conditional expressions, CAST, the scalar, map, array, JSON and
// timestamp functions and the aggregates sqlfuncs.go runs. A family is a
// MAP<BYTES, BYTES> of its columns' latest values, or, with
// with_history => TRUE, a MAP<BYTES, ARRAY<STRUCT<timestamp TIMESTAMP,
// value BYTES>>>, newest first; as_of, after, after_or_equal, before and
// latest_n filter the cells, as the reference documents.
//
// FROM names a table, a logical view (whose query runs as a query) or a
// materialized view (whose query runs over the table as it is now, so its
// rows are always up to date). A documented part of the language the
// emulator's SQL does not run (UNPACK, window functions, the functions
// sqlfuncs.go lists as documented and does not build) is refused
// UNIMPLEMENTED naming it, and anything else it cannot run INVALID_ARGUMENT,
// so a query never returns rows Google would not.
//
// PrepareQuery checks the query against the table's schema and returns its
// columns' types and the query itself as the prepared query, valid for an
// hour; ExecuteQuery reads the table through the emulator and answers in one
// ProtoRows batch with its CRC32C checksum and a resume token, as the
// service does.

const preparedValidity = time.Hour

// viewDepth bounds views reading views.
const viewDepth = 8

var crc32c = crc32.MakeTable(crc32.Castagnoli)

type sqlFront struct {
	upstream *grpc.ClientConn
	views    *sqlViews
	now      func() time.Time
}

func (q *sqlFront) methods() map[string]method {
	return map[string]method{
		data + "PrepareQuery": unary(func() *btpb.PrepareQueryRequest { return &btpb.PrepareQueryRequest{} }, q.prepare),
		data + "ExecuteQuery": unary(func() *btpb.ExecuteQueryRequest { return &btpb.ExecuteQueryRequest{} }, q.execute),
	}
}

// prepared is the prepared query the client sends back.
type prepared struct {
	Instance string
	Query    string
	Params   map[string][]byte // name: the marshalled google.bigtable.v2.Type
	Until    int64
}

// sqlError is err as the status the front answers.
func sqlError(err error) error {
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	var u unsupportedErr
	if errors.As(err, &u) {
		return status.Error(codes.Unimplemented, err.Error())
	}
	return status.Error(codes.InvalidArgument, err.Error())
}

// compile parses sql and checks it against what FROM names; it returns
// the plan, whose source is read when read is set.
func (q *sqlFront) compile(ctx context.Context, instance, sql string, params map[string]*btpb.Type, bound map[string]any, read bool, depth int) (*plan, error) {
	qu, err := parseQuery(sql)
	if err != nil {
		return nil, err
	}
	base := env{params: bound, now: q.now()}
	src, err := q.source(ctx, instance, qu, params, base, read, depth)
	if err != nil {
		return nil, err
	}
	return build(qu, src, params)
}

// view is the logical or materialized view name names in instance, if any.
func (q *sqlFront) view(instance, name string) (*sqlView, string) {
	if q.views == nil {
		return nil, ""
	}
	for _, k := range []sqlKind{logicalKind, materializedKind} {
		if v, err := q.views.get(k, instance+"/"+k.segment+"/"+name); err == nil {
			return v, k.noun
		}
	}
	return nil, ""
}

func (q *sqlFront) source(ctx context.Context, instance string, qu *query, params map[string]*btpb.Type, base env, read bool, depth int) (*source, error) {
	if qu.from == "" {
		return &source{rows: [][]any{{}}}, nil
	}
	if v, noun := q.view(instance, qu.from); v != nil {
		if len(qu.args) > 0 {
			return nil, fmt.Errorf("%s %s takes no table arguments", noun, qu.from)
		}
		if depth >= viewDepth {
			return nil, fmt.Errorf("%s %s: views nest more than %d deep", noun, qu.from, viewDepth)
		}
		// A view's query takes no parameters.
		p, err := q.compile(ctx, instance, v.query, nil, nil, read, depth+1)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", noun, qu.from, err)
		}
		src := &source{}
		for _, c := range p.cols {
			src.cols = append(src.cols, column{c.GetName(), c.GetType()})
		}
		if read {
			if src.rows, err = p.run(env{now: base.now}); err != nil {
				return nil, fmt.Errorf("%s %s: %w", noun, qu.from, err)
			}
		}
		return src, nil
	}
	t, err := btapb.NewBigtableTableAdminClient(q.upstream).GetTable(ctx, &btapb.GetTableRequest{Name: instance + "/tables/" + qu.from, View: btapb.Table_SCHEMA_VIEW})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "Table not found: %s", qu.from)
		}
		return nil, err
	}
	tp, err := temporalArgs(qu.args, params, base)
	if err != nil {
		return nil, err
	}
	fams := map[string]*familyInfo{}
	for id, f := range t.GetColumnFamilies() {
		fams[id] = &familyInfo{typed: f.GetValueType() != nil}
	}
	cols, refused := tableColumns(fams, tp)
	src := &source{cols: cols, refused: refused}
	if read {
		rows, err := readAll(ctx, btpb.NewBigtableClient(q.upstream), &btpb.ReadRowsRequest{TableName: instance + "/tables/" + qu.from})
		if err != nil {
			return nil, err
		}
		src.rows = tableRows(rows, cols, tp)
	}
	return src, nil
}

func (q *sqlFront) prepare(ctx context.Context, req *btpb.PrepareQueryRequest) (proto.Message, error) {
	if !instanceRe.MatchString(req.GetInstanceName()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid instance_name %q", req.GetInstanceName())
	}
	pq := prepared{Instance: req.GetInstanceName(), Query: req.GetQuery(), Params: map[string][]byte{}, Until: q.now().Add(preparedValidity).Unix()}
	for name, t := range req.GetParamTypes() {
		if !paramTypeOK(t) {
			return nil, status.Errorf(codes.InvalidArgument, "@%s: parameters of type %s are not supported", name, kind(t))
		}
		b, err := proto.Marshal(t)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "%v", err)
		}
		pq.Params[name] = b
	}
	p, err := q.compile(ctx, pq.Instance, pq.Query, req.GetParamTypes(), nil, false, 0)
	if err != nil {
		return nil, sqlError(err)
	}
	blob, err := json.Marshal(pq)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &btpb.PrepareQueryResponse{
		Metadata:      &btpb.ResultSetMetadata{Schema: &btpb.ResultSetMetadata_ProtoSchema{ProtoSchema: &btpb.ProtoSchema{Columns: p.cols}}},
		PreparedQuery: blob,
		ValidUntil:    timestamppb.New(time.Unix(pq.Until, 0)),
	}, nil
}

func (q *sqlFront) execute(ctx context.Context, req *btpb.ExecuteQueryRequest) (proto.Message, error) {
	if req.GetQuery() != "" || len(req.GetPreparedQuery()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "prepared_query is required: prepare the query with PrepareQuery")
	}
	var pq prepared
	if err := json.Unmarshal(req.GetPreparedQuery(), &pq); err != nil {
		return nil, status.Error(codes.InvalidArgument, "prepared_query is not one PrepareQuery returned")
	}
	if pq.Instance != req.GetInstanceName() {
		return nil, status.Error(codes.InvalidArgument, "prepared_query was prepared for another instance")
	}
	if q.now().Unix() > pq.Until {
		return nil, status.Error(codes.FailedPrecondition, "the prepared query has expired: prepare it again")
	}
	if len(req.GetResumeToken()) > 0 {
		return nil, status.Error(codes.InvalidArgument, "resume_token: the emulator answers a query in one response, which is never resumed")
	}
	types := map[string]*btpb.Type{}
	bound := map[string]any{}
	for name, b := range pq.Params {
		t := &btpb.Type{}
		if err := proto.Unmarshal(b, t); err != nil {
			return nil, status.Error(codes.InvalidArgument, "prepared_query is not one PrepareQuery returned")
		}
		types[name] = t
		pv, ok := req.GetParams()[name]
		if !ok {
			continue // reported if the query reads it
		}
		v, err := decode(pv, t)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "@%s: %v", name, err)
		}
		bound[name] = v
	}
	for name := range req.GetParams() {
		if _, ok := types[name]; !ok {
			return nil, status.Errorf(codes.InvalidArgument, "Query parameter '%s' was not declared when the query was prepared", name)
		}
	}
	p, err := q.compile(ctx, pq.Instance, pq.Query, types, bound, true, 0)
	if err != nil {
		return nil, sqlError(err)
	}
	rows, err := p.run(env{params: bound, now: q.now()})
	if err != nil {
		return nil, sqlError(err)
	}
	var values []*btpb.Value
	for _, r := range rows {
		for i, v := range r {
			values = append(values, encode(v, p.cols[i].GetType()))
		}
	}
	return q.results(values)
}

// results is the one response holding values, a complete batch.
func (q *sqlFront) results(values []*btpb.Value) (proto.Message, error) {
	batch, err := proto.Marshal(&btpb.ProtoRows{Values: values})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	sum := crc32.Checksum(batch, crc32c)
	return &btpb.ExecuteQueryResponse{Response: &btpb.ExecuteQueryResponse_Results{Results: &btpb.PartialResultSet{
		PartialRows:   &btpb.PartialResultSet_ProtoRowsBatch{ProtoRowsBatch: &btpb.ProtoRowsBatch{BatchData: batch}},
		BatchChecksum: &sum,
		ResumeToken:   []byte("done"),
		Reset_:        true,
	}}}, nil
}

// checkViewQuery checks a view's query as CreateLogicalView and
// CreateMaterializedView do: it must compile, take no parameters and not
// use SELECT *.
func (q *sqlFront) checkViewQuery(ctx context.Context, instance, sql string) error {
	qu, err := parseQuery(sql)
	if err != nil {
		return sqlError(err)
	}
	for _, it := range qu.items {
		if it.star {
			return status.Error(codes.InvalidArgument, "a view's query cannot use SELECT *")
		}
	}
	if _, err := q.compile(ctx, instance, sql, nil, nil, false, 1); err != nil {
		if strings.Contains(err.Error(), "Query parameter") {
			return status.Error(codes.InvalidArgument, "a view's query takes no parameters")
		}
		return sqlError(err)
	}
	return nil
}
