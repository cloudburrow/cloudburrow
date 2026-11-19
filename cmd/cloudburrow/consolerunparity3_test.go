package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestWriteFunctionInlineEditor (#1852): Write a function's source is
// Google's inline editor, starting from the default runtime's sample and
// following the runtime, and its files are zipped for Create function; a
// name that leaves the source is refused.
func TestWriteFunctionInlineEditor(t *testing.T) {
	fields := inlineSourceFields([]console.Field{
		{Name: "runtime", Type: "select", Default: "python312"},
		{Name: "entryPoint", Type: "text"},
		{Name: "file", Type: console.FileFieldType},
	})
	var src console.Field
	for _, f := range fields {
		if f.Type == console.FileFieldType {
			t.Fatal("the .zip field is still there")
		}
		if f.Name == "entryPoint" && f.Default != "hello_http" {
			t.Errorf("entry point default = %q", f.Default)
		}
		if f.Name == runSourceFilesField {
			src = f
		}
	}
	if src.Type != "sourcefiles" || src.Follows != "runtime" || !strings.Contains(src.Default, "main.py") ||
		src.Templates["nodejs22"]["package.json"] == "" || src.Templates["go125"]["go.mod"] == "" {
		t.Fatalf("source field = %+v", src)
	}
	data, err := zipSourceFiles(src.Default)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "main.py,requirements.txt" {
		t.Errorf("zipped %v", names)
	}
	for _, bad := range []string{`{"../x":"y"}`, `{"/etc/x":"y"}`, `{}`, `nope`} {
		if _, err := zipSourceFiles(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// TestConnectRepoBuildIsGooglesTrigger (#1852): Connect repo's trigger
// builds with docker or Google's buildpacks, pushes, and deploys with
// gcloud run services update, with Google's substitutions.
func TestConnectRepoBuildIsGooglesTrigger(t *testing.T) {
	b, subs, err := connectRepoBuild("p", "us-central1", "web", map[string]string{"buildType": runBuildDockerfile, "dockerfile": "/app/Dockerfile"})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.GetSteps()) != 3 || strings.Join(b.GetSteps()[0].GetArgs()[4:], " ") != ". -f app/Dockerfile" ||
		b.GetSteps()[2].GetEntrypoint() != "gcloud" || b.GetSteps()[2].GetArgs()[3] != "$_SERVICE_NAME" {
		j, _ := json.Marshal(b)
		t.Errorf("build = %s", j)
	}
	if subs["_SERVICE_NAME"] != "web" || subs["_DEPLOY_REGION"] != "us-central1" || subs["_AR_HOSTNAME"] != "us-central1-docker.pkg.dev" {
		t.Errorf("substitutions = %v", subs)
	}
	b, _, err = connectRepoBuild("p", "us-central1", "web", map[string]string{"buildType": runBuildPacks, "context": "/svc", "functionTarget": "Hello"})
	if err != nil || b.GetSteps()[0].GetName() != "gcr.io/k8s-skaffold/pack" ||
		!strings.Contains(strings.Join(b.GetSteps()[0].GetArgs(), " "), "--path=svc --env=GOOGLE_FUNCTION_TARGET=Hello") {
		t.Errorf("buildpacks build = %v, %v", b, err)
	}
	if _, _, err := connectRepoBuild("p", "us-central1", "web", map[string]string{"dockerfile": "../x"}); err == nil {
		t.Error("a Dockerfile outside the repository was accepted")
	}
}
