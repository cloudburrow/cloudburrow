package main

import (
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
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
