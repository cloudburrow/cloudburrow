package storage

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// objects.bulkRestore (#1279), to the BulkRestoreObjectsRequest reference
// and docs.cloud.google.com/storage/docs/use-soft-deleted-objects. It
// restores, in one transaction, the bucket's soft-deleted objects that
// match every condition given:
//
//   - matchGlobs: any one of them, in objects.list's matchGlob syntax;
//   - softDeletedAfterTime / softDeletedBeforeTime: when it was deleted;
//   - createdAfterTime / createdBeforeTime: when its version was created.
//
// "If multiple generations of an object match the criteria, only the
// latest generation is restored and the rest are skipped" (the
// bulkRestore reference, read 2026-10-17): the matching version with the
// highest generation is restored. Without allowOverwrite, a name that has
// a live version is skipped; with it, the live version is retired as any
// overwrite is. copySourceAcl is objects.restore's. A version the bucket
// refuses (retention on an object of a bucket without object retention)
// is counted failed. Each is restored as objects.restore does it: a new
// generation, metageneration 1.
//
// The answer is a long-running operation, done when it is returned, kept
// with the bucket's folder operations for buckets.operations.get and list.
// Its BulkRestoreObjectsMetadata is the reference's: the request's
// allowOverwrite, matchGlobs, the four times and copySourceAcl, echoed,
// and succeededCount (objects restored), skippedCount ("the number of
// objects skipped as a result of the specified" filters: every
// soft-deleted version the time and glob filters leave out, each
// generation past the latest, and a name with a live version left alone)
// and failedCount. commonMetadata (type BULK_RESTORE_OBJECTS) is the
// other bucket operations' and is not on that page (UNVERIFIED).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/json_api/v1/objects/bulkRestore

const bulkRestoreType = "BULK_RESTORE_OBJECTS"

// bulkRestoreCounts are what one bulk restore did, and the request it
// echoes.
type bulkRestoreCounts struct {
	Restored int64               `json:"restored"`
	Skipped  int64               `json:"skipped"`
	Failed   int64               `json:"failed"`
	Request  *bulkRestoreRequest `json:"request,omitempty"`
}

type bulkRestoreRequest struct {
	AllowOverwrite        bool     `json:"allowOverwrite"`
	CopySourceACL         bool     `json:"copySourceAcl"`
	MatchGlobs            []string `json:"matchGlobs"`
	SoftDeletedAfterTime  string   `json:"softDeletedAfterTime"`
	SoftDeletedBeforeTime string   `json:"softDeletedBeforeTime"`
	CreatedAfterTime      string   `json:"createdAfterTime"`
	CreatedBeforeTime     string   `json:"createdBeforeTime"`
}

// bulkRestoreFilter is a parsed request's conditions.
type bulkRestoreFilter struct {
	globs                         []*regexp.Regexp
	deletedAfter, deletedBefore   time.Time
	createdAfter, createdBefore   time.Time
	allowOverwrite, copySourceACL bool
	// req is the request as sent, for the metadata's echo.
	req bulkRestoreRequest
}

func parseBulkRestore(r *http.Request) (bulkRestoreFilter, error) {
	var f bulkRestoreFilter
	body, err := readBody(r)
	if err != nil {
		return f, err
	}
	raw, _ := json.Marshal(body)
	var req bulkRestoreRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return f, badRequest("Invalid argument: the request body is not a BulkRestoreObjectsRequest: %v", err)
	}
	for k := range body {
		switch k {
		case "allowOverwrite", "copySourceAcl", "matchGlobs", "softDeletedAfterTime", "softDeletedBeforeTime",
			"createdAfterTime", "createdBeforeTime":
		default:
			return f, badRequest("Invalid argument: %s is not a BulkRestoreObjectsRequest field", k)
		}
	}
	f.allowOverwrite, f.copySourceACL, f.req = req.AllowOverwrite, req.CopySourceACL, req
	for _, g := range req.MatchGlobs {
		re, err := globRE(g)
		if err != nil {
			return f, err
		}
		f.globs = append(f.globs, re)
	}
	for _, t := range []struct {
		name, value string
		into        *time.Time
	}{
		{"softDeletedAfterTime", req.SoftDeletedAfterTime, &f.deletedAfter},
		{"softDeletedBeforeTime", req.SoftDeletedBeforeTime, &f.deletedBefore},
		{"createdAfterTime", req.CreatedAfterTime, &f.createdAfter},
		{"createdBeforeTime", req.CreatedBeforeTime, &f.createdBefore},
	} {
		if t.value == "" {
			continue
		}
		v, err := time.Parse(time.RFC3339Nano, t.value)
		if err != nil {
			return f, badRequest("Invalid argument: %s %q is not an RFC 3339 time", t.name, t.value)
		}
		*t.into = v
	}
	return f, nil
}

// matches says whether a soft-deleted version meets every condition.
func (f bulkRestoreFilter) matches(o objectRecord) bool {
	if len(f.globs) > 0 {
		hit := false
		for _, re := range f.globs {
			if re.MatchString(o.Name) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return within(o.SoftDeleted, f.deletedAfter, f.deletedBefore) && within(o.Created, f.createdAfter, f.createdBefore)
}

// within is after < t < before, each bound only when set.
func within(t, after, before time.Time) bool {
	return (after.IsZero() || t.After(after)) && (before.IsZero() || t.Before(before))
}

func (s *Server) objectsBulkRestore(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	f, err := parseBulkRestore(r)
	if err != nil {
		writeError(w, err)
		return
	}
	now := s.now().UTC()
	op := operationRecord{Bucket: bucket, ID: newOperationID(), Type: bulkRestoreType, Created: now, Ended: now,
		Restore: &bulkRestoreCounts{Request: &f.req}}
	err = s.meta.Update(func(tx Tx) error {
		b, ok, err := s.getBucket(tx, bucket)
		if err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		var names []string
		for _, key := range tx.List(softObjectPrefix + bucket + "/") {
			names = append(names, strings.TrimPrefix(key, softObjectPrefix+bucket+"/"))
		}
		sort.Strings(names)
		for _, name := range names {
			if err := s.bulkRestoreName(tx, b, name, f, op.Restore, now); err != nil {
				return err
			}
		}
		return putOperation(tx, op)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.operationJSON(r, op))
}

// bulkRestoreName restores the latest matching soft-deleted generation of
// one name, and counts what it did: every other soft-deleted version of
// the name is skipped, whether the filters left it out or a later
// generation matched.
func (s *Server) bulkRestoreName(tx Tx, b bucketRecord, name string, f bulkRestoreFilter, n *bulkRestoreCounts, now time.Time) error {
	vs, err := softVersions(tx, b, name, now)
	if err != nil {
		return err
	}
	var pick *objectRecord
	for i := range vs {
		if f.matches(vs[i]) && (pick == nil || vs[i].Generation > pick.Generation) {
			pick = &vs[i]
		}
	}
	if pick == nil {
		n.Skipped += int64(len(vs))
		return nil
	}
	n.Skipped += int64(len(vs) - 1)
	soft := *pick
	cur, exists, err := getObject(tx, b.Name, name)
	if err != nil {
		return err
	}
	if exists && !f.allowOverwrite {
		n.Skipped++
		return nil
	}
	if _, err := s.restoreSoft(tx, b, soft, cur, exists, f.copySourceACL, now); err != nil {
		if _, refused := err.(*httpError); refused {
			n.Failed++
			return nil
		}
		return err
	}
	n.Restored++
	return nil
}
