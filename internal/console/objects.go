package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Object upload, download and preview (#295).
//
// These are routes of their own rather than actions, because each moves
// bytes: an action carries a JSON body of a few kilobytes and answers with
// JSON, and neither a 30 MiB upload nor a download fits that. Every one
// streams — an upload goes into the provider as it is read, and a download
// reaches the browser as the provider produces it — so the console never
// holds an object in memory, whatever its size.
//
// A preview is the dangerous one. An object's content type is whatever its
// uploader said, and serving "text/html" from the console's own origin would
// run the uploader's script with the console's authority, which can delete
// everything. So a preview is only ever plain text or a raster image, never
// what the object claims to be, and every object response carries a CSP that
// forbids script and a nosniff that stops the browser guessing otherwise.

// ObjectStore is a provider that holds objects with bytes: Cloud Storage.
//
// A path names a location as the detail routes do: the bucket, then the
// object name's segments.
type ObjectStore interface {
	// Upload writes r as the object name under the prefix path, and returns
	// the object's full name. It must stop, leaving no object, when ctx is
	// cancelled or r returns an error.
	Upload(ctx context.Context, project string, prefix []string, name, contentType string, r io.Reader) (string, error)
	// OpenObject returns the object's bytes and what is known about it.
	OpenObject(ctx context.Context, project string, path []string) (ObjectReader, error)
	// DeleteObject removes one object.
	DeleteObject(ctx context.Context, project string, path []string) error
}

// ObjectReader is an open object.
type ObjectReader struct {
	io.ReadCloser
	Name        string
	Size        int64
	ContentType string
}

// DefaultUploadLimit is the upload cap when Settings has not changed it.
const DefaultUploadLimit int64 = 32 << 20

// maxUploadLimit bounds what the setting may be raised to. The console is a
// developer tool on a laptop; a limit is still a limit.
const maxUploadLimit int64 = 5 << 30

// PreviewLimit is the largest object previewed inline.
const PreviewLimit int64 = 1 << 20

// objectCSP is sent with every object response. A preview is text or an
// image, so it needs nothing else; sandbox makes the response a unique
// origin even if something is rendered that should not have been.
const objectCSP = "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; sandbox"

// previewImages are the image types shown as images. SVG is absent on
// purpose: it is a document that can carry script, so it previews as text.
var previewImages = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

// settings are the console's server-side settings.
type settings struct {
	mu          sync.Mutex
	uploadLimit int64
}

func (s *settings) UploadLimit() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploadLimit == 0 {
		return DefaultUploadLimit
	}
	return s.uploadLimit
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		var req struct {
			UploadLimitBytes int64 `json:"uploadLimitBytes"`
		}
		if err := decodeStrict(w, r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if req.UploadLimitBytes < 1 || req.UploadLimitBytes > maxUploadLimit {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("the upload limit must be between 1 byte and %s", formatSize(maxUploadLimit)),
			})
			return
		}
		s.settings.mu.Lock()
		s.settings.uploadLimit = req.UploadLimitBytes
		s.settings.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uploadLimitBytes": s.settings.UploadLimit(), "previewLimitBytes": PreviewLimit,
	})
}

func decodeStrict(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Server) objectStore(w http.ResponseWriter, r *http.Request) (Provider, ObjectStore, bool) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return nil, nil, false
	}
	st, ok := p.(ObjectStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": p.Title() + " holds no objects"})
		return nil, nil, false
	}
	return p, st, true
}

// objectPath reads the path from repeated name parameters, as detail does.
func objectPath(r *http.Request) []string {
	var out []string
	// Not trimmed: an object name may begin or end with a space, and
	// trimming it would address a different object.
	for _, seg := range r.URL.Query()["name"] {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// errTooLarge is what the upload's reader returns past the limit.
type errTooLarge struct{ limit int64 }

func (e errTooLarge) Error() string {
	return fmt.Sprintf("the file is larger than the upload limit of %s; change the limit in Settings",
		formatSize(e.limit))
}

// cappedReader fails once more than limit bytes have been read.
type cappedReader struct {
	r     io.Reader
	n     int64
	limit int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.limit {
		return n, errTooLarge{c.limit}
	}
	return n, err
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	p, store, ok := s.objectStore(w, r)
	if !ok {
		return
	}
	prefix := objectPath(r)
	project := r.URL.Query().Get("project")
	if len(prefix) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name the bucket to upload into"})
		return
	}
	limit := s.settings.UploadLimit()
	// Refused before a byte is read when the request says how large it is.
	// Multipart framing adds a little, so the allowance covers it.
	const framing = 64 << 10
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
	var part io.Reader
	var filename, contentType string
	for {
		pt, err := mr.NextPart()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the upload has no file part"})
			return
		}
		if pt.FormName() == "file" {
			part, filename, contentType = pt, pt.FileName(), pt.Header.Get("Content-Type")
			break
		}
	}
	filename = path.Base(strings.ReplaceAll(filename, "\\", "/"))
	if filename == "" || filename == "." || filename == "/" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the file has no name"})
		return
	}
	if contentType == "" || contentType == "application/octet-stream" {
		if t := mime.TypeByExtension(path.Ext(filename)); t != "" {
			contentType = t
		} else {
			contentType = "application/octet-stream"
		}
	}

	target := strings.Join(append(append([]string{}, prefix...), filename), "/")
	opID := s.logs.StartOperation("upload", target, project)
	// Cancelled on any failure, so a half-read upload is abandoned rather
	// than committed as a truncated object.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	capped := &cappedReader{r: part, limit: limit}
	name, err := store.Upload(ctx, project, prefix, filename, contentType, readerCancelling{capped, cancel})
	if err != nil {
		var tl errTooLarge
		var mbe *http.MaxBytesError
		code := http.StatusBadRequest
		msg := userMessage(err)
		if errors.As(err, &tl) || errors.As(err, &mbe) || capped.n > limit {
			code, msg = http.StatusRequestEntityTooLarge, errTooLarge{limit}.Error()
		}
		s.logs.FinishOperation(opID, OperationFailed, msg)
		s.logs.Log(Entry{Severity: SeverityError, Source: p.ID(), Project: project, Resource: target,
			OperationID: opID, Message: "upload failed: " + msg})
		writeJSON(w, code, map[string]string{"error": msg, "operation": opID})
		return
	}
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	s.logs.Log(Entry{Severity: SeverityInfo, Source: p.ID(), Project: project, Resource: name,
		OperationID: opID, Message: fmt.Sprintf("uploaded %s (%s)", name, formatSize(capped.n))})
	writeJSON(w, http.StatusOK, map[string]any{"uploaded": name, "size": capped.n, "operation": opID})
}

// readerCancelling cancels the upload's context the moment its source
// fails, so the provider's writer is abandoned even if it would otherwise
// commit what it had on seeing an error.
type readerCancelling struct {
	r      io.Reader
	cancel context.CancelFunc
}

func (rc readerCancelling) Read(p []byte) (int, error) {
	n, err := rc.r.Read(p)
	if err != nil && err != io.EOF {
		rc.cancel()
	}
	return n, err
}

func objectHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", objectCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	_, store, ok := s.objectStore(w, r)
	if !ok {
		return
	}
	obj, err := store.OpenObject(r.Context(), r.URL.Query().Get("project"), objectPath(r))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": userMessage(err)})
		return
	}
	defer obj.Close()
	objectHeaders(w)
	// Always an attachment of opaque bytes: a download is saved, never
	// rendered, whatever the object says it is.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment",
		map[string]string{"filename": downloadName(obj.Name)}))
	if obj.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(flushingWriter{w}, obj)
}

// flushingWriter flushes after every write, so a download reaches the
// browser as it is read rather than when a server buffer fills.
type flushingWriter struct{ w http.ResponseWriter }

func (f flushingWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// downloadName is the object's last segment, reduced to characters every
// browser and file system accepts.
func downloadName(name string) string {
	base := path.Base(name)
	b := strings.Builder{}
	for _, r := range base {
		switch {
		case r < 0x20 || r == 0x7f || strings.ContainsRune(`"\/:*?<>|`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	if out := strings.Trim(b.String(), ". "); out != "" {
		return out
	}
	return "download"
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	_, store, ok := s.objectStore(w, r)
	if !ok {
		return
	}
	obj, err := store.OpenObject(r.Context(), r.URL.Query().Get("project"), objectPath(r))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": userMessage(err)})
		return
	}
	defer obj.Close()
	objectHeaders(w)
	if obj.Size > PreviewLimit {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("%s is %s; previews are limited to %s — download it instead",
				path.Base(obj.Name), formatSize(obj.Size), formatSize(PreviewLimit)),
		})
		return
	}
	mt, _, _ := mime.ParseMediaType(obj.ContentType)
	switch {
	case previewImages[mt]:
		w.Header().Set("Content-Type", mt)
	case previewAsText(mt):
		// Whatever the object claims — text/html included — it is shown as
		// the characters it contains.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	default:
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{
			"error": fmt.Sprintf("no preview for %s; download it instead", orUnknown(obj.ContentType)),
		})
		return
	}
	w.WriteHeader(http.StatusOK)
	// The object may be larger than it said; the preview never is.
	_, _ = io.Copy(w, io.LimitReader(obj, PreviewLimit))
}

func previewAsText(mt string) bool {
	switch {
	case strings.HasPrefix(mt, "text/"):
		return true
	case mt == "application/json", strings.HasSuffix(mt, "+json"),
		mt == "application/xml", strings.HasSuffix(mt, "+xml"),
		mt == "application/javascript", mt == "application/x-yaml", mt == "application/yaml",
		mt == "application/x-ndjson", mt == "image/svg+xml":
		return true
	}
	return false
}

func orUnknown(ct string) string {
	if ct == "" {
		return "an object with no content type"
	}
	return ct
}

func (s *Server) handleDeleteObject(w http.ResponseWriter, r *http.Request) {
	p, store, ok := s.objectStore(w, r)
	if !ok {
		return
	}
	pth := objectPath(r)
	if len(pth) < 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name the bucket and the object"})
		return
	}
	project := r.URL.Query().Get("project")
	target := strings.Join(pth, "/")
	opID := s.logs.StartOperation("delete", target, project)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := store.DeleteObject(ctx, project, pth); err != nil {
		s.logs.FinishOperation(opID, OperationFailed, userMessage(err))
		s.logs.Log(Entry{Severity: SeverityError, Source: p.ID(), Project: project, Resource: target,
			OperationID: opID, Message: "delete failed: " + userMessage(err)})
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err), "operation": opID})
		return
	}
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	s.logs.Log(Entry{Severity: SeverityInfo, Source: p.ID(), Project: project, Resource: target,
		OperationID: opID, Message: "deleted " + target})
	writeJSON(w, http.StatusOK, map[string]string{"deleted": target, "operation": opID})
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
