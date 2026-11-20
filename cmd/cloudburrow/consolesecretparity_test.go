package main

import (
	"context"
	"strings"
	"testing"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleSecretParity (#1850): the list's Google columns and row
// Actions, Create secret's sections and version aliases, the secret's
// Overview, Versions, Permissions and Logs tabs, each version row's
// Actions, Add new version's Disable all past versions, and Edit secret's
// unchecked Set rotation period and Set expiration date clearing them.
func TestConsoleSecretParity(t *testing.T) {
	p := secretsFixture(t)
	ctx := context.Background()
	st := p.svc.Store()

	title, fields := p.CreateForm()
	if title != "Create secret" {
		t.Errorf("create title %q", title)
	}
	var sections []string
	seen := map[string]bool{}
	for _, f := range fields {
		if f.Section != "" && !seen[f.Section] {
			seen[f.Section] = true
			sections = append(sections, f.Section)
		}
	}
	want := "Replication policy,Encryption,Rotation,Notifications,Expiration,Labels,Annotations,Version aliases,Delayed destruction,Tags"
	if got := strings.Join(sections, ","); got != want {
		t.Errorf("Create secret sections = %s\nwant %s", got, want)
	}
	if fields[1].Name != "payload" || fields[1].Upload != "Upload file" {
		t.Errorf("the second field is %+v; want the Secret value with Upload file", fields[1])
	}

	name, err := p.Create(ctx, "demo", map[string]string{"secretId": "s", "payload": "one",
		"replication": replicationAutomatic, "encryption": secretEncryptionGoogle,
		"setRotation": "false", "setExpiration": "false", "setDestroyDelay": "false",
		"labels": `{"env":"prod"}`, "versionAliases": `{"stable":"1"}`})
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := st.GetSecret("demo", "s")
	if sec.VersionAliases["stable"] != 1 {
		t.Errorf("aliases after create = %v", sec.VersionAliases)
	}
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "bad", "payload": "x",
		"versionAliases": `{"latest":"1"}`}); err == nil {
		t.Error("an alias named latest was accepted")
	}
	if _, err := st.GetSecret("demo", "bad"); err == nil {
		t.Error("a refused alias left the secret behind")
	}

	l, err := p.List(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(l.Columns, ","); got != "Location,Encryption,Labels,Created,Expiration" {
		t.Errorf("columns %s", got)
	}
	row := l.Items[0]
	if row.Fields["Labels"] != "env: prod" || row.Fields["Encryption"] != "Google-managed" || len(row.Actions) != 1 ||
		row.Actions[0].Label != "Add new version" {
		t.Errorf("row %+v", row)
	}
	if p.RowEditLabel() != "Edit" {
		t.Error("no Edit in the row menu")
	}

	// Add new version from the row, disabling the past ones.
	if err := p.ActAt(ctx, "demo", []string{name}, actSecretAddVersion, map[string]string{"payload": "two", "disablePast": "true"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.GetVersion("demo", "s", "1"); v.State != secrets.StateDisabled {
		t.Errorf("version 1 is %s after Disable all past versions", v.State)
	}

	d, err := p.Detail(ctx, "demo", []string{name})
	if err != nil {
		t.Fatal(err)
	}
	var tabs []string
	for _, s := range d.Sections {
		tabs = append(tabs, s.ID)
	}
	if got := strings.Join(tabs, ","); got != "overview,versions,permissions,logs" {
		t.Errorf("tabs %s", got)
	}
	if !strings.Contains(detailText(d), "stable") {
		t.Error("the Overview does not show the alias")
	}
	versions := d.Sections[1].Listing
	labels := func(r console.Resource) string {
		var out []string
		for _, a := range r.Actions {
			out = append(out, a.Label)
		}
		return strings.Join(out, ",")
	}
	if got := labels(versions.Items[0]); got != "View secret value,Disable,Destroy" {
		t.Errorf("version 2's Actions: %s", got)
	}
	if got := labels(versions.Items[1]); got != "Enable,Destroy" {
		t.Errorf("version 1's Actions: %s", got)
	}
	if versions.Items[1].Fields["Aliases"] != "stable" {
		t.Errorf("version 1's aliases %q", versions.Items[1].Fields["Aliases"])
	}

	// Edit: rotation and expiry set, then unchecked; aliases moved.
	edit := map[string]string{"setRotation": "false", "setExpiration": "true", "ttl": "48h",
		"setDestroyDelay": "false", "versionAliases": `{"stable":"2"}`, "labels": "", "annotations": "", "topics": ""}
	if err := p.Edit(ctx, "demo", []string{name}, edit); err != nil {
		t.Fatal(err)
	}
	sec, _ = st.GetSecret("demo", "s")
	if sec.Expire.IsZero() || sec.VersionAliases["stable"] != 2 {
		t.Errorf("after edit: expire %v, aliases %v", sec.Expire, sec.VersionAliases)
	}
	edit["setExpiration"] = "false"
	if err := p.Edit(ctx, "demo", []string{name}, edit); err != nil {
		t.Fatal(err)
	}
	if sec, _ = st.GetSecret("demo", "s"); !sec.Expire.IsZero() {
		t.Errorf("Set expiration date unchecked left %v", sec.Expire)
	}

	// Regional secrets: their own page and form.
	r := secretsRegionalProvider{p}
	if _, err := r.Create(ctx, "demo", map[string]string{"secretId": "g", "payload": "x"}); err == nil {
		t.Error("Create regional secret without a location was accepted")
	}
	if _, f := r.CreateForm(); f[1].Name != "location" {
		t.Errorf("Create regional secret's second field is %s", f[1].Name)
	}
}

// Create secret's Tags bind through Resource Manager's registry; an unknown
// key refuses the create with nothing stored (#1961).
func TestConsoleSecretTags(t *testing.T) {
	ctx := context.Background()
	p := secretsFixture(t)
	st := p.svc.Store()
	reg := resourcemanager.New(store.NewMemory())
	st.SetTagBinder(func() secrets.TagBinder { return reg })
	k, err := reg.CreateTagKey(&rmpb.TagKey{Parent: resourcemanager.OrganizationName, ShortName: "env"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateTagValue(&rmpb.TagValue{Parent: k.GetName(), ShortName: "prod"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, "tags-project", map[string]string{"secretId": "bad", "payload": "x", "tags": `{"tagKeys/42":"prod"}`}); err == nil {
		t.Error("an unknown tag key was accepted")
	}
	if _, err := st.GetSecret("tags-project", "bad"); err == nil {
		t.Error("the refused secret was stored")
	}
	if _, err := p.Create(ctx, "tags-project", map[string]string{"secretId": "ok", "payload": "x", "tags": `{"` + k.GetNamespacedName() + `":"prod"}`}); err != nil {
		t.Fatal(err)
	}
	b, err := reg.TagBindings(secrets.FullResourcePrefix + "projects/tags-project/secrets/ok")
	if err != nil || len(b) != 1 || b[0].GetTagValueNamespacedName() != k.GetNamespacedName()+"/prod" {
		t.Errorf("the secret's bindings = %v, %v", b, err)
	}
}
