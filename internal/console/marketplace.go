package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/marketplace"
)

// Marketplace (#2149): the catalogue's search, product pages and Configure
// step, Deploy onto a cluster, and Kubernetes Engine's Applications page,
// as Google Cloud Marketplace's Kubernetes apps are deployed
// (https://docs.cloud.google.com/kubernetes-engine/docs/how-to/deploying-marketplace-app):
// Configure, select the cluster (or Create a new cluster), select or create
// a Namespace, enter an App instance name and the app's parameters, Deploy,
// then the app on the Applications page.

// MarketCluster is a cluster the Configure page offers.
type MarketCluster struct {
	Name     string `json:"name"`
	Location string `json:"location"`
	Status   string `json:"status"`
	// Own marks the instance's own kind cluster, whose objects the
	// Workloads and Services pages show.
	Own        bool   `json:"own"`
	Kubeconfig string `json:"-"`
}

// MarketClusters are the clusters an app can be deployed to: the
// instance's own and the project's Kubernetes Engine clusters.
type MarketClusters interface {
	Clusters(ctx context.Context, project string) ([]MarketCluster, error)
	// CreateCluster is the Configure page's Create a new cluster; an
	// instance that serves no Kubernetes Engine API returns why not.
	CreateCluster(ctx context.Context, project, zone, name string) error
	// CanCreate is "" when CreateCluster works, else why not.
	CanCreate() string
}

// Marketplace is the console's Marketplace.
type Marketplace struct {
	Catalog  *marketplace.Catalog
	Kube     marketplace.Kube
	Clusters MarketClusters
}

// SetMarketplace offers the Marketplace and the Applications page; nil
// offers neither.
func (s *Server) SetMarketplace(m *Marketplace) { s.market = m }

func (s *Server) marketplaceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/marketplace/sources", s.handleMarketSources)
	mux.HandleFunc("GET /api/marketplace/search", s.handleMarketSearch)
	mux.HandleFunc("GET /api/marketplace/product", s.handleMarketProduct)
	mux.HandleFunc("GET /api/marketplace/versions", s.handleMarketVersions)
	mux.HandleFunc("GET /api/marketplace/parameters", s.handleMarketParameters)
	mux.HandleFunc("GET /api/marketplace/clusters", s.handleMarketClusters)
	mux.HandleFunc("POST /api/marketplace/clusters", s.handleMarketCreateCluster)
	mux.HandleFunc("GET /api/marketplace/namespaces", s.handleMarketNamespaces)
	mux.HandleFunc("POST /api/marketplace/deploy", s.handleMarketDeploy)
	mux.HandleFunc("GET /api/marketplace/applications", s.handleMarketApplications)
	mux.HandleFunc("GET /api/marketplace/application", s.handleMarketApplication)
	mux.HandleFunc("POST /api/marketplace/application/delete", s.handleMarketDeleteApplication)
	mux.HandleFunc("GET /api/marketplace/logo", s.handleMarketLogo)
}

// logoCache holds the logos read, by URL.
type logoCache struct {
	mu    sync.Mutex
	items map[string]logoItem
}

type logoItem struct {
	typ  string
	body []byte
}

var marketLogos = &logoCache{items: map[string]logoItem{}}

// handleMarketLogo serves a product's logo through the console's own
// origin (its policy loads no remote image): only from the hosts a source
// names in marketplace.LogoHosts, only raster images, at most 512 KB.
func (s *Server) handleMarketLogo(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("u")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !marketplace.LogoHosts[u.Hostname()] || u.Port() != "" || u.User != nil {
		http.Error(w, "not a Marketplace logo", http.StatusBadRequest)
		return
	}
	marketLogos.mu.Lock()
	it, ok := marketLogos.items[raw]
	marketLogos.mu.Unlock()
	if !ok {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		req.Header.Set("User-Agent", "cloudburrow-marketplace")
		resp, err := (&http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			http.Error(w, "logo unreachable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10+1))
		typ := http.DetectContentType(body)
		if resp.StatusCode != http.StatusOK || len(body) > 512<<10 ||
			(typ != "image/png" && typ != "image/jpeg" && typ != "image/gif" && typ != "image/webp") {
			http.Error(w, "not a logo", http.StatusBadGateway)
			return
		}
		it = logoItem{typ: typ, body: body}
		marketLogos.mu.Lock()
		if len(marketLogos.items) > 1000 {
			marketLogos.items = map[string]logoItem{}
		}
		marketLogos.items[raw] = it
		marketLogos.mu.Unlock()
	}
	w.Header().Set("Content-Type", it.typ)
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(it.body)
}

// marketStatus maps a Marketplace error to a status.
func marketStatus(err error) int {
	switch {
	case errors.Is(err, marketplace.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, marketplace.ErrBadRequest):
		return http.StatusBadRequest
	case errors.Is(err, marketplace.ErrUnreachable):
		return http.StatusBadGateway
	}
	return http.StatusBadGateway
}

func marketError(w http.ResponseWriter, err error) {
	writeJSON(w, marketStatus(err), map[string]string{"error": err.Error()})
}

func (s *Server) marketOff(w http.ResponseWriter) bool {
	if s.market == nil || s.market.Catalog == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Marketplace is not available on this instance"})
		return true
	}
	return false
}

func (s *Server) handleMarketSources(w http.ResponseWriter, r *http.Request) {
	if s.market == nil || s.market.Catalog == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "sources": []any{}, "categories": []any{}})
		return
	}
	type src struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Type  string `json:"type"`
	}
	var out []src
	for _, x := range s.market.Catalog.Sources() {
		out = append(out, src{ID: x.ID(), Label: x.Label(), Type: x.Type()})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	body := map[string]any{"configured": true, "sources": out,
		"types": []string{marketplace.TypeKubernetesApp, marketplace.TypeContainerImage}}
	cats, err := s.market.Catalog.Categories(ctx)
	if err != nil {
		body["categories"] = []any{}
		body["error"] = err.Error()
	} else {
		body["categories"] = cats
	}
	writeJSON(w, http.StatusOK, body)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) handleMarketSearch(w http.ResponseWriter, r *http.Request) {
	if s.marketOff(w) {
		return
	}
	v := r.URL.Query()
	page, _ := strconv.Atoi(v.Get("page"))
	size, _ := strconv.Atoi(v.Get("size"))
	sources := splitList(v.Get("source"))
	if t := v.Get("type"); t != "" {
		byType := s.market.Catalog.ByType(t)
		if len(sources) == 0 {
			sources = byType
		} else {
			keep := map[string]bool{}
			for _, id := range byType {
				keep[id] = true
			}
			var both []string
			for _, id := range sources {
				if keep[id] {
					both = append(both, id)
				}
			}
			sources = both
		}
		if len(sources) == 0 {
			writeJSON(w, http.StatusOK, marketplace.Results{Page: max(page, 1), Size: size, Items: []marketplace.Summary{}})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := s.market.Catalog.Search(ctx, sources, marketplace.Query{Text: v.Get("q"), Page: page, Size: size,
		Categories: splitList(v.Get("category")), Badges: splitList(v.Get("badge"))})
	if err != nil {
		marketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) marketSource(w http.ResponseWriter, r *http.Request) (marketplace.Source, string, bool) {
	if s.marketOff(w) {
		return nil, "", false
	}
	src, err := s.market.Catalog.Source(r.URL.Query().Get("source"))
	if err != nil {
		marketError(w, err)
		return nil, "", false
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "want ?source=&id="})
		return nil, "", false
	}
	return src, id, true
}

func (s *Server) handleMarketProduct(w http.ResponseWriter, r *http.Request) {
	src, id, ok := s.marketSource(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	p, err := src.Product(ctx, id)
	if err != nil {
		marketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleMarketVersions(w http.ResponseWriter, r *http.Request) {
	src, id, ok := s.marketSource(w, r)
	if !ok {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	vs, total, err := src.Versions(ctx, id, page)
	if err != nil {
		marketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": vs, "total": total, "page": max(page, 1)})
}

func (s *Server) handleMarketParameters(w http.ResponseWriter, r *http.Request) {
	src, id, ok := s.marketSource(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	ps, err := src.Parameters(ctx, id, r.URL.Query().Get("version"))
	if err != nil {
		marketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"parameters": ps})
}

func (s *Server) marketClusters(ctx context.Context, project string) ([]MarketCluster, error) {
	if s.market == nil || s.market.Clusters == nil {
		return nil, errors.New("no Kubernetes cluster is available")
	}
	return s.market.Clusters.Clusters(ctx, project)
}

func (s *Server) handleMarketClusters(w http.ResponseWriter, r *http.Request) {
	if s.marketOff(w) {
		return
	}
	cs, err := s.marketClusters(r.Context(), r.URL.Query().Get("project"))
	if err != nil {
		marketError(w, err)
		return
	}
	canCreate := "Marketplace has no cluster backend."
	if s.market.Clusters != nil {
		canCreate = s.market.Clusters.CanCreate()
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": cs, "createUnavailable": canCreate})
}

func (s *Server) handleMarketCreateCluster(w http.ResponseWriter, r *http.Request) {
	if s.marketOff(w) {
		return
	}
	var req struct {
		Project, Zone, Name string
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	if json.Unmarshal(b, &req) != nil || req.Project == "" || req.Zone == "" || !marketplace.DNSLabel.MatchString(req.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `want {"project", "zone", "name"}, the name lowercase letters, digits and dashes`})
		return
	}
	if s.market.Clusters == nil || s.market.Clusters.CanCreate() != "" {
		why := "Marketplace has no cluster backend."
		if s.market.Clusters != nil {
			why = s.market.Clusters.CanCreate()
		}
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": why})
		return
	}
	if err := s.market.Clusters.CreateCluster(r.Context(), req.Project, req.Zone, req.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"created": req.Name})
}

// target finds the named cluster.
func (s *Server) marketTarget(ctx context.Context, project, cluster string) (marketplace.Target, MarketCluster, error) {
	cs, err := s.marketClusters(ctx, project)
	if err != nil {
		return marketplace.Target{}, MarketCluster{}, err
	}
	for _, c := range cs {
		if c.Name == cluster {
			if c.Kubeconfig == "" {
				return marketplace.Target{}, c, errors.New("cluster " + cluster + " is " + strings.ToLower(c.Status) + ": its Kubernetes API is not reachable yet")
			}
			return marketplace.Target{Cluster: c.Name, Kubeconfig: c.Kubeconfig}, c, nil
		}
	}
	return marketplace.Target{}, MarketCluster{}, errors.New("no cluster named " + cluster)
}

func (s *Server) handleMarketNamespaces(w http.ResponseWriter, r *http.Request) {
	if s.marketOff(w) {
		return
	}
	t, _, err := s.marketTarget(r.Context(), r.URL.Query().Get("project"), r.URL.Query().Get("cluster"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ns, err := s.market.Kube.Namespaces(r.Context(), t)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": ns})
}

func (s *Server) handleMarketDeploy(w http.ResponseWriter, r *http.Request) {
	if s.marketOff(w) {
		return
	}
	var req struct {
		Project string `json:"project"`
		marketplace.DeployRequest
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if json.Unmarshal(b, &req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the request is not JSON"})
		return
	}
	src, err := s.market.Catalog.Source(req.Source)
	if err != nil {
		marketError(w, err)
		return
	}
	if !marketplace.DNSLabel.MatchString(req.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "App instance name: lowercase letters, digits and dashes, starting with a letter, at most 63"})
		return
	}
	if !marketplace.DNSLabel.MatchString(req.Namespace) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Namespace: lowercase letters, digits and dashes, starting with a letter, at most 63"})
		return
	}
	t, _, err := s.marketTarget(r.Context(), req.Project, req.Cluster)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	app := marketplace.AppMeta{Name: req.Name, Namespace: req.Namespace}
	plan, err := src.Plan(ctx, req.Product, req.Version, app, req.Values)
	if err != nil {
		marketError(w, err)
		return
	}
	if plan.Descriptor.Description == "" {
		if p, err := src.Product(ctx, req.Product); err == nil {
			plan.Descriptor.Description = p.Description
		}
	}
	if err := s.market.Kube.Deploy(ctx, t, app, req.CreateNamespace, plan); err != nil {
		marketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"cluster": t.Cluster, "namespace": req.Namespace, "name": req.Name})
}

func (s *Server) handleMarketApplications(w http.ResponseWriter, r *http.Request) {
	if s.market == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "applications": []any{}})
		return
	}
	cs, err := s.marketClusters(r.Context(), r.URL.Query().Get("project"))
	if err != nil {
		marketError(w, err)
		return
	}
	apps := []marketplace.App{}
	var problems []string
	for _, c := range cs {
		if c.Kubeconfig == "" {
			continue
		}
		as, err := s.market.Kube.Applications(r.Context(), marketplace.Target{Cluster: c.Name, Kubeconfig: c.Kubeconfig})
		if err != nil {
			problems = append(problems, c.Name+": "+err.Error())
			continue
		}
		apps = append(apps, as...)
	}
	sort.SliceStable(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "applications": apps, "clusters": cs, "problems": problems})
}

func (s *Server) handleMarketApplication(w http.ResponseWriter, r *http.Request) {
	if s.market == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Applications are not available on this instance"})
		return
	}
	v := r.URL.Query()
	t, c, err := s.marketTarget(r.Context(), v.Get("project"), v.Get("cluster"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	a, err := s.market.Kube.Application(r.Context(), t, v.Get("namespace"), v.Get("name"))
	if err != nil {
		marketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"application": a, "own": c.Own})
}

func (s *Server) handleMarketDeleteApplication(w http.ResponseWriter, r *http.Request) {
	if s.market == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Applications are not available on this instance"})
		return
	}
	var req struct {
		Project, Cluster, Namespace, Name string
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	if json.Unmarshal(b, &req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the request is not JSON"})
		return
	}
	t, _, err := s.marketTarget(r.Context(), req.Project, req.Cluster)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err := s.market.Kube.DeleteApplication(r.Context(), t, req.Namespace, req.Name); err != nil {
		marketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": req.Name})
}
