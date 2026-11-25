package beamrun

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestYAMLPackagesLicences: every package Yaml_Template's index is
// seeded with is pinned (a PyPI wheel URL whose file name it gives, and
// a SHA-256), is a pure-Python wheel, and has a recorded licence that is
// not GPL, LGPL or AGPL.
func TestYAMLPackagesLicences(t *testing.T) {
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	seen := map[string]bool{}
	for _, p := range YAMLPackages() {
		if p.Name == "" || p.Version == "" || !sha.MatchString(p.SHA256) {
			t.Errorf("%+v is not pinned", p)
		}
		if !strings.HasPrefix(p.URL, "https://files.pythonhosted.org/packages/") || path.Base(p.URL) != p.File {
			t.Errorf("%s's URL %s is not PyPI's file %s", p.Name, p.URL, p.File)
		}
		if !strings.HasSuffix(p.File, "-py3-none-any.whl") || !strings.Contains(p.File, "-"+p.Version+"-") {
			t.Errorf("%s is not a pure-Python wheel of %s %s", p.File, p.Name, p.Version)
		}
		if p.License == "" || strings.Contains(strings.ToUpper(p.License), "GPL") {
			t.Errorf("%s's licence %q is not recorded, or is GPL-family", p.Name, p.License)
		}
		if seen[p.Name] {
			t.Errorf("%s is listed twice", p.Name)
		}
		seen[p.Name] = true
	}
	// main.py's DEFAULT_DEPENDENCIES, and job_builder_util_transforms'
	// build backend.
	for _, n := range []string{"boto3", "botocore", "azure-storage-blob", "azure-core", "azure-identity", "poetry-core"} {
		if !seen[n] {
			t.Errorf("the seeded packages lack %s", n)
		}
	}
	if !strings.HasPrefix(ProvidedPythonIndex, "https://us-python.pkg.dev/") || pkgDevIndex.FindStringSubmatch(ProvidedPythonIndex) == nil {
		t.Errorf("ProvidedPythonIndex %s is not an Artifact Registry Python index", ProvidedPythonIndex)
	}
}

// TestReadProvidedYAML: the python module's build is read for
// Yaml_Template, whose spec's SDK is YAML and whose container is
// yaml-template; a v2 template's spec without sdkInfo is JAVA.
func TestReadProvidedYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]string{
		"yaml-template-generated-metadata.json":      `{"name": "YAML", "internalName": "Yaml_Template", "mainClass": "x.YAMLTemplate", "flexTemplate": true}`,
		"yaml-template-spec-generated-metadata.json": `{"image": "gcr.io/{project-id}/yaml-template", "sdkInfo": {"language": "YAML"}, "metadata": {}}`,
		"other-generated-metadata.json":              `{"name": "Other", "internalName": "BigQuery_Anomaly_Detection", "mainClass": "x.Other"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, "metadata", name), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readProvided(dir, ProvidedFlexNames[YAMLTemplateName])
	if err != nil {
		t.Fatal(err)
	}
	y := got[YAMLTemplateName]
	if len(got) != 1 || !y.Flex || y.Language != "YAML" || y.Container != "yaml-template" || y.Module != "python" {
		t.Errorf("readProvided = %+v", got)
	}
	if n, ok := ProvidedFlexTemplate(ProvidedFlexImage(y)); !ok || n != YAMLTemplateName {
		t.Errorf("ProvidedFlexTemplate(%s) = %q, %v", ProvidedFlexImage(y), n, ok)
	}
	if _, lang := flexSpecOf(filepath.Join(dir, "metadata", "none-generated-metadata.json"), "X"); lang != "JAVA" {
		t.Errorf("a template with no spec file is %s; want JAVA", lang)
	}
}

// TestRelayHostRewriter: the relay's HostRewriter (assets/relay.py) sends
// each HTTP/1.1 request of a stream with the Host it is given, and passes
// Content-Length and chunked bodies, which may hold a Host line
// themselves, through as they are, however the stream is split.
func TestRelayHostRewriter(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is not on PATH: the relay runs in Python: %v", err)
	}
	src, err := assets.ReadFile("assets/relay.py")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)class HostRewriter:.*?return b"".join\(out\)\n`).Find(src)
	if m == nil {
		t.Fatal("relay.py has no HostRewriter")
	}
	script := string(m) + `
stream = (b"GET /a HTTP/1.1\r\nHost: storage.googleapis.com\r\nAccept: */*\r\n\r\n"
          b"POST /b HTTP/1.1\r\nhost: storage.googleapis.com\r\nContent-Length: 30\r\n\r\nHost: storage.googleapis.com\r\n\r"
          b"PUT /c HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nHost:\r\n0\r\n\r\n"
          b"GET /d HTTP/1.1\r\nHost: storage.googleapis.com\r\n\r\n")
want = (b"GET /a HTTP/1.1\r\nHost: 10.0.0.1:9\r\nAccept: */*\r\n\r\n"
        b"POST /b HTTP/1.1\r\nHost: 10.0.0.1:9\r\nContent-Length: 30\r\n\r\nHost: storage.googleapis.com\r\n\r"
        b"PUT /c HTTP/1.1\r\nHost: 10.0.0.1:9\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nHost:\r\n0\r\n\r\n"
        b"GET /d HTTP/1.1\r\nHost: 10.0.0.1:9\r\n\r\n")
for step in (1, 2, 5, 13, len(stream)):
    h = HostRewriter("10.0.0.1:9")
    got = b"".join(h.feed(stream[i:i + step]) for i in range(0, len(stream), step))
    assert got == want, (step, got)
print("ok")
`
	out, err := exec.Command(py, "-I", "-c", script).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("HostRewriter: %v\n%s", err, out)
	}
}
