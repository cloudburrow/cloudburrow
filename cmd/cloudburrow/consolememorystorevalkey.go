package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// valkeyProvider is Memorystore for Valkey's page (#1519): the project's
// instances through the Memorystore API `up` serves, created (cluster mode
// or not), resized (shards, replicas, node type) and deleted, each change
// waited for, as the Memorystore console's Valkey tab does.
type valkeyProvider struct {
	// server returns the admin API, nil before it has started.
	server func() *redisadmin.Server
}

func newValkeyProvider(svc *memorystoreAdminService) valkeyProvider {
	return valkeyProvider{server: func() *redisadmin.Server {
		if svc == nil {
			return nil
		}
		return svc.api
	}}
}

func (valkeyProvider) ID() string    { return "memorystore-valkey" }
func (valkeyProvider) Title() string { return "Memorystore for Valkey" }

func (p valkeyProvider) api() (*redisadmin.MemorystoreServer, *redisadmin.Server, error) {
	srv := p.server()
	if srv == nil {
		return nil, nil, errors.New("the Memorystore admin API has not started")
	}
	return redisadmin.NewMemorystoreServer(srv), srv, nil
}

const valkeyNote = "Each instance runs real Valkey 8.1 nodes: in cluster mode shard_count primaries with replica_count replicas each, " +
	"with cluster mode disabled one primary and its replicas. A client on the host connects to the endpoints shown (a cluster client in the Kubernetes cluster to a node's Service, which gives it the nodes' Services, when the instance is bound to 127.0.0.1), over TLS trusting the instance's own CA when in-transit encryption is on; token auth users and backups are on its page."

// valkeyEndpoint is an instance's first endpoint of kind, host:port.
func valkeyEndpoint(inst *memorystorepb.Instance, kind memorystorepb.ConnectionType) string {
	for _, a := range valkeyConnections(inst) {
		if a.GetConnectionType() == kind {
			return net.JoinHostPort(a.GetIpAddress(), strconv.Itoa(int(a.GetPort())))
		}
	}
	if kind == memorystorepb.ConnectionType_CONNECTION_TYPE_DISCOVERY {
		if eps := inst.GetDiscoveryEndpoints(); len(eps) > 0 {
			return net.JoinHostPort(eps[0].GetAddress(), strconv.Itoa(int(eps[0].GetPort())))
		}
	}
	return "—"
}

// valkeyConnections are an instance's PSC auto connections, from its
// endpoints or the deprecated psc_auto_connections.
func valkeyConnections(inst *memorystorepb.Instance) []*memorystorepb.PscAutoConnection {
	out := inst.GetPscAutoConnections()
	for _, e := range inst.GetEndpoints() {
		for _, c := range e.GetConnections() {
			if a := c.GetPscAutoConnection(); a != nil {
				out = append(out, a)
			}
		}
	}
	return out
}

func valkeyMainEndpoint(inst *memorystorepb.Instance) string {
	if inst.GetMode() == memorystorepb.Instance_CLUSTER_DISABLED {
		return valkeyEndpoint(inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	}
	return valkeyEndpoint(inst, memorystorepb.ConnectionType_CONNECTION_TYPE_DISCOVERY)
}

// List implements console.Provider: the project's instances.
func (p valkeyProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Location", "Cluster Mode", "Version", "Shards", "Replicas", "Node type", "Endpoint"},
		NameColumn: "Instance ID", Noun: "instances", Note: valkeyNote}
	api, _, err := p.api()
	if err != nil {
		l.Unavailable = err.Error()
		return l, nil
	}
	resp, err := api.ListInstances(ctx, &memorystorepb.ListInstancesRequest{Parent: "projects/" + project + "/locations/-", PageSize: 1000})
	if err != nil {
		l.Unavailable = "cannot reach Memorystore: " + err.Error()
		return l, nil
	}
	for _, i := range resp.GetInstances() {
		l.Items = append(l.Items, console.Resource{Name: memorystoreID(i.GetName()), Status: i.GetState().String(),
			Fields: map[string]string{
				"Location":     strings.Split(i.GetName(), "/")[3],
				"Cluster Mode": map[bool]string{true: "Disabled", false: "Enabled"}[i.GetMode() == memorystorepb.Instance_CLUSTER_DISABLED],
				"Endpoint":     valkeyMainEndpoint(i),
				"Shards":       strconv.Itoa(int(i.GetShardCount())),
				"Replicas":     strconv.Itoa(int(i.GetReplicaCount())),
				"Node type":    memorystoreOption(valkeyNodeTypeLabels, i.GetNodeType().String()),
				"Version":      memorystoreOption(valkeyVersionLabels, i.GetEngineVersion()),
			}})
	}
	l.Total = len(l.Items)
	return l, nil
}

var valkeyNodeTypes = []string{"SHARED_CORE_NANO", "STANDARD_SMALL", "HIGHMEM_MEDIUM", "HIGHMEM_XLARGE"}

// valkeyVersionLabels are the engine versions as Google names them.
var valkeyVersionLabels = map[string]string{"VALKEY_7_2": "Valkey 7.2", "VALKEY_8_0": "Valkey 8.0", "VALKEY_9_0": "Valkey 9.0", "VALKEY_9_1": "Valkey 9.1"}

// valkeyNodeTypeLabels are the node types as Google's page names them.
var valkeyNodeTypeLabels = map[string]string{"SHARED_CORE_NANO": "shared-core-nano", "STANDARD_SMALL": "standard-small",
	"HIGHMEM_MEDIUM": "highmem-medium", "HIGHMEM_XLARGE": "highmem-xlarge"}

// CreateForm implements console.Creator: Google's Create instance page, in
// its order (#1862): Instance ID, Region, Zonal availability, Cluster Mode,
// Node type, Instance size, Replicas (default 1), Set up connection,
// Security, Certificate authority management, Encryption, ACL policy, Data
// persistence strategy, Automated backups, Maintenance and Choose version,
// then this console's advanced options (Engine configs, Restore from
// backup, Secondary of).
func (valkeyProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "id", Label: "Instance ID", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9\-]{2,61}[a-z0-9]$`,
			Help: "4 to 63 lowercase letters, digits and hyphens, starting with a letter; unique in its region."},
		{Name: "region", Label: "Region", Type: "text", Required: true, Default: redisadmin.Region},
	}
	fields = append(fields, memorystoreZonalFields()...)
	fields = append(fields,
		console.Field{Name: "mode", Label: "Cluster Mode", Type: "radio", Default: "CLUSTER", Options: []string{"CLUSTER", "CLUSTER_DISABLED"},
			OptionLabels: map[string]string{"CLUSTER": "Enabled", "CLUSTER_DISABLED": "Disabled"},
			Help:         "Enabled shards the keyspace; Disabled is one primary and its replicas, of one shard. It cannot be changed later."},
		console.Field{Name: "nodeType", Label: "Node type", Type: "select", Default: "HIGHMEM_MEDIUM", Options: valkeyNodeTypes,
			OptionLabels: valkeyNodeTypeLabels,
			Help:         "Each node's maxmemory: 1.4, 6.5, 13 or 58 GB. shared-core-nano has no SLA on Google: for development and testing."},
		console.Field{Name: "shardCount", Label: "Instance size", Type: "number", Required: true, Default: "1", Pattern: `^[0-9]{1,3}$`,
			Help: "The number of shards, which sets the instance's capacity; Cluster Mode Disabled has one."},
		console.Field{Name: "replicaCount", Label: "Replicas", Type: "number", Default: "1", Pattern: `^[0-5]$`, Help: "0 to 5 replicas per shard."},
		console.Field{Name: "network", Label: "Network", Type: "text", Section: msSectionConnection,
			Help: "Optional. projects/{project}/global/networks/{network}: the network your applications connect from; an endpoint is created on it."},
	)
	fields = append(fields, memorystoreSecurityFields("instance")...)
	fields = append(fields, memorystoreEncryptionFields("instance")...)
	fields = append(fields,
		console.Field{Name: "aclPolicy", Label: "Policy", Type: "text", Section: msSectionACL,
			Help: "Optional. projects/{project}/locations/{location}/aclPolicies/{policy}, in the instance's region: its rules become ACL users on every node."},
		memorystorePersistenceField("Memory Snapshot"))
	fields = append(fields, memorystoreBackupFields()...)
	fields = append(fields, memorystoreMaintenanceFields()...)
	fields = append(fields,
		console.Field{Name: "engineVersion", Label: "Choose version", Type: "radio", Default: redisadmin.DefaultValkeyVersion, Section: "Choose version",
			Options:      valkeyEngineVersions,
			OptionLabels: valkeyVersionLabels,
			Help:         "Valkey 9.1 is the default, as Google's. A version cannot be downgraded later."},
		console.Field{Name: "engineConfigs", Label: "Engine configs", Type: "map", Section: msSectionAdvanced,
			Help: "Optional. One key=value per line, such as maxmemory-policy."},
		console.Field{Name: "restoreBackup", Label: "Restore from backup", Type: "text", Section: msSectionAdvanced,
			Help: "Optional. projects/{project}/locations/{location}/backupCollections/{collection}/backups/{backup}: the instance starts with its keys."},
		console.Field{Name: "primaryInstance", Label: "Secondary of (cross-instance replication)", Type: "text", Section: msSectionAdvanced,
			ShowWhen: "mode=CLUSTER_DISABLED",
			Help:     "Optional. projects/{project}/locations/{location}/instances/{instance}: the instance replicates that primary and is read-only."})
	return "Create instance", fields
}

// CreateTitle implements console.CreateTitler.
func (valkeyProvider) CreateTitle() string { return "Create a Valkey instance" }

// CreateLayout implements console.CreateLayouter: one page under
// headings, as Google's.
func (valkeyProvider) CreateLayout() string { return console.CreateLayoutPage }

// valkeyEngineVersions are the engine_version values served (#1557, 9.1
// since #2019), Google's Choose version options.
var valkeyEngineVersions = []string{"VALKEY_7_2", "VALKEY_8_0", "VALKEY_9_0", "VALKEY_9_1"}

// CreateOnPage implements console.PageCreator.
func (valkeyProvider) CreateOnPage() bool { return true }

// Create implements console.Creator, waiting for the instance to serve.
func (p valkeyProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	api, srv, err := p.api()
	if err != nil {
		return "", err
	}
	shards, err := atoiField(values, "shardCount", "shards")
	if err != nil {
		return "", err
	}
	replicas, err := atoiField(values, "replicaCount", "replicas")
	if err != nil {
		return "", err
	}
	configs, err := console.ParseMap(values["engineConfigs"])
	if err != nil {
		return "", err
	}
	in := &memorystorepb.Instance{ShardCount: shards, ReplicaCount: proto.Int32(replicas), EngineConfigs: configs,
		NodeType: memorystorepb.Instance_NodeType(memorystorepb.Instance_NodeType_value[values["nodeType"]]),
		Mode:     memorystorepb.Instance_Mode(memorystorepb.Instance_Mode_value[values["mode"]])}
	if memorystoreTLS(values) {
		in.TransitEncryptionMode = memorystorepb.Instance_SERVER_AUTHENTICATION
	}
	if mode, pool := memorystoreCAMode(values); mode != 0 {
		in.ServerCaMode = memorystorepb.Instance_ServerCaMode(mode).Enum()
		if pool != "" {
			in.ServerCaPool = proto.String(pool)
		}
	}
	switch memorystoreAuthMode(values) {
	case "IAM_AUTH":
		in.AuthorizationMode = memorystorepb.Instance_IAM_AUTH
	case "TOKEN_AUTH":
		in.AuthorizationMode = memorystorepb.Instance_TOKEN_AUTH
	}
	if values["zoneDistribution"] == "SINGLE_ZONE" {
		in.ZoneDistributionConfig = &memorystorepb.ZoneDistributionConfig{Mode: memorystorepb.ZoneDistributionConfig_SINGLE_ZONE,
			Zone: strings.TrimSpace(values["zone"])}
	} else if values["zoneDistribution"] == "MULTI_ZONE" {
		in.ZoneDistributionConfig = &memorystorepb.ZoneDistributionConfig{Mode: memorystorepb.ZoneDistributionConfig_MULTI_ZONE}
	}
	on, hour, retention, err := memorystoreAutoBackup(values)
	if err != nil {
		return "", err
	}
	if on {
		in.AutomatedBackupConfig = &memorystorepb.AutomatedBackupConfig{AutomatedBackupMode: memorystorepb.AutomatedBackupConfig_ENABLED,
			Retention: durationpb.New(retention),
			Schedule: &memorystorepb.AutomatedBackupConfig_FixedFrequencySchedule_{FixedFrequencySchedule: &memorystorepb.AutomatedBackupConfig_FixedFrequencySchedule{
				StartTime: &timeofday.TimeOfDay{Hours: hour}}}}
	}
	day, start, has, err := memorystoreCreateWindow(values)
	if err != nil {
		return "", err
	}
	if has {
		in.MaintenancePolicy = &memorystorepb.MaintenancePolicy{WeeklyMaintenanceWindow: []*memorystorepb.WeeklyMaintenanceWindow{{Day: day, StartTime: start}}}
	}
	if pol := strings.TrimSpace(values["aclPolicy"]); pol != "" {
		in.AclPolicy = proto.String(pol)
	}
	if b := strings.TrimSpace(values["restoreBackup"]); b != "" {
		in.ImportSources = &memorystorepb.Instance_ManagedBackupSource_{ManagedBackupSource: &memorystorepb.Instance_ManagedBackupSource{Backup: b}}
	}
	if n := strings.TrimSpace(values["network"]); n != "" {
		in.Endpoints = []*memorystorepb.Instance_InstanceEndpoint{{Connections: []*memorystorepb.Instance_ConnectionDetail{{
			Connection: &memorystorepb.Instance_ConnectionDetail_PscAutoConnection{PscAutoConnection: &memorystorepb.PscAutoConnection{
				ProjectId: project, Network: n}}}}}}
	}
	in.EngineVersion = values["engineVersion"]
	if m := values["persistence"]; m != "" && m != "DISABLED" {
		in.PersistenceConfig = &memorystorepb.PersistenceConfig{Mode: memorystorepb.PersistenceConfig_PersistenceMode(memorystorepb.PersistenceConfig_PersistenceMode_value[m])}
	}
	if k := memorystoreKMSKey(values); k != "" {
		in.KmsKey = proto.String(k)
	}
	if pi := strings.TrimSpace(values["primaryInstance"]); pi != "" {
		in.CrossInstanceReplicationConfig = &memorystorepb.CrossInstanceReplicationConfig{InstanceRole: memorystorepb.CrossInstanceReplicationConfig_SECONDARY,
			PrimaryInstance: &memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{Instance: pi}}
	}
	region, id := strings.TrimSpace(values["region"]), strings.TrimSpace(values["id"])
	op, err := api.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/" + region, InstanceId: id, Instance: in})
	if err != nil {
		return "", err
	}
	if err := waitRedisOp(ctx, srv, op.GetName()); err != nil {
		return "", err
	}
	return memorystoreID("projects/" + project + "/locations/" + region + "/instances/" + id), nil
}

// Delete implements console.Deleter: DeleteInstance, waited for.
func (p valkeyProvider) Delete(ctx context.Context, project, id string) error {
	api, srv, err := p.api()
	if err != nil {
		return err
	}
	name, err := memorystoreName(project, id)
	if err != nil {
		return err
	}
	op, err := api.DeleteInstance(ctx, &memorystorepb.DeleteInstanceRequest{Name: name})
	if err != nil {
		return err
	}
	return waitRedisOp(ctx, srv, op.GetName())
}

// Detail implements console.Detailer: an instance's properties and its
// Edit form.
func (p valkeyProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 || (len(path) == 2 && path[0] == redisadmin.Region) {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	api, _, err := p.api()
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	name, err := memorystoreName(project, strings.Join(path, "/"))
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	i, err := api.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the instance: " + consoleErr(err)}, nil
	}
	var nets []string
	seen := map[string]bool{}
	for _, a := range valkeyConnections(i) {
		if !seen[a.GetNetwork()] {
			seen[a.GetNetwork()] = true
			nets = append(nets, a.GetNetwork())
		}
	}
	// Google's instance page (#1862): Overview, Configuration,
	// Connections, the Server CA, Token auth, Backups and ACL policies,
	// Maintenance, and Monitoring, which reads Cloud Monitoring's metrics.
	overview := []console.Property{
		{Label: "State", Value: i.GetState().String()},
		{Label: "Cluster Mode", Value: memorystoreOnOff(i.GetMode() != memorystorepb.Instance_CLUSTER_DISABLED)},
		{Label: "Shards", Value: strconv.Itoa(int(i.GetShardCount()))},
		{Label: "Replicas per shard", Value: strconv.Itoa(int(i.GetReplicaCount()))},
		{Label: "Node type", Value: memorystoreOption(valkeyNodeTypeLabels, i.GetNodeType().String())},
		{Label: "Node capacity", Value: fmt.Sprintf("%g GB", i.GetNodeConfig().GetSizeGb())},
		{Label: "Version", Value: memorystoreOption(valkeyVersionLabels, i.GetEngineVersion())},
		{Label: "Zonal availability", Value: memorystoreZones(i.GetZoneDistributionConfig().GetMode().String(), i.GetZoneDistributionConfig().GetZone())},
		{Label: "Labels", Value: orDash(memorystorePairs(i.GetLabels()))},
		{Label: "Deletion protection", Value: strconv.FormatBool(i.GetDeletionProtectionEnabled())},
		{Label: "Encryption", Value: memorystoreEncryptionText(i.GetEncryptionInfo().GetEncryptionType().String())},
		{Label: "Customer-managed key", Value: orDash(i.GetKmsKey())},
	}
	if ks := i.GetEncryptionInfo().GetKmsKeyPrimaryState(); i.GetKmsKey() != "" && ks != memorystorepb.EncryptionInfo_KMS_KEY_STATE_UNSPECIFIED {
		overview = append(overview, console.Property{Label: "Key state", Value: ks.String()})
	}
	ab := i.GetAutomatedBackupConfig()
	config := []console.Property{
		{Label: "Data persistence strategy", Value: memorystorePersistenceText(i.GetPersistenceConfig().GetMode().String(), "Memory Snapshot")},
		{Label: "Automated backups", Value: memorystoreAutoBackupText(ab.GetAutomatedBackupMode() == memorystorepb.AutomatedBackupConfig_ENABLED,
			ab.GetFixedFrequencySchedule().GetStartTime().GetHours(), ab.GetRetention().AsDuration())},
		{Label: "Security", Value: memorystoreAuthText(i.GetAuthorizationMode().String())},
		{Label: "ACL policy", Value: orDash(i.GetAclPolicy())},
		// IAM auth checks the token (#1614): who may connect is the
		// project's IAM policy, granted on the IAM page.
		{Label: "IAM connect role", Value: valkeyIAMRole(i)},
		{Label: "In-transit encryption", Value: memorystoreTLSText(i.GetTransitEncryptionMode().String())},
		{Label: "Server CA mode", Value: orDash(memorystoreCAModeText(i.GetServerCaMode().String(), i.ServerCaMode != nil))},
		{Label: "Server CA pool", Value: orDash(i.GetServerCaPool())},
		{Label: "Engine configs", Value: orDash(memorystorePairs(i.GetEngineConfigs()))},
	}
	config = append(config, valkeyReplicationProps(i)...)
	var conns []console.Property
	if i.GetMode() == memorystorepb.Instance_CLUSTER_DISABLED {
		conns = append(conns,
			console.Property{Label: "Primary endpoint", Value: valkeyEndpoint(i, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)},
			console.Property{Label: "Reader endpoint", Value: valkeyEndpoint(i, memorystorepb.ConnectionType_CONNECTION_TYPE_READER)})
	} else {
		conns = append(conns, console.Property{Label: "Discovery endpoint", Value: valkeyEndpoint(i, memorystorepb.ConnectionType_CONNECTION_TYPE_DISCOVERY)})
	}
	conns = append(conns, console.Property{Label: "PSC networks", Value: orDash(strings.Join(nets, ", "))})
	d := console.Detail{Sections: []console.Section{
		memorystorePropsTab("overview", "Overview", overview),
		memorystorePropsTab("configuration", "Configuration", config),
		memorystorePropsTab("connections", "Connections", conns),
	}}
	if i.GetTransitEncryptionMode() == memorystorepb.Instance_SERVER_AUTHENTICATION {
		// The CA its clients trust (#1549), as GetCertificateAuthority
		// returns it.
		if ca, err := api.GetCertificateAuthority(ctx, &memorystorepb.GetCertificateAuthorityRequest{Name: name + "/certificateAuthority"}); err == nil {
			var text strings.Builder
			for _, chain := range ca.GetManagedServerCa().GetCaCerts() {
				for _, cert := range chain.GetCertificates() {
					text.WriteString(strings.TrimSpace(cert) + "\n")
				}
			}
			d.Sections = append(d.Sections, console.Section{ID: "server-ca", Label: "Server CA", Kind: console.KindText, Text: text.String()})
		}
	}
	d.Sections = append(d.Sections, p.authSections(ctx, api, i)...)
	d.Sections = append(d.Sections, memorystorePropsTab("maintenance", "Maintenance", valkeyMaintenance(i).summary()), memorystoreMonitoring("instance"))
	memorystoreTabActions(&d, valkeyPersistenceView(i).action(), valkeyMaintenance(i))
	fields := []console.Field{}
	if i.GetMode() != memorystorepb.Instance_CLUSTER_DISABLED {
		fields = append(fields, console.Field{Name: "shardCount", Label: "Shards", Type: "number", Required: true, Pattern: `^[0-9]{1,3}$`,
			Default: strconv.Itoa(int(i.GetShardCount())), Help: "Slots and their keys move to or off the primaries added or removed."})
	}
	fields = append(fields,
		console.Field{Name: "replicaCount", Label: "Replicas per shard", Type: "number", Required: true, Pattern: `^[0-5]$`,
			Default: strconv.Itoa(int(i.GetReplicaCount()))},
		console.Field{Name: "nodeType", Label: "Node type", Type: "select", Default: i.GetNodeType().String(), Options: valkeyNodeTypes, OptionLabels: valkeyNodeTypeLabels},
		console.Field{Name: "deletionProtection", Label: "Deletion protection", Type: "checkbox", Default: strconv.FormatBool(i.GetDeletionProtectionEnabled())},
		console.Field{Name: "engineVersion", Label: "Engine version", Type: "select", Default: i.GetEngineVersion(), Options: valkeyEngineVersions, OptionLabels: valkeyVersionLabels,
			Help: "A later version upgrades the instance: new nodes are formed and loaded with a snapshot of its data."})
	d.Edit = &console.EditForm{Label: "Edit", Title: "Edit instance", Submit: "Save", Page: true, Fields: fields,
		Note: "Saved through UpdateInstance, shards or replicas one change at a time."}
	return d, nil
}

// Edit implements console.Editor: UpdateInstance of what changed, shards
// and replicas each in an update of its own, as Google requires.
func (p valkeyProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	api, srv, err := p.api()
	if err != nil {
		return err
	}
	name, err := memorystoreName(project, strings.Join(path, "/"))
	if err != nil {
		return err
	}
	cur, err := api.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	if err != nil {
		return err
	}
	shards := cur.GetShardCount()
	if _, ok := values["shardCount"]; ok {
		if shards, err = atoiField(values, "shardCount", "shards"); err != nil {
			return err
		}
	}
	replicas, err := atoiField(values, "replicaCount", "replicas")
	if err != nil {
		return err
	}
	update := func(path string, in *memorystorepb.Instance) error {
		in.Name = name
		op, err := api.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{path}}, Instance: in})
		if err != nil {
			return err
		}
		return waitRedisOp(ctx, srv, op.GetName())
	}
	if nt := values["nodeType"]; nt != "" && nt != cur.GetNodeType().String() {
		if err := update("node_type", &memorystorepb.Instance{NodeType: memorystorepb.Instance_NodeType(memorystorepb.Instance_NodeType_value[nt])}); err != nil {
			return err
		}
	}
	if dp := values["deletionProtection"] == "true"; dp != cur.GetDeletionProtectionEnabled() {
		if err := update("deletion_protection_enabled", &memorystorepb.Instance{DeletionProtectionEnabled: proto.Bool(dp)}); err != nil {
			return err
		}
	}
	if ev := values["engineVersion"]; ev != "" && ev != cur.GetEngineVersion() {
		if err := update("engine_version", &memorystorepb.Instance{EngineVersion: ev}); err != nil {
			return err
		}
	}
	if shards != cur.GetShardCount() {
		if err := update("shard_count", &memorystorepb.Instance{ShardCount: shards}); err != nil {
			return err
		}
	}
	if replicas != cur.GetReplicaCount() {
		return update("replica_count", &memorystorepb.Instance{ReplicaCount: proto.Int32(replicas)})
	}
	return nil
}

// valkeyReplicationProps are an instance's cross-instance replication and
// migration (#1557): its role, its primary or secondaries, its membership,
// and a migration's state and source.
func valkeyReplicationProps(i *memorystorepb.Instance) []console.Property {
	cfg := i.GetCrossInstanceReplicationConfig()
	role := "NONE"
	if cfg != nil {
		role = cfg.GetInstanceRole().String()
	}
	props := []console.Property{{Label: "Cross-instance replication", Value: role}}
	if cfg != nil {
		var secs []string
		for _, s := range cfg.GetSecondaryInstances() {
			secs = append(secs, s.GetInstance())
		}
		var members []string
		if m := cfg.GetMembership(); m != nil {
			members = append(members, m.GetPrimaryInstance().GetInstance()+" (primary)")
			for _, s := range m.GetSecondaryInstances() {
				members = append(members, s.GetInstance())
			}
		}
		props = append(props,
			console.Property{Label: "Primary instance", Value: orDash(cfg.GetPrimaryInstance().GetInstance())},
			console.Property{Label: "Secondary instances", Value: orDash(strings.Join(secs, ", "))},
			console.Property{Label: "Replication membership", Value: orDash(strings.Join(members, ", "))})
	}
	if m := i.GetMigrationConfig(); m != nil {
		src := m.GetSelfManagedSource()
		props = append(props, console.Property{Label: "Migration", Value: m.GetState().String()},
			console.Property{Label: "Migration source", Value: valkeySourceAddr(src)})
	}
	return props
}

func valkeySourceAddr(src *memorystorepb.SelfManagedSource) string {
	if src.GetIpAddress() == "" {
		return "-"
	}
	return net.JoinHostPort(src.GetIpAddress(), strconv.Itoa(int(src.GetPort())))
}

// valkeyReplicationActions are a CLUSTER_DISABLED instance's (#1557):
// a secondary's Switchover and Detach; a migration's Start and Finish.
func valkeyReplicationActions(i *memorystorepb.Instance) []console.Action {
	if i.GetMode() != memorystorepb.Instance_CLUSTER_DISABLED {
		return nil
	}
	var out []console.Action
	role := i.GetCrossInstanceReplicationConfig().GetInstanceRole()
	switch {
	case role == memorystorepb.CrossInstanceReplicationConfig_SECONDARY:
		out = append(out,
			console.Action{ID: "switchover", Label: "Switchover", Confirm: "This instance becomes the primary, writable; its primary and the other secondaries replicate it."},
			console.Action{ID: "detach-replication", Label: "Detach", Destructive: true, Confirm: "This instance stops replicating its primary and becomes writable."})
	case i.GetState() == memorystorepb.Instance_MIGRATING:
		out = append(out, console.Action{ID: "finish-migration", Label: "Finish migration",
			Confirm: "The instance stops replicating the source and becomes writable."})
	case role != memorystorepb.CrossInstanceReplicationConfig_PRIMARY:
		out = append(out, console.Action{ID: "start-migration", Label: "Start migration", Fields: []console.Field{
			{Name: "ip", Label: "Source IP address", Type: "text", Required: true},
			{Name: "port", Label: "Source port", Type: "number", Required: true, Default: "6379", Pattern: `^[0-9]{1,5}$`},
			{Name: "attachment", Label: "Network attachment", Type: "text", Help: "Optional. projects/{project}/regions/{region}/networkAttachments/{attachment}; default the region's default."}}})
	}
	return out
}

// valkeyReplicationAct runs one of valkeyReplicationActions; ok is false
// for an action of another kind.
func valkeyReplicationAct(ctx context.Context, api *redisadmin.MemorystoreServer, i *memorystorepb.Instance, action string, values map[string]string) (op interface{ GetName() string }, ok bool, err error) {
	cir := func(cfg *memorystorepb.CrossInstanceReplicationConfig) (interface{ GetName() string }, bool, error) {
		op, err := api.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cross_instance_replication_config"}},
			Instance:   &memorystorepb.Instance{Name: i.GetName(), CrossInstanceReplicationConfig: cfg}})
		return op, true, err
	}
	switch action {
	case "detach-replication":
		return cir(&memorystorepb.CrossInstanceReplicationConfig{InstanceRole: memorystorepb.CrossInstanceReplicationConfig_NONE})
	case "switchover":
		cfg := i.GetCrossInstanceReplicationConfig()
		secs := []*memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{{Instance: cfg.GetPrimaryInstance().GetInstance()}}
		for _, s := range cfg.GetMembership().GetSecondaryInstances() {
			if s.GetInstance() != i.GetName() {
				secs = append(secs, &memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{Instance: s.GetInstance()})
			}
		}
		return cir(&memorystorepb.CrossInstanceReplicationConfig{InstanceRole: memorystorepb.CrossInstanceReplicationConfig_PRIMARY, SecondaryInstances: secs})
	case "start-migration":
		port, perr := strconv.Atoi(strings.TrimSpace(values["port"]))
		if perr != nil {
			return nil, true, fmt.Errorf("source port %q is not a number", values["port"])
		}
		att := strings.TrimSpace(values["attachment"])
		if parts := strings.Split(i.GetName(), "/"); att == "" && len(parts) > 3 {
			// The region's default, as no network is real here.
			att = "projects/" + parts[1] + "/regions/" + parts[3] + "/networkAttachments/default"
		}
		op, err := api.StartMigration(ctx, &memorystorepb.StartMigrationRequest{Name: i.GetName(),
			Source: &memorystorepb.StartMigrationRequest_SelfManagedSource{SelfManagedSource: &memorystorepb.SelfManagedSource{
				IpAddress: strings.TrimSpace(values["ip"]), Port: int32(port), NetworkAttachment: att}}})
		return op, true, err
	case "finish-migration":
		op, err := api.FinishMigration(ctx, &memorystorepb.FinishMigrationRequest{Name: i.GetName()})
		return op, true, err
	}
	return nil, false, nil
}

// valkeyIAMRole is, for an instance with IAM auth, the role Google
// documents a principal needs to connect (#1614), and where it is granted.
func valkeyIAMRole(i *memorystorepb.Instance) string {
	if i.GetAuthorizationMode() != memorystorepb.Instance_IAM_AUTH {
		return "-"
	}
	return "roles/memorystore.dbConnectionUser on the project, granted on IAM & Admin; a client's AUTH is an access token minted for the principal"
}
