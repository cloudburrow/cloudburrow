package orgpolicy

import (
	"context"
	"errors"
	"testing"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
)

type fakeCustoms struct{ list []CustomConstraint }

func (fakeCustoms) Effective(context.Context, string, string) (Effective, error) {
	return Effective{}, nil
}
func (f fakeCustoms) Customs(_ context.Context, _, resourceType, _ string) ([]CustomConstraint, error) {
	var out []CustomConstraint
	for _, c := range f.list {
		if c.Name != "custom.other" || resourceType == TypeTopic {
			out = append(out, c)
		}
	}
	return out, nil
}

func enforced() Response {
	return AsResponse(Effective{Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{
		{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}}}}})
}

// TestCheckCustom (#1891): ALLOW refuses a resource its condition does
// not hold for, DENY one it holds for; a condition that cannot be
// evaluated refuses; durations compare with duration(); the message is
// Google's.
func TestCheckCustom(t *testing.T) {
	Use(fakeCustoms{list: []CustomConstraint{
		{Name: "custom.letters", Condition: "resource.name.matches('^[a-z]+$')", ActionType: "ALLOW", Description: "Letters only.", Effective: enforced()},
		{Name: "custom.ttl", Condition: "resource.versionDestroyTtl < duration('30h')", ActionType: "DENY", DisplayName: "TTL", Effective: enforced()},
	}})
	defer Use(nil)
	ctx := context.Background()
	if err := CheckCustom(ctx, "projects/p", TypeSecret, MethodCreate, map[string]any{"name": "abc", "versionDestroyTtl": "200000s"}); err != nil {
		t.Errorf("allowed resource: %v", err)
	}
	err := CheckCustom(ctx, "projects/p", TypeSecret, MethodCreate, map[string]any{"name": "abc1", "versionDestroyTtl": "3600s"})
	var v *CustomViolation
	if !errors.As(err, &v) || len(v.Names) != 2 {
		t.Fatalf("violations = %v", err)
	}
	want := `Operation denied by custom org policies: ["customConstraints/custom.letters":"Letters only.", "customConstraints/custom.ttl":"TTL"]`
	if v.Error() != want {
		t.Errorf("message = %s\nwant %s", v.Error(), want)
	}
	if msg, details := v.Bucket("abc1"); msg != "orgpolicy:projects/_/buckets/abc1 violates customConstraints/custom.letters." || details != "Letters only." {
		t.Errorf("bucket message = %q, %q", msg, details)
	}
	// No versionDestroyTtl: the DENY condition cannot be evaluated.
	if err := CheckCustom(ctx, "projects/p", TypeSecret, MethodCreate, map[string]any{"name": "abc"}); err == nil {
		t.Error("a condition on a missing field allowed the resource")
	}
}

func TestEvaluate(t *testing.T) {
	for cond, want := range map[string]bool{
		"resource.storageClass == 'STANDARD'":                              true,
		"resource.versioning.enabled == true":                              false,
		"resource.labels['team'] == 'a'":                                   true,
		"resource.retentionPolicy.retentionPeriod > 100":                   true,
		"resource.lifecycle.rule.exists(r, r.action.type == 'Delete')":     true,
		"has(resource.website)":                                            false,
		"resource.location.startsWith('US') && size(resource.labels) == 1": true,
	} {
		got, err := Evaluate(cond, map[string]any{"storageClass": "STANDARD", "versioning": map[string]any{"enabled": false},
			"labels": map[string]string{"team": "a"}, "retentionPolicy": map[string]any{"retentionPeriod": 3600},
			"lifecycle": map[string]any{"rule": []any{map[string]any{"action": map[string]any{"type": "Delete"}}}}, "location": "US-EAST1"})
		if err != nil || got != want {
			t.Errorf("%s = %v, %v; want %v", cond, got, err, want)
		}
	}
	if err := CheckCondition("resource.name =="); err == nil {
		t.Error("a malformed condition compiled")
	}
	if err := CheckCondition("resource.name"); err != nil {
		t.Errorf("a dyn condition: %v", err)
	}
}
