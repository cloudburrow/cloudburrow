package main

// Cloud Storage > Inventory reports (#1957): Storage Insights inventory
// report configurations (storageinsights.googleapis.com v1), as Google's
// Inventory reports page lists them, with Create inventory report
// configuration, each configuration's page (its settings and Report
// history, the reportDetails it has written), Edit and Delete. Reports are
// written on the instance's clock, which Advance clock (CloudBurrow
// extension) moves. A configuration lives in its source bucket's location,
// so the list reads the project's buckets' locations from Cloud Storage.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	urlpkg "net/url"
	"sort"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

type storageInventoryProvider struct {
	endpoint string // the Storage Insights API's host address
	storage  string // Cloud Storage's, for the buckets' locations
}

func (storageInventoryProvider) ID() string    { return "storage-inventory-reports" }
func (storageInventoryProvider) Title() string { return "Inventory reports" }

type insightsDate struct {
	Year  int `json:"year"`
	Month int `json:"month"`
	Day   int `json:"day"`
}

func (d *insightsDate) String() string {
	if d == nil || d.Year == 0 {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day)
}

// parseInsightsDate reads a date field's YYYY-MM-DD.
func parseInsightsDate(s string) (*insightsDate, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, fmt.Errorf("%q is not a date such as 2026-11-19", s)
	}
	return &insightsDate{Year: t.Year(), Month: int(t.Month()), Day: t.Day()}, nil
}

// reportConfigJSON is a ReportConfig as the REST API answers it.
type reportConfigJSON struct {
	Name             string `json:"name"`
	DisplayName      string `json:"displayName"`
	CreateTime       string `json:"createTime"`
	UpdateTime       string `json:"updateTime"`
	FrequencyOptions *struct {
		Frequency string        `json:"frequency"`
		StartDate *insightsDate `json:"startDate"`
		EndDate   *insightsDate `json:"endDate"`
	} `json:"frequencyOptions"`
	CSVOptions *struct {
		RecordSeparator string `json:"recordSeparator"`
		Delimiter       string `json:"delimiter"`
		HeaderRequired  bool   `json:"headerRequired"`
	} `json:"csvOptions"`
	ParquetOptions *struct{} `json:"parquetOptions"`
	Options        *struct {
		MetadataFields []string `json:"metadataFields"`
		StorageFilters *struct {
			Bucket string `json:"bucket"`
		} `json:"storageFilters"`
		Destination *struct {
			Bucket          string `json:"bucket"`
			DestinationPath string `json:"destinationPath"`
		} `json:"storageDestinationOptions"`
	} `json:"objectMetadataReportOptions"`
}

func (c reportConfigJSON) format() string {
	if c.ParquetOptions != nil {
		return "Parquet"
	}
	return "CSV"
}

func (c reportConfigJSON) source() string {
	if c.Options == nil || c.Options.StorageFilters == nil {
		return ""
	}
	return c.Options.StorageFilters.Bucket
}

func (c reportConfigJSON) destination() string {
	if c.Options == nil || c.Options.Destination == nil {
		return ""
	}
	d := "gs://" + c.Options.Destination.Bucket + "/"
	if p := strings.Trim(c.Options.Destination.DestinationPath, "/"); p != "" {
		d += p + "/"
	}
	return d
}

func (c reportConfigJSON) frequency() (string, string, string) {
	if c.FrequencyOptions == nil {
		return "", "", ""
	}
	f := c.FrequencyOptions
	return strings.ToLower(f.Frequency), f.StartDate.String(), f.EndDate.String()
}

// location is the location segment of the configuration's name.
func (c reportConfigJSON) location() string {
	parts := strings.Split(c.Name, "/")
	if len(parts) >= 4 {
		return parts[3]
	}
	return ""
}

// inventoryMetadataFields are the metadata fields Google's form offers,
// beyond project, bucket and name, which every report has.
var inventoryMetadataFields = []string{"location", "size", "timeCreated", "timeDeleted", "updated", "storageClass", "etag",
	"retentionExpirationTime", "crc32c", "md5Hash", "generation", "metageneration", "contentType", "contentEncoding",
	"timeStorageClassUpdated"}

// inventoryDefaultFields are the fields checked by default.
var inventoryDefaultFields = map[string]bool{"location": true, "size": true, "timeCreated": true, "updated": true, "storageClass": true}

func (p storageInventoryProvider) parent(project, location string) string {
	return "http://" + p.endpoint + "/v1/projects/" + urlpkg.PathEscape(project) + "/locations/" + urlpkg.PathEscape(location)
}

// bucketJSON is what a bucket listing gives the forms and the list.
type bucketJSON struct {
	Name     string `json:"name"`
	Location string `json:"location"`
}

// buckets are the project's buckets, from Cloud Storage's JSON API.
func (p storageInventoryProvider) buckets(ctx context.Context, project string) ([]bucketJSON, error) {
	if p.storage == "" {
		return nil, errors.New("Cloud Storage is not served on this instance")
	}
	return listPages[bucketJSON](ctx, hostURL(p.storage)+"/storage/v1/b?project="+urlpkg.QueryEscape(project), "items")
}

// locations are the locations of the project's buckets, where their report
// configurations are.
func (p storageInventoryProvider) locations(ctx context.Context, project string) ([]string, error) {
	bs, err := p.buckets(ctx, project)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, b := range bs {
		l := strings.ToLower(b.Location)
		if l != "" && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (p storageInventoryProvider) configs(ctx context.Context, project string) ([]reportConfigJSON, error) {
	locs, err := p.locations(ctx, project)
	if err != nil {
		return nil, err
	}
	var out []reportConfigJSON
	for _, l := range locs {
		cs, err := listPages[reportConfigJSON](ctx, p.parent(project, l)+"/reportConfigs?pageSize=100", "reportConfigs")
		if err != nil {
			return nil, fmt.Errorf("list %s's report configurations: %w", l, err)
		}
		out = append(out, cs...)
	}
	return out, nil
}

// config finds a configuration by its ID, in any of the project's
// locations.
func (p storageInventoryProvider) config(ctx context.Context, project, id string) (reportConfigJSON, error) {
	cs, err := p.configs(ctx, project)
	if err != nil {
		return reportConfigJSON{}, err
	}
	for _, c := range cs {
		if lastSegment(c.Name) == id {
			return c, nil
		}
	}
	return reportConfigJSON{}, fmt.Errorf("no inventory report configuration %s in project %s", id, project)
}

func (p storageInventoryProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{
		Columns:    []string{"Display name", "Source bucket", "Destination", "Format", "Frequency", "Location", "Created"},
		NameColumn: "Configuration ID",
		Noun:       "inventory report configurations",
		Note: "Storage Insights inventory reports of this instance's buckets, written as CSV or Parquet into the destination " +
			"bucket on the instance's clock (Advance clock moves it).",
	}
	if project == "" {
		listing.Prompt = "Inventory reports are shown for a project. Choose one in the toolbar."
		return listing, nil
	}
	cs, err := p.configs(ctx, project)
	if err != nil {
		return console.Listing{}, err
	}
	for _, c := range cs {
		freq, _, _ := c.frequency()
		listing.Items = append(listing.Items, console.Resource{Name: lastSegment(c.Name), Fields: map[string]string{
			"Display name": c.DisplayName, "Source bucket": c.source(), "Destination": c.destination(),
			"Format": c.format(), "Frequency": freq, "Location": c.location(), "Created": shortTime(c.CreateTime)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (storageInventoryProvider) CreateForm() (string, []console.Field) {
	const (
		identify = "Identify your report configuration"
		metadata = "Choose metadata fields"
		dest     = "Choose a file format and a destination"
		schedule = "Scheduling options"
	)
	today := time.Now().UTC().Format("2006-01-02")
	fields := []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Section: identify, Pattern: `^.{0,256}$`, Help: "Up to 256 characters."},
		{Name: "sourceBucket", Label: "Source bucket", Type: "text", Required: true, Section: identify, Browse: "storage",
			Help: "The bucket the reports list. The configuration is made in its location."},
	}
	for _, f := range inventoryMetadataFields {
		def, help := "", ""
		if inventoryDefaultFields[f] {
			def = "true"
		}
		if f == inventoryMetadataFields[0] {
			help = "project, bucket and name are always included."
		}
		fields = append(fields, console.Field{Name: "field." + f, Label: f, Type: "checkbox", Default: def, Section: metadata, Help: help})
	}
	return "Create report configuration", append(fields,
		console.Field{Name: "format", Label: "File format", Type: "radio", Section: dest, Default: "csv",
			Options: []string{"csv", "parquet"}, OptionLabels: map[string]string{"csv": "CSV", "parquet": "Apache Parquet"}},
		console.Field{Name: "delimiter", Label: "Delimiter", Type: "text", Section: dest, Default: ",", ShowWhen: "format=csv"},
		console.Field{Name: "header", Label: "Include a header row", Type: "checkbox", Section: dest, Default: "true", ShowWhen: "format=csv"},
		console.Field{Name: "destinationBucket", Label: "Destination bucket", Type: "text", Required: true, Section: dest,
			Browse: "storage", Help: "In the source bucket's project and location; it can be the source bucket."},
		console.Field{Name: "destinationPath", Label: "Destination path", Type: "text", Section: dest,
			Help: "A folder of the destination bucket, such as reports/. Empty: the bucket's root."},
		console.Field{Name: "frequency", Label: "Frequency", Type: "radio", Section: schedule, Default: "DAILY",
			Options: []string{"DAILY", "WEEKLY"}, OptionLabels: map[string]string{"DAILY": "Daily", "WEEKLY": "Weekly"}},
		console.Field{Name: "startDate", Label: "Start date", Type: "date", Required: true, Section: schedule, Default: today,
			Help: "The first report is written on this date (UTC), on the instance's clock."},
		console.Field{Name: "endDate", Label: "End date", Type: "date", Required: true, Section: schedule, Default: today,
			Help: "The last report is written on this date."},
	)
}

// CreateOnPage implements console.PageCreator.
func (storageInventoryProvider) CreateOnPage() bool { return true }

// CreateTitle implements console.CreateTitler.
func (storageInventoryProvider) CreateTitle() string { return "Create report configuration" }

func (p storageInventoryProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project")
	}
	src, dst := gsBucket(values["sourceBucket"]), gsBucket(values["destinationBucket"])
	if src == "" || dst == "" {
		return "", errors.New("choose a source and a destination bucket")
	}
	bs, err := p.buckets(ctx, project)
	if err != nil {
		return "", err
	}
	location := ""
	for _, b := range bs {
		if b.Name == src {
			location = strings.ToLower(b.Location)
		}
	}
	if location == "" {
		return "", fmt.Errorf("project %s has no bucket %s", project, src)
	}
	fields := []string{"project", "bucket", "name"}
	for _, f := range inventoryMetadataFields {
		if values["field."+f] == "true" {
			fields = append(fields, f)
		}
	}
	freq, err := inventoryFrequency(values)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"frequencyOptions": freq,
		"objectMetadataReportOptions": map[string]any{
			"metadataFields": fields,
			"storageFilters": map[string]any{"bucket": src},
			"storageDestinationOptions": map[string]any{"bucket": dst,
				"destinationPath": strings.Trim(strings.TrimSpace(values["destinationPath"]), "/")},
		},
	}
	if d := strings.TrimSpace(values["displayName"]); d != "" {
		body["displayName"] = d
	}
	if values["format"] == "parquet" {
		body["parquetOptions"] = map[string]any{}
	} else {
		delim := values["delimiter"]
		if delim == "" {
			delim = ","
		}
		body["csvOptions"] = map[string]any{"recordSeparator": "\n", "delimiter": delim, "headerRequired": values["header"] == "true"}
	}
	var made reportConfigJSON
	if err := storageCall(ctx, http.MethodPost, p.parent(project, location)+"/reportConfigs", body, &made); err != nil {
		return "", err
	}
	return lastSegment(made.Name), nil
}

// inventoryFrequency is the form's frequencyOptions.
func inventoryFrequency(values map[string]string) (map[string]any, error) {
	start, err := parseInsightsDate(values["startDate"])
	if err != nil {
		return nil, err
	}
	end, err := parseInsightsDate(values["endDate"])
	if err != nil {
		return nil, err
	}
	if start == nil || end == nil {
		return nil, errors.New("give a start date and an end date")
	}
	if end.String() < start.String() {
		return nil, errors.New("the end date is before the start date")
	}
	f := values["frequency"]
	if f != "WEEKLY" {
		f = "DAILY"
	}
	return map[string]any{"frequency": f, "startDate": start, "endDate": end}, nil
}

// reportDetailJSON is a ReportDetail as the REST API answers it.
type reportDetailJSON struct {
	Name             string      `json:"name"`
	SnapshotTime     string      `json:"snapshotTime"`
	ReportPathPrefix string      `json:"reportPathPrefix"`
	ShardsCount      json.Number `json:"shardsCount"`
	Status           *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
	TargetDatetime *struct {
		Year    int `json:"year"`
		Month   int `json:"month"`
		Day     int `json:"day"`
		Hours   int `json:"hours"`
		Minutes int `json:"minutes"`
	} `json:"targetDatetime"`
	ReportMetrics *struct {
		ProcessedRecordsCount json.Number `json:"processedRecordsCount"`
	} `json:"reportMetrics"`
}

func (d reportDetailJSON) target() string {
	t := d.TargetDatetime
	if t == nil || t.Year == 0 {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d", t.Year, t.Month, t.Day, t.Hours, t.Minutes)
}

func (d reportDetailJSON) status() string {
	if d.Status == nil || d.Status.Code == 0 {
		return "Succeeded"
	}
	return "Failed"
}

func (p storageInventoryProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.config(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the configuration: " + err.Error()}, nil
	}
	freq, start, end := c.frequency()
	var fields []string
	if c.Options != nil {
		fields = c.Options.MetadataFields
	}
	format := c.format()
	if c.CSVOptions != nil {
		format += fmt.Sprintf(" (delimiter %q, header %s)", c.CSVOptions.Delimiter, map[bool]string{true: "yes", false: "no"}[c.CSVOptions.HeaderRequired])
	}
	props := []console.Property{
		{Label: "Configuration ID", Value: lastSegment(c.Name)},
		{Label: "Display name", Value: c.DisplayName},
		{Label: "Source bucket", Value: c.source()},
		{Label: "Destination", Value: c.destination()},
		{Label: "Format", Value: format},
		{Label: "Metadata fields", Value: strings.Join(fields, ", ")},
		{Label: "Frequency", Value: freq},
		{Label: "Start date", Value: start},
		{Label: "End date", Value: end},
		{Label: "Location", Value: c.location()},
		{Label: "Created", Value: shortTime(c.CreateTime)},
		{Label: "Updated", Value: shortTime(c.UpdateTime)},
		{Label: "Resource name", Value: c.Name, Copy: c.Name},
	}
	history := console.Listing{Columns: []string{"Snapshot", "Records", "Shards", "Report path"}, NameColumn: "Target date",
		Noun: "reports", AlwaysStatus: true}
	ds, err := listPages[reportDetailJSON](ctx, "http://"+p.endpoint+"/v1/"+c.Name+"/reportDetails?pageSize=100", "reportDetails")
	if err != nil {
		history.Note = "cannot list the reports: " + err.Error()
	}
	for _, d := range ds {
		records := ""
		if d.ReportMetrics != nil {
			records = jobCount(d.ReportMetrics.ProcessedRecordsCount)
		}
		history.Items = append(history.Items, console.Resource{Name: orDash(d.target()), Status: d.status(), Fields: map[string]string{
			"Snapshot": shortTime(d.SnapshotTime), "Records": records, "Shards": jobCount(d.ShardsCount), "Report path": d.ReportPathPrefix}})
	}
	history.Total = len(history.Items)
	return console.Detail{Summary: []console.Property{props[2], props[3]}, Sections: []console.Section{
		{ID: "details", Label: "Configuration", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Configuration", Properties: props}}},
		{ID: "history", Label: "Report history", Listing: history},
	}, Edit: &console.EditForm{Label: "Edit", Title: "Edit report configuration", Submit: "Save", Fields: []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Default: c.DisplayName},
		{Name: "frequency", Label: "Frequency", Type: "radio", Default: strings.ToUpper(freq),
			Options: []string{"DAILY", "WEEKLY"}, OptionLabels: map[string]string{"DAILY": "Daily", "WEEKLY": "Weekly"}},
		{Name: "startDate", Label: "Start date", Type: "date", Required: true, Default: start},
		{Name: "endDate", Label: "End date", Type: "date", Required: true, Default: end},
		{Name: "destinationPath", Label: "Destination path", Type: "text", Default: destPath(c)},
	}}}, nil
}

func destPath(c reportConfigJSON) string {
	if c.Options == nil || c.Options.Destination == nil {
		return ""
	}
	return c.Options.Destination.DestinationPath
}

// Edit implements console.Editor: reportConfigs.patch of the display name,
// the schedule and the destination path, under an update mask.
func (p storageInventoryProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a configuration can be edited")
	}
	c, err := p.config(ctx, project, path[0])
	if err != nil {
		return err
	}
	freq, err := inventoryFrequency(values)
	if err != nil {
		return err
	}
	dest := map[string]any{"destinationPath": strings.Trim(strings.TrimSpace(values["destinationPath"]), "/")}
	if c.Options != nil && c.Options.Destination != nil {
		dest["bucket"] = c.Options.Destination.Bucket
	}
	body := map[string]any{"displayName": strings.TrimSpace(values["displayName"]), "frequencyOptions": freq,
		"objectMetadataReportOptions": map[string]any{"storageDestinationOptions": dest}}
	mask := "displayName,frequencyOptions,objectMetadataReportOptions.storageDestinationOptions.destinationPath"
	return storageCall(ctx, http.MethodPatch, "http://"+p.endpoint+"/v1/"+c.Name+"?updateMask="+urlpkg.QueryEscape(mask), body, nil)
}

// Delete implements console.Deleter: reportConfigs.delete. The reports
// already written stay in the destination bucket, as Google documents.
func (p storageInventoryProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.config(ctx, project, name)
	if err != nil {
		return err
	}
	return storageCall(ctx, http.MethodDelete, "http://"+p.endpoint+"/v1/"+c.Name, nil, nil)
}

// ListActions implements console.ListActor: Advance clock.
func (storageInventoryProvider) ListActions(context.Context, string) []console.Action {
	return []console.Action{storageJobsClockAction()}
}

// DetailActions implements console.PathActor: none on a configuration's
// page beyond Edit and Delete; Advance clock is the list screen's.
func (storageInventoryProvider) DetailActions(context.Context, string, []string) []console.Action {
	return nil
}

// ActAt implements console.PathActor.
func (p storageInventoryProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, values)
	return err
}

// ActAtResult implements console.ResultActor: Advance clock answers the
// clock's new time.
func (p storageInventoryProvider) ActAtResult(ctx context.Context, _ string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) == 0 && action == actStorageJobsClock {
		return advanceStorageJobsClock(ctx, p.endpoint, values)
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

var (
	_ console.PageCreator  = storageInventoryProvider{}
	_ console.CreateTitler = storageInventoryProvider{}
	_ console.Driller      = storageInventoryProvider{}
	_ console.Editor       = storageInventoryProvider{}
	_ console.Deleter      = storageInventoryProvider{}
	_ console.ListActor    = storageInventoryProvider{}
	_ console.ResultActor  = storageInventoryProvider{}
)
