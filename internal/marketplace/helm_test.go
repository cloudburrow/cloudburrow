package marketplace_test

import (
	"context"
	"os"
	"strings"
	"testing"

	mp "github.com/cloudburrow/cloudburrow/internal/marketplace"
	"github.com/cloudburrow/cloudburrow/internal/marketplace/marketplacetest"
)

func TestArtifactHubSearchAndProduct(t *testing.T) {
	ctx := context.Background()
	cs := marketplacetest.ChartServer(t)
	ah := cs.ArtifactHub()
	res, err := ah.Search(ctx, mp.Query{Text: "nginx", Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 336 || len(res.Items) != 3 || !res.HasMore {
		t.Fatalf("results %+v", res)
	}
	first := res.Items[0]
	if first.ID != "bitnami/nginx" || first.Source != "artifacthub" || first.Type != mp.TypeKubernetesApp ||
		first.Badge != "verified_publisher" || first.Publisher != "Bitnami" || !strings.HasPrefix(first.Logo, cs.URL+"/image/") {
		t.Errorf("first %+v", first)
	}
	if res, err := ah.Search(ctx, mp.Query{Text: "nginx", Badges: []string{"open_source"}}); err != nil || len(res.Items) != 0 {
		t.Errorf("an unknown badge filter: %+v %v", res, err)
	}
	p, err := ah.Product(ctx, "bitnami/nginx")
	if err != nil {
		t.Fatal(err)
	}
	if p.Overview == "" || len(p.Versions) != 25 || p.VersionsTotal != 30 || p.Versions[0].Name != "25.2.16" {
		t.Errorf("product versions %d/%d %+v", len(p.Versions), p.VersionsTotal, p.Versions[:1])
	}
	if _, err := ah.Product(ctx, "../etc"); err == nil {
		t.Error("a bad id is accepted")
	}
}

func TestArtifactHubParametersAndPlan(t *testing.T) {
	ctx := context.Background()
	ah := marketplacetest.ChartServer(t).ArtifactHub()
	params, err := ah.Parameters(ctx, "bitnami/nginx", "")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]mp.Param{}
	for _, p := range params {
		byName[p.Name] = p
	}
	if p := byName["v.replicaCount"]; p.Label != "Replicas" || p.Default != "1" || p.Type != "number" {
		t.Errorf("replicaCount %+v", p)
	}
	if p := byName["v.service.type"]; p.Type != "select" || len(p.Options) != 3 || p.Default != "ClusterIP" {
		t.Errorf("service.type %+v", p)
	}
	if p := byName["values"]; p.Type != "yaml" || !strings.Contains(p.Default, "replicaCount: 1") {
		t.Errorf("values %+v", p)
	}
	if byName["chartVersion"].Default != "25.2.16" {
		t.Errorf("chartVersion %+v", byName["chartVersion"])
	}
	plan, err := ah.Plan(ctx, "bitnami/nginx", "", mp.AppMeta{Name: "web", Namespace: "default"}, map[string]string{
		"values":         "replicaCount: 1\nimage:\n  repository: nginx\n  tag: alpine\nservice:\n  type: ClusterIP\n  port: 80\n",
		"v.replicaCount": "3", "v.service.type": "NodePort", "v.image.tag": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Chart == nil || plan.Chart.Chart.Metadata.Name != "hello" {
		t.Fatalf("plan %+v", plan)
	}
	v := plan.Chart.Values
	if v["replicaCount"] != int64(3) || v["service"].(map[string]any)["type"] != "NodePort" || v["image"].(map[string]any)["tag"] != "alpine" {
		t.Errorf("values %#v", v)
	}
	if plan.Annotations[mp.AnnotationSource] != "artifacthub" || plan.Annotations[mp.AnnotationProduct] != "bitnami/nginx" {
		t.Errorf("annotations %v", plan.Annotations)
	}
	if _, err := ah.Plan(ctx, "bitnami/nginx", "", mp.AppMeta{}, map[string]string{"v.replicaCount": "three"}); err == nil {
		t.Error("a non-number is accepted")
	}
	if _, err := ah.Plan(ctx, "bitnami/nginx", "", mp.AppMeta{}, map[string]string{"values": "a: [b"}); err == nil {
		t.Error("bad YAML is accepted")
	}
}

func TestHelmIndexSource(t *testing.T) {
	ctx := context.Background()
	src := marketplacetest.ChartServer(t).Repo("rancher", "Rancher")
	res, err := src.Search(ctx, mp.Query{Text: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].ID != "fixture/hello" || res.Items[0].Name != "Hello" || res.Items[0].Badge != "verified_publisher" ||
		res.Items[0].SourceLabel != "Rancher" {
		t.Fatalf("results %+v", res)
	}
	if res, _ := src.Search(ctx, mp.Query{}); res.Total != 1 {
		t.Errorf("the deprecated chart is listed: %+v", res)
	}
	p, err := src.Product(ctx, "fixture/hello")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Overview, "A small web server.") || len(p.Versions) != 1 {
		t.Errorf("product %+v", p)
	}
	plan, err := src.Plan(ctx, "fixture/hello", "0.1.0", mp.AppMeta{}, map[string]string{"v.replicaCount": "2"})
	if err != nil || plan.Chart.Values["replicaCount"] != int64(2) {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if _, err := src.Plan(ctx, "fixture/hello", "9.9.9", mp.AppMeta{}, nil); err == nil {
		t.Error("an unknown version is accepted")
	}
}

func TestUserRepos(t *testing.T) {
	u := &mp.UserRepos{File: t.TempDir() + "/repos.json"}
	if len(u.List()) != 0 {
		t.Fatal("not empty")
	}
	for _, bad := range [][2]string{{"Bad Name", "https://x.example"}, {"ok", "http://x.example"}, {"ok", "https://user:pw@x.example"}} {
		if err := u.Add(bad[0], bad[1]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if err := u.Add("bitnami", "https://charts.bitnami.com/bitnami/"); err != nil {
		t.Fatal(err)
	}
	if err := u.Add("bitnami", "https://charts.bitnami.com/bitnami"); err == nil {
		t.Error("a duplicate is accepted")
	}
	if l := u.List(); len(l) != 1 || l[0].URL != "https://charts.bitnami.com/bitnami" {
		t.Errorf("list %+v", l)
	}
	if err := u.Remove("bitnami"); err != nil || len(u.List()) != 0 {
		t.Errorf("remove: %v %+v", err, u.List())
	}
}

// TestHelmSourcesLive reads Artifact Hub and Rancher's charts repository;
// it runs only with CLOUDBURROW_MARKETPLACE_LIVE=1.
func TestHelmSourcesLive(t *testing.T) {
	if os.Getenv("CLOUDBURROW_MARKETPLACE_LIVE") != "1" {
		t.Skip("set CLOUDBURROW_MARKETPLACE_LIVE=1 to read Artifact Hub and Rancher's chart repositories")
	}
	ctx := context.Background()
	ah := &mp.ArtifactHub{}
	res, err := ah.Search(ctx, mp.Query{Text: "nginx", Size: 5})
	if err != nil || res.Total == 0 || len(res.Items) == 0 {
		t.Fatalf("Artifact Hub: %+v %v", res, err)
	}
	params, err := ah.Parameters(ctx, res.Items[0].ID, "")
	if err != nil || len(params) < 2 {
		t.Fatalf("Artifact Hub parameters: %v %v", params, err)
	}
	rancher := &mp.HelmIndex{SourceID: "rancher", SourceLabel: "Rancher", Repos: func() []mp.HelmRepo { return mp.RancherRepos[:1] }}
	rr, err := rancher.Search(ctx, mp.Query{Text: "monitoring"})
	if err != nil || len(rr.Items) == 0 {
		t.Fatalf("Rancher: %+v %v", rr, err)
	}
}
