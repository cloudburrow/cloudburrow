package bigqueryfront

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// fakeTable is a table (or a view, with a query) of fakeTables.
type fakeTable struct {
	schema string
	rows   int64
	query  string
}

// fakeTables is the emulator's REST API for the Storage Read front: the
// datasets and tables it has ("ds.t"), in the order they were made, and
// the views the front makes (tables.insert, tables.patch, tables.delete).
type fakeTables struct {
	mu       sync.Mutex
	datasets []string
	tables   map[string]*fakeTable
	order    []string
	// inserts and deletes count the views made and deleted.
	inserts, deletes int
}

func newFakeTables() *fakeTables { return &fakeTables{tables: map[string]*fakeTable{}} }

func (f *fakeTables) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	p := strings.TrimPrefix(r.URL.Path, "/bigquery/v2/projects/p/")
	parts := strings.Split(p, "/")
	switch {
	case p == "datasets" && r.Method == http.MethodGet:
		var list []map[string]any
		for _, ds := range f.datasets {
			list = append(list, map[string]any{"datasetReference": map[string]string{"datasetId": ds}})
		}
		writeJSON(w, 200, map[string]any{"datasets": list})
	case p == "datasets" && r.Method == http.MethodPost:
		var body struct {
			DatasetReference struct{ DatasetID string } `json:"datasetReference"`
		}
		_ = json.Unmarshal(b, &body)
		f.datasets = append(f.datasets, body.DatasetReference.DatasetID)
		writeJSON(w, 200, map[string]any{})
	case len(parts) == 3 && parts[2] == "tables" && r.Method == http.MethodGet:
		var list []map[string]any
		for _, k := range f.order {
			if ds, t, _ := strings.Cut(k, "."); ds == parts[1] {
				list = append(list, map[string]any{"tableReference": map[string]string{"tableId": t}})
			}
		}
		writeJSON(w, 200, map[string]any{"tables": list})
	case len(parts) == 3 && parts[2] == "tables" && r.Method == http.MethodPost:
		if !f.hasDataset(parts[1]) {
			writeError(w, 404, "notFound", "dataset not found")
			return
		}
		var body struct {
			TableReference struct{ TableID string } `json:"tableReference"`
			View           struct{ Query string }   `json:"view"`
		}
		_ = json.Unmarshal(b, &body)
		f.inserts++
		f.put(parts[1]+"."+body.TableReference.TableID, &fakeTable{schema: `{"fields":[{"mode":"NULLABLE","name":"s","type":"STRING"}]}`, query: body.View.Query})
		writeJSON(w, 200, map[string]any{})
	case len(parts) == 4 && f.tables[parts[1]+"."+parts[3]] != nil:
		t := f.tables[parts[1]+"."+parts[3]]
		switch r.Method {
		case http.MethodGet:
			out := map[string]any{"schema": json.RawMessage(t.schema)}
			if t.query != "" {
				out["view"] = map[string]string{"query": t.query}
			}
			writeJSON(w, 200, out)
		case http.MethodPatch:
			var body struct {
				Schema json.RawMessage `json:"schema"`
			}
			_ = json.Unmarshal(b, &body)
			t.schema = string(body.Schema)
			writeJSON(w, 200, map[string]any{})
		case http.MethodDelete:
			f.deletes++
			delete(f.tables, parts[1]+"."+parts[3])
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		writeError(w, 404, "notFound", "not found")
	}
}

func (f *fakeTables) hasDataset(ds string) bool {
	for _, d := range f.datasets {
		if d == ds {
			return true
		}
	}
	return false
}

// put makes (or replaces) the table name, and its dataset.
func (f *fakeTables) put(name string, t *fakeTable) {
	if ds, _, _ := strings.Cut(name, "."); !f.hasDataset(ds) {
		f.datasets = append(f.datasets, ds)
	}
	if _, ok := f.tables[name]; !ok {
		f.order = append(f.order, name)
	}
	f.tables[name] = t
}

func (f *fakeTables) add(name string, rows int64, schema string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.put(name, &fakeTable{schema: schema, rows: rows})
}

// rowsOf returns the rows the emulator reads for a session of the table
// path: a view's are its query's table's; a table's are those of the
// first table of its ID in any dataset, as the pinned engine reads a
// bare ID (#1032).
func (f *fakeTables) rowsOf(path string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt, _ := parseReadTable(path)
	if t := f.tables[rt.dataset+"."+rt.table]; t != nil && t.query != "" {
		name := strings.Trim(strings.TrimPrefix(t.query, "SELECT * FROM "), "`")
		if u := f.tables[name]; u != nil {
			return u.rows
		}
		return -1
	}
	for _, k := range f.order {
		if _, id, _ := strings.Cut(k, "."); id == rt.table && f.tables[k] != nil {
			return f.tables[k].rows
		}
	}
	return -1
}

// fakeReadServer is the emulator's Storage Read API: one stream a session,
// whose rows are read as fakeTables.rowsOf reads them, with an Avro
// schema written as the emulator writes one for the session's table.
type fakeReadServer struct {
	storagepb.UnimplementedBigQueryReadServer
	rest     *fakeTables
	mu       sync.Mutex
	sessions []string
	reads    int
	streams  map[string]*storagepb.ReadSession
}

func avroOf(path string) string {
	t, _ := parseReadTable(path)
	return `{"namespace":"` + t.project + "." + t.dataset + `","name":"` + t.table + `","type":"record","fields":[]}`
}

func (s *fakeReadServer) CreateReadSession(_ context.Context, req *storagepb.CreateReadSessionRequest) (*storagepb.ReadSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	table := req.GetReadSession().GetTable()
	s.sessions = append(s.sessions, table)
	sess := &storagepb.ReadSession{Name: "sess", Table: table, DataFormat: req.GetReadSession().GetDataFormat(),
		Streams: []*storagepb.ReadStream{{Name: fmt.Sprintf("%s/streams/%d", table, len(s.sessions))}}}
	if sess.DataFormat == storagepb.DataFormat_AVRO {
		sess.Schema = &storagepb.ReadSession_AvroSchema{AvroSchema: &storagepb.AvroSchema{Schema: avroOf(table)}}
	}
	if s.streams == nil {
		s.streams = map[string]*storagepb.ReadSession{}
	}
	s.streams[sess.Streams[0].Name] = sess
	return sess, nil
}

func (s *fakeReadServer) ReadRows(req *storagepb.ReadRowsRequest, stream storagepb.BigQueryRead_ReadRowsServer) error {
	s.mu.Lock()
	s.reads++
	sess := s.streams[req.GetReadStream()]
	s.mu.Unlock()
	if sess == nil {
		return status.Error(codes.NotFound, "no such stream")
	}
	resp := &storagepb.ReadRowsResponse{RowCount: s.rest.rowsOf(sess.Table)}
	if sess.DataFormat == storagepb.DataFormat_AVRO {
		resp.Schema = &storagepb.ReadRowsResponse_AvroSchema{AvroSchema: &storagepb.AvroSchema{Schema: avroOf(sess.Table)}}
	}
	return stream.Send(resp)
}

// TestStorageReadOfATableIDAnotherDatasetHas (#1032, #1046): a read
// session of a table whose ID another dataset has too is sent to the
// emulator for the front's view of the table, made once with the table's
// schema and made again when that changes, and streams the table's own
// rows; the session names the client's table, as does an Avro schema. A
// stream of a table whose ID another dataset made a table of after the
// session is read through the view too; one the front did not see made is
// UNIMPLEMENTED. Any other read, and every other method, is passed
// through.
func TestStorageReadOfATableIDAnotherDatasetHas(t *testing.T) {
	const schemaS = `{"fields":[{"name":"s","type":"STRING","mode":"REQUIRED"}]}`
	rest := newFakeTables()
	rest.add("a.only", 1, schemaS)
	rest.add("a.same", 3, schemaS)
	rest.add("b.same", 2, `{"fields":[{"name":"a","type":"INTEGER","mode":"REQUIRED","description":"d"}]}`)
	rest.add("a.later", 4, schemaS)
	emu := &fakeReadServer{rest: rest}
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	storagepb.RegisterBigQueryReadServer(gs, emu)
	go func() { _ = gs.Serve(up) }()
	defer gs.Stop()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- ServeStorageRead(ctx, l, up.Addr().String(), rest) }()

	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := storagepb.NewBigQueryReadClient(conn)
	path := func(table string) string { return "projects/p/datasets/" + strings.Replace(table, ".", "/tables/", 1) }
	session := func(table string, format storagepb.DataFormat) (*storagepb.ReadSession, error) {
		return c.CreateReadSession(ctx, &storagepb.CreateReadSessionRequest{Parent: "projects/p",
			ReadSession: &storagepb.ReadSession{Table: path(table), DataFormat: format}})
	}
	// read returns the rows streamed and the Avro schemas sent with them.
	read := func(stream string) (int64, []string, error) {
		rs, err := c.ReadRows(ctx, &storagepb.ReadRowsRequest{ReadStream: stream})
		if err != nil {
			return 0, nil, err
		}
		var n int64
		var schemas []string
		for {
			resp, err := rs.Recv()
			if errors.Is(err, io.EOF) {
				return n, schemas, nil
			}
			if err != nil {
				return n, schemas, err
			}
			n += resp.GetRowCount()
			if a := resp.GetAvroSchema(); a != nil {
				schemas = append(schemas, a.GetSchema())
			}
		}
	}
	readAll := func(table string, format storagepb.DataFormat) (*storagepb.ReadSession, int64, []string) {
		t.Helper()
		s, err := session(table, format)
		if err != nil || len(s.GetStreams()) != 1 {
			t.Fatalf("a session of %s: %v %v", table, s, err)
		}
		n, schemas, err := read(s.Streams[0].Name)
		if err != nil {
			t.Fatalf("ReadRows of %s: %v", table, err)
		}
		return s, n, schemas
	}
	alias := "projects/p/datasets/" + readAliasDataset + "/tables/" + aliasID("b", "same")

	// A table whose ID no other dataset has: read as it is.
	if s, n, _ := readAll("a.only", storagepb.DataFormat_ARROW); n != 1 || s.Table != path("a.only") {
		t.Errorf("a.only: %d rows, table %q", n, s.Table)
	}

	// b.same shares its ID with a.same, made first: read through the
	// front's view, which has b.same's schema, and gives b.same's rows.
	s, n, _ := readAll("b.same", storagepb.DataFormat_ARROW)
	if n != 2 || s.Table != path("b.same") {
		t.Errorf("b.same: %d rows (want its 2), table %q", n, s.Table)
	}
	if got := emu.sessions[len(emu.sessions)-1]; got != alias {
		t.Errorf("the emulator's session of b.same was of %s, want %s", got, alias)
	}
	if v := rest.tables[readAliasDataset+"."+aliasID("b", "same")]; v == nil || v.schema != rest.tables["b.same"].schema ||
		v.query != "SELECT * FROM `b.same`" {
		t.Errorf("the view of b.same: %+v", v)
	}
	if _, n, _ := readAll("a.same", storagepb.DataFormat_ARROW); n != 3 {
		t.Errorf("a.same: %d rows, want its 3", n)
	}

	// Avro: the schemas name the client's table.
	s, n, schemas := readAll("b.same", storagepb.DataFormat_AVRO)
	want := `{"fields":[],"name":"same","namespace":"p.b","type":"record"}`
	if n != 2 || s.GetAvroSchema().GetSchema() != want || len(schemas) != 1 || schemas[0] != want {
		t.Errorf("b.same in Avro: %d rows, session schema %q, rows' schemas %q; want %q", n, s.GetAvroSchema().GetSchema(), schemas, want)
	}
	// Four sessions of the two same, one view each, none made twice.
	if rest.inserts != 2 || rest.deletes != 0 {
		t.Errorf("views made %d, deleted %d; want 2 and 0", rest.inserts, rest.deletes)
	}

	// The table's schema changes: its view is made again.
	rest.add("b.same", 5, `{"fields":[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"}]}`)
	if _, n, _ := readAll("b.same", storagepb.DataFormat_ARROW); n != 5 {
		t.Errorf("b.same after its schema changed: %d rows, want 5", n)
	}
	if v := rest.tables[readAliasDataset+"."+aliasID("b", "same")]; rest.inserts != 3 || rest.deletes != 1 || v.schema != rest.tables["b.same"].schema {
		t.Errorf("after a schema change: views made %d, deleted %d, view %+v", rest.inserts, rest.deletes, v)
	}

	// A session made before another dataset made a table of its ID: its
	// rows are read through the view.
	s, err = session("a.later", storagepb.DataFormat_ARROW)
	if err != nil {
		t.Fatalf("a session of a.later: %v", err)
	}
	if got := emu.sessions[len(emu.sessions)-1]; got != path("a.later") {
		t.Errorf("the session of a.later was sent for %s", got)
	}
	rest.add("c.later", 6, schemaS)
	if n, _, err := read(s.Streams[0].Name); err != nil || n != 4 {
		t.Errorf("ReadRows of a.later after c.later: %d %v, want its 4", n, err)
	}
	if got := emu.sessions[len(emu.sessions)-1]; got != "projects/p/datasets/"+readAliasDataset+"/tables/"+aliasID("a", "later") {
		t.Errorf("ReadRows of a.later read %s", got)
	}

	// A stream the front did not see made.
	_, _, err = read("projects/p/locations/l/sessions/x/streams/y")
	if status.Code(err) != codes.Unimplemented || !strings.Contains(status.Convert(err).Message(), "did not see made") {
		t.Errorf("ReadRows of an unknown stream: %v, want UNIMPLEMENTED", err)
	}

	// Another method is passed through, with the emulator's answer.
	_, err = c.SplitReadStream(ctx, &storagepb.SplitReadStreamRequest{Name: "x"})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(status.Convert(err).Message(), "SplitReadStream not implemented") {
		t.Errorf("SplitReadStream: %v, want the emulator's own answer", err)
	}

	cancel()
	if err := <-served; err != nil {
		t.Errorf("ServeStorageRead: %v", err)
	}
}

func TestParseReadTable(t *testing.T) {
	for path, want := range map[string]bool{
		"projects/p/datasets/d/tables/t": true,
		"projects/p/datasets/d/tables/":  false,
		"projects/p/datasets/d":          false,
		"projects/p/tables/t/datasets/d": false,
	} {
		if _, ok := parseReadTable(path); ok != want {
			t.Errorf("parseReadTable(%q) = %v, want %v", path, ok, want)
		}
	}
}
