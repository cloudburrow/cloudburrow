package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func localTestCertificate(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "local test IdP"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func detailProp(d console.Detail, label string) string {
	for _, s := range d.Sections {
		for _, g := range s.Groups {
			for _, p := range g.Properties {
				if p.Label == label {
					return p.Value
				}
			}
		}
	}
	return ""
}

// TestConsoleIAMSSO1959 (#1959): a group's member restriction set from
// Edit group refuses an excluded member and shows its state; SSO profiles
// (SAML with an IdP certificate, OIDC) are added, edited and deleted; an
// assignment gives a group a profile with a priority and an org unit None.
func TestConsoleIAMSSO1959(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	ctx := context.Background()
	d := "@" + resourcemanager.OrganizationDomain
	groups := iamGroupsProvider{registry: reg}
	for _, e := range []string{"eng" + d, "ops" + d} {
		if _, err := groups.Create(ctx, "", map[string]string{"email": e}); err != nil {
			t.Fatal(err)
		}
	}
	if err := groups.Edit(ctx, "", []string{"eng" + d}, map[string]string{"memberRestriction": "member.type == 1 && member.customer_id == groupCustomerId()"}); err != nil {
		t.Fatal(err)
	}
	if err := groups.Edit(ctx, "", []string{"eng" + d}, map[string]string{"add": "bot@p.iam.gserviceaccount.com"}); err == nil {
		t.Error("Add member took a service account under a users-only restriction")
	}
	if err := groups.Edit(ctx, "", []string{"eng" + d}, map[string]string{"add": "ada" + d}); err != nil {
		t.Fatal(err)
	}
	if err := groups.Edit(ctx, "", []string{"eng" + d}, map[string]string{"memberRestriction": "member.type == 9"}); err == nil {
		t.Error("Edit group took member.type == 9")
	}
	det, _ := groups.Detail(ctx, "", []string{"eng" + d})
	if detailProp(det, "Restriction state") != "COMPLIANT" || det.Sections[0].Listing.Items[0].Fields["Restriction"] != "COMPLIANT" {
		t.Errorf("eng's security settings = %q, member row %+v", detailProp(det, "Restriction state"), det.Sections[0].Listing.Items)
	}

	profiles := iamSSOProfilesProvider{registry: reg}
	if _, err := profiles.Create(ctx, "", map[string]string{"type": "SAML", "entityId": "https://idp.example.com", "ssoUrl": "http://idp.example.com/sso"}); err == nil {
		t.Error("Add SSO profile took an http sign-in page URL")
	}
	saml, err := profiles.Create(ctx, "", map[string]string{"type": "SAML", "displayName": "Corp IdP", "entityId": "https://idp.example.com", "ssoUrl": "https://idp.example.com/sso"})
	if err != nil {
		t.Fatal(err)
	}
	oidc, err := profiles.Create(ctx, "", map[string]string{"type": "OIDC", "displayName": "OIDC IdP", "issuerUri": "https://issuer.example.com", "clientId": "c", "clientSecret": "s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := profiles.Edit(ctx, "", []string{saml}, map[string]string{"displayName": "Corp", "entityId": "https://idp.example.com", "ssoUrl": "https://idp.example.com/sso",
		"certificate": localTestCertificate(t)}); err != nil {
		t.Fatal(err)
	}
	sd, _ := profiles.Detail(ctx, "", []string{saml})
	if sd.Unavailable != "" || detailProp(sd, "ACS URL") == "" || len(sd.Sections[1].Listing.Items) != 1 || sd.Sections[1].Listing.Items[0].Fields["Key"] != "RSA 2048-bit" {
		t.Fatalf("SAML profile page = %+v", sd)
	}
	od, _ := profiles.Detail(ctx, "", []string{oidc})
	if detailProp(od, "Client secret") != "Set (never shown)" || !strings.HasPrefix(detailProp(od, "Redirect URI"), "https://") {
		t.Errorf("OIDC profile page = %+v", od.Sections)
	}
	l, _ := profiles.List(ctx, "")
	if len(l.Items) != 2 {
		t.Errorf("SSO profiles = %+v", l.Items)
	}

	as := iamSSOAssignmentsProvider{registry: reg}
	a1, err := as.Create(ctx, "", map[string]string{"targetType": "group", "group": "eng" + d, "rank": "1", "mode": "SAML_SSO", "profile": saml})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := as.Create(ctx, "", map[string]string{"targetType": "group", "group": "ops" + d, "rank": "1", "mode": "OIDC_SSO", "profile": oidc, "redirect": "NEVER"}); err != nil {
		t.Fatal(err)
	}
	if _, err := as.Create(ctx, "", map[string]string{"targetType": "orgUnit", "orgUnit": "03ph8a2z1", "mode": "SSO_OFF"}); err != nil {
		t.Fatal(err)
	}
	al, _ := as.List(ctx, "")
	if len(al.Items) != 3 || al.Items[0].Fields["Target"] != "Group ops"+d || al.Items[1].Name != a1 || al.Items[1].Fields["Priority"] != "2" {
		t.Fatalf("assignments = %+v", al.Items)
	}
	if err := as.Edit(ctx, "", []string{a1}, map[string]string{"mode": "SAML_SSO", "profile": saml, "rank": "1"}); err != nil {
		t.Fatal(err)
	}
	if al, _ = as.List(ctx, ""); al.Items[0].Name != a1 {
		t.Errorf("after moving eng's assignment to priority 1: %+v", al.Items)
	}
	if err := profiles.Delete(ctx, "", saml); err == nil {
		t.Error("deleted a profile an assignment uses")
	}
	for _, it := range al.Items {
		if err := as.Delete(ctx, "", it.Name); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{saml, oidc} {
		if err := profiles.Delete(ctx, "", id); err != nil {
			t.Fatal(err)
		}
	}
}
