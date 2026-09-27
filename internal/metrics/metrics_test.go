package metrics

import (
	"reflect"
	"strings"
	"testing"
)

// The unmeasured set is the enabled services minus those an observer was
// registered for (#600), in the order they were given, and /metrics reports
// exactly that set.
func TestUnmeasuredIsDerivedFromMeasure(t *testing.T) {
	r := New("storage", "pubsub", "scheduler", "logging")
	if got, want := r.Unmeasured(), []string{"storage", "pubsub", "scheduler", "logging"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("before any observer, unmeasured = %v, want %v", got, want)
	}
	r.Measure("scheduler")
	r.Measure("logging")
	r.Measure("resourcemanager") // measured but not in the enabled list: no effect
	if got, want := r.Unmeasured(), []string{"storage", "pubsub"}; !reflect.DeepEqual(got, want) {
		t.Errorf("unmeasured = %v, want %v", got, want)
	}
	var b strings.Builder
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, s := range []string{"scheduler", "logging", "resourcemanager"} {
		if strings.Contains(out, `cloudburrow_service_measured{service="`+s+`"}`) {
			t.Errorf("%s is reported unmeasured:\n%s", s, out)
		}
	}
	if !strings.Contains(out, `cloudburrow_service_measured{service="pubsub"} 0`) {
		t.Errorf("pubsub is not reported unmeasured:\n%s", out)
	}
	var nilReg *Registry
	nilReg.Measure("tasks") // a nil registry, as callEvents may be given, is a no-op
}
