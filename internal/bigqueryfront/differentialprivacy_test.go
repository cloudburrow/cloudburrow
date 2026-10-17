package bigqueryfront

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
)

func mustDPQuery(t *testing.T, sql string) dpQuery {
	t.Helper()
	toks, ok := lex(sql)
	if !ok {
		t.Fatalf("lex %q", sql)
	}
	q, msg, _ := parseDPQuery(sql, toks)
	if msg != "" {
		t.Fatalf("parseDPQuery(%q): %s", sql, msg)
	}
	return q
}

func TestParseDPQuery(t *testing.T) {
	q := mustDPQuery(t, `SELECT WITH DIFFERENTIAL_PRIVACY
    OPTIONS(epsilon=10, delta=.01, max_groups_contributed=2, privacy_unit_column=id)
    item,
    AVG(quantity, contribution_bounds_per_group => (0,100)) AS average_quantity,
    COUNT(*) n, PERCENTILE_CONT(quantity, 0.5, contribution_bounds_per_row => (-1, 1e2))
FROM professors WHERE quantity > 0
GROUP BY item ORDER BY item`)
	o := q.opts
	if o.eps != 10 || o.delta != 0.01 || o.maxGroups != 2 || o.unitName != "id" {
		t.Fatalf("options %+v", o)
	}
	if len(q.items) != 4 || q.items[0].text != "item" || q.items[1].agg.fn != "AVG" || q.items[1].alias != "average_quantity" ||
		!q.items[1].agg.bounded || q.items[1].agg.hi != 100 || !q.items[2].agg.star || q.items[2].alias != "n" ||
		q.items[3].agg.pct != 0.5 || q.items[3].agg.lo != -1 || q.items[3].agg.hi != 100 {
		t.Fatalf("items %+v", q.items)
	}
	if q.group != "item" || q.tail != "ORDER BY item" || q.from != "FROM professors WHERE quantity > 0" {
		t.Fatalf("clauses %q %q %q", q.from, q.group, q.tail)
	}
	inner := q.innerSQL("id")
	want := "SELECT item, AVG(CAST(quantity AS FLOAT64)) AS `__dp_a1`, COUNT(*) AS `__dp_a2`, " +
		"ARRAY_AGG(CAST(quantity AS FLOAT64) IGNORE NULLS) AS `__dp_a3`, id AS `__dp_unit` " +
		"FROM professors WHERE quantity > 0 GROUP BY item, id"
	if inner != want {
		t.Fatalf("inner\n%s\nwant\n%s", inner, want)
	}
	for sql, wantMsg := range map[string]string{
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) item FROM t GROUP BY item":                                                                                                   "must use a differentially private aggregate",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) MAX(x) FROM t":                                                                                                               "Aggregate function MAX is not supported",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=-1, delta=.1, privacy_unit_column=id) COUNT(*) FROM t":                                                                                                            "epsilon option",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) PERCENTILE_CONT(x, 0.5) FROM t":                                                                                              "requires contribution_bounds_per_row",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) SUM(x, contribution_bounds_per_row => (0, 1)) FROM t":                                                                        "takes contribution_bounds_per_group",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) SUM(AVG(x)) FROM t":                                                                                                          "multi-level aggregation",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) * FROM t":                                                                                                                    "cannot select *",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) COUNT(*) FROM t QUALIFY 1=1":                                                                                                 "QUALIFY",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) COUNT(*) FROM t GROUP BY g HAVING MAX(x) > 1":                                                                                "Aggregate function MAX",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) COUNT(DISTINCT x) FROM t":                                                                                                    "COUNT(DISTINCT",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, max_rows_contributed=1, privacy_unit_column=id) COUNT(*) FROM t":                                                                                     "max_rows_contributed",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, group_selection_strategy=SOME, privacy_unit_column=id) COUNT(*) FROM t":                                                                              "LAPLACE_THRESHOLD or PUBLIC_GROUPS",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, group_selection_strategy=PUBLIC_GROUPS, privacy_unit_column=id) g, COUNT(*) FROM t GROUP BY g":                                                       "public group join",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, group_selection_strategy=PUBLIC_GROUPS, privacy_unit_column=id) g, COUNT(*) FROM t JOIN (SELECT DISTINCT g FROM p) USING (g) GROUP BY g":             "public group join",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, group_selection_strategy=PUBLIC_GROUPS, privacy_unit_column=id) g, h, COUNT(*) FROM t RIGHT JOIN (SELECT DISTINCT g FROM p) USING (g) GROUP BY g, h": "none for h",
	} {
		toks, _ := lex(sql)
		_, msg, _ := parseDPQuery(sql, toks)
		if !strings.Contains(msg, wantMsg) {
			t.Errorf("%s: %q, want %q", sql, msg, wantMsg)
		}
	}
}

// professorsRows is innerSQL's rows of the reference's professors table
// for item, AVG(quantity): one per (item, id).
func dpRowsOf(rows [][3]string) [][]any {
	var out [][]any
	for _, r := range rows {
		out = append(out, []any{r[0], r[1], r[2]})
	}
	return out
}

func TestDPComputeWithoutNoise(t *testing.T) {
	q := mustDPQuery(t, `SELECT WITH DIFFERENTIAL_PRIVACY
    OPTIONS(epsilon=1e20, delta=.01, max_groups_contributed=2, privacy_unit_column=id)
    item, AVG(quantity, contribution_bounds_per_group => (0,100)) AS average_quantity
FROM professors GROUP BY item`)
	// The reference's professors: per (item, id) averages.
	rows := dpRowsOf([][3]string{
		{"pencil", "24", "101"}, {"pencil", "24", "123"}, {"pencil", "72", "150"},
		{"pen", "21", "101"}, {"pen", "16", "123"}, {"scissors", "8", "130"},
	})
	out, msg := q.compute(rows)
	if msg != "" {
		t.Fatal(msg)
	}
	got := map[string]float64{}
	for _, r := range out {
		got[r[0].(string)] = r[1].(float64)
	}
	for item, want := range map[string]float64{"pencil": 40, "pen": 18.5, "scissors": 8} {
		if math.Abs(got[item]-want) > 1e-6 {
			t.Errorf("%s: %v, want %v (the reference's)", item, got[item], want)
		}
	}
}

func TestDPNoiseDistribution(t *testing.T) {
	q := mustDPQuery(t, `SELECT WITH DIFFERENTIAL_PRIVACY
    OPTIONS(epsilon=2, delta=1e-5, max_groups_contributed=1, privacy_unit_column=id)
    g, COUNT(*, contribution_bounds_per_group => (0, 1)) c, SUM(x, contribution_bounds_per_group => (0, 10)) s
FROM t GROUP BY g`)
	q.items[2].agg.sumInt = true
	var rows [][]any
	for u := 0; u < 1000; u++ {
		// Each unit has 3 rows (clamped to 1) and a sum of 50 (clamped to 10).
		rows = append(rows, []any{"big", "3", "50", strconv.Itoa(u)})
	}
	rows = append(rows, []any{"small", "1", "1", "lonely"})
	const runs = 400
	var sumC, sqC float64
	distinct := map[int64]bool{}
	for i := 0; i < runs; i++ {
		out, msg := q.compute(rows)
		if msg != "" {
			t.Fatal(msg)
		}
		if len(out) != 1 || out[0][0] != "big" {
			t.Fatalf("groups %v: the one-unit group must be dropped, the 1000-unit one kept", out)
		}
		c := out[0][1].(int64)
		s := out[0][2].(int64)
		if math.Abs(float64(s)-10000) > 2000 {
			t.Fatalf("sum %d far from 10000: the contributions were not clamped", s)
		}
		distinct[c] = true
		sumC += float64(c)
		sqC += float64(c) * float64(c)
	}
	mean := sumC / runs
	sd := math.Sqrt(sqC/runs - mean*mean)
	// Laplace, epsilon 2/3 per aggregate, l0 1, lInf 1: scale 1.5, sd 2.12.
	if math.Abs(mean-1000) > 0.6 {
		t.Errorf("mean count %v, want about 1000", mean)
	}
	if sd < 1.5 || sd > 2.8 {
		t.Errorf("count sd %v, want about 2.12 (Laplace scale 1.5)", sd)
	}
	if len(distinct) < 5 {
		t.Errorf("only %d distinct counts: no noise", len(distinct))
	}
}

func TestDPMaxGroupsContributed(t *testing.T) {
	q := mustDPQuery(t, `SELECT WITH DIFFERENTIAL_PRIVACY
    OPTIONS(epsilon=1e20, delta=.01, max_groups_contributed=1, privacy_unit_column=id)
    g, COUNT(*, contribution_bounds_per_group => (0, 2)) c
FROM t GROUP BY g`)
	var rows [][]any
	for u := 0; u < 30; u++ {
		for _, g := range []string{"a", "b", "c"} {
			rows = append(rows, []any{g, "5", strconv.Itoa(u)})
		}
	}
	out, msg := q.compute(rows)
	if msg != "" {
		t.Fatal(msg)
	}
	total := int64(0)
	for _, r := range out {
		total += r[1].(int64)
	}
	if total != 60 {
		t.Fatalf("total %d, want 60: 30 units, one group each, 5 rows clamped to 2", total)
	}
}

func TestDPImplicitBounds(t *testing.T) {
	q := mustDPQuery(t, `SELECT WITH DIFFERENTIAL_PRIVACY
    OPTIONS(epsilon=1e20, delta=.01, privacy_unit_column=id) AVG(x) a
FROM t`)
	var rows [][]any
	for u := 0; u < 50; u++ {
		rows = append(rows, []any{strconv.Itoa(500 + u%7), strconv.Itoa(u)})
	}
	out, msg := q.compute(rows)
	if msg != "" || len(out) != 1 {
		t.Fatal(msg, out)
	}
	if a := out[0][0].(float64); math.Abs(a-503) > 1 {
		t.Fatalf("avg %v, want about 503", a)
	}
	// Too few units: implicit bounding finds none, and no rows.
	q2 := mustDPQuery(t, `SELECT WITH DIFFERENTIAL_PRIVACY
    OPTIONS(epsilon=1, delta=.01, privacy_unit_column=id) AVG(x) a FROM t`)
	if out, _ := q2.compute([][]any{{"5", "u1"}, {"6", "u2"}}); len(out) != 0 {
		t.Fatalf("got %v, want no rows", out)
	}
}

func TestParseDPPolicyAndBudget(t *testing.T) {
	pol, why := parsePrivacyPolicy(`{"differential_privacy_policy": {"privacy_unit_column": "last_name", "max_epsilon_per_query": 10.01, "epsilon_budget": 25.6, "delta_per_query": 0.01, "delta_budget": 0.1, "max_groups_contributed": 2}}`)
	if why != "" || !pol.dp {
		t.Fatal(why)
	}
	p := pol.dpPolicy
	if p.unit != "last_name" || p.maxEps != 10.01 || p.epsBudget != 25.6 || p.deltaPerQuery != 0.01 || p.maxGroups != 2 {
		t.Fatalf("%+v", p)
	}
	s := &dpBudgetStore{spent: map[string][2]float64{}}
	if !s.charge("k", p, 10, 0.01) || !s.charge("k", p, 10, 0.01) || s.charge("k", p, 10, 0.01) {
		t.Fatal("the third query of epsilon 10 must pass the budget of 25.6")
	}
	for _, bad := range []string{
		`{"differential_privacy_policy": {"max_epsilon_per_query": 1, "delta_per_query": 0.1}}`,
		`{"differential_privacy_policy": {"privacy_unit_column": "u", "max_epsilon_per_query": 10, "epsilon_budget": 5, "delta_per_query": 0.1}}`,
		`{"differential_privacy_policy": {"privacy_unit_column": "u", "max_epsilon_per_query": 10, "delta_per_query": 2}}`,
	} {
		if _, why := parsePrivacyPolicy(bad); why == "" {
			t.Errorf("%s: accepted", bad)
		}
	}
	b, _ := json.Marshal(s.spent)
	if !strings.Contains(string(b), "20") {
		t.Fatalf("spent %s", b)
	}
}

func TestDPHaving(t *testing.T) {
	q := mustDPQuery(t, `SELECT WITH DIFFERENTIAL_PRIVACY
    OPTIONS(epsilon=1e20, delta=.01, max_groups_contributed=2, privacy_unit_column=id)
    item, COUNT(*, contribution_bounds_per_group => (0, 100)) AS n
FROM professors GROUP BY item HAVING count(*, contribution_bounds_per_group => (0, 100)) > 1 AND item != 'x'
  AND SUM(quantity, contribution_bounds_per_group => (0, 100)) > 50 ORDER BY n`)
	if q.group != "item" || q.tail != "ORDER BY n" || len(q.items) != 3 || !q.items[2].hidden || q.items[2].agg.fn != "SUM" {
		t.Fatalf("%q %q %+v", q.group, q.tail, q.items)
	}
	inner := q.innerSQL("id")
	if !strings.Contains(inner, "SUM(quantity) AS `__dp_a2`") || !strings.HasSuffix(inner, "GROUP BY item, id") {
		t.Fatalf("inner %s", inner)
	}
	rows := [][]any{{"pen", "2", "39", "101"}, {"pen", "1", "16", "123"}, {"pencil", "1", "24", "101"},
		{"pencil", "2", "48", "123"}, {"pencil", "1", "72", "150"}, {"scissors", "1", "8", "130"}}
	out, msg := q.compute(rows)
	if msg != "" || len(out) != 3 {
		t.Fatal(msg, out)
	}
	schema := []field{{Name: "item", Type: "STRING"}, {Name: "__dp_a1", Type: "INTEGER"}, {Name: "__dp_a2", Type: "INTEGER"}, {Name: "__dp_unit", Type: "INTEGER"}}
	q.items[2].agg.sumInt = true
	sql, msg := q.resultSQL(schema, out)
	if msg != "" {
		t.Fatal(msg)
	}
	for _, want := range []string{"SELECT `__dp_c0` AS `item`, `__dp_c1` AS `n` FROM UNNEST(",
		" WHERE `__dp_c1` > 1 AND `__dp_c0` != 'x'\n  AND `__dp_c2` > 50 ORDER BY n"} {
		if !strings.Contains(sql, want) {
			t.Errorf("result\n%s\nwant %s", sql, want)
		}
	}
}

func TestDPPublicGroups(t *testing.T) {
	q := mustDPQuery(t, `SELECT WITH DIFFERENTIAL_PRIVACY
    OPTIONS(epsilon=1e20, delta=.01, max_groups_contributed=1, group_selection_strategy=PUBLIC_GROUPS, privacy_unit_column=id)
    item, COUNT(*, contribution_bounds_per_group => (0, 10)) AS n
FROM professors RIGHT OUTER JOIN (SELECT DISTINCT item FROM UNNEST(['pen', 'pencil', 'book']) AS item) USING (item)
GROUP BY item`)
	if !q.opts.public {
		t.Fatal("PUBLIC_GROUPS not read")
	}
	// One unit in pen: kept with no thresholding; book has no unit (the
	// outer join's NULL) and is kept too.
	out, msg := q.compute([][]any{{"pen", "3", "101"}, {"book", "1", nil}})
	if msg != "" || len(out) != 2 {
		t.Fatal(msg, out)
	}
	got := map[string]int64{}
	for _, r := range out {
		got[r[0].(string)] = r[1].(int64)
	}
	if got["pen"] != 3 || got["book"] != 0 {
		t.Fatalf("%v, want pen 3, book 0", got)
	}
	// LEFT JOIN with the public side first, joined ON.
	mustDPQuery(t, `SELECT WITH DIFFERENTIAL_PRIVACY
    OPTIONS(epsilon=1, delta=.01, group_selection_strategy=PUBLIC_GROUPS, privacy_unit_column=id)
    p.item, COUNT(*) FROM (SELECT DISTINCT item FROM pub) AS p LEFT JOIN professors ON p.item = professors.item
GROUP BY p.item`)
}
