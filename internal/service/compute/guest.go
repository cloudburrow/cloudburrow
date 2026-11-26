package compute

import (
	"net/http"
	"strings"
)

// GuestPrefix begins the path a VM's guest reads its own metadata under
// (#1963): /cloudburrow/guest/TOKEN/computeMetadata/v1/..., TOKEN the
// instance's own, which its container is given. The guest's metadata
// server relay (guest/root/usr/lib/cloudburrow/metadata-server) carries
// metadata.google.internal and 169.254.169.254 here, so the whole metadata
// server, the service account's tokens included, is the instance's own
// (#2177, guestmetadata.go).
const GuestPrefix = "/cloudburrow/guest/"

// firmwarePrefix is where the guest's firmware unit reads the instance's
// Google UEFI variables (#2179, mdsmtls.go).
const firmwarePrefix = "cloudburrow/firmware/"

// serveGuest answers a guest's metadata read.
func (s *Server) serveGuest(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, GuestPrefix)
	token, path, _ := strings.Cut(rest, "/")
	s.mu.Lock()
	var inst obj
	rel := ""
	for k, o := range s.res {
		if strings.Contains(k, "/instances/") && token != "" && str(o, "_guestToken") == token {
			inst, rel = o, k
			break
		}
	}
	if path == vpcSuffix && r.Header.Get("Metadata-Flavor") != "Google" {
		s.mu.Unlock()
		http.Error(w, "Missing Metadata-Flavor:Google header.", http.StatusForbidden)
		return
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
	s.mu.Unlock()
	if path == vpcSuffix {
		s.serveVPC(w, r, inst)
		return
	}
	if name, ok := strings.CutPrefix(path, firmwarePrefix); ok {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "Missing Metadata-Flavor:Google header.", http.StatusForbidden)
			return
		}
		s.serveFirmware(w, r, rel, name)
		return
	}
	s.serveInstanceMetadata(w, r, rel, path)
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
	if str(out, "cloudArmorTier") == "" {
		out["cloudArmorTier"] = "CA_STANDARD"
	}
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
	if fp := str(c.body, "fingerprint"); fp != "" && !sameFingerprint(fp, str(cur, "fingerprint")) {
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
