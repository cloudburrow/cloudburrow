package bigqueryfront

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// countingEngine answers every request 200 with a body naming how many
// requests of that method and path it has had, or 404 for a path
// containing "missing", and counts them.
type countingEngine struct{ calls map[string]int }

func (e *countingEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	e.calls[key]++
	if strings.Contains(r.URL.Path, "missing") {
		writeError(w, http.StatusNotFound, "notFound", "Not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"n":`+string(rune('0'+e.calls[key]))+`}`)
}

func memoGet(t *testing.T, h http.Handler, ctx context.Context, method, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// #1623: within one client request, a repeated datasets.get or
// tables.get is the engine's first answer, until anything else goes to
// the engine; another request reads again.
func TestMemoReadsWithinOneRequest(t *testing.T) {
	engine := &countingEngine{calls: map[string]int{}}
	h := memoReads(engine)
	ctx := withReadMemo(context.Background())
	table := "/projects/p/datasets/d/tables/t"
	dataset := "/projects/p/datasets/d"
	for i := 0; i < 3; i++ {
		if code, body := memoGet(t, h, ctx, http.MethodGet, table); code != http.StatusOK || body != `{"n":1}` {
			t.Fatalf("read %d of the table: %d %s, want the first answer", i, code, body)
		}
		if code, body := memoGet(t, h, ctx, http.MethodGet, dataset); code != http.StatusOK || body != `{"n":1}` {
			t.Fatalf("read %d of the dataset: %d %s, want the first answer", i, code, body)
		}
	}
	if engine.calls["GET "+table] != 1 || engine.calls["GET "+dataset] != 1 {
		t.Fatalf("engine reads %v, want one of each", engine.calls)
	}
	// A write, and any other request, forgets them.
	for _, other := range [][2]string{
		{http.MethodPost, "/projects/p/queries"},
		{http.MethodGet, "/projects/p/jobs/j"},
		{http.MethodGet, "/projects/p/datasets/d/tables"},
		{http.MethodGet, table + "/data"},
	} {
		before := engine.calls["GET "+table]
		memoGet(t, h, ctx, other[0], other[1])
		memoGet(t, h, ctx, http.MethodGet, table)
		memoGet(t, h, ctx, http.MethodGet, table)
		if got := engine.calls["GET "+table]; got != before+1 {
			t.Errorf("after %s %s: the table read %d more times, want once", other[0], other[1], got-before)
		}
	}
	// A failure is not kept.
	memoGet(t, h, ctx, http.MethodGet, "/projects/p/datasets/missing")
	if code, _ := memoGet(t, h, ctx, http.MethodGet, "/projects/p/datasets/missing"); code != http.StatusNotFound ||
		engine.calls["GET /projects/p/datasets/missing"] != 2 {
		t.Errorf("a 404 was kept: %d, %v", code, engine.calls)
	}
	// Another request, and a request with no memo, read the engine.
	before := engine.calls["GET "+table]
	memoGet(t, h, withReadMemo(context.Background()), http.MethodGet, table)
	memoGet(t, h, context.Background(), http.MethodGet, table)
	memoGet(t, h, context.Background(), http.MethodGet, table)
	if got := engine.calls["GET "+table] - before; got != 3 {
		t.Errorf("reads outside the first request: %d engine reads, want 3", got)
	}
}

// #1623: a streamed insert reads its table from the engine once, though
// two checks of the front's read it.
func TestInsertAllReadsItsTableOnce(t *testing.T) {
	engine := &countingEngine{calls: map[string]int{}}
	schema := `{"tableReference":{"projectId":"p","datasetId":"d","tableId":"t"},"type":"TABLE",` +
		`"schema":{"fields":[{"name":"id","type":"INTEGER"}]}}`
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		engine.calls[key]++
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, schema)
			return
		}
		_, _ = io.WriteString(w, `{"kind":"bigquery#tableDataInsertAllResponse"}`)
	}))
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/projects/p/datasets/d/tables/t/insertAll",
			strings.NewReader(`{"rows":[{"json":{"id":1}}]}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("insertAll: %d %s", rec.Code, rec.Body.String())
		}
	}
	if got := engine.calls["GET /projects/p/datasets/d/tables/t"]; got != 2 {
		t.Errorf("two inserts read the table %d times, want once each (%v)", got, engine.calls)
	}
	if got := engine.calls["POST /projects/p/datasets/d/tables/t/insertAll"]; got != 2 {
		t.Errorf("the engine had %d inserts, want 2", got)
	}
}
