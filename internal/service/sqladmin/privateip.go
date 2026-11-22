package sqladmin

// Zones and private IP (#2141), as the Admin API reference and
// https://docs.cloud.google.com/sql/docs/postgres/configure-private-ip
// (read 2026-11-22) describe them:
//
//   - settings.locationPreference.zone is "the preferred Compute Engine
//     zone" and secondaryZone "the preferred Compute Engine zone for the
//     secondary/failover"; gceZone is the zone the instance serves from
//     and secondaryGceZone its standby's, swapped by a failover. Each must
//     be a zone of the instance's region. Unset, the region's first zone
//     serves and its second holds the standby.
//   - settings.ipConfiguration.privateNetwork puts the instance on a VPC
//     network through private services access: its private IP is taken
//     from the range allocatedIpRange names, or else from any VPC_PEERING
//     range allocated on the network, and answered as an ipAddresses entry
//     of type PRIVATE ("Private IP used when using private IPs and network
//     peering"). A network with no allocated range has no private services
//     connection, and the instance is refused as Google refuses it
//     (NETWORK_NOT_PEERED). ipv4Enabled false drops the PRIMARY address.
//   - The address is reachable from the network's VMs (PrivateEndpoints):
//     Compute Engine's VPC model sends a VM's connections to it on to the
//     instance's server.

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// zones are a region's zones, as Compute Engine lists them.
func zonesOf(region string) []string { return compute.Zones(region) }

// zone is the zone of a server, by index: node 0's is
// locationPreference.zone (else the region's first), node 1's its
// secondaryZone (else the region's next zone after node 0's).
func (s Spec) zone(node int) string {
	zs := zonesOf(s.region())
	primary, secondary := s.preferredZones()
	if primary == "" {
		primary = zs[0]
	}
	if node == 0 {
		return primary
	}
	if secondary != "" {
		return secondary
	}
	for _, z := range zs {
		if z != primary {
			return z
		}
	}
	return primary
}

// preferredZones are settings.locationPreference's zone and secondaryZone.
func (s Spec) preferredZones() (zone, secondary string) {
	lp, _ := s.Settings["locationPreference"].(map[string]any)
	zone, _ = lp["zone"].(string)
	secondary, _ = lp["secondaryZone"].(string)
	return strings.TrimSpace(zone), strings.TrimSpace(secondary)
}

// checkZones refuses a zone outside the instance's region, and a
// secondary zone that is the primary's. The wording is UNVERIFIED.
func checkZones(spec Spec) error {
	zone, secondary := spec.preferredZones()
	in := func(z string) bool {
		for _, x := range zonesOf(spec.region()) {
			if x == z {
				return true
			}
		}
		return false
	}
	if zone != "" && !in(zone) {
		return Errorf(http.StatusBadRequest, "Invalid request: Invalid value for settings.locationPreference.zone: %s is not a zone of the instance's region, %s.", zone, spec.region())
	}
	if secondary != "" && !in(secondary) {
		return Errorf(http.StatusBadRequest, "Invalid request: Invalid value for settings.locationPreference.secondaryZone: %s is not a zone of the instance's region, %s.", secondary, spec.region())
	}
	if secondary != "" && secondary == zone {
		return Errorf(http.StatusBadRequest, "Invalid request: settings.locationPreference.secondaryZone must be a different zone from zone, %s.", zone)
	}
	return nil
}

// ipConfig is settings.ipConfiguration.
func (s Spec) ipConfig() map[string]any {
	ic, _ := s.Settings["ipConfiguration"].(map[string]any)
	return ic
}

// privateNetwork is the instance's private IP network, relative
// (projects/P/global/networks/N), or "".
func (s Spec) privateNetwork() string {
	pn, _ := s.ipConfig()["privateNetwork"].(string)
	pn = strings.TrimSpace(pn)
	if pn == "" {
		return ""
	}
	if rel, ok := compute.NetworkName(pn, s.Project); ok {
		return rel
	}
	return pn
}

// publicIP is whether ipConfiguration.ipv4Enabled is not false.
func (s Spec) publicIP() bool {
	v, ok := s.ipConfig()["ipv4Enabled"].(bool)
	return !ok || v
}

// pscEnabled is ipConfiguration.pscConfig.pscEnabled.
func (s Spec) pscEnabled() bool {
	pc, _ := s.ipConfig()["pscConfig"].(map[string]any)
	v, _ := pc["pscEnabled"].(bool)
	return v
}

// fallbackPrivateRange is where a private IP is taken from when Compute
// Engine is not served, so no allocated range can be read.
var fallbackPrivateRange = &net.IPNet{IP: net.IPv4(10, 112, 0, 0).To4(), Mask: net.CIDRMask(16, 32)}

// assignNetworkingLocked checks next's zones and connectivity and gives it
// its private IP: kept while its network and range keep it, else the first
// free address (from .3, as Cloud SQL's first instance in a range has) of
// the range. The caller holds s.emu.
func (s *Server) assignNetworkingLocked(next *Spec) error {
	if err := checkZones(*next); err != nil {
		return err
	}
	pn := next.privateNetwork()
	if !next.publicIP() && pn == "" && !next.pscEnabled() {
		// UNVERIFIED wording.
		return Errorf(http.StatusBadRequest, "Invalid request: At least one of Public IP or Private IP or PSC connectivity must be enabled.")
	}
	if pn == "" {
		next.PrivateIP, next.PrivateNetwork = "", ""
		return nil
	}
	ranges := map[string]*net.IPNet{}
	var nets Networks
	if s.networks != nil {
		nets = s.networks()
	}
	named, _ := next.ipConfig()["allocatedIpRange"].(string)
	switch {
	case nets == nil:
		ranges[""] = fallbackPrivateRange
	case named != "":
		if r, ok := nets.AllocatedRange(pn, named); ok {
			ranges[named] = r
		}
	default:
		ranges = nets.AllocatedRanges(pn)
	}
	if len(ranges) == 0 {
		// The network has no private services access connection: Google's
		// failure is NETWORK_NOT_PEERED (configure-private-ip). The
		// wording around it is UNVERIFIED.
		return Errorf(http.StatusBadRequest, "Invalid request: Incorrect Service Networking config for instance: %s:%s:NETWORK_NOT_PEERED. "+
			"Allocate an IP range for private services access on %s (a global address with purpose VPC_PEERING).", next.Project, next.Name, pn)
	}
	names := make([]string, 0, len(ranges))
	for n := range ranges {
		names = append(names, n)
	}
	sort.Strings(names)
	if next.PrivateIP != "" && next.PrivateNetwork == pn {
		if ip := net.ParseIP(next.PrivateIP); ip != nil {
			for _, n := range names {
				if ranges[n].Contains(ip) {
					return nil
				}
			}
		}
	}
	used := map[string]bool{}
	if s.writes != nil {
		for name, c := range s.writes.byName {
			if name != next.Name && c.spec.PrivateNetwork == pn && c.spec.PrivateIP != "" {
				used[c.spec.PrivateIP] = true
			}
		}
	}
	for _, n := range names {
		r := ranges[n]
		base := binary.BigEndian.Uint32(r.IP.To4())
		ones, bits := r.Mask.Size()
		size := uint32(1) << uint(bits-ones)
		for off := uint32(3); off+1 < size; off++ {
			b := make(net.IP, 4)
			binary.BigEndian.PutUint32(b, base+off)
			if !used[b.String()] {
				next.PrivateIP, next.PrivateNetwork = b.String(), pn
				return nil
			}
		}
	}
	return Errorf(http.StatusBadRequest, "Invalid request: Couldn't find free blocks in allocated IP ranges on %s.", pn)
}

// ipAddressesJSON is the instance's ipAddresses: PRIMARY unless
// ipv4Enabled is false, then PRIVATE when it has a private IP.
func ipAddressesJSON(spec Spec, host string) []any {
	out := []any{}
	if spec.publicIP() {
		out = append(out, map[string]any{"type": "PRIMARY", "ipAddress": host})
	}
	if spec.PrivateIP != "" {
		out = append(out, map[string]any{"type": "PRIVATE", "ipAddress": spec.PrivateIP})
	}
	return out
}

// PrivateEndpoint is a created instance's private IP on a VPC network.
type PrivateEndpoint struct {
	// Network is projects/P/global/networks/N.
	Network, IP string
	// Instance and Server name the instance and its server in the
	// cluster; DatabaseVersion says which port it serves.
	Instance, Server, DatabaseVersion string
}

// PrivateEndpoints are every created instance's private IPs, for Compute
// Engine's VPC model, which makes them reachable from the network's VMs.
func (s *Server) PrivateEndpoints() []PrivateEndpoint {
	s.emu.RLock()
	defer s.emu.RUnlock()
	if s.writes == nil {
		return nil
	}
	var out []PrivateEndpoint
	for _, c := range s.writes.byName {
		sp := c.spec
		if sp.PrivateIP == "" || sp.Stopped() {
			continue
		}
		out = append(out, PrivateEndpoint{Network: sp.PrivateNetwork, IP: sp.PrivateIP, Instance: sp.Name,
			Server: sp.ServerName(), DatabaseVersion: sp.DatabaseVersion})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

// pscServiceAttachment is a Private Service Connect instance's service
// attachment, projects/TENANT_PROJECT/regions/REGION/serviceAttachments/NAME
// as the Admin API reference gives its form: the tenant project and the
// name derived from the instance, stable across reads.
func pscServiceAttachment(spec Spec) string {
	h := crc32.ChecksumIEEE([]byte(spec.Project + ":" + spec.Name))
	return fmt.Sprintf("projects/%d/regions/%s/serviceAttachments/a-%08x-psc-service-attachment-%04x",
		100000000000+uint64(h)%900000000000, spec.region(), h, h>>16)
}
