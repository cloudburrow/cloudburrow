package bigqueryfront

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestParseVectorIndexStatement(t *testing.T) {
	for sql, want := range map[string]string{
		"CREATE VECTOR INDEX i ON ds.t(e) OPTIONS(index_type = 'IVF')":                                   "CREATE i ds.t e",
		"CREATE OR REPLACE VECTOR INDEX i ON `p.ds.t`(e) STORING(a, b) OPTIONS(index_type = 'TREE_AH');": "CREATE i p.ds.t e a,b replace",
		"CREATE VECTOR INDEX IF NOT EXISTS i ON ds.t(e) OPTIONS(index_type = 'IVF')":                     "CREATE i ds.t e if",
		"DROP VECTOR INDEX IF EXISTS i ON ds.t":                                                          "DROP i ds.t  if",
		"ALTER VECTOR INDEX i ON ds.t REBUILD":                                                           "ALTER i ds.t ",
	} {
		st, code, msg, ok := parseVectorIndexStatement(sql)
		if !ok || code != 0 {
			t.Errorf("%s: %d %s", sql, code, msg)
			continue
		}
		got := st.kind + " " + st.name + " " + strings.Join(st.table, ".") + " " + st.column
		if len(st.stored) > 0 {
			got += " " + strings.Join(st.stored, ",")
		}
		if st.replace {
			got += " replace"
		}
		if st.ifClause {
			got += " if"
		}
		if strings.TrimSpace(got) != strings.TrimSpace(want) {
			t.Errorf("%s: %q, want %q", sql, got, want)
		}
	}
	for sql, want := range map[string]int{
		"CREATE OR REPLACE VECTOR INDEX IF NOT EXISTS i ON ds.t(e) OPTIONS(index_type = 'IVF')": 400,
		"CREATE VECTOR INDEX i ON ds.t(e, f) OPTIONS(index_type = 'IVF')":                       400,
		"CREATE VECTOR INDEX i ON ds.t(e)":                                                      400,
		"ALTER VECTOR INDEX i ON ds.t SET OPTIONS (x = 1)":                                      400,
		"CREATE VECTOR INDEX i ON ds.t(e) PARTITION BY DATE(d) OPTIONS(index_type = 'TREE_AH')": 501,
		"CREATE VECTOR INDEX i ON ds.t(e) OPTIONS(index_type = 'IVF'); SELECT 1":                501,
	} {
		if _, code, msg, ok := parseVectorIndexStatement(sql); !ok || code != want {
			t.Errorf("%s: %v %d %s, want %d", sql, ok, code, msg, want)
		}
	}
	for _, sql := range []string{"SELECT 'CREATE VECTOR INDEX' AS s", "SELECT * FROM ds.INFORMATION_SCHEMA.VECTOR_INDEXES", "CREATE TABLE ds.t (x INT64)"} {
		if _, _, _, ok := parseVectorIndexStatement(sql); ok {
			t.Errorf("%s: taken for a vector index statement", sql)
		}
	}
}

func TestKMeans(t *testing.T) {
	// Three tight clusters: each is one list.
	var vecs [][]float64
	for c := 0; c < 3; c++ {
		for i := 0; i < 50; i++ {
			vecs = append(vecs, []float64{float64(c*10) + float64(i%5)*0.01, float64(-c*10) + float64(i%7)*0.01})
		}
	}
	cs := kMeans(vecs, 3, 10)
	if len(cs) != 3 {
		t.Fatalf("%d centroids", len(cs))
	}
	lists := map[int]map[int]bool{}
	for i, v := range vecs {
		c := i / 50
		if lists[c] == nil {
			lists[c] = map[int]bool{}
		}
		lists[c][nearestCentroid(cs, v)] = true
	}
	seen := map[int]bool{}
	for c, l := range lists {
		if len(l) != 1 {
			t.Errorf("cluster %d split over lists %v", c, l)
		}
		for k := range l {
			if seen[k] {
				t.Errorf("two clusters share list %d", k)
			}
			seen[k] = true
		}
	}
	again := kMeans(vecs, 3, 10)
	for i := range cs {
		if math.Abs(cs[i][0]-again[i][0]) > 1e-12 {
			t.Errorf("kMeans is not deterministic")
		}
	}
}

func TestMergeVectorStats(t *testing.T) {
	used := vectorIndexUse{index: &vectorIndex{}}
	unused := vectorIndexUse{code: "BASE_TABLE_TOO_SMALL", message: "small", table: tableRef{ProjectID: "p", DatasetID: "d", TableID: "t"}}
	for want, all := range map[string][]map[string]any{
		"FULLY_USED":     {used.stats()},
		"UNUSED":         {unused.stats()},
		"PARTIALLY_USED": {used.stats(), unused.stats()},
	} {
		var got struct {
			Mode    string `json:"indexUsageMode"`
			Reasons []struct {
				Code string `json:"code"`
			} `json:"indexUnusedReasons"`
		}
		if err := json.Unmarshal(mergeVectorStats(all), &got); err != nil || got.Mode != want {
			t.Errorf("%s: %+v %v", want, got, err)
		}
		if want != "FULLY_USED" && (len(got.Reasons) != 1 || got.Reasons[0].Code != "BASE_TABLE_TOO_SMALL") {
			t.Errorf("%s: reasons %+v", want, got.Reasons)
		}
	}
	if mergeVectorStats(nil) != nil {
		t.Errorf("no search: stats")
	}
}

func TestLexicalTerms(t *testing.T) {
	if got := strings.Join(lexicalTerms("Striped HUNTER, striped-cat's"), " "); got != "striped hunter cat s" {
		t.Errorf("lexicalTerms: %q", got)
	}
}

func TestVectorIndexDDL(t *testing.T) {
	x := &vectorIndex{Name: "i", Column: "e", Stored: []string{"a"}, Options: []vectorIndexOption{
		{"index_type", "STRING", `"IVF"`}, {"distance_type", "STRING", `"COSINE"`}, {"ivf_options", "STRING", `"{\"num_lists\": 20}"`}}}
	want := "CREATE VECTOR INDEX `i` ON `p.d.t`(e)\nSTORING(a)\nOPTIONS (distance_type = 'COSINE', index_type = 'IVF', ivf_options = '{\"num_lists\": 20}')"
	if got := vectorIndexDDL(x, "p", "d", "t"); got != want {
		t.Errorf("DDL:\n%s\nwant\n%s", got, want)
	}
}
