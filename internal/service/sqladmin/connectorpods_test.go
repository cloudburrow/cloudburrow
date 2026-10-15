package sqladmin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A connectSettings request that came through the cluster name pods use is
// told the address pods dial (#1346), and served by the pods' own listener
// even when the host's 3307 could not be bound; one from this machine is
// still told why it cannot connect.
func TestConnectSettingsForPods(t *testing.T) {
	srv := NewServer("127.0.0.1:0", echoEngine{newFake(), echoServer(t)})
	if err := srv.EnableConnector("127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	srv.ServeConnectorToPods("127.0.0.1:0", func() string { return "10.96.0.20" })
	// Another instance holds the host's 3307: pods are still served.
	srv.conn.addr = "256.0.0.1:3307"
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop(ctx)
	if srv.ConnectorAddr() == "" {
		t.Fatal("ConnectorAddr is empty with the pod listener bound")
	}
	get := func(host string) string {
		req, _ := http.NewRequest(http.MethodGet, "/v1/projects/p/instances/pg/connectSettings", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if b := get("cloudburrow-host.cloudburrow.svc.cluster.local:9020"); !strings.Contains(b, `"10.96.0.20"`) {
		t.Errorf("from a pod: %s", b)
	}
	if b := get("127.0.0.1:9020"); !strings.Contains(b, "could not bind") {
		t.Errorf("from the host, with 3307 unbound: %s", b)
	}
}
