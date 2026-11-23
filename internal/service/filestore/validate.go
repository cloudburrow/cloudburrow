package filestore

import (
	"crypto/sha256"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"

	"cloud.google.com/go/filestore/apiv1/filestorepb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
)

const (
	gib = int64(1)
	tib = 1024 * gib
	// MaxSnapshots is the snapshots an instance keeps at a time.
	MaxSnapshots = 240
	// maxExportOptions and maxIPRanges are a share's export option limits.
	maxExportOptions = 10
	maxIPRanges      = 64
)

// band is a range of capacities a tier takes, in GiB, and its step.
type band struct{ min, max, step int64 }

// tierInfo is what Google documents of a tier.
type tierInfo struct {
	bands []band
	// basic tiers: scaled up only, NFSv3, no snapshots, 16-character
	// share names, /29 ranges, restore in place.
	basic bool
	// regional tiers live in a region; the others in a zone.
	regional bool
}

// tiers are the tiers' capacity ranges (service-tiers, read 2026-11-23).
// 63.9 TiB is 65434 GiB, as Google's console caps it.
var tiers = map[filestorepb.Instance_Tier]tierInfo{
	filestorepb.Instance_BASIC_HDD:      {bands: []band{{1 * tib, 65434, 1}}, basic: true},
	filestorepb.Instance_STANDARD:       {bands: []band{{1 * tib, 65434, 1}}, basic: true},
	filestorepb.Instance_BASIC_SSD:      {bands: []band{{2560, 65434, 1}}, basic: true},
	filestorepb.Instance_PREMIUM:        {bands: []band{{2560, 65434, 1}}, basic: true},
	filestorepb.Instance_ZONAL:          {bands: []band{{1 * tib, 9984, 256}, {10 * tib, 100 * tib, 2560}}},
	filestorepb.Instance_HIGH_SCALE_SSD: {bands: []band{{10 * tib, 100 * tib, 2560}}},
	filestorepb.Instance_REGIONAL:       {bands: []band{{100, 10239, 1}, {10 * tib, 100 * tib, 2560}}, regional: true},
	filestorepb.Instance_ENTERPRISE:     {bands: []band{{1 * tib, 10 * tib, 256}}, regional: true},
}

var (
	idRE        = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	shareRE     = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_]*[A-Za-z0-9])?$`)
	parentRE    = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
	instanceRE  = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/instances/([^/]+)$`)
	snapshotRE  = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/instances/([^/]+)/snapshots/([^/]+)$`)
	backupRE    = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/backups/([^/]+)$`)
	labelKeyRE  = regexp.MustCompile(`^\p{Ll}[\p{Ll}\p{Lo}\p{N}_-]{0,62}$`)
	labelValRE  = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{0,63}$`)
	networkRE   = regexp.MustCompile(`^(projects/[^/]+/global/networks/)?[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	rangeNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// isZone reports whether loc is a zone of a served region; isRegion
// whether it is a region.
func isZone(loc string) bool {
	i := strings.LastIndex(loc, "-")
	return i > 0 && isRegion(loc[:i]) && slices.Contains(locations.Zones(loc[:i]), loc)
}

func isRegion(loc string) bool { return slices.Contains(locations.Regions, loc) }

// regionOf is a location's region.
func regionOf(loc string) string {
	if isZone(loc) {
		return loc[:strings.LastIndex(loc, "-")]
	}
	return loc
}

func validLocation(loc string) bool { return isZone(loc) || isRegion(loc) }

func checkLabels(labels map[string]string) error {
	if len(labels) > 64 {
		return apierror.InvalidArgument("A resource can have at most 64 labels")
	}
	for k, v := range labels {
		if !labelKeyRE.MatchString(k) {
			return apierror.InvalidArgument("Label key %q is invalid: keys are 1 to 63 lowercase letters, digits, underscores and dashes, starting with a lowercase letter", k)
		}
		if !labelValRE.MatchString(v) {
			return apierror.InvalidArgument("Label value %q of key %q is invalid: values are at most 63 lowercase letters, digits, underscores and dashes", v, k)
		}
	}
	return nil
}

func tierName(t filestorepb.Instance_Tier) string { return t.String() }

// checkCapacity checks capacity (GiB) is in one of the tier's ranges and
// on its step. UNVERIFIED wording.
func checkCapacity(t filestorepb.Instance_Tier, capacity int64) error {
	info := tiers[t]
	for _, b := range info.bands {
		if capacity >= b.min && capacity <= b.max {
			if (capacity-b.min)%b.step != 0 {
				return apierror.InvalidArgument("capacity_gb %d is invalid for tier %s: capacity between %d GiB and %d GiB is set in steps of %d GiB", capacity, tierName(t), b.min, b.max, b.step)
			}
			return nil
		}
	}
	var r []string
	for _, b := range info.bands {
		r = append(r, fmt.Sprintf("%d GiB to %d GiB", b.min, b.max))
	}
	return apierror.InvalidArgument("capacity_gb %d is out of range for tier %s: the tier takes %s", capacity, tierName(t), strings.Join(r, ", or "))
}

// bandOf is the index of the range capacity is in.
func bandOf(t filestorepb.Instance_Tier, capacity int64) int {
	for i, b := range tiers[t].bands {
		if capacity >= b.min && capacity <= b.max {
			return i
		}
	}
	return -1
}

// checkShareName checks a file share's name for the tier.
func checkShareName(t filestorepb.Instance_Tier, name string) error {
	max := 32
	if tiers[t].basic {
		max = 16
	}
	if len(name) < 1 || len(name) > max || !shareRE.MatchString(name) {
		return apierror.InvalidArgument("file share name %q is invalid: it must be 1 to %d characters of letters, numbers and underscores, begin with a letter and end with a letter or number", name, max)
	}
	return nil
}

// checkExports checks and normalizes a share's NFS export options.
func checkExports(opts []*filestorepb.NfsExportOptions) error {
	if len(opts) > maxExportOptions {
		return apierror.InvalidArgument("There is a limit of %d export options per file share; %d were given", maxExportOptions, len(opts))
	}
	n := 0
	seen := map[string]bool{}
	for _, o := range opts {
		if len(o.GetIpRanges()) == 0 {
			return apierror.InvalidArgument("nfs_export_options.ip_ranges is required in each export option")
		}
		for _, r := range o.GetIpRanges() {
			n++
			if !validRange(r) {
				return apierror.InvalidArgument("ip_ranges entry %q is not an IPv4 address or an IPv4 CIDR block", r)
			}
			if seen[r] {
				return apierror.InvalidArgument("ip_ranges entry %q appears more than once: overlapping IP ranges are not allowed", r)
			}
			seen[r] = true
		}
		if o.GetSquashMode() != filestorepb.NfsExportOptions_ROOT_SQUASH && (o.GetAnonUid() != 0 || o.GetAnonGid() != 0) {
			return apierror.InvalidArgument("anon_uid and anon_gid may only be set with squash_mode ROOT_SQUASH")
		}
		if o.GetAnonUid() < 0 || o.GetAnonGid() < 0 {
			return apierror.InvalidArgument("anon_uid and anon_gid must not be negative")
		}
	}
	if n > maxIPRanges {
		return apierror.InvalidArgument("The limit is %d IP ranges/addresses for each FileShareConfig among all NfsExportOptions; %d were given", maxIPRanges, n)
	}
	for _, o := range opts {
		if o.GetAccessMode() == filestorepb.NfsExportOptions_ACCESS_MODE_UNSPECIFIED {
			o.AccessMode = filestorepb.NfsExportOptions_READ_WRITE
		}
		if o.GetSquashMode() == filestorepb.NfsExportOptions_SQUASH_MODE_UNSPECIFIED {
			o.SquashMode = filestorepb.NfsExportOptions_NO_ROOT_SQUASH
		}
		if o.GetSquashMode() == filestorepb.NfsExportOptions_ROOT_SQUASH {
			if o.GetAnonUid() == 0 {
				o.AnonUid = 65534
			}
			if o.GetAnonGid() == 0 {
				o.AnonGid = 65534
			}
		}
	}
	return nil
}

func validRange(r string) bool {
	if strings.Contains(r, "/") {
		ip, _, err := net.ParseCIDR(r)
		return err == nil && ip.To4() != nil
	}
	ip := net.ParseIP(r)
	return ip != nil && ip.To4() != nil
}

// checkNetwork checks and fills an instance's network: one, IPv4, its
// connect mode, and a reserved range of the tier's size (chosen when
// unset, in 10.0.0.0/8, the same for the same instance name).
func (s *Server) checkNetwork(in *filestorepb.Instance, project, name string) error {
	if len(in.GetNetworks()) != 1 {
		return apierror.InvalidArgument("networks must have exactly one entry: an instance is connected to one VPC network")
	}
	n := in.Networks[0]
	if n.GetNetwork() == "" {
		return apierror.InvalidArgument("networks[0].network is required")
	}
	if !networkRE.MatchString(n.GetNetwork()) {
		return apierror.InvalidArgument("networks[0].network %q is not a network name", n.GetNetwork())
	}
	if s.opts.Networks != nil {
		if nets := s.opts.Networks(); nets != nil && !nets.HasNetwork(n.GetNetwork(), project) {
			return apierror.InvalidArgument("network %q does not exist", n.GetNetwork())
		}
	}
	if len(n.GetModes()) == 0 {
		n.Modes = []filestorepb.NetworkConfig_AddressMode{filestorepb.NetworkConfig_MODE_IPV4}
	}
	for _, m := range n.GetModes() {
		if m != filestorepb.NetworkConfig_MODE_IPV4 {
			return apierror.InvalidArgument("networks[0].modes: only MODE_IPV4 is supported")
		}
	}
	if n.GetConnectMode() == filestorepb.NetworkConfig_CONNECT_MODE_UNSPECIFIED {
		n.ConnectMode = filestorepb.NetworkConfig_DIRECT_PEERING
	}
	if len(n.GetIpAddresses()) > 0 {
		return apierror.InvalidArgument("networks[0].ip_addresses is output only")
	}
	prefix := 26
	if tiers[in.GetTier()].basic {
		prefix = 29
	}
	if in.GetTier() == filestorepb.Instance_HIGH_SCALE_SSD {
		prefix = 24
	}
	r := n.GetReservedIpRange()
	switch {
	case r == "":
		n.ReservedIpRange = defaultRange(name, prefix)
	case strings.Contains(r, "/"):
		ip, cidr, err := net.ParseCIDR(r)
		if err != nil || ip.To4() == nil {
			return apierror.InvalidArgument("reserved_ip_range %q is not an IPv4 CIDR block", r)
		}
		if ones, _ := cidr.Mask.Size(); ones != prefix {
			return apierror.InvalidArgument("reserved_ip_range %q must be a /%d CIDR block for tier %s", r, prefix, tierName(in.GetTier()))
		}
		if !cidr.IP.Equal(ip) {
			return apierror.InvalidArgument("reserved_ip_range %q is not a network address: use %s", r, cidr.String())
		}
	case n.GetConnectMode() == filestorepb.NetworkConfig_PRIVATE_SERVICE_ACCESS && rangeNameRE.MatchString(r):
		// The name of an allocated range.
	default:
		return apierror.InvalidArgument("reserved_ip_range %q is not a CIDR block (or, with PRIVATE_SERVICE_ACCESS, an allocated range's name)", r)
	}
	return nil
}

// defaultRange is a /prefix block in 10.0.0.0/8 chosen from name.
func defaultRange(name string, prefix int) string {
	h := sha256.Sum256([]byte(name))
	v := uint32(h[0])<<16 | uint32(h[1])<<8 | uint32(h[2])
	size := uint32(1) << (32 - prefix)
	v = (v * size) & 0x00ffffff
	return fmt.Sprintf("10.%d.%d.%d/%d", v>>16&0xff, v>>8&0xff, v&0xff, prefix)
}
