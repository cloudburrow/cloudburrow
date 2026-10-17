package run

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
)

// Yum (#1539): a YUM repository is a yum and dnf repository at Google's
// {location}-yum.pkg.dev/projects/{project}/{repository}, the baseurl of
//
//	[REPOSITORY]
//	name=REPOSITORY
//	baseurl=https://LOCATION-yum.pkg.dev/projects/PROJECT/REPOSITORY
//	enabled=1
//	repo_gpgcheck=0
//	gpgcheck=0
//
// It serves the createrepo metadata of the imported packages,
// repodata/repomd.xml with primary.xml.gz, filelists.xml.gz and
// other.xml.gz and their SHA-256 checksums, sizes and timestamps, built
// from each .rpm's header, and each package at its location href. The
// metadata is made again when the repository's packages change: an
// import, or a deleted package or version. It is not signed, so the
// .repo needs repo_gpgcheck=0, and the packages are served as imported.

// yumRepodata is one repository's generated metadata.
type yumRepodata struct {
	fingerprint string
	revision    int64
	files       map[string][]byte // repodata/{name} to its content
	repomd      []byte
}

var yumCache = struct {
	mu    sync.Mutex
	repos map[string]*yumRepodata
}{repos: map[string]*yumRepodata{}}

// yumFingerprint names a repository's package set: the metadata is made
// again when it changes.
func yumFingerprint(files []fmtFile) string {
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f.ID + "\x00" + f.Digest + "\n")
	}
	return sha256Hex([]byte(b.String()))
}

// refreshYumRepodata makes a YUM repository's metadata again after its
// packages changed.
func refreshYumRepodata(repoName string) {
	if r, ok := repoFormat(repoName); !ok || r.GetFormat() != arpb.Repository_YUM {
		yumCache.mu.Lock()
		delete(yumCache.repos, repoName)
		yumCache.mu.Unlock()
		return
	}
	_ = yumRepodataOf(repoName)
}

// yumRepodataOf is a repository's metadata, made when its packages
// changed since it was last made.
func yumRepodataOf(repoName string) *yumRepodata {
	files := repoFilesOf(repoName)
	fp := yumFingerprint(files)
	yumCache.mu.Lock()
	defer yumCache.mu.Unlock()
	if md := yumCache.repos[repoName]; md != nil && md.fingerprint == fp {
		return md
	}
	var pkgs []yumPackage
	for _, f := range files {
		if !strings.HasSuffix(f.ID, ".rpm") {
			continue
		}
		p := rpmInfoOf(f)
		if p == nil {
			continue
		}
		pkgs = append(pkgs, yumPackage{info: p, file: f})
	}
	revision := time.Now().Unix()
	if prev := yumCache.repos[repoName]; prev != nil && revision <= prev.revision {
		revision = prev.revision + 1 // a new revision even within the same second
	}
	md := buildYumRepodata(pkgs, revision)
	md.fingerprint = fp
	yumCache.repos[repoName] = md
	return md
}

// rpmInfoOf is a file's header as imported, or read again from the blob
// for a file imported before the header was saved.
func rpmInfoOf(f fmtFile) *rpmInfo {
	if s := f.Meta["rpm"]; s != "" {
		var p rpmInfo
		if json.Unmarshal([]byte(s), &p) == nil {
			return &p
		}
	}
	st := RegistryStore()
	if st == nil {
		return nil
	}
	rc, err := st.OpenBlob(f.Digest)
	if err != nil {
		return nil
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxImportObject))
	if err != nil {
		return nil
	}
	p, err := parseRPM(b)
	if err != nil {
		return nil
	}
	return p
}

// yumPackage is one package of the metadata: its header and its file.
type yumPackage struct {
	info *rpmInfo
	file fmtFile
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func xmlAttr(s string) string {
	return strings.ReplaceAll(xmlText(s), `"`, "&quot;")
}

// primaryFile reports whether createrepo lists a file in primary.xml, as
// well as filelists.xml: those that file dependencies usually name.
func primaryFile(p string) bool {
	return strings.HasPrefix(p, "/etc/") || strings.Contains(p, "bin/") || p == "/usr/lib/sendmail"
}

func writeDeps(b *bytes.Buffer, element string, deps []rpmDep) {
	if len(deps) == 0 {
		return
	}
	fmt.Fprintf(b, "    <rpm:%s>\n", element)
	for _, d := range deps {
		fmt.Fprintf(b, `      <rpm:entry name="%s"`, xmlAttr(d.Name))
		if d.Flags != "" {
			fmt.Fprintf(b, ` flags="%s" epoch="%s" ver="%s"`, d.Flags, xmlAttr(d.Epoch), xmlAttr(d.Ver))
			if d.Rel != "" {
				fmt.Fprintf(b, ` rel="%s"`, xmlAttr(d.Rel))
			}
		}
		if d.Pre && element == "requires" {
			b.WriteString(` pre="1"`)
		}
		b.WriteString("/>\n")
	}
	fmt.Fprintf(b, "    </rpm:%s>\n", element)
}

func writeFile(b *bytes.Buffer, indent string, f rpmFile) {
	switch {
	case f.Dir:
		fmt.Fprintf(b, "%s<file type=\"dir\">%s</file>\n", indent, xmlText(f.Path))
	case f.Ghost:
		fmt.Fprintf(b, "%s<file type=\"ghost\">%s</file>\n", indent, xmlText(f.Path))
	default:
		fmt.Fprintf(b, "%s<file>%s</file>\n", indent, xmlText(f.Path))
	}
}

func versionElement(p *rpmInfo) string {
	return fmt.Sprintf(`<version epoch="%s" ver="%s" rel="%s"/>`, xmlAttr(p.Epoch), xmlAttr(p.Version), xmlAttr(p.Release))
}

// yumPrimary, yumFilelists and yumOther are the three metadata documents
// createrepo writes.
func yumPrimary(pkgs []yumPackage) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metadata xmlns=\"http://linux.duke.edu/metadata/common\" xmlns:rpm=\"http://linux.duke.edu/metadata/rpm\" packages=\"%d\">\n", len(pkgs))
	for _, y := range pkgs {
		p, f := y.info, y.file
		b.WriteString("<package type=\"rpm\">\n")
		fmt.Fprintf(&b, "  <name>%s</name>\n  <arch>%s</arch>\n  %s\n", xmlText(p.Name), xmlText(p.Arch), versionElement(p))
		fmt.Fprintf(&b, "  <checksum type=\"sha256\" pkgid=\"YES\">%s</checksum>\n", strings.TrimPrefix(f.Digest, "sha256:"))
		fmt.Fprintf(&b, "  <summary>%s</summary>\n  <description>%s</description>\n  <packager>%s</packager>\n  <url>%s</url>\n",
			xmlText(p.Summary), xmlText(p.Description), xmlText(p.Packager), xmlText(p.URL))
		fmt.Fprintf(&b, "  <time file=\"%d\" build=\"%d\"/>\n", f.Uploaded.Unix(), p.BuildTime)
		fmt.Fprintf(&b, "  <size package=\"%d\" installed=\"%d\" archive=\"%d\"/>\n", f.Size, p.Installed, p.Archive)
		fmt.Fprintf(&b, "  <location href=\"%s\"/>\n", xmlAttr(f.ID))
		b.WriteString("  <format>\n")
		fmt.Fprintf(&b, "    <rpm:license>%s</rpm:license>\n    <rpm:vendor>%s</rpm:vendor>\n    <rpm:group>%s</rpm:group>\n    <rpm:buildhost>%s</rpm:buildhost>\n    <rpm:sourcerpm>%s</rpm:sourcerpm>\n",
			xmlText(p.License), xmlText(p.Vendor), xmlText(p.Group), xmlText(p.BuildHost), xmlText(p.SourceRPM))
		fmt.Fprintf(&b, "    <rpm:header-range start=\"%d\" end=\"%d\"/>\n", p.HeaderStart, p.HeaderEnd)
		writeDeps(&b, "provides", p.Provides)
		writeDeps(&b, "requires", p.Requires)
		writeDeps(&b, "conflicts", p.Conflicts)
		writeDeps(&b, "obsoletes", p.Obsoletes)
		for _, file := range p.Files {
			if primaryFile(file.Path) {
				writeFile(&b, "    ", file)
			}
		}
		b.WriteString("  </format>\n</package>\n")
	}
	b.WriteString("</metadata>\n")
	return b.Bytes()
}

func yumFilelists(pkgs []yumPackage) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<filelists xmlns=\"http://linux.duke.edu/metadata/filelists\" packages=\"%d\">\n", len(pkgs))
	for _, y := range pkgs {
		p := y.info
		fmt.Fprintf(&b, "<package pkgid=\"%s\" name=\"%s\" arch=\"%s\">\n  %s\n", strings.TrimPrefix(y.file.Digest, "sha256:"), xmlAttr(p.Name), xmlAttr(p.Arch), versionElement(p))
		for _, f := range p.Files {
			writeFile(&b, "  ", f)
		}
		b.WriteString("</package>\n")
	}
	b.WriteString("</filelists>\n")
	return b.Bytes()
}

func yumOther(pkgs []yumPackage) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<otherdata xmlns=\"http://linux.duke.edu/metadata/other\" packages=\"%d\">\n", len(pkgs))
	for _, y := range pkgs {
		p := y.info
		fmt.Fprintf(&b, "<package pkgid=\"%s\" name=\"%s\" arch=\"%s\">\n  %s\n", strings.TrimPrefix(y.file.Digest, "sha256:"), xmlAttr(p.Name), xmlAttr(p.Arch), versionElement(p))
		for _, c := range p.Changelog {
			fmt.Fprintf(&b, "  <changelog author=\"%s\" date=\"%d\">%s</changelog>\n", xmlAttr(c.Author), c.Date, xmlText(c.Text))
		}
		b.WriteString("</package>\n")
	}
	b.WriteString("</otherdata>\n")
	return b.Bytes()
}

func gzipBytes(b []byte) []byte {
	var out bytes.Buffer
	zw := gzip.NewWriter(&out) // no name or time in its header: the same input compresses the same
	_, _ = zw.Write(b)
	_ = zw.Close()
	return out.Bytes()
}

// buildYumRepodata makes repomd.xml over the three documents.
func buildYumRepodata(pkgs []yumPackage, revision int64) *yumRepodata {
	md := &yumRepodata{revision: revision, files: map[string][]byte{}}
	var b bytes.Buffer
	fmt.Fprintf(&b, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<repomd xmlns=\"http://linux.duke.edu/metadata/repo\" xmlns:rpm=\"http://linux.duke.edu/metadata/rpm\">\n  <revision>%d</revision>\n", revision)
	for _, d := range []struct {
		typ  string
		body []byte
	}{{"primary", yumPrimary(pkgs)}, {"filelists", yumFilelists(pkgs)}, {"other", yumOther(pkgs)}} {
		gz := gzipBytes(d.body)
		name := d.typ + ".xml.gz"
		md.files[name] = gz
		fmt.Fprintf(&b, "  <data type=\"%s\">\n    <checksum type=\"sha256\">%s</checksum>\n    <open-checksum type=\"sha256\">%s</open-checksum>\n    <location href=\"repodata/%s\"/>\n    <timestamp>%d</timestamp>\n    <size>%d</size>\n    <open-size>%d</open-size>\n  </data>\n",
			d.typ, sha256Hex(gz), sha256Hex(d.body), name, revision, len(gz), len(d.body))
	}
	b.WriteString("</repomd>\n")
	md.repomd = b.Bytes()
	md.files["repomd.xml"] = md.repomd
	return md
}

// resolveYumRequest reads /projects/{project}/{repository}/....
func resolveYumRequest(location, prefix, p string) (*pkgRequest, string, bool) {
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 4)
	if len(parts) < 3 || parts[0] != "projects" || parts[2] == "" {
		return nil, "the yum repository is /projects/{project}/{repository}/", true
	}
	name := "projects/" + parts[1] + "/locations/" + location + "/repositories/" + parts[2]
	repo, ok := repoFormat(name)
	if !ok || repo.GetFormat() != arpb.Repository_YUM {
		return nil, fmt.Sprintf("repository %s was not found, or is not a YUM repository", name), true
	}
	pr := &pkgRequest{repo: repo, prefix: prefix + "/projects/" + parts[1] + "/" + parts[2]}
	if len(parts) == 4 {
		pr.rest = parts[3]
	}
	return pr, "", true
}

func serveYum(w http.ResponseWriter, r *http.Request, p *pkgRequest) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "a yum repository is read-only: import packages with ImportYumArtifacts", http.StatusMethodNotAllowed)
		return
	}
	repoName := p.repo.GetName()
	rest, _ := url.PathUnescape(p.rest)
	if name, ok := strings.CutPrefix(rest, "repodata/"); ok {
		md := yumRepodataOf(repoName)
		body, found := md.files[name]
		if !found {
			http.NotFound(w, r)
			return
		}
		ct := "application/gzip"
		if name == "repomd.xml" {
			ct = "application/xml"
		}
		w.Header().Set("Content-Type", ct)
		http.ServeContent(w, r, "", time.Unix(md.revision, 0), bytes.NewReader(body))
		return
	}
	if f, found := fileByID(repoName, rest); found && strings.HasSuffix(rest, ".rpm") {
		serveBlob(w, r, f, "application/x-rpm")
		return
	}
	http.NotFound(w, r)
}
