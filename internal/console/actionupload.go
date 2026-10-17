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

// ListActor is a provider with actions on its list screen itself, on no
// existing resource (#1289): Cloud Run's Create service from source, and
// Spanner's Create instance beside its Create database (#1491). The list
// screen draws them beside Create, and each is sent with an empty path: one
// that takes a file through the upload route, which needs a FileActor, and
// any other through the action route, to ActAt with no path.
type ListActor interface {
	PathActor
	// ListActions returns what the list screen offers for a project.
	ListActions(ctx context.Context, project string) []Action
}

// takesFile reports whether an action's form holds a file.
func takesFile(a Action) bool {
	for _, f := range a.Fields {
		if f.Type == FileFieldType {
			return true
		}
	}
	return false
}

// listActions are a provider's list-screen actions, or none when it has
// no ListActor. One that takes a file is drawn only for a FileActor, which
// the upload route needs.
func listActions(ctx context.Context, p Provider, project string) []Action {
	la, ok := p.(ListActor)
	if !ok {
		return nil
	}
	_, files := p.(FileActor)
	var out []Action
	for _, a := range la.ListActions(ctx, project) {
		if files || !takesFile(a) {
			out = append(out, a)
		}
	}
	return out
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
	// An empty path is the list screen's own action (ListActor), on no
	// existing resource (#1289).
	if req.Action == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action is required"})
		return
	}
	project := r.URL.Query().Get("project")
	ctx, cancel := context.WithTimeout(r.Context(), actionUploadBudget)
	defer cancel()

	offers := listActions(ctx, p, project)
	if len(req.Path) > 0 {
		offers = actor.DetailActions(ctx, project, req.Path)
	}
	var offered *Action
	for _, a := range offers {
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
	if !takesFile(*offered) {
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
	if name == "" {
		name = p.Title()
	}
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
