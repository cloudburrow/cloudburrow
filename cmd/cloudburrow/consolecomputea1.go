package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Compute Engine console parity, part A1 (#2139, #2140): a VM's provisioning
// model after create.

// provisioningModelEditField is the VM page Edit's VM provisioning model
// (compute/docs/instances/create-use-spot, "Change the provisioning model
// of a VM", read 2026-11-22: stop the VM, click Edit, and in Management's
// Availability policies choose Standard or Spot): instances.setScheduling,
// which refuses the change while the VM is not stopped, so the other model
// is drawn unavailable until it is.
func provisioningModelEditField(o map[string]any) console.Field {
	cur := provisioningModelText(o)
	f := console.Field{Name: "provisioningModel", Label: "VM provisioning model", Type: "radio", Default: cur,
		Options: []string{"Standard", "Spot"}, Section: "Availability policies",
		Help: "Spot VMs cost less and can be preempted; locally nothing preempts them."}
	if vpcStr(o, "status") != "TERMINATED" {
		other := "Spot"
		if cur == "Spot" {
			other = "Standard"
		}
		f.UnavailableOptions = map[string]string{other: "Stop the VM to change its provisioning model: it is " + vpcStr(o, "status") + "."}
	}
	return f
}

// terminationActionEditField is a Spot VM's On VM termination.
func terminationActionEditField(o map[string]any) console.Field {
	ta := "Stop"
	if vpcStr(vmMap(o, "scheduling"), "instanceTerminationAction") == "DELETE" {
		ta = "Delete"
	}
	return console.Field{Name: "terminationAction", Label: "On VM termination", Type: "select", Default: ta,
		Options: []string{"Stop", "Delete"}, Section: "Availability policies",
		VisibleWhen: map[string]string{"provisioningModel": "Spot"}, Help: "What happens to the VM when it is preempted."}
}

// editProvisioningModel sends setScheduling when the provisioning model or a
// Spot VM's termination action changed.
func (p computeInstancesProvider) editProvisioningModel(project, base string, o map[string]any, values map[string]string) error {
	pm := strings.TrimSpace(values["provisioningModel"])
	if pm == "" {
		return nil
	}
	cur := provisioningModelText(o)
	ta := strings.ToUpper(vpcOr(values["terminationAction"], "Stop"))
	curTA := vpcStr(vmMap(o, "scheduling"), "instanceTerminationAction")
	var body map[string]any
	switch {
	case pm == "Spot" && (cur != "Spot" || ta != curTA):
		body = map[string]any{"provisioningModel": "SPOT", "instanceTerminationAction": ta, "automaticRestart": false, "onHostMaintenance": "TERMINATE"}
	case pm == "Standard" && cur != "Standard":
		body = map[string]any{"provisioningModel": "STANDARD", "automaticRestart": true, "onHostMaintenance": "MIGRATE", "preemptible": false}
	default:
		return nil
	}
	_, err := p.call("POST", project, base+"/setScheduling", nil, body)
	return err
}

// availabilityGroup is the VM details' Management > Availability policies:
// VM provisioning model, On VM termination (Spot), On host maintenance and
// Automatic restart, as setScheduling leaves them.
func availabilityGroup(o map[string]any) console.PropertyGroup {
	sch := vmMap(o, "scheduling")
	props := []console.Property{{Label: "VM provisioning model", Value: provisioningModelText(o)}}
	if ta := vpcStr(sch, "instanceTerminationAction"); ta != "" {
		props = append(props, console.Property{Label: "On VM termination", Value: vpcTitle(ta)})
	}
	ohm := "Migrate VM instance"
	if vpcStr(sch, "onHostMaintenance") == "TERMINATE" {
		ohm = "Terminate VM instance"
	}
	props = append(props, console.Property{Label: "On host maintenance", Value: ohm},
		console.Property{Label: "Automatic restart", Value: map[bool]string{true: "On", false: "Off"}[sch["automaticRestart"] == true]})
	return console.PropertyGroup{Heading: "Availability policies", Properties: props}
}

// attachToField is Reserve external static IP address's Attached to
// (vpc/docs/reserve-static-external-ip-address, read 2026-11-22: "Attached
// to: None, or a VM instance"): the project's VMs, by ZONE/NAME, whose
// external address the new one replaces; an IPv6-only VM has none to
// replace.
func (p computeAddressesProvider) attachToField(project string, visible map[string]string) console.Field {
	opts := []string{"None"}
	if m, err := p.call("GET", project, "aggregated/instances", nil, nil); err == nil {
		for _, o := range aggregatedItems(m, "instances") {
			if nics := vmMaps(o, "networkInterfaces"); len(nics) > 0 && vpcStr(nics[0], "stackType") == "IPV6_ONLY" {
				continue // no IPv4 interface to take an IPv4 address
			}
			opts = append(opts, zoneOf(vpcStr(o, "zone"))+"/"+vpcStr(o, "name"))
		}
	}
	return console.Field{Name: "attachTo", Label: "Attached to", Type: "select", Default: "None", Options: opts, VisibleWhen: visible,
		Help: "A VM of the address's region: its external address (ephemeral or none) is replaced by this one."}
}

// attachExternal assigns the reserved regional address name to the VM at
// ZONE/NAME: deleteAccessConfig of its current one, then addAccessConfig
// with the address as natIP, as Google's console does.
func (p computeAddressesProvider) attachExternal(project, region, name, target string) error {
	target = strings.TrimSpace(target)
	if target == "" || target == "None" {
		return nil
	}
	zone, vm, err := splitZoneName(target, "attached to")
	if err != nil {
		return err
	}
	if regionOfZoneName(zone) != region {
		return fmt.Errorf("attached to: %s is not in the address's region %s", target, region)
	}
	a, err := p.call("GET", project, "regions/"+region+"/addresses/"+name, nil, nil)
	if err != nil {
		return err
	}
	base := "zones/" + zone + "/instances/" + vm
	o, err := p.call("GET", project, base, nil, nil)
	if err != nil {
		return err
	}
	nics := vmMaps(o, "networkInterfaces")
	if len(nics) == 0 {
		return fmt.Errorf("attached to: %s has no network interface", target)
	}
	nic := vpcStr(nics[0], "name")
	for _, ac := range vmMaps(nics[0], "accessConfigs") {
		if _, err := p.call("POST", project, base+"/deleteAccessConfig", url.Values{"networkInterface": {nic}, "accessConfig": {vpcStr(ac, "name")}}, nil); err != nil {
			return err
		}
	}
	_, err = p.call("POST", project, base+"/addAccessConfig", url.Values{"networkInterface": {nic}},
		map[string]any{"name": "External NAT", "type": "ONE_TO_ONE_NAT", "natIP": vpcStr(a, "address"), "networkTier": vpcStr(a, "networkTier")})
	return err
}

// --- Regional disks -----------------------------------------------------------

// diskLoc is where a disk is: its zone, or a regional disk's region; the
// first part of its console path and of its ZONE/NAME (REGION/NAME) name.
func diskLoc(d map[string]any) string {
	if z := zoneOf(vpcStr(d, "zone")); z != "" {
		return z
	}
	return vpcLast(vpcStr(d, "region"))
}

// diskPath is the API path of the disk name at loc, a zone or a region.
func diskPath(loc, name string) string {
	if compute.IsRegion(loc) {
		return "regions/" + loc + "/disks/" + name
	}
	return "zones/" + loc + "/disks/" + name
}

// diskZoneText is the Disks list's Zone: a zonal disk's zone, a regional
// disk's two replica zones, as Google's list writes them.
func diskZoneText(d map[string]any) string {
	if z := zoneOf(vpcStr(d, "zone")); z != "" {
		return z
	}
	var zones []string
	for _, z := range vpcStrs(d, "replicaZones") {
		zones = append(zones, vpcLast(z))
	}
	return strings.Join(zones, ", ")
}

// diskLocationProperty is a disk page's Zone, or a regional disk's Region
// and Replica zones.
func diskLocationProperty(d map[string]any) console.Property {
	if z := zoneOf(vpcStr(d, "zone")); z != "" {
		return console.Property{Label: "Zone", Value: z}
	}
	return console.Property{Label: "Replica zones", Value: vpcLast(vpcStr(d, "region")) + ": " + diskZoneText(d)}
}

// replicaZoneField is Create a disk's Replica zone, for Location Regional
// (compute/docs/disks/regional-persistent-disk, read 2026-11-22: "Select
// Regional ... select the Region and Zone ... Select a Replica zone"): the
// second zone of the region the disk is kept in.
func replicaZoneField() console.Field {
	f := vmZoneField("replicaZone")
	f.Label, f.Section, f.Default = "Replica zone", "Location", "us-central1-b"
	f.VisibleWhen = map[string]string{"location": "Regional"}
	f.Help = "Another zone of the disk's region; the disk can be attached to a VM of either zone."
	return f
}

// freeRegionalDisks are the regional disks, as REGION/NAME, that a VM of
// zone can attach: free, with a replica in zone.
func (c computeScreen) freeRegionalDisks(project, zone string) []string {
	region := regionOfZoneName(zone)
	l, err := c.list(project, "regions/"+region+"/disks")
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range l {
		if len(vpcStrs(d, "users")) > 0 {
			continue
		}
		for _, z := range vpcStrs(d, "replicaZones") {
			if vpcLast(z) == zone {
				out = append(out, region+"/"+vpcStr(d, "name"))
			}
		}
	}
	return out
}

// cloneDisk is Clone disk: a zonal disk's clone in its zone, a regional
// disk's in its region with the same replica zones.
func (p computeDisksProvider) cloneDisk(project string, path []string, b map[string]any) error {
	b["sourceDisk"] = diskPath(path[0], path[1])
	if !compute.IsRegion(path[0]) {
		_, err := p.call("POST", project, "zones/"+path[0]+"/disks", nil, b)
		return err
	}
	src, err := p.call("GET", project, diskPath(path[0], path[1]), nil, nil)
	if err != nil {
		return err
	}
	b["replicaZones"] = []any{}
	for _, z := range vpcStrs(src, "replicaZones") {
		b["replicaZones"] = append(b["replicaZones"].([]any), "zones/"+vpcLast(z))
	}
	b["type"] = "regions/" + path[0] + "/diskTypes/" + vpcLast(vpcStr(src, "type"))
	_, err = p.call("POST", project, "regions/"+path[0]+"/disks", nil, b)
	return err
}

// --- IPv6 ---------------------------------------------------------------------

// stackTypeText is a stack type as Google's pages write it.
func stackTypeText(st string) string {
	switch st {
	case "IPV4_IPV6":
		return "IPv4 and IPv6 (dual-stack)"
	case "IPV6_ONLY":
		return "IPv6 (single-stack)"
	}
	return "IPv4 (single-stack)"
}

// ipv6AccessTypeField is a dual-stack subnet's IPv6 access type
// (vpc/docs/create-modify-vpc-networks, "Add a dual-stack subnet", read
// 2026-11-22): Internal (a /64 of the network's ULA range, which the
// network must have) or External.
func ipv6AccessTypeField(name, stackField string) console.Field {
	return console.Field{Name: name, Label: "IPv6 access type", Type: "radio", Default: "Internal", Options: []string{"Internal", "External"},
		ShowWhen: stackField + "=IPV4_IPV6|IPV6_ONLY",
		Help:     "Internal takes a /64 of the network's ULA internal IPv6 range; External a /64 of 2600:1900::/28, reached only from the network's own VMs here."}
}

// vmStackTypeField is Create an instance's network interface IP stack type
// (compute/docs/ip-addresses/configure-ipv6-address, read 2026-11-22): a
// dual-stack interface needs a dual-stack subnet, and gets an internal or
// external IPv6 address as the subnet's IPv6 access type says; an IPv6-only
// one (IPv6 (single-stack)) a dual-stack or IPv6-only subnet, and no IPv4
// address at all.
func vmStackTypeField(section string) console.Field {
	return console.Field{Name: "stackType", Label: "IP stack type", Type: "radio", Section: section, Default: "IPV4_ONLY",
		Options:      []string{"IPV4_ONLY", "IPV4_IPV6", "IPV6_ONLY"},
		OptionLabels: map[string]string{"IPV4_ONLY": "IPv4 (single-stack)", "IPV4_IPV6": "IPv4 and IPv6 (dual-stack)", "IPV6_ONLY": "IPv6 (single-stack)"},
		Help: "Dual-stack needs a dual-stack subnetwork; IPv6 (single-stack) a dual-stack or IPv6-only one and a Debian or Ubuntu image. " +
			"The VM gets a /96 of its IPv6 range, reachable from the network's other IPv6 VMs."}
}

// nicExternalIPv6 is a network interface's external IPv6 address, or "".
func nicExternalIPv6(n map[string]any) string {
	for _, ac := range vmMaps(n, "ipv6AccessConfigs") {
		if ip := vpcStr(ac, "externalIpv6"); ip != "" {
			return ip
		}
	}
	return ""
}

// subnetIpv6AccessEditField is Edit subnet's IPv6 access type: chosen when
// the subnet becomes dual-stack, fixed once it is (Google's subnetworks.patch
// does not change it, vpc/docs/create-modify-vpc-networks#add-ipv6, read
// 2026-11-22).
func subnetIpv6AccessEditField(s map[string]any) console.Field {
	f := ipv6AccessTypeField("ipv6AccessType", "stackType")
	if at := vpcStr(s, "ipv6AccessType"); at != "" {
		f.Default = strings.ToUpper(at[:1]) + strings.ToLower(at[1:])
		f.Immutable = true
	}
	return f
}

// subnetStackEdit changes a subnet's IP stack type (subnetworks.patch):
// IPv4 to dual-stack with its IPv6 access type, or back while no interface
// uses its IPv6 range, as the API refuses otherwise.
func (p computeNetworksProvider) subnetStackEdit(project, base string, s map[string]any, values map[string]string) error {
	st := values["stackType"]
	cur := vpcOr(vpcStr(s, "stackType"), "IPV4_ONLY")
	if st == "" || st == cur {
		return nil
	}
	if st == "IPV6_ONLY" || cur == "IPV6_ONLY" {
		return fmt.Errorf("IP stack type: a subnet cannot be changed to or from IPv6 (single-stack)")
	}
	patch := map[string]any{"stackType": st, "fingerprint": vpcStr(s, "fingerprint")}
	if st == "IPV4_IPV6" {
		patch["ipv6AccessType"] = strings.ToUpper(vpcOr(values["ipv6AccessType"], "Internal"))
	}
	_, err := p.call("PATCH", project, base, nil, patch)
	return err
}

// withStaticExternal adds the project's reserved regional external
// addresses to Create an instance's External IPv4 address, as Google's
// list offers them (compute/docs/ip-addresses/configure-static-external-ip-address,
// read 2026-11-22): REGION/NAME, labelled with the address.
func (p computeInstancesProvider) withStaticExternal(project string, fields []console.Field) []console.Field {
	m, err := p.call("GET", project, "aggregated/addresses", nil, nil)
	if err != nil {
		return fields
	}
	addrs := aggregatedItems(m, "addresses")
	for i := range fields {
		if fields[i].Name != "externalIP" {
			continue
		}
		f := &fields[i]
		labels := map[string]string{}
		for k, v := range f.OptionLabels {
			labels[k] = v
		}
		for _, a := range addrs {
			if vpcStr(a, "addressType") != "EXTERNAL" || vpcStr(a, "status") != "RESERVED" || vpcStr(a, "region") == "" {
				continue
			}
			opt := vpcLast(vpcStr(a, "region")) + "/" + vpcStr(a, "name")
			f.Options = append(f.Options, opt)
			labels[opt] = vpcStr(a, "name") + " (" + vpcStr(a, "address") + ", " + vpcLast(vpcStr(a, "region")) + ")"
		}
		f.OptionLabels = labels
		f.Help = "Ephemeral gives the instance an external IP of its own from 192.0.2.0/24; a reserved static address of the instance's region keeps its IP. Its port 22 is forwarded to a port on 127.0.0.1."
	}
	return fields
}

// createAccessConfig is the instance's access config for External IPv4
// address: none, ephemeral of the tier chosen, or a reserved address
// (natIP), of the address's own tier.
func (p computeInstancesProvider) createAccessConfig(project string, values map[string]string) (map[string]any, error) {
	ext := values["externalIP"]
	if ext == "None" {
		return nil, nil
	}
	ac := map[string]any{"name": "External NAT", "type": "ONE_TO_ONE_NAT", "networkTier": strings.ToUpper(vpcOr(values["networkTier"], "Premium"))}
	if region, name, ok := strings.Cut(ext, "/"); ok {
		a, err := p.call("GET", project, "regions/"+region+"/addresses/"+name, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("external IPv4 address %s: %w", name, err)
		}
		ac["natIP"], ac["networkTier"] = vpcStr(a, "address"), vpcOr(vpcStr(a, "networkTier"), "PREMIUM")
	}
	return ac, nil
}
