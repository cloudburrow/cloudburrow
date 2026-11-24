package compute

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestTunnelDestinations (#2160): an instance's interface and a host in
// its network resolve to the instance, the firewall rules must let IAP's
// range in to the port, and the connection reaches the instance's port.
func TestTunnelDestinations(t *testing.T) {
	s, _, _ := lbFixture(t)
	v, err := s.TunnelInstance("p", "us-central1-a", "web-1", "nic0")
	if err != nil || v.IP() == "" {
		t.Fatalf("TunnelInstance: %v %q", err, v.IP())
	}
	if _, err := s.TunnelInstance("p", "us-central1-a", "gone", "nic0"); !errors.Is(err, ErrTunnelNoInstance) {
		t.Fatalf("unknown instance: %v", err)
	}
	if _, err := s.TunnelInstance("p", "us-central1-a", "web-1", "nic3"); err == nil {
		t.Fatal("an interface the instance lacks")
	}
	h, err := s.TunnelHost("p", "us-central1", "default", v.IP())
	if err != nil || h.IP() != v.IP() {
		t.Fatalf("TunnelHost by address: %v", err)
	}
	if h, err := s.TunnelHost("p", "us-central1", "default", "web-1.us-central1-a.c.p.internal"); err != nil || h.IP() != v.IP() {
		t.Fatalf("TunnelHost by name: %v", err)
	}
	if _, err := s.TunnelHost("p", "europe-west1", "default", v.IP()); !errors.Is(err, ErrTunnelNoInstance) {
		t.Fatalf("another region: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.TunnelDial(ctx, v, 80); err == nil {
		t.Fatal("dialled without a firewall rule for 35.235.240.0/20")
	}
	lbCall(t, s, "POST", "global/firewalls", obj{"name": "allow-iap", "network": "global/networks/default",
		"sourceRanges": []any{"35.235.240.0/20"}, "allowed": []any{obj{"IPProtocol": "tcp", "ports": []any{"80"}}}})
	c, err := s.TunnelDial(ctx, v, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "GET /x HTTP/1.0\r\n\r\n")
	b, _ := io.ReadAll(c)
	if want := "web-1 "; len(b) == 0 || !strings.Contains(string(b), want) {
		t.Fatalf("reply %q", b)
	}
}
