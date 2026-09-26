// Package storage is CloudBurrow's Cloud Storage server, built to Google's
// spec to replace fake-gcs-server (#485; the ADR-0005 evaluation amendment
// says why). One handler serves every Cloud Storage surface on one host, as
// storage.googleapis.com does:
//
//   - the JSON API under /storage/v1/, its uploads under /upload/storage/v1/
//     and /resumable/upload/storage/v1/, and media under /download/storage/v1/;
//   - the batch endpoint, /batch/storage/v1;
//   - the XML API, path-style (/{bucket}/{object}) and virtual-hosted
//     ({bucket}.<host>).
//
// Every JSON API method comes from Google's discovery document. One that is
// not built yet answers 501 notImplemented naming it, never a stub.
package storage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Server serves Cloud Storage.
type Server struct {
	methods  []method
	handlers map[string]http.HandlerFunc
	hosts    []string
	meta     MetaStore
	blobs    BlobStore
	now      func() time.Time
	genMu    sync.Mutex
	lastGen  int64
}

// Options configure a Server.
type Options struct {
	// Hosts are the names clients reach the server by, for virtual-hosted
	// XML requests (<bucket>.<host>); a request to any other host is
	// path-style.
	Hosts []string
	// Meta and Blobs hold the state; nil means in memory.
	Meta  MetaStore
	Blobs BlobStore
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// NewServer returns the server.
func NewServer(o Options) (*Server, error) {
	ms, err := discoveryMethods()
	if err != nil {
		return nil, err
	}
	s := &Server{methods: ms, handlers: map[string]http.HandlerFunc{}, hosts: o.Hosts, meta: o.Meta, blobs: o.Blobs, now: o.Now}
	if s.meta == nil {
		s.meta = NewMemMetaStore()
	}
	if s.blobs == nil {
		s.blobs = NewMemBlobStore(Limits{})
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.handlers["storage.buckets.insert"] = s.bucketsInsert
	s.handlers["storage.buckets.get"] = s.bucketsGet
	s.handlers["storage.buckets.list"] = s.bucketsList
	s.handlers["storage.buckets.patch"] = s.bucketsModify(false)
	s.handlers["storage.buckets.update"] = s.bucketsModify(true)
	s.handlers["storage.buckets.delete"] = s.bucketsDelete
	s.handlers["storage.objects.insert"] = s.metadataOnlyInsert
	s.handlers["storage.objects.get"] = s.objectsGet
	s.handlers["storage.objects.delete"] = s.objectsDelete
	s.handlers["storage.objects.patch"] = s.objectsModify(false)
	s.handlers["storage.objects.update"] = s.objectsModify(true)
	for id := range s.handlers {
		if methodStatus[id] != built {
			return nil, fmt.Errorf("%s has a handler but methodStatus does not say it is built", id)
		}
	}
	return s, nil
}

// uploadPathRE is objects.insert's media path, b/{bucket}/o.
var uploadPathRE = pathRE("b/{bucket}/o")

const (
	jsonPrefix      = "/storage/v1/"
	uploadPrefix    = "/upload/storage/v1/"
	resumablePrefix = "/resumable/upload/storage/v1/"
	downloadPrefix  = "/download/storage/v1/"
	batchPath       = "/batch/storage/v1"
)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	switch {
	case strings.HasPrefix(path, jsonPrefix):
		if err := checkSystemParams(r); err != nil {
			apierror.WriteJSON(w, err)
			return
		}
		s.serveJSON(w, r, strings.TrimPrefix(path, jsonPrefix), false)
	case strings.HasPrefix(path, uploadPrefix), strings.HasPrefix(path, resumablePrefix):
		if err := checkSystemParams(r); err != nil {
			apierror.WriteJSON(w, err)
			return
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(path, resumablePrefix), uploadPrefix)
		if r.Method == http.MethodPost && uploadPathRE.MatchString(rest) {
			s.objectsInsertUpload(w, r)
			return
		}
		s.serveJSON(w, r, rest, true)
	case strings.HasPrefix(path, downloadPrefix):
		s.serveDownload(w, r)
	case path == batchPath || strings.HasPrefix(path, batchPath+"/"):
		apierror.WriteJSON(w, apierror.Unimplemented("the batch endpoint %s is not implemented yet", batchPath))
	default:
		s.serveXML(w, r)
	}
}

// serveJSON routes a JSON API request by the discovery document. upload
// restricts the match to methods that take media.
func (s *Server) serveJSON(w http.ResponseWriter, r *http.Request, rest string, upload bool) {
	for _, m := range s.methods {
		if m.Verb != r.Method || !m.re.MatchString(rest) || (upload && !m.Upload) {
			continue
		}
		if h, ok := s.handlers[m.ID]; ok && methodStatus[m.ID] == built {
			h(w, r)
			return
		}
		what := m.ID
		if upload {
			what += " (media upload)"
		}
		apierror.WriteJSON(w, apierror.Unimplemented("%s is not implemented", what))
		return
	}
	apierror.WriteJSON(w, apierror.NotFound("no Cloud Storage JSON API method is bound to %s %s", r.Method, r.URL.Path))
}

// checkSystemParams accepts the system parameters the discovery document
// lists. alt is json or media (media only where a method serves it); fields
// is applied by writeResponse; prettyPrint, quotaUser, userProject, key,
// oauth_token and userIp are accepted and change nothing, since CloudBurrow
// authenticates and bills nothing.
func checkSystemParams(r *http.Request) error {
	switch alt := r.URL.Query().Get("alt"); alt {
	case "", "json", "media":
	default:
		return apierror.InvalidArgument("alt=%s is not supported: use json or media", alt)
	}
	return nil
}

// writeResponse writes v as the JSON API does: trimmed to fields, when the
// request names them, and indented unless prettyPrint=false.
func writeResponse(w http.ResponseWriter, r *http.Request, status int, v any) {
	q := r.URL.Query()
	if spec := q.Get("fields"); spec != "" {
		sel, err := parseFields(spec)
		if err != nil {
			apierror.WriteJSON(w, err)
			return
		}
		v = sel.apply(toGeneric(v))
	}
	var b []byte
	if q.Get("prettyPrint") == "false" {
		b, _ = json.Marshal(v)
	} else {
		b, _ = json.MarshalIndent(v, "", "  ")
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// toGeneric round-trips v through JSON so a field selection can walk it.
func toGeneric(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// baseURL is the scheme and host the request reached this server by. Every
// link in a response (selfLink, mediaLink) is built from it, never from
// storage.googleapis.com, so a link works from wherever the client is: a
// host tool, or a pod in the cluster.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// selfLink and mediaLink are an object's links, as the JSON API spells them.
func selfLink(r *http.Request, bucket, object string) string {
	return fmt.Sprintf("%s%sb/%s/o/%s", baseURL(r), jsonPrefix, escape(bucket), escape(object))
}

func mediaLink(r *http.Request, bucket, object string, generation int64) string {
	return fmt.Sprintf("%s%sb/%s/o/%s?generation=%d&alt=media", baseURL(r), downloadPrefix, escape(bucket), escape(object), generation)
}
