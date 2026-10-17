//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/AddTokenAuthUser, google.cloud.redis.cluster.v1.CloudRedisCluster/AddAuthToken, google.cloud.redis.cluster.v1.CloudRedisCluster/GetAuthToken, google.cloud.redis.cluster.v1.CloudRedisCluster/ListAuthTokens, google.cloud.redis.cluster.v1.CloudRedisCluster/GetTokenAuthUser, google.cloud.redis.cluster.v1.CloudRedisCluster/ListTokenAuthUsers, google.cloud.redis.cluster.v1.CloudRedisCluster/DeleteAuthToken, google.cloud.redis.cluster.v1.CloudRedisCluster/DeleteTokenAuthUser, google.cloud.redis.cluster.v1.CloudRedisCluster/BackupCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/ListBackupCollections, google.cloud.redis.cluster.v1.CloudRedisCluster/GetBackupCollection, google.cloud.redis.cluster.v1.CloudRedisCluster/ListBackups, google.cloud.redis.cluster.v1.CloudRedisCluster/GetBackup, google.cloud.redis.cluster.v1.CloudRedisCluster/ExportBackup, google.cloud.redis.cluster.v1.CloudRedisCluster/DeleteBackup, google.cloud.redis.cluster.v1.CloudRedisCluster/CreateAclPolicy, google.cloud.redis.cluster.v1.CloudRedisCluster/GetAclPolicy, google.cloud.redis.cluster.v1.CloudRedisCluster/ListAclPolicies, google.cloud.redis.cluster.v1.CloudRedisCluster/UpdateAclPolicy, google.cloud.redis.cluster.v1.CloudRedisCluster/DeleteAclPolicy, google.cloud.redis.cluster.v1.CloudRedisCluster/GetAclPolicyRevision, google.cloud.redis.cluster.v1.CloudRedisCluster/ListAclPolicyRevisions
//
// TestMemorystoreRedisClusterTokenAuthBackups (#1549), with
// cloud.google.com/go/redis/cluster/apiv1 and go-redis: a cluster of 2
// shards created with AUTH_MODE_TOKEN_AUTH and an ACL policy refuses a
// client without a token (NOAUTH) and one with a wrong token; a token auth
// user with a token the service generated (GetAuthToken) sets keys in both
// shards; the policy's user may read app:* keys only, and an update of the
// policy (a second revision) lets it write them. BackupCluster keeps an RDB
// file per shard in the cluster's backup collection; ExportBackup writes
// them to a bucket; a cluster created with managed_backup_source has every
// key back. A deleted token no longer logs in; a deleted backup is gone.
func TestMemorystoreRedisClusterTokenAuthBackups(t *testing.T) {
	h := New(t)
	c, project := redisClusterAdminClient(t, h)
	sc := instanceStorage(t, h)
	bh := bucket(t, h, sc)
	parent := "projects/" + project + "/locations/us-central1"
	sfx := time.Now().UnixNano() % 1e8

	pol, err := c.CreateAclPolicy(h.Context(), &clusterpb.CreateAclPolicyRequest{Parent: parent, AclPolicyId: fmt.Sprintf("compat-acl-%d", sfx),
		AclPolicy: &clusterpb.AclPolicy{Rules: []*clusterpb.AclRule{{Username: "reader", Rule: "on >readerpass ~app:* +@read +ping +cluster|slots +cluster|shards"}}}})
	if err != nil {
		t.Fatalf("CreateAclPolicy: %v", err)
	}
	if got, err := c.GetAclPolicy(h.Context(), &clusterpb.GetAclPolicyRequest{Name: pol.GetName()}); err != nil || len(got.GetRules()) != 1 {
		t.Errorf("GetAclPolicy = %v, %v", got, err)
	}
	found := false
	for it := c.ListAclPolicies(h.Context(), &clusterpb.ListAclPoliciesRequest{Parent: parent}); ; {
		p, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListAclPolicies: %v", err)
		}
		found = found || p.GetName() == pol.GetName()
	}
	if !found {
		t.Errorf("ListAclPolicies does not list %s", pol.GetName())
	}

	create := func(id string, in *clusterpb.Cluster) *clusterpb.Cluster {
		t.Helper()
		name := parent + "/clusters/" + id
		op, err := c.CreateCluster(h.Context(), &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: id, Cluster: in})
		if err != nil {
			t.Fatalf("CreateCluster %s: %v", id, err)
		}
		t.Cleanup(func() {
			if op, err := c.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: name}); err == nil {
				_ = op.Wait(h.Context())
			}
		})
		cl, err := op.Wait(h.Context())
		if err != nil {
			t.Fatalf("CreateCluster %s operation: %v", id, err)
		}
		return cl
	}
	id := fmt.Sprintf("compat-cltok-%d", sfx)
	name := parent + "/clusters/" + id
	cl := create(id, &clusterpb.Cluster{ShardCount: proto.Int32(2), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO,
		AuthorizationMode: clusterpb.AuthorizationMode_AUTH_MODE_TOKEN_AUTH, AclPolicy: pol.GetName()})
	if cl.GetAuthorizationMode() != clusterpb.AuthorizationMode_AUTH_MODE_TOKEN_AUTH || cl.GetAclPolicy() != pol.GetName() || cl.GetBackupCollection() == "" {
		t.Fatalf("created cluster = %v", cl)
	}
	ep := cl.GetDiscoveryEndpoints()[0]
	addr := net.JoinHostPort(ep.GetAddress(), strconv.Itoa(int(ep.GetPort())))

	none := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	defer none.Close()
	if err := none.Set(h.Context(), "x", "1", 0).Err(); err == nil || !strings.Contains(err.Error(), "NOAUTH") {
		t.Errorf("SET without a token = %v, want NOAUTH", err)
	}

	uop, err := c.AddTokenAuthUser(h.Context(), &clusterpb.AddTokenAuthUserRequest{Cluster: name, TokenAuthUser: "app"})
	if err != nil {
		t.Fatalf("AddTokenAuthUser: %v", err)
	}
	if _, err := uop.Wait(h.Context()); err != nil {
		t.Fatalf("AddTokenAuthUser operation: %v", err)
	}
	user := name + "/tokenAuthUsers/app"
	top, err := c.AddAuthToken(h.Context(), &clusterpb.AddAuthTokenRequest{TokenAuthUser: user, AuthToken: &clusterpb.AuthToken{}})
	if err != nil {
		t.Fatalf("AddAuthToken: %v", err)
	}
	if u, err := top.Wait(h.Context()); err != nil || u.GetName() != user {
		t.Fatalf("AddAuthToken operation = %v, %v", u, err)
	}
	if u, err := c.GetTokenAuthUser(h.Context(), &clusterpb.GetTokenAuthUserRequest{Name: user}); err != nil || u.GetState() != clusterpb.TokenAuthUser_ACTIVE {
		t.Errorf("GetTokenAuthUser = %v, %v", u, err)
	}
	if u, err := c.ListTokenAuthUsers(h.Context(), &clusterpb.ListTokenAuthUsersRequest{Parent: name}).Next(); err != nil || u.GetName() != user {
		t.Errorf("ListTokenAuthUsers = %v, %v", u, err)
	}
	tok, err := c.ListAuthTokens(h.Context(), &clusterpb.ListAuthTokensRequest{Parent: user}).Next()
	if err != nil || tok.GetToken() == "" {
		t.Fatalf("ListAuthTokens = %v, %v", tok, err)
	}
	if got, err := c.GetAuthToken(h.Context(), &clusterpb.GetAuthTokenRequest{Name: tok.GetName()}); err != nil || got.GetToken() != tok.GetToken() {
		t.Errorf("GetAuthToken = %v, %v", got, err)
	}
	wrong := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}, Username: "app", Password: "not-the-token", MaxRetries: -1})
	defer wrong.Close()
	if err := wrong.Set(h.Context(), "x", "1", 0).Err(); err == nil || !strings.Contains(err.Error(), "WRONGPASS") {
		t.Errorf("SET with a wrong token = %v, want WRONGPASS", err)
	}
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}, Username: "app", Password: tok.GetToken()})
	defer rc.Close()
	clusterKeys(t, h, rc, "tok", 30, true)
	if err := rc.Set(h.Context(), "app:greeting", "hello", 0).Err(); err != nil {
		t.Fatalf("SET app:greeting: %v", err)
	}

	// The ACL policy's user: read app:* only, then, after an update, write too.
	reader := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}, Username: "reader", Password: "readerpass", MaxRetries: -1})
	defer reader.Close()
	if v, err := reader.Get(h.Context(), "app:greeting").Result(); err != nil || v != "hello" {
		t.Errorf("the policy's user GET app:greeting = %q, %v", v, err)
	}
	if err := reader.Set(h.Context(), "app:other", "x", 0).Err(); err == nil || !strings.Contains(err.Error(), "NOPERM") {
		t.Errorf("the policy's user SET = %v, want NOPERM", err)
	}
	if err := reader.Get(h.Context(), "tok-1").Err(); err == nil || !strings.Contains(err.Error(), "NOPERM") {
		t.Errorf("the policy's user GET tok-1 = %v, want NOPERM", err)
	}
	pop, err := c.UpdateAclPolicy(h.Context(), &clusterpb.UpdateAclPolicyRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rules"}},
		AclPolicy: &clusterpb.AclPolicy{Name: pol.GetName(), Rules: []*clusterpb.AclRule{{Username: "reader", Rule: "on >readerpass ~app:* +@read +@write +ping +cluster|slots +cluster|shards"}}}})
	if err != nil {
		t.Fatalf("UpdateAclPolicy: %v", err)
	}
	if p, err := pop.Wait(h.Context()); err != nil || len(p.GetClusterAclPolicyAttachments()) != 1 {
		t.Fatalf("UpdateAclPolicy operation = %v, %v", p, err)
	}
	if err := reader.Set(h.Context(), "app:other", "x", 0).Err(); err != nil {
		t.Errorf("the policy's user SET after the update: %v", err)
	}
	if rev, err := c.GetAclPolicyRevision(h.Context(), &clusterpb.GetAclPolicyRevisionRequest{Name: pol.GetName() + "/revisions/2"}); err != nil || rev.GetRevisionNumber() != 2 {
		t.Errorf("GetAclPolicyRevision = %v, %v", rev, err)
	}
	revs := 0
	for it := c.ListAclPolicyRevisions(h.Context(), &clusterpb.ListAclPolicyRevisionsRequest{Parent: pol.GetName()}); ; revs++ {
		if _, err := it.Next(); errors.Is(err, iterator.Done) {
			break
		} else if err != nil {
			t.Fatalf("ListAclPolicyRevisions: %v", err)
		}
	}
	if revs != 2 {
		t.Errorf("ListAclPolicyRevisions lists %d revisions, want 2", revs)
	}
	if _, err := c.DeleteAclPolicy(h.Context(), &clusterpb.DeleteAclPolicyRequest{Name: pol.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteAclPolicy of an attached policy = %v, want FAILED_PRECONDITION", err)
	}

	// Backups.
	bop, err := c.BackupCluster(h.Context(), &clusterpb.BackupClusterRequest{Name: name, BackupId: proto.String("first")})
	if err != nil {
		t.Fatalf("BackupCluster: %v", err)
	}
	if _, err := bop.Wait(h.Context()); err != nil {
		t.Fatalf("BackupCluster operation: %v", err)
	}
	col, err := c.GetBackupCollection(h.Context(), &clusterpb.GetBackupCollectionRequest{Name: cl.GetBackupCollection()})
	if err != nil || col.GetCluster() != name {
		t.Fatalf("GetBackupCollection = %v, %v", col, err)
	}
	if got, err := c.ListBackupCollections(h.Context(), &clusterpb.ListBackupCollectionsRequest{Parent: parent}).Next(); err != nil || got.GetName() == "" {
		t.Errorf("ListBackupCollections = %v, %v", got, err)
	}
	bname := cl.GetBackupCollection() + "/backups/first"
	b, err := c.GetBackup(h.Context(), &clusterpb.GetBackupRequest{Name: bname})
	if err != nil || b.GetState() != clusterpb.Backup_ACTIVE || len(b.GetBackupFiles()) != 2 || b.GetShardCount() != 2 {
		t.Fatalf("GetBackup = %v, %v", b, err)
	}
	if got, err := c.ListBackups(h.Context(), &clusterpb.ListBackupsRequest{Parent: cl.GetBackupCollection()}).Next(); err != nil || got.GetName() != bname {
		t.Errorf("ListBackups = %v, %v", got, err)
	}
	eop, err := c.ExportBackup(h.Context(), &clusterpb.ExportBackupRequest{Name: bname, Destination: &clusterpb.ExportBackupRequest_GcsBucket{GcsBucket: bh.BucketName()}})
	if err != nil {
		t.Fatalf("ExportBackup: %v", err)
	}
	if _, err := eop.Wait(h.Context()); err != nil {
		t.Fatalf("ExportBackup operation: %v", err)
	}
	for _, f := range b.GetBackupFiles() {
		if _, err := bh.Object("first/" + f.GetFileName()).Attrs(h.Context()); err != nil {
			t.Errorf("exported %s: %v", f.GetFileName(), err)
		}
	}

	rid := fmt.Sprintf("compat-clrst-%d", sfx)
	restored := create(rid, &clusterpb.Cluster{ShardCount: proto.Int32(1), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO,
		ImportSources: &clusterpb.Cluster_ManagedBackupSource_{ManagedBackupSource: &clusterpb.Cluster_ManagedBackupSource{Backup: bname}}})
	if restored.GetManagedBackupSource().GetBackup() != bname {
		t.Errorf("restored cluster's managed_backup_source = %v", restored.GetManagedBackupSource())
	}
	rep := restored.GetDiscoveryEndpoints()[0]
	rrc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{net.JoinHostPort(rep.GetAddress(), strconv.Itoa(int(rep.GetPort())))}})
	defer rrc.Close()
	clusterKeys(t, h, rrc, "tok", 30, false)
	if v, err := rrc.Get(h.Context(), "app:greeting").Result(); err != nil || v != "hello" {
		t.Errorf("restored app:greeting = %q, %v", v, err)
	}

	dop, err := c.DeleteAuthToken(h.Context(), &clusterpb.DeleteAuthTokenRequest{Name: tok.GetName()})
	if err != nil {
		t.Fatalf("DeleteAuthToken: %v", err)
	}
	if err := dop.Wait(h.Context()); err != nil {
		t.Fatalf("DeleteAuthToken operation: %v", err)
	}
	gone := redis.NewClient(&redis.Options{Addr: addr, Username: "app", Password: tok.GetToken(), MaxRetries: -1})
	defer gone.Close()
	if err := gone.Ping(h.Context()).Err(); err == nil || !strings.Contains(err.Error(), "WRONGPASS") {
		t.Errorf("PING with a deleted token = %v, want WRONGPASS", err)
	}
	duop, err := c.DeleteTokenAuthUser(h.Context(), &clusterpb.DeleteTokenAuthUserRequest{Name: user})
	if err != nil {
		t.Fatalf("DeleteTokenAuthUser: %v", err)
	}
	if err := duop.Wait(h.Context()); err != nil {
		t.Fatalf("DeleteTokenAuthUser operation: %v", err)
	}
	delop, err := c.DeleteBackup(h.Context(), &clusterpb.DeleteBackupRequest{Name: bname})
	if err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if err := delop.Wait(h.Context()); err != nil {
		t.Fatalf("DeleteBackup operation: %v", err)
	}
	if _, err := c.GetBackup(h.Context(), &clusterpb.GetBackupRequest{Name: bname}); status.Code(err) != codes.NotFound {
		t.Errorf("GetBackup of a deleted backup = %v, want NOT_FOUND", err)
	}
	if _, err := c.UpdateCluster(h.Context(), &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cross_cluster_replication_config"}},
		Cluster: &clusterpb.Cluster{Name: name}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("UpdateCluster cross_cluster_replication_config = %v, want UNIMPLEMENTED", err)
	}
	// Detach the policy so it can be deleted.
	upd, err := c.UpdateCluster(h.Context(), &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"acl_policy"}},
		Cluster: &clusterpb.Cluster{Name: name}})
	if err != nil {
		t.Fatalf("UpdateCluster acl_policy: %v", err)
	}
	if _, err := upd.Wait(h.Context()); err != nil {
		t.Fatalf("UpdateCluster acl_policy operation: %v", err)
	}
	if err := reader.Ping(h.Context()).Err(); err == nil {
		t.Error("the policy's user still logs in after the policy is detached")
	}
	if op, err := c.DeleteAclPolicy(h.Context(), &clusterpb.DeleteAclPolicyRequest{Name: pol.GetName()}); err != nil {
		t.Errorf("DeleteAclPolicy: %v", err)
	} else if err := op.Wait(h.Context()); err != nil {
		t.Errorf("DeleteAclPolicy operation: %v", err)
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/AddTokenAuthUser, google.cloud.memorystore.v1.Memorystore/AddAuthToken, google.cloud.memorystore.v1.Memorystore/GetAuthToken, google.cloud.memorystore.v1.Memorystore/ListAuthTokens, google.cloud.memorystore.v1.Memorystore/GetTokenAuthUser, google.cloud.memorystore.v1.Memorystore/ListTokenAuthUsers, google.cloud.memorystore.v1.Memorystore/DeleteAuthToken, google.cloud.memorystore.v1.Memorystore/DeleteTokenAuthUser, google.cloud.memorystore.v1.Memorystore/BackupInstance, google.cloud.memorystore.v1.Memorystore/ListBackupCollections, google.cloud.memorystore.v1.Memorystore/GetBackupCollection, google.cloud.memorystore.v1.Memorystore/ListBackups, google.cloud.memorystore.v1.Memorystore/GetBackup, google.cloud.memorystore.v1.Memorystore/ExportBackup, google.cloud.memorystore.v1.Memorystore/DeleteBackup
//
// TestMemorystoreValkeyTokenAuthBackups (#1549), with
// cloud.google.com/go/memorystore/apiv1 and go-redis: a CLUSTER_DISABLED
// instance with TOKEN_AUTH refuses a client without a token; a token auth
// user's generated token writes keys; BackupInstance keeps the primary's
// RDB file; an instance created from it (managed_backup_source, in
// CLUSTER mode) has the keys, and one created from the exported file
// (gcs_source) too. A deleted token no longer logs in.
func TestMemorystoreValkeyTokenAuthBackups(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	sc := instanceStorage(t, h)
	bh := bucket(t, h, sc)
	parent := "projects/" + project + "/locations/us-central1"
	net0 := []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: "projects/" + project + "/global/networks/default"}}
	sfx := time.Now().UnixNano() % 1e8
	create := func(id string, in *memorystorepb.Instance) *memorystorepb.Instance {
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
		inst, err := op.Wait(h.Context())
		if err != nil {
			t.Fatalf("CreateInstance %s operation: %v", id, err)
		}
		return inst
	}
	inst := create(fmt.Sprintf("compat-vktok-%d", sfx), &memorystorepb.Instance{Mode: memorystorepb.Instance_CLUSTER_DISABLED,
		ReplicaCount: proto.Int32(1), NodeType: memorystorepb.Instance_SHARED_CORE_NANO, PscAutoConnections: net0,
		AuthorizationMode: memorystorepb.Instance_TOKEN_AUTH})
	name := inst.GetName()
	if inst.GetAuthorizationMode() != memorystorepb.Instance_TOKEN_AUTH || inst.GetBackupCollection() == "" {
		t.Fatalf("created instance = %v", inst)
	}
	paddr := valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	none := redis.NewClient(&redis.Options{Addr: paddr, MaxRetries: -1})
	defer none.Close()
	if err := none.Set(h.Context(), "x", "1", 0).Err(); err == nil || !strings.Contains(err.Error(), "NOAUTH") {
		t.Errorf("SET without a token = %v, want NOAUTH", err)
	}
	uop, err := c.AddTokenAuthUser(h.Context(), &memorystorepb.AddTokenAuthUserRequest{Instance: name, TokenAuthUser: "app"})
	if err != nil {
		t.Fatalf("AddTokenAuthUser: %v", err)
	}
	if _, err := uop.Wait(h.Context()); err != nil {
		t.Fatalf("AddTokenAuthUser operation: %v", err)
	}
	user := name + "/tokenAuthUsers/app"
	top, err := c.AddAuthToken(h.Context(), &memorystorepb.AddAuthTokenRequest{TokenAuthUser: user, AuthToken: &memorystorepb.AuthToken{}})
	if err != nil {
		t.Fatalf("AddAuthToken: %v", err)
	}
	if _, err := top.Wait(h.Context()); err != nil {
		t.Fatalf("AddAuthToken operation: %v", err)
	}
	if u, err := c.GetTokenAuthUser(h.Context(), &memorystorepb.GetTokenAuthUserRequest{Name: user}); err != nil || u.GetState() != memorystorepb.TokenAuthUser_ACTIVE {
		t.Errorf("GetTokenAuthUser = %v, %v", u, err)
	}
	if u, err := c.ListTokenAuthUsers(h.Context(), &memorystorepb.ListTokenAuthUsersRequest{Parent: name}).Next(); err != nil || u.GetName() != user {
		t.Errorf("ListTokenAuthUsers = %v, %v", u, err)
	}
	tok, err := c.ListAuthTokens(h.Context(), &memorystorepb.ListAuthTokensRequest{Parent: user}).Next()
	if err != nil || tok.GetToken() == "" {
		t.Fatalf("ListAuthTokens = %v, %v", tok, err)
	}
	if got, err := c.GetAuthToken(h.Context(), &memorystorepb.GetAuthTokenRequest{Name: tok.GetName()}); err != nil || got.GetToken() != tok.GetToken() {
		t.Errorf("GetAuthToken = %v, %v", got, err)
	}
	rc := redis.NewClient(&redis.Options{Addr: paddr, Username: "app", Password: tok.GetToken()})
	defer rc.Close()
	for i := 0; i < 20; i++ {
		if err := rc.Set(h.Context(), fmt.Sprintf("vk-%d", i), strconv.Itoa(i), 0).Err(); err != nil {
			t.Fatalf("SET with the token: %v", err)
		}
	}
	reader := redis.NewClient(&redis.Options{Addr: valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_READER), Username: "app", Password: tok.GetToken()})
	defer reader.Close()
	var v string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if v, _ = reader.Get(h.Context(), "vk-3").Result(); v == "3" {
			break
		}
	}
	if v != "3" {
		t.Errorf("the reader, with the token, GET vk-3 = %q, want 3 (replicated with the service's own password)", v)
	}

	bop, err := c.BackupInstance(h.Context(), &memorystorepb.BackupInstanceRequest{Name: name, BackupId: proto.String("first")})
	if err != nil {
		t.Fatalf("BackupInstance: %v", err)
	}
	if _, err := bop.Wait(h.Context()); err != nil {
		t.Fatalf("BackupInstance operation: %v", err)
	}
	if col, err := c.GetBackupCollection(h.Context(), &memorystorepb.GetBackupCollectionRequest{Name: inst.GetBackupCollection()}); err != nil ||
		col.GetInstance() != name || col.GetTotalBackupCount() != 1 {
		t.Errorf("GetBackupCollection = %v, %v", col, err)
	}
	if got, err := c.ListBackupCollections(h.Context(), &memorystorepb.ListBackupCollectionsRequest{Parent: parent}).Next(); err != nil || got.GetName() == "" {
		t.Errorf("ListBackupCollections = %v, %v", got, err)
	}
	bname := inst.GetBackupCollection() + "/backups/first"
	b, err := c.GetBackup(h.Context(), &memorystorepb.GetBackupRequest{Name: bname})
	if err != nil || len(b.GetBackupFiles()) != 1 || b.GetNodeType() != memorystorepb.Instance_SHARED_CORE_NANO {
		t.Fatalf("GetBackup = %v, %v", b, err)
	}
	if got, err := c.ListBackups(h.Context(), &memorystorepb.ListBackupsRequest{Parent: inst.GetBackupCollection()}).Next(); err != nil || got.GetName() != bname {
		t.Errorf("ListBackups = %v, %v", got, err)
	}
	eop, err := c.ExportBackup(h.Context(), &memorystorepb.ExportBackupRequest{Name: bname, Destination: &memorystorepb.ExportBackupRequest_GcsBucket{GcsBucket: bh.BucketName()}})
	if err != nil {
		t.Fatalf("ExportBackup: %v", err)
	}
	if _, err := eop.Wait(h.Context()); err != nil {
		t.Fatalf("ExportBackup operation: %v", err)
	}
	restored := create(fmt.Sprintf("compat-vkrst-%d", sfx), &memorystorepb.Instance{ShardCount: 2, NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		ImportSources: &memorystorepb.Instance_ManagedBackupSource_{ManagedBackupSource: &memorystorepb.Instance_ManagedBackupSource{Backup: bname}}})
	ep := restored.GetDiscoveryEndpoints()[0]
	rrc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{net.JoinHostPort(ep.GetAddress(), strconv.Itoa(int(ep.GetPort())))}})
	defer rrc.Close()
	clusterKeys(t, h, rrc, "vk", 20, false)
	fromGCS := create(fmt.Sprintf("compat-vkgcs-%d", sfx), &memorystorepb.Instance{Mode: memorystorepb.Instance_CLUSTER_DISABLED,
		NodeType: memorystorepb.Instance_SHARED_CORE_NANO, PscAutoConnections: net0,
		ImportSources: &memorystorepb.Instance_GcsSource{GcsSource: &memorystorepb.Instance_GcsBackupSource{
			Uris: []string{"gs://" + bh.BucketName() + "/first/" + b.GetBackupFiles()[0].GetFileName()}}}})
	grc := redis.NewClient(&redis.Options{Addr: valkeyEndpoint(t, fromGCS, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)})
	defer grc.Close()
	if v, err := grc.Get(h.Context(), "vk-7").Result(); err != nil || v != "7" {
		t.Errorf("instance from gcs_source GET vk-7 = %q, %v", v, err)
	}

	dop, err := c.DeleteAuthToken(h.Context(), &memorystorepb.DeleteAuthTokenRequest{Name: tok.GetName()})
	if err != nil {
		t.Fatalf("DeleteAuthToken: %v", err)
	}
	if err := dop.Wait(h.Context()); err != nil {
		t.Fatalf("DeleteAuthToken operation: %v", err)
	}
	gone := redis.NewClient(&redis.Options{Addr: paddr, Username: "app", Password: tok.GetToken(), MaxRetries: -1})
	defer gone.Close()
	if err := gone.Ping(h.Context()).Err(); err == nil || !strings.Contains(err.Error(), "WRONGPASS") {
		t.Errorf("PING with a deleted token = %v, want WRONGPASS", err)
	}
	duop, err := c.DeleteTokenAuthUser(h.Context(), &memorystorepb.DeleteTokenAuthUserRequest{Name: user})
	if err != nil {
		t.Fatalf("DeleteTokenAuthUser: %v", err)
	}
	if err := duop.Wait(h.Context()); err != nil {
		t.Fatalf("DeleteTokenAuthUser operation: %v", err)
	}
	delop, err := c.DeleteBackup(h.Context(), &memorystorepb.DeleteBackupRequest{Name: bname})
	if err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if err := delop.Wait(h.Context()); err != nil {
		t.Fatalf("DeleteBackup operation: %v", err)
	}
	if _, err := c.CreateInstance(h.Context(), &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: fmt.Sprintf("compat-vkccr-%d", sfx),
		Instance: &memorystorepb.Instance{ShardCount: 1, CrossInstanceReplicationConfig: &memorystorepb.CrossInstanceReplicationConfig{
			InstanceRole: memorystorepb.CrossInstanceReplicationConfig_SECONDARY}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("CreateInstance with cross-instance replication = %v, want UNIMPLEMENTED", err)
	}
}
