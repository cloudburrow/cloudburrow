//go:build compat

package compat

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Artifact Registry's package formats (#1531): PYTHON, NPM, MAVEN,
// GENERIC, APT and YUM repositories, each written and read through its
// own protocol on the Run port at Google's documented paths, with
// {location}-{format}.pkg.dev first in the path, and read back as
// packages, versions, files and tags through the official client.

func arClient(t *testing.T, h *Harness) (*artifactregistry.Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ar.Close() })
	return ar, ctx
}

// formatRepo creates a repository, deleted when the test ends.
func formatRepo(t *testing.T, ctx context.Context, ar *artifactregistry.Client, parent, id string, r *arpb.Repository) *arpb.Repository {
	t.Helper()
	name := parent + "/repositories/" + id
	t.Cleanup(func() {
		if op, err := ar.DeleteRepository(context.Background(), &arpb.DeleteRepositoryRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	op, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id, Repository: r})
	if err != nil {
		t.Fatalf("CreateRepository %s: %v", id, err)
	}
	got, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateRepository %s .Wait: %v", id, err)
	}
	return got
}

// pkgBase is a format repository's protocol URL on the Run port.
func pkgBase(h *Harness, format, project, repo string) string {
	return "http://" + h.Endpoint(EnvRun) + "/us-central1-" + format + ".pkg.dev/" + project + "/" + repo
}

func httpDo(t *testing.T, method, url, ctype string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func mustHTTP(t *testing.T, method, url, ctype string, body []byte) []byte {
	t.Helper()
	code, b := httpDo(t, method, url, ctype, body)
	if code >= 300 {
		t.Fatalf("%s %s = %d: %s", method, url, code, b)
	}
	return b
}

// pyWheel is a minimal pure-Python wheel of package demo_pkg.
func pyWheel(t *testing.T, version string) (string, []byte) {
	t.Helper()
	dist := "demo_pkg-" + version + ".dist-info"
	files := []struct{ name, body string }{
		{"demo_pkg/__init__.py", "VALUE = " + fmt.Sprintf("%q", version) + "\n"},
		{dist + "/METADATA", "Metadata-Version: 2.1\nName: demo-pkg\nVersion: " + version + "\n"},
		{dist + "/WHEEL", "Wheel-Version: 1.0\nGenerator: cloudburrow-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"},
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var record strings.Builder
	for _, f := range files {
		w, _ := zw.Create(f.name)
		_, _ = io.WriteString(w, f.body)
		s := sha256.Sum256([]byte(f.body))
		fmt.Fprintf(&record, "%s,sha256=%s,%d\n", f.name, base64.RawURLEncoding.EncodeToString(s[:]), len(f.body))
	}
	record.WriteString(dist + "/RECORD,,\n")
	w, _ := zw.Create(dist + "/RECORD")
	_, _ = io.WriteString(w, record.String())
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return "demo_pkg-" + version + "-py3-none-any.whl", buf.Bytes()
}

// twineUpload is twine's upload: the legacy PyPI multipart form POSTed to
// the repository URL.
func twineUpload(t *testing.T, base, name, version, filename string, content []byte) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	s := sha256.Sum256(content)
	for k, v := range map[string]string{":action": "file_upload", "protocol_version": "1", "name": name, "version": version,
		"filetype": "bdist_wheel", "pyversion": "py3", "metadata_version": "2.1", "sha256_digest": hex.EncodeToString(s[:])} {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("content", filename)
	_, _ = fw.Write(content)
	_ = mw.Close()
	return httpDo(t, "POST", base+"/", mw.FormDataContentType(), buf.Bytes())
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/ListPythonPackages, google.devtools.artifactregistry.v1.ArtifactRegistry/GetPythonPackage, google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/ListPackages, google.devtools.artifactregistry.v1.ArtifactRegistry/ListVersions, google.devtools.artifactregistry.v1.ArtifactRegistry/ListFiles, google.devtools.artifactregistry.v1.ArtifactRegistry/GetFile, google.devtools.artifactregistry.v1.ArtifactRegistry/CreateTag, google.devtools.artifactregistry.v1.ArtifactRegistry/ListTags
//
// TestArtifactRegistryPythonRepository (#1531): a wheel uploaded with
// twine's upload protocol is listed by the PEP 503 simple index, and pip
// (when on PATH) installs it with --index-url; the official client reads
// it as a Python package, a package, a version, a file and a tag. A
// virtual repository over it serves the same index and refuses uploads; a
// remote repository is refused with the reason.
func TestArtifactRegistryPythonRepository(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	parent := runParent(h)
	repo := formatRepo(t, ctx, ar, parent, "py-local", &arpb.Repository{Format: arpb.Repository_PYTHON})
	if want := "us-central1-python.pkg.dev/" + h.Project() + "/py-local"; repo.GetRegistryUri() != want {
		t.Errorf("registryUri = %q; want %q", repo.GetRegistryUri(), want)
	}
	base := pkgBase(h, "python", h.Project(), "py-local")
	file, wheel := pyWheel(t, "0.1.0")
	if code, b := twineUpload(t, base, "Demo_Pkg", "0.1.0", file, wheel); code != http.StatusOK {
		t.Fatalf("twine upload = %d %s", code, b)
	}
	if code, _ := twineUpload(t, base, "Demo_Pkg", "0.1.0", file, wheel); code != http.StatusConflict {
		t.Errorf("a second upload of the same file = %d; want 409", code)
	}
	index := string(mustHTTP(t, "GET", base+"/simple/", "", nil))
	if !strings.Contains(index, "demo-pkg") {
		t.Fatalf("simple index:\n%s", index)
	}
	links := string(mustHTTP(t, "GET", base+"/simple/demo-pkg/", "", nil))
	s := sha256.Sum256(wheel)
	if !strings.Contains(links, file) || !strings.Contains(links, "#sha256="+hex.EncodeToString(s[:])) {
		t.Fatalf("project page:\n%s", links)
	}
	if pip, err := exec.LookPath("pip3"); err == nil {
		target := t.TempDir()
		cmd := exec.Command(pip, "install", "--disable-pip-version-check", "--no-cache-dir", "--no-deps", "--no-input",
			"--target", target, "--index-url", base+"/simple/", "demo-pkg==0.1.0")
		cmd.Env = append(os.Environ(), "PIP_CONFIG_FILE="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pip install --index-url: %v\n%s", err, out)
		}
		if b, err := os.ReadFile(filepath.Join(target, "demo_pkg", "__init__.py")); err != nil || !strings.Contains(string(b), "0.1.0") {
			t.Errorf("pip installed %q, %v", b, err)
		}
	} else {
		t.Log("pip3 is not on PATH: the simple index was read over HTTP only")
	}

	pp, err := ar.GetPythonPackage(ctx, &arpb.GetPythonPackageRequest{Name: repo.GetName() + "/pythonPackages/demo-pkg:0.1.0"})
	if err != nil || pp.GetPackageName() != "demo-pkg" || pp.GetVersion() != "0.1.0" || !strings.HasSuffix(pp.GetUri(), "/"+file) {
		t.Fatalf("GetPythonPackage = %v, %v", pp, err)
	}
	pps := collectAll(t, ar.ListPythonPackages(ctx, &arpb.ListPythonPackagesRequest{Parent: repo.GetName()}).Next)
	if len(pps) != 1 || pps[0].GetName() != pp.GetName() {
		t.Errorf("ListPythonPackages = %v", pps)
	}
	pkgs := collectAll(t, ar.ListPackages(ctx, &arpb.ListPackagesRequest{Parent: repo.GetName()}).Next)
	if len(pkgs) != 1 || pkgs[0].GetName() != repo.GetName()+"/packages/demo-pkg" {
		t.Fatalf("ListPackages = %v", pkgs)
	}
	vs := collectAll(t, ar.ListVersions(ctx, &arpb.ListVersionsRequest{Parent: pkgs[0].GetName()}).Next)
	if len(vs) != 1 || vs[0].GetName() != pkgs[0].GetName()+"/versions/0.1.0" {
		t.Fatalf("ListVersions = %v", vs)
	}
	files := collectAll(t, ar.ListFiles(ctx, &arpb.ListFilesRequest{Parent: repo.GetName()}).Next)
	if len(files) != 1 || files[0].GetOwner() != vs[0].GetName() || files[0].GetSizeBytes() != int64(len(wheel)) {
		t.Fatalf("ListFiles = %v", files)
	}
	if f, err := ar.GetFile(ctx, &arpb.GetFileRequest{Name: files[0].GetName()}); err != nil || !bytes.Equal(f.GetHashes()[0].GetValue(), s[:]) {
		t.Errorf("GetFile = %v, %v", f, err)
	}
	if _, err := ar.CreateTag(ctx, &arpb.CreateTagRequest{Parent: pkgs[0].GetName(), TagId: "stable", Tag: &arpb.Tag{Version: vs[0].GetName()}}); err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	if tags := collectAll(t, ar.ListTags(ctx, &arpb.ListTagsRequest{Parent: pkgs[0].GetName()}).Next); len(tags) != 1 || tags[0].GetVersion() != vs[0].GetName() {
		t.Errorf("ListTags = %v", tags)
	}

	formatRepo(t, ctx, ar, parent, "py-virtual", &arpb.Repository{Format: arpb.Repository_PYTHON, Mode: arpb.Repository_VIRTUAL_REPOSITORY,
		ModeConfig: &arpb.Repository_VirtualRepositoryConfig{VirtualRepositoryConfig: &arpb.VirtualRepositoryConfig{
			UpstreamPolicies: []*arpb.UpstreamPolicy{{Id: "local", Repository: repo.GetName(), Priority: 10}}}}})
	vbase := pkgBase(h, "python", h.Project(), "py-virtual")
	if links := string(mustHTTP(t, "GET", vbase+"/simple/demo-pkg/", "", nil)); !strings.Contains(links, file) {
		t.Errorf("the virtual repository's project page:\n%s", links)
	}
	if code, _ := twineUpload(t, vbase, "other", "1.0", "other-1.0-py3-none-any.whl", wheel); code != http.StatusMethodNotAllowed {
		t.Errorf("an upload to the virtual repository = %d; want 405", code)
	}
	_, err = ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "py-remote",
		Repository: &arpb.Repository{Format: arpb.Repository_PYTHON, Mode: arpb.Repository_REMOTE_REPOSITORY}})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "public upstreams") {
		t.Errorf("a remote repository = %v; want UNIMPLEMENTED naming the public upstreams", err)
	}
}

// npmTarball is an npm package tarball: package/package.json and
// package/index.js.
func npmTarball(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, body := range map[string]string{
		"package/package.json": fmt.Sprintf(`{"name":%q,"version":%q,"main":"index.js"}`, name, version),
		"package/index.js":     "module.exports = " + fmt.Sprintf("%q", version) + ";\n"} {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = io.WriteString(tw, body)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// npmPublishBody is the document `npm publish` PUTs.
func npmPublishBody(name, version string, tgz []byte) []byte {
	s1 := sha1.Sum(tgz)
	s512 := sha512.Sum512(tgz)
	base := name[strings.LastIndex(name, "/")+1:]
	b, _ := json.Marshal(map[string]any{"_id": name, "name": name, "dist-tags": map[string]string{"latest": version},
		"versions": map[string]any{version: map[string]any{"name": name, "version": version, "main": "index.js",
			"dist": map[string]any{"shasum": hex.EncodeToString(s1[:]), "integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:]),
				"tarball": "http://example.invalid/" + name + "/-/" + base + "-" + version + ".tgz"}}},
		"_attachments": map[string]any{base + "-" + version + ".tgz": map[string]any{"content_type": "application/octet-stream",
			"data": base64.StdEncoding.EncodeToString(tgz), "length": len(tgz)}}})
	return b
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/ListNpmPackages, google.devtools.artifactregistry.v1.ArtifactRegistry/GetNpmPackage, google.devtools.artifactregistry.v1.ArtifactRegistry/GetPackage, google.devtools.artifactregistry.v1.ArtifactRegistry/GetVersion, google.devtools.artifactregistry.v1.ArtifactRegistry/GetTag, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteVersion
//
// TestArtifactRegistryNpmRepository (#1531): `npm publish` (when npm is on
// PATH, else its PUT of the package document) and a scoped package's PUT
// publish to an NPM repository; `npm install` installs the package back;
// the packument names both versions and the latest dist-tag; the official
// client reads them as npm packages with their tags, packages, versions
// and tags, and DeleteVersion removes one.
func TestArtifactRegistryNpmRepository(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	repo := formatRepo(t, ctx, ar, runParent(h), "npm-local", &arpb.Repository{Format: arpb.Repository_NPM})
	base := pkgBase(h, "npm", h.Project(), "npm-local")
	npm, npmErr := exec.LookPath("npm")
	if npmErr == nil {
		dir, cache := t.TempDir(), t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"cb-demo","version":"1.0.0","main":"index.js"}`), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "index.js"), []byte("module.exports = \"1.0.0\";\n"), 0o644)
		rc := filepath.Join(t.TempDir(), ".npmrc")
		hostPath := strings.TrimPrefix(base, "http:")
		_ = os.WriteFile(rc, []byte(hostPath+"/:_authToken=local-test\n"), 0o600)
		run := func(dir string, args ...string) {
			t.Helper()
			cmd := exec.Command(npm, append(args, "--registry", base+"/", "--userconfig", rc, "--cache", cache, "--no-audit", "--no-fund")...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "NPM_CONFIG_UPDATE_NOTIFIER=false")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("npm %s: %v\n%s", strings.Join(args, " "), err, out)
			}
		}
		run(dir, "publish")
		app := t.TempDir()
		_ = os.WriteFile(filepath.Join(app, "package.json"), []byte(`{"name":"app","version":"0.0.0","private":true}`), 0o644)
		run(app, "install", "cb-demo@1.0.0")
		if b, err := os.ReadFile(filepath.Join(app, "node_modules", "cb-demo", "index.js")); err != nil || !strings.Contains(string(b), "1.0.0") {
			t.Errorf("npm install gave %q, %v", b, err)
		}
	} else {
		t.Log("npm is not on PATH: publish and install were made over HTTP only")
		tgz := npmTarball(t, "cb-demo", "1.0.0")
		mustHTTP(t, "PUT", base+"/cb-demo", "application/json", npmPublishBody("cb-demo", "1.0.0", tgz))
	}
	tgz := npmTarball(t, "@cb/scoped", "2.0.0")
	mustHTTP(t, "PUT", base+"/@cb%2fscoped", "application/json", npmPublishBody("@cb/scoped", "2.0.0", tgz))
	if code, _ := httpDo(t, "PUT", base+"/@cb%2fscoped", "application/json", npmPublishBody("@cb/scoped", "2.0.0", tgz)); code != http.StatusForbidden {
		t.Errorf("publishing over 2.0.0 = %d; want 403", code)
	}
	var doc struct {
		Versions map[string]struct {
			Dist struct{ Tarball string } `json:"dist"`
		} `json:"versions"`
		DistTags map[string]string `json:"dist-tags"`
	}
	if err := json.Unmarshal(mustHTTP(t, "GET", base+"/@cb%2fscoped", "", nil), &doc); err != nil || doc.DistTags["latest"] != "2.0.0" {
		t.Fatalf("packument = %+v, %v", doc, err)
	}
	if got := mustHTTP(t, "GET", doc.Versions["2.0.0"].Dist.Tarball, "", nil); !bytes.Equal(got, tgz) {
		t.Errorf("the tarball at %s is not the one published", doc.Versions["2.0.0"].Dist.Tarball)
	}
	mustHTTP(t, "PUT", base+"/-/package/@cb%2fscoped/dist-tags/beta", "application/json", []byte(`"2.0.0"`))

	np, err := ar.GetNpmPackage(ctx, &arpb.GetNpmPackageRequest{Name: repo.GetName() + "/npmPackages/@cb%2Fscoped:2.0.0"})
	if err != nil || np.GetPackageName() != "@cb/scoped" || strings.Join(np.GetTags(), ",") != "beta,latest" {
		t.Fatalf("GetNpmPackage = %v, %v", np, err)
	}
	nps := collectAll(t, ar.ListNpmPackages(ctx, &arpb.ListNpmPackagesRequest{Parent: repo.GetName()}).Next)
	if len(nps) != 2 {
		t.Errorf("ListNpmPackages = %v", nps)
	}
	pkg := repo.GetName() + "/packages/@cb%2Fscoped"
	if p, err := ar.GetPackage(ctx, &arpb.GetPackageRequest{Name: pkg}); err != nil || p.GetDisplayName() != "@cb/scoped" {
		t.Errorf("GetPackage = %v, %v", p, err)
	}
	v, err := ar.GetVersion(ctx, &arpb.GetVersionRequest{Name: pkg + "/versions/2.0.0", View: arpb.VersionView_FULL})
	if err != nil || len(v.GetRelatedTags()) != 2 {
		t.Fatalf("GetVersion = %v, %v", v, err)
	}
	if tag, err := ar.GetTag(ctx, &arpb.GetTagRequest{Name: pkg + "/tags/latest"}); err != nil || tag.GetVersion() != v.GetName() {
		t.Errorf("GetTag = %v, %v", tag, err)
	}
	if _, err := ar.DeleteVersion(ctx, &arpb.DeleteVersionRequest{Name: v.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteVersion of a tagged version without force = %v; want FAILED_PRECONDITION", err)
	}
	op, err := ar.DeleteVersion(ctx, &arpb.DeleteVersionRequest{Name: v.GetName(), Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _ := httpDo(t, "GET", base+"/@cb%2fscoped", "", nil); code != http.StatusNotFound {
		t.Errorf("the packument after DeleteVersion = %d; want 404", code)
	}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/ListMavenArtifacts, google.devtools.artifactregistry.v1.ArtifactRegistry/GetMavenArtifact, google.devtools.artifactregistry.v1.ArtifactRegistry/DeletePackage
//
// TestArtifactRegistryMavenRepository (#1531): the PUTs `mvn deploy`
// makes, a jar, its pom and their checksums, land in a MAVEN repository's
// layout; dependency resolution's GETs read them back, with
// maven-metadata.xml and checksums computed for what was not uploaded; a
// release is not overwritten; a virtual repository resolves through its
// upstream; the official client reads the artifact, and DeletePackage
// removes it.
func TestArtifactRegistryMavenRepository(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	parent := runParent(h)
	repo := formatRepo(t, ctx, ar, parent, "mvn-local", &arpb.Repository{Format: arpb.Repository_MAVEN})
	base := pkgBase(h, "maven", h.Project(), "mvn-local")
	dir := "/com/example/demo-lib/1.2.0/"
	jar, pom := []byte("PK-not-really-a-jar"), []byte("<project><groupId>com.example</groupId><artifactId>demo-lib</artifactId><version>1.2.0</version></project>")
	for name, body := range map[string][]byte{"demo-lib-1.2.0.jar": jar, "demo-lib-1.2.0.pom": pom} {
		mustHTTP(t, "PUT", base+dir+name, "", body)
		s := sha1.Sum(body)
		mustHTTP(t, "PUT", base+dir+name+".sha1", "", []byte(hex.EncodeToString(s[:])))
	}
	if got := mustHTTP(t, "GET", base+dir+"demo-lib-1.2.0.jar", "", nil); !bytes.Equal(got, jar) {
		t.Errorf("GET jar = %q", got)
	}
	s := sha256.Sum256(jar)
	if got := string(mustHTTP(t, "GET", base+dir+"demo-lib-1.2.0.jar.sha256", "", nil)); got != hex.EncodeToString(s[:]) {
		t.Errorf("the computed sha256 = %q", got)
	}
	meta := string(mustHTTP(t, "GET", base+"/com/example/demo-lib/maven-metadata.xml", "", nil))
	if !strings.Contains(meta, "<version>1.2.0</version>") || !strings.Contains(meta, "<release>1.2.0</release>") {
		t.Errorf("maven-metadata.xml:\n%s", meta)
	}
	if code, _ := httpDo(t, "PUT", base+dir+"demo-lib-1.2.0.jar", "", []byte("other")); code != http.StatusConflict {
		t.Errorf("overwriting a release = %d; want 409", code)
	}
	formatRepo(t, ctx, ar, parent, "mvn-virtual", &arpb.Repository{Format: arpb.Repository_MAVEN, Mode: arpb.Repository_VIRTUAL_REPOSITORY,
		ModeConfig: &arpb.Repository_VirtualRepositoryConfig{VirtualRepositoryConfig: &arpb.VirtualRepositoryConfig{
			UpstreamPolicies: []*arpb.UpstreamPolicy{{Id: "local", Repository: repo.GetName(), Priority: 1}}}}})
	if got := mustHTTP(t, "GET", pkgBase(h, "maven", h.Project(), "mvn-virtual")+dir+"demo-lib-1.2.0.pom", "", nil); !bytes.Equal(got, pom) {
		t.Errorf("the virtual repository's pom = %q", got)
	}

	name := repo.GetName() + "/mavenArtifacts/com.example:demo-lib:1.2.0"
	m, err := ar.GetMavenArtifact(ctx, &arpb.GetMavenArtifactRequest{Name: name})
	if err != nil || m.GetGroupId() != "com.example" || m.GetArtifactId() != "demo-lib" || m.GetVersion() != "1.2.0" ||
		m.GetPomUri() != repo.GetRegistryUri()+dir+"demo-lib-1.2.0.pom" {
		t.Fatalf("GetMavenArtifact = %v, %v", m, err)
	}
	if ms := collectAll(t, ar.ListMavenArtifacts(ctx, &arpb.ListMavenArtifactsRequest{Parent: repo.GetName()}).Next); len(ms) != 1 || ms[0].GetName() != name {
		t.Errorf("ListMavenArtifacts = %v", ms)
	}
	files := collectAll(t, ar.ListFiles(ctx, &arpb.ListFilesRequest{Parent: repo.GetName(),
		Filter: `owner="` + repo.GetName() + `/packages/com.example:demo-lib/versions/1.2.0"`}).Next)
	if len(files) != 2 {
		t.Errorf("the version's files = %v", files)
	}
	op, err := ar.DeletePackage(ctx, &arpb.DeletePackageRequest{Name: repo.GetName() + "/packages/com.example:demo-lib"})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ar.GetMavenArtifact(ctx, &arpb.GetMavenArtifactRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetMavenArtifact after DeletePackage = %v; want NOT_FOUND", err)
	}
}

// genericUpload is the API's media upload of a generic artifact,
// uploadType=multipart: the JSON metadata, then the file.
func genericUpload(t *testing.T, h *Harness, repoName, pkg, version, filename string, body []byte) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	pw, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	_ = json.NewEncoder(pw).Encode(map[string]string{"packageId": pkg, "versionId": version, "filename": filename})
	pw, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
	_, _ = pw.Write(body)
	_ = mw.Close()
	return httpDo(t, "POST", "http://"+h.Endpoint(EnvRun)+"/upload/v1/"+repoName+"/genericArtifacts:create?uploadType=multipart",
		"multipart/related; boundary="+mw.Boundary(), buf.Bytes())
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteFile, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateTag, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteTag
//
// TestArtifactRegistryGenericRepository (#1531): a file uploaded with the
// API's media upload (genericArtifacts:create) downloads back through
// files:download?alt=media; a second upload of the same name is refused;
// the official client reads it as a package, version and file, moves and
// deletes a tag, and deletes the file, as only a generic repository
// allows.
func TestArtifactRegistryGenericRepository(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	repo := formatRepo(t, ctx, ar, runParent(h), "generic-local", &arpb.Repository{Format: arpb.Repository_GENERIC})
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if code, b := genericUpload(t, h, repo.GetName(), "tool", v, "tool.tar.gz", []byte("tool "+v)); code != http.StatusOK {
			t.Fatalf("upload %s = %d %s", v, code, b)
		}
	}
	if code, _ := genericUpload(t, h, repo.GetName(), "tool", "1.0.0", "tool.tar.gz", []byte("again")); code != http.StatusConflict {
		t.Errorf("a second upload = %d; want 409", code)
	}
	fileName := repo.GetName() + "/files/tool:1.0.0:tool.tar.gz"
	if got := mustHTTP(t, "GET", "http://"+h.Endpoint(EnvRun)+"/v1/"+fileName+":download?alt=media", "", nil); string(got) != "tool 1.0.0" {
		t.Errorf("download = %q", got)
	}
	f, err := ar.GetFile(ctx, &arpb.GetFileRequest{Name: fileName})
	if err != nil || f.GetOwner() != repo.GetName()+"/packages/tool/versions/1.0.0" {
		t.Fatalf("GetFile = %v, %v", f, err)
	}
	pkg := repo.GetName() + "/packages/tool"
	if _, err := ar.CreateTag(ctx, &arpb.CreateTagRequest{Parent: pkg, TagId: "current", Tag: &arpb.Tag{Version: pkg + "/versions/1.0.0"}}); err != nil {
		t.Fatal(err)
	}
	if tag, err := ar.UpdateTag(ctx, &arpb.UpdateTagRequest{Tag: &arpb.Tag{Name: pkg + "/tags/current", Version: pkg + "/versions/1.1.0"}}); err != nil || tag.GetVersion() != pkg+"/versions/1.1.0" {
		t.Fatalf("UpdateTag = %v, %v", tag, err)
	}
	if err := ar.DeleteTag(ctx, &arpb.DeleteTagRequest{Name: pkg + "/tags/current"}); err != nil {
		t.Fatal(err)
	}
	op, err := ar.DeleteFile(ctx, &arpb.DeleteFileRequest{Name: fileName})
	if err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if vs := collectAll(t, ar.ListVersions(ctx, &arpb.ListVersionsRequest{Parent: pkg}).Next); len(vs) != 1 || !strings.HasSuffix(vs[0].GetName(), "/1.1.0") {
		t.Errorf("versions after DeleteFile = %v", vs)
	}
}

// debPackage is a minimal .deb: an ar archive of debian-binary,
// control.tar.gz and an empty data.tar.gz.
func debPackage(name, version, arch string) []byte {
	tgz := func(files map[string]string) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for n, body := range files {
			_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			_, _ = io.WriteString(tw, body)
		}
		_ = tw.Close()
		_ = gz.Close()
		return buf.Bytes()
	}
	control := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: test <t@example.com>\nDescription: a test package\n", name, version, arch)
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	member := func(n string, b []byte) {
		fmt.Fprintf(&out, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", n, 0, 0, 0, "100644", len(b))
		out.Write(b)
		if len(b)%2 == 1 {
			out.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar.gz", tgz(map[string]string{"./control": control}))
	member("data.tar.gz", tgz(nil))
	return out.Bytes()
}

// rpmPackage is a minimal .rpm: the lead, an empty signature header, and
// a header with its name, version, release, architecture and source RPM.
func rpmPackage(name, version, release, arch string) []byte {
	header := func(tags map[uint32]string) []byte {
		var index, store bytes.Buffer
		for _, tag := range []uint32{1000, 1001, 1002, 1022, 1044} {
			v, ok := tags[tag]
			if !ok {
				continue
			}
			_ = binary.Write(&index, binary.BigEndian, []uint32{tag, 6, uint32(store.Len()), 1})
			store.WriteString(v)
			store.WriteByte(0)
		}
		var out bytes.Buffer
		out.Write([]byte{0x8e, 0xad, 0xe8, 0x01, 0, 0, 0, 0})
		_ = binary.Write(&out, binary.BigEndian, []uint32{uint32(index.Len() / 16), uint32(store.Len())})
		out.Write(index.Bytes())
		out.Write(store.Bytes())
		return out.Bytes()
	}
	var out bytes.Buffer
	lead := make([]byte, 96)
	copy(lead, []byte{0xed, 0xab, 0xee, 0xdb, 3, 0})
	copy(lead[10:], name)
	binary.BigEndian.PutUint16(lead[78:], 5)
	out.Write(lead)
	out.Write(header(nil)) // 16 bytes: already 8-aligned
	out.Write(header(map[uint32]string{1000: name, 1001: version, 1002: release, 1022: arch, 1044: name + "-" + version + "-" + release + ".src.rpm"}))
	return out.Bytes()
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/ImportAptArtifacts, google.devtools.artifactregistry.v1.ArtifactRegistry/ImportYumArtifacts
//
// TestArtifactRegistryAptAndYumImports (#1531): a .deb and an .rpm written
// to this instance's Cloud Storage import into an APT and a YUM
// repository; the operations' responses name each package with its
// architecture (and a .deb's control file); an object that is not a
// package is reported in the response's errors; the APT repository's
// archive serves its Release, Packages index and pool file; the official
// client then lists each as a package and version with its file.
func TestArtifactRegistryAptAndYumImports(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	gcs := instanceStorage(t, h)
	parent := runParent(h)
	bucket := gcs.Bucket("ar-import-" + strings.TrimPrefix(h.Project(), "cb-test-"))
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	put := func(object string, b []byte) {
		w := bucket.Object(object).NewWriter(ctx)
		_, _ = w.Write(b)
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", object, err)
		}
		t.Cleanup(func() { _ = bucket.Object(object).Delete(context.Background()) })
	}
	t.Cleanup(func() { _ = bucket.Delete(context.Background()) })
	put("hello_1.0-1_amd64.deb", debPackage("hello", "1.0-1", "amd64"))
	put("hello-2.1-3.x86_64.rpm", rpmPackage("hello", "2.1", "3", "x86_64"))
	put("not-a-package.deb", []byte("plain text"))
	gs := "gs://" + bucket.BucketName() + "/"

	apt := formatRepo(t, ctx, ar, parent, "apt-local", &arpb.Repository{Format: arpb.Repository_APT})
	if want := "us-central1-apt.pkg.dev/projects/" + h.Project(); apt.GetRegistryUri() != want {
		t.Errorf("the APT registryUri = %q; want %q", apt.GetRegistryUri(), want)
	}
	aop, err := ar.ImportAptArtifacts(ctx, &arpb.ImportAptArtifactsRequest{Parent: apt.GetName(),
		Source: &arpb.ImportAptArtifactsRequest_GcsSource{GcsSource: &arpb.ImportAptArtifactsGcsSource{
			Uris: []string{gs + "hello_1.0-1_amd64.deb", gs + "not-a-package.deb"}}}})
	if err != nil {
		t.Fatalf("ImportAptArtifacts: %v", err)
	}
	ar1, err := aop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ar1.GetAptArtifacts()) != 1 || ar1.GetAptArtifacts()[0].GetPackageName() != "hello" ||
		ar1.GetAptArtifacts()[0].GetArchitecture() != "amd64" || !strings.Contains(string(ar1.GetAptArtifacts()[0].GetControlFile()), "Version: 1.0-1") {
		t.Errorf("ImportAptArtifacts response = %v", ar1)
	}
	if len(ar1.GetErrors()) != 1 {
		t.Errorf("ImportAptArtifacts errors = %v; want the text object's", ar1.GetErrors())
	}
	if vs := collectAll(t, ar.ListVersions(ctx, &arpb.ListVersionsRequest{Parent: apt.GetName() + "/packages/hello"}).Next); len(vs) != 1 ||
		!strings.HasSuffix(vs[0].GetName(), "/versions/1.0-1") {
		t.Errorf("APT versions = %v", vs)
	}

	// The archive apt reads with `deb [trusted=yes] URL apt-local main`
	// (apt is not on this machine: Go HTTP to its documented paths).
	archive := "http://" + h.Endpoint(EnvRun) + "/us-central1-apt.pkg.dev/projects/" + h.Project()
	packages := string(mustHTTP(t, "GET", archive+"/dists/apt-local/main/binary-amd64/Packages", "", nil))
	release := string(mustHTTP(t, "GET", archive+"/dists/apt-local/Release", "", nil))
	ps := sha256.Sum256([]byte(packages))
	if !strings.Contains(packages, "Package: hello\n") || !strings.Contains(release, "Architectures: amd64") ||
		!strings.Contains(release, hex.EncodeToString(ps[:])+" ") {
		t.Fatalf("Release:\n%s\nPackages:\n%s", release, packages)
	}
	filename := ""
	for _, line := range strings.Split(packages, "\n") {
		if v, ok := strings.CutPrefix(line, "Filename: "); ok {
			filename = v
		}
	}
	if got := mustHTTP(t, "GET", archive+"/"+filename, "", nil); !bytes.Equal(got, debPackage("hello", "1.0-1", "amd64")) {
		t.Errorf("the pool file %s is not the imported .deb", filename)
	}

	yum := formatRepo(t, ctx, ar, parent, "yum-local", &arpb.Repository{Format: arpb.Repository_YUM})
	yop, err := ar.ImportYumArtifacts(ctx, &arpb.ImportYumArtifactsRequest{Parent: yum.GetName(),
		Source: &arpb.ImportYumArtifactsRequest_GcsSource{GcsSource: &arpb.ImportYumArtifactsGcsSource{Uris: []string{gs + "hello-2.1-3.x86_64.rpm"}}}})
	if err != nil {
		t.Fatalf("ImportYumArtifacts: %v", err)
	}
	yr, err := yop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(yr.GetYumArtifacts()) != 1 || yr.GetYumArtifacts()[0].GetPackageName() != "hello" || yr.GetYumArtifacts()[0].GetArchitecture() != "x86_64" ||
		yr.GetYumArtifacts()[0].GetPackageType() != arpb.YumArtifact_BINARY {
		t.Errorf("ImportYumArtifacts response = %v", yr)
	}
	files := collectAll(t, ar.ListFiles(ctx, &arpb.ListFilesRequest{Parent: yum.GetName()}).Next)
	if len(files) != 1 || files[0].GetName() != yum.GetName()+"/files/hello-2.1-3.x86_64.rpm" || files[0].GetOwner() != yum.GetName()+"/packages/hello/versions/2.1-3" {
		t.Errorf("YUM files = %v", files)
	}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/ListPackages, google.devtools.artifactregistry.v1.ArtifactRegistry/ListVersions, google.devtools.artifactregistry.v1.ArtifactRegistry/ListFiles
//
// TestGcloudArtifactsPackageFormats (#1531): `gcloud artifacts
// repositories create --repository-format=python`, then `gcloud artifacts
// packages list`, `versions list` and `files list` over a wheel twine's
// protocol uploaded; and `gcloud artifacts generic upload` and `generic
// download` through a GENERIC repository, through the configuration
// `cloudburrow gcloud-setup` writes, egress blocked.
func TestGcloudArtifactsPackageFormats(t *testing.T) {
	h := New(t)
	ar, _ := arClient(t, h)
	g := newGcloudSession(t, h)
	repoName := runParent(h) + "/repositories/gcloud-python"
	t.Cleanup(func() {
		if op, err := ar.DeleteRepository(context.Background(), &arpb.DeleteRepositoryRequest{Name: repoName}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	g.must("artifacts", "repositories", "create", "gcloud-python", "--repository-format=python", "--location=us-central1",
		"--project="+h.Project(), "--quiet")
	file, wheel := pyWheel(t, "0.2.0")
	if code, b := twineUpload(t, pkgBase(h, "python", h.Project(), "gcloud-python"), "demo-pkg", "0.2.0", file, wheel); code != http.StatusOK {
		t.Fatalf("twine upload = %d %s", code, b)
	}
	loc := []string{"--repository=gcloud-python", "--location=us-central1", "--project=" + h.Project()}
	if out := g.must(append([]string{"artifacts", "packages", "list", "--format=value(name)"}, loc...)...); !strings.Contains(out, "demo-pkg") {
		t.Errorf("gcloud artifacts packages list:\n%s", out)
	}
	if out := g.must(append([]string{"artifacts", "versions", "list", "--package=demo-pkg", "--format=value(name)"}, loc...)...); !strings.Contains(out, "0.2.0") {
		t.Errorf("gcloud artifacts versions list:\n%s", out)
	}
	if out := g.must(append([]string{"artifacts", "files", "list", "--package=demo-pkg", "--version=0.2.0", "--format=value(name)"}, loc...)...); !strings.Contains(out, file) {
		t.Errorf("gcloud artifacts files list:\n%s", out)
	}

	genName := runParent(h) + "/repositories/gcloud-generic"
	t.Cleanup(func() {
		if op, err := ar.DeleteRepository(context.Background(), &arpb.DeleteRepositoryRequest{Name: genName}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	g.must("artifacts", "repositories", "create", "gcloud-generic", "--repository-format=generic", "--location=us-central1",
		"--project="+h.Project(), "--quiet")
	src := filepath.Join(t.TempDir(), "notes.txt")
	_ = os.WriteFile(src, []byte("generic from gcloud"), 0o644)
	gloc := []string{"--repository=gcloud-generic", "--location=us-central1", "--project=" + h.Project()}
	g.must(append([]string{"artifacts", "generic", "upload", "--source=" + src, "--package=notes", "--version=1.0.0"}, gloc...)...)
	dst := t.TempDir()
	g.must(append([]string{"artifacts", "generic", "download", "--destination=" + dst, "--package=notes", "--version=1.0.0", "--name=notes.txt"}, gloc...)...)
	if b, err := os.ReadFile(filepath.Join(dst, "notes.txt")); err != nil || string(b) != "generic from gcloud" {
		t.Errorf("gcloud artifacts generic download gave %q, %v", b, err)
	}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/GetRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteRepository
//
// TestTerraformArtifactRegistryRepositoryFormats (#1531): a
// google_artifact_registry_repository of each format this instance serves,
// PYTHON, NPM, MAVEN (with maven_config), GENERIC, APT and YUM, and a
// virtual PYTHON repository over the standard one, apply through
// `cloudburrow terraform` with egress blocked; the official client reads
// each back with its format, mode and registry URL; the plan is clean; and
// destroy removes them.
func TestTerraformArtifactRegistryRepositoryFormats(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ar, ctx := arClient(t, h)
	suffix := strings.TrimPrefix(h.Project(), "cb-test-")
	formats := map[string]arpb.Repository_Format{"PYTHON": arpb.Repository_PYTHON, "NPM": arpb.Repository_NPM,
		"MAVEN": arpb.Repository_MAVEN, "GENERIC": arpb.Repository_GENERIC, "APT": arpb.Repository_APT, "YUM": arpb.Repository_YUM}
	var body strings.Builder
	for f := range formats {
		extra := ""
		if f == "MAVEN" {
			extra = "\n  maven_config {\n    version_policy = \"RELEASE\"\n  }"
		}
		fmt.Fprintf(&body, `
resource "google_artifact_registry_repository" %q {
  project       = %q
  location      = "us-central1"
  repository_id = "tf-%s-%s"
  format        = %q%s
}
`, strings.ToLower(f), h.Project(), strings.ToLower(f), suffix, f, extra)
	}
	fmt.Fprintf(&body, `
resource "google_artifact_registry_repository" "virtual" {
  project       = %q
  location      = "us-central1"
  repository_id = "tf-pyvirtual-%s"
  format        = "PYTHON"
  mode          = "VIRTUAL_REPOSITORY"
  virtual_repository_config {
    upstream_policies {
      id         = "local"
      repository = google_artifact_registry_repository.python.id
      priority   = 1
    }
  }
}
`, h.Project(), suffix)
	m.write(body.String())
	m.must("init", "-input=false", "-no-color")
	m.apply()
	parent := runParent(h)
	for f, want := range formats {
		id := "tf-" + strings.ToLower(f) + "-" + suffix
		r, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: parent + "/repositories/" + id})
		if err != nil {
			t.Fatalf("GetRepository %s: %v", id, err)
		}
		url := "us-central1-" + strings.ToLower(f) + ".pkg.dev/" + h.Project() + "/" + id
		if f == "APT" || f == "YUM" {
			url = "us-central1-" + strings.ToLower(f) + ".pkg.dev/projects/" + h.Project()
		}
		if r.GetFormat() != want || r.GetMode() != arpb.Repository_STANDARD_REPOSITORY || r.GetRegistryUri() != url {
			t.Errorf("%s = %v", id, r)
		}
		if f == "MAVEN" && r.GetMavenConfig().GetVersionPolicy() != arpb.Repository_MavenRepositoryConfig_RELEASE {
			t.Errorf("the MAVEN repository's maven_config = %v", r.GetMavenConfig())
		}
	}
	v, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: parent + "/repositories/tf-pyvirtual-" + suffix})
	if err != nil || v.GetMode() != arpb.Repository_VIRTUAL_REPOSITORY ||
		v.GetVirtualRepositoryConfig().GetUpstreamPolicies()[0].GetRepository() != parent+"/repositories/tf-python-"+suffix {
		t.Errorf("the virtual repository = %v, %v", v, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: parent + "/repositories/tf-python-" + suffix}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRepository after destroy: %v; want NotFound", err)
	}
}
