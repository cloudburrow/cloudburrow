package main

// Cloud SQL's zones and private IP in the console (#2141), as Google's
// create page and Connections > Networking have them
// (https://docs.cloud.google.com/sql/docs/postgres/create-instance,
// https://docs.cloud.google.com/sql/docs/postgres/configure-private-ip,
// https://docs.cloud.google.com/sql/docs/postgres/configure-private-service-connect,
// read 2026-11-22): Choose region and zonal availability's Specify zones
// (Primary zone, Secondary zone for Multiple zones); Connections' Private
// IP, with Private Service Access (PSA), its network from Compute Engine's
// VPC networks, Show allocated IP range option and Enable private path, or
// Private Service Connect (PSC); and on an instance, the Networking tab's
// Private IP and network, ending in Save.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	sqladminapi "google.golang.org/api/sqladmin/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// The Private IP connection methods.
const (
	sqlPrivatePSA = "psa"
	sqlPrivatePSC = "psc"
)

// sqlNetworks are project's VPC networks (projects/P/global/networks/N)
// and, by network, the ranges allocated on it for private services access.
func (p cloudSQLInstancesProvider) sqlNetworks(project string) ([]string, map[string][]string) {
	if p.compute == nil || project == "" {
		return nil, nil
	}
	api := p.compute()
	if api == nil {
		return nil, nil
	}
	list, err := api.Call("GET", project, "global/networks", nil, nil)
	if err != nil {
		return nil, nil
	}
	items, _ := list["items"].([]any)
	var nets []string
	ranges := map[string][]string{}
	for _, it := range items {
		o, _ := it.(map[string]any)
		name, _ := o["name"].(string)
		if name == "" {
			continue
		}
		rel := "projects/" + project + "/global/networks/" + name
		nets = append(nets, rel)
		names := []string{""}
		var rs []string
		for n := range api.AllocatedRanges(rel) {
			rs = append(rs, n)
		}
		sort.Strings(rs)
		ranges[rel] = append(names, rs...)
	}
	sort.Strings(nets)
	return nets, ranges
}

// sqlZoneOptions are each region's zones, "" (Any) first.
func sqlZoneOptions() ([]string, map[string][]string, map[string]string) {
	all := []string{""}
	by := map[string][]string{}
	labels := map[string]string{"": "Any"}
	for _, r := range sqlRegions {
		zs := append([]string{""}, compute.Zones(r)...)
		by[r] = zs
		all = append(all, zs[1:]...)
	}
	return all, by, labels
}

// sqlZoneFields are Specify zones' Primary zone and Secondary zone, in
// section, the zones of the region field's region.
func sqlZoneFields(section, region, primary, secondary string, regional bool) []console.Field {
	all, by, labels := sqlZoneOptions()
	f := []console.Field{
		{Name: "zone", Label: "Primary zone", Type: "select", Section: section, Options: all, OptionLabels: labels, Default: primary,
			Help: "The zone the instance serves from (settings.locationPreference.zone); Any lets Cloud SQL choose."},
		{Name: "secondaryZone", Label: "Secondary zone", Type: "select", Section: section, Options: all, OptionLabels: labels, Default: secondary,
			Help: "The standby's zone (settings.locationPreference.secondaryZone), different from the primary's; a failover swaps them."},
	}
	if region == "" {
		f[0].OptionsBy, f[0].OptionsFor = "region", by
		f[1].OptionsBy, f[1].OptionsFor = "region", by
		f[1].VisibleWhen = map[string]string{"availabilityType": "REGIONAL"}
	} else {
		f[0].Options, f[1].Options = by[region], by[region]
		if f[0].Options == nil {
			f[0].Options = append([]string{""}, compute.Zones(region)...)
			f[1].Options = f[0].Options
		}
		if !regional {
			f[1].VisibleWhen = map[string]string{"availabilityType": "REGIONAL"}
		}
	}
	return f
}

// sqlPrivateIPFields are Connections' Private IP fields, the network
// chosen from nets and its allocated range from ranges.
func sqlPrivateIPFields(section string, nets []string, ranges map[string][]string, cur *sqladminapi.IpConfiguration) []console.Field {
	private, method, network, rng, path := "false", sqlPrivatePSA, "", "", "false"
	projects := ""
	if cur != nil {
		if cur.PrivateNetwork != "" {
			private, network, rng = "true", cur.PrivateNetwork, cur.AllocatedIpRange
			if i := strings.Index(network, "projects/"); i > 0 {
				network = network[i:]
			}
		}
		if cur.PscConfig != nil && cur.PscConfig.PscEnabled {
			private, method = "true", sqlPrivatePSC
			projects = strings.Join(cur.PscConfig.AllowedConsumerProjects, ", ")
		}
		if cur.EnablePrivatePathForGoogleCloudServices {
			path = "true"
		}
	}
	if network == "" && len(nets) > 0 {
		network = nets[0]
		for _, n := range nets {
			if strings.HasSuffix(n, "/networks/default") {
				network = n
			}
		}
	}
	netLabels := map[string]string{}
	for _, n := range nets {
		netLabels[n] = n[strings.LastIndex(n, "/")+1:]
	}
	var allRanges []string
	rangeLabels := map[string]string{"": "Use an automatically allocated IP range"}
	seen := map[string]bool{}
	for _, n := range nets {
		for _, r := range ranges[n] {
			if !seen[r] {
				seen[r] = true
				allRanges = append(allRanges, r)
			}
		}
	}
	if len(allRanges) == 0 {
		allRanges = []string{""}
	}
	onPrivate := map[string]string{"privateIp": "true"}
	psa := map[string]string{"privateIp": "true", "privateMethod": sqlPrivatePSA}
	netField := console.Field{Name: "privateNetwork", Label: "Network", Type: "select", Section: section, Options: nets, OptionLabels: netLabels,
		Default: network, VisibleWhen: psa,
		Help: "A VPC network of this project, from Compute Engine. It needs an IP range allocated for private services access " +
			"(a global address with purpose VPC_PEERING); the instance's private IP is taken from it, and the network's VMs reach the instance at it."}
	if len(nets) == 0 {
		netField.Type, netField.Options, netField.OptionLabels = "text", nil, nil
		netField.Help = "projects/PROJECT/global/networks/NETWORK. Compute Engine is not served by this instance, so the network is not checked and the private IP is not reachable from VMs."
	}
	return []console.Field{
		{Name: "privateIp", Label: "Private IP", Type: "checkbox", Section: section, Default: private,
			Help: "Gives the instance an internal address on a VPC network (ipConfiguration.privateNetwork) or a Private Service Connect service attachment."},
		{Name: "privateMethod", Label: "Choose a private connection method", Type: "radio", Section: section, Default: method,
			Options: []string{sqlPrivatePSA, sqlPrivatePSC}, VisibleWhen: onPrivate,
			OptionLabels: map[string]string{sqlPrivatePSA: "Private Service Access (PSA)", sqlPrivatePSC: "Private Service Connect (PSC)"}},
		netField,
		{Name: "allocatedIpRange", Label: "Allocated IP range", Type: "select", Section: section, Panel: "Show allocated IP range option",
			Options: allRanges, OptionLabels: rangeLabels, Default: rng, OptionsBy: "privateNetwork", OptionsFor: ranges, VisibleWhen: psa,
			Help: "A range allocated on the network for private services access; the automatic choice is any of them."},
		{Name: "enablePrivatePath", Label: "Enable private path", Type: "checkbox", Section: section, Default: path, VisibleWhen: psa,
			Help: "Lets Google Cloud services such as BigQuery reach the instance over its private IP (enablePrivatePathForGoogleCloudServices)."},
		{Name: "pscAllowedProjects", Label: "Allowed projects", Type: "text", Section: section, Default: projects,
			VisibleWhen: map[string]string{"privateIp": "true", "privateMethod": sqlPrivatePSC},
			Help:        "The consumer projects allowed to connect to the instance's service attachment, separated by commas (pscConfig.allowedConsumerProjects)."},
		sqlAutoEndpointsField(section, nets, netLabels, cur),
	}
}

// sqlAutoEndpointsField is Endpoint setup's Add an Endpoint (#2141): each
// New Endpoint card a Project and a Network, made through the network's
// google-cloud-sql connection policy (pscConfig.pscAutoConnections).
func sqlAutoEndpointsField(section string, nets []string, netLabels map[string]string, cur *sqladminapi.IpConfiguration) console.Field {
	netField := console.Field{Name: "network", Label: "Network", Type: "select", Required: true, Options: nets, OptionLabels: netLabels}
	if len(nets) == 0 {
		netField.Type, netField.Options, netField.OptionLabels = "text", nil, nil
	}
	f := console.Field{Name: "pscAutoConnections", Label: "Endpoint setup", Type: console.GroupFieldType, Section: section, AddLabel: "Add an Endpoint",
		VisibleWhen: map[string]string{"privateIp": "true", "privateMethod": sqlPrivatePSC},
		Group: []console.Field{
			{Name: "project", Label: "Project", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9\-]{4,28}[a-z0-9]$`},
			netField,
		},
		Help: "Endpoints Cloud SQL creates for you in a network, through the network's connection policy for the google-cloud-sql service class " +
			"(Network services, Private Service Connect, Connection Policies). Without a policy the endpoint is not created until one is."}
	if cur != nil && cur.PscConfig != nil && len(cur.PscConfig.PscAutoConnections) > 0 {
		var rows []map[string]string
		for _, a := range cur.PscConfig.PscAutoConnections {
			project := a.ConsumerProject
			if project == "" {
				if p := strings.Split(a.ConsumerNetwork, "/"); len(p) > 1 {
					project = p[1]
				}
			}
			rows = append(rows, map[string]string{"project": project, "network": a.ConsumerNetwork})
		}
		if b, err := json.Marshal(rows); err == nil {
			f.Default = string(b)
		}
	}
	return f
}

// sqlAutoEndpointRows is the form's endpoints as pscAutoConnections.
func sqlAutoEndpointRows(v string) ([]*sqladminapi.PscAutoConnectionConfig, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	rows, err := console.ParseGroup(v)
	if err != nil {
		return nil, err
	}
	var out []*sqladminapi.PscAutoConnectionConfig
	for i, r := range rows {
		n := strings.TrimSpace(r["network"])
		project := strings.TrimSpace(r["project"])
		if n == "" {
			return nil, fmt.Errorf("endpoint %d: choose a network", i+1)
		}
		if !strings.HasPrefix(n, "projects/") {
			n = "projects/" + project + "/global/networks/" + n
		}
		out = append(out, &sqladminapi.PscAutoConnectionConfig{ConsumerNetwork: n, ConsumerProject: project})
	}
	return out, nil
}

// sqlPrivateIPConfig applies the Private IP fields to ic.
func sqlPrivateIPConfig(ic *sqladminapi.IpConfiguration, v map[string]string) error {
	if _, ok := v["privateIp"]; !ok {
		return nil
	}
	if v["privateIp"] != "true" {
		ic.PrivateNetwork, ic.AllocatedIpRange, ic.PscConfig = "", "", nil
		ic.NullFields = append(ic.NullFields, "PrivateNetwork", "AllocatedIpRange", "PscConfig")
		return nil
	}
	if v["privateMethod"] == sqlPrivatePSC {
		pc := &sqladminapi.PscConfig{PscEnabled: true}
		for _, p := range strings.Split(v["pscAllowedProjects"], ",") {
			if p = strings.TrimSpace(p); p != "" {
				pc.AllowedConsumerProjects = append(pc.AllowedConsumerProjects, p)
			}
		}
		auto, err := sqlAutoEndpointRows(v["pscAutoConnections"])
		if err != nil {
			return err
		}
		pc.PscAutoConnections = auto
		ic.PscConfig = pc
		return nil
	}
	n := strings.TrimSpace(v["privateNetwork"])
	if n == "" {
		return fmt.Errorf("choose the VPC network of the private IP")
	}
	ic.PrivateNetwork = n
	ic.AllocatedIpRange = strings.TrimSpace(v["allocatedIpRange"])
	ic.EnablePrivatePathForGoogleCloudServices = v["enablePrivatePath"] == "true"
	ic.ForceSendFields = append(ic.ForceSendFields, "EnablePrivatePathForGoogleCloudServices")
	return nil
}

// sqlZonesConfig applies Specify zones to st.
func sqlZonesConfig(st *sqladminapi.Settings, v map[string]string) {
	z, sz := strings.TrimSpace(v["zone"]), strings.TrimSpace(v["secondaryZone"])
	if _, ok := v["zone"]; !ok {
		return
	}
	if v["availabilityType"] != "REGIONAL" {
		sz = ""
	}
	st.LocationPreference = &sqladminapi.LocationPreference{Zone: z, SecondaryZone: sz, ForceSendFields: []string{"Zone", "SecondaryZone"}}
}

// CreateFormFor implements console.ProjectCreateFormer: the create page
// with the project's VPC networks.
func (p cloudSQLInstancesProvider) CreateFormFor(_ context.Context, project string) []console.Field {
	return p.sqlCreateFormIn(project)
}

// networkingAction is the Networking tab's Save: Public IP, Private IP and
// its network, as Google's Connections > Networking edits them.
func (p cloudSQLInstancesProvider) networkingAction(project string, in *sqladminapi.DatabaseInstance) console.Action {
	nets, ranges := p.sqlNetworks(project)
	var ic *sqladminapi.IpConfiguration
	if in.Settings != nil {
		ic = in.Settings.IpConfiguration
	}
	public := "true"
	if ic != nil && !ic.Ipv4Enabled {
		public = "false"
	}
	fields := append([]console.Field{{Name: "publicIp", Label: "Public IP", Type: "checkbox", Default: public,
		Help: "The instance's public address (ipv4Enabled)."}}, sqlPrivateIPFields("", nets, ranges, ic)...)
	switch {
	case sqlPoolable(in) && strings.HasPrefix(in.DatabaseVersion, "MYSQL_"):
		fields = append(fields, sqlMySQLPoolFields("", nil, in.Settings.ConnectionPoolConfig)...)
	case sqlPoolable(in):
		fields = append(fields, sqlPoolFields("", nil, in.Settings.ConnectionPoolConfig)...)
	}
	return console.Action{ID: "edit-networking", Label: "Edit", Title: "Networking", Submit: "Save", Fields: fields,
		Confirm: "Changing private IP or managed connection pooling restarts the instance."}
}

// editNetworking saves the Networking tab.
func editNetworking(st *sqladminapi.Settings, v map[string]string) error {
	ic := &sqladminapi.IpConfiguration{Ipv4Enabled: v["publicIp"] == "true", ForceSendFields: []string{"Ipv4Enabled"}}
	if err := sqlPrivateIPConfig(ic, v); err != nil {
		return err
	}
	st.IpConfiguration = ic
	st.ConnectionPoolConfig = sqlPoolConfig(v)
	return nil
}
