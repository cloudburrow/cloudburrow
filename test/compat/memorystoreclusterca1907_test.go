//go:build compat

package compat

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// envMemorystorePrivateCA is the memorystore shard's name for Certificate
// Authority Service, for a Redis cluster's customer-managed CA, so the
// privateca suite does not run there a second time.
const envMemorystorePrivateCA = "CLOUDBURROW_TEST_MEMORYSTORE_PRIVATECA"

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/GetCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/GetClusterCertificateAuthority, google.cloud.redis.cluster.v1.CloudRedisCluster/GetSharedRegionalCertificateAuthority
//
// TestRedisClusterServerCAModes (#1907), with cloud.google.com/go/redis/
// cluster/apiv1, the official Certificate Authority Service client and
// go-redis over TLS: server_ca_mode and server_ca_pool as Google
// documents them (https://docs.cloud.google.com/memorystore/docs/cluster/use-customer-managed-ca).
// Refused: a customer-managed mode without a pool ("server_ca_pool must be
// provided ..."), a pool with another mode, a shared CA without in-transit
// encryption, a pool in another region (INVALID_ARGUMENT), a pool that
// does not exist (NOT_FOUND) and a pool with no enabled CA
// (FAILED_PRECONDITION). A SHARED_CA cluster's certificate authority is
// the region's (GetSharedRegionalCertificateAuthority), whose CA a client
// trusts to connect. A CUSTOMER_MANAGED_CAS_CA cluster reads back its mode
// and pool, GetClusterCertificateAuthority is FAILED_PRECONDITION ("not
// supported for customer-managed CAs"), its node's certificate is one the
// pool issued (listed among the pool's certificates), and a client that
// trusts only the pool's root CA connects and sets and gets a key.
func TestRedisClusterServerCAModes(t *testing.T) {
	h := New(t)
	c, project := redisClusterAdminClient(t, h)
	shardEndpoint(h, envMemorystorePrivateCA, EnvPrivateCA)
	cas := privateCAClients(t, h)["grpc"]
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	suffix := time.Now().UnixNano() % 1e7
	rootCert, pool := sqlCAHierarchy(t, ctx, cas, project, fmt.Sprintf("mca%d", suffix))
	empty := strings.TrimSuffix(pool, "-issuing") + "-empty"

	tlsOn := clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION
	shape := func(cl *clusterpb.Cluster) *clusterpb.Cluster {
		cl.ShardCount, cl.ReplicaCount, cl.NodeType = proto.Int32(1), proto.Int32(0), clusterpb.NodeType_REDIS_SHARED_CORE_NANO
		cl.AuthorizationMode = clusterpb.AuthorizationMode_AUTH_MODE_DISABLED
		cl.PscConfigs = []*clusterpb.PscConfig{{Network: "projects/" + project + "/global/networks/default"}}
		return cl
	}
	customer := clusterpb.ServerCaMode_SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA.Enum()
	for _, tc := range []struct {
		what string
		cl   *clusterpb.Cluster
		want codes.Code
	}{
		{"customer-managed without a pool", &clusterpb.Cluster{TransitEncryptionMode: tlsOn, ServerCaMode: customer}, codes.InvalidArgument},
		{"a pool with the per-instance CA", &clusterpb.Cluster{TransitEncryptionMode: tlsOn, ServerCaMode: clusterpb.ServerCaMode_SERVER_CA_MODE_GOOGLE_MANAGED_PER_INSTANCE_CA.Enum(), ServerCaPool: proto.String(pool)}, codes.InvalidArgument},
		{"shared CA without in-transit encryption", &clusterpb.Cluster{ServerCaMode: clusterpb.ServerCaMode_SERVER_CA_MODE_GOOGLE_MANAGED_SHARED_CA.Enum()}, codes.InvalidArgument},
		{"a pool in another region", &clusterpb.Cluster{TransitEncryptionMode: tlsOn, ServerCaMode: customer, ServerCaPool: proto.String(strings.Replace(pool, "us-central1", "europe-west1", 1))}, codes.InvalidArgument},
		{"a pool that does not exist", &clusterpb.Cluster{TransitEncryptionMode: tlsOn, ServerCaMode: customer, ServerCaPool: proto.String(parent + "/caPools/no-such-pool")}, codes.NotFound},
		{"a pool with no enabled CA", &clusterpb.Cluster{TransitEncryptionMode: tlsOn, ServerCaMode: customer, ServerCaPool: proto.String(empty)}, codes.FailedPrecondition},
	} {
		_, err := c.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: "ca-refused", Cluster: shape(tc.cl)})
		wantRedisCode(t, "CreateCluster with "+tc.what, err, tc.want)
	}
	create := func(id string, cl *clusterpb.Cluster) *clusterpb.Cluster {
		t.Helper()
		name := parent + "/clusters/" + id
		t.Cleanup(func() {
			if op, err := c.DeleteCluster(context.Background(), &clusterpb.DeleteClusterRequest{Name: name}); err == nil {
				_ = waitLRODone(context.Background(), op)
			}
		})
		op, err := c.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: id, Cluster: shape(cl)})
		if err != nil {
			t.Fatalf("CreateCluster %s: %v", id, err)
		}
		got, err := waitLRO(ctx, op)
		if err != nil {
			t.Fatalf("CreateCluster %s operation: %v", id, err)
		}
		return got
	}
	addrOf := func(cl *clusterpb.Cluster) string {
		ep := cl.GetDiscoveryEndpoints()[0]
		return net.JoinHostPort(ep.GetAddress(), strconv.Itoa(int(ep.GetPort())))
	}
	setGet := func(what string, addr string, tc *tls.Config) {
		t.Helper()
		rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}, TLSConfig: tc})
		defer rc.Close()
		if err := rc.Set(ctx, "k", what, 0).Err(); err != nil {
			t.Fatalf("%s: SET over TLS: %v", what, err)
		}
		if v, err := rc.Get(ctx, "k").Result(); err != nil || v != what {
			t.Errorf("%s: GET = %q, %v", what, v, err)
		}
	}

	shared := create(fmt.Sprintf("ca-shared-%d", suffix), &clusterpb.Cluster{TransitEncryptionMode: tlsOn,
		ServerCaMode: clusterpb.ServerCaMode_SERVER_CA_MODE_GOOGLE_MANAGED_SHARED_CA.Enum()})
	if shared.GetServerCaMode() != clusterpb.ServerCaMode_SERVER_CA_MODE_GOOGLE_MANAGED_SHARED_CA {
		t.Errorf("shared: server_ca_mode %v", shared.GetServerCaMode())
	}
	regional, err := c.GetSharedRegionalCertificateAuthority(ctx, &clusterpb.GetSharedRegionalCertificateAuthorityRequest{Name: parent + "/sharedRegionalCertificateAuthority"})
	if err != nil || len(regional.GetManagedServerCa().GetCaCerts()) == 0 {
		t.Fatalf("GetSharedRegionalCertificateAuthority = %v, %v", regional, err)
	}
	regionalCerts := regional.GetManagedServerCa().GetCaCerts()[0].GetCertificates()
	own, err := c.GetClusterCertificateAuthority(ctx, &clusterpb.GetClusterCertificateAuthorityRequest{Name: shared.GetName() + "/certificateAuthority"})
	if err != nil || len(own.GetManagedServerCa().GetCaCerts()) == 0 || own.GetManagedServerCa().GetCaCerts()[0].GetCertificates()[0] != regionalCerts[0] {
		t.Errorf("the shared CA cluster's certificate authority is not the region's: %v, %v", own, err)
	}
	setGet("shared", addrOf(shared), trustCA(t, regionalCerts))

	cm := create(fmt.Sprintf("ca-cust-%d", suffix), &clusterpb.Cluster{TransitEncryptionMode: tlsOn, ServerCaMode: customer, ServerCaPool: proto.String(pool)})
	if cm.GetServerCaMode() != *customer || cm.GetServerCaPool() != pool {
		t.Errorf("customer-managed: server_ca_mode %v, server_ca_pool %q", cm.GetServerCaMode(), cm.GetServerCaPool())
	}
	_, err = c.GetClusterCertificateAuthority(ctx, &clusterpb.GetClusterCertificateAuthorityRequest{Name: cm.GetName() + "/certificateAuthority"})
	wantRedisCode(t, "GetClusterCertificateAuthority of a customer-managed CA cluster", err, codes.FailedPrecondition)
	rootPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootCert.Raw}))
	setGet("customer", addrOf(cm), trustCA(t, []string{rootPEM}))
	issued := false
	it := cas.ListCertificates(ctx, &privatecapb.ListCertificatesRequest{Parent: pool})
	for {
		cert, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListCertificates: %v", err)
		}
		issued = issued || strings.Contains(cert.GetName(), "/certificates/memorystore-")
	}
	if !issued {
		t.Errorf("the pool %s lists no certificate issued for the cluster's node", pool)
	}
}
