//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// withIntercept sets LogSink.intercept_children (field 10 of
// google/logging/v2/logging_config.proto), which cloud.google.com/go/logging
// v1.20.0's loggingpb lacks, as a client built from the current proto sends it.
func withIntercept(sk *loggingpb.LogSink) *loggingpb.LogSink {
	var u []byte
	u = protowire.AppendTag(u, 10, protowire.VarintType)
	u = protowire.AppendVarint(u, 1)
	sk.ProtoReflect().SetUnknown(u)
	return sk
}

func hasIntercept(sk *loggingpb.LogSink) bool {
	b := sk.ProtoReflect().GetUnknown()
	for len(b) > 0 {
		n, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			return false
		}
		b = b[l:]
		if n == 10 && typ == protowire.VarintType {
			v, _ := protowire.ConsumeVarint(b)
			return v != 0
		}
		m := protowire.ConsumeFieldValue(n, typ, b)
		if m < 0 {
			return false
		}
		b = b[m:]
	}
	return false
}

// TestLoggingInterceptingSinks (#1521): a folder sink with include_children
// and intercept_children, made with the official ConfigClient over gRPC,
// routes what it matches from a project in the folder to its destination
// project, and the project's own _Default bucket and sinks do not get it
// ("Any logs matched by the sink won't be included by non-_Required sinks
// owned by child resources"); what it does not match stays. GetSink and
// ListSinks answer the field, the documented restrictions are
// INVALID_ARGUMENT, and an update naming it clears it. The console's Logs
// Explorer shows the entry in the destination project's _Default bucket only.
// covers: google.logging.v2.ConfigServiceV2/CreateSink, google.logging.v2.ConfigServiceV2/GetSink, google.logging.v2.ConfigServiceV2/ListSinks, google.logging.v2.ConfigServiceV2/UpdateSink, google.logging.v2.ConfigServiceV2/DeleteSink, google.logging.v2.LoggingServiceV2/WriteLogEntries, google.logging.v2.LoggingServiceV2/ListLogEntries
func TestLoggingInterceptingSinks(t *testing.T) {
	h := New(t)
	opts := loggingClientOptions(h)
	ctx := h.Context()
	folder, project := loggingParentsFixture(t, h, "lic-")
	cc, err := vlogging.NewConfigClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	lc, err := vlogging.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	dest := "logging.googleapis.com/projects/" + h.Project()

	for what, sk := range map[string]struct {
		parent string
		sink   *loggingpb.LogSink
	}{
		"a project's sink":         {"projects/" + project, &loggingpb.LogSink{Name: "bad", Destination: dest}},
		"without include_children": {folder, &loggingpb.LogSink{Name: "bad", Destination: dest}},
		"to a bucket": {folder, &loggingpb.LogSink{Name: "bad", IncludeChildren: true,
			Destination: dest + "/locations/global/buckets/_Default"}},
	} {
		_, err := cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: sk.parent, Sink: withIntercept(sk.sink)})
		if err == nil || !strings.Contains(err.Error(), "InvalidArgument") || !strings.Contains(err.Error(), "intercept_children") {
			t.Errorf("CreateSink %s: %v; want INVALID_ARGUMENT naming intercept_children", what, err)
		}
	}

	created, err := cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: folder, Sink: withIntercept(&loggingpb.LogSink{
		Name: "grab", Destination: dest, IncludeChildren: true, Filter: `labels.route="grab"`})})
	if err != nil {
		t.Fatalf("CreateSink: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: folder + "/sinks/grab"}) })
	if !hasIntercept(created) {
		t.Errorf("CreateSink answered no intercept_children: %v", created)
	}
	got, err := cc.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: folder + "/sinks/grab"})
	if err != nil || !hasIntercept(got) {
		t.Errorf("GetSink = %v, %v; want intercept_children", got, err)
	}
	it := cc.ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: folder})
	if s, err := it.Next(); err != nil || !hasIntercept(s) {
		t.Errorf("ListSinks = %v, %v; want intercept_children", s, err)
	}

	write := func(text, route string) {
		t.Helper()
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: "projects/" + project + "/logs/app",
			Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries: []*loggingpb.LogEntry{{Labels: map[string]string{"route": route},
				Payload: &loggingpb.LogEntry_TextPayload{TextPayload: text}}}}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(resource, text string) bool {
		t.Helper()
		got := entryTexts(t, h, lc, resource)
		if !slices.Contains(got, text) {
			t.Logf("%s holds %v", resource, got)
			return false
		}
		return true
	}
	grabbed, stays := "grab-"+suffix, "stay-"+suffix
	write(grabbed, "grab")
	write(stays, "other")
	for _, tc := range []struct {
		resource, text string
		want           bool
	}{
		{"projects/" + project, grabbed, false},
		{"projects/" + project, stays, true},
		{"projects/" + h.Project(), grabbed, true},
		{"projects/" + h.Project(), stays, false},
	} {
		if got := read(tc.resource, tc.text); got != tc.want {
			t.Errorf("%s holds %s: %v, want %v", tc.resource, tc.text, got, tc.want)
		}
	}

	// The console's Logs Explorer, scoped to a _Default bucket's _AllLogs
	// view, shows the intercepted entry in the destination project's, and
	// not in that of the project it was written in.
	for p, want := range map[string]bool{h.Project(): true, project: false} {
		parent := "projects/" + p + "/locations/global/buckets/_Default/views/_AllLogs"
		q := url.Values{"view": {parent}}
		deadline := time.Now().Add(10 * time.Second)
		for {
			code, body := consoleDo(t, consoleAddr(t, h), http.MethodGet, "/api/logs?"+q.Encode(), "")
			if code == http.StatusOK && strings.Contains(body, grabbed) == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the Logs Explorer scoped to %s: %d %s; want the intercepted entry shown %v", parent, code, body, want)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	if _, err := cc.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: folder + "/sinks/grab", Sink: &loggingpb.LogSink{},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"intercept_children"}}}); err != nil {
		t.Fatalf("UpdateSink: %v", err)
	}
	if got, err := cc.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: folder + "/sinks/grab"}); err != nil || hasIntercept(got) {
		t.Errorf("GetSink after clearing = %v, %v", got, err)
	}
	again := "again-" + suffix
	write(again, "grab")
	if !read("projects/"+project, again) {
		t.Errorf("after clearing intercept_children, %s is not in projects/%s", again, project)
	}
}

// TestGcloudAndTerraformInterceptingSinks (#1521): `gcloud logging sinks
// create --folder --include-children --intercept-children`, describe and
// `update --no-intercept-children`, and google_logging_folder_sink with
// intercept_children = true applied with hashicorp/google 8.4.0 through
// `cloudburrow terraform`, planning clean, both through the harness (egress
// blocked).
func TestGcloudAndTerraformInterceptingSinks(t *testing.T) {
	h := New(t)
	opts := loggingClientOptions(h)
	ctx := h.Context()
	folder, _ := loggingParentsFixture(t, h, "lig-")
	folderID := strings.TrimPrefix(folder, "folders/")
	dest := "logging.googleapis.com/projects/" + h.Project()
	cc, err := vlogging.NewConfigClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	g := newGcloudSession(t, h)
	g.must("logging", "sinks", "create", "gsink", dest, "--folder="+folderID, "--include-children", "--intercept-children")
	if got := g.must("logging", "sinks", "describe", "gsink", "--folder="+folderID, "--format=value(includeChildren,interceptChildren)"); strings.TrimSpace(got) != "True\tTrue" {
		t.Errorf("sinks describe = %q; want True\\tTrue", got)
	}
	if s, err := cc.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: folder + "/sinks/gsink"}); err != nil || !hasIntercept(s) {
		t.Errorf("GetSink of gcloud's sink = %v, %v; want intercept_children", s, err)
	}
	g.must("logging", "sinks", "update", "gsink", dest, "--folder="+folderID, "--no-intercept-children")
	if got := g.must("logging", "sinks", "describe", "gsink", "--folder="+folderID, "--format=value(interceptChildren)"); strings.TrimSpace(got) != "" {
		t.Errorf("sinks describe after --no-intercept-children = %q; want it unset", got)
	}
	g.must("logging", "sinks", "delete", "gsink", "--folder="+folderID, "--quiet")

	m := newTFModule(t)
	m.write(fmt.Sprintf(`
resource "google_logging_folder_sink" "f" {
  name               = "tf-intercept"
  folder             = %q
  destination        = %q
  include_children   = true
  intercept_children = true
}
`, folder, dest))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	if s, err := cc.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: folder + "/sinks/tf-intercept"}); err != nil || !hasIntercept(s) || !s.GetIncludeChildren() {
		t.Errorf("GetSink of Terraform's sink = %v, %v; want include_children and intercept_children", s, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}
