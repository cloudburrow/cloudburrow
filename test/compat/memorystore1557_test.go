//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	memorystore "cloud.google.com/go/memorystore/apiv1"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// valkeyDisabled creates a CLUSTER_DISABLED instance of no replicas, with
// cfg's further fields, and deletes it when the test ends.
func valkeyDisabled(t *testing.T, ctx context.Context, c *memorystore.Client, project, id string, cfg func(*memorystorepb.Instance)) *memorystorepb.Instance {
	t.Helper()
	parent := "projects/" + project + "/locations/us-central1"
	in := &memorystorepb.Instance{Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(0),
		NodeType:           memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: "projects/" + project + "/global/networks/default"}}}
	if cfg != nil {
		cfg(in)
	}
	op, err := c.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: in})
	if err != nil {
		t.Fatalf("CreateInstance %s: %v", id, err)
	}
	name := parent + "/instances/" + id
	t.Cleanup(func() {
		for i := 0; i < 3; i++ {
			// A primary is deleted once its secondaries are gone.
			if op, err := c.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
				_ = waitLRODone(context.Background(), op)
				return
			}
			time.Sleep(2 * time.Second)
		}
	})
	inst, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("CreateInstance %s operation: %v", id, err)
	}
	return inst
}

func valkeyPrimary(t *testing.T, inst *memorystorepb.Instance) *redis.Client {
	t.Helper()
	rc := redis.NewClient(&redis.Options{Addr: valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)})
	t.Cleanup(func() { _ = rc.Close() })
	return rc
}

// valkeyEventually waits for rc's GET of k to be want.
func valkeyEventually(t *testing.T, ctx context.Context, rc *redis.Client, k, want string) {
	t.Helper()
	var got string
	var err error
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if got, err = rc.Get(ctx, k).Result(); got == want {
			return
		}
	}
	t.Errorf("GET %s = %q, %v; want %q", k, got, err, want)
}

func cirUpdate(t *testing.T, ctx context.Context, c *memorystore.Client, name string, cfg *memorystorepb.CrossInstanceReplicationConfig) *memorystorepb.Instance {
	t.Helper()
	op, err := c.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cross_instance_replication_config"}},
		Instance:   &memorystorepb.Instance{Name: name, CrossInstanceReplicationConfig: cfg}})
	if err != nil {
		t.Fatalf("UpdateInstance cross_instance_replication_config of %s: %v", name, err)
	}
	inst, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("UpdateInstance cross_instance_replication_config of %s operation: %v", name, err)
	}
	return inst
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance, google.cloud.memorystore.v1.Memorystore/DeleteInstance
//
// TestMemorystoreValkeyCrossInstanceReplication (#1557), with cloud.google.
// com/go/memorystore/apiv1 and go-redis: a CLUSTER_DISABLED instance created
// with instance_role SECONDARY and a primary_instance serves the primary's
// keys (read from the secondary), refuses writes (READONLY), and each
// reports the membership; the primary is not deleted while it has a
// secondary. A switchover (the secondary updated to PRIMARY, the former
// primary its secondary_instances) makes the secondary writable and the
// former primary its replica; a detach (instance_role NONE) makes it
// writable again and both leave the replication.
func TestMemorystoreValkeyCrossInstanceReplication(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	sfx := time.Now().UnixNano() % 1e7
	p := valkeyDisabled(t, ctx, c, project, fmt.Sprintf("cir-p-%d", sfx), nil)
	pc := valkeyPrimary(t, p)
	if err := pc.Set(ctx, "before", "1", 0).Err(); err != nil {
		t.Fatalf("SET on the primary: %v", err)
	}
	s := valkeyDisabled(t, ctx, c, project, fmt.Sprintf("cir-s-%d", sfx), func(in *memorystorepb.Instance) {
		in.CrossInstanceReplicationConfig = &memorystorepb.CrossInstanceReplicationConfig{
			InstanceRole:    memorystorepb.CrossInstanceReplicationConfig_SECONDARY,
			PrimaryInstance: &memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{Instance: p.GetName()}}
	})
	cfg := s.GetCrossInstanceReplicationConfig()
	if cfg.GetInstanceRole() != memorystorepb.CrossInstanceReplicationConfig_SECONDARY || cfg.GetPrimaryInstance().GetInstance() != p.GetName() ||
		cfg.GetPrimaryInstance().GetUid() != p.GetUid() || cfg.GetMembership().GetPrimaryInstance().GetInstance() != p.GetName() {
		t.Errorf("secondary's cross_instance_replication_config = %v", cfg)
	}
	got, err := c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: p.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if pcfg := got.GetCrossInstanceReplicationConfig(); pcfg.GetInstanceRole() != memorystorepb.CrossInstanceReplicationConfig_PRIMARY ||
		len(pcfg.GetSecondaryInstances()) != 1 || pcfg.GetSecondaryInstances()[0].GetInstance() != s.GetName() {
		t.Errorf("primary's cross_instance_replication_config = %v", pcfg)
	}
	sc := valkeyPrimary(t, s)
	valkeyEventually(t, ctx, sc, "before", "1")
	if err := pc.Set(ctx, "after", "2", 0).Err(); err != nil {
		t.Fatal(err)
	}
	valkeyEventually(t, ctx, sc, "after", "2")
	if err := sc.Set(ctx, "x", "y", 0).Err(); err == nil || !strings.Contains(err.Error(), "READONLY") {
		t.Errorf("SET on the secondary = %v, want READONLY", err)
	}
	if _, err := c.DeleteInstance(ctx, &memorystorepb.DeleteInstanceRequest{Name: p.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteInstance of a primary with a secondary = %v, want FAILED_PRECONDITION", err)
	}

	// Switchover: the secondary becomes the primary.
	s = cirUpdate(t, ctx, c, s.GetName(), &memorystorepb.CrossInstanceReplicationConfig{
		InstanceRole:       memorystorepb.CrossInstanceReplicationConfig_PRIMARY,
		SecondaryInstances: []*memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{{Instance: p.GetName()}}})
	if cfg := s.GetCrossInstanceReplicationConfig(); cfg.GetInstanceRole() != memorystorepb.CrossInstanceReplicationConfig_PRIMARY ||
		len(cfg.GetSecondaryInstances()) != 1 || cfg.GetSecondaryInstances()[0].GetInstance() != p.GetName() {
		t.Errorf("after the switchover, the new primary's config = %v", cfg)
	}
	if err := sc.Set(ctx, "switched", "3", 0).Err(); err != nil {
		t.Fatalf("SET on the new primary: %v", err)
	}
	valkeyEventually(t, ctx, pc, "switched", "3")
	if err := pc.Set(ctx, "x", "y", 0).Err(); err == nil || !strings.Contains(err.Error(), "READONLY") {
		t.Errorf("SET on the former primary = %v, want READONLY", err)
	}

	// Detach the former primary, now a secondary.
	p = cirUpdate(t, ctx, c, p.GetName(), &memorystorepb.CrossInstanceReplicationConfig{
		InstanceRole: memorystorepb.CrossInstanceReplicationConfig_NONE})
	if p.GetCrossInstanceReplicationConfig() != nil {
		t.Errorf("detached instance's config = %v, want none", p.GetCrossInstanceReplicationConfig())
	}
	if err := pc.Set(ctx, "detached", "4", 0).Err(); err != nil {
		t.Errorf("SET on the detached instance: %v", err)
	}
	if got, _ := c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: s.GetName()}); got.GetCrossInstanceReplicationConfig() != nil {
		t.Errorf("the primary with no secondary left: config = %v, want none", got.GetCrossInstanceReplicationConfig())
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance
//
// TestMemorystoreValkeyEngineVersions (#1557), with cloud.google.com/go/
// memorystore/apiv1 and go-redis: an instance in CLUSTER mode created at
// VALKEY_7_2 runs Valkey 7.2 and serves a ClusterClient; upgraded to
// VALKEY_9_0 it runs Valkey 9.0 and its keys are still there; a downgrade
// is INVALID_ARGUMENT.
func TestMemorystoreValkeyEngineVersions(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("vers-%d", time.Now().UnixNano()%1e8)
	name := parent + "/instances/" + id
	op, err := c.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &memorystorepb.Instance{
		ShardCount: 2, ReplicaCount: proto.Int32(0), NodeType: memorystorepb.Instance_SHARED_CORE_NANO, EngineVersion: "VALKEY_7_2",
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: "projects/" + project + "/global/networks/default"}}}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	inst, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	if inst.GetEngineVersion() != "VALKEY_7_2" {
		t.Errorf("engine_version = %q, want VALKEY_7_2", inst.GetEngineVersion())
	}
	addr := net.JoinHostPort(inst.GetDiscoveryEndpoints()[0].GetAddress(), strconv.Itoa(int(inst.GetDiscoveryEndpoints()[0].GetPort())))
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}})
	version := func(rc *redis.ClusterClient) string {
		info, err := rc.Info(ctx, "server").Result()
		if err != nil {
			t.Fatalf("INFO server: %v", err)
		}
		for _, l := range strings.Split(info, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "valkey_version:"); ok {
				return v
			}
		}
		return ""
	}
	if v := version(rc); !strings.HasPrefix(v, "7.2.") {
		t.Errorf("valkey_version = %q, want 7.2.x", v)
	}
	keys := map[string]string{}
	for i := 0; i < 20; i++ {
		k := fmt.Sprintf("k%d", i)
		keys[k] = strconv.Itoa(i)
		if err := rc.Set(ctx, k, keys[k], 0).Err(); err != nil {
			t.Fatalf("SET %s: %v", k, err)
		}
	}
	_ = rc.Close()
	uop, err := c.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"engine_version"}},
		Instance: &memorystorepb.Instance{Name: name, EngineVersion: "VALKEY_9_0"}})
	if err != nil {
		t.Fatalf("UpdateInstance engine_version: %v", err)
	}
	if inst, err = waitLRO(ctx, uop); err != nil || inst.GetEngineVersion() != "VALKEY_9_0" {
		t.Fatalf("UpdateInstance engine_version operation = %v, %v", inst.GetEngineVersion(), err)
	}
	addr = net.JoinHostPort(inst.GetDiscoveryEndpoints()[0].GetAddress(), strconv.Itoa(int(inst.GetDiscoveryEndpoints()[0].GetPort())))
	rc = redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}})
	defer rc.Close()
	if v := version(rc); !strings.HasPrefix(v, "9.0.") {
		t.Errorf("after the upgrade, valkey_version = %q, want 9.0.x", v)
	}
	for k, want := range keys {
		if got, err := rc.Get(ctx, k).Result(); got != want {
			t.Errorf("after the upgrade, GET %s = %q, %v; want %q", k, got, err, want)
		}
	}
	if _, err := c.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"engine_version"}},
		Instance: &memorystorepb.Instance{Name: name, EngineVersion: "VALKEY_8_0"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a downgrade to VALKEY_8_0 = %v, want INVALID_ARGUMENT", err)
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance, google.cloud.memorystore.v1.Memorystore/BackupInstance, google.cloud.memorystore.v1.Memorystore/GetBackup, google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion
//
// TestMemorystoreValkeyKmsKey (#1557), with cloud.google.com/go/memorystore/
// apiv1, cloud.google.com/go/redis/cluster/apiv1, cloud.google.com/go/kms/
// apiv1 and go-redis: an instance created with kms_key, a key in its
// region, reports it with encryption_info CUSTOMER_MANAGED_ENCRYPTION, and
// its backup is encrypted with it. With the key's version disabled the
// instance is suspended (not ACTIVE, kms_key_primary_state DISABLED) and no
// longer answers; enabled again, it resumes with its data. A Redis cluster
// takes kms_key too. A key in another region is INVALID_ARGUMENT.
func TestMemorystoreValkeyKmsKey(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	cc, _ := redisClusterAdminClient(t, h)
	// Cloud KMS as the shard's CMEK_KMS (#1568), so the KMS suite stays in served.
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	kc := kmsClients(t, h)["grpc"]
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	key := memorystoreKey(t, h, project)
	if _, err := c.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/europe-west1",
		InstanceId: "kms-elsewhere", Instance: &memorystorepb.Instance{ShardCount: 1, KmsKey: proto.String(key)}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a key in another region = %v, want INVALID_ARGUMENT", err)
	}
	inst := valkeyDisabled(t, ctx, c, project, fmt.Sprintf("kms-%d", time.Now().UnixNano()%1e8), func(in *memorystorepb.Instance) {
		in.KmsKey = proto.String(key)
	})
	if inst.GetKmsKey() != key || inst.GetEncryptionInfo().GetEncryptionType() != memorystorepb.EncryptionInfo_CUSTOMER_MANAGED_ENCRYPTION {
		t.Fatalf("created: kms_key %q, encryption_info %v", inst.GetKmsKey(), inst.GetEncryptionInfo())
	}
	rc := valkeyPrimary(t, inst)
	if err := rc.Set(ctx, "secret", "kept", 0).Err(); err != nil {
		t.Fatalf("SET: %v", err)
	}
	bop, err := c.BackupInstance(ctx, &memorystorepb.BackupInstanceRequest{Name: inst.GetName(), BackupId: proto.String("cmek")})
	if err != nil {
		t.Fatalf("BackupInstance: %v", err)
	}
	if _, err := waitLRO(ctx, bop); err != nil {
		t.Fatalf("BackupInstance operation: %v", err)
	}
	bname := inst.GetBackupCollection() + "/backups/cmek"
	b, err := c.GetBackup(ctx, &memorystorepb.GetBackupRequest{Name: bname})
	if err != nil || b.GetEncryptionInfo().GetEncryptionType() != memorystorepb.EncryptionInfo_CUSTOMER_MANAGED_ENCRYPTION {
		t.Errorf("GetBackup = %v, %v; want encryption_info CUSTOMER_MANAGED_ENCRYPTION", b.GetEncryptionInfo(), err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteBackup(context.Background(), &memorystorepb.DeleteBackupRequest{Name: bname}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})

	versionState := func(st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: st},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatalf("UpdateCryptoKeyVersion %v: %v", st, err)
		}
	}
	until := func(what string, ok func(*memorystorepb.Instance) bool) *memorystorepb.Instance {
		t.Helper()
		for {
			got, err := c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: inst.GetName()})
			if err == nil && ok(got) {
				return got
			}
			if ctx.Err() != nil {
				t.Fatalf("%s: %v, %v", what, got, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	versionState(kmspb.CryptoKeyVersion_DISABLED)
	got := until("suspended", func(i *memorystorepb.Instance) bool {
		return i.GetEncryptionInfo().GetKmsKeyPrimaryState() == memorystorepb.EncryptionInfo_DISABLED && len(i.GetPscAutoConnections()) == 0
	})
	if got.GetState() == memorystorepb.Instance_ACTIVE {
		t.Errorf("suspended: state %v, want not ACTIVE", got.GetState())
	}
	pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
	if err := rc.Ping(pctx).Err(); err == nil {
		t.Error("a suspended instance's primary still answers")
	}
	pcancel()
	versionState(kmspb.CryptoKeyVersion_ENABLED)
	got = until("resumed", func(i *memorystorepb.Instance) bool { return i.GetState() == memorystorepb.Instance_ACTIVE })
	if got.GetEncryptionInfo().GetKmsKeyPrimaryState() != memorystorepb.EncryptionInfo_ENABLED {
		t.Errorf("resumed: encryption_info %v", got.GetEncryptionInfo())
	}
	valkeyEventually(t, ctx, valkeyPrimary(t, got), "secret", "kept")

	parent := "projects/" + project + "/locations/us-central1"
	cid := fmt.Sprintf("kms-c-%d", time.Now().UnixNano()%1e8)
	cop, err := cc.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: cid, Cluster: &clusterpb.Cluster{
		ShardCount: proto.Int32(1), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO, KmsKey: proto.String(key),
		PscConfigs: []*clusterpb.PscConfig{{Network: "projects/" + project + "/global/networks/default"}}}})
	if err != nil {
		t.Fatalf("CreateCluster with kms_key: %v", err)
	}
	t.Cleanup(func() {
		if op, err := cc.DeleteCluster(context.Background(), &clusterpb.DeleteClusterRequest{Name: parent + "/clusters/" + cid}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	cl, err := waitLRO(ctx, cop)
	if err != nil {
		t.Fatalf("CreateCluster operation: %v", err)
	}
	if cl.GetKmsKey() != key || cl.GetEncryptionInfo().GetEncryptionType() != clusterpb.EncryptionInfo_CUSTOMER_MANAGED_ENCRYPTION {
		t.Errorf("cluster: kms_key %q, encryption_info %v", cl.GetKmsKey(), cl.GetEncryptionInfo())
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/StartMigration, google.cloud.memorystore.v1.Memorystore/FinishMigration
//
// TestMemorystoreValkeyMigration (#1557), with cloud.google.com/go/
// memorystore/apiv1 and go-redis: StartMigration of a CLUSTER_DISABLED
// instance from a self-managed source (another Valkey server here, named
// by the address its clients use) completes with the instance MIGRATING,
// migration_config REPLICATION_ESTABLISHED, and the source's keys, old and
// new, served by it; FinishMigration makes it ACTIVE, MIGRATED and
// writable. A FinishMigration with no migration is FAILED_PRECONDITION.
func TestMemorystoreValkeyMigration(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sfx := time.Now().UnixNano() % 1e7
	src := valkeyDisabled(t, ctx, c, project, fmt.Sprintf("mig-src-%d", sfx), nil)
	dst := valkeyDisabled(t, ctx, c, project, fmt.Sprintf("mig-dst-%d", sfx), nil)
	sc, dc := valkeyPrimary(t, src), valkeyPrimary(t, dst)
	if err := sc.Set(ctx, "old", "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FinishMigration(ctx, &memorystorepb.FinishMigrationRequest{Name: dst.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("FinishMigration with no migration = %v, want FAILED_PRECONDITION", err)
	}
	host, ps, _ := net.SplitHostPort(valkeyEndpoint(t, src, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY))
	port, _ := strconv.Atoi(ps)
	op, err := c.StartMigration(ctx, &memorystorepb.StartMigrationRequest{Name: dst.GetName(),
		Source: &memorystorepb.StartMigrationRequest_SelfManagedSource{SelfManagedSource: &memorystorepb.SelfManagedSource{
			IpAddress: host, Port: int32(port), NetworkAttachment: "projects/" + project + "/regions/us-central1/networkAttachments/mig"}}})
	if err != nil {
		t.Fatalf("StartMigration: %v", err)
	}
	inst, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("StartMigration operation: %v", err)
	}
	if inst.GetState() != memorystorepb.Instance_MIGRATING || inst.GetMigrationConfig().GetState() != memorystorepb.MigrationConfig_REPLICATION_ESTABLISHED ||
		inst.GetMigrationConfig().GetSelfManagedSource().GetIpAddress() != host {
		t.Errorf("migrating: state %v, migration_config %v", inst.GetState(), inst.GetMigrationConfig())
	}
	valkeyEventually(t, ctx, dc, "old", "1")
	if err := sc.Set(ctx, "new", "2", 0).Err(); err != nil {
		t.Fatal(err)
	}
	valkeyEventually(t, ctx, dc, "new", "2")
	fop, err := c.FinishMigration(ctx, &memorystorepb.FinishMigrationRequest{Name: dst.GetName()})
	if err != nil {
		t.Fatalf("FinishMigration: %v", err)
	}
	if inst, err = waitLRO(ctx, fop); err != nil {
		t.Fatalf("FinishMigration operation: %v", err)
	}
	if inst.GetState() != memorystorepb.Instance_ACTIVE || inst.GetMigrationConfig().GetState() != memorystorepb.MigrationConfig_MIGRATED {
		t.Errorf("migrated: state %v, migration_config %v", inst.GetState(), inst.GetMigrationConfig())
	}
	if err := dc.Set(ctx, "mine", "3", 0).Err(); err != nil {
		t.Errorf("SET after the migration: %v", err)
	}
}
