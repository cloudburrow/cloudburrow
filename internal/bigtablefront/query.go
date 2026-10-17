package bigtablefront

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SQL (#1499): PrepareQuery and ExecuteQuery, GoogleSQL for Bigtable over
// the emulator's rows, for the subset of the language a key-value query
// needs:
//
//	SELECT * | item [, item ...] FROM table
//	  [WHERE condition [AND condition ...]] [LIMIT n]
//
// where an item is _key, a column family (a MAP<BYTES, BYTES> of its
// columns' latest values), family['qualifier'] (the latest value, BYTES),
// CAST(item AS STRING) or TO_INT64(item) (big-endian, as Increment writes
// it), each with an optional AS alias; and a condition is _key compared
// (=, !=, <, <=, >, >=) with a quoted string, a b'...' bytes literal or an
// @parameter, or STARTS_WITH(_key, value). Anything else (aggregates,
// joins, other functions, timestamps, views in FROM) is refused
// INVALID_ARGUMENT naming what is not supported, so a query never returns
// rows Google would not.
//
// PrepareQuery checks the query against the table's schema and returns its
// columns' types and the query itself as the prepared query, valid for an
// hour; ExecuteQuery reads the table through the emulator and answers in one
// or more ProtoRows batches, each with its CRC32C checksum and a resume
// token, as the service does.

const preparedValidity = time.Hour

var crc32c = crc32.MakeTable(crc32.Castagnoli)

type sqlFront struct {
	upstream *grpc.ClientConn
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
	Params   map[string]string // name: type ("BYTES", "STRING")
	Until    int64
}

// --- the parse ---

type token struct {
	kind string // "ident", "string", "bytes", "param", "number", "op"
	text string
}

func lex(src string) ([]token, error) {
	var out []token
	r := []rune(src)
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '`':
			j := i + 1
			for j < len(r) && r[j] != '`' {
				j++
			}
			if j == len(r) {
				return nil, fmt.Errorf("unterminated quoted identifier")
			}
			out = append(out, token{"ident", string(r[i+1 : j])})
			i = j + 1
		case (c == 'b' || c == 'B') && i+1 < len(r) && (r[i+1] == '\'' || r[i+1] == '"'):
			s, n, err := quoted(r[i+1:])
			if err != nil {
				return nil, err
			}
			out = append(out, token{"bytes", s})
			i += 1 + n
		case c == '\'' || c == '"':
			s, n, err := quoted(r[i:])
			if err != nil {
				return nil, err
			}
			out = append(out, token{"string", s})
			i += n
		case c == '@':
			j := i + 1
			for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_') {
				j++
			}
			out = append(out, token{"param", string(r[i+1 : j])})
			i = j
		case unicode.IsDigit(c):
			j := i
			for j < len(r) && unicode.IsDigit(r[j]) {
				j++
			}
			out = append(out, token{"number", string(r[i:j])})
			i = j
		case unicode.IsLetter(c) || c == '_':
			j := i
			for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_' || r[j] == '-') {
				j++
			}
			out = append(out, token{"ident", string(r[i:j])})
			i = j
		default:
			for _, op := range []string{"<=", ">=", "!=", "<>", "=", "<", ">", ",", "(", ")", "[", "]", "*", ";"} {
				if strings.HasPrefix(string(r[i:]), op) {
					out = append(out, token{"op", op})
					i += len([]rune(op))
					goto next
				}
			}
			return nil, fmt.Errorf("unexpected %q", string(c))
		next:
		}
	}
	return out, nil
}

// quoted reads a quoted literal at r[0], returning its value and length.
func quoted(r []rune) (string, int, error) {
	q := r[0]
	var b strings.Builder
	for i := 1; i < len(r); i++ {
		switch r[i] {
		case '\\':
			if i+1 < len(r) {
				i++
				switch r[i] {
				case 'n':
					b.WriteRune('\n')
				case 't':
					b.WriteRune('\t')
				default:
					b.WriteRune(r[i])
				}
			}
		case q:
			return b.String(), i + 1, nil
		default:
			b.WriteRune(r[i])
		}
	}
	return "", 0, fmt.Errorf("unterminated string literal")
}

// item is one selected column.
type item struct {
	family    string // "" for _key
	qualifier *string
	cast      string // "", "STRING", "INT64"
	name      string
}

// operand is a literal or a parameter.
type operand struct {
	literal *string
	param   string
}

type cond struct {
	op    string // "=", "!=", "<", "<=", ">", ">=", "prefix"
	value operand
}

type query struct {
	star  bool
	items []item
	table string
	conds []cond
	limit int
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return token{}
}

func (p *parser) next() token { t := p.peek(); p.pos++; return t }

func (p *parser) keyword(k string) bool {
	if t := p.peek(); t.kind == "ident" && strings.EqualFold(t.text, k) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) op(o string) bool {
	if t := p.peek(); t.kind == "op" && t.text == o {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expect(o string) error {
	if !p.op(o) {
		return fmt.Errorf("expected %q at %q", o, p.peek().text)
	}
	return nil
}

func parseQuery(src string) (*query, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	q := &query{limit: -1}
	if !p.keyword("SELECT") {
		return nil, fmt.Errorf("only SELECT queries are supported")
	}
	if p.op("*") {
		q.star = true
	} else {
		for {
			it, err := p.item()
			if err != nil {
				return nil, err
			}
			if p.keyword("AS") {
				t := p.next()
				if t.kind != "ident" {
					return nil, fmt.Errorf("expected an alias after AS")
				}
				it.name = t.text
			}
			q.items = append(q.items, it)
			if !p.op(",") {
				break
			}
		}
	}
	if !p.keyword("FROM") {
		return nil, fmt.Errorf("expected FROM at %q", p.peek().text)
	}
	t := p.next()
	if t.kind != "ident" {
		return nil, fmt.Errorf("expected a table name after FROM")
	}
	q.table = t.text
	if p.keyword("WHERE") {
		for {
			c, err := p.cond()
			if err != nil {
				return nil, err
			}
			q.conds = append(q.conds, c)
			if !p.keyword("AND") {
				break
			}
		}
	}
	if p.keyword("LIMIT") {
		t := p.next()
		n, err := strconv.Atoi(t.text)
		if t.kind != "number" || err != nil {
			return nil, fmt.Errorf("LIMIT takes a number")
		}
		q.limit = n
	}
	p.op(";")
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("%q is not supported by the emulator's SQL", p.peek().text)
	}
	for i := range q.items {
		if q.items[i].name == "" {
			q.items[i].name = fmt.Sprintf("$col%d", i+1)
		}
	}
	return q, nil
}

func (p *parser) item() (item, error) {
	for _, fn := range []string{"CAST", "TO_INT64"} {
		if p.peek().kind == "ident" && strings.EqualFold(p.peek().text, fn) && p.pos+1 < len(p.toks) && p.toks[p.pos+1].text == "(" {
			p.pos += 2
			inner, err := p.item()
			if err != nil {
				return item{}, err
			}
			if inner.cast != "" || inner.family != "" && inner.qualifier == nil {
				return item{}, fmt.Errorf("%s takes _key or a column", fn)
			}
			cast := "INT64"
			if fn == "CAST" {
				if !p.keyword("AS") {
					return item{}, fmt.Errorf("expected AS in CAST")
				}
				typ := strings.ToUpper(p.next().text)
				if typ != "STRING" && typ != "BYTES" {
					return item{}, fmt.Errorf("CAST to %s is not supported by the emulator's SQL", typ)
				}
				cast = typ
				if typ == "BYTES" {
					cast = ""
				}
			}
			if err := p.expect(")"); err != nil {
				return item{}, err
			}
			inner.cast = cast
			inner.name = ""
			return inner, nil
		}
	}
	t := p.next()
	if t.kind != "ident" {
		return item{}, fmt.Errorf("%q is not supported by the emulator's SQL", t.text)
	}
	if t.text == "_key" {
		return item{name: "_key"}, nil
	}
	if t.text == "_timestamp" {
		return item{}, fmt.Errorf("_timestamp is not supported by the emulator's SQL")
	}
	it := item{family: t.text, name: t.text}
	if p.op("[") {
		k := p.next()
		if k.kind != "string" && k.kind != "bytes" {
			return item{}, fmt.Errorf("a column is named by a string or bytes literal")
		}
		q := k.text
		it.qualifier, it.name = &q, ""
		if err := p.expect("]"); err != nil {
			return item{}, err
		}
	} else if p.peek().text == "(" {
		return item{}, fmt.Errorf("function %s is not supported by the emulator's SQL", t.text)
	}
	return it, nil
}

func (p *parser) operand() (operand, error) {
	t := p.next()
	switch t.kind {
	case "string", "bytes":
		v := t.text
		return operand{literal: &v}, nil
	case "param":
		return operand{param: t.text}, nil
	}
	return operand{}, fmt.Errorf("expected a string, bytes or @parameter, got %q", t.text)
}

func (p *parser) cond() (cond, error) {
	if p.keyword("STARTS_WITH") {
		if err := p.expect("("); err != nil {
			return cond{}, err
		}
		if t := p.next(); t.text != "_key" {
			return cond{}, fmt.Errorf("STARTS_WITH is supported on _key alone")
		}
		if err := p.expect(","); err != nil {
			return cond{}, err
		}
		v, err := p.operand()
		if err != nil {
			return cond{}, err
		}
		return cond{op: "prefix", value: v}, p.expect(")")
	}
	if t := p.next(); t.text != "_key" {
		return cond{}, fmt.Errorf("WHERE is supported on _key alone, not %q", t.text)
	}
	t := p.next()
	if t.kind != "op" || !strings.Contains("= != <> < <= > >=", t.text) || t.text == "," {
		return cond{}, fmt.Errorf("expected a comparison after _key")
	}
	op := t.text
	if op == "<>" {
		op = "!="
	}
	v, err := p.operand()
	return cond{op: op, value: v}, err
}

// --- the types ---

var (
	bytesType  = &btpb.Type{Kind: &btpb.Type_BytesType{BytesType: &btpb.Type_Bytes{}}}
	stringType = &btpb.Type{Kind: &btpb.Type_StringType{StringType: &btpb.Type_String{}}}
	int64Type  = &btpb.Type{Kind: &btpb.Type_Int64Type{Int64Type: &btpb.Type_Int64{}}}
	familyType = &btpb.Type{Kind: &btpb.Type_MapType{MapType: &btpb.Type_Map{KeyType: bytesType, ValueType: bytesType}}}
)

func (it item) typ() *btpb.Type {
	switch {
	case it.cast == "STRING":
		return stringType
	case it.cast == "INT64":
		return int64Type
	case it.family != "" && it.qualifier == nil:
		return familyType
	}
	return bytesType
}

func typeName(t *btpb.Type) (string, error) {
	switch t.GetKind().(type) {
	case *btpb.Type_BytesType:
		return "BYTES", nil
	case *btpb.Type_StringType:
		return "STRING", nil
	}
	return "", fmt.Errorf("parameters of type %v are not supported by the emulator's SQL", t)
}

// resolve checks q against the table's families and expands *.
func (q *sqlFront) resolve(ctx context.Context, instance string, qu *query) error {
	t, err := btapb.NewBigtableTableAdminClient(q.upstream).GetTable(ctx, &btapb.GetTableRequest{Name: instance + "/tables/" + qu.table, View: btapb.Table_SCHEMA_VIEW})
	if err != nil {
		return err
	}
	fams := t.GetColumnFamilies()
	if qu.star {
		qu.items = []item{{name: "_key"}}
		ids := make([]string, 0, len(fams))
		for id := range fams {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			qu.items = append(qu.items, item{family: id, name: id})
		}
		return nil
	}
	for _, it := range qu.items {
		if _, ok := fams[it.family]; it.family != "" && !ok {
			return status.Errorf(codes.InvalidArgument, "Unrecognized name: %s", it.family)
		}
	}
	return nil
}

func (q *sqlFront) prepare(ctx context.Context, req *btpb.PrepareQueryRequest) (proto.Message, error) {
	if !instanceRe.MatchString(req.GetInstanceName()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid instance_name %q", req.GetInstanceName())
	}
	qu, err := parseQuery(req.GetQuery())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	pq := prepared{Instance: req.GetInstanceName(), Query: req.GetQuery(), Params: map[string]string{}, Until: q.now().Add(preparedValidity).Unix()}
	for name, t := range req.GetParamTypes() {
		n, err := typeName(t)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "@%s: %v", name, err)
		}
		pq.Params[name] = n
	}
	for _, c := range qu.conds {
		if c.value.param != "" {
			if _, ok := pq.Params[c.value.param]; !ok {
				return nil, status.Errorf(codes.InvalidArgument, "Query parameter '%s' not found", c.value.param)
			}
		}
	}
	if err := q.resolve(ctx, req.GetInstanceName(), qu); err != nil {
		return nil, err
	}
	cols := make([]*btpb.ColumnMetadata, 0, len(qu.items))
	for _, it := range qu.items {
		cols = append(cols, &btpb.ColumnMetadata{Name: it.name, Type: it.typ()})
	}
	blob, err := json.Marshal(pq)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &btpb.PrepareQueryResponse{
		Metadata:      &btpb.ResultSetMetadata{Schema: &btpb.ResultSetMetadata_ProtoSchema{ProtoSchema: &btpb.ProtoSchema{Columns: cols}}},
		PreparedQuery: blob,
		ValidUntil:    timestamppb.New(time.Unix(pq.Until, 0)),
	}, nil
}

// paramBytes reads a bound parameter's value.
func paramBytes(v *btpb.Value) ([]byte, bool) {
	switch k := v.GetKind().(type) {
	case *btpb.Value_BytesValue:
		return k.BytesValue, true
	case *btpb.Value_StringValue:
		return []byte(k.StringValue), true
	case nil:
		return nil, false
	}
	return nil, false
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
	qu, err := parseQuery(pq.Query)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if err := q.resolve(ctx, pq.Instance, qu); err != nil {
		return nil, err
	}
	// The conditions, with their parameters bound.
	type bound struct {
		op string
		v  []byte
	}
	var conds []bound
	for _, c := range qu.conds {
		var v []byte
		if c.value.literal != nil {
			v = []byte(*c.value.literal)
		} else {
			pv, ok := req.GetParams()[c.value.param]
			if !ok {
				return nil, status.Errorf(codes.InvalidArgument, "Query parameter '%s' is not bound", c.value.param)
			}
			b, ok := paramBytes(pv)
			if !ok {
				// A NULL compares as nothing: no row matches.
				return q.results(nil)
			}
			v = b
		}
		conds = append(conds, bound{c.op, v})
	}
	rr := &btpb.ReadRowsRequest{TableName: pq.Instance + "/tables/" + qu.table,
		Filter: &btpb.RowFilter{Filter: &btpb.RowFilter_CellsPerColumnLimitFilter{CellsPerColumnLimitFilter: 1}}}
	rows, err := readAll(ctx, btpb.NewBigtableClient(q.upstream), rr)
	if err != nil {
		return nil, err
	}
	var values []*btpb.Value
	n := 0
rows:
	for _, r := range rows {
		if qu.limit >= 0 && n >= qu.limit {
			break
		}
		for _, c := range conds {
			cmp := bytes.Compare(r.key, c.v)
			ok := false
			switch c.op {
			case "=":
				ok = cmp == 0
			case "!=":
				ok = cmp != 0
			case "<":
				ok = cmp < 0
			case "<=":
				ok = cmp <= 0
			case ">":
				ok = cmp > 0
			case ">=":
				ok = cmp >= 0
			case "prefix":
				ok = bytes.HasPrefix(r.key, c.v)
			}
			if !ok {
				continue rows
			}
		}
		for _, it := range qu.items {
			v, err := it.value(r)
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		n++
	}
	return q.results(values)
}

// value is it's value in row r.
func (it item) value(r row) (*btpb.Value, error) {
	var raw []byte
	found := false
	switch {
	case it.family == "":
		raw, found = r.key, true
	case it.qualifier == nil:
		var entries []*btpb.Value
		for _, c := range r.cells {
			if c.family == it.family {
				entries = append(entries, &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{Values: []*btpb.Value{
					{Kind: &btpb.Value_BytesValue{BytesValue: c.qualifier}}, {Kind: &btpb.Value_BytesValue{BytesValue: c.value}},
				}}}})
			}
		}
		return &btpb.Value{Kind: &btpb.Value_ArrayValue{ArrayValue: &btpb.ArrayValue{Values: entries}}}, nil
	default:
		for _, c := range r.cells {
			if c.family == it.family && string(c.qualifier) == *it.qualifier {
				raw, found = c.value, true
				break
			}
		}
	}
	if !found {
		return &btpb.Value{}, nil // NULL
	}
	switch it.cast {
	case "STRING":
		return &btpb.Value{Kind: &btpb.Value_StringValue{StringValue: string(raw)}}, nil
	case "INT64":
		if len(raw) != 8 {
			return nil, status.Errorf(codes.InvalidArgument, "TO_INT64: a value of %d bytes is not a big-endian INT64", len(raw))
		}
		return &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: int64(binary.BigEndian.Uint64(raw))}}, nil
	}
	return &btpb.Value{Kind: &btpb.Value_BytesValue{BytesValue: raw}}, nil
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
