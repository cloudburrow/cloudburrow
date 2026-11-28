package beamrun

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadProvided: the build's generated metadata files are read by
// their template's name (internalName), its main class, staging options
// and streaming flag kept; a build missing one of ProvidedNames fails.
func TestReadProvided(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, n := range ProvidedNames {
		b := fmt.Sprintf(`{"name": "Display %d", "internalName": %q, "mainClass": "x.Main%d", "runtimeParameters": {"useSubscription": "true"}, "streaming": %v}`, i, n, i, i%2 == 0)
		if err := os.WriteFile(filepath.Join(dir, "metadata", fmt.Sprintf("t%d-generated-metadata.json", i)), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readProvided(dir, "v1")
	if err != nil {
		t.Fatal(err)
	}
	wc := got["Word_Count"]
	if wc.MainClass == "" || wc.RuntimeParameters["useSubscription"] != "true" || len(wc.Metadata) == 0 || len(got) != len(ProvidedNames) {
		t.Errorf("readProvided = %+v", got)
	}
	if err := os.Remove(filepath.Join(dir, "metadata", "t0-generated-metadata.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := readProvided(dir, "v1"); err == nil {
		t.Error("readProvided of a build missing a template succeeded")
	}
}

// TestReadProvidedFlex: a v2 module's build is read for the Flex
// Templates in it, each with its launcher image's container name from its
// spec file, and that image stands for the template's build.
func TestReadProvidedFlex(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	module := ProvidedFlexNames["GCS_Text_to_BigQuery_Flex"]
	for i, n := range moduleNames(module) {
		b := fmt.Sprintf(`{"name": "Display %d", "internalName": %q, "mainClass": "x.Main%d", "flexTemplate": true}`, i, n, i)
		if err := os.WriteFile(filepath.Join(dir, "metadata", fmt.Sprintf("T%d-generated-metadata.json", i)), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
		if n == "GCS_Text_to_BigQuery_Flex" {
			spec := `{"image": "gcr.io/some-project/gcs-to-bigquery:tag", "metadata": {}}`
			if err := os.WriteFile(filepath.Join(dir, "metadata", fmt.Sprintf("T%d-spec-generated-metadata.json", i)), []byte(spec), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := readProvided(dir, module)
	if err != nil {
		t.Fatal(err)
	}
	tpl := got["GCS_Text_to_BigQuery_Flex"]
	if !tpl.Flex || tpl.Container != "gcs-to-bigquery" || tpl.Module != module || len(got) != len(moduleNames(module)) {
		t.Errorf("readProvided = %+v", got)
	}
	if p := got["PubSub_to_BigQuery_Flex"]; p.Container != "pubsub-to-bigquery-flex" {
		t.Errorf("a template with no spec file has container %q", p.Container)
	}
	img := ProvidedFlexImage(tpl)
	if img != "gcr.io/dataflow-templates/gcs-to-bigquery/gcs-text-to-bigquery-flex:"+strings.ToLower(TemplatesRelease) {
		t.Errorf("ProvidedFlexImage = %s", img)
	}
	if n, ok := ProvidedFlexTemplate(img); !ok || n != "GCS_Text_to_BigQuery_Flex" {
		t.Errorf("ProvidedFlexTemplate(%s) = %q, %v", img, n, ok)
	}
	for _, other := range []string{"gcr.io/dataflow-templates/gcs-to-bigquery/gcs-text-to-bigquery-flex:2020-01-01",
		"us-docker.pkg.dev/p/r/gcs-text-to-bigquery-flex:" + strings.ToLower(TemplatesRelease),
		"gcr.io/dataflow-templates/a/no-such-template:" + strings.ToLower(TemplatesRelease), "gcr.io/dataflow-templates/gcs-text-to-bigquery-flex:" + strings.ToLower(TemplatesRelease)} {
		if _, ok := ProvidedFlexTemplate(other); ok {
			t.Errorf("ProvidedFlexTemplate(%s) is a provided template", other)
		}
	}
}
