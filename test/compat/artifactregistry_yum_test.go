//go:build compat

package compat

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"

	"github.com/cloudburrow/cloudburrow/internal/rpmfixture"
)

// A YUM repository's createrepo metadata (#1539), at Google's
// {location}-yum.pkg.dev/projects/{project}/{repository}/repodata/.

// yumFixtures are two packages, the first requiring the second.
func yumFixtures() (hello, data rpmfixture.Package) {
	epoch := int32(1)
	hello = rpmfixture.Package{Name: "cbhello", Version: "1.2", Release: "3", Arch: "noarch", Epoch: &epoch,
		Summary: "Says hello", Description: "A test package that says hello.", BuildTime: 1760000000,
		Files: []rpmfixture.File{{Path: "/usr/bin/cbhello", Mode: 0o755,
			Body: []byte("#!/bin/sh\ncat /usr/share/cbhello-data/greeting\n")}},
		Provides:  []rpmfixture.Dep{{Name: "cbhello", Flags: "EQ", EVR: "1:1.2-3"}},
		Requires:  []rpmfixture.Dep{{Name: "cbhello-data", Flags: "GE", EVR: "0.9"}, {Name: "/bin/sh"}},
		Obsoletes: []rpmfixture.Dep{{Name: "cbhello-old", Flags: "LT", EVR: "1.0"}},
		Changelog: []rpmfixture.Change{{Time: 1760000000, Author: "Test <t@example.com> - 1:1.2-3", Text: "- first build"}}}
	data = rpmfixture.Package{Name: "cbhello-data", Version: "1.0", Release: "1", Arch: "noarch",
		Summary: "The greeting", Description: "The greeting cbhello prints.", BuildTime: 1760000000,
		Files:    []rpmfixture.File{{Path: "/usr/share/cbhello-data/greeting", Body: []byte("hello from a yum repository\n")}},
		Provides: []rpmfixture.Dep{{Name: "cbhello-data", Flags: "EQ", EVR: "1.0-1"}}}
	return hello, data
}

type repomdXML struct {
	Revision string `xml:"revision"`
	Data     []struct {
		Type         string `xml:"type,attr"`
		Checksum     string `xml:"checksum"`
		OpenChecksum string `xml:"open-checksum"`
		Location     struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
		Timestamp int64 `xml:"timestamp"`
		Size      int   `xml:"size"`
		OpenSize  int   `xml:"open-size"`
	} `xml:"data"`
}

type primaryXML struct {
	Packages []struct {
		Name    string `xml:"name"`
		Arch    string `xml:"arch"`
		Version struct {
			Epoch string `xml:"epoch,attr"`
			Ver   string `xml:"ver,attr"`
			Rel   string `xml:"rel,attr"`
		} `xml:"version"`
		Checksum string `xml:"checksum"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
		Format struct {
			Requires []struct {
				Name  string `xml:"name,attr"`
				Flags string `xml:"flags,attr"`
				Ver   string `xml:"ver,attr"`
			} `xml:"requires>entry"`
			Files []string `xml:"file"`
		} `xml:"format"`
	} `xml:"package"`
}

// yumRepodata reads repomd.xml and every document it names, checking
// each one's checksums and sizes; it returns the uncompressed documents
// by type, and the revision.
func yumRepodata(t *testing.T, base string) (map[string][]byte, string) {
	t.Helper()
	var md repomdXML
	if err := xml.Unmarshal(mustHTTP(t, "GET", base+"/repodata/repomd.xml", "", nil), &md); err != nil {
		t.Fatalf("repomd.xml: %v", err)
	}
	out := map[string][]byte{}
	for _, d := range md.Data {
		gz := mustHTTP(t, "GET", base+"/"+d.Location.Href, "", nil)
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			t.Fatalf("%s: %v", d.Location.Href, err)
		}
		body, _ := io.ReadAll(zr)
		sum, open := sha256.Sum256(gz), sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != d.Checksum || hex.EncodeToString(open[:]) != d.OpenChecksum ||
			len(gz) != d.Size || len(body) != d.OpenSize || d.Timestamp == 0 {
			t.Errorf("repomd's %s entry does not match %s: %+v", d.Type, d.Location.Href, d)
		}
		out[d.Type] = body
	}
	for _, typ := range []string{"primary", "filelists", "other"} {
		if out[typ] == nil {
			t.Fatalf("repomd.xml names no %s document: %+v", typ, md)
		}
	}
	return out, md.Revision
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/ImportYumArtifacts, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteVersion
//
// TestArtifactRegistryYumRepodata (#1539): two .rpm packages imported
// from this instance's Cloud Storage into a YUM repository are listed in
// its createrepo metadata at Google's documented baseurl,
// {location}-yum.pkg.dev/projects/{project}/{repository}: repomd.xml
// names primary, filelists and other with checksums and sizes that match
// what is served, and primary lists each package's name, epoch, version,
// requires and location from its header. When docker has an image with
// dnf, microdnf or yum on this machine, a container on a network with no
// route out but this instance's Run port installs the first package with
// it, which pulls in the second through its requires. Deleting a version
// makes the metadata again without it.
func TestArtifactRegistryYumRepodata(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	gcs := instanceStorage(t, h)
	parent := runParent(h)
	bucket := gcs.Bucket("ar-yum-" + strings.TrimPrefix(h.Project(), "cb-test-"))
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = bucket.Delete(context.Background()) })
	hello, data := yumFixtures()
	var uris []string
	for _, p := range []rpmfixture.Package{hello, data} {
		object := rpmfixture.FileName(p)
		w := bucket.Object(object).NewWriter(ctx)
		_, _ = w.Write(rpmfixture.Build(p))
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", object, err)
		}
		t.Cleanup(func() { _ = bucket.Object(object).Delete(context.Background()) })
		uris = append(uris, "gs://"+bucket.BucketName()+"/"+object)
	}
	yum := formatRepo(t, ctx, ar, parent, "yum-dnf", &arpb.Repository{Format: arpb.Repository_YUM})
	op, err := ar.ImportYumArtifacts(ctx, &arpb.ImportYumArtifactsRequest{Parent: yum.GetName(),
		Source: &arpb.ImportYumArtifactsRequest_GcsSource{GcsSource: &arpb.ImportYumArtifactsGcsSource{Uris: uris}}})
	if err != nil {
		t.Fatalf("ImportYumArtifacts: %v", err)
	}
	if resp, err := op.Wait(ctx); err != nil || len(resp.GetYumArtifacts()) != 2 {
		t.Fatalf("ImportYumArtifacts = %v, %v; want both packages", resp, err)
	}

	path := "/us-central1-yum.pkg.dev/projects/" + h.Project() + "/yum-dnf"
	base := "http://" + h.Endpoint(EnvRun) + path
	docs, rev := yumRepodata(t, base)
	var primary primaryXML
	if err := xml.Unmarshal(docs["primary"], &primary); err != nil {
		t.Fatalf("primary.xml: %v", err)
	}
	if len(primary.Packages) != 2 {
		t.Fatalf("primary.xml lists %d packages; want 2:\n%s", len(primary.Packages), docs["primary"])
	}
	p := primary.Packages[0]
	if p.Name != "cbhello" || p.Arch != "noarch" || p.Version.Epoch != "1" || p.Version.Ver != "1.2" || p.Version.Rel != "3" ||
		p.Location.Href != rpmfixture.FileName(hello) {
		t.Errorf("primary.xml's cbhello = %+v", p)
	}
	build := sha256.Sum256(rpmfixture.Build(hello))
	if p.Checksum != hex.EncodeToString(build[:]) {
		t.Errorf("cbhello's checksum = %s; want the .rpm's SHA-256", p.Checksum)
	}
	if len(p.Format.Requires) != 2 || p.Format.Requires[0].Name != "cbhello-data" || p.Format.Requires[0].Flags != "GE" ||
		p.Format.Requires[0].Ver != "0.9" || p.Format.Requires[1].Name != "/bin/sh" {
		t.Errorf("cbhello's requires = %+v; want cbhello-data >= 0.9 and /bin/sh, without rpmlib()", p.Format.Requires)
	}
	if len(p.Format.Files) != 1 || p.Format.Files[0] != "/usr/bin/cbhello" {
		t.Errorf("cbhello's primary files = %v", p.Format.Files)
	}
	if !strings.Contains(string(docs["filelists"]), "<file>/usr/share/cbhello-data/greeting</file>") ||
		!strings.Contains(string(docs["other"]), "- first build</changelog>") {
		t.Errorf("filelists.xml:\n%s\nother.xml:\n%s", docs["filelists"], docs["other"])
	}
	if got := mustHTTP(t, "GET", base+"/"+p.Location.Href, "", nil); !bytes.Equal(got, rpmfixture.Build(hello)) {
		t.Errorf("%s is not the imported package", p.Location.Href)
	}

	t.Run("dnf", func(t *testing.T) {
		h := h.For(t)
		yumInstall(t, h, path)
	})

	// Deleting cbhello-data's version makes the metadata again.
	dop, err := ar.DeleteVersion(ctx, &arpb.DeleteVersionRequest{Name: yum.GetName() + "/packages/cbhello-data/versions/1.0-1"})
	if err != nil {
		t.Fatalf("DeleteVersion: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteVersion .Wait: %v", err)
	}
	docs, rev2 := yumRepodata(t, base)
	if rev2 == rev || strings.Contains(string(docs["primary"]), "<name>cbhello-data</name>") ||
		!strings.Contains(string(docs["primary"]), `packages="1"`) {
		t.Errorf("after DeleteVersion, revision %s (was %s), primary.xml:\n%s", rev2, rev, docs["primary"])
	}
}

// dnfImage is a local image with dnf, microdnf or yum, and that command:
// $CB_DNF_IMAGE, or the first image here whose name suggests one. Nothing
// is pulled.
func dnfImage(t *testing.T) (image, command string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	var candidates []string
	if v := os.Getenv("CB_DNF_IMAGE"); v != "" {
		candidates = []string{v}
	} else {
		out, err := exec.Command("docker", "images", "--format", "{{.Repository}}:{{.Tag}}").Output()
		if err != nil {
			t.Skipf("docker images: %v", err)
		}
		likely := regexp.MustCompile(`(?i)fedora|rocky|alma|centos|ubi|rhel|redhat|oracle|amazonlinux|mariner|azurelinux|cockroach`)
		for _, line := range strings.Fields(string(out)) {
			if likely.MatchString(line) && !strings.HasSuffix(line, ":<none>") {
				candidates = append(candidates, line)
			}
		}
	}
	for _, img := range candidates {
		out, err := exec.Command("docker", "run", "--rm", "--network", "none", "--entrypoint", "sh", img, "-c",
			"command -v dnf || command -v microdnf || command -v yum").Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			return img, filepath.Base(strings.TrimSpace(strings.Split(string(out), "\n")[0]))
		}
	}
	t.Skip("no local image has dnf, microdnf or yum (set CB_DNF_IMAGE; nothing is pulled)")
	return "", ""
}

// forwarderSource relays one TCP port to another address: the dnf
// container's only route, to this instance's Run port.
const forwarderSource = `package main

import (
	"io"
	"net"
	"os"
)

func main() {
	// The target's address is resolved once, before the relay joins the
	// network where its own alias would shadow the target's name.
	host, port, err := net.SplitHostPort(os.Args[2])
	if err != nil {
		panic(err)
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		panic(err)
	}
	target := net.JoinHostPort(addrs[0], port)
	ln, err := net.Listen("tcp", os.Args[1])
	if err != nil {
		panic(err)
	}
	println("ready", target)
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer c.Close()
			u, err := net.Dial("tcp", target)
			if err != nil {
				return
			}
			defer u.Close()
			go io.Copy(u, c)
			io.Copy(c, u)
		}()
	}
}
`

// yumInstall installs cbhello with the image's dnf from the repository at
// path on the Run port. The dnf container is on an internal Docker
// network, with no route out; a relay on that network and the default
// bridge, named host.docker.internal there as Docker Desktop names the
// host, forwards the Run port alone, so the repository is all dnf can
// reach.
func yumInstall(t *testing.T, h *Harness, path string) {
	image, dnf := dnfImage(t)
	_, port, err := net.SplitHostPort(h.Endpoint(EnvRun))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	arch, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatalf("docker version: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(forwarderSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module relay\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", "relay", ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0", "GOFLAGS=")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the relay: %v\n%s", err, b)
	}
	repo := fmt.Sprintf("[cbtest]\nname=cbtest\nbaseurl=http://host.docker.internal:%s%s\nenabled=1\nrepo_gpgcheck=0\ngpgcheck=0\n", port, path)
	if err := os.WriteFile(filepath.Join(dir, "cbtest.repo"), []byte(repo), 0o644); err != nil {
		t.Fatal(err)
	}

	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	network, relay := "cb-yum-"+suffix, "cb-yum-relay-"+suffix
	if b, err := exec.CommandContext(ctx, "docker", "network", "create", "--internal", network).CombinedOutput(); err != nil {
		t.Fatalf("create the internal network: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	args := []string{"run", "-d", "--name", relay, "-v", dir + ":/relay:ro", "--entrypoint", "/relay/relay"}
	if runtime.GOOS == "linux" {
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}
	args = append(args, image, ":"+port, "host.docker.internal:"+port)
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("start the relay: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", relay).Run() })
	for deadline := time.Now().Add(30 * time.Second); ; {
		logs, _ := exec.CommandContext(ctx, "docker", "logs", relay).CombinedOutput()
		if strings.Contains(string(logs), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the relay did not start:\n%s", logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if b, err := exec.CommandContext(ctx, "docker", "network", "connect", "--alias", "host.docker.internal", network, relay).CombinedOutput(); err != nil {
		t.Fatalf("connect the relay: %v\n%s", err, b)
	}

	script := dnf + " -y --disablerepo='*' --enablerepo=cbtest install cbhello && cbhello && rpm -q cbhello cbhello-data"
	if dnf == "microdnf" {
		script = "microdnf -y --disablerepo='*' --enablerepo=cbtest --setopt=install_weak_deps=0 install cbhello && cbhello && rpm -q cbhello cbhello-data"
	}
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", network,
		"-v", filepath.Join(dir, "cbtest.repo")+":/etc/yum.repos.d/cbtest.repo:ro", "--entrypoint", "sh", image, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%s install cbhello in %s: %v\n%s", dnf, image, err, out)
	}
	if !strings.Contains(string(out), "hello from a yum repository") || !strings.Contains(string(out), "cbhello-1.2-3.noarch") ||
		!strings.Contains(string(out), "cbhello-data-1.0-1.noarch") {
		t.Errorf("%s install cbhello:\n%s", dnf, out)
	}
	t.Logf("%s in %s installed from the repository:\n%s", dnf, image, out)
}
