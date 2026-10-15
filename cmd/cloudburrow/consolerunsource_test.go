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
