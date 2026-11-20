package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// tabsTestForm is the edit form of a one-container service.
func tabsTestForm(t *testing.T) *console.EditForm {
	t.Helper()
	var rev knTemplate
	if err := json.Unmarshal([]byte(`{
		"metadata": {"name": "api-00001", "annotations": {
			"autoscaling.knative.dev/min-scale": "1", "autoscaling.knative.dev/max-scale": "10"}},
		"spec": {"containerConcurrency": 80, "timeoutSeconds": 600, "containers": [{
			"image": "example.com/api:v1", "ports": [{"containerPort": 8080}],
			"resources": {"limits": {"cpu": "1", "memory": "512Mi"}},
			"env": [{"name": "MODE", "value": "live"}]}]}}`), &rev); err != nil {
		t.Fatal(err)
	}
	form := runEditForm("api", rev, "Prefilled from the serving revision, api-00001.")
	if form == nil {
		t.Fatal("no edit form")
	}
	return form
}

// TestRunServiceTabsEditTheirOwnFields (#1852): Google's service page is
// changed on its Containers, Networking, Security and Scaling tabs, each
// ending in View diff & redeploy, whose review ends in Deploy changes.
// Every field of the edit form but the name is on exactly one tab.
func TestRunServiceTabsEditTheirOwnFields(t *testing.T) {
	form := tabsTestForm(t)
	networking := console.Section{ID: "networking"}
	security := console.Section{ID: "security"}
	containers, scaling := runEditTabs(form, &networking, &security)
	if containers.ID != "containers" || containers.Label != "Containers" || scaling.ID != "scaling" || scaling.Label != "Scaling" {
		t.Fatalf("tabs %q/%q and %q/%q", containers.ID, containers.Label, scaling.ID, scaling.Label)
	}
	seen := map[string]string{}
	for tab, f := range map[string]*console.EditForm{
		"containers": containers.Edit, "scaling": scaling.Edit, "networking": networking.Edit, "security": security.Edit,
	} {
		if f == nil {
			t.Fatalf("the %s tab has no form", tab)
		}
		if f.Submit != "View diff & redeploy" || f.Review != "Deploy changes" {
			t.Errorf("the %s tab ends in %q then %q; want View diff & redeploy, then Deploy changes", tab, f.Submit, f.Review)
		}
		for _, field := range f.Fields {
			if was, dup := seen[field.Name]; dup {
				t.Errorf("%s is on %s and %s", field.Name, was, tab)
			}
			seen[field.Name] = tab
		}
	}
	for _, f := range form.Fields {
		if f.Immutable {
			if _, ok := seen[f.Name]; ok {
				t.Errorf("the immutable %s is on a tab", f.Name)
			}
			continue
		}
		if seen[f.Name] == "" {
			t.Errorf("%s is on no tab", f.Name)
		}
	}
	for name, tab := range map[string]string{"image": "containers", "env": "containers", "minInstances": "scaling",
		"serviceScalingMode": "scaling", "vpcConnector": "networking", "serviceAccount": "security"} {
		if seen[name] != tab {
			t.Errorf("%s is on %q, want %s", name, seen[name], tab)
		}
	}
	// Each tab shows what it holds now.
	var current []string
	for _, p := range containers.Groups[0].Properties {
		current = append(current, p.Label+"="+p.Value)
	}
	if got := strings.Join(current, "|"); !strings.Contains(got, "example.com/api:v1") || !strings.Contains(got, "MODE=live") {
		t.Errorf("the Containers tab's current settings are %s", got)
	}

	// A service the console cannot edit says why, and offers no form.
	networking, security = console.Section{}, console.Section{}
	containers, scaling = runEditTabs(nil, &networking, &security)
	if containers.Edit != nil || scaling.Edit != nil || networking.Edit != nil || containers.Note == "" {
		t.Errorf("an uneditable service's tabs are %+v, %+v", containers, scaling)
	}
}

// TestRunTabValuesKeepTheOtherTabs (#1852): a tab sends its own fields;
// the others' are filled in as the service holds them, and a field its own
// tab left out stays out, as do the secret-backed variables.
func TestRunTabValuesKeepTheOtherTabs(t *testing.T) {
	form := tabsTestForm(t)
	got := withTabDefaults(map[string]string{"minInstances": "2"}, form)
	if got["minInstances"] != "2" || got["image"] != "example.com/api:v1" || got["port"] != "8080" {
		t.Errorf("the Scaling tab's values became %v", got)
	}
	if _, ok := got["maxInstances"]; ok {
		t.Errorf("a Scaling field the tab did not send was filled in: %v", got)
	}
	if _, ok := got["secretEnv"]; ok {
		t.Error("the secret-backed variables were filled in, so they would no longer be kept")
	}
	if _, ok := got["name"]; ok {
		t.Error("the immutable name was filled in")
	}
	// The whole form, as Edit and deploy new revision sends it, is left alone.
	all := editValues(map[string]string{"MODE": "x"})
	if got := withTabDefaults(all, form); len(got) != len(all)+countOtherTabFields(form, all) {
		t.Errorf("the whole form gained %d values", len(got)-len(all))
	}
}

// countOtherTabFields is how many of form's fillable fields are on a tab
// none of values' keys is on.
func countOtherTabFields(form *console.EditForm, values map[string]string) int {
	sent := map[string]bool{}
	for k := range values {
		sent[runFieldTab(k)] = true
	}
	n := 0
	for _, f := range form.Fields {
		if _, ok := values[f.Name]; !ok && !f.Immutable && f.Name != "secretEnv" && !sent[runFieldTab(f.Name)] {
			n++
		}
	}
	return n
}

// TestRunServicesListCopiesTheCheckedService (#1852): the Services list's
// rows have checkboxes, and Copy opens the service copy page for the one
// checked: the Create service page holding its settings, under a new name.
func TestRunServicesListCopiesTheCheckedService(t *testing.T) {
	p := runProvider{runAddr: fixedAddr("127.0.0.1:1"), defaultProject: "demo", region: "us-central1"}
	acts := p.ListSelectActions(context.Background(), "demo")
	if len(acts) != 1 || acts[0].ID != runCopySelected || acts[0].Label != "Copy" || acts[0].Href != "/run/create" ||
		acts[0].SelectionField != "from" {
		t.Fatalf("the Services list's selection actions are %+v; want Copy, opening /run/create?from=", acts)
	}
	if _, err := p.CreateFormFrom(context.Background(), "demo", "a,b"); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("a copy of two services = %v; want it refused", err)
	}
	if acts := (runProvider{}).ListSelectActions(context.Background(), "demo"); acts != nil {
		t.Errorf("with no adapter, the list offers %+v", acts)
	}

	fields := runCopyFields(p, "api", tabsTestForm(t))
	got := map[string]string{}
	for _, f := range fields {
		got[f.Name] = f.Default
	}
	if got["name"] != "api-copy" || got["image"] != "example.com/api:v1" || got["port"] != "8080" ||
		got["maxInstances"] != "10" || got["region"] != "us-central1" {
		t.Errorf("the copy page's fields hold %v; want the service's settings under api-copy", got)
	}
	if _, create := p.CreateForm(); len(fields) != len(create) {
		t.Errorf("the copy page has %d fields, Create service %d", len(fields), len(create))
	}
}
