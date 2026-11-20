package iameval

import (
	"encoding/json"
	"regexp"
	"strings"
)

// The eighth round's checks (#1879): Compute Engine's field-level
// permissions, which a request's body asks for beyond its method's own
// (read 2026-11-20 from each method's REST reference,
// https://docs.cloud.google.com/compute/docs/reference/rest/v1/RESOURCE/METHOD,
// "Authorization requires the following IAM permission on the specified
// resource FIELD").

// FieldCheck is one further check a request's body asks for: Permission
// on Resource, or, when ActAs is set, iam.serviceAccounts.actAs on the
// service account Resource (an email) attached in Project.
type FieldCheck struct {
	Permission, Resource string
	ActAs                bool
	Project              string
}

var (
	computeInstancesPath = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/zones/([^/]+)/instances$`)
	computeAttachPath    = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/zones/([^/]+)/instances/([^/]+)/attachDisk$`)
	computeDisksPath     = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/zones/([^/]+)/disks$`)
	computeSubnetsPath   = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/regions/([^/]+)/subnetworks$`)
	computeAddrsPath     = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/regions/([^/]+)/addresses$`)
	computeNetPolicyPath = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/global/(firewalls|routes)$`)
)

// ComputeFieldChecks are the field-level checks of a Compute request,
// none for a method that names none. The pages read:
//
//   - instances.insert: networkInterfaces[].network compute.networks.use
//     (and compute.networks.useExternalIp with an access config),
//     networkInterfaces[].subnetwork compute.subnetworks.use (and
//     compute.subnetworks.useExternalIp), disks[].source compute.disks.use
//     (compute.disks.useReadOnly for a READ_ONLY disk),
//     disks[].initializeParams.sourceImage compute.images.useReadOnly,
//     .sourceSnapshot compute.snapshots.useReadOnly, .labels
//     compute.disks.setLabels, and compute.disks.create, which the
//     method's own list names, for a disk it creates; tags
//     compute.instances.setTags, metadata compute.instances.setMetadata,
//     labels compute.instances.setLabels, deletionProtection
//     compute.instances.setDeletionProtection, serviceAccounts[]
//     compute.instances.setServiceAccount and serviceAccounts[].email
//     iam.serviceAccounts.actAs ("default" is the Compute Engine default
//     service account). accessConfigs[].natIP's compute.addresses.use is
//     not checked: the instance is given no address of the project's (its
//     external IP is local).
//   - instances.attachDisk: source compute.disks.use (useReadOnly).
//   - disks.insert: sourceImage compute.images.useReadOnly, sourceSnapshot
//     compute.snapshots.useReadOnly, sourceDisk compute.disks.useReadOnly,
//     labels compute.disks.setLabels.
//   - subnetworks.insert, firewalls.insert and routes.insert: network
//     compute.networks.updatePolicy.
//   - addresses.insert: subnetwork compute.subnetworks.use, labels
//     compute.addresses.setLabels.
//
// A public image project's images (public) are readable by every caller,
// as Google's are, and need no check.
func ComputeFieldChecks(method, path string, body []byte, public func(project string) bool) []FieldCheck {
	if method != "POST" || len(body) == 0 {
		return nil
	}
	var b map[string]any
	if json.Unmarshal(body, &b) != nil {
		return nil
	}
	var out []FieldCheck
	add := func(perm, res string) { out = append(out, FieldCheck{Permission: perm, Resource: res}) }
	image := func(project, link string) {
		if link == "" {
			return
		}
		rel := computeRel(link, project, "global/images/")
		if p := Project(rel); public != nil && public(p) {
			return
		}
		add("compute.images.useReadOnly", rel)
	}
	if g := computeInstancesPath.FindStringSubmatch(path); g != nil {
		project, zone := g[1], g[2]
		region := zone
		if i := strings.LastIndex(zone, "-"); i > 0 {
			region = zone[:i]
		}
		instance := "projects/" + project + "/zones/" + zone + "/instances/" + jstr(b, "name")
		for _, n := range jlist(b, "networkInterfaces") {
			external := len(jlist(n, "accessConfigs")) > 0
			if nw := jstr(n, "network"); nw != "" {
				rel := computeRel(nw, project, "global/networks/")
				add("compute.networks.use", rel)
				if external {
					add("compute.networks.useExternalIp", rel)
				}
			}
			if sub := jstr(n, "subnetwork"); sub != "" {
				rel := computeRel(sub, project, "regions/"+region+"/subnetworks/")
				add("compute.subnetworks.use", rel)
				if external {
					add("compute.subnetworks.useExternalIp", rel)
				}
			}
		}
		created := false
		for _, d := range jlist(b, "disks") {
			computeDiskSource(d, project, zone, add)
			ip, _ := d["initializeParams"].(map[string]any)
			if ip == nil {
				continue
			}
			created = true
			image(project, jstr(ip, "sourceImage"))
			if s := jstr(ip, "sourceSnapshot"); s != "" {
				add("compute.snapshots.useReadOnly", computeRel(s, project, "global/snapshots/"))
			}
			if m, _ := ip["labels"].(map[string]any); len(m) > 0 {
				name := jstr(ip, "diskName")
				if name == "" {
					name = jstr(b, "name")
				}
				add("compute.disks.setLabels", "projects/"+project+"/zones/"+zone+"/disks/"+name)
			}
		}
		if created {
			add("compute.disks.create", "projects/"+project)
		}
		if t, _ := b["tags"].(map[string]any); len(jstrs(t, "items")) > 0 {
			add("compute.instances.setTags", instance)
		}
		if m, _ := b["metadata"].(map[string]any); len(jlist(m, "items")) > 0 {
			add("compute.instances.setMetadata", instance)
		}
		if m, _ := b["labels"].(map[string]any); len(m) > 0 {
			add("compute.instances.setLabels", instance)
		}
		if v, _ := b["deletionProtection"].(bool); v {
			add("compute.instances.setDeletionProtection", instance)
		}
		if sas := jlist(b, "serviceAccounts"); len(sas) > 0 {
			add("compute.instances.setServiceAccount", instance)
			for _, sa := range sas {
				email := jstr(sa, "email")
				if email == "" || email == "default" {
					email = project + "-compute@developer.gserviceaccount.com"
				}
				out = append(out, FieldCheck{Permission: ActAs, Resource: email, ActAs: true, Project: project})
			}
		}
		return out
	}
	if g := computeAttachPath.FindStringSubmatch(path); g != nil {
		computeDiskSource(b, g[1], g[2], add)
		return out
	}
	if g := computeDisksPath.FindStringSubmatch(path); g != nil {
		project, zone := g[1], g[2]
		image(project, jstr(b, "sourceImage"))
		if s := jstr(b, "sourceSnapshot"); s != "" {
			add("compute.snapshots.useReadOnly", computeRel(s, project, "global/snapshots/"))
		}
		if s := jstr(b, "sourceDisk"); s != "" {
			add("compute.disks.useReadOnly", computeRel(s, project, "zones/"+zone+"/disks/"))
		}
		if m, _ := b["labels"].(map[string]any); len(m) > 0 {
			add("compute.disks.setLabels", "projects/"+project+"/zones/"+zone+"/disks/"+jstr(b, "name"))
		}
		return out
	}
	if g := computeSubnetsPath.FindStringSubmatch(path); g != nil {
		if nw := jstr(b, "network"); nw != "" {
			add("compute.networks.updatePolicy", computeRel(nw, g[1], "global/networks/"))
		}
		return out
	}
	if g := computeNetPolicyPath.FindStringSubmatch(path); g != nil {
		if nw := jstr(b, "network"); nw != "" {
			add("compute.networks.updatePolicy", computeRel(nw, g[1], "global/networks/"))
		}
		return out
	}
	if g := computeAddrsPath.FindStringSubmatch(path); g != nil {
		project, region := g[1], g[2]
		if sub := jstr(b, "subnetwork"); sub != "" {
			add("compute.subnetworks.use", computeRel(sub, project, "regions/"+region+"/subnetworks/"))
		}
		if m, _ := b["labels"].(map[string]any); len(m) > 0 {
			add("compute.addresses.setLabels", "projects/"+project+"/regions/"+region+"/addresses/"+jstr(b, "name"))
		}
		return out
	}
	return nil
}

// computeDiskSource adds an attached disk's source check: compute.disks.use,
// or compute.disks.useReadOnly for a READ_ONLY attachment.
func computeDiskSource(d map[string]any, project, zone string, add func(perm, res string)) {
	src := jstr(d, "source")
	if src == "" {
		return
	}
	perm := "compute.disks.use"
	if jstr(d, "mode") == "READ_ONLY" {
		perm = "compute.disks.useReadOnly"
	}
	add(perm, computeRel(src, project, "zones/"+zone+"/disks/"))
}

// computeRel is a Compute link's relative name "projects/P/...": a full
// URL's, a partial URL's (global/..., regions/..., zones/... in project)
// or, for a bare name, collection's (in project) with the name.
func computeRel(link, project, collection string) string {
	if i := strings.Index(link, "/projects/"); i >= 0 && strings.Contains(link, "://") {
		return link[i+1:]
	}
	switch {
	case strings.HasPrefix(link, "projects/"):
		return link
	case strings.HasPrefix(link, "global/"), strings.HasPrefix(link, "regions/"), strings.HasPrefix(link, "zones/"):
		return "projects/" + project + "/" + link
	case !strings.Contains(link, "/"):
		return "projects/" + project + "/" + collection + link
	}
	return link
}

func jstr(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func jlist(m map[string]any, k string) []map[string]any {
	l, _ := m[k].([]any)
	var out []map[string]any
	for _, e := range l {
		if o, ok := e.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

func jstrs(m map[string]any, k string) []string {
	l, _ := m[k].([]any)
	var out []string
	for _, e := range l {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// APIKeysLookup is keys.lookupKey's permission, "on the parent project"
// of the key the request names by its key string
// (https://docs.cloud.google.com/api-keys/docs/reference/rest/v2/keys/lookupKey,
// read 2026-11-20; the access-control page's method table names
// apikeys.keys.undelete for it, which its role table does not grant for
// lookups: the reference page's is checked).
const APIKeysLookup = "apikeys.keys.lookup"

// ServiceUsageOperationsGet is operations.get's permission for both API
// Keys' and Service Usage's operations ("Access control",
// https://docs.cloud.google.com/api-keys/docs/access-control, read
// 2026-11-20), checked on the project the operation was made in, which
// its name carries by number. Service Usage's operations.list, cancel and
// delete name no permission (the permission index has only
// serviceusage.operations.get) and are not checked.
const ServiceUsageOperationsGet = "serviceusage.operations.get"
