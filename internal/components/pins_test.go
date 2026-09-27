package components_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/buildpacks"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
)

type entry struct {
	Image        string            `json:"image"`
	Version      string            `json:"version"`
	Digest       *string           `json:"digest"`
	Verification string            `json:"verification"`
	Manifests    map[string]string `json:"manifests"`
}

func inventory(t *testing.T) (map[string]map[string]entry, []any) {
	t.Helper()
	raw, err := os.ReadFile("../../dependencies.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Components map[string]map[string]json.RawMessage `json:"components"`
		Unresolved []any                                 `json:"unresolved"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]entry{}
	for g, items := range d.Components {
		out[g] = map[string]entry{}
		for k, v := range items {
			var e entry
			if json.Unmarshal(v, &e) == nil {
				out[g][k] = e
			}
		}
	}
	return out, d.Unresolved
}

func pinned(e entry) string {
	if e.Digest == nil {
		return e.Image + ":" + e.Version
	}
	return e.Image + "@" + *e.Digest
}

// The binary ships the pins dependencies.json records (#597): a Go constant
// that disagreed with the inventory would make the release's "tested
// against" list false without anything failing.
func TestPinsMatchTheInventory(t *testing.T) {
	inv, unresolved := inventory(t)
	node := inv["cluster"]["kubernetesNodeImage"]
	checks := map[string][2]string{
		"PubSubImage":      {components.PubSubImage, pinned(inv["emulatorBackends"]["googleCloudCliEmulatorsImage"])},
		"SpannerImage":     {components.SpannerImage, pinned(inv["emulatorBackends"]["spannerEmulator"])},
		"KnativeVersion":   {components.KnativeVersion, inv["serving"]["knativeServing"].Version},
		"net-kourier":      {components.KnativeVersion, inv["serving"]["knativeNetKourier"].Version},
		"DefaultNodeImage": {config.DefaultNodeImage, node.Image + ":" + node.Version + "@" + *node.Digest},
		"KindVersion":      {doctor.KindVersion, inv["cluster"]["kind"].Version},
		"BuilderImage":     {buildpacks.BuilderImage, pinned(inv["build"]["buildpacksBuilder"])},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q; dependencies.json has %q", name, c[0], c[1])
		}
	}
	want := map[string]string{}
	for _, comp := range []string{"knativeServing", "knativeNetKourier"} {
		for f, sum := range inv["serving"][comp].Manifests {
			want[f] = sum
		}
	}
	for _, m := range components.KnativeManifests() {
		if want[m.Name] != "sha256:"+m.SHA256 {
			t.Errorf("KnativeManifests %s = sha256:%s; dependencies.json has %q", m.Name, m.SHA256, want[m.Name])
		}
		delete(want, m.Name)
	}
	for f := range want {
		t.Errorf("dependencies.json pins %s, which KnativeManifests does not apply", f)
	}
	if !strings.Contains(config.DefaultNodeImage, "@sha256:") {
		t.Errorf("DefaultNodeImage %q is not pinned by digest", config.DefaultNodeImage)
	}
	if len(unresolved) != 0 {
		t.Errorf("dependencies.json still lists unresolved pins: %v", unresolved)
	}
}

// Every pulled image recorded as digest-verified has a digest, and every
// release YAML recorded as checksum-verified has its per-file hashes.
func TestInventoryVerificationsHaveTheirPins(t *testing.T) {
	inv, _ := inventory(t)
	for g, items := range inv {
		for k, e := range items {
			switch {
			case e.Verification == "image-digest" && e.Image != "" && (e.Digest == nil || !strings.HasPrefix(*e.Digest, "sha256:")):
				t.Errorf("%s.%s is image-digest verified with no digest", g, k)
			case e.Verification == "release-yaml-checksum" && len(e.Manifests) == 0:
				t.Errorf("%s.%s is release-yaml-checksum verified with no manifest hashes", g, k)
			}
		}
	}
}
