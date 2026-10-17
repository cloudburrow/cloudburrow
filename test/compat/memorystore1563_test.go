//go:build compat

package compat

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	memorystore "cloud.google.com/go/memorystore/apiv1"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// valkeyCA is the CA certificates of inst, from GetCertificateAuthority.
func valkeyCA(t *testing.T, ctx context.Context, c *memorystore.Client, inst *memorystorepb.Instance) []string {
	t.Helper()
	ca, err := c.GetCertificateAuthority(ctx, &memorystorepb.GetCertificateAuthorityRequest{Name: inst.GetName() + "/certificateAuthority"})
	if err != nil {
		t.Fatalf("GetCertificateAuthority %s: %v", inst.GetName(), err)
	}
	var out []string
	for _, ch := range ca.GetManagedServerCa().GetCaCerts() {
		out = append(out, ch.GetCertificates()...)
	}
	if len(out) == 0 {
		t.Fatalf("GetCertificateAuthority %s = %v", inst.GetName(), ca)
	}
	return out
}

func valkeyTLSPrimary(t *testing.T, inst *memorystorepb.Instance, tc *tls.Config) *redis.Client {
	t.Helper()
	rc := redis.NewClient(&redis.Options{Addr: valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY), TLSConfig: tc})
	t.Cleanup(func() { _ = rc.Close() })
	return rc
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance, google.cloud.memorystore.v1.Memorystore/GetCertificateAuthority
//
// TestMemorystoreValkeyCrossInstanceReplicationTLS (#1563), with cloud.
// google.com/go/memorystore/apiv1 and go-redis over TLS: a secondary of a
// primary with in-transit encryption (SERVER_AUTHENTICATION) replicates it
// over TLS, each served with its own CA, and a switchover makes the former
// primary replicate the new one; a secondary whose transit_encryption_mode
// is not its primary's is INVALID_ARGUMENT.
func TestMemorystoreValkeyCrossInstanceReplicationTLS(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	sfx := time.Now().UnixNano() % 1e7
	withTLS := func(in *memorystorepb.Instance) {
		in.TransitEncryptionMode = memorystorepb.Instance_SERVER_AUTHENTICATION
	}
	p := valkeyDisabled(t, ctx, c, project, fmt.Sprintf("cirtls-p-%d", sfx), withTLS)
	pc := valkeyTLSPrimary(t, p, trustCA(t, valkeyCA(t, ctx, c, p)))
	if err := pc.Set(ctx, "before", "1", 0).Err(); err != nil {
		t.Fatalf("SET on the primary: %v", err)
	}
	secondaryOf := func(in *memorystorepb.Instance) {
		in.CrossInstanceReplicationConfig = &memorystorepb.CrossInstanceReplicationConfig{
			InstanceRole:    memorystorepb.CrossInstanceReplicationConfig_SECONDARY,
			PrimaryInstance: &memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{Instance: p.GetName()}}
	}
	_, err := c.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/us-central1",
		InstanceId: fmt.Sprintf("cirtls-x-%d", sfx), Instance: func() *memorystorepb.Instance {
			in := &memorystorepb.Instance{Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(0), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
				PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: "projects/" + project + "/global/networks/default"}}}
			secondaryOf(in)
			return in
		}()})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a secondary without the primary's in-transit encryption = %v, want INVALID_ARGUMENT", err)
	}
	s := valkeyDisabled(t, ctx, c, project, fmt.Sprintf("cirtls-s-%d", sfx), func(in *memorystorepb.Instance) {
		withTLS(in)
		secondaryOf(in)
	})
	if s.GetTransitEncryptionMode() != memorystorepb.Instance_SERVER_AUTHENTICATION ||
		s.GetCrossInstanceReplicationConfig().GetInstanceRole() != memorystorepb.CrossInstanceReplicationConfig_SECONDARY {
		t.Errorf("secondary = %v", s)
	}
	sc := valkeyTLSPrimary(t, s, trustCA(t, valkeyCA(t, ctx, c, s)))
	valkeyEventually(t, ctx, sc, "before", "1")
	if err := pc.Set(ctx, "after", "2", 0).Err(); err != nil {
		t.Fatal(err)
	}
	valkeyEventually(t, ctx, sc, "after", "2")
	if err := sc.Set(ctx, "x", "y", 0).Err(); err == nil || !strings.Contains(err.Error(), "READONLY") {
		t.Errorf("SET on the secondary = %v, want READONLY", err)
	}

	// Switchover: the former primary replicates the new one, over TLS.
	cirUpdate(t, ctx, c, s.GetName(), &memorystorepb.CrossInstanceReplicationConfig{
		InstanceRole:       memorystorepb.CrossInstanceReplicationConfig_PRIMARY,
		SecondaryInstances: []*memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{{Instance: p.GetName()}}})
	if err := sc.Set(ctx, "switched", "3", 0).Err(); err != nil {
		t.Fatalf("SET on the new primary: %v", err)
	}
	valkeyEventually(t, ctx, pc, "switched", "3")
	if err := pc.Set(ctx, "x", "y", 0).Err(); err == nil || !strings.Contains(err.Error(), "READONLY") {
		t.Errorf("SET on the former primary = %v, want READONLY", err)
	}
	cirUpdate(t, ctx, c, p.GetName(), &memorystorepb.CrossInstanceReplicationConfig{
		InstanceRole: memorystorepb.CrossInstanceReplicationConfig_NONE})
}

// upgradeWhileWriting upgrades instance name to version while write sets
// keys; it returns the keys whose SET was acknowledged and the instance.
func upgradeWhileWriting(t *testing.T, ctx context.Context, c *memorystore.Client, name, version string, write func(k, v string) error) (map[string]string, *memorystorepb.Instance) {
	t.Helper()
	acked := map[string]string{}
	var mu sync.Mutex
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			k, v := fmt.Sprintf("during-%d", i), strconv.Itoa(i)
			if write(k, v) == nil {
				mu.Lock()
				acked[k] = v
				mu.Unlock()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	op, err := c.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"engine_version"}},
		Instance: &memorystorepb.Instance{Name: name, EngineVersion: version}})
	if err != nil {
		close(stop)
		t.Fatalf("UpdateInstance engine_version: %v", err)
	}
	inst, err := op.Wait(ctx)
	close(stop)
	<-done
	if err != nil || inst.GetEngineVersion() != version {
		t.Fatalf("UpdateInstance engine_version operation = %v, %v", inst.GetEngineVersion(), err)
	}
	if len(acked) == 0 {
		t.Fatal("no SET was acknowledged during the upgrade")
	}
	return acked, inst
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance
//
// TestMemorystoreValkeyUpgradeKeepsWrites (#1563), with cloud.google.com/
// go/memorystore/apiv1 and go-redis: every write a client made, and the
// server acknowledged, while a CLUSTER_DISABLED instance with a replica was
// upgraded from VALKEY_7_2 to VALKEY_8_0 is there after the upgrade, which
// runs VALKEY_8_0 (Valkey 8.1).
func TestMemorystoreValkeyUpgradeKeepsWrites(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	inst := valkeyDisabled(t, ctx, c, project, fmt.Sprintf("upw-%d", time.Now().UnixNano()%1e7), func(in *memorystorepb.Instance) {
		in.EngineVersion, in.ReplicaCount = "VALKEY_7_2", proto.Int32(1)
	})
	rc := valkeyPrimary(t, inst)
	acked, inst := upgradeWhileWriting(t, ctx, c, inst.GetName(), "VALKEY_8_0", func(k, v string) error {
		return rc.Set(ctx, k, v, 0).Err()
	})
	after := valkeyPrimary(t, inst)
	info, err := after.Info(ctx, "server").Result()
	if err != nil || !strings.Contains(info, "valkey_version:8.") {
		t.Errorf("after the upgrade, INFO server = %v (%q), want valkey_version 8.x (VALKEY_8_0 runs Valkey 8.1)", err, info)
	}
	for k, want := range acked {
		if got, err := after.Get(ctx, k).Result(); got != want {
			t.Errorf("after the upgrade, GET %s (acknowledged during it) = %q, %v; want %q", k, got, err, want)
		}
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance, google.cloud.memorystore.v1.Memorystore/StartMigration
//
// TestMemorystoreValkeyClusterUpgradeKeepsWrites (#1563), with cloud.
// google.com/go/memorystore/apiv1 and a go-redis ClusterClient: writes made
// during the engine upgrade of a CLUSTER mode instance (new nodes join as
// replicas and take over by CLUSTER FAILOVER) are there after it. It also
// measures why cross-instance replication and migration into CLUSTER mode
// stay UNIMPLEMENTED: a cluster-mode node refuses REPLICAOF, so its shards
// cannot replicate another cluster's or a self-managed source's primaries.
func TestMemorystoreValkeyClusterUpgradeKeepsWrites(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("cupw-%d", time.Now().UnixNano()%1e7)
	name := parent + "/instances/" + id
	op, err := c.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &memorystorepb.Instance{
		ShardCount: 2, ReplicaCount: proto.Int32(0), NodeType: memorystorepb.Instance_SHARED_CORE_NANO, EngineVersion: "VALKEY_8_0",
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: "projects/" + project + "/global/networks/default"}}}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	inst, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	endpoint := func(inst *memorystorepb.Instance) string {
		e := inst.GetDiscoveryEndpoints()[0]
		return net.JoinHostPort(e.GetAddress(), strconv.Itoa(int(e.GetPort())))
	}
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{endpoint(inst)}})
	defer rc.Close()

	// The measurement: REPLICAOF on a cluster-mode node.
	node := redis.NewClient(&redis.Options{Addr: endpoint(inst)})
	err = node.Do(ctx, "REPLICAOF", "127.0.0.1", "6379").Err()
	_ = node.Close()
	if err == nil || !strings.Contains(err.Error(), "not allowed in cluster mode") {
		t.Errorf("REPLICAOF on a cluster-mode node = %v, want it refused in cluster mode", err)
	}
	if _, err := c.StartMigration(ctx, &memorystorepb.StartMigrationRequest{Name: name, Source: &memorystorepb.StartMigrationRequest_SelfManagedSource{
		SelfManagedSource: &memorystorepb.SelfManagedSource{IpAddress: "127.0.0.1", Port: 6379,
			NetworkAttachment: "projects/" + project + "/regions/us-central1/networkAttachments/na"}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("StartMigration into CLUSTER mode = %v, want UNIMPLEMENTED", err)
	}

	acked, inst := upgradeWhileWriting(t, ctx, c, name, "VALKEY_9_0", func(k, v string) error {
		return rc.Set(ctx, k, v, 0).Err()
	})
	after := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{endpoint(inst)}})
	defer after.Close()
	for k, want := range acked {
		if got, err := after.Get(ctx, k).Result(); got != want {
			t.Errorf("after the upgrade, GET %s (acknowledged during it) = %q, %v; want %q", k, got, err, want)
		}
	}
}
