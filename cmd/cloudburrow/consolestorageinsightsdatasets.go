package main

// Cloud Storage > Storage Insights datasets (#1976): Storage Insights
// dataset configurations (storageinsights.googleapis.com v1), as Google's
// Storage Insights page lists them, with Create dataset configuration
// (its ID, location, scope of projects, folders or an organization, bucket
// and location filters, retention and service agent), each
// configuration's page with its BigQuery linked dataset, Link dataset and
// Unlink dataset, Edit and Delete. Snapshots are taken on the instance's
// clock, which Advance clock (CloudBurrow extension) moves.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	urlpkg "net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

type storageInsightsDatasetsProvider struct {
	endpoint string // the Storage Insights API's host address
}

func (storageInsightsDatasetsProvider) ID() string    { return "storage-insights-datasets" }
func (storageInsightsDatasetsProvider) Title() string { return "Insights datasets" }

// insightsDatasetLocations are the locations Google lets a linked dataset
// be in, where dataset configurations are made.
var insightsDatasetLocations = []string{"us", "eu", "asia-south1", "asia-south2", "asia-southeast1", "europe-west1",
	"us-central1", "us-east1", "us-east4"}

type insightsBucketFilter struct {
	BucketName        string `json:"bucketName,omitempty"`
	BucketPrefixRegex string `json:"bucketPrefixRegex,omitempty"`
}

type insightsBuckets struct {
	CloudStorageBuckets []insightsBucketFilter `json:"cloudStorageBuckets"`
}

type insightsLocs struct {
	Locations []string `json:"locations"`
}

// datasetConfigJSON is a DatasetConfig as the REST API answers it.
type datasetConfigJSON struct {
	Name           string `json:"name"`
	UID            string `json:"uid"`
	Description    string `json:"description"`
	CreateTime     string `json:"createTime"`
	UpdateTime     string `json:"updateTime"`
	State          string `json:"datasetConfigState"`
	Retention      int    `json:"retentionPeriodDays"`
	Activity       *int   `json:"activityDataRetentionPeriodDays"`
	Organization   string `json:"organizationNumber"`
	NewBuckets     bool   `json:"includeNewlyCreatedBuckets"`
	SourceProjects *struct {
		ProjectNumbers []string `json:"projectNumbers"`
	} `json:"sourceProjects"`
	SourceFolders *struct {
		FolderNumbers []string `json:"folderNumbers"`
	} `json:"sourceFolders"`
	OrganizationScope bool             `json:"organizationScope"`
	IncludeLocations  *insightsLocs    `json:"includeCloudStorageLocations"`
	ExcludeLocations  *insightsLocs    `json:"excludeCloudStorageLocations"`
	IncludeBuckets    *insightsBuckets `json:"includeCloudStorageBuckets"`
	ExcludeBuckets    *insightsBuckets `json:"excludeCloudStorageBuckets"`
	Identity          *struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"identity"`
	Link *struct {
		Dataset string `json:"dataset"`
		Linked  bool   `json:"linked"`
	} `json:"link"`
}

func (c datasetConfigJSON) location() string {
	if parts := strings.Split(c.Name, "/"); len(parts) >= 4 {
		return parts[3]
	}
	return ""
}

func (c datasetConfigJSON) scope() string {
	switch {
	case c.SourceProjects != nil:
		return "Projects " + strings.Join(c.SourceProjects.ProjectNumbers, ", ")
	case c.SourceFolders != nil:
		return "Folders " + strings.Join(c.SourceFolders.FolderNumbers, ", ")
	case c.OrganizationScope:
		return "Organization " + c.Organization
	}
	return ""
}

func (c datasetConfigJSON) linked() string {
	if c.Link != nil && c.Link.Linked {
		return c.Link.Dataset
	}
	return ""
}

func describeBuckets(b *insightsBuckets) string {
	if b == nil {
		return ""
	}
	var out []string
	for _, f := range b.CloudStorageBuckets {
		if f.BucketName != "" {
			out = append(out, f.BucketName)
		} else {
			out = append(out, "regex "+f.BucketPrefixRegex)
		}
	}
	return strings.Join(out, ", ")
}

func (c datasetConfigJSON) filters() (string, string) {
	locs, buckets := "All locations", "All buckets"
	if c.IncludeLocations != nil {
		locs = "Include " + strings.Join(c.IncludeLocations.Locations, ", ")
	}
	if c.ExcludeLocations != nil {
		locs = "Exclude " + strings.Join(c.ExcludeLocations.Locations, ", ")
	}
	if c.IncludeBuckets != nil {
		buckets = "Include " + describeBuckets(c.IncludeBuckets)
	}
	if c.ExcludeBuckets != nil {
		buckets = "Exclude " + describeBuckets(c.ExcludeBuckets)
	}
	return locs, buckets
}

func (p storageInsightsDatasetsProvider) parent(project, location string) string {
	return "http://" + p.endpoint + "/v1/projects/" + urlpkg.PathEscape(project) + "/locations/" + urlpkg.PathEscape(location)
}

func (p storageInsightsDatasetsProvider) configs(ctx context.Context, project string) ([]datasetConfigJSON, error) {
	var out []datasetConfigJSON
	for _, l := range insightsDatasetLocations {
		cs, err := listPages[datasetConfigJSON](ctx, p.parent(project, l)+"/datasetConfigs?pageSize=100", "datasetConfigs")
		if err != nil {
			return nil, fmt.Errorf("list %s's dataset configurations: %w", l, err)
		}
		out = append(out, cs...)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	return out, nil
}

func (p storageInsightsDatasetsProvider) config(ctx context.Context, project, id string) (datasetConfigJSON, error) {
	cs, err := p.configs(ctx, project)
	if err != nil {
		return datasetConfigJSON{}, err
	}
	for _, c := range cs {
		if lastSegment(c.Name) == id {
			return c, nil
		}
	}
	return datasetConfigJSON{}, fmt.Errorf("no dataset configuration %s in project %s", id, project)
}

func (p storageInsightsDatasetsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{
		Columns:    []string{"Location", "Scope", "Retention", "Linked dataset", "Created"},
		NameColumn: "Dataset configuration",
		Noun:       "dataset configurations",
		Note: "Storage Insights datasets: daily snapshots of the scope's project, bucket and object metadata, which a linked " +
			"dataset in this instance's BigQuery exposes as Google's views. Snapshots follow the instance's clock " +
			"(Advance clock moves it).",
	}
	if project == "" {
		listing.Prompt = "Dataset configurations are shown for a project. Choose one in the toolbar."
		return listing, nil
	}
	cs, err := p.configs(ctx, project)
	if err != nil {
		return console.Listing{}, err
	}
	for _, c := range cs {
		listing.Items = append(listing.Items, console.Resource{Name: lastSegment(c.Name), Status: c.State, Fields: map[string]string{
			"Location": c.location(), "Scope": c.scope(), "Retention": fmt.Sprintf("%d days", c.Retention),
			"Linked dataset": orDash(c.linked()), "Created": shortTime(c.CreateTime)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (storageInsightsDatasetsProvider) CreateForm() (string, []console.Field) {
	const (
		identify = "Name your dataset configuration"
		scope    = "Define the dataset scope"
		filters  = "Filter buckets"
		settings = "Retention and service agent"
	)
	return "Create dataset configuration", []console.Field{
		{Name: "id", Label: "Dataset configuration name", Type: "text", Required: true, Section: identify,
			Pattern: `^[A-Za-z][A-Za-z0-9_]{0,127}$`,
			Help:    "Up to 128 letters, numbers and underscores, starting with a letter. It cannot be changed."},
		{Name: "description", Label: "Description", Type: "text", Section: identify, Pattern: `^.{0,256}$`, Help: "Up to 256 characters."},
		{Name: "location", Label: "Location", Type: "select", Required: true, Section: identify, Default: "us-central1",
			Options: insightsDatasetLocations, Help: "Where the linked BigQuery dataset is. Source buckets can be in any location."},
		{Name: "scope", Label: "Scope", Type: "radio", Section: scope, Default: "projects",
			Options:      []string{"projects", "folders", "organization"},
			OptionLabels: map[string]string{"projects": "Projects", "folders": "Folders", "organization": "Organization"}},
		{Name: "projects", Label: "Project numbers", Type: "textarea", Section: scope, ShowWhen: "scope=projects",
			Help: "One project number per line, up to 10,000."},
		{Name: "folders", Label: "Folder numbers", Type: "textarea", Section: scope, ShowWhen: "scope=folders",
			Help: "One folder number per line, up to 10,000."},
		{Name: "organization", Label: "Organization number", Type: "text", Section: scope, Pattern: `^[0-9]*$`,
			Help: "Required for an organization scope; with projects or folders, those outside it are left out."},
		{Name: "locationFilter", Label: "Bucket locations", Type: "radio", Section: filters, Default: "all",
			Options:      []string{"all", "include", "exclude"},
			OptionLabels: map[string]string{"all": "All locations", "include": "Include", "exclude": "Exclude"}},
		{Name: "locations", Label: "Locations", Type: "text", Section: filters,
			Help: "With Include or Exclude: comma-separated, such as us-central1, eu."},
		{Name: "bucketFilter", Label: "Bucket names", Type: "radio", Section: filters, Default: "all",
			Options:      []string{"all", "include", "exclude"},
			OptionLabels: map[string]string{"all": "All buckets", "include": "Include", "exclude": "Exclude"}},
		{Name: "buckets", Label: "Buckets", Type: "textarea", Section: filters,
			Help: "With Include or Exclude, one per line: a bucket name, or regex:PATTERN for a bucket name prefix regular expression (RE2)."},
		{Name: "newBuckets", Label: "Include newly created buckets", Type: "checkbox", Section: filters, Default: "true"},
		{Name: "retention", Label: "Retention period (days)", Type: "number", Required: true, Section: settings, Default: "30",
			Help: "1 to 90 days of snapshots."},
		{Name: "activity", Label: "Activity data retention period (days)", Type: "number", Section: settings,
			Help: "0 to 365; empty uses the retention period."},
		{Name: "identity", Label: "Service agent", Type: "radio", Section: settings, Default: "IDENTITY_TYPE_PER_CONFIG",
			Options: []string{"IDENTITY_TYPE_PER_CONFIG", "IDENTITY_TYPE_PER_PROJECT"},
			OptionLabels: map[string]string{"IDENTITY_TYPE_PER_CONFIG": "Configuration-scoped",
				"IDENTITY_TYPE_PER_PROJECT": "Project-scoped"}},
	}
}

// CreateOnPage implements console.PageCreator.
func (storageInsightsDatasetsProvider) CreateOnPage() bool { return true }

// CreateTitle implements console.CreateTitler.
func (storageInsightsDatasetsProvider) CreateTitle() string { return "Create dataset configuration" }

var insightsConfigID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,127}$`)

func numberLines(s, what string) ([]string, error) {
	var out []string
	for _, l := range textLines(strings.ReplaceAll(s, ",", "\n")) {
		if _, err := strconv.ParseInt(l, 10, 64); err != nil {
			return nil, fmt.Errorf("%q is not a %s number", l, what)
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("give at least one %s number", what)
	}
	return out, nil
}

// datasetFilters adds the form's location and bucket filters to body.
func datasetFilters(body map[string]any, values map[string]string) error {
	if f := values["locationFilter"]; f == "include" || f == "exclude" {
		var locs []string
		for _, l := range strings.Split(values["locations"], ",") {
			if l = strings.TrimSpace(l); l != "" {
				locs = append(locs, strings.ToLower(l))
			}
		}
		if len(locs) == 0 {
			return errors.New("give at least one location to " + f)
		}
		body[f+"CloudStorageLocations"] = map[string]any{"locations": locs}
	}
	if f := values["bucketFilter"]; f == "include" || f == "exclude" {
		var bs []map[string]string
		for _, l := range textLines(values["buckets"]) {
			if re, ok := strings.CutPrefix(l, "regex:"); ok {
				bs = append(bs, map[string]string{"bucketPrefixRegex": re})
			} else {
				bs = append(bs, map[string]string{"bucketName": gsBucket(l)})
			}
		}
		if len(bs) == 0 {
			return errors.New("give at least one bucket to " + f)
		}
		body[f+"CloudStorageBuckets"] = map[string]any{"cloudStorageBuckets": bs}
	}
	return nil
}

func datasetRetention(body map[string]any, values map[string]string) error {
	n, err := strconv.Atoi(strings.TrimSpace(values["retention"]))
	if err != nil || n < 1 || n > 90 {
		return errors.New("the retention period is 1 to 90 days")
	}
	body["retentionPeriodDays"] = n
	if a := strings.TrimSpace(values["activity"]); a != "" {
		m, err := strconv.Atoi(a)
		if err != nil || m < 0 || m > 365 {
			return errors.New("the activity data retention period is 0 to 365 days")
		}
		body["activityDataRetentionPeriodDays"] = m
	}
	return nil
}

func (p storageInsightsDatasetsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project")
	}
	id := strings.TrimSpace(values["id"])
	if !insightsConfigID.MatchString(id) {
		return "", errors.New("the name is up to 128 letters, numbers and underscores, starting with a letter")
	}
	loc := values["location"]
	if loc == "" {
		loc = "us-central1"
	}
	body := map[string]any{"includeNewlyCreatedBuckets": values["newBuckets"] == "true",
		"identity": map[string]any{"type": values["identity"]}}
	if values["identity"] == "" {
		body["identity"] = map[string]any{"type": "IDENTITY_TYPE_PER_CONFIG"}
	}
	if d := strings.TrimSpace(values["description"]); d != "" {
		body["description"] = d
	}
	if o := strings.TrimSpace(values["organization"]); o != "" {
		body["organizationNumber"] = o
	}
	switch values["scope"] {
	case "folders":
		n, err := numberLines(values["folders"], "folder")
		if err != nil {
			return "", err
		}
		body["sourceFolders"] = map[string]any{"folderNumbers": n}
	case "organization":
		if body["organizationNumber"] == nil {
			return "", errors.New("give the organization number")
		}
		body["organizationScope"] = true
	default:
		n, err := numberLines(values["projects"], "project")
		if err != nil {
			return "", err
		}
		body["sourceProjects"] = map[string]any{"projectNumbers": n}
	}
	if err := datasetFilters(body, values); err != nil {
		return "", err
	}
	if err := datasetRetention(body, values); err != nil {
		return "", err
	}
	if err := storageCall(ctx, http.MethodPost, p.parent(project, loc)+"/datasetConfigs?datasetConfigId="+urlpkg.QueryEscape(id),
		body, nil); err != nil {
		return "", err
	}
	return id, nil
}

func (p storageInsightsDatasetsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.config(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the dataset configuration: " + err.Error()}, nil
	}
	locs, buckets := c.filters()
	activity := "Same as the retention period"
	if c.Activity != nil {
		activity = fmt.Sprintf("%d days", *c.Activity)
	}
	identity, itype := "", ""
	if c.Identity != nil {
		identity, itype = c.Identity.Name, map[string]string{"IDENTITY_TYPE_PER_CONFIG": "Configuration-scoped",
			"IDENTITY_TYPE_PER_PROJECT": "Project-scoped"}[c.Identity.Type]
	}
	props := []console.Property{
		{Label: "Dataset configuration name", Value: lastSegment(c.Name)},
		{Label: "Description", Value: c.Description},
		{Label: "Location", Value: c.location()},
		{Label: "State", Value: c.State},
		{Label: "Scope", Value: c.scope()},
		{Label: "Organization number", Value: c.Organization},
		{Label: "Bucket locations", Value: locs},
		{Label: "Bucket names", Value: buckets},
		{Label: "Include newly created buckets", Value: map[bool]string{true: "Yes", false: "No"}[c.NewBuckets]},
		{Label: "Retention period", Value: fmt.Sprintf("%d days", c.Retention)},
		{Label: "Activity data retention period", Value: activity},
		{Label: "Service agent", Value: identity, Copy: identity},
		{Label: "Service agent type", Value: itype},
		{Label: "UID", Value: c.UID},
		{Label: "Created", Value: shortTime(c.CreateTime)},
		{Label: "Updated", Value: shortTime(c.UpdateTime)},
		{Label: "Resource name", Value: c.Name, Copy: c.Name},
	}
	link := []console.Property{{Label: "Linked dataset", Value: orDash(c.linked()), Copy: c.linked()}}
	if c.linked() != "" {
		link = append(link, console.Property{Label: "Views", Value: "bucket_attributes_view, bucket_attributes_latest_snapshot_view, " +
			"object_attributes_view, object_attributes_latest_snapshot_view, project_attributes_view, object_contexts_catalog_view, " +
			"events_view, error_attributes_view, and the activity views"})
	}
	activityDefault := ""
	if c.Activity != nil {
		activityDefault = strconv.Itoa(*c.Activity)
	}
	return console.Detail{Summary: []console.Property{props[4], link[0]}, Sections: []console.Section{
		{ID: "details", Label: "Configuration", Kind: console.KindProperties, Groups: []console.PropertyGroup{
			{Heading: "Configuration", Properties: props}, {Heading: "BigQuery linked dataset", Properties: link}}},
	}, Edit: &console.EditForm{Label: "Edit", Title: "Edit dataset configuration", Submit: "Save", Fields: []console.Field{
		{Name: "description", Label: "Description", Type: "text", Default: c.Description, Pattern: `^.{0,256}$`},
		{Name: "retention", Label: "Retention period (days)", Type: "number", Required: true, Default: strconv.Itoa(c.Retention)},
		{Name: "activity", Label: "Activity data retention period (days)", Type: "number", Default: activityDefault},
	}}}, nil
}

// Edit implements console.Editor: datasetConfigs.patch of the
// description and the retention periods.
func (p storageInsightsDatasetsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a dataset configuration can be edited")
	}
	c, err := p.config(ctx, project, path[0])
	if err != nil {
		return err
	}
	body := map[string]any{"description": strings.TrimSpace(values["description"])}
	if err := datasetRetention(body, values); err != nil {
		return err
	}
	mask := "description,retentionPeriodDays"
	if _, ok := body["activityDataRetentionPeriodDays"]; ok {
		mask += ",activityDataRetentionPeriodDays"
	}
	return storageCall(ctx, http.MethodPatch, "http://"+p.endpoint+"/v1/"+c.Name+"?updateMask="+urlpkg.QueryEscape(mask), body, nil)
}

// Delete implements console.Deleter: datasetConfigs.delete, refused while
// the dataset is linked, as Google documents.
func (p storageInsightsDatasetsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.config(ctx, project, name)
	if err != nil {
		return err
	}
	return storageCall(ctx, http.MethodDelete, "http://"+p.endpoint+"/v1/"+c.Name, nil, nil)
}

const (
	actInsightsLink   = "link-dataset"
	actInsightsUnlink = "unlink-dataset"
)

// ListActions implements console.ListActor: Advance clock.
func (storageInsightsDatasetsProvider) ListActions(context.Context, string) []console.Action {
	return []console.Action{storageJobsClockAction()}
}

// DetailActions implements console.PathActor: Link dataset, or Unlink
// dataset when it is linked.
func (p storageInsightsDatasetsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	c, err := p.config(ctx, project, path[0])
	if err != nil {
		return nil
	}
	if c.linked() != "" {
		return []console.Action{{ID: actInsightsUnlink, Label: "Unlink dataset",
			Confirm: "Unlink the dataset? The BigQuery dataset " + c.linked() + " and its views are deleted; the configuration keeps " +
				"taking snapshots, and linking again makes the dataset anew with them."}}
	}
	return []console.Action{{ID: actInsightsLink, Label: "Link dataset"}}
}

// ActAt implements console.PathActor.
func (p storageInsightsDatasetsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, values)
	return err
}

// ActAtResult implements console.ResultActor.
func (p storageInsightsDatasetsProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) == 0 && action == actStorageJobsClock {
		return advanceStorageJobsClock(ctx, p.endpoint, values)
	}
	if len(path) == 1 && (action == actInsightsLink || action == actInsightsUnlink) {
		c, err := p.config(ctx, project, path[0])
		if err != nil {
			return nil, err
		}
		verb := ":linkDataset"
		if action == actInsightsUnlink {
			verb = ":unlinkDataset"
		}
		var op json.RawMessage
		return nil, storageCall(ctx, http.MethodPost, "http://"+p.endpoint+"/v1/"+c.Name+verb, map[string]any{}, &op)
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

var (
	_ console.PageCreator  = storageInsightsDatasetsProvider{}
	_ console.CreateTitler = storageInsightsDatasetsProvider{}
	_ console.Driller      = storageInsightsDatasetsProvider{}
	_ console.Editor       = storageInsightsDatasetsProvider{}
	_ console.Deleter      = storageInsightsDatasetsProvider{}
	_ console.ListActor    = storageInsightsDatasetsProvider{}
	_ console.ResultActor  = storageInsightsDatasetsProvider{}
	_ console.PathActor    = storageInsightsDatasetsProvider{}
)
