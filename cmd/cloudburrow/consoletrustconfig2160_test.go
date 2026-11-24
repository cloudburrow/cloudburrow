package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/certificatemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/networksecurity"
)

func testCAPEM(t *testing.T, cn string) string {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestConsoleTrustConfigsAndClientAuthentication2160: Certificate
// Manager's Trust configs tab (Create Trust Config from pasted PEM, the
// Trust Config details page, Edit's Save, Delete) and Network Security's
// Client Authentication tab (Create with a validation mode and the trust
// config, listed with it, Edit, Delete), over the real APIs.
func TestConsoleTrustConfigsAndClientAuthentication2160(t *testing.T) {
	ctx := context.Background()
	screen, project := lb2160Fixture(t)
	cm, _ := certificatemanager.New(certificatemanager.Options{})
	ns, _ := networksecurity.New(networksecurity.Options{})
	useNetSec(screen.svc.api, ns, cm, nil)
	cmrest, nsrest, api := certificatemanager.NewRESTHandler(cm), networksecurity.NewRESTHandler(ns), screen.svc.api
	screen.svc.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case ns.Handles(r):
			nsrest.ServeHTTP(w, r)
		case cm.Handles(r):
			cmrest.ServeHTTP(w, r)
		default:
			api.ServeHTTP(w, r)
		}
	})
	cs := ccmScreen{screen}
	tp := ccmTrustProvider{cs}
	if _, err := tp.Create(ctx, project, map[string]string{"name": "tc", "location": "global", "trustAnchors": "not a certificate"}); err == nil {
		t.Fatal("a trust anchor that is not PEM was accepted")
	}
	if _, err := tp.Create(ctx, project, map[string]string{"name": "tc", "location": "global"}); err == nil {
		t.Fatal("an empty trust store was accepted")
	}
	root, other := testCAPEM(t, "root"), testCAPEM(t, "other")
	if _, err := tp.Create(ctx, project, map[string]string{"name": "tc", "location": "global", "description": "mTLS", "trustAnchors": root + root,
		"labels": `{"env":"test"}`}); err != nil {
		t.Fatal(err)
	}
	l, _ := tp.List(ctx, project)
	if r := vpcRow(l, "tc"); r == nil || r.Fields["Trust anchors"] != "2" || r.Fields["Location"] != "Global" {
		t.Fatalf("Trust configs = %+v", l)
	}
	d, _ := tp.Detail(ctx, project, []string{"global", "tc"})
	if d.Heading != "Trust Config details" || d.Edit == nil || d.Edit.Submit != "Save" || len(d.Sections) != 2 || d.Sections[1].Listing.Total != 2 ||
		!strings.Contains(d.Sections[1].Listing.Items[0].Fields["Subject"], "CN=root") {
		t.Fatalf("details = %+v", d)
	}
	if err := tp.Edit(ctx, project, []string{"global", "tc"}, map[string]string{"description": "edited", "trustAnchors": root, "allowlisted": other}); err != nil {
		t.Fatal(err)
	}
	if l, _ := tp.List(ctx, project); vpcRow(l, "tc").Fields["Trust anchors"] != "1" || vpcRow(l, "tc").Fields["Allowlisted certificates"] != "1" {
		t.Fatalf("after Edit = %+v", l)
	}

	ca := netsecClientAuthProvider{cs}
	if _, err := ca.Create(ctx, project, map[string]string{"name": "auth", "location": "global", "validation": "REJECT_INVALID", "trustConfig": "nope"}); err == nil {
		t.Fatal("a missing trust config was accepted")
	}
	if _, err := ca.Create(ctx, project, map[string]string{"name": "auth", "location": "global", "validation": "REJECT_INVALID", "trustConfig": "tc"}); err != nil {
		t.Fatal(err)
	}
	cl, _ := ca.List(ctx, project)
	if r := vpcRow(cl, "auth"); r == nil || r.Fields["Client Validation Mode"] != "Reject invalid" || r.Fields["Trust config"] != "tc" ||
		r.Fields["Client Authentication mode"] != "Load balancing" {
		t.Fatalf("Client Authentication = %+v", cl)
	}
	if err := tp.ActAt(ctx, project, []string{"global", "tc"}, "delete", nil); err == nil {
		t.Error("a trust config in use was deleted")
	}
	if err := ca.Edit(ctx, project, []string{"global", "auth"}, map[string]string{"validation": "ALLOW_INVALID_OR_MISSING_CLIENT_CERT", "trustConfig": "tc"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := ca.Detail(ctx, project, []string{"global", "auth"}); d.Sections[0].Groups[0].Properties[3].Value != "Allow invalid or missing client certificate" {
		t.Fatalf("after Edit = %+v", d)
	}
	if err := ca.ActAt(ctx, project, []string{"global", "auth"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if err := tp.ActAt(ctx, project, []string{"global", "tc"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := tp.List(ctx, project); vpcRow(l, "tc") != nil {
		t.Fatalf("after Delete = %+v", l)
	}
}
