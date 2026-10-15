package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Deploy from source (#1268) is offered on a service's page only when both
// the Cloud Run adapter and Cloud Storage run here, with a file field, and a
// JSON action without the file is refused, pointing at the upload route.
func TestRunDeployFromSourceIsOfferedWithStorage(t *testing.T) {
	addr := func() string { return "127.0.0.1:1" }
	if got := (runProvider{runAddr: addr}).sourceActions(); len(got) != 0 {
		t.Errorf("without Cloud Storage: %v; want no Deploy from source", got)
	}
	if got := (runProvider{storageAddr: "http://127.0.0.1:2"}).sourceActions(); len(got) != 0 {
		t.Errorf("without the adapter: %v; want no Deploy from source", got)
	}
	got := (runProvider{runAddr: addr, storageAddr: "http://127.0.0.1:2"}).sourceActions()
	if len(got) != 1 || got[0].ID != runDeploySource || got[0].Label != "Deploy from source" {
		t.Fatalf("sourceActions = %+v; want Deploy from source", got)
	}
	var file bool
	for _, f := range got[0].Fields {
		file = file || (f.Type == console.FileFieldType && f.Required)
	}
	if !file {
		t.Error("Deploy from source has no required file field")
	}
	p := runProvider{runAddr: addr, storageAddr: "http://127.0.0.1:2"}
	if err := p.ActAt(context.Background(), "p", []string{"svc"}, runDeploySource, nil); err == nil ||
		!strings.Contains(err.Error(), "/api/actions/run/upload") {
		t.Errorf("ActAt(deploy-source) = %v; want it sent to the upload route", err)
	}
	if _, err := (runProvider{runAddr: addr}).ActAtFile(context.Background(), "p", []string{"svc"}, runDeploySource, nil,
		console.UploadedFile{Reader: strings.NewReader("")}); err == nil || !strings.Contains(err.Error(), "Cloud Storage") {
		t.Errorf("ActAtFile without storage = %v; want refused naming Cloud Storage", err)
	}
}

// Create service from source (#1289) is the Cloud Run list screen's action,
// offered only when both the adapter and Cloud Storage run here: the deploy
// form's service name, the one region served, and Deploy from source's
// fields. A name the API would refuse and a region not served are refused
// before anything is uploaded.
func TestRunCreateServiceFromSourceIsAListAction(t *testing.T) {
	addr := func() string { return "127.0.0.1:1" }
	if got := (runProvider{runAddr: addr}).ListActions(context.Background(), "p"); len(got) != 0 {
		t.Errorf("without Cloud Storage: %v; want no list actions", got)
	}
	p := runProvider{runAddr: addr, storageAddr: "http://127.0.0.1:2", region: "europe-west1"}
	got := p.ListActions(context.Background(), "p")
	if len(got) != 1 || got[0].ID != runCreateFromSource || got[0].Label != "Create service from source" {
		t.Fatalf("ListActions = %+v; want Create service from source", got)
	}
	var names []string
	for _, f := range got[0].Fields {
		names = append(names, f.Name+":"+f.Type)
		if f.Name == "region" && (len(f.Options) != 1 || f.Options[0] != "europe-west1") {
			t.Errorf("region offers %v; want only the instance's europe-west1", f.Options)
		}
		if f.Name == "name" && (!f.Required || f.Pattern == "") {
			t.Errorf("the name field %+v is not the deploy form's", f)
		}
	}
	if want := "name:text region:select file:file build:select functionTarget:"; strings.Join(names, " ") != want {
		t.Errorf("fields = %q; want %q", strings.Join(names, " "), want)
	}
	for values, why := range map[string]string{"Bad_Name": "service name", "ok|us-east1": "not served here"} {
		name, region, _ := strings.Cut(values, "|")
		_, err := p.ActAtFile(context.Background(), "p", nil, runCreateFromSource,
			map[string]string{"name": name, "region": region}, console.UploadedFile{Reader: strings.NewReader("")})
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("create-from-source %q = %v; want refused naming %q", values, err, why)
		}
	}
	if _, err := p.ActAtFile(context.Background(), "p", []string{"svc"}, runCreateFromSource, nil,
		console.UploadedFile{Reader: strings.NewReader("")}); err == nil {
		t.Error("create-from-source on a service's page was accepted")
	}
}
