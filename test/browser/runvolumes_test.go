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

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const envRun = "CLOUDBURROW_TEST_RUN"

// TestCloudRunJobNFSAndStorageVolumesThroughTheBrowser (#1448, #1474), in the
// run shard: the Create job form's NFS volumes and Cloud Storage volumes
// fields, one line per mount, create a job the official JobsClient reads
// back with an NFS volume (read-only, as :ro asks) and a Cloud Storage volume
// with its gcsfuse mount option, each mounted where the line says; Edit job
// opens with both fields prefilled with the same lines. Creating a job runs
// nothing, so no NFS server or bucket has to answer.
func TestCloudRunJobNFSAndStorageVolumesThroughTheBrowser(t *testing.T) {
	needService(t, "run-jobs")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the job is read back through the Cloud Run API itself", envRun)
	}
	ctx := context.Background()
	c, err := run.NewJobsClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := open(t)
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-volumes-%d", time.Now().UnixNano()%1e6)
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/run-jobs?project="+project+"&name="+id, "")
	})
	const nfs = "/mnt/share=10.0.0.2:/export:ro"
	const gcs = "/mnt/data=browser-volumes-bucket;implicit-dirs"

	p.navigate("/run/jobs/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-nfsVolumes") !== null && document.querySelector("#view #f-gcsVolumes") !== null`)
	p.run(chromedp.SendKeys(`#view #f-name`, id, chromedp.ByQuery),
		chromedp.SendKeys(`#view #f-image`, "docker.io/library/busybox:1.36", chromedp.ByQuery))
	setField(p, "#view", "f-nfsVolumes", nfs)
	setField(p, "#view", "f-gcsVolumes", gcs)
	p.clickText(`#view button[type="submit"]`, "Create job")
	p.waitFor(fmt.Sprintf(`location.pathname === "/run/jobs" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, id))
	if posts := p.posts("/api/resources/run-jobs"); len(posts) != 1 {
		t.Errorf("the create was posted %d times, want once: %v", len(posts), posts)
	}

	job, err := c.GetJob(ctx, &runpb.GetJobRequest{Name: "projects/" + project + "/locations/us-central1/jobs/" + id})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	tt := job.GetTemplate().GetTemplate()
	mounts := map[string]string{}
	for _, m := range tt.GetContainers()[0].GetVolumeMounts() {
		mounts[m.GetName()] = m.GetMountPath()
	}
	var sawNFS, sawGCS bool
	for _, v := range tt.GetVolumes() {
		if n := v.GetNfs(); n != nil {
			sawNFS = n.GetServer() == "10.0.0.2" && n.GetPath() == "/export" && n.GetReadOnly() && mounts[v.GetName()] == "/mnt/share"
		}
		if g := v.GetGcs(); g != nil {
			sawGCS = g.GetBucket() == "browser-volumes-bucket" && !g.GetReadOnly() &&
				strings.Join(g.GetMountOptions(), ",") == "implicit-dirs" && mounts[v.GetName()] == "/mnt/data"
		}
	}
	if !sawNFS || !sawGCS || len(tt.GetVolumes()) != 2 {
		t.Errorf("GetJob reads volumes %v and mounts %v; want the read-only NFS export at /mnt/share and the bucket at /mnt/data",
			tt.GetVolumes(), mounts)
	}

	// Edit job opens with both fields prefilled with the lines given.
	p.clickText("#view tbody a", id)
	p.clickText("#view .page-actions button", "Edit job")
	p.waitFor(`document.querySelector(".modal.is-open #f-nfsVolumes") !== null`)
	var form struct{ NFS, GCS string }
	p.eval(`({ NFS: document.querySelector(".modal.is-open #f-nfsVolumes").value,
		GCS: document.querySelector(".modal.is-open #f-gcsVolumes").value })`, &form)
	if strings.TrimSpace(form.NFS) != nfs || strings.TrimSpace(form.GCS) != gcs {
		t.Errorf("Edit job is prefilled with NFS %q and Cloud Storage %q; want %q and %q", form.NFS, form.GCS, nfs, gcs)
	}
	p.clickText(".modal.is-open .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
}
