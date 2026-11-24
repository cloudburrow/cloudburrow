package vertexai

// Agent Platform Model Monitoring's statistics (#2214), shared by Model
// Monitoring v2 (modelmonitor.go: ModelMonitor and ModelMonitoringJob,
// v1beta1) and v1 (mdmjobs.go: ModelDeploymentMonitoringJob). Both
// compare a target dataset's feature distributions with a baseline's
// and raise an anomaly where the distance passes the threshold.
//
// Documented behaviour (the ModelMonitoringSpec, ModelMonitoringStats and
// ModelMonitoringAlert protos, v1beta1; "Monitor feature skew and drift",
// https://docs.cloud.google.com/vertex-ai/docs/model-monitoring/using-model-monitoring;
// "Run a monitoring job", .../model-monitoring/run-monitoring-job; read
// 2026-11-24):
//   - a categorical feature's distance is the L-infinity norm or the
//     Jensen-Shannon divergence, a numeric feature's the Jensen-Shannon
//     divergence;
//   - CSV files need header names matching the schema's feature names,
//     JSONL keys matching them; nested features are not supported;
//   - v1 monitors "every categorical and numerical feature ... with
//     threshold values of 0.3" by default.
//
// UNVERIFIED: Google does not document how numeric features are bucketed:
// here 10 equal-width buckets over the two datasets' joint range; the
// Jensen-Shannon divergence uses base 2, so it lies in [0, 1].

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const (
	algoLInf = "l_infinity"
	algoJS   = "jensen_shannon_divergence"
	// numericBuckets is how many equal-width buckets a numeric feature's
	// histogram has.
	numericBuckets = 10
)

// monitorField is one schema field: its name and whether it is numeric.
type monitorField struct {
	Name    string
	Numeric bool
}

// fieldNumeric is whether a ModelMonitoringSchema data type is numeric:
// float and integer are; boolean, string and categorical are categorical.
func fieldNumeric(dataType string) (bool, error) {
	switch dataType {
	case "float", "integer":
		return true, nil
	case "boolean", "string", "categorical":
		return false, nil
	}
	return false, apierror.InvalidArgument("data_type %q is not supported: one of float, integer, boolean, string, categorical", dataType)
}

// featureDistance is one feature's comparison.
type featureDistance struct {
	Field              monitorField
	Algorithm          string
	Distance           float64
	Current, Baseline  *structpb.Value
	CurrentN, BaseN    int
	CurrentMissing     int
	BaselineMissing    int
	Threshold          float64
	HasThreshold       bool
	Anomaly            bool
	CurrentMean, BMean float64
}

// rowsValues are the field's values in rows, as strings; missing ones
// are counted.
func rowsValues(rows []map[string]any, name string) (vals []string, missing int) {
	for _, r := range rows {
		v, ok := r[name]
		if !ok || v == nil {
			missing++
			continue
		}
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				vals = append(vals, scalarText(e))
			}
		default:
			s := scalarText(x)
			if s == "" {
				missing++
				continue
			}
			vals = append(vals, s)
		}
	}
	return vals, missing
}

func scalarText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return x.String()
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// compareFeature computes f's distance between baseline and target rows
// with algo ("" picks the default for its kind).
func compareFeature(f monitorField, algo string, baseline, target []map[string]any) (*featureDistance, error) {
	bv, bm := rowsValues(baseline, f.Name)
	tv, tm := rowsValues(target, f.Name)
	d := &featureDistance{Field: f, CurrentN: len(tv), BaseN: len(bv), CurrentMissing: tm, BaselineMissing: bm}
	if f.Numeric {
		if algo == "" {
			algo = algoJS
		}
		if algo != algoJS {
			return nil, apierror.InvalidArgument("numeric_metric_type %q is not supported: only jensen_shannon_divergence", algo)
		}
		bn, err := parseNumbers(f.Name, bv)
		if err != nil {
			return nil, err
		}
		tn, err := parseNumbers(f.Name, tv)
		if err != nil {
			return nil, err
		}
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, x := range append(append([]float64{}, bn...), tn...) {
			lo, hi = math.Min(lo, x), math.Max(hi, x)
		}
		bh, edges := histogram(bn, lo, hi)
		th, _ := histogram(tn, lo, hi)
		d.Distance = jsDivergence(normDist(bh), normDist(th))
		d.Baseline, d.BMean = numericStats(f.Name, bn, bh, edges, bm)
		d.Current, d.CurrentMean = numericStats(f.Name, tn, th, edges, tm)
	} else {
		if algo == "" {
			algo = algoLInf
		}
		if algo != algoLInf && algo != algoJS {
			return nil, apierror.InvalidArgument("categorical_metric_type %q is not supported: l_infinity or jensen_shannon_divergence", algo)
		}
		labels := map[string]bool{}
		for _, v := range append(append([]string{}, bv...), tv...) {
			labels[v] = true
		}
		keys := sortedNames(labels)
		bc, tc := counts(bv, keys), counts(tv, keys)
		bp, tp := normDist(bc), normDist(tc)
		if algo == algoLInf {
			for i := range bp {
				d.Distance = math.Max(d.Distance, math.Abs(bp[i]-tp[i]))
			}
		} else {
			d.Distance = jsDivergence(bp, tp)
		}
		d.Baseline = categoricalStats(f.Name, keys, bc, bm)
		d.Current = categoricalStats(f.Name, keys, tc, tm)
	}
	d.Algorithm = algo
	if len(bv) == 0 || len(tv) == 0 {
		d.Distance = 0
	}
	d.Distance = math.Round(d.Distance*1e9) / 1e9
	return d, nil
}

func parseNumbers(name string, vals []string) ([]float64, error) {
	out := make([]float64, 0, len(vals))
	for _, v := range vals {
		x, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil, apierror.InvalidArgument("feature %q is numeric but has the value %q", name, v)
		}
		out = append(out, x)
	}
	return out, nil
}

// histogram counts xs in numericBuckets equal-width buckets over [lo, hi].
func histogram(xs []float64, lo, hi float64) ([]float64, []float64) {
	h := make([]float64, numericBuckets)
	edges := make([]float64, numericBuckets+1)
	if math.IsInf(lo, 0) {
		lo, hi = 0, 0
	}
	w := (hi - lo) / numericBuckets
	for i := range edges {
		edges[i] = lo + w*float64(i)
	}
	for _, x := range xs {
		i := 0
		if w > 0 {
			i = int((x - lo) / w)
		}
		if i >= numericBuckets {
			i = numericBuckets - 1
		}
		if i < 0 {
			i = 0
		}
		h[i]++
	}
	return h, edges
}

func counts(vals, keys []string) []float64 {
	idx := map[string]int{}
	for i, k := range keys {
		idx[k] = i
	}
	out := make([]float64, len(keys))
	for _, v := range vals {
		out[idx[v]]++
	}
	return out
}

func normDist(c []float64) []float64 {
	sum := 0.0
	for _, x := range c {
		sum += x
	}
	out := make([]float64, len(c))
	if sum == 0 {
		return out
	}
	for i, x := range c {
		out[i] = x / sum
	}
	return out
}

// jsDivergence is the base-2 Jensen-Shannon divergence of p and q.
func jsDivergence(p, q []float64) float64 {
	kl := func(a, m []float64) float64 {
		s := 0.0
		for i := range a {
			if a[i] > 0 && m[i] > 0 {
				s += a[i] * math.Log2(a[i]/m[i])
			}
		}
		return s
	}
	m := make([]float64, len(p))
	for i := range p {
		m[i] = (p[i] + q[i]) / 2
	}
	return math.Max(0, (kl(p, m)+kl(q, m))/2)
}

// numericStats is a feature's distribution in the shape of
// tensorflow.metadata.v0.FeatureNameStatistics' numStats.
func numericStats(name string, xs, h, edges []float64, missing int) (*structpb.Value, float64) {
	mean, sd, lo, hi := 0.0, 0.0, 0.0, 0.0
	if len(xs) > 0 {
		lo, hi = xs[0], xs[0]
		for _, x := range xs {
			mean += x
			lo, hi = math.Min(lo, x), math.Max(hi, x)
		}
		mean /= float64(len(xs))
		for _, x := range xs {
			sd += (x - mean) * (x - mean)
		}
		sd = math.Sqrt(sd / float64(len(xs)))
	}
	sorted := append([]float64{}, xs...)
	sort.Float64s(sorted)
	median := 0.0
	if n := len(sorted); n > 0 {
		median = sorted[n/2]
		if n%2 == 0 {
			median = (sorted[n/2-1] + sorted[n/2]) / 2
		}
	}
	buckets := []any{}
	for i := range h {
		buckets = append(buckets, map[string]any{"lowValue": edges[i], "highValue": edges[i+1], "sampleCount": h[i]})
	}
	v, _ := structpb.NewValue(map[string]any{
		"name": name, "type": "FLOAT",
		"numStats": map[string]any{
			"commonStats": map[string]any{"numNonMissing": float64(len(xs)), "numMissing": float64(missing)},
			"mean":        mean, "stdDev": sd, "min": lo, "max": hi, "median": median,
			"histograms": []any{map[string]any{"type": "STANDARD", "buckets": buckets}},
		},
	})
	return v, mean
}

// categoricalStats is a feature's distribution in the shape of
// FeatureNameStatistics' stringStats.
func categoricalStats(name string, keys []string, c []float64, missing int) *structpb.Value {
	n := 0.0
	buckets := []any{}
	unique := 0.0
	for i, k := range keys {
		n += c[i]
		if c[i] > 0 {
			unique++
		}
		buckets = append(buckets, map[string]any{"label": k, "sampleCount": c[i]})
	}
	v, _ := structpb.NewValue(map[string]any{
		"name": name, "type": "STRING",
		"stringStats": map[string]any{
			"commonStats":   map[string]any{"numNonMissing": n, "numMissing": float64(missing)},
			"unique":        unique,
			"rankHistogram": map[string]any{"buckets": buckets},
		},
	})
	return v
}

// ---- reading datasets

// objectStore is the instance's Cloud Storage, as the pipeline runner
// reads and writes it.
type objectStore interface {
	ReadObject(ctx context.Context, bucket, object string, generation int64) ([]byte, error)
	WriteObject(ctx context.Context, bucket, object, contentType string, data []byte) error
	ListObjects(ctx context.Context, bucket, prefix string) ([]string, error)
}

func monGCS(field, uri string) (string, string, error) {
	rest, ok := strings.CutPrefix(uri, "gs://")
	b, o, _ := strings.Cut(rest, "/")
	if !ok || b == "" {
		return "", "", apierror.InvalidArgument("%s %q is not a gs:// URI", field, uri)
	}
	return b, o, nil
}

// readGCSRows reads the rows of the objects uri names (with wildcards) in
// format: "csv" or "jsonl".
func readGCSRows(ctx context.Context, store objectStore, uri, format string) ([]map[string]any, error) {
	if store == nil {
		return nil, apierror.FailedPrecondition("Cloud Storage is not served on this instance, so %s cannot be read", uri)
	}
	b, o, err := monGCS("gcs_uri", uri)
	if err != nil {
		return nil, err
	}
	objects := []string{o}
	if i := strings.IndexAny(o, "*?["); i >= 0 {
		names, err := store.ListObjects(ctx, b, o[:i])
		if err != nil {
			return nil, err
		}
		objects = objects[:0]
		for _, n := range names {
			if ok, _ := path.Match(o, n); ok || strings.Contains(o, "**") && strings.HasPrefix(n, o[:i]) {
				objects = append(objects, n)
			}
		}
		if len(objects) == 0 {
			return nil, apierror.NotFound("no objects match %s", uri)
		}
	}
	var rows []map[string]any
	for _, obj := range objects {
		data, err := store.ReadObject(ctx, b, obj, 0)
		if err != nil {
			return nil, apierror.NotFound("reading gs://%s/%s: %v", b, obj, err)
		}
		r, err := parseRows(data, format)
		if err != nil {
			return nil, fmt.Errorf("gs://%s/%s: %w", b, obj, err)
		}
		rows = append(rows, r...)
	}
	return rows, nil
}

func parseRows(data []byte, format string) ([]map[string]any, error) {
	switch strings.ToLower(format) {
	case "csv":
		rd := csv.NewReader(bytes.NewReader(data))
		rd.FieldsPerRecord = -1
		head, err := rd.Read()
		if err != nil {
			return nil, apierror.InvalidArgument("the CSV file has no header row: %v", err)
		}
		var rows []map[string]any
		for {
			rec, err := rd.Read()
			if err == io.EOF {
				return rows, nil
			}
			if err != nil {
				return nil, apierror.InvalidArgument("reading the CSV file: %v", err)
			}
			row := map[string]any{}
			for i, h := range head {
				if i < len(rec) {
					row[strings.TrimSpace(h)] = rec[i]
				}
			}
			rows = append(rows, row)
		}
	case "jsonl":
		var rows []map[string]any
		for i, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var row map[string]any
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				return nil, apierror.InvalidArgument("line %d is not a JSON object: %v", i+1, err)
			}
			rows = append(rows, row)
		}
		return rows, nil
	}
	return nil, apierror.InvalidArgument("data format %q is not supported here: csv or jsonl", format)
}
