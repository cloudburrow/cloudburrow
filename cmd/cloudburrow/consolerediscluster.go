package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// redisClusterProvider is Memorystore's Redis Cluster page (#1519): the
// project's clusters through the Redis Cluster API `up` serves, created,
// resized (shards, replicas, node type) and deleted, each change waited
// for, as the Memorystore console's Clusters tab does.
type redisClusterProvider struct {
	svc *memorystoreAdminService
}

func (redisClusterProvider) ID() string    { return "memorystore-clusters" }
func (redisClusterProvider) Title() string { return "Memorystore Redis Cluster" }

// redisClusterConnectRole is the role Google documents a principal needs
// to connect to a cluster with IAM auth (#1614).
const redisClusterConnectRole = "roles/redis.dbConnectionUser"

func (p redisClusterProvider) api() (*redisadmin.ClusterServer, *redisadmin.Server, error) {
	if p.svc == nil || p.svc.api == nil {
		return nil, nil, errors.New("the Memorystore admin API has not started")
	}
	return redisadmin.NewClusterServer(p.svc.api), p.svc.api, nil
}

const redisClusterNote = "Each cluster is a real Redis Cluster: shard_count primaries with replica_count replicas each, every node a Valkey server in cluster mode. " +
	"A cluster client on the host starts from the discovery endpoint and follows MOVED to the nodes; a cluster client in the Kubernetes cluster starts from a node's Service, and is given the nodes' Services, when the instance is bound to 127.0.0.1; with in-transit encryption it trusts the cluster's own CA, which its page shows. IAM auth takes any access token as the AUTH string (IAM is not enforced)."

// List implements console.Provider: the project's clusters.
func (p redisClusterProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Location", "Shards", "Replicas", "Node type", "Capacity", "Discovery endpoint"},
		NameColumn: "Cluster ID", Noun: "clusters", Note: redisClusterNote}
	api, _, err := p.api()
	if err != nil {
		l.Unavailable = err.Error()
		return l, nil
	}
	resp, err := api.ListClusters(ctx, &clusterpb.ListClustersRequest{Parent: "projects/" + project + "/locations/-", PageSize: 1000})
	if err != nil {
		l.Unavailable = "cannot reach Memorystore: " + err.Error()
		return l, nil
	}
	for _, c := range resp.GetClusters() {
		l.Items = append(l.Items, console.Resource{Name: memorystoreID(c.GetName()), Status: c.GetState().String(),
			Fields: map[string]string{
				"Location":           strings.Split(c.GetName(), "/")[3],
				"Discovery endpoint": redisClusterEndpoint(c),
				"Shards":             strconv.Itoa(int(c.GetShardCount())),
				"Replicas":           strconv.Itoa(int(c.GetReplicaCount())),
				"Node type":          memorystoreOption(redisClusterNodeTypeLabels, c.GetNodeType().String()),
				"Capacity":           fmt.Sprintf("%g GB", c.GetPreciseSizeGb()),
			}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func redisClusterEndpoint(c *clusterpb.Cluster) string {
	if eps := c.GetDiscoveryEndpoints(); len(eps) > 0 {
		return net.JoinHostPort(eps[0].GetAddress(), strconv.Itoa(int(eps[0].GetPort())))
	}
	return "—"
}

// redisClusterName is the resource name of the console's name, region/ID
// or ID in the default region.
func redisClusterName(project, id string) (string, error) {
	name, err := memorystoreName(project, id)
	if err != nil {
		return "", errors.New("no such cluster: " + id)
	}
	return strings.Replace(name, "/instances/", "/clusters/", 1), nil
}

var redisClusterNodeTypes = []string{"REDIS_SHARED_CORE_NANO", "REDIS_STANDARD_SMALL", "REDIS_HIGHMEM_MEDIUM", "REDIS_HIGHMEM_XLARGE"}

// redisClusterNodeTypeLabels are the node types as Google's page names them.
var redisClusterNodeTypeLabels = map[string]string{"REDIS_SHARED_CORE_NANO": "redis-shared-core-nano",
	"REDIS_STANDARD_SMALL": "redis-standard-small", "REDIS_HIGHMEM_MEDIUM": "redis-highmem-medium", "REDIS_HIGHMEM_XLARGE": "redis-highmem-xlarge"}

// CreateForm implements console.Creator: Google's Create cluster page, in
// its order (#1862): Cluster ID, Region, Zonal availability, Node type,
// Cluster size, Replicas (default 1), Set up connection, Security,
// Certificate authority management, Encryption, ACL policy, Data
// persistence strategy, Automated backups and Maintenance, then this
// console's advanced options (Redis configs, Restore from backup,
// Secondary of).
func (redisClusterProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "id", Label: "Cluster ID", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9\-]{2,61}[a-z0-9]$`,
			Help: "4 to 63 lowercase letters, digits and hyphens, starting with a letter; unique in its region."},
		{Name: "region", Label: "Region", Type: "text", Required: true, Default: redisadmin.Region},
	}
	fields = append(fields, memorystoreZonalFields()...)
	fields = append(fields,
		console.Field{Name: "nodeType", Label: "Node type", Type: "select", Default: "REDIS_HIGHMEM_MEDIUM", Options: redisClusterNodeTypes,
			OptionLabels: redisClusterNodeTypeLabels,
			Help:         "Each node's maxmemory: 1.4, 6.5, 13 or 58 GB. redis-shared-core-nano has no SLA on Google: for development and testing."},
		console.Field{Name: "shardCount", Label: "Cluster size", Type: "number", Required: true, Default: "3", Pattern: `^[0-9]{1,3}$`,
			Help: "The number of shards (primaries the 16384 slots are spread over), which sets the cluster's capacity."},
		console.Field{Name: "replicaCount", Label: "Replicas", Type: "number", Default: "1", Pattern: `^[0-5]$`, Help: "0 to 5 replicas per shard."},
		console.Field{Name: "network", Label: "Network", Type: "text", Section: msSectionConnection,
			Help: "Optional. projects/{project}/global/networks/{network}: the network your applications connect from (PSC), kept and returned."},
	)
	fields = append(fields, memorystoreSecurityFields("cluster")...)
	fields = append(fields, memorystoreEncryptionFields("cluster")...)
	fields = append(fields,
		console.Field{Name: "aclPolicy", Label: "Policy", Type: "text", Section: msSectionACL,
			Help: "Optional. projects/{project}/locations/{location}/aclPolicies/{policy}, in the cluster's region: its rules become ACL users on every node."},
		memorystorePersistenceField("Redis Database (RDB)"))
	fields = append(fields, memorystoreBackupFields()...)
	fields = append(fields, memorystoreMaintenanceFields()...)
	fields = append(fields,
		console.Field{Name: "redisConfigs", Label: "Redis configs", Type: "map", Section: msSectionAdvanced,
			Help: "Optional. One key=value per line, such as maxmemory-policy."},
		console.Field{Name: "restoreBackup", Label: "Restore from backup", Type: "text", Section: msSectionAdvanced,
			Help: "Optional. projects/{project}/locations/{location}/backupCollections/{collection}/backups/{backup}: the cluster starts with its keys."},
		redisClusterSecondaryField)
	return "Create cluster", fields
}

// CreateTitle implements console.CreateTitler.
func (redisClusterProvider) CreateTitle() string { return "Create a Redis cluster" }

// CreateLayout implements console.CreateLayouter: one page under
// headings, as Google's.
func (redisClusterProvider) CreateLayout() string { return console.CreateLayoutPage }

// CreateOnPage implements console.PageCreator.
func (redisClusterProvider) CreateOnPage() bool { return true }

func atoiField(values map[string]string, k, what string) (int32, error) {
	v := strings.TrimSpace(values[k])
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a number", what, v)
	}
	return int32(n), nil
}

// Create implements console.Creator, waiting for the cluster to form.
func (p redisClusterProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
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
	configs, err := console.ParseMap(values["redisConfigs"])
	if err != nil {
		return "", err
	}
	in := &clusterpb.Cluster{ShardCount: proto.Int32(shards), ReplicaCount: proto.Int32(replicas), RedisConfigs: configs,
		NodeType: clusterpb.NodeType(clusterpb.NodeType_value[values["nodeType"]])}
	if memorystoreTLS(values) {
		in.TransitEncryptionMode = clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION
	}
	if mode, pool := memorystoreCAMode(values); mode != 0 {
		in.ServerCaMode = clusterpb.ServerCaMode(mode).Enum()
		if pool != "" {
			in.ServerCaPool = proto.String(pool)
		}
	}
	switch memorystoreAuthMode(values) {
	case "IAM_AUTH":
		in.AuthorizationMode = clusterpb.AuthorizationMode_AUTH_MODE_IAM_AUTH
	case "TOKEN_AUTH":
		in.AuthorizationMode = clusterpb.AuthorizationMode_AUTH_MODE_TOKEN_AUTH
	}
	in.AclPolicy = strings.TrimSpace(values["aclPolicy"])
	if k := memorystoreKMSKey(values); k != "" {
		in.KmsKey = proto.String(k)
	}
	if values["zoneDistribution"] == "SINGLE_ZONE" {
		in.ZoneDistributionConfig = &clusterpb.ZoneDistributionConfig{Mode: clusterpb.ZoneDistributionConfig_SINGLE_ZONE,
			Zone: strings.TrimSpace(values["zone"])}
	} else if values["zoneDistribution"] == "MULTI_ZONE" {
		in.ZoneDistributionConfig = &clusterpb.ZoneDistributionConfig{Mode: clusterpb.ZoneDistributionConfig_MULTI_ZONE}
	}
	on, hour, retention, err := memorystoreAutoBackup(values)
	if err != nil {
		return "", err
	}
	if on {
		in.AutomatedBackupConfig = &clusterpb.AutomatedBackupConfig{AutomatedBackupMode: clusterpb.AutomatedBackupConfig_ENABLED,
			Retention: durationpb.New(retention),
			Schedule: &clusterpb.AutomatedBackupConfig_FixedFrequencySchedule_{FixedFrequencySchedule: &clusterpb.AutomatedBackupConfig_FixedFrequencySchedule{
				StartTime: &timeofday.TimeOfDay{Hours: hour}}}}
	}
	day, start, has, err := memorystoreCreateWindow(values)
	if err != nil {
		return "", err
	}
	if has {
		in.MaintenancePolicy = &clusterpb.ClusterMaintenancePolicy{WeeklyMaintenanceWindow: []*clusterpb.ClusterWeeklyMaintenanceWindow{{Day: day, StartTime: start}}}
	}
	if m := values["persistence"]; m != "" && m != "DISABLED" {
		in.PersistenceConfig = &clusterpb.ClusterPersistenceConfig{Mode: clusterpb.ClusterPersistenceConfig_PersistenceMode(clusterpb.ClusterPersistenceConfig_PersistenceMode_value[m])}
	}
	if b := strings.TrimSpace(values["restoreBackup"]); b != "" {
		in.ImportSources = &clusterpb.Cluster_ManagedBackupSource_{ManagedBackupSource: &clusterpb.Cluster_ManagedBackupSource{Backup: b}}
	}
	if n := strings.TrimSpace(values["network"]); n != "" {
		in.PscConfigs = []*clusterpb.PscConfig{{Network: n}}
	}
	redisClusterSecondaryOf(in, values)
	region, id := strings.TrimSpace(values["region"]), strings.TrimSpace(values["id"])
	op, err := api.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: "projects/" + project + "/locations/" + region, ClusterId: id, Cluster: in})
	if err != nil {
		return "", err
	}
	if err := waitRedisOp(ctx, srv, op.GetName()); err != nil {
		return "", err
	}
	return memorystoreID("projects/" + project + "/locations/" + region + "/clusters/" + id), nil
}

// waitRedisOp polls an operation until it is done, and returns its error.
func waitRedisOp(ctx context.Context, srv *redisadmin.Server, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	for {
		op, err := srv.Operation(name)
		if err != nil {
			return err
		}
		if op.GetDone() {
			if e := op.GetError(); e != nil {
				return errors.New(e.GetMessage())
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation %s is still running", name)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Delete implements console.Deleter: DeleteCluster, waited for.
func (p redisClusterProvider) Delete(ctx context.Context, project, id string) error {
	api, srv, err := p.api()
	if err != nil {
		return err
	}
	name, err := redisClusterName(project, id)
	if err != nil {
		return err
	}
	op, err := api.DeleteCluster(ctx, &clusterpb.DeleteClusterRequest{Name: name})
	if err != nil {
		return err
	}
	return waitRedisOp(ctx, srv, op.GetName())
}

// Detail implements console.Detailer: a cluster's properties and its Edit
// form.
func (p redisClusterProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
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
	name, err := redisClusterName(project, strings.Join(path, "/"))
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	c, err := api.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the cluster: " + consoleErr(err)}, nil
	}
	var nets []string
	for _, pc := range c.GetPscConfigs() {
		nets = append(nets, pc.GetNetwork())
	}
	// Google's cluster page (#1862): Overview, Configuration,
	// Connections, the Server CA, Token auth, Backups and ACL policies,
	// Maintenance, and Monitoring, which reads Cloud Monitoring's metrics.
	overview := []console.Property{
		{Label: "State", Value: c.GetState().String()},
		{Label: "Shards", Value: strconv.Itoa(int(c.GetShardCount()))},
		{Label: "Replicas per shard", Value: strconv.Itoa(int(c.GetReplicaCount()))},
		{Label: "Node type", Value: memorystoreOption(redisClusterNodeTypeLabels, c.GetNodeType().String())},
		{Label: "Capacity", Value: fmt.Sprintf("%g GB", c.GetPreciseSizeGb())},
		{Label: "Zonal availability", Value: memorystoreZones(c.GetZoneDistributionConfig().GetMode().String(), c.GetZoneDistributionConfig().GetZone())},
		{Label: "Deletion protection", Value: strconv.FormatBool(c.GetDeletionProtectionEnabled())},
		{Label: "Encryption", Value: memorystoreEncryptionText(c.GetEncryptionInfo().GetEncryptionType().String())},
		{Label: "Customer-managed key", Value: orDash(c.GetKmsKey())},
	}
	config := []console.Property{
		{Label: "Security", Value: memorystoreAuthText(c.GetAuthorizationMode().String())},
		{Label: "In-transit encryption", Value: memorystoreTLSText(c.GetTransitEncryptionMode().String())},
		{Label: "Server CA mode", Value: orDash(memorystoreCAModeText(c.GetServerCaMode().String(), c.ServerCaMode != nil))},
		{Label: "Server CA pool", Value: orDash(c.GetServerCaPool())},
	}
	if c.GetAuthorizationMode() == clusterpb.AuthorizationMode_AUTH_MODE_IAM_AUTH {
		// IAM auth checks the token (#1614): who may connect is the
		// project's IAM policy, granted on the IAM page.
		config = append(config, console.Property{Label: "IAM connect role", Value: redisClusterConnectRole + " on the project, granted on IAM & Admin; a client's AUTH is an access token minted for the principal"})
	}
	ab := c.GetAutomatedBackupConfig()
	config = append(config, []console.Property{
		{Label: "Redis configs", Value: orDash(memorystorePairs(c.GetRedisConfigs()))},
		{Label: "ACL policy", Value: orDash(c.GetAclPolicy())},
		{Label: "Data persistence strategy", Value: memorystorePersistenceText(c.GetPersistenceConfig().GetMode().String(), "Redis Database (RDB)")},
		{Label: "Automated backups", Value: memorystoreAutoBackupText(ab.GetAutomatedBackupMode() == clusterpb.AutomatedBackupConfig_ENABLED,
			ab.GetFixedFrequencySchedule().GetStartTime().GetHours(), ab.GetRetention().AsDuration())},
		{Label: "Backup collection", Value: orDash(c.GetBackupCollection())},
	}...)
	config = append(config, redisClusterReplicationProps(c)...)
	conns := []console.Property{
		{Label: "Discovery endpoint", Value: redisClusterEndpoint(c)},
		{Label: "PSC networks", Value: orDash(strings.Join(nets, ", "))},
	}
	d := console.Detail{Sections: []console.Section{
		memorystorePropsTab("overview", "Overview", overview),
		memorystorePropsTab("configuration", "Configuration", config),
		memorystorePropsTab("connections", "Connections", conns),
	}}
	if c.GetTransitEncryptionMode() == clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION {
		// The CA its clients trust (#1549), as GetClusterCertificateAuthority
		// returns it.
		if ca, err := api.GetClusterCertificateAuthority(ctx, &clusterpb.GetClusterCertificateAuthorityRequest{Name: name + "/certificateAuthority"}); err == nil {
			var text strings.Builder
			for _, chain := range ca.GetManagedServerCa().GetCaCerts() {
				for _, cert := range chain.GetCertificates() {
					text.WriteString(strings.TrimSpace(cert) + "\n")
				}
			}
			d.Sections = append(d.Sections, console.Section{ID: "server-ca", Label: "Server CA", Kind: console.KindText, Text: text.String()})
		}
	}
	d.Sections = append(d.Sections, p.authSections(ctx, api, c)...)
	d.Sections = append(d.Sections, memorystorePropsTab("maintenance", "Maintenance", redisClusterMaintenance(c).summary()), memorystoreMonitoring("cluster"))
	d.Edit = &console.EditForm{
		Label: "Edit", Title: "Edit cluster", Submit: "Save", Page: true,
		Fields: []console.Field{
			{Name: "shardCount", Label: "Shards", Type: "number", Required: true, Pattern: `^[0-9]{1,3}$`,
				Default: strconv.Itoa(int(c.GetShardCount())), Help: "Slots and their keys move to or off the primaries added or removed."},
			{Name: "replicaCount", Label: "Replicas per shard", Type: "number", Required: true, Pattern: `^[0-5]$`,
				Default: strconv.Itoa(int(c.GetReplicaCount()))},
			{Name: "nodeType", Label: "Node type", Type: "select", Default: c.GetNodeType().String(), Options: redisClusterNodeTypes, OptionLabels: redisClusterNodeTypeLabels},
			{Name: "deletionProtection", Label: "Deletion protection", Type: "checkbox", Default: strconv.FormatBool(c.GetDeletionProtectionEnabled())},
		},
		Note: "Saved through UpdateCluster, shards or replicas one change at a time.",
	}
	if c.GetAuthorizationMode() != clusterpb.AuthorizationMode_AUTH_MODE_TOKEN_AUTH {
		// IAM auth on or off (#1614), as Google's console offers it.
		d.Edit.Fields = append(d.Edit.Fields, console.Field{Name: "iamAuth", Label: "IAM authentication", Type: "checkbox",
			Default: strconv.FormatBool(c.GetAuthorizationMode() == clusterpb.AuthorizationMode_AUTH_MODE_IAM_AUTH),
			Help:    "Clients send an access token of a principal granted " + redisClusterConnectRole + " as the AUTH string."})
	}
	return d, nil
}

// Edit implements console.Editor: UpdateCluster of what changed, shards
// and replicas each in an update of its own, as Google requires.
func (p redisClusterProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	api, srv, err := p.api()
	if err != nil {
		return err
	}
	name, err := redisClusterName(project, strings.Join(path, "/"))
	if err != nil {
		return err
	}
	cur, err := api.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: name})
	if err != nil {
		return err
	}
	shards, err := atoiField(values, "shardCount", "shards")
	if err != nil {
		return err
	}
	replicas, err := atoiField(values, "replicaCount", "replicas")
	if err != nil {
		return err
	}
	update := func(path string, in *clusterpb.Cluster) error {
		in.Name = name
		op, err := api.UpdateCluster(ctx, &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{path}}, Cluster: in})
		if err != nil {
			return err
		}
		return waitRedisOp(ctx, srv, op.GetName())
	}
	if nt := values["nodeType"]; nt != "" && nt != cur.GetNodeType().String() {
		if err := update("node_type", &clusterpb.Cluster{NodeType: clusterpb.NodeType(clusterpb.NodeType_value[nt])}); err != nil {
			return err
		}
	}
	if v, ok := values["iamAuth"]; ok && cur.GetAuthorizationMode() != clusterpb.AuthorizationMode_AUTH_MODE_TOKEN_AUTH {
		mode := clusterpb.AuthorizationMode_AUTH_MODE_DISABLED
		if v == "true" {
			mode = clusterpb.AuthorizationMode_AUTH_MODE_IAM_AUTH
		}
		if mode != cur.GetAuthorizationMode() {
			if err := update("authorization_mode", &clusterpb.Cluster{AuthorizationMode: mode}); err != nil {
				return err
			}
		}
	}
	if dp := values["deletionProtection"] == "true"; dp != cur.GetDeletionProtectionEnabled() {
		if err := update("deletion_protection_enabled", &clusterpb.Cluster{DeletionProtectionEnabled: proto.Bool(dp)}); err != nil {
			return err
		}
	}
	if shards != cur.GetShardCount() {
		if err := update("shard_count", &clusterpb.Cluster{ShardCount: proto.Int32(shards)}); err != nil {
			return err
		}
	}
	if replicas != cur.GetReplicaCount() {
		return update("replica_count", &clusterpb.Cluster{ReplicaCount: proto.Int32(replicas)})
	}
	return nil
}
