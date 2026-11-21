package main

// Restoring a soft-deleted bucket (#2047), as Google's console does it
// (https://docs.cloud.google.com/storage/docs/use-soft-deleted-buckets): a
// soft-deleted bucket's Restore opens a pane with the tabs Specify bucket
// generation (which of the name's soft-deleted generations) and Specify
// objects to restore (Don't restore objects, Restore all objects, or
// Restore objects that match criteria: a deletion time range and, with
// Filter by glob pattern, globs), an Additional options section, and
// Restore. Restore is buckets.restore of the chosen generation through
// Google's generated JSON API client; objects chosen are then restored by
// objects.bulkRestore on the restored bucket, as the console creates a bulk
// restore operation for them.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	storagev1 "google.golang.org/api/storage/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The pane's tabs and section, in Google's words.
const (
	bucketRestoreSection    = "Restore bucket"
	bucketRestoreGeneration = "Specify bucket generation"
	bucketRestoreObjects    = "Specify objects to restore"
	bucketRestoreAdditional = "Additional options"
)

// The Specify objects to restore choices.
const (
	restoreObjectsNone     = "none"
	restoreObjectsAll      = "all"
	restoreObjectsCriteria = "criteria"
)

// softGeneration is one soft-deleted generation of a bucket name.
type softGeneration struct {
	generation int64
	deleted    string
}

// bucketRestorePane is Restore on a soft-deleted bucket: its generations,
// the one the row or page stands for chosen first, newest deleted first
// after it.
func bucketRestorePane(chosen int64, gens []softGeneration) console.Action {
	sort.SliceStable(gens, func(i, j int) bool { return gens[i].deleted > gens[j].deleted })
	var options []string
	labels := map[string]string{}
	for _, g := range gens {
		id := strconv.FormatInt(g.generation, 10)
		options = append(options, id)
		labels[id] = fmt.Sprintf("Generation %s, deleted %s", id, shortTime(g.deleted))
	}
	if len(options) == 0 {
		id := strconv.FormatInt(chosen, 10)
		options, labels[id] = []string{id}, "Generation "+id
	}
	return console.Action{ID: "restore", Label: "Restore", Submit: "Restore", Fields: []console.Field{
		{Name: "generation", Label: "Bucket generation", Type: "radio", Required: true,
			Section: bucketRestoreSection, Tab: bucketRestoreGeneration,
			Default: strconv.FormatInt(chosen, 10), Options: options, OptionLabels: labels,
			Help: "The bucket's soft-deleted generations. The one restored becomes the live bucket of this name."},
		{Name: "objects", Label: "Objects to restore", Type: "radio", Required: true,
			Section: bucketRestoreSection, Tab: bucketRestoreObjects, Default: restoreObjectsNone,
			Options: []string{restoreObjectsNone, restoreObjectsAll, restoreObjectsCriteria},
			OptionLabels: map[string]string{
				restoreObjectsNone:     "Don't restore objects",
				restoreObjectsAll:      "Restore all objects",
				restoreObjectsCriteria: "Restore objects that match criteria",
			},
			Help: "Restoring the bucket does not restore its objects; they can be restored later with Create restore job."},
		{Name: "softDeletedAfterTime", Label: "Deleted after", Type: "text",
			Section: bucketRestoreSection, Tab: bucketRestoreObjects, ShowWhen: "objects=" + restoreObjectsCriteria,
			Help: "The start of the time range to restore objects from, an RFC 3339 time (2026-10-15T00:00:00Z); empty for no lower bound."},
		{Name: "softDeletedBeforeTime", Label: "Deleted before", Type: "text",
			Section: bucketRestoreSection, Tab: bucketRestoreObjects, ShowWhen: "objects=" + restoreObjectsCriteria,
			Help: "The end of the time range, an RFC 3339 time; empty for no upper bound."},
		{Name: "filterByGlob", Label: "Filter by glob pattern", Type: "checkbox", Default: "false",
			Section: bucketRestoreSection, Tab: bucketRestoreObjects, ShowWhen: "objects=" + restoreObjectsCriteria},
		{Name: "matchGlobs", Label: "Glob pattern", Type: "text",
			Section: bucketRestoreSection, Tab: bucketRestoreObjects, ShowWhen: "objects=" + restoreObjectsCriteria + "&filterByGlob",
			Help: "Comma-separated globs in objects.list's matchGlob syntax (logs/**, *.png)."},
		{Name: "allowOverwrite", Label: "Allow overwrite", Type: "checkbox", Default: "false", Section: bucketRestoreAdditional,
			Help: "For objects restored: off, a name with a live object is skipped; on, the restored version replaces it."},
		{Name: "copySourceAcl", Label: "Copy source ACL", Type: "checkbox", Default: "false", Section: bucketRestoreAdditional,
			Help: "For objects restored: keep each version's ACL; off, it takes the bucket's default object ACL."},
	}}
}

// softGenerations are the soft-deleted generations of the bucket name.
func softGenerations(ctx context.Context, s *storagev1.Service, project, name string) ([]softGeneration, error) {
	var out []softGeneration
	err := s.Buckets.List(project).SoftDeleted(true).Prefix(name).Context(ctx).Pages(ctx, func(page *storagev1.Buckets) error {
		for _, b := range page.Items {
			if b.Name == name {
				out = append(out, softGeneration{generation: b.Generation, deleted: b.SoftDeleteTime})
			}
		}
		return nil
	})
	return out, err
}

// restoreBucket restores generation gen of the bucket name, then the
// objects the pane chose, through one objects.bulkRestore.
func restoreBucket(ctx context.Context, s *storagev1.Service, name string, gen int64, values map[string]string) error {
	if v := strings.TrimSpace(values["generation"]); v != "" {
		chosen, err := strconv.ParseInt(v, 10, 64)
		if err != nil || chosen <= 0 {
			return fmt.Errorf("generation %q is not a number", v)
		}
		gen = chosen
	}
	req := &storagev1.BulkRestoreObjectsRequest{
		AllowOverwrite: values["allowOverwrite"] == "true",
		CopySourceAcl:  values["copySourceAcl"] == "true",
	}
	switch objects := strings.TrimSpace(values["objects"]); objects {
	case "", restoreObjectsNone:
		req = nil
	case restoreObjectsAll:
	case restoreObjectsCriteria:
		req.SoftDeletedAfterTime = strings.TrimSpace(values["softDeletedAfterTime"])
		req.SoftDeletedBeforeTime = strings.TrimSpace(values["softDeletedBeforeTime"])
		if values["filterByGlob"] == "true" {
			for _, g := range strings.Split(values["matchGlobs"], ",") {
				if g = strings.TrimSpace(g); g != "" {
					req.MatchGlobs = append(req.MatchGlobs, g)
				}
			}
			if len(req.MatchGlobs) == 0 {
				return errors.New("enter a glob pattern, or clear Filter by glob pattern")
			}
		}
	default:
		return fmt.Errorf("%q is not a choice of objects to restore", objects)
	}
	if _, err := s.Buckets.Restore(name, gen).Context(ctx).Do(); err != nil {
		return err
	}
	if req == nil {
		return nil
	}
	if _, err := s.Objects.BulkRestore(name, req).Context(ctx).Do(); err != nil {
		return fmt.Errorf("the bucket is restored, but restoring its objects failed: %w", err)
	}
	return nil
}
