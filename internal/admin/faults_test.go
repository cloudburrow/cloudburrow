package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func adminServer(t *testing.T) (*API, *httptest.Server) {
	t.Helper()
	api := NewAPI(NewRecorder(100, nil))
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return api, srv
}

func addRule(t *testing.T, srv *httptest.Server, rule string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/admin/faults", "application/json", strings.NewReader(rule))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	_ = json.NewDecoder(resp.Body).Decode(&map[string]any{})
	return resp.StatusCode, b.String()
}

func grpcWith(t *testing.T, i grpc.UnaryServerInterceptor, register func(*grpc.Server)) []option.ClientOption {
	t.Helper()
	g := grpc.NewServer(grpc.ChainUnaryInterceptor(i))
	register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	return []option.ClientOption{option.WithEndpoint(ln.Addr().String()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// Two UNAVAILABLE faults, and the official client's own retry succeeds on
// the third attempt; both faults are events.
func TestFaultsLetTheSDKRetrySucceed(t *testing.T) {
	api, srv := adminServer(t)
	st := secrets.NewStore(store.NewMemory())
	sec, err := st.CreateSecret("demo-project", "k", nil, nil, "automatic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddVersion("demo-project", "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	opts := grpcWith(t, api.Faults().Interceptor("secretmanager"), func(g *grpc.Server) { secrets.NewGRPCServer(st).Register(g) })
	if code, _ := addRule(t, srv, `{"service":"secretmanager","method":"AccessSecretVersion","code":"UNAVAILABLE","count":2}`); code != http.StatusCreated {
		t.Fatalf("add rule = %d", code)
	}
	c, err := secretmanager.NewClient(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	resp, err := c.AccessSecretVersion(context.Background(), &secretmanagerpb.AccessSecretVersionRequest{Name: sec.Name + "/versions/latest"})
	if err != nil || string(resp.GetPayload().GetData()) != "v" {
		t.Fatalf("AccessSecretVersion after 2 faults = %v, %v; want the SDK's retry to succeed", resp, err)
	}
	if n := len(api.recorder.EventsWhere(Filter{Kind: "fault"}, 100)); n != 2 {
		t.Errorf("%d fault events, want 2", n)
	}
}

func TestLatencyFaultExceedsTheDeadline(t *testing.T) {
	api, srv := adminServer(t)
	st := tasks.NewStore(store.NewMemory())
	opts := grpcWith(t, api.Faults().Interceptor("tasks"), func(g *grpc.Server) { tasks.NewGRPCServer(st).Register(g) })
	if code, _ := addRule(t, srv, `{"service":"tasks","method":"GetQueue","latencyMs":500}`); code != http.StatusCreated {
		t.Fatalf("add rule = %d", code)
	}
	c, err := cloudtasks.NewClient(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = c.GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: "projects/demo-project/locations/us-central1/queues/q"})
	// The client reports its own deadline as a context error, or as the
	// status the server returned when it saw the deadline first.
	if !errors.Is(err, context.DeadlineExceeded) && status.Code(err) != codes.DeadlineExceeded {
		t.Errorf("GetQueue under a 500ms fault with a 200ms deadline = %v, want DeadlineExceeded", err)
	}
}

func TestSeededFaultsAreReproducible(t *testing.T) {
	seq := func() string {
		f := NewFaults(NewRecorder(100, nil))
		seed := int64(42)
		r := &FaultRule{Service: "tasks", Probability: 0.5, Seed: &seed}
		if err := r.validate(nil); err != nil {
			t.Fatal(err)
		}
		f.rules = []*FaultRule{r}
		var b strings.Builder
		for i := 0; i < 20; i++ {
			if f.decide("tasks", "/google.cloud.tasks.v2.CloudTasks/GetQueue", "") != nil {
				b.WriteByte('F')
			} else {
				b.WriteByte('.')
			}
		}
		return b.String()
	}
	a, b := seq(), seq()
	if a != b {
		t.Errorf("seed 42 gave %s then %s", a, b)
	}
	if !strings.Contains(a, "F") || !strings.Contains(a, ".") {
		t.Errorf("probability 0.5 over 20 calls gave %s", a)
	}
}

func TestFaultRulesRefusedClearedAndReset(t *testing.T) {
	api, srv := adminServer(t)
	if code, _ := addRule(t, srv, `{"service":"storage","code":"UNAVAILABLE"}`); code != http.StatusBadRequest {
		t.Errorf("a storage rule = %d, want 400", code)
	}
	if code, _ := addRule(t, srv, `{"service":"tasks","code":"NOT_A_CODE"}`); code != http.StatusBadRequest {
		t.Errorf("a bad code = %d, want 400", code)
	}
	for i := 0; i < 2; i++ {
		if code, _ := addRule(t, srv, `{"service":"tasks"}`); code != http.StatusCreated {
			t.Fatalf("add = %d", code)
		}
	}
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/admin/faults", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE = %v, %v", resp, err)
	}
	if n := len(api.faults.rules); n != 0 {
		t.Errorf("%d rules after DELETE", n)
	}
	addRule(t, srv, `{"service":"tasks"}`)
	if resp, err := http.Post(srv.URL+"/admin/reset", "", nil); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("reset = %v, %v", resp, err)
	}
	if n := len(api.faults.rules); n != 0 {
		t.Errorf("%d rules after /admin/reset", n)
	}
}

type namedResetter string

func (n namedResetter) Name() string                { return string(n) }
func (n namedResetter) Reset(context.Context) error { return nil }

// Cloud KMS is served in-process, so its calls can be faulted (#392), and a
// reset of kms clears its rules and only its rules.
func TestKMSFaultRulesAreAcceptedAndResetWithKMS(t *testing.T) {
	api, srv := adminServer(t)
	api.RegisterResetter(namedResetter("kms"))
	if code, body := addRule(t, srv, `{"service":"kms","method":"ListKeyRings","code":"UNAVAILABLE","count":1}`); code != http.StatusCreated {
		t.Fatalf("a kms rule = %d %s, want 201", code, body)
	}
	if code, _ := addRule(t, srv, `{"service":"tasks"}`); code != http.StatusCreated {
		t.Fatal("tasks rule")
	}
	resp, err := http.Post(srv.URL+"/admin/reset?service=kms", "", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("reset?service=kms = %v, %v", resp, err)
	}
	if n := len(api.faults.rules); n != 1 || api.faults.rules[0].Service != "tasks" {
		t.Errorf("after reset?service=kms the rules are %+v; want only the tasks rule", api.faults.rules)
	}
}

// A service interposed at run time (Cloud Storage on the builtin server,
// #513) accepts rules, and DecideHTTP applies them as HTTP statuses and
// records the fault.
func TestFaultsInterposedStorage(t *testing.T) {
	api, srv := adminServer(t)
	if code, _ := addRule(t, srv, `{"service":"storage","httpStatus":503}`); code != http.StatusBadRequest {
		t.Fatalf("a storage rule before storage is interposed = %d, want 400", code)
	}
	api.Faults().Interpose("storage")
	if code, body := addRule(t, srv, `{"service":"storage","method":"storage.objects.*","httpStatus":503,"count":1}`); code != http.StatusCreated {
		t.Fatalf("a storage rule once interposed = %d %s", code, body)
	}
	if st, _, ok := api.Faults().DecideHTTP("storage", "storage.buckets.get", "b/x"); ok {
		t.Errorf("a rule for storage.objects.* faulted storage.buckets.get (%d)", st)
	}
	if st, _, ok := api.Faults().DecideHTTP("storage", "storage.objects.get", "b/x/o/y"); !ok || st != 503 {
		t.Errorf("the rule = %d, %v; want 503", st, ok)
	}
	if _, _, ok := api.Faults().DecideHTTP("storage", "storage.objects.get", "b/x/o/y"); ok {
		t.Error("a count-1 rule faulted twice")
	}
	addRule(t, srv, `{"service":"storage","code":"UNAVAILABLE","count":1}`)
	if st, _, _ := api.Faults().DecideHTTP("storage", "storage.objects.list", "b/x"); st != 503 {
		t.Errorf("a gRPC code UNAVAILABLE = HTTP %d, want 503", st)
	}
}

// A storage rule is refused with storage's own reason unless storage was
// interposed (#519): its server runs in the cluster, not behind a
// port-forward to an upstream emulator.
func TestFaultRuleForStorageNamesWhyItIsRefused(t *testing.T) {
	r := FaultRule{Service: "storage", Method: "storage.buckets.get", HTTPStatus: 503}
	err := r.validate(nil)
	if err == nil || !strings.Contains(err.Error(), "runs in the cluster") || strings.Contains(err.Error(), "upstream emulator") {
		t.Errorf("a storage rule = %v, want refused because its server runs in the cluster", err)
	}
	r = FaultRule{Service: "storage", Method: "storage.buckets.get", HTTPStatus: 503}
	if err := r.validate(map[string]bool{"storage": true}); err != nil {
		t.Errorf("a storage rule once storage is interposed = %v", err)
	}
}
