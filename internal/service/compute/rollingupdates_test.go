package compute

import (
	"sort"
	"strings"
	"testing"
	"time"
)

const rollMIG = "projects/p2139/zones/us-central1-a/instanceGroupManagers/"

// rollFixture is newScaleFixture with the templates web2 (another machine
// type) and web-md (web's properties but its metadata), and the zonal
// group name of size VMs made from web.
func rollFixture(t *testing.T, name string, size int) *scaleFixture {
	t.Helper()
	f := newScaleFixture(t)
	disks := []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}}
	f.call("POST", "global/instanceTemplates", nil, obj{"name": "web2", "properties": obj{"machineType": "e2-medium", "disks": disks}})
	f.call("POST", "global/instanceTemplates", nil, obj{"name": "web-md", "properties": obj{"machineType": "e2-small", "disks": disks,
		"metadata": obj{"items": []any{map[string]any{"key": "release", "value": "2"}}}}})
	f.call("POST", "zones/us-central1-a/instanceGroupManagers", nil, obj{"name": name, "instanceTemplate": "global/instanceTemplates/web", "targetSize": float64(size)})
	return f
}

// templates are the group's live VMs' templates, by VM name.
func (f *scaleFixture) templates(mig string) map[string]string {
	f.t.Helper()
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	out := map[string]string{}
	for _, vm := range f.s.liveManagedLocked(rollMIG + mig) {
		out[lastSeg(vm)] = lastSeg(instanceTemplateOf(f.s.res[vm]))
	}
	return out
}

// roll steps the update until the group has size VMs all on want (or a
// count of each template, for a canary), checking no step goes beyond
// size+surge VMs; the number of steps.
func (f *scaleFixture) roll(mig string, size, surge int, done func(map[string]string) bool) int {
	f.t.Helper()
	for i := 1; i <= 40; i++ {
		f.s.rollOnce()
		waitIdle(f.t, f.s)
		got := f.templates(mig)
		if len(got) > size+surge {
			f.t.Fatalf("step %d: %d VMs, more than %d + maxSurge %d", i, len(got), size, surge)
		}
		if len(got) == size && done(got) {
			return i
		}
	}
	f.t.Fatalf("the update never finished: %v", f.templates(mig))
	return 0
}

func allOn(tpl string) func(map[string]string) bool {
	return func(m map[string]string) bool {
		for _, v := range m {
			if v != tpl {
				return false
			}
		}
		return true
	}
}

func names(m map[string]string) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// TestProactiveRollingUpdate (#2139): patch with a new version and an
// Automatic (PROACTIVE) policy replaces every VM made from another template
// with new ones, within maxSurge 1 and maxUnavailable 1, the group's
// status reaching its version target; the operation is a patch.
func TestProactiveRollingUpdate(t *testing.T) {
	f := rollFixture(t, "web", 4)
	before := f.templates("web")
	op := f.call("PATCH", "zones/us-central1-a/instanceGroupManagers/web", nil, obj{
		"versions":     []any{map[string]any{"instanceTemplate": "global/instanceTemplates/web2"}},
		"updatePolicy": obj{"type": "PROACTIVE", "maxSurge": obj{"fixed": 1.0}, "maxUnavailable": obj{"fixed": 1.0}}})
	if op["operationType"] != "patch" {
		t.Fatalf("operation = %v", op)
	}
	m := f.call("GET", "zones/us-central1-a/instanceGroupManagers/web", nil, nil)
	if strings.Contains(m["instanceTemplate"].(string), "/web2") == false {
		t.Fatalf("instanceTemplate = %v", m["instanceTemplate"])
	}
	up := m["updatePolicy"].(map[string]any)
	if up["minimalAction"] != "REPLACE" || up["replacementMethod"] != "SUBSTITUTE" || up["maxSurge"].(map[string]any)["calculated"] != 1.0 {
		t.Fatalf("updatePolicy = %v", up)
	}
	steps := f.roll("web", 4, 1, allOn("web2"))
	if steps < 2 {
		t.Errorf("4 VMs replaced in %d steps within maxSurge 1, maxUnavailable 1", steps)
	}
	after := f.templates("web")
	for n := range before {
		if _, kept := after[n]; kept {
			t.Errorf("SUBSTITUTE kept the name %s: %s", n, names(after))
		}
	}
	m = f.call("GET", "zones/us-central1-a/instanceGroupManagers/web", nil, nil)
	if st := m["status"].(map[string]any); st["isStable"] != true || st["versionTarget"].(map[string]any)["isReached"] != true {
		t.Errorf("status after the update = %v", st)
	}
}

// TestRollingUpdateRecreateAndRefresh (#2139): RECREATE keeps the VMs'
// names (maxSurge 0); a template differing in metadata only is applied by
// REFRESH in place, every VM at once, when minimalAction allows it.
func TestRollingUpdateRecreateAndRefresh(t *testing.T) {
	f := rollFixture(t, "web", 3)
	before := names(f.templates("web"))
	f.call("PATCH", "zones/us-central1-a/instanceGroupManagers/web", nil, obj{
		"versions":     []any{map[string]any{"instanceTemplate": "global/instanceTemplates/web2"}},
		"updatePolicy": obj{"type": "PROACTIVE", "replacementMethod": "RECREATE", "maxSurge": obj{"fixed": 0.0}, "maxUnavailable": obj{"fixed": 2.0}}})
	f.roll("web", 3, 0, allOn("web2"))
	if got := names(f.templates("web")); got != before {
		t.Errorf("RECREATE renamed the VMs: %s, were %s", got, before)
	}

	g := rollFixture(t, "md", 3)
	before = names(g.templates("md"))
	g.call("PATCH", "zones/us-central1-a/instanceGroupManagers/md", nil, obj{
		"versions":     []any{map[string]any{"instanceTemplate": "global/instanceTemplates/web-md"}},
		"updatePolicy": obj{"type": "PROACTIVE", "minimalAction": "REFRESH"}})
	if got := g.templates("md"); !allOn("web-md")(got) || names(got) != before {
		t.Fatalf("REFRESH = %v, was %s", got, before)
	}
	g.s.mu.Lock()
	for _, vm := range g.s.liveManagedLocked(rollMIG + "md") {
		if !strings.Contains(jsonString(g.s.res[vm]["metadata"]), `"release"`) {
			t.Errorf("%s's metadata = %v", vm, g.s.res[vm]["metadata"])
		}
	}
	g.s.mu.Unlock()
}

// TestCanaryAndSelectiveUpdates (#2139): two versions, the canary of fixed
// size 1, the rest on the other; Selective (OPPORTUNISTIC) leaves the VMs
// as they are until applyUpdatesToInstances names them, which keeps their
// names.
func TestCanaryAndSelectiveUpdates(t *testing.T) {
	f := rollFixture(t, "web", 3)
	f.call("PATCH", "zones/us-central1-a/instanceGroupManagers/web", nil, obj{
		"versions": []any{map[string]any{"instanceTemplate": "global/instanceTemplates/web", "name": "stable"},
			map[string]any{"instanceTemplate": "global/instanceTemplates/web2", "name": "canary", "targetSize": map[string]any{"fixed": 1.0}}},
		"updatePolicy": obj{"type": "PROACTIVE"}})
	f.roll("web", 3, 1, func(m map[string]string) bool {
		n := 0
		for _, v := range m {
			if v == "web2" {
				n++
			}
		}
		return n == 1
	})

	g := rollFixture(t, "sel", 2)
	g.call("PATCH", "zones/us-central1-a/instanceGroupManagers/sel", nil, obj{
		"versions":     []any{map[string]any{"instanceTemplate": "global/instanceTemplates/web2"}},
		"updatePolicy": obj{"type": "OPPORTUNISTIC"}})
	g.s.rollOnce()
	waitIdle(t, g.s)
	got := g.templates("sel")
	if !allOn("web")(got) {
		t.Fatalf("OPPORTUNISTIC changed VMs: %v", got)
	}
	m := g.call("GET", "zones/us-central1-a/instanceGroupManagers/sel", nil, nil)
	if m["status"].(map[string]any)["versionTarget"].(map[string]any)["isReached"] != false {
		t.Errorf("version target reached with VMs on the old template: %v", m["status"])
	}
	var one string
	for n := range got {
		one = n
	}
	g.call("POST", "zones/us-central1-a/instanceGroupManagers/sel/applyUpdatesToInstances", nil, obj{
		"instances": []any{"zones/us-central1-a/instances/" + one}, "minimalAction": "REPLACE"})
	// The VM is deleted, then made again by its name once the delete is
	// done.
	var after map[string]string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		waitIdle(t, g.s)
		if after = g.templates("sel"); after[one] == "web2" && len(after) == 2 {
			break
		}
	}
	if after[one] != "web2" || len(after) != 2 {
		t.Errorf("after applyUpdatesToInstances: %v (applied to %s)", after, one)
	}
}

// TestPatchManagerRefusals (#2139): Google's rules on versions and
// updatePolicy.
func TestPatchManagerRefusals(t *testing.T) {
	f := rollFixture(t, "web", 2)
	path := "zones/us-central1-a/instanceGroupManagers/web"
	tpl := func(n string) map[string]any {
		return map[string]any{"instanceTemplate": "global/instanceTemplates/" + n}
	}
	for _, c := range []struct {
		name string
		body obj
		want string
	}{
		{"RECREATE with surge", obj{"updatePolicy": obj{"type": "PROACTIVE", "replacementMethod": "RECREATE", "maxSurge": obj{"fixed": 1.0}}}, "maxSurge must be 0"},
		{"both zero", obj{"updatePolicy": obj{"type": "PROACTIVE", "maxSurge": obj{"fixed": 0.0}, "maxUnavailable": obj{"fixed": 0.0}}}, "cannot both be 0"},
		{"minimal above most", obj{"updatePolicy": obj{"minimalAction": "REPLACE", "mostDisruptiveAllowedAction": "RESTART"}}, "cannot be more disruptive"},
		{"bad type", obj{"updatePolicy": obj{"type": "ROLLING"}}, "Must be one of"},
		{"fixed and percent", obj{"updatePolicy": obj{"maxSurge": obj{"fixed": 1.0, "percent": 10.0}}}, "only one of"},
		{"three versions", obj{"versions": []any{tpl("web"), tpl("web2"), tpl("web-md")}}, "one or two versions"},
		{"two unsized", obj{"versions": []any{tpl("web"), tpl("web2")}}, "exactly one version"},
		{"missing template", obj{"versions": []any{tpl("nope")}}, "was not found"},
		{"autohealing health check not found", obj{"autoHealingPolicies": []any{map[string]any{"healthCheck": "x"}}}, "was not found"},
		{"stale fingerprint", obj{"fingerprint": "AAAA", "targetSize": 3.0}, "fingerprint"},
	} {
		if _, err := f.s.Call("PATCH", "p2139", path, nil, c.body); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	// A regional group's fixed values are 0 or at least its zones.
	f.call("POST", "regions/us-central1/instanceGroupManagers", nil, obj{"name": "reg", "instanceTemplate": "global/instanceTemplates/web", "targetSize": 3.0})
	if _, err := f.s.Call("PATCH", "p2139", "regions/us-central1/instanceGroupManagers/reg", nil, obj{
		"updatePolicy": obj{"type": "PROACTIVE", "maxSurge": obj{"fixed": 1.0}}}); err == nil || !strings.Contains(err.Error(), "at least its number of zones") {
		t.Errorf("regional maxSurge 1: %v", err)
	}
	m := f.call("PATCH", "regions/us-central1/instanceGroupManagers/reg", nil, obj{"updatePolicy": obj{"type": "PROACTIVE"}})
	_ = m
	r := f.call("GET", "regions/us-central1/instanceGroupManagers/reg", nil, nil)
	if up := r["updatePolicy"].(map[string]any); up["maxSurge"].(map[string]any)["fixed"] != 3.0 || up["maxUnavailable"].(map[string]any)["fixed"] != 3.0 {
		t.Errorf("a regional group's defaults = %v", up)
	}
}
