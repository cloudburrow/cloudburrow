package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	pmpb "cloud.google.com/go/parametermanager/apiv1/parametermanagerpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/parametermanager"
)

// parameterManagerProvider is the console's Parameter Manager page
// (#2054), under Secret Manager in the menu as Google has it: a project's
// parameters in every location (format, labels, encryption); Create
// parameter (name, location, format, encryption, labels); a parameter's
// page with its Details and Versions, New version, Edit (labels and key)
// and Delete (once it has no versions); a version's page with its Payload
// and Rendered payload (its secret references read as the parameter's
// principal), Disable or Enable, and Delete. Paths are [parameter] and
// [parameter, version ID].
type parameterManagerProvider struct {
	svc *parameterManagerService
}

const parameterManagerNotStarted = "Parameter Manager has not started"

func (parameterManagerProvider) ID() string    { return "parametermanager" }
func (parameterManagerProvider) Title() string { return "Parameter Manager" }

// parameterFormats are Google's Format choices, in its order.
var parameterFormats = []string{"UNFORMATTED", "YAML", "JSON"}

var parameterFormatLabels = map[string]string{"UNFORMATTED": "Unformatted", "YAML": "YAML", "JSON": "JSON"}

func (p parameterManagerProvider) api() (*parametermanager.Server, error) {
	s := p.svc.API()
	if s == nil {
		return nil, errors.New(parameterManagerNotStarted)
	}
	return s, nil
}

func (p parameterManagerProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := p.api()
	if err != nil {
		return console.Listing{}, err
	}
	listing := console.Listing{Columns: []string{"Location", "Format", "Encryption", "Labels", "Created"}, NameColumn: "Name", Noun: "parameters"}
	if project == "" {
		listing.Prompt = "Parameter Manager lists parameters per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, loc := range parametermanager.Locations() {
		resp, err := s.ListParameters(ctx, &pmpb.ListParametersRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, prm := range resp.GetParameters() {
			listing.Items = append(listing.Items, console.Resource{Name: prm.GetName(), Fields: map[string]string{
				"Location": kmsLocation(prm.GetName()), "Format": parameterFormatLabels[prm.GetFormat().String()],
				"Encryption": parameterEncryption(prm.GetKmsKey()), "Labels": kmsOrDash(labelsText(prm.GetLabels())),
				"Created": kmsTime(prm.GetCreateTime())}, Actions: pmRowActions(parametersRoute, prm.GetName()),
				Info: pubsubInfoTabs(prm.GetLabels(), "parameter", false)})
		}
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	listing.InfoOnSelect = true // check a parameter, then Labels (#2143)
	return listing, nil
}

func parameterEncryption(key string) string {
	if key == "" {
		return "Google-managed"
	}
	return "Customer-managed: " + key
}

func (p parameterManagerProvider) CreateForm() (string, []console.Field) {
	const details = "Parameter details"
	// Google's Create parameter (create-parameter, read 2026-11-22): the
	// name, then the Parameter format and Location sections; Encryption
	// and Labels' Add label rows.
	fields := []console.Field{
		{Name: "parameterId", Label: "Name", Type: "text", Required: true, Section: details, Pattern: `^[A-Za-z0-9_\-]{1,63}$`,
			Help: "Letters, numbers, hyphens and underscores; at most 63 characters."},
		{Name: "format", Label: "Parameter format", Type: "radio", Default: "UNFORMATTED", Section: "Parameter format", Options: parameterFormats,
			OptionLabels: parameterFormatLabels, Help: "A JSON or YAML parameter's versions must parse as that format, and can reference Secret Manager secrets."},
	}
	fields = append(fields, pmLocationFields()...)
	return "Create parameter", append(fields,
		console.Field{Name: "encryption", Label: "Encryption", Type: "radio", Default: "google", Section: "Encryption", Options: []string{"google", "cmek"},
			OptionLabels: map[string]string{"google": "Google-managed encryption key", "cmek": "Cloud KMS key"}},
		console.Field{Name: "kmsKey", Label: "Cloud KMS key", Type: "text", Section: "Encryption", ShowWhen: "encryption=cmek", Browse: "kms",
			Pattern: `^projects/[^\/]+/locations/[^\/]+/keyRings/[^\/]+/cryptoKeys/[^\/]+$`,
			Help:    "A key of this instance's Cloud KMS in the parameter's location: projects/PROJECT/locations/LOCATION/keyRings/RING/cryptoKeys/KEY."},
		pmLabelsField(nil))
}

func (p parameterManagerProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := p.api()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a parameter")
	}
	labels, err := pmLabels(values["labels"])
	if err != nil {
		return "", err
	}
	prm := &pmpb.Parameter{Labels: labels, Format: pmpb.ParameterFormat(pmpb.ParameterFormat_value[values["format"]])}
	if values["encryption"] == "cmek" {
		key := strings.TrimSpace(values["kmsKey"])
		if key == "" {
			return "", fmt.Errorf("choose a Cloud KMS key, or Google-managed encryption")
		}
		prm.KmsKey = &key
	}
	out, err := s.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: "projects/" + project + "/locations/" + pmLocation(values),
		ParameterId: strings.TrimSpace(values["parameterId"]), Parameter: prm})
	if err != nil {
		return "", err
	}
	return out.GetName(), nil
}

func (p parameterManagerProvider) parameter(ctx context.Context, project, name string) (*pmpb.Parameter, error) {
	s, err := p.api()
	if err != nil {
		return nil, err
	}
	if err := kmsOwned(project, name); err != nil {
		return nil, err
	}
	return s.GetParameter(ctx, &pmpb.GetParameterRequest{Name: name})
}

func (p parameterManagerProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	switch len(path) {
	case 1:
		return p.parameterDetail(ctx, project, path[0])
	case 2:
		return p.versionDetail(ctx, project, path[0], path[1])
	}
	return console.DeeperThan(2, path), nil
}

func (p parameterManagerProvider) parameterDetail(ctx context.Context, project, name string) (console.Detail, error) {
	prm, err := p.parameter(ctx, project, name)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the parameter: " + consoleErr(err)}, nil
	}
	s, _ := p.api()
	versions := console.Listing{Columns: []string{"State", "Created", "Encryption key version"}, NameColumn: "Version", Noun: "versions"}
	if resp, err := s.ListParameterVersions(ctx, &pmpb.ListParameterVersionsRequest{Parent: name, PageSize: 1000}); err == nil {
		for _, v := range resp.GetParameterVersions() {
			versions.Items = append(versions.Items, console.Resource{Name: lastSegment(v.GetName()), Status: versionState(v),
				Opens: []string{name, lastSegment(v.GetName())}, Fields: map[string]string{"State": versionState(v), "Created": kmsTime(v.GetCreateTime()),
					"Encryption key version": kmsOrDash(v.GetKmsKeyVersion())},
				Actions: pmVersionRowActions(parametersRoute, name, lastSegment(v.GetName()), v.GetDisabled(), name)})
		}
	}
	versions.Total = len(versions.Items)
	return console.Detail{
		Summary: []console.Property{{Label: "Location", Value: kmsLocation(name)}, {Label: "Format", Value: parameterFormatLabels[prm.GetFormat().String()]}},
		Sections: []console.Section{
			{ID: "versions", Label: "Versions", Listing: versions},
			{ID: "details", Label: "Overview", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Parameter", Properties: []console.Property{
				{Label: "Resource name", Value: name}, {Label: "Format", Value: parameterFormatLabels[prm.GetFormat().String()]},
				{Label: "Encryption", Value: parameterEncryption(prm.GetKmsKey())},
				{Label: "Principal", Value: prm.GetPolicyMember().GetIamPolicyUidPrincipal()},
				{Label: "Created", Value: kmsTime(prm.GetCreateTime())}, {Label: "Last updated", Value: kmsTime(prm.GetUpdateTime())},
				{Label: "Labels", Value: kmsOrDash(labelsText(prm.GetLabels()))}}}}},
		},
	}, nil
}

func versionState(v *pmpb.ParameterVersion) string {
	if v.GetDisabled() {
		return "Disabled"
	}
	return "Enabled"
}

// versionName is the version a page's path names: the parameter's name
// and the version's ID, so the page's heading is the ID.
func versionName(param, id string) string { return param + "/versions/" + id }

func (p parameterManagerProvider) versionDetail(ctx context.Context, project, param, id string) (console.Detail, error) {
	if _, err := p.parameter(ctx, project, param); err != nil {
		return console.Detail{Unavailable: "cannot read the parameter: " + consoleErr(err)}, nil
	}
	if strings.Contains(id, "/") {
		return console.Detail{Unavailable: "the version is not the parameter's"}, nil
	}
	name := versionName(param, id)
	s, _ := p.api()
	v, err := s.GetParameterVersion(ctx, &pmpb.GetParameterVersionRequest{Name: name, View: pmpb.View_FULL})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the version: " + consoleErr(err)}, nil
	}
	payload := console.Section{ID: "payload", Label: "Payload", Kind: console.KindText, Text: string(v.GetPayload().GetData())}
	rendered := console.Section{ID: "rendered", Label: "Rendered payload", Kind: console.KindText}
	if v.GetDisabled() {
		payload.Unavailable = "The version is disabled: its payload is not returned."
		rendered.Unavailable = "A disabled version cannot be rendered."
	} else if r, err := s.RenderParameterVersion(ctx, &pmpb.RenderParameterVersionRequest{Name: name}); err != nil {
		rendered.Unavailable = "cannot render the version: " + consoleErr(err)
	} else {
		rendered.Text = string(r.GetRenderedPayload())
	}
	return console.Detail{
		Summary: []console.Property{{Label: "State", Value: versionState(v)}, {Label: "Created", Value: kmsTime(v.GetCreateTime())}},
		Sections: []console.Section{payload, rendered,
			{ID: "details", Label: "Overview", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Version", Properties: []console.Property{
				{Label: "Resource name", Value: name}, {Label: "State", Value: versionState(v)},
				{Label: "Encryption key version", Value: kmsOrDash(v.GetKmsKeyVersion())},
				{Label: "Created", Value: kmsTime(v.GetCreateTime())}, {Label: "Last updated", Value: kmsTime(v.GetUpdateTime())}}}}}},
	}, nil
}

func (p parameterManagerProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	switch len(path) {
	case 1:
		prm, err := p.parameter(ctx, project, path[0])
		if err != nil {
			return nil
		}
		enc, key := "google", prm.GetKmsKey()
		if key != "" {
			enc = "cmek"
		}
		return []console.Action{
			// Google's Create parameter version page (add-parameter-version).
			{ID: "newversion", Label: "New version", Title: "Create parameter version", Submit: "Create", Page: true, Fields: []console.Field{
				{Name: "versionId", Label: "Name", Type: "text", Required: true, Pattern: pmVersionIDPattern, Help: pmVersionIDHelp},
				{Name: "payload", Label: "Payload", Type: "textarea", Required: true, Upload: "Browse",
					Help: "At most 1 MiB, in the parameter's format. In JSON or YAML, __REF__(\"//secretmanager.googleapis.com/projects/PROJECT/secrets/SECRET/versions/VERSION\") is replaced by the secret version when rendered."},
				{Name: "disabled", Label: "Disable this version", Type: "checkbox"}}},
			// Google's Edit parameter page, ending in Save (edit-parameter).
			{ID: "edit", Label: "Edit", Title: "Edit parameter", Submit: "Save", Page: true, Fields: []console.Field{
				{Name: "encryption", Label: "Encryption", Type: "radio", Default: enc, Options: []string{"google", "cmek"},
					OptionLabels: map[string]string{"google": "Google-managed encryption key", "cmek": "Cloud KMS key"},
					Help:         "A new key encrypts new versions only; existing versions keep theirs."},
				{Name: "kmsKey", Label: "Cloud KMS key", Type: "text", Default: key, ShowWhen: "encryption=cmek", Browse: "kms",
					Pattern: `^projects/[^\/]+/locations/[^\/]+/keyRings/[^\/]+/cryptoKeys/[^\/]+$`},
				pmLabelsField(prm.GetLabels())}},
			// Google's Delete parameter asks for the parameter's name.
			{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, ConfirmWord: lastSegment(path[0]),
				Confirm: "A parameter can be deleted only once it has no versions, enabled or disabled."},
			// The list's Labels panel (#2143).
			pubsubLabelsAction(prm.GetLabels()),
		}
	case 2:
		s, err := p.api()
		if err != nil {
			return nil
		}
		v, err := s.GetParameterVersion(ctx, &pmpb.GetParameterVersionRequest{Name: versionName(path[0], path[1]), View: pmpb.View_BASIC})
		if err != nil {
			return nil
		}
		return []console.Action{pmToggleAction(v.GetDisabled()), pmDeleteVersionAction(path[0])}
	}
	return nil
}

func (p parameterManagerProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) == 0 {
		return fmt.Errorf("no parameter")
	}
	if _, err := p.parameter(ctx, project, path[0]); err != nil {
		return err
	}
	s, _ := p.api()
	switch action {
	case "newversion":
		_, err := s.CreateParameterVersion(ctx, &pmpb.CreateParameterVersionRequest{Parent: path[0], ParameterVersionId: strings.TrimSpace(values["versionId"]),
			ParameterVersion: &pmpb.ParameterVersion{Disabled: values["disabled"] == "true",
				Payload: &pmpb.ParameterVersionPayload{Data: []byte(values["payload"])}}})
		return err
	case "edit":
		labels, err := pmLabels(values["labels"])
		if err != nil {
			return err
		}
		key := ""
		if values["encryption"] == "cmek" {
			if key = strings.TrimSpace(values["kmsKey"]); key == "" {
				return fmt.Errorf("choose a Cloud KMS key, or Google-managed encryption")
			}
		}
		_, err = s.UpdateParameter(ctx, &pmpb.UpdateParameterRequest{Parameter: &pmpb.Parameter{Name: path[0], Labels: labels, KmsKey: &key},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels", "kms_key"}}})
		return err
	case "delete":
		_, err := s.DeleteParameter(ctx, &pmpb.DeleteParameterRequest{Name: path[0]})
		return err
	case actEditLabels:
		// The list's Labels panel: the labels alone (#2143).
		labels, err := console.ParseMap(values["labels"])
		if err != nil {
			return err
		}
		_, err = s.UpdateParameter(ctx, &pmpb.UpdateParameterRequest{Parameter: &pmpb.Parameter{Name: path[0], Labels: labels},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
		return err
	}
	if len(path) != 2 || strings.Contains(path[1], "/") {
		return fmt.Errorf("%s needs a version of the parameter", action)
	}
	version := versionName(path[0], path[1])
	switch action {
	case "disable", "enable":
		_, err := s.UpdateParameterVersion(ctx, &pmpb.UpdateParameterVersionRequest{
			ParameterVersion: &pmpb.ParameterVersion{Name: version, Disabled: action == "disable"},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}})
		return err
	case "deleteversion":
		_, err := s.DeleteParameterVersion(ctx, &pmpb.DeleteParameterVersionRequest{Name: version})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

var (
	_ console.Creator   = parameterManagerProvider{}
	_ console.Driller   = parameterManagerProvider{}
	_ console.PathActor = parameterManagerProvider{}
)
