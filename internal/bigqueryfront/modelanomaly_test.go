package bigqueryfront

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestAnomalyBaseline: KMEANS, PCA and AUTOENCODER keep their training
// rows' metrics at CREATE MODEL, and contamination sets the cutoff so that
// that share of the training rows is above it (modelanomaly.go, #1929).
func TestAnomalyBaseline(t *testing.T) {
	cols := []mlColumn{{"x", "FLOAT"}, {"y", "FLOAT"}}
	var rows [][]*string
	for i := 0; i < 20; i++ {
		x, y := float64(i%4)*0.1, float64(i%5)*0.1
		if i%2 == 1 {
			x, y = x+10, y+10
		}
		if i == 19 {
			x, y = 5, -3 // the outlier
		}
		rows = append(rows, []*string{sp(strconv.FormatFloat(x, 'f', -1, 64)), sp(strconv.FormatFloat(y, 'f', -1, 64))})
	}
	for _, typ := range []string{"KMEANS", "PCA", "AUTOENCODER"} {
		o := defaultMLOptions()
		o.ModelType = typ
		switch typ {
		case "KMEANS":
			o.NumClusters = 2
		case "PCA":
			o.NumPC = 1
		case "AUTOENCODER":
			o.HiddenUnits = []int{4, 1, 4}
			o.MaxIterations = 30
		}
		m, f := trainModelT(o, cols, rows)
		if f != nil {
			t.Fatalf("%s: %v", typ, f)
		}
		b := m.Anomaly
		if b == nil || b.Rows != 20 || len(b.Top) != 11 {
			t.Fatalf("%s: baseline %+v", typ, b)
		}
		for i := 1; i < len(b.Top); i++ {
			if b.Top[i] > b.Top[i-1] {
				t.Fatalf("%s: not largest first: %v", typ, b.Top)
			}
		}
		if typ == "KMEANS" && len(b.Radius) != 2 {
			t.Fatalf("radii %v", b.Radius)
		}
		// contamination 0 flags nothing of the training data, 0.1 two rows
		// at most, and 0.5 half.
		for _, c := range []struct {
			cont float64
			max  int
		}{{0, 0}, {0.1, 2}, {0.5, 10}} {
			cut := b.cutoff(c.cont)
			n := 0
			for _, row := range rows {
				x := m.encodeRow(map[string]*string{"x": row[0], "y": row[1]})[1:]
				if v, _ := m.anomalyMetric(typ, x); v > cut {
					n++
				}
			}
			if n > c.max {
				t.Errorf("%s contamination %v: %d anomalies of the training rows, want at most %d", typ, c.cont, n, c.max)
			}
		}
		// The outlier is the most anomalous training row.
		x := m.encodeRow(map[string]*string{"x": rows[19][0], "y": rows[19][1]})[1:]
		if v, _ := m.anomalyMetric(typ, x); typ != "AUTOENCODER" && v != b.Top[0] {
			t.Errorf("%s: outlier's metric %v, largest %v", typ, v, b.Top[0])
		}
	}
}

func TestAnomalyHelpers(t *testing.T) {
	if normalized(0, 0) != 0 || !math.IsInf(normalized(1, 0), 1) || normalized(2, 4) != 0.5 {
		t.Error("normalized")
	}
	if got := sqlFloatInf(math.Inf(1)); got != "CAST('inf' AS FLOAT64)" {
		t.Error(got)
	}
	for _, c := range []struct {
		struc map[string]string
		ok    bool
	}{
		{map[string]string{"contamination": "0.1"}, true},
		{map[string]string{"contamination": "0"}, true},
		{map[string]string{"contamination": "0.5"}, true},
		{map[string]string{"contamination": "0.6"}, false},
		{map[string]string{"contamination": "-0.1"}, false},
		{map[string]string{}, false},
		{map[string]string{"contamination": "0.1", "anomaly_prob_threshold": "0.9"}, false},
	} {
		_, why := mlCall{struc: c.struc}.contamination()
		if (why == "") != c.ok {
			t.Errorf("%v: %q", c.struc, why)
		}
	}
	b := &anomalyBaseline{Rows: 10, Top: []float64{9, 8, 7, 6, 5, 4}}
	if b.cutoff(0) != 9 || b.cutoff(0.1) != 8 || b.cutoff(0.5) != 4 {
		t.Error("cutoff")
	}
	var nilTop anomalyBaseline
	if !math.IsInf(nilTop.cutoff(0.1), 1) {
		t.Error("empty cutoff")
	}
	if !strings.HasPrefix(mseOf([]string{"a"}, []string{"b"}), "((POW(a - b, 2))") {
		t.Error(mseOf([]string{"a"}, []string{"b"}))
	}
}
