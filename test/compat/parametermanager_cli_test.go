//go:build compat

package compat

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pmpb "cloud.google.com/go/parametermanager/apiv1/parametermanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGcloudParameterManager (#2054): gcloud parametermanager, through
// gcloud-setup's parametermanager override alone, creates a global and a
// regional JSON parameter, a version of each from a file, describes,
// lists and renders it (its secret reference replaced), disables the
// version, and deletes both; the official client reads back what gcloud
// made.
func TestGcloudParameterManager(t *testing.T) {
	h := New(t)
	c := parameterManagerClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_PARAMETERMANAGER
	sm := secretsClient(t, h)
	ctx := h.Context()
	g := newGcloudSession(t, h)
	secretID := fmt.Sprintf("gcloud-pm-secret-%d", time.Now().UnixNano()%1e9)
	secretVersion := createSecretWithVersion(t, h, sm, "projects/"+g.project, secretID, "from-secret")
	for _, loc := range []string{"global", "us-central1"} {
		id := fmt.Sprintf("gcloud-pm-%s-%d", loc, time.Now().UnixNano()%1e6)
		args := []string{"--location", loc, "--project", g.project, "--quiet"}
		run := func(a ...string) string {
			t.Helper()
			out, err := g.run(nil, append(append([]string{"parametermanager", "parameters"}, a...), args...)...)
			if err != nil {
				t.Fatalf("gcloud parametermanager parameters %s: %v\n%s", strings.Join(a, " "), err, out)
			}
			return out
		}
		run("create", id, "--parameter-format", "JSON", "--labels", "team=burrow")
		name := "projects/" + g.project + "/locations/" + loc + "/parameters/" + id
		p, err := c.GetParameter(ctx, &pmpb.GetParameterRequest{Name: name})
		if err != nil || p.GetFormat() != pmpb.ParameterFormat_JSON || p.GetLabels()["team"] != "burrow" {
			t.Fatalf("the parameter gcloud created: %+v, %v", p, err)
		}
		grantSecretAccessor(t, sm, "projects/"+g.project+"/secrets/"+secretID, p)
		file := filepath.Join(t.TempDir(), "v1.json")
		payload := `{"user": "app", "password": "__REF__(\"//secretmanager.googleapis.com/` + secretVersion + `\")"}`
		if err := os.WriteFile(file, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		run("versions", "create", "v1", "--parameter", id, "--payload-data-from-file", file)
		v, err := c.GetParameterVersion(ctx, &pmpb.GetParameterVersionRequest{Name: name + "/versions/v1"})
		if err != nil || string(v.GetPayload().GetData()) != payload {
			t.Fatalf("the version gcloud created: %+v, %v", v, err)
		}
		if out := run("describe", id, "--format", "value(format)"); !strings.Contains(out, "JSON") {
			t.Errorf("gcloud parametermanager parameters describe: %s", out)
		}
		if out := run("list", "--format", "value(name)"); !strings.Contains(out, id) {
			t.Errorf("gcloud parametermanager parameters list: %s", out)
		}
		out := run("versions", "render", "v1", "--parameter", id, "--format", "value(renderedPayload)")
		rendered := strings.TrimSpace(out)
		if b, err := base64.StdEncoding.DecodeString(rendered); err == nil {
			rendered = string(b)
		}
		if !strings.Contains(rendered, `"password": "from-secret"`) {
			t.Errorf("gcloud parametermanager parameters versions render: %s", out)
		}
		run("versions", "update", "v1", "--parameter", id, "--disabled")
		if v, err := c.GetParameterVersion(ctx, &pmpb.GetParameterVersionRequest{Name: name + "/versions/v1"}); err != nil || !v.GetDisabled() {
			t.Errorf("after gcloud versions update --disabled: %+v, %v", v, err)
		}
		run("versions", "delete", "v1", "--parameter", id)
		run("delete", id)
		if _, err := c.GetParameter(ctx, &pmpb.GetParameterRequest{Name: name}); status.Code(err) != codes.NotFound {
			t.Errorf("after gcloud delete: %v, want NOT_FOUND", err)
		}
	}
}

// TestTerraformParameterManager (#2054): google_parameter_manager_parameter
// and _parameter_version, and their regional twins, apply against this
// instance through parameter_manager_custom_endpoint and
// parameter_manager_regional_custom_endpoint, plan clean, update a
// parameter's labels in place and destroy; the official client reads back
// what Terraform made.
func TestTerraformParameterManager(t *testing.T) {
	h := New(t)
	c := parameterManagerClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_PARAMETERMANAGER
	ctx := h.Context()
	m := newTFModule(t)
	id := fmt.Sprintf("tf-pm-%d", time.Now().UnixNano()%1e6)
	conf := func(team string) string {
		return fmt.Sprintf(`
resource "google_parameter_manager_parameter" "p" {
  project      = %[1]q
  parameter_id = %[2]q
  format       = "YAML"
  labels       = { team = %[3]q }
}

resource "google_parameter_manager_parameter_version" "v" {
  parameter            = google_parameter_manager_parameter.p.id
  parameter_version_id = "v1"
  parameter_data       = "port: 8080\n"
}

resource "google_parameter_manager_regional_parameter" "r" {
  project      = %[1]q
  location     = "us-central1"
  parameter_id = "%[2]s-r"
  format       = "JSON"
}

resource "google_parameter_manager_regional_parameter_version" "rv" {
  parameter            = google_parameter_manager_regional_parameter.r.id
  parameter_version_id = "v1"
  parameter_data       = jsonencode({ port = 8080 })
}
`, m.project, id, team)
	}
	m.write(conf("one"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "projects/" + m.project + "/locations/global/parameters/" + id
	p, err := c.GetParameter(ctx, &pmpb.GetParameterRequest{Name: name})
	if err != nil || p.GetFormat() != pmpb.ParameterFormat_YAML || p.GetLabels()["team"] != "one" {
		t.Fatalf("the parameter Terraform made: %+v, %v", p, err)
	}
	v, err := c.GetParameterVersion(ctx, &pmpb.GetParameterVersionRequest{Name: name + "/versions/v1"})
	if err != nil || string(v.GetPayload().GetData()) != "port: 8080\n" {
		t.Fatalf("the version Terraform made: %+v, %v", v, err)
	}
	rname := "projects/" + m.project + "/locations/us-central1/parameters/" + id + "-r"
	if rv, err := c.GetParameterVersion(ctx, &pmpb.GetParameterVersionRequest{Name: rname + "/versions/v1"}); err != nil || string(rv.GetPayload().GetData()) != `{"port":8080}` {
		t.Fatalf("the regional version Terraform made: %+v, %v", rv, err)
	}
	m.planClean()
	m.write(conf("two"))
	m.apply()
	if p, err := c.GetParameter(ctx, &pmpb.GetParameterRequest{Name: name}); err != nil || p.GetLabels()["team"] != "two" {
		t.Fatalf("after the update: %+v, %v", p, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, n := range []string{name, rname} {
		if _, err := c.GetParameter(ctx, &pmpb.GetParameterRequest{Name: n}); status.Code(err) != codes.NotFound {
			t.Errorf("%s after destroy: %v; want NOT_FOUND", n, err)
		}
	}
}
