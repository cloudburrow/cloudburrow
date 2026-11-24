package compute

import (
	"net/http"
	"sort"
	"strings"
)

// Shared VPC (#2140): the projects resource's enableXpnHost,
// disableXpnHost, enableXpnResource, disableXpnResource, getXpnHost,
// getXpnResources and listXpnHosts
// (https://docs.cloud.google.com/compute/docs/reference/rest/v1/projects,
// and https://docs.cloud.google.com/vpc/docs/shared-vpc and
// provisioning-shared-vpc, read 2026-11-22), with Google's rules:
//
//   - Shared VPC is for projects of an organization: "subnets ... to other
//     service projects in the same organization". A project the Resource
//     Manager registry keeps without a parent (a top-level project, outside
//     the local organization) can be neither a host nor a service project
//     (Options.ProjectOrganization);
//   - a host project cannot be a service project, nor a service project a
//     host;
//   - "A service project can only attach to one host project";
//   - a host project with attached service projects cannot be disabled,
//     and a service project whose instances or addresses use the host's
//     subnets cannot be detached;
//   - a VM or internal address of another project may use a subnet only
//     when its project is attached to the subnet's host project
//     ("Cross project referencing is not allowed for this resource"
//     otherwise). compute.subnetworks.use on the host's subnet is checked
//     by IAM enforcement (internal/iameval round8), as on Google.
//
// A service project's VMs are on the host network's VXLAN segment
// (vpc.go keys segments by network), under the host project's firewall
// rules, as Google's are.

// xpnStatus is the project's xpnProjectStatus.
const (
	xpnHost = "HOST"
	xpnNone = "UNSPECIFIED_XPN_PROJECT_STATUS"
)

// xpnMethod is whether name is one of the projects resource's Shared VPC
// methods.
func xpnMethod(name string) bool {
	switch name {
	case "enableXpnHost", "disableXpnHost", "enableXpnResource", "disableXpnResource", "getXpnHost", "getXpnResources", "listXpnHosts":
		return true
	}
	return false
}

// projectLocked is the stored project object, made as projects.get makes
// it.
func (s *Server) projectLocked(project string) obj {
	if _, err := s.project(call{project: project}); err != nil {
		return nil
	}
	return s.res[projectRel(project)]
}

// xpnHostOfLocked is the host project the project is attached to, or "".
func (s *Server) xpnHostOfLocked(project string) string {
	o, ok := s.res[projectRel(project)]
	if !ok {
		return ""
	}
	return str(o, "_xpnHost")
}

// isXpnHostLocked is whether the project is a Shared VPC host project.
func (s *Server) isXpnHostLocked(project string) bool {
	o, ok := s.res[projectRel(project)]
	return ok && str(o, "xpnProjectStatus") == xpnHost
}

// xpnServicesLocked are the service projects attached to host, sorted.
func (s *Server) xpnServicesLocked(host string) []string {
	var out []string
	for k, o := range s.res {
		if strings.HasSuffix(k, "/project") && str(o, "_xpnHost") == host {
			out = append(out, strings.Split(k, "/")[1])
		}
	}
	sort.Strings(out)
	return out
}

// checkXpnOrg refuses a project outside an organization.
func (s *Server) checkXpnOrg(project string) (string, error) {
	if s.opts.ProjectOrganization == nil {
		return "", nil
	}
	org, known := s.opts.ProjectOrganization(project)
	if known && org == "" {
		return "", errf(http.StatusBadRequest, "invalid",
			"Project '%s' does not belong to an organization. Shared VPC is available only to projects in an organization.", project)
	}
	return org, nil
}

func (s *Server) xpn(c call) (obj, error) {
	get, post := c.method == http.MethodGet, c.method == http.MethodPost
	switch name := c.parts[0]; {
	case name == "enableXpnHost" && post:
		return s.enableXpnHost(c.project)
	case name == "disableXpnHost" && post:
		return s.disableXpnHost(c.project)
	case name == "enableXpnResource" && post:
		return s.xpnResource(c.project, c.body, true)
	case name == "disableXpnResource" && post:
		return s.xpnResource(c.project, c.body, false)
	case name == "getXpnHost" && get:
		host := s.xpnHostOfLocked(c.project)
		if host == "" {
			// No host: an empty project, as Google answers it.
			return obj{}, nil
		}
		return s.project(call{project: host})
	case name == "getXpnResources" && get:
		if s.projectLocked(c.project) == nil {
			return nil, notFound("projects/" + c.project)
		}
		var all []obj
		for _, p := range s.xpnServicesLocked(c.project) {
			all = append(all, obj{"id": p, "type": "PROJECT", "name": p})
		}
		items, next, err := page(all, c.query)
		if err != nil {
			return nil, err
		}
		res := make([]any, len(items))
		for i, it := range items {
			res[i] = obj{"id": it["id"], "type": it["type"]}
		}
		out := obj{"kind": "compute#projectsGetXpnResources", "resources": res}
		if next != "" {
			out["nextPageToken"] = next
		}
		return out, nil
	case name == "listXpnHosts" && post:
		return s.listXpnHosts(c)
	}
	return nil, unimplemented("%s projects/%s/%s is not a Shared VPC method", c.method, c.project, c.parts[0])
}

func (s *Server) enableXpnHost(project string) (obj, error) {
	o := s.projectLocked(project)
	if o == nil {
		return nil, notFound("projects/" + project)
	}
	if _, err := s.checkXpnOrg(project); err != nil {
		return nil, err
	}
	if h := str(o, "_xpnHost"); h != "" {
		return nil, errf(http.StatusBadRequest, "invalid",
			"Cannot enable project '%s' as a shared VPC host project: it is a service project of the shared VPC host project '%s'.", project, h)
	}
	o["xpnProjectStatus"] = xpnHost
	return s.done(project, "global", "enableXpnHost", "projects/"+project, o["id"])
}

func (s *Server) disableXpnHost(project string) (obj, error) {
	o := s.projectLocked(project)
	if o == nil {
		return nil, notFound("projects/" + project)
	}
	if svcs := s.xpnServicesLocked(project); len(svcs) > 0 {
		return nil, errf(http.StatusBadRequest, "invalid",
			"Cannot disable the shared VPC host project '%s': service projects are still attached to it (%s). Detach them first.", project, strings.Join(svcs, ", "))
	}
	o["xpnProjectStatus"] = xpnNone
	return s.done(project, "global", "disableXpnHost", "projects/"+project, o["id"])
}

// xpnResource is enableXpnResource (attach) and disableXpnResource
// (detach) of a service project on the host project.
func (s *Server) xpnResource(host string, b obj, enable bool) (obj, error) {
	r, _ := b["xpnResource"].(map[string]any)
	id := str(r, "id")
	if id == "" {
		return nil, required("xpnResource.id")
	}
	switch t := str(r, "type"); t {
	case "", "PROJECT":
	default:
		return nil, invalid("Invalid value for field 'xpnResource.type': '%s'. Must be PROJECT.", t)
	}
	ho := s.projectLocked(host)
	if ho == nil {
		return nil, notFound("projects/" + host)
	}
	if str(ho, "xpnProjectStatus") != xpnHost {
		return nil, errf(http.StatusBadRequest, "invalid", "Project '%s' is not a shared VPC host project.", host)
	}
	op := "enableXpnResource"
	if !enable {
		op = "disableXpnResource"
	}
	if !enable {
		so, ok := s.res[projectRel(id)]
		if !ok || str(so, "_xpnHost") != host {
			return nil, errf(http.StatusBadRequest, "invalid", "Project '%s' is not a service project of the shared VPC host project '%s'.", id, host)
		}
		if u := s.sharedUsersLocked(id, host); len(u) > 0 {
			return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
				"Cannot detach service project '%s': its resources use the shared VPC host project's network: %s.", id, strings.Join(u, ", "))
		}
		delete(so, "_xpnHost")
		return s.done(host, "global", op, "projects/"+host, ho["id"])
	}
	if id == host {
		return nil, errf(http.StatusBadRequest, "invalid", "A shared VPC host project cannot be attached to itself as a service project.")
	}
	hostOrg, err := s.checkXpnOrg(host)
	if err != nil {
		return nil, err
	}
	svcOrg, err := s.checkXpnOrg(id)
	if err != nil {
		return nil, err
	}
	if hostOrg != "" && svcOrg != "" && hostOrg != svcOrg {
		return nil, errf(http.StatusBadRequest, "invalid", "Project '%s' is not in the organization of the shared VPC host project '%s'.", id, host)
	}
	so := s.projectLocked(id)
	if so == nil {
		return nil, notFound("projects/" + id)
	}
	if str(so, "xpnProjectStatus") == xpnHost {
		return nil, errf(http.StatusBadRequest, "invalid", "Project '%s' is a shared VPC host project and cannot be attached as a service project.", id)
	}
	if h := str(so, "_xpnHost"); h != "" && h != host {
		return nil, errf(http.StatusBadRequest, "invalid",
			"Project '%s' is already a service project of the shared VPC host project '%s'. A service project can only attach to one host project.", id, h)
	}
	so["_xpnHost"] = host
	return s.done(host, "global", op, "projects/"+host, ho["id"])
}

// sharedUsersLocked are the service project's resources on a subnet or
// network of the host project.
func (s *Server) sharedUsersLocked(service, host string) []string {
	var out []string
	hp := "projects/" + host + "/"
	for k, o := range s.res {
		if !strings.HasPrefix(k, "projects/"+service+"/") {
			continue
		}
		used := strings.HasPrefix(str(o, "subnetwork"), hp) || strings.HasPrefix(str(o, "network"), hp)
		for _, e := range sliceOf(o["networkInterfaces"]) {
			nic, _ := e.(map[string]any)
			used = used || strings.HasPrefix(str(nic, "subnetwork"), hp)
		}
		if used {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// listXpnHosts lists the host projects of the organization (the body's
// organization, else the project's own).
func (s *Server) listXpnHosts(c call) (obj, error) {
	org := str(c.body, "organization")
	if org == "" && s.opts.ProjectOrganization != nil {
		org, _ = s.opts.ProjectOrganization(c.project)
	}
	var all []obj
	var keys []string
	for k, o := range s.res {
		if strings.HasSuffix(k, "/project") && str(o, "xpnProjectStatus") == xpnHost {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := strings.Split(k, "/")[1]
		if org != "" && s.opts.ProjectOrganization != nil {
			if po, _ := s.opts.ProjectOrganization(p); po != org {
				continue
			}
		}
		o, _ := s.project(call{project: p})
		all = append(all, o)
	}
	items, next, err := page(all, c.query)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []obj{}
	}
	out := obj{"kind": "compute#xpnHostList", "id": "projects/" + c.project, "items": items, "selfLink": "projects/" + c.project + "/listXpnHosts"}
	if next != "" {
		out["nextPageToken"] = next
	}
	return out, nil
}

// checkSharedSubnetLocked refuses a subnet of another project unless
// project is a service project of that project.
func (s *Server) checkSharedSubnetLocked(project, sub, field string) error {
	parts := strings.Split(sub, "/")
	if len(parts) < 2 || parts[0] != "projects" || parts[1] == project {
		return nil
	}
	owner := parts[1]
	if s.xpnHostOfLocked(project) == owner && s.isXpnHostLocked(owner) {
		return nil
	}
	return invalid("Invalid value for field '%s': '%s'. Cross project referencing is not allowed for this resource.", field, sub)
}
