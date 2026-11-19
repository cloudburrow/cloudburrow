package main

// Create instance (#1491, #1859): Google's Create instance is a page of
// steps, Select an edition, Name your instance, Configure your instance,
// Allocate compute capacity and Backups, ending in Create
// (https://docs.cloud.google.com/spanner/docs/create-manage-instances). It
// sends InstanceAdmin's CreateInstance with the ID, name, configuration,
// compute capacity and labels. What an emulator instance cannot carry is on
// the page, disabled, with its reason: the Enterprise editions (the
// emulator has no editions), dual-region configurations (none is served),
// autoscaling (the emulator does not scale) and default backup schedules
// (new databases get none here).

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

const spannerCreateInstanceAction = "createinstance"

// ListActions implements console.ListActor. Create instance is the list's
// Create now, the provider's Creator (#1859), as Google's Instances page has
// it on the title line, so the list has no actions of its own.
func (p spannerProvider) ListActions(_ context.Context, project string) []console.Action {
	return nil
}

const (
	spannerNoEdition   = "Not available locally: the Spanner emulator has no editions, so an instance is created with none, as a Standard edition instance serves the features the emulator has."
	spannerNoDual      = "Not available locally: CloudBurrow serves no dual-region instance configuration (its configurations are regional-<region>, nam3 and eur3)."
	spannerNoAutoscale = "Not available locally: the Spanner emulator does not autoscale; UpdateInstance refuses an autoscaling configuration."
	spannerNoDefaultBk = "Not available locally: new databases get no default backup schedule here; create one on the database's Backup schedules tab."
)

// spannerCompareEditions is Create instance's Compare editions dialog,
// Google's comparison (https://docs.cloud.google.com/spanner/docs/editions-overview,
// read 2026-11-19), with what the emulator serves of it.
var spannerCompareEditions = &console.FieldDialog{
	Button:  "Compare editions",
	Title:   "Compare editions",
	Intro:   "Locally, the Spanner emulator has no editions: every instance serves the same features, those of the Standard edition that the emulator has.",
	Columns: []string{"", "Standard", "Enterprise", "Enterprise Plus"},
	Rows: [][]string{
		{"Availability SLA", "99.99%", "99.99%", "Up to 99.999%"},
		{"Configurations", "Regional", "Regional, plus optional custom read-only replicas", "Regional, dual-region and multi-region, plus optional custom read-only replicas"},
		{"Multi-model capabilities", "Relational (GoogleSQL, PostgreSQL), key-value", "Standard's, plus Spanner Graph", "Enterprise's"},
		{"Search capabilities", "—", "Full-text search, vector search (KNN, ANN)", "Enterprise's"},
		{"Resource management", "Open source Autoscaler", "Managed autoscaler, asymmetric read-only autoscaling, locality groups, tiered storage", "Enterprise's, plus geo-partitioning"},
		{"Analytics", "BigQuery federation, Spanner Data Boost, Reverse ETL", "Standard's, plus the columnar engine", "Enterprise's"},
		{"Data protection", "Standard backups, 7-day PITR, scheduled backups", "Standard's, plus incremental backups", "Enterprise's"},
		{"Committed use discounts", "20% for 1 year, 40% for 3 years", "Standard's", "Standard's"},
		{"Locally", "Created with no edition", "Not available locally", "Not available locally"},
	},
}

// spannerMultiRegions are the multi-region configurations the call front
// serves, with Google's names for them.
var spannerMultiRegions = map[string]string{
	"nam3": "nam3 (North America: Northern Virginia/South Carolina)",
	"eur3": "eur3 (Europe: Belgium/Netherlands)",
}

// spannerCreateInstanceFields is the Create instance page.
func spannerCreateInstanceFields() []console.Field {
	regional := []string{"emulator-config"}
	labels := map[string]string{"emulator-config": "emulator-config (the emulator's own configuration)"}
	for _, r := range bqRegionNames {
		id := "regional-" + r[0]
		regional = append(regional, id)
		labels[id] = fmt.Sprintf("%s (%s)", r[0], r[1])
	}
	for id, name := range spannerMultiRegions {
		labels[id] = name
	}
	return []console.Field{
		{Name: "edition", Label: "Select an edition", Type: "radio", Section: "Select an edition",
			Options: []string{"Standard", "Enterprise", "Enterprise Plus"}, Default: "Standard",
			UnavailableOptions: map[string]string{"Enterprise": spannerNoEdition, "Enterprise Plus": spannerNoEdition},
			Help:               "The emulator serves every instance the same, with no edition.",
			Dialog:             spannerCompareEditions},
		{Name: "displayName", Label: "Instance name", Type: "text", Required: true, Section: "Name your instance",
			Pattern: `^.{4,30}$`, Help: "For display purposes only: 4 to 30 characters."},
		{Name: "instance", Label: "Instance ID", Type: "text", Required: true, Section: "Name your instance",
			DeriveFrom: "displayName", Pattern: `^[a-z][a-z0-9\-]{1,62}[a-z0-9]$`,
			Help: "Lowercase letters, digits and hyphens, 2 to 64 characters, starting with a letter. It is permanent."},
		{Name: "configType", Label: "Choose a configuration", Type: "radio", Section: "Configure your instance",
			Options: []string{"Regional", "Dual-region", "Multi-region"}, Default: "Regional",
			UnavailableOptions: map[string]string{"Dual-region": spannerNoDual}},
		{Name: "regionalConfig", Label: "Select a configuration", Type: "select", Section: "Configure your instance",
			Options: regional, OptionLabels: labels, Default: "regional-us-central1", ShowWhen: "configType=Regional",
			Help: "Where the instance's data is kept: the emulator serves every configuration the same."},
		{Name: "multiConfig", Label: "Select a configuration", Type: "select", Section: "Configure your instance",
			Options: []string{"nam3", "eur3"}, OptionLabels: labels, Default: "nam3", ShowWhen: "configType=Multi-region"},
		{Name: "unit", Label: "Select unit", Type: "radio", Section: "Allocate compute capacity",
			Options: []string{"Nodes", "Processing units (PUs)"}, Default: "Nodes"},
		{Name: "scaling", Label: "Choose a scaling mode", Type: "radio", Section: "Allocate compute capacity",
			Options: []string{"Manual allocation", "Autoscaling"}, Default: "Manual allocation",
			UnavailableOptions: map[string]string{"Autoscaling": spannerNoAutoscale}},
		{Name: "nodes", Label: "Quantity", Type: "text", Required: true, Section: "Allocate compute capacity",
			Default: "1", Pattern: `^[1-9][0-9]*$`, ShowWhen: "unit=Nodes", Help: "Nodes: 1 node is 1,000 processing units."},
		{Name: "processingUnits", Label: "Quantity", Type: "text", Required: true, Section: "Allocate compute capacity",
			Default: "1000", Pattern: `^[0-9]+$`, ShowWhen: "unit=Processing units (PUs)",
			Help: "Processing units: a multiple of 100 below 1,000, and of 1,000 from 1,000."},
		{Name: "defaultBackups", Label: "Enable default backup schedules", Type: "checkbox", Section: "Backups",
			Unavailable: spannerNoDefaultBk},
		{Name: "labels", Label: "Labels", Type: "map", Section: "Labels (optional)",
			KeyPattern: pubsubfront.LabelKeyPattern, ValuePattern: pubsubfront.LabelValuePattern,
			KeyHelp:    "A key is 1 to 63 lowercase letters, international characters, digits, _ and -, starting with a lowercase letter or an international character.",
			ValueHelp:  "A value is 0 to 63 lowercase letters, international characters, digits, _ and -.",
			MaxEntries: pubsubfront.MaxLabels},
	}
}

// createInstance is CreateInstance, waited on. It reads the page's fields,
// and the older form's config and processingUnits when those are sent.
func (p spannerProvider) createInstance(ctx context.Context, project string, v map[string]string) error {
	id, display := strings.TrimSpace(v["instance"]), strings.TrimSpace(v["displayName"])
	if id == "" || display == "" {
		return fmt.Errorf("an instance ID and an instance name are required")
	}
	if e := strings.TrimSpace(v["edition"]); e != "" && e != "Standard" {
		return fmt.Errorf("edition %s: %s", e, spannerNoEdition)
	}
	if strings.TrimSpace(v["scaling"]) == "Autoscaling" {
		return fmt.Errorf("%s", spannerNoAutoscale)
	}
	var pu int64
	var err error
	if strings.TrimSpace(v["unit"]) == "Nodes" {
		n, perr := strconv.ParseInt(strings.TrimSpace(v["nodes"]), 10, 32)
		if perr != nil || n <= 0 {
			return fmt.Errorf("nodes %q: a whole number of nodes, at least 1", v["nodes"])
		}
		pu = n * 1000
	} else {
		pu, err = strconv.ParseInt(strings.TrimSpace(v["processingUnits"]), 10, 32)
		if err != nil || pu <= 0 || (pu < 1000 && pu%100 != 0) || (pu >= 1000 && pu%1000 != 0) {
			return fmt.Errorf("processing units %q: a multiple of 100 below 1000, and of 1000 from 1000", v["processingUnits"])
		}
	}
	cfg := strings.TrimSpace(v["config"])
	switch strings.TrimSpace(v["configType"]) {
	case "Regional":
		cfg = strings.TrimSpace(v["regionalConfig"])
	case "Multi-region":
		cfg = strings.TrimSpace(v["multiConfig"])
	case "Dual-region":
		return fmt.Errorf("%s", spannerNoDual)
	}
	if cfg == "" {
		cfg = "emulator-config"
	}
	if !strings.HasPrefix(cfg, "projects/") {
		cfg = "projects/" + project + "/instanceConfigs/" + cfg
	}
	labels, err := console.ParseMap(v["labels"])
	if err != nil {
		return fmt.Errorf("labels: %w", err)
	}
	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return fmt.Errorf("cannot reach Spanner: %w", err)
	}
	defer instAdmin.Close()
	in := &instancepb.Instance{Config: cfg, DisplayName: display, ProcessingUnits: int32(pu)}
	if len(labels) > 0 {
		in.Labels = labels
	}
	op, err := instAdmin.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: "projects/" + project, InstanceId: id, Instance: in})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

// spannerEdition is an instance's edition as Google's list names it; an
// emulator instance has none.
func spannerEdition(e instancepb.Instance_Edition) string {
	switch e {
	case instancepb.Instance_STANDARD:
		return "Standard"
	case instancepb.Instance_ENTERPRISE:
		return "Enterprise"
	case instancepb.Instance_ENTERPRISE_PLUS:
		return "Enterprise Plus"
	}
	return "—"
}

var _ console.ListActor = spannerProvider{}
