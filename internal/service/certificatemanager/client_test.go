package certificatemanager

import (
	"context"
	"net/http/httptest"
	"testing"

	cmclient "cloud.google.com/go/certificatemanager/apiv1"
	cmpb "cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	"google.golang.org/api/option"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
)

// TestOfficialRESTClient: the official certificatemanager/apiv1 REST
// client against the JSON API, its operations' waits included.
func TestOfficialRESTClient(t *testing.T) {
	s, _ := New(Options{})
	s.UseIssuer(&localCA{})
	srv := httptest.NewServer(NewRESTHandler(s))
	defer srv.Close()
	ctx := context.Background()
	c, err := cmclient.NewRESTClient(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	op, err := c.CreateDnsAuthorization(ctx, &cmpb.CreateDnsAuthorizationRequest{Parent: g, DnsAuthorizationId: "d",
		DnsAuthorization: &cmpb.DnsAuthorization{Domain: "example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := op.Wait(ctx)
	if err != nil || a.GetDnsResourceRecord().GetType() != "CNAME" {
		t.Fatalf("wait: %v %v", a, err)
	}
	cop, err := c.CreateCertificate(ctx, &cmpb.CreateCertificateRequest{Parent: g, CertificateId: "w",
		Certificate: &cmpb.Certificate{Type: &cmpb.Certificate_Managed{Managed: &cmpb.Certificate_ManagedCertificate{
			Domains: []string{"*.example.com"}, DnsAuthorizations: []string{a.GetName()}}}}})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := cop.Wait(ctx)
	if err != nil || cert.GetManaged().GetState() != cmpb.Certificate_ManagedCertificate_ACTIVE {
		t.Fatalf("certificate: %v %v", cert, err)
	}
	dop, err := c.DeleteCertificate(ctx, &cmpb.DeleteCertificateRequest{Name: cert.GetName()})
	if err != nil || dop.Wait(ctx) != nil {
		t.Fatalf("delete: %v", err)
	}
	if l, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: g}); err != nil || l.GetLocationId() != "global" {
		t.Errorf("GetLocation: %v %v", l, err)
	}
}
