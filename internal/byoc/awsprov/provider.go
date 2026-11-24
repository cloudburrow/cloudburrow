package awsprov

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

// Provider is one connected AWS account.
type Provider struct {
	d     *Driver
	conn  byoc.Connection
	home  string
	creds aws.CredentialsProvider

	mu   sync.Mutex
	sec  secret
	save func([]byte) error
}

var _ byoc.Provider = (*Provider)(nil)

// Cloud is AWS.
func (p *Provider) Cloud() byoc.Cloud { return byoc.AWS }

func (p *Provider) cfg(region string) aws.Config { return p.d.config(region, p.creds) }

func (p *Provider) ec2(region string) *ec2.Client {
	return ec2.NewFromConfig(p.cfg(region), func(o *ec2.Options) { o.BaseEndpoint = p.d.base(ServiceEC2) })
}

func (p *Provider) iam() *iam.Client {
	return iam.NewFromConfig(p.cfg(defaultRegion), func(o *iam.Options) { o.BaseEndpoint = p.d.base(ServiceIAM) })
}

func (p *Provider) pricing() *pricing.Client {
	return pricing.NewFromConfig(p.cfg(defaultRegion), func(o *pricing.Options) { o.BaseEndpoint = p.d.base(ServicePricing) })
}

// Connect checks the credentials with GetCallerIdentity.
func (p *Provider) Connect(ctx context.Context) (byoc.Account, error) {
	who, err := p.d.sts(p.home, p.creds).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return byoc.Account{}, mapErr("sts:GetCallerIdentity", err)
	}
	return byoc.Account{ID: aws.ToString(who.Account), Name: p.conn.AccountName, Principal: aws.ToString(who.Arn)}, nil
}

// CheckPermissions simulates every action CloudBurrow needs against the
// signed-in principal's policies (iam:SimulatePrincipalPolicy).
func (p *Provider) CheckPermissions(ctx context.Context, region string) (byoc.PermissionReport, error) {
	who, err := p.d.sts(p.home, p.creds).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return byoc.PermissionReport{}, mapErr("sts:GetCallerIdentity", err)
	}
	arn := aws.ToString(who.Arn)
	if strings.HasSuffix(arn, ":root") {
		return byoc.PermissionReport{Checked: true, Note: "signed in as the account's root user, which may do everything; " +
			"an IAM Identity Center user or an IAM user with only these permissions is safer"}, nil
	}
	source := arn
	if i := strings.Index(arn, ":assumed-role/"); i >= 0 {
		// arn:aws:sts::ACCOUNT:assumed-role/ROLE/SESSION: the policies are
		// the role's, whose ARN carries its path (IAM Identity Center's
		// roles live under /aws-reserved/sso.amazonaws.com/).
		role := strings.SplitN(arn[i+len(":assumed-role/"):], "/", 2)[0]
		out, err := p.iam().GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)})
		if err != nil {
			if _, ok := mapErr("iam:GetRole", err).(*byoc.PermissionError); ok {
				return byoc.PermissionReport{Note: "this sign-in may not read its own role (iam:GetRole), so CloudBurrow cannot check its permissions in advance; " +
					"a missing one is named when a step needs it"}, nil
			}
			return byoc.PermissionReport{}, mapErr("iam:GetRole", err)
		}
		source = aws.ToString(out.Role.Arn)
	}
	actions := make([]string, len(permissions))
	for i, q := range permissions {
		actions[i] = q.Action
	}
	decisions := map[string]string{}
	in := &iam.SimulatePrincipalPolicyInput{PolicySourceArn: aws.String(source), ActionNames: actions}
	pg := iam.NewSimulatePrincipalPolicyPaginator(p.iam(), in)
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			if _, ok := mapErr("iam:SimulatePrincipalPolicy", err).(*byoc.PermissionError); ok {
				return byoc.PermissionReport{Note: "this sign-in may not simulate its own policies (iam:SimulatePrincipalPolicy), so CloudBurrow cannot check its permissions in advance; " +
					"a missing one is named when a step needs it"}, nil
			}
			return byoc.PermissionReport{}, mapErr("iam:SimulatePrincipalPolicy", err)
		}
		for _, r := range page.EvaluationResults {
			decisions[aws.ToString(r.EvalActionName)] = string(r.EvalDecision)
		}
	}
	rep := byoc.PermissionReport{Checked: true}
	for _, q := range permissions {
		if d, ok := decisions[q.Action]; !ok || d != "allowed" {
			rep.Missing = append(rep.Missing, q)
		}
	}
	return rep, nil
}

// Regions is the account's enabled regions.
func (p *Provider) Regions(ctx context.Context) ([]byoc.Region, error) {
	out, err := p.ec2(p.region("")).DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, mapErr("ec2:DescribeRegions", err)
	}
	var rs []byoc.Region
	for _, r := range out.Regions {
		name := aws.ToString(r.RegionName)
		info, ok := regions[name]
		if !ok {
			info = regionInfo{display: name}
		}
		rs = append(rs, byoc.Region{Name: name, DisplayName: info.display, Latitude: info.lat, Longitude: info.lon})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
	return rs, nil
}

func (p *Provider) region(r string) string {
	if r != "" {
		return r
	}
	if p.home != "" {
		return p.home
	}
	return defaultRegion
}

// Sizes is every preset in region with its on-demand Linux price from the
// AWS Price List API.
func (p *Provider) Sizes(ctx context.Context, region string) ([]byoc.Size, error) {
	if region == "" {
		return nil, errors.New("a region is required")
	}
	disk, diskErr := p.price(ctx, []pricingtypes.Filter{
		termMatch("productFamily", "Storage"), termMatch("volumeApiName", "gp3"), termMatch("regionCode", region),
	}, "GB-Mo")
	out := make([]byoc.Size, 0, len(sizes))
	for _, s := range sizes {
		sz := byoc.Size{Preset: s.preset, Name: s.name, VCPUs: s.vcpus, MemoryGiB: s.memoryGiB, GPU: s.gpu, GPUs: s.gpus,
			PriceSource: "AWS Price List API: on-demand, Linux, shared tenancy, " + region}
		hourly, err := p.price(ctx, []pricingtypes.Filter{
			termMatch("instanceType", s.name), termMatch("regionCode", region), termMatch("operatingSystem", "Linux"),
			termMatch("tenancy", "Shared"), termMatch("preInstalledSw", "NA"), termMatch("capacitystatus", "Used"),
			termMatch("licenseModel", "No License required"),
		}, "Hrs")
		if err != nil {
			sz.PriceError = err.Error()
		} else {
			sz.HourlyUSD = hourly
		}
		if diskErr == nil {
			sz.DiskHourlyUSD = disk * defaultDiskGiB / 730
		}
		out = append(out, sz)
	}
	return out, nil
}

func termMatch(field, value string) pricingtypes.Filter {
	return pricingtypes.Filter{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String(field), Value: aws.String(value)}
}

// priceListItem is the part of a Price List product CloudBurrow reads.
type priceListItem struct {
	Terms struct {
		OnDemand map[string]struct {
			PriceDimensions map[string]struct {
				Unit         string            `json:"unit"`
				PricePerUnit map[string]string `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

// price is the USD on-demand price per unit of the one product filters
// match.
func (p *Provider) price(ctx context.Context, filters []pricingtypes.Filter, unit string) (float64, error) {
	out, err := p.pricing().GetProducts(ctx, &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"), Filters: filters, FormatVersion: aws.String("aws_v1"), MaxResults: aws.Int32(10),
	})
	if err != nil {
		return 0, fmt.Errorf("the price could not be read: %w", mapErr("pricing:GetProducts", err))
	}
	for _, raw := range out.PriceList {
		var it priceListItem
		if err := json.Unmarshal([]byte(raw), &it); err != nil {
			continue
		}
		for _, term := range it.Terms.OnDemand {
			for _, dim := range term.PriceDimensions {
				if dim.Unit != unit {
					continue
				}
				if v, err := strconv.ParseFloat(dim.PricePerUnit["USD"], 64); err == nil && v > 0 {
					return v, nil
				}
			}
		}
	}
	return 0, errors.New("the AWS Price List API has no on-demand price for it here: it may not be offered in this region")
}

func tags(kind ec2types.ResourceType, instance string) ec2types.TagSpecification {
	return ec2types.TagSpecification{ResourceType: kind, Tags: []ec2types.Tag{
		{Key: aws.String(byoc.TagKey), Value: aws.String(instance)},
		{Key: aws.String("Name"), Value: aws.String("cloudburrow-" + instance)},
	}}
}

func tagFilter(instance string) ec2types.Filter {
	return ec2types.Filter{Name: aws.String("tag:" + byoc.TagKey), Values: []string{instance}}
}

// ubuntuOwner is Canonical's AWS account, which publishes Ubuntu's AMIs
// (https://documentation.ubuntu.com/aws/aws-how-to/instances/find-ubuntu-images/).
const ubuntuOwner = "099720109477"

const ubuntuImages = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"

// Create builds the network, firewall and VM, tagged; a failure removes what
// was built before it.
func (p *Provider) Create(ctx context.Context, spec byoc.MachineSpec, progress byoc.Progress) (m byoc.Machine, err error) {
	if spec.Instance == "" || spec.Region == "" || spec.Size == "" {
		return byoc.Machine{}, errors.New("an instance name, a region and a size are required")
	}
	disk := spec.DiskGiB
	if disk <= 0 {
		disk = defaultDiskGiB
	}
	c := p.ec2(spec.Region)
	ids := map[string]string{}
	type undo struct {
		what string
		do   func(context.Context) error
	}
	var undos []undo
	defer func() {
		if err == nil {
			return
		}
		progress.Report("rollback", byoc.StepRunning, "Removing what was created: %s", err)
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Minute)
		defer cancel()
		for i := len(undos) - 1; i >= 0; i-- {
			if uerr := undos[i].do(rctx); uerr != nil {
				progress.Report("rollback", byoc.StepFailed, "Could not remove %s: %s", undos[i].what, uerr)
				continue
			}
			progress.Report("rollback", byoc.StepRolledBack, "Removed %s", undos[i].what)
		}
	}()
	fail := func(step, action string, e error) error {
		e = mapErr(action, e)
		progress.Report(step, byoc.StepFailed, "%s", e)
		return e
	}

	progress.Report("network", byoc.StepRunning, "Creating the network (VPC 10.209.0.0/16) in %s", spec.Region)
	vpc, err := c.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.209.0.0/16"),
		TagSpecifications: []ec2types.TagSpecification{tags(ec2types.ResourceTypeVpc, spec.Instance)}})
	if err != nil {
		return byoc.Machine{}, fail("network", "ec2:CreateVpc", err)
	}
	ids["vpc"] = aws.ToString(vpc.Vpc.VpcId)
	undos = append(undos, undo{"the VPC " + ids["vpc"], func(ctx context.Context) error {
		_, e := c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(ids["vpc"])})
		return e
	}})
	sub, err := c.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: aws.String(ids["vpc"]), CidrBlock: aws.String("10.209.1.0/24"),
		TagSpecifications: []ec2types.TagSpecification{tags(ec2types.ResourceTypeSubnet, spec.Instance)}})
	if err != nil {
		return byoc.Machine{}, fail("network", "ec2:CreateSubnet", err)
	}
	ids["subnet"] = aws.ToString(sub.Subnet.SubnetId)
	undos = append(undos, undo{"the subnet " + ids["subnet"], func(ctx context.Context) error {
		_, e := c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: aws.String(ids["subnet"])})
		return e
	}})
	igw, err := c.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{
		TagSpecifications: []ec2types.TagSpecification{tags(ec2types.ResourceTypeInternetGateway, spec.Instance)}})
	if err != nil {
		return byoc.Machine{}, fail("network", "ec2:CreateInternetGateway", err)
	}
	ids["igw"] = aws.ToString(igw.InternetGateway.InternetGatewayId)
	undos = append(undos, undo{"the internet gateway " + ids["igw"], func(ctx context.Context) error {
		_, e := c.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{InternetGatewayId: aws.String(ids["igw"])})
		return e
	}})
	if _, err = c.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{InternetGatewayId: aws.String(ids["igw"]), VpcId: aws.String(ids["vpc"])}); err != nil {
		return byoc.Machine{}, fail("network", "ec2:AttachInternetGateway", err)
	}
	undos = append(undos, undo{"the gateway's attachment", func(ctx context.Context) error {
		_, e := c.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{InternetGatewayId: aws.String(ids["igw"]), VpcId: aws.String(ids["vpc"])})
		return e
	}})
	rtb, err := c.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{VpcId: aws.String(ids["vpc"]),
		TagSpecifications: []ec2types.TagSpecification{tags(ec2types.ResourceTypeRouteTable, spec.Instance)}})
	if err != nil {
		return byoc.Machine{}, fail("network", "ec2:CreateRouteTable", err)
	}
	ids["rtb"] = aws.ToString(rtb.RouteTable.RouteTableId)
	undos = append(undos, undo{"the route table " + ids["rtb"], func(ctx context.Context) error {
		_, e := c.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{RouteTableId: aws.String(ids["rtb"])})
		return e
	}})
	if _, err = c.CreateRoute(ctx, &ec2.CreateRouteInput{RouteTableId: aws.String(ids["rtb"]), DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId: aws.String(ids["igw"])}); err != nil {
		return byoc.Machine{}, fail("network", "ec2:CreateRoute", err)
	}
	assoc, err := c.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{RouteTableId: aws.String(ids["rtb"]), SubnetId: aws.String(ids["subnet"])})
	if err != nil {
		return byoc.Machine{}, fail("network", "ec2:AssociateRouteTable", err)
	}
	ids["rtbassoc"] = aws.ToString(assoc.AssociationId)
	undos = append(undos, undo{"the route table's association", func(ctx context.Context) error {
		_, e := c.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{AssociationId: aws.String(ids["rtbassoc"])})
		return e
	}})
	progress.Report("network", byoc.StepDone, "Created the VPC %s, its subnet, internet gateway and route table", ids["vpc"])

	progress.Report("firewall", byoc.StepRunning, "Creating the firewall (security group)")
	sg, err := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:         aws.String("cloudburrow-" + spec.Instance),
		Description:       aws.String("CloudBurrow instance " + spec.Instance + ": SSH from the CloudBurrow machine only"),
		VpcId:             aws.String(ids["vpc"]),
		TagSpecifications: []ec2types.TagSpecification{tags(ec2types.ResourceTypeSecurityGroup, spec.Instance)},
	})
	if err != nil {
		return byoc.Machine{}, fail("firewall", "ec2:CreateSecurityGroup", err)
	}
	ids["sg"] = aws.ToString(sg.GroupId)
	undos = append(undos, undo{"the security group " + ids["sg"], func(ctx context.Context) error {
		_, e := c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(ids["sg"])})
		return e
	}})
	if spec.SourceCIDR != "" {
		if _, err = c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(ids["sg"]),
			IpPermissions: []ec2types.IpPermission{{
				IpProtocol: aws.String("tcp"), FromPort: aws.Int32(22), ToPort: aws.Int32(22),
				IpRanges: []ec2types.IpRange{{CidrIp: aws.String(spec.SourceCIDR), Description: aws.String("SSH from the CloudBurrow machine")}},
			}},
		}); err != nil {
			return byoc.Machine{}, fail("firewall", "ec2:AuthorizeSecurityGroupIngress", err)
		}
		progress.Report("firewall", byoc.StepDone, "Created the security group %s: SSH from %s only, nothing else inbound", ids["sg"], spec.SourceCIDR)
	} else {
		progress.Report("firewall", byoc.StepDone, "Created the security group %s: nothing inbound", ids["sg"])
	}

	progress.Report("image", byoc.StepRunning, "Finding Canonical's Ubuntu 24.04 image")
	imgs, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{Owners: []string{ubuntuOwner}, Filters: []ec2types.Filter{
		{Name: aws.String("name"), Values: []string{ubuntuImages}},
		{Name: aws.String("state"), Values: []string{"available"}},
		{Name: aws.String("architecture"), Values: []string{"x86_64"}},
	}})
	if err != nil {
		return byoc.Machine{}, fail("image", "ec2:DescribeImages", err)
	}
	if len(imgs.Images) == 0 {
		err = fmt.Errorf("no Ubuntu 24.04 image from Canonical is offered in %s", spec.Region)
		return byoc.Machine{}, fail("image", "ec2:DescribeImages", err)
	}
	img := imgs.Images[0]
	for _, i := range imgs.Images[1:] {
		if aws.ToString(i.CreationDate) > aws.ToString(img.CreationDate) {
			img = i
		}
	}
	ids["ami"] = aws.ToString(img.ImageId)
	root := aws.ToString(img.RootDeviceName)
	if root == "" {
		root = "/dev/sda1"
	}
	progress.Report("image", byoc.StepDone, "Using %s (%s)", aws.ToString(img.Name), ids["ami"])

	progress.Report("vm", byoc.StepRunning, "Creating the VM (%s) with a %d GiB encrypted gp3 disk", spec.Size, disk)
	run, err := c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: aws.String(ids["ami"]), InstanceType: ec2types.InstanceType(spec.Size),
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		UserData:                          aws.String(base64.StdEncoding.EncodeToString([]byte(spec.UserData))),
		InstanceInitiatedShutdownBehavior: ec2types.ShutdownBehaviorStop,
		MetadataOptions: &ec2types.InstanceMetadataOptionsRequest{
			HttpTokens: ec2types.HttpTokensStateRequired, HttpEndpoint: ec2types.InstanceMetadataEndpointStateEnabled},
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{{DeviceName: aws.String(root), Ebs: &ec2types.EbsBlockDevice{
			VolumeSize: aws.Int32(int32(disk)), VolumeType: ec2types.VolumeTypeGp3, Encrypted: aws.Bool(true), DeleteOnTermination: aws.Bool(true)}}},
		NetworkInterfaces: []ec2types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex: aws.Int32(0), SubnetId: aws.String(ids["subnet"]), Groups: []string{ids["sg"]},
			AssociatePublicIpAddress: aws.Bool(true), DeleteOnTermination: aws.Bool(true)}},
		TagSpecifications: []ec2types.TagSpecification{
			tags(ec2types.ResourceTypeInstance, spec.Instance), tags(ec2types.ResourceTypeVolume, spec.Instance),
			tags(ec2types.ResourceTypeNetworkInterface, spec.Instance)},
	})
	if err != nil {
		return byoc.Machine{}, fail("vm", "ec2:RunInstances", err)
	}
	if len(run.Instances) == 0 {
		err = errors.New("RunInstances returned no instance")
		return byoc.Machine{}, fail("vm", "ec2:RunInstances", err)
	}
	ids["instance"] = aws.ToString(run.Instances[0].InstanceId)
	undos = append(undos, undo{"the VM " + ids["instance"], func(ctx context.Context) error {
		if _, e := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{ids["instance"]}}); e != nil {
			return e
		}
		_, e := p.wait(ctx, c, ids["instance"], "terminated")
		return e
	}})
	progress.Report("vm", byoc.StepDone, "Created the VM %s", ids["instance"])

	progress.Report("boot", byoc.StepRunning, "Waiting for the VM to run")
	inst, err := p.wait(ctx, c, ids["instance"], "running")
	if err != nil {
		return byoc.Machine{}, fail("boot", "ec2:DescribeInstances", err)
	}
	now := p.d.opts.clock().Now().UTC()
	m = byoc.Machine{
		Instance: spec.Instance, Cloud: byoc.AWS, Connection: p.conn.ID, Region: spec.Region,
		Size: spec.Size, Preset: spec.Preset, DiskGiB: disk, AutoStopMinutes: spec.AutoStopMinutes,
		SSHUser: spec.SSHUser, SSHPort: 22, IDs: ids, CreatedAt: now, UpdatedAt: now,
	}
	if m.Preset == "" {
		m.Preset = presetOf(spec.Size)
	}
	fill(&m, inst)
	progress.Report("boot", byoc.StepDone, "The VM is running at %s; cloud-init is installing Docker and kind", m.PublicIP)
	return m, nil
}

// fill copies an instance's state, addresses and volume into m.
func fill(m *byoc.Machine, inst ec2types.Instance) {
	if inst.State != nil {
		m.State = stateOf(string(inst.State.Name))
	}
	m.PublicIP = aws.ToString(inst.PublicIpAddress)
	m.PrivateIP = aws.ToString(inst.PrivateIpAddress)
	if inst.InstanceType != "" {
		m.Size = string(inst.InstanceType)
		if p := presetOf(m.Size); p != "" {
			m.Preset = p
		}
	}
	for _, b := range inst.BlockDeviceMappings {
		if b.Ebs != nil && b.Ebs.VolumeId != nil {
			if m.IDs == nil {
				m.IDs = map[string]string{}
			}
			m.IDs["volume"] = aws.ToString(b.Ebs.VolumeId)
		}
	}
}

func stateOf(name string) byoc.State {
	switch name {
	case "pending":
		return byoc.Starting
	case "running":
		return byoc.Running
	case "stopping":
		return byoc.Stopping
	case "stopped":
		return byoc.Stopped
	case "shutting-down":
		return byoc.Deleting
	case "terminated":
		return byoc.Deleted
	}
	return byoc.Unknown
}

// maxPolls bounds a wait on a VM's state: two hours at the default pace,
// past any start, stop or terminate AWS documents.
const maxPolls = 1440

// wait reads the instance until it is in state want.
func (p *Provider) wait(ctx context.Context, c *ec2.Client, id, want string) (ec2types.Instance, error) {
	clock := p.d.opts.clock()
	for i := 0; i < maxPolls; i++ {
		out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
		if err != nil {
			if apiCode(err) == "InvalidInstanceID.NotFound" && want == "terminated" {
				return ec2types.Instance{}, nil
			}
			return ec2types.Instance{}, mapErr("ec2:DescribeInstances", err)
		}
		for _, r := range out.Reservations {
			for _, inst := range r.Instances {
				if inst.State != nil && string(inst.State.Name) == want {
					return inst, nil
				}
				if inst.State != nil && string(inst.State.Name) == "terminated" {
					return inst, fmt.Errorf("the VM %s was terminated (%s)", id, reason(inst))
				}
			}
		}
		select {
		case <-ctx.Done():
			return ec2types.Instance{}, ctx.Err()
		case <-clock.After(p.d.opts.poll()):
		}
	}
	return ec2types.Instance{}, fmt.Errorf("the VM %s did not reach %s", id, want)
}

func reason(inst ec2types.Instance) string {
	if inst.StateReason != nil && inst.StateReason.Message != nil {
		return aws.ToString(inst.StateReason.Message)
	}
	return aws.ToString(inst.StateTransitionReason)
}

// Status reads the VM.
func (p *Provider) Status(ctx context.Context, m byoc.Machine) (byoc.Machine, error) {
	id := m.IDs["instance"]
	if id == "" {
		return m, fmt.Errorf("machine %s has no instance ID", m.Instance)
	}
	out, err := p.ec2(m.Region).DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		if apiCode(err) == "InvalidInstanceID.NotFound" {
			m.State = byoc.Deleted
			return m, nil
		}
		return m, mapErr("ec2:DescribeInstances", err)
	}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			fill(&m, inst)
		}
	}
	m.UpdatedAt = p.d.opts.clock().Now().UTC()
	return m, nil
}

// Start starts the VM and waits for it to run.
func (p *Provider) Start(ctx context.Context, m byoc.Machine) (byoc.Machine, error) {
	c := p.ec2(m.Region)
	if _, err := c.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{m.IDs["instance"]}}); err != nil {
		return m, mapErr("ec2:StartInstances", err)
	}
	inst, err := p.wait(ctx, c, m.IDs["instance"], "running")
	if err != nil {
		return m, err
	}
	fill(&m, inst)
	m.UpdatedAt = p.d.opts.clock().Now().UTC()
	return m, nil
}

// Stop stops the VM and waits for it to stop. Its disk and the cluster's
// state on it are kept; only the disk is billed while it is stopped.
func (p *Provider) Stop(ctx context.Context, m byoc.Machine) (byoc.Machine, error) {
	c := p.ec2(m.Region)
	if _, err := c.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{m.IDs["instance"]}}); err != nil {
		return m, mapErr("ec2:StopInstances", err)
	}
	inst, err := p.wait(ctx, c, m.IDs["instance"], "stopped")
	if err != nil {
		return m, err
	}
	fill(&m, inst)
	m.UpdatedAt = p.d.opts.clock().Now().UTC()
	return m, nil
}

// Resize changes the instance type. EC2 changes it only while the instance
// is stopped, so a running VM is stopped, resized and started again.
func (p *Provider) Resize(ctx context.Context, m byoc.Machine, size string, progress byoc.Progress) (byoc.Machine, error) {
	if size == "" {
		return m, errors.New("a size is required")
	}
	cur, err := p.Status(ctx, m)
	if err != nil {
		return m, err
	}
	m = cur
	if m.Size == size {
		progress.Report("resize", byoc.StepDone, "The VM is already %s", size)
		return m, nil
	}
	wasRunning := m.State == byoc.Running || m.State == byoc.Starting
	if wasRunning {
		progress.Report("stop", byoc.StepRunning, "Stopping the VM: EC2 changes an instance type only while it is stopped")
		if m, err = p.Stop(ctx, m); err != nil {
			progress.Report("stop", byoc.StepFailed, "%s", err)
			return m, err
		}
		progress.Report("stop", byoc.StepDone, "Stopped; the disk and the cluster's state on it are kept")
	}
	progress.Report("resize", byoc.StepRunning, "Changing the instance type from %s to %s", m.Size, size)
	if _, err := p.ec2(m.Region).ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(m.IDs["instance"]), InstanceType: &ec2types.AttributeValue{Value: aws.String(size)},
	}); err != nil {
		err = mapErr("ec2:ModifyInstanceAttribute", err)
		progress.Report("resize", byoc.StepFailed, "%s", err)
		if wasRunning {
			if back, serr := p.Start(ctx, m); serr == nil {
				m = back
			}
		}
		return m, err
	}
	m.Size = size
	if pr := presetOf(size); pr != "" {
		m.Preset = pr
	}
	progress.Report("resize", byoc.StepDone, "The VM is now %s", size)
	if wasRunning {
		progress.Report("start", byoc.StepRunning, "Starting the VM again")
		if m, err = p.Start(ctx, m); err != nil {
			progress.Report("start", byoc.StepFailed, "%s", err)
			return m, err
		}
		progress.Report("start", byoc.StepDone, "The VM is running at %s", m.PublicIP)
	}
	return m, nil
}

// ListResources is every resource in region tagged with the instance;
// terminated instances, which AWS lists for an hour, are not counted.
func (p *Provider) ListResources(ctx context.Context, instance, region string) ([]byoc.Resource, error) {
	c := p.ec2(region)
	f := []ec2types.Filter{tagFilter(instance)}
	var out []byoc.Resource
	add := func(kind, id, state string) {
		out = append(out, byoc.Resource{Type: kind, ID: id, Name: "cloudburrow-" + instance, Region: region, State: state})
	}
	inst, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: f})
	if err != nil {
		return nil, mapErr("ec2:DescribeInstances", err)
	}
	for _, r := range inst.Reservations {
		for _, i := range r.Instances {
			if i.State != nil && i.State.Name == ec2types.InstanceStateNameTerminated {
				continue
			}
			st := ""
			if i.State != nil {
				st = string(i.State.Name)
			}
			add("ec2:instance", aws.ToString(i.InstanceId), st)
		}
	}
	vols, err := c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{Filters: f})
	if err != nil {
		return nil, mapErr("ec2:DescribeVolumes", err)
	}
	for _, v := range vols.Volumes {
		add("ec2:volume", aws.ToString(v.VolumeId), string(v.State))
	}
	sgs, err := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: f})
	if err != nil {
		return nil, mapErr("ec2:DescribeSecurityGroups", err)
	}
	for _, g := range sgs.SecurityGroups {
		add("ec2:security-group", aws.ToString(g.GroupId), "")
	}
	rtbs, err := c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: f})
	if err != nil {
		return nil, mapErr("ec2:DescribeRouteTables", err)
	}
	for _, t := range rtbs.RouteTables {
		add("ec2:route-table", aws.ToString(t.RouteTableId), "")
	}
	subs, err := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: f})
	if err != nil {
		return nil, mapErr("ec2:DescribeSubnets", err)
	}
	for _, s := range subs.Subnets {
		add("ec2:subnet", aws.ToString(s.SubnetId), string(s.State))
	}
	igws, err := c.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{Filters: f})
	if err != nil {
		return nil, mapErr("ec2:DescribeInternetGateways", err)
	}
	for _, g := range igws.InternetGateways {
		add("ec2:internet-gateway", aws.ToString(g.InternetGatewayId), "")
	}
	vpcs, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: f})
	if err != nil {
		return nil, mapErr("ec2:DescribeVpcs", err)
	}
	for _, v := range vpcs.Vpcs {
		add("ec2:vpc", aws.ToString(v.VpcId), string(v.State))
	}
	return out, nil
}

// Delete removes every resource in region tagged with the instance, in
// dependency order, then lists again: anything still there is a
// *byoc.LeftoverError.
func (p *Provider) Delete(ctx context.Context, instance, region string, progress byoc.Progress) error {
	if instance == "" || region == "" {
		return errors.New("an instance name and a region are required")
	}
	c := p.ec2(region)
	f := []ec2types.Filter{tagFilter(instance)}
	var problems []string
	note := func(step, what string, err error) {
		err = mapErr(step, err)
		problems = append(problems, what+": "+err.Error())
		progress.Report("delete", byoc.StepFailed, "Could not delete %s: %s", what, err)
	}

	inst, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: f})
	if err != nil {
		return mapErr("ec2:DescribeInstances", err)
	}
	var live []string
	for _, r := range inst.Reservations {
		for _, i := range r.Instances {
			if i.State != nil && i.State.Name != ec2types.InstanceStateNameTerminated {
				live = append(live, aws.ToString(i.InstanceId))
			}
		}
	}
	if len(live) > 0 {
		progress.Report("vm", byoc.StepRunning, "Deleting the VM %s and its disk", strings.Join(live, ", "))
		if _, err := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: live}); err != nil {
			note("ec2:TerminateInstances", "the VM", err)
		} else {
			for _, id := range live {
				if _, err := p.wait(ctx, c, id, "terminated"); err != nil {
					note("ec2:DescribeInstances", "the VM "+id, err)
				}
			}
			progress.Report("vm", byoc.StepDone, "Deleted the VM")
		}
	}
	if vols, err := c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{Filters: f}); err != nil {
		note("ec2:DescribeVolumes", "the disks", err)
	} else {
		for _, v := range vols.Volumes {
			if _, err := c.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: v.VolumeId}); err != nil && apiCode(err) != "InvalidVolume.NotFound" {
				note("ec2:DeleteVolume", "the disk "+aws.ToString(v.VolumeId), err)
			}
		}
	}
	progress.Report("network", byoc.StepRunning, "Deleting the firewall and the network")
	if sgs, err := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: f}); err != nil {
		note("ec2:DescribeSecurityGroups", "the security group", err)
	} else {
		for _, g := range sgs.SecurityGroups {
			if _, err := c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: g.GroupId}); err != nil {
				note("ec2:DeleteSecurityGroup", "the security group "+aws.ToString(g.GroupId), err)
			}
		}
	}
	if rtbs, err := c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: f}); err != nil {
		note("ec2:DescribeRouteTables", "the route table", err)
	} else {
		for _, t := range rtbs.RouteTables {
			for _, a := range t.Associations {
				if aws.ToBool(a.Main) {
					continue
				}
				if _, err := c.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{AssociationId: a.RouteTableAssociationId}); err != nil {
					note("ec2:DisassociateRouteTable", "the route table's association", err)
				}
			}
			if _, err := c.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{RouteTableId: t.RouteTableId}); err != nil {
				note("ec2:DeleteRouteTable", "the route table "+aws.ToString(t.RouteTableId), err)
			}
		}
	}
	if subs, err := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: f}); err != nil {
		note("ec2:DescribeSubnets", "the subnet", err)
	} else {
		for _, s := range subs.Subnets {
			if _, err := c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: s.SubnetId}); err != nil {
				note("ec2:DeleteSubnet", "the subnet "+aws.ToString(s.SubnetId), err)
			}
		}
	}
	if igws, err := c.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{Filters: f}); err != nil {
		note("ec2:DescribeInternetGateways", "the internet gateway", err)
	} else {
		for _, g := range igws.InternetGateways {
			for _, a := range g.Attachments {
				if _, err := c.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{InternetGatewayId: g.InternetGatewayId, VpcId: a.VpcId}); err != nil {
					note("ec2:DetachInternetGateway", "the internet gateway's attachment", err)
				}
			}
			if _, err := c.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{InternetGatewayId: g.InternetGatewayId}); err != nil {
				note("ec2:DeleteInternetGateway", "the internet gateway "+aws.ToString(g.InternetGatewayId), err)
			}
		}
	}
	if vpcs, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: f}); err != nil {
		note("ec2:DescribeVpcs", "the VPC", err)
	} else {
		for _, v := range vpcs.Vpcs {
			if _, err := c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: v.VpcId}); err != nil {
				note("ec2:DeleteVpc", "the VPC "+aws.ToString(v.VpcId), err)
			}
		}
	}

	progress.Report("verify", byoc.StepRunning, "Checking that nothing tagged %s=%s is left in %s", byoc.TagKey, instance, region)
	left, err := p.ListResources(ctx, instance, region)
	if err != nil {
		progress.Report("verify", byoc.StepFailed, "%s", err)
		return fmt.Errorf("the check after delete could not run: %w", err)
	}
	if len(left) > 0 {
		lerr := &byoc.LeftoverError{Resources: left}
		progress.Report("verify", byoc.StepFailed, "%s", lerr)
		return lerr
	}
	if len(problems) > 0 {
		progress.Report("verify", byoc.StepDone, "Nothing is left, though some steps reported: %s", strings.Join(problems, "; "))
	} else {
		progress.Report("verify", byoc.StepDone, "Nothing tagged %s=%s is left", byoc.TagKey, instance)
	}
	return nil
}
