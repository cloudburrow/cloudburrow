package run

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/uuid"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"
)

// GO and KFP repositories (#1897).
//
// GO, as https://docs.cloud.google.com/artifact-registry/docs/go/store-go
// documents it: `gcloud artifacts go upload` sends a module zip (the
// module proxy's .zip, every file under {module}@{version}/) to the API's
// media upload, /upload/v1/{repository}/goModules:create, and `go` reads
// the repository as a module proxy with
// GOPROXY=https://{location}-go.pkg.dev/{project}/{repository}: the
// GOPROXY protocol's $module/@v/list, $version.info, .mod and .zip, and
// $module/@latest (https://go.dev/ref/mod#goproxy-protocol). A package is
// a module path, a version its canonical semantic version; a version once
// uploaded cannot be replaced, as the module proxy protocol needs.
//
// KFP, as https://docs.cloud.google.com/vertex-ai/docs/pipelines/create-pipeline-template
// documents its REST protocol, which the kfp SDK's RegistryClient speaks:
// a multipart POST to https://{location}-kfp.pkg.dev/{project}/{repository}
// of `content` (a pipeline_spec.yaml) and `tags` (comma-separated, up to
// eight) makes a version named by the file's sha256 digest in the package
// pipeline_spec.pipeline_info.name names, and answers
// {packageName}/{versionName}; GET {base}/{package}/{version or tag}
// downloads it. The API's media upload, kfpArtifacts:create, does the
// same. Packages, versions and tags are the API's, as for other formats.

var (
	goUploadRE  = regexp.MustCompile(`^/upload/v1/(projects/[^/]+/locations/[^/]+/repositories/[^/]+)/goModules:create$`)
	kfpUploadRE = regexp.MustCompile(`^/upload/v1/(projects/[^/]+/locations/[^/]+/repositories/[^/]+)/kfpArtifacts:create$`)
	// kfpPackageRE and kfpTagRE are the documented package and tag names.
	kfpPackageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{3,127}$`)
	kfpTagRE     = regexp.MustCompile(`^[a-zA-Z0-9\-._~:@+]{1,128}$`)
)

// maxGoModule is the module proxy's limit of a module zip, 500 MiB, which
// golang.org/x/mod/zip enforces.
const maxGoModule = 500 << 20

// serveFormatUploads serves the GO and KFP media uploads.
func serveFormatUploads(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	if m := goUploadRE.FindStringSubmatch(r.URL.Path); m != nil {
		goUpload(w, r, m[1])
		return true
	}
	if m := kfpUploadRE.FindStringSubmatch(r.URL.Path); m != nil {
		kfpMediaUpload(w, r, m[1])
		return true
	}
	return false
}

// mediaParts reads a multipart/related media upload: the JSON metadata,
// then the content.
func mediaParts(r *http.Request, meta any, limit int64) ([]byte, error) {
	boundary, ok := multipartBoundary(r.Header.Get("Content-Type"))
	if !ok {
		return nil, fmt.Errorf("the upload must be uploadType=multipart: the metadata, then the file")
	}
	mr := multipart.NewReader(r.Body, boundary)
	part, err := mr.NextPart()
	if err != nil {
		return nil, fmt.Errorf("the first part must be the JSON metadata")
	}
	if b, _ := io.ReadAll(io.LimitReader(part, 1<<20)); len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, meta); err != nil {
			return nil, fmt.Errorf("the first part must be the JSON metadata: %v", err)
		}
	}
	part, err = mr.NextPart()
	if err != nil {
		return nil, fmt.Errorf("the second part must be the file")
	}
	b, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("the file is larger than %d bytes", limit)
	}
	return b, nil
}

// doneUpload answers a media upload with its operation, already done.
func doneUpload(w http.ResponseWriter, repoName string, response proto.Message) {
	parent := repoName[:strings.Index(repoName, "/repositories/")]
	opName := parent + "/operations/" + uuid.NewString()
	if a := arServer.Load(); a != nil && a.s != nil {
		op := a.s.ops.Create(parent, repoName)
		_ = a.s.ops.SetMetadata(op.Name, &arpb.OperationMetadata{})
		_ = a.s.ops.Succeed(op.Name, response)
		opName = op.Name
	}
	jsonReply(w, http.StatusOK, map[string]any{"operation": map[string]any{"name": opName, "done": true,
		"response": map[string]any{"@type": "type.googleapis.com/" + string(proto.MessageName(response))}}})
}

func formatRepoFor(w http.ResponseWriter, repoName string, f arpb.Repository_Format) bool {
	repo, ok := repoFormat(repoName)
	if !ok {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "Requested entity was not found.")
		return false
	}
	if repo.GetFormat() != f || repo.GetMode() != arpb.Repository_STANDARD_REPOSITORY {
		apiError(w, http.StatusBadRequest, "FAILED_PRECONDITION", repoName+" is not a standard "+f.String()+" repository")
		return false
	}
	return true
}

// goFile is a Go module file's ID: the module proxy's path, unescaped.
func goFile(mod, version, ext string) string { return mod + "/@v/" + version + ext }

func goUpload(w http.ResponseWriter, r *http.Request, repoName string) {
	if !formatRepoFor(w, repoName, arpb.Repository_GO) {
		return
	}
	var meta struct{}
	b, err := mediaParts(r, &meta, maxGoModule)
	if err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	mod, version, gomod, err := readModuleZip(b)
	if err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid module zip: "+err.Error())
		return
	}
	if _, exists := fileByID(repoName, goFile(mod, version, ".zip")); exists {
		apiError(w, http.StatusConflict, "ALREADY_EXISTS", "module "+mod+"@"+version+" already exists")
		return
	}
	if _, err := addFile(repoName, goFile(mod, version, ".mod"), mod, version, gomod, nil); err != nil {
		apiError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if _, err := addFile(repoName, goFile(mod, version, ".zip"), mod, version, b, nil); err != nil {
		apiError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	now := timestamppb.Now()
	doneUpload(w, repoName, &arpb.GoModule{Name: repoName + "/goModules/" + esc(mod) + ":" + version, Version: version,
		CreateTime: now, UpdateTime: now})
}

// readModuleZip checks a module zip as the module proxy's: every file
// under one {module}@{version}/, a valid module path and canonical
// version, and returns its go.mod (a bare module line when it has none).
func readModuleZip(b []byte) (mod, version string, gomod []byte, err error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", "", nil, err
	}
	prefix := ""
	for _, f := range zr.File {
		at := strings.Index(f.Name, "@")
		slash := strings.Index(f.Name[max(at, 0):], "/")
		if at <= 0 || slash < 0 {
			return "", "", nil, fmt.Errorf("file %q is not under {module}@{version}/", f.Name)
		}
		p := f.Name[:at+slash+1]
		if prefix == "" {
			prefix = p
		} else if p != prefix {
			return "", "", nil, fmt.Errorf("file %q is not under %s", f.Name, prefix)
		}
		if f.Name == prefix+"go.mod" {
			rc, err := f.Open()
			if err != nil {
				return "", "", nil, err
			}
			gomod, err = io.ReadAll(io.LimitReader(rc, 16<<20))
			rc.Close()
			if err != nil {
				return "", "", nil, err
			}
		}
	}
	if prefix == "" {
		return "", "", nil, fmt.Errorf("the zip is empty")
	}
	mod, version, _ = strings.Cut(strings.TrimSuffix(prefix, "/"), "@")
	if err := module.Check(mod, version); err != nil {
		return "", "", nil, err
	}
	if semver.Canonical(version) != version {
		return "", "", nil, fmt.Errorf("version %q is not canonical", version)
	}
	if gomod == nil {
		gomod = []byte("module " + mod + "\n")
	}
	return mod, version, gomod, nil
}

// serveGo is the module proxy protocol over a GO repository.
func serveGo(w http.ResponseWriter, r *http.Request, p *pkgRequest) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "the module proxy is read-only: upload with gcloud artifacts go upload", http.StatusMethodNotAllowed)
		return
	}
	rest := p.rest
	escMod, tail, ok := strings.Cut(rest, "/@v/")
	latest := false
	if !ok {
		escMod, latest = strings.CutSuffix(rest, "/@latest")
		if !latest {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	mod, err := module.UnescapePath(escMod)
	if err != nil {
		http.Error(w, "not found: "+err.Error(), http.StatusNotFound)
		return
	}
	var versions []string
	byVersion := map[string]fmtFile{}
	for _, repo := range p.readRepos() {
		for _, f := range repoFilesOf(repo) {
			if f.Package != mod || !strings.HasSuffix(f.ID, ".zip") {
				continue
			}
			if _, seen := byVersion[f.Version]; !seen {
				versions = append(versions, f.Version)
				byVersion[f.Version] = f
			}
		}
	}
	semver.Sort(versions)
	info := func(v string) {
		jsonReply(w, http.StatusOK, map[string]any{"Version": v, "Time": byVersion[v].Uploaded.UTC().Format(time.RFC3339)})
	}
	if latest || tail == "list" {
		if len(versions) == 0 {
			http.Error(w, "not found: module "+mod+" has no versions in "+p.repo.GetName(), http.StatusNotFound)
			return
		}
		if latest {
			info(versions[len(versions)-1])
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, strings.Join(versions, "\n")+"\n")
		return
	}
	for _, ext := range []string{".info", ".mod", ".zip"} {
		escVer, ok := strings.CutSuffix(tail, ext)
		if !ok {
			continue
		}
		v, err := module.UnescapeVersion(escVer)
		if err != nil {
			break
		}
		f, ok := byVersion[v]
		if !ok {
			http.Error(w, "not found: "+mod+"@"+v, http.StatusNotFound)
			return
		}
		switch ext {
		case ".info":
			info(v)
		case ".mod":
			mf, ok := fileByID(f.repo, goFile(mod, v, ".mod"))
			if !ok {
				http.Error(w, "not found: "+mod+"@"+v+" go.mod", http.StatusNotFound)
				return
			}
			serveBlob(w, r, mf, "text/plain; charset=utf-8")
		case ".zip":
			serveBlob(w, r, f, "application/zip")
		}
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// kfpUpload stores a pipeline template as a KFP version and its tags.
func kfpUpload(repoName string, content []byte, tags []string, description string) (pkg, version string, status int, err error) {
	if len(tags) > 8 {
		return "", "", http.StatusBadRequest, fmt.Errorf("at most 8 tags; got %d", len(tags))
	}
	for _, t := range tags {
		if !kfpTagRE.MatchString(t) {
			return "", "", http.StatusBadRequest, fmt.Errorf("invalid tag %q: it must match %s", t, kfpTagRE)
		}
	}
	var spec struct {
		PipelineInfo struct {
			Name string `yaml:"name"`
		} `yaml:"pipelineInfo"`
	}
	if err := yaml.Unmarshal(content, &spec); err != nil {
		return "", "", http.StatusBadRequest, fmt.Errorf("content must be a pipeline_spec.yaml: %v", err)
	}
	pkg = spec.PipelineInfo.Name
	if !kfpPackageRE.MatchString(pkg) {
		return "", "", http.StatusBadRequest, fmt.Errorf("pipeline_spec.pipeline_info.name %q must match %s", pkg, kfpPackageRE)
	}
	version = "sha256:" + sha256Hex(content)
	id := pkg + "/" + version
	if _, exists := fileByID(repoName, id); !exists {
		var meta map[string]string
		if description != "" {
			meta = map[string]string{"description": description}
		}
		if _, err := addFile(repoName, id, pkg, version, content, meta); err != nil {
			return "", "", http.StatusInternalServerError, err
		}
	}
	for _, t := range tags {
		setFmtTag(repoName, pkg, t, version)
	}
	return pkg, version, http.StatusOK, nil
}

func kfpMediaUpload(w http.ResponseWriter, r *http.Request, repoName string) {
	if !formatRepoFor(w, repoName, arpb.Repository_KFP) {
		return
	}
	var meta struct {
		Tags        []string `json:"tags"`
		Description string   `json:"description"`
	}
	b, err := mediaParts(r, &meta, 10<<20)
	if err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	pkg, version, code, err := kfpUpload(repoName, b, meta.Tags, meta.Description)
	if err != nil {
		apiError(w, code, "INVALID_ARGUMENT", err.Error())
		return
	}
	doneUpload(w, repoName, &arpb.KfpArtifact{Name: repoName + "/kfpArtifacts/" + version, Version: fmtVersionName(repoName, pkg, version)})
}

// serveKfp is the KFP template protocol over a KFP repository.
func serveKfp(w http.ResponseWriter, r *http.Request, p *pkgRequest) {
	rest := strings.Trim(p.rest, "/")
	switch {
	case r.Method == http.MethodPost && rest == "":
		if !p.writable(w) {
			return
		}
		mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mt != "multipart/form-data" || params["boundary"] == "" {
			http.Error(w, "the upload is multipart/form-data with content and tags", http.StatusBadRequest)
			return
		}
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, _, err := r.FormFile("content")
		if err != nil {
			http.Error(w, "the upload needs a content file", http.StatusBadRequest)
			return
		}
		b, err := io.ReadAll(io.LimitReader(f, 10<<20+1))
		f.Close()
		if err != nil || len(b) > 10<<20 {
			http.Error(w, "content is larger than 10 MiB", http.StatusRequestEntityTooLarge)
			return
		}
		var tags []string
		for _, v := range r.MultipartForm.Value["tags"] {
			for _, t := range strings.Split(v, ",") {
				if t = strings.TrimSpace(t); t != "" {
					tags = append(tags, t)
				}
			}
		}
		desc := r.Header.Get("description")
		if v := r.FormValue("description"); v != "" {
			desc = v
		}
		pkg, version, code, err := kfpUpload(p.repo.GetName(), b, tags, desc)
		if err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, pkg+"/"+version)
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.Count(rest, "/") == 1:
		pkg, ref, _ := strings.Cut(rest, "/")
		for _, repo := range p.readRepos() {
			version := ref
			if !strings.HasPrefix(ref, "sha256:") {
				version = tagsOf(repo, pkg)[ref]
			}
			if f, ok := fileByID(repo, pkg+"/"+version); ok {
				serveBlob(w, r, f, "application/x-yaml")
				return
			}
		}
		http.Error(w, "not found: "+pkg+"/"+ref, http.StatusNotFound)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}
