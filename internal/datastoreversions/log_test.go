package datastoreversions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// fake is an emulator: entities by database and key, each with a version.
type fake struct {
	pb.DatastoreClient
	mu   sync.Mutex
	ents map[string]map[string]*pb.EntityResult
	ver  int64
}

func newFake() *fake { return &fake{ents: map[string]map[string]*pb.EntityResult{}} }

func (f *fake) db(project, database string) map[string]*pb.EntityResult {
	n := DBName(project, emulatorDB(database))
	if f.ents[n] == nil {
		f.ents[n] = map[string]*pb.EntityResult{}
	}
	return f.ents[n]
}

func (f *fake) Lookup(_ context.Context, req *pb.LookupRequest, _ ...grpc.CallOption) (*pb.LookupResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &pb.LookupResponse{}
	d := f.db(req.GetProjectId(), req.GetDatabaseId())
	for _, k := range req.GetKeys() {
		if r, ok := d[keyID(k)]; ok {
			out.Found = append(out.Found, proto.Clone(r).(*pb.EntityResult))
		} else {
			out.Missing = append(out.Missing, &pb.EntityResult{Entity: &pb.Entity{Key: k}})
		}
	}
	return out, nil
}

func (f *fake) Commit(_ context.Context, req *pb.CommitRequest, _ ...grpc.CallOption) (*pb.CommitResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.db(req.GetProjectId(), req.GetDatabaseId())
	out := &pb.CommitResponse{}
	for _, m := range req.GetMutations() {
		f.ver++
		k, e := mutationKey(m)
		k = proto.Clone(k).(*pb.Key)
		if last := k.Path[len(k.Path)-1]; last.GetId() == 0 && last.GetName() == "" {
			last.IdType = &pb.Key_PathElement_Id{Id: f.ver}
		}
		if m.GetInsert() != nil {
			if _, ok := d[keyID(k)]; ok {
				return nil, status.Error(codes.AlreadyExists, "entity already exists")
			}
		}
		if e == nil {
			delete(d, keyID(k))
		} else {
			e = proto.Clone(e).(*pb.Entity)
			e.Key = k
			d[keyID(k)] = &pb.EntityResult{Entity: e, Version: f.ver}
		}
		out.MutationResults = append(out.MutationResults, &pb.MutationResult{Key: k, Version: f.ver})
	}
	return out, nil
}

func (f *fake) RunQuery(_ context.Context, req *pb.RunQueryRequest, _ ...grpc.CallOption) (*pb.RunQueryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.db(req.GetProjectId(), req.GetDatabaseId())
	q := req.GetQuery()
	b := &pb.QueryResultBatch{MoreResults: pb.QueryResultBatch_NO_MORE_RESULTS}
	if len(q.GetKind()) == 1 && q.GetKind()[0].GetName() == "__namespace__" {
		seen := map[string]bool{}
		for _, r := range d {
			if ns := r.GetEntity().GetKey().GetPartitionId().GetNamespaceId(); ns != "" && !seen[ns] {
				seen[ns] = true
				b.EntityResults = append(b.EntityResults, &pb.EntityResult{Entity: &pb.Entity{Key: &pb.Key{
					Path: []*pb.Key_PathElement{{Kind: "__namespace__", IdType: &pb.Key_PathElement_Name{Name: ns}}}}}})
			}
		}
		return &pb.RunQueryResponse{Batch: b}, nil
	}
	for _, r := range d {
		k := r.GetEntity().GetKey()
		if k.GetPartitionId().GetNamespaceId() != req.GetPartitionId().GetNamespaceId() {
			continue
		}
		if len(q.GetKind()) == 1 && kindOf(k) != q.GetKind()[0].GetName() {
			continue
		}
		b.EntityResults = append(b.EntityResults, proto.Clone(r).(*pb.EntityResult))
	}
	return &pb.RunQueryResponse{Batch: b}, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

const marker = "marker-project"

func key(name string) *pb.Key {
	return &pb.Key{PartitionId: &pb.PartitionId{ProjectId: "p"}, Path: []*pb.Key_PathElement{{Kind: "Task", IdType: &pb.Key_PathElement_Name{Name: name}}}}
}

func entity(name string, v int64) *pb.Entity {
	return &pb.Entity{Key: key(name), Properties: map[string]*pb.Value{"n": num(v)}}
}

func upsert(t *testing.T, c pb.DatastoreClient, e *pb.Entity) {
	t.Helper()
	if _, err := c.Commit(context.Background(), &pb.CommitRequest{ProjectId: "p", Mode: pb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*pb.Mutation{{Operation: &pb.Mutation_Upsert{Upsert: e}}}}); err != nil {
		t.Fatal(err)
	}
}

func del(t *testing.T, c pb.DatastoreClient, k *pb.Key) {
	t.Helper()
	if _, err := c.Commit(context.Background(), &pb.CommitRequest{ProjectId: "p", Mode: pb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*pb.Mutation{{Operation: &pb.Mutation_Delete{Delete: k}}}}); err != nil {
		t.Fatal(err)
	}
}

// open is a log over the fake, open, its clock clk.
func open(t *testing.T, f *fake, dir string, clk *clock, retention time.Duration) (*Log, context.CancelFunc) {
	t.Helper()
	l := New(func(string) time.Duration { return retention }, t.Logf)
	l.SetClock(clk.now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.Open(ctx, f, marker, dir) }()
	if err := l.waitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	return l, func() { cancel(); <-done }
}

// n is the entity's n at t, or -1 for none.
func lookupAt(t *testing.T, l *Log, f *fake, name string, at time.Time) int64 {
	t.Helper()
	resp, err := l.LookupAt(context.Background(), f, &pb.LookupRequest{ProjectId: "p", Keys: []*pb.Key{key(name)}}, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetFound()) == 0 {
		return -1
	}
	return resp.GetFound()[0].GetEntity().GetProperties()["n"].GetIntegerValue()
}

func TestVersionsServeEarlierTimes(t *testing.T) {
	f := newFake()
	clk := &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	upsert(t, f, entity("old", 7)) // before the log, never changed
	upsert(t, f, entity("a", 0))   // before the log
	l, stop := open(t, f, "", clk, time.Hour)
	defer stop()
	c := l.Client(f)
	t0 := clk.add(time.Second)
	t1 := clk.add(time.Second)
	upsert(t, c, entity("a", 1))
	upsert(t, c, entity("b", 1))
	t2 := clk.add(time.Second)
	upsert(t, c, entity("a", 2))
	t3 := clk.add(time.Second)
	del(t, c, key("a"))
	t4 := clk.add(time.Second)
	for _, tc := range []struct {
		name string
		at   time.Time
		want int64
	}{
		{"a", t0, 0}, {"a", t1, 1}, {"a", t2, 2}, {"a", t3, -1}, {"a", t4, -1},
		{"b", t0, -1}, {"b", t1, 1}, {"b", t4, 1},
		{"old", t0, 7}, {"old", t4, 7},
	} {
		if got := lookupAt(t, l, f, tc.name, tc.at.Add(time.Millisecond/2)); got != tc.want {
			t.Errorf("%s at %s = %d; want %d", tc.name, tc.at.Sub(t0), got, tc.want)
		}
	}
	// An export at t1: a as it was, b, old.
	es, err := l.At(context.Background(), "p", "", t1.Add(time.Millisecond), EntityFilter{}, func() ([]*pb.Entity, error) {
		return readAll(context.Background(), f, "p", "")
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, e := range es {
		got[e.GetKey().GetPath()[0].GetName()] = e.GetProperties()["n"].GetIntegerValue()
	}
	if len(got) != 3 || got["a"] != 1 || got["b"] != 1 || got["old"] != 7 {
		t.Errorf("entities at t1 = %v; want a=1 b=1 old=7", got)
	}
}

func TestVersionsPersistAndReset(t *testing.T) {
	f := newFake()
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	l, stop := open(t, f, dir, clk, time.Hour)
	c := l.Client(f)
	upsert(t, c, entity("a", 1))
	t1 := clk.add(time.Second)
	upsert(t, c, entity("a", 2))
	stop()

	// The front restarts; the emulator kept its entities.
	l, stop = open(t, f, dir, clk, time.Hour)
	if got := lookupAt(t, l, f, "a", t1.Add(-time.Millisecond)); got != 1 {
		t.Errorf("after a restart, a at t1 = %d; want 1", got)
	}
	stop()

	// The emulator lost its entities (its marker with them).
	f.mu.Lock()
	f.ents = map[string]map[string]*pb.EntityResult{}
	f.mu.Unlock()
	t2 := clk.add(time.Second)
	l, stop = open(t, f, dir, clk, time.Hour)
	defer stop()
	if s := l.Since("p", ""); s.Before(t2) {
		t.Errorf("since = %s; want no earlier than %s, when the log began again", s, t2)
	}
	if l.Bytes() != 0 {
		t.Errorf("the log holds %d bytes after the reset; want none", l.Bytes())
	}
}

func TestVersionsPrunedByRetentionAndBound(t *testing.T) {
	f := newFake()
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	l, stop := open(t, f, dir, clk, time.Hour)
	defer stop()
	c := l.Client(f)
	upsert(t, c, entity("a", 1))
	t1 := clk.add(time.Minute)
	upsert(t, c, entity("a", 2))
	clk.add(time.Hour - 30*time.Second)
	l.Prune()
	// The first version is folded into the base, the second is within
	// the hour: a just before t1 is still 1.
	if got := lookupAt(t, l, f, "a", t1.Add(-time.Second)); got != 1 {
		t.Errorf("a just before t1 = %d; want 1", got)
	}
	clk.add(2 * time.Hour)
	l.Prune()
	if l.Bytes() != 0 {
		t.Errorf("after the retention, the log holds %d bytes; want none", l.Bytes())
	}

	// The size bound: the oldest versions are folded and since moves up.
	l.SetMaxBytes(20000)
	big := strings.Repeat("x", 1000)
	for i := 0; i < 100; i++ {
		clk.add(time.Second)
		e := entity("big", int64(i))
		e.Properties["s"] = str(big)
		upsert(t, c, e)
	}
	if b := l.Bytes(); b > 20000 {
		t.Errorf("the log holds %d bytes; want at most its bound, 20000", b)
	}
	if s := l.Since("p", ""); !s.After(t1) {
		t.Errorf("since = %s after folding early; want it moved up", s)
	}
	l.Prune()
	if fs := l.FileSize(); fs > 2*20000+8<<20 {
		t.Errorf("the log file is %d bytes; want it compacted", fs)
	}
}

func TestVersionsUnderConcurrentCommits(t *testing.T) {
	f := newFake()
	clk := &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	l, stop := open(t, f, t.TempDir(), clk, time.Hour)
	defer stop()
	c := l.Client(f)
	start := clk.add(time.Second)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				clk.add(time.Millisecond)
				upsert(t, c, entity("k", int64(i)))
				_ = lookupAt(t, l, f, "k", start)
			}
		}()
	}
	wg.Wait()
	if got := lookupAt(t, l, f, "k", start); got != -1 {
		t.Errorf("k before every commit = %d; want none", got)
	}
}

func TestReadOnlyTransactions(t *testing.T) {
	l := New(func(string) time.Duration { return time.Hour }, nil)
	at := time.Now().Add(-time.Minute)
	tx := l.BeginReadOnly("p", "", at)
	got, ok, err := l.ReadTime(&pb.ReadOptions{ConsistencyType: &pb.ReadOptions_Transaction{Transaction: tx}})
	if err != nil || !ok || !got.Equal(at) {
		t.Fatalf("the transaction's read time = %s, %v, %v; want %s", got, ok, err, at)
	}
	if err := l.EndTransaction(tx, 1); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a write in the read-only transaction = %v; want INVALID_ARGUMENT", err)
	}
	if _, _, err := l.ReadTime(&pb.ReadOptions{ConsistencyType: &pb.ReadOptions_Transaction{Transaction: tx}}); err == nil {
		t.Error("an ended transaction still reads")
	}
}

func TestClaims(t *testing.T) {
	l := New(func(string) time.Duration { return time.Hour }, nil)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	for _, tc := range []struct {
		path, body string
		want       bool
	}{
		{"/v1/projects/p:commit", `{"mode":"NON_TRANSACTIONAL"}`, true},
		{"/v1/projects/p:lookup", `{"keys":[]}`, false},
		{"/v1/projects/p:lookup", `{"readOptions":{"readTime":"` + past + `"}}`, true},
		{"/v1/projects/p:runQuery", `{"readOptions":{"newTransaction":{"readOnly":{"readTime":"` + past + `"}}}}`, true},
		{"/v1/projects/p:beginTransaction", `{"transactionOptions":{"readOnly":{"readTime":"` + past + `"}}}`, true},
		{"/v1/projects/p:beginTransaction", `{}`, false},
		{"/v1/projects/p:allocateIds", `{}`, false},
	} {
		r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		if got := l.Claims(r); got != tc.want {
			t.Errorf("Claims(%s %s) = %v; want %v", tc.path, tc.body, got, tc.want)
		}
	}
}
