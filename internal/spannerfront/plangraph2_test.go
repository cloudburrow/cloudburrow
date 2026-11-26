package spannerfront

import (
	"strings"
	"testing"
)

// finGraphJSON is PROPERTY_GRAPH_METADATA_JSON as the engine reports it
// for Google's FinGraph (Person and Account nodes, Owns and Transfers
// edges; Spanner Graph's "Set up and query" page).
const finGraphJSON = `{"catalog":"","edgeTables":[{"baseCatalogName":"","baseSchemaName":"","baseTableName":"AccountTransferAccount","destinationNodeTable":{"edgeTableColumns":["to_id"],"nodeTableColumns":["id"],"nodeTableName":"Account"},"keyColumns":["id","to_id","create_time"],"kind":"EDGE","labelNames":["Transfers"],"name":"AccountTransferAccount","propertyDefinitions":[{"propertyDeclarationName":"id","valueExpressionSql":"id"},{"propertyDeclarationName":"to_id","valueExpressionSql":"to_id"},{"propertyDeclarationName":"amount","valueExpressionSql":"amount"},{"propertyDeclarationName":"create_time","valueExpressionSql":"create_time"},{"propertyDeclarationName":"order_number","valueExpressionSql":"order_number"}],"sourceNodeTable":{"edgeTableColumns":["id"],"nodeTableColumns":["id"],"nodeTableName":"Account"}},{"baseCatalogName":"","baseSchemaName":"","baseTableName":"PersonOwnAccount","destinationNodeTable":{"edgeTableColumns":["account_id"],"nodeTableColumns":["id"],"nodeTableName":"Account"},"keyColumns":["id","account_id"],"kind":"EDGE","labelNames":["Owns"],"name":"PersonOwnAccount","propertyDefinitions":[{"propertyDeclarationName":"id","valueExpressionSql":"id"},{"propertyDeclarationName":"account_id","valueExpressionSql":"account_id"},{"propertyDeclarationName":"create_time","valueExpressionSql":"create_time"}],"sourceNodeTable":{"edgeTableColumns":["id"],"nodeTableColumns":["id"],"nodeTableName":"Person"}}],"labels":[{"name":"Account","propertyDeclarationNames":["create_time","is_blocked","id","nick_name"]},{"name":"Person","propertyDeclarationNames":["country","birthday","id","name","city"]},{"name":"Owns","propertyDeclarationNames":["account_id","create_time","id"]},{"name":"Transfers","propertyDeclarationNames":["create_time","to_id","id","amount","order_number"]}],"name":"FinGraph","nodeTables":[{"baseCatalogName":"","baseSchemaName":"","baseTableName":"Account","keyColumns":["id"],"kind":"NODE","labelNames":["Account"],"name":"Account","propertyDefinitions":[{"propertyDeclarationName":"id","valueExpressionSql":"id"},{"propertyDeclarationName":"create_time","valueExpressionSql":"create_time"},{"propertyDeclarationName":"is_blocked","valueExpressionSql":"is_blocked"},{"propertyDeclarationName":"nick_name","valueExpressionSql":"nick_name"}]},{"baseCatalogName":"","baseSchemaName":"","baseTableName":"Person","keyColumns":["id"],"kind":"NODE","labelNames":["Person"],"name":"Person","propertyDefinitions":[{"propertyDeclarationName":"id","valueExpressionSql":"id"},{"propertyDeclarationName":"name","valueExpressionSql":"name"},{"propertyDeclarationName":"birthday","valueExpressionSql":"birthday"},{"propertyDeclarationName":"country","valueExpressionSql":"country"},{"propertyDeclarationName":"city","valueExpressionSql":"city"}]}],"propertyDeclarations":[{"name":"id","type":"INT64"},{"name":"create_time","type":"TIMESTAMP"},{"name":"is_blocked","type":"BOOL"},{"name":"nick_name","type":"STRING"},{"name":"name","type":"STRING"},{"name":"birthday","type":"TIMESTAMP"},{"name":"country","type":"STRING"},{"name":"city","type":"STRING"},{"name":"account_id","type":"INT64"},{"name":"to_id","type":"INT64"},{"name":"amount","type":"FLOAT64"},{"name":"order_number","type":"STRING"}],"schema":""}`

func finGraphs(t *testing.T) map[string]*graphMeta {
	g, err := parseGraphMeta(finGraphJSON)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*graphMeta{"fingraph": g}
}

// finSchema is FinGraph's tables.
func finSchema() *planSchema {
	s := &planSchema{tables: map[string]*planTable{}, indexes: map[string]*planIndex{}, views: map[string]string{}}
	tab := func(name, parent string, pk []string, cols ...string) {
		t := &planTable{name: name, parent: parent, cols: cols}
		for _, k := range pk {
			t.pk = append(t.pk, keyCol{name: k})
		}
		s.tables[strings.ToLower(name)] = t
	}
	tab("Person", "", []string{"id"}, "id", "name", "birthday", "country", "city")
	tab("Account", "", []string{"id"}, "id", "create_time", "is_blocked", "nick_name")
	tab("PersonOwnAccount", "Person", []string{"id", "account_id"}, "id", "account_id", "create_time")
	tab("AccountTransferAccount", "Account", []string{"id", "to_id", "create_time"}, "id", "to_id", "amount", "create_time", "order_number")
	return s
}

// graphPlan translates and plans a graph query.
func graphPlan(t *testing.T, gql string) (string, string) {
	t.Helper()
	sql, quant, err := graphToSQLQuant(gql, finGraphs(t))
	if err != nil {
		t.Fatalf("%s: %v", gql, err)
	}
	ps, q, err := parseQuerySQL(sql, false)
	if err != nil {
		t.Fatalf("%s: parse %s: %v", gql, sql, err)
	}
	b := &planBuilder{ps: ps, schema: finSchema(), ctes: map[string]*sqlCTE{}, cols: map[*sqlExpr]colRef{},
		subPlans: map[*sqlExpr]*planOp{}, orderFor: map[*sqlSelect][]sqlOrder{}, quant: quant}
	root, err := b.buildPlan(q)
	if err != nil {
		t.Fatalf("%s: plan %s: %v", gql, sql, err)
	}
	nodes, _ := planNodes(root)
	return sql, planTree(nodes)
}

// TestGraphPlansOfEveryStatement (#2029) plans the graph queries the first
// translation refused, on FinGraph: label expressions and unlabeled
// patterns over several element tables (a Union All of each table's
// reads), edges in either direction, OPTIONAL MATCH (an Outer Apply), NEXT
// and WITH (a derived table), LET, FOR (an Array Unnest), ORDER BY and
// LIMIT statements, {0,n} quantifiers (a Recursive Union whose base is the
// start nodes), a quantified edge's own WHERE, group variables in
// ARRAY_LENGTH and horizontal aggregates, path modes, PATH_LENGTH, element
// comparisons and GQL's set operations.
func TestGraphPlansOfEveryStatement(t *testing.T) {
	for _, c := range []struct {
		gql  string
		want []string
	}{
		{"GRAPH FinGraph MATCH (n) RETURN n.name, n.id", []string{"Union All", "Scan[TableScan Account full]", "Scan[TableScan Person full]"}},
		{"GRAPH FinGraph MATCH (n:Person|Account) RETURN n.id, n.name, n.nick_name", []string{"Union All", "Scan[TableScan Account full]", "Scan[TableScan Person full]"}},
		{"GRAPH FinGraph MATCH (n:!Person) RETURN n.id", []string{"Scan[TableScan Account full]"}},
		{"GRAPH FinGraph MATCH (n:%) RETURN n.id", []string{"Union All"}},
		{"GRAPH FinGraph MATCH -[e]-> RETURN e.id", []string{"Union All", "Scan[TableScan PersonOwnAccount", "Scan[TableScan AccountTransferAccount"}},
		{"GRAPH FinGraph MATCH ()-[e]-() RETURN COUNT(e.id) AS results", []string{"Aggregate", "Union All"}},
		{"GRAPH FinGraph MATCH (src:Account)-[transfer:Transfers]-(dst:Account) RETURN src.id AS src_id, transfer.amount, dst.id AS dst_id", []string{"Union All"}},
		{"GRAPH FinGraph MATCH (n) RETURN LABELS(n) AS label", []string{"Union All"}},
		{"GRAPH FinGraph MATCH (n:Person) OPTIONAL MATCH (n:Person)-[:Owns]->(a:Account {is_blocked: TRUE}) RETURN n.name, a.id AS blocked_account_id", []string{"Outer Apply", "Residual Condition: $is_blocked = (TRUE)"}},
		{"GRAPH FinGraph MATCH (src:Account)-[t:Transfers]->(dst:Account) RETURN dst, COUNT(*) AS n GROUP BY dst NEXT MATCH (dst)<-[:Owns]-(p:Person) RETURN p.name, dst.id, n", []string{"Aggregate", "Scan[TableScan PersonOwnAccount"}},
		{"GRAPH FinGraph MATCH (p:Person) LET n = p.name RETURN n", []string{"Scan[TableScan Person full]"}},
		{"GRAPH FinGraph MATCH (a:Account) FOR x IN [1, 2] WITH OFFSET AS o RETURN a.id AS id, x, o", []string{"Array Unnest"}},
		{"GRAPH FinGraph MATCH (a:Account) WITH a.id AS id, a.is_blocked AS b RETURN id, b", []string{"Scan[TableScan Account full]"}},
		{"GRAPH FinGraph MATCH (a:Account)-[t:Transfers]->(b:Account) ORDER BY t.amount DESC LIMIT 3 RETURN a.id AS a_id, t.amount", []string{"Sort Limit"}},
		{"GRAPH FinGraph MATCH (n:Person) RETURN n.name, n.id ORDER BY n.id LIMIT 1 NEXT RETURN *", []string{"Sort Limit"}},
		{"GRAPH FinGraph MATCH (src:Account {id: 7})-[e:Transfers]->{1, 3}(dst:Account) WHERE src != dst RETURN ARRAY_LENGTH(e) AS hops, dst.id AS dst_account_id", []string{"Recursive Union", "Base: Distributed Union", "Scan[TableScan AccountTransferAccount full]"}},
		{"GRAPH FinGraph MATCH (src:Account {id: 7})-[e:Transfers WHERE e.amount > 100]->{0,2} (dst:Account) WHERE src.id != dst.id LET total_amount = SUM(e.amount) RETURN src.id AS src_account_id, dst.id AS dst_account_id, ARRAY_LENGTH(e) AS number_of_hops, total_amount",
			[]string{"Recursive Union", "Scan[TableScan Account full]", "Residual Condition: ($amount > 100)"}},
		{"GRAPH FinGraph MATCH TRAIL (a1:Account)-[t1:Transfers]->(a2:Account)-[t2:Transfers]->(a3:Account)-[t3:Transfers]->(a4:Account) WHERE a1.id < a4.id RETURN t1.id as transfer1_id, t2.id as transfer2_id, t3.id as transfer3_id", []string{"Cross Apply"}},
		{"GRAPH FinGraph MATCH ACYCLIC (a1:Account)-[t1:Transfers]->(a2:Account)-[t2:Transfers]->(a3:Account) RETURN a1.id as account1_id, a2.id as account2_id, a3.id as account3_id", []string{"Cross Apply"}},
		{"GRAPH FinGraph MATCH TRAIL (a1)-[t1]-(a2), (a2)-[t1]-(a3) RETURN COUNT(1) as num_paths", []string{"Union All"}},
		{"GRAPH FinGraph MATCH TRAIL -[:Transfers]->{6} RETURN COUNT(1) as num_paths", []string{"Recursive Union", "Filter"}},
		{"GRAPH FinGraph MATCH ((src:Account)-[t1:Transfers]->(mid:Account))-[t2:Transfers]->(dst:Account) RETURN src.id AS s, mid.id AS m, dst.id AS d", []string{"Cross Apply"}},
		{"GRAPH FinGraph MATCH p = (a:Account)-[t:Transfers]->{1,2}(b:Account) RETURN PATH_LENGTH(p) AS n", []string{"Recursive Union"}},
		{"GRAPH FinGraph MATCH (n:Person) RETURN n.id AS id UNION ALL MATCH (n:Account) RETURN n.id AS id", []string{"Union All"}},
	} {
		sql, got := graphPlan(t, c.gql)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s:\nSQL %s\nplan\n%s\nwant %q", c.gql, sql, got, w)
			}
		}
	}
	// the SQL the counts read: a {0,n} path of no edge stays on its node,
	// and each further edge keeps the edge pattern's own condition
	sql, _, err := graphToSQLQuant("GRAPH FinGraph MATCH (src:Account {id: 7})-[e:Transfers WHERE e.amount > 100]->{0,1}(dst:Account) LET total = SUM(e.amount) RETURN total", finGraphs(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"SELECT cb_e0.id AS cb_src_id, cb_e0.id AS cb_dst_to_id, [] AS cb_arr_amount FROM Account AS cb_e0",
		"[cb_e1.amount] AS cb_arr_amount FROM AccountTransferAccount AS cb_e1 WHERE ((cb_e1.amount > 100))", "(SELECT SUM(cb_x) FROM UNNEST(cb_q0.cb_arr_amount) AS cb_x)"} {
		if !strings.Contains(sql, w) {
			t.Errorf("{0,1}: want %q in\n%s", w, sql)
		}
	}
}
