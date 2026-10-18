package debugpprof

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestStartIsOffByDefault(t *testing.T) {
	addr, err := Start(env(nil), io.Discard)
	if addr != "" || err != nil {
		t.Fatalf("Start with no %s = %q, %v; want nothing listening", EnvVar, addr, err)
	}
}

func TestStartRefusesANonLoopbackAddress(t *testing.T) {
	for _, a := range []string{"0.0.0.0:0", ":0", "localhost:0", "10.0.0.1:6060", "nonsense"} {
		if addr, err := Start(env(map[string]string{EnvVar: a}), io.Discard); err == nil {
			t.Errorf("Start(%q) = %q; want an error", a, addr)
		}
	}
}

func TestStartServesGoroutines(t *testing.T) {
	addr, err := Start(env(map[string]string{EnvVar: "127.0.0.1:0"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "goroutine profile:") {
		t.Fatalf("goroutine profile: %d %.200s", resp.StatusCode, b)
	}
}
