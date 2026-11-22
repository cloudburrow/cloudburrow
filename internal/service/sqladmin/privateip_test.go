package sqladmin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// TestZonesAndPrivateIP is #2141: locationPreference's zone and
// secondaryZone are the instance's gceZone and secondaryGceZone, checked
// against its region; privateNetwork gives it a PRIVATE address from the
// network's allocated range, refused on a network with none, and
// ipv4Enabled false drops the PRIMARY one; a failover swaps the zones.
func TestZonesAndPrivateIP(t *testing.T) {
	cs, err := compute.New(compute.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Call("POST", "proj", "global/addresses", nil, map[string]any{"name": "google-managed-services-default",
		"addressType": "INTERNAL", "purpose": "VPC_PEERING", "address": "10.20.0.0", "prefixLength": 16.0,
		"network": "global/networks/default"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Call("POST", "proj", "global/networks", nil, map[string]any{"name": "lonely", "autoCreateSubnetworks": true}); err != nil {
		t.Fatal(err)
	}
	prov := newFakeProv()
	srv := NewServer("", newFake())
	srv.EnableInstanceWrites(prov, "")
	srv.SetNetworks(func() Networks { return cs })
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	insert := func(name string, st *sqladmin.Settings) error {
		st.Tier = "db-custom-1-3840"
		op, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", Region: "us-east1",
			Settings: st}).Do()
		if err == nil {
			wait(t, svc, "proj", op)
		}
		return err
	}
	// A zone of another region, and the same zone twice, are refused.
	if err := insert("bad-zone", &sqladmin.Settings{LocationPreference: &sqladmin.LocationPreference{Zone: "us-central1-a"}}); err == nil ||
		!strings.Contains(err.Error(), "400") {
		t.Errorf("a zone outside the region: %v", err)
	}
	if err := insert("same-zone", &sqladmin.Settings{AvailabilityType: "REGIONAL",
		LocationPreference: &sqladmin.LocationPreference{Zone: "us-east1-c", SecondaryZone: "us-east1-c"}}); err == nil {
		t.Error("the same zone twice was accepted")
	}
	if err := insert("ha", &sqladmin.Settings{AvailabilityType: "REGIONAL",
		LocationPreference: &sqladmin.LocationPreference{Zone: "us-east1-c", SecondaryZone: "us-east1-d"},
		IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: false, PrivateNetwork: "projects/proj/global/networks/default",
			ForceSendFields: []string{"Ipv4Enabled"}}}); err != nil {
		t.Fatal(err)
	}
	in, err := svc.Instances.Get("proj", "ha").Do()
	if err != nil {
		t.Fatal(err)
	}
	if in.GceZone != "us-east1-c" || in.SecondaryGceZone != "us-east1-d" {
		t.Errorf("zones %s, %s", in.GceZone, in.SecondaryGceZone)
	}
	if len(in.IpAddresses) != 1 || in.IpAddresses[0].Type != "PRIVATE" || in.IpAddresses[0].IpAddress != "10.20.0.3" {
		t.Errorf("ipAddresses %+v", in.IpAddresses)
	}
	// The next instance on the network takes the next address; an unset
	// zone is the region's first.
	if err := insert("second", &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: true,
		PrivateNetwork: "https://www.googleapis.com/compute/v1/projects/proj/global/networks/default"}}); err != nil {
		t.Fatal(err)
	}
	in, err = svc.Instances.Get("proj", "second").Do()
	if err != nil {
		t.Fatal(err)
	}
	if in.GceZone != "us-east1-b" || len(in.IpAddresses) != 2 || in.IpAddresses[0].Type != "PRIMARY" ||
		in.IpAddresses[1].IpAddress != "10.20.0.4" {
		t.Errorf("second: zone %s, ipAddresses %+v", in.GceZone, in.IpAddresses)
	}
	if eps := srv.PrivateEndpoints(); len(eps) != 2 || eps[0].Network != "projects/proj/global/networks/default" {
		t.Errorf("PrivateEndpoints %+v", eps)
	}
	// Private IP cannot be turned off.
	if _, err := svc.Instances.Patch("proj", "second", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: true, NullFields: []string{"PrivateNetwork"}}}}).Do(); err == nil {
		t.Error("private IP was turned off")
	}
	// A network with no allocated range has no private services access.
	if err := insert("unpeered", &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{
		PrivateNetwork: "projects/proj/global/networks/lonely"}}); err == nil || !strings.Contains(err.Error(), "NETWORK_NOT_PEERED") {
		t.Errorf("a network with no allocated range: %v", err)
	}
	// No public IP, no private IP and no PSC is no connectivity.
	if err := insert("none", &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: false,
		ForceSendFields: []string{"Ipv4Enabled"}}}); err == nil {
		t.Error("an instance with no connectivity was accepted")
	}
	// Private Service Connect answers its service attachment.
	if err := insert("psc", &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: false,
		PscConfig:       &sqladmin.PscConfig{PscEnabled: true, AllowedConsumerProjects: []string{"proj"}},
		ForceSendFields: []string{"Ipv4Enabled"}}}); err != nil {
		t.Fatal(err)
	}
	in, err = svc.Instances.Get("proj", "psc").Do()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(in.PscServiceAttachmentLink, "/regions/us-east1/serviceAttachments/") || len(in.IpAddresses) != 0 {
		t.Errorf("psc: %q %+v", in.PscServiceAttachmentLink, in.IpAddresses)
	}
	// Its attachment resolves for an endpoint of an allowed project only.
	if ep, accepted, ok := srv.PSCAttachment(in.PscServiceAttachmentLink, "proj"); !ok || !accepted || ep.Instance != "psc" {
		t.Errorf("PSCAttachment for proj: %+v %v %v", ep, accepted, ok)
	}
	if _, accepted, ok := srv.PSCAttachment(in.PscServiceAttachmentLink, "other"); !ok || accepted {
		t.Errorf("PSCAttachment for another project: accepted %v ok %v", accepted, ok)
	}
}
