package main

// BigQuery's Create dataset, as Google's (#1861): Dataset ID, Location type
// (Region or Multi-region, with its location), Link to an external dataset
// (not available locally), then Advanced options: Encryption, Enable case
// insensitive table names, Default collation, Enable table expiration with
// Default maximum table age in days, Default rounding mode, Storage billing
// model and Time travel window, ending in Create dataset
// (https://docs.cloud.google.com/bigquery/docs/datasets). Each option the
// form offers is one the front keeps (datasets.insert then datasets.get,
// measured 2026-11-19); a description and labels, which Google's form does
// not ask for, are set with Edit dataset, and Create still takes them from a
// client of this route.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	bqv2 "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	bqLocationRegion      = "Region"
	bqLocationMultiRegion = "Multi-region"
	bqEncryptionGoogle    = "Google-owned and Google-managed encryption key"
	bqEncryptionKMS       = "Cloud KMS key"
	bqBillingLogical      = "Logical"
	bqBillingPhysical     = "Physical"
	bqNoExternalDataset   = "Not available locally: an external dataset links to AWS Glue or Spanner federation, " +
		"which CloudBurrow does not serve to BigQuery."
	bqNoDatasetKMS = "Not available locally: the local BigQuery engine encrypts nothing with a customer-managed key."
)

// bqMultiRegions and bqRegions are Google's BigQuery locations, as
// https://docs.cloud.google.com/bigquery/docs/locations lists them (read
// 2026-11-19), by geography, each named as the form names it.
var bqMultiRegions = []string{"US", "EU"}

var bqMultiRegionLabels = map[string]string{
	"US": "US (multiple regions in United States)",
	"EU": "EU (multiple regions in European Union)",
}

var bqRegionNames = [][2]string{
	// Americas
	{"us-east5", "Columbus, Ohio"}, {"us-south1", "Dallas"}, {"us-central1", "Iowa"}, {"us-west2", "Los Angeles"},
	{"us-west4", "Las Vegas"}, {"northamerica-south1", "Mexico"}, {"northamerica-northeast1", "Montréal"},
	{"us-east4", "Northern Virginia"}, {"us-central2", "Oklahoma"}, {"us-west1", "Oregon"}, {"us-west3", "Salt Lake City"},
	{"southamerica-east1", "São Paulo"}, {"southamerica-west1", "Santiago"}, {"us-east1", "South Carolina"},
	{"northamerica-northeast2", "Toronto"},
	// Europe
	{"europe-west1", "Belgium"}, {"europe-west10", "Berlin"}, {"europe-north1", "Finland"}, {"europe-west3", "Frankfurt"},
	{"europe-west2", "London"}, {"europe-southwest1", "Madrid"}, {"europe-west8", "Milan"}, {"europe-west4", "Netherlands"},
	{"europe-west9", "Paris"}, {"europe-north2", "Stockholm"}, {"europe-west12", "Turin"}, {"europe-central2", "Warsaw"},
	{"europe-west6", "Zürich"},
	// Asia Pacific
	{"asia-southeast3", "Bangkok"}, {"asia-south2", "Delhi"}, {"asia-east2", "Hong Kong"}, {"asia-southeast2", "Jakarta"},
	{"australia-southeast2", "Melbourne"}, {"asia-south1", "Mumbai"}, {"asia-northeast2", "Osaka"},
	{"asia-northeast3", "Seoul"}, {"asia-southeast1", "Singapore"}, {"australia-southeast1", "Sydney"},
	{"asia-east1", "Taiwan"}, {"asia-northeast1", "Tokyo"},
	// Middle East
	{"me-central2", "Dammam"}, {"me-central1", "Doha"}, {"me-west1", "Tel Aviv"},
	// Africa
	{"africa-south1", "Johannesburg"},
}

// bqRegions are the region IDs, and bqRegionLabels each one's "ID (City)".
func bqRegions() ([]string, map[string]string) {
	ids := make([]string, len(bqRegionNames))
	labels := make(map[string]string, len(bqRegionNames))
	for i, r := range bqRegionNames {
		ids[i] = r[0]
		labels[r[0]] = r[0] + " (" + r[1] + ")"
	}
	return ids, labels
}

// bqKnownLocation reports whether loc is one of Google's BigQuery
// locations; a multi-region's case is BigQuery's, US and EU.
func bqKnownLocation(loc string) bool {
	if slices.Contains(bqMultiRegions, loc) {
		return true
	}
	for _, r := range bqRegionNames {
		if r[0] == loc {
			return true
		}
	}
	return false
}

var bqRoundingModes = []string{"", "ROUND_HALF_AWAY_FROM_ZERO", "ROUND_HALF_EVEN"}

// bqTimeTravelDays are the windows Google's form offers, 2 to 7 days.
var bqTimeTravelDays = []string{"2", "3", "4", "5", "6", "7"}

func bigqueryCreateDatasetFields() []console.Field {
	const adv = "Advanced options"
	regions, regionLabels := bqRegions()
	return []console.Field{
		{Name: "datasetId", Label: "Dataset ID", Type: "text", Required: true, Pattern: bigqueryDatasetIDPattern,
			Help: "Letters, numbers and underscores, at most 1,024. It cannot be changed later."},
		{Name: "locationType", Label: "Location type", Type: "radio", Options: []string{bqLocationRegion, bqLocationMultiRegion},
			Default: bqLocationMultiRegion,
			Help:    "Where the dataset's data is stored. It cannot be changed later. The local engine runs every location in one place."},
		{Name: "multiRegion", Label: "Multi-region", Type: "select", Options: bqMultiRegions, Default: "US",
			OptionLabels: bqMultiRegionLabels, ShowWhen: "locationType=" + bqLocationMultiRegion},
		{Name: "region", Label: "Region", Type: "select", Options: regions, OptionLabels: regionLabels, Default: "us-central1",
			ShowWhen: "locationType=" + bqLocationRegion},
		{Name: "externalDataset", Label: "Link to an external dataset", Type: "checkbox", Default: "false",
			Unavailable: bqNoExternalDataset},
		{Name: "encryption", Label: "Encryption", Type: "radio", Section: adv,
			Options: []string{bqEncryptionGoogle, bqEncryptionKMS}, Default: bqEncryptionGoogle,
			UnavailableOptions: map[string]string{bqEncryptionKMS: bqNoDatasetKMS}},
		{Name: "caseInsensitive", Label: "Enable case insensitive table names", Type: "checkbox", Default: "false", Section: adv,
			Help: "Table names in the dataset are matched without regard to case."},
		{Name: "defaultCollation", Label: "Default collation", Type: "select", Options: bigqueryCollationOptions, Section: adv,
			Help: "und:ci compares the dataset's new STRING columns without regard to case."},
		{Name: "enableTableExpiration", Label: "Enable table expiration", Type: "checkbox", Default: "false", Section: adv},
		{Name: "tableExpirationDays", Label: "Default maximum table age (days)", Type: "number", Default: "1", Section: adv,
			ShowWhen: "enableTableExpiration", Required: true,
			Help: "A new table is deleted this many days after it is created, unless it sets its own expiration."},
		{Name: "defaultRoundingMode", Label: "Default rounding mode", Type: "select", Options: bqRoundingModes, Section: adv,
			Help: "How NUMERIC and BIGNUMERIC values written to the dataset's new tables are rounded."},
		{Name: "storageBillingModel", Label: "Storage billing model", Type: "radio", Section: adv,
			Options: []string{bqBillingLogical, bqBillingPhysical}, Default: bqBillingLogical,
			Help: "Kept as the dataset's setting; nothing is billed locally."},
		{Name: "timeTravelDays", Label: "Time travel window (days)", Type: "select", Options: bqTimeTravelDays, Default: "7", Section: adv},
	}
}

// CreateForm implements console.Creator: Create dataset, with Tags when
// the instance's Resource Manager holds tag values to bind.
func (p bigqueryProvider) CreateForm() (string, []console.Field) {
	fields := bigqueryCreateDatasetFields()
	if values := p.tagValueNames(); len(values) > 0 {
		keys := map[string]bool{}
		for _, v := range values {
			keys[v[:strings.LastIndex(v, "/")]] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		slices.Sort(names)
		fields = append(fields, console.Field{Name: "tags", Label: "Tags", Type: "map", Section: "Tags",
			KeyHelp: "Key: a tag key's namespaced name, such as " + names[0] + ".", ValueHelp: "Value: one of the key's short names.",
			Help: "Bound to the dataset as it is created, as Google's Create dataset adds tags (Resource Manager tag bindings). " +
				"This instance's tag keys: " + strings.Join(names, ", ") + ". A dataset carries one value of each key."})
	}
	return "Create dataset", fields
}

// tagValueNames are the namespaced names of every tag value the
// instance's Resource Manager holds, such as 123/env/prod.
func (p bigqueryProvider) tagValueNames() []string {
	if p.registry == nil {
		return nil
	}
	var out []string
	keys, err := p.registry.AllTagKeys()
	if err != nil {
		return nil
	}
	for _, k := range keys {
		vs, err := p.registry.TagValues(k.GetName())
		if err != nil {
			continue
		}
		for _, v := range vs {
			out = append(out, v.GetNamespacedName())
		}
	}
	slices.Sort(out)
	return out
}

// datasetTags are Create dataset's Tags as tag values' namespaced names,
// each checked against the registry before the dataset is made, so a
// refused tag makes no dataset.
func (p bigqueryProvider) datasetTags(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	tags, err := console.ParseMap(raw)
	if err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	if len(tags) == 0 {
		return nil, nil
	}
	if p.registry == nil {
		return nil, errors.New("tags: this instance's Resource Manager is not served to the console")
	}
	known := p.tagValueNames()
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		name := strings.TrimSuffix(strings.TrimSpace(k), "/") + "/" + strings.TrimSpace(tags[k])
		if !slices.Contains(known, name) {
			return nil, fmt.Errorf("tags: no tag value %s: create it on the Tags screen first", name)
		}
		out = append(out, name)
	}
	return out, nil
}

// CreateOnPage: Google draws Create dataset as a pane of its own, not a
// dialog over the list.
func (bigqueryProvider) CreateOnPage() bool { return true }

// CreateLayout: one page under its headings, Advanced options last.
func (bigqueryProvider) CreateLayout() string { return console.CreateLayoutPage }

// bigqueryDatasetFromForm is the dataset Create dataset's values describe.
func bigqueryDatasetFromForm(project string, values map[string]string) (*bqv2.Dataset, error) {
	if err := refuseUnavailable(values, bigqueryCreateDatasetFields()); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(values["datasetId"])
	ds := &bqv2.Dataset{DatasetReference: &bqv2.DatasetReference{ProjectId: project, DatasetId: id}}
	switch values["locationType"] {
	case bqLocationRegion:
		ds.Location = strings.TrimSpace(values["region"])
		if ds.Location == "" {
			return nil, errors.New("a region is required")
		}
		if !bqKnownLocation(ds.Location) {
			return nil, fmt.Errorf("%q is not a BigQuery region", ds.Location)
		}
	case bqLocationMultiRegion:
		ds.Location = values["multiRegion"]
		if !slices.Contains(bqMultiRegions, ds.Location) {
			return nil, fmt.Errorf("%q is not a BigQuery multi-region: US or EU", ds.Location)
		}
	default:
		// A client of the route that names the location itself (#854).
		ds.Location = strings.TrimSpace(values["location"])
	}
	ds.Description = values["description"]
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return nil, fmt.Errorf("labels: %w", err)
	}
	if len(labels) > 0 {
		ds.Labels = labels
	}
	ds.IsCaseInsensitive = values["caseInsensitive"] == "true"
	ds.DefaultCollation = values["defaultCollation"]
	if values["enableTableExpiration"] == "true" {
		days, err := strconv.ParseInt(values["tableExpirationDays"], 10, 64)
		if err != nil || days < 1 {
			return nil, errors.New("the default maximum table age is a whole number of days, at least 1")
		}
		ds.DefaultTableExpirationMs = days * 24 * 3600 * 1000
	}
	ds.DefaultRoundingMode = values["defaultRoundingMode"]
	if values["storageBillingModel"] == bqBillingPhysical {
		ds.StorageBillingModel = "PHYSICAL"
	}
	if d := values["timeTravelDays"]; d != "" {
		days, err := strconv.ParseInt(d, 10, 64)
		if err != nil || days < 2 || days > 7 {
			return nil, errors.New("the time travel window is 2 to 7 days")
		}
		ds.MaxTimeTravelHours = days * 24
	}
	return ds, nil
}

// Create implements console.Creator, with datasets.insert, which carries
// every option the form offers (the Go client has no default rounding
// mode).
func (p bigqueryProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	p = p.in(project) // #1764: the project chosen in the toolbar
	if err := p.writable(project); err != nil {
		return "", err
	}
	ds, err := bigqueryDatasetFromForm(p.project, values)
	if err != nil {
		return "", err
	}
	tags, err := p.datasetTags(values["tags"])
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	svc, err := bqv2.NewService(ctx, option.WithEndpoint("http://"+p.endpoint), option.WithoutAuthentication())
	if err != nil {
		return "", fmt.Errorf("cannot reach BigQuery: %w", err)
	}
	// An invalid ID is the front's 400 and an existing dataset its 409
	// "Already Exists", at once, as BigQuery answers them (#861).
	if _, err := svc.Datasets.Insert(p.project, ds).Context(ctx).Do(); err != nil {
		return "", bigqueryRefusal(err)
	}
	// The tags, as tagBindings.create on the dataset's full resource name,
	// the binding a dataset's Tags tab lists (consolebigquerytags.go).
	target := p.bigqueryTagTarget([]string{ds.DatasetReference.DatasetId})
	for _, tag := range tags {
		if _, err := tagAct(p.registry, target, tagBind, map[string]string{"value": tag}); err != nil {
			return "", fmt.Errorf("the dataset %s was created, but tag %s was not bound: %w", ds.DatasetReference.DatasetId, tag, err)
		}
	}
	return ds.DatasetReference.DatasetId, nil
}
