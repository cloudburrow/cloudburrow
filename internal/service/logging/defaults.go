package logging

// The _Default sink and default resource settings (#2108), as Google
// documents them (docs.cloud.google.com/logging/docs/routing/overview,
// /logging/docs/export/configure_export_v2 and /logging/docs/default-settings,
// read 2026-11-22):
//
//   - every project, folder, organization and billing account has a
//     _Default sink, "The _Default log sink in a resource routes log entries
//     to the resource's _Default log bucket", whose filter is Google's six
//     NOT LOG_ID terms (the _Required sink's logs). "You can modify and
//     disable the _Default log sink ... change the destination ... modify
//     any existing filter and add exclusion filters", so UpdateSink of it
//     takes destination, filter, description, disabled and exclusions; "You
//     can't delete the _Default or the _Required sinks" (FAILED_PRECONDITION).
//     ListSinks and GetSink answer it. A parent whose _Default sink was
//     never changed routes as before (its own entries less the exclusions,
//     to its _Default bucket); once it is changed the sink is stored and
//     routes like any sink: "Disabling a log sink stops routing new log
//     entries but doesn't affect log entries that were previously routed",
//     and the parent's exclusions (ConfigServiceV2's) still apply to it;
//   - default resource settings: "Child resources inherit their parent's
//     default resource settings", and they "apply only to resources that
//     are created after the configuration is complete". When Resource
//     Manager creates a project or folder (OnCreated), the nearest ancestor
//     setting each one decides it:
//       - storage_location: "the _Default and _Required log buckets are
//         created in the specified location", so they are made there, and
//         the _Default and _Required sinks name them; "You can't change the
//         location of these log buckets after they are created";
//       - kms_key_name (with its storage_location, "If the default resource
//         settings for Cloud Logging have a CMEK setting, then the default
//         resource settings must also set the storage location"): both
//         buckets are made with that key, as cmek.go encrypts a bucket;
//       - disable_default_sink: "You can disable the _Default sinks for all
//         new resources", so the new resource's _Default sink is disabled;
//       - defaultSinkConfig (the JSON API's Settings field, which the
//         google.logging.v2 proto lacks, so REST only, rest.go):
//         "Overrides the built-in configuration for _Default sink": mode
//         OVERWRITE replaces the built-in filter, APPEND appends to it
//         ("Using the append mode with an empty filter will keep the sink
//         inclusion filter unchanged"), and the exclusions are added.
//
// Choices Google does not document (UNVERIFIED, docs re-read 2026-11-22:
// https://docs.cloud.google.com/logging/docs/default-settings): APPEND
// joins the two filters as (built-in) AND (filter); a defaultSinkConfig
// exclusion needs a name, as an exclusion does; disable_default_sink and
// defaultSinkConfig come from the nearest ancestor that set them, and the
// location and key from the nearest ancestor with a storage_location.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// sinkDefault is the _Default sink's ID.
const sinkDefault = "_Default"

// DefaultSinkFilter is the _Default sink's built-in filter, Google's.
var DefaultSinkFilter = func() string {
	terms := make([]string, len(requiredLogIDs))
	for i, id := range requiredLogIDs {
		terms[i] = `NOT LOG_ID("` + id + `")`
	}
	return strings.Join(terms, " AND ")
}()

// SinkConfig is a Settings' defaultSinkConfig, as the JSON API spells it.
type SinkConfig struct {
	Filter     string          `json:"filter,omitempty"`
	Mode       string          `json:"mode,omitempty"`
	Exclusions []SinkExclusion `json:"exclusions,omitempty"`
}

// SinkExclusion is one of a SinkConfig's exclusions.
type SinkExclusion struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Filter      string `json:"filter,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
}

// settingsExtra is what a parent's settings keep beside loggingpb.Settings:
// whether disable_default_sink was set (so a folder's false overrides its
// organization's true), and the defaultSinkConfig.
type settingsExtra struct {
	DisableSet bool        `json:"disableSet,omitempty"`
	SinkConfig *SinkConfig `json:"defaultSinkConfig,omitempty"`
}

func settingsExtraKey(p parent) string { return "logging/settingsx/" + p.seg() }

func (a *Admin) settingsExtra(p parent) settingsExtra {
	var x settingsExtra
	if raw, err := a.db.Get(settingsExtraKey(p)); err == nil {
		_ = json.Unmarshal(raw, &x)
	}
	return x
}

func (a *Admin) putSettingsExtra(p parent, x settingsExtra) error {
	raw, err := json.Marshal(x)
	if err != nil {
		return apierror.Internal(err, "encoding the settings")
	}
	if err := a.db.Put(settingsExtraKey(p), raw); err != nil {
		return apierror.Internal(err, "writing the settings")
	}
	return nil
}

// checkSinkConfig validates a defaultSinkConfig.
func (a *Admin) checkSinkConfig(c *SinkConfig) error {
	switch c.Mode {
	case "APPEND":
	case "OVERWRITE":
		if strings.TrimSpace(c.Filter) == "" {
			return apierror.InvalidArgument("defaultSinkConfig.filter cannot be empty or unset when the value of mode is OVERWRITE")
		}
	case "", "FILTER_WRITE_MODE_UNSPECIFIED":
		return apierror.InvalidArgument("defaultSinkConfig.mode is required: APPEND or OVERWRITE")
	default:
		return apierror.InvalidArgument("defaultSinkConfig.mode %q is not APPEND or OVERWRITE", c.Mode)
	}
	if err := checkFilter(c.Filter, false); err != nil {
		return apierror.InvalidArgument("defaultSinkConfig.filter: %v", apierror.From(err).Message)
	}
	seen := map[string]bool{}
	for i, e := range c.Exclusions {
		ex := &loggingpb.LogExclusion{Name: e.Name, Description: e.Description, Filter: e.Filter, Disabled: e.Disabled}
		if err := a.checkExclusion(ex); err != nil {
			return apierror.InvalidArgument("defaultSinkConfig.exclusions[%d]: %v", i, apierror.From(err).Message)
		}
		if seen[e.Name] {
			return apierror.InvalidArgument("defaultSinkConfig.exclusions[%d]: exclusion %q is listed twice", i, e.Name)
		}
		seen[e.Name] = true
	}
	return nil
}

// SinkConfigOf is p's defaultSinkConfig (name a parent), or nil.
func (a *Admin) SinkConfigOf(name string) (*SinkConfig, error) {
	p, err := a.parseSettings("name", name, "settings")
	if err != nil {
		return nil, err
	}
	return a.settingsExtra(p).SinkConfig, nil
}

// SetSinkConfig sets (c non-nil) or clears p's defaultSinkConfig, for
// UpdateSettings over JSON (rest.go).
func (a *Admin) SetSinkConfig(name string, c *SinkConfig) error {
	p, err := a.parseSettings("name", name, "settings")
	if err != nil {
		return err
	}
	if p.coll == collBilling {
		return apierror.FailedPrecondition("%s: Log Router settings can only be configured for organizations and folders (and, here, projects); they are read for billing accounts", name)
	}
	if c != nil {
		if err := a.checkSinkConfig(c); err != nil {
			return err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	x := a.settingsExtra(p)
	x.SinkConfig = c
	return a.putSettingsExtra(p, x)
}

// CheckSinkConfig validates c without storing it.
func (a *Admin) CheckSinkConfig(c *SinkConfig) error { return a.checkSinkConfig(c) }

// born is what a project or folder took from its ancestors' default
// resource settings when it was created.
type born struct {
	Location string `json:"location,omitempty"`
	KmsKey   string `json:"kmsKey,omitempty"`
	Disabled bool   `json:"disableDefaultSink,omitempty"`
	// Sink is the _Default sink it was made with, when not the built-in.
	Sink *SinkConfig `json:"defaultSinkConfig,omitempty"`
}

func bornKey(p parent) string { return "logging/born/" + p.seg() }

func (a *Admin) bornOf(p parent) born {
	var b born
	if a == nil || a.db == nil {
		return b
	}
	if raw, err := a.db.Get(bornKey(p)); err == nil {
		_ = json.Unmarshal(raw, &b)
	}
	return b
}

// builtinLoc is the location of p's _Default and _Required buckets: where
// its default resource settings made them, else global.
func (a *Admin) builtinLoc(p parent) string {
	if l := a.bornOf(p).Location; l != "" {
		return l
	}
	return "global"
}

// isBuiltin is whether ref is its parent's _Default or _Required bucket.
func (a *Admin) isBuiltin(ref bucketRef) bool {
	return builtinBucket(ref.bucket) && ref.location == a.builtinLoc(ref.parent)
}

// Born gives a project or folder just created (projects/{p}, folders/{n})
// its ancestors' default resource settings: Resource Manager's OnCreated
// calls it. A resource made again under a purged one's name starts over.
func (a *Admin) Born(name string) {
	p, rest, err := cutParent("name", name)
	if err != nil || rest != "" || (p.coll != collProjects && p.coll != collFolders) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.db.Delete(bornKey(p))
	if err := a.db.Delete(kindSink.key(p, sinkDefault)); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("Cloud Logging: a new resource's _Default sink could not be reset", "resource", name, "error", err)
	}
	var b born
	haveLoc, haveDisabled, haveSink := false, false, false
	for _, q := range a.ancestors(p) {
		st, err := a.settings(q)
		if err != nil {
			continue
		}
		x := a.settingsExtra(q)
		if !haveLoc && st.GetStorageLocation() != "" {
			b.Location, b.KmsKey, haveLoc = st.GetStorageLocation(), st.GetKmsKeyName(), true
		}
		if !haveDisabled && (x.DisableSet || st.GetDisableDefaultSink()) {
			b.Disabled, haveDisabled = st.GetDisableDefaultSink(), true
		}
		if !haveSink && x.SinkConfig != nil {
			b.Sink, haveSink = x.SinkConfig, true
		}
	}
	if b.Location == "global" {
		b.Location, b.KmsKey = "", ""
	}
	if b == (born{}) {
		return
	}
	raw, err := json.Marshal(b)
	if err == nil {
		err = a.db.Put(bornKey(p), raw)
	}
	if err != nil {
		slog.Warn("Cloud Logging: a new resource's default settings could not be kept", "resource", name, "error", err)
		return
	}
	if b.Location != "" {
		for _, id := range []string{bucketDefault, bucketRequired} {
			ref := bucketRef{p, b.Location, id}
			bk := a.defaultBucket(ref)
			now := timestamppb.New(a.now())
			bk.CreateTime, bk.UpdateTime = now, now
			if b.KmsKey != "" {
				if err := a.setBucketKey(context.Background(), ref, bk, b.KmsKey); err != nil {
					slog.Warn("Cloud Logging: a new resource's built-in bucket could not take the default key", "bucket", ref.name(), "error", err)
				}
			}
			if err := a.putBucket(ref, bk); err != nil {
				slog.Warn("Cloud Logging: a new resource's built-in bucket could not be made", "bucket", ref.name(), "error", err)
			}
		}
	}
	sk := a.builtinDefaultSink(p)
	if !b.Disabled && b.Sink == nil && b.Location == "" {
		return
	}
	now := timestamppb.New(a.now())
	sk.CreateTime, sk.UpdateTime = now, now
	sk.Disabled = b.Disabled
	if c := b.Sink; c != nil {
		switch {
		case c.Mode == "OVERWRITE":
			sk.Filter = c.Filter
		case strings.TrimSpace(c.Filter) != "":
			sk.Filter = "(" + sk.GetFilter() + ") AND (" + c.Filter + ")"
		}
		for _, e := range c.Exclusions {
			sk.Exclusions = append(sk.Exclusions, &loggingpb.LogExclusion{Name: e.Name, Description: e.Description,
				Filter: e.Filter, Disabled: e.Disabled, CreateTime: now, UpdateTime: now})
		}
	}
	if err := a.put(kindSink, p, sinkDefault, sk); err != nil {
		slog.Warn("Cloud Logging: a new resource's _Default sink could not be made", "resource", name, "error", err)
	}
}

// builtinDefaultSink is p's _Default sink as Google makes it.
func (a *Admin) builtinDefaultSink(p parent) *loggingpb.LogSink {
	epoch := timestamppb.New(time.Unix(0, 0))
	return &loggingpb.LogSink{
		Name:        sinkDefault,
		Destination: "logging.googleapis.com/" + (bucketRef{p, a.builtinLoc(p), bucketDefault}).name(),
		Filter:      DefaultSinkFilter,
		CreateTime:  epoch,
		UpdateTime:  epoch,
	}
}

// storedDefaultSink is p's _Default sink once changed (or made otherwise
// than the built-in), or nil while it is the built-in.
func (a *Admin) storedDefaultSink(p parent) *loggingpb.LogSink {
	m, err := a.get(kindSink, p, sinkDefault)
	if err != nil {
		return nil
	}
	return m.(*loggingpb.LogSink)
}

// defaultSink is p's _Default sink.
func (a *Admin) defaultSink(p parent) *loggingpb.LogSink {
	if sk := a.storedDefaultSink(p); sk != nil {
		return sk
	}
	return a.builtinDefaultSink(p)
}

// explicitDefault is whether p's _Default sink routes as a stored sink
// (sinkRoutes) rather than as the built-in.
func (a *Admin) explicitDefault(p parent) bool {
	if a == nil {
		return false
	}
	_, err := a.db.Get(kindSink.key(p, sinkDefault))
	return err == nil
}

// defaultSinkUpdatable are the _Default sink's fields an update_mask may
// name: "change the destination ... modify any existing filter and add
// exclusion filters", disable it, and describe it.
var defaultSinkUpdatable = map[string]bool{
	"destination": true, "filter": true, "description": true, "disabled": true, "exclusions": true,
}

// updateDefaultSink applies an UpdateSink of p's _Default sink. The caller
// holds a.mu.
func (a *Admin) updateDefaultSink(p parent, in *loggingpb.LogSink, paths []string) (*loggingpb.LogSink, error) {
	if len(paths) == 0 {
		paths = []string{"destination", "filter"}
	}
	old := a.defaultSink(p)
	sk := proto.Clone(old).(*loggingpb.LogSink)
	var mask []string
	for _, path := range paths {
		if path == "include_children" || path == "intercept_children" || path == "interceptChildren" {
			if in.GetIncludeChildren() {
				return nil, apierror.InvalidArgument("the _Default sink can't include children")
			}
			continue
		}
		mask = append(mask, path)
	}
	if err := applyMask(sk, in, &fieldmaskpb.FieldMask{Paths: mask}, defaultSinkUpdatable); err != nil {
		return nil, err
	}
	sk.Name = sinkDefault
	check := proto.Clone(sk).(*loggingpb.LogSink)
	check.Name = "Default"
	if err := a.checkSink(p, check); err != nil {
		return nil, err
	}
	a.stampSink(p, sk, old, "")
	if err := a.put(kindSink, p, sinkDefault, sk); err != nil {
		return nil, err
	}
	return sk, nil
}
