package lifecycle

import (
	"context"
	"net/http"
	"testing"
)

// The control port is loopback-only, which a rebound browser page still
// reaches (#676): it refuses a foreign Host before any route, token or not.
func TestControlServerRefusesForeignHosts(t *testing.T) {
	t.Parallel()
	c, cs := startCoordinator(t)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]int{"attacker.example:9000": http.StatusMisdirectedRequest, "127.0.0.1:9000": http.StatusOK} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+cs.Addr()+"/readyz", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("/readyz with Host %s = %d, want %d", host, resp.StatusCode, want)
		}
	}
}
