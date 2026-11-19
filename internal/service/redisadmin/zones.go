package redisadmin

import (
	"encoding/json"
	"regexp"
	"sort"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// zone_distribution_config of a Redis cluster and a Memorystore for Valkey
// instance (#1908), as Google documents it (the protos, and
// https://docs.cloud.google.com/memorystore/docs/valkey/create-instances,
// /cluster/create-instances): MULTI_ZONE, the default, distributes the
// nodes "across 3 zones picked at random within the region" (a Valkey
// instance's `zones` names them); SINGLE_ZONE provisions every node in
// `zone`, which "must be specified". The zone, and every zone of `zones`,
// is checked to be a zone of the instance's region; the configuration is
// immutable ("Optional. Immutable." on Valkey; a Redis cluster's update
// of it is refused the same way when it changes it).
//
// There are no zones here, so the distribution is reflected where a
// client can see it: each node's server has Valkey's availability-zone
// set to its zone, which HELLO returns to clients (availability_zone),
// and a Valkey instance's active_zones lists its nodes' zones. MULTI_ZONE
// spreads each shard's nodes over the zones, its primary in the first
// (the shard's number, in turn), so no two nodes of a shard of at most
// three share one; zones are region-a, -b and -c unless `zones` names
// others.

// zoneRE is a zone name: its region and a letter.
var zoneRE = regexp.MustCompile(`^([a-z]+(?:-[a-z]+)*[0-9]+)-([a-z])$`)

// zoneDist is a zone_distribution_config, of either API.
type zoneDist struct {
	Mode  string
	Zone  string
	Zones []string
}

func clusterZoneDist(z *clusterpb.ZoneDistributionConfig) zoneDist {
	return zoneDist{Mode: z.GetMode().String(), Zone: z.GetZone()}
}

func valkeyZoneDist(z *memorystorepb.ZoneDistributionConfig) zoneDist {
	return zoneDist{Mode: z.GetMode().String(), Zone: z.GetZone(), Zones: z.GetZones()}
}

// check refuses what Google refuses of z in region.
func (z zoneDist) check(region string) error {
	inRegion := func(field, zone string) error {
		m := zoneRE.FindStringSubmatch(zone)
		if m == nil || m[1] != region {
			return apierror.InvalidArgument("zone_distribution_config.%s %q is not a zone of the instance's region %s", field, zone, region)
		}
		return nil
	}
	switch z.Mode {
	case "ZONE_DISTRIBUTION_MODE_UNSPECIFIED", "MULTI_ZONE":
		seen := map[string]bool{}
		for _, zone := range z.Zones {
			if err := inRegion("zones", zone); err != nil {
				return err
			}
			if seen[zone] {
				return apierror.InvalidArgument("zone_distribution_config.zones names %s twice", zone)
			}
			seen[zone] = true
		}
	case "SINGLE_ZONE":
		if z.Zone == "" {
			return apierror.InvalidArgument("zone_distribution_config.zone must be specified with SINGLE_ZONE")
		}
		return inRegion("zone", z.Zone)
	default:
		return apierror.InvalidArgument("zone_distribution_config.mode %s is not a zone distribution mode", z.Mode)
	}
	return nil
}

// same is whether z and o are the same configuration, the defaults
// counted as what they mean.
func (z zoneDist) same(o zoneDist) bool {
	norm := func(d zoneDist) zoneDist {
		if d.Mode == "ZONE_DISTRIBUTION_MODE_UNSPECIFIED" || d.Mode == "" {
			d.Mode = "MULTI_ZONE"
		}
		if d.Mode == "MULTI_ZONE" {
			d.Zone = "" // ignored for MULTI_ZONE
		}
		return d
	}
	a, b := norm(z), norm(o)
	if a.Mode != b.Mode || a.Zone != b.Zone || len(a.Zones) != len(b.Zones) {
		return false
	}
	for i := range a.Zones {
		if a.Zones[i] != b.Zones[i] {
			return false
		}
	}
	return true
}

// zoneDist is r's configuration as kept.
func (r *clusterRecord) zoneDist() zoneDist {
	if r.Valkey != nil {
		z := &memorystorepb.ZoneDistributionConfig{}
		if r.Valkey.ZoneDistribution != "" {
			_ = protojson.Unmarshal([]byte(r.Valkey.ZoneDistribution), z)
		}
		return valkeyZoneDist(z)
	}
	z := &clusterpb.ZoneDistributionConfig{}
	if r.ZoneDistribution != "" {
		_ = json.Unmarshal([]byte(r.ZoneDistribution), z)
	}
	return clusterZoneDist(z)
}

// zones are the zones r's nodes are in.
func (r *clusterRecord) zones() []string {
	z := r.zoneDist()
	if z.Mode == "SINGLE_ZONE" && z.Zone != "" {
		return []string{z.Zone}
	}
	if len(z.Zones) > 0 {
		return z.Zones
	}
	m := nodesOwnerRE.FindStringSubmatch(r.Name)
	if m == nil {
		return nil
	}
	return []string{m[2] + "-a", m[2] + "-b", m[2] + "-c"}
}

// nodeZone is the zone of n, one of r's nodes: its shard's nodes in
// turn over the zones, the shard's first node in zone (shard mod zones).
func (r *clusterRecord) nodeZone(n *clusterNode) string {
	zones := r.zones()
	if len(zones) == 0 {
		return ""
	}
	k := 0
	for _, m := range r.Nodes {
		if m == n {
			break
		}
		if m.Shard == n.Shard {
			k++
		}
	}
	return zones[(n.Shard+k)%len(zones)]
}

// activeZones are the zones r's nodes are in, sorted.
func (r *clusterRecord) activeZones() []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range r.Nodes {
		if z := r.nodeZone(n); z != "" && !seen[z] {
			seen[z] = true
			out = append(out, z)
		}
	}
	sort.Strings(out)
	return out
}

// sameClusterZones is whether a Redis cluster's update keeps its
// configuration, kept as JSON.
func sameClusterZones(kept string, in *clusterpb.ZoneDistributionConfig) bool {
	z := &clusterpb.ZoneDistributionConfig{}
	if kept != "" {
		_ = json.Unmarshal([]byte(kept), z)
	}
	if in == nil {
		in = &clusterpb.ZoneDistributionConfig{}
	}
	return clusterZoneDist(z).same(clusterZoneDist(in))
}
