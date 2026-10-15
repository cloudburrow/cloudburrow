package buildpacks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrNoRuntime means the source names no language the builder can detect.
var ErrNoRuntime = errors.New("no runtime detected")

// EnvRuntime is the variable Google's buildpacks read to force a runtime
// instead of detecting one, such as "nodejs" or "python". Cloud Functions
// sets it from --runtime; a SubmitBuild caller sets it in
// buildpack_build.environment_variables.
const EnvRuntime = "GOOGLE_RUNTIME"

// marker is one file a runtime buildpack's detection passes on.
type marker struct {
	runtime string
	// manifest is an exact file name at the source's root; ext, a source
	// file extension anywhere in the tree.
	manifest, ext string
}

// markers are the files Google's runtime buildpacks detect on, in the order
// a message lists them. A manifest at the root is stronger evidence than a
// source file somewhere in the tree: a Python service with a static .js file
// is a Python service.
var markers = []marker{
	{runtime: "go", manifest: "go.mod"},
	{runtime: "nodejs", manifest: "package.json"},
	{runtime: "python", manifest: "requirements.txt"},
	{runtime: "python", manifest: "pyproject.toml"},
	{runtime: "python", manifest: "setup.py"},
	{runtime: "java", manifest: "pom.xml"},
	{runtime: "java", manifest: "build.gradle"},
	{runtime: "java", manifest: "build.gradle.kts"},
	{runtime: "ruby", manifest: "Gemfile"},
	{runtime: "ruby", manifest: "gems.rb"},
	{runtime: "php", manifest: "composer.json"},
	{runtime: "go", ext: ".go"},
	{runtime: "nodejs", ext: ".js"},
	{runtime: "python", ext: ".py"},
	{runtime: "java", ext: ".jar"},
	{runtime: "ruby", ext: ".rb"},
	{runtime: "php", ext: ".php"},
	{runtime: "dotnet", ext: ".csproj"},
	{runtime: "dotnet", ext: ".fsproj"},
	{runtime: "dotnet", ext: ".vbproj"},
}

// skipDirs are never searched for source files: dependencies and tool state,
// not the application.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "venv": true}

// maxWalk bounds the source files examined. Past it, detection gives up
// rather than refuse a build the builder may accept.
const maxWalk = 20000

// Detection is the runtime a source builds as.
type Detection struct {
	// Runtime is the language, such as "go" or "nodejs"; empty when the
	// source matches several and the builder's own order decides.
	Runtime string
	// Version is GOOGLE_RUNTIME's version suffix ("22" in "nodejs22"), when
	// it carries one.
	Version string
	// From says why: "GOOGLE_RUNTIME", or the marker file that matched.
	From string
	// Candidates are every runtime the source's markers match, sorted, when
	// more than one does.
	Candidates []string
}

// String describes the detection for a build's messages.
func (d Detection) String() string {
	switch {
	case d.Runtime != "" && d.From == EnvRuntime:
		return fmt.Sprintf("runtime %s, from %s", d.Runtime, EnvRuntime)
	case d.Runtime != "":
		return fmt.Sprintf("runtime %s, detected from %s", d.Runtime, d.From)
	case len(d.Candidates) > 0:
		return fmt.Sprintf("runtime chosen by the builder among %s", strings.Join(d.Candidates, ", "))
	}
	return "runtime chosen by the builder"
}

// Detect reports the runtime the pinned builder builds dir as, the way
// Google's buildpacks choose one: GOOGLE_RUNTIME in env when set, otherwise
// the source's markers (go.mod, package.json, requirements.txt, pom.xml and
// the others in markers).
//
// Detect never overrides the builder. A GOOGLE_RUNTIME the caller set is
// passed to pack unchanged, and nothing is set when it is not, so a source
// that matches several runtimes is built as the builder's order decides; that
// case is reported in Candidates. It refuses, with ErrNoRuntime, only a
// source with no marker at all, which no runtime buildpack would accept.
func Detect(dir string, env map[string]string) (Detection, error) {
	if v := strings.TrimSpace(env[EnvRuntime]); v != "" {
		lang := strings.TrimRight(v, "0123456789.")
		return Detection{Runtime: lang, Version: strings.TrimPrefix(v, lang), From: EnvRuntime}, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return Detection{}, fmt.Errorf("%w: read the source: %v", ErrBuildFailed, err)
	}
	if !info.IsDir() {
		return Detection{}, fmt.Errorf("%w: the source %s is not a directory", ErrBuildFailed, dir)
	}

	// Manifests at the root decide first.
	found := map[string]string{}
	for _, m := range markers {
		if m.manifest == "" || found[m.runtime] != "" {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, m.manifest)); err == nil && !fi.IsDir() {
			found[m.runtime] = m.manifest
		}
	}
	if len(found) == 0 {
		exts := map[string]marker{}
		for _, m := range markers {
			if m.ext != "" {
				exts[m.ext] = m
			}
		}
		n := 0
		gaveUp := errors.New("too many files")
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != dir && skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if n++; n > maxWalk {
				return gaveUp
			}
			if m, ok := exts[filepath.Ext(d.Name())]; ok && found[m.runtime] == "" {
				rel, _ := filepath.Rel(dir, path)
				found[m.runtime] = filepath.ToSlash(rel)
			}
			return nil
		})
		if errors.Is(err, gaveUp) && len(found) == 0 {
			return Detection{}, nil
		}
	}

	switch len(found) {
	case 0:
		return Detection{}, fmt.Errorf("%w: the source has none of the files Google's buildpacks detect a language by "+
			"(go.mod, package.json, requirements.txt, pyproject.toml, setup.py, pom.xml, build.gradle, Gemfile, composer.json, "+
			"or .go, .js, .py, .jar, .rb, .php or .csproj sources); set %s to choose one", ErrNoRuntime, EnvRuntime)
	case 1:
		for rt, from := range found {
			return Detection{Runtime: rt, From: from}, nil
		}
	}
	d := Detection{}
	for rt := range found {
		d.Candidates = append(d.Candidates, rt)
	}
	sort.Strings(d.Candidates)
	return d, nil
}
