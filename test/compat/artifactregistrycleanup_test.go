//go:build compat

package compat

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/logging/logadmin"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	auditpb "google.golang.org/genproto/googleapis/cloud/audit"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// cleanupInterval is the instance's --artifact-registry-cleanup-interval,
// which the test is told through the same variable `up` reads; a test
// cannot wait out Google's daily run.
func cleanupInterval(t *testing.T) time.Duration {
	v := os.Getenv("CLOUDBURROW_ARTIFACT_REGISTRY_CLEANUP_INTERVAL")
	d, err := time.ParseDuration(v)
	if v == "" || err != nil || d <= 0 || d > 2*time.Minute {
		t.Skip("start the instance with CLOUDBURROW_ARTIFACT_REGISTRY_CLEANUP_INTERVAL of 2m or less, and export it here, to run cleanup policies in a test")
	}
	return d
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/ListVersions
//
// TestArtifactRegistryCleanupPolicies (#1897): a DOCKER repository's
// cleanup policies, set with the official client, run in the background.
// A Delete policy of untagged versions and a Keep policy of the "keepme"
// package: in a dry run nothing is deleted, and when Cloud Logging is
// served the run's BatchDeleteVersions Data Access audit entry names the
// untagged version with validateOnly true; with the dry run off the
// untagged version of "app" is deleted, its tagged version and the kept
// package's untagged version stay. More than 10 policies are refused.
func TestArtifactRegistryCleanupPolicies(t *testing.T) {
	h := New(t)
	every := cleanupInterval(t)
	ar, ctx := arClient(t, h)
	parent := runParent(h)
	repo := formatRepo(t, ctx, ar, parent, "cleanup-docker", &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER})
	port := h.Endpoint(EnvRun)[strings.LastIndex(h.Endpoint(EnvRun), ":")+1:]
	base := "us-central1-docker.pkg.localhost:" + port + "/" + h.Project() + "/cleanup-docker/"
	opts := registryOptions(h)
	push := func(ref string) string {
		img, err := random.Image(256, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := crane.Push(img, ref, opts...); err != nil {
			t.Fatalf("push %s: %v", ref, err)
		}
		d, err := img.Digest()
		if err != nil {
			t.Fatal(err)
		}
		return d.String()
	}
	tagged := push(base + "app:v1")
	untagged := push(base + "app:tmp")
	kept := push(base + "keepme:tmp")
	for _, tag := range []string{"app/tags/tmp", "keepme/tags/tmp"} {
		if err := ar.DeleteTag(ctx, &artifactregistrypb.DeleteTagRequest{Name: repo.GetName() + "/packages/" + tag}); err != nil {
			t.Fatalf("DeleteTag %s: %v", tag, err)
		}
	}
	untaggedName := repo.GetName() + "/packages/app/versions/" + untagged

	tooMany := map[string]*artifactregistrypb.CleanupPolicy{}
	for i := 0; i < 11; i++ {
		id := "p" + string(rune('a'+i))
		tooMany[id] = &artifactregistrypb.CleanupPolicy{Id: id, Action: artifactregistrypb.CleanupPolicy_DELETE,
			ConditionType: &artifactregistrypb.CleanupPolicy_Condition{Condition: &artifactregistrypb.CleanupPolicyCondition{}}}
	}
	mask := &fieldmaskpb.FieldMask{Paths: []string{"cleanup_policies", "cleanup_policy_dry_run"}}
	if _, err := ar.UpdateRepository(ctx, &artifactregistrypb.UpdateRepositoryRequest{UpdateMask: mask,
		Repository: &artifactregistrypb.Repository{Name: repo.GetName(), CleanupPolicies: tooMany}}); err == nil {
		t.Error("UpdateRepository with 11 cleanup policies succeeded; want INVALID_ARGUMENT")
	}

	policies := map[string]*artifactregistrypb.CleanupPolicy{
		"delete-untagged": {Id: "delete-untagged", Action: artifactregistrypb.CleanupPolicy_DELETE,
			ConditionType: &artifactregistrypb.CleanupPolicy_Condition{Condition: &artifactregistrypb.CleanupPolicyCondition{
				TagState: artifactregistrypb.CleanupPolicyCondition_UNTAGGED.Enum()}}},
		"keep-keepme": {Id: "keep-keepme", Action: artifactregistrypb.CleanupPolicy_KEEP,
			ConditionType: &artifactregistrypb.CleanupPolicy_Condition{Condition: &artifactregistrypb.CleanupPolicyCondition{
				PackageNamePrefixes: []string{"keep"}}}},
	}
	set := func(dry bool) {
		t.Helper()
		if _, err := ar.UpdateRepository(ctx, &artifactregistrypb.UpdateRepositoryRequest{UpdateMask: mask,
			Repository: &artifactregistrypb.Repository{Name: repo.GetName(), CleanupPolicies: policies, CleanupPolicyDryRun: dry}}); err != nil {
			t.Fatalf("UpdateRepository: %v", err)
		}
	}
	versions := func(pkg string) map[string]bool {
		t.Helper()
		out := map[string]bool{}
		it := ar.ListVersions(ctx, &artifactregistrypb.ListVersionsRequest{Parent: repo.GetName() + "/packages/" + pkg})
		for {
			v, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("ListVersions %s: %v", pkg, err)
			}
			out[v.GetName()[strings.LastIndex(v.GetName(), "/")+1:]] = true
		}
	}

	set(true)
	time.Sleep(3 * every)
	if vs := versions("app"); !vs[untagged] || !vs[tagged] {
		t.Fatalf("after a dry run the app versions are %v; want both kept", vs)
	}
	if os.Getenv(EnvLogging) != "" {
		checkCleanupAudit(t, h, ctx, untaggedName)
	}

	set(false)
	deadline := time.Now().Add(3*every + 30*time.Second)
	for versions("app")[untagged] && time.Now().Before(deadline) {
		time.Sleep(every / 2)
	}
	if vs := versions("app"); vs[untagged] || !vs[tagged] {
		t.Errorf("after a run the app versions are %v; want %s only", vs, tagged)
	}
	if vs := versions("keepme"); !vs[kept] {
		t.Errorf("after a run the keepme versions are %v; want %s kept by the Keep policy", vs, kept)
	}
}

// checkCleanupAudit finds the dry run's BatchDeleteVersions entry.
func checkCleanupAudit(t *testing.T, h *Harness, ctx context.Context, version string) {
	t.Helper()
	ac, err := logadmin.NewClient(ctx, "projects/"+h.Project(), option.WithEndpoint(h.Endpoint(EnvLogging)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	it := ac.Entries(ctx, logadmin.Filter(`logName="projects/`+h.Project()+`/logs/cloudaudit.googleapis.com%2Fdata_access"`))
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("logadmin Entries: %v", err)
		}
		al, ok := e.Payload.(*auditpb.AuditLog)
		if !ok || al.GetMethodName() != "google.devtools.artifactregistry.v1.ArtifactRegistry.BatchDeleteVersions" {
			continue
		}
		f := al.GetRequest().GetFields()
		for _, n := range f["names"].GetListValue().GetValues() {
			if n.GetStringValue() == version && f["validateOnly"].GetBoolValue() {
				return
			}
		}
	}
	t.Errorf("no BatchDeleteVersions audit entry with validateOnly true names %s", version)
}
