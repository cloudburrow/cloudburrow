package run

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/registry"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Package formats other than Docker (#1531): PYTHON, NPM, MAVEN, GENERIC,
// APT and YUM repositories. Each one's files are blobs in the registry's
// content-addressed store; an index of them, by repository, says which
// package and version each file belongs to, and is saved beside the
// repositories. The packages, versions, files and tags Artifact
// Registry's API lists are read from that index, as a DOCKER repository's
// are read from its manifests, and each format's own protocol
// (artifactregistryprotocols.go) writes and reads it.

// fmtFile is one uploaded file of a package version.
type fmtFile struct {
	// ID is the file's path in the repository, unescaped: a Python
	// distribution's filename, an npm tarball's package/-/name-version.tgz,
	// a Maven file's repository path, or a generic package:version:name.
	ID       string            `json:"id"`
	Package  string            `json:"package"`
	Version  string            `json:"version"`
	Digest   string            `json:"digest"`
	MD5      string            `json:"md5,omitempty"`
	Size     int64             `json:"size"`
	Uploaded time.Time         `json:"uploaded"`
	Meta     map[string]string `json:"meta,omitempty"`
}

// fmtRepo is one repository's index.
type fmtRepo struct {
	Files []*fmtFile `json:"files,omitempty"`
	// Tags are each package's tags, tag to version: npm's dist-tags, and
	// those CreateTag makes.
	Tags map[string]map[string]string `json:"tags,omitempty"`
	// Docs are npm's version documents, by package and version, served
	// back in its packument.
	Docs map[string]map[string]json.RawMessage `json:"docs,omitempty"`
}

var fmtState = struct {
	mu    sync.Mutex
	repos map[string]*fmtRepo
}{repos: map[string]*fmtRepo{}}

const formatsFile = "formats.json"

func saveFormatsLocked() {
	st := RegistryStore()
	if st == nil {
		return
	}
	b, err := json.Marshal(fmtState.repos)
	if err != nil {
		return
	}
	_ = st.WriteState(formatsFile, b)
}

func loadFormats(st *registry.Store) {
	b, err := st.ReadState(formatsFile)
	if err != nil {
		return
	}
	m := map[string]*fmtRepo{}
	if json.Unmarshal(b, &m) != nil {
		return
	}
	fmtState.mu.Lock()
	fmtState.repos = m
	fmtState.mu.Unlock()
}

// fmtRepoLocked is a repository's index, made when absent.
func fmtRepoLocked(repoName string) *fmtRepo {
	r := fmtState.repos[repoName]
	if r == nil {
		r = &fmtRepo{}
		fmtState.repos[repoName] = r
	}
	return r
}

func dropFormatRepo(repoName string) {
	fmtState.mu.Lock()
	defer fmtState.mu.Unlock()
	if _, ok := fmtState.repos[repoName]; ok {
		delete(fmtState.repos, repoName)
		saveFormatsLocked()
	}
}

// servedFormats are the formats CreateRepository accepts.
var servedFormats = map[arpb.Repository_Format]string{
	arpb.Repository_DOCKER: "docker", arpb.Repository_PYTHON: "python", arpb.Repository_NPM: "npm",
	arpb.Repository_MAVEN: "maven", arpb.Repository_GENERIC: "generic", arpb.Repository_APT: "apt", arpb.Repository_YUM: "yum",
}

// formatURL is a repository's registryUri: Docker's is RepositoryURL;
// every other format's host is {location}-{format}.pkg.dev, and Apt's and
// Yum's path is projects/{project}, as Google's.
func formatURL(name string, f arpb.Repository_Format) string {
	parts := strings.Split(name, "/")
	if len(parts) != 6 {
		return ""
	}
	host := parts[3] + "-" + servedFormats[f] + ".pkg.dev"
	switch f {
	case arpb.Repository_DOCKER:
		return RepositoryURL(name)
	case arpb.Repository_APT, arpb.Repository_YUM:
		return host + "/projects/" + parts[1]
	}
	return host + "/" + parts[1] + "/" + parts[5]
}

// repoNameOf is the repository a resource name is under, or "".
func repoNameOf(name string) string {
	i := strings.Index(name, "/repositories/")
	if i < 0 {
		return ""
	}
	rest := name[i+len("/repositories/"):]
	if j := strings.Index(rest, "/"); j >= 0 {
		rest = rest[:j]
	}
	return name[:i] + "/repositories/" + rest
}

// repoFormat is a stored repository's format, or false when it does not
// exist.
func repoFormat(repoName string) (*arpb.Repository, bool) {
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	r, ok := repositories.repos[repoName]
	return r, ok
}

// isPackageRepo reports whether name is under a stored repository of a
// format other than Docker, whose content is the index.
func isPackageRepo(name string) bool {
	r, ok := repoFormat(repoNameOf(name))
	return ok && r.GetFormat() != arpb.Repository_DOCKER
}

func esc(s string) string { return registry.EscapePackage(s) }

// addFile stores b as a file of a package version, replacing a file with
// the same ID, and returns it.
func addFile(repoName, id, pkg, version string, b []byte, meta map[string]string) (*fmtFile, error) {
	st := RegistryStore()
	if st == nil {
		return nil, apierror.FailedPrecondition("this instance has no registry store")
	}
	d, err := st.PutBlob(b)
	if err != nil {
		return nil, err
	}
	m := md5.Sum(b)
	f := &fmtFile{ID: id, Package: pkg, Version: version, Digest: d, MD5: hex.EncodeToString(m[:]), Size: int64(len(b)),
		Uploaded: time.Now().UTC(), Meta: meta}
	fmtState.mu.Lock()
	defer fmtState.mu.Unlock()
	r := fmtRepoLocked(repoName)
	for i, x := range r.Files {
		if x.ID == id {
			r.Files = append(r.Files[:i], r.Files[i+1:]...)
			break
		}
	}
	r.Files = append(r.Files, f)
	saveFormatsLocked()
	return f, nil
}

// repoFilesOf is a copy of a repository's index entries.
func repoFilesOf(repoName string) []fmtFile {
	fmtState.mu.Lock()
	defer fmtState.mu.Unlock()
	r := fmtState.repos[repoName]
	if r == nil {
		return nil
	}
	out := make([]fmtFile, len(r.Files))
	for i, f := range r.Files {
		out[i] = *f
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// fileByID finds one file of a repository.
func fileByID(repoName, id string) (fmtFile, bool) {
	for _, f := range repoFilesOf(repoName) {
		if f.ID == id {
			return f, true
		}
	}
	return fmtFile{}, false
}

// versionsOf groups a repository's files by package and version.
type pkgVersion struct {
	Package, Version string
	Files            []fmtFile
	First, Last      time.Time
}

func versionsOf(repoName string) []*pkgVersion {
	by := map[[2]string]*pkgVersion{}
	var order []*pkgVersion
	for _, f := range repoFilesOf(repoName) {
		if f.Package == "" || f.Version == "" {
			continue
		}
		k := [2]string{f.Package, f.Version}
		v := by[k]
		if v == nil {
			v = &pkgVersion{Package: f.Package, Version: f.Version, First: f.Uploaded, Last: f.Uploaded}
			by[k] = v
			order = append(order, v)
		}
		v.Files = append(v.Files, f)
		if f.Uploaded.Before(v.First) {
			v.First = f.Uploaded
		}
		if f.Uploaded.After(v.Last) {
			v.Last = f.Uploaded
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].Package != order[j].Package {
			return order[i].Package < order[j].Package
		}
		return order[i].First.After(order[j].First)
	})
	return order
}

func tagsOf(repoName, pkg string) map[string]string {
	fmtState.mu.Lock()
	defer fmtState.mu.Unlock()
	out := map[string]string{}
	if r := fmtState.repos[repoName]; r != nil {
		for t, v := range r.Tags[pkg] {
			out[t] = v
		}
	}
	return out
}

func setFmtTag(repoName, pkg, tag, version string) {
	fmtState.mu.Lock()
	defer fmtState.mu.Unlock()
	r := fmtRepoLocked(repoName)
	if r.Tags == nil {
		r.Tags = map[string]map[string]string{}
	}
	if r.Tags[pkg] == nil {
		r.Tags[pkg] = map[string]string{}
	}
	if version == "" {
		delete(r.Tags[pkg], tag)
	} else {
		r.Tags[pkg][tag] = version
	}
	saveFormatsLocked()
}

// removeFiles drops the files drop selects, and the tags and npm documents
// of versions left with no file.
func removeFiles(repoName string, drop func(fmtFile) bool) int {
	fmtState.mu.Lock()
	defer fmtState.mu.Unlock()
	r := fmtState.repos[repoName]
	if r == nil {
		return 0
	}
	n := 0
	kept := r.Files[:0]
	live := map[[2]string]bool{}
	for _, f := range r.Files {
		if drop(*f) {
			n++
			continue
		}
		kept = append(kept, f)
		live[[2]string{f.Package, f.Version}] = true
	}
	r.Files = kept
	for pkg, tags := range r.Tags {
		for t, v := range tags {
			if !live[[2]string{pkg, v}] {
				delete(tags, t)
			}
		}
	}
	for pkg, docs := range r.Docs {
		for v := range docs {
			if !live[[2]string{pkg, v}] {
				delete(docs, v)
			}
		}
	}
	saveFormatsLocked()
	return n
}

// The API's packages, versions, files and tags of a package repository.

func fmtPackageName(repoName, pkg string) string { return repoName + "/packages/" + esc(pkg) }

func fmtVersionName(repoName, pkg, v string) string {
	return fmtPackageName(repoName, pkg) + "/versions/" + esc(v)
}

func fmtFileName(repoName, id string) string { return repoName + "/files/" + esc(id) }

// splitFmtName splits a package, version or tag name of a package
// repository into its repository, package and child ID.
func splitFmtName(name, collection string) (repoName, pkg, child string, err error) {
	repoName = repoNameOf(name)
	rest := strings.TrimPrefix(name, repoName+"/packages/")
	if repoName == "" || rest == name {
		return "", "", "", apierror.InvalidArgument("invalid name %q", name)
	}
	if collection == "" {
		if rest == "" || strings.Contains(rest, "/") {
			return "", "", "", apierror.InvalidArgument("invalid package name %q", name)
		}
		return repoName, registry.UnescapePackage(rest), "", nil
	}
	p, c, ok := strings.Cut(rest, "/"+collection+"/")
	if !ok || p == "" || c == "" || strings.Contains(p, "/") || strings.Contains(c, "/") {
		return "", "", "", apierror.InvalidArgument("invalid %s name %q", strings.TrimSuffix(collection, "s"), name)
	}
	return repoName, registry.UnescapePackage(p), registry.UnescapePackage(c), nil
}

func fmtPackage(repoName string, vs []*pkgVersion, pkg string) (*arpb.Package, bool) {
	var p *arpb.Package
	for _, v := range vs {
		if v.Package != pkg {
			continue
		}
		if p == nil {
			p = &arpb.Package{Name: fmtPackageName(repoName, pkg), DisplayName: pkg,
				CreateTime: timestamppb.New(v.First), UpdateTime: timestamppb.New(v.Last)}
		}
		if v.First.Before(p.CreateTime.AsTime()) {
			p.CreateTime = timestamppb.New(v.First)
		}
		if v.Last.After(p.UpdateTime.AsTime()) {
			p.UpdateTime = timestamppb.New(v.Last)
		}
	}
	if p == nil {
		return nil, false
	}
	decorate(p.Name, func(a map[string]string) { p.Annotations = a }, func(d string) { p.DisplayName = d })
	return p, true
}

func (a *ArtifactRegistryServer) fmtListPackages(req *arpb.ListPackagesRequest) (*arpb.ListPackagesResponse, error) {
	vs := versionsOf(req.GetParent())
	var all []*arpb.Package
	seen := map[string]bool{}
	for _, v := range vs {
		if seen[v.Package] {
			continue
		}
		seen[v.Package] = true
		if p, ok := fmtPackage(req.GetParent(), vs, v.Package); ok {
			all = append(all, p)
		}
	}
	page, next, err := pageOf(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &arpb.ListPackagesResponse{Packages: page, NextPageToken: next}, nil
}

func (a *ArtifactRegistryServer) fmtGetPackage(name string) (*arpb.Package, error) {
	repoName, pkg, _, err := splitFmtName(name, "")
	if err != nil {
		return nil, err
	}
	p, ok := fmtPackage(repoName, versionsOf(repoName), pkg)
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return p, nil
}

func (a *ArtifactRegistryServer) fmtDeletePackage(name string) (*longrunningpb.Operation, error) {
	repoName, pkg, _, err := splitFmtName(name, "")
	if err != nil {
		return nil, err
	}
	if removeFiles(repoName, func(f fmtFile) bool { return f.Package == pkg }) == 0 {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	forgetUnder(name)
	return a.doneOrNil(repoName, name)
}

// doneOrNil is doneEmpty, or no operation for the console's server.
func (a *ArtifactRegistryServer) doneOrNil(repoName, target string) (*longrunningpb.Operation, error) {
	if a.s == nil {
		return nil, nil
	}
	return a.doneEmpty(repoName, target)
}

func fmtVersion(repoName string, v *pkgVersion, full bool) *arpb.Version {
	out := &arpb.Version{Name: fmtVersionName(repoName, v.Package, v.Version),
		CreateTime: timestamppb.New(v.First), UpdateTime: timestamppb.New(v.Last)}
	decorate(out.Name, func(a map[string]string) { out.Annotations = a }, nil)
	if full {
		tags := tagsOf(repoName, v.Package)
		names := make([]string, 0, len(tags))
		for t, ver := range tags {
			if ver == v.Version {
				names = append(names, t)
			}
		}
		sort.Strings(names)
		for _, t := range names {
			out.RelatedTags = append(out.RelatedTags, &arpb.Tag{Name: fmtPackageName(repoName, v.Package) + "/tags/" + esc(t), Version: out.Name})
		}
	}
	return out
}

func findVersion(repoName, pkg, version string) (*pkgVersion, bool) {
	for _, v := range versionsOf(repoName) {
		if v.Package == pkg && v.Version == version {
			return v, true
		}
	}
	return nil, false
}

func (a *ArtifactRegistryServer) fmtListVersions(req *arpb.ListVersionsRequest) (*arpb.ListVersionsResponse, error) {
	repoName, pkg, _, err := splitFmtName(req.GetParent(), "")
	if err != nil {
		return nil, err
	}
	var all []*arpb.Version
	for _, v := range versionsOf(repoName) {
		if v.Package == pkg {
			all = append(all, fmtVersion(repoName, v, req.GetView() == arpb.VersionView_FULL))
		}
	}
	if len(all) == 0 {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	page, next, err := pageOf(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &arpb.ListVersionsResponse{Versions: page, NextPageToken: next}, nil
}

func (a *ArtifactRegistryServer) fmtGetVersion(req *arpb.GetVersionRequest) (*arpb.Version, error) {
	repoName, pkg, ver, err := splitFmtName(req.GetName(), "versions")
	if err != nil {
		return nil, err
	}
	v, ok := findVersion(repoName, pkg, ver)
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return fmtVersion(repoName, v, req.GetView() == arpb.VersionView_FULL), nil
}

// fmtDeleteVersionNow deletes a version's files and tags.
func fmtDeleteVersionNow(name string, force bool) error {
	repoName, pkg, ver, err := splitFmtName(name, "versions")
	if err != nil {
		return err
	}
	if _, ok := findVersion(repoName, pkg, ver); !ok {
		return apierror.NotFound("Requested entity was not found.")
	}
	if !force {
		var tagged []string
		for t, v := range tagsOf(repoName, pkg) {
			if v == ver {
				tagged = append(tagged, t)
			}
		}
		if len(tagged) > 0 {
			sort.Strings(tagged)
			return apierror.FailedPrecondition("version %s has tags %s; delete them first, or set force", name, strings.Join(tagged, ", "))
		}
	}
	removeFiles(repoName, func(f fmtFile) bool { return f.Package == pkg && f.Version == ver })
	forgetUnder(name)
	return nil
}

func (a *ArtifactRegistryServer) fmtDeleteVersion(req *arpb.DeleteVersionRequest) (*longrunningpb.Operation, error) {
	if err := fmtDeleteVersionNow(req.GetName(), req.GetForce()); err != nil {
		return nil, err
	}
	return a.doneOrNil(repoNameOf(req.GetName()), req.GetName())
}

func fmtFileOf(repoName string, f fmtFile) *arpb.File {
	out := &arpb.File{Name: fmtFileName(repoName, f.ID), SizeBytes: f.Size,
		CreateTime: timestamppb.New(f.Uploaded), UpdateTime: timestamppb.New(f.Uploaded), FetchTime: timestamppb.New(f.Uploaded)}
	if f.Package != "" && f.Version != "" {
		out.Owner = fmtVersionName(repoName, f.Package, f.Version)
	}
	if h, err := hex.DecodeString(strings.TrimPrefix(f.Digest, "sha256:")); err == nil {
		out.Hashes = append(out.Hashes, &arpb.Hash{Type: arpb.Hash_SHA256, Value: h})
	}
	if h, err := hex.DecodeString(f.MD5); err == nil && len(h) > 0 {
		out.Hashes = append(out.Hashes, &arpb.Hash{Type: arpb.Hash_MD5, Value: h})
	}
	decorate(out.Name, func(a map[string]string) { out.Annotations = a }, nil)
	return out
}

func (a *ArtifactRegistryServer) fmtListFiles(req *arpb.ListFilesRequest, owner string) (*arpb.ListFilesResponse, error) {
	var all []*arpb.File
	for _, f := range repoFilesOf(req.GetParent()) {
		x := fmtFileOf(req.GetParent(), f)
		if owner == "" || x.GetOwner() == owner {
			all = append(all, x)
		}
	}
	page, next, err := pageOf(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &arpb.ListFilesResponse{Files: page, NextPageToken: next}, nil
}

func fmtFileID(name string) (repoName, id string, err error) {
	repoName = repoNameOf(name)
	rest, ok := strings.CutPrefix(name, repoName+"/files/")
	if repoName == "" || !ok || rest == "" || strings.Contains(rest, "/") {
		return "", "", apierror.InvalidArgument("invalid file name %q", name)
	}
	return repoName, registry.UnescapePackage(rest), nil
}

func (a *ArtifactRegistryServer) fmtGetFile(name string) (*arpb.File, error) {
	repoName, id, err := fmtFileID(name)
	if err != nil {
		return nil, err
	}
	f, ok := fileByID(repoName, id)
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return fmtFileOf(repoName, f), nil
}

// fmtDeleteFile deletes a file of a GENERIC repository, the one format
// whose files Artifact Registry deletes one by one.
func (a *ArtifactRegistryServer) fmtDeleteFile(name string) (*longrunningpb.Operation, error) {
	repoName, id, err := fmtFileID(name)
	if err != nil {
		return nil, err
	}
	if _, ok := fileByID(repoName, id); !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	if r, _ := repoFormat(repoName); r.GetFormat() != arpb.Repository_GENERIC {
		return nil, apierror.FailedPrecondition("DeleteFile is only allowed on generic repositories; delete the version that holds %s instead", name)
	}
	removeFiles(repoName, func(f fmtFile) bool { return f.ID == id })
	forgetUnder(name)
	return a.doneOrNil(repoName, name)
}

func (a *ArtifactRegistryServer) fmtListTags(req *arpb.ListTagsRequest, wantVersion string) (*arpb.ListTagsResponse, error) {
	repoName, pkg, _, err := splitFmtName(req.GetParent(), "")
	if err != nil {
		return nil, err
	}
	tags := tagsOf(repoName, pkg)
	names := make([]string, 0, len(tags))
	for t := range tags {
		names = append(names, t)
	}
	sort.Strings(names)
	var all []*arpb.Tag
	for _, t := range names {
		tag := &arpb.Tag{Name: req.GetParent() + "/tags/" + esc(t), Version: fmtVersionName(repoName, pkg, tags[t])}
		if wantVersion == "" || tag.Version == wantVersion {
			all = append(all, tag)
		}
	}
	page, next, err := pageOf(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &arpb.ListTagsResponse{Tags: page, NextPageToken: next}, nil
}

func (a *ArtifactRegistryServer) fmtGetTag(name string) (*arpb.Tag, error) {
	repoName, pkg, tag, err := splitFmtName(name, "tags")
	if err != nil {
		return nil, err
	}
	v, ok := tagsOf(repoName, pkg)[tag]
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return &arpb.Tag{Name: name, Version: fmtVersionName(repoName, pkg, v)}, nil
}

func (a *ArtifactRegistryServer) fmtSetTag(tagName, version string, create bool) (*arpb.Tag, error) {
	repoName, pkg, tag, err := splitFmtName(tagName, "tags")
	if err != nil {
		return nil, err
	}
	if !registry.ValidTag(tag) {
		return nil, apierror.InvalidArgument("invalid tag name %q", tagName)
	}
	_, exists := tagsOf(repoName, pkg)[tag]
	if create && exists {
		return nil, apierror.AlreadyExists("the tag already exists")
	}
	if !create && !exists {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	vRepo, vPkg, ver, err := splitFmtName(version, "versions")
	if err != nil || vRepo != repoName || vPkg != pkg {
		return nil, apierror.InvalidArgument("tag version %q is not a version of %s", version, fmtPackageName(repoName, pkg))
	}
	if _, ok := findVersion(repoName, pkg, ver); !ok {
		return nil, apierror.NotFound("version %s was not found", version)
	}
	setFmtTag(repoName, pkg, tag, ver)
	return &arpb.Tag{Name: tagName, Version: version}, nil
}

func (a *ArtifactRegistryServer) fmtDeleteTag(name string) (*emptypb.Empty, error) {
	repoName, pkg, tag, err := splitFmtName(name, "tags")
	if err != nil {
		return nil, err
	}
	if _, ok := tagsOf(repoName, pkg)[tag]; !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	setFmtTag(repoName, pkg, tag, "")
	return &emptypb.Empty{}, nil
}

// formatRepo checks name is a stored repository of format f.
func formatRepo(name string, f arpb.Repository_Format) (*arpb.Repository, error) {
	if _, _, err := parseRepositoryName(name); err != nil {
		return nil, err
	}
	r, ok := repoFormat(name)
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	if r.GetFormat() != f {
		return nil, apierror.FailedPrecondition("repository %s is a %s repository, not %s", name, r.GetFormat(), f)
	}
	return r, nil
}

// splitColon splits a format artifact's ID, the escaped parts joined by
// colons, into n parts.
func splitColon(id string, n int) ([]string, bool) {
	parts := strings.Split(id, ":")
	if len(parts) != n {
		return nil, false
	}
	for i, p := range parts {
		parts[i] = registry.UnescapePackage(p)
		if parts[i] == "" {
			return nil, false
		}
	}
	return parts, true
}

func childOf(name, collection string) (repoName, id string, err error) {
	repoName, id, ok := splitChild(name, collection)
	if !ok {
		return "", "", apierror.InvalidArgument("invalid %s name %q", collection, name)
	}
	return repoName, id, nil
}

func mavenArtifactOf(r *arpb.Repository, v *pkgVersion) *arpb.MavenArtifact {
	group, artifact, _ := strings.Cut(v.Package, ":")
	m := &arpb.MavenArtifact{Name: r.GetName() + "/mavenArtifacts/" + esc(v.Package) + ":" + esc(v.Version),
		GroupId: group, ArtifactId: artifact, Version: v.Version,
		CreateTime: timestamppb.New(v.First), UpdateTime: timestamppb.New(v.Last)}
	for _, f := range v.Files {
		if strings.HasSuffix(f.ID, ".pom") {
			m.PomUri = r.GetRegistryUri() + "/" + f.ID
		}
	}
	return m
}

// ListMavenArtifacts lists a MAVEN repository's artifact versions.
func (a *ArtifactRegistryServer) ListMavenArtifacts(_ context.Context, req *arpb.ListMavenArtifactsRequest) (*arpb.ListMavenArtifactsResponse, error) {
	r, err := formatRepo(req.GetParent(), arpb.Repository_MAVEN)
	if err != nil {
		return nil, err
	}
	var all []*arpb.MavenArtifact
	for _, v := range versionsOf(r.GetName()) {
		all = append(all, mavenArtifactOf(r, v))
	}
	page, next, err := pageOf(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &arpb.ListMavenArtifactsResponse{MavenArtifacts: page, NextPageToken: next}, nil
}

// GetMavenArtifact reads one, named {groupId}:{artifactId}:{version}.
func (a *ArtifactRegistryServer) GetMavenArtifact(_ context.Context, req *arpb.GetMavenArtifactRequest) (*arpb.MavenArtifact, error) {
	repoName, id, err := childOf(req.GetName(), "mavenArtifacts")
	if err != nil {
		return nil, err
	}
	parts, ok := splitColon(id, 3)
	if !ok {
		return nil, apierror.InvalidArgument("invalid maven artifact name %q: want .../mavenArtifacts/GROUP:ARTIFACT:VERSION", req.GetName())
	}
	r, err := formatRepo(repoName, arpb.Repository_MAVEN)
	if err != nil {
		return nil, err
	}
	v, ok := findVersion(repoName, parts[0]+":"+parts[1], parts[2])
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return mavenArtifactOf(r, v), nil
}

func npmPackageOf(r *arpb.Repository, v *pkgVersion) *arpb.NpmPackage {
	n := &arpb.NpmPackage{Name: r.GetName() + "/npmPackages/" + esc(v.Package) + ":" + esc(v.Version),
		PackageName: v.Package, Version: v.Version, CreateTime: timestamppb.New(v.First), UpdateTime: timestamppb.New(v.Last)}
	tags := tagsOf(r.GetName(), v.Package)
	for t, ver := range tags {
		if ver == v.Version {
			n.Tags = append(n.Tags, t)
		}
	}
	sort.Strings(n.Tags)
	return n
}

// ListNpmPackages lists an NPM repository's package versions.
func (a *ArtifactRegistryServer) ListNpmPackages(_ context.Context, req *arpb.ListNpmPackagesRequest) (*arpb.ListNpmPackagesResponse, error) {
	r, err := formatRepo(req.GetParent(), arpb.Repository_NPM)
	if err != nil {
		return nil, err
	}
	var all []*arpb.NpmPackage
	for _, v := range versionsOf(r.GetName()) {
		all = append(all, npmPackageOf(r, v))
	}
	page, next, err := pageOf(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &arpb.ListNpmPackagesResponse{NpmPackages: page, NextPageToken: next}, nil
}

// GetNpmPackage reads one, named {package}:{version}.
func (a *ArtifactRegistryServer) GetNpmPackage(_ context.Context, req *arpb.GetNpmPackageRequest) (*arpb.NpmPackage, error) {
	repoName, id, err := childOf(req.GetName(), "npmPackages")
	if err != nil {
		return nil, err
	}
	parts, ok := splitColon(id, 2)
	if !ok {
		return nil, apierror.InvalidArgument("invalid npm package name %q: want .../npmPackages/PACKAGE:VERSION", req.GetName())
	}
	r, err := formatRepo(repoName, arpb.Repository_NPM)
	if err != nil {
		return nil, err
	}
	v, ok := findVersion(repoName, parts[0], parts[1])
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return npmPackageOf(r, v), nil
}

func pythonPackageOf(r *arpb.Repository, v *pkgVersion) *arpb.PythonPackage {
	p := &arpb.PythonPackage{Name: r.GetName() + "/pythonPackages/" + esc(v.Package) + ":" + esc(v.Version),
		PackageName: v.Package, Version: v.Version, CreateTime: timestamppb.New(v.First), UpdateTime: timestamppb.New(v.Last)}
	if len(v.Files) > 0 {
		p.Uri = r.GetRegistryUri() + "/" + v.Package + "/" + v.Files[0].ID
	}
	return p
}

// ListPythonPackages lists a PYTHON repository's package versions.
func (a *ArtifactRegistryServer) ListPythonPackages(_ context.Context, req *arpb.ListPythonPackagesRequest) (*arpb.ListPythonPackagesResponse, error) {
	r, err := formatRepo(req.GetParent(), arpb.Repository_PYTHON)
	if err != nil {
		return nil, err
	}
	var all []*arpb.PythonPackage
	for _, v := range versionsOf(r.GetName()) {
		all = append(all, pythonPackageOf(r, v))
	}
	page, next, err := pageOf(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &arpb.ListPythonPackagesResponse{PythonPackages: page, NextPageToken: next}, nil
}

// GetPythonPackage reads one, named {package}:{version}.
func (a *ArtifactRegistryServer) GetPythonPackage(_ context.Context, req *arpb.GetPythonPackageRequest) (*arpb.PythonPackage, error) {
	repoName, id, err := childOf(req.GetName(), "pythonPackages")
	if err != nil {
		return nil, err
	}
	parts, ok := splitColon(id, 2)
	if !ok {
		return nil, apierror.InvalidArgument("invalid python package name %q: want .../pythonPackages/PACKAGE:VERSION", req.GetName())
	}
	r, err := formatRepo(repoName, arpb.Repository_PYTHON)
	if err != nil {
		return nil, err
	}
	v, ok := findVersion(repoName, normalizePython(parts[0]), parts[1])
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return pythonPackageOf(r, v), nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
