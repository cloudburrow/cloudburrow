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

var bqMultiRegions = []string{"US", "EU"}

var bqRoundingModes = []string{"", "ROUND_HALF_AWAY_FROM_ZERO", "ROUND_HALF_EVEN"}

// bqTimeTravelDays are the windows Google's form offers, 2 to 7 days.
var bqTimeTravelDays = []string{"2", "3", "4", "5", "6", "7"}

func bigqueryCreateDatasetFields() []console.Field {
	const adv = "Advanced options"
	return []console.Field{
		{Name: "datasetId", Label: "Dataset ID", Type: "text", Required: true, Pattern: bigqueryDatasetIDPattern,
			Help: "Letters, numbers and underscores, at most 1,024. It cannot be changed later."},
		{Name: "locationType", Label: "Location type", Type: "radio", Options: []string{bqLocationRegion, bqLocationMultiRegion},
			Default: bqLocationMultiRegion,
			Help:    "Where the dataset's data is stored. It cannot be changed later. The local engine runs every location in one place."},
		{Name: "multiRegion", Label: "Multi-region", Type: "select", Options: bqMultiRegions, Default: "US",
			ShowWhen: "locationType=" + bqLocationMultiRegion},
		{Name: "region", Label: "Region", Type: "text", Default: "us-central1", Pattern: `^[a-z]+(-[a-z]+)*[0-9]+$`,
			ShowWhen: "locationType=" + bqLocationRegion, Help: "Such as us-central1 or europe-west1."},
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

// CreateForm implements console.Creator: Create dataset.
func (bigqueryProvider) CreateForm() (string, []console.Field) {
	return "Create dataset", bigqueryCreateDatasetFields()
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
	case bqLocationMultiRegion:
		ds.Location = values["multiRegion"]
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
	return ds.DatasetReference.DatasetId, nil
}
