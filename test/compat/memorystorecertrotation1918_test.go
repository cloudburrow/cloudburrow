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
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/UpdateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/GetClusterCertificateAuthority
//
// TestRedisClusterServerCertRotation (#1918), with cloud.google.com/go/
// redis/cluster/apiv1, the official Certificate Authority Service client
// and go-redis over TLS: "Every week, Memorystore for Redis Cluster
// conducts server-side certificate rotation", so advancing the Memorystore
// clock a week (clock:advance) gives the node of a per-instance CA cluster
// a new server certificate (another serial in a new TLS handshake), of the
// same CA: a connection opened before keeps working ("existing
// connections remain alive"), and a new client trusting the CA it
// downloaded before connects. rotate_server_certificate on that cluster is
// FAILED_PRECONDITION ("Server certificate rotation can only be triggered
// for customer-managed CAs."); on a customer-managed CA cluster it gives
// the node a certificate the pool issues (listed among its certificates),
// and a client trusting the pool's root connects.
func TestRedisClusterServerCertRotation(t *testing.T) {
	h := New(t)
	c, project := redisClusterAdminClient(t, h)
	shardEndpoint(h, envMemorystorePrivateCA, EnvPrivateCA)
	cas := privateCAClients(t, h)["grpc"]
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	suffix := time.Now().UnixNano() % 1e7
	tlsOn := clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION

	create := func(id string, cl *clusterpb.Cluster) *clusterpb.Cluster {
		t.Helper()
		name := parent + "/clusters/" + id
		t.Cleanup(func() {
			if op, err := c.DeleteCluster(context.Background(), &clusterpb.DeleteClusterRequest{Name: name}); err == nil {
				_ = waitLRODone(context.Background(), op)
			}
		})
		cl.ShardCount, cl.ReplicaCount, cl.NodeType = proto.Int32(1), proto.Int32(0), clusterpb.NodeType_REDIS_SHARED_CORE_NANO
		cl.AuthorizationMode, cl.TransitEncryptionMode = clusterpb.AuthorizationMode_AUTH_MODE_DISABLED, tlsOn
		cl.PscConfigs = []*clusterpb.PscConfig{{Network: "projects/" + project + "/global/networks/default"}}
		op, err := c.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: id, Cluster: cl})
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
	// serial is the server certificate a new TLS handshake presents.
	serial := func(addr string, tc *tls.Config) string {
		t.Helper()
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, tc.Clone())
		if err != nil {
			t.Fatalf("TLS handshake with %s: %v", addr, err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.String()
	}
	waitNewSerial := func(what, addr string, tc *tls.Config, old string) string {
		t.Helper()
		deadline := time.Now().Add(90 * time.Second)
		for {
			if s := serial(addr, tc); s != old {
				return s
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the node still presents certificate %s", what, old)
			}
			time.Sleep(time.Second)
		}
	}
	setGet := func(what, addr string, tc *tls.Config) {
		t.Helper()
		rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}, TLSConfig: tc})
		defer rc.Close()
		if err := rc.Set(ctx, "rot", what, 0).Err(); err != nil {
			t.Fatalf("%s: SET over TLS: %v", what, err)
		}
		if v, err := rc.Get(ctx, "rot").Result(); err != nil || v != what {
			t.Errorf("%s: GET = %q, %v", what, v, err)
		}
	}

	// The weekly rotation, by the Memorystore clock.
	own := create(fmt.Sprintf("rot-own-%d", suffix), &clusterpb.Cluster{})
	ca, err := c.GetClusterCertificateAuthority(ctx, &clusterpb.GetClusterCertificateAuthorityRequest{Name: own.GetName() + "/certificateAuthority"})
	if err != nil {
		t.Fatalf("GetClusterCertificateAuthority: %v", err)
	}
	downloaded := trustCA(t, ca.GetManagedServerCa().GetCaCerts()[0].GetCertificates())
	addr := addrOf(own)
	before := serial(addr, downloaded)
	held := redis.NewClient(&redis.Options{Addr: addr, TLSConfig: downloaded.Clone(), PoolSize: 1})
	defer held.Close()
	conn := held.Conn()
	defer conn.Close()
	if err := conn.Ping(ctx).Err(); err != nil {
		t.Fatalf("PING before the rotation: %v", err)
	}
	_, err = c.UpdateCluster(ctx, &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotate_server_certificate"}},
		Cluster: &clusterpb.Cluster{Name: own.GetName(), RotateServerCertificate: proto.Bool(true)}})
	wantRedisCode(t, "rotate_server_certificate on a per-instance CA cluster", err, codes.FailedPrecondition)
	if err != nil && !strings.Contains(err.Error(), "Server certificate rotation can only be triggered for customer-managed CAs.") {
		t.Errorf("rotate_server_certificate refusal: %v", err)
	}
	memorystoreControl(t, h, "clock:advance", `{"duration": "608400s"}`)
	after := waitNewSerial("a week on", addr, downloaded, before)
	t.Logf("server certificate %s, a week on %s", before, after)
	if err := conn.Ping(ctx).Err(); err != nil {
		t.Errorf("the connection opened before the rotation: PING %v", err)
	}
	setGet("after the weekly rotation", addr, downloaded)

	// On demand, with a customer-managed CA.
	rootCert, pool := sqlCAHierarchy(t, ctx, cas, project, fmt.Sprintf("rot%d", suffix))
	cm := create(fmt.Sprintf("rot-cust-%d", suffix), &clusterpb.Cluster{
		ServerCaMode: clusterpb.ServerCaMode_SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA.Enum(), ServerCaPool: proto.String(pool)})
	root := trustCA(t, []string{string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootCert.Raw}))})
	cmAddr := addrOf(cm)
	cmBefore := serial(cmAddr, root)
	op, err := c.UpdateCluster(ctx, &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotate_server_certificate"}},
		Cluster: &clusterpb.Cluster{Name: cm.GetName(), RotateServerCertificate: proto.Bool(true)}})
	if err != nil {
		t.Fatalf("UpdateCluster rotate_server_certificate: %v", err)
	}
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatalf("rotate_server_certificate operation: %v", err)
	}
	if s := serial(cmAddr, root); s == cmBefore {
		t.Errorf("rotate_server_certificate: the node still presents certificate %s", s)
	}
	setGet("after rotate_server_certificate", cmAddr, root)
	issued := 0
	it := cas.ListCertificates(ctx, &privatecapb.ListCertificatesRequest{Parent: pool})
	for {
		cert, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListCertificates: %v", err)
		}
		if strings.Contains(cert.GetName(), "/certificates/memorystore-") {
			issued++
		}
	}
	if issued < 2 {
		t.Errorf("the pool lists %d certificates for the node, want the first and the rotated one", issued)
	}
}
