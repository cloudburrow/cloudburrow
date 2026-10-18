//go:build browser

package browser

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// registryDo sends one request to the Run port's registry, as twine, npm,
// mvn or the API's media upload does.
func registryDo(t *testing.T, addr, method, path, ctype string, body []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://"+addr+path, bytes.NewReader(body))
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

// arFiles are the names of a repository's files, as ListFiles reads them.
func arFiles(t *testing.T, c *artifactregistry.Client, repo string) []string {
	t.Helper()
	var out []string
	it := c.ListFiles(context.Background(), &arpb.ListFilesRequest{Parent: repo})
	for {
		f, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f.GetName()[strings.LastIndex(f.GetName(), "/")+1:])
	}
}

// TestCloudRunArtifactRegistryPackageFormatsThroughThePage (#1592), in the
// run shard, whose Run port serves Artifact Registry and its formats'
// registries:
//   - Create repository with the PYTHON format makes a repository the
//     official client's GetRepository reads as PYTHON; a wheel uploaded as
//     twine does is on its page's Packages, the package's Versions and the
//     version's Files, and ListPythonPackages reads it;
//   - in a GENERIC repository, Delete file on a version's page
//     deletes the file ListFiles then no longer lists, and, the version and
//     package gone with it, the page goes up to the repository's.
func TestCloudRunArtifactRegistryPackageFormatsThroughThePage(t *testing.T) {
	needService(t, "artifact-registry")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the repository is read back through the Artifact Registry API itself", envRun)
	}
	ctx := context.Background()
	c, err := artifactregistry.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	n := time.Now().UnixNano() % 1e6
	py, gen := fmt.Sprintf("browser-py-%d", n), fmt.Sprintf("browser-gen-%d", n)
	parent := "projects/" + project + "/locations/us-central1"
	t.Cleanup(func() {
		for _, id := range []string{py, gen} {
			consoleDo(t, http.MethodDelete, "/api/resources/artifact-registry?project="+project+"&name="+id, "")
		}
	})

	p := open(t)
	p.navigate("/artifact-registry?project=" + project)
	p.clickText("#view button", "Create repository")
	p.waitFor(`document.querySelector("#view #f-id, .modal.is-open #f-id") !== null`)
	scope := "#view"
	var inModal bool
	p.eval(`document.querySelector(".modal.is-open #f-id") !== null`, &inModal)
	if inModal {
		scope = ".modal.is-open"
	}
	setField(p, scope, "f-id", py)
	setField(p, scope, "f-location", "us-central1")
	setField(p, scope, "f-format", "PYTHON")
	p.run(chromedp.Click(scope+` button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, py))
	if r, err := c.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: parent + "/repositories/" + py}); err != nil || r.GetFormat() != arpb.Repository_PYTHON {
		t.Fatalf("GetRepository after Create repository = %v, %v; want a PYTHON repository", r, err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField(":action", "file_upload")
	_ = mw.WriteField("name", "Demo_Pkg")
	_ = mw.WriteField("version", "0.1.0")
	fw, _ := mw.CreateFormFile("content", "demo_pkg-0.1.0-py3-none-any.whl")
	_, _ = fw.Write([]byte("wheel"))
	_ = mw.Close()
	registryDo(t, addr, http.MethodPost, "/us-central1-python.pkg.dev/"+project+"/"+py+"/", mw.FormDataContentType(), buf.Bytes())
	it := c.ListPythonPackages(ctx, &arpb.ListPythonPackagesRequest{Parent: parent + "/repositories/" + py})
	if pkg, err := it.Next(); err != nil || pkg.GetPackageName() != "demo-pkg" || pkg.GetVersion() != "0.1.0" {
		t.Fatalf("ListPythonPackages after the upload = %v, %v", pkg, err)
	}

	p.navigate("/artifact-registry/" + py + "?project=" + project)
	p.clickText("#view tbody a", "demo-pkg")
	p.waitFor(`location.pathname.includes("demo-pkg")`)
	p.clickText("#view tbody a", "0.1.0")
	p.waitFor(`location.pathname.includes("0.1.0")`)
	p.waitFor(`document.querySelector("#view").textContent.includes("demo_pkg-0.1.0-py3-none-any.whl")`)

	if code, body := consoleDo(t, http.MethodPost, "/api/resources/artifact-registry?project="+project,
		`{"id":"`+gen+`","format":"GENERIC","location":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create repository %s through the console API = %d: %s", gen, code, body)
	}
	buf.Reset()
	mw = multipart.NewWriter(&buf)
	pw, _ := mw.CreatePart(map[string][]string{"Content-Type": {"application/json"}})
	_, _ = pw.Write([]byte(`{"packageId":"tool","versionId":"2.0","filename":"tool.bin"}`))
	pw, _ = mw.CreatePart(map[string][]string{"Content-Type": {"application/octet-stream"}})
	_, _ = pw.Write([]byte("binary"))
	_ = mw.Close()
	registryDo(t, addr, http.MethodPost, "/upload/v1/"+parent+"/repositories/"+gen+"/genericArtifacts:create?uploadType=multipart",
		"multipart/related; boundary="+mw.Boundary(), buf.Bytes())
	if files := arFiles(t, c, parent+"/repositories/"+gen); len(files) != 1 {
		t.Fatalf("ListFiles after the generic upload = %v; want one file", files)
	}

	p.navigate("/artifact-registry/" + gen + "?project=" + project)
	p.clickText("#view tbody a", "tool")
	p.waitFor(`location.pathname.includes("tool")`)
	p.clickText("#view tbody a", "2.0")
	p.waitFor(`location.pathname.includes("2.0")`)
	p.waitFor(`document.querySelector("#view").textContent.includes("tool.bin")`)
	p.pageAction("Delete file", map[string]string{"file": "tool:2.0:tool.bin"}, "")
	if files := arFiles(t, c, parent+"/repositories/"+gen); len(files) != 0 {
		t.Errorf("ListFiles after Delete file = %v; want none", files)
	}
	// The version and the package went with their only file, so the page
	// gone to is the repository's, not the package's NotFound.
	p.waitFor(fmt.Sprintf(`location.pathname === %q`, "/artifact-registry/"+gen))
	p.waitFor(`document.querySelector("#view .page-actions") !== null`)
}
