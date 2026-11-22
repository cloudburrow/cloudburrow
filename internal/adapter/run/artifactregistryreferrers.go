package run

import (
	"encoding/json"
	"regexp"
	"strings"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// A DOCKER repository's OCI 1.1 referrers as attachments (#2111): an
// artifact pushed with a subject (`oras attach`) is an attachment of the
// version it refers to, as Artifact Registry lists it
// (https://docs.cloud.google.com/artifact-registry/docs/store-artifact-metadata-in-attachments,
// read 2026-11-21: "Artifact Registry generates a random UUID to use as
// the attachment name"; here a UUID made from the referrer's digest, so
// the name is the same on every read). Its type is the referrer's
// artifactType, its target the subject's version, its files the
// referrer's layers, and its ociVersionName the referrer's own version
// ("The name of the OCI version that this attachment created", populated
// for Docker attachments only). Deleting the attachment deletes that
// version; deleting the target deletes its attachments (the store's
// DeleteManifest).

// dockerAttachments are a DOCKER repository's referrers as attachments;
// nil for any other repository.
func dockerAttachments(repoName string) []*arpb.Attachment {
	if isPackageRepo(repoName) {
		return nil
	}
	claimed := claimedOCIVersions()
	st, err := contentStore(repoName)
	if err != nil {
		return nil
	}
	repo := repoOf(repoName)
	var out []*arpb.Attachment
	for _, im := range st.Images(repo) {
		ms, _ := st.Manifests(repo, im)
		pkg := repoName + "/packages/" + registry.EscapePackage(im)
		for _, m := range ms {
			if m.Doc == nil || m.Doc.Subject == nil || !registry.ValidDigest(m.Doc.Subject.Digest) ||
				claimed[pkg+"/versions/"+m.Digest] {
				continue
			}
			at := m.Doc.ArtifactType
			if at == "" && m.Doc.Config != nil {
				at = m.Doc.Config.MediaType
			}
			a := &arpb.Attachment{
				Name:           repoName + "/attachments/" + registry.AttachmentID(m.Digest),
				Target:         pkg + "/versions/" + m.Doc.Subject.Digest,
				Type:           at,
				OciVersionName: pkg + "/versions/" + m.Digest,
				CreateTime:     timestamppb.New(m.Uploaded),
				UpdateTime:     timestamppb.New(m.Uploaded),
			}
			for _, l := range m.Doc.Layers {
				a.Files = append(a.Files, repoName+"/files/"+l.Digest)
			}
			out = append(out, a)
		}
	}
	return out
}

// dockerAttachment is the referrer attachment name names, nil for none.
func dockerAttachment(name string) *arpb.Attachment {
	repoName, _, ok := splitChild(name, "attachments")
	if !ok {
		return nil
	}
	for _, a := range dockerAttachments(repoName) {
		if a.GetName() == name {
			return a
		}
	}
	return nil
}

// deleteDockerAttachment deletes the version a referrer attachment
// created; ok is false when name is no such attachment.
func deleteDockerAttachment(name string) (bool, error) {
	a := dockerAttachment(name)
	if a == nil {
		return false, nil
	}
	_, repoName, image, digest, err := parseVersionName(a.GetOciVersionName())
	if err != nil {
		return true, err
	}
	st, err := contentStore(repoName)
	if err != nil {
		return true, err
	}
	if err := st.DeleteManifest(repoOf(repoName), image, digest); err != nil {
		return true, storeErr(err)
	}
	forgetUnder(a.GetOciVersionName())
	return true, nil
}

// dockerTarget is a filter's target as a version name: Google's filter
// names a version; a Docker image URI ({host}/{project}/{repository}/
// {image}@{digest} or :{tag}), which gcloud's --target takes, is read as
// one too.
func dockerTarget(repoName, target string) string {
	if strings.HasPrefix(target, "projects/") {
		return target
	}
	if i := strings.LastIndex(target, "@"); i >= 0 {
		parts := strings.SplitN(target[:i], "/", 4)
		if len(parts) != 4 {
			return target
		}
		return repoName + "/packages/" + registry.EscapePackage(parts[3]) + "/versions/" + target[i+1:]
	}
	// An image URI with a tag names the version the tag is on.
	i := strings.LastIndex(target, ":")
	if i < 0 || i < strings.LastIndex(target, "/") {
		return target
	}
	parts := strings.SplitN(target[:i], "/", 4)
	st, err := contentStore(repoName)
	if len(parts) != 4 || err != nil {
		return target
	}
	d, _, err := st.Resolve(repoOf(repoName), parts[3], target[i+1:])
	if err != nil {
		return target
	}
	return repoName + "/packages/" + registry.EscapePackage(parts[3]) + "/versions/" + d
}

// claimedOCIVersions are the OCI versions CreateAttachment made, which
// are their stored attachments, not a referrer's UUID-named one too.
func claimedOCIVersions() map[string]bool {
	arConfig.mu.Lock()
	defer arConfig.mu.Unlock()
	out := map[string]bool{}
	for _, a := range arConfig.attachments {
		if v := a.GetOciVersionName(); v != "" {
			out[v] = true
		}
	}
	return out
}

// ociVersionGone reports whether a stored attachment's OCI version was
// deleted (by the Docker registry API, or with the version it refers
// to): the attachment is gone with it.
func ociVersionGone(a *arpb.Attachment) bool {
	v := a.GetOciVersionName()
	if v == "" {
		return false
	}
	_, repoName, image, digest, err := parseVersionName(v)
	if err != nil {
		return true
	}
	st, err := contentStore(repoName)
	if err != nil {
		return true
	}
	_, err = st.Manifest(repoOf(repoName), image, digest)
	return err != nil
}

// ociArtifactType is a media type as the OCI image specification's
// artifactType takes it (RFC 6838: type/subtype, each of restricted
// name characters).
var ociArtifactType = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}$`)

// emptyConfig is the OCI image specification's empty descriptor's
// content, an artifact's config when it has none.
const emptyConfig = "{}"

// pushAttachmentArtifact makes the OCI artifact a DOCKER repository's
// attachment is ("For Docker repositories, attachments must be OCI
// artifacts", https://docs.cloud.google.com/artifact-registry/docs/store-artifact-metadata-in-attachments,
// read 2026-11-22): an image manifest of artifactType the attachment's
// type, an empty config, the attachment's files as its layers and the
// target's manifest as its subject, stored in the target's image, as
// `oras attach` would push it, so the referrers API lists it. It returns
// the artifact's version name, the attachment's ociVersionName.
func pushAttachmentArtifact(repoName string, at *arpb.Attachment) (string, error) {
	if !ociArtifactType.MatchString(at.GetType()) {
		return "", apierror.InvalidArgument("invalid attachment type %q: a Docker attachment's type must be an OCI artifactType, a media type such as application/vnd.example+json", at.GetType())
	}
	pkgName, _, image, digest, err := parseVersionName(at.GetTarget())
	if err != nil {
		return "", err
	}
	st, err := contentStore(repoName)
	if err != nil {
		return "", err
	}
	repo := repoOf(repoName)
	target, err := st.Manifest(repo, image, digest)
	if err != nil {
		return "", apierror.InvalidArgument("attachment target %s does not exist", at.GetTarget())
	}
	cfg, err := st.PutRepoBlob(repo, []byte(emptyConfig))
	if err != nil {
		return "", storeErr(err)
	}
	type desc struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}
	var layers []desc
	for _, f := range at.GetFiles() {
		_, d, _ := splitChild(f, "files")
		size, err := st.RepoBlobSize(repo, d)
		if err != nil {
			return "", apierror.InvalidArgument("attachment file %s does not exist", f)
		}
		layers = append(layers, desc{MediaType: "application/octet-stream", Digest: d, Size: size})
	}
	doc := struct {
		SchemaVersion int               `json:"schemaVersion"`
		MediaType     string            `json:"mediaType"`
		ArtifactType  string            `json:"artifactType"`
		Config        desc              `json:"config"`
		Layers        []desc            `json:"layers"`
		Subject       desc              `json:"subject"`
		Annotations   map[string]string `json:"annotations,omitempty"`
	}{2, registry.MediaTypeOCIManifest, at.GetType(),
		desc{MediaType: "application/vnd.oci.empty.v1+json", Digest: cfg, Size: int64(len(emptyConfig))}, layers,
		desc{MediaType: target.MediaType, Digest: target.Digest, Size: target.Size}, at.GetAnnotations()}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	d, err := st.PutManifest(repo, image, registry.Digest(b), registry.MediaTypeOCIManifest, b)
	if err != nil {
		return "", storeErr(err)
	}
	return pkgName + "/versions/" + d, nil
}

// deleteOCIVersion deletes a stored attachment's OCI version, if it has
// one that is still there.
func deleteOCIVersion(a *arpb.Attachment) error {
	if a.GetOciVersionName() == "" || ociVersionGone(a) {
		return nil
	}
	_, repoName, image, digest, err := parseVersionName(a.GetOciVersionName())
	if err != nil {
		return err
	}
	st, err := contentStore(repoName)
	if err != nil {
		return err
	}
	if err := st.DeleteManifest(repoOf(repoName), image, digest); err != nil {
		return storeErr(err)
	}
	return nil
}
