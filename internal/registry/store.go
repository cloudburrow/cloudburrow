// Package registry is a Docker registry: the Docker Registry HTTP API v2,
// as the OCI distribution specification describes it, over a
// content-addressed store on disk (#1383). Artifact Registry's DOCKER
// repositories are served from it, so `docker push`, `docker pull`, crane
// and a kind node's containerd reach {location}-docker.pkg.dev/{project}/
// {repository}/{image} locally.
//
// The store's layout, under its root:
//
//	blobs/sha256/<hex>                       every blob and manifest, by digest
//	uploads/<uuid>                           an upload in progress
//	repos/<location>/<project>/<repository>/<image>/_manifests/
//	    revisions/sha256/<hex>               a manifest the image holds; its content is the media type
//	    tags/<tag>                           a tag; its content is the digest it names
//
// Blobs are shared across repositories, as a content-addressed store's are;
// a manifest belongs to an image only once it is pushed there.
package registry

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors the store returns, which the HTTP API maps to its error codes.
var (
	ErrBlobUnknown     = errors.New("blob unknown to registry")
	ErrManifestUnknown = errors.New("manifest unknown")
	ErrUploadUnknown   = errors.New("blob upload unknown to registry")
	ErrDigestInvalid   = errors.New("provided digest did not match uploaded content")
	ErrNameInvalid     = errors.New("invalid repository name")
	ErrTagInvalid      = errors.New("invalid tag")
)

// Store is the registry's content on disk.
type Store struct {
	root string
	mu   sync.Mutex
}

// Open returns the store rooted at dir, creating it.
func Open(dir string) (*Store, error) {
	for _, d := range []string{"blobs/sha256", "uploads", "repos"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{root: dir}, nil
}

// Repo names one Artifact Registry repository's images.
type Repo struct {
	Location, Project, Repository string
}

func (r Repo) dir(root string) string {
	return filepath.Join(root, "repos", r.Location, r.Project, r.Repository)
}

var (
	// componentRE is the distribution spec's path component.
	componentRE = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*$`)
	tagRE       = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	digestRE    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// ValidImage reports whether image is a valid image path inside a
// repository: one or more slash-separated components.
func ValidImage(image string) bool {
	if image == "" || len(image) > 255 {
		return false
	}
	for _, c := range strings.Split(image, "/") {
		if !componentRE.MatchString(c) || strings.HasPrefix(c, "_") {
			return false
		}
	}
	return true
}

// ValidTag reports whether t is a tag the distribution spec accepts.
func ValidTag(t string) bool { return tagRE.MatchString(t) }

// ValidDigest reports whether d is a sha256 digest, the one algorithm
// stored.
func ValidDigest(d string) bool { return digestRE.MatchString(d) }

func (s *Store) blobPath(digest string) string {
	return filepath.Join(s.root, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
}

func (s *Store) imageDir(r Repo, image string) string {
	return filepath.Join(r.dir(s.root), filepath.FromSlash(image), "_manifests")
}

// BlobSize returns a blob's size, or ErrBlobUnknown.
func (s *Store) BlobSize(digest string) (int64, error) {
	if !ValidDigest(digest) {
		return 0, ErrBlobUnknown
	}
	fi, err := os.Stat(s.blobPath(digest))
	if err != nil {
		return 0, ErrBlobUnknown
	}
	return fi.Size(), nil
}

// OpenBlob opens a blob for reading.
func (s *Store) OpenBlob(digest string) (*os.File, error) {
	if !ValidDigest(digest) {
		return nil, ErrBlobUnknown
	}
	f, err := os.Open(s.blobPath(digest))
	if err != nil {
		return nil, ErrBlobUnknown
	}
	return f, nil
}

// ReadBlob reads a whole blob.
func (s *Store) ReadBlob(digest string) ([]byte, error) {
	f, err := s.OpenBlob(digest)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// DeleteBlob removes a blob.
func (s *Store) DeleteBlob(digest string) error {
	if !ValidDigest(digest) {
		return ErrBlobUnknown
	}
	if err := os.Remove(s.blobPath(digest)); err != nil {
		return ErrBlobUnknown
	}
	return nil
}

// Digest is the sha256 digest of b.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PutBlob stores b, returning its digest.
func (s *Store) PutBlob(b []byte) (string, error) {
	d := Digest(b)
	if _, err := os.Stat(s.blobPath(d)); err == nil {
		return d, nil
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "uploads"), "put-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	tmp.Close()
	return d, os.Rename(tmp.Name(), s.blobPath(d))
}

// StartUpload begins an upload and returns its ID.
func (s *Store) StartUpload() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	f, err := os.Create(filepath.Join(s.root, "uploads", id))
	if err != nil {
		return "", err
	}
	return id, f.Close()
}

func (s *Store) uploadPath(id string) (string, error) {
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return "", ErrUploadUnknown
	}
	return filepath.Join(s.root, "uploads", id), nil
}

// UploadSize is how many bytes an upload holds so far.
func (s *Store) UploadSize(id string) (int64, error) {
	p, err := s.uploadPath(id)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return 0, ErrUploadUnknown
	}
	return fi.Size(), nil
}

// AppendUpload appends r to an upload and returns its new size.
func (s *Store) AppendUpload(id string, r io.Reader) (int64, error) {
	p, err := s.uploadPath(id)
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return 0, ErrUploadUnknown
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// FinishUpload verifies an upload against digest and stores it as a blob.
func (s *Store) FinishUpload(id, digest string) error {
	p, err := s.uploadPath(id)
	if err != nil {
		return err
	}
	if !ValidDigest(digest) {
		return ErrDigestInvalid
	}
	f, err := os.Open(p)
	if err != nil {
		return ErrUploadUnknown
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return err
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest {
		return ErrDigestInvalid
	}
	return os.Rename(p, s.blobPath(digest))
}

// CancelUpload removes an upload.
func (s *Store) CancelUpload(id string) error {
	p, err := s.uploadPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return ErrUploadUnknown
	}
	return nil
}

// Manifest media types the registry accepts.
const (
	MediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeOCIManifest        = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex           = "application/vnd.oci.image.index.v1+json"
)

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType    string            `json:"mediaType,omitempty"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Platform     *Platform         `json:"platform,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// Platform is an index entry's platform.
type Platform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
}

// ManifestDoc is the union of an image manifest and an image index.
type ManifestDoc struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        *Descriptor       `json:"config,omitempty"`
	Layers        []Descriptor      `json:"layers,omitempty"`
	Manifests     []Descriptor      `json:"manifests,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// IsIndex reports whether a media type is an image index or manifest list.
func IsIndex(mediaType string) bool {
	return mediaType == MediaTypeOCIIndex || mediaType == MediaTypeDockerManifestList
}

// ParseManifest checks a manifest's content against its media type and
// returns it parsed; the media type is the content's own when contentType
// is empty.
func ParseManifest(contentType string, b []byte) (*ManifestDoc, string, error) {
	var m ManifestDoc
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, "", fmt.Errorf("manifest is not JSON: %v", err)
	}
	mt := contentType
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	if mt == "" || mt == "application/json" || mt == "application/octet-stream" {
		mt = m.MediaType
		if mt == "" {
			if m.Manifests != nil {
				mt = MediaTypeOCIIndex
			} else {
				mt = MediaTypeOCIManifest
			}
		}
	}
	switch mt {
	case MediaTypeDockerManifest, MediaTypeOCIManifest:
		if m.SchemaVersion != 2 || m.Config == nil {
			return nil, "", errors.New("an image manifest needs schemaVersion 2 and a config")
		}
	case MediaTypeDockerManifestList, MediaTypeOCIIndex:
		if m.SchemaVersion != 2 {
			return nil, "", errors.New("an image index needs schemaVersion 2")
		}
	default:
		return nil, "", fmt.Errorf("manifest media type %q is not supported", mt)
	}
	if m.MediaType != "" && m.MediaType != mt {
		return nil, "", fmt.Errorf("manifest mediaType %q does not match Content-Type %q", m.MediaType, mt)
	}
	return &m, mt, nil
}

// PutManifest stores a manifest in an image, under its digest and, when
// ref is a tag, that tag. Every blob or manifest it references must
// already be stored. It returns the manifest's digest.
func (s *Store) PutManifest(r Repo, image, ref, contentType string, b []byte) (string, error) {
	if !ValidImage(image) {
		return "", ErrNameInvalid
	}
	m, mt, err := ParseManifest(contentType, b)
	if err != nil {
		return "", &ManifestInvalidError{err}
	}
	d := Digest(b)
	isDigest := strings.HasPrefix(ref, "sha256:") || strings.Contains(ref, ":")
	if isDigest && ref != d {
		return "", ErrDigestInvalid
	}
	if !isDigest && !ValidTag(ref) {
		return "", ErrTagInvalid
	}
	var refs []Descriptor
	if m.Config != nil {
		refs = append(refs, *m.Config)
	}
	refs = append(refs, m.Layers...)
	for _, x := range refs {
		if _, err := s.BlobSize(x.Digest); err != nil {
			return "", &BlobUnknownError{x.Digest}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range m.Manifests {
		if _, err := os.Stat(s.revisionPath(r, image, x.Digest)); err != nil {
			return "", &ManifestBlobUnknownError{x.Digest}
		}
	}
	if _, err := s.PutBlob(b); err != nil {
		return "", err
	}
	rev := s.revisionPath(r, image, d)
	if err := os.MkdirAll(filepath.Dir(rev), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(rev, []byte(mt), 0o644); err != nil {
		return "", err
	}
	if !isDigest {
		if err := s.setTagLocked(r, image, ref, d); err != nil {
			return "", err
		}
	}
	return d, nil
}

// CopyManifest copies a manifest, by tag or digest, from one repository's
// image to another's, with the manifests an index names; the blobs are
// shared by digest, so none is copied. A tag is set to the same digest.
func (s *Store) CopyManifest(from, to Repo, image, ref string) error {
	b, d, mt, err := s.GetManifest(from, image, ref)
	if err != nil {
		return err
	}
	if IsIndex(mt) {
		m, _, err := ParseManifest(mt, b)
		if err != nil {
			return err
		}
		for _, x := range m.Manifests {
			if err := s.CopyManifest(from, to, image, x.Digest); err != nil {
				return err
			}
		}
	}
	if !ValidDigest(ref) {
		if cur, _, err := s.Resolve(to, image, ref); err == nil && cur == d {
			return nil
		}
	} else if _, _, err := s.Resolve(to, image, ref); err == nil {
		return nil
	}
	_, err = s.PutManifest(to, image, ref, mt, b)
	return err
}

// ManifestInvalidError is a manifest the registry refuses.
type ManifestInvalidError struct{ Err error }

func (e *ManifestInvalidError) Error() string { return "manifest invalid: " + e.Err.Error() }

// BlobUnknownError is a manifest naming a blob that is not stored.
type BlobUnknownError struct{ Digest string }

func (e *BlobUnknownError) Error() string { return "blob unknown to registry: " + e.Digest }

// ManifestBlobUnknownError is an index naming a manifest the image lacks.
type ManifestBlobUnknownError struct{ Digest string }

func (e *ManifestBlobUnknownError) Error() string { return "manifest unknown: " + e.Digest }

func (s *Store) revisionPath(r Repo, image, digest string) string {
	return filepath.Join(s.imageDir(r, image), "revisions", "sha256", strings.TrimPrefix(digest, "sha256:"))
}

func (s *Store) tagPath(r Repo, image, tag string) string {
	return filepath.Join(s.imageDir(r, image), "tags", tag)
}

// Resolve turns a tag or digest into the digest and media type of a
// manifest the image holds.
func (s *Store) Resolve(r Repo, image, ref string) (digest, mediaType string, err error) {
	if !ValidImage(image) {
		return "", "", ErrNameInvalid
	}
	digest = ref
	if !ValidDigest(ref) {
		if !ValidTag(ref) {
			return "", "", ErrManifestUnknown
		}
		b, err := os.ReadFile(s.tagPath(r, image, ref))
		if err != nil {
			return "", "", ErrManifestUnknown
		}
		digest = strings.TrimSpace(string(b))
	}
	mt, err := os.ReadFile(s.revisionPath(r, image, digest))
	if err != nil {
		return "", "", ErrManifestUnknown
	}
	return digest, string(mt), nil
}

// GetManifest returns a manifest by tag or digest.
func (s *Store) GetManifest(r Repo, image, ref string) (b []byte, digest, mediaType string, err error) {
	digest, mediaType, err = s.Resolve(r, image, ref)
	if err != nil {
		return nil, "", "", err
	}
	b, err = s.ReadBlob(digest)
	if err != nil {
		return nil, "", "", ErrManifestUnknown
	}
	return b, digest, mediaType, nil
}

// SetTag points a tag at a manifest the image holds.
func (s *Store) SetTag(r Repo, image, tag, digest string) error {
	if !ValidTag(tag) {
		return ErrTagInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.revisionPath(r, image, digest)); err != nil {
		return ErrManifestUnknown
	}
	return s.setTagLocked(r, image, tag, digest)
}

func (s *Store) setTagLocked(r Repo, image, tag, digest string) error {
	p := s.tagPath(r, image, tag)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(digest), 0o644)
}

// DeleteTag removes a tag; the manifest stays.
func (s *Store) DeleteTag(r Repo, image, tag string) error {
	if !ValidTag(tag) {
		return ErrManifestUnknown
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.tagPath(r, image, tag)); err != nil {
		return ErrManifestUnknown
	}
	return nil
}

// DeleteManifest removes a manifest from an image, and every tag naming
// it. The image itself goes when it holds no manifest.
func (s *Store) DeleteManifest(r Repo, image, digest string) error {
	if !ValidDigest(digest) {
		return ErrManifestUnknown
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.revisionPath(r, image, digest)); err != nil {
		return ErrManifestUnknown
	}
	tags, _ := s.tagsLocked(r, image)
	for t, d := range tags {
		if d == digest {
			_ = os.Remove(s.tagPath(r, image, t))
		}
	}
	if ents, _ := os.ReadDir(filepath.Dir(s.revisionPath(r, image, digest))); len(ents) == 0 {
		_ = os.RemoveAll(filepath.Join(r.dir(s.root), filepath.FromSlash(image)))
		s.pruneEmpty(filepath.Dir(filepath.Join(r.dir(s.root), filepath.FromSlash(image))), r.dir(s.root))
	}
	return nil
}

// pruneEmpty removes empty directories from dir up to, not including, stop.
func (s *Store) pruneEmpty(dir, stop string) {
	for dir != stop && strings.HasPrefix(dir, stop) {
		if ents, err := os.ReadDir(dir); err != nil || len(ents) > 0 {
			return
		}
		_ = os.Remove(dir)
		dir = filepath.Dir(dir)
	}
}

// DeleteImage removes an image: every manifest and tag it holds.
func (s *Store) DeleteImage(r Repo, image string) error {
	if !ValidImage(image) {
		return ErrNameInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(r.dir(s.root), filepath.FromSlash(image))
	if _, err := os.Stat(filepath.Join(dir, "_manifests")); err != nil {
		return ErrNameInvalid
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	s.pruneEmpty(filepath.Dir(dir), r.dir(s.root))
	return nil
}

// DeleteRepository removes everything a repository holds.
func (s *Store) DeleteRepository(r Repo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(r.dir(s.root)); err != nil {
		return err
	}
	s.pruneEmpty(filepath.Dir(r.dir(s.root)), filepath.Join(s.root, "repos"))
	return nil
}

// Tags returns an image's tags and the digest each names.
func (s *Store) Tags(r Repo, image string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tagsLocked(r, image)
}

func (s *Store) tagsLocked(r Repo, image string) (map[string]string, error) {
	if !ValidImage(image) {
		return nil, ErrNameInvalid
	}
	if _, err := os.Stat(s.imageDir(r, image)); err != nil {
		return nil, ErrNameInvalid
	}
	out := map[string]string{}
	ents, _ := os.ReadDir(filepath.Join(s.imageDir(r, image), "tags"))
	for _, e := range ents {
		if b, err := os.ReadFile(filepath.Join(s.imageDir(r, image), "tags", e.Name())); err == nil {
			out[e.Name()] = strings.TrimSpace(string(b))
		}
	}
	return out, nil
}

// Images lists a repository's images, sorted.
func (s *Store) Images(r Repo) []string {
	root := r.dir(s.root)
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if d.Name() == "_manifests" {
			rel, _ := filepath.Rel(root, filepath.Dir(p))
			out = append(out, filepath.ToSlash(rel))
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// Catalog lists every image of a location (every location when it is
// empty), as project/repository/image.
func (s *Store) Catalog(location string) []string {
	var out []string
	locs, _ := os.ReadDir(filepath.Join(s.root, "repos"))
	for _, l := range locs {
		if location != "" && l.Name() != location {
			continue
		}
		projects, _ := os.ReadDir(filepath.Join(s.root, "repos", l.Name()))
		for _, p := range projects {
			repos, _ := os.ReadDir(filepath.Join(s.root, "repos", l.Name(), p.Name()))
			for _, rp := range repos {
				for _, im := range s.Images(Repo{l.Name(), p.Name(), rp.Name()}) {
					out = append(out, p.Name()+"/"+rp.Name()+"/"+im)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// ManifestInfo is one manifest an image holds, for Artifact Registry's
// versions and Docker images.
type ManifestInfo struct {
	Digest    string
	MediaType string
	// Size is the manifest's own size; ImageSize adds its config and
	// layers, or for an index the manifests it lists.
	Size, ImageSize int64
	Tags            []string
	Uploaded        time.Time
	Doc             *ManifestDoc
}

// Manifests lists an image's manifests, newest first.
func (s *Store) Manifests(r Repo, image string) ([]ManifestInfo, error) {
	tags, err := s.Tags(r, image)
	if err != nil {
		return nil, err
	}
	byDigest := map[string][]string{}
	for t, d := range tags {
		byDigest[d] = append(byDigest[d], t)
	}
	dir := filepath.Join(s.imageDir(r, image), "revisions", "sha256")
	ents, _ := os.ReadDir(dir)
	var out []ManifestInfo
	for _, e := range ents {
		d := "sha256:" + e.Name()
		info, err := s.manifestInfo(r, image, d)
		if err != nil {
			continue
		}
		info.Tags = byDigest[d]
		sort.Strings(info.Tags)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Uploaded.Equal(out[j].Uploaded) {
			return out[i].Uploaded.After(out[j].Uploaded)
		}
		return out[i].Digest < out[j].Digest
	})
	return out, nil
}

// Manifest describes one manifest an image holds.
func (s *Store) Manifest(r Repo, image, digest string) (ManifestInfo, error) {
	info, err := s.manifestInfo(r, image, digest)
	if err != nil {
		return info, err
	}
	tags, _ := s.Tags(r, image)
	for t, d := range tags {
		if d == digest {
			info.Tags = append(info.Tags, t)
		}
	}
	sort.Strings(info.Tags)
	return info, nil
}

func (s *Store) manifestInfo(r Repo, image, digest string) (ManifestInfo, error) {
	if !ValidImage(image) || !ValidDigest(digest) {
		return ManifestInfo{}, ErrManifestUnknown
	}
	rev := s.revisionPath(r, image, digest)
	mt, err := os.ReadFile(rev)
	if err != nil {
		return ManifestInfo{}, ErrManifestUnknown
	}
	fi, _ := os.Stat(rev)
	b, err := s.ReadBlob(digest)
	if err != nil {
		return ManifestInfo{}, ErrManifestUnknown
	}
	var m ManifestDoc
	_ = json.Unmarshal(b, &m)
	info := ManifestInfo{Digest: digest, MediaType: string(mt), Size: int64(len(b)), ImageSize: int64(len(b)),
		Uploaded: fi.ModTime(), Doc: &m}
	if m.Config != nil {
		info.ImageSize += m.Config.Size
	}
	for _, l := range m.Layers {
		info.ImageSize += l.Size
	}
	for _, x := range m.Manifests {
		info.ImageSize += x.Size
	}
	return info, nil
}

// WriteState saves a small file of the caller's beside the content, such
// as the repositories the registry serves.
func (s *Store) WriteState(name string, b []byte) error {
	tmp := filepath.Join(s.root, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.root, name))
}

// ReadState reads a file WriteState saved.
func (s *Store) ReadState(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.root, name))
}
