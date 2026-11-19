package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// privateCATemplatesProvider is Certificate Authority Service's
// Certificate templates page (#1834): a project's templates in every
// region, Create template (its predefined key usages, identity constraints
// and maximum lifetime), a template's page, Edit, Delete and a Permissions
// tab. A pool's Request certificate forms name a template to use. Paths
// are [template resource name].
type privateCATemplatesProvider struct{ svc *privateCAService }

func (privateCATemplatesProvider) ID() string    { return "privateca-templates" }
func (privateCATemplatesProvider) Title() string { return "Certificate templates" }

func (p privateCATemplatesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s := p.svc.API()
	if s == nil {
		return console.Listing{}, errors.New(privateCANotStarted)
	}
	listing := console.Listing{Columns: []string{"Location", "Description", "Maximum lifetime", "Labels"}, NameColumn: "Template", Noun: "certificate templates"}
	if project == "" {
		listing.Prompt = "Certificate Authority Service lists certificate templates per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, t := range s.TemplatesInProject(project) {
		life := "—"
		if d := t.GetMaximumLifetime(); d != nil {
			life = d.AsDuration().String()
		}
		listing.Items = append(listing.Items, console.Resource{Name: t.GetName(), Fields: map[string]string{
			"Location": kmsLocation(t.GetName()), "Description": kmsOrDash(t.GetDescription()), "Maximum lifetime": life,
			"Labels": kmsOrDash(labelsText(t.GetLabels()))}})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

var privateCAUsages = []string{"digital_signature", "key_encipherment", "server_auth", "client_auth", "code_signing", "email_protection"}

func (privateCATemplatesProvider) CreateForm() (string, []console.Field) {
	return "Create template", []console.Field{
		{Name: "certificateTemplateId", Label: "Template ID", Type: "text", Required: true, Pattern: privateCAIDPattern},
		{Name: "location", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: privateCALocations()},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "usages", Label: "Key usages", Type: "text", Default: "digital_signature,server_auth",
			Help: "Comma-separated predefined values: " + strings.Join(privateCAUsages, ", ") + ". They override the request's."},
		{Name: "allowSubjectPassthrough", Label: "Allow the request's subject", Type: "checkbox", Default: "true"},
		{Name: "allowSansPassthrough", Label: "Allow the request's subject alternative names", Type: "checkbox", Default: "true"},
		{Name: "maximumLifetimeDays", Label: "Maximum lifetime (days)", Type: "text", Pattern: `^[0-9]{0,5}$`},
		labelsField(nil, ""),
	}
}

// privateCATemplateValues are predefined values from a usages list.
func privateCATemplateValues(usages string) (*privatecapb.X509Parameters, error) {
	base, ext := &privatecapb.KeyUsage_KeyUsageOptions{}, &privatecapb.KeyUsage_ExtendedKeyUsageOptions{}
	for _, u := range strings.Split(usages, ",") {
		switch strings.TrimSpace(u) {
		case "":
		case "digital_signature":
			base.DigitalSignature = true
		case "key_encipherment":
			base.KeyEncipherment = true
		case "server_auth":
			ext.ServerAuth = true
		case "client_auth":
			ext.ClientAuth = true
		case "code_signing":
			ext.CodeSigning = true
		case "email_protection":
			ext.EmailProtection = true
		default:
			return nil, fmt.Errorf("key usages: %q is not one of %s", u, strings.Join(privateCAUsages, ", "))
		}
	}
	return &privatecapb.X509Parameters{KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: base, ExtendedKeyUsage: ext}}, nil
}

func (p privateCATemplatesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s := p.svc.API()
	if s == nil {
		return "", errors.New(privateCANotStarted)
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a template")
	}
	pv, err := privateCATemplateValues(values["usages"])
	if err != nil {
		return "", err
	}
	life, err := privateCADays("maximum lifetime", values["maximumLifetimeDays"])
	if err != nil {
		return "", err
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", err
	}
	subj, sans := values["allowSubjectPassthrough"] != "false", values["allowSansPassthrough"] != "false"
	op, err := s.API().CreateCertificateTemplate(ctx, &privatecapb.CreateCertificateTemplateRequest{
		Parent: "projects/" + project + "/locations/" + strings.TrimSpace(values["location"]), CertificateTemplateId: strings.TrimSpace(values["certificateTemplateId"]),
		CertificateTemplate: &privatecapb.CertificateTemplate{Description: strings.TrimSpace(values["description"]), PredefinedValues: pv, MaximumLifetime: life, Labels: labels,
			IdentityConstraints: &privatecapb.CertificateIdentityConstraints{AllowSubjectPassthrough: &subj, AllowSubjectAltNamesPassthrough: &sans}}})
	if err != nil {
		return "", err
	}
	out := &privatecapb.CertificateTemplate{}
	if err := op.GetResponse().UnmarshalTo(out); err != nil {
		return "", err
	}
	return out.GetName(), nil
}

func (p privateCATemplatesProvider) template(ctx context.Context, project string, path []string) (*privatecapb.CertificateTemplate, error) {
	s := p.svc.API()
	if s == nil {
		return nil, errors.New(privateCANotStarted)
	}
	if len(path) != 1 {
		return nil, fmt.Errorf("no template at %s", strings.Join(path, "/"))
	}
	if err := kmsOwned(project, path[0]); err != nil {
		return nil, err
	}
	return s.API().GetCertificateTemplate(ctx, &privatecapb.GetCertificateTemplateRequest{Name: path[0]})
}

// privateCAUsagesText is a template's predefined key usages as Create
// template takes them.
func privateCAUsagesText(pv *privatecapb.X509Parameters) string {
	b, e := pv.GetKeyUsage().GetBaseKeyUsage(), pv.GetKeyUsage().GetExtendedKeyUsage()
	var out []string
	for _, u := range []struct {
		on   bool
		name string
	}{{b.GetDigitalSignature(), "digital_signature"}, {b.GetKeyEncipherment(), "key_encipherment"}, {e.GetServerAuth(), "server_auth"},
		{e.GetClientAuth(), "client_auth"}, {e.GetCodeSigning(), "code_signing"}, {e.GetEmailProtection(), "email_protection"}} {
		if u.on {
			out = append(out, u.name)
		}
	}
	return strings.Join(out, ",")
}

func (p privateCATemplatesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	t, err := p.template(ctx, project, path)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the template: " + consoleErr(err)}, nil
	}
	life := "No maximum"
	if d := t.GetMaximumLifetime(); d != nil {
		life = d.AsDuration().String()
	}
	ic := t.GetIdentityConstraints()
	return console.Detail{
		Summary: []console.Property{{Label: "Region", Value: kmsLocation(t.GetName())}, {Label: "Created", Value: kmsTime(t.GetCreateTime())}},
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
			{Heading: "Template", Properties: []console.Property{
				{Label: "Resource name", Value: t.GetName()}, {Label: "Description", Value: kmsOrDash(t.GetDescription())},
				{Label: "Key usages", Value: kmsOrDash(privateCAUsagesText(t.GetPredefinedValues()))},
				{Label: "Request's subject allowed", Value: fmt.Sprint(ic == nil || ic.GetAllowSubjectPassthrough())},
				{Label: "Request's SANs allowed", Value: fmt.Sprint(ic == nil || ic.GetAllowSubjectAltNamesPassthrough())},
				{Label: "Maximum lifetime", Value: life}, {Label: "Labels", Value: kmsOrDash(labelsText(t.GetLabels()))}}}}}},
	}, nil
}

func (p privateCATemplatesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	t, err := p.template(ctx, project, path)
	if err != nil {
		return nil
	}
	return []console.Action{
		{ID: "edit", Label: "Edit template", Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: t.GetDescription()},
			{Name: "usages", Label: "Key usages", Type: "text", Default: privateCAUsagesText(t.GetPredefinedValues()),
				Help: "Comma-separated: " + strings.Join(privateCAUsages, ", ") + "."},
			labelsField(t.GetLabels(), "")}},
		{ID: "delete", Label: "Delete template", Destructive: true, Leaves: true},
	}
}

func (p privateCATemplatesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if _, err := p.template(ctx, project, path); err != nil {
		return err
	}
	api := p.svc.API().API()
	switch action {
	case "edit":
		pv, err := privateCATemplateValues(values["usages"])
		if err != nil {
			return err
		}
		labels, err := console.ParseMap(values["labels"])
		if err != nil {
			return err
		}
		_, err = api.UpdateCertificateTemplate(ctx, &privatecapb.UpdateCertificateTemplateRequest{
			CertificateTemplate: &privatecapb.CertificateTemplate{Name: path[0], Description: strings.TrimSpace(values["description"]), PredefinedValues: pv, Labels: labels},
			UpdateMask:          &fieldmaskpb.FieldMask{Paths: []string{"description", "predefined_values", "labels"}}})
		return err
	case "delete":
		_, err := api.DeleteCertificateTemplate(ctx, &privatecapb.DeleteCertificateTemplateRequest{Name: path[0]})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

func (privateCATemplatesProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "certificate-authority-service--googlecloudsecurityprivatecav1",
		RoleHelp: roleHelp("Certificate Authority Service", "roles/privateca.templateUser"), Conditions: true}
}

func (p privateCATemplatesProvider) iam(ctx context.Context, project string, path []string) (iamServer, string, error) {
	if _, err := p.template(ctx, project, path); err != nil {
		return nil, "", err
	}
	return p.svc.API().IAMPolicy(), path[0], nil
}

func (p privateCATemplatesProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	api, name, err := p.iam(ctx, project, path)
	if err != nil {
		return console.Policy{}, err
	}
	return getProtoPolicy(ctx, api, name)
}

func (p privateCATemplatesProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	api, name, err := p.iam(ctx, project, path)
	if err != nil {
		return err
	}
	return setProtoPolicy(ctx, api, name, pol)
}

func (p privateCATemplatesProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) ([]string, error) {
	api, name, err := p.iam(ctx, project, path)
	if err != nil {
		return nil, err
	}
	return testProtoPermissions(ctx, api, name, perms)
}

var (
	_ console.Creator      = privateCATemplatesProvider{}
	_ console.Driller      = privateCATemplatesProvider{}
	_ console.PathActor    = privateCATemplatesProvider{}
	_ console.PolicyEditor = privateCATemplatesProvider{}
)
