package main

// Cloud Logging's project settings (#1311), at /logs/settings under Logs
// Storage: GetSettings, UpdateSettings and GetCmekSettings on the same
// ConfigServiceV2 the transports serve (#1292), with the instance's fault
// rules applied first. The list holds the project's one settings resource;
// its page shows the settings and the CMEK settings as served, and Edit
// settings is UpdateSettings, so a refusal (a KMS key, a malformed location)
// is the API's own.

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

type loggingSettingsProvider struct{ loggingAdminScreen }

func (loggingSettingsProvider) ID() string    { return "logging-settings" }
func (loggingSettingsProvider) Title() string { return "Logs Storage settings" }

const loggingSettingsNote = "Stored and read back; a storage location and the default sink change nothing that is written or read here. " +
	"Customer-managed encryption (a KMS key) is not supported: the API refuses one."

func settingsName(project string) string { return "projects/" + project + "/settings" }

func defaultSinkWord(disabled bool) string {
	if disabled {
		return "Disabled"
	}
	return "Enabled"
}

func (p loggingSettingsProvider) settings(ctx context.Context, project string) (*loggingpb.Settings, error) {
	a, err := p.admin()
	if err != nil {
		return nil, err
	}
	name := settingsName(project)
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_GetSettings_FullMethodName, name); err != nil {
		return nil, err
	}
	return a.ConfigServer().GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: name})
}

func (p loggingSettingsProvider) cmek(ctx context.Context, project string) (*loggingpb.CmekSettings, error) {
	a, err := p.admin()
	if err != nil {
		return nil, err
	}
	name := "projects/" + project + "/cmekSettings"
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_GetCmekSettings_FullMethodName, name); err != nil {
		return nil, err
	}
	return a.ConfigServer().GetCmekSettings(ctx, &loggingpb.GetCmekSettingsRequest{Name: name})
}

func (p loggingSettingsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Storage location", "Default sink", "KMS key"}, NameColumn: "Settings", Noun: "settings",
		Note: loggingSettingsNote}
	if _, err := p.admin(); err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Cloud Logging settings are per project. Choose one in the toolbar."
		return out, nil
	}
	st, err := p.settings(ctx, project)
	if err != nil {
		return console.Listing{}, err
	}
	out.Items = []console.Resource{{Name: st.GetName(), Fields: map[string]string{
		"Storage location": orDash(st.GetStorageLocation()), "Default sink": defaultSinkWord(st.GetDisableDefaultSink()),
		"KMS key": orDash(st.GetKmsKeyName())}}}
	out.Total = 1
	out.RowsOpenable = true
	return out, nil
}

func (p loggingSettingsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	if path[0] != settingsName(project) {
		return console.Detail{Unavailable: path[0] + " is not project " + project + "'s settings"}, nil
	}
	st, err := p.settings(ctx, project)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	cmekGroup := console.PropertyGroup{Heading: "CMEK settings"}
	if c, err := p.cmek(ctx, project); err != nil {
		cmekGroup.Properties = []console.Property{{Label: "Unavailable", Value: consoleErr(err)}}
	} else {
		cmekGroup.Properties = []console.Property{
			{Label: "Name", Value: c.GetName()},
			{Label: "KMS key", Value: orDash(c.GetKmsKeyName())},
			{Label: "KMS key version", Value: orDash(c.GetKmsKeyVersionName())},
			{Label: "Service account", Value: c.GetServiceAccountId()},
		}
	}
	return console.Detail{
		Title: "Logs Storage settings",
		Summary: []console.Property{
			{Label: "Storage location", Value: orDash(st.GetStorageLocation())},
			{Label: "Default sink", Value: defaultSinkWord(st.GetDisableDefaultSink())},
		},
		Sections: []console.Section{{ID: "settings", Label: "Settings", Kind: console.KindProperties, Note: loggingSettingsNote, Groups: []console.PropertyGroup{
			{Heading: "Project settings", Properties: []console.Property{
				{Label: "Name", Value: st.GetName()},
				{Label: "Storage location", Value: orDash(st.GetStorageLocation())},
				{Label: "Default sink (_Default)", Value: defaultSinkWord(st.GetDisableDefaultSink())},
				{Label: "KMS key", Value: orDash(st.GetKmsKeyName())},
				{Label: "KMS service account", Value: st.GetKmsServiceAccountId()},
			}},
			cmekGroup,
		}}},
		Edit: &console.EditForm{Label: "Edit settings", Note: "UpdateSettings: a KMS key is refused as UNIMPLEMENTED.", Fields: []console.Field{
			{Name: "storageLocation", Label: "Default storage location", Type: "text", Default: st.GetStorageLocation(),
				Help: "Where new buckets are made by default: global, or a region such as us-central1. Empty for none."},
			{Name: "disableDefaultSink", Label: "Disable the _Default sink for new projects", Type: "checkbox",
				Default: strconv.FormatBool(st.GetDisableDefaultSink())},
			{Name: "kmsKeyName", Label: "KMS key name", Type: "text", Default: st.GetKmsKeyName(),
				Help: "projects/P/locations/L/keyRings/R/cryptoKeys/K. Not supported: the API refuses a key."},
		}},
	}, nil
}

// Edit is UpdateSettings with every field the form holds in the mask.
func (p loggingSettingsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	if len(path) != 1 || path[0] != settingsName(project) {
		return errors.New("only the project's settings can be edited here")
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_UpdateSettings_FullMethodName, path[0]); err != nil {
		return err
	}
	_, err = a.ConfigServer().UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: path[0],
		Settings: &loggingpb.Settings{StorageLocation: strings.TrimSpace(values["storageLocation"]),
			DisableDefaultSink: values["disableDefaultSink"] == "true", KmsKeyName: strings.TrimSpace(values["kmsKeyName"])},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"storage_location", "disable_default_sink", "kms_key_name"}}})
	return err
}

var (
	_ console.Driller = loggingSettingsProvider{}
	_ console.Editor  = loggingSettingsProvider{}
)
