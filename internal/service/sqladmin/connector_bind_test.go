package sqladmin

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestConnectorBindFailureNamesTheHolder (#1364): a second instance whose
// connector address is taken still serves the Admin API, and connect.get
// says which CloudBurrow instance holds the address and which setting moves
// it. The holder's record goes when it stops.
func TestConnectorBindFailureNamesTheHolder(t *testing.T) {
	ctx := context.Background()
	first := NewServer("127.0.0.1:0", newFake())
	if err := first.EnableConnector("127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	first.SetConnectorOwner("first")
	if err := first.Start(ctx); err != nil {
		t.Fatal(err)
	}
	held := first.conn.ln.Addr().String()
	record := holderPath(held)
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("the listener recorded no holder: %v", err)
	}

	second := NewServer("127.0.0.1:0", newFake())
	if err := second.EnableConnector(held, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	second.SetConnectorOwner("second")
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer second.Stop(ctx)
	req, _ := http.NewRequest(http.MethodGet, "/v1/projects/p/instances/pg/connectSettings", nil)
	rec := httptest.NewRecorder()
	second.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("connectSettings with the address taken: %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{held, `CloudBurrow instance \"first\"`, "--cloudsql-connector", "127.0.0.2:3307"} {
		if !strings.Contains(body, want) {
			t.Errorf("the error does not say %s: %s", want, body)
		}
	}

	if err := first.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Errorf("the holder record outlived its listener: %v", err)
	}
	// A taken address with no record says it is not a CloudBurrow instance.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if msg := bindError(ln.Addr().String(), os.ErrExist); !strings.Contains(msg, "no running CloudBurrow instance recorded it") {
		t.Errorf("bindError without a record: %s", msg)
	}
}

// TestConnectSettingsReportsTheListenerHost (#1364): the address connect.get
// hands the connectors is the listener's host, so an instance on another
// loopback address is dialed there, at 3307.
func TestConnectSettingsReportsTheListenerHost(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1:3307": "127.0.0.1",
		"127.0.0.2:3307": "127.0.0.2",
		"0.0.0.0:3307":   "127.0.0.1",
	} {
		srv := NewServer("", newFake())
		if err := srv.EnableConnector(addr, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodGet, "/v1/projects/p/instances/pg/connectSettings", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		var cs struct {
			IPAddresses []struct{ IPAddress string } `json:"ipAddresses"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &cs); err != nil || len(cs.IPAddresses) != 1 {
			t.Fatalf("%s: %d %s", addr, rec.Code, rec.Body)
		}
		if got := cs.IPAddresses[0].IPAddress; got != want {
			t.Errorf("listener %s: connectSettings reports %s, want %s", addr, got, want)
		}
	}
}
