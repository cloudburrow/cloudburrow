package run

import (
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Splits must add up to 100%, name a revision when by revision and none
// when to the latest, and name no revision twice (#1387).
func TestInstanceSplitsAreChecked(t *testing.T) {
	rev := func(r string, p int32) *runpb.InstanceSplit {
		return &runpb.InstanceSplit{Type: splitByRev, Revision: r, Percent: p}
	}
	latest := func(p int32) *runpb.InstanceSplit { return &runpb.InstanceSplit{Type: splitLatest, Percent: p} }
	for name, splits := range map[string][]*runpb.InstanceSplit{
		"sum":            {latest(50), rev("a", 40)},
		"negative":       {latest(110), rev("a", -10)},
		"no revision":    {latest(50), {Type: splitByRev, Percent: 50}},
		"latest named":   {{Type: splitLatest, Revision: "a", Percent: 100}},
		"twice":          {rev("a", 50), rev("a", 50)},
		"latest twice":   {latest(50), latest(50)},
		"no type at all": {{Percent: 100}},
	} {
		if err := checkInstanceSplits(splits); apierror.From(err).Code != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", name, err)
		}
	}
	if err := checkInstanceSplits([]*runpb.InstanceSplit{latest(60), rev("p/revisions/a", 30), {Revision: "b", Percent: 10}}); err != nil {
		t.Errorf("a valid split is refused: %v", err)
	}
}

// Each revision's instances are its percentage of the count, rounded by
// largest remainder so they add up to it, the latest first.
func TestSplitSharesAddUpToTheInstanceCount(t *testing.T) {
	splits := []*runpb.InstanceSplit{{Type: splitLatest, Percent: 50}, {Type: splitByRev, Revision: "old", Percent: 25},
		{Type: splitByRev, Revision: "older", Percent: 25}}
	for total, want := range map[int32][]int32{4: {2, 1, 1}, 3: {1, 1, 1}, 1: {1, 0, 0}, 0: {0, 0, 0}, 10: {5, 3, 2}} {
		got := splitShares(splits, "new", total)
		var counts []int32
		for _, sh := range got {
			counts = append(counts, sh.count)
		}
		if len(counts) != 3 || counts[0] != want[0] || counts[1] != want[1] || counts[2] != want[2] {
			t.Errorf("%d instances: %v, want %v", total, counts, want)
		}
	}
	// The latest named by ID adds to its share.
	if got := splitShares([]*runpb.InstanceSplit{{Type: splitLatest, Percent: 50}, {Type: splitByRev, Revision: "new", Percent: 50}}, "new", 4); len(got) != 1 || got[0].count != 4 {
		t.Errorf("latest by ID = %+v", got)
	}
	if got := splitShares(nil, "new", 3); len(got) != 1 || got[0].count != 3 || got[0].percent != 100 {
		t.Errorf("no splits = %+v", got)
	}
}

// A split revision's Deployment is its own, labelled as the pool's split
// on itself, its selector and its pods, and the pool's Deployment runs only
// the latest revision's share.
func TestSplitRevisionRendersItsOwnDeployment(t *testing.T) {
	s := NewServer(&Knative{}, "inst", time.Second)
	w := workload{kind: kindWorkerPool, id: "wp", name: "projects/p/locations/us-central1/workerPools/wp"}
	tmpl := &runpb.WorkerPoolRevisionTemplate{Containers: []*runpb.Container{{Image: "example.com/img:v1"}}}
	dep, err := s.deployment(w, podSpec{containers: tmpl.GetContainers(), revision: "wp-00001-abc",
		revisionName: w.name + "/revisions/wp-00001-abc", revisionSpec: tmpl, generation: 1,
		deploymentName: splitDeploymentName(w, "wp-00001-abc")}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(dep, labelSplitOf+": worker-pool-wp") != 3 || !strings.Contains(dep, "name: run-worker-pool-wp-wp-00001-abc") ||
		!strings.Contains(dep, "replicas: 2") {
		t.Errorf("split Deployment:\n%s", dep)
	}
	spec := workerPoolSpec(&runpb.WorkerPool{Name: w.name, Template: tmpl, Scaling: &runpb.WorkerPoolScaling{},
		InstanceSplits: []*runpb.InstanceSplit{{Type: splitLatest, Percent: 75}, {Type: splitByRev, Revision: "wp-00001-abc", Percent: 25}}},
		2, w.name+"/revisions/wp-00002-def")
	n := int32(4)
	spec.Scaling.ManualInstanceCount = &n
	_, main, err := s.renderWorkerPool(w, spec, 2, "")
	if err != nil || !strings.Contains(main, "replicas: 3") || strings.Contains(main, labelSplitOf) {
		t.Errorf("the pool's Deployment (%v):\n%s", err, main)
	}
	serving := servingRevisions(spec)
	if !serving[w.name+"/revisions/wp-00001-abc"] || !serving[w.name+"/revisions/wp-00002-def"] || len(serving) != 2 {
		t.Errorf("serving = %v", serving)
	}
	st := splitStatuses(spec.GetInstanceSplits())
	if len(st) != 2 || st[1].GetRevision() != "wp-00001-abc" || st[1].GetPercent() != 25 {
		t.Errorf("statuses = %v", st)
	}
}

// An update_mask names the fields that change (AIP-134); an output-only
// or unknown path is INVALID_ARGUMENT.
func TestMergeWorkerPoolMask(t *testing.T) {
	two, five := int32(2), int32(5)
	current := &runpb.WorkerPool{Name: "n", Description: "old", Labels: map[string]string{"a": "1"},
		Scaling:  &runpb.WorkerPoolScaling{ManualInstanceCount: &two},
		Template: &runpb.WorkerPoolRevisionTemplate{Containers: []*runpb.Container{{Image: "example.com/img:v1"}}}}
	in := &runpb.WorkerPool{Name: "n", Description: "new", Scaling: &runpb.WorkerPoolScaling{ManualInstanceCount: &five}}
	got, err := mergeWorkerPoolMask(current, in, []string{"scaling.manual_instance_count"})
	if err != nil || got.GetScaling().GetManualInstanceCount() != 5 || got.GetDescription() != "old" ||
		got.GetTemplate().GetContainers()[0].GetImage() != "example.com/img:v1" || got.GetLabels()["a"] != "1" {
		t.Errorf("merge = %v, %v", got, err)
	}
	got, err = mergeWorkerPoolMask(current, in, []string{"description", "labels"})
	if err != nil || got.GetDescription() != "new" || len(got.GetLabels()) != 0 || got.GetScaling().GetManualInstanceCount() != 2 {
		t.Errorf("merge description, labels = %v, %v", got, err)
	}
	for _, bad := range []string{"etag", "instance_split_statuses", "no_such_field"} {
		if _, err := mergeWorkerPoolMask(current, in, []string{bad}); apierror.From(err).Code != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", bad, err)
		}
	}
}
