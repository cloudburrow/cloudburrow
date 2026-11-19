package bigqueryfront

import (
	"math"
	"math/big"
	"sort"
	"strings"
)

// seqSplit is data_split_method SEQ: which of rows are evaluation data,
// split by column col (at index si).
//
// BigQuery documents it
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-glm,
// read 2026-11-19): data_split_col is one of NUMERIC, BIGNUMERIC, STRING
// or TIMESTAMP; the data is sorted smallest to largest by it and, without
// hyperparameter tuning (which CloudBurrow does not serve), the last n
// rows, n from data_split_eval_fraction, are evaluation data and the rest
// training data; "all rows with split values smaller than the threshold,
// including NULLs, are used as training data", the remaining rows
// evaluation data. The split column is not a feature.
//
// UNVERIFIED (docs re-read 2026-11-19), as the page does not say: n is
// round(fraction × rows); the threshold is the split value of the n-th
// row from the end, so rows tied with it are all evaluation data; NULLs
// sort first and are always training data; STRING compares by bytes, and
// NUMERIC and BIGNUMERIC exactly.
func seqSplit(col mlColumn, rows [][]*string, si int, frac float64) ([]bool, *mlFail) {
	typ := strings.ToUpper(col.Type)
	var less func(a, b string) bool
	switch typ {
	case "STRING":
		less = func(a, b string) bool { return a < b }
	case "NUMERIC", "BIGNUMERIC":
		less = func(a, b string) bool {
			x, okx := new(big.Rat).SetString(a)
			y, oky := new(big.Rat).SetString(b)
			if !okx || !oky {
				return a < b
			}
			return x.Cmp(y) < 0
		}
	case "TIMESTAMP":
		less = func(a, b string) bool {
			x, _ := parseTSValue(a, "TIMESTAMP")
			y, _ := parseTSValue(b, "TIMESTAMP")
			return x < y
		}
	default:
		return nil, mlInvalid("Data split column %s has type %s; data_split_method SEQ requires a NUMERIC, BIGNUMERIC, STRING or TIMESTAMP column", col.Name, col.Type)
	}
	isEval := make([]bool, len(rows))
	n := int(math.Round(frac * float64(len(rows))))
	if n <= 0 {
		return isEval, nil
	}
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := rows[order[i]][si], rows[order[j]][si]
		if a == nil || b == nil {
			return a == nil && b != nil
		}
		return less(*a, *b)
	})
	th := rows[order[len(order)-min(n, len(order))]][si]
	for i, row := range rows {
		v := row[si]
		if v == nil {
			continue
		}
		isEval[i] = th == nil || !less(*v, *th)
	}
	return isEval, nil
}
