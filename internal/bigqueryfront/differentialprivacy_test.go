package bigqueryfront

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/google/differential-privacy/go/v4/noise"
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
	// report_format, which GoogleSQL documents for AVG, COUNT and SUM but
	// BigQuery's reference does not, is unsupported (501), not invalid; on
	// PERCENTILE_CONT, which neither documents it for, it is invalid (#1589).
	for sql, want501 := range map[string]bool{
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) AVG(x, report_format => 'JSON') FROM t":                                             true,
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) COUNT(*, contribution_bounds_per_group => (0, 1), report_format => 'PROTO') FROM t": true,
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) SUM(x, report_format => 'JSON') + 1 AS s FROM t":                                    true,
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) PERCENTILE_CONT(x, 0.5, report_format => 'JSON') FROM t":                            false,
	} {
		toks, _ := lex(sql)
		_, msg, unsup := parseDPQuery(sql, toks)
		if msg == "" || unsup != want501 || want501 && !strings.Contains(msg, "report_format") {
			t.Errorf("%s: %q unsupported=%v, want unsupported=%v", sql, msg, unsup, want501)
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

// seededLaplace is Laplace noise from a seeded source, for a deterministic
// test: an int64 is rounded continuous Laplace, as the library's is (its
// granularity, about 2^-40 of the scale here, aside), of scale
// l0*lInf/epsilon. The threshold is the library's.
type seededLaplace struct {
	noise.Noise
	r *rand.Rand
}

func (s seededLaplace) sample(l0 int64, lInf, eps float64) float64 {
	return float64(l0) * lInf / eps * (s.r.ExpFloat64() - s.r.ExpFloat64())
}

func (s seededLaplace) AddNoiseInt64(x, l0, lInf int64, eps, delta float64) (int64, error) {
	if _, err := s.Noise.AddNoiseInt64(x, l0, lInf, eps, delta); err != nil {
		return 0, err
	}
	return x + int64(math.Round(s.sample(l0, float64(lInf), eps))), nil
}

func (s seededLaplace) AddNoiseFloat64(x float64, l0 int64, lInf, eps, delta float64) (float64, error) {
	if _, err := s.Noise.AddNoiseFloat64(x, l0, lInf, eps, delta); err != nil {
		return 0, err
	}
	return x + s.sample(l0, lInf, eps), nil
}

// TestDPNoiseDistribution checks the group selection, the clamping and the
// noise's centre and scale over many runs, with a seeded noise (so the test
// is deterministic) and with the library's own. Each bound is set so that
// correct noise breaks it with probability under 1e-11 (computed below), so
// under 1e-9 for the whole test, whatever the seed.
func TestDPNoiseDistribution(t *testing.T) {
	for _, src := range []struct {
		name    string
		laplace noise.Noise
	}{
		{"seeded", seededLaplace{noise.Laplace(), rand.New(rand.NewPCG(2213, 1))}},
		{"library", nil},
	} {
		t.Run(src.name, func(t *testing.T) { dpNoiseDistribution(t, src.laplace) })
	}
}

func dpNoiseDistribution(t *testing.T, laplace noise.Noise) {
	q := mustDPQuery(t, `SELECT WITH DIFFERENTIAL_PRIVACY
    OPTIONS(epsilon=2, delta=1e-5, max_groups_contributed=1, privacy_unit_column=id)
    g, COUNT(*, contribution_bounds_per_group => (0, 1)) c, SUM(x, contribution_bounds_per_group => (0, 10)) s
FROM t GROUP BY g`)
	q.items[2].agg.sumInt = true
	q.laplace = laplace
	const units = 100
	var rows [][]any
	for u := 0; u < units; u++ {
		// Each unit has 3 rows (clamped to 1) and a sum of 50 (clamped to 10).
		rows = append(rows, []any{"big", "3", "50", strconv.Itoa(u)})
	}
	rows = append(rows, []any{"small", "1", "1", "lonely"})
	// Epsilon 2/3 for each of the selection, COUNT and SUM; l0 1. The
	// noise X of a count is round(Laplace(1.5)): variance 4.58, and
	// P(|X| >= 2) = P(|Laplace(1.5)| >= 1.5) = 1/e.
	const runs = 2000
	var sumC float64
	smallKept, wide := 0, 0
	for i := 0; i < runs; i++ {
		out, msg := q.compute(rows)
		if msg != "" {
			t.Fatal(msg)
		}
		// The selection's threshold is ceil(17.23) = 18 (delta 1e-5), so
		// the 100-unit group is dropped with probability
		// 2000 * exp(-82.5/1.5) / 2 < 1e-21 over the runs.
		var big dpOut
		for _, r := range out {
			switch r[0] {
			case "big":
				big = r
			case "small":
				smallKept++
			default:
				t.Fatalf("groups %v", out)
			}
		}
		if big == nil {
			t.Fatalf("groups %v: the %d-unit group must be kept", out, units)
		}
		c := big[1].(int64)
		s := big[2].(int64)
		// SUM's noise is round(Laplace(15)) (lInf 10): beyond 500 with
		// probability 2000 * exp(-499.5/15) < 7e-12 over the runs. Without
		// the clamping, the sum would be 5000.
		if math.Abs(float64(s)-10*units) > 500 {
			t.Fatalf("sum %d far from %d: the contributions were not clamped", s, 10*units)
		}
		if math.Abs(float64(c)-units) >= 2 {
			wide++
		}
		sumC += float64(c)
	}
	// The one-unit group passes the threshold with probability
	// P(1 + X >= 18) = exp(-16.5/1.5) / 2 = 8.4e-6 a run: 5 or more times
	// in 2000 runs with probability C(2000,5) p^5 < 1.1e-11. Without the
	// threshold it would be kept every time.
	if smallKept >= 5 {
		t.Errorf("the one-unit group was kept in %d of %d runs; want it dropped (probability 8.4e-6 a run)", smallKept, runs)
	}
	// Chernoff, with X's moment generating function: the mean of 2000
	// is more than 0.4 from 0 with probability < 2e-15.
	if mean := sumC / runs; math.Abs(mean-units) > 0.4 {
		t.Errorf("mean count %v, want %d (unbiased noise)", mean, units)
	}
	// |X| >= 2 in Binomial(2000, 1/e) runs, mean 736: outside [591, 884]
	// with probability < 9e-12 (exact binomial tails). The range holds a
	// Laplace scale between 1.23 and 1.83, so neither epsilon unsplit
	// (scale 0.5) nor counted twice (3) passes, nor no noise.
	if wide < 591 || wide > 884 {
		t.Errorf("|count - %d| >= 2 in %d of %d runs, want 591..884 (Laplace scale 1.5: about 736)", units, wide, runs)
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

// An expression over DP aggregates (#1517, UNVERIFIED): each call a hidden
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/analysis-rules
// aggregate with its share of the epsilon, the expression evaluated on the
// noisy values.
func TestDPExpressionItem(t *testing.T) {
	q := mustDPQuery(t, `SELECT WITH DIFFERENTIAL_PRIVACY
    OPTIONS(epsilon=1e20, delta=.01, max_groups_contributed=2, privacy_unit_column=id)
    item, ROUND(SUM(quantity, contribution_bounds_per_group => (0, 100)) / 2, 1) AS half,
    COUNT(*, contribution_bounds_per_group => (0, 100)) + 1 plus
FROM professors GROUP BY item HAVING half > 10 ORDER BY item`)
	if len(q.items) != 5 || !q.items[1].isExpr() || q.items[1].alias != "half" || !q.items[3].isExpr() ||
		q.items[3].alias != "plus" || !q.items[2].hidden || q.items[2].agg.fn != "SUM" || !q.items[4].hidden ||
		q.items[4].agg.fn != "COUNT" || q.items[0].text != "item" {
		t.Fatalf("items %+v", q.items)
	}
	inner := q.innerSQL("id")
	if !strings.Contains(inner, "CAST(NULL AS INT64) AS `__dp_x1`, SUM(quantity) AS `__dp_a2`") ||
		!strings.HasSuffix(inner, "GROUP BY item, id") {
		t.Fatalf("inner %s", inner)
	}
	rows := [][]any{{"pen", nil, "39", nil, "2", "101"}, {"pen", nil, "16", nil, "1", "123"},
		{"pencil", nil, "24", nil, "1", "101"}, {"pencil", nil, "48", nil, "2", "123"}}
	out, msg := q.compute(rows)
	if msg != "" || len(out) != 2 {
		t.Fatal(msg, out)
	}
	schema := []field{{Name: "item", Type: "STRING"}, {Name: "__dp_x1", Type: "INTEGER"}, {Name: "__dp_a2", Type: "INTEGER"},
		{Name: "__dp_x3", Type: "INTEGER"}, {Name: "__dp_a4", Type: "INTEGER"}, {Name: "__dp_unit", Type: "INTEGER"}}
	q.items[2].agg.sumInt = true
	sql, msg := q.resultSQL(schema, out)
	if msg != "" {
		t.Fatal(msg)
	}
	for _, want := range []string{"SELECT `__dp_c0` AS `item`, (ROUND(`__dp_c2` / 2, 1)) AS `half`, (`__dp_c4` + 1) AS `plus` FROM UNNEST(",
		" WHERE (ROUND(`__dp_c2` / 2, 1)) > 10 ORDER BY item"} {
		if !strings.Contains(sql, want) {
			t.Errorf("result\n%s\nwant %s", sql, want)
		}
	}
	for sql, wantMsg := range map[string]string{
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) SUM(x) + MAX(x) FROM t":   "Aggregate function MAX",
		"SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.1, privacy_unit_column=id) 1 + SUM(COUNT(*)) FROM t": "multi-level aggregation",
	} {
		toks, _ := lex(sql)
		if _, msg, _ := parseDPQuery(sql, toks); !strings.Contains(msg, wantMsg) {
			t.Errorf("%s: %q, want %q", sql, msg, wantMsg)
		}
	}
}
