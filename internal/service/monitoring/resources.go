package monitoring

import (
	labelpb "google.golang.org/genproto/googleapis/api/label"
	"google.golang.org/genproto/googleapis/api/monitoredres"
)

// writableResources are the monitored resource types user-defined metrics
// may be written against (docs.cloud.google.com/monitoring/custom-metrics,
// "Choose a monitored resource type", read 2026-11-19).
var writableResources = map[string]bool{
	"aws_ec2_instance": true, "dataflow_job": true, "gae_instance": true, "gce_instance": true,
	"generic_node": true, "generic_task": true, "gke_container": true, "global": true,
	"k8s_cluster": true, "k8s_container": true, "k8s_node": true, "k8s_pod": true,
}

// resourceDescriptors are the monitored resource types served: the ones
// user-defined metrics are written against, and the ones CloudBurrow's
// built-in metrics and log entries name. Labels as Google's monitored
// resource list publishes them
// (docs.cloud.google.com/monitoring/api/resources).
var resourceDescriptors = []*monitoredres.MonitoredResourceDescriptor{
	res("aws_ec2_instance", "Amazon EC2 Instance", "A VM instance in Amazon EC2.",
		"project_id", "instance_id", "region", "aws_account"),
	res("cloud_run_job", "Cloud Run Job", "A Cloud Run job.",
		"project_id", "job_name", "location"),
	res("cloud_run_revision", "Cloud Run Revision", "A revision in Cloud Run.",
		"project_id", "service_name", "revision_name", "location", "configuration_name"),
	res("cloud_tasks_queue", "Cloud Tasks Queue", "A Cloud Tasks queue.",
		"project_id", "queue_id", "target_type", "location"),
	res("consumed_api", "Consumed API", "An API consumed by a Google Cloud project.",
		"project_id", "service", "method", "version", "location", "credential_id"),
	res("dataflow_job", "Dataflow Job", "A Dataflow job.",
		"project_id", "job_name", "region"),
	res("gae_instance", "GAE Instance", "An App Engine instance.",
		"project_id", "module_id", "version_id", "instance_id", "location"),
	res("gce_instance", "VM Instance", "A virtual machine instance hosted in Compute Engine.",
		"project_id", "instance_id", "zone"),
	res("gcs_bucket", "GCS Bucket", "A Cloud Storage bucket.",
		"project_id", "bucket_name", "location"),
	res("generic_node", "Generic Node", "A generic node identifies a machine or other computational resource for which no more specific resource type is applicable.",
		"project_id", "location", "namespace", "node_id"),
	res("generic_task", "Generic Task", "A generic task identifies an application process for which no more specific resource is applicable.",
		"project_id", "location", "namespace", "job", "task_id"),
	res("gke_container", "GKE Container", "A container in Google Kubernetes Engine (legacy).",
		"project_id", "cluster_name", "namespace_id", "instance_id", "pod_id", "container_name", "zone"),
	res("global", "Global", "A global resource identifies a resource that is not bound to a specific location.",
		"project_id"),
	res("k8s_cluster", "Kubernetes Cluster", "A Kubernetes cluster.",
		"project_id", "location", "cluster_name"),
	res("k8s_container", "Kubernetes Container", "A Kubernetes container instance.",
		"project_id", "location", "cluster_name", "namespace_name", "pod_name", "container_name"),
	res("k8s_node", "Kubernetes Node", "A Kubernetes node.",
		"project_id", "location", "cluster_name", "node_name"),
	res("k8s_pod", "Kubernetes Pod", "A Kubernetes pod.",
		"project_id", "location", "cluster_name", "namespace_name", "pod_name"),
	res("pubsub_subscription", "Cloud Pub/Sub Subscription", "A Pub/Sub subscription.",
		"project_id", "subscription_id"),
	res("uptime_url", "Uptime Check URL", "A URL checked by an uptime check.",
		"project_id", "host"),
	res("pubsub_topic", "Cloud Pub/Sub Topic", "A Pub/Sub topic.",
		"project_id", "topic_id"),
}

func res(t, display, desc string, labels ...string) *monitoredres.MonitoredResourceDescriptor {
	d := &monitoredres.MonitoredResourceDescriptor{Type: t, DisplayName: display, Description: desc,
		LaunchStage: 0}
	for _, l := range labels {
		d.Labels = append(d.Labels, &labelpb.LabelDescriptor{Key: l, ValueType: labelpb.LabelDescriptor_STRING})
	}
	return d
}

// resourceDescriptor finds a type, or nil.
func resourceDescriptor(t string) *monitoredres.MonitoredResourceDescriptor {
	for _, d := range resourceDescriptors {
		if d.GetType() == t {
			return d
		}
	}
	return nil
}

// hasResourceLabel is whether resource type t has label key k.
func hasResourceLabel(t, k string) bool {
	d := resourceDescriptor(t)
	for _, l := range d.GetLabels() {
		if l.GetKey() == k {
			return true
		}
	}
	return false
}
