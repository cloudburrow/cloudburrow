package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/vpcaccess/apiv1/vpcaccesspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/vpcaccess"
)

// Serverless VPC Access (#2140): the VPC network menu's Serverless VPC
// access page (/networking/connectors/list), its connectors and Create
// connector, as vpc/docs/configure-serverless-vpc-access (read 2026-11-22)
// draws them: Name, Region, Network, Subnet (an existing /28 subnet or a
// Custom IP range), and Show scaling settings' Minimum instances, Maximum
// instances and Instance type. The list's columns beyond the name are not
// in the docs (UNVERIFIED, re-read 2026-11-22: the same page).

type vpcAccessProvider struct{ computeScreen }

func (vpcAccessProvider) ID() string    { return "vpc-connectors" }
func (vpcAccessProvider) Title() string { return "Serverless VPC access" }

const vpcAccessNote = "A connector is READY once created, and Cloud Run services and jobs name it in their VPC connector, which is checked (it must exist, be READY and be in the service's region) and recorded; " +
	"no connector VMs run, so no traffic is carried through it (docs/gaps.md)."

func (p vpcAccessProvider) va() *vpcaccess.Server {
	if p.svc == nil {
		return nil
	}
	return p.svc.VPCAccess()
}

func vpcAccessColumns() []string {
	return []string{"Region", "Network", "Subnet", "IP address range", "Min instances", "Max instances", "Instance type", "Connected projects"}
}

// connectors are the project's connectors in every region.
func (p vpcAccessProvider) connectors(ctx context.Context, project string) ([]*vpcaccesspb.Connector, error) {
	var out []*vpcaccesspb.Connector
	for _, r := range compute.Regions() {
		l, err := p.va().ListConnectors(ctx, &vpcaccesspb.ListConnectorsRequest{Parent: "projects/" + project + "/locations/" + r, PageSize: 1000})
		if err != nil {
			return nil, err
		}
		out = append(out, l.GetConnectors()...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out, nil
}

func connectorRegion(c *vpcaccesspb.Connector) string { return strings.Split(c.GetName(), "/")[3] }

func connectorStatus(c *vpcaccesspb.Connector) string {
	switch c.GetState() {
	case vpcaccesspb.Connector_READY:
		return "Ready"
	case vpcaccesspb.Connector_ERROR:
		return "Error"
	}
	st := strings.ToLower(c.GetState().String())
	return strings.ToUpper(st[:1]) + st[1:]
}

func connectorRow(c *vpcaccesspb.Connector) console.Resource {
	target := []string{connectorRegion(c), vpcLast(c.GetName())}
	sub := vpcDash(c.GetSubnet().GetName())
	return console.Resource{Name: vpcLast(c.GetName()), Status: connectorStatus(c), Opens: target, Target: target,
		Fields: map[string]string{"Region": connectorRegion(c), "Network": c.GetNetwork(), "Subnet": sub,
			"IP address range": vpcDash(c.GetIpCidrRange()), "Min instances": strconv.Itoa(int(c.GetMinInstances())),
			"Max instances": strconv.Itoa(int(c.GetMaxInstances())), "Instance type": c.GetMachineType(),
			"Connected projects": vpcDash(strings.Join(c.GetConnectedProjects(), ", "))}}
}

func (p vpcAccessProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: vpcAccessColumns(), Noun: "connectors", AlwaysStatus: true, Note: vpcAccessNote}
	if p.va() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Connectors are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	cs, err := p.connectors(ctx, project)
	if err != nil {
		return listing, err
	}
	for _, c := range cs {
		r := connectorRow(c)
		r.Actions = []console.Action{connectorDelete(r.Target)}
		listing.Items = append(listing.Items, r)
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func connectorDelete(path []string) console.Action {
	return console.Action{ID: "delete", Label: "Delete", Destructive: true, Confirm: "Delete connector " + path[1] + "?"}
}

func (vpcAccessProvider) CreateOnPage() bool  { return true }
func (vpcAccessProvider) CreateTitle() string { return "Create connector" }

func (vpcAccessProvider) CreateForm() (string, []console.Field) {
	return "Create connector", connectorFields(nil, nil)
}

func (p vpcAccessProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	if p.api() == nil || project == "" {
		return connectorFields(nil, nil)
	}
	return connectorFields(p.networkNames(ctx, project), p.slash28Subnets(project))
}

// slash28Subnets are the project's /28 subnets, as region/name, which the
// Subnet list offers beside Custom IP range.
func (p vpcAccessProvider) slash28Subnets(project string) []string {
	m, err := p.call("GET", project, "aggregated/subnetworks", nil, nil)
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range aggregatedItems(m, "subnetworks") {
		if strings.HasSuffix(vpcStr(s, "ipCidrRange"), "/28") {
			out = append(out, vpcLast(vpcStr(s, "region"))+"/"+vpcStr(s, "name"))
		}
	}
	sort.Strings(out)
	return out
}

// connectorFields are Google's Create connector's.
func connectorFields(networks, subnets []string) []console.Field {
	regions := compute.Regions()
	netField := console.Field{Name: "network", Label: "Network", Type: "select", Required: true, Options: networks}
	if networks == nil {
		netField.Type = "text"
	}
	subOpts := append([]string{"custom"}, subnets...)
	subLabels := map[string]string{"custom": "Custom IP range"}
	for _, s := range subnets {
		subLabels[s] = s[strings.Index(s, "/")+1:] + " (" + s[:strings.Index(s, "/")] + ")"
	}
	instances := func(lo, hi int) []string {
		var out []string
		for i := lo; i <= hi; i++ {
			out = append(out, strconv.Itoa(i))
		}
		return out
	}
	return []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9\-]{0,23}[a-z0-9]$`,
			Help: "Lowercase letters, numbers and hyphens, starting with a letter; at most 25 characters."},
		{Name: "region", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: regions,
			Help: "The region of the serverless services that use the connector."},
		netField,
		{Name: "subnet", Label: "Subnet", Type: "select", Required: true, Default: "custom", Options: subOpts, OptionLabels: subLabels,
			Help: "An existing /28 subnet that nothing else uses, or a custom IP range."},
		{Name: "ipCidrRange", Label: "IP range", Type: "text", Required: true, VisibleWhen: map[string]string{"subnet": "custom"},
			Pattern: `^[0-9]{1,3}(\.[0-9]{1,3}){3}/28$`, Help: "An unused /28 range of the network, such as 10.8.0.0/28."},
		{Name: "minInstances", Label: "Minimum instances", Type: "select", Default: "2", Options: instances(2, 9), Section: "Scaling settings"},
		{Name: "maxInstances", Label: "Maximum instances", Type: "select", Default: "10", Options: instances(3, 10), Section: "Scaling settings"},
		{Name: "machineType", Label: "Instance type", Type: "select", Default: "e2-micro", Options: vpcaccess.MachineTypes, Section: "Scaling settings"},
	}
}

func (p vpcAccessProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if p.va() == nil {
		return "", errors.New(computeNotStarted)
	}
	name, region := strings.TrimSpace(values["name"]), strings.TrimSpace(values["region"])
	c := &vpcaccesspb.Connector{Network: strings.TrimSpace(values["network"]), MachineType: values["machineType"]}
	if sub := values["subnet"]; sub != "" && sub != "custom" {
		if i := strings.Index(sub, "/"); i >= 0 {
			if sub[:i] != region {
				return "", fmt.Errorf("Subnet: %s is in region %s, not %s", sub[i+1:], sub[:i], region)
			}
			sub = sub[i+1:]
		}
		c.Subnet, c.Network = &vpcaccesspb.Connector_Subnet{Name: sub, ProjectId: project}, ""
	} else {
		c.IpCidrRange = strings.TrimSpace(values["ipCidrRange"])
	}
	for k, dst := range map[string]*int32{"minInstances": &c.MinInstances, "maxInstances": &c.MaxInstances} {
		if v := strings.TrimSpace(values[k]); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return "", fmt.Errorf("%s: %q is not a number", k, v)
			}
			*dst = int32(n)
		}
	}
	if _, err := p.va().CreateConnector(ctx, &vpcaccesspb.CreateConnectorRequest{Parent: "projects/" + project + "/locations/" + region,
		ConnectorId: name, Connector: c}); err != nil {
		return "", err
	}
	return name, nil
}

func (p vpcAccessProvider) connector(ctx context.Context, project string, path []string) (*vpcaccesspb.Connector, error) {
	if len(path) != 2 {
		return nil, fmt.Errorf("a connector is addressed by its region and name, not %v", path)
	}
	return p.va().GetConnector(ctx, &vpcaccesspb.GetConnectorRequest{Name: "projects/" + project + "/locations/" + path[0] + "/connectors/" + path[1]})
}

func (p vpcAccessProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.va() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	c, err := p.connector(ctx, project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	row := connectorRow(c)
	props := []console.Property{{Label: "Status", Value: row.Status}}
	for _, col := range vpcAccessColumns() {
		props = append(props, console.Property{Label: col, Value: row.Fields[col]})
	}
	props = append(props,
		console.Property{Label: "Minimum throughput", Value: strconv.Itoa(int(c.GetMinThroughput())) + " Mbps"},
		console.Property{Label: "Maximum throughput", Value: strconv.Itoa(int(c.GetMaxThroughput())) + " Mbps"})
	return console.Detail{Title: path[1], Heading: "Connector details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Note: vpcAccessNote,
			Groups: []console.PropertyGroup{{Properties: props}}}}}, nil
}

func (p vpcAccessProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.va() == nil || len(path) != 2 {
		return nil
	}
	if _, err := p.connector(ctx, project, path); err != nil {
		return nil
	}
	a := connectorDelete(path)
	a.Leaves = true
	return []console.Action{a}
}

func (p vpcAccessProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if p.va() == nil {
		return errors.New(computeNotStarted)
	}
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.va().DeleteConnector(ctx, &vpcaccesspb.DeleteConnectorRequest{Name: "projects/" + project + "/locations/" + path[0] + "/connectors/" + path[1]})
	return err
}
