package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// runDomainsProvider is Cloud Run's Domain mappings page (#1852), as
// Google's /run/domains is: every mapping of the project, Add mapping (a
// service, then the domain), each mapping's page with its DNS records, and
// Delete. It maps through the Cloud Run Admin API v1, as the service page's
// Domain mappings tab does.
type runDomainsProvider struct{ run runProvider }

const runAddMapping = "add-mapping"

func (runDomainsProvider) ID() string    { return "run-domains" }
func (runDomainsProvider) Title() string { return "Domain mappings" }

func (p runDomainsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{
		Columns:    []string{"Mapped to", "Ready", "Certificate", "Routable", "Certificate mode", "DNS records", "Detail"},
		NameColumn: "Domain", Noun: "domain mappings", Items: []console.Resource{}, AlwaysStatus: true,
		RowsOpenable: true, Note: runDomainsNote,
	}
	c, err := p.run.runV1(ctx)
	if err != nil {
		l.Unavailable = err.Error()
		return l, nil
	}
	got, err := c.Projects.Locations.Domainmappings.List("projects/" + p.run.projectOr(project) + "/locations/" + p.run.location()).Context(ctx).Do()
	if err != nil {
		l.Unavailable = "cannot list the domain mappings: " + err.Error()
		return l, nil
	}
	for _, dm := range got.Items {
		r := domainRow(dm)
		r.Fields["Mapped to"] = "-"
		if dm.Spec != nil {
			r.Fields["Mapped to"] = dm.Spec.RouteName
		}
		r.Status = "Pending"
		if dm.Status != nil {
			for _, c := range dm.Status.Conditions {
				if c.Type == "Ready" {
					r.Status = map[string]string{"True": "Ready", "False": "Failed"}[c.Status]
					if r.Status == "" {
						r.Status = "Pending"
					}
				}
			}
		}
		l.Items = append(l.Items, r)
	}
	l.Total = len(l.Items)
	return l, nil
}

// Detail is one mapping's page: what it maps, and its DNS records, which
// Google's row menu shows as DNS Records.
func (p runDomainsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.run.runV1(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	dm, err := c.Namespaces.Domainmappings.Get("namespaces/" + p.run.projectOr(project) + "/domainmappings/" + path[0]).Context(ctx).Do()
	if err != nil {
		return console.Detail{Unavailable: "cannot read the domain mapping: " + err.Error()}, nil
	}
	row := domainRow(dm)
	summary := []console.Property{{Label: "Domain", Value: path[0]}}
	if dm.Spec != nil {
		summary = append(summary, console.Property{Label: "Mapped to", Value: dm.Spec.RouteName,
			Link: "/run/" + dm.Spec.RouteName}, console.Property{Label: "Certificate mode", Value: orDash(dm.Spec.CertificateMode)})
	}
	for _, k := range []string{"Ready", "Certificate", "Routable", "Detail"} {
		summary = append(summary, console.Property{Label: k, Value: row.Fields[k]})
	}
	records := console.Listing{Columns: []string{"Type", "Data"}, NameColumn: "Name", Noun: "DNS records", Items: []console.Resource{}}
	if dm.Status != nil {
		for _, r := range dm.Status.ResourceRecords {
			name := r.Name
			if name == "" {
				name = "@"
			}
			records.Items = append(records.Items, console.Resource{Name: name, Fields: map[string]string{"Type": r.Type, "Data": r.Rrdata}})
		}
	}
	records.Total = len(records.Items)
	return console.Detail{Summary: summary, Sections: []console.Section{{ID: "dns", Label: "DNS records", Listing: records,
		Note: "The records to add at your domain's DNS provider. No public DNS is served locally: the instance's gateway routes the domain."}}}, nil
}

// ListActions implements console.ListActor: Add mapping, with the
// project's services to choose from.
func (p runDomainsProvider) ListActions(ctx context.Context, project string) []console.Action {
	if p.run.runEndpoint() == "" {
		return nil
	}
	svcs, err := p.run.services(ctx, project)
	if err != nil || len(svcs) == 0 {
		return nil
	}
	names := make([]string, 0, len(svcs))
	for _, s := range svcs {
		names = append(names, s.Metadata.Name)
	}
	fields := append([]console.Field{{Name: "service", Label: "Select a service to map to", Type: "select", Options: names,
		Default: names[0], Required: true}}, runMapDomainFields()...)
	return []console.Action{{ID: runAddMapping, Label: "Add mapping", Fields: fields}}
}

// DetailActions implements console.PathActor: a mapping page offers nothing
// of its own beyond Delete.
func (runDomainsProvider) DetailActions(context.Context, string, []string) []console.Action {
	return nil
}

// ActAt performs Add mapping from the list screen (no path).
func (p runDomainsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if action != runAddMapping || len(path) != 0 {
		return fmt.Errorf("unknown action %q", action)
	}
	service := strings.TrimSpace(values["service"])
	if service == "" {
		return errors.New("select a service to map to")
	}
	svc, err := p.run.service(ctx, project, service)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %s", service)
	}
	return p.run.actOnDomains(ctx, project, service, runMapDomain, values)
}

// Delete removes a mapping.
func (p runDomainsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.run.runV1(ctx)
	if err != nil {
		return err
	}
	_, err = c.Namespaces.Domainmappings.Delete("namespaces/" + p.run.projectOr(project) + "/domainmappings/" + name).Context(ctx).Do()
	return err
}

var (
	_ console.Driller   = runDomainsProvider{}
	_ console.ListActor = runDomainsProvider{}
	_ console.Deleter   = runDomainsProvider{}
)
