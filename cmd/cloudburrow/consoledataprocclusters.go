package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/dataproc"
)

// The console's Dataproc pages (#2162), as Google's Dataproc menu has them
// above Serverless: Clusters (Create cluster on a page of its own, in
// Google's Set up cluster, Configure nodes and Customize cluster steps; a
// cluster's page with its Jobs, VM instances, Configuration and Web
// interfaces, the Spark History Server among them; Edit, Stop, Start,
// Delete, Diagnose and Submit job), Jobs (Submit job, a job's page with its
// output from Cloud Storage, Configuration, Cancel and Delete), Workflows
// (workflow templates, their DAG and parameters, Run, and their runs) and
// Autoscaling policies (Create, Edit, Delete).

func dataprocRegion(project, region string) string {
	return "projects/" + project + "/regions/" + region
}

// dataprocParse is a projects/P/regions/R/KIND/ID name's parts.
func dataprocParse(name string) (project, region, id string) {
	m := strings.Split(name, "/")
	if len(m) != 6 {
		return "", "", ""
	}
	return m[1], m[3], m[5]
}

// dataprocDriverOutput reads a driver output's first part from Cloud
// Storage, as the Output tab and a job's page show it.
func dataprocDriverOutput(ctx context.Context, svc *dataprocService, uri string, running bool) string {
	if uri == "" {
		return "The job has no output yet."
	}
	if svc.live == nil || svc.live()["storage"] == "" {
		return "Cloud Storage is not served by this instance, so the driver output at " + uri + " cannot be read."
	}
	st := &dataprocStorage{live: svc.live}
	c, err := st.client(ctx)
	if err != nil {
		return consoleErr(err)
	}
	defer c.Close()
	bucket, prefix, err := dataprocGS(uri)
	if err != nil {
		return consoleErr(err)
	}
	r, err := c.Bucket(bucket).Object(prefix + ".000000000").NewReader(ctx)
	if err != nil {
		if running {
			return "The driver output is written to " + uri + " as the job runs."
		}
		return "Cannot read the driver output at " + uri + ": " + consoleErr(err)
	}
	defer r.Close()
	out, _ := io.ReadAll(io.LimitReader(r, 1<<20))
	if len(out) == 0 {
		return "The driver wrote no output."
	}
	return string(out)
}

// dataprocImageVersions are the Versioning step's choices.
func dataprocImageVersions() ([]string, map[string]string) {
	var opts []string
	labels := map[string]string{}
	mms := make([]string, 0, len(dataproc.ClusterImageVersions))
	for mm := range dataproc.ClusterImageVersions {
		mms = append(mms, mm)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(mms)))
	for _, mm := range mms {
		for _, osName := range dataproc.ClusterImageVersions[mm] {
			v := mm + "-" + osName
			opts = append(opts, v)
			labels[v] = v
		}
	}
	labels[dataproc.DefaultImageVersion] = dataproc.DefaultImageVersion + " (default)"
	return opts, labels
}

func dataprocDur(d *durationpb.Duration) string {
	if d == nil {
		return "-"
	}
	return dataprocElapsed(time.Unix(0, 0), time.Unix(0, 0).Add(d.AsDuration()))
}

// ---- Clusters ----

type dataprocClustersProvider struct{ svc *dataprocService }

func (dataprocClustersProvider) ID() string    { return "dataproc-clusters" }
func (dataprocClustersProvider) Title() string { return "Clusters" }

func dataprocWorkers(c *dataprocpb.Cluster) int32 {
	return c.GetConfig().GetWorkerConfig().GetNumInstances() + c.GetConfig().GetSecondaryWorkerConfig().GetNumInstances()
}

func dataprocClusterType(c *dataprocpb.Cluster) string {
	switch {
	case c.GetConfig().GetMasterConfig().GetNumInstances() == 3:
		return "High Availability"
	case c.GetConfig().GetWorkerConfig().GetNumInstances() == 0:
		return "Single Node"
	}
	return "Standard"
}

func (p dataprocClustersProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	listing := console.Listing{Columns: []string{"Status", "Region", "Zone", "Total worker nodes", "Scheduled deletion", "Scheduled stop", "Cloud Storage staging bucket", "Created", "Labels"},
		NameColumn: "Name", Noun: "clusters"}
	if project == "" {
		listing.Prompt = "Dataproc lists a project's clusters. Choose one in the toolbar."
		return listing, nil
	}
	for _, loc := range dataproc.Locations() {
		resp, err := s.ListClusters(ctx, &dataprocpb.ListClustersRequest{ProjectId: project, Region: loc, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, c := range resp.GetClusters() {
			name := dataprocRegion(project, loc) + "/clusters/" + c.GetClusterName()
			lc := c.GetConfig().GetLifecycleConfig()
			del, stop := "Off", "Off"
			if lc.GetIdleDeleteTtl() != nil || lc.GetAutoDeleteTtl() != nil || lc.GetAutoDeleteTime() != nil {
				del = "On"
			}
			if lc.GetIdleStopTtl() != nil || lc.GetAutoStopTtl() != nil || lc.GetAutoStopTime() != nil {
				stop = "On"
			}
			listing.Items = append(listing.Items, console.Resource{Name: c.GetClusterName(), Opens: []string{name}, Status: c.GetStatus().GetState().String(), Fields: map[string]string{
				"Name": c.GetClusterName(), "Status": dataprocState(c.GetStatus().GetState().String()), "Region": loc,
				"Zone": kmsOrDash(lastSegment(c.GetConfig().GetGceClusterConfig().GetZoneUri())), "Total worker nodes": fmt.Sprint(dataprocWorkers(c)),
				"Scheduled deletion": del, "Scheduled stop": stop, "Cloud Storage staging bucket": c.GetConfig().GetConfigBucket(),
				"Created": kmsTime(dataprocClusterCreated(c)), "Labels": dataprocLabels(dataprocUserLabels(c.GetLabels()))}})
		}
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

// dataprocClusterCreated is when the cluster was created: its first
// status's start.
func dataprocClusterCreated(c *dataprocpb.Cluster) *timestamppb.Timestamp {
	if h := c.GetStatusHistory(); len(h) > 0 {
		return h[0].GetStateStartTime()
	}
	return c.GetStatus().GetStateStartTime()
}

// dataprocUserLabels leaves out the goog-dataproc-* labels Dataproc adds,
// as Google's list does.
func dataprocUserLabels(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if !strings.HasPrefix(k, "goog-dataproc-") {
			out[k] = v
		}
	}
	return out
}

func (dataprocClustersProvider) CreateOnPage() bool { return true }
func (dataprocClustersProvider) CreateTitle() string {
	return "Create a Dataproc cluster on Compute Engine"
}

func (p dataprocClustersProvider) CreateForm() (string, []console.Field) {
	const setup, nodes, custom = "Set up cluster", "Configure nodes", "Customize cluster"
	versions, versionLabels := dataprocImageVersions()
	return "Create", []console.Field{
		{Name: "clusterName", Label: "Name", Type: "text", Required: true, Section: setup, Pattern: `^[a-z]([a-z0-9\-]{0,49}[a-z0-9])?$`,
			Help: "Starts with a lower-case letter; up to 51 lower-case letters, numbers and hyphens; cannot end with a hyphen."},
		{Name: "region", Label: "Region", Type: "select", Required: true, Section: setup, Options: dataproc.Locations(), Default: "us-central1"},
		{Name: "zone", Label: "Zone", Type: "text", Section: setup, Help: "A zone of the region, such as us-central1-a. Left empty, Any: Dataproc picks one."},
		{Name: "clusterType", Label: "Cluster type", Type: "radio", Required: true, Section: setup, Default: "STANDARD",
			Options:      []string{"STANDARD", "SINGLE", "HA"},
			OptionLabels: map[string]string{"STANDARD": "Standard (1 master, N workers)", "SINGLE": "Single Node (1 master, 0 workers)", "HA": "High Availability (3 masters, N workers)"}},
		{Name: "imageVersion", Label: "Image type and version", Type: "select", Section: setup, Options: versions, OptionLabels: versionLabels, Default: dataproc.DefaultImageVersion,
			Help: "2.1 runs Apache Spark 3.3, 2.2 and 2.3 Spark 3.5, 3.0 Spark 4.1."},
		{Name: "componentGateway", Label: "Enable component gateway", Type: "checkbox", Section: setup, Default: "true",
			Help: "Opens the cluster's web interfaces (YARN ResourceManager, MapReduce Job History, Spark History Server, YARN Application Timeline and the components') from its Web interfaces tab."},
		{Name: "comp_JUPYTER", Label: "Jupyter Notebook", Type: "checkbox", Section: setup},
		{Name: "comp_TRINO", Label: "Trino", Type: "checkbox", Section: setup},
		{Name: "comp_FLINK", Label: "Flink", Type: "checkbox", Section: setup},
		{Name: "comp_HIVE_WEBHCAT", Label: "Hive WebHCat", Type: "checkbox", Section: setup},
		{Name: "comp_ZOOKEEPER", Label: "Zookeeper", Type: "checkbox", Section: setup},
		{Name: "comp_SOLR", Label: "Solr", Type: "checkbox", Section: setup},
		{Name: "comp_PIG", Label: "Pig", Type: "checkbox", Section: setup, Help: "Optional components: on image 2.3 and later Pig is one; before, it is always installed."},
		{Name: "comp_ZEPPELIN", Label: "Zeppelin Notebook", Type: "checkbox", Section: setup, Help: "Image 2.x; it runs linux/amd64 under emulation here."},
		{Name: "comp_DELTA", Label: "Delta Lake", Type: "checkbox", Section: setup},
		{Name: "comp_ICEBERG", Label: "Iceberg", Type: "checkbox", Section: setup,
			Help: "On 2.x, dataproc:dataproc.iceberg.version=1.10 in Properties picks 1.10; 3.0 runs Iceberg 1.11 with its default_iceberg REST catalog."},
		{Name: "comp_HUDI", Label: "Hudi", Type: "checkbox", Section: setup},
		{Name: "comp_DOCKER", Label: "Docker", Type: "checkbox", Section: setup, Help: "A Docker daemon on every node."},
		{Name: "autoscalingPolicy", Label: "Autoscaling policy", Type: "text", Section: setup,
			Help: "An autoscaling policy's ID in the cluster's region, or its resource name. Left empty, none."},
		{Name: "masterMachineType", Label: "Manager node machine type", Type: "text", Section: nodes, Default: "n4-standard-4"},
		{Name: "workers", Label: "Number of worker nodes", Type: "number", Section: nodes, Default: "2", ShowWhen: "clusterType=STANDARD|HA",
			Help: "At least 2. Each worker is an Apache Spark worker of its machine type's cores and memory."},
		{Name: "workerMachineType", Label: "Worker node machine type", Type: "text", Section: nodes, Default: "n4-standard-4", ShowWhen: "clusterType=STANDARD|HA"},
		{Name: "driverNodes", Label: "Driver node group: number of nodes", Type: "number", Section: nodes,
			Help: "Nodes that run the cluster's job drivers, its node group of role DRIVER. Left empty, none. Jobs then need driver memory and vCores."},
		{Name: "idleDelete", Label: "Delete after a cluster idle time period without submitted jobs (minutes)", Type: "number", Section: custom,
			Help: "10 to 20160 minutes. Left empty, the cluster is not deleted when idle."},
		{Name: "autoDelete", Label: "Delete after a fixed time period (minutes)", Type: "number", Section: custom, Help: "10 to 20160 minutes. Left empty, off."},
		{Name: "properties", Label: "Cluster properties", Type: "textarea", Section: custom, Help: "One prefix:property=value a line, such as spark:spark.executor.memory=2g."},
		{Name: "labels", Label: "Labels", Type: "textarea", Section: custom, Help: "One key=value a line."},
	}
}

func dataprocMinutesField(v string) (*durationpb.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil, fmt.Errorf("%q is not a number of minutes", v)
	}
	return durationpb.New(time.Duration(n) * time.Minute), nil
}

func (p dataprocClustersProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a cluster")
	}
	name, region := strings.TrimSpace(values["clusterName"]), values["region"]
	cfg := &dataprocpb.ClusterConfig{
		SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: values["imageVersion"]},
		MasterConfig:   &dataprocpb.InstanceGroupConfig{NumInstances: 1, MachineTypeUri: strings.TrimSpace(values["masterMachineType"])},
	}
	switch values["clusterType"] {
	case "SINGLE":
		cfg.SoftwareConfig.Properties = map[string]string{"dataproc:dataproc.allow.zero.workers": "true"}
	case "HA":
		cfg.MasterConfig.NumInstances = 3
		fallthrough
	default:
		n, err := strconv.Atoi(strings.TrimSpace(values["workers"]))
		if err != nil {
			return "", fmt.Errorf("number of worker nodes: %q is not a number", values["workers"])
		}
		cfg.WorkerConfig = &dataprocpb.InstanceGroupConfig{NumInstances: int32(n), MachineTypeUri: strings.TrimSpace(values["workerMachineType"])}
	}
	props, err := dataprocPairs(values["properties"])
	if err != nil {
		return "", fmt.Errorf("cluster properties: %w", err)
	}
	for k, v := range props {
		if cfg.SoftwareConfig.Properties == nil {
			cfg.SoftwareConfig.Properties = map[string]string{}
		}
		cfg.SoftwareConfig.Properties[k] = v
	}
	for _, comp := range []string{"JUPYTER", "TRINO", "FLINK", "HIVE_WEBHCAT", "ZOOKEEPER", "SOLR", "PIG", "ZEPPELIN", "DELTA", "ICEBERG", "HUDI", "DOCKER"} {
		if values["comp_"+comp] == "true" {
			cfg.SoftwareConfig.OptionalComponents = append(cfg.SoftwareConfig.OptionalComponents, dataprocpb.Component(dataprocpb.Component_value[comp]))
		}
	}
	if n := strings.TrimSpace(values["driverNodes"]); n != "" {
		v, err := strconv.Atoi(n)
		if err != nil {
			return "", fmt.Errorf("driver node group: %q is not a number of nodes", n)
		}
		cfg.AuxiliaryNodeGroups = []*dataprocpb.AuxiliaryNodeGroup{{NodeGroup: &dataprocpb.NodeGroup{
			Roles: []dataprocpb.NodeGroup_Role{dataprocpb.NodeGroup_DRIVER}, NodeGroupConfig: &dataprocpb.InstanceGroupConfig{NumInstances: int32(v)}}}}
	}
	if values["componentGateway"] == "true" {
		cfg.EndpointConfig = &dataprocpb.EndpointConfig{EnableHttpPortAccess: true}
	}
	if z := strings.TrimSpace(values["zone"]); z != "" {
		cfg.GceClusterConfig = &dataprocpb.GceClusterConfig{ZoneUri: z}
	}
	if pol := strings.TrimSpace(values["autoscalingPolicy"]); pol != "" {
		if !strings.Contains(pol, "/") {
			pol = dataprocRegion(project, region) + "/autoscalingPolicies/" + pol
		}
		cfg.AutoscalingConfig = &dataprocpb.AutoscalingConfig{PolicyUri: pol}
	}
	idle, err := dataprocMinutesField(values["idleDelete"])
	if err != nil {
		return "", err
	}
	auto, err := dataprocMinutesField(values["autoDelete"])
	if err != nil {
		return "", err
	}
	if idle != nil || auto != nil {
		cfg.LifecycleConfig = &dataprocpb.LifecycleConfig{IdleDeleteTtl: idle}
		if auto != nil {
			cfg.LifecycleConfig.Ttl = &dataprocpb.LifecycleConfig_AutoDeleteTtl{AutoDeleteTtl: auto}
		}
	}
	labels, err := dataprocPairs(values["labels"])
	if err != nil {
		return "", fmt.Errorf("labels: %w", err)
	}
	if _, err := s.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: region,
		Cluster: &dataprocpb.Cluster{ProjectId: project, ClusterName: name, Config: cfg, Labels: labels}}); err != nil {
		return "", err
	}
	return dataprocRegion(project, region) + "/clusters/" + name, nil
}

func (p dataprocClustersProvider) cluster(ctx context.Context, project, name string) (*dataproc.Server, *dataprocpb.Cluster, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return nil, nil, err
	}
	if err := dataprocOwned(project, name); err != nil {
		return nil, nil, err
	}
	pr, region, id := dataprocParse(name)
	c, err := s.GetCluster(dataproc.WithAuthority(ctx, p.svc.Addr()), &dataprocpb.GetClusterRequest{ProjectId: pr, Region: region, ClusterName: id})
	return s, c, err
}

func dataprocJobType(j *dataprocpb.Job) string {
	switch j.GetTypeJob().(type) {
	case *dataprocpb.Job_PysparkJob:
		return "PySpark"
	case *dataprocpb.Job_SparkJob:
		return "Spark"
	case *dataprocpb.Job_SparkRJob:
		return "SparkR"
	case *dataprocpb.Job_SparkSqlJob:
		return "SparkSql"
	case *dataprocpb.Job_HiveJob:
		return "Hive"
	case *dataprocpb.Job_PigJob:
		return "Pig"
	case *dataprocpb.Job_HadoopJob:
		return "Hadoop"
	case *dataprocpb.Job_PrestoJob:
		return "Presto"
	case *dataprocpb.Job_TrinoJob:
		return "Trino"
	case *dataprocpb.Job_FlinkJob:
		return "Flink"
	}
	return "-"
}

func dataprocJobEnd(j *dataprocpb.Job) time.Time {
	switch j.GetStatus().GetState() {
	case dataprocpb.JobStatus_DONE, dataprocpb.JobStatus_ERROR, dataprocpb.JobStatus_CANCELLED:
		return j.GetStatus().GetStateStartTime().AsTime()
	}
	return time.Time{}
}

func dataprocJobStart(j *dataprocpb.Job) time.Time {
	if h := j.GetStatusHistory(); len(h) > 0 {
		return h[0].GetStateStartTime().AsTime()
	}
	return j.GetStatus().GetStateStartTime().AsTime()
}

// dataprocJobsListing lists jobs as the Jobs page and a cluster's Jobs tab
// do.
func dataprocJobsListing(jobs []*dataprocpb.Job, project, region string) console.Listing {
	l := console.Listing{Columns: []string{"Status", "Region", "Type", "Cluster", "Start time", "Elapsed time", "Labels"}, NameColumn: "Job ID", Noun: "jobs"}
	for _, j := range jobs {
		r := region
		if r == "" {
			r = j.GetLabels()["goog-dataproc-location"]
		}
		name := dataprocRegion(project, r) + "/jobs/" + j.GetReference().GetJobId()
		l.Items = append(l.Items, console.Resource{Name: j.GetReference().GetJobId(), Opens: []string{name}, Status: j.GetStatus().GetState().String(),
			Link: "/dataproc/jobs/" + url.PathEscape(name) + "?project=" + url.QueryEscape(project), Fields: map[string]string{
				"Job ID": j.GetReference().GetJobId(), "Status": dataprocState(j.GetStatus().GetState().String()), "Region": r, "Type": dataprocJobType(j),
				"Cluster": j.GetPlacement().GetClusterName(), "Start time": kmsTimeOf(dataprocJobStart(j)),
				"Elapsed time": dataprocElapsed(dataprocJobStart(j), dataprocJobEnd(j)), "Labels": dataprocLabels(dataprocUserLabels(j.GetLabels()))}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l
}

func kmsTimeOf(t time.Time) string {
	if t.IsZero() || t.Unix() == 0 {
		return "-"
	}
	return t.Local().Format("Jan 2, 2006, 3:04:05 PM")
}

func (p dataprocClustersProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	s, c, err := p.cluster(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the cluster: " + consoleErr(err)}, nil
	}
	pr, region, _ := dataprocParse(path[0])
	cfg := c.GetConfig()
	jobs, _ := s.ListJobs(ctx, &dataprocpb.ListJobsRequest{ProjectId: pr, Region: region, ClusterName: c.GetClusterName(), PageSize: 1000})
	jobsListing := dataprocJobsListing(jobs.GetJobs(), pr, region)

	vms := console.Listing{Columns: []string{"Role", "Machine type"}, NameColumn: "Name", Noun: "VM instances"}
	for _, g := range []struct {
		role string
		ig   *dataprocpb.InstanceGroupConfig
	}{{"Master", cfg.GetMasterConfig()}, {"Worker", cfg.GetWorkerConfig()}, {"Secondary worker", cfg.GetSecondaryWorkerConfig()}} {
		for _, n := range g.ig.GetInstanceNames() {
			vms.Items = append(vms.Items, console.Resource{Name: n, Fields: map[string]string{"Name": n, "Role": g.role, "Machine type": kmsOrDash(lastSegment(g.ig.GetMachineTypeUri()))}})
		}
	}
	vms.Total = len(vms.Items)

	web := console.Listing{Columns: []string{"Link"}, NameColumn: "Web interface", Noun: "web interfaces"}
	if !cfg.GetEndpointConfig().GetEnableHttpPortAccess() {
		web.Prompt = "Component gateway is not enabled for this cluster, so its web interfaces are not reachable from the console. Create the cluster with Enable component gateway to open them."
	} else if c.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
		web.Prompt = "The cluster's web interfaces are available while it is running."
	}
	if c.GetStatus().GetState() == dataprocpb.ClusterStatus_RUNNING {
		ports := cfg.GetEndpointConfig().GetHttpPorts()
		keys := make([]string, 0, len(ports))
		for k := range ports {
			keys = append(keys, k)
		}
		// The console's order: the Hadoop and Spark interfaces first, then
		// the components' by name.
		rank := map[string]int{"YARN ResourceManager": 1, "MapReduce Job History": 2, "Spark History Server": 3, "HDFS NameNode": 4,
			"YARN Application Timeline": 5, "Tez": 6}
		sort.Slice(keys, func(i, j int) bool {
			ri, rj := rank[keys[i]], rank[keys[j]]
			if ri == 0 {
				ri = 100
			}
			if rj == 0 {
				rj = 100
			}
			if ri != rj {
				return ri < rj
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys {
			web.Items = append(web.Items, console.Resource{Name: k, Link: ports[k], Fields: map[string]string{"Web interface": k, "Link": ports[k]}})
		}
	}
	web.Total = len(web.Items)

	sw := cfg.GetSoftwareConfig()
	var comps []string
	for _, comp := range sw.GetOptionalComponents() {
		comps = append(comps, comp.String())
	}
	lc := cfg.GetLifecycleConfig()
	conf := []console.PropertyGroup{
		{Heading: "Cluster", Properties: []console.Property{
			{Label: "Name", Value: c.GetClusterName()}, {Label: "Cluster UUID", Value: c.GetClusterUuid(), Copy: "Copy UUID"},
			{Label: "Type", Value: "Dataproc on Compute Engine"}, {Label: "Status", Value: dataprocState(c.GetStatus().GetState().String())},
			{Label: "Status detail", Value: kmsOrDash(c.GetStatus().GetDetail())},
			{Label: "Region", Value: region}, {Label: "Zone", Value: kmsOrDash(lastSegment(cfg.GetGceClusterConfig().GetZoneUri()))},
			{Label: "Cluster type", Value: dataprocClusterType(c)}, {Label: "Image version", Value: sw.GetImageVersion()},
			{Label: "Optional components", Value: kmsOrDash(strings.Join(comps, ", "))},
			{Label: "Node groups", Value: kmsOrDash(dataprocNodeGroups(cfg))},
			{Label: "Autoscaling", Value: kmsOrDash(cfg.GetAutoscalingConfig().GetPolicyUri())},
			{Label: "Component gateway", Value: map[bool]string{true: "Enabled", false: "Disabled"}[cfg.GetEndpointConfig().GetEnableHttpPortAccess()]},
			{Label: "Scheduled deletion", Value: dataprocLifecycle(lc.GetIdleDeleteTtl(), lc.GetAutoDeleteTtl(), lc.GetAutoDeleteTime() != nil)},
			{Label: "Scheduled stop", Value: dataprocLifecycle(lc.GetIdleStopTtl(), lc.GetAutoStopTtl(), lc.GetAutoStopTime() != nil)},
			{Label: "Cloud Storage staging bucket", Value: cfg.GetConfigBucket()}, {Label: "Cloud Storage temp bucket", Value: cfg.GetTempBucket()},
			{Label: "Labels", Value: dataprocLabels(c.GetLabels())}}},
		{Heading: "Master node", Properties: []console.Property{
			{Label: "Nodes", Value: fmt.Sprint(cfg.GetMasterConfig().GetNumInstances())}, {Label: "Machine type", Value: kmsOrDash(lastSegment(cfg.GetMasterConfig().GetMachineTypeUri()))}}},
		{Heading: "Worker nodes", Properties: []console.Property{
			{Label: "Nodes", Value: fmt.Sprint(cfg.GetWorkerConfig().GetNumInstances())}, {Label: "Machine type", Value: kmsOrDash(lastSegment(cfg.GetWorkerConfig().GetMachineTypeUri()))}}},
		{Heading: "Properties", Properties: []console.Property{{Label: "Cluster properties", Value: kmsOrDash(strings.ReplaceAll(dataprocPropsText(sw.GetProperties()), "\n", ", "))}}},
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Status", Value: dataprocState(c.GetStatus().GetState().String())}, {Label: "Region", Value: region},
			{Label: "Total worker nodes", Value: fmt.Sprint(dataprocWorkers(c))}},
		Sections: []console.Section{
			{ID: "jobs", Label: "Jobs", Listing: jobsListing},
			{ID: "vms", Label: "VM instances", Listing: vms},
			{ID: "configuration", Label: "Configuration", Kind: console.KindProperties, Groups: conf},
			{ID: "web", Label: "Web interfaces", Listing: web},
		},
	}, nil
}

func dataprocLifecycle(idle, ttl *durationpb.Duration, at bool) string {
	var out []string
	if idle != nil {
		out = append(out, "after "+dataprocDur(idle)+" idle")
	}
	if ttl != nil {
		out = append(out, "after "+dataprocDur(ttl))
	}
	if at {
		out = append(out, "at a set time")
	}
	if len(out) == 0 {
		return "Off"
	}
	return "On: " + strings.Join(out, ", ")
}

func (p dataprocClustersProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	_, c, err := p.cluster(ctx, project, path[0])
	if err != nil {
		return nil
	}
	_, region, _ := dataprocParse(path[0])
	st := c.GetStatus().GetState()
	running := st == dataprocpb.ClusterStatus_RUNNING
	edit := console.Action{ID: "edit", Label: "Edit", Submit: "Save", Fields: []console.Field{
		{Name: "workers", Label: "Worker nodes", Type: "number", Default: fmt.Sprint(c.GetConfig().GetWorkerConfig().GetNumInstances()),
			Help: "Resizes the cluster: workers are added or decommissioned."},
		{Name: "autoscalingPolicy", Label: "Autoscaling policy", Type: "text", Default: c.GetConfig().GetAutoscalingConfig().GetPolicyUri(), Help: "Its resource name; empty for none."},
		{Name: "labels", Label: "Labels", Type: "textarea", Default: dataprocPropsText(dataprocUserLabels(c.GetLabels()))},
	}}
	if dataprocClusterType(c) == "Single Node" {
		edit.Fields = edit.Fields[1:]
	}
	submit := console.Action{ID: "submit", Label: "Submit job", Href: "/dataproc/jobs/create?region=" + url.QueryEscape(region) + "&cluster=" + url.QueryEscape(c.GetClusterName())}
	stop := console.Action{ID: "stop", Label: "Stop", Confirm: "The cluster's nodes stop; its jobs end. Start it again to resume."}
	start := console.Action{ID: "start", Label: "Start"}
	diagnose := console.Action{ID: "diagnose", Label: "Diagnose", Confirm: "A diagnostic tarball of the cluster's logs and configuration is written to its staging bucket."}
	del := console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, Confirm: "The cluster and its nodes are deleted; its buckets and their contents stay."}
	if !running {
		edit.Unavailable, submit.Unavailable, diagnose.Unavailable, stop.Unavailable = "The cluster is not running.", "The cluster is not running.", "The cluster is not running.", "The cluster is not running."
	}
	if st != dataprocpb.ClusterStatus_STOPPED {
		start.Unavailable = "The cluster is not stopped."
	}
	// Driver node groups: Add one, or resize the cluster's.
	nodes := console.Action{ID: "addnodegroup", Label: "Add driver node group", Submit: "Add", Fields: []console.Field{
		{Name: "nodeGroupId", Label: "Node group ID", Type: "text", Pattern: `^[A-Za-z0-9][A-Za-z0-9_\-]{1,31}[A-Za-z0-9]$`,
			Help: "3 to 33 letters, numbers, underscores and hyphens, not beginning or ending with an underscore or hyphen. Left empty, one is generated."},
		{Name: "size", Label: "Number of nodes", Type: "number", Default: "1", Required: true}}}
	if g := c.GetConfig().GetAuxiliaryNodeGroups(); len(g) > 0 {
		stop.Unavailable = "Clusters with node groups cannot be stopped."
		nodes = console.Action{ID: "resizenodegroup", Label: "Resize node group", Submit: "Resize", Fields: []console.Field{
			{Name: "nodeGroupId", Label: "Node group", Type: "select", Options: dataprocNodeGroupIDs(c), Default: g[0].GetNodeGroupId(), Required: true},
			{Name: "size", Label: "Number of nodes", Type: "number", Default: fmt.Sprint(g[0].GetNodeGroup().GetNodeGroupConfig().GetNumInstances()), Required: true}}}
	}
	if !running {
		nodes.Unavailable = "The cluster is not running."
	}
	return []console.Action{submit, edit, nodes, start, stop, diagnose, del}
}

func (p dataprocClustersProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("no cluster")
	}
	s, c, err := p.cluster(ctx, project, path[0])
	if err != nil {
		return err
	}
	pr, region, name := dataprocParse(path[0])
	switch action {
	case "edit":
		x := &dataprocpb.Cluster{Config: &dataprocpb.ClusterConfig{}}
		var mask []string
		if w, ok := values["workers"]; ok && strings.TrimSpace(w) != fmt.Sprint(c.GetConfig().GetWorkerConfig().GetNumInstances()) {
			n, err := strconv.Atoi(strings.TrimSpace(w))
			if err != nil {
				return fmt.Errorf("worker nodes: %q is not a number", w)
			}
			x.Config.WorkerConfig = &dataprocpb.InstanceGroupConfig{NumInstances: int32(n)}
			mask = append(mask, "config.worker_config.num_instances")
		}
		if pol := strings.TrimSpace(values["autoscalingPolicy"]); pol != c.GetConfig().GetAutoscalingConfig().GetPolicyUri() {
			x.Config.AutoscalingConfig = &dataprocpb.AutoscalingConfig{PolicyUri: pol}
			mask = append(mask, "config.autoscaling_config.policy_uri")
		}
		labels, err := dataprocPairs(values["labels"])
		if err != nil {
			return fmt.Errorf("labels: %w", err)
		}
		if dataprocPropsText(labels) != dataprocPropsText(dataprocUserLabels(c.GetLabels())) {
			x.Labels = labels
			mask = append(mask, "labels")
		}
		if len(mask) == 0 {
			return nil
		}
		_, err = s.UpdateCluster(ctx, &dataprocpb.UpdateClusterRequest{ProjectId: pr, Region: region, ClusterName: name, Cluster: x, UpdateMask: &fieldmaskpb.FieldMask{Paths: mask}})
		return err
	case "addnodegroup", "resizenodegroup":
		n, err := strconv.Atoi(strings.TrimSpace(values["size"]))
		if err != nil {
			return fmt.Errorf("number of nodes: %q is not a number", values["size"])
		}
		id := strings.TrimSpace(values["nodeGroupId"])
		if action == "resizenodegroup" {
			_, err = s.ResizeNodeGroup(ctx, &dataprocpb.ResizeNodeGroupRequest{Name: path[0] + "/nodeGroups/" + id, Size: int32(n)})
			return err
		}
		_, err = s.CreateNodeGroup(ctx, &dataprocpb.CreateNodeGroupRequest{Parent: path[0], NodeGroupId: id, NodeGroup: &dataprocpb.NodeGroup{
			Roles: []dataprocpb.NodeGroup_Role{dataprocpb.NodeGroup_DRIVER}, NodeGroupConfig: &dataprocpb.InstanceGroupConfig{NumInstances: int32(n)}}})
		return err
	case "stop":
		_, err := s.StopCluster(ctx, &dataprocpb.StopClusterRequest{ProjectId: pr, Region: region, ClusterName: name})
		return err
	case "start":
		_, err := s.StartCluster(ctx, &dataprocpb.StartClusterRequest{ProjectId: pr, Region: region, ClusterName: name})
		return err
	case "diagnose":
		_, err := s.DiagnoseCluster(ctx, &dataprocpb.DiagnoseClusterRequest{ProjectId: pr, Region: region, ClusterName: name})
		return err
	case "delete":
		_, err := s.DeleteCluster(ctx, &dataprocpb.DeleteClusterRequest{ProjectId: pr, Region: region, ClusterName: name})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// ---- Jobs ----

type dataprocJobsProvider struct{ svc *dataprocService }

func (dataprocJobsProvider) ID() string    { return "dataproc-jobs" }
func (dataprocJobsProvider) Title() string { return "Jobs" }

func (p dataprocJobsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		l := dataprocJobsListing(nil, "", "")
		l.Prompt = "Dataproc lists a project's jobs. Choose one in the toolbar."
		return l, nil
	}
	var all console.Listing
	for _, loc := range dataproc.Locations() {
		resp, err := s.ListJobs(ctx, &dataprocpb.ListJobsRequest{ProjectId: project, Region: loc, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		l := dataprocJobsListing(resp.GetJobs(), project, loc)
		if all.Columns == nil {
			all = l
			continue
		}
		all.Items = append(all.Items, l.Items...)
	}
	for i := range all.Items {
		all.Items[i].Link = ""
	}
	all.Total = len(all.Items)
	return all, nil
}

var dataprocJobTypes = map[string]string{"SPARK": "Spark", "PYSPARK": "PySpark", "SPARKR": "SparkR", "SPARKSQL": "SparkSql", "HIVE": "Hive", "PIG": "Pig", "HADOOP": "Hadoop", "PRESTO": "Presto", "TRINO": "Trino", "FLINK": "Flink"}

func (dataprocJobsProvider) CreateOnPage() bool  { return true }
func (dataprocJobsProvider) CreateTitle() string { return "Submit a job" }

func (p dataprocJobsProvider) CreateForm() (string, []console.Field) {
	const info, job, props = "Job info", "Job", "Properties"
	return "Submit", []console.Field{
		{Name: "jobId", Label: "Job ID", Type: "text", Section: info, Pattern: `^[a-zA-Z0-9_\-]{1,100}$`, Help: "Letters, numbers, underscores and hyphens, at most 100. Left empty, one is generated."},
		{Name: "region", Label: "Region", Type: "select", Required: true, Section: info, Options: dataproc.Locations(), Default: "us-central1"},
		{Name: "cluster", Label: "Cluster", Type: "text", Required: true, Section: info, Help: "The name of a running cluster in the region."},
		{Name: "jobType", Label: "Job type", Type: "select", Required: true, Section: job, Default: "PYSPARK",
			Options: []string{"SPARK", "PYSPARK", "SPARKR", "SPARKSQL", "HIVE", "PIG", "HADOOP", "PRESTO", "TRINO", "FLINK"}, OptionLabels: dataprocJobTypes,
			Help: "Trino and Flink jobs need the cluster's Trino or Flink optional component; Presto is not offered on image 2.1 and later."},
		{Name: "mainPythonFile", Label: "Main python file", Type: "text", Section: job, ShowWhen: "jobType=PYSPARK", Help: "The gs:// URI of the main .py file."},
		{Name: "mainR", Label: "Main R file", Type: "text", Section: job, ShowWhen: "jobType=SPARKR", Help: "The gs:// URI of the main .R file."},
		{Name: "mainClass", Label: "Main class or jar", Type: "text", Section: job, ShowWhen: "jobType=SPARK|HADOOP|FLINK",
			Help: "A main class (with its jar in Jar files), or the gs:// URI of a jar whose manifest names one."},
		{Name: "queryFile", Label: "Query source: query file", Type: "text", Section: job, ShowWhen: "jobType=SPARKSQL|HIVE|PIG|TRINO|PRESTO", Help: "The gs:// URI of a script; or give the queries below."},
		{Name: "queries", Label: "Query source: query text", Type: "textarea", Section: job, ShowWhen: "jobType=SPARKSQL|HIVE|PIG|TRINO|PRESTO", Help: "Statements, ; separated."},
		{Name: "args", Label: "Arguments", Type: "textarea", Section: job, ShowWhen: "jobType=SPARK|PYSPARK|SPARKR|HADOOP|FLINK", Help: "One argument a line."},
		{Name: "pyFiles", Label: "Additional python files", Type: "textarea", Section: job, ShowWhen: "jobType=PYSPARK", Help: "gs:// URIs, one a line."},
		{Name: "jarFiles", Label: "Jar files", Type: "textarea", Section: job, ShowWhen: "jobType=SPARK|PYSPARK|SPARKSQL|HADOOP|HIVE|PIG|FLINK", Help: "gs:// URIs, one a line."},
		{Name: "files", Label: "Files", Type: "textarea", Section: job, ShowWhen: "jobType=SPARK|PYSPARK|SPARKR|HADOOP", Help: "gs:// URIs, one a line."},
		{Name: "driverMemory", Label: "Driver memory (MB)", Type: "number", Section: job, Help: "For a cluster with a driver node group: the driver's memory and vCores."},
		{Name: "driverVcores", Label: "Driver vCores", Type: "number", Section: job},
		{Name: "maxRestarts", Label: "Max restarts per hour", Type: "number", Section: job, Help: "Restarts the driver when it fails, at most this many times an hour. Left empty, never."},
		{Name: "properties", Label: "Properties", Type: "textarea", Section: props, Help: "One spark.property=value a line."},
		{Name: "labels", Label: "Labels", Type: "textarea", Section: props, Help: "One key=value a line."},
	}
}

func (p dataprocJobsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before submitting a job")
	}
	props, err := dataprocPairs(values["properties"])
	if err != nil {
		return "", fmt.Errorf("properties: %w", err)
	}
	labels, err := dataprocPairs(values["labels"])
	if err != nil {
		return "", fmt.Errorf("labels: %w", err)
	}
	j := &dataprocpb.Job{Placement: &dataprocpb.JobPlacement{ClusterName: strings.TrimSpace(values["cluster"])}, Labels: labels}
	if id := strings.TrimSpace(values["jobId"]); id != "" {
		j.Reference = &dataprocpb.JobReference{JobId: id}
	}
	if n := strings.TrimSpace(values["maxRestarts"]); n != "" {
		v, err := strconv.Atoi(n)
		if err != nil {
			return "", fmt.Errorf("max restarts per hour: %q is not a number", n)
		}
		j.Scheduling = &dataprocpb.JobScheduling{MaxFailuresPerHour: int32(v)}
	}
	args := dataprocLines(values["args"])
	switch values["jobType"] {
	case "SPARK":
		w := &dataprocpb.SparkJob{Args: args, JarFileUris: dataprocURIs(values["jarFiles"]), FileUris: dataprocURIs(values["files"]), Properties: props}
		if m := strings.TrimSpace(values["mainClass"]); strings.Contains(m, "://") {
			w.Driver = &dataprocpb.SparkJob_MainJarFileUri{MainJarFileUri: m}
		} else {
			w.Driver = &dataprocpb.SparkJob_MainClass{MainClass: m}
		}
		j.TypeJob = &dataprocpb.Job_SparkJob{SparkJob: w}
	case "SPARKR":
		j.TypeJob = &dataprocpb.Job_SparkRJob{SparkRJob: &dataprocpb.SparkRJob{MainRFileUri: strings.TrimSpace(values["mainR"]), Args: args, FileUris: dataprocURIs(values["files"]), Properties: props}}
	case "SPARKSQL":
		w := &dataprocpb.SparkSqlJob{JarFileUris: dataprocURIs(values["jarFiles"]), Properties: props}
		if q := strings.TrimSpace(values["queries"]); q != "" {
			w.Queries = &dataprocpb.SparkSqlJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{q}}}
		} else {
			w.Queries = &dataprocpb.SparkSqlJob_QueryFileUri{QueryFileUri: strings.TrimSpace(values["queryFile"])}
		}
		j.TypeJob = &dataprocpb.Job_SparkSqlJob{SparkSqlJob: w}
	case "HIVE":
		w := &dataprocpb.HiveJob{JarFileUris: dataprocURIs(values["jarFiles"]), Properties: props}
		if q := strings.TrimSpace(values["queries"]); q != "" {
			w.Queries = &dataprocpb.HiveJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{q}}}
		} else {
			w.Queries = &dataprocpb.HiveJob_QueryFileUri{QueryFileUri: strings.TrimSpace(values["queryFile"])}
		}
		j.TypeJob = &dataprocpb.Job_HiveJob{HiveJob: w}
	case "PIG":
		w := &dataprocpb.PigJob{JarFileUris: dataprocURIs(values["jarFiles"]), Properties: props}
		if q := strings.TrimSpace(values["queries"]); q != "" {
			w.Queries = &dataprocpb.PigJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{q}}}
		} else {
			w.Queries = &dataprocpb.PigJob_QueryFileUri{QueryFileUri: strings.TrimSpace(values["queryFile"])}
		}
		j.TypeJob = &dataprocpb.Job_PigJob{PigJob: w}
	case "HADOOP":
		w := &dataprocpb.HadoopJob{Args: args, JarFileUris: dataprocURIs(values["jarFiles"]), FileUris: dataprocURIs(values["files"]), Properties: props}
		if m := strings.TrimSpace(values["mainClass"]); strings.Contains(m, "://") {
			w.Driver = &dataprocpb.HadoopJob_MainJarFileUri{MainJarFileUri: m}
		} else {
			w.Driver = &dataprocpb.HadoopJob_MainClass{MainClass: m}
		}
		j.TypeJob = &dataprocpb.Job_HadoopJob{HadoopJob: w}
	case "PRESTO":
		w := &dataprocpb.PrestoJob{Properties: props}
		if q := strings.TrimSpace(values["queries"]); q != "" {
			w.Queries = &dataprocpb.PrestoJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{q}}}
		} else {
			w.Queries = &dataprocpb.PrestoJob_QueryFileUri{QueryFileUri: strings.TrimSpace(values["queryFile"])}
		}
		j.TypeJob = &dataprocpb.Job_PrestoJob{PrestoJob: w}
	case "TRINO":
		w := &dataprocpb.TrinoJob{Properties: props}
		if q := strings.TrimSpace(values["queries"]); q != "" {
			w.Queries = &dataprocpb.TrinoJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{q}}}
		} else {
			w.Queries = &dataprocpb.TrinoJob_QueryFileUri{QueryFileUri: strings.TrimSpace(values["queryFile"])}
		}
		j.TypeJob = &dataprocpb.Job_TrinoJob{TrinoJob: w}
	case "FLINK":
		w := &dataprocpb.FlinkJob{Args: args, JarFileUris: dataprocURIs(values["jarFiles"]), Properties: props}
		if m := strings.TrimSpace(values["mainClass"]); strings.Contains(m, "://") {
			w.Driver = &dataprocpb.FlinkJob_MainJarFileUri{MainJarFileUri: m}
		} else {
			w.Driver = &dataprocpb.FlinkJob_MainClass{MainClass: m}
		}
		j.TypeJob = &dataprocpb.Job_FlinkJob{FlinkJob: w}
	default:
		j.TypeJob = &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: strings.TrimSpace(values["mainPythonFile"]), Args: args,
			PythonFileUris: dataprocURIs(values["pyFiles"]), JarFileUris: dataprocURIs(values["jarFiles"]), FileUris: dataprocURIs(values["files"]), Properties: props}}
	}
	if m, v := strings.TrimSpace(values["driverMemory"]), strings.TrimSpace(values["driverVcores"]); m != "" || v != "" {
		mb, err1 := strconv.Atoi(m)
		vc, err2 := strconv.Atoi(v)
		if err1 != nil || err2 != nil {
			return "", fmt.Errorf("driver memory and vCores: %q and %q are not numbers", m, v)
		}
		j.DriverSchedulingConfig = &dataprocpb.DriverSchedulingConfig{MemoryMb: int32(mb), Vcores: int32(vc)}
	}
	got, err := s.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: project, Region: values["region"], Job: j})
	if err != nil {
		return "", err
	}
	return dataprocRegion(project, values["region"]) + "/jobs/" + got.GetReference().GetJobId(), nil
}

func (p dataprocJobsProvider) job(ctx context.Context, project, name string) (*dataproc.Server, *dataprocpb.Job, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return nil, nil, err
	}
	if err := dataprocOwned(project, name); err != nil {
		return nil, nil, err
	}
	pr, region, id := dataprocParse(name)
	j, err := s.GetJob(dataproc.WithAuthority(ctx, p.svc.Addr()), &dataprocpb.GetJobRequest{ProjectId: pr, Region: region, JobId: id})
	return s, j, err
}

func (p dataprocJobsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	_, j, err := p.job(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the job: " + consoleErr(err)}, nil
	}
	pr, region, id := dataprocParse(path[0])
	st := j.GetStatus().GetState()
	running := !j.GetDone()
	var workload []console.Property
	switch w := j.GetTypeJob().(type) {
	case *dataprocpb.Job_PysparkJob:
		workload = []console.Property{{Label: "Main python file", Value: w.PysparkJob.GetMainPythonFileUri()},
			{Label: "Arguments", Value: kmsOrDash(strings.Join(w.PysparkJob.GetArgs(), " "))},
			{Label: "Properties", Value: dataprocLabels(w.PysparkJob.GetProperties())}}
	case *dataprocpb.Job_SparkJob:
		main := w.SparkJob.GetMainClass()
		if main == "" {
			main = w.SparkJob.GetMainJarFileUri()
		}
		workload = []console.Property{{Label: "Main class or jar", Value: main}, {Label: "Arguments", Value: kmsOrDash(strings.Join(w.SparkJob.GetArgs(), " "))},
			{Label: "Jar files", Value: kmsOrDash(strings.Join(w.SparkJob.GetJarFileUris(), ", "))}}
	case *dataprocpb.Job_SparkRJob:
		workload = []console.Property{{Label: "Main R file", Value: w.SparkRJob.GetMainRFileUri()}, {Label: "Arguments", Value: kmsOrDash(strings.Join(w.SparkRJob.GetArgs(), " "))}}
	case *dataprocpb.Job_SparkSqlJob:
		q := w.SparkSqlJob.GetQueryFileUri()
		if q == "" {
			q = strings.Join(w.SparkSqlJob.GetQueryList().GetQueries(), "; ")
		}
		workload = []console.Property{{Label: "Query", Value: q}}
	}
	var apps []string
	for _, a := range j.GetYarnApplications() {
		apps = append(apps, a.GetName()+" ("+a.GetState().String()+")")
	}
	history := console.Listing{Columns: []string{"State", "Details", "Started"}, NameColumn: "#", Noun: "states"}
	for i, h := range append(append([]*dataprocpb.JobStatus(nil), j.GetStatusHistory()...), j.GetStatus()) {
		history.Items = append(history.Items, console.Resource{Name: fmt.Sprint(i + 1), Fields: map[string]string{
			"State": dataprocState(h.GetState().String()), "Details": kmsOrDash(h.GetDetails()), "Started": kmsTime(h.GetStateStartTime())}})
	}
	history.Total = len(history.Items)
	conf := []console.PropertyGroup{
		{Heading: "Job", Properties: []console.Property{
			{Label: "Job ID", Value: id}, {Label: "Job UUID", Value: j.GetJobUuid(), Copy: "Copy UUID"}, {Label: "Type", Value: dataprocJobType(j)},
			{Label: "Status", Value: dataprocState(st.String())}, {Label: "Status details", Value: kmsOrDash(j.GetStatus().GetDetails())},
			{Label: "Region", Value: region}, {Label: "Cluster", Value: j.GetPlacement().GetClusterName(), Link: "/dataproc/clusters/" + url.PathEscape(dataprocRegion(pr, region)+"/clusters/"+j.GetPlacement().GetClusterName()) + "?project=" + url.QueryEscape(pr)},
			{Label: "Start time", Value: kmsTimeOf(dataprocJobStart(j))}, {Label: "Elapsed time", Value: dataprocElapsed(dataprocJobStart(j), dataprocJobEnd(j))},
			{Label: "Driver output", Value: kmsOrDash(j.GetDriverOutputResourceUri()), Copy: "Copy URI"},
			{Label: "Spark applications", Value: kmsOrDash(strings.Join(apps, ", "))},
			{Label: "Labels", Value: dataprocLabels(j.GetLabels())}}},
		{Heading: "Workload", Properties: workload},
	}
	if len(j.GetYarnApplications()) > 0 && j.GetYarnApplications()[0].GetTrackingUrl() != "" {
		conf[0].Properties = append(conf[0].Properties, console.Property{Label: "Spark UI", Value: "View the application in the Spark History Server", Link: j.GetYarnApplications()[0].GetTrackingUrl()})
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Status", Value: dataprocState(st.String())}, {Label: "Region", Value: region}, {Label: "Cluster", Value: j.GetPlacement().GetClusterName()}},
		Sections: []console.Section{
			{ID: "output", Label: "Output", Kind: console.KindText, Text: dataprocDriverOutput(ctx, p.svc, j.GetDriverOutputResourceUri(), running)},
			{ID: "configuration", Label: "Configuration", Kind: console.KindProperties, Groups: conf},
			{ID: "states", Label: "Status history", Listing: history},
		},
	}, nil
}

func (p dataprocJobsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	_, j, err := p.job(ctx, project, path[0])
	if err != nil {
		return nil
	}
	stop := console.Action{ID: "stop", Label: "Stop", Destructive: true, Confirm: "The job's driver is stopped and the job ends Cancelled."}
	del := console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, Confirm: "The job is deleted; its driver output in Cloud Storage stays."}
	if j.GetDone() {
		stop.Unavailable = "The job has ended."
	} else {
		del.Unavailable = "A job can be deleted once it has ended: stop it first."
	}
	return []console.Action{stop, del}
}

func (p dataprocJobsProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	if len(path) != 1 {
		return errors.New("no job")
	}
	s, _, err := p.job(ctx, project, path[0])
	if err != nil {
		return err
	}
	pr, region, id := dataprocParse(path[0])
	switch action {
	case "stop":
		_, err := s.CancelJob(ctx, &dataprocpb.CancelJobRequest{ProjectId: pr, Region: region, JobId: id})
		return err
	case "delete":
		_, err := s.DeleteJob(ctx, &dataprocpb.DeleteJobRequest{ProjectId: pr, Region: region, JobId: id})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// ---- Workflows ----

type dataprocWorkflowsProvider struct{ svc *dataprocService }

func (dataprocWorkflowsProvider) ID() string    { return "dataproc-workflows" }
func (dataprocWorkflowsProvider) Title() string { return "Workflow templates" }

func (p dataprocWorkflowsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	listing := console.Listing{Columns: []string{"Region", "Version", "Cluster", "Jobs", "Parameters", "Created", "Updated", "Labels"}, NameColumn: "Template ID", Noun: "workflow templates"}
	if project == "" {
		listing.Prompt = "Dataproc lists a project's workflow templates. Choose one in the toolbar."
		return listing, nil
	}
	for _, loc := range dataproc.Locations() {
		resp, err := s.ListWorkflowTemplates(ctx, &dataprocpb.ListWorkflowTemplatesRequest{Parent: dataprocRegion(project, loc), PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, t := range resp.GetTemplates() {
			listing.Items = append(listing.Items, console.Resource{Name: t.GetId(), Opens: []string{t.GetName()}, Fields: map[string]string{
				"Template ID": t.GetId(), "Region": loc, "Version": fmt.Sprint(t.GetVersion()), "Cluster": dataprocPlacement(t.GetPlacement()),
				"Jobs": fmt.Sprint(len(t.GetJobs())), "Parameters": fmt.Sprint(len(t.GetParameters())), "Created": kmsTime(t.GetCreateTime()),
				"Updated": kmsTime(t.GetUpdateTime()), "Labels": dataprocLabels(t.GetLabels())}})
		}
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func dataprocPlacement(pl *dataprocpb.WorkflowTemplatePlacement) string {
	if mc := pl.GetManagedCluster(); mc != nil {
		return "Managed: " + mc.GetClusterName()
	}
	if cs := pl.GetClusterSelector(); cs != nil {
		return "Selector: " + dataprocLabels(cs.GetClusterLabels())
	}
	return "-"
}

func (dataprocWorkflowsProvider) CreateOnPage() bool  { return true }
func (dataprocWorkflowsProvider) CreateTitle() string { return "Create workflow template" }

func (p dataprocWorkflowsProvider) CreateForm() (string, []console.Field) {
	const info, cluster, job = "Template info", "Cluster", "Jobs"
	versions, versionLabels := dataprocImageVersions()
	return "Create", []console.Field{
		{Name: "templateId", Label: "Template ID", Type: "text", Required: true, Section: info, Pattern: `^[a-zA-Z0-9_\-]{3,50}$`,
			Help: "3 to 50 letters, numbers, underscores and hyphens; starts and ends with a letter or number."},
		{Name: "region", Label: "Region", Type: "select", Required: true, Section: info, Options: dataproc.Locations(), Default: "us-central1"},
		{Name: "placement", Label: "Cluster", Type: "radio", Required: true, Section: cluster, Default: "MANAGED", Options: []string{"MANAGED", "SELECTOR"},
			OptionLabels: map[string]string{"MANAGED": "Managed cluster: created for each run and deleted after", "SELECTOR": "Cluster selector: an existing cluster with these labels"}},
		{Name: "clusterName", Label: "Managed cluster name prefix", Type: "text", Section: cluster, ShowWhen: "placement=MANAGED", Default: "workflow-cluster"},
		{Name: "imageVersion", Label: "Image version", Type: "select", Section: cluster, ShowWhen: "placement=MANAGED", Options: versions, OptionLabels: versionLabels, Default: dataproc.DefaultImageVersion},
		{Name: "workers", Label: "Worker nodes", Type: "number", Section: cluster, ShowWhen: "placement=MANAGED", Default: "2"},
		{Name: "selector", Label: "Cluster labels", Type: "textarea", Section: cluster, ShowWhen: "placement=SELECTOR", Help: "One key=value a line; a cluster with all of them runs the jobs."},
		{Name: "jobs", Label: "Jobs", Type: "textarea", Required: true, Section: job,
			Help: "One job a line: STEP_ID TYPE MAIN [ARGS...] [jars=URI,URI] [after=STEP,STEP], TYPE pyspark, spark (MAIN a main class or a jar), sparkr or sparksql. Example: prepare pyspark gs://bucket/prepare.py 10"},
		{Name: "labels", Label: "Labels", Type: "textarea", Section: info, Help: "One key=value a line."},
	}
}

// dataprocOrderedJobs reads the form's Jobs lines.
func dataprocOrderedJobs(text string) ([]*dataprocpb.OrderedJob, error) {
	var out []*dataprocpb.OrderedJob
	for _, line := range dataprocLines(text) {
		f := strings.Fields(line)
		if len(f) < 3 {
			return nil, fmt.Errorf("job %q: give STEP_ID TYPE MAIN_FILE", line)
		}
		j := &dataprocpb.OrderedJob{StepId: f[0]}
		var args, jars []string
		for _, a := range f[3:] {
			if after, ok := strings.CutPrefix(a, "after="); ok {
				j.PrerequisiteStepIds = strings.Split(after, ",")
				continue
			}
			if js, ok := strings.CutPrefix(a, "jars="); ok {
				jars = strings.Split(js, ",")
				continue
			}
			args = append(args, a)
		}
		switch strings.ToLower(f[1]) {
		case "pyspark":
			j.JobType = &dataprocpb.OrderedJob_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: f[2], Args: args, JarFileUris: jars}}
		case "spark":
			w := &dataprocpb.SparkJob{Args: args, JarFileUris: jars}
			if strings.Contains(f[2], "://") {
				w.Driver = &dataprocpb.SparkJob_MainJarFileUri{MainJarFileUri: f[2]}
			} else {
				w.Driver = &dataprocpb.SparkJob_MainClass{MainClass: f[2]}
			}
			j.JobType = &dataprocpb.OrderedJob_SparkJob{SparkJob: w}
		case "sparkr":
			j.JobType = &dataprocpb.OrderedJob_SparkRJob{SparkRJob: &dataprocpb.SparkRJob{MainRFileUri: f[2], Args: args}}
		case "sparksql":
			j.JobType = &dataprocpb.OrderedJob_SparkSqlJob{SparkSqlJob: &dataprocpb.SparkSqlJob{Queries: &dataprocpb.SparkSqlJob_QueryFileUri{QueryFileUri: f[2]}}}
		default:
			return nil, fmt.Errorf("job %q: type %q is not pyspark, spark, sparkr or sparksql", line, f[1])
		}
		out = append(out, j)
	}
	return out, nil
}

func (p dataprocWorkflowsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a workflow template")
	}
	jobs, err := dataprocOrderedJobs(values["jobs"])
	if err != nil {
		return "", err
	}
	labels, err := dataprocPairs(values["labels"])
	if err != nil {
		return "", fmt.Errorf("labels: %w", err)
	}
	t := &dataprocpb.WorkflowTemplate{Id: strings.TrimSpace(values["templateId"]), Jobs: jobs, Labels: labels, Placement: &dataprocpb.WorkflowTemplatePlacement{}}
	if values["placement"] == "SELECTOR" {
		sel, err := dataprocPairs(values["selector"])
		if err != nil {
			return "", fmt.Errorf("cluster labels: %w", err)
		}
		t.Placement.Placement = &dataprocpb.WorkflowTemplatePlacement_ClusterSelector{ClusterSelector: &dataprocpb.ClusterSelector{ClusterLabels: sel}}
	} else {
		n, err := strconv.Atoi(strings.TrimSpace(values["workers"]))
		if err != nil {
			return "", fmt.Errorf("worker nodes: %q is not a number", values["workers"])
		}
		t.Placement.Placement = &dataprocpb.WorkflowTemplatePlacement_ManagedCluster{ManagedCluster: &dataprocpb.ManagedCluster{
			ClusterName: strings.TrimSpace(values["clusterName"]), Config: &dataprocpb.ClusterConfig{
				SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: values["imageVersion"]}, WorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: int32(n)}}}}
	}
	got, err := s.CreateWorkflowTemplate(ctx, &dataprocpb.CreateWorkflowTemplateRequest{Parent: dataprocRegion(project, values["region"]), Template: t})
	if err != nil {
		return "", err
	}
	return got.GetName(), nil
}

func (p dataprocWorkflowsProvider) template(ctx context.Context, project, name string) (*dataproc.Server, *dataprocpb.WorkflowTemplate, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return nil, nil, err
	}
	if err := dataprocOwned(project, name); err != nil {
		return nil, nil, err
	}
	t, err := s.GetWorkflowTemplate(ctx, &dataprocpb.GetWorkflowTemplateRequest{Name: name})
	return s, t, err
}

func dataprocOrderedType(j *dataprocpb.OrderedJob) (string, string) {
	switch w := j.GetJobType().(type) {
	case *dataprocpb.OrderedJob_PysparkJob:
		return "PySpark", w.PysparkJob.GetMainPythonFileUri()
	case *dataprocpb.OrderedJob_SparkJob:
		if w.SparkJob.GetMainClass() != "" {
			return "Spark", w.SparkJob.GetMainClass()
		}
		return "Spark", w.SparkJob.GetMainJarFileUri()
	case *dataprocpb.OrderedJob_SparkRJob:
		return "SparkR", w.SparkRJob.GetMainRFileUri()
	case *dataprocpb.OrderedJob_SparkSqlJob:
		return "SparkSql", w.SparkSqlJob.GetQueryFileUri()
	case *dataprocpb.OrderedJob_HiveJob:
		return "Hive", "-"
	case *dataprocpb.OrderedJob_PigJob:
		return "Pig", "-"
	case *dataprocpb.OrderedJob_HadoopJob:
		return "Hadoop", "-"
	}
	return "-", "-"
}

func (p dataprocWorkflowsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	s, t, err := p.template(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the workflow template: " + consoleErr(err)}, nil
	}
	pr, region, _ := dataprocParse(path[0])
	jobs := console.Listing{Columns: []string{"Type", "Main file", "Prerequisite steps"}, NameColumn: "Step ID", Noun: "jobs"}
	for _, j := range t.GetJobs() {
		typ, main := dataprocOrderedType(j)
		jobs.Items = append(jobs.Items, console.Resource{Name: j.GetStepId(), Fields: map[string]string{"Step ID": j.GetStepId(), "Type": typ, "Main file": main,
			"Prerequisite steps": kmsOrDash(strings.Join(j.GetPrerequisiteStepIds(), ", "))}})
	}
	jobs.Total = len(jobs.Items)
	params := console.Listing{Columns: []string{"Fields", "Description"}, NameColumn: "Name", Noun: "parameters"}
	for _, prm := range t.GetParameters() {
		params.Items = append(params.Items, console.Resource{Name: prm.GetName(), Fields: map[string]string{"Name": prm.GetName(),
			"Fields": strings.Join(prm.GetFields(), ", "), "Description": kmsOrDash(prm.GetDescription())}})
	}
	params.Total = len(params.Items)
	runs := console.Listing{Columns: []string{"Status", "Cluster", "Start time", "Elapsed time", "Jobs"}, NameColumn: "Workflow ID", Noun: "runs"}
	for _, op := range s.WorkflowRuns(dataprocRegion(pr, region), t.GetName()) {
		var md dataprocpb.WorkflowMetadata
		_ = op.GetMetadata().UnmarshalTo(&md)
		done := 0
		for _, n := range md.GetGraph().GetNodes() {
			if n.GetState() == dataprocpb.WorkflowNode_COMPLETED {
				done++
			}
		}
		state := dataprocState(md.GetState().String())
		if op.GetError() != nil {
			state = "Failed: " + op.GetError().GetMessage()
		}
		runs.Items = append(runs.Items, console.Resource{Name: lastSegment(op.GetName()), Fields: map[string]string{
			"Workflow ID": lastSegment(op.GetName()), "Status": state, "Cluster": kmsOrDash(md.GetClusterName()), "Start time": kmsTime(md.GetStartTime()),
			"Elapsed time": dataprocElapsed(md.GetStartTime().AsTime(), md.GetEndTime().AsTime()),
			"Jobs":         fmt.Sprintf("%d of %d completed", done, len(md.GetGraph().GetNodes()))}})
	}
	runs.Total = len(runs.Items)
	return console.Detail{
		Summary: []console.Property{{Label: "Region", Value: region}, {Label: "Version", Value: fmt.Sprint(t.GetVersion())}},
		Sections: []console.Section{
			{ID: "configuration", Label: "Configuration", Kind: console.KindProperties, Groups: []console.PropertyGroup{
				{Heading: "Template", Properties: []console.Property{
					{Label: "Template ID", Value: t.GetId()}, {Label: "Resource name", Value: t.GetName()}, {Label: "Version", Value: fmt.Sprint(t.GetVersion())},
					{Label: "Cluster", Value: dataprocPlacement(t.GetPlacement())}, {Label: "DAG timeout", Value: dataprocDur(t.GetDagTimeout())},
					{Label: "Created", Value: kmsTime(t.GetCreateTime())}, {Label: "Updated", Value: kmsTime(t.GetUpdateTime())},
					{Label: "Labels", Value: dataprocLabels(t.GetLabels())}}}}},
			{ID: "jobs", Label: "Jobs", Listing: jobs},
			{ID: "parameters", Label: "Parameters", Listing: params},
			{ID: "runs", Label: "Workflows", Listing: runs},
		},
	}, nil
}

func (p dataprocWorkflowsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	_, t, err := p.template(ctx, project, path[0])
	if err != nil {
		return nil
	}
	run := console.Action{ID: "run", Label: "Run"}
	for _, prm := range t.GetParameters() {
		run.Fields = append(run.Fields, console.Field{Name: "param." + prm.GetName(), Label: prm.GetName(), Type: "text", Required: true, Help: prm.GetDescription()})
	}
	return []console.Action{run, {ID: "delete", Label: "Delete", Destructive: true, Leaves: true, Confirm: "The template and its versions are deleted; running workflows go on."}}
}

func (p dataprocWorkflowsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("no workflow template")
	}
	s, t, err := p.template(ctx, project, path[0])
	if err != nil {
		return err
	}
	switch action {
	case "run":
		params := map[string]string{}
		for k, v := range values {
			if n, ok := strings.CutPrefix(k, "param."); ok {
				params[n] = v
			}
		}
		_, err := s.InstantiateWorkflowTemplate(ctx, &dataprocpb.InstantiateWorkflowTemplateRequest{Name: t.GetName(), Parameters: params})
		return err
	case "delete":
		_, err := s.DeleteWorkflowTemplate(ctx, &dataprocpb.DeleteWorkflowTemplateRequest{Name: t.GetName()})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// ---- Autoscaling policies ----

type dataprocPoliciesProvider struct{ svc *dataprocService }

func (dataprocPoliciesProvider) ID() string    { return "dataproc-autoscaling" }
func (dataprocPoliciesProvider) Title() string { return "Autoscaling policies" }

func (p dataprocPoliciesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	listing := console.Listing{Columns: []string{"Region", "Primary workers", "Secondary workers", "Scale up factor", "Scale down factor", "Cooldown"}, NameColumn: "Name", Noun: "autoscaling policies"}
	if project == "" {
		listing.Prompt = "Dataproc lists a project's autoscaling policies. Choose one in the toolbar."
		return listing, nil
	}
	for _, loc := range dataproc.Locations() {
		resp, err := s.ListAutoscalingPolicies(ctx, &dataprocpb.ListAutoscalingPoliciesRequest{Parent: dataprocRegion(project, loc), PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, x := range resp.GetPolicies() {
			y := x.GetBasicAlgorithm().GetYarnConfig()
			listing.Items = append(listing.Items, console.Resource{Name: x.GetId(), Opens: []string{x.GetName()}, Fields: map[string]string{
				"Name": x.GetId(), "Region": loc,
				"Primary workers":   fmt.Sprintf("%d to %d", x.GetWorkerConfig().GetMinInstances(), x.GetWorkerConfig().GetMaxInstances()),
				"Secondary workers": fmt.Sprintf("%d to %d", x.GetSecondaryWorkerConfig().GetMinInstances(), x.GetSecondaryWorkerConfig().GetMaxInstances()),
				"Scale up factor":   fmt.Sprint(y.GetScaleUpFactor()), "Scale down factor": fmt.Sprint(y.GetScaleDownFactor()),
				"Cooldown": dataprocDur(x.GetBasicAlgorithm().GetCooldownPeriod())}})
		}
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func (dataprocPoliciesProvider) CreateOnPage() bool  { return true }
func (dataprocPoliciesProvider) CreateTitle() string { return "Create autoscaling policy" }

func dataprocPolicyFields(x *dataprocpb.AutoscalingPolicy) []console.Field {
	const workers, yarn = "Worker configuration", "Scaling"
	y := x.GetBasicAlgorithm().GetYarnConfig()
	def := func(v, d string) string {
		if x == nil {
			return d
		}
		return v
	}
	return []console.Field{
		{Name: "minWorkers", Label: "Primary workers: minimum instances", Type: "number", Section: workers, Default: def(fmt.Sprint(x.GetWorkerConfig().GetMinInstances()), "2")},
		{Name: "maxWorkers", Label: "Primary workers: maximum instances", Type: "number", Required: true, Section: workers, Default: def(fmt.Sprint(x.GetWorkerConfig().GetMaxInstances()), "10")},
		{Name: "minSecondary", Label: "Secondary workers: minimum instances", Type: "number", Section: workers, Default: def(fmt.Sprint(x.GetSecondaryWorkerConfig().GetMinInstances()), "0")},
		{Name: "maxSecondary", Label: "Secondary workers: maximum instances", Type: "number", Section: workers, Default: def(fmt.Sprint(x.GetSecondaryWorkerConfig().GetMaxInstances()), "0")},
		{Name: "scaleUp", Label: "Scale up factor", Type: "text", Required: true, Section: yarn, Default: def(fmt.Sprint(y.GetScaleUpFactor()), "0.5"), Help: "0.0 to 1.0: the fraction of pending memory added."},
		{Name: "scaleDown", Label: "Scale down factor", Type: "text", Required: true, Section: yarn, Default: def(fmt.Sprint(y.GetScaleDownFactor()), "1.0"), Help: "0.0 to 1.0: the fraction of available memory removed."},
		{Name: "decommission", Label: "Graceful decommission timeout (seconds)", Type: "number", Required: true, Section: yarn,
			Default: def(fmt.Sprint(int(y.GetGracefulDecommissionTimeout().AsDuration().Seconds())), "3600"), Help: "0 to 86400."},
		{Name: "cooldown", Label: "Cooldown duration (seconds)", Type: "number", Section: yarn,
			Default: def(fmt.Sprint(int(x.GetBasicAlgorithm().GetCooldownPeriod().AsDuration().Seconds())), "120"), Help: "120 to 86400."},
	}
}

func (p dataprocPoliciesProvider) CreateForm() (string, []console.Field) {
	return "Create", append([]console.Field{
		{Name: "policyId", Label: "Policy name", Type: "text", Required: true, Section: "Policy", Pattern: `^[a-zA-Z0-9_\-]{3,50}$`,
			Help: "3 to 50 letters, numbers, underscores and hyphens; starts and ends with a letter or number."},
		{Name: "region", Label: "Region", Type: "select", Required: true, Section: "Policy", Options: dataproc.Locations(), Default: "us-central1"},
	}, dataprocPolicyFields(nil)...)
}

func dataprocPolicyOf(id string, values map[string]string) (*dataprocpb.AutoscalingPolicy, error) {
	num := func(k string) (int32, error) {
		v := strings.TrimSpace(values[k])
		if v == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("%q is not a number", v)
		}
		return int32(n), nil
	}
	flt := func(k string) (float64, error) {
		f, err := strconv.ParseFloat(strings.TrimSpace(values[k]), 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a number", values[k])
		}
		return f, nil
	}
	var errs []error
	n := func(k string) int32 { v, err := num(k); errs = append(errs, err); return v }
	f := func(k string) float64 { v, err := flt(k); errs = append(errs, err); return v }
	x := &dataprocpb.AutoscalingPolicy{Id: id,
		WorkerConfig:          &dataprocpb.InstanceGroupAutoscalingPolicyConfig{MinInstances: n("minWorkers"), MaxInstances: n("maxWorkers")},
		SecondaryWorkerConfig: &dataprocpb.InstanceGroupAutoscalingPolicyConfig{MinInstances: n("minSecondary"), MaxInstances: n("maxSecondary")},
		Algorithm: &dataprocpb.AutoscalingPolicy_BasicAlgorithm{BasicAlgorithm: &dataprocpb.BasicAutoscalingAlgorithm{
			CooldownPeriod: durationpb.New(time.Duration(n("cooldown")) * time.Second),
			Config: &dataprocpb.BasicAutoscalingAlgorithm_YarnConfig{YarnConfig: &dataprocpb.BasicYarnAutoscalingConfig{
				ScaleUpFactor: f("scaleUp"), ScaleDownFactor: f("scaleDown"),
				GracefulDecommissionTimeout: durationpb.New(time.Duration(n("decommission")) * time.Second)}}}}}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if x.SecondaryWorkerConfig.MaxInstances == 0 {
		x.SecondaryWorkerConfig = nil
	}
	return x, nil
}

func (p dataprocPoliciesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating an autoscaling policy")
	}
	x, err := dataprocPolicyOf(strings.TrimSpace(values["policyId"]), values)
	if err != nil {
		return "", err
	}
	got, err := s.CreateAutoscalingPolicy(ctx, &dataprocpb.CreateAutoscalingPolicyRequest{Parent: dataprocRegion(project, values["region"]), Policy: x})
	if err != nil {
		return "", err
	}
	return got.GetName(), nil
}

func (p dataprocPoliciesProvider) policy(ctx context.Context, project, name string) (*dataproc.Server, *dataprocpb.AutoscalingPolicy, error) {
	s, err := dataprocAPI(p.svc)
	if err != nil {
		return nil, nil, err
	}
	if err := dataprocOwned(project, name); err != nil {
		return nil, nil, err
	}
	x, err := s.GetAutoscalingPolicy(ctx, &dataprocpb.GetAutoscalingPolicyRequest{Name: name})
	return s, x, err
}

func (p dataprocPoliciesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	_, x, err := p.policy(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the autoscaling policy: " + consoleErr(err)}, nil
	}
	_, region, _ := dataprocParse(path[0])
	y := x.GetBasicAlgorithm().GetYarnConfig()
	return console.Detail{
		Summary: []console.Property{{Label: "Region", Value: region}},
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
			{Heading: "Policy", Properties: []console.Property{{Label: "Name", Value: x.GetId()}, {Label: "Resource name", Value: x.GetName(), Copy: "Copy name"},
				{Label: "Labels", Value: dataprocLabels(x.GetLabels())}}},
			{Heading: "Worker configuration", Properties: []console.Property{
				{Label: "Primary workers", Value: fmt.Sprintf("%d to %d (weight %d)", x.GetWorkerConfig().GetMinInstances(), x.GetWorkerConfig().GetMaxInstances(), x.GetWorkerConfig().GetWeight())},
				{Label: "Secondary workers", Value: fmt.Sprintf("%d to %d", x.GetSecondaryWorkerConfig().GetMinInstances(), x.GetSecondaryWorkerConfig().GetMaxInstances())}}},
			{Heading: "Scaling", Properties: []console.Property{
				{Label: "Scale up factor", Value: fmt.Sprint(y.GetScaleUpFactor())}, {Label: "Scale down factor", Value: fmt.Sprint(y.GetScaleDownFactor())},
				{Label: "Scale up minimum worker fraction", Value: fmt.Sprint(y.GetScaleUpMinWorkerFraction())},
				{Label: "Scale down minimum worker fraction", Value: fmt.Sprint(y.GetScaleDownMinWorkerFraction())},
				{Label: "Graceful decommission timeout", Value: dataprocDur(y.GetGracefulDecommissionTimeout())},
				{Label: "Cooldown duration", Value: dataprocDur(x.GetBasicAlgorithm().GetCooldownPeriod())}}}}}},
	}, nil
}

func (p dataprocPoliciesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	_, x, err := p.policy(ctx, project, path[0])
	if err != nil {
		return nil
	}
	return []console.Action{{ID: "edit", Label: "Edit", Submit: "Save", Fields: dataprocPolicyFields(x)},
		{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, Confirm: "The policy is deleted. A policy a cluster uses cannot be."}}
}

func (p dataprocPoliciesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("no autoscaling policy")
	}
	s, x, err := p.policy(ctx, project, path[0])
	if err != nil {
		return err
	}
	switch action {
	case "edit":
		y, err := dataprocPolicyOf(x.GetId(), values)
		if err != nil {
			return err
		}
		y.Name = x.GetName()
		_, err = s.UpdateAutoscalingPolicy(ctx, &dataprocpb.UpdateAutoscalingPolicyRequest{Policy: y})
		return err
	case "delete":
		_, err := s.DeleteAutoscalingPolicy(ctx, &dataprocpb.DeleteAutoscalingPolicyRequest{Name: x.GetName()})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// dataprocNodeGroups is a cluster's auxiliary node groups, each as ID
// (ROLE): its instances.
func dataprocNodeGroups(cfg *dataprocpb.ClusterConfig) string {
	var out []string
	for _, g := range cfg.GetAuxiliaryNodeGroups() {
		var roles []string
		for _, r := range g.GetNodeGroup().GetRoles() {
			roles = append(roles, r.String())
		}
		out = append(out, g.GetNodeGroupId()+" ("+strings.Join(roles, ", ")+"): "+strings.Join(g.GetNodeGroup().GetNodeGroupConfig().GetInstanceNames(), ", "))
	}
	return strings.Join(out, "; ")
}

// dataprocNodeGroupIDs are a cluster's node groups' IDs.
func dataprocNodeGroupIDs(c *dataprocpb.Cluster) []string {
	var ids []string
	for _, g := range c.GetConfig().GetAuxiliaryNodeGroups() {
		ids = append(ids, g.GetNodeGroupId())
	}
	return ids
}
