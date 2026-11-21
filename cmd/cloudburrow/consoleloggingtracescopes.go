package main

// Trace scopes (#2108), as Google's Trace Scopes tab lays them out
// (docs.cloud.google.com/trace/docs/trace-scope/create-and-manage, read
// 2026-11-21): a table of Name, Description, Type and Resources, the
// default scope marked Default; Create trace scope with Add projects and a
// name and description; each row's More has Edit scope, Delete scope and
// Set as default. _Default lists the project and can be neither edited
// nor deleted. Google draws the tab in Settings beside Log Scopes; here it
// is a tab of Logging's Settings, over the Observability API's
// projects.locations.traceScopes the Logging port serves, in process.
//
// With --services trace (#2118) the Trace explorer searches the projects
// of the scope it is given, the default one first.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
)

type loggingTraceScopesProvider struct {
	loggingAdminScreen
}

func (loggingTraceScopesProvider) ID() string    { return "logging-trace-scopes" }
func (loggingTraceScopesProvider) Title() string { return "Trace Scopes" }

const traceScopeNote = "The default trace scope is the one the Trace explorer opens on: it searches the spans of every project the scope names."

func traceScopesPath(project string) string {
	return "/v1/projects/" + url.PathEscape(project) + "/locations/global/traceScopes"
}

func defaultTraceScopeName(project string) string {
	return "projects/" + project + "/locations/global/traceScopes/" + logging.DefaultScopeID
}

func (p loggingTraceScopesProvider) scopes(project string) ([]logging.TraceScope, error) {
	a, err := p.admin()
	if err != nil {
		return nil, err
	}
	var resp struct {
		TraceScopes []logging.TraceScope `json:"traceScopes"`
	}
	if err := callObservabilityJSON(a, http.MethodGet, traceScopesPath(project)+"?pageSize=1000", nil, &resp); err != nil {
		return nil, err
	}
	return resp.TraceScopes, nil
}

// defaultScope is the project's default trace scope.
func (p loggingTraceScopesProvider) defaultScope(project string) string {
	a, err := p.admin()
	if err != nil {
		return defaultTraceScopeName(project)
	}
	var sc logging.ObservabilityScope
	if err := callObservabilityJSON(a, http.MethodGet, observabilityScopePath(project), nil, &sc); err != nil || sc.TraceScope == "" {
		return defaultTraceScopeName(project)
	}
	return sc.TraceScope
}

func (p loggingTraceScopesProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Description", "Type", "Resources"}, NameColumn: "Name", Noun: "trace scopes", Note: traceScopeNote}
	if _, err := p.admin(); err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Trace scopes are kept per project. Choose one in the toolbar."
		return out, nil
	}
	all, err := p.scopes(project)
	if err != nil {
		return console.Listing{}, err
	}
	def := p.defaultScope(project)
	for _, s := range all {
		typ := "Custom"
		if strings.HasSuffix(s.Name, "/"+logging.DefaultScopeID) {
			typ = "System"
		}
		if s.Name == def {
			typ += " (Default)"
		}
		row := console.Resource{Name: s.Name, Fields: map[string]string{
			"Description": orDash(s.Description), "Type": typ, "Resources": orDash(strings.Join(s.ResourceNames, ", "))}}
		if strings.HasSuffix(s.Name, "/"+logging.DefaultScopeID) {
			row.Withheld = map[string]string{"edit": "You can't modify the trace scope named _Default.", "delete": "You can't delete the trace scope named _Default."}
		}
		out.Items = append(out.Items, row)
	}
	out.Total = len(out.Items)
	return out, nil
}

func (loggingTraceScopesProvider) CreateOnPage() bool   { return true }
func (loggingTraceScopesProvider) CreateLayout() string { return console.CreateLayoutPage }
func (loggingTraceScopesProvider) CreateSubmit() string { return "Create trace scope" }
func (loggingTraceScopesProvider) RowEditLabel() string { return "Edit scope" }
func (loggingTraceScopesProvider) DeleteLabel() string  { return "Delete scope" }
func (loggingTraceScopesProvider) RowMenuOrder() []string {
	return []string{console.RowMenuEdit, console.RowMenuDelete, scopeSetDefault}
}

const (
	traceSectionProjects = "Projects"
	traceSectionName     = "Name trace scope"
)

func traceScopeProjectsField(projects []string) console.Field {
	var rows []map[string]string
	for _, v := range projects {
		rows = append(rows, map[string]string{"project": strings.TrimPrefix(v, "projects/")})
	}
	return console.Field{Name: "projects", Label: "Project", Type: console.GroupFieldType, Section: traceSectionProjects, AddLabel: "Add projects",
		Default: console.FormatGroup(rows), Group: []console.Field{
			{Name: "project", Label: "Project", Type: "text", Required: true, Pattern: `(projects/)?[a-z][a-z0-9\-]{4,28}[a-z0-9]`,
				Help: "A project ID, or projects/PROJECT_ID. At most 20 projects."}},
		Help: "Each project adds its default trace view, _Trace/Spans/_AllSpans."}
}

func (loggingTraceScopesProvider) CreateForm() (string, []console.Field) {
	return "Create trace scope", []console.Field{
		traceScopeProjectsField(nil),
		{Name: "id", Label: "Trace scope name", Type: "text", Required: true, Pattern: `[A-Za-z0-9][A-Za-z0-9_\-]{0,99}`, Section: traceSectionName,
			Help: "Up to 100 letters, digits, _ and -. It can't be modified later and is unique in the project."},
		{Name: "description", Label: "Description", Type: "text", Section: traceSectionName, Help: "Up to 8000 characters."},
	}
}

func traceScopeProjects(values map[string]string) ([]string, error) {
	rows, err := console.ParseGroup(values["projects"])
	if err != nil {
		return nil, fmt.Errorf("projects: %w", err)
	}
	var out []string
	for _, r := range rows {
		v := strings.TrimSpace(r["project"])
		if v == "" {
			continue
		}
		if !strings.HasPrefix(v, "projects/") {
			v = "projects/" + v
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, errors.New("add at least one project")
	}
	return out, nil
}

func (p loggingTraceScopesProvider) Create(_ context.Context, project string, values map[string]string) (string, error) {
	a, err := p.admin()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a trace scope")
	}
	names, err := traceScopeProjects(values)
	if err != nil {
		return "", err
	}
	var s logging.TraceScope
	path := traceScopesPath(project) + "?traceScopeId=" + url.QueryEscape(strings.TrimSpace(values["id"]))
	if err := callObservabilityJSON(a, http.MethodPost, path, map[string]any{"resourceNames": names, "description": values["description"]}, &s); err != nil {
		return "", err
	}
	return s.Name, nil
}

func (loggingTraceScopesProvider) own(project, name string) error {
	if !strings.HasPrefix(name, "projects/"+project+"/locations/global/traceScopes/") {
		return fmt.Errorf("%s is not a trace scope of project %s", name, project)
	}
	return nil
}

func (p loggingTraceScopesProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	name := strings.Join(path, "/")
	if err := p.own(project, name); err != nil {
		return console.Detail{}, err
	}
	a, err := p.admin()
	if err != nil {
		return console.Detail{}, err
	}
	var s logging.TraceScope
	if err := callObservabilityJSON(a, http.MethodGet, "/v1/"+name, nil, &s); err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	def := "No"
	if p.defaultScope(project) == name {
		def = "Yes"
	}
	views := make([]string, 0, len(s.ResourceNames))
	for _, r := range s.ResourceNames {
		views = append(views, strings.TrimPrefix(r, "projects/")+": _Trace/Spans/_AllSpans")
	}
	d := console.Detail{Title: name, Sections: []console.Section{{ID: "overview", Label: "Details", Note: traceScopeNote, Groups: []console.PropertyGroup{{
		Heading: "Trace scope", Properties: []console.Property{
			{Label: "Name", Value: name[strings.LastIndex(name, "/")+1:]}, {Label: "Description", Value: orDash(s.Description)},
			{Label: "Default", Value: def}, {Label: "Resources", Value: orDash(strings.Join(s.ResourceNames, ", "))},
			{Label: "Views", Value: orDash(strings.Join(views, ", "))},
			{Label: "Created", Value: orDash(s.CreateTime)}, {Label: "Updated", Value: orDash(s.UpdateTime)}}}}}}}
	if !strings.HasSuffix(name, "/"+logging.DefaultScopeID) {
		d.Edit = &console.EditForm{Label: "Edit scope", Submit: "Save", Note: "The trace scope's name can't be modified.", Fields: []console.Field{
			traceScopeProjectsField(s.ResourceNames),
			{Name: "description", Label: "Description", Type: "text", Default: s.Description, Section: traceSectionName},
		}}
	}
	return d, nil
}

func (p loggingTraceScopesProvider) Edit(_ context.Context, project string, path []string, values map[string]string) error {
	a, err := p.admin()
	if err != nil {
		return err
	}
	name := strings.Join(path, "/")
	if err := p.own(project, name); err != nil {
		return err
	}
	names, err := traceScopeProjects(values)
	if err != nil {
		return err
	}
	return callObservabilityJSON(a, http.MethodPatch, "/v1/"+name+"?updateMask=resourceNames,description",
		map[string]any{"resourceNames": names, "description": values["description"]}, nil)
}

func (loggingTraceScopesProvider) Actions(r console.Resource) []console.Action {
	a := console.Action{ID: scopeSetDefault, Label: "Set as default"}
	if strings.HasSuffix(r.Fields["Type"], "(Default)") {
		a.Unavailable = "This is the project's default trace scope."
	}
	return []console.Action{a}
}

// Act performs Set as default: scopes.patch of traceScope.
func (p loggingTraceScopesProvider) Act(_ context.Context, project, name, action string) error {
	if action != scopeSetDefault {
		return fmt.Errorf("unknown action %q", action)
	}
	if err := p.own(project, name); err != nil {
		return err
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	return callObservabilityJSON(a, http.MethodPatch, observabilityScopePath(project)+"?updateMask=traceScope",
		map[string]string{"traceScope": name}, nil)
}

func (p loggingTraceScopesProvider) Delete(_ context.Context, project, name string) error {
	a, err := p.admin()
	if err != nil {
		return err
	}
	if err := p.own(project, name); err != nil {
		return err
	}
	return callObservabilityJSON(a, http.MethodDelete, "/v1/"+name, nil, nil)
}

var (
	_ console.Driller         = loggingTraceScopesProvider{}
	_ console.RowEditor       = loggingTraceScopesProvider{}
	_ console.DeleteLabeler   = loggingTraceScopesProvider{}
	_ console.RowMenuOrderer  = loggingTraceScopesProvider{}
	_ console.Actor           = loggingTraceScopesProvider{}
	_ console.CreateSubmitter = loggingTraceScopesProvider{}
)
