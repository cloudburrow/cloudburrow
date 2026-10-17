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
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/iam"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

// bigtableInstancesProvider is Bigtable's second screen.
type bigtableInstancesProvider struct{ endpoint string }

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
	base := console.Listing{Columns: []string{"Display name", "Type", "Clusters", "Labels"},
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
		clusters := "?"
		if cs, err := c.Clusters(ctx, in.Name); err == nil {
			clusters = strconv.Itoa(len(cs))
		}
		base.Items = append(base.Items, console.Resource{Name: in.Name, Status: bigtableInstanceState(in.InstanceState),
			Fields: map[string]string{"Display name": in.DisplayName, "Type": bigtableInstanceType(in.InstanceType),
				"Clusters": clusters, "Labels": labelsText(in.Labels)}})
	}
	base.Total = len(base.Items)
	base.Note = fmt.Sprintf("Instances are served by CloudBurrow's Bigtable front; clusters and nodes are stored, "+
		"and every table is served by the one emulator. The Bigtable screen browses the tables of the instance its "+
		"picker chooses: these, or %q, which the emulator serves without it being created. An instance's Tables tab "+
		"links to its tables there. Deleting an instance deletes its tables.", bigtableInstance)
	return base, nil
}

// Detail is an instance's page: its settings, and its Clusters, App
// profiles and Tables tabs. The server adds the Permissions tab.
func (p bigtableInstancesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer c.Close()
	in, err := c.InstanceInfo(ctx, path[0])
	if err != nil {
		return console.Detail{Unavailable: "reading the instance: " + err.Error()}, nil
	}
	summary := []console.Property{
		{Label: "Display name", Value: in.DisplayName},
		{Label: "Type", Value: bigtableInstanceType(in.InstanceType)},
		{Label: "State", Value: bigtableInstanceState(in.InstanceState)},
		{Label: "Labels", Value: labelsText(in.Labels)},
	}
	clusters, cerr := c.Clusters(ctx, path[0])
	return console.Detail{Summary: summary, Sections: []console.Section{
		{ID: "clusters", Label: "Clusters", Listing: clustersListing(path[0], clusters, cerr)},
		{ID: "appprofiles", Label: "App profiles", Listing: p.profilesListing(ctx, c, path[0], clusters)},
		{ID: "tables", Label: "Tables", Listing: p.tablesListing(ctx, project, path[0])},
		{ID: "backups", Label: "Backups", Listing: p.backupsListing(ctx, project, path[0])},
		{ID: "logicalviews", Label: "Logical views", Listing: p.sqlViewsListing(ctx, c, path[0], false)},
		{ID: "materializedviews", Label: "Materialized views", Listing: p.sqlViewsListing(ctx, c, path[0], true)},
	}}, nil
}

// tablesListing is an instance's Tables tab: its tables, each a link to its
// page on the Bigtable screen with the instance chosen there (#1360).
func (p bigtableInstancesProvider) tablesListing(ctx context.Context, project, instance string) console.Listing {
	list := console.Listing{Columns: []string{"Column families"}, NameColumn: "Table", Noun: "tables"}
	admin, err := bigtable.NewAdminClient(ctx, project, instance, localOpts(p.endpoint)...)
	if err != nil {
		list.Unavailable = "cannot reach the Bigtable admin API: " + err.Error()
		return list
	}
	defer admin.Close()
	tables, err := admin.Tables(ctx)
	if err != nil {
		list.Unavailable = "listing tables: " + err.Error()
		return list
	}
	sort.Strings(tables)
	for _, t := range tables {
		families := "—"
		if info, err := admin.TableInfo(ctx, t); err == nil && len(info.FamilyInfos) > 0 {
			names := make([]string, 0, len(info.FamilyInfos))
			for _, f := range info.FamilyInfos {
				names = append(names, f.Name)
			}
			sort.Strings(names)
			families = strings.Join(names, ", ")
		}
		list.Items = append(list.Items, console.Resource{Name: t, Link: bigtableTablesHref(project, instance, t),
			Fields: map[string]string{"Column families": families}})
	}
	list.Total = len(list.Items)
	list.Note = fmt.Sprintf("Each table opens on the Bigtable screen, with instance %q chosen in its picker, "+
		"where its rows, column families and actions are. Create table there makes one in this instance.", instance)
	return list
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

func (p bigtableInstancesProvider) profilesListing(ctx context.Context, c *bigtable.InstanceAdminClient, instance string, clusters []*bigtable.ClusterInfo) console.Listing {
	list := console.Listing{Columns: []string{"Description", "Routing", "Transactional writes"}, NameColumn: "App profile ID",
		Noun: "app profiles"}
	it := c.ListAppProfiles(ctx, instance)
	for {
		ap, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			list.Unavailable = "listing app profiles: " + err.Error()
			return list
		}
		id := lastSegment(ap.Name)
		writes := "No"
		if ap.GetSingleClusterRouting().GetAllowTransactionalWrites() {
			writes = "Yes"
		}
		list.Items = append(list.Items, console.Resource{Name: id,
			Fields:  map[string]string{"Description": ap.Description, "Routing": profileRouting(ap), "Transactional writes": writes},
			Actions: profileActions(ap, clusters), ActsOn: []string{instance, bigtableProfilesSegment, id}})
	}
	list.Total = len(list.Items)
	list.Note = "Routing is stored and returned: the emulator has one copy of every table, so a request is served the same through any profile."
	return list
}

// profileRouting is an app profile's routing policy in words.
func profileRouting(ap *btapb.AppProfile) string {
	if s := ap.GetSingleClusterRouting(); s != nil {
		return "Single cluster: " + s.GetClusterId()
	}
	if m := ap.GetMultiClusterRoutingUseAny(); m != nil {
		if ids := m.GetClusterIds(); len(ids) > 0 {
			return "Multi-cluster: " + strings.Join(ids, ", ")
		}
		return "Multi-cluster: any"
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

// clusterFields are the fields of a new cluster, under section.
func clusterFields(section string) []console.Field {
	return []console.Field{
		{Name: "cluster", Label: "Cluster ID", Type: "text", Required: true, Section: section, Pattern: bigtableClusterIDPattern,
			Help: "6 to 30 lowercase letters, numbers and hyphens, starting with a letter and not ending with a hyphen."},
		{Name: "zone", Label: "Zone", Type: "text", Required: true, Default: "us-central1-b", Section: section, Pattern: `^[a-z][\-a-z0-9]*$`,
			Help: "A Google Cloud zone, such as us-central1-b. Stored; the emulator serves every cluster from this machine."},
		{Name: "storage", Label: "Storage type", Type: "select", Default: "SSD", Options: []string{"SSD", "HDD"}, Section: section},
	}
}

// nodeFields are a cluster's size: a fixed number of nodes, or autoscaling
// limits and a CPU target, prefilled from c (nil for a new cluster).
func nodeFields(c *bigtable.ClusterInfo, section string) []console.Field {
	nodes, auto, min, max, cpu := "1", false, "1", "3", "60"
	if c != nil {
		nodes = strconv.Itoa(c.ServeNodes)
		if a := c.AutoscalingConfig; a != nil {
			auto, min, max, cpu = true, strconv.Itoa(a.MinNodes), strconv.Itoa(a.MaxNodes), strconv.Itoa(a.CPUTargetPercent)
		}
	}
	return []console.Field{
		{Name: "nodes", Label: "Nodes", Type: "text", Default: nodes, Section: section, Pattern: `^[0-9]+$`,
			Help: "The cluster's fixed number of nodes, when it is not autoscaled. A production instance's cluster needs at least 1."},
		{Name: "autoscaling", Label: "Autoscaling", Type: "checkbox", Default: strconv.FormatBool(auto), Section: section,
			Help: "Checked, the cluster is sized between the limits below instead of fixed; stored, as nothing is scaled here."},
		{Name: "minNodes", Label: "Minimum nodes", Type: "text", Default: min, Section: section, Pattern: `^[0-9]+$`},
		{Name: "maxNodes", Label: "Maximum nodes", Type: "text", Default: max, Section: section, Pattern: `^[0-9]+$`},
		{Name: "cpuTarget", Label: "CPU utilization target (%)", Type: "text", Default: cpu, Section: section, Pattern: `^[0-9]+$`,
			Help: "10 to 80."},
	}
}

// formInt reads a whole-number field.
func formInt(values map[string]string, name, label string) (int, error) {
	v := strings.TrimSpace(values[name])
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s %q is not a whole number", label, v)
	}
	return n, nil
}

// formAutoscaling is the autoscaling config the form asks for, or nil.
func formAutoscaling(values map[string]string) (*bigtable.AutoscalingConfig, error) {
	if values["autoscaling"] != "true" {
		return nil, nil
	}
	min, err := formInt(values, "minNodes", "minimum nodes")
	if err != nil {
		return nil, err
	}
	max, err := formInt(values, "maxNodes", "maximum nodes")
	if err != nil {
		return nil, err
	}
	cpu, err := formInt(values, "cpuTarget", "CPU utilization target")
	if err != nil {
		return nil, err
	}
	return &bigtable.AutoscalingConfig{MinNodes: min, MaxNodes: max, CPUTargetPercent: cpu}, nil
}

// formCluster is the cluster the form describes.
func formCluster(instance string, values map[string]string) (*bigtable.ClusterConfig, error) {
	cc := &bigtable.ClusterConfig{InstanceID: instance, ClusterID: strings.TrimSpace(values["cluster"]),
		Zone: strings.TrimSpace(values["zone"]), StorageType: bigtable.SSD}
	switch values["storage"] {
	case "", "SSD":
	case "HDD":
		cc.StorageType = bigtable.HDD
	default:
		return nil, fmt.Errorf("storage type %q is not SSD or HDD", values["storage"])
	}
	auto, err := formAutoscaling(values)
	if err != nil {
		return nil, err
	}
	cc.AutoscalingConfig = auto
	if auto == nil {
		n, err := formInt(values, "nodes", "nodes")
		if err != nil {
			return nil, err
		}
		cc.NumNodes = int32(n)
	}
	return cc, nil
}

// CreateForm implements console.Creator: Create instance, with its first
// cluster.
func (bigtableInstancesProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "instance", Label: "Instance ID", Type: "text", Required: true, Section: "Instance", Pattern: bigtableInstanceIDPattern,
			Help: "6 to 33 lowercase letters, numbers and hyphens, starting with a letter and not ending with a hyphen."},
		{Name: "displayName", Label: "Display name", Type: "text", Required: true, Section: "Instance", Pattern: `^.{4,30}$`,
			Help: "4 to 30 characters, shown in place of the ID."},
		{Name: "type", Label: "Instance type", Type: "select", Default: "PRODUCTION", Options: []string{"PRODUCTION", "DEVELOPMENT"},
			Section: "Instance", Help: "A development instance's cluster has no node count. A production instance cannot become a development one."},
	}
	labels := bigtableLabelsField(nil)
	labels.Section = "Instance"
	fields = append(fields, labels)
	fields = append(fields, clusterFields("First cluster")...)
	return "Create instance", append(fields, nodeFields(nil, "First cluster")...)
}

// CreateOnPage implements console.PageCreator.
func (bigtableInstancesProvider) CreateOnPage() bool { return true }

// Create implements console.Creator through CreateInstanceWithClusters.
func (p bigtableInstancesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return "", err
	}
	defer c.Close()
	id := strings.TrimSpace(values["instance"])
	conf := &bigtable.InstanceWithClustersConfig{InstanceID: id, DisplayName: strings.TrimSpace(values["displayName"])}
	switch values["type"] {
	case "", "PRODUCTION":
		conf.InstanceType = bigtable.PRODUCTION
	case "DEVELOPMENT":
		conf.InstanceType = bigtable.DEVELOPMENT
	default:
		return "", fmt.Errorf("instance type %q is not PRODUCTION or DEVELOPMENT", values["type"])
	}
	if conf.Labels, err = console.ParseMap(values["labels"]); err != nil {
		return "", fmt.Errorf("labels: %w", err)
	}
	cc, err := formCluster(id, values)
	if err != nil {
		return "", err
	}
	conf.Clusters = []bigtable.ClusterConfig{*cc}
	if err := c.CreateInstanceWithClusters(ctx, conf); err != nil {
		return "", fmt.Errorf("creating the instance: %w", err)
	}
	return id, nil
}

// Delete implements console.Deleter: the instance, its clusters, app
// profiles and tables.
func (p bigtableInstancesProvider) Delete(ctx context.Context, project, name string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.DeleteInstance(ctx, name)
}

// instanceEditFields are Edit instance's inputs, prefilled from in.
func instanceEditFields(in *bigtable.InstanceInfo) []console.Field {
	types := []string{"PRODUCTION"}
	def := "PRODUCTION"
	if in.InstanceType == bigtable.DEVELOPMENT {
		types, def = []string{"DEVELOPMENT", "PRODUCTION"}, "DEVELOPMENT"
	}
	return []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Required: true, Default: in.DisplayName, Pattern: `^.{4,30}$`,
			Help: "4 to 30 characters."},
		{Name: "type", Label: "Instance type", Type: "select", Default: def, Options: types,
			Help: "A development instance can be upgraded to production; a production one cannot go back."},
		bigtableLabelsField(in.Labels),
	}
}

// profileIDField is Create app profile's ID.
func profileIDField() console.Field {
	return console.Field{Name: "profile", Label: "App profile ID", Type: "text", Required: true,
		Pattern: bigtableProfileIDPattern, Help: "1 to 50 letters, numbers, -, _ and ., not starting with - or ."}
}

func profileFields(ap *btapb.AppProfile, clusters []*bigtable.ClusterInfo) []console.Field {
	ids := make([]string, 0, len(clusters))
	for _, c := range clusters {
		ids = append(ids, c.Name)
	}
	routing, cluster, writes, desc := "MULTI_CLUSTER", "", false, ""
	if ap != nil {
		desc = ap.Description
		if s := ap.GetSingleClusterRouting(); s != nil {
			routing, cluster, writes = "SINGLE_CLUSTER", s.GetClusterId(), s.GetAllowTransactionalWrites()
		}
	}
	if cluster == "" && len(ids) > 0 {
		cluster = ids[0]
	}
	return []console.Field{
		{Name: "description", Label: "Description", Type: "text", Default: desc},
		{Name: "routing", Label: "Routing", Type: "select", Default: routing, Options: []string{"MULTI_CLUSTER", "SINGLE_CLUSTER"},
			Help: "MULTI_CLUSTER routes to any of the instance's clusters; SINGLE_CLUSTER to the one below."},
		{Name: "routeTo", Label: "Cluster (single-cluster routing)", Type: "select", Default: cluster, Options: ids},
		{Name: "transactionalWrites", Label: "Allow single-row transactions (single-cluster routing)", Type: "checkbox",
			Default: strconv.FormatBool(writes), Help: "CheckAndMutateRow and ReadModifyWriteRow through this profile."},
	}
}

// formRouting is the routing policy the form asks for.
func formRouting(values map[string]string) (bigtable.RoutingPolicyConfig, error) {
	switch values["routing"] {
	case "", "MULTI_CLUSTER":
		return &bigtable.MultiClusterRoutingUseAnyConfig{}, nil
	case "SINGLE_CLUSTER":
		id := strings.TrimSpace(values["routeTo"])
		if id == "" {
			return nil, errors.New("single-cluster routing needs a cluster")
		}
		return &bigtable.SingleClusterRoutingConfig{ClusterID: id, AllowTransactionalWrites: values["transactionalWrites"] == "true"}, nil
	}
	return nil, fmt.Errorf("routing %q is not MULTI_CLUSTER or SINGLE_CLUSTER", values["routing"])
}

func clusterActions(c *bigtable.ClusterInfo) []console.Action {
	return []console.Action{
		{ID: "editnodes", Label: "Edit nodes", Fields: nodeFields(c, "")},
		{ID: "deletecluster", Label: "Delete cluster", Destructive: true,
			Confirm: "The cluster is removed from the instance. Its tables stay: they belong to the instance."},
	}
}

func profileActions(ap *btapb.AppProfile, clusters []*bigtable.ClusterInfo) []console.Action {
	return []console.Action{
		{ID: "editprofile", Label: "Edit app profile", Fields: profileFields(ap, clusters)},
		{ID: "deleteprofile", Label: "Delete app profile", Destructive: true,
			Confirm: "A client that names this app profile is refused once it is gone."},
	}
}

// DetailActions implements console.PathActor.
func (p bigtableInstancesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if project == "" || (len(path) != 1 && len(path) != 3) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return nil
	}
	defer c.Close()
	in, err := c.InstanceInfo(ctx, path[0])
	if err != nil {
		return nil
	}
	clusters, err := c.Clusters(ctx, path[0])
	if err != nil {
		return nil
	}
	if len(path) == 1 {
		add := append(clusterFields(""), nodeFields(nil, "")...)
		create := append([]console.Field{profileIDField()}, profileFields(nil, clusters)...)
		return append([]console.Action{
			{ID: "editinstance", Label: "Edit instance", Fields: instanceEditFields(in)},
			{ID: "addcluster", Label: "Add cluster", Fields: add},
			{ID: "createprofile", Label: "Create app profile", Fields: create},
		}, append(instanceExtraActions(),
			console.Action{ID: "deleteinstance", Label: "Delete instance", Destructive: true, Leaves: true,
				Confirm: "The instance is deleted with its clusters, app profiles and every table in it."})...)
	}
	switch path[1] {
	case bigtableBackupsSegment:
		return backupActions()
	case bigtableLogicalSegment, bigtableMaterializedSegment:
		return sqlViewActions()
	case bigtableClustersSegment:
		for _, cl := range clusters {
			if cl.Name == path[2] {
				return append(clusterActions(cl), memoryLayerAction(memoryLayerEnabled(ctx, c, "projects/"+project+"/instances/"+path[0]+"/clusters/"+cl.Name)))
			}
		}
	case bigtableProfilesSegment:
		ap, err := c.GetAppProfile(ctx, path[0], path[2])
		if err == nil {
			return profileActions(ap, clusters)
		}
	}
	return nil
}

// ActAt implements console.PathActor.
func (p bigtableInstancesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	if ok, err := p.actInstanceExtra(ctx, c, project, path, action, values); ok {
		return err
	}
	inst := path[0]
	name := "projects/" + project + "/instances/" + inst
	switch {
	case len(path) == 1 && action == "editinstance":
		labels, err := console.ParseMap(values["labels"])
		if err != nil {
			return fmt.Errorf("labels: %w", err)
		}
		typ, ok := btapb.Instance_Type_value[values["type"]]
		if !ok || typ == 0 {
			return fmt.Errorf("instance type %q is not PRODUCTION or DEVELOPMENT", values["type"])
		}
		// UpdateInstance replaces the display name, type and labels, which
		// is what the form shows.
		_, err = c.InstanceAdminClientV2().UpdateInstance(ctx, &btapb.Instance{Name: name,
			DisplayName: strings.TrimSpace(values["displayName"]), Type: btapb.Instance_Type(typ), Labels: labels})
		return err
	case len(path) == 1 && action == "addcluster":
		cc, err := formCluster(inst, values)
		if err != nil {
			return err
		}
		return c.CreateCluster(ctx, cc)
	case len(path) == 1 && action == "createprofile":
		routing, err := formRouting(values)
		if err != nil {
			return err
		}
		_, err = c.CreateAppProfile(ctx, bigtable.ProfileConf{InstanceID: inst, ProfileID: strings.TrimSpace(values["profile"]),
			Description: values["description"], RoutingConfig: routing})
		return err
	case len(path) == 1 && action == "deleteinstance":
		return c.DeleteInstance(ctx, inst)
	case len(path) == 3 && path[1] == bigtableClustersSegment && action == "editnodes":
		return editNodes(ctx, c, name, inst, path[2], values)
	case len(path) == 3 && path[1] == bigtableClustersSegment && action == "deletecluster":
		return c.DeleteCluster(ctx, inst, path[2])
	case len(path) == 3 && path[1] == bigtableProfilesSegment && action == "editprofile":
		routing, err := formRouting(values)
		if err != nil {
			return err
		}
		return c.UpdateAppProfile(ctx, inst, path[2], bigtable.ProfileAttrsToUpdate{Description: values["description"],
			RoutingConfig: routing})
	case len(path) == 3 && path[1] == bigtableProfilesSegment && action == "deleteprofile":
		return c.DeleteAppProfile(ctx, inst, path[2])
	}
	return fmt.Errorf("%s is not available on this resource", action)
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

// Permissions: an instance's page, through the client's instance IAM
// handle. Clusters and app profiles have no policy of their own.

func (bigtableInstancesProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "optional-services",
		RoleHelp: roleHelp("Bigtable", "roles/bigtable.user")}
}

func (p bigtableInstancesProvider) instanceIAM(ctx context.Context, project string, path []string) (*iam.Handle, func(), error) {
	c, err := p.client(ctx, project)
	if err != nil {
		return nil, nil, err
	}
	return c.InstanceIAM(path[0]), func() { _ = c.Close() }, nil
}

func (p bigtableInstancesProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	h, done, err := p.instanceIAM(ctx, project, path)
	if err != nil {
		return console.Policy{}, err
	}
	defer done()
	pol, err := h.Policy(ctx)
	if err != nil {
		return console.Policy{}, err
	}
	return policyFromProto(pol.InternalProto), nil
}

func (p bigtableInstancesProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	h, done, err := p.instanceIAM(ctx, project, path)
	if err != nil {
		return err
	}
	defer done()
	etag, err := protoEtag(pol)
	if err != nil {
		return err
	}
	proto, err := policyToProto(pol, etag)
	if err != nil {
		return err
	}
	return h.SetPolicy(ctx, &iam.Policy{InternalProto: proto})
}

func (p bigtableInstancesProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) ([]string, error) {
	h, done, err := p.instanceIAM(ctx, project, path)
	if err != nil {
		return nil, err
	}
	defer done()
	return h.TestPermissions(ctx, perms)
}

var (
	_ console.PageCreator  = bigtableInstancesProvider{}
	_ console.Deleter      = bigtableInstancesProvider{}
	_ console.Driller      = bigtableInstancesProvider{}
	_ console.PathActor    = bigtableInstancesProvider{}
	_ console.PolicyEditor = bigtableInstancesProvider{}
)
