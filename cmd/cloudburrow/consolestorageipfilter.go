package main

// A bucket's IP filtering in the console (#1505), as Google's console has it
// on a bucket's Configuration tab: the IP filtering group shows the mode, the
// public network ranges, the VPC network sources and the two allowances, and
// Edit IP filtering sets them, or removes the filter, through buckets.patch
// on Google's generated JSON API client.
//
// An enabled filter is enforced (internal/service/storage/ipfilter.go): an
// object request from an address in none of the public ranges is refused
// 403, the console's own included, so the Objects tab then says so. The
// bucket's metadata stays reachable, as it is to a caller holding
// storage.buckets.exemptFromIpFilter, so the filter can always be read,
// changed or removed here.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	storagev1 "google.golang.org/api/storage/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const ipFilterNone = "Not configured"

// ipFilterGroup is the Configuration tab's IP filtering group.
func ipFilterGroup(f *storagev1.BucketIpFilter) console.PropertyGroup {
	g := console.PropertyGroup{Heading: "IP filtering"}
	if f == nil {
		g.Properties = []console.Property{{Label: "IP filtering", Value: ipFilterNone}}
		return g
	}
	yes := func(v bool) string {
		if v {
			return "Yes"
		}
		return "No"
	}
	public := "None"
	if f.PublicNetworkSource != nil && len(f.PublicNetworkSource.AllowedIpCidrRanges) > 0 {
		public = strings.Join(f.PublicNetworkSource.AllowedIpCidrRanges, ", ")
	}
	vpc := "None"
	if len(f.VpcNetworkSources) > 0 {
		vpc = strings.ReplaceAll(vpcSourcesText(f.VpcNetworkSources), "\n", "; ")
	}
	g.Properties = []console.Property{
		{Label: "IP filtering", Value: f.Mode},
		{Label: "Public network ranges", Value: public},
		{Label: "VPC network sources", Value: vpc},
		{Label: "Allow all service agents", Value: yes(f.AllowAllServiceAgentAccess)},
		{Label: "Allow cross-org VPCs", Value: yes(f.AllowCrossOrgVpcs)},
	}
	return g
}

// vpcSourcesText is one VPC network source per line: its network, then its
// ranges, comma-separated.
func vpcSourcesText(srcs []*storagev1.BucketIpFilterVpcNetworkSources) string {
	lines := make([]string, 0, len(srcs))
	for _, s := range srcs {
		lines = append(lines, strings.TrimSpace(s.Network+" "+strings.Join(s.AllowedIpCidrRanges, ",")))
	}
	return strings.Join(lines, "\n")
}

// ipFilterAction is Edit IP filtering, prefilled from the bucket.
func ipFilterAction(b bucketMeta) console.Action {
	f := b.IPFilter
	mode, public, vpc, agents, crossOrg := ipFilterNone, "", "", "false", "false"
	if f != nil {
		mode, vpc = f.Mode, vpcSourcesText(f.VpcNetworkSources)
		if f.PublicNetworkSource != nil {
			public = strings.Join(f.PublicNetworkSource.AllowedIpCidrRanges, "\n")
		}
		agents, crossOrg = fmt.Sprint(f.AllowAllServiceAgentAccess), fmt.Sprint(f.AllowCrossOrgVpcs)
	}
	return console.Action{ID: "editipfilter", Label: "Edit IP filtering", Fields: []console.Field{
		{Name: "mode", Label: "IP filtering", Type: "select", Default: mode, Options: []string{ipFilterNone, "Enabled", "Disabled"},
			Help: "Enabled refuses every object request from an address in none of the public ranges, this console's own included; " +
				"the bucket's settings stay reachable, so the filter can be changed here. Disabled keeps the rules without enforcing them; " +
				ipFilterNone + " removes the filter."},
		{Name: "publicRanges", Label: "Public network ranges", Type: "textarea", Default: public,
			Help: "One IPv4 or IPv6 CIDR range per line, such as 203.0.113.0/24 or 127.0.0.1/32; at most 200 ranges in all."},
		{Name: "vpcSources", Label: "VPC network sources", Type: "textarea", Default: vpc,
			Help: "One per line: projects/PROJECT/global/networks/NETWORK, then its ranges, comma-separated; at most 25. " +
				"Stored as sent: no request reaches CloudBurrow from a VPC network, so none is admitted by one."},
		{Name: "allowAllServiceAgentAccess", Label: "Allow all service agents", Type: "checkbox", Default: agents},
		{Name: "allowCrossOrgVpcs", Label: "Allow cross-org VPCs", Type: "checkbox", Default: crossOrg},
	}}
}

// parseIPFilter reads Edit IP filtering's values; nil is no filter.
func parseIPFilter(values map[string]string) (*storagev1.BucketIpFilter, error) {
	mode := strings.TrimSpace(values["mode"])
	switch mode {
	case ipFilterNone, "":
		return nil, nil
	case "Enabled", "Disabled":
	default:
		return nil, fmt.Errorf("IP filtering %q is not Enabled, Disabled or %s", mode, ipFilterNone)
	}
	f := &storagev1.BucketIpFilter{Mode: mode,
		AllowAllServiceAgentAccess: values["allowAllServiceAgentAccess"] == "true",
		AllowCrossOrgVpcs:          values["allowCrossOrgVpcs"] == "true",
		PublicNetworkSource:        &storagev1.BucketIpFilterPublicNetworkSource{AllowedIpCidrRanges: []string{}, ForceSendFields: []string{"AllowedIpCidrRanges"}},
		VpcNetworkSources:          []*storagev1.BucketIpFilterVpcNetworkSources{},
		ForceSendFields:            []string{"AllowAllServiceAgentAccess", "AllowCrossOrgVpcs", "VpcNetworkSources"},
	}
	for _, r := range strings.FieldsFunc(values["publicRanges"], func(c rune) bool { return c == '\n' || c == ',' || c == ' ' || c == '\r' }) {
		f.PublicNetworkSource.AllowedIpCidrRanges = append(f.PublicNetworkSource.AllowedIpCidrRanges, r)
	}
	for _, line := range strings.Split(values["vpcSources"], "\n") {
		parts := strings.Fields(strings.ReplaceAll(line, ",", " "))
		if len(parts) == 0 {
			continue
		}
		src := &storagev1.BucketIpFilterVpcNetworkSources{Network: parts[0], AllowedIpCidrRanges: parts[1:]}
		if len(src.AllowedIpCidrRanges) == 0 {
			return nil, fmt.Errorf("the VPC network %s has no range: name its ranges after it", parts[0])
		}
		f.VpcNetworkSources = append(f.VpcNetworkSources, src)
	}
	return f, nil
}

// editIPFilter is buckets.patch of the bucket's ipFilter, or of null to
// remove it.
func (p storageProvider) editIPFilter(ctx context.Context, path []string, values map[string]string) error {
	if len(path) != 1 || strings.HasPrefix(path[0], "_") {
		return errors.New("IP filtering is set on its bucket's page")
	}
	f, err := parseIPFilter(values)
	if err != nil {
		return err
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return err
	}
	patch := &storagev1.Bucket{IpFilter: f}
	if f == nil {
		patch.NullFields = []string{"IpFilter"}
	}
	_, err = s.Buckets.Patch(path[0], patch).Context(ctx).Do()
	return err
}
