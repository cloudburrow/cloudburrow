package main

// The Artifact Registry repository's page (#1383): a DOCKER repository's
// images, an image's versions (manifests) and tags, a version's files, and
// Delete at each level, Add tag and Remove tag on a version: what the
// registry the repository serves holds, read from the same store the
// ArtifactRegistry API and the Docker Registry API read. A repository's
// rules and attachments, its location's VPC-SC policy, an image's display
// name and a version's annotations (#1449) are read and set through the
// same methods the API serves.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// repository finds the project's repository of that ID, refusing an ID in
// two locations, as Delete does.
func (p artifactRegistryProvider) repository(project, id string) (*arpb.Repository, registry.Repo, error) {
	if project == "" {
		project = p.defaultProject
	}
	var match []*arpb.Repository
	for _, r := range runadapter.Repositories() {
		parts := strings.Split(r.GetName(), "/")
		if len(parts) == 6 && parts[1] == project && parts[5] == id {
			match = append(match, r)
		}
	}
	switch len(match) {
	case 0:
		return nil, registry.Repo{}, fmt.Errorf("repository %q not found in project %s", id, project)
	case 1:
		parts := strings.Split(match[0].GetName(), "/")
		return match[0], registry.Repo{Location: parts[3], Project: parts[1], Repository: parts[5]}, nil
	}
	return nil, registry.Repo{}, fmt.Errorf("repository %q is in %d locations; use gcloud artifacts with --location", id, len(match))
}

// Detail opens a repository, an image in it, or one of its versions.
func (p artifactRegistryProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	r, repo, err := p.repository(project, path[0])
	if err != nil {
		return console.Detail{}, err
	}
	if r.GetFormat() != arpb.Repository_DOCKER {
		return p.packageRepoDetail(ctx, r, path)
	}
	st := runadapter.RegistryStore()
	if st == nil {
		return console.Detail{Unavailable: "this instance has no registry store"}, nil
	}
	switch len(path) {
	case 1:
		return p.repositoryDetail(st, r, repo), nil
	case 2:
		return p.imageDetail(st, repo, path[1])
	case 3:
		return p.versionDetail(st, repo, path[1], path[2])
	}
	return console.DeeperThan(3, path), nil
}

func (p artifactRegistryProvider) repositoryDetail(st *registry.Store, r *arpb.Repository, repo registry.Repo) console.Detail {
	images := console.Listing{Columns: []string{"Versions", "Tags", "Updated"}, NameColumn: "Image", Noun: "images",
		RowsOpenable: true}
	for _, im := range st.Images(repo) {
		ms, err := st.Manifests(repo, im)
		if err != nil || len(ms) == 0 {
			continue
		}
		tags := 0
		for _, m := range ms {
			tags += len(m.Tags)
		}
		images.Items = append(images.Items, console.Resource{Name: im, Fields: map[string]string{
			"Versions": strconv.Itoa(len(ms)), "Tags": strconv.Itoa(tags), "Updated": stampAge(timestamppb.New(ms[0].Uploaded))}})
	}
	images.Total = len(images.Items)
	policies := []string{}
	for id := range r.GetCleanupPolicies() {
		policies = append(policies, id)
	}
	sort.Strings(policies)
	cleanup := "none"
	if len(policies) > 0 {
		cleanup = strings.Join(policies, ", ")
		if r.GetCleanupPolicyDryRun() {
			cleanup += " (dry run)"
		}
	}
	ctx := context.Background()
	cs := runadapter.ConfigServer()
	rules := console.Listing{Columns: []string{"Action", "Operation", "Package", "Condition"}, NameColumn: "Rule", Noun: "rules",
		Note: "Rules are stored and read back, never enforced: this instance serves downloads to any caller, as its IAM policies are not enforced."}
	if rs, err := cs.ListRules(ctx, &arpb.ListRulesRequest{Parent: r.GetName()}); err == nil {
		for _, x := range rs.GetRules() {
			rules.Items = append(rules.Items, console.Resource{Name: x.GetName()[strings.LastIndex(x.GetName(), "/")+1:], Fields: map[string]string{
				"Action": x.GetAction().String(), "Operation": x.GetOperation().String(), "Package": orDash(x.GetPackageId()),
				"Condition": orDash(x.GetCondition().GetExpression())}})
		}
	}
	rules.Total = len(rules.Items)
	attachments := console.Listing{Columns: []string{"Type", "Target", "Files"}, NameColumn: "Attachment", Noun: "attachments"}
	if as, err := cs.ListAttachments(ctx, &arpb.ListAttachmentsRequest{Parent: r.GetName()}); err == nil {
		for _, x := range as.GetAttachments() {
			attachments.Items = append(attachments.Items, console.Resource{Name: x.GetName()[strings.LastIndex(x.GetName(), "/")+1:],
				Fields: map[string]string{"Type": x.GetType(), "Target": shortDigest(x.GetTarget()[strings.LastIndex(x.GetTarget(), "/")+1:]),
					"Files": strconv.Itoa(len(x.GetFiles()))}})
		}
	}
	attachments.Total = len(attachments.Items)
	vpcsc := "-"
	if c, err := cs.GetVPCSCConfig(ctx, &arpb.GetVPCSCConfigRequest{Name: "projects/" + repo.Project + "/locations/" + repo.Location + "/vpcscConfig"}); err == nil {
		vpcsc = c.GetVpcscPolicy().String()
	}
	platformLogs := "-"
	if st, sev := runadapter.ProjectPlatformLogs(repo.Project, repo.Location); st != "" || sev != "" {
		platformLogs = strings.Trim(st+", severity "+orDash(sev), ", ")
	}
	settings := "-"
	if ps, err := cs.GetProjectSettings(ctx, &arpb.GetProjectSettingsRequest{Name: "projects/" + repo.Project + "/projectSettings"}); err == nil {
		settings = fmt.Sprintf("%s, pull percent %d", ps.GetLegacyRedirectionState(), ps.GetPullPercent())
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Format", Value: r.GetFormat().String()}, {Label: "Mode", Value: dockerMode(r)}, {Label: "Location", Value: repo.Location},
			{Label: "URL", Value: r.GetRegistryUri()}, {Label: "Push to", Value: registryPushHint(repo)},
			{Label: "Description", Value: orDash(r.GetDescription())}, {Label: "Cleanup policies", Value: cleanup},
			{Label: "VPC-SC policy (location)", Value: vpcsc}, {Label: "Project settings", Value: settings},
			{Label: "Platform logs (location)", Value: platformLogs}},
		Sections: []console.Section{{ID: "images", Label: "Images", Listing: images}, {ID: "rules", Label: "Rules", Listing: rules},
			{ID: "attachments", Label: "Attachments", Listing: attachments}},
	}
}

// dockerMode is a DOCKER repository's mode, with a remote one's upstream.
func dockerMode(r *arpb.Repository) string {
	if u := r.GetRemoteRepositoryConfig().GetCommonRepository().GetUri(); u != "" {
		return r.GetMode().String() + ": pulls through from " + u
	}
	return r.GetMode().String()
}

// registryPushHint is where a local docker client pushes the repository's
// images: the Run port under {location}-docker.pkg.localhost.
func registryPushHint(repo registry.Repo) string {
	return repo.Location + "-docker.pkg.localhost:<run port>/" + repo.Project + "/" + repo.Repository + "/IMAGE:TAG"
}

func (p artifactRegistryProvider) imageDetail(st *registry.Store, repo registry.Repo, image string) (console.Detail, error) {
	ms, err := st.Manifests(repo, image)
	if err != nil || len(ms) == 0 {
		return console.Detail{}, fmt.Errorf("image %q not found in repository %s", image, repo.Repository)
	}
	versions := console.Listing{Columns: []string{"Tags", "Size", "Media type", "Uploaded"}, NameColumn: "Digest",
		Noun: "versions", RowsOpenable: true}
	tags := console.Listing{Columns: []string{"Version"}, NameColumn: "Tag", Noun: "tags"}
	for _, m := range ms {
		versions.Items = append(versions.Items, console.Resource{Name: m.Digest, Fields: map[string]string{
			"Tags": orDash(strings.Join(m.Tags, ", ")), "Size": humanBytes(m.ImageSize), "Media type": m.MediaType,
			"Uploaded": stampAge(timestamppb.New(m.Uploaded))}})
		for _, t := range m.Tags {
			tags.Items = append(tags.Items, console.Resource{Name: t, Fields: map[string]string{"Version": shortDigest(m.Digest)}})
		}
	}
	sort.Slice(tags.Items, func(i, j int) bool { return tags.Items[i].Name < tags.Items[j].Name })
	versions.Total, tags.Total = len(versions.Items), len(tags.Items)
	display := image
	if pk, err := runadapter.ConfigServer().GetPackage(context.Background(), &arpb.GetPackageRequest{Name: packageName(repo, image)}); err == nil {
		display = pk.GetDisplayName()
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Image", Value: registry.ImageURL(repo, image)}, {Label: "Display name", Value: display},
			{Label: "Versions", Value: strconv.Itoa(len(ms))}},
		Sections: []console.Section{{ID: "versions", Label: "Versions", Listing: versions}, {ID: "tags", Label: "Tags", Listing: tags}},
	}, nil
}

func (p artifactRegistryProvider) versionDetail(st *registry.Store, repo registry.Repo, image, digest string) (console.Detail, error) {
	m, err := st.Manifest(repo, image, digest)
	if err != nil {
		return console.Detail{}, fmt.Errorf("version %s of %s not found", digest, image)
	}
	files := console.Listing{Columns: []string{"Kind", "Media type", "Size", "Annotations"}, NameColumn: "Digest", Noun: "files"}
	repoName := "projects/" + repo.Project + "/locations/" + repo.Location + "/repositories/" + repo.Repository
	add := func(kind string, d registry.Descriptor) {
		ann := "-"
		if f, err := runadapter.ConfigServer().GetFile(context.Background(), &arpb.GetFileRequest{Name: repoName + "/files/" + d.Digest}); err == nil {
			ann = formatAnnotations(f.GetAnnotations())
		}
		files.Items = append(files.Items, console.Resource{Name: d.Digest, Fields: map[string]string{
			"Kind": kind, "Media type": orDash(d.MediaType), "Size": humanBytes(d.Size), "Annotations": ann}})
	}
	if m.Doc != nil {
		if m.Doc.Config != nil {
			add("config", *m.Doc.Config)
		}
		for _, l := range m.Doc.Layers {
			add("layer", l)
		}
		for _, x := range m.Doc.Manifests {
			kind := "manifest"
			if x.Platform != nil {
				kind = "manifest (" + x.Platform.OS + "/" + x.Platform.Architecture + ")"
			}
			add(kind, x)
		}
	}
	files.Total = len(files.Items)
	annotations := "-"
	if v, err := runadapter.ConfigServer().GetVersion(context.Background(), &arpb.GetVersionRequest{Name: packageName(repo, image) + "/versions/" + m.Digest}); err == nil {
		annotations = formatAnnotations(v.GetAnnotations())
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Pull by digest", Value: registry.ImageURL(repo, image) + "@" + m.Digest},
			{Label: "Annotations", Value: annotations},
			{Label: "Tags", Value: orDash(strings.Join(m.Tags, ", "))}, {Label: "Media type", Value: m.MediaType},
			{Label: "Size", Value: humanBytes(m.ImageSize)}, {Label: "Uploaded", Value: stampAge(timestamppb.New(m.Uploaded))}},
		Sections: []console.Section{{ID: "files", Label: "Files", Listing: files}},
	}, nil
}

// DetailActions are Delete at each level, and Add tag and Remove tag on a
// version.
func (p artifactRegistryProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if r, _, err := p.repository(project, path[0]); err == nil && r.GetFormat() != arpb.Repository_DOCKER && len(path) > 1 {
		return packageRepoActions(r, path)
	}
	switch len(path) {
	case 1:
		var acts []console.Action
		if r, _, err := p.repository(project, path[0]); err == nil {
			acts = append(acts, editRepositoryAction(r), editCleanupAction(r))
			if a, ok := editRuleAction(ctx, r.GetName()); ok {
				acts = append(acts, a)
			}
			if a, ok := importAction(r); ok {
				acts = append(acts, a)
			}
		}
		return append(acts, []console.Action{
			{ID: "add-rule", Label: "Add rule", Fields: []console.Field{
				{Name: "id", Label: "Rule ID", Type: "text", Required: true, Pattern: "^[a-z0-9]([a-z0-9._\\-]{0,126}[a-z0-9])?$"},
				{Name: "action", Label: "Action", Type: "select", Options: []string{"DENY", "ALLOW"}, Default: "DENY"},
				{Name: "package", Label: "Package", Type: "text", Help: "optional: the image the rule applies to; empty is every image"},
				{Name: "condition", Label: "Condition", Type: "text", Help: "optional: a CEL expression, as CreateRule takes"}}},
			{ID: "delete-rule", Label: "Delete rule", Destructive: true, Fields: []console.Field{
				{Name: "id", Label: "Rule ID", Type: "text", Required: true}}},
			{ID: "delete-attachment", Label: "Delete attachment", Destructive: true, Fields: []console.Field{
				{Name: "id", Label: "Attachment ID", Type: "text", Required: true, Help: "its files stay"}}},
			{ID: "pull-percent", Label: "Set project pull percent", Fields: []console.Field{
				{Name: "percent", Label: "Pull percent", Type: "number", Default: "0",
					Help: "UpdateProjectSettings' pull_percent; gcr.io redirection stays off, as nothing here serves gcr.io"}}},
			{ID: "vpcsc", Label: "Set VPC-SC policy", Fields: []console.Field{
				{Name: "policy", Label: "Policy", Type: "select", Options: []string{"DENY", "ALLOW"}, Default: "DENY",
					Help: "the location's policy for remote repositories' upstream fetches, as UpdateVPCSCConfig sets it"}}},
			{ID: "platform-logs", Label: "Set platform logs", Fields: []console.Field{
				{Name: "state", Label: "Logging state", Type: "select", Options: []string{"ENABLED", "DISABLED", "LOGGING_STATE_UNSPECIFIED"}, Default: "ENABLED",
					Help: "the location's projectConfig platformLogsConfig, as updateProjectConfig sets it (#1519); stored: this instance writes no platform logs"},
				{Name: "severity", Label: "Severity level", Type: "select", Options: []string{"SEVERITY_LEVEL_UNSPECIFIED", "DEBUG", "INFO", "NOTICE", "WARNING", "ERROR", "CRITICAL", "ALERT", "EMERGENCY"},
					Default: "SEVERITY_LEVEL_UNSPECIFIED"}}},
			{ID: "delete", Label: "Delete repository", Destructive: true, Leaves: true,
				Confirm: "The repository and every image it holds are deleted, as DeleteRepository does."}}...)
	case 2:
		return []console.Action{
			{ID: "display-name", Label: "Set display name", Fields: []console.Field{
				{Name: "name", Label: "Display name", Type: "text", Help: "empty restores the image's name"}}},
			{ID: "delete-untagged", Label: "Delete untagged versions", Destructive: true,
				Confirm: "Every version with no tag is deleted at once, as BatchDeleteVersions does."},
			{ID: "delete", Label: "Delete image", Destructive: true, Leaves: true,
				Confirm: "Every version and tag of the image is deleted, as DeletePackage does."}}
	case 3:
		return []console.Action{
			{ID: "annotate", Label: "Set annotations", Fields: []console.Field{{Name: "annotations", Label: "Annotations", Type: "text",
				Help: "key=value pairs, comma separated, replacing the version's annotations; empty clears them"}}},
			{ID: "annotate-file", Label: "Set file annotations", Fields: []console.Field{
				{Name: "file", Label: "File digest", Type: "text", Required: true, Help: "one of the Files below, sha256:..."},
				{Name: "annotations", Label: "Annotations", Type: "text", Help: "key=value pairs, comma separated; empty clears them"}}},
			{ID: "attach", Label: "Add attachment", Fields: []console.Field{
				{Name: "id", Label: "Attachment ID", Type: "text", Required: true},
				{Name: "type", Label: "Type", Type: "text", Required: true, Default: "application/vnd.spdx+json"},
				{Name: "file", Label: "File digest", Type: "text", Required: true, Help: "a file of this repository, sha256:..."}}},
			{ID: "export", Label: "Export to Cloud Storage", Fields: []console.Field{
				{Name: "path", Label: "Destination", Type: "text", Required: true, Help: "bucket or bucket/dir in this instance's Cloud Storage"}}},
			{ID: "add-tag", Label: "Add tag", Fields: []console.Field{{Name: "tag", Label: "Tag", Type: "text", Required: true,
				Help: "a new tag, or an existing one to move to this version"}}},
			{ID: "remove-tag", Label: "Remove tag", Fields: []console.Field{{Name: "tag", Label: "Tag", Type: "text", Required: true,
				Help: "one of this version's tags; the version stays"}}},
			{ID: "delete", Label: "Delete version", Destructive: true, Leaves: true,
				Confirm: "The version and its tags are deleted, as DeleteVersion with force does."},
		}
	}
	return nil
}

// ActAt performs one of DetailActions.
func (p artifactRegistryProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) == 1 && action == "delete" {
		return p.Delete(ctx, project, path[0])
	}
	r, repo, err := p.repository(project, path[0])
	if err != nil {
		return err
	}
	if len(path) == 1 && action == "edit-repository" {
		return p.editRepository(ctx, r.GetName(), values)
	}
	if len(path) == 1 && action == "edit-cleanup" {
		return p.editCleanup(ctx, r.GetName(), values)
	}
	if len(path) == 1 && action == "edit-rule" {
		return editRule(ctx, r.GetName(), values)
	}
	if r.GetFormat() != arpb.Repository_DOCKER && len(path) > 1 {
		return packageRepoAct(ctx, r, path, action, values)
	}
	if len(path) == 1 && action == "import" {
		return importPackages(ctx, r, values["uris"])
	}
	st := runadapter.RegistryStore()
	if st == nil {
		return fmt.Errorf("this instance has no registry store")
	}
	cs, repoName := runadapter.ConfigServer(), "projects/"+repo.Project+"/locations/"+repo.Location+"/repositories/"+repo.Repository
	switch {
	case len(path) == 1 && action == "add-rule":
		rule := &arpb.Rule{Action: arpb.Rule_Action(arpb.Rule_Action_value[values["action"]]), Operation: arpb.Rule_DOWNLOAD,
			PackageId: strings.TrimSpace(values["package"])}
		if c := strings.TrimSpace(values["condition"]); c != "" {
			rule.Condition = &expr.Expr{Expression: c}
		}
		_, err := cs.CreateRule(ctx, &arpb.CreateRuleRequest{Parent: repoName, RuleId: strings.TrimSpace(values["id"]), Rule: rule})
		return err
	case len(path) == 1 && action == "delete-rule":
		_, err := cs.DeleteRule(ctx, &arpb.DeleteRuleRequest{Name: repoName + "/rules/" + strings.TrimSpace(values["id"])})
		return err
	case len(path) == 1 && action == "delete-attachment":
		_, err := cs.DeleteAttachment(ctx, &arpb.DeleteAttachmentRequest{Name: repoName + "/attachments/" + strings.TrimSpace(values["id"])})
		return err
	case len(path) == 1 && action == "pull-percent":
		n, err := strconv.Atoi(strings.TrimSpace(values["percent"]))
		if err != nil {
			return fmt.Errorf("pull percent %q is not a number", values["percent"])
		}
		_, err = cs.UpdateProjectSettings(ctx, &arpb.UpdateProjectSettingsRequest{ProjectSettings: &arpb.ProjectSettings{
			Name: "projects/" + repo.Project + "/projectSettings", PullPercent: int32(n)}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"pull_percent"}}})
		return err
	case len(path) == 2 && action == "delete-untagged":
		ms, err := st.Manifests(repo, path[1])
		if err != nil {
			return err
		}
		var names []string
		for _, m := range ms {
			if len(m.Tags) == 0 {
				names = append(names, packageName(repo, path[1])+"/versions/"+m.Digest)
			}
		}
		if len(names) == 0 {
			return fmt.Errorf("every version of %s is tagged", path[1])
		}
		_, err = cs.BatchDeleteVersions(ctx, &arpb.BatchDeleteVersionsRequest{Parent: packageName(repo, path[1]), Names: names})
		return err
	case len(path) == 3 && action == "annotate-file":
		a, err := parseAnnotations(values["annotations"])
		if err != nil {
			return err
		}
		_, err = cs.UpdateFile(ctx, &arpb.UpdateFileRequest{File: &arpb.File{Name: repoName + "/files/" + strings.TrimSpace(values["file"]), Annotations: a},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"annotations"}}})
		return err
	case len(path) == 3 && action == "attach":
		_, err := cs.CreateAttachment(ctx, &arpb.CreateAttachmentRequest{Parent: repoName, AttachmentId: strings.TrimSpace(values["id"]),
			Attachment: &arpb.Attachment{Target: packageName(repo, path[1]) + "/versions/" + path[2], Type: strings.TrimSpace(values["type"]),
				Files: []string{repoName + "/files/" + strings.TrimSpace(values["file"])}}})
		return err
	case len(path) == 3 && action == "export":
		_, err := cs.ExportArtifact(ctx, &arpb.ExportArtifactRequest{Repository: repoName,
			SourceArtifact: &arpb.ExportArtifactRequest_SourceVersion{SourceVersion: packageName(repo, path[1]) + "/versions/" + path[2]},
			Destination:    &arpb.ExportArtifactRequest_GcsPath{GcsPath: strings.TrimSpace(values["path"])}})
		return err
	case len(path) == 1 && action == "platform-logs":
		return runadapter.SetProjectPlatformLogs(repo.Project, repo.Location, values["state"], values["severity"])
	case len(path) == 1 && action == "vpcsc":
		_, err := cs.UpdateVPCSCConfig(ctx, &arpb.UpdateVPCSCConfigRequest{VpcscConfig: &arpb.VPCSCConfig{
			Name:        "projects/" + repo.Project + "/locations/" + repo.Location + "/vpcscConfig",
			VpcscPolicy: arpb.VPCSCConfig_VPCSCPolicy(arpb.VPCSCConfig_VPCSCPolicy_value[values["policy"]])}})
		return err
	case len(path) == 2 && action == "display-name":
		_, err := cs.UpdatePackage(ctx, &arpb.UpdatePackageRequest{Package: &arpb.Package{Name: packageName(repo, path[1]),
			DisplayName: strings.TrimSpace(values["name"])}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
		return err
	case len(path) == 3 && action == "annotate":
		a, err := parseAnnotations(values["annotations"])
		if err != nil {
			return err
		}
		_, err = cs.UpdateVersion(ctx, &arpb.UpdateVersionRequest{Version: &arpb.Version{Name: packageName(repo, path[1]) + "/versions/" + path[2],
			Annotations: a}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"annotations"}}})
		return err
	case len(path) == 2 && action == "delete":
		return st.DeleteImage(repo, path[1])
	case len(path) == 3 && action == "delete":
		return st.DeleteManifest(repo, path[1], path[2])
	case len(path) == 3 && action == "add-tag":
		tag := strings.TrimSpace(values["tag"])
		if !registry.ValidTag(tag) {
			return fmt.Errorf("%q is not a valid tag", tag)
		}
		return st.SetTag(repo, path[1], tag, path[2])
	case len(path) == 3 && action == "remove-tag":
		tag := strings.TrimSpace(values["tag"])
		tags, err := st.Tags(repo, path[1])
		if err != nil {
			return err
		}
		if tags[tag] != path[2] {
			return fmt.Errorf("%q is not a tag of this version", tag)
		}
		return st.DeleteTag(repo, path[1], tag)
	}
	return fmt.Errorf("unknown action %q", action)
}

// packageName is an image's Artifact Registry package name.
func packageName(repo registry.Repo, image string) string {
	return "projects/" + repo.Project + "/locations/" + repo.Location + "/repositories/" + repo.Repository +
		"/packages/" + registry.EscapePackage(image)
}

// parseAnnotations reads comma-separated key=value pairs.
func parseAnnotations(s string) (map[string]string, error) {
	a := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("annotation %q must be key=value", kv)
		}
		a[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return a, nil
}

// formatAnnotations is key=value pairs, sorted, or a dash.
func formatAnnotations(a map[string]string) string {
	if len(a) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		keys[i] = k + "=" + a[k]
	}
	return strings.Join(keys, ", ")
}

var (
	_ console.Driller   = artifactRegistryProvider{}
	_ console.PathActor = artifactRegistryProvider{}
	_ console.Deleter   = artifactRegistryProvider{}
)
