package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apphubpb "cloud.google.com/go/apphub/apiv1/apphubpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/apphub"
)

// The console's App Hub pages (#2110), as Google's menu has them:
// Applications (a project's applications in every location, Create
// application with its scope, location and attributes; an application's
// page with its Services and workloads and Details, Register service or
// workload, Edit and Delete), Services and workloads (the boundary's
// discovered services and workloads in every location, registered or not),
// and Settings (the service projects attached to this host project, Attach
// projects and Detach).

const appHubNotStarted = "App Hub has not started"

func appHubAPI(svc *appHubService) (*apphub.Server, error) {
	s := svc.API()
	if s == nil {
		return nil, errors.New(appHubNotStarted)
	}
	return s, nil
}

var (
	appHubCriticalities = []string{"", "MISSION_CRITICAL", "HIGH", "MEDIUM", "LOW"}
	appHubEnvironments  = []string{"", "PRODUCTION", "STAGING", "TEST", "DEVELOPMENT"}
	appHubAttrLabels    = map[string]string{"": "None", "MISSION_CRITICAL": "Mission critical", "HIGH": "High", "MEDIUM": "Medium", "LOW": "Low",
		"PRODUCTION": "Production", "STAGING": "Staging", "TEST": "Test", "DEVELOPMENT": "Development"}
)

func appHubCriticality(a *apphubpb.Attributes) string {
	return appHubAttrLabels[strings.TrimPrefix(a.GetCriticality().GetType().String(), "TYPE_UNSPECIFIED")]
}

func appHubEnvironment(a *apphubpb.Attributes) string {
	return appHubAttrLabels[strings.TrimPrefix(a.GetEnvironment().GetType().String(), "TYPE_UNSPECIFIED")]
}

func appHubOwners(cs []*apphubpb.ContactInfo) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.GetEmail())
	}
	return kmsOrDash(strings.Join(out, ", "))
}

// appHubAttributes reads the attribute fields of a form.
func appHubAttributes(values map[string]string) *apphubpb.Attributes {
	a := &apphubpb.Attributes{}
	if c := values["criticality"]; c != "" {
		a.Criticality = &apphubpb.Criticality{Type: apphubpb.Criticality_Type(apphubpb.Criticality_Type_value[c])}
	}
	if e := values["environment"]; e != "" {
		a.Environment = &apphubpb.Environment{Type: apphubpb.Environment_Type(apphubpb.Environment_Type_value[e])}
	}
	owners := func(s string) []*apphubpb.ContactInfo {
		var out []*apphubpb.ContactInfo
		for _, e := range strings.Split(s, ",") {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, &apphubpb.ContactInfo{Email: e})
			}
		}
		return out
	}
	a.DeveloperOwners, a.OperatorOwners, a.BusinessOwners = owners(values["developerOwners"]), owners(values["operatorOwners"]), owners(values["businessOwners"])
	return a
}

func appHubAttributeFields(a *apphubpb.Attributes, section string) []console.Field {
	enumOf := func(s string) string { return strings.TrimPrefix(s, "TYPE_UNSPECIFIED") }
	join := func(cs []*apphubpb.ContactInfo) string {
		var out []string
		for _, c := range cs {
			out = append(out, c.GetEmail())
		}
		return strings.Join(out, ", ")
	}
	return []console.Field{
		{Name: "criticality", Label: "Criticality", Type: "select", Section: section, Options: appHubCriticalities, OptionLabels: appHubAttrLabels,
			Default: enumOf(a.GetCriticality().GetType().String())},
		{Name: "environment", Label: "Environment", Type: "select", Section: section, Options: appHubEnvironments, OptionLabels: appHubAttrLabels,
			Default: enumOf(a.GetEnvironment().GetType().String())},
		{Name: "developerOwners", Label: "Developer owners", Type: "text", Section: section, Default: join(a.GetDeveloperOwners()), Help: "Email addresses, separated by commas."},
		{Name: "operatorOwners", Label: "Operator owners", Type: "text", Section: section, Default: join(a.GetOperatorOwners()), Help: "Email addresses, separated by commas."},
		{Name: "businessOwners", Label: "Business owners", Type: "text", Section: section, Default: join(a.GetBusinessOwners()), Help: "Email addresses, separated by commas."},
	}
}

// ---- Applications ----

type appHubProvider struct{ svc *appHubService }

func (appHubProvider) ID() string    { return "apphub" }
func (appHubProvider) Title() string { return "Applications" }

func (p appHubProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := appHubAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	listing := console.Listing{Columns: []string{"Display name", "Scope", "Location", "Criticality", "Environment", "Created"}, NameColumn: "Name", Noun: "applications"}
	if project == "" {
		listing.Prompt = "App Hub lists the applications of a host or management project. Choose one in the toolbar."
		return listing, nil
	}
	for _, loc := range apphub.Locations() {
		resp, err := s.ListApplications(ctx, &apphubpb.ListApplicationsRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, a := range resp.GetApplications() {
			listing.Items = append(listing.Items, console.Resource{Name: a.GetName(), Fields: map[string]string{
				"Display name": kmsOrDash(a.GetDisplayName()), "Scope": scopeLabel(a.GetScope().GetType()), "Location": kmsLocation(a.GetName()),
				"Criticality": appHubCriticality(a.GetAttributes()), "Environment": appHubEnvironment(a.GetAttributes()), "Created": kmsTime(a.GetCreateTime())}})
		}
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func scopeLabel(t apphubpb.Scope_Type) string {
	switch t {
	case apphubpb.Scope_GLOBAL:
		return "Global"
	case apphubpb.Scope_REGIONAL:
		return "Regional"
	}
	return "-"
}

func (p appHubProvider) CreateForm() (string, []console.Field) {
	const details, attrs = "Application details", "Attributes"
	fields := []console.Field{
		{Name: "scope", Label: "Application scope", Type: "radio", Default: "GLOBAL", Section: details, Options: []string{"GLOBAL", "REGIONAL"},
			OptionLabels: map[string]string{"GLOBAL": "Global", "REGIONAL": "Regional"},
			Help:         "A global application can hold resources from any location; a regional one only its own region's."},
		{Name: "region", Label: "Region", Type: "select", Section: details, ShowWhen: "scope=REGIONAL", Options: apphub.Regions, Default: "us-central1"},
		{Name: "applicationId", Label: "Application name", Type: "text", Required: true, Section: details, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`,
			Help: "Lower-case letters, numbers and hyphens, starting with a letter; at most 63 characters."},
		{Name: "displayName", Label: "Display name", Type: "text", Section: details, Help: "At most 63 characters."},
		{Name: "description", Label: "Description", Type: "textarea", Section: details, Help: "At most 2,048 characters."},
	}
	return "Create application", append(fields, appHubAttributeFields(nil, attrs)...)
}

func (p appHubProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := appHubAPI(p.svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating an application")
	}
	scope, loc := apphubpb.Scope_GLOBAL, "global"
	if values["scope"] == "REGIONAL" {
		scope, loc = apphubpb.Scope_REGIONAL, strings.TrimSpace(values["region"])
	}
	parent := "projects/" + project + "/locations/" + loc
	id := strings.TrimSpace(values["applicationId"])
	_, err = s.CreateApplication(ctx, &apphubpb.CreateApplicationRequest{Parent: parent,
		ApplicationId: id, Application: &apphubpb.Application{
			DisplayName: values["displayName"], Description: values["description"], Scope: &apphubpb.Scope{Type: scope},
			Attributes: appHubAttributes(values)}})
	if err != nil {
		return "", err
	}
	return parent + "/applications/" + id, nil
}

func (p appHubProvider) app(ctx context.Context, project, name string) (*apphub.Server, *apphubpb.Application, error) {
	s, err := appHubAPI(p.svc)
	if err != nil {
		return nil, nil, err
	}
	if err := kmsOwned(project, name); err != nil {
		return nil, nil, err
	}
	a, err := s.GetApplication(ctx, &apphubpb.GetApplicationRequest{Name: name})
	return s, a, err
}

// appHubComponents are an application's registered services and
// workloads, as one listing.
func appHubComponents(ctx context.Context, s *apphub.Server, app string) console.Listing {
	l := console.Listing{Columns: []string{"Type", "Resource", "Location", "State", "Criticality", "Environment"}, NameColumn: "Name", Noun: "services and workloads"}
	if resp, err := s.ListServices(ctx, &apphubpb.ListServicesRequest{Parent: app, PageSize: 1000}); err == nil {
		for _, v := range resp.GetServices() {
			l.Items = append(l.Items, console.Resource{Name: lastSegment(v.GetName()), Status: v.GetState().String(), Fields: map[string]string{
				"Type": "Service", "Resource": v.GetServiceReference().GetUri(), "Location": v.GetServiceProperties().GetLocation(), "State": v.GetState().String(),
				"Criticality": appHubCriticality(v.GetAttributes()), "Environment": appHubEnvironment(v.GetAttributes())}})
		}
	}
	if resp, err := s.ListWorkloads(ctx, &apphubpb.ListWorkloadsRequest{Parent: app, PageSize: 1000}); err == nil {
		for _, v := range resp.GetWorkloads() {
			l.Items = append(l.Items, console.Resource{Name: lastSegment(v.GetName()), Status: v.GetState().String(), Fields: map[string]string{
				"Type": "Workload", "Resource": v.GetWorkloadReference().GetUri(), "Location": v.GetWorkloadProperties().GetLocation(), "State": v.GetState().String(),
				"Criticality": appHubCriticality(v.GetAttributes()), "Environment": appHubEnvironment(v.GetAttributes())}})
		}
	}
	l.Total = len(l.Items)
	return l
}

func (p appHubProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	s, a, err := p.app(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the application: " + consoleErr(err)}, nil
	}
	at := a.GetAttributes()
	return console.Detail{
		Summary: []console.Property{{Label: "Scope", Value: scopeLabel(a.GetScope().GetType())}, {Label: "Location", Value: kmsLocation(a.GetName())}},
		Sections: []console.Section{
			{ID: "components", Label: "Services and workloads", Listing: appHubComponents(ctx, s, a.GetName())},
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
				{Heading: "Application", Properties: []console.Property{
					{Label: "Resource name", Value: a.GetName()}, {Label: "Display name", Value: kmsOrDash(a.GetDisplayName())},
					{Label: "Description", Value: kmsOrDash(a.GetDescription())}, {Label: "Scope", Value: scopeLabel(a.GetScope().GetType())},
					{Label: "State", Value: a.GetState().String()}, {Label: "Created", Value: kmsTime(a.GetCreateTime())}, {Label: "Last updated", Value: kmsTime(a.GetUpdateTime())}}},
				{Heading: "Attributes", Properties: []console.Property{
					{Label: "Criticality", Value: appHubCriticality(at)}, {Label: "Environment", Value: appHubEnvironment(at)},
					{Label: "Developer owners", Value: appHubOwners(at.GetDeveloperOwners())}, {Label: "Operator owners", Value: appHubOwners(at.GetOperatorOwners())},
					{Label: "Business owners", Value: appHubOwners(at.GetBusinessOwners())}}}}},
		},
	}, nil
}

// registrable are the discovered services and workloads app may register:
// its own region's, or every location's for a global one; as options
// "service|NAME" and "workload|NAME", labelled by resource URI.
func registrable(ctx context.Context, s *apphub.Server, app *apphubpb.Application) ([]string, map[string]string) {
	m := strings.Split(app.GetName(), "/")
	host, loc := m[1], m[3]
	locs := []string{loc}
	if app.GetScope().GetType() == apphubpb.Scope_GLOBAL {
		locs = apphub.Locations()
	}
	var opts []string
	labels := map[string]string{}
	for _, l := range locs {
		parent := "projects/" + host + "/locations/" + l
		if r, err := s.ListDiscoveredServices(ctx, &apphubpb.ListDiscoveredServicesRequest{Parent: parent, PageSize: 1000}); err == nil {
			for _, d := range r.GetDiscoveredServices() {
				k := "service|" + d.GetName()
				opts, labels[k] = append(opts, k), "Service: "+d.GetServiceReference().GetUri()
			}
		}
		if r, err := s.ListDiscoveredWorkloads(ctx, &apphubpb.ListDiscoveredWorkloadsRequest{Parent: parent, PageSize: 1000}); err == nil {
			for _, d := range r.GetDiscoveredWorkloads() {
				k := "workload|" + d.GetName()
				opts, labels[k] = append(opts, k), "Workload: "+d.GetWorkloadReference().GetUri()
			}
		}
	}
	sort.Strings(opts)
	return opts, labels
}

func (p appHubProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	s, a, err := p.app(ctx, project, path[0])
	if err != nil {
		return nil
	}
	opts, labels := registrable(ctx, s, a)
	register := console.Action{ID: "register", Label: "Register service/workload", Fields: append([]console.Field{
		{Name: "resource", Label: "Service or workload", Type: "select", Required: true, Options: opts, OptionLabels: labels,
			Help: "A discovered service or workload of the boundary; a regional application registers its own region's only."},
		{Name: "id", Label: "Name", Type: "text", Required: true, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`},
		{Name: "displayName", Label: "Display name", Type: "text"}}, appHubAttributeFields(nil, "")...)}
	if len(opts) == 0 {
		register.Unavailable = "The boundary has no discovered service or workload this application can register: attach a project in Settings, and deploy a Cloud Run service or job there."
	}
	return []console.Action{register,
		{ID: "edit", Label: "Edit", Fields: append([]console.Field{
			{Name: "displayName", Label: "Display name", Type: "text", Default: a.GetDisplayName()},
			{Name: "description", Label: "Description", Type: "textarea", Default: a.GetDescription()}}, appHubAttributeFields(a.GetAttributes(), "")...)},
		{ID: "unregister", Label: "Unregister", Destructive: true, Fields: []console.Field{
			{Name: "name", Label: "Service or workload name", Type: "text", Required: true}}, Confirm: "The service or workload goes back to discovered."},
		{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, Confirm: "An application can be deleted once its services and workloads are unregistered."},
	}
}

func (p appHubProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("no application")
	}
	s, a, err := p.app(ctx, project, path[0])
	if err != nil {
		return err
	}
	switch action {
	case "register":
		kind, disc, _ := strings.Cut(values["resource"], "|")
		id := strings.TrimSpace(values["id"])
		attrs := appHubAttributes(values)
		if kind == "workload" {
			_, err = s.CreateWorkload(ctx, &apphubpb.CreateWorkloadRequest{Parent: a.GetName(), WorkloadId: id,
				Workload: &apphubpb.Workload{DiscoveredWorkload: disc, DisplayName: values["displayName"], Attributes: attrs}})
			return err
		}
		if strings.Contains(disc, "/discoveredServices/") {
			if d, derr := s.GetDiscoveredService(ctx, &apphubpb.GetDiscoveredServiceRequest{Name: disc}); derr == nil &&
				d.GetServiceProperties().GetRegistrationType().GetType() == apphubpb.RegistrationType_SHARED {
				attrs = nil
			}
		}
		_, err = s.CreateService(ctx, &apphubpb.CreateServiceRequest{Parent: a.GetName(), ServiceId: id,
			Service: &apphubpb.Service{DiscoveredService: disc, DisplayName: values["displayName"], Attributes: attrs}})
		return err
	case "edit":
		_, err = s.UpdateApplication(ctx, &apphubpb.UpdateApplicationRequest{Application: &apphubpb.Application{Name: a.GetName(),
			DisplayName: values["displayName"], Description: values["description"], Attributes: appHubAttributes(values)},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "description", "attributes"}}})
		return err
	case "unregister":
		n := strings.TrimSpace(values["name"])
		if _, err := s.GetService(ctx, &apphubpb.GetServiceRequest{Name: a.GetName() + "/services/" + n}); err == nil {
			_, err = s.DeleteService(ctx, &apphubpb.DeleteServiceRequest{Name: a.GetName() + "/services/" + n})
			return err
		}
		_, err = s.DeleteWorkload(ctx, &apphubpb.DeleteWorkloadRequest{Name: a.GetName() + "/workloads/" + n})
		return err
	case "delete":
		_, err = s.DeleteApplication(ctx, &apphubpb.DeleteApplicationRequest{Name: a.GetName()})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// ---- Services and workloads ----

type appHubDiscoveredProvider struct{ svc *appHubService }

func (appHubDiscoveredProvider) ID() string    { return "apphub-discovered" }
func (appHubDiscoveredProvider) Title() string { return "Services and workloads" }

func (p appHubDiscoveredProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := appHubAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	listing := console.Listing{Columns: []string{"Type", "Location", "Project", "Registration type", "Status"}, NameColumn: "Resource", Noun: "services and workloads"}
	if project == "" {
		listing.Prompt = "App Hub discovers the services and workloads of a host project's boundary. Choose the host project in the toolbar."
		return listing, nil
	}
	registered := map[string]string{}
	for _, loc := range apphub.Locations() {
		apps, err := s.ListApplications(ctx, &apphubpb.ListApplicationsRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, a := range apps.GetApplications() {
			if r, err := s.ListServices(ctx, &apphubpb.ListServicesRequest{Parent: a.GetName(), PageSize: 1000}); err == nil {
				for _, v := range r.GetServices() {
					registered[v.GetDiscoveredService()] = "Registered in " + lastSegment(a.GetName())
				}
			}
			if r, err := s.ListWorkloads(ctx, &apphubpb.ListWorkloadsRequest{Parent: a.GetName(), PageSize: 1000}); err == nil {
				for _, v := range r.GetWorkloads() {
					registered[v.GetDiscoveredWorkload()] = "Registered in " + lastSegment(a.GetName())
				}
			}
		}
	}
	status := func(name string) string {
		if s := registered[name]; s != "" {
			return s
		}
		return "Discovered"
	}
	for _, loc := range apphub.Locations() {
		parent := "projects/" + project + "/locations/" + loc
		ds, err := s.ListDiscoveredServices(ctx, &apphubpb.ListDiscoveredServicesRequest{Parent: parent, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, d := range ds.GetDiscoveredServices() {
			pr := d.GetServiceProperties()
			listing.Items = append(listing.Items, console.Resource{Name: d.GetServiceReference().GetUri(), Fields: map[string]string{
				"Type": "Service", "Location": pr.GetLocation(), "Project": strings.TrimPrefix(pr.GetGcpProject(), "projects/"),
				"Registration type": strings.Title(strings.ToLower(pr.GetRegistrationType().GetType().String())), "Status": status(d.GetName())}})
		}
		dw, err := s.ListDiscoveredWorkloads(ctx, &apphubpb.ListDiscoveredWorkloadsRequest{Parent: parent, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, d := range dw.GetDiscoveredWorkloads() {
			pr := d.GetWorkloadProperties()
			listing.Items = append(listing.Items, console.Resource{Name: d.GetWorkloadReference().GetUri(), Fields: map[string]string{
				"Type": "Workload", "Location": pr.GetLocation(), "Project": strings.TrimPrefix(pr.GetGcpProject(), "projects/"),
				"Registration type": "-", "Status": status(d.GetName())}})
		}
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

// ---- Settings: service projects ----

type appHubSettingsProvider struct{ svc *appHubService }

func (appHubSettingsProvider) ID() string    { return "apphub-settings" }
func (appHubSettingsProvider) Title() string { return "Settings" }

func (p appHubSettingsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := appHubAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	listing := console.Listing{Columns: []string{"Project number", "State", "Attached"}, NameColumn: "Service project", Noun: "service projects"}
	if project == "" {
		listing.Prompt = "Choose the host project in the toolbar to see its service projects."
		return listing, nil
	}
	resp, err := s.ListServiceProjectAttachments(ctx, &apphubpb.ListServiceProjectAttachmentsRequest{Parent: "projects/" + project + "/locations/global", PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	for _, a := range resp.GetServiceProjectAttachments() {
		listing.Items = append(listing.Items, console.Resource{Name: a.GetName(), Fields: map[string]string{
			"Project number": strings.TrimPrefix(a.GetServiceProject(), "projects/"), "State": a.GetState().String(), "Attached": kmsTime(a.GetCreateTime())}})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func (p appHubSettingsProvider) CreateForm() (string, []console.Field) {
	return "Attach projects", []console.Field{{Name: "project", Label: "Service project", Type: "text", Required: true,
		Help: "The ID of a project to attach to this host project. A project can be attached to one host project at a time."}}
}

func (p appHubSettingsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := appHubAPI(p.svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", fmt.Errorf("choose the host project first")
	}
	id := strings.TrimSpace(values["project"])
	parent := "projects/" + project + "/locations/global"
	_, err = s.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: parent, ServiceProjectAttachmentId: id,
		ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/" + id}})
	if err != nil {
		return "", err
	}
	return parent + "/serviceProjectAttachments/" + id, nil
}

func (p appHubSettingsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	s, err := appHubAPI(p.svc)
	if err != nil {
		return console.Detail{}, err
	}
	if err := kmsOwned(project, path[0]); err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	a, err := s.GetServiceProjectAttachment(ctx, &apphubpb.GetServiceProjectAttachmentRequest{Name: path[0]})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the attachment: " + consoleErr(err)}, nil
	}
	return console.Detail{Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{
		Heading: "Service project attachment", Properties: []console.Property{{Label: "Resource name", Value: a.GetName()},
			{Label: "Service project", Value: a.GetServiceProject()}, {Label: "State", Value: a.GetState().String()},
			{Label: "Attached", Value: kmsTime(a.GetCreateTime())}, {Label: "Unique ID", Value: a.GetUid()}}}}}}}, nil
}

func (p appHubSettingsProvider) DetailActions(context.Context, string, []string) []console.Action {
	return []console.Action{{ID: "detach", Label: "Detach", Destructive: true, Leaves: true,
		Confirm: "The project's resources leave the boundary; services and workloads registered from it become detached."}}
}

func (p appHubSettingsProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	s, err := appHubAPI(p.svc)
	if err != nil {
		return err
	}
	if len(path) != 1 || action != "detach" {
		return fmt.Errorf("unknown action %q", action)
	}
	if err := kmsOwned(project, path[0]); err != nil {
		return err
	}
	_, err = s.DeleteServiceProjectAttachment(ctx, &apphubpb.DeleteServiceProjectAttachmentRequest{Name: path[0]})
	return err
}

var (
	_ console.Creator   = appHubProvider{}
	_ console.Driller   = appHubProvider{}
	_ console.PathActor = appHubProvider{}
	_ console.Creator   = appHubSettingsProvider{}
	_ console.Driller   = appHubSettingsProvider{}
	_ console.PathActor = appHubSettingsProvider{}
)
