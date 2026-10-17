package main

// Custom domains on a Cloud Run service's page (#1450), through Google's
// generated v1 client (google.golang.org/api/run/v1), the API gcloud beta
// run domain-mappings and Terraform's google_cloud_run_domain_mapping call.
// The Domain mappings tab lists the domains mapped to the service, with the
// DNS records Google documents and each condition and its reason; Map a
// domain and Remove domain mapping create and delete one. The instance's
// gateway routes a mapped domain (send it as the Host header); no public DNS
// is served and no certificate issued, which the conditions say.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/api/option"
	runv1 "google.golang.org/api/run/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

const (
	runMapDomain   = "mapdomain"
	runUnmapDomain = "unmapdomain"
	runDomainsNote = "This instance's Cloud Run gateway routes a mapped domain: send a request to the gateway with the domain as its Host header. " +
		"CloudBurrow serves no public DNS, so the records are not in place, and issues no certificate, so a mapping with certificate mode AUTOMATIC ends " +
		"Ready False (CertificateNotIssued) while NONE becomes Ready."
)

// runV1 is the v1 client against the adapter.
func (p runProvider) runV1(ctx context.Context) (*runv1.APIService, error) {
	if p.runEndpoint() == "" {
		return nil, errors.New("the Cloud Run adapter is not running")
	}
	return runv1.NewService(ctx, option.WithEndpoint("http://"+p.runEndpoint()+"/"), option.WithoutAuthentication())
}

func (p runProvider) projectOr(project string) string {
	if project == "" {
		return p.defaultProject
	}
	return project
}

// domainsSection is a service's Domain mappings tab.
func (p runProvider) domainsSection(ctx context.Context, project, service string) console.Section {
	sec := console.Section{ID: "domains", Label: "Domain mappings", Listing: console.Listing{
		Columns: []string{"Ready", "Certificate", "Routable", "Certificate mode", "DNS records", "Detail"}, NameColumn: "Domain",
		Noun: "domain mappings", Items: []console.Resource{}, Note: runDomainsNote,
	}}
	c, err := p.runV1(ctx)
	if err != nil {
		sec.Listing.Unavailable = err.Error()
		return sec
	}
	l, err := c.Projects.Locations.Domainmappings.List("projects/" + p.projectOr(project) + "/locations/" + p.location()).Context(ctx).Do()
	if err != nil {
		sec.Listing.Unavailable = "cannot list the domain mappings: " + err.Error()
		return sec
	}
	for _, dm := range l.Items {
		if dm.Spec == nil || dm.Spec.RouteName != service {
			continue
		}
		sec.Listing.Items = append(sec.Listing.Items, domainRow(dm))
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// domainRow is one mapping: each condition's status, the records, and the
// reason the mapping is not Ready, when it is not.
func domainRow(dm *runv1.DomainMapping) console.Resource {
	cond := map[string]*runv1.GoogleCloudRunV1Condition{}
	if dm.Status != nil {
		for _, c := range dm.Status.Conditions {
			cond[c.Type] = c
		}
	}
	status := func(typ string) string {
		c := cond[typ]
		if c == nil {
			return "-"
		}
		if c.Reason != "" {
			return c.Status + " (" + c.Reason + ")"
		}
		return c.Status
	}
	var records []string
	if dm.Status != nil {
		for _, r := range dm.Status.ResourceRecords {
			name := r.Name
			if name == "" {
				name = "@"
			}
			records = append(records, name+" "+r.Type+" "+r.Rrdata)
		}
	}
	detail := "-"
	if c := cond["Ready"]; c != nil && c.Message != "" {
		detail = c.Message
	} else if c := cond["DomainRoutable"]; c != nil && c.Message != "" {
		detail = c.Message
	}
	return console.Resource{Name: dm.Metadata.Name, Fields: map[string]string{
		"Ready": status("Ready"), "Certificate": status("CertificateProvisioned"), "Routable": status("DomainRoutable"),
		"Certificate mode": dm.Spec.CertificateMode, "DNS records": orDash(strings.Join(records, "; ")), "Detail": detail,
	}}
}

// domainActions are a service page's Map a domain and Remove domain
// mapping, on a service CloudBurrow owns, which the v1 API maps.
func (p runProvider) domainActions(svc *ksvcStatus) []console.Action {
	if p.runEndpoint() == "" || svc == nil || svc.Metadata.Labels[k8s.OwnedLabel] != k8s.OwnedValue {
		return nil
	}
	return []console.Action{
		{ID: runMapDomain, Label: "Map a domain", Fields: []console.Field{
			{Name: "domain", Label: "Domain", Type: "text", Required: true, Pattern: `([a-z0-9]([a-z0-9\-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}`,
				Help: "Such as www.example.com. Ownership is not checked, as there is no Search Console locally."},
			{Name: "certificateMode", Label: "Certificate mode", Type: "select", Default: "AUTOMATIC", Options: []string{"AUTOMATIC", "NONE"},
				Help: "AUTOMATIC asks for a managed certificate, which is never issued locally; NONE serves the domain without one."},
		}},
		{ID: runUnmapDomain, Label: "Remove domain mapping", Fields: []console.Field{
			{Name: "domain", Label: "Domain", Type: "text", Required: true, Help: "A domain on the Domain mappings tab."},
		}},
	}
}

// actOnDomains performs domainActions on a service's page.
func (p runProvider) actOnDomains(ctx context.Context, project, service, action string, values map[string]string) error {
	c, err := p.runV1(ctx)
	if err != nil {
		return err
	}
	project = p.projectOr(project)
	if project == "" {
		return errors.New("choose a project first")
	}
	domain := strings.ToLower(strings.TrimSpace(values["domain"]))
	if domain == "" {
		return errors.New("a domain is required")
	}
	ns := "namespaces/" + project
	switch action {
	case runMapDomain:
		_, err = c.Namespaces.Domainmappings.Create(ns, &runv1.DomainMapping{ApiVersion: "domains.cloudrun.com/v1", Kind: "DomainMapping",
			Metadata: &runv1.ObjectMeta{Name: domain, Namespace: project, Labels: map[string]string{"cloud.googleapis.com/location": p.location()}},
			Spec:     &runv1.DomainMappingSpec{RouteName: service, CertificateMode: values["certificateMode"]}}).Context(ctx).Do()
		return err
	case runUnmapDomain:
		dm, err := c.Namespaces.Domainmappings.Get(ns + "/domainmappings/" + domain).Context(ctx).Do()
		if err != nil {
			return err
		}
		if dm.Spec == nil || dm.Spec.RouteName != service {
			return fmt.Errorf("%s is not mapped to %s", domain, service)
		}
		_, err = c.Namespaces.Domainmappings.Delete(ns + "/domainmappings/" + domain).Context(ctx).Do()
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// serviceIDOf is a service's ID, from its ID or its full name.
func serviceIDOf(name string) string {
	if _, id, ok := strings.Cut(name, "/services/"); ok {
		return id
	}
	return name
}
