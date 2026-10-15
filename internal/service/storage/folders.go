package storage

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Hierarchical-namespace folders (#1203), to
// docs.cloud.google.com/storage/docs/hns-overview, .../create-folders,
// .../rename-folders and .../delete-folders, and the discovery document's
// folders and buckets.operations resources. The same folders are served on
// the Storage Control API's REST surface (/v2/projects/_/buckets/...,
// control.go), which cloud.google.com/go/storage/control/apiv2's REST client
// calls.
//
//   - Folders exist only in a bucket created with hierarchicalNamespace
//     enabled, which needs uniform bucket-level access, and the setting is
//     fixed at creation (hns-overview). A folder call on any other bucket is
//     400 (UNVERIFIED: the status and message).
//   - A folder name ends in "/"; one sent without it has it added
//     (UNVERIFIED). A folder's parent must exist unless recursive is true,
//     which creates the missing ones (create-folders); a missing parent is
//     400 failedPrecondition (UNVERIFIED).
//   - Writing an object creates the folders in its path that are missing,
//     as the HNS overview says; deleting an object leaves them.
//   - delete refuses, 409, a folder that holds objects or folders
//     (delete-folders: only an empty folder is deleted; the status is
//     UNVERIFIED). deleteRecursive deletes the folder, the folders under it
//     and every live object under it, each by the bucket's soft-delete
//     policy, as objects.delete does.
//   - rename moves the folder, its folders and its objects to the new name
//     in one transaction and returns a long-running operation, as Google's
//     does; here it is done when it is returned, so pendingRenameInfo is
//     never set. Objects keep their generation (UNVERIFIED). The
//     destination must not exist (409), must not be inside the source
//     (400), and its parent must exist (400 failedPrecondition, UNVERIFIED).
//   - Operations are kept by bucket and read with buckets.operations.get and
//     list. cancel stays 501: an operation here is done when it is returned,
//     so there is nothing to cancel. advanceRelocateBucket stays 501, as
//     buckets.relocate does.

const (
	folderPrefix    = "folder/"
	operationPrefix = "operation/"
)

type folderRecord struct {
	Bucket         string    `json:"bucket"`
	Name           string    `json:"name"`
	Created        time.Time `json:"created"`
	Updated        time.Time `json:"updated"`
	Metageneration int64     `json:"metageneration"`
}

// operationRecord is a folder operation or a bulk restore (#1279), done
// when it is recorded.
type operationRecord struct {
	Bucket  string        `json:"bucket"`
	ID      string        `json:"id"`
	Type    string        `json:"type"` // RENAME_FOLDER, DELETE_FOLDER or BULK_RESTORE_OBJECTS
	Created time.Time     `json:"created"`
	Ended   time.Time     `json:"ended"`
	Source  string        `json:"source"`
	Dest    string        `json:"dest,omitempty"`
	Folder  *folderRecord `json:"folder,omitempty"`
	// Restore is a bulk restore's counts.
	Restore *bulkRestoreCounts `json:"restore,omitempty"`
}

func folderKey(bucket, name string) string  { return folderPrefix + bucket + "/" + name }
func operationKey(bucket, id string) string { return operationPrefix + bucket + "/" + id }
func folderResource(bucket string) string   { return "projects/_/buckets/" + bucket }
func operationName(op operationRecord) string {
	return folderResource(op.Bucket) + "/operations/" + op.ID
}
func folderV2Name(f folderRecord) string   { return folderResource(f.Bucket) + "/folders/" + f.Name }
func hasFolders(tx Tx, bucket string) bool { return len(tx.List(folderPrefix+bucket+"/")) > 0 }
func folderMissing(bucket, name string) *httpError {
	// UNVERIFIED: the message; the status is the JSON API's 404.
	return notFound("The folder %s does not exist in bucket %s.", name, bucket)
}

func getFolder(tx Tx, bucket, name string) (folderRecord, bool, error) {
	var f folderRecord
	raw, ok := tx.Get(folderKey(bucket, name))
	if !ok {
		return f, false, nil
	}
	return f, true, json.Unmarshal(raw, &f)
}

func putFolder(tx Tx, f folderRecord) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tx.Put(folderKey(f.Bucket, f.Name), raw)
	return nil
}

// hierarchical says whether a bucket has hierarchicalNamespace enabled.
func hierarchical(b bucketRecord) bool {
	ns, _ := b.Fields["hierarchicalNamespace"].(map[string]any)
	on, _ := ns["enabled"].(bool)
	return on
}

// checkHierarchical applies the HNS rules to a bucket's fields at creation:
// hierarchical namespace needs uniform bucket-level access and excludes
// object versioning (hns-overview; the statuses and messages are
// UNVERIFIED).
func checkHierarchical(f map[string]any) error {
	if !hierarchical(bucketRecord{Fields: f}) {
		return nil
	}
	if !uniformAccess(bucketRecord{Fields: f}) {
		return badRequest("Invalid argument: hierarchicalNamespace can only be enabled on a bucket with iamConfiguration.uniformBucketLevelAccess enabled.")
	}
	if v, _ := f["versioning"].(map[string]any); v != nil && v["enabled"] == true {
		return badRequest("Invalid argument: versioning cannot be enabled on a bucket with hierarchicalNamespace enabled.")
	}
	return nil
}

// hnsBucket reads a bucket that exists and has hierarchical namespace.
func (s *Server) hnsBucket(tx Tx, bucket string) (bucketRecord, error) {
	b, ok, err := s.getBucket(tx, bucket)
	if err != nil {
		return b, err
	} else if !ok {
		return b, notFound("The specified bucket does not exist.")
	}
	if !hierarchical(b) {
		return b, badRequest("The bucket %s does not have hierarchical namespace enabled; folders exist only in a bucket that does.", bucket)
	}
	return b, nil
}

// parentFolder is the folder a name is directly in, or "" at the root.
func parentFolder(name string) string {
	trimmed := strings.TrimSuffix(name, "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[:i+1]
	}
	return ""
}

// ensureFolders creates every missing folder from the root down to dir
// (dir ending in "/", or "" for none).
func ensureFolders(tx Tx, bucket, dir string, now time.Time) error {
	if dir == "" {
		return nil
	}
	if _, ok, err := getFolder(tx, bucket, dir); err != nil || ok {
		return err
	}
	if err := ensureFolders(tx, bucket, parentFolder(dir), now); err != nil {
		return err
	}
	return putFolder(tx, folderRecord{Bucket: bucket, Name: dir, Created: now, Updated: now, Metageneration: 1})
}

// objectFolders creates the folders in a new object's path, in a
// hierarchical bucket.
func objectFolders(tx Tx, o objectRecord, now time.Time) error {
	raw, ok := tx.Get(bucketPrefix + o.Bucket)
	if !ok {
		return nil
	}
	var b bucketRecord
	if err := json.Unmarshal(raw, &b); err != nil || !hierarchical(b) {
		return err
	}
	return ensureFolders(tx, o.Bucket, parentFolder(o.Name), now)
}

func (s *Server) createFolder(bucket, rawName string, recursive bool) (folderRecord, error) {
	name, err := managedFolderName(rawName)
	if err != nil {
		return folderRecord{}, err
	}
	now := s.now().UTC()
	f := folderRecord{Bucket: bucket, Name: name, Created: now, Updated: now, Metageneration: 1}
	err = s.meta.Update(func(tx Tx) error {
		if _, err := s.hnsBucket(tx, bucket); err != nil {
			return err
		}
		if _, exists, err := getFolder(tx, bucket, name); err != nil {
			return err
		} else if exists {
			return conflict("The folder %s already exists in bucket %s.", name, bucket)
		}
		if p := parentFolder(name); p != "" {
			if _, ok, err := getFolder(tx, bucket, p); err != nil {
				return err
			} else if !ok && !recursive {
				return errorf(http.StatusBadRequest, "failedPrecondition", "The parent folder %s does not exist; create it first, or set recursive.", p)
			}
			if err := ensureFolders(tx, bucket, p, now); err != nil {
				return err
			}
		}
		return putFolder(tx, f)
	})
	return f, err
}

// existingFolder reads a folder of a hierarchical bucket and applies its
// metageneration preconditions.
func (s *Server) existingFolder(tx Tx, bucket, name string, pre preconditions, read bool) (folderRecord, error) {
	if _, err := s.hnsBucket(tx, bucket); err != nil {
		return folderRecord{}, err
	}
	name = withSlash(name)
	f, ok, err := getFolder(tx, bucket, name)
	if err != nil {
		return f, err
	} else if !ok {
		return f, folderMissing(bucket, name)
	}
	return f, pre.check(f.Metageneration, read)
}

func (s *Server) readFolder(bucket, name string, pre preconditions) (f folderRecord, err error) {
	err = s.meta.View(func(tx Tx) error {
		f, err = s.existingFolder(tx, bucket, name, pre, true)
		return err
	})
	return f, err
}

func (s *Server) deleteFolder(bucket, name string, pre preconditions) error {
	return s.meta.Update(func(tx Tx) error {
		f, err := s.existingFolder(tx, bucket, name, pre, false)
		if err != nil {
			return err
		}
		inside := tx.List(folderKey(bucket, f.Name))
		if len(tx.List(objectKey(bucket, f.Name))) > 0 || len(inside) > 1 {
			return conflict("The folder %s is not empty: objects or folders are in it.", f.Name)
		}
		tx.Delete(folderKey(bucket, f.Name))
		return nil
	})
}

func newOperationID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func putOperation(tx Tx, op operationRecord) error {
	raw, err := json.Marshal(op)
	if err != nil {
		return err
	}
	tx.Put(operationKey(op.Bucket, op.ID), raw)
	return nil
}

func (s *Server) deleteFolderRecursive(bucket, name string, pre preconditions) (operationRecord, error) {
	now := s.now().UTC()
	op := operationRecord{Bucket: bucket, ID: newOperationID(), Type: "DELETE_FOLDER", Created: now, Ended: now}
	err := s.meta.Update(func(tx Tx) error {
		b, err := s.hnsBucket(tx, bucket)
		if err != nil {
			return err
		}
		f, err := s.existingFolder(tx, bucket, name, pre, false)
		if err != nil {
			return err
		}
		op.Source = f.Name
		for _, k := range tx.List(objectKey(bucket, f.Name)) {
			o, ok, err := getObject(tx, bucket, strings.TrimPrefix(k, objectPrefix+bucket+"/"))
			if err != nil {
				return err
			} else if !ok {
				continue
			}
			if err := protect(b, o, now, false); err != nil {
				return err
			}
			tx.Delete(k)
			if err := removed(tx, b, o, now, 0); err != nil {
				return err
			}
		}
		for _, k := range tx.List(folderKey(bucket, f.Name)) {
			tx.Delete(k)
		}
		return putOperation(tx, op)
	})
	return op, err
}

func (s *Server) renameFolder(bucket, src, dst string, pre preconditions) (operationRecord, error) {
	dst, err := managedFolderName(dst)
	if err != nil {
		return operationRecord{}, err
	}
	now := s.now().UTC()
	op := operationRecord{Bucket: bucket, ID: newOperationID(), Type: "RENAME_FOLDER", Created: now, Ended: now, Dest: dst}
	err = s.meta.Update(func(tx Tx) error {
		f, err := s.existingFolder(tx, bucket, src, pre, false)
		if err != nil {
			return err
		}
		op.Source = f.Name
		if strings.HasPrefix(dst, f.Name) {
			return badRequest("Invalid argument: a folder cannot be renamed to %s, inside itself.", dst)
		}
		if _, exists, err := getFolder(tx, bucket, dst); err != nil {
			return err
		} else if exists {
			return conflict("The folder %s already exists in bucket %s.", dst, bucket)
		}
		if p := parentFolder(dst); p != "" {
			if _, ok, err := getFolder(tx, bucket, p); err != nil {
				return err
			} else if !ok {
				return errorf(http.StatusBadRequest, "failedPrecondition", "The destination's parent folder %s does not exist.", p)
			}
		}
		for _, k := range tx.List(objectKey(bucket, f.Name)) {
			o, ok, err := getObject(tx, bucket, strings.TrimPrefix(k, objectPrefix+bucket+"/"))
			if err != nil {
				return err
			} else if !ok {
				continue
			}
			tx.Delete(k)
			o.Name = dst + strings.TrimPrefix(o.Name, f.Name)
			o.Updated = now
			if err := putObject(tx, o); err != nil {
				return err
			}
		}
		for _, k := range tx.List(folderKey(bucket, f.Name)) {
			sub, _, err := getFolder(tx, bucket, strings.TrimPrefix(k, folderPrefix+bucket+"/"))
			if err != nil {
				return err
			}
			tx.Delete(k)
			sub.Name = dst + strings.TrimPrefix(sub.Name, f.Name)
			sub.Updated, sub.Metageneration = now, sub.Metageneration+1
			if err := putFolder(tx, sub); err != nil {
				return err
			}
			if sub.Name == dst {
				moved := sub
				op.Folder = &moved
			}
		}
		return putOperation(tx, op)
	})
	return op, err
}

// folderPage lists a bucket's folders in name order: under prefix, with
// delimiter "/" only those directly under it, from startOffset (inclusive)
// to endOffset (exclusive), after the page token's name, max at a time.
func (s *Server) folderPage(bucket, prefix, delimiter, start, end, token string, max int) ([]folderRecord, string, error) {
	if delimiter != "" && delimiter != "/" {
		return nil, "", badRequest("Invalid argument: the only folder delimiter is \"/\".")
	}
	if max <= 0 || max > 1000 {
		max = 1000
	}
	after := ""
	if token != "" {
		b, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			return nil, "", badRequest("Invalid argument: pageToken")
		}
		after = string(b)
	}
	var out []folderRecord
	next := ""
	err := s.meta.View(func(tx Tx) error {
		if _, err := s.hnsBucket(tx, bucket); err != nil {
			return err
		}
		for _, k := range tx.List(folderKey(bucket, prefix)) {
			name := strings.TrimPrefix(k, folderPrefix+bucket+"/")
			switch {
			case after != "" && name <= after, start != "" && name < start, end != "" && name >= end:
				continue
			case delimiter != "" && strings.Contains(strings.TrimSuffix(strings.TrimPrefix(name, prefix), "/"), "/"):
				continue
			}
			if len(out) == max {
				next = base64.RawURLEncoding.EncodeToString([]byte(out[len(out)-1].Name))
				break
			}
			f, _, err := getFolder(tx, bucket, name)
			if err != nil {
				return err
			}
			out = append(out, f)
		}
		return nil
	})
	return out, next, err
}

func (s *Server) readOperation(bucket, id string) (op operationRecord, err error) {
	err = s.meta.View(func(tx Tx) error {
		if _, ok, err := s.getBucket(tx, bucket); err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		raw, ok := tx.Get(operationKey(bucket, id))
		if !ok {
			return notFound("The operation %s does not exist in bucket %s.", id, bucket)
		}
		return json.Unmarshal(raw, &op)
	})
	return op, err
}

// --- JSON API v1 ---

func (s *Server) folderJSON(r *http.Request, f folderRecord) map[string]any {
	return map[string]any{
		"kind": "storage#folder", "id": f.Bucket + "/" + f.Name,
		"bucket": f.Bucket, "name": f.Name,
		"selfLink":       baseURL(r) + jsonPrefix + "b/" + escape(f.Bucket) + "/folders/" + escape(f.Name),
		"metageneration": strconv.FormatInt(f.Metageneration, 10),
		"createTime":     rfc3339(f.Created), "updateTime": rfc3339(f.Updated),
	}
}

// operationMetadata is the operation's metadata, typed as the Storage
// Control API's RenameFolderMetadata or DeleteFolderRecursiveMetadata. The
// commonMetadata type strings are UNVERIFIED.
func operationMetadata(op operationRecord) map[string]any {
	common := map[string]any{"type": op.Type, "createTime": rfc3339(op.Created),
		"endTime": rfc3339(op.Ended), "updateTime": rfc3339(op.Ended), "progressPercent": 100}
	if op.Type == bulkRestoreType {
		n := bulkRestoreCounts{}
		if op.Restore != nil {
			n = *op.Restore
		}
		// int64 counts are JSON strings, as proto3 JSON writes them.
		return map[string]any{"@type": "type.googleapis.com/google.storage.v1.BulkRestoreObjectsMetadata",
			"commonMetadata": common, "successfulObjectCount": strconv.FormatInt(n.Restored, 10),
			"skippedObjectCount": strconv.FormatInt(n.Skipped, 10), "failedObjectCount": strconv.FormatInt(n.Failed, 10)}
	}
	if op.Type == "RENAME_FOLDER" {
		return map[string]any{"@type": "type.googleapis.com/google.storage.control.v2.RenameFolderMetadata",
			"commonMetadata": common, "sourceFolderId": op.Source, "destinationFolderId": op.Dest}
	}
	return map[string]any{"@type": "type.googleapis.com/google.storage.control.v2.DeleteFolderRecursiveMetadata",
		"commonMetadata": common, "folderId": op.Source}
}

func operationResponse(op operationRecord) map[string]any {
	if op.Type == "RENAME_FOLDER" && op.Folder != nil {
		resp := folderV2JSON(*op.Folder)
		resp["@type"] = "type.googleapis.com/google.storage.control.v2.Folder"
		return resp
	}
	return map[string]any{"@type": "type.googleapis.com/google.protobuf.Empty", "value": map[string]any{}}
}

func (s *Server) operationJSON(r *http.Request, op operationRecord) map[string]any {
	return map[string]any{
		"kind": "storage#operation", "name": operationName(op), "done": true,
		"selfLink": baseURL(r) + jsonPrefix + "b/" + escape(op.Bucket) + "/operations/" + op.ID,
		"metadata": operationMetadata(op), "response": operationResponse(op),
	}
}

func boolParam(r *http.Request, name string) (bool, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, badRequest("Invalid argument: %s=%q is not a boolean", name, v)
	}
	return b, nil
}

func (s *Server) foldersInsert(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	recursive, err := boolParam(r, "recursive")
	if err != nil {
		writeError(w, err)
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	for k := range body {
		switch k {
		case "name", "kind", "id", "bucket", "selfLink", "metageneration", "createTime", "updateTime", "pendingRenameInfo":
		default:
			writeError(w, badRequest("Invalid argument: %s is not a Folder field", k))
			return
		}
	}
	name, _ := body["name"].(string)
	f, err := s.createFolder(bucket, name, recursive)
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.folderJSON(r, f))
}

func (s *Server) foldersGet(w http.ResponseWriter, r *http.Request) {
	pre, err := parsePreconditions(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch")
	if err != nil {
		writeError(w, err)
		return
	}
	f, err := s.readFolder(pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3), pre)
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.folderJSON(r, f))
}

func (s *Server) foldersDelete(w http.ResponseWriter, r *http.Request) {
	pre, err := parsePreconditions(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.deleteFolder(pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3), pre); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) foldersDeleteRecursive(w http.ResponseWriter, r *http.Request) {
	pre, err := parsePreconditions(r, "ifMetagenerationMatch", "ifMetagenerationNotMatch")
	if err != nil {
		writeError(w, err)
		return
	}
	op, err := s.deleteFolderRecursive(pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3), pre)
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.operationJSON(r, op))
}

func (s *Server) foldersRename(w http.ResponseWriter, r *http.Request) {
	pre, err := parsePreconditions(r, "ifSourceMetagenerationMatch", "ifSourceMetagenerationNotMatch")
	if err != nil {
		writeError(w, err)
		return
	}
	op, err := s.renameFolder(pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3), pathVar(r, jsonPrefix, 6), pre)
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.operationJSON(r, op))
}

func (s *Server) foldersList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	max := 0
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, badRequest("Invalid argument: pageSize=%q", v))
			return
		}
		max = n
	}
	fs, next, err := s.folderPage(pathVar(r, jsonPrefix, 1), q.Get("prefix"), q.Get("delimiter"),
		q.Get("startOffset"), q.Get("endOffset"), q.Get("pageToken"), max)
	if err != nil {
		writeError(w, err)
		return
	}
	resp := map[string]any{"kind": "storage#folders"}
	if len(fs) > 0 {
		items := make([]any, len(fs))
		for i, f := range fs {
			items[i] = s.folderJSON(r, f)
		}
		resp["items"] = items
	}
	if next != "" {
		resp["nextPageToken"] = next
	}
	writeResponse(w, r, http.StatusOK, resp)
}

func (s *Server) operationsGet(w http.ResponseWriter, r *http.Request) {
	op, err := s.readOperation(pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3))
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.operationJSON(r, op))
}

// operationsList lists a bucket's operations by ID. A filter is refused by
// name, since none is evaluated (UNVERIFIED).
func (s *Server) operationsList(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	q := r.URL.Query()
	if q.Get("filter") != "" {
		writeError(w, errorf(http.StatusNotImplemented, "notImplemented", "filter is not implemented for storage.buckets.operations.list"))
		return
	}
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
	after := q.Get("pageToken")
	var items []any
	next := ""
	err := s.meta.View(func(tx Tx) error {
		if _, ok, err := s.getBucket(tx, bucket); err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		last := ""
		for _, k := range tx.List(operationPrefix + bucket + "/") {
			id := strings.TrimPrefix(k, operationPrefix+bucket+"/")
			if after != "" && id <= after {
				continue
			}
			if len(items) == max {
				next = last
				break
			}
			raw, _ := tx.Get(k)
			var op operationRecord
			if err := json.Unmarshal(raw, &op); err != nil {
				return err
			}
			items, last = append(items, s.operationJSON(r, op)), id
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	resp := map[string]any{"kind": "storage#operationsList"}
	if len(items) > 0 {
		resp["operations"] = items
	}
	if next != "" {
		resp["nextPageToken"] = next
	}
	writeResponse(w, r, http.StatusOK, resp)
}
