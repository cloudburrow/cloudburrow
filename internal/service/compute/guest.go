package compute

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/resource"
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
		g := s.gatewayByTokenLocked(token)
		sp, isSpeaker := s.speakerByTokenLocked(token)
		ng, isNAT := s.natGatewayByTokenLocked(token)
		s.mu.Unlock()
		if isNAT && path == vpcSuffix {
			s.serveNATVPC(w, r, ng)
			return
		}
		if g != nil && path == vpcSuffix {
			s.serveGatewayVPC(w, r, g)
			return
		}
		if isSpeaker && path == vpcSuffix {
			s.serveSpeakerVPC(w, r, sp)
			return
		}
		http.NotFound(w, r)
		return
	}
	if path == vpcSuffix {
		s.mu.Unlock()
		s.serveVPC(w, r, inst)
		return
	}
	if guestDir(path) {
		// A directory named without its slash: Google's metadata server
		// answers 301 to the directory (#2139).
		s.mu.Unlock()
		loc := r.URL.Path + "/"
		if r.URL.RawQuery != "" {
			loc += "?" + r.URL.RawQuery
		}
		w.Header().Set("Metadata-Flavor", "Google")
		http.Redirect(w, r, loc, http.StatusMovedPermanently)
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
	case path == "instance/tags":
		// A JSON list, as Google's metadata server writes it (#2139).
		tags := []string{}
		if t, ok := inst["tags"].(map[string]any); ok {
			tags = append(tags, strsAny(t["items"])...)
		}
		b, _ := json.Marshal(tags)
		return string(b), true
	case strings.HasPrefix(path, "instance/network-interfaces/"):
		return nicValue(inst, strings.TrimPrefix(path, "instance/network-interfaces/"))
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

// guestDir is whether path names one of the metadata directories served,
// without its trailing slash.
func guestDir(path string) bool {
	switch path {
	case "instance/attributes", "project/attributes", "instance/network-interfaces":
		return true
	}
	rest, ok := strings.CutPrefix(path, "instance/network-interfaces/")
	if !ok {
		return false
	}
	p := strings.Split(rest, "/")
	switch {
	case len(p) == 1, len(p) == 2 && p[1] == "access-configs", len(p) == 3 && p[1] == "access-configs":
		return isIndex(p[0]) && (len(p) < 3 || isIndex(p[2]))
	}
	return false
}

func isIndex(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 0 && strconv.Itoa(n) == s
}

// nicValue answers instance/network-interfaces/REST from the instance's
// networkInterfaces, as Google's metadata server lists and reads them
// (https://docs.cloud.google.com/compute/docs/metadata/predefined-metadata-keys,
// #2139): the directory lists each interface as "N/"; an interface's
// directory its keys; ip its internal IPv4 address, mac its MAC, network
// projects/PROJECT_NUMBER/networks/NAME, and access-configs/ its access
// configs, each with external-ip (its natIP) and type (ONE_TO_ONE_NAT).
// An interface without an access config lists none, and its external-ip
// is not found.
func nicValue(inst obj, rest string) (string, bool) {
	nics := listOf(inst["networkInterfaces"])
	if rest == "" {
		var b strings.Builder
		for i := range nics {
			fmt.Fprintf(&b, "%d/\n", i)
		}
		return b.String(), true
	}
	p := strings.Split(rest, "/")
	if !isIndex(p[0]) {
		return "", false
	}
	i, _ := strconv.Atoi(p[0])
	if i >= len(nics) {
		return "", false
	}
	nic := objOf(nics[i])
	acs := listOf(nic["accessConfigs"])
	// An interface without an external IP still shows access config 0 with an
	// empty external-ip, as on Compute Engine: the Ops Agent's resource
	// detector (metadata.ExternalIP) treats a missing key as fatal.
	if len(acs) == 0 {
		acs = []any{obj{}}
	}
	v6 := str(nic, "ipv6Address")
	for _, a := range listOf(nic["ipv6AccessConfigs"]) {
		if v6 == "" {
			v6 = str(objOf(a), "externalIpv6")
		}
	}
	key := strings.Join(p[1:], "/")
	switch {
	case key == "":
		keys := "ip\nmac\nnetwork\n"
		if v6 != "" {
			keys = "ip\nipv6s\nmac\nnetwork\n"
		}
		return "access-configs/\n" + keys, true
	case key == "ip":
		return str(nic, "networkIP"), true
	case key == "ipv6s" && v6 != "":
		// The interface's IPv6 address (dual-stack or IPv6-only, ipv6.go).
		return v6 + "\n", true
	case key == "mac":
		return vmNet{ip: str(nic, "networkIP"), ip6: v6}.mac(), true
	case key == "network":
		parts := strings.Split(Rel(str(nic, "network"), ""), "/")
		if len(parts) < 5 || parts[0] != "projects" {
			return "", false
		}
		return "projects/" + strconv.FormatInt(resource.ProjectNumber(parts[1]), 10) + "/networks/" + parts[len(parts)-1], true
	case key == "access-configs/":
		var b strings.Builder
		for j := range acs {
			fmt.Fprintf(&b, "%d/\n", j)
		}
		return b.String(), true
	case len(p) >= 3 && p[1] == "access-configs" && isIndex(p[2]):
		j, _ := strconv.Atoi(p[2])
		if j >= len(acs) {
			return "", false
		}
		ac := objOf(acs[j])
		switch strings.Join(p[3:], "/") {
		case "":
			return "external-ip\ntype\n", true
		case "external-ip":
			return str(ac, "natIP"), true
		case "type":
			t := str(ac, "type")
			if t == "" {
				t = "ONE_TO_ONE_NAT"
			}
			return t, true
		}
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
