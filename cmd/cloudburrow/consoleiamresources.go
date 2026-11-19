package main

// IAM & Admin's Resource Manager pages (#1864), as Google's IAM & Admin
// menu lists them: Organization Policies, Labels and Settings for the
// selected project, over the same registry Resource Manager's API serves,
// so a change here is one gcloud and Terraform read back.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// --- Organization Policies ---------------------------------------------

type iamOrgPoliciesProvider struct {
	registry *resourcemanager.Registry
}

func (iamOrgPoliciesProvider) ID() string    { return "iam-orgpolicies" }
func (iamOrgPoliciesProvider) Title() string { return "Organization Policies" }

// List is every constraint Google's page lists for the project, set or
// not, with where its policy comes from and what is in effect.
func (p iamOrgPoliciesProvider) List(_ context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Constraint", Columns: []string{"Name", "Policy source", "Effective", "Last edited"},
		Noun: "organization policies", Note: orgPolicyNote}
	if project == "" {
		l.Prompt = "Organization policies are per project. Choose one in the toolbar."
		return l, nil
	}
	resource := "projects/" + project
	own := map[string]string{}
	if all, err := p.registry.OrgPolicies(resource); err == nil {
		for _, op := range all {
			own[op.Constraint] = op.UpdateTime
		}
	} else {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, c := range resourcemanager.Constraints {
		source := "Inherit parent's policy"
		updated, set := own[c.Name]
		if set {
			source = "Override parent's policy"
		}
		effective := "Google-managed default"
		if eff, err := p.registry.EffectiveOrgPolicy(resource, c.Name); err == nil && eff != nil {
			if t := orgPolicyText(eff); t != "-" {
				effective = t
			}
		}
		l.Items = append(l.Items, console.Resource{Name: c.Name, Fields: map[string]string{
			"Name": c.DisplayName, "Policy source": source, "Effective": effective, "Last edited": updated}})
	}
	l.Total = len(l.Items)
	return l, nil
}

// Detail is Google's Policy details: the constraint, the project's own
// policy and the effective one, with Manage policy beside the title.
func (p iamOrgPoliciesProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	c := resourcemanager.KnownConstraint(path[0])
	if c == nil {
		return console.Detail{Unavailable: fmt.Sprintf("no constraint %s", path[0])}, nil
	}
	if project == "" {
		return console.Detail{Unavailable: "choose a project in the toolbar"}, nil
	}
	resource := "projects/" + project
	kind := "Boolean"
	if c.ListConstraint != nil {
		kind = "List"
	}
	own, effective := "Inherit parent's policy", "Google-managed default"
	if all, err := p.registry.OrgPolicies(resource); err == nil {
		for _, op := range all {
			if op.Constraint == c.Name {
				own = orgPolicyText(op)
			}
		}
	}
	if eff, err := p.registry.EffectiveOrgPolicy(resource, c.Name); err == nil && eff != nil && orgPolicyText(eff) != "-" {
		effective = orgPolicyText(eff)
	}
	return console.Detail{Title: c.DisplayName, Summary: []console.Property{
		{Label: "Constraint ID", Value: c.Name}, {Label: "Constraint type", Value: kind},
		{Label: "Applies to", Value: resource},
		{Label: "Policy for this project", Value: own}, {Label: "Effective policy", Value: effective},
	}, Sections: []console.Section{{ID: "about", Label: "Details", Kind: console.KindText, Text: orgPolicyNote}}}, nil
}

// DetailActions are Manage policy, which opens Edit policy
// (consoleorgpolicyedit.go), as Google's page does, and, on a policy of the
// project's own, Delete policy, each on this constraint.
func (p iamOrgPoliciesProvider) DetailActions(_ context.Context, project string, path []string) []console.Action {
	if project == "" || len(path) != 1 || resourcemanager.KnownConstraint(path[0]) == nil {
		return nil
	}
	out := []console.Action{{ID: "manage-policy", Label: "Manage policy",
		Href: "/iam-admin/orgpolicies/" + url.PathEscape(path[0]) + "/edit?project=" + url.QueryEscape(project)}}
	for _, a := range orgPolicyActions(p.registry, "projects/"+project, "") {
		if a.ID == orgPolicySet {
			continue
		}
		fields := make([]console.Field, 0, len(a.Fields))
		for _, f := range a.Fields {
			if f.Name == "constraint" {
				if !slices.Contains(f.Options, path[0]) {
					fields = nil
					break
				}
				f.Default, f.Options = path[0], []string{path[0]}
			}
			fields = append(fields, f)
		}
		if fields == nil {
			continue
		}
		a.Fields = fields
		a.Label = "Delete policy"
		out = append(out, a)
	}
	return out
}

func (p iamOrgPoliciesProvider) ActAt(_ context.Context, project string, path []string, action string, values map[string]string) error {
	if project == "" || len(path) != 1 {
		return errors.New("open a constraint of the selected project")
	}
	values["constraint"] = path[0]
	ok, err := orgPolicyAct(p.registry, "projects/"+project, action, values)
	if !ok {
		return fmt.Errorf("unknown action %q", action)
	}
	return err
}

// --- Labels ---------------------------------------------------------------

type iamLabelsProvider struct {
	registry *resourcemanager.Registry
}

func (iamLabelsProvider) ID() string    { return "iam-labels" }
func (iamLabelsProvider) Title() string { return "Labels" }

const iamLabelsNote = "The selected project's labels, Resource Manager's projects.labels: stored and returned, interpreted by nothing."

// Google's label rules: a key starts with a lowercase letter; keys and
// values hold lowercase letters, digits, underscores and dashes, at most 63
// characters.
var (
	labelKeyPattern   = regexp.MustCompile(`^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_\-]{0,62}$`)
	labelValuePattern = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_\-]{0,63}$`)
)

func (p iamLabelsProvider) List(_ context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Key", Columns: []string{"Value"}, Noun: "labels", Note: iamLabelsNote}
	if project == "" {
		l.Prompt = "Labels are per project. Choose one in the toolbar."
		return l, nil
	}
	pr, err := p.registry.Get(project)
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, k := range slices.Sorted(maps.Keys(pr.Labels)) {
		l.Items = append(l.Items, console.Resource{Name: k, Fields: map[string]string{"Value": pr.Labels[k]}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (iamLabelsProvider) CreateForm() (string, []console.Field) {
	return "Add label", []console.Field{
		{Name: "key", Label: "Key", Type: "text", Required: true, Pattern: labelKeyPattern.String(),
			Help: "Starts with a lowercase letter; lowercase letters, numbers, underscores and dashes, at most 63 characters."},
		{Name: "value", Label: "Value", Type: "text", Pattern: labelValuePattern.String(),
			Help: "Lowercase letters, numbers, underscores and dashes, at most 63 characters."},
	}
}

func (p iamLabelsProvider) setLabel(project, key, value string, remove bool) error {
	if project == "" {
		return errors.New("choose a project in the toolbar")
	}
	pr, err := p.registry.Get(project)
	if err != nil {
		return err
	}
	labels := maps.Clone(pr.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	if remove {
		if _, ok := labels[key]; !ok {
			return fmt.Errorf("project %s has no label %q", project, key)
		}
		delete(labels, key)
	} else {
		if !labelKeyPattern.MatchString(key) {
			return fmt.Errorf("label key %q: start with a lowercase letter; use lowercase letters, numbers, underscores and dashes, at most 63 characters", key)
		}
		if !labelValuePattern.MatchString(value) {
			return fmt.Errorf("label value %q: use lowercase letters, numbers, underscores and dashes, at most 63 characters", value)
		}
		if _, ok := labels[key]; !ok && len(labels) >= 64 {
			return errors.New("a project holds at most 64 labels")
		}
		labels[key] = value
	}
	// Update writes the display name too: the project's own, unchanged.
	_, err = p.registry.Update(project, pr.DisplayName, labels)
	return err
}

func (p iamLabelsProvider) Create(_ context.Context, project string, values map[string]string) (string, error) {
	key := strings.TrimSpace(values["key"])
	pr, err := p.registry.Get(project)
	if err == nil {
		if _, ok := pr.Labels[key]; ok {
			return "", fmt.Errorf("project %s already has a label %q: edit it instead", project, key)
		}
	}
	return key, p.setLabel(project, key, strings.TrimSpace(values["value"]), false)
}

func (p iamLabelsProvider) Delete(_ context.Context, project, name string) error {
	return p.setLabel(project, name, "", true)
}

func (p iamLabelsProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	pr, err := p.registry.Get(project)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	v, ok := pr.Labels[path[0]]
	if !ok {
		return console.Detail{Unavailable: fmt.Sprintf("project %s has no label %q", project, path[0])}, nil
	}
	return console.Detail{Title: path[0], Summary: []console.Property{{Label: "Key", Value: path[0]}, {Label: "Value", Value: orDash(v)}},
		Edit: &console.EditForm{Label: "Edit label", Fields: []console.Field{
			{Name: "value", Label: "Value", Type: "text", Default: v, Pattern: labelValuePattern.String()}}}}, nil
}

func (p iamLabelsProvider) Edit(_ context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a label can be edited")
	}
	return p.setLabel(project, path[0], strings.TrimSpace(values["value"]), false)
}

// --- Settings -------------------------------------------------------------

type iamSettingsProvider struct {
	registry *resourcemanager.Registry
}

func (iamSettingsProvider) ID() string    { return "iam-settings" }
func (iamSettingsProvider) Title() string { return "Settings" }

// List is the selected project's one row, Google's Settings page; its page
// has Edit (Project name) and Shut down (#1865), as Google's Settings page
// does, which Manage resources' Delete also makes.
func (p iamSettingsProvider) List(_ context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Project ID", Columns: []string{"Project name", "Project number", "Location"}, Noun: "projects",
		Note: "The selected project's settings. Shut down, on its page or Manage resources' Delete, schedules the project for deletion: it is kept 30 days, when Restore brings it back, then purged."}
	if project == "" {
		l.Prompt = "Settings are per project. Choose one in the toolbar."
		return l, nil
	}
	pr, err := p.registry.Get(project)
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	l.Items = []console.Resource{{Name: pr.ProjectID, Fields: map[string]string{
		"Project name": pr.DisplayName, "Project number": strconv.FormatInt(pr.Number, 10), "Location": settingsLocation(pr.Parent)}}}
	l.Total = 1
	return l, nil
}

func settingsLocation(parent string) string {
	if parent == "" {
		return "No organization"
	}
	return parent
}

func (p iamSettingsProvider) Detail(_ context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	pr, err := p.registry.Get(path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	return console.Detail{Title: pr.DisplayName, Summary: []console.Property{
		{Label: "Project name", Value: pr.DisplayName}, {Label: "Project ID", Value: pr.ProjectID},
		{Label: "Project number", Value: strconv.FormatInt(pr.Number, 10)}, {Label: "Location", Value: settingsLocation(pr.Parent)},
	}, Edit: &console.EditForm{Label: "Edit", Note: "The project ID and number cannot be changed.", Fields: []console.Field{
		{Name: "displayName", Label: "Project name", Type: "text", Required: true, Default: pr.DisplayName,
			Help: "4 to 30 characters: letters, numbers, quotes, hyphens, spaces and exclamation points."}}}}, nil
}

func (p iamSettingsProvider) Edit(_ context.Context, _ string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a project can be edited")
	}
	pr, err := p.registry.Get(path[0])
	if err != nil {
		return err
	}
	_, err = p.registry.Update(path[0], values["displayName"], pr.Labels)
	return err
}

// settingsShutDown is Google's Shut down on the Settings page: the
// project is DELETE_REQUESTED for 30 days, then purged (#1865).
const settingsShutDown = "shut-down"

// DetailActions is Shut down, on an active project.
func (p iamSettingsProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	pr, err := p.registry.Get(path[0])
	if err != nil || pr.State != resourcemanager.StateActive {
		return nil
	}
	return []console.Action{{ID: settingsShutDown, Label: "Shut down", Destructive: true,
		Confirm: "all billing and traffic serving stop, and every resource in the project is scheduled for deletion. " +
			"The project is kept for 30 days, when Restore on Manage resources' Resources pending deletion brings it back; then it is deleted"}}
}

func (p iamSettingsProvider) ActAt(_ context.Context, _ string, path []string, action string, _ map[string]string) error {
	if len(path) != 1 || action != settingsShutDown {
		return fmt.Errorf("unknown action %q", action)
	}
	_, err := p.registry.Delete(path[0])
	return err
}

var (
	_ console.Driller   = iamOrgPoliciesProvider{}
	_ console.PathActor = iamOrgPoliciesProvider{}
	_ console.Creator   = iamLabelsProvider{}
	_ console.Deleter   = iamLabelsProvider{}
	_ console.Driller   = iamLabelsProvider{}
	_ console.Editor    = iamLabelsProvider{}
	_ console.Driller   = iamSettingsProvider{}
	_ console.Editor    = iamSettingsProvider{}
	_ console.PathActor = iamSettingsProvider{}
)
