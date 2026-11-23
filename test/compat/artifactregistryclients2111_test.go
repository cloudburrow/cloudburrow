//go:build compat

package compat

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
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
//     the relays, and is the docker daemon docker login and push run in.
//
// scripts/oci-tools.sh fetches both by dependencies.json's realTools pins
// (each binary checked against its release's published SHA-256, the image
// pulled by digest), as CI's iam shard does.

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
	// env is more environment for every client (SSL_CERT_FILE, once
	// the TLS relay is up).
	env []string
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
// (each identity its own); it logs and returns the output. The client runs
// as the test's own user: run as the image's root, what it writes under
// /work (crane's 0700 .docker) is root's on a Linux host, and the test's
// TempDir cleanup cannot remove it (main run 45047408927).
func (o *ociRunner) run(home, stdin, bin string, args ...string) (string, error) {
	o.t.Helper()
	a := []string{"run", "--rm", "-i", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--network", o.network, "-v", o.tools + ":/tools:ro", "-v", o.work + ":/work", "-w", "/work",
		"-e", "HOME=/work/" + home, "-e", "DOCKER_CONFIG=/work/" + home + "/.docker", "-e", "COSIGN_PASSWORD=", "-e", "COSIGN_YES=true", "-e", "COSIGN_EXPERIMENTAL=1",
		"--entrypoint", "/tools/" + bin}
	for _, e := range o.env {
		a = append(a, "-e", e)
	}
	a = append(a, o.image)
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

// tlsRelaySource is a reverse proxy, on :443 with the certificate and key
// its first two arguments name (once they are written: the certificate
// names the relay's address on the internal network, known only once it
// is connected), to the plain HTTP registry at its third,
// saying X-Forwarded-Proto: https so the registry's Bearer challenge names
// an https token endpoint. Clients that dial HTTPS whatever their insecure
// flags say (cosign's OCI 1.1 mode) reach the registry through it.
const tlsRelaySource = `package main

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"
)

func main() {
	host, port, err := net.SplitHostPort(os.Args[3])
	if err != nil {
		panic(err)
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		panic(err)
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(addrs[0], port)}
	p := httputil.NewSingleHostReverseProxy(target)
	direct := p.Director
	p.Director = func(r *http.Request) { direct(r); r.Header.Set("X-Forwarded-Proto", "https") }
	p.FlushInterval = -1
	for {
		if _, err := os.Stat(os.Args[2]); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	ln, err := net.Listen("tcp", ":443")
	if err != nil {
		panic(err)
	}
	println("ready", target.Host)
	panic(http.ServeTLS(ln, p, os.Args[1], os.Args[2]))
}
`

// tlsRelay serves the registry on the Run port over TLS to o's network at
// the relay's address there, which it returns (the registry answers an IP
// address, ADR-0004's DNS-rebinding defence refusing other names): the
// relay built for the daemon's architecture and run as internalNetwork
// runs its own, a CA and a leaf certificate for that address made here
// (never trusted outside the test), and SSL_CERT_FILE set for every later
// client to the CA, so they verify the registry's certificate rather than
// skip verification.
func (o *ociRunner) tlsRelay(port string) string {
	t := o.t
	t.Helper()
	dir := filepath.Join(o.work, "tls")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	arch, err := exec.CommandContext(o.ctx, "docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatalf("docker version: %v", err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(tlsRelaySource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module tlsrelay\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(o.ctx, "go", "build", "-o", filepath.Join(dir, "relay"), ".")
	build.Dir = src
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0", "GOFLAGS=")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the TLS relay: %v\n%s", err, b)
	}
	relay := fmt.Sprintf("cb-tlsrelay-%d", time.Now().UnixNano()%1e9)
	args := append([]string{"run", "-d", "--name", relay, "-v", dir + ":/relay:ro", "--entrypoint", "/relay/relay"}, hostRelayArgs(t, o.ctx)...)
	args = append(args, o.image, "/relay/cert.pem", "/relay/key.pem", "host.docker.internal:"+port)
	if b, err := exec.CommandContext(o.ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("start the TLS relay: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", relay).Run() })
	if b, err := exec.CommandContext(o.ctx, "docker", "network", "connect", o.network, relay).CombinedOutput(); err != nil {
		t.Fatalf("connect the TLS relay: %v\n%s", err, b)
	}
	b, err := exec.CommandContext(o.ctx, "docker", "inspect", "--format", "{{(index .NetworkSettings.Networks \""+o.network+"\").IPAddress}}", relay).Output()
	ip := net.ParseIP(strings.TrimSpace(string(b)))
	if err != nil || ip == nil {
		t.Fatalf("the TLS relay's address on %s = %q, %v", o.network, b, err)
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CloudBurrow test registry CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: ip.String()}, IPAddresses: []net.IP{ip},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	// The key last: the relay starts serving once it is there.
	for _, f := range []struct {
		name string
		b    *pem.Block
	}{{"ca.pem", &pem.Block{Type: "CERTIFICATE", Bytes: caDER}}, {"cert.pem", &pem.Block{Type: "CERTIFICATE", Bytes: leafDER}},
		{"key.pem", &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}}} {
		if err := os.WriteFile(filepath.Join(dir, f.name), pem.EncodeToMemory(f.b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		logs, _ := exec.CommandContext(o.ctx, "docker", "logs", relay).CombinedOutput()
		if strings.Contains(string(logs), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the TLS relay did not start:\n%s", logs)
		}
	}
	o.env = append(o.env, "SSL_CERT_FILE=/work/tls/ca.pem")
	return ip.String()
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
//     sha256-<hex>.sig and .sbom tags); then, over TLS through a relay
//     whose certificate a CA made here signs (cosign trusts it by
//     SSL_CERT_FILE, no insecure flag), sign
//     --registry-referrers-mode=oci-1-1, which the referrers API lists
//     and ListAttachments has, and verify --experimental-oci11;
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

		// cosign's OCI 1.1 mode dials HTTPS whatever --allow-http-registry
		// says ("server gave HTTP response to HTTPS client"), so it runs
		// through the TLS relay, with the test CA trusted and no insecure
		// flag: the signature is an OCI 1.1 artifact whose subject is the
		// image, which the referrers API lists and verify finds there.
		tlsHost := o.tlsRelay(port)
		tsubject := tlsHost + "/" + project + "/oci-clients/app@" + digest
		o.must("owner", ownerTok.AccessToken+"\n", "cosign", "login", tlsHost, "-u", "oauth2accesstoken", "--password-stdin")
		o.must("owner", "", "cosign", "sign", "--key", "cosign.key", "--tlog-upload=false", "--registry-referrers-mode=oci-1-1", tsubject)
		if out := o.must("owner", "", "cosign", "verify", "--experimental-oci11", "--key", "cosign.pub", "--insecure-ignore-tlog=true",
			tsubject); !strings.Contains(out, digest) {
			t.Errorf("cosign verify --experimental-oci11 = %q; want the signed digest", out)
		}
		const sigType = "application/vnd.dev.cosign.artifact.sig.v1+json"
		req, _ := http.NewRequest(http.MethodGet, "http://"+strings.SplitN(local, "/", 2)[0]+"/v2/"+project+"/oci-clients/app/referrers/"+digest+
			"?artifactType="+url.QueryEscape(sigType), nil)
		req.SetBasicAuth("oauth2accesstoken", ownerTok.AccessToken)
		resp, err := (&http.Client{Transport: registryTransport(h)}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var idx struct {
			Manifests []struct{ ArtifactType string } `json:"manifests"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&idx)
		resp.Body.Close()
		if resp.StatusCode != 200 || len(idx.Manifests) != 1 || idx.Manifests[0].ArtifactType != sigType {
			t.Errorf("referrers of the image with artifactType %s = %d %+v; want cosign's OCI 1.1 signature", sigType, resp.StatusCode, idx.Manifests)
		}
		if a := listAttachments()[sigType]; a == nil || !strings.HasSuffix(a.GetTarget(), "/versions/"+digest) {
			t.Errorf("ListAttachments has cosign's OCI 1.1 signature as %v; want an attachment of the image's version", a)
		}
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
