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

// intelligenceEditions are the editions a row can be set to, by action ID.
var intelligenceEditions = []struct{ id, label, edition string }{
	{"standard", "Enable (Standard)", "STANDARD"},
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

// ActAt implements console.PathActor.
func (p storageIntelligenceProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	return p.ActWith(ctx, project, path[0], action, values)
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
		if e.edition != r.Status {
			out = append(out, console.Action{ID: e.id, Label: e.label})
		}
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
	_ console.Driller   = storageIntelligenceProvider{}
)
