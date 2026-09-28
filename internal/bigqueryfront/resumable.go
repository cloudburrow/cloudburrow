package bigqueryfront

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// A load from a local file too large for one request is sent as a
// resumable upload: the job in a first request, then the data in chunks
// to the URL the first request's answer gives in its Location header. The
// Go client does so above 16 MiB. The emulator does not serve that
// protocol (measured against the pinned image, #919): its Location is its
// own listen address, http://0.0.0.0:9051/..., which no client outside its
// pod can reach, so the Go client retried the first chunk until it gave up
// ("connection refused"); and it takes a chunk only by PUT, and only as
// the whole of the data, where the Go client sends each chunk by POST.
// Upstream: goccy/bigquery-emulator#160 (the Location, closed) and #393
// (a fix, closed unmerged).
//
// So the front serves resumable uploads itself (resumable): it keeps the
// job and writes the chunks to a file of its own, answers each chunk but
// the last 308 with the Range it holds (or, as the Go client asks with
// X-GUploader-No-308, 200 with X-Http-Status-Code-Override: 308), and on
// the last sends the job and
// the data to the emulator as one multipart upload, through the same
// checks as any load (insertJob). Its answer, the job, is the last
// chunk's.
// https://cloud.google.com/bigquery/docs/reference/api-uploads#resumable

// uploadSessions are the resumable uploads in progress.
type uploadSessions struct {
	mu       sync.Mutex
	sessions map[string]*uploadSession
}

type uploadSession struct {
	mu      sync.Mutex
	job     []byte
	file    *os.File
	size    int64
	created time.Time
}

// maxUploadAge is how long an upload that is never finished is kept.
const maxUploadAge = 24 * time.Hour

func (u *uploadSessions) add(s *uploadSession) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.sessions == nil {
		u.sessions = map[string]*uploadSession{}
	}
	for k, old := range u.sessions {
		if time.Since(old.created) > maxUploadAge {
			old.close()
			delete(u.sessions, k)
		}
	}
	u.sessions[id] = s
	return id
}

func (u *uploadSessions) get(id string) *uploadSession {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.sessions[id]
}

func (u *uploadSessions) remove(id string) {
	u.mu.Lock()
	s := u.sessions[id]
	delete(u.sessions, id)
	u.mu.Unlock()
	if s != nil {
		s.close()
	}
}

func (s *uploadSession) close() {
	if s.file != nil {
		_ = s.file.Close()
		_ = os.Remove(s.file.Name())
	}
}

// contentRange matches a chunk's Content-Range: "bytes 0-99/*",
// "bytes 100-199/200", or "bytes */200" for a status query or an empty
// last chunk.
var contentRange = regexp.MustCompile(`^bytes (?:(\d+)-(\d+)|\*)/(\d+|\*)$`)

// resumable serves a resumable upload's requests: the first, with the job
// and no upload_id, and each chunk after it.
func (f front) resumable(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("upload_id")
	if id == "" {
		f.startUpload(w, r)
		return
	}
	s := f.uploads.get(id)
	if s == nil {
		writeError(w, http.StatusNotFound, "notFound", "Not found: resumable upload "+id)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var start int64
	total := int64(-1)
	if cr := r.Header.Get("Content-Range"); cr != "" {
		m := contentRange.FindStringSubmatch(cr)
		if m == nil {
			writeError(w, http.StatusBadRequest, "invalid", "Invalid Content-Range "+strconv.Quote(cr))
			return
		}
		start = s.size
		if m[1] != "" {
			start, _ = strconv.ParseInt(m[1], 10, 64)
		}
		if m[3] != "*" {
			total, _ = strconv.ParseInt(m[3], 10, 64)
		}
	} else {
		// No Content-Range: the body is all of the data.
		start = 0
	}
	if start > s.size {
		writeError(w, http.StatusBadRequest, "invalid", fmt.Sprintf("The chunk starts at byte %d, but %d bytes have been received", start, s.size))
		return
	}
	// A chunk sent again, after a lost answer, overwrites what it covers.
	if err := s.file.Truncate(start); err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: "+err.Error())
		return
	}
	if _, err := s.file.Seek(start, io.SeekStart); err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: "+err.Error())
		return
	}
	n, err := io.Copy(s.file, r.Body)
	s.size = start + n
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: reading the chunk: "+err.Error())
		return
	}
	if r.Header.Get("Content-Range") == "" {
		total = s.size
	}
	if total < 0 || s.size < total {
		if s.size > 0 {
			w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", s.size-1))
		}
		w.Header().Set("Content-Length", "0")
		// "X-GUploader-No-308: yes" asks for 200 with the 308 in a
		// header instead, which the Go client sends and expects.
		if r.Header.Get("X-GUploader-No-308") == "yes" {
			w.Header().Set("X-Http-Status-Code-Override", "308")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	}
	// The upload is over unless the emulator failed with a status the
	// client retries (5xx), when the client sends the last chunk again.
	sw := &statusWriter{ResponseWriter: w}
	f.finishUpload(sw, r, s)
	if sw.status < 500 {
		f.uploads.remove(id)
	}
}

// statusWriter records the status written through it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// startUpload answers a resumable upload's first request: the job is
// checked as far as it can be without its data (the destination's table
// ID and the schema), kept, and the upload's URL given in Location.
func (f front) startUpload(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "cloudburrow: "+err.Error())
		return
	}
	var job jobBody
	if _, ok := decode(r, &job); ok && job.Configuration.Load != nil {
		l := job.Configuration.Load
		msg := ""
		if l.DestinationTable != nil {
			msg = checkTableID(l.DestinationTable.TableID)
		}
		if msg == "" && l.Schema != nil {
			msg = checkSchema(l.Schema.Fields, "")
		}
		if msg != "" {
			writeError(w, http.StatusBadRequest, "invalid", msg)
			return
		}
	}
	file, err := os.CreateTemp("", "bigquery-upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: keeping the upload: "+err.Error())
		return
	}
	id := f.uploads.add(&uploadSession{job: body, file: file, created: time.Now()})
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	q := url.Values{"uploadType": {"resumable"}, "upload_id": {id}}
	w.Header().Set("Location", scheme+"://"+r.Host+r.URL.EscapedPath()+"?"+q.Encode())
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

// finishUpload sends the job and its data to the emulator as a multipart
// upload, through insertJob, and answers with its answer.
func (f front) finishUpload(w http.ResponseWriter, r *http.Request, s *uploadSession) {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: "+err.Error())
		return
	}
	rb := make([]byte, 12)
	_, _ = rand.Read(rb)
	boundary := "cloudburrow-" + hex.EncodeToString(rb)
	head := []byte("--" + boundary + "\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n")
	head = append(head, s.job...)
	head = append(head, []byte("\r\n--"+boundary+"\r\nContent-Type: application/octet-stream\r\n\r\n")...)
	tail := []byte("\r\n--" + boundary + "--\r\n")
	u := *r.URL
	u.RawQuery = url.Values{"uploadType": {"multipart"}}.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, u.String(),
		io.MultiReader(bytes.NewReader(head), io.LimitReader(s.file, s.size), bytes.NewReader(tail)))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: "+err.Error())
		return
	}
	req.Host = r.Host
	req.Proto, req.ProtoMajor, req.ProtoMinor = "HTTP/1.1", 1, 1
	req.RemoteAddr = r.RemoteAddr
	req.Header.Set("Content-Type", "multipart/related; boundary="+boundary)
	req.ContentLength = int64(len(head)) + s.size + int64(len(tail))
	f.insertJob(w, req)
}
