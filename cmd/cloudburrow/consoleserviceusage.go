package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/serviceusage"
)

// serviceUsageProvider is the APIs & Services page (#1352): a project's
// APIs and their state, read and changed through the same
// serviceusage.Server the Service Usage API serves, so an API disabled here
// is one gcloud and Terraform see DISABLED.
type serviceUsageProvider struct{ api *serviceusage.Server }

func (serviceUsageProvider) ID() string    { return "serviceusage" }
func (serviceUsageProvider) Title() string { return "APIs & Services" }

const serviceUsageNote = "Every API this instance serves, as Service Usage lists it. " +
	"Disabling one is recorded and reported, not enforced: the API keeps answering (docs/serviceusage.md)."

// List implements console.Provider.
func (p serviceUsageProvider) List(_ context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Title"}, NameColumn: "Service", Noun: "APIs", Note: serviceUsageNote}
	if project == "" {
		l.Prompt = "Service Usage lists APIs per project. Choose one in the toolbar."
		return l, nil
	}
	services, err := p.api.Services(project)
	if err != nil {
		return console.Listing{}, err
	}
	for _, s := range services {
		cfg, _ := s["config"].(map[string]any)
		name, _ := cfg["name"].(string)
		title, _ := cfg["title"].(string)
		state, _ := s["state"].(string)
		r := console.Resource{Name: name, Status: state, Fields: map[string]string{"Title": title}}
		r.Actions = p.Actions(r)
		l.Items = append(l.Items, r)
	}
	l.Total = len(l.Items)
	return l, nil
}

// Actions implements console.Actor: Enable on a disabled API, Disable on an
// enabled one.
func (serviceUsageProvider) Actions(r console.Resource) []console.Action {
	if r.Status == "DISABLED" {
		return []console.Action{{ID: "enable", Label: "Enable"}}
	}
	return []console.Action{{ID: "disable", Label: "Disable",
		Confirm: "The API is recorded as disabled for this project. CloudBurrow keeps serving it."}}
}

// Act implements console.Actor.
func (p serviceUsageProvider) Act(_ context.Context, project, name, action string) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	switch action {
	case "enable":
		return p.api.SetState(project, name, true)
	case "disable":
		return p.api.SetState(project, name, false)
	}
	return fmt.Errorf("unknown action %q", action)
}

// serviceUsageOperationsProvider is the Service Usage operations page
// (#1530): the operations a project's enables and disables returned, as
// operations.list answers them, with Delete (operations.delete). Every
// operation is done when it is returned, so there is no Cancel: cancelling
// a done operation changes nothing.
type serviceUsageOperationsProvider struct{ api *serviceusage.Server }

func (serviceUsageOperationsProvider) ID() string    { return "serviceusage-operations" }
func (serviceUsageOperationsProvider) Title() string { return "Service Usage operations" }

const serviceUsageOperationsNote = "The operations this project's API enables and disables returned, oldest first. " +
	"Each is done when it is returned; Delete forgets the operation, not the change it made. Operations are kept in memory."

// List implements console.Provider.
func (p serviceUsageOperationsProvider) List(_ context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Services", "Result"}, NameColumn: "Operation", Noun: "operations",
		Note: serviceUsageOperationsNote}
	if project == "" {
		l.Prompt = "Service Usage operations are listed per project. Choose one in the toolbar."
		return l, nil
	}
	for _, op := range p.api.ProjectOperations(project) {
		name, _ := op["name"].(string)
		md, _ := op["metadata"].(map[string]any)
		names, _ := md["resourceNames"].([]string)
		var services []string
		for _, n := range names {
			if _, svc, ok := strings.Cut(n, "/services/"); ok {
				services = append(services, svc)
			}
		}
		result := ""
		if resp, ok := op["response"].(map[string]any); ok {
			t, _ := resp["@type"].(string)
			result = t[strings.LastIndex(t, ".")+1:]
		}
		// The row is named by the operation's ID, without "operations/",
		// so its name is one path segment.
		r := console.Resource{Name: strings.TrimPrefix(name, "operations/"), Status: "DONE",
			Fields: map[string]string{"Services": strings.Join(services, ", "), "Result": result}}
		r.Actions = p.Actions(r)
		l.Items = append(l.Items, r)
	}
	l.Total = len(l.Items)
	return l, nil
}

// Actions implements console.Actor.
func (serviceUsageOperationsProvider) Actions(console.Resource) []console.Action {
	return []console.Action{{ID: "delete", Label: "Delete",
		Confirm: "The operation is forgotten: operations.get no longer answers it. The change it made stays."}}
}

// Act implements console.Actor.
func (p serviceUsageOperationsProvider) Act(_ context.Context, project, name, action string) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	if action != "delete" {
		return fmt.Errorf("unknown action %q", action)
	}
	return p.api.DeleteOperation("operations/" + name)
}
