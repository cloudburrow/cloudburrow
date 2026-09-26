package storage

import (
	"encoding/json"
	"sort"
	"strconv"
	"time"
)

// Object versioning (#498), to
// docs.cloud.google.com/storage/docs/object-versioning. On a bucket with
// versioning.enabled, replacing or deleting the live version keeps it as a
// noncurrent version, with its generation and a timeDeleted. Noncurrent
// versions are reached only with versions=true or generation=. Disabling
// versioning stops new ones and keeps those that exist. A delete that names
// a generation removes that version permanently.
//
// An object's noncurrent versions are one record under noncurrentPrefix,
// oldest first; the live version stays at objectKey.

const noncurrentPrefix = "noncurrent/"

func noncurrentKey(bucket, name string) string { return noncurrentPrefix + bucket + "/" + name }

func getNoncurrent(tx Tx, bucket, name string) ([]objectRecord, error) {
	raw, ok := tx.Get(noncurrentKey(bucket, name))
	if !ok {
		return nil, nil
	}
	var vs []objectRecord
	return vs, json.Unmarshal(raw, &vs)
}

func putNoncurrent(tx Tx, bucket, name string, vs []objectRecord) error {
	if len(vs) == 0 {
		tx.Delete(noncurrentKey(bucket, name))
		return nil
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].Generation < vs[j].Generation })
	raw, err := json.Marshal(vs)
	if err != nil {
		return err
	}
	tx.Put(noncurrentKey(bucket, name), raw)
	return nil
}

func versioningEnabled(b bucketRecord) bool {
	v, _ := b.Fields["versioning"].(map[string]any)
	on, _ := v["enabled"].(bool)
	return on
}

// retireLive ends the live version of bucket/name, if there is one: on a
// versioned bucket it becomes noncurrent, deleted at now; otherwise it
// leaves the bucket, soft-deleted under the bucket's policy (#499). The
// caller then writes the new live version, or nothing.
func retireLive(tx Tx, b bucketRecord, name string, now time.Time) error {
	cur, exists, err := getObject(tx, b.Name, name)
	if err != nil || !exists {
		return err
	}
	tx.Delete(objectKey(b.Name, name))
	if !versioningEnabled(b) {
		return discard(tx, b, cur, now)
	}
	vs, err := getNoncurrent(tx, b.Name, name)
	if err != nil {
		return err
	}
	cur.Deleted = now
	return putNoncurrent(tx, b.Name, name, append(vs, cur))
}

// findVersion reads the version of bucket/name that generation names: the
// live one when generation is "", else whichever version has it. live
// reports which. A generation that is not a number matches nothing.
func findVersion(tx Tx, bucket, name, generation string) (o objectRecord, live, exists bool, err error) {
	o, exists, err = getObject(tx, bucket, name)
	if err != nil || generation == "" {
		return o, exists, exists, err
	}
	if exists && generation == strconv.FormatInt(o.Generation, 10) {
		return o, true, true, nil
	}
	vs, err := getNoncurrent(tx, bucket, name)
	if err != nil {
		return objectRecord{}, false, false, err
	}
	for _, v := range vs {
		if generation == strconv.FormatInt(v.Generation, 10) {
			return v, false, true, nil
		}
	}
	return objectRecord{}, false, false, nil
}

// putVersion writes back a version findVersion returned, such as after a
// metadata patch.
func putVersion(tx Tx, o objectRecord, live bool) error {
	if live {
		return putObject(tx, o)
	}
	vs, err := getNoncurrent(tx, o.Bucket, o.Name)
	if err != nil {
		return err
	}
	for i := range vs {
		if vs[i].Generation == o.Generation {
			vs[i] = o
		}
	}
	return putNoncurrent(tx, o.Bucket, o.Name, vs)
}

// deleteVersion removes one noncurrent version permanently.
func deleteVersion(tx Tx, o objectRecord) error {
	vs, err := getNoncurrent(tx, o.Bucket, o.Name)
	if err != nil {
		return err
	}
	kept := vs[:0]
	for _, v := range vs {
		if v.Generation != o.Generation {
			kept = append(kept, v)
		}
	}
	return putNoncurrent(tx, o.Bucket, o.Name, kept)
}
