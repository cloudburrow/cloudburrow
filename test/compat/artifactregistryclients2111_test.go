//go:build compat

package compat

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"google.golang.org/api/iterator"
)

// The real registry clients (#2111): oras, cosign, crane and docker run
// against the instance's Docker registry, each in a container on an
// internal Docker network whose one route is a relay to the Run port, so
// no client can reach anything else (EGRESS rule). They are not pulled or
// downloaded here:
//   - CB_OCI_TOOLS is a directory of the linux binaries, for the docker
//     daemon's architecture, of oras (1.2 or later, Apache-2.0), cosign
//     (Apache-2.0) and crane (go-containerregistry's, Apache-2.0), each
//     from its project's own release page; a missing one's steps skip;
//   - CB_OCI_CLIENT_IMAGE, or a local docker:*dind* image, runs them and
//     the relay, and is the docker daemon docker login and push run in.

// ociClientEnv is the tools directory and the image, skipping the test
// when either is missing.
func ociClientEnv(t *testing.T) (tools, image string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	tools = os.Getenv("CB_OCI_TOOLS")
	if tools == "" {
		t.Skip("CB_OCI_TOOLS is not set (a directory of linux oras, cosign and crane binaries)")
	}
	image = os.Getenv("CB_OCI_CLIENT_IMAGE")
	if image == "" {
		out, err := exec.Command("docker", "images", "--format", "{{.Repository}}:{{.Tag}}").Output()
		if err != nil {
			t.Skipf("docker images: %v", err)
		}
		for _, l := range strings.Fields(string(out)) {
			if strings.HasPrefix(l, "docker:") && strings.Contains(l, "dind") {
				image = l
				break
			}
		}
	}
	if image == "" {
		t.Skip("no local docker:*dind* image and CB_OCI_CLIENT_IMAGE is not set (nothing is pulled)")
	}
	return tools, image
}

// ociRunner runs the tools in containers on network.
type ociRunner struct {
	t                     *testing.T
	ctx                   context.Context
	tools, image, network string
	work                  string
}

// has reports whether the tools directory holds bin.
func (o *ociRunner) has(bin string) bool {
	_, err := os.Stat(filepath.Join(o.tools, bin))
	if err != nil {
		o.t.Logf("%s is not in CB_OCI_TOOLS: its steps are not run", bin)
	}
	return err == nil
}

// run runs bin with args, stdin, and the docker configuration home names
// (each identity its own); it logs and returns the output.
func (o *ociRunner) run(home, stdin, bin string, args ...string) (string, error) {
	o.t.Helper()
	a := []string{"run", "--rm", "-i", "--network", o.network, "-v", o.tools + ":/tools:ro", "-v", o.work + ":/work", "-w", "/work",
		"-e", "HOME=/work/" + home, "-e", "DOCKER_CONFIG=/work/" + home + "/.docker", "-e", "COSIGN_PASSWORD=", "-e", "COSIGN_YES=true", "-e", "COSIGN_EXPERIMENTAL=1",
		"--entrypoint", "/tools/" + bin, o.image}
	cmd := exec.CommandContext(o.ctx, "docker", append(a, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	o.t.Logf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, redactToken(string(out)))
	return string(out), err
}

func (o *ociRunner) must(home, stdin, bin string, args ...string) string {
	o.t.Helper()
	out, err := o.run(home, stdin, bin, args...)
	if err != nil {
		o.t.Fatalf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, redactToken(out))
	}
	return out
}

// redactToken hides access tokens in a client's output.
func redactToken(s string) string {
	for _, p := range []string{"ya29.", "cbl_", "cbt_"} {
		for i := strings.Index(s, p); i >= 0; i = strings.Index(s, p) {
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\n\"',", rune(s[j])) {
				j++
			}
			s = s[:i] + "<token>" + s[j:]
		}
	}
	return s
}

// layerTar is a one-file tar layer.
func layerTar(t *testing.T, name, content string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), ModTime: time.Unix(0, 0)}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	return b.Bytes()
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateAttachment
//
// TestIAMEnforcementArtifactRegistryOCIClients (#2111), in the iam shard,
// with the real clients (see ociClientEnv) and gcloud:
//   - crane: `crane auth login -u oauth2accesstoken` with an access token
//     the instance minted, then append (push), digest and ls; a token the
//     instance did not mint is 401; an account with no role is 403
//     DENIED naming downloadArtifacts;
//   - oras: `oras login` (the Bearer challenge and the token exchange)
//     succeeds with a minted token and fails with another; `oras attach`
//     as Google documents it, over the referrers API, and `oras discover
//     --distribution-spec v1.1-referrers-api`, with --artifact-type; the
//     tag schema's fallback (v1.1-referrers-tag), whose sha256-<hex>
//     index is a tag like any other and is discovered the same way;
//     `oras pull` of the attachment; each referrer is an attachment the
//     official client's ListAttachments lists;
//   - cosign: generate-key-pair, sign with the key (no transparency log;
//     the network has no route out), verify, attach sbom, tree (its
//     sha256-<hex>.sig and .sbom tags);
//   - docker, in a docker:dind daemon on the same network: `docker login
//     -u oauth2accesstoken --password-stdin` with gcloud's `gcloud auth
//     print-access-token`, as Google documents, then import, push, pull;
//     a token not minted here fails to log in; an account with no role
//     logs in and is refused the pull (denied), then with Artifact
//     Registry Reader on the repository pulls and is refused the push;
//   - gcloud's credential helper: `docker-credential-gcloud get` for
//     us-central1-docker.pkg.dev answers _dcgcloud_token and a token
//     this registry takes, and docker logs in with it;
//   - gcloud artifacts attachments create (UploadFile, then
//     CreateAttachment: an OCI artifact oras discovers), list --target,
//     describe, download, delete.
func TestIAMEnforcementArtifactRegistryOCIClients(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	h.Endpoint(EnvMetadata)
	h.Endpoint(EnvResourceManager)
	requireIAMEnforcement(t)
	tools, image := ociClientEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	project := iamProject(t, h, "ioc-")
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ar.Close() })
	repoName, local := dockerRepository(t, h, ar, project, "oci-clients")
	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	reg := "host.docker.internal:" + port
	base := reg + "/" + project + "/oci-clients"

	_, owner := instanceAccount(t)
	ownerTok, err := owner.Token()
	if err != nil {
		t.Fatalf("the instance account's token: %v", err)
	}
	p := newIAMPrincipal(t, h, project, "ioc")
	o := &ociRunner{t: t, ctx: ctx, tools: tools, image: image, network: internalNetwork(t, ctx, image, port), work: t.TempDir()}
	for _, d := range []string{"owner", "nobody", "bad"} {
		if err := os.MkdirAll(filepath.Join(o.work, d), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(o.work, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	listAttachments := func() map[string]*arpb.Attachment {
		out := map[string]*arpb.Attachment{}
		it := ar.ListAttachments(ctx, &arpb.ListAttachmentsRequest{Parent: repoName})
		for {
			a, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("ListAttachments: %v", err)
			}
			out[a.GetType()] = a
		}
	}

	// crane: login, push, read; refusals.
	var digest string
	if o.has("crane") {
		o.must("owner", ownerTok.AccessToken+"\n", "crane", "auth", "login", reg, "-u", "oauth2accesstoken", "--password-stdin")
		if err := os.WriteFile(filepath.Join(o.work, "layer.tar"), layerTar(t, "hello.txt", "hello from crane"), 0o644); err != nil {
			t.Fatal(err)
		}
		o.must("owner", "", "crane", "--insecure", "append", "-f", "/work/layer.tar", "-t", base+"/app:v1")
		digest = strings.TrimSpace(o.must("owner", "", "crane", "--insecure", "digest", base+"/app:v1"))
		if ls := o.must("owner", "", "crane", "--insecure", "ls", base+"/app"); !strings.Contains(ls, "v1") {
			t.Errorf("crane ls = %q; want v1", ls)
		}
		o.must("bad", "cbl_not-minted-here\n", "crane", "auth", "login", reg, "-u", "oauth2accesstoken", "--password-stdin")
		if out, err := o.run("bad", "", "crane", "--insecure", "digest", base+"/app:v1"); err == nil || !strings.Contains(out, "UNAUTHORIZED") {
			t.Errorf("crane digest with a token the instance did not mint = %v: %s; want 401 UNAUTHORIZED", err, out)
		}
		o.must("nobody", p.token+"\n", "crane", "auth", "login", reg, "-u", "oauth2accesstoken", "--password-stdin")
		if out, err := o.run("nobody", "", "crane", "--insecure", "digest", base+"/app:v1"); err == nil || !strings.Contains(out, "DENIED") ||
			!strings.Contains(out, "artifactregistry.repositories.downloadArtifacts") {
			t.Errorf("crane digest with no role = %v: %s; want 403 DENIED naming downloadArtifacts", err, out)
		}
	}
	if digest == "" {
		// The image the other clients refer to, pushed as the instance's
		// account with go-containerregistry.
		img, err := crane.Image(map[string][]byte{"hello.txt": []byte("hello")})
		if err != nil {
			t.Fatal(err)
		}
		if err := crane.Push(img, local+":v1", append(registryOptions(h), crane.WithAuth(&authn.Basic{Username: "oauth2accesstoken",
			Password: ownerTok.AccessToken}))...); err != nil {
			t.Fatal(err)
		}
		d, _ := img.Digest()
		digest = d.String()
	}
	subject := base + "/app@" + digest

	// oras: login, attach (referrers API, then the tag schema), discover, pull.
	if o.has("oras") {
		if out, err := o.run("bad", "cbl_not-minted-here\n", "oras", "login", "--plain-http", "-u", "oauth2accesstoken", "--password-stdin", reg); err == nil {
			t.Errorf("oras login with a token the instance did not mint succeeded: %s", out)
		}
		if out := o.must("owner", ownerTok.AccessToken+"\n", "oras", "login", "--plain-http", "-u", "oauth2accesstoken", "--password-stdin", reg); !strings.Contains(out, "Login Succeeded") {
			t.Errorf("oras login = %q; want Login Succeeded", out)
		}
		write("hello-world.txt", "hello, attachments\n")
		// Google's example: oras attach --artifact-type doc/example IMAGE hello-world.txt:application/vnd.me.hi
		o.must("owner", "", "oras", "attach", "--plain-http", "--artifact-type", "doc/example", base+"/app:v1", "hello-world.txt:application/vnd.me.hi")
		write("notes.txt", "tag schema\n")
		o.must("owner", "", "oras", "attach", "--plain-http", "--distribution-spec", "v1.1-referrers-tag", "--artifact-type",
			"application/vnd.example.notes", subject, "notes.txt:text/plain")
		discover := func(spec string, extra ...string) []string {
			out := o.must("owner", "", "oras", append([]string{"discover", "--plain-http", "--distribution-spec", spec, "--format", "json", subject}, extra...)...)
			var got struct {
				Manifests []struct {
					Digest, ArtifactType string
				} `json:"manifests"`
			}
			if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &got); err != nil {
				t.Fatalf("oras discover --format json: %v\n%s", err, out)
			}
			var types []string
			for _, m := range got.Manifests {
				types = append(types, m.ArtifactType)
			}
			return types
		}
		if got := strings.Join(discover("v1.1-referrers-api"), ","); !strings.Contains(got, "doc/example") || !strings.Contains(got, "application/vnd.example.notes") {
			t.Errorf("oras discover over the referrers API = %s; want both attachments", got)
		}
		if got := discover("v1.1-referrers-api", "--artifact-type", "doc/example"); len(got) != 1 || got[0] != "doc/example" {
			t.Errorf("oras discover --artifact-type doc/example = %v; want doc/example alone", got)
		}
		if got := strings.Join(discover("v1.1-referrers-tag"), ","); !strings.Contains(got, "application/vnd.example.notes") {
			t.Errorf("oras discover over the tag schema = %s; want the notes", got)
		}
		tagged := "sha256-" + strings.TrimPrefix(digest, "sha256:")
		if out := o.must("owner", "", "oras", "repo", "tags", "--plain-http", base+"/app"); !strings.Contains(out, tagged) {
			t.Errorf("oras repo tags = %q; want the tag schema's %s", out, tagged)
		}
		as := listAttachments()
		for _, typ := range []string{"doc/example", "application/vnd.example.notes"} {
			if a := as[typ]; a == nil || !strings.HasSuffix(a.GetTarget(), "/versions/"+digest) || a.GetOciVersionName() == "" {
				t.Errorf("ListAttachments has %s as %v; want an attachment of the image's version", typ, a)
			}
		}
		if a := as["doc/example"]; a != nil {
			ref := base + "/app@" + a.GetOciVersionName()[strings.LastIndex(a.GetOciVersionName(), "/")+1:]
			if err := os.MkdirAll(filepath.Join(o.work, "pulled"), 0o777); err != nil {
				t.Fatal(err)
			}
			o.must("owner", "", "oras", "pull", "--plain-http", "-o", "/work/pulled", ref)
			if b, err := os.ReadFile(filepath.Join(o.work, "pulled", "hello-world.txt")); err != nil || string(b) != "hello, attachments\n" {
				t.Errorf("oras pull of the attachment = %q, %v", b, err)
			}
		}
	}

	// cosign: sign, verify, attach, tree, and an OCI 1.1 signature.
	if o.has("cosign") {
		if !o.has("crane") {
			o.must("owner", ownerTok.AccessToken+"\n", "cosign", "login", reg, "-u", "oauth2accesstoken", "--password-stdin")
		}
		flags := []string{"--allow-insecure-registry", "--allow-http-registry"}
		o.must("owner", "", "cosign", "generate-key-pair")
		o.must("owner", "", "cosign", append([]string{"sign", "--key", "cosign.key", "--tlog-upload=false"}, append(flags, subject)...)...)
		if out := o.must("owner", "", "cosign", append([]string{"verify", "--key", "cosign.pub", "--insecure-ignore-tlog=true"}, append(flags, subject)...)...); !strings.Contains(out, digest) {
			t.Errorf("cosign verify = %q; want the signed digest", out)
		}
		write("sbom.spdx.json", `{"spdxVersion":"SPDX-2.3","name":"app"}`)
		o.must("owner", "", "cosign", append([]string{"attach", "sbom", "--sbom", "sbom.spdx.json", "--type", "spdx"}, append(flags, subject)...)...)
		if out := o.must("owner", "", "cosign", append([]string{"tree"}, append(flags, subject)...)...); !strings.Contains(out, "Signatures") || !strings.Contains(out, "SBOMs") {
			t.Errorf("cosign tree = %q; want its signatures and SBOMs", out)
		}
		// cosign sign --registry-referrers-mode=oci-1-1 is not run: cosign
		// 2.4 dials that mode's registry over HTTPS whatever
		// --allow-http-registry says ("server gave HTTP response to HTTPS
		// client"), and this registry is plain HTTP on loopback. oras
		// covers the referrers API.
	}

	// docker, in a docker:dind daemon on the network.
	t.Run("docker", func(t *testing.T) {
		h := h.For(t)
		g := newGcloudSession(t, h)
		dind := fmt.Sprintf("cb-dind-%d", time.Now().UnixNano()%1e9)
		if b, err := exec.CommandContext(ctx, "docker", "run", "-d", "--privileged", "--name", dind, "--network", o.network,
			"-e", "DOCKER_TLS_CERTDIR=", image, "dockerd", "--insecure-registry="+reg).CombinedOutput(); err != nil {
			t.Fatalf("start the docker daemon: %v\n%s", err, b)
		}
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", dind).Run() })
		dockerIn := func(stdin string, args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", "-i", dind}, args...)...)
			cmd.Stdin = strings.NewReader(stdin)
			b, err := cmd.CombinedOutput()
			t.Logf("docker %s: %v\n%s", strings.Join(args, " "), err, redactToken(string(b)))
			return string(b), err
		}
		for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
			if _, err := exec.CommandContext(ctx, "docker", "exec", dind, "docker", "version").CombinedOutput(); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the docker daemon did not start")
			}
		}
		login := func(user, tok string) (string, error) {
			return dockerIn(tok, "docker", "login", "-u", user, "--password-stdin", reg)
		}
		if out, err := login("oauth2accesstoken", "cbl_not-minted-here"); err == nil {
			t.Errorf("docker login with a token the instance did not mint succeeded: %s", out)
		}
		// Google's documented login: gcloud auth print-access-token | docker login -u oauth2accesstoken --password-stdin
		gtok := strings.TrimSpace(g.must("auth", "print-access-token"))
		if out, err := login("oauth2accesstoken", gtok); err != nil || !strings.Contains(out, "Login Succeeded") {
			t.Fatalf("docker login with gcloud auth print-access-token = %v: %s", err, out)
		}
		img := base + "/dockerapp:v1"
		if out, err := dockerIn("", "sh", "-c", "mkdir -p /x && echo hi > /x/hi && tar -C /x -c . | docker import - "+img+" && docker push "+img); err != nil {
			t.Fatalf("docker import and push = %v: %s", err, out)
		}
		if out, err := dockerIn("", "sh", "-c", "docker rmi "+img+" && docker pull "+img); err != nil {
			t.Errorf("docker pull = %v: %s", err, out)
		}

		// gcloud's credential helper.
		helper := realTool(t, "docker-credential-gcloud")
		cmd := exec.CommandContext(ctx, helper, "get")
		cmd.Env = g.env
		cmd.Stdin = strings.NewReader("https://us-central1-docker.pkg.dev")
		b, err := cmd.Output()
		var cred struct{ Username, Secret string }
		if err != nil || json.Unmarshal(b, &cred) != nil || cred.Username != "_dcgcloud_token" || cred.Secret == "" {
			t.Fatalf("docker-credential-gcloud get = %v; want _dcgcloud_token and a token%s", err, stderrOf(err))
		}
		if _, err := crane.Digest(local+":v1", append(registryOptions(h), crane.WithAuth(&authn.Basic{Username: cred.Username, Password: cred.Secret}))...); err != nil {
			t.Errorf("a pull with docker-credential-gcloud's credentials: %v", err)
		}
		if out, err := login(cred.Username, cred.Secret); err != nil || !strings.Contains(out, "Login Succeeded") {
			t.Errorf("docker login -u _dcgcloud_token = %v: %s", err, out)
		}

		// An account with no role, then Reader on the repository.
		if out, err := login("oauth2accesstoken", p.token); err != nil {
			t.Fatalf("docker login as an account with no role = %v: %s", err, out)
		}
		if out, err := dockerIn("", "sh", "-c", "docker rmi "+img+"; docker pull "+img); err == nil || !strings.Contains(out, "denied") {
			t.Errorf("docker pull with no role = %v: %s; want denied", err, out)
		}
		undo := bindOn(t, ar.GetIamPolicy, ar.SetIamPolicy, repoName,
			&iampb.Binding{Role: "roles/artifactregistry.reader", Members: []string{"serviceAccount:" + p.email}})
		defer undo()
		waitGranted(t, "docker pull as a reader", func() error {
			out, err := dockerIn("", "docker", "pull", img)
			if err != nil {
				return fmt.Errorf("%v: %s", err, out)
			}
			return nil
		})
		if out, err := dockerIn("", "sh", "-c", "docker tag "+img+" "+base+"/dockerapp:v2 && docker push "+base+"/dockerapp:v2"); err == nil || !strings.Contains(out, "denied") {
			t.Errorf("docker push as a reader = %v: %s; want denied", err, out)
		}
	})

	// gcloud artifacts attachments.
	t.Run("gcloud", func(t *testing.T) {
		h := h.For(t)
		g := newGcloudSession(t, h)
		write("cyclonedx.json", `{"bomFormat":"CycloneDX","specVersion":"1.5"}`)
		target := repoName + "/packages/app/versions/" + digest
		common := []string{"--project", project, "--location", "us-central1", "--repository", "oci-clients"}
		g.must(append([]string{"artifacts", "attachments", "create", "bom", "--target", target, "--attachment-type",
			"application/vnd.cyclonedx+json", "--attachment-namespace", "example.com", "--files", filepath.Join(o.work, "cyclonedx.json")}, common...)...)
		a := listAttachments()["application/vnd.cyclonedx+json"]
		if a == nil || a.GetName() != repoName+"/attachments/bom" || a.GetOciVersionName() == "" || a.GetAttachmentNamespace() != "example.com" {
			t.Fatalf("ListAttachments has the gcloud attachment as %v; want bom, an OCI artifact", a)
		}
		if o.has("oras") {
			out := o.must("owner", "", "oras", "discover", "--plain-http", "--distribution-spec", "v1.1-referrers-api", "--artifact-type",
				"application/vnd.cyclonedx+json", subject)
			if !strings.Contains(out, a.GetOciVersionName()[strings.LastIndex(a.GetOciVersionName(), "/")+1:]) {
				t.Errorf("oras discover does not list the gcloud attachment's artifact: %s", out)
			}
		}
		uri := "us-central1-docker.pkg.dev/" + project + "/oci-clients/app@" + digest
		if out := g.must(append([]string{"artifacts", "attachments", "list", "--target", uri, "--format", "value(name)"}, common...)...); !strings.Contains(out, "bom") {
			t.Errorf("gcloud artifacts attachments list --target %s = %q; want bom", uri, out)
		}
		if out := g.must(append([]string{"artifacts", "attachments", "describe", "bom", "--format", "value(type)"}, common...)...); strings.TrimSpace(out) != "application/vnd.cyclonedx+json" {
			t.Errorf("gcloud artifacts attachments describe = %q", out)
		}
		dest := t.TempDir()
		g.must(append([]string{"artifacts", "attachments", "download", "bom", "--destination", dest}, common...)...)
		ents, _ := os.ReadDir(dest)
		if len(ents) != 1 {
			t.Errorf("gcloud artifacts attachments download wrote %d files; want 1", len(ents))
		} else if b, _ := os.ReadFile(filepath.Join(dest, ents[0].Name())); !strings.Contains(string(b), "CycloneDX") {
			t.Errorf("the downloaded attachment = %q", b)
		}
		g.must(append([]string{"artifacts", "attachments", "delete", "bom", "--quiet"}, common...)...)
		if a := listAttachments()["application/vnd.cyclonedx+json"]; a != nil {
			t.Errorf("ListAttachments after gcloud artifacts attachments delete = %v; want it gone", a)
		}
	})
}
