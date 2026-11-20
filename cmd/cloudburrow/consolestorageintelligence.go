package main

// The Storage Intelligence screen (#1519), at /storage/intelligence: the
// IntelligenceConfig of the chosen project, of the local organization and
// of each active folder, read and set through the Storage Control API's
// REST mapping on the storage port (GetProjectIntelligenceConfig and its
// folder and organization variants, and their Update), the calls an
// application makes. A row's actions set its edition (STANDARD, TRIAL,
// DISABLED, or back to INHERIT); Edit filter sets its bucket and location
// filters. The configurations are stored, and nothing acts on them: the
// features Storage Intelligence enables are other APIs, not served here.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	urlpkg "net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

type storageIntelligenceProvider struct {
	endpoint string
	registry *resourcemanager.Registry
}

func (storageIntelligenceProvider) ID() string    { return "storage-intelligence" }
func (storageIntelligenceProvider) Title() string { return "Storage Intelligence" }

// intelligenceJSON is an IntelligenceConfig as the REST mapping returns it.
type intelligenceJSON struct {
	Name          string `json:"name"`
	EditionConfig string `json:"editionConfig"`
	UpdateTime    string `json:"updateTime"`
	Filter        *struct {
		IncludedCloudStorageLocations *struct{ Locations []string } `json:"includedCloudStorageLocations"`
		ExcludedCloudStorageLocations *struct{ Locations []string } `json:"excludedCloudStorageLocations"`
		IncludedCloudStorageBuckets   *struct {
			BucketIDRegexes []string `json:"bucketIdRegexes"`
		} `json:"includedCloudStorageBuckets"`
		ExcludedCloudStorageBuckets *struct {
			BucketIDRegexes []string `json:"bucketIdRegexes"`
		} `json:"excludedCloudStorageBuckets"`
	} `json:"filter"`
	Effective struct {
		EffectiveEdition   string `json:"effectiveEdition"`
		IntelligenceConfig string `json:"intelligenceConfig"`
	} `json:"effectiveIntelligenceConfig"`
	TrialConfig *struct {
		ExpireTime string `json:"expireTime"`
	} `json:"trialConfig"`
}

func (p storageIntelligenceProvider) url(scope string) string {
	return fmt.Sprintf("http://%s/v2/%s/locations/global/intelligenceConfig", p.endpoint, scope)
}

// scopes are the rows: the project, the organization and its active folders.
func (p storageIntelligenceProvider) scopes(project string) []string {
	out := []string{"projects/" + project, resourcemanager.OrganizationName}
	if p.registry != nil {
		if fs, err := p.registry.Folders(); err == nil {
			for _, f := range fs {
				if f.State == resourcemanager.StateActive {
					out = append(out, f.Name)
				}
			}
		}
	}
	return out
}

func filterText(c intelligenceJSON) string {
	if c.Filter == nil {
		return ""
	}
	var parts []string
	if f := c.Filter.IncludedCloudStorageLocations; f != nil {
		parts = append(parts, "locations in "+strings.Join(f.Locations, ", "))
	}
	if f := c.Filter.ExcludedCloudStorageLocations; f != nil {
		parts = append(parts, "locations not in "+strings.Join(f.Locations, ", "))
	}
	if f := c.Filter.IncludedCloudStorageBuckets; f != nil {
		parts = append(parts, "buckets matching "+strings.Join(f.BucketIDRegexes, ", "))
	}
	if f := c.Filter.ExcludedCloudStorageBuckets; f != nil {
		parts = append(parts, "buckets not matching "+strings.Join(f.BucketIDRegexes, ", "))
	}
	return strings.Join(parts, "; ")
}

func (p storageIntelligenceProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{
		Columns:      []string{"Effective edition", "Effective from", "Filter", "Trial ends", "Updated"},
		NameColumn:   "Resource",
		Noun:         "configurations",
		AlwaysStatus: true,
		Note: "Storage Intelligence's configuration of the project, the organization and each folder. " +
			"It is stored and returned as Google documents it; nothing here acts on it (Storage Insights " +
			"datasets and batch operations are not served).",
	}
	if project == "" {
		listing.Prompt = "Storage Intelligence is shown for a project. Choose one in the toolbar."
		return listing, nil
	}
	for _, scope := range p.scopes(project) {
		var c intelligenceJSON
		if err := storageCall(ctx, http.MethodGet, p.url(scope), nil, &c); err != nil {
			return console.Listing{}, fmt.Errorf("read %s's configuration: %w", scope, err)
		}
		trial := ""
		if c.TrialConfig != nil {
			trial = shortTime(c.TrialConfig.ExpireTime)
		}
		// Google's Configuration page's Overview: the project's Status,
		// Enabled or Disabled by its effective edition (#1843).
		if scope == "projects/"+project {
			status := "Disabled"
			if intelligenceEnabled(c) {
				status = "Enabled"
			}
			listing.Summary = []console.PropertyGroup{{Heading: "Overview", Properties: []console.Property{
				{Label: "Resource", Value: scope},
				{Label: "Status", Value: status, Action: disableLink(status), ActionText: disableText(status)},
				{Label: "Edition", Value: orDash(c.Effective.EffectiveEdition)},
				{Label: "Bucket filters", Value: orDash(filterText(c))},
			}}}
		}
		listing.Items = append(listing.Items, console.Resource{
			Name:   scope,
			Status: c.EditionConfig,
			Fields: map[string]string{
				"Effective edition": c.Effective.EffectiveEdition,
				"Effective from":    strings.TrimSuffix(c.Effective.IntelligenceConfig, "/locations/global/intelligenceConfig"),
				"Filter":            filterText(c),
				"Trial ends":        trial,
				"Updated":           shortTime(c.UpdateTime),
			},
		})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

// disableLink and disableText put Disable beside an enabled Status, as
// Google's Overview has it (#2047).
func disableLink(status string) string {
	if status == "Enabled" {
		return intelligenceDisableID
	}
	return ""
}

func disableText(status string) string {
	if status == "Enabled" {
		return "Disable"
	}
	return ""
}

// intelligenceEditions are the editions a row can be set to, by action ID.
var intelligenceEditions = []struct{ id, label, edition string }{
	{"standard", "Enable Storage Intelligence", "STANDARD"},
	{"trial", "Start trial", "TRIAL"},
	{"disable", "Disable", "DISABLED"},
	{"inherit", "Inherit from parent", "INHERIT"},
}

// Detail is a configuration's page, whose actions are the row's with Edit
// filter.
func (p storageIntelligenceProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	var c intelligenceJSON
	if err := storageCall(ctx, http.MethodGet, p.url(path[0]), nil, &c); err != nil {
		return console.Detail{Unavailable: "cannot read the configuration: " + err.Error()}, nil
	}
	trial := ""
	if c.TrialConfig != nil {
		trial = shortTime(c.TrialConfig.ExpireTime)
	}
	props := []console.Property{
		{Label: "Name", Value: c.Name},
		{Label: "Edition", Value: c.EditionConfig},
		{Label: "Effective edition", Value: c.Effective.EffectiveEdition},
		{Label: "Effective configuration", Value: c.Effective.IntelligenceConfig},
		{Label: "Filter", Value: filterText(c)},
		{Label: "Trial ends", Value: trial},
		{Label: "Updated", Value: shortTime(c.UpdateTime)},
	}
	shown := props[:0]
	for _, pr := range props {
		if pr.Value != "" {
			shown = append(shown, pr)
		}
	}
	props = shown
	return console.Detail{Summary: props[:2], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Configuration", Properties: props}}},
	}}, nil
}

// DetailActions implements console.PathActor.
func (p storageIntelligenceProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	var c intelligenceJSON
	if err := storageCall(ctx, http.MethodGet, p.url(path[0]), nil, &c); err != nil {
		return nil
	}
	return intelligenceActions(c.EditionConfig, true)
}

// ActAt implements console.PathActor. With no path it is the list
// screen's: Configure Storage Intelligence and the Overview's Disable.
func (p storageIntelligenceProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) == 0 {
		switch action {
		case intelligenceConfigureID:
			return p.configure(ctx, project, values)
		case intelligenceDisableID:
			return p.ActWith(ctx, project, "projects/"+project, "disable", nil)
		}
		return fmt.Errorf("%s is not an action of the Storage Intelligence page", action)
	}
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	return p.ActWith(ctx, project, path[0], action, values)
}

// Google's Configuration page (#2047,
// configure-and-manage-storage-intelligence): Enable Storage Intelligence
// opens the Configure Storage Intelligence dialog, the resource, Continue,
// optional bucket filters by location and by name (a regular expression
// at a time, Add regular expression), then Enable this configuration; and
// the Overview's Disable beside Status asks for Disable typed.
const (
	intelligenceConfigureID = "configure"
	intelligenceDisableID   = "disableproject"
	intelligenceResource    = "Resource"
	intelligenceFilters     = "Bucket filters"
)

// The bucket filter types.
const (
	filterNone    = "none"
	filterInclude = "include"
	filterExclude = "exclude"
)

// ListActions implements console.ListActor.
func (p storageIntelligenceProvider) ListActions(ctx context.Context, project string) []console.Action {
	if project == "" {
		return nil
	}
	scopes := p.scopes(project)
	labels := map[string]string{}
	for _, sc := range scopes {
		labels[sc] = sc
	}
	labels["projects/"+project] = "Project " + project
	labels[resourcemanager.OrganizationName] = "Organization (" + resourcemanager.OrganizationName + ")"
	kinds := func(noun string) (map[string]string, []string) {
		return map[string]string{
			filterNone:    "No " + noun + " filter",
			filterInclude: "Include buckets " + map[string]string{"location": "in these locations", "name": "whose names match"}[noun],
			filterExclude: "Exclude buckets " + map[string]string{"location": "in these locations", "name": "whose names match"}[noun],
		}, []string{filterNone, filterInclude, filterExclude}
	}
	locLabels, locOpts := kinds("location")
	nameLabels, nameOpts := kinds("name")
	out := []console.Action{{ID: intelligenceConfigureID, Label: "Enable Storage Intelligence",
		Title: "Configure Storage Intelligence", Steps: "continue", Submit: "Enable this configuration",
		Fields: []console.Field{
			{Name: "resource", Label: "Resource", Type: "select", Required: true, Section: intelligenceResource,
				Options: scopes, OptionLabels: labels, Default: "projects/" + project,
				Help: "The project, folder or organization to configure. Its edition becomes Standard; " +
					"what it sets is inherited by the resources under it that inherit from their parent."},
			{Name: "locationFilter", Label: "Filter by location", Type: "select", Section: intelligenceFilters,
				Options: locOpts, OptionLabels: locLabels, Default: filterNone},
			{Name: "locations", Label: "Locations", Type: console.GroupFieldType, Section: intelligenceFilters,
				AddLabel: "Add location", ShowWhen: "locationFilter=" + filterInclude + "|" + filterExclude,
				Group: []console.Field{{Name: "location", Label: "Location", Type: "text", Pattern: `[a-zA-Z0-9\-]+`,
					Help: "A Cloud Storage location, such as us-central1, us or eu."}}},
			{Name: "bucketFilter", Label: "Filter by bucket name", Type: "select", Section: intelligenceFilters,
				Options: nameOpts, OptionLabels: nameLabels, Default: filterNone},
			{Name: "regexes", Label: "Bucket name regular expressions", Type: console.GroupFieldType, Section: intelligenceFilters,
				AddLabel: "Add regular expression", ShowWhen: "bucketFilter=" + filterInclude + "|" + filterExclude,
				Group: []console.Field{{Name: "regex", Label: "Regular expression", Type: "text",
					Help: "An RE2 expression matched against bucket names, such as ^logs-."}}},
		}}}
	var c intelligenceJSON
	if err := storageCall(ctx, http.MethodGet, p.url("projects/"+project), nil, &c); err == nil && intelligenceEnabled(c) {
		out = append(out, intelligenceDisableAction(intelligenceDisableID, true))
	}
	return out
}

// intelligenceEnabled is the Overview's Status: Enabled while the effective
// edition is Standard or a trial.
func intelligenceEnabled(c intelligenceJSON) bool {
	e := c.Effective.EffectiveEdition
	return e == "STANDARD" || e == "TRIAL"
}

// intelligenceDisableAction is Disable, confirmed by typing Disable.
func intelligenceDisableAction(id string, linked bool) console.Action {
	return console.Action{ID: id, Label: "Disable", Linked: linked, ConfirmTitle: "Disable Storage Intelligence?",
		ConfirmStyle: console.DeleteConfirmDisable, Submit: "Disable",
		Confirm: "The edition becomes Disabled: Storage Intelligence stops for this resource, and the resources " +
			"under it that inherit from their parent."}
}

// configure is Enable this configuration: the resource's edition STANDARD
// with the dialog's filter, in one Update...IntelligenceConfig.
func (p storageIntelligenceProvider) configure(ctx context.Context, project string, values map[string]string) error {
	if project == "" {
		return errors.New("choose a project")
	}
	resource := strings.TrimSpace(values["resource"])
	known := false
	for _, s := range p.scopes(project) {
		known = known || s == resource
	}
	if !known {
		return fmt.Errorf("%q is not a resource this page configures", resource)
	}
	rows := func(field, sub string) ([]string, error) {
		parsed, err := console.ParseGroup(values[field])
		if err != nil {
			return nil, err
		}
		var out []string
		for _, r := range parsed {
			if v := strings.TrimSpace(r[sub]); v != "" {
				out = append(out, v)
			}
		}
		return out, nil
	}
	filter := map[string]any{}
	add := func(kind, field, sub, included, excluded, key, noun string) error {
		if kind == "" || kind == filterNone {
			return nil
		}
		vals, err := rows(field, sub)
		if err != nil {
			return err
		}
		if len(vals) == 0 {
			return fmt.Errorf("add a %s, or choose no filter", noun)
		}
		switch kind {
		case filterInclude:
			filter[included] = map[string]any{key: vals}
		case filterExclude:
			filter[excluded] = map[string]any{key: vals}
		default:
			return fmt.Errorf("%q is not a filter type", kind)
		}
		return nil
	}
	if err := add(values["locationFilter"], "locations", "location", "includedCloudStorageLocations",
		"excludedCloudStorageLocations", "locations", "location"); err != nil {
		return err
	}
	if err := add(values["bucketFilter"], "regexes", "regex", "includedCloudStorageBuckets",
		"excludedCloudStorageBuckets", "bucketIdRegexes", "regular expression"); err != nil {
		return err
	}
	body := map[string]any{"editionConfig": "STANDARD"}
	if len(filter) > 0 {
		body["filter"] = filter
	}
	return storageCall(ctx, http.MethodPatch, p.url(resource)+"?updateMask="+urlpkg.QueryEscape("edition_config,filter"), body, nil)
}

// Actions implements console.Actor.
func (storageIntelligenceProvider) Actions(r console.Resource) []console.Action {
	return intelligenceActions(r.Status, false)
}

// intelligenceActions are every edition but the current one, and, on a
// configuration's page, Edit filter while an edition that takes one is set.
func intelligenceActions(edition string, page bool) []console.Action {
	r := console.Resource{Status: edition}
	var out []console.Action
	for _, e := range intelligenceEditions {
		if e.edition == r.Status {
			continue
		}
		if e.id == "disable" {
			out = append(out, intelligenceDisableAction(e.id, false))
			continue
		}
		out = append(out, console.Action{ID: e.id, Label: e.label})
	}
	if page && (r.Status == "STANDARD" || r.Status == "TRIAL") {
		out = append(out, console.Action{ID: "filter", Label: "Edit filter", Fields: []console.Field{
			{Name: "includeLocations", Label: "Include locations", Type: "text", Help: "Comma-separated, such as us-central1,us-east1. Leave both location fields empty for every location."},
			{Name: "excludeLocations", Label: "Exclude locations", Type: "text", Help: "Comma-separated; not with Include locations."},
			{Name: "includeBuckets", Label: "Include bucket ID regexes", Type: "text", Help: "Comma-separated RE2 expressions."},
			{Name: "excludeBuckets", Label: "Exclude bucket ID regexes", Type: "text", Help: "Comma-separated; not with Include bucket ID regexes."},
		}})
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Act implements console.Actor.
func (p storageIntelligenceProvider) Act(ctx context.Context, project, name, action string) error {
	return p.ActWith(ctx, project, name, action, nil)
}

// ActWith sets name's configuration: an edition is
// Update...IntelligenceConfig with update_mask edition_config,filter (the
// filter cleared, as gcloud does), and Edit filter is the filter alone.
func (p storageIntelligenceProvider) ActWith(ctx context.Context, project, name, action string, values map[string]string) error {
	if project == "" {
		return errors.New("choose a project")
	}
	known := false
	for _, s := range p.scopes(project) {
		known = known || s == name
	}
	if !known {
		return fmt.Errorf("%s is not a row of this screen", name)
	}
	if action == "filter" {
		filter := map[string]any{}
		inL, exL := splitList(values["includeLocations"]), splitList(values["excludeLocations"])
		inB, exB := splitList(values["includeBuckets"]), splitList(values["excludeBuckets"])
		if len(inL) > 0 && len(exL) > 0 || len(inB) > 0 && len(exB) > 0 {
			return errors.New("a filter includes or excludes locations, and includes or excludes buckets, not both")
		}
		if len(inL) > 0 {
			filter["includedCloudStorageLocations"] = map[string]any{"locations": inL}
		}
		if len(exL) > 0 {
			filter["excludedCloudStorageLocations"] = map[string]any{"locations": exL}
		}
		if len(inB) > 0 {
			filter["includedCloudStorageBuckets"] = map[string]any{"bucketIdRegexes": inB}
		}
		if len(exB) > 0 {
			filter["excludedCloudStorageBuckets"] = map[string]any{"bucketIdRegexes": exB}
		}
		return storageCall(ctx, http.MethodPatch, p.url(name)+"?updateMask=filter", map[string]any{"filter": filter}, nil)
	}
	for _, e := range intelligenceEditions {
		if e.id == action {
			return storageCall(ctx, http.MethodPatch, p.url(name)+"?updateMask="+urlpkg.QueryEscape("edition_config,filter"),
				map[string]any{"editionConfig": e.edition}, nil)
		}
	}
	return fmt.Errorf("%s is not a Storage Intelligence action", action)
}

var (
	_ console.Actor     = storageIntelligenceProvider{}
	_ console.PathActor = storageIntelligenceProvider{}
	_ console.ListActor = storageIntelligenceProvider{}
	_ console.Driller   = storageIntelligenceProvider{}
)
