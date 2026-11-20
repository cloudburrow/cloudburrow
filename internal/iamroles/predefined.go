package iamroles

// Predefined roles (#1746). roles.tsv.gz is every role Google's "IAM roles
// and permissions index" lists (https://docs.cloud.google.com/iam/docs/roles-permissions
// and its 357 per-service pages, .../roles-permissions/SERVICE, read
// 2026-11-17), one per line, tab-separated: the name (roles/...), the
// title, the stage (BETA for a role the page marks Beta, else GA), the
// description (the page's first paragraph of it), the lowest-level
// resources the page says the role can be granted on (comma-separated,
// often none), and the permissions it includes (comma-separated, the
// page's wildcard groups expanded as the page expands them). 2,423 roles.
// To refresh it, fetch the index and every page it links to and extract
// each role-description/role-permissions table row the same way.
//
// products.tsv.gz is each of those roles' product (#2013): the title of
// the per-service page that lists it, as the index's menu names that page
// ("Compute Engine" for .../roles-permissions/compute), read 2026-11-20,
// one "name<TAB>product" per line. Google's console groups Select a role
// by product; this is the grouping the docs give. The nine job function
// roles (roles/iam.devOps ...) are on "Roles for job functions". To
// refresh it, take each page's role-title ids (class="role-title" id="X"
// is roles/X) with the page's menu title.
//
// The basic roles (roles/owner, editor, viewer, admin, writer, reader)
// are not on those pages; their titles are from "Roles overview"
// (https://docs.cloud.google.com/iam/docs/roles-overview, read
// 2026-11-17), which does not list their permissions (it points to the
// console): they are served without included permissions.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"sort"
	"strings"
	"sync"
)

//go:embed roles.tsv.gz
var rolesGz []byte

//go:embed products.tsv.gz
var productsGz []byte

// BasicProduct is the basic roles' group in Select a role.
const BasicProduct = "Basic"

// Role is a predefined role.
type Role struct {
	Name        string
	Title       string
	Description string
	// Stage is GA or BETA.
	Stage string
	// Lowest are the lowest-level resources Google's page says the role
	// can be granted on ("Project", "Bucket", "Organization"), often none.
	Lowest      []string
	Permissions []string
	// Product is the product Google's docs list the role under ("Cloud
	// Storage"), BasicProduct for a basic role.
	Product string
}

// basicRoles are the basic roles, from "Roles overview".
var basicRoles = []Role{
	{Name: "roles/owner", Title: "Owner", Description: "All Editor permissions, plus permissions for actions like managing roles and permissions for a project and all resources within the project, and setting up billing for a project.", Stage: "GA"},
	{Name: "roles/editor", Title: "Editor", Description: "All viewer permissions, plus permissions for actions that modify state, such as changing existing resources.", Stage: "GA"},
	{Name: "roles/viewer", Title: "Viewer", Description: "Permissions for read-only actions that don't affect state, such as viewing (but not modifying) existing resources or data.", Stage: "GA"},
	{Name: "roles/admin", Title: "Admin", Description: "All of the permissions in the Writer role, plus permissions for actions like managing roles and permissions for a project and all resources within the project, and setting up billing for a project.", Stage: "GA"},
	{Name: "roles/writer", Title: "Writer", Description: "All of the permissions in the Reader role, plus permissions for actions that modify state, such as changing existing resources.", Stage: "GA"},
	{Name: "roles/reader", Title: "Reader", Description: "Permissions for read-only actions that don't affect state, such as viewing (but not modifying) existing resources or data.", Stage: "GA"},
}

var (
	rolesOnce sync.Once
	roles     map[string]*Role
	roleNames []string
)

func loadRoles() {
	roles = map[string]*Role{}
	for i := range basicRoles {
		roles[basicRoles[i].Name] = &basicRoles[i]
	}
	zr, err := gzip.NewReader(bytes.NewReader(rolesGz))
	if err != nil {
		panic("iamroles: the embedded role catalog is not gzip: " + err.Error())
	}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 6 {
			continue
		}
		r := &Role{Name: f[0], Title: f[1], Stage: f[2], Description: f[3], Lowest: split(f[4]), Permissions: split(f[5])}
		roles[r.Name] = r
	}
	if err := sc.Err(); err != nil {
		panic("iamroles: read the embedded role catalog: " + err.Error())
	}
	pz, err := gzip.NewReader(bytes.NewReader(productsGz))
	if err != nil {
		panic("iamroles: the embedded product catalog is not gzip: " + err.Error())
	}
	ps := bufio.NewScanner(pz)
	for ps.Scan() {
		if name, product, ok := strings.Cut(ps.Text(), "\t"); ok && roles[name] != nil {
			roles[name].Product = product
		}
	}
	if err := ps.Err(); err != nil {
		panic("iamroles: read the embedded product catalog: " + err.Error())
	}
	for i := range basicRoles {
		basicRoles[i].Product = BasicProduct
	}
	for n := range roles {
		roleNames = append(roleNames, n)
	}
	sort.Strings(roleNames)
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// Predefined is the predefined role named name (roles/...), nil when
// Google lists none. The caller must not change it.
func Predefined(name string) *Role {
	rolesOnce.Do(loadRoles)
	return roles[name]
}

// PredefinedNames are every predefined role's name, sorted. The caller
// must not change the slice.
func PredefinedNames() []string {
	rolesOnce.Do(loadRoles)
	return roleNames
}

// GrantableOn reports whether a predefined role can be granted on a
// resource of kind "Organization", "Folder" or "Project". Every role can
// be granted on the organization. A role whose lowest-level resources, as
// Google's page lists them, are only the organization (or a billing
// account, which is not under a folder or project) is not grantable below
// it, nor, on a project, one whose lowest are only folders; and a role
// with no permission that can work on a project
// (ProjectApplicable) is not grantable on one. Which roles Google's
// queryGrantableRoles answers for a folder or project beyond these is
// UNVERIFIED (docs re-read 2026-11-17: https://docs.cloud.google.com/iam/docs/reference/rest/v1/roles/queryGrantableRoles).
func (r *Role) GrantableOn(kind string) bool {
	if kind == "Organization" {
		return true
	}
	if len(r.Lowest) > 0 {
		below := false
		for _, l := range r.Lowest {
			switch l {
			case "Organization", "Billing Account":
			case "Folder":
				below = below || kind == "Folder"
			default:
				below = true
			}
		}
		if !below {
			return false
		}
	}
	if kind == "Project" && len(r.Permissions) > 0 {
		for _, p := range r.Permissions {
			if ProjectApplicable(p) {
				return true
			}
		}
		return false
	}
	return true
}
