package networkconnectivity

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	ncpb "cloud.google.com/go/networkconnectivity/apiv1/networkconnectivitypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// fakeCompute has one network, net1 in project p, with subnet s1 of two
// free addresses in us-central1.
type fakeCompute struct {
	made    map[string]string
	free    int
	deleted []string
}

func (f *fakeCompute) HasNetwork(ref, _ string) bool { return ref == "projects/p/global/networks/net1" }

func (f *fakeCompute) Subnetwork(ref, _, _ string) (string, bool) {
	if ref == "projects/p/regions/us-central1/subnetworks/s1" {
		return "projects/p/global/networks/net1", true
	}
	return "", false
}

func (f *fakeCompute) CreateAutoEndpoint(e Endpoint) (EndpointResult, error) {
	rel := "projects/" + e.Project + "/regions/" + e.Region + "/forwardingRules/" + e.Name
	if ip, ok := f.made[rel]; ok {
		return EndpointResult{ForwardingRule: rel, IP: ip}, nil
	}
	if f.free == 0 || len(e.Subnetworks) == 0 {
		return EndpointResult{}, ErrNoAddress
	}
	f.free--
	ip := fmt.Sprintf("10.0.0.%d", 10+len(f.made))
	f.made[rel] = ip
	return EndpointResult{ForwardingRule: rel, IP: ip, Subnetwork: e.Subnetworks[0], PSCConnectionID: "1"}, nil
}

func (f *fakeCompute) DeleteAutoEndpoint(fr string) error {
	if _, ok := f.made[fr]; ok {
		f.free++
	}
	delete(f.made, fr)
	f.deleted = append(f.deleted, fr)
	return nil
}

func newTest(t *testing.T, state string) (*Server, *fakeCompute) {
	t.Helper()
	fc := &fakeCompute{made: map[string]string{}, free: 2}
	s, err := NewServer(Options{State: state, Compute: func() Compute { return fc }})
	if err != nil {
		t.Fatal(err)
	}
	return s, fc
}

const parentP = "projects/p/locations/us-central1"

func policy(limit *int64) *ncpb.ServiceConnectionPolicy {
	return &ncpb.ServiceConnectionPolicy{Network: "projects/p/global/networks/net1", ServiceClass: CloudSQLServiceClass,
		PscConfig: &ncpb.ServiceConnectionPolicy_PscConfig{Subnetworks: []string{"projects/p/regions/us-central1/subnetworks/s1"}, Limit: limit}}
}

func code(err error) codes.Code { return status.Code(err) }

func TestPolicyValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := newTest(t, "")
	cases := []struct {
		name string
		id   string
		p    *ncpb.ServiceConnectionPolicy
		want codes.Code
	}{
		{"no id", "", policy(nil), codes.InvalidArgument},
		{"bad id", "Bad_ID", policy(nil), codes.InvalidArgument},
		{"no class", "a", &ncpb.ServiceConnectionPolicy{Network: "projects/p/global/networks/net1"}, codes.InvalidArgument},
		{"missing network", "a", &ncpb.ServiceConnectionPolicy{Network: "projects/p/global/networks/nope", ServiceClass: "x"}, codes.InvalidArgument},
		{"another project's network", "a", &ncpb.ServiceConnectionPolicy{Network: "projects/q/global/networks/net1", ServiceClass: "x"}, codes.InvalidArgument},
		{"subnet elsewhere", "a", &ncpb.ServiceConnectionPolicy{Network: "projects/p/global/networks/net1", ServiceClass: "x",
			PscConfig: &ncpb.ServiceConnectionPolicy_PscConfig{Subnetworks: []string{"projects/p/regions/europe-west1/subnetworks/s1"}}}, codes.InvalidArgument},
		{"ok", "a", policy(nil), codes.OK},
		{"duplicate name", "a", policy(nil), codes.AlreadyExists},
		{"second for the class", "b", policy(nil), codes.AlreadyExists},
	}
	for _, c := range cases {
		_, err := s.CreateServiceConnectionPolicy(ctx, &ncpb.CreateServiceConnectionPolicyRequest{Parent: parentP, ServiceConnectionPolicyId: c.id, ServiceConnectionPolicy: c.p})
		if code(err) != c.want {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	p, err := s.GetServiceConnectionPolicy(ctx, &ncpb.GetServiceConnectionPolicyRequest{Name: parentP + "/serviceConnectionPolicies/a"})
	if err != nil || p.GetInfrastructure() != ncpb.Infrastructure_PSC || p.GetEtag() == "" || p.GetCreateTime() == nil {
		t.Fatalf("get = %v, %v", p, err)
	}
	if _, err := s.UpdateServiceConnectionPolicy(ctx, &ncpb.UpdateServiceConnectionPolicyRequest{ServiceConnectionPolicy: &ncpb.ServiceConnectionPolicy{
		Name: p.Name, ServiceClass: "other"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"service_class"}}}); code(err) != codes.InvalidArgument {
		t.Errorf("update service_class: %v", err)
	}
	stale := "stale"
	if _, err := s.DeleteServiceConnectionPolicy(ctx, &ncpb.DeleteServiceConnectionPolicyRequest{Name: p.Name, Etag: &stale}); code(err) != codes.Aborted {
		t.Errorf("delete with a stale etag: %v", err)
	}
}

// A producer's map waits for the consumer's policy, connects through it
// up to its limit, and its endpoints go with it; a policy with
// connections is not deleted.
func TestProducerConnectsThroughPolicy(t *testing.T) {
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "nc.json")
	s, fc := newTest(t, state)
	att := "projects/9999/regions/us-central1/serviceAttachments/a-1"
	if err := s.SetProducer(CloudSQLServiceClass, att, "p", "inst", []Consumer{{Network: "projects/p/global/networks/net1"}}); err != nil {
		t.Fatal(err)
	}
	got := s.Producer(att)
	if len(got) != 1 || got[0].NetworkStatus != "CONNECTION_POLICY_MISSING" || got[0].IP != "" {
		t.Fatalf("before the policy: %+v", got)
	}
	one := int64(1)
	if _, err := s.CreateServiceConnectionPolicy(ctx, &ncpb.CreateServiceConnectionPolicyRequest{Parent: parentP, ServiceConnectionPolicyId: "sql",
		ServiceConnectionPolicy: policy(&one)}); err != nil {
		t.Fatal(err)
	}
	got = s.Producer(att)
	if len(got) != 1 || got[0].NetworkStatus != "VALID" || got[0].Status != "ACTIVE" || got[0].IP != "10.0.0.10" {
		t.Fatalf("after the policy: %+v", got)
	}
	p, _ := s.GetServiceConnectionPolicy(ctx, &ncpb.GetServiceConnectionPolicyRequest{Name: parentP + "/serviceConnectionPolicies/sql"})
	if len(p.GetPscConnections()) != 1 || p.GetPscConnections()[0].GetConsumerAddress() != "10.0.0.10" || p.GetPscConnections()[0].GetState() != ncpb.ServiceConnectionPolicy_ACTIVE {
		t.Fatalf("policy connections: %v", p.GetPscConnections())
	}
	// A second producer is over the limit of one.
	att2 := "projects/9999/regions/us-central1/serviceAttachments/a-2"
	_ = s.SetProducer(CloudSQLServiceClass, att2, "p", "inst2", []Consumer{{Network: "projects/p/global/networks/net1"}})
	if got := s.Producer(att2); got[0].NetworkStatus != "POLICY_LIMIT_REACHED" {
		t.Fatalf("over the limit: %+v", got)
	}
	// A producer in another project is outside the default scope.
	att3 := "projects/9999/regions/us-central1/serviceAttachments/a-3"
	_ = s.SetProducer(CloudSQLServiceClass, att3, "other", "inst3", []Consumer{{Network: "projects/p/global/networks/net1"}})
	if got := s.Producer(att3); got[0].NetworkStatus != "CONSUMER_INSTANCE_PROJECT_NOT_ALLOWLISTED" {
		t.Fatalf("out of scope: %+v", got)
	}
	if _, err := s.DeleteServiceConnectionPolicy(ctx, &ncpb.DeleteServiceConnectionPolicyRequest{Name: p.Name}); code(err) != codes.FailedPrecondition {
		t.Fatalf("delete with a connection: %v", err)
	}
	// Restored from its state, the same.
	s2, _ := newTest(t, state)
	if got := s2.Producer(att); len(got) != 1 || got[0].IP != "10.0.0.10" {
		t.Fatalf("restored: %+v", got)
	}
	// Raising the limit connects the second.
	two := int64(2)
	if _, err := s.UpdateServiceConnectionPolicy(ctx, &ncpb.UpdateServiceConnectionPolicyRequest{ServiceConnectionPolicy: &ncpb.ServiceConnectionPolicy{Name: p.Name,
		PscConfig: &ncpb.ServiceConnectionPolicy_PscConfig{Limit: &two}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"psc_config.limit"}}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Producer(att2); got[0].Status != "ACTIVE" {
		t.Fatalf("after raising the limit: %+v", got)
	}
	// Removing the producers removes their endpoints; then the policy goes.
	_ = s.SetProducer(CloudSQLServiceClass, att, "p", "inst", nil)
	_ = s.SetProducer(CloudSQLServiceClass, att2, "p", "inst2", nil)
	if len(fc.made) != 0 {
		t.Fatalf("endpoints left: %v", fc.made)
	}
	if _, err := s.DeleteServiceConnectionPolicy(ctx, &ncpb.DeleteServiceConnectionPolicyRequest{Name: p.Name}); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// A user's map makes its service class, which is not deleted while the
// map names it, and reports its consumers' connections.
func TestUserMapAndClass(t *testing.T) {
	ctx := context.Background()
	s, _ := newTest(t, "")
	if _, err := s.CreateServiceConnectionPolicy(ctx, &ncpb.CreateServiceConnectionPolicyRequest{Parent: parentP, ServiceConnectionPolicyId: "mine",
		ServiceConnectionPolicy: &ncpb.ServiceConnectionPolicy{Network: "projects/p/global/networks/net1", ServiceClass: "my-class",
			PscConfig: &ncpb.ServiceConnectionPolicy_PscConfig{Subnetworks: []string{"projects/p/regions/us-central1/subnetworks/s1"}}}}); err != nil {
		t.Fatal(err)
	}
	m := &ncpb.ServiceConnectionMap{ServiceClass: "my-class",
		ProducerPscConfigs: []*ncpb.ServiceConnectionMap_ProducerPscConfig{{ServiceAttachmentUri: "projects/p/regions/us-central1/serviceAttachments/sa"}},
		ConsumerPscConfigs: []*ncpb.ServiceConnectionMap_ConsumerPscConfig{{Network: "projects/p/global/networks/net1", ConsumerInstanceProject: "projects/p"}}}
	if _, err := s.CreateServiceConnectionMap(ctx, &ncpb.CreateServiceConnectionMapRequest{Parent: parentP, ServiceConnectionMapId: "m", ServiceConnectionMap: m}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetServiceConnectionMap(ctx, &ncpb.GetServiceConnectionMapRequest{Name: parentP + "/serviceConnectionMaps/m"})
	if err != nil || got.GetServiceClassUri() != parentP+"/serviceClasses/my-class" || len(got.GetConsumerPscConnections()) != 1 ||
		got.GetConsumerPscConfigs()[0].GetState() != ncpb.ServiceConnectionMap_ConsumerPscConfig_VALID || got.GetConsumerPscConfigs()[0].GetProject() != "p" {
		t.Fatalf("map = %v, %v", got, err)
	}
	if _, err := s.DeleteServiceClass(ctx, &ncpb.DeleteServiceClassRequest{Name: got.GetServiceClassUri()}); code(err) != codes.FailedPrecondition {
		t.Fatalf("delete a class in use: %v", err)
	}
	ls, err := s.ListServiceConnectionMaps(ctx, &ncpb.ListServiceConnectionMapsRequest{Parent: "projects/p/locations/-"})
	if err != nil || len(ls.GetServiceConnectionMaps()) != 1 {
		t.Fatalf("list across locations: %v, %v", ls, err)
	}
	if _, err := s.DeleteServiceConnectionMap(ctx, &ncpb.DeleteServiceConnectionMapRequest{Name: got.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteServiceClass(ctx, &ncpb.DeleteServiceClassRequest{Name: got.GetServiceClassUri()}); err != nil {
		t.Fatalf("delete the class: %v", err)
	}
	tok, err := s.CreateServiceConnectionToken(ctx, &ncpb.CreateServiceConnectionTokenRequest{Parent: parentP, ServiceConnectionTokenId: "t",
		ServiceConnectionToken: &ncpb.ServiceConnectionToken{Network: "projects/p/global/networks/net1"}})
	if err != nil || !tok.GetDone() || !strings.HasPrefix(tok.GetName(), parentP+"/operations/") {
		t.Fatalf("token: %v, %v", tok, err)
	}
}
