package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ArtifactHub is the Artifact Hub source: the Helm charts of every
// repository artifacthub.io lists (Bitnami, Rancher's partner charts, the
// projects' own repositories and the rest), through its public, anonymous
// API (api/v1/packages/search, packages/helm/{repo}/{name}[/{version}],
// packages/{id}/{version}/values and values-schema). A chart's archive is
// read from its content URL, an HTTP(S) chart repository or an OCI
// registry, anonymously.
type ArtifactHub struct {
	// BaseURL defaults to https://artifacthub.io; a test points it at a
	// fixture.
	BaseURL string
	Client  *http.Client
	charts  chartFetcher
}

// ArtifactHubID is the source's id.
const ArtifactHubID = "artifacthub"

func (a *ArtifactHub) ID() string    { return ArtifactHubID }
func (a *ArtifactHub) Label() string { return "Artifact Hub" }
func (a *ArtifactHub) Type() string  { return TypeKubernetesApp }

func (a *ArtifactHub) base() string {
	return strings.TrimRight(or(a.BaseURL, "https://artifacthub.io"), "/")
}

func (a *ArtifactHub) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// ahCategories are Artifact Hub's categories, with the catalogue's slug
// each is filtered by (Docker Hub's where they mean the same).
var ahCategories = []struct {
	ID   int
	Slug string
	Name string
}{
	{1, "machine-learning-and-ai", "Machine learning & AI"}, {2, "databases-and-storage", "Databases & storage"},
	{3, "integration-and-delivery", "Integration & delivery"}, {4, "monitoring-and-observability", "Monitoring & observability"},
	{5, "networking", "Networking"}, {6, "security", "Security"}, {7, "databases-and-storage", "Databases & storage"},
	{8, "message-queues", "Message queues"}, {9, "data-science", "Data science"}, {10, "data-science", "Data science"},
	{11, "web-applications", "Web applications"},
}

func (a *ArtifactHub) Categories(context.Context) ([]Category, error) {
	seen := map[string]bool{}
	var out []Category
	for _, c := range ahCategories {
		if !seen[c.Slug] {
			seen[c.Slug] = true
			out = append(out, Category{Slug: c.Slug, Name: c.Name})
		}
	}
	return out, nil
}

func (a *ArtifactHub) get(ctx context.Context, path string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base()+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cloudburrow-marketplace")
	req.Header.Set("Accept", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: Artifact Hub has no %s", ErrNotFound, path)
	case resp.StatusCode == http.StatusBadRequest:
		return nil, fmt.Errorf("%w: Artifact Hub refused %s: %s", ErrBadRequest, path, strings.TrimSpace(string(body)))
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("Artifact Hub answered %d for %s", resp.StatusCode, path)
	}
	if s, ok := out.(*string); ok {
		*s = string(body)
		return resp.Header, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("decode Artifact Hub's %s: %w", path, err)
	}
	return resp.Header, nil
}

type ahPackage struct {
	PackageID       string `json:"package_id"`
	Name            string `json:"name"`
	DisplayName     string `json:"display_name"`
	Description     string `json:"description"`
	Version         string `json:"version"`
	AppVersion      string `json:"app_version"`
	Stars           int    `json:"stars"`
	LogoImageID     string `json:"logo_image_id"`
	TS              int64  `json:"ts"`
	Official        bool   `json:"official"`
	Category        int    `json:"category"`
	Readme          string `json:"readme"`
	ContentURL      string `json:"content_url"`
	HomeURL         string `json:"home_url"`
	HasValuesSchema bool   `json:"has_values_schema"`
	Links           []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"links"`
	AvailableVersions []struct {
		Version string `json:"version"`
		TS      int64  `json:"ts"`
	} `json:"available_versions"`
	Repository struct {
		Name                    string `json:"name"`
		URL                     string `json:"url"`
		DisplayName             string `json:"display_name"`
		VerifiedPublisher       bool   `json:"verified_publisher"`
		Official                bool   `json:"official"`
		OrganizationDisplayName string `json:"organization_display_name"`
		OrganizationName        string `json:"organization_name"`
		UserAlias               string `json:"user_alias"`
	} `json:"repository"`
}

func (p ahPackage) id() string { return p.Repository.Name + "/" + p.Name }

func (p ahPackage) badge() string {
	switch {
	case p.Official || p.Repository.Official:
		return "official"
	case p.Repository.VerifiedPublisher:
		return "verified_publisher"
	}
	return ""
}

func (p ahPackage) publisher() string {
	return or(p.Repository.OrganizationDisplayName, or(p.Repository.OrganizationName, or(p.Repository.UserAlias, p.Repository.Name)))
}

func (a *ArtifactHub) summary(p ahPackage) Summary {
	s := Summary{Source: ArtifactHubID, SourceLabel: a.Label(), ID: p.id(), Name: or(p.DisplayName, p.Name),
		Publisher: p.publisher(), Description: p.Description, Type: TypeKubernetesApp, Badge: p.badge(), Stars: p.Stars}
	if p.LogoImageID != "" {
		s.Logo = a.base() + "/image/" + url.PathEscape(p.LogoImageID)
	}
	if p.TS > 0 {
		s.Updated = time.Unix(p.TS, 0).UTC().Format(time.RFC3339)
	}
	for _, c := range ahCategories {
		if c.ID == p.Category {
			s.Categories = []Category{{Slug: c.Slug, Name: c.Name}}
		}
	}
	return s
}

// Search is one page of Artifact Hub's Helm charts.
func (a *ArtifactHub) Search(ctx context.Context, q Query) (Results, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.Size < 1 || q.Size > 60 {
		q.Size = 24
	}
	out := Results{Page: q.Page, Size: q.Size, Items: []Summary{}}
	v := url.Values{}
	v.Set("kind", "0")
	v.Set("ts_query_web", q.Text)
	v.Set("limit", strconv.Itoa(q.Size))
	v.Set("offset", strconv.Itoa((q.Page-1)*q.Size))
	v.Set("facets", "false")
	if len(q.Categories) > 0 {
		any := false
		for _, slug := range q.Categories {
			for _, c := range ahCategories {
				if c.Slug == slug {
					v.Add("category", strconv.Itoa(c.ID))
					any = true
				}
			}
		}
		if !any {
			return out, nil
		}
	}
	if len(q.Badges) > 0 {
		any := false
		for _, b := range q.Badges {
			switch b {
			case "official":
				v.Set("official", "true")
				any = true
			case "verified_publisher":
				v.Set("verified_publisher", "true")
				any = true
			}
		}
		if !any {
			return out, nil
		}
	}
	var r struct {
		Packages []ahPackage `json:"packages"`
	}
	h, err := a.get(ctx, "/api/v1/packages/search?"+v.Encode(), &r)
	if err != nil {
		return Results{}, err
	}
	out.Total, _ = strconv.Atoi(h.Get("Pagination-Total-Count"))
	for _, p := range r.Packages {
		out.Items = append(out.Items, a.summary(p))
	}
	out.HasMore = q.Page*q.Size < out.Total
	return out, nil
}

func splitChart(id string) (repo, name string, err error) {
	repo, name, ok := strings.Cut(id, "/")
	if !ok || repo == "" || name == "" || strings.ContainsAny(id, "?#%\\ ") || strings.Contains(name, "/") || strings.Contains(id, "..") {
		return "", "", fmt.Errorf("%w: %q is not a chart (repository/name)", ErrBadRequest, id)
	}
	return repo, name, nil
}

func (a *ArtifactHub) pkg(ctx context.Context, id, version string) (ahPackage, error) {
	repo, name, err := splitChart(id)
	if err != nil {
		return ahPackage{}, err
	}
	path := "/api/v1/packages/helm/" + url.PathEscape(repo) + "/" + url.PathEscape(name)
	if version != "" && version != "latest" {
		path += "/" + url.PathEscape(version)
	}
	var p ahPackage
	if _, err := a.get(ctx, path, &p); err != nil {
		return ahPackage{}, err
	}
	return p, nil
}

// Product is a chart's page.
func (a *ArtifactHub) Product(ctx context.Context, id string) (Product, error) {
	p, err := a.pkg(ctx, id, "")
	if err != nil {
		return Product{}, err
	}
	out := Product{Summary: a.summary(p), Overview: p.Readme,
		Pricing: "Free. The chart is installed with Helm from " + p.Repository.URL + "; its images are pulled by the cluster from their registries."}
	page := a.base() + "/packages/helm/" + url.PathEscape(p.Repository.Name) + "/" + url.PathEscape(p.Name)
	out.Documentation = []Link{{Label: "Artifact Hub page", URL: page}}
	if u := safeURL(p.HomeURL); u != "" {
		out.Documentation = append(out.Documentation, Link{Label: "Home page", URL: u})
	}
	for _, l := range p.Links {
		if u := safeURL(l.URL); u != "" {
			out.Documentation = append(out.Documentation, Link{Label: or(l.Name, u), URL: u})
		}
	}
	out.Support = []Link{{Label: "Artifact Hub page", URL: page}}
	out.Pull = p.ContentURL
	for _, v := range p.AvailableVersions {
		ver := Version{Name: v.Version}
		if v.TS > 0 {
			ver.Updated = time.Unix(v.TS, 0).UTC().Format(time.RFC3339)
		}
		out.Versions = append(out.Versions, ver)
	}
	out.VersionsTotal = len(out.Versions)
	if len(out.Versions) > 25 {
		out.Versions = out.Versions[:25]
	}
	return out, nil
}

// Versions is a page of 25 of a chart's versions.
func (a *ArtifactHub) Versions(ctx context.Context, id string, page int) ([]Version, int, error) {
	full, err := a.pkg(ctx, id, "")
	if err != nil {
		return nil, 0, err
	}
	var all []Version
	for _, v := range full.AvailableVersions {
		all = append(all, Version{Name: v.Version, Updated: time.Unix(v.TS, 0).UTC().Format(time.RFC3339)})
	}
	return pageOf(all, page), len(all), nil
}

func pageOf(all []Version, page int) []Version {
	if page < 1 {
		page = 1
	}
	from := (page - 1) * 25
	if from >= len(all) {
		return []Version{}
	}
	return all[from:min(from+25, len(all))]
}

// Parameters are the chart version's values as fields and an editor.
func (a *ArtifactHub) Parameters(ctx context.Context, id, version string) ([]Param, error) {
	p, err := a.pkg(ctx, id, version)
	if err != nil {
		return nil, err
	}
	var values string
	if _, err := a.get(ctx, "/api/v1/packages/"+url.PathEscape(p.PackageID)+"/"+url.PathEscape(p.Version)+"/values", &values); err != nil &&
		!errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var schema []byte
	if p.HasValuesSchema {
		var s string
		if _, err := a.get(ctx, "/api/v1/packages/"+url.PathEscape(p.PackageID)+"/"+url.PathEscape(p.Version)+"/values-schema", &s); err == nil {
			schema = []byte(s)
		}
	}
	return append([]Param{{Name: "chartVersion", Label: "Chart version", Type: "text", Default: p.Version, Required: true,
		Help: "The chart version Helm installs.", Section: ""}}, chartParams(values, schema)...), nil
}

// Plan reads the chart version's archive and builds its install.
func (a *ArtifactHub) Plan(ctx context.Context, id, version string, _ AppMeta, values map[string]string) (Plan, error) {
	version = or(values["chartVersion"], version)
	p, err := a.pkg(ctx, id, version)
	if err != nil {
		return Plan{}, err
	}
	if p.ContentURL == "" {
		return Plan{}, fmt.Errorf("%w: Artifact Hub names no archive for %s %s", ErrNotFound, id, p.Version)
	}
	if a.Client != nil && a.charts.client == nil {
		a.charts.client = a.Client
	}
	b, err := a.charts.fetch(ctx, p.ContentURL)
	if err != nil {
		return Plan{}, err
	}
	c, err := loadChart(b)
	if err != nil {
		return Plan{}, err
	}
	links := []AppLink{{Description: "Artifact Hub", URL: a.base() + "/packages/helm/" + url.PathEscape(p.Repository.Name) + "/" + url.PathEscape(p.Name)}}
	return chartPlan(c, ArtifactHubID, id, p.Version, values, links)
}
