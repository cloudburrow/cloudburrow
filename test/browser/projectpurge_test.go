//go:build browser

package browser

import (
	"context"
	"fmt"
	"testing"
	"time"

	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestResourceManagerPurgeProjectThroughThePage (#1591), in the storage
// shard. A project created and deleted with the official v3 client is
// DELETE_REQUESTED; its page offers Undelete project and Purge project now,
// which, confirmed by the project's ID, purges it: GetProject is then
// NOT_FOUND, and the ID is free for CreateProject again.
func TestResourceManagerPurgeProjectThroughThePage(t *testing.T) {
	needService(t, "projects")
	ctx := context.Background()
	projects, err := resourcemanager.NewProjectsClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projects.Close() })
	id := fmt.Sprintf("purge-br-%d", time.Now().UnixNano()%1e8)
	op, err := projects.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	dop, err := projects.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: "projects/" + id})
	if err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	p := open(t)
	p.navigate("/projects/" + id)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Purge project now")`)
	p.clickText("#view .page-actions button", "Purge project now")
	p.confirmByName(id)
	p.waitFor(`document.querySelector(".modal") === null`)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		_, err := projects.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + id})
		if status.Code(err) == codes.NotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Purge project now, GetProject = %v; want NOT_FOUND", err)
		}
	}
	again, err := projects.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id}})
	if err != nil {
		t.Fatalf("CreateProject of the purged ID: %v", err)
	}
	if _, err := again.Wait(ctx); err != nil {
		t.Fatalf("CreateProject of the purged ID: %v", err)
	}
	t.Cleanup(func() { _, _ = projects.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + id}) })
}
