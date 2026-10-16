package main

// The Log storage screen's parent picker (#1411). Cloud Logging keeps
// buckets under four parents: a project, a folder, an organization and a
// billing account. The screen read the toolbar's project's alone, so a
// folder's, organization's or billing account's bucket, and the Copy log
// entries action on it, could not be reached from the console. The parent
// now travels in the URL as ?parent=, the way Bigtable's instance does
// (#1360), and every read and change on the screen addresses it.
//
// The picker offers the toolbar's project, the organization and the folders
// Resource Manager holds, and the billing accounts Logging holds anything
// for: Cloud Billing is not served, so nothing else names one. Any other
// billing account is reached by naming it in the address,
// ?parent=billingAccounts/XXXXXX-XXXXXX-XXXXXX, and the picker then offers
// it too.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// bucketScopeKey is the query parameter the chosen parent travels in.
const bucketScopeKey = "parent"

// ScopeKey implements console.Scoper.
func (loggingBucketsProvider) ScopeKey() string { return bucketScopeKey }

var bucketParentRe = regexp.MustCompile(`^(folders/[0-9]{1,32}|organizations/[0-9]{1,32}|billingAccounts/[0-9A-Fa-f]{6}-[0-9A-Fa-f]{6}-[0-9A-Fa-f]{6})$`)

// bucketParent is the parent ctx's request names, or the toolbar's project
// when it names none. A folder, organization or billing account is taken
// in its documented form, and Logging says whether it exists; another
// project is refused, so the screen cannot reach past the toolbar's.
func bucketParent(ctx context.Context, project string) (string, error) {
	v := strings.TrimSpace(console.ScopeValue(ctx, bucketScopeKey))
	switch {
	case v == "" || v == "projects/"+project:
		if project == "" {
			return "", errors.New("choose a project first, or a folder, organization or billing account in the picker")
		}
		return "projects/" + project, nil
	case bucketParentRe.MatchString(v):
		if id, ok := strings.CutPrefix(v, "billingAccounts/"); ok {
			return "billingAccounts/" + strings.ToUpper(id), nil
		}
		return v, nil
	}
	return "", fmt.Errorf("parent %q is not the toolbar's project, a folder, an organization or a billing account", v)
}

// bucketOfParent checks a bucket name against the screen's parent.
func bucketOfParent(ctx context.Context, project, name string) error {
	parent, err := bucketParent(ctx, project)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(name, parent+"/locations/") || !strings.Contains(name, "/buckets/") {
		return fmt.Errorf("%s is not one of %s's buckets", name, parent)
	}
	return nil
}

// bucketScope is the picker: the project, the organization, its folders,
// the billing accounts Logging knows, and the selected parent.
func (p loggingBucketsProvider) bucketScope(ctx context.Context, project string) *console.Scope {
	sc := &console.Scope{Key: bucketScopeKey, Label: "Parent"}
	if project != "" {
		sc.Default = "projects/" + project
		sc.Options = append(sc.Options, console.ScopeOption{ID: sc.Default, Label: "Project " + project})
	}
	if reg := p.svc.hierarchy; reg != nil {
		sc.Options = append(sc.Options, console.ScopeOption{ID: resourcemanager.OrganizationName,
			Label: "Organization " + resourcemanager.OrganizationDomain + " (" + resourcemanager.OrganizationName + ")"})
		if fs, err := reg.Folders(); err == nil {
			sort.Slice(fs, func(i, j int) bool { return fs[i].Name < fs[j].Name })
			for _, f := range fs {
				if f.State != resourcemanager.StateActive {
					continue
				}
				sc.Options = append(sc.Options, console.ScopeOption{ID: f.Name, Label: "Folder " + f.DisplayName + " (" + f.Name + ")"})
			}
		}
	}
	if p.svc.api != nil {
		for _, b := range p.svc.api.BillingAccounts() {
			sc.Options = append(sc.Options, console.ScopeOption{ID: b, Label: "Billing account " + strings.TrimPrefix(b, "billingAccounts/")})
		}
	}
	sc.Selected = sc.Default
	if v, err := bucketParent(ctx, project); err == nil {
		sc.Selected = v
	}
	found := false
	for _, o := range sc.Options {
		found = found || o.ID == sc.Selected
	}
	if !found && sc.Selected != "" {
		sc.Options = append(sc.Options, console.ScopeOption{ID: sc.Selected})
	}
	return sc
}
