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
	if defaults["description"] != "first" || defaults["labels"] != `{"team":"web"}` || defaults["immutableTags"] != arDisabled {
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

// Google's Create repository page (#1855): a Multi-Region location, a
// customer-managed key, immutable image tags, cleanup policies in dry run,
// a Maven version policy and a virtual repository's upstream rows each
// reach CreateRepository; Go, Kubeflow Pipelines, a preset upstream and
// vulnerability scanning, drawn disabled, are refused.
func TestConsoleArtifactRegistryCreatePage(t *testing.T) {
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
	const proj = "console-ar-page"
	p := artifactRegistryProvider{defaultProject: proj, runAddr: func() string { return ln.Addr().String() }}
	t.Cleanup(func() {
		for _, id := range []string{"virt", "jars", "base", "multi"} {
			_ = p.Delete(ctx, "", id)
		}
	})
	key := "projects/" + proj + "/locations/us/keyRings/r/cryptoKeys/k"
	if _, err := p.Create(ctx, "", map[string]string{"id": "multi", "format": "DOCKER", "locationType": arLocMultiRegion, "multiRegion": "us",
		"location": "us-central1", "labels": `{"team":"web"}`, "encryption": arEncCMEK, "kmsKey": key, "immutableTags": arEnabled,
		"cleanupMode": arCleanupDryRun, "cleanupPolicies": `[{"name":"untagged","type":"Conditional delete","tagState":"Untagged","olderThan":"30d"}]`,
		"vulnerabilityScanning": arDisabled}); err != nil {
		t.Fatalf("Create repository: %v", err)
	}
	r, _, err := p.repository(proj, "multi")
	if err != nil || !strings.Contains(r.GetName(), "/locations/us/") || r.GetKmsKeyName() != key || !r.GetDockerConfig().GetImmutableTags() ||
		!r.GetCleanupPolicyDryRun() || r.GetCleanupPolicies()["untagged"].GetCondition().GetOlderThan().GetSeconds() != 30*86400 ||
		r.GetLabels()["team"] != "web" {
		t.Fatalf("the repository = %v, %v", r, err)
	}
	l, _ := p.List(ctx, "")
	if len(l.Items) != 1 || l.Items[0].Fields["Encryption"] != "Customer-managed key" || l.Items[0].Fields["Cleanup policies"] != "1 (dry run)" ||
		l.Items[0].Fields["Labels"] != "team:web" || l.Items[0].Fields["Location"] != "us" {
		t.Errorf("Repositories = %+v", l.Items)
	}
	// Edit Repository turns immutable tags off.
	if err := p.ActAt(ctx, "", []string{"multi"}, "edit-repository", map[string]string{"immutableTags": arDisabled, "labels": `{"team":"web"}`}); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := p.repository(proj, "multi"); r.GetDockerConfig().GetImmutableTags() || r.GetKmsKeyName() != key {
		t.Errorf("after Edit Repository: %v", r)
	}

	if _, err := p.Create(ctx, "", map[string]string{"id": "jars", "format": "MAVEN", "location": "us-east1",
		"versionPolicy": "SNAPSHOT", "allowSnapshotOverwrites": "true"}); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := p.repository(proj, "jars"); r.GetMavenConfig().GetVersionPolicy().String() != "SNAPSHOT" || !r.GetMavenConfig().GetAllowSnapshotOverwrites() {
		t.Errorf("the Maven repository = %v", r)
	}
	if _, err := p.Create(ctx, "", map[string]string{"id": "base", "format": "MAVEN", "location": "us-east1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, "", map[string]string{"id": "virt", "format": "MAVEN", "location": "us-east1", "mode": "VIRTUAL_REPOSITORY",
		"upstreams": `[{"policy":"first","repository":"jars","priority":"10"},{"policy":"second","repository":"base","priority":"5"}]`}); err != nil {
		t.Fatalf("Create repository, virtual: %v", err)
	}
	r, _, _ = p.repository(proj, "virt")
	if ups := r.GetVirtualRepositoryConfig().GetUpstreamPolicies(); len(ups) != 2 || ups[0].GetId() != "first" || ups[0].GetPriority() != 10 ||
		!strings.HasSuffix(ups[1].GetRepository(), "/repositories/base") {
		t.Errorf("the virtual repository's upstreams = %v", ups)
	}
	for name, v := range map[string]map[string]string{
		"Go":                     {"format": "GO"},
		"Kubeflow Pipelines":     {"format": "KFP"},
		"a preset upstream":      {"mode": "REMOTE_REPOSITORY", "remoteSource": arRemotePreset, "upstream": "base"},
		"vulnerability scanning": {"vulnerabilityScanning": arEnabled},
	} {
		v["id"], v["location"] = "refused", "us-east1"
		if _, err := p.Create(ctx, "", v); err == nil || !strings.Contains(err.Error(), "Not available locally") {
			_ = p.Delete(ctx, "", "refused")
			t.Errorf("Create repository with %s = %v; want refused as not available locally", name, err)
		}
	}
	_, fields := p.CreateForm()
	labels := map[string]string{}
	for _, f := range fields {
		labels[f.Name] = f.Label
	}
	if labels["locationType"] != "Location type" || labels["encryption"] != "Encryption" || labels["immutableTags"] != "Immutable image tags" ||
		labels["cleanupPolicies"] != "Cleanup policy" || labels["vulnerabilityScanning"] != "Vulnerability scanning" {
		t.Errorf("Create repository's fields = %v", labels)
	}
}
