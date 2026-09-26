package storage

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/storagenotify"
)

// Object metadata changes (#492): patch (merge) and update (replace). Each
// advances the metageneration and keeps the generation and the bytes. The
// preconditions combine with AND and apply to the live generation unless the
// request names one.

// objectMutable says how each Object property is treated in a patch or
// update body.
var objectMutable = map[string]string{
	"contentType": "kept", "contentEncoding": "kept", "contentDisposition": "kept", "contentLanguage": "kept",
	"cacheControl": "kept", "metadata": "kept", "customTime": "kept",
	"storageClass":  "the storage class changes only by rewrite (#495)",
	"temporaryHold": "kept", "eventBasedHold": "kept", "retention": "kept",
	"acl": "ACL methods are not implemented", "contexts": "object contexts are not implemented",
	"kmsKeyName": "customer-managed keys are not implemented", "customerEncryption": "customer-supplied keys are not implemented",
}

// objectOutput are read-only Object properties; a body may carry them, and
// they are ignored.
var objectOutput = map[string]bool{"bucket": true, "name": true, "id": true, "kind": true, "selfLink": true, "mediaLink": true,
	"generation": true, "metageneration": true, "size": true, "etag": true, "md5Hash": true, "crc32c": true,
	"timeCreated": true, "updated": true, "timeStorageClassUpdated": true, "timeFinalized": true, "componentCount": true, "owner": true,
	"retentionExpirationTime": true, "timeDeleted": true, "softDeleteTime": true, "hardDeleteTime": true}

func checkObjectBody(body map[string]any) error {
	for k, v := range body {
		how, ok := objectMutable[k]
		switch {
		case objectOutput[k]:
		case !ok:
			return badRequest("Invalid argument: %s is not an Object field", k)
		case how == "kept", empty(v):
		default:
			return badRequest("The object field %q cannot be changed here (%s); it is refused rather than dropped", k, how)
		}
	}
	if v, ok := body["metadata"]; ok && v != nil {
		m, isMap := v.(map[string]any)
		if !isMap {
			return badRequest("Invalid argument: metadata must be an object")
		}
		for k, mv := range m {
			if _, isStr := mv.(string); mv != nil && !isStr {
				return badRequest("Invalid argument: metadata %q must be a string", k)
			}
		}
	}
	for _, k := range []string{"contentType", "contentEncoding", "contentDisposition", "contentLanguage", "cacheControl", "customTime"} {
		if v, ok := body[k]; ok && v != nil {
			if _, isStr := v.(string); !isStr {
				return badRequest("Invalid argument: %s must be a string", k)
			}
		}
	}
	return nil
}

func str(v any) string { s, _ := v.(string); return s }

// applyObjectBody writes a patch (merge) or update (replace) onto o.
func applyObjectBody(o *objectRecord, body map[string]any, replace bool) error {
	set := func(k string, dst *string) {
		if v, ok := body[k]; ok {
			*dst = str(v)
		} else if replace {
			*dst = ""
		}
	}
	set("contentType", &o.ContentType)
	set("contentEncoding", &o.ContentEncoding)
	set("contentDisposition", &o.ContentDisposition)
	set("contentLanguage", &o.ContentLanguage)
	set("cacheControl", &o.CacheControl)
	if replace && o.ContentType == "" {
		o.ContentType = "application/octet-stream"
	}
	if v, ok := body["metadata"]; ok {
		m, _ := v.(map[string]any)
		if v == nil {
			o.Metadata = nil
		} else {
			if replace || o.Metadata == nil {
				o.Metadata = map[string]string{}
			}
			for k, mv := range m {
				if mv == nil {
					delete(o.Metadata, k)
				} else {
					o.Metadata[k] = str(mv)
				}
			}
		}
	} else if replace {
		o.Metadata = nil
	}
	if v, ok := body["customTime"]; ok && v != nil {
		t, err := time.Parse(time.RFC3339Nano, str(v))
		if err != nil {
			return badRequest("Invalid argument: customTime %q is not RFC 3339", str(v))
		}
		// "customTime can't be removed and can't be set to an earlier time"
		// (docs.cloud.google.com/storage/docs/metadata#custom-time).
		if !o.CustomTime.IsZero() && t.Before(o.CustomTime) {
			return badRequest("Invalid argument: customTime cannot be set to an earlier time")
		}
		o.CustomTime = t
	} else if ok && v == nil && !o.CustomTime.IsZero() {
		return badRequest("Invalid argument: customTime cannot be removed once set")
	}
	return nil
}

func (s *Server) objectsModify(replace bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bucket, name := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
		q := r.URL.Query()
		if q.Get("predefinedAcl") != "" {
			writeError(w, errorf(http.StatusNotImplemented, "notImplemented", "predefinedAcl is not implemented: ACL methods are not implemented"))
			return
		}
		pre, err := parseObjectPreconditions(q, "if")
		if err != nil {
			writeError(w, err)
			return
		}
		body, err := readBody(r)
		if err != nil {
			writeError(w, err)
			return
		}
		// A body may restate the object's storage class (Terraform's update
		// sends the whole resource, #515); only a change needs a rewrite.
		class, restated := body["storageClass"].(string)
		if restated {
			delete(body, "storageClass")
		}
		if err := checkObjectBody(body); err != nil {
			writeError(w, err)
			return
		}
		override := q.Get("overrideUnlockedRetention") == "true"
		var o objectRecord
		err = s.meta.Update(func(tx Tx) error {
			b, ok, gerr := s.getBucket(tx, bucket)
			if gerr != nil {
				return gerr
			} else if !ok {
				return notFound("The specified bucket does not exist.")
			}
			var live, exists bool
			if o, live, exists, gerr = findVersion(tx, bucket, name, q.Get("generation")); gerr != nil {
				return gerr
			}
			if !exists {
				return notFound("No such object: %s/%s", bucket, name)
			}
			if err := pre.check(o, true, false); err != nil {
				return err
			}
			if restated && class != "" && !strings.EqualFold(class, o.StorageClass) {
				return badRequest("The object field %q cannot be changed here (%s); it is refused rather than dropped",
					"storageClass", objectMutable["storageClass"])
			}
			before := o
			if err := applyObjectBody(&o, body, replace); err != nil {
				return err
			}
			now := s.now()
			if err := applyProtection(&o, before, body, replace, override, now); err != nil {
				return err
			}
			if o.Retention != nil && before.Retention == nil && !objectRetentionEnabled(b) {
				return errorf(http.StatusBadRequest, "invalid", "The bucket %s does not have object retention enabled.", bucket)
			}
			o.Metageneration++
			o.Updated = now
			if err := putVersion(tx, o, live); err != nil {
				return err
			}
			return emit(tx, objectEvent{Type: storagenotify.EventMetadataUpdate, Object: o, Time: now})
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeResponse(w, r, http.StatusOK, s.objectJSON(r, o))
	}
}

// headerPreconditions are the HTTP ones: If-Match / If-None-Match on the
// etag, If-Modified-Since / If-Unmodified-Since on the update time, and the
// XML API's x-goog-if-generation-match and x-goog-if-metageneration-match.
func headerPreconditions(r *http.Request, o objectRecord) error {
	fail := preconditionFailed("At least one of the pre-conditions you specified did not hold.")
	notModified := errorf(http.StatusNotModified, "notModified", "not modified")
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	for name, want := range map[string]int64{"X-Goog-If-Generation-Match": o.Generation, "X-Goog-If-Metageneration-Match": o.Metageneration} {
		if v := r.Header.Get(name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return badRequest("Invalid argument: %s %q is not a number", name, v)
			}
			if n != want {
				return fail
			}
		}
	}
	etags := []string{objectETag(o), `"` + objectETag(o) + `"`, `"` + hexMD5(o) + `"`}
	matches := func(h string) bool {
		for _, t := range strings.Split(h, ",") {
			t = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "W/"))
			if t == "*" {
				return true
			}
			for _, e := range etags {
				if t == e {
					return true
				}
			}
		}
		return false
	}
	if h := r.Header.Get("If-Match"); h != "" && !matches(h) {
		return fail
	}
	if h := r.Header.Get("If-None-Match"); h != "" && matches(h) {
		if read {
			return notModified
		}
		return fail
	}
	updated := o.Updated.Truncate(time.Second)
	if h := r.Header.Get("If-Unmodified-Since"); h != "" {
		if t, err := http.ParseTime(h); err == nil && updated.After(t) {
			return fail
		}
	}
	if h := r.Header.Get("If-Modified-Since"); h != "" && read {
		if t, err := http.ParseTime(h); err == nil && !updated.After(t) {
			return notModified
		}
	}
	return nil
}
