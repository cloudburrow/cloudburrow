package redisadmin

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"github.com/cloudburrow/cloudburrow/internal/redistlsfront"
)

// TestInTransitEncryption (#1413): an instance created with
// SERVER_AUTHENTICATION reports it and lists its CA in server_ca_certs,
// valid ten years; its server is started with the CA, and the server
// certificate ServerTLS issues verifies against it for the host's
// loopback and the names given, valid 180 days.
func TestInTransitEncryption(t *testing.T) {
	s, p, _ := newCreating(t, nil)
	ctx := context.Background()
	op, err := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "tls",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, TransitEncryptionMode: redispb.Instance_SERVER_AUTHENTICATION}})
	if err != nil {
		t.Fatal(err)
	}
	if op = wait(t, s, op); op.GetError() != nil {
		t.Fatalf("create: %v", op.GetError())
	}
	inst, err := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: parent + "/instances/tls"})
	if err != nil {
		t.Fatal(err)
	}
	if inst.GetTransitEncryptionMode() != redispb.Instance_SERVER_AUTHENTICATION || len(inst.GetServerCaCerts()) != 1 {
		t.Fatalf("transit_encryption_mode %v, server_ca_certs %v", inst.GetTransitEncryptionMode(), inst.GetServerCaCerts())
	}
	ca := inst.GetServerCaCerts()[0]
	if d := ca.GetExpireTime().AsTime().Sub(ca.GetCreateTime().AsTime()); d < 9*365*24*time.Hour || ca.GetSha1Fingerprint() == "" || ca.GetSerialNumber() == "" {
		t.Errorf("the CA: %v (valid %v)", ca, d)
	}
	p.mu.Lock()
	spec := p.running[ServerName(parent+"/instances/tls")]
	p.mu.Unlock()
	if spec.TransitCA != ca.GetCert() || spec.TransitCAKey == "" {
		t.Fatalf("the server's spec has CA %q", spec.TransitCA)
	}
	files, err := ServerTLS(spec, time.Now(), "redis-tls.cloudburrow.svc.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode([]byte(files.Cert))
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(ca.GetCert()))
	for _, name := range []string{"127.0.0.1", "redis-tls.cloudburrow.svc.cluster.local"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
			t.Errorf("the server certificate for %s: %v", name, err)
		}
	}
	if d := cert.NotAfter.Sub(time.Now()); d > 181*24*time.Hour || d < 179*24*time.Hour {
		t.Errorf("the server certificate is valid %v, want 180 days", d)
	}
	// A plain instance has neither.
	if off, _ := ServerTLS(Spec{}, time.Now()); off != nil {
		t.Error("ServerTLS of a plain server")
	}
}

// TestClusterAddrBefore60 (#1768): a Redis before 6.0 with in-transit
// encryption replicates from its front's relay on the pod's loopback, by
// the node it names; 6.x and later dial the node's Service on TLSPort.
func TestClusterAddrBefore60(t *testing.T) {
	rs := &haCluster{}
	old := &record{Server: "redis-x", TransitCA: "ca", Version: "REDIS_5_0"}
	if h, p := old.clusterAddr(rs, "redis-x-n2"); h != "127.0.0.1" || p != redistlsfront.RelayBase+3 {
		t.Errorf("node 2 of a 5.0 instance: %s:%d", h, p)
	}
	if h, p := old.clusterAddr(rs, "redis-x"); h != "127.0.0.1" || p != redistlsfront.RelayBase {
		t.Errorf("the 5.0 instance's Service: %s:%d", h, p)
	}
	cur := &record{Server: "redis-x", TransitCA: "ca", Version: "REDIS_6_X"}
	if h, p := cur.clusterAddr(rs, "redis-x-n2"); h != "redis-x-n2.svc" || p != TLSPort {
		t.Errorf("node 2 of a 6.x instance: %s:%d", h, p)
	}
	plain := &record{Server: "redis-x", Version: "REDIS_5_0"}
	if h, p := plain.clusterAddr(rs, "redis-x-n2"); h != "redis-x-n2.svc" || p != 6379 {
		t.Errorf("node 2 of a 5.0 instance without TLS: %s:%d", h, p)
	}
}
