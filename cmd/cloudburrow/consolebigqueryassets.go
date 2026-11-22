package main

// BigQuery Studio's code assets over Dataform (#2094): saved queries,
// notebooks and data canvases, kept as Google keeps them, each a Dataform
// repository labelled single-file-asset-type (sql, notebook, data_canvas;
// https://docs.cloud.google.com/dataform/docs/manage-repository, read
// 2026-11-21), hidden from the Dataform page as Google hides them, and
// arranged in Dataform folders and team folders in Studio's Files pane.
// Each save is a commit on the asset's repository, so Version history is
// FetchRepositoryHistory and a version is ReadRepositoryFile at its commit;
// Share > Manage permissions is the repository's IAM policy, with Google's
// Code Owner, Code Editor and Code Viewer roles
// (https://docs.cloud.google.com/bigquery/docs/work-with-saved-queries).
// Source control lists the project's other repositories, their
// workspaces and files.
//
// Google does not document the file inside an asset repository; this
// Studio writes content.sql, content.ipynb and content.canvas.json.
//
// The screen reaches these through POST /api/studio/dataform.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
)

// dataformAssetLabel is the label Google's Studio puts on a code asset's
// repository.
const dataformAssetLabel = "single-file-asset-type"

// dataformAssetFiles are the asset types Studio keeps and the file each
// keeps its content in.
var dataformAssetFiles = map[string]string{
	"sql":         "content.sql",
	"notebook":    "content.ipynb",
	"data_canvas": "content.canvas.json",
	// A pipeline and a data preparation (#2105) keep the editor's model
	// here and the Dataform project it describes beside it
	// (consolebigqueryorchestration.go).
	"pipeline":         "content.pipeline.json",
	"data_preparation": "content.dataprep.json",
}

// dataformAssetLimit bounds an asset's content, as a request body is.
const dataformAssetLimit = 1 << 17

// dataformIsAsset reports whether a repository is a Studio code asset,
// which the Dataform page leaves out, as Google's does.
func dataformIsAsset(r *dfpb.Repository) bool {
	return r.GetLabels()[dataformAssetLabel] != ""
}

// studioEntry is one row of the Files pane: a folder, a team folder or a
// code asset, under parent ("" for the user's root).
type studioEntry struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Parent      string `json:"parent"`
	Location    string `json:"location"`
	Created     string `json:"created,omitempty"`
}

type studioRepo struct {
	Name       string   `json:"name"`
	Location   string   `json:"location"`
	Remote     string   `json:"remote,omitempty"`
	Workspaces []string `json:"workspaces"`
}

type studioAssetReq struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	DisplayName string `json:"displayName"`
	Location    string `json:"location"`
	Parent      string `json:"parent"`
	Content     string `json:"content"`
	Commit      string `json:"commit"`
	Role        string `json:"role"`
	Member      string `json:"member"`
	Workspace   string `json:"workspace"`
	Path        string `json:"path"`
}

// StudioCall answers Studio's code-asset calls.
func (p dataformProvider) StudioCall(ctx context.Context, project, call string, body json.RawMessage) (any, error) {
	s, err := p.api()
	if err != nil {
		return nil, err
	}
	if project == "" {
		return nil, errors.New("choose a project first")
	}
	if out, ok, err := studioOrchestration(ctx, s, project, call, body); ok {
		return out, err
	}
	var in studioAssetReq
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("a code asset request: %w", err)
		}
	}
	if in.Name != "" {
		if err := kmsOwned(project, in.Name); err != nil {
			return nil, err
		}
	}
	if in.Parent != "" {
		if err := kmsOwned(project, in.Parent); err != nil {
			return nil, err
		}
	}
	switch call {
	case "files":
		return studioFiles(ctx, s, project)
	case "read":
		return studioReadAsset(ctx, s, in)
	case "save":
		return studioSaveAsset(ctx, s, project, in)
	case "history":
		return studioHistory(ctx, s, in.Name)
	case "rename":
		return studioRename(ctx, s, in)
	case "move":
		dest := in.Parent
		if _, err := s.MoveRepository(ctx, &dfpb.MoveRepositoryRequest{Name: in.Name, DestinationContainingFolder: &dest}); err != nil {
			return nil, err
		}
		return map[string]string{"moved": in.Name}, nil
	case "delete":
		if _, err := s.DeleteRepository(ctx, &dfpb.DeleteRepositoryRequest{Name: in.Name, Force: true}); err != nil {
			return nil, err
		}
		return map[string]string{"deleted": in.Name}, nil
	case "createfolder":
		loc := in.Location
		if loc == "" {
			loc = "us-central1"
		}
		f, err := s.CreateFolder(ctx, &dfpb.CreateFolderRequest{Parent: "projects/" + project + "/locations/" + loc,
			Folder: &dfpb.Folder{DisplayName: strings.TrimSpace(in.DisplayName), ContainingFolder: in.Parent}})
		if err != nil {
			return nil, err
		}
		return studioEntry{Kind: "folder", Name: f.GetName(), DisplayName: f.GetDisplayName(), Parent: f.GetContainingFolder(), Location: loc}, nil
	case "deletefolder":
		if strings.Contains(in.Name, "/teamFolders/") {
			_, err = s.DeleteTeamFolderTree(ctx, &dfpb.DeleteTeamFolderTreeRequest{Name: in.Name, Force: true})
		} else {
			_, err = s.DeleteFolderTree(ctx, &dfpb.DeleteFolderTreeRequest{Name: in.Name, Force: true})
		}
		if err != nil {
			return nil, err
		}
		return map[string]string{"deleted": in.Name}, nil
	case "getiam":
		pol, err := s.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: in.Name})
		if err != nil {
			return nil, err
		}
		return studioPolicy(pol), nil
	case "grant", "revoke":
		return studioChangeIAM(ctx, s, call, in)
	case "repositories":
		return studioRepositories(ctx, s, project)
	case "repofiles":
		return studioRepoFiles(ctx, s, in)
	case "readrepofile":
		ws, err := dataformChild(in.Name, "workspaces", in.Workspace)
		if err != nil {
			return nil, err
		}
		r, err := s.ReadFile(ctx, &dfpb.ReadFileRequest{Workspace: ws, Path: in.Path})
		if err != nil {
			return nil, err
		}
		return map[string]string{"content": string(r.GetFileContents())}, nil
	case "writerepofile":
		ws, err := dataformChild(in.Name, "workspaces", in.Workspace)
		if err != nil {
			return nil, err
		}
		if len(in.Content) > dataformAssetLimit {
			return nil, fmt.Errorf("a file here is at most %d bytes", dataformAssetLimit)
		}
		if _, err := s.WriteFile(ctx, &dfpb.WriteFileRequest{Workspace: ws, Path: in.Path, Contents: []byte(in.Content)}); err != nil {
			return nil, err
		}
		return map[string]string{"written": in.Path}, nil
	}
	return nil, fmt.Errorf("unknown studio call %q", call)
}

// studioFiles is every folder, team folder and code asset of the project,
// in every Dataform region, each under its parent.
func studioFiles(ctx context.Context, s *dataform.Server, project string) (any, error) {
	entries := []studioEntry{}
	for _, loc := range dataform.Locations() {
		parent := "projects/" + project + "/locations/" + loc
		var queue []string
		add := func(container string, f *dfpb.Folder, r *dfpb.Repository) {
			switch {
			case f != nil:
				entries = append(entries, studioEntry{Kind: "folder", Name: f.GetName(), DisplayName: f.GetDisplayName(), Parent: container, Location: loc, Created: kmsTime(f.GetCreateTime())})
				queue = append(queue, f.GetName())
			case r != nil && dataformIsAsset(r):
				entries = append(entries, studioEntry{Kind: r.GetLabels()[dataformAssetLabel], Name: r.GetName(), DisplayName: studioDisplay(r), Parent: container, Location: loc, Created: kmsTime(r.GetCreateTime())})
			}
		}
		root, err := s.QueryUserRootContents(ctx, &dfpb.QueryUserRootContentsRequest{Location: parent, PageSize: 1000})
		if err != nil {
			return nil, err
		}
		for _, e := range root.GetEntries() {
			add("", e.GetFolder(), e.GetRepository())
		}
		teams, err := s.SearchTeamFolders(ctx, &dfpb.SearchTeamFoldersRequest{Location: parent, PageSize: 1000})
		if err != nil {
			return nil, err
		}
		for _, t := range teams.GetResults() {
			tf := t.GetTeamFolder()
			if tf == nil {
				continue
			}
			entries = append(entries, studioEntry{Kind: "teamfolder", Name: tf.GetName(), DisplayName: tf.GetDisplayName(), Location: loc, Created: kmsTime(tf.GetCreateTime())})
			c, err := s.QueryTeamFolderContents(ctx, &dfpb.QueryTeamFolderContentsRequest{TeamFolder: tf.GetName(), PageSize: 1000})
			if err != nil {
				return nil, err
			}
			for _, e := range c.GetEntries() {
				add(tf.GetName(), e.GetFolder(), e.GetRepository())
			}
		}
		for len(queue) > 0 {
			f := queue[0]
			queue = queue[1:]
			c, err := s.QueryFolderContents(ctx, &dfpb.QueryFolderContentsRequest{Folder: f, PageSize: 1000})
			if err != nil {
				return nil, err
			}
			for _, e := range c.GetEntries() {
				add(f, e.GetFolder(), e.GetRepository())
			}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].DisplayName) < strings.ToLower(entries[j].DisplayName)
	})
	return map[string]any{"entries": entries, "locations": dataform.Locations()}, nil
}

func studioDisplay(r *dfpb.Repository) string {
	if d := r.GetDisplayName(); d != "" {
		return d
	}
	return lastSegment(r.GetName())
}

func studioAsset(ctx context.Context, s *dataform.Server, name string) (*dfpb.Repository, string, error) {
	r, err := s.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: name})
	if err != nil {
		return nil, "", err
	}
	file, ok := dataformAssetFiles[r.GetLabels()[dataformAssetLabel]]
	if !ok {
		return nil, "", fmt.Errorf("%s is not a saved query, notebook, data canvas, pipeline or data preparation", name)
	}
	return r, file, nil
}

// studioReadAsset is the asset's content at its head, or at a commit of
// its Version history.
func studioReadAsset(ctx context.Context, s *dataform.Server, in studioAssetReq) (any, error) {
	r, file, err := studioAsset(ctx, s, in.Name)
	if err != nil {
		return nil, err
	}
	content := ""
	f, err := s.ReadRepositoryFile(ctx, &dfpb.ReadRepositoryFileRequest{Name: r.GetName(), CommitSha: in.Commit, Path: file})
	if err == nil {
		content = string(f.GetContents())
	} else if in.Commit != "" {
		return nil, err
	}
	return map[string]any{"name": r.GetName(), "kind": r.GetLabels()[dataformAssetLabel], "displayName": studioDisplay(r),
		"location": kmsLocation(r.GetName()), "parent": r.GetContainingFolder(), "content": content}, nil
}

// studioSaveAsset is Save: a new asset (a repository with its label, its
// display name and folder, the user made its admin as Google's Studio
// does) or a new version of the one named, a commit of its file.
func studioSaveAsset(ctx context.Context, s *dataform.Server, project string, in studioAssetReq) (any, error) {
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if len(in.Content) > dataformAssetLimit {
		return nil, fmt.Errorf("a code asset is at most %d bytes", dataformAssetLimit)
	}
	name := in.Name
	if name == "" {
		_, ok := dataformAssetFiles[in.Kind]
		if !ok {
			return nil, fmt.Errorf("%q is not a code asset type: sql, notebook, data_canvas, pipeline or data_preparation", in.Kind)
		}
		if in.DisplayName == "" {
			return nil, errors.New("a code asset needs a name")
		}
		if studioGeneratedKinds[in.Kind] {
			// Checked before the repository is made, so a model that does
			// not compile leaves nothing behind.
			if _, err := studioGeneratedFiles(project, in.Kind, in.Content); err != nil {
				return nil, err
			}
		}
		if len(in.DisplayName) > 256 {
			return nil, errors.New("a code asset's name is at most 256 characters")
		}
		loc := in.Location
		if loc == "" {
			loc = "us-central1"
		}
		r := &dfpb.Repository{DisplayName: in.DisplayName, Labels: map[string]string{dataformAssetLabel: in.Kind}, SetAuthenticatedUserAdmin: true}
		if in.Parent != "" {
			r.ContainingFolder = proto.String(in.Parent)
		}
		out, err := s.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: "projects/" + project + "/locations/" + loc,
			RepositoryId: newAssetID(), Repository: r})
		if err != nil {
			return nil, err
		}
		name = out.GetName()
	} else if in.DisplayName != "" {
		if _, err := studioRename(ctx, s, in); err != nil {
			return nil, err
		}
	}
	r, file, err := studioAsset(ctx, s, name)
	if err != nil {
		return nil, err
	}
	kind := r.GetLabels()[dataformAssetLabel]
	ops := map[string]*dfpb.CommitRepositoryChangesRequest_FileOperation{}
	if studioGeneratedKinds[kind] {
		// A pipeline or data preparation saves the Dataform project its
		// model describes, so Run and its schedules compile it.
		files, err := studioGeneratedFiles(project, kind, in.Content)
		if err != nil {
			return nil, err
		}
		ops = studioGeneratedOps(ctx, s, name, files)
	}
	ops[file] = &dfpb.CommitRepositoryChangesRequest_FileOperation{Operation: &dfpb.CommitRepositoryChangesRequest_FileOperation_WriteFile_{
		WriteFile: &dfpb.CommitRepositoryChangesRequest_FileOperation_WriteFile{Contents: []byte(in.Content)}}}
	if _, err := s.CommitRepositoryChanges(ctx, &dfpb.CommitRepositoryChangesRequest{Name: name,
		CommitMetadata: &dfpb.CommitMetadata{Author: &dfpb.CommitAuthor{Name: "BigQuery Studio", EmailAddress: "studio@cloudburrow.local"}, CommitMessage: "Save " + studioDisplay(r)},
		FileOperations: ops}); err != nil {
		return nil, err
	}
	return map[string]any{"name": name, "kind": r.GetLabels()[dataformAssetLabel], "displayName": studioDisplay(r), "location": kmsLocation(name), "parent": r.GetContainingFolder()}, nil
}

func newAssetID() string {
	// A repository ID that names nothing: Google's Studio uses a UUID too.
	return dataformUUID()
}

func studioRename(ctx context.Context, s *dataform.Server, in studioAssetReq) (any, error) {
	d := strings.TrimSpace(in.DisplayName)
	if d == "" {
		return nil, errors.New("a name is required")
	}
	if strings.Contains(in.Name, "/folders/") {
		f, err := s.UpdateFolder(ctx, &dfpb.UpdateFolderRequest{Folder: &dfpb.Folder{Name: in.Name, DisplayName: d}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
		if err != nil {
			return nil, err
		}
		return map[string]string{"name": f.GetName(), "displayName": f.GetDisplayName()}, nil
	}
	r, err := s.UpdateRepository(ctx, &dfpb.UpdateRepositoryRequest{Repository: &dfpb.Repository{Name: in.Name, DisplayName: d}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		return nil, err
	}
	return map[string]string{"name": r.GetName(), "displayName": r.GetDisplayName()}, nil
}

// studioHistory is Version history: the asset's commits, newest first.
func studioHistory(ctx context.Context, s *dataform.Server, name string) (any, error) {
	if _, _, err := studioAsset(ctx, s, name); err != nil {
		return nil, err
	}
	resp, err := s.FetchRepositoryHistory(ctx, &dfpb.FetchRepositoryHistoryRequest{Name: name, PageSize: 1000})
	if err != nil {
		return nil, err
	}
	type version struct {
		Commit  string `json:"commit"`
		Time    string `json:"time"`
		Author  string `json:"author"`
		Message string `json:"message"`
	}
	out := []version{}
	for _, c := range resp.GetCommits() {
		out = append(out, version{Commit: c.GetCommitSha(), Time: kmsTime(c.GetCommitTime()), Author: c.GetAuthor().GetName(), Message: c.GetCommitMessage()})
	}
	return map[string]any{"versions": out}, nil
}

// studioRoles are the roles Manage permissions grants, in Google's words.
var studioRoles = map[string]string{
	"roles/dataform.codeOwner":  "Code Owner",
	"roles/dataform.codeEditor": "Code Editor",
	"roles/dataform.codeViewer": "Code Viewer",
	"roles/dataform.admin":      "Dataform Admin",
}

func studioPolicy(pol *iampb.Policy) map[string]any {
	type binding struct {
		Role    string   `json:"role"`
		Title   string   `json:"title"`
		Members []string `json:"members"`
	}
	out := []binding{}
	for _, b := range pol.GetBindings() {
		t := studioRoles[b.GetRole()]
		if t == "" {
			t = b.GetRole()
		}
		out = append(out, binding{Role: b.GetRole(), Title: t, Members: b.GetMembers()})
	}
	return map[string]any{"bindings": out, "note": console.PermissionsNote}
}

// studioChangeIAM is Manage permissions' Add user/group (grant) and a
// principal's removal (revoke): a read-modify-write with the etag read.
func studioChangeIAM(ctx context.Context, s *dataform.Server, call string, in studioAssetReq) (any, error) {
	if _, ok := studioRoles[in.Role]; !ok {
		return nil, fmt.Errorf("%q is not a Code Owner, Code Editor, Code Viewer or Dataform Admin role", in.Role)
	}
	member := strings.TrimSpace(in.Member)
	if !strings.Contains(member, ":") {
		return nil, errors.New("a principal is written type:id, such as user:ana@example.com or group:team@example.com")
	}
	pol, err := s.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: in.Name})
	if err != nil {
		return nil, err
	}
	var kept []*iampb.Binding
	found := false
	for _, b := range pol.GetBindings() {
		if b.GetRole() == in.Role && b.GetCondition() == nil {
			found = true
			var ms []string
			for _, m := range b.GetMembers() {
				if m != member {
					ms = append(ms, m)
				}
			}
			if call == "grant" {
				ms = append(ms, member)
			}
			b.Members = ms
		}
		if len(b.GetMembers()) > 0 {
			kept = append(kept, b)
		}
	}
	if !found && call == "grant" {
		kept = append(kept, &iampb.Binding{Role: in.Role, Members: []string{member}})
	}
	pol.Bindings = kept
	out, err := s.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: in.Name, Policy: pol})
	if err != nil {
		return nil, err
	}
	return studioPolicy(out), nil
}

// studioRepositories is Source control: the project's Dataform
// repositories that are not code assets, with their workspaces.
func studioRepositories(ctx context.Context, s *dataform.Server, project string) (any, error) {
	out := []studioRepo{}
	for _, loc := range dataform.Locations() {
		resp, err := s.ListRepositories(ctx, &dfpb.ListRepositoriesRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return nil, err
		}
		for _, r := range resp.GetRepositories() {
			if dataformIsAsset(r) {
				continue
			}
			sr := studioRepo{Name: r.GetName(), Location: loc, Remote: r.GetGitRemoteSettings().GetUrl(), Workspaces: []string{}}
			ws, err := s.ListWorkspaces(ctx, &dfpb.ListWorkspacesRequest{Parent: r.GetName(), PageSize: 1000})
			if err != nil {
				return nil, err
			}
			for _, w := range ws.GetWorkspaces() {
				sr.Workspaces = append(sr.Workspaces, lastSegment(w.GetName()))
			}
			out = append(out, sr)
		}
	}
	return map[string]any{"repositories": out}, nil
}

// studioRepoFiles is a workspace's files, every directory walked.
func studioRepoFiles(ctx context.Context, s *dataform.Server, in studioAssetReq) (any, error) {
	ws, err := dataformChild(in.Name, "workspaces", in.Workspace)
	if err != nil {
		return nil, err
	}
	var files []string
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if depth > 16 {
			return nil
		}
		resp, err := s.QueryDirectoryContents(ctx, &dfpb.QueryDirectoryContentsRequest{Workspace: ws, Path: dir, PageSize: 1000})
		if err != nil {
			return err
		}
		for _, e := range resp.GetDirectoryEntries() {
			if f := e.GetFile(); f != "" {
				files = append(files, f)
			} else if d := e.GetDirectory(); d != "" {
				if err := walk(d, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk("", 0); err != nil {
		return nil, err
	}
	sort.Strings(files)
	if files == nil {
		files = []string{}
	}
	return map[string]any{"files": files}, nil
}

var _ console.Studio = dataformProvider{}

// dataformUUID is a random version-4 UUID.
func dataformUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
