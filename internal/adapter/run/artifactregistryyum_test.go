package run

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/rpmfixture"
)

func yumTestPackages() []rpmfixture.Package {
	epoch := int32(2)
	return []rpmfixture.Package{{
		Name: "tool", Version: "3.1", Release: "4.el9", Arch: "x86_64", Epoch: &epoch,
		Summary: "A <tool> & more", Description: "Line one.\nLine \"two\".", License: "Apache-2.0",
		URL: "https://example.com/tool", Group: "Applications/System", Packager: "Builder <b@example.com>",
		Vendor: "Example", BuildHost: "build.example.com", BuildTime: 1750000000,
		Files: []rpmfixture.File{
			{Path: "/etc/tool.conf", Body: []byte("x=1\n")},
			{Path: "/usr/bin/tool", Body: []byte("#!/bin/sh\n"), Mode: 0o755},
			{Path: "/usr/share/doc/tool", Dir: true},
			{Path: "/usr/share/doc/tool/README", Body: []byte("read me\n")},
		},
		Provides:  []rpmfixture.Dep{{Name: "tool", Flags: "EQ", EVR: "2:3.1-4.el9"}, {Name: "tool(x86-64)", Flags: "EQ", EVR: "2:3.1-4.el9"}, {Name: "config(tool)"}},
		Requires:  []rpmfixture.Dep{{Name: "libc.so.6()(64bit)"}, {Name: "toollib", Flags: "GE", EVR: "1:2.0-1"}, {Name: "/etc/tool.conf"}, {Name: "toollib", Flags: "GE", EVR: "1:2.0-1"}},
		Conflicts: []rpmfixture.Dep{{Name: "othertool", Flags: "LT", EVR: "5"}},
		Obsoletes: []rpmfixture.Dep{{Name: "tool-legacy", Flags: "LE", EVR: "2.9-1"}},
		Changelog: []rpmfixture.Change{{Time: 1750000000, Author: "Builder <b@example.com> - 2:3.1-4", Text: "- fix <it>"},
			{Time: 1740000000, Author: "Builder <b@example.com> - 2:3.0-1", Text: "- first"}},
	}, {
		Name: "tool-src", Version: "3.1", Release: "4.el9", Arch: "x86_64", Source: true,
		Summary: "source", Description: "source package", BuildTime: 1750000000,
	}}
}

// TestParseRPMHeader reads the header fields the repository metadata
// lists from known packages.
func TestParseRPMHeader(t *testing.T) {
	pkgs := yumTestPackages()
	b := rpmfixture.Build(pkgs[0])
	p, err := parseRPM(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "tool" || p.Epoch != "2" || p.Version != "3.1" || p.Release != "4.el9" || p.Arch != "x86_64" ||
		p.Summary != "A <tool> & more" || p.Description != "Line one.\nLine \"two\"." || p.License != "Apache-2.0" ||
		p.URL != "https://example.com/tool" || p.Group != "Applications/System" || p.Packager != "Builder <b@example.com>" ||
		p.Vendor != "Example" || p.BuildHost != "build.example.com" || p.BuildTime != 1750000000 ||
		p.SourceRPM != "tool-3.1-4.el9.src.rpm" || p.Installed != int64(len("x=1\n#!/bin/sh\nread me\n")) || p.Archive == 0 {
		t.Errorf("parseRPM = %+v", p)
	}
	if p.HeaderStart <= 96 || p.HeaderStart%8 != 0 || p.HeaderEnd <= p.HeaderStart || p.HeaderEnd >= len(b) ||
		!bytes.Equal(b[p.HeaderStart:p.HeaderStart+3], []byte{0x8e, 0xad, 0xe8}) {
		t.Errorf("header range %d-%d of %d bytes", p.HeaderStart, p.HeaderEnd, len(b))
	}
	wantFiles := []rpmFile{{Path: "/etc/tool.conf"}, {Path: "/usr/bin/tool"}, {Path: "/usr/share/doc/tool", Dir: true}, {Path: "/usr/share/doc/tool/README"}}
	if !reflect.DeepEqual(p.Files, wantFiles) {
		t.Errorf("files = %+v; want %+v", p.Files, wantFiles)
	}
	wantProvides := []rpmDep{{Name: "tool", Flags: "EQ", Epoch: "2", Ver: "3.1", Rel: "4.el9"}, {Name: "tool(x86-64)", Flags: "EQ", Epoch: "2", Ver: "3.1", Rel: "4.el9"}, {Name: "config(tool)"}}
	if !reflect.DeepEqual(clearRaw(p.Provides), wantProvides) {
		t.Errorf("provides = %+v", p.Provides)
	}
	// rpmlib(), the package's own file and the duplicate are dropped.
	wantRequires := []rpmDep{{Name: "libc.so.6()(64bit)"}, {Name: "toollib", Flags: "GE", Epoch: "1", Ver: "2.0", Rel: "1"}}
	if !reflect.DeepEqual(clearRaw(p.Requires), wantRequires) {
		t.Errorf("requires = %+v; want %+v", p.Requires, wantRequires)
	}
	if !reflect.DeepEqual(clearRaw(p.Conflicts), []rpmDep{{Name: "othertool", Flags: "LT", Epoch: "0", Ver: "5"}}) ||
		!reflect.DeepEqual(clearRaw(p.Obsoletes), []rpmDep{{Name: "tool-legacy", Flags: "LE", Epoch: "0", Ver: "2.9", Rel: "1"}}) {
		t.Errorf("conflicts = %+v, obsoletes = %+v", p.Conflicts, p.Obsoletes)
	}
	if len(p.Changelog) != 2 || p.Changelog[0] != (rpmChange{Author: "Builder <b@example.com> - 2:3.1-4", Date: 1750000000, Text: "- fix <it>"}) {
		t.Errorf("changelog = %+v", p.Changelog)
	}

	src, err := parseRPM(rpmfixture.Build(pkgs[1]))
	if err != nil || src.Arch != "src" || src.SourceRPM != "" || src.Epoch != "0" {
		t.Errorf("source package = %+v, %v", src, err)
	}
	if _, err := parseRPM([]byte("not an rpm")); err == nil {
		t.Error("parseRPM accepted a text file")
	}
	if _, err := parseRPM(b[:200]); err == nil {
		t.Error("parseRPM accepted a truncated package")
	}
}

func clearRaw(ds []rpmDep) []rpmDep {
	out := make([]rpmDep, len(ds))
	for i, d := range ds {
		d.rawFlag = 0
		out[i] = d
	}
	return out
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestYumRepodata checks repomd.xml's checksums and sizes against the
// documents, and primary, filelists and other against the packages'
// headers.
func TestYumRepodata(t *testing.T) {
	var pkgs []yumPackage
	for _, fp := range yumTestPackages() {
		b := rpmfixture.Build(fp)
		p, err := parseRPM(b)
		if err != nil {
			t.Fatal(err)
		}
		s := sha256.Sum256(b)
		pkgs = append(pkgs, yumPackage{info: p, file: fmtFile{ID: rpmfixture.FileName(fp), Digest: "sha256:" + hex.EncodeToString(s[:]),
			Size: int64(len(b)), Uploaded: time.Unix(1760000000, 0)}})
	}
	md := buildYumRepodata(pkgs, 1760000001)

	var repomd struct {
		Revision string `xml:"revision"`
		Data     []struct {
			Type         string `xml:"type,attr"`
			Checksum     string `xml:"checksum"`
			OpenChecksum string `xml:"open-checksum"`
			Href         struct {
				V string `xml:"href,attr"`
			} `xml:"location"`
			Timestamp string `xml:"timestamp"`
			Size      int    `xml:"size"`
			OpenSize  int    `xml:"open-size"`
		} `xml:"data"`
	}
	if err := xml.Unmarshal(md.files["repomd.xml"], &repomd); err != nil {
		t.Fatalf("repomd.xml: %v\n%s", err, md.repomd)
	}
	if repomd.Revision != "1760000001" || len(repomd.Data) != 3 {
		t.Fatalf("repomd.xml = %+v", repomd)
	}
	docs := map[string][]byte{}
	for _, d := range repomd.Data {
		name := strings.TrimPrefix(d.Href.V, "repodata/")
		gz := md.files[name]
		body := gunzip(t, gz)
		if name != d.Type+".xml.gz" || sha256Hex(gz) != d.Checksum || sha256Hex(body) != d.OpenChecksum ||
			len(gz) != d.Size || len(body) != d.OpenSize || d.Timestamp != "1760000001" {
			t.Errorf("repomd's %s entry = %+v", d.Type, d)
		}
		var any struct{}
		if err := xml.Unmarshal(body, &any); err != nil {
			t.Errorf("%s is not well-formed XML: %v", name, err)
		}
		docs[d.Type] = body
	}

	primary := string(docs["primary"])
	for _, want := range []string{
		`packages="2"`,
		"<name>tool</name>\n  <arch>x86_64</arch>\n  <version epoch=\"2\" ver=\"3.1\" rel=\"4.el9\"/>",
		`<checksum type="sha256" pkgid="YES">` + strings.TrimPrefix(pkgs[0].file.Digest, "sha256:") + "</checksum>",
		"<summary>A &lt;tool&gt; &amp; more</summary>",
		"<description>Line one.&#xA;Line &#34;two&#34;.</description>",
		"<packager>Builder &lt;b@example.com&gt;</packager>",
		"<url>https://example.com/tool</url>",
		`<time file="1760000000" build="1750000000"/>`,
		`<location href="tool-3.1-4.el9.x86_64.rpm"/>`,
		"<rpm:license>Apache-2.0</rpm:license>", "<rpm:vendor>Example</rpm:vendor>", "<rpm:group>Applications/System</rpm:group>",
		"<rpm:buildhost>build.example.com</rpm:buildhost>", "<rpm:sourcerpm>tool-3.1-4.el9.src.rpm</rpm:sourcerpm>",
		`<rpm:entry name="tool" flags="EQ" epoch="2" ver="3.1" rel="4.el9"/>`,
		`<rpm:entry name="config(tool)"/>`,
		"<rpm:requires>\n      <rpm:entry name=\"libc.so.6()(64bit)\"/>\n      <rpm:entry name=\"toollib\" flags=\"GE\" epoch=\"1\" ver=\"2.0\" rel=\"1\"/>\n    </rpm:requires>",
		`<rpm:conflicts>` + "\n" + `      <rpm:entry name="othertool" flags="LT" epoch="0" ver="5"/>`,
		`<rpm:entry name="tool-legacy" flags="LE" epoch="0" ver="2.9" rel="1"/>`,
		"<file>/etc/tool.conf</file>\n    <file>/usr/bin/tool</file>\n  </format>",
		"<name>tool-src</name>\n  <arch>src</arch>",
		`<location href="tool-src-3.1-4.el9.src.rpm"/>`,
	} {
		if !strings.Contains(primary, want) {
			t.Errorf("primary.xml has no %q:\n%s", want, primary)
		}
	}
	if strings.Contains(primary, "rpmlib(") || strings.Contains(primary, "README") {
		t.Errorf("primary.xml lists rpmlib() requires or files filelists alone lists:\n%s", primary)
	}
	m := regexp.MustCompile(`<rpm:header-range start="(\d+)" end="(\d+)"/>`).FindStringSubmatch(primary)
	if m == nil || m[1] != strconv.Itoa(pkgs[0].info.HeaderStart) || m[2] != strconv.Itoa(pkgs[0].info.HeaderEnd) {
		t.Errorf("header-range = %v; want %d-%d", m, pkgs[0].info.HeaderStart, pkgs[0].info.HeaderEnd)
	}

	pkgid := strings.TrimPrefix(pkgs[0].file.Digest, "sha256:")
	if want := "<package pkgid=\"" + pkgid + "\" name=\"tool\" arch=\"x86_64\">\n  <version epoch=\"2\" ver=\"3.1\" rel=\"4.el9\"/>\n" +
		"  <file>/etc/tool.conf</file>\n  <file>/usr/bin/tool</file>\n  <file type=\"dir\">/usr/share/doc/tool</file>\n  <file>/usr/share/doc/tool/README</file>\n</package>"; !strings.Contains(string(docs["filelists"]), want) {
		t.Errorf("filelists.xml has no\n%s\n:\n%s", want, docs["filelists"])
	}
	if want := "<package pkgid=\"" + pkgid + "\" name=\"tool\" arch=\"x86_64\">\n  <version epoch=\"2\" ver=\"3.1\" rel=\"4.el9\"/>\n" +
		"  <changelog author=\"Builder &lt;b@example.com&gt; - 2:3.1-4\" date=\"1750000000\">- fix &lt;it&gt;</changelog>\n" +
		"  <changelog author=\"Builder &lt;b@example.com&gt; - 2:3.0-1\" date=\"1740000000\">- first</changelog>\n</package>"; !strings.Contains(string(docs["other"]), want) {
		t.Errorf("other.xml has no\n%s\n:\n%s", want, docs["other"])
	}

	// The same packages make the same documents: repomd's checksums hold
	// across requests.
	again := buildYumRepodata(pkgs, 1760000001)
	if !bytes.Equal(again.repomd, md.repomd) {
		t.Error("the metadata of the same packages differs")
	}
}
