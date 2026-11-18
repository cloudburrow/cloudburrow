package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleAuditLogsScopes (#1756): the Audit Logs page lists a folder's
// and the organization's auditable services when the resource selector
// names one, with that resource's audit configs; Edit log types and Set
// default configuration change only the audit configs, through
// SetIamPolicy with the auditConfigs mask, and a stale etag is refused.
func TestConsoleAuditLogsScopes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := resourcemanager.New(store.NewMemory())
	h := hierarchyProvider{registry: reg}
	id, err := h.Create(ctx, "", map[string]string{"displayName": "Audit", "parent": resourcemanager.OrganizationName})
	if err != nil {
		t.Fatal(err)
	}
	p := iamAuditProvider{registry: reg}
	for _, c := range []struct {
		resource string
		api      iamServer
	}{
		{"folders/" + id, resourcemanager.NewFoldersServer(reg)},
		{resourcemanager.OrganizationName, resourcemanager.NewOrganizationsServer(reg)},
	} {
		t.Run(c.resource, func(t *testing.T) {
			sctx := console.WithScopeValue(ctx, iamAuditScopeKey, c.resource)
			// A binding the audit config edits must keep.
			pol, err := c.api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: c.resource})
			if err != nil {
				t.Fatal(err)
			}
			pol.Bindings = []*iampb.Binding{{Role: "roles/viewer", Members: []string{"user:kept@example.com"}}}
			if _, err := c.api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: c.resource, Policy: pol}); err != nil {
				t.Fatal(err)
			}
			l, err := p.List(sctx, "any-project")
			if err != nil || l.Unavailable != "" || len(l.Items) == 0 {
				t.Fatalf("List = %d rows, %q, %v", len(l.Items), l.Unavailable, err)
			}
			if l.Scope == nil || l.Scope.Selected != c.resource ||
				!slices.ContainsFunc(l.Scope.Options, func(o console.ScopeOption) bool { return o.ID == "folders/"+id }) {
				t.Errorf("the resource selector = %+v; want %s selected and the folder offered", l.Scope, c.resource)
			}
			row := l.Items[slices.IndexFunc(l.Items, func(r console.Resource) bool { return r.Name == "storage.googleapis.com" })]
			if len(row.Actions) != 1 {
				t.Fatalf("storage.googleapis.com's row offers %v", row.Actions)
			}
			etag := row.Actions[0].Fields[len(row.Actions[0].Fields)-1].Default
			edit := map[string]string{"dataRead": "true", "dataReadExempted": "user:ada@example.com\nuser:bo@example.com", "etag": etag}
			if err := p.ActAt(sctx, "any-project", []string{"storage.googleapis.com"}, auditEditAction, edit); err != nil {
				t.Fatalf("Edit log types: %v", err)
			}
			// The page's etag is now stale.
			if err := p.ActAt(sctx, "any-project", []string{"storage.googleapis.com"}, auditEditAction, edit); err == nil ||
				!strings.Contains(consoleErr(err), "etag") {
				t.Errorf("Edit log types with a stale etag = %v; want the etag refused", err)
			}
			acts := p.ListActions(sctx, "any-project")
			if len(acts) != 1 || acts[0].ID != auditDefaultAction {
				t.Fatalf("ListActions = %v", acts)
			}
			def := map[string]string{"adminRead": "true", "etag": acts[0].Fields[len(acts[0].Fields)-1].Default}
			if err := p.ActAt(sctx, "any-project", nil, auditDefaultAction, def); err != nil {
				t.Fatalf("Set default configuration: %v", err)
			}
			got, err := c.api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: c.resource})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != "user:kept@example.com" {
				t.Errorf("the bindings after the audit edits = %v; want them kept", got.GetBindings())
			}
			cfg := auditConfigsByService(got)
			if lc := logConfig(cfg["storage.googleapis.com"], "DATA_READ"); lc == nil || len(lc.GetExemptedMembers()) != 2 {
				t.Errorf("storage.googleapis.com's audit config = %v", cfg["storage.googleapis.com"])
			}
			if logConfig(cfg[allServices], "ADMIN_READ") == nil {
				t.Errorf("allServices' audit config = %v", cfg[allServices])
			}
			l, _ = p.List(sctx, "any-project")
			for _, r := range l.Items {
				if r.Fields["Admin Read"] != "Enabled" {
					t.Errorf("%s's Admin Read reads %q; allServices enables it", r.Name, r.Fields["Admin Read"])
					break
				}
			}
			// Exempting principals from a log type that is not enabled is refused.
			acts = p.ListActions(sctx, "any-project")
			bad := map[string]string{"dataWriteExempted": "user:x@example.com", "etag": acts[0].Fields[len(acts[0].Fields)-1].Default}
			if err := p.ActAt(sctx, "any-project", nil, auditDefaultAction, bad); err == nil {
				t.Error("an exemption from a disabled log type was accepted")
			}
			// Clearing every log type removes the service's config.
			l, _ = p.List(sctx, "any-project")
			row = l.Items[slices.IndexFunc(l.Items, func(r console.Resource) bool { return r.Name == "storage.googleapis.com" })]
			clear := map[string]string{"etag": row.Actions[0].Fields[len(row.Actions[0].Fields)-1].Default}
			if err := p.ActAt(sctx, "any-project", []string{"storage.googleapis.com"}, auditEditAction, clear); err != nil {
				t.Fatal(err)
			}
			got, _ = c.api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: c.resource})
			if _, ok := auditConfigsByService(got)["storage.googleapis.com"]; ok {
				t.Errorf("storage.googleapis.com's config stayed after every log type was cleared: %v", got.GetAuditConfigs())
			}
		})
	}
	if _, err := auditResource(console.WithScopeValue(ctx, iamAuditScopeKey, "buckets/x"), "p"); err == nil {
		t.Error("a resource that is no project, folder or organization was accepted")
	}
}

// TestPolicyResourceNames (#1756): the full resource names Run Linter
// gives LintPolicy.
func TestPolicyResourceNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := resourcemanager.New(store.NewMemory())
	for _, c := range []struct {
		got, want string
	}{
		{projectIAMProvider{registry: reg}.PolicyResource(ctx, "", []string{"my-project"}), "//cloudresourcemanager.googleapis.com/projects/my-project"},
		{hierarchyProvider{registry: reg}.PolicyResource(ctx, "", []string{"123"}), "//cloudresourcemanager.googleapis.com/folders/123"},
		{hierarchyProvider{registry: reg}.PolicyResource(ctx, "", []string{organizationSegment}), "//cloudresourcemanager.googleapis.com/" + resourcemanager.OrganizationName},
		{spannerProvider{}.PolicyResource(ctx, "p", []string{"i", "d"}), "//spanner.googleapis.com/projects/p/instances/i/databases/d"},
		{bigtableInstancesProvider{}.PolicyResource(ctx, "p", []string{"i"}), "//bigtableadmin.googleapis.com/projects/p/instances/i"},
		{loggingBucketsProvider{}.PolicyResource(ctx, "p", []string{"projects/p/locations/global/buckets/b", "v"}), "//logging.googleapis.com/projects/p/locations/global/buckets/b/views/v"},
		{kmsProvider{}.PolicyResource(ctx, "p", []string{"projects/p/locations/l/keyRings/r", "k"}), "//cloudkms.googleapis.com/projects/p/locations/l/keyRings/r/cryptoKeys/k"},
	} {
		if c.got != c.want {
			t.Errorf("PolicyResource = %q; want %q", c.got, c.want)
		}
	}
}
