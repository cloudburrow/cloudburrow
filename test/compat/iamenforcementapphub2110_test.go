//go:build compat

package compat

import (
	"testing"

	apphub "cloud.google.com/go/apphub/apiv1"
	"cloud.google.com/go/apphub/apiv1/apphubpb"
)

// TestIAMEnforcementAppHub (#2110), with the official App Hub client, as
// the REST reference names each method's permissions: without a role
// ListApplications is refused; App Hub Viewer lists and reads but cannot
// create an application; App Hub Editor creates one, but cannot attach a
// service project, which also needs apphub.serviceProjectAttachments.attach
// on the service project (Admin only); the instance's own account attaches
// it.
func TestIAMEnforcementAppHub(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvAppHub)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ieah-")
	global := "projects/" + project + "/locations/global"
	p := newIAMPrincipal(t, h, project, "ieah")
	c, err := apphub.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	list := func() error {
		_, err := c.ListApplications(ctx, &apphubpb.ListApplicationsRequest{Parent: global}).Next()
		return first(err)
	}
	wantDenied(t, "ListApplications without a role", list(), newerDenied("apphub.applications.list", global), "apphub.applications.list", true)
	p.grant("roles/apphub.viewer")
	t.Cleanup(func() { p.revoke("roles/apphub.viewer") })
	waitGranted(t, "ListApplications as an App Hub Viewer", list)
	create := func(id string) error {
		op, err := c.CreateApplication(ctx, &apphubpb.CreateApplicationRequest{Parent: global, ApplicationId: id,
			Application: &apphubpb.Application{Scope: &apphubpb.Scope{Type: apphubpb.Scope_GLOBAL}}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	wantDenied(t, "CreateApplication as a Viewer", create("viewer-app"), newerDenied("apphub.applications.create", global), "apphub.applications.create", true)
	p.grant("roles/apphub.editor")
	t.Cleanup(func() { p.revoke("roles/apphub.editor") })
	waitGranted(t, "CreateApplication as an App Hub Editor", func() error { return create("editor-app") })
	if _, err := c.GetApplication(ctx, &apphubpb.GetApplicationRequest{Name: global + "/applications/editor-app"}); err != nil {
		t.Fatalf("GetApplication as an Editor: %v", err)
	}
	attach := func(c *apphub.Client) error {
		op, err := c.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: global, ServiceProjectAttachmentId: project,
			ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/" + project}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	wantDenied(t, "CreateServiceProjectAttachment as an Editor", attach(c),
		newerDenied("apphub.serviceProjectAttachments.attach", "projects/"+project), "apphub.serviceProjectAttachments.attach", true)
	owner, err := apphub.NewClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	t.Cleanup(func() {
		_, _ = owner.DetachServiceProjectAttachment(h.Context(), &apphubpb.DetachServiceProjectAttachmentRequest{Name: global})
		if op, err := owner.DeleteApplication(h.Context(), &apphubpb.DeleteApplicationRequest{Name: global + "/applications/editor-app"}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	if err := attach(owner); err != nil {
		t.Fatalf("CreateServiceProjectAttachment as the instance's account: %v", err)
	}
}
