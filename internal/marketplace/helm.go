package marketplace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"sigs.k8s.io/yaml"
)

// Helm charts (#2149): fetching a chart's archive from an HTTP(S) chart
// repository or an OCI registry, anonymously; its values as the Configure
// step's parameters (the top-level fields of its values.schema.json as
// fields, and its values.yaml as a YAML editor); and a plan that a Helm
// install carries out.

// ChartPlan is a chart deploy: the loaded chart and the values to install
// it with.
type ChartPlan struct {
	Chart  *chart.Chart
	Values map[string]any
}

// chartFetcher reads chart archives, anonymously.
type chartFetcher struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string][]byte
}

func (f *chartFetcher) http() *http.Client {
	if f.client != nil {
		return f.client
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// maxChart bounds a chart archive.
const maxChart = 20 << 20

// fetch reads a chart archive from an https URL or an oci:// reference.
func (f *chartFetcher) fetch(ctx context.Context, ref string) ([]byte, error) {
	f.mu.Lock()
	if b, ok := f.cache[ref]; ok {
		f.mu.Unlock()
		return b, nil
	}
	f.mu.Unlock()
	var b []byte
	var err error
	if strings.HasPrefix(ref, "oci://") {
		b, err = f.fetchOCI(ctx, strings.TrimPrefix(ref, "oci://"))
	} else {
		b, err = f.get(ctx, ref, nil, "")
	}
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.cache == nil || len(f.cache) > 50 {
		f.cache = map[string][]byte{}
	}
	f.cache[ref] = b
	f.mu.Unlock()
	return b, nil
}

// get reads a URL, answering a registry's Bearer challenge with an
// anonymous token.
func (f *chartFetcher) get(ctx context.Context, u string, header http.Header, token string) ([]byte, error) {
	if p, err := url.Parse(u); err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
		return nil, fmt.Errorf("%w: %q is not an http(s) URL", ErrBadRequest, u)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "cloudburrow-marketplace")
	// "-" is the token request itself: no credentials, no retry.
	if token != "" && token != "-" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := f.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && token == "" {
		t, err := f.anonymousToken(ctx, resp.Header.Get("WWW-Authenticate"))
		if err != nil {
			return nil, err
		}
		return f.get(ctx, u, header, t)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxChart+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s answered 404", ErrNotFound, u)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", u, resp.StatusCode)
	}
	if len(body) > maxChart {
		return nil, fmt.Errorf("%w: %s is larger than 20 MB", ErrBadRequest, u)
	}
	return body, nil
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// anonymousToken answers `Bearer realm="…",service="…",scope="…"` with
// no credentials, as an anonymous pull does.
func (f *chartFetcher) anonymousToken(ctx context.Context, challenge string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return "", fmt.Errorf("the registry asks for credentials (%q); Marketplace pulls anonymously", challenge)
	}
	params := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" {
		return "", fmt.Errorf("the registry's token realm %q is not https", params["realm"])
	}
	q := realm.Query()
	if params["service"] != "" {
		q.Set("service", params["service"])
	}
	if params["scope"] != "" {
		q.Set("scope", params["scope"])
	}
	realm.RawQuery = q.Encode()
	b, err := f.get(ctx, realm.String(), nil, "-")
	if err != nil {
		return "", err
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(b, &tok); err != nil {
		return "", fmt.Errorf("decode the registry's token: %w", err)
	}
	return or(tok.Token, tok.AccessToken), nil
}

// fetchOCI pulls a chart stored in an OCI registry (host/repo:tag): its
// manifest, then the layer of the Helm chart content type.
func (f *chartFetcher) fetchOCI(ctx context.Context, ref string) ([]byte, error) {
	host, rest, ok := strings.Cut(ref, "/")
	repo, tag, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || host == "" || repo == "" || !tagPattern.MatchString(strings.ReplaceAll(tag, "+", "_")) {
		return nil, fmt.Errorf("%w: %q is not host/repository:version", ErrBadRequest, ref)
	}
	base := "https://" + host + "/v2/" + repo
	b, err := f.get(ctx, base+"/manifests/"+url.PathEscape(strings.ReplaceAll(tag, "+", "_")),
		http.Header{"Accept": {"application/vnd.oci.image.manifest.v1+json"}}, "")
	if err != nil {
		return nil, err
	}
	var m struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode %s's manifest: %w", ref, err)
	}
	for _, l := range m.Layers {
		if l.MediaType == "application/vnd.cncf.helm.chart.content.v1.tar+gzip" {
			return f.get(ctx, base+"/blobs/"+l.Digest, nil, "")
		}
	}
	return nil, fmt.Errorf("%w: %s is not a Helm chart", ErrNotFound, ref)
}

// loadChart loads an archive.
func loadChart(b []byte) (*chart.Chart, error) {
	c, err := loader.LoadArchive(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%w: load the chart: %v", ErrBadRequest, err)
	}
	return c, nil
}

// chartReadme is a chart's README.md.
func chartReadme(c *chart.Chart) string {
	for _, f := range c.Files {
		if strings.EqualFold(f.Name, "README.md") {
			return string(f.Data)
		}
	}
	return ""
}

// chartValuesYAML is the chart's own values.yaml text.
func chartValuesYAML(c *chart.Chart) string {
	for _, f := range c.Raw {
		if f.Name == "values.yaml" {
			return string(f.Data)
		}
	}
	return ""
}

// The values editor's parameter name; a schema field's name is "v." and
// its dotted path.
const (
	paramValues  = "values"
	schemaPrefix = "v."
)

// chartParams are a chart's Configure fields: each scalar property of its
// values.schema.json (top level and one level down) as a field, and the
// whole values.yaml as a YAML editor, which a field's value overrides.
func chartParams(valuesYAML string, schema []byte) []Param {
	var out []Param
	if len(schema) > 0 {
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if json.Unmarshal(schema, &s) == nil {
			var defaults map[string]any
			_ = yaml.Unmarshal([]byte(valuesYAML), &defaults)
			out = append(out, schemaFields("", s.Properties, defaults, 0)...)
		}
	}
	return append(out, Param{Name: paramValues, Label: "Values (YAML)", Type: "yaml", Default: valuesYAML,
		Section: "Values", Help: "The chart's values.yaml; edit any value. A field above overrides the same value here."})
}

type schemaProp struct {
	Type        any                        `json:"type"`
	Title       string                     `json:"title"`
	Description string                     `json:"description"`
	Enum        []any                      `json:"enum"`
	Default     any                        `json:"default"`
	Properties  map[string]json.RawMessage `json:"properties"`
}

func schemaFields(prefix string, props map[string]json.RawMessage, defaults map[string]any, depth int) []Param {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Param
	for _, k := range keys {
		var p schemaProp
		if json.Unmarshal(props[k], &p) != nil {
			continue
		}
		typ, _ := p.Type.(string)
		path := strings.TrimPrefix(prefix+"."+k, ".")
		def := p.Default
		if v, ok := defaults[k]; ok {
			def = v
		}
		if typ == "object" && depth < 1 && len(p.Properties) > 0 {
			sub, _ := defaults[k].(map[string]any)
			out = append(out, schemaFields(path, p.Properties, sub, depth+1)...)
			continue
		}
		if typ != "string" && typ != "integer" && typ != "number" && typ != "boolean" {
			continue
		}
		f := Param{Name: schemaPrefix + path, Label: or(p.Title, path), Type: "text", Help: p.Description, Section: "Parameters"}
		if def != nil {
			f.Default = fmt.Sprint(def)
		}
		switch {
		case typ == "boolean":
			f.Type, f.Options = "select", []string{"true", "false"}
			if f.Default == "" {
				f.Default = "false"
			}
		case len(p.Enum) > 0:
			f.Type = "select"
			for _, e := range p.Enum {
				f.Options = append(f.Options, fmt.Sprint(e))
			}
		case typ == "integer":
			f.Type, f.Pattern = "number", `^-?[0-9]+$`
		case typ == "number":
			f.Type, f.Pattern = "number", `^-?[0-9]+(\.[0-9]+)?$`
		}
		out = append(out, f)
	}
	return out
}

// chartValues are the values to install with: the YAML editor's, with
// each schema field's value set at its path.
func chartValues(c *chart.Chart, values map[string]string) (map[string]any, error) {
	out := map[string]any{}
	if y := values[paramValues]; strings.TrimSpace(y) != "" {
		if err := yaml.Unmarshal([]byte(y), &out); err != nil {
			return nil, fmt.Errorf("%w: Values (YAML): %v", ErrBadRequest, err)
		}
		if out == nil {
			out = map[string]any{}
		}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		if strings.HasPrefix(k, schemaPrefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	schemaTypes := map[string]string{}
	if len(c.Schema) > 0 {
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		_ = json.Unmarshal(c.Schema, &s)
		for _, p := range schemaFields("", s.Properties, nil, 0) {
			schemaTypes[p.Name] = p.Type
			if len(p.Options) == 2 && p.Options[0] == "true" {
				schemaTypes[p.Name] = "boolean"
			}
		}
	}
	for _, k := range keys {
		raw := values[k]
		var v any = raw
		switch schemaTypes[k] {
		case "boolean":
			v = raw == "true"
		case "number":
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
				v = n
			} else if f, err := strconv.ParseFloat(raw, 64); err == nil {
				v = f
			} else if raw != "" {
				return nil, fmt.Errorf("%w: %s %q is not a number", ErrBadRequest, strings.TrimPrefix(k, schemaPrefix), raw)
			}
		}
		if raw == "" && schemaTypes[k] != "boolean" {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(k, schemaPrefix), ".")
		m := out
		for _, p := range parts[:len(parts)-1] {
			next, _ := m[p].(map[string]any)
			if next == nil {
				next = map[string]any{}
				m[p] = next
			}
			m = next
		}
		m[parts[len(parts)-1]] = v
	}
	return out, nil
}

// chartPlan builds a chart deploy's plan.
func chartPlan(c *chart.Chart, sourceID, product, version string, values map[string]string, links []AppLink) (Plan, error) {
	vals, err := chartValues(c, values)
	if err != nil {
		return Plan{}, err
	}
	md := c.Metadata
	return Plan{
		Chart: &ChartPlan{Chart: c, Values: vals},
		Descriptor: Descriptor{Type: md.Name, Version: or(version, md.Version), Description: md.Description,
			Links: links, Keywords: append([]string{"helm"}, md.Keywords...)},
		Annotations: map[string]string{AnnotationSource: sourceID, AnnotationProduct: product},
	}, nil
}
