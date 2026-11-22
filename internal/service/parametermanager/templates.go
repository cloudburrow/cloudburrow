package parametermanager

// Parameter templates (#2063, in preview on Google): templates and their
// versions, in the v1 surface (google.cloud.parametermanager.v1, the
// Template and TemplateVersion methods), global and regional, and
// RenderTemplateVersion, which substitutes a template version's variables
// with a parameter version's values.
//
// As Google documents them (docs.cloud.google.com/secret-manager/
// parameter-manager/docs: "Parameter templates overview", "Add a parameter
// template version", "Render a parameter template version", read
// 2026-11-20):
//
//   - a template's format is YAML (the default) or JSON, fixed at creation:
//     "Templates do not support the UNFORMATTED format" (the proto);
//   - a template version's payload "must be a valid JSON or YAML object"
//     in its template's format, at most 1 MiB;
//   - "You can only use direct variable substitution": {{.key}}, or
//     {{.key1.key2}} for a nested value; "Control flow statements, such as
//     if...else, loops, or the range function, are not supported";
//   - "A single template version can contain a maximum of 64 variables";
//   - "Don't embed Secret Manager secrets directly in a template version":
//     a __REF__ belongs in the parameter version, which rendering resolves
//     first ("hydrated into the template during rendering");
//   - a template version name "can be up to 63 characters long" and "can't
//     begin with a dash";
//   - rendering takes one parameter version, of a JSON or YAML parameter;
//     "The value of a variable must not contain an array" nor "a nested
//     key-value pair"; the output must be a valid JSON or YAML object, at
//     most 1 MiB, or the render fails;
//   - a disabled template version is metadata only: its payload is never
//     returned (the proto's TemplateVersion.disabled).
//
// Not documented, and decided here (UNVERIFIED (docs re-read 2026-11-20:
// https://docs.cloud.google.com/secret-manager/parameter-manager/docs/render-parameter-template-version)): a variable the parameter
// version lacks fails the render INVALID_ARGUMENT naming it; the parameter
// version must be in the template's project and location; a template with
// versions cannot be deleted, as a parameter with versions cannot.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	pmpb "cloud.google.com/go/parametermanager/apiv1/parametermanagerpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// MaxTemplateVariables is a template version's variable limit.
const MaxTemplateVariables = 64

type templateRecord struct {
	T        *pmpb.Template
	Versions map[string]*templateVersionRecord
}

type templateVersionRecord struct {
	V    *pmpb.TemplateVersion
	Data []byte
}

var (
	templateRE        = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/templates/([^/]+)$`)
	templateVersionRE = regexp.MustCompile(`^(projects/([^/]+)/locations/([^/]+)/templates/([^/]+))/versions/([^/]+)$`)
	// templateVersionIDRE: at most 63 characters, not beginning with a
	// dash.
	templateVersionIDRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,62}$`)
	// actionRE is any {{ ... }} action; variableRE the one kind allowed,
	// {{.key}} or {{.key1.key2}}, spaces inside the braces allowed.
	actionRE   = regexp.MustCompile(`\{\{-?\s*(.*?)\s*-?\}\}`)
	variableRE = regexp.MustCompile(`^\.([A-Za-z_][A-Za-z0-9_-]*(?:\.[A-Za-z_][A-Za-z0-9_-]*)*)$`)
)

func templateFormat(f pmpb.TemplateFormat) (pmpb.TemplateFormat, error) {
	switch f {
	case pmpb.TemplateFormat_TEMPLATE_FORMAT_UNSPECIFIED:
		return pmpb.TemplateFormat_TEMPLATE_FORMAT_YAML, nil
	case pmpb.TemplateFormat_TEMPLATE_FORMAT_YAML, pmpb.TemplateFormat_TEMPLATE_FORMAT_JSON:
		return f, nil
	}
	return 0, apierror.InvalidArgument("template.format %d is not a TemplateFormat: a template is TEMPLATE_FORMAT_YAML or TEMPLATE_FORMAT_JSON", f)
}

// CreateTemplate creates a template.
func (s *Server) CreateTemplate(ctx context.Context, req *pmpb.CreateTemplateRequest) (*pmpb.Template, error) {
	project, _, err := checkParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.charge(ctx, quotaWrite, project); err != nil {
		return nil, err
	}
	id := req.GetTemplateId()
	if !idRE.MatchString(id) {
		return nil, apierror.InvalidArgument("template_id %q must be 1 to 63 letters, digits, hyphens or underscores", id)
	}
	in := req.GetTemplate()
	if in == nil {
		in = &pmpb.Template{}
	}
	if err := validateLabels(in.GetLabels()); err != nil {
		return nil, err
	}
	format, err := templateFormat(in.GetFormat())
	if err != nil {
		return nil, err
	}
	name := req.GetParent() + "/templates/" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.templates[name]; ok {
		return nil, apierror.AlreadyExists("Template %s already exists", name)
	}
	now := timestamppb.New(s.now())
	t := &pmpb.Template{Name: name, CreateTime: now, UpdateTime: now, Labels: in.GetLabels(), Format: format}
	s.templates[name] = &templateRecord{T: t, Versions: map[string]*templateVersionRecord{}}
	return clone(t), apierror.Wrap(s.save())
}

// template is the named template. The caller holds mu.
func (s *Server) template(name string) (*templateRecord, error) {
	if !templateRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/templates/{template}", name)
	}
	rec := s.templates[name]
	if rec == nil {
		return nil, apierror.NotFound("Template %s not found", name)
	}
	return rec, nil
}

// templateVersion is the named template version. The caller holds mu.
func (s *Server) templateVersion(name string) (*templateRecord, *templateVersionRecord, error) {
	m := templateVersionRE.FindStringSubmatch(name)
	if m == nil {
		return nil, nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/templates/{template}/versions/{version}", name)
	}
	rec := s.templates[m[1]]
	if rec == nil {
		return nil, nil, apierror.NotFound("Template %s not found", m[1])
	}
	v := rec.Versions[m[5]]
	if v == nil {
		return nil, nil, apierror.NotFound("TemplateVersion %s not found", name)
	}
	return rec, v, nil
}

// GetTemplate reads a template.
func (s *Server) GetTemplate(ctx context.Context, req *pmpb.GetTemplateRequest) (*pmpb.Template, error) {
	if err := s.chargeName(ctx, quotaRead, req.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.template(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(rec.T), nil
}

// ListTemplates lists a location's templates.
func (s *Server) ListTemplates(ctx context.Context, req *pmpb.ListTemplatesRequest) (*pmpb.ListTemplatesResponse, error) {
	project, _, err := checkParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.charge(ctx, quotaRead, project); err != nil {
		return nil, err
	}
	prefix := req.GetParent() + "/templates/"
	s.mu.Lock()
	var items []*pmpb.Template
	for _, name := range sortedKeys(s.templates) {
		if strings.HasPrefix(name, prefix) {
			items = append(items, clone(s.templates[name].T))
		}
	}
	s.mu.Unlock()
	items, err = aipfilter.List(req.GetFilter(), req.GetOrderBy(), items)
	if err != nil {
		return nil, err
	}
	page, next, err := pageOf("templates:"+req.GetParent()+req.GetFilter()+req.GetOrderBy(), items,
		func(t *pmpb.Template) string { return t.GetName() }, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &pmpb.ListTemplatesResponse{Templates: page, NextPageToken: next}, nil
}

// UpdateTemplate changes a template's labels. Its format is immutable.
func (s *Server) UpdateTemplate(ctx context.Context, req *pmpb.UpdateTemplateRequest) (*pmpb.Template, error) {
	in := req.GetTemplate()
	if in == nil {
		return nil, apierror.InvalidArgument("template is required")
	}
	if err := s.chargeName(ctx, quotaWrite, in.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.template(in.GetName())
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"labels"}
		if in.GetFormat() != pmpb.TemplateFormat_TEMPLATE_FORMAT_UNSPECIFIED {
			paths = append(paths, "format")
		}
	}
	next := clone(rec.T)
	for _, p := range paths {
		switch snake(p) {
		case "labels":
			if err := validateLabels(in.GetLabels()); err != nil {
				return nil, err
			}
			next.Labels = in.GetLabels()
		case "format":
			if in.GetFormat() != rec.T.GetFormat() {
				return nil, apierror.InvalidArgument("template.format is immutable: the template's format is %s", rec.T.GetFormat())
			}
		case "name", "create_time", "update_time":
			return nil, apierror.InvalidArgument("update_mask path %q is an output-only field", p)
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of Template that can be updated", p)
		}
	}
	next.UpdateTime = timestamppb.New(s.now())
	rec.T = next
	return clone(next), apierror.Wrap(s.save())
}

// DeleteTemplate deletes a template that has no versions.
func (s *Server) DeleteTemplate(ctx context.Context, req *pmpb.DeleteTemplateRequest) (*emptypb.Empty, error) {
	if err := s.chargeName(ctx, quotaWrite, req.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.template(req.GetName()); err != nil {
		return nil, err
	}
	// "Deleting a template also deletes all of its template versions"
	// (https://docs.cloud.google.com/secret-manager/parameter-manager/docs/delete-parameter-templates,
	// read 2026-11-22): a template's versions are kept on it, and go with
	// it. A parameter, unlike a template, refuses while it has versions.
	delete(s.templates, req.GetName())
	return &emptypb.Empty{}, apierror.Wrap(s.save())
}

// templateVariables checks a template version's payload: a JSON or YAML
// object in format, whose every action is a direct variable, at most 64 of
// them, and no secret reference. It answers the variables' paths.
func templateVariables(format pmpb.TemplateFormat, data []byte) ([]string, error) {
	if len(data) > MaxPayloadBytes {
		return nil, apierror.InvalidArgument("template_version.payload.data is %d bytes: a template version's payload can be at most 1 MiB (%d bytes)", len(data), MaxPayloadBytes)
	}
	if refRE.Match(data) {
		return nil, apierror.InvalidArgument("template_version.payload.data has a secret reference (__REF__): don't embed Secret Manager secrets in a template version; reference them in the parameter version that renders it")
	}
	var vars []string
	for _, m := range actionRE.FindAllSubmatch(data, -1) {
		v := variableRE.FindSubmatch(m[1])
		if v == nil {
			return nil, apierror.InvalidArgument("template_version.payload.data: %q is not a variable: templates allow only direct variable substitution, {{.key}} or {{.key1.key2}}, and no control flow (if, else, range, loops) or functions", m[0])
		}
		vars = append(vars, string(v[1]))
	}
	if len(vars) > MaxTemplateVariables {
		return nil, apierror.InvalidArgument("template_version.payload.data has %d variables: a template version can contain at most %d", len(vars), MaxTemplateVariables)
	}
	// The object it is once rendered, each variable standing for a value.
	sample, err := substitute(format, data, func(string) (any, error) { return "x", nil })
	if err != nil {
		return nil, err
	}
	if err := checkObject(format, sample, "template_version.payload.data"); err != nil {
		return nil, err
	}
	return vars, nil
}

// checkObject checks data is a JSON or YAML object (a mapping).
func checkObject(format pmpb.TemplateFormat, data []byte, field string) error {
	if format == pmpb.TemplateFormat_TEMPLATE_FORMAT_JSON {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return apierror.InvalidArgument("%s is not valid JSON, and the template's format is JSON: %v", field, err)
		}
		if _, ok := v.(map[string]any); !ok {
			return apierror.InvalidArgument("%s must be a JSON object", field)
		}
		return nil
	}
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return apierror.InvalidArgument("%s is not valid YAML, and the template's format is YAML: %v", field, err)
	}
	if _, ok := v.(map[string]any); !ok {
		return apierror.InvalidArgument("%s must be a YAML object (a mapping)", field)
	}
	return nil
}

// CreateTemplateVersion adds a version to a template.
func (s *Server) CreateTemplateVersion(ctx context.Context, req *pmpb.CreateTemplateVersionRequest) (*pmpb.TemplateVersion, error) {
	if err := s.chargeName(ctx, quotaWrite, req.GetParent()); err != nil {
		return nil, err
	}
	id := req.GetTemplateVersionId()
	if !templateVersionIDRE.MatchString(id) {
		return nil, apierror.InvalidArgument("template_version_id %q must be 1 to 63 letters, digits, hyphens or underscores, and can't begin with a dash", id)
	}
	in := req.GetTemplateVersion()
	if in.GetPayload() == nil {
		return nil, apierror.InvalidArgument("template_version.payload is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.template(req.GetParent())
	if err != nil {
		return nil, err
	}
	if _, ok := rec.Versions[id]; ok {
		return nil, apierror.AlreadyExists("TemplateVersion %s/versions/%s already exists", req.GetParent(), id)
	}
	data := in.GetPayload().GetData()
	if _, err := templateVariables(rec.T.GetFormat(), data); err != nil {
		return nil, err
	}
	now := timestamppb.New(s.now())
	v := &pmpb.TemplateVersion{Name: req.GetParent() + "/versions/" + id, CreateTime: now, UpdateTime: now, Disabled: in.GetDisabled()}
	vr := &templateVersionRecord{V: v, Data: bytes.Clone(data)}
	rec.Versions[id] = vr
	return templateView(vr, pmpb.View_FULL), apierror.Wrap(s.save())
}

// templateView is a version as answered: with its payload in the FULL
// view of an enabled version.
func templateView(vr *templateVersionRecord, view pmpb.View) *pmpb.TemplateVersion {
	out := clone(vr.V)
	if view != pmpb.View_BASIC && !vr.V.GetDisabled() {
		out.Payload = &pmpb.TemplateVersionPayload{Data: bytes.Clone(vr.Data)}
	}
	return out
}

// GetTemplateVersion reads a template version; the FULL view (the
// default) carries an enabled version's payload.
func (s *Server) GetTemplateVersion(ctx context.Context, req *pmpb.GetTemplateVersionRequest) (*pmpb.TemplateVersion, error) {
	if err := s.chargeName(ctx, quotaRead, req.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, vr, err := s.templateVersion(req.GetName())
	if err != nil {
		return nil, err
	}
	view := req.GetView()
	if view == pmpb.View_VIEW_UNSPECIFIED {
		view = pmpb.View_FULL
	}
	return templateView(vr, view), nil
}

// ListTemplateVersions lists a template's versions, without payloads.
func (s *Server) ListTemplateVersions(ctx context.Context, req *pmpb.ListTemplateVersionsRequest) (*pmpb.ListTemplateVersionsResponse, error) {
	if err := s.chargeName(ctx, quotaRead, req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	rec, err := s.template(req.GetParent())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var items []*pmpb.TemplateVersion
	for _, id := range sortedKeys(rec.Versions) {
		items = append(items, templateView(rec.Versions[id], pmpb.View_BASIC))
	}
	s.mu.Unlock()
	items, err = aipfilter.List(req.GetFilter(), req.GetOrderBy(), items)
	if err != nil {
		return nil, err
	}
	page, next, err := pageOf("templateversions:"+req.GetParent()+req.GetFilter()+req.GetOrderBy(), items,
		func(v *pmpb.TemplateVersion) string { return v.GetName() }, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &pmpb.ListTemplateVersionsResponse{TemplateVersions: page, NextPageToken: next}, nil
}

// UpdateTemplateVersion enables or disables a template version.
func (s *Server) UpdateTemplateVersion(ctx context.Context, req *pmpb.UpdateTemplateVersionRequest) (*pmpb.TemplateVersion, error) {
	in := req.GetTemplateVersion()
	if in == nil {
		return nil, apierror.InvalidArgument("template_version is required")
	}
	if err := s.chargeName(ctx, quotaWrite, in.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, vr, err := s.templateVersion(in.GetName())
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"disabled"}
	}
	next := clone(vr.V)
	for _, p := range paths {
		switch snake(p) {
		case "disabled":
			next.Disabled = in.GetDisabled()
		case "payload", "payload.data":
			return nil, apierror.InvalidArgument("update_mask path %q: a template version's payload is immutable; create a new version", p)
		case "name", "create_time", "update_time":
			return nil, apierror.InvalidArgument("update_mask path %q is an output-only field", p)
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of TemplateVersion that can be updated", p)
		}
	}
	next.UpdateTime = timestamppb.New(s.now())
	vr.V = next
	return templateView(vr, pmpb.View_BASIC), apierror.Wrap(s.save())
}

// DeleteTemplateVersion deletes a template version.
func (s *Server) DeleteTemplateVersion(ctx context.Context, req *pmpb.DeleteTemplateVersionRequest) (*emptypb.Empty, error) {
	if err := s.chargeName(ctx, quotaWrite, req.GetName()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, _, err := s.templateVersion(req.GetName())
	if err != nil {
		return nil, err
	}
	delete(rec.Versions, templateVersionRE.FindStringSubmatch(req.GetName())[5])
	return &emptypb.Empty{}, apierror.Wrap(s.save())
}

// RenderTemplateVersion substitutes a template version's variables with
// the values of a parameter version, rendered first so its secret
// references are resolved.
func (s *Server) RenderTemplateVersion(ctx context.Context, req *pmpb.RenderTemplateVersionRequest) (*pmpb.RenderTemplateVersionResponse, error) {
	if err := s.chargeName(ctx, quotaRender, req.GetName()); err != nil {
		return nil, err
	}
	tm := templateVersionRE.FindStringSubmatch(req.GetName())
	if tm == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/templates/{template}/versions/{version}", req.GetName())
	}
	pv := req.GetParameterVersion()
	pm := versionRE.FindStringSubmatch(pv)
	if pm == nil {
		return nil, apierror.InvalidArgument("parameter_version %q must be projects/{project}/locations/{location}/parameters/{parameter}/versions/{version}", pv)
	}
	if pm[2] != tm[2] || pm[3] != tm[3] {
		return nil, apierror.InvalidArgument("parameter_version %s must be in the template's project and location, projects/%s/locations/%s", pv, tm[2], tm[3])
	}
	if s.opts.CheckCaller != nil {
		if err := s.opts.CheckCaller(ctx, "parametermanager.parameterVersions.render", pv); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	_, vr, err := s.templateVersion(req.GetName())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if vr.V.GetDisabled() {
		s.mu.Unlock()
		return nil, apierror.FailedPrecondition("TemplateVersion %s is disabled: a disabled version cannot be rendered", req.GetName())
	}
	format := s.templates[tm[1]].T.GetFormat()
	data := bytes.Clone(vr.Data)
	s.mu.Unlock()

	values, err := s.renderedValues(ctx, pv)
	if err != nil {
		return nil, err
	}
	out, err := substitute(format, data, func(path string) (any, error) { return lookup(values, path) })
	if err != nil {
		return nil, err
	}
	if len(out) > MaxPayloadBytes {
		return nil, apierror.InvalidArgument("the rendered template version is %d bytes: the final configuration payload can't exceed 1 MiB (%d bytes)", len(out), MaxPayloadBytes)
	}
	if err := checkObject(format, out, "the rendered template version"); err != nil {
		return nil, err
	}
	return &pmpb.RenderTemplateVersionResponse{TemplateVersion: req.GetName(), Payload: &pmpb.TemplateVersionPayload{Data: data},
		RenderedPayload: out, TemplateFormat: format, ParameterVersion: pv}, nil
}

// renderedValues is a parameter version's rendered payload, parsed: the
// variables a template version reads.
func (s *Server) renderedValues(ctx context.Context, name string) (map[string]any, error) {
	s.mu.Lock()
	rec, _, err := s.version(name)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	format := rec.P.GetFormat()
	s.mu.Unlock()
	if format != pmpb.ParameterFormat_JSON && format != pmpb.ParameterFormat_YAML {
		return nil, apierror.InvalidArgument("parameter_version %s is of an %s parameter: a template is rendered with the values of a JSON or YAML parameter", name, format)
	}
	resp, err := s.renderVersion(ctx, name)
	if err != nil {
		return nil, err
	}
	var v any
	if format == pmpb.ParameterFormat_JSON {
		err = json.Unmarshal(resp.GetRenderedPayload(), &v)
	} else {
		err = yaml.Unmarshal(resp.GetRenderedPayload(), &v)
	}
	m, ok := v.(map[string]any)
	if err != nil || !ok {
		return nil, apierror.InvalidArgument("parameter_version %s: its variables must be key-value pairs, a %s object", name, format)
	}
	return m, nil
}

// substitute replaces each {{.path}} in data with value(path).
// In a JSON template a variable inside a string is replaced by the value's
// text, escaped; outside one, by the value as JSON. In YAML it is replaced
// by the value's text.
func substitute(format pmpb.TemplateFormat, data []byte, value func(path string) (any, error)) ([]byte, error) {
	var out bytes.Buffer
	last := 0
	for _, loc := range actionRE.FindAllSubmatchIndex(data, -1) {
		out.Write(data[last:loc[0]])
		last = loc[1]
		path := variableRE.FindSubmatch(data[loc[2]:loc[3]])
		if path == nil {
			return nil, apierror.InvalidArgument("%q is not a variable", data[loc[0]:loc[1]])
		}
		v, err := value(string(path[1]))
		if err != nil {
			return nil, err
		}
		text := scalarText(v)
		if format == pmpb.TemplateFormat_TEMPLATE_FORMAT_JSON {
			if inJSONString(data[:loc[0]]) {
				b, _ := json.Marshal(text)
				text = string(b[1 : len(b)-1])
			} else {
				b, _ := json.Marshal(v)
				text = string(b)
			}
		}
		out.WriteString(text)
	}
	out.Write(data[last:])
	return out.Bytes(), nil
}

// lookup is the value at a dotted path, which must be a scalar.
func lookup(values map[string]any, path string) (any, error) {
	var cur any = values
	for _, k := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, apierror.InvalidArgument("variable {{.%s}}: the parameter version has no key %q there", path, k)
		}
		if cur, ok = m[k]; !ok {
			return nil, apierror.InvalidArgument("variable {{.%s}} is not in the parameter version: it has no key %q", path, k)
		}
	}
	switch cur.(type) {
	case []any:
		return nil, apierror.InvalidArgument("variable {{.%s}}: the value of a variable must not contain an array", path)
	case map[string]any:
		return nil, apierror.InvalidArgument("variable {{.%s}}: the value of a variable must not contain a nested key-value pair", path)
	}
	return cur, nil
}

func scalarText(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

// inJSONString is whether the end of prefix is inside a JSON string.
func inJSONString(prefix []byte) bool {
	in, esc := false, false
	for _, c := range prefix {
		switch {
		case esc:
			esc = false
		case c == '\\' && in:
			esc = true
		case c == '"':
			in = !in
		}
	}
	return in
}
