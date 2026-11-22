//go:build compat

package compat

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	logging "google.golang.org/api/logging/v2"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// The _Default sink and default resource settings of #2108 (round 2).

// googleDefaultSinkFilter is the _Default sink's filter, as
// docs.cloud.google.com/logging/docs/export/configure_export_v2 prints it.
const googleDefaultSinkFilter = `NOT LOG_ID("cloudaudit.googleapis.com/activity") AND NOT LOG_ID("externalaudit.googleapis.com/activity") AND ` +
	`NOT LOG_ID("cloudaudit.googleapis.com/system_event") AND NOT LOG_ID("externalaudit.googleapis.com/system_event") AND ` +
	`NOT LOG_ID("cloudaudit.googleapis.com/access_transparency") AND NOT LOG_ID("externalaudit.googleapis.com/access_transparency")`

// projectIn makes a project under parent with the official Resource
// Manager client, and deletes it when the test ends.
func projectIn(t *testing.T, h *Harness, parent, prefix string) string {
	t.Helper()
	ctx := h.Context()
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(runShardEndpoint(h, envLoggingResourceManager, EnvResourceManager)))
	id := v1ID(h, prefix)
	op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id, Parent: parent}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateProject in %s: %v", parent, err)
	}
	t.Cleanup(func() {
		if op, err := pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + id}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	return id
}

// covers: google.logging.v2.ConfigServiceV2/GetSink
// covers: google.logging.v2.ConfigServiceV2/ListSinks
// covers: google.logging.v2.ConfigServiceV2/UpdateSink
// covers: google.logging.v2.ConfigServiceV2/DeleteSink
// covers: google.logging.v2.LoggingServiceV2/WriteLogEntries
// covers: google.logging.v2.LoggingServiceV2/ListLogEntries
//
// TestLoggingDefaultSink2108, with the official apiv2 ConfigClient and
// Client: a project's _Default sink is listed and read with Google's filter
// and its _Default bucket as destination; it can't be deleted; disabled, it
// routes nothing new and what it routed before stays; re-enabled with a
// filter and an exclusion, it routes what they say, and the project's
// exclusions still apply.
func TestLoggingDefaultSink2108(t *testing.T) {
	h := New(t)
	cc := loggingConfigClient(t, h) // asks for the Logging endpoint first
	lc := loggingWriter(t, h)
	ctx := h.Context()
	pp := "projects/" + v1ID(h, "ldf-")
	sinkName := pp + "/sinks/_Default"

	sk, err := cc.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: sinkName})
	if err != nil || sk.GetFilter() != googleDefaultSinkFilter || sk.GetDisabled() ||
		sk.GetDestination() != "logging.googleapis.com/"+pp+"/locations/global/buckets/_Default" {
		t.Fatalf("GetSink _Default = %v, %v", sk, err)
	}
	var names []string
	it := cc.ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: pp})
	for s, err := it.Next(); err == nil; s, err = it.Next() {
		names = append(names, s.GetName())
	}
	if !slices.Contains(names, "_Default") || !slices.Contains(names, "_Required") {
		t.Errorf("ListSinks = %v; want _Default and _Required", names)
	}
	wantLoggingCode(t, "deleting the _Default sink", cc.DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: sinkName}), codes.FailedPrecondition)
	t.Cleanup(func() {
		_, _ = cc.UpdateSink(context.Background(), &loggingpb.UpdateSinkRequest{SinkName: sinkName,
			Sink:       &loggingpb.LogSink{Destination: sk.GetDestination(), Filter: googleDefaultSinkFilter},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"destination", "filter", "disabled", "exclusions"}}})
	})

	write := func(text string, sev ltype.LogSeverity) {
		t.Helper()
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: pp + "/logs/app", Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries: []*loggingpb.LogEntry{{Severity: sev, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: text}}}}); err != nil {
			t.Fatalf("WriteLogEntries %s: %v", text, err)
		}
	}
	read := func() string { return strings.Join(listTexts(t, h, lc, `logName="`+pp+`/logs/app"`, pp), ",") }
	write("a-before", ltype.LogSeverity_INFO)
	if sk, err = cc.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: sinkName, Sink: &loggingpb.LogSink{Disabled: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}}); err != nil || !sk.GetDisabled() || sk.GetFilter() != googleDefaultSinkFilter {
		t.Fatalf("disabling the _Default sink = %v, %v", sk, err)
	}
	write("b-while-disabled", ltype.LogSeverity_INFO)
	if got := read(); got != "a-before" {
		t.Errorf("with the _Default sink disabled the project reads %q; want a-before alone", got)
	}

	ex, err := cc.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: pp,
		Exclusion: &loggingpb.LogExclusion{Name: "noisy", Filter: `textPayload = "c-noisy"`}})
	if err != nil {
		t.Fatalf("CreateExclusion: %v", err)
	}
	t.Cleanup(func() {
		_ = cc.DeleteExclusion(context.Background(), &loggingpb.DeleteExclusionRequest{Name: pp + "/exclusions/" + ex.GetName()})
	})
	if sk, err = cc.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: sinkName, Sink: &loggingpb.LogSink{
		Filter: `severity >= WARNING`, Exclusions: []*loggingpb.LogExclusion{{Name: "skip", Filter: `textPayload = "c-skip"`}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled", "filter", "exclusions"}}}); err != nil || sk.GetDisabled() || len(sk.GetExclusions()) != 1 {
		t.Fatalf("re-enabling the _Default sink with a filter = %v, %v", sk, err)
	}
	write("c-info", ltype.LogSeverity_INFO)
	write("c-warning", ltype.LogSeverity_WARNING)
	write("c-skip", ltype.LogSeverity_ERROR)
	write("c-noisy", ltype.LogSeverity_ERROR)
	if got := read(); got != "a-before,c-warning" {
		t.Errorf("the project reads %q; want a-before and c-warning", got)
	}
	if got, err := cc.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: sinkName}); err != nil || got.GetFilter() != `severity >= WARNING` {
		t.Errorf("GetSink after the update = %v, %v", got, err)
	}
}

// covers: google.logging.v2.ConfigServiceV2/UpdateSettings
// covers: google.logging.v2.ConfigServiceV2/GetSettings
// covers: google.logging.v2.ConfigServiceV2/ListBuckets
// covers: google.logging.v2.ConfigServiceV2/GetBucket
//
// TestLoggingDefaultResourceSettings2108, with the official apiv2
// ConfigClient, Client and Resource Manager client, and Google's generated
// Logging JSON client for defaultSinkConfig: a folder's default resource
// settings (storage location, disable_default_sink, defaultSinkConfig)
// apply to a project made under it afterwards, whose _Default and _Required
// buckets are made in that location, whose sinks name them, and whose
// _Default sink starts disabled with the configured filter and exclusion;
// a project made before keeps the built-ins.
func TestLoggingDefaultResourceSettings2108(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	folder, before := loggingParentsFixture(t, h, "lds-")
	cc := loggingConfigClient(t, h)
	lc := loggingWriter(t, h)
	js := loggingJSONService(t, h)

	if st, err := cc.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: folder + "/settings",
		Settings:   &loggingpb.Settings{StorageLocation: "us-central1", DisableDefaultSink: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"storage_location", "disable_default_sink"}}}); err != nil || st.GetStorageLocation() != "us-central1" {
		t.Fatalf("UpdateSettings(%s) = %v, %v", folder, st, err)
	}
	// defaultSinkConfig: OVERWRITE needs a filter; APPEND is kept and read back.
	if _, err := js.Folders.UpdateSettings(folder, &logging.Settings{DefaultSinkConfig: &logging.DefaultSinkConfig{Mode: "OVERWRITE"}}).
		UpdateMask("defaultSinkConfig").Context(ctx).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("OVERWRITE without a filter = %v; want 400", err)
	}
	st, err := js.Folders.UpdateSettings(folder, &logging.Settings{DefaultSinkConfig: &logging.DefaultSinkConfig{Mode: "APPEND",
		Filter: "severity >= WARNING", Exclusions: []*logging.LogExclusion{{Name: "no-debug", Filter: `textPayload = "debug"`}}}}).
		UpdateMask("defaultSinkConfig").Context(ctx).Do()
	if err != nil || st.DefaultSinkConfig == nil || st.DefaultSinkConfig.Mode != "APPEND" || st.StorageLocation != "us-central1" || !st.DisableDefaultSink {
		t.Fatalf("UpdateSettings defaultSinkConfig = %+v, %v", st, err)
	}
	if got, err := js.Folders.GetSettings(folder).Context(ctx).Do(); err != nil || got.DefaultSinkConfig == nil ||
		len(got.DefaultSinkConfig.Exclusions) != 1 || got.DefaultSinkConfig.Filter != "severity >= WARNING" {
		t.Errorf("GetSettings = %+v, %v", got, err)
	}

	child := projectIn(t, h, folder, "ldc-")
	pp := "projects/" + child
	var names []string
	bit := cc.ListBuckets(ctx, &loggingpb.ListBucketsRequest{Parent: pp + "/locations/-"})
	for b, err := bit.Next(); err == nil; b, err = bit.Next() {
		names = append(names, b.GetName())
	}
	loc := pp + "/locations/us-central1/buckets/"
	if !slices.Equal(names, []string{loc + "_Default", loc + "_Required"}) {
		t.Errorf("the new project's buckets = %v; want _Default and _Required in us-central1", names)
	}
	if b, err := cc.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: loc + "_Required"}); err != nil || b.GetRetentionDays() != 400 || !b.GetLocked() {
		t.Errorf("GetBucket _Required = %v, %v", b, err)
	}
	_, err = cc.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: pp + "/locations/global/buckets/_Default"})
	wantLoggingCode(t, "the new project's global _Default", err, codes.NotFound)
	sk, err := cc.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: pp + "/sinks/_Default"})
	if err != nil || !sk.GetDisabled() || sk.GetDestination() != "logging.googleapis.com/"+loc+"_Default" ||
		sk.GetFilter() != "("+googleDefaultSinkFilter+") AND (severity >= WARNING)" || len(sk.GetExclusions()) != 1 || sk.GetExclusions()[0].GetName() != "no-debug" {
		t.Errorf("the new project's _Default sink = %v, %v", sk, err)
	}
	if rs, err := cc.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: pp + "/sinks/_Required"}); err != nil || rs.GetDestination() != "logging.googleapis.com/"+loc+"_Required" {
		t.Errorf("the new project's _Required sink = %v, %v", rs, err)
	}

	write := func(log, text string, sev ltype.LogSeverity) {
		t.Helper()
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: pp + "/logs/" + log, Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries: []*loggingpb.LogEntry{{Severity: sev, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: text}}}}); err != nil {
			t.Fatalf("WriteLogEntries %s: %v", text, err)
		}
	}
	write("app", "while-disabled", ltype.LogSeverity_ERROR)
	write("cloudaudit.googleapis.com%2Factivity", "audit", ltype.LogSeverity_NOTICE)
	if got := strings.Join(listTexts(t, h, lc, "", pp), ","); got != "audit" {
		t.Errorf("the new project reads %q; want the audit entry alone", got)
	}
	if got := strings.Join(listTexts(t, h, lc, "", loc+"_Required/views/_AllLogs"), ","); got != "audit" {
		t.Errorf("its _Required bucket holds %q", got)
	}
	if _, err := cc.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: pp + "/sinks/_Default", Sink: &loggingpb.LogSink{},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}}); err != nil {
		t.Fatalf("enabling the new project's _Default sink: %v", err)
	}
	write("app", "info", ltype.LogSeverity_INFO)
	write("app", "error", ltype.LogSeverity_ERROR)
	write("app", "debug", ltype.LogSeverity_ERROR)
	if got := strings.Join(listTexts(t, h, lc, "", loc+"_Default/views/_AllLogs"), ","); got != "error" {
		t.Errorf("its _Default bucket holds %q; want error alone", got)
	}

	// The project made before the settings keeps the built-ins.
	if sk, err := cc.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: "projects/" + before + "/sinks/_Default"}); err != nil || sk.GetDisabled() ||
		sk.GetFilter() != googleDefaultSinkFilter || !strings.Contains(sk.GetDestination(), "/locations/global/") {
		t.Errorf("the earlier project's _Default sink = %v, %v", sk, err)
	}
	if _, err := cc.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: "projects/" + before + "/locations/global/buckets/_Required"}); err != nil {
		t.Errorf("the earlier project's global _Required: %v", err)
	}
}

// covers: google.logging.v2.ConfigServiceV2/GetBucket
//
// TestLoggingDefaultSettingsCMEK2108, beside Cloud KMS: a folder's default
// key and storage location make a new project's _Default and _Required
// buckets in the key's location with that key, and what they hold is read
// through it.
func TestLoggingDefaultSettingsCMEK2108(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLogging)
	h.Endpoint(EnvKMS)
	ctx := h.Context()
	folder, _ := loggingParentsFixture(t, h, "ldk-")
	cc := loggingConfigClient(t, h)
	lc := loggingWriter(t, h)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	key := loggingCMEKKey(t, h, cc, "us-central1", "logging-born-"+suffix, "default")
	k := kmsClients(t, h)["grpc"]
	grant := func(parent string) {
		t.Helper()
		cs, err := cc.GetCmekSettings(ctx, &loggingpb.GetCmekSettingsRequest{Name: parent + "/cmekSettings"})
		if err != nil {
			t.Fatalf("GetCmekSettings(%s): %v", parent, err)
		}
		ih := k.ResourceIAM(key)
		p, err := ih.Policy(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p.Add("serviceAccount:"+cs.GetServiceAccountId(), iam.RoleName("roles/cloudkms.cryptoKeyEncrypterDecrypter"))
		if err := ih.SetPolicy(ctx, p); err != nil {
			t.Fatalf("grant %s's Logging service account: %v", parent, err)
		}
	}
	grant(folder)
	if _, err := cc.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: folder + "/settings",
		Settings:   &loggingpb.Settings{StorageLocation: "us-central1", KmsKeyName: key},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"storage_location", "kms_key_name"}}}); err != nil {
		t.Fatalf("UpdateSettings(%s) with a key: %v", folder, err)
	}
	id := v1ID(h, "ldkc-")
	grant("projects/" + id) // its Logging service account, before it exists
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(runShardEndpoint(h, envLoggingResourceManager, EnvResourceManager)))
	op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id, Parent: folder}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Cleanup(func() {
		if op, err := pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + id}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	loc := "projects/" + id + "/locations/us-central1/buckets/"
	for _, b := range []string{"_Default", "_Required"} {
		got, err := cc.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: loc + b})
		if err != nil || got.GetCmekSettings().GetKmsKeyName() != key || got.GetCmekSettings().GetKmsKeyVersionName() != key+"/cryptoKeyVersions/1" {
			t.Errorf("GetBucket %s = %v, %v; want the folder's key", b, got, err)
		}
	}
	if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: "projects/" + id + "/logs/app", Resource: &mrpb.MonitoredResource{Type: "global"},
		Entries: []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "sealed"}}}}); err != nil {
		t.Fatalf("WriteLogEntries: %v", err)
	}
	if got := strings.Join(listTexts(t, h, lc, "", loc+"_Default/views/_AllLogs"), ","); got != "sealed" {
		t.Errorf("the keyed _Default holds %q", got)
	}
}

// TestGcloudLoggingDefaultSink2108: gcloud logging sinks list and describe
// show the _Default sink with Google's filter, sinks update _Default
// --disabled and --no-disabled change it, and sinks delete _Default is
// refused.
func TestGcloudLoggingDefaultSink2108(t *testing.T) {
	h := New(t)
	_ = loggingConfigClient(t, h) // asks for the Logging endpoint first
	g := newGcloudSession(t, h)
	project := v1ID(h, "lgd-")
	p := "--project=" + project
	if got := g.must("logging", "sinks", "list", p, "--format=value(name)"); !strings.Contains(got, "_Default") || !strings.Contains(got, "_Required") {
		t.Errorf("sinks list = %q; want _Default and _Required", got)
	}
	if got := g.must("logging", "sinks", "describe", "_Default", p, "--format=value(filter)"); strings.TrimSpace(got) != googleDefaultSinkFilter {
		t.Errorf("sinks describe _Default = %q", got)
	}
	g.must("logging", "sinks", "update", "_Default", "--disabled", p)
	if got := g.must("logging", "sinks", "describe", "_Default", p, "--format=value(disabled)"); strings.TrimSpace(got) != "True" {
		t.Errorf("after --disabled, disabled = %q", got)
	}
	g.must("logging", "sinks", "update", "_Default", "--no-disabled", p)
	if got := g.must("logging", "sinks", "describe", "_Default", p, "--format=value(disabled)"); strings.TrimSpace(got) == "True" {
		t.Errorf("after --no-disabled, disabled = %q", got)
	}
	if out, err := g.run(nil, "logging", "sinks", "delete", "_Default", p, "--quiet"); err == nil || !strings.Contains(out, "FAILED_PRECONDITION") {
		t.Errorf("sinks delete _Default: %v %q; want the refusal", err, out)
	}
}
