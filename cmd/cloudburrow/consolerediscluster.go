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
	"google.golang.org/protobuf/proto"
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

func (p redisClusterProvider) api() (*redisadmin.ClusterServer, *redisadmin.Server, error) {
	if p.svc == nil || p.svc.api == nil {
		return nil, nil, errors.New("the Memorystore admin API has not started")
	}
	return redisadmin.NewClusterServer(p.svc.api), p.svc.api, nil
}

const redisClusterNote = "Each cluster is a real Redis Cluster: shard_count primaries with replica_count replicas each, every node a Valkey server in cluster mode. " +
	"A cluster client on the host starts from the discovery endpoint and follows MOVED to the nodes; with in-transit encryption it trusts the cluster's own CA, which its page shows. IAM auth takes any access token as the AUTH string (IAM is not enforced)."

// List implements console.Provider: the project's clusters.
func (p redisClusterProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Location", "Discovery endpoint", "Shards", "Replicas", "Node type", "Capacity"},
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
				"Node type":          c.GetNodeType().String(),
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

// CreateForm implements console.Creator.
func (redisClusterProvider) CreateForm() (string, []console.Field) {
	return "Create cluster", []console.Field{
		{Name: "id", Label: "Cluster ID", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9\-]{2,61}[a-z0-9]$`,
			Help: "4 to 63 lowercase letters, digits and hyphens, starting with a letter."},
		{Name: "region", Label: "Region", Type: "text", Required: true, Default: redisadmin.Region},
		{Name: "shardCount", Label: "Shards", Type: "number", Required: true, Default: "3", Pattern: `^[0-9]{1,3}$`,
			Help: "Primaries the 16384 slots are spread over."},
		{Name: "replicaCount", Label: "Replicas per shard", Type: "number", Default: "0", Pattern: `^[0-5]$`},
		{Name: "nodeType", Label: "Node type", Type: "select", Default: "REDIS_HIGHMEM_MEDIUM", Options: redisClusterNodeTypes,
			Help: "Each node's maxmemory: 1.4, 6.5, 13 or 58 GB."},
		{Name: "network", Label: "Network (PSC)", Type: "text", Help: "Optional. projects/{project}/global/networks/{network}, kept and returned."},
		{Name: "redisConfigs", Label: "Redis configs", Type: "map", Help: "Optional. One key=value per line, such as maxmemory-policy."},
		{Name: "transitEncryption", Label: "Enable in-transit encryption", Type: "checkbox",
			Help: "SERVER_AUTHENTICATION: every node serves TLS only, with a certificate of the cluster's own CA, which its page shows. It cannot be changed later."},
		{Name: "iamAuth", Label: "IAM authentication", Type: "checkbox",
			Help: "AUTH_MODE_IAM_AUTH: clients send an access token as the AUTH string. IAM is not enforced here, so any token is taken."},
		{Name: "tokenAuth", Label: "Token authentication", Type: "checkbox",
			Help: "AUTH_MODE_TOKEN_AUTH: clients log in as a token auth user with a token the service generates, which the cluster's page adds and shows."},
		{Name: "aclPolicy", Label: "ACL policy", Type: "text", Help: "Optional. projects/{project}/locations/{location}/aclPolicies/{policy}: its rules become ACL users on every node."},
		{Name: "restoreBackup", Label: "Restore from backup", Type: "text",
			Help: "Optional. projects/{project}/locations/{location}/backupCollections/{collection}/backups/{backup}: the cluster starts with its keys."},
	}
}

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
	if values["transitEncryption"] == "true" {
		in.TransitEncryptionMode = clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION
	}
	if values["iamAuth"] == "true" {
		in.AuthorizationMode = clusterpb.AuthorizationMode_AUTH_MODE_IAM_AUTH
	}
	if values["tokenAuth"] == "true" {
		in.AuthorizationMode = clusterpb.AuthorizationMode_AUTH_MODE_TOKEN_AUTH
	}
	in.AclPolicy = strings.TrimSpace(values["aclPolicy"])
	if b := strings.TrimSpace(values["restoreBackup"]); b != "" {
		in.ImportSources = &clusterpb.Cluster_ManagedBackupSource_{ManagedBackupSource: &clusterpb.Cluster_ManagedBackupSource{Backup: b}}
	}
	if n := strings.TrimSpace(values["network"]); n != "" {
		in.PscConfigs = []*clusterpb.PscConfig{{Network: n}}
	}
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
	props := []console.Property{
		{Label: "State", Value: c.GetState().String()},
		{Label: "Discovery endpoint", Value: redisClusterEndpoint(c)},
		{Label: "Shards", Value: strconv.Itoa(int(c.GetShardCount()))},
		{Label: "Replicas per shard", Value: strconv.Itoa(int(c.GetReplicaCount()))},
		{Label: "Node type", Value: c.GetNodeType().String()},
		{Label: "Capacity", Value: fmt.Sprintf("%g GB", c.GetPreciseSizeGb())},
		{Label: "Authorization", Value: c.GetAuthorizationMode().String()},
		{Label: "In-transit encryption", Value: c.GetTransitEncryptionMode().String()},
		{Label: "PSC networks", Value: orDash(strings.Join(nets, ", "))},
		{Label: "Redis configs", Value: orDash(memorystorePairs(c.GetRedisConfigs()))},
		{Label: "Deletion protection", Value: strconv.FormatBool(c.GetDeletionProtectionEnabled())},
		{Label: "ACL policy", Value: orDash(c.GetAclPolicy())},
		{Label: "Backup collection", Value: orDash(c.GetBackupCollection())},
	}
	d := console.Detail{Sections: []console.Section{{ID: "config", Label: "Configuration", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{{Properties: props}}}}}
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
	d.Edit = &console.EditForm{
		Label: "Edit cluster",
		Fields: []console.Field{
			{Name: "shardCount", Label: "Shards", Type: "number", Required: true, Pattern: `^[0-9]{1,3}$`,
				Default: strconv.Itoa(int(c.GetShardCount())), Help: "Slots and their keys move to or off the primaries added or removed."},
			{Name: "replicaCount", Label: "Replicas per shard", Type: "number", Required: true, Pattern: `^[0-5]$`,
				Default: strconv.Itoa(int(c.GetReplicaCount()))},
			{Name: "nodeType", Label: "Node type", Type: "select", Default: c.GetNodeType().String(), Options: redisClusterNodeTypes},
			{Name: "deletionProtection", Label: "Deletion protection", Type: "checkbox", Default: strconv.FormatBool(c.GetDeletionProtectionEnabled())},
		},
		Note: "Saved through UpdateCluster, shards or replicas one change at a time.",
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
