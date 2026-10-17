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

// The NFS volumes field (#1448) builds an NFS volume per mount path, a
// trailing :ro making it read-only, and prefills back to the same lines;
// a line that is not server:/export is refused.
func TestRunNFSVolumeFieldRoundTrip(t *testing.T) {
	lines := `{"/mnt/ro":"10.0.0.2:/exports:ro","/mnt/share":"nfs.example:/"}`
	tmpl, err := runFormTemplate(map[string]string{"image": "ghcr.io/x/y:1", "nfsVolumes": lines})
	if err != nil {
		t.Fatal(err)
	}
	v := tmpl.GetVolumes()
	if len(v) != 2 || v[0].GetNfs().GetServer() != "10.0.0.2" || v[0].GetNfs().GetPath() != "/exports" ||
		!v[0].GetNfs().GetReadOnly() || v[1].GetNfs().GetServer() != "nfs.example" || v[1].GetNfs().GetReadOnly() {
		t.Fatalf("volumes = %v", v)
	}
	if got := runNFSDefault(v, tmpl.GetContainers()[0].GetVolumeMounts()); got != lines {
		t.Errorf("prefill = %s; want %s", got, lines)
	}
	for _, bad := range []string{`{"/mnt":"server"}`, `{"/mnt":":/x"}`, `{"/mnt":"s:relative"}`, `{"/":"s:/"}`} {
		if _, err := runFormTemplate(map[string]string{"image": "ghcr.io/x/y:1", "nfsVolumes": bad}); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	job, err := runJobFormTemplate(map[string]string{"image": "busybox:1", "nfsVolumes": `{"/data":"s:/x"}`})
	if err != nil || len(job.GetTemplate().GetVolumes()) != 1 {
		t.Errorf("job form NFS volume = %v, %v", job, err)
	}
}

// The Cloud Storage volumes field (#1474) is /mount/path=bucket[:ro][;option...].
func TestRunGCSVolumeFieldRoundTrip(t *testing.T) {
	lines := `{"/mnt/data":"data","/mnt/logs":"logs:ro;only-dir=app;implicit-dirs"}`
	tmpl, err := runFormTemplate(map[string]string{"image": "ghcr.io/x/y:1", "gcsVolumes": lines})
	if err != nil {
		t.Fatal(err)
	}
	v := tmpl.GetVolumes()
	if len(v) != 2 || v[0].GetGcs().GetBucket() != "data" || v[0].GetGcs().GetReadOnly() ||
		v[1].GetGcs().GetBucket() != "logs" || !v[1].GetGcs().GetReadOnly() || len(v[1].GetGcs().GetMountOptions()) != 2 {
		t.Fatalf("volumes = %v", v)
	}
	if got := runGCSDefault(v, tmpl.GetContainers()[0].GetVolumeMounts()); got != lines {
		t.Errorf("prefill = %s; want %s", got, lines)
	}
	for _, bad := range []string{`{"/mnt":""}`, `{"/mnt":"gs://b"}`, `{"/":"b"}`, `{"rel":"b"}`} {
		if _, err := runFormTemplate(map[string]string{"image": "ghcr.io/x/y:1", "gcsVolumes": bad}); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	job, err := runJobFormTemplate(map[string]string{"image": "busybox:1", "gcsVolumes": `{"/data":"b"}`})
	if err != nil || len(job.GetTemplate().GetVolumes()) != 1 {
		t.Errorf("job form Cloud Storage volume = %v, %v", job, err)
	}
}

// Use HTTP/2 end-to-end (#1475) names the container port h2c.
func TestRunHTTP2FieldNamesThePortH2C(t *testing.T) {
	for port, want := range map[string]int32{"50051": 50051, "": 8080} {
		tmpl, err := runFormTemplate(map[string]string{"image": "ghcr.io/x/y:1", "port": port, "useHttp2": "true"})
		if err != nil {
			t.Fatal(err)
		}
		if p := tmpl.GetContainers()[0].GetPorts(); len(p) != 1 || p[0].GetName() != "h2c" || p[0].GetContainerPort() != want {
			t.Errorf("port %q: ports = %v; want h2c %d", port, p, want)
		}
	}
	tmpl, err := runFormTemplate(map[string]string{"image": "ghcr.io/x/y:1", "port": "8080"})
	if err != nil || tmpl.GetContainers()[0].GetPorts()[0].GetName() != "" {
		t.Errorf("without the option the port is named: %v, %v", tmpl, err)
	}
}

// TestRunFormsOfferEveryVolumeField: the Deploy container and Create job
// forms carry each field runVolumeFields defines; the Cloud Storage volumes
// field (#1474) was defined, read and prefilled but left off both forms, so
// the browser never showed it.
func TestRunFormsOfferEveryVolumeField(t *testing.T) {
	for name, form := range map[string]func() (string, []console.Field){
		"Deploy container": runProvider{}.CreateForm, "Create job": runJobsProvider{}.CreateForm,
	} {
		_, fields := form()
		have := map[string]bool{}
		for _, f := range fields {
			have[f.Name] = true
		}
		for _, f := range runVolumeFields("Container") {
			if !have[f.Name] {
				t.Errorf("%s has no %s field", name, f.Name)
			}
		}
	}
}
