package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/filestore"
	betapb "github.com/cloudburrow/cloudburrow/internal/service/filestore/filestorev1beta1pb"
)

// The console's Filestore pages (#2192), Google's menu in its order:
// Instances, Backups, Snapshots. Instances: a project's instances in
// every location (service tier, location, IP address, file share,
// capacity, protocol, labels); Create instance with Google's fields
// (instance ID, description, instance type and storage type, capacity,
// region and zone, protocol, VPC network and allocated IP range, file
// share name, access controls, labels, deletion protection); an
// instance's page with its Overview, Snapshots and Backups, and Edit,
// Create snapshot, Revert, Create backup, Restore and Delete. Backups:
// Create backup, a backup's page, Create instance from it (Restore),
// Edit and Delete. Snapshots: every instance's snapshots, Edit and
// Delete. Console names are LOCATION/ID.

const filestoreNotStarted = "Filestore has not started"

var filestoreTierLabels = map[string]string{
	"BASIC_HDD": "Basic HDD", "BASIC_SSD": "Basic SSD", "ZONAL": "Zonal", "REGIONAL": "Regional", "ENTERPRISE": "Enterprise",
	"STANDARD": "Basic HDD (Standard)", "PREMIUM": "Basic SSD (Premium)", "HIGH_SCALE_SSD": "High Scale SSD",
}

var filestoreProtocolLabels = map[string]string{"NFS_V3": "NFSv3", "NFS_V4_1": "NFSv4.1"}

func filestoreAPI(svc *filestoreService) (*filestore.Server, error) {
	s := svc.API()
	if s == nil {
		return nil, errors.New(filestoreNotStarted)
	}
	return s, nil
}

// filestoreConsoleName is LOCATION/ID of a resource name.
func filestoreConsoleName(name string) string {
	p := strings.Split(name, "/")
	if len(p) >= 6 {
		return p[3] + "/" + p[5]
	}
	return name
}

// filestoreResourceName is the resource name of a console name in project.
func filestoreResourceName(project, coll, name string) (string, error) {
	loc, id, ok := strings.Cut(name, "/")
	if !ok || loc == "" || id == "" || strings.Contains(id, "/") {
		return "", fmt.Errorf("no such %s: %s", strings.TrimSuffix(coll, "s"), name)
	}
	return "projects/" + project + "/locations/" + loc + "/" + coll + "/" + id, nil
}

// filestoreWait waits for an operation and answers its error.
func filestoreWait(ctx context.Context, s *filestore.Server, op *longrunningpb.Operation, err error) error {
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	s.WaitDone(ctx, op.GetName())
	got, err := s.Operation(op.GetName())
	if err != nil {
		return err
	}
	if !got.GetDone() {
		return fmt.Errorf("the operation %s is still running; refresh to see it finish", op.GetName())
	}
	if e := got.GetError(); e != nil {
		return errors.New(e.GetMessage())
	}
	return nil
}

func filestoreCapacity(gb int64) string {
	if gb >= 1024 && gb%256 == 0 {
		return strconv.FormatFloat(float64(gb)/1024, 'f', -1, 64) + " TiB"
	}
	return strconv.FormatInt(gb, 10) + " GiB"
}

func filestoreState(s string) string {
	if s == "" {
		return "—"
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(strings.ReplaceAll(s[1:], "_", " "))
}

// filestoreProvider is the Instances page.
type filestoreProvider struct{ svc *filestoreService }

func (filestoreProvider) ID() string    { return "filestore" }
func (filestoreProvider) Title() string { return "Instances" }

func (p filestoreProvider) List(_ context.Context, project string) (console.Listing, error) {
	s, err := filestoreAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	l := console.Listing{Columns: []string{"Status", "Service tier", "Location", "IP address", "File share name", "Capacity", "Protocol", "Labels", "Created"},
		NameColumn: "Instance ID", Noun: "instances"}
	if project == "" {
		l.Prompt = "Filestore lists instances per project. Choose one in the toolbar."
		return l, nil
	}
	for _, in := range s.Instances() {
		if !strings.HasPrefix(in.GetName(), "projects/"+project+"/") {
			continue
		}
		share, capacity := "—", int64(0)
		if fss := in.GetFileShares(); len(fss) > 0 {
			share, capacity = fss[0].GetName(), fss[0].GetCapacityGb()
		} else if c, _, shares, ok := s.Multishare(in.GetName()); ok {
			share, capacity = fmt.Sprintf("Multishare (%d shares)", len(shares)), c
		}
		ip := "—"
		if n := in.GetNetworks(); len(n) > 0 && len(n[0].GetIpAddresses()) > 0 {
			ip = n[0].GetIpAddresses()[0]
		}
		l.Items = append(l.Items, console.Resource{Name: filestoreConsoleName(in.GetName()), Status: filestoreState(in.GetState().String()),
			Fields: map[string]string{"Status": filestoreState(in.GetState().String()), "Service tier": filestoreTierLabels[in.GetTier().String()],
				"Location": strings.Split(in.GetName(), "/")[3], "IP address": ip, "File share name": share,
				"Capacity": filestoreCapacity(capacity), "Protocol": filestoreProtocolLabels[in.GetProtocol().String()],
				"Labels": kmsOrDash(labelsText(in.GetLabels())), "Created": kmsTime(in.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

// CreateForm is Google's Create an instance page, in its order.
func (filestoreProvider) CreateForm() (string, []console.Field) {
	return "Create instance", filestoreInstanceFields(false)
}

func filestoreInstanceFields(fromBackup bool) []console.Field {
	tiers := []string{"BASIC_HDD", "BASIC_SSD", "ZONAL", "REGIONAL", "ENTERPRISE"}
	f := []console.Field{
		{Name: "instanceId", Label: "Instance ID", Type: "text", Required: true, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`,
			Help: "1 to 63 lowercase letters, digits and dashes, starting with a letter and ending with a letter or digit. It cannot be changed later."},
		{Name: "description", Label: "Description", Type: "text", Help: "Optional."},
		{Name: "tier", Label: "Instance type", Type: "select", Default: "BASIC_HDD", Options: tiers, OptionLabels: filestoreTierLabels, Section: "Instance type",
			Help: "Basic HDD and Basic SSD are zonal, NFSv3, 1 TiB (2.5 TiB for SSD) to 63.9 TiB, scaled up only, with backups and no snapshots. Zonal is zonal; Regional and Enterprise are regional; these take NFSv4.1, snapshots and revert."},
		{Name: "capacityGb", Label: "Capacity (GiB)", Type: "number", Required: true, Default: "1024", Pattern: `^[0-9]{3,6}$`, Section: "Allocate capacity",
			Help: "Basic HDD 1024 to 65434 GiB; Basic SSD 2560 to 65434 GiB, in 1 GiB steps. Zonal 1024 to 9984 GiB in 256 GiB steps or 10240 to 102400 GiB in 2560 GiB steps; Regional 100 to 10239 GiB in 1 GiB steps or 10240 to 102400 in 2560 GiB steps; Enterprise 1024 to 10240 GiB in 256 GiB steps."},
		{Name: "multiShare", Label: "Enable multishares", Type: "checkbox", Section: "Allocate capacity", ShowWhen: "tier=ENTERPRISE",
			Help: "Several file shares (10 GiB to 1 TiB each) on one instance, as GKE's enterprise-multishare-rwx class provisions them; create the shares on the instance's page."},
		{Name: "region", Label: "Region", Type: "text", Required: true, Default: "us-central1", Section: "Choose where to store your data"},
		{Name: "zone", Label: "Zone", Type: "text", Default: "us-central1-a", Section: "Choose where to store your data", ShowWhen: "tier=BASIC_HDD|BASIC_SSD|ZONAL",
			Help: "Basic and Zonal instances are in a zone of the region."},
		{Name: "protocol", Label: "Protocol", Type: "radio", Default: "NFS_V3", Options: []string{"NFS_V3", "NFS_V4_1"}, OptionLabels: filestoreProtocolLabels,
			Section: "Protocol", ShowWhen: "tier=ZONAL|REGIONAL|ENTERPRISE", Help: "It cannot be changed later. Basic tiers serve NFSv3."},
		{Name: "network", Label: "VPC network", Type: "text", Required: true, Default: "default", Section: "Set up connections",
			Help: "The instance's VPC network. Every VM and Kubernetes Engine pod of this CloudBurrow instance reaches the NFS server at its IP address."},
		{Name: "reservedIpRange", Label: "Allocated IP range", Type: "text", Section: "Set up connections", Pattern: `^([0-9]{1,3}(\.[0-9]{1,3}){3}\/[0-9]{1,2})?$`,
			Help: "Optional: a /29 for Basic tiers, a /26 for the others. Unset, an unused range is chosen."},
		{Name: "shareName", Label: "File share name", Type: "text", Required: true, Default: "vol1", Section: "Configure your file share",
			Pattern: `^[A-Za-z]([A-Za-z0-9_]{0,30}[A-Za-z0-9])?$`, Help: "Letters, numbers and underscores, starting with a letter: at most 16 characters for Basic tiers, 32 for the others. Clients mount IP_ADDRESS:/SHARE."},
		{Name: "access", Label: "Access controls", Type: "radio", Default: "all", Section: "Configure your file share", Options: []string{"all", "restrict"},
			OptionLabels: map[string]string{"all": "Grant access to all clients on the VPC network", "restrict": "Restrict access by IP address or range"}},
		{Name: "ipRanges", Label: "IP address or range", Type: "text", Section: "Configure your file share", ShowWhen: "access=restrict",
			Help: "Comma-separated IPv4 addresses or CIDR ranges. VMs reach the server from their internal IP, Kubernetes Engine pods from their pod address and nodes (PersistentVolume mounts) from their InternalIP."},
		{Name: "accessMode", Label: "Access level", Type: "select", Default: "READ_WRITE", Section: "Configure your file share", ShowWhen: "access=restrict",
			Options: []string{"READ_WRITE", "READ_ONLY"}, OptionLabels: map[string]string{"READ_WRITE": "Admin (read-write)", "READ_ONLY": "Viewer (read-only)"}},
		{Name: "squashMode", Label: "Root squash", Type: "select", Default: "NO_ROOT_SQUASH", Section: "Configure your file share", ShowWhen: "access=restrict",
			Options: []string{"NO_ROOT_SQUASH", "ROOT_SQUASH"}, OptionLabels: map[string]string{"NO_ROOT_SQUASH": "No root squash", "ROOT_SQUASH": "Root squash"}},
		{Name: "encryption", Label: "Encryption", Type: "radio", Default: "google", Section: "Encryption", ShowWhen: "tier=ZONAL|REGIONAL|ENTERPRISE",
			Options: []string{"google", "cmek"}, OptionLabels: map[string]string{"google": "Google-managed encryption key", "cmek": "Cloud KMS key"}},
		{Name: "kmsKey", Label: "Cloud KMS key", Type: "text", Section: "Encryption", ShowWhen: "encryption=cmek",
			Help: "projects/PROJECT/locations/REGION/keyRings/RING/cryptoKeys/KEY, in the instance's region; the Filestore service agent needs Cloud KMS CryptoKey Encrypter/Decrypter on it."},
		{Name: "labels", Label: "Labels", Type: "map", Section: "Labels"},
		{Name: "tags", Label: "Tags", Type: "map", Section: "Tags", Help: "Tag key namespaced names (ORG_ID/KEY or PROJECT_ID/KEY) and value short names; bound to the instance in Resource Manager."},
		{Name: "deletionProtection", Label: "Enable deletion protection", Type: "checkbox", Section: "Deletion protection"},
	}
	if fromBackup {
		f[0].Default = ""
	}
	return f
}

// CreateTitle is Google's page title.
func (filestoreProvider) CreateTitle() string { return "Create an instance" }

// CreateSubmit is the page's button.
func (filestoreProvider) CreateSubmit() string { return "Create" }

// CreateLayout is one page, as Google's.
func (filestoreProvider) CreateLayout() string { return console.CreateLayoutPage }

// CreateOnPage gives the form its page.
func (filestoreProvider) CreateOnPage() bool { return true }

func (p filestoreProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	return filestoreCreate(ctx, p.svc, project, values, "")
}

// filestoreCreate creates an instance from the form, restored from backup
// when one is named, and waits for it.
func filestoreCreate(ctx context.Context, svc *filestoreService, project string, values map[string]string, backup string) (string, error) {
	s, err := filestoreAPI(svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating an instance")
	}
	capacity, err := strconv.ParseInt(strings.TrimSpace(values["capacityGb"]), 10, 64)
	if err != nil {
		return "", fmt.Errorf("capacity %q is not a number of GiB", values["capacityGb"])
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", err
	}
	tier := filestorepb.Instance_Tier(filestorepb.Instance_Tier_value[values["tier"]])
	loc := strings.TrimSpace(values["region"])
	if tier != filestorepb.Instance_REGIONAL && tier != filestorepb.Instance_ENTERPRISE {
		loc = strings.TrimSpace(values["zone"])
	}
	fs := &filestorepb.FileShareConfig{Name: strings.TrimSpace(values["shareName"]), CapacityGb: capacity}
	if backup != "" {
		fs.Source = &filestorepb.FileShareConfig_SourceBackup{SourceBackup: backup}
	}
	if values["access"] == "restrict" {
		var ranges []string
		for _, r := range strings.Split(values["ipRanges"], ",") {
			if r = strings.TrimSpace(r); r != "" {
				ranges = append(ranges, r)
			}
		}
		fs.NfsExportOptions = []*filestorepb.NfsExportOptions{{IpRanges: ranges,
			AccessMode: filestorepb.NfsExportOptions_AccessMode(filestorepb.NfsExportOptions_AccessMode_value[values["accessMode"]]),
			SquashMode: filestorepb.NfsExportOptions_SquashMode(filestorepb.NfsExportOptions_SquashMode_value[values["squashMode"]])}}
	}
	tags, err := console.ParseMap(values["tags"])
	if err != nil {
		return "", err
	}
	in := &filestorepb.Instance{Description: values["description"], Tier: tier, Labels: labels, Tags: tags, FileShares: []*filestorepb.FileShareConfig{fs},
		Networks:                  []*filestorepb.NetworkConfig{{Network: strings.TrimSpace(values["network"]), ReservedIpRange: strings.TrimSpace(values["reservedIpRange"])}},
		DeletionProtectionEnabled: values["deletionProtection"] == "true"}
	if values["encryption"] == "cmek" {
		in.KmsKeyName = strings.TrimSpace(values["kmsKey"])
	}
	if tier != filestorepb.Instance_BASIC_HDD && tier != filestorepb.Instance_BASIC_SSD {
		in.Protocol = filestorepb.Instance_FileProtocol(filestorepb.Instance_FileProtocol_value[values["protocol"]])
	}
	var op *longrunningpb.Operation
	if tier == filestorepb.Instance_ENTERPRISE && values["multiShare"] == "true" && backup == "" {
		beta := &betapb.Instance{Description: in.Description, Tier: betapb.Instance_ENTERPRISE, Labels: labels, Tags: tags,
			MultiShareEnabled: true, CapacityGb: capacity, MaxShareCount: 10, KmsKeyName: in.KmsKeyName,
			Protocol:                  betapb.Instance_FileProtocol(in.Protocol),
			Networks:                  []*betapb.NetworkConfig{{Network: in.Networks[0].Network, ReservedIpRange: in.Networks[0].ReservedIpRange}},
			DeletionProtectionEnabled: in.DeletionProtectionEnabled}
		op, err = s.Beta().CreateInstance(ctx, &betapb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/" + loc,
			InstanceId: strings.TrimSpace(values["instanceId"]), Instance: beta})
	} else {
		op, err = s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/" + loc,
			InstanceId: strings.TrimSpace(values["instanceId"]), Instance: in})
	}
	if err := filestoreWait(ctx, s, op, err); err != nil {
		return "", err
	}
	return loc + "/" + strings.TrimSpace(values["instanceId"]), nil
}

func (p filestoreProvider) instance(ctx context.Context, project, name string) (*filestore.Server, *filestorepb.Instance, error) {
	s, err := filestoreAPI(p.svc)
	if err != nil {
		return nil, nil, err
	}
	rn, err := filestoreResourceName(project, "instances", name)
	if err != nil {
		return nil, nil, err
	}
	in, err := s.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: rn})
	return s, in, err
}

func (p filestoreProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	s, in, err := p.instance(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the instance: " + consoleErr(err)}, nil
	}
	multiCap, maxShares, shares, multi := s.Multishare(in.GetName())
	fs := &filestorepb.FileShareConfig{}
	if len(in.GetFileShares()) > 0 {
		fs = in.GetFileShares()[0]
	}
	net := in.GetNetworks()[0]
	ip := "—"
	if len(net.GetIpAddresses()) > 0 {
		ip = net.GetIpAddresses()[0]
	}
	mount := "—"
	if ip != "—" {
		mount = "sudo mount -o rw,intr " + ip + ":/" + fs.GetName() + " /mnt/" + strings.ToLower(fs.GetName())
	}
	used := "—"
	if n, err := s.Usage(ctx, in.GetName()); err == nil {
		used = strconv.FormatInt(n, 10) + " bytes"
	}
	access := "All clients on the VPC network, read-write, no root squash"
	if len(fs.GetNfsExportOptions()) > 0 {
		var parts []string
		for _, o := range fs.GetNfsExportOptions() {
			parts = append(parts, strings.Join(o.GetIpRanges(), ", ")+": "+o.GetAccessMode().String()+", "+o.GetSquashMode().String())
		}
		access = strings.Join(parts, "; ")
	}
	snaps := console.Listing{Columns: []string{"State", "Created", "Filesystem used", "Description"}, NameColumn: "Snapshot", Noun: "snapshots"}
	for _, sn := range s.Snapshots(in.GetName()) {
		snaps.Items = append(snaps.Items, console.Resource{Name: lastSegment(sn.GetName()), Status: filestoreState(sn.GetState().String()),
			Fields: map[string]string{"State": filestoreState(sn.GetState().String()), "Created": kmsTime(sn.GetCreateTime()),
				"Filesystem used": strconv.FormatInt(sn.GetFilesystemUsedBytes(), 10) + " bytes", "Description": kmsOrDash(sn.GetDescription())}})
	}
	snaps.Total = len(snaps.Items)
	backups := console.Listing{Columns: []string{"State", "Location", "Created", "Storage bytes"}, NameColumn: "Backup", Noun: "backups"}
	for _, b := range s.Backups() {
		if b.GetSourceInstance() == in.GetName() {
			backups.Items = append(backups.Items, console.Resource{Name: filestoreConsoleName(b.GetName()), Status: filestoreState(b.GetState().String()),
				Fields: map[string]string{"State": filestoreState(b.GetState().String()), "Location": strings.Split(b.GetName(), "/")[3],
					"Created": kmsTime(b.GetCreateTime()), "Storage bytes": strconv.FormatInt(b.GetStorageBytes(), 10)}})
		}
	}
	backups.Total = len(backups.Items)
	overview := []console.PropertyGroup{
		{Heading: "Instance", Properties: []console.Property{{Label: "Instance ID", Value: lastSegment(in.GetName())},
			{Label: "Status", Value: filestoreState(in.GetState().String())}, {Label: "Status message", Value: kmsOrDash(in.GetStatusMessage())},
			{Label: "Description", Value: kmsOrDash(in.GetDescription())},
			{Label: "Service tier", Value: filestoreTierLabels[in.GetTier().String()]}, {Label: "Location", Value: strings.Split(in.GetName(), "/")[3]},
			{Label: "Protocol", Value: filestoreProtocolLabels[in.GetProtocol().String()]}, {Label: "Created", Value: kmsTime(in.GetCreateTime())},
			{Label: "Deletion protection", Value: map[bool]string{true: "Enabled", false: "Disabled"}[in.GetDeletionProtectionEnabled()]},
			{Label: "Encryption", Value: map[bool]string{true: "Cloud KMS key " + in.GetKmsKeyName(), false: "Google-managed"}[in.GetKmsKeyName() != ""]},
			{Label: "Labels", Value: kmsOrDash(labelsText(in.GetLabels()))}}},
		filestoreShareGroup(multi, fs, multiCap, maxShares, len(shares), used, access, mount),
		filestorePerformanceGroup(in),
		{Heading: "Networking", Properties: []console.Property{{Label: "VPC network", Value: net.GetNetwork()},
			{Label: "IP address", Value: ip}, {Label: "Reserved IP range", Value: net.GetReservedIpRange()},
			{Label: "Connect mode", Value: net.GetConnectMode().String()}}},
	}
	if rp := in.GetReplication(); rp != nil {
		props := []console.Property{{Label: "Role", Value: map[bool]string{true: "Replica (standby)", false: "Active"}[isStandbyRole(rp)]}}
		for _, r := range rp.GetReplicas() {
			props = append(props, console.Property{Label: "Peer instance", Value: filestoreConsoleName(r.GetPeerInstance())},
				console.Property{Label: "Replication state", Value: filestoreState(r.GetState().String())},
				console.Property{Label: "Last active sync", Value: kmsTime(r.GetLastActiveSyncTime())})
		}
		overview = append(overview, console.PropertyGroup{Heading: "Replication", Properties: props})
	}
	sections := []console.Section{{ID: "overview", Label: "Overview", Kind: console.KindProperties, Groups: overview}}
	if multi {
		l := console.Listing{Columns: []string{"State", "Mount name", "Capacity", "Mount command", "Labels", "Created"}, NameColumn: "Share", Noun: "shares"}
		for _, sh := range shares {
			l.Items = append(l.Items, console.Resource{Name: lastSegment(sh.GetName()), Status: filestoreState(sh.GetState().String()),
				Fields: map[string]string{"State": filestoreState(sh.GetState().String()), "Mount name": sh.GetMountName(),
					"Capacity": filestoreCapacity(sh.GetCapacityGb()), "Mount command": "sudo mount -o rw,intr " + ip + ":/" + sh.GetMountName() + " /mnt/" + strings.ToLower(sh.GetMountName()),
					"Labels": kmsOrDash(labelsText(sh.GetLabels())), "Created": kmsTime(sh.GetCreateTime())}})
		}
		l.Total = len(l.Items)
		sections = append(sections, console.Section{ID: "shares", Label: "Shares", Listing: l})
		return console.Detail{Summary: []console.Property{{Label: "Status", Value: filestoreState(in.GetState().String())}, {Label: "IP address", Value: ip}},
			Sections: sections}, nil
	}
	if !strings.HasPrefix(in.GetTier().String(), "BASIC") && in.GetTier() != filestorepb.Instance_STANDARD && in.GetTier() != filestorepb.Instance_PREMIUM {
		sections = append(sections, console.Section{ID: "snapshots", Label: "Snapshots", Listing: snaps})
	}
	sections = append(sections, console.Section{ID: "backups", Label: "Backups", Listing: backups})
	return console.Detail{Summary: []console.Property{{Label: "Status", Value: filestoreState(in.GetState().String())}, {Label: "IP address", Value: ip}},
		Sections: sections}, nil
}

func (p filestoreProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	s, in, err := p.instance(ctx, project, path[0])
	if err != nil {
		return nil
	}
	if multiCap, _, shares, ok := s.Multishare(in.GetName()); ok {
		return filestoreMultishareActions(in, multiCap, shares)
	}
	fs := in.GetFileShares()[0]
	basic := in.GetTier() == filestorepb.Instance_BASIC_HDD || in.GetTier() == filestorepb.Instance_BASIC_SSD ||
		in.GetTier() == filestorepb.Instance_STANDARD || in.GetTier() == filestorepb.Instance_PREMIUM
	region := strings.Split(in.GetName(), "/")[3]
	if i := strings.LastIndex(region, "-"); i > 0 && strings.Count(region, "-") == 2 {
		region = region[:i]
	}
	acts := []console.Action{
		{ID: "edit", Label: "Edit", Title: "Edit instance", Submit: "Save", Page: true, Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: in.GetDescription()},
			{Name: "capacityGb", Label: "Capacity (GiB)", Type: "number", Required: true, Default: strconv.FormatInt(fs.GetCapacityGb(), 10),
				Help: "Basic tiers can only grow; the other tiers stay within the capacity range the instance was created in."},
			{Name: "labels", Label: "Labels", Type: "map", Default: console.FormatMap(in.GetLabels())},
			{Name: "deletionProtection", Label: "Enable deletion protection", Type: "checkbox", Default: strconv.FormatBool(in.GetDeletionProtectionEnabled())}}},
	}
	if !basic {
		var snapIDs []string
		for _, sn := range s.Snapshots(in.GetName()) {
			if sn.GetState() == filestorepb.Snapshot_READY {
				snapIDs = append(snapIDs, lastSegment(sn.GetName()))
			}
		}
		acts = append(acts, console.Action{ID: "snapshot", Label: "Create snapshot", Title: "Create a snapshot", Submit: "Create", Fields: []console.Field{
			{Name: "snapshotId", Label: "Snapshot ID", Type: "text", Required: true, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`},
			{Name: "description", Label: "Description", Type: "text"}}})
		revert := console.Action{ID: "revert", Label: "Revert", Title: "Revert instance", Submit: "Revert", Destructive: true,
			Confirm: "The file share's data is replaced by the snapshot's, and every snapshot taken after it is deleted. This cannot be undone."}
		if len(snapIDs) == 0 {
			revert.Unavailable = "The instance has no snapshot to revert to."
		} else {
			revert.Fields = []console.Field{{Name: "snapshotId", Label: "Snapshot", Type: "select", Required: true, Options: snapIDs, Default: snapIDs[len(snapIDs)-1]}}
		}
		acts = append(acts, revert)
	}
	acts = append(acts, console.Action{ID: "backup", Label: "Create backup", Title: "Create a backup", Submit: "Create", Fields: []console.Field{
		{Name: "backupId", Label: "Backup ID", Type: "text", Required: true, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`},
		{Name: "region", Label: "Region", Type: "text", Required: true, Default: region, Help: "Backups are regional."},
		{Name: "description", Label: "Description", Type: "text"}}})
	if basic {
		var ids []string
		for _, b := range s.Backups() {
			if strings.HasPrefix(b.GetName(), "projects/"+project+"/") && b.GetState() == filestorepb.Backup_READY && strings.HasPrefix(b.GetSourceInstanceTier().String(), "BASIC") {
				ids = append(ids, filestoreConsoleName(b.GetName()))
			}
		}
		restore := console.Action{ID: "restore", Label: "Restore", Title: "Restore from backup", Submit: "Restore", Destructive: true,
			Confirm: "The file share's data is replaced by the backup's."}
		if len(ids) == 0 {
			restore.Unavailable = "The project has no basic tier backup to restore."
		} else {
			restore.Fields = []console.Field{{Name: "backup", Label: "Backup", Type: "select", Required: true, Options: ids, Default: ids[0]}}
		}
		acts = append(acts, restore)
	}
	if rp := in.GetReplication(); rp != nil {
		acts = append(acts, console.Action{ID: "promote", Label: "Promote replica", Title: "Promote replica", Submit: "Promote", Destructive: true,
			Confirm: "Replication stops: the replica becomes an independent read-write instance with the data of the last replicated sync."})
	} else if in.GetTier() == filestorepb.Instance_REGIONAL || in.GetTier() == filestorepb.Instance_ENTERPRISE {
		acts = append(acts, console.Action{ID: "replica", Label: "Create replica", Title: "Create a replica", Submit: "Create", Fields: []console.Field{
			{Name: "instanceId", Label: "Replica instance ID", Type: "text", Required: true, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`},
			{Name: "region", Label: "Region", Type: "text", Required: true, Help: "The replica's region, other than the instance's."}}})
	}
	del := console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, ConfirmWord: lastSegment(in.GetName()),
		Confirm: "The instance and its file share's data are deleted. Backups are kept.",
		Fields:  []console.Field{{Name: "force", Label: "Also delete the instance's snapshots", Type: "checkbox"}}}
	if in.GetDeletionProtectionEnabled() {
		del.Unavailable = "Deletion protection is enabled: disable it with Edit first."
	}
	return append(acts, del)
}

func (p filestoreProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("no instance")
	}
	s, in, err := p.instance(ctx, project, path[0])
	if err != nil {
		return err
	}
	if _, _, _, ok := s.Multishare(in.GetName()); ok && action != "delete" {
		return filestoreMultishareAct(ctx, s, in, action, values)
	}
	switch action {
	case "edit":
		capacity, err := strconv.ParseInt(strings.TrimSpace(values["capacityGb"]), 10, 64)
		if err != nil {
			return fmt.Errorf("capacity %q is not a number of GiB", values["capacityGb"])
		}
		labels, err := console.ParseMap(values["labels"])
		if err != nil {
			return err
		}
		fs := in.GetFileShares()[0]
		fs.CapacityGb = capacity
		op, err := s.UpdateInstance(ctx, &filestorepb.UpdateInstanceRequest{Instance: &filestorepb.Instance{Name: in.GetName(),
			Description: values["description"], Labels: labels, FileShares: []*filestorepb.FileShareConfig{fs},
			DeletionProtectionEnabled: values["deletionProtection"] == "true"},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description", "labels", "file_shares", "deletion_protection_enabled"}}})
		return filestoreWait(ctx, s, op, err)
	case "snapshot":
		op, err := s.CreateSnapshot(ctx, &filestorepb.CreateSnapshotRequest{Parent: in.GetName(), SnapshotId: strings.TrimSpace(values["snapshotId"]),
			Snapshot: &filestorepb.Snapshot{Description: values["description"]}})
		return filestoreWait(ctx, s, op, err)
	case "revert":
		op, err := s.RevertInstance(ctx, &filestorepb.RevertInstanceRequest{Name: in.GetName(), TargetSnapshotId: values["snapshotId"]})
		return filestoreWait(ctx, s, op, err)
	case "backup":
		op, err := s.CreateBackup(ctx, &filestorepb.CreateBackupRequest{Parent: "projects/" + project + "/locations/" + strings.TrimSpace(values["region"]),
			BackupId: strings.TrimSpace(values["backupId"]), Backup: &filestorepb.Backup{Description: values["description"],
				SourceInstance: in.GetName(), SourceFileShare: in.GetFileShares()[0].GetName()}})
		return filestoreWait(ctx, s, op, err)
	case "restore":
		b, err := filestoreResourceName(project, "backups", values["backup"])
		if err != nil {
			return err
		}
		op, err := s.RestoreInstance(ctx, &filestorepb.RestoreInstanceRequest{Name: in.GetName(), FileShare: in.GetFileShares()[0].GetName(),
			Source: &filestorepb.RestoreInstanceRequest_SourceBackup{SourceBackup: b}})
		return filestoreWait(ctx, s, op, err)
	case "replica":
		rep := &filestorepb.Instance{Tier: in.GetTier(), Protocol: in.GetProtocol(), Networks: []*filestorepb.NetworkConfig{{Network: in.GetNetworks()[0].GetNetwork()}},
			FileShares:  []*filestorepb.FileShareConfig{{Name: in.GetFileShares()[0].GetName(), CapacityGb: in.GetFileShares()[0].GetCapacityGb()}},
			Replication: &filestorepb.Replication{Role: filestorepb.Replication_STANDBY, Replicas: []*filestorepb.ReplicaConfig{{PeerInstance: in.GetName()}}}}
		op, err := s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/" + strings.TrimSpace(values["region"]),
			InstanceId: strings.TrimSpace(values["instanceId"]), Instance: rep})
		return filestoreWait(ctx, s, op, err)
	case "promote":
		req := &filestorepb.PromoteReplicaRequest{Name: in.GetName()}
		if !isStandbyRole(in.GetReplication()) && len(in.GetReplication().GetReplicas()) > 0 {
			req.PeerInstance = in.GetReplication().GetReplicas()[0].GetPeerInstance()
		}
		op, err := s.PromoteReplica(ctx, req)
		return filestoreWait(ctx, s, op, err)
	case "delete":
		op, err := s.DeleteInstance(ctx, &filestorepb.DeleteInstanceRequest{Name: in.GetName(), Force: values["force"] == "true"})
		return filestoreWait(ctx, s, op, err)
	}
	return fmt.Errorf("unknown action %q", action)
}

// filestoreBackupsProvider is the Backups page.
type filestoreBackupsProvider struct{ svc *filestoreService }

func (filestoreBackupsProvider) ID() string    { return "filestore-backups" }
func (filestoreBackupsProvider) Title() string { return "Backups" }

func (p filestoreBackupsProvider) List(_ context.Context, project string) (console.Listing, error) {
	s, err := filestoreAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	l := console.Listing{Columns: []string{"Status", "Location", "Source instance", "Source file share", "Source tier", "Capacity", "Storage bytes", "Created"},
		NameColumn: "Backup ID", Noun: "backups"}
	if project == "" {
		l.Prompt = "Filestore lists backups per project. Choose one in the toolbar."
		return l, nil
	}
	for _, b := range s.Backups() {
		if !strings.HasPrefix(b.GetName(), "projects/"+project+"/") {
			continue
		}
		l.Items = append(l.Items, console.Resource{Name: filestoreConsoleName(b.GetName()), Status: filestoreState(b.GetState().String()),
			Fields: map[string]string{"Status": filestoreState(b.GetState().String()), "Location": strings.Split(b.GetName(), "/")[3],
				"Source instance": filestoreConsoleName(b.GetSourceInstance()), "Source file share": b.GetSourceFileShare(),
				"Source tier": filestoreTierLabels[b.GetSourceInstanceTier().String()], "Capacity": filestoreCapacity(b.GetCapacityGb()),
				"Storage bytes": strconv.FormatInt(b.GetStorageBytes(), 10), "Created": kmsTime(b.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func (p filestoreBackupsProvider) backup(ctx context.Context, project, name string) (*filestore.Server, *filestorepb.Backup, error) {
	s, err := filestoreAPI(p.svc)
	if err != nil {
		return nil, nil, err
	}
	rn, err := filestoreResourceName(project, "backups", name)
	if err != nil {
		return nil, nil, err
	}
	b, err := s.GetBackup(ctx, &filestorepb.GetBackupRequest{Name: rn})
	return s, b, err
}

func (p filestoreBackupsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	_, b, err := p.backup(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the backup: " + consoleErr(err)}, nil
	}
	return console.Detail{Summary: []console.Property{{Label: "Status", Value: filestoreState(b.GetState().String())}},
		Sections: []console.Section{{ID: "overview", Label: "Overview", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Backup", Properties: []console.Property{
			{Label: "Backup ID", Value: lastSegment(b.GetName())}, {Label: "Location", Value: strings.Split(b.GetName(), "/")[3]},
			{Label: "Description", Value: kmsOrDash(b.GetDescription())}, {Label: "Source instance", Value: b.GetSourceInstance()},
			{Label: "Source file share", Value: b.GetSourceFileShare()}, {Label: "Source instance tier", Value: filestoreTierLabels[b.GetSourceInstanceTier().String()]},
			{Label: "Protocol", Value: filestoreProtocolLabels[b.GetFileSystemProtocol().String()]},
			{Label: "Capacity", Value: filestoreCapacity(b.GetCapacityGb())}, {Label: "Storage bytes", Value: strconv.FormatInt(b.GetStorageBytes(), 10)},
			{Label: "Created", Value: kmsTime(b.GetCreateTime())}, {Label: "Labels", Value: kmsOrDash(labelsText(b.GetLabels()))}}}}}}}, nil
}

func (p filestoreBackupsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	_, b, err := p.backup(ctx, project, path[0])
	if err != nil {
		return nil
	}
	fields := filestoreInstanceFields(true)
	for i := range fields {
		switch fields[i].Name {
		case "tier":
			if strings.HasPrefix(b.GetSourceInstanceTier().String(), "BASIC") {
				fields[i].Options, fields[i].Default = []string{"BASIC_HDD", "BASIC_SSD"}, "BASIC_HDD"
			} else {
				fields[i].Options, fields[i].Default = []string{"ZONAL", "REGIONAL", "ENTERPRISE"}, b.GetSourceInstanceTier().String()
			}
		case "capacityGb":
			fields[i].Default = strconv.FormatInt(b.GetCapacityGb(), 10)
		case "shareName":
			fields[i].Default = b.GetSourceFileShare()
		case "protocol":
			fields[i].Default = b.GetFileSystemProtocol().String()
		}
	}
	return []console.Action{
		{ID: "restore", Label: "Restore", Title: "Create an instance from the backup", Submit: "Create", Page: true, Fields: fields},
		{ID: "edit", Label: "Edit", Title: "Edit backup", Submit: "Save", Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: b.GetDescription()},
			{Name: "labels", Label: "Labels", Type: "map", Default: console.FormatMap(b.GetLabels())}}},
		{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, ConfirmWord: lastSegment(b.GetName()), Confirm: "The backup's data is deleted."},
	}
}

func (p filestoreBackupsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("no backup")
	}
	s, b, err := p.backup(ctx, project, path[0])
	if err != nil {
		return err
	}
	switch action {
	case "restore":
		_, err := filestoreCreate(ctx, p.svc, project, values, b.GetName())
		return err
	case "edit":
		labels, err := console.ParseMap(values["labels"])
		if err != nil {
			return err
		}
		op, err := s.UpdateBackup(ctx, &filestorepb.UpdateBackupRequest{Backup: &filestorepb.Backup{Name: b.GetName(), Description: values["description"], Labels: labels},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description", "labels"}}})
		return filestoreWait(ctx, s, op, err)
	case "delete":
		op, err := s.DeleteBackup(ctx, &filestorepb.DeleteBackupRequest{Name: b.GetName()})
		return filestoreWait(ctx, s, op, err)
	}
	return fmt.Errorf("unknown action %q", action)
}

// filestoreSnapshotsProvider is the Snapshots page: every instance's.
type filestoreSnapshotsProvider struct{ svc *filestoreService }

func (filestoreSnapshotsProvider) ID() string    { return "filestore-snapshots" }
func (filestoreSnapshotsProvider) Title() string { return "Snapshots" }

func (p filestoreSnapshotsProvider) List(_ context.Context, project string) (console.Listing, error) {
	s, err := filestoreAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	l := console.Listing{Columns: []string{"Status", "Instance", "Location", "Filesystem used", "Created", "Description"}, NameColumn: "Snapshot ID", Noun: "snapshots"}
	if project == "" {
		l.Prompt = "Filestore lists snapshots per project. Choose one in the toolbar."
		return l, nil
	}
	for _, in := range s.Instances() {
		if !strings.HasPrefix(in.GetName(), "projects/"+project+"/") {
			continue
		}
		for _, sn := range s.Snapshots(in.GetName()) {
			l.Items = append(l.Items, console.Resource{Name: filestoreConsoleName(in.GetName()) + "/" + lastSegment(sn.GetName()),
				Status: filestoreState(sn.GetState().String()),
				Fields: map[string]string{"Status": filestoreState(sn.GetState().String()), "Instance": lastSegment(in.GetName()),
					"Location": strings.Split(in.GetName(), "/")[3], "Filesystem used": strconv.FormatInt(sn.GetFilesystemUsedBytes(), 10) + " bytes",
					"Created": kmsTime(sn.GetCreateTime()), "Description": kmsOrDash(sn.GetDescription())}})
		}
	}
	l.Total = len(l.Items)
	return l, nil
}

// Actions are a snapshot row's: Delete.
func (filestoreSnapshotsProvider) Actions(console.Resource) []console.Action {
	return []console.Action{{ID: "delete", Label: "Delete", Destructive: true, Confirm: "The snapshot is deleted."}}
}

// Act deletes a snapshot, named LOCATION/INSTANCE/SNAPSHOT.
func (p filestoreSnapshotsProvider) Act(ctx context.Context, project, name, action string) error {
	s, err := filestoreAPI(p.svc)
	if err != nil {
		return err
	}
	parts := strings.Split(name, "/")
	if action != "delete" || len(parts) != 3 {
		return fmt.Errorf("unknown action %q on %q", action, name)
	}
	op, err := s.DeleteSnapshot(ctx, &filestorepb.DeleteSnapshotRequest{
		Name: "projects/" + project + "/locations/" + parts[0] + "/instances/" + parts[1] + "/snapshots/" + parts[2]})
	return filestoreWait(ctx, s, op, err)
}

func isStandbyRole(rp *filestorepb.Replication) bool {
	return rp.GetRole() == filestorepb.Replication_STANDBY
}

// filestoreShareGroup is the instance's file share, or its multishare
// capacity and share count.
func filestoreShareGroup(multi bool, fs *filestorepb.FileShareConfig, capGB, maxShares int64, n int, used, access, mount string) console.PropertyGroup {
	if multi {
		return console.PropertyGroup{Heading: "Multishare", Properties: []console.Property{{Label: "Multishares", Value: "Enabled"},
			{Label: "Capacity", Value: filestoreCapacity(capGB)}, {Label: "Shares", Value: fmt.Sprintf("%d of at most %d", n, maxShares)},
			{Label: "Used", Value: used}}}
	}
	return console.PropertyGroup{Heading: "File share", Properties: []console.Property{{Label: "File share name", Value: fs.GetName()},
		{Label: "Capacity", Value: filestoreCapacity(fs.GetCapacityGb())}, {Label: "Used", Value: used},
		{Label: "Access controls", Value: access}, {Label: "Source backup", Value: kmsOrDash(fs.GetSourceBackup())},
		{Label: "Mount command", Value: mount}}}
}

// filestorePerformanceGroup is the instance's performance limits.
func filestorePerformanceGroup(in *filestorepb.Instance) console.PropertyGroup {
	pl := in.GetPerformanceLimits()
	mibps := func(b int64) string { return strconv.FormatFloat(float64(b)/(1<<20), 'f', 1, 64) + " MiB/s" }
	custom := "Capacity-based"
	switch {
	case in.GetPerformanceConfig().GetIopsPerTb() != nil:
		custom = fmt.Sprintf("Custom: %d IOPS per TiB", in.GetPerformanceConfig().GetIopsPerTb().GetMaxIopsPerTb())
	case in.GetPerformanceConfig().GetFixedIops() != nil:
		custom = fmt.Sprintf("Custom: %d IOPS", in.GetPerformanceConfig().GetFixedIops().GetMaxIops())
	}
	return console.PropertyGroup{Heading: "Performance", Properties: []console.Property{{Label: "Performance", Value: custom},
		{Label: "Max read IOPS", Value: strconv.FormatInt(pl.GetMaxReadIops(), 10)}, {Label: "Max write IOPS", Value: strconv.FormatInt(pl.GetMaxWriteIops(), 10)},
		{Label: "Max read throughput", Value: mibps(pl.GetMaxReadThroughputBps())}, {Label: "Max write throughput", Value: mibps(pl.GetMaxWriteThroughputBps())}}}
}

// filestoreMultishareActions are a multishare instance's actions: edit its
// capacity, create and delete shares, delete it.
func filestoreMultishareActions(in *filestorepb.Instance, capGB int64, shares []*betapb.Share) []console.Action {
	var ids []string
	for _, sh := range shares {
		ids = append(ids, lastSegment(sh.GetName()))
	}
	editShare := console.Action{ID: "editShare", Label: "Edit share", Title: "Edit a share", Submit: "Save"}
	if len(ids) == 0 {
		editShare.Unavailable = "The instance has no share."
	} else {
		editShare.Fields = []console.Field{{Name: "share", Label: "Share", Type: "select", Required: true, Options: ids, Default: ids[0]},
			{Name: "capacityGb", Label: "Capacity (GiB)", Type: "number", Required: true, Default: strconv.FormatInt(shares[0].GetCapacityGb(), 10),
				Help: "10 to 1024 GiB, within the instance's capacity."},
			{Name: "description", Label: "Description", Type: "text", Help: "Optional."},
			{Name: "labels", Label: "Labels", Type: "map"}}
	}
	delShare := console.Action{ID: "deleteShare", Label: "Delete share", Title: "Delete a share", Submit: "Delete", Destructive: true,
		ConfirmStyle: console.DeleteConfirmClick, ConfirmTitle: "Delete share?", Confirm: "The share and its files are deleted."}
	if len(ids) == 0 {
		delShare.Unavailable = "The instance has no share."
	} else {
		delShare.Fields = []console.Field{{Name: "share", Label: "Share", Type: "select", Required: true, Options: ids, Default: ids[0]}}
	}
	del := console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, ConfirmWord: lastSegment(in.GetName()),
		Confirm: "The instance and its shares' data are deleted."}
	if in.GetDeletionProtectionEnabled() {
		del.Unavailable = "Deletion protection is enabled: disable it with Edit first."
	}
	return []console.Action{
		{ID: "edit", Label: "Edit", Title: "Edit instance", Submit: "Save", Page: true, Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: in.GetDescription()},
			{Name: "capacityGb", Label: "Capacity (GiB)", Type: "number", Required: true, Default: strconv.FormatInt(capGB, 10),
				Help: "1024 to 10240 GiB in 256 GiB steps, at least what the shares take."},
			{Name: "labels", Label: "Labels", Type: "map", Default: console.FormatMap(in.GetLabels())},
			{Name: "deletionProtection", Label: "Enable deletion protection", Type: "checkbox", Default: strconv.FormatBool(in.GetDeletionProtectionEnabled())}}},
		{ID: "createShare", Label: "Create share", Title: "Create a share", Submit: "Create", Fields: []console.Field{
			{Name: "shareId", Label: "Share ID", Type: "text", Required: true, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`},
			{Name: "mountName", Label: "Mount name", Type: "text", Required: true, Pattern: `^[A-Za-z][A-Za-z0-9_]{0,62}$`,
				Help: "Clients mount IP_ADDRESS:/MOUNT_NAME."},
			{Name: "capacityGb", Label: "Capacity (GiB)", Type: "number", Required: true, Default: "100", Help: "10 to 1024 GiB."}}},
		editShare, delShare, del,
	}
}

// filestoreMultishareAct runs a multishare instance's action.
func filestoreMultishareAct(ctx context.Context, s *filestore.Server, in *filestorepb.Instance, action string, values map[string]string) error {
	b := s.Beta()
	gb := func() (int64, error) {
		v, err := strconv.ParseInt(strings.TrimSpace(values["capacityGb"]), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("capacity %q is not a number of GiB", values["capacityGb"])
		}
		return v, nil
	}
	switch action {
	case "edit":
		capacity, err := gb()
		if err != nil {
			return err
		}
		labels, err := console.ParseMap(values["labels"])
		if err != nil {
			return err
		}
		op, err := b.UpdateInstance(ctx, &betapb.UpdateInstanceRequest{Instance: &betapb.Instance{Name: in.GetName(), Description: values["description"],
			Labels: labels, CapacityGb: capacity, DeletionProtectionEnabled: values["deletionProtection"] == "true"},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description", "labels", "capacity_gb", "deletion_protection_enabled"}}})
		return filestoreWait(ctx, s, op, err)
	case "createShare":
		capacity, err := gb()
		if err != nil {
			return err
		}
		op, err := b.CreateShare(ctx, &betapb.CreateShareRequest{Parent: in.GetName(), ShareId: strings.TrimSpace(values["shareId"]),
			Share: &betapb.Share{MountName: strings.TrimSpace(values["mountName"]), CapacityGb: capacity}})
		return filestoreWait(ctx, s, op, err)
	case "editShare":
		capacity, err := gb()
		if err != nil {
			return err
		}
		labels, err := console.ParseMap(values["labels"])
		if err != nil {
			return err
		}
		op, err := b.UpdateShare(ctx, &betapb.UpdateShareRequest{Share: &betapb.Share{Name: in.GetName() + "/shares/" + values["share"],
			CapacityGb: capacity, Description: values["description"], Labels: labels},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"capacity_gb", "description", "labels"}}})
		return filestoreWait(ctx, s, op, err)
	case "deleteShare":
		op, err := b.DeleteShare(ctx, &betapb.DeleteShareRequest{Name: in.GetName() + "/shares/" + values["share"]})
		return filestoreWait(ctx, s, op, err)
	}
	return fmt.Errorf("action %q is not available on a multishare instance", action)
}
