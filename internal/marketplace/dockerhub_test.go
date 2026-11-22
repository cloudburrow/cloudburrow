package marketplace_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	mp "github.com/cloudburrow/cloudburrow/internal/marketplace"
	"github.com/cloudburrow/cloudburrow/internal/marketplace/marketplacetest"
)

func TestDockerHubSearch(t *testing.T) {
	h, rec := marketplacetest.DockerHub(t)
	r, err := h.Search(context.Background(), mp.Query{Text: "nginx", Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if r.Total < 1000 || !r.HasMore {
		t.Errorf("total %d, hasMore %v: want Docker Hub's count and more pages", r.Total, r.HasMore)
	}
	var official, verified *mp.Summary
	for i, s := range r.Items {
		if s.ID == "dhi/nginx" {
			t.Errorf("a Docker Hardened Image (type dhi, pulled from dhi.io with sign-in) is listed: %+v", s)
		}
		if s.ID == "library/nginx" {
			official = &r.Items[i]
		}
		if s.Badge == "verified_publisher" {
			verified = &r.Items[i]
		}
	}
	if official == nil || official.Badge != "official" || official.Publisher != "Docker" || official.Pulls == "" || official.Type != mp.TypeContainerImage {
		t.Errorf("library/nginx: %+v", official)
	}
	if verified == nil || verified.Publisher == "" {
		t.Errorf("no verified publisher among %+v", r.Items)
	}
	if !strings.Contains(rec.URLs[0], "type=image") || !strings.Contains(rec.URLs[0], "query=nginx") {
		t.Errorf("search URL %s", rec.URLs[0])
	}

	f, err := h.Search(context.Background(), mp.Query{Badges: []string{"official"}, Categories: []string{"databases-and-storage"}, Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	last := rec.URLs[len(rec.URLs)-1]
	if !strings.Contains(last, "badges=official") || !strings.Contains(last, "categories=databases-and-storage") {
		t.Errorf("filtered search URL %s", last)
	}
	for _, s := range f.Items {
		if s.Badge != "official" {
			t.Errorf("filtered result %s has badge %q", s.ID, s.Badge)
		}
	}

	lim, err := h.Search(context.Background(), mp.Query{Page: 41, Size: 25})
	if err != nil || lim.Limit == "" || len(lim.Items) != 0 {
		t.Errorf("past the anonymous paging limit: %+v, %v", lim, err)
	}
}

func TestDockerHubProductAndConfig(t *testing.T) {
	h, rec := marketplacetest.DockerHub(t)
	ctx := context.Background()
	p, err := h.Product(ctx, "library/nginx")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "nginx" || p.Badge != "official" || p.Pull != "nginx" || !strings.Contains(p.Overview, "# Quick reference") ||
		len(p.Versions) != 3 || p.VersionsTotal < 100 || len(p.Documentation) == 0 {
		t.Errorf("product: name %q badge %q pull %q versions %d/%d docs %v", p.Name, p.Badge, p.Pull, len(p.Versions), p.VersionsTotal, p.Documentation)
	}
	if _, err := h.Product(ctx, "nobody/nothing"); !errors.Is(err, mp.ErrNotFound) {
		t.Errorf("missing repository: %v", err)
	}
	if _, err := h.Product(ctx, "../etc"); !errors.Is(err, mp.ErrBadRequest) {
		t.Errorf("bad id: %v", err)
	}

	cfg, err := h.Config(ctx, "library/nginx", "alpine")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ExposedPorts) != 1 || cfg.ExposedPorts[0] != "80/tcp" || cfg.Arch != "arm64" || len(cfg.Env) == 0 {
		t.Errorf("config %+v", cfg)
	}
	for _, a := range rec.Auth {
		if a != "Bearer fixture-token" {
			t.Errorf("registry request authorization %q, want the anonymous pull token", a)
		}
	}
	ps, err := h.Parameters(ctx, "library/nginx", "alpine")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range ps {
		got[p.Name] = p.Default
	}
	if got["ports"] != "80/tcp" || got["tag"] != "alpine" || got["replicas"] != "1" {
		t.Errorf("parameters %v", got)
	}
}

func TestDockerHubPlan(t *testing.T) {
	h := &mp.DockerHub{}
	plan, err := h.Plan(context.Background(), "library/nginx", "alpine", mp.AppMeta{Name: "web-1", Namespace: "shop"},
		map[string]string{"tag": "alpine", "replicas": "2", "ports": "80/tcp, 9113/udp", "serviceType": "Node port",
			"env": "A=1\nB=two=2", "cpuRequest": "100m", "memoryLimit": "256Mi", "args": "-g daemon"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Manifests) != 2 || len(plan.Kinds) != 2 {
		t.Fatalf("plan %+v", plan)
	}
	b, _ := json.Marshal(plan.Manifests)
	s := string(b)
	for _, want := range []string{`"image":"docker.io/library/nginx:alpine"`, `"replicas":2`, `"containerPort":9113`, `"protocol":"UDP"`,
		`"type":"NodePort"`, `"name":"B","value":"two=2"`, `"cpu":"100m"`, `"memory":"256Mi"`, `"app.kubernetes.io/name":"web-1"`, `"args":["-g","daemon"]`} {
		if !strings.Contains(s, want) {
			t.Errorf("manifests lack %s: %s", want, s)
		}
	}
	for _, bad := range []map[string]string{{"ports": "99999"}, {"ports": "80/xyz"}, {"env": "1BAD=x"}, {"replicas": "-1"},
		{"serviceType": "Ingress"}, {"cpuRequest": "lots"}, {"tag": "a b"}} {
		if _, err := h.Plan(context.Background(), "library/nginx", "alpine", mp.AppMeta{Name: "a", Namespace: "b"}, bad); !errors.Is(err, mp.ErrBadRequest) {
			t.Errorf("%v: %v, want a bad request", bad, err)
		}
	}
	none, err := h.Plan(context.Background(), "library/busybox", "latest", mp.AppMeta{Name: "a", Namespace: "b"}, map[string]string{})
	if err != nil || len(none.Manifests) != 1 {
		t.Errorf("an image with no ports has no Service: %+v %v", none, err)
	}
}

func TestCatalogSearchJoinsSources(t *testing.T) {
	h, _ := marketplacetest.DockerHub(t)
	c := mp.NewCatalog(h)
	if _, err := c.Search(context.Background(), []string{"nope"}, mp.Query{}); !errors.Is(err, mp.ErrNotFound) {
		t.Errorf("unknown source: %v", err)
	}
	r, err := c.Search(context.Background(), nil, mp.Query{Text: "nginx"})
	if err != nil || len(r.Items) == 0 || r.Items[0].SourceLabel != "Docker Hub" {
		t.Errorf("catalog search %+v %v", r, err)
	}
	if got := c.ByType(mp.TypeKubernetesApp); len(got) != 0 {
		t.Errorf("Docker Hub is not a source of Kubernetes apps: %v", got)
	}
	cats, err := c.Categories(context.Background())
	if err != nil || len(cats) < 5 {
		t.Errorf("categories %v %v", cats, err)
	}
}

// TestDockerHubLive reads the real Docker Hub, anonymously, when
// CLOUDBURROW_MARKETPLACE_LIVE=1; CI runs the fixture.
func TestDockerHubLive(t *testing.T) {
	if os.Getenv("CLOUDBURROW_MARKETPLACE_LIVE") != "1" {
		t.Skip("set CLOUDBURROW_MARKETPLACE_LIVE=1 to read the real Docker Hub")
	}
	h := &mp.DockerHub{}
	ctx := context.Background()
	r, err := h.Search(ctx, mp.Query{Text: "nginx", Size: 5})
	if err != nil || len(r.Items) == 0 {
		t.Fatalf("search: %+v %v", r, err)
	}
	cfg, err := h.Config(ctx, "library/nginx", "alpine")
	if err != nil || len(cfg.ExposedPorts) == 0 {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	p, err := h.Product(ctx, "library/nginx")
	if err != nil || p.Overview == "" || len(p.Versions) == 0 {
		t.Fatalf("product: %v", err)
	}
}
