//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// registryTransport dials the Run port whatever the reference's host, as
// registryOptions does for crane.
func registryTransport(h *Harness) http.RoundTripper {
	addr := h.Endpoint(EnvRun)
	return &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
}

// dockerRepository creates a DOCKER repository in project with client ar
// and returns its name and the registry address of its image app.
func dockerRepository(t *testing.T, h *Harness, ar *artifactregistry.Client, project, id string) (string, string) {
	t.Helper()
	ctx := h.Context()
	parent := "projects/" + project + "/locations/us-central1"
	repoName := parent + "/repositories/" + id
	op, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id,
		Repository: &arpb.Repository{Format: arpb.Repository_DOCKER}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateRepository.Wait: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ar.DeleteRepository(context.Background(), &arpb.DeleteRepositoryRequest{Name: repoName}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	return repoName, "us-central1-docker.pkg.localhost:" + port + "/" + project + "/" + id + "/app"
}

// registryToken is a token from the registry's token endpoint for a
// client with no credentials, which a raw request then bears, as a docker
// client does after the challenge.
func registryToken(t *testing.T, h *Harness, host string) string {
	t.Helper()
	resp, err := (&http.Client{Transport: registryTransport(h)}).Get("http://" + host + "/v2/token?service=" + host)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.Token == "" {
		t.Fatalf("the token endpoint = %d, %v", resp.StatusCode, err)
	}
	return tok.Token
}

// bearerTransport sends a bearer token.
type bearerTransport struct {
	rt  http.RoundTripper
	tok string
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return b.rt.RoundTrip(r)
}

// sbomFor is an SPDX SBOM as an OCI artifact referring to subject, the
// manifest `oras attach --artifact-type application/spdx+json IMAGE
// sbom.json:application/spdx+json` pushes: an empty config of the
// artifact type, one layer, and the subject.
func sbomFor(t *testing.T, subject v1.Image) v1.Image {
	t.Helper()
	d, _ := subject.Digest()
	sz, _ := subject.Size()
	m, _ := subject.Manifest()
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, "application/spdx+json")
	img, err := mutate.Append(img, mutate.Addendum{Layer: static.NewLayer([]byte(`{"spdxVersion":"SPDX-2.3","name":"app"}`), "application/spdx+json")})
	if err != nil {
		t.Fatal(err)
	}
	return mutate.Subject(img, v1.Descriptor{MediaType: m.MediaType, Digest: d, Size: sz}).(v1.Image)
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/ListAttachments, google.devtools.artifactregistry.v1.ArtifactRegistry/GetAttachment, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteAttachment
//
// TestArtifactRegistryReferrers (#2111): go-containerregistry pushes an
// image and an SPDX SBOM referring to it (the subject field, as `oras
// attach` sends), and a signature; the OCI 1.1 referrers API lists both,
// and with ?artifactType= only the SBOM, saying so in OCI-Filters-Applied;
// the PUT answers OCI-Subject. The official Artifact Registry client sees
// each referrer as an attachment of the image's version, as Google
// documents for Docker attachments: its type the artifact type, its
// target the image's version, its ociVersionName the referrer's own
// version, listed by type and by target, read by name; DeleteAttachment
// removes the referrer, and deleting the image's version removes the
// rest. A schema 1 manifest, which Artifact Registry lists among its
// supported formats, is pushed and pulled back with its digest.
func TestArtifactRegistryReferrers(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ar.Close() })
	repoName, local := dockerRepository(t, h, ar, h.Project(), "referrers-repo")
	rt := registryTransport(h)
	img, err := random.Image(512, 1)
	if err != nil {
		t.Fatal(err)
	}
	tag, _ := name.NewTag(local+":v1", name.Insecure)
	if err := remote.Write(tag, img, remote.WithTransport(rt)); err != nil {
		t.Fatalf("push the image: %v", err)
	}
	d, _ := img.Digest()
	sbom := sbomFor(t, img)
	sz, _ := img.Size()
	sig := mutate.Subject(mutate.ConfigMediaType(mutate.MediaType(empty.Image, types.OCIManifestSchema1),
		"application/vnd.dev.cosign.artifact.sig.v1+json"), v1.Descriptor{MediaType: types.DockerManifestSchema2, Digest: d, Size: sz}).(v1.Image)
	sd, _ := sbom.Digest()
	gd, _ := sig.Digest()
	for _, a := range []v1.Image{sbom, sig} {
		ad, _ := a.Digest()
		ref, _ := name.NewDigest(local+"@"+ad.String(), name.Insecure)
		if err := remote.Write(ref, a, remote.WithTransport(rt)); err != nil {
			t.Fatalf("push a referrer: %v", err)
		}
	}
	subject, _ := name.NewDigest(local+"@"+d.String(), name.Insecure)
	idx, err := remote.Referrers(subject, remote.WithTransport(rt))
	if err != nil {
		t.Fatalf("referrers: %v", err)
	}
	im, _ := idx.IndexManifest()
	if len(im.Manifests) != 2 {
		t.Fatalf("referrers = %+v; want the SBOM and the signature", im.Manifests)
	}
	idx, err = remote.Referrers(subject, remote.WithTransport(rt), remote.WithFilter("artifactType", "application/spdx+json"))
	if err != nil {
		t.Fatal(err)
	}
	if im, _ = idx.IndexManifest(); len(im.Manifests) != 1 || im.Manifests[0].Digest != sd || im.Manifests[0].ArtifactType != "application/spdx+json" {
		t.Fatalf("referrers of type application/spdx+json = %+v; want the SBOM %s", im.Manifests, sd)
	}
	hc := &http.Client{Transport: bearerTransport{rt, registryToken(t, h, strings.SplitN(local, "/", 2)[0])}}
	resp, err := hc.Get("http://" + strings.Replace(local, "/", "/v2/", 1) + "/referrers/" + d.String() + "?artifactType=application/spdx%2Bjson")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("OCI-Filters-Applied") != "artifactType" {
		t.Errorf("GET referrers?artifactType = %d, OCI-Filters-Applied %q", resp.StatusCode, resp.Header.Get("OCI-Filters-Applied"))
	}
	raw, _ := sbom.RawManifest()
	req, _ := http.NewRequest(http.MethodPut, "http://"+strings.Replace(local, "/", "/v2/", 1)+"/manifests/"+sd.String(), strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", string(types.OCIManifestSchema1))
	if resp, err = hc.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || resp.Header.Get("OCI-Subject") != d.String() {
		t.Errorf("PUT a referrer = %d, OCI-Subject %q; want 201, %s", resp.StatusCode, resp.Header.Get("OCI-Subject"), d)
	}

	// The attachments.
	pkg := repoName + "/packages/app"
	target := pkg + "/versions/" + d.String()
	all := collectAll(t, ar.ListAttachments(ctx, &arpb.ListAttachmentsRequest{Parent: repoName}).Next)
	if len(all) != 2 {
		t.Fatalf("ListAttachments = %v; want 2", all)
	}
	sboms := collectAll(t, ar.ListAttachments(ctx, &arpb.ListAttachmentsRequest{Parent: repoName, Filter: `type="application/spdx+json"`}).Next)
	if len(sboms) != 1 || sboms[0].GetTarget() != target || sboms[0].GetOciVersionName() != pkg+"/versions/"+sd.String() ||
		len(sboms[0].GetFiles()) != 1 || !strings.HasPrefix(sboms[0].GetName(), repoName+"/attachments/") {
		t.Fatalf("ListAttachments of the SBOM's type = %v", sboms)
	}
	if byTarget := collectAll(t, ar.ListAttachments(ctx, &arpb.ListAttachmentsRequest{Parent: repoName,
		Filter: `target="` + target + `"`}).Next); len(byTarget) != 2 {
		t.Errorf("ListAttachments by target = %v; want 2", byTarget)
	}
	got, err := ar.GetAttachment(ctx, &arpb.GetAttachmentRequest{Name: sboms[0].GetName()})
	if err != nil || got.GetType() != "application/spdx+json" || got.GetTarget() != target {
		t.Fatalf("GetAttachment = %v, %v", got, err)
	}
	// Deleting the signature's attachment deletes its version.
	var sigAt string
	for _, a := range all {
		if a.GetOciVersionName() == pkg+"/versions/"+gd.String() {
			sigAt = a.GetName()
		}
	}
	dop, err := ar.DeleteAttachment(ctx, &arpb.DeleteAttachmentRequest{Name: sigAt})
	if err != nil {
		t.Fatalf("DeleteAttachment: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteAttachment.Wait: %v", err)
	}
	if _, err := ar.GetVersion(ctx, &arpb.GetVersionRequest{Name: pkg + "/versions/" + gd.String()}); status.Code(err) != codes.NotFound {
		t.Errorf("the signature's version after DeleteAttachment = %v; want NOT_FOUND", err)
	}
	// Deleting the image's version deletes its attachments.
	vop, err := ar.DeleteVersion(ctx, &arpb.DeleteVersionRequest{Name: target, Force: true})
	if err != nil {
		t.Fatalf("DeleteVersion: %v", err)
	}
	if err := vop.Wait(ctx); err != nil {
		t.Fatalf("DeleteVersion.Wait: %v", err)
	}
	if _, err := ar.GetAttachment(ctx, &arpb.GetAttachmentRequest{Name: sboms[0].GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("the SBOM's attachment after its target is deleted = %v; want NOT_FOUND", err)
	}

	// A schema 1 manifest.
	layer := static.NewLayer([]byte("schema1 layer"), types.DockerLayer)
	ld, _ := layer.Digest()
	lref, _ := name.NewRepository(strings.TrimSuffix(local, "/app")+"/old", name.Insecure)
	if err := remote.WriteLayer(lref, layer, remote.WithTransport(rt)); err != nil {
		t.Fatalf("push a layer: %v", err)
	}
	s1 := `{"schemaVersion":1,"name":"old","tag":"v1","architecture":"amd64","fsLayers":[{"blobSum":"` + ld.String() +
		`"}],"history":[{"v1Compatibility":"{\"id\":\"1\"}"}]}`
	req, _ = http.NewRequest(http.MethodPut, "http://"+strings.Replace(lref.String(), "/", "/v2/", 1)+"/manifests/v1", strings.NewReader(s1))
	req.Header.Set("Content-Type", "application/vnd.docker.distribution.manifest.v1+json")
	if resp, err = hc.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("PUT a schema 1 manifest = %d", resp.StatusCode)
	}
	desc, err := remote.Get(lref.Tag("v1"), remote.WithTransport(rt))
	if err != nil || desc.MediaType != types.DockerManifestSchema1 || string(desc.Manifest) != s1 {
		t.Fatalf("pull the schema 1 manifest = %+v, %v", desc, err)
	}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/SetIamPolicy
//
// TestIAMEnforcementArtifactRegistryDocker (#2111): with
// --iam-enforcement=on the Docker registry asks for a bearer token
// (WWW-Authenticate: Bearer realm=…/v2/token), and `docker login -u
// oauth2accesstoken` with an access token the instance minted succeeds
// where a token it did not mint fails. A service account with no role is
// refused a pull with 403 DENIED naming
// artifactregistry.repositories.downloadArtifacts; Artifact Registry
// Reader on the repository's own policy lets it pull and still not push
// (uploadArtifacts); Writer on the project lets it push. A _json_key
// login with a key of the account's is the account too.
func TestIAMEnforcementArtifactRegistryDocker(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	h.Endpoint(EnvMetadata)
	h.Endpoint(EnvResourceManager)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iear-")
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ar.Close() })
	repoName, local := dockerRepository(t, h, ar, project, "enforced")
	rt := registryTransport(h)

	// The challenge.
	resp, err := (&http.Client{Transport: rt}).Get("http://" + strings.SplitN(local, "/", 2)[0] + "/v2/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ch := resp.Header.Get("WWW-Authenticate"); resp.StatusCode != 401 || !strings.HasPrefix(ch, `Bearer realm="http://`) ||
		!strings.Contains(ch, `/v2/token",service="`) {
		t.Fatalf("GET /v2/ = %d, WWW-Authenticate %q; want 401 and the Bearer challenge", resp.StatusCode, ch)
	}

	// The instance's own account pushes (no credentials: the owner).
	img, _ := random.Image(256, 1)
	if err := crane.Push(img, local+":v1", append(registryOptions(h), crane.WithAuth(authn.Anonymous))...); err != nil {
		t.Fatalf("push as the instance's account: %v", err)
	}
	p := newIAMPrincipal(t, h, project, "iear")
	as := func(tok string) []crane.Option {
		return append(registryOptions(h), crane.WithAuth(&authn.Basic{Username: "oauth2accesstoken", Password: tok}))
	}
	wantStatus := func(what string, err error, code int, perm string) {
		t.Helper()
		var te *transport.Error
		if !errors.As(err, &te) || te.StatusCode != code || (perm != "" && !strings.Contains(err.Error(), perm)) {
			t.Fatalf("%s = %v; want %d %s", what, err, code, perm)
		}
	}
	_, err = crane.Digest(local+":v1", as(p.token)...)
	wantStatus("pull without a role", err, 403, `Permission "artifactregistry.repositories.downloadArtifacts" denied on resource "`+repoName+`"`)
	_, err = crane.Digest(local+":v1", as("cbl_not-minted-here")...)
	wantStatus("pull with a token the instance did not mint", err, 401, "")

	// Reader on the repository's own policy.
	undo := bindOn(t, ar.GetIamPolicy, ar.SetIamPolicy, repoName,
		&iampb.Binding{Role: "roles/artifactregistry.reader", Members: []string{"serviceAccount:" + p.email}})
	waitGranted(t, "pull as a reader", func() error { _, err := crane.Digest(local+":v1", as(p.token)...); return err })
	other, _ := random.Image(128, 1)
	err = crane.Push(other, local+":v2", as(p.token)...)
	wantStatus("push as a reader", err, 403, "artifactregistry.repositories.uploadArtifacts")
	undo()

	// Writer on the project.
	p.grant("roles/artifactregistry.writer")
	t.Cleanup(func() { p.revoke("roles/artifactregistry.writer") })
	waitGranted(t, "push as a writer", func() error { return crane.Push(other, local+":v2", as(p.token)...) })

	// _json_key: a key of the account's.
	key, err := iamAdmin(t, h).CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: "projects/-/serviceAccounts/" + p.email})
	if err != nil {
		t.Fatalf("CreateServiceAccountKey: %v", err)
	}
	keyOpts := append(registryOptions(h), crane.WithAuth(&authn.Basic{Username: "_json_key", Password: string(key.GetPrivateKeyData())}))
	if _, err := crane.Digest(local+":v2", keyOpts...); err != nil {
		t.Errorf("pull with _json_key: %v", err)
	}

	// docker login, when a docker CLI and daemon are here.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Logf("docker login not run: %v", err)
		return
	}
	if err := exec.CommandContext(ctx, "docker", "version").Run(); err != nil {
		t.Logf("docker login not run: no daemon: %v", err)
		return
	}
	cfg := t.TempDir()
	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	login := func(tok string) (string, error) {
		cmd := exec.CommandContext(ctx, "docker", "login", "-u", "oauth2accesstoken", "--password-stdin", "127.0.0.1:"+port)
		cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+cfg)
		cmd.Stdin = strings.NewReader(tok)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := login(p.token)
	if strings.Contains(out, "connection refused") || strings.Contains(out, "deadline exceeded") {
		// Docker Desktop's daemon runs in a VM, whose loopback is not
		// this machine's: it cannot reach the instance.
		t.Logf("docker login not run: the daemon cannot reach the registry: %s", out)
		return
	}
	if err != nil || !strings.Contains(out, "Login Succeeded") {
		t.Errorf("docker login with a minted token = %v: %s", err, out)
	}
	if out, err := login("cbl_not-minted-here"); err == nil {
		t.Errorf("docker login with a token the instance did not mint succeeded: %s", out)
	}
}
