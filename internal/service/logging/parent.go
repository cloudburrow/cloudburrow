package logging

// Parents other than projects (#1395). Cloud Logging documents four parents
// for entries, logs, sinks, exclusions, buckets, views, links and settings:
// projects/{project}, folders/{folder}, organizations/{organization} and
// billingAccounts/{account}.
//
//   - Folders and the organization are the ones Resource Manager holds,
//     through a Hierarchy the CLI wires in: one that does not exist is
//     NOT_FOUND, and without a Hierarchy they are UNIMPLEMENTED.
//   - Billing accounts are accepted in the documented ID form
//     (XXXXXX-XXXXXX-XXXXXX) and are not checked: Cloud Billing is not served,
//     so nothing holds them. Nothing is a billing account's child.
//   - Log-based metrics have only project parents, as Google documents.

import (
	"errors"
	"regexp"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Hierarchy is what Logging needs of Resource Manager: whether a folder or
// organization exists, and what a project or folder sits under.
type Hierarchy interface {
	// Exists answers nil for an existing folders/{n} or organizations/{n},
	// and an error (NOT_FOUND) otherwise.
	Exists(name string) error
	// Ancestors are the folders and organization above projects/{p} or
	// folders/{n}, nearest first, as resource names.
	Ancestors(name string) ([]string, error)
}

// UseHierarchy makes folders and the organization parents, as Resource
// Manager holds them.
func (a *Admin) UseHierarchy(h Hierarchy) { a.h = h }

const (
	collProjects = "projects"
	collFolders  = "folders"
	collOrgs     = "organizations"
	collBilling  = "billingAccounts"
)

var (
	numericRe = regexp.MustCompile(`^[0-9]{1,32}$`)
	billingRe = regexp.MustCompile(`^[0-9A-Fa-f]{6}-[0-9A-Fa-f]{6}-[0-9A-Fa-f]{6}$`)
)

// parent is one of the four documented parents.
type parent struct{ coll, id string }

func projectParent(project string) parent { return parent{collProjects, project} }

func (p parent) String() string { return p.coll + "/" + p.id }

func (p parent) isProject() bool { return p.coll == collProjects }

// seg is the parent's store-key segment. A project's is its ID, as it was
// before other parents were served, so stored state keeps its keys; the
// others hold a '~', which no project ID can.
func (p parent) seg() string {
	if p.isProject() {
		return p.id
	}
	return "~" + p.coll + "~" + p.id
}

// hasChildren is whether include_children means anything: organizations and
// folders only, as logging_config.proto documents.
func (p parent) hasChildren() bool { return p.coll == collFolders || p.coll == collOrgs }

// cutParent splits a leading parent off name, checking its shape, and
// returns the rest after its slash ("" when name is only the parent).
func cutParent(field, name string) (parent, string, error) {
	bare := strings.TrimPrefix(name, "/")
	parts := strings.SplitN(bare, "/", 3)
	if len(parts) < 2 || parts[1] == "" {
		return parent{}, "", apierror.InvalidArgument("%s %q does not start with projects/{project}, folders/{folder}, organizations/{organization} or billingAccounts/{account}", field, name)
	}
	p := parent{parts[0], parts[1]}
	rest := ""
	if len(parts) == 3 {
		rest = parts[2]
	}
	switch p.coll {
	case collProjects:
		if _, err := parseProjectParent(field, p.String()); err != nil {
			return parent{}, "", err
		}
	case collFolders, collOrgs:
		if !numericRe.MatchString(p.id) {
			return parent{}, "", apierror.InvalidArgument("%s %q: %s are named by number", field, name, p.coll)
		}
	case collBilling:
		if !billingRe.MatchString(p.id) {
			return parent{}, "", apierror.InvalidArgument("%s %q: a billing account ID is XXXXXX-XXXXXX-XXXXXX", field, name)
		}
		p.id = strings.ToUpper(p.id)
	default:
		return parent{}, "", apierror.InvalidArgument("%s %q does not start with projects/{project}, folders/{folder}, organizations/{organization} or billingAccounts/{account}", field, name)
	}
	return p, rest, nil
}

// check refuses a folder or organization Resource Manager does not hold.
func (a *Admin) check(p parent) error {
	if p.coll != collFolders && p.coll != collOrgs {
		return nil
	}
	if a == nil || a.h == nil {
		return apierror.Unimplemented("%s: folders and organizations need Resource Manager, which this instance does not serve", p)
	}
	if err := a.h.Exists(p.String()); err != nil {
		var ae *apierror.Error
		if errors.As(err, &ae) {
			return apierror.NotFound("%s does not exist: %s", p, ae.Message)
		}
		return apierror.NotFound("%s does not exist: %v", p, err)
	}
	return nil
}

// resolve parses a whole parent name and checks it exists.
func (a *Admin) resolve(field, name string) (parent, error) {
	p, rest, err := cutParent(field, name)
	if err != nil {
		return parent{}, err
	}
	if rest != "" {
		return parent{}, apierror.InvalidArgument("%s %q is not projects/{project}, folders/{folder}, organizations/{organization} or billingAccounts/{account}", field, name)
	}
	return p, a.check(p)
}

// ancestors are the folders and organization above p, nearest first; none
// without a Hierarchy, or for an organization or a billing account.
func (a *Admin) ancestors(p parent) []parent {
	if a == nil || a.h == nil || (p.coll != collProjects && p.coll != collFolders) {
		return nil
	}
	names, err := a.h.Ancestors(p.String())
	if err != nil {
		return nil
	}
	out := make([]parent, 0, len(names))
	for _, n := range names {
		if q, rest, err := cutParent("ancestor", n); err == nil && rest == "" {
			out = append(out, q)
		}
	}
	return out
}
