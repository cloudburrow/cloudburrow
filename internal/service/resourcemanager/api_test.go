package resourcemanager

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/store"
	grpcx "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

func startAPI(t *testing.T) (*Registry, *Server, *resourcemanager.ProjectsClient) {
	t.Helper()
	reg := New(store.NewMemory())
	srv := NewServer("127.0.0.1:0", reg)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	c, err := resourcemanager.NewProjectsClient(context.Background(),
		option.WithEndpoint(srv.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return reg, srv, c
}

// The lifecycle through the official v3 client, against the registry the
// console reads: what the API creates the registry holds, and the reverse.
func TestProjectsLifecycleThroughTheOfficialClient(t *testing.T) {
	reg, _, c := startAPI(t)
	ctx := context.Background()

	op, err := c.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{
		ProjectId: "api-made-1", DisplayName: "API made", Labels: map[string]string{"team": "a"}}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	p, err := op.Wait(ctx)
	// v3 names a project by its number, resource.ProjectNumber (#1211), and
	// answers a request naming it either way.
	number := fmt.Sprint(resource.ProjectNumber("api-made-1"))
	if err != nil || p.GetName() != "projects/"+number || p.GetState() != rmpb.Project_ACTIVE {
		t.Fatalf("created %v, %v; want the name projects/%s", p, err, number)
	}
	if g, err := c.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + number}); err != nil || g.GetProjectId() != "api-made-1" {
		t.Errorf("GetProject(projects/%s) = %v, %v; want api-made-1", number, g, err)
	}
	if _, err := c.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/123"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetProject of a number no project has = %v, want NotFound", err)
	}
	if got, err := reg.Get("api-made-1"); err != nil || got.DisplayName != "API made" {
		t.Errorf("the registry does not hold the API-created project: %+v %v", got, err)
	}

	// Made through the registry, as the console does, and found by the API.
	if _, err := reg.Create(Project{ProjectID: "console-made-1"}); err != nil {
		t.Fatal(err)
	}
	if g, err := c.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/console-made-1"}); err != nil || g.GetProjectId() != "console-made-1" {
		t.Errorf("GetProject(console-made-1) = %v, %v", g, err)
	}
	var ids []string
	it := c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{})
	for {
		sp, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("SearchProjects: %v", err)
		}
		ids = append(ids, sp.GetProjectId())
	}
	if len(ids) != 2 || ids[0] != "api-made-1" || ids[1] != "console-made-1" {
		t.Errorf("SearchProjects = %v", ids)
	}
	it = c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{Query: "labels.team:a"})
	if sp, err := it.Next(); err != nil || sp.GetProjectId() != "api-made-1" {
		t.Errorf("SearchProjects(labels.team:a) = %v, %v", sp, err)
	} else if _, err := it.Next(); err != iterator.Done {
		t.Errorf("SearchProjects(labels.team:a) returned more than one project")
	}

	uop, err := c.UpdateProject(ctx, &rmpb.UpdateProjectRequest{
		Project:    &rmpb.Project{Name: "projects/api-made-1", Labels: map[string]string{"team": "b", "env": "dev"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil {
		t.Fatal(err)
	}
	if up, err := uop.Wait(ctx); err != nil || up.GetLabels()["team"] != "b" || up.GetDisplayName() != "API made" {
		t.Errorf("UpdateProject(labels) = %v, %v; want labels changed and the display name kept", up, err)
	}
	if _, err := c.UpdateProject(ctx, &rmpb.UpdateProjectRequest{
		Project: &rmpb.Project{Name: "projects/api-made-1"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"parent"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateProject(parent) = %v, want InvalidArgument", err)
	}

	dop, err := c.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: "projects/api-made-1"})
	if err != nil {
		t.Fatal(err)
	}
	if dp, err := dop.Wait(ctx); err != nil || dp.GetState() != rmpb.Project_DELETE_REQUESTED {
		t.Errorf("DeleteProject = %v, %v", dp, err)
	}
	// Kept in DELETE_REQUESTED for 30 days (#1216): read, not searched by
	// default, found by a state query, and restored by UndeleteProject.
	if p, err := reg.Get("api-made-1"); err != nil || p.State != StateDeleteRequested {
		t.Errorf("registry after delete = %+v, %v; want DELETE_REQUESTED", p, err)
	}
	if p, err := c.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/api-made-1"}); err != nil || p.GetState() != rmpb.Project_DELETE_REQUESTED {
		t.Errorf("GetProject after delete = %v, %v; want DELETE_REQUESTED", p, err)
	}
	if p, err := c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{Query: "id:api-made-1"}).Next(); err != iterator.Done {
		t.Errorf("SearchProjects found a deleted project without a state term: %v, %v", p, err)
	}
	if p, err := c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{Query: "id:api-made-1 state:DELETE_REQUESTED"}).Next(); err != nil || p.GetProjectId() != "api-made-1" {
		t.Errorf("SearchProjects(state:DELETE_REQUESTED) = %v, %v", p, err)
	}
	undo, err := c.UndeleteProject(ctx, &rmpb.UndeleteProjectRequest{Name: "projects/api-made-1"})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := undo.Wait(ctx); err != nil || p.GetState() != rmpb.Project_ACTIVE {
		t.Errorf("UndeleteProject = %v, %v; want ACTIVE", p, err)
	}
	if _, err := c.UndeleteProject(ctx, &rmpb.UndeleteProjectRequest{Name: "projects/api-made-1"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("UndeleteProject of an active project = %v, want FailedPrecondition", err)
	}
	if _, err := c.MoveProject(ctx, &rmpb.MoveProjectRequest{Name: "projects/api-made-1", DestinationParent: "folders/1"}); status.Code(err) != codes.NotFound {
		t.Errorf("MoveProject to an absent folder = %v, want NotFound", err)
	}
}

func TestProjectsRefusesWhatItDoesNotServe(t *testing.T) {
	_, _, c := startAPI(t)
	ctx := context.Background()
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/x"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("GetIamPolicy = %v", err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: "projects/x"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("SetIamPolicy = %v", err)
	}
	if _, err := c.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: "in-folder-1", Parent: "folders/123"}}); status.Code(err) != codes.NotFound {
		t.Errorf("CreateProject under an absent folder = %v", err)
	}
	if _, err := c.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: "BAD"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateProject(BAD) = %v", err)
	}
	it := c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{Query: "id:a OR id:b"})
	if _, err := it.Next(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("SearchProjects with OR = %v, want InvalidArgument rather than a partial match", err)
	}
}

// The hierarchy through the official v3 Folders, Organizations and Projects
// clients (#1294): the organization, folders under it and under a folder,
// projects parented by both, list, search, rename, move, delete only when
// empty, undelete, and IAM UNIMPLEMENTED.
func TestHierarchyThroughTheOfficialClients(t *testing.T) {
	_, srv, pc := startAPI(t)
	ctx := context.Background()
	opts := []option.ClientOption{option.WithEndpoint(srv.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	fc, err := resourcemanager.NewFoldersClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()
	oc, err := resourcemanager.NewOrganizationsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer oc.Close()

	if o, err := oc.GetOrganization(ctx, &rmpb.GetOrganizationRequest{Name: OrganizationName}); err != nil || o.GetDisplayName() != OrganizationDomain {
		t.Fatalf("GetOrganization = %v, %v", o, err)
	}
	if _, err := oc.GetOrganization(ctx, &rmpb.GetOrganizationRequest{Name: "organizations/1"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetOrganization(other) = %v, want NotFound", err)
	}
	if o, err := oc.SearchOrganizations(ctx, &rmpb.SearchOrganizationsRequest{Query: "domain:" + OrganizationDomain}).Next(); err != nil || o.GetName() != OrganizationName {
		t.Errorf("SearchOrganizations = %v, %v", o, err)
	}
	if _, err := oc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: OrganizationName}); status.Code(err) != codes.Unimplemented {
		t.Errorf("organization GetIamPolicy = %v, want Unimplemented", err)
	}

	create := func(parent, name string) *rmpb.Folder {
		t.Helper()
		op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: parent, DisplayName: name}})
		if err != nil {
			t.Fatalf("CreateFolder(%s, %s): %v", parent, name, err)
		}
		f, err := op.Wait(ctx)
		if err != nil || f.GetParent() != parent || f.GetState() != rmpb.Folder_ACTIVE {
			t.Fatalf("CreateFolder = %v, %v", f, err)
		}
		return f
	}
	eng := create(OrganizationName, "Engineering")
	team := create(eng.GetName(), "Team A")
	ops := create(OrganizationName, "Ops")
	if _, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: OrganizationName, DisplayName: "Engineering"}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a duplicate sibling name = %v, want FailedPrecondition", err)
	}
	if _, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: OrganizationName, DisplayName: "-bad"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a bad name = %v, want InvalidArgument", err)
	}
	if f, err := fc.GetFolder(ctx, &rmpb.GetFolderRequest{Name: team.GetName()}); err != nil || f.GetDisplayName() != "Team A" {
		t.Errorf("GetFolder = %v, %v", f, err)
	}
	var top []string
	it := fc.ListFolders(ctx, &rmpb.ListFoldersRequest{Parent: OrganizationName})
	for {
		f, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		top = append(top, f.GetDisplayName())
	}
	if len(top) != 2 {
		t.Errorf("ListFolders(org) = %v, want Engineering and Ops", top)
	}
	if f, err := fc.SearchFolders(ctx, &rmpb.SearchFoldersRequest{Query: "displayName=Team*"}).Next(); err != nil || f.GetName() != team.GetName() {
		t.Errorf("SearchFolders = %v, %v", f, err)
	}
	up, err := fc.UpdateFolder(ctx, &rmpb.UpdateFolderRequest{Folder: &rmpb.Folder{Name: team.GetName(), DisplayName: "Team B"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatal(err)
	}
	if f, err := up.Wait(ctx); err != nil || f.GetDisplayName() != "Team B" {
		t.Errorf("UpdateFolder = %v, %v", f, err)
	}

	// A project under the nested folder.
	cp, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: "in-folder-1", Parent: team.GetName()}})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := cp.Wait(ctx); err != nil || p.GetParent() != team.GetName() {
		t.Fatalf("CreateProject under a folder = %v, %v", p, err)
	}
	if p, err := pc.ListProjects(ctx, &rmpb.ListProjectsRequest{Parent: team.GetName()}).Next(); err != nil || p.GetProjectId() != "in-folder-1" {
		t.Errorf("ListProjects(folder) = %v, %v", p, err)
	}
	if p, err := pc.SearchProjects(ctx, &rmpb.SearchProjectsRequest{Query: "parent:" + team.GetName()}).Next(); err != nil || p.GetProjectId() != "in-folder-1" {
		t.Errorf("SearchProjects(parent) = %v, %v", p, err)
	}
	// The folder cannot be moved under itself, and is not deleted while
	// the project is in it.
	if _, err := fc.MoveFolder(ctx, &rmpb.MoveFolderRequest{Name: eng.GetName(), DestinationParent: team.GetName()}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("MoveFolder under its own child = %v, want InvalidArgument", err)
	}
	if _, err := fc.DeleteFolder(ctx, &rmpb.DeleteFolderRequest{Name: team.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteFolder of a folder with a project = %v, want FailedPrecondition", err)
	}
	mv, err := fc.MoveFolder(ctx, &rmpb.MoveFolderRequest{Name: team.GetName(), DestinationParent: ops.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if f, err := mv.Wait(ctx); err != nil || f.GetParent() != ops.GetName() {
		t.Errorf("MoveFolder = %v, %v", f, err)
	}
	mp, err := pc.MoveProject(ctx, &rmpb.MoveProjectRequest{Name: "projects/in-folder-1", DestinationParent: OrganizationName})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := mp.Wait(ctx); err != nil || p.GetParent() != OrganizationName {
		t.Errorf("MoveProject = %v, %v", p, err)
	}
	del, err := fc.DeleteFolder(ctx, &rmpb.DeleteFolderRequest{Name: team.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if f, err := del.Wait(ctx); err != nil || f.GetState() != rmpb.Folder_DELETE_REQUESTED {
		t.Errorf("DeleteFolder = %v, %v", f, err)
	}
	if _, err := fc.ListFolders(ctx, &rmpb.ListFoldersRequest{Parent: ops.GetName()}).Next(); err != iterator.Done {
		t.Errorf("ListFolders without show_deleted lists a deleted folder: %v", err)
	}
	ud, err := fc.UndeleteFolder(ctx, &rmpb.UndeleteFolderRequest{Name: team.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if f, err := ud.Wait(ctx); err != nil || f.GetState() != rmpb.Folder_ACTIVE {
		t.Errorf("UndeleteFolder = %v, %v", f, err)
	}
	if _, err := fc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: team.GetName()}); status.Code(err) != codes.Unimplemented {
		t.Errorf("folder GetIamPolicy = %v, want Unimplemented", err)
	}
}

func TestProjectsOverREST(t *testing.T) {
	_, srv, _ := startAPI(t)
	c, err := resourcemanager.NewProjectsRESTClient(context.Background(),
		option.WithEndpoint("http://"+srv.Addr()), option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	op, err := c.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: "rest-made-1"}})
	if err != nil {
		t.Fatalf("REST CreateProject: %v", err)
	}
	if p, err := op.Wait(ctx); err != nil || p.GetProjectId() != "rest-made-1" {
		t.Fatalf("REST created %v, %v", p, err)
	}
	if p, err := c.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/rest-made-1"}); err != nil || p.GetDisplayName() != "rest-made-1" {
		t.Errorf("REST GetProject = %v, %v", p, err)
	}
	it := c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{Query: "id:rest-made-1"})
	if p, err := it.Next(); err != nil || p.GetProjectId() != "rest-made-1" {
		t.Errorf("REST SearchProjects = %v, %v", p, err)
	}
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/rest-made-1"}); err == nil {
		t.Error("REST GetIamPolicy succeeded")
	}
	dop, err := c.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: "projects/rest-made-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Errorf("REST delete wait: %v", err)
	}
}

// An interposed interceptor sees each gRPC call, which is how `up` writes the
// request line `cloudburrow logs --service resourcemanager` reads (#587).
func TestInterposeSeesEachCall(t *testing.T) {
	reg := New(store.NewMemory())
	srv := NewServer("127.0.0.1:0", reg)
	var logged bytes.Buffer
	srv.Interpose(grpcx.LogInterceptor(slog.New(grpcx.NewLineHandler(&logged, slog.LevelInfo)), "resourcemanager"))
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	c, err := resourcemanager.NewProjectsClient(context.Background(),
		option.WithEndpoint(srv.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.GetProject(context.Background(), &rmpb.GetProjectRequest{Name: "projects/absent-1"}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetProject(absent) = %v, want NotFound", err)
	}
	if got := logged.String(); !strings.HasPrefix(got, "INFO  resourcemanager.GetProject => NOT_FOUND") {
		t.Errorf("request log = %q", got)
	}
}
