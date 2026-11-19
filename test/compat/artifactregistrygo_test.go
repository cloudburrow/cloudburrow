//go:build compat

package compat

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"mime/multipart"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
	"google.golang.org/api/iterator"
)

// mediaUpload is the API's uploadType=multipart media upload: the JSON
// metadata, then the file.
func mediaUpload(t *testing.T, h *Harness, path string, meta any, body []byte) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	pw, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	_ = json.NewEncoder(pw).Encode(meta)
	pw, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
	_, _ = pw.Write(body)
	_ = mw.Close()
	return httpDo(t, "POST", "http://"+h.Endpoint(EnvRun)+path+"?uploadType=multipart", "multipart/related; boundary="+mw.Boundary(), buf.Bytes())
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/ListPackages, google.devtools.artifactregistry.v1.ArtifactRegistry/ListVersions
//
// TestArtifactRegistryGoRepository (#1897): a GO repository, as Google
// documents it. A module zip made with golang.org/x/mod/zip, as `gcloud
// artifacts go upload` makes it, is uploaded through the API's media
// upload (goModules:create); a second upload of the same version is
// refused. The go command, with GOPROXY pointed at
// {location}-go.pkg.dev/{project}/{repository} on the Run port and nothing
// else, lists the module's versions and builds and runs a program that
// requires it. The official client lists the module as a package and its
// version.
func TestArtifactRegistryGoRepository(t *testing.T) {
	h := New(t)
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go command is not installed")
	}
	ar, ctx := arClient(t, h)
	parent := runParent(h)
	repo := formatRepo(t, ctx, ar, parent, "go-local", &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_GO})
	if repo.GetRegistryUri() != "us-central1-go.pkg.dev/"+h.Project()+"/go-local" {
		t.Errorf("registryUri = %q", repo.GetRegistryUri())
	}
	const mod = "example.com/arhello"
	src := t.TempDir()
	writeFiles(t, src, map[string]string{
		"go.mod":   "module " + mod + "\n\ngo 1.21\n",
		"hello.go": "package arhello\n\n// Hello greets.\nfunc Hello() string { return \"hello from a GO repository\" }\n",
	})
	var zipBuf bytes.Buffer
	if err := modzip.CreateFromDir(&zipBuf, module.Version{Path: mod, Version: "v0.1.0"}, src); err != nil {
		t.Fatal(err)
	}
	upload := "/upload/v1/" + repo.GetName() + "/goModules:create"
	if code, body := mediaUpload(t, h, upload, map[string]any{}, zipBuf.Bytes()); code != 200 || !strings.Contains(string(body), `"done":true`) {
		t.Fatalf("goModules:create = %d %s", code, body)
	}
	if code, _ := mediaUpload(t, h, upload, map[string]any{}, zipBuf.Bytes()); code != 409 {
		t.Errorf("a second upload of v0.1.0 = %d; want 409 ALREADY_EXISTS", code)
	}

	use := t.TempDir()
	writeFiles(t, use, map[string]string{
		"go.mod":  "module example.com/aruser\n\ngo 1.21\n\nrequire " + mod + " v0.1.0\n",
		"main.go": "package main\n\nimport (\n\t\"fmt\"\n\n\t\"" + mod + "\"\n)\n\nfunc main() { fmt.Println(arhello.Hello()) }\n",
	})
	modcache := t.TempDir()
	t.Cleanup(func() {
		// The module cache is read-only; make it removable.
		_ = filepath.WalkDir(modcache, func(p string, d fs.DirEntry, err error) error {
			if err == nil {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	env := append(os.Environ(),
		"GOPROXY=http://"+h.Endpoint(EnvRun)+"/us-central1-go.pkg.dev/"+h.Project()+"/go-local",
		"GOSUMDB=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "GOWORK=off", "GOMODCACHE="+modcache, "GOPATH="+t.TempDir())
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, goBin, args...)
		cmd.Dir, cmd.Env = use, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	if out := run("list", "-m", "-versions", mod); !strings.Contains(out, "v0.1.0") {
		t.Errorf("go list -m -versions = %q; want v0.1.0", out)
	}
	if out := run("run", "."); !strings.Contains(out, "hello from a GO repository") {
		t.Errorf("go run = %q", out)
	}

	pit := ar.ListPackages(ctx, &artifactregistrypb.ListPackagesRequest{Parent: repo.GetName()})
	p, err := pit.Next()
	if err != nil || !strings.HasSuffix(p.GetName(), "/packages/"+strings.ReplaceAll(mod, "/", "%2F")) {
		t.Fatalf("ListPackages = %v, %v", p, err)
	}
	v, err := ar.ListVersions(ctx, &artifactregistrypb.ListVersionsRequest{Parent: p.GetName()}).Next()
	if err != nil || !strings.HasSuffix(v.GetName(), "/versions/v0.1.0") {
		t.Errorf("ListVersions = %v, %v", v, err)
	}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/ListVersions, google.devtools.artifactregistry.v1.ArtifactRegistry/ListTags
//
// TestArtifactRegistryKfpRepository (#1897): a KFP repository's REST
// protocol, as Google documents it for pipeline templates (the kfp SDK's
// RegistryClient is not in the harness): a multipart POST of content (a
// pipeline_spec.yaml) and tags=v1,latest to
// {location}-kfp.pkg.dev/{project}/{repository} answers
// {packageName}/{versionName}, the package named by
// pipelineInfo.name and the version by the file's sha256; GET
// {base}/{package}/{version} and /{tag} download it. The official client
// lists the version and its tags. The API's kfpArtifacts:create media
// upload adds a second version; a package name the docs refuse is 400.
func TestArtifactRegistryKfpRepository(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	parent := runParent(h)
	repo := formatRepo(t, ctx, ar, parent, "kfp-local", &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_KFP})
	base := "http://" + h.Endpoint(EnvRun) + "/us-central1-kfp.pkg.dev/" + h.Project() + "/kfp-local"
	spec := func(name, text string) []byte {
		return []byte("pipelineInfo:\n  name: " + name + "\n  description: " + text + "\nschemaVersion: 2.1.0\nsdkVersion: kfp-2.7.0\nroot:\n  dag:\n    tasks: {}\n")
	}
	post := func(content []byte, tags string) (int, string) {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("tags", tags)
		fw, _ := mw.CreateFormFile("content", "pipeline_spec.yaml")
		_, _ = fw.Write(content)
		_ = mw.Close()
		code, body := httpDo(t, "POST", base, mw.FormDataContentType(), buf.Bytes())
		return code, string(body)
	}
	content := spec("hello-world", "first")
	sum := sha256.Sum256(content)
	version := "sha256:" + hex.EncodeToString(sum[:])
	if code, body := post(content, "v1,latest"); code != 200 || body != "hello-world/"+version {
		t.Fatalf("upload = %d %q; want hello-world/%s", code, body, version)
	}
	for _, ref := range []string{version, "v1", "latest"} {
		if got := mustHTTP(t, "GET", base+"/hello-world/"+ref, "", nil); !bytes.Equal(got, content) {
			t.Errorf("download of %s = %q", ref, got)
		}
	}
	if code, _ := post(spec("Bad_Name", "x"), ""); code != 400 {
		t.Errorf("a package name outside ^[a-z0-9][a-z0-9-]{3,127}$ = %d; want 400", code)
	}
	second := spec("hello-world", "second")
	if code, body := mediaUpload(t, h, "/upload/v1/"+repo.GetName()+"/kfpArtifacts:create",
		map[string]any{"tags": []string{"v2"}, "description": "second"}, second); code != 200 || !strings.Contains(string(body), `"done":true`) {
		t.Fatalf("kfpArtifacts:create = %d %s", code, body)
	}
	if got := mustHTTP(t, "GET", base+"/hello-world/v2", "", nil); !bytes.Equal(got, second) {
		t.Errorf("download of v2 = %q", got)
	}

	pkg := repo.GetName() + "/packages/hello-world"
	n := 0
	it := ar.ListVersions(ctx, &artifactregistrypb.ListVersionsRequest{Parent: pkg})
	for {
		v, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListVersions: %v", err)
		}
		n++
		if !strings.Contains(v.GetName(), "/versions/sha256:") {
			t.Errorf("version %s", v.GetName())
		}
	}
	if n != 2 {
		t.Errorf("ListVersions found %d versions; want 2", n)
	}
	tags := map[string]string{}
	tit := ar.ListTags(ctx, &artifactregistrypb.ListTagsRequest{Parent: pkg})
	for {
		tg, err := tit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListTags: %v", err)
		}
		tags[tg.GetName()[strings.LastIndex(tg.GetName(), "/")+1:]] = tg.GetVersion()
	}
	if !strings.HasSuffix(tags["v1"], version) || !strings.HasSuffix(tags["latest"], version) || tags["v2"] == "" {
		t.Errorf("tags = %v", tags)
	}
}
