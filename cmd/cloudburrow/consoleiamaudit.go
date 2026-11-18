package main

// IAM & Admin > Audit Logs (#1756): the services that can opt into Data
// Access audit logs, as Google's Audit Logs page lists them, from the IAM
// API's iamPolicies.queryAuditableServices of the selected project, each
// with the log types the project's allow policy enables for it (its
// auditConfigs, and allServices' for every service). Stored, as the
// policy's audit configs are: no audit log is written or withheld because
// of them (#1486). The log types are set through the project policy's
// SetIamPolicy (gcloud projects set-iam-policy, Terraform's
// google_project_iam_audit_config).
//
// The IAM and Data Access log type of the organization's and a folder's
// policy, which Google's page also offers when one is picked in the
// resource selector, are not shown here: this page lists the toolbar's
// project.

import (
	"context"
	"slices"
	"strings"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/type/expr"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

type iamAuditProvider struct {
	registry *resourcemanager.Registry
}

func (iamAuditProvider) ID() string    { return "iam-audit" }
func (iamAuditProvider) Title() string { return "Audit Logs" }

const iamAuditNote = "The services that can write Data Access audit logs, from the IAM API's queryAuditableServices, with the log types " +
	"the project's allow policy enables for each (its auditConfigs; allServices applies to every service). " +
	"The configuration is stored and returned, never acted on: no audit log is written or withheld because of it."

var auditLogTypes = []struct{ column, logType string }{
	{"Admin Read", "ADMIN_READ"}, {"Data Read", "DATA_READ"}, {"Data Write", "DATA_WRITE"},
}

func (p iamAuditProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Service", Columns: []string{"Admin Read", "Data Read", "Data Write", "Exempted principals"},
		Noun: "services", Note: iamAuditNote}
	if project == "" {
		l.Unavailable = "Choose a project in the toolbar."
		return l, nil
	}
	res, err := resourcemanager.NewIAMAdminServer(p.registry).QueryAuditableServices(ctx, &adminpb.QueryAuditableServicesRequest{
		FullResourceName: "//cloudresourcemanager.googleapis.com/projects/" + project})
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	pol, err := resourcemanager.NewProjectsServer(p.registry).GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resourcemanager.V3Name(project)})
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	configs := map[string]*iampb.AuditConfig{}
	for _, c := range pol.GetAuditConfigs() {
		configs[c.GetService()] = c
	}
	for _, s := range res.GetServices() {
		fields := map[string]string{}
		var exempt []string
		for _, t := range auditLogTypes {
			on := false
			for _, c := range []*iampb.AuditConfig{configs["allServices"], configs[s.GetName()]} {
				for _, lc := range c.GetAuditLogConfigs() {
					if lc.GetLogType().String() == t.logType {
						on = true
						for _, m := range lc.GetExemptedMembers() {
							if !slices.Contains(exempt, m) {
								exempt = append(exempt, m)
							}
						}
					}
				}
			}
			fields[t.column] = "-"
			if on {
				fields[t.column] = "Enabled"
			}
		}
		fields["Exempted principals"] = "-"
		if len(exempt) > 0 {
			fields["Exempted principals"] = strings.Join(exempt, ", ")
		}
		l.Items = append(l.Items, console.Resource{Name: s.GetName(), Fields: fields})
	}
	l.Total = len(l.Items)
	return l, nil
}

// iamLinter is the console's Run Linter (#1756): the IAM API's LintPolicy
// on the same server the gRPC and JSON transports serve.
type iamLinter struct {
	registry *resourcemanager.Registry
}

func (l iamLinter) LintCondition(ctx context.Context, resource, title, expression string) ([]console.LintResult, error) {
	res, err := resourcemanager.NewIAMAdminServer(l.registry).LintPolicy(ctx, &adminpb.LintPolicyRequest{FullResourceName: resource,
		LintObject: &adminpb.LintPolicyRequest_Condition{Condition: &expr.Expr{Title: title, Expression: expression}}})
	if err != nil {
		return nil, err
	}
	var out []console.LintResult
	for _, r := range res.GetLintResults() {
		out = append(out, console.LintResult{Severity: r.GetSeverity().String(), Unit: r.GetValidationUnitName(),
			Message: r.GetDebugMessage(), Position: int(r.GetLocationOffset())})
	}
	return out, nil
}
