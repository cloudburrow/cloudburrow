package run

import (
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"
)

func TestSplitCountFirstListedRoundsUp(t *testing.T) {
	got := splitCount(10, []revisionShare{{"a", 60}, {"b", 40}})
	if got[0] != 6 || got[1] != 4 {
		t.Fatalf("10 at 60/40 = %v, want [6 4]", got)
	}
	got = splitCount(3, []revisionShare{{"a", 50}, {"b", 50}})
	if got[0] != 2 || got[1] != 1 {
		t.Fatalf("3 at 50/50 = %v, want [2 1]", got)
	}
	got = splitCount(5, []revisionShare{{"a", 100}, {"t", 0}})
	if got[0] != 5 || got[1] != 0 {
		t.Fatalf("5 at 100/0 = %v, want [5 0]", got)
	}
}

func TestEffectiveScale(t *testing.T) {
	own := map[string]*runpb.RevisionScaling{"a": {MinInstanceCount: 5}, "b": {MaxInstanceCount: 2}}
	get := func(n string) *runpb.RevisionScaling { return own[n] }
	shares := []revisionShare{{"a", 60}, {"b", 40}}
	// Highest minimum wins, the lower maximum caps it.
	got, err := effectiveScale(&runpb.ServiceScaling{MinInstanceCount: 10, MaxInstanceCount: 20}, shares, get)
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != (revisionScale{6, 12}) || got["b"] != (revisionScale{2, 2}) {
		t.Fatalf("automatic = %v", got)
	}
	// MANUAL ignores revision-level settings.
	got, err = effectiveScale(&runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(5)},
		append(shares, revisionShare{"t", 0}), get)
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != (revisionScale{3, 3}) || got["b"] != (revisionScale{2, 2}) || got["t"] != (revisionScale{0, 1}) {
		t.Fatalf("manual = %v", got)
	}
	// MANUAL 0 disables the service (#1800): no instance for a revision with
	// traffic, at most one for a tag-only one.
	got, err = effectiveScale(&runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(0)},
		append(shares, revisionShare{"t", 0}), get)
	if err != nil || got["a"] != (revisionScale{0, 1}) || got["b"] != (revisionScale{0, 1}) || got["t"] != (revisionScale{0, 1}) {
		t.Fatalf("manual 0 = %v, %v", got, err)
	}
	// So does MANUAL with no count, as Terraform sends manual_instance_count = 0.
	if got, _ = effectiveScale(&runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL}, shares, get); got["a"] != (revisionScale{0, 1}) {
		t.Fatalf("manual with no count = %v", got)
	}
	// A count that leaves one serving revision at zero is still refused.
	if _, err := effectiveScale(&runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(1)},
		shares, get); err == nil {
		t.Fatal("manual 1 over two serving revisions accepted")
	}
	// No service scaling: each revision's own.
	got, _ = effectiveScale(nil, shares, get)
	if got["a"] != (revisionScale{5, 0}) || got["b"] != (revisionScale{0, 2}) {
		t.Fatalf("none = %v", got)
	}
}

func TestCheckScalingLimits(t *testing.T) {
	f := proto.Float32
	for _, tc := range []struct {
		svc  *runpb.Service
		want string
	}{
		{&runpb.Service{Scaling: &runpb.ServiceScaling{MinInstanceCount: -1}}, "must not be negative"},
		{&runpb.Service{Scaling: &runpb.ServiceScaling{MaxInstanceCount: -1}}, "must not be negative"},
		{&runpb.Service{Scaling: &runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(-2)}}, "must not be negative"},
		{&runpb.Service{Scaling: &runpb.ServiceScaling{ManualInstanceCount: proto.Int32(2)}}, "only when scaling.scalingMode is MANUAL"},
		{&runpb.Service{Template: &runpb.RevisionTemplate{Scaling: &runpb.RevisionScaling{CpuUtilization: f(0.95)}}}, "between 0.1 and 0.90"},
		{&runpb.Service{Template: &runpb.RevisionTemplate{Scaling: &runpb.RevisionScaling{CpuUtilization: f(0.05)}}}, "between 0.1 and 0.90"},
		{&runpb.Service{Template: &runpb.RevisionTemplate{Scaling: &runpb.RevisionScaling{ConcurrencyUtilization: f(0.96)}}}, "between 0.1 and 0.95"},
		{&runpb.Service{Template: &runpb.RevisionTemplate{Scaling: &runpb.RevisionScaling{CpuUtilization: f(0), ConcurrencyUtilization: f(0)}}}, "cannot both be disabled"},
		{&runpb.Service{Template: &runpb.RevisionTemplate{Scaling: &runpb.RevisionScaling{MinInstanceCount: 3, MaxInstanceCount: 2}}}, "must not exceed"},
		{&runpb.Service{Template: &runpb.RevisionTemplate{Scaling: &runpb.RevisionScaling{CpuUtilization: f(0.6)}}}, ""}, // the CPU driver (#1800)
		{&runpb.Service{Template: &runpb.RevisionTemplate{Scaling: &runpb.RevisionScaling{ConcurrencyUtilization: f(0)}}}, "not supported locally"},
		{&runpb.Service{Scaling: &runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(0)}}, ""}, // disabled (#1800)
		{&runpb.Service{Template: &runpb.RevisionTemplate{Scaling: &runpb.RevisionScaling{CpuUtilization: f(0), ConcurrencyUtilization: f(0.95)}},
			Scaling: &runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(2)}}, ""},
	} {
		err := checkScaling(tc.svc)
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("checkScaling(%v) = %v, want %q", tc.svc, err, tc.want)
		}
	}
}

func TestServiceScalingRoundTrips(t *testing.T) {
	svc := &runpb.Service{Name: "projects/proj-1/locations/us-central1/services/s",
		Scaling: &runpb.ServiceScaling{MinInstanceCount: 2, MaxInstanceCount: 8},
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: "nginx:1"}},
			Scaling: &runpb.RevisionScaling{MaxInstanceCount: 4, ConcurrencyUtilization: proto.Float32(0.8), CpuUtilization: proto.Float32(0)}}}
	m, err := ToKnative(svc, "ns", "i", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{annServiceScaling, annTargetUtilization + `: "80"`, annMaxScale + `: "4"`} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %s:\n%s", want, m)
		}
	}
	ann := v1ServiceScalingAnnotations(svc)
	if ann[v1ServiceMinScale] != "2" || ann[v1ServiceMaxScale] != "8" {
		t.Errorf("v1 annotations = %v", ann)
	}
	back := &runpb.Service{}
	for k, v := range ann {
		if err := fromV1ServiceScaling(back, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if !proto.Equal(back.Scaling, svc.Scaling) {
		t.Errorf("v1 round trip = %v", back.Scaling)
	}
}
