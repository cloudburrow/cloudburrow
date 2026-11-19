package redisadmin

import (
	"context"
	"testing"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type fakeCAS map[string]int

func (f fakeCAS) EnabledCAs(_ context.Context, pool string) (int, error) {
	n, ok := f[pool]
	if !ok {
		return 0, status.Error(codes.NotFound, "no pool")
	}
	return n, nil
}

func (f fakeCAS) CACerts(context.Context, string) ([]string, error) { return []string{"ROOT"}, nil }

// TestClusterServerCA (#1907): the server CA modes' checks, the shared CA
// one per project and region, and a customer's pool.
func TestClusterServerCA(t *testing.T) {
	ctx := context.Background()
	c := &clusters{now: time.Now, byName: map[string]*clusterRecord{}}
	const pool = "projects/p/locations/us-central1/caPools/pool"
	cust := clusterpb.ServerCaMode_SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA.Enum()
	shared := clusterpb.ServerCaMode_SERVER_CA_MODE_GOOGLE_MANAGED_SHARED_CA.Enum()
	try := func(in *clusterpb.Cluster, tlsOn bool) (*clusterRecord, error) {
		r := &clusterRecord{Name: "projects/p/locations/us-central1/clusters/c"}
		return r, c.serverCA(ctx, r, in, "p", "us-central1", tlsOn)
	}
	for _, tc := range []struct {
		in   *clusterpb.Cluster
		tls  bool
		want codes.Code
	}{
		{&clusterpb.Cluster{ServerCaMode: cust}, true, codes.InvalidArgument},
		{&clusterpb.Cluster{ServerCaPool: proto.String(pool)}, true, codes.InvalidArgument},
		{&clusterpb.Cluster{ServerCaMode: shared}, false, codes.InvalidArgument},
		{&clusterpb.Cluster{ServerCaMode: cust, ServerCaPool: proto.String("pool")}, true, codes.InvalidArgument},
		{&clusterpb.Cluster{ServerCaMode: cust, ServerCaPool: proto.String("projects/p/locations/europe-west1/caPools/pool")}, true, codes.InvalidArgument},
		{&clusterpb.Cluster{ServerCaMode: cust, ServerCaPool: proto.String(pool)}, true, codes.Unimplemented},
	} {
		if _, err := try(tc.in, tc.tls); code(err) != tc.want {
			t.Errorf("%v (TLS %v): %v, want %v", tc.in, tc.tls, err, tc.want)
		}
	}
	a, err := try(&clusterpb.Cluster{ServerCaMode: shared}, true)
	b, err2 := try(&clusterpb.Cluster{ServerCaMode: shared}, true)
	if err != nil || err2 != nil || a.TransitCA == "" || a.TransitCA != b.TransitCA || a.ServerCAMode != shared.String() {
		t.Errorf("shared CA: %v %v; the same CA for two clusters: %v", err, err2, a.TransitCA == b.TransitCA)
	}
	if p, _ := try(&clusterpb.Cluster{}, true); p.TransitCA == "" || p.TransitCA == a.TransitCA || p.TransitCAKey == "" {
		t.Error("the default mode makes the cluster a CA of its own")
	}
	c.cas = fakeCAS{pool: 1, "projects/p/locations/us-central1/caPools/empty": 0}
	if _, err := try(&clusterpb.Cluster{ServerCaMode: cust, ServerCaPool: proto.String("projects/p/locations/us-central1/caPools/none")}, true); code(err) != codes.NotFound {
		t.Errorf("missing pool: %v", err)
	}
	if _, err := try(&clusterpb.Cluster{ServerCaMode: cust, ServerCaPool: proto.String("projects/p/locations/us-central1/caPools/empty")}, true); code(err) != codes.FailedPrecondition {
		t.Errorf("pool with no enabled CA: %v", err)
	}
	r, err := try(&clusterpb.Cluster{ServerCaMode: cust, ServerCaPool: proto.String(pool)}, true)
	if err != nil || r.ServerCAPool != pool || r.TransitCA != "ROOT" || r.TransitCAKey != "" || r.spec("n").ServerCAPool != pool {
		t.Errorf("customer pool: %+v, %v", r, err)
	}
}
