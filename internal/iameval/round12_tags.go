package iameval

import (
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// A tag binding on another service's resource (#1879, round 12): "To
// attach or remove tags on specific service resources, principals also
// need the resource-specific tag binding permissions", such as
// compute.instances.createTagBinding and storage.buckets.createTagBinding
// ("Creating and managing tags",
// https://docs.cloud.google.com/resource-manager/docs/tags/tags-creating-and-managing,
// read 2026-11-20). Each is SERVICE.COLLECTION.VERB of the resource's
// service and last collection, checked on the resource in that service,
// with its own policies where that service's table keeps them; a
// resource whose permission Google's index lacks is not checked.

// tagPermissionPrefix is a host's permission prefix where it is not the
// host's first label.
var tagPermissionPrefix = map[string]string{
	"sqladmin.googleapis.com":      "cloudsql",
	"bigtableadmin.googleapis.com": "bigtable",
	"firestore.googleapis.com":     "datastore",
}

// tagServices are the tables of hosts no table of their own names.
var tagServices sync.Map

// tagService is host's table, its own where one is enforced.
func tagService(host string) *Service {
	if s := ServiceByHost(host); s != nil {
		return s
	}
	s, _ := tagServices.LoadOrStore(host, &Service{Name: host, Deny: newerDeny})
	return s.(*Service)
}

// TagResourceCheck is the check verb ("createTagBinding",
// "deleteTagBinding", "listTagBindings", "listEffectiveTags") needs on
// another service's resource named by full ("//storage.googleapis.com/projects/_/buckets/b"),
// ok false for Resource Manager's own or a resource the index has no such
// permission for.
func TagResourceCheck(full, verb string) (Check, bool) {
	rest, ok := strings.CutPrefix(full, "//")
	if !ok || HierarchyNode(full) != "" {
		return Check{}, false
	}
	host, rel, ok := strings.Cut(rest, "/")
	if !ok || rel == "" {
		return Check{}, false
	}
	parts := strings.Split(rel, "/")
	if len(parts)%2 != 0 {
		return Check{}, false
	}
	prefix := tagPermissionPrefix[host]
	if prefix == "" {
		prefix, _, _ = strings.Cut(host, ".")
	}
	permission := prefix + "." + parts[len(parts)-2] + "." + verb
	if _, ok := slices.BinarySearch(iamroles.Names(), permission); !ok {
		return Check{}, false
	}
	return Check{Permission: permission, Resource: rel, Service: tagService(host)}, true
}

// tagResourceChecks are TagResourceCheck's check, none or one.
func tagResourceChecks(full, verb string) []Check {
	if c, ok := TagResourceCheck(full, verb); ok {
		return []Check{c}
	}
	return nil
}

// tagBindingResource is the resource a binding's name
// ("tagBindings/{url-encoded resource}/tagValues/{id}") binds, "" for
// none.
func tagBindingResource(name string) string {
	rest, ok := strings.CutPrefix(name, "tagBindings/")
	if !ok {
		return ""
	}
	i := strings.LastIndex(rest, "/tagValues/")
	if i < 0 {
		return ""
	}
	full, err := url.PathUnescape(rest[:i])
	if err != nil {
		return ""
	}
	return full
}
