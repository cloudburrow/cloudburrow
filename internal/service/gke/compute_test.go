package gke

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
)

// TestNodePoolInstanceGroups (#1913): a pool names one instance group
// manager per zone, whose targetSize the port answers under /compute/v1/
// (Terraform's node_count), following a resize; any other Compute call is
// 501 and an unknown group 404. The cluster keeps its initial_node_count
// and names its network and subnetwork in full.
func TestNodePoolInstanceGroups(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t, filepath.Join(t.TempDir(), "gke.json"))
	parent := "projects/p1/locations/us-central1-a"
	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "c1", InitialNodeCount: 2}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	c, _ := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/c1"})
	if c.GetInitialNodeCount() != 2 {
		t.Errorf("initial_node_count = %d, want 2 as given", c.GetInitialNodeCount())
	}
	if c.GetNetworkConfig().GetNetwork() != "projects/p1/global/networks/default" ||
		c.GetNetworkConfig().GetSubnetwork() != "projects/p1/regions/us-central1/subnetworks/default" {
		t.Errorf("networkConfig = %v", c.GetNetworkConfig())
	}
	urls := c.GetNodePools()[0].GetInstanceGroupUrls()
	if len(urls) != 1 || !strings.HasPrefix(urls[0], "https://www.googleapis.com/compute/v1/projects/p1/zones/us-central1-a/instanceGroupManagers/gke-c1-default-pool-") {
		t.Fatalf("instanceGroupUrls = %v", urls)
	}
	h := NewRESTHandler(s)
	get := func(path string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	path := strings.TrimPrefix(urls[0], "https://www.googleapis.com")
	code, igm := get(path)
	if code != http.StatusOK || igm["targetSize"] != float64(2) || igm["kind"] != "compute#instanceGroupManager" {
		t.Fatalf("GET %s = %d %v", path, code, igm)
	}
	op, err = s.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: parent + "/clusters/c1/nodePools/default-pool", NodeCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	if _, igm = get(path); igm["targetSize"] != float64(1) {
		t.Errorf("targetSize after the resize = %v", igm["targetSize"])
	}
	if code, _ := get("/compute/v1/projects/p1/zones/us-central1-a/instanceGroupManagers/nope"); code != http.StatusNotFound {
		t.Errorf("an unknown group = %d, want 404", code)
	}
	if code, _ := get("/compute/v1/projects/p1/zones/us-central1-a/instances"); code != http.StatusNotImplemented {
		t.Errorf("another Compute call = %d, want 501", code)
	}
	// The API itself is still served beside it.
	if code, _ := get("/v1/" + parent + "/clusters/c1"); code != http.StatusOK {
		t.Errorf("GET the cluster over JSON = %d", code)
	}
}

// TestNodePoolInstanceGroupList: instanceGroupManagers.list in a zone,
// filtered by name as the provider asks, finds the pool's group.
func TestNodePoolInstanceGroupList(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t, filepath.Join(t.TempDir(), "gke.json"))
	parent := "projects/p1/locations/us-central1-a"
	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{Name: "c1", InitialNodeCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p1", "us-central1-a", op)
	c, _ := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/c1"})
	url := c.GetNodePools()[0].GetInstanceGroupUrls()[0]
	name := url[strings.LastIndex(url, "/")+1:]
	h := NewRESTHandler(s)
	for _, filter := range []string{"", "name = " + name, `name = "` + name + `"`, "name eq " + name} {
		rec := httptest.NewRecorder()
		u := "/compute/v1/projects/p1/zones/us-central1-a/instanceGroupManagers"
		if filter != "" {
			u += "?filter=" + strings.ReplaceAll(strings.ReplaceAll(filter, " ", "%20"), `"`, "%22")
		}
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u, nil))
		var l struct{ Items []struct{ Name string } }
		if err := json.Unmarshal(rec.Body.Bytes(), &l); rec.Code != http.StatusOK || err != nil || len(l.Items) != 1 || l.Items[0].Name != name {
			t.Errorf("list with %q = %d %s", filter, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/compute/v1/projects/p1/zones/us-central1-a/instanceGroupManagers?filter=name%20%3D%20other", nil))
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("a filter matching nothing = %s", rec.Body)
	}
}
