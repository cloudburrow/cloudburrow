package main

import (
	"context"
	"fmt"
	"strings"

	pmpb "cloud.google.com/go/parametermanager/apiv1/parametermanagerpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/parametermanager"
)

// parameterTemplatesProvider is the console's Parameter Manager Templates
// tab (#2063, in preview on Google): a project's templates in every
// location (format, labels); Create template (name, location, format,
// labels); a template's page with its Versions and Overview, New version
// (payload, disabled), Edit (labels) and Delete (once it has no versions);
// a version's page with its Payload, Render (a parameter version chosen,
// answering the payload and the rendered value, as Google's "Render
// template version" page does), Disable or Enable, and Delete. Paths are
// [template] and [template, version ID].
type parameterTemplatesProvider struct {
	svc *parameterManagerService
}

func (parameterTemplatesProvider) ID() string    { return "parametermanager-templates" }
func (parameterTemplatesProvider) Title() string { return "Parameter templates" }

var templateFormats = []string{"TEMPLATE_FORMAT_YAML", "TEMPLATE_FORMAT_JSON"}

var templateFormatLabels = map[string]string{"TEMPLATE_FORMAT_YAML": "YAML", "TEMPLATE_FORMAT_JSON": "JSON"}

func (p parameterTemplatesProvider) api() (*parametermanager.Server, error) {
	return parameterManagerProvider(p).api()
}

func (p parameterTemplatesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := p.api()
	if err != nil {
		return console.Listing{}, err
	}
	listing := console.Listing{Columns: []string{"Location", "Format", "Labels", "Created"}, NameColumn: "Name", Noun: "templates"}
	if project == "" {
		listing.Prompt = "Parameter Manager lists templates per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, loc := range parametermanager.Locations() {
		resp, err := s.ListTemplates(ctx, &pmpb.ListTemplatesRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, t := range resp.GetTemplates() {
			listing.Items = append(listing.Items, console.Resource{Name: t.GetName(), Fields: map[string]string{
				"Location": kmsLocation(t.GetName()), "Format": templateFormatLabels[t.GetFormat().String()],
				"Labels": kmsOrDash(labelsText(t.GetLabels())), "Created": kmsTime(t.GetCreateTime())}, Actions: pmRowActions(templatesRoute, t.GetName()),
				Info: pubsubInfoTabs(t.GetLabels(), "template", false)})
		}
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	listing.InfoOnSelect = true // check a template, then Labels (#2143)
	return listing, nil
}

func (p parameterTemplatesProvider) CreateForm() (string, []console.Field) {
	const details = "Template details"
	// Google's Create parameter template (create-parameter-template, read
	// 2026-11-22): the name, Template format, Location and Labels.
	fields := []console.Field{
		{Name: "templateId", Label: "Name", Type: "text", Required: true, Section: details, Pattern: `^[A-Za-z0-9_\-]{1,63}$`,
			Help: "Letters, numbers, hyphens and underscores; at most 63 characters."},
		{Name: "format", Label: "Template format", Type: "radio", Default: "TEMPLATE_FORMAT_YAML", Section: "Template format", Options: templateFormats,
			OptionLabels: templateFormatLabels, Help: "A template's versions are YAML or JSON objects; unformatted text is not supported."},
	}
	return "Create template", append(append(fields, pmLocationFields()...), pmLabelsField(nil))
}

func (p parameterTemplatesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := p.api()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a template")
	}
	labels, err := pmLabels(values["labels"])
	if err != nil {
		return "", err
	}
	out, err := s.CreateTemplate(ctx, &pmpb.CreateTemplateRequest{Parent: "projects/" + project + "/locations/" + pmLocation(values),
		TemplateId: strings.TrimSpace(values["templateId"]),
		Template:   &pmpb.Template{Labels: labels, Format: pmpb.TemplateFormat(pmpb.TemplateFormat_value[values["format"]])}})
	if err != nil {
		return "", err
	}
	return out.GetName(), nil
}

func (p parameterTemplatesProvider) template(ctx context.Context, project, name string) (*pmpb.Template, error) {
	s, err := p.api()
	if err != nil {
		return nil, err
	}
	if err := kmsOwned(project, name); err != nil {
		return nil, err
	}
	return s.GetTemplate(ctx, &pmpb.GetTemplateRequest{Name: name})
}

func (p parameterTemplatesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	switch len(path) {
	case 1:
		return p.templateDetail(ctx, project, path[0])
	case 2:
		return p.versionDetail(ctx, project, path[0], path[1])
	}
	return console.DeeperThan(2, path), nil
}

func templateVersionState(v *pmpb.TemplateVersion) string {
	if v.GetDisabled() {
		return "Disabled"
	}
	return "Enabled"
}

func (p parameterTemplatesProvider) templateDetail(ctx context.Context, project, name string) (console.Detail, error) {
	t, err := p.template(ctx, project, name)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the template: " + consoleErr(err)}, nil
	}
	s, _ := p.api()
	versions := console.Listing{Columns: []string{"State", "Created"}, NameColumn: "Version", Noun: "versions"}
	if resp, err := s.ListTemplateVersions(ctx, &pmpb.ListTemplateVersionsRequest{Parent: name, PageSize: 1000}); err == nil {
		for _, v := range resp.GetTemplateVersions() {
			versions.Items = append(versions.Items, console.Resource{Name: lastSegment(v.GetName()), Status: templateVersionState(v),
				Opens: []string{name, lastSegment(v.GetName())}, Fields: map[string]string{"State": templateVersionState(v), "Created": kmsTime(v.GetCreateTime())},
				Actions: pmVersionRowActions(templatesRoute, name, lastSegment(v.GetName()), v.GetDisabled(), name)})
		}
	}
	versions.Total = len(versions.Items)
	format := templateFormatLabels[t.GetFormat().String()]
	return console.Detail{
		Summary: []console.Property{{Label: "Location", Value: kmsLocation(name)}, {Label: "Format", Value: format}},
		Sections: []console.Section{
			{ID: "versions", Label: "Versions", Listing: versions},
			{ID: "details", Label: "Overview", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Template", Properties: []console.Property{
				{Label: "Resource name", Value: name}, {Label: "Format", Value: format},
				{Label: "Created", Value: kmsTime(t.GetCreateTime())}, {Label: "Last updated", Value: kmsTime(t.GetUpdateTime())},
				{Label: "Labels", Value: kmsOrDash(labelsText(t.GetLabels()))}}}}},
		},
	}, nil
}

func (p parameterTemplatesProvider) versionDetail(ctx context.Context, project, tpl, id string) (console.Detail, error) {
	if _, err := p.template(ctx, project, tpl); err != nil {
		return console.Detail{Unavailable: "cannot read the template: " + consoleErr(err)}, nil
	}
	if strings.Contains(id, "/") {
		return console.Detail{Unavailable: "the version is not the template's"}, nil
	}
	name := tpl + "/versions/" + id
	s, _ := p.api()
	v, err := s.GetTemplateVersion(ctx, &pmpb.GetTemplateVersionRequest{Name: name, View: pmpb.View_FULL})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the version: " + consoleErr(err)}, nil
	}
	payload := console.Section{ID: "payload", Label: "Payload", Kind: console.KindText, Text: string(v.GetPayload().GetData())}
	if v.GetDisabled() {
		payload.Unavailable = "The version is disabled: its payload is not returned."
	}
	return console.Detail{
		Summary: []console.Property{{Label: "State", Value: templateVersionState(v)}, {Label: "Created", Value: kmsTime(v.GetCreateTime())}},
		Sections: []console.Section{payload,
			{ID: "details", Label: "Overview", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Version", Properties: []console.Property{
				{Label: "Resource name", Value: name}, {Label: "State", Value: templateVersionState(v)},
				{Label: "Created", Value: kmsTime(v.GetCreateTime())}, {Label: "Last updated", Value: kmsTime(v.GetUpdateTime())}}}}}},
	}, nil
}

// parameterVersionsIn are the enabled versions of the JSON and YAML
// parameters in a template's location, which a render can take.
func (p parameterTemplatesProvider) parameterVersionsIn(ctx context.Context, tpl string) []string {
	s, err := p.api()
	if err != nil {
		return nil
	}
	parent := tpl[:strings.Index(tpl, "/templates/")]
	resp, err := s.ListParameters(ctx, &pmpb.ListParametersRequest{Parent: parent, PageSize: 1000})
	if err != nil {
		return nil
	}
	var out []string
	for _, prm := range resp.GetParameters() {
		if f := prm.GetFormat(); f != pmpb.ParameterFormat_JSON && f != pmpb.ParameterFormat_YAML {
			continue
		}
		vs, err := s.ListParameterVersions(ctx, &pmpb.ListParameterVersionsRequest{Parent: prm.GetName(), PageSize: 1000})
		if err != nil {
			continue
		}
		for _, v := range vs.GetParameterVersions() {
			if !v.GetDisabled() {
				out = append(out, v.GetName())
			}
		}
	}
	return out
}

func (p parameterTemplatesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	switch len(path) {
	case 1:
		t, err := p.template(ctx, project, path[0])
		if err != nil {
			return nil
		}
		return []console.Action{
			// Google's Create version, opening the Create template version
			// page (add-parameter-template-version).
			{ID: "newversion", Label: "Create version", Title: "Create template version", Submit: "Create", Page: true, Fields: []console.Field{
				{Name: "versionId", Label: "Name", Type: "text", Required: true, Pattern: pmVersionIDPattern, Help: pmVersionIDHelp},
				{Name: "payload", Label: "Payload", Type: "textarea", Required: true, Upload: "Browse",
					Help: "A " + templateFormatLabels[t.GetFormat().String()] + " object of at most 1 MiB. Variables are {{.key}}, or {{.key1.key2}} for a nested value, at most 64; no control flow, and no Secret Manager references (put them in the parameter version)."},
				{Name: "disabled", Label: "Disable this version", Type: "checkbox"}}},
			// Google's Edit parameter template page, ending in Save.
			{ID: "edit", Label: "Edit", Title: "Edit parameter template", Submit: "Save", Page: true,
				Fields: []console.Field{pmLabelsField(t.GetLabels())}},
			// "Deleting a template also deletes all of its template
			// versions" (delete-parameter-templates), the name typed.
			{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, ConfirmWord: lastSegment(path[0]),
				Confirm: "Deleting a template also deletes all of its template versions."},
			pubsubLabelsAction(t.GetLabels()),
		}
	case 2:
		s, err := p.api()
		if err != nil {
			return nil
		}
		v, err := s.GetTemplateVersion(ctx, &pmpb.GetTemplateVersionRequest{Name: path[0] + "/versions/" + path[1], View: pmpb.View_BASIC})
		if err != nil {
			return nil
		}
		toggle := pmToggleAction(v.GetDisabled())
		render := console.Action{ID: "render", Label: "Render", Title: "Render template version", Fields: []console.Field{
			{Name: "parameterVersion", Label: "Parameter version", Type: "select", Required: true, Options: p.parameterVersionsIn(ctx, path[0]),
				Help: "An enabled version of a JSON or YAML parameter in the template's location; its values replace the template's variables."}}}
		if v.GetDisabled() {
			render.Unavailable = "A disabled version cannot be rendered."
		}
		return []console.Action{render, toggle, pmDeleteVersionAction(path[0])}
	}
	return nil
}

// ActAtResult answers Render with the payload and the rendered value.
func (p parameterTemplatesProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if action != "render" {
		return nil, p.ActAt(ctx, project, path, action, values)
	}
	if len(path) != 2 || strings.Contains(path[1], "/") {
		return nil, fmt.Errorf("render needs a version of the template")
	}
	if _, err := p.template(ctx, project, path[0]); err != nil {
		return nil, err
	}
	s, _ := p.api()
	r, err := s.RenderTemplateVersion(ctx, &pmpb.RenderTemplateVersionRequest{Name: path[0] + "/versions/" + path[1],
		ParameterVersion: strings.TrimSpace(values["parameterVersion"])})
	if err != nil {
		return nil, err
	}
	return &console.Listing{Columns: []string{"Payload", "Rendered value"}, NameColumn: "Parameter version", Noun: "renders", Total: 1,
		Items: []console.Resource{{Name: r.GetParameterVersion(), Fields: map[string]string{
			"Payload": string(r.GetPayload().GetData()), "Rendered value": string(r.GetRenderedPayload())}}}}, nil
}

func (p parameterTemplatesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) == 0 {
		return fmt.Errorf("no template")
	}
	if _, err := p.template(ctx, project, path[0]); err != nil {
		return err
	}
	s, _ := p.api()
	switch action {
	case "newversion":
		_, err := s.CreateTemplateVersion(ctx, &pmpb.CreateTemplateVersionRequest{Parent: path[0], TemplateVersionId: strings.TrimSpace(values["versionId"]),
			TemplateVersion: &pmpb.TemplateVersion{Disabled: values["disabled"] == "true",
				Payload: &pmpb.TemplateVersionPayload{Data: []byte(values["payload"])}}})
		return err
	case "edit", actEditLabels:
		raw := values["labels"]
		if action == actEditLabels {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				raw = "{}"
			}
		}
		labels, err := pmLabels(raw)
		if err != nil {
			return err
		}
		_, err = s.UpdateTemplate(ctx, &pmpb.UpdateTemplateRequest{Template: &pmpb.Template{Name: path[0], Labels: labels},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
		return err
	case "delete":
		_, err := s.DeleteTemplate(ctx, &pmpb.DeleteTemplateRequest{Name: path[0]})
		return err
	}
	if len(path) != 2 || strings.Contains(path[1], "/") {
		return fmt.Errorf("%s needs a version of the template", action)
	}
	version := path[0] + "/versions/" + path[1]
	switch action {
	case "disable", "enable":
		_, err := s.UpdateTemplateVersion(ctx, &pmpb.UpdateTemplateVersionRequest{
			TemplateVersion: &pmpb.TemplateVersion{Name: version, Disabled: action == "disable"},
			UpdateMask:      &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}})
		return err
	case "deleteversion":
		_, err := s.DeleteTemplateVersion(ctx, &pmpb.DeleteTemplateVersionRequest{Name: version})
		return err
	case "render":
		_, err := p.ActAtResult(ctx, project, path, action, values)
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

var (
	_ console.Creator     = parameterTemplatesProvider{}
	_ console.Driller     = parameterTemplatesProvider{}
	_ console.ResultActor = parameterTemplatesProvider{}
)
