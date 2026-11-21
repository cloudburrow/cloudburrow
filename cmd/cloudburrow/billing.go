package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	billingpb "cloud.google.com/go/billing/apiv1/billingpb"
	budgetspb "cloud.google.com/go/billing/budgets/apiv1/budgetspb"

	"github.com/cloudburrow/cloudburrow/internal/service/billing"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/serviceusage"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// isCloudBillingPath matches the Cloud Billing API v1 (#2109) on the
// Resource Manager port: billingAccounts (and their subAccounts, projects
// and IAM methods), organizations/{o}/billingAccounts,
// projects/{p}/billingInfo, and the Cloud Catalog's services and skus.
// Budgets are the Budget API's (isBudgetsPath).
func isCloudBillingPath(r *http.Request) bool {
	p, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	if !ok || isBudgetsPath(r) {
		return false
	}
	switch {
	case strings.HasPrefix(p, "billingAccounts"):
		return true
	case strings.HasPrefix(p, "organizations/") && strings.HasSuffix(p, "/billingAccounts"):
		return true
	case strings.HasPrefix(p, "projects/") && strings.HasSuffix(p, "/billingInfo"):
		return true
	case p == "services", strings.HasPrefix(p, "services/") && strings.HasSuffix(p, "/skus"):
		return true
	}
	return false
}

// isBudgetsPath matches the Budget API v1: billingAccounts/{a}/budgets...
func isBudgetsPath(r *http.Request) bool {
	p, ok := strings.CutPrefix(r.URL.Path, "/v1/billingAccounts/")
	if !ok {
		return false
	}
	_, rest, _ := strings.Cut(p, "/")
	return rest == "budgets" || strings.HasPrefix(rest, "budgets/")
}

// freeAPIs are the served APIs Google offers without a billing account, so
// unlinking a project's billing leaves them on: resource and API
// management, IAM, Cloud Billing itself, Cloud Logging and Monitoring
// (on in every new project), BigQuery (the sandbox needs no billing
// account) and Firestore with its rules (the no-cost Spark plan).
var freeAPIs = map[string]bool{
	"cloudresourcemanager.googleapis.com": true, "serviceusage.googleapis.com": true, "apikeys.googleapis.com": true,
	"cloudquotas.googleapis.com": true, "siteverification.googleapis.com": true, "iam.googleapis.com": true,
	"iamcredentials.googleapis.com": true, "cloudbilling.googleapis.com": true, "billingbudgets.googleapis.com": true,
	"logging.googleapis.com": true, "monitoring.googleapis.com": true, "observability.googleapis.com": true,
	"bigquery.googleapis.com": true, "bigquerystorage.googleapis.com": true,
	"firestore.googleapis.com": true, "firebaserules.googleapis.com": true, "datastore.googleapis.com": true,
}

// billingAPIs disables and re-enables a project's billable APIs through
// Service Usage when its billing is unlinked and linked again.
type billingAPIs struct{ su func() *serviceusage.Server }

func (b billingAPIs) DisableBillable(project string) ([]string, error) {
	s := b.su()
	if s == nil {
		return nil, nil
	}
	var out []string
	for _, a := range s.Catalog() {
		if freeAPIs[a.Name] || !s.Enabled(project, a.Name) {
			continue
		}
		if err := s.SetState(project, a.Name, false); err != nil {
			continue
		}
		out = append(out, a.Name)
	}
	return out, nil
}

func (b billingAPIs) Enable(project string, apis []string) error {
	s := b.su()
	if s == nil {
		return nil
	}
	for _, a := range apis {
		if err := s.SetState(project, a, true); err != nil {
			return err
		}
	}
	return nil
}

// billingProjects resolves a project the registry has.
type billingProjects struct{ reg *resourcemanager.Registry }

func (p billingProjects) Resolve(s string) (string, error) {
	id, err := serviceUsageProjects(p).Resolve(s)
	if err != nil {
		return "", err
	}
	if _, err := p.reg.Get(id); err != nil {
		return "", err
	}
	return id, nil
}

// serveBilling mounts the Cloud Billing API and the Budget API (#2109) on
// the Resource Manager port, in the registry's store.
func serveBilling(rm *resourcemanager.Server, reg *resourcemanager.Registry, su func() *serviceusage.Server) *billing.Server {
	s := billing.NewServer(reg.Backing(), billingProjects{reg: reg}, resourcemanager.OrganizationName)
	s.APIs = billingAPIs{su: su}
	catalog := &billing.Catalog{Since: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC), APIs: func() []billing.API {
		srv := su()
		var out []billing.API
		if srv != nil {
			for _, a := range srv.Catalog() {
				out = append(out, billing.API{Name: a.Name, Title: a.Title})
			}
		}
		return out
	}}
	rm.HandleService("google.cloud.billing.budgets.v1", isBudgetsPath,
		resourcemanager.Impl{Desc: &budgetspb.BudgetService_ServiceDesc, Impl: s.Budgets()})
	// google_billing_project_info's URL ends in a slash
	// (projects/{{project}}/billingInfo/), which Google accepts.
	slashed := &rest.Transcoder{Packages: []string{"google.cloud.billing.v1"}}
	slashed.RegisterService(&billingpb.CloudBilling_ServiceDesc, s)
	rm.HandleJSON(func(r *http.Request) bool {
		return strings.HasPrefix(r.URL.Path, "/v1/projects/") && strings.HasSuffix(r.URL.Path, "/billingInfo/")
	}, trimTrailingBillingSlash{t: slashed})
	rm.HandleService("google.cloud.billing.v1", isCloudBillingPath,
		resourcemanager.Impl{Desc: &billingpb.CloudBilling_ServiceDesc, Impl: s},
		resourcemanager.Impl{Desc: &billingpb.CloudCatalog_ServiceDesc, Impl: catalog})
	billingCatalogRef = catalog
	return s
}

// trimTrailingBillingSlash serves projects/{p}/billingInfo/ as
// projects/{p}/billingInfo, through Cloud Billing's transcoder.
type trimTrailingBillingSlash struct{ t *rest.Transcoder }

func (h trimTrailingBillingSlash) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r2 := r.Clone(r.Context())
	r2.URL.Path = strings.TrimSuffix(r.URL.Path, "/")
	r2.URL.RawPath = ""
	h.t.ServeHTTP(w, r2)
}

// billingCatalogRef is the instance's Cloud Catalog, for the console.
var billingCatalogRef *billing.Catalog

// billingResetter forgets the billing accounts, links and budgets.
type billingResetter struct{ srv func() *billing.Server }

func (r *billingResetter) Name() string { return "billing" }

func (r *billingResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *billingResetter) ResetProject(_ context.Context, project string) error {
	if s := r.srv(); s != nil {
		return s.ResetProject(project)
	}
	return nil
}
