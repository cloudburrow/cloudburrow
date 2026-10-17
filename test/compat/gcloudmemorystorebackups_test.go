//go:build compat

package compat

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"github.com/redis/go-redis/v9"
)

// covers: google.cloud.memorystore.v1.Memorystore/AddTokenAuthUser, google.cloud.memorystore.v1.Memorystore/AddAuthToken, google.cloud.memorystore.v1.Memorystore/ListAuthTokens, google.cloud.memorystore.v1.Memorystore/BackupInstance, google.cloud.memorystore.v1.Memorystore/ListBackups, google.cloud.memorystore.v1.Memorystore/GetBackup, google.cloud.memorystore.v1.Memorystore/DeleteBackup, google.cloud.redis.cluster.v1.CloudRedisCluster/BackupCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/ListBackupCollections, google.cloud.redis.cluster.v1.CloudRedisCluster/ListBackups, google.cloud.redis.cluster.v1.CloudRedisCluster/GetBackup, google.cloud.redis.cluster.v1.CloudRedisCluster/DeleteBackup
//
// TestGcloudMemorystoreTokenAuthBackups (#1549): through the configuration
// `cloudburrow gcloud-setup` writes, the real gcloud creates a Memorystore
// for Valkey instance with --authorization-mode=token-auth, adds a token
// auth user and a token (create-token-auth-user, create-auth-token), whose
// token (auth-tokens list) logs a go-redis client in; backs it up
// (instances backup), lists and describes the backup, creates an instance
// from it (--managed-backup-source) with the key, and deletes it. For a
// Redis cluster: redis clusters create-backup, backup-collections list,
// backups list, describe and delete.
func TestGcloudMemorystoreTokenAuthBackups(t *testing.T) {
	h := New(t)
	rc, _ := redisClusterAdminClient(t, h) // skips without the admin API
	vc, _ := valkeyAdminClient(t, h)
	g := newGcloudSession(t, h)
	sfx := time.Now().UnixNano() % 1e8
	p := "--project=" + g.project
	loc := "--location=us-central1"
	psc := "--psc-auto-connections=network=projects/" + g.project + "/global/networks/default,projectId=" + g.project

	vid := fmt.Sprintf("gvktok-%d", sfx)
	vname := "projects/" + g.project + "/locations/us-central1/instances/" + vid
	rid := fmt.Sprintf("gvkrst-%d", sfx)
	for _, n := range []string{vname, "projects/" + g.project + "/locations/us-central1/instances/" + rid} {
		t.Cleanup(func() {
			if op, err := vc.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: n}); err == nil {
				_ = op.Wait(h.Context())
			}
		})
	}
	g.must("memorystore", "instances", "create", vid, p, loc, "--shard-count=1", "--replica-count=0",
		"--node-type=shared-core-nano", "--authorization-mode=token-auth", psc)
	g.must("memorystore", "instances", "create-token-auth-user", vid, p, loc, "--token-auth-user=app")
	g.must("memorystore", "instances", "token-auth-users", "create-auth-token", "app", "--instance="+vid, p, loc)
	tok := strings.TrimSpace(g.must("memorystore", "instances", "token-auth-users", "auth-tokens", "list", "--token-auth-user=app",
		"--instance="+vid, p, loc, "--format=value(token)"))
	inst, err := vc.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: vname})
	if err != nil || tok == "" {
		t.Fatalf("GetInstance = %v, %v; token %q", inst, err, tok)
	}
	ep := inst.GetDiscoveryEndpoints()[0]
	cc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{net.JoinHostPort(ep.GetAddress(), strconv.Itoa(int(ep.GetPort())))},
		Username: "app", Password: tok})
	defer cc.Close()
	if err := cc.Set(h.Context(), "greeting", "hello", 0).Err(); err != nil {
		t.Fatalf("SET with the token gcloud listed: %v", err)
	}
	col := inst.GetBackupCollection()
	colID := col[strings.LastIndex(col, "/")+1:]
	g.must("memorystore", "instances", "backup", vid, p, loc, "--backup-id=first")
	if got := g.must("memorystore", "backup-collections", "list", p, loc, "--format=value(name)"); !strings.Contains(got, colID) {
		t.Errorf("backup-collections list = %q, want %s", got, colID)
	}
	if got := g.must("memorystore", "backup-collections", "backups", "list", "--backup-collection="+colID, p, loc, "--format=value(name)"); !strings.Contains(got, "first") {
		t.Errorf("backups list = %q", got)
	}
	if got := g.must("memorystore", "backup-collections", "backups", "describe", "first", "--backup-collection="+colID, p, loc,
		"--format=value(state,backupType)"); !strings.Contains(got, "ACTIVE\tON_DEMAND") {
		t.Errorf("backups describe = %q", got)
	}
	g.must("memorystore", "instances", "create", rid, p, loc, "--shard-count=1", "--replica-count=0", "--node-type=shared-core-nano", psc,
		"--managed-backup-source="+col+"/backups/first")
	rinst, err := vc.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: "projects/" + g.project + "/locations/us-central1/instances/" + rid})
	if err != nil {
		t.Fatalf("GetInstance of the restored instance: %v", err)
	}
	rep := rinst.GetDiscoveryEndpoints()[0]
	rcc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{net.JoinHostPort(rep.GetAddress(), strconv.Itoa(int(rep.GetPort())))}})
	defer rcc.Close()
	if v, err := rcc.Get(h.Context(), "greeting").Result(); err != nil || v != "hello" {
		t.Errorf("restored greeting = %q, %v", v, err)
	}
	g.must("memorystore", "backup-collections", "backups", "delete", "first", "--backup-collection="+colID, p, loc)

	// A Redis cluster's backups.
	cid := fmt.Sprintf("gclbk-%d", sfx)
	cname := "projects/" + g.project + "/locations/us-central1/clusters/" + cid
	t.Cleanup(func() {
		if op, err := rc.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: cname}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	g.must("redis", "clusters", "create", cid, p, "--region=us-central1", "--shard-count=1", "--replica-count=0",
		"--node-type=redis-shared-core-nano", "--network=projects/"+g.project+"/global/networks/default")
	g.must("redis", "clusters", "create-backup", cid, p, "--region=us-central1", "--backup-id=first", "--ttl=7d")
	cl, err := rc.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: cname})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	ccol := cl.GetBackupCollection()[strings.LastIndex(cl.GetBackupCollection(), "/")+1:]
	if got := g.must("redis", "clusters", "backup-collections", "list", p, "--region=us-central1", "--format=value(name)"); !strings.Contains(got, ccol) {
		t.Errorf("redis clusters backup-collections list = %q, want %s", got, ccol)
	}
	if got := g.must("redis", "clusters", "backups", "list", "--backup-collection="+ccol, p, "--region=us-central1", "--format=value(name)"); !strings.Contains(got, "first") {
		t.Errorf("redis clusters backups list = %q", got)
	}
	if got := g.must("redis", "clusters", "backups", "describe", "first", "--backup-collection="+ccol, p, "--region=us-central1",
		"--format=value(shardCount,state)"); !strings.Contains(got, "1\tACTIVE") {
		t.Errorf("redis clusters backups describe = %q", got)
	}
	g.must("redis", "clusters", "backups", "delete", "first", "--backup-collection="+ccol, p, "--region=us-central1")
}
