package sqladmin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"strings"
	"testing"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// presented is the fingerprint of the server certificate the listener
// presents for connection name proj:instance, checked as the Go connector
// checks it: against connect settings' serverCaCert, for its DNS name.
func presented(t *testing.T, svc *sqladmin.Service, connAddr, instance string) string {
	t.Helper()
	cs, err := svc.Connect.Get("proj", instance).Do()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cs.ServerCaCert.Cert)) {
		t.Fatal("serverCaCert holds no certificate")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	eph, err := svc.Connect.GenerateEphemeralCert("proj", instance, &sqladmin.GenerateEphemeralCertRequest{
		PublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}).Do()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode([]byte(eph.EphemeralCert.Cert))
	name := cs.DnsName
	if len(cs.DnsNames) > 0 {
		name = cs.DnsNames[0].Name
	}
	c, err := tls.Dial("tcp", connAddr, &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: strings.TrimSuffix(name, "."),
		Certificates: []tls.Certificate{{Certificate: [][]byte{b.Bytes}, PrivateKey: key}},
	})
	if err != nil {
		t.Fatalf("TLS to %s: %v", instance, err)
	}
	defer c.Close()
	fp := sha1.Sum(c.ConnectionState().PeerCertificates[0].Raw) //nolint:gosec
	return hex.EncodeToString(fp[:])
}

// TestSQLServerCertificates (#1614): a GOOGLE_MANAGED_CAS_CA instance's
// server certificate is signed by its region's CA chain, which connect
// settings answers with its DNS names; addServerCertificate adds an
// upcoming version, rotateServerCertificate makes it the one the listener
// presents, and a rotation to the previous one rolls back. An internal-CA
// instance refuses the server certificate methods, a CAS one the server CA
// methods, and connect.resolve finds an instance by its DNS name.
func TestSQLServerCertificates(t *testing.T) {
	srv := NewServer("127.0.0.1:0", newFake())
	if err := srv.EnableConnector("127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	srv.EnableInstanceWrites(newFakeProv(), "")
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop(ctx)
	connAddr := srv.conn.ln.Addr().String()
	api := "http://" + srv.Addr() + "/"
	svc, err := sqladmin.NewService(ctx, option.WithEndpoint(api), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	base := api + "v1/projects/proj/instances"
	for _, b := range []string{
		`{"name":"cas","databaseVersion":"POSTGRES_17","settings":{"tier":"t","ipConfiguration":{"serverCaMode":"GOOGLE_MANAGED_CAS_CA"}}}`,
		`{"name":"internal","databaseVersion":"POSTGRES_17","settings":{"tier":"t"}}`,
	} {
		if code, resp := call(t, "POST", base, b); code != 200 {
			t.Fatalf("insert = %d %s", code, resp)
		}
	}
	in := getRunnable(t, base+"/cas")
	if in["dnsName"] == nil || in["dnsNames"] == nil {
		t.Errorf("a CAS instance answers no DNS names: %v", in)
	}

	cs, err := svc.Connect.Get("proj", "cas").Do()
	if err != nil || cs.ServerCaMode != casCA || len(cs.DnsNames) != 1 || strings.Count(cs.ServerCaCert.Cert, "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("CAS connect settings: %v %+v", err, cs)
	}
	got, err := svc.Connect.Resolve("us-central1", cs.DnsNames[0].Name).Do()
	if err != nil || got.ServerCaMode != casCA || got.DatabaseVersion != "POSTGRES_17" {
		t.Errorf("resolve: %v %+v", err, got)
	}
	for _, bad := range [][2]string{{"europe-west1", cs.DnsNames[0].Name}, {"us-central1", "cas.other" + dnsSuffix}, {"us-central1", "x.example.com"}} {
		if _, err := svc.Connect.Resolve(bad[0], bad[1]).Do(); err == nil || !strings.Contains(err.Error(), "404") {
			t.Errorf("resolve %v: %v", bad, err)
		}
	}

	list, err := svc.Instances.ListServerCertificates("proj", "cas").Do()
	if err != nil || len(list.ServerCerts) != 1 || len(list.CaCerts) != 1 || list.ActiveVersion != list.ServerCerts[0].Sha1Fingerprint {
		t.Fatalf("list: %v %+v", err, list)
	}
	first := list.ActiveVersion
	if fp := presented(t, svc, connAddr, "cas"); fp != first {
		t.Errorf("the listener presents %s, the active version is %s", fp, first)
	}
	if _, err := svc.Instances.RotateServerCertificate("proj", "cas", &sqladmin.InstancesRotateServerCertificateRequest{}).Do(); err == nil ||
		!strings.Contains(err.Error(), "400") {
		t.Errorf("rotate with no upcoming version: %v", err)
	}
	for i := 0; i < 2; i++ { // the second replaces the first, never rotated in
		if op, err := svc.Instances.AddServerCertificate("proj", "cas").Do(); err != nil || op.Status != "DONE" {
			t.Fatalf("add: %v %+v", err, op)
		}
	}
	list, _ = svc.Instances.ListServerCertificates("proj", "cas").Do()
	if len(list.ServerCerts) != 2 || list.ActiveVersion != first {
		t.Fatalf("after add: %+v", list)
	}
	next := list.ServerCerts[1].Sha1Fingerprint
	if !(list.ServerCerts[1].CreateTime > list.ServerCerts[0].CreateTime) {
		t.Errorf("the upcoming version is not newer: %s, %s", list.ServerCerts[1].CreateTime, list.ServerCerts[0].CreateTime)
	}
	if _, err := svc.Instances.RotateServerCertificate("proj", "cas", &sqladmin.InstancesRotateServerCertificateRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	if fp := presented(t, svc, connAddr, "cas"); fp != next {
		t.Errorf("after rotate the listener presents %s, want %s", fp, next)
	}
	// gcloud's rollback: back to the previous version by name.
	if _, err := svc.Instances.RotateServerCertificate("proj", "cas", &sqladmin.InstancesRotateServerCertificateRequest{
		RotateServerCertificateContext: &sqladmin.RotateServerCertificateContext{NextVersion: first}}).Do(); err != nil {
		t.Fatal(err)
	}
	if fp := presented(t, svc, connAddr, "cas"); fp != first {
		t.Errorf("after rollback the listener presents %s, want %s", fp, first)
	}
	if _, err := svc.Instances.RotateServerCertificate("proj", "cas", &sqladmin.InstancesRotateServerCertificateRequest{
		RotateServerCertificateContext: &sqladmin.RotateServerCertificateContext{NextVersion: "nope"}}).Do(); err == nil ||
		!strings.Contains(err.Error(), "400") {
		t.Errorf("rotate to an unknown version: %v", err)
	}

	// Each CA mode's own methods.
	if _, err := svc.Instances.ListServerCertificates("proj", "internal").Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("server certificates of an internal-CA instance: %v", err)
	}
	if _, err := svc.Instances.AddServerCa("proj", "cas").Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("addServerCa of a CAS instance: %v", err)
	}
	if cas, err := svc.Instances.ListServerCas("proj", "cas").Do(); err != nil || len(cas.Certs) != 1 {
		t.Errorf("listServerCas of a CAS instance: %v %+v", err, cas)
	}
	// The internal-CA instance is still reached as before.
	presented(t, svc, connAddr, "internal")

	// Switching a created instance to the internal CA keeps it reachable.
	if code, resp := call(t, "PATCH", base+"/cas", `{"settings":{"ipConfiguration":{"serverCaMode":"GOOGLE_MANAGED_INTERNAL_CA"}}}`); code != 200 {
		t.Fatalf("patch = %d %s", code, resp)
	}
	if cs, err := svc.Connect.Get("proj", "cas").Do(); err != nil || cs.ServerCaMode != internalCA {
		t.Fatalf("after the patch: %v %+v", err, cs)
	}
	presented(t, svc, connAddr, "cas")
}
