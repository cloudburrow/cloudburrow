//go:build compat

package compat

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	certificatemanager "cloud.google.com/go/certificatemanager/apiv1"
	"cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	iap "cloud.google.com/go/iap/apiv1"
	"cloud.google.com/go/iap/apiv1/iappb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/api/iterator"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Certificate Manager and Identity-Aware Proxy (#2160) are served on the
// Compute Engine port, JSON under /v1/: the official REST clients' options
// point there, never at Google, with no credentials.

func compatSelfSigned(t *testing.T, name string) (string, string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(30 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
}

// TestCertificateManagerThroughTheGoClient (#2160), the official
// certificatemanager/apiv1 REST client: the global location; a DNS
// authorization and its CNAME; a self-managed certificate (its key never
// returned) and a wildcard Google-managed one authorized by DNS, ACTIVE
// with a certificate from the local CA; a load-balancer-authorized one
// PROVISIONING until served; a map with a hostname entry and the PRIMARY
// entry; the documented refusals (a wildcard without DNS authorization, a
// second PRIMARY entry, a regional map, deleting what is in use); a label
// update; the operations; and the deletes.
// covers: google.cloud.certificatemanager.v1.CertificateManager/CreateCertificate, google.cloud.certificatemanager.v1.CertificateManager/GetCertificate, google.cloud.certificatemanager.v1.CertificateManager/ListCertificates, google.cloud.certificatemanager.v1.CertificateManager/UpdateCertificate, google.cloud.certificatemanager.v1.CertificateManager/DeleteCertificate, google.cloud.certificatemanager.v1.CertificateManager/CreateCertificateMap, google.cloud.certificatemanager.v1.CertificateManager/GetCertificateMap, google.cloud.certificatemanager.v1.CertificateManager/ListCertificateMaps, google.cloud.certificatemanager.v1.CertificateManager/UpdateCertificateMap, google.cloud.certificatemanager.v1.CertificateManager/DeleteCertificateMap, google.cloud.certificatemanager.v1.CertificateManager/CreateCertificateMapEntry, google.cloud.certificatemanager.v1.CertificateManager/GetCertificateMapEntry, google.cloud.certificatemanager.v1.CertificateManager/ListCertificateMapEntries, google.cloud.certificatemanager.v1.CertificateManager/UpdateCertificateMapEntry, google.cloud.certificatemanager.v1.CertificateManager/DeleteCertificateMapEntry, google.cloud.certificatemanager.v1.CertificateManager/CreateDnsAuthorization, google.cloud.certificatemanager.v1.CertificateManager/GetDnsAuthorization, google.cloud.certificatemanager.v1.CertificateManager/ListDnsAuthorizations, google.cloud.certificatemanager.v1.CertificateManager/UpdateDnsAuthorization, google.cloud.certificatemanager.v1.CertificateManager/DeleteDnsAuthorization
func TestCertificateManagerThroughTheGoClient(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvCompute) // skips without it
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	c, err := certificatemanager.NewRESTClient(ctx, computeOpts(t, h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := h.Project()
	g := "projects/" + p + "/locations/global"
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e6)
	id := func(s string) string { return "cm-" + s + "-" + sfx }

	loc, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: g})
	if err != nil || loc.GetLocationId() != "global" {
		t.Fatalf("GetLocation: %v %v", loc, err)
	}
	dop, err := c.CreateDnsAuthorization(ctx, &certificatemanagerpb.CreateDnsAuthorizationRequest{Parent: g, DnsAuthorizationId: id("dns"),
		DnsAuthorization: &certificatemanagerpb.DnsAuthorization{Domain: "cm" + sfx + ".example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	da, err := dop.Wait(ctx)
	if err != nil || da.GetDnsResourceRecord().GetType() != "CNAME" || da.GetDnsResourceRecord().GetName() != "_acme-challenge.cm"+sfx+".example.com." ||
		!strings.HasSuffix(da.GetDnsResourceRecord().GetData(), ".authorize.certificatemanager.goog.") || da.GetType() != certificatemanagerpb.DnsAuthorization_FIXED_RECORD {
		t.Fatalf("DNS authorization: %v %v", da, err)
	}
	crt, key := compatSelfSigned(t, "www.cm"+sfx+".example.com")
	op, err := c.CreateCertificate(ctx, &certificatemanagerpb.CreateCertificateRequest{Parent: g, CertificateId: id("self"),
		Certificate: &certificatemanagerpb.Certificate{Type: &certificatemanagerpb.Certificate_SelfManaged{SelfManaged: &certificatemanagerpb.Certificate_SelfManagedCertificate{
			PemCertificate: crt, PemPrivateKey: key}}}})
	if err != nil {
		t.Fatal(err)
	}
	self, err := op.Wait(ctx)
	if err != nil || self.GetPemCertificate() != crt || self.GetSelfManaged().GetPemPrivateKey() != "" || self.GetExpireTime() == nil {
		t.Fatalf("self-managed: %v %v", self, err)
	}
	if _, err := c.CreateCertificate(ctx, &certificatemanagerpb.CreateCertificateRequest{Parent: g, CertificateId: id("bad"),
		Certificate: &certificatemanagerpb.Certificate{Type: &certificatemanagerpb.Certificate_Managed{Managed: &certificatemanagerpb.Certificate_ManagedCertificate{
			Domains: []string{"*.cm" + sfx + ".example.com"}}}}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("a wildcard without DNS authorization: %v", err)
	}
	op, err = c.CreateCertificate(ctx, &certificatemanagerpb.CreateCertificateRequest{Parent: g, CertificateId: id("wild"),
		Certificate: &certificatemanagerpb.Certificate{Type: &certificatemanagerpb.Certificate_Managed{Managed: &certificatemanagerpb.Certificate_ManagedCertificate{
			Domains: []string{"*.cm" + sfx + ".example.com"}, DnsAuthorizations: []string{da.GetName()}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	wild, err := c.GetCertificate(ctx, &certificatemanagerpb.GetCertificateRequest{Name: g + "/certificates/" + id("wild")})
	if err != nil || wild.GetManaged().GetState() != certificatemanagerpb.Certificate_ManagedCertificate_ACTIVE || !strings.Contains(wild.GetPemCertificate(), "BEGIN CERTIFICATE") {
		t.Fatalf("DNS-authorized certificate: %v %v", wild, err)
	}
	op, err = c.CreateCertificate(ctx, &certificatemanagerpb.CreateCertificateRequest{Parent: g, CertificateId: id("lb"),
		Certificate: &certificatemanagerpb.Certificate{Type: &certificatemanagerpb.Certificate_Managed{Managed: &certificatemanagerpb.Certificate_ManagedCertificate{
			Domains: []string{"lb.cm" + sfx + ".example.com"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	lb, err := op.Wait(ctx)
	if err != nil || lb.GetManaged().GetState() != certificatemanagerpb.Certificate_ManagedCertificate_PROVISIONING {
		t.Fatalf("LB-authorized certificate before it is served: %v %v", lb, err)
	}

	mop, err := c.CreateCertificateMap(ctx, &certificatemanagerpb.CreateCertificateMapRequest{Parent: g, CertificateMapId: id("map")})
	if err != nil {
		t.Fatal(err)
	}
	m, err := mop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateCertificateMap(ctx, &certificatemanagerpb.CreateCertificateMapRequest{Parent: "projects/" + p + "/locations/us-central1", CertificateMapId: id("map")}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("a regional map: %v", err)
	}
	entry := func(eid string, e *certificatemanagerpb.CertificateMapEntry) error {
		op, err := c.CreateCertificateMapEntry(ctx, &certificatemanagerpb.CreateCertificateMapEntryRequest{Parent: m.GetName(), CertificateMapEntryId: eid, CertificateMapEntry: e})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	if err := entry("www", &certificatemanagerpb.CertificateMapEntry{Match: &certificatemanagerpb.CertificateMapEntry_Hostname{Hostname: "www.cm" + sfx + ".example.com"},
		Certificates: []string{self.GetName()}}); err != nil {
		t.Fatal(err)
	}
	if err := entry("primary", &certificatemanagerpb.CertificateMapEntry{Match: &certificatemanagerpb.CertificateMapEntry_Matcher_{Matcher: certificatemanagerpb.CertificateMapEntry_PRIMARY},
		Certificates: []string{wild.GetName()}}); err != nil {
		t.Fatal(err)
	}
	if err := entry("primary2", &certificatemanagerpb.CertificateMapEntry{Match: &certificatemanagerpb.CertificateMapEntry_Matcher_{Matcher: certificatemanagerpb.CertificateMapEntry_PRIMARY}}); computeCode(err) != http.StatusConflict {
		t.Errorf("a second PRIMARY entry: %v", err)
	}
	it := c.ListCertificateMapEntries(ctx, &certificatemanagerpb.ListCertificateMapEntriesRequest{Parent: m.GetName()})
	n := 0
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if e.GetState() != certificatemanagerpb.ServingState_ACTIVE {
			t.Errorf("entry state: %v", e)
		}
		n++
	}
	if n != 2 {
		t.Errorf("entries: %d", n)
	}
	got, err := c.GetCertificate(ctx, &certificatemanagerpb.GetCertificateRequest{Name: self.GetName()})
	if err != nil || len(got.GetUsedBy()) != 1 {
		t.Errorf("usedBy: %v %v", got, err)
	}
	if _, err := c.DeleteCertificate(ctx, &certificatemanagerpb.DeleteCertificateRequest{Name: self.GetName()}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("deleting a certificate in use: %v", err)
	}
	if _, err := c.DeleteCertificateMap(ctx, &certificatemanagerpb.DeleteCertificateMapRequest{Name: m.GetName()}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("deleting a map with entries: %v", err)
	}
	uop, err := c.UpdateCertificateMap(ctx, &certificatemanagerpb.UpdateCertificateMapRequest{CertificateMap: &certificatemanagerpb.CertificateMap{Name: m.GetName(),
		Labels: map[string]string{"env": "test"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil {
		t.Fatal(err)
	}
	if um, err := uop.Wait(ctx); err != nil || um.GetLabels()["env"] != "test" {
		t.Errorf("update: %v %v", um, err)
	}
	ops := c.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: g})
	if _, err := ops.Next(); err != nil {
		t.Errorf("ListOperations: %v", err)
	}
	certs := c.ListCertificates(ctx, &certificatemanagerpb.ListCertificatesRequest{Parent: g, Filter: `name:"` + sfx + `"`})
	k := 0
	for {
		_, err := certs.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		k++
	}
	if k != 3 {
		t.Errorf("listed certificates: %d", k)
	}
	// Deletes, in the order Google needs them.
	for _, e := range []string{"www", "primary"} {
		op, err := c.DeleteCertificateMapEntry(ctx, &certificatemanagerpb.DeleteCertificateMapEntryRequest{Name: m.GetName() + "/certificateMapEntries/" + e})
		if err == nil {
			err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatalf("delete entry %s: %v", e, err)
		}
	}
	if op, err := c.DeleteCertificateMap(ctx, &certificatemanagerpb.DeleteCertificateMapRequest{Name: m.GetName()}); err != nil || op.Wait(ctx) != nil {
		t.Fatalf("delete map: %v", err)
	}
	for _, n := range []string{self.GetName(), wild.GetName(), lb.GetName()} {
		if op, err := c.DeleteCertificate(ctx, &certificatemanagerpb.DeleteCertificateRequest{Name: n}); err != nil || op.Wait(ctx) != nil {
			t.Fatalf("delete %s: %v", n, err)
		}
	}
	upd, err := c.UpdateDnsAuthorization(ctx, &certificatemanagerpb.UpdateDnsAuthorizationRequest{DnsAuthorization: &certificatemanagerpb.DnsAuthorization{
		Name: da.GetName(), Description: "d"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upd.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetDnsAuthorization(ctx, &certificatemanagerpb.GetDnsAuthorizationRequest{Name: da.GetName()}); err != nil || got.GetDescription() != "d" {
		t.Errorf("updated DNS authorization: %v %v", got, err)
	}
	if l := c.ListDnsAuthorizations(ctx, &certificatemanagerpb.ListDnsAuthorizationsRequest{Parent: g}); l == nil {
		t.Error("ListDnsAuthorizations")
	} else if _, err := l.Next(); err != nil {
		t.Errorf("ListDnsAuthorizations: %v", err)
	}
	if l := c.ListCertificateMaps(ctx, &certificatemanagerpb.ListCertificateMapsRequest{Parent: g}); l != nil {
		if _, err := l.Next(); err != nil && err != iterator.Done {
			t.Errorf("ListCertificateMaps: %v", err)
		}
	}
	if _, err := c.GetCertificateMap(ctx, &certificatemanagerpb.GetCertificateMapRequest{Name: m.GetName()}); computeCode(err) != http.StatusNotFound {
		t.Errorf("deleted map: %v", err)
	}
	if _, err := c.GetCertificateMapEntry(ctx, &certificatemanagerpb.GetCertificateMapEntryRequest{Name: m.GetName() + "/certificateMapEntries/www"}); computeCode(err) != http.StatusNotFound {
		t.Errorf("deleted entry: %v", err)
	}
	if op, err := c.DeleteDnsAuthorization(ctx, &certificatemanagerpb.DeleteDnsAuthorizationRequest{Name: da.GetName()}); err != nil || op.Wait(ctx) != nil {
		t.Fatalf("delete DNS authorization: %v", err)
	}
}

// TestIdentityAwareProxyThroughTheGoClient (#2160), the official iap/apiv1
// REST clients: the IAM policy of a project's IAP web resources
// (roles/iap.httpsResourceAccessor) and testIamPermissions; IAP settings
// set at the top level and read merged-free per level, with an update
// mask; the refusal of a backend service's resource that does not exist;
// attribute-expression validation; a TCP destination group's lifecycle;
// and the deprecated OAuth Admin API (no brands, creation refused).
// covers: google.cloud.iap.v1.IdentityAwareProxyAdminService/SetIamPolicy, google.cloud.iap.v1.IdentityAwareProxyAdminService/GetIamPolicy, google.cloud.iap.v1.IdentityAwareProxyAdminService/TestIamPermissions, google.cloud.iap.v1.IdentityAwareProxyAdminService/GetIapSettings, google.cloud.iap.v1.IdentityAwareProxyAdminService/UpdateIapSettings, google.cloud.iap.v1.IdentityAwareProxyAdminService/ValidateIapAttributeExpression, google.cloud.iap.v1.IdentityAwareProxyAdminService/CreateTunnelDestGroup, google.cloud.iap.v1.IdentityAwareProxyAdminService/GetTunnelDestGroup, google.cloud.iap.v1.IdentityAwareProxyAdminService/ListTunnelDestGroups, google.cloud.iap.v1.IdentityAwareProxyAdminService/UpdateTunnelDestGroup, google.cloud.iap.v1.IdentityAwareProxyAdminService/DeleteTunnelDestGroup, google.cloud.iap.v1.IdentityAwareProxyOAuthService/ListBrands, google.cloud.iap.v1.IdentityAwareProxyOAuthService/CreateBrand
func TestIdentityAwareProxyThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without Compute Engine
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	admin, err := iap.NewIdentityAwareProxyAdminRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	oauth, err := iap.NewIdentityAwareProxyOAuthRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = oauth.Close() })
	p := h.Project()
	web := "projects/" + p + "/iap_web/compute"
	member := "user:iap-" + fmt.Sprint(time.Now().UnixNano()%1e6) + "@example.com"
	pol, err := admin.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: web, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/iap.httpsResourceAccessor", Members: []string{member}}}}})
	if err != nil || len(pol.GetBindings()) != 1 {
		t.Fatalf("SetIamPolicy: %v %v", pol, err)
	}
	t.Cleanup(func() {
		_, _ = admin.SetIamPolicy(context.Background(), &iampb.SetIamPolicyRequest{Resource: web, Policy: &iampb.Policy{}})
	})
	got, err := admin.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: web})
	if err != nil || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != member {
		t.Errorf("GetIamPolicy: %v %v", got, err)
	}
	tp, err := admin.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: web, Permissions: []string{"iap.webServiceVersions.accessViaIAP"}})
	if err != nil || len(tp.GetPermissions()) != 1 {
		t.Errorf("TestIamPermissions: %v %v", tp, err)
	}
	if _, err := admin.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: web + "/services/no-such-backend-service"}); computeCode(err) != http.StatusNotFound {
		t.Errorf("a missing backend service: %v", err)
	}
	st, err := admin.UpdateIapSettings(ctx, &iappb.UpdateIapSettingsRequest{IapSettings: &iappb.IapSettings{Name: web,
		AccessSettings: &iappb.AccessSettings{CorsSettings: &iappb.CorsSettings{AllowHttpOptions: wrapperspb.Bool(true)},
			OauthSettings: &iappb.OAuthSettings{LoginHint: wrapperspb.String("ignored@example.com")}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"access_settings.cors_settings.allow_http_options"}}})
	if err != nil || !st.GetAccessSettings().GetCorsSettings().GetAllowHttpOptions().GetValue() || st.GetAccessSettings().GetOauthSettings() != nil {
		t.Fatalf("UpdateIapSettings: %v %v", st, err)
	}
	t.Cleanup(func() {
		_, _ = admin.UpdateIapSettings(context.Background(), &iappb.UpdateIapSettingsRequest{IapSettings: &iappb.IapSettings{Name: web}})
	})
	if gs, err := admin.GetIapSettings(ctx, &iappb.GetIapSettingsRequest{Name: web}); err != nil || !gs.GetAccessSettings().GetCorsSettings().GetAllowHttpOptions().GetValue() {
		t.Errorf("GetIapSettings: %v %v", gs, err)
	}
	if _, err := admin.ValidateIapAttributeExpression(ctx, &iappb.ValidateIapAttributeExpressionRequest{Name: web,
		Expression: `attributes.saml_attributes.filter(attribute, attribute.name in ["test1", "test2"])`}); err != nil {
		t.Errorf("a valid expression: %v", err)
	}
	if _, err := admin.ValidateIapAttributeExpression(ctx, &iappb.ValidateIapAttributeExpressionRequest{Name: web, Expression: "attributes.saml_attributes.filter("}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("an invalid expression: %v", err)
	}
	parent := "projects/" + p + "/iap_tunnel/locations/us-central1"
	gid := "dg-" + fmt.Sprint(time.Now().UnixNano()%1e6)
	g, err := admin.CreateTunnelDestGroup(ctx, &iappb.CreateTunnelDestGroupRequest{Parent: parent, TunnelDestGroupId: gid,
		TunnelDestGroup: &iappb.TunnelDestGroup{Cidrs: []string{"10.1.0.0/24"}}})
	if err != nil || g.GetName() != parent+"/destGroups/"+gid {
		t.Fatalf("CreateTunnelDestGroup: %v %v", g, err)
	}
	if _, err := admin.UpdateTunnelDestGroup(ctx, &iappb.UpdateTunnelDestGroupRequest{TunnelDestGroup: &iappb.TunnelDestGroup{Name: g.GetName(),
		Fqdns: []string{"db.example.com"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"fqdns"}}}); err != nil {
		t.Fatal(err)
	}
	if gg, err := admin.GetTunnelDestGroup(ctx, &iappb.GetTunnelDestGroupRequest{Name: g.GetName()}); err != nil || len(gg.GetFqdns()) != 1 || len(gg.GetCidrs()) != 1 {
		t.Errorf("GetTunnelDestGroup: %v %v", gg, err)
	}
	if _, err := admin.ListTunnelDestGroups(ctx, &iappb.ListTunnelDestGroupsRequest{Parent: parent}).Next(); err != nil {
		t.Errorf("ListTunnelDestGroups: %v", err)
	}
	if err := admin.DeleteTunnelDestGroup(ctx, &iappb.DeleteTunnelDestGroupRequest{Name: g.GetName()}); err != nil {
		t.Fatal(err)
	}
	if bl, err := oauth.ListBrands(ctx, &iappb.ListBrandsRequest{Parent: "projects/" + p}); err != nil || len(bl.GetBrands()) != 0 {
		t.Errorf("ListBrands: %v %v", bl, err)
	}
	if _, err := oauth.CreateBrand(ctx, &iappb.CreateBrandRequest{Parent: "projects/" + p, Brand: &iappb.Brand{SupportEmail: "a@example.com", ApplicationTitle: "x"}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("CreateBrand (deprecated): %v", err)
	}
}
