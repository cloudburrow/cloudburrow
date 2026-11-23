package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"

	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
)

// TestStudioCodeAssets (#2094): Studio's saved queries, notebooks and data
// canvases are Dataform repositories labelled single-file-asset-type, in
// folders, each save a version, shared through the repository's policy,
// and hidden from the Dataform page; Source control lists the others.
// The Folders and Settings pages read and change Dataform's folders and
// configuration.
func TestStudioCodeAssets(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	api, err := dataform.NewServer(dataform.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	p := dataformProvider{svc: &dataformService{api: api}}
	call := func(name string, body any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		out, err := p.StudioCall(ctx, "cp", name, b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var m map[string]any
		raw, _ := json.Marshal(out)
		_ = json.Unmarshal(raw, &m)
		return m
	}
	folder := call("createfolder", map[string]any{"displayName": "Reports", "location": "us-central1"})
	q := call("save", map[string]any{"kind": "sql", "displayName": "Daily", "location": "us-central1", "parent": folder["name"], "content": "SELECT 1"})
	call("save", map[string]any{"name": q["name"], "content": "SELECT 2"})
	nb := call("save", map[string]any{"kind": "notebook", "displayName": "Explore", "content": `{"cells":[]}`})
	if got := call("read", map[string]any{"name": q["name"]})["content"]; got != "SELECT 2" {
		t.Errorf("read = %v", got)
	}
	versions := call("history", map[string]any{"name": q["name"]})["versions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("history = %v", versions)
	}
	first := versions[1].(map[string]any)["commit"]
	if got := call("read", map[string]any{"name": q["name"], "commit": first})["content"]; got != "SELECT 1" {
		t.Errorf("the first version = %v", got)
	}
	files := call("files", nil)["entries"].([]any)
	kinds := map[string]string{}
	for _, e := range files {
		m := e.(map[string]any)
		kinds[m["displayName"].(string)] = m["kind"].(string) + ":" + m["parent"].(string)
	}
	if kinds["Reports"] != "folder:" || kinds["Daily"] != "sql:"+folder["name"].(string) || kinds["Explore"] != "notebook:" {
		t.Errorf("files = %v", kinds)
	}
	pol := call("grant", map[string]any{"name": q["name"], "role": "roles/dataform.codeViewer", "member": "user:ana@example.com"})
	if b, _ := json.Marshal(pol["bindings"]); !strings.Contains(string(b), "user:ana@example.com") || !strings.Contains(string(b), "Code Viewer") {
		t.Errorf("grant = %s", b)
	}
	if _, err := p.StudioCall(ctx, "cp", "grant", json.RawMessage(`{"name":"`+q["name"].(string)+`","role":"roles/owner","member":"user:a@b"}`)); err == nil {
		t.Error("a role that is not a code role was granted")
	}
	call("move", map[string]any{"name": q["name"], "parent": ""})
	call("rename", map[string]any{"name": nb["name"], "displayName": "Explore 2"})

	// The Dataform page leaves the assets out; Source control lists the rest.
	if _, err := p.Create(ctx, "cp", map[string]string{"repositoryId": "code", "region": "us-central1"}); err != nil {
		t.Fatal(err)
	}
	listing, err := p.List(ctx, "cp")
	if err != nil || len(listing.Items) != 1 {
		t.Fatalf("Dataform page: %+v, %v", listing.Items, err)
	}
	repos := call("repositories", nil)["repositories"].([]any)
	if len(repos) != 1 || !strings.HasSuffix(repos[0].(map[string]any)["name"].(string), "/code") {
		t.Errorf("Source control = %v", repos)
	}

	fp := dataformFoldersProvider{df: p}
	fl, err := fp.List(ctx, "cp")
	if err != nil || len(fl.Items) != 1 || fl.Items[0].Fields["Type"] != "Folder" {
		t.Fatalf("Folders page: %+v, %v", fl, err)
	}
	if _, err := fp.Create(ctx, "cp", map[string]string{"type": "Team folder", "displayName": "Team", "region": "us-central1"}); err != nil {
		t.Fatal(err)
	}
	if err := fp.ActAt(ctx, "cp", []string{folder["name"].(string)}, "rename", map[string]string{"displayName": "Weekly"}); err != nil {
		t.Fatal(err)
	}
	d, err := fp.Detail(ctx, "cp", []string{folder["name"].(string)})
	if err != nil || d.Summary[1].Value != "Weekly" {
		t.Fatalf("folder page: %+v, %v", d, err)
	}
	if err := fp.ActAt(ctx, "cp", []string{folder["name"].(string)}, "delete", nil); err != nil {
		t.Fatal(err)
	}

	sp := dataformSettingsProvider{df: p}
	sl, err := sp.List(ctx, "cp")
	if err != nil || len(sl.Items) != len(dataform.Locations()) {
		t.Fatalf("Settings page: %d rows, %v", len(sl.Items), err)
	}
	call("delete", map[string]any{"name": nb["name"]})
}

// TestStudioAssetLabels (#2094): a new pipeline's repository carries
// Google's bigquery-workflow label and a data preparation's
// single-file-asset-type data_preparation_sqlx (manage-repository's
// table), and both read back as their kinds, as do the labels an earlier
// CloudBurrow wrote.
func TestStudioAssetLabels(t *testing.T) {
	t.Parallel()
	for kind, want := range map[string]map[string]string{
		"pipeline":         {"bigquery-workflow": "true"},
		"data_preparation": {"single-file-asset-type": "data_preparation_sqlx"},
		"sql":              {"single-file-asset-type": "sql"},
		"notebook":         {"single-file-asset-type": "notebook"},
		"data_canvas":      {"single-file-asset-type": "data_canvas"},
	} {
		got := dataformAssetLabels(kind)
		if len(got) != len(want) {
			t.Errorf("%s: labels %v, want %v", kind, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: labels %v, want %v", kind, got, want)
			}
		}
		if back := dataformAssetKind(&dfpb.Repository{Labels: got}); back != kind {
			t.Errorf("%v reads back as %q, want %q", got, back, kind)
		}
	}
	for labels, kind := range map[[2]string]string{
		{"single-file-asset-type", "pipeline"}:         "pipeline",
		{"single-file-asset-type", "data_preparation"}: "data_preparation",
		{"bigquery-workflow", "anything"}:              "pipeline",
		{"origin", "bigquery-studio"}:                  "",
	} {
		if got := dataformAssetKind(&dfpb.Repository{Labels: map[string]string{labels[0]: labels[1]}}); got != kind {
			t.Errorf("%v reads as %q, want %q", labels, got, kind)
		}
	}
}
