package registry

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// sbomArtifact is an OCI artifact of artifactType at referring to subject,
// as `oras attach --artifact-type at` pushes one.
func sbomArtifact(t *testing.T, at string, subject v1.Descriptor, body string) v1.Image {
	t.Helper()
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.MediaType(at))
	img, err := mutate.Append(img, mutate.Addendum{Layer: static.NewLayer([]byte(body), "application/spdx+json")})
	if err != nil {
		t.Fatal(err)
	}
	return mutate.Subject(img, subject).(v1.Image)
}

func TestReferrers(t *testing.T) {
	st, host := newTestRegistry(t)
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref := host + "/p/r/app:v1"
	if err := crane.Push(img, ref); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	m, _ := img.Manifest()
	sz, _ := img.Size()
	subject := v1.Descriptor{MediaType: m.MediaType, Digest: d, Size: sz}
	sbom := sbomArtifact(t, "application/spdx+json", subject, `{"spdxVersion":"SPDX-2.3"}`)
	sig := sbomArtifact(t, "application/vnd.dev.cosign.artifact.sig.v1+json", subject, "sig")
	for _, a := range []v1.Image{sbom, sig} {
		ad, _ := a.Digest()
		r, _ := name.NewDigest(host + "/p/r/app@" + ad.String())
		if err := remote.Write(r, a); err != nil {
			t.Fatalf("push the referrer: %v", err)
		}
	}
	dref, _ := name.NewDigest(host + "/p/r/app@" + d.String())
	idx, err := remote.Referrers(dref)
	if err != nil {
		t.Fatal(err)
	}
	im, _ := idx.IndexManifest()
	if len(im.Manifests) != 2 {
		t.Fatalf("referrers = %+v; want 2", im.Manifests)
	}
	idx, err = remote.Referrers(dref, remote.WithFilter("artifactType", "application/spdx+json"))
	if err != nil {
		t.Fatal(err)
	}
	im, _ = idx.IndexManifest()
	sd, _ := sbom.Digest()
	if len(im.Manifests) != 1 || im.Manifests[0].Digest != sd || im.Manifests[0].ArtifactType != "application/spdx+json" {
		t.Fatalf("filtered referrers = %+v; want the SBOM %s", im.Manifests, sd)
	}
	// The raw API: the header, an unknown digest's empty index.
	resp, err := http.Get("http://" + host + "/v2/p/r/app/referrers/" + d.String() + "?artifactType=application/spdx%2Bjson")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("OCI-Filters-Applied") != "artifactType" || resp.Header.Get("Content-Type") != MediaTypeOCIIndex {
		t.Errorf("headers = %v", resp.Header)
	}
	resp, err = http.Get("http://" + host + "/v2/p/r/app/referrers/sha256:" + strings.Repeat("0", 64))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"manifests":[]`) {
		t.Errorf("unknown digest = %d %s; want an empty index", resp.StatusCode, b)
	}
	// PUT answers OCI-Subject.
	raw, _ := sbom.RawManifest()
	req, _ := http.NewRequest(http.MethodPut, "http://"+host+"/v2/p/r/app/manifests/"+sd.String(), bytes.NewReader(raw))
	req.Header.Set("Content-Type", string(types.OCIManifestSchema1))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || resp.Header.Get("OCI-Subject") != d.String() {
		t.Errorf("PUT = %d, OCI-Subject %q; want 201 %s", resp.StatusCode, resp.Header.Get("OCI-Subject"), d)
	}
	// Deleting the subject deletes its referrers.
	if err := st.DeleteTag(Repo{"us-central1", "p", "r"}, "app", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteManifest(Repo{"us-central1", "p", "r"}, "app", d.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Manifest(Repo{"us-central1", "p", "r"}, "app", sd.String()); err == nil {
		t.Error("the SBOM outlived its subject")
	}
}

func TestSchema1Manifests(t *testing.T) {
	st, host := newTestRegistry(t)
	repo := Repo{"us-central1", "p", "r"}
	layer, err := st.PutRepoBlob(repo, []byte("layer"))
	if err != nil {
		t.Fatal(err)
	}
	unsigned := `{"schemaVersion":1,"name":"p/r/old","tag":"v1","architecture":"amd64","fsLayers":[{"blobSum":"` + layer +
		`"}],"history":[{"v1Compatibility":"{\"id\":\"a\"}"}]}`
	put := func(ct, ref, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPut, "http://"+host+"/v2/p/r/old/manifests/"+ref, strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := put(MediaTypeDockerSchema1, "v1", unsigned); resp.StatusCode != 201 || resp.Header.Get("Docker-Content-Digest") != Digest([]byte(unsigned)) {
		t.Fatalf("PUT schema 1 = %d %v", resp.StatusCode, resp.Header)
	}
	// A signed one: its digest is its payload's.
	payload := strings.TrimSuffix(unsigned, "}")
	tail := "}"
	protected := base64.RawURLEncoding.EncodeToString([]byte(`{"formatLength":` + itoa(len(payload)) + `,"formatTail":"` +
		base64.RawURLEncoding.EncodeToString([]byte(tail)) + `","time":"2026-11-21T00:00:00Z"}`))
	signed := payload + `,"signatures":[{"header":{"alg":"ES256"},"signature":"c2ln","protected":"` + protected + `"}]}`
	want := Digest([]byte(unsigned))
	if resp := put(MediaTypeDockerSchema1Signed, "v2", signed); resp.StatusCode != 201 || resp.Header.Get("Docker-Content-Digest") != want {
		t.Fatalf("PUT signed schema 1 = %d %v; want digest %s", resp.StatusCode, resp.Header, want)
	}
	b, d, mt, err := st.GetManifest(repo, "old", "v2")
	if err != nil || d != want || mt != MediaTypeDockerSchema1Signed || string(b) != signed {
		t.Fatalf("GetManifest = %q %s %s %v", b, d, mt, err)
	}
	// A layer not stored is refused.
	bad := strings.Replace(unsigned, layer, "sha256:"+strings.Repeat("1", 64), 1)
	if resp := put(MediaTypeDockerSchema1, "v3", bad); resp.StatusCode != 400 {
		t.Errorf("PUT naming a missing layer = %d; want 400", resp.StatusCode)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestTokenAuth(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const good = "minted-token"
	required, public := true, false
	h := &Handler{Store: st, Lookup: func(loc, project, repo string) (Repo, bool) {
		return Repo{"us-central1", project, repo}, project == "p"
	}, Auth: &Auth{
		Login: func(u, p string) (string, error) {
			if IsAccessTokenUser(u) && (p == good || p == "reader") {
				return p, nil
			}
			return "", ErrUnauthenticated
		},
		Authorize: func(bearer, perm, res string) error {
			switch {
			case bearer == AnonymousToken && !required:
				return nil // enforcement off: nothing is checked
			case bearer == AnonymousToken && perm == PermissionDownload && public, bearer == AnonymousToken && perm == "":
				return nil
			case bearer == AnonymousToken:
				return &DeniedError{Permission: perm, Resource: res, Anonymous: true}
			case bearer == "" || bearer == good:
				return nil
			case bearer == "reader" && (perm == PermissionDownload || perm == ""):
				return nil
			case bearer == "reader":
				return &DeniedError{Permission: perm, Resource: res}
			}
			return ErrUnauthenticated
		},
		Required: func() bool { return required },
	}}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	resp, err := http.Get(srv.URL + "/v2/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ch := resp.Header.Get("WWW-Authenticate"); resp.StatusCode != 401 || ch != `Bearer realm="`+srv.URL+`/v2/token",service="`+host+`"` {
		t.Fatalf("/v2/ = %d %q", resp.StatusCode, ch)
	}
	img, _ := random.Image(64, 1)
	ref := host + "/p/r/app:v1"
	as := func(tok string) crane.Option {
		return crane.WithAuth(&authn.Basic{Username: "oauth2accesstoken", Password: tok})
	}
	if err := crane.Push(img, ref, as(good)); err != nil {
		t.Fatalf("push with a good token: %v", err)
	}
	if _, err := crane.Digest(ref, as("reader")); err != nil {
		t.Fatalf("pull as a reader: %v", err)
	}
	err = crane.Push(img, host+"/p/r/app:v2", as("reader"))
	if err == nil || !strings.Contains(err.Error(), "DENIED") || !strings.Contains(err.Error(), PermissionUpload) {
		t.Fatalf("push as a reader = %v; want DENIED %s", err, PermissionUpload)
	}
	if _, err := crane.Digest(ref, as("forged")); err == nil {
		t.Fatal("pull with a token not minted here succeeded")
	}
	// _json_key and _json_key_base64 name a key.
	if k, ok := KeyCredential("_json_key_base64", base64.StdEncoding.EncodeToString([]byte(`{"a":1}`))); !ok || string(k) != `{"a":1}` {
		t.Errorf("KeyCredential = %q %v", k, ok)
	}
	// Anonymous: a token is issued, and with credentials not required a
	// request with none is served.
	resp, err = http.Get(srv.URL + "/v2/token?service=" + host + "&scope=repository:p/r/app:pull")
	if err != nil {
		t.Fatal(err)
	}
	var tok struct{ Token string }
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if tok.Token != AnonymousToken {
		t.Errorf("anonymous token = %q", tok.Token)
	}
	resp, _ = http.Get(srv.URL + "/v2/p/r/app/tags/list")
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `scope="repository:p/r/app:pull"`) {
		t.Errorf("tags without credentials, required = %d %v", resp.StatusCode, resp.Header)
	}
	// With credentials required, no credentials are allUsers: a pull of a
	// repository that is not public is refused as Google refuses it, one
	// that is public is served, and a push never is.
	anon := crane.WithAuth(authn.Anonymous)
	if _, err := crane.Digest(ref, anon); err == nil || !strings.Contains(err.Error(), "DENIED") ||
		!strings.Contains(err.Error(), "Unauthenticated request. Unauthenticated requests do not have permission") {
		t.Errorf("anonymous pull of a private repository = %v; want DENIED, unauthenticated", err)
	}
	public = true
	if _, err := crane.Digest(ref, anon); err != nil {
		t.Errorf("anonymous pull of a public repository: %v", err)
	}
	if err := crane.Push(img, host+"/p/r/app:anon", anon); err == nil || !strings.Contains(err.Error(), PermissionUpload) {
		t.Errorf("anonymous push = %v; want DENIED %s", err, PermissionUpload)
	}
	public = false
	// A cluster node's containerd pulls through the mirror (ns names
	// {location}-docker.pkg.dev) as the instance's account, with or
	// without the anonymous token.
	mirror := srv.URL + "/v2/p/r/app/manifests/v1?ns=us-central1-docker.pkg.dev"
	for _, auth := range []string{"", "Bearer " + AnonymousToken} {
		req, _ := http.NewRequest(http.MethodHead, mirror, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("the cluster's pull through the mirror (%q) = %d; want 200", auth, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v2/p/r/app/manifests/v9?ns=us-central1-docker.pkg.dev", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+AnonymousToken)
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else if resp.Body.Close(); resp.StatusCode != 403 {
		t.Errorf("an anonymous push naming the mirror = %d; want 403", resp.StatusCode)
	}
	required = false
	resp, _ = http.Get(srv.URL + "/v2/p/r/app/tags/list")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("tags without credentials, not required = %d", resp.StatusCode)
	}
}
