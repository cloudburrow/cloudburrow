package xgbmodel

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The models in testdata were trained and saved by XGBoost 3.1.0, the
// version BigQuery ML runs (testdata/gen.py, run in a virtualenv with
// xgboost==3.1.0 and numpy), and cases.json holds its Booster.predict of
// each model's test rows and its get_score importances.
type xgbCase struct {
	Test       [][]*float64                    `json:"test"`
	Pred       json.RawMessage                 `json:"pred"`
	Importance map[string][]map[string]float64 `json:"importance"`
}

func TestPredictMatchesXGBoost(t *testing.T) {
	raw, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]xgbCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for name, c := range cases {
		var preds [][]float64
		var flat []float64
		if json.Unmarshal(c.Pred, &flat) == nil {
			for _, v := range flat {
				preds = append(preds, []float64{v})
			}
		} else if err := json.Unmarshal(c.Pred, &preds); err != nil {
			t.Fatal(err)
		}
		exts := []string{".json", ".ubj"}
		if name == "noname" {
			exts = exts[:1]
		}
		for _, ext := range exts {
			data, err := os.ReadFile(filepath.Join("testdata", name+ext))
			if err != nil {
				t.Fatal(err)
			}
			m, err := Load(data)
			if err != nil {
				t.Fatalf("%s%s: %v", name, ext, err)
			}
			if m.Version != [3]int{3, 1, 0} {
				t.Errorf("%s%s: version %v", name, ext, m.Version)
			}
			for i, row := range c.Test {
				x := make([]float64, len(row))
				for j, v := range row {
					x[j] = math.NaN()
					if v != nil {
						x[j] = *v
					}
				}
				got, err := m.Predict(x)
				if err != nil {
					t.Fatalf("%s%s row %d: %v", name, ext, i, err)
				}
				if len(got) != len(preds[i]) || len(got) != m.OutputWidth() {
					t.Fatalf("%s%s row %d: %v, want %v", name, ext, i, got, preds[i])
				}
				for k := range got {
					if d := math.Abs(got[k] - preds[i][k]); d > 1e-6*math.Max(1, math.Abs(preds[i][k])) {
						t.Errorf("%s%s row %d: %v, want %v (XGBoost 3.1.0)", name, ext, i, got, preds[i])
						break
					}
				}
			}
			if imp := c.Importance; len(imp) > 0 {
				names := m.FeatureNames
				for _, fi := range m.Importances() {
					n := names[fi.Feature]
					w, ok := imp["weight"][0][n]
					if !ok {
						if fi.Weight != 0 {
							t.Errorf("%s: %s weight %d, XGBoost none", name, n, fi.Weight)
						}
						continue
					}
					if float64(fi.Weight) != w || math.Abs(fi.Gain-imp["gain"][0][n]) > 1e-3*imp["gain"][0][n] ||
						math.Abs(fi.Cover-imp["cover"][0][n]) > 1e-3*imp["cover"][0][n] {
						t.Errorf("%s: %s %+v, XGBoost weight %v gain %v cover %v", name, n, fi, w, imp["gain"][0][n], imp["cover"][0][n])
					}
				}
			}
		}
	}
}

func TestLoadRefusals(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"old binary", "\x00\x00\x00\x3fbinf"},
		{"not a model", `{"a": 1}`},
		{"truncated ubj", "{L\x00\x00\x00\x00\x00\x00\x00\x07learn"},
		{"unknown objective", `{"learner":{"learner_model_param":{"num_feature":"1","base_score":"5E-1"},"objective":{"name":"foo"},"gradient_booster":{"name":"gbtree","model":{"trees":[],"tree_info":[]}}}}`},
	} {
		if _, err := Load([]byte(tc.data)); err == nil {
			t.Errorf("%s: loaded", tc.name)
		}
	}
	// A tree whose child points back at its root never loads.
	bad := `{"learner":{"learner_model_param":{"num_feature":"1","base_score":"0"},"objective":{"name":"reg:squarederror"},"gradient_booster":{"name":"gbtree","model":{"tree_info":[0],"trees":[{"tree_param":{"num_nodes":"3"},"left_children":[1,0,-1],"right_children":[2,-1,-1],"split_indices":[0,0,0],"split_conditions":[0,1,2],"default_left":[1,0,0]}]}}}}`
	if _, err := Load([]byte(bad)); err == nil {
		t.Error("a cyclic tree loaded")
	}
}
