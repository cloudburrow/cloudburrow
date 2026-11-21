package osvscan

// Language package licenses, read from the image's own files (#2072).
//
// OSV-Scanner's --licenses reads an OS package's license from the package
// database, but a language package's (Go, npm, Maven, PyPI) only from
// deps.dev, which --offline never calls (and an instance never reaches
// the network). Artifact Analysis shows them as SPDX expressions in each
// PACKAGE occurrence's license, so they are read here from what the
// image itself holds:
//   - npm: each node_modules/<name>/package.json's license (or the
//     legacy licenses list), and the hidden lockfile's license fields;
//   - PyPI: each *.dist-info/METADATA (or *.egg-info/PKG-INFO)'s
//     License-Expression, else its License, else its License ::
//     classifiers;
//   - Maven: the <licenses> of each pom.xml in a jar's META-INF/maven/,
//     and of each .pom file (a local Maven repository), following the
//     <parent> chain when a POM names none;
//   - Go: the LICENSE files of each module in a module cache
//     (pkg/mod/<module>@<version>/) or a vendor directory, classified by
//     Google's license classifier, github.com/google/licensecheck.

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/google/licensecheck"
	"github.com/klauspost/compress/zstd"
)

const (
	// maxMetadataFile is the largest package.json, METADATA, POM or
	// LICENSE file read.
	maxMetadataFile = 4 << 20
	// maxArchiveFile is the largest jar read for its POMs.
	maxArchiveFile = 256 << 20
)

// localLicenses are the licenses found in an image, by package.
type localLicenses struct {
	npm  map[string]string     // name@version
	pypi map[string]string     // normalised name@version
	pyAt map[string]string     // METADATA path
	poms map[string]*pomRecord // group:artifact:version
	// goMod are module cache LICENSE texts by module@version, then file
	// name; goVendor by module path.
	goMod    map[string]map[string][]byte
	goVendor map[string]map[string][]byte
}

type pomRecord struct {
	parent   string // group:artifact:version
	licenses []pomLicense
}

type pomLicense struct {
	Name string `xml:"name"`
	URL  string `xml:"url"`
}

// AddLocalLicenses sets the licenses of r's language packages that the
// scanner left without one from the image archive's own files.
func AddLocalLicenses(r *Result, archive string) error {
	need := false
	for _, res := range r.Results {
		for _, p := range res.Packages {
			if licenseOf(p.Licenses) == "" && languageEcosystem(p.Package.Ecosystem) {
				need = true
			}
		}
	}
	if !need {
		return nil
	}
	ll, err := readLocalLicenses(archive)
	if err != nil {
		return err
	}
	for i := range r.Results {
		res := &r.Results[i]
		for j := range res.Packages {
			p := &res.Packages[j]
			if licenseOf(p.Licenses) != "" {
				continue
			}
			if l := ll.lookup(p, res.Source.Path); l != "" {
				p.Licenses = []string{l}
			}
		}
	}
	return nil
}

func languageEcosystem(e string) bool {
	switch e {
	case "npm", "PyPI", "Maven", "Go":
		return true
	}
	return false
}

func (ll *localLicenses) lookup(p *Package, source string) string {
	name, ver := p.Package.Name, p.Package.Version
	switch p.Package.Ecosystem {
	case "npm":
		return ll.npm[name+"@"+ver]
	case "PyPI":
		if l, ok := ll.pyAt[strings.TrimPrefix(source, "/")]; ok {
			return l
		}
		return ll.pypi[pyName(name)+"@"+ver]
	case "Maven":
		return ll.mavenLicense(name + ":" + ver)
	case "Go":
		if name == "stdlib" || ver == "" || ver == "(devel)" {
			return ""
		}
		if f := ll.goMod[name+"@v"+strings.TrimPrefix(ver, "v")]; f != nil {
			return classifyLicenseFiles(f)
		}
		if f := ll.goVendor[name]; f != nil {
			return classifyLicenseFiles(f)
		}
	}
	return ""
}

// mavenLicense is a POM's licenses, or its nearest parent's that names
// some, as Maven inherits them.
func (ll *localLicenses) mavenLicense(gav string) string {
	for range 10 {
		p := ll.poms[gav]
		if p == nil {
			return ""
		}
		if len(p.licenses) > 0 {
			var ids []string
			for _, l := range p.licenses {
				if id := mavenLicenseID(l); id != "" {
					ids = append(ids, id)
				}
			}
			// Maven: "If multiple licenses are listed, it is assumed
			// that the user can select any of them".
			return joinExpr(ids, "OR")
		}
		gav = p.parent
	}
	return ""
}

// readLocalLicenses reads a `docker save` archive's layers, bottom first,
// keeping the package metadata the top of the image still holds.
func readLocalLicenses(archive string) (*localLicenses, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var man []struct {
		Layers []string `json:"Layers"`
	}
	if err := eachTarEntry(f, func(h *tar.Header, r io.Reader) error {
		if h.Name == "manifest.json" {
			return json.NewDecoder(io.LimitReader(r, maxMetadataFile)).Decode(&man)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if len(man) == 0 {
		return nil, errors.New("the image archive has no manifest.json")
	}
	idx := map[string]int{}
	for i, l := range man[0].Layers {
		idx[l] = i
	}
	files := map[string]fileFound{}
	for i := range man[0].Layers {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		err := eachTarEntry(f, func(h *tar.Header, r io.Reader) error {
			if j, ok := idx[h.Name]; !ok || j != i {
				return nil
			}
			return readLayer(r, files)
		})
		if err != nil {
			return nil, err
		}
	}
	ll := &localLicenses{npm: map[string]string{}, pypi: map[string]string{}, pyAt: map[string]string{},
		poms: map[string]*pomRecord{}, goMod: map[string]map[string][]byte{}, goVendor: map[string]map[string][]byte{}}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		ll.add(p, files[p])
	}
	return ll, nil
}

func eachTarEntry(r io.Reader, fn func(*tar.Header, io.Reader) error) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if err := fn(h, tr); err != nil {
			return err
		}
	}
}

// fileFound is a metadata file of the image: its kind and contents.
type fileFound struct {
	kind string
	data []byte
}

// readLayer applies one layer to files: its whiteouts remove what the
// layers below added, and its metadata files are kept.
func readLayer(r io.Reader, files map[string]fileFound) error {
	br := bufio.NewReader(r)
	magic, _ := br.Peek(4)
	var lr io.Reader = br
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer gz.Close()
		lr = gz
	case len(magic) == 4 && bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		zr, err := zstd.NewReader(br)
		if err != nil {
			return err
		}
		defer zr.Close()
		lr = zr
	}
	tr := tar.NewReader(lr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(strings.TrimPrefix(h.Name, "./"), "/"))
		dir, base := path.Split(name)
		if base == ".wh..wh..opq" {
			removeUnder(files, strings.TrimSuffix(dir, "/"))
			continue
		}
		if strings.HasPrefix(base, ".wh.") {
			gone := dir + strings.TrimPrefix(base, ".wh.")
			delete(files, gone)
			removeUnder(files, gone)
			continue
		}
		if h.Typeflag != tar.TypeReg {
			delete(files, name)
			continue
		}
		kind := metadataKind(name)
		if kind == "" {
			delete(files, name)
			continue
		}
		limit := int64(maxMetadataFile)
		if kind == "jar" {
			limit = maxArchiveFile
		}
		if h.Size > limit {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, limit))
		if err != nil {
			return err
		}
		files[name] = fileFound{kind: kind, data: b}
	}
}

func removeUnder(files map[string]fileFound, dir string) {
	prefix := dir + "/"
	if dir == "" || dir == "." {
		prefix = ""
	}
	for p := range files {
		if strings.HasPrefix(p, prefix) {
			delete(files, p)
		}
	}
}

// goLicenseFiles are the file names pkg.go.dev reads as a module's
// license files.
var goLicenseFiles = map[string]bool{}

func init() {
	for _, n := range []string{"COPYING", "COPYING.md", "COPYING.markdown", "COPYING.txt",
		"LICENSE", "LICENSE.md", "LICENSE.markdown", "LICENSE.txt", "LICENSE-2.0.txt",
		"LICENSE-APACHE", "LICENSE-APACHE-2.0.txt", "LICENSE-MIT", "LICENSE.MIT",
		"LICENSE.code", "LICENSE.docs", "LICENSE.rst", "MIT-LICENSE", "MIT-LICENSE.md",
		"MIT-LICENSE.markdown", "MIT-LICENSE.txt", "MIT_LICENSE", "UNLICENSE"} {
		goLicenseFiles[n] = true
		goLicenseFiles[strings.Replace(n, "LICENSE", "LICENCE", 1)] = true
	}
}

// metadataKind is the kind of package metadata a path holds, or "".
func metadataKind(name string) string {
	dir, base := path.Split(name)
	dir = strings.TrimSuffix(dir, "/")
	switch {
	case base == "package.json" && isNodeModule(dir):
		return "npm"
	case (base == ".package-lock.json" && path.Base(dir) == "node_modules") || base == "package-lock.json":
		return "npm-lock"
	case base == "METADATA" && strings.HasSuffix(dir, ".dist-info"),
		base == "PKG-INFO" && strings.HasSuffix(dir, ".egg-info"),
		strings.HasSuffix(name, ".egg-info") && !strings.Contains(base, "/"):
		return "pypi"
	case strings.HasSuffix(base, ".jar") || strings.HasSuffix(base, ".war") || strings.HasSuffix(base, ".ear"):
		return "jar"
	case strings.HasSuffix(base, ".pom") || (base == "pom.xml" && strings.Contains(dir, "META-INF/maven/")):
		return "pom"
	case goLicenseFiles[base] && (strings.Contains(dir, "pkg/mod/") || strings.Contains(dir, "vendor/")):
		return "go"
	}
	return ""
}

func isNodeModule(dir string) bool {
	parent := path.Dir(dir)
	if path.Base(parent) == "node_modules" {
		return true
	}
	return strings.HasPrefix(path.Base(parent), "@") && path.Base(path.Dir(parent)) == "node_modules"
}

func (ll *localLicenses) add(name string, f fileFound) {
	switch f.kind {
	case "npm":
		var pj struct {
			Name     string          `json:"name"`
			Version  string          `json:"version"`
			License  json.RawMessage `json:"license"`
			Licenses json.RawMessage `json:"licenses"`
		}
		if json.Unmarshal(f.data, &pj) != nil || pj.Name == "" {
			return
		}
		if l := npmLicense(pj.License, pj.Licenses); l != "" {
			ll.npm[pj.Name+"@"+pj.Version] = l
		}
	case "npm-lock":
		var lock struct {
			Packages map[string]struct {
				Name    string          `json:"name"`
				Version string          `json:"version"`
				License json.RawMessage `json:"license"`
			} `json:"packages"`
		}
		if json.Unmarshal(f.data, &lock) != nil {
			return
		}
		for p, e := range lock.Packages {
			i := strings.LastIndex(p, "node_modules/")
			if i < 0 {
				continue
			}
			n := e.Name
			if n == "" {
				n = p[i+len("node_modules/"):]
			}
			key := n + "@" + e.Version
			if _, ok := ll.npm[key]; ok {
				continue
			}
			if l := npmLicense(e.License, nil); l != "" {
				ll.npm[key] = l
			}
		}
	case "pypi":
		n, v, l := pypiLicense(f.data)
		if l == "" {
			return
		}
		ll.pyAt[name] = l
		if n != "" {
			ll.pypi[pyName(n)+"@"+v] = l
		}
	case "jar":
		zr, err := zip.NewReader(bytes.NewReader(f.data), int64(len(f.data)))
		if err != nil {
			return
		}
		for _, zf := range zr.File {
			if !strings.HasPrefix(zf.Name, "META-INF/maven/") || path.Base(zf.Name) != "pom.xml" || zf.UncompressedSize64 > maxMetadataFile {
				continue
			}
			rc, err := zf.Open()
			if err != nil {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(rc, maxMetadataFile))
			rc.Close()
			if err == nil {
				ll.addPOM(b)
			}
		}
	case "pom":
		ll.addPOM(f.data)
	case "go":
		dir, base := path.Split(name)
		dir = strings.TrimSuffix(dir, "/")
		if i := strings.LastIndex(dir, "pkg/mod/"); i >= 0 {
			mv := dir[i+len("pkg/mod/"):]
			at := strings.LastIndex(mv, "@")
			if at < 0 || strings.Contains(mv[at:], "/") || strings.HasPrefix(mv, "cache/") {
				return
			}
			mod, ok := unescapeModulePath(mv[:at])
			if !ok {
				return
			}
			key := mod + "@" + mv[at+1:]
			if ll.goMod[key] == nil {
				ll.goMod[key] = map[string][]byte{}
			}
			ll.goMod[key][base] = f.data
			return
		}
		if i := strings.LastIndex(dir, "vendor/"); i >= 0 {
			mod := dir[i+len("vendor/"):]
			if !strings.Contains(mod, ".") {
				return
			}
			if ll.goVendor[mod] == nil {
				ll.goVendor[mod] = map[string][]byte{}
			}
			ll.goVendor[mod][base] = f.data
		}
	}
}

func (ll *localLicenses) addPOM(b []byte) {
	var p struct {
		GroupID    string `xml:"groupId"`
		ArtifactID string `xml:"artifactId"`
		Version    string `xml:"version"`
		Parent     struct {
			GroupID    string `xml:"groupId"`
			ArtifactID string `xml:"artifactId"`
			Version    string `xml:"version"`
		} `xml:"parent"`
		Licenses []pomLicense `xml:"licenses>license"`
	}
	if xml.Unmarshal(b, &p) != nil || p.ArtifactID == "" {
		return
	}
	g, v := p.GroupID, p.Version
	if g == "" {
		g = p.Parent.GroupID
	}
	if v == "" {
		v = p.Parent.Version
	}
	rec := &pomRecord{licenses: p.Licenses}
	if p.Parent.ArtifactID != "" {
		rec.parent = p.Parent.GroupID + ":" + p.Parent.ArtifactID + ":" + p.Parent.Version
	}
	key := g + ":" + p.ArtifactID + ":" + v
	if old := ll.poms[key]; old == nil || len(old.licenses) == 0 {
		ll.poms[key] = rec
	}
}

// unescapeModulePath undoes the module cache's case encoding (!x is X).
func unescapeModulePath(s string) (string, bool) {
	var b strings.Builder
	bang := false
	for _, r := range s {
		switch {
		case bang:
			if r < 'a' || r > 'z' {
				return "", false
			}
			b.WriteRune(r - 'a' + 'A')
			bang = false
		case r == '!':
			bang = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), !bang
}

var pyNameSep = regexp.MustCompile(`[-_.]+`)

// pyName is a PyPI project name normalised (PEP 503).
func pyName(n string) string {
	return pyNameSep.ReplaceAllString(strings.ToLower(n), "-")
}

// npmLicense reads package.json's license (an SPDX expression, or the
// deprecated {"type": ...} object) or its deprecated licenses list,
// which npm reads as a choice of any of them.
func npmLicense(license, licenses json.RawMessage) string {
	one := func(raw json.RawMessage) string {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return licenseFromText(s)
		}
		var o struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &o) == nil {
			return licenseFromText(o.Type)
		}
		return ""
	}
	if len(license) > 0 {
		if l := one(license); l != "" {
			return l
		}
	}
	var list []json.RawMessage
	if len(licenses) > 0 && json.Unmarshal(licenses, &list) == nil {
		var ids []string
		for _, e := range list {
			if l := one(e); l != "" {
				ids = append(ids, l)
			}
		}
		return joinExpr(ids, "OR")
	}
	return ""
}

// pypiLicense reads a core metadata file (an email-header block): its
// name, version and license, from License-Expression (PEP 639), else
// License, else its License :: classifiers.
func pypiLicense(b []byte) (name, version, license string) {
	var expr, lic string
	var classifiers []string
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	key := ""
	for _, line := range lines {
		if line == "" {
			break
		}
		if (line[0] == ' ' || line[0] == '\t') && key == "license" {
			lic += "\n" + strings.TrimSpace(line)
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			key = ""
			continue
		}
		key = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch key {
		case "name":
			name = v
		case "version":
			version = v
		case "license-expression":
			expr = v
		case "license":
			lic = v
		case "classifier":
			if strings.HasPrefix(v, "License ::") {
				classifiers = append(classifiers, v)
			}
		}
	}
	if l := canonicalExpression(expr); l != "" {
		return name, version, l
	}
	if l := licenseFromText(lic); l != "" {
		return name, version, l
	}
	var ids []string
	for _, c := range classifiers {
		if id := pypiClassifiers[c]; id != "" {
			ids = append(ids, id)
		}
	}
	return name, version, joinExpr(ids, "OR")
}

// pypiClassifiers are the trove license classifiers that name one SPDX
// license (https://pypi.org/classifiers/); the others, such as "BSD
// License" or "GNU General Public License (GPL)", name a family.
var pypiClassifiers = map[string]string{
	"License :: OSI Approved :: MIT License":                                             "MIT",
	"License :: OSI Approved :: MIT No Attribution License (MIT-0)":                      "MIT-0",
	"License :: OSI Approved :: Apache Software License":                                 "Apache-2.0",
	"License :: OSI Approved :: ISC License (ISCL)":                                      "ISC",
	"License :: OSI Approved :: Mozilla Public License 1.1 (MPL 1.1)":                    "MPL-1.1",
	"License :: OSI Approved :: Mozilla Public License 2.0 (MPL 2.0)":                    "MPL-2.0",
	"License :: OSI Approved :: Python Software Foundation License":                      "PSF-2.0",
	"License :: OSI Approved :: The Unlicense (Unlicense)":                               "Unlicense",
	"License :: OSI Approved :: zlib/libpng License":                                     "Zlib",
	"License :: OSI Approved :: Boost Software License 1.0 (BSL-1.0)":                    "BSL-1.0",
	"License :: OSI Approved :: Eclipse Public License 1.0 (EPL-1.0)":                    "EPL-1.0",
	"License :: OSI Approved :: Eclipse Public License 2.0 (EPL-2.0)":                    "EPL-2.0",
	"License :: OSI Approved :: Universal Permissive License (UPL)":                      "UPL-1.0",
	"License :: OSI Approved :: European Union Public Licence 1.2 (EUPL 1.2)":            "EUPL-1.2",
	"License :: OSI Approved :: Artistic License":                                        "Artistic-2.0",
	"License :: OSI Approved :: GNU General Public License v2 or later (GPLv2+)":         "GPL-2.0-or-later",
	"License :: OSI Approved :: GNU General Public License v3 or later (GPLv3+)":         "GPL-3.0-or-later",
	"License :: OSI Approved :: GNU Lesser General Public License v2 or later (LGPLv2+)": "LGPL-2.0-or-later",
	"License :: OSI Approved :: GNU Lesser General Public License v3 or later (LGPLv3+)": "LGPL-3.0-or-later",
	"License :: OSI Approved :: GNU Affero General Public License v3 or later (AGPLv3+)": "AGPL-3.0-or-later",
	"License :: CC0 1.0 Universal (CC0 1.0) Public Domain Dedication":                    "CC0-1.0",
}

// mavenLicenseID is one POM <license> as an SPDX expression: its name
// when that is one, else the license its URL names, else its name's
// license.
func mavenLicenseID(l pomLicense) string {
	if e := canonicalExpression(l.Name); e != "" {
		return e
	}
	if u := strings.TrimSpace(l.URL); u != "" {
		if id := classifyText([]byte(u)); id != "" {
			return id
		}
	}
	return licenseFromText(l.Name)
}

// licenseFromText is a license field's SPDX expression: the field when it
// is one, else the license a common name names, else the license its text
// is (a field holding a whole license).
func licenseFromText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "UNLICENSED") || strings.EqualFold(s, "UNKNOWN") {
		return ""
	}
	if e := canonicalExpression(s); e != "" {
		return e
	}
	if id := licenseNames[normalLicenseName(s)]; id != "" {
		return id
	}
	if len(s) > 200 {
		return classifyText([]byte(s))
	}
	return ""
}

var nameNoise = regexp.MustCompile(`[^a-z0-9.+]+`)

// normalLicenseName lowers a license's name and drops the words names
// vary in ("the", "license", "version", punctuation).
func normalLicenseName(s string) string {
	s = nameNoise.ReplaceAllString(strings.ToLower(s), " ")
	var out []string
	for _, w := range strings.Fields(s) {
		switch w {
		case "the", "license", "licence", "licensed", "version", "v", "software", "public":
			continue
		}
		out = append(out, strings.TrimPrefix(w, "v"))
	}
	return strings.Join(out, " ")
}

// licenseNames are common license names, normalised, and the SPDX
// license each names.
var licenseNames = map[string]string{}

func init() {
	for id, names := range map[string][]string{
		"Apache-2.0":   {"Apache 2.0", "Apache 2", "Apache-2", "Apache2", "Apache License, Version 2.0", "The Apache Software License, Version 2.0", "Apache Software License 2.0", "ASL 2.0", "ASF 2.0"},
		"MIT":          {"MIT License", "The MIT License", "MIT/X11", "Expat"},
		"BSD-2-Clause": {"BSD 2-Clause", "Simplified BSD License", "FreeBSD License", "The BSD 2-Clause License"},
		"BSD-3-Clause": {"BSD 3-Clause", "New BSD License", "Modified BSD License", "Revised BSD License", "The BSD 3-Clause License", "3-Clause BSD License"},
		"ISC":          {"ISC License"},
		"MPL-2.0":      {"Mozilla Public License 2.0", "Mozilla Public License, Version 2.0", "MPL 2.0"},
		"EPL-1.0":      {"Eclipse Public License 1.0", "Eclipse Public License - v 1.0", "EPL 1.0"},
		"EPL-2.0":      {"Eclipse Public License 2.0", "Eclipse Public License - v 2.0", "EPL 2.0"},
		"CDDL-1.0":     {"CDDL 1.0", "Common Development and Distribution License 1.0", "COMMON DEVELOPMENT AND DISTRIBUTION LICENSE (CDDL) Version 1.0"},
		"CC0-1.0":      {"CC0 1.0 Universal", "CC0"},
		"Unlicense":    {"The Unlicense"},
		"BSL-1.0":      {"Boost Software License 1.0", "Boost Software License, Version 1.0"},
		"PSF-2.0":      {"Python Software Foundation License", "PSF License"},
		"Zlib":         {"zlib/libpng License", "zlib License"},
		"0BSD":         {"Zero-Clause BSD", "BSD Zero Clause License"},
	} {
		for _, n := range names {
			licenseNames[normalLicenseName(n)] = id
		}
	}
}

// joinExpr joins SPDX expressions with op, each compound one in
// parentheses, dropping repeats.
func joinExpr(ids []string, op string) string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) > 1 {
		for i, l := range out {
			if strings.Contains(l, " ") && !(strings.HasPrefix(l, "(") && strings.HasSuffix(l, ")")) {
				out[i] = "(" + l + ")"
			}
		}
	}
	return strings.Join(out, " "+op+" ")
}

var (
	spdxOnce sync.Once
	spdxIDs  map[string]string // lower case -> SPDX ID
)

// spdxID is an SPDX license ID in its canonical case, from the IDs
// licensecheck knows (the SPDX license list).
func spdxID(s string) (string, bool) {
	spdxOnce.Do(func() {
		spdxIDs = map[string]string{}
		for _, l := range licensecheck.BuiltinLicenses() {
			if l.ID != "" && !strings.Contains(l.ID, " ") {
				spdxIDs[strings.ToLower(l.ID)] = l.ID
			}
		}
		// IDs the SPDX list deprecated for an -only or -or-later form,
		// still common in package metadata.
		for _, id := range []string{"GPL-2.0", "GPL-3.0", "LGPL-2.0", "LGPL-2.1", "LGPL-3.0", "AGPL-3.0", "GFDL-1.3"} {
			if _, ok := spdxIDs[strings.ToLower(id)]; !ok {
				spdxIDs[strings.ToLower(id)] = id
			}
		}
	})
	id, ok := spdxIDs[strings.ToLower(s)]
	return id, ok
}

var exprToken = regexp.MustCompile(`\(|\)|[^\s()]+`)

// canonicalExpression is s as an SPDX license expression with each ID in
// its canonical case, or "" when s is not one.
func canonicalExpression(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	toks := exprToken.FindAllString(s, -1)
	out := make([]string, 0, len(toks))
	depth, operand := 0, false
	for i, t := range toks {
		switch u := strings.ToUpper(t); {
		case t == "(":
			if operand {
				return ""
			}
			depth++
			out = append(out, t)
		case t == ")":
			if !operand || depth == 0 {
				return ""
			}
			depth--
			out = append(out, t)
		case u == "AND" || u == "OR" || u == "WITH":
			if !operand || i == len(toks)-1 {
				return ""
			}
			operand = false
			out = append(out, " "+u+" ")
		default:
			if operand {
				return ""
			}
			operand = true
			if i > 0 && strings.EqualFold(toks[i-1], "WITH") {
				out = append(out, t)
				continue
			}
			plus := strings.HasSuffix(t, "+")
			id := strings.TrimSuffix(t, "+")
			if strings.HasPrefix(id, "LicenseRef-") || strings.HasPrefix(id, "DocumentRef-") {
				out = append(out, t)
				continue
			}
			c, ok := spdxID(id)
			if !ok {
				return ""
			}
			if plus {
				c += "+"
			}
			out = append(out, c)
		}
	}
	if depth != 0 || !operand {
		return ""
	}
	e := strings.Join(out, "")
	e = strings.ReplaceAll(strings.ReplaceAll(e, "( ", "("), " )", ")")
	return e
}

// classifyText is the license a text is, or the licenses it holds
// (AND), by licensecheck, when they cover most of it.
func classifyText(b []byte) string {
	cov := licensecheck.Scan(b)
	if cov.Percent < 75 {
		return ""
	}
	var ids []string
	for _, m := range cov.Match {
		if id, ok := spdxID(m.ID); ok {
			ids = append(ids, id)
		}
	}
	return joinExpr(ids, "AND")
}

// classifyLicenseFiles is a Go module's license, from its license files:
// each one's license, all of them applying.
func classifyLicenseFiles(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var ids []string
	for _, n := range names {
		if l := classifyText(files[n]); l != "" {
			ids = append(ids, l)
		}
	}
	return joinExpr(ids, "AND")
}
