package spannerfront

import (
	"strings"
	"testing"

	"cloud.google.com/go/spanner/apiv1/spannerpb"
)

// A subquery in the select list of a SELECT that aggregates (#2029): one
// outside the aggregates runs once per group, after the Aggregate; one in
// an aggregate's argument once per input row.
func TestPlanSubqueryInAnAggregatingSelect(t *testing.T) {
	sc := docsSchema(false)
	for _, c := range []struct{ sql, want string }{
		{"SELECT s.SingerId, (SELECT COUNT(*) FROM Albums AS a WHERE a.SingerId = s.SingerId) AS albums, COUNT(*) AS songs FROM Songs AS s GROUP BY s.SingerId", "Scan[Albums]"},
		{"SELECT (SELECT COUNT(*) FROM Singers) AS singers, COUNT(*) AS songs FROM Songs", "Scan[Singers]"},
		{"SELECT SUM((SELECT COUNT(*) FROM Albums AS a WHERE a.SingerId = s.SingerId)) AS n FROM Singers AS s", "Scan[Albums]"},
	} {
		nodes, _ := planFor(t, sc, c.sql)
		if got := relNames(nodes); !strings.Contains(got, "Aggregate") || !strings.Contains(got, c.want) {
			t.Errorf("%s: want an Aggregate and the subquery's %s:\n%s", c.sql, c.want, planTree(nodes)+got)
		}
	}
}

// relNames lists every relational operator, the scalar subqueries' plans
// included.
func relNames(nodes []*spannerpb.PlanNode) string {
	var out []string
	for _, n := range nodes {
		if n.GetKind() == spannerpb.PlanNode_RELATIONAL {
			name := n.GetDisplayName()
			if st := n.GetMetadata().GetFields()["scan_target"].GetStringValue(); st != "" {
				name += "[" + st + "]"
			}
			out = append(out, name)
		}
	}
	return "all: " + strings.Join(out, ", ")
}
