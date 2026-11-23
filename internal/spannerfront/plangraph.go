package spannerfront

// Graph queries' plans (#2029). Spanner runs a graph query (GQL: GRAPH g
// MATCH ... RETURN ..., and GRAPH_TABLE in a SQL query's FROM clause) on
// the relational operators of its query plans: each node and edge pattern
// reads its element table (a Scan, or a Filter Scan seeking on its key),
// and an edge joins its source and destination nodes on the element keys
// the property graph declares (Google's binary operators page draws a
// graph query's plan with the same Distributed Cross Apply, Cross Apply
// and Filter Scan operators as SQL's). So the front plans a graph query
// as the SQL query that reads the same rows, and the planner and PROFILE's
// counts read that SQL (plangraph2.go translates it).
//
// The property graph's element tables, labels, keys and properties come
// from INFORMATION_SCHEMA.PROPERTY_GRAPHS (PROPERTY_GRAPH_METADATA_JSON).

import (
	"encoding/json"
	"fmt"
	"strings"
)

// graphMeta is a property graph's PROPERTY_GRAPH_METADATA_JSON.
type graphMeta struct {
	Name       string         `json:"name"`
	NodeTables []graphElement `json:"nodeTables"`
	EdgeTables []graphElement `json:"edgeTables"`
}

type graphElement struct {
	Name          string   `json:"name"`
	BaseTableName string   `json:"baseTableName"`
	KeyColumns    []string `json:"keyColumns"`
	LabelNames    []string `json:"labelNames"`
	PropertyDefs  []struct {
		Name string `json:"propertyDeclarationName"`
		SQL  string `json:"valueExpressionSql"`
	} `json:"propertyDefinitions"`
	Source *graphEndpoint `json:"sourceNodeTable"`
	Dest   *graphEndpoint `json:"destinationNodeTable"`
}

type graphEndpoint struct {
	EdgeCols []string `json:"edgeTableColumns"`
	NodeCols []string `json:"nodeTableColumns"`
	Node     string   `json:"nodeTableName"`
}

func parseGraphMeta(js string) (*graphMeta, error) {
	var g graphMeta
	if err := json.Unmarshal([]byte(js), &g); err != nil {
		return nil, fmt.Errorf("reading a property graph's metadata: %w", err)
	}
	return &g, nil
}

func (e *graphElement) hasLabel(l string) bool {
	for _, x := range e.LabelNames {
		if strings.EqualFold(x, l) {
			return true
		}
	}
	return false
}

// hasProperty says whether the element table defines a property.
func (e *graphElement) hasProperty(prop string) bool {
	for _, p := range e.PropertyDefs {
		if strings.EqualFold(p.Name, prop) {
			return true
		}
	}
	return false
}

// column is the column a property reads.
func (e *graphElement) column(prop string) (string, error) {
	for _, p := range e.PropertyDefs {
		if strings.EqualFold(p.Name, prop) {
			c := strings.Trim(p.SQL, "`")
			if !isPlainName(c) {
				return "", unsupportedSQL{fmt.Sprintf("property %s, which element table %s defines by the expression %s", prop, e.Name, p.SQL)}
			}
			return c, nil
		}
	}
	return "", unsupportedSQL{fmt.Sprintf("property %s, which element table %s does not have", prop, e.Name)}
}

func isPlainName(s string) bool {
	if s == "" || !isIdentStart(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isIdentByte(s[i]) {
			return false
		}
	}
	return true
}

// mentionsGraph says whether a GoogleSQL statement is a graph query or
// reads GRAPH_TABLE.
func mentionsGraph(src string) bool {
	toks, err := lexSQL(src, false)
	if err != nil {
		return false
	}
	for _, t := range toks {
		if t.kind == tkIdent && (strings.EqualFold(t.text, "GRAPH") || strings.EqualFold(t.text, "GRAPH_TABLE")) {
			return true
		}
	}
	return false
}

// graphQuant is a quantified edge pattern, -[:Label]->{m,n}: the paths of
// m to n edges of one edge table, as a FROM item whose columns are each
// path's first source key (cb_src_<column>), last destination key
// (cb_dst_<column>), its length (cb_len) and, for each property the query
// reads of its group variable, the path's values of it in order
// (cb_arr_<property>). The planner draws it as Google's binary operators
// page draws a quantified path: a Recursive Union of the first edges (its
// base input; the start nodes when m is 0) and, for each further edge, a
// Cross Apply of the previous iteration's rows (a Recursive Spool Scan) to
// the edge table.
type graphQuant struct {
	alias    string
	table    *graphElement
	node     *graphElement // the node table a path of 0 edges stays on
	min, max int
	// hopConds are the edge pattern's own conditions, on each edge, with
	// the edge as cb_hop
	hopConds []string
	arrs     []string // properties collected as arrays
	withLen  bool     // the paths' lengths are read (cb_len)
	trail    bool     // no edge twice (TRAIL)
	acyc     bool     // no node twice (ACYCLIC)
}

// hopsSQL is the SQL for the paths of k edges.
func (q *graphQuant) hopsSQL(k int) string {
	e := q.table
	var cols, froms, conds []string
	if k == 0 {
		n := q.node
		for i, c := range e.Source.EdgeCols {
			cols = append(cols, "cb_e0."+e.Source.NodeCols[i]+" AS cb_src_"+c)
		}
		for i, c := range e.Dest.EdgeCols {
			cols = append(cols, "cb_e0."+e.Dest.NodeCols[i]+" AS cb_dst_"+c)
		}
		if q.withLen {
			cols = append(cols, "0 AS cb_len")
		}
		for _, p := range q.arrs {
			cols = append(cols, "[] AS cb_arr_"+p)
		}
		return "SELECT " + strings.Join(cols, ", ") + " FROM " + n.BaseTableName + " AS cb_e0"
	}
	for _, c := range e.Source.EdgeCols {
		cols = append(cols, "cb_e1."+c+" AS cb_src_"+c)
	}
	for _, c := range e.Dest.EdgeCols {
		cols = append(cols, fmt.Sprintf("cb_e%d.%s AS cb_dst_%s", k, c, c))
	}
	if q.withLen {
		cols = append(cols, fmt.Sprintf("%d AS cb_len", k))
	}
	for _, p := range q.arrs {
		col, _ := e.column(p)
		var vals []string
		for i := 1; i <= k; i++ {
			vals = append(vals, fmt.Sprintf("cb_e%d.%s", i, col))
		}
		cols = append(cols, "["+strings.Join(vals, ", ")+"] AS cb_arr_"+p)
	}
	for i := 1; i <= k; i++ {
		froms = append(froms, fmt.Sprintf("%s AS cb_e%d", e.BaseTableName, i))
		if i > 1 {
			// the edge leaves the node the one before reached
			for j, c := range e.Source.EdgeCols {
				conds = append(conds, fmt.Sprintf("cb_e%d.%s = cb_e%d.%s", i, c, i-1, e.Dest.EdgeCols[j]))
			}
		}
		for _, c := range q.hopConds {
			conds = append(conds, strings.ReplaceAll(c, "cb_hop.", fmt.Sprintf("cb_e%d.", i)))
		}
	}
	if q.trail {
		for i := 1; i <= k; i++ {
			for j := i + 1; j <= k; j++ {
				conds = append(conds, "NOT ("+keysEqual(e.KeyColumns, fmt.Sprintf("cb_e%d.", i), fmt.Sprintf("cb_e%d.", j))+")")
			}
		}
	}
	if q.acyc {
		// the path's nodes: the first edge's source, every edge's destination
		nodes := []struct {
			alias string
			cols  []string
		}{{"cb_e1.", e.Source.EdgeCols}}
		for i := 1; i <= k; i++ {
			nodes = append(nodes, struct {
				alias string
				cols  []string
			}{fmt.Sprintf("cb_e%d.", i), e.Dest.EdgeCols})
		}
		for i := range nodes {
			for j := i + 1; j < len(nodes); j++ {
				var eq []string
				for c := range nodes[i].cols {
					eq = append(eq, nodes[i].alias+nodes[i].cols[c]+" = "+nodes[j].alias+nodes[j].cols[c])
				}
				conds = append(conds, "NOT ("+strings.Join(eq, " AND ")+")")
			}
		}
	}
	return "SELECT " + strings.Join(cols, ", ") + " FROM " + strings.Join(froms, ", ") + whereSQL(conds)
}

// keysEqual is a AND b's columns are equal.
func keysEqual(cols []string, a, b string) string {
	var eq []string
	for _, c := range cols {
		eq = append(eq, a+c+" = "+b+c)
	}
	return strings.Join(eq, " AND ")
}

// pathsSQL is the SQL for the paths of lo to hi edges.
func (q *graphQuant) pathsSQL(lo, hi int) string {
	var parts []string
	for k := lo; k <= hi; k++ {
		parts = append(parts, q.hopsSQL(k))
	}
	return strings.Join(parts, " UNION ALL ")
}

// graphToSQL rewrites a graph query, or each GRAPH_TABLE of a SQL query,
// as SQL.
func graphToSQL(src string, graphs map[string]*graphMeta) (string, error) {
	s, _, err := graphToSQLQuant(src, graphs)
	return s, err
}

// graphToSQLQuant is graphToSQL, with the quantified edge patterns it
// wrote as FROM items, by their lower-case aliases.
func graphToSQLQuant(src string, graphs map[string]*graphMeta) (string, map[string]*graphQuant, error) {
	quants := map[string]*graphQuant{}
	s, err := graphRewrite(src, graphs, quants)
	return s, quants, err
}

func graphRewrite(src string, graphs map[string]*graphMeta, quants map[string]*graphQuant) (string, error) {
	toks, err := lexSQL(src, false)
	if err != nil {
		return "", err
	}
	ps := &sqlParser{src: src, toks: toks, stmtHints: map[string]string{}}
	var out strings.Builder
	pos := 0
	if ps.isP("@{") {
		if _, err := ps.hints(); err != nil {
			return "", err
		}
	}
	if ps.isKw("GRAPH") {
		out.WriteString(src[:ps.peek().start])
		gq := newGQL(ps, graphs, quants, false)
		sql, err := gq.statement()
		if err != nil {
			return "", err
		}
		if ps.isP(";") {
			ps.p++
		}
		if ps.peek().kind != tkEOF {
			return "", ps.errAt("the end of the graph query")
		}
		out.WriteString(sql)
		return out.String(), nil
	}
	for ps.peek().kind != tkEOF {
		if ps.isKw("GRAPH_TABLE") && ps.peekN(1).text == "(" {
			out.WriteString(src[pos:ps.peek().start])
			ps.p += 2
			gq := newGQL(ps, graphs, quants, true)
			sql, err := gq.statement()
			if err != nil {
				return "", err
			}
			if err := ps.expectP(")"); err != nil {
				return "", err
			}
			out.WriteString("(" + sql + ")")
			pos = ps.toks[ps.p-1].end
			continue
		}
		ps.p++
	}
	out.WriteString(src[pos:])
	return out.String(), nil
}
