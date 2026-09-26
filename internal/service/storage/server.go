// Package storage is CloudBurrow's Cloud Storage server, built to Google's
// spec (#485; the ADR-0005 evaluation amendment says why), and the only
// Cloud Storage backend (#519). One handler serves every Cloud Storage surface on one host, as
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
	"crypto/rsa"
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
	// publisher and notify deliver Pub/Sub notifications (#506); nil
	// without a Pub/Sub emulator.
	publisher Publisher
	notify    *notifier
	// signingKeys verify RSA signed URLs, by service account (#509).
	signingKeys map[string]*rsa.PublicKey
	// observe, faults and ring measure and fault requests (#513).
	observe func(Call)
	faults  Faulter
	ring    callRing
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
	// Publisher delivers Pub/Sub notifications (#506). Without one,
	// notificationConfigs cannot be created.
	Publisher Publisher
	// SigningKeys are the registered public keys RSA signed URLs are
	// verified against, by service account email (#509). A signed URL for
	// any other account is refused.
	SigningKeys map[string]*rsa.PublicKey
	// Observe is told of every request once it is served (#513); nil means
	// none. It never sees a query string or a body.
	Observe func(Call)
	// Faults may fail or delay requests before they are served (#513).
	Faults Faulter
}

// NewServer returns the server.
func NewServer(o Options) (*Server, error) {
	ms, err := discoveryMethods()
	if err != nil {
		return nil, err
	}
	s := &Server{methods: ms, handlers: map[string]http.HandlerFunc{}, hosts: o.Hosts, meta: o.Meta, blobs: o.Blobs, now: o.Now}
	s.signingKeys, s.observe, s.faults = o.SigningKeys, o.Observe, o.Faults
	if o.Publisher != nil {
		s.publisher, s.notify = o.Publisher, &notifier{wake: make(chan struct{}, 1)}
	}
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
	s.handlers["storage.objects.list"] = s.objectsList
	s.handlers["storage.objects.compose"] = s.objectsCompose
	s.handlers["storage.objects.copy"] = s.objectsCopy
	s.handlers["storage.objects.rewrite"] = s.objectsRewrite
	s.handlers["storage.objects.move"] = s.objectsMove
	s.handlers["storage.objects.restore"] = s.objectsRestore
	s.handlers["storage.buckets.restore"] = s.bucketsRestore
	s.handlers["storage.buckets.lockRetentionPolicy"] = s.bucketsLockRetentionPolicy
	s.handlers["storage.buckets.getIamPolicy"] = s.bucketsGetIamPolicy
	s.handlers["storage.buckets.getStorageLayout"] = s.bucketsGetStorageLayout
	s.handlers["storage.managedFolders.list"] = s.managedFoldersList
	s.handlers["storage.buckets.setIamPolicy"] = s.bucketsSetIamPolicy
	s.handlers["storage.buckets.testIamPermissions"] = s.bucketsTestIamPermissions
	s.handlers["storage.projects.hmacKeys.create"] = s.hmacKeysCreate
	s.handlers["storage.projects.hmacKeys.get"] = s.hmacKeysGet
	s.handlers["storage.projects.hmacKeys.update"] = s.hmacKeysUpdate
	s.handlers["storage.projects.hmacKeys.delete"] = s.hmacKeysDelete
	s.handlers["storage.projects.hmacKeys.list"] = s.hmacKeysList
	s.handlers["storage.projects.serviceAccount.get"] = s.serviceAccountGet
	s.handlers["storage.notifications.insert"] = s.notificationsInsert
	s.handlers["storage.notifications.get"] = s.notificationsGet
	s.handlers["storage.notifications.list"] = s.notificationsList
	s.handlers["storage.notifications.delete"] = s.notificationsDelete
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
	defer s.wakeNotifier()
	s.observed(w, r, s.route)
}

// route dispatches one request.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	if s.serveCORS(w, r) {
		return
	}
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
		if uploadPathRE.MatchString(rest) {
			q := r.URL.Query()
			switch {
			case q.Get("upload_id") != "":
				s.resumableChunk(w, r, q.Get("upload_id"))
				return
			case r.Method == http.MethodPost && q.Get("uploadType") == "resumable":
				s.resumableStart(w, r, pathVar(r, strings.TrimSuffix(path, rest), 1))
				return
			case r.Method == http.MethodPost:
				s.objectsInsertUpload(w, r)
				return
			}
		}
		s.serveJSON(w, r, rest, true)
	case strings.HasPrefix(path, downloadPrefix):
		s.serveDownload(w, r)
	case path == batchPath || strings.HasPrefix(path, batchPath+"/"):
		s.serveBatch(w, r)
	case path == lifecyclePath:
		s.serveLifecycle(w, r)
	case path == resetPath:
		s.serveReset(w, r)
	case path == statePath:
		s.serveState(w, r)
	case path == eventsPath:
		s.serveEvents(w, r)
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
// baseURL is where the request reached the server. Without a request (a
// notification's payload, built after the fact) it is Google's, as the
// payloads Google sends carry it.
func baseURL(r *http.Request) string {
	if r == nil {
		return "https://storage.googleapis.com"
	}
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
