package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
)

// TestConsoleLoggingSettings (#1311): the Logs Storage settings screen lists
// the project's settings from GetSettings, its page shows the settings and
// the CMEK settings as served, and Edit settings (the console's PATCH route)
// changes what the API's GetSettings then reads. A KMS key is refused with
// the API's own UNIMPLEMENTED, and another project's settings are not
// reachable from this one.
func TestConsoleLoggingSettings(t *testing.T) {
	srv, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	name := "projects/" + project + "/settings"
	u := srv.URL + "/api/resources/logging-settings?project=" + project

	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	var l struct {
		Items []struct {
			Name   string
			Fields map[string]string
		}
	}
	_ = json.NewDecoder(resp.Body).Decode(&l)
	_ = resp.Body.Close()
	if len(l.Items) != 1 || l.Items[0].Name != name || l.Items[0].Fields["Default sink"] != "Enabled" {
		t.Fatalf("listing = %+v; want the project's settings with the default sink on", l)
	}

	if code, out := consolePatch(t, u, []string{name}, map[string]string{"storageLocation": "us-central1", "disableDefaultSink": "true"}); code != http.StatusOK {
		t.Fatalf("Edit settings = %d %s", code, out)
	}
	st, err := svc.admin.ConfigServer().GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: name})
	if err != nil || st.GetStorageLocation() != "us-central1" || !st.GetDisableDefaultSink() {
		t.Fatalf("GetSettings after Edit = %v, %v", st, err)
	}

	p := loggingSettingsProvider{loggingAdminScreen{svc: svc}}
	d, err := p.Detail(ctx, project, []string{name})
	if err != nil || d.Edit == nil {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	page := fmt.Sprint(d.Summary, d.Sections)
	for _, want := range []string{"us-central1", "Disabled", "CMEK settings", "projects/proj-1/cmekSettings",
		"service-proj-1@gcp-sa-logging.iam.gserviceaccount.com"} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings page lacks %q: %s", want, page)
		}
	}

	code, out := consolePatch(t, u, []string{name}, map[string]string{"kmsKeyName": "projects/proj-1/locations/global/keyRings/r/cryptoKeys/k"})
	if code == http.StatusOK || !strings.Contains(out, "not supported") {
		t.Errorf("a KMS key = %d %s; want the API's refusal", code, out)
	}
	if err := p.Edit(ctx, project, []string{"projects/other-1/settings"}, map[string]string{}); err == nil {
		t.Error("another project's settings were edited")
	}
	if d, _ := p.Detail(ctx, project, []string{"projects/other-1/settings"}); d.Unavailable == "" {
		t.Error("another project's settings page was shown")
	}
}
