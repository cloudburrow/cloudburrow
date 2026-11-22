package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	pubsubadmin "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	sqladminapi "google.golang.org/api/sqladmin/v1"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// Cloud Monitoring's resource inventories (#2146): each served product's
// resources as monitored resources, with the metadata Google's group
// criteria match (display name, labels and network tags, region), read
// through the product's own API at each group call, so a group's members
// are the project's real VMs, Cloud SQL instances, GKE clusters, buckets
// and topics.

// monitoringInventoryWiring is what the inventories reach.
type monitoringInventoryWiring struct {
	cfg     config.Config
	compute *computeService
	gke     *gkeService
	storage func() *netfwd.Forwarder
	pubsub  func() *netfwd.Forwarder
}

// inventories are the products served.
func (w monitoringInventoryWiring) inventories() []monitoring.ResourceInventory {
	var out []monitoring.ResourceInventory
	if w.compute != nil {
		out = append(out, w.computeInstances)
	}
	if w.gke != nil {
		out = append(out, w.gkeClusters)
	}
	if sqlAdminEnabled(w.cfg) && w.cfg.Endpoints.SQLAdmin > 0 {
		out = append(out, w.sqlInstances)
	}
	if serviceEnabled(w.cfg, config.ServiceStorage) && w.storage != nil {
		out = append(out, w.buckets)
	}
	if serviceEnabled(w.cfg, config.ServicePubSub) && w.pubsub != nil {
		out = append(out, w.topics)
	}
	return out
}

func strMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, x := range m {
		s, _ := x.(string)
		out[k] = s
	}
	return out
}

// computeInstances are the project's VMs as gce_instance resources.
func (w monitoringInventoryWiring) computeInstances(_ context.Context, project string) ([]monitoring.InventoryResource, error) {
	api := w.compute.API()
	if api == nil {
		return nil, errors.New(computeNotStarted)
	}
	m, err := api.Call("GET", project, "aggregated/instances", nil, nil)
	if err != nil {
		return nil, err
	}
	var out []monitoring.InventoryResource
	for _, vm := range aggregatedItems(m, "instances") {
		zone := path.Base(vpcStr(vm, "zone"))
		var tags []string
		if t, ok := vm["tags"].(map[string]any); ok {
			items, _ := t["items"].([]any)
			for _, it := range items {
				if s, ok := it.(string); ok {
					tags = append(tags, s)
				}
			}
		}
		spot := "false"
		if sch, ok := vm["scheduling"].(map[string]any); ok && sch["provisioningModel"] == "SPOT" {
			spot = "true"
		}
		name := vpcStr(vm, "name")
		out = append(out, monitoring.InventoryResource{
			Resource: &monitoredres.MonitoredResource{Type: "gce_instance",
				Labels: map[string]string{"project_id": project, "instance_id": vpcStr(vm, "id"), "zone": zone}},
			Metadata: monitoring.ResourceMetadata{Name: name, Region: monitoring.RegionOf(zone), UserLabels: strMap(vm["labels"]),
				NetworkTags: tags,
				SystemLabels: map[string]string{"name": name, "machine_type": path.Base(vpcStr(vm, "machineType")),
					"spot_instance": spot, "state": vpcStr(vm, "status")}},
		})
	}
	return out, nil
}

// gkeClusters are the project's clusters as k8s_cluster resources.
func (w monitoringInventoryWiring) gkeClusters(ctx context.Context, project string) ([]monitoring.InventoryResource, error) {
	api := w.gke.API()
	if api == nil {
		return nil, errors.New("the Kubernetes Engine API has not started")
	}
	resp, err := api.ListClusters(ctx, &containerpb.ListClustersRequest{Parent: "projects/" + project + "/locations/-"})
	if err != nil {
		return nil, err
	}
	var out []monitoring.InventoryResource
	for _, c := range resp.GetClusters() {
		out = append(out, monitoring.InventoryResource{
			Resource: &monitoredres.MonitoredResource{Type: "k8s_cluster",
				Labels: map[string]string{"project_id": project, "location": c.GetLocation(), "cluster_name": c.GetName()}},
			Metadata: monitoring.ResourceMetadata{Name: c.GetName(), Region: monitoring.RegionOf(c.GetLocation()), UserLabels: c.GetResourceLabels(),
				SystemLabels: map[string]string{"name": c.GetName()}},
		})
	}
	return out, nil
}

// sqlInstances are the project's Cloud SQL instances as cloudsql_database
// resources, through the Admin API.
func (w monitoringInventoryWiring) sqlInstances(ctx context.Context, project string) ([]monitoring.InventoryResource, error) {
	host := w.cfg.BindAddress
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	svc, err := sqladminapi.NewService(ctx, option.WithEndpoint("http://"+net.JoinHostPort(host, strconv.Itoa(w.cfg.Endpoints.SQLAdmin))+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		return nil, err
	}
	var out []monitoring.InventoryResource
	err = svc.Instances.List(project).Context(ctx).Pages(ctx, func(l *sqladminapi.InstancesListResponse) error {
		for _, in := range l.Items {
			var labels map[string]string
			sys := map[string]string{"name": in.Name, "database_version": in.DatabaseVersion}
			if in.Settings != nil {
				labels = in.Settings.UserLabels
				sys["tier"] = in.Settings.Tier
			}
			out = append(out, monitoring.InventoryResource{
				Resource: &monitoredres.MonitoredResource{Type: "cloudsql_database",
					Labels: map[string]string{"project_id": project, "database_id": project + ":" + in.Name, "region": in.Region}},
				Metadata: monitoring.ResourceMetadata{Name: in.Name, Region: monitoring.RegionOf(in.Region), UserLabels: labels, SystemLabels: sys},
			})
		}
		return nil
	})
	return out, err
}

// buckets are the project's buckets as gcs_bucket resources.
func (w monitoringInventoryWiring) buckets(ctx context.Context, project string) ([]monitoring.InventoryResource, error) {
	addr := fwdAddr(w.storage)
	if addr == "" {
		return nil, errors.New("Cloud Storage is not reachable")
	}
	c, err := storageProvider{endpoint: addr}.storageClient(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var out []monitoring.InventoryResource
	it := c.Buckets(ctx, project)
	for {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return out, err
		}
		loc := strings.ToLower(b.Location)
		out = append(out, monitoring.InventoryResource{
			Resource: &monitoredres.MonitoredResource{Type: "gcs_bucket",
				Labels: map[string]string{"project_id": project, "bucket_name": b.Name, "location": loc}},
			Metadata: monitoring.ResourceMetadata{Name: b.Name, Region: monitoring.RegionOf(loc), UserLabels: b.Labels,
				SystemLabels: map[string]string{"name": b.Name, "storage_class": b.StorageClass}},
		})
	}
	return out, nil
}

// topics are the project's topics as pubsub_topic resources; a topic has
// no location, so its region is global.
func (w monitoringInventoryWiring) topics(ctx context.Context, project string) ([]monitoring.InventoryResource, error) {
	addr := fwdAddr(w.pubsub)
	if addr == "" {
		return nil, errors.New("Pub/Sub is not reachable")
	}
	c, err := pubsubadmin.NewTopicAdminClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var out []monitoring.InventoryResource
	it := c.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: "projects/" + project})
	for {
		t, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return out, err
		}
		id := path.Base(t.GetName())
		out = append(out, monitoring.InventoryResource{
			Resource: &monitoredres.MonitoredResource{Type: "pubsub_topic", Labels: map[string]string{"project_id": project, "topic_id": id}},
			Metadata: monitoring.ResourceMetadata{Name: id, Region: "global", UserLabels: t.GetLabels(), SystemLabels: map[string]string{"name": id}},
		})
	}
	return out, nil
}
