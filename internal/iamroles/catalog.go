// Package iamroles is IAM's permission catalog and the permissions of the
// roles CloudBurrow's permission checks evaluate (#1743).
//
// permissions.tsv.gz is every permission Google's "Permissions supported in
// custom roles" page lists (https://docs.cloud.google.com/iam/docs/custom-roles-permissions-support,
// read 2026-11-17), one per line, a tab, then its custom roles support
// level: S (SUPPORTED), T (TESTING) or N (NOT_SUPPORTED). The page's table
// was extracted as it stands, 14,527 permissions; to refresh it, extract the
// page's two-column table the same way and sort it.
//
// A custom role may hold a permission the catalog lists as SUPPORTED or
// TESTING; one it does not list, or lists NOT_SUPPORTED, is refused
// "Permission P is not valid.", as Google documents for the REST API
// ("Creating and managing custom roles"). A project-level custom role may
// not hold a permission that works only on an organization or a folder
// ("Not applicable for project-level custom roles"): ProjectApplicable.
package iamroles

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"sort"
	"strings"
	"sync"
)

//go:embed permissions.tsv.gz
var catalogGz []byte

// Support is a permission's custom roles support level.
type Support int

const (
	// Unknown is a permission the catalog does not list.
	Unknown Support = iota
	Supported
	Testing
	NotSupported
)

var (
	loadOnce sync.Once
	catalog  map[string]Support
	names    []string
)

func load() {
	catalog = map[string]Support{}
	zr, err := gzip.NewReader(bytes.NewReader(catalogGz))
	if err != nil {
		panic("iamroles: the embedded permission catalog is not gzip: " + err.Error())
	}
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		name, level, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			continue
		}
		s := Supported
		switch level {
		case "T":
			s = Testing
		case "N":
			s = NotSupported
		}
		catalog[name] = s
		names = append(names, name)
	}
	sort.Strings(names)
}

// Level is p's custom roles support level.
func Level(p string) Support {
	loadOnce.Do(load)
	return catalog[p]
}

// Names are every permission in the catalog, sorted. The caller must not
// change the slice.
func Names() []string {
	loadOnce.Do(load)
	return names
}

// ProjectApplicable reports whether p can work in a project-level custom
// role: not one of the permissions on an organization or a folder itself
// (resourcemanager.organizations.get, resourcemanager.folders.list, as
// Google's "Roles and permissions" page names them, and the other
// services' organizations.* and folders.* permissions on those
// resources). Which other permissions Google treats as organization-only
// is UNVERIFIED (docs re-read 2026-11-17: https://docs.cloud.google.com/iam/docs/roles-overview).
func ProjectApplicable(p string) bool {
	parts := strings.Split(p, ".")
	if len(parts) != 3 {
		return true
	}
	switch parts[0] {
	case "resourcemanager", "compute", "cloudkms":
		return parts[1] != "organizations" && parts[1] != "folders"
	}
	return true
}

// Grants reports whether perms, a role's permissions, include p.
func Grants(perms []string, p string) bool {
	for _, g := range perms {
		if g == p {
			return true
		}
	}
	return false
}
