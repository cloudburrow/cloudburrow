//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGcloudLoggingFolderAndOrganizationSinks (#1395): `gcloud logging sinks
// create --folder --include-children` and `--organization`, describe, list
// and delete them, and `gcloud logging read --folder` reads the folder's own
// entries, written with the official Client, through the harness (egress
// blocked).
func TestGcloudLoggingFolderAndOrganizationSinks(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	folder, _ := loggingParentsFixture(t, h, "lgg-")
	folderID := strings.TrimPrefix(folder, "folders/")
	orgID := strings.TrimPrefix(localOrg, "organizations/")
	g := newGcloudSession(t, h)
	suffix := fmt.Sprint(time.Now().UnixNano())
	dest := "logging.googleapis.com/projects/" + h.Project() + "/locations/global/buckets/_Default"

	g.must("logging", "sinks", "create", "fsink", dest, "--folder="+folderID, "--include-children", `--log-filter=severity>=WARNING`)
	if got := g.must("logging", "sinks", "describe", "fsink", "--folder="+folderID, "--format=value(destination,includeChildren,filter)"); !strings.Contains(got, dest+"\tTrue\tseverity>=WARNING") {
		t.Errorf("sinks describe --folder = %q", got)
	}
	osink := "osink-" + suffix
	g.must("logging", "sinks", "create", osink, "storage.googleapis.com/some-bucket", "--organization="+orgID)
	if got := g.must("logging", "sinks", "list", "--organization="+orgID, "--format=value(name)"); !strings.Contains(got, osink) {
		t.Errorf("sinks list --organization = %q", got)
	}
	g.must("logging", "sinks", "delete", osink, "--organization="+orgID, "--quiet")
	g.must("logging", "sinks", "delete", "fsink", "--folder="+folderID, "--quiet")

	lc, err := vlogging.NewClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: folder + "/logs/audit",
		Resource: &mrpb.MonitoredResource{Type: "global"},
		Entries:  []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "folder-" + suffix}}}}); err != nil {
		t.Fatal(err)
	}
	if got := g.must("logging", "read", "--folder="+folderID, "--format=value(textPayload)"); strings.TrimSpace(got) != "folder-"+suffix {
		t.Errorf("logging read --folder = %q; want the folder's entry", got)
	}
}

// TestTerraformLoggingFolderAndOrganizationSinks (#1395):
// google_logging_folder_sink (include_children) and
// google_logging_organization_sink apply with the official hashicorp/google
// provider through `cloudburrow terraform` (egress blocked), are read back
// with the official ConfigClient, plan clean, take a filter change in place,
// plan clean again, and are destroyed and confirmed NOT_FOUND.
// covers: google.logging.v2.ConfigServiceV2/CreateSink, google.logging.v2.ConfigServiceV2/GetSink, google.logging.v2.ConfigServiceV2/UpdateSink, google.logging.v2.ConfigServiceV2/DeleteSink
func TestTerraformLoggingFolderAndOrganizationSinks(t *testing.T) {
	h := New(t)
	opts := loggingClientOptions(h) // skips without CLOUDBURROW_TEST_LOGGING
	ctx := h.Context()
	folder, _ := loggingParentsFixture(t, h, "lgt-")
	config, err := vlogging.NewConfigClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer config.Close()
	m := newTFModule(t)
	orgSink := "tf-org-" + fmt.Sprint(time.Now().UnixNano())
	dest := "logging.googleapis.com/projects/" + h.Project() + "/locations/global/buckets/_Default"
	module := func(filter string) string {
		return fmt.Sprintf(`
resource "google_logging_folder_sink" "f" {
  name             = "tf-folder"
  folder           = %[1]q
  destination      = %[2]q
  filter           = %[3]q
  include_children = true
}
resource "google_logging_organization_sink" "o" {
  name        = %[4]q
  org_id      = %[5]q
  destination = %[2]q
  filter      = %[3]q
}
`, folder, dest, filter, orgSink, strings.TrimPrefix(localOrg, "organizations/"))
	}
	check := func(when, filter string) {
		t.Helper()
		for _, name := range []string{folder + "/sinks/tf-folder", localOrg + "/sinks/" + orgSink} {
			s, err := config.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: name})
			if err != nil {
				t.Fatalf("%s: GetSink %s: %v", when, name, err)
			}
			if s.GetFilter() != filter || s.GetDestination() != dest || s.GetIncludeChildren() != strings.HasPrefix(name, "folders/") {
				t.Errorf("%s: sink %s = %v", when, name, s)
			}
		}
	}
	m.write(module("severity>=ERROR"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "severity>=ERROR")
	m.planClean()
	m.write(module("severity>=WARNING"))
	m.apply()
	check("after the update", "severity>=WARNING")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, name := range []string{folder + "/sinks/tf-folder", localOrg + "/sinks/" + orgSink} {
		if _, err := config.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: name}); status.Code(err) != codes.NotFound {
			t.Errorf("GetSink %s after destroy: %v; want NotFound", name, err)
		}
	}
}
