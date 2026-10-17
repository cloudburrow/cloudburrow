package run

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/emptypb"
)

// arServer is the Artifact Registry server the protocols record
// operations on.
var arServer atomic.Pointer[ArtifactRegistryServer]

// Each package format's own protocol (#1531), on the registry's port, at
// the endpoints Google documents for it:
//
//   - PYTHON: {base}/simple/ is the PEP 503 simple index pip reads with
//     --index-url, and a POST to {base}/ is the upload twine makes;
//   - NPM: {base}/ is an npm registry: `npm publish` PUTs a package
//     document, `npm install` GETs it and its tarballs, and
//     /-/package/{name}/dist-tags/ serves `npm dist-tag`;
//   - MAVEN: {base}/ is a Maven repository layout `mvn deploy` PUTs to and
//     dependency resolution GETs from, with maven-metadata.xml and
//     checksums computed for what was not uploaded;
//   - GENERIC: the API's media upload,
//     /upload/v1/{repository}/genericArtifacts:create, and every format's
//     /v1/{file}:download?alt=media.
//
// {base} is {location}-{format}.pkg.dev/{project}/{repository}: the host,
// or {location}-{format}.pkg.localhost, names it, or a client that can
// only reach this port by address puts the host first in the path,
// http://127.0.0.1:PORT/{location}-{format}.pkg.dev/{project}/{repository}/.
// A VIRTUAL repository serves the reads from its upstream repositories,
// highest priority first, and refuses writes, as Google's does.

var pkgHostRE = regexp.MustCompile(`^([a-z0-9-]+)-(python|npm|maven)\.pkg\.(dev|localhost)$`)

// pkgRequest is a protocol request resolved to its repository.
type pkgRequest struct {
	repo *arpb.Repository
	// prefix is the URL up to {base}, for absolute links back.
	prefix string
	// rest is the escaped path after {base}/.
	rest string
}

func hostOnly(h string) string {
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i:], "]") {
		return h[:i]
	}
	return h
}

// resolvePkgRequest finds the format repository a protocol request names.
func resolvePkgRequest(r *http.Request) (*pkgRequest, string, bool) {
	p := r.URL.EscapedPath()
	host := strings.ToLower(hostOnly(r.Host))
	prefix := "http://" + r.Host
	var m []string
	if m = pkgHostRE.FindStringSubmatch(host); m == nil {
		seg, rest, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
		if m = pkgHostRE.FindStringSubmatch(strings.ToLower(seg)); m == nil {
			return nil, "", false
		}
		prefix += "/" + seg
		p = "/" + rest
	}
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 3)
	if len(parts) < 2 {
		return nil, "repository path must be /{project}/{repository}/", true
	}
	name := "projects/" + parts[0] + "/locations/" + m[1] + "/repositories/" + parts[1]
	repo, ok := repoFormat(name)
	if !ok || servedFormats[repo.GetFormat()] != m[2] {
		return nil, fmt.Sprintf("repository %s was not found, or is not a %s repository", name, strings.ToUpper(m[2])), true
	}
	pr := &pkgRequest{repo: repo, prefix: prefix + "/" + parts[0] + "/" + parts[1]}
	if len(parts) == 3 {
		pr.rest = parts[2]
	}
	return pr, "", true
}

// readRepos are the repositories a read searches: the repository, or a
// virtual one's upstreams by descending priority.
func (p *pkgRequest) readRepos() []string {
	if p.repo.GetMode() != arpb.Repository_VIRTUAL_REPOSITORY {
		return []string{p.repo.GetName()}
	}
	ups := append([]*arpb.UpstreamPolicy(nil), p.repo.GetVirtualRepositoryConfig().GetUpstreamPolicies()...)
	sort.SliceStable(ups, func(i, j int) bool { return ups[i].GetPriority() > ups[j].GetPriority() })
	var out []string
	for _, u := range ups {
		out = append(out, u.GetRepository())
	}
	return out
}

func (p *pkgRequest) writable(w http.ResponseWriter) bool {
	if p.repo.GetMode() == arpb.Repository_VIRTUAL_REPOSITORY {
		http.Error(w, "a virtual repository is read-only: upload to one of its upstream repositories", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// servePackages serves a format protocol or generic upload or download
// request, and reports whether it was one.
func servePackages(w http.ResponseWriter, r *http.Request) bool {
	if handled := serveMediaAPI(w, r); handled {
		return true
	}
	pr, msg, ok := resolvePkgRequest(r)
	if !ok {
		return false
	}
	if pr == nil {
		http.Error(w, msg, http.StatusNotFound)
		return true
	}
	switch pr.repo.GetFormat() {
	case arpb.Repository_PYTHON:
		servePython(w, r, pr)
	case arpb.Repository_NPM:
		serveNpm(w, r, pr)
	case arpb.Repository_MAVEN:
		serveMaven(w, r, pr)
	}
	return true
}

func serveBlob(w http.ResponseWriter, r *http.Request, f fmtFile, contentType string) {
	st := RegistryStore()
	if st == nil {
		http.Error(w, "this instance has no registry store", http.StatusServiceUnavailable)
		return
	}
	rc, err := st.OpenBlob(f.Digest)
	if err != nil {
		http.Error(w, "file content is missing", http.StatusNotFound)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, "", f.Uploaded, rc)
}

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, maxImportObject))
}

// Python.

var pyNormRE = regexp.MustCompile(`[-_.]+`)

// normalizePython is PEP 503's normalized project name.
func normalizePython(n string) string { return strings.ToLower(pyNormRE.ReplaceAllString(n, "-")) }

func servePython(w http.ResponseWriter, r *http.Request, p *pkgRequest) {
	rest, _ := url.PathUnescape(p.rest)
	switch {
	case r.Method == http.MethodPost && (rest == "" || rest == "legacy/" || rest == "legacy"):
		if p.writable(w) {
			pythonUpload(w, r, p)
		}
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case rest == "simple" || rest == "simple/":
		names := map[string]bool{}
		for _, repo := range p.readRepos() {
			for _, f := range repoFilesOf(repo) {
				names[f.Package] = true
			}
		}
		sorted := make([]string, 0, len(names))
		for n := range names {
			sorted = append(sorted, n)
		}
		sort.Strings(sorted)
		var b strings.Builder
		b.WriteString("<!DOCTYPE html>\n<html><head><meta name=\"pypi:repository-version\" content=\"1.0\"><title>Simple index</title></head><body>\n")
		for _, n := range sorted {
			fmt.Fprintf(&b, "<a href=\"%s/simple/%s/\">%s</a><br>\n", p.prefix, html.EscapeString(n), html.EscapeString(n))
		}
		b.WriteString("</body></html>\n")
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, b.String())
	case strings.HasPrefix(rest, "simple/"):
		want := normalizePython(strings.Trim(strings.TrimPrefix(rest, "simple/"), "/"))
		if dst := strings.Trim(strings.TrimPrefix(rest, "simple/"), "/"); dst != want {
			http.Redirect(w, r, p.prefix+"/simple/"+want+"/", http.StatusMovedPermanently)
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "<!DOCTYPE html>\n<html><head><meta name=\"pypi:repository-version\" content=\"1.0\"><title>Links for %s</title></head><body>\n<h1>Links for %s</h1>\n", want, want)
		n := 0
		for _, repo := range p.readRepos() {
			for _, f := range repoFilesOf(repo) {
				if f.Package != want {
					continue
				}
				n++
				attr := ""
				if rp := f.Meta["requires_python"]; rp != "" {
					attr = ` data-requires-python="` + html.EscapeString(rp) + `"`
				}
				fmt.Fprintf(&b, "<a href=\"%s/%s/%s#sha256=%s\"%s>%s</a><br>\n", p.prefix, want, url.PathEscape(f.ID),
					strings.TrimPrefix(f.Digest, "sha256:"), attr, html.EscapeString(f.ID))
			}
		}
		if n == 0 {
			http.Error(w, "project "+want+" was not found", http.StatusNotFound)
			return
		}
		b.WriteString("</body></html>\n")
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, b.String())
	default:
		pkg, file, ok := strings.Cut(rest, "/")
		if ok {
			for _, repo := range p.readRepos() {
				if f, found := fileByID(repo, file); found && f.Package == normalizePython(pkg) {
					serveBlob(w, r, f, "application/octet-stream")
					return
				}
			}
		}
		http.NotFound(w, r)
	}
}

func pythonUpload(w http.ResponseWriter, r *http.Request, p *pkgRequest) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "the upload must be multipart/form-data: "+err.Error(), http.StatusBadRequest)
		return
	}
	if a := r.FormValue(":action"); a != "" && a != "file_upload" {
		http.Error(w, "unsupported :action "+a, http.StatusBadRequest)
		return
	}
	name, version := r.FormValue("name"), r.FormValue("version")
	fh, hdr, err := r.FormFile("content")
	if name == "" || version == "" || err != nil {
		http.Error(w, "name, version and content are required", http.StatusBadRequest)
		return
	}
	defer fh.Close()
	b, err := io.ReadAll(fh)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if d := r.FormValue("sha256_digest"); d != "" && !strings.EqualFold(d, sha256Hex(b)) {
		http.Error(w, "sha256_digest does not match the content", http.StatusBadRequest)
		return
	}
	if _, exists := fileByID(p.repo.GetName(), hdr.Filename); exists {
		http.Error(w, "file "+hdr.Filename+" already exists", http.StatusConflict)
		return
	}
	meta := map[string]string{}
	if rp := r.FormValue("requires_python"); rp != "" {
		meta["requires_python"] = rp
	}
	if _, err := addFile(p.repo.GetName(), hdr.Filename, normalizePython(name), version, b, meta); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// npm.

func npmDocs(repo, pkg string) map[string]json.RawMessage {
	fmtState.mu.Lock()
	defer fmtState.mu.Unlock()
	out := map[string]json.RawMessage{}
	if r := fmtState.repos[repo]; r != nil {
		for v, d := range r.Docs[pkg] {
			out[v] = d
		}
	}
	return out
}

func npmTarballID(pkg, version string) string {
	base := pkg
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return pkg + "/-/" + base + "-" + version + ".tgz"
}

func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func npmError(w http.ResponseWriter, status int, msg string) {
	jsonReply(w, status, map[string]string{"error": msg})
}

func serveNpm(w http.ResponseWriter, r *http.Request, p *pkgRequest) {
	rest := p.rest
	if t, ok := strings.CutPrefix(rest, "-/package/"); ok {
		npmDistTags(w, r, p, t)
		return
	}
	if strings.HasPrefix(rest, "-/") {
		npmError(w, http.StatusNotFound, "not found")
		return
	}
	escPkg, file, hasFile := strings.Cut(rest, "/-/")
	pkg, err := url.PathUnescape(escPkg)
	if err != nil || pkg == "" {
		npmError(w, http.StatusBadRequest, "invalid package name")
		return
	}
	switch {
	case hasFile && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		f, _ := url.PathUnescape(file)
		for _, repo := range p.readRepos() {
			if x, ok := fileByID(repo, pkg+"/-/"+f); ok {
				serveBlob(w, r, x, "application/octet-stream")
				return
			}
		}
		npmError(w, http.StatusNotFound, "not found")
	case hasFile:
		npmError(w, http.StatusMethodNotAllowed, "method not allowed")
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		npmPackument(w, p, pkg)
	case r.Method == http.MethodPut:
		if p.writable(w) {
			npmPublish(w, r, p, pkg)
		}
	default:
		npmError(w, http.StatusMethodNotAllowed, "method not allowed: unpublish with the API's DeleteVersion")
	}
}

func npmPackument(w http.ResponseWriter, p *pkgRequest, pkg string) {
	versions := map[string]any{}
	times := map[string]string{}
	tags := map[string]string{}
	for _, repo := range p.readRepos() {
		docs := npmDocs(repo, pkg)
		for _, v := range versionsOf(repo) {
			if v.Package != pkg {
				continue
			}
			if _, ok := versions[v.Version]; ok {
				continue
			}
			var doc map[string]any
			if json.Unmarshal(docs[v.Version], &doc) != nil || doc == nil {
				doc = map[string]any{"name": pkg, "version": v.Version}
			}
			dist, _ := doc["dist"].(map[string]any)
			if dist == nil {
				dist = map[string]any{}
			}
			id := npmTarballID(pkg, v.Version)
			dist["tarball"] = p.prefix + "/" + strings.Replace(url.PathEscape(pkg), "%2F", "/", 1) + "/-/" + id[strings.Index(id, "/-/")+3:]
			doc["dist"] = dist
			versions[v.Version] = doc
			times[v.Version] = v.First.Format(time.RFC3339)
		}
		for t, v := range tagsOf(repo, pkg) {
			if _, ok := tags[t]; !ok {
				tags[t] = v
			}
		}
	}
	if len(versions) == 0 {
		npmError(w, http.StatusNotFound, "not found")
		return
	}
	jsonReply(w, http.StatusOK, map[string]any{"_id": pkg, "name": pkg, "versions": versions, "dist-tags": tags, "time": times})
}

func npmPublish(w http.ResponseWriter, r *http.Request, p *pkgRequest, pkg string) {
	b, err := readBody(r)
	if err != nil {
		npmError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Name        string                     `json:"name"`
		Versions    map[string]json.RawMessage `json:"versions"`
		DistTags    map[string]string          `json:"dist-tags"`
		Attachments map[string]struct {
			Data string `json:"data"`
		} `json:"_attachments"`
	}
	if err := json.Unmarshal(b, &body); err != nil || body.Name != pkg {
		npmError(w, http.StatusBadRequest, "the body must be a package document named "+pkg)
		return
	}
	if len(body.Versions) == 0 || len(body.Attachments) == 0 {
		npmError(w, http.StatusBadRequest, "nothing to publish: no versions or attachments")
		return
	}
	repo := p.repo.GetName()
	for v, doc := range body.Versions {
		if _, ok := findVersion(repo, pkg, v); ok {
			npmError(w, http.StatusForbidden, "cannot publish over the previously published version "+v)
			return
		}
		id := npmTarballID(pkg, v)
		var data []byte
		for _, a := range body.Attachments {
			data, err = base64.StdEncoding.DecodeString(a.Data)
			break
		}
		if err != nil || len(data) == 0 {
			npmError(w, http.StatusBadRequest, "the attachment is not base64")
			return
		}
		if _, err := addFile(repo, id, pkg, v, data, nil); err != nil {
			npmError(w, http.StatusInternalServerError, err.Error())
			return
		}
		fmtState.mu.Lock()
		fr := fmtRepoLocked(repo)
		if fr.Docs == nil {
			fr.Docs = map[string]map[string]json.RawMessage{}
		}
		if fr.Docs[pkg] == nil {
			fr.Docs[pkg] = map[string]json.RawMessage{}
		}
		fr.Docs[pkg][v] = doc
		saveFormatsLocked()
		fmtState.mu.Unlock()
	}
	for t, v := range body.DistTags {
		setFmtTag(repo, pkg, t, v)
	}
	jsonReply(w, http.StatusOK, map[string]any{"ok": true})
}

// npmDistTags serves /-/package/{pkg}/dist-tags[/{tag}].
func npmDistTags(w http.ResponseWriter, r *http.Request, p *pkgRequest, rest string) {
	escPkg, tail, _ := strings.Cut(rest, "/dist-tags")
	pkg, err := url.PathUnescape(escPkg)
	if err != nil || pkg == "" {
		npmError(w, http.StatusBadRequest, "invalid package name")
		return
	}
	tag := strings.TrimPrefix(tail, "/")
	repo := p.repo.GetName()
	switch {
	case r.Method == http.MethodGet && tag == "":
		tags := map[string]string{}
		for _, rn := range p.readRepos() {
			for t, v := range tagsOf(rn, pkg) {
				if _, ok := tags[t]; !ok {
					tags[t] = v
				}
			}
		}
		jsonReply(w, http.StatusOK, tags)
	case (r.Method == http.MethodPut || r.Method == http.MethodPost) && tag != "":
		if !p.writable(w) {
			return
		}
		b, _ := readBody(r)
		var v string
		if json.Unmarshal(b, &v) != nil {
			npmError(w, http.StatusBadRequest, "the body must be a JSON string, the version")
			return
		}
		if _, ok := findVersion(repo, pkg, v); !ok {
			npmError(w, http.StatusNotFound, "version "+v+" was not found")
			return
		}
		setFmtTag(repo, pkg, tag, v)
		jsonReply(w, http.StatusCreated, map[string]any{"ok": true})
	case r.Method == http.MethodDelete && tag != "":
		if !p.writable(w) {
			return
		}
		setFmtTag(repo, pkg, tag, "")
		jsonReply(w, http.StatusOK, map[string]any{"ok": true})
	default:
		npmError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// Maven.

var mavenChecksums = map[string]func() hash.Hash{".sha1": sha1.New, ".md5": md5.New, ".sha256": sha256.New, ".sha512": sha512.New}

// mavenCoordinates are the package and version a Maven repository path
// holds: {group path}/{artifactId}/{version}/{artifactId}-{version}...;
// ok is false for metadata, checksums and anything else.
func mavenCoordinates(p string) (pkg, version string, ok bool) {
	segs := strings.Split(p, "/")
	n := len(segs)
	if n < 4 {
		return "", "", false
	}
	file, ver, art := segs[n-1], segs[n-2], segs[n-3]
	if strings.HasPrefix(file, "maven-metadata") {
		return "", "", false
	}
	for ext := range mavenChecksums {
		if strings.HasSuffix(file, ext) {
			return "", "", false
		}
	}
	stem := art + "-" + strings.TrimSuffix(ver, "-SNAPSHOT")
	if !strings.HasPrefix(file, stem) {
		return "", "", false
	}
	return strings.Join(segs[:n-3], ".") + ":" + art, ver, true
}

func serveMaven(w http.ResponseWriter, r *http.Request, p *pkgRequest) {
	path, err := url.PathUnescape(p.rest)
	if err != nil || path == "" || strings.Contains(path, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		if !p.writable(w) {
			return
		}
		b, err := readBody(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pkg, ver, ok := mavenCoordinates(path)
		if ok && !strings.HasSuffix(ver, "-SNAPSHOT") {
			if _, exists := fileByID(p.repo.GetName(), path); exists {
				http.Error(w, "the release "+pkg+":"+ver+" already holds "+path+": release versions cannot be overwritten", http.StatusConflict)
				return
			}
		}
		if _, err := addFile(p.repo.GetName(), path, pkg, ver, b, nil); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet, http.MethodHead:
		for _, repo := range p.readRepos() {
			if f, ok := fileByID(repo, path); ok {
				serveBlob(w, r, f, mavenType(path))
				return
			}
		}
		for ext, h := range mavenChecksums {
			if base, ok := strings.CutSuffix(path, ext); ok {
				if b, found := mavenBytes(p, base); found {
					x := h()
					x.Write(b)
					_, _ = io.WriteString(w, hex.EncodeToString(x.Sum(nil)))
					return
				}
			}
		}
		if b, found := mavenBytes(p, path); found {
			w.Header().Set("Content-Type", mavenType(path))
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func mavenType(p string) string {
	switch {
	case strings.HasSuffix(p, ".pom"), strings.HasSuffix(p, ".xml"):
		return "text/xml"
	case strings.HasSuffix(p, ".jar"):
		return "application/java-archive"
	}
	return "application/octet-stream"
}

// mavenBytes is a stored file's content, or a maven-metadata.xml made
// from the artifact's versions when none was uploaded.
func mavenBytes(p *pkgRequest, path string) ([]byte, bool) {
	for _, repo := range p.readRepos() {
		if f, ok := fileByID(repo, path); ok {
			if st := RegistryStore(); st != nil {
				if b, err := st.ReadBlob(f.Digest); err == nil {
					return b, true
				}
			}
		}
	}
	dir, ok := strings.CutSuffix(path, "/maven-metadata.xml")
	segs := strings.Split(dir, "/")
	if !ok || len(segs) < 2 {
		return nil, false
	}
	pkg := strings.Join(segs[:len(segs)-1], ".") + ":" + segs[len(segs)-1]
	var vs []*pkgVersion
	for _, repo := range p.readRepos() {
		for _, v := range versionsOf(repo) {
			if v.Package == pkg {
				vs = append(vs, v)
			}
		}
	}
	if len(vs) == 0 {
		return nil, false
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].First.Before(vs[j].First) })
	group, art, _ := strings.Cut(pkg, ":")
	var b bytes.Buffer
	fmt.Fprintf(&b, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metadata>\n  <groupId>%s</groupId>\n  <artifactId>%s</artifactId>\n  <versioning>\n", group, art)
	latest, release := vs[len(vs)-1].Version, ""
	for _, v := range vs {
		if !strings.HasSuffix(v.Version, "-SNAPSHOT") {
			release = v.Version
		}
	}
	fmt.Fprintf(&b, "    <latest>%s</latest>\n", latest)
	if release != "" {
		fmt.Fprintf(&b, "    <release>%s</release>\n", release)
	}
	b.WriteString("    <versions>\n")
	for _, v := range vs {
		fmt.Fprintf(&b, "      <version>%s</version>\n", html.EscapeString(v.Version))
	}
	fmt.Fprintf(&b, "    </versions>\n    <lastUpdated>%s</lastUpdated>\n  </versioning>\n</metadata>\n", vs[len(vs)-1].Last.UTC().Format("20060102150405"))
	return b.Bytes(), true
}

// Generic upload, and every format's download.

var (
	genericUploadRE = regexp.MustCompile(`^/upload/v1/(projects/[^/]+/locations/[^/]+/repositories/[^/]+)/genericArtifacts:create$`)
	downloadRE      = regexp.MustCompile(`^(?:/download)?/v1/(projects/[^/]+/locations/[^/]+/repositories/[^/]+/files/[^/]+):download$`)
	genericIDRE     = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$`)
)

func apiError(w http.ResponseWriter, status int, code, msg string) {
	jsonReply(w, status, map[string]any{"error": map[string]any{"code": status, "message": msg, "status": code}})
}

func serveMediaAPI(w http.ResponseWriter, r *http.Request) bool {
	if m := genericUploadRE.FindStringSubmatch(r.URL.Path); m != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
		genericUpload(w, r, m[1])
		return true
	}
	if m := downloadRE.FindStringSubmatch(r.URL.EscapedPath()); m != nil && r.Method == http.MethodGet {
		downloadFile(w, r, m[1])
		return true
	}
	return false
}

// uploadSessions are generic uploads begun with uploadType=resumable,
// by upload ID, until their content arrives.
var uploadSessions = struct {
	sync.Mutex
	m map[string]*uploadSession
}{m: map[string]*uploadSession{}}

type uploadSession struct {
	meta genericMeta
	buf  bytes.Buffer
}

type genericMeta struct {
	PackageID string `json:"packageId"`
	VersionID string `json:"versionId"`
	Filename  string `json:"filename"`
}

// genericUpload is UploadGenericArtifact's media upload, as
// uploadType=multipart (the JSON metadata, then the file) or
// uploadType=resumable (the metadata, then the file PUT to the session
// URL the first response's Location names), which gcloud's uploads use.
func genericUpload(w http.ResponseWriter, r *http.Request, repoName string) {
	repo, ok := repoFormat(repoName)
	if !ok {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "Requested entity was not found.")
		return
	}
	if repo.GetFormat() != arpb.Repository_GENERIC {
		apiError(w, http.StatusBadRequest, "FAILED_PRECONDITION", repoName+" is not a GENERIC repository")
		return
	}
	q := r.URL.Query()
	if id := q.Get("upload_id"); id != "" {
		uploadSessions.Lock()
		sess := uploadSessions.m[id]
		uploadSessions.Unlock()
		if sess == nil {
			apiError(w, http.StatusNotFound, "NOT_FOUND", "upload session "+id+" was not found")
			return
		}
		if _, err := io.Copy(&sess.buf, io.LimitReader(r.Body, maxImportObject)); err != nil {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
			return
		}
		// Content-Range: bytes S-E/T, or bytes */T for a status query.
		if cr := r.Header.Get("Content-Range"); cr != "" {
			var start, end, total int64
			if n, _ := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total); n == 3 && end+1 < total {
				w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", sess.buf.Len()-1))
				w.WriteHeader(http.StatusPermanentRedirect)
				return
			}
		}
		uploadSessions.Lock()
		delete(uploadSessions.m, id)
		uploadSessions.Unlock()
		finishGenericUpload(w, repoName, sess.meta, sess.buf.Bytes())
		return
	}
	switch q.Get("uploadType") {
	case "resumable":
		var meta genericMeta
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&meta); err != nil {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "the body must be the JSON metadata: packageId, versionId and filename")
			return
		}
		id := uuid.NewString()
		uploadSessions.Lock()
		uploadSessions.m[id] = &uploadSession{meta: meta}
		uploadSessions.Unlock()
		u := *r.URL
		q.Set("upload_id", id)
		u.RawQuery = q.Encode()
		w.Header().Set("Location", "http://"+r.Host+u.RequestURI())
		w.WriteHeader(http.StatusOK)
		return
	}
	boundary, ok := multipartBoundary(r.Header.Get("Content-Type"))
	if !ok {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "the upload must be uploadType=multipart (metadata, then the file) or uploadType=resumable")
		return
	}
	mr := multipart.NewReader(r.Body, boundary)
	var meta genericMeta
	part, err := mr.NextPart()
	if err != nil || json.NewDecoder(part).Decode(&meta) != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "the first part must be the JSON metadata")
		return
	}
	part, err = mr.NextPart()
	if err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "the second part must be the file")
		return
	}
	b, err := io.ReadAll(io.LimitReader(part, maxImportObject))
	if err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	finishGenericUpload(w, repoName, meta, b)
}

func finishGenericUpload(w http.ResponseWriter, repoName string, meta genericMeta, b []byte) {
	if !genericIDRE.MatchString(meta.PackageID) || meta.VersionID == "" || meta.Filename == "" || strings.ContainsAny(meta.Filename, "/:") {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "packageId, versionId and filename are required; packageId is lower case letters, digits, '.', '_' and '-'")
		return
	}
	id := meta.PackageID + ":" + meta.VersionID + ":" + meta.Filename
	if _, exists := fileByID(repoName, id); exists {
		apiError(w, http.StatusConflict, "ALREADY_EXISTS", "the file "+meta.Filename+" already exists in "+meta.PackageID+" "+meta.VersionID)
		return
	}
	if _, err := addFile(repoName, id, meta.PackageID, meta.VersionID, b, nil); err != nil {
		apiError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	// The operation is stored, done, so a client that waits on it (gcloud
	// does) reads it back through google.longrunning.Operations.
	opName := repoName[:strings.Index(repoName, "/repositories/")] + "/operations/" + uuid.NewString()
	if a := arServer.Load(); a != nil && a.s != nil {
		op := a.s.ops.Create(repoName[:strings.Index(repoName, "/repositories/")], repoName)
		_ = a.s.ops.SetMetadata(op.Name, &arpb.OperationMetadata{})
		_ = a.s.ops.Succeed(op.Name, &emptypb.Empty{})
		opName = op.Name
	}
	jsonReply(w, http.StatusOK, map[string]any{"operation": map[string]any{"name": opName, "done": true,
		"response": map[string]any{"@type": "type.googleapis.com/google.protobuf.Empty"}}})
}

func downloadFile(w http.ResponseWriter, r *http.Request, name string) {
	repoName := repoNameOf(name)
	repo, ok := repoFormat(repoName)
	if !ok {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "Requested entity was not found.")
		return
	}
	if repo.GetFormat() == arpb.Repository_DOCKER {
		_, id, _ := splitChild(name, "files")
		st := RegistryStore()
		if st == nil {
			apiError(w, http.StatusNotFound, "NOT_FOUND", "Requested entity was not found.")
			return
		}
		rc, err := st.OpenBlob(id)
		if err != nil {
			apiError(w, http.StatusNotFound, "NOT_FOUND", "Requested entity was not found.")
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, rc)
		return
	}
	_, id, err := fmtFileID(name)
	if err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	f, ok := fileByID(repoName, id)
	if !ok {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "Requested entity was not found.")
		return
	}
	serveBlob(w, r, f, "application/octet-stream")
}

// multipartBoundary is a multipart Content-Type's boundary. gcloud's
// apitools quotes it with single quotes, which mime.ParseMediaType
// refuses, so a refused one is read by hand.
func multipartBoundary(ct string) (string, bool) {
	if mt, params, err := mime.ParseMediaType(ct); err == nil {
		return params["boundary"], strings.HasPrefix(mt, "multipart/") && params["boundary"] != ""
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "multipart/") {
		return "", false
	}
	_, b, ok := strings.Cut(ct, "boundary=")
	if !ok {
		return "", false
	}
	if i := strings.Index(b, ";"); i >= 0 {
		b = b[:i]
	}
	b = strings.Trim(strings.TrimSpace(b), `'"`)
	return b, b != ""
}
