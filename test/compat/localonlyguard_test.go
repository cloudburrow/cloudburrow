//go:build compat

package compat

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/unserved"
)

// The local-only guard (#1975). Every test here runs its tool under
// noGoogleEgress (HTTPS to a closed loopback proxy), so a request the guard
// missed fails at the proxy rather than leaving the machine, and the test
// tells the two apart: a refusal is CloudBurrow's message, not a proxy error.

const refusedLocally = "CloudBurrow refused this request locally"

// TestTerraformRefusesAnUnservedResourceLocally: a google_workflows_workflow,
// which no instance serves, applied through `cloudburrow terraform`, fails
// with the instance's refusal naming workflows_custom_endpoint. Before #1975
// the provider sent it to workflows.googleapis.com.
func TestTerraformRefusesAnUnservedResourceLocally(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager) // the refusal is served on its port
	m := newTFModule(t)
	m.write(`
resource "google_workflows_workflow" "w" {
  name            = "localonly"
  region          = "us-central1"
  source_contents = "main:\n  steps:\n  - done:\n      return: 1\n"
  deletion_protection = false
}
`)
	m.must("init", "-input=false", "-no-color")
	out, err := m.tf("apply", "-auto-approve", "-input=false", "-no-color")
	if err == nil {
		t.Fatalf("apply of an unserved resource succeeded:\n%s", lastLines(out, 20))
	}
	if !strings.Contains(out, refusedLocally) || !strings.Contains(out, "workflows_custom_endpoint") {
		t.Errorf("apply failed, but not with CloudBurrow's refusal naming workflows_custom_endpoint:\n%s", lastLines(out, 30))
	}
	if strings.Contains(out, "proxyconnect") || strings.Contains(out, "googleapis.com") {
		t.Errorf("the provider tried Google:\n%s", lastLines(out, 30))
	}
}

// TestGcloudRefusesAnUnservedAPILocally: `gcloud workflows list`, an API no
// instance serves, through gcloud-setup's configuration, fails at once with
// the instance's refusal naming the workflows override (a 400, which gcloud
// does not retry), never at Google.
func TestGcloudRefusesAnUnservedAPILocally(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	gdir := t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	conf, _ := os.ReadFile(filepath.Join(gdir, "configurations", "config_"+name))
	if !strings.Contains(string(conf), "workflows = http://") || !strings.Contains(string(conf), unserved.PathPrefix+"workflows/") {
		t.Fatalf("the configuration does not refuse workflows:\n%s", conf)
	}
	cmd := exec.Command(gcloud, "workflows", "list", "--location", "us-central1", "--project", h.Project(), "--quiet")
	cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name)
	b, err := cmd.CombinedOutput()
	out := string(b)
	if err == nil {
		t.Fatalf("gcloud workflows list succeeded:\n%s", out)
	}
	if !strings.Contains(out, refusedLocally) || !strings.Contains(out, "workflows is not served") {
		t.Errorf("gcloud failed, but not with CloudBurrow's refusal:\n%s", lastLines(out, 20))
	}
	if strings.Contains(out, "googleapis.com") || strings.Contains(out, "ProxyError") {
		t.Errorf("gcloud tried Google:\n%s", lastLines(out, 20))
	}
}

// TestPinnedToolsGainNoUnmappedEndpoint reads the endpoint settings of the
// real pinned tools, as scripts/endpoint-inventory.sh does, and fails when
// one has a setting internal/unserved's inventory lacks: a new provider
// argument or gcloud override would otherwise stay at its Google default.
func TestPinnedToolsGainNoUnmappedEndpoint(t *testing.T) {
	t.Run("hashicorp/google", func(t *testing.T) {
		New(t)
		m := newTFModule(t)
		m.write("")
		m.must("init", "-input=false", "-no-color")
		out := m.must("providers", "schema", "-json")
		var schema struct {
			ProviderSchemas map[string]struct {
				Provider struct {
					Block struct {
						Attributes map[string]json.RawMessage `json:"attributes"`
					} `json:"block"`
				} `json:"provider"`
			} `json:"provider_schemas"`
		}
		// The wrapper prints its own lines on stderr; the JSON is the line
		// that starts with {.
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "{") {
				if err := json.Unmarshal([]byte(line), &schema); err != nil {
					t.Fatal(err)
				}
			}
		}
		p, ok := schema.ProviderSchemas["registry.terraform.io/hashicorp/google"]
		if !ok {
			t.Fatalf("no hashicorp/google schema in:\n%s", lastLines(out, 5))
		}
		var got []string
		for a := range p.Provider.Block.Attributes {
			if strings.HasSuffix(a, "_custom_endpoint") {
				got = append(got, a)
			}
		}
		sort.Strings(got)
		if want := unserved.TerraformEndpoints(false); !slices.Equal(got, want) {
			t.Errorf("hashicorp/google %s's endpoint arguments differ from internal/unserved's inventory; rerun scripts/endpoint-inventory.sh:\n got %d %v\nwant %d", googleProviderVersion, len(got), missing(got, want), len(want))
		}
	})
	t.Run("gcloud", func(t *testing.T) {
		gcloud := gcloudBinary(t)
		cmd := exec.Command(gcloud, "info", "--format=value(installation.sdk_root)")
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+t.TempDir())
		root, err := cmd.Output()
		if err != nil {
			t.Fatalf("gcloud info: %v", err)
		}
		src, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(root)), "lib", "googlecloudsdk", "core", "properties.py"))
		if err != nil {
			t.Fatal(err)
		}
		got := gcloudOverrideProperties(t, string(src))
		if want := unserved.GcloudOverrides(); !slices.Equal(got, want) {
			t.Errorf("gcloud's api_endpoint_overrides differ from internal/unserved's inventory (gcloud %s); rerun scripts/endpoint-inventory.sh: new %v", unserved.GcloudVersion, missing(got, want))
		}
	})
}

// gcloudOverrideProperties are the names _SectionApiEndpointOverrides adds.
func gcloudOverrideProperties(t *testing.T, src string) []string {
	t.Helper()
	start := strings.Index(src, "class _SectionApiEndpointOverrides")
	if start < 0 {
		t.Fatal("properties.py has no _SectionApiEndpointOverrides")
	}
	body := src[start+1:]
	if end := regexp.MustCompile(`\nclass `).FindStringIndex(body); end != nil {
		body = body[:end[0]]
	}
	var out []string
	for _, m := range regexp.MustCompile(`_Add\(\s*'([a-z0-9_]+)'`).FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// missing names what got has that want does not.
func missing(got, want []string) []string {
	var out []string
	for _, g := range got {
		if !slices.Contains(want, g) {
			out = append(out, g)
		}
	}
	return out
}
