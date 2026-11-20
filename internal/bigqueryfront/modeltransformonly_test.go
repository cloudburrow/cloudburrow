package bigqueryfront

import (
	"strings"
	"testing"
)

// TestTransformOnly: a TRANSFORM_ONLY model learns its output columns'
// statistics for ML.FEATURE_INFO and trains nothing (#1929).
func TestTransformOnly(t *testing.T) {
	cols := []mlColumn{{"x", "FLOAT"}, {"s", "STRING"}, {"d", "DATE"}}
	rows := [][]*string{{sp("1"), sp("a"), sp("2026-01-01")}, {sp("3"), nil, sp("2026-01-02")}}
	o := defaultMLOptions()
	o.ModelType = "TRANSFORM_ONLY"
	m, f := trainModelT(o, cols, rows)
	if f != nil {
		t.Fatal(f)
	}
	if len(m.Features) != 3 || m.Features[0].Mean != 2 || m.Features[1].NullCount != 1 || m.Features[2].Type != "DATE" || len(m.Features[2].Categories) != 2 {
		t.Fatalf("features %+v", m.Features)
	}
	if code, msg := checkModelOptions(o); code != 0 {
		t.Errorf("options: %d %s", code, msg)
	}
}

// TestMulticlassWeightRows: ML.WEIGHTS of a multiclass model gives a row
// per class and processed_input, the class last (#1929).
func TestMulticlassWeightRows(t *testing.T) {
	tr := mlTrained{Features: []mlFeature{{Name: "x", Type: "FLOAT64", Mean: 1, Stddev: 2}, {Name: "c", Type: "STRING", Categories: []string{"p", "q"}}}}
	rows := tr.weightRows([]float64{1, 4, 0.5, -0.5}, false, ", 'k'")
	want := []string{"('x', 2.0, " + categoryWeightsType + "[], 'k')", "('c', NULL, " + categoryWeightsType + "[('p', 0.5), ('q', (-0.5))], 'k')",
		"('__INTERCEPT__', (-1.0), " + categoryWeightsType + "[], 'k')"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Errorf("rows\n%v\nwant\n%v", rows, want)
	}
}
