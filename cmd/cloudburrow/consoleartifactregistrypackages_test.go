package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// TestConsoleArtifactRegistryPackageFormats (#1531): a repository of each
// package format has a page listing what its protocol uploaded, a
// package page with its versions, and a version page with its files.
func TestConsoleArtifactRegistryPackageFormats(t *testing.T) {
	ctx := context.Background()
	st, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runadapter.UseRegistry(st)
	t.Cleanup(func() { runadapter.UseRegistry(nil) })
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	parent := "projects/console-pkg/locations/us-central1"
	for id, f := range map[string]arpb.Repository_Format{"py": arpb.Repository_PYTHON, "js": arpb.Repository_NPM,
		"mvn": arpb.Repository_MAVEN, "gen": arpb.Repository_GENERIC, "deb": arpb.Repository_APT} {
		if _, err := s.ArtifactRegistry().CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id,
			Repository: &arpb.Repository{Format: f}}); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(runadapter.RegistryHandlerOr(http.NotFoundHandler()))
	t.Cleanup(srv.Close)
	do := func(method, path, ctype string, body []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s = %d %s", method, path, resp.StatusCode, b)
		}
	}
	// Python: twine's upload.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField(":action", "file_upload")
	_ = mw.WriteField("name", "Demo_Pkg")
	_ = mw.WriteField("version", "0.1.0")
	fw, _ := mw.CreateFormFile("content", "demo_pkg-0.1.0-py3-none-any.whl")
	_, _ = fw.Write([]byte("wheel"))
	_ = mw.Close()
	do("POST", "/us-central1-python.pkg.dev/console-pkg/py/", mw.FormDataContentType(), buf.Bytes())
	// npm: npm publish's document.
	tgz := base64.StdEncoding.EncodeToString([]byte("tarball"))
	do("PUT", "/us-central1-npm.pkg.dev/console-pkg/js/demo", "application/json", []byte(fmt.Sprintf(
		`{"name":"demo","versions":{"1.0.0":{"name":"demo","version":"1.0.0"}},"dist-tags":{"latest":"1.0.0"},"_attachments":{"demo-1.0.0.tgz":{"data":%q}}}`, tgz)))
	// Maven: mvn deploy's PUT.
	do("PUT", "/us-central1-maven.pkg.dev/console-pkg/mvn/com/example/lib/1.0/lib-1.0.jar", "", []byte("jar"))
	// Generic: the API's media upload.
	buf.Reset()
	mw = multipart.NewWriter(&buf)
	h := map[string][]string{"Content-Type": {"application/json"}}
	pw, _ := mw.CreatePart(h)
	_, _ = pw.Write([]byte(`{"packageId":"tool","versionId":"2.0","filename":"tool.bin"}`))
	pw, _ = mw.CreatePart(map[string][]string{"Content-Type": {"application/octet-stream"}})
	_, _ = pw.Write([]byte("binary"))
	_ = mw.Close()
	do("POST", "/upload/v1/"+parent+"/repositories/gen/genericArtifacts:create?uploadType=multipart",
		"multipart/related; boundary="+mw.Boundary(), buf.Bytes())

	p := artifactRegistryProvider{defaultProject: "console-pkg"}
	for repo, want := range map[string][3]string{"py": {"demo-pkg", "0.1.0", "demo_pkg-0.1.0-py3-none-any.whl"},
		"js": {"demo", "1.0.0", "demo/-/demo-1.0.0.tgz"}, "mvn": {"com.example:lib", "1.0", "com/example/lib/1.0/lib-1.0.jar"},
		"gen": {"tool", "2.0", "tool:2.0:tool.bin"}} {
		d, err := p.Detail(ctx, "", []string{repo})
		if err != nil || len(d.Sections[0].Listing.Items) != 1 || d.Sections[0].Listing.Items[0].Name != want[0] {
			t.Fatalf("%s repository page = %+v, %v", repo, d, err)
		}
		// MAVEN, NPM and PYTHON pages also list ListMavenArtifacts',
		// ListNpmPackages' or ListPythonPackages' view.
		if wantFormat := repo != "gen"; wantFormat != (len(d.Sections) == 2) || (wantFormat && len(d.Sections[1].Listing.Items) != 1) {
			t.Errorf("%s repository page sections = %+v", repo, d.Sections)
		}
		d, err = p.Detail(ctx, "", []string{repo, want[0]})
		if err != nil || len(d.Sections[0].Listing.Items) != 1 || d.Sections[0].Listing.Items[0].Name != want[1] {
			t.Fatalf("%s package page = %+v, %v", repo, d, err)
		}
		d, err = p.Detail(ctx, "", []string{repo, want[0], want[1]})
		if err != nil || len(d.Sections[0].Listing.Items) != 1 || d.Sections[0].Listing.Items[0].Name != want[2] {
			t.Fatalf("%s version page = %+v, %v", repo, d, err)
		}
	}
	if d, _ := p.Detail(ctx, "", []string{"js", "demo"}); !strings.Contains(fmt.Sprint(d.Sections[0].Listing.Items), "latest") {
		t.Errorf("npm's dist-tag is not on the package page: %+v", d.Sections[0].Listing.Items)
	}
	if d, _ := p.Detail(ctx, "", []string{"mvn", "com.example:lib", "1.0"}); !strings.Contains(fmt.Sprint(d.Summary), "POM") {
		t.Errorf("the Maven version page has no POM: %v", d.Summary)
	}
	offers := func(path []string, id string) bool {
		for _, a := range p.DetailActions(ctx, "", path) {
			if a.ID == id {
				return true
			}
		}
		return false
	}
	if !offers([]string{"deb"}, "import") || offers([]string{"py"}, "import") {
		t.Error("Import from Cloud Storage is offered on other than an APT or YUM repository's page")
	}
	path := []string{"gen", "tool", "2.0"}
	if !offers(path, "delete-file") || offers([]string{"py", "demo-pkg", "0.1.0"}, "delete-file") {
		t.Error("Delete file is offered on other than a GENERIC version's page")
	}
	if err := p.ActAt(ctx, "", []string{"py", "demo-pkg", "0.1.0"}, "delete-file", map[string]string{"file": "x"}); err == nil {
		t.Error("Delete file in a PYTHON repository was accepted")
	}
	if err := p.ActAt(ctx, "", path, "add-tag", map[string]string{"tag": "stable"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "", path, "delete-file", map[string]string{"file": "tool:2.0:tool.bin"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Detail(ctx, "", []string{"gen"}); len(d.Sections[0].Listing.Items) != 0 {
		t.Errorf("after Delete version, the generic repository lists %+v", d.Sections[0].Listing.Items)
	}
}
