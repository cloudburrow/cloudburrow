package bigqueryfront

import "testing"

func TestReadsTable(t *testing.T) {
	for q, want := range map[string]bool{
		"SELECT 1": false,
		"SELECT EXTRACT(YEAR FROM CURRENT_DATE())":           false,
		"SELECT 1 IS DISTINCT FROM 2":                        false,
		"SELECT * FROM (SELECT 10 AS x UNION ALL SELECT 20)": false,
		"SELECT * FROM UNNEST([1, 2]) AS x":                  false,
		"SELECT (SELECT COUNT(*) FROM ds.t)":                 true,
		"SELECT * FROM (SELECT 1), ds.t":                     true,
		"SELECT * FROM (SELECT 1 AS a) JOIN ds.t USING (a)":  true,
		"SELECT * FROM UNNEST([1]) CROSS JOIN UNNEST([2])":   false,
	} {
		toks, _ := lex(q)
		if got := readsTable(toks); got != want {
			t.Errorf("%s: %v, want %v", q, got, want)
		}
	}
}
