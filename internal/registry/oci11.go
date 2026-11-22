package registry

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The OCI distribution specification 1.1's referrers (#2111): a manifest
// with a subject refers to another manifest of the same image, as an SBOM,
// a signature or an attestation attaches to an image (`oras attach`), and
// GET /v2/<name>/referrers/<digest> lists them as an image index. Artifact
// Registry serves the API ("v1.1-referrers-api is the Referrer API used",
// https://docs.cloud.google.com/artifact-registry/docs/manage-metadata-with-attachments,
// read 2026-11-21) and lists each referrer as an attachment of its target
// version. And Docker Image Manifest V2, Schema 1, which Artifact Registry
// lists among its supported formats
// (https://docs.cloud.google.com/artifact-registry/docs/supported-formats,
// read 2026-11-21).

// Referrers are the descriptors of the manifests of image whose subject is
// digest, by digest, as the referrers API lists them: each with its
// artifactType (its own, or an image manifest's config media type when it
// has none) and annotations. A non-empty artifactType keeps only those of
// that type. A digest the image does not hold has none.
func (s *Store) Referrers(r Repo, image, digest, artifactType string) ([]Descriptor, error) {
	if !ValidImage(image) {
		return nil, ErrNameInvalid
	}
	if !ValidDigest(digest) {
		return nil, ErrDigestInvalid
	}
	var out []Descriptor
	for _, x := range s.referrers(r, image, digest) {
		if artifactType == "" || x.ArtifactType == artifactType {
			out = append(out, x)
		}
	}
	return out, nil
}

// referrers is every referrer of digest in image.
func (s *Store) referrers(r Repo, image, digest string) []Descriptor {
	dir := filepath.Join(s.imageDir(r, image), "revisions", "sha256")
	ents, _ := os.ReadDir(dir)
	var out []Descriptor
	for _, e := range ents {
		d := "sha256:" + e.Name()
		b, _, mt, err := s.GetManifest(r, image, d)
		if err != nil {
			continue
		}
		var m ManifestDoc
		if json.Unmarshal(b, &m) != nil || m.Subject == nil || m.Subject.Digest != digest {
			continue
		}
		at := m.ArtifactType
		if at == "" && m.Config != nil && !IsIndex(mt) {
			at = m.Config.MediaType
		}
		out = append(out, Descriptor{MediaType: mt, Digest: d, Size: int64(len(b)), ArtifactType: at, Annotations: m.Annotations})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out
}

// referrerDigests are the digests of digest's referrers in image.
func (s *Store) referrerDigests(r Repo, image, digest string) []string {
	if !ValidImage(image) || !ValidDigest(digest) {
		return nil
	}
	var out []string
	for _, x := range s.referrers(r, image, digest) {
		if x.Digest != digest {
			out = append(out, x.Digest)
		}
	}
	return out
}

// SubjectOf is the digest of the manifest b refers to, "" for none.
func SubjectOf(b []byte) string {
	var m struct {
		Subject *Descriptor `json:"subject"`
	}
	if json.Unmarshal(b, &m) != nil || m.Subject == nil {
		return ""
	}
	return m.Subject.Digest
}

// ManifestDigest is a manifest's digest: its content's, but a signed
// schema 1 manifest's is its payload's, the content without its
// signatures, as the Docker registry computes it.
func ManifestDigest(mediaType string, b []byte) string {
	if mediaType == MediaTypeDockerSchema1Signed {
		if p, err := schema1Payload(b); err == nil {
			return Digest(p)
		}
	}
	return Digest(b)
}

// schema1Type is the media type of schema 1 content: signed when it has
// signatures.
func schema1Type(b []byte) string {
	var m struct {
		Signatures []json.RawMessage `json:"signatures"`
	}
	if json.Unmarshal(b, &m) == nil && len(m.Signatures) > 0 {
		return MediaTypeDockerSchema1Signed
	}
	return MediaTypeDockerSchema1
}

// schema1Manifest is the part of a schema 1 manifest the registry checks.
type schema1Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	Name          string            `json:"name"`
	Tag           string            `json:"tag"`
	Architecture  string            `json:"architecture"`
	FSLayers      []FSLayer         `json:"fsLayers"`
	History       []json.RawMessage `json:"history"`
	Signatures    []struct {
		Protected string `json:"protected"`
	} `json:"signatures"`
}

// checkSchema1 checks a schema 1 manifest: schemaVersion 1, its layers'
// digests, a history entry per layer, and for a signed one, a signature
// whose protected header gives the payload.
func checkSchema1(mt string, b []byte) error {
	var m schema1Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("manifest is not JSON: %v", err)
	}
	if m.SchemaVersion != 1 {
		return errors.New("a schema 1 manifest needs schemaVersion 1")
	}
	if len(m.FSLayers) == 0 {
		return errors.New("a schema 1 manifest needs fsLayers")
	}
	if len(m.History) != len(m.FSLayers) {
		return fmt.Errorf("a schema 1 manifest needs one history entry per layer: %d layers, %d entries", len(m.FSLayers), len(m.History))
	}
	for _, l := range m.FSLayers {
		if !ValidDigest(l.BlobSum) {
			return fmt.Errorf("fsLayers blobSum %q is not a digest", l.BlobSum)
		}
	}
	if mt == MediaTypeDockerSchema1Signed {
		if len(m.Signatures) == 0 {
			return errors.New("a signed schema 1 manifest needs signatures")
		}
		if _, err := schema1Payload(b); err != nil {
			return err
		}
	}
	return nil
}

// schema1Payload is a signed schema 1 manifest's payload: its first
// formatLength bytes and formatTail, which its signature's protected
// header gives (Docker's libtrust JWS).
func schema1Payload(b []byte) ([]byte, error) {
	var m schema1Manifest
	if err := json.Unmarshal(b, &m); err != nil || len(m.Signatures) == 0 {
		return nil, errors.New("a signed schema 1 manifest needs signatures")
	}
	ph, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(m.Signatures[0].Protected, "="))
	if err != nil {
		return nil, errors.New("signature's protected header is not base64url")
	}
	var h struct {
		FormatLength int    `json:"formatLength"`
		FormatTail   string `json:"formatTail"`
	}
	if err := json.Unmarshal(ph, &h); err != nil || h.FormatLength <= 0 || h.FormatLength > len(b) {
		return nil, errors.New("signature's protected header has no valid formatLength")
	}
	tail, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(h.FormatTail, "="))
	if err != nil {
		return nil, errors.New("signature's formatTail is not base64url")
	}
	return append(append([]byte{}, b[:h.FormatLength]...), tail...), nil
}

// revisionRecord is a revision file's content: the media type, and the
// content's digest on a second line when it is not the manifest's.
func revisionRecord(mediaType, digest, content string) string {
	if content == "" || content == digest {
		return mediaType
	}
	return mediaType + "\n" + content
}

// parseRevision reads a revision file: the manifest's media type and the
// digest its content is stored under.
func parseRevision(digest string, rec []byte) (mediaType, content string) {
	mt, c, _ := strings.Cut(string(rec), "\n")
	c = strings.TrimSpace(c)
	if !ValidDigest(c) {
		c = digest
	}
	return strings.TrimSpace(mt), c
}

// AttachmentID is a stable UUID for the referrer digest names, as
// Artifact Registry names an attachment `oras attach` makes with "a random
// UUID".
func AttachmentID(digest string) string {
	sum := sha256.Sum256([]byte(digest))
	u := sum[:16]
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	h := hex.EncodeToString(u)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// referrers serves GET /v2/<name>/referrers/<digest>: an OCI image index
// of the manifests whose subject is digest, filtered by artifactType when
// asked, which OCI-Filters-Applied then says. A virtual repository lists
// its upstreams' referrers.
func (h *Handler) referrers(w http.ResponseWriter, r *http.Request, repo Repo, image, digest string) {
	if r.Method != http.MethodGet {
		h.fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}
	if !ValidDigest(digest) {
		h.fail(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest "+digest)
		return
	}
	at := r.URL.Query().Get("artifactType")
	repos := []Repo{repo}
	if ups, ok := h.virtual(repo); ok {
		repos = ups
	} else if up, ok := h.remote(repo); ok && up.Repository != "" {
		repos = append(repos, up)
	}
	seen := map[string]bool{}
	manifests := []Descriptor{}
	for _, rp := range repos {
		ds, err := h.Store.Referrers(rp, image, digest, at)
		if err != nil {
			h.failErr(w, err)
			return
		}
		for _, d := range ds {
			if !seen[d.Digest] {
				seen[d.Digest] = true
				manifests = append(manifests, d)
			}
		}
	}
	if at != "" {
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}
	w.Header().Set("Content-Type", MediaTypeOCIIndex)
	_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 2, "mediaType": MediaTypeOCIIndex, "manifests": manifests})
}

// mountReadable reports whether a cross-repository mount's source may be
// read by the request's caller; a mount it may not read falls back to an
// upload, as the distribution specification allows.
func (h *Handler) mountReadable(r *http.Request, from string) bool {
	if h.Auth == nil || h.Auth.Authorize == nil || from == "" {
		return true
	}
	src, _, ok := h.resolve(r, from)
	if !ok {
		return true // blobs are shared by digest; the source names nothing
	}
	bearer, _, err := h.credentials(r)
	return err == nil && h.Auth.Authorize(bearer, PermissionDownload, RepoResource(src)) == nil
}

// readableCatalog keeps the catalog's names whose repository the caller
// may read.
func (h *Handler) readableCatalog(r *http.Request, all []string) []string {
	if h.Auth == nil || h.Auth.Authorize == nil {
		return all
	}
	bearer, _, err := h.credentials(r)
	if err != nil {
		return []string{}
	}
	ok := map[string]bool{}
	out := []string{}
	for _, n := range all {
		parts := strings.SplitN(n, "/", 3)
		if len(parts) < 3 {
			continue
		}
		key := parts[0] + "/" + parts[1]
		allowed, seen := ok[key]
		if !seen {
			repo, found := h.Lookup(location(r), parts[0], parts[1])
			allowed = found && h.Auth.Authorize(bearer, PermissionDownload, RepoResource(repo)) == nil
			ok[key] = allowed
		}
		if allowed {
			out = append(out, n)
		}
	}
	return out
}
