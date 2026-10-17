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
	"strings"

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
		Filter string `json:"filter"`
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
		query := "—"
		switch {
		case q.LoggingQuery != nil:
			query = orDash(q.LoggingQuery.Filter)
		case q.OpsAnalyticsQuery != nil:
			query = "SQL: " + q.OpsAnalyticsQuery.SQLQueryText
		}
		out.Items = append(out.Items, console.Resource{Name: q.Name, Fields: map[string]string{
			"Display name": q.DisplayName, "Visibility": q.Visibility, "Query": query, "Updated": q.UpdateTime}})
	}
	out.Total = len(out.Items)
	return out, nil
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
	body := map[string]any{"displayName": values["displayName"], "description": values["description"], "visibility": values["visibility"]}
	if values["kind"] == savedQuerySQL {
		body["opsAnalyticsQuery"] = map[string]string{"sqlQueryText": values["query"]}
	} else {
		body["loggingQuery"] = map[string]string{"filter": values["query"]}
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

type loggingLogScopesProvider struct{ loggingAdminScreen }

func (loggingLogScopesProvider) ID() string    { return "logging-log-scopes" }
func (loggingLogScopesProvider) Title() string { return "Log scopes" }

type consoleLogScope struct {
	Name          string   `json:"name"`
	ResourceNames []string `json:"resourceNames"`
	Description   string   `json:"description"`
	UpdateTime    string   `json:"updateTime"`
}

func (p loggingLogScopesProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Resources", "Description", "Updated"}, NameColumn: "Log scope", Noun: "log scopes"}
	a, err := p.admin()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Cloud Logging lists log scopes per project. Choose one in the toolbar."
		return out, nil
	}
	var resp struct {
		LogScopes []consoleLogScope `json:"logScopes"`
	}
	if err := callLoggingJSON(a, http.MethodGet, "/v2/projects/"+url.PathEscape(project)+"/locations/global/logScopes", nil, &resp); err != nil {
		return console.Listing{}, err
	}
	for _, s := range resp.LogScopes {
		out.Items = append(out.Items, console.Resource{Name: s.Name, Fields: map[string]string{
			"Resources": strings.Join(s.ResourceNames, ", "), "Description": orDash(s.Description), "Updated": s.UpdateTime}})
	}
	out.Total = len(out.Items)
	return out, nil
}

func (loggingLogScopesProvider) CreateForm() (string, []console.Field) {
	return "Create log scope", []console.Field{
		{Name: "id", Label: "ID", Type: "text", Required: true, Pattern: `^[A-Za-z0-9][A-Za-z0-9_\-]{0,99}$`,
			Help: "Up to 100 letters, digits, _ and -."},
		{Name: "resourceNames", Label: "Resources", Type: "textarea", Required: true,
			Help: "One per line: projects/PROJECT, or a log view's name, projects/P/locations/L/buckets/B/views/V. At most 100."},
		{Name: "description", Label: "Description", Type: "text"},
	}
}

func (p loggingLogScopesProvider) Create(_ context.Context, project string, values map[string]string) (string, error) {
	a, err := p.admin()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a log scope")
	}
	var names []string
	for _, l := range strings.Split(values["resourceNames"], "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	var s consoleLogScope
	path := "/v2/projects/" + url.PathEscape(project) + "/locations/global/logScopes?logScopeId=" + url.QueryEscape(strings.TrimSpace(values["id"]))
	if err := callLoggingJSON(a, http.MethodPost, path, map[string]any{"resourceNames": names, "description": values["description"]}, &s); err != nil {
		return "", err
	}
	return s.Name, nil
}

func (p loggingLogScopesProvider) Delete(_ context.Context, project, name string) error {
	a, err := p.admin()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(name, "projects/"+project+"/locations/global/logScopes/") {
		return fmt.Errorf("%s is not a log scope of project %s", name, project)
	}
	return callLoggingJSON(a, http.MethodDelete, "/v2/"+name, nil, nil)
}
