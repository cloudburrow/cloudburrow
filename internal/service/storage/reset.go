package storage

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Reset (#510) clears the server's state from its store directly, not
// through the API: a delete through the API would soft-delete (#499), keep
// noncurrent versions (#498), refuse held or retained objects (#500) and
// emit notifications (#506), none of which a reset wants. Buckets record
// their project, so a reset can be confined to one. The admin API reaches
// it at POST /_cloudburrow/reset[?project=], which cannot collide with the
// XML API since no bucket name begins with "_". Blob bytes are
// content-addressed and left in the blob store.

const resetPath = "/_cloudburrow/reset"

// bucketScoped are the prefixes keyed by bucket name: <prefix><bucket>/...
var bucketScoped = []string{objectPrefix, noncurrentPrefix, softObjectPrefix, notificationPrefix, mpuPrefix}

// everyPrefix is all the state the server keeps.
var everyPrefix = []string{bucketPrefix, objectPrefix, noncurrentPrefix, softObjectPrefix, softBucketPrefix,
	sessionPrefix, rewritePrefix, notificationPrefix, outboxPrefix, mpuPrefix, hmacPrefix}

// Reset removes every bucket, object version (live, noncurrent and
// soft-deleted), soft-deleted bucket, upload session, rewrite token,
// notification configuration and undelivered event, multipart upload, IAM
// policy (kept on its bucket) and HMAC key.
func (s *Server) Reset() error {
	return s.meta.Update(func(tx Tx) error {
		for _, p := range everyPrefix {
			for _, k := range tx.List(p) {
				tx.Delete(k)
			}
		}
		return nil
	})
}

// ResetProject removes one project's buckets, with everything in and about
// them, its soft-deleted buckets and its HMAC keys, and leaves every other
// project alone.
func (s *Server) ResetProject(project string) error {
	return s.meta.Update(func(tx Tx) error {
		gone := map[string]bool{}
		for _, k := range tx.List(bucketPrefix) {
			raw, _ := tx.Get(k)
			var b bucketRecord
			if err := json.Unmarshal(raw, &b); err != nil {
				return err
			}
			if b.Project == project {
				gone[b.Name] = true
				tx.Delete(k)
			}
		}
		for _, k := range tx.List(softBucketPrefix) {
			raw, _ := tx.Get(k)
			var b bucketRecord
			if err := json.Unmarshal(raw, &b); err != nil {
				return err
			}
			if b.Project == project {
				gone[b.Name] = true
				tx.Delete(k)
			}
		}
		for name := range gone {
			for _, p := range bucketScoped {
				for _, k := range tx.List(p + name + "/") {
					tx.Delete(k)
				}
			}
			tx.Delete(notificationPrefix + name + ".seq")
		}
		for _, k := range tx.List(sessionPrefix) {
			raw, _ := tx.Get(k)
			var u uploadSession
			if json.Unmarshal(raw, &u) == nil && gone[u.Bucket] {
				tx.Delete(k)
			}
		}
		for _, k := range tx.List(rewritePrefix) {
			raw, _ := tx.Get(k)
			var st rewriteState
			if json.Unmarshal(raw, &st) == nil && (gone[st.DstBucket] || gone[st.Source.Bucket]) {
				tx.Delete(k)
			}
		}
		for _, k := range tx.List(outboxPrefix) {
			raw, _ := tx.Get(k)
			var e outboxEntry
			if json.Unmarshal(raw, &e) == nil && gone[e.Attributes["bucketId"]] {
				tx.Delete(k)
			}
		}
		for _, k := range tx.List(hmacPrefix + project + "/") {
			tx.Delete(k)
		}
		return nil
	})
}

// serveReset is POST /_cloudburrow/reset[?project=].
func (s *Server) serveReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, errorf(http.StatusMethodNotAllowed, "methodNotAllowed", "POST %s resets the server", resetPath))
		return
	}
	var err error
	if p := strings.TrimSpace(r.URL.Query().Get("project")); p != "" {
		err = s.ResetProject(p)
	} else {
		err = s.Reset()
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
