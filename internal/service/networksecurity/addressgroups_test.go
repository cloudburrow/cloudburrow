package networksecurity

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	nsclient "cloud.google.com/go/networksecurity/apiv1"
	nspb "cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestAddressGroupsThroughTheRESTClients: the official AddressGroup and
// OrganizationAddressGroup REST clients against the JSON API: the
// documented checks, the item calls, references, and the state restored.
func TestAddressGroupsThroughTheRESTClients(t *testing.T) {
	state := filepath.Join(t.TempDir(), "ns.json")
	s, _ := New(Options{State: state})
	var refs []AddressGroupReference
	s.UseHooks(Hooks{AddressGroupReferences: func(string) []AddressGroupReference { return refs }})
	srv := httptest.NewServer(NewRESTHandler(s))
	defer srv.Close()
	ctx := context.Background()
	opts := []option.ClientOption{option.WithEndpoint(srv.URL), option.WithoutAuthentication()}
	c, err := nsclient.NewAddressGroupRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	armor := []nspb.AddressGroup_Purpose{nspb.AddressGroup_CLOUD_ARMOR}
	op, err := c.CreateAddressGroup(ctx, &nspb.CreateAddressGroupRequest{Parent: g, AddressGroupId: "bad",
		AddressGroup: &nspb.AddressGroup{Type: nspb.AddressGroup_IPV4, Capacity: 3, Purpose: armor, Items: []string{"10.0.0.0/8", "192.0.2.1"}}})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := op.Wait(ctx)
	if err != nil || ag.GetName() != g+"/addressGroups/bad" || ag.GetSelfLink() == "" || !slices.Equal(ag.GetItems(), []string{"10.0.0.0/8", "192.0.2.1"}) {
		t.Fatalf("create: %v %v", ag, err)
	}
	for name, b := range map[string]*nspb.AddressGroup{
		"no type":             {Capacity: 1},
		"no capacity":         {Type: nspb.AddressGroup_IPV4},
		"IPv6 in IPv4":        {Type: nspb.AddressGroup_IPV4, Capacity: 2, Items: []string{"2001:db8::/32"}},
		"host bits":           {Type: nspb.AddressGroup_IPV4, Capacity: 2, Items: []string{"10.0.0.1/8"}},
		"over capacity":       {Type: nspb.AddressGroup_IPV4, Capacity: 1, Items: []string{"10.0.0.1", "10.0.0.2"}},
		"default over 1,000":  {Type: nspb.AddressGroup_IPV4, Capacity: 1001},
		"armor over 150,000":  {Type: nspb.AddressGroup_IPV4, Capacity: 150001, Purpose: armor},
		"armor IPv6 > 50,000": {Type: nspb.AddressGroup_IPV6, Capacity: 50001, Purpose: armor},
	} {
		if _, err := c.CreateAddressGroup(ctx, &nspb.CreateAddressGroupRequest{Parent: g, AddressGroupId: "x", AddressGroup: b}); code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	aop, err := c.AddAddressGroupItems(ctx, &nspb.AddAddressGroupItemsRequest{AddressGroup: ag.GetName(), Items: []string{"192.0.2.1", "127.0.0.1/32"}})
	if err != nil {
		t.Fatal(err)
	}
	if ag, err = aop.Wait(ctx); err != nil || len(ag.GetItems()) != 3 {
		t.Fatalf("add: %v %v", ag, err)
	}
	if _, err := c.AddAddressGroupItems(ctx, &nspb.AddAddressGroupItemsRequest{AddressGroup: ag.GetName(), Items: []string{"198.51.100.0/24"}}); code(err) != codes.InvalidArgument {
		t.Errorf("add over capacity: %v", err)
	}
	rop, err := c.RemoveAddressGroupItems(ctx, &nspb.RemoveAddressGroupItemsRequest{AddressGroup: ag.GetName(), Items: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	if ag, err = rop.Wait(ctx); err != nil || len(ag.GetItems()) != 2 {
		t.Fatalf("remove: %v %v", ag, err)
	}
	if _, err := c.RemoveAddressGroupItems(ctx, &nspb.RemoveAddressGroupItemsRequest{AddressGroup: ag.GetName(), Items: []string{"10.0.0.0/8"}}); code(err) != codes.InvalidArgument {
		t.Errorf("remove a missing item: %v", err)
	}
	if got, ok := s.AddressGroupItems(ag.GetName()); !ok || len(got) != 2 {
		t.Errorf("AddressGroupItems = %v %v", got, ok)
	}
	// Clone into a second group; a capacity change is refused.
	op, err = c.CreateAddressGroup(ctx, &nspb.CreateAddressGroupRequest{Parent: g, AddressGroupId: "copy",
		AddressGroup: &nspb.AddressGroup{Type: nspb.AddressGroup_IPV4, Capacity: 10}})
	if err != nil {
		t.Fatal(err)
	}
	cp, _ := op.Wait(ctx)
	clop, err := c.CloneAddressGroupItems(ctx, &nspb.CloneAddressGroupItemsRequest{AddressGroup: cp.GetName(), SourceAddressGroup: ag.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if cp, err = clop.Wait(ctx); err != nil || len(cp.GetItems()) != 2 {
		t.Fatalf("clone: %v %v", cp, err)
	}
	if _, ok := s.AddressGroupItems(cp.GetName()); ok {
		t.Error("a DEFAULT group was given to Cloud Armor")
	}
	if _, err := c.UpdateAddressGroup(ctx, &nspb.UpdateAddressGroupRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"capacity"}},
		AddressGroup: &nspb.AddressGroup{Name: cp.GetName(), Capacity: 20}}); code(err) != codes.InvalidArgument {
		t.Errorf("capacity change: %v", err)
	}
	uop, err := c.UpdateAddressGroup(ctx, &nspb.UpdateAddressGroupRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
		AddressGroup: &nspb.AddressGroup{Name: cp.GetName(), Description: "copy"}})
	if err != nil {
		t.Fatal(err)
	}
	if cp, err = uop.Wait(ctx); err != nil || cp.GetDescription() != "copy" || len(cp.GetItems()) != 2 {
		t.Fatalf("update: %v %v", cp, err)
	}
	n := 0
	for it := c.ListAddressGroups(ctx, &nspb.ListAddressGroupsRequest{Parent: g}); ; n++ {
		if _, err := it.Next(); err == iterator.Done {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if n != 2 {
		t.Errorf("listed %d groups", n)
	}
	// References, and a referenced group cannot be deleted.
	refs = []AddressGroupReference{{SecurityPolicy: "https://www.googleapis.com/compute/v1/projects/p/global/securityPolicies/sp", RulePriority: 100}}
	it := c.ListAddressGroupReferences(ctx, &nspb.ListAddressGroupReferencesRequest{AddressGroup: ag.GetName()})
	r, err := it.Next()
	if err != nil || r.GetRulePriority() != 100 || r.GetSecurityPolicy() != refs[0].SecurityPolicy {
		t.Fatalf("references: %v %v", r, err)
	}
	if _, err := c.DeleteAddressGroup(ctx, &nspb.DeleteAddressGroupRequest{Name: ag.GetName()}); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("delete in use: %v", err)
	}
	refs = nil
	dop, err := c.DeleteAddressGroup(ctx, &nspb.DeleteAddressGroupRequest{Name: ag.GetName()})
	if err != nil || dop.Wait(ctx) != nil {
		t.Fatalf("delete: %v", err)
	}

	// The organization's address groups.
	oc, err := nsclient.NewOrganizationAddressGroupRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer oc.Close()
	org := "organizations/123/locations/global"
	oop, err := oc.CreateAddressGroup(ctx, &nspb.CreateAddressGroupRequest{Parent: org, AddressGroupId: "org-bad",
		AddressGroup: &nspb.AddressGroup{Type: nspb.AddressGroup_IPV6, Capacity: 5, Purpose: armor, Items: []string{"2001:db8::/32"}}})
	if err != nil {
		t.Fatal(err)
	}
	og, err := oop.Wait(ctx)
	if err != nil || og.GetName() != org+"/addressGroups/org-bad" {
		t.Fatalf("organization create: %v %v", og, err)
	}
	if got, err := oc.GetAddressGroup(ctx, &nspb.GetAddressGroupRequest{Name: og.GetName()}); err != nil || got.GetType() != nspb.AddressGroup_IPV6 {
		t.Fatalf("organization get: %v %v", got, err)
	}
	if !s.Handles(httptest.NewRequest("GET", "/v1/"+org+"/addressGroups", nil)) {
		t.Error("Handles: organization address groups")
	}
	re, _ := New(Options{State: state})
	if _, ok := re.AddressGroupItems(og.GetName()); !ok {
		t.Error("state not restored")
	}
}

func TestBackendAuthenticationConfigChecks(t *testing.T) {
	s, _ := New(Options{})
	tc, cert, server := g+"/trustConfigs/tc", g+"/certificates/client", g+"/certificates/server"
	s.UseHooks(Hooks{TrustConfigExists: func(n string) bool { return n == tc },
		CertificateScope: func(n string) (string, bool) {
			switch n {
			case cert:
				return "CLIENT_AUTH", true
			case server:
				return "DEFAULT", true
			}
			return "", false
		}})
	srv := httptest.NewServer(NewRESTHandler(s))
	defer srv.Close()
	ctx := context.Background()
	c, err := nsclient.NewRESTClient(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	op, err := c.CreateBackendAuthenticationConfig(ctx, &nspb.CreateBackendAuthenticationConfigRequest{Parent: g, BackendAuthenticationConfigId: "bac",
		BackendAuthenticationConfig: &nspb.BackendAuthenticationConfig{TrustConfig: tc, ClientCertificate: cert}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := op.Wait(ctx)
	if err != nil || b.GetEtag() == "" || b.GetCreateTime() == nil {
		t.Fatalf("create: %v %v", b, err)
	}
	for name, x := range map[string]*nspb.BackendAuthenticationConfig{
		"no trust config":         {},
		"missing trust config":    {TrustConfig: g + "/trustConfigs/nope"},
		"regional trust config":   {TrustConfig: "projects/p/locations/us-east1/trustConfigs/tc"},
		"server certificate":      {TrustConfig: tc, ClientCertificate: server},
		"missing certificate":     {TrustConfig: tc, ClientCertificate: g + "/certificates/nope"},
		"malformed certificate":   {TrustConfig: tc, ClientCertificate: "client"},
		"another location's cert": {TrustConfig: tc, ClientCertificate: "projects/p/locations/us-east1/certificates/client"},
	} {
		if _, err := c.CreateBackendAuthenticationConfig(ctx, &nspb.CreateBackendAuthenticationConfigRequest{Parent: g,
			BackendAuthenticationConfigId: "x", BackendAuthenticationConfig: x}); code(err) != codes.InvalidArgument && code(err) != codes.NotFound {
			t.Errorf("%s: %v", name, err)
		}
	}
	if op, err := c.CreateBackendAuthenticationConfig(ctx, &nspb.CreateBackendAuthenticationConfigRequest{Parent: g, BackendAuthenticationConfigId: "public",
		BackendAuthenticationConfig: &nspb.BackendAuthenticationConfig{WellKnownRoots: nspb.BackendAuthenticationConfig_PUBLIC_ROOTS}}); err != nil {
		t.Errorf("PUBLIC_ROOTS without a trust config: %v", err)
	} else if _, err := op.Wait(ctx); err != nil {
		t.Errorf("PUBLIC_ROOTS without a trust config: %v", err)
	}
	if _, err := c.UpdateBackendAuthenticationConfig(ctx, &nspb.UpdateBackendAuthenticationConfigRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"client_certificate"}},
		BackendAuthenticationConfig: &nspb.BackendAuthenticationConfig{Name: b.GetName()}}); code(err) != codes.InvalidArgument {
		t.Errorf("changing the client certificate: %v", err)
	}
	uop, err := c.UpdateBackendAuthenticationConfig(ctx, &nspb.UpdateBackendAuthenticationConfigRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
		BackendAuthenticationConfig: &nspb.BackendAuthenticationConfig{Name: b.GetName(), Description: "d"}})
	if err != nil {
		t.Fatal(err)
	}
	if u, err := uop.Wait(ctx); err != nil || u.GetDescription() != "d" || u.GetClientCertificate() != cert || u.GetEtag() == b.GetEtag() {
		t.Fatalf("update: %v %v", u, err)
	}
	if got := s.TrustConfigUsers(tc); len(got) != 1 || got[0] != b.GetName() {
		t.Errorf("TrustConfigUsers = %v", got)
	}
	if got := s.CertificateUsers(cert); len(got) != 1 {
		t.Errorf("CertificateUsers = %v", got)
	}
	if _, err := c.DeleteBackendAuthenticationConfig(ctx, &nspb.DeleteBackendAuthenticationConfigRequest{Name: b.GetName(), Etag: "stale"}); code(err) != codes.Aborted {
		t.Errorf("delete with a stale etag: %v", err)
	}
	n := 0
	for it := c.ListBackendAuthenticationConfigs(ctx, &nspb.ListBackendAuthenticationConfigsRequest{Parent: g}); ; n++ {
		if _, err := it.Next(); err == iterator.Done {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if n != 2 {
		t.Errorf("listed %d configs", n)
	}
	dop, err := c.DeleteBackendAuthenticationConfig(ctx, &nspb.DeleteBackendAuthenticationConfigRequest{Name: b.GetName()})
	if err != nil || dop.Wait(ctx) != nil {
		t.Fatalf("delete: %v", err)
	}
}
