package main

// Developer Connect's Account connectors page (#2105), as Google's
// "Configure account connectors" walks it
// (https://docs.cloud.google.com/developer-connect/docs/configure-connectors,
// read 2026-11-22): Developer Connect > Account connectors lists the
// project's connectors, with Create connector (a region, a name, the
// Pre-configured OAuth clients configuration type, the OAuth provider and
// its scopes, then Create); a connector's name opens its Connector details,
// with Connect your account (which opens its oauth start URI, the
// provider's consent, in a new window), Edit (its scopes; changing them
// removes its users) and its users; a row's menu has Delete. Every read and
// write goes through the official Developer Connect client on the Run port.
//
// GOOGLE, which the instance's own authorization server serves, is the one
// provider offered: GitHub, GitLab and the others authorize at the hosted
// provider, which no instance reaches, and the API refuses them.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	developerconnect "cloud.google.com/go/developerconnect/apiv1"
	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// devConnectAccountsProvider is Developer Connect's Account connectors page.
type devConnectAccountsProvider struct{ ssm sourceManagerProvider }

func (devConnectAccountsProvider) ID() string    { return "developer-connect-account-connectors" }
func (devConnectAccountsProvider) Title() string { return "Account connectors" }

// dcGoogleScopes are the GOOGLE provider's recommended scopes, as
// SystemProvider documents them.
var dcGoogleScopes = []string{"https://www.googleapis.com/auth/drive.readonly", "https://www.googleapis.com/auth/documents.readonly"}

func (p devConnectAccountsProvider) connectors(ctx context.Context, project string) ([]*dcpb.AccountConnector, error) {
	project = p.ssm.project(project)
	var out []*dcpb.AccountConnector
	err := p.ssm.withDevConnect(ctx, func(c *developerconnect.Client) error {
		for _, loc := range runadapter.DeveloperConnectAccountLocations(project) {
			it := c.ListAccountConnectors(ctx, &dcpb.ListAccountConnectorsRequest{Parent: "projects/" + project + "/locations/" + loc})
			for {
				ac, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					return err
				}
				out = append(out, ac)
			}
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool { return lastSegment(out[i].GetName()) < lastSegment(out[j].GetName()) })
	return out, err
}

// connector resolves a connector by its ID; one ID in two locations is
// refused, since the page names connectors by ID alone.
func (p devConnectAccountsProvider) connector(ctx context.Context, project, id string) (*dcpb.AccountConnector, error) {
	all, err := p.connectors(ctx, project)
	if err != nil {
		return nil, err
	}
	var match []*dcpb.AccountConnector
	for _, c := range all {
		if lastSegment(c.GetName()) == id {
			match = append(match, c)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("account connector %q not found", id)
	case 1:
		return match[0], nil
	}
	return nil, fmt.Errorf("account connector %q is in %d regions; use the API with its full name", id, len(match))
}

func dcProviderLabel(c *dcpb.AccountConnector) string {
	if c.GetProviderOauthConfig().GetSystemProviderId() == dcpb.SystemProvider_GOOGLE {
		return "Google"
	}
	return c.GetProviderOauthConfig().GetSystemProviderId().String()
}

func (p devConnectAccountsProvider) users(ctx context.Context, name string) ([]*dcpb.User, error) {
	var out []*dcpb.User
	err := p.ssm.withDevConnect(ctx, func(c *developerconnect.Client) error {
		it := c.ListUsers(ctx, &dcpb.ListUsersRequest{Parent: name})
		for {
			u, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, u)
		}
	})
	return out, err
}

func (p devConnectAccountsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Region", "Provider", "Scopes", "Created"}, NameColumn: "Name", Noun: "account connectors",
		RowsOpenable: true,
		Note: "An account connector connects each user's own account at a provider. Google is the provider served: " +
			"this instance's own OAuth authorization server, signed in as the instance's user. GitHub, GitLab and the other " +
			"providers authorize at the hosted provider, which no instance reaches."}
	if p.ssm.project(project) == "" {
		out.Prompt = "Choose a project to list its account connectors."
		return out, nil
	}
	all, err := p.connectors(ctx, project)
	if err != nil {
		out.Unavailable = apiMessage(err)
		return out, nil
	}
	for _, c := range all {
		out.Items = append(out.Items, console.Resource{Name: lastSegment(c.GetName()), Fields: map[string]string{
			"Region": locationOf(c.GetName()), "Provider": dcProviderLabel(c),
			"Scopes": strings.Join(c.GetProviderOauthConfig().GetScopes(), ", "), "Created": stampText(c.GetCreateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

func (devConnectAccountsProvider) CreateForm() (string, []console.Field) {
	return "Create connector", []console.Field{
		{Name: "region", Label: "Region", Type: "select", Required: true, Options: ssmRegions, OptionLabels: ssmRegionLabels, Default: "us-central1"},
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: "^[a-z]([\\-a-z0-9]{0,61}[a-z0-9])?$",
			Help: "Lowercase letters, numbers and hyphens, starting with a letter. It cannot be changed later."},
		{Name: "configType", Label: "Configuration type", Type: "radio", Options: []string{"preconfigured"}, Default: "preconfigured",
			OptionLabels: map[string]string{"preconfigured": "Pre-configured OAuth clients"},
			Help:         "A custom OAuth client authorizes at your own provider's server, which no instance reaches."},
		{Name: "provider", Label: "OAuth provider", Type: "select", Required: true, Options: []string{"GOOGLE"}, Default: "GOOGLE",
			OptionLabels: map[string]string{"GOOGLE": "Google"},
			Help:         "Google is this instance's own OAuth authorization server. GitHub, GitLab, Sentry, Rovo, New Relic, DataStax and Dynatrace are hosted providers no instance reaches."},
		{Name: "scopes", Label: "Scopes", Type: "textarea", Required: true, Default: strings.Join(dcGoogleScopes, "\n"),
			Help: "One OAuth scope per line; at least one."},
	}
}

func (devConnectAccountsProvider) CreateOnPage() bool   { return true }
func (devConnectAccountsProvider) CreateTitle() string  { return "Create account connector" }
func (devConnectAccountsProvider) CreateSubmit() string { return "Create" }
func (devConnectAccountsProvider) CreateLayout() string { return console.CreateLayoutPage }

// dcScopes are a form's scopes, one per line or comma.
func dcScopes(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t' })
}

func (p devConnectAccountsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	project = p.ssm.project(project)
	if project == "" {
		return "", errors.New("choose a project before creating an account connector")
	}
	provider, ok := dcpb.SystemProvider_value[v["provider"]]
	if !ok {
		return "", fmt.Errorf("unknown OAuth provider %q", v["provider"])
	}
	var name string
	err := p.ssm.withDevConnect(ctx, func(c *developerconnect.Client) error {
		op, err := c.CreateAccountConnector(ctx, &dcpb.CreateAccountConnectorRequest{
			Parent: "projects/" + project + "/locations/" + strings.TrimSpace(v["region"]), AccountConnectorId: strings.TrimSpace(v["name"]),
			AccountConnector: &dcpb.AccountConnector{AccountConnectorConfig: &dcpb.AccountConnector_ProviderOauthConfig{
				ProviderOauthConfig: &dcpb.ProviderOAuthConfig{OauthProviderId: &dcpb.ProviderOAuthConfig_SystemProviderId{
					SystemProviderId: dcpb.SystemProvider(provider)}, Scopes: dcScopes(v["scopes"])}}}})
		if err != nil {
			return err
		}
		ac, err := op.Wait(ctx)
		if err != nil {
			return err
		}
		name = lastSegment(ac.GetName())
		return nil
	})
	return name, err
}

func (p devConnectAccountsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if p.ssm.project(project) == "" {
		return console.Detail{Prompt: "Choose a project to open its account connectors."}, nil
	}
	c, err := p.connector(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	users, err := p.users(ctx, c.GetName())
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	sec := console.Section{ID: "users", Label: "Users", Listing: console.Listing{
		Columns: []string{"Created", "Last token request"}, NameColumn: "User", Noun: "users",
		Note: "Each user who connected their account through this connector, by email. Connect your account opens the " +
			"provider's consent page, signed in as this instance's user."}}
	for _, u := range users {
		last := "-"
		if u.GetLastTokenRequestTime() != nil {
			last = stampText(u.GetLastTokenRequestTime())
		}
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: u.GetDisplayName(), Fields: map[string]string{
			"Created": stampText(u.GetCreateTime()), "Last token request": last}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return console.Detail{
		Title:   lastSegment(c.GetName()),
		Heading: "Connector details",
		Summary: stored(
			console.Property{Label: "Region", Value: locationOf(c.GetName())},
			console.Property{Label: "Configuration type", Value: "Pre-configured OAuth clients"},
			console.Property{Label: "OAuth provider", Value: dcProviderLabel(c)},
			console.Property{Label: "Scopes", Value: strings.Join(c.GetProviderOauthConfig().GetScopes(), ", ")},
			console.Property{Label: "OAuth start URI", Value: c.GetOauthStartUri(), Copy: "Copy URI"},
			console.Property{Label: "Created", Value: stampText(c.GetCreateTime())},
			console.Property{Label: "Updated", Value: stampText(c.GetUpdateTime())},
		),
		Sections: []console.Section{sec},
	}, nil
}

func (p devConnectAccountsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	c, err := p.connector(ctx, project, path[0])
	if err != nil {
		return nil
	}
	out := []console.Action{
		{ID: "connect", Label: "Connect your account", Href: c.GetOauthStartUri()},
		{ID: "edit", Label: "Edit", Submit: "Save",
			Fields: []console.Field{{Name: "scopes", Label: "Scopes", Type: "textarea", Required: true,
				Default: strings.Join(c.GetProviderOauthConfig().GetScopes(), "\n"),
				Help:    "One OAuth scope per line; at least one. Changing the scopes removes every user, who connect again."}}},
	}
	users, _ := p.users(ctx, c.GetName())
	var names []string
	for _, u := range users {
		names = append(names, u.GetDisplayName())
	}
	if len(names) > 0 {
		out = append(out, console.Action{ID: "delete-user", Label: "Delete user", Destructive: true,
			Confirm: "the user's connection; they connect again to use the connector", Fields: []console.Field{
				{Name: "user", Label: "User", Type: "select", Required: true, Options: names, Default: names[0]}}})
	}
	return out
}

func (p devConnectAccountsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	c, err := p.connector(ctx, project, path[0])
	if err != nil {
		return err
	}
	switch action {
	case "edit":
		return p.ssm.withDevConnect(ctx, func(dc *developerconnect.Client) error {
			op, err := dc.UpdateAccountConnector(ctx, &dcpb.UpdateAccountConnectorRequest{
				AccountConnector: &dcpb.AccountConnector{Name: c.GetName(), AccountConnectorConfig: &dcpb.AccountConnector_ProviderOauthConfig{
					ProviderOauthConfig: &dcpb.ProviderOAuthConfig{Scopes: dcScopes(v["scopes"])}}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"provider_oauth_config.scopes"}}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		})
	case "delete-user":
		users, err := p.users(ctx, c.GetName())
		if err != nil {
			return err
		}
		for _, u := range users {
			if u.GetDisplayName() == v["user"] {
				return p.ssm.withDevConnect(ctx, func(dc *developerconnect.Client) error {
					op, err := dc.DeleteUser(ctx, &dcpb.DeleteUserRequest{Name: u.GetName()})
					if err != nil {
						return err
					}
					return op.Wait(ctx)
				})
			}
		}
		return fmt.Errorf("user %q not found", v["user"])
	}
	return fmt.Errorf("unknown action %q", action)
}

// Delete removes a connector and its users, as Google's Delete does.
func (p devConnectAccountsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.connector(ctx, project, name)
	if err != nil {
		return err
	}
	return p.ssm.withDevConnect(ctx, func(dc *developerconnect.Client) error {
		op, err := dc.DeleteAccountConnector(ctx, &dcpb.DeleteAccountConnectorRequest{Name: c.GetName(), Force: true})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	})
}

var (
	_ console.Driller        = devConnectAccountsProvider{}
	_ console.PageCreator    = devConnectAccountsProvider{}
	_ console.CreateTitler   = devConnectAccountsProvider{}
	_ console.CreateLayouter = devConnectAccountsProvider{}
	_ console.Deleter        = devConnectAccountsProvider{}
	_ console.PathActor      = devConnectAccountsProvider{}
)
