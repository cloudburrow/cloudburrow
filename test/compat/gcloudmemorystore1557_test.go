//go:build compat

package compat

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
)

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance, google.cloud.memorystore.v1.Memorystore/StartMigration, google.cloud.memorystore.v1.Memorystore/FinishMigration, google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster
//
// TestGcloudMemorystore1557 (#1557): through the configuration `cloudburrow
// gcloud-setup` writes, the real gcloud creates a Memorystore for Valkey
// instance with --kms-key (a key of this instance's Cloud KMS, made with
// the official client) and a Redis cluster with --kms-key, each read back
// with the key; an instance at --engine-version=VALKEY_7_2 upgraded with
// `instances update --engine-version=VALKEY_9_0`; a secondary created with
// --cross-instance-replication-config-role=secondary --primary-instance,
// detached with --cross-instance-replication-config-role=none; and
// start-migration and finish-migration from a self-managed source.
func TestGcloudMemorystore1557(t *testing.T) {
	h := New(t)
	vc, _ := valkeyAdminClient(t, h) // skips without the admin API
	rc, _ := redisClusterAdminClient(t, h)
	// Cloud KMS as the shard's CMEK_KMS (#1568), so the KMS suite stays in served.
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	kmsClients(t, h) // skips without Cloud KMS
	g := newGcloudSession(t, h)
	sfx := time.Now().UnixNano() % 1e7
	p := "--project=" + g.project
	loc := "--location=us-central1"
	parent := "projects/" + g.project + "/locations/us-central1"
	psc := "--psc-auto-connections=network=projects/" + g.project + "/global/networks/default,projectId=" + g.project
	small := []string{p, loc, "--mode=cluster-disabled", "--replica-count=0", "--node-type=shared-core-nano", psc}
	instance := func(id string) *memorystorepb.Instance {
		t.Helper()
		inst, err := vc.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: parent + "/instances/" + id})
		if err != nil {
			t.Fatalf("GetInstance %s: %v", id, err)
		}
		return inst
	}
	cleanup := func(id string) {
		t.Cleanup(func() {
			for i := 0; i < 3; i++ {
				if op, err := vc.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: parent + "/instances/" + id}); err == nil {
					_ = op.Wait(h.Context())
					return
				}
				time.Sleep(2 * time.Second)
			}
		})
	}
	create := func(id string, extra ...string) *memorystorepb.Instance {
		t.Helper()
		cleanup(id)
		g.must(append([]string{"memorystore", "instances", "create", id}, append(small, extra...)...)...)
		return instance(id)
	}

	// kms_key, on a Valkey instance and a Redis cluster.
	key := memorystoreKey(t, h, g.project)
	kid := fmt.Sprintf("gkms-%d", sfx)
	if inst := create(kid, "--kms-key="+key); inst.GetKmsKey() != key ||
		inst.GetEncryptionInfo().GetEncryptionType() != memorystorepb.EncryptionInfo_CUSTOMER_MANAGED_ENCRYPTION {
		t.Errorf("instance created with --kms-key: kms_key %q, encryption_info %v", inst.GetKmsKey(), inst.GetEncryptionInfo())
	}
	if got := strings.TrimSpace(g.must("memorystore", "instances", "describe", kid, p, loc, "--format=value(kmsKey)")); got != key {
		t.Errorf("describe kmsKey = %q, want %s", got, key)
	}
	cid := fmt.Sprintf("gkmsc-%d", sfx)
	cname := parent + "/clusters/" + cid
	t.Cleanup(func() {
		if op, err := rc.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: cname}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	g.must("redis", "clusters", "create", cid, p, "--region=us-central1", "--shard-count=1", "--replica-count=0",
		"--node-type=redis-shared-core-nano", "--network=projects/"+g.project+"/global/networks/default", "--kms-key="+key)
	if cl, err := rc.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: cname}); err != nil || cl.GetKmsKey() != key {
		t.Errorf("redis cluster created with --kms-key: %v, %v", cl.GetKmsKey(), err)
	}

	// engine_version: created at 7.2, upgraded to 9.0.
	eid := fmt.Sprintf("gver-%d", sfx)
	if inst := create(eid, "--engine-version=VALKEY_7_2"); inst.GetEngineVersion() != "VALKEY_7_2" {
		t.Errorf("created with --engine-version=VALKEY_7_2: engine_version %q", inst.GetEngineVersion())
	}
	g.must("memorystore", "instances", "update", eid, p, loc, "--engine-version=VALKEY_9_0")
	if inst := instance(eid); inst.GetEngineVersion() != "VALKEY_9_0" || inst.GetState() != memorystorepb.Instance_ACTIVE {
		t.Errorf("after update --engine-version=VALKEY_9_0: %q, %v", inst.GetEngineVersion(), inst.GetState())
	}

	// Cross-instance replication: a secondary, then detached.
	pid, sid := fmt.Sprintf("gcirp-%d", sfx), fmt.Sprintf("gcirs-%d", sfx)
	pri := create(pid)
	sec := create(sid, "--cross-instance-replication-config-role=secondary", "--primary-instance="+pri.GetName())
	if cfg := sec.GetCrossInstanceReplicationConfig(); cfg.GetInstanceRole() != memorystorepb.CrossInstanceReplicationConfig_SECONDARY ||
		cfg.GetPrimaryInstance().GetInstance() != pri.GetName() {
		t.Errorf("secondary created with gcloud: %v", cfg)
	}
	if got := g.must("memorystore", "instances", "describe", pid, p, loc,
		"--format=value(crossInstanceReplicationConfig.instanceRole,crossInstanceReplicationConfig.secondaryInstances[0].instance)"); !strings.Contains(got, "PRIMARY") || !strings.Contains(got, sid) {
		t.Errorf("describe of the primary = %q, want PRIMARY and its secondary", got)
	}
	g.must("memorystore", "instances", "update", sid, p, loc, "--cross-instance-replication-config-role=none")
	if cfg := instance(sid).GetCrossInstanceReplicationConfig(); cfg != nil {
		t.Errorf("after the detach: cross_instance_replication_config = %v, want none", cfg)
	}

	// Migration from a self-managed source (another instance's primary).
	srcID, dstID := fmt.Sprintf("gmigs-%d", sfx), fmt.Sprintf("gmigd-%d", sfx)
	src := create(srcID)
	create(dstID)
	host, port, _ := net.SplitHostPort(valkeyEndpoint(t, src, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY))
	g.must("memorystore", "instances", "start-migration", dstID, p, loc, "--self-managed-source-ip-address="+host,
		"--self-managed-source-port="+port, "--self-managed-source-network-attachment=projects/"+g.project+"/regions/us-central1/networkAttachments/mig")
	if inst := instance(dstID); inst.GetState() != memorystorepb.Instance_MIGRATING ||
		inst.GetMigrationConfig().GetState() != memorystorepb.MigrationConfig_REPLICATION_ESTABLISHED {
		t.Errorf("after start-migration: %v, %v", inst.GetState(), inst.GetMigrationConfig())
	}
	g.must("memorystore", "instances", "finish-migration", dstID, p, loc)
	if inst := instance(dstID); inst.GetState() != memorystorepb.Instance_ACTIVE ||
		inst.GetMigrationConfig().GetState() != memorystorepb.MigrationConfig_MIGRATED {
		t.Errorf("after finish-migration: %v, %v", inst.GetState(), inst.GetMigrationConfig())
	}
}
