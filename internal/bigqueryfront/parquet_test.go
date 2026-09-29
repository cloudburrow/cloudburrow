package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// sentSchema returns the schema of the load job e was sent, and whether
// one was sent.
func sentSchema(t *testing.T, e *jobsEmulator) (string, bool) {
	t.Helper()
	for _, l := range e.log {
		if !strings.HasPrefix(l, "jobs ") {
			continue
		}
		b := []byte(strings.TrimPrefix(l, "jobs "))
		var job struct {
			Configuration struct {
				Load struct {
					Schema json.RawMessage `json:"schema"`
				} `json:"load"`
			} `json:"configuration"`
		}
		if i := bytes.Index(b, []byte("{")); i >= 0 {
			// An upload: the job is the first part's JSON.
			dec := json.NewDecoder(bytes.NewReader(b[i:]))
			if dec.Decode(&job) != nil {
				t.Fatalf("the job sent is not JSON: %s", l)
			}
		}
		return string(job.Configuration.Load.Schema), true
	}
	return "", false
}

const (
	abTable     = `{"type":"TABLE","schema":{"fields":[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"}]}}`
	abSchema    = `{"fields":[{"name":"a","type":"INTEGER","mode":"NULLABLE"},{"name":"b","type":"STRING","mode":"NULLABLE"}]}`
	abRequiredS = `{"fields":[{"name":"a","type":"INTEGER","mode":"REQUIRED"},{"name":"b","type":"STRING","mode":"REQUIRED"}]}`
	// The schemas as the front sends them for a new table.
	abSent         = `{"fields":[{"mode":"NULLABLE","name":"a","type":"INTEGER"},{"mode":"NULLABLE","name":"b","type":"STRING"}]}`
	abRequiredSent = `{"fields":[{"mode":"REQUIRED","name":"a","type":"INTEGER"},{"mode":"REQUIRED","name":"b","type":"STRING"}]}`
)

func parquetJobBody(table, extra string) string {
	return `{"jobReference":{"projectId":"p","jobId":"pq"},"configuration":{"load":{"sourceFormat":"PARQUET",` +
		`"destinationTable":{"datasetId":"ds","tableId":"` + table + `"}` + extra + `}}}`
}

// TestParquetLoadColumns (#988, #1006): a Parquet upload's columns, read
// from the file's footer, are held to the table's by BigQuery's rules:
// what BigQuery refuses is 400, what CloudBurrow does not load is 501, and
// neither is sent; a load the emulator carries out as BigQuery does is
// sent with the schema the emulator reads the file by, the table's, or for
// a new table the file's (the rest the front carries out itself:
// TestParquetLoadsTheFrontCarriesOut). Files written by Apache Arrow
// (testdata/parquet).
func TestParquetLoadColumns(t *testing.T) {
	tables := func(schema string) map[string]string {
		return map[string]string{tablesBase + "t": `{"type":"TABLE","schema":{"fields":` + schema + `}}`}
	}
	ab := tables(`[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"}]`)
	for _, c := range []struct {
		name   string
		tables map[string]string
		file   string
		extra  string
		code   int
		want   string // the schema sent, or a part of the error's message
	}{
		// Loaded.
		{"the table's columns", ab, "ab.parquet", "", 200, `{"fields":[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"}]}`},
		{"REQUIRED columns into NULLABLE ones", ab, "ab_required.parquet", "", 200, `"name":"a"`},
		{"a table column the file lacks", tables(`[{"name":"a","type":"INT64"},{"name":"b","type":"STRING"},{"name":"c","type":"STRING"}]`),
			"ab.parquet", "", 200, `"name":"c"`},
		{"a new table", nil, "ab_required.parquet", "", 200, abRequiredSent},
		{"the file's own schema in the job", nil, "ab.parquet", `,"schema":` + abSchema, 200, abSent},
		{"a schema that relaxes the file's", nil, "ab_required.parquet", `,"schema":` + abSchema, 200, abSent},
		{"a schema that makes the file's REQUIRED", nil, "ab.parquet", `,"schema":` + abRequiredS, 501, "not the file's"},
		{"WRITE_TRUNCATE, the table's schema", tables(`[{"name":"a","type":"INTEGER","mode":"REQUIRED"},{"name":"b","type":"STRING","mode":"REQUIRED"}]`),
			"ab_required.parquet", `,"writeDisposition":"WRITE_TRUNCATE"`, 200, `"mode":"REQUIRED"`},
		{"ALLOW_FIELD_ADDITION, nothing added", ab, "ab.parquet", `,"schemaUpdateOptions":["ALLOW_FIELD_ADDITION"]`, 200, `"name":"b"`},

		// Refused by BigQuery: 400.
		{"a column the table lacks", tables(`[{"name":"a","type":"INTEGER"}]`), "ab.parquet", "", 400,
			"Provided Schema does not match Table p:ds.t. Cannot add fields (field: b)"},
		{"no column of the table's", tables(`[{"name":"z","type":"STRING"}]`), "ab.parquet", "", 400, "Cannot add fields (field: a)"},
		{"another type", tables(`[{"name":"a","type":"STRING"},{"name":"b","type":"STRING"}]`), "ab.parquet", "", 400,
			"Field a has changed type from STRING to INTEGER"},
		{"a RECORD column", tables(`[{"name":"a","type":"RECORD","fields":[{"name":"x","type":"INTEGER"}]},{"name":"b","type":"STRING"}]`),
			"ab.parquet", "", 400, "Field a has changed type from RECORD to INTEGER"},
		{"a REPEATED column", tables(`[{"name":"a","type":"INTEGER","mode":"REPEATED"},{"name":"b","type":"STRING"}]`),
			"ab.parquet", "", 400, "Field a has changed type from REPEATED INTEGER to INTEGER"},
		{"NULLABLE into REQUIRED", tables(`[{"name":"a","type":"INTEGER","mode":"REQUIRED"},{"name":"b","type":"STRING"}]`),
			"ab.parquet", "", 400, "Field a has changed mode from REQUIRED to NULLABLE"},
		{"a REQUIRED column the file lacks", tables(`[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"},{"name":"c","type":"STRING","mode":"REQUIRED"}]`),
			"ab.parquet", "", 400, "Field c is missing in new schema"},
		{"not a Parquet file", ab, "", "", 400, "not a Parquet file"},

		{"a nested column of another type", tables(`[{"name":"s","type":"RECORD","fields":[{"name":"x","type":"STRING"}]}]`),
			"struct.parquet", "", 400, "Field s.x has changed type from STRING to INTEGER"},
		{"a nested field the table lacks", tables(`[{"name":"s","type":"RECORD","fields":[{"name":"y","type":"STRING"}]}]`),
			"struct.parquet", "", 400, "Cannot add fields (field: s.x)"},
		{"a REQUIRED nested field the file lacks", tables(`[{"name":"s","type":"RECORD","fields":[{"name":"x","type":"INTEGER"},` +
			`{"name":"y","type":"STRING","mode":"REQUIRED"}]}]`), "struct.parquet", "", 400, "Field s.y is missing in new schema"},
		{"another writeDisposition", ab, "ab.parquet", `,"writeDisposition":"WRITE_SOMETIMES"`, 400, "Invalid value for writeDisposition"},

		// Not loaded here: 501.
		{"ALLOW_FIELD_ADDITION of a REQUIRED column", tables(`[{"name":"a","type":"INTEGER","mode":"REQUIRED"}]`), "ab_required.parquet",
			`,"schemaUpdateOptions":["ALLOW_FIELD_ADDITION"]`, 501, "adds a REQUIRED column, b"},
		{"a schema not the file's", nil, "ab.parquet", `,"schema":{"fields":[{"name":"z","type":"STRING"}]}`, 501,
			"a schema that is not the file's (z STRING; the file's is a INTEGER, b STRING)"},
		{"a STRING into GEOGRAPHY", tables(`[{"name":"a","type":"INTEGER"},{"name":"b","type":"GEOGRAPHY"}]`), "ab.parquet", "", 501, "GEOGRAPHY"},
		{"TIMESTAMP(NANOS)", nil, "ts_ns.parquet", "", 501, "INT64 (TIMESTAMP(NANOS)), which BigQuery's Parquet conversion table does not list"},
		{"JSON", nil, "json.parquet", "", 501, "BYTE_ARRAY (JSON), which BigQuery's Parquet conversion table does not list"},
		{"referenceFileSchemaUri", ab, "ab.parquet", `,"referenceFileSchemaUri":"gs://b/x.parquet"`, 501, "referenceFileSchemaUri"},
		{"into a view", map[string]string{tablesBase + "t": `{"type":"VIEW"}`}, "ab.parquet", "", 501, "which is a VIEW"},
	} {
		emu := &jobsEmulator{tables: c.tables}
		data := "not parquet"
		if c.file != "" {
			data = string(fixture(t, c.file))
		}
		w := upload(t, Wrap(emu), parquetJobBody("t", c.extra), data)
		sent, ok := sentSchema(t, emu)
		if w.Code != c.code {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		if c.code != 200 {
			if ok {
				t.Errorf("%s: %d, but the load was sent: %v", c.name, c.code, emu.log)
			}
			if !strings.Contains(w.Body.String(), c.want) || !strings.Contains(w.Body.String(), "Nothing was loaded") {
				t.Errorf("%s: %s, want %q", c.name, w.Body, c.want)
			}
			continue
		}
		if !ok || !strings.Contains(sent, c.want) {
			t.Errorf("%s: sent schema %s, want %s", c.name, sent, c.want)
		}
	}
}

// TestParquetUploadIsSentWhole (#988): the upload the front reads the
// footer of is sent on with the same data, byte for byte, and the file it
// kept is removed.
func TestParquetUploadIsSentWhole(t *testing.T) {
	var got []byte
	emu := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		if _, err := mr.NextPart(); err != nil {
			t.Fatal(err)
		}
		p, err := mr.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		got, _ = io.ReadAll(p)
		writeJSON(w, 200, map[string]any{"jobReference": map[string]any{"projectId": "p", "jobId": "pq"}, "status": map[string]any{"state": "DONE"}})
	})
	// Only the columns the emulator loads, so that it is sent.
	ab := fixture(t, "ab.parquet")
	w := upload(t, Wrap(emu), parquetJobBody("new", ""), string(ab))
	if w.Code != 200 || !bytes.Equal(got, ab) {
		t.Errorf("%d %s: sent %d bytes, want the file's %d", w.Code, w.Body, len(got), len(ab))
	}
	// The spooled body removes its file when closed.
	r := httptest.NewRequest("POST", "/upload"+base+"/jobs?uploadType=multipart", multipartBody(t, parquetJobBody("t", ""), string(ab)))
	r.Header.Set("Content-Type", "multipart/related; boundary=b0undary")
	if _, err := spoolParquetUpload(r); err != nil {
		t.Fatal(err)
	}
	s, ok := r.Body.(*spooled)
	if !ok {
		t.Fatalf("body is %T", r.Body)
	}
	name := s.file.Name()
	b, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if !bytes.Contains(b, ab) || int64(len(b)) != r.ContentLength {
		t.Errorf("the body is %d bytes (ContentLength %d) and holds the file: %v", len(b), r.ContentLength, bytes.Contains(b, ab))
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Errorf("%s was not removed: %v", name, err)
	}
}

func multipartBody(t *testing.T, job, data string) io.Reader {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	if err := mw.SetBoundary("b0undary"); err != nil {
		t.Fatal(err)
	}
	p, _ := mw.CreatePart(map[string][]string{"Content-Type": {"application/json"}})
	_, _ = p.Write([]byte(job))
	p, _ = mw.CreatePart(map[string][]string{"Content-Type": {"application/octet-stream"}})
	_, _ = p.Write([]byte(data))
	_ = mw.Close()
	return &b
}

// rangeStorage serves objects with ranges, as Cloud Storage does, and
// records the ranges read.
type rangeStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	ranges  []string
}

func (s *rangeStorage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rest, _ := strings.CutPrefix(r.URL.Path, "/storage/v1/b/")
	if bucket, ok := strings.CutSuffix(rest, "/o"); ok {
		var items []map[string]string
		for k := range s.objects {
			if b, n, _ := strings.Cut(k, "/"); b == bucket && strings.HasPrefix(n, r.URL.Query().Get("prefix")) {
				items = append(items, map[string]string{"name": n})
			}
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	bucket, name, _ := strings.Cut(rest, "/o/")
	data, ok := s.objects[bucket+"/"+name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("alt") != "media" {
		writeJSON(w, 200, map[string]any{"name": name, "size": json.Number(itoa(len(data)))})
		return
	}
	s.ranges = append(s.ranges, name+" "+r.Header.Get("Range"))
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestParquetLoadFromCloudStorage (#988): the schema of a Parquet load
// from Cloud Storage is read from the objects' footers, by ranges where
// Cloud Storage serves them and whole where it does not, and held to the
// table's as an upload's is; files whose schemas differ are refused.
func TestParquetLoadFromCloudStorage(t *testing.T) {
	objects := map[string][]byte{
		"b/ab.parquet":       fixture(t, "ab.parquet"),
		"b/ab2.parquet":      fixture(t, "ab.parquet"),
		"b/required.parquet": fixture(t, "ab_required.parquet"),
		"b/upper.parquet":    fixture(t, "upper.parquet"),
		"b/not.parquet":      []byte("a,b\n1,x\n"),
	}
	st := &rangeStorage{objects: objects}
	srv := httptest.NewServer(st)
	defer srv.Close()
	plain := &fakeStorage{objects: map[string]string{"b/ab.parquet": string(objects["b/ab.parquet"])}}
	plainSrv := httptest.NewServer(plain)
	defer plainSrv.Close()
	ab := map[string]string{tablesBase + "t": abTable}
	gcs := func(uris string) string { return parquetJobBody("t", `,"sourceUris":[`+uris+`]`) }
	for _, c := range []struct {
		name, uris string
		storage    string
		code       int
		want       string
	}{
		{"one file", `"gs://b/ab.parquet"`, srv.URL, 200, `"name":"b"`},
		{"one file, no ranges", `"gs://b/ab.parquet"`, plainSrv.URL, 200, `"name":"b"`},
		{"two alike", `"gs://b/ab.parquet","gs://b/ab2.parquet"`, srv.URL, 200, `"name":"b"`},
		{"a wildcard", `"gs://b/ab*.parquet"`, srv.URL, 200, `"name":"b"`},
		{"modes differ", `"gs://b/ab.parquet","gs://b/required.parquet"`, srv.URL, 400, "must have the same mode"},
		{"columns differ", `"gs://b/ab.parquet","gs://b/upper.parquet"`, srv.URL, 501, "alphabetically last file"},
		{"not Parquet", `"gs://b/not.parquet"`, srv.URL, 400, "gs://b/not.parquet"},
		{"missing", `"gs://b/none.parquet"`, srv.URL, 404, "gs://b/none.parquet"},
		{"no Cloud Storage", `"gs://b/ab.parquet"`, "", 501, "no Cloud Storage"},
	} {
		emu := &jobsEmulator{tables: ab}
		var opts []Option
		if c.storage != "" {
			opts = append(opts, WithStorage(c.storage))
		}
		st.ranges = nil
		code, got := do(t, Wrap(emu, opts...), "POST", base+"/jobs", gcs(c.uris))
		sent, ok := sentSchema(t, emu)
		if code != c.code {
			t.Errorf("%s: %d %v", c.name, code, got)
			continue
		}
		if code != 200 {
			if b, _ := json.Marshal(got); !strings.Contains(string(b), c.want) || ok {
				t.Errorf("%s: %s (sent: %v), want %q", c.name, b, ok, c.want)
			}
			continue
		}
		if !strings.Contains(sent, c.want) {
			t.Errorf("%s: sent schema %s", c.name, sent)
		}
		if c.storage == srv.URL {
			for _, r := range st.ranges {
				if !strings.HasPrefix(strings.SplitN(r, " ", 2)[1], "bytes=") {
					t.Errorf("%s: read %q whole", c.name, r)
				}
			}
		}
	}
}

// TestParquetAddsNestedFields (#1068): ALLOW_FIELD_ADDITION puts a field
// the file adds inside a RECORD at the end of it, and the table's rows get
// it NULL, through a REPEATED RECORD too.
func TestParquetAddsNestedFields(t *testing.T) {
	table := []field{{Name: "s", Type: "RECORD", Fields: []field{{Name: "y", Type: "STRING"}}},
		{Name: "r", Type: "RECORD", Mode: "REPEATED", Fields: []field{{Name: "a", Type: "INT64"}}}}
	file := []field{{Name: "s", Type: "RECORD", Fields: []field{{Name: "x", Type: "INT64"}, {Name: "y", Type: "STRING"}}},
		{Name: "r", Type: "RECORD", Mode: "REPEATED", Fields: []field{{Name: "a", Type: "INT64"}, {Name: "b", Type: "STRING"}}}}
	var m pqMerge
	got, code, why := mergeFields(table, file, true, false, "", "p:ds.t", &m)
	if code != 0 || !m.added {
		t.Fatalf("merge: %d %s %+v", code, why, m)
	}
	if n := got[0].Fields; len(n) != 2 || n[0].Name != "y" || n[1].Name != "x" {
		t.Errorf("s's fields: %+v, want y then x", n)
	}
	if _, code, _ := mergeFields(table, file, false, false, "", "p:ds.t", &pqMerge{}); code != http.StatusBadRequest {
		t.Errorf("without ALLOW_FIELD_ADDITION: %d, want 400", code)
	}
	for i, want := range []string{"STRUCT(`s`.`y` AS `y`, ", "ARRAY(SELECT IF(_cb_e0 IS NULL, NULL, STRUCT(_cb_e0.`a` AS `a`, "} {
		if v := widenedValue(table[i], got[i], quoteName(table[i].Name), 0); !strings.Contains(v, want) {
			t.Errorf("%s: %s, want %s", table[i].Name, v, want)
		}
	}
	if v := widenedValue(table[0], table[0], "`s`", 0); v != "`s`" {
		t.Errorf("an unchanged RECORD: %s", v)
	}
}
