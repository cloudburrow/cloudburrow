// Package marketplace is the console's Marketplace (#2149): a catalogue of
// deployable software read from public, anonymous registries, each one a
// Source behind the same interface (search, product detail, the Configure
// step's parameters and a deploy plan), and the deploy of a plan onto a
// Kubernetes cluster as a Kubernetes Application (app.k8s.io/v1beta1), as
// Google Cloud Marketplace's Kubernetes apps are deployed
// (https://docs.cloud.google.com/kubernetes-engine/docs/how-to/deploying-marketplace-app).
//
// Docker Hub is the first source: every public repository it serves is
// reachable, by search, filters or its name. Everything is read online; an
// instance with no route to the registry gets the registry's error.
package marketplace

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Errors a source returns, which the console maps to statuses.
var (
	// ErrNotFound is a product or version the source does not have.
	ErrNotFound = errors.New("not found")
	// ErrBadRequest is a malformed id, query or parameter value.
	ErrBadRequest = errors.New("bad request")
	// ErrUnreachable wraps a failure to reach the source at all.
	ErrUnreachable = errors.New("unreachable")
)

// Type is a product's Marketplace type, as Google's Type filter names them.
const (
	TypeKubernetesApp  = "Kubernetes apps"
	TypeContainerImage = "Container images"
)

// LogoHosts are the hosts the console reads a product's logo from: Docker
// Hub's logo CDN.
var LogoHosts = map[string]bool{"djeqr6to3dedg.cloudfront.net": true}

// Query is one search of a source.
type Query struct {
	Text string `json:"text"`
	// Page is 1-based; Size the page size.
	Page int `json:"page"`
	Size int `json:"size"`
	// Categories are the source's category slugs, any of which matches.
	Categories []string `json:"categories,omitempty"`
	// Badges are publisher badges (official, verified_publisher,
	// open_source), any of which matches.
	Badges []string `json:"badges,omitempty"`
}

// Category is one of a source's categories.
type Category struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Summary is one search result's card.
type Summary struct {
	Source      string     `json:"source"`
	SourceLabel string     `json:"sourceLabel"`
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Publisher   string     `json:"publisher"`
	Description string     `json:"description"`
	Type        string     `json:"type"`
	Badge       string     `json:"badge,omitempty"`
	Pulls       string     `json:"pulls,omitempty"`
	Stars       int        `json:"stars,omitempty"`
	Logo        string     `json:"logo,omitempty"`
	Updated     string     `json:"updated,omitempty"`
	Categories  []Category `json:"categories,omitempty"`
}

// Results is one page of a search.
type Results struct {
	Total   int       `json:"total"`
	Page    int       `json:"page"`
	Size    int       `json:"size"`
	Items   []Summary `json:"items"`
	HasMore bool      `json:"hasMore"`
	// Limit is set when the source refuses to page further anonymously.
	Limit string `json:"limit,omitempty"`
}

// Version is one deployable version: a tag or a chart version.
type Version struct {
	Name    string   `json:"name"`
	Updated string   `json:"updated,omitempty"`
	Size    int64    `json:"size,omitempty"`
	Digest  string   `json:"digest,omitempty"`
	Archs   []string `json:"archs,omitempty"`
}

// Link is a product's documentation or support link.
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Product is a product's page.
type Product struct {
	Summary
	// Overview is the product's README, markdown, rendered by the console
	// as text-only markup.
	Overview      string    `json:"overview"`
	Registered    string    `json:"registered,omitempty"`
	Versions      []Version `json:"versions"`
	VersionsTotal int       `json:"versionsTotal"`
	Documentation []Link    `json:"documentation,omitempty"`
	Support       []Link    `json:"support,omitempty"`
	// Pricing is what the Pricing tab says.
	Pricing string `json:"pricing"`
	// Pull is what the cluster pulls, for an image.
	Pull string `json:"pull,omitempty"`
}

// Param is one of the Configure step's per-app parameters, drawn in
// Google's field style.
type Param struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Type     string   `json:"type"` // text, number, select, textarea
	Default  string   `json:"default,omitempty"`
	Options  []string `json:"options,omitempty"`
	Help     string   `json:"help,omitempty"`
	Required bool     `json:"required,omitempty"`
	Pattern  string   `json:"pattern,omitempty"`
	Section  string   `json:"section,omitempty"`
}

// AppMeta is what every plan records on its Application.
type AppMeta struct {
	Name, Namespace string
}

// Plan is what a deploy applies: the components' manifests and the
// Application's descriptor.
type Plan struct {
	Manifests []map[string]any
	// Kinds are the components' group/kinds, the Application's
	// componentKinds.
	Kinds []GroupKind
	// Descriptor is the Application's spec.descriptor.
	Descriptor Descriptor
	// Annotations go on the Application, recording the source and product.
	Annotations map[string]string
}

// GroupKind is one of an Application's componentKinds.
type GroupKind struct {
	Group string `json:"group"`
	Kind  string `json:"kind"`
}

// Descriptor is an Application's spec.descriptor.
type Descriptor struct {
	Type        string    `json:"type,omitempty"`
	Version     string    `json:"version,omitempty"`
	Description string    `json:"description,omitempty"`
	Links       []AppLink `json:"links,omitempty"`
	Keywords    []string  `json:"keywords,omitempty"`
}

// AppLink is one of a descriptor's links.
type AppLink struct {
	Description string `json:"description"`
	URL         string `json:"url"`
}

// Source is one catalogue.
type Source interface {
	// ID is the source's id in addresses (dockerhub); Label its name on
	// cards and in the Source filter (Docker Hub).
	ID() string
	Label() string
	// Type is its products' Marketplace type (TypeContainerImage,
	// TypeKubernetesApp), which the Type filter selects sources by.
	Type() string
	Categories(ctx context.Context) ([]Category, error)
	Search(ctx context.Context, q Query) (Results, error)
	Product(ctx context.Context, id string) (Product, error)
	Versions(ctx context.Context, id string, page int) ([]Version, int, error)
	// Parameters are the Configure step's per-app fields for a version.
	Parameters(ctx context.Context, id, version string) ([]Param, error)
	// Plan builds what Deploy applies.
	Plan(ctx context.Context, id, version string, app AppMeta, values map[string]string) (Plan, error)
}

// Catalog is the Marketplace's sources.
type Catalog struct {
	mu      sync.RWMutex
	sources []Source
}

// NewCatalog is a catalogue over sources, in their order.
func NewCatalog(sources ...Source) *Catalog { return &Catalog{sources: sources} }

// Sources are the catalogue's sources.
func (c *Catalog) Sources() []Source {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Source(nil), c.sources...)
}

// Source finds one by id.
func (c *Catalog) Source(id string) (Source, error) {
	for _, s := range c.Sources() {
		if s.ID() == id {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%w: no Marketplace source %q", ErrNotFound, id)
}

// Search searches the named sources (all when none is named), each for the
// same page, and joins their pages: the total is the sum, and the page has
// more while any source does. One source's failure fails the search only
// when it is the only source searched.
func (c *Catalog) Search(ctx context.Context, sourceIDs []string, q Query) (Results, error) {
	var sources []Source
	if len(sourceIDs) == 0 {
		sources = c.Sources()
	} else {
		for _, id := range sourceIDs {
			s, err := c.Source(id)
			if err != nil {
				return Results{}, err
			}
			sources = append(sources, s)
		}
	}
	if q.Page < 1 {
		q.Page = 1
	}
	if q.Size < 1 || q.Size > 100 {
		q.Size = 24
	}
	out := Results{Page: q.Page, Size: q.Size, Items: []Summary{}}
	var errs []error
	for _, s := range sources {
		r, err := s.Search(ctx, q)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Label(), err))
			continue
		}
		out.Total += r.Total
		out.Items = append(out.Items, r.Items...)
		out.HasMore = out.HasMore || r.HasMore
		if r.Limit != "" {
			out.Limit = r.Limit
		}
	}
	if len(errs) == len(sources) && len(errs) > 0 {
		return Results{}, errors.Join(errs...)
	}
	return out, nil
}

// ByType are the sources of a Marketplace type, all when it is empty.
func (c *Catalog) ByType(t string) []string {
	var out []string
	for _, s := range c.Sources() {
		if t == "" || s.Type() == t {
			out = append(out, s.ID())
		}
	}
	return out
}

// Categories are every source's categories, by name.
func (c *Catalog) Categories(ctx context.Context) ([]Category, error) {
	seen := map[string]bool{}
	var out []Category
	var errs []error
	for _, s := range c.Sources() {
		cs, err := s.Categories(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, cat := range cs {
			if !seen[cat.Slug] {
				seen[cat.Slug] = true
				out = append(out, cat)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	if len(out) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}
