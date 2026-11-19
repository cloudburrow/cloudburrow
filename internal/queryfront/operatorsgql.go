package queryfront

// GQL's !=, IN, NOT IN and OR (#1888,
// https://cloud.google.com/datastore/docs/reference/gql_reference): the
// emulator parses them, then refuses to run them (operators.go), and
// returns no parsed form. The front parses the WHERE clause's structure
// itself (AND before OR, parentheses, each condition's comparator) and has
// the emulator parse everything else, so literals, bindings, keys and
// names keep the emulator's own parsing:
//
//   - the query without its WHERE clause, for its kind, projection,
//     distinct, orders, limit, offset and cursors;
//   - SELECT __key__ FROM <kind> WHERE <condition> AND ... LIMIT 1, where
//     each condition is written as an equality, `p = v` (`p IS NULL` for an
//     IN of a binding, whose values the front reads from the binding), and
//     IS NULL, HAS ANCESTOR and HAS DESCENDANT as they are: the emulator
//     answers its filters, in their order, from which the front takes each
//     condition's property and value, and puts back its comparator.
//
// Positional bindings are passed as named ones, so a binding the second
// query does not use is not refused as unused.

import (
	"context"
	"fmt"
	"strings"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// gqlPositionalPrefix names a positional binding passed as a named one.
const gqlPositionalPrefix = "cloudburrowPositional"

// gqlNeedsOperators reports whether GQL query q has !=, IN, NOT IN or OR.
func gqlNeedsOperators(q string) bool {
	toks := gqlTokens(q)
	for i, t := range toks {
		text := q[t.start:t.end]
		switch {
		case t.kind == 'w' && (strings.EqualFold(text, "OR") || strings.EqualFold(text, "IN") || strings.EqualFold(text, "NOT")):
			return true
		case t.kind == 'p' && text == "!" && i+1 < len(toks) && q[toks[i+1].start:toks[i+1].end] == "=":
			return true
		}
	}
	return false
}

// gqlNode is a WHERE clause's condition tree.
type gqlNode struct {
	op   pb.CompositeFilter_Operator // of a composite
	kids []*gqlNode
	// A condition: the texts given to the emulator, and how its filters
	// become this condition's.
	items  []string
	cmp    pb.PropertyFilter_Operator // UNSPECIFIED: the emulator's filter as it is
	values []*pb.Value                // an IN or NOT IN of a binding
	list   bool                       // IN or NOT IN
}

type gqlParser struct {
	q    string
	toks []gqlToken
	i    int
	end  int
	bind map[string]*pb.GqlQueryParameter
}

func (p *gqlParser) text(i int) string { return p.q[p.toks[i].start:p.toks[i].end] }

func (p *gqlParser) word(i int, w string) bool {
	return i < p.end && p.toks[i].kind == 'w' && strings.EqualFold(p.text(i), w)
}

func (p *gqlParser) errorf(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, "GQL query error: "+format, args...)
}

func (p *gqlParser) expr() (*gqlNode, error) {
	var kids []*gqlNode
	for {
		n, err := p.and()
		if err != nil {
			return nil, err
		}
		kids = append(kids, n)
		if !p.word(p.i, "OR") {
			break
		}
		p.i++
	}
	if len(kids) == 1 {
		return kids[0], nil
	}
	return &gqlNode{op: pb.CompositeFilter_OR, kids: kids}, nil
}

func (p *gqlParser) and() (*gqlNode, error) {
	var kids []*gqlNode
	for {
		n, err := p.atom()
		if err != nil {
			return nil, err
		}
		kids = append(kids, n)
		if !p.word(p.i, "AND") {
			break
		}
		p.i++
	}
	if len(kids) == 1 {
		return kids[0], nil
	}
	return &gqlNode{op: pb.CompositeFilter_AND, kids: kids}, nil
}

func (p *gqlParser) atom() (*gqlNode, error) {
	if p.i < p.end && p.text(p.i) == "(" {
		p.i++
		n, err := p.expr()
		if err != nil {
			return nil, err
		}
		if p.i >= p.end || p.text(p.i) != ")" {
			return nil, p.errorf("expected ) in the WHERE clause")
		}
		p.i++
		return n, nil
	}
	// A condition runs to the next AND, OR or ) outside parentheses.
	start, depth := p.i, 0
	for ; p.i < p.end; p.i++ {
		t := p.text(p.i)
		if depth == 0 && (p.word(p.i, "AND") || p.word(p.i, "OR") || t == ")") {
			break
		}
		switch t {
		case "(":
			depth++
		case ")":
			depth--
		}
	}
	if p.i == start {
		return nil, p.errorf("expected a condition in the WHERE clause")
	}
	return p.condition(start, p.i)
}

// span is the query text of tokens [a, b).
func (p *gqlParser) span(a, b int) string {
	if a >= b {
		return ""
	}
	return p.q[p.toks[a].start:p.toks[b-1].end]
}

// isValue reports whether tokens [a, b) are a value, not a property name.
func (p *gqlParser) isValue(a, b int) bool {
	t := p.toks[a]
	text := p.text(a)
	switch {
	case t.kind == 's':
		return true
	case t.kind == 'q':
		return false
	case t.kind == 'p':
		return text == "-" || text == "+" || text == "."
	case strings.HasPrefix(text, "@") || (text[0] >= '0' && text[0] <= '9'):
		return true
	}
	for _, w := range []string{"TRUE", "FALSE", "NULL"} {
		if strings.EqualFold(text, w) && b == a+1 {
			return true
		}
	}
	for _, w := range []string{"KEY", "ARRAY", "BLOB", "DATETIME"} {
		if strings.EqualFold(text, w) && a+1 < b && p.text(a+1) == "(" {
			return true
		}
	}
	return false
}

var gqlFlipped = map[pb.PropertyFilter_Operator]pb.PropertyFilter_Operator{
	pb.PropertyFilter_LESS_THAN: pb.PropertyFilter_GREATER_THAN, pb.PropertyFilter_LESS_THAN_OR_EQUAL: pb.PropertyFilter_GREATER_THAN_OR_EQUAL,
	pb.PropertyFilter_GREATER_THAN: pb.PropertyFilter_LESS_THAN, pb.PropertyFilter_GREATER_THAN_OR_EQUAL: pb.PropertyFilter_LESS_THAN_OR_EQUAL,
	pb.PropertyFilter_EQUAL: pb.PropertyFilter_EQUAL, pb.PropertyFilter_NOT_EQUAL: pb.PropertyFilter_NOT_EQUAL,
}

// condition is the condition of tokens [a, b).
func (p *gqlParser) condition(a, b int) (*gqlNode, error) {
	whole := p.span(a, b)
	// IS NULL, HAS ANCESTOR and HAS DESCENDANT are the emulator's.
	if (b-a >= 3 && p.word(b-2, "IS") && p.word(b-1, "NULL")) || containsWords(p, a, b, "HAS") {
		return &gqlNode{items: []string{whole}}, nil
	}
	depth := 0
	for i := a; i < b; i++ {
		t := p.text(i)
		switch t {
		case "(":
			depth++
			continue
		case ")":
			depth--
			continue
		}
		if depth != 0 || i == a {
			continue
		}
		var op pb.PropertyFilter_Operator
		n := 1
		switch {
		case t == "!" && i+1 < b && p.text(i+1) == "=":
			op, n = pb.PropertyFilter_NOT_EQUAL, 2
		case t == "<" && i+1 < b && p.text(i+1) == "=":
			op, n = pb.PropertyFilter_LESS_THAN_OR_EQUAL, 2
		case t == ">" && i+1 < b && p.text(i+1) == "=":
			op, n = pb.PropertyFilter_GREATER_THAN_OR_EQUAL, 2
		case t == "<":
			op = pb.PropertyFilter_LESS_THAN
		case t == ">":
			op = pb.PropertyFilter_GREATER_THAN
		case t == "=" || p.word(i, "CONTAINS"):
			op = pb.PropertyFilter_EQUAL
		case p.word(i, "NOT") && p.word(i+1, "IN"):
			op, n = pb.PropertyFilter_NOT_IN, 2
		case p.word(i, "IN"):
			op = pb.PropertyFilter_IN
		default:
			continue
		}
		left, right := [2]int{a, i}, [2]int{i + n, b}
		if right[0] >= right[1] {
			return nil, p.errorf("expected a value after %q", strings.TrimSpace(p.span(i, i+n)))
		}
		if p.isValue(left[0], left[1]) {
			// <value> <backward-comparator> <property-name>; a value IN a
			// property is the property's equality.
			left, right = right, left
			if op == pb.PropertyFilter_IN {
				op = pb.PropertyFilter_EQUAL
			} else if f, ok := gqlFlipped[op]; ok && op != pb.PropertyFilter_NOT_IN {
				op = f
			} else {
				return nil, p.errorf("%q: NOT IN takes a property name on its left", whole)
			}
		}
		prop := p.span(left[0], left[1])
		if op != pb.PropertyFilter_IN && op != pb.PropertyFilter_NOT_IN {
			return &gqlNode{items: []string{prop + " = " + p.span(right[0], right[1])}, cmp: op}, nil
		}
		node := &gqlNode{cmp: op, list: true}
		r0, r1 := right[0], right[1]
		switch {
		case p.word(r0, "ARRAY") && r0+1 < r1 && p.text(r0+1) == "(" && p.text(r1-1) == ")":
			// Each element an equality of its own.
			d, from := 0, r0+2
			for j := r0 + 2; j < r1; j++ {
				switch t := p.text(j); {
				case t == "(":
					d++
				case t == ")" && d > 0:
					d--
				case (t == "," && d == 0) || j == r1-1:
					if from >= j {
						return nil, p.errorf("%q: an empty element of ARRAY", whole)
					}
					node.items = append(node.items, prop+" = "+p.span(from, j))
					from = j + 1
				}
			}
		case r1 == r0+1 && strings.HasPrefix(p.text(r0), "@"):
			b, ok := p.bind[strings.TrimPrefix(p.text(r0), "@")]
			if !ok {
				return nil, p.errorf("no binding for %s", p.text(r0))
			}
			if b.GetValue() == nil {
				return nil, p.errorf("%s is a cursor binding, not a value", p.text(r0))
			}
			if av := b.GetValue().GetArrayValue(); av != nil {
				node.values = av.GetValues()
				if len(node.values) == 0 {
					node.values = []*pb.Value{}
				}
			} else {
				node.values = []*pb.Value{b.GetValue()}
				node.list = false // not an array: validateOperators refuses it
			}
			node.items = []string{prop + " IS NULL"}
		default:
			return nil, p.errorf("%q: %s takes ARRAY(...) or a binding", whole, op)
		}
		return node, nil
	}
	return nil, p.errorf("%q is not a condition", whole)
}

func containsWords(p *gqlParser, a, b int, w string) bool {
	for i := a; i < b; i++ {
		if p.word(i, w) {
			return true
		}
	}
	return false
}

// collect appends n's items to out, in order.
func (n *gqlNode) collect(out []string) []string {
	if n.kids == nil {
		return append(out, n.items...)
	}
	for _, k := range n.kids {
		out = k.collect(out)
	}
	return out
}

// build is n's filter from the emulator's filters for its items, in order.
func (n *gqlNode) build(fs *[]*pb.PropertyFilter) *pb.Filter {
	if n.kids != nil {
		cf := &pb.CompositeFilter{Op: n.op}
		for _, k := range n.kids {
			cf.Filters = append(cf.Filters, k.build(fs))
		}
		return &pb.Filter{FilterType: &pb.Filter_CompositeFilter{CompositeFilter: cf}}
	}
	mine := (*fs)[:len(n.items)]
	*fs = (*fs)[len(n.items):]
	pf := proto.Clone(mine[0]).(*pb.PropertyFilter)
	switch {
	case n.cmp == pb.PropertyFilter_OPERATOR_UNSPECIFIED:
	case n.values != nil || (n.cmp == pb.PropertyFilter_IN || n.cmp == pb.PropertyFilter_NOT_IN):
		pf.Op = n.cmp
		vals := n.values
		if vals == nil {
			for _, f := range mine {
				vals = append(vals, f.GetValue())
			}
		}
		if n.list {
			pf.Value = &pb.Value{ValueType: &pb.Value_ArrayValue{ArrayValue: &pb.ArrayValue{Values: vals}}}
		} else {
			pf.Value = vals[0]
		}
	default:
		pf.Op = n.cmp
	}
	return &pb.Filter{FilterType: &pb.Filter_PropertyFilter{PropertyFilter: pf}}
}

// namedBindings is g with its positional bindings passed as named ones,
// its query text rewritten to match.
func namedBindings(g *pb.GqlQuery) *pb.GqlQuery {
	out := proto.Clone(g).(*pb.GqlQuery)
	if len(g.GetPositionalBindings()) == 0 {
		return out
	}
	if out.NamedBindings == nil {
		out.NamedBindings = map[string]*pb.GqlQueryParameter{}
	}
	for i, b := range g.GetPositionalBindings() {
		out.NamedBindings[fmt.Sprintf("%s%d", gqlPositionalPrefix, i+1)] = b
	}
	out.PositionalBindings = nil
	q := g.GetQueryString()
	var s strings.Builder
	last := 0
	for _, t := range gqlTokens(q) {
		text := q[t.start:t.end]
		if t.kind == 'w' && len(text) > 1 && text[0] == '@' && strings.Trim(text[1:], "0123456789") == "" {
			s.WriteString(q[last:t.start])
			s.WriteString("@" + gqlPositionalPrefix + text[1:])
			last = t.end
		}
	}
	s.WriteString(q[last:])
	out.QueryString = s.String()
	return out
}

// parseOperatorGQL is the structured form of the GQL query of req, which
// has !=, IN, NOT IN or OR (above). ctx is the outgoing call's.
func parseOperatorGQL(ctx context.Context, client pb.DatastoreClient, req request, partition *pb.PartitionId, g *pb.GqlQuery) (*pb.Query, error) {
	g = namedBindings(g)
	q := g.GetQueryString()
	toks := gqlTokens(q)
	p := &gqlParser{q: q, toks: toks, end: len(toks), bind: g.GetNamedBindings()}
	where, from, stop := -1, -1, len(toks)
	depth := 0
	for i := range toks {
		switch p.text(i) {
		case "(":
			depth++
		case ")":
			depth--
		}
		if depth != 0 {
			continue
		}
		switch {
		case p.word(i, "FROM") && from < 0 && where < 0:
			from = i
		case p.word(i, "WHERE") && where < 0:
			where = i
		case where >= 0 && stop == len(toks) && ((p.word(i, "ORDER") && p.word(i+1, "BY")) || p.word(i, "LIMIT") || p.word(i, "OFFSET")):
			stop = i
		}
	}
	if where < 0 {
		return nil, p.errorf("!=, IN, NOT IN and OR belong in a WHERE clause")
	}
	p.i, p.end = where+1, stop
	tree, err := p.expr()
	if err != nil {
		return nil, err
	}
	if p.i != stop {
		return nil, p.errorf("unexpected %q in the WHERE clause", p.text(p.i))
	}
	run := func(text string) (*pb.Query, error) {
		gq := proto.Clone(g).(*pb.GqlQuery)
		gq.QueryString = text
		resp, err := client.RunQuery(ctx, &pb.RunQueryRequest{ProjectId: req.GetProjectId(), DatabaseId: req.GetDatabaseId(),
			PartitionId: partition, QueryType: &pb.RunQueryRequest_GqlQuery{GqlQuery: gq}})
		if err != nil {
			return nil, err
		}
		if resp.GetQuery() == nil {
			return nil, status.Error(codes.Internal, "the emulator did not return the parsed form of the GQL query")
		}
		return resp.GetQuery(), nil
	}
	end := len(q)
	if stop < len(toks) {
		end = toks[stop].start
	}
	rest := q[:toks[where].start] + " " + q[end:]
	outer, err := run(rest)
	if err != nil {
		return nil, err
	}
	items := tree.collect(nil)
	fromText := ""
	if from >= 0 {
		fromText = q[toks[from].start:toks[where].start]
	}
	text := "SELECT __key__ " + fromText
	if len(items) > 0 {
		text += " WHERE " + strings.Join(items, " AND ")
	}
	inner, err := run(text + " LIMIT 1")
	if err != nil {
		return nil, err
	}
	var fs []*pb.PropertyFilter
	switch f := inner.GetFilter(); {
	case f.GetCompositeFilter() != nil:
		for _, s := range f.GetCompositeFilter().GetFilters() {
			fs = append(fs, s.GetPropertyFilter())
		}
	case f.GetPropertyFilter() != nil:
		fs = []*pb.PropertyFilter{f.GetPropertyFilter()}
	}
	if len(fs) != len(items) {
		return nil, status.Errorf(codes.Internal, "the emulator parsed %d conditions of the GQL query's WHERE clause, want %d", len(fs), len(items))
	}
	outer.Filter = tree.build(&fs)
	return outer, nil
}
