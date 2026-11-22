package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DockerHub is the Docker Hub source: hub.docker.com's public, anonymous
// search (api/search/v4, the search Docker Hub's own site runs, with
// publisher badges and categories; v2/search/repositories when it is not
// answering), repository (v2/repositories/{ns}/{repo}) and tag APIs, and
// the registry's anonymous pull token, manifest and image config for an
// image's EXPOSE ports and environment.
type DockerHub struct {
	// HubURL, AuthURL and RegistryURL default to Docker Hub's; a test
	// points them at a fixture.
	HubURL, AuthURL, RegistryURL string
	// Client defaults to one with a 20-second timeout.
	Client *http.Client
	// Arch is the platform an image index is resolved for: the cluster
	// nodes', the host's architecture by default.
	Arch string

	mu      sync.Mutex
	configs map[string]ImageConfig
	cats    []Category
}

// DockerHubID is the source's id.
const DockerHubID = "dockerhub"

func (h *DockerHub) ID() string    { return DockerHubID }
func (h *DockerHub) Label() string { return "Docker Hub" }
func (h *DockerHub) Type() string  { return TypeContainerImage }

func (h *DockerHub) hub() string {
	return strings.TrimRight(or(h.HubURL, "https://hub.docker.com"), "/")
}
func (h *DockerHub) auth() string {
	return strings.TrimRight(or(h.AuthURL, "https://auth.docker.io"), "/")
}
func (h *DockerHub) registry() string {
	return strings.TrimRight(or(h.RegistryURL, "https://registry-1.docker.io"), "/")
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (h *DockerHub) client() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// hubError is a Docker Hub refusal: its message, and the status.
type hubError struct {
	Status  int
	Message string
}

func (e *hubError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("Docker Hub answered %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("Docker Hub answered %d", e.Status)
}

func (e *hubError) Unwrap() error {
	switch e.Status {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusBadRequest:
		return ErrBadRequest
	}
	return nil
}

// get reads one JSON document. limit bounds the body.
func (h *DockerHub) get(ctx context.Context, u string, header http.Header, limit int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "cloudburrow-marketplace")
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if resp.StatusCode != http.StatusOK {
		var m struct {
			Message string `json:"message"`
			Detail  string `json:"detail"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(body, &m)
		msg := or(m.Message, m.Detail)
		if msg == "" && len(m.Errors) > 0 {
			msg = m.Errors[0].Message
		}
		return &hubError{Status: resp.StatusCode, Message: msg}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", u, err)
	}
	return nil
}

// Categories are Docker Hub's (v2/categories).
func (h *DockerHub) Categories(ctx context.Context) ([]Category, error) {
	h.mu.Lock()
	cached := h.cats
	h.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	var cs []Category
	if err := h.get(ctx, h.hub()+"/v2/categories", nil, 1<<20, &cs); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.cats = cs
	h.mu.Unlock()
	return cs, nil
}

type hubV4Result struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Slug             string `json:"slug"`
	Type             string `json:"type"`
	ShortDescription string `json:"short_description"`
	Badge            string `json:"badge"`
	StarCount        int    `json:"star_count"`
	PullCount        string `json:"pull_count"`
	UpdatedAt        string `json:"updated_at"`
	Publisher        struct {
		Name string `json:"name"`
	} `json:"publisher"`
	LogoURL struct {
		Small string `json:"small"`
	} `json:"logo_url"`
	Categories []Category `json:"categories"`
}

// Search is one page of Docker Hub's search, its images only.
func (h *DockerHub) Search(ctx context.Context, q Query) (Results, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.Size < 1 {
		q.Size = 24
	}
	v := url.Values{}
	v.Set("query", q.Text)
	v.Set("from", strconv.Itoa((q.Page-1)*q.Size))
	v.Set("size", strconv.Itoa(q.Size))
	v.Set("type", "image")
	if len(q.Badges) > 0 {
		v.Set("badges", strings.Join(q.Badges, ","))
	}
	if len(q.Categories) > 0 {
		v.Set("categories", strings.Join(q.Categories, ","))
	}
	var r struct {
		Total   int           `json:"total"`
		Results []hubV4Result `json:"results"`
	}
	err := h.get(ctx, h.hub()+"/api/search/v4?"+v.Encode(), nil, 8<<20, &r)
	var he *hubError
	if errors.As(err, &he) && he.Status == http.StatusForbidden {
		// Docker Hub pages an anonymous search only so far: "pagination
		// too large for anonymous requests". The rest is reached by a
		// narrower search or a filter.
		return Results{Page: q.Page, Size: q.Size, Items: []Summary{}, Limit: he.Message}, nil
	}
	if errors.As(err, &he) && (he.Status == http.StatusNotFound || he.Status >= 500) && len(q.Badges) == 0 && len(q.Categories) == 0 {
		return h.searchV2(ctx, q)
	}
	if err != nil {
		return Results{}, err
	}
	out := Results{Total: r.Total, Page: q.Page, Size: q.Size, Items: make([]Summary, 0, len(r.Results))}
	for _, x := range r.Results {
		if x.Type != "" && x.Type != "image" {
			continue
		}
		out.Items = append(out.Items, h.summaryV4(x))
	}
	out.HasMore = q.Page*q.Size < r.Total
	return out, nil
}

func (h *DockerHub) summaryV4(x hubV4Result) Summary {
	id := x.ID
	if id == "" {
		id = x.Slug
	}
	if !strings.Contains(id, "/") {
		id = "library/" + id
	}
	badge := x.Badge
	if badge == "none" {
		badge = ""
	}
	return Summary{
		Source: DockerHubID, SourceLabel: h.Label(), ID: id, Name: or(x.Name, id),
		Publisher: hubPublisher(id, x.Publisher.Name, badge), Description: x.ShortDescription,
		Type: TypeContainerImage, Badge: badge, Pulls: x.PullCount, Stars: x.StarCount,
		Logo: safeURL(x.LogoURL.Small), Updated: x.UpdatedAt, Categories: x.Categories,
	}
}

// searchV2 is the v2 search, which knows Docker Official Images only.
func (h *DockerHub) searchV2(ctx context.Context, q Query) (Results, error) {
	v := url.Values{}
	v.Set("query", q.Text)
	v.Set("page", strconv.Itoa(q.Page))
	v.Set("page_size", strconv.Itoa(q.Size))
	var r struct {
		Count   int    `json:"count"`
		Next    string `json:"next"`
		Results []struct {
			RepoName         string `json:"repo_name"`
			ShortDescription string `json:"short_description"`
			StarCount        int    `json:"star_count"`
			PullCount        int64  `json:"pull_count"`
			IsOfficial       bool   `json:"is_official"`
		} `json:"results"`
	}
	if err := h.get(ctx, h.hub()+"/v2/search/repositories/?"+v.Encode(), nil, 8<<20, &r); err != nil {
		var he *hubError
		if errors.As(err, &he) && he.Status == http.StatusForbidden {
			return Results{Page: q.Page, Size: q.Size, Items: []Summary{}, Limit: he.Message}, nil
		}
		return Results{}, err
	}
	out := Results{Total: r.Count, Page: q.Page, Size: q.Size, HasMore: r.Next != "", Items: []Summary{}}
	for _, x := range r.Results {
		id := x.RepoName
		if !strings.Contains(id, "/") {
			id = "library/" + id
		}
		badge := ""
		if x.IsOfficial {
			badge = "official"
		}
		out.Items = append(out.Items, Summary{Source: DockerHubID, SourceLabel: h.Label(), ID: id, Name: x.RepoName,
			Publisher: hubPublisher(id, "", badge), Description: x.ShortDescription, Type: TypeContainerImage,
			Badge: badge, Pulls: HumanCount(x.PullCount), Stars: x.StarCount})
	}
	return out, nil
}

// hubPublisher is the publisher a card names: Docker for a Docker
// Official Image, the namespace otherwise.
func hubPublisher(id, publisher, badge string) string {
	ns := id[:strings.Index(id, "/")]
	if ns == "library" || badge == "official" {
		return "Docker"
	}
	return or(publisher, ns)
}

var repoPart = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$`)

// splitRepo checks a namespace/repository id.
func splitRepo(id string) (ns, repo string, err error) {
	ns, repo, ok := strings.Cut(id, "/")
	if !ok {
		ns, repo = "library", id
	}
	if !repoPart.MatchString(ns) || !repoPart.MatchString(repo) || len(ns) > 255 || len(repo) > 255 {
		return "", "", fmt.Errorf("%w: %q is not a Docker Hub repository (namespace/name)", ErrBadRequest, id)
	}
	return ns, repo, nil
}

// HumanCount is a pull count as Docker Hub's site writes it: 13.6B, 92.8M,
// 1.2K.
func HumanCount(n int64) string {
	f := float64(n)
	for _, u := range []struct {
		div    float64
		suffix string
	}{{1e9, "B"}, {1e6, "M"}, {1e3, "K"}} {
		if f >= u.div {
			s := strconv.FormatFloat(f/u.div, 'f', 1, 64)
			return strings.TrimSuffix(s, ".0") + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// safeURL keeps an https URL and drops anything else, so a link or logo
// the console draws can never be javascript: or data:.
func safeURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return u.String()
}

// Product is a repository's page: its description and README, counts,
// categories and first page of tags.
func (h *DockerHub) Product(ctx context.Context, id string) (Product, error) {
	ns, repo, err := splitRepo(id)
	if err != nil {
		return Product{}, err
	}
	var r struct {
		Namespace       string     `json:"namespace"`
		Name            string     `json:"name"`
		RepositoryType  string     `json:"repository_type"`
		Description     string     `json:"description"`
		FullDescription string     `json:"full_description"`
		StarCount       int        `json:"star_count"`
		PullCount       int64      `json:"pull_count"`
		LastUpdated     string     `json:"last_updated"`
		DateRegistered  string     `json:"date_registered"`
		IsPrivate       bool       `json:"is_private"`
		Categories      []Category `json:"categories"`
	}
	if err := h.get(ctx, h.hub()+"/v2/repositories/"+url.PathEscape(ns)+"/"+url.PathEscape(repo)+"/", nil, 4<<20, &r); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Product{}, fmt.Errorf("%w: Docker Hub has no public repository %s/%s", ErrNotFound, ns, repo)
		}
		return Product{}, err
	}
	full := ns + "/" + repo
	badge := ""
	if ns == "library" {
		badge = "official"
	}
	name := full
	if ns == "library" {
		name = repo
	}
	p := Product{Summary: Summary{Source: DockerHubID, SourceLabel: h.Label(), ID: full, Name: name,
		Publisher: hubPublisher(full, "", badge), Description: r.Description, Type: TypeContainerImage, Badge: badge,
		Pulls: HumanCount(r.PullCount), Stars: r.StarCount, Updated: r.LastUpdated, Categories: r.Categories},
		Overview: r.FullDescription, Registered: r.DateRegistered,
		Pricing: "Free. Docker Hub serves this image's public tags anonymously; the cluster pulls it from docker.io, " +
			"subject to Docker Hub's anonymous pull rate limit.",
	}
	// The badge and logo Docker Hub's own search carries: a verified
	// publisher or sponsored open-source project is not in v2.
	if s, err := h.Search(ctx, Query{Text: repo, Size: 25}); err == nil {
		for _, x := range s.Items {
			if x.ID == full {
				p.Badge, p.Logo, p.Publisher = x.Badge, x.Logo, x.Publisher
				if p.Name == full && x.Name != "" {
					p.Name = x.Name
				}
				break
			}
		}
	}
	page := "https://hub.docker.com/r/" + full
	if ns == "library" {
		page = "https://hub.docker.com/_/" + repo
	}
	p.Documentation = []Link{{Label: "Docker Hub page", URL: page}, {Label: "Tags", URL: page + "/tags"}}
	p.Support = []Link{{Label: "Docker Hub page", URL: page}}
	p.Pull = "docker.io/" + full
	if ns == "library" {
		p.Pull = repo
	}
	vs, total, err := h.Versions(ctx, full, 1)
	if err != nil {
		return Product{}, err
	}
	p.Versions, p.VersionsTotal = vs, total
	return p, nil
}

// Versions is one page (25) of a repository's tags, newest first.
func (h *DockerHub) Versions(ctx context.Context, id string, page int) ([]Version, int, error) {
	ns, repo, err := splitRepo(id)
	if err != nil {
		return nil, 0, err
	}
	if page < 1 {
		page = 1
	}
	var r struct {
		Count   int `json:"count"`
		Results []struct {
			Name        string `json:"name"`
			LastUpdated string `json:"last_updated"`
			FullSize    int64  `json:"full_size"`
			Digest      string `json:"digest"`
			Images      []struct {
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
				OS           string `json:"os"`
			} `json:"images"`
		} `json:"results"`
	}
	u := fmt.Sprintf("%s/v2/repositories/%s/%s/tags?page_size=25&page=%d&ordering=last_updated", h.hub(), url.PathEscape(ns), url.PathEscape(repo), page)
	if err := h.get(ctx, u, nil, 8<<20, &r); err != nil {
		return nil, 0, err
	}
	out := make([]Version, 0, len(r.Results))
	for _, t := range r.Results {
		v := Version{Name: t.Name, Updated: t.LastUpdated, Size: t.FullSize, Digest: t.Digest}
		seen := map[string]bool{}
		for _, im := range t.Images {
			if im.OS != "" && im.OS != "linux" {
				continue
			}
			a := im.Architecture
			if im.Variant != "" {
				a += "/" + im.Variant
			}
			if a != "" && a != "unknown" && !seen[a] {
				seen[a] = true
				v.Archs = append(v.Archs, a)
			}
		}
		sort.Strings(v.Archs)
		out = append(out, v)
	}
	return out, r.Count, nil
}

// ImageConfig is what an image's config says about running it.
type ImageConfig struct {
	ExposedPorts []string `json:"exposedPorts"`
	Env          []string `json:"env"`
	Entrypoint   []string `json:"entrypoint"`
	Cmd          []string `json:"cmd"`
	User         string   `json:"user"`
	WorkingDir   string   `json:"workingDir"`
	Arch         string   `json:"arch"`
}

const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json"

var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// Config reads a tag's image config through the registry's anonymous pull
// token: the index, the platform's manifest and its config blob.
func (h *DockerHub) Config(ctx context.Context, id, tag string) (ImageConfig, error) {
	ns, repo, err := splitRepo(id)
	if err != nil {
		return ImageConfig{}, err
	}
	if !tagPattern.MatchString(tag) {
		return ImageConfig{}, fmt.Errorf("%w: %q is not a tag", ErrBadRequest, tag)
	}
	key := ns + "/" + repo + ":" + tag
	h.mu.Lock()
	if c, ok := h.configs[key]; ok {
		h.mu.Unlock()
		return c, nil
	}
	h.mu.Unlock()
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	scope := "repository:" + ns + "/" + repo + ":pull"
	if err := h.get(ctx, h.auth()+"/token?service=registry.docker.io&scope="+url.QueryEscape(scope), nil, 1<<20, &tok); err != nil {
		return ImageConfig{}, err
	}
	header := http.Header{"Accept": {manifestAccept}}
	if t := or(tok.Token, tok.AccessToken); t != "" {
		header.Set("Authorization", "Bearer "+t)
	}
	base := h.registry() + "/v2/" + ns + "/" + repo
	var m struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := h.get(ctx, base+"/manifests/"+tag, header, 4<<20, &m); err != nil {
		return ImageConfig{}, err
	}
	arch := or(h.Arch, runtime.GOARCH)
	chosen := arch
	if len(m.Manifests) > 0 {
		digest := ""
		for _, d := range m.Manifests {
			if d.Platform.OS == "linux" && d.Platform.Architecture == arch {
				digest = d.Digest
				break
			}
		}
		if digest == "" {
			for _, d := range m.Manifests {
				if d.Platform.OS == "linux" && d.Platform.Architecture != "unknown" {
					digest, chosen = d.Digest, d.Platform.Architecture
					break
				}
			}
		}
		if digest == "" {
			return ImageConfig{}, fmt.Errorf("%w: %s has no linux image", ErrNotFound, key)
		}
		m.Manifests = nil
		if err := h.get(ctx, base+"/manifests/"+digest, header, 4<<20, &m); err != nil {
			return ImageConfig{}, err
		}
	}
	if m.Config.Digest == "" {
		return ImageConfig{}, fmt.Errorf("%w: %s's manifest names no config", ErrNotFound, key)
	}
	var cfg struct {
		Architecture string `json:"architecture"`
		Config       struct {
			ExposedPorts map[string]any `json:"ExposedPorts"`
			Env          []string       `json:"Env"`
			Entrypoint   []string       `json:"Entrypoint"`
			Cmd          []string       `json:"Cmd"`
			User         string         `json:"User"`
			WorkingDir   string         `json:"WorkingDir"`
		} `json:"config"`
	}
	blobHeader := http.Header{"Accept": {"*/*"}}
	if a := header.Get("Authorization"); a != "" {
		blobHeader.Set("Authorization", a)
	}
	if err := h.get(ctx, base+"/blobs/"+m.Config.Digest, blobHeader, 8<<20, &cfg); err != nil {
		return ImageConfig{}, err
	}
	out := ImageConfig{Env: cfg.Config.Env, Entrypoint: cfg.Config.Entrypoint, Cmd: cfg.Config.Cmd,
		User: cfg.Config.User, WorkingDir: cfg.Config.WorkingDir, Arch: or(cfg.Architecture, chosen), ExposedPorts: []string{}}
	for p := range cfg.Config.ExposedPorts {
		out.ExposedPorts = append(out.ExposedPorts, p)
	}
	sort.Slice(out.ExposedPorts, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.Split(out.ExposedPorts[i], "/")[0])
		b, _ := strconv.Atoi(strings.Split(out.ExposedPorts[j], "/")[0])
		return a < b
	})
	h.mu.Lock()
	if h.configs == nil {
		h.configs = map[string]ImageConfig{}
	}
	h.configs[key] = out
	h.mu.Unlock()
	return out, nil
}
