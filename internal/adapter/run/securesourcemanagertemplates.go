package run

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A repository's initialization (#2071): initial_config's gitignores,
// license and readme, as Google's "Create a repository" page documents
// them (https://docs.cloud.google.com/secure-source-manager/docs/create-repository,
// read 2026-11-20): "initialize the repository and add .gitignore,
// license, and README files", --gitignores a comma-separated list of
// gitignore template names, --license a license template name, --readme
// default to create a README from the default template.
//
// The template names are the ones InitialConfig's reference lists. Their
// content comes from github/gitignore (CC0-1.0) and the SPDX License List
// (third_party/ssm-templates/PROVENANCE.md), generated into
// ssmtemplates.tar.gz by tools/ssmtemplates/generate.py: Google publishes
// neither its templates' text nor its default README, so the files are
// UNVERIFIED stand-ins for Google's. The README is "# {repository ID}",
// then the description when there is one. The files are the repository's
// first commit on its default branch, "Initial commit", written by the
// caller: .gitignore is the chosen templates one after another, each
// headed by its name, LICENSE the licence's text, README.md the README.

//go:embed ssmtemplates.tar.gz
var ssmTemplatesArchive []byte

var ssmTemplates = struct {
	once sync.Once
	// gitignore and license are the templates' content by name.
	gitignore, license map[string][]byte
	err                error
}{}

func loadSSMTemplates() error {
	ssmTemplates.once.Do(func() {
		ssmTemplates.gitignore, ssmTemplates.license = map[string][]byte{}, map[string][]byte{}
		zr, err := gzip.NewReader(bytes.NewReader(ssmTemplatesArchive))
		if err != nil {
			ssmTemplates.err = err
			return
		}
		tr := tar.NewReader(zr)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				return
			}
			if err != nil {
				ssmTemplates.err = err
				return
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				ssmTemplates.err = err
				return
			}
			kind, name, _ := strings.Cut(h.Name, "/")
			switch kind {
			case "gitignore":
				ssmTemplates.gitignore[name] = b
			case "license":
				ssmTemplates.license[name] = b
			}
		}
	})
	return ssmTemplates.err
}

// SourceManagerTemplateNames are the gitignore and license template names
// initial_config takes, sorted, for the console's menus.
func SourceManagerTemplateNames() (gitignores, licenses []string) {
	if loadSSMTemplates() != nil {
		return nil, nil
	}
	for k := range ssmTemplates.gitignore {
		gitignores = append(gitignores, k)
	}
	for k := range ssmTemplates.license {
		licenses = append(licenses, k)
	}
	sort.Strings(gitignores)
	sort.Strings(licenses)
	return gitignores, licenses
}

// ssmInitialFiles are the files an initial_config asks for, by path, or
// the refusal of a template it names that does not exist.
func ssmInitialFiles(ic *ssmpb.Repository_InitialConfig, id, description string) (map[string][]byte, error) {
	if len(ic.GetGitignores()) == 0 && ic.GetLicense() == "" && ic.GetReadme() == "" {
		return nil, nil
	}
	if err := loadSSMTemplates(); err != nil {
		return nil, apierror.Internal(err, "repository templates")
	}
	files := map[string][]byte{}
	if gs := ic.GetGitignores(); len(gs) > 0 {
		var b bytes.Buffer
		seen := map[string]bool{}
		for _, g := range gs {
			t, ok := ssmTemplates.gitignore[g]
			if !ok {
				return nil, apierror.InvalidArgument("initial_config.gitignores: %q is not a gitignore template name", g)
			}
			if seen[g] {
				continue
			}
			seen[g] = true
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "# %s\n", g)
			b.Write(t)
			if !bytes.HasSuffix(t, []byte("\n")) {
				b.WriteString("\n")
			}
		}
		files[".gitignore"] = b.Bytes()
	}
	if l := ic.GetLicense(); l != "" {
		t, ok := ssmTemplates.license[l]
		if !ok {
			return nil, apierror.InvalidArgument("initial_config.license: %q is not a license template name", l)
		}
		files["LICENSE"] = t
	}
	switch ic.GetReadme() {
	case "":
	case "default":
		readme := "# " + id + "\n"
		if d := strings.TrimSpace(description); d != "" {
			readme += "\n" + d + "\n"
		}
		files["README.md"] = []byte(readme)
	default:
		return nil, apierror.InvalidArgument("initial_config.readme: %q is not a README template name: the one template is default", ic.GetReadme())
	}
	return files, nil
}

// ssmWriteInitialCommit writes files as a bare repository's first commit on
// branch, by author (an email).
func ssmWriteInitialCommit(ctx context.Context, dir, branch, author string, files map[string][]byte) error {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var tree bytes.Buffer
	for _, p := range paths {
		cmd := exec.CommandContext(ctx, "git", "-C", dir, "hash-object", "-w", "--stdin")
		cmd.Stdin = bytes.NewReader(files[p])
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("git hash-object %s: %w", p, err)
		}
		fmt.Fprintf(&tree, "100644 blob %s\t%s\n", strings.TrimSpace(string(out)), p)
	}
	mk := exec.CommandContext(ctx, "git", "-C", dir, "mktree")
	mk.Stdin = &tree
	treeSHA, err := mk.Output()
	if err != nil {
		return fmt.Errorf("git mktree: %w", err)
	}
	name := author
	if i := strings.Index(name, "@"); i > 0 {
		name = name[:i]
	}
	now := fmt.Sprintf("%d +0000", time.Now().Unix())
	ct := exec.CommandContext(ctx, "git", "-C", dir, "commit-tree", strings.TrimSpace(string(treeSHA)), "-m", "Initial commit")
	ct.Env = append(os.Environ(), "GIT_AUTHOR_NAME="+name, "GIT_AUTHOR_EMAIL="+author, "GIT_AUTHOR_DATE="+now,
		"GIT_COMMITTER_NAME="+name, "GIT_COMMITTER_EMAIL="+author, "GIT_COMMITTER_DATE="+now)
	commit, err := ct.Output()
	if err != nil {
		return fmt.Errorf("git commit-tree: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", dir, "update-ref", "refs/heads/"+branch, strings.TrimSpace(string(commit)), "").CombinedOutput(); err != nil {
		return fmt.Errorf("git update-ref: %w: %s", err, out)
	}
	return nil
}
