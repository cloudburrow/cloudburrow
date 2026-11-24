//go:build compat

package compat

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // Cloud SQL names certificates by SHA-1 fingerprint
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// presentedServerCert is the SHA-1 fingerprint of the server certificate
// the connector listener presents for instance, verified as the official
// connector verifies it: against connect.get's serverCaCert, for its first
// DNS name, with a client certificate generateEphemeralCert signed.
func presentedServerCert(t *testing.T, svc *sqladmin.Service, project, instance string) string {
	t.Helper()
	cs, err := svc.Connect.Get(project, instance).Do()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cs.ServerCaCert.Cert)) {
		t.Fatal("serverCaCert holds no certificate")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	eph, err := svc.Connect.GenerateEphemeralCert(project, instance, &sqladmin.GenerateEphemeralCertRequest{
		PublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}).Do()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode([]byte(eph.EphemeralCert.Cert))
	addr := os.Getenv(EnvCloudSQLConnector)
	if addr == "" {
		addr = net.JoinHostPort(cs.IpAddresses[0].IpAddress, "3307")
	}
	name := cs.DnsName
	if len(cs.DnsNames) > 0 {
		name = cs.DnsNames[0].Name
	}
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: strings.TrimSuffix(name, "."),
		Certificates: []tls.Certificate{{Certificate: [][]byte{b.Bytes}, PrivateKey: key}},
	})
	if err != nil {
		t.Fatalf("TLS to %s's server: %v", instance, err)
	}
	defer c.Close()
	fp := sha1.Sum(c.ConnectionState().PeerCertificates[0].Raw) //nolint:gosec
	return hex.EncodeToString(fp[:])
}

// TestCloudSQLServerCertificates (#1614): through the official Go client,
// an instance created with settings.ipConfiguration.serverCaMode
// GOOGLE_MANAGED_CAS_CA answers connect.get with that mode, its region's
// CA chain (a subordinate CA under a root) and a DNS name, and the
// official Go connector (pgx) reaches it, checking the server certificate
// against that chain and DNS name; connect.resolve finds it by that name.
// Its server certificate is rotated: listServerCertificates answers the
// active version, which the server presents; addServerCertificate adds an
// upcoming one, rotateServerCertificate makes it the presented one (and the
// connector still reaches the server), and a rotation to the previous
// version by nextVersion rolls back. gcloud sql ssl server-certs create,
// list, rotate and rollback do the same. An internal-CA instance's server
// certificates and a CAS instance's addServerCa are 400.
// CUSTOMER_MANAGED_CAS_CA is TestCloudSQLCustomerManagedCA's (#1834).
// covers: sql.instances.ListServerCertificates, sql.instances.addServerCertificate, sql.instances.RotateServerCertificate, sql.connect.resolve, sql.connect.get, sql.connect.generateEphemeral, sql.instances.insert, sql.instances.get, sql.instances.delete, sql.instances.addServerCa
func TestCloudSQLServerCertificates(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	project := h.Project()
	n := time.Now().UnixNano() % 1e6
	name := fmt.Sprintf("cas-%d", n)

	in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840",
			IpConfiguration: &sqladmin.IpConfiguration{ServerCaMode: "GOOGLE_MANAGED_CAS_CA"}}})
	if in.Settings.IpConfiguration == nil || in.Settings.IpConfiguration.ServerCaMode != "GOOGLE_MANAGED_CAS_CA" || in.DnsName == "" {
		t.Fatalf("the instance: ipConfiguration %+v dnsName %q", in.Settings.IpConfiguration, in.DnsName)
	}
	cs, err := svc.Connect.Get(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if cs.ServerCaMode != "GOOGLE_MANAGED_CAS_CA" || len(cs.DnsNames) != 1 || cs.DnsNames[0].Name != in.DnsName ||
		strings.Count(cs.ServerCaCert.Cert, "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("connect settings: mode %s dnsNames %+v chain of %d", cs.ServerCaMode, cs.DnsNames,
			strings.Count(cs.ServerCaCert.Cert, "BEGIN CERTIFICATE"))
	}
	db := sqlEngine{}.root(t, h, in.ConnectionName)
	query := func(what string) {
		t.Helper()
		eventually(t, what, func() error {
			var one int
			return db.QueryRow("SELECT 1").Scan(&one)
		})
	}
	query("SELECT 1 through the official connector")

	got, err := svc.Connect.Resolve(in.Region, in.DnsName).Do()
	if err != nil || got.ConnectionName != in.ConnectionName || got.ServerCaMode != "GOOGLE_MANAGED_CAS_CA" {
		t.Fatalf("resolve %s: %v %+v", in.DnsName, err, got)
	}
	_, err = svc.Connect.Resolve("europe-west1", in.DnsName).Do()
	wantCode(t, "resolve in another region", err, 404, "DNS name")

	list, err := svc.Instances.ListServerCertificates(project, name).Do()
	if err != nil || len(list.ServerCerts) != 1 || len(list.CaCerts) != 1 {
		t.Fatalf("list: %v %+v", err, list)
	}
	first := list.ActiveVersion
	if fp := presentedServerCert(t, svc, project, name); fp != first {
		t.Fatalf("the server presents %s; the active version is %s", fp, first)
	}
	_, err = svc.Instances.RotateServerCertificate(project, name, &sqladmin.InstancesRotateServerCertificateRequest{}).Do()
	wantCode(t, "rotate with no upcoming version", err, 400, "addServerCertificate")
	op, err := svc.Instances.AddServerCertificate(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOpDone(t, svc, project, op)
	list, _ = svc.Instances.ListServerCertificates(project, name).Do()
	if len(list.ServerCerts) != 2 || list.ActiveVersion != first {
		t.Fatalf("after add: %+v", list)
	}
	next := list.ServerCerts[1].Sha1Fingerprint
	op, err = svc.Instances.RotateServerCertificate(project, name, &sqladmin.InstancesRotateServerCertificateRequest{}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOpDone(t, svc, project, op)
	if fp := presentedServerCert(t, svc, project, name); fp != next {
		t.Errorf("after rotate the server presents %s, want %s", fp, next)
	}
	db = sqlEngine{}.root(t, h, in.ConnectionName)
	query("SELECT 1 after the rotation, through a new connector")
	op, err = svc.Instances.RotateServerCertificate(project, name, &sqladmin.InstancesRotateServerCertificateRequest{
		RotateServerCertificateContext: &sqladmin.RotateServerCertificateContext{NextVersion: first}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOpDone(t, svc, project, op)
	if fp := presentedServerCert(t, svc, project, name); fp != first {
		t.Errorf("after the rollback the server presents %s, want %s", fp, first)
	}

	// Each CA mode's own methods.
	_, err = svc.Instances.AddServerCa(project, name).Do()
	wantCode(t, "addServerCa of a CAS instance", err, 400, "addServerCertificate")
	plain := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: name + "-int", DatabaseVersion: "POSTGRES_17", RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	_, err = svc.Instances.ListServerCertificates(project, plain.Name).Do()
	wantCode(t, "server certificates of an internal-CA instance", err, 400, "addServerCa")

	// gcloud sql ssl server-certs, through `cloudburrow gcloud-setup`'s
	// configuration, egress to Google blocked.
	g := newGcloudSession(t, h)
	g.must("sql", "ssl", "server-certs", "create", "--instance", name, "--project", project)
	out := g.must("sql", "ssl", "server-certs", "list", "--instance", name, "--project", project, "--format=value(state,ssl_cert.sha1Fingerprint)")
	if !strings.Contains(out, "NEXT") || !strings.Contains(out, "ACTIVE\t"+first) {
		t.Fatalf("gcloud sql ssl server-certs list after create:\n%s", out)
	}
	g.must("sql", "ssl", "server-certs", "rotate", "--instance", name, "--project", project)
	out = g.must("sql", "ssl", "server-certs", "list", "--instance", name, "--project", project, "--format=value(state,ssl_cert.sha1Fingerprint)")
	if !strings.Contains(out, "PREVIOUS\t"+first) {
		t.Fatalf("gcloud sql ssl server-certs list after rotate:\n%s", out)
	}
	g.must("sql", "ssl", "server-certs", "rollback", "--instance", name, "--project", project)
	if fp := presentedServerCert(t, svc, project, name); fp != first {
		t.Errorf("after gcloud's rollback the server presents %s, want %s", fp, first)
	}
}
