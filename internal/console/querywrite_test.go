package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// writable is a statement box with a read-write mode on paths two deep.
type writable struct {
	fakeProvider
	queried, written *[]string
}

func (writable) QueryHint() string { return "Read-only." }
func (w writable) Query(_ context.Context, _ string, _ []string, statement string) (Listing, error) {
	*w.queried = append(*w.queried, statement)
	return Listing{NameColumn: "id", Items: []Resource{{Name: "1"}}}, nil
}
func (writable) WriteSpec(path []string) *WriteSpec {
	if len(path) < 2 {
		return nil
	}
	return &WriteSpec{Label: "Read-write", Hint: "Writes data.", Target: "database " + path[1]}
}
func (w writable) Write(_ context.Context, _ string, _ []string, statement string) (int64, error) {
	*w.written = append(*w.written, statement)
	if strings.Contains(statement, "dup") {
		return 0, errors.New("ALREADY_EXISTS: Row [1] in table Widgets already exists")
	}
	return 3, nil
}
func (writable) Detail(context.Context, string, []string) (Detail, error) {
	return Detail{Sections: []Section{{ID: "s", Label: "S", Listing: Listing{Items: []Resource{}}}}}, nil
}

func newWritable() (writable, *[]string, *[]string) {
	var q, w []string
	return writable{fakeProvider: fakeProvider{id: "db", title: "DB"}, queried: &q, written: &w}, &q, &w
}

// TestQueryModeChoosesTheCallNotTheText: the request's Mode, not the
// statement, decides whether Query or Write runs (#798). A statement sent
// without read-write never reaches Write, whatever it says; one sent with it
// never reaches Query; and the row count and the provider's error come back.
func TestQueryModeChoosesTheCallNotTheText(t *testing.T) {
	t.Parallel()
	p, queried, written := newWritable()
	srv := serve(t, p)

	if code, body := post(t, srv, "/api/query/db?project=demo",
		`{"Path":["i","d"],"Statement":"DELETE FROM t WHERE true"}`); code != http.StatusOK {
		t.Fatalf("read-only query = %d: %s", code, body)
	}
	if code, body := post(t, srv, "/api/query/db?project=demo",
		`{"Path":["i","d"],"Statement":"SELECT 1","Mode":"read-only"}`); code != http.StatusOK {
		t.Fatalf("explicit read-only query = %d: %s", code, body)
	}
	if len(*written) != 0 || len(*queried) != 2 {
		t.Fatalf("read-only requests reached Write %d times and Query %d, want 0 and 2", len(*written), len(*queried))
	}

	code, body := post(t, srv, "/api/query/db?project=demo",
		`{"Path":["i","d"],"Statement":"INSERT INTO t (id) VALUES (1)","Mode":"read-write"}`)
	if code != http.StatusOK {
		t.Fatalf("read-write = %d: %s", code, body)
	}
	var out struct {
		RowCount  int64
		Operation string
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.RowCount != 3 || out.Operation == "" {
		t.Errorf("read-write answered %s, want rowCount 3 and an operation", body)
	}
	if len(*queried) != 2 || len(*written) != 1 {
		t.Errorf("a read-write request reached Query %d times and Write %d, want 2 and 1", len(*queried), len(*written))
	}
	_, logs := get(t, srv, "/api/logs?operation="+out.Operation, nil)
	if !strings.Contains(logs, "statement changed 3 rows") {
		t.Errorf("no log entry for the write: %s", logs)
	}
	if strings.Contains(logs, "INSERT INTO") {
		t.Error("the statement was written to the record")
	}

	code, body = post(t, srv, "/api/query/db?project=demo",
		`{"Path":["i","d"],"Statement":"INSERT dup","Mode":"read-write"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "already exists") {
		t.Errorf("a failed write = %d %s, want 400 with the provider's message", code, body)
	}
}

// TestReadWriteIsOfferedOnlyWhereItCanSucceed: a provider with no write mode,
// or a path its WriteSpec declines, answers 501 rather than trying; the
// detail page carries the write spec exactly where WriteSpec gives one.
func TestReadWriteIsOfferedOnlyWhereItCanSucceed(t *testing.T) {
	t.Parallel()
	p, _, written := newWritable()
	srv := serve(t, p, &queryable{fakeProvider: fakeProvider{id: "ro", title: "RO"}})

	if code, body := post(t, srv, "/api/query/ro",
		`{"Path":["x","y"],"Statement":"DELETE","Mode":"read-write"}`); code != http.StatusNotImplemented {
		t.Errorf("read-write on a read-only provider = %d, want 501: %s", code, body)
	}
	if code, body := post(t, srv, "/api/query/db?project=demo",
		`{"Path":["i"],"Statement":"DELETE","Mode":"read-write"}`); code != http.StatusNotImplemented {
		t.Errorf("read-write where WriteSpec is nil = %d, want 501: %s", code, body)
	}
	if code, body := post(t, srv, "/api/query/db?project=demo",
		`{"Path":["i","d"],"Statement":"x","Mode":"write"}`); code != http.StatusBadRequest {
		t.Errorf("an unknown mode = %d, want 400: %s", code, body)
	}
	if len(*written) != 0 {
		t.Errorf("a refused request reached Write %d times", len(*written))
	}

	detail := func(name string) Detail {
		t.Helper()
		_, body := get(t, srv, "/api/detail/db?project=demo&name="+strings.ReplaceAll(name, "/", "&name="), nil)
		var d Detail
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := detail("i"); d.Query != nil && d.Query.Write != nil {
		t.Errorf("the instance page offers a write mode: %+v", d.Query.Write)
	}
	d := detail("i/d")
	if d.Query == nil || d.Query.Write == nil || d.Query.Write.Target != "database d" || d.Query.Hint == "" {
		t.Errorf("the database page's query spec = %+v, want the read-only hint and a write mode naming database d", d.Query)
	}
}
