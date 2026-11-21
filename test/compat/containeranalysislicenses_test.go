//go:build compat

package compat

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	containeranalysis "cloud.google.com/go/containeranalysis/apiv1"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	gpb "google.golang.org/genproto/googleapis/grafeas/v1"
)

// licenseDemoBinary builds a small Linux program requiring
// github.com/google/uuid v1.6.0 (BSD-3-Clause), from the module cache
// only (GOPROXY=off), so its build info names the module as a Go binary
// in an image does. It returns the binary and the module's LICENSE.
func licenseDemoBinary(t *testing.T) (bin, license []byte) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/licdemo\n\ngo 1.22\n\nrequire github.com/google/uuid v1.6.0\n")
	write("go.sum", "github.com/google/uuid v1.6.0 h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0=\ngithub.com/google/uuid v1.6.0/go.mod h1:TIyPZe4MgqvfeYDBFedMoGGpEw/LqOeaOT+nhxU+yHo=\n")
	write("main.go", "package main\n\nimport (\n\t\"fmt\"\n\n\t\"github.com/google/uuid\"\n)\n\nfunc main() { fmt.Println(uuid.NewString()) }\n")
	out := filepath.Join(dir, "licdemo")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOPROXY=off", "GOFLAGS=-mod=mod", "GOWORK=off", "GOTOOLCHAIN=local")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build the license demo: %v: %s", err, b)
	}
	bin, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	mc, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	license, err = os.ReadFile(filepath.Join(strings.TrimSpace(string(mc)), "github.com", "google", "uuid@v1.6.0", "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	return bin, license
}

// licensedImage is an image holding npm, PyPI, Maven and Go packages
// whose licenses are known, in the files each ecosystem keeps them in.
func licensedImage(t *testing.T) v1.Image {
	t.Helper()
	bin, uuidLicense := licenseDemoBinary(t)
	var jar bytes.Buffer
	z := zip.NewWriter(&jar)
	for name, s := range map[string]string{
		"META-INF/MANIFEST.MF": "Manifest-Version: 1.0\n",
		"META-INF/maven/com.google.code.gson/gson/pom.xml": `<project><modelVersion>4.0.0</modelVersion>
<groupId>com.google.code.gson</groupId><artifactId>gson</artifactId><version>2.10.1</version>
<licenses><license><name>Apache-2.0</name><url>https://www.apache.org/licenses/LICENSE-2.0.txt</url></license></licenses></project>`,
		"META-INF/maven/com.google.code.gson/gson/pom.properties": "groupId=com.google.code.gson\nartifactId=gson\nversion=2.10.1\n",
	} {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(s))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	files := []struct {
		name string
		mode int64
		data []byte
	}{
		{"app/licdemo", 0o755, bin},
		{"root/go/pkg/mod/github.com/google/uuid@v1.6.0/LICENSE", 0o444, uuidLicense},
		{"app/node_modules/ms/package.json", 0o644, []byte(`{"name":"ms","version":"2.1.3","license":"MIT"}`)},
		{"app/node_modules/inherits/package.json", 0o644, []byte(`{"name":"inherits","version":"2.0.4","license":"ISC"}`)},
		{"app/node_modules/.package-lock.json", 0o644, []byte(`{"name":"app","lockfileVersion":3,"requires":true,"packages":{"node_modules/ms":{"version":"2.1.3","license":"MIT"},"node_modules/inherits":{"version":"2.0.4","license":"ISC"}}}`)},
		{"usr/local/lib/python3.11/site-packages/six-1.16.0.dist-info/METADATA", 0o644,
			[]byte("Metadata-Version: 2.1\nName: six\nVersion: 1.16.0\nLicense: MIT\nClassifier: License :: OSI Approved :: MIT License\n\n")},
		{"usr/local/lib/python3.11/site-packages/packaging-24.2.dist-info/METADATA", 0o644,
			[]byte("Metadata-Version: 2.4\nName: packaging\nVersion: 24.2\nLicense-Expression: Apache-2.0 OR BSD-2-Clause\n\n")},
		{"app/lib/gson-2.10.1.jar", 0o644, jar.Bytes()},
	}
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(f.data)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	b := layer.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.Append(empty.Image, mutate.Addendum{Layer: l, History: v1.History{CreatedBy: "compat test packages"}})
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", "amd64"
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// covers: grafeas.v1.Grafeas/ListOccurrences
//
// TestArtifactRegistryLanguagePackageLicenses (#2072): an image holding
// npm, PyPI, Maven and Go packages of known licenses is scanned, and each
// PACKAGE occurrence, read with the official grafeas client, carries the
// package's license as an SPDX expression, as Artifact Analysis reports
// licenses for "supported language packages"
// (https://docs.cloud.google.com/artifact-analysis/docs/view-licenses-dependencies).
// Offline, OSV-Scanner reads none of these (it asks deps.dev): they come
// from the image's own package.json, METADATA, POM and LICENSE files.
func TestArtifactRegistryLanguagePackageLicenses(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	parent := runParent(h)
	scanningAPI(t, h, h.Project())
	formatRepo(t, ctx, ar, parent, "scan-licenses", &arpb.Repository{Format: arpb.Repository_DOCKER})
	ca, err := containeranalysis.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ca.Close() })
	g := ca.GetGrafeasClient()

	img := licensedImage(t)
	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	ref := "us-central1-docker.pkg.localhost:" + port + "/" + h.Project() + "/scan-licenses/licensed-app:v1"
	if err := crane.Push(img, ref, registryOptions(h)...); err != nil {
		t.Fatalf("crane push %s: %v", ref, err)
	}
	d, _ := img.Digest()
	uri := "https://us-central1-docker.pkg.dev/" + h.Project() + "/scan-licenses/licensed-app@" + d.String()
	disc := waitDiscovery(t, ctx, g, h.Project(), uri)
	for _, typ := range []string{"GO", "MAVEN", "NPM", "PYPI"} {
		if !strings.Contains(strings.Join(disc.GetDiscovery().GetAnalysisCompleted().GetAnalysisType(), ","), typ) {
			t.Errorf("analysisCompleted %v lacks %s", disc.GetDiscovery().GetAnalysisCompleted(), typ)
		}
	}
	pkgs := allOccurrences(t, g.ListOccurrences(ctx, &gpb.ListOccurrencesRequest{Parent: "projects/" + h.Project(),
		Filter: `kind="PACKAGE" AND resourceUrl="` + uri + `"`}))
	want := map[string][2]string{
		"github.com/google/uuid":    {"GO", "BSD-3-Clause"},
		"com.google.code.gson:gson": {"MAVEN", "Apache-2.0"},
		"ms":                        {"NPM", "MIT"},
		"inherits":                  {"NPM", "ISC"},
		"six":                       {"PYPI", "MIT"},
		"packaging":                 {"PYPI", "Apache-2.0 OR BSD-2-Clause"},
	}
	seen := map[string]bool{}
	for _, o := range pkgs {
		p := o.GetPackage()
		w, ok := want[p.GetName()]
		if !ok {
			continue
		}
		seen[p.GetName()] = true
		if p.GetPackageType() != w[0] || p.GetLicense().GetExpression() != w[1] {
			t.Errorf("PACKAGE %s: type %s, license %q; want %s, %q", p.GetName(), p.GetPackageType(), p.GetLicense().GetExpression(), w[0], w[1])
		}
	}
	for n := range want {
		if !seen[n] {
			t.Errorf("no PACKAGE occurrence of %s among %d", n, len(pkgs))
		}
	}
}
