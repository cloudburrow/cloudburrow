package main

import (
	"encoding/json"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The deploy, edit and job forms' Cloud SQL connections field (#1346) is one
// Cloud SQL volume mounted at /cloudsql, as gcloud's --add-cloudsql-instances
// makes, and prefills from it; sending it empty removes the volume.
func TestConsoleRunCloudSQLConnections(t *testing.T) {
	_, svcForm := runProvider{}.CreateForm()
	_, jobForm := runJobsProvider{}.CreateForm()
	for _, form := range [][]console.Field{svcForm, jobForm} {
		found := false
		for _, f := range form {
			found = found || f.Name == "cloudSqlInstances"
		}
		if !found {
			t.Error("a form lacks cloudSqlInstances")
		}
	}
	var sa string
	var vols []*runpb.Volume
	c := &runpb.Container{}
	if err := applyRunVolumes(podFields{&sa, &vols}, c, map[string]string{
		"cloudSqlInstances": "p:us-central1:postgres, p:us-central1:mysql"}); err != nil {
		t.Fatal(err)
	}
	if len(vols) != 1 || len(vols[0].GetCloudSqlInstance().GetInstances()) != 2 ||
		len(c.GetVolumeMounts()) != 1 || c.GetVolumeMounts()[0].GetMountPath() != "/cloudsql" {
		t.Fatalf("volumes %v, mounts %v", vols, c.GetVolumeMounts())
	}
	if got := runCloudSQLDefault(vols); got != "p:us-central1:postgres, p:us-central1:mysql" {
		t.Errorf("prefill = %q", got)
	}
	if err := applyRunVolumes(podFields{&sa, &vols}, c, map[string]string{"cloudSqlInstances": ""}); err != nil {
		t.Fatal(err)
	}
	if len(vols) != 0 || len(c.GetVolumeMounts()) != 0 {
		t.Errorf("an empty field left %v, %v", vols, c.GetVolumeMounts())
	}
}

// A service with a Cloud SQL volume keeps its edit form: the Auth Proxy
// beside its container is not one of its containers.
func TestConsoleRunEditFormSkipsTheCloudSQLProxy(t *testing.T) {
	var tmpl knTemplate
	raw := `{"spec":{"containers":[{"image":"example.com/app:1"},{"image":"` + runadapter.CloudSQLProxyImage + `"}]}}`
	if err := json.Unmarshal([]byte(raw), &tmpl); err != nil {
		t.Fatal(err)
	}
	if form := runEditForm("web", tmpl, ""); form == nil {
		t.Fatal("no edit form beside the Cloud SQL Auth Proxy")
	}
}
