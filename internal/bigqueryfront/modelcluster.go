package bigqueryfront

import (
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// KMEANS and PCA models (#1481), trained by the front on the rows of their
// training query, with BigQuery's documented options and defaults:
//
// KMEANS (https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-kmeans):
// num_clusters log10 of the rows by default (2 to 100), kmeans_init_method
// RANDOM (KMEANS++, CUSTOM with kmeans_init_col, a BOOL column with exactly
// num_clusters TRUE rows, left out of the features), distance_type
// EUCLIDEAN (COSINE), standardize_features TRUE, max_iterations 20,
// early_stop TRUE, min_rel_progress 0.01. A STRING or BOOL feature is
// one-hot encoded and never standardized. Training is Lloyd's algorithm:
// each row to its nearest centroid, each centroid the mean of its rows.
//
// PCA (https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-pca):
// numeric features always centered, and scaled to unit variance with
// scale_features TRUE (the default); exactly one of
// num_principal_components and pca_explained_variance_ratio; pca_solver
// FULL or AUTO, both a full eigendecomposition of the sample covariance
// (AUTO's documented choice for fewer than 1,000 columns, and CloudBurrow
// refuses more).
//
// What is not documented, and so chosen here (UNVERIFIED against Google;
// docs/compatibility.md): the default num_clusters is log10(rows) rounded,
// at least 2; RANDOM picks distinct rows, and KMEANS++ the usual D²
// sampling, both with a fixed seed, so the same each run but not Google's
// draw; a centroid left with no rows keeps its place; the loss each
// iteration is the mean squared distance, and early stop compares it as
// BigQuery's GLM training does; the COSINE distance is 1 − cos θ (1 when a
// vector is zero) and its centroids plain means; a NULL number takes the
// mean; PCA's components are signed so their largest-magnitude entry is
// positive, and PCA refuses categorical features (501: whether their
// one-hot columns are centered is undocumented).

// maxPCAColumns is how many encoded columns CloudBurrow decomposes.
const maxPCAColumns = 1000

// mlUnsupervisedFeatures reads every column but skip as a feature.
func mlUnsupervisedFeatures(cols []mlColumn, skip int) ([]mlFeature, []int, *mlFail) {
	var fts []mlFeature
	var fidx []int
	for i, c := range cols {
		if i == skip {
			continue
		}
		ty := mlTypeOf(c.Type)
		if ty == "" {
			return nil, nil, mlUnimplemented("a feature column (%s) of type %s: CloudBurrow trains on INT64, FLOAT64, NUMERIC, BIGNUMERIC, "+
				"STRING and BOOL features only", c.Name, c.Type)
		}
		fts = append(fts, mlFeature{Name: c.Name, Type: ty})
		fidx = append(fidx, i)
	}
	if len(fts) == 0 {
		return nil, nil, mlInvalid("No feature columns found in the input data")
	}
	return fts, fidx, nil
}

// encodeRows encodes rows' features, without the intercept.
func encodeRows(fts []mlFeature, fidx []int, rows [][]*string, scaling string) [][]float64 {
	X := make([][]float64, len(rows))
	for i, row := range rows {
		var x []float64
		for k, ft := range fts {
			x = append(x, encodeFeatureMode(ft, row[fidx[k]], scaling)...)
		}
		X[i] = x
	}
	return X
}

// mlDistance is the distance between a and b by distance type.
func mlDistance(a, b []float64, typ string) float64 {
	if typ == "COSINE" {
		var ab, aa, bb float64
		for i := range a {
			ab += a[i] * b[i]
			aa += a[i] * a[i]
			bb += b[i] * b[i]
		}
		if aa == 0 || bb == 0 {
			return 1
		}
		return 1 - ab/math.Sqrt(aa*bb)
	}
	var s float64
	for i := range a {
		s += (a[i] - b[i]) * (a[i] - b[i])
	}
	return math.Sqrt(s)
}

// nearest is the index of the centroid nearest x, and its distance.
func nearest(x []float64, cs [][]float64, typ string) (int, float64) {
	best, bd := 0, math.Inf(1)
	for k, c := range cs {
		if d := mlDistance(x, c, typ); d < bd {
			best, bd = k, d
		}
	}
	return best, bd
}

func trainKMeans(opts mlOptions, cols []mlColumn, rows [][]*string) (mlTrained, *mlFail) {
	var t mlTrained
	if len(opts.LabelCols) > 0 {
		return t, mlInvalid("input_label_cols is not supported for model type KMEANS")
	}
	ii := -1
	if opts.KMeansInitCol != "" {
		for i, c := range cols {
			if strings.EqualFold(c.Name, opts.KMeansInitCol) {
				ii = i
			}
		}
		if ii < 0 {
			return t, mlInvalid("Unable to find the kmeans_init_col '%s' in the input data", opts.KMeansInitCol)
		}
		if mlTypeOf(cols[ii].Type) != "BOOL" {
			return t, mlInvalid("kmeans_init_col %s must be of type BOOL", cols[ii].Name)
		}
	}
	fts, fidx, fe := mlUnsupervisedFeatures(cols, ii)
	if fe != nil {
		return t, fe
	}
	n := len(rows)
	if n == 0 {
		return t, mlInvalid("Input data doesn't contain any rows")
	}
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	if fe := learnFeatures(fts, fidx, rows, all); fe != nil {
		return t, fe
	}
	t.Features, t.Rows, t.Distance, t.Strategy = fts, n, opts.DistanceType, "KMEANS"
	if !opts.Standardize {
		t.Scaling = "raw"
	}
	X := encodeRows(fts, fidx, rows, t.Scaling)
	k := opts.NumClusters
	if k == 0 {
		k = int(math.Round(math.Log10(float64(n))))
		if k < 2 {
			k = 2
		}
	}
	if k > n {
		return t, mlInvalid("Number of clusters (%d) must not exceed the number of rows in the input data (%d)", k, n)
	}
	rng := rand.New(rand.NewSource(1481))
	var cs [][]float64
	switch opts.KMeansInit {
	case "CUSTOM":
		for i, row := range rows {
			if row[ii] != nil && strings.EqualFold(*row[ii], "true") {
				cs = append(cs, append([]float64(nil), X[i]...))
			}
		}
		if len(cs) != k {
			return t, mlInvalid("kmeans_init_col %s has %d TRUE rows; it must have num_clusters (%d)", opts.KMeansInitCol, len(cs), k)
		}
	case "KMEANS++":
		cs = append(cs, append([]float64(nil), X[rng.Intn(n)]...))
		for len(cs) < k {
			d2 := make([]float64, n)
			var sum float64
			for i, x := range X {
				_, d := nearest(x, cs, opts.DistanceType)
				d2[i] = d * d
				sum += d2[i]
			}
			pick := rng.Intn(n)
			if sum > 0 {
				r := rng.Float64() * sum
				for i := range d2 {
					r -= d2[i]
					if r <= 0 && d2[i] > 0 {
						pick = i
						break
					}
				}
			}
			cs = append(cs, append([]float64(nil), X[pick]...))
		}
	default:
		for _, p := range rng.Perm(n)[:k] {
			cs = append(cs, append([]float64(nil), X[p]...))
		}
	}
	assign := make([]int, n)
	msd := func() float64 {
		var s float64
		for i, x := range X {
			var d float64
			assign[i], d = nearest(x, cs, opts.DistanceType)
			s += d * d
		}
		return s / float64(n)
	}
	prev := msd()
	for iter := 0; iter < opts.MaxIterations; iter++ {
		start := time.Now()
		sums := make([][]float64, k)
		counts := make([]int, k)
		for c := range sums {
			sums[c] = make([]float64, len(X[0]))
		}
		for i, x := range X {
			counts[assign[i]]++
			for j, v := range x {
				sums[assign[i]][j] += v
			}
		}
		for c := range cs {
			if counts[c] == 0 {
				continue
			}
			for j := range cs[c] {
				cs[c][j] = sums[c][j] / float64(counts[c])
			}
		}
		cur := msd()
		t.Iterations = append(t.Iterations, mlIteration{Index: iter, Loss: cur, DurationMs: time.Since(start).Milliseconds()})
		if opts.EarlyStop && prev > 0 && (prev-cur)/prev < opts.MinRelProgress {
			break
		}
		if cur == 0 {
			break
		}
		prev = cur
	}
	sort.Slice(t.Iterations, func(a, b int) bool { return t.Iterations[a].Index > t.Iterations[b].Index })
	t.Centroids = cs
	t.Eval = clusteringMetrics(X, cs, opts.DistanceType)
	t.ClusterSizes = make([]int, k)
	for i := range X {
		t.ClusterSizes[assign[i]]++
	}
	return t, nil
}

// clusteringMetrics are a k-means model's ML.EVALUATE metrics on the
// encoded rows X: the Davies-Bouldin index (the mean over the clusters of
// the largest (sᵢ + sⱼ) / d(cᵢ, cⱼ), s a cluster's mean distance of its
// rows to its centroid; clusters with no rows left out) and the mean
// squared distance of a row to its nearest centroid.
func clusteringMetrics(X [][]float64, cs [][]float64, typ string) map[string]*float64 {
	k := len(cs)
	spread := make([]float64, k)
	counts := make([]int, k)
	var sq float64
	for _, x := range X {
		c, d := nearest(x, cs, typ)
		spread[c] += d
		counts[c]++
		sq += d * d
	}
	var db float64
	used := 0
	for i := 0; i < k; i++ {
		if counts[i] == 0 {
			continue
		}
		used++
		worst := 0.0
		for j := 0; j < k; j++ {
			if j == i || counts[j] == 0 {
				continue
			}
			d := mlDistance(cs[i], cs[j], typ)
			if d == 0 {
				continue
			}
			worst = math.Max(worst, (spread[i]/float64(counts[i])+spread[j]/float64(counts[j]))/d)
		}
		db += worst
	}
	m := map[string]*float64{"mean_squared_distance": f64(sq / float64(len(X)))}
	if used > 1 {
		m["davies_bouldin_index"] = f64(db / float64(used))
	} else {
		m["davies_bouldin_index"] = nil
	}
	return m
}

// clusteringMetricNames are a k-means model's ML.EVALUATE columns.
var clusteringMetricNames = []string{"davies_bouldin_index", "mean_squared_distance"}

func trainPCA(opts mlOptions, cols []mlColumn, rows [][]*string) (mlTrained, *mlFail) {
	var t mlTrained
	if len(opts.LabelCols) > 0 {
		return t, mlInvalid("input_label_cols is not supported for model type PCA")
	}
	fts, fidx, fe := mlUnsupervisedFeatures(cols, -1)
	if fe != nil {
		return t, fe
	}
	for _, ft := range fts {
		if !ft.numeric() {
			return t, mlUnimplemented("a PCA model with a %s feature (%s): whether BigQuery centers its one-hot columns is undocumented; "+
				"CloudBurrow trains PCA on numeric features only", ft.Type, ft.Name)
		}
	}
	n := len(rows)
	if n < 2 {
		return t, mlInvalid("PCA requires at least 2 rows of input data")
	}
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	if fe := learnFeatures(fts, fidx, rows, all); fe != nil {
		return t, fe
	}
	t.Features, t.Rows, t.Strategy = fts, n, "FULL"
	if !opts.ScaleFeatures {
		t.Scaling = "center"
	}
	X := encodeRows(fts, fidx, rows, t.Scaling)
	d := len(X[0])
	if d > maxPCAColumns {
		return t, mlUnimplemented("a PCA model of %d columns: CloudBurrow solves PCA in full, to %d columns", d, maxPCAColumns)
	}
	if opts.NumPC > d || opts.NumPC > n {
		return t, mlInvalid("num_principal_components (%d) must not exceed the feature cardinality (%d) or the number of rows (%d)", opts.NumPC, d, n)
	}
	C := make([][]float64, d)
	for i := range C {
		C[i] = make([]float64, d)
	}
	for _, x := range X {
		for i := 0; i < d; i++ {
			for j := 0; j < d; j++ {
				C[i][j] += x[i] * x[j] / float64(n-1)
			}
		}
	}
	vals, vecs := jacobiEigen(C)
	order := make([]int, d)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return vals[order[a]] > vals[order[b]] })
	var total float64
	for _, v := range vals {
		total += math.Max(v, 0)
	}
	t.TotalVariance = total
	k := opts.NumPC
	if k == 0 {
		var cum float64
		for k < d {
			cum += math.Max(vals[order[k]], 0)
			k++
			if total == 0 || cum/total >= opts.PCAVarRatio {
				break
			}
		}
	}
	for _, o := range order[:k] {
		v := make([]float64, d)
		big := 0
		for i := range v {
			v[i] = vecs[i][o]
			if math.Abs(v[i]) > math.Abs(v[big]) {
				big = i
			}
		}
		if v[big] < 0 {
			for i := range v {
				v[i] = -v[i]
			}
		}
		t.Components = append(t.Components, v)
		t.Eigenvalues = append(t.Eigenvalues, math.Max(vals[o], 0))
	}
	var kept float64
	for _, e := range t.Eigenvalues {
		kept += e
	}
	ratio := math.NaN()
	if total > 0 {
		ratio = kept / total
	}
	t.Eval = map[string]*float64{"total_explained_variance_ratio": f64(ratio)}
	return t, nil
}
