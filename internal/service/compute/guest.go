package compute

import (
	"net/http"
	"sort"
	"strings"
)

// GuestPrefix begins the path a VM's guest reads its own metadata under
// (#1963): /cloudburrow/guest/TOKEN/computeMetadata/v1/..., TOKEN the
// instance's own, which its container is given. The guest scripts
// (startup-script runner, account manager) read instance and project
// attributes here; service account tokens come from the metadata server,
// which GCE_METADATA_HOST names in the guest.
const GuestPrefix = "/cloudburrow/guest/"

// serveGuest answers a guest's metadata read.
func (s *Server) serveGuest(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, GuestPrefix)
	token, path, _ := strings.Cut(rest, "/")
	if r.Header.Get("Metadata-Flavor") != "Google" {
		http.Error(w, "Missing Metadata-Flavor:Google header.", http.StatusForbidden)
		return
	}
	path = strings.TrimPrefix(path, "computeMetadata/v1/")
	s.mu.Lock()
	var inst obj
	for k, o := range s.res {
		if strings.Contains(k, "/instances/") && token != "" && str(o, "_guestToken") == token {
			inst = o
			break
		}
	}
	if inst == nil {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	project := strings.Split(str(inst, "selfLink"), "/")[1]
	pmeta := map[string]string{}
	if po, ok := s.res[projectRel(project)]; ok {
		pmeta = metadataItems(po["commonInstanceMetadata"])
	}
	imeta := metadataItems(inst["metadata"])
	val, ok := guestValue(inst, project, path, imeta, pmeta)
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Metadata-Flavor", "Google")
	w.Header().Set("Content-Type", "application/text")
	_, _ = w.Write([]byte(val))
}

func keyList(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return strings.Join(keys, "\n") + "\n"
}

func guestValue(inst obj, project, path string, imeta, pmeta map[string]string) (string, bool) {
	zone := lastSeg(str(inst, "zone"))
	ip := ""
	if nics, _ := inst["networkInterfaces"].([]any); len(nics) > 0 {
		ip = str(nics[0].(map[string]any), "networkIP")
	}
	host := str(inst, "hostname")
	if host == "" {
		host = str(inst, "name") + "." + zone + ".c." + project + ".internal"
	}
	switch {
	case path == "instance/id":
		return str(inst, "id"), true
	case path == "instance/name":
		return str(inst, "name"), true
	case path == "instance/hostname":
		return host, true
	case path == "instance/zone":
		return "projects/" + project + "/zones/" + zone, true
	case path == "instance/machine-type":
		return "projects/" + project + "/machineTypes/" + lastSeg(str(inst, "machineType")), true
	case path == "instance/network-interfaces/0/ip":
		return ip, true
	case path == "instance/attributes/":
		return keyList(imeta), true
	case strings.HasPrefix(path, "instance/attributes/"):
		v, ok := imeta[strings.TrimPrefix(path, "instance/attributes/")]
		return v, ok
	case path == "project/project-id":
		return project, true
	case path == "project/attributes/":
		return keyList(pmeta), true
	case strings.HasPrefix(path, "project/attributes/"):
		v, ok := pmeta[strings.TrimPrefix(path, "project/attributes/")]
		return v, ok
	}
	return "", false
}

func projectRel(project string) string { return "projects/" + project + "/project" }

// project is projects.get: the project's common instance metadata and
// default service account.
func (s *Server) project(c call) (obj, error) {
	rel := projectRel(c.project)
	o, ok := s.res[rel]
	if !ok {
		o = obj{"kind": "compute#project", "id": stableID(c.project), "name": c.project,
			"commonInstanceMetadata": obj{"kind": "compute#metadata", "fingerprint": metadataFingerprint([]any{})},
			"defaultServiceAccount":  c.project + "-compute@developer.gserviceaccount.com",
			"creationTimestamp":      s.stamp(), "xpnProjectStatus": "UNSPECIFIED_XPN_PROJECT_STATUS",
			"defaultNetworkTier": "PREMIUM", "vmDnsSetting": "ZONAL_ONLY"}
		s.res[rel] = o
	}
	out := copyObj(o)
	out["selfLink"] = "projects/" + c.project
	return out, nil
}

// setCommonInstanceMetadata replaces the project's metadata under its
// fingerprint.
func (s *Server) setCommonInstanceMetadata(c call) (obj, error) {
	if _, err := s.project(c); err != nil {
		return nil, err
	}
	o := s.res[projectRel(c.project)]
	cur, _ := o["commonInstanceMetadata"].(map[string]any)
	if fp := str(c.body, "fingerprint"); fp != "" && fp != str(cur, "fingerprint") {
		return nil, errf(http.StatusPreconditionFailed, "conditionNotMet",
			"Supplied fingerprint does not match current metadata fingerprint.")
	}
	md, err := checkMetadata("commonInstanceMetadata", c.body)
	if err != nil {
		return nil, err
	}
	md["fingerprint"] = newFingerprint()
	o["commonInstanceMetadata"] = norm(md)
	return s.done(c.project, "global", "setMetadata", "projects/"+c.project, o["id"])
}
