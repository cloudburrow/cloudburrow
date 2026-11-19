package bigqueryfront

import "sort"

// category_encoding_method DUMMY_ENCODING and TARGET_ENCODING (#1929,
// https://docs.cloud.google.com/bigquery/docs/auto-preprocessing):
//
//   - DUMMY_ENCODING (LINEAR_REG, LOGISTIC_REG): one-hot encoding with
//     N-1 placeholder columns for N categories, "the category with the
//     most occurrences in the training dataset is dropped"; ML.WEIGHTS
//     "still includes the dropped category, but its weight is always 0.0".
//     A tie is broken by "a random category within that set"; CloudBurrow
//     drops the first of them in sorted order, so a model is reproducible
//     (UNVERIFIED, docs re-read 2026-11-19). NULL is not a category it
//     drops (UNVERIFIED).
//   - TARGET_ENCODING (BOOSTED_TREE_*): the category replaced "with the
//     probability of the target for classification models, or with the
//     expected value of the target for regression models": a binary
//     classifier's share of the training rows of the category whose label
//     is the positive (second sorted) class, a regressor's mean label of
//     them. UNVERIFIED (docs re-read 2026-11-19, unanswered there): no
//     smoothing, as the documented examples show none; NULL and a
//     category training did not see take the training rows' overall value;
//     a multiclass classifier's encoding is not documented and stays 501.

// learnEncoding learns the DUMMY_ENCODING or TARGET_ENCODING of t's
// categorical features over the training rows use of rows (fidx the
// features' columns, y each row's label: a class index or the value).
func learnEncoding(opts mlOptions, t *mlTrained, fidx []int, rows [][]*string, use []int, y []float64) *mlFail {
	switch opts.CategoryEncoding {
	case "DUMMY_ENCODING":
		for k := range t.Features {
			ft := &t.Features[k]
			if ft.numeric() || len(ft.Categories) == 0 {
				continue
			}
			counts := make([]int, len(ft.Categories))
			for _, i := range use {
				if v := rows[i][fidx[k]]; v != nil {
					if j := sort.SearchStrings(ft.Categories, *v); j < len(ft.Categories) && ft.Categories[j] == *v {
						counts[j]++
					}
				}
			}
			best := 0
			for j, n := range counts {
				if n > counts[best] {
					best = j
				}
			}
			ft.Dropped = best + 1
		}
	case "TARGET_ENCODING":
		if len(t.Classes) > 2 {
			return mlUnimplemented("category_encoding_method TARGET_ENCODING of a multiclass model: BigQuery documents the probability " +
				"of the target for a classifier, not which class's a multiclass model encodes. Nothing was run.")
		}
		var all float64
		for _, i := range use {
			all += y[i]
		}
		prior := 0.0
		if len(use) > 0 {
			prior = all / float64(len(use))
		}
		for k := range t.Features {
			ft := &t.Features[k]
			if ft.numeric() {
				continue
			}
			ft.LabelEncoded, ft.TargetEncoded, ft.TargetPrior = false, true, prior
			sum := make([]float64, len(ft.Categories))
			n := make([]int, len(ft.Categories))
			for _, i := range use {
				if v := rows[i][fidx[k]]; v != nil {
					if j := sort.SearchStrings(ft.Categories, *v); j < len(ft.Categories) && ft.Categories[j] == *v {
						sum[j] += y[i]
						n[j]++
					}
				}
			}
			ft.Targets = make([]float64, len(ft.Categories))
			for j := range sum {
				ft.Targets[j] = prior
				if n[j] > 0 {
					ft.Targets[j] = sum[j] / float64(n[j])
				}
			}
		}
	}
	return nil
}

// targetEncode is a TARGET_ENCODING feature's value of v.
func targetEncode(ft mlFeature, v *string) float64 {
	if v != nil {
		if j := sort.SearchStrings(ft.Categories, *v); j < len(ft.Categories) && ft.Categories[j] == *v {
			return ft.Targets[j]
		}
	}
	return ft.TargetPrior
}

// dropDummies makes each DUMMY_ENCODING feature's dropped category's
// weight 0.0 in w, laid out as the encoded row (the intercept first).
func dropDummies(fts []mlFeature, w []float64) {
	j := 1
	for _, ft := range fts {
		if ft.Dropped > 0 {
			w[j+ft.Dropped-1] = 0
		}
		j += ft.dims()
	}
}
