package awsprov

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov/ssmtunnel"
)

// Session Manager (#2209): the VM's role lets its SSM agent register with
// Systems Manager (the AWS managed policy AmazonSSMManagedInstanceCore),
// the security group allows nothing inbound, and CloudBurrow's SSH reaches
// the VM's sshd through a port-forwarding session
// (AWS-StartPortForwardingSession) that ssmtunnel speaks.

// ssmCorePolicy is the managed policy the instance role carries
// (https://docs.aws.amazon.com/systems-manager/latest/userguide/setup-instance-permissions.html).
const ssmCorePolicy = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"

// portForwardingDocument is the SSM document for port forwarding
// (https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-sessions-start.html#sessions-start-port-forwarding).
const portForwardingDocument = "AWS-StartPortForwardingSession"

// ServiceSSM is the Systems Manager API; its data channel's URL comes back
// from StartSession.
const ServiceSSM = "ssm"

// roleName is the instance's IAM role and instance profile name.
func roleName(instance string) string { return "cloudburrow-" + instance + "-ssm" }

const ec2TrustPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

func (p *Provider) ssm(region string) *ssm.Client {
	return ssm.NewFromConfig(p.cfg(region), func(o *ssm.Options) { o.BaseEndpoint = p.d.base(ServiceSSM) })
}

func iamTags(instance string) []iamtypes.Tag {
	return []iamtypes.Tag{{Key: aws.String(byoc.TagKey), Value: aws.String(instance)}}
}

// createInstanceRole makes the role and instance profile, returning undo
// steps for a failed create.
func (p *Provider) createInstanceRole(ctx context.Context, instance string, progress byoc.Progress) (string, []func(context.Context) error, error) {
	c := p.iam()
	name := roleName(instance)
	var undos []func(context.Context) error
	progress.Report("role", byoc.StepRunning, "Creating the VM's IAM role %s, which lets its SSM agent register with Systems Manager", name)
	if _, err := c.CreateRole(ctx, &iam.CreateRoleInput{RoleName: aws.String(name), AssumeRolePolicyDocument: aws.String(ec2TrustPolicy),
		Description: aws.String("CloudBurrow instance " + instance + ": Session Manager for its VM"), Tags: iamTags(instance)}); err != nil {
		return "", undos, mapErr("iam:CreateRole", err)
	}
	undos = append(undos, func(ctx context.Context) error {
		_, e := c.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(name)})
		return e
	})
	if _, err := c.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{RoleName: aws.String(name), PolicyArn: aws.String(ssmCorePolicy)}); err != nil {
		return "", undos, mapErr("iam:AttachRolePolicy", err)
	}
	undos = append(undos, func(ctx context.Context) error {
		_, e := c.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(name), PolicyArn: aws.String(ssmCorePolicy)})
		return e
	})
	if _, err := c.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{InstanceProfileName: aws.String(name), Tags: iamTags(instance)}); err != nil {
		return "", undos, mapErr("iam:CreateInstanceProfile", err)
	}
	undos = append(undos, func(ctx context.Context) error {
		_, e := c.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(name)})
		return e
	})
	if _, err := c.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{InstanceProfileName: aws.String(name), RoleName: aws.String(name)}); err != nil {
		return "", undos, mapErr("iam:AddRoleToInstanceProfile", err)
	}
	undos = append(undos, func(ctx context.Context) error {
		_, e := c.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{InstanceProfileName: aws.String(name), RoleName: aws.String(name)})
		return e
	})
	progress.Report("role", byoc.StepDone, "Created the role and instance profile %s (AmazonSSMManagedInstanceCore only)", name)
	return name, undos, nil
}

// deleteInstanceRole removes the role and profile; one already gone is
// fine.
func (p *Provider) deleteInstanceRole(ctx context.Context, instance string) []error {
	c := p.iam()
	name := roleName(instance)
	var errs []error
	gone := func(err error) bool { return err == nil || apiCode(err) == "NoSuchEntity" }
	if _, err := c.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{InstanceProfileName: aws.String(name), RoleName: aws.String(name)}); !gone(err) {
		errs = append(errs, mapErr("iam:RemoveRoleFromInstanceProfile", err))
	}
	if _, err := c.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(name)}); !gone(err) {
		errs = append(errs, mapErr("iam:DeleteInstanceProfile", err))
	}
	if _, err := c.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(name), PolicyArn: aws.String(ssmCorePolicy)}); !gone(err) {
		errs = append(errs, mapErr("iam:DetachRolePolicy", err))
	}
	if _, err := c.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(name)}); !gone(err) {
		errs = append(errs, mapErr("iam:DeleteRole", err))
	}
	return errs
}

// instanceRoleResources is the role and profile, if they exist and carry
// the instance's tag.
func (p *Provider) instanceRoleResources(ctx context.Context, instance string) ([]byoc.Resource, error) {
	c := p.iam()
	name := roleName(instance)
	tagged := func(tags []iamtypes.Tag) bool {
		for _, t := range tags {
			if aws.ToString(t.Key) == byoc.TagKey && aws.ToString(t.Value) == instance {
				return true
			}
		}
		return false
	}
	var out []byoc.Resource
	prof, err := c.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(name)})
	switch {
	case err == nil && tagged(prof.InstanceProfile.Tags):
		out = append(out, byoc.Resource{Type: "iam:instance-profile", ID: aws.ToString(prof.InstanceProfile.Arn), Name: name})
	case err != nil && apiCode(err) != "NoSuchEntity":
		return nil, mapErr("iam:GetInstanceProfile", err)
	}
	role, err := c.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	switch {
	case err == nil && tagged(role.Role.Tags):
		out = append(out, byoc.Resource{Type: "iam:role", ID: aws.ToString(role.Role.Arn), Name: name})
	case err != nil && apiCode(err) != "NoSuchEntity":
		return nil, mapErr("iam:GetRole", err)
	}
	return out, nil
}

// runInstancesRetrying is RunInstances, retried while a new instance
// profile is still propagating through IAM (EC2 answers
// InvalidParameterValue "Invalid IAM Instance Profile name" for a few
// seconds after it is made).
func (p *Provider) runInstancesRetrying(ctx context.Context, c *ec2.Client, in *ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
	deadline := p.d.opts.clock().Now().Add(2 * time.Minute)
	for {
		out, err := c.RunInstances(ctx, in)
		if err == nil || in.IamInstanceProfile == nil || apiCode(err) != "InvalidParameterValue" ||
			!strings.Contains(err.Error(), "nstance Profile") || p.d.opts.clock().Now().After(deadline) {
			return out, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(p.d.opts.poll(), 5*time.Second)):
		}
	}
}

// UpdateSourceAddress implements byoc.Provider: the security group's SSH
// rule admits cidr in place of the old address (SSH mode only).
func (p *Provider) UpdateSourceAddress(ctx context.Context, m byoc.Machine, cidr string) (byoc.Machine, error) {
	sg := m.IDs["sg"]
	if sg == "" || cidr == "" {
		return m, errors.New("the machine has no security group, or no address was given")
	}
	c := p.ec2(m.Region)
	out, err := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{sg}})
	if err != nil {
		return m, mapErr("ec2:DescribeSecurityGroups", err)
	}
	have := false
	var stale []ec2types.IpRange
	for _, g := range out.SecurityGroups {
		for _, perm := range g.IpPermissions {
			if aws.ToString(perm.IpProtocol) != "tcp" || aws.ToInt32(perm.FromPort) != 22 || aws.ToInt32(perm.ToPort) != 22 {
				continue
			}
			for _, r := range perm.IpRanges {
				if aws.ToString(r.CidrIp) == cidr {
					have = true
				} else {
					stale = append(stale, ec2types.IpRange{CidrIp: r.CidrIp})
				}
			}
		}
	}
	if !have {
		if _, err := c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(sg),
			IpPermissions: []ec2types.IpPermission{{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(22), ToPort: aws.Int32(22),
				IpRanges: []ec2types.IpRange{{CidrIp: aws.String(cidr), Description: aws.String("SSH from the CloudBurrow machine")}}}}}); err != nil {
			return m, mapErr("ec2:AuthorizeSecurityGroupIngress", err)
		}
	}
	if len(stale) > 0 {
		if _, err := c.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{GroupId: aws.String(sg),
			IpPermissions: []ec2types.IpPermission{{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(22), ToPort: aws.Int32(22), IpRanges: stale}}}); err != nil {
			return m, mapErr("ec2:RevokeSecurityGroupIngress", err)
		}
	}
	m.SourceCIDR = cidr
	return m, nil
}

var _ byoc.Tunneler = (*Provider)(nil)

// OpenTunnel implements byoc.Tunneler: it waits for the VM's SSM agent to
// report Online, then listens on 127.0.0.1 for connections to port, each
// carried by a Session Manager port-forwarding session.
func (p *Provider) OpenTunnel(ctx context.Context, m byoc.Machine, port int, progress byoc.Progress) (byoc.Tunnel, error) {
	id := m.IDs["instance"]
	if id == "" {
		return nil, fmt.Errorf("machine %s has no instance ID", m.Instance)
	}
	if err := p.waitOnline(ctx, m.Region, id, progress); err != nil {
		return nil, err
	}
	st := &starter{p: p, region: m.Region, instance: id, port: port}
	return ssmtunnel.Listen(st, ssmtunnel.Options{})
}

// waitOnline polls DescribeInstanceInformation until the agent is Online:
// it registers a minute or two after the VM first boots.
func (p *Provider) waitOnline(ctx context.Context, region, id string, progress byoc.Progress) error {
	c := p.ssm(region)
	deadline := p.d.opts.clock().Now().Add(10 * time.Minute)
	said := false
	for {
		out, err := c.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
			Filters: []ssmtypes.InstanceInformationStringFilter{{Key: aws.String("InstanceIds"), Values: []string{id}}}})
		if err != nil {
			return mapErr("ssm:DescribeInstanceInformation", err)
		}
		for _, i := range out.InstanceInformationList {
			if i.PingStatus == ssmtypes.PingStatusOnline {
				return nil
			}
		}
		if st, err := p.ec2(region).DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}}); err == nil {
			for _, r := range st.Reservations {
				for _, i := range r.Instances {
					if i.State != nil && i.State.Name != ec2types.InstanceStateNameRunning && i.State.Name != ec2types.InstanceStateNamePending {
						return fmt.Errorf("the VM is %s, so its SSM agent is not connected: start it first", i.State.Name)
					}
				}
			}
		}
		if p.d.opts.clock().Now().After(deadline) {
			return fmt.Errorf("the VM's SSM agent has not registered with Systems Manager after 10 minutes: " +
				"check that the VM can reach the internet (its route table and internet gateway) and that its role still carries " +
				"AmazonSSMManagedInstanceCore; or create it with SSH (--connect ssh)")
		}
		if !said {
			progress.Report("ssm", byoc.StepRunning, "The VM's SSM agent has not registered yet; it does so a minute or two after boot")
			said = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.d.opts.poll()):
		}
	}
}

// starter is ssmtunnel's Starter over the SDK.
type starter struct {
	p        *Provider
	region   string
	instance string
	port     int
}

func (s *starter) Start(ctx context.Context) (ssmtunnel.Session, error) {
	out, err := s.p.ssm(s.region).StartSession(ctx, &ssm.StartSessionInput{Target: aws.String(s.instance),
		DocumentName: aws.String(portForwardingDocument),
		Reason:       aws.String("CloudBurrow: the instance's Docker over SSH"),
		Parameters:   map[string][]string{"portNumber": {strconv.Itoa(s.port)}}})
	if err != nil {
		if apiCode(err) == "TargetNotConnected" {
			return ssmtunnel.Session{}, fmt.Errorf("Session Manager cannot reach %s: its SSM agent is not connected (the VM is stopped, still booting, or cannot reach Systems Manager)", s.instance)
		}
		return ssmtunnel.Session{}, mapErr("ssm:StartSession", err)
	}
	return ssmtunnel.Session{SessionID: aws.ToString(out.SessionId), StreamURL: aws.ToString(out.StreamUrl), TokenValue: aws.ToString(out.TokenValue)}, nil
}

func (s *starter) Terminate(ctx context.Context, id string) error {
	_, err := s.p.ssm(s.region).TerminateSession(ctx, &ssm.TerminateSessionInput{SessionId: aws.String(id)})
	return err
}

// Header signs the stream URL's GET as the plugin does: SigV4 for
// ssmmessages, over the empty body.
func (s *starter) Header(ctx context.Context, streamURL string) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return nil, err
	}
	creds, err := s.p.creds.Retrieve(ctx)
	if err != nil {
		return nil, err
	}
	empty := sha256.Sum256(nil)
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(empty[:]), "ssmmessages", s.region, s.p.d.opts.clock().Now()); err != nil {
		return nil, err
	}
	return req.Header, nil
}
