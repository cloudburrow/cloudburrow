package resourcemanager

import (
	"context"
	"net/http"
	"testing"
	"time"

	crmv2 "google.golang.org/api/cloudresourcemanager/v2"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// v2 folders, the API gcloud resource-manager folders calls, through the
// official v2 client, over the registry v3 reads.
func TestV2FoldersThroughTheOfficialClient(t *testing.T) {
	reg, srv, _ := startAPI(t)
	ctx := context.Background()
	c, err := crmv2.NewService(ctx, option.WithEndpoint("http://"+srv.Addr()+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	op, err := c.Folders.Create(&crmv2.Folder{DisplayName: "Made by v2"}).Parent(OrganizationName).Context(ctx).Do()
	if err != nil || !op.Done {
		t.Fatalf("v2 create = %+v, %v", op, err)
	}
	all, _ := reg.Folders()
	if len(all) != 1 {
		t.Fatalf("registry folders = %+v", all)
	}
	name := all[0].Name
	if f, err := c.Folders.Get(name).Context(ctx).Do(); err != nil || f.DisplayName != "Made by v2" || f.LifecycleState != "ACTIVE" {
		t.Errorf("v2 get = %+v, %v", f, err)
	}
	if l, err := c.Folders.List().Parent(OrganizationName).Context(ctx).Do(); err != nil || len(l.Folders) != 1 {
		t.Errorf("v2 list = %+v, %v", l, err)
	}
	if l, err := c.Folders.Search(&crmv2.SearchFoldersRequest{Query: "displayName=Made*"}).Context(ctx).Do(); err != nil || len(l.Folders) != 1 {
		t.Errorf("v2 search = %+v, %v", l, err)
	}
	if f, err := c.Folders.Patch(name, &crmv2.Folder{DisplayName: "Renamed"}).UpdateMask("displayName").Context(ctx).Do(); err != nil || f.DisplayName != "Renamed" {
		t.Errorf("v2 patch = %+v, %v", f, err)
	}
	child, err := reg.CreateFolder(OrganizationName, "Other")
	if err != nil {
		t.Fatal(err)
	}
	if op, err := c.Folders.Move(name, &crmv2.MoveFolderRequest{DestinationParent: child.Name}).Context(ctx).Do(); err != nil || !op.Done {
		t.Errorf("v2 move = %+v, %v", op, err)
	}
	if f, err := c.Folders.Delete(name).Context(ctx).Do(); err != nil || f.LifecycleState != "DELETE_REQUESTED" {
		t.Errorf("v2 delete = %+v, %v", f, err)
	}
	if f, err := c.Folders.Undelete(name, &crmv2.UndeleteFolderRequest{}).Context(ctx).Do(); err != nil || f.LifecycleState != "ACTIVE" {
		t.Errorf("v2 undelete = %+v, %v", f, err)
	}
}

// A deleted folder is purged after RetentionPeriod, and folders nest at
// most MaxFolderDepth deep.
func TestFolderRetentionAndDepth(t *testing.T) {
	reg := New(store.NewMemory())
	now := time.Now()
	reg.now = func() time.Time { return now }
	parent := OrganizationName
	var first Folder
	for i := 0; i < MaxFolderDepth; i++ {
		f, err := reg.CreateFolder(parent, "Level")
		if err != nil {
			t.Fatalf("depth %d: %v", i+1, err)
		}
		if i == 0 {
			first = f
		}
		parent = f.Name
	}
	if _, err := reg.CreateFolder(parent, "Too deep"); err == nil {
		t.Error("an eleventh nested folder was created")
	}
	leaf, _ := reg.CreateFolder(OrganizationName, "Leaf")
	if _, err := reg.MoveFolder(first.Name, leaf.Name); err == nil {
		t.Error("a move that nests eleven deep succeeded")
	}
	if _, err := reg.DeleteFolder(leaf.Name); err != nil {
		t.Fatal(err)
	}
	now = now.Add(RetentionPeriod)
	if _, err := reg.Folder(leaf.Name); err == nil {
		t.Error("a folder deleted RetentionPeriod ago is still there")
	}
}
