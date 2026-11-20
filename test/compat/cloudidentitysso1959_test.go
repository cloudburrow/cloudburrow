//go:build compat

package compat

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"

	ci "google.golang.org/api/cloudidentity/v1"
)

// covers: cloudidentity.groups.getSecuritySettings, cloudidentity.groups.updateSecuritySettings
//
// TestCloudIdentityMemberRestriction1959 (#1959), with the official
// google.golang.org/api/cloudidentity/v1 client: a group's security
// settings start with no member restriction; updateSecuritySettings (mask
// member_restriction.query alone, 400 otherwise; an unsupported query
// 400) sets "member.type == 1 && member.customer_id == groupCustomerId()";
// memberships.create then refuses a service account and a user of another
// domain directly (400 FAILED_PRECONDITION), takes a user of the domain,
// and reports the MEMBER role's restrictionEvaluations; the restriction's
// evaluation is COMPLIANT.
func TestCloudIdentityMemberRestriction1959(t *testing.T) {
	h := New(t)
	svc := cloudIdentity(t, h, h.Endpoint(EnvResourceManager))
	ctx := h.Context()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	g, _ := groupFixture(t, svc, "ci-restricted-"+suffix, "ci-restricted-inner-"+suffix)
	name := g.Name + "/securitySettings"

	ss, err := svc.Groups.GetSecuritySettings(name).Context(ctx).Do()
	if err != nil || ss.Name != name || ss.MemberRestriction != nil {
		t.Fatalf("getSecuritySettings = %+v, %v", ss, err)
	}
	q := "member.type == 1 && member.customer_id == groupCustomerId()"
	if _, err := svc.Groups.UpdateSecuritySettings(name, &ci.SecuritySettings{MemberRestriction: &ci.MemberRestriction{Query: q}}).
		UpdateMask("member_restriction").Context(ctx).Do(); apiCode(err) != 400 {
		t.Errorf("updateSecuritySettings with mask member_restriction: %v, want 400", err)
	}
	if _, err := svc.Groups.UpdateSecuritySettings(name, &ci.SecuritySettings{MemberRestriction: &ci.MemberRestriction{Query: "member.type == 4"}}).
		UpdateMask("member_restriction.query").Context(ctx).Do(); apiCode(err) != 400 {
		t.Errorf("updateSecuritySettings with member.type == 4: %v, want 400", err)
	}
	op, err := svc.Groups.UpdateSecuritySettings(name, &ci.SecuritySettings{MemberRestriction: &ci.MemberRestriction{Query: q}}).
		UpdateMask("member_restriction.query").Context(ctx).Do()
	if err != nil || !op.Done {
		t.Fatalf("updateSecuritySettings = %+v, %v", op, err)
	}

	for _, refused := range []string{"robot-" + suffix + "@compat-project.iam.gserviceaccount.com", "someone-" + suffix + "@example.com"} {
		_, err := svc.Groups.Memberships.Create(g.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: refused}}).Context(ctx).Do()
		if apiCode(err) != 400 {
			t.Errorf("memberships.create %s under %q: %v, want 400 FAILED_PRECONDITION", refused, q, err)
		}
	}
	user := "user-" + suffix + "@" + localDomain
	op, err = svc.Groups.Memberships.Create(g.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: user}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("memberships.create %s: %v", user, err)
	}
	var m ci.Membership
	if err := json.Unmarshal(op.Response, &m); err != nil {
		t.Fatal(err)
	}
	var state string
	for _, r := range m.Roles {
		if r.Name == "MEMBER" && r.RestrictionEvaluations != nil && r.RestrictionEvaluations.MemberRestrictionEvaluation != nil {
			state = r.RestrictionEvaluations.MemberRestrictionEvaluation.State
		}
	}
	if state != "COMPLIANT" {
		t.Errorf("the user's MEMBER restrictionEvaluations = %q, want COMPLIANT (%+v)", state, m.Roles)
	}
	ss, err = svc.Groups.GetSecuritySettings(name).ReadMask("member_restriction").Context(ctx).Do()
	if err != nil || ss.MemberRestriction == nil || ss.MemberRestriction.Query != q || ss.MemberRestriction.Evaluation == nil ||
		ss.MemberRestriction.Evaluation.State != "COMPLIANT" {
		t.Errorf("getSecuritySettings = %+v, %v", ss, err)
	}
}

// localIdPCertificate is a self-signed RSA certificate made here, for an
// IdP credential: never a real IdP's.
func localIdPCertificate(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "compat test IdP"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// covers: cloudidentity.inboundSamlSsoProfiles.create, cloudidentity.inboundSamlSsoProfiles.get, cloudidentity.inboundSamlSsoProfiles.list, cloudidentity.inboundSamlSsoProfiles.patch, cloudidentity.inboundSamlSsoProfiles.delete, cloudidentity.inboundSamlSsoProfiles.idpCredentials.add, cloudidentity.inboundSamlSsoProfiles.idpCredentials.get, cloudidentity.inboundSamlSsoProfiles.idpCredentials.list, cloudidentity.inboundSamlSsoProfiles.idpCredentials.delete, cloudidentity.inboundSsoAssignments.create, cloudidentity.inboundSsoAssignments.get, cloudidentity.inboundSsoAssignments.list, cloudidentity.inboundSsoAssignments.patch, cloudidentity.inboundSsoAssignments.delete, cloudidentity.inboundOidcSsoProfiles.create, cloudidentity.inboundOidcSsoProfiles.get, cloudidentity.inboundOidcSsoProfiles.list, cloudidentity.inboundOidcSsoProfiles.patch, cloudidentity.inboundOidcSsoProfiles.delete
//
// TestCloudIdentityInboundSSO1959 (#1959), with the official client: a
// SAML SSO profile (an https sign-in URL required, 400 for http) is
// created, read, listed by customer and patched, with its service
// provider details; two IdP credentials (locally generated RSA
// certificates) are added, a third refused (400), one read, listed with
// its key size and deleted; SSO assignments target a group (rank 1 and
// up, 400 for 0; a new rank 1 moves the other down) and an org unit
// (SSO_OFF, rank 0); a profile in use cannot be deleted (400); an OIDC
// profile's client secret is input only. Nothing signs in locally.
func TestCloudIdentityInboundSSO1959(t *testing.T) {
	h := New(t)
	svc := cloudIdentity(t, h, h.Endpoint(EnvResourceManager))
	ctx := h.Context()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	ga, gb := groupFixture(t, svc, "ci-sso-a-"+suffix, "ci-sso-b-"+suffix)

	if _, err := svc.InboundSamlSsoProfiles.Create(&ci.InboundSamlSsoProfile{Customer: localCustomer,
		IdpConfig: &ci.SamlIdpConfig{EntityId: "https://idp.example.com", SingleSignOnServiceUri: "http://idp.example.com/sso"}}).Context(ctx).Do(); apiCode(err) != 400 {
		t.Errorf("a SAML profile with an http sign-in URL: %v, want 400", err)
	}
	op, err := svc.InboundSamlSsoProfiles.Create(&ci.InboundSamlSsoProfile{Customer: localCustomer, DisplayName: "Compat IdP " + suffix,
		IdpConfig: &ci.SamlIdpConfig{EntityId: "https://idp.example.com/" + suffix, SingleSignOnServiceUri: "https://idp.example.com/sso"}}).Context(ctx).Do()
	if err != nil || !op.Done {
		t.Fatalf("inboundSamlSsoProfiles.create = %+v, %v", op, err)
	}
	var prof ci.InboundSamlSsoProfile
	if err := json.Unmarshal(op.Response, &prof); err != nil || prof.Name == "" {
		t.Fatalf("created profile = %s, %v", op.Response, err)
	}
	t.Cleanup(func() { _, _ = svc.InboundSamlSsoProfiles.Delete(prof.Name).Context(context.Background()).Do() })
	got, err := svc.InboundSamlSsoProfiles.Get(prof.Name).Context(ctx).Do()
	if err != nil || got.Customer != localCustomer || got.SpConfig == nil || got.SpConfig.EntityId == "" || got.SpConfig.AssertionConsumerServiceUri == "" {
		t.Errorf("inboundSamlSsoProfiles.get = %+v, %v", got, err)
	}
	list, err := svc.InboundSamlSsoProfiles.List().Filter(`customer=="` + localCustomer + `"`).Context(ctx).Do()
	found := false
	for _, p := range list.InboundSamlSsoProfiles {
		found = found || p.Name == prof.Name
	}
	if err != nil || !found {
		t.Errorf("inboundSamlSsoProfiles.list: %v, found %v", err, found)
	}
	if _, err := svc.InboundSamlSsoProfiles.Patch(prof.Name, &ci.InboundSamlSsoProfile{DisplayName: "Renamed " + suffix}).UpdateMask("displayName").Context(ctx).Do(); err != nil {
		t.Errorf("inboundSamlSsoProfiles.patch: %v", err)
	}

	cert := localIdPCertificate(t)
	for i := 0; i < 2; i++ {
		if op, err := svc.InboundSamlSsoProfiles.IdpCredentials.Add(prof.Name, &ci.AddIdpCredentialRequest{PemData: cert}).Context(ctx).Do(); err != nil || !op.Done {
			t.Fatalf("idpCredentials.add %d = %+v, %v", i, op, err)
		}
	}
	if _, err := svc.InboundSamlSsoProfiles.IdpCredentials.Add(prof.Name, &ci.AddIdpCredentialRequest{PemData: cert}).Context(ctx).Do(); apiCode(err) != 400 {
		t.Errorf("a third IdP credential: %v, want 400 (up to 2 are allowed)", err)
	}
	creds, err := svc.InboundSamlSsoProfiles.IdpCredentials.List(prof.Name).Context(ctx).Do()
	if err != nil || len(creds.IdpCredentials) != 2 || creds.IdpCredentials[0].RsaKeyInfo == nil || creds.IdpCredentials[0].RsaKeyInfo.KeySize != 2048 {
		t.Fatalf("idpCredentials.list = %+v, %v", creds, err)
	}
	if c, err := svc.InboundSamlSsoProfiles.IdpCredentials.Get(creds.IdpCredentials[1].Name).Context(ctx).Do(); err != nil || c.UpdateTime == "" {
		t.Errorf("idpCredentials.get = %+v, %v", c, err)
	}
	if _, err := svc.InboundSamlSsoProfiles.IdpCredentials.Delete(creds.IdpCredentials[0].Name).Context(ctx).Do(); err != nil {
		t.Errorf("idpCredentials.delete: %v", err)
	}

	if _, err := svc.InboundSsoAssignments.Create(&ci.InboundSsoAssignment{Customer: localCustomer, TargetGroup: ga.Name, SsoMode: "SAML_SSO",
		SamlSsoInfo: &ci.SamlSsoInfo{InboundSamlSsoProfile: prof.Name}}).Context(ctx).Do(); apiCode(err) != 400 {
		t.Errorf("a group assignment of rank 0: %v, want 400", err)
	}
	assign := func(a *ci.InboundSsoAssignment) string {
		t.Helper()
		op, err := svc.InboundSsoAssignments.Create(a).Context(ctx).Do()
		if err != nil || !op.Done {
			t.Fatalf("inboundSsoAssignments.create %+v = %+v, %v", a, op, err)
		}
		var out ci.InboundSsoAssignment
		if err := json.Unmarshal(op.Response, &out); err != nil || out.Name == "" {
			t.Fatalf("created assignment = %s, %v", op.Response, err)
		}
		t.Cleanup(func() { _, _ = svc.InboundSsoAssignments.Delete(out.Name).Context(context.Background()).Do() })
		return out.Name
	}
	a1 := assign(&ci.InboundSsoAssignment{Customer: localCustomer, TargetGroup: ga.Name, Rank: 1, SsoMode: "SAML_SSO",
		SamlSsoInfo: &ci.SamlSsoInfo{InboundSamlSsoProfile: prof.Name}})
	a2 := assign(&ci.InboundSsoAssignment{Customer: localCustomer, TargetGroup: gb.Name, Rank: 1, SsoMode: "SAML_SSO",
		SamlSsoInfo: &ci.SamlSsoInfo{InboundSamlSsoProfile: prof.Name}, SignInBehavior: &ci.SignInBehavior{RedirectCondition: "NEVER"}})
	if a, err := svc.InboundSsoAssignments.Get(a1).Context(ctx).Do(); err != nil || a.Rank < 2 {
		t.Errorf("the first assignment after another took rank 1 = %+v, %v", a, err)
	}
	if _, err := svc.InboundSsoAssignments.Patch(a1, &ci.InboundSsoAssignment{Rank: 1}).UpdateMask("rank").Context(ctx).Do(); err != nil {
		t.Errorf("inboundSsoAssignments.patch rank: %v", err)
	}
	if a, err := svc.InboundSsoAssignments.Get(a2).Context(ctx).Do(); err != nil || a.Rank < 2 || a.SignInBehavior == nil {
		t.Errorf("the second assignment after the first moved to rank 1 = %+v, %v", a, err)
	}
	ou := assign(&ci.InboundSsoAssignment{Customer: localCustomer, TargetOrgUnit: "orgUnits/03ph8a2z" + suffix, SsoMode: "SSO_OFF"})
	all, err := svc.InboundSsoAssignments.List().Context(ctx).Do()
	seen := map[string]bool{}
	for _, a := range all.InboundSsoAssignments {
		seen[a.Name] = true
	}
	if err != nil || !seen[a1] || !seen[a2] || !seen[ou] {
		t.Errorf("inboundSsoAssignments.list: %v, saw %v", err, seen)
	}
	if _, err := svc.InboundSamlSsoProfiles.Delete(prof.Name).Context(ctx).Do(); apiCode(err) != 400 {
		t.Errorf("deleting a profile an assignment uses: %v, want 400", err)
	}
	for _, a := range []string{a1, a2, ou} {
		if _, err := svc.InboundSsoAssignments.Delete(a).Context(ctx).Do(); err != nil {
			t.Errorf("inboundSsoAssignments.delete %s: %v", a, err)
		}
	}
	if _, err := svc.InboundSamlSsoProfiles.Delete(prof.Name).Context(ctx).Do(); err != nil {
		t.Errorf("inboundSamlSsoProfiles.delete: %v", err)
	}

	op, err = svc.InboundOidcSsoProfiles.Create(&ci.InboundOidcSsoProfile{Customer: localCustomer, DisplayName: "Compat OIDC " + suffix,
		IdpConfig: &ci.OidcIdpConfig{IssuerUri: "https://issuer.example.com"},
		RpConfig:  &ci.OidcRpConfig{ClientId: "client-" + suffix, ClientSecret: "local-test-secret"}}).Context(ctx).Do()
	if err != nil || !op.Done {
		t.Fatalf("inboundOidcSsoProfiles.create = %+v, %v", op, err)
	}
	var oidc ci.InboundOidcSsoProfile
	if err := json.Unmarshal(op.Response, &oidc); err != nil || oidc.Name == "" {
		t.Fatalf("created OIDC profile = %s, %v", op.Response, err)
	}
	if o, err := svc.InboundOidcSsoProfiles.Get(oidc.Name).Context(ctx).Do(); err != nil || o.RpConfig == nil || o.RpConfig.ClientId != "client-"+suffix ||
		o.RpConfig.ClientSecret != "" || len(o.RpConfig.RedirectUris) == 0 {
		t.Errorf("inboundOidcSsoProfiles.get = %+v, %v", o, err)
	}
	if _, err := svc.InboundOidcSsoProfiles.Patch(oidc.Name, &ci.InboundOidcSsoProfile{IdpConfig: &ci.OidcIdpConfig{IssuerUri: "https://issuer2.example.com"}}).
		UpdateMask("idpConfig.issuerUri").Context(ctx).Do(); err != nil {
		t.Errorf("inboundOidcSsoProfiles.patch: %v", err)
	}
	if l, err := svc.InboundOidcSsoProfiles.List().Context(ctx).Do(); err != nil || len(l.InboundOidcSsoProfiles) == 0 {
		t.Errorf("inboundOidcSsoProfiles.list = %+v, %v", l, err)
	}
	if _, err := svc.InboundOidcSsoProfiles.Delete(oidc.Name).Context(ctx).Do(); err != nil {
		t.Errorf("inboundOidcSsoProfiles.delete: %v", err)
	}
}
