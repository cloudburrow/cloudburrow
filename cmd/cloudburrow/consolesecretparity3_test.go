package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
)

// TestConsoleSecretParityRound3 (#1850): the Secret type on Create secret
// and the Overview; the Versions tab's checkbox selection with Disable,
// Enable and Destroy confirmed as Google's dialogs are ("Disable selected
// versions", "Enable selected versions", and Destroy asking for the secret
// ID); View secret value as a dialog from the row; Edit secret as a page
// ending in Update secret; and Create regional secret's Region or
// Multi-region.
func TestConsoleSecretParityRound3(t *testing.T) {
	p := secretsFixture(t)
	ctx := context.Background()
	st := p.svc.Store()

	name, err := p.Create(ctx, "demo", map[string]string{"secretId": "typed", "payload": "one",
		"secretType": "ACCESS_KEY", "setRotation": "false", "setExpiration": "false", "setDestroyDelay": "false"})
	if err != nil {
		t.Fatal(err)
	}
	if sec, _ := st.GetSecret("demo", "typed"); sec.SecretType != "ACCESS_KEY" {
		t.Errorf("secret type after create = %q", sec.SecretType)
	}
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "badtype", "payload": "x", "secretType": "NOPE"}); err == nil {
		t.Error("an unknown secret type was accepted")
	}
	if _, err := st.GetSecret("demo", "badtype"); err == nil {
		t.Error("a refused secret type left the secret behind")
	}
	for _, v := range []string{"two", "three"} {
		if err := p.ActAt(ctx, "demo", []string{name}, actSecretAddVersion, map[string]string{"payload": v}); err != nil {
			t.Fatal(err)
		}
	}

	d, err := p.Detail(ctx, "demo", []string{name})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detailText(d), "Access key") {
		t.Error("the Overview does not show the Secret type")
	}
	versions := d.Sections[1].Listing
	var sel []string
	for _, a := range versions.SelectActions {
		sel = append(sel, a.Label+"/"+a.Submit)
		if a.SelectionField != secretVersionsField {
			t.Errorf("%s has selection field %q", a.Label, a.SelectionField)
		}
	}
	if got := strings.Join(sel, ","); got != "Disable/Disable selected versions,Enable/Enable selected versions,Destroy/Destroy selected versions" {
		t.Errorf("Versions tab selection actions = %s", got)
	}
	destroy := versions.SelectActions[2]
	if destroy.ConfirmWord != "typed" || !destroy.Destructive || destroy.ConfirmStyle != "" {
		t.Errorf("Destroy asks for %q (style %q); want the secret ID typed back", destroy.ConfirmWord, destroy.ConfirmStyle)
	}
	if versions.SelectActions[0].ConfirmStyle != console.DeleteConfirmClick {
		t.Error("Disable selected versions asks for something typed")
	}
	for _, it := range versions.Items {
		if !it.Selectable {
			t.Errorf("version %s cannot be checked", it.Name)
		}
	}
	top := versions.Items[0]
	if top.Actions[0].Label != "View secret value" || !top.Actions[0].Reveal || top.Actions[0].Href != "" {
		t.Errorf("version 3's first action = %+v; want View secret value shown in a dialog", top.Actions[0])
	}
	if top.Actions[1].Submit != "Disable selected versions" || top.Actions[2].ConfirmWord != "typed" {
		t.Errorf("a row's Disable and Destroy are not confirmed as Google's: %+v", top.Actions[1:])
	}
	// The page offers the selection actions, so the server takes them.
	offered := map[string]bool{}
	for _, a := range p.DetailActions(ctx, "demo", []string{name}) {
		offered[a.ID] = true
	}
	for _, id := range []string{"disableversions", "enableversions", "destroyversions"} {
		if !offered[id] {
			t.Errorf("the secret's page does not offer %s", id)
		}
	}

	// Disable versions 1 and 2 at once, enable 1 again, destroy 2.
	if err := p.ActAt(ctx, "demo", []string{name}, "disableversions", map[string]string{secretVersionsField: "1\n2"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"1", "2"} {
		if v, _ := st.GetVersion("demo", "typed", n); v.State != secrets.StateDisabled {
			t.Errorf("version %s is %s after Disable selected versions", n, v.State)
		}
	}
	if err := p.ActAt(ctx, "demo", []string{name}, "enableversions", map[string]string{secretVersionsField: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "demo", []string{name}, "destroyversions", map[string]string{secretVersionsField: "2"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.GetVersion("demo", "typed", "1"); v.State != secrets.StateEnabled {
		t.Errorf("version 1 is %s after Enable selected versions", v.State)
	}
	if v, _ := st.GetVersion("demo", "typed", "2"); v.State != secrets.StateDestroyed {
		t.Errorf("version 2 is %s after Destroy selected versions", v.State)
	}
	if err := p.ActAt(ctx, "demo", []string{name}, "enableversions", map[string]string{secretVersionsField: "2"}); err == nil ||
		!strings.Contains(err.Error(), "version 2") {
		t.Errorf("enabling a destroyed version = %v; want a refusal naming it", err)
	}
	if err := p.ActAt(ctx, "demo", []string{name}, "disableversions", map[string]string{secretVersionsField: ""}); err == nil {
		t.Error("an empty selection was accepted")
	}

	// Edit secret is a page ending in Update secret, its Starting on a
	// date and time picker.
	if d.Edit == nil || d.Edit.Layout != console.EditLayoutPage || d.Edit.Submit != "Update secret" {
		t.Errorf("Edit secret = %+v; want a page ending in Update secret", d.Edit)
	}
	for _, f := range d.Edit.Fields {
		if f.Name == "nextRotationTime" && (f.Type != console.DateTimeFieldType || f.Label != "Starting on") {
			t.Errorf("Starting on is %s %q", f.Type, f.Label)
		}
	}

	// Create regional secret: a multi-region.
	r := secretsRegionalProvider{p}
	made, err := r.Create(ctx, "demo", map[string]string{"secretId": "multi", "payload": "x",
		"locationType": secretLocationMulti, "multiRegion": "eu", "location": "", "secretType": "CLOUD_SQL_DB_CREDENTIALS"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(made, "/locations/eu/") {
		t.Errorf("multi-region secret is %s", made)
	}
	if sec, err := st.GetSecret(secrets.RegionalScope("demo", "eu"), "multi"); err != nil || sec.SecretType != "CLOUD_SQL_DB_CREDENTIALS" {
		t.Errorf("regional secret type = %q, %v", sec.SecretType, err)
	}
}
