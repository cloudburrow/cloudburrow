package run

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The registry behind DOCKER repositories (#1383): the Docker Registry
// HTTP API v2 a docker client pushes to and pulls from, and the packages,
// versions, tags, Docker images and files Artifact Registry's API reads
// from what it holds. An image is a package, named by its path with
// slashes escaped; a manifest is a version, named by its digest.

var registryState = struct {
	mu    sync.Mutex
	store *registry.Store
}{}

// UseRegistry sets the store the registry serves, in the instance's state
// directory, and loads the repositories saved beside it.
func UseRegistry(st *registry.Store) {
	registryState.mu.Lock()
	registryState.store = st
	registryState.mu.Unlock()
	if st != nil {
		loadRepositories(st)
	}
}

// RegistryStore is the registry's store, or nil when none is set.
func RegistryStore() *registry.Store {
	registryState.mu.Lock()
	defer registryState.mu.Unlock()
	return registryState.store
}

// RegistryHandler serves the Docker Registry HTTP API v2 over the
// repositories this instance holds; nil without a store.
func RegistryHandler() http.Handler {
	st := RegistryStore()
	if st == nil {
		return nil
	}
	return &registry.Handler{Store: st, Lookup: lookupRegistryRepo, Remote: lookupRegistryUpstream}
}

// RegistryHandlerOr serves the registry's /v2/ paths and passes every other
// request to next.
func RegistryHandlerOr(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RegistryStore() != nil && servePackages(w, r) {
			return
		}
		if registry.IsRegistryPath(r.URL.Path) {
			if h := RegistryHandler(); h != nil {
				h.ServeHTTP(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// lookupRegistryRepo finds a stored repository by location, project and
// ID; with no location, the one repository of that project and ID.
func lookupRegistryRepo(location, project, repo string) (registry.Repo, bool) {
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	if location != "" {
		_, ok := repositories.repos["projects/"+project+"/locations/"+location+"/repositories/"+repo]
		return registry.Repo{Location: location, Project: project, Repository: repo}, ok
	}
	var found []registry.Repo
	for name := range repositories.repos {
		parts := strings.Split(name, "/")
		if parts[1] == project && parts[5] == repo {
			found = append(found, registry.Repo{Location: parts[3], Project: project, Repository: repo})
		}
	}
	if len(found) != 1 {
		return registry.Repo{}, false
	}
	return found[0], true
}

// repoOf is the registry repository a repository name names.
func repoOf(name string) registry.Repo {
	parts := strings.Split(name, "/")
	return registry.Repo{Location: parts[3], Project: parts[1], Repository: parts[5]}
}

// contentStore checks a repository exists and returns the store.
func contentStore(repoName string) (*registry.Store, error) {
	if _, _, err := parseRepositoryName(repoName); err != nil {
		return nil, err
	}
	repositories.mu.Lock()
	_, ok := repositories.repos[repoName]
	repositories.mu.Unlock()
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	st := RegistryStore()
	if st == nil {
		return nil, apierror.FailedPrecondition("this instance has no registry store")
	}
	return st, nil
}

// splitChild splits name at /<collection>/ into the parent and the ID.
func splitChild(name, collection string) (parent, id string, ok bool) {
	i := strings.LastIndex(name, "/"+collection+"/")
	if i < 0 {
		return "", "", false
	}
	id = name[i+len(collection)+2:]
	return name[:i], id, id != "" && !strings.Contains(id, "/")
}

// parsePackageName splits a package name into its repository and image.
func parsePackageName(name string) (repoName, image string, err error) {
	repoName, id, ok := splitChild(name, "packages")
	if !ok {
		return "", "", apierror.InvalidArgument("invalid package name %q", name)
	}
	image = registry.UnescapePackage(id)
	if !registry.ValidImage(image) {
		return "", "", apierror.InvalidArgument("invalid package name %q", name)
	}
	return repoName, image, nil
}

func pageOf[T any](items []T, size int32, token string) ([]T, string, error) {
	start := 0
	if token != "" {
		n, err := strconv.Atoi(token)
		if err != nil || n < 0 || n > len(items) {
			return nil, "", apierror.InvalidArgument("invalid page_token")
		}
		start = n
	}
	end := len(items)
	if size > 0 && start+int(size) < end {
		end = start + int(size)
	}
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[start:end], next, nil
}

func noFilter(filter, orderBy string) error {
	if filter != "" || orderBy != "" {
		return apierror.Unimplemented("filter and order_by are not supported")
	}
	return nil
}

func storeErr(err error) error {
	switch {
	case errors.Is(err, registry.ErrManifestUnknown), errors.Is(err, registry.ErrNameInvalid):
		return apierror.NotFound("Requested entity was not found.")
	case errors.Is(err, registry.ErrTagInvalid):
		return apierror.InvalidArgument("%v", err)
	}
	return err
}

func (a *ArtifactRegistryServer) packageOf(st *registry.Store, repoName, image string) (*arpb.Package, error) {
	ms, err := st.Manifests(repoOf(repoName), image)
	if err != nil || len(ms) == 0 {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	oldest, newest := ms[len(ms)-1].Uploaded, ms[0].Uploaded
	p := &arpb.Package{Name: repoName + "/packages/" + registry.EscapePackage(image), DisplayName: image,
		CreateTime: timestamppb.New(oldest), UpdateTime: timestamppb.New(newest)}
	decorate(p.Name, func(a map[string]string) { p.Annotations = a }, func(d string) { p.DisplayName = d })
	return p, nil
}

// ListPackages lists a repository's images.
func (a *ArtifactRegistryServer) ListPackages(_ context.Context, req *arpb.ListPackagesRequest) (*arpb.ListPackagesResponse, error) {
	if isPackageRepo(req.GetParent()) {
		if err := noFilter(req.GetFilter(), req.GetOrderBy()); err != nil {
			return nil, err
		}
		return a.fmtListPackages(req)
	}
	if err := noFilter(req.GetFilter(), req.GetOrderBy()); err != nil {
		return nil, err
	}
	st, err := contentStore(req.GetParent())
	if err != nil {
		return nil, err
	}
	var all []*arpb.Package
	for _, im := range st.Images(repoOf(req.GetParent())) {
		if p, err := a.packageOf(st, req.GetParent(), im); err == nil {
			all = append(all, p)
		}
	}
	page, next, err := pageOf(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &arpb.ListPackagesResponse{Packages: page, NextPageToken: next}, nil
}

// GetPackage reads an image as a package.
func (a *ArtifactRegistryServer) GetPackage(_ context.Context, req *arpb.GetPackageRequest) (*arpb.Package, error) {
	if isPackageRepo(req.GetName()) {
		return a.fmtGetPackage(req.GetName())
	}
	repoName, image, err := parsePackageName(req.GetName())
	if err != nil {
		return nil, err
	}
	st, err := contentStore(repoName)
	if err != nil {
		return nil, err
	}
	return a.packageOf(st, repoName, image)
}

// DeletePackage deletes an image: every manifest and tag it holds.
func (a *ArtifactRegistryServer) DeletePackage(_ context.Context, req *arpb.DeletePackageRequest) (*longrunningpb.Operation, error) {
	if isPackageRepo(req.GetName()) {
		return a.fmtDeletePackage(req.GetName())
	}
	repoName, image, err := parsePackageName(req.GetName())
	if err != nil {
		return nil, err
	}
	st, err := contentStore(repoName)
	if err != nil {
		return nil, err
	}
	if err := st.DeleteImage(repoOf(repoName), image); err != nil {
		return nil, storeErr(err)
	}
	forgetUnder(req.GetName())
	return a.doneEmpty(repoName, req.GetName())
}

func (a *ArtifactRegistryServer) doneEmpty(parent, target string) (*longrunningpb.Operation, error) {
	loc := parent
	if i := strings.Index(loc, "/repositories/"); i >= 0 {
		loc = loc[:i]
	}
	op := a.s.ops.Create(loc, target)
	_ = a.s.ops.SetMetadata(op.Name, &arpb.OperationMetadata{})
	_ = a.s.ops.Succeed(op.Name, &emptypb.Empty{})
	return a.s.toProtoOperation(op.Name)
}

func versionOf(pkgName string, m registry.ManifestInfo, full bool) *arpb.Version {
	v := &arpb.Version{Name: pkgName + "/versions/" + m.Digest,
		CreateTime: timestamppb.New(m.Uploaded), UpdateTime: timestamppb.New(m.Uploaded)}
	if h, err := hex.DecodeString(strings.TrimPrefix(m.Digest, "sha256:")); err == nil {
		v.Fingerprints = []*arpb.Hash{{Type: arpb.Hash_SHA256, Value: h}}
	}
	decorate(v.Name, func(a map[string]string) { v.Annotations = a }, nil)
	if full {
		for _, t := range m.Tags {
			v.RelatedTags = append(v.RelatedTags, &arpb.Tag{Name: pkgName + "/tags/" + t, Version: v.Name})
		}
	}
	return v
}

// parseVersionName splits a version name into its package and digest.
func parseVersionName(name string) (pkgName, repoName, image, digest string, err error) {
	pkgName, digest, ok := splitChild(name, "versions")
	if !ok || !registry.ValidDigest(digest) {
		return "", "", "", "", apierror.InvalidArgument("invalid version name %q", name)
	}
	repoName, image, err = parsePackageName(pkgName)
	return pkgName, repoName, image, digest, err
}

// ListVersions lists an image's manifests as versions, newest first.
func (a *ArtifactRegistryServer) ListVersions(_ context.Context, req *arpb.ListVersionsRequest) (*arpb.ListVersionsResponse, error) {
	if isPackageRepo(req.GetParent()) {
		if err := noFilter(req.GetFilter(), req.GetOrderBy()); err != nil {
			return nil, err
		}
		return a.fmtListVersions(req)
	}
	if err := noFilter(req.GetFilter(), req.GetOrderBy()); err != nil {
		return nil, err
	}
	repoName, image, err := parsePackageName(req.GetParent())
	if err != nil {
		return nil, err
	}
	st, err := contentStore(repoName)
	if err != nil {
		return nil, err
	}
	ms, err := st.Manifests(repoOf(repoName), image)
	if err != nil {
		return nil, storeErr(err)
	}
	var all []*arpb.Version
	for _, m := range ms {
		all = append(all, versionOf(req.GetParent(), m, req.GetView() == arpb.VersionView_FULL))
	}
	page, next, err := pageOf(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &arpb.ListVersionsResponse{Versions: page, NextPageToken: next}, nil
}

// GetVersion reads one manifest as a version.
func (a *ArtifactRegistryServer) GetVersion(_ context.Context, req *arpb.GetVersionRequest) (*arpb.Version, error) {
	if isPackageRepo(req.GetName()) {
		return a.fmtGetVersion(req)
	}
	pkgName, repoName, image, digest, err := parseVersionName(req.GetName())
	if err != nil {
		return nil, err
	}
	st, err := contentStore(repoName)
	if err != nil {
		return nil, err
	}
	m, err := st.Manifest(repoOf(repoName), image, digest)
	if err != nil {
		return nil, storeErr(err)
	}
	return versionOf(pkgName, m, req.GetView() == arpb.VersionView_FULL), nil
}

// DeleteVersion deletes a manifest. A tagged one is refused unless force
// is set, which deletes its tags too, as Artifact Registry's does.
func (a *ArtifactRegistryServer) DeleteVersion(_ context.Context, req *arpb.DeleteVersionRequest) (*longrunningpb.Operation, error) {
	if isPackageRepo(req.GetName()) {
		return a.fmtDeleteVersion(req)
	}
	_, repoName, image, digest, err := parseVersionName(req.GetName())
	if err != nil {
		return nil, err
	}
	st, err := contentStore(repoName)
	if err != nil {
		return nil, err
	}
	m, err := st.Manifest(repoOf(repoName), image, digest)
	if err != nil {
		return nil, storeErr(err)
	}
	if len(m.Tags) > 0 && !req.GetForce() {
		return nil, apierror.FailedPrecondition("version %s has tags %s; delete them first, or set force",
			req.GetName(), strings.Join(m.Tags, ", "))
	}
	if err := st.DeleteManifest(repoOf(repoName), image, digest); err != nil {
		return nil, storeErr(err)
	}
	forgetUnder(req.GetName())
	return a.doneEmpty(repoName, req.GetName())
}

func parseTagName(name string) (pkgName, repoName, image, tag string, err error) {
	pkgName, tag, ok := splitChild(name, "tags")
	if !ok || !registry.ValidTag(tag) {
		return "", "", "", "", apierror.InvalidArgument("invalid tag name %q", name)
	}
	repoName, image, err = parsePackageName(pkgName)
	return pkgName, repoName, image, tag, err
}

// ListTags lists an image's tags, by name. The filter Artifact Registry
// documents for tags, version="<version name>", is applied.
func (a *ArtifactRegistryServer) ListTags(_ context.Context, req *arpb.ListTagsRequest) (*arpb.ListTagsResponse, error) {
	pkgRepo := isPackageRepo(req.GetParent())
	repoName, image, err := parsePackageName(req.GetParent())
	if err != nil && !pkgRepo {
		return nil, err
	}
	wantVersion := ""
	if f := strings.TrimSpace(req.GetFilter()); f != "" {
		v, ok := strings.CutPrefix(f, "version=")
		if !ok {
			return nil, apierror.Unimplemented("ListTags filter %q is not supported: only version=\"...\"", f)
		}
		wantVersion = strings.Trim(strings.TrimSpace(v), `"`)
	}
	if isPackageRepo(req.GetParent()) {
		return a.fmtListTags(req, wantVersion)
	}
	st, err := contentStore(repoName)
	if err != nil {
		return nil, err
	}
	tags, err := st.Tags(repoOf(repoName), image)
	if err != nil {
		return nil, storeErr(err)
	}
	names := make([]string, 0, len(tags))
	for t := range tags {
		names = append(names, t)
	}
	sort.Strings(names)
	var all []*arpb.Tag
	for _, t := range names {
		tag := &arpb.Tag{Name: req.GetParent() + "/tags/" + t, Version: req.GetParent() + "/versions/" + tags[t]}
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

// GetTag reads a tag.
func (a *ArtifactRegistryServer) GetTag(_ context.Context, req *arpb.GetTagRequest) (*arpb.Tag, error) {
	if isPackageRepo(req.GetName()) {
		return a.fmtGetTag(req.GetName())
	}
	pkgName, repoName, image, tag, err := parseTagName(req.GetName())
	if err != nil {
		return nil, err
	}
	st, err := contentStore(repoName)
	if err != nil {
		return nil, err
	}
	tags, err := st.Tags(repoOf(repoName), image)
	if err != nil {
		return nil, storeErr(err)
	}
	d, ok := tags[tag]
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return &arpb.Tag{Name: req.GetName(), Version: pkgName + "/versions/" + d}, nil
}

// setTag points a tag at a version of the same package.
func (a *ArtifactRegistryServer) setTag(pkgName, repoName, image, tag, version string) (*arpb.Tag, error) {
	vPkg, _, _, digest, err := parseVersionName(version)
	if err != nil {
		return nil, err
	}
	if vPkg != pkgName {
		return nil, apierror.InvalidArgument("tag version %q is not a version of %s", version, pkgName)
	}
	st, err := contentStore(repoName)
	if err != nil {
		return nil, err
	}
	if err := st.SetTag(repoOf(repoName), image, tag, digest); err != nil {
		return nil, storeErr(err)
	}
	return &arpb.Tag{Name: pkgName + "/tags/" + tag, Version: version}, nil
}

// CreateTag tags a version; an existing tag is refused.
func (a *ArtifactRegistryServer) CreateTag(ctx context.Context, req *arpb.CreateTagRequest) (*arpb.Tag, error) {
	if isPackageRepo(req.GetParent()) {
		return a.fmtSetTag(req.GetParent()+"/tags/"+esc(req.GetTagId()), req.GetTag().GetVersion(), true)
	}
	repoName, image, err := parsePackageName(req.GetParent())
	if err != nil {
		return nil, err
	}
	if !registry.ValidTag(req.GetTagId()) {
		return nil, apierror.InvalidArgument("invalid tag_id %q", req.GetTagId())
	}
	if _, err := a.GetTag(ctx, &arpb.GetTagRequest{Name: req.GetParent() + "/tags/" + req.GetTagId()}); err == nil {
		return nil, apierror.AlreadyExists("the tag already exists")
	}
	return a.setTag(req.GetParent(), repoName, image, req.GetTagId(), req.GetTag().GetVersion())
}

// UpdateTag moves a tag to another version; version is the one field.
func (a *ArtifactRegistryServer) UpdateTag(ctx context.Context, req *arpb.UpdateTagRequest) (*arpb.Tag, error) {
	for _, p := range req.GetUpdateMask().GetPaths() {
		if p != "version" {
			return nil, apierror.InvalidArgument("update_mask path %q is not a tag field: only version can change", p)
		}
	}
	if isPackageRepo(req.GetTag().GetName()) {
		return a.fmtSetTag(req.GetTag().GetName(), req.GetTag().GetVersion(), false)
	}
	pkgName, repoName, image, tag, err := parseTagName(req.GetTag().GetName())
	if err != nil {
		return nil, err
	}
	if _, err := a.GetTag(ctx, &arpb.GetTagRequest{Name: req.GetTag().GetName()}); err != nil {
		return nil, err
	}
	return a.setTag(pkgName, repoName, image, tag, req.GetTag().GetVersion())
}

// DeleteTag removes a tag; its version stays.
func (a *ArtifactRegistryServer) DeleteTag(_ context.Context, req *arpb.DeleteTagRequest) (*emptypb.Empty, error) {
	if isPackageRepo(req.GetName()) {
		return a.fmtDeleteTag(req.GetName())
	}
	_, repoName, image, tag, err := parseTagName(req.GetName())
	if err != nil {
		return nil, err
	}
	st, err := contentStore(repoName)
	if err != nil {
		return nil, err
	}
	if err := st.DeleteTag(repoOf(repoName), image, tag); err != nil {
		return nil, storeErr(err)
	}
	return &emptypb.Empty{}, nil
}

func dockerImageOf(repoName, image string, m registry.ManifestInfo) *arpb.DockerImage {
	r := repoOf(repoName)
	d := &arpb.DockerImage{Name: repoName + "/dockerImages/" + registry.EscapePackage(image) + "@" + m.Digest,
		Uri: registry.ImageURL(r, image) + "@" + m.Digest, Tags: m.Tags, ImageSizeBytes: m.ImageSize,
		UploadTime: timestamppb.New(m.Uploaded), UpdateTime: timestamppb.New(m.Uploaded), MediaType: m.MediaType}
	if m.Doc != nil {
		d.ArtifactType = m.Doc.ArtifactType
		for _, x := range m.Doc.Manifests {
			im := &arpb.ImageManifest{Digest: x.Digest, MediaType: x.MediaType}
			if p := x.Platform; p != nil {
				im.Architecture, im.Os, im.OsVersion, im.OsFeatures, im.Variant = p.Architecture, p.OS, p.OSVersion, p.OSFeatures, p.Variant
			}
			d.ImageManifests = append(d.ImageManifests, im)
		}
	}
	return d
}

// ListDockerImages lists every manifest of every image in a repository.
func (a *ArtifactRegistryServer) ListDockerImages(_ context.Context, req *arpb.ListDockerImagesRequest) (*arpb.ListDockerImagesResponse, error) {
	if isPackageRepo(req.GetParent()) {
		return nil, apierror.FailedPrecondition("%s is not a DOCKER repository", req.GetParent())
	}
	if req.GetOrderBy() != "" {
		return nil, apierror.Unimplemented("order_by is not supported")
	}
	st, err := contentStore(req.GetParent())
	if err != nil {
		return nil, err
	}
	var all []*arpb.DockerImage
	for _, im := range st.Images(repoOf(req.GetParent())) {
		ms, _ := st.Manifests(repoOf(req.GetParent()), im)
		for _, m := range ms {
			all = append(all, dockerImageOf(req.GetParent(), im, m))
		}
	}
	page, next, err := pageOf(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &arpb.ListDockerImagesResponse{DockerImages: page, NextPageToken: next}, nil
}

// GetDockerImage reads one, named {image}@{digest}.
func (a *ArtifactRegistryServer) GetDockerImage(_ context.Context, req *arpb.GetDockerImageRequest) (*arpb.DockerImage, error) {
	repoName, id, ok := splitChild(req.GetName(), "dockerImages")
	im, digest, ok2 := strings.Cut(id, "@")
	if !ok || !ok2 {
		return nil, apierror.InvalidArgument("invalid docker image name %q: want .../dockerImages/IMAGE@sha256:DIGEST", req.GetName())
	}
	image := registry.UnescapePackage(im)
	st, err := contentStore(repoName)
	if err != nil {
		return nil, err
	}
	m, err := st.Manifest(repoOf(repoName), image, digest)
	if err != nil {
		return nil, storeErr(err)
	}
	return dockerImageOf(repoName, image, m), nil
}

// repoFiles are a repository's files: every manifest, config and layer its
// images hold, each named by its digest and owned by the first version
// that holds it.
func repoFiles(st *registry.Store, repoName string) []*arpb.File {
	seen := map[string]*arpb.File{}
	var order []string
	add := func(digest string, size int64, owner string, at time.Time) {
		if _, ok := seen[digest]; ok || !registry.ValidDigest(digest) {
			return
		}
		f := &arpb.File{Name: repoName + "/files/" + digest, SizeBytes: size, Owner: owner,
			CreateTime: timestamppb.New(at), UpdateTime: timestamppb.New(at)}
		if h, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:")); err == nil {
			f.Hashes = []*arpb.Hash{{Type: arpb.Hash_SHA256, Value: h}}
		}
		seen[digest] = f
		order = append(order, digest)
	}
	for _, im := range st.Images(repoOf(repoName)) {
		ms, _ := st.Manifests(repoOf(repoName), im)
		pkg := repoName + "/packages/" + registry.EscapePackage(im)
		for _, m := range ms {
			owner := pkg + "/versions/" + m.Digest
			add(m.Digest, m.Size, owner, m.Uploaded)
			if m.Doc != nil && m.Doc.Config != nil {
				add(m.Doc.Config.Digest, m.Doc.Config.Size, owner, m.Uploaded)
			}
			if m.Doc != nil {
				for _, l := range m.Doc.Layers {
					add(l.Digest, l.Size, owner, m.Uploaded)
				}
			}
		}
	}
	sort.Strings(order)
	out := make([]*arpb.File, len(order))
	for i, d := range order {
		f := seen[d]
		decorate(f.Name, func(a map[string]string) { f.Annotations = a }, nil)
		out[i] = f
	}
	return out
}

// ListFiles lists a repository's files. The owner filter Artifact Registry
// documents, owner="<version name>", is applied.
func (a *ArtifactRegistryServer) ListFiles(_ context.Context, req *arpb.ListFilesRequest) (*arpb.ListFilesResponse, error) {
	if req.GetOrderBy() != "" {
		return nil, apierror.Unimplemented("order_by is not supported")
	}
	owner := ""
	if f := strings.TrimSpace(req.GetFilter()); f != "" {
		v, ok := strings.CutPrefix(f, "owner=")
		if !ok {
			return nil, apierror.Unimplemented("ListFiles filter %q is not supported: only owner=\"...\"", f)
		}
		owner = strings.Trim(strings.TrimSpace(v), `"`)
	}
	if isPackageRepo(req.GetParent()) {
		return a.fmtListFiles(req, owner)
	}
	st, err := contentStore(req.GetParent())
	if err != nil {
		return nil, err
	}
	var all []*arpb.File
	for _, f := range repoFiles(st, req.GetParent()) {
		if owner == "" || f.GetOwner() == owner {
			all = append(all, f)
		}
	}
	page, next, err := pageOf(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &arpb.ListFilesResponse{Files: page, NextPageToken: next}, nil
}

// GetFile reads one file, named by its digest.
func (a *ArtifactRegistryServer) GetFile(_ context.Context, req *arpb.GetFileRequest) (*arpb.File, error) {
	if isPackageRepo(req.GetName()) {
		return a.fmtGetFile(req.GetName())
	}
	repoName, _, ok := splitChild(req.GetName(), "files")
	if !ok {
		return nil, apierror.InvalidArgument("invalid file name %q", req.GetName())
	}
	st, err := contentStore(repoName)
	if err != nil {
		return nil, err
	}
	for _, f := range repoFiles(st, repoName) {
		if f.GetName() == req.GetName() {
			return f, nil
		}
	}
	return nil, apierror.NotFound("Requested entity was not found.")
}

// ImageSaver is an ImageBuilder that can read an image it built back, so
// a build's image is pushed to this instance's registry (#1383).
type ImageSaver interface {
	Save(ctx context.Context, local string) (v1.Image, func(), error)
}

// ParseImageURL splits an Artifact Registry image reference,
// {location}-docker.pkg.dev/{project}/{repository}/{image}[:tag], into its
// repository and image, and its tag (latest when it has none).
func ParseImageURL(ref string) (repo registry.Repo, image, tag string, ok bool) {
	host, rest, found := strings.Cut(ref, "/")
	loc := registry.LocationOf(host)
	if !found || loc == "" || strings.Contains(rest, "@") {
		return registry.Repo{}, "", "", false
	}
	tag = "latest"
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		rest, tag = rest[:i], rest[i+1:]
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || !registry.ValidImage(parts[2]) || !registry.ValidTag(tag) {
		return registry.Repo{}, "", "", false
	}
	return registry.Repo{Location: loc, Project: parts[0], Repository: parts[1]}, parts[2], tag, true
}

// pushBuilt pushes a locally built image to the registry under uri, as
// Cloud Build pushes a build's images, and returns its digest. pushed is
// false, with no error, when uri is not under a repository this instance
// holds or the builder cannot read images back: the image is then only
// loaded into the cluster, as before #1383, and its digest is derived.
func pushBuilt(ctx context.Context, builder ImageBuilder, local, uri string) (digest string, pushed bool, err error) {
	saver, ok := builder.(ImageSaver)
	st := RegistryStore()
	repo, image, tag, ok2 := ParseImageURL(uri)
	if !ok || st == nil || !ok2 {
		return "", false, nil
	}
	if _, found := lookupRegistryRepo(repo.Location, repo.Project, repo.Repository); !found {
		return "", false, nil
	}
	img, cleanup, err := saver.Save(ctx, local)
	if err != nil {
		return "", false, err
	}
	defer cleanup()
	d, err := st.WriteImage(repo, image, tag, img)
	if err != nil {
		return "", false, fmt.Errorf("push %s: %w", uri, err)
	}
	return d, true, nil
}

// RegistryImage resolves an image reference under a repository this
// instance's registry holds to its by-digest form, which a Cloud Run
// revision pulls through the nodes' registry mirror (#1383); ok is false
// for any other reference.
func RegistryImage(ref string) (string, bool) {
	st := RegistryStore()
	if st == nil {
		return "", false
	}
	host, _, _ := strings.Cut(ref, "/")
	if base, d, found := strings.Cut(ref, "@"); found {
		repo, image, _, ok := ParseImageURL(base)
		if !ok {
			return "", false
		}
		if _, found := lookupRegistryRepo(repo.Location, repo.Project, repo.Repository); !found {
			return "", false
		}
		if _, _, err := st.Resolve(repo, image, d); err != nil {
			return "", false
		}
		return ref, true
	}
	repo, image, tag, ok := ParseImageURL(ref)
	if !ok {
		return "", false
	}
	if _, found := lookupRegistryRepo(repo.Location, repo.Project, repo.Repository); !found {
		return "", false
	}
	d, _, err := st.Resolve(repo, image, tag)
	if err != nil {
		return "", false
	}
	return host + "/" + repo.Project + "/" + repo.Repository + "/" + image + "@" + d, true
}
