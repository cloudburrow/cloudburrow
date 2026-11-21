//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	observability "google.golang.org/api/observability/v1"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// The Cloud Logging gaps of #2108: the _Required bucket and sink, custom
// indexes, and the Observability API's trace scopes.

func loggingWriter(t *testing.T, h *Harness) *vlogging.Client {
	t.Helper()
	c, err := vlogging.NewClient(h.Context(), loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// listTexts reads every entry of resource names through the official
// client: each entry's text payload, or its jsonPayload.name.
func listTexts(t *testing.T, h *Harness, c *vlogging.Client, filter string, names ...string) []string {
	t.Helper()
	it := c.ListLogEntries(h.Context(), &loggingpb.ListLogEntriesRequest{ResourceNames: names, Filter: filter})
	var out []string
	for {
		e, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListLogEntries %v %q: %v", names, filter, err)
		}
		if p := e.GetTextPayload(); p != "" {
			out = append(out, p)
		} else {
			out = append(out, e.GetJsonPayload().GetFields()["name"].GetStringValue())
		}
	}
	sort.Strings(out)
	return out
}

// covers: google.logging.v2.ConfigServiceV2/GetSink
// covers: google.logging.v2.ConfigServiceV2/ListSinks
// covers: google.logging.v2.ConfigServiceV2/UpdateSink
// covers: google.logging.v2.ConfigServiceV2/DeleteSink
// covers: google.logging.v2.ConfigServiceV2/UpdateBucket
// covers: google.logging.v2.ConfigServiceV2/DeleteBucket
// covers: google.logging.v2.ConfigServiceV2/GetBucket
// covers: google.logging.v2.LoggingServiceV2/WriteLogEntries
// covers: google.logging.v2.LoggingServiceV2/ListLogEntries
//
// TestLoggingRequiredBucketAndSink (#2108), with the official apiv2
// ConfigClient and Client: the project's _Required sink is listed and read
// with Google's filter and its _Required bucket as destination, and can't
// be disabled, changed or deleted; the _Required bucket keeps 400 days,
// locked, and refuses a retention change and deletion; Admin Activity and
// System Event audit entries go to _Required, not _Default, and an
// exclusion matching them does not keep them out, while a Data Access entry
// stays in _Default; and reading the project reads both buckets.
func TestLoggingRequiredBucketAndSink(t *testing.T) {
	h := New(t)
	cc := loggingConfigClient(t, h) // asks for the Logging endpoint first
	lc := loggingWriter(t, h)
	ctx := h.Context()
	project := h.Project() + "-req2108"
	pp := "projects/" + project
	g := pp + "/locations/global/buckets/"

	sk, err := cc.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: pp + "/sinks/_Required"})
	if err != nil {
		t.Fatalf("GetSink _Required: %v", err)
	}
	wantFilter := `LOG_ID("cloudaudit.googleapis.com/activity") OR LOG_ID("externalaudit.googleapis.com/activity") OR ` +
		`LOG_ID("cloudaudit.googleapis.com/system_event") OR LOG_ID("externalaudit.googleapis.com/system_event") OR ` +
		`LOG_ID("cloudaudit.googleapis.com/access_transparency") OR LOG_ID("externalaudit.googleapis.com/access_transparency")`
	if sk.GetDestination() != "logging.googleapis.com/"+g+"_Required" || sk.GetFilter() != wantFilter || sk.GetDisabled() {
		t.Errorf("the _Required sink = %v", sk)
	}
	found := false
	it := cc.ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: pp})
	for s, err := it.Next(); err == nil; s, err = it.Next() {
		found = found || s.GetName() == "_Required"
	}
	if !found {
		t.Error("ListSinks has no _Required sink")
	}
	_, err = cc.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: pp + "/sinks/_Required", Sink: &loggingpb.LogSink{Disabled: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}})
	wantLoggingCode(t, "disabling the _Required sink", err, codes.FailedPrecondition)
	_, err = cc.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: pp + "/sinks/_Required", Sink: &loggingpb.LogSink{Filter: "severity>=ERROR"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"filter"}}})
	wantLoggingCode(t, "changing the _Required sink's filter", err, codes.FailedPrecondition)
	wantLoggingCode(t, "deleting the _Required sink", cc.DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: pp + "/sinks/_Required"}), codes.FailedPrecondition)

	b, err := cc.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: g + "_Required"})
	if err != nil || b.GetRetentionDays() != 400 || !b.GetLocked() {
		t.Errorf("GetBucket _Required = %v, %v; want 400 days, locked", b, err)
	}
	_, err = cc.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: g + "_Required", Bucket: &loggingpb.LogBucket{RetentionDays: 30},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retention_days"}}})
	wantLoggingCode(t, "changing _Required's retention", err, codes.FailedPrecondition)
	if err != nil && !strings.Contains(err.Error(), "retention period of the _Required bucket is 400 days and can't be changed") {
		t.Errorf("the retention refusal says %q", err)
	}
	wantLoggingCode(t, "deleting _Required", cc.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: g + "_Required"}), codes.FailedPrecondition)

	ex, err := cc.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: pp,
		Exclusion: &loggingpb.LogExclusion{Name: "drop-everything", Filter: `labels.run = "req2108"`}})
	if err != nil {
		t.Fatalf("CreateExclusion: %v", err)
	}
	t.Cleanup(func() {
		_ = cc.DeleteExclusion(context.Background(), &loggingpb.DeleteExclusionRequest{Name: pp + "/exclusions/" + ex.GetName()})
	})
	for _, log := range []string{"cloudaudit.googleapis.com%2Factivity", "cloudaudit.googleapis.com%2Fsystem_event", "cloudaudit.googleapis.com%2Fdata_access", "app"} {
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: pp + "/logs/" + log, Resource: &mrpb.MonitoredResource{Type: "global"},
			Labels: map[string]string{"run": "req2108"}, Entries: []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: log}}}}); err != nil {
			t.Fatalf("WriteLogEntries %s: %v", log, err)
		}
	}
	const audit = "cloudaudit.googleapis.com%2Factivity,cloudaudit.googleapis.com%2Fsystem_event"
	if got := strings.Join(listTexts(t, h, lc, `labels.run = "req2108"`, g+"_Required/views/_AllLogs"), ","); got != audit {
		t.Errorf("_Required holds %q; want %q (no exclusion reaches it)", got, audit)
	}
	if got := listTexts(t, h, lc, `labels.run = "req2108"`, g+"_Default/views/_AllLogs"); len(got) != 0 {
		t.Errorf("_Default holds %q; the exclusion keeps the rest out and the audit logs are _Required's", got)
	}
	if got := strings.Join(listTexts(t, h, lc, `labels.run = "req2108"`, pp), ","); got != audit {
		t.Errorf("reading %s = %q; want _Required's entries too", pp, got)
	}
}

// covers: google.logging.v2.ConfigServiceV2/CreateBucket
// covers: google.logging.v2.ConfigServiceV2/UpdateBucket
// covers: google.logging.v2.ConfigServiceV2/GetBucket
// covers: google.logging.v2.ConfigServiceV2/ListBuckets
// covers: google.logging.v2.ConfigServiceV2/CreateSink
// covers: google.logging.v2.LoggingServiceV2/ListLogEntries
//
// TestLoggingCustomIndexes (#2108), with the official apiv2 ConfigClient and
// Client, as docs.cloud.google.com/logging/docs/analyze/custom-index
// documents: index_configs are stored with their create_time and read back
// by GetBucket and ListBuckets; a path LogEntry does not define, an
// unspecified type, a duplicate path and a 21st field are INVALID_ARGUMENT;
// and an hour after it is made (the Logging clock), a query of the bucket
// compares the indexed field as the index's type: the page's own example,
// jsonPayload.value > 20 over {"value": 12345} and {"value": "3"}, matches
// A alone with an integer index and B alone with a string one.
func TestLoggingCustomIndexes(t *testing.T) {
	h := New(t)
	cc := loggingConfigClient(t, h)
	lc := loggingWriter(t, h)
	ctx := h.Context()
	project := h.Project() + "-idx2108"
	pp := "projects/" + project
	loc := pp + "/locations/global"
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)

	str, integer := loggingpb.IndexType_INDEX_TYPE_STRING, loggingpb.IndexType_INDEX_TYPE_INTEGER
	twentyOne := []*loggingpb.IndexConfig{}
	for i := 0; i < 21; i++ {
		twentyOne = append(twentyOne, &loggingpb.IndexConfig{FieldPath: fmt.Sprintf("jsonPayload.f%d", i), Type: str})
	}
	for what, ics := range map[string][]*loggingpb.IndexConfig{
		"an unspecified type":  {{FieldPath: "jsonPayload.value"}},
		"an undefined field":   {{FieldPath: "nosuch.value", Type: str}},
		"no field path":        {{Type: integer}},
		"a path indexed twice": {{FieldPath: "jsonPayload.a", Type: str}, {FieldPath: "jsonPayload.a", Type: integer}},
		"21 fields":            twentyOne,
	} {
		_, err := cc.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: loc, BucketId: "bad-" + suffix, Bucket: &loggingpb.LogBucket{IndexConfigs: ics}})
		wantLoggingCode(t, "CreateBucket with "+what, err, codes.InvalidArgument)
	}

	buckets := map[loggingpb.IndexType]string{integer: "ints-" + suffix, str: "strs-" + suffix}
	for typ, id := range buckets {
		b, err := cc.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: loc, BucketId: id,
			Bucket: &loggingpb.LogBucket{IndexConfigs: []*loggingpb.IndexConfig{{FieldPath: "jsonPayload.value", Type: typ}}}})
		if err != nil {
			t.Fatalf("CreateBucket %s: %v", id, err)
		}
		t.Cleanup(func() { _ = cc.DeleteBucket(context.Background(), &loggingpb.DeleteBucketRequest{Name: b.GetName()}) })
		if ic := b.GetIndexConfigs(); len(ic) != 1 || ic[0].GetFieldPath() != "jsonPayload.value" || ic[0].GetType() != typ || ic[0].GetCreateTime() == nil {
			t.Errorf("CreateBucket %s index_configs = %v", id, ic)
		}
		sk, err := cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: pp, Sink: &loggingpb.LogSink{Name: "to-" + id,
			Destination: "logging.googleapis.com/" + b.GetName(), Filter: `logName:"idx2108"`}})
		if err != nil {
			t.Fatalf("CreateSink: %v", err)
		}
		t.Cleanup(func() {
			_ = cc.DeleteSink(context.Background(), &loggingpb.DeleteSinkRequest{SinkName: pp + "/sinks/" + sk.GetName()})
		})
	}
	listed := false
	it := cc.ListBuckets(ctx, &loggingpb.ListBucketsRequest{Parent: loc})
	for b, err := it.Next(); err == nil; b, err = it.Next() {
		if strings.HasSuffix(b.GetName(), "/"+buckets[integer]) && len(b.GetIndexConfigs()) == 1 {
			listed = true
		}
	}
	if !listed {
		t.Error("ListBuckets does not show the bucket's index_configs")
	}

	for name, v := range map[string]any{"A": 12345, "B": "3"} {
		p, _ := structpb.NewStruct(map[string]any{"name": name, "value": v})
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: pp + "/logs/idx2108", Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries: []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: p}}}}); err != nil {
			t.Fatalf("WriteLogEntries: %v", err)
		}
	}
	view := func(typ loggingpb.IndexType) string { return loc + "/buckets/" + buckets[typ] + "/views/_AllLogs" }
	if got := strings.Join(listTexts(t, h, lc, "jsonPayload.value > 20", view(integer)), ","); got != "A,B" {
		t.Errorf("before the index applies, jsonPayload.value > 20 = %q; want A,B (the field's own types)", got)
	}
	advanceLoggingClock(t, h, time.Hour+time.Minute)
	if got := strings.Join(listTexts(t, h, lc, "jsonPayload.value > 20", view(integer)), ","); got != "A" {
		t.Errorf("integer index: jsonPayload.value > 20 = %q; want A", got)
	}
	if got := strings.Join(listTexts(t, h, lc, "jsonPayload.value > 20", view(str)), ","); got != "B" {
		t.Errorf("string index: jsonPayload.value > 20 = %q; want B", got)
	}
	if got := strings.Join(listTexts(t, h, lc, "jsonPayload.value > 50", view(str)), ","); got != "" {
		t.Errorf("string index: jsonPayload.value > 50 = %q; want neither", got)
	}

	// UpdateBucket adds a field; the kept index keeps its create_time.
	name := loc + "/buckets/" + buckets[integer]
	before, err := cc.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	after, err := cc.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: name, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"index_configs"}},
		Bucket: &loggingpb.LogBucket{IndexConfigs: []*loggingpb.IndexConfig{{FieldPath: "jsonPayload.value", Type: integer}, {FieldPath: "jsonPayload.req_status", Type: str}}}})
	if err != nil || len(after.GetIndexConfigs()) != 2 ||
		!after.GetIndexConfigs()[0].GetCreateTime().AsTime().Equal(before.GetIndexConfigs()[0].GetCreateTime().AsTime()) {
		t.Errorf("UpdateBucket index_configs = %v, %v", after, err)
	}
}

// covers: observability.projects.locations.traceScopes.create, observability.projects.locations.traceScopes.get, observability.projects.locations.traceScopes.list, observability.projects.locations.traceScopes.patch, observability.projects.locations.traceScopes.delete, observability.projects.locations.scopes.patch, observability.projects.locations.scopes.get
//
// TestObservabilityTraceScopes (#2108), through Google's generated
// Observability v1 client (Google publishes no other Go client for it):
// _Default holds the project and can't be changed or deleted; a trace scope
// of two projects is created, listed after _Default, patched by updateMask
// and set as the project's default trace scope through scopes.patch; a
// trace scope of 21 projects, of a folder, and one the project does not
// hold as the default are refused; and deleting the default makes _Default
// the default again.
func TestObservabilityTraceScopes(t *testing.T) {
	h := New(t)
	obs := observabilityService(t, h) // asks for the Logging endpoint first
	ctx := h.Context()
	pa, pb := h.Project(), h.Project()+"-ts2108"
	parent := "projects/" + pa + "/locations/global"
	ts := obs.Projects.Locations.TraceScopes

	def, err := ts.Get(parent + "/traceScopes/_Default").Context(ctx).Do()
	if err != nil || len(def.ResourceNames) != 1 || def.ResourceNames[0] != "projects/"+pa {
		t.Fatalf("traceScopes.get _Default = %+v, %v", def, err)
	}
	if _, err := ts.Patch(def.Name, &observability.TraceScope{Description: "x"}).UpdateMask("description").Context(ctx).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("patching _Default: %v; want 400", err)
	}
	if _, err := ts.Delete(def.Name).Context(ctx).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("deleting _Default: %v; want 400", err)
	}

	id := fmt.Sprintf("team-%d", time.Now().UnixNano()%1e9)
	made, err := ts.Create(parent, &observability.TraceScope{ResourceNames: []string{"projects/" + pa, "projects/" + pb}, Description: "two projects"}).
		TraceScopeId(id).Context(ctx).Do()
	if err != nil {
		t.Fatalf("traceScopes.create: %v", err)
	}
	t.Cleanup(func() { _, _ = ts.Delete(made.Name).Context(context.Background()).Do() })
	if made.Name != parent+"/traceScopes/"+id || made.CreateTime == "" || len(made.ResourceNames) != 2 {
		t.Errorf("traceScopes.create = %+v", made)
	}
	if _, err := ts.Create(parent, &observability.TraceScope{ResourceNames: []string{"projects/" + pa}}).TraceScopeId(id).Context(ctx).Do(); httpCode(err) != http.StatusConflict {
		t.Errorf("a second create of %s: %v; want 409", id, err)
	}
	var many []string
	for i := 0; i < 21; i++ {
		many = append(many, fmt.Sprintf("projects/%s-%d", pa, i))
	}
	for what, names := range map[string][]string{"21 projects": many, "a folder": {"folders/123"}, "no projects": nil} {
		if _, err := ts.Create(parent, &observability.TraceScope{ResourceNames: names}).TraceScopeId("bad-" + id).Context(ctx).Do(); httpCode(err) != http.StatusBadRequest {
			t.Errorf("a trace scope of %s: %v; want 400", what, err)
		}
	}
	list, err := ts.List(parent).Context(ctx).Do()
	if err != nil || len(list.TraceScopes) < 2 || !strings.HasSuffix(list.TraceScopes[0].Name, "/_Default") {
		t.Fatalf("traceScopes.list = %+v, %v", list, err)
	}
	got, err := ts.Patch(made.Name, &observability.TraceScope{ResourceNames: []string{"projects/" + pb}}).UpdateMask("resourceNames").Context(ctx).Do()
	if err != nil || len(got.ResourceNames) != 1 || got.Description != "two projects" {
		t.Errorf("traceScopes.patch = %+v, %v", got, err)
	}

	scopes := obs.Projects.Locations.Scopes
	scope := "projects/" + pa + "/locations/global/scopes/_Default"
	if _, err := scopes.Patch(scope, &observability.Scope{TraceScope: parent + "/traceScopes/none"}).UpdateMask("traceScope").Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("a missing trace scope as the default: %v; want 404", err)
	}
	if _, err := scopes.Patch(scope, &observability.Scope{TraceScope: made.Name}).UpdateMask("traceScope").Context(ctx).Do(); err != nil {
		t.Fatalf("scopes.patch traceScope: %v", err)
	}
	t.Cleanup(func() {
		_, _ = scopes.Patch(scope, &observability.Scope{TraceScope: parent + "/traceScopes/_Default"}).UpdateMask("traceScope").Context(context.Background()).Do()
	})
	if sc, err := scopes.Get(scope).Context(ctx).Do(); err != nil || sc.TraceScope != made.Name {
		t.Errorf("scopes.get after patch = %+v, %v", sc, err)
	}
	if _, err := ts.Delete(made.Name).Context(ctx).Do(); err != nil {
		t.Fatalf("traceScopes.delete: %v", err)
	}
	if _, err := ts.Get(made.Name).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("traceScopes.get after delete: %v; want 404", err)
	}
	if sc, err := scopes.Get(scope).Context(ctx).Do(); err != nil || !strings.HasSuffix(sc.TraceScope, "/traceScopes/_Default") {
		t.Errorf("after deleting the default trace scope, scopes.get = %+v, %v", sc, err)
	}
}

// TestGcloudLogging2108 (#2108): gcloud logging sinks describe _Required
// and buckets update _Required --retention-days (refused), buckets create
// --index, update --add-index and --remove-indexes, and gcloud
// observability trace-scopes create, describe, list, update and delete,
// through the harness's gcloud session (egress blocked).
func TestGcloudLogging2108(t *testing.T) {
	h := New(t)
	_ = loggingConfigClient(t, h) // asks for the Logging endpoint first
	g := newGcloudSession(t, h)
	p := "--project=" + h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)

	if got := g.must("logging", "sinks", "describe", "_Required", p, "--format=value(destination)"); strings.TrimSpace(got) !=
		"logging.googleapis.com/projects/"+h.Project()+"/locations/global/buckets/_Required" {
		t.Errorf("sinks describe _Required = %q", got)
	}
	if got := g.must("logging", "sinks", "list", p, "--format=value(name)"); !strings.Contains(got, "_Required") {
		t.Errorf("sinks list = %q; want _Required", got)
	}
	if out, err := g.run(nil, "logging", "buckets", "update", "_Required", "--location=global", "--retention-days=30", p); err == nil ||
		!strings.Contains(out, "can't be changed") {
		t.Errorf("buckets update _Required --retention-days: %v %q; want the refusal", err, out)
	}

	bucket := "idx-" + suffix
	g.must("logging", "buckets", "create", bucket, "--location=global", p, "--index=fieldPath=jsonPayload.req_status,type=INDEX_TYPE_INTEGER")
	t.Cleanup(func() { _, _ = g.run(nil, "logging", "buckets", "delete", bucket, "--location=global", p, "--quiet") })
	if got := g.must("logging", "buckets", "describe", bucket, "--location=global", p, "--format=value(indexConfigs[0].fieldPath,indexConfigs[0].type)"); strings.TrimSpace(got) !=
		"jsonPayload.req_status\tINDEX_TYPE_INTEGER" {
		t.Errorf("buckets describe after --index = %q", got)
	}
	g.must("logging", "buckets", "update", bucket, "--location=global", p, "--add-index=fieldPath=jsonPayload.user,type=INDEX_TYPE_STRING")
	g.must("logging", "buckets", "update", bucket, "--location=global", p, "--remove-indexes=jsonPayload.req_status")
	if got := g.must("logging", "buckets", "describe", bucket, "--location=global", p, "--format=value(indexConfigs[].fieldPath)"); strings.TrimSpace(got) != "jsonPayload.user" {
		t.Errorf("buckets describe after add and remove = %q", got)
	}

	scope := "gts-" + suffix
	g.must("observability", "trace-scopes", "create", scope, "--location=global", p, "--resource-names=projects/"+h.Project(), "--description=from gcloud")
	t.Cleanup(func() {
		_, _ = g.run(nil, "observability", "trace-scopes", "delete", scope, "--location=global", p, "--quiet")
	})
	if got := g.must("observability", "trace-scopes", "describe", scope, "--location=global", p, "--format=value(description)"); strings.TrimSpace(got) != "from gcloud" {
		t.Errorf("trace-scopes describe = %q", got)
	}
	if got := g.must("observability", "trace-scopes", "list", "--location=global", p, "--format=value(name)"); !strings.Contains(got, scope) || !strings.Contains(got, "_Default") {
		t.Errorf("trace-scopes list = %q", got)
	}
	g.must("observability", "trace-scopes", "update", scope, "--location=global", p, "--description=changed")
	if got := g.must("observability", "trace-scopes", "describe", scope, "--location=global", p, "--format=value(description)"); strings.TrimSpace(got) != "changed" {
		t.Errorf("trace-scopes describe after update = %q", got)
	}
	g.must("observability", "trace-scopes", "delete", scope, "--location=global", p, "--quiet")
}
