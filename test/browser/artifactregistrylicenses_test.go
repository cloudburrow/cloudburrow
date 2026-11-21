//go:build browser

package browser

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// licensedPackagesImage is an image of npm, PyPI, Maven and Go packages
// of known licenses: package.json files, METADATA files, a jar's POM, and
// a Go program requiring github.com/google/uuid (built from the module
// cache only) with the module's LICENSE in a module cache.
func licensedPackagesImage(t *testing.T) v1.Image {
	t.Helper()
	dir := t.TempDir()
	for name, s := range map[string]string{
		"go.mod":  "module example.com/licdemo\n\ngo 1.22\n\nrequire github.com/google/uuid v1.6.0\n",
		"go.sum":  "github.com/google/uuid v1.6.0 h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0=\ngithub.com/google/uuid v1.6.0/go.mod h1:TIyPZe4MgqvfeYDBFedMoGGpEw/LqOeaOT+nhxU+yHo=\n",
		"main.go": "package main\n\nimport (\n\t\"fmt\"\n\n\t\"github.com/google/uuid\"\n)\n\nfunc main() { fmt.Println(uuid.NewString()) }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "build", "-o", "licdemo", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOPROXY=off", "GOFLAGS=-mod=mod", "GOWORK=off", "GOTOOLCHAIN=local")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, b)
	}
	bin, err := os.ReadFile(filepath.Join(dir, "licdemo"))
	if err != nil {
		t.Fatal(err)
	}
	mc, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	uuidLicense, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(mc)), "github.com", "google", "uuid@v1.6.0", "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	var jar bytes.Buffer
	z := zip.NewWriter(&jar)
	w, _ := z.Create("META-INF/maven/com.google.code.gson/gson/pom.xml")
	_, _ = w.Write([]byte(`<project><groupId>com.google.code.gson</groupId><artifactId>gson</artifactId><version>2.10.1</version>
<licenses><license><name>Apache-2.0</name><url>https://www.apache.org/licenses/LICENSE-2.0.txt</url></license></licenses></project>`))
	w, _ = z.Create("META-INF/maven/com.google.code.gson/gson/pom.properties")
	_, _ = w.Write([]byte("groupId=com.google.code.gson\nartifactId=gson\nversion=2.10.1\n"))
	_ = z.Close()
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	for _, f := range []struct {
		name string
		mode int64
		data []byte
	}{
		{"app/licdemo", 0o755, bin},
		{"root/go/pkg/mod/github.com/google/uuid@v1.6.0/LICENSE", 0o444, uuidLicense},
		{"app/node_modules/ms/package.json", 0o644, []byte(`{"name":"ms","version":"2.1.3","license":"MIT"}`)},
		{"app/node_modules/inherits/package.json", 0o644, []byte(`{"name":"inherits","version":"2.0.4","license":"ISC"}`)},
		{"app/node_modules/.package-lock.json", 0o644, []byte(`{"lockfileVersion":3,"packages":{"node_modules/ms":{"version":"2.1.3"},"node_modules/inherits":{"version":"2.0.4"}}}`)},
		{"usr/local/lib/python3.11/site-packages/six-1.16.0.dist-info/METADATA", 0o644, []byte("Metadata-Version: 2.1\nName: six\nVersion: 1.16.0\nLicense: MIT\n\n")},
		{"app/lib/gson-2.10.1.jar", 0o644, jar.Bytes()},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(f.data)
	}
	_ = tw.Close()
	b := layer.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.Append(empty.Image, mutate.Addendum{Layer: l, History: v1.History{CreatedBy: "browser test packages"}})
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", "amd64"
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	return img
}

// TestCloudRunArtifactRegistryLanguageLicensesThroughThePage (#2072), in
// the run shard: an image of npm, PyPI, Maven and Go packages is pushed
// with scanning on, and its version's Dependencies tab shows each
// package's License type as an SPDX ID and Most common licenses counts
// them (MIT 2, then Apache-2.0, BSD-3-Clause and ISC 1 each), as Google's
// Dependencies tab does for language packages. The page passes the WCAG
// 2.1 AA audit. It skips when the scanner is not prefetched, or fails then
// with CLOUDBURROW_TEST_OSV_REQUIRED set.
func TestCloudRunArtifactRegistryLanguageLicensesThroughThePage(t *testing.T) {
	c := arClient(t)
	su := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_RUN_SERVICEUSAGE"))
	if su == "" {
		su = strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_SERVICEUSAGE"))
	}
	if su == "" {
		t.Skip("no Service Usage endpoint: the Container Scanning API is enabled through it")
	}
	ctx := context.Background()
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-lic-%d", time.Now().UnixNano()%1e6)
	parent := "projects/" + project + "/locations/us-central1"
	hc := &http.Client{Timeout: 30 * time.Second}
	t.Cleanup(hc.CloseIdleConnections)
	api := "http://" + strings.TrimPrefix(su, "http://") + "/v1/projects/" + project + "/services/containerscanning.googleapis.com"
	resp, err := hc.Post(api+":enable", "application/json", strings.NewReader("{}"))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("enable the Container Scanning API: %v %v", resp, err)
	}
	resp.Body.Close()
	t.Cleanup(func() {
		if r, err := hc.Post(api+":disable", "application/json", strings.NewReader("{}")); err == nil {
			r.Body.Close()
		}
	})
	op, err := c.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id, Repository: &arpb.Repository{Format: arpb.Repository_DOCKER}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteRepository(context.Background(), &arpb.DeleteRepositoryRequest{Name: parent + "/repositories/" + id}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	img := licensedPackagesImage(t)
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	_, port, _ := net.SplitHostPort(addr)
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	if err := crane.Push(img, "us-central1-docker.pkg.localhost:"+port+"/"+project+"/"+id+"/licensed-app:v1", crane.Insecure, crane.WithTransport(tr)); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()

	p := open(t)
	path := "/artifact-registry/" + id + "/licensed-app/" + d.String() + "?project=" + project + "&tab=dependencies"
	deadline := time.Now().Add(2 * time.Minute)
	var text string
	for {
		p.navigate(path)
		p.waitFor(`document.querySelector("#view [role=tabpanel]") !== null`)
		p.eval(`document.querySelector("#view").textContent`, &text)
		if strings.Contains(text, "github.com/google/uuid") {
			break
		}
		if strings.Contains(text, "not prefetched") {
			if os.Getenv("CLOUDBURROW_TEST_OSV_REQUIRED") != "" {
				t.Fatalf("the instance's scanner is not prefetched, and CLOUDBURROW_TEST_OSV_REQUIRED is set:\n%s", text)
			}
			t.Skip("the instance's scanner is not prefetched (cloudburrow prefetch --vulnerability-scanning)")
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Dependencies tab never listed the image's packages:\n%s", text)
		}
		time.Sleep(2 * time.Second)
	}
	var rows []string
	p.eval(`[...document.querySelectorAll("#view [role=tabpanel] tbody tr")].map((r) => [...r.cells].map((c) => c.textContent.trim()).join("|"))`, &rows)
	for name, want := range map[string]string{
		"ms": "NPM|MIT", "inherits": "NPM|ISC", "six": "PYPI|MIT",
		"com.google.code.gson:gson": "MAVEN|Apache-2.0", "github.com/google/uuid": "GO|BSD-3-Clause",
	} {
		found := false
		for _, r := range rows {
			if strings.HasPrefix(r, name+"|") {
				found = true
				if !strings.HasSuffix(r, "|"+want) {
					t.Errorf("%s's row %q; want it to end %q (Package type, License type)", name, r, want)
				}
			}
		}
		if !found {
			t.Errorf("no row for %s among %q", name, rows)
		}
	}
	var bars string
	p.eval(`[...document.querySelectorAll("#view [role=tabpanel] .bar-graph .bar-row")].map((r) => r.textContent).join("|")`, &bars)
	if bars != "MIT2|Apache-2.01|BSD-3-Clause1|ISC1" {
		t.Errorf("Most common licenses bars %q; want MIT 2, then Apache-2.0, BSD-3-Clause and ISC 1 each", bars)
	}
	auditScreens(t, []a11yPage{
		{"image digest dependencies with language package licenses", path, settled, ""},
	})
}
