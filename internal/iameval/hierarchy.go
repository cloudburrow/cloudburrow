package iameval

import "strings"

// HierarchySource is a Source that knows the ancestry of a folder, the
// organization or a billing account: what applies to a resource under one
// of them rather than under a project (Cloud Logging's sinks, buckets,
// views and entries of folders/N, organizations/N and billingAccounts/X,
// #1879).
type HierarchySource interface {
	// HierarchyAncestry is root's ancestry as policy resources, root
	// first: "folders/N", its parent folders, "organizations/N"; for
	// "organizations/N" itself; for "billingAccounts/X" itself (and its
	// organization, when it has one). nil for a root it does not know.
	HierarchyAncestry(root string) []string
}

// HierarchyRoot is the folder, organization or billing account a resource
// name begins with ("folders/1/sinks/s" is "folders/1"), "" for any other
// name (a project's resources have their project's ancestry).
func HierarchyRoot(name string) string {
	parts := strings.SplitN(strings.TrimPrefix(name, "/"), "/", 3)
	if len(parts) < 2 || parts[1] == "" {
		return ""
	}
	switch parts[0] {
	case "folders", "organizations", "billingAccounts":
		return parts[0] + "/" + parts[1]
	}
	return ""
}

// hierarchyPolicies are the policies of a resource under a folder, the
// organization or a billing account, its root's first: what a request
// with no project checks besides the service's own (nil for a project's
// resource, or a Source that knows no hierarchy).
func (e *Evaluator) hierarchyPolicies(resource string) []Attached {
	root := HierarchyRoot(resource)
	if root == "" || e.Source == nil {
		return nil
	}
	hs, ok := e.Source.(HierarchySource)
	if !ok {
		return nil
	}
	var out []Attached
	for _, r := range hs.HierarchyAncestry(root) {
		out = append(out, Attached{Resource: r, Policy: e.Source.Policy(r)})
	}
	return out
}
