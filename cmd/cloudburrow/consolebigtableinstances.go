package main

// Bigtable instances (#1358): the console's surface for the instance admin
// API CloudBurrow's Bigtable front serves (#1331, internal/bigtablefront).
//
// The screen lists the project's instances, with Create instance (its ID,
// display name, type, labels and a first cluster) and Delete instance, which
// deletes its tables. An instance's page shows its settings and its Clusters
// and App profiles tabs, and a Permissions tab through the instance's IAM
// policy, stored and not enforced (ADR-0006):
//
//   - the page: Edit instance (UpdateInstance), Add cluster (CreateCluster),
//     Create app profile (CreateAppProfile) and Delete instance;
//   - a cluster's row, addressed [instance, "clusters", cluster]: Edit nodes
//     (UpdateCluster for a fixed size, PartialUpdateCluster to turn
//     autoscaling on, change it or turn it off) and Delete cluster;
//   - an app profile's row, addressed [instance, "appProfiles", profile]:
//     Edit app profile (UpdateAppProfile) and Delete app profile.
//
// Every read and every change goes through the official InstanceAdminClient,
// or the generated client it carries (InstanceAdminClientV2) for the two
// calls the handwritten one does not make: UpdateInstance and UpdateCluster.
// A refusal is the front's own message, as Google's would be the API's.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/bigtable"
	btadmin "cloud.google.com/go/bigtable/admin/apiv2"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

// bigtableInstancesProvider is Bigtable's second screen.
type bigtableInstancesProvider struct {
	endpoint string
	// savedQueries is the record of Bigtable Studio's saved queries in the
	// instance's state directory (bigtableSavedQueriesFile, #1858).
	savedQueries string
}

func (bigtableInstancesProvider) ID() string    { return "bigtable-instances" }
func (bigtableInstancesProvider) Title() string { return "Bigtable instances" }

// The second segment of a cluster's and an app profile's action path.
const (
	bigtableClustersSegment = "clusters"
	bigtableProfilesSegment = "appProfiles"
)

// The IDs the API accepts, as the front holds it to them: the browser
// compiles a pattern with the v flag, so a hyphen in a class is escaped.
const (
	bigtableInstanceIDPattern = `^[a-z][\-a-z0-9]{4,31}[a-z0-9]$`
	bigtableClusterIDPattern  = `^[a-z][\-a-z0-9]{4,28}[a-z0-9]$`
	bigtableProfileIDPattern  = `^[_a-zA-Z0-9][\-_.a-zA-Z0-9]{0,49}$`
)

func (p bigtableInstancesProvider) client(ctx context.Context, project string) (*bigtable.InstanceAdminClient, error) {
	if project == "" {
		return nil, errors.New("choose a project first")
	}
	c, err := bigtable.NewInstanceAdminClient(ctx, project, localOpts(p.endpoint)...)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the Bigtable instance admin API: %w", err)
	}
	return c, nil
}

func bigtableInstanceType(t bigtable.InstanceType) string {
	if t == bigtable.DEVELOPMENT {
		return "Development"
	}
	return "Production"
}

func bigtableInstanceState(s bigtable.InstanceState) string {
	switch s {
	case bigtable.Ready:
		return "Ready"
	case bigtable.Creating:
		return "Creating"
	}
	return "Unknown"
}

func bigtableStorage(s bigtable.StorageType) string {
	if s == bigtable.HDD {
		return "HDD"
	}
	return "SSD"
}

// labelsText is labels as key=value pairs, in key order.
func labelsText(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+l[k])
	}
	return strings.Join(out, ", ")
}

// clusterNodes is a cluster's size as Google's console words it.
func clusterNodes(c *bigtable.ClusterInfo) string {
	if a := c.AutoscalingConfig; a != nil {
		return fmt.Sprintf("%d (autoscaling %d to %d, CPU target %d%%)", c.ServeNodes, a.MinNodes, a.MaxNodes, a.CPUTargetPercent)
	}
	return strconv.Itoa(c.ServeNodes)
}

func (p bigtableInstancesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	// Google's Instances list (#1858): the ID links, then the name, the
	// edition, the storage type, the clusters and nodes, and the labels.
	base := console.Listing{Columns: []string{"Instance name", "Edition", "Storage type", "Clusters", "Nodes", "Labels"},
		NameColumn: "Instance ID", Noun: "instances", RowsOpenable: true, AlwaysStatus: true}
	if project == "" {
		base.Prompt = "Bigtable holds instances per project. Choose one in the toolbar."
		return base, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		base.Unavailable = err.Error()
		return base, nil
	}
	defer c.Close()
	list, err := c.Instances(ctx)
	if err != nil {
		base.Unavailable = "listing instances: " + err.Error()
		return base, nil
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	for _, in := range list {
		clusters, nodes, storage := "?", "?", "—"
		if cs, err := c.Clusters(ctx, in.Name); err == nil {
			n := 0
			for _, cl := range cs {
				n += cl.ServeNodes
			}
			clusters, nodes = strconv.Itoa(len(cs)), strconv.Itoa(n)
			if len(cs) > 0 {
				storage = bigtableStorage(cs[0].StorageType)
			}
		}
		base.Items = append(base.Items, console.Resource{Name: in.Name, Status: bigtableInstanceState(in.InstanceState),
			Fields: map[string]string{"Instance name": in.DisplayName, "Edition": bigtableEdition(in.Edition), "Storage type": storage,
				"Clusters": clusters, "Nodes": nodes, "Labels": orDash(labelsText(in.Labels))}})
	}
	base.Total = len(base.Items)
	base.Note = fmt.Sprintf("Instances are served by CloudBurrow's Bigtable front; editions, clusters and nodes are stored, "+
		"and every table is served by the one emulator. An instance's Tables page lists its tables; %q, which the emulator "+
		"serves without it being created, is browsed on the Tables page of this menu. Deleting an instance deletes its tables.", bigtableInstance)
	return base, nil
}

func clustersListing(instance string, clusters []*bigtable.ClusterInfo, err error) console.Listing {
	list := console.Listing{Columns: []string{"Zone", "Nodes", "Storage type"}, NameColumn: "Cluster ID",
		Noun: "clusters", AlwaysStatus: true}
	if err != nil {
		list.Unavailable = "listing clusters: " + err.Error()
		return list
	}
	for _, cl := range clusters {
		list.Items = append(list.Items, console.Resource{Name: cl.Name, Status: enumWord(stringer(cl.State), ""),
			Fields:  map[string]string{"Zone": cl.Zone, "Nodes": clusterNodes(cl), "Storage type": bigtableStorage(cl.StorageType)},
			Actions: clusterActions(cl), ActsOn: []string{instance, bigtableClustersSegment, cl.Name}})
	}
	list.Total = len(list.Items)
	list.Note = "Nodes and autoscaling are stored as the API returns them; the emulator serves every table the same " +
		"whatever the size. A cluster is refused deletion while it is the instance's last, or an app profile routes to it."
	return list
}

// stringer lets enumWord word a plain string.
type stringer string

func (s stringer) String() string { return string(s) }

// profileRouting is an app profile's routing policy in words.
func profileRouting(ap *btapb.AppProfile) string {
	if s := ap.GetSingleClusterRouting(); s != nil {
		return "Single-cluster routing: " + s.GetClusterId()
	}
	if m := ap.GetMultiClusterRoutingUseAny(); m != nil {
		if ids := m.GetClusterIds(); len(ids) > 0 {
			return "Multi-cluster routing: " + strings.Join(ids, ", ")
		}
		return "Multi-cluster routing: Any cluster"
	}
	return "None"
}

// bigtableLabelsField is the labels field, held to Google's label rules.
func bigtableLabelsField(l map[string]string) console.Field {
	return console.Field{Name: "labels", Label: "Labels", Type: "map", Default: console.FormatMap(l),
		KeyPattern: pubsubfront.LabelKeyPattern, ValuePattern: pubsubfront.LabelValuePattern,
		KeyHelp:    "A key is 1 to 63 lowercase letters, international characters, digits, _ and -, starting with a lowercase letter or an international character.",
		ValueHelp:  "A value is 0 to 63 lowercase letters, international characters, digits, _ and -.",
		MaxEntries: pubsubfront.MaxLabels}
}

// editNodes is Edit nodes: SetAutoscaling (PartialUpdateCluster) to turn
// autoscaling on or change it; UpdateCluster for a new fixed size of a
// cluster that is not autoscaled, and the client's UpdateCluster
// (PartialUpdateCluster with autoscaling cleared) for one that was.
func editNodes(ctx context.Context, c *bigtable.InstanceAdminClient, instanceName, inst, cluster string, values map[string]string) error {
	auto, err := formAutoscaling(values)
	if err != nil {
		return err
	}
	if auto != nil {
		return c.SetAutoscaling(ctx, inst, cluster, *auto)
	}
	n, err := formInt(values, "nodes", "nodes")
	if err != nil {
		return err
	}
	cur, err := c.GetCluster(ctx, inst, cluster)
	if err != nil {
		return err
	}
	if cur.AutoscalingConfig != nil {
		return c.UpdateCluster(ctx, inst, cluster, int32(n))
	}
	op, err := c.InstanceAdminClientV2().UpdateCluster(ctx, &btapb.Cluster{Name: instanceName + "/clusters/" + cluster, ServeNodes: int32(n)})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	return err
}

// Permissions: an instance's page, through the generated instance admin
// client's GetIamPolicy and SetIamPolicy at version 3 (#1748), so a
// conditional binding is shown and edited with its condition, as on a
// table's page. Clusters and app profiles have no policy of their own.

func (bigtableInstancesProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "optional-services",
		RoleHelp: roleHelp("Bigtable", "roles/bigtable.user"), Conditions: true}
}

// withInstanceIAM calls fn with the instance admin API and the instance's
// full name.
func (p bigtableInstancesProvider) withInstanceIAM(ctx context.Context, project string, path []string, fn func(iamServer, string) error) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	c, err := btadmin.NewBigtableInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return fmt.Errorf("cannot reach the Bigtable instance admin API: %w", err)
	}
	defer c.Close()
	return fn(gaxServer{c}, "projects/"+project+"/instances/"+path[0])
}

func (p bigtableInstancesProvider) GetPolicy(ctx context.Context, project string, path []string) (pol console.Policy, err error) {
	err = p.withInstanceIAM(ctx, project, path, func(api iamServer, name string) error {
		pol, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return pol, err
}

func (p bigtableInstancesProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.withInstanceIAM(ctx, project, path, func(api iamServer, name string) error {
		return setProtoPolicy(ctx, api, name, pol)
	})
}

func (p bigtableInstancesProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) (held []string, err error) {
	err = p.withInstanceIAM(ctx, project, path, func(api iamServer, name string) error {
		held, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return held, err
}

var (
	_ console.PageCreator  = bigtableInstancesProvider{}
	_ console.Deleter      = bigtableInstancesProvider{}
	_ console.Driller      = bigtableInstancesProvider{}
	_ console.PathActor    = bigtableInstancesProvider{}
	_ console.PolicyEditor = bigtableInstancesProvider{}
)
