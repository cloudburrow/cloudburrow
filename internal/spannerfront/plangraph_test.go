package spannerfront

import (
	"strings"
	"testing"
)

// musicGraphJSON is PROPERTY_GRAPH_METADATA_JSON as the engine reports it
// for Google's MusicGraph example (Singers and Albums nodes, Released and
// Collabs edges).
const musicGraphJSON = `{"edgeTables":[{"baseTableName":"Collabs","destinationNodeTable":{"edgeTableColumns":["OtherId"],"nodeTableColumns":["SingerId"],"nodeTableName":"Singers"},"keyColumns":["SingerId","OtherId"],"kind":"EDGE","labelNames":["Collabs"],"name":"Collabs","propertyDefinitions":[{"propertyDeclarationName":"SingerId","valueExpressionSql":"SingerId"},{"propertyDeclarationName":"OtherId","valueExpressionSql":"OtherId"}],"sourceNodeTable":{"edgeTableColumns":["SingerId"],"nodeTableColumns":["SingerId"],"nodeTableName":"Singers"}},{"baseTableName":"Albums","destinationNodeTable":{"edgeTableColumns":["SingerId","AlbumId"],"nodeTableColumns":["SingerId","AlbumId"],"nodeTableName":"Albums"},"keyColumns":["SingerId","AlbumId"],"kind":"EDGE","labelNames":["Released"],"name":"Albums","propertyDefinitions":[{"propertyDeclarationName":"SingerId","valueExpressionSql":"SingerId"},{"propertyDeclarationName":"AlbumId","valueExpressionSql":"AlbumId"},{"propertyDeclarationName":"AlbumTitle","valueExpressionSql":"AlbumTitle"}],"sourceNodeTable":{"edgeTableColumns":["SingerId"],"nodeTableColumns":["SingerId"],"nodeTableName":"Singers"}}],"name":"MusicGraph","nodeTables":[{"baseTableName":"Albums","keyColumns":["SingerId","AlbumId"],"kind":"NODE","labelNames":["Albums"],"name":"Albums","propertyDefinitions":[{"propertyDeclarationName":"SingerId","valueExpressionSql":"SingerId"},{"propertyDeclarationName":"AlbumId","valueExpressionSql":"AlbumId"},{"propertyDeclarationName":"AlbumTitle","valueExpressionSql":"AlbumTitle"}]},{"baseTableName":"Singers","keyColumns":["SingerId"],"kind":"NODE","labelNames":["Singers"],"name":"Singers","propertyDefinitions":[{"propertyDeclarationName":"SingerId","valueExpressionSql":"SingerId"},{"propertyDeclarationName":"FirstName","valueExpressionSql":"FirstName"}]}]}`

func musicGraphs(t *testing.T) map[string]*graphMeta {
	g, err := parseGraphMeta(musicGraphJSON)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*graphMeta{"musicgraph": g}
}

func TestGraphToSQL(t *testing.T) {
	for _, c := range []struct{ gql, want string }{
		{"GRAPH MusicGraph MATCH (s:Singers)-[:Released]->(a:Albums) RETURN s.FirstName, a.AlbumTitle",
			"SELECT s.FirstName AS FirstName, a.AlbumTitle AS AlbumTitle FROM Singers AS s, Albums AS cb_g0, Albums AS a WHERE cb_g0.SingerId = s.SingerId AND cb_g0.SingerId = a.SingerId AND cb_g0.AlbumId = a.AlbumId"},
		{"GRAPH MusicGraph MATCH (s:Singers {SingerId: @id}) WHERE s.FirstName IS NOT NULL RETURN s.FirstName AS name ORDER BY name LIMIT 1",
			"SELECT s.FirstName AS name FROM Singers AS s WHERE s.SingerId = (@id) AND (s.FirstName IS NOT NULL) ORDER BY name LIMIT 1"},
		{"GRAPH MusicGraph MATCH (o)<-[c:Collabs]-(s:Singers WHERE s.SingerId = 1) RETURN o.FirstName",
			"SELECT o.FirstName AS FirstName FROM Singers AS o, Collabs AS c, Singers AS s WHERE c.SingerId = s.SingerId AND c.OtherId = o.SingerId AND (s.SingerId = 1)"},
		{"GRAPH MusicGraph MATCH (s:Singers) MATCH (s)-[r:Released]->(a) RETURN s.SingerId, COUNT(*) AS n GROUP BY s.SingerId",
			"SELECT s.SingerId AS SingerId, COUNT(*) AS n FROM Singers AS s, Albums AS r, Albums AS a WHERE r.SingerId = s.SingerId AND r.SingerId = a.SingerId AND r.AlbumId = a.AlbumId GROUP BY s.SingerId"},
		{"SELECT x.FirstName FROM GRAPH_TABLE(MusicGraph MATCH (s:Singers) COLUMNS (s.FirstName)) AS x",
			"SELECT x.FirstName FROM (SELECT s.FirstName AS FirstName FROM Singers AS s) AS x"},
	} {
		got, err := graphToSQL(c.gql, musicGraphs(t))
		if err != nil || got != c.want {
			t.Errorf("%s:\ngot  %s (%v)\nwant %s", c.gql, got, err, c.want)
		}
	}
	for _, gql := range []string{
		"GRAPH MusicGraph MATCH (s:Singers)-[:Collabs]->{0,2}(o:Singers) RETURN o.FirstName",
		"GRAPH MusicGraph MATCH (s:Singers)-[:Collabs]->+(o:Singers) RETURN o.FirstName",
		"GRAPH MusicGraph MATCH (s:Singers)-[c:Collabs]->{1,2}(o:Singers) RETURN c.OtherId",
		"GRAPH MusicGraph MATCH (n) RETURN LABELS(n) AS l",
		"GRAPH MusicGraph MATCH (s:Singers)-[c:Collabs]-(o:Singers) RETURN o.FirstName",
		"GRAPH MusicGraph OPTIONAL MATCH (s:Singers) RETURN s.FirstName",
		"GRAPH MusicGraph MATCH (s:Singers) RETURN s",
	} {
		if _, err := graphToSQL(gql, musicGraphs(t)); err == nil {
			t.Errorf("%s: translated, want it refused by name", gql)
		} else if _, ok := err.(unsupportedSQL); !ok {
			t.Errorf("%s: %v, want it refused by name", gql, err)
		}
	}
	// a quantified edge: the paths of 1 and 2 edges, planned as a
	// Recursive Union of the first edges and a Cross Apply of the spool to
	// the next ones
	sql, quant, err := graphToSQLQuant("GRAPH MusicGraph MATCH (s:Singers {SingerId: 1})-[:Collabs]->{1,2}(o:Singers) RETURN o.FirstName", musicGraphs(t))
	want := "SELECT o.FirstName AS FirstName FROM Singers AS s, (SELECT cb_e1.SingerId AS cb_src_SingerId, cb_e1.OtherId AS cb_dst_OtherId FROM Collabs AS cb_e1 UNION ALL " +
		"SELECT cb_e1.SingerId AS cb_src_SingerId, cb_e2.OtherId AS cb_dst_OtherId FROM Collabs AS cb_e1, Collabs AS cb_e2 WHERE (cb_e2.SingerId = cb_e1.OtherId)) AS cb_q0, Singers AS o " +
		"WHERE cb_q0.cb_src_SingerId = s.SingerId AND cb_q0.cb_dst_OtherId = o.SingerId AND s.SingerId = (1)"
	if err != nil || sql != want || quant["cb_q0"] == nil {
		t.Fatalf("quantified edge:\ngot  %s (%v)\nwant %s", sql, err, want)
	}
	sc := docsSchema(false)
	sc.tables["collabs"] = &planTable{name: "Collabs", cols: []string{"SingerId", "OtherId"}, pk: []keyCol{{name: "SingerId"}, {name: "OtherId"}}}
	ps, q, err := parseQuerySQL(sql, false)
	if err != nil {
		t.Fatal(err)
	}
	b := &planBuilder{ps: ps, schema: sc, ctes: map[string]*sqlCTE{}, cols: map[*sqlExpr]colRef{},
		subPlans: map[*sqlExpr]*planOp{}, orderFor: map[*sqlSelect][]sqlOrder{}, quant: quant}
	root, err := b.buildPlan(q)
	if err != nil {
		t.Fatal(err)
	}
	nodes, _ := planNodes(root)
	if got := planTree(nodes); !strings.Contains(got, "Recursive Union") || !strings.Contains(got, "Base: Distributed Union(Global)") ||
		!strings.Contains(got, "Recursive: Cross Apply") || !strings.Contains(got, "Input: Scan[RecursiveSpoolScan $cb_q0]") {
		t.Errorf("quantified edge plan:\n%s", got)
	}
	// the translation plans as SQL does: a co-located Cross Apply
	sql, err = graphToSQL("GRAPH MusicGraph MATCH (s:Singers {SingerId: 2})-[:Released]->(a:Albums) RETURN a.AlbumTitle", musicGraphs(t))
	if err != nil {
		t.Fatal(err)
	}
	nodes, _ = planFor(t, docsSchema(false), sql)
	if got := planTree(nodes); !strings.Contains(got, "Cross Apply") || !strings.Contains(got, "Seek Condition: $SingerId = (2)") {
		t.Errorf("%s:\n%s", sql, got)
	}
}
