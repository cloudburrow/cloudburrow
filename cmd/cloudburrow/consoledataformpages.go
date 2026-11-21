package main

// The Dataform console pages beyond a repository's own (#2094, the open
// items of #2105): a repository's code at its head without a workspace
// (QueryRepositoryDirectoryContents, ReadRepositoryFile) and its
// Permissions (the repository's IAM policy); the Folders page, the
// project's folders and team folders with their contents, Create folder,
// Create team folder, Rename, Delete and Permissions; and Settings, the
// project's Dataform configuration per region (GetConfig, UpdateConfig:
// the default Cloud KMS key, https://docs.cloud.google.com/dataform/docs/cmek,
// read 2026-11-21).

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
)

// dataformCode is the repository's Code tab: its files at the head of its
// default branch, each opening its contents.
func dataformCode(ctx context.Context, s *dataform.Server, repo string) console.Section {
	files := console.Listing{Columns: []string{"Type"}, NameColumn: "Path", Noun: "files",
		Note: "The repository's code at its head commit, read without a development workspace. Edit it in a workspace."}
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if depth > 16 {
			return nil
		}
		resp, err := s.QueryRepositoryDirectoryContents(ctx, &dfpb.QueryRepositoryDirectoryContentsRequest{Name: repo, Path: dir, PageSize: 1000})
		if err != nil {
			return err
		}
		entries := resp.GetDirectoryEntries()
		sort.SliceStable(entries, func(i, j int) bool {
			return entries[i].GetFile()+entries[i].GetDirectory() < entries[j].GetFile()+entries[j].GetDirectory()
		})
		for _, e := range entries {
			if d := e.GetDirectory(); d != "" {
				full := path.Join(dir, path.Base(d))
				files.Items = append(files.Items, console.Resource{Name: full + "/", Fields: map[string]string{"Type": "Folder"}})
				if err := walk(full, depth+1); err != nil {
					return err
				}
				continue
			}
			full := path.Join(dir, path.Base(e.GetFile()))
			files.Items = append(files.Items, console.Resource{Name: full, Opens: []string{repo, "code", full}, Fields: map[string]string{"Type": "File"}})
		}
		return nil
	}
	if err := walk("", 0); err != nil {
		files.Unavailable = "The repository has no commits yet: " + consoleErr(err)
	}
	files.Total = len(files.Items)
	return console.Section{ID: "code", Label: "Code", Listing: files}
}

func dataformCodeFile(ctx context.Context, s *dataform.Server, repo, file string) console.Detail {
	resp, err := s.ReadRepositoryFile(ctx, &dfpb.ReadRepositoryFileRequest{Name: repo, Path: file})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the file: " + consoleErr(err)}
	}
	return console.Detail{
		Summary:  []console.Property{{Label: "Repository", Value: lastSegment(repo)}, {Label: "Size", Value: fmt.Sprintf("%d bytes", len(resp.GetContents()))}},
		Sections: []console.Section{{ID: "code", Label: "Code", Kind: console.KindText, Text: string(resp.GetContents()), Language: dataformLanguage(file)}},
	}
}

// Permissions on a repository's page: its IAM policy.
func (dataformProvider) PolicyOn(_ context.Context, pth []string) *console.PolicyTarget {
	if len(pth) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "dataform", RoleHelp: roleHelp("Dataform", "roles/dataform.editor")}
}

func (p dataformProvider) withIAM(ctx context.Context, project string, pth []string, fn func(iamServer, string) error) error {
	s, err := p.api()
	if err != nil {
		return err
	}
	if len(pth) == 0 {
		return errors.New("no resource")
	}
	if err := kmsOwned(project, pth[0]); err != nil {
		return err
	}
	return fn(s, pth[0])
}

func (p dataformProvider) GetPolicy(ctx context.Context, project string, pth []string) (pol console.Policy, err error) {
	err = p.withIAM(ctx, project, pth, func(api iamServer, name string) error {
		pol, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return pol, err
}

func (p dataformProvider) SetPolicy(ctx context.Context, project string, pth []string, pol console.Policy) error {
	return p.withIAM(ctx, project, pth, func(api iamServer, name string) error { return setProtoPolicy(ctx, api, name, pol) })
}

func (p dataformProvider) TestPermissions(ctx context.Context, project string, pth []string, perms []string) (held []string, err error) {
	err = p.withIAM(ctx, project, pth, func(api iamServer, name string) error {
		held, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return held, err
}

var _ console.PolicyEditor = dataformProvider{}

// dataformFoldersProvider is the Folders page (/bigquery-dataform-folders):
// the project's Dataform folders and team folders in every region.
type dataformFoldersProvider struct{ df dataformProvider }

func (p dataformFoldersProvider) api() (*dataform.Server, error) { return p.df.api() }

func (dataformFoldersProvider) ID() string    { return "dataform-folders" }
func (dataformFoldersProvider) Title() string { return "Dataform folders" }

func (p dataformFoldersProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := p.api()
	if err != nil {
		return console.Listing{}, err
	}
	l := console.Listing{Columns: []string{"Type", "Region", "Containing folder", "Created"}, NameColumn: "Folder", Noun: "folders", RowsOpenable: true,
		Note: "Folders and team folders organize repositories and BigQuery Studio's saved queries, notebooks and data canvases, which Studio's Files pane lists."}
	if project == "" {
		l.Prompt = "Dataform lists folders per project. Choose one in the toolbar."
		return l, nil
	}
	for _, loc := range dataform.Locations() {
		parent := "projects/" + project + "/locations/" + loc
		teams, err := s.SearchTeamFolders(ctx, &dfpb.SearchTeamFoldersRequest{Location: parent, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		queue := []string{""}
		for _, t := range teams.GetResults() {
			if tf := t.GetTeamFolder(); tf != nil {
				l.Items = append(l.Items, console.Resource{Name: tf.GetName(), Fields: map[string]string{"Type": "Team folder", "Region": loc, "Containing folder": "—",
					"Created": kmsTime(tf.GetCreateTime()), "Display name": tf.GetDisplayName()}})
				queue = append(queue, tf.GetName())
			}
		}
		for len(queue) > 0 {
			c := queue[0]
			queue = queue[1:]
			var fs []*dfpb.Folder
			switch {
			case c == "":
				r, err := s.QueryUserRootContents(ctx, &dfpb.QueryUserRootContentsRequest{Location: parent, PageSize: 1000})
				if err != nil {
					return console.Listing{}, err
				}
				for _, e := range r.GetEntries() {
					if f := e.GetFolder(); f != nil {
						fs = append(fs, f)
					}
				}
			case strings.Contains(c, "/teamFolders/"):
				r, err := s.QueryTeamFolderContents(ctx, &dfpb.QueryTeamFolderContentsRequest{TeamFolder: c, PageSize: 1000})
				if err != nil {
					return console.Listing{}, err
				}
				for _, e := range r.GetEntries() {
					if f := e.GetFolder(); f != nil {
						fs = append(fs, f)
					}
				}
			default:
				r, err := s.QueryFolderContents(ctx, &dfpb.QueryFolderContentsRequest{Folder: c, PageSize: 1000})
				if err != nil {
					return console.Listing{}, err
				}
				for _, e := range r.GetEntries() {
					if f := e.GetFolder(); f != nil {
						fs = append(fs, f)
					}
				}
			}
			for _, f := range fs {
				l.Items = append(l.Items, console.Resource{Name: f.GetName(), Fields: map[string]string{"Type": "Folder", "Region": loc,
					"Containing folder": kmsOrDash(f.GetContainingFolder()), "Created": kmsTime(f.GetCreateTime())}})
				queue = append(queue, f.GetName())
			}
		}
	}
	l.Total = len(l.Items)
	return l, nil
}

func (p dataformFoldersProvider) CreateForm() (string, []console.Field) {
	return "Create folder", []console.Field{
		{Name: "type", Label: "Type", Type: "select", Required: true, Default: "Folder", Options: []string{"Folder", "Team folder"}},
		{Name: "displayName", Label: "Name", Type: "text", Required: true},
		{Name: "region", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: dataform.Locations()},
		{Name: "containingFolder", Label: "Containing folder", Type: "text",
			Help: "Optional, for a folder: the folder or team folder it is in, projects/PROJECT/locations/REGION/folders/ID."},
	}
}

func (p dataformFoldersProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	s, err := p.api()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a folder")
	}
	parent := "projects/" + project + "/locations/" + strings.TrimSpace(v["region"])
	if v["type"] == "Team folder" {
		if strings.TrimSpace(v["containingFolder"]) != "" {
			return "", errors.New("a team folder is not in another folder")
		}
		t, err := s.CreateTeamFolder(ctx, &dfpb.CreateTeamFolderRequest{Parent: parent, TeamFolder: &dfpb.TeamFolder{DisplayName: strings.TrimSpace(v["displayName"])}})
		if err != nil {
			return "", err
		}
		return t.GetName(), nil
	}
	f, err := s.CreateFolder(ctx, &dfpb.CreateFolderRequest{Parent: parent, Folder: &dfpb.Folder{DisplayName: strings.TrimSpace(v["displayName"]), ContainingFolder: strings.TrimSpace(v["containingFolder"])}})
	if err != nil {
		return "", err
	}
	return f.GetName(), nil
}

func (p dataformFoldersProvider) Detail(ctx context.Context, project string, pth []string) (console.Detail, error) {
	s, err := p.api()
	if err != nil {
		return console.Detail{}, err
	}
	if len(pth) != 1 {
		return console.DeeperThan(1, pth), nil
	}
	name := pth[0]
	if err := kmsOwned(project, name); err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	contents := console.Listing{Columns: []string{"Type", "Created"}, NameColumn: "Name", Noun: "items"}
	add := func(f *dfpb.Folder, r *dfpb.Repository) {
		switch {
		case f != nil:
			contents.Items = append(contents.Items, console.Resource{Name: f.GetDisplayName(), Target: []string{f.GetName()}, Opens: []string{f.GetName()},
				Fields: map[string]string{"Type": "Folder", "Created": kmsTime(f.GetCreateTime())}})
		case r != nil:
			kind := "Repository"
			switch r.GetLabels()[dataformAssetLabel] {
			case "sql":
				kind = "Saved query"
			case "notebook":
				kind = "Notebook"
			case "data_canvas":
				kind = "Data canvas"
			}
			contents.Items = append(contents.Items, console.Resource{Name: studioDisplay(r), Fields: map[string]string{"Type": kind, "Created": kmsTime(r.GetCreateTime())}})
		}
	}
	var props []console.Property
	if strings.Contains(name, "/teamFolders/") {
		t, err := s.GetTeamFolder(ctx, &dfpb.GetTeamFolderRequest{Name: name})
		if err != nil {
			return console.Detail{Unavailable: "cannot read the team folder: " + consoleErr(err)}, nil
		}
		props = []console.Property{{Label: "Type", Value: "Team folder"}, {Label: "Name", Value: t.GetDisplayName()}, {Label: "Created", Value: kmsTime(t.GetCreateTime())}}
		r, err := s.QueryTeamFolderContents(ctx, &dfpb.QueryTeamFolderContentsRequest{TeamFolder: name, PageSize: 1000})
		if err != nil {
			contents.Unavailable = consoleErr(err)
		}
		for _, e := range r.GetEntries() {
			add(e.GetFolder(), e.GetRepository())
		}
	} else {
		f, err := s.GetFolder(ctx, &dfpb.GetFolderRequest{Name: name})
		if err != nil {
			return console.Detail{Unavailable: "cannot read the folder: " + consoleErr(err)}, nil
		}
		props = []console.Property{{Label: "Type", Value: "Folder"}, {Label: "Name", Value: f.GetDisplayName()},
			{Label: "Containing folder", Value: kmsOrDash(f.GetContainingFolder())}, {Label: "Created", Value: kmsTime(f.GetCreateTime())}}
		r, err := s.QueryFolderContents(ctx, &dfpb.QueryFolderContentsRequest{Folder: name, PageSize: 1000})
		if err != nil {
			contents.Unavailable = consoleErr(err)
		}
		for _, e := range r.GetEntries() {
			add(e.GetFolder(), e.GetRepository())
		}
	}
	contents.Total = len(contents.Items)
	return console.Detail{Summary: props, Sections: []console.Section{
		{ID: "contents", Label: "Contents", Listing: contents},
		{ID: "permissions", Kind: console.KindPermissions},
	}}, nil
}

func (p dataformFoldersProvider) DetailActions(_ context.Context, _ string, pth []string) []console.Action {
	if len(pth) != 1 {
		return nil
	}
	return []console.Action{
		{ID: "rename", Label: "Rename", Submit: "Rename", Fields: []console.Field{{Name: "displayName", Label: "Name", Type: "text", Required: true}}},
		{ID: "move", Label: "Move", Submit: "Move", Fields: []console.Field{{Name: "destination", Label: "Destination folder", Type: "text",
			Help: "A folder or team folder of the same region; empty moves it to your root folder."}}},
		{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, Confirm: "An empty folder is deleted; one with contents is deleted with everything in it, folders, repositories and code assets."},
	}
}

func (p dataformFoldersProvider) ActAt(ctx context.Context, project string, pth []string, action string, v map[string]string) error {
	s, err := p.api()
	if err != nil {
		return err
	}
	if len(pth) != 1 {
		return errors.New("no folder")
	}
	name := pth[0]
	if err := kmsOwned(project, name); err != nil {
		return err
	}
	team := strings.Contains(name, "/teamFolders/")
	switch action {
	case "rename":
		d := strings.TrimSpace(v["displayName"])
		mask := &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}
		if team {
			_, err = s.UpdateTeamFolder(ctx, &dfpb.UpdateTeamFolderRequest{TeamFolder: &dfpb.TeamFolder{Name: name, DisplayName: d}, UpdateMask: mask})
		} else {
			_, err = s.UpdateFolder(ctx, &dfpb.UpdateFolderRequest{Folder: &dfpb.Folder{Name: name, DisplayName: d}, UpdateMask: mask})
		}
		return err
	case "move":
		if team {
			return errors.New("a team folder is not in another folder")
		}
		dest := strings.TrimSpace(v["destination"])
		_, err = s.MoveFolder(ctx, &dfpb.MoveFolderRequest{Name: name, DestinationContainingFolder: &dest})
		return err
	case "delete":
		// DeleteFolder and DeleteTeamFolder take an empty one; one with
		// contents goes with its tree.
		if team {
			if _, err = s.DeleteTeamFolder(ctx, &dfpb.DeleteTeamFolderRequest{Name: name}); err != nil {
				_, err = s.DeleteTeamFolderTree(ctx, &dfpb.DeleteTeamFolderTreeRequest{Name: name, Force: true})
			}
		} else if _, err = s.DeleteFolder(ctx, &dfpb.DeleteFolderRequest{Name: name}); err != nil {
			_, err = s.DeleteFolderTree(ctx, &dfpb.DeleteFolderTreeRequest{Name: name, Force: true})
		}
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

func (dataformFoldersProvider) PolicyOn(_ context.Context, pth []string) *console.PolicyTarget {
	if len(pth) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "dataform", RoleHelp: roleHelp("Dataform", "roles/dataform.codeViewer")}
}

var (
	_ console.Creator      = dataformFoldersProvider{}
	_ console.Driller      = dataformFoldersProvider{}
	_ console.PathActor    = dataformFoldersProvider{}
	_ console.PolicyEditor = dataformFoldersProvider{}
)

// dataformSettingsProvider is Dataform's Settings page
// (/bigquery-dataform-settings): the project's configuration in each
// region, its default Cloud KMS key.
type dataformSettingsProvider struct{ df dataformProvider }

func (p dataformSettingsProvider) api() (*dataform.Server, error) { return p.df.api() }

func (dataformSettingsProvider) ID() string    { return "dataform-settings" }
func (dataformSettingsProvider) Title() string { return "Dataform settings" }

func (p dataformSettingsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := p.api()
	if err != nil {
		return console.Listing{}, err
	}
	l := console.Listing{Columns: []string{"Region", "Default Cloud KMS key"}, NameColumn: "Configuration", Noun: "regions", RowsOpenable: true,
		Note: "A repository created in a region with a default key is encrypted with it unless it names its own."}
	if project == "" {
		l.Prompt = "Dataform keeps its settings per project. Choose one in the toolbar."
		return l, nil
	}
	for _, loc := range dataform.Locations() {
		name := "projects/" + project + "/locations/" + loc + "/config"
		c, err := s.GetConfig(ctx, &dfpb.GetConfigRequest{Name: name})
		if err != nil {
			return console.Listing{}, err
		}
		l.Items = append(l.Items, console.Resource{Name: name, Fields: map[string]string{"Region": loc, "Default Cloud KMS key": kmsOrDash(c.GetDefaultKmsKeyName())}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (p dataformSettingsProvider) Detail(ctx context.Context, project string, pth []string) (console.Detail, error) {
	s, err := p.api()
	if err != nil {
		return console.Detail{}, err
	}
	if len(pth) != 1 {
		return console.DeeperThan(1, pth), nil
	}
	if err := kmsOwned(project, pth[0]); err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	c, err := s.GetConfig(ctx, &dfpb.GetConfigRequest{Name: pth[0]})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the settings: " + consoleErr(err)}, nil
	}
	return console.Detail{Summary: []console.Property{{Label: "Region", Value: kmsLocation(pth[0])},
		{Label: "Default Cloud KMS key", Value: dataformEncryption(c.GetDefaultKmsKeyName())}}}, nil
}

func (p dataformSettingsProvider) DetailActions(ctx context.Context, project string, pth []string) []console.Action {
	key := ""
	if s, err := p.api(); err == nil && len(pth) == 1 {
		if c, err := s.GetConfig(ctx, &dfpb.GetConfigRequest{Name: pth[0]}); err == nil {
			key = c.GetDefaultKmsKeyName()
		}
	}
	return []console.Action{{ID: "edit", Label: "Edit", Submit: "Save", Fields: []console.Field{
		{Name: "kmsKeyName", Label: "Default Cloud KMS key", Type: "text", Browse: "kms", Default: key,
			Pattern: `^(projects/[^\/]+/locations/[^\/]+/keyRings/[^\/]+/cryptoKeys/[^\/]+)?$`,
			Help:    "A key of this instance's Cloud KMS in the region; empty uses Google-managed encryption."}}}}
}

func (p dataformSettingsProvider) ActAt(ctx context.Context, project string, pth []string, action string, v map[string]string) error {
	s, err := p.api()
	if err != nil {
		return err
	}
	if len(pth) != 1 || action != "edit" {
		return fmt.Errorf("unknown action %q", action)
	}
	if err := kmsOwned(project, pth[0]); err != nil {
		return err
	}
	_, err = s.UpdateConfig(ctx, &dfpb.UpdateConfigRequest{Config: &dfpb.Config{Name: pth[0], DefaultKmsKeyName: strings.TrimSpace(v["kmsKeyName"])},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_kms_key_name"}}})
	return err
}

var (
	_ console.Driller   = dataformSettingsProvider{}
	_ console.PathActor = dataformSettingsProvider{}
)

func (p dataformFoldersProvider) GetPolicy(ctx context.Context, project string, pth []string) (console.Policy, error) {
	return p.df.GetPolicy(ctx, project, pth)
}

func (p dataformFoldersProvider) SetPolicy(ctx context.Context, project string, pth []string, pol console.Policy) error {
	return p.df.SetPolicy(ctx, project, pth, pol)
}

func (p dataformFoldersProvider) TestPermissions(ctx context.Context, project string, pth []string, perms []string) ([]string, error) {
	return p.df.TestPermissions(ctx, project, pth, perms)
}
