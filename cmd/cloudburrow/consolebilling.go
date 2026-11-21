package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	billingpb "cloud.google.com/go/billing/apiv1/billingpb"
	budgetspb "cloud.google.com/go/billing/budgets/apiv1/budgetspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/type/money"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/billing"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// The console's Billing pages (#2109), in Google's Billing menu: Manage
// billing accounts (billingAccountsProvider), Account management, the
// projects and the accounts they are linked to (billingProjectsProvider),
// Budgets & alerts (billingBudgetsProvider) and Pricing
// (billingPricingProvider). Every account is a local record: nothing asks
// for, holds or shows payment details, and the spend shown is the real
// local spend, 0.

const billingNotStarted = "Cloud Billing has not started"

// billingConsole is what the Billing pages share.
type billingConsole struct {
	srv      *billing.Server
	catalog  *billing.Catalog
	projects *resourcemanager.Registry
}

func (b billingConsole) api() (*billing.Server, error) {
	if b.srv == nil {
		return nil, errors.New(billingNotStarted)
	}
	return b.srv, nil
}

func billingAccountID(name string) string { return strings.TrimPrefix(name, "billingAccounts/") }

func billingStatus(a *billingpb.BillingAccount) string {
	if a.GetOpen() {
		return "Active"
	}
	return "Closed"
}

func billingType(a *billingpb.BillingAccount) string {
	if a.GetMasterBillingAccount() != "" {
		return "Subaccount of " + billingAccountID(a.GetMasterBillingAccount())
	}
	return "Billing account"
}

// accountOptions are the accounts' names with their display names, for a
// select.
func (b billingConsole) accountOptions(openOnly bool) ([]string, map[string]string) {
	s, err := b.api()
	if err != nil {
		return nil, nil
	}
	all, _ := s.Accounts()
	var names []string
	labels := map[string]string{}
	for _, a := range all {
		if openOnly && !a.GetOpen() {
			continue
		}
		names = append(names, a.GetName())
		labels[a.GetName()] = a.GetDisplayName() + " (" + billingAccountID(a.GetName()) + ")"
	}
	return names, labels
}

func (b billingConsole) projectIDs() []string {
	if b.projects == nil {
		return nil
	}
	list, err := b.projects.Active()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, p.ProjectID)
	}
	sort.Strings(out)
	return out
}

// billingAccountsProvider is Manage billing accounts: every account, with
// Create account; an account's page shows its overview, linked projects,
// subaccounts, budgets and permissions.
type billingAccountsProvider struct{ billingConsole }

func (billingAccountsProvider) ID() string    { return "billing" }
func (billingAccountsProvider) Title() string { return "Manage billing accounts" }

func (p billingAccountsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := p.api()
	if err != nil {
		return console.Listing{}, err
	}
	all, err := s.Accounts()
	if err != nil {
		return console.Listing{}, err
	}
	l := console.Listing{Columns: []string{"Billing account ID", "Status", "Type", "Projects", "Organization"}, NameColumn: "Name",
		Noun: "billing accounts", RowsOpenable: true}
	for _, a := range all {
		linked, _ := s.Linked(a.GetName())
		org := a.GetParent()
		if a.GetMasterBillingAccount() != "" {
			if m, err := s.Account(a.GetMasterBillingAccount()); err == nil {
				org = m.GetParent()
			}
		}
		l.Items = append(l.Items, console.Resource{Name: a.GetName(), Status: billingStatus(a), Fields: map[string]string{
			"Name": a.GetDisplayName(), "Billing account ID": billingAccountID(a.GetName()), "Status": billingStatus(a),
			"Type": billingType(a), "Projects": strconv.Itoa(len(linked)), "Organization": org}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (p billingAccountsProvider) CreateForm() (string, []console.Field) {
	names, labels := p.accountOptions(true)
	var tops []string
	if s, err := p.api(); err == nil {
		for _, n := range names {
			if a, err := s.Account(n); err == nil && a.GetMasterBillingAccount() == "" {
				tops = append(tops, n)
			}
		}
	}
	return "Create account", []console.Field{
		{Name: "displayName", Label: "Name", Type: "text", Required: true, Help: "The billing account's display name."},
		{Name: "kind", Label: "Account type", Type: "radio", Default: "account", Options: []string{"account", "subaccount"},
			OptionLabels: map[string]string{"account": "Billing account", "subaccount": "Subaccount of an existing account"},
			Help:         "A local account holds no payment details: CloudBurrow charges nothing, and never contacts Google."},
		{Name: "parent", Label: "Parent billing account", Type: "select", ShowWhen: "kind=subaccount", Options: tops, OptionLabels: labels,
			Help: "A subaccount takes its parent's currency and organization."},
	}
}

func (p billingAccountsProvider) Create(ctx context.Context, _ string, values map[string]string) (string, error) {
	s, err := p.api()
	if err != nil {
		return "", err
	}
	dn := strings.TrimSpace(values["displayName"])
	if values["kind"] == "subaccount" {
		a, err := s.CreateBillingAccount(ctx, &billingpb.CreateBillingAccountRequest{Parent: values["parent"],
			BillingAccount: &billingpb.BillingAccount{DisplayName: dn}})
		if err != nil {
			return "", err
		}
		return a.GetName(), nil
	}
	a, err := s.CreateTopLevel(dn)
	if err != nil {
		return "", err
	}
	return a.GetName(), nil
}

func (p billingAccountsProvider) Detail(ctx context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	s, err := p.api()
	if err != nil {
		return console.Detail{Unavailable: billingNotStarted}, nil
	}
	a, err := s.Account(path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the billing account: " + consoleErr(err)}, nil
	}
	projects := console.Listing{Columns: []string{"Project ID", "Billing"}, NameColumn: "Project name", Noun: "projects"}
	linked, _ := s.Linked(a.GetName())
	for _, pi := range linked {
		state := "Enabled"
		if !pi.GetBillingEnabled() {
			state = "Disabled (the account is closed)"
		}
		projects.Items = append(projects.Items, console.Resource{Name: pi.GetProjectId(), Fields: map[string]string{
			"Project name": pi.GetProjectId(), "Project ID": pi.GetProjectId(), "Billing": state}})
	}
	projects.Total = len(projects.Items)
	subs := console.Listing{Columns: []string{"Billing account ID", "Status"}, NameColumn: "Name", Noun: "subaccounts"}
	if l, err := s.ListBillingAccounts(ctx, &billingpb.ListBillingAccountsRequest{Filter: "masterBillingAccount=" + a.GetName()}); err == nil {
		for _, sub := range l.GetBillingAccounts() {
			subs.Items = append(subs.Items, console.Resource{Name: sub.GetName(), Fields: map[string]string{"Name": sub.GetDisplayName(),
				"Billing account ID": billingAccountID(sub.GetName()), "Status": billingStatus(sub)}})
		}
	}
	subs.Total = len(subs.Items)
	budgets := budgetListing(s, []*billingpb.BillingAccount{a})
	perms := console.Listing{Columns: []string{"Role"}, NameColumn: "Principal", Noun: "principals"}
	if pol, err := s.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: a.GetName()}); err == nil {
		for _, bd := range pol.GetBindings() {
			for _, m := range bd.GetMembers() {
				perms.Items = append(perms.Items, console.Resource{Name: m, Fields: map[string]string{"Principal": m, "Role": bd.GetRole()}})
			}
		}
	}
	perms.Total = len(perms.Items)
	return console.Detail{
		Summary: []console.Property{{Label: "Billing account ID", Value: billingAccountID(a.GetName())}, {Label: "Status", Value: billingStatus(a)},
			{Label: "Currency", Value: a.GetCurrencyCode()}},
		Sections: []console.Section{
			{ID: "overview", Label: "Overview", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Billing account", Properties: []console.Property{
				{Label: "Name", Value: a.GetDisplayName()}, {Label: "Billing account ID", Value: billingAccountID(a.GetName())},
				{Label: "Type", Value: billingType(a)}, {Label: "Parent", Value: a.GetParent()}, {Label: "Currency", Value: a.GetCurrencyCode()},
				{Label: "Status", Value: billingStatus(a)},
				{Label: "Costs this month", Value: "0.00 " + a.GetCurrencyCode() + " (CloudBurrow is local: usage costs nothing)"},
				{Label: "Payment", Value: "None: a local account holds no payment details"}}}}},
			{ID: "projects", Label: "Account management", Listing: projects},
			{ID: "subaccounts", Label: "Subaccounts", Listing: subs},
			{ID: "budgets", Label: "Budgets & alerts", Listing: budgets},
			{ID: "permissions", Label: "Permissions", Listing: perms},
		},
	}, nil
}

func (p billingAccountsProvider) DetailActions(ctx context.Context, _ string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	s, err := p.api()
	if err != nil {
		return nil
	}
	a, err := s.Account(path[0])
	if err != nil {
		return nil
	}
	linked, _ := s.Linked(a.GetName())
	var linkedIDs []string
	for _, pi := range linked {
		linkedIDs = append(linkedIDs, pi.GetProjectId())
	}
	out := []console.Action{
		{ID: "rename", Label: "Rename billing account", Fields: []console.Field{{Name: "displayName", Label: "Name", Type: "text", Required: true, Default: a.GetDisplayName()}}},
		{ID: "link", Label: "Link a project", Fields: []console.Field{{Name: "project", Label: "Project", Type: "select", Required: true, Options: p.projectIDs()}}},
		{ID: "unlink", Label: "Disable billing", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
			Confirm: "Disabling billing stops the project's billable services: CloudBurrow disables its billable APIs until billing is enabled again.",
			Fields:  []console.Field{{Name: "project", Label: "Project", Type: "select", Required: true, Options: linkedIDs}}},
		{ID: "grant", Label: "Grant access", Fields: []console.Field{
			{Name: "member", Label: "New principals", Type: "text", Required: true, Help: "user:EMAIL, group:EMAIL or serviceAccount:EMAIL"},
			{Name: "role", Label: "Role", Type: "select", Required: true, Default: "roles/billing.viewer",
				Options: []string{"roles/billing.admin", "roles/billing.user", "roles/billing.viewer", "roles/billing.costsManager"},
				OptionLabels: map[string]string{"roles/billing.admin": "Billing Account Administrator", "roles/billing.user": "Billing Account User",
					"roles/billing.viewer": "Billing Account Viewer", "roles/billing.costsManager": "Billing Account Costs Manager"}}}},
	}
	if a.GetOpen() {
		out = append(out, console.Action{ID: "close", Label: "Close billing account", Destructive: true,
			Confirm: "A closed account's projects keep their link with billing disabled, and its subaccounts close too."})
	} else {
		out = append(out, console.Action{ID: "reopen", Label: "Reopen billing account"})
	}
	return out
}

func (p billingAccountsProvider) ActAt(ctx context.Context, _ string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("no billing account")
	}
	s, err := p.api()
	if err != nil {
		return err
	}
	name := path[0]
	switch action {
	case "rename":
		_, err := s.UpdateBillingAccount(ctx, &billingpb.UpdateBillingAccountRequest{Name: name,
			Account: &billingpb.BillingAccount{DisplayName: strings.TrimSpace(values["displayName"])}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
		return err
	case "link":
		_, err := s.UpdateProjectBillingInfo(ctx, &billingpb.UpdateProjectBillingInfoRequest{Name: "projects/" + values["project"],
			ProjectBillingInfo: &billingpb.ProjectBillingInfo{BillingAccountName: name}})
		return err
	case "unlink":
		info, err := s.ProjectBilling(values["project"])
		if err != nil {
			return err
		}
		if info.GetBillingAccountName() != name {
			return fmt.Errorf("project %s is not linked to %s", values["project"], name)
		}
		_, err = s.UpdateProjectBillingInfo(ctx, &billingpb.UpdateProjectBillingInfoRequest{Name: "projects/" + values["project"],
			ProjectBillingInfo: &billingpb.ProjectBillingInfo{}})
		return err
	case "grant":
		pol, err := s.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
		if err != nil {
			return err
		}
		member, role := strings.TrimSpace(values["member"]), values["role"]
		found := false
		for _, b := range pol.GetBindings() {
			if b.GetRole() == role && b.GetCondition() == nil {
				b.Members = append(b.Members, member)
				found = true
			}
		}
		if !found {
			pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: role, Members: []string{member}})
		}
		_, err = s.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: pol})
		return err
	case "close", "reopen":
		_, err := s.SetOpen(name, action == "reopen")
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// billingProjectsProvider is Account management's projects: each project
// and the account it is linked to, with Change billing and Disable billing.
type billingProjectsProvider struct{ billingConsole }

func (billingProjectsProvider) ID() string    { return "billing-projects" }
func (billingProjectsProvider) Title() string { return "Account management" }

func (p billingProjectsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := p.api()
	if err != nil {
		return console.Listing{}, err
	}
	l := console.Listing{Columns: []string{"Project ID", "Billing account", "Billing"}, NameColumn: "Project name", Noun: "projects"}
	_, labels := p.accountOptions(false)
	for _, id := range p.projectIDs() {
		info, err := s.ProjectBilling(id)
		if err != nil {
			continue
		}
		acct, state := "—", "Disabled"
		if n := info.GetBillingAccountName(); n != "" {
			acct = labels[n]
			if !info.GetBillingEnabled() {
				state = "Disabled (the account is closed)"
			}
		}
		if info.GetBillingEnabled() {
			state = "Enabled"
		}
		l.Items = append(l.Items, console.Resource{Name: id, Fields: map[string]string{"Project name": id, "Project ID": id,
			"Billing account": acct, "Billing": state}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (p billingProjectsProvider) ListActions(ctx context.Context, project string) []console.Action {
	accts, labels := p.accountOptions(true)
	projects := p.projectIDs()
	return []console.Action{
		{ID: "change", Label: "Change billing", Fields: []console.Field{
			{Name: "project", Label: "Project", Type: "select", Required: true, Default: project, Options: projects},
			{Name: "account", Label: "Billing account", Type: "select", Required: true, Options: accts, OptionLabels: labels}}},
		{ID: "disable", Label: "Disable billing", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
			Confirm: "Disabling billing stops the project's billable services: CloudBurrow disables its billable APIs until billing is enabled again.",
			Fields:  []console.Field{{Name: "project", Label: "Project", Type: "select", Required: true, Default: project, Options: projects}}},
	}
}

func (p billingProjectsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) == 0 {
		return p.ListActions(ctx, project)
	}
	return nil
}

func (p billingProjectsProvider) ActAt(ctx context.Context, _ string, path []string, action string, values map[string]string) error {
	s, err := p.api()
	if err != nil {
		return err
	}
	target := ""
	switch action {
	case "change":
		target = values["account"]
		if target == "" {
			return fmt.Errorf("choose a billing account")
		}
	case "disable":
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	_, err = s.UpdateProjectBillingInfo(ctx, &billingpb.UpdateProjectBillingInfoRequest{Name: "projects/" + values["project"],
		ProjectBillingInfo: &billingpb.ProjectBillingInfo{BillingAccountName: target}})
	return err
}

// budgetListing lists the accounts' budgets, with their actual spend.
func budgetListing(s *billing.Server, accounts []*billingpb.BillingAccount) console.Listing {
	l := console.Listing{Columns: []string{"Billing account", "Budget type", "Amount", "Actual spend", "Thresholds", "Notifications"},
		NameColumn: "Name", Noun: "budgets"}
	for _, a := range accounts {
		list, err := s.Budgets().List(a.GetName())
		if err != nil {
			continue
		}
		for _, x := range list {
			l.Items = append(l.Items, console.Resource{Name: x.GetName(), Opens: []string{x.GetName()}, Fields: map[string]string{
				"Name": kmsOrDash(x.GetDisplayName()), "Billing account": billingAccountID(a.GetName()), "Budget type": budgetType(x),
				"Amount": budgetAmount(x, a.GetCurrencyCode()), "Actual spend": fmt.Sprintf("%.2f %s", billing.Spend(x), a.GetCurrencyCode()),
				"Thresholds": budgetThresholds(x), "Notifications": kmsOrDash(x.GetNotificationsRule().GetPubsubTopic())}})
		}
	}
	l.Total = len(l.Items)
	return l
}

func budgetType(x *budgetspb.Budget) string {
	if x.GetAmount().GetLastPeriodAmount() != nil {
		return "Last period's spend"
	}
	return "Specified amount"
}

func budgetAmount(x *budgetspb.Budget, currency string) string {
	m := x.GetAmount().GetSpecifiedAmount()
	if m == nil {
		return "100% of last period's spend"
	}
	if c := m.GetCurrencyCode(); c != "" {
		currency = c
	}
	return fmt.Sprintf("%.2f %s", float64(m.GetUnits())+float64(m.GetNanos())/1e9, currency)
}

func budgetThresholds(x *budgetspb.Budget) string {
	var out []string
	for _, r := range x.GetThresholdRules() {
		s := strconv.FormatFloat(r.GetThresholdPercent()*100, 'f', -1, 64) + "%"
		if r.GetSpendBasis() == budgetspb.ThresholdRule_FORECASTED_SPEND {
			s += " (forecasted)"
		}
		out = append(out, s)
	}
	return kmsOrDash(strings.Join(out, ", "))
}

// billingBudgetsProvider is Budgets & alerts: every account's budgets,
// with Create budget; a budget's page shows its scope, amount, actual spend
// and thresholds, with Edit and Delete.
type billingBudgetsProvider struct{ billingConsole }

func (billingBudgetsProvider) ID() string    { return "billing-budgets" }
func (billingBudgetsProvider) Title() string { return "Budgets & alerts" }

func (p billingBudgetsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := p.api()
	if err != nil {
		return console.Listing{}, err
	}
	all, err := s.Accounts()
	if err != nil {
		return console.Listing{}, err
	}
	l := budgetListing(s, all)
	l.RowsOpenable = true
	return l, nil
}

func (p billingBudgetsProvider) CreateForm() (string, []console.Field) {
	accts, labels := p.accountOptions(true)
	def := ""
	if len(accts) > 0 {
		def = accts[0]
	}
	return "Create budget", budgetFields(accts, labels, def, nil)
}

func budgetFields(accts []string, labels map[string]string, account string, x *budgetspb.Budget) []console.Field {
	amount, thresholds, topic, projects := "", "50,90,100", "", ""
	name := ""
	if x != nil {
		name = x.GetDisplayName()
		if m := x.GetAmount().GetSpecifiedAmount(); m != nil {
			amount = strconv.FormatFloat(float64(m.GetUnits())+float64(m.GetNanos())/1e9, 'f', -1, 64)
		}
		var ts []string
		for _, r := range x.GetThresholdRules() {
			ts = append(ts, strconv.FormatFloat(r.GetThresholdPercent()*100, 'f', -1, 64))
		}
		thresholds = strings.Join(ts, ",")
		topic = x.GetNotificationsRule().GetPubsubTopic()
		projects = strings.Join(x.GetBudgetFilter().GetProjects(), ",")
	}
	const scope, amt, actions = "Scope", "Amount", "Actions"
	var out []console.Field
	if x == nil {
		out = append(out, console.Field{Name: "account", Label: "Billing account", Type: "select", Required: true, Default: account,
			Options: accts, OptionLabels: labels, Section: scope})
	}
	return append(out,
		console.Field{Name: "displayName", Label: "Name", Type: "text", Default: name, Section: scope, Help: "At most 60 characters."},
		console.Field{Name: "projects", Label: "Projects", Type: "text", Default: projects, Section: scope,
			Help: "projects/PROJECT, comma-separated; empty is every project of the account."},
		console.Field{Name: "amount", Label: "Target amount", Type: "text", Required: true, Default: amount, Section: amt,
			Pattern: `^[0-9]+(\.[0-9]{1,9})?$`, Help: "In the account's currency."},
		console.Field{Name: "thresholds", Label: "Alert thresholds (% of budget)", Type: "text", Default: thresholds, Section: actions,
			Pattern: `^\s*[0-9]+(\.[0-9]+)?(\s*,\s*[0-9]+(\.[0-9]+)?)*\s*$`, Help: "Percentages of the actual spend, comma-separated."},
		console.Field{Name: "topic", Label: "Pub/Sub topic", Type: "text", Default: topic, Section: actions,
			Pattern: `^(projects/[^\/]+/topics/[^\/]+)?$`, Help: "projects/PROJECT/topics/TOPIC, for programmatic notifications (schema version 1.0)."},
	)
}

// budgetFromValues builds the budget a form describes.
func budgetFromValues(values map[string]string, currency string) (*budgetspb.Budget, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(values["amount"]), 64)
	if err != nil || f < 0 {
		return nil, fmt.Errorf("the target amount must be a non-negative number")
	}
	units := int64(f)
	x := &budgetspb.Budget{DisplayName: strings.TrimSpace(values["displayName"]),
		Amount: &budgetspb.BudgetAmount{BudgetAmount: &budgetspb.BudgetAmount_SpecifiedAmount{SpecifiedAmount: &money.Money{
			CurrencyCode: currency, Units: units, Nanos: int32((f - float64(units)) * 1e9)}}},
		BudgetFilter: &budgetspb.Filter{}}
	for _, p := range strings.Split(values["projects"], ",") {
		if p = strings.TrimSpace(p); p != "" {
			if !strings.HasPrefix(p, "projects/") {
				p = "projects/" + p
			}
			x.BudgetFilter.Projects = append(x.BudgetFilter.Projects, p)
		}
	}
	for _, t := range strings.Split(values["thresholds"], ",") {
		if t = strings.TrimSpace(t); t != "" {
			v, err := strconv.ParseFloat(t, 64)
			if err != nil {
				return nil, fmt.Errorf("threshold %q is not a number", t)
			}
			x.ThresholdRules = append(x.ThresholdRules, &budgetspb.ThresholdRule{ThresholdPercent: v / 100})
		}
	}
	if t := strings.TrimSpace(values["topic"]); t != "" {
		x.NotificationsRule = &budgetspb.NotificationsRule{PubsubTopic: t, SchemaVersion: "1.0"}
	}
	return x, nil
}

func (p billingBudgetsProvider) Create(ctx context.Context, _ string, values map[string]string) (string, error) {
	s, err := p.api()
	if err != nil {
		return "", err
	}
	a, err := s.Account(values["account"])
	if err != nil {
		return "", err
	}
	x, err := budgetFromValues(values, a.GetCurrencyCode())
	if err != nil {
		return "", err
	}
	out, err := s.Budgets().CreateBudget(ctx, &budgetspb.CreateBudgetRequest{Parent: a.GetName(), Budget: x})
	if err != nil {
		return "", err
	}
	return out.GetName(), nil
}

func (p billingBudgetsProvider) budget(ctx context.Context, name string) (*billing.Server, *budgetspb.Budget, error) {
	s, err := p.api()
	if err != nil {
		return nil, nil, err
	}
	x, err := s.Budgets().GetBudget(ctx, &budgetspb.GetBudgetRequest{Name: name})
	return s, x, err
}

func (p billingBudgetsProvider) Detail(ctx context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	s, x, err := p.budget(ctx, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the budget: " + consoleErr(err)}, nil
	}
	acct, _, _ := strings.Cut(strings.TrimPrefix(x.GetName(), "billingAccounts/"), "/")
	cur := billing.Currency
	if a, err := s.Account("billingAccounts/" + acct); err == nil {
		cur = a.GetCurrencyCode()
	}
	scope := strings.Join(x.GetBudgetFilter().GetProjects(), ", ")
	if scope == "" {
		scope = "All projects"
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Amount", Value: budgetAmount(x, cur)}, {Label: "Actual spend", Value: fmt.Sprintf("%.2f %s", billing.Spend(x), cur)}},
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Budget", Properties: []console.Property{
			{Label: "Name", Value: kmsOrDash(x.GetDisplayName())}, {Label: "Resource name", Value: x.GetName()},
			{Label: "Billing account", Value: acct}, {Label: "Projects", Value: scope},
			{Label: "Credits", Value: x.GetBudgetFilter().GetCreditTypesTreatment().String()},
			{Label: "Budget type", Value: budgetType(x)}, {Label: "Amount", Value: budgetAmount(x, cur)},
			{Label: "Actual spend", Value: fmt.Sprintf("%.2f %s (CloudBurrow is local: usage costs nothing, so no threshold is reached)", billing.Spend(x), cur)},
			{Label: "Alert thresholds", Value: budgetThresholds(x)},
			{Label: "Pub/Sub topic", Value: kmsOrDash(x.GetNotificationsRule().GetPubsubTopic())}}}}}},
	}, nil
}

func (p billingBudgetsProvider) DetailActions(ctx context.Context, _ string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	_, x, err := p.budget(ctx, path[0])
	if err != nil {
		return nil
	}
	return []console.Action{
		{ID: "edit", Label: "Edit budget", Fields: budgetFields(nil, nil, "", x)},
		{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Deleting a budget cannot be undone."},
	}
}

func (p billingBudgetsProvider) ActAt(ctx context.Context, _ string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("no budget")
	}
	s, x, err := p.budget(ctx, path[0])
	if err != nil {
		return err
	}
	switch action {
	case "edit":
		cur := x.GetAmount().GetSpecifiedAmount().GetCurrencyCode()
		if cur == "" {
			cur = billing.Currency
		}
		next, err := budgetFromValues(values, cur)
		if err != nil {
			return err
		}
		next.Name = x.GetName()
		next.BudgetFilter.CreditTypesTreatment = x.GetBudgetFilter().GetCreditTypesTreatment()
		_, err = s.Budgets().UpdateBudget(ctx, &budgetspb.UpdateBudgetRequest{Budget: next, UpdateMask: &fieldmaskpb.FieldMask{
			Paths: []string{"display_name", "budget_filter.projects", "amount", "threshold_rules", "notifications_rule"}}})
		return err
	case "delete":
		_, err := s.Budgets().DeleteBudget(ctx, &budgetspb.DeleteBudgetRequest{Name: x.GetName()})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// billingPricingProvider is Pricing: the local catalog's SKUs, one per
// served API, each at the local list price of 0.
type billingPricingProvider struct{ billingConsole }

func (billingPricingProvider) ID() string    { return "billing-pricing" }
func (billingPricingProvider) Title() string { return "Pricing" }

func (p billingPricingProvider) List(ctx context.Context, project string) (console.Listing, error) {
	if p.catalog == nil {
		return console.Listing{}, errors.New(billingNotStarted)
	}
	l := console.Listing{Columns: []string{"Service ID", "SKU ID", "SKU description", "Unit", "List price"}, NameColumn: "Service",
		Noun: "SKUs", TableTitle: "Pricing for the services this instance serves"}
	svcs, err := p.catalog.ListServices(ctx, &billingpb.ListServicesRequest{})
	if err != nil {
		return console.Listing{}, err
	}
	for _, sv := range svcs.GetServices() {
		skus, err := p.catalog.ListSkus(ctx, &billingpb.ListSkusRequest{Parent: sv.GetName()})
		if err != nil {
			continue
		}
		for _, sk := range skus.GetSkus() {
			pe := sk.GetPricingInfo()[0].GetPricingExpression()
			price := pe.GetTieredRates()[0].GetUnitPrice()
			l.Items = append(l.Items, console.Resource{Name: sk.GetName(), Fields: map[string]string{
				"Service": sv.GetDisplayName(), "Service ID": sv.GetServiceId(), "SKU ID": sk.GetSkuId(), "SKU description": sk.GetDescription(),
				"Unit": pe.GetUsageUnitDescription(), "List price": fmt.Sprintf("%.2f %s", float64(price.GetUnits())+float64(price.GetNanos())/1e9, price.GetCurrencyCode())}})
		}
	}
	l.Total = len(l.Items)
	return l, nil
}

var (
	_ console.Creator   = billingAccountsProvider{}
	_ console.Driller   = billingAccountsProvider{}
	_ console.PathActor = billingAccountsProvider{}
	_ console.ListActor = billingProjectsProvider{}
	_ console.Creator   = billingBudgetsProvider{}
	_ console.Driller   = billingBudgetsProvider{}
	_ console.PathActor = billingBudgetsProvider{}
)
