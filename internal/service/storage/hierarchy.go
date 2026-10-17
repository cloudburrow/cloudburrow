package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The resource hierarchy Storage Intelligence inheritance walks (#1532):
// INHERIT "inherits the edition configuration of the parent resource", and
// an organization's or a folder's configuration applies to the folders and
// projects under it "unless overridden" (docs.cloud.google.com/storage/
// docs/storage-intelligence/overview), so the effective configuration is
// the nearest one set up project → folder(s) → organization. The storage server
// keeps no hierarchy of its own: it asks the instance's Resource Manager
// (the --resourcemanager address `up` gives the pod, through the
// cloudburrow-host Service) for each step's parent, with v3 projects.get
// (which takes a project number, the form an IntelligenceConfig name
// carries) and folders.get.
//
// A server given no Resource Manager (a bare `cloudburrow storage-server`)
// cannot see folders, so a project or a folder that inherits inherits the
// local organization's configuration, as before #1532; so does a project
// or a folder Resource Manager does not have (the storage server accepts
// any), as the instance's resources are its organization's.

// Hierarchy reads a resource's parent.
type Hierarchy interface {
	// Parent is the parent of kind/id (projects/{number}, folders/{id}):
	// "folders/{id}", "organizations/{id}", or "" for a resource with no
	// parent. found is false when there is no such resource.
	Parent(ctx context.Context, kind, id string) (parent string, found bool, err error)
}

// ResourceManagerHierarchy is a Hierarchy over Resource Manager's v3 REST
// API at base (http://host:port).
type ResourceManagerHierarchy struct {
	Base   string
	Client *http.Client
}

// Parent implements Hierarchy.
func (h ResourceManagerHierarchy) Parent(ctx context.Context, kind, id string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := strings.TrimRight(h.Base, "/") + "/v3/" + kind + "/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", false, err
	}
	c := h.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return "", false, nil
	}
	var out struct {
		Parent string `json:"parent"`
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("GET %s/%s: HTTP %d: %s", kind, id, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", false, fmt.Errorf("GET %s/%s: %w", kind, id, err)
	}
	return out.Parent, true, nil
}

// maxAncestry bounds the walk: a project, Resource Manager's ten levels of
// folders, and the organization, with room to spare.
const maxAncestry = 16

// intelligenceAncestry is t and its ancestors, nearest first.
func (s *Server) intelligenceAncestry(ctx context.Context, t intelligenceTarget) ([]intelligenceTarget, error) {
	org := intelligenceTarget{kind: "organizations", id: localOrganization}
	out := []intelligenceTarget{t}
	if t.kind == "organizations" {
		return out, nil
	}
	if s.hierarchy == nil {
		return append(out, org), nil
	}
	for cur := t; len(out) < maxAncestry; {
		parent, found, err := s.hierarchy.Parent(ctx, cur.kind, cur.id)
		if err != nil {
			return nil, errorf(http.StatusServiceUnavailable, "backendError",
				"cloudburrow: could not read the ancestry of %s/%s from Resource Manager: %v", cur.kind, cur.id, err)
		}
		if !found {
			return append(out, org), nil
		}
		kind, id, ok := strings.Cut(parent, "/")
		if !ok || (kind != "folders" && kind != "organizations") || id == "" {
			return out, nil // no parent: nothing above it
		}
		cur = intelligenceTarget{kind: kind, id: id}
		out = append(out, cur)
		if kind == "organizations" {
			return out, nil
		}
	}
	return out, nil
}
