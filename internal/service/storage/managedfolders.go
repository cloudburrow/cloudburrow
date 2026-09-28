package storage

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Managed folders (#828), to docs.cloud.google.com/storage/docs/managed-folders
// and the discovery document's managedFolders resource: insert, get, list
// and delete, and getIamPolicy, setIamPolicy and testIamPermissions on
// b/{bucket}/managedFolders/{managedFolder}/iam. A managed folder is a
// record of its own beside the bucket, keyed by bucket and name, so it
// follows the server's persistence, reset and state capture as the bucket's
// other metadata does. Its IAM policy is kept on that record and, like the
// bucket's (iam.go, ADR-0006), stored and never enforced: no read or write of
// an object consults it.
//
//   - A name is stored and returned ending in "/", as the API returns
//     managed folder names; one sent without it has it added (UNVERIFIED:
//     no page states whether Google adds it or refuses the name).
//   - A managed folder is made only in a bucket with uniform bucket-level
//     access enabled, as the managed folders page requires.
//   - insert of a name that exists is 409; get, delete and the IAM methods
//     of one that does not are 404.
//   - delete refuses, 409, a managed folder that objects (live or
//     noncurrent) or other managed folders are under, unless allowNonEmpty
//     is true; the objects are never deleted, only the managed folder and its
//     policy. ifMetagenerationMatch and ifMetagenerationNotMatch apply to get
//     and delete.
//   - list takes prefix, pageSize and pageToken, the parameters the
//     discovery document gives it; it has no delimiter.
//   - A bucket that holds a managed folder cannot be deleted (409), as one
//     that holds an object cannot.
//
// managedFolders.update, whose only settable field is rapidCacheConfig,
// stays 501, as rapidCaches do; so is an insert carrying rapidCacheConfig.

const managedFolderPrefix = "managedfolder/"

type managedFolderRecord struct {
	Bucket         string        `json:"bucket"`
	Name           string        `json:"name"`
	Created        time.Time     `json:"created"`
	Updated        time.Time     `json:"updated"`
	Metageneration int64         `json:"metageneration"`
	Policy         *bucketPolicy `json:"policy,omitempty"`
}

func managedFolderKey(bucket, name string) string { return managedFolderPrefix + bucket + "/" + name }

// maxManagedFolderName is the longest name, in bytes, as for an object.
const maxManagedFolderName = 1024

// managedFolderName validates a name and returns it ending in "/".
// UNVERIFIED: the naming rules are the object name rules the managed
// folders page points to, with no empty segment; Google's exact messages
// are not stated.
func managedFolderName(name string) (string, error) {
	if name == "" {
		return "", required("name")
	}
	if !strings.HasSuffix(name, "/") {
		name += "/"
	}
	switch {
	case len(name) > maxManagedFolderName:
		return "", badRequest("Invalid argument: a managed folder name is at most %d bytes", maxManagedFolderName)
	case !utf8.ValidString(name):
		return "", badRequest("Invalid argument: a managed folder name must be valid UTF-8")
	case strings.ContainsAny(name, "\r\n"):
		return "", badRequest("Invalid argument: a managed folder name cannot contain a carriage return or a line feed")
	case strings.HasPrefix(name, "/") || strings.Contains(name, "//"):
		return "", badRequest("Invalid argument: managed folder name %q has an empty segment", name)
	}
	for _, seg := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if seg == "." || seg == ".." {
			return "", badRequest("Invalid argument: managed folder name %q has a %q segment", name, seg)
		}
	}
	return name, nil
}

func getManagedFolder(tx Tx, bucket, name string) (managedFolderRecord, bool, error) {
	var f managedFolderRecord
	raw, ok := tx.Get(managedFolderKey(bucket, name))
	if !ok {
		return f, false, nil
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, false, err
	}
	return f, true, nil
}

func putManagedFolder(tx Tx, f managedFolderRecord) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tx.Put(managedFolderKey(f.Bucket, f.Name), raw)
	return nil
}

// hasManagedFolders says whether a bucket holds any managed folder.
func hasManagedFolders(tx Tx, bucket string) bool {
	return len(tx.List(managedFolderPrefix+bucket+"/")) > 0
}

func managedFolderMissing(bucket, name string) error {
	// UNVERIFIED: the message; the status is the JSON API's 404.
	return notFound("The managed folder %s does not exist in bucket %s.", name, bucket)
}

// existingManagedFolder reads a managed folder in a bucket that exists.
func (s *Server) existingManagedFolder(tx Tx, bucket, name string) (managedFolderRecord, error) {
	if _, ok, err := s.getBucket(tx, bucket); err != nil {
		return managedFolderRecord{}, err
	} else if !ok {
		return managedFolderRecord{}, notFound("The specified bucket does not exist.")
	}
	f, ok, err := getManagedFolder(tx, bucket, withSlash(name))
	if err != nil {
		return f, err
	} else if !ok {
		return f, managedFolderMissing(bucket, withSlash(name))
	}
	return f, nil
}

// withSlash is a path's managed folder name as it is stored.
func withSlash(name string) string {
	if name == "" || strings.HasSuffix(name, "/") {
		return name
	}
	return name + "/"
}

func (s *Server) managedFolderJSON(r *http.Request, f managedFolderRecord) map[string]any {
	return map[string]any{
		"kind": "storage#managedFolder", "id": f.Bucket + "/" + f.Name,
		"bucket": f.Bucket, "name": f.Name,
		"selfLink":       baseURL(r) + jsonPrefix + "b/" + escape(f.Bucket) + "/managedFolders/" + escape(f.Name),
		"metageneration": strconv.FormatInt(f.Metageneration, 10),
		"createTime":     rfc3339(f.Created), "updateTime": rfc3339(f.Updated),
	}
}

// uniformAccess says whether a bucket has uniform bucket-level access
// enabled, under either of the names the API gives the setting.
func uniformAccess(b bucketRecord) bool {
	cfg, _ := b.Fields["iamConfiguration"].(map[string]any)
	for _, k := range []string{"uniformBucketLevelAccess", "bucketPolicyOnly"} {
		if v, _ := cfg[k].(map[string]any); v != nil {
			if on, _ := v["enabled"].(bool); on {
				return true
			}
		}
	}
	return false
}

func (s *Server) managedFoldersInsert(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	for k, v := range body {
		switch k {
		case "name":
		case "kind", "id", "bucket", "selfLink", "metageneration", "createTime", "updateTime":
			// Output only, and ignored, as Google ignores them.
		case "rapidCacheConfig":
			if !empty(v) {
				writeError(w, errorf(http.StatusNotImplemented, "notImplemented", "rapidCacheConfig is not implemented: rapid caches are not implemented"))
				return
			}
		default:
			writeError(w, badRequest("Invalid argument: %s is not a ManagedFolder field", k))
			return
		}
	}
	raw, _ := body["name"].(string)
	name, err := managedFolderName(raw)
	if err != nil {
		writeError(w, err)
		return
	}
	now := s.now().UTC()
	f := managedFolderRecord{Bucket: bucket, Name: name, Created: now, Updated: now, Metageneration: 1}
	err = s.meta.Update(func(tx Tx) error {
		b, ok, err := s.getBucket(tx, bucket)
		if err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		if !uniformAccess(b) {
			// UNVERIFIED: the status and message; the requirement is the
			// managed folders page's.
			return badRequest("Managed folders can only be created in a bucket with uniform bucket-level access enabled; %s does not have it.", bucket)
		}
		if _, exists, err := getManagedFolder(tx, bucket, name); err != nil {
			return err
		} else if exists {
			return conflict("The managed folder %s already exists in bucket %s.", name, bucket)
		}
		return putManagedFolder(tx, f)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.managedFolderJSON(r, f))
}

func (s *Server) managedFoldersGet(w http.ResponseWriter, r *http.Request) {
	bucket, name := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	pre, err := parsePreconditions(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch")
	if err != nil {
		writeError(w, err)
		return
	}
	var f managedFolderRecord
	err = s.meta.View(func(tx Tx) error {
		var err error
		if f, err = s.existingManagedFolder(tx, bucket, name); err != nil {
			return err
		}
		return pre.check(f.Metageneration, true)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.managedFolderJSON(r, f))
}

func (s *Server) managedFoldersDelete(w http.ResponseWriter, r *http.Request) {
	bucket, name := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	pre, err := parsePreconditions(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch")
	if err != nil {
		writeError(w, err)
		return
	}
	allowNonEmpty := false
	if v := r.URL.Query().Get("allowNonEmpty"); v != "" {
		if allowNonEmpty, err = strconv.ParseBool(v); err != nil {
			writeError(w, badRequest("Invalid argument: allowNonEmpty=%q is not a boolean", v))
			return
		}
	}
	err = s.meta.Update(func(tx Tx) error {
		f, err := s.existingManagedFolder(tx, bucket, name)
		if err != nil {
			return err
		}
		if err := pre.check(f.Metageneration, false); err != nil {
			return err
		}
		if !allowNonEmpty && managedFolderInUse(tx, f) {
			// UNVERIFIED: the status and message. The discovery document
			// says only that a non-empty managed folder needs allowNonEmpty.
			return conflict("The managed folder %s is not empty: objects or managed folders are under it. Delete with allowNonEmpty=true to delete it anyway; the objects are kept.", f.Name)
		}
		tx.Delete(managedFolderKey(f.Bucket, f.Name))
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// managedFolderInUse says whether any object version, or any other managed
// folder, is under f: the discovery document's "objects or managed folders
// that it applies to".
func managedFolderInUse(tx Tx, f managedFolderRecord) bool {
	if len(tx.List(objectPrefix+f.Bucket+"/"+f.Name)) > 0 || len(tx.List(noncurrentPrefix+f.Bucket+"/"+f.Name)) > 0 {
		return true
	}
	for _, k := range tx.List(managedFolderKey(f.Bucket, f.Name)) {
		if k != managedFolderKey(f.Bucket, f.Name) {
			return true
		}
	}
	return false
}

// managedFoldersList is GET b/{bucket}/managedFolders, which gcloud storage
// also calls on every recursive rm and ls of a bucket (#517): the managed
// folders under prefix, by name, pageSize at a time (1000 when unset or
// larger). The page token is the last name returned.
func (s *Server) managedFoldersList(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	q := r.URL.Query()
	max := 1000
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, badRequest("Invalid argument: pageSize=%q", v))
			return
		}
		if n > 0 && n < max {
			max = n
		}
	}
	after := ""
	if tok := q.Get("pageToken"); tok != "" {
		b, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			writeError(w, badRequest("Invalid argument: pageToken"))
			return
		}
		after = string(b)
	}
	prefix := q.Get("prefix")
	var items []any
	next := ""
	err := s.meta.View(func(tx Tx) error {
		if _, ok, gerr := s.getBucket(tx, bucket); gerr != nil {
			return gerr
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		last := ""
		for _, k := range tx.List(managedFolderKey(bucket, prefix)) {
			name := strings.TrimPrefix(k, managedFolderPrefix+bucket+"/")
			if after != "" && name <= after {
				continue
			}
			if len(items) == max {
				next = base64.RawURLEncoding.EncodeToString([]byte(last))
				break
			}
			f, _, err := getManagedFolder(tx, bucket, name)
			if err != nil {
				return err
			}
			items, last = append(items, s.managedFolderJSON(r, f)), name
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	resp := map[string]any{"kind": "storage#managedFolders"}
	if len(items) > 0 {
		resp["items"] = items
	}
	if next != "" {
		resp["nextPageToken"] = next
	}
	writeResponse(w, r, http.StatusOK, resp)
}

func managedFolderResource(f managedFolderRecord) string {
	return "projects/_/buckets/" + f.Bucket + "/managedFolders/" + f.Name
}

func (s *Server) managedFoldersGetIamPolicy(w http.ResponseWriter, r *http.Request) {
	bucket, name := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	if err := checkPolicyVersion(r); err != nil {
		writeError(w, err)
		return
	}
	var f managedFolderRecord
	err := s.meta.View(func(tx Tx) error {
		var err error
		f, err = s.existingManagedFolder(tx, bucket, name)
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, policyDocument(managedFolderResource(f), f.Policy))
}

func (s *Server) managedFoldersSetIamPolicy(w http.ResponseWriter, r *http.Request) {
	bucket, name := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	p, sentETag, err := readPolicy(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var f managedFolderRecord
	err = s.meta.Update(func(tx Tx) error {
		var err error
		if f, err = s.existingManagedFolder(tx, bucket, name); err != nil {
			return err
		}
		next, err := nextPolicy(f.Policy, p, sentETag)
		if err != nil {
			return err
		}
		f.Policy = next
		return putManagedFolder(tx, f)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, policyDocument(managedFolderResource(f), f.Policy))
}

func (s *Server) managedFoldersTestIamPermissions(w http.ResponseWriter, r *http.Request) {
	bucket, name := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	perms := r.URL.Query()["permissions"]
	if len(perms) == 0 {
		writeError(w, required("permissions"))
		return
	}
	err := s.meta.View(func(tx Tx) error {
		_, err := s.existingManagedFolder(tx, bucket, name)
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, map[string]any{"kind": "storage#testIamPermissionsResponse", "permissions": perms})
}
