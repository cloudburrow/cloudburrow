package run

import (
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
)

type fakeVPC struct{}

func (fakeVPC) ConnectorReady(name string) error {
	if strings.HasSuffix(name, "/connectors/ready") {
		return nil
	}
	return errors.New("connector " + name + " was not found")
}

func (fakeVPC) HasNetwork(ref, project string) bool { return ref == "default" || ref == "blue" }

func (fakeVPC) Subnetwork(ref, project, region string) (string, bool) {
	if region == "us-central1" && (ref == "default" || ref == "blue-sub") {
		net := "default"
		if ref == "blue-sub" {
			net = "blue"
		}
		return "projects/" + project + "/global/networks/" + net, true
	}
	return "", false
}

// TestCheckVPC (#2140): a connector must exist, be READY and be in the
// service's region, given bare or in full; Direct VPC egress needs its
// network and subnetwork, the subnet defaulting to the network's name and
// the network to the subnet's; the two kinds never together.
func TestCheckVPC(t *testing.T) {
	s := (&Server{}).WithVPC(fakeVPC{})
	const svc = "projects/p/locations/us-central1/services/s"
	ni := func(network, sub string) []*runpb.VpcAccess_NetworkInterface {
		return []*runpb.VpcAccess_NetworkInterface{{Network: network, Subnetwork: sub}}
	}
	v := &runpb.VpcAccess{Connector: "ready", Egress: runpb.VpcAccess_ALL_TRAFFIC}
	if err := s.checkVPC(svc, v); err != nil || v.GetConnector() != "projects/p/locations/us-central1/connectors/ready" {
		t.Fatalf("bare connector: %v, %v", err, v)
	}
	d := &runpb.VpcAccess{NetworkInterfaces: ni("", "blue-sub")}
	if err := s.checkVPC(svc, d); err != nil || d.GetNetworkInterfaces()[0].GetNetwork() != "blue" {
		t.Fatalf("subnet only: %v, %v", err, d)
	}
	if err := s.checkVPC(svc, &runpb.VpcAccess{NetworkInterfaces: ni("default", "")}); err != nil {
		t.Fatalf("network only: %v", err)
	}
	for _, tc := range []struct {
		v    *runpb.VpcAccess
		want string
	}{
		{&runpb.VpcAccess{Connector: "missing"}, "was not found"},
		{&runpb.VpcAccess{Connector: "projects/p/locations/europe-west1/connectors/ready"}, "same region"},
		{&runpb.VpcAccess{Connector: "projects/p/connectors/ready"}, "must be projects/"},
		{&runpb.VpcAccess{Connector: "ready", NetworkInterfaces: ni("default", "")}, "cannot both be set"},
		{&runpb.VpcAccess{NetworkInterfaces: append(ni("default", ""), ni("default", "")...)}, "at most one"},
		{&runpb.VpcAccess{NetworkInterfaces: ni("nope", "")}, "network \"nope\" was not found"},
		{&runpb.VpcAccess{NetworkInterfaces: ni("blue", "")}, "subnetwork \"blue\" was not found"},
		{&runpb.VpcAccess{NetworkInterfaces: ni("default", "blue-sub")}, "is not in network"},
		{&runpb.VpcAccess{NetworkInterfaces: ni("", "")}, "network or subnetwork must be set"},
	} {
		if err := s.checkVPC(svc, tc.v); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("checkVPC(%v) = %v, want %q", tc.v, err, tc.want)
		}
	}
	// Without Compute Engine, a connector is recorded unchecked.
	if err := (&Server{}).checkVPC(svc, &runpb.VpcAccess{Connector: "anything"}); err != nil {
		t.Errorf("without a VPC: %v", err)
	}
}

// TestVPCEgressAnnotation (#2140): a template with a connector or Direct
// VPC egress puts its resource's name and vpcAccess on its pods; one
// without puts nothing.
func TestVPCEgressAnnotation(t *testing.T) {
	if got := vpcEgressAnnotation("projects/p/locations/r/services/s", nil); got != "" {
		t.Fatalf("no vpcAccess: %q", got)
	}
	got := vpcEgressAnnotation("projects/p/locations/r/services/s", &runpb.VpcAccess{Connector: "projects/p/locations/r/connectors/c",
		Egress: runpb.VpcAccess_ALL_TRAFFIC})
	if !strings.Contains(got, `"name":"projects/p/locations/r/services/s"`) || !strings.Contains(got, `"connector":"projects/p/locations/r/connectors/c"`) ||
		!strings.Contains(got, `"egress":"ALL_TRAFFIC"`) {
		t.Fatalf("annotation = %s", got)
	}
}
