package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"helm.sh/helm/v3/pkg/repo"
	"sigs.k8s.io/yaml"
)

// HelmRepo is one Helm chart repository read directly from its index.yaml.
type HelmRepo struct {
	// Name is the repository's short name in product ids (rancher).
	Name string `json:"name"`
	URL  string `json:"url"`
}

// RancherRepos are Rancher's chart repositories, which Rancher's Apps
// catalogue installs from: rancher/charts, the partner charts and the
// Rancher server chart.
var RancherRepos = []HelmRepo{
	{Name: "rancher-charts", URL: "https://charts.rancher.io"},
	{Name: "rancher-partner-charts", URL: "https://partner-charts.rancher.io"},
	{Name: "rancher-stable", URL: "https://releases.rancher.com/server-charts/stable"},
}

// HelmIndex is a source over Helm repositories' index.yaml: Rancher's (the
// "Rancher" source) or the repositories a user added (the "Helm" source).
type HelmIndex struct {
	SourceID, SourceLabel string
	// Repos lists the repositories; read on every search, so a user's
	// repository is searched as soon as it is added.
	Repos func() []HelmRepo
	// TTL is how long an index is kept, 10 minutes by default.
	TTL time.Duration

	fetch  chartFetcher
	mu     sync.Mutex
	loaded map[string]indexEntry
}

type indexEntry struct {
	at  time.Time
	idx *repo.IndexFile
	err error
}

func (h *HelmIndex) ID() string    { return h.SourceID }
func (h *HelmIndex) Label() string { return h.SourceLabel }
func (h *HelmIndex) Type() string  { return TypeKubernetesApp }

func (h *HelmIndex) Categories(context.Context) ([]Category, error) { return nil, nil }

func (h *HelmIndex) repos() []HelmRepo {
	if h.Repos == nil {
		return nil
	}
	return h.Repos()
}

// index reads a repository's index.yaml, kept for the TTL.
func (h *HelmIndex) index(ctx context.Context, r HelmRepo) (*repo.IndexFile, error) {
	ttl := h.TTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	h.mu.Lock()
	if e, ok := h.loaded[r.URL]; ok && time.Since(e.at) < ttl {
		h.mu.Unlock()
		return e.idx, e.err
	}
	h.mu.Unlock()
	b, err := h.fetch.get(ctx, strings.TrimRight(r.URL, "/")+"/index.yaml", nil, "-")
	var idx *repo.IndexFile
	if err == nil {
		idx = &repo.IndexFile{}
		if uerr := yaml.Unmarshal(b, idx); uerr != nil {
			idx, err = nil, fmt.Errorf("%s's index.yaml: %w", r.URL, uerr)
		} else {
			idx.SortEntries()
		}
	}
	h.mu.Lock()
	if h.loaded == nil {
		h.loaded = map[string]indexEntry{}
	}
	h.loaded[r.URL] = indexEntry{at: time.Now(), idx: idx, err: err}
	h.mu.Unlock()
	return idx, err
}

func (h *HelmIndex) summary(r HelmRepo, cv *repo.ChartVersion) Summary {
	name := cv.Name
	if d := cv.Annotations["catalog.cattle.io/display-name"]; d != "" {
		name = d
	}
	s := Summary{Source: h.SourceID, SourceLabel: h.SourceLabel, ID: r.Name + "/" + cv.Name, Name: name,
		Publisher: r.Name, Description: cv.Description, Type: TypeKubernetesApp}
	if cv.Annotations["catalog.cattle.io/certified"] == "rancher" || cv.Annotations["catalog.cattle.io/certified"] == "partner" {
		s.Badge = "verified_publisher"
	}
	if !cv.Created.IsZero() {
		s.Updated = cv.Created.UTC().Format(time.RFC3339)
	}
	if u, err := url.Parse(safeURL(cv.Icon)); err == nil && LogoHosts[u.Hostname()] {
		s.Logo = u.String()
	}
	return s
}

// Search matches the text against each chart's name, description and
// keywords, newest version per chart, in name order.
func (h *HelmIndex) Search(ctx context.Context, q Query) (Results, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.Size < 1 {
		q.Size = 24
	}
	out := Results{Page: q.Page, Size: q.Size, Items: []Summary{}}
	if len(q.Categories) > 0 {
		return out, nil // an index has no categories
	}
	wantBadge := len(q.Badges) == 0
	for _, b := range q.Badges {
		if b == "verified_publisher" {
			wantBadge = true
		}
	}
	if !wantBadge {
		return out, nil
	}
	needle := strings.ToLower(strings.TrimSpace(q.Text))
	var all []Summary
	var errs []string
	for _, r := range h.repos() {
		idx, err := h.index(ctx, r)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		for _, versions := range idx.Entries {
			if len(versions) == 0 || versions[0].Deprecated {
				continue
			}
			cv := versions[0]
			hay := strings.ToLower(cv.Name + " " + cv.Description + " " + strings.Join(cv.Keywords, " ") + " " + cv.Annotations["catalog.cattle.io/display-name"])
			if needle != "" && !strings.Contains(hay, needle) {
				continue
			}
			s := h.summary(r, cv)
			if len(q.Badges) > 0 && s.Badge == "" {
				continue
			}
			all = append(all, s)
		}
	}
	if len(all) == 0 && len(errs) > 0 && len(errs) == len(h.repos()) {
		return Results{}, fmt.Errorf("%w: %s", ErrUnreachable, strings.Join(errs, "; "))
	}
	sort.Slice(all, func(i, j int) bool { return strings.ToLower(all[i].Name) < strings.ToLower(all[j].Name) })
	out.Total = len(all)
	from := (q.Page - 1) * q.Size
	if from < len(all) {
		out.Items = all[from:min(from+q.Size, len(all))]
	}
	out.HasMore = from+q.Size < len(all)
	return out, nil
}

func (h *HelmIndex) find(ctx context.Context, id string) (HelmRepo, repo.ChartVersions, error) {
	repoName, chartName, err := splitChart(id)
	if err != nil {
		return HelmRepo{}, nil, err
	}
	for _, r := range h.repos() {
		if r.Name != repoName {
			continue
		}
		idx, err := h.index(ctx, r)
		if err != nil {
			return HelmRepo{}, nil, err
		}
		if vs, ok := idx.Entries[chartName]; ok && len(vs) > 0 {
			return r, vs, nil
		}
	}
	return HelmRepo{}, nil, fmt.Errorf("%w: no chart %s in %s", ErrNotFound, id, h.SourceLabel)
}

// archiveURL resolves a chart version's archive URL against its
// repository.
func archiveURL(r HelmRepo, cv *repo.ChartVersion) (string, error) {
	if len(cv.URLs) == 0 {
		return "", fmt.Errorf("%w: %s %s has no archive", ErrNotFound, cv.Name, cv.Version)
	}
	base, err := url.Parse(strings.TrimRight(r.URL, "/") + "/")
	if err != nil {
		return "", err
	}
	u, err := base.Parse(cv.URLs[0])
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "oci" {
		return "", fmt.Errorf("%w: %s's archive %q is not http(s) or oci", ErrBadRequest, cv.Name, cv.URLs[0])
	}
	return u.String(), nil
}

func (h *HelmIndex) version(ctx context.Context, id, version string) (HelmRepo, *repo.ChartVersion, error) {
	r, vs, err := h.find(ctx, id)
	if err != nil {
		return HelmRepo{}, nil, err
	}
	if version == "" || version == "latest" {
		return r, vs[0], nil
	}
	for _, cv := range vs {
		if cv.Version == version {
			return r, cv, nil
		}
	}
	return HelmRepo{}, nil, fmt.Errorf("%w: %s has no version %s", ErrNotFound, id, version)
}

func (h *HelmIndex) load(ctx context.Context, r HelmRepo, cv *repo.ChartVersion) (*ChartPlan, error) {
	u, err := archiveURL(r, cv)
	if err != nil {
		return nil, err
	}
	b, err := h.fetch.fetch(ctx, u)
	if err != nil {
		return nil, err
	}
	c, err := loadChart(b)
	if err != nil {
		return nil, err
	}
	return &ChartPlan{Chart: c}, nil
}

// Product is the chart's page, its README read from the newest archive.
func (h *HelmIndex) Product(ctx context.Context, id string) (Product, error) {
	r, vs, err := h.find(ctx, id)
	if err != nil {
		return Product{}, err
	}
	cv := vs[0]
	p := Product{Summary: h.summary(r, cv), Pricing: "Free. The chart is installed with Helm from " + r.URL + "."}
	if cp, err := h.load(ctx, r, cv); err == nil {
		p.Overview = chartReadme(cp.Chart)
	}
	if p.Overview == "" {
		p.Overview = cv.Description
	}
	p.Documentation = []Link{{Label: "Chart repository", URL: r.URL}}
	if u := safeURL(cv.Home); u != "" {
		p.Documentation = append(p.Documentation, Link{Label: "Home page", URL: u})
	}
	for _, s := range cv.Sources {
		if u := safeURL(s); u != "" {
			p.Documentation = append(p.Documentation, Link{Label: "Source", URL: u})
		}
	}
	p.Support = []Link{{Label: "Chart repository", URL: r.URL}}
	for _, m := range cv.Maintainers {
		if u := safeURL(m.URL); u != "" {
			p.Support = append(p.Support, Link{Label: m.Name, URL: u})
		}
	}
	p.Pull = r.URL
	var all []Version
	for _, v := range vs {
		all = append(all, Version{Name: v.Version, Updated: v.Created.UTC().Format(time.RFC3339), Digest: v.Digest})
	}
	p.Versions, p.VersionsTotal = pageOf(all, 1), len(all)
	return p, nil
}

func (h *HelmIndex) Versions(ctx context.Context, id string, page int) ([]Version, int, error) {
	_, vs, err := h.find(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	var all []Version
	for _, v := range vs {
		all = append(all, Version{Name: v.Version, Updated: v.Created.UTC().Format(time.RFC3339), Digest: v.Digest})
	}
	return pageOf(all, page), len(all), nil
}

func (h *HelmIndex) Parameters(ctx context.Context, id, version string) ([]Param, error) {
	r, cv, err := h.version(ctx, id, version)
	if err != nil {
		return nil, err
	}
	cp, err := h.load(ctx, r, cv)
	if err != nil {
		return nil, err
	}
	return append([]Param{{Name: "chartVersion", Label: "Chart version", Type: "text", Default: cv.Version, Required: true,
		Help: "The chart version Helm installs."}}, chartParams(chartValuesYAML(cp.Chart), cp.Chart.Schema)...), nil
}

func (h *HelmIndex) Plan(ctx context.Context, id, version string, _ AppMeta, values map[string]string) (Plan, error) {
	r, cv, err := h.version(ctx, id, or(values["chartVersion"], version))
	if err != nil {
		return Plan{}, err
	}
	cp, err := h.load(ctx, r, cv)
	if err != nil {
		return Plan{}, err
	}
	return chartPlan(cp.Chart, h.SourceID, id, cv.Version, values, []AppLink{{Description: "Chart repository", URL: r.URL}})
}

// UserRepos are the Helm repositories a user added, kept in a JSON file.
type UserRepos struct {
	File string
	mu   sync.Mutex
}

var userRepoName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,40}[a-z0-9])?$`)

// List reads the repositories; a missing file is none.
func (u *UserRepos) List() []HelmRepo {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.read()
}

func (u *UserRepos) read() []HelmRepo {
	b, err := os.ReadFile(u.File)
	if err != nil {
		return []HelmRepo{}
	}
	var out []HelmRepo
	if json.Unmarshal(b, &out) != nil {
		return []HelmRepo{}
	}
	return out
}

func (u *UserRepos) write(rs []HelmRepo) error {
	if err := os.MkdirAll(filepath.Dir(u.File), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(rs, "", "  ")
	return os.WriteFile(u.File, b, 0o644)
}

// Add adds a repository by name and https URL.
func (u *UserRepos) Add(name, rawURL string) error {
	if !userRepoName.MatchString(name) {
		return fmt.Errorf("%w: repository name %q: lowercase letters, digits and dashes", ErrBadRequest, name)
	}
	p, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || p.Scheme != "https" || p.Host == "" || p.User != nil {
		return fmt.Errorf("%w: repository URL %q is not an https URL", ErrBadRequest, rawURL)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	rs := u.read()
	for _, r := range rs {
		if r.Name == name {
			return fmt.Errorf("%w: a repository named %s exists", ErrBadRequest, name)
		}
	}
	return u.write(append(rs, HelmRepo{Name: name, URL: strings.TrimRight(p.String(), "/")}))
}

// Remove removes a repository by name.
func (u *UserRepos) Remove(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	rs := u.read()
	out := rs[:0]
	found := false
	for _, r := range rs {
		if r.Name == name {
			found = true
			continue
		}
		out = append(out, r)
	}
	if !found {
		return fmt.Errorf("%w: no repository named %s", ErrNotFound, name)
	}
	return u.write(out)
}
