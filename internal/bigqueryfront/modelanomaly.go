package bigqueryfront

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ML.DETECT_ANOMALIES of a KMEANS, PCA or AUTOENCODER model (#1929,
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-detect-anomalies):
//
//	ML.DETECT_ANOMALIES(MODEL m, STRUCT(contamination AS contamination), {TABLE t | (query)})
//
// contamination, a FLOAT64 in [0, 0.5], is "the proportion of anomalies in
// the training data"; it "determines the cutoff threshold of the target
// metric", and a row whose metric is greater than the cutoff is an anomaly.
// The target metric is the mean squared error of the row's reconstruction
// for an AUTOENCODER or PCA model, and for KMEANS the normalized distance:
// the smallest over the clusters of the row's distance to the centroid
// (by the model's distance_type) over the cluster's radius, the root mean
// square of the distances of its training rows to its centroid.
//
// Output: is_anomaly BOOL, then mean_squared_error FLOAT64 (AUTOENCODER,
// PCA) or normalized_distance FLOAT64 and centroid_id INT64 (KMEANS), then
// the input's columns.
//
// UNVERIFIED (docs re-read 2026-11-19, unanswered there):
//   - the cutoff is the training rows' metric at nearest rank: sorted
//     ascending, the ceil((1-contamination)·n)-th value, so that at most a
//     contamination share of the training rows is above it; no
//     interpolation;
//   - the training rows' metrics are kept at CREATE MODEL (the top half,
//     contamination being at most 0.5); a model kept before then reads its
//     training query again;
//   - centroid_id is the centroid of the smallest normalized distance; a
//     cluster of radius 0 gives 0 for a row on its centroid and +inf
//     otherwise;
//   - a PCA row's error is measured in the model's encoded (standardized,
//     or centered) space against its projection on the kept components; an
//     AUTOENCODER's as ML.RECONSTRUCTION_LOSS' mean_squared_error;
//   - the error texts.

// anomalyBaseline is what ML.DETECT_ANOMALIES needs of the training rows.
type anomalyBaseline struct {
	// Rows is the number of training rows; Top their metrics, largest
	// first, the largest Rows/2 + 1 of them.
	Rows int       `json:"rows"`
	Top  []float64 `json:"top"`
	// Radius is a KMEANS model's radius of each cluster.
	Radius []float64 `json:"radius,omitempty"`
}

// anomalyModel reports whether typ detects anomalies by contamination.
func anomalyModel(typ string) bool { return typ == "KMEANS" || typ == "PCA" || typ == "AUTOENCODER" }

// setAnomalyBaseline keeps the training rows' metrics (X their encoded
// columns, no intercept).
func (t *mlTrained) setAnomalyBaseline(typ string, X [][]float64) {
	b := &anomalyBaseline{Rows: len(X)}
	if typ == "KMEANS" {
		b.Radius = clusterRadii(X, t.Centroids, t.Distance)
	}
	t.Anomaly = b
	ms := make([]float64, len(X))
	for i, x := range X {
		ms[i], _ = t.anomalyMetric(typ, x)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(ms)))
	b.Top = ms[:min(len(ms), len(ms)/2+1)]
}

// clusterRadii are the root mean square distances of each cluster's rows
// to its centroid, each row in its nearest centroid's cluster.
func clusterRadii(X [][]float64, cs [][]float64, typ string) []float64 {
	sum := make([]float64, len(cs))
	n := make([]int, len(cs))
	for _, x := range X {
		k, d := nearest(x, cs, typ)
		sum[k] += d * d
		n[k]++
	}
	out := make([]float64, len(cs))
	for k := range cs {
		if n[k] > 0 {
			out[k] = math.Sqrt(sum[k] / float64(n[k]))
		}
	}
	return out
}

// normalized is a distance over a radius (UNVERIFIED for radius 0).
func normalized(d, radius float64) float64 {
	if radius == 0 {
		if d == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return d / radius
}

// anomalyMetric is the target metric of the encoded row x, and for KMEANS
// the index of its centroid.
func (t *mlTrained) anomalyMetric(typ string, x []float64) (float64, int) {
	switch typ {
	case "KMEANS":
		best, bk := math.Inf(1), 0
		for k, c := range t.Centroids {
			if v := normalized(mlDistance(x, c, t.Distance), t.Anomaly.Radius[k]); v < best || k == 0 {
				best, bk = v, k
			}
		}
		return best, bk
	case "PCA":
		rec := make([]float64, len(x))
		for _, v := range t.Components {
			z := dot(v, x)
			for j := range rec {
				rec[j] += z * v[j]
			}
		}
		return meanSquared(x, rec), 0
	}
	return meanSquared(x, t.DNN.forward(x)), 0
}

func meanSquared(a, b []float64) float64 {
	if len(a) == 0 {
		return 0
	}
	var s float64
	for j := range a {
		s += (a[j] - b[j]) * (a[j] - b[j])
	}
	return s / float64(len(a))
}

// cutoff is the metric a row must exceed to be an anomaly.
func (b *anomalyBaseline) cutoff(contamination float64) float64 {
	if len(b.Top) == 0 {
		return math.Inf(1)
	}
	i := int(math.Floor(contamination*float64(b.Rows) + 1e-9))
	return b.Top[min(i, len(b.Top)-1)]
}

// sqlFloatInf is sqlF, with infinities as FLOAT64 'inf'.
func sqlFloatInf(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "CAST('inf' AS FLOAT64)"
	case math.IsInf(v, -1):
		return "CAST('-inf' AS FLOAT64)"
	}
	return sqlF(v)
}

// contamination reads a call's contamination option.
func (c mlCall) contamination() (float64, string) {
	if _, ok := c.struc["anomaly_prob_threshold"]; ok {
		return 0, "anomaly_prob_threshold is only supported for time series models; use contamination"
	}
	v, ok := c.struc["contamination"]
	if !ok {
		return 0, "ML.DETECT_ANOMALIES requires contamination for an autoencoder, k-means or PCA model: STRUCT(<value> AS contamination)"
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || f < 0 || f > 0.5 {
		return 0, "Invalid contamination " + v + ": it must be in the range [0, 0.5]"
	}
	return f, ""
}

// detectAnomalies is ML.DETECT_ANOMALIES of a KMEANS, PCA or AUTOENCODER
// model.
func (f front) detectAnomalies(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	t := md.Trained
	typ := md.Options.ModelType
	cont, why := c.contamination()
	if why != "" {
		return "", 400, why
	}
	if c.input == "" {
		return "", 400, "ML.DETECT_ANOMALIES requires an input: a query in parentheses or TABLE name"
	}
	if typ == "AUTOENCODER" && t.DNN == nil {
		return "", 500, "cloudburrow: the autoencoder has no network"
	}
	if t.Anomaly == nil || typ == "KMEANS" && len(t.Anomaly.Radius) != len(t.Centroids) {
		// A model kept before its baseline was: its training rows again.
		X, code, msg := f.mlFeatureRows(r, q, md, md.Query)
		if code != 0 {
			return "", code, "ML.DETECT_ANOMALIES could not read the model's training data: " + msg
		}
		t.setAnomalyBaseline(typ, X)
	}
	cut := sqlFloatInf(t.Anomaly.cutoff(cont))
	from, ref, helpers := md.mlInput(c.input)
	enc := t.encodedSQL(ref)
	switch typ {
	case "KMEANS":
		var nds, names, cases []string
		for k, cen := range t.Centroids {
			d := distanceSQL(enc, cen, t.Distance)
			nd := "(" + d + " / " + sqlF(t.Anomaly.Radius[k]) + ")"
			if t.Anomaly.Radius[k] == 0 {
				nd = "IF(" + d + " = 0, 0.0, CAST('inf' AS FLOAT64))"
			}
			nds = append(nds, nd+fmt.Sprintf(" AS _cbml_nd%d", k))
			names = append(names, fmt.Sprintf("_cbml_nd%d", k))
			cases = append(cases, fmt.Sprintf("WHEN _cbml_nd%d = _cbml_m THEN %d", k, k+1))
		}
		least := names[0]
		if len(names) > 1 {
			least = "LEAST(" + strings.Join(names, ", ") + ")"
		}
		return "(SELECT _cbml_m > " + cut + " AS is_anomaly, _cbml_m AS normalized_distance, CASE " + strings.Join(cases, " ") +
			" END AS centroid_id, * EXCEPT (_cbml_m, " + strings.Join(names, ", ") + ")" +
			" FROM (SELECT *, " + least + " AS _cbml_m FROM (SELECT " + except(helpers) + ", " + strings.Join(nds, ", ") + " FROM " + from + ")))", 0, ""
	case "PCA":
		var zs []string
		for k, v := range t.Components {
			zs = append(zs, dotSQL(append([]float64{0}, v...), enc)+fmt.Sprintf(" AS _cbml_z%d", k))
		}
		rec := make([]string, len(enc))
		for j := range enc {
			terms := []string{"0.0"}
			for k, v := range t.Components {
				if v[j] != 0 {
					terms = append(terms, fmt.Sprintf("%s * _cbml_z%d", sqlF(v[j]), k))
				}
			}
			rec[j] = "(" + strings.Join(terms, " + ") + ")"
		}
		var zn []string
		for k := range t.Components {
			zn = append(zn, fmt.Sprintf("_cbml_z%d", k))
		}
		inner := "(SELECT *, " + strings.Join(append(zs, strings.Join(encAs(enc), ", ")), ", ") + " FROM " + from + ")"
		encRef := make([]string, len(enc))
		for j := range enc {
			encRef[j] = fmt.Sprintf("_cbml_e%d", j)
		}
		hidden := append(append(append([]string{}, helpers...), zn...), encRef...)
		return "(SELECT _cbml_m > " + cut + " AS is_anomaly, _cbml_m AS mean_squared_error, * EXCEPT (_cbml_m)" +
			" FROM (SELECT " + except(hidden) + ", " + mseOf(encRef, rec) + " AS _cbml_m FROM " + inner + "))", 0, ""
	}
	from, added := t.DNN.dnnFrom(from, enc)
	var outs []string
	for k := range enc {
		outs = append(outs, fmt.Sprintf("_cbml_o%d", k))
	}
	return "(SELECT _cbml_m > " + cut + " AS is_anomaly, _cbml_m AS mean_squared_error, * EXCEPT (_cbml_m)" +
		" FROM (SELECT " + except(append(helpers, added...)) + ", " + mseOf(enc, outs) + " AS _cbml_m FROM " + from + "))", 0, ""
}

// encAs names each encoded column _cbml_e<j>.
func encAs(enc []string) []string {
	out := make([]string, len(enc))
	for j, e := range enc {
		out[j] = fmt.Sprintf("%s AS _cbml_e%d", e, j)
	}
	return out
}

// mseOf is the SQL mean of (a_j - b_j)².
func mseOf(a, b []string) string {
	if len(a) == 0 {
		return "0.0"
	}
	var sq []string
	for j := range a {
		sq = append(sq, fmt.Sprintf("POW(%s - %s, 2)", a[j], b[j]))
	}
	return "((" + strings.Join(sq, " + ") + ") / " + sqlF(float64(len(a))) + ")"
}
