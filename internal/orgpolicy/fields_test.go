package orgpolicy

import (
	"reflect"
	"testing"
)

// TestRestrict (#1895): a registered type is read on its documented
// fields alone: a leaf keeps its value whole, a path through a list
// applies to each element, and anything else is absent.
func TestRestrict(t *testing.T) {
	obj := map[string]any{"name": "n", "revisionId": "r", "labels": map[string]any{"a": "b"},
		"spec": map[string]any{"containers": []any{map[string]any{"image": "i", "env": "e"}}, "other": 1}}
	got := restrict(obj, []string{"name", "resource.labels", "spec.containers.image", "missing.deep"})
	want := map[string]any{"name": "n", "labels": map[string]any{"a": "b"},
		"spec": map[string]any{"containers": []any{map[string]any{"image": "i"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("restrict = %v\nwant %v", got, want)
	}
	if ok, err := evaluate("resource.revisionId == ''", TypeSchema, map[string]any{"name": "n", "revisionId": ""}); err == nil {
		t.Errorf("an unlisted Schema field evaluated: %v", ok)
	}
	if ok, err := evaluate("resource.expireTime < timestamp('2030-01-01T00:00:00Z')", TypeSnapshot,
		map[string]any{"expireTime": "2029-01-01T00:00:00Z", "topic": "t"}); err != nil || !ok {
		t.Errorf("a Snapshot's expireTime = %v, %v", ok, err)
	}
}
