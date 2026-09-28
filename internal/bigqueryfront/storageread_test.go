package bigqueryfront

import (
	"context"
	"errors"
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

// fakeReadServer is the emulator's Storage Read API: one stream a session,
// whose rows are counted by the table it names.
type fakeReadServer struct {
	storagepb.UnimplementedBigQueryReadServer
	mu       sync.Mutex
	sessions int
	reads    int
}

func (s *fakeReadServer) CreateReadSession(_ context.Context, req *storagepb.CreateReadSessionRequest) (*storagepb.ReadSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions++
	return &storagepb.ReadSession{Name: "sess", Table: req.GetReadSession().GetTable(),
		Streams: []*storagepb.ReadStream{{Name: req.GetReadSession().GetTable() + "/streams/1"}}}, nil
}

func (s *fakeReadServer) ReadRows(req *storagepb.ReadRowsRequest, stream storagepb.BigQueryRead_ReadRowsServer) error {
	s.mu.Lock()
	s.reads++
	s.mu.Unlock()
	return stream.Send(&storagepb.ReadRowsResponse{RowCount: 7})
}

// fakeTables is the emulator's REST API for the checks: datasets.list and
// tables.get of the tables it has, "ds.t".
type fakeTables struct {
	mu     sync.Mutex
	tables map[string]bool
}

func (f *fakeTables) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/bigquery/v2/projects/p/")
	if p == "datasets" {
		seen := map[string]bool{}
		var list []map[string]any
		for k := range f.tables {
			ds := k[:strings.Index(k, ".")]
			if !seen[ds] {
				seen[ds] = true
				list = append(list, map[string]any{"datasetReference": map[string]string{"datasetId": ds}})
			}
		}
		writeJSON(w, 200, map[string]any{"datasets": list})
		return
	}
	parts := strings.Split(p, "/")
	if len(parts) == 4 && f.tables[parts[1]+"."+parts[3]] {
		writeJSON(w, 200, map[string]any{})
		return
	}
	writeError(w, 404, "notFound", "not found")
}

func (f *fakeTables) add(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tables[name] = true
}

// TestStorageReadRefusesTablesOfAnotherDatasetsID (#1032): a read session
// of a table whose ID another dataset has too is UNIMPLEMENTED, and so is
// ReadRows of a stream once another dataset has made a table of its ID,
// or of a stream the front did not see made; nothing reaches the emulator.
// Any other read, and every other method, is passed through.
func TestStorageReadRefusesTablesOfAnotherDatasetsID(t *testing.T) {
	emu := &fakeReadServer{}
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	storagepb.RegisterBigQueryReadServer(gs, emu)
	go func() { _ = gs.Serve(up) }()
	defer gs.Stop()

	rest := &fakeTables{tables: map[string]bool{"a.only": true, "a.same": true, "b.same": true, "a.later": true}}
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
	session := func(table string) (*storagepb.ReadSession, error) {
		return c.CreateReadSession(ctx, &storagepb.CreateReadSessionRequest{Parent: "projects/p",
			ReadSession: &storagepb.ReadSession{Table: "projects/p/datasets/" + strings.Replace(table, ".", "/tables/", 1),
				DataFormat: storagepb.DataFormat_ARROW}})
	}
	read := func(stream string) (int64, error) {
		rs, err := c.ReadRows(ctx, &storagepb.ReadRowsRequest{ReadStream: stream})
		if err != nil {
			return 0, err
		}
		var n int64
		for {
			resp, err := rs.Recv()
			if errors.Is(err, io.EOF) {
				return n, nil
			}
			if err != nil {
				return n, err
			}
			n += resp.GetRowCount()
		}
	}
	unimplemented := func(what string, err error, want string) {
		t.Helper()
		if status.Code(err) != codes.Unimplemented || !strings.Contains(status.Convert(err).Message(), want) {
			t.Errorf("%s: %v, want UNIMPLEMENTED naming %q", what, err, want)
		}
	}

	// A table whose ID no other dataset has: read.
	s, err := session("a.only")
	if err != nil || len(s.GetStreams()) != 1 {
		t.Fatalf("a session of a.only: %v %v", s, err)
	}
	if n, err := read(s.Streams[0].Name); err != nil || n != 7 {
		t.Errorf("ReadRows of a.only: %d %v", n, err)
	}

	// b.same shares its ID with a.same: refused, never sent.
	_, err = session("b.same")
	unimplemented("a session of b.same", err, "b.same")
	if emu.sessions != 1 {
		t.Errorf("the emulator got %d sessions, want 1", emu.sessions)
	}

	// A session made before another dataset made a table of its ID: its
	// ReadRows is refused.
	s, err = session("a.later")
	if err != nil {
		t.Fatalf("a session of a.later: %v", err)
	}
	rest.add("c.later")
	_, err = read(s.Streams[0].Name)
	unimplemented("ReadRows of a.later after c.later", err, "a.later")

	// A stream the front did not see made.
	_, err = read("projects/p/locations/l/sessions/x/streams/y")
	unimplemented("ReadRows of an unknown stream", err, "did not see made")
	if emu.reads != 1 {
		t.Errorf("the emulator got %d reads, want 1", emu.reads)
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
