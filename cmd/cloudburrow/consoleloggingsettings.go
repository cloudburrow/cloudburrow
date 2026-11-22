package main

// Cloud Logging's project settings (#1311), at /logs/settings under Logs
// Storage: GetSettings, UpdateSettings and GetCmekSettings on the same
// ConfigServiceV2 the transports serve (#1292), with the instance's fault
// rules applied first. The list holds the project's one settings resource;
// its page shows the settings and the CMEK settings as served, and Edit
// settings is UpdateSettings, so a refusal (a key outside the storage
// location, a disabled key, a malformed location) is the API's own. A KMS
// key is the project's default key for its new buckets (#1972).
//
// Since #2108 the screen takes the Log Router's parent picker, so an
// organization's or folder's default resource settings are edited here
// too: the storage location and key where its new projects' and folders'
// _Default and _Required buckets are made, the _Default sink off for them,
// and the defaultSinkConfig (mode, filter, exclusion filters) their
// _Default sink starts with.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
)

type loggingSettingsProvider struct{ loggingAdminScreen }

func (loggingSettingsProvider) ID() string    { return "logging-settings" }
func (loggingSettingsProvider) Title() string { return "Settings" }

const loggingSettingsNote = "Default resource settings apply only to resources created after they are set: a project or folder made under this resource gets its _Default and _Required buckets in the storage location (encrypted with the KMS key, when one is set), and its _Default sink disabled or with the default sink filter and exclusions. Existing buckets and sinks are not changed. " +
	"A KMS key is also the default customer-managed key of new log buckets: it must be in the storage location, and the KMS service account needs roles/cloudkms.cryptoKeyEncrypterDecrypter on it."

func defaultSinkWord(disabled bool) string {
	if disabled {
		return "Disabled"
	}
	return "Enabled"
}

func (p loggingSettingsProvider) settings(ctx context.Context, parent string) (*loggingpb.Settings, error) {
	a, err := p.admin()
	if err != nil {
		return nil, err
	}
	name := parent + "/settings"
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_GetSettings_FullMethodName, name); err != nil {
		return nil, err
	}
	return a.ConfigServer().GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: name})
}

func (p loggingSettingsProvider) cmek(ctx context.Context, parent string) (*loggingpb.CmekSettings, error) {
	a, err := p.admin()
	if err != nil {
		return nil, err
	}
	name := parent + "/cmekSettings"
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_GetCmekSettings_FullMethodName, name); err != nil {
		return nil, err
	}
	return a.ConfigServer().GetCmekSettings(ctx, &loggingpb.GetCmekSettingsRequest{Name: name})
}

func (p loggingSettingsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Storage location", "Default sink", "KMS key", "Default sink filter"}, NameColumn: "Settings", Noun: "settings",
		Note: loggingSettingsNote}
	a, err := p.admin()
	if err != nil {
		return console.Listing{}, err
	}
	out.Scope = p.bucketScope(ctx, project)
	parent, err := bucketParent(ctx, project)
	if err != nil {
		out.Prompt = "Cloud Logging settings are per project, folder or organization. Choose a project in the toolbar, or a parent in the picker."
		return out, nil
	}
	st, err := p.settings(ctx, parent)
	if err != nil {
		return console.Listing{}, err
	}
	sc, _ := a.SinkConfigOf(st.GetName())
	out.Items = []console.Resource{{Name: st.GetName(), Fields: map[string]string{
		"Storage location": orDash(st.GetStorageLocation()), "Default sink": defaultSinkWord(st.GetDisableDefaultSink()),
		"KMS key": orDash(st.GetKmsKeyName()), "Default sink filter": sinkConfigWord(sc)}}}
	out.Total = 1
	out.RowsOpenable = true
	return out, nil
}

// sinkConfigWord is a defaultSinkConfig in a word or two.
func sinkConfigWord(sc *logging.SinkConfig) string {
	if sc == nil {
		return "Built-in"
	}
	if sc.Mode == "OVERWRITE" {
		return "Overwritten"
	}
	return "Appended"
}

// exclusionLines is a defaultSinkConfig's exclusions, one "name: filter"
// a line, as the form takes them.
func exclusionLines(sc *logging.SinkConfig) string {
	if sc == nil {
		return ""
	}
	var lines []string
	for _, e := range sc.Exclusions {
		lines = append(lines, e.Name+": "+e.Filter)
	}
	return strings.Join(lines, "\n")
}

// parseExclusionLines reads the form's exclusion filters.
func parseExclusionLines(text string) ([]logging.SinkExclusion, error) {
	var out []logging.SinkExclusion
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, filter, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("exclusion filter %q is not NAME: FILTER", line)
		}
		out = append(out, logging.SinkExclusion{Name: strings.TrimSpace(name), Filter: strings.TrimSpace(filter)})
	}
	return out, nil
}

func (p loggingSettingsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	parent, err := bucketParent(ctx, project)
	if err != nil {
		return console.Detail{Prompt: "Choose a project in the toolbar, or a parent in the picker."}, nil
	}
	if path[0] != parent+"/settings" {
		return console.Detail{Unavailable: path[0] + " is not " + parent + "'s settings"}, nil
	}
	a, err := p.admin()
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	st, err := p.settings(ctx, parent)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	sc, _ := a.SinkConfigOf(st.GetName())
	cmekGroup := console.PropertyGroup{Heading: "CMEK settings"}
	if c, err := p.cmek(ctx, parent); err != nil {
		cmekGroup.Properties = []console.Property{{Label: "Unavailable", Value: consoleErr(err)}}
	} else {
		cmekGroup.Properties = []console.Property{
			{Label: "Name", Value: c.GetName()},
			{Label: "KMS key", Value: orDash(c.GetKmsKeyName())},
			{Label: "KMS key version", Value: orDash(c.GetKmsKeyVersionName())},
			{Label: "Service account", Value: c.GetServiceAccountId()},
		}
	}
	mode, filter := "", ""
	if sc != nil {
		mode, filter = sc.Mode, sc.Filter
	}
	sinkGroup := console.PropertyGroup{Heading: "Default sink configuration", Properties: []console.Property{
		{Label: "Mode", Value: orDash(mode)},
		{Label: "Filter", Value: orDash(filter)},
		{Label: "Exclusion filters", Value: orDash(exclusionLines(sc))},
	}}
	return console.Detail{
		Title: "Logs Storage settings",
		Summary: []console.Property{
			{Label: "Storage location", Value: orDash(st.GetStorageLocation())},
			{Label: "Default sink", Value: defaultSinkWord(st.GetDisableDefaultSink())},
		},
		Sections: []console.Section{{ID: "settings", Label: "Settings", Kind: console.KindProperties, Note: loggingSettingsNote, Groups: []console.PropertyGroup{
			{Heading: "Default resource settings", Properties: []console.Property{
				{Label: "Name", Value: st.GetName()},
				{Label: "Storage location", Value: orDash(st.GetStorageLocation())},
				{Label: "Default sink (_Default)", Value: defaultSinkWord(st.GetDisableDefaultSink())},
				{Label: "KMS key", Value: orDash(st.GetKmsKeyName())},
				{Label: "KMS service account", Value: st.GetKmsServiceAccountId()},
			}},
			sinkGroup,
			cmekGroup,
		}}},
		Edit: &console.EditForm{Label: "Edit settings", Note: "UpdateSettings: a KMS key must be in the default storage location. These settings apply to projects and folders created afterwards.", Fields: []console.Field{
			{Name: "storageLocation", Label: "Default storage location", Type: "text", Default: st.GetStorageLocation(),
				Help: "Where new projects' and folders' _Default and _Required buckets are made: global, or a region such as us-central1. Empty for none."},
			{Name: "disableDefaultSink", Label: "Disable the _Default sink for new resources", Type: "checkbox",
				Default: strconv.FormatBool(st.GetDisableDefaultSink())},
			{Name: "kmsKeyName", Label: "KMS key name", Type: "text", Default: st.GetKmsKeyName(),
				Help: "projects/P/locations/L/keyRings/R/cryptoKeys/K, in the default storage location; empty for none. New log buckets are encrypted with it."},
			{Name: "sinkMode", Label: "Default sink filter mode", Type: "select", Default: mode, Options: []string{"", "APPEND", "OVERWRITE"},
				Help: "Empty keeps the built-in _Default sink. APPEND adds the filter to the built-in filter; OVERWRITE replaces it."},
			{Name: "sinkFilter", Label: "Default sink filter", Type: "textarea", Default: filter,
				Help: "The inclusion filter new resources' _Default sink takes; required with OVERWRITE."},
			{Name: "sinkExclusions", Label: "Default sink exclusion filters", Type: "textarea", Default: exclusionLines(sc),
				Help: "One a line, NAME: FILTER. Added to new resources' _Default sink."},
		}},
	}, nil
}

// Edit is UpdateSettings with every field the form holds in the mask, and
// the defaultSinkConfig the JSON API's UpdateSettings takes.
func (p loggingSettingsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	parent, err := bucketParent(ctx, project)
	if err != nil {
		return err
	}
	if len(path) != 1 || path[0] != parent+"/settings" {
		return errors.New("only " + parent + "'s settings can be edited here")
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	var sc *logging.SinkConfig
	if mode := strings.TrimSpace(values["sinkMode"]); mode != "" {
		ex, err := parseExclusionLines(values["sinkExclusions"])
		if err != nil {
			return err
		}
		sc = &logging.SinkConfig{Mode: mode, Filter: strings.TrimSpace(values["sinkFilter"]), Exclusions: ex}
		if err := a.CheckSinkConfig(sc); err != nil {
			return err
		}
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_UpdateSettings_FullMethodName, path[0]); err != nil {
		return err
	}
	if _, err = a.ConfigServer().UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: path[0],
		Settings: &loggingpb.Settings{StorageLocation: strings.TrimSpace(values["storageLocation"]),
			DisableDefaultSink: values["disableDefaultSink"] == "true", KmsKeyName: strings.TrimSpace(values["kmsKeyName"])},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"storage_location", "disable_default_sink", "kms_key_name"}}}); err != nil {
		return err
	}
	if _, has := values["sinkMode"]; has {
		return a.SetSinkConfig(path[0], sc)
	}
	return nil
}

var (
	_ console.Driller = loggingSettingsProvider{}
	_ console.Editor  = loggingSettingsProvider{}
)
