package hfmodels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Browsing the whole public Hub for Model Garden's Hugging Face tab (#2148),
// through Hugging Face's anonymous public API:
//
//   - GET /api/models with search, pipeline_tag, library and a license:
//     filter, sorted by downloads, likes, trendingScore or lastModified,
//     descending, a page at a time: the next page's URL is the Link header's
//     rel="next", whose cursor parameter is passed back as it is;
//   - GET /api/models/{id}?blobs=true for a model's card: licence, task,
//     library, gated status, files with their sizes, downloads and likes;
//   - GET /{id}/resolve/{revision}/README.md for the card's text, which
//     Hugging Face serves for gated models too.
//
// Nothing here sends a token: browsing is anonymous. CheckAccess is the one
// call that carries one, the end user's own, to ask Hugging Face whether it
// opens a gated model, as Google's deploy does before it starts.

// Sorts are the orders the Hub API sorts by, all descending.
var Sorts = []string{"trendingScore", "downloads", "likes", "lastModified"}

// SearchQuery is one page of a Hub search.
type SearchQuery struct {
	Search  string
	Task    string // pipeline_tag
	Library string // library_name
	License string // license:<id> tag
	Sort    string // one of Sorts; trendingScore when empty
	Cursor  string // from the previous page's Next
	Limit   int    // 30 when zero, at most 100
}

// HubModel is one model of a search page.
type HubModel struct {
	ID           string   `json:"id"`
	Author       string   `json:"author"`
	Task         string   `json:"task,omitempty"`
	Library      string   `json:"library,omitempty"`
	License      string   `json:"license"`
	Gated        string   `json:"gated,omitempty"` // "", "auto" or "manual"
	Downloads    int64    `json:"downloads"`
	Likes        int64    `json:"likes"`
	Trending     int64    `json:"trending"`
	LastModified string   `json:"lastModified,omitempty"`
	Parameters   int64    `json:"parameters,omitempty"`
	Tags         []string `json:"tags,omitempty"`
}

// SearchPage is a page of models and the cursor of the next one.
type SearchPage struct {
	Models []HubModel `json:"models"`
	Next   string     `json:"next,omitempty"`
}

var (
	tokenPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	cursorPattern = regexp.MustCompile(`^[A-Za-z0-9_=+/-]+$`)
)

// expand is the fields a search asks for: the list's default leaves out
// gated, which Model Garden marks on every card.
var expand = []string{"gated", "downloads", "likes", "pipeline_tag", "library_name", "tags", "trendingScore",
	"lastModified", "safetensors"}

// rawModel is the part of the Hub's model JSON both calls read.
type rawModel struct {
	ID           string          `json:"id"`
	Author       string          `json:"author"`
	SHA          string          `json:"sha"`
	Gated        json.RawMessage `json:"gated"`
	PipelineTag  string          `json:"pipeline_tag"`
	LibraryName  string          `json:"library_name"`
	Tags         []string        `json:"tags"`
	Downloads    int64           `json:"downloads"`
	Likes        int64           `json:"likes"`
	Trending     int64           `json:"trendingScore"`
	LastModified string          `json:"lastModified"`
	Private      bool            `json:"private"`
	Disabled     bool            `json:"disabled"`
	CardData     struct {
		License     any    `json:"license"`
		LicenseName string `json:"license_name"`
		LicenseLink string `json:"license_link"`
		GatedPrompt string `json:"extra_gated_prompt"`
	} `json:"cardData"`
	Safetensors *struct {
		Total int64 `json:"total"`
	} `json:"safetensors"`
	Siblings []struct {
		RFilename string `json:"rfilename"`
		Size      int64  `json:"size"`
		LFS       *struct {
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"lfs"`
	} `json:"siblings"`
}

func gatedOf(raw json.RawMessage) string {
	if g := strings.Trim(string(raw), `"`); g != "" && g != "false" && g != "null" {
		return g
	}
	return ""
}

func licenseOf(r rawModel) string {
	switch l := r.CardData.License.(type) {
	case string:
		if l != "" {
			return l
		}
	case []any:
		parts := []string{}
		for _, v := range l {
			parts = append(parts, fmt.Sprint(v))
		}
		if len(parts) > 0 {
			return strings.Join(parts, ", ")
		}
	}
	for _, t := range r.Tags {
		if l, ok := strings.CutPrefix(t, "license:"); ok {
			return l
		}
	}
	return "not stated"
}

func (r rawModel) hubModel() HubModel {
	author := r.Author
	if author == "" {
		author, _, _ = strings.Cut(r.ID, "/")
	}
	m := HubModel{ID: r.ID, Author: author, Task: r.PipelineTag, Library: r.LibraryName, License: licenseOf(r),
		Gated: gatedOf(r.Gated), Downloads: r.Downloads, Likes: r.Likes, Trending: r.Trending, LastModified: r.LastModified}
	if r.Safetensors != nil {
		m.Parameters = r.Safetensors.Total
	}
	for _, t := range r.Tags {
		// The tags a card shows: not the long dataset, arXiv and base-model
		// lists, which the card's README covers.
		if !strings.Contains(t, ":") {
			m.Tags = append(m.Tags, t)
		}
	}
	return m
}

// CheckQuery validates a search, as the Hub would refuse it.
func CheckQuery(q SearchQuery) error {
	if len(q.Search) > 200 {
		return fmt.Errorf("%w: search is longer than 200 characters", ErrBadID)
	}
	for name, v := range map[string]string{"task": q.Task, "library": q.Library, "license": q.License} {
		if v != "" && !tokenPattern.MatchString(v) {
			return fmt.Errorf("%w: %s %q is not a Hub tag", ErrBadID, name, v)
		}
	}
	if q.Sort != "" {
		ok := false
		for _, s := range Sorts {
			ok = ok || s == q.Sort
		}
		if !ok {
			return fmt.Errorf("%w: sort %q is not one of %s", ErrBadID, q.Sort, strings.Join(Sorts, ", "))
		}
	}
	if q.Cursor != "" && (len(q.Cursor) > 4096 || !cursorPattern.MatchString(q.Cursor)) {
		return fmt.Errorf("%w: cursor is not a Hub page cursor", ErrBadID)
	}
	if q.Limit < 0 || q.Limit > 100 {
		return fmt.Errorf("%w: limit %d is not between 1 and 100", ErrBadID, q.Limit)
	}
	return nil
}

// SearchURL is the Hub API URL of a search page.
func (m *Manager) SearchURL(q SearchQuery) string {
	v := url.Values{}
	if q.Search != "" {
		v.Set("search", q.Search)
	}
	if q.Task != "" {
		v.Set("pipeline_tag", q.Task)
	}
	if q.Library != "" {
		v.Set("library", q.Library)
	}
	if q.License != "" {
		v.Set("filter", "license:"+q.License)
	}
	sort := q.Sort
	if sort == "" {
		sort = "trendingScore"
	}
	v.Set("sort", sort)
	v.Set("direction", "-1")
	limit := q.Limit
	if limit == 0 {
		limit = 30
	}
	v.Set("limit", strconv.Itoa(limit))
	for _, e := range expand {
		v.Add("expand[]", e)
	}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	return m.base() + "/api/models?" + v.Encode()
}

// nextCursor reads the cursor of a Link header's rel="next".
func nextCursor(link string) string {
	for _, part := range strings.Split(link, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start, end := strings.Index(part, "<"), strings.Index(part, ">")
		if start < 0 || end < start {
			continue
		}
		u, err := url.Parse(part[start+1 : end])
		if err != nil {
			continue
		}
		if c := u.Query().Get("cursor"); len(c) <= 4096 && cursorPattern.MatchString(c) {
			return c
		}
	}
	return ""
}

// Search reads one page of the Hub's public models, anonymously.
func (m *Manager) Search(ctx context.Context, q SearchQuery) (SearchPage, error) {
	if err := CheckQuery(q); err != nil {
		return SearchPage{}, err
	}
	resp, err := m.get(ctx, m.SearchURL(q), false)
	if err != nil {
		return SearchPage{}, fmt.Errorf("search huggingface.co: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return SearchPage{}, fmt.Errorf("search huggingface.co: HTTP %d: %s", resp.StatusCode, hubMessage(b))
	}
	var raw []rawModel
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&raw); err != nil {
		return SearchPage{}, fmt.Errorf("decode huggingface.co's search: %w", err)
	}
	page := SearchPage{Models: []HubModel{}, Next: nextCursor(resp.Header.Get("Link"))}
	for _, r := range raw {
		if r.Private || CheckID(r.ID) != nil {
			continue
		}
		page.Models = append(page.Models, r.hubModel())
	}
	return page, nil
}

// hubMessage is the Hub's {"error": ...} text, or the body.
func hubMessage(b []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(b))
}

// Card is a model's page in Model Garden: what Hugging Face says about it,
// its files and their sizes, and its README.
type Card struct {
	HubModel
	Revision    string `json:"revision"`
	LicenseName string `json:"licenseName,omitempty"`
	LicenseLink string `json:"licenseLink,omitempty"`
	GatedPrompt string `json:"gatedPrompt,omitempty"`
	Files       []File `json:"files"`
	Bytes       int64  `json:"bytes"`
	// WeightsBytes is what a serving runtime downloads: the safetensors
	// files, or the PyTorch .bin files of a model with none.
	WeightsBytes int64  `json:"weightsBytes"`
	README       string `json:"readme"`
	Disabled     bool   `json:"disabled,omitempty"`
}

// weightsBytes sums the files a serving runtime loads.
func weightsBytes(files []File) int64 {
	var st, bin int64
	for _, f := range files {
		if strings.Contains(f.Path, "/") && !strings.HasPrefix(f.Path, "0_") {
			// Subfolders hold other formats (onnx/, openvino/) or a
			// sentence-transformers module's config.
			continue
		}
		switch {
		case strings.HasSuffix(f.Path, ".safetensors"):
			st += f.Size
		case strings.HasSuffix(f.Path, ".bin") && strings.HasPrefix(f.Path, "pytorch_model"):
			bin += f.Size
		}
	}
	if st > 0 {
		return st
	}
	return bin
}

// Card reads a model's card, anonymously.
func (m *Manager) Card(ctx context.Context, id string) (Card, error) {
	if err := CheckID(id); err != nil {
		return Card{}, err
	}
	resp, err := m.get(ctx, m.base()+"/api/models/"+id+"?blobs=true", false)
	if err != nil {
		return Card{}, fmt.Errorf("read %s from Hugging Face: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
		return Card{}, fmt.Errorf("%w: %s is not a public model on huggingface.co", ErrNotFound, id)
	}
	if resp.StatusCode != http.StatusOK {
		return Card{}, fmt.Errorf("read %s from Hugging Face: HTTP %d", id, resp.StatusCode)
	}
	var raw rawModel
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&raw); err != nil {
		return Card{}, fmt.Errorf("decode %s's model info: %w", id, err)
	}
	if raw.ID == "" {
		raw.ID = id
	}
	c := Card{HubModel: raw.hubModel(), Revision: raw.SHA, LicenseName: raw.CardData.LicenseName,
		LicenseLink: safeLink(raw.CardData.LicenseLink), GatedPrompt: StripHTML(raw.CardData.GatedPrompt), Disabled: raw.Disabled}
	if !revisionPattern.MatchString(c.Revision) {
		return Card{}, fmt.Errorf("%s's model info has no commit revision", id)
	}
	hasReadme := false
	for _, s := range raw.Siblings {
		if !safePath(s.RFilename) {
			continue
		}
		f := File{Path: s.RFilename, Size: s.Size}
		if s.LFS != nil {
			f.SHA256, f.Size = s.LFS.SHA256, s.LFS.Size
		}
		c.Files = append(c.Files, f)
		c.Bytes += f.Size
		hasReadme = hasReadme || s.RFilename == "README.md"
	}
	c.WeightsBytes = weightsBytes(c.Files)
	if hasReadme {
		c.README = m.readme(ctx, id, c.Revision)
	}
	return c, nil
}

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// readmeLimit bounds a card's README.
const readmeLimit = 256 << 10

func (m *Manager) readme(ctx context.Context, id, rev string) string {
	resp, err := m.get(ctx, m.base()+"/"+id+"/resolve/"+rev+"/README.md", false)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, readmeLimit))
	return SanitizeREADME(string(b))
}

// SanitizeREADME is a model card's markdown with its YAML front matter
// (which the card's facts show) and every HTML element removed: script,
// style, iframe and comment blocks with their content, any other tag with
// its text kept. The console renders what is left as markdown into text
// nodes, so no markup from the Hub reaches the page.
func SanitizeREADME(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if strings.HasPrefix(s, "---\n") {
		if end := strings.Index(s[4:], "\n---"); end >= 0 {
			s = s[4+end+4:]
			s = strings.TrimPrefix(s, "\n")
		}
	}
	return strings.TrimSpace(StripHTML(s))
}

var (
	blockPattern = regexp.MustCompile(`(?is)<!--.*?-->|<(script|style|iframe|object|embed|noscript|template|svg|math)\b.*?</(script|style|iframe|object|embed|noscript|template|svg|math)\s*>`)
	openBlock    = regexp.MustCompile(`(?is)<(script|style|iframe|object|embed|noscript|template|svg|math)\b.*$`)
	tagPattern   = regexp.MustCompile(`(?s)</?[A-Za-z][A-Za-z0-9-]*(\s[^<>]*)?/?>`)
)

// StripHTML removes HTML elements from s, keeping the text between tags.
func StripHTML(s string) string {
	s = blockPattern.ReplaceAllString(s, "")
	s = openBlock.ReplaceAllString(s, "")
	s = tagPattern.ReplaceAllString(s, "")
	// A stray "<" left by malformed markup cannot open an element in a text
	// node, but neither does it read as anything: keep it.
	return s
}

func safeLink(u string) string {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
		return ""
	}
	return p.String()
}

// CheckAccess asks Hugging Face whether token opens model id's files at
// revision, reading one file's headers. It returns ErrGated when the token
// is refused. The token is sent only to the Hub, in the Authorization
// header, and never kept, logged or put in an error.
func (m *Manager) CheckAccess(ctx context.Context, id, revision, file, token string) error {
	if err := CheckID(id); err != nil {
		return err
	}
	if !revisionPattern.MatchString(revision) || !safePath(file) {
		return fmt.Errorf("%w: no file of %s to check access with", ErrNotFound, id)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		m.base()+"/"+id+"/resolve/"+revision+"/"+(&url.URL{Path: file}).EscapedPath(), nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// The Hub answers a resolve with a redirect to its CDN; whether the
	// token opens the file is the Hub's first answer, so it is not followed.
	c := *m.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("ask huggingface.co about %s: %s", id, Scrub(err.Error(), token))
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode < 400:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: Model %s is a gated Hugging Face model. The token is not valid or does not have permission", ErrGated, id)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s has no file %s at %s", ErrNotFound, id, file, revision[:12])
	}
	return fmt.Errorf("ask huggingface.co about %s: HTTP %d", id, resp.StatusCode)
}

// ReadFile reads one small file of a model at revision, with token when it
// is not empty (a gated model's config.json, for its context length).
func (m *Manager) ReadFile(ctx context.Context, id, revision, file, token string, limit int64) ([]byte, error) {
	if CheckID(id) != nil || !revisionPattern.MatchString(revision) || !safePath(file) {
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, id, file)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		m.base()+"/"+id+"/resolve/"+revision+"/"+(&url.URL{Path: file}).EscapedPath(), nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("read %s from huggingface.co: %s", file, Scrub(err.Error(), token))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read %s of %s from huggingface.co: HTTP %d", file, id, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// Scrub replaces every occurrence of token in s; an empty token changes
// nothing.
func Scrub(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "<token>")
}
