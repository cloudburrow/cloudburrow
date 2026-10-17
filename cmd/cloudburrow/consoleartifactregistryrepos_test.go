package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
)

// The Repositories page's Create repository and a repository's Edit
// repository (the console parity audit) reach CreateRepository and
// UpdateRepository on the Run port: the list then shows the repository, its
// page its description, and Edit opens prefilled with what it set.
func TestConsoleArtifactRegistryCreateAndEditRepository(t *testing.T) {
	ctx := context.Background()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	g := grpc.NewServer()
	s.ArtifactRegistry().Register(g)
	runadapter.NewOperationsServer(s).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	p := artifactRegistryProvider{defaultProject: "console-ar-create", runAddr: func() string { return ln.Addr().String() }}
	t.Cleanup(func() { _ = p.Delete(ctx, "", "made") })

	if _, err := p.Create(ctx, "", map[string]string{"id": "made", "format": "DOCKER", "location": "us-east1",
		"description": "first", "labels": "team=web"}); err != nil {
		t.Fatalf("Create repository: %v", err)
	}
	l, _ := p.List(ctx, "")
	if len(l.Items) != 1 || l.Items[0].Name != "made" || l.Items[0].Fields["Location"] != "us-east1" || l.Items[0].Fields["Description"] != "first" {
		t.Fatalf("repositories = %+v", l.Items)
	}
	if _, err := p.Create(ctx, "", map[string]string{"id": "made", "location": "us-east1"}); err == nil {
		t.Error("a second repository of the same name was created")
	}
	defaults := map[string]string{}
	for _, a := range p.DetailActions(ctx, "", []string{"made"}) {
		if a.ID == "edit-repository" {
			for _, f := range a.Fields {
				defaults[f.Name] = f.Default
			}
		}
	}
	if defaults["description"] != "first" || defaults["labels"] != "team=web" {
		t.Fatalf("Edit repository opens on %v; want the repository's description and labels", defaults)
	}
	if err := p.ActAt(ctx, "", []string{"made"}, "edit-repository", map[string]string{"description": "second", "labels": "team=api, env=dev"}); err != nil {
		t.Fatalf("Edit repository: %v", err)
	}
	r, _, err := p.repository("console-ar-create", "made")
	if err != nil || r.GetDescription() != "second" || r.GetLabels()["team"] != "api" || r.GetLabels()["env"] != "dev" {
		t.Errorf("the repository after Edit repository = %v, %v", r, err)
	}
	if err := p.ActAt(ctx, "", []string{"made"}, "edit-repository", map[string]string{"labels": "nokey"}); err == nil {
		t.Error("Edit repository took labels that are not key=value")
	}
}

// Create repository's Mode and Upstream repositories (#1546) make a remote
// repository over a standard one, and its page names the upstream.
func TestConsoleArtifactRegistryCreateRemoteRepository(t *testing.T) {
	ctx := context.Background()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	g := grpc.NewServer()
	s.ArtifactRegistry().Register(g)
	runadapter.NewOperationsServer(s).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	p := artifactRegistryProvider{defaultProject: "console-ar-remote", runAddr: func() string { return ln.Addr().String() }}
	t.Cleanup(func() { _ = p.Delete(ctx, "", "cache"); _ = p.Delete(ctx, "", "base") })
	if _, err := p.Create(ctx, "", map[string]string{"id": "base", "format": "PYTHON", "location": "us-east1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, "", map[string]string{"id": "cache", "format": "PYTHON", "location": "us-east1", "mode": "REMOTE_REPOSITORY"}); err == nil {
		t.Error("a remote repository with no upstream was created")
	}
	if _, err := p.Create(ctx, "", map[string]string{"id": "cache", "format": "PYTHON", "location": "us-east1",
		"mode": "REMOTE_REPOSITORY", "upstream": "base"}); err != nil {
		t.Fatalf("Create repository, remote: %v", err)
	}
	r, _, err := p.repository("console-ar-remote", "cache")
	if err != nil || r.GetMode().String() != "REMOTE_REPOSITORY" ||
		r.GetRemoteRepositoryConfig().GetCommonRepository().GetUri() != "projects/console-ar-remote/locations/us-east1/repositories/base" {
		t.Fatalf("the remote repository = %v, %v", r, err)
	}
	d, err := p.Detail(ctx, "", []string{"cache"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range d.Summary {
		found = found || (x.Label == "Mode" && strings.Contains(x.Value, "pulls through from projects/console-ar-remote/locations/us-east1/repositories/base"))
	}
	if !found {
		t.Errorf("the remote repository's page = %+v; want its Mode to name the upstream", d.Summary)
	}
}
