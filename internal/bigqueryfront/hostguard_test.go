package bigqueryfront

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunGuardsHostAndOrigin (#1587): the front serves BigQuery's REST port
// to every client of the Service, the host's tunnel and the pods alike, so
// it checks Host and Origin itself. A page on another site, or one a
// rebound name serves, is refused; an SDK, a pod that names the Service
// and a kubelet probe by the pod's address are served.
func TestRunGuardsHostAndOrigin(t *testing.T) {
	var reached atomic.Int32
	emulator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"kind":"bigquery#datasetList"}`)
	}))
	defer emulator.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{"--listen", addr, "--upstream", strings.TrimPrefix(emulator.URL, "http://")}, &out, &errOut)
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	waitListening(t, addr)

	_, port, _ := net.SplitHostPort(addr)
	for _, c := range []struct {
		host, origin string
		want         int
		why          string
	}{
		{"127.0.0.1:" + port, "", http.StatusOK, "an SDK or bq through the host's tunnel"},
		{"bigquery.cloudburrow.svc.cluster.local:" + port, "", http.StatusOK, "a pod that dials the Service"},
		{"bigquery.cloudburrow.svc:" + port, "", http.StatusOK, "a pod that dials the Service's shorter name"},
		{"bigquery:" + port, "", http.StatusOK, "a pod in the namespace that dials the bare name"},
		{"10.244.0.7:" + port, "", http.StatusOK, "a kubelet probe or a pod by the pod's address"},
		{"cloudburrow-host.cloudburrow.svc.cluster.local:" + port, "", http.StatusOK, "relayed through cloudburrow-host"},
		{"127.0.0.1:" + port, "http://localhost:3000", http.StatusOK, "a local web app"},

		{"attacker.example:" + port, "", http.StatusMisdirectedRequest, "a rebound name"},
		{"bigquery.attacker.example:" + port, "", http.StatusMisdirectedRequest, "the Service's name under another domain"},
		{"spanner.cloudburrow.svc.cluster.local:" + port, "", http.StatusMisdirectedRequest, "another Service's name"},
		{"127.0.0.1:" + port, "https://evil.example", http.StatusForbidden, "a page on another site"},
		{"bigquery.cloudburrow.svc.cluster.local:" + port, "https://evil.example", http.StatusForbidden, "a page on another site, by the Service's name"},
	} {
		reached.Store(0)
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/bigquery/v2/projects/p/datasets", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.why, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s (Host %s, Origin %q): status %d, want %d: %s", c.why, c.host, c.origin, resp.StatusCode, c.want, body)
		}
		if c.want != http.StatusOK && reached.Load() != 0 {
			t.Errorf("%s: a refused request reached the emulator", c.why)
		}
	}
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the front did not listen on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
