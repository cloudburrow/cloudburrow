package main

// Private Service Connect's Connection policies (#2141), the Network
// Connectivity API's service connection policies, as Google's console has
// them (https://docs.cloud.google.com/vpc/docs/configure-service-connection-policies
// and https://docs.cloud.google.com/sql/docs/postgres/configure-private-service-connect,
// re-read 2026-11-22): the Private Service Connect page's Connection
// Policies tab; Create connection policy with Name, Service details
// (Source service class Google services, Service class), Target endpoints
// scope (Network, Region), Custom service instance scope (Scopes),
// Endpoints configuration (Subnetworks) and Connection limit, ending in
// Create policy; a policy's page, whose Edit ends in Update policy; Delete.
// The list's columns and the policy page's layout are not in the docs
// (UNVERIFIED, re-read 2026-11-22: the same pages).

import (
	"context"
	"errors"
	"strconv"
	"strings"

	ncpb "cloud.google.com/go/networkconnectivity/apiv1/networkconnectivitypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/service/networkconnectivity"
)

type ncPoliciesProvider struct{ computeScreen }

func (ncPoliciesProvider) ID() string    { return "psc-policies" }
func (ncPoliciesProvider) Title() string { return "Connection policies" }

const ncNotStarted = "the Network Connectivity API has not started (it is served with Compute Engine)"

// ncServiceClasses are the Google service classes the form offers:
// Cloud SQL's, whose automatic endpoints CloudBurrow makes. Others are
// typed.
var ncServiceClasses = []string{networkconnectivity.CloudSQLServiceClass}

func (p ncPoliciesProvider) nc() *networkconnectivity.Server { return p.svc.NetworkConnectivity() }

func (p ncPoliciesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Name", "Region", "Network", "Service class", "Subnetworks", "Connection limit", "Connections"},
		NameColumn: "Name", Noun: "connection policies",
		Note: "A service connection policy lets a service, such as Cloud SQL, create Private Service Connect endpoints in a network's subnets for you."}
	nc := p.nc()
	if nc == nil {
		return listing, errors.New(ncNotStarted)
	}
	if project == "" {
		listing.Prompt = "Connection policies are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	resp, err := nc.ListServiceConnectionPolicies(ctx, &ncpb.ListServiceConnectionPoliciesRequest{Parent: "projects/" + project + "/locations/-", PageSize: 1000})
	if err != nil {
		return listing, err
	}
	for _, pol := range resp.GetServiceConnectionPolicies() {
		region := ncRegion(pol.GetName())
		limit := "None"
		if l := pol.GetPscConfig().Limit; l != nil {
			limit = strconv.FormatInt(*l, 10)
		}
		subs := make([]string, 0, len(pol.GetPscConfig().GetSubnetworks()))
		for _, s := range pol.GetPscConfig().GetSubnetworks() {
			subs = append(subs, vpcLast(s))
		}
		listing.Items = append(listing.Items, console.Resource{Name: region + "/" + vpcLast(pol.GetName()),
			Fields: map[string]string{"Name": vpcLast(pol.GetName()), "Region": region, "Network": vpcLast(pol.GetNetwork()),
				"Service class": pol.GetServiceClass(), "Subnetworks": vpcDash(strings.Join(subs, ", ")), "Connection limit": limit,
				"Connections": strconv.Itoa(len(pol.GetPscConnections()))}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func ncRegion(name string) string {
	_, rest, _ := strings.Cut(name, "/locations/")
	r, _, _ := strings.Cut(rest, "/")
	return r
}

func ncPolicyFields(nets []string) []console.Field {
	if len(nets) == 0 {
		nets = []string{"default"}
	}
	scope := map[string]string{"customScope": "true"}
	return []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "source", Label: "Source service class", Type: "radio", Default: "google", Options: []string{"google"},
			OptionLabels: map[string]string{"google": "Google services"}, Section: "Service details"},
		{Name: "serviceClass", Label: "Service class", Type: "select", Required: true, Default: ncServiceClasses[0], Options: ncServiceClasses,
			Section: "Service details", Help: "The service that may create endpoints through this policy: google-cloud-sql for Cloud SQL."},
		{Name: "network", Label: "Network", Type: "select", Required: true, Default: "default", Options: nets, Section: "Target endpoints scope"},
		{Name: "region", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: locations.Regions, Section: "Target endpoints scope"},
		{Name: "customScope", Label: "Custom service instance scope", Type: "checkbox", Section: "Target endpoints scope",
			Help: "By default, endpoints connect to service instances in this project. Choose organizations, folders and projects instead."},
		{Name: "scopes", Label: "Scopes", Type: "text", VisibleWhen: scope, Section: "Target endpoints scope",
			Help: "projects/PROJECT_ID, folders/FOLDER_ID or organizations/ORGANIZATION_ID, separated by commas."},
		{Name: "subnetworks", Label: "Subnetworks", Type: "text", Required: true, Default: "default", Section: "Endpoints configuration",
			Help: "Subnets of the network in the region, separated by commas: the endpoints' IP addresses are allocated from them."},
		{Name: "limit", Label: "Connection limit", Type: "number", Section: "Endpoints configuration",
			Help: "The most endpoints the service may create in this network and region. Empty is no limit."},
	}
}

func (ncPoliciesProvider) CreateForm() (string, []console.Field) {
	return "Create connection policy", ncPolicyFields(nil)
}

func (p ncPoliciesProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	return ncPolicyFields(p.networkNames(ctx, project))
}

func (ncPoliciesProvider) CreateTitle() string  { return "Create connection policy" }
func (ncPoliciesProvider) CreateSubmit() string { return "Create policy" }

// ncPscConfig is the form's subnetworks, limit and scope.
func ncPscConfig(project, region string, v map[string]string) (*ncpb.ServiceConnectionPolicy_PscConfig, error) {
	pc := &ncpb.ServiceConnectionPolicy_PscConfig{}
	for _, s := range strings.Split(v["subnetworks"], ",") {
		if s = strings.TrimSpace(s); s != "" {
			if !strings.Contains(s, "/") {
				s = "projects/" + project + "/regions/" + region + "/subnetworks/" + s
			}
			pc.Subnetworks = append(pc.Subnetworks, s)
		}
	}
	if l := strings.TrimSpace(v["limit"]); l != "" {
		n, err := strconv.ParseInt(l, 10, 64)
		if err != nil || n < 0 {
			return nil, errors.New("Connection limit must be a whole number, 0 or more")
		}
		pc.Limit = &n
	}
	if v["customScope"] == "true" {
		pc.ProducerInstanceLocation = ncpb.ServiceConnectionPolicy_PscConfig_CUSTOM_RESOURCE_HIERARCHY_LEVELS
		for _, s := range strings.Split(v["scopes"], ",") {
			if s = strings.TrimSpace(s); s != "" {
				pc.AllowedGoogleProducersResourceHierarchyLevel = append(pc.AllowedGoogleProducersResourceHierarchyLevel, s)
			}
		}
	}
	return pc, nil
}

func (p ncPoliciesProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	nc := p.nc()
	if nc == nil {
		return "", errors.New(ncNotStarted)
	}
	name, region := strings.TrimSpace(v["name"]), strings.TrimSpace(v["region"])
	pc, err := ncPscConfig(project, region, v)
	if err != nil {
		return "", err
	}
	_, err = nc.CreateServiceConnectionPolicy(ctx, &ncpb.CreateServiceConnectionPolicyRequest{Parent: "projects/" + project + "/locations/" + region,
		ServiceConnectionPolicyId: name, ServiceConnectionPolicy: &ncpb.ServiceConnectionPolicy{Description: strings.TrimSpace(v["description"]),
			Network: "projects/" + project + "/global/networks/" + strings.TrimSpace(v["network"]), ServiceClass: strings.TrimSpace(v["serviceClass"]), PscConfig: pc}})
	if err != nil {
		return "", err
	}
	return region + "/" + name, nil
}

func (p ncPoliciesProvider) policyName(project, row string) (string, error) {
	region, id, ok := strings.Cut(row, "/")
	if !ok {
		return "", errors.New("a connection policy is named REGION/NAME")
	}
	return "projects/" + project + "/locations/" + region + "/serviceConnectionPolicies/" + id, nil
}

func (p ncPoliciesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	nc := p.nc()
	if nc == nil {
		return console.Detail{Unavailable: ncNotStarted}, nil
	}
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	name, err := p.policyName(project, path[0])
	if err != nil {
		return console.Detail{}, err
	}
	pol, err := nc.GetServiceConnectionPolicy(ctx, &ncpb.GetServiceConnectionPolicyRequest{Name: name})
	if err != nil {
		return console.Detail{}, err
	}
	pc := pol.GetPscConfig()
	limit := "None"
	if pc.Limit != nil {
		limit = strconv.FormatInt(*pc.Limit, 10)
	}
	scope := "This project"
	if pc.GetProducerInstanceLocation() == ncpb.ServiceConnectionPolicy_PscConfig_CUSTOM_RESOURCE_HIERARCHY_LEVELS {
		scope = strings.Join(pc.GetAllowedGoogleProducersResourceHierarchyLevel(), ", ")
	}
	subs := make([]string, 0, len(pc.GetSubnetworks()))
	for _, s := range pc.GetSubnetworks() {
		subs = append(subs, vpcLast(s))
	}
	conns := console.Listing{Columns: []string{"Endpoint", "IP address", "Subnetwork", "Project", "Status", "Service class"}, NameColumn: "Endpoint",
		Noun: "connections", Note: "The Private Service Connect endpoints services created through this policy."}
	for _, c := range pol.GetPscConnections() {
		conns.Items = append(conns.Items, console.Resource{Name: vpcLast(c.GetConsumerForwardingRule()),
			Fields: map[string]string{"Endpoint": vpcLast(c.GetConsumerForwardingRule()), "IP address": c.GetConsumerAddress(),
				"Subnetwork": vpcLast(c.GetSelectedSubnetwork()), "Project": c.GetConsumerTargetProject(), "Status": vpcTitle(c.GetState().String()),
				"Service class": c.GetServiceClass()}})
	}
	conns.Total = len(conns.Items)
	edit := []console.Field{
		{Name: "subnetworks", Label: "Subnetworks", Type: "text", Required: true, Default: strings.Join(subs, ", "),
			Help: "Subnets of the network in the region, separated by commas. Existing endpoints keep their addresses."},
		{Name: "limit", Label: "Connection limit", Type: "number", Help: "Empty is no limit. A limit below the current count blocks new endpoints only."},
		{Name: "customScope", Label: "Custom service instance scope", Type: "checkbox"},
		{Name: "scopes", Label: "Scopes", Type: "text", VisibleWhen: map[string]string{"customScope": "true"},
			Help: "projects/PROJECT_ID, folders/FOLDER_ID or organizations/ORGANIZATION_ID, separated by commas."},
	}
	if pc.Limit != nil {
		edit[1].Default = limit
	}
	if pc.GetProducerInstanceLocation() == ncpb.ServiceConnectionPolicy_PscConfig_CUSTOM_RESOURCE_HIERARCHY_LEVELS {
		edit[2].Default, edit[3].Default = "true", scope
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Name", Value: vpcLast(pol.GetName())}, {Label: "Region", Value: ncRegion(pol.GetName())}},
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
				{Heading: "Service details", Properties: []console.Property{{Label: "Service class", Value: pol.GetServiceClass()},
					{Label: "Description", Value: vpcDash(pol.GetDescription())}}},
				{Heading: "Target endpoints scope", Properties: []console.Property{{Label: "Network", Value: vpcLast(pol.GetNetwork())},
					{Label: "Region", Value: ncRegion(pol.GetName())}, {Label: "Service instance scope", Value: scope}}},
				{Heading: "Endpoints configuration", Properties: []console.Property{{Label: "Subnetworks", Value: vpcDash(strings.Join(subs, ", "))},
					{Label: "Connection limit", Value: limit}}},
			}},
			{ID: "connections", Label: "Connections", Listing: conns},
		},
		Edit: &console.EditForm{Label: "Edit", Title: "Edit connection policy", Submit: "Update policy", Fields: edit,
			Note: "Only the subnetworks, the connection limit and the service instance scope of a policy can be changed; delete it and create a new one to change the rest."},
	}, nil
}

func (p ncPoliciesProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	nc := p.nc()
	if nc == nil {
		return errors.New(ncNotStarted)
	}
	if len(path) != 1 {
		return errors.New("only a policy can be edited")
	}
	name, err := p.policyName(project, path[0])
	if err != nil {
		return err
	}
	pc, err := ncPscConfig(project, ncRegion(name), v)
	if err != nil {
		return err
	}
	_, err = nc.UpdateServiceConnectionPolicy(ctx, &ncpb.UpdateServiceConnectionPolicyRequest{
		ServiceConnectionPolicy: &ncpb.ServiceConnectionPolicy{Name: name, PscConfig: pc},
		UpdateMask:              &fieldmaskpb.FieldMask{Paths: []string{"psc_config"}}})
	return err
}

func (p ncPoliciesProvider) Delete(ctx context.Context, project, row string) error {
	nc := p.nc()
	if nc == nil {
		return errors.New(ncNotStarted)
	}
	name, err := p.policyName(project, row)
	if err != nil {
		return err
	}
	_, err = nc.DeleteServiceConnectionPolicy(ctx, &ncpb.DeleteServiceConnectionPolicyRequest{Name: name})
	return err
}
