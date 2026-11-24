package awsprov

import "github.com/cloudburrow/cloudburrow/internal/byoc"

// regionInfo is a region's name as the AWS console shows it and the rough
// location of its data centres (the metro area AWS names), which picks the
// closest region by default.
type regionInfo struct {
	display  string
	lat, lon float64
}

// regions is AWS's commercial regions
// (https://docs.aws.amazon.com/global-infrastructure/latest/regions/aws-regions.html).
var regions = map[string]regionInfo{
	"us-east-1":      {"US East (N. Virginia)", 38.9, -77.4},
	"us-east-2":      {"US East (Ohio)", 40.0, -83.0},
	"us-west-1":      {"US West (N. California)", 37.4, -121.9},
	"us-west-2":      {"US West (Oregon)", 45.6, -121.2},
	"af-south-1":     {"Africa (Cape Town)", -33.9, 18.4},
	"ap-east-1":      {"Asia Pacific (Hong Kong)", 22.3, 114.2},
	"ap-east-2":      {"Asia Pacific (Taipei)", 25.0, 121.5},
	"ap-south-1":     {"Asia Pacific (Mumbai)", 19.1, 72.9},
	"ap-south-2":     {"Asia Pacific (Hyderabad)", 17.4, 78.5},
	"ap-southeast-1": {"Asia Pacific (Singapore)", 1.3, 103.8},
	"ap-southeast-2": {"Asia Pacific (Sydney)", -33.9, 151.2},
	"ap-southeast-3": {"Asia Pacific (Jakarta)", -6.2, 106.8},
	"ap-southeast-4": {"Asia Pacific (Melbourne)", -37.8, 145.0},
	"ap-southeast-5": {"Asia Pacific (Malaysia)", 3.1, 101.7},
	"ap-southeast-7": {"Asia Pacific (Thailand)", 13.8, 100.5},
	"ap-northeast-1": {"Asia Pacific (Tokyo)", 35.7, 139.7},
	"ap-northeast-2": {"Asia Pacific (Seoul)", 37.6, 127.0},
	"ap-northeast-3": {"Asia Pacific (Osaka)", 34.7, 135.5},
	"ca-central-1":   {"Canada (Central)", 45.5, -73.6},
	"ca-west-1":      {"Canada West (Calgary)", 51.0, -114.1},
	"eu-central-1":   {"Europe (Frankfurt)", 50.1, 8.7},
	"eu-central-2":   {"Europe (Zurich)", 47.4, 8.5},
	"eu-west-1":      {"Europe (Ireland)", 53.3, -6.3},
	"eu-west-2":      {"Europe (London)", 51.5, -0.1},
	"eu-west-3":      {"Europe (Paris)", 48.9, 2.4},
	"eu-north-1":     {"Europe (Stockholm)", 59.3, 18.1},
	"eu-south-1":     {"Europe (Milan)", 45.5, 9.2},
	"eu-south-2":     {"Europe (Spain)", 41.6, -0.9},
	"il-central-1":   {"Israel (Tel Aviv)", 32.1, 34.8},
	"me-central-1":   {"Middle East (UAE)", 25.2, 55.3},
	"me-south-1":     {"Middle East (Bahrain)", 26.1, 50.6},
	"mx-central-1":   {"Mexico (Central)", 20.6, -100.4},
	"sa-east-1":      {"South America (Sao Paulo)", -23.5, -46.6},
}

// sizeSpec is a preset's instance type
// (https://aws.amazon.com/ec2/instance-types/m7i/ and
// https://aws.amazon.com/ec2/instance-types/g6/).
type sizeSpec struct {
	preset    byoc.Preset
	name      string
	vcpus     int
	memoryGiB float64
	gpu       string
	gpus      int
}

// sizes is the presets in the slider's order. GPU is g6.xlarge: one NVIDIA
// L4 Tensor Core GPU (24 GB), 4 vCPUs and 16 GiB, as Google's
// g2-standard-4 is one L4 with 4 vCPUs and 16 GB.
var sizes = []sizeSpec{
	{byoc.Small, "m7i.xlarge", 4, 16, "", 0},
	{byoc.Medium, "m7i.4xlarge", 16, 64, "", 0},
	{byoc.Large, "m7i.8xlarge", 32, 128, "", 0},
	{byoc.GPU, "g6.xlarge", 4, 16, "NVIDIA L4", 1},
}

// presetOf is the preset whose instance type is name, or "".
func presetOf(name string) byoc.Preset {
	for _, s := range sizes {
		if s.name == name {
			return s.preset
		}
	}
	return ""
}

// defaultDiskGiB is the boot disk's size when the spec names none: room for
// kind's node image, the images an instance pulls, and its state.
const defaultDiskGiB = 100

// permissions is every IAM action CloudBurrow's AWS calls need, with why.
var permissions = []byoc.Permission{
	{Action: "ec2:DescribeRegions", Why: "list the regions you can choose"},
	{Action: "ec2:DescribeImages", Why: "find Canonical's Ubuntu 24.04 image"},
	{Action: "ec2:CreateVpc", Why: "create the VM's own network"},
	{Action: "ec2:CreateSubnet", Why: "create the VM's subnet"},
	{Action: "ec2:CreateInternetGateway", Why: "let the VM download Docker and kind"},
	{Action: "ec2:AttachInternetGateway", Why: "let the VM download Docker and kind"},
	{Action: "ec2:CreateRouteTable", Why: "route the VM's subnet to the internet"},
	{Action: "ec2:CreateRoute", Why: "route the VM's subnet to the internet"},
	{Action: "ec2:AssociateRouteTable", Why: "route the VM's subnet to the internet"},
	{Action: "ec2:CreateSecurityGroup", Why: "create the VM's firewall"},
	{Action: "ec2:AuthorizeSecurityGroupIngress", Why: "allow SSH from your address only"},
	{Action: "ec2:RunInstances", Why: "create the VM and its disk"},
	{Action: "ec2:CreateTags", Why: "tag every resource with cloudburrow-instance"},
	{Action: "ec2:DescribeInstances", Why: "read the VM's state and address"},
	{Action: "ec2:DescribeVpcs", Why: "find the resources CloudBurrow created"},
	{Action: "ec2:DescribeSubnets", Why: "find the resources CloudBurrow created"},
	{Action: "ec2:DescribeSecurityGroups", Why: "find the resources CloudBurrow created"},
	{Action: "ec2:DescribeInternetGateways", Why: "find the resources CloudBurrow created"},
	{Action: "ec2:DescribeRouteTables", Why: "find the resources CloudBurrow created"},
	{Action: "ec2:DescribeVolumes", Why: "find the resources CloudBurrow created"},
	{Action: "ec2:StartInstances", Why: "start the VM"},
	{Action: "ec2:StopInstances", Why: "stop the VM"},
	{Action: "ec2:ModifyInstanceAttribute", Why: "resize the VM"},
	{Action: "ec2:TerminateInstances", Why: "delete the VM"},
	{Action: "ec2:DeleteVolume", Why: "delete the VM's disk"},
	{Action: "ec2:DeleteSecurityGroup", Why: "delete the VM's firewall"},
	{Action: "ec2:DisassociateRouteTable", Why: "delete the VM's network"},
	{Action: "ec2:DeleteRouteTable", Why: "delete the VM's network"},
	{Action: "ec2:DeleteSubnet", Why: "delete the VM's network"},
	{Action: "ec2:DetachInternetGateway", Why: "delete the VM's network"},
	{Action: "ec2:DeleteInternetGateway", Why: "delete the VM's network"},
	{Action: "ec2:DeleteVpc", Why: "delete the VM's network"},
	{Action: "pricing:GetProducts", Why: "show the hourly price before you create or resize"},
}

// Permissions is the IAM actions CloudBurrow needs, for docs and the
// console's list of what to grant.
func Permissions() []byoc.Permission { return append([]byoc.Permission(nil), permissions...) }

func whyOf(action string) string {
	for _, p := range permissions {
		if p.Action == action {
			return p.Why
		}
	}
	return "a call CloudBurrow makes"
}
