package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// IAM & Admin > Service accounts (#1747): Create, the list, the account's
// page with its keys, Edit, Disable and Enable, Create key, a key's
// Disable and Delete, the Permissions tab, Delete and Undelete, over the
// registry the IAM API serves, with the API's refusals.
func TestConsoleIAMServiceAccounts(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "console-sa"}); err != nil {
		t.Fatal(err)
	}
	p := iamServiceAccountsProvider{registry: reg}
	ctx := context.Background()
	email, err := p.Create(ctx, "console-sa", map[string]string{"accountId": "console-account", "displayName": "Console"})
	if err != nil || email != "console-account@console-sa.iam.gserviceaccount.com" {
		t.Fatalf("Create = %q, %v", email, err)
	}
	if _, err := p.Create(ctx, "console-sa", map[string]string{"accountId": "x"}); err == nil {
		t.Error("a one-letter ID was accepted")
	}
	l, _ := p.List(ctx, "console-sa")
	if len(l.Items) != 1 || l.Items[0].Fields["Name"] != "Console" || l.Items[0].Status != "ENABLED" || l.Items[0].Fields["Keys"] != "0" {
		t.Fatalf("List = %+v", l.Items)
	}
	if err := p.Edit(ctx, "console-sa", []string{email}, map[string]string{"displayName": "Renamed", "description": "d"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Act(ctx, "console-sa", email, "disable"); err != nil {
		t.Fatal(err)
	}
	if st, _ := reg.ServiceAccountByName(email); !st.Disabled {
		t.Error("not disabled")
	}
	if err := p.Act(ctx, "console-sa", email, "enable"); err != nil {
		t.Fatal(err)
	}
	res, err := p.ActAtResult(ctx, "console-sa", []string{email}, "create-key", nil)
	if err != nil || res == nil || len(res.Items) != 1 || !json.Valid([]byte(res.Items[0].Fields["Key file (JSON)"])) {
		t.Fatalf("Create key = %v", err)
	}
	keyID := res.Items[0].Name
	d, _ := p.Detail(ctx, "console-sa", []string{email})
	// Google's tabs (#1864): Details, Permissions, Keys, Metrics.
	if d.Unavailable != "" || d.Edit != nil || len(d.Sections) != 4 || d.Sections[2].Listing.Total != 2 {
		t.Fatalf("Detail: unavailable %q, sections %d", d.Unavailable, len(d.Sections))
	}
	var ids []string
	for _, s := range d.Sections {
		ids = append(ids, s.ID)
	}
	if got := strings.Join(ids, ","); got != "details,permissions,keys,metrics" {
		t.Errorf("the account's tabs are %s; want details,permissions,keys,metrics", got)
	}
	// The Details tab's own Name box and Save (#1864): a name change keeps
	// the description.
	if e := d.Sections[0].Edit; e == nil || e.Submit != "Save" || e.Fields[0].Label != "Name" {
		t.Fatalf("the Details tab's form is %+v", e)
	}
	if err := p.Edit(ctx, "console-sa", []string{email}, map[string]string{"description": "keep me"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Edit(ctx, "console-sa", []string{email}, map[string]string{"displayName": "Renamed"}); err != nil {
		t.Fatal(err)
	}
	if a, _ := p.registry.GetServiceAccount("projects/console-sa/serviceAccounts/" + email); a.GetDisplayName() != "Renamed" || a.GetDescription() != "keep me" {
		t.Errorf("after the Name box's Save the account is %q, %q", a.GetDisplayName(), a.GetDescription())
	}
	kd, _ := p.Detail(ctx, "console-sa", []string{email, keyID})
	if kd.Unavailable != "" || !strings.HasPrefix(kd.Sections[1].Text, "-----BEGIN CERTIFICATE-----") {
		t.Errorf("key page: %q", kd.Unavailable)
	}
	if err := p.ActAt(ctx, "console-sa", []string{email, keyID}, "disable-key", nil); err != nil {
		t.Fatal(err)
	}
	if acts := p.DetailActions(ctx, "console-sa", []string{email, keyID}); len(acts) != 2 || acts[0].ID != "enable-key" {
		t.Errorf("a disabled key's actions = %+v", acts)
	}
	if err := p.ActAt(ctx, "console-sa", []string{email, keyID}, "delete-key", nil); err != nil {
		t.Fatal(err)
	}
	// The Permissions tab is the account's own policy.
	if p.PolicyOn(ctx, []string{email}) == nil {
		t.Fatal("no Permissions tab")
	}
	pol, err := p.GetPolicy(ctx, "console-sa", []string{email})
	if err != nil {
		t.Fatal(err)
	}
	pol.Bindings = append(pol.Bindings, console.Binding{Role: "roles/iam.serviceAccountTokenCreator", Members: []string{"user:dev@example.com"}})
	if err := p.SetPolicy(ctx, "console-sa", []string{email}, pol); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if stored, _ := reg.IAMPolicy("projects/console-sa/serviceAccounts/" + email); stored == nil || len(stored.Bindings) != 1 {
		t.Errorf("stored policy = %+v", stored)
	}
	st, _ := reg.ServiceAccountByName(email)
	if err := p.Delete(ctx, "console-sa", email); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, "console-sa"); len(l.Items) != 0 {
		t.Errorf("a deleted account is listed")
	}
	if err := p.ActAt(ctx, "console-sa", nil, "undelete", map[string]string{"uniqueId": st.UniqueID}); err != nil {
		t.Fatalf("Undelete: %v", err)
	}
	if l, _ := p.List(ctx, "console-sa"); len(l.Items) != 1 || l.Items[0].Fields["Name"] != "Renamed" {
		t.Errorf("after Undelete = %+v", l.Items)
	}
}

// TestConsoleIAMServiceAccountCreateAndContinue (#1864): Create and
// continue makes the account with no grant; Done then grants its project
// role and users to the account already made, not a second one.
func TestConsoleIAMServiceAccountCreateAndContinue(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "console-sa"}); err != nil {
		t.Fatal(err)
	}
	p := iamServiceAccountsProvider{registry: reg}
	ctx := context.Background()
	if p.CreateCommitStep() != 1 {
		t.Errorf("the account is made at step %d", p.CreateCommitStep())
	}
	email, err := p.Create(ctx, "console-sa", map[string]string{"accountId": "runner", "displayName": "Runner", console.CreateCommitKey: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.GetServiceAccount("projects/console-sa/serviceAccounts/" + email); err != nil {
		t.Fatalf("Create and continue made no account: %v", err)
	}
	got, err := p.Create(ctx, "console-sa", map[string]string{"accountId": "runner", "displayName": "Runner", "roles": "roles/viewer",
		"users": "user:ada@example.com", console.CreateCreatedKey: email})
	if err != nil || got != email {
		t.Fatalf("Done after Create and continue = %q, %v", got, err)
	}
	pol, err := getProtoPolicy(ctx, resourcemanager.NewProjectsServer(reg), resourcemanager.V3Name("console-sa"))
	if err != nil {
		t.Fatal(err)
	}
	granted := false
	for _, b := range pol.Bindings {
		for _, m := range b.Members {
			granted = granted || (b.Role == "roles/viewer" && m == "serviceAccount:"+email)
		}
	}
	if !granted {
		t.Errorf("Done did not grant roles/viewer to %s: %+v", email, pol.Bindings)
	}
	if _, err := p.Create(ctx, "console-sa", map[string]string{console.CreateCreatedKey: "nobody@console-sa.iam.gserviceaccount.com"}); err == nil {
		t.Error("Done for an account never made was accepted")
	}
}
