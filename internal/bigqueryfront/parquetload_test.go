package bigqueryfront

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// tablesEmulator is an emulator of tables and DML for the Parquet loads
// the front carries out itself: it keeps tables (tables.insert, get,
// patch, delete) and records every statement sent to jobs.query, which it
// answers as done. A table's rows are not kept; nonEmpty names the tables
// a SELECT 1 ... LIMIT 1 finds a row in.
type tablesEmulator struct {
	mu       sync.Mutex
	tables   map[string]map[string]any // by table ID
	nonEmpty map[string]bool
	fail     *regexp.Regexp // a statement it fails
	log      []string       // "METHOD path" or "SQL <statement>"
}

func (e *tablesEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	id := path[strings.LastIndex(path, "/")+1:]
	switch {
	case strings.HasSuffix(path, "/queries"):
		var q queryOptions
		_ = json.Unmarshal(b, &q)
		e.log = append(e.log, "SQL "+q.Query)
		if e.fail != nil && e.fail.MatchString(q.Query) {
			http.Error(w, `{"error":{"code":400,"message":"failed"}}`, http.StatusBadRequest)
			return
		}
		if strings.HasPrefix(q.Query, "SELECT 1 FROM ") {
			rows := []any{}
			for t := range e.nonEmpty {
				if strings.Contains(q.Query, "."+t+"`") {
					rows = append(rows, map[string]any{"f": []any{map[string]any{"v": "1"}}})
				}
			}
			writeJSON(w, 200, map[string]any{"jobComplete": true, "rows": rows})
			return
		}
		writeJSON(w, 200, map[string]any{"jobComplete": true})
	case strings.Contains(path, "/jobs/"):
		http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
	case strings.Contains(path, "/tables/") || strings.HasSuffix(path, "/tables"):
		e.log = append(e.log, r.Method+" "+id+" "+string(b))
		switch r.Method {
		case http.MethodGet:
			t, ok := e.tables[id]
			if !ok {
				http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
				return
			}
			writeJSON(w, 200, t)
		case http.MethodPost:
			var t map[string]any
			_ = json.Unmarshal(b, &t)
			ref, _ := t["tableReference"].(map[string]any)
			name, _ := ref["tableId"].(string)
			if e.tables == nil {
				e.tables = map[string]map[string]any{}
			}
			e.tables[name] = t
			writeJSON(w, 200, t)
		case http.MethodPatch:
			var p map[string]any
			_ = json.Unmarshal(b, &p)
			t := e.tables[id]
			for k, v := range p {
				t[k] = v
			}
			writeJSON(w, 200, t)
		case http.MethodDelete:
			delete(e.tables, id)
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		// datasets.get
		writeJSON(w, 200, map[string]any{})
	}
}

// statements returns the SQL sent, with the scratch table's name made
// fixed.
func (e *tablesEmulator) statements() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, l := range e.log {
		if s, ok := strings.CutPrefix(l, "SQL "); ok {
			out = append(out, scratchName.ReplaceAllString(s, "_scratch"))
		}
	}
	return out
}

var scratchName = regexp.MustCompile(`_cloudburrow_replace_[0-9a-f]+`)

func abcTable(extra string) map[string]map[string]any {
	var t map[string]any
	_ = json.Unmarshal([]byte(`{"type":"TABLE","tableReference":{"projectId":"p","datasetId":"ds","tableId":"t"},`+
		`"description":"kept","labels":{"k":"v"},"etag":"e","numRows":"1",`+
		`"schema":{"fields":[{"name":"a","type":"INTEGER","description":"da"},{"name":"b","type":"STRING"`+extra+`}]}}`), &t)
	return map[string]map[string]any{"t": t}
}

// TestParquetLoadsTheFrontCarriesOut (#1004, #1005, #1006): a Parquet load
// the emulator would not carry out as BigQuery does is the front's own
// job: the file's rows (files written by Apache Arrow, testdata/parquet)
// are inserted with DML as literals of the table's row type, converted
// exactly, after any change to the table's schema, and the job is
// answered done with its counts.
func TestParquetLoadsTheFrontCarriesOut(t *testing.T) {
	for _, c := range []struct {
		name   string
		tables map[string]map[string]any
		file   string
		extra  string
		want   []string // the statements, in order
		schema string   // a part of the table's schema after the load
	}{
		{"converted types, a new table", nil, "converted.parquet", "", []string{
			"INSERT INTO `ds.t` (`id`, `time_ms`, `time_us`, `ts_ms`, `bytes`, `fixed`, `uint64`, `float`, `double`) VALUES " +
				"(1, TIME '01:02:03.004000', TIME '01:02:03.004567', TIMESTAMP '2024-01-02 03:04:05.123000+00', b'\\x00\\x01', b'ab', 1, 1.5, 0.1), " +
				"(2, TIME '23:59:59.999000', TIME '00:00:00.000000', TIMESTAMP '1969-12-31 23:59:59.999000+00', b'\\xff', b'\\xff\\x00', 9223372036854775807, IEEE_DIVIDE(1, 0), IEEE_DIVIDE(-1, 0)), " +
				"(3, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL)"},
			`{"mode":"NULLABLE","name":"time_ms","type":"TIME"}`},
		{"INT96", nil, "int96.parquet", "", []string{
			"INSERT INTO `ds.t` (`t`) VALUES (TIMESTAMP '2024-01-02 03:04:05.123456+00'), (TIMESTAMP '2024-01-02 03:04:05.123456+00')"}, ""},
		{"DECIMAL, FIXED_LEN_BYTE_ARRAY", nil, "decimal.parquet", "", []string{
			"INSERT INTO `ds.t` (`d`) VALUES (NUMERIC '1.25'), (NUMERIC '-2.50')"}, `"type":"NUMERIC"`},
		{"DECIMAL, INT32 and INT64", nil, "decimal_int.parquet", "", []string{
			"INSERT INTO `ds.t` (`d32`, `d64`) VALUES (NUMERIC '1.25', NUMERIC '123456789012.345678'), " +
				"(NUMERIC '-2.50', NUMERIC '-0.000001'), (NULL, NULL)"}, ""},
		{"DECIMAL(38, 10) into NUMERIC, exact", nil, "decimal_wide.parquet", "", []string{
			"INSERT INTO `ds.t` (`d`) VALUES (NUMERIC '1.500000000'), (NUMERIC '-12345678901234567890.123456789')"}, ""},
		{"DECIMAL(38, 10) into BIGNUMERIC", nil, "decimal_wide.parquet", `,"decimalTargetTypes":["NUMERIC","BIGNUMERIC"]`, []string{
			"INSERT INTO `ds.t` (`d`) VALUES (BIGNUMERIC '1.5000000000'), (BIGNUMERIC '-12345678901234567890.1234567890')"},
			`"type":"BIGNUMERIC"`},
		{"DECIMAL(40, 2), 256 bits", nil, "decimal256.parquet", `,"decimalTargetTypes":["BIGNUMERIC"]`, []string{
			"INSERT INTO `ds.t` (`d`) VALUES (BIGNUMERIC '12345678901234567890123456789012345678.12'), (BIGNUMERIC '-1.00')"}, ""},
		{"ENUM as BYTES", nil, "enum.parquet", "", []string{"INSERT INTO `ds.t` (`e`) VALUES (b'red'), (b''), (NULL)"}, `"type":"BYTES"`},
		{"ENUM as STRING", nil, "enum.parquet", `,"parquetOptions":{"enumAsString":true}`, []string{
			"INSERT INTO `ds.t` (`e`) VALUES ('red'), (''), (NULL)"}, `"type":"STRING"`},
		{"nested, without inference", nil, "nested.parquet", "", []string{
			"INSERT INTO `ds.t` (`id`, `s`, `l`, `ls`, `m`, `sl`) VALUES " +
				"(1, STRUCT<`x` INT64, `y` STRING, `z` STRUCT<`q` BOOL>>(1, 'a', STRUCT<`q` BOOL>(TRUE)), " +
				"STRUCT<`list` ARRAY<STRUCT<`element` INT64>>>(ARRAY<STRUCT<`element` INT64>>[STRUCT<`element` INT64>(1), STRUCT<`element` INT64>(2)]), " +
				"STRUCT<`list` ARRAY<STRUCT<`element` STRUCT<`x` INT64, `y` STRING>>>>(ARRAY<STRUCT<`element` STRUCT<`x` INT64, `y` STRING>>>[" +
				"STRUCT<`element` STRUCT<`x` INT64, `y` STRING>>(STRUCT<`x` INT64, `y` STRING>(1, 'a')), " +
				"STRUCT<`element` STRUCT<`x` INT64, `y` STRING>>(STRUCT<`x` INT64, `y` STRING>(2, NULL))]), " +
				"STRUCT<`key_value` ARRAY<STRUCT<`key` STRING, `value` INT64>>>(ARRAY<STRUCT<`key` STRING, `value` INT64>>[" +
				"STRUCT<`key` STRING, `value` INT64>('a', 1), STRUCT<`key` STRING, `value` INT64>('b', NULL)]), " +
				"STRUCT<`l` STRUCT<`list` ARRAY<STRUCT<`element` INT64>>>>(STRUCT<`list` ARRAY<STRUCT<`element` INT64>>>(" +
				"ARRAY<STRUCT<`element` INT64>>[STRUCT<`element` INT64>(1), STRUCT<`element` INT64>(2)]))), " +
				"(2, STRUCT<`x` INT64, `y` STRING, `z` STRUCT<`q` BOOL>>(NULL, 'b', NULL), " +
				"STRUCT<`list` ARRAY<STRUCT<`element` INT64>>>(ARRAY<STRUCT<`element` INT64>>[]), " +
				"STRUCT<`list` ARRAY<STRUCT<`element` STRUCT<`x` INT64, `y` STRING>>>>(ARRAY<STRUCT<`element` STRUCT<`x` INT64, `y` STRING>>>[]), " +
				"STRUCT<`key_value` ARRAY<STRUCT<`key` STRING, `value` INT64>>>(ARRAY<STRUCT<`key` STRING, `value` INT64>>[]), " +
				"STRUCT<`l` STRUCT<`list` ARRAY<STRUCT<`element` INT64>>>>(NULL)), " +
				"(3, NULL, NULL, NULL, NULL, NULL), " +
				"(4, STRUCT<`x` INT64, `y` STRING, `z` STRUCT<`q` BOOL>>(4, NULL, STRUCT<`q` BOOL>(NULL)), " +
				"STRUCT<`list` ARRAY<STRUCT<`element` INT64>>>(ARRAY<STRUCT<`element` INT64>>[STRUCT<`element` INT64>(3)]), " +
				"STRUCT<`list` ARRAY<STRUCT<`element` STRUCT<`x` INT64, `y` STRING>>>>(ARRAY<STRUCT<`element` STRUCT<`x` INT64, `y` STRING>>>[" +
				"STRUCT<`element` STRUCT<`x` INT64, `y` STRING>>(STRUCT<`x` INT64, `y` STRING>(3, 'c'))]), " +
				"STRUCT<`key_value` ARRAY<STRUCT<`key` STRING, `value` INT64>>>(ARRAY<STRUCT<`key` STRING, `value` INT64>>[" +
				"STRUCT<`key` STRING, `value` INT64>('c', 3)]), " +
				"STRUCT<`l` STRUCT<`list` ARRAY<STRUCT<`element` INT64>>>>(STRUCT<`list` ARRAY<STRUCT<`element` INT64>>>(ARRAY<STRUCT<`element` INT64>>[])))"},
			`"mode":"REPEATED","name":"list"`},
		{"nested, with list inference and ARRAY_OF_STRUCT", nil, "nested.parquet",
			`,"parquetOptions":{"enableListInference":true,"mapTargetType":"ARRAY_OF_STRUCT"}`, []string{
				"INSERT INTO `ds.t` (`id`, `s`, `l`, `ls`, `m`, `sl`) VALUES " +
					"(1, STRUCT<`x` INT64, `y` STRING, `z` STRUCT<`q` BOOL>>(1, 'a', STRUCT<`q` BOOL>(TRUE)), ARRAY<INT64>[1, 2], " +
					"ARRAY<STRUCT<`x` INT64, `y` STRING>>[STRUCT<`x` INT64, `y` STRING>(1, 'a'), STRUCT<`x` INT64, `y` STRING>(2, NULL)], " +
					"ARRAY<STRUCT<`key` STRING, `value` INT64>>[STRUCT<`key` STRING, `value` INT64>('a', 1), STRUCT<`key` STRING, `value` INT64>('b', NULL)], " +
					"STRUCT<`l` ARRAY<INT64>>(ARRAY<INT64>[1, 2])), " +
					"(2, STRUCT<`x` INT64, `y` STRING, `z` STRUCT<`q` BOOL>>(NULL, 'b', NULL), ARRAY<INT64>[], ARRAY<STRUCT<`x` INT64, `y` STRING>>[], " +
					"ARRAY<STRUCT<`key` STRING, `value` INT64>>[], STRUCT<`l` ARRAY<INT64>>(ARRAY<INT64>[])), " +
					"(3, NULL, ARRAY<INT64>[], ARRAY<STRUCT<`x` INT64, `y` STRING>>[], ARRAY<STRUCT<`key` STRING, `value` INT64>>[], NULL), " +
					"(4, STRUCT<`x` INT64, `y` STRING, `z` STRUCT<`q` BOOL>>(4, NULL, STRUCT<`q` BOOL>(NULL)), ARRAY<INT64>[3], " +
					"ARRAY<STRUCT<`x` INT64, `y` STRING>>[STRUCT<`x` INT64, `y` STRING>(3, 'c')], " +
					"ARRAY<STRUCT<`key` STRING, `value` INT64>>[STRUCT<`key` STRING, `value` INT64>('c', 3)], STRUCT<`l` ARRAY<INT64>>(ARRAY<INT64>[]))"},
			`"mode":"REPEATED","name":"l","type":"INTEGER"`},

		// Appends.
		{"names in another case", abcTable(""), "upper.parquet", "", []string{
			"INSERT INTO `_cloudburrow_query_results._scratch` (`a`, `b`) VALUES (1, 'x'), (2, 'y'), (3, 'z')",
			"INSERT INTO `ds.t` (`a`, `b`) SELECT `a`, `b` FROM `_cloudburrow_query_results._scratch`"}, `"name":"a"`},
		{"ALLOW_FIELD_RELAXATION", abcTable(`,"mode":"REQUIRED"`), "ab.parquet", `,"schemaUpdateOptions":["ALLOW_FIELD_RELAXATION"]`, []string{
			"INSERT INTO `_cloudburrow_query_results._scratch` (`a`, `b`) VALUES (1, 'x'), (2, 'y'), (3, 'z')",
			"INSERT INTO `ds.t` (`a`, `b`) SELECT `a`, `b` FROM `_cloudburrow_query_results._scratch`"},
			`{"mode":"NULLABLE","name":"b","type":"STRING"}`},
		{"ALLOW_FIELD_ADDITION", abcTable(""), "abc.parquet", `,"schemaUpdateOptions":["ALLOW_FIELD_ADDITION"]`, []string{
			"INSERT INTO `_cloudburrow_query_results._scratch` (`a`, `b`, `c`) SELECT `a`, `b`, NULL FROM `ds.t`",
			"INSERT INTO `_cloudburrow_query_results._scratch` (`a`, `b`, `c`) VALUES (4, 'w', TRUE)",
			"INSERT INTO `ds.t` (`a`, `b`, `c`) SELECT `a`, `b`, `c` FROM `_cloudburrow_query_results._scratch`"},
			`{"description":"da","mode":"NULLABLE","name":"a","type":"INTEGER"},{"mode":"NULLABLE","name":"b","type":"STRING"},` +
				`{"mode":"NULLABLE","name":"c","type":"BOOLEAN"}`},
		{"WRITE_TRUNCATE with the file's schema", abcTable(""), "ab_required.parquet", `,"writeDisposition":"WRITE_TRUNCATE"`, []string{
			"INSERT INTO `_cloudburrow_query_results._scratch` (`a`, `b`) VALUES (1, 'x'), (2, 'y'), (3, 'z')",
			"INSERT INTO `ds.t` (`a`, `b`) SELECT `a`, `b` FROM `_cloudburrow_query_results._scratch`"},
			`{"mode":"REQUIRED","name":"a","type":"INTEGER"},{"mode":"REQUIRED","name":"b","type":"STRING"}`},
		{"WRITE_TRUNCATE_DATA", abcTable(""), "upper.parquet", `,"writeDisposition":"WRITE_TRUNCATE_DATA"`, []string{
			"INSERT INTO `_cloudburrow_query_results._scratch` (`a`, `b`) VALUES (1, 'x'), (2, 'y'), (3, 'z')",
			"DELETE FROM `ds.t` WHERE TRUE",
			"INSERT INTO `ds.t` (`a`, `b`) SELECT `a`, `b` FROM `_cloudburrow_query_results._scratch`"}, `"description":"da"`},
	} {
		emu := &tablesEmulator{tables: c.tables}
		w := upload(t, Wrap(emu), parquetJobBody("t", c.extra), string(fixture(t, c.file)))
		if w.Code != 200 {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		var job struct {
			Status struct {
				ErrorResult *rowError `json:"errorResult"`
			} `json:"status"`
			Statistics struct {
				Load map[string]string `json:"load"`
			} `json:"statistics"`
			Configuration struct {
				JobType string `json:"jobType"`
				Load    struct {
					SourceFormat string `json:"sourceFormat"`
				} `json:"load"`
			} `json:"configuration"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil || job.Status.ErrorResult != nil ||
			job.Configuration.JobType != "LOAD" || job.Configuration.Load.SourceFormat != "PARQUET" ||
			job.Statistics.Load["inputFiles"] != "1" || job.Statistics.Load["outputRows"] == "" {
			t.Errorf("%s: the job %s (%v)", c.name, w.Body, err)
		}
		got := emu.statements()
		var dml []string
		for _, s := range got {
			if !strings.HasPrefix(s, "SELECT 1 FROM") {
				dml = append(dml, s)
			}
		}
		if strings.Join(dml, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s: statements\n got %s\nwant %s", c.name, strings.Join(dml, "\n"), strings.Join(c.want, "\n"))
		}
		tbl, ok := emu.tables["t"]
		if !ok {
			t.Errorf("%s: no table", c.name)
			continue
		}
		b, _ := json.Marshal(tbl["schema"])
		if !strings.Contains(string(b), c.schema) {
			t.Errorf("%s: the table's schema %s, want %s", c.name, b, c.schema)
		}
		for k := range emu.tables {
			if k != "t" {
				t.Errorf("%s: table %s was left", c.name, k)
			}
		}
		if c.tables != nil && (tbl["description"] != "kept" || tbl["etag"] != nil && c.name == "ALLOW_FIELD_ADDITION") {
			t.Errorf("%s: the table's properties are now %v", c.name, tbl)
		}
	}
}

// TestParquetLoadValuesRefused (#1004, #1005): a value BigQuery refuses
// fails the job, reason invalid; one the front does not load is 501; and
// neither loads a row or makes a table.
func TestParquetLoadValuesRefused(t *testing.T) {
	for _, c := range []struct {
		name, file, extra string
		code              int
		reason, msg       string
	}{
		{"unsigned INT64 above INT64's maximum", "uint64_big.parquet", "", 200, "invalid", "exceeds the maximum INTEGER value"},
		{"NaN", "nan.parquet", "", 501, "notImplemented", "has a NaN"},
		{"INT96 with nanoseconds", "int96_ns.parquet", "", 501, "notImplemented", "part of a microsecond"},
		{"a NULL element with list inference", "list_null.parquet", `,"parquetOptions":{"enableListInference":true}`, 501,
			"notImplemented", "a NULL element"},
		{"a LIST of LISTs with list inference", "list_list.parquet", `,"parquetOptions":{"enableListInference":true}`, 501,
			"notImplemented", "LIST of lists"},
		{"DECIMAL digits NUMERIC would round", "decimal_frac.parquet", "", 501, "notImplemented", "more than the 9 fractional digits"},
		{"DECIMAL to STRING", "decimal.parquet", `,"decimalTargetTypes":["STRING"]`, 501, "notImplemented", "makes STRING"},
		{"ENUM as STRING, not UTF-8", "enum_bytes.parquet", `,"parquetOptions":{"enumAsString":true}`, 501, "notImplemented", "not UTF-8"},
		{"a bad mapTargetType", "ab.parquet", `,"parquetOptions":{"mapTargetType":"LIST"}`, 400, "invalid", "mapTargetType"},
		{"a dry run", "converted.parquet", `},"dryRun":true,"x":{`, 501, "notImplemented", "dry run"},
	} {
		emu := &tablesEmulator{}
		w := upload(t, Wrap(emu), parquetJobBody("t", c.extra), string(fixture(t, c.file)))
		body := w.Body.String()
		if w.Code != c.code || !strings.Contains(body, `"reason":"`+c.reason+`"`) || !strings.Contains(body, c.msg) {
			t.Errorf("%s: %d %s", c.name, w.Code, body)
		}
		if len(emu.tables) != 0 || len(emu.statements()) != 0 {
			t.Errorf("%s: %v", c.name, emu.log)
		}
	}
}

// TestParquetLoadFailuresLeaveTheTable (#1006): a load the front carries
// out that fails part way leaves the table as it was, or, once the table
// has been made again, names the scratch table the rows are in; WRITE_EMPTY
// into a table with rows fails "duplicate"; into a table that does not
// exist, CREATE_NEVER fails "notFound".
func TestParquetLoadFailuresLeaveTheTable(t *testing.T) {
	failed := func(t *testing.T, w *httptest.ResponseRecorder) rowError {
		t.Helper()
		var job struct {
			Status struct {
				ErrorResult *rowError `json:"errorResult"`
			} `json:"status"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &job) != nil || job.Status.ErrorResult == nil {
			t.Fatalf("%d %s, want a failed job", w.Code, w.Body)
		}
		return *job.Status.ErrorResult
	}
	// The rows' INSERT fails: the table is untouched.
	emu := &tablesEmulator{tables: abcTable(""), fail: regexp.MustCompile("VALUES")}
	e := failed(t, upload(t, Wrap(emu), parquetJobBody("t", `,"schemaUpdateOptions":["ALLOW_FIELD_ADDITION"]`),
		string(fixture(t, "abc.parquet"))))
	if e.Reason != "backendError" || len(emu.tables) != 1 || len(emu.tables["t"]["schema"].(map[string]any)["fields"].([]any)) != 2 {
		t.Errorf("a failed insert: %+v, tables %v", e, emu.tables)
	}
	// The copy into the table made again fails: the rows are named.
	emu = &tablesEmulator{tables: abcTable(""), fail: regexp.MustCompile("^INSERT INTO `ds.t`")}
	e = failed(t, upload(t, Wrap(emu), parquetJobBody("t", `,"schemaUpdateOptions":["ALLOW_FIELD_ADDITION"]`),
		string(fixture(t, "abc.parquet"))))
	if !strings.Contains(e.Message, "the table's rows and the loaded ones are in _cloudburrow_query_results._cloudburrow_replace_") || len(emu.tables) != 2 {
		t.Errorf("a failed copy: %+v, tables %v", e, emu.tables)
	}
	// WRITE_EMPTY.
	emu = &tablesEmulator{tables: abcTable(""), nonEmpty: map[string]bool{"t": true}}
	if e := failed(t, upload(t, Wrap(emu), parquetJobBody("t", `,"writeDisposition":"WRITE_EMPTY"`),
		string(fixture(t, "upper.parquet")))); e.Reason != "duplicate" || !strings.Contains(e.Message, "Already Exists: Table p:ds.t") {
		t.Errorf("WRITE_EMPTY: %+v", e)
	}
	// CREATE_NEVER.
	emu = &tablesEmulator{}
	if e := failed(t, upload(t, Wrap(emu), parquetJobBody("t", `,"createDisposition":"CREATE_NEVER"`),
		string(fixture(t, "converted.parquet")))); e.Reason != "notFound" || len(emu.tables) != 0 {
		t.Errorf("CREATE_NEVER: %+v", e)
	}
	// A value BigQuery refuses: the job fails and is kept for jobs.get.
	emu = &tablesEmulator{}
	h := Wrap(emu)
	e = failed(t, upload(t, h, parquetJobBody("t", ""), string(fixture(t, "uint64_big.parquet"))))
	if e.Reason != "invalid" {
		t.Errorf("uint64: %+v", e)
	}
	code, got := do(t, h, "GET", base+"/jobs/pq", "")
	if b, _ := json.Marshal(got); code != 200 || !strings.Contains(string(b), "exceeds the maximum INTEGER value") {
		t.Errorf("jobs.get: %d %s", code, b)
	}
}

// TestParquetLoadFromCloudStorageTheFrontCarriesOut (#1004): the objects of
// a load from Cloud Storage that the front carries out are read from the
// instance's Cloud Storage, each by its own schema, and inserted as one
// job.
func TestParquetLoadFromCloudStorageTheFrontCarriesOut(t *testing.T) {
	st := &rangeStorage{objects: map[string][]byte{"b/a.parquet": fixture(t, "decimal.parquet"),
		"b/b.parquet": fixture(t, "decimal.parquet")}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	emu := &tablesEmulator{}
	code, got := do(t, Wrap(emu, WithStorage(srv.URL)), "POST", base+"/jobs",
		parquetJobBody("t", `,"sourceUris":["gs://b/*.parquet"]`))
	b, _ := json.Marshal(got)
	if code != 200 || !strings.Contains(string(b), `"inputFiles":"2"`) || !strings.Contains(string(b), `"outputRows":"4"`) {
		t.Errorf("%d %s", code, b)
	}
	if s := emu.statements(); len(s) != 1 || s[0] != "INSERT INTO `ds.t` (`d`) VALUES (NUMERIC '1.25'), (NUMERIC '-2.50'), "+
		"(NUMERIC '1.25'), (NUMERIC '-2.50')" {
		t.Errorf("statements %v", s)
	}
}

func hexBytes(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// TestParquetLiterals: values at their limits are written as literals of
// their type, and those beyond them refused.
func TestParquetLiterals(t *testing.T) {
	for _, c := range []struct {
		s    pqScalar
		v    any
		want string // or the error's text
	}{
		{pqScalar{bq: "INTEGER", conv: pqToUint32}, int32(-1), "4294967295"},
		{pqScalar{bq: "INTEGER", conv: pqToInt64}, int64(-9223372036854775808), "-9223372036854775808"},
		{pqScalar{bq: "INTEGER", conv: pqToUint64}, int64(-1), "exceeds the maximum INTEGER value"},
		{pqScalar{bq: "FLOAT", conv: pqToFloat64}, float64(2), "2.0"},
		{pqScalar{bq: "FLOAT", conv: pqToFloat64}, 1e300, "1e+300"},
		{pqScalar{bq: "FLOAT", conv: pqToFloat32}, float32(0.1), "0.10000000149011612"},
		{pqScalar{bq: "DATE", conv: pqToDateDays}, int32(-719162), "DATE '0001-01-01'"},
		{pqScalar{bq: "DATE", conv: pqToDateDays}, int32(-719163), "outside BigQuery's range"},
		{pqScalar{bq: "DATE", conv: pqToDateDays}, int32(2932896), "DATE '9999-12-31'"},
		{pqScalar{bq: "TIME", conv: pqToTimeMicros64}, int64(86400e6 - 1), "TIME '23:59:59.999999'"},
		{pqScalar{bq: "TIME", conv: pqToTimeMicros64}, int64(86400e6), "outside a day"},
		{pqScalar{bq: "TIMESTAMP", conv: pqToTsMicros}, int64(253402300799999999), "TIMESTAMP '9999-12-31 23:59:59.999999+00'"},
		{pqScalar{bq: "TIMESTAMP", conv: pqToTsMicros}, int64(253402300800000000), "outside BigQuery's range"},
		{pqScalar{bq: "TIMESTAMP", conv: pqToTsMillis}, int64(-62135596800000), "TIMESTAMP '0001-01-01 00:00:00.000000+00'"},
		{pqScalar{bq: "TIMESTAMP", conv: pqToTsMillis}, int64(9223372036854775807), "outside BigQuery's range"},
		{pqScalar{bq: "STRING", conv: pqToString}, []byte("it's \\ é\n"), `'it\'s \\ \U000000e9\U0000000a'`},
		{pqScalar{bq: "BYTES", conv: pqToBytes}, []byte("a'\\\x00"), `b'a\'\\\x00'`},
		{pqScalar{bq: "NUMERIC", conv: pqToDecimal, scale: 0}, []byte{0x80}, "NUMERIC '-128'"},
		{pqScalar{bq: "NUMERIC", conv: pqToDecimal, scale: 0}, int64(-1), "NUMERIC '-1'"},
		{pqScalar{bq: "NUMERIC", conv: pqToDecimal, scale: 9}, hexBytes("4b3b4ca85a86c47a098a223fffffffff"),
			"NUMERIC '99999999999999999999999999999.999999999'"},
		{pqScalar{bq: "NUMERIC", conv: pqToDecimal, scale: 9}, hexBytes("b4c4b357a5793b85f675ddc000000001"),
			"NUMERIC '-99999999999999999999999999999.999999999'"},
		{pqScalar{bq: "NUMERIC", conv: pqToDecimal, scale: 9}, hexBytes("4b3b4ca85a86c47a098a224000000000"),
			"exceeds the range of NUMERIC"},
	} {
		got, err := c.s.literal("c", c.v)
		if err != nil {
			got = err.Error()
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%+v %v: %s, want %s", c.s, c.v, got, c.want)
		}
	}
}

// FuzzParquetRows: no file makes the row reader panic, loop, or give rows
// it cannot account for.
func FuzzParquetRows(f *testing.F) {
	for _, name := range []string{"nested.parquet", "converted.parquet", "decimal_int.parquet", "enum.parquet"} {
		b, err := os.ReadFile(filepath.Join("testdata", "parquet", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		elems, err := readParquetSchema(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return
		}
		cols, err := parquetColumns(elems, pqOptions{listInference: true})
		if err != nil {
			return
		}
		_, _ = readParquetRows(&readerAtSeeker{ReaderAt: bytes.NewReader(b), size: int64(len(b))}, elems, cols,
			func([]any) error { return nil })
	})
}
