//go:build compat

package compat

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	control "cloud.google.com/go/storage/control/apiv2"
	"cloud.google.com/go/storage/control/apiv2/controlpb"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	storagev1 "google.golang.org/api/storage/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Hierarchical-namespace folders (#1203), through the official Storage
// Control client's REST transport (cloud.google.com/go/storage/control/apiv2,
// NewStorageControlRESTClient, the v2 API) and Google's generated JSON API
// client (google.golang.org/api/storage/v1), on a bucket the official
// storage client creates with HierarchicalNamespace enabled.

func storageControl(t *testing.T, h *Harness) *control.StorageControlClient {
	t.Helper()
	c, err := control.NewStorageControlRESTClient(h.Context(), option.WithoutAuthentication(),
		option.WithEndpoint(h.Endpoint(EnvStorage)), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatalf("NewStorageControlRESTClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// hnsBucket is a bucket with hierarchical namespace, and so uniform
// bucket-level access. Its cleanup deletes what the test left, folders
// included.
func hnsBucket(t *testing.T, h *Harness, c *storage.Client, s *storagev1.Service, suffix string) *storage.BucketHandle {
	t.Helper()
	bh := c.Bucket(h.Project() + suffix)
	if err := bh.Create(h.Context(), h.Project(), &storage.BucketAttrs{
		UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true},
		HierarchicalNamespace:    &storage.HierarchicalNamespace{Enabled: true},
	}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		fs, _ := s.Folders.List(bh.BucketName()).Delimiter("/").Context(ctx).Do()
		if fs != nil {
			for _, f := range fs.Items {
				_, _ = s.Folders.DeleteRecursive(bh.BucketName(), f.Name).Context(ctx).Do()
			}
		}
		emptyAndDelete(ctx, bh)
	})
	return bh
}

func putText(t *testing.T, ctx context.Context, bh *storage.BucketHandle, name string) {
	t.Helper()
	w := bh.Object(name).NewWriter(ctx)
	_, _ = io.WriteString(w, "x")
	if err := w.Close(); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// grpcCode is the canonical code of an error from the control client's
// REST transport, read from the error body's status, as apierror does.
func grpcCode(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	if ae, ok := apierror.FromError(err); ok && ae.GRPCStatus() != nil {
		return ae.GRPCStatus().Code()
	}
	if s, ok := status.FromError(err); ok {
		return s.Code()
	}
	return codes.Unknown
}

// TestStorageFoldersControl: on an HNS bucket the Storage Control client
// creates a folder (with recursive, its parents too), gets and lists it;
// writing an object creates the folders in its path; a non-empty folder is
// not deleted; RenameFolder is a long-running operation that, waited on,
// returns the renamed folder with the objects moved under it; and
// DeleteFolderRecursive removes the folder and its objects. A folder call on
// a bucket without hierarchical namespace is refused.
//
// unverified: storage.folders.insert 400: a bucket without hierarchical namespace
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/create-folders
// unverified: storage.folders.insert 400: a missing parent without recursive
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/create-folders
// unverified: storage.folders.delete 409: a folder holding objects or folders (FAILED_PRECONDITION, 400, on the v2 API)
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/storage/control/v2/storage_control.proto
// covers: storage.folders.insert, storage.folders.get, storage.folders.list, storage.folders.delete, storage.folders.rename, storage.folders.deleteRecursive
func TestStorageFoldersControl(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	s := storageJSON(t, h)
	sc := storageControl(t, h)
	ctx := h.Context()
	bh := hnsBucket(t, h, c, s, "-hnsc")
	b := bh.BucketName()
	parent := "projects/_/buckets/" + b

	attrs, err := bh.Attrs(ctx)
	if err != nil || attrs.HierarchicalNamespace == nil || !attrs.HierarchicalNamespace.Enabled {
		t.Fatalf("bucket attrs = %+v, %v; want hierarchical namespace enabled", attrs, err)
	}
	if _, err := sc.CreateFolder(ctx, &controlpb.CreateFolderRequest{Parent: parent, FolderId: "a/b/"}); ctlHTTP(err) != http.StatusBadRequest {
		t.Errorf("CreateFolder a/b/ without its parent = %v; want 400", err)
	}
	f, err := sc.CreateFolder(ctx, &controlpb.CreateFolderRequest{Parent: parent, FolderId: "a/b/", Recursive: true})
	if err != nil || f.GetName() != parent+"/folders/a/b/" || f.GetMetageneration() != 1 || f.GetCreateTime() == nil {
		t.Fatalf("CreateFolder recursive = %+v, %v", f, err)
	}
	if _, err := sc.CreateFolder(ctx, &controlpb.CreateFolderRequest{Parent: parent, FolderId: "a/b/"}); ctlHTTP(err) != http.StatusConflict {
		t.Errorf("a second CreateFolder = %v; want 409", err)
	}
	if got, err := sc.GetFolder(ctx, &controlpb.GetFolderRequest{Name: parent + "/folders/a/"}); err != nil || got.GetName() != parent+"/folders/a/" {
		t.Errorf("GetFolder a/ (made by recursive) = %+v, %v", got, err)
	}
	if _, err := sc.GetFolder(ctx, &controlpb.GetFolderRequest{Name: parent + "/folders/absent/"}); grpcCode(err) != codes.NotFound {
		t.Errorf("GetFolder of an absent folder = %v; want NOT_FOUND", err)
	}

	putText(t, ctx, bh, "a/b/c/d.txt")
	var names []string
	it := sc.ListFolders(ctx, &controlpb.ListFoldersRequest{Parent: parent, Prefix: "a/", PageSize: 1})
	for {
		f, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		} else if err != nil {
			t.Fatalf("ListFolders: %v", err)
		}
		names = append(names, strings.TrimPrefix(f.GetName(), parent+"/folders/"))
	}
	if strings.Join(names, ",") != "a/,a/b/,a/b/c/" {
		t.Errorf("ListFolders prefix=a/ one at a time = %v; want a/, a/b/ and a/b/c/ (made by the object write)", names)
	}

	if err := sc.DeleteFolder(ctx, &controlpb.DeleteFolderRequest{Name: parent + "/folders/a/b/c/"}); ctlHTTP(err) != http.StatusBadRequest {
		t.Errorf("DeleteFolder of a folder holding an object = %v; want 400 FAILED_PRECONDITION", err)
	}

	op, err := sc.RenameFolder(ctx, &controlpb.RenameFolderRequest{Name: parent + "/folders/a/", DestinationFolderId: "z/"})
	if err != nil {
		t.Fatalf("RenameFolder: %v", err)
	}
	renamed, err := op.Wait(ctx)
	if err != nil || renamed.GetName() != parent+"/folders/z/" {
		t.Fatalf("RenameFolder Wait = %+v, %v", renamed, err)
	}
	if md, err := op.Metadata(); err != nil || md.GetSourceFolderId() != "a/" || md.GetDestinationFolderId() != "z/" {
		t.Errorf("RenameFolder metadata = %+v, %v", md, err)
	}
	if _, err := bh.Object("z/b/c/d.txt").Attrs(ctx); err != nil {
		t.Errorf("the object after the rename: %v", err)
	}
	if _, err := bh.Object("a/b/c/d.txt").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("the old object name after the rename = %v; want not found", err)
	}
	if _, err := sc.GetFolder(ctx, &controlpb.GetFolderRequest{Name: parent + "/folders/a/b/"}); grpcCode(err) != codes.NotFound {
		t.Errorf("GetFolder a/b/ after the rename = %v; want NOT_FOUND", err)
	}

	if err := sc.DeleteFolder(ctx, &controlpb.DeleteFolderRequest{Name: parent + "/folders/z/b/c/", IfMetagenerationMatch: ptr(int64(2))}); ctlHTTP(err) != http.StatusBadRequest {
		t.Errorf("DeleteFolder non-empty = %v; want 400 FAILED_PRECONDITION", err)
	}
	dop, err := sc.DeleteFolderRecursive(ctx, &controlpb.DeleteFolderRecursiveRequest{Name: parent + "/folders/z/"})
	if err != nil {
		t.Fatalf("DeleteFolderRecursive: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteFolderRecursive Wait: %v", err)
	}
	if _, err := bh.Object("z/b/c/d.txt").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("the object after DeleteFolderRecursive = %v; want not found", err)
	}
	if it := sc.ListFolders(ctx, &controlpb.ListFoldersRequest{Parent: parent}); true {
		if f, err := it.Next(); !errors.Is(err, iterator.Done) {
			t.Errorf("ListFolders after DeleteFolderRecursive = %+v, %v; want none", f, err)
		}
	}

	flat := bucket(t, h, c)
	if _, err := sc.CreateFolder(ctx, &controlpb.CreateFolderRequest{Parent: "projects/_/buckets/" + flat.BucketName(), FolderId: "x/"}); grpcCode(err) == codes.OK {
		t.Errorf("CreateFolder on a bucket without hierarchical namespace succeeded")
	}
}

// ctlHTTP is the HTTP status of an error from the control client's REST
// transport, or 0.
func ctlHTTP(err error) int {
	if ae, ok := apierror.FromError(err); ok && ae.HTTPCode() > 0 {
		return ae.HTTPCode()
	}
	return httpCode(err)
}

func ptr[T any](v T) *T { return &v }

// TestStorageFoldersJSONAPI: the JSON API's folders, through Google's
// generated client: insert, get, list with a delimiter, delete, rename (an
// operation read back with buckets.operations.get and list) and
// deleteRecursive.
//
// covers: storage.folders.insert, storage.folders.get, storage.folders.list, storage.folders.delete, storage.folders.rename, storage.folders.deleteRecursive, storage.buckets.operations.get, storage.buckets.operations.list
func TestStorageFoldersJSONAPI(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	s := storageJSON(t, h)
	ctx := h.Context()
	bh := hnsBucket(t, h, c, s, "-hnsj")
	b := bh.BucketName()

	f, err := s.Folders.Insert(b, &storagev1.Folder{Name: "docs/"}).Context(ctx).Do()
	if err != nil || f.Name != "docs/" || f.Kind != "storage#folder" || f.Metageneration != 1 || f.Bucket != b {
		t.Fatalf("folders.insert = %+v, %v", f, err)
	}
	if _, err := s.Folders.Insert(b, &storagev1.Folder{Name: "docs/old/"}).Context(ctx).Do(); err != nil {
		t.Fatalf("folders.insert docs/old/: %v", err)
	}
	if _, err := s.Folders.Insert(b, &storagev1.Folder{Name: "docs/"}).Context(ctx).Do(); httpCode(err) != http.StatusConflict {
		t.Errorf("a second insert = %v; want 409", err)
	}
	if got, err := s.Folders.Get(b, "docs/old/").Context(ctx).Do(); err != nil || got.Name != "docs/old/" {
		t.Errorf("folders.get = %+v, %v", got, err)
	}
	if _, err := s.Folders.Get(b, "docs/old/").IfMetagenerationMatch(5).Context(ctx).Do(); httpCode(err) != http.StatusPreconditionFailed {
		t.Errorf("folders.get with a stale ifMetagenerationMatch = %v; want 412", err)
	}
	top, err := s.Folders.List(b).Delimiter("/").Context(ctx).Do()
	if err != nil || len(top.Items) != 1 || top.Items[0].Name != "docs/" {
		t.Errorf("folders.list delimiter=/ = %+v, %v; want docs/ only", top, err)
	}

	op, err := s.Folders.Rename(b, "docs/old/", "archive/").Context(ctx).Do()
	if err != nil || !op.Done || op.Name == "" {
		t.Fatalf("folders.rename = %+v, %v", op, err)
	}
	id := op.Name[strings.LastIndex(op.Name, "/")+1:]
	if got, err := s.Operations.Get(b, id).Context(ctx).Do(); err != nil || got.Name != op.Name || !got.Done {
		t.Errorf("buckets.operations.get = %+v, %v", got, err)
	}
	if ops, err := s.Operations.List(b).Context(ctx).Do(); err != nil || len(ops.Operations) != 1 {
		t.Errorf("buckets.operations.list = %+v, %v; want the rename", ops, err)
	}
	if _, err := s.Folders.Get(b, "archive/").Context(ctx).Do(); err != nil {
		t.Errorf("folders.get after the rename: %v", err)
	}

	if err := s.Folders.Delete(b, "archive/").Context(ctx).Do(); err != nil {
		t.Errorf("folders.delete of an empty folder: %v", err)
	}
	putText(t, ctx, bh, "docs/readme.txt")
	if err := s.Folders.Delete(b, "docs/").Context(ctx).Do(); httpCode(err) != http.StatusConflict {
		t.Errorf("folders.delete of a folder with an object = %v; want 409", err)
	}
	if _, err := s.Folders.DeleteRecursive(b, "docs/").Context(ctx).Do(); err != nil {
		t.Fatalf("folders.deleteRecursive: %v", err)
	}
	if _, err := s.Folders.Get(b, "docs/").Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("folders.get after deleteRecursive = %v; want 404", err)
	}
}
