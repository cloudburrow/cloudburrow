package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// tagsProvider is the Tags screen of the Resource hierarchy (#1295): tag keys
// under the organization or a project, their values, over the registry the
// v3 TagKeys and TagValues APIs use. Bindings are made on the page of the
// project or folder they tag (tagSection, tagActions), and on a BigQuery
// dataset's or table's (consolebigquerytags.go, #1780). The IAM conditions
// of Memorystore's IAM auth and BigQuery's per-project IAM read them; no
// organization policy does.
type tagsProvider struct {
	registry *resourcemanager.Registry
}

func (tagsProvider) ID() string    { return "tags" }
func (tagsProvider) Title() string { return "Tags" }

const tagsNote = "Tag keys under the organization (" + resourcemanager.OrganizationName + ") or a project, and their values. " +
	"Bind a value on a project's or folder's page, which also shows the tags it inherits. Stored, not enforced: " +
	"nothing here reads a tag in an IAM condition or an organization policy. A key with values, or a value that is bound, is not deleted."

const tagShortNamePattern = `^[A-Za-z0-9]([A-Za-z0-9._\-]{0,254}[A-Za-z0-9])?$`

func tagID(name string) string { return name[strings.LastIndex(name, "/")+1:] }

func (p tagsProvider) List(ctx context.Context, _ string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Tag key ID", Columns: []string{"Short name", "Namespaced name", "Parent", "Values", "Description"},
		Noun: "tag keys", Note: tagsNote}
	// The organization chosen in the toolbar lists its own keys, through
	// TagKeys' list of that parent (#2023); a folder holds no tag keys, so
	// with a folder chosen the page lists the organization's, the keys its
	// resources can carry.
	sel, scoped, err := consoleResourceScope(ctx, p.registry)
	if err != nil {
		l.Unavailable = err.Error()
		return l, nil
	}
	var keys []*rmpb.TagKey
	if scoped {
		keys, err = p.registry.TagKeys(resourcemanager.OrganizationName)
		if sel.Kind == "Folder" {
			l.Note = "A folder holds no tag keys: they are created in the organization or a project. These are the organization's, " +
				"which " + sel.Display + " and its resources can carry. " + tagsNote
		}
	} else {
		keys, err = p.registry.AllTagKeys()
	}
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, k := range keys {
		n := 0
		if vs, err := p.registry.TagValues(k.GetName()); err == nil {
			n = len(vs)
		}
		count := strconv.Itoa(n)
		if re := p.registry.TagKeyAllowedValuesRegex(k.GetName()); re != "" {
			count = "Dynamic: " + re
		}
		// Google's row menu: Actions, View details, and Manage access, the
		// key's IAM policy (#1865).
		page := "/tags/" + tagID(k.GetName())
		l.Items = append(l.Items, console.Resource{Name: tagID(k.GetName()), Fields: map[string]string{
			"Short name": k.GetShortName(), "Namespaced name": k.GetNamespacedName(), "Parent": k.GetParent(),
			"Values": count, "Description": k.GetDescription()},
			Actions: []console.Action{{ID: "view", Label: "View details", Href: page},
				{ID: "access", Label: "Manage access", Href: page + "?tab=permissions"}}})
	}
	l.Total = len(l.Items)
	return l, nil
}

// parentOptions are the parents a tag key can be created under: the
// organization, then every active project.
func (p tagsProvider) parentOptions() []string {
	out := []string{resourcemanager.OrganizationName}
	if ps, err := p.registry.Active(); err == nil {
		for _, pr := range ps {
			out = append(out, "projects/"+pr.ProjectID)
		}
	}
	return out
}

// CreateForm is Google's Create tag key (#1865, "Creating and managing
// tags", read 2026-11-20): Tag key, Tag key description, the purpose
// (For use with network firewall, GCE_FIREWALL, with its network), then
// the Tag values section: Predefined values, a row per value made with
// Add value, each its Tag value and Tag value description, or Dynamic
// values (#2023): Add value constraints, then the validation regular
// expression, the key's allowedValuesRegex (".*", any value, without
// constraints). The page ends in Create tag key.
// The key is made in the organization or a project, as the resource
// selector names it on Google's page.
func (p tagsProvider) CreateForm() (string, []console.Field) {
	return "Create tag key", []console.Field{
		{Name: "shortName", Label: "Tag key", Type: "text", Required: true, Pattern: tagShortNamePattern,
			Help: "Up to 256 characters, starting and ending with a letter or digit; it can contain dashes, underscores and dots. It cannot be changed later."},
		{Name: "description", Label: "Tag key description", Type: "text", Help: "Optional, at most 256 characters."},
		{Name: "purpose", Label: "For use with network firewall", Type: "checkbox",
			Help: "A key for network firewall policies (purpose GCE_FIREWALL): its values can be bound to the VPC network below only."},
		{Name: "network", Label: "Network", Type: "text", ShowWhen: "purpose",
			Help: "The VPC network the key's values apply to, as PROJECT_ID/NETWORK or projects/PROJECT_ID/global/networks/NETWORK."},
		{Name: "valueType", Label: "Tag values", Type: "radio", Default: tagValuesPredefined,
			Options:      []string{tagValuesPredefined, tagValuesDynamic},
			OptionLabels: map[string]string{tagValuesPredefined: "Predefined values", tagValuesDynamic: "Dynamic values"},
			Help: "Predefined values: a fixed list of allowed values, which can also be added later on the key's page. " +
				"Dynamic values: a value is typed when the tag is attached, and the key can't also have predefined values."},
		{Name: "valueConstraints", Label: "Add value constraints", Type: "checkbox", ShowWhen: "valueType=" + tagValuesDynamic,
			Help: "Without constraints, any value can be attached (the regular expression .*)."},
		{Name: "allowedValuesRegex", Label: "Validation regular expression", Type: "text", Default: ".*",
			ShowWhen: "valueType=" + tagValuesDynamic + "&valueConstraints",
			Help:     "RE2 syntax. A dynamic value must match it whole, for example env-[a-z]+."},
		{Name: "values", Label: "Tag value", Type: console.GroupFieldType, AddLabel: "Add value", ShowWhen: "valueType=" + tagValuesPredefined,
			Group: []console.Field{
				{Name: "shortName", Label: "Tag value", Type: "text", Required: true, Pattern: tagShortNamePattern,
					Help: "up to 256 characters, starting and ending with a letter or digit"},
				{Name: "description", Label: "Tag value description", Type: "text"},
			}},
		{Name: "parent", Label: "Organization or project", Type: "select", Required: true, Options: p.parentOptions(), Default: resourcemanager.OrganizationName,
			Help: "The key's namespace: the organization, or a project."},
	}
}

// CreateFormFor implements console.ProjectCreateFormer: with the
// organization or a folder chosen in the toolbar (#2023), the key is made
// in the organization, as Google's Create tag key does for the resource
// the selector names.
func (p tagsProvider) CreateFormFor(ctx context.Context, _ string) []console.Field {
	_, fields := p.CreateForm()
	if _, scoped, err := consoleResourceScope(ctx, p.registry); err == nil && scoped {
		for i := range fields {
			if fields[i].Name == "parent" {
				fields[i].Options, fields[i].Default = []string{resourcemanager.OrganizationName}, resourcemanager.OrganizationName
			}
		}
	}
	return fields
}

var _ console.ProjectCreateFormer = tagsProvider{}

// The Tag values choices of Create tag key.
const (
	tagValuesPredefined = "predefined"
	tagValuesDynamic    = "dynamic"
)

// CreateSubmit implements console.CreateSubmitter: Google's page ends in
// Create tag key.
func (tagsProvider) CreateSubmit() string { return "Create tag key" }

// CreateLayout implements console.CreateLayouter: one page, as Google's.
func (tagsProvider) CreateLayout() string { return console.CreateLayoutPage }

func (p tagsProvider) Create(_ context.Context, _ string, values map[string]string) (string, error) {
	in := &rmpb.TagKey{Parent: values["parent"], ShortName: strings.TrimSpace(values["shortName"]), Description: values["description"]}
	if values["purpose"] == "true" {
		in.Purpose = rmpb.Purpose_GCE_FIREWALL
		in.PurposeData = map[string]string{"network": strings.TrimSpace(values["network"])}
	}
	if values["valueType"] == tagValuesDynamic {
		re := ".*"
		if values["valueConstraints"] == "true" && strings.TrimSpace(values["allowedValuesRegex"]) != "" {
			re = strings.TrimSpace(values["allowedValuesRegex"])
		}
		k, err := p.registry.CreateTagKeyWithRegex(in, re, false)
		if err != nil {
			return "", err
		}
		return tagID(k.GetName()), nil
	}
	rows, err := console.ParseGroup(values["values"])
	if err != nil {
		return "", fmt.Errorf("tag values: %w", err)
	}
	tagValues := map[string]string{}
	for _, r := range rows {
		v := strings.TrimSpace(r["shortName"])
		if _, dup := tagValues[v]; dup {
			return "", fmt.Errorf("tag value %q is listed twice", v)
		}
		tagValues[v] = strings.TrimSpace(r["description"])
	}
	// Every value is checked before the key is made, so a bad one leaves
	// nothing behind.
	for v := range tagValues {
		if !tagShortNameRE.MatchString(v) {
			return "", fmt.Errorf("tag value %q: up to 256 characters, starting and ending with a letter or digit", v)
		}
	}
	k, err := p.registry.CreateTagKey(in, false)
	if err != nil {
		return "", err
	}
	short := make([]string, 0, len(tagValues))
	for v := range tagValues {
		short = append(short, v)
	}
	sort.Strings(short)
	for _, v := range short {
		if _, err := p.registry.CreateTagValue(&rmpb.TagValue{Parent: k.GetName(), ShortName: v, Description: tagValues[v]}, false); err != nil {
			return tagID(k.GetName()), fmt.Errorf("tag key %s was created, but its value %s was not: %w", k.GetNamespacedName(), v, err)
		}
	}
	return tagID(k.GetName()), nil
}

var tagShortNameRE = regexp.MustCompile(tagShortNamePattern)

func (p tagsProvider) Delete(_ context.Context, _ string, name string) error {
	_, err := p.registry.DeleteTagKey("tagKeys/"+name, "", false)
	return err
}

func (p tagsProvider) Detail(_ context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	k, err := p.registry.TagKey("tagKeys/" + path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the tag key: " + consoleErr(err)}, nil
	}
	if len(path) == 2 {
		v, err := p.registry.TagValue("tagValues/" + path[1])
		if err != nil || v.GetParent() != k.GetName() {
			return console.Detail{Unavailable: fmt.Sprintf("tag value %s is not a value of %s", path[1], k.GetName())}, nil
		}
		props := []console.Property{
			{Label: "Short name", Value: v.GetShortName()},
			{Label: "Namespaced name", Value: v.GetNamespacedName()},
			{Label: "Resource name", Value: v.GetName()},
			{Label: "Tag key", Value: k.GetNamespacedName() + " (" + k.GetName() + ")"},
			{Label: "Description", Value: orDash(v.GetDescription())},
			{Label: "Created", Value: v.GetCreateTime().AsTime().Local().Format("2006-01-02 15:04")},
		}
		return console.Detail{Summary: []console.Property{props[0], props[1], {Label: "Description", Value: orDash(v.GetDescription()), Edit: "Edit"}},
			Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{Heading: "Tag value", Properties: props}}}},
			Edit: tagDescriptionEdit(v.GetDescription(), ""),
		}, nil
	}
	props := []console.Property{
		{Label: "Short name", Value: k.GetShortName()},
		{Label: "Namespaced name", Value: k.GetNamespacedName()},
		{Label: "Resource name", Value: k.GetName()},
		{Label: "Parent", Value: k.GetParent()},
		{Label: "Purpose", Value: orDash(strings.TrimPrefix(k.GetPurpose().String(), "PURPOSE_UNSPECIFIED"))},
		{Label: "Description", Value: orDash(k.GetDescription())},
		{Label: "Created", Value: k.GetCreateTime().AsTime().Local().Format("2006-01-02 15:04")},
	}
	if re := p.registry.TagKeyAllowedValuesRegex(k.GetName()); re != "" {
		// A key of dynamic values shows its validation regular expression
		// instead of a list of values, as Google's page does (#2023).
		props = append(props, console.Property{Label: "Tag values", Value: "Dynamic values"},
			console.Property{Label: "Validation regular expression", Value: re})
		edit := tagDescriptionEdit(k.GetDescription(), "The short name and parent cannot change, as on Google. "+
			"A new regular expression applies to values attached from now on; bound values stay.")
		edit.Title = "Edit tag key"
		edit.Fields = append(edit.Fields, console.Field{Name: "allowedValuesRegex", Label: "Validation regular expression", Type: "text",
			Required: true, Default: re, Help: "RE2 syntax; .* allows any value."})
		return console.Detail{Summary: []console.Property{props[0], props[1], {Label: "Description", Value: orDash(k.GetDescription()), Edit: "Edit"},
			{Label: "Validation regular expression", Value: re, Edit: "Edit"}},
			Sections: []console.Section{
				{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Tag key", Properties: props}}},
			},
			Edit: edit,
		}, nil
	}
	values := console.Listing{NameColumn: "Tag value ID", Columns: []string{"Short name", "Namespaced name", "Description"}, Noun: "values"}
	if vs, err := p.registry.TagValues(k.GetName()); err != nil {
		values.Unavailable = consoleErr(err)
	} else {
		for _, v := range vs {
			values.Items = append(values.Items, console.Resource{Name: tagID(v.GetName()), Fields: map[string]string{
				"Short name": v.GetShortName(), "Namespaced name": v.GetNamespacedName(), "Description": v.GetDescription()}})
		}
	}
	values.Total = len(values.Items)
	return console.Detail{Summary: []console.Property{props[0], props[1], {Label: "Description", Value: orDash(k.GetDescription()), Edit: "Edit"}},
		Sections: []console.Section{
			{ID: "values", Label: "Values", Listing: values},
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Tag key", Properties: props}}},
		},
		Edit: tagDescriptionEdit(k.GetDescription(), "The short name and parent cannot change, as on Google."),
	}, nil
}

// tagDescriptionEdit is the edit panel Google's Edit next to a tag key's or
// value's Description opens (#1865): Description, then Save.
func tagDescriptionEdit(description, note string) *console.EditForm {
	return &console.EditForm{Label: "Edit", Title: "Edit description", Submit: "Save", Inline: true, Note: note,
		Fields: []console.Field{{Name: "description", Label: "Description", Type: "text", Default: description,
			Help: "At most 256 characters."}}}
}

func (p tagsProvider) Edit(_ context.Context, _ string, path []string, values map[string]string) error {
	switch len(path) {
	case 1:
		name := "tagKeys/" + path[0]
		if re, ok := values["allowedValuesRegex"]; ok && p.registry.TagKeyAllowedValuesRegex(name) != "" {
			_, err := p.registry.UpdateTagKeyWithRegex(&rmpb.TagKey{Name: name, Description: values["description"]}, strings.TrimSpace(re),
				[]string{"description", "allowed_values_regex"}, false)
			return err
		}
		_, err := p.registry.UpdateTagKey(&rmpb.TagKey{Name: name, Description: values["description"]}, []string{"description"}, false)
		return err
	case 2:
		_, err := p.registry.UpdateTagValue(&rmpb.TagValue{Name: "tagValues/" + path[1], Description: values["description"]}, []string{"description"}, false)
		return err
	}
	return fmt.Errorf("only a tag key or value can be edited")
}

const (
	tagCreateValue = "create-value"
	tagDeleteValue = "delete-value"
)

// DetailActions offers Create tag value and Delete tag value on a key's page.
func (p tagsProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	if len(path) == 2 {
		return []console.Action{{ID: tagDeleteValue, Label: "Delete tag value", Destructive: true}}
	}
	if len(path) != 1 || p.registry.TagKeyAllowedValuesRegex("tagKeys/"+path[0]) != "" {
		return nil
	}
	out := []console.Action{{ID: tagCreateValue, Label: "Create tag value", Fields: []console.Field{
		{Name: "shortName", Label: "Short name", Type: "text", Required: true, Pattern: tagShortNamePattern,
			Help: "1 to 256 characters, starting and ending with a letter or digit; unique within the key."},
		{Name: "description", Label: "Description", Type: "text", Help: "Optional, at most 256 characters."},
	}}}
	if vs, err := p.registry.TagValues("tagKeys/" + path[0]); err == nil && len(vs) > 0 {
		ids := make([]string, 0, len(vs))
		for _, v := range vs {
			ids = append(ids, v.GetShortName())
		}
		out = append(out, console.Action{ID: tagDeleteValue, Label: "Delete tag value", Destructive: true, Fields: []console.Field{
			{Name: "value", Label: "Value", Type: "select", Required: true, Options: ids, Default: ids[0],
				Help: "A value bound to a resource, or held, is not deleted."}}})
	}
	return out
}

func (p tagsProvider) ActAt(_ context.Context, _ string, path []string, action string, values map[string]string) error {
	if len(path) == 0 {
		return fmt.Errorf("unknown action %q", action)
	}
	k, err := p.registry.TagKey("tagKeys/" + path[0])
	if err != nil {
		return err
	}
	switch action {
	case tagCreateValue:
		_, err := p.registry.CreateTagValue(&rmpb.TagValue{Parent: k.GetName(), ShortName: strings.TrimSpace(values["shortName"]),
			Description: values["description"]}, false)
		return err
	case tagDeleteValue:
		name := ""
		if len(path) == 2 {
			name = "tagValues/" + path[1]
		} else {
			v, err := p.registry.NamespacedTagValue(k.GetNamespacedName() + "/" + values["value"])
			if err != nil {
				return err
			}
			name = v.GetName()
		}
		_, err := p.registry.DeleteTagValue(name, "", false)
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

var _ console.PathActor = tagsProvider{}
var _ console.Creator = tagsProvider{}
var _ console.Deleter = tagsProvider{}
var _ console.Driller = tagsProvider{}
var _ console.Editor = tagsProvider{}

// --- bindings on a project's or folder's page ---------------------------

const (
	tagBind        = "bind-tag"
	tagBindDynamic = "bind-dynamic-tag"
	tagUnbind      = "unbind-tag"
)

// dynamicTagKeys are the keys of dynamic values (#2023) by namespaced name,
// each with its validation regular expression.
func dynamicTagKeys(reg *resourcemanager.Registry) map[string]string {
	out := map[string]string{}
	if keys, err := reg.AllTagKeys(); err == nil {
		for _, k := range keys {
			if re := reg.TagKeyAllowedValuesRegex(k.GetName()); re != "" {
				out[k.GetNamespacedName()] = re
			}
		}
	}
	return out
}

// tagSection is a resource's effective tags: its own bindings and the ones
// its folders and the organization give it.
func tagSection(reg *resourcemanager.Registry, target string) console.Section {
	l := console.Listing{NameColumn: "Tag value", Columns: []string{"Tag key", "Source"}, Noun: "tags",
		Note: "Stored, not enforced. Bind tag attaches a value here; a value of the same key bound nearer replaces an inherited one."}
	all, err := reg.EffectiveTags(target)
	if err != nil {
		l.Unavailable = consoleErr(err)
		return console.Section{ID: "tags", Label: "Tags", Listing: l}
	}
	for _, e := range all {
		src := "Bound here"
		if e.GetInherited() {
			src = "Inherited"
		}
		l.Items = append(l.Items, console.Resource{Name: e.GetNamespacedTagValue(), Fields: map[string]string{
			"Tag key": e.GetNamespacedTagKey(), "Source": src}})
	}
	l.Total = len(l.Items)
	return console.Section{ID: "tags", Label: "Tags", Listing: l}
}

// tagActions are Bind tag, and Unbind tag when something is bound here.
func tagActions(reg *resourcemanager.Registry, target string) []console.Action {
	var values []string
	if keys, err := reg.AllTagKeys(); err == nil {
		for _, k := range keys {
			if vs, err := reg.TagValues(k.GetName()); err == nil {
				for _, v := range vs {
					values = append(values, v.GetNamespacedName())
				}
			}
		}
	}
	sort.Strings(values)
	var out []console.Action
	if len(values) > 0 {
		out = append(out, console.Action{ID: tagBind, Label: "Bind tag", Fields: []console.Field{
			{Name: "value", Label: "Tag value", Type: "select", Required: true, Options: values, Default: values[0],
				Help: "Created on the Tags screen. A resource carries one value of each key."}}})
	}
	if keys := dynamicTagKeys(reg); len(keys) > 0 {
		names := make([]string, 0, len(keys))
		for n := range keys {
			names = append(names, n)
		}
		sort.Strings(names)
		labels := map[string]string{}
		for _, n := range names {
			labels[n] = n + " (matching " + keys[n] + ")"
		}
		out = append(out, console.Action{ID: tagBindDynamic, Label: "Bind dynamic tag", Fields: []console.Field{
			{Name: "key", Label: "Tag key", Type: "select", Required: true, Options: names, OptionLabels: labels, Default: names[0],
				Help: "A key of dynamic values: its value is typed here."},
			{Name: "dynamicValue", Label: "Tag value", Type: "text", Required: true,
				Help: "A value matching the key's validation regular expression."}}})
	}
	if bs, err := reg.TagBindings(target); err == nil && len(bs) > 0 {
		bound := make([]string, 0, len(bs))
		for _, b := range bs {
			bound = append(bound, b.GetTagValueNamespacedName())
		}
		out = append(out, console.Action{ID: tagUnbind, Label: "Unbind tag", Destructive: true, Fields: []console.Field{
			{Name: "value", Label: "Tag value", Type: "select", Required: true, Options: bound, Default: bound[0]}}})
	}
	return out
}

// tagAct performs a tag action; handled is false for any other action.
func tagAct(reg *resourcemanager.Registry, target, action string, values map[string]string) (handled bool, err error) {
	switch action {
	case tagBind:
		_, err := reg.CreateTagBinding(&rmpb.TagBinding{Parent: target, TagValueNamespacedName: values["value"]}, false)
		return true, err
	case tagBindDynamic:
		_, err := reg.CreateTagBinding(&rmpb.TagBinding{Parent: target,
			TagValueNamespacedName: values["key"] + "/" + strings.TrimSpace(values["dynamicValue"])}, false)
		return true, err
	case tagUnbind:
		bs, err := reg.TagBindings(target)
		if err != nil {
			return true, err
		}
		for _, b := range bs {
			if b.GetTagValueNamespacedName() == values["value"] {
				return true, reg.DeleteTagBinding(b.GetName())
			}
		}
		return true, fmt.Errorf("%s is not bound to %s", values["value"], target)
	}
	return false, nil
}

// --- liens on a project's page -------------------------------------------

const (
	lienAdd    = "add-lien"
	lienRemove = "remove-lien"
)

func lienSection(reg *resourcemanager.Registry, project string) console.Section {
	l := console.Listing{NameColumn: "Lien", Columns: []string{"Restrictions", "Reason", "Origin", "Created"}, Noun: "liens",
		Note: "A lien restricting " + resourcemanager.ProjectDeleteRestriction + " makes deleting this project fail, as on Google; " +
			"other restrictions are stored, not enforced."}
	all, err := reg.Liens("projects/" + project)
	if err != nil {
		l.Unavailable = consoleErr(err)
		return console.Section{ID: "liens", Label: "Liens", Listing: l}
	}
	for _, x := range all {
		l.Items = append(l.Items, console.Resource{Name: strings.TrimPrefix(x.Name, "liens/"), Fields: map[string]string{
			"Restrictions": strings.Join(x.Restrictions, ", "), "Reason": x.Reason, "Origin": x.Origin, "Created": consoleTime(x.CreateTime)}})
	}
	l.Total = len(l.Items)
	return console.Section{ID: "liens", Label: "Liens", Listing: l}
}

func lienActions(reg *resourcemanager.Registry, project string) []console.Action {
	out := []console.Action{{ID: lienAdd, Label: "Add lien", Fields: []console.Field{
		{Name: "restrictions", Label: "Restrictions", Type: "text", Required: true, Default: resourcemanager.ProjectDeleteRestriction,
			Help: "Comma-separated permissions. " + resourcemanager.ProjectDeleteRestriction + " blocks deleting the project."},
		{Name: "reason", Label: "Reason", Type: "text", Required: true, Help: "Why the lien is placed, at most 200 characters."},
		{Name: "origin", Label: "Origin", Type: "text", Required: true, Default: "cloudburrow-console",
			Help: "Who placed it, at most 200 characters."},
	}}}
	if all, err := reg.Liens("projects/" + project); err == nil && len(all) > 0 {
		ids := make([]string, 0, len(all))
		for _, x := range all {
			ids = append(ids, x.Name)
		}
		out = append(out, console.Action{ID: lienRemove, Label: "Remove lien", Destructive: true, Fields: []console.Field{
			{Name: "lien", Label: "Lien", Type: "select", Required: true, Options: ids, Default: ids[0]}}})
	}
	return out
}

func lienAct(reg *resourcemanager.Registry, project, action string, values map[string]string) (bool, error) {
	switch action {
	case lienAdd:
		var rs []string
		for _, s := range strings.Split(values["restrictions"], ",") {
			if s = strings.TrimSpace(s); s != "" {
				rs = append(rs, s)
			}
		}
		_, err := reg.CreateLien(resourcemanager.Lien{Parent: "projects/" + project, Restrictions: rs,
			Reason: strings.TrimSpace(values["reason"]), Origin: strings.TrimSpace(values["origin"])})
		return true, err
	case lienRemove:
		return true, reg.DeleteLien(values["lien"])
	}
	return false, nil
}
