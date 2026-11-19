package bigqueryfront

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// ML.DETECT_ANOMALIES of an ARIMA_PLUS_XREG model (#1766 follow-up).
//
// BigQuery documents one syntax for ARIMA_PLUS and ARIMA_PLUS_XREG
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-detect-anomalies,
// read 2026-11-19): with no input it scores the training data, which
// needs decompose_time_series TRUE; an input (TABLE or query, its column
// names those the model was trained on, so its feature columns too) is
// new data scored against the model; anomaly_prob_threshold is in [0, 1),
// default 0.95; "the anomaly probability at each timestamp is calculated
// using the actual time series data value and the values of the
// predicted time series data and the variance from the model training",
// a point anomalous when it exceeds the threshold, which also sets the
// bounds. The output is the ID column(s), the timestamp, the data,
// is_anomaly, lower_bound, upper_bound and anomaly_probability, followed
// by the input's other columns.
//
// The prediction here is ARIMA_PLUS's (modelarima.go) of the residual
// series plus the regression of the row's features: in the history, the
// residual's one-step fit, seasonal, holiday and step parts plus the
// intercept and the training row's attributions; for new data, the
// residual's forecast at the row's grid point plus the intercept and the
// attributions of the row's own features (a NULL numeric feature its
// training mean, an unseen category 0, as ML.FORECAST does). The
// probability and the bounds are ARIMA_PLUS's: P(|N(0, σ²)| < |y − ŷ|)
// and ŷ ± z σ. UNVERIFIED (docs re-read 2026-11-19): Google does not document how the
// regression enters the predicted value or the variance; the regression's
// own uncertainty is not added to σ, and rows off the grid, after the
// model's horizon or without data are left unscored (NULL), as
// ARIMA_PLUS's are.

// xregDetect is ML.DETECT_ANOMALIES of ARIMA_PLUS_XREG model md.
func (f front) xregDetect(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	a, ts, x := md.Trained.Arima, md.Options.TS, md.Trained.Xreg
	thr, why := arimaLevel(c, "anomaly_prob_threshold", 0.95)
	if why != "" {
		return "", 400, why
	}
	z := normalQuantile(thr)
	idCol, idVal := tsIDDecl(ts, a), func(s arimaSeries) string { return tsIDSQL(a, s) }
	score := func(y, yhat, sd float64) string {
		if math.IsNaN(yhat) || sd <= 0 {
			return "NULL, NULL, NULL, NULL"
		}
		p := math.Erf(math.Abs(y-yhat) / (sd * math.Sqrt2))
		return fmt.Sprintf("%t, %s, %s, %s", p > thr, sqlF(yhat-z*sd), sqlF(yhat+z*sd), sqlF(p))
	}
	tsName, dataName := sqlIdent(ts.TimestampCol), sqlIdent(ts.DataCol)
	if c.input == "" {
		if !ts.Decompose {
			return "", 400, "ML.DETECT_ANOMALIES of the training data requires a model trained with decompose_time_series TRUE"
		}
		var rows []string
		for si, s := range a.Series {
			if len(s.Cands) == 0 {
				continue
			}
			fit := s.Cands[0]
			fitted, _, _ := s.arimaPath(fit, a.Cond, 0)
			for k := range s.Times {
				if !s.Observed[k] {
					continue
				}
				reg := x.Intercept + sum(x.Hist[si][k])
				yhat := fitted[k] + s.seasonalAt(k) + s.holidayAt(k) + reg
				if s.Steps != nil {
					yhat += s.Steps[k]
				}
				y := s.Data[k] + reg
				rows = append(rows, fmt.Sprintf("(%s%s, %s, %s)", idVal(s), sqlTS(s.Times[k]), sqlF(y), score(y, yhat, math.Sqrt(fit.Sigma2))))
			}
		}
		return mlRowsSubquery("STRUCT<"+idCol+tsName+" TIMESTAMP, "+dataName+" FLOAT64, is_anomaly BOOL, lower_bound FLOAT64, "+
			"upper_bound FLOAT64, anomaly_probability FLOAT64>", rows), 0, ""
	}
	qcols, qrows, code, msg := f.queryAllRows(r, q, c.input)
	if code != 0 {
		return "", code, msg
	}
	find := func(name string) int {
		for i, col := range qcols {
			if strings.EqualFold(col.Name, name) {
				return i
			}
		}
		return -1
	}
	ti, di := find(ts.TimestampCol), find(ts.DataCol)
	idx, missing := findTSIDs(qcols, ts)
	if ti < 0 || di < 0 || missing != "" {
		return "", 400, "The input data must have the model's time_series_timestamp_col, time_series_data_col and time_series_id_col"
	}
	var fi []int
	for _, ft := range x.Features {
		i := find(ft.Name)
		if i < 0 {
			return "", 400, "The input data must have the model's feature column " + ft.Name
		}
		fi = append(fi, i)
	}
	tt := strings.ToUpper(qcols[ti].Type)
	if tt == "STRING" {
		tt = "TIMESTAMP"
	}
	type path struct {
		s      arimaSeries
		fc, se []float64
	}
	paths := map[string]path{}
	for _, s := range a.Series {
		if len(s.Cands) == 0 {
			continue
		}
		s = s.withETS()
		key := ""
		if s.ID != nil {
			key = *s.ID
		}
		fc, se, _, _ := s.arimaForecast(ts, a.Cond, ts.Horizon, 0)
		paths[key] = path{s, fc, se}
	}
	var keys []string
	for _, row := range qrows {
		key, ok := rowSeriesKey(row, idx)
		if !ok {
			continue
		}
		p, ok := paths[key]
		if !ok || row[ti] == nil || row[di] == nil {
			continue
		}
		tm, ok := parseTSValue(*row[ti], tt)
		y, err := strconv.ParseFloat(*row[di], 64)
		if !ok || err != nil {
			continue
		}
		last := len(p.s.Times) - 1
		k := gridIndex(p.s.Times[0], a.Frequency, tm) - last - 1
		if k < 0 || k >= ts.Horizon || gridPoint(p.s.Times[0], a.Frequency, last+k+1) != tm {
			continue
		}
		vals := make([]*string, len(fi))
		for j, i := range fi {
			vals[j] = row[i]
		}
		yhat := p.fc[k] + x.Intercept + sum(x.attr(vals))
		keys = append(keys, fmt.Sprintf("(%s%d, %s, %s)", idVal(p.s), tm, sqlF(y), score(y, yhat, p.se[k])))
	}
	return anomalyInputJoin(c.input, qcols, ti, di, idx, ts, a, keys), 0, ""
}

// anomalyInputJoin is a time series model's ML.DETECT_ANOMALIES of an
// input: the input's rows, each with its score from keys (rows of the
// series' ID values, the timestamp in microseconds, the data, is_anomaly,
// lower_bound, upper_bound and anomaly_probability), its IDs (columns
// idx), timestamp and data first and its other columns after.
func anomalyInputJoin(input string, qcols []mlColumn, ti, di int, idx []int, ts tsOptions, a *arimaTrained, keys []string) string {
	tsName, dataName := sqlIdent(ts.TimestampCol), sqlIdent(ts.DataCol)
	isID := map[int]bool{}
	var idSel, idJoin string
	scoredCols := "STRUCT<"
	for j, i := range idx {
		isID[i] = true
		col := sqlIdent(qcols[i].Name)
		idSel += "_cbml_in." + col + ", "
		idJoin += fmt.Sprintf(" AND _cbml_s._cbml_id%d = _cbml_in.%s", j, col)
		scoredCols += fmt.Sprintf("_cbml_id%d %s, ", j, a.idTypes()[j])
	}
	scoredCols += "_cbml_t INT64, _cbml_y FLOAT64, is_anomaly BOOL, lower_bound FLOAT64, upper_bound FLOAT64, anomaly_probability FLOAT64>"
	if len(keys) == 0 {
		keys = nil
	}
	var rest string
	for i, col := range qcols {
		if i != ti && i != di && !isID[i] {
			rest += ", _cbml_in." + sqlIdent(col.Name)
		}
	}
	return "(SELECT " + idSel + "CAST(_cbml_in." + tsName + " AS TIMESTAMP) AS " + tsName + ", CAST(_cbml_in." + dataName + " AS FLOAT64) AS " + dataName +
		", _cbml_s.is_anomaly, _cbml_s.lower_bound, _cbml_s.upper_bound, _cbml_s.anomaly_probability" + rest +
		" FROM (" + input + ") AS _cbml_in LEFT JOIN " + mlRowsSubquery(scoredCols, keys) + " AS _cbml_s ON _cbml_s._cbml_t = UNIX_MICROS(CAST(_cbml_in." + tsName +
		" AS TIMESTAMP)) AND _cbml_s._cbml_y = CAST(_cbml_in." + dataName + " AS FLOAT64)" + idJoin + ")"
}
