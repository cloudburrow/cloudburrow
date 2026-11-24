package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/tfconvert"
)

// exportTransformModel is a model whose TRANSFORM is items over the
// columns of exportTransformRows.
func exportTransformModel(items []string) bqModel {
	return bqModel{Project: "p", Dataset: "d", ID: "m", Type: "TRANSFORM_ONLY", Transform: items, Inputs: []mlColumn{
		{Name: "x", Type: "FLOAT"}, {Name: "n", Type: "INTEGER"}, {Name: "s", Type: "STRING"}, {Name: "s2", Type: "STRING"},
		{Name: "d", Type: "DATE"}, {Name: "dt", Type: "DATETIME"}, {Name: "ts", Type: "TIMESTAMP"}, {Name: "b", Type: "BOOLEAN"},
		{Name: "arr", Type: "ARRAY<STRING>"}, {Name: "sp", Type: "ARRAY<RECORD>"}}}
}

// exportTransformRows are two rows of the preprocessing model's inputs, as
// the export page's "Supported data types" table writes them; the first x
// is NULL.
var exportTransformRows = map[string][]any{
	"x": {transformNullFloat, 2.5}, "n": {-3, 7}, "s": {"abc", "Hello World"}, "s2": {"a", "b"},
	"d": {"2021-12-31", "2020-02-29"}, "dt": {"2021-01-01 00:00:00.000000", "2023-02-02 02:02:01.152903"},
	"ts": {"2021-01-01 00:00:00.0 +0000", "2017-02-28 20:30:30.4 +0000"}, "b": {false, true},
	// The ARRAY inputs, which no output reads: arr as the page's table gives
	// it, sp the SparseTensor of tf.sparse.from_dense([[0, 1.0, 1.0, 0],
	// [0, 0, 1.0, 1.0]]), its indices, values and dense shape.
	"arr": {[]string{"a", "b"}, []string{"c", "d"}},
	"sp":  {[]int{0, 1}, []int{0, 2}, []int{1, 2}, []int{1, 3}}, "sp_1": {1.0, 1.0, 1.0, 1.0}, "sp_2": {2, 4},
}

// exportTransformCases are TRANSFORM items, as the front keeps them (the
// ML functions written out as SQL), and what GoogleSQL computes for each
// row of exportTransformRows (nil NULL).
func exportTransformCases() []struct {
	item string
	want []any
} {
	bucketize, _, _ := mlPreprocessSQL("BUCKETIZE", []string{"x", "[1.0, 2.0, 3.0]"}, "", nil)
	quantile, _, _ := mlQuantileSQL("QUANTILE_BUCKETIZE", []string{"n", "2"}, "()", func(string, []float64) (mlStats, int, string) {
		v := 0.0
		return mlStats{quant: []*float64{&v}}, 0, ""
	})
	return []struct {
		item string
		want []any
	}{
		{"SAFE_DIVIDE(CAST((x) AS FLOAT64) - 1.5, 2.0) AS std", []any{nil, 0.5}},
		{"IFNULL(CAST((x) AS FLOAT64), 3.25) AS imp", []any{3.25, 2.5}},
		{"GREATEST(0.0, LEAST(1.0, SAFE_DIVIDE(CAST((x) AS FLOAT64) - 1.0, 2.0))) AS mm", []any{nil, 0.75}},
		{bucketize + " AS bk", []any{nil, "bin_3"}},
		{quantile + " AS qb", []any{"bin_1", "bin_2"}},
		{mlHashBucketizeSQL("s2", "3") + " AS hb", []any{int64(0), int64(1)}},
		{mlIndexSQL("ARRAY<STRING>['abc', 'zzz']", "(s)") + " AS le", []any{int64(1), int64(0)}},
		{"n * 2 + MOD(n, 3) AS m", []any{int64(-6), int64(15)}},
		{"n / 2 AS half", []any{-1.5, 3.5}},
		{"LOWER(s) AS lo", []any{"abc", "hello world"}},
		{"UPPER(SUBSTR(s, 2, 3)) AS sub", []any{"BC", "ELL"}},
		{"REGEXP_REPLACE(s, r'o', '0') AS rr", []any{"abc", "Hell0 W0rld"}},
		{"CONCAT(s, '-', CAST(n AS STRING)) AS cc", []any{"abc--3", "Hello World-7"}},
		{"LENGTH(s) AS ln", []any{int64(3), int64(11)}},
		{"SPLIT(s, ' ')[SAFE_OFFSET(1)] AS sp", []any{nil, "World"}},
		{"TRIM('  ' || s || ' ') AS tr", []any{"abc", "Hello World"}},
		{"EXTRACT(DAYOFWEEK FROM d) AS dw", []any{int64(6), int64(7)}},
		{"DATE_ADD(d, INTERVAL 1 YEAR) AS dy", []any{"2022-12-31", "2021-02-28"}},
		{"DATE_DIFF(d, DATE '2020-01-01', MONTH) AS dm", []any{int64(23), int64(1)}},
		{"UNIX_DATE(d) AS ud", []any{int64(18992), int64(18321)}},
		{"FORMAT_DATE('%Y/%m/%d %a', d) AS fd", []any{"2021/12/31 Fri", "2020/02/29 Sat"}},
		{"PARSE_DATE('%d/%m/%Y', '25/12/2008') AS pd", []any{"2008-12-25", "2008-12-25"}},
		{"DATETIME_TRUNC(dt, HOUR) AS dtt", []any{"2021-01-01 00:00:00.000000", "2023-02-02 02:00:00.000000"}},
		{"EXTRACT(MICROSECOND FROM dt) AS us", []any{int64(0), int64(152903)}},
		{"EXTRACT(HOUR FROM ts) AS hh", []any{int64(0), int64(20)}},
		{"TIMESTAMP_DIFF(ts, TIMESTAMP '2017-01-01 00:00:00+00', DAY) AS td", []any{int64(1461), int64(58)}},
		{"IF(b, 'yes', 'no') AS yn", []any{"no", "yes"}},
		{"CASE WHEN n > 0 THEN 'pos' WHEN n < 0 THEN 'neg' ELSE 'zero' END AS sg", []any{"neg", "pos"}},
		{"CAST(x AS STRING) AS xs", []any{nil, "2.5"}},
		{"ROUND(x) AS rx", []any{nil, 3.0}},
		{"n IN (7, 8) AS inn", []any{false, true}},
		{"x BETWEEN 1 AND 3 OR b AS bt", []any{nil, true}},
		{"SQRT(ABS(n)) + POW(2, 3) AS mth", []any{math.Sqrt(3) + 8, math.Sqrt(7) + 8}},
		// Time zones: the tz database's offsets, DST included.
		{"FORMAT_TIMESTAMP('%F %H:%M %Ez %z %Z', ts, 'America/New_York') AS fz", []any{"2020-12-31 19:00 -05:00 -0500 EST", "2017-02-28 15:30 -05:00 -0500 EST"}},
		{"EXTRACT(HOUR FROM ts AT TIME ZONE 'Asia/Kolkata') AS hk", []any{int64(5), int64(2)}},
		{"DATE(ts, 'Asia/Kolkata') AS dk", []any{"2021-01-01", "2017-03-01"}},
		{"STRING(ts, '+05:30') AS so", []any{"2021-01-01 05:30:00+05:30", "2017-03-01 02:00:30.4+05:30"}},
		{"STRING(ts) AS su", []any{"2021-01-01 00:00:00+00", "2017-02-28 20:30:30.4+00"}},
		{"STRING(ts, 'America/Los_Angeles') AS sl", []any{"2020-12-31 16:00:00-08", "2017-02-28 12:30:30.4-08"}},
		{"UNIX_SECONDS(TIMESTAMP(dt, 'America/Los_Angeles')) AS tl", []any{int64(1609488000), int64(1675332121)}},
		{"UNIX_SECONDS(TIMESTAMP_TRUNC(ts, DAY, 'America/New_York')) AS tt", []any{int64(1609390800), int64(1488258000)}},
		{"UNIX_SECONDS(PARSE_TIMESTAMP('%Y-%m-%d %H:%M', '2021-07-04 12:00', 'America/New_York')) AS pn", []any{int64(1625414400), int64(1625414400)}},
		{"UNIX_SECONDS(TIMESTAMP('2021-07-04 12:00:00', 'Europe/Paris')) AS tp", []any{int64(1625392800), int64(1625392800)}},
		{"UNIX_SECONDS(TIMESTAMP('2021-07-04 12:00:00+00', 'Europe/Paris')) AS t0", []any{int64(1625400000), int64(1625400000)}},
		{"UNIX_SECONDS(TIMESTAMP(DATETIME '2021-03-14 02:30:00', 'America/New_York')) AS gap", []any{int64(1615707000), int64(1615707000)}},
		{"DATETIME(ts, 'UTC-3') AS dm3", []any{"2020-12-31 21:00:00.000000", "2017-02-28 17:30:30.400000"}},
	}
}

// Every case compiles; the refusals are the page's: an ARRAY and a
// TIMESTAMP output, and what CloudBurrow does not compile.
func TestTransformSpecCompiles(t *testing.T) {
	for _, tc := range exportTransformCases() {
		if _, code, msg := transformSpec(exportTransformModel([]string{tc.item})); code != 0 {
			t.Errorf("%s: %d %s", tc.item, code, msg)
		}
	}
	onehot := mlEncodedSQL("ONE_HOT_ENCODER", "ARRAY<STRING>['a']", "CAST(NULL AS STRING)", "", "(s)", "")
	for item, want := range map[string]string{
		onehot + " AS oh":                                 "is an ARRAY",
		"TIMESTAMP_ADD(ts, INTERVAL 1 HOUR) AS t":         "is a TIMESTAMP",
		"FORMAT_TIMESTAMP('%H', ts, 'Mars/Olympus') AS z": "Invalid time zone: Mars/Olympus",
		"ST_GEOGPOINT(x, x) AS g":                         "does not support the function ST_GEOGPOINT",
	} {
		_, code, msg := transformSpec(exportTransformModel([]string{item}))
		if code == 0 || !strings.Contains(msg, want) {
			t.Errorf("%s: %d %q, want %q", item, code, msg, want)
		}
	}
	// An ARRAY input is in the signature; an output that reads it is
	// refused, as no TRANSFORM function the page lists makes a scalar of one.
	spec, code, msg := transformSpec(exportTransformModel([]string{"x AS x"}))
	if code != 0 {
		t.Fatalf("ARRAY inputs no output reads: %d %s", code, msg)
	}
	got := map[string]any{}
	for _, in := range spec["transform"].(map[string]any)["inputs"].([]map[string]any) {
		got[in["name"].(string)] = fmt.Sprintf("%v/%v/%v", in["dtype"], in["rank"], in["sparse"])
	}
	if got["arr"] != "string/2/<nil>" || got["sp"] != "float64/<nil>/true" {
		t.Errorf("ARRAY inputs: %v", got)
	}
	if _, code, msg := transformSpec(exportTransformModel([]string{"ARRAY_LENGTH(arr) AS al"})); code != 400 || !strings.Contains(msg, "column al reads an ARRAY input") {
		t.Errorf("an output of an ARRAY input: %d %q", code, msg)
	}
	md := exportTransformModel([]string{"x AS x"})
	md.Inputs = append(md.Inputs, mlColumn{Name: "g", Type: "ARRAY<GEOGRAPHY>"})
	if _, code, msg := transformSpec(md); code != 400 || !strings.Contains(msg, "g, of type ARRAY<GEOGRAPHY>") {
		t.Errorf("an ARRAY<GEOGRAPHY> input: %d %q", code, msg)
	}
}

// With the converter image (CLOUDBURROW_TFCONVERT_IMAGE), the
// preprocessing SavedModel loads in TensorFlow and its serving_default
// answers what GoogleSQL computes for each item.
func TestTransformExportWithTensorFlow(t *testing.T) {
	image := os.Getenv("CLOUDBURROW_TFCONVERT_IMAGE")
	if image == "" {
		t.Skip("CLOUDBURROW_TFCONVERT_IMAGE is not set")
	}
	cases := exportTransformCases()
	var items []string
	for _, tc := range cases {
		items = append(items, tc.item)
	}
	spec, code, msg := transformSpec(exportTransformModel(items))
	if code != 0 {
		t.Fatal(code, msg)
	}
	b, _ := json.Marshal(spec)
	model := filepath.Join(t.TempDir(), "model")
	if err := os.Mkdir(model, 0o755); err != nil {
		t.Fatal(err)
	}
	d := &tfconvert.Docker{Image: image}
	if err := d.Export(context.Background(), b, model); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(exportTransformRows)
	cmd := exec.Command("docker", "run", "--rm", "-i", "--network", "none", "-e", "TF_CPP_MIN_LOG_LEVEL=2", "-v", model+":/model:ro", image, "predict")
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("predict: %v: %s", err, stderr.String())
	}
	var out map[string][]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	for _, tc := range cases {
		_, name := transformItemName(tc.item)
		got := out[name]
		if len(got) != 2 {
			t.Errorf("%s: %v", name, got)
			continue
		}
		for i, w := range tc.want {
			if !transformValueIs(got[i], w) {
				t.Errorf("%s row %d: %v, want %v", name, i, got[i], w)
			}
		}
	}
}

// transformValueIs compares an output with GoogleSQL's value: NULL is the
// page's missing value of the output's dtype.
func transformValueIs(got, want any) bool {
	switch w := want.(type) {
	case nil:
		switch g := got.(type) {
		case string:
			return g == transformNullString
		case float64:
			return g == transformNullFloat || g == float64(transformNullInt)
		case bool:
			return !g
		}
		return false
	case float64:
		g, ok := got.(float64)
		return ok && math.Abs(g-w) <= 1e-12*math.Max(1, math.Abs(w))
	case int64:
		g, ok := got.(float64)
		return ok && g == float64(w)
	default:
		return fmt.Sprint(got) == fmt.Sprint(want)
	}
}
