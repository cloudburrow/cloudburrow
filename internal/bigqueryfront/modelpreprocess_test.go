package bigqueryfront

import (
	"strings"
	"testing"
)

func TestRewritePreprocess(t *testing.T) {
	got, code, msg := rewritePreprocess("SELECT ML.POLYNOMIAL_EXPAND(STRUCT(a, 2 * b AS bb)) AS p FROM t", nil)
	if code != 0 {
		t.Fatalf("%d %s", code, msg)
	}
	for _, want := range []string{"AS `a`", "AS `bb`", "AS `a_a`", "AS `a_bb`", "AS `bb_bb`"} {
		if !strings.Contains(got, want) {
			t.Errorf("POLYNOMIAL_EXPAND: %s lacks %s", got, want)
		}
	}
	if strings.Index(got, "AS `bb`") > strings.Index(got, "AS `a_a`") {
		t.Errorf("POLYNOMIAL_EXPAND: degree 1 first: %s", got)
	}
	if got, _, _ := rewritePreprocess("SELECT ML.STANDARD_SCALER(x) OVER (PARTITION BY g) FROM t", nil); !strings.Contains(got, "STDDEV_SAMP(CAST((x) AS FLOAT64)) OVER (PARTITION BY g)") {
		t.Errorf("STANDARD_SCALER: %s", got)
	}
	// Nested, and with the statistics learned.
	learned := 0
	got, code, _ = rewritePreprocess("ML.MIN_MAX_SCALER(ML.IMPUTER(x, 'mean') OVER ()) OVER () AS m", func(expr string) (mlStats, int, string) {
		learned++
		one, five := 1.0, 5.0
		return mlStats{mean: &five, min: &one, max: &five}, 0, ""
	})
	if code != 0 || learned != 2 || strings.Contains(got, "OVER") || !strings.Contains(got, "4.0") {
		t.Errorf("learned: %d %d %s", code, learned, got)
	}
	if !strings.HasSuffix(got, " AS m") {
		t.Errorf("learned: %s", got)
	}
	for sql, want := range map[string]int{
		"SELECT ML.QUANTILE_BUCKETIZE(x, 4) OVER () FROM t": 501,
		"SELECT ML.STANDARD_SCALER(x) FROM t":               400,
		"SELECT ML.BUCKETIZE(x, [1.0]) OVER () FROM t":      400,
		"SELECT ML.FEATURE_CROSS(STRUCT(a AS x), 5) FROM t": 400,
		"SELECT ML.POLYNOMIAL_EXPAND(s) FROM t":             501,
	} {
		if _, code, _ := rewritePreprocess(sql, nil); code != want {
			t.Errorf("%s: %d; want %d", sql, code, want)
		}
	}
	for _, sql := range []string{"SELECT * FROM ML.PREDICT(MODEL m, TABLE t)", "SELECT ML.my_udf(x) FROM t"} {
		if got, code, _ := rewritePreprocess(sql, nil); got != sql || code != 0 {
			t.Errorf("rewritten: %s, %d", got, code)
		}
	}
	if c := combinations(3, 2, false); len(c) != 3 {
		t.Errorf("combinations: %v", c)
	}
}
