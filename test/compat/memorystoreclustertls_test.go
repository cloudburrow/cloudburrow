//go:build compat

package compat

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// trustCA is a TLS configuration trusting only the PEM certificates, as a
// client of a Memorystore server with in-transit encryption is set up.
func trustCA(t *testing.T, certs []string) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	for _, c := range certs {
		if !pool.AppendCertsFromPEM([]byte(c)) {
			t.Fatalf("managed_server_ca holds no PEM certificate: %q", c)
		}
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// iamToken is an access token of the form CloudBurrow's credentials issue,
// which a client of an IAM_AUTH cluster sends as its AUTH string.
const iamToken = "cbl_compatcompatcompatcompatcompatcomp"

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/GetClusterCertificateAuthority
//
// TestMemorystoreRedisClusterInTransitEncryption (#1549), with
// cloud.google.com/go/redis/cluster/apiv1: a cluster of 2 shards of 1
// replica created with transit_encryption_mode SERVER_AUTHENTICATION and
// AUTH_MODE_IAM_AUTH comes back with both; GetClusterCertificateAuthority
// returns its CA as managed_server_ca, which a go-redis ClusterClient
// trusts, sending an access token as its AUTH string, to set and get keys
// in every shard over TLS (the replicas replicate over TLS). A plain TCP
// client and a TLS client trusting no CA are refused; transit_encryption_mode
// cannot be updated.
func TestMemorystoreRedisClusterInTransitEncryption(t *testing.T) {
	h := New(t)
	c, project := redisClusterAdminClient(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("compat-cltls-%d", time.Now().UnixNano()%1e8)
	name := parent + "/clusters/" + id
	op, err := c.CreateCluster(h.Context(), &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: id, Cluster: &clusterpb.Cluster{
		ShardCount: proto.Int32(2), ReplicaCount: proto.Int32(1), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO,
		AuthorizationMode:     clusterpb.AuthorizationMode_AUTH_MODE_IAM_AUTH,
		TransitEncryptionMode: clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION,
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	cl, err := op.Wait(memorystoreWait(t))
	if err != nil {
		t.Fatalf("CreateCluster operation: %v", err)
	}
	if cl.GetState() != clusterpb.Cluster_ACTIVE || len(cl.GetDiscoveryEndpoints()) != 1 ||
		cl.GetTransitEncryptionMode() != clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION ||
		cl.GetAuthorizationMode() != clusterpb.AuthorizationMode_AUTH_MODE_IAM_AUTH {
		t.Fatalf("created cluster = %v", cl)
	}
	ca, err := c.GetClusterCertificateAuthority(h.Context(), &clusterpb.GetClusterCertificateAuthorityRequest{Name: name + "/certificateAuthority"})
	if err != nil {
		t.Fatalf("GetClusterCertificateAuthority: %v", err)
	}
	chains := ca.GetManagedServerCa().GetCaCerts()
	if ca.GetName() != name+"/certificateAuthority" || len(chains) != 1 || len(chains[0].GetCertificates()) == 0 {
		t.Fatalf("GetClusterCertificateAuthority = %v", ca)
	}
	ep := cl.GetDiscoveryEndpoints()[0]
	addr := net.JoinHostPort(ep.GetAddress(), strconv.Itoa(int(ep.GetPort())))
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}, Password: iamToken,
		TLSConfig: trustCA(t, chains[0].GetCertificates())})
	defer rc.Close()
	if n := len(clusterMasters(t, h, rc)); n != 2 {
		t.Errorf("CLUSTER SLOTS names %d primaries, want 2", n)
	}
	clusterKeys(t, h, rc, "tls", 40, true)
	nodes, err := rc.ClusterNodes(h.Context()).Result()
	if err != nil || strings.Count(nodes, "slave") != 2 || strings.Contains(nodes, "fail") {
		t.Errorf("CLUSTER NODES = %q, %v; want 2 replicas linked over TLS", nodes, err)
	}

	plain := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, DialTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second})
	defer plain.Close()
	if err := plain.Ping(h.Context()).Err(); err == nil {
		t.Error("a plain TCP client reached a node of a cluster with in-transit encryption")
	}
	untrusted := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}})
	defer untrusted.Close()
	if err := untrusted.Ping(h.Context()).Err(); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("a TLS client trusting no CA = %v, want a certificate error", err)
	}
	if _, err := c.UpdateCluster(h.Context(), &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"transit_encryption_mode"}},
		Cluster: &clusterpb.Cluster{Name: name}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateCluster transit_encryption_mode = %v, want INVALID_ARGUMENT", err)
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetCertificateAuthority
//
// TestMemorystoreValkeyInTransitEncryption (#1549), with
// cloud.google.com/go/memorystore/apiv1: an instance in CLUSTER_DISABLED
// mode with 1 replica, created with SERVER_AUTHENTICATION and IAM_AUTH,
// comes back with both; GetCertificateAuthority returns its CA as
// managed_server_ca, which go-redis clients of the primary and the reader
// trust, the primary's writes reaching the reader over TLS replication. An
// instance in CLUSTER mode of 1 shard with TLS serves a go-redis
// ClusterClient trusting its own CA. A plain client is refused.
func TestMemorystoreValkeyInTransitEncryption(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	net0 := []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: "projects/" + project + "/global/networks/default"}}
	create := func(id string, in *memorystorepb.Instance) (*memorystorepb.Instance, string) {
		t.Helper()
		name := parent + "/instances/" + id
		op, err := c.CreateInstance(h.Context(), &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: in})
		if err != nil {
			t.Fatalf("CreateInstance %s: %v", id, err)
		}
		t.Cleanup(func() {
			if op, err := c.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
				_ = op.Wait(h.Context())
			}
		})
		inst, err := op.Wait(memorystoreWait(t))
		if err != nil {
			t.Fatalf("CreateInstance %s operation: %v", id, err)
		}
		if inst.GetTransitEncryptionMode() != memorystorepb.Instance_SERVER_AUTHENTICATION {
			t.Fatalf("created instance = %v", inst)
		}
		ca, err := c.GetCertificateAuthority(h.Context(), &memorystorepb.GetCertificateAuthorityRequest{Name: name + "/certificateAuthority"})
		if err != nil {
			t.Fatalf("GetCertificateAuthority %s: %v", id, err)
		}
		chains := ca.GetManagedServerCa().GetCaCerts()
		if len(chains) != 1 || len(chains[0].GetCertificates()) == 0 {
			t.Fatalf("GetCertificateAuthority = %v", ca)
		}
		return inst, chains[0].GetCertificates()[0]
	}
	sfx := time.Now().UnixNano() % 1e8
	inst, caPEM := create(fmt.Sprintf("compat-vktls-%d", sfx), &memorystorepb.Instance{Mode: memorystorepb.Instance_CLUSTER_DISABLED,
		ReplicaCount: proto.Int32(1), NodeType: memorystorepb.Instance_SHARED_CORE_NANO, PscAutoConnections: net0,
		AuthorizationMode: memorystorepb.Instance_IAM_AUTH, TransitEncryptionMode: memorystorepb.Instance_SERVER_AUTHENTICATION})
	if inst.GetAuthorizationMode() != memorystorepb.Instance_IAM_AUTH {
		t.Errorf("authorization_mode = %s, want IAM_AUTH", inst.GetAuthorizationMode())
	}
	tc := trustCA(t, []string{caPEM})
	paddr := valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	primary := redis.NewClient(&redis.Options{Addr: paddr, Password: iamToken, TLSConfig: tc})
	defer primary.Close()
	reader := redis.NewClient(&redis.Options{Addr: valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_READER), Password: iamToken, TLSConfig: tc})
	defer reader.Close()
	if err := primary.Set(h.Context(), "greeting", "hello", 0).Err(); err != nil {
		t.Fatalf("SET on the primary over TLS: %v", err)
	}
	var v string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if v, _ = reader.Get(h.Context(), "greeting").Result(); v == "hello" {
			break
		}
	}
	if v != "hello" {
		t.Errorf("GET on the reader over TLS = %q, want hello", v)
	}
	plain := redis.NewClient(&redis.Options{Addr: paddr, MaxRetries: -1, DialTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second})
	defer plain.Close()
	if err := plain.Ping(h.Context()).Err(); err == nil {
		t.Error("a plain TCP client reached an instance with in-transit encryption")
	}

	cinst, cPEM := create(fmt.Sprintf("compat-vkctls-%d", sfx), &memorystorepb.Instance{ShardCount: 1,
		NodeType: memorystorepb.Instance_SHARED_CORE_NANO, TransitEncryptionMode: memorystorepb.Instance_SERVER_AUTHENTICATION})
	if len(cinst.GetDiscoveryEndpoints()) != 1 {
		t.Fatalf("created instance = %v", cinst)
	}
	if cPEM == caPEM {
		t.Error("two instances share a CA")
	}
	ep := cinst.GetDiscoveryEndpoints()[0]
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{net.JoinHostPort(ep.GetAddress(), strconv.Itoa(int(ep.GetPort())))},
		TLSConfig: trustCA(t, []string{cPEM})})
	defer rc.Close()
	clusterKeys(t, h, rc, "vtls", 20, true)
}
