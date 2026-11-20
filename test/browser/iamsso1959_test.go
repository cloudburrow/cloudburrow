//go:build browser

package browser

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
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	ci "google.golang.org/api/cloudidentity/v1"
	"google.golang.org/api/option"
)

// cloudIdentityClient is the official discovery client at the instance's
// Resource Manager endpoint, which serves Cloud Identity.
func cloudIdentityClient(t *testing.T) *ci.Service {
	t.Helper()
	ep := endpoint(t, envResourceManager)
	svc, err := ci.NewService(context.Background(), option.WithEndpoint("http://"+strings.TrimPrefix(ep, "http://")+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func browserGroup(t *testing.T, svc *ci.Service, email string) string {
	t.Helper()
	if _, err := svc.Groups.Create(&ci.Group{Parent: "customers/C0cloudburrow", GroupKey: &ci.EntityKey{Id: email},
		Labels: map[string]string{"cloudidentity.googleapis.com/groups.discussion_forum": ""}}).Do(); err != nil {
		t.Fatalf("groups.create %s: %v", email, err)
	}
	name, err := svc.Groups.Lookup().GroupKeyId(email).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = svc.Groups.Delete(name.Name).Do() })
	return name.Name
}

// TestIAMGroupMemberRestriction1959 (#1959): a group's Edit group sets
// its member restriction, which the official client reads back as the
// group's security settings; the group's page shows it and its state.
func TestIAMGroupMemberRestriction1959(t *testing.T) {
	needService(t, "iam-groups")
	svc := cloudIdentityClient(t)
	project := uniqueProject(t)
	email := fmt.Sprintf("restricted-%s@cloudburrow.local", project[len(project)-6:])
	name := browserGroup(t, svc, email)
	q := "member.type == 1 && member.customer_id == groupCustomerId()"

	p := open(t)
	p.navigate("/iam-admin/groups/" + email + "?project=" + project)
	p.clickText("#view .page-actions button", "Edit group")
	p.waitFor(`document.querySelector(".modal.is-open #f-memberRestriction") !== null`)
	p.setField(".modal.is-open #f-memberRestriction", q)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	ss, err := svc.Groups.GetSecuritySettings(name + "/securitySettings").Do()
	if err != nil || ss.MemberRestriction == nil || ss.MemberRestriction.Query != q {
		t.Fatalf("after Edit group, getSecuritySettings = %+v, %v", ss, err)
	}
	if _, err := svc.Groups.Memberships.Create(name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: "bot@x.iam.gserviceaccount.com"}}).Do(); err == nil {
		t.Error("the restriction set on the page let a service account in")
	}
	p.navigate("/iam-admin/groups/" + email + "?project=" + project)
	p.clickText("#view [role=tab]", "Details")
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q) && document.querySelector("#view").textContent.includes("COMPLIANT")`, q))
}

func browserIdPCertificate(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "browser test IdP"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestIAMSSOProfilesAndAssignments1959 (#1959): SSO profiles' Add SSO
// profile makes a SAML profile, whose Edit profile uploads a locally made
// IdP certificate; SSO profile assignments' Assign SSO profile gives a
// group that profile at priority 1. The official client reads both back.
func TestIAMSSOProfilesAndAssignments1959(t *testing.T) {
	needService(t, "iam-sso-profiles")
	needService(t, "iam-sso-assignments")
	svc := cloudIdentityClient(t)
	project := uniqueProject(t)
	suffix := project[len(project)-6:]
	email := fmt.Sprintf("sso-%s@cloudburrow.local", suffix)
	group := browserGroup(t, svc, email)
	display := "Browser IdP " + suffix

	p := open(t)
	p.navigate("/iam-admin/sso?project=" + project)
	p.clickText("#view button", "Add")
	p.waitFor(`document.querySelector("#view #f-entityId") !== null`)
	p.setField("#view #f-displayName", display)
	p.setField("#view #f-entityId", "https://idp.example.com/"+suffix)
	p.setField("#view #f-ssoUrl", "https://idp.example.com/sso")
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, display))
	list, err := svc.InboundSamlSsoProfiles.List().Do()
	if err != nil {
		t.Fatal(err)
	}
	var prof string
	for _, x := range list.InboundSamlSsoProfiles {
		if x.DisplayName == display {
			prof = x.Name
		}
	}
	if prof == "" {
		t.Fatalf("Add SSO profile made no profile named %q", display)
	}
	t.Cleanup(func() { _, _ = svc.InboundSamlSsoProfiles.Delete(prof).Do() })
	id := strings.TrimPrefix(prof, "inboundSamlSsoProfiles/")

	p.navigate("/iam-admin/sso/" + id + "?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("ACS URL")`)
	p.clickText("#view .page-actions button", "Edit profile")
	p.waitFor(`document.querySelector(".modal.is-open #f-certificate") !== null`)
	p.setField(".modal.is-open #f-certificate", browserIdPCertificate(t))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	creds, err := svc.InboundSamlSsoProfiles.IdpCredentials.List(prof).Do()
	if err != nil || len(creds.IdpCredentials) != 1 || creds.IdpCredentials[0].RsaKeyInfo == nil {
		t.Fatalf("after Upload IdP certificate, idpCredentials.list = %+v, %v", creds, err)
	}

	p.navigate("/iam-admin/sso-assignments?project=" + project)
	p.clickText("#view button", "Assign")
	p.waitFor(`document.querySelector("#view #f-group") !== null`)
	p.setField("#view #f-group", email)
	p.setField("#view #f-profile", id)
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, email))
	as, err := svc.InboundSsoAssignments.List().Do()
	if err != nil {
		t.Fatal(err)
	}
	var got *ci.InboundSsoAssignment
	for _, a := range as.InboundSsoAssignments {
		if a.TargetGroup == group {
			got = a
		}
	}
	if got == nil || got.Rank != 1 || got.SsoMode != "SAML_SSO" || got.SamlSsoInfo == nil || got.SamlSsoInfo.InboundSamlSsoProfile != prof {
		b, _ := json.Marshal(got)
		t.Fatalf("Assign SSO profile made %s", b)
	}
	if _, err := svc.InboundSsoAssignments.Delete(got.Name).Do(); err != nil {
		t.Error(err)
	}
}
