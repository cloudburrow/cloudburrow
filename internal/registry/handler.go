package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var zeroTime time.Time

// Lookup finds the Artifact Registry repository a request's name is in.
// location is the one the request's host names ({location}-docker.pkg.dev
// or {location}-docker.pkg.localhost, or containerd's ns parameter), or ""
// when the host names none, such as 127.0.0.1; Lookup then finds the one
// repository of that project and ID in any location. ok is false when no
// such DOCKER repository exists.
type Lookup func(location, project, repository string) (Repo, bool)

// Handler serves the Docker Registry HTTP API v2 at /v2/.
type Handler struct {
	Store  *Store
	Lookup Lookup
	// Remote, when set, reports whether a repository is a remote one, and
	// the repository it pulls through from (#1546). A manifest read from
	// a remote repository is first copied from its upstream, which shares
	// the blobs, and kept, so the remote repository answers from its cache
	// once the upstream no longer has it; pushes to it are refused.
	Remote func(Repo) (upstream Repo, ok bool)
}

// remote is the upstream of a remote repository; ok is false for any other.
func (h *Handler) remote(repo Repo) (Repo, bool) {
	if h.Remote == nil {
		return Repo{}, false
	}
	return h.Remote(repo)
}

func (h *Handler) readOnly(w http.ResponseWriter, repo Repo) bool {
	up, ok := h.remote(repo)
	if !ok {
		return false
	}
	h.fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", fmt.Sprintf("%s/%s is a remote repository, which is read-only: "+
		"it caches what it pulls from its upstream, %s/%s; push there", repo.Project, repo.Repository, up.Project, up.Repository))
	return true
}

// HostSuffixes are the registry hosts whose first label is
// {location}-docker: Artifact Registry's, and the loopback alias a local
// docker client resolves without any DNS configuration (RFC 6761).
var HostSuffixes = []string{"-docker.pkg.dev", "-docker.pkg.localhost"}

// LocationOf is the location a registry host names, or "".
func LocationOf(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	for _, s := range HostSuffixes {
		if loc, ok := strings.CutSuffix(host, s); ok && loc != "" && !strings.Contains(loc, ".") {
			return loc
		}
	}
	return ""
}

var (
	manifestPath = regexp.MustCompile(`^/v2/(.+)/manifests/([^/]+)$`)
	uploadPath   = regexp.MustCompile(`^/v2/(.+)/blobs/uploads/([^/]*)$`)
	blobPath     = regexp.MustCompile(`^/v2/(.+)/blobs/([^/]+)$`)
	tagsPath     = regexp.MustCompile(`^/v2/(.+)/tags/list$`)
)

func (h *Handler) fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"code": code, "message": msg}}})
}

func (h *Handler) failErr(w http.ResponseWriter, err error) {
	var bu *BlobUnknownError
	var mbu *ManifestBlobUnknownError
	var mi *ManifestInvalidError
	switch {
	case errors.As(err, &bu):
		h.fail(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", err.Error())
	case errors.As(err, &mbu):
		h.fail(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", err.Error())
	case errors.As(err, &mi):
		h.fail(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
	case errors.Is(err, ErrBlobUnknown):
		h.fail(w, http.StatusNotFound, "BLOB_UNKNOWN", err.Error())
	case errors.Is(err, ErrManifestUnknown):
		h.fail(w, http.StatusNotFound, "MANIFEST_UNKNOWN", err.Error())
	case errors.Is(err, ErrUploadUnknown):
		h.fail(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", err.Error())
	case errors.Is(err, ErrDigestInvalid):
		h.fail(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
	case errors.Is(err, ErrTagInvalid):
		h.fail(w, http.StatusBadRequest, "TAG_INVALID", err.Error())
	case errors.Is(err, ErrNameInvalid):
		h.fail(w, http.StatusNotFound, "NAME_UNKNOWN", "repository name not known to registry")
	default:
		h.fail(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
	}
}

// IsRegistryPath reports whether a request path is the registry API's:
// /v2/, the catalog, or a name's manifests, blobs, uploads or tags. Cloud
// Run's REST API shares the /v2/ prefix on the same port, under
// /v2/projects/{project}/locations/, and is never one of these.
func IsRegistryPath(p string) bool {
	if p == "/v2" || p == "/v2/" || p == "/v2/_catalog" {
		return true
	}
	if parts := strings.SplitN(p, "/", 6); len(parts) >= 5 && parts[2] == "projects" && parts[4] == "locations" {
		return false
	}
	return manifestPath.MatchString(p) || uploadPath.MatchString(p) || blobPath.MatchString(p) || tagsPath.MatchString(p)
}

// location is the request's location: its host's, or containerd's ns
// parameter when it reaches the registry as a mirror.
func location(r *http.Request) string {
	if ns := r.URL.Query().Get("ns"); ns != "" {
		if l := LocationOf(ns); l != "" {
			return l
		}
	}
	return LocationOf(r.Host)
}

// resolve splits a name into its repository and image.
func (h *Handler) resolve(r *http.Request, name string) (Repo, string, bool) {
	parts := strings.SplitN(name, "/", 3)
	if len(parts) != 3 || !ValidImage(parts[2]) {
		return Repo{}, "", false
	}
	repo, ok := h.Lookup(location(r), parts[0], parts[1])
	return repo, parts[2], ok
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	p := r.URL.Path
	if p == "/v2" || p == "/v2/" {
		// No authentication is required: CloudBurrow authenticates
		// nothing, and a client that sends the credential helper's token
		// is answered all the same.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
		return
	}
	if p == "/v2/_catalog" {
		h.catalog(w, r)
		return
	}
	if m := tagsPath.FindStringSubmatch(p); m != nil {
		h.withRepo(w, r, m[1], func(repo Repo, image string) { h.tags(w, r, repo, image, m[1]) })
		return
	}
	if m := manifestPath.FindStringSubmatch(p); m != nil {
		h.withRepo(w, r, m[1], func(repo Repo, image string) { h.manifest(w, r, repo, image, m[1], m[2]) })
		return
	}
	if m := uploadPath.FindStringSubmatch(p); m != nil {
		h.withRepo(w, r, m[1], func(repo Repo, image string) {
			if !h.readOnly(w, repo) {
				h.upload(w, r, m[1], m[2])
			}
		})
		return
	}
	if m := blobPath.FindStringSubmatch(p); m != nil {
		h.withRepo(w, r, m[1], func(repo Repo, image string) { h.blob(w, r, m[2]) })
		return
	}
	h.fail(w, http.StatusNotFound, "NAME_UNKNOWN", "repository name not known to registry")
}

func (h *Handler) withRepo(w http.ResponseWriter, r *http.Request, name string, f func(Repo, string)) {
	repo, image, ok := h.resolve(r, name)
	if !ok {
		h.fail(w, http.StatusNotFound, "NAME_UNKNOWN", fmt.Sprintf("repository name not known to registry: %s; "+
			"create its Artifact Registry repository (format DOCKER) first", name))
		return
	}
	f(repo, image)
}

// page applies the n and last parameters to a sorted list, and sets the
// Link header for the next page.
func page(w http.ResponseWriter, r *http.Request, all []string) []string {
	q := r.URL.Query()
	if last := q.Get("last"); last != "" {
		i := sort.SearchStrings(all, last)
		if i < len(all) && all[i] == last {
			i++
		}
		all = all[i:]
	}
	if n, err := strconv.Atoi(q.Get("n")); err == nil && n >= 0 && n < len(all) {
		all = all[:n]
		if n > 0 {
			next := *r.URL
			nq := next.Query()
			nq.Set("last", all[n-1])
			next.RawQuery = nq.Encode()
			w.Header().Set("Link", fmt.Sprintf("<%s>; rel=\"next\"", next.RequestURI()))
		}
	}
	if all == nil {
		all = []string{}
	}
	return all
}

func (h *Handler) catalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}
	all := h.Store.Catalog(location(r))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"repositories": page(w, r, all)})
}

func (h *Handler) tags(w http.ResponseWriter, r *http.Request, repo Repo, image, name string) {
	if r.Method != http.MethodGet {
		h.fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}
	tags, err := h.Store.Tags(repo, image)
	if err != nil {
		h.failErr(w, err)
		return
	}
	all := make([]string, 0, len(tags))
	for t := range tags {
		all = append(all, t)
	}
	sort.Strings(all)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "tags": page(w, r, all)})
}

func (h *Handler) manifest(w http.ResponseWriter, r *http.Request, repo Repo, image, name, ref string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if up, ok := h.remote(repo); ok && up.Repository != "" {
			// What the upstream holds now wins, so a moved tag is
			// followed; what it no longer holds is served from the cache.
			_ = h.Store.CopyManifest(up, repo, image, ref)
		}
		b, d, mt, err := h.Store.GetManifest(repo, image, ref)
		if err != nil {
			h.failErr(w, err)
			return
		}
		w.Header().Set("Content-Type", mt)
		w.Header().Set("Docker-Content-Digest", d)
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(b)
		}
	case http.MethodPut:
		if h.readOnly(w, repo) {
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20+1))
		if err != nil {
			h.failErr(w, err)
			return
		}
		if len(b) > 4<<20 {
			h.fail(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "manifest is larger than 4 MiB")
			return
		}
		d, err := h.Store.PutManifest(repo, image, ref, r.Header.Get("Content-Type"), b)
		if err != nil {
			h.failErr(w, err)
			return
		}
		w.Header().Set("Location", "/v2/"+name+"/manifests/"+d)
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		var err error
		if ValidDigest(ref) {
			err = h.Store.DeleteManifest(repo, image, ref)
		} else {
			err = h.Store.DeleteTag(repo, image, ref)
		}
		if err != nil {
			h.failErr(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		h.fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

func (h *Handler) blob(w http.ResponseWriter, r *http.Request, digest string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		f, err := h.Store.OpenBlob(digest)
		if err != nil {
			h.failErr(w, err)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Accept-Ranges", "bytes")
		// ServeContent answers Range requests, which containerd and
		// resuming clients send.
		http.ServeContent(w, r, "", zeroTime, f)
	case http.MethodDelete:
		if err := h.Store.DeleteBlob(digest); err != nil {
			h.failErr(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		h.fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

func (h *Handler) uploadLocation(w http.ResponseWriter, name, id string, size int64) {
	w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+id)
	w.Header().Set("Docker-Upload-UUID", id)
	end := size - 1
	if end < 0 {
		end = 0
	}
	w.Header().Set("Range", fmt.Sprintf("0-%d", end))
}

func (h *Handler) blobCreated(w http.ResponseWriter, name, digest string) {
	w.Header().Set("Location", "/v2/"+name+"/blobs/"+digest)
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) upload(w http.ResponseWriter, r *http.Request, name, id string) {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && id == "":
		// A cross-repository mount succeeds whenever the blob is stored:
		// blobs are shared by digest.
		if mount := q.Get("mount"); mount != "" {
			if _, err := h.Store.BlobSize(mount); err == nil {
				h.blobCreated(w, name, mount)
				return
			}
		}
		if d := q.Get("digest"); d != "" {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				h.failErr(w, err)
				return
			}
			if Digest(b) != d {
				h.failErr(w, ErrDigestInvalid)
				return
			}
			if _, err := h.Store.PutBlob(b); err != nil {
				h.failErr(w, err)
				return
			}
			h.blobCreated(w, name, d)
			return
		}
		nid, err := h.Store.StartUpload()
		if err != nil {
			h.failErr(w, err)
			return
		}
		h.uploadLocation(w, name, nid, 0)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodGet && id != "":
		size, err := h.Store.UploadSize(id)
		if err != nil {
			h.failErr(w, err)
			return
		}
		h.uploadLocation(w, name, id, size)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPatch && id != "":
		size, err := h.Store.UploadSize(id)
		if err != nil {
			h.failErr(w, err)
			return
		}
		if cr := r.Header.Get("Content-Range"); cr != "" {
			var start, end int64
			if _, err := fmt.Sscanf(cr, "%d-%d", &start, &end); err != nil || start != size {
				h.uploadLocation(w, name, id, size)
				h.fail(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "Content-Range does not continue the upload")
				return
			}
		}
		size, err = h.Store.AppendUpload(id, r.Body)
		if err != nil {
			h.failErr(w, err)
			return
		}
		h.uploadLocation(w, name, id, size)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut && id != "":
		if _, err := h.Store.AppendUpload(id, r.Body); err != nil {
			h.failErr(w, err)
			return
		}
		d := q.Get("digest")
		if err := h.Store.FinishUpload(id, d); err != nil {
			h.failErr(w, err)
			return
		}
		h.blobCreated(w, name, d)
	case r.Method == http.MethodDelete && id != "":
		if err := h.Store.CancelUpload(id); err != nil {
			h.failErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		h.fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

// ImageURL is the docker reference of an image in repo, under
// Artifact Registry's host.
func ImageURL(repo Repo, image string) string {
	return fmt.Sprintf("%s-docker.pkg.dev/%s/%s/%s", repo.Location, repo.Project, repo.Repository, image)
}

// EscapePackage is an image path as an Artifact Registry package ID: its
// slashes escaped, as the API names nested images.
func EscapePackage(image string) string { return strings.ReplaceAll(url.PathEscape(image), "/", "%2F") }

// UnescapePackage reverses EscapePackage.
func UnescapePackage(id string) string {
	if s, err := url.PathUnescape(id); err == nil {
		return s
	}
	return id
}
