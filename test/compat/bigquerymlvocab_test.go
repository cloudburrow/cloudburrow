//go:build compat

package compat

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// indexValue is a STRUCT<index INT64, value FLOAT64> as mlQuery reads it.
func indexValue(t *testing.T, e bigquery.Value) (int64, float64) {
	t.Helper()
	m, ok := e.(map[string]bigquery.Value)
	if !ok {
		t.Fatalf("not a STRUCT(index, value): %#v", e)
	}
	i, iok := m["index"].(int64)
	v, vok := m["value"].(float64)
	if !iok || !vok || len(m) != 2 {
		t.Fatalf("not a STRUCT(index INT64, value FLOAT64): %#v", m)
	}
	return i, v
}

// encoded is an ARRAY<STRUCT<index INT64, value FLOAT64>> as
// "index:value" pairs, values to 6 decimal places.
func encoded(t *testing.T, v bigquery.Value) string {
	t.Helper()
	arr, ok := v.([]bigquery.Value)
	if !ok {
		t.Fatalf("not an array: %#v", v)
	}
	var out []string
	for _, e := range arr {
		i, f := indexValue(t, e)
		out = append(out, fmt.Sprintf("%d:%.6f", i, f))
	}
	return strings.Join(out, " ")
}

// TestBigQueryMLVocabularyPreprocess (#1548): ML.LABEL_ENCODER,
// ML.ONE_HOT_ENCODER, ML.MULTI_HOT_ENCODER, ML.BAG_OF_WORDS and ML.TF_IDF
// as BigQuery documents them, on its own examples
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-label-encoder,
// .../bigqueryml-syntax-one-hot-encoder, -multi-hot-encoder, -bag-of-words,
// -tf-idf): the vocabulary of the window's top_k categories in at least
// frequency_threshold rows, ties alphabetical, indexed alphabetically
// from 1, 0 the unknown; and in a TRANSFORM, the vocabulary learned over
// the training rows and kept for prediction.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryMLVocabularyPreprocess(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	letters := "UNNEST([NULL, 'a', 'b', 'b', 'c', 'c', 'c', 'd', 'd']) AS f"

	got := mlQuery(t, h, c, "SELECT f, ML.LABEL_ENCODER(f, 2, 2) OVER () AS output FROM "+letters+" ORDER BY f")
	var labels []string
	for _, g := range got {
		labels = append(labels, fmt.Sprint(g["output"]))
	}
	if s := strings.Join(labels, " "); s != "0 0 1 1 2 2 2 0 0" {
		t.Errorf("ML.LABEL_ENCODER(f, 2, 2): %s; want 0 0 1 1 2 2 2 0 0", s)
	}

	got = mlQuery(t, h, c, "SELECT f, ML.ONE_HOT_ENCODER(f, 'most_frequent', 10, 0) OVER () AS output FROM "+letters+" ORDER BY f")
	var hot []string
	for _, g := range got {
		hot = append(hot, encoded(t, g["output"]))
	}
	if s := strings.Join(hot, ", "); s != "0:1.000000, 1:1.000000, 2:1.000000, 2:1.000000, 3:0.000000, 3:0.000000, 3:0.000000, 4:1.000000, 4:1.000000" {
		t.Errorf("ML.ONE_HOT_ENCODER(f, 'most_frequent', 10, 0): %s", s)
	}
	got = mlQuery(t, h, c, "SELECT f, ML.ONE_HOT_ENCODER(f) OVER (PARTITION BY f IN ('b', 'c')) AS output FROM UNNEST(['a', 'b', 'b', 'b', 'b', 'b', 'c']) AS f ORDER BY f")
	if s := encoded(t, got[0]["output"]) + ", " + encoded(t, got[1]["output"]) + ", " + encoded(t, got[6]["output"]); s != "0:1.000000, 1:1.000000, 0:1.000000" {
		t.Errorf("ML.ONE_HOT_ENCODER(f) of the defaults over partitions: %s; want 0:1, 1:1, 0:1 (only b is in 5 rows)", s)
	}

	got = mlQuery(t, h, c, "SELECT f[OFFSET(0)] AS f0, ML.MULTI_HOT_ENCODER(f, 3, 1) OVER () AS output FROM "+
		"(SELECT ['a', 'b', 'b', 'c', NULL] AS f UNION ALL SELECT ['c', 'c', 'd', 'd', NULL] AS f) ORDER BY f[OFFSET(0)]")
	if len(got) != 2 || encoded(t, got[0]["output"]) != "1:1.000000 2:1.000000 3:1.000000 0:1.000000" || encoded(t, got[1]["output"]) != "3:1.000000 0:1.000000" {
		t.Errorf("ML.MULTI_HOT_ENCODER(f, 3, 1): %v", got)
	}

	for _, ex := range []struct{ docs, args, want string }{
		{"SELECT 1 AS id, ['a', 'b', 'b', 'c'] AS f UNION ALL SELECT 2 AS id, ['a', 'c'] AS f", "32, 1",
			"1:1.000000 2:2.000000 3:1.000000 | 1:1.000000 3:1.000000"},
		{"SELECT 1 AS id, ['a', 'b', 'b', 'b', 'c', 'c', 'c', 'c', 'd', 'd'] AS f UNION ALL SELECT 2 AS id, ['a', 'c', NULL] AS f", "4, 2",
			"0:5.000000 1:1.000000 2:4.000000 | 0:1.000000 1:1.000000 2:1.000000"},
		{"SELECT 1 AS id, ['a', 'b', 'b', 'c'] AS f UNION ALL SELECT 2 AS id, ['a', 'c', 'c'] AS f", "2, 1",
			"0:2.000000 1:1.000000 2:1.000000 | 1:1.000000 2:2.000000"},
		{"SELECT 1 AS id, ['a', 'b', 'b', 'c', 'd', 'd', 'd'] as f UNION ALL SELECT 2 AS id, ['a', 'c', 'c', 'd', 'd', 'd'] as f", "2, 2",
			"0:5.000000 1:1.000000 2:1.000000 | 0:3.000000 1:1.000000 2:2.000000"},
	} {
		got = mlQuery(t, h, c, "WITH ExampleTable AS ("+ex.docs+") SELECT id, ML.BAG_OF_WORDS(f, "+ex.args+") OVER () AS results FROM ExampleTable ORDER BY id")
		if len(got) != 2 {
			t.Fatalf("ML.BAG_OF_WORDS rows: %v", got)
		}
		if s := encoded(t, got[0]["results"]) + " | " + encoded(t, got[1]["results"]); s != ex.want {
			t.Errorf("ML.BAG_OF_WORDS(f, %s) of %s:\n got %s\nwant %s", ex.args, ex.docs, s, ex.want)
		}
	}

	got = mlQuery(t, h, c, "WITH ExampleTable AS (SELECT 1 AS id, ['I', 'like', 'pie', 'pie', 'pie', NULL] AS f "+
		"UNION ALL SELECT 2 AS id, ['yum', 'yum', 'pie', NULL] AS f UNION ALL SELECT 3 AS id, ['I', 'yum', 'pie', NULL] AS f "+
		"UNION ALL SELECT 4 AS id, ['you', 'like', 'pie', NULL] AS f) SELECT id, ML.TF_IDF(f, 3, 1) OVER () AS results FROM ExampleTable ORDER BY id")
	wantTF := [][]float64{
		{0, 0.12679902142647365, 1, 0.1412163100645339, 2, 0.1412163100645339, 3, 0.29389333245105953},
		{0, 0.5705955964191315, 3, 0.14694666622552977},
		{0, 0.380397064279421, 1, 0.21182446509680086, 3, 0.14694666622552977},
		{0, 0.380397064279421, 2, 0.21182446509680086, 3, 0.14694666622552977},
	}
	if len(got) != 4 {
		t.Fatalf("ML.TF_IDF rows: %v", got)
	}
	for i, g := range got {
		arr, _ := g["results"].([]bigquery.Value)
		ok := len(arr)*2 == len(wantTF[i])
		for j := 0; ok && j < len(arr); j++ {
			idx, val := indexValue(t, arr[j])
			ok = idx == int64(wantTF[i][2*j]) && math.Abs(val-wantTF[i][2*j+1]) < 1e-12
		}
		if !ok {
			t.Errorf("ML.TF_IDF(f, 3, 1) of document %d: %v; want %v", i+1, arr, wantTF[i])
		}
	}

	// In a TRANSFORM: the vocabulary of the training rows (a in 5 rows, b in
	// 6, c in 1, below the default threshold) is kept: at prediction b is
	// still 2 and c unknown, whatever the rows predicted. y = 2 * index - 1.
	name := mlDataset(t, h, c, map[string]string{
		"v": "SELECT c, CAST(2 * IF(c = 'a', 1, IF(c = 'b', 2, 0)) - 1 AS FLOAT64) AS y FROM " +
			"UNNEST(['a', 'a', 'a', 'a', 'a', 'b', 'b', 'b', 'b', 'b', 'b', 'c']) AS c",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.le` TRANSFORM(ML.LABEL_ENCODER(c) OVER () AS ce, y) "+
		"OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT c, y FROM `%s.v`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL with ML.LABEL_ENCODER: %v", err)
	}
	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT c, predicted_y FROM ML.PREDICT(MODEL `%s.le`, (SELECT 'b' AS c UNION ALL SELECT 'c')) ORDER BY c", name))
	if len(pred) != 2 || !near(pred[0]["predicted_y"], 3, 1e-6) || !near(pred[1]["predicted_y"], -1, 1e-6) {
		t.Errorf("ML.PREDICT label-encoded: %v; want b 3 and c -1", pred)
	}
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.oh` TRANSFORM(ML.ONE_HOT_ENCODER(c, 'none', 1, 1) OVER ()[OFFSET(0)].index AS ce, y) "+
		"OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT c, y FROM `%s.v`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL with ML.ONE_HOT_ENCODER: %v", err)
	}
	// Top 1: b alone, index 1; a and c are 0.
	pred = mlQuery(t, h, c, fmt.Sprintf("SELECT c, predicted_y FROM ML.PREDICT(MODEL `%s.oh`, (SELECT 'a' AS c UNION ALL SELECT 'b')) ORDER BY c", name))
	if len(pred) != 2 || !near(pred[1]["predicted_y"], 3, 1e-6) || near(pred[0]["predicted_y"], 3, 1e-6) {
		t.Errorf("ML.PREDICT one-hot-encoded (top 1): %v; want b 3 and a less", pred)
	}
}
