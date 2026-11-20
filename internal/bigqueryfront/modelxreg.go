package bigqueryfront

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ARIMA_PLUS_XREG time series models (#1766,
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-multivariate-time-series),
// which BigQuery documents as "an ARIMA_PLUS model with linear external
// regressors": every column of the training query other than the
// timestamp, data and ID columns is a feature; a NULL feature value is
// the column's mean over every series, as documented; l2_reg is the
// linear regression's L2 regularization. ML.FORECAST and
// ML.EXPLAIN_FORECAST take the future features as their (required)
// input, ML.EVALUATE the future features and data, and
// ML.EXPLAIN_FORECAST answers each feature's attribution_<feature>,
// weight × value, as documented, which is added to time_series_data and
// time_series_adjusted_data. ML.ARIMA_COEFFICIENTS answers processed_input,
// weight and category_weights besides the ARIMA coefficients.
//
// UNVERIFIED (docs re-read 2026-11-17, which say neither how the
// regressors enter the fit nor in which order): the regression is fitted
// first, by ridge least squares of l2_reg (0 when not given) with an
// unregularized intercept over every training row of every series, and
// the ARIMA_PLUS pipeline (modelarima.go) is then trained on each row's
// residual; the intercept is reported as processed_input __INTERCEPT__
// and is part of ML.EXPLAIN_FORECAST's trend; a STRING or BOOL feature is
// one-hot (its category_weights, weight NULL), a category not seen in
// training contributing 0; a feature's attribution between training rows
// is interpolated as the data is; ML.FORECAST and ML.EXPLAIN_FORECAST
// forecast only the horizon's points whose timestamp the input has (on
// the model's grid), a NULL future feature taken as its training mean;
// ML.DETECT_ANOMALIES is modelxreganomaly.go's. Custom
// holidays (modelholiday.go, #1776) are a stage of the residuals'
// ARIMA_PLUS pipeline, their columns before the attributions.

// xregFeature is one external regressor.
type xregFeature struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Mean is a numeric feature's training mean, which stands for NULL.
	Mean float64 `json:"mean"`
	// Categories are a STRING or BOOL feature's values, one weight each;
	// a numeric feature has one weight.
	Categories []string  `json:"categories,omitempty"`
	Weights    []float64 `json:"weights"`
}

func (x xregFeature) numeric() bool { return x.Categories == nil }

// xregTrained is an ARIMA_PLUS_XREG model's regression; its ARIMA is
// mlTrained.Arima, fitted to the residuals.
type xregTrained struct {
	Features  []xregFeature `json:"features"`
	Intercept float64       `json:"intercept"`
	// Hist is each series' (in Arima.Series order) attributions on its
	// grid: Hist[s][k][feature].
	Hist [][][]float64 `json:"hist"`
}

// attr is each feature's attribution of a row's raw values.
func (x *xregTrained) attr(vals []*string) []float64 {
	out := make([]float64, len(x.Features))
	for j, ft := range x.Features {
		v := vals[j]
		if ft.numeric() {
			f := ft.Mean
			if v != nil {
				if p, err := strconv.ParseFloat(*v, 64); err == nil {
					f = p
				}
			}
			out[j] = ft.Weights[0] * f
			continue
		}
		if v == nil {
			continue
		}
		for c, name := range ft.Categories {
			if name == *v {
				out[j] = ft.Weights[c]
			}
		}
	}
	return out
}

func sum(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s
}

// trainXreg trains an ARIMA_PLUS_XREG model of opts on rows.
func trainXreg(opts mlOptions, cols []mlColumn, rows [][]*string) (mlTrained, *mlFail) {
	var t mlTrained
	ts := opts.TS
	ti, di := -1, -1
	idx, missing := findTSIDs(cols, ts)
	isID := map[int]bool{}
	for _, i := range idx {
		isID[i] = true
	}
	var fi []int
	for i, c := range cols {
		switch {
		case strings.EqualFold(c.Name, ts.TimestampCol):
			ti = i
		case strings.EqualFold(c.Name, ts.DataCol):
			di = i
		case isID[i]:
		default:
			fi = append(fi, i)
		}
	}
	if ti < 0 || di < 0 || missing != "" {
		return trainArima(opts, cols, rows) // its error names the column
	}
	if len(fi) == 0 {
		return t, mlInvalid("An ARIMA_PLUS_XREG model needs at least one feature column besides time_series_timestamp_col, time_series_data_col and time_series_id_col")
	}
	x := &xregTrained{}
	for _, i := range fi {
		ty := mlTypeOf(cols[i].Type)
		ft := xregFeature{Name: cols[i].Name, Type: ty}
		switch ty {
		case "INT64", "FLOAT64", "NUMERIC", "BIGNUMERIC":
			var s float64
			n := 0
			for _, row := range rows {
				if row[i] != nil {
					if v, err := strconv.ParseFloat(*row[i], 64); err == nil {
						s, n = s+v, n+1
					}
				}
			}
			if n > 0 {
				ft.Mean = s / float64(n)
			}
			ft.Weights = []float64{0}
		case "STRING", "BOOL":
			seen := map[string]bool{}
			for _, row := range rows {
				if row[i] != nil && !seen[*row[i]] {
					seen[*row[i]] = true
					ft.Categories = append(ft.Categories, *row[i])
				}
			}
			sort.Strings(ft.Categories)
			if ft.Categories == nil {
				ft.Categories = []string{}
			}
			ft.Weights = make([]float64, len(ft.Categories))
		default:
			return t, mlUnimplemented("feature %s of type %s of an ARIMA_PLUS_XREG model: CloudBurrow takes numeric, STRING and BOOL features", cols[i].Name, cols[i].Type)
		}
		x.Features = append(x.Features, ft)
	}
	// The design: [1, features…], one column a numeric feature and one a
	// category.
	width := 1
	for _, ft := range x.Features {
		width += len(ft.Weights)
	}
	design := func(row []*string) []float64 {
		z := make([]float64, 0, width)
		z = append(z, 1)
		for j, i := range fi {
			ft := x.Features[j]
			if ft.numeric() {
				v := ft.Mean
				if row[i] != nil {
					if p, err := strconv.ParseFloat(*row[i], 64); err == nil {
						v = p
					}
				}
				z = append(z, v)
				continue
			}
			for _, c := range ft.Categories {
				if row[i] != nil && *row[i] == c {
					z = append(z, 1)
				} else {
					z = append(z, 0)
				}
			}
		}
		return z
	}
	A := make([][]float64, width)
	b := make([]float64, width)
	for a := range A {
		A[a] = make([]float64, width)
		if a > 0 {
			A[a][a] = opts.L2
		}
	}
	used := 0
	for _, row := range rows {
		if row[ti] == nil || row[di] == nil {
			continue
		}
		y, err := strconv.ParseFloat(*row[di], 64)
		if err != nil {
			continue
		}
		z := design(row)
		for a := range z {
			b[a] += z[a] * y
			for c := range z {
				A[a][c] += z[a] * z[c]
			}
		}
		used++
	}
	if used == 0 {
		return t, mlInvalid("Input data doesn't contain any rows")
	}
	w := solveLinear(A, b)
	x.Intercept = w[0]
	k := 1
	for j := range x.Features {
		for c := range x.Features[j].Weights {
			x.Features[j].Weights[c] = w[k]
			k++
		}
	}
	// The ARIMA_PLUS pipeline on the residuals.
	featVals := func(row []*string) []*string {
		out := make([]*string, len(fi))
		for j, i := range fi {
			out[j] = row[i]
		}
		return out
	}
	resid := make([][]*string, 0, len(rows))
	for _, row := range rows {
		nr := append([]*string(nil), row...)
		if row[di] != nil {
			if y, err := strconv.ParseFloat(*row[di], 64); err == nil {
				v := strconv.FormatFloat(y-x.Intercept-sum(x.attr(featVals(row))), 'g', -1, 64)
				nr[di] = &v
			}
		}
		resid = append(resid, nr)
	}
	rc := append([]mlColumn(nil), cols...)
	rc[di].Type = "FLOAT64"
	t, fe := trainArima(opts, rc, resid)
	if fe != nil {
		return t, fe
	}
	// Each series' attributions on its grid: averaged over the rows of a
	// point, interpolated between them.
	a := t.Arima
	tt := strings.ToUpper(cols[ti].Type)
	for _, s := range a.Series {
		n := len(s.Times)
		acc := make([][]float64, n)
		cnt := make([]int, n)
		for _, row := range rows {
			if row[ti] == nil || row[di] == nil {
				continue
			}
			if len(idx) > 0 {
				if key, ok := rowSeriesKey(row, idx); !ok || s.ID == nil || key != *s.ID {
					continue
				}
			}
			tm, ok := parseTSValue(*row[ti], tt)
			if !ok {
				continue
			}
			g := gridIndex(s.Times[0], a.Frequency, tm)
			if g < 0 || g >= n {
				continue
			}
			v := x.attr(featVals(row))
			if acc[g] == nil {
				acc[g] = make([]float64, len(v))
			}
			for j := range v {
				acc[g][j] += v[j]
			}
			cnt[g]++
		}
		hist := make([][]float64, n)
		for g := range hist {
			if cnt[g] > 0 {
				hist[g] = acc[g]
				for j := range hist[g] {
					hist[g][j] /= float64(cnt[g])
				}
			}
		}
		for g := 0; g < n; g++ {
			if hist[g] != nil {
				continue
			}
			l, r := g-1, g+1
			for r < n && hist[r] == nil {
				r++
			}
			for m := g; m < r; m++ {
				hist[m] = make([]float64, len(x.Features))
				for j := range hist[m] {
					switch {
					case l >= 0 && r < n:
						hist[m][j] = hist[l][j] + (hist[r][j]-hist[l][j])*float64(m-l)/float64(r-l)
					case l >= 0:
						hist[m][j] = hist[l][j]
					case r < n:
						hist[m][j] = hist[r][j]
					}
				}
			}
			g = r
		}
		x.Hist = append(x.Hist, hist)
	}
	t.Xreg = x
	for _, ft := range x.Features {
		t.Features = append(t.Features, mlFeature{Name: ft.Name, Type: ft.Type})
	}
	return t, nil
}

// tsFuture is an input's rows of one series on its grid after the
// training: each point's features' attributions (XREG) and data.
type tsFuture struct {
	attr map[int][]float64
	data map[int]float64
}

// tsInput reads an input of time series model md: each series' (keyed by
// its ID) rows on the grid after its training, at most h points ahead.
// needData asks for the data column.
func (f front) tsInput(r *http.Request, q queryOptions, md bqModel, input string, h int, needData bool) (map[string]*tsFuture, int, string) {
	a, ts, x := md.Trained.Arima, md.Options.TS, md.Trained.Xreg
	qcols, qrows, code, msg := f.queryAllRows(r, q, input)
	if code != 0 {
		return nil, code, msg
	}
	find := func(name string) int {
		for i, c := range qcols {
			if strings.EqualFold(c.Name, name) {
				return i
			}
		}
		return -1
	}
	ti, di := find(ts.TimestampCol), find(ts.DataCol)
	idx, missing := findTSIDs(qcols, ts)
	switch {
	case ti < 0:
		return nil, 400, "The input data must have the model's time_series_timestamp_col " + ts.TimestampCol
	case needData && di < 0:
		return nil, 400, "The input data must have the model's time_series_data_col " + ts.DataCol
	case missing != "":
		return nil, 400, "The input data must have the model's time_series_id_col " + missing
	}
	var fi []int
	if x != nil {
		for _, ft := range x.Features {
			i := find(ft.Name)
			if i < 0 {
				return nil, 400, "The input data must have the model's feature column " + ft.Name
			}
			fi = append(fi, i)
		}
	}
	tt := strings.ToUpper(qcols[ti].Type)
	if tt == "STRING" {
		tt = "TIMESTAMP"
	}
	series := map[string]arimaSeries{}
	for _, s := range a.Series {
		key := ""
		if s.ID != nil {
			key = *s.ID
		}
		series[key] = s
	}
	out := map[string]*tsFuture{}
	for _, row := range qrows {
		key, ok := rowSeriesKey(row, idx)
		if !ok {
			continue
		}
		s, ok := series[key]
		if !ok || row[ti] == nil {
			continue
		}
		tm, ok := parseTSValue(*row[ti], tt)
		if !ok {
			continue
		}
		last := len(s.Times) - 1
		k := gridIndex(s.Times[0], a.Frequency, tm) - last - 1
		if k < 0 || k >= h || gridPoint(s.Times[0], a.Frequency, last+k+1) != tm {
			continue
		}
		fu := out[key]
		if fu == nil {
			fu = &tsFuture{attr: map[int][]float64{}, data: map[int]float64{}}
			out[key] = fu
		}
		if _, dup := fu.attr[k]; dup {
			continue // the first row of a point
		}
		if x != nil {
			vals := make([]*string, len(fi))
			for j, i := range fi {
				vals[j] = row[i]
			}
			fu.attr[k] = x.attr(vals)
		} else {
			fu.attr[k] = nil
		}
		if di >= 0 && row[di] != nil {
			if y, err := strconv.ParseFloat(*row[di], 64); err == nil {
				fu.data[k] = y
			}
		}
	}
	return out, 0, ""
}

// xregSubquery is the subquery that stands for call c of ARIMA_PLUS_XREG
// model md.
func (f front) xregSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	a, ts, x := md.Trained.Arima, md.Options.TS, md.Trained.Xreg
	idCol, idVal := tsIDDecl(ts, a), func(s arimaSeries) string { return tsIDSQL(a, s) }
	key := func(s arimaSeries) string {
		if s.ID != nil {
			return *s.ID
		}
		return ""
	}
	switch c.fn {
	case "EVALUATE":
		return f.tsEvaluate(r, q, md, c)
	case "ARIMA_EVALUATE", "HOLIDAY_INFO":
		return f.arimaSubquery(r, q, md, c)
	case "DETECT_ANOMALIES":
		return f.xregDetect(r, q, md, c) // modelxreganomaly.go
	case "ARIMA_COEFFICIENTS":
		if c.input != "" {
			return "", 400, "ML.ARIMA_COEFFICIENTS takes no input data"
		}
		arr := func(v []float64) string {
			var out []string
			for _, y := range v {
				out = append(out, sqlF(y))
			}
			return "ARRAY<FLOAT64>[" + strings.Join(out, ", ") + "]"
		}
		var rows []string
		for _, s := range a.Series {
			if len(s.Cands) == 0 {
				continue
			}
			fit := s.Cands[0]
			pre := idVal(s) + arr(fit.AR) + ", " + arr(fit.MA) + ", " + sqlF(fit.Mu) + ", "
			rows = append(rows, "("+pre+"'__INTERCEPT__', "+sqlF(x.Intercept)+", [])")
			for _, ft := range x.Features {
				if ft.numeric() {
					rows = append(rows, "("+pre+sqlString(ft.Name)+", "+sqlF(ft.Weights[0])+", [])")
					continue
				}
				var cw []string
				for k, name := range ft.Categories {
					cw = append(cw, "("+sqlString(name)+", "+sqlF(ft.Weights[k])+")")
				}
				rows = append(rows, "("+pre+sqlString(ft.Name)+", NULL, ["+strings.Join(cw, ", ")+"])")
			}
		}
		return mlRowsSubquery("STRUCT<"+idCol+"ar_coefficients ARRAY<FLOAT64>, ma_coefficients ARRAY<FLOAT64>, intercept_or_drift FLOAT64, "+
			"processed_input STRING, weight FLOAT64, category_weights ARRAY<STRUCT<category STRING, weight FLOAT64>>>", rows), 0, ""
	case "FORECAST", "EXPLAIN_FORECAST":
	default:
		return "", 400, "ML." + c.fn + " is not supported for model type ARIMA_PLUS_XREG"
	}
	if c.input == "" {
		return "", 400, "ML." + c.fn + " of an ARIMA_PLUS_XREG model requires its future features as input: a query in parentheses or TABLE name"
	}
	h, why := arimaHorizon(c, ts.Horizon)
	if why != "" {
		return "", 400, why
	}
	level, why := arimaLevel(c, "confidence_level", 0.95)
	if why != "" {
		return "", 400, why
	}
	fut, code, msg := f.tsInput(r, q, md, c.input, h, false)
	if code != 0 {
		return "", code, msg
	}
	z := normalQuantile(level)
	names := holidayNames(md.holidays()) // #1776
	var attrCols []string
	for _, ft := range x.Features {
		attrCols = append(attrCols, sqlIdent("attribution_"+ft.Name)+" FLOAT64")
	}
	attrVals := func(v []float64) string {
		var out []string
		for _, y := range v {
			out = append(out, sqlF(y))
		}
		return strings.Join(out, ", ")
	}
	var rows []string
	for si, s := range a.Series {
		if len(s.Cands) == 0 {
			continue
		}
		s = s.withETS()
		fu := fut[key(s)]
		fit := s.Cands[0]
		fitted, _, _ := s.arimaPath(fit, a.Cond, 0)
		fc, se, lo, hi := s.arimaForecast(ts, a.Cond, h, z)
		last := len(s.Times) - 1
		seasonCols := func(k int) (string, float64) {
			var out []string
			var total float64
			for _, name := range []string{"YEARLY", "QUARTERLY", "MONTHLY", "WEEKLY", "DAILY"} {
				v := "NULL"
				for i, p := range s.Seasons {
					if p.Name == name {
						y := s.seasonalPart(i, k)
						v, total = sqlF(y), total+y
					}
				}
				out = append(out, v)
			}
			return strings.Join(out, ", "), total
		}
		if c.fn == "EXPLAIN_FORECAST" {
			trend := smooth(fitted, ts.SmoothWindow)
			sigma := math.Sqrt(fit.Sigma2)
			for k := range s.Times {
				seas, ssum := seasonCols(k)
				at := x.Hist[si][k]
				reg := x.Intercept + sum(at)
				spikes, steps, other := "NULL", "NULL", 0.0
				if s.Spikes != nil {
					spikes, other = sqlF(s.Spikes[k]), other+s.Spikes[k]
				}
				if s.Steps != nil {
					steps, other = sqlF(s.Steps[k]), other+s.Steps[k]
				}
				data := s.Data[k] + reg
				hol, each := s.holidayCols(names, k)
				res, adj, tr := math.NaN(), data-other, math.NaN()
				if !math.IsNaN(trend[k]) {
					tr = trend[k] + x.Intercept
					adj = trend[k] + ssum + s.holidayAt(k) + reg
					res = data - adj - other
				}
				rows = append(rows, fmt.Sprintf("(%s%s, 'history', %s, %s, %s, NULL, NULL, NULL, %s, %s, %s, %s, %s, %s%s, %s)",
					idVal(s), sqlTS(s.Times[k]), sqlF(data), sqlF(adj), sqlF(sigma), sqlNaN(tr), seas, hol, spikes, steps, sqlNaN(res), joinLead(each), attrVals(at)))
			}
		}
		if fu == nil {
			continue
		}
		for k := 0; k < h; k++ {
			at, ok := fu.attr[k]
			if !ok {
				continue
			}
			reg := x.Intercept + sum(at)
			tm := gridPoint(s.Times[0], a.Frequency, last+k+1)
			v, l, u := fc[k]+reg, lo[k]+reg, hi[k]+reg
			if c.fn == "FORECAST" {
				rows = append(rows, fmt.Sprintf("(%s%s, %s, %s, %s, %s, %s, %s, %s)", idVal(s), sqlTS(tm), sqlF(v), sqlF(se[k]), sqlF(level),
					sqlF(l), sqlF(u), sqlF(l), sqlF(u)))
				continue
			}
			seas, ssum := seasonCols(last + k + 1)
			hol, each := s.holidayCols(names, last+k+1)
			rows = append(rows, fmt.Sprintf("(%s%s, 'forecast', %s, %s, %s, %s, %s, %s, %s, %s, %s, NULL, NULL, NULL%s, %s)",
				idVal(s), sqlTS(tm), sqlF(v), sqlF(v), sqlF(se[k]), sqlF(level), sqlF(l), sqlF(u), sqlF(fc[k]-ssum-s.holidayAt(last+k+1)+x.Intercept), seas, hol, joinLead(each), attrVals(at)))
		}
	}
	if c.fn == "FORECAST" {
		return mlRowsSubquery("STRUCT<"+idCol+"forecast_timestamp TIMESTAMP, forecast_value FLOAT64, standard_error FLOAT64, confidence_level FLOAT64, "+
			"prediction_interval_lower_bound FLOAT64, prediction_interval_upper_bound FLOAT64, confidence_interval_lower_bound FLOAT64, "+
			"confidence_interval_upper_bound FLOAT64>", rows), 0, ""
	}
	return mlRowsSubquery("STRUCT<"+idCol+"time_series_timestamp TIMESTAMP, time_series_type STRING, time_series_data FLOAT64, "+
		"time_series_adjusted_data FLOAT64, standard_error FLOAT64, confidence_level FLOAT64, prediction_interval_lower_bound FLOAT64, "+
		"prediction_interval_upper_bound FLOAT64, trend FLOAT64, seasonal_period_yearly FLOAT64, seasonal_period_quarterly FLOAT64, "+
		"seasonal_period_monthly FLOAT64, seasonal_period_weekly FLOAT64, seasonal_period_daily FLOAT64, holiday_effect FLOAT64, "+
		"spikes_and_dips FLOAT64, step_changes FLOAT64, residual FLOAT64"+holidayColumnTypes(names)+", "+strings.Join(attrCols, ", ")+">", rows), 0, ""
}

// tsEvaluate is ML.EVALUATE of an ARIMA_PLUS or ARIMA_PLUS_XREG model
// against an input of the series' later data (and an XREG model's
// features), with the documented options perform_aggregation (default
// TRUE), horizon (default the model's) and confidence_level (default
// 0.95): each row's forecast error, or per series the
// documented aggregate errors.
//
// UNVERIFIED (docs re-read 2026-11-17:
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-evaluate,
// which names the columns but not their computation): only the input's
// rows on the model's grid after the training and within the horizon are
// scored; absolute_percentage_error and the mean_absolute_percentage_error
// are percentages, symmetric_mean_absolute_percentage_error the mean of
// 200 |y − ŷ| / (|y| + |ŷ|); mean_absolute_scaled_error the MAE over the
// training series' mean absolute one-step change; the per-row columns are
// named after the model's timestamp and data columns, the forecast
// forecasted_<data column>, and lower_bound and upper_bound are the
// prediction interval's; a series with no row scored is left out; with
// no input the ARIMA_PLUS model's ML.EVALUATE is 400 as before.
func (f front) tsEvaluate(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	a, ts, x := md.Trained.Arima, md.Options.TS, md.Trained.Xreg
	if c.input == "" {
		return "", 400, "ML.EVALUATE of a time series model requires the series' later data as input: a query in parentheses or TABLE name"
	}
	for k := range c.struc {
		switch k {
		case "perform_aggregation", "horizon", "confidence_level":
		default:
			return "", 400, "Unknown option " + k + " of ML.EVALUATE"
		}
	}
	agg := true
	if v, ok := c.struc["perform_aggregation"]; ok {
		switch strings.ToUpper(v) {
		case "TRUE":
		case "FALSE":
			agg = false
		default:
			return "", 400, "Invalid perform_aggregation " + v + ": it must be TRUE or FALSE"
		}
	}
	h := ts.Horizon
	if v, ok := c.struc["horizon"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return "", 400, "Invalid horizon " + v + ": it must be a positive integer"
		}
		h = n
	}
	level, why := arimaLevel(c, "confidence_level", 0.95)
	if why != "" {
		return "", 400, why
	}
	z := normalQuantile(level)
	fut, code, msg := f.tsInput(r, q, md, c.input, h, true)
	if code != 0 {
		return "", code, msg
	}
	idCol, idVal := tsIDDecl(ts, a), func(s arimaSeries) string { return tsIDSQL(a, s) }
	var rows []string
	for si, s := range a.Series {
		if len(s.Cands) == 0 {
			continue
		}
		key := ""
		if s.ID != nil {
			key = *s.ID
		}
		fu := fut[key]
		if fu == nil {
			continue
		}
		s = s.withETS()
		fc, _, lo, hi := s.arimaForecast(ts, a.Cond, h, z)
		hist := func(k int) float64 {
			v := s.Data[k]
			if x != nil {
				v += x.Intercept + sum(x.Hist[si][k])
			}
			return v
		}
		var naive float64
		for k := 1; k < len(s.Data); k++ {
			naive += math.Abs(hist(k) - hist(k-1))
		}
		if len(s.Data) > 1 {
			naive /= float64(len(s.Data) - 1)
		}
		var ks []int
		for k := range fu.data {
			ks = append(ks, k)
		}
		sort.Ints(ks)
		var ae, se, ape, sape float64
		nape := 0
		last := len(s.Times) - 1
		for _, k := range ks {
			y := fu.data[k]
			reg := 0.0
			if x != nil {
				reg = x.Intercept + sum(fu.attr[k])
			}
			p, l, u := fc[k]+reg, lo[k]+reg, hi[k]+reg
			e := math.Abs(y - p)
			ae, se = ae+e, se+e*e
			pe := "NULL"
			if y != 0 {
				v := 100 * e / math.Abs(y)
				pe, ape, nape = sqlF(v), ape+v, nape+1
			}
			if d := math.Abs(y) + math.Abs(p); d > 0 {
				sape += 200 * e / d
			}
			if !agg {
				rows = append(rows, fmt.Sprintf("(%s%s, %s, %s, %s, %s, %s, %s)", idVal(s), sqlTS(gridPoint(s.Times[0], a.Frequency, last+k+1)),
					sqlF(y), sqlF(p), sqlF(l), sqlF(u), sqlF(e), pe))
			}
		}
		if agg && len(ks) > 0 {
			n := float64(len(ks))
			mape, mase := "NULL", "NULL"
			if nape > 0 {
				mape = sqlF(ape / float64(nape))
			}
			if naive > 0 {
				mase = sqlF(ae / n / naive)
			}
			rows = append(rows, fmt.Sprintf("(%s%s, %s, %s, %s, %s, %s)", idVal(s), sqlF(ae/n), sqlF(se/n), sqlF(math.Sqrt(se/n)), mape, sqlF(sape/n), mase))
		}
	}
	if agg {
		return mlRowsSubquery("STRUCT<"+idCol+"mean_absolute_error FLOAT64, mean_squared_error FLOAT64, root_mean_squared_error FLOAT64, "+
			"mean_absolute_percentage_error FLOAT64, symmetric_mean_absolute_percentage_error FLOAT64, mean_absolute_scaled_error FLOAT64>", rows), 0, ""
	}
	return mlRowsSubquery("STRUCT<"+idCol+sqlIdent(ts.TimestampCol)+" TIMESTAMP, "+sqlIdent(ts.DataCol)+" FLOAT64, "+sqlIdent("forecasted_"+ts.DataCol)+
		" FLOAT64, lower_bound FLOAT64, upper_bound FLOAT64, absolute_error FLOAT64, absolute_percentage_error FLOAT64>", rows), 0, ""
}
