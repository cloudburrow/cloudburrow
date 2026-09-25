package storage

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Resumable uploads (#493), the JSON API's protocol
// (docs.cloud.google.com/storage/docs/performing-resumable-uploads):
//
//   - POST ...?uploadType=resumable starts a session: 200 with a Location
//     naming a random upload_id.
//   - Chunks are PUT or POST to that URL with Content-Range "bytes a-b/TOTAL"
//     or "bytes a-b/*"; every chunk but the last is a multiple of 256 KiB.
//     Bytes already persisted are immutable: a re-sent range is skipped.
//   - An incomplete chunk is answered 308 with "Range: bytes=0-N" (no Range
//     when nothing is persisted), or, when the client sends
//     "X-GUploader-No-308: yes" as the Go client does, 200 with
//     "X-Http-Status-Code-Override: 308". Neither handshake is documented;
//     both are what the clients require (gensupport resumable.go).
//   - "bytes */TOTAL" and "bytes */*" query the status.
//   - DELETE cancels: 499, and the session is gone.
//   - A session lasts a week; for a week more it answers 410, then 404.
//
// A session is a metadata record plus one blob per chunk, so it survives a
// restart in persistent mode. The chunks are joined into the object's blob
// at the final chunk.

const (
	sessionPrefix  = "session/"
	chunkUnit      = 256 << 10
	sessionLife    = 7 * 24 * time.Hour
	sessionGoneFor = 7 * 24 * time.Hour
)

type sessionChunk struct {
	Blob string `json:"blob"`
	Size int64  `json:"size"`
}

type uploadSession struct {
	ID       string            `json:"id"`
	Bucket   string            `json:"bucket"`
	Meta     uploadMeta        `json:"meta"`
	Query    string            `json:"query"` // the initiating request's preconditions
	Created  time.Time         `json:"created"`
	Chunks   []sessionChunk    `json:"chunks"`
	Received int64             `json:"received"`
	Headers  map[string]string `json:"headers,omitempty"` // X-Goog-Meta-* at initiation
	Done     *objectRecord     `json:"done,omitempty"`
}

func newUploadID() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func getSession(tx Tx, id string) (uploadSession, bool, error) {
	var u uploadSession
	raw, ok := tx.Get(sessionPrefix + id)
	if !ok {
		return u, false, nil
	}
	return u, true, json.Unmarshal(raw, &u)
}

func putSession(tx Tx, u uploadSession) error {
	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	tx.Put(sessionPrefix+u.ID, raw)
	return nil
}

// resumableStart begins a session.
func (s *Server) resumableStart(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	for _, p := range []string{"predefinedAcl", "kmsKeyName"} {
		if q.Get(p) != "" {
			writeError(w, errorf(http.StatusNotImplemented, "notImplemented", "%s is not implemented", p))
			return
		}
	}
	if _, err := parseObjectPreconditions(q, "if"); err != nil {
		writeError(w, err)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, badRequest("the request body could not be read"))
		return
	}
	meta, err := parseUploadMeta(raw)
	if err != nil {
		writeError(w, err)
		return
	}
	if meta.Name == "" {
		meta.Name = q.Get("name")
	}
	if meta.Name == "" {
		writeError(w, required("name"))
		return
	}
	if err := validObjectName(meta.Name); err != nil {
		writeError(w, err)
		return
	}
	if meta.ContentType == "" {
		meta.ContentType = r.Header.Get("X-Upload-Content-Type")
	}
	if err := s.bucketExists(bucket); err != nil {
		writeError(w, err)
		return
	}
	u := uploadSession{ID: newUploadID(), Bucket: bucket, Meta: meta, Created: s.now()}
	pre := url.Values{}
	for k, v := range q {
		if strings.HasPrefix(k, "if") {
			pre[k] = v
		}
	}
	u.Query = pre.Encode()
	for k, v := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-goog-meta-") && len(v) > 0 {
			if u.Headers == nil {
				u.Headers = map[string]string{}
			}
			u.Headers[strings.ToLower(k)[len("x-goog-meta-"):]] = v[0]
		}
	}
	if err := s.meta.Update(func(tx Tx) error { return putSession(tx, u) }); err != nil {
		writeError(w, err)
		return
	}
	loc := url.Values{"uploadType": {"resumable"}, "name": {meta.Name}, "upload_id": {u.ID}}
	w.Header().Set("Location", baseURL(r)+uploadPrefix+"b/"+escape(bucket)+"/o?"+loc.Encode())
	w.Header().Set("X-GUploader-UploadID", u.ID)
	w.WriteHeader(http.StatusOK)
}

var contentRangeRE = regexp.MustCompile(`^bytes (?:(\d+)-(\d+)|\*)/(\d+|\*)$`)

// resumableChunk handles a chunk, a status query or a cancel on a session.
func (s *Server) resumableChunk(w http.ResponseWriter, r *http.Request, id string) {
	var u uploadSession
	var found bool
	if err := s.meta.View(func(tx Tx) error {
		var err error
		u, found, err = getSession(tx, id)
		return err
	}); err != nil {
		writeError(w, err)
		return
	}
	if !found {
		writeError(w, notFound("No such upload session"))
		return
	}
	switch age := s.now().Sub(u.Created); {
	case age >= sessionLife+sessionGoneFor:
		_ = s.meta.Update(func(tx Tx) error { tx.Delete(sessionPrefix + id); return nil })
		writeError(w, notFound("No such upload session"))
		return
	case age >= sessionLife:
		writeError(w, errorf(http.StatusGone, "gone", "The upload session has expired"))
		return
	}
	if r.Method == http.MethodDelete {
		_ = s.meta.Update(func(tx Tx) error { tx.Delete(sessionPrefix + id); return nil })
		writeError(w, errorf(499, "clientClosedRequest", "The upload session was cancelled"))
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		writeError(w, badRequest("A resumable upload takes PUT or POST"))
		return
	}
	if u.Done != nil {
		writeResponse(w, r, http.StatusOK, s.objectJSON(r, *u.Done))
		return
	}
	cr := strings.TrimSpace(r.Header.Get("Content-Range"))
	if cr == "" {
		cr = "bytes */*"
	}
	m := contentRangeRE.FindStringSubmatch(cr)
	if m == nil {
		writeError(w, badRequest("Invalid Content-Range %q", cr))
		return
	}
	total := int64(-1)
	if m[3] != "*" {
		total, _ = strconv.ParseInt(m[3], 10, 64)
	}
	if m[1] != "" {
		first, _ := strconv.ParseInt(m[1], 10, 64)
		last, _ := strconv.ParseInt(m[2], 10, 64)
		if last < first || first > u.Received {
			writeError(w, badRequest("Content-Range %q does not continue from the %d byte(s) persisted", cr, u.Received))
			return
		}
		size := last - first + 1
		final := total >= 0 && last+1 == total
		if !final && size%chunkUnit != 0 {
			writeError(w, badRequest("Invalid request. There were %d byte(s) in the request body. The number of bytes must be a multiple of %d unless it is the last chunk.", size, chunkUnit))
			return
		}
		// Persisted bytes are immutable: skip what was already received.
		if skip := u.Received - first; skip > 0 {
			if _, err := io.CopyN(io.Discard, r.Body, skip); err != nil {
				writeError(w, badRequest("the chunk ended early"))
				return
			}
		}
		if rest := last + 1 - u.Received; rest > 0 {
			b, err := s.blobs.Write(io.LimitReader(r.Body, rest))
			if err != nil {
				writeError(w, err)
				return
			}
			if b.Size != rest {
				writeError(w, badRequest("the chunk carried %d byte(s), not the %d its Content-Range declares", b.Size, rest))
				return
			}
			u.Chunks = append(u.Chunks, sessionChunk{Blob: b.ID, Size: b.Size})
			u.Received += b.Size
			if err := s.meta.Update(func(tx Tx) error { return putSession(tx, u) }); err != nil {
				writeError(w, err)
				return
			}
		}
	}
	if total >= 0 && u.Received > total {
		writeError(w, badRequest("%d byte(s) persisted, more than the declared total %d", u.Received, total))
		return
	}
	if total < 0 || u.Received < total {
		incomplete(w, r, u.Received)
		return
	}
	s.resumableFinish(w, r, u)
}

// incomplete says how many bytes are persisted, in whichever form the client
// asked for.
func incomplete(w http.ResponseWriter, r *http.Request, received int64) {
	if received > 0 {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", received-1))
	}
	if strings.EqualFold(r.Header.Get("X-GUploader-No-308"), "yes") {
		w.Header().Set("X-Http-Status-Code-Override", "308")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusPermanentRedirect)
}

// resumableFinish joins the chunks into the object's bytes and commits it.
func (s *Server) resumableFinish(w http.ResponseWriter, r *http.Request, u uploadSession) {
	readers := make([]io.Reader, 0, len(u.Chunks))
	closers := make([]io.Closer, 0, len(u.Chunks))
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()
	for _, c := range u.Chunks {
		f, err := s.blobs.Open(c.Blob)
		if err != nil {
			writeError(w, err)
			return
		}
		readers, closers = append(readers, f), append(closers, f)
	}
	blob, err := s.blobs.Write(io.MultiReader(readers...))
	if err != nil {
		writeError(w, err)
		return
	}
	if blob.Size != u.Received {
		writeError(w, errorf(http.StatusInternalServerError, "backendError", "the upload's bytes could not be joined"))
		return
	}
	meta := u.Meta
	if len(u.Headers) > 0 && meta.Metadata == nil {
		meta.Metadata = map[string]string{}
	}
	for k, v := range u.Headers {
		if _, set := meta.Metadata[k]; !set {
			meta.Metadata[k] = v
		}
	}
	q, _ := url.ParseQuery(u.Query)
	pre, _ := parseObjectPreconditions(q, "if")
	wantMD5, wantCRC, err := expectedHashes(r, meta)
	if err != nil {
		writeError(w, err)
		return
	}
	o, err := s.finalizeObject(u.Bucket, meta, blob, pre, wantMD5, wantCRC)
	if err != nil {
		writeError(w, err)
		return
	}
	u.Done = &o
	_ = s.meta.Update(func(tx Tx) error { return putSession(tx, u) })
	writeResponse(w, r, http.StatusOK, s.objectJSON(r, o))
}
