package main

import (
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The volume fields build a secret volume per directory and an in-memory
// volume per path, prefill back to the same lines, and an edit that did
// not send them keeps the volumes as they were (#1297).
func TestRunVolumeFieldsRoundTrip(t *testing.T) {
	values := map[string]string{
		"serviceAccount": " web@p.iam.gserviceaccount.com ",
		"secretFiles":    `{"/secrets/token":"db:2","/secrets/user":"db","/other/key":"api"}`,
		"memoryVolumes":  `{"/scratch":"64Mi","/cache":""}`,
	}
	tmpl, err := runFormTemplate(map[string]string{"image": "ghcr.io/x/y:1", "serviceAccount": values["serviceAccount"],
		"secretFiles": values["secretFiles"], "memoryVolumes": values["memoryVolumes"]})
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.GetServiceAccount() != "web@p.iam.gserviceaccount.com" || len(tmpl.GetVolumes()) != 4 ||
		len(tmpl.GetContainers()[0].GetVolumeMounts()) != 4 {
		t.Fatalf("template = %v", tmpl)
	}
	secretFiles, memory := runVolumeDefaults(tmpl.GetVolumes(), tmpl.GetContainers()[0].GetVolumeMounts())
	if secretFiles != `{"/other/key":"api:latest","/secrets/token":"db:2","/secrets/user":"db:latest"}` ||
		memory != `{"/cache":"","/scratch":"64Mi"}` {
		t.Errorf("prefill = %s / %s", secretFiles, memory)
	}

	// A form without the fields keeps what is there; one with an empty
	// field clears that kind only.
	c := tmpl.GetContainers()[0]
	if err := applyRunVolumes(podFields{&tmpl.ServiceAccount, &tmpl.Volumes}, c, map[string]string{}); err != nil || len(tmpl.Volumes) != 4 {
		t.Errorf("an edit without the fields = %v, %d volumes", err, len(tmpl.Volumes))
	}
	if err := applyRunVolumes(podFields{&tmpl.ServiceAccount, &tmpl.Volumes}, c, map[string]string{"memoryVolumes": ""}); err != nil ||
		len(tmpl.Volumes) != 2 || len(c.VolumeMounts) != 2 || tmpl.Volumes[0].GetSecret() == nil {
		t.Errorf("clearing the in-memory volumes = %v, %v", err, tmpl.Volumes)
	}

	for _, bad := range []map[string]string{
		{"secretFiles": `{"relative/file":"s"}`},
		{"secretFiles": `{"/d/a":"s1","/d/b":"s2"}`},
		{"secretFiles": `{"/d/a":"s:"}`},
		{"memoryVolumes": `{"/":"1Mi"}`},
		{"secretFiles": `{"/d/a":"s"}`, "memoryVolumes": `{"/d":""}`},
	} {
		bad["image"] = "ghcr.io/x/y:1"
		if _, err := runFormTemplate(bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	job, err := runJobFormTemplate(map[string]string{"image": "busybox:1", "memoryVolumes": `{"/tmp/m":"8Mi"}`, "serviceAccount": "j@p"})
	if err != nil || job.GetTemplate().GetServiceAccount() != "j@p" ||
		job.GetTemplate().GetVolumes()[0].GetEmptyDir().GetMedium() != runpb.EmptyDirVolumeSource_MEMORY {
		t.Errorf("job template = %v, %v", job, err)
	}
}

// The console shows the account each workload is told it runs as (#1388):
// the template's, or the project's Compute Engine default account.
func TestConsoleShowsTheWorkloadsServiceAccount(t *testing.T) {
	if got := runAccountText("a@p.iam.gserviceaccount.com", "p"); got != "a@p.iam.gserviceaccount.com" {
		t.Errorf("set = %q", got)
	}
	if got := runAccountText("", "my-project"); !strings.HasPrefix(got, runadapter.DefaultComputeAccount("my-project")) {
		t.Errorf("unset = %q, want the default compute account", got)
	}
	wp := &runpb.WorkerPool{Name: "projects/my-project/locations/us-central1/workerPools/wp",
		Template: &runpb.WorkerPoolRevisionTemplate{ServiceAccount: "pool@my-project.iam.gserviceaccount.com"}}
	found := false
	for _, p := range workerPoolDetail(wp, console.Listing{}).Summary {
		found = found || (p.Label == "Service account" && p.Value == "pool@my-project.iam.gserviceaccount.com")
	}
	if !found {
		t.Error("the worker pool's summary does not name its service account")
	}
	job := &runpb.Job{Name: "projects/my-project/locations/us-central1/jobs/j"}
	found = false
	for _, p := range runJobDetail(job, console.Listing{}).Summary {
		found = found || (p.Label == "Service account" && strings.HasPrefix(p.Value, runadapter.DefaultComputeAccount("my-project")))
	}
	if !found {
		t.Error("the job's summary does not name the default compute account")
	}
}
