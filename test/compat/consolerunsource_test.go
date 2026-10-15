//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// consoleRunCreateFromSource posts the Cloud Run list page's Create service
// from source through the console's action upload route, as the form does:
// the action's request, with no path, then the archive.
func consoleRunCreateFromSource(t *testing.T, addr, project string, values map[string]string, archive []byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, _ := mw.CreateFormField("request")
	_ = json.NewEncoder(w).Encode(map[string]any{"Path": []string{}, "Action": "create-from-source", "Values": values})
	part, _ := mw.CreateFormFile("file", "source.tar.gz")
	_, _ = part.Write(archive)
	_ = mw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+addr+"/api/actions/run/upload?project="+url.QueryEscape(project), &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestConsoleRunCreateServiceFromSource (#1289).
//
// The Cloud Run list screen offers Create service from source, a list-level
// action with a file field. Posted with a Dockerfile archive and a new
// service's name, it uploads the archive to run-sources-{project}-{region},
// builds it through SubmitBuild and creates the service; the official
// client reads the service back with the built image under
// cloud-run-source-deploy, and it answers through the Knative ingress. A
// second create of the same name is refused before anything is built, and a
// region the instance does not serve is refused.
func TestConsoleRunCreateServiceFromSource(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: SubmitBuild builds with the host's Docker")
	}
	addr := consoleAddr(t, h)
	rc := runClient(t, h)
	project := h.Project()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var listing struct {
		Actions []struct {
			ID     string
			Fields []struct{ Name, Type string }
		}
	}
	resp, err := http.Get("http://" + addr + "/api/resources/run?project=" + url.QueryEscape(project))
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewDecoder(resp.Body).Decode(&listing)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var fields []string
	for _, a := range listing.Actions {
		if a.ID == "create-from-source" {
			for _, f := range a.Fields {
				fields = append(fields, f.Name+":"+f.Type)
			}
		}
	}
	if got := strings.Join(fields, " "); !strings.Contains(got, "name:text") || !strings.Contains(got, "region:select") || !strings.Contains(got, "file:file") {
		t.Fatalf("the list screen's Create service from source has fields %q; want name, region and file", got)
	}

	id := fmt.Sprintf("console-src-%d", time.Now().UnixNano()%1e6)
	name := runParent(h) + "/services/" + id
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if op, err := rc.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: name}); err == nil {
			_, _ = op.Wait(dctx)
		}
	})
	archive := sourceBuildArchive(ctx, t)
	if code, body := consoleRunCreateFromSource(t, addr, project, map[string]string{
		"name": id, "region": "europe-north9", "build": "Dockerfile"}, archive); code != http.StatusBadRequest || !strings.Contains(body, "not served here") {
		t.Errorf("a region not served = %d %s; want refused", code, body)
	}
	code, body := consoleRunCreateFromSource(t, addr, project, map[string]string{
		"name": id, "region": "us-central1", "build": "Dockerfile"}, archive)
	if code != http.StatusOK {
		t.Fatalf("Create service from source = %d: %s", code, body)
	}
	svc, err := rc.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("the service the console created from source: %v", err)
	}
	prefix := "us-central1-docker.pkg.dev/" + project + "/cloud-run-source-deploy/" + id + ":"
	if got := svc.GetTemplate().GetContainers()[0].GetImage(); !strings.HasPrefix(got, prefix) {
		t.Errorf("the service reads back image %q; want one under %s", got, prefix)
	}
	if code, got := functionRequest(t, ingress(t), hostOf(t, svc.GetUri()), "", nil); code != http.StatusOK || got != "built from source by SubmitBuild" {
		t.Errorf("the service created from source answered %d %q; want the fixture's greeting", code, got)
	}

	if code, body := consoleRunCreateFromSource(t, addr, project, map[string]string{
		"name": id, "region": "us-central1", "build": "Dockerfile"}, archive); code != http.StatusBadRequest || !strings.Contains(body, "already exists") {
		t.Errorf("creating %s again = %d %s; want refused as existing", id, code, body)
	}
}
