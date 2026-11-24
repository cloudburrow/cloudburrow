package gke

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
)

// A node pool's instance groups (#1913). The Kubernetes Engine API names
// each pool's managed instance groups in instanceGroupUrls, one per zone,
// and the API's clients read a pool's size from them, not from the pool:
// Terraform's google_container_node_pool reads node_count as the instance
// group managers' targetSize, through Compute Engine's
// instanceGroupManagers.get. Without the compute service (#1936),
// this port answers those reads (get, and list by name) for its own pools' groups, under
// /compute/v1/ as Compute's paths are, and every other Compute path with
// 501: a client pointed here never falls through to Google. With it, the
// Compute Engine API's own port answers them too, through
// ServeInstanceGroupManagers, and cloudburrow terraform points
// compute_custom_endpoint there.

const computeBase = "https://www.googleapis.com/compute/v1/"

// igmName is the group's name: gke-CLUSTER-POOL-HASH-grp, a name of
// CloudBurrow's making; clients read the URL, not the name's parts.
func igmName(cluster, pool, zone string) string {
	name := "gke-" + cluster + "-" + pool
	if len(name) > 40 {
		name = name[:40]
	}
	return strings.TrimRight(name, "-") + "-" + shortHash(cluster+"/"+pool+"/"+zone) + "-grp"
}

// setInstanceGroups names a pool's groups, one per zone.
func setInstanceGroups(project, cluster string, np *containerpb.NodePool) {
	np.InstanceGroupUrls = nil
	for _, z := range np.GetLocations() {
		np.InstanceGroupUrls = append(np.InstanceGroupUrls,
			computeBase+"projects/"+project+"/zones/"+z+"/instanceGroupManagers/"+igmName(cluster, np.GetName(), z))
	}
}

var (
	igmPath     = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/zones/([^/]+)/instanceGroupManagers/([^/]+)$`)
	igmListPath = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/zones/([^/]+)/instanceGroupManagers$`)
	// igmFilter is the name comparison a list's filter makes: name = X,
	// name = "X", name eq X.
	igmFilter = regexp.MustCompile(`^\s*\(?\s*name\s*(?:=|eq)\s*"?([^"\s)]+)"?\s*\)?\s*$`)
)

// computeError writes Compute Engine's error body.
func computeError(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": msg, "errors": []map[string]string{{"domain": "global", "reason": reason, "message": msg}}}})
}

// zoneIGMs are the instance group managers of the pools in a project's
// zone, by name; s.mu is held.
func (s *Server) zoneIGMsLocked(project, zone string) map[string]map[string]any {
	prefix := computeBase + "projects/" + project + "/zones/" + zone + "/instanceGroupManagers/"
	out := map[string]map[string]any{}
	for _, e := range s.clusters {
		for _, np := range e.cluster.GetNodePools() {
			for _, u := range np.GetInstanceGroupUrls() {
				name, ok := strings.CutPrefix(u, prefix)
				if !ok {
					continue
				}
				out[name] = map[string]any{
					"kind": "compute#instanceGroupManager", "name": name, "selfLink": u,
					"zone":             computeBase + "projects/" + project + "/zones/" + zone,
					"instanceGroup":    computeBase + "projects/" + project + "/zones/" + zone + "/instanceGroups/" + name,
					"baseInstanceName": strings.TrimSuffix(name, "-grp"),
					"targetSize":       np.GetInitialNodeCount(),
					"status":           map[string]any{"isStable": np.GetStatus() == containerpb.NodePool_RUNNING},
				}
			}
		}
	}
	return out
}

// serveCompute answers instanceGroupManagers.get and .list for the pools'
// groups.
func (s *Server) serveCompute(w http.ResponseWriter, r *http.Request) {
	if s.ServeInstanceGroupManagers(w, r) {
		return
	}
	computeError(w, http.StatusNotImplemented, "notImplemented",
		fmt.Sprintf("Not available locally: CloudBurrow serves no Compute Engine API on the Kubernetes Engine port, only a node pool's instance group managers (%s %s); enable the compute service for its VPC networking.", r.Method, r.URL.Path))
}

// ServeInstanceGroupManagers answers instanceGroupManagers.get and .list
// for the pools' groups, and reports whether r was one of them: the
// Compute Engine API's own port (#1936) answers them through it.
func (s *Server) ServeInstanceGroupManagers(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet {
		if m := igmPath.FindStringSubmatch(r.URL.Path); m != nil {
			s.mu.Lock()
			igm := s.zoneIGMsLocked(m[1], m[2])[m[3]]
			s.mu.Unlock()
			if igm == nil {
				computeError(w, http.StatusNotFound, "notFound",
					fmt.Sprintf("The resource 'projects/%s/zones/%s/instanceGroupManagers/%s' was not found", m[1], m[2], m[3]))
				return true
			}
			writeJSON(w, igm)
			return true
		}
		if m := igmListPath.FindStringSubmatch(r.URL.Path); m != nil {
			want := ""
			if f := r.URL.Query().Get("filter"); f != "" {
				fm := igmFilter.FindStringSubmatch(f)
				if fm == nil {
					computeError(w, http.StatusNotImplemented, "notImplemented",
						fmt.Sprintf("Not available locally: a filter other than one name comparison (%q).", f))
					return true
				}
				want = fm[1]
			}
			s.mu.Lock()
			all := s.zoneIGMsLocked(m[1], m[2])
			s.mu.Unlock()
			names := make([]string, 0, len(all))
			for n := range all {
				if want == "" || n == want {
					names = append(names, n)
				}
			}
			sort.Strings(names)
			items := make([]map[string]any, 0, len(names))
			for _, n := range names {
				items = append(items, all[n])
			}
			writeJSON(w, map[string]any{"kind": "compute#instanceGroupManagerList", "items": items,
				"selfLink": computeBase + "projects/" + m[1] + "/zones/" + m[2] + "/instanceGroupManagers"})
			return true
		}
	}
	return false
}

// InstanceGroupManagers are a zone's node-pool groups, by name, for the
// Compute Engine API to list beside its own (#2139).
func (s *Server) InstanceGroupManagers(project, zone string) map[string]map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.zoneIGMsLocked(project, zone)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(v)
}
