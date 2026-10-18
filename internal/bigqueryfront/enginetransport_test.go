package bigqueryfront

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestEngineIdleTimeoutMatchesPatch keeps engineIdleTimeout in step with the
// IdleTimeout the engine's patches give its server, and the front's below
// it (#1626).
func TestEngineIdleTimeoutMatchesPatch(t *testing.T) {
	patches, err := filepath.Glob("../../third_party/bigquery-emulator/patches/bigquery-emulator/*.patch")
	if err != nil || len(patches) == 0 {
		t.Fatalf("no engine patches found: %v", err)
	}
	line := regexp.MustCompile(`(?m)^\+\s*IdleTimeout:\s*(\d+)\s*\*\s*time\.Second,`)
	var found []time.Duration
	for _, p := range patches {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range line.FindAllSubmatch(b, -1) {
			n, _ := strconv.Atoi(string(m[1]))
			found = append(found, time.Duration(n)*time.Second)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the engine's patches set its server's IdleTimeout %d times (%v), want once", len(found), found)
	}
	if found[0] != engineIdleTimeout {
		t.Errorf("the engine's IdleTimeout is %v, engineIdleTimeout %v", found[0], engineIdleTimeout)
	}
	if idleConnTimeout >= engineIdleTimeout {
		t.Errorf("idleConnTimeout %v is not below the engine's %v", idleConnTimeout, engineIdleTimeout)
	}
}

// countingEngine is an engine whose server closes a connection idle for
// idle, and which counts the POSTs it runs.
func countingEngine(t *testing.T, idle time.Duration) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var runs atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost {
			if string(body) != `{"jobReference":{"jobId":"j1"}}` {
				http.Error(w, "body "+string(body), http.StatusBadRequest)
				return
			}
			runs.Add(1)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	srv.Config.IdleTimeout = idle
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &runs
}

func postInsert(h http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/bigquery/v2/projects/p/jobs", strings.NewReader(`{"jobReference":{"jobId":"j1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// lagConn is a connection that learns the engine has closed it late: the
// moment between the engine closing an idle connection and the front's
// transport seeing it, in which the front can still send a request on it,
// held open long enough that a test can send one.
type lagConn struct {
	net.Conn
	lag time.Duration
}

func (c lagConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		time.Sleep(c.lag)
	}
	return n, err
}

// TestProxyStaleConnection reproduces #1626: a POST sent on a connection
// the engine has just closed for idleness is answered 502 by a front that
// keeps idle connections longer than the engine (90 s against 15 s, as it
// did), and the client's retry would run it again. A front that closes
// them first never sends on one the engine has closed.
func TestProxyStaleConnection(t *testing.T) {
	const serverIdle = 150 * time.Millisecond
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return lagConn{Conn: c, lag: time.Second}, nil
	}
	cases := []struct {
		name      string
		transport http.RoundTripper
		want      int
	}{
		{"idle 90 s, as before", &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second, DialContext: dial}, http.StatusBadGateway},
		// The front's ratio to the engine's, at the test's scale.
		{"idle below the engine's", engineTransport(serverIdle*idleConnTimeout/engineIdleTimeout, dial), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, runs := countingEngine(t, serverIdle)
			h := proxyThrough(strings.TrimPrefix(engine.URL, "http://"), t.Logf, tc.transport)
			if rec := postInsert(h); rec.Code != http.StatusOK {
				t.Fatalf("first insert: %d %s", rec.Code, rec.Body)
			}
			// The engine closes the connection; the transport has not seen
			// it yet.
			time.Sleep(2 * serverIdle)
			rec := postInsert(h)
			if rec.Code != tc.want {
				t.Fatalf("second insert: %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
			if tc.want == http.StatusOK && runs.Load() != 2 {
				t.Errorf("the engine ran %d inserts, want 2", runs.Load())
			}
		})
	}
}

// resetConn fails every write once broken, writing nothing, as a
// connection the engine has reset does.
type resetConn struct {
	net.Conn
	broken *atomic.Bool
}

func (c resetConn) Write(p []byte) (int, error) {
	if c.broken.Load() {
		return 0, &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}
	}
	return c.Conn.Write(p)
}

// TestProxyRetriesUnsentPOST: a POST whose connection fails before any of
// it was written is sent again on a fresh connection, once, since the
// engine cannot have seen it. Without a GetBody, Go's transport answered
// such a POST with the error, and the front 502 (#1626).
func TestProxyRetriesUnsentPOST(t *testing.T) {
	cases := []struct {
		name string
		wrap func(http.RoundTripper) http.RoundTripper
		want int
	}{
		{"no GetBody, as before", func(rt http.RoundTripper) http.RoundTripper { return rt.(replayable).next }, http.StatusBadGateway},
		{"replayable", func(rt http.RoundTripper) http.RoundTripper { return rt }, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, runs := countingEngine(t, time.Minute)
			var mu sync.Mutex
			var flags []*atomic.Bool
			dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				f := new(atomic.Bool)
				mu.Lock()
				flags = append(flags, f)
				mu.Unlock()
				return resetConn{Conn: c, broken: f}, nil
			}
			h := proxyThrough(strings.TrimPrefix(engine.URL, "http://"), t.Logf, tc.wrap(engineTransport(time.Minute, dial)))
			if rec := postInsert(h); rec.Code != http.StatusOK {
				t.Fatalf("first insert: %d %s", rec.Code, rec.Body)
			}
			// Every connection dialed so far is reset; a new one is not.
			mu.Lock()
			for _, f := range flags {
				f.Store(true)
			}
			mu.Unlock()
			rec := postInsert(h)
			if rec.Code != tc.want {
				t.Fatalf("second insert: %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
			if tc.want == http.StatusOK && runs.Load() != 2 {
				t.Errorf("the engine ran %d inserts, want 2", runs.Load())
			}
		})
	}
}

// TestReplayableLeavesLargeBodies: a body over maxReplayBody, or of no
// known length, is streamed as it was, with no GetBody.
func TestReplayableLeavesLargeBodies(t *testing.T) {
	var got []bool
	rt := replayable{next: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = append(got, r.GetBody != nil)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
	})}
	for _, n := range []int64{10, maxReplayBody + 1, -1} {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(strings.Repeat("a", 10)))
		req.ContentLength = n
		if n == maxReplayBody+1 {
			req.Body = io.NopCloser(strings.NewReader(strings.Repeat("a", int(n))))
		}
		if _, err := rt.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
	}
	if want := []bool{true, false, false}; !equalBools(got, want) {
		t.Errorf("GetBody set: %v, want %v", got, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
