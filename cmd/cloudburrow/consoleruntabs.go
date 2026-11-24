package main

// Cloud Run: a service is changed on its own tabs, as Google's is (#1852).
// "Configure settings across the Containers, Networking, Security, and
// Scaling tabs ... Click View diff & redeploy, then Deploy changes"
// (https://docs.cloud.google.com/run/docs/deploying). Each tab's form holds
// that tab's fields of the service's edit form; the Editor fills the fields
// a tab does not send from the serving revision, so a change on one tab
// leaves the others as they are.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	// runViewDiff and runDeployChanges are Google's labels.
	runViewDiff      = "View diff & redeploy"
	runDeployChanges = "Deploy changes"
	// runEditContainer heads the Containers tab's form, over its
	// Container(s), Variables & Secrets and Volumes tabs.
	runEditContainer = "Edit container"
)

// runTabOf is the service tab each edit-form field is on; a field not named
// is on Containers.
var runTabOf = map[string]string{
	"vpcMode": "networking", "vpcNetwork": "networking", "vpcSubnet": "networking", "vpcTags": "networking",
	"vpcConnector": "networking", "vpcEgress": "networking", "sessionAffinity": "networking",
	"serviceAccount": "security", "encryptionKey": "security",
	"minInstances": "scaling", "maxInstances": "scaling", "cpuUtilization": "scaling",
	"concurrencyUtilization": "scaling", "serviceScalingMode": "scaling",
	"serviceMinInstances": "scaling", "serviceMaxInstances": "scaling", "manualInstances": "scaling",
}

// runTabForm is the part of the service's edit form that is on tab, ending
// in View diff & redeploy, whose review ends in Deploy changes. Nil when the
// service cannot be edited here (editForm's reasons).
func runTabForm(edit *console.EditForm, tab, title string) *console.EditForm {
	if edit == nil {
		return nil
	}
	var fields []console.Field
	for _, f := range edit.Fields {
		if f.Immutable {
			continue
		}
		on := runTabOf[f.Name]
		if on == "" {
			on = "containers"
		}
		if on != tab {
			continue
		}
		if tab == "containers" {
			f.Section = runEditContainer
		} else {
			f.Section, f.Tab = "", ""
		}
		fields = append(fields, f)
	}
	if len(fields) == 0 {
		return nil
	}
	return &console.EditForm{Label: title, Title: title, Fields: fields, Submit: runViewDiff, Review: runDeployChanges,
		Note: "Deploying creates a new revision with these settings; the settings on the other tabs are kept."}
}

// runFieldProperties are a form's fields as the tab's current values.
func runFieldProperties(form *console.EditForm) []console.Property {
	if form == nil {
		return nil
	}
	var out []console.Property
	for _, f := range form.Fields {
		v := f.Default
		if f.Type == "map" && v != "" {
			var m map[string]string
			if json.Unmarshal([]byte(v), &m) == nil {
				keys := make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k+"="+m[k])
				}
				sort.Strings(keys)
				v = strings.Join(keys, ", ")
			}
		}
		// A setting the service does not hold is left out (parity §4.4).
		if strings.TrimSpace(v) == "" {
			continue
		}
		out = append(out, console.Property{Label: f.Label, Value: v})
	}
	return out
}

// runEditTabs are the Containers and Scaling tabs, and the edit forms of
// the Networking and Security tabs, from the service's edit form.
func runEditTabs(edit *console.EditForm, networking, security *console.Section) (containers, scaling console.Section) {
	why := "This service cannot be changed here: " +
		"the console edits a service with one container whose entrypoint and arguments have no spaces in them."
	tab := func(id, label string) console.Section {
		form := runTabForm(edit, id, label)
		sec := console.Section{ID: id, Label: label, Kind: console.KindProperties, Edit: form,
			Groups: []console.PropertyGroup{{Heading: "Current settings", Properties: runFieldProperties(form)}}}
		if form == nil {
			sec.Note = why
		}
		return sec
	}
	containers, scaling = tab("containers", "Containers"), tab("scaling", "Scaling")
	if form := runTabForm(edit, "networking", "Edit networking"); form != nil {
		networking.Edit = form
	}
	if form := runTabForm(edit, "security", "Edit security"); form != nil {
		security.Edit = form
	}
	return containers, scaling
}

// runFieldTab is the service tab a field of the edit form is on.
func runFieldTab(name string) string {
	if t := runTabOf[name]; t != "" {
		return t
	}
	return "containers"
}

// withTabDefaults is a tab's values with every edit-form field of the tabs
// it did not send filled in from the form, as the service holds it now, so
// a change on one tab keeps the others' settings. A field of a tab that was
// sent is left as sent: one the tab left out (a scaling mode's inactive
// count) stays out. The secret-backed variables are never filled in: a
// request without them keeps them as they are (Edit's keepSecrets).
func withTabDefaults(values map[string]string, edit *console.EditForm) map[string]string {
	out := make(map[string]string, len(values))
	sent := map[string]bool{}
	for k, v := range values {
		out[k] = v
		sent[runFieldTab(k)] = true
	}
	if edit == nil {
		return out
	}
	for _, f := range edit.Fields {
		if f.Immutable || f.Name == "secretEnv" || sent[runFieldTab(f.Name)] {
			continue
		}
		out[f.Name] = f.Default
	}
	return out
}

// tabDefaults reads the service's edit form for withTabDefaults; nil when
// the service cannot be read.
func (p runProvider) tabDefaults(ctx context.Context, project, service string) *console.EditForm {
	if p.kubeconfig == "" {
		// No cluster to read the service's revisions from.
		return nil
	}
	svc, err := p.service(ctx, project, service)
	if err != nil || svc == nil {
		return nil
	}
	return p.editForm(ctx, service, svc)
}
