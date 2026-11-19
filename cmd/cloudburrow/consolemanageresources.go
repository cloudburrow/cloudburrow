package main

// Manage resources (#1865), as Google's console draws it
// (https://docs.cloud.google.com/resource-manager/docs/creating-managing-folders,
// .../creating-managing-projects, .../tags/tags-creating-and-managing, read
// 2026-11-19): one tree table of the organization, its folders and the
// projects under each, with Create project and Create folder; Move,
// Delete (a project is shut down: DELETE_REQUESTED for 30 days, then
// purged) and, under Resources pending deletion, Restore; a project's
// labels; and the Tags each resource carries, with Add tag. The IAM side
// panel of a row is the Permissions view the project's (service "iam") or
// the folder's or organization's (service "hierarchy") page already
// serves, so the screen sends these calls only:
//
//	tree     the organization, every folder and project with its state,
//	         labels, tags and, when pending deletion, its purge time; the
//	         tag values a resource can be given
//	folder   {displayName, parent}: Create folder
//	move     {kind, id, parent}
//	delete   {kind, id}
//	restore  {kind, id}
//	labels   {id, labels}: a project's labels, replaced whole
//	tag      {kind, id, value}: bind a tag value
//	untag    {kind, id, value}: remove it

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// mrNode is one row of the tree.
type mrNode struct {
	// Kind is organization, folder or project.
	Kind string `json:"kind"`
	// ID is the project ID, the folder's or organization's number.
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Number      string            `json:"number,omitempty"`
	Parent      string            `json:"parent"`
	State       string            `json:"state"`
	Labels      map[string]string `json:"labels,omitempty"`
	Tags        []string          `json:"tags"`
	DeleteTime  string            `json:"deleteTime,omitempty"`
	PurgeTime   string            `json:"purgeTime,omitempty"`
	ResourceRef string            `json:"resource"`
}

type mrTree struct {
	Nodes []mrNode `json:"nodes"`
	// TagValues are the namespaced names Add tag offers.
	TagValues []string `json:"tagValues"`
	Note      string   `json:"note"`
}

type mrRequest struct {
	Kind        string            `json:"kind"`
	ID          string            `json:"id"`
	Parent      string            `json:"parent"`
	DisplayName string            `json:"displayName"`
	Labels      map[string]string `json:"labels"`
	Value       string            `json:"value"`
}

const mrNote = "One local organization, " + resourcemanager.OrganizationName + " (" + resourcemanager.OrganizationDomain +
	"). A deleted project is shut down: it is kept pending deletion for 30 days, when Restore brings it back, and then purged with " +
	"what every service keeps under it. A folder is deleted only when it is empty. IAM policies are stored and inherited " +
	"by nothing unless `up --iam-enforcement=on` (ADR-0006)."

func mrPurgeTime(deleted string) string {
	t, err := time.Parse(time.RFC3339, deleted)
	if err != nil {
		return ""
	}
	return t.Add(resourcemanager.RetentionPeriod).Local().Format("2006-01-02 15:04")
}

func (p projectsProvider) mrTags(target string) []string {
	out := []string{}
	if bs, err := p.registry.TagBindings(target); err == nil {
		for _, b := range bs {
			out = append(out, b.GetTagValueNamespacedName())
		}
	}
	sort.Strings(out)
	return out
}

func (p projectsProvider) mrTree() (mrTree, error) {
	t := mrTree{Note: mrNote, TagValues: []string{}}
	org := resourcemanager.OrganizationName
	t.Nodes = append(t.Nodes, mrNode{Kind: "organization", ID: strings.TrimPrefix(org, "organizations/"), Name: resourcemanager.OrganizationDomain,
		State: string(resourcemanager.StateActive), Tags: p.mrTags(resourcemanager.CRMPrefix + org), ResourceRef: org})
	folders, err := p.registry.Folders()
	if err != nil {
		return t, err
	}
	for _, f := range folders {
		n := mrNode{Kind: "folder", ID: f.ID(), Name: f.DisplayName, Parent: f.Parent, State: string(f.State),
			Tags: p.mrTags(resourcemanager.CRMPrefix + f.Name), ResourceRef: f.Name, DeleteTime: f.DeleteTime}
		if f.State == resourcemanager.StateDeleteRequested {
			n.PurgeTime = mrPurgeTime(f.DeleteTime)
		}
		t.Nodes = append(t.Nodes, n)
	}
	projects, err := p.registry.List()
	if err != nil {
		return t, err
	}
	for _, pr := range projects {
		n := mrNode{Kind: "project", ID: pr.ProjectID, Name: pr.DisplayName, Number: projectNumberText(pr.ProjectID),
			Parent: pr.Parent, State: string(pr.State), Labels: pr.Labels, Tags: p.mrTags(projectTarget(pr.ProjectID)),
			ResourceRef: "projects/" + pr.ProjectID, DeleteTime: pr.DeleteTime}
		if pr.State == resourcemanager.StateDeleteRequested {
			n.PurgeTime = mrPurgeTime(pr.DeleteTime)
		}
		t.Nodes = append(t.Nodes, n)
	}
	if keys, err := p.registry.AllTagKeys(); err == nil {
		for _, k := range keys {
			if vs, err := p.registry.TagValues(k.GetName()); err == nil {
				for _, v := range vs {
					t.TagValues = append(t.TagValues, v.GetNamespacedName())
				}
			}
		}
	}
	sort.Strings(t.TagValues)
	return t, nil
}

// mrTarget is the full resource name a tag binding of the row names.
func mrTarget(kind, id string) (string, error) {
	switch kind {
	case "project":
		return projectTarget(id), nil
	case "folder":
		return resourcemanager.CRMPrefix + "folders/" + strings.TrimPrefix(id, "folders/"), nil
	case "organization":
		return resourcemanager.CRMPrefix + resourcemanager.OrganizationName, nil
	}
	return "", fmt.Errorf("unknown resource kind %q", kind)
}

// StudioCall implements console.Studio for Manage resources.
func (p projectsProvider) StudioCall(_ context.Context, _ string, call string, body json.RawMessage) (any, error) {
	if p.registry == nil {
		return nil, errors.New("Resource Manager is not available")
	}
	var req mrRequest
	if call != "tree" {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("%s: %w", call, err)
		}
	}
	done := map[string]string{"status": "done"}
	switch call {
	case "tree":
		return p.mrTree()
	case "folder":
		f, err := p.registry.CreateFolder(req.Parent, strings.TrimSpace(req.DisplayName))
		if err != nil {
			return nil, err
		}
		return map[string]string{"name": f.Name}, nil
	case "move":
		var err error
		switch req.Kind {
		case "project":
			_, err = p.registry.Move(req.ID, req.Parent)
		case "folder":
			_, err = p.registry.MoveFolder(req.ID, req.Parent)
		default:
			err = fmt.Errorf("only a project or a folder can be moved")
		}
		return done, err
	case "delete":
		var err error
		switch req.Kind {
		case "project":
			_, err = p.registry.Delete(req.ID)
		case "folder":
			_, err = p.registry.DeleteFolder(req.ID)
		default:
			err = fmt.Errorf("the organization cannot be deleted")
		}
		return done, err
	case "restore":
		var err error
		switch req.Kind {
		case "project":
			_, err = p.registry.Undelete(req.ID)
		case "folder":
			_, err = p.registry.UndeleteFolder(req.ID)
		default:
			err = fmt.Errorf("only a project or a folder can be restored")
		}
		return done, err
	case "labels":
		pr, err := p.registry.Get(req.ID)
		if err != nil {
			return nil, err
		}
		if req.Labels == nil {
			req.Labels = map[string]string{}
		}
		_, err = p.registry.Update(req.ID, pr.DisplayName, req.Labels)
		return done, err
	case "tag", "untag":
		target, err := mrTarget(req.Kind, req.ID)
		if err != nil {
			return nil, err
		}
		if call == "tag" {
			_, err = p.registry.CreateTagBinding(&rmpb.TagBinding{Parent: target, TagValueNamespacedName: req.Value}, false)
			return done, err
		}
		_, err = tagAct(p.registry, target, tagUnbind, map[string]string{"value": req.Value})
		return done, err
	}
	return nil, fmt.Errorf("unknown call %q", call)
}

var _ console.Studio = projectsProvider{}
