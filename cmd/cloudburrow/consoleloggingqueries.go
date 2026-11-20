package main

// Cloud Logging's saved queries and log scopes (#1519) on screens of their
// own beside the Logs Explorer: list, create and delete, through the same
// JSON API Google's generated Logging v2 client calls.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
)

// callLoggingJSON calls the in-process JSON API and decodes its answer.
func callLoggingJSON(a *logging.Admin, method, path string, body any, out any) error {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return err
		}
	}
	code, resp := a.CallExtra(method, path, raw)
	if code != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(resp, &e) == nil && e.Error.Message != "" {
			return errors.New(e.Error.Message)
		}
		return fmt.Errorf("HTTP %d", code)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(resp, out)
}

// ---- Saved queries ----

type loggingSavedQueriesProvider struct{ loggingAdminScreen }

func (loggingSavedQueriesProvider) ID() string    { return "logging-saved-queries" }
func (loggingSavedQueriesProvider) Title() string { return "Saved queries" }

type consoleSavedQuery struct {
	Name         string `json:"name,omitempty"`
	DisplayName  string `json:"displayName,omitempty"`
	Description  string `json:"description,omitempty"`
	Visibility   string `json:"visibility,omitempty"`
	UpdateTime   string `json:"updateTime,omitempty"`
	LoggingQuery *struct {
		Filter        string `json:"filter"`
		SummaryFields []struct {
			Field string `json:"field"`
		} `json:"summaryFields,omitempty"`
		SummaryFieldStart int32 `json:"summaryFieldStart,omitempty"`
		SummaryFieldEnd   int32 `json:"summaryFieldEnd,omitempty"`
	} `json:"loggingQuery,omitempty"`
	OpsAnalyticsQuery *struct {
		SQLQueryText string `json:"sqlQueryText"`
	} `json:"opsAnalyticsQuery,omitempty"`
}

func (p loggingSavedQueriesProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "Visibility", "Query", "Updated"}, NameColumn: "Saved query", Noun: "saved queries"}
	a, err := p.admin()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Cloud Logging lists saved queries per project. Choose one in the toolbar."
		return out, nil
	}
	var resp struct {
		SavedQueries []consoleSavedQuery `json:"savedQueries"`
	}
	if err := callLoggingJSON(a, http.MethodGet, "/v2/projects/"+url.PathEscape(project)+"/locations/-/savedQueries", nil, &resp); err != nil {
		return console.Listing{}, err
	}
	for _, q := range resp.SavedQueries {
		out.Items = append(out.Items, console.Resource{Name: q.Name, Fields: savedQueryFields(q)})
	}
	out.Total = len(out.Items)
	return out, nil
}

// savedQueryFields are a saved query's row: its columns, and the raw values
// the Logs Explorer's Query library runs and edits it by (#1863).
func savedQueryFields(q consoleSavedQuery) map[string]string {
	f := map[string]string{"Display name": q.DisplayName, "Visibility": q.Visibility, "Query": "—", "Updated": q.UpdateTime,
		"description": q.Description, "visibility": q.Visibility}
	switch {
	case q.LoggingQuery != nil:
		f["Query"] = orDash(q.LoggingQuery.Filter)
		f["kind"], f["filter"] = "logging", q.LoggingQuery.Filter
		var fields []string
		for _, sf := range q.LoggingQuery.SummaryFields {
			fields = append(fields, sf.Field)
		}
		f["summaryFields"] = strings.Join(fields, ",")
		if q.LoggingQuery.SummaryFieldStart > 0 {
			f["summaryFieldStart"] = strconv.Itoa(int(q.LoggingQuery.SummaryFieldStart))
		}
		if q.LoggingQuery.SummaryFieldEnd > 0 {
			f["summaryFieldEnd"] = strconv.Itoa(int(q.LoggingQuery.SummaryFieldEnd))
		}
	case q.OpsAnalyticsQuery != nil:
		f["Query"] = "SQL: " + q.OpsAnalyticsQuery.SQLQueryText
		f["kind"], f["filter"] = "sql", q.OpsAnalyticsQuery.SQLQueryText
	}
	return f
}

// savedQueryBody is the SavedQuery a create or edit sends: a Logging query
// with its summary fields (a comma-separated summaryFields, and the
// characters kept from the start or the end), or a Log Analytics one.
func savedQueryBody(values map[string]string) (map[string]any, error) {
	body := map[string]any{"displayName": values["displayName"], "description": values["description"], "visibility": values["visibility"]}
	if values["kind"] == savedQuerySQL {
		body["opsAnalyticsQuery"] = map[string]string{"sqlQueryText": values["query"]}
		return body, nil
	}
	lq := map[string]any{"filter": values["query"]}
	var fields []map[string]string
	for _, f := range strings.Split(values["summaryFields"], ",") {
		if f = strings.TrimSpace(f); f != "" {
			fields = append(fields, map[string]string{"field": f})
		}
	}
	if len(fields) > 0 {
		lq["summaryFields"] = fields
	}
	for _, k := range []string{"summaryFieldStart", "summaryFieldEnd"} {
		v := strings.TrimSpace(values[k])
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%s %q is not a number of characters", k, v)
		}
		lq[k] = n
	}
	body["loggingQuery"] = lq
	return body, nil
}

// Edit implements console.Editor: the Query library's Edit changes a saved
// query's name, description, query, summary fields and sharing through
// savedQueries.patch.
func (p loggingSavedQueriesProvider) Edit(_ context.Context, project string, path []string, values map[string]string) error {
	a, err := p.admin()
	if err != nil {
		return err
	}
	name := strings.Join(path, "/")
	if !strings.HasPrefix(name, "projects/"+project+"/locations/") || !strings.Contains(name, "/savedQueries/") {
		return fmt.Errorf("%s is not a saved query of project %s", name, project)
	}
	if strings.TrimSpace(values["displayName"]) == "" {
		return errors.New("a saved query needs a name")
	}
	body, err := savedQueryBody(values)
	if err != nil {
		return err
	}
	mask := "displayName,description,visibility,loggingQuery"
	if values["kind"] == savedQuerySQL {
		mask = "displayName,description,visibility,opsAnalyticsQuery"
	}
	return callLoggingJSON(a, http.MethodPatch, "/v2/"+name+"?updateMask="+url.QueryEscape(mask), body, nil)
}

const (
	savedQueryLogging = "Logging query"
	savedQuerySQL     = "SQL (Log Analytics)"
)

func (loggingSavedQueriesProvider) CreateForm() (string, []console.Field) {
	return "Create saved query", []console.Field{
		{Name: "id", Label: "ID", Type: "text", Pattern: `^([A-Za-z0-9][A-Za-z0-9_\-]{0,99})?$`,
			Help: "Optional. Up to 100 letters, digits, _ and -; empty makes one."},
		{Name: "displayName", Label: "Display name", Type: "text", Required: true},
		{Name: "location", Label: "Location", Type: "text", Default: "global", Required: true, Pattern: `^[a-z0-9\-]+$`},
		{Name: "kind", Label: "Query kind", Type: "select", Options: []string{savedQueryLogging, savedQuerySQL}, Default: savedQueryLogging},
		{Name: "query", Label: "Query", Type: "textarea", Required: true,
			Help: "A Logging query, such as severity>=ERROR, or for SQL a Log Analytics query."},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "visibility", Label: "Visibility", Type: "select", Options: []string{"PRIVATE", "SHARED"}, Default: "PRIVATE"},
	}
}

func (p loggingSavedQueriesProvider) Create(_ context.Context, project string, values map[string]string) (string, error) {
	a, err := p.admin()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a saved query")
	}
	body, err := savedQueryBody(values)
	if err != nil {
		return "", err
	}
	path := "/v2/projects/" + url.PathEscape(project) + "/locations/" + url.PathEscape(strings.TrimSpace(values["location"])) + "/savedQueries"
	if id := strings.TrimSpace(values["id"]); id != "" {
		path += "?savedQueryId=" + url.QueryEscape(id)
	}
	var q consoleSavedQuery
	if err := callLoggingJSON(a, http.MethodPost, path, body, &q); err != nil {
		return "", err
	}
	return q.Name, nil
}

func (p loggingSavedQueriesProvider) Delete(_ context.Context, project, name string) error {
	a, err := p.admin()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(name, "projects/"+project+"/locations/") || !strings.Contains(name, "/savedQueries/") {
		return fmt.Errorf("%s is not a saved query of project %s", name, project)
	}
	return callLoggingJSON(a, http.MethodDelete, "/v2/"+name, nil, nil)
}

// ---- Log scopes ----

// Log scopes as Google's Log Scopes tab lays them out
// (docs.cloud.google.com/logging/docs/log-scope/create-and-manage, re-read
// 2026-11-20): Create log scope with Add projects and Add log views, then
// Name log scope (name and description); each row's More has Set as
// default, Edit scope and Delete scope, and the default scope carries a
// Default marker. The _Default scope lists the project and cannot be edited
// or deleted.
//
// Google keeps a project's default log scope in the Observability API
// (projects.locations.scopes/_Default's logScope), which CloudBurrow does
// not serve, so Set as default writes it to the instance's state directory
// (logging-default-log-scopes.json) and the page says so.
type loggingLogScopesProvider struct {
	loggingAdminScreen
	// defaults is the file of each project's default log scope.
	defaults string
}

func (loggingLogScopesProvider) ID() string    { return "logging-log-scopes" }
func (loggingLogScopesProvider) Title() string { return "Log scopes" }

// loggingDefaultScopesFile is the default scopes' record in the instance
// directory.
const loggingDefaultScopesFile = "logging-default-log-scopes.json"

var loggingDefaultScopesMu sync.Mutex

type consoleLogScope struct {
	Name          string   `json:"name"`
	ResourceNames []string `json:"resourceNames"`
	Description   string   `json:"description"`
	UpdateTime    string   `json:"updateTime"`
}

const defaultLogScopeID = "_Default"

func defaultLogScopeName(project string) string {
	return "projects/" + project + "/locations/global/logScopes/" + defaultLogScopeID
}

// defaultScope is the project's default log scope: the one Set as default
// named, or _Default.
func (p loggingLogScopesProvider) defaultScope(project string) string {
	loggingDefaultScopesMu.Lock()
	defer loggingDefaultScopesMu.Unlock()
	if m, err := p.readDefaults(); err == nil && m[project] != "" {
		return m[project]
	}
	return defaultLogScopeName(project)
}

func (p loggingLogScopesProvider) readDefaults() (map[string]string, error) {
	m := map[string]string{}
	if p.defaults == "" {
		return m, nil
	}
	b, err := os.ReadFile(p.defaults)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("the default log scopes record %s: %w", p.defaults, err)
	}
	return m, nil
}

func (p loggingLogScopesProvider) setDefault(project, name string) error {
	if p.defaults == "" {
		return errors.New("a default log scope is kept in the instance's state directory, which this console has none of")
	}
	loggingDefaultScopesMu.Lock()
	defer loggingDefaultScopesMu.Unlock()
	m, err := p.readDefaults()
	if err != nil {
		return err
	}
	if name == defaultLogScopeName(project) {
		delete(m, project)
	} else {
		m[project] = name
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.defaults), 0o700); err != nil {
		return err
	}
	tmp := p.defaults + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.defaults)
}

func (p loggingLogScopesProvider) scopes(project string) ([]consoleLogScope, error) {
	a, err := p.admin()
	if err != nil {
		return nil, err
	}
	var resp struct {
		LogScopes []consoleLogScope `json:"logScopes"`
	}
	if err := callLoggingJSON(a, http.MethodGet, "/v2/projects/"+url.PathEscape(project)+"/locations/global/logScopes", nil, &resp); err != nil {
		return nil, err
	}
	out := resp.LogScopes
	if !slices.ContainsFunc(out, func(s consoleLogScope) bool { return s.Name == defaultLogScopeName(project) }) {
		out = append([]consoleLogScope{{Name: defaultLogScopeName(project), ResourceNames: []string{"projects/" + project},
			Description: "The project's own log scope. It cannot be edited or deleted."}}, out...)
	}
	return out, nil
}

func (p loggingLogScopesProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Default", "Description", "Projects", "Log views", "Updated"}, NameColumn: "Name", Noun: "log scopes",
		Note: "A log scope's Set as default is kept in the instance's state directory: Google keeps it in the Observability API, which CloudBurrow does not serve."}
	if _, err := p.admin(); err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Cloud Logging lists log scopes per project. Choose one in the toolbar."
		return out, nil
	}
	all, err := p.scopes(project)
	if err != nil {
		return console.Listing{}, err
	}
	def := p.defaultScope(project)
	for _, s := range all {
		projects, views := splitScopeResources(s.ResourceNames)
		mark := "—"
		if s.Name == def {
			mark = "Default"
		}
		out.Items = append(out.Items, console.Resource{Name: s.Name, Fields: map[string]string{
			"Default": mark, "Description": orDash(s.Description), "Projects": orDash(strings.Join(projects, ", ")),
			"Log views": orDash(strings.Join(views, ", ")), "Updated": orDash(s.UpdateTime)}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// splitScopeResources parts a scope's resource names into projects and
// log views.
func splitScopeResources(names []string) (projects, views []string) {
	for _, n := range names {
		if strings.Contains(n, "/views/") {
			views = append(views, n)
		} else {
			projects = append(projects, n)
		}
	}
	return projects, views
}

// CreateOnPage implements console.PageCreator: Google's Create log scope is
// a page, its sections (the projects and log views, each added through a
// row of its own, then Name log scope) ending in Create log scope (#1932).
func (loggingLogScopesProvider) CreateOnPage() bool { return true }

// CreateLayout implements console.CreateLayouter: one page under headings.
func (loggingLogScopesProvider) CreateLayout() string { return console.CreateLayoutPage }

// CreateSubmit implements console.CreateSubmitter.
func (loggingLogScopesProvider) CreateSubmit() string { return "Create log scope" }

const (
	scopeSectionProjects = "Projects"
	scopeSectionViews    = "Log views"
	scopeSectionName     = "Name log scope"
)

// logScopeResourceFields are Add projects and Add log views: a row per
// project or log view, prefilled on edit.
func logScopeResourceFields(projects, views []string) []console.Field {
	rows := func(key string, vals []string) string {
		var r []map[string]string
		for _, v := range vals {
			r = append(r, map[string]string{key: v})
		}
		return console.FormatGroup(r)
	}
	return []console.Field{
		{Name: "projects", Label: "Project", Type: console.GroupFieldType, Section: scopeSectionProjects, AddLabel: "Add projects",
			Default: rows("project", projects), Group: []console.Field{
				{Name: "project", Label: "Project", Type: "text", Required: true, Pattern: `(projects/)?[a-z][a-z0-9\-]{4,28}[a-z0-9]`,
					Help: "A project ID, or projects/PROJECT_ID. At most 5 projects."}},
			Help: "The projects whose log entries the scope searches."},
		{Name: "views", Label: "Log view", Type: console.GroupFieldType, Section: scopeSectionViews, AddLabel: "Add log views",
			Default: rows("view", views), Group: []console.Field{
				{Name: "view", Label: "Log view", Type: "text", Required: true, Pattern: `projects/[^\/]+/locations/[^\/]+/buckets/[^\/]+/views/[^\/]+`,
					Help: "projects/PROJECT_ID/locations/LOCATION/buckets/BUCKET_ID/views/VIEW_ID"}},
			Help: "The log views the scope searches. A scope holds at most 100 projects and log views."},
	}
}

func (loggingLogScopesProvider) CreateForm() (string, []console.Field) {
	fields := logScopeResourceFields(nil, nil)
	fields = append(fields,
		console.Field{Name: "id", Label: "Log scope name", Type: "text", Required: true, Pattern: `[A-Za-z0-9][A-Za-z0-9_\-]{0,99}`, Section: scopeSectionName,
			Help: "Up to 100 letters, digits, _ and -. It cannot be changed later and is unique in the project."},
		console.Field{Name: "description", Label: "Description", Type: "text", Section: scopeSectionName})
	return "Create log scope", fields
}

// scopeResources reads the form's projects and log views (and the older
// one-per-line resourceNames) into a scope's resource names.
func scopeResources(values map[string]string) ([]string, error) {
	var names []string
	for _, l := range strings.Split(values["resourceNames"], "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	for _, g := range []struct{ field, key string }{{"projects", "project"}, {"views", "view"}} {
		rows, err := console.ParseGroup(values[g.field])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", g.field, err)
		}
		for _, r := range rows {
			v := strings.TrimSpace(r[g.key])
			if v == "" {
				continue
			}
			if g.key == "project" && !strings.HasPrefix(v, "projects/") {
				v = "projects/" + v
			}
			names = append(names, v)
		}
	}
	if len(names) == 0 {
		return nil, errors.New("add at least one project or log view")
	}
	return names, nil
}

func (p loggingLogScopesProvider) Create(_ context.Context, project string, values map[string]string) (string, error) {
	a, err := p.admin()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a log scope")
	}
	if strings.TrimSpace(values["id"]) == defaultLogScopeID {
		return "", errors.New("_Default is the project's own log scope; choose another name")
	}
	names, err := scopeResources(values)
	if err != nil {
		return "", err
	}
	var s consoleLogScope
	path := "/v2/projects/" + url.PathEscape(project) + "/locations/global/logScopes?logScopeId=" + url.QueryEscape(strings.TrimSpace(values["id"]))
	if err := callLoggingJSON(a, http.MethodPost, path, map[string]any{"resourceNames": names, "description": values["description"]}, &s); err != nil {
		return "", err
	}
	return s.Name, nil
}

func (p loggingLogScopesProvider) ownScope(project, name string) error {
	if !strings.HasPrefix(name, "projects/"+project+"/locations/global/logScopes/") {
		return fmt.Errorf("%s is not a log scope of project %s", name, project)
	}
	return nil
}

// Detail implements console.Driller: the scope's projects and log views,
// and the Edit scope form (none on _Default).
func (p loggingLogScopesProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	name := strings.Join(path, "/")
	if err := p.ownScope(project, name); err != nil {
		return console.Detail{}, err
	}
	all, err := p.scopes(project)
	if err != nil {
		return console.Detail{}, err
	}
	i := slices.IndexFunc(all, func(s consoleLogScope) bool { return s.Name == name })
	if i < 0 {
		return console.Detail{}, fmt.Errorf("log scope %s was not found", name)
	}
	s := all[i]
	projects, views := splitScopeResources(s.ResourceNames)
	def := "No"
	if p.defaultScope(project) == name {
		def = "Yes"
	}
	d := console.Detail{Title: name, Sections: []console.Section{{ID: "overview", Label: "Details", Groups: []console.PropertyGroup{{
		Heading: "Log scope", Properties: []console.Property{
			{Label: "Name", Value: name[strings.LastIndex(name, "/")+1:]}, {Label: "Description", Value: orDash(s.Description)},
			{Label: "Default", Value: def}, {Label: "Projects", Value: orDash(strings.Join(projects, ", "))},
			{Label: "Log views", Value: orDash(strings.Join(views, ", "))}, {Label: "Updated", Value: orDash(s.UpdateTime)}}}}}}}
	if !strings.HasSuffix(name, "/"+defaultLogScopeID) {
		fields := logScopeResourceFields(projects, views)
		fields = append(fields, console.Field{Name: "description", Label: "Description", Type: "text", Default: s.Description, Section: scopeSectionName})
		d.Edit = &console.EditForm{Label: "Edit scope", Fields: fields, Note: "The log scope's name cannot change.", Submit: "Save"}
	}
	return d, nil
}

// Edit implements console.Editor: Edit scope, through logScopes.patch.
func (p loggingLogScopesProvider) Edit(_ context.Context, project string, path []string, values map[string]string) error {
	a, err := p.admin()
	if err != nil {
		return err
	}
	name := strings.Join(path, "/")
	if err := p.ownScope(project, name); err != nil {
		return err
	}
	if strings.HasSuffix(name, "/"+defaultLogScopeID) {
		return errors.New("the _Default log scope cannot be edited")
	}
	names, err := scopeResources(values)
	if err != nil {
		return err
	}
	return callLoggingJSON(a, http.MethodPatch, "/v2/"+name+"?updateMask=resourceNames,description",
		map[string]any{"resourceNames": names, "description": values["description"]}, nil)
}

func (loggingLogScopesProvider) RowEditLabel() string { return "Edit scope" }
func (loggingLogScopesProvider) DeleteLabel() string  { return "Delete scope" }

const scopeSetDefault = "set-default"

// RowMenuOrder implements console.RowMenuOrderer: More's Set as default,
// Edit scope, Delete scope.
func (loggingLogScopesProvider) RowMenuOrder() []string {
	return []string{scopeSetDefault, console.RowMenuEdit, console.RowMenuDelete}
}

// Actions implements console.Actor: Set as default, unavailable on the
// default scope itself.
func (loggingLogScopesProvider) Actions(r console.Resource) []console.Action {
	a := console.Action{ID: scopeSetDefault, Label: "Set as default"}
	if r.Fields["Default"] == "Default" {
		a.Unavailable = "This is the project's default log scope."
	}
	return []console.Action{a}
}

// Act performs Set as default.
func (p loggingLogScopesProvider) Act(_ context.Context, project, name, action string) error {
	if action != scopeSetDefault {
		return fmt.Errorf("unknown action %q", action)
	}
	if err := p.ownScope(project, name); err != nil {
		return err
	}
	all, err := p.scopes(project)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(all, func(s consoleLogScope) bool { return s.Name == name }) {
		return fmt.Errorf("log scope %s was not found", name)
	}
	return p.setDefault(project, name)
}

func (p loggingLogScopesProvider) Delete(_ context.Context, project, name string) error {
	a, err := p.admin()
	if err != nil {
		return err
	}
	if err := p.ownScope(project, name); err != nil {
		return err
	}
	if strings.HasSuffix(name, "/"+defaultLogScopeID) {
		return errors.New("the _Default log scope cannot be deleted")
	}
	if err := callLoggingJSON(a, http.MethodDelete, "/v2/"+name, nil, nil); err != nil {
		return err
	}
	// A deleted default leaves _Default the default again.
	if p.defaultScope(project) == name {
		return p.setDefault(project, defaultLogScopeName(project))
	}
	return nil
}

var (
	_ console.Driller         = loggingLogScopesProvider{}
	_ console.RowEditor       = loggingLogScopesProvider{}
	_ console.DeleteLabeler   = loggingLogScopesProvider{}
	_ console.RowMenuOrderer  = loggingLogScopesProvider{}
	_ console.Actor           = loggingLogScopesProvider{}
	_ console.CreateSubmitter = loggingLogScopesProvider{}
)
