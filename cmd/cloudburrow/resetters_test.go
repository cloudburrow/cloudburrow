package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// forwarderAt returns a Forwarder whose host address is addr. It is never
// started: the resetters only read the address.
func forwarderAt(t *testing.T, addr string) *netfwd.Forwarder {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return netfwd.New(netfwd.Target{Name: "test", HostPort: p}, "", host)
}

func TestASecretManagerProjectResetLeavesOtherProjectsAlone(t *testing.T) {
	st := secrets.NewStore(store.NewMemory())
	for _, p := range []string{"p1", "p2"} {
		for _, id := range []string{"a", "b"} {
			if _, err := st.CreateSecret(p, id, nil, nil, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	r := &secretsResetter{svc: &secretsService{store: st}}

	if err := r.ResetProject(context.Background(), "p1"); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.ListSecrets("p1"); len(left) != 0 {
		t.Errorf("p1 kept %d secrets after its reset", len(left))
	}
	if left, _ := st.ListSecrets("p2"); len(left) != 2 {
		t.Errorf("p2 has %d secrets after p1's reset, want 2 untouched", len(left))
	}

	if err := r.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.ListSecrets("p2"); len(left) != 0 {
		t.Errorf("p2 kept %d secrets after a full reset", len(left))
	}
}

func TestASecretManagerResetBeforeStartIsAnError(t *testing.T) {
	r := &secretsResetter{svc: &secretsService{}}
	if err := r.Reset(context.Background()); err == nil {
		t.Error("a reset of a service that has not started reported success")
	}
}

// fakeGCS is the part of the JSON API the storage resetter uses. It pages
// every listing one item at a time, so a resetter that ignored nextPageToken
// would leave all but the first bucket and object behind.
type fakeGCS struct {
	mu      sync.Mutex
	buckets map[string]map[string]bool
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Split the escaped path, as a real server does: an object name may hold
	// a slash, which the resetter must send as %2F.
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/storage/v1/b"), "/")
	for i := range parts {
		parts[i], _ = url.PathUnescape(parts[i])
	}
	// parts: [""] | ["", bucket] | ["", bucket, "o"] | ["", bucket, "o", object]
	switch {
	case r.Method == http.MethodGet && len(parts) == 1:
		var names []string
		for b := range f.buckets {
			names = append(names, b)
		}
		page(w, r, names)
	case r.Method == http.MethodGet && len(parts) == 3:
		var names []string
		for o := range f.buckets[parts[1]] {
			names = append(names, o)
		}
		page(w, r, names)
	case r.Method == http.MethodDelete && len(parts) == 4:
		if !f.buckets[parts[1]][parts[3]] {
			http.NotFound(w, r)
			return
		}
		delete(f.buckets[parts[1]], parts[3])
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && len(parts) == 2:
		if len(f.buckets[parts[1]]) > 0 {
			http.Error(w, "bucket not empty", http.StatusConflict)
			return
		}
		delete(f.buckets, parts[1])
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, r.Method+" "+r.URL.Path, http.StatusBadRequest)
	}
}

func page(w http.ResponseWriter, r *http.Request, names []string) {
	sort.Strings(names)
	start := 0
	if tok := r.URL.Query().Get("pageToken"); tok != "" {
		start, _ = strconv.Atoi(tok)
	}
	body := map[string]any{"items": []map[string]string{}}
	if start < len(names) {
		body["items"] = []map[string]string{{"name": names[start]}}
		if start+1 < len(names) {
			body["nextPageToken"] = strconv.Itoa(start + 1)
		}
	}
	_ = json.NewEncoder(w).Encode(body)
}

func TestAStorageResetEmptiesEveryBucketAcrossPages(t *testing.T) {
	gcs := &fakeGCS{buckets: map[string]map[string]bool{
		"one":   {"a": true, "b": true, "c/d": true},
		"two":   {"x": true},
		"three": {},
	}}
	srv := httptest.NewServer(gcs)
	defer srv.Close()

	r := &storageResetter{tunnel: forwarderAt(t, srv.Listener.Addr().String())}
	if err := r.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gcs.buckets) != 0 {
		t.Errorf("buckets left after a reset: %v", gcs.buckets)
	}
}

func TestAStorageResetReportsABackendFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	r := &storageResetter{tunnel: forwarderAt(t, srv.Listener.Addr().String())}
	if err := r.Reset(context.Background()); err == nil {
		t.Error("a reset that could not list buckets reported success")
	}
}

func TestPubSubResetsAreScopedAndSpareTheEventTopic(t *testing.T) {
	fake := pstest.NewServer()
	defer func() { _ = fake.Close() }()
	ctx := context.Background()

	client := func(project string) *pubsub.Client {
		c, err := pubsub.NewClient(ctx, project,
			option.WithEndpoint(fake.Addr),
			option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	seed := func(project string) {
		c := client(project)
		topic := "projects/" + project + "/topics/t"
		if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
			Name: "projects/" + project + "/subscriptions/s", Topic: topic}); err != nil {
			t.Fatal(err)
		}
	}
	topics := func(project string) int {
		it := client(project).TopicAdminClient.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: "projects/" + project})
		n := 0
		for {
			if _, err := it.Next(); err == iterator.Done {
				return n
			} else if err != nil {
				t.Fatal(err)
			}
			n++
		}
	}
	event := components.EventProject()
	for _, p := range []string{"p1", "p2", event} {
		seed(p)
	}

	r := &pubsubResetter{
		tunnel:   forwarderAt(t, fake.Addr),
		projects: func() []string { return []string{"p1", "p2", event} },
	}
	if err := r.ResetProject(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if n := topics("p1"); n != 0 {
		t.Errorf("p1 has %d topics after its reset", n)
	}
	if n := topics("p2"); n != 1 {
		t.Errorf("p2 has %d topics after p1's reset, want 1", n)
	}

	if err := r.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if n := topics("p2"); n != 0 {
		t.Errorf("p2 has %d topics after a full reset", n)
	}
	if n := topics(event); n != 1 {
		t.Errorf("the event topic was deleted by a reset; storage notifications would stop")
	}
}

func TestKnownProjectsIncludesTheDefaultOnce(t *testing.T) {
	got := knownProjects("dev", func() []string { return []string{"b", "dev", "", "a"} })
	if strings.Join(got, ",") != "a,b,dev" {
		t.Errorf("knownProjects = %v, want [a b dev]", got)
	}
}

func TestACloudTasksProjectResetLeavesOtherProjectsAlone(t *testing.T) {
	st := tasks.NewStore(store.NewMemory())
	// "proj-onex" shares proj-one's prefix as a string, so it proves the match stops at
	// the path separator.
	for _, p := range []string{"proj-one", "proj-onex", "proj-two"} {
		if _, err := st.CreateQueue(tasks.Queue{Name: "projects/" + p + "/locations/us-central1/queues/q"}); err != nil {
			t.Fatal(err)
		}
	}
	r := &tasksResetter{svc: &tasksService{store: st}}
	if err := r.ResetProject(context.Background(), "proj-one"); err != nil {
		t.Fatal(err)
	}
	all, err := st.AllQueues()
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, q := range all {
		left = append(left, strings.Split(q.Name, "/")[1])
	}
	sort.Strings(left)
	if strings.Join(left, ",") != "proj-onex,proj-two" {
		t.Errorf("queues left after resetting proj-one: %v, want [proj-onex proj-two]", left)
	}
}

// TestAKMSProjectResetLeavesOtherProjectsAlone: with kms registered,
// POST /admin/reset?project=p is accepted and removes p's rings, keys and
// versions while q's ring stays readable through the official client (#387).
func TestAKMSProjectResetLeavesOtherProjectsAlone(t *testing.T) {
	svc := startedKMS(t, kmsConfig(t, "kms"), nil)
	ctx := context.Background()
	c, err := kmsapi.NewKeyManagementClient(ctx, option.WithEndpoint(svc.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rings := map[string]string{}
	for _, p := range []string{"project-p", "project-q"} {
		ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + p + "/locations/global", KeyRingId: "r"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}}); err != nil {
			t.Fatal(err)
		}
		rings[p] = ring.GetName()
	}
	a := admin.NewAPI(admin.NewRecorder(10, nil))
	a.RegisterResetter(&kmsResetter{svc: svc})
	mux := http.NewServeMux()
	a.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/admin/reset?project=project-p", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("project reset = %d", resp.StatusCode)
	}
	left, err := svc.db.List("kms/")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range left {
		if strings.Contains(k, "projects~project-p~") {
			t.Errorf("%s survived its project's reset", k)
		}
	}
	if _, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: rings["project-p"]}); status.Code(err) != codes.NotFound {
		t.Errorf("GetKeyRing after reset = %v, want NotFound", err)
	}
	if _, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: rings["project-q"] + "/cryptoKeys/k"}); err != nil {
		t.Errorf("project-q's key after project-p's reset: %v", err)
	}
}

// failingDeleteStore fails every Delete, as an unreachable API server would.
type failingDeleteStore struct{ store.Store }

func (failingDeleteStore) Delete(string) error { return errors.New("connection refused") }

func TestAKMSProjectResetReportsAStoreFailure(t *testing.T) {
	mem := store.NewMemory()
	if err := mem.Put("kms/ring/projects~project-p~locations~global~keyRings~r", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	r := &kmsResetter{svc: &kmsService{db: failingDeleteStore{mem}}}
	if err := r.ResetProject(context.Background(), "project-p"); err == nil {
		t.Fatal("a reset whose delete failed reported success")
	}
	if err := (&kmsResetter{svc: &kmsService{db: mem}}).ResetProject(context.Background(), "a~b"); err == nil {
		t.Error("a project name with a separator was accepted")
	}
}
