package run

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/registry"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Artifact Registry's configuration and metadata methods (#1449): a
// repository's rules and attachments, a project's settings and a
// location's VPC Service Controls config, which are stored, and the
// annotations and display names UpdatePackage, UpdateVersion and
// UpdateFile set on the registry's content. Rules, like IAM policies
// (ADR-0006), are stored and never enforced: this instance has no callers
// to deny. They are saved beside the repositories and removed with them.

// arConfigFile is where they are saved, beside repositoriesFile.
const arConfigFile = "artifactregistry-config.json"

var arConfig = struct {
	mu          sync.Mutex
	rules       map[string]*arpb.Rule
	attachments map[string]*arpb.Attachment
	settings    map[string]*arpb.ProjectSettings
	vpcsc       map[string]*arpb.VPCSCConfig
	// annotations are a package's, version's or file's, by its name.
	annotations map[string]map[string]string
	// displayNames are packages' display names set by UpdatePackage.
	displayNames map[string]string
	// projectConfigs are locations' projectConfig (#1519), by name.
	projectConfigs map[string]*arProjectConfig
}{rules: map[string]*arpb.Rule{}, attachments: map[string]*arpb.Attachment{}, settings: map[string]*arpb.ProjectSettings{},
	vpcsc: map[string]*arpb.VPCSCConfig{}, annotations: map[string]map[string]string{}, displayNames: map[string]string{},
	projectConfigs: map[string]*arProjectConfig{}}

type arConfigState struct {
	Rules        map[string]json.RawMessage   `json:"rules,omitempty"`
	Attachments  map[string]json.RawMessage   `json:"attachments,omitempty"`
	Settings     map[string]json.RawMessage   `json:"settings,omitempty"`
	VPCSC        map[string]json.RawMessage   `json:"vpcsc,omitempty"`
	Annotations  map[string]map[string]string `json:"annotations,omitempty"`
	DisplayNames map[string]string            `json:"displayNames,omitempty"`
	// ProjectConfigs are hand-served JSON resources (#1519).
	ProjectConfigs map[string]*arProjectConfig `json:"projectConfigs,omitempty"`
}

func marshalAll[T proto.Message](m map[string]T) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range m {
		if b, err := protojson.Marshal(v); err == nil {
			out[k] = b
		}
	}
	return out
}

func unmarshalAll[T proto.Message](in map[string]json.RawMessage, dst map[string]T, mk func() T) {
	for k, raw := range in {
		v := mk()
		if protojson.Unmarshal(raw, v) == nil {
			dst[k] = v
		}
	}
}

// saveARConfigLocked writes the configuration; with no store it is kept in
// memory only.
func saveARConfigLocked() {
	st := RegistryStore()
	if st == nil {
		return
	}
	b, err := json.Marshal(arConfigState{Rules: marshalAll(arConfig.rules), Attachments: marshalAll(arConfig.attachments),
		Settings: marshalAll(arConfig.settings), VPCSC: marshalAll(arConfig.vpcsc),
		Annotations: arConfig.annotations, DisplayNames: arConfig.displayNames, ProjectConfigs: arConfig.projectConfigs})
	if err != nil {
		return
	}
	_ = st.WriteState(arConfigFile, b)
}

// loadARConfig reads what a previous run saved.
func loadARConfig(st *registry.Store) {
	b, err := st.ReadState(arConfigFile)
	if err != nil {
		return
	}
	var s arConfigState
	if json.Unmarshal(b, &s) != nil {
		return
	}
	arConfig.mu.Lock()
	defer arConfig.mu.Unlock()
	unmarshalAll(s.Rules, arConfig.rules, func() *arpb.Rule { return &arpb.Rule{} })
	unmarshalAll(s.Attachments, arConfig.attachments, func() *arpb.Attachment { return &arpb.Attachment{} })
	unmarshalAll(s.Settings, arConfig.settings, func() *arpb.ProjectSettings { return &arpb.ProjectSettings{} })
	unmarshalAll(s.VPCSC, arConfig.vpcsc, func() *arpb.VPCSCConfig { return &arpb.VPCSCConfig{} })
	for k, v := range s.Annotations {
		arConfig.annotations[k] = v
	}
	for k, v := range s.DisplayNames {
		arConfig.displayNames[k] = v
	}
	for k, v := range s.ProjectConfigs {
		arConfig.projectConfigs[k] = v
	}
}

// forgetUnder drops everything stored for name and the resources under it:
// a deleted repository's rules and attachments, or a deleted package's or
// version's annotations.
func forgetUnder(name string) {
	arConfig.mu.Lock()
	defer arConfig.mu.Unlock()
	under := func(k string) bool { return k == name || strings.HasPrefix(k, name+"/") }
	changed := false
	for k := range arConfig.rules {
		if under(k) {
			delete(arConfig.rules, k)
			changed = true
		}
	}
	for k, a := range arConfig.attachments {
		if under(k) || under(a.GetTarget()) {
			delete(arConfig.attachments, k)
			changed = true
		}
	}
	for k := range arConfig.annotations {
		if under(k) {
			delete(arConfig.annotations, k)
			changed = true
		}
	}
	for k := range arConfig.displayNames {
		if under(k) {
			delete(arConfig.displayNames, k)
			changed = true
		}
	}
	if changed {
		saveARConfigLocked()
	}
}

// decorate sets the stored annotations, and a package's display name, on
// a package, version or file read from the registry.
func decorate(name string, set func(map[string]string), display func(string)) {
	arConfig.mu.Lock()
	defer arConfig.mu.Unlock()
	if a, ok := arConfig.annotations[name]; ok && len(a) > 0 {
		cp := make(map[string]string, len(a))
		for k, v := range a {
			cp[k] = v
		}
		set(cp)
	}
	if display != nil {
		if d, ok := arConfig.displayNames[name]; ok {
			display(d)
		}
	}
}

// annotationKey is Artifact Registry's annotation key shape: an optional
// DNS prefix and a slash, then a name of at most 63 characters.
var annotationKey = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)

func checkAnnotations(a map[string]string) error {
	total := 0
	for k, v := range a {
		if !annotationKey.MatchString(k) {
			return apierror.InvalidArgument("invalid annotation key %q", k)
		}
		total += len(k) + len(v)
	}
	if total > 256*1024 {
		return apierror.InvalidArgument("annotations total %d bytes, more than 256 KiB", total)
	}
	return nil
}

// onlyPaths checks an update mask names only the allowed paths; an empty
// mask is all of them.
func onlyPaths(paths []string, allowed ...string) ([]string, error) {
	if len(paths) == 0 {
		return allowed, nil
	}
	for _, p := range paths {
		ok := false
		for _, a := range allowed {
			ok = ok || p == a
		}
		if !ok {
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated: only %s can", p, strings.Join(allowed, ", "))
		}
	}
	return paths, nil
}

// UpdatePackage sets a package's display_name and annotations.
func (a *ArtifactRegistryServer) UpdatePackage(ctx context.Context, req *arpb.UpdatePackageRequest) (*arpb.Package, error) {
	in := req.GetPackage()
	paths, err := onlyPaths(req.GetUpdateMask().GetPaths(), "display_name", "annotations")
	if err != nil {
		return nil, err
	}
	if _, err := a.GetPackage(ctx, &arpb.GetPackageRequest{Name: in.GetName()}); err != nil {
		return nil, err
	}
	if err := checkAnnotations(in.GetAnnotations()); err != nil {
		return nil, err
	}
	arConfig.mu.Lock()
	for _, p := range paths {
		switch p {
		case "display_name":
			if in.GetDisplayName() == "" {
				delete(arConfig.displayNames, in.GetName())
			} else {
				arConfig.displayNames[in.GetName()] = in.GetDisplayName()
			}
		case "annotations":
			setAnnotationsLocked(in.GetName(), in.GetAnnotations())
		}
	}
	saveARConfigLocked()
	arConfig.mu.Unlock()
	return a.GetPackage(ctx, &arpb.GetPackageRequest{Name: in.GetName()})
}

func setAnnotationsLocked(name string, a map[string]string) {
	if len(a) == 0 {
		delete(arConfig.annotations, name)
		return
	}
	cp := make(map[string]string, len(a))
	for k, v := range a {
		cp[k] = v
	}
	arConfig.annotations[name] = cp
}

// UpdateVersion sets a version's annotations, the one field a version's
// owner sets; the rest come from its manifest.
func (a *ArtifactRegistryServer) UpdateVersion(ctx context.Context, req *arpb.UpdateVersionRequest) (*arpb.Version, error) {
	in := req.GetVersion()
	if _, err := onlyPaths(req.GetUpdateMask().GetPaths(), "annotations"); err != nil {
		return nil, err
	}
	if _, err := a.GetVersion(ctx, &arpb.GetVersionRequest{Name: in.GetName()}); err != nil {
		return nil, err
	}
	if err := checkAnnotations(in.GetAnnotations()); err != nil {
		return nil, err
	}
	arConfig.mu.Lock()
	setAnnotationsLocked(in.GetName(), in.GetAnnotations())
	saveARConfigLocked()
	arConfig.mu.Unlock()
	return a.GetVersion(ctx, &arpb.GetVersionRequest{Name: in.GetName(), View: arpb.VersionView_FULL})
}

// UpdateFile sets a file's annotations.
func (a *ArtifactRegistryServer) UpdateFile(ctx context.Context, req *arpb.UpdateFileRequest) (*arpb.File, error) {
	in := req.GetFile()
	if _, err := onlyPaths(req.GetUpdateMask().GetPaths(), "annotations"); err != nil {
		return nil, err
	}
	if _, err := a.GetFile(ctx, &arpb.GetFileRequest{Name: in.GetName()}); err != nil {
		return nil, err
	}
	if err := checkAnnotations(in.GetAnnotations()); err != nil {
		return nil, err
	}
	arConfig.mu.Lock()
	setAnnotationsLocked(in.GetName(), in.GetAnnotations())
	saveARConfigLocked()
	arConfig.mu.Unlock()
	return a.GetFile(ctx, &arpb.GetFileRequest{Name: in.GetName()})
}

// DeleteFile is refused for a DOCKER repository, as on Google: deleting a
// file is allowed only in a GENERIC repository, and a Docker image's
// blobs go when the versions holding them are deleted.
func (a *ArtifactRegistryServer) DeleteFile(ctx context.Context, req *arpb.DeleteFileRequest) (*longrunningpb.Operation, error) {
	if _, err := a.GetFile(ctx, &arpb.GetFileRequest{Name: req.GetName()}); err != nil {
		return nil, err
	}
	return nil, apierror.FailedPrecondition("DeleteFile is only allowed on generic repositories; delete the version that holds %s instead", req.GetName())
}

// BatchDeleteVersions deletes versions of one package, with their tags, as
// Artifact Registry's does. validate_only checks each exists and deletes
// nothing. A version that cannot be deleted is named in the operation's
// metadata.
func (a *ArtifactRegistryServer) BatchDeleteVersions(ctx context.Context, req *arpb.BatchDeleteVersionsRequest) (*longrunningpb.Operation, error) {
	if len(req.GetNames()) == 0 {
		return nil, apierror.InvalidArgument("names is required")
	}
	repoName := ""
	for _, n := range req.GetNames() {
		pkg, r, _, _, err := parseVersionName(n)
		if err != nil {
			return nil, err
		}
		if req.GetParent() != "" && pkg != req.GetParent() {
			return nil, apierror.InvalidArgument("version %q is not in package %q", n, req.GetParent())
		}
		if repoName != "" && r != repoName {
			return nil, apierror.InvalidArgument("all versions must be in one repository")
		}
		repoName = r
		if _, err := a.GetVersion(ctx, &arpb.GetVersionRequest{Name: n}); err != nil {
			return nil, err
		}
	}
	meta := &arpb.BatchDeleteVersionsMetadata{}
	if !req.GetValidateOnly() {
		for _, n := range req.GetNames() {
			if err := deleteVersionNow(n); err != nil {
				meta.FailedVersions = append(meta.FailedVersions, n)
			}
		}
	}
	if a.s == nil {
		if len(meta.FailedVersions) > 0 {
			return nil, apierror.Internal(nil, "could not delete %s", strings.Join(meta.FailedVersions, ", "))
		}
		return nil, nil // the console's call (ConfigServer)
	}
	loc := repoName[:strings.Index(repoName, "/repositories/")]
	op := a.s.ops.Create(loc, repoName)
	_ = a.s.ops.SetMetadata(op.Name, meta)
	_ = a.s.ops.Succeed(op.Name, &emptypb.Empty{})
	return a.s.toProtoOperation(op.Name)
}

// ruleID and attachmentID are the IDs Artifact Registry accepts.
var childID = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$`)

func repoExists(name string) error {
	if _, _, err := parseRepositoryName(name); err != nil {
		return err
	}
	repositories.mu.Lock()
	_, ok := repositories.repos[name]
	repositories.mu.Unlock()
	if !ok {
		return apierror.NotFound("Requested entity was not found.")
	}
	return nil
}

func checkRule(r *arpb.Rule) error {
	if r.GetAction() == arpb.Rule_ACTION_UNSPECIFIED {
		return apierror.InvalidArgument("rule action is required: ALLOW or DENY")
	}
	if r.GetOperation() == arpb.Rule_OPERATION_UNSPECIFIED {
		return apierror.InvalidArgument("rule operation is required: DOWNLOAD")
	}
	if r.GetPackageId() != "" && !registry.ValidImage(r.GetPackageId()) {
		return apierror.InvalidArgument("invalid package_id %q", r.GetPackageId())
	}
	return nil
}

// CreateRule stores a rule. Nothing enforces it: this instance serves
// downloads to any caller, as its IAM policies are not enforced.
func (a *ArtifactRegistryServer) CreateRule(_ context.Context, req *arpb.CreateRuleRequest) (*arpb.Rule, error) {
	if err := repoExists(req.GetParent()); err != nil {
		return nil, err
	}
	if !childID.MatchString(req.GetRuleId()) {
		return nil, apierror.InvalidArgument("invalid rule_id %q", req.GetRuleId())
	}
	r := proto.Clone(req.GetRule()).(*arpb.Rule)
	if r == nil {
		r = &arpb.Rule{}
	}
	if err := checkRule(r); err != nil {
		return nil, err
	}
	r.Name = req.GetParent() + "/rules/" + req.GetRuleId()
	arConfig.mu.Lock()
	defer arConfig.mu.Unlock()
	if _, ok := arConfig.rules[r.Name]; ok {
		return nil, apierror.AlreadyExists("the rule already exists")
	}
	arConfig.rules[r.Name] = r
	saveARConfigLocked()
	return proto.Clone(r).(*arpb.Rule), nil
}

// GetRule reads a rule.
func (a *ArtifactRegistryServer) GetRule(_ context.Context, req *arpb.GetRuleRequest) (*arpb.Rule, error) {
	arConfig.mu.Lock()
	defer arConfig.mu.Unlock()
	r, ok := arConfig.rules[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return proto.Clone(r).(*arpb.Rule), nil
}

// ListRules lists a repository's rules, by name.
func (a *ArtifactRegistryServer) ListRules(_ context.Context, req *arpb.ListRulesRequest) (*arpb.ListRulesResponse, error) {
	if err := repoExists(req.GetParent()); err != nil {
		return nil, err
	}
	arConfig.mu.Lock()
	var all []*arpb.Rule
	for n, r := range arConfig.rules {
		if strings.HasPrefix(n, req.GetParent()+"/rules/") {
			all = append(all, proto.Clone(r).(*arpb.Rule))
		}
	}
	arConfig.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	page, next, err := pageOf(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &arpb.ListRulesResponse{Rules: page, NextPageToken: next}, nil
}

// UpdateRule changes a rule's action, operation, condition or package_id.
func (a *ArtifactRegistryServer) UpdateRule(_ context.Context, req *arpb.UpdateRuleRequest) (*arpb.Rule, error) {
	in := req.GetRule()
	paths, err := onlyPaths(req.GetUpdateMask().GetPaths(), "action", "operation", "condition", "package_id")
	if err != nil {
		return nil, err
	}
	arConfig.mu.Lock()
	defer arConfig.mu.Unlock()
	cur, ok := arConfig.rules[in.GetName()]
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	r := proto.Clone(cur).(*arpb.Rule)
	for _, p := range paths {
		switch p {
		case "action":
			r.Action = in.GetAction()
		case "operation":
			r.Operation = in.GetOperation()
		case "condition":
			r.Condition = in.GetCondition()
		case "package_id":
			r.PackageId = in.GetPackageId()
		}
	}
	if err := checkRule(r); err != nil {
		return nil, err
	}
	arConfig.rules[r.Name] = r
	saveARConfigLocked()
	return proto.Clone(r).(*arpb.Rule), nil
}

// DeleteRule removes a rule.
func (a *ArtifactRegistryServer) DeleteRule(_ context.Context, req *arpb.DeleteRuleRequest) (*emptypb.Empty, error) {
	arConfig.mu.Lock()
	defer arConfig.mu.Unlock()
	if _, ok := arConfig.rules[req.GetName()]; !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	delete(arConfig.rules, req.GetName())
	saveARConfigLocked()
	return &emptypb.Empty{}, nil
}

// CreateAttachment stores an attachment: metadata of type type linking
// files of the repository to a target version in it, as an SBOM or a
// signature is attached to an image. Its files and target must exist.
func (a *ArtifactRegistryServer) CreateAttachment(ctx context.Context, req *arpb.CreateAttachmentRequest) (*longrunningpb.Operation, error) {
	if err := repoExists(req.GetParent()); err != nil {
		return nil, err
	}
	if !childID.MatchString(req.GetAttachmentId()) {
		return nil, apierror.InvalidArgument("invalid attachment_id %q", req.GetAttachmentId())
	}
	at := proto.Clone(req.GetAttachment()).(*arpb.Attachment)
	if at == nil {
		at = &arpb.Attachment{}
	}
	if at.GetTarget() == "" || at.GetType() == "" || len(at.GetFiles()) == 0 {
		return nil, apierror.InvalidArgument("attachment target, type and files are required")
	}
	if _, r, _, _, err := parseVersionName(at.GetTarget()); err != nil || r != req.GetParent() {
		return nil, apierror.InvalidArgument("attachment target %q must be a version in %s", at.GetTarget(), req.GetParent())
	}
	if _, err := a.GetVersion(ctx, &arpb.GetVersionRequest{Name: at.GetTarget()}); err != nil {
		return nil, apierror.InvalidArgument("attachment target %s does not exist", at.GetTarget())
	}
	for _, f := range at.GetFiles() {
		if r, _, _ := splitChild(f, "files"); r != req.GetParent() {
			return nil, apierror.InvalidArgument("attachment file %q must be a file in %s", f, req.GetParent())
		}
		if _, err := a.GetFile(ctx, &arpb.GetFileRequest{Name: f}); err != nil {
			return nil, apierror.InvalidArgument("attachment file %s does not exist", f)
		}
	}
	if err := checkAnnotations(at.GetAnnotations()); err != nil {
		return nil, err
	}
	now := timestamppb.New(time.Now())
	at.Name, at.CreateTime, at.UpdateTime = req.GetParent()+"/attachments/"+req.GetAttachmentId(), now, now
	arConfig.mu.Lock()
	if _, ok := arConfig.attachments[at.Name]; ok {
		arConfig.mu.Unlock()
		return nil, apierror.AlreadyExists("the attachment already exists")
	}
	arConfig.attachments[at.Name] = at
	saveARConfigLocked()
	arConfig.mu.Unlock()
	if a.s == nil {
		return nil, nil // the console's call (ConfigServer): stored, no operation
	}
	loc := req.GetParent()[:strings.Index(req.GetParent(), "/repositories/")]
	op := a.s.ops.Create(loc, at.Name)
	_ = a.s.ops.SetMetadata(op.Name, &arpb.OperationMetadata{})
	_ = a.s.ops.Succeed(op.Name, proto.Clone(at))
	return a.s.toProtoOperation(op.Name)
}

// GetAttachment reads an attachment.
func (a *ArtifactRegistryServer) GetAttachment(_ context.Context, req *arpb.GetAttachmentRequest) (*arpb.Attachment, error) {
	arConfig.mu.Lock()
	defer arConfig.mu.Unlock()
	at, ok := arConfig.attachments[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return proto.Clone(at).(*arpb.Attachment), nil
}

// attachmentFilter is the filter Artifact Registry documents for
// attachments: target="..." and type="...", joined by AND.
func attachmentFilter(f string) (map[string]string, error) {
	out := map[string]string{}
	f = strings.TrimSpace(f)
	if f == "" {
		return out, nil
	}
	for _, term := range regexp.MustCompile(`\s+AND\s+`).Split(f, -1) {
		k, v, ok := strings.Cut(strings.TrimSpace(term), "=")
		k = strings.TrimSpace(k)
		if !ok || (k != "target" && k != "type") {
			return nil, apierror.InvalidArgument("invalid filter %q: only target=\"...\" and type=\"...\" are supported", f)
		}
		out[k] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out, nil
}

// ListAttachments lists a repository's attachments, by name.
func (a *ArtifactRegistryServer) ListAttachments(_ context.Context, req *arpb.ListAttachmentsRequest) (*arpb.ListAttachmentsResponse, error) {
	if err := repoExists(req.GetParent()); err != nil {
		return nil, err
	}
	want, err := attachmentFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	arConfig.mu.Lock()
	var all []*arpb.Attachment
	for n, at := range arConfig.attachments {
		if !strings.HasPrefix(n, req.GetParent()+"/attachments/") {
			continue
		}
		if t, ok := want["target"]; ok && at.GetTarget() != t {
			continue
		}
		if t, ok := want["type"]; ok && at.GetType() != t {
			continue
		}
		all = append(all, proto.Clone(at).(*arpb.Attachment))
	}
	arConfig.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	page, next, err := pageOf(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &arpb.ListAttachmentsResponse{Attachments: page, NextPageToken: next}, nil
}

// DeleteAttachment removes an attachment; its files stay.
func (a *ArtifactRegistryServer) DeleteAttachment(_ context.Context, req *arpb.DeleteAttachmentRequest) (*longrunningpb.Operation, error) {
	arConfig.mu.Lock()
	if _, ok := arConfig.attachments[req.GetName()]; !ok {
		arConfig.mu.Unlock()
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	delete(arConfig.attachments, req.GetName())
	saveARConfigLocked()
	arConfig.mu.Unlock()
	if a.s == nil {
		return nil, nil // the console's call (ConfigServer)
	}
	repoName, _, _ := splitChild(req.GetName(), "attachments")
	return a.doneEmpty(repoName, req.GetName())
}

// projectSettingsName checks projects/{project}/projectSettings.
func projectSettingsName(name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] != "projects" || parts[1] == "" || parts[2] != "projectSettings" {
		return apierror.InvalidArgument("invalid project settings name %q: want projects/*/projectSettings", name)
	}
	return nil
}

// GetProjectSettings reads a project's settings. A project's gcr.io
// traffic is not redirected, as nothing here serves gcr.io.
func (a *ArtifactRegistryServer) GetProjectSettings(_ context.Context, req *arpb.GetProjectSettingsRequest) (*arpb.ProjectSettings, error) {
	if err := projectSettingsName(req.GetName()); err != nil {
		return nil, err
	}
	arConfig.mu.Lock()
	defer arConfig.mu.Unlock()
	if s, ok := arConfig.settings[req.GetName()]; ok {
		return proto.Clone(s).(*arpb.ProjectSettings), nil
	}
	return &arpb.ProjectSettings{Name: req.GetName(), LegacyRedirectionState: arpb.ProjectSettings_REDIRECTION_FROM_GCR_IO_DISABLED}, nil
}

// UpdateProjectSettings stores legacy_redirection_state and pull_percent.
// Redirecting gcr.io to Artifact Registry is refused: this instance does
// not serve gcr.io, so there is no Container Registry traffic to redirect.
func (a *ArtifactRegistryServer) UpdateProjectSettings(ctx context.Context, req *arpb.UpdateProjectSettingsRequest) (*arpb.ProjectSettings, error) {
	in := req.GetProjectSettings()
	if err := projectSettingsName(in.GetName()); err != nil {
		return nil, err
	}
	paths, err := onlyPaths(req.GetUpdateMask().GetPaths(), "legacy_redirection_state", "pull_percent")
	if err != nil {
		return nil, err
	}
	cur, _ := a.GetProjectSettings(ctx, &arpb.GetProjectSettingsRequest{Name: in.GetName()})
	for _, p := range paths {
		switch p {
		case "legacy_redirection_state":
			switch in.GetLegacyRedirectionState() {
			case arpb.ProjectSettings_REDIRECTION_FROM_GCR_IO_DISABLED, arpb.ProjectSettings_REDIRECTION_STATE_UNSPECIFIED:
				cur.LegacyRedirectionState = arpb.ProjectSettings_REDIRECTION_FROM_GCR_IO_DISABLED
			default:
				return nil, apierror.Unimplemented("legacy_redirection_state %v is not supported: this instance does not serve gcr.io, "+
					"so there is no Container Registry traffic to redirect", in.GetLegacyRedirectionState())
			}
		case "pull_percent":
			if in.GetPullPercent() < 0 || in.GetPullPercent() > 100 {
				return nil, apierror.InvalidArgument("pull_percent must be between 0 and 100")
			}
			cur.PullPercent = in.GetPullPercent()
		}
	}
	arConfig.mu.Lock()
	arConfig.settings[in.GetName()] = cur
	saveARConfigLocked()
	arConfig.mu.Unlock()
	return proto.Clone(cur).(*arpb.ProjectSettings), nil
}

// vpcscConfigName checks projects/{project}/locations/{location}/vpcscConfig.
func vpcscConfigName(name string) error {
	loc, ok := strings.CutSuffix(name, "/vpcscConfig")
	if _, _, err := resource.ParseLocation(loc); !ok || err != nil {
		return apierror.InvalidArgument("invalid VPC-SC config name %q: want projects/*/locations/*/vpcscConfig", name)
	}
	return nil
}

// GetVPCSCConfig reads a location's VPC Service Controls config: DENY,
// Google's default, until it is changed. It governs only remote
// repositories' upstream fetches inside a perimeter; it is stored here,
// where there are neither.
func (a *ArtifactRegistryServer) GetVPCSCConfig(_ context.Context, req *arpb.GetVPCSCConfigRequest) (*arpb.VPCSCConfig, error) {
	if err := vpcscConfigName(req.GetName()); err != nil {
		return nil, err
	}
	arConfig.mu.Lock()
	defer arConfig.mu.Unlock()
	if c, ok := arConfig.vpcsc[req.GetName()]; ok {
		return proto.Clone(c).(*arpb.VPCSCConfig), nil
	}
	return &arpb.VPCSCConfig{Name: req.GetName(), VpcscPolicy: arpb.VPCSCConfig_DENY}, nil
}

// UpdateVPCSCConfig stores vpcsc_policy, DENY or ALLOW.
func (a *ArtifactRegistryServer) UpdateVPCSCConfig(_ context.Context, req *arpb.UpdateVPCSCConfigRequest) (*arpb.VPCSCConfig, error) {
	in := req.GetVpcscConfig()
	if err := vpcscConfigName(in.GetName()); err != nil {
		return nil, err
	}
	if _, err := onlyPaths(req.GetUpdateMask().GetPaths(), "vpcsc_policy"); err != nil {
		return nil, err
	}
	if in.GetVpcscPolicy() == arpb.VPCSCConfig_VPCSC_POLICY_UNSPECIFIED {
		return nil, apierror.InvalidArgument("vpcsc_policy is required: DENY or ALLOW")
	}
	c := &arpb.VPCSCConfig{Name: in.GetName(), VpcscPolicy: in.GetVpcscPolicy()}
	arConfig.mu.Lock()
	arConfig.vpcsc[c.Name] = c
	saveARConfigLocked()
	arConfig.mu.Unlock()
	return proto.Clone(c).(*arpb.VPCSCConfig), nil
}

// The methods below are refused with the reason: they serve formats or
// sources this instance has no repositories for.

const noFormat = "only DOCKER repositories are served: CreateRepository refuses %s, so no repository here holds them"

// ListMavenArtifacts is refused: no MAVEN repository can exist here.
func (a *ArtifactRegistryServer) ListMavenArtifacts(context.Context, *arpb.ListMavenArtifactsRequest) (*arpb.ListMavenArtifactsResponse, error) {
	return nil, apierror.Unimplemented(noFormat, "MAVEN")
}

// GetMavenArtifact is refused: no MAVEN repository can exist here.
func (a *ArtifactRegistryServer) GetMavenArtifact(context.Context, *arpb.GetMavenArtifactRequest) (*arpb.MavenArtifact, error) {
	return nil, apierror.Unimplemented(noFormat, "MAVEN")
}

// ListNpmPackages is refused: no NPM repository can exist here.
func (a *ArtifactRegistryServer) ListNpmPackages(context.Context, *arpb.ListNpmPackagesRequest) (*arpb.ListNpmPackagesResponse, error) {
	return nil, apierror.Unimplemented(noFormat, "NPM")
}

// GetNpmPackage is refused: no NPM repository can exist here.
func (a *ArtifactRegistryServer) GetNpmPackage(context.Context, *arpb.GetNpmPackageRequest) (*arpb.NpmPackage, error) {
	return nil, apierror.Unimplemented(noFormat, "NPM")
}

// ListPythonPackages is refused: no PYTHON repository can exist here.
func (a *ArtifactRegistryServer) ListPythonPackages(context.Context, *arpb.ListPythonPackagesRequest) (*arpb.ListPythonPackagesResponse, error) {
	return nil, apierror.Unimplemented(noFormat, "PYTHON")
}

// GetPythonPackage is refused: no PYTHON repository can exist here.
func (a *ArtifactRegistryServer) GetPythonPackage(context.Context, *arpb.GetPythonPackageRequest) (*arpb.PythonPackage, error) {
	return nil, apierror.Unimplemented(noFormat, "PYTHON")
}

// ImportAptArtifacts is refused: no APT repository can exist here.
func (a *ArtifactRegistryServer) ImportAptArtifacts(context.Context, *arpb.ImportAptArtifactsRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented(noFormat, "APT")
}

// ImportYumArtifacts is refused: no YUM repository can exist here.
func (a *ArtifactRegistryServer) ImportYumArtifacts(context.Context, *arpb.ImportYumArtifactsRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented(noFormat, "YUM")
}

// ConfigServer serves the methods above for the console: those returning
// an operation (CreateAttachment, DeleteAttachment, BatchDeleteVersions,
// ExportArtifact) do their work and return no operation on it.
// LastExport is the files the latest ExportArtifact wrote.
func ConfigServer() *ArtifactRegistryServer { return &ArtifactRegistryServer{} }

var lastExport atomic.Pointer[arpb.ExportArtifactMetadata]

// LastExport is the metadata of the latest ExportArtifact, for the console.
func LastExport() *arpb.ExportArtifactMetadata { return lastExport.Load() }

// deleteVersionNow deletes a version and its tags, as DeleteVersion with
// force does, with no operation.
func deleteVersionNow(name string) error {
	_, repoName, image, digest, err := parseVersionName(name)
	if err != nil {
		return err
	}
	st, err := contentStore(repoName)
	if err != nil {
		return err
	}
	if err := st.DeleteManifest(repoOf(repoName), image, digest); err != nil {
		return storeErr(err)
	}
	forgetUnder(name)
	return nil
}

// exportWriter writes ExportArtifact's files to the instance's Cloud
// Storage; nil when Cloud Storage is not served.
var exportWriter struct {
	mu sync.Mutex
	w  ObjectWriter
}

// UseExportWriter sets the Cloud Storage writer ExportArtifact uses.
func UseExportWriter(w ObjectWriter) {
	exportWriter.mu.Lock()
	exportWriter.w = w
	exportWriter.mu.Unlock()
}

// ExportArtifact copies a version's files, named by version or by tag, to
// gcs_path (bucket[/dir]) in this instance's Cloud Storage, one object per
// file named by its ID, and returns its operation, already done, with the
// exported files in its metadata. The bucket must exist, as on Google.
func (a *ArtifactRegistryServer) ExportArtifact(ctx context.Context, req *arpb.ExportArtifactRequest) (*longrunningpb.Operation, error) {
	exportWriter.mu.Lock()
	w := exportWriter.w
	exportWriter.mu.Unlock()
	if w == nil {
		return nil, apierror.FailedPrecondition("ExportArtifact writes to Cloud Storage, which is not enabled on this instance: start it with storage in --services")
	}
	if err := repoExists(req.GetRepository()); err != nil {
		return nil, err
	}
	version := req.GetSourceVersion()
	if tag := req.GetSourceTag(); tag != "" {
		t, err := a.GetTag(ctx, &arpb.GetTagRequest{Name: tag})
		if err != nil {
			return nil, err
		}
		version = t.GetVersion()
	}
	if version == "" {
		return nil, apierror.InvalidArgument("source_version or source_tag is required")
	}
	if _, r, _, _, err := parseVersionName(version); err != nil || r != req.GetRepository() {
		return nil, apierror.InvalidArgument("source %q must be a version in %s", version, req.GetRepository())
	}
	v, err := a.GetVersion(ctx, &arpb.GetVersionRequest{Name: version})
	if err != nil {
		return nil, err
	}
	bucket, dir, _ := strings.Cut(strings.Trim(strings.TrimPrefix(req.GetGcsPath(), "gs://"), "/"), "/")
	if bucket == "" {
		return nil, apierror.InvalidArgument("gcs_path is required: bucket, or bucket/dir")
	}
	st := RegistryStore()
	project := strings.Split(req.GetRepository(), "/")[1]
	meta := &arpb.ExportArtifactMetadata{}
	defer func() { lastExport.Store(meta) }()
	for _, f := range repoFiles(st, req.GetRepository()) {
		if f.GetOwner() != version {
			continue
		}
		_, id, _ := splitChild(f.GetName(), "files")
		b, err := st.ReadBlob(id)
		if err != nil {
			if mb, _, _, merr := st.GetManifest(repoOf(req.GetRepository()), registry.UnescapePackage(pkgIDOf(version)), id); merr == nil {
				b = mb
			} else {
				return nil, apierror.Internal(err, "read file %s", f.GetName())
			}
		}
		object := id
		if dir != "" {
			object = dir + "/" + id
		}
		if err := w.Put(ctx, project, bucket, object, b, false); err != nil {
			return nil, apierror.FailedPrecondition("write gs://%s/%s: %v", bucket, object, err)
		}
		meta.ExportedFiles = append(meta.ExportedFiles, &arpb.ExportArtifactMetadata_ExportedFile{Name: f.GetName(), Hashes: f.GetHashes(),
			Destination: &arpb.ExportArtifactMetadata_ExportedFile_GcsObjectPath{GcsObjectPath: bucket + "/" + object}})
	}
	if a.s == nil {
		return nil, nil // the console's call (ConfigServer)
	}
	loc := req.GetRepository()[:strings.Index(req.GetRepository(), "/repositories/")]
	op := a.s.ops.Create(loc, req.GetRepository())
	_ = a.s.ops.SetMetadata(op.Name, meta)
	_ = a.s.ops.Succeed(op.Name, &arpb.ExportArtifactResponse{ExportedVersion: v})
	return a.s.toProtoOperation(op.Name)
}

// pkgIDOf is a version name's package ID.
func pkgIDOf(version string) string {
	pkg, _, _ := splitChild(version, "versions")
	_, id, _ := splitChild(pkg, "packages")
	return id
}
