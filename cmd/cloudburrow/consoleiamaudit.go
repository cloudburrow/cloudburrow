package main

// IAM & Admin > Audit Logs (#1756): the services that can opt into Data
// Access audit logs, as Google's Audit Logs page lists them, from the IAM
// API's iamPolicies.queryAuditableServices of the resource picked in the
// page's resource selector (the toolbar's project, a folder or the
// organization), each with the log types that resource's allow policy
// enables for it (its auditConfigs, and allServices' for every service).
//
// Each service's log types and exempted principals are edited on its row,
// and the default configuration (allServices) with Set default
// configuration, as on Google's page; each is saved through the resource's
// SetIamPolicy with the update mask auditConfigs, so the policy's bindings
// are left as they are. Stored, as the policy's audit configs are: no audit
// log is written or withheld because of them (#1486).

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

type iamAuditProvider struct {
	registry *resourcemanager.Registry
}

func (iamAuditProvider) ID() string    { return "iam-audit" }
func (iamAuditProvider) Title() string { return "Audit Logs" }

// iamAuditScopeKey is the query parameter the resource selector's choice
// travels in: projects/ID, folders/NUMBER or organizations/NUMBER.
const iamAuditScopeKey = "auditResource"

// ScopeKey implements console.Scoper.
func (iamAuditProvider) ScopeKey() string { return iamAuditScopeKey }

const iamAuditNote = "The services that can write Data Access audit logs, from the IAM API's queryAuditableServices, with the log types " +
	"the selected resource's allow policy enables for each (its auditConfigs; allServices, the default configuration, applies to every service). " +
	"Edit log types and Set Default Configuration save through SetIamPolicy with the update mask auditConfigs. " +
	"The configuration is stored and returned, never acted on: no audit log is written or withheld because of it."

// allServices is the audit config that applies to every service: the
// page's default configuration.
const allServices = "allServices"

var auditLogTypes = []struct{ column, logType, field string }{
	{"Admin Read", "ADMIN_READ", "adminRead"}, {"Data Read", "DATA_READ", "dataRead"}, {"Data Write", "DATA_WRITE", "dataWrite"},
}

const (
	auditEditAction    = "edit-audit-config"
	auditDefaultAction = "set-default-audit-config"
)

// auditResource is the resource the page addresses: the selector's choice,
// or the toolbar's project.
func auditResource(ctx context.Context, project string) (string, error) {
	v := console.ScopeValue(ctx, iamAuditScopeKey)
	switch {
	case v == "" && project == "":
		return "", errors.New("choose a project in the toolbar, or a folder or the organization in the resource selector")
	case v == "":
		return "projects/" + project, nil
	case v == resourcemanager.OrganizationName, strings.HasPrefix(v, "projects/") && strings.Count(v, "/") == 1:
		return v, nil
	case strings.HasPrefix(v, "folders/"):
		return resourcemanager.FolderName(v)
	}
	return "", fmt.Errorf("%s is not a project, a folder or the organization", v)
}

// auditPolicyAPI is the API whose policy resource holds its audit configs,
// and the name it takes there.
func (p iamAuditProvider) auditPolicyAPI(resource string) (iamServer, string) {
	switch {
	case strings.HasPrefix(resource, "folders/"):
		return resourcemanager.NewFoldersServer(p.registry), resource
	case strings.HasPrefix(resource, "organizations/"):
		return resourcemanager.NewOrganizationsServer(p.registry), resource
	}
	return resourcemanager.NewProjectsServer(p.registry), resourcemanager.V3Name(strings.TrimPrefix(resource, "projects/"))
}

func (p iamAuditProvider) auditPolicy(ctx context.Context, resource string) (*iampb.Policy, error) {
	api, name := p.auditPolicyAPI(resource)
	return api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name,
		Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
}

// scopeOptions are the resource selector's choices: the toolbar's
// project, every other active project, the organization and every active
// folder.
func (p iamAuditProvider) scopeOptions(project string) []console.ScopeOption {
	var opts []console.ScopeOption
	if project != "" {
		opts = append(opts, console.ScopeOption{ID: "projects/" + project, Label: "Project " + project})
	}
	projects, _ := p.registry.List()
	for _, pr := range projects {
		if pr.State == resourcemanager.StateActive && pr.ProjectID != project {
			opts = append(opts, console.ScopeOption{ID: "projects/" + pr.ProjectID, Label: "Project " + pr.ProjectID})
		}
	}
	opts = append(opts, console.ScopeOption{ID: resourcemanager.OrganizationName, Label: "Organization (" + resourcemanager.OrganizationName + ")"})
	folders, _ := p.registry.Folders()
	for _, f := range folders {
		if f.State == resourcemanager.StateActive {
			opts = append(opts, console.ScopeOption{ID: f.Name, Label: "Folder " + f.DisplayName + " (" + f.Name + ")"})
		}
	}
	return opts
}

func (p iamAuditProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Service", Columns: []string{"Admin Read", "Data Read", "Data Write", "Exempted principals"},
		Noun: "services", Note: iamAuditNote, TableTitle: "Data Access audit logs configuration"}
	resource, err := auditResource(ctx, project)
	sc := &console.Scope{Key: iamAuditScopeKey, Label: "Resource", Selected: resource, Options: p.scopeOptions(project)}
	if project != "" {
		sc.Default = "projects/" + project
	}
	if resource != "" && !slices.ContainsFunc(sc.Options, func(o console.ScopeOption) bool { return o.ID == resource }) {
		sc.Options = append(sc.Options, console.ScopeOption{ID: resource})
	}
	l.Scope = sc
	if err != nil {
		l.Unavailable = err.Error()
		return l, nil
	}
	res, err := resourcemanager.NewIAMAdminServer(p.registry).QueryAuditableServices(ctx, &adminpb.QueryAuditableServicesRequest{
		FullResourceName: "//cloudresourcemanager.googleapis.com/" + resource})
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	pol, err := p.auditPolicy(ctx, resource)
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	configs := auditConfigsByService(pol)
	etag := base64.StdEncoding.EncodeToString(pol.GetEtag())
	for _, s := range res.GetServices() {
		fields := map[string]string{}
		var exempt []string
		for _, t := range auditLogTypes {
			on := false
			for _, c := range []*iampb.AuditConfig{configs[allServices], configs[s.GetName()]} {
				if lc := logConfig(c, t.logType); lc != nil {
					on = true
					for _, m := range lc.GetExemptedMembers() {
						if !slices.Contains(exempt, m) {
							exempt = append(exempt, m)
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
		l.Items = append(l.Items, console.Resource{Name: s.GetName(), Fields: fields, Target: []string{s.GetName()},
			Actions: []console.Action{auditConfigAction(auditEditAction, "Edit log types", s.GetName(), configs[s.GetName()], etag)}})
	}
	l.Total = len(l.Items)
	for _, a := range p.ListActions(ctx, project) {
		if a.ID == auditSelectAction {
			l.SelectActions = []console.Action{a}
		}
	}
	return l, nil
}

func auditConfigsByService(pol *iampb.Policy) map[string]*iampb.AuditConfig {
	configs := map[string]*iampb.AuditConfig{}
	for _, c := range pol.GetAuditConfigs() {
		configs[c.GetService()] = c
	}
	return configs
}

// logConfig is c's config of logType, or nil when c does not enable it.
func logConfig(c *iampb.AuditConfig, logType string) *iampb.AuditLogConfig {
	for _, lc := range c.GetAuditLogConfigs() {
		if lc.GetLogType().String() == logType {
			return lc
		}
	}
	return nil
}

// auditConfigAction is the form that edits service's audit config, drawn
// from c, its config in the policy the page read (nil: none), with that
// policy's etag, so a policy changed since is refused.
func auditConfigAction(id, label, service string, c *iampb.AuditConfig, etag string) console.Action {
	var fields []console.Field
	for _, t := range auditLogTypes {
		lc := logConfig(c, t.logType)
		on := "false"
		if lc != nil {
			on = "true"
		}
		fields = append(fields,
			console.Field{Name: t.field, Label: t.column, Type: "checkbox", Default: on, Section: "Log Types",
				Help: t.column + " audit logs for " + service + "."},
			console.Field{Name: t.field + "Exempted", Label: t.column + ": exempted principals", Type: "textarea", Section: "Exempted Principals",
				Default: strings.Join(lc.GetExemptedMembers(), "\n"),
				Help:    "Principals whose " + t.column + " accesses are not logged, one per line or separated by commas, such as user:ada@example.com. Only for a log type that is enabled."})
	}
	fields = append(fields, console.Field{Name: "etag", Type: "hidden", Default: etag})
	// Each of Google's audit log forms ends in Save
	// (docs.cloud.google.com/logging/docs/audit/configure-data-access, #1864).
	return console.Action{ID: id, Label: label, Fields: fields, Submit: "Save"}
}

// DetailActions implements console.PathActor: a service row's Edit log
// types.
func (p iamAuditProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 || path[0] == allServices {
		return nil
	}
	resource, err := auditResource(ctx, project)
	if err != nil {
		return nil
	}
	pol, err := p.auditPolicy(ctx, resource)
	if err != nil {
		return nil
	}
	return []console.Action{auditConfigAction(auditEditAction, "Edit log types", path[0], auditConfigsByService(pol)[path[0]],
		base64.StdEncoding.EncodeToString(pol.GetEtag()))}
}

// ListActions implements console.ListActor: Set Default Configuration.
func (p iamAuditProvider) ListActions(ctx context.Context, project string) []console.Action {
	resource, err := auditResource(ctx, project)
	if err != nil {
		return nil
	}
	pol, err := p.auditPolicy(ctx, resource)
	if err != nil {
		return nil
	}
	etag := base64.StdEncoding.EncodeToString(pol.GetEtag())
	// Google's page configures the services checked in its table together
	// (#1864): the selection's Edit selected services, its services written
	// into Services.
	sel := auditConfigAction(auditSelectAction, "Edit selected services", "the selected services", nil, etag)
	sel.SelectionField = "services"
	sel.Fields = append([]console.Field{{Name: "services", Label: "Services", Type: "textarea", Required: true,
		Help: "The services checked in the table, one per line. Each gets the log types and exempted principals below."}}, sel.Fields...)
	return []console.Action{auditConfigAction(auditDefaultAction, "Set Default Configuration", allServices, auditConfigsByService(pol)[allServices], etag), sel}
}

const auditSelectAction = "edit-selected"

// ActAt implements console.PathActor: it replaces one service's audit
// config (allServices' for the default configuration) in the resource's
// policy, through SetIamPolicy with the update mask auditConfigs.
func (p iamAuditProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	var services []string
	switch {
	case action == auditEditAction && len(path) == 1 && path[0] != allServices:
		services = []string{path[0]}
	case action == auditDefaultAction && len(path) == 0:
		services = []string{allServices}
	case action == auditSelectAction && len(path) == 0:
		for _, s := range strings.Fields(values["services"]) {
			if s != allServices && !slices.Contains(services, s) {
				services = append(services, s)
			}
		}
		if len(services) == 0 {
			return errors.New("select at least one service")
		}
	default:
		return fmt.Errorf("%s is not an action of the Audit Logs page", action)
	}
	resource, err := auditResource(ctx, project)
	if err != nil {
		return err
	}
	etag, err := base64.StdEncoding.DecodeString(values["etag"])
	if err != nil || len(etag) == 0 {
		return errors.New("the form carries no policy etag this console read: reload the page")
	}
	var logConfigs []*iampb.AuditLogConfig
	for _, t := range auditLogTypes {
		exempt := splitPrincipals(values[t.field+"Exempted"])
		if values[t.field] != "true" {
			if len(exempt) > 0 {
				return fmt.Errorf("%s exempts principals from %s, which is not enabled: enable it, or clear its exempted principals", strings.Join(services, ", "), t.column)
			}
			continue
		}
		logConfigs = append(logConfigs, &iampb.AuditLogConfig{
			LogType: iampb.AuditLogConfig_LogType(iampb.AuditLogConfig_LogType_value[t.logType]), ExemptedMembers: exempt})
	}
	pol, err := p.auditPolicy(ctx, resource)
	if err != nil {
		return err
	}
	configs := pol.GetAuditConfigs()
	for _, service := range services {
		next := &iampb.AuditConfig{Service: service, AuditLogConfigs: logConfigs}
		var kept []*iampb.AuditConfig
		replaced := false
		for _, c := range configs {
			if c.GetService() != service {
				kept = append(kept, c)
				continue
			}
			replaced = true
			if len(next.AuditLogConfigs) > 0 {
				kept = append(kept, next)
			}
		}
		if !replaced && len(next.AuditLogConfigs) > 0 {
			kept = append(kept, next)
		}
		configs = kept
	}
	api, name := p.auditPolicyAPI(resource)
	// The page's etag, so a policy changed since it was read is refused;
	// only auditConfigs is in the mask, so the bindings stay as stored.
	_, err = api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name,
		Policy:     &iampb.Policy{Version: 3, Etag: etag, AuditConfigs: configs},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"audit_configs"}}})
	return err
}

// splitPrincipals is a textarea's principals, separated by commas or new
// lines.
func splitPrincipals(v string) []string {
	var out []string
	for _, m := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if m = strings.TrimSpace(m); m != "" && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

var (
	_ console.Scoper       = iamAuditProvider{}
	_ console.ListActor    = iamAuditProvider{}
	_ console.PathActor    = iamAuditProvider{}
	_ console.PolicyLinter = iamLinter{}
)

// iamLinter is the console's Run Linter (#1756): the IAM API's LintPolicy
// on the same server the gRPC and JSON transports serve, given the full
// resource name of the policy the condition is in.
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
