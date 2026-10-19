//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestConsoleRedisCluster (#1519): Create cluster on the Memorystore Redis
// Cluster page forms a cluster through CreateCluster, which the official
// client reads back and a go-redis ClusterClient reaches at the discovery
// endpoint its page shows; Edit adds a shard through UpdateCluster, with
// the keys kept; Delete removes it through DeleteCluster.
func TestConsoleRedisCluster(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c, project := redisClusterAdminClient(t, h)
	id := fmt.Sprintf("console-cl-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + project + "/locations/us-central1/clusters/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: name}); err == nil {
			_ = waitLRODone(h.Context(), op)
		}
	})
	q := "?project=" + url.QueryEscape(project)
	body, _ := json.Marshal(map[string]string{"id": id, "region": "us-central1", "shardCount": "1", "replicaCount": "0",
		"nodeType": "REDIS_SHARED_CORE_NANO", "redisConfigs": `{"maxmemory-policy":"allkeys-lru"}`})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/memorystore-clusters"+q, string(body)); code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	cl, err := c.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: name})
	if err != nil || cl.GetState() != clusterpb.Cluster_ACTIVE || cl.GetShardCount() != 1 || cl.GetRedisConfigs()["maxmemory-policy"] != "allkeys-lru" {
		t.Fatalf("created from the console: %v, %v", cl, err)
	}
	code, out := consoleDo(t, addr, http.MethodGet, "/api/resources/memorystore-clusters"+q, "")
	if code != http.StatusOK || !strings.Contains(out, id) {
		t.Errorf("cluster list = %d: %s", code, out)
	}
	var page struct {
		Sections []struct {
			Groups []struct {
				Properties []struct{ Label, Value string }
			}
		}
		Edit *struct{ Label string }
	}
	code, out = consoleDo(t, addr, http.MethodGet, "/api/detail/memorystore-clusters"+q+"&name="+id, "")
	if code != http.StatusOK {
		t.Fatalf("cluster page = %d: %s", code, out)
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatal(err)
	}
	props := map[string]string{}
	for _, s := range page.Sections {
		for _, g := range s.Groups {
			for _, p := range g.Properties {
				props[p.Label] = p.Value
			}
		}
	}
	if page.Edit == nil || props["Shards"] != "1" {
		t.Fatalf("cluster page = %s", out)
	}
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{props["Discovery endpoint"]}})
	defer rc.Close()
	clusterKeys(t, h, rc, "console", 20, true)

	if code, out := consoleEdit(t, addr, "memorystore-clusters", project, []string{id}, map[string]string{
		"shardCount": "2", "replicaCount": "0", "nodeType": "REDIS_SHARED_CORE_NANO"}); code != http.StatusOK {
		t.Fatalf("console edit = %d: %s", code, out)
	}
	if cl, err := c.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: name}); err != nil || cl.GetShardCount() != 2 {
		t.Errorf("after Edit, GetCluster = %v, %v", cl, err)
	}
	rc.ReloadState(h.Context())
	clusterKeys(t, h, rc, "console", 20, false)

	if code, out := consoleDo(t, addr, http.MethodDelete, "/api/resources/memorystore-clusters"+q+"&name="+id, ""); code != http.StatusOK {
		t.Fatalf("console delete = %d: %s", code, out)
	}
	if _, err := c.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetCluster after Delete = %v, want NOT_FOUND", err)
	}
}
