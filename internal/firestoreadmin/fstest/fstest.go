// Package fstest is a stand-in for the Firestore emulator's documents API
// and for Cloud Storage, in memory, for tests of the Firestore Admin API's
// exports, imports and backups (internal/firestoreadmin).
package fstest

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Serve serves a new Docs over gRPC and a new GCS over HTTP until the test
// ends, and returns them with their addresses.
func Serve(t testing.TB) (docs *Docs, docsAddr string, gcs *GCS, gcsURL string) {
	t.Helper()
	docs = &Docs{docs: map[string]*firestorepb.Document{}}
	srv := grpc.NewServer()
	firestorepb.RegisterFirestoreServer(srv, docs)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	gcs = &GCS{Objects: map[string][]byte{}}
	hs := httptest.NewServer(gcs)
	t.Cleanup(hs.Close)
	return docs, l.Addr().String(), gcs, hs.URL
}

// Docs is the Firestore emulator's documents API, in memory: enough of
// ListCollectionIds, ListDocuments and Commit for the admin's walks.
type Docs struct {
	firestorepb.UnimplementedFirestoreServer
	mu   sync.Mutex
	docs map[string]*firestorepb.Document
}

func (f *Docs) ListCollectionIds(_ context.Context, r *firestorepb.ListCollectionIdsRequest) (*firestorepb.ListCollectionIdsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	set := map[string]bool{}
	for n := range f.docs {
		if rest, ok := strings.CutPrefix(n, r.Parent+"/"); ok {
			set[strings.SplitN(rest, "/", 2)[0]] = true
		}
	}
	out := &firestorepb.ListCollectionIdsResponse{}
	for id := range set {
		out.CollectionIds = append(out.CollectionIds, id)
	}
	sort.Strings(out.CollectionIds)
	return out, nil
}

func (f *Docs) ListDocuments(_ context.Context, r *firestorepb.ListDocumentsRequest) (*firestorepb.ListDocumentsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := r.Parent + "/" + r.CollectionId + "/"
	seen := map[string]*firestorepb.Document{}
	for n, d := range f.docs {
		rest, ok := strings.CutPrefix(n, prefix)
		if !ok {
			continue
		}
		id := strings.SplitN(rest, "/", 2)[0]
		if !strings.Contains(rest, "/") {
			seen[prefix+id] = d
		} else if _, ok := seen[prefix+id]; !ok {
			seen[prefix+id] = &firestorepb.Document{Name: prefix + id} // missing
		}
	}
	out := &firestorepb.ListDocumentsResponse{}
	for _, d := range seen {
		out.Documents = append(out.Documents, d)
	}
	sort.Slice(out.Documents, func(i, j int) bool { return out.Documents[i].Name < out.Documents[j].Name })
	return out, nil
}

func (f *Docs) Commit(_ context.Context, r *firestorepb.CommitRequest) (*firestorepb.CommitResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range r.Writes {
		switch op := w.Operation.(type) {
		case *firestorepb.Write_Update:
			d := op.Update
			d.CreateTime, d.UpdateTime = timestamppb.Now(), timestamppb.Now()
			f.docs[d.Name] = d
		case *firestorepb.Write_Delete:
			delete(f.docs, op.Delete)
		}
	}
	return &firestorepb.CommitResponse{}, nil
}

// Names is the paths of the documents of the database db.
func (f *Docs) Names(db string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.docs {
		if strings.HasPrefix(n, db+"/documents/") {
			out = append(out, strings.TrimPrefix(n, db+"/documents/"))
		}
	}
	sort.Strings(out)
	return out
}

// GCS is enough of Cloud Storage's JSON API for exports: one bucket,
// "exports".
type GCS struct {
	mu      sync.Mutex
	Objects map[string][]byte
}

func (g *GCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/storage/v1/b/exports":
		_, _ = io.WriteString(w, `{"name":"exports"}`)
	case r.Method == http.MethodPost && r.URL.Path == "/upload/storage/v1/b/exports/o":
		b, _ := io.ReadAll(r.Body)
		g.Objects[r.URL.Query().Get("name")] = b
		_, _ = io.WriteString(w, `{}`)
	case r.Method == http.MethodGet && r.URL.Path == "/storage/v1/b/exports/o":
		var items []string
		for n := range g.Objects {
			if strings.HasPrefix(n, r.URL.Query().Get("prefix")) {
				items = append(items, `{"name":"`+n+`"}`)
			}
		}
		_, _ = io.WriteString(w, `{"items":[`+strings.Join(items, ",")+`]}`)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/storage/v1/b/exports/o/"):
		b, ok := g.Objects[strings.TrimPrefix(r.URL.Path, "/storage/v1/b/exports/o/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

// Seed writes four documents and one under a missing parent into db.
func (f *Docs) Seed(db string) {
	for _, n := range []string{"cities/sf", "cities/la", "cities/sf/landmarks/bridge", "users/ada", "orphans/missing/kids/k1"} {
		_, _ = f.Commit(context.Background(), &firestorepb.CommitRequest{Writes: []*firestorepb.Write{{Operation: &firestorepb.Write_Update{
			Update: &firestorepb.Document{Name: db + "/documents/" + n, Fields: map[string]*firestorepb.Value{
				"n": {ValueType: &firestorepb.Value_StringValue{StringValue: n}}}}}}}})
	}
}
