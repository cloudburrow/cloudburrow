package callfront

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/frontready"
)

func serve(t *testing.T, upstream string) (string, *calllog.Ring) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ring := &calllog.Ring{}
	srv := Server(Handler(upstream, ring, t.Logf), t.Logf)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return l.Addr().String(), ring
}

func calls(t *testing.T, addr string) []calllog.Call {
	t.Helper()
	resp, err := http.Get("http://" + addr + calllog.Path + "?after=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page struct {
		Calls []calllog.Call `json:"calls"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	return page.Calls
}

// A REST call passes through unchanged, body and status, and is logged
// with its method, path, status and resource, never its body or query.
func TestRESTPassesThroughAndIsLogged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(append([]byte("echo:"), b...))
	}))
	defer up.Close()
	addr, _ := serve(t, strings.TrimPrefix(up.URL, "http://"))
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/projects/p/databases/(default)/documents/c?key=secret", strings.NewReader("payload"))
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(b) != "echo:payload" {
		t.Fatalf("answer = %d %q, want 201 echo:payload", resp.StatusCode, b)
	}
	got := calls(t, addr)
	if len(got) != 1 {
		t.Fatalf("calls = %+v, want 1", got)
	}
	c := got[0]
	if c.Method != "POST /v1/projects/p/databases/(default)/documents/c" || c.Transport != "http" || c.Code != 201 ||
		c.Resource != "projects/p/databases/(default)/documents/c" || !strings.HasPrefix(c.TraceParent, "00-4bf9") {
		t.Errorf("call = %+v", c)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "payload") || strings.Contains(string(raw), "secret") {
		t.Errorf("the call log carries a body or a query: %s", raw)
	}
}

// The readiness path is the front's: 200 while the emulator accepts
// connections, 503 once it does not.
func TestReadinessFollowsTheEmulator(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	addr, _ := serve(t, strings.TrimPrefix(up.URL, "http://"))
	get := func() int {
		resp, err := http.Get("http://" + addr + frontready.Path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(); got != http.StatusOK {
		t.Errorf("ready with the emulator up = %d, want 200", got)
	}
	up.Close()
	if got := get(); got != http.StatusServiceUnavailable {
		t.Errorf("ready with the emulator down = %d, want 503", got)
	}
}

// gRPC passes through over h2c with its codes, and each call is logged;
// the added latency of the front is measured against the emulator direct.
func TestGRPCPassesThroughAndAddedLatency(t *testing.T) {
	fake := pstest.NewServer()
	defer fake.Close()
	addr, _ := serve(t, fake.Addr)
	dial := func(a string) pubsubpb.PublisherClient {
		conn, err := grpc.NewClient(a, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return pubsubpb.NewPublisherClient(conn)
	}
	front, direct := dial(addr), dial(fake.Addr)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "x-goog-request-params", "name=projects/p/topics/t")
	if _, err := front.CreateTopic(ctx, &pubsubpb.Topic{Name: "projects/p/topics/t"}); err != nil {
		t.Fatal(err)
	}
	_, err := front.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/p/topics/absent"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetTopic of an absent topic = %v, want NotFound", err)
	}
	got := calls(t, addr)
	if len(got) != 2 || got[0].Method != "/google.pubsub.v1.Publisher/CreateTopic" || got[0].Code != 0 ||
		got[0].Transport != "grpc" || got[0].Resource != "projects/p/topics/t" || got[1].Code != int(codes.NotFound) {
		t.Fatalf("calls = %+v", got)
	}

	const n = 300
	median := func(c pubsubpb.PublisherClient) time.Duration {
		d := make([]time.Duration, n)
		for i := range d {
			start := time.Now()
			if _, err := c.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/p/topics/t"}); err != nil {
				t.Fatal(err)
			}
			d[i] = time.Since(start)
		}
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[n/2]
	}
	m1, m0 := median(front), median(direct)
	t.Logf("unary GetTopic, median of %d: %v through the front, %v direct, %v added", n, m1, m0, m1-m0)
}
