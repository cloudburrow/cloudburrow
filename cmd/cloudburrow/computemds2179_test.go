package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestComputePortSortsHTTPSMetadataConnections2179: the Compute port as
// `up` binds it sorts a Shielded VM guest's port 443 connections (#2179),
// which begin with the relay's line naming the instance, from the API's:
// a hello naming no Shielded VM is closed unanswered, never given to the
// API's HTTP server (which answered it 400, so curl in the guest saw
// "wrong version number"), and the API still answers HTTP.
func TestComputePortSortsHTTPSMetadataConnections2179(t *testing.T) {
	fx := computeConsoleFixture(t)
	addr := fx.svc.Addr()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, "\x00CBMDS no-such-instance\n\x16\x03\x01\x00\x00"); err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(bufio.NewReader(c)); len(b) != 0 {
		t.Fatalf("a hello naming no Shielded VM was answered: %q", b)
	}
	resp, err := http.Get("http://" + addr + "/compute/v1/projects/p/zones/us-central1-a/instances")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "compute#instanceList") {
		t.Fatalf("the API over HTTP: %d %s", resp.StatusCode, b)
	}
}
