package logging

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Saved queries and log scopes (#1519), Logging v2 resources Google's REST
// API serves at {parent}/locations/*/savedQueries and
// {parent}/locations/*/logScopes, as logging.googleapis.com's discovery
// document describes them. google.logging.v2's protos in
// cloud.google.com/go/logging carry neither, so the transcoder cannot serve
// them: this serves their JSON by hand, beside it, and their official client
// is the generated google.golang.org/api/logging/v2. Over gRPC they answer
// UNIMPLEMENTED, as no published proto names them.
//
// Choices Google documents only in part (UNVERIFIED): a list's filter is
// refused as UNIMPLEMENTED rather than ignored; a create with an ID that is
// taken is ALREADY_EXISTS; an update's mask may name only the fields listed
// in savedQueryMutable and logScopeMutable.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/projects.locations.savedQueries/list

// savedQuery is a SavedQuery resource.
type savedQuery struct {
	Name              string             `json:"name,omitempty"`
	DisplayName       string             `json:"displayName,omitempty"`
	Description       string             `json:"description,omitempty"`
	LoggingQuery      *loggingQuery      `json:"loggingQuery,omitempty"`
	OpsAnalyticsQuery *opsAnalyticsQuery `json:"opsAnalyticsQuery,omitempty"`
	Visibility        string             `json:"visibility,omitempty"`
	CreateTime        string             `json:"createTime,omitempty"`
	UpdateTime        string             `json:"updateTime,omitempty"`
}

type loggingQuery struct {
	Filter            string         `json:"filter,omitempty"`
	SummaryFields     []summaryField `json:"summaryFields,omitempty"`
	SummaryFieldStart int32          `json:"summaryFieldStart,omitempty"`
	SummaryFieldEnd   int32          `json:"summaryFieldEnd,omitempty"`
}

type summaryField struct {
	Field string `json:"field,omitempty"`
}

type opsAnalyticsQuery struct {
	SQLQueryText string `json:"sqlQueryText,omitempty"`
}

// logScope is a LogScope resource.
type logScope struct {
	Name          string   `json:"name,omitempty"`
	ResourceNames []string `json:"resourceNames,omitempty"`
	Description   string   `json:"description,omitempty"`
	CreateTime    string   `json:"createTime,omitempty"`
	UpdateTime    string   `json:"updateTime,omitempty"`
}

var (
	savedQueryMutable = map[string]bool{"displayName": true, "description": true, "loggingQuery": true,
		"opsAnalyticsQuery": true, "visibility": true}
	logScopeMutable = map[string]bool{"resourceNames": true, "description": true}
	extraIDRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_\-]{0,99}$`)
	// A log scope names projects and log views (docs.cloud.google.com/logging/docs/log-scope).
	scopeResourceRe = regexp.MustCompile(`^(projects/[^/]+|projects/[^/]+/locations/[^/]+/buckets/[^/]+/views/[^/]+)$`)
)

// maxScopeResources is a log scope's limit on resource_names (documented
// as at most 100 entries, of which 5 projects; only the total is checked).
const maxScopeResources = 100

// extraCollection is savedQueries or logScopes.
type extraCollection struct {
	name    string // the URL's collection
	idParam string // the create's ID query parameter
	listKey string // the list response's field
	parents map[string]bool
}

var (
	collSavedQueries = extraCollection{"savedQueries", "savedQueryId", "savedQueries",
		map[string]bool{collProjects: true, collFolders: true, collOrgs: true, collBilling: true}}
	collLogScopes = extraCollection{"logScopes", "logScopeId", "logScopes",
		map[string]bool{collProjects: true, collFolders: true, collOrgs: true}}
)

func (c extraCollection) prefix(p parent, location string) string {
	return "logging/" + strings.ToLower(c.name) + "/" + p.seg() + "/" + hex.EncodeToString([]byte(location)) + "/"
}

func (c extraCollection) key(p parent, location, id string) string {
	return c.prefix(p, location) + hex.EncodeToString([]byte(id))
}

// ServeExtra serves a saved query or log scope request and reports true,
// or reports false for any other path, which the transcoder serves.
func (a *Admin) ServeExtra(w http.ResponseWriter, r *http.Request) bool {
	path, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	if !ok {
		return false
	}
	var c extraCollection
	var before, after string
	switch {
	case strings.Contains(path, "/savedQueries"):
		c = collSavedQueries
	case strings.Contains(path, "/logScopes"):
		c = collLogScopes
	default:
		return false
	}
	before, after, _ = strings.Cut(path, "/"+c.name)
	if after != "" && !strings.HasPrefix(after, "/") {
		return false
	}
	id := strings.TrimPrefix(after, "/")
	if err := a.serveExtra(w, r, c, before, id); err != nil {
		rest.WriteError(w, err)
	}
	return true
}

func (a *Admin) serveExtra(w http.ResponseWriter, r *http.Request, c extraCollection, parentName, id string) error {
	p, tail, err := cutParent("parent", parentName)
	if err != nil {
		return err
	}
	if !c.parents[p.coll] {
		return apierror.InvalidArgument("%s are not kept under %s", c.name, p.coll)
	}
	loc, ok := strings.CutPrefix(tail, "locations/")
	listAll := loc == "-" && id == "" && r.Method == http.MethodGet
	if !ok || loc == "" || strings.Contains(loc, "/") || (!locationRe.MatchString(loc) && !listAll) {
		return apierror.InvalidArgument("parent %q is not {parent}/locations/{location}", parentName)
	}
	if c.name == "logScopes" && loc != "global" && !listAll {
		return apierror.InvalidArgument("log scopes are kept in the global location, not %s", loc)
	}
	if strings.Contains(id, "/") || strings.Contains(id, ":") {
		return apierror.NotFound("%s/%s does not exist", parentName, c.name+"/"+id)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.check(p); err != nil {
		return err
	}
	switch {
	case id == "" && r.Method == http.MethodPost:
		return a.createExtra(w, r, c, p, loc)
	case id == "" && r.Method == http.MethodGet:
		return a.listExtra(w, r, c, p, loc)
	case id != "" && r.Method == http.MethodGet:
		raw, err := a.getExtra(c, p, loc, id)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, json.RawMessage(raw))
	case id != "" && r.Method == http.MethodPatch:
		return a.patchExtra(w, r, c, p, loc, id)
	case id != "" && r.Method == http.MethodDelete:
		if _, err := a.getExtra(c, p, loc, id); err != nil {
			return err
		}
		if err := a.db.Delete(c.key(p, loc, id)); err != nil {
			return apierror.Internal(err, "deleting the %s", c.name)
		}
		return rest.WriteJSON(w, http.StatusOK, map[string]any{})
	}
	return apierror.NotFound("no %s method answers %s %s", c.name, r.Method, r.URL.Path)
}

func extraName(c extraCollection, p parent, loc, id string) string {
	return p.String() + "/locations/" + loc + "/" + c.name + "/" + id
}

func (a *Admin) getExtra(c extraCollection, p parent, loc, id string) ([]byte, error) {
	raw, err := a.db.Get(c.key(p, loc, id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, apierror.NotFound("%s does not exist", extraName(c, p, loc, id))
	}
	if err != nil {
		return nil, apierror.Internal(err, "reading the %s", c.name)
	}
	return raw, nil
}

// checkExtra validates a resource's fields.
func checkExtra(c extraCollection, v any) error {
	switch x := v.(type) {
	case *savedQuery:
		if (x.LoggingQuery == nil) == (x.OpsAnalyticsQuery == nil) {
			return apierror.InvalidArgument("a saved query needs exactly one of loggingQuery and opsAnalyticsQuery")
		}
		if x.LoggingQuery != nil && len(x.LoggingQuery.Filter) > maxFilterLen {
			return apierror.InvalidArgument("loggingQuery.filter is longer than %d characters", maxFilterLen)
		}
		switch x.Visibility {
		case "PRIVATE", "SHARED":
		default:
			return apierror.InvalidArgument("visibility must be PRIVATE or SHARED, got %q", x.Visibility)
		}
		return checkDescription(x.Description)
	case *logScope:
		if len(x.ResourceNames) == 0 {
			return apierror.InvalidArgument("resourceNames must name at least one project or log view")
		}
		if len(x.ResourceNames) > maxScopeResources {
			return apierror.InvalidArgument("resourceNames has %d entries; at most %d", len(x.ResourceNames), maxScopeResources)
		}
		for _, n := range x.ResourceNames {
			if !scopeResourceRe.MatchString(n) {
				return apierror.InvalidArgument("resourceNames entry %q is not projects/{project} or a log view's name", n)
			}
		}
		return checkDescription(x.Description)
	}
	return nil
}

func newExtra(c extraCollection) any {
	if c.name == "savedQueries" {
		return &savedQuery{}
	}
	return &logScope{}
}

// stamp sets a resource's server-owned fields.
func stamp(v any, name, created, updated string) {
	switch x := v.(type) {
	case *savedQuery:
		x.Name, x.CreateTime, x.UpdateTime = name, created, updated
	case *logScope:
		x.Name, x.CreateTime, x.UpdateTime = name, created, updated
	}
}

func (a *Admin) createExtra(w http.ResponseWriter, r *http.Request, c extraCollection, p parent, loc string) error {
	id := r.URL.Query().Get(c.idParam)
	if id == "" && c.name == "savedQueries" {
		// The ID is optional for a saved query; one is made.
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		id = "q" + hex.EncodeToString(b)
	}
	if !extraIDRe.MatchString(id) {
		return apierror.InvalidArgument("%s %q must be up to 100 letters, digits, _ and -, starting with a letter or digit", c.idParam, id)
	}
	v := newExtra(c)
	if err := rest.DecodeJSON(r, v); err != nil {
		return err
	}
	if err := checkExtra(c, v); err != nil {
		return err
	}
	if _, err := a.db.Get(c.key(p, loc, id)); err == nil {
		return apierror.AlreadyExists("%s already exists", extraName(c, p, loc, id))
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	stamp(v, extraName(c, p, loc, id), now, now)
	return a.putExtra(w, c, p, loc, id, v)
}

func (a *Admin) putExtra(w http.ResponseWriter, c extraCollection, p parent, loc, id string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return apierror.Internal(err, "encoding the %s", c.name)
	}
	if err := a.db.Put(c.key(p, loc, id), raw); err != nil {
		return apierror.Internal(err, "writing the %s", c.name)
	}
	return rest.WriteJSON(w, http.StatusOK, json.RawMessage(raw))
}

func (a *Admin) patchExtra(w http.ResponseWriter, r *http.Request, c extraCollection, p parent, loc, id string) error {
	raw, err := a.getExtra(c, p, loc, id)
	if err != nil {
		return err
	}
	cur := newExtra(c)
	if err := json.Unmarshal(raw, cur); err != nil {
		return apierror.Internal(err, "decoding the %s", c.name)
	}
	body := newExtra(c)
	if err := rest.DecodeJSON(r, body); err != nil {
		return err
	}
	var paths []string
	for _, f := range strings.Split(r.URL.Query().Get("updateMask"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			paths = append(paths, f)
		}
	}
	if len(paths) == 0 {
		return apierror.InvalidArgument("updateMask must name at least one field")
	}
	mutable := savedQueryMutable
	if c.name == "logScopes" {
		mutable = logScopeMutable
	}
	for _, f := range paths {
		if !mutable[f] {
			return apierror.InvalidArgument("updateMask path %q is not a field that can be updated", f)
		}
		switch x := cur.(type) {
		case *savedQuery:
			b := body.(*savedQuery)
			switch f {
			case "displayName":
				x.DisplayName = b.DisplayName
			case "description":
				x.Description = b.Description
			case "loggingQuery":
				x.LoggingQuery = b.LoggingQuery
			case "opsAnalyticsQuery":
				x.OpsAnalyticsQuery = b.OpsAnalyticsQuery
			case "visibility":
				x.Visibility = b.Visibility
			}
		case *logScope:
			b := body.(*logScope)
			switch f {
			case "resourceNames":
				x.ResourceNames = b.ResourceNames
			case "description":
				x.Description = b.Description
			}
		}
	}
	if err := checkExtra(c, cur); err != nil {
		return err
	}
	switch x := cur.(type) {
	case *savedQuery:
		x.UpdateTime = a.now().UTC().Format(time.RFC3339Nano)
	case *logScope:
		x.UpdateTime = a.now().UTC().Format(time.RFC3339Nano)
	}
	return a.putExtra(w, c, p, loc, id, cur)
}

func (a *Admin) listExtra(w http.ResponseWriter, r *http.Request, c extraCollection, p parent, loc string) error {
	q := r.URL.Query()
	if q.Get("filter") != "" {
		return apierror.Unimplemented("filtering a %s list is not implemented", c.name)
	}
	size := 0
	if s := q.Get("pageSize"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return apierror.InvalidArgument("pageSize %q is not a number", s)
		}
		size = n
	}
	// "-" lists every location's.
	prefix := c.prefix(p, loc)
	if loc == "-" {
		prefix = "logging/" + strings.ToLower(c.name) + "/" + p.seg() + "/"
	}
	keys, err := a.db.List(prefix)
	if err != nil {
		return apierror.Internal(err, "listing %s", c.name)
	}
	values := map[string][]byte{}
	for _, k := range keys {
		raw, err := a.db.Get(k)
		if err != nil {
			continue
		}
		v := newExtra(c)
		if json.Unmarshal(raw, v) != nil {
			continue
		}
		switch x := v.(type) {
		case *savedQuery:
			values[x.Name] = raw
		case *logScope:
			values[x.Name] = raw
		}
	}
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	sort.Strings(names)
	page, next, err := paging.Page("logging-"+c.name+"|"+p.String()+"|"+loc, names, q.Get("pageToken"), size)
	if err != nil {
		return apierror.InvalidArgument("pageToken: %v", err)
	}
	items := make([]json.RawMessage, 0, len(page))
	for _, n := range page {
		items = append(items, values[n])
	}
	out := map[string]any{c.listKey: items}
	if next != "" {
		out["nextPageToken"] = next
	}
	return rest.WriteJSON(w, http.StatusOK, out)
}

// CallExtra calls the saved query and log scope JSON API in process, for
// the console, and returns the status and body a client would get.
func (a *Admin) CallExtra(method, path string, body []byte) (int, []byte) {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if len(body) > 0 {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	if !a.ServeExtra(w, r) {
		return http.StatusNotFound, []byte(`{"error":{"message":"not a saved query or log scope path"}}`)
	}
	return w.Code, w.Body.Bytes()
}
