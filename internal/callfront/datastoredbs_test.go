package callfront

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

// fakeDatastore keeps the entities commits write, by key name, and answers
// a query with every entity of its kind whose "project" is the filter's.
type fakeDatastore struct {
	pb.UnimplementedDatastoreServer
	mu       sync.Mutex
	entities map[string]*pb.Entity
}

func (f *fakeDatastore) Commit(_ context.Context, req *pb.CommitRequest) (*pb.CommitResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range req.GetMutations() {
		if e := m.GetUpsert(); e != nil {
			f.entities[e.GetKey().GetPath()[0].GetName()] = e
		}
		if k := m.GetDelete(); k != nil {
			delete(f.entities, k.GetPath()[0].GetName())
		}
	}
	return &pb.CommitResponse{}, nil
}

func (f *fakeDatastore) RunQuery(_ context.Context, req *pb.RunQueryRequest) (*pb.RunQueryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := req.GetQuery().GetFilter().GetPropertyFilter().GetValue().GetStringValue()
	batch := &pb.QueryResultBatch{}
	for _, e := range f.entities {
		if e.GetProperties()["project"].GetStringValue() == want {
			batch.EntityResults = append(batch.EntityResults, &pb.EntityResult{Entity: e})
		}
	}
	return &pb.RunQueryResponse{Batch: batch}, nil
}

// TestNamedDatastoreDatabasesAreKeptForThePurge (#1550): the call front
// records each named database a call names, over gRPC (its routing header)
// and over REST (its body's databaseId, which still reaches the emulator
// whole), and ForgetPath answers a project's and forgets them.
func TestNamedDatastoreDatabasesAreKeptForThePurge(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeDatastore{entities: map[string]*pb.Entity{}}
	srv := grpc.NewServer()
	pb.RegisterDatastoreServer(srv, fake)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)

	var gotBody []byte
	emu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(emu.Close)
	dbs := newDatastoreDatabases(l.Addr().String(), t.Logf)
	front := httptest.NewServer(HandlerWith(strings.TrimPrefix(emu.URL, "http://"), &calllog.Ring{}, t.Logf, Intercepts{dbs}))
	t.Cleanup(front.Close)

	// gRPC names it in its routing header, which netfwd reads into the
	// call's resource; the front is shown that.
	dbs.SawResource("projects/gone/databases/by-grpc")
	dbs.SawResource("projects/gone/databases/(default)")
	dbs.SawResource("projects/kept/databases/other")
	body := `{"databaseId":"by-rest","mutations":[]}`
	resp, err := http.Post(front.URL+"/v1/projects/gone:commit", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if string(gotBody) != body {
		t.Errorf("the emulator got %q, want the body whole, %q", gotBody, body)
	}
	pbBody, _ := proto.Marshal(&pb.LookupRequest{ProjectId: "gone", DatabaseId: "by-proto"})
	resp, err = http.Post(front.URL+"/v1/projects/gone:lookup", "application/x-protobuf", bytes.NewReader(pbBody))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !bytes.Equal(gotBody, pbBody) {
		t.Errorf("the emulator got %x, want the body whole, %x", gotBody, pbBody)
	}

	// A front started again, in place of the first, reads the markers
	// from the emulator.
	front.Close()
	dbs = newDatastoreDatabases(l.Addr().String(), t.Logf)
	again := httptest.NewServer(HandlerWith(strings.TrimPrefix(emu.URL, "http://"), &calllog.Ring{}, t.Logf, Intercepts{dbs}))
	t.Cleanup(again.Close)
	forget := func(url, project string) []string {
		t.Helper()
		resp, err := http.Post(url+frontstate.DatastoreForgetPath+"?project="+project, "", nil)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("forget %s = %v, %v", project, resp, err)
		}
		defer resp.Body.Close()
		var out struct{ Databases []string }
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Databases
	}
	if got, want := forget(again.URL, "gone"), []string{"by-grpc", "by-proto", "by-rest"}; !reflect.DeepEqual(got, want) {
		t.Errorf("gone's named databases = %v, want %v", got, want)
	}
	if got := forget(again.URL, "gone"); len(got) != 0 {
		t.Errorf("gone's named databases once forgotten = %v, want none", got)
	}
	if got, want := forget(again.URL, "kept"), []string{"other"}; !reflect.DeepEqual(got, want) {
		t.Errorf("kept's named databases = %v, want %v", got, want)
	}
	// Forgotten, a database is recorded again when a call names it.
	dbs.SawResource("projects/gone/databases/by-grpc")
	if got, want := forget(again.URL, "gone"), []string{"by-grpc"}; !reflect.DeepEqual(got, want) {
		t.Errorf("gone's named databases named again = %v, want %v", got, want)
	}
}
