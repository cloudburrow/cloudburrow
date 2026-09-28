package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

// An action whose form holds a file (#999): BigQuery's Load from a file.
//
// An action's JSON body is at most 64 KiB, and the one upload route there
// was is Cloud Storage's, which writes an object. This route takes the
// action's request and its file in one multipart body, and streams the file
// into the provider as it is read, under the same upload limit as an object
// upload (Settings) and behind the same Host and same-origin guards as every
// /api/ route, so the console never holds the file in memory and a visited
// page cannot post one.
//
// The body's first part is "request", the JSON an action posts ({Path,
// Action, Values}); its second is "file". The action must be one the page
// offers (DetailActions), with a field of type "file", exactly as a JSON
// action must be offered.

// FileFieldType is the Field.Type of a file control. An action with such a
// field is sent to the upload route with the file the field names.
const FileFieldType = "file"

// UploadedFile is a file posted with an action.
type UploadedFile struct {
	// Name is the file's base name, as the browser gave it.
	Name string
	// ContentType is what the browser said the file is, which may be empty.
	ContentType string
	io.Reader
}

// FileActor is a PathActor with actions that take a file.
type FileActor interface {
	PathActor
	// ActAtFile performs an action whose form holds a file. It must stop,
	// leaving nothing behind it would not have left on a failure, when ctx
	// is cancelled or the file's reader fails.
	ActAtFile(ctx context.Context, project string, path []string, action string, values map[string]string, file UploadedFile) (*Listing, error)
}

// actionUploadBudget bounds one action with a file, as an object upload's
// thirty minutes do.
const actionUploadBudget = 30 * time.Minute

func (s *Server) handleActionUpload(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return
	}
	actor, ok := p.(FileActor)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": p.Title() + " has no actions that take a file"})
		return
	}
	limit := s.settings.UploadLimit()
	// As handleUpload: the file may be the limit, and the request part and
	// the multipart framing are allowed on top of it.
	const framing = 128 << 10
	if r.ContentLength > limit+framing {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": errTooLarge{limit}.Error()})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit+framing)
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected a multipart upload: " + err.Error()})
		return
	}
	first, err := mr.NextPart()
	if err != nil || first.FormName() != "request" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the upload's first part is the action's request"})
		return
	}
	var req struct {
		Action string
		Path   []string
		Values map[string]string
	}
	dec := json.NewDecoder(io.LimitReader(first, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the action's request: " + err.Error()})
		return
	}
	if req.Action == "" || len(req.Path) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action and path are required"})
		return
	}
	project := r.URL.Query().Get("project")
	ctx, cancel := context.WithTimeout(r.Context(), actionUploadBudget)
	defer cancel()

	var offered *Action
	for _, a := range actor.DetailActions(ctx, project, req.Path) {
		if a.ID == req.Action {
			a := a
			offered = &a
			break
		}
	}
	if offered == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": req.Action + " is not available on this resource"})
		return
	}
	takesFile := false
	for _, f := range offered.Fields {
		takesFile = takesFile || f.Type == FileFieldType
	}
	if !takesFile {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": offered.Label + " takes no file; it is sent to /api/actions"})
		return
	}

	part, err := mr.NextPart()
	if err != nil || part.FormName() != "file" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the upload has no file part"})
		return
	}
	filename := path.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
	if filename == "" || filename == "." || filename == "/" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the file has no name"})
		return
	}

	name := strings.Join(req.Path, "/")
	opID := s.logs.StartOperation(req.Action, name, project)
	capped := &cappedReader{r: part, limit: limit}
	result, err := actor.ActAtFile(ctx, project, req.Path, req.Action, req.Values, UploadedFile{
		Name: filename, ContentType: part.Header.Get("Content-Type"), Reader: readerCancelling{capped, cancel},
	})
	if err != nil {
		var tl errTooLarge
		var mbe *http.MaxBytesError
		code, msg := http.StatusBadRequest, userMessage(err)
		if errors.As(err, &tl) || errors.As(err, &mbe) || capped.n > limit {
			code, msg = http.StatusRequestEntityTooLarge, errTooLarge{limit}.Error()
		}
		s.logs.FinishOperation(opID, OperationFailed, msg)
		s.logs.Log(Entry{Severity: SeverityError, Source: p.ID(), Project: project, Resource: name,
			OperationID: opID, Message: req.Action + " failed: " + msg})
		writeJSON(w, code, map[string]string{"error": msg, "operation": opID})
		return
	}
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	s.logs.Log(Entry{Severity: SeverityInfo, Source: p.ID(), Project: project, Resource: name,
		OperationID: opID, Message: fmt.Sprintf("%s applied to %s with %s (%s)", req.Action, name, filename, formatSize(capped.n))})
	out := map[string]any{"applied": req.Action, "operation": opID, "size": capped.n}
	if result != nil {
		if result.Items == nil {
			result.Items = []Resource{}
		}
		out["result"] = result
	}
	writeJSON(w, http.StatusOK, out)
}
