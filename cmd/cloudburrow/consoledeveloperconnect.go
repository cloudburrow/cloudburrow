package main

// Developer Connect on Secure Source Manager's pages (#1546): an instance's
// page lists the connections to it, with Connect to Developer Connect and
// Delete connection; a repository's page lists the git repository links to
// it, with Link repository and Delete link. Every change goes through the
// official Developer Connect client on the Run port, as an SDK's would.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	developerconnect "cloud.google.com/go/developerconnect/apiv1"
	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// withDevConnect dials Developer Connect on the Run port as an SDK would.
func (p sourceManagerProvider) withDevConnect(ctx context.Context, fn func(*developerconnect.Client) error) error {
	addr := ""
	if p.b.runAddr != nil {
		addr = p.b.runAddr()
	}
	if addr == "" {
		return errors.New("the Cloud Run adapter is not running")
	}
	c, err := developerconnect.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return fmt.Errorf("connect to Developer Connect: %w", err)
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

// instanceConnections are the connections to an instance.
func instanceConnections(project string, in *ssmpb.Instance) []*dcpb.Connection {
	var out []*dcpb.Connection
	for _, c := range runadapter.DeveloperConnectConnections(project) {
		if c.GetSecureSourceManagerInstanceConfig().GetInstance() == in.GetName() {
			out = append(out, c)
		}
	}
	return out
}

// connectionsSection lists the connections to an instance.
func (p sourceManagerProvider) connectionsSection(project string, in *ssmpb.Instance) console.Section {
	sec := console.Section{ID: "connections", Label: "Developer Connect connections", Listing: console.Listing{
		Columns: []string{"Location", "Links", "Age"}, NameColumn: "Connection", Noun: "connections", AlwaysStatus: true},
		Note: "Connections to this instance; a repository's links are on its page, and a trigger names one in devConnectGitRepositoryLink."}
	for _, c := range instanceConnections(p.project(project), in) {
		st := "Installed"
		if c.GetDisabled() {
			st = "Disabled"
		}
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: lastSegment(c.GetName()), Status: st, Fields: map[string]string{
			"Location": locationOf(c.GetName()), "Links": fmt.Sprint(len(runadapter.DeveloperConnectLinks(c.GetName()))),
			"Age": stampAge(c.GetCreateTime())}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// linksSection lists the links to a repository.
func (p sourceManagerProvider) linksSection(project string, in *ssmpb.Instance, r *ssmpb.Repository) console.Section {
	sec := console.Section{ID: "links", Label: "Git repository links", Listing: console.Listing{
		Columns: []string{"Connection", "Clone URI", "Age"}, NameColumn: "Link", Noun: "links"},
		Note: "Developer Connect links to this repository; a trigger in " + runadapter.SourceManagerTriggersFile +
			" runs through one when its devConnectGitRepositoryLink names it."}
	for _, c := range instanceConnections(p.project(project), in) {
		for _, l := range runadapter.DeveloperConnectLinks(c.GetName()) {
			if l.GetCloneUri() == r.GetUris().GetGitHttps() {
				sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: l.GetName(), Fields: map[string]string{
					"Connection": lastSegment(c.GetName()), "Clone URI": l.GetCloneUri(), "Age": stampAge(l.GetCreateTime())}})
			}
		}
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// connectionActions are an instance page's Developer Connect actions.
func (p sourceManagerProvider) connectionActions(project string, in *ssmpb.Instance) []console.Action {
	out := []console.Action{{ID: "create-connection", Label: "Connect to Developer Connect", Fields: []console.Field{
		{Name: "id", Label: "Connection ID", Type: "text", Required: true, Pattern: "^[a-z]([\\-a-z0-9]{0,61}[a-z0-9])?$",
			Help: "made in the instance's region, " + locationOf(in.GetName())}}}}
	var names []string
	for _, c := range instanceConnections(p.project(project), in) {
		names = append(names, lastSegment(c.GetName()))
	}
	if len(names) > 0 {
		out = append(out, console.Action{ID: "delete-connection", Label: "Delete connection", Destructive: true,
			Confirm: "the connection (its links must be deleted first)", Fields: []console.Field{
				{Name: "connection", Label: "Connection", Type: "select", Required: true, Options: names, Default: names[0]}}})
	}
	return out
}

// linkActions are a repository page's Developer Connect actions.
func (p sourceManagerProvider) linkActions(project string, in *ssmpb.Instance, r *ssmpb.Repository) []console.Action {
	var conns []string
	for _, c := range instanceConnections(p.project(project), in) {
		conns = append(conns, lastSegment(c.GetName()))
	}
	if len(conns) == 0 {
		return nil
	}
	out := []console.Action{{ID: "create-link", Label: "Link repository", Fields: []console.Field{
		{Name: "connection", Label: "Connection", Type: "select", Required: true, Options: conns, Default: conns[0]},
		{Name: "id", Label: "Link ID", Type: "text", Required: true, Pattern: "^[a-z]([\\-a-z0-9]{0,61}[a-z0-9])?$"}}}}
	var links []string
	for _, item := range p.linksSection(project, in, r).Listing.Items {
		links = append(links, item.Name)
	}
	if len(links) > 0 {
		out = append(out, console.Action{ID: "delete-link", Label: "Delete link", Destructive: true,
			Confirm: "the link; triggers naming it stop running", Fields: []console.Field{
				{Name: "link", Label: "Link", Type: "select", Required: true, Options: links, Default: links[0]}}})
	}
	return out
}

// devConnectAct performs a Developer Connect action; ok is false for
// another action.
func (p sourceManagerProvider) devConnectAct(ctx context.Context, path []string, action string, in *ssmpb.Instance, v map[string]string) (bool, error) {
	parent := in.GetName()[:strings.Index(in.GetName(), "/instances/")]
	wait := func(fn func(c *developerconnect.Client) error) (bool, error) { return true, p.withDevConnect(ctx, fn) }
	switch {
	case len(path) == 1 && action == "create-connection":
		return wait(func(c *developerconnect.Client) error {
			op, err := c.CreateConnection(ctx, &dcpb.CreateConnectionRequest{Parent: parent, ConnectionId: strings.TrimSpace(v["id"]),
				Connection: &dcpb.Connection{ConnectionConfig: &dcpb.Connection_SecureSourceManagerInstanceConfig{
					SecureSourceManagerInstanceConfig: &dcpb.SecureSourceManagerInstanceConfig{Instance: in.GetName()}}}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		})
	case len(path) == 1 && action == "delete-connection":
		return wait(func(c *developerconnect.Client) error {
			op, err := c.DeleteConnection(ctx, &dcpb.DeleteConnectionRequest{Name: parent + "/connections/" + v["connection"]})
			if err != nil {
				return err
			}
			return op.Wait(ctx)
		})
	case len(path) == 2 && action == "create-link":
		return wait(func(c *developerconnect.Client) error {
			op, err := c.CreateGitRepositoryLink(ctx, &dcpb.CreateGitRepositoryLinkRequest{Parent: parent + "/connections/" + v["connection"],
				GitRepositoryLinkId: strings.TrimSpace(v["id"]),
				GitRepositoryLink:   &dcpb.GitRepositoryLink{CloneUri: runadapter.SourceManagerCloneURI(parent + "/repositories/" + path[1])}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		})
	case len(path) == 2 && action == "delete-link":
		return wait(func(c *developerconnect.Client) error {
			op, err := c.DeleteGitRepositoryLink(ctx, &dcpb.DeleteGitRepositoryLinkRequest{Name: v["link"]})
			if err != nil {
				return err
			}
			return op.Wait(ctx)
		})
	}
	return false, nil
}
