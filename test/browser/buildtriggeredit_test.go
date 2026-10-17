//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudRunBuildTriggerEditAndDisableThroughThePage, in the run shard,
// whose Run port serves Cloud Build. A trigger had no page, so
// UpdateBuildTrigger had no console surface: from the Triggers list, a
// trigger's page opens, Edit trigger changes the description and the step
// the official client's GetBuildTrigger then reads, and Disable makes it
// disabled.
func TestCloudRunBuildTriggerEditAndDisableThroughThePage(t *testing.T) {
	needService(t, "cloud-build-triggers")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the trigger is read back through the Cloud Build API itself", envRun)
	}
	ctx := context.Background()
	c, err := cloudbuild.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	name := fmt.Sprintf("browser-trig-%d", time.Now().UnixNano()%1e6)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/cloud-build-triggers?project="+project,
		`{"name":"`+name+`","region":"global","image":"alpine:3.20","command":"echo one","description":"first"}`); code != http.StatusOK {
		t.Fatalf("create trigger %s through the console API = %d: %s", name, code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/cloud-build-triggers?project="+project+"&name="+name, "")
	})
	get := func() *cbpb.BuildTrigger {
		t.Helper()
		got, err := c.GetBuildTrigger(ctx, &cbpb.GetBuildTriggerRequest{ProjectId: project, TriggerId: name,
			Name: "projects/" + project + "/locations/global/triggers/" + name})
		if err != nil {
			t.Fatalf("GetBuildTrigger %s: %v", name, err)
		}
		return got
	}

	p := open(t)
	p.navigate("/cloud-build/triggers?project=" + project)
	p.clickText("#view tbody a", name)
	p.waitFor(fmt.Sprintf(`location.pathname === %q`, "/cloud-build/triggers/"+name))
	p.pageAction("Edit trigger", map[string]string{"description": "edited in the browser", "image": "busybox:1.36", "command": "echo two"}, "")
	got := get()
	if s := got.GetBuild().GetSteps(); got.GetDescription() != "edited in the browser" || len(s) != 1 || s[0].GetName() != "busybox:1.36" ||
		len(s[0].GetArgs()) != 2 || s[0].GetArgs()[1] != "echo two" {
		t.Errorf("GetBuildTrigger after Edit trigger = %v; want the new description and step", got)
	}
	p.clickText("#view .page-actions button", "Disable")
	p.confirmByName(name)
	p.waitFor(`document.querySelector(".modal") === null`)
	for deadline := time.Now().Add(10 * time.Second); !get().GetDisabled() && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
	}
	if !get().GetDisabled() {
		t.Error("GetBuildTrigger after Disable: the trigger is not disabled")
	}
}

// TestCloudRunBuildWorkerPoolEditThroughThePage (#1491), in the run shard.
// A pool had no page and was changed only with gcloud: from the Worker
// pools list, a pool's page opens, and Edit worker pool changes the display
// name, machine type and disk the official client's GetWorkerPool then
// reads. The Triggers page names the default build service account.
func TestCloudRunBuildWorkerPoolEditThroughThePage(t *testing.T) {
	needService(t, "cloud-build-worker-pools")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the pool is read back through the Cloud Build API itself", envRun)
	}
	ctx := context.Background()
	c, err := cloudbuild.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	name := fmt.Sprintf("browser-pool-%d", time.Now().UnixNano()%1e6)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/cloud-build-worker-pools?project="+project,
		`{"name":"`+name+`","region":"us-central1","machineType":"e2-standard-2"}`); code != http.StatusOK {
		t.Fatalf("create pool %s through the console API = %d: %s", name, code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/cloud-build-worker-pools?project="+project+"&name="+name, "")
	})

	p := open(t)
	p.navigate("/cloud-build/worker-pools?project=" + project)
	p.clickText("#view tbody a", name)
	p.waitFor(fmt.Sprintf(`location.pathname === %q`, "/cloud-build/worker-pools/"+name))
	p.pageAction("Edit worker pool", map[string]string{"displayName": "Browser builders", "machineType": "e2-highcpu-8", "diskSizeGb": "200"}, "")
	w, err := c.GetWorkerPool(ctx, &cbpb.GetWorkerPoolRequest{Name: "projects/" + project + "/locations/us-central1/workerPools/" + name})
	if wc := w.GetPrivatePoolV1Config().GetWorkerConfig(); err != nil || w.GetDisplayName() != "Browser builders" ||
		wc.GetMachineType() != "e2-highcpu-8" || wc.GetDiskSizeGb() != 200 {
		t.Errorf("GetWorkerPool after Edit worker pool = %v, %v; want the form's values", w, err)
	}

	p.navigate("/cloud-build/triggers?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Default build service account")`)
}
