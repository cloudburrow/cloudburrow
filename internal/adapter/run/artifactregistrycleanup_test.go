package run

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	auditpb "google.golang.org/genproto/googleapis/cloud/audit"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/registry"
)

func ptrInt32(n int32) *int32 { return &n }

// Each documented condition, keep overriding delete, most recent versions
// per package, the 10-policy limit, tag_prefixes needing TAGGED and no
// policies on a virtual repository.
func TestCleanupPolicyEvaluation(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	vs := []cleanupVersion{
		{Name: "a1", PackageName: "pa", Package: "alpha", Version: "v0.1", Created: now.Add(-40 * day)},
		{Name: "a2", PackageName: "pa", Package: "alpha", Version: "v1.0", Tags: []string{"release-1"}, Created: now.Add(-35 * day)},
		{Name: "a3", PackageName: "pa", Package: "alpha", Version: "v1.1", Created: now.Add(-2 * day)},
		{Name: "b1", PackageName: "pb", Package: "beta", Version: "v0.2", Created: now.Add(-50 * day)},
		{Name: "b2", PackageName: "pb", Package: "beta", Version: "v0.3", Tags: []string{"dev"}, Created: now.Add(-1 * day)},
		{Name: "c1", PackageName: "pc", Package: "gamma", Version: "v9", Created: now.Add(-60 * day), listed: true},
	}
	del := func(c *arpb.CleanupPolicyCondition) *arpb.CleanupPolicy {
		return &arpb.CleanupPolicy{Action: arpb.CleanupPolicy_DELETE, ConditionType: &arpb.CleanupPolicy_Condition{Condition: c}}
	}
	keep := func(c *arpb.CleanupPolicyCondition) *arpb.CleanupPolicy {
		return &arpb.CleanupPolicy{Action: arpb.CleanupPolicy_KEEP, ConditionType: &arpb.CleanupPolicy_Condition{Condition: c}}
	}
	untagged := arpb.CleanupPolicyCondition_UNTAGGED.Enum()
	tagged := arpb.CleanupPolicyCondition_TAGGED.Enum()
	for _, tc := range []struct {
		name     string
		policies map[string]*arpb.CleanupPolicy
		want     []string
	}{
		{"no delete policy deletes nothing", map[string]*arpb.CleanupPolicy{"k": keep(&arpb.CleanupPolicyCondition{})}, nil},
		{"untagged", map[string]*arpb.CleanupPolicy{"d": del(&arpb.CleanupPolicyCondition{TagState: untagged})}, []string{"a1", "a3", "b1"}},
		{"tagged with prefix", map[string]*arpb.CleanupPolicy{"d": del(&arpb.CleanupPolicyCondition{TagState: tagged, TagPrefixes: []string{"rel"}})}, []string{"a2"}},
		{"older than 30d", map[string]*arpb.CleanupPolicy{"d": del(&arpb.CleanupPolicyCondition{OlderThan: durationpb.New(30 * day)})}, []string{"a1", "a2", "b1"}},
		{"newer than 3d", map[string]*arpb.CleanupPolicy{"d": del(&arpb.CleanupPolicyCondition{NewerThan: durationpb.New(3 * day)})}, []string{"a3", "b2"}},
		{"version prefixes OR, AND with age", map[string]*arpb.CleanupPolicy{"d": del(&arpb.CleanupPolicyCondition{
			VersionNamePrefixes: []string{"v0", "alpha"}, OlderThan: durationpb.New(30 * day)})}, []string{"a1", "b1"}},
		{"package prefix", map[string]*arpb.CleanupPolicy{"d": del(&arpb.CleanupPolicyCondition{PackageNamePrefixes: []string{"be"}})}, []string{"b1", "b2"}},
		{"keep wins", map[string]*arpb.CleanupPolicy{
			"d": del(&arpb.CleanupPolicyCondition{TagState: arpb.CleanupPolicyCondition_ANY.Enum()}),
			"k": keep(&arpb.CleanupPolicyCondition{PackageNamePrefixes: []string{"alp"}}),
		}, []string{"b1", "b2"}},
		{"keep most recent", map[string]*arpb.CleanupPolicy{
			"d": del(&arpb.CleanupPolicyCondition{}),
			"k": {Action: arpb.CleanupPolicy_KEEP, ConditionType: &arpb.CleanupPolicy_MostRecentVersions{
				MostRecentVersions: &arpb.CleanupPolicyMostRecentVersions{KeepCount: ptrInt32(1)}}},
		}, []string{"a1", "a2", "b1"}},
		{"keep most recent of a prefix", map[string]*arpb.CleanupPolicy{
			"d": del(&arpb.CleanupPolicyCondition{}),
			"k": {Action: arpb.CleanupPolicy_KEEP, ConditionType: &arpb.CleanupPolicy_MostRecentVersions{
				MostRecentVersions: &arpb.CleanupPolicyMostRecentVersions{KeepCount: ptrInt32(2), PackageNamePrefixes: []string{"alpha"}}}},
		}, []string{"a1", "b1", "b2"}},
	} {
		r := &arpb.Repository{Name: "projects/p/locations/l/repositories/r", CleanupPolicies: tc.policies}
		var got []string
		for _, names := range cleanupDeletions(r, vs, now) {
			got = append(got, names...)
		}
		sort.Strings(got)
		if len(got) != len(tc.want) {
			t.Errorf("%s: deletes %v; want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: deletes %v; want %v", tc.name, got, tc.want)
				break
			}
		}
	}
	imm := &arpb.Repository{CleanupPolicies: map[string]*arpb.CleanupPolicy{"d": del(&arpb.CleanupPolicyCondition{})},
		FormatConfig: &arpb.Repository_DockerConfig{DockerConfig: &arpb.Repository_DockerRepositoryConfig{ImmutableTags: true}}}
	for _, names := range cleanupDeletions(imm, vs, now) {
		for _, n := range names {
			if n == "a2" || n == "b2" {
				t.Errorf("immutable tags: tagged %s deleted", n)
			}
		}
	}

	many := map[string]*arpb.CleanupPolicy{}
	for i := 0; i < 11; i++ {
		many[string(rune('a'+i))] = del(&arpb.CleanupPolicyCondition{})
	}
	for name, r := range map[string]*arpb.Repository{
		"11 policies":  {CleanupPolicies: many},
		"tag prefixes": {CleanupPolicies: map[string]*arpb.CleanupPolicy{"d": del(&arpb.CleanupPolicyCondition{TagPrefixes: []string{"x"}})}},
		"no condition": {CleanupPolicies: map[string]*arpb.CleanupPolicy{"d": {Action: arpb.CleanupPolicy_DELETE}}},
		"virtual": {Mode: arpb.Repository_VIRTUAL_REPOSITORY,
			CleanupPolicies: map[string]*arpb.CleanupPolicy{"d": del(&arpb.CleanupPolicyCondition{})}},
	} {
		if err := checkCleanupPolicies(r); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v; want INVALID_ARGUMENT", name, err)
		}
	}
}

// A run deletes a generic repository's matching versions, or in a dry run
// deletes nothing; either way it writes BatchDeleteVersions' Data Access
// audit entry, validateOnly true for the dry run.
func TestCleanupPoliciesRun(t *testing.T) {
	st, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	UseRegistry(st)
	t.Cleanup(func() { UseRegistry(nil) })
	var mu sync.Mutex
	var entries []*loggingpb.LogEntry
	UseCleanupLogging(func(_ context.Context, e *loggingpb.LogEntry) error {
		mu.Lock()
		entries = append(entries, e)
		mu.Unlock()
		return nil
	})
	t.Cleanup(func() { UseCleanupLogging(nil) })
	a := NewServer(&Knative{}, "t", time.Minute).ArtifactRegistry()
	ctx := context.Background()
	parent := "projects/cleanup-unit/locations/us-central1"
	name := parent + "/repositories/gen"
	policy := map[string]*arpb.CleanupPolicy{"old": {Id: "old", Action: arpb.CleanupPolicy_DELETE,
		ConditionType: &arpb.CleanupPolicy_Condition{Condition: &arpb.CleanupPolicyCondition{VersionNamePrefixes: []string{"0."}}}}}
	if _, err := a.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "gen",
		Repository: &arpb.Repository{Format: arpb.Repository_GENERIC, CleanupPolicies: policy, CleanupPolicyDryRun: true}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RemoveRepository(name) })
	for _, v := range []string{"0.1", "1.0"} {
		if _, err := addFile(name, "tool:"+v+":f.txt", "tool", v, []byte(v), nil); err != nil {
			t.Fatal(err)
		}
	}
	RunCleanupPolicies(ctx)
	if n := len(versionsOf(name)); n != 2 {
		t.Fatalf("a dry run left %d versions; want 2", n)
	}
	r, _ := repoFormat(name)
	r.CleanupPolicyDryRun = false
	RunCleanupPolicies(ctx)
	vs := versionsOf(name)
	if len(vs) != 1 || vs[0].Version != "1.0" {
		t.Fatalf("after a run the versions are %v; want 1.0 only", vs)
	}
	if len(entries) != 2 {
		t.Fatalf("%d audit entries; want 2", len(entries))
	}
	for i, wantDry := range []bool{true, false} {
		e := entries[i]
		if e.GetLogName() != "projects/cleanup-unit/logs/cloudaudit.googleapis.com%2Fdata_access" {
			t.Errorf("logName %q", e.GetLogName())
		}
		var al auditpb.AuditLog
		if err := e.GetProtoPayload().UnmarshalTo(&al); err != nil {
			t.Fatal(err)
		}
		f := al.GetRequest().GetFields()
		if al.GetMethodName() != "google.devtools.artifactregistry.v1.ArtifactRegistry.BatchDeleteVersions" ||
			f["validateOnly"].GetBoolValue() != wantDry || f["parent"].GetStringValue() != name+"/packages/tool" ||
			len(f["names"].GetListValue().GetValues()) != 1 || f["names"].GetListValue().GetValues()[0].GetStringValue() != name+"/packages/tool/versions/0.1" {
			t.Errorf("entry %d: %v", i, &al)
		}
	}
}
