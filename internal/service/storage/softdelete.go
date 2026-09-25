package storage

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// Soft delete (#499), to docs.cloud.google.com/storage/docs/soft-delete.
// Under a bucket's soft delete policy (7 days by default, 7 to 90 days, or 0
// for none), a version that leaves the bucket (deleted or overwritten, and
// not kept noncurrent by versioning) is kept soft-deleted until its
// hardDeleteTime: hidden from reads and listings, listable with
// softDeleted=true and restorable as a new generation. A deleted bucket is
// kept the same way under its own policy, by generation, and restores empty.
// A policy change applies only to what is deleted after it. Sweep removes
// what has passed its hardDeleteTime; reads treat it as gone before then.
//
// An object's soft-deleted versions are one record under softObjectPrefix,
// oldest first, each tagged with the bucket generation it was deleted from,
// so a later bucket of the same name does not see them. A soft-deleted
// bucket is one record under softBucketPrefix/<name>/<generation>.

const (
	softObjectPrefix = "softdeleted/"
	softBucketPrefix = "softbucket/"
	minSoftRetention = 7 * 24 * time.Hour
	maxSoftRetention = 90 * 24 * time.Hour
)

func softObjectKey(bucket, name string) string { return softObjectPrefix + bucket + "/" + name }

func softBucketKey(name string, gen int64) string {
	return softBucketPrefix + name + "/" + strconv.FormatInt(gen, 10)
}

// softRetention is the bucket's soft delete retention; 0 means none.
func softRetention(b bucketRecord) time.Duration {
	p, _ := b.Fields["softDeletePolicy"].(map[string]any)
	n, _ := strconv.ParseInt(numberString(p["retentionDurationSeconds"]), 10, 64)
	return time.Duration(n) * time.Second
}

// numberString reads an int64 the API sends as a string, or as a number.
func numberString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatInt(int64(x), 10)
	}
	return ""
}

// checkSoftDeletePolicy validates softDeletePolicy.retentionDurationSeconds:
// 0, or 7 to 90 days (soft-delete docs).
func checkSoftDeletePolicy(f map[string]any) error {
	p, ok := f["softDeletePolicy"].(map[string]any)
	if !ok || p["retentionDurationSeconds"] == nil {
		return nil
	}
	s := numberString(p["retentionDurationSeconds"])
	n, err := strconv.ParseInt(s, 10, 64)
	d := time.Duration(n) * time.Second
	if err != nil || n != 0 && (d < minSoftRetention || d > maxSoftRetention) {
		return badRequest("Invalid argument: softDeletePolicy.retentionDurationSeconds %q must be 0, or between 604800 (7 days) and 7776000 (90 days)", s)
	}
	p["retentionDurationSeconds"] = strconv.FormatInt(n, 10)
	return nil
}

// stampSoftDeletePolicy sets effectiveTime when the retention is new or
// changed; the client never sets it (it is output only).
func stampSoftDeletePolicy(next, old map[string]any, now time.Time) {
	p, ok := next["softDeletePolicy"].(map[string]any)
	if !ok {
		return
	}
	prev, _ := old["softDeletePolicy"].(map[string]any)
	if prev != nil && numberString(prev["retentionDurationSeconds"]) == numberString(p["retentionDurationSeconds"]) && prev["effectiveTime"] != nil {
		p["effectiveTime"] = prev["effectiveTime"]
		return
	}
	p["effectiveTime"] = rfc3339(now)
}

// copyMap is a shallow copy of a JSON object, nil for anything else.
func copyMap(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, e := range m {
		out[k] = e
	}
	return out
}

func getSoftDeleted(tx Tx, bucket, name string) ([]objectRecord, error) {
	raw, ok := tx.Get(softObjectKey(bucket, name))
	if !ok {
		return nil, nil
	}
	var vs []objectRecord
	return vs, json.Unmarshal(raw, &vs)
}

func putSoftDeleted(tx Tx, bucket, name string, vs []objectRecord) error {
	if len(vs) == 0 {
		tx.Delete(softObjectKey(bucket, name))
		return nil
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].Generation < vs[j].Generation })
	raw, err := json.Marshal(vs)
	if err != nil {
		return err
	}
	tx.Put(softObjectKey(bucket, name), raw)
	return nil
}

// discard is the end of a version that leaves bucket b: under a soft delete
// policy it is kept soft-deleted until now plus the retention; otherwise it
// is gone. Its bytes are content-addressed and stay in the blob store.
func discard(tx Tx, b bucketRecord, o objectRecord, now time.Time) error {
	d := softRetention(b)
	if d == 0 {
		return nil
	}
	vs, err := getSoftDeleted(tx, b.Name, o.Name)
	if err != nil {
		return err
	}
	o.SoftDeleted, o.HardDelete, o.BucketGeneration = now, now.Add(d), b.Generation
	return putSoftDeleted(tx, b.Name, o.Name, append(vs, o))
}

// softVersions are the soft-deleted versions of bucket/name that are still
// restorable: deleted from this generation of the bucket, and not past their
// hardDeleteTime.
func softVersions(tx Tx, b bucketRecord, name string, now time.Time) ([]objectRecord, error) {
	vs, err := getSoftDeleted(tx, b.Name, name)
	if err != nil {
		return nil, err
	}
	out := vs[:0]
	for _, v := range vs {
		if v.BucketGeneration == b.Generation && now.Before(v.HardDelete) {
			out = append(out, v)
		}
	}
	return out, nil
}

func findSoftDeleted(tx Tx, b bucketRecord, name, generation string, now time.Time) (objectRecord, bool, error) {
	vs, err := softVersions(tx, b, name, now)
	if err != nil {
		return objectRecord{}, false, err
	}
	for _, v := range vs {
		if strconv.FormatInt(v.Generation, 10) == generation {
			return v, true, nil
		}
	}
	return objectRecord{}, false, nil
}

// getSoftDeletedObject is objects.get with softDeleted=true: the metadata of
// one soft-deleted version, named by generation. Its bytes cannot be read
// (soft-delete docs: soft-deleted objects "cannot be read").
func (s *Server) getSoftDeletedObject(w http.ResponseWriter, r *http.Request, bucket, name string) {
	q := r.URL.Query()
	gen := q.Get("generation")
	if gen == "" {
		writeError(w, required("generation"))
		return
	}
	if q.Get("alt") == "media" {
		writeError(w, badRequest("A soft-deleted object cannot be read; restore it first"))
		return
	}
	var o objectRecord
	err := s.meta.View(func(tx Tx) error {
		b, ok, err := s.getBucket(tx, bucket)
		if err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		found := false
		if o, found, err = findSoftDeleted(tx, b, name, gen, s.now()); err != nil {
			return err
		} else if !found {
			return notFound("No such object: %s/%s", bucket, name)
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.objectJSON(r, o))
}

// objectsRestore is objects.restore: a soft-deleted version becomes the
// live one as a new generation with metageneration 1; a live version it
// replaces is retired as any overwrite is (soft-delete docs: it "is then
// automatically soft-deleted"). The 412 reasons are the status-codes page's.
func (s *Server) objectsRestore(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bucket, name := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	gen := q.Get("generation")
	if gen == "" {
		writeError(w, required("generation"))
		return
	}
	if q.Get("copySourceAcl") == "true" {
		writeError(w, badRequest("copySourceAcl is not supported by CloudBurrow: ACL methods are not implemented; it is refused rather than ignored"))
		return
	}
	if q.Get("restoreToken") != "" {
		writeError(w, badRequest("restoreToken is not supported by CloudBurrow: hierarchical namespace buckets are not implemented (#503)"))
		return
	}
	pre, err := parseObjectPreconditions(q, "if")
	if err != nil {
		writeError(w, err)
		return
	}
	var o objectRecord
	err = s.meta.Update(func(tx Tx) error {
		b, ok, err := s.getBucket(tx, bucket)
		if err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		now := s.now()
		soft, found, err := findSoftDeleted(tx, b, name, gen, now)
		if err != nil {
			return err
		}
		if !found {
			if softRetention(b) == 0 {
				return errorf(http.StatusPreconditionFailed, "softDeletePolicyNotSet", "Bucket does not have a soft delete policy.")
			}
			if _, _, exists, err := findVersion(tx, bucket, name, gen); err != nil {
				return err
			} else if exists {
				return errorf(http.StatusPreconditionFailed, "objectNotSoftDeleted", "Object is not soft deleted and is either live or noncurrent.")
			}
			return notFound("No such object: %s/%s", bucket, name)
		}
		cur, exists, err := getObject(tx, bucket, name)
		if err != nil {
			return err
		}
		if err := pre.check(cur, exists, false); err != nil {
			return err
		}
		vs, err := getSoftDeleted(tx, bucket, name)
		if err != nil {
			return err
		}
		kept := vs[:0]
		for _, v := range vs {
			if v.Generation != soft.Generation || v.BucketGeneration != soft.BucketGeneration {
				kept = append(kept, v)
			}
		}
		if err := putSoftDeleted(tx, bucket, name, kept); err != nil {
			return err
		}
		o = soft
		fresh(&o)
		if err := checkNewObject(b, &o); err != nil {
			return err
		}
		o.Generation, o.Metageneration, o.Created, o.Updated = s.nextGeneration(), 1, now, now
		o.Deleted, o.SoftDeleted, o.HardDelete, o.BucketGeneration = time.Time{}, time.Time{}, time.Time{}, 0
		if err := retireLive(tx, b, name, now, o.Generation); err != nil {
			return err
		}
		return finalized(tx, o, cur, exists, now)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.objectJSON(r, o))
}

// softDeleteBucket keeps a deleted bucket under its own policy.
func softDeleteBucket(tx Tx, b bucketRecord, now time.Time) error {
	d := softRetention(b)
	if d == 0 {
		return nil
	}
	b.SoftDeleted, b.HardDelete = now, now.Add(d)
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	tx.Put(softBucketKey(b.Name, b.Generation), raw)
	return nil
}

func (s *Server) getSoftBucket(tx Tx, name, gen string) (bucketRecord, bool, error) {
	var b bucketRecord
	n, err := strconv.ParseInt(gen, 10, 64)
	if err != nil {
		return b, false, nil
	}
	raw, ok := tx.Get(softBucketKey(name, n))
	if !ok {
		return b, false, nil
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, false, err
	}
	return b, s.now().Before(b.HardDelete), nil
}

// bucketsGetSoftDeleted is buckets.get with softDeleted=true, which needs
// the generation (discovery document).
func (s *Server) bucketsGetSoftDeleted(w http.ResponseWriter, r *http.Request, name string) {
	gen := r.URL.Query().Get("generation")
	if gen == "" {
		writeError(w, required("generation"))
		return
	}
	var b bucketRecord
	err := s.meta.View(func(tx Tx) error {
		var ok bool
		var err error
		if b, ok, err = s.getSoftBucket(tx, name, gen); err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.bucketJSON(r, b))
}

// softBuckets are a project's restorable soft-deleted buckets under prefix,
// by name then generation.
func (s *Server) softBuckets(tx Tx, project, prefix string) ([]bucketRecord, error) {
	var out []bucketRecord
	now := s.now()
	for _, key := range tx.List(softBucketPrefix + prefix) {
		raw, _ := tx.Get(key)
		var b bucketRecord
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		if b.Project == project && now.Before(b.HardDelete) {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Generation < out[j].Generation
	})
	return out, nil
}

// bucketsRestore is buckets.restore: the soft-deleted bucket of that
// generation comes back, empty of live objects (soft-delete docs: restoring
// a bucket "restores only the empty bucket"); its soft-deleted objects are
// restorable again. A live bucket of the same name refuses it with 409
// (UNVERIFIED: no page states the code).
func (s *Server) bucketsRestore(w http.ResponseWriter, r *http.Request) {
	name := pathVar(r, jsonPrefix, 1)
	gen := r.URL.Query().Get("generation")
	if gen == "" {
		writeError(w, required("generation"))
		return
	}
	var b bucketRecord
	err := s.meta.Update(func(tx Tx) error {
		var ok bool
		var err error
		if b, ok, err = s.getSoftBucket(tx, name, gen); err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		if _, live, err := s.getBucket(tx, name); err != nil {
			return err
		} else if live {
			return conflict("A bucket named %s already exists; delete it before restoring this one.", name)
		}
		tx.Delete(softBucketKey(name, b.Generation))
		b.SoftDeleted, b.HardDelete = time.Time{}, time.Time{}
		b.Updated = s.now()
		return putBucket(tx, b)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.bucketJSON(r, b))
}

// Sweep permanently removes the soft-deleted objects and buckets whose
// hardDeleteTime has passed, and returns the next hardDeleteTime, or zero
// if nothing is waiting.
func (s *Server) Sweep() (time.Time, error) {
	now := s.now()
	var next time.Time
	later := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	err := s.meta.Update(func(tx Tx) error {
		for _, key := range tx.List(softObjectPrefix) {
			raw, _ := tx.Get(key)
			var vs []objectRecord
			if err := json.Unmarshal(raw, &vs); err != nil {
				return err
			}
			kept := vs[:0]
			for _, v := range vs {
				if now.Before(v.HardDelete) {
					kept = append(kept, v)
					later(v.HardDelete)
				}
			}
			if len(kept) == len(vs) {
				continue
			}
			bucket, name, _ := strings.Cut(strings.TrimPrefix(key, softObjectPrefix), "/")
			if err := putSoftDeleted(tx, bucket, name, kept); err != nil {
				return err
			}
		}
		for _, key := range tx.List(softBucketPrefix) {
			raw, _ := tx.Get(key)
			var b bucketRecord
			if err := json.Unmarshal(raw, &b); err != nil {
				return err
			}
			if now.Before(b.HardDelete) {
				later(b.HardDelete)
				continue
			}
			tx.Delete(key)
		}
		return nil
	})
	return next, err
}

// Run sweeps at each hardDeleteTime and applies the lifecycle rules (#501)
// until ctx is done, at least hourly, so a deletion made meanwhile is swept
// on time and a rule's lag stays under an hour.
func (s *Server) Run(ctx context.Context, clock sched.Clock) {
	if s.notify != nil {
		go s.runNotifier(ctx)
	}
	retry := time.Second
	for {
		_, lerr := s.ApplyLifecycle()
		next, err := s.Sweep()
		if err == nil {
			err = lerr
		}
		wait := time.Hour
		switch {
		case err != nil:
			wait = retry
			if retry *= 2; retry > time.Minute {
				retry = time.Minute
			}
		case !next.IsZero():
			retry = time.Second
			if d := next.Sub(clock.Now()); d < wait {
				wait = d
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-clock.After(wait):
		}
	}
}
