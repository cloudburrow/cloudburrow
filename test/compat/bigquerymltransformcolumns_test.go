//go:build compat

package compat

import (
	"fmt"
	"testing"
)

// TestBigQueryMLTransformColumns (#1595): a model trained with TRANSFORM
// gives, in models.get, its featureColumns as the columns the TRANSFORM
// reads ("Input feature columns for the model inference") and its
// transformColumns each with transformSql, "the SQL expression used in the
// column transform" (https://docs.cloud.google.com/bigquery/docs/reference/rest/v2/models,
// read 2026-10-17). Read through the generated REST client, since the Go
// client exposes no transformColumns. Before, featureColumns were the
// TRANSFORM's outputs and no transformSql was set.
//
// covers: bigquery.models.get, bigquery.jobs.query
func TestBigQueryMLTransformColumns(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := generatedBigQuery(t, h)
	name := mlDataset(t, h, c, map[string]string{
		"rows": "SELECT CAST(x AS FLOAT64) AS x, 3.0 + 2 * x AS y, 'k' AS unused FROM UNNEST(GENERATE_ARRAY(0, 9)) AS x",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.tc` TRANSFORM(ML.STANDARD_SCALER(x) OVER () AS xs, y) "+
		"OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT x, y, unused FROM `%s.rows`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL with TRANSFORM: %v", err)
	}
	m, err := svc.Models.Get(project, name, "tc").Context(h.Context()).Do()
	if err != nil {
		t.Fatalf("models.get: %v", err)
	}
	var feats []string
	for _, f := range m.FeatureColumns {
		kind := ""
		if f.Type != nil {
			kind = f.Type.TypeKind
		}
		feats = append(feats, f.Name+" "+kind)
	}
	if fmt.Sprint(feats) != "[x FLOAT64]" {
		t.Errorf("featureColumns %v, want [x FLOAT64]", feats)
	}
	got := map[string]string{}
	for _, tc := range m.TransformColumns {
		got[tc.Name] = tc.TransformSql
	}
	if got["xs"] != "ML.STANDARD_SCALER(x) OVER ()" {
		t.Errorf("transformColumns %v, want xs with transformSql ML.STANDARD_SCALER(x) OVER ()", got)
	}
}

// TestBigQueryMLPolynomialExpandOrder (#1595): ML.POLYNOMIAL_EXPAND's
// fields come feature by feature, as its page's example gives them:
// {"f1":…,"f1_f1":…,"f1_f2":…,"f2":…,"f2_f2":…}
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-polynomial-expand,
// read 2026-10-17). Before, the input fields came first, then the products.
//
// covers: bigquery.jobs.query
func TestBigQueryMLPolynomialExpandOrder(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	row := mlQuery(t, h, c, "SELECT TO_JSON_STRING(ML.POLYNOMIAL_EXPAND(STRUCT(2 AS f1, 3 AS f2))) AS j")
	if len(row) != 1 || row[0]["j"] != `{"f1":2.0,"f1_f1":4.0,"f1_f2":6.0,"f2":3.0,"f2_f2":9.0}` {
		t.Errorf("ML.POLYNOMIAL_EXPAND: %v, want f1, f1_f1, f1_f2, f2, f2_f2", row)
	}
}
