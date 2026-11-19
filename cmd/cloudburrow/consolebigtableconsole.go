package main

// Bigtable's pages as Google's console has them (#1858).
//
// The Instances list leads to an instance's page, whose left-hand menu
// (Detail.SideNav) holds Google's pages: Overview, Tables, Bigtable Studio,
// Clusters, Application profiles, Backups, Authorized views, Logical views,
// Materialized views, then Monitoring, Key Visualizer and System insights,
// which are Cloud Monitoring's and are not available locally, then
// CloudBurrow's Snapshots and Schema bundles. Create instance is a page of
// Google's steps, each with Continue
// (https://docs.cloud.google.com/bigtable/docs/creating-instance), and
// Create application profile is Google's form
// (https://docs.cloud.google.com/bigtable/docs/configuring-app-profiles).
// Create table, Edit table and Delete table are on the Tables page
// (https://docs.cloud.google.com/bigtable/docs/managing-tables).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigtable"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// bigtableTablesSegment addresses a table's row on an instance's Tables page.
const bigtableTablesSegment = "tables"

// The reasons a Google choice is drawn disabled.
const (
	// Measured 2026-11-19 (bttest, cloud.google.com/go/bigtable v1.58.0):
	// CreateTable keeps no row_key_schema and GetTable returns none.
	bigtableNoRowKeySchema = "Not available locally: the Bigtable emulator keeps no row key schema (CreateTable drops it and GetTable returns none), so SQL could not read a row key by its fields."
	bigtableNoKMS          = "Not available locally: the Bigtable front encrypts nothing with a Cloud KMS key; every instance uses Google-managed encryption."
	bigtableNoInMemory     = "Not available locally: the in-memory tier is turned on per cluster here, with Edit memory layer on the Clusters page; the emulator serves every read from memory either way."
	bigtableNoAutoBkp      = "Not available locally: the Bigtable front takes no automated backups. Create backup on the Backups page takes one."
)

func bigtableEdition(e bigtable.Edition) string {
	if e == bigtable.EnterprisePlus {
		return "Enterprise Plus"
	}
	return "Enterprise"
}

// bigtableRegions are the regions Create instance and Add cluster offer,
// worded as Google's are ("us-central1 (Iowa)").
func bigtableRegions() (ids []string, labels map[string]string) {
	labels = map[string]string{}
	for _, r := range bqRegionNames {
		ids = append(ids, r[0])
		labels[r[0]] = r[0] + " (" + r[1] + ")"
	}
	return ids, labels
}

// clusterFields are a new cluster's ID, region and zone, under section.
func clusterFields(section string) []console.Field {
	regions, labels := bigtableRegions()
	return []console.Field{
		{Name: "cluster", Label: "Cluster ID", Type: "text", Required: true, Section: section, Pattern: bigtableClusterIDPattern,
			Help: "6 to 30 lowercase letters, numbers and hyphens, starting with a letter and not ending with a hyphen. It is permanent."},
		{Name: "region", Label: "Region", Type: "select", Default: "us-central1", Options: regions, OptionLabels: labels, Section: section},
		{Name: "zone", Label: "Zone", Type: "text", Required: true, Default: "us-central1-b", Section: section, Pattern: `^[a-z][\-a-z0-9]*$`,
			Help: "A zone of the region, such as us-central1-b. Stored; the emulator serves every cluster from this machine."},
		{Name: "nodeScaling2x", Label: "Enable 2x node scaling", Type: "checkbox", Section: section,
			Help: "Nodes are added and removed two at a time. Stored, as nothing is scaled here."},
		{Name: "inMemory", Label: "Enable in-memory", Type: "checkbox", Section: section, Unavailable: bigtableNoInMemory},
	}
}

// nodeFields are a cluster's node scaling mode: Manual node allocation (a
// number of nodes) or Autoscaling (its limits and targets), prefilled from c
// (nil for a new cluster).
func nodeFields(c *bigtable.ClusterInfo, section string) []console.Field {
	mode, nodes, min, max, cpu, storage := "MANUAL", "1", "1", "3", "60", ""
	if c != nil {
		nodes = strconv.Itoa(c.ServeNodes)
		if a := c.AutoscalingConfig; a != nil {
			mode, min, max, cpu = "AUTOSCALING", strconv.Itoa(a.MinNodes), strconv.Itoa(a.MaxNodes), strconv.Itoa(a.CPUTargetPercent)
			if a.StorageUtilizationPerNode > 0 {
				storage = strconv.Itoa(a.StorageUtilizationPerNode)
			}
		}
	}
	return []console.Field{
		{Name: "scaling", Label: "Node scaling mode", Type: "radio", Default: mode, Options: []string{"MANUAL", "AUTOSCALING"},
			OptionLabels: map[string]string{"MANUAL": "Manual node allocation", "AUTOSCALING": "Autoscaling"}, Section: section,
			Help: "Stored as the API returns it: the emulator serves every table the same whatever the size."},
		{Name: "nodes", Label: "Number of nodes", Type: "text", Default: nodes, Section: section, Pattern: `^[0-9]+$`, ShowWhen: "scaling=MANUAL"},
		{Name: "minNodes", Label: "Minimum", Type: "text", Default: min, Section: section, Pattern: `^[0-9]+$`, ShowWhen: "scaling=AUTOSCALING",
			Help: "The fewest nodes the cluster scales down to."},
		{Name: "maxNodes", Label: "Maximum", Type: "text", Default: max, Section: section, Pattern: `^[0-9]+$`, ShowWhen: "scaling=AUTOSCALING",
			Help: "The most nodes the cluster scales up to."},
		{Name: "cpuTarget", Label: "CPU utilization target", Type: "text", Default: cpu, Section: section, Pattern: `^[0-9]+$`, ShowWhen: "scaling=AUTOSCALING",
			Help: "A percentage, 10 to 80."},
		{Name: "storageTarget", Label: "Storage utilization target", Type: "text", Default: storage, Section: section, Pattern: `^[0-9]*$`, ShowWhen: "scaling=AUTOSCALING",
			Help: "GiB per node: 2560 to 5120 on SSD, 8192 to 16384 on HDD. Empty for Google's default."},
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

// formAutoscaling is the autoscaling config the form asks for, or nil. The
// earlier form's autoscaling checkbox is still read.
func formAutoscaling(values map[string]string) (*bigtable.AutoscalingConfig, error) {
	switch values["scaling"] {
	case "AUTOSCALING":
	case "", "MANUAL":
		if values["scaling"] == "MANUAL" || values["autoscaling"] != "true" {
			return nil, nil
		}
	default:
		return nil, fmt.Errorf("node scaling mode %q is not MANUAL or AUTOSCALING", values["scaling"])
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
	a := &bigtable.AutoscalingConfig{MinNodes: min, MaxNodes: max, CPUTargetPercent: cpu}
	if strings.TrimSpace(values["storageTarget"]) != "" {
		if a.StorageUtilizationPerNode, err = formInt(values, "storageTarget", "storage utilization target"); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// formCluster is the cluster the form describes.
func formCluster(instance string, values map[string]string) (*bigtable.ClusterConfig, error) {
	cc := &bigtable.ClusterConfig{InstanceID: instance, ClusterID: strings.TrimSpace(values["cluster"]),
		Zone: strings.TrimSpace(values["zone"]), StorageType: bigtable.SSD}
	if r := strings.TrimSpace(values["region"]); r != "" && !strings.HasPrefix(cc.Zone, r+"-") {
		return nil, fmt.Errorf("zone %q is not in region %s: choose one of its zones, such as %s-b", cc.Zone, r, r)
	}
	switch values["storage"] {
	case "", "SSD":
	case "HDD":
		cc.StorageType = bigtable.HDD
	default:
		return nil, fmt.Errorf("storage type %q is not SSD or HDD", values["storage"])
	}
	if values["nodeScaling2x"] == "true" {
		cc.NodeScalingFactor = bigtable.NodeScalingFactor2X
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

// CreateForm implements console.Creator: Create instance, Google's steps.
func (bigtableInstancesProvider) CreateForm() (string, []console.Field) {
	const (
		name    = "Name your instance"
		edition = "Select your edition"
		storage = "Select your storage type"
		cluster = "Configure your first cluster"
		more    = "Encryption, labels and advanced options"
	)
	fields := []console.Field{
		{Name: "displayName", Label: "Instance name", Type: "text", Required: true, Section: name, Pattern: `^.{4,30}$`,
			Help: "4 to 30 characters. The console shows this name to identify the instance."},
		{Name: "instance", Label: "Instance ID", Type: "text", Required: true, Section: name, Pattern: bigtableInstanceIDPattern,
			DeriveFrom: "displayName",
			Help:       "6 to 33 lowercase letters, numbers and hyphens, starting with a letter and not ending with a hyphen. It is permanent."},
		{Name: "edition", Label: "Select your edition", Type: "radio", Section: edition, Default: "ENTERPRISE",
			Options: []string{"ENTERPRISE", "ENTERPRISE_PLUS"}, OptionLabels: map[string]string{"ENTERPRISE": "Enterprise", "ENTERPRISE_PLUS": "Enterprise Plus"},
			Help: "Stored as Google keeps it; the emulator serves both editions alike."},
		{Name: "storage", Label: "Storage type", Type: "radio", Section: storage, Default: "SSD", Options: []string{"SSD", "HDD"},
			Help: "This choice is permanent. Every cluster of the instance uses it."},
	}
	fields = append(fields, clusterFields(cluster)...)
	fields = append(fields, nodeFields(nil, cluster)...)
	labels := bigtableLabelsField(nil)
	labels.Section = more
	// Google's Show advanced options > Add cluster: more clusters made with
	// the instance, each in its own zone, replicated (#1858).
	fields = append(fields, console.Field{Name: "moreClusters", Label: "Additional clusters", Type: console.GroupFieldType,
		Section: more, AddLabel: "Add cluster",
		Help: "Each cluster is in a zone of its own, and replicates the instance's tables. The emulator serves every cluster from this machine.",
		Group: []console.Field{
			{Name: "cluster", Label: "Cluster ID", Type: "text", Required: true, Pattern: bigtableClusterIDPattern},
			{Name: "zone", Label: "Zone", Type: "text", Required: true, Pattern: `^[a-z][\-a-z0-9]*$`},
			{Name: "nodes", Label: "Number of nodes", Type: "text", Pattern: `^[0-9]*$`},
		}})
	return "Create instance", append(fields,
		console.Field{Name: "encryption", Label: "Encryption", Type: "radio", Section: more, Default: "GOOGLE",
			Options:            []string{"GOOGLE", "CMEK"},
			OptionLabels:       map[string]string{"GOOGLE": "Google-managed encryption key", "CMEK": "Cloud KMS key"},
			UnavailableOptions: map[string]string{"CMEK": bigtableNoKMS}},
		labels,
		console.Field{Name: "type", Label: "Instance type", Type: "select", Default: "PRODUCTION", Options: []string{"PRODUCTION", "DEVELOPMENT"},
			OptionLabels: map[string]string{"PRODUCTION": "Production", "DEVELOPMENT": "Development"}, Section: more,
			Help: "Google's console makes production instances; the API still takes a development one, whose cluster has no node count."})
}

// CreateOnPage implements console.PageCreator.
func (bigtableInstancesProvider) CreateOnPage() bool { return true }

// CreateSubmit implements console.CreateSubmitter: the page ends in Create.
func (bigtableInstancesProvider) CreateSubmit() string { return "Create" }

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
	switch values["edition"] {
	case "":
	case "ENTERPRISE":
		conf.Edition = bigtable.Enterprise
	case "ENTERPRISE_PLUS":
		conf.Edition = bigtable.EnterprisePlus
	default:
		return "", fmt.Errorf("edition %q is not ENTERPRISE or ENTERPRISE_PLUS", values["edition"])
	}
	if e := values["encryption"]; e != "" && e != "GOOGLE" {
		return "", errors.New(bigtableNoKMS)
	}
	if values["inMemory"] == "true" {
		return "", errors.New(bigtableNoInMemory)
	}
	if conf.Labels, err = console.ParseMap(values["labels"]); err != nil {
		return "", fmt.Errorf("labels: %w", err)
	}
	cc, err := formCluster(id, values)
	if err != nil {
		return "", err
	}
	conf.Clusters = []bigtable.ClusterConfig{*cc}
	more, err := console.ParseGroup(values["moreClusters"])
	if err != nil {
		return "", fmt.Errorf("additional clusters: %w", err)
	}
	zones := map[string]string{cc.Zone: cc.ClusterID}
	for _, row := range more {
		extra := &bigtable.ClusterConfig{InstanceID: id, ClusterID: strings.TrimSpace(row["cluster"]),
			Zone: strings.TrimSpace(row["zone"]), StorageType: cc.StorageType, NodeScalingFactor: cc.NodeScalingFactor}
		if extra.ClusterID == "" || extra.Zone == "" {
			return "", errors.New("an additional cluster needs a cluster ID and a zone")
		}
		if other, dup := zones[extra.Zone]; dup {
			// Google: "Each cluster must be in a different zone".
			return "", fmt.Errorf("cluster %s is in zone %s, as cluster %s is: each cluster of an instance is in its own zone", extra.ClusterID, extra.Zone, other)
		}
		zones[extra.Zone] = extra.ClusterID
		if conf.InstanceType != bigtable.DEVELOPMENT {
			n := 1
			if v := strings.TrimSpace(row["nodes"]); v != "" {
				if n, err = strconv.Atoi(v); err != nil || n < 1 {
					return "", fmt.Errorf("cluster %s: nodes %q: a whole number, at least 1", extra.ClusterID, v)
				}
			}
			extra.NumNodes = int32(n)
		}
		conf.Clusters = append(conf.Clusters, *extra)
	}
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
	edition := "ENTERPRISE"
	if in.Edition == bigtable.EnterprisePlus {
		edition = "ENTERPRISE_PLUS"
	}
	return []console.Field{
		{Name: "displayName", Label: "Instance name", Type: "text", Required: true, Default: in.DisplayName, Pattern: `^.{4,30}$`,
			Help: "4 to 30 characters."},
		{Name: "edition", Label: "Edition", Type: "radio", Default: edition, Options: []string{"ENTERPRISE", "ENTERPRISE_PLUS"},
			OptionLabels: map[string]string{"ENTERPRISE": "Enterprise", "ENTERPRISE_PLUS": "Enterprise Plus"}},
		{Name: "type", Label: "Instance type", Type: "select", Default: def, Options: types,
			Help: "A development instance can be upgraded to production; a production one cannot go back."},
		bigtableLabelsField(in.Labels),
	}
}

// --- application profiles ---

// profileIDField is Create application profile's ID.
func profileIDField() console.Field {
	return console.Field{Name: "profile", Label: "Application profile ID", Type: "text", Required: true,
		Pattern: bigtableProfileIDPattern, Help: "1 to 50 letters, numbers, -, _ and ., not starting with - or .; it is permanent."}
}

// profileType is an app profile's type in the form's words.
func profileType(ap *btapb.AppProfile) string {
	if ap.GetDataBoostIsolationReadOnly() != nil {
		return "DATA_BOOST"
	}
	return "STANDARD"
}

// profilePriority is a standard profile's routing priority.
func profilePriority(ap *btapb.AppProfile) string {
	p := ap.GetStandardIsolation().GetPriority()
	if p == btapb.AppProfile_PRIORITY_UNSPECIFIED {
		p = ap.GetPriority()
	}
	switch p {
	case btapb.AppProfile_PRIORITY_LOW:
		return "LOW"
	case btapb.AppProfile_PRIORITY_MEDIUM:
		return "MEDIUM"
	}
	return "HIGH"
}

func profileFields(ap *btapb.AppProfile, clusters []*bigtable.ClusterInfo) []console.Field {
	ids := make([]string, 0, len(clusters))
	for _, c := range clusters {
		ids = append(ids, c.Name)
	}
	typ, routing, cluster, group, writes, affinity, priority, desc := "STANDARD", "MULTI_CLUSTER", "", "", false, false, "HIGH", ""
	if ap != nil {
		typ, priority, desc = profileType(ap), profilePriority(ap), ap.Description
		if s := ap.GetSingleClusterRouting(); s != nil {
			routing, cluster, writes = "SINGLE_CLUSTER", s.GetClusterId(), s.GetAllowTransactionalWrites()
		}
		if m := ap.GetMultiClusterRoutingUseAny(); m != nil {
			group, affinity = strings.Join(m.GetClusterIds(), ", "), m.GetRowAffinity() != nil
		}
	}
	if cluster == "" && len(ids) > 0 {
		cluster = ids[0]
	}
	return []console.Field{
		{Name: "profileType", Label: "Application profile type", Type: "radio", Default: typ,
			Options:            []string{"STANDARD", "IN_MEMORY", "DATA_BOOST"},
			OptionLabels:       map[string]string{"STANDARD": "Standard", "IN_MEMORY": "In-memory tier", "DATA_BOOST": "Data Boost"},
			UnavailableOptions: map[string]string{"IN_MEMORY": bigtableNoInMemory},
			Help:               "Data Boost profiles read only, with compute the host project pays for (HOST_PAYS); stored, as the emulator serves every read alike."},
		{Name: "description", Label: "Description", Type: "text", Default: desc},
		{Name: "routing", Label: "Cluster routing", Type: "radio", Default: routing, Options: []string{"SINGLE_CLUSTER", "MULTI_CLUSTER"},
			OptionLabels: map[string]string{"SINGLE_CLUSTER": "Single-cluster routing", "MULTI_CLUSTER": "Multi-cluster routing"},
			Help:         "Stored and returned: the emulator has one copy of every table, so a request is served the same through any profile."},
		{Name: "routeTo", Label: "Cluster", Type: "select", Default: cluster, Options: ids, ShowWhen: "routing=SINGLE_CLUSTER"},
		{Name: "clusterGroup", Label: "Cluster group", Type: "text", Default: group, ShowWhen: "routing=MULTI_CLUSTER",
			Help: "Empty for Any cluster, or the cluster IDs to route to, comma-separated."},
		{Name: "rowAffinity", Label: "Enable row affinity", Type: "checkbox", Default: strconv.FormatBool(affinity), ShowWhen: "routing=MULTI_CLUSTER&profileType=STANDARD"},
		{Name: "transactionalWrites", Label: "Allow single-row transactions", Type: "checkbox", Default: strconv.FormatBool(writes),
			ShowWhen: "routing=SINGLE_CLUSTER&profileType=STANDARD", Help: "CheckAndMutateRow and ReadModifyWriteRow through this profile."},
		{Name: "priority", Label: "Select a routing priority", Type: "radio", Default: priority, Options: []string{"HIGH", "MEDIUM", "LOW"},
			OptionLabels: map[string]string{"HIGH": "High", "MEDIUM": "Medium", "LOW": "Low"}, ShowWhen: "profileType=STANDARD"},
	}
}

// profileCreateFields is Create application profile as Google's steps:
// Choose a profile type, then (Next) the profile's ID, description and
// routing.
func profileCreateFields(clusters []*bigtable.ClusterInfo) []console.Field {
	const typeStep, details = "Choose an application profile type", "Configure the application profile"
	fields := profileFields(nil, clusters)
	out := make([]console.Field, 0, len(fields)+1)
	for _, f := range fields {
		if f.Name == "profileType" {
			f.Section = typeStep
			out = append(out, f)
		}
	}
	id := profileIDField()
	id.Section = details
	out = append(out, id)
	for _, f := range fields {
		if f.Name != "profileType" {
			f.Section = details
			out = append(out, f)
		}
	}
	return out
}

// formProfile is the app profile the form describes, and the update mask
// paths it sets.
func formProfile(name string, values map[string]string) (*btapb.AppProfile, []string, error) {
	ap := &btapb.AppProfile{Name: name, Description: values["description"]}
	paths := []string{"description"}
	typ := values["profileType"]
	switch values["routing"] {
	case "", "MULTI_CLUSTER":
		m := &btapb.AppProfile_MultiClusterRoutingUseAny{ClusterIds: splitAndTrim(values["clusterGroup"])}
		if values["rowAffinity"] == "true" && typ != "DATA_BOOST" {
			m.Affinity = &btapb.AppProfile_MultiClusterRoutingUseAny_RowAffinity_{RowAffinity: &btapb.AppProfile_MultiClusterRoutingUseAny_RowAffinity{}}
		}
		ap.RoutingPolicy = &btapb.AppProfile_MultiClusterRoutingUseAny_{MultiClusterRoutingUseAny: m}
		paths = append(paths, "multi_cluster_routing_use_any")
	case "SINGLE_CLUSTER":
		id := strings.TrimSpace(values["routeTo"])
		if id == "" {
			return nil, nil, errors.New("single-cluster routing needs a cluster")
		}
		ap.RoutingPolicy = &btapb.AppProfile_SingleClusterRouting_{SingleClusterRouting: &btapb.AppProfile_SingleClusterRouting{
			ClusterId: id, AllowTransactionalWrites: values["transactionalWrites"] == "true" && typ != "DATA_BOOST"}}
		paths = append(paths, "single_cluster_routing")
	default:
		return nil, nil, fmt.Errorf("cluster routing %q is not SINGLE_CLUSTER or MULTI_CLUSTER", values["routing"])
	}
	switch typ {
	case "", "STANDARD":
		pr := btapb.AppProfile_PRIORITY_HIGH
		switch values["priority"] {
		case "", "HIGH":
		case "MEDIUM":
			pr = btapb.AppProfile_PRIORITY_MEDIUM
		case "LOW":
			pr = btapb.AppProfile_PRIORITY_LOW
		default:
			return nil, nil, fmt.Errorf("routing priority %q is not HIGH, MEDIUM or LOW", values["priority"])
		}
		ap.Isolation = &btapb.AppProfile_StandardIsolation_{StandardIsolation: &btapb.AppProfile_StandardIsolation{Priority: pr}}
		paths = append(paths, "standard_isolation")
	case "DATA_BOOST":
		ap.Isolation = &btapb.AppProfile_DataBoostIsolationReadOnly_{DataBoostIsolationReadOnly: &btapb.AppProfile_DataBoostIsolationReadOnly{
			ComputeBillingOwner: btapb.AppProfile_DataBoostIsolationReadOnly_HOST_PAYS.Enum()}}
		paths = append(paths, "data_boost_isolation_read_only")
	case "IN_MEMORY":
		return nil, nil, errors.New(bigtableNoInMemory)
	default:
		return nil, nil, fmt.Errorf("application profile type %q is not STANDARD or DATA_BOOST", typ)
	}
	return ap, paths, nil
}

// formRouting is the routing policy the form asks for (the handwritten
// client's, kept for its callers).
func formRouting(values map[string]string) (bigtable.RoutingPolicyConfig, error) {
	switch values["routing"] {
	case "", "MULTI_CLUSTER":
		return &bigtable.MultiClusterRoutingUseAnyConfig{ClusterIDs: splitAndTrim(values["clusterGroup"])}, nil
	case "SINGLE_CLUSTER":
		id := strings.TrimSpace(values["routeTo"])
		if id == "" {
			return nil, errors.New("single-cluster routing needs a cluster")
		}
		return &bigtable.SingleClusterRoutingConfig{ClusterID: id, AllowTransactionalWrites: values["transactionalWrites"] == "true"}, nil
	}
	return nil, fmt.Errorf("routing %q is not MULTI_CLUSTER or SINGLE_CLUSTER", values["routing"])
}

// createProfile is Create application profile, through the generated
// client, which carries the type and priority.
func createProfile(ctx context.Context, c *bigtable.InstanceAdminClient, instanceName string, values map[string]string) error {
	id := strings.TrimSpace(values["profile"])
	ap, _, err := formProfile("", values)
	if err != nil {
		return err
	}
	_, err = c.InstanceAdminClientV2().CreateAppProfile(ctx, &btapb.CreateAppProfileRequest{Parent: instanceName,
		AppProfileId: id, AppProfile: ap, IgnoreWarnings: true})
	return err
}

// updateProfile is Edit application profile.
func updateProfile(ctx context.Context, c *bigtable.InstanceAdminClient, profileName string, values map[string]string) error {
	ap, paths, err := formProfile(profileName, values)
	if err != nil {
		return err
	}
	op, err := c.InstanceAdminClientV2().UpdateAppProfile(ctx, &btapb.UpdateAppProfileRequest{AppProfile: ap,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}, IgnoreWarnings: true})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func clusterActions(c *bigtable.ClusterInfo) []console.Action {
	return []console.Action{
		{ID: "editnodes", Label: "Edit cluster", Fields: nodeFields(c, "")},
		{ID: "deletecluster", Label: "Delete cluster", Destructive: true,
			Confirm: "The cluster is removed from the instance. Its tables stay: they belong to the instance."},
	}
}

func profileActions(ap *btapb.AppProfile, clusters []*bigtable.ClusterInfo) []console.Action {
	return []console.Action{
		{ID: "editprofile", Label: "Edit application profile", Fields: profileFields(ap, clusters)},
		{ID: "deleteprofile", Label: "Delete application profile", Destructive: true,
			Confirm: "A client that names this application profile is refused once it is gone."},
	}
}

// --- the instance page ---

// Detail is an instance's page, with Google's left-hand menu. The server
// adds the Permissions page.
func (p bigtableInstancesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*dbTimeout)
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
	inst := path[0]
	clusters, cerr := c.Clusters(ctx, inst)
	nodes, storage := 0, "—"
	for _, cl := range clusters {
		nodes += cl.ServeNodes
	}
	if len(clusters) > 0 {
		storage = bigtableStorage(clusters[0].StorageType)
	}
	summary := []console.Property{
		{Label: "Instance name", Value: in.DisplayName},
		{Label: "Edition", Value: bigtableEdition(in.Edition)},
		{Label: "Storage type", Value: storage},
		{Label: "Clusters", Value: strconv.Itoa(len(clusters))},
		{Label: "Nodes", Value: strconv.Itoa(nodes)},
	}
	actions := map[string]console.Action{}
	var header []console.Action
	for _, a := range p.DetailActions(ctx, project, path) {
		actions[a.ID] = a
		if a.ID == "editinstance" || a.ID == "deleteinstance" {
			header = append(header, a)
		}
	}
	pick := func(ids ...string) []console.Action {
		var out []console.Action
		for _, id := range ids {
			if a, ok := actions[id]; ok {
				out = append(out, a)
			}
		}
		return out
	}
	overview := console.Section{ID: "overview", Label: "Overview", Kind: console.KindProperties, Groups: []console.PropertyGroup{
		{Heading: "Instance", Properties: append(summary,
			console.Property{Label: "Instance ID", Value: inst},
			console.Property{Label: "Instance type", Value: bigtableInstanceType(in.InstanceType)},
			console.Property{Label: "State", Value: bigtableInstanceState(in.InstanceState)},
			console.Property{Label: "Encryption", Value: "Google-managed encryption key"},
			console.Property{Label: "Labels", Value: orDash(labelsText(in.Labels))})},
	}}
	for _, cl := range clusters {
		overview.Groups = append(overview.Groups, console.PropertyGroup{Heading: "Cluster " + cl.Name, Properties: []console.Property{
			{Label: "Zone", Value: cl.Zone}, {Label: "Nodes", Value: clusterNodes(cl)},
			{Label: "Node scaling mode", Value: clusterScalingMode(cl)}, {Label: "Storage type", Value: bigtableStorage(cl.StorageType)}}})
	}
	extras := p.tableExtrasListings(ctx, project, inst)
	sections := []console.Section{
		overview,
		{ID: "tables", Label: "Tables", Listing: p.tablesListing(ctx, project, inst, len(clusters)), Actions: pick("createtable", "undeletetable")},
		{ID: "studio", Label: "Bigtable Studio", Kind: console.KindStudio},
		{ID: "clusters", Label: "Clusters", Listing: clustersListing(inst, clusters, cerr), Actions: pick("addcluster")},
		{ID: "appprofiles", Label: "Application profiles", Listing: p.profilesListing(ctx, c, inst, clusters), Actions: pick("createprofile")},
		{ID: "backups", Label: "Backups", Listing: p.backupsListing(ctx, project, inst), Actions: pick("createbackup")},
		extras[1],
		{ID: "logicalviews", Label: "Logical views", Listing: p.sqlViewsListing(ctx, c, project, inst, false), Actions: pick("createlogical")},
		{ID: "materializedviews", Label: "Materialized views", Listing: p.sqlViewsListing(ctx, c, project, inst, true), Actions: pick("creatematerialized")},
		spannerNotLocal("monitoring", "Monitoring", "an instance's charts (CPU utilization, storage utilization, latency, throughput) are Cloud Monitoring metrics, which the Bigtable emulator does not report. The console's Monitoring page charts the requests this instance served."),
		spannerNotLocal("key-visualizer", "Key Visualizer", "Key Visualizer draws Bigtable's key-access heatmaps from scans Google takes of each table, which the emulator does not record."),
		spannerNotLocal("system-insights", "System insights", "system insights are Cloud Monitoring dashboards over Bigtable's metrics, which the emulator does not report."),
		extras[0],
		extras[2],
	}
	return console.Detail{SideNav: true, Actions: header, Summary: summary, Sections: sections}, nil
}

func clusterScalingMode(c *bigtable.ClusterInfo) string {
	if c.AutoscalingConfig != nil {
		return "Autoscaling"
	}
	return "Manual node allocation"
}

// tablesListing is an instance's Tables page: Google's columns, each table
// a link to its page, with Edit table and Delete table on its row.
func (p bigtableInstancesProvider) tablesListing(ctx context.Context, project, instance string, clusters int) console.Listing {
	list := console.Listing{Columns: []string{"Clusters", "Column families", "Change stream", "Deletion protection", "Current backups"},
		NameColumn: "Table ID", Noun: "tables"}
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
	backups := map[string]int{}
	it := admin.TableAdminClientV2().ListBackups(ctx, &btapb.ListBackupsRequest{Parent: "projects/" + project + "/instances/" + instance + "/clusters/-"})
	for {
		b, err := it.Next()
		if err != nil {
			break
		}
		backups[lastSegment(b.GetSourceTable())]++
	}
	sort.Strings(tables)
	for _, t := range tables {
		fields := map[string]string{"Clusters": strconv.Itoa(clusters), "Column families": "—", "Change stream": "Disabled",
			"Deletion protection": "Disabled", "Current backups": strconv.Itoa(backups[t])}
		var info *bigtable.TableInfo
		if info, err = admin.TableInfo(ctx, t); err == nil {
			names := make([]string, 0, len(info.FamilyInfos))
			for _, f := range info.FamilyInfos {
				names = append(names, f.Name)
			}
			sort.Strings(names)
			if len(names) > 0 {
				fields["Column families"] = strings.Join(names, ", ")
			}
			if d, ok := info.ChangeStreamRetention.(time.Duration); ok && d > 0 {
				fields["Change stream"] = "Enabled (" + formatChangeStreamRetention(d) + ")"
			}
			if info.DeletionProtection == bigtable.Protected {
				fields["Deletion protection"] = "Enabled"
			}
		}
		list.Items = append(list.Items, console.Resource{Name: t, Link: bigtableTablesHref(project, instance, t), Fields: fields,
			Actions: tableRowActions(info, t), ActsOn: []string{instance, bigtableTablesSegment, t}})
	}
	list.Total = len(list.Items)
	list.Note = "Each table's page holds its rows, column families and every table action; Bigtable Studio queries them. " +
		"Storage utilization is not shown: the emulator keeps every table in memory and reports no size."
	return list
}

// --- tables ---

// bigtableFamilyGroup is the column families field: a row per family with
// its type and garbage-collection policy, as Google's "Add a column
// family" holds them.
func bigtableFamilyGroup(def string) console.Field {
	return console.Field{Name: "families", Label: "Column families", Type: console.GroupFieldType, AddLabel: "Add a column family",
		Default: def,
		Help: "A family's garbage collection: Maximum versions keeps that many versions of each cell, Maximum age (7d, 12h, 30m) deletes " +
			"older cells; with both, a cell goes when either is exceeded, or only when both are with the last box checked. Neither: cells never expire.",
		Group: []console.Field{
			{Name: "family", Label: "Column family ID", Type: "text", Required: true, Pattern: bigtableFamilyPattern},
			{Name: "type", Label: "Type", Type: "select", Default: "Standard", Options: []string{"Standard", "Sum", "Min", "Max", "HyperLogLog"}},
			{Name: "maxVersions", Label: "Maximum versions", Type: "text", Pattern: `^[1-9][0-9]{0,8}$`},
			{Name: "maxAge", Label: "Maximum age", Type: "text", Pattern: `^[1-9][0-9]{0,8}[dhm]$`},
			{Name: "both", Label: "Only when both are exceeded", Type: "checkbox"},
		}}
}

// bigtableCreateTableFields are Create table's inputs.
func bigtableCreateTableFields() []console.Field {
	return []console.Field{
		{Name: "table", Label: "Table ID", Type: "text", Required: true, Pattern: `^[A-Za-z0-9][A-Za-z0-9_\-.]{0,49}$`,
			Help: "1 to 50 letters, digits, hyphens, underscores and periods. It is permanent."},
		bigtableFamilyGroup(`[{"family":"cf1","type":"Standard"}]`),
		{Name: "rowKeySchema", Label: "Define row key schema", Type: "checkbox", Unavailable: bigtableNoRowKeySchema},
		{Name: "changeStream", Label: "Enable change stream", Type: "checkbox",
			Help: "Records every change to the table, read with ReadChangeStream."},
		{Name: "retention", Label: "Retention period (days)", Type: "text", Default: "1", Pattern: `^[1-7]$`, ShowWhen: "changeStream",
			Help: "1 to 7 days."},
		{Name: "protected", Label: "Deletion protection", Type: "checkbox",
			Help: "While it is on, Bigtable refuses to delete the table or its column families."},
		{Name: "automatedBackup", Label: "Automated backup", Type: "checkbox", Unavailable: bigtableNoAutoBkp},
	}
}

// familyConf is one row of the column families field as the client's
// Family.
func familyConf(row map[string]string) (string, bigtable.Family, error) {
	name := strings.TrimSpace(row["family"])
	if name == "" {
		return "", bigtable.Family{}, errors.New("a column family ID is required")
	}
	gc, err := parseGCPolicy(row)
	if err != nil {
		return "", bigtable.Family{}, fmt.Errorf("column family %s: %w", name, err)
	}
	f := bigtable.Family{GCPolicy: gc}
	switch row["type"] {
	case "", "Standard":
	case "Sum", "Min", "Max", "HyperLogLog":
		// Every aggregate of Int64 inputs; the Bigtable front computes Min,
		// Max and HyperLogLog++, which the emulator does not (#1893).
		agg := map[string]bigtable.Aggregator{"Sum": bigtable.SumAggregator{}, "Min": bigtable.MinAggregator{},
			"Max": bigtable.MaxAggregator{}, "HyperLogLog": bigtable.HllppUniqueCountAggregator{}}[row["type"]]
		f.ValueType = bigtable.AggregateType{Input: bigtable.Int64Type{Encoding: bigtable.BigEndianBytesEncoding{}}, Aggregator: agg}
	default:
		return "", bigtable.Family{}, fmt.Errorf("column family %s: type %q is not Standard, Sum, Min, Max or HyperLogLog", name, row["type"])
	}
	return name, f, nil
}

// createBigtableTable is Create table: the table with its column families,
// change stream and deletion protection in one CreateTable.
func createBigtableTable(ctx context.Context, admin *bigtable.AdminClient, values map[string]string) (string, error) {
	table := strings.TrimSpace(values["table"])
	if table == "" {
		return "", errors.New("a table ID is required")
	}
	if values["rowKeySchema"] == "true" {
		return "", errors.New(bigtableNoRowKeySchema)
	}
	if values["automatedBackup"] == "true" {
		return "", errors.New(bigtableNoAutoBkp)
	}
	conf := &bigtable.TableConf{TableID: table, ColumnFamilies: map[string]bigtable.Family{}}
	if strings.HasPrefix(strings.TrimSpace(values["families"]), "[") {
		rows, err := console.ParseGroup(values["families"])
		if err != nil {
			return "", fmt.Errorf("column families: %w", err)
		}
		for _, r := range rows {
			name, f, err := familyConf(r)
			if err != nil {
				return "", err
			}
			conf.ColumnFamilies[name] = f
		}
	} else {
		// The earlier form's comma-separated names.
		for _, f := range splitAndTrim(values["families"]) {
			conf.ColumnFamilies[f] = bigtable.Family{GCPolicy: bigtable.NoGcPolicy()}
		}
	}
	if len(conf.ColumnFamilies) == 0 {
		conf.ColumnFamilies["cf1"] = bigtable.Family{GCPolicy: bigtable.NoGcPolicy()}
	}
	if values["changeStream"] == "true" {
		days, err := strconv.Atoi(strings.TrimSpace(values["retention"]))
		if err != nil || days < 1 || days > 7 {
			return "", fmt.Errorf("change stream retention %q: want 1 to 7 days", values["retention"])
		}
		conf.ChangeStreamRetention = time.Duration(days) * 24 * time.Hour
	}
	if values["protected"] == "true" {
		conf.DeletionProtection = bigtable.Protected
	}
	if err := admin.CreateTableFromConf(ctx, conf); err != nil {
		return "", fmt.Errorf("creating the table: %w", err)
	}
	return table, nil
}

// tableRowActions are a Tables row's Edit table and Delete table, the edit
// prefilled from info (nil when the table could not be read).
func tableRowActions(info *bigtable.TableInfo, table string) []console.Action {
	del := console.Action{ID: "deletetable", Label: "Delete", Destructive: true,
		Confirm: "The table " + table + " and every row in it are deleted. A deleted table can be undeleted for seven days."}
	if info == nil {
		return []console.Action{del}
	}
	return []console.Action{{ID: "edittable", Label: "Edit", Fields: editTableFields(info)}, del}
}

// bigtableFamilyType is a family's type in Create table's words: Standard,
// or the aggregate it is.
func bigtableFamilyType(t bigtable.Type) string {
	at, ok := t.(bigtable.AggregateType)
	if !ok {
		return "Standard"
	}
	switch at.Aggregator.(type) {
	case bigtable.SumAggregator:
		return "Sum"
	case bigtable.MinAggregator:
		return "Min"
	case bigtable.MaxAggregator:
		return "Max"
	case bigtable.HllppUniqueCountAggregator:
		return "HyperLogLog"
	}
	return "Standard"
}

// editTableFields are the Edit table pane: its column families (a family
// removed is deleted, one added is created, one changed has its policy
// updated), its change stream and deletion protection.
func editTableFields(info *bigtable.TableInfo) []console.Field {
	var rows []string
	names := make([]bigtable.FamilyInfo, len(info.FamilyInfos))
	copy(names, info.FamilyInfos)
	sort.Slice(names, func(i, j int) bool { return names[i].Name < names[j].Name })
	for _, f := range names {
		v, a, both, _ := formatGCPolicy(f.FullGCPolicy)
		typ := bigtableFamilyType(f.ValueType)
		rows = append(rows, fmt.Sprintf(`{"family":%q,"type":%q,"maxVersions":%q,"maxAge":%q,"both":%q}`, f.Name, typ, v, a, strconv.FormatBool(both)))
	}
	var retention time.Duration
	if d, ok := info.ChangeStreamRetention.(time.Duration); ok {
		retention = d
	}
	days := "1"
	if retention > 0 {
		days = strconv.Itoa(int((retention + 23*time.Hour) / (24 * time.Hour)))
	}
	protection := "ENABLE"
	if info.DeletionProtection == bigtable.Protected {
		protection = "PREVENT"
	}
	return []console.Field{
		bigtableFamilyGroup("[" + strings.Join(rows, ",") + "]"),
		{Name: "changeStream", Label: "Enable change stream", Type: "checkbox", Default: strconv.FormatBool(retention > 0)},
		{Name: "retention", Label: "Retention period (days)", Type: "text", Default: days, Pattern: `^[1-7]$`, ShowWhen: "changeStream"},
		{Name: "protection", Label: "Deletion protection", Type: "radio", Default: protection, Options: []string{"PREVENT", "ENABLE"},
			OptionLabels: map[string]string{"PREVENT": "Prevent deletion", "ENABLE": "Enable deletion"}},
	}
}

// editTable applies the Edit table pane.
func editTable(ctx context.Context, admin *bigtable.AdminClient, table string, values map[string]string) error {
	info, err := admin.TableInfo(ctx, table)
	if err != nil {
		return err
	}
	rows, err := console.ParseGroup(values["families"])
	if err != nil {
		return fmt.Errorf("column families: %w", err)
	}
	want := map[string]bigtable.Family{}
	for _, r := range rows {
		name, f, err := familyConf(r)
		if err != nil {
			return err
		}
		want[name] = f
	}
	have := map[string]bigtable.FamilyInfo{}
	for _, f := range info.FamilyInfos {
		have[f.Name] = f
	}
	// Deletion protection first when it is being turned off, so the
	// families it guards can then be deleted.
	if values["protection"] == "ENABLE" && info.DeletionProtection == bigtable.Protected {
		if err := admin.UpdateTableWithDeletionProtection(ctx, table, bigtable.Unprotected); err != nil {
			return err
		}
	}
	for name, f := range want {
		cur, ok := have[name]
		switch {
		case !ok:
			if err := admin.CreateColumnFamilyWithConfig(ctx, table, name, f); err != nil {
				return fmt.Errorf("adding column family %s: %w", name, err)
			}
		case cur.GCPolicy != f.GCPolicy.String():
			if err := admin.SetGCPolicy(ctx, table, name, f.GCPolicy); err != nil {
				return fmt.Errorf("column family %s: %w", name, err)
			}
		}
	}
	for name := range have {
		if _, ok := want[name]; !ok {
			if err := admin.DeleteColumnFamily(ctx, table, name); err != nil {
				return fmt.Errorf("deleting column family %s: %w", name, err)
			}
		}
	}
	var retention time.Duration
	if d, ok := info.ChangeStreamRetention.(time.Duration); ok {
		retention = d
	}
	if values["changeStream"] == "true" {
		days, err := strconv.Atoi(strings.TrimSpace(values["retention"]))
		if err != nil || days < 1 || days > 7 {
			return fmt.Errorf("change stream retention %q: want 1 to 7 days", values["retention"])
		}
		if d := time.Duration(days) * 24 * time.Hour; d != retention {
			if err := admin.UpdateTableWithChangeStream(ctx, table, d); err != nil {
				return err
			}
		}
	} else if retention > 0 {
		if err := admin.UpdateTableDisableChangeStream(ctx, table); err != nil {
			return err
		}
	}
	if values["protection"] == "PREVENT" && info.DeletionProtection != bigtable.Protected {
		return admin.UpdateTableWithDeletionProtection(ctx, table, bigtable.Protected)
	}
	return nil
}

// createBackupFields are the Backups page's Create backup.
func createBackupFields(tables []string, clusters []*bigtable.ClusterInfo) []console.Field {
	ids := make([]string, 0, len(clusters))
	for _, c := range clusters {
		ids = append(ids, c.Name)
	}
	def := ""
	if len(ids) > 0 {
		def = ids[0]
	}
	return []console.Field{
		{Name: "table", Label: "Table", Type: "select", Required: true, Options: tables},
		{Name: "cluster", Label: "Cluster", Type: "select", Required: true, Options: ids, Default: def},
		{Name: "backup", Label: "Backup ID", Type: "text", Required: true, Pattern: `^[_a-zA-Z0-9][\-_.a-zA-Z0-9]{0,49}$`},
		{Name: "days", Label: "Expiration (days)", Type: "text", Default: "7", Pattern: `^[0-9]{1,2}$`, Help: "Between 1 and 90 days from now."},
	}
}

// instanceTables lists the instance's tables, or nil.
func (p bigtableInstancesProvider) instanceTables(ctx context.Context, project, instance string) []string {
	admin, err := bigtable.NewAdminClient(ctx, project, instance, localOpts(p.endpoint)...)
	if err != nil {
		return nil
	}
	defer admin.Close()
	tables, err := admin.Tables(ctx)
	if err != nil {
		return nil
	}
	sort.Strings(tables)
	return tables
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
		create := profileCreateFields(clusters)
		return append([]console.Action{
			{ID: "editinstance", Label: "Edit instance", Fields: instanceEditFields(in)},
			{ID: "createtable", Label: "Create table", Fields: bigtableCreateTableFields()},
			{ID: "addcluster", Label: "Add cluster", Fields: add},
			{ID: "createprofile", Label: "Create application profile", Fields: create, Steps: "next"},
			{ID: "createbackup", Label: "Create backup", Fields: createBackupFields(p.instanceTables(ctx, project, path[0]), clusters)},
		}, append(instanceExtraActions(),
			console.Action{ID: "deleteinstance", Label: "Delete instance", Destructive: true, Leaves: true,
				Confirm: "The instance is deleted with its clusters, application profiles and every table in it."})...)
	}
	switch path[1] {
	case bigtableTablesSegment:
		admin, err := bigtable.NewAdminClient(ctx, project, path[0], localOpts(p.endpoint)...)
		if err != nil {
			return nil
		}
		defer admin.Close()
		info, err := admin.TableInfo(ctx, path[2])
		if err != nil {
			return nil
		}
		return tableRowActions(info, path[2])
	case bigtableBackupsSegment:
		return bigtableBackupActions()
	case bigtableLogicalSegment:
		return sqlViewActions(false)
	case bigtableMaterializedSegment:
		return sqlViewActions(true)
	case bigtableSnapshotsSegment:
		return snapshotActions()
	case bigtableAuthViewsSegment:
		return authViewRowActions()
	case bigtableBundlesSegment:
		return bundleRowActions()
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
	tableAdmin := func(fn func(*bigtable.AdminClient) error) error {
		admin, err := bigtable.NewAdminClient(ctx, project, inst, localOpts(p.endpoint)...)
		if err != nil {
			return err
		}
		defer admin.Close()
		return fn(admin)
	}
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
		ipb := &btapb.Instance{Name: name, DisplayName: strings.TrimSpace(values["displayName"]), Type: btapb.Instance_Type(typ), Labels: labels}
		paths := []string{"display_name", "type", "labels"}
		if e, ok := btapb.Instance_Edition_value[values["edition"]]; ok && e != 0 {
			ipb.Edition = btapb.Instance_Edition(e)
			paths = append(paths, "edition")
		}
		op, err := c.InstanceAdminClientV2().PartialUpdateInstance(ctx, &btapb.PartialUpdateInstanceRequest{Instance: ipb,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case len(path) == 1 && action == "createtable":
		return tableAdmin(func(a *bigtable.AdminClient) error {
			_, err := createBigtableTable(ctx, a, values)
			return err
		})
	case len(path) == 1 && action == "createbackup":
		days, err := strconv.Atoi(strings.TrimSpace(values["days"]))
		if err != nil || days < 1 || days > 90 {
			return fmt.Errorf("expiration %q days: want a whole number of days from 1 to 90", values["days"])
		}
		return tableAdmin(func(a *bigtable.AdminClient) error {
			return a.CreateBackup(ctx, strings.TrimSpace(values["table"]), strings.TrimSpace(values["cluster"]),
				strings.TrimSpace(values["backup"]), time.Now().Add(time.Duration(days)*24*time.Hour))
		})
	case len(path) == 1 && action == "addcluster":
		if values["storage"] == "" {
			if cs, err := c.Clusters(ctx, inst); err == nil && len(cs) > 0 {
				values["storage"] = bigtableStorage(cs[0].StorageType)
			}
		}
		cc, err := formCluster(inst, values)
		if err != nil {
			return err
		}
		return c.CreateCluster(ctx, cc)
	case len(path) == 1 && action == "createprofile":
		return createProfile(ctx, c, name, values)
	case len(path) == 1 && action == "deleteinstance":
		return c.DeleteInstance(ctx, inst)
	case len(path) == 3 && path[1] == bigtableTablesSegment && action == "edittable":
		return tableAdmin(func(a *bigtable.AdminClient) error { return editTable(ctx, a, path[2], values) })
	case len(path) == 3 && path[1] == bigtableTablesSegment && action == "deletetable":
		return tableAdmin(func(a *bigtable.AdminClient) error { return a.DeleteTable(ctx, path[2]) })
	case len(path) == 3 && path[1] == bigtableClustersSegment && action == "editnodes":
		return editNodes(ctx, c, name, inst, path[2], values)
	case len(path) == 3 && path[1] == bigtableClustersSegment && action == "deletecluster":
		return c.DeleteCluster(ctx, inst, path[2])
	case len(path) == 3 && path[1] == bigtableProfilesSegment && action == "editprofile":
		return updateProfile(ctx, c, name+"/appProfiles/"+path[2], values)
	case len(path) == 3 && path[1] == bigtableProfilesSegment && action == "deleteprofile":
		return c.DeleteAppProfile(ctx, inst, path[2])
	}
	return fmt.Errorf("%s is not available on this resource", action)
}

// profilesListing is the Application profiles page.
func (p bigtableInstancesProvider) profilesListing(ctx context.Context, c *bigtable.InstanceAdminClient, instance string, clusters []*bigtable.ClusterInfo) console.Listing {
	list := console.Listing{Columns: []string{"Description", "Type", "Cluster routing", "Single-row transactions", "Routing priority"},
		NameColumn: "Application profile ID", Noun: "application profiles"}
	it := c.ListAppProfiles(ctx, instance)
	for {
		ap, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			list.Unavailable = "listing application profiles: " + err.Error()
			return list
		}
		id := lastSegment(ap.Name)
		writes := "Not allowed"
		if ap.GetSingleClusterRouting().GetAllowTransactionalWrites() {
			writes = "Allowed"
		}
		typ, priority := "Standard", map[string]string{"HIGH": "High", "MEDIUM": "Medium", "LOW": "Low"}[profilePriority(ap)]
		if profileType(ap) == "DATA_BOOST" {
			typ, priority = "Data Boost", "—"
		}
		list.Items = append(list.Items, console.Resource{Name: id,
			Fields: map[string]string{"Description": ap.Description, "Type": typ, "Cluster routing": profileRouting(ap),
				"Single-row transactions": writes, "Routing priority": priority},
			Actions: profileActions(ap, clusters), ActsOn: []string{instance, bigtableProfilesSegment, id}})
	}
	list.Total = len(list.Items)
	list.Note = "Routing and priority are stored and returned: the emulator has one copy of every table, so a request is served the same through any profile. " +
		"Review the replication settings in the default application profile."
	return list
}
