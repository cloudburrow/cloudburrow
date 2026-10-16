package storage

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// The Storage Control API v2's REST surface for folders (#1203): the paths
// cloud.google.com/go/storage/control/apiv2's NewStorageControlRESTClient
// calls, on the same server and over the same folder records as the JSON
// API's folders (folders.go).
//
//	POST   /v2/projects/_/buckets/{bucket}/folders?folderId=&recursive=  CreateFolder
//	GET    /v2/projects/_/buckets/{bucket}/folders                       ListFolders
//	GET    /v2/projects/_/buckets/{bucket}/folders/{folder}              GetFolder
//	DELETE /v2/projects/_/buckets/{bucket}/folders/{folder}              DeleteFolder
//	POST   /v2/projects/_/buckets/{bucket}/folders/{folder}:rename       RenameFolder
//	POST   /v2/projects/_/buckets/{bucket}/folders/{folder}:deleteRecursive
//	GET    /v2/projects/_/buckets/{bucket}/operations/{id}               GetOperation
//
// Every other v2 call, on gRPC's REST mapping, is 501 UNIMPLEMENTED naming
// the path. Errors carry the canonical status, as Google's v2 REST errors
// do; the mapping of each refusal to a code is UNVERIFIED.

const controlPrefix = "/v2/"

func (s *Server) serveControl(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, controlPrefix+"projects/_/buckets/")
	if rest == r.URL.Path {
		writeControlError(w, errorf(http.StatusNotImplemented, "notImplemented", "%s %s is not implemented", r.Method, r.URL.Path))
		return
	}
	bucket, sub, _ := strings.Cut(rest, "/")
	q := r.URL.Query()
	switch {
	case sub == "folders" && r.Method == http.MethodPost:
		s.controlCreateFolder(w, r, bucket)
	case sub == "folders" && r.Method == http.MethodGet:
		s.controlListFolders(w, r, bucket)
	case strings.HasPrefix(sub, "folders/"):
		name := strings.TrimPrefix(sub, "folders/")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(name, ":rename"):
			s.controlRenameFolder(w, r, bucket, strings.TrimSuffix(name, ":rename"))
		case r.Method == http.MethodPost && strings.HasSuffix(name, ":deleteRecursive"):
			s.controlDeleteRecursive(w, r, bucket, strings.TrimSuffix(name, ":deleteRecursive"))
		case r.Method == http.MethodGet:
			pre, err := controlPreconditions(q.Get("ifMetagenerationMatch"), q.Get("ifMetagenerationNotMatch"))
			if err != nil {
				writeControlError(w, err)
				return
			}
			var f folderRecord
			err = s.meta.View(func(tx Tx) error {
				f, err = s.existingFolder(tx, bucket, name, pre, false)
				return err
			})
			if err != nil {
				writeControlError(w, err)
				return
			}
			writeControlJSON(w, folderV2JSON(f))
		case r.Method == http.MethodDelete:
			pre, err := controlPreconditions(q.Get("ifMetagenerationMatch"), q.Get("ifMetagenerationNotMatch"))
			if err != nil {
				writeControlError(w, err)
				return
			}
			if err := s.deleteFolder(bucket, name, pre); err != nil {
				writeControlError(w, err)
				return
			}
			writeControlJSON(w, map[string]any{})
		default:
			writeControlError(w, errorf(http.StatusNotImplemented, "notImplemented", "%s %s is not implemented", r.Method, r.URL.Path))
		}
	case strings.HasPrefix(sub, "operations/") && r.Method == http.MethodGet:
		op, err := s.readOperation(bucket, strings.TrimPrefix(sub, "operations/"))
		if err != nil {
			writeControlError(w, err)
			return
		}
		writeControlJSON(w, operationV2JSON(op))
	default:
		writeControlError(w, errorf(http.StatusNotImplemented, "notImplemented", "%s %s is not implemented", r.Method, r.URL.Path))
	}
}

func folderV2JSON(f folderRecord) map[string]any {
	return map[string]any{
		"name": folderV2Name(f), "metageneration": strconv.FormatInt(f.Metageneration, 10),
		"createTime": rfc3339(f.Created), "updateTime": rfc3339(f.Updated),
	}
}

func operationV2JSON(op operationRecord) map[string]any {
	out := map[string]any{"name": operationName(op), "done": !op.Pending, "metadata": operationMetadata(op)}
	addOperationResult(out, op)
	return out
}

// controlPreconditions reads ifMetagenerationMatch and NotMatch, sent as a
// query parameter or, in a request body, as protojson's int64 string.
func controlPreconditions(match, notMatch string) (preconditions, error) {
	var p preconditions
	for i, v := range []string{match, notMatch} {
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return p, badRequest("Invalid argument: metageneration precondition %q is not a number", v)
		}
		if i == 0 {
			p.match = &n
		} else {
			p.notMatch = &n
		}
	}
	return p, nil
}

func (s *Server) controlCreateFolder(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	recursive, err := boolParam(r, "recursive")
	if err != nil {
		writeControlError(w, err)
		return
	}
	id := q.Get("folderId")
	if id == "" {
		writeControlError(w, required("folderId"))
		return
	}
	if !strings.HasSuffix(id, "/") {
		// CreateFolderRequest: "The folder_id must end with a slash."
		writeControlError(w, badRequest("Invalid argument: folder_id %q must end with a slash", id))
		return
	}
	f, err := s.createFolder(bucket, id, recursive)
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeControlJSON(w, folderV2JSON(f))
}

func (s *Server) controlListFolders(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	max := 0
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeControlError(w, badRequest("Invalid argument: page_size=%q", v))
			return
		}
		max = n
	}
	fs, next, err := s.folderPage(bucket, q.Get("prefix"), q.Get("delimiter"),
		q.Get("lexicographicStart"), q.Get("lexicographicEnd"), q.Get("pageToken"), max)
	if err != nil {
		writeControlError(w, err)
		return
	}
	out := make([]any, len(fs))
	for i, f := range fs {
		out[i] = folderV2JSON(f)
	}
	resp := map[string]any{"folders": out}
	if next != "" {
		resp["nextPageToken"] = next
	}
	writeControlJSON(w, resp)
}

// controlBody is the fields RenameFolder and DeleteFolderRecursive send.
type controlBody struct {
	DestinationFolderID      string `json:"destinationFolderId"`
	IfMetagenerationMatch    string `json:"ifMetagenerationMatch"`
	IfMetagenerationNotMatch string `json:"ifMetagenerationNotMatch"`
}

func readControlBody(r *http.Request) (controlBody, preconditions, error) {
	var b controlBody
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return b, preconditions{}, badRequest("Invalid argument: %v", err)
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &b); err != nil {
			return b, preconditions{}, badRequest("Invalid argument: %v", err)
		}
	}
	q := r.URL.Query()
	match, notMatch := b.IfMetagenerationMatch, b.IfMetagenerationNotMatch
	if match == "" {
		match = q.Get("ifMetagenerationMatch")
	}
	if notMatch == "" {
		notMatch = q.Get("ifMetagenerationNotMatch")
	}
	pre, err := controlPreconditions(match, notMatch)
	return b, pre, err
}

func (s *Server) controlRenameFolder(w http.ResponseWriter, r *http.Request, bucket, name string) {
	b, pre, err := readControlBody(r)
	if err != nil {
		writeControlError(w, err)
		return
	}
	if b.DestinationFolderID == "" {
		writeControlError(w, required("destinationFolderId"))
		return
	}
	op, err := s.renameFolder(bucket, name, b.DestinationFolderID, pre)
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeControlJSON(w, operationV2JSON(op))
}

func (s *Server) controlDeleteRecursive(w http.ResponseWriter, r *http.Request, bucket, name string) {
	_, pre, err := readControlBody(r)
	if err != nil {
		writeControlError(w, err)
		return
	}
	op, err := s.deleteFolderRecursive(bucket, name, pre)
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeControlJSON(w, operationV2JSON(op))
}

func writeControlJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// controlStatus maps a JSON API refusal to the HTTP status and canonical
// code of Google's REST mapping of gRPC.
func controlStatus(e *httpError) (int, string) {
	switch {
	case e.status == http.StatusNotFound:
		return http.StatusNotFound, "NOT_FOUND"
	case e.status == http.StatusConflict && strings.Contains(e.message, "already exists"):
		return http.StatusConflict, "ALREADY_EXISTS"
	case e.status == http.StatusConflict, e.status == http.StatusPreconditionFailed,
		e.status == http.StatusNotModified, e.reason == "failedPrecondition":
		return http.StatusBadRequest, "FAILED_PRECONDITION"
	case e.status == http.StatusBadRequest:
		return http.StatusBadRequest, "INVALID_ARGUMENT"
	case e.status == http.StatusNotImplemented:
		return http.StatusNotImplemented, "UNIMPLEMENTED"
	case e.status == http.StatusTooManyRequests:
		return e.status, "RESOURCE_EXHAUSTED"
	}
	return http.StatusInternalServerError, "INTERNAL"
}

func writeControlError(w http.ResponseWriter, err error) {
	e, ok := err.(*httpError)
	if !ok {
		e = errorf(http.StatusInternalServerError, "backendError", "internal error")
	}
	code, status := controlStatus(e)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": e.message, "status": status}})
}
