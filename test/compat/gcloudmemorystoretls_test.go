//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
)

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.memorystore.v1.Memorystore/CreateInstance
//
// TestGcloudMemorystoreInTransitEncryption (#1549): through the
// configuration `cloudburrow gcloud-setup` writes, the real gcloud creates a
// Redis cluster with --transit-encryption-mode=server-authentication and
// --auth-mode=iam-auth, and a Memorystore for Valkey instance with
// --transit-encryption-mode=server-authentication and
// --authorization-mode=iam-auth; describe reads both modes back, and the
// official clients read back each one's CA.
func TestGcloudMemorystoreInTransitEncryption(t *testing.T) {
	h := New(t)
	rc, _ := redisClusterAdminClient(t, h) // skips without the admin API
	vc, _ := valkeyAdminClient(t, h)
	g := newGcloudSession(t, h)
	sfx := time.Now().UnixNano() % 1e8
	p := "--project=" + g.project

	cid := fmt.Sprintf("gcltls-%d", sfx)
	cname := "projects/" + g.project + "/locations/us-central1/clusters/" + cid
	t.Cleanup(func() {
		if op, err := rc.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: cname}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	g.must("redis", "clusters", "create", cid, p, "--region=us-central1", "--shard-count=1", "--replica-count=0",
		"--node-type=redis-shared-core-nano", "--network=projects/"+g.project+"/global/networks/default",
		"--transit-encryption-mode=server-authentication", "--auth-mode=iam-auth")
	if got := g.must("redis", "clusters", "describe", cid, p, "--region=us-central1", "--format=value(transitEncryptionMode,authorizationMode)"); !strings.Contains(got, "TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION\tAUTH_MODE_IAM_AUTH") {
		t.Errorf("redis clusters describe = %q", got)
	}
	if ca, err := rc.GetClusterCertificateAuthority(h.Context(), &clusterpb.GetClusterCertificateAuthorityRequest{Name: cname + "/certificateAuthority"}); err != nil ||
		len(ca.GetManagedServerCa().GetCaCerts()) != 1 {
		t.Errorf("GetClusterCertificateAuthority = %v, %v", ca, err)
	}

	vid := fmt.Sprintf("gvktls-%d", sfx)
	vname := "projects/" + g.project + "/locations/us-central1/instances/" + vid
	t.Cleanup(func() {
		if op, err := vc.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: vname}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	g.must("memorystore", "instances", "create", vid, p, "--location=us-central1", "--shard-count=1", "--replica-count=0",
		"--node-type=shared-core-nano", "--transit-encryption-mode=server-authentication", "--authorization-mode=iam-auth",
		"--psc-auto-connections=network=projects/"+g.project+"/global/networks/default,projectId="+g.project)
	if got := g.must("memorystore", "instances", "describe", vid, p, "--location=us-central1", "--format=value(transitEncryptionMode,authorizationMode)"); !strings.Contains(got, "SERVER_AUTHENTICATION\tIAM_AUTH") {
		t.Errorf("memorystore instances describe = %q", got)
	}
	if ca, err := vc.GetCertificateAuthority(h.Context(), &memorystorepb.GetCertificateAuthorityRequest{Name: vname + "/certificateAuthority"}); err != nil ||
		len(ca.GetManagedServerCa().GetCaCerts()) != 1 {
		t.Errorf("GetCertificateAuthority = %v, %v", ca, err)
	}
}
