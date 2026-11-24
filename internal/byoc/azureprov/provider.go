package azureprov

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v2"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

// VirtualMachineContributor is the built-in role a VM's own identity gets on
// the VM alone, so the idle script cloud-init installs can deallocate it
// (Azure keeps billing a VM that only shut its OS down).
const VirtualMachineContributor = "9980e02c-c2be-4d73-94e8-173b1dc7cf3c"

// The names of what Create makes, inside the instance's own resource group.
const (
	vnetName   = "cloudburrow-vnet"
	subnetName = "default"
	nsgName    = "cloudburrow-nsg"
	pipName    = "cloudburrow-ip"
	nicName    = "cloudburrow-nic"
)

// ResourceGroup is the resource group Create makes for instance.
func ResourceGroup(instance string) string { return "cloudburrow-" + instance }

// VMName is the VM Create makes for instance.
func VMName(instance string) string { return "cloudburrow-" + instance }

// Provider is one Azure subscription.
type Provider struct {
	opts         Options
	subscription string
	principal    string
	cred         azcore.TokenCredential

	subs   *armsubscriptions.Client
	groups *armresources.ResourceGroupsClient
	res    *armresources.Client
	vnets  *armnetwork.VirtualNetworksClient
	nsgs   *armnetwork.SecurityGroupsClient
	pips   *armnetwork.PublicIPAddressesClient
	nics   *armnetwork.InterfacesClient
	vms    *armcompute.VirtualMachinesClient
	roles  *armauthorization.RoleAssignmentsClient
	raw    *arm.Client
}

var _ byoc.Provider = (*Provider)(nil)

// NewProvider is subscription's provider, with cred's tokens.
func NewProvider(subscription string, cred azcore.TokenCredential, opts Options) (*Provider, error) {
	opts = opts.withDefaults()
	ao := opts.armOptions()
	p := &Provider{opts: opts, subscription: subscription, cred: cred}
	if c, ok := cred.(*credential); ok {
		p.principal = c.account.PreferredUsername
	}
	var err error
	errs := func(e error) {
		if err == nil {
			err = e
		}
	}
	var e error
	p.subs, e = armsubscriptions.NewClient(cred, ao)
	errs(e)
	p.groups, e = armresources.NewResourceGroupsClient(subscription, cred, ao)
	errs(e)
	p.res, e = armresources.NewClient(subscription, cred, ao)
	errs(e)
	p.vnets, e = armnetwork.NewVirtualNetworksClient(subscription, cred, ao)
	errs(e)
	p.nsgs, e = armnetwork.NewSecurityGroupsClient(subscription, cred, ao)
	errs(e)
	p.pips, e = armnetwork.NewPublicIPAddressesClient(subscription, cred, ao)
	errs(e)
	p.nics, e = armnetwork.NewInterfacesClient(subscription, cred, ao)
	errs(e)
	p.vms, e = armcompute.NewVirtualMachinesClient(subscription, cred, ao)
	errs(e)
	p.roles, e = armauthorization.NewRoleAssignmentsClient(subscription, cred, ao)
	errs(e)
	p.raw, e = arm.NewClient("cloudburrow.azureprov", "v1.0.0", cred, ao)
	errs(e)
	if err != nil {
		return nil, fmt.Errorf("azure clients: %w", err)
	}
	return p, nil
}

// Cloud implements byoc.Provider.
func (*Provider) Cloud() byoc.Cloud { return byoc.Azure }

// Subscription is the provider's subscription ID.
func (p *Provider) Subscription() string { return p.subscription }

// Connect implements byoc.Provider.
func (p *Provider) Connect(ctx context.Context) (byoc.Account, error) {
	res, err := p.subs.Get(ctx, p.subscription, nil)
	if err != nil {
		return byoc.Account{}, mapError("reading subscription "+p.subscription, err)
	}
	return byoc.Account{ID: p.subscription, Name: str(res.DisplayName), Principal: p.principal}, nil
}

// Regions implements byoc.Provider: the subscription's physical regions.
func (p *Provider) Regions(ctx context.Context) ([]byoc.Region, error) {
	var out []byoc.Region
	pager := p.subs.NewListLocationsPager(p.subscription, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapError("listing Azure regions", err)
		}
		for _, l := range page.Value {
			if l == nil || l.Name == nil {
				continue
			}
			if l.Metadata != nil && l.Metadata.RegionType != nil && *l.Metadata.RegionType != armsubscriptions.RegionTypePhysical {
				continue
			}
			r := byoc.Region{Name: *l.Name, DisplayName: str(l.DisplayName)}
			if l.Metadata != nil {
				r.Latitude, r.Longitude = float(l.Metadata.Latitude), float(l.Metadata.Longitude)
			}
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// tags is every resource's tags.
func tags(instance string) map[string]*string {
	return map[string]*string{byoc.TagKey: to.Ptr(instance)}
}

func (p *Provider) rgID(instance string) string {
	return "/subscriptions/" + p.subscription + "/resourceGroups/" + ResourceGroup(instance)
}

// Create implements byoc.Provider.
func (p *Provider) Create(ctx context.Context, spec byoc.MachineSpec, progress byoc.Progress) (m byoc.Machine, err error) {
	if spec.Instance == "" || spec.Region == "" || spec.Size == "" {
		return byoc.Machine{}, errors.New("create: the instance, region and size are required")
	}
	if spec.SSHPublicKey == "" || spec.SSHUser == "" {
		return byoc.Machine{}, errors.New("create: the SSH user and public key are required")
	}
	disk := spec.DiskGiB
	if disk <= 0 {
		disk = DiskGiB
	}
	rg := ResourceGroup(spec.Instance)
	m = byoc.Machine{
		Instance: spec.Instance, Cloud: byoc.Azure, Region: spec.Region, Size: spec.Size, Preset: spec.Preset,
		DiskGiB: disk, AutoStopMinutes: spec.AutoStopMinutes, State: byoc.Provisioning, SSHUser: spec.SSHUser,
		SSHPort: 22, IDs: map[string]string{"subscription": p.subscription}, CreatedAt: p.opts.Clock.Now().UTC(),
	}
	created := false
	defer func() {
		if err == nil || !created {
			return
		}
		progress.Report("rollback", byoc.StepRunning, "Removing what was created in resource group %s", rg)
		if derr := p.deleteGroup(context.WithoutCancel(ctx), rg); derr != nil {
			progress.Report("rollback", byoc.StepFailed, "Resource group %s could not be removed: %v; delete it in the Azure portal", rg, derr)
			err = fmt.Errorf("%w (and the rollback failed: %v)", err, derr)
			return
		}
		progress.Report("rollback", byoc.StepRolledBack, "Removed resource group %s and everything in it", rg)
	}()
	step := func(name, msg string) { progress.Report(name, byoc.StepRunning, "%s", msg) }
	done := func(name, msg string) { progress.Report(name, byoc.StepDone, "%s", msg) }
	fail := func(name, what string, e error) error {
		e = mapError(what, e)
		progress.Report(name, byoc.StepFailed, "%v", e)
		return e
	}

	step("network", "Creating resource group "+rg+" in "+spec.Region)
	g, err := p.groups.CreateOrUpdate(ctx, rg, armresources.ResourceGroup{Location: to.Ptr(spec.Region), Tags: tags(spec.Instance)}, nil)
	if err != nil {
		return m, fail("network", "creating resource group "+rg, err)
	}
	created = true
	m.IDs["resource_group"] = str(g.ID)

	step("firewall", "Creating network security group "+nsgName+": inbound SSH from "+orNone(spec.SourceCIDR)+" only")
	nsg := armnetwork.SecurityGroup{Location: to.Ptr(spec.Region), Tags: tags(spec.Instance), Properties: &armnetwork.SecurityGroupPropertiesFormat{}}
	if spec.SourceCIDR != "" {
		nsg.Properties.SecurityRules = []*armnetwork.SecurityRule{{
			Name: to.Ptr("cloudburrow-ssh"),
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Protocol: to.Ptr(armnetwork.SecurityRuleProtocolTCP), Access: to.Ptr(armnetwork.SecurityRuleAccessAllow),
				Direction: to.Ptr(armnetwork.SecurityRuleDirectionInbound), Priority: to.Ptr[int32](100),
				SourceAddressPrefix: to.Ptr(spec.SourceCIDR), SourcePortRange: to.Ptr("*"),
				DestinationAddressPrefix: to.Ptr("*"), DestinationPortRange: to.Ptr("22"),
				Description: to.Ptr("CloudBurrow's SSH tunnel, from your own address only"),
			},
		}}
	}
	np, err := p.nsgs.BeginCreateOrUpdate(ctx, rg, nsgName, nsg, nil)
	if err != nil {
		return m, fail("firewall", "creating network security group", err)
	}
	nsgRes, err := wait(ctx, p.opts, np)
	if err != nil {
		return m, fail("firewall", "creating network security group", err)
	}
	m.IDs["nsg"] = str(nsgRes.ID)
	done("firewall", "Network security group ready")

	step("network", "Creating virtual network "+vnetName+" (10.209.0.0/16)")
	vp, err := p.vnets.BeginCreateOrUpdate(ctx, rg, vnetName, armnetwork.VirtualNetwork{
		Location: to.Ptr(spec.Region), Tags: tags(spec.Instance),
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{AddressPrefixes: []*string{to.Ptr("10.209.0.0/16")}},
			Subnets: []*armnetwork.Subnet{{Name: to.Ptr(subnetName), Properties: &armnetwork.SubnetPropertiesFormat{
				AddressPrefix: to.Ptr("10.209.0.0/24"), NetworkSecurityGroup: &armnetwork.SecurityGroup{ID: nsgRes.ID},
			}}},
		},
	}, nil)
	if err != nil {
		return m, fail("network", "creating virtual network", err)
	}
	vnet, err := wait(ctx, p.opts, vp)
	if err != nil {
		return m, fail("network", "creating virtual network", err)
	}
	m.IDs["vnet"] = str(vnet.ID)
	var subnetID *string
	if vnet.Properties != nil && len(vnet.Properties.Subnets) > 0 && vnet.Properties.Subnets[0] != nil {
		subnetID = vnet.Properties.Subnets[0].ID
	}
	if subnetID == nil {
		subnetID = to.Ptr(str(vnet.ID) + "/subnets/" + subnetName)
	}
	m.IDs["subnet"] = *subnetID
	done("network", "Virtual network ready")

	step("network", "Creating static public IP "+pipName)
	pp, err := p.pips.BeginCreateOrUpdate(ctx, rg, pipName, armnetwork.PublicIPAddress{
		Location: to.Ptr(spec.Region), Tags: tags(spec.Instance),
		SKU: &armnetwork.PublicIPAddressSKU{Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard)},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
			PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
			DeleteOption:             to.Ptr(armnetwork.DeleteOptionsDelete),
		},
	}, nil)
	if err != nil {
		return m, fail("network", "creating public IP", err)
	}
	pip, err := wait(ctx, p.opts, pp)
	if err != nil {
		return m, fail("network", "creating public IP", err)
	}
	m.IDs["public_ip"] = str(pip.ID)
	if pip.Properties != nil {
		m.PublicIP = str(pip.Properties.IPAddress)
	}

	step("network", "Creating network interface "+nicName)
	ip, err := p.nics.BeginCreateOrUpdate(ctx, rg, nicName, armnetwork.Interface{
		Location: to.Ptr(spec.Region), Tags: tags(spec.Instance),
		Properties: &armnetwork.InterfacePropertiesFormat{
			NetworkSecurityGroup: &armnetwork.SecurityGroup{ID: nsgRes.ID},
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
				Name: to.Ptr("ipconfig1"),
				Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
					Subnet:                    &armnetwork.Subnet{ID: subnetID},
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					PublicIPAddress:           &armnetwork.PublicIPAddress{ID: pip.ID},
				},
			}},
		},
	}, nil)
	if err != nil {
		return m, fail("network", "creating network interface", err)
	}
	nic, err := wait(ctx, p.opts, ip)
	if err != nil {
		return m, fail("network", "creating network interface", err)
	}
	m.IDs["nic"] = str(nic.ID)
	if nic.Properties != nil && len(nic.Properties.IPConfigurations) > 0 && nic.Properties.IPConfigurations[0].Properties != nil {
		m.PrivateIP = str(nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress)
	}
	done("network", "Network interface ready")

	vmName := VMName(spec.Instance)
	step("vm", "Creating VM "+vmName+" ("+spec.Size+", Ubuntu 24.04 LTS) with a "+fmt.Sprint(disk)+" GiB Premium SSD")
	vmp, err := p.vms.BeginCreateOrUpdate(ctx, rg, vmName, armcompute.VirtualMachine{
		Location: to.Ptr(spec.Region), Tags: tags(spec.Instance),
		Identity: &armcompute.VirtualMachineIdentity{Type: to.Ptr(armcompute.ResourceIdentityTypeSystemAssigned)},
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(spec.Size))},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{Publisher: to.Ptr("Canonical"), Offer: to.Ptr("ubuntu-24_04-lts"),
					SKU: to.Ptr("server"), Version: to.Ptr("latest")},
				OSDisk: &armcompute.OSDisk{
					Name: to.Ptr(vmName + "-osdisk"), CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
					DiskSizeGB:   to.Ptr(int32(disk)),
					DeleteOption: to.Ptr(armcompute.DiskDeleteOptionTypesDelete),
					ManagedDisk:  &armcompute.ManagedDiskParameters{StorageAccountType: to.Ptr(armcompute.StorageAccountTypesPremiumLRS)},
				},
			},
			OSProfile: &armcompute.OSProfile{
				ComputerName: to.Ptr(hostname(spec.Instance)), AdminUsername: to.Ptr(spec.SSHUser),
				CustomData: to.Ptr(base64.StdEncoding.EncodeToString([]byte(spec.UserData))),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: to.Ptr(true),
					SSH: &armcompute.SSHConfiguration{PublicKeys: []*armcompute.SSHPublicKey{{
						Path:    to.Ptr("/home/" + spec.SSHUser + "/.ssh/authorized_keys"),
						KeyData: to.Ptr(strings.TrimSpace(spec.SSHPublicKey)),
					}}},
				},
			},
			NetworkProfile: &armcompute.NetworkProfile{NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
				ID: nic.ID, Properties: &armcompute.NetworkInterfaceReferenceProperties{
					Primary: to.Ptr(true), DeleteOption: to.Ptr(armcompute.DeleteOptionsDelete)},
			}}},
		},
	}, nil)
	if err != nil {
		return m, fail("vm", "creating VM "+vmName, err)
	}
	vm, err := wait(ctx, p.opts, vmp)
	if err != nil {
		return m, fail("vm", "creating VM "+vmName, err)
	}
	m.IDs["vm"] = str(vm.ID)
	if vm.Properties != nil && vm.Properties.StorageProfile != nil && vm.Properties.StorageProfile.OSDisk != nil &&
		vm.Properties.StorageProfile.OSDisk.ManagedDisk != nil {
		m.IDs["disk"] = str(vm.Properties.StorageProfile.OSDisk.ManagedDisk.ID)
	}
	if vm.Identity != nil {
		m.IDs["principal"] = str(vm.Identity.PrincipalID)
	}
	done("vm", "VM "+vmName+" is running")

	if spec.AutoStopMinutes > 0 && m.IDs["principal"] != "" {
		step("autostop", "Letting the VM deallocate itself after "+fmt.Sprint(spec.AutoStopMinutes)+" idle minutes (Virtual Machine Contributor on the VM alone)")
		if rerr := p.assignSelfDeallocate(ctx, str(vm.ID), m.IDs["principal"]); rerr != nil {
			progress.Report("autostop", byoc.StepFailed,
				"The VM may not deallocate itself (%v): when idle it will only shut its OS down, which Azure keeps billing; stop it from the Compute capacity page", mapError("assigning the VM's role", rerr))
		} else {
			done("autostop", "The VM deallocates itself when idle")
		}
	}

	m.State = byoc.Running
	m.UpdatedAt = p.opts.Clock.Now().UTC()
	if s, ok := sizeByName(spec.Size); ok && m.Preset == "" {
		m.Preset = s.Preset
	}
	if h, perr := p.vmHourly(ctx, spec.Region, spec.Size); perr == nil {
		m.HourlyUSD = h
	}
	return m, nil
}

func orNone(cidr string) string {
	if cidr == "" {
		return "nowhere"
	}
	return cidr
}

// hostname is the VM's computer name: letters, digits and hyphens, at most 64.
func hostname(instance string) string {
	var b strings.Builder
	for _, r := range "cloudburrow-" + instance {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := b.String()
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.TrimRight(s, "-")
}

// assignSelfDeallocate gives the VM's identity Virtual Machine Contributor on
// the VM. A new identity takes a moment to reach Entra ID's replicas, so
// PrincipalNotFound is retried a few times.
func (p *Provider) assignSelfDeallocate(ctx context.Context, vmID, principal string) error {
	params := armauthorization.RoleAssignmentCreateParameters{Properties: &armauthorization.RoleAssignmentProperties{
		PrincipalID:      to.Ptr(principal),
		PrincipalType:    to.Ptr(armauthorization.PrincipalTypeServicePrincipal),
		RoleDefinitionID: to.Ptr("/subscriptions/" + p.subscription + "/providers/Microsoft.Authorization/roleDefinitions/" + VirtualMachineContributor),
		Description:      to.Ptr("CloudBurrow: lets the VM deallocate itself after its idle time"),
	}}
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		_, err = p.roles.Create(ctx, vmID, newGUID(), params, nil)
		if err == nil || errorCode(err) != "PrincipalNotFound" {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.opts.Clock.After(p.opts.PollInterval):
		}
	}
	return err
}

func newGUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// machineRefs are the resource group and VM name of m.
func machineRefs(m byoc.Machine) (string, string) {
	return ResourceGroup(m.Instance), VMName(m.Instance)
}

// powerState maps the VM's instance view to byoc's states.
func powerState(view *armcompute.VirtualMachineInstanceView) byoc.State {
	if view == nil {
		return byoc.Unknown
	}
	state := byoc.Unknown
	for _, s := range view.Statuses {
		if s == nil || s.Code == nil {
			continue
		}
		code := strings.ToLower(*s.Code)
		switch code {
		case "provisioningstate/failed":
			return byoc.Failed
		case "provisioningstate/deleting":
			return byoc.Deleting
		case "provisioningstate/creating":
			state = byoc.Provisioning
		case "powerstate/running":
			state = byoc.Running
		case "powerstate/starting":
			state = byoc.Starting
		case "powerstate/stopping", "powerstate/deallocating":
			state = byoc.Stopping
		case "powerstate/stopped", "powerstate/deallocated":
			state = byoc.Stopped
		}
	}
	return state
}

// Status implements byoc.Provider.
func (p *Provider) Status(ctx context.Context, m byoc.Machine) (byoc.Machine, error) {
	rg, name := machineRefs(m)
	vm, err := p.vms.Get(ctx, rg, name, nil)
	if err != nil {
		if isNotFound(err) {
			m.State = byoc.Deleted
			return m, nil
		}
		return m, mapError("reading VM "+name, err)
	}
	if vm.Properties != nil && vm.Properties.HardwareProfile != nil && vm.Properties.HardwareProfile.VMSize != nil {
		m.Size = string(*vm.Properties.HardwareProfile.VMSize)
		if s, ok := sizeByName(m.Size); ok {
			m.Preset = s.Preset
		}
	}
	view, err := p.vms.InstanceView(ctx, rg, name, nil)
	if err != nil {
		return m, mapError("reading VM "+name+"'s state", err)
	}
	m.State = powerState(&view.VirtualMachineInstanceView)
	if pip, err := p.pips.Get(ctx, rg, pipName, nil); err == nil && pip.Properties != nil {
		m.PublicIP = str(pip.Properties.IPAddress)
	}
	m.UpdatedAt = p.opts.Clock.Now().UTC()
	return m, nil
}

// Start implements byoc.Provider.
func (p *Provider) Start(ctx context.Context, m byoc.Machine) (byoc.Machine, error) {
	rg, name := machineRefs(m)
	poller, err := p.vms.BeginStart(ctx, rg, name, nil)
	if err == nil {
		_, err = wait(ctx, p.opts, poller)
	}
	if err != nil {
		return m, mapError("starting VM "+name, err)
	}
	return p.Status(ctx, m)
}

// Stop implements byoc.Provider: a deallocation, so Azure stops billing the
// VM's compute; the disk and the static IP stay, and are billed.
func (p *Provider) Stop(ctx context.Context, m byoc.Machine) (byoc.Machine, error) {
	rg, name := machineRefs(m)
	poller, err := p.vms.BeginDeallocate(ctx, rg, name, nil)
	if err == nil {
		_, err = wait(ctx, p.opts, poller)
	}
	if err != nil {
		return m, mapError("stopping (deallocating) VM "+name, err)
	}
	return p.Status(ctx, m)
}

// Resize implements byoc.Provider. Azure resizes a running VM in place
// (restarting it) when the new size is on its current hardware cluster;
// otherwise it refuses, and the VM is deallocated, resized and started.
func (p *Provider) Resize(ctx context.Context, m byoc.Machine, size string, progress byoc.Progress) (byoc.Machine, error) {
	rg, name := machineRefs(m)
	before, err := p.Status(ctx, m)
	if err != nil {
		return m, err
	}
	wasRunning := before.State == byoc.Running || before.State == byoc.Starting
	update := func() error {
		poller, err := p.vms.BeginUpdate(ctx, rg, name, armcompute.VirtualMachineUpdate{Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(size))},
		}}, nil)
		if err == nil {
			_, err = wait(ctx, p.opts, poller)
		}
		return err
	}
	progress.Report("resize", byoc.StepRunning, "Resizing %s from %s to %s", name, before.Size, size)
	err = update()
	if err != nil && wasRunning {
		code := errorCode(err)
		if code == "OperationNotAllowed" || code == "AllocationFailed" || code == "ZonalAllocationFailed" {
			progress.Report("resize", byoc.StepRunning, "Azure cannot resize the running VM on its current hardware: deallocating it first")
			if _, serr := p.Stop(ctx, m); serr != nil {
				progress.Report("resize", byoc.StepFailed, "%v", serr)
				return m, serr
			}
			err = update()
			if err != nil {
				e := mapError("resizing VM "+name+" to "+size, err)
				progress.Report("resize", byoc.StepFailed, "%v; starting it again at %s", e, before.Size)
				_, _ = p.Start(ctx, m)
				return m, e
			}
			progress.Report("resize", byoc.StepRunning, "Starting %s at %s", name, size)
			if _, serr := p.Start(ctx, m); serr != nil {
				progress.Report("resize", byoc.StepFailed, "%v", serr)
				return m, serr
			}
		}
	}
	if err != nil {
		e := mapError("resizing VM "+name+" to "+size, err)
		progress.Report("resize", byoc.StepFailed, "%v", e)
		return m, e
	}
	after, err := p.Status(ctx, m)
	if err != nil {
		return m, err
	}
	if h, perr := p.vmHourly(ctx, after.Region, size); perr == nil {
		after.HourlyUSD = h
	}
	progress.Report("resize", byoc.StepDone, "%s is now %s", name, size)
	return after, nil
}

// tagFilter is ARM's $filter for the instance's tag.
func tagFilter(instance string) string {
	return fmt.Sprintf("tagName eq '%s' and tagValue eq '%s'", byoc.TagKey, strings.ReplaceAll(instance, "'", "''"))
}

// taggedGroups is every resource group with the instance's tag.
func (p *Provider) taggedGroups(ctx context.Context, instance string) ([]*armresources.ResourceGroup, error) {
	var out []*armresources.ResourceGroup
	pager := p.groups.NewListPager(&armresources.ResourceGroupsClientListOptions{Filter: to.Ptr(tagFilter(instance))})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapError("listing resource groups tagged "+byoc.TagKey+"="+instance, err)
		}
		out = append(out, page.Value...)
	}
	return out, nil
}

// ListResources implements byoc.Provider: each tagged resource group, every
// resource in it (an OS disk Azure made from the VM carries no tag of its
// own), and any tagged resource outside them.
func (p *Provider) ListResources(ctx context.Context, instance, _ string) ([]byoc.Resource, error) {
	groups, err := p.taggedGroups(ctx, instance)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []byoc.Resource
	add := func(r byoc.Resource) {
		k := strings.ToLower(r.ID)
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	for _, g := range groups {
		state := ""
		if g.Properties != nil {
			state = str(g.Properties.ProvisioningState)
		}
		add(byoc.Resource{Type: "Microsoft.Resources/resourceGroups", ID: str(g.ID), Name: str(g.Name), Region: str(g.Location), State: state})
		pager := p.res.NewListByResourceGroupPager(str(g.Name), nil)
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				if isNotFound(err) {
					break
				}
				return nil, mapError("listing resource group "+str(g.Name), err)
			}
			for _, r := range page.Value {
				add(genericResource(r))
			}
		}
	}
	pager := p.res.NewListPager(&armresources.ClientListOptions{Filter: to.Ptr(tagFilter(instance))})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapError("listing resources tagged "+byoc.TagKey+"="+instance, err)
		}
		for _, r := range page.Value {
			add(genericResource(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func genericResource(r *armresources.GenericResourceExpanded) byoc.Resource {
	if r == nil {
		return byoc.Resource{}
	}
	return byoc.Resource{Type: str(r.Type), ID: str(r.ID), Name: str(r.Name), Region: str(r.Location), State: str(r.ProvisioningState)}
}

// apiVersions are the versions a tagged resource outside the group is
// deleted with, by type.
var apiVersions = map[string]string{
	"microsoft.network":   "2025-01-01",
	"microsoft.compute":   "2025-04-01",
	"microsoft.resources": "2021-04-01",
}

// deleteGroup deletes a resource group and the role assignments inside it
// (the VM's own, which Azure does not remove with the VM).
func (p *Provider) deleteGroup(ctx context.Context, rg string) error {
	pager := p.roles.NewListForResourceGroupPager(rg, nil)
	prefix := strings.ToLower("/subscriptions/" + p.subscription + "/resourceGroups/" + rg + "/")
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			break // the group may already be gone; its assignments then are too
		}
		for _, ra := range page.Value {
			if ra == nil || ra.Properties == nil || !strings.HasPrefix(strings.ToLower(str(ra.Properties.Scope))+"/", prefix) {
				continue
			}
			if _, err := p.roles.DeleteByID(ctx, str(ra.ID), nil); err != nil && !isNotFound(err) {
				return mapError("removing the VM's role assignment", err)
			}
		}
	}
	poller, err := p.groups.BeginDelete(ctx, rg, nil)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return mapError("deleting resource group "+rg, err)
	}
	if _, err := wait(ctx, p.opts, poller); err != nil && !isNotFound(err) {
		return mapError("deleting resource group "+rg, err)
	}
	return nil
}

// Delete implements byoc.Provider.
func (p *Provider) Delete(ctx context.Context, instance, region string, progress byoc.Progress) error {
	groups, err := p.taggedGroups(ctx, instance)
	if err != nil {
		progress.Report("delete", byoc.StepFailed, "%v", err)
		return err
	}
	for _, g := range groups {
		name := str(g.Name)
		progress.Report("delete", byoc.StepRunning, "Deleting resource group %s and everything in it", name)
		if err := p.deleteGroup(ctx, name); err != nil {
			progress.Report("delete", byoc.StepFailed, "%v", err)
			return err
		}
		progress.Report("delete", byoc.StepDone, "Deleted resource group %s", name)
	}
	// Tagged resources outside the groups.
	rest, err := p.ListResources(ctx, instance, region)
	if err != nil {
		return err
	}
	for _, r := range rest {
		ns := strings.ToLower(strings.SplitN(r.Type, "/", 2)[0])
		v, ok := apiVersions[ns]
		if !ok || strings.EqualFold(r.Type, "Microsoft.Resources/resourceGroups") {
			continue
		}
		progress.Report("delete", byoc.StepRunning, "Deleting %s %s", r.Type, r.Name)
		poller, err := p.res.BeginDeleteByID(ctx, r.ID, v, nil)
		if err == nil {
			_, err = wait(ctx, p.opts, poller)
		}
		if err != nil && !isNotFound(err) {
			e := mapError("deleting "+r.ID, err)
			progress.Report("delete", byoc.StepFailed, "%v", e)
			return e
		}
	}
	progress.Report("verify", byoc.StepRunning, "Checking that nothing tagged %s=%s is left", byoc.TagKey, instance)
	left, err := p.ListResources(ctx, instance, region)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		e := &byoc.LeftoverError{Resources: left}
		progress.Report("verify", byoc.StepFailed, "%v", e)
		return e
	}
	progress.Report("verify", byoc.StepDone, "Nothing tagged %s=%s is left in subscription %s", byoc.TagKey, instance, p.subscription)
	return nil
}

// permissionsPath is ARM's Permissions - List for the subscription.
const permissionsAPIVersion = "2022-04-01"

type permissionSet struct {
	Actions    []string `json:"actions"`
	NotActions []string `json:"notActions"`
}

// need is every action CloudBurrow's Azure calls use, with why.
var need = []byoc.Permission{
	{Action: "Microsoft.Resources/subscriptions/resourceGroups/write", Why: "to create the instance's own resource group"},
	{Action: "Microsoft.Resources/subscriptions/resourceGroups/read", Why: "to find the instance's resource group"},
	{Action: "Microsoft.Resources/subscriptions/resourceGroups/delete", Why: "to delete the instance's resource group, and everything in it"},
	{Action: "Microsoft.Network/virtualNetworks/write", Why: "to create the instance's virtual network"},
	{Action: "Microsoft.Network/virtualNetworks/delete", Why: "to delete the virtual network"},
	{Action: "Microsoft.Network/virtualNetworks/subnets/join/action", Why: "to attach the VM's network interface to the subnet"},
	{Action: "Microsoft.Network/networkSecurityGroups/write", Why: "to create the firewall that lets SSH in from your address only"},
	{Action: "Microsoft.Network/networkSecurityGroups/delete", Why: "to delete the firewall"},
	{Action: "Microsoft.Network/publicIPAddresses/write", Why: "to give the VM an address CloudBurrow can reach"},
	{Action: "Microsoft.Network/publicIPAddresses/delete", Why: "to delete the address"},
	{Action: "Microsoft.Network/networkInterfaces/write", Why: "to create the VM's network interface"},
	{Action: "Microsoft.Network/networkInterfaces/delete", Why: "to delete the network interface"},
	{Action: "Microsoft.Compute/virtualMachines/write", Why: "to create and resize the VM"},
	{Action: "Microsoft.Compute/virtualMachines/read", Why: "to read the VM's state and size"},
	{Action: "Microsoft.Compute/virtualMachines/delete", Why: "to delete the VM"},
	{Action: "Microsoft.Compute/virtualMachines/start/action", Why: "to start the VM"},
	{Action: "Microsoft.Compute/virtualMachines/deallocate/action", Why: "to stop the VM so Azure stops billing its compute"},
	{Action: "Microsoft.Compute/disks/delete", Why: "to delete the VM's disk"},
	{Action: "Microsoft.Authorization/roleAssignments/write", Why: "for auto-stop: without it the idle VM only shuts its OS down, which Azure keeps billing"},
	{Action: "Microsoft.Resources/tags/read", Why: "to list what CloudBurrow created, by its tag"},
}

// CheckPermissions implements byoc.Provider, with Azure's Permissions - List
// for the subscription: the actions and not-actions of every role the
// signed-in user holds there.
func (p *Provider) CheckPermissions(ctx context.Context, _ string) (byoc.PermissionReport, error) {
	u := runtime.JoinPaths(p.raw.Endpoint(), "/subscriptions/"+p.subscription+"/providers/Microsoft.Authorization/permissions")
	var sets []permissionSet
	for u != "" {
		req, err := runtime.NewRequest(ctx, http.MethodGet, u)
		if err != nil {
			return byoc.PermissionReport{}, err
		}
		if !strings.Contains(u, "api-version=") {
			q := req.Raw().URL.Query()
			q.Set("api-version", permissionsAPIVersion)
			req.Raw().URL.RawQuery = q.Encode()
		}
		resp, err := p.raw.Pipeline().Do(req)
		if err != nil {
			return byoc.PermissionReport{}, mapError("checking permissions", err)
		}
		if !runtime.HasStatusCode(resp, http.StatusOK) {
			rerr := runtime.NewResponseError(resp)
			if resp.StatusCode == http.StatusForbidden {
				return byoc.PermissionReport{Checked: false,
					Note: "Azure would not list the signed-in account's permissions (" + mapError("listing permissions", rerr).Error() + "); Create will say what is missing if anything is"}, nil
			}
			return byoc.PermissionReport{}, mapError("checking permissions", rerr)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return byoc.PermissionReport{}, err
		}
		var page struct {
			Value    []permissionSet `json:"value"`
			NextLink string          `json:"nextLink"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return byoc.PermissionReport{}, fmt.Errorf("checking permissions: %w", err)
		}
		sets = append(sets, page.Value...)
		u = page.NextLink
	}
	report := byoc.PermissionReport{Checked: true}
	for _, n := range need {
		if !allowed(sets, n.Action) {
			report.Missing = append(report.Missing, n)
		}
	}
	return report, nil
}

// allowed reports whether one of the role permission sets grants action:
// one of its actions matches and none of its not-actions does.
func allowed(sets []permissionSet, action string) bool {
	for _, s := range sets {
		match := false
		for _, a := range s.Actions {
			if wildcard(a, action) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		denied := false
		for _, a := range s.NotActions {
			if wildcard(a, action) {
				denied = true
				break
			}
		}
		if !denied {
			return true
		}
	}
	return false
}

// wildcard matches Azure RBAC action patterns: case-insensitive, with *
// matching any run of characters.
func wildcard(pattern, action string) bool {
	return glob(strings.ToLower(pattern), strings.ToLower(action))
}

func glob(p, s string) bool {
	for p != "" {
		if p[0] == '*' {
			for p != "" && p[0] == '*' {
				p = p[1:]
			}
			if p == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if glob(p, s[i:]) {
					return true
				}
			}
			return false
		}
		if s == "" || p[0] != s[0] {
			return false
		}
		p, s = p[1:], s[1:]
	}
	return s == ""
}
