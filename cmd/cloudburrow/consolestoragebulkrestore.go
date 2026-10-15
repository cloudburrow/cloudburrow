package main

// Bulk restore in the console (#1279). A bucket's page offers Bulk restore
// deleted objects, which is objects.bulkRestore through Google's generated
// JSON API client (the official storage client has no call for it): globs,
// a soft-deleted-after and -before window, Allow overwrite and Copy source
// ACL, as BulkRestoreObjectsRequest has them. The dialog answers with the
// operation and its counts, and the bucket's Operations tab lists it,
// which a bucket shows once it has an operation, hierarchical namespace or
// not.

import (
	"context"
	"errors"
	"strings"

	storagev1 "google.golang.org/api/storage/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// bulkRestoreAction is Bulk restore deleted objects, on a bucket's page.
func bulkRestoreAction() console.Action {
	return console.Action{
		ID: "bulkrestore", Label: "Bulk restore deleted objects",
		Fields: []console.Field{
			{Name: "matchGlobs", Label: "Match globs", Type: "text",
				Help: "Comma-separated globs in objects.list's matchGlob syntax (logs/**, *.png); empty restores every soft-deleted object."},
			{Name: "softDeletedAfterTime", Label: "Deleted after", Type: "text",
				Help: "An RFC 3339 time (2026-10-15T00:00:00Z); empty for no lower bound."},
			{Name: "softDeletedBeforeTime", Label: "Deleted before", Type: "text",
				Help: "An RFC 3339 time; empty for no upper bound."},
			{Name: "allowOverwrite", Label: "Allow overwrite", Type: "checkbox", Default: "false",
				Help: "Off, a name with a live object is skipped; on, the restored version replaces it."},
			{Name: "copySourceAcl", Label: "Copy source ACL", Type: "checkbox", Default: "false",
				Help: "Keep each version's ACL; off, it takes the bucket's default object ACL."},
		},
	}
}

// bulkRestore is objects.bulkRestore on the bucket the path names.
func (p storageProvider) bulkRestore(ctx context.Context, path []string, values map[string]string) (*console.Listing, error) {
	if len(path) != 1 || strings.HasPrefix(path[0], "_") {
		return nil, errors.New("a bulk restore is made on a bucket's page")
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return nil, err
	}
	req := &storagev1.BulkRestoreObjectsRequest{
		AllowOverwrite:        values["allowOverwrite"] == "true",
		CopySourceAcl:         values["copySourceAcl"] == "true",
		SoftDeletedAfterTime:  strings.TrimSpace(values["softDeletedAfterTime"]),
		SoftDeletedBeforeTime: strings.TrimSpace(values["softDeletedBeforeTime"]),
	}
	for _, g := range strings.Split(values["matchGlobs"], ",") {
		if g = strings.TrimSpace(g); g != "" {
			req.MatchGlobs = append(req.MatchGlobs, g)
		}
	}
	op, err := s.Objects.BulkRestore(path[0], req).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return operationResult(op), nil
}
