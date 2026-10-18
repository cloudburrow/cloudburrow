package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// TestConsoleRedisClusterServerCA (#1591): a cluster created on the
// Memorystore Redis Cluster page with Enable in-transit encryption and IAM
// authentication shows both modes and a Server CA tab with the CA
// GetClusterCertificateAuthority returns; a cluster without in-transit
// encryption has no such tab, as its CA is FAILED_PRECONDITION.
func TestConsoleRedisClusterServerCA(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		id  string
		tls bool
	}{{"cl-tls", true}, {"cl-plain", false}} {
		// A server of its own each: the fake counts every running node as
		// known, so a second cluster on the same nodes would never form.
		srv := redisadmin.NewServer(redisadmin.Instance{Project: "p", Host: "127.0.0.1", Port: 9016, Created: time.Unix(100, 0)}, valkeyNoBackend{})
		srv.EnableCreate(ctx, valkeyNoProv{}, func(string, string, string) redisadmin.DataPlane { return nil }, nil, nil)
		nodes := &fakeValkeyNodes{running: map[string]string{}}
		srv.EnableClusters(ctx, nodes, func(addr, _, _ string) redisadmin.ClusterNode { return fakeValkeyNode{nodes, addr} }, nil)
		cp := redisClusterProvider{svc: &memorystoreAdminService{api: srv}}
		if _, err := cp.Create(ctx, "p", map[string]string{"id": tc.id, "region": redisadmin.Region, "shardCount": "1",
			"nodeType": "REDIS_SHARED_CORE_NANO", "transitEncryption": boolWord(tc.tls), "iamAuth": boolWord(tc.tls)}); err != nil {
			t.Fatalf("Create cluster %s: %v", tc.id, err)
		}
		d, err := cp.Detail(ctx, "p", []string{tc.id})
		if err != nil || d.Unavailable != "" {
			t.Fatalf("Detail %s = %+v, %v", tc.id, d, err)
		}
		props := map[string]string{}
		for _, pr := range d.Sections[0].Groups[0].Properties {
			props[pr.Label] = pr.Value
		}
		ca := sectionByID(d, "server-ca")
		if tc.tls {
			if ca == nil || ca.Label != "Server CA" || !strings.Contains(ca.Text, "BEGIN CERTIFICATE") {
				t.Errorf("%s with in-transit encryption: Server CA tab = %+v", tc.id, ca)
			}
			if !strings.Contains(props["In-transit encryption"], "SERVER_AUTHENTICATION") || !strings.Contains(props["Authorization"], "IAM_AUTH") {
				t.Errorf("%s with in-transit encryption and IAM auth: page shows %v", tc.id, props)
			}
		} else if ca != nil {
			t.Errorf("%s without in-transit encryption has a Server CA tab", tc.id)
		}
	}
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
