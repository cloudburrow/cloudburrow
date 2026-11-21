package bigqueryfront

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestIcebergCreateChecksColumnTypes (#1427): the CREATE TABLE form of a
// table for Apache Iceberg checks its column list's types as tables.insert
// does, at any position, ARRAY and STRUCT included.
func TestIcebergCreateChecksColumnTypes(t *testing.T) {
	opts := " WITH CONNECTION `p.us.c` OPTIONS (file_format = 'PARQUET', table_format = 'ICEBERG', storage_uri = 'gs://b/t')"
	for cols, bad := range map[string]string{
		"(i INT64 NOT NULL, s STRING, n NUMERIC(10, 2), ts TIMESTAMP)": "",
		"(i INT64, j JSON)":                        "j (JSON)",
		"(i INT64, a ARRAY<INT64>)":                "a (ARRAY<INTEGER>)",
		"(r STRUCT<x INT64>, i INT64)":             "r (RECORD)",
		"(i INT64, b BIGNUMERIC)":                  "b (BIGNUMERIC)",
		"(g GEOGRAPHY OPTIONS (description = ''))": "g (GEOGRAPHY)",
	} {
		d, msg, ok := parseDDLOptions("CREATE TABLE ds.t " + cols + opts)
		if !ok || msg != "" || d.invalid != "" || !d.iceberg() {
			t.Fatalf("%s: parsed %v %q %q iceberg %v", cols, ok, msg, d.invalid, d.iceberg())
		}
		got := front{}.icebergColumnsOf(nil, queryOptions{}, d)
		if bad == "" && got != "" || bad != "" && !strings.Contains(got, "the column "+bad+" ") {
			t.Errorf("%s: %q, want the column %q", cols, got, bad)
		}
	}
	if got := ctasQuery("CREATE TABLE t WITH CONNECTION c OPTIONS (table_format = 'ICEBERG') AS SELECT (1) AS i;"); got != " SELECT (1) AS i" {
		t.Errorf("ctasQuery: %q", got)
	}
	if d, _, _ := parseDDLOptions("CREATE TABLE ds.t (j JSON) OPTIONS (description = 'x')"); d.iceberg() {
		t.Error("a table of no table_format is a table for Apache Iceberg")
	}
}

// TestIcebergWriteError (#1427): a write whose files could not be written
// is answered by Cloud Storage's status where BigQuery has its reason.
func TestIcebergWriteError(t *testing.T) {
	it := icebergTable{Dataset: "ds", Table: "t", Location: "gs://b/t"}
	for msg, want := range map[string]int{
		"writing gs://b/t/data/x.parquet: Cloud Storage answered 404: no bucket": http.StatusNotFound,
		"writing gs://b/t/data/x.parquet: Cloud Storage answered 403: denied":    http.StatusForbidden,
		"writing gs://b/t/data/x.parquet: dial tcp: refused":                     http.StatusBadRequest,
	} {
		status, why := icebergWriteError(it, msg)
		if status != want || !strings.Contains(why.Message, "ds.t") || !strings.Contains(why.Message, msg) {
			t.Errorf("%s: %d %+v, want %d", msg, status, why, want)
		}
	}
}

// icebergFake is an engine of one table, ds.t of project p, for the
// Iceberg writes: tables.get, tabledata.list (its rows in the order rows
// gives, which may change between reads), and the dataset and table lists
// sharedID reads; and a Cloud Storage that counts the data files written.
type icebergFake struct {
	mu    sync.Mutex
	rows  func(read int) string
	reads int
	data  int
	// read, when set, holds each tabledata.list until it is closed: the
	// reads of requests answered at once.
	read chan struct{}
}

func (e *icebergFake) engine(w http.ResponseWriter, r *http.Request) {
	switch p := r.URL.Path; {
	case strings.HasSuffix(p, "/datasets"):
		_, _ = io.WriteString(w, `{"datasets":[{"datasetReference":{"datasetId":"ds"}}]}`)
	case strings.HasSuffix(p, "/datasets/ds/tables"):
		_, _ = io.WriteString(w, `{"tables":[{"tableReference":{"tableId":"t"}}]}`)
	case strings.HasSuffix(p, "/datasets/ds/tables/t"):
		_, _ = io.WriteString(w, `{"creationTime":"1","schema":{"fields":[{"name":"i","type":"INTEGER","mode":"NULLABLE"}]}}`)
	case strings.HasSuffix(p, "/datasets/ds/tables/t/data"):
		if e.read != nil {
			<-e.read
		}
		e.mu.Lock()
		n := e.reads
		e.reads++
		e.mu.Unlock()
		_, _ = io.WriteString(w, `{"rows":`+e.rows(n)+`}`)
	default:
		http.NotFound(w, r)
	}
}

func (e *icebergFake) storage(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Query().Get("name"), "/data/") {
		e.mu.Lock()
		e.data++
		e.mu.Unlock()
	}
	_, _ = io.WriteString(w, `{}`)
}

// icebergFakeTables is a table for Apache Iceberg, gs://b/t, on e.
func icebergFakeTables(t *testing.T, e *icebergFake) (*icebergTables, func(string) front, string) {
	t.Helper()
	gcs := httptest.NewServer(http.HandlerFunc(e.storage))
	t.Cleanup(gcs.Close)
	host := strings.TrimPrefix(gcs.URL, "http://")
	mk := func(base string) front {
		return front{next: http.HandlerFunc(e.engine), base: base, storageHost: host}
	}
	tables := &icebergTables{}
	tables.keep(t.TempDir(), nil)
	base := "/bigquery/v2/projects/p"
	tables.add(mk(base), "ds", "t", "gs://b/t", "1")
	return tables, mk, streamKey("p", "ds", "t")
}

// TestIcebergChangeIsOneSnapshotWhoeverWritesIt (#1427, main run
// 44845978136): every request but a GET writes the files of the tables
// whose rows changed, so requests answered at once each found the same
// change unwritten and committed it, a snapshot each: 3 snapshots of an
// INSERT and an UPDATE. A table is written by one request at a time, which
// finds the change already written when it was not first.
func TestIcebergChangeIsOneSnapshotWhoeverWritesIt(t *testing.T) {
	e := &icebergFake{rows: func(int) string { return `[{"f":[{"v":"1"}]},{"f":[{"v":"2"}]}]` }, read: make(chan struct{})}
	tables, mk, key := icebergFakeTables(t, e)
	r := httptest.NewRequest(http.MethodPost, "/bigquery/v2/projects/p/queries", nil)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, msg := tables.sync(r, mk, key); msg != "" {
				t.Errorf("sync: %s", msg)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond) // the four are under way
	close(e.read)
	wg.Wait()
	if e.data != 1 {
		t.Errorf("four requests at once after one change wrote %d data files, want 1: one snapshot", e.data)
	}
}

// TestIcebergRowsInAnotherOrderAreNoChange (#1427): tabledata.list
// promises no order, and a table whose rows come back in another order has
// not changed, so it is no new snapshot.
func TestIcebergRowsInAnotherOrderAreNoChange(t *testing.T) {
	e := &icebergFake{rows: func(n int) string {
		if n%2 == 0 {
			return `[{"f":[{"v":"1"}]},{"f":[{"v":"2"}]}]`
		}
		return `[{"f":[{"v":"2"}]},{"f":[{"v":"1"}]}]`
	}}
	tables, mk, key := icebergFakeTables(t, e)
	r := httptest.NewRequest(http.MethodPost, "/bigquery/v2/projects/p/queries", nil)
	for range 3 {
		if _, msg := tables.sync(r, mk, key); msg != "" {
			t.Fatalf("sync: %s", msg)
		}
	}
	if e.data != 1 {
		t.Errorf("three reads of the same rows in two orders wrote %d data files, want 1", e.data)
	}
	// A change is still a snapshot.
	e.rows = func(int) string { return `[{"f":[{"v":"3"}]}]` }
	if _, msg := tables.sync(r, mk, key); msg != "" {
		t.Fatalf("sync: %s", msg)
	}
	if e.data != 2 {
		t.Errorf("a change after them wrote %d data files in all, want 2", e.data)
	}
}
