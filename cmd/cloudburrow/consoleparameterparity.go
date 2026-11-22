package main

// Parameter Manager's console laid out as Google's how-to pages describe
// it (#2143, read 2026-11-22; pages under
// https://docs.cloud.google.com/secret-manager/parameter-manager/docs/):
// Create parameter's Parameter format and Location sections
// (create-parameter), Create template's Template format
// (create-parameter-template), Add label rows; the list's Actions menu
// with Add new version, Edit and Delete; Create parameter version and
// Create template version as pages (add-parameter-version,
// add-parameter-template-version); Edit parameter and Edit parameter
// template as pages ending in Save (edit-parameter,
// edit-parameter-template); a version row's Actions menu with View
// payload, Render, Disable or Enable and Delete
// (view-parameter-version-details); a version's Delete asking for the
// parameter's (or template's) ID (delete-parameter-version,
// delete-parameter-template-version); Disable and Enable asking for
// nothing typed (disable-parameter-versions, enable-disabled-versions).

import (
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/parametermanager"
)

// The routes of the two pages, for the rows' links.
const (
	parametersRoute = "/secrets-parameters"
	templatesRoute  = "/secrets-parameters/templates"
)

// pmVersionIDPattern is Google's version name: "63 characters or less",
// letters, numbers, - and _, "cannot begin with a dash".
const pmVersionIDPattern = `^[A-Za-z0-9_][A-Za-z0-9_\-]{0,62}$`

const pmVersionIDHelp = "Letters, numbers, hyphens and underscores; at most 63 characters, not beginning with a hyphen."

// pmLocationFields are Google's Location section: Multi-region, whose
// Region list holds global (Global), or Region with a region.
func pmLocationFields() []console.Field {
	var regions []string
	for _, l := range parametermanager.Locations() {
		if l != "global" {
			regions = append(regions, l)
		}
	}
	region := "us-central1"
	if len(regions) > 0 && !pmHas(regions, region) {
		region = regions[0]
	}
	return []console.Field{
		{Name: "locationType", Label: "Location type", Type: "radio", Default: "multi", Section: "Location",
			Options: []string{"multi", "region"}, OptionLabels: map[string]string{"multi": "Multi-region", "region": "Region"}},
		{Name: "multiRegion", Label: "Region", Type: "select", Default: "global", Section: "Location", Options: []string{"global"},
			OptionLabels: map[string]string{"global": "global (Global)"}, VisibleWhen: map[string]string{"locationType": "multi"}},
		{Name: "region", Label: "Region", Type: "select", Default: region, Section: "Location", Options: regions,
			VisibleWhen: map[string]string{"locationType": "region"}, Help: "A regional parameter is stored and served in this region only."},
	}
}

func pmHas(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// pmLocation is the location the Location section chose; an older form's
// single location field still works.
func pmLocation(v map[string]string) string {
	switch v["locationType"] {
	case "region":
		return strings.TrimSpace(v["region"])
	case "multi":
		return "global"
	}
	return strings.TrimSpace(v["location"])
}

// pmLabelsField is Labels' Add label rows.
func pmLabelsField(labels map[string]string) console.Field {
	return secretRowsField("labels", "Labels", "Add label", "Key", "Value", console.FormatMap(labels))
}

// pmLabels reads the Labels rows, or an older form's key=value map.
func pmLabels(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "[") {
		return console.ParseMap(raw)
	}
	rows, err := console.ParseGroup(raw)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, r := range rows {
		if k := strings.TrimSpace(r["key"]); k != "" {
			m[k] = strings.TrimSpace(r["value"])
		}
	}
	return m, nil
}

// pmRowActions are a parameter's or template's Actions menu on the list:
// Add new version, Edit and Delete, each opened on its page.
func pmRowActions(route, name string) []console.Action {
	page := route + "/" + url.PathEscape(name)
	return []console.Action{
		{ID: "row-newversion", Label: "Add new version", Href: page + "?action=newversion"},
		{ID: "row-edit", Label: "Edit", Href: page + "?action=edit"},
		{ID: "row-delete", Label: "Delete", Href: page + "?action=delete"},
	}
}

// pmVersionRowActions are a version row's Actions menu: View payload and
// Render, on the version's page, then Disable or Enable and Delete.
func pmVersionRowActions(route, parent, id string, disabled bool, owner string) []console.Action {
	page := route + "/" + url.PathEscape(parent) + "/" + url.PathEscape(id)
	render := console.Action{ID: "row-render", Label: "Render", Href: page + "?tab=rendered"}
	if route == templatesRoute {
		render.Href = page + "?action=render"
	}
	acts := []console.Action{{ID: "row-view", Label: "View payload", Href: page + "?tab=payload"}, render}
	del := pmDeleteVersionAction(owner)
	del.Leaves = false // the row's: the page stays
	return append(acts, pmToggleAction(disabled), del)
}

// pmToggleAction is Disable or Enable, confirmed by its own button.
func pmToggleAction(disabled bool) console.Action {
	if disabled {
		return console.Action{ID: "enable", Label: "Enable", ConfirmTitle: "Enable version?", ConfirmStyle: console.DeleteConfirmClick,
			Confirm: "The version can be read and rendered again."}
	}
	return console.Action{ID: "disable", Label: "Disable", ConfirmTitle: "Disable version?", ConfirmStyle: console.DeleteConfirmClick,
		Confirm: "A disabled version's payload is not returned and it cannot be rendered until it is enabled again."}
}

// pmDeleteVersionAction is a version's Delete, the owner's ID typed back.
func pmDeleteVersionAction(owner string) console.Action {
	return console.Action{ID: "deleteversion", Label: "Delete", Destructive: true, Leaves: true, ConfirmWord: lastSegment(owner),
		Confirm: "Deleting a version cannot be undone."}
}
