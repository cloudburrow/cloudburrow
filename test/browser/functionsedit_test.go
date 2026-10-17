//go:build browser

package browser

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	functionsv1 "cloud.google.com/go/functions/apiv1"
	"cloud.google.com/go/iam/apiv1/iampb"
	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudRunFunctionsEditDownloadAndPermissionsThroughThePage (#1491), in
// the run shard, whose Run port serves Cloud Functions. A function could be
// changed, its source downloaded and its IAM policy set only from gcloud: on
// a 1st-gen function's page, Edit function changes the description and the
// environment the official v1 client's GetFunction then reads, Download
// source answers a link that reads a .zip, and the Permissions tab's Grant
// access makes a binding GetIamPolicy reads.
func TestCloudRunFunctionsEditDownloadAndPermissionsThroughThePage(t *testing.T) {
	needService(t, "functions")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the function is made and read back through the Cloud Functions v1 API itself", envRun)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	c, err := functionsv1.NewCloudFunctionsClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-fn-%d", time.Now().UnixNano()%1e6)
	parent := "projects/" + project + "/locations/us-central1"
	name := parent + "/functions/" + id

	var src bytes.Buffer
	zw := zip.NewWriter(&src)
	for file, body := range map[string]string{
		"package.json": `{"name":"hello","version":"1.0.0","main":"index.js"}`,
		"index.js":     `exports.hello = (req, res) => res.send("hello " + (process.env.MODE || "none"));`,
	} {
		w, _ := zw.Create(file)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	up, err := c.GenerateUploadUrl(ctx, &fv1.GenerateUploadUrlRequest{Parent: parent})
	if err != nil {
		t.Skipf("GenerateUploadUrl: %v: this instance takes no function source", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, up.GetUploadUrl(), bytes.NewReader(src.Bytes()))
	req.Header.Set("Content-Type", "application/zip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("upload the source = %v, %v", resp, err)
	}
	resp.Body.Close()
	op, err := c.CreateFunction(ctx, &fv1.CreateFunctionRequest{Location: parent, Function: &fv1.CloudFunction{Name: name,
		Runtime: "nodejs22", EntryPoint: "hello", AvailableMemoryMb: 256,
		SourceCode: &fv1.CloudFunction_SourceUploadUrl{SourceUploadUrl: up.GetUploadUrl()},
		Trigger:    &fv1.CloudFunction_HttpsTrigger{HttpsTrigger: &fv1.HttpsTrigger{}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteFunction(context.Background(), &fv1.DeleteFunctionRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}

	p := open(t)
	page := "/functions/" + id + "?project=" + project
	p.navigate(page)
	p.pageAction("Edit function", map[string]string{"description": "edited in the browser", "env": "MODE=browser"}, "")
	got, err := c.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name})
	if err != nil || got.GetDescription() != "edited in the browser" || got.GetEnvironmentVariables()["MODE"] != "browser" {
		t.Errorf("GetFunction after Edit function = %v, %v; want the new description and environment", got, err)
	}

	p.navigate(page)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", "Download source")
	p.waitFor(`document.querySelector("#action-result-table a") !== null`)
	var link string
	p.eval(`document.querySelector("#action-result-table a").href`, &link)
	if code, body := do(t, http.MethodGet, link, "", ""); code != http.StatusOK || !strings.HasPrefix(body, "PK") {
		t.Errorf("Download source's link %s = %d; want the .zip", link, code)
	}
	p.clickText(".modal.is-open .modal-actions button", "Close")

	p.navigate(page)
	p.clickText("#view .tab-strip .tab", "Permissions")
	p.waitFor(`document.querySelector("#permissions-note") !== null`)
	p.clickText("#view #grant-access", "Grant access")
	p.waitFor(`document.querySelector(".modal.is-open #f-members") !== null`)
	p.eval(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		q("#f-members").value = "allUsers"; q("#f-members").dispatchEvent(new Event("input", { bubbles: true }));
		q("#f-role").value = "roles/cloudfunctions.invoker"; q("#f-role").dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	b, _ := json.Marshal(pol.GetBindings())
	if err != nil || !strings.Contains(string(b), "allUsers") || !strings.Contains(string(b), "roles/cloudfunctions.invoker") {
		t.Errorf("GetIamPolicy after Grant access = %s, %v; want allUsers as invoker", b, err)
	}
}
