package sqladmin

import (
	"net"
	"net/http"
	"strings"
)

// Networks are the Compute Engine VPC networks (#1936) a Cloud SQL
// instance's private IP settings are checked against, as Google checks
// them, when the instance serves Compute Engine; without it (nil) any
// network is accepted, as before. internal/service/compute's Server is
// one.
type Networks interface {
	// HasNetwork reports whether the network ref exists, in project when
	// ref is a bare name.
	HasNetwork(ref, project string) bool
	// AllocatedRange is the range of a VPC_PEERING global address name
	// allocated on network (projects/P/global/networks/N).
	AllocatedRange(network, name string) (*net.IPNet, bool)
}

// SetNetworks makes instances.insert, patch and update check
// settings.ipConfiguration.privateNetwork and allocatedIpRange against
// nets(), read at each call; nil checks nothing.
func (s *Server) SetNetworks(nets func() Networks) {
	s.emu.Lock()
	s.networks = nets
	s.emu.Unlock()
}

// checkPrivateNetwork refuses a private IP network that does not exist,
// and an allocated range that is not one of that network's
// (https://docs.cloud.google.com/sql/docs/postgres/configure-private-ip and
// the ipConfiguration reference, re-read 2026-11-19). The wording is
// UNVERIFIED.
func (s *Server) checkPrivateNetwork(project string, settings map[string]any) error {
	s.emu.RLock()
	get := s.networks
	s.emu.RUnlock()
	if get == nil {
		return nil
	}
	nets := get()
	if nets == nil {
		return nil
	}
	ic, _ := settings["ipConfiguration"].(map[string]any)
	pn, _ := ic["privateNetwork"].(string)
	if pn == "" {
		return nil
	}
	if !nets.HasNetwork(pn, project) {
		return Errorf(http.StatusBadRequest, "Invalid request: the network %s of settings.ipConfiguration.privateNetwork does not exist.", pn)
	}
	if ar, _ := ic["allocatedIpRange"].(string); ar != "" {
		rel := pn
		if i := strings.Index(pn, "projects/"); i > 0 {
			rel = pn[i:]
		}
		if _, ok := nets.AllocatedRange(rel, ar); !ok {
			return Errorf(http.StatusBadRequest, "Invalid request: settings.ipConfiguration.allocatedIpRange %q is not an IP range allocated for private services access on %s.", ar, pn)
		}
	}
	return nil
}
