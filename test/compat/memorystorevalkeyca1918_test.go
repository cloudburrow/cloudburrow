//go:build compat

package compat

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance, google.cloud.memorystore.v1.Memorystore/GetCertificateAuthority, google.cloud.memorystore.v1.Memorystore/GetSharedRegionalCertificateAuthority
//
// TestMemorystoreValkeyServerCA (#1918), with cloud.google.com/go/
// memorystore/apiv1, the official Certificate Authority Service client and
// go-redis over TLS: a Memorystore for Valkey instance's server_ca_mode
// and server_ca_pool, as Google documents them
// (https://docs.cloud.google.com/memorystore/docs/valkey/about-in-transit-encryption,
// /valkey/use-customer-managed-ca). Refused: a customer-managed mode
// without a pool, a pool with another mode, a shared CA without in-transit
// encryption, a malformed pool and a pool in another region
// (INVALID_ARGUMENT, Google's texts), a pool that does not exist
// (NOT_FOUND, "Resource {pool} not found.") and one with no enabled CA
// (FAILED_PRECONDITION). A per-instance CA instance's on-demand
// rotate_server_certificate is FAILED_PRECONDITION, "Server certificate
// rotation can only be triggered for customer-managed CAs.". A SHARED_CA
// instance's CA is the region's (GetSharedRegionalCertificateAuthority),
// which a client trusts to connect. A CUSTOMER_MANAGED_CAS_CA instance
// reads back its mode and pool, GetCertificateAuthority is
// FAILED_PRECONDITION (the certificates are the pool's, from CA Service),
// a client trusting only the pool's root connects, its node's certificate
// is listed among the pool's, and rotate_server_certificate gives the node
// another one the pool issues, after which a new client still connects.
func TestMemorystoreValkeyServerCA(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	shardEndpoint(h, envMemorystorePrivateCA, EnvPrivateCA)
	cas := privateCAClients(t, h)["grpc"]
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	suffix := time.Now().UnixNano() % 1e7
	rootCert, pool := sqlCAHierarchy(t, ctx, cas, project, fmt.Sprintf("vca%d", suffix))
	empty := strings.TrimSuffix(pool, "-issuing") + "-empty"

	tlsOn := memorystorepb.Instance_SERVER_AUTHENTICATION
	shape := func(in *memorystorepb.Instance) *memorystorepb.Instance {
		in.Mode, in.ReplicaCount, in.NodeType = memorystorepb.Instance_CLUSTER_DISABLED, proto.Int32(0), memorystorepb.Instance_SHARED_CORE_NANO
		in.PscAutoConnections = []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: "projects/" + project + "/global/networks/default"}}
		return in
	}
	customer := memorystorepb.Instance_SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA.Enum()
	for _, tc := range []struct {
		what string
		in   *memorystorepb.Instance
		want codes.Code
		text string
	}{
		{"customer-managed without a pool", &memorystorepb.Instance{TransitEncryptionMode: tlsOn, ServerCaMode: customer}, codes.InvalidArgument,
			"server_ca_pool must be provided when server_ca_mode is SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA."},
		{"a pool with the per-instance CA", &memorystorepb.Instance{TransitEncryptionMode: tlsOn,
			ServerCaMode: memorystorepb.Instance_SERVER_CA_MODE_GOOGLE_MANAGED_PER_INSTANCE_CA.Enum(), ServerCaPool: proto.String(pool)}, codes.InvalidArgument,
			"server_ca_pool must not be provided when server_ca_mode is not SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA."},
		{"shared CA without in-transit encryption", &memorystorepb.Instance{ServerCaMode: memorystorepb.Instance_SERVER_CA_MODE_GOOGLE_MANAGED_SHARED_CA.Enum()}, codes.InvalidArgument, ""},
		{"a malformed pool", &memorystorepb.Instance{TransitEncryptionMode: tlsOn, ServerCaMode: customer, ServerCaPool: proto.String("my-pool")}, codes.InvalidArgument,
			"Invalid format for server CA pool: my-pool. Expected projects/{project}/locations/{location}/caPools/{caPool}."},
		{"a pool in another region", &memorystorepb.Instance{TransitEncryptionMode: tlsOn, ServerCaMode: customer,
			ServerCaPool: proto.String(strings.Replace(pool, "us-central1", "europe-west1", 1))}, codes.InvalidArgument,
			"The instance's server CA pool must be in the same region as the instance."},
		{"a pool that does not exist", &memorystorepb.Instance{TransitEncryptionMode: tlsOn, ServerCaMode: customer,
			ServerCaPool: proto.String(parent + "/caPools/no-such-pool")}, codes.NotFound, "Resource " + parent + "/caPools/no-such-pool not found."},
		{"a pool with no enabled CA", &memorystorepb.Instance{TransitEncryptionMode: tlsOn, ServerCaMode: customer, ServerCaPool: proto.String(empty)},
			codes.FailedPrecondition, "There are no enabled CAs in the CaPool " + empty + "."},
	} {
		_, err := c.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: "vca-refused", Instance: shape(tc.in)})
		wantRedisCode(t, "CreateInstance with "+tc.what, err, tc.want)
		if tc.text != "" && err != nil && status.Convert(err).Message() != tc.text {
			t.Errorf("CreateInstance with %s: %q, want %q", tc.what, status.Convert(err).Message(), tc.text)
		}
	}
	create := func(id string, in *memorystorepb.Instance) *memorystorepb.Instance {
		t.Helper()
		name := parent + "/instances/" + id
		t.Cleanup(func() {
			if op, err := c.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
				wctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				_ = waitLRODone(wctx, op)
			}
		})
		op, err := c.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: shape(in)})
		if err != nil {
			t.Fatalf("CreateInstance %s: %v", id, err)
		}
		got, err := waitLRO(ctx, op)
		if err != nil {
			t.Fatalf("CreateInstance %s operation: %v", id, err)
		}
		return got
	}
	setGet := func(what, addr string, tc *tls.Config) {
		t.Helper()
		rc := redis.NewClient(&redis.Options{Addr: addr, TLSConfig: tc.Clone()})
		defer rc.Close()
		if err := rc.Set(ctx, "k", what, 0).Err(); err != nil {
			t.Fatalf("%s: SET over TLS: %v", what, err)
		}
		if v, err := rc.Get(ctx, "k").Result(); err != nil || v != what {
			t.Errorf("%s: GET = %q, %v", what, v, err)
		}
	}
	serial := func(addr string, tc *tls.Config) string {
		t.Helper()
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, tc.Clone())
		if err != nil {
			t.Fatalf("TLS handshake with %s: %v", addr, err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.String()
	}
	primary := func(in *memorystorepb.Instance) string {
		return valkeyEndpoint(t, in, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	}
	rotate := func(name string) (*memorystorepb.Instance, error) {
		op, err := c.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotate_server_certificate"}},
			Instance: &memorystorepb.Instance{Name: name, RotateServerCertificate: proto.Bool(true)}})
		if err != nil {
			return nil, err
		}
		return waitValkeyOp(t, h, op)
	}

	// The per-instance CA: no on-demand rotation.
	own := create(fmt.Sprintf("vca-own-%d", suffix), &memorystorepb.Instance{TransitEncryptionMode: tlsOn})
	if own.GetServerCaMode() != memorystorepb.Instance_SERVER_CA_MODE_GOOGLE_MANAGED_PER_INSTANCE_CA {
		t.Errorf("per-instance: server_ca_mode %v", own.GetServerCaMode())
	}
	_, err := rotate(own.GetName())
	wantRedisCode(t, "rotate_server_certificate on a per-instance CA instance", err, codes.FailedPrecondition)
	if err != nil && status.Convert(err).Message() != "Server certificate rotation can only be triggered for customer-managed CAs." {
		t.Errorf("rotate_server_certificate refusal: %v", err)
	}

	// The shared regional CA.
	shared := create(fmt.Sprintf("vca-shr-%d", suffix), &memorystorepb.Instance{TransitEncryptionMode: tlsOn,
		ServerCaMode: memorystorepb.Instance_SERVER_CA_MODE_GOOGLE_MANAGED_SHARED_CA.Enum()})
	if shared.GetServerCaMode() != memorystorepb.Instance_SERVER_CA_MODE_GOOGLE_MANAGED_SHARED_CA {
		t.Errorf("shared: server_ca_mode %v", shared.GetServerCaMode())
	}
	regional, err := c.GetSharedRegionalCertificateAuthority(ctx, &memorystorepb.GetSharedRegionalCertificateAuthorityRequest{Name: parent + "/sharedRegionalCertificateAuthority"})
	if err != nil || len(regional.GetManagedServerCa().GetCaCerts()) == 0 {
		t.Fatalf("GetSharedRegionalCertificateAuthority = %v, %v", regional, err)
	}
	regionalCerts := regional.GetManagedServerCa().GetCaCerts()[0].GetCertificates()
	ownCA, err := c.GetCertificateAuthority(ctx, &memorystorepb.GetCertificateAuthorityRequest{Name: shared.GetName() + "/certificateAuthority"})
	if err != nil || len(ownCA.GetManagedServerCa().GetCaCerts()) == 0 || ownCA.GetManagedServerCa().GetCaCerts()[0].GetCertificates()[0] != regionalCerts[0] {
		t.Errorf("the shared CA instance's certificate authority is not the region's: %v, %v", ownCA, err)
	}
	setGet("shared", primary(shared), trustCA(t, regionalCerts))

	// A customer-managed CA, rotated on demand.
	cm := create(fmt.Sprintf("vca-cust-%d", suffix), &memorystorepb.Instance{TransitEncryptionMode: tlsOn, ServerCaMode: customer, ServerCaPool: proto.String(pool)})
	if cm.GetServerCaMode() != *customer || cm.GetServerCaPool() != pool {
		t.Errorf("customer-managed: server_ca_mode %v, server_ca_pool %q", cm.GetServerCaMode(), cm.GetServerCaPool())
	}
	_, err = c.GetCertificateAuthority(ctx, &memorystorepb.GetCertificateAuthorityRequest{Name: cm.GetName() + "/certificateAuthority"})
	wantRedisCode(t, "GetCertificateAuthority of a customer-managed CA instance", err, codes.FailedPrecondition)
	root := trustCA(t, []string{string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootCert.Raw}))})
	addr := primary(cm)
	setGet("customer", addr, root)
	listed := func() int {
		t.Helper()
		n := 0
		it := cas.ListCertificates(ctx, &privatecapb.ListCertificatesRequest{Parent: pool})
		for {
			cert, err := it.Next()
			if err == iterator.Done {
				return n
			}
			if err != nil {
				t.Fatalf("ListCertificates: %v", err)
			}
			if strings.Contains(cert.GetName(), "/certificates/memorystore-") {
				n++
			}
		}
	}
	issued := listed()
	if issued == 0 {
		t.Errorf("the pool %s lists no certificate issued for the instance's node", pool)
	}
	before := serial(addr, root)
	if _, err := rotate(cm.GetName()); err != nil {
		t.Fatalf("UpdateInstance rotate_server_certificate: %v", err)
	}
	if after := serial(addr, root); after == before {
		t.Errorf("rotate_server_certificate: the node still presents certificate %s", before)
	}
	if n := listed(); n <= issued {
		t.Errorf("rotate_server_certificate: the pool lists %d node certificates, %d before", n, issued)
	}
	setGet("customer, rotated", addr, root)
}
