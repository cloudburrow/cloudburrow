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
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudBuildDependenciesAndPoolMachineTypesThroughThePages (#2074,
// #2073), in the run shard, whose Run port serves Cloud Build and Secure
// Source Manager, on an instance with --cloudbuild-host-steps:
//   - a build with a git dependency on a Secure Source Manager repository
//     runs, its step reading the file from destPath, and its page's Build
//     dependencies tab lists the repository URL, revision, destination path
//     and depth;
//   - Create private pool's Series and Machine type follow the Region:
//     us-central1 offers E2, N2D and C3, europe-north2 E2 only, and C3 in
//     europe-west2 has no c3-highcpu machine types; a pool created there
//     is read back by GetWorkerPool with the chosen machine type;
//   - each page passes the WCAG 2.1 AA audit.
func TestCloudBuildDependenciesAndPoolMachineTypesThroughThePages(t *testing.T) {
	needService(t, "cloud-build-worker-pools")
	ssm := ssmClient(t)
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	c, err := cloudbuild.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	q := "?project=" + project
	loc := "projects/" + project + "/locations/us-central1"
	suffix := time.Now().UnixNano() % 1e6

	// A repository with a pushed file, which a build depends on.
	iop, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: loc, InstanceId: fmt.Sprintf("deps-%d", suffix)})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	inst, err := iop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	rop, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: loc, RepositoryId: "lib",
		Repository: &ssmpb.Repository{Instance: inst.GetName()}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ssm.DeleteRepository(context.Background(), &ssmpb.DeleteRepositoryRequest{Name: repo.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
		if op, err := ssm.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: inst.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	gitURL := repo.GetUris().GetGitHttps()
	ssmPush(t, gitURL, "main", map[string]string{"VERSION": "lib-2074\n"})

	op, err := c.CreateBuild(ctx, &cbpb.CreateBuildRequest{Parent: loc, ProjectId: project, Build: &cbpb.Build{
		Steps: []*cbpb.BuildStep{{Name: "alpine:3.20", Entrypoint: "sh", Args: []string{"-c", `test "$(cat /workspace/deps/lib/VERSION)" = lib-2074`}}},
		Dependencies: []*cbpb.Dependency{{Dep: &cbpb.Dependency_GitSource{GitSource: &cbpb.Dependency_GitSourceDependency{
			Repository: &cbpb.Dependency_GitSourceRepository{Repotype: &cbpb.Dependency_GitSourceRepository_Url{Url: gitURL}},
			Revision:   "main", DestPath: "deps/lib", Depth: 1}}}}}})
	if err != nil {
		t.Skipf("CreateBuild with a git dependency: %v (the run shard starts its instance with --cloudbuild-host-steps)", err)
	}
	b, err := op.Wait(ctx)
	if err != nil || b.GetStatus() != cbpb.Build_SUCCESS {
		t.Fatalf("the build with a git dependency = %v, %v; want SUCCESS", b, err)
	}

	p := open(t)
	buildPage := "/cloud-build/builds/" + b.GetId() + q + "&location=us-central1"
	p.navigate(buildPage + "&tab=dependencies")
	p.waitFor(`document.querySelector("#tab-dependencies") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-dependencies`)))
	row := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, gitURL)
	p.waitFor(row + ` !== undefined`)
	var cells string
	p.eval(row+`.textContent`, &cells)
	for _, want := range []string{"main", "deps/lib", "1"} {
		if !strings.Contains(cells, want) {
			t.Errorf("Build dependencies' row is %q; want %q in it", cells, want)
		}
	}

	// Create private pool: Series and Machine type follow the Region.
	p.navigate("/cloud-build/worker-pools/create" + q)
	p.waitFor(`document.querySelector("#f-series") !== null`)
	series := func(region string) string {
		p.eval(fmt.Sprintf(`(() => { const r = document.querySelector("#f-region"); r.value = %q;
			r.dispatchEvent(new Event("change")); return true })()`, region), nil)
		var s string
		p.eval(`[...document.querySelectorAll("#f-series option")].map((o) => o.value).join(",")`, &s)
		return s
	}
	if s := series("us-central1"); s != "e2,n2d,c3" {
		t.Errorf("Series in us-central1 = %q", s)
	}
	if s := series("europe-north2"); s != "e2" {
		t.Errorf("Series in europe-north2 = %q", s)
	}
	series("europe-west2")
	p.eval(`(() => { const s = document.querySelector("#f-series"); s.value = "c3"; s.dispatchEvent(new Event("change")); return true })()`, nil)
	var c3 string
	p.eval(`[...document.querySelectorAll("#f-machineType-c3 option")].map((o) => o.value).join(",")`, &c3)
	if strings.Contains(c3, "c3-highcpu") || !strings.Contains(c3, "c3-highmem-4") {
		t.Errorf("C3 in europe-west2 offers %q; it has c3-standard and c3-highmem only", c3)
	}
	pool := fmt.Sprintf("deps-pool-%d", suffix)
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/cloud-build-worker-pools"+q+"&name="+pool, "")
	})
	p.submitForm(map[string]string{"name": pool, "region": "europe-west2", "series": "c3", "machineType-c3": "c3-highmem-8"}, "name")
	var w *cbpb.WorkerPool
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if w, err = c.GetWorkerPool(ctx, &cbpb.GetWorkerPoolRequest{Name: "projects/" + project + "/locations/europe-west2/workerPools/" + pool}); err == nil {
			break
		}
	}
	if w.GetPrivatePoolV1Config().GetWorkerConfig().GetMachineType() != "c3-highmem-8" {
		t.Errorf("GetWorkerPool after Create private pool = %v, %v", w, err)
	}

	auditScreens(t, []a11yPage{
		{"Build dependencies", buildPage + "&tab=dependencies", settled, ""},
		{"Create private pool", "/cloud-build/worker-pools/create" + q, settled, ""},
		{"Edit private pool", "/cloud-build/worker-pools/" + pool + q + "&edit=1", settled, ""},
	})
}
