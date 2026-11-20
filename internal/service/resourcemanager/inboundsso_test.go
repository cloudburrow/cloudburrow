package resourcemanager

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http/httptest"
	"testing"
	"time"

	ci "google.golang.org/api/cloudidentity/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func TestMemberRestrictionParse1959(t *testing.T) {
	for _, tc := range []struct {
		q    string
		ok   bool
		want []memberFacts // allowed
	}{
		{"member.type == 2 && member.customer_id == groupCustomerId()", true, []memberFacts{{2, true}}},
		{"member.type == 1 || member.type == 3", true, []memberFacts{{1, true}, {1, false}, {3, true}, {3, false}}},
		{"(member.type == 1 || member.type == 2) && member.customer_id == groupCustomerId()", true, []memberFacts{{1, true}, {2, true}}},
		{"groupCustomerId() == member.customer_id", true, []memberFacts{{1, true}, {2, true}, {3, true}}},
		{"member.type == 4", false, nil},
		{"member.type != 1", false, nil},
		{"member.customer_id == 'C123'", false, nil},
		{"member.domain == 'x'", false, nil},
		{"member.type == 1 &&", false, nil},
		{"(member.type == 1", false, nil},
	} {
		x, err := parseMemberRestriction(tc.q)
		if (err == nil) != tc.ok {
			t.Errorf("parse(%q) err = %v, want ok %v", tc.q, err, tc.ok)
			continue
		}
		if !tc.ok {
			continue
		}
		var got []memberFacts
		for _, f := range allFacts {
			if x.allows(f) {
				got = append(got, f)
			}
		}
		if len(got) != len(tc.want) {
			t.Errorf("%q allows %v, want %v", tc.q, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%q allows %v, want %v", tc.q, got, tc.want)
				break
			}
		}
	}
}

func testCert(t *testing.T, ec bool) string {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test idp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	var der []byte
	var err error
	if ec {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		der, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	} else {
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		der, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func ciCode(err error) int {
	var g *googleapi.Error
	if errors.As(err, &g) {
		return g.Code
	}
	return 0
}

func TestSecuritySettingsAndInboundSSO1959(t *testing.T) {
	r := New(store.NewMemory())
	srv := httptest.NewServer(r.IdentityHandler())
	defer srv.Close()
	ctx := context.Background()
	svc, err := ci.NewService(ctx, option.WithEndpoint(srv.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(email string) *Group {
		g, err := r.CreateGroup(GroupInput{Email: email, Parent: CustomerName, Labels: map[string]string{LabelDiscussionForum: ""}})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	g, inner := mk("restricted@"+OrganizationDomain), mk("inner@"+OrganizationDomain)
	if _, _, err := r.CreateMembership(inner.Name(), "outsider@example.com", "", nil); err != nil {
		t.Fatal(err)
	}

	ss, err := svc.Groups.GetSecuritySettings(g.Name() + "/securitySettings").Do()
	if err != nil || ss.Name != g.Name()+"/securitySettings" || ss.MemberRestriction != nil {
		t.Fatalf("getSecuritySettings = %+v, %v", ss, err)
	}
	if _, err := svc.Groups.UpdateSecuritySettings(g.Name()+"/securitySettings",
		&ci.SecuritySettings{MemberRestriction: &ci.MemberRestriction{Query: "member.type == 1"}}).UpdateMask("member_restriction").Do(); ciCode(err) != 400 {
		t.Errorf("update with a bad mask: %v, want 400", err)
	}
	if _, err := svc.Groups.UpdateSecuritySettings(g.Name()+"/securitySettings",
		&ci.SecuritySettings{MemberRestriction: &ci.MemberRestriction{Query: "member.type == 7"}}).UpdateMask("member_restriction.query").Do(); ciCode(err) != 400 {
		t.Errorf("update with a bad query: %v, want 400", err)
	}
	q := "(member.type == 1 || member.type == 3) && member.customer_id == groupCustomerId()"
	op, err := svc.Groups.UpdateSecuritySettings(g.Name()+"/securitySettings",
		&ci.SecuritySettings{MemberRestriction: &ci.MemberRestriction{Query: q}}).UpdateMask("member_restriction.query").Do()
	if err != nil || !op.Done {
		t.Fatalf("updateSecuritySettings = %+v, %v", op, err)
	}
	// A service account is refused; a user of the domain is not.
	_, err = svc.Groups.Memberships.Create(g.Name(), &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: "sa@p.iam.gserviceaccount.com"}}).Do()
	if ciCode(err) != 400 {
		t.Errorf("a service account under a users-and-groups restriction: %v, want 400 FAILED_PRECONDITION", err)
	}
	if _, err := svc.Groups.Memberships.Create(g.Name(), &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: "u@" + OrganizationDomain}}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Groups.Memberships.Create(g.Name(), &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: "u@example.com"}}).Do(); ciCode(err) != 400 {
		t.Errorf("a user of another customer: %v, want 400", err)
	}
	// A nested group is allowed directly; its outside member makes it
	// NON_COMPLIANT, and the group's evaluation too.
	if _, err := svc.Groups.Memberships.Create(g.Name(), &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: inner.Email}}).Do(); err != nil {
		t.Fatal(err)
	}
	ms, err := svc.Groups.Memberships.List(g.Name()).View("FULL").Do()
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, m := range ms.Memberships {
		for _, role := range m.Roles {
			if role.RestrictionEvaluations != nil {
				states[m.PreferredMemberKey.Id] = role.RestrictionEvaluations.MemberRestrictionEvaluation.State
			}
		}
	}
	if states["u@"+OrganizationDomain] != "COMPLIANT" || states[inner.Email] != "NON_COMPLIANT" {
		t.Errorf("restriction evaluations = %v", states)
	}
	ss, err = svc.Groups.GetSecuritySettings(g.Name() + "/securitySettings").ReadMask("member_restriction").Do()
	if err != nil || ss.MemberRestriction.Query != q || ss.MemberRestriction.Evaluation.State != "NON_COMPLIANT" {
		t.Errorf("getSecuritySettings = %+v, %v", ss.MemberRestriction, err)
	}
	// The inner group restricted the same way is FORWARD_COMPLIANT.
	if _, err := r.UpdateSecuritySettings(inner.Name()+"/securitySettings", "member.type == 1 && member.customer_id == groupCustomerId()", []string{"member_restriction.query"}); err != nil {
		t.Fatal(err)
	}
	if s := r.MemberEvaluation(mustGroup(t, r, g.Name()), inner.Email); s != "FORWARD_COMPLIANT" {
		t.Errorf("inner evaluation = %s, want FORWARD_COMPLIANT", s)
	}

	// SAML profile, credentials, assignment.
	if _, err := svc.InboundSamlSsoProfiles.Create(&ci.InboundSamlSsoProfile{Customer: CustomerName,
		IdpConfig: &ci.SamlIdpConfig{EntityId: "idp", SingleSignOnServiceUri: "http://idp.example.com/sso"}}).Do(); ciCode(err) != 400 {
		t.Errorf("an http sign-in URL: %v, want 400", err)
	}
	if _, err := svc.InboundSamlSsoProfiles.Create(&ci.InboundSamlSsoProfile{Customer: "customers/Cother",
		IdpConfig: &ci.SamlIdpConfig{EntityId: "idp", SingleSignOnServiceUri: "https://idp.example.com/sso"}}).Do(); ciCode(err) != 403 {
		t.Errorf("another customer: %v, want 403", err)
	}
	op, err = svc.InboundSamlSsoProfiles.Create(&ci.InboundSamlSsoProfile{Customer: "customers/my_customer", DisplayName: "Test IdP",
		IdpConfig: &ci.SamlIdpConfig{EntityId: "https://idp.example.com", SingleSignOnServiceUri: "https://idp.example.com/sso"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	var prof ci.InboundSamlSsoProfile
	if err := json.Unmarshal(op.Response, &prof); err != nil || prof.Name == "" || prof.Customer != CustomerName || prof.SpConfig.EntityId == "" {
		t.Fatalf("created profile = %+v, %v", prof, err)
	}
	cred := testCert(t, false)
	for i := 0; i < 2; i++ {
		op, err := svc.InboundSamlSsoProfiles.IdpCredentials.Add(prof.Name, &ci.AddIdpCredentialRequest{PemData: cred}).Do()
		if err != nil || !op.Done {
			t.Fatalf("add credential %d: %+v, %v", i, op, err)
		}
	}
	if _, err := svc.InboundSamlSsoProfiles.IdpCredentials.Add(prof.Name, &ci.AddIdpCredentialRequest{PemData: cred}).Do(); ciCode(err) != 400 {
		t.Errorf("a third credential: %v, want 400", err)
	}
	creds, err := svc.InboundSamlSsoProfiles.IdpCredentials.List(prof.Name).Do()
	if err != nil || len(creds.IdpCredentials) != 2 || creds.IdpCredentials[0].RsaKeyInfo.KeySize != 2048 {
		t.Fatalf("list credentials = %+v, %v", creds, err)
	}
	if _, err := svc.InboundSamlSsoProfiles.IdpCredentials.Delete(creds.IdpCredentials[0].Name).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InboundSamlSsoProfiles.IdpCredentials.Add(prof.Name, &ci.AddIdpCredentialRequest{PemData: testCert(t, true)}).Do(); ciCode(err) != 400 {
		t.Errorf("an ECDSA certificate: %v, want 400", err)
	}
	if _, err := svc.InboundSsoAssignments.Create(&ci.InboundSsoAssignment{Customer: CustomerName, TargetGroup: g.Name(), SsoMode: "SAML_SSO",
		SamlSsoInfo: &ci.SamlSsoInfo{InboundSamlSsoProfile: prof.Name}}).Do(); ciCode(err) != 400 {
		t.Errorf("a group assignment of rank 0: %v, want 400", err)
	}
	mkA := func(target string, rank int64) string {
		op, err := svc.InboundSsoAssignments.Create(&ci.InboundSsoAssignment{Customer: CustomerName, TargetGroup: target, Rank: rank, SsoMode: "SAML_SSO",
			SamlSsoInfo: &ci.SamlSsoInfo{InboundSamlSsoProfile: prof.Name}}).Do()
		if err != nil {
			t.Fatal(err)
		}
		var a ci.InboundSsoAssignment
		_ = json.Unmarshal(op.Response, &a)
		return a.Name
	}
	a1 := mkA(g.Name(), 1)
	a2 := mkA(inner.Name(), 1) // goes first, a1 moves to 2
	if a, _ := svc.InboundSsoAssignments.Get(a1).Do(); a == nil || a.Rank != 2 {
		t.Errorf("a1 after a2 took rank 1 = %+v", a)
	}
	if _, err := svc.InboundSsoAssignments.Patch(a1, &ci.InboundSsoAssignment{Rank: 1}).UpdateMask("rank").Do(); err != nil {
		t.Fatal(err)
	}
	if a, _ := svc.InboundSsoAssignments.Get(a2).Do(); a == nil || a.Rank != 2 {
		t.Errorf("a2 after a1 moved to rank 1 = %+v", a)
	}
	if _, err := svc.InboundSamlSsoProfiles.Delete(prof.Name).Do(); ciCode(err) != 400 {
		t.Errorf("deleting a profile in use: %v, want 400", err)
	}
	if _, err := svc.InboundSsoAssignments.Create(&ci.InboundSsoAssignment{Customer: CustomerName, TargetOrgUnit: "orgUnits/03ph8a2z1", SsoMode: "SSO_OFF"}).Do(); err != nil {
		t.Fatal(err)
	}
	all, err := svc.InboundSsoAssignments.List().Filter(`customer=="` + CustomerName + `"`).Do()
	if err != nil || len(all.InboundSsoAssignments) != 3 || all.InboundSsoAssignments[0].Name != a1 {
		for _, x := range all.InboundSsoAssignments {
			t.Logf("%s %s %d", x.Name, x.TargetGroup+x.TargetOrgUnit, x.Rank)
		}
		t.Errorf("list assignments: %v (a1 %s)", err, a1)
	}
	for _, a := range []string{a1, a2} {
		if _, err := svc.InboundSsoAssignments.Delete(a).Do(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.InboundSamlSsoProfiles.Delete(prof.Name).Do(); err != nil {
		t.Fatal(err)
	}

	// OIDC: the client secret is input only.
	op, err = svc.InboundOidcSsoProfiles.Create(&ci.InboundOidcSsoProfile{Customer: CustomerName, DisplayName: "OIDC",
		IdpConfig: &ci.OidcIdpConfig{IssuerUri: "https://issuer.example.com"}, RpConfig: &ci.OidcRpConfig{ClientId: "c", ClientSecret: "s"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	var oidc ci.InboundOidcSsoProfile
	_ = json.Unmarshal(op.Response, &oidc)
	got, err := svc.InboundOidcSsoProfiles.Get(oidc.Name).Do()
	if err != nil || got.RpConfig.ClientId != "c" || got.RpConfig.ClientSecret != "" || len(got.RpConfig.RedirectUris) == 0 {
		t.Errorf("oidc get = %+v, %v", got.RpConfig, err)
	}
}

func mustGroup(t *testing.T, r *Registry, name string) *Group {
	t.Helper()
	g, err := r.GetGroup(name)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
