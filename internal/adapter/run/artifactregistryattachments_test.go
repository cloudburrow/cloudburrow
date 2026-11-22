package run

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// uploadMultipart sends UploadFile's media upload as apitools' simple
// multipart one: the JSON metadata, then the content.
func uploadMultipart(t *testing.T, base, repoName, meta string, content []byte) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	p, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json"}})
	_, _ = io.WriteString(p, meta)
	p, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
	_, _ = p.Write(content)
	_ = mw.Close()
	resp, err := http.Post(base+"/upload/v1/"+repoName+"/files:upload?uploadType=multipart&alt=json",
		"multipart/related; boundary="+mw.Boundary(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// uploadedFileName is the File an UploadFile response's done operation
// carries.
func uploadedFileName(t *testing.T, out map[string]any) string {
	t.Helper()
	op, _ := out["operation"].(map[string]any)
	res, _ := op["response"].(map[string]any)
	if op["done"] != true || res["@type"] != "type.googleapis.com/google.devtools.artifactregistry.v1.File" {
		t.Fatalf("UploadFile response = %v; want a done operation whose response is the File", out)
	}
	return res["name"].(string)
}

// UploadFile (#2111), as `gcloud artifacts attachments create` sends each
// file: multipart or resumable, named by its digest unless fileId names
// it, refused a second time; in a DOCKER repository the file is a blob
// named by its digest. CreateAttachment in a DOCKER repository makes the
// OCI artifact Google documents a Docker attachment is: the referrers API
// lists it, ListAttachments lists it once under its own ID with its
// ociVersionName, DeleteAttachment deletes the artifact, and deleting the
// target deletes the attachment.
func TestUploadFileAndDockerAttachments(t *testing.T) {
	st, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	UseRegistry(st)
	t.Cleanup(func() { UseRegistry(nil) })
	s := NewServer(&Knative{}, "t", time.Minute)
	a := s.ArtifactRegistry()
	ctx := context.Background()
	parent := "projects/attach-unit/locations/us-central1"
	for id, f := range map[string]arpb.Repository_Format{"att": arpb.Repository_DOCKER, "gen": arpb.Repository_GENERIC} {
		if _, err := a.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id,
			Repository: &arpb.Repository{Format: f}}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { RemoveRepository(parent + "/repositories/" + id) })
	}
	docker, generic := parent+"/repositories/att", parent+"/repositories/gen"
	srv := httptest.NewServer(RegistryHandlerOr(http.NotFoundHandler()))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	img, _ := random.Image(128, 1)
	if err := crane.Push(img, host+"/attach-unit/att/app:v1"); err != nil {
		t.Fatal(err)
	}
	digest, _ := img.Digest()
	target := docker + "/packages/app/versions/" + digest.String()

	// UploadFile into the DOCKER repository: a blob, named by its digest.
	sbom := []byte(`{"spdxVersion":"SPDX-2.3","name":"app"}`)
	code, out := uploadMultipart(t, srv.URL, docker, `{}`, sbom)
	if code != http.StatusOK {
		t.Fatalf("UploadFile = %d %v", code, out)
	}
	file := uploadedFileName(t, out)
	if file != docker+"/files/"+registry.Digest(sbom) {
		t.Fatalf("uploaded file = %s; want it named by its digest", file)
	}
	if f, err := a.GetFile(ctx, &arpb.GetFileRequest{Name: file}); err != nil || f.GetSizeBytes() != int64(len(sbom)) {
		t.Fatalf("GetFile(%s) = %v, %v", file, f, err)
	}
	if code, _ := uploadMultipart(t, srv.URL, docker, `{"fileId":"sbom.json"}`, []byte("other")); code != http.StatusBadRequest {
		t.Errorf("UploadFile with a fileId that is not the digest, in a DOCKER repository = %d; want 400", code)
	}

	// UploadFile into the GENERIC repository, resumable, with a fileId.
	resp, err := http.Post(srv.URL+"/resumable/upload/v1/"+generic+"/files:upload?uploadType=resumable", "application/json",
		strings.NewReader(`{"fileId":"notes.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusOK || loc == "" {
		t.Fatalf("resumable UploadFile start = %d, Location %q", resp.StatusCode, loc)
	}
	req, _ := http.NewRequest(http.MethodPut, loc, strings.NewReader("hello"))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if name := uploadedFileName(t, out); name != generic+"/files/notes.txt" {
		t.Errorf("resumable UploadFile = %s; want %s/files/notes.txt", name, generic)
	}
	if code, _ := uploadMultipart(t, srv.URL, generic, `{"fileId":"notes.txt"}`, []byte("again")); code != http.StatusConflict {
		t.Errorf("a second UploadFile of notes.txt = %d; want 409", code)
	}
	code, out = uploadMultipart(t, srv.URL, generic, ``, []byte("unnamed"))
	if name := uploadedFileName(t, out); code != http.StatusOK || name != generic+"/files/"+esc(registry.Digest([]byte("unnamed"))) {
		t.Errorf("UploadFile with no fileId = %d %s; want the file named by its sha256 digest", code, name)
	}

	// CreateAttachment in the DOCKER repository.
	create := func(id, typ string) (*arpb.Attachment, error) {
		_, err := a.CreateAttachment(ctx, &arpb.CreateAttachmentRequest{Parent: docker, AttachmentId: id,
			Attachment: &arpb.Attachment{Target: target, Type: typ, AttachmentNamespace: "example.com", Files: []string{file}}})
		if err != nil {
			return nil, err
		}
		return a.GetAttachment(ctx, &arpb.GetAttachmentRequest{Name: docker + "/attachments/" + id})
	}
	if _, err := create("bad", "not a media type"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateAttachment with a type that is no artifactType = %v; want INVALID_ARGUMENT", err)
	}
	at, err := create("sbom", "application/spdx+json")
	if err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	if at.GetOciVersionName() == "" || !strings.HasPrefix(at.GetOciVersionName(), docker+"/packages/app/versions/sha256:") ||
		at.GetAttachmentNamespace() != "example.com" {
		t.Fatalf("attachment = %v; want its OCI version in app, and its namespace", at)
	}
	if _, err := create("sbom", "application/spdx+json"); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second CreateAttachment of sbom = %v; want ALREADY_EXISTS", err)
	}
	refs, err := st.Referrers(registry.Repo{Location: "us-central1", Project: "attach-unit", Repository: "att"}, "app", digest.String(), "")
	ociDigest := at.GetOciVersionName()[strings.LastIndex(at.GetOciVersionName(), "/")+1:]
	if err != nil || len(refs) != 1 || refs[0].ArtifactType != "application/spdx+json" || refs[0].Digest != ociDigest {
		t.Fatalf("referrers = %+v, %v; want the attachment's artifact", refs, err)
	}
	list, err := a.ListAttachments(ctx, &arpb.ListAttachmentsRequest{Parent: docker})
	if err != nil || len(list.GetAttachments()) != 1 || list.GetAttachments()[0].GetName() != docker+"/attachments/sbom" {
		t.Fatalf("ListAttachments = %v, %v; want sbom alone (its artifact is not listed a second time)", list, err)
	}
	for _, f := range []string{`target="` + target + `"`, `target="us-central1-docker.pkg.dev/attach-unit/att/app:v1"`} {
		if l, err := a.ListAttachments(ctx, &arpb.ListAttachmentsRequest{Parent: docker, Filter: f}); err != nil || len(l.GetAttachments()) != 1 {
			t.Errorf("ListAttachments(%s) = %v, %v; want sbom", f, l, err)
		}
	}
	if _, err := a.DeleteAttachment(ctx, &arpb.DeleteAttachmentRequest{Name: at.GetName()}); err != nil {
		t.Fatalf("DeleteAttachment: %v", err)
	}
	if refs, _ := st.Referrers(registry.Repo{Location: "us-central1", Project: "attach-unit", Repository: "att"}, "app", digest.String(), ""); len(refs) != 0 {
		t.Errorf("referrers after DeleteAttachment = %+v; want none: the artifact goes with it", refs)
	}

	// Deleting the target deletes its attachment.
	if _, err := create("sbom2", "application/spdx+json"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DeleteVersion(ctx, &arpb.DeleteVersionRequest{Name: target, Force: true}); err != nil {
		t.Fatalf("DeleteVersion: %v", err)
	}
	if l, err := a.ListAttachments(ctx, &arpb.ListAttachmentsRequest{Parent: docker}); err != nil || len(l.GetAttachments()) != 0 {
		t.Errorf("ListAttachments after the target's deletion = %v, %v; want none", l, err)
	}
}
