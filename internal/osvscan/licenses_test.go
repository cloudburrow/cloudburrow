package osvscan

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// bsd3 is the BSD 3-Clause license as Go modules carry it.
const bsd3 = `Copyright (c) 2009,2014 Google Inc. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google Inc. nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
`

func layerTar(t *testing.T, gz bool, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	var w = &b
	var zw *gzip.Writer
	tw := tar.NewWriter(w)
	if gz {
		zw = gzip.NewWriter(w)
		tw = tar.NewWriter(zw)
	}
	for n, c := range files {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(c))
	}
	tw.Close()
	if zw != nil {
		zw.Close()
	}
	return b.Bytes()
}

func jarOf(t *testing.T, files map[string]string) string {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for n, c := range files {
		w, err := z.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(c))
	}
	z.Close()
	return b.String()
}

func imageArchive(t *testing.T, layers ...[]byte) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "image.tar")
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	var names []string
	for i, l := range layers {
		n := string(rune('a'+i)) + "/layer.tar"
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(l)), Typeflag: tar.TypeReg})
		tw.Write(l)
		names = append(names, n)
	}
	man, _ := json.Marshal([]map[string]any{{"Config": "c.json", "Layers": names}})
	tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(man)), Typeflag: tar.TypeReg})
	tw.Write(man)
	tw.Close()
	if err := os.WriteFile(f, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func pkgOf(eco, name, ver string) Package {
	var p Package
	p.Package.Ecosystem, p.Package.Name, p.Package.Version = eco, name, ver
	return p
}

// TestAddLocalLicenses reads each language's license from an image's
// files (#2072): package.json, the hidden lockfile, METADATA, a jar's POM
// (and its parent's), and a Go module's LICENSE in the module cache and a
// vendor directory, through gzip layers and whiteouts.
func TestAddLocalLicenses(t *testing.T) {
	pom := `<project><groupId>com.google.guava</groupId><artifactId>guava</artifactId><version>33.0.0-jre</version>
<licenses><license><name>The Apache Software License, Version 2.0</name><url>http://www.apache.org/licenses/LICENSE-2.0.txt</url></license></licenses></project>`
	child := `<project><parent><groupId>org.example</groupId><artifactId>parent</artifactId><version>1</version></parent><artifactId>child</artifactId></project>`
	parent := `<project><groupId>org.example</groupId><artifactId>parent</artifactId><version>1</version>
<licenses><license><name>Eclipse Public License - v 1.0</name></license><license><name>GNU Lesser General Public License</name><url>https://www.gnu.org/licenses/old-licenses/lgpl-2.1-standalone.html</url></license></licenses></project>`
	base := layerTar(t, true, map[string]string{
		"app/node_modules/ms/package.json":                                `{"name":"ms","version":"2.1.3","license":"mit"}`,
		"app/node_modules/@scope/dual/package.json":                       `{"name":"@scope/dual","version":"1.0.0","licenses":[{"type":"MIT"},{"type":"Apache-2.0"}]}`,
		"app/node_modules/old/package.json":                               `{"name":"old","version":"0.1.0","license":{"type":"ISC"}}`,
		"app/node_modules/.package-lock.json":                             `{"packages":{"node_modules/inherits":{"version":"2.0.4","license":"ISC"}}}`,
		"app/node_modules/gone/package.json":                              `{"name":"gone","version":"1.0.0","license":"MIT"}`,
		"usr/lib/python3/site-packages/six-1.16.0.dist-info/METADATA":     "Metadata-Version: 2.1\nName: six\nVersion: 1.16.0\nLicense: MIT\n\nbody\n",
		"usr/lib/python3/site-packages/packaging-24.2.dist-info/METADATA": "Metadata-Version: 2.4\nName: packaging\nVersion: 24.2\nLicense-Expression: apache-2.0 or bsd-2-clause\n\n",
		"usr/lib/python3/site-packages/Requests-2.0.dist-info/METADATA":   "Name: Requests\nVersion: 2.0\nClassifier: License :: OSI Approved :: Apache Software License\nClassifier: Programming Language :: Python\n\n",
		"app/lib/guava-33.0.0-jre.jar":                                    jarOf(t, map[string]string{"META-INF/maven/com.google.guava/guava/pom.xml": pom}),
		"app/lib/child.jar":                                               jarOf(t, map[string]string{"META-INF/maven/org.example/child/pom.xml": child}),
		"root/.m2/repository/org/example/parent/1/parent-1.pom":           parent,
		"go/pkg/mod/github.com/google/uuid@v1.6.0/LICENSE":                bsd3,
		"go/pkg/mod/github.com/!burnt!sushi/toml@v1.3.2/COPYING":          bsd3,
		"src/vendor/golang.org/x/text/LICENSE":                            bsd3,
	})
	top := layerTar(t, false, map[string]string{"app/node_modules/.wh.gone": ""})
	r := &Result{}
	r.Results = append(r.Results, struct {
		Source struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"source"`
		Packages []Package `json:"packages"`
	}{Packages: []Package{
		pkgOf("npm", "ms", "2.1.3"), pkgOf("npm", "@scope/dual", "1.0.0"), pkgOf("npm", "old", "0.1.0"),
		pkgOf("npm", "inherits", "2.0.4"), pkgOf("npm", "gone", "1.0.0"),
		pkgOf("PyPI", "six", "1.16.0"), pkgOf("PyPI", "packaging", "24.2"), pkgOf("PyPI", "requests", "2.0"),
		pkgOf("Maven", "com.google.guava:guava", "33.0.0-jre"), pkgOf("Maven", "org.example:child", "1"),
		pkgOf("Go", "github.com/google/uuid", "1.6.0"), pkgOf("Go", "github.com/BurntSushi/toml", "1.3.2"),
		pkgOf("Go", "golang.org/x/text", "0.3.0"), pkgOf("Go", "stdlib", "1.22.0"),
		pkgOf("Alpine:v3.13", "zlib", "1.2.11-r3"),
	}})
	r.Results[0].Packages[len(r.Results[0].Packages)-1].Licenses = []string{"Zlib"}
	if err := AddLocalLicenses(r, imageArchive(t, base, top)); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"ms": "MIT", "@scope/dual": "MIT OR Apache-2.0", "old": "ISC", "inherits": "ISC", "gone": "",
		"six": "MIT", "packaging": "Apache-2.0 OR BSD-2-Clause", "requests": "Apache-2.0",
		"com.google.guava:guava": "Apache-2.0", "org.example:child": "EPL-1.0 OR LGPL-2.1",
		"github.com/google/uuid": "BSD-3-Clause", "github.com/BurntSushi/toml": "BSD-3-Clause",
		"golang.org/x/text": "BSD-3-Clause", "stdlib": "", "zlib": "Zlib",
	}
	for _, p := range r.Results[0].Packages {
		if got := licenseOf(p.Licenses); got != want[p.Package.Name] {
			t.Errorf("%s: license %q, want %q", p.Package.Name, got, want[p.Package.Name])
		}
	}
	f := Convert(r)
	if got := f.Packages[0].GetPackage().GetLicense().GetExpression(); got != "MIT" {
		t.Errorf("the PACKAGE occurrence's license = %q", got)
	}
}

func TestCanonicalExpression(t *testing.T) {
	for in, want := range map[string]string{
		"mit":                 "MIT",
		"(MIT OR apache-2.0)": "(MIT OR Apache-2.0)",
		"GPL-2.0-or-later WITH Classpath-exception-2.0": "GPL-2.0-or-later WITH Classpath-exception-2.0",
		"Apache-2.0 AND (MIT OR BSD-3-Clause)":          "Apache-2.0 AND (MIT OR BSD-3-Clause)",
		"LicenseRef-Proprietary":                        "LicenseRef-Proprietary",
		"Apache 2.0":                                    "",
		"MIT OR":                                        "",
		"SEE LICENSE IN LICENSE.txt":                    "",
	} {
		if got := canonicalExpression(in); got != want {
			t.Errorf("canonicalExpression(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"Apache License, Version 2.0": "Apache-2.0",
		"The MIT License":             "MIT",
		"New BSD License":             "BSD-3-Clause",
		"UNLICENSED":                  "",
		"BSD":                         "",
	} {
		if got := licenseFromText(in); got != want {
			t.Errorf("licenseFromText(%q) = %q, want %q", in, got, want)
		}
	}
}
