package main

import (
	"context"
	"fmt"
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
// project or folder they tag (tagSection, tagActions). Tags are stored, not
// enforced: no IAM condition or organization policy reads them here.
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

func (p tagsProvider) List(context.Context, string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Tag key ID", Columns: []string{"Short name", "Namespaced name", "Parent", "Values", "Description"},
		Noun: "tag keys", Note: tagsNote}
	keys, err := p.registry.AllTagKeys()
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, k := range keys {
		n := 0
		if vs, err := p.registry.TagValues(k.GetName()); err == nil {
			n = len(vs)
		}
		l.Items = append(l.Items, console.Resource{Name: tagID(k.GetName()), Fields: map[string]string{
			"Short name": k.GetShortName(), "Namespaced name": k.GetNamespacedName(), "Parent": k.GetParent(),
			"Values": strconv.Itoa(n), "Description": k.GetDescription()}})
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

func (p tagsProvider) CreateForm() (string, []console.Field) {
	return "Create tag key", []console.Field{
		{Name: "parent", Label: "Parent", Type: "select", Required: true, Options: p.parentOptions(), Default: resourcemanager.OrganizationName,
			Help: "The organization, or a project: the key's namespace."},
		{Name: "shortName", Label: "Short name", Type: "text", Required: true, Pattern: tagShortNamePattern,
			Help: "1 to 256 characters, starting and ending with a letter or digit; unique under its parent."},
		{Name: "description", Label: "Description", Type: "text", Help: "Optional, at most 256 characters."},
	}
}

func (p tagsProvider) Create(_ context.Context, _ string, values map[string]string) (string, error) {
	k, err := p.registry.CreateTagKey(&rmpb.TagKey{Parent: values["parent"], ShortName: strings.TrimSpace(values["shortName"]),
		Description: values["description"]}, false)
	if err != nil {
		return "", err
	}
	return tagID(k.GetName()), nil
}

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
		return console.Detail{Summary: props[:2],
			Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{Heading: "Tag value", Properties: props}}}},
			Edit: &console.EditForm{Label: "Edit description", Fields: []console.Field{
				{Name: "description", Label: "Description", Type: "text", Default: v.GetDescription()}}},
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
	return console.Detail{Summary: props[:2],
		Sections: []console.Section{
			{ID: "values", Label: "Values", Listing: values},
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Tag key", Properties: props}}},
		},
		Edit: &console.EditForm{Label: "Edit description", Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: k.GetDescription()}},
			Note: "The short name and parent cannot change, as on Google."},
	}, nil
}

func (p tagsProvider) Edit(_ context.Context, _ string, path []string, values map[string]string) error {
	switch len(path) {
	case 1:
		_, err := p.registry.UpdateTagKey(&rmpb.TagKey{Name: "tagKeys/" + path[0], Description: values["description"]}, []string{"description"}, false)
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
	if len(path) != 1 {
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
	tagBind   = "bind-tag"
	tagUnbind = "unbind-tag"
)

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
