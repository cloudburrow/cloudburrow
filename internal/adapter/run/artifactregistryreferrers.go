package run

import (
	"strings"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/protobuf/types/known/timestamppb"

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
			if m.Doc == nil || m.Doc.Subject == nil || !registry.ValidDigest(m.Doc.Subject.Digest) {
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
// {image}@{digest}), which gcloud's --target takes, is read as one too.
func dockerTarget(repoName, target string) string {
	if strings.HasPrefix(target, "projects/") {
		return target
	}
	i := strings.LastIndex(target, "@")
	if i < 0 {
		return target
	}
	parts := strings.SplitN(target[:i], "/", 4)
	if len(parts) != 4 {
		return target
	}
	return repoName + "/packages/" + registry.EscapePackage(parts[3]) + "/versions/" + target[i+1:]
}
