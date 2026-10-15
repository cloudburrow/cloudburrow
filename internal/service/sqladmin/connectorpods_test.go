package sqladmin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A connectSettings request that came through the cluster name pods use is
// told the address pods reach the listener at (#1346); one from this
// machine is still told loopback.
func TestConnectSettingsForPods(t *testing.T) {
	srv := NewServer("127.0.0.1:0", echoEngine{newFake(), echoServer(t)})
	if err := srv.EnableConnector("127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop(ctx)
	if srv.ConnectorAddr() == "" {
		t.Fatal("ConnectorAddr is empty with the listener bound")
	}
	srv.SetPodConnectorHost(func() string { return "192.168.65.254" })
	get := func(host string) string {
		req, _ := http.NewRequest(http.MethodGet, "/v1/projects/p/instances/pg/connectSettings", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if b := get("cloudburrow-host.cloudburrow.svc.cluster.local:9020"); !strings.Contains(b, `"192.168.65.254"`) {
		t.Errorf("from a pod: %s", b)
	}
	if b := get("127.0.0.1:9020"); !strings.Contains(b, `"127.0.0.1"`) {
		t.Errorf("from the host: %s", b)
	}
}
