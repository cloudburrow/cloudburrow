package apphub

import "strings"

// LogEntry's App Hub metadata (#2096). Google's LogEntry REST reference
// documents the output-only apphub field ("AppHub application metadata
// associated with this LogEntry. May be empty if there is no associated
// AppHub application or multiple associated applications"): the
// application (container, location, id) and the service or workload (id,
// environmentType, criticalityType) the entry's resource is registered as.
// Docs re-read 2026-11-21: https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry.

// LogMetadata is a LogEntry's apphub, flattened to its JSON paths
// (apphub.application.id, apphub.service.environmentType, ...).
type LogMetadata map[string]string

// LogMetadata is the apphub of an entry whose resource has uri: set when
// exactly one application registers it, as a service or a workload.
func (s *Server) LogMetadata(uri string) (LogMetadata, bool) {
	if uri == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var found []LogMetadata
	for name, sv := range s.services {
		if sv.GetServiceReference().GetUri() != uri {
			continue
		}
		m := serviceRE.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		found = append(found, logMetadata(m[2], m[3], m[4], "service", m[5],
			sv.GetAttributes().GetEnvironment().GetType().String(), sv.GetAttributes().GetCriticality().GetType().String()))
	}
	for name, wl := range s.workloads {
		if wl.GetWorkloadReference().GetUri() != uri {
			continue
		}
		m := workloadRE.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		found = append(found, logMetadata(m[2], m[3], m[4], "workload", m[5],
			wl.GetAttributes().GetEnvironment().GetType().String(), wl.GetAttributes().GetCriticality().GetType().String()))
	}
	if len(found) != 1 {
		return nil, false
	}
	return found[0], true
}

func logMetadata(host, loc, app, kind, id, env, crit string) LogMetadata {
	m := LogMetadata{
		"apphub.application.container": "projects/" + host,
		"apphub.application.location":  loc,
		"apphub.application.id":        app,
		"apphub." + kind + ".id":       id,
	}
	if env != "" && !strings.HasSuffix(env, "UNSPECIFIED") {
		m["apphub."+kind+".environmentType"] = env
	}
	if crit != "" && !strings.HasSuffix(crit, "UNSPECIFIED") {
		m["apphub."+kind+".criticalityType"] = crit
	}
	return m
}

// Nested is m as the LogEntry's JSON nests it, under "apphub".
func (m LogMetadata) Nested() map[string]any {
	out := map[string]any{}
	for path, v := range m {
		parts := strings.Split(strings.TrimPrefix(path, "apphub."), ".")
		cur := out
		for _, p := range parts[:len(parts)-1] {
			next, ok := cur[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[p] = next
			}
			cur = next
		}
		cur[parts[len(parts)-1]] = v
	}
	return out
}

// LogResourceURIs are the App Hub URIs (supported-resources) a LogEntry's
// monitored resource can stand for, with the resource's project as ID and
// as number; none for a type that is not a supported resource's. Docs
// re-read 2026-11-21: https://docs.cloud.google.com/logging/docs/api/v2/resource-list.
func LogResourceURIs(resourceType string, labels map[string]string, number string) []string {
	p := labels["project_id"]
	if p == "" {
		return nil
	}
	loc := labels["location"]
	var out []string
	add := func(uri string) { out = append(out, uri) }
	switch resourceType {
	case "cloud_run_revision":
		add("//run.googleapis.com/projects/" + p + "/locations/" + loc + "/services/" + labels["service_name"])
	case "cloud_run_job":
		add("//run.googleapis.com/projects/" + p + "/locations/" + loc + "/jobs/" + labels["job_name"])
	case "cloud_run_worker_pool":
		add("//run.googleapis.com/projects/" + p + "/locations/" + loc + "/workerPools/" + labels["worker_pool_name"])
	case "cloud_function":
		add("//cloudfunctions.googleapis.com/projects/" + p + "/locations/" + labels["region"] + "/functions/" + labels["function_name"])
	case "k8s_cluster", "gke_cluster":
		add("//container.googleapis.com/projects/" + p + "/locations/" + loc + "/clusters/" + labels["cluster_name"])
	case "pubsub_topic":
		add("//pubsub.googleapis.com/projects/" + p + "/topics/" + labels["topic_id"])
	case "pubsub_subscription":
		add("//pubsub.googleapis.com/projects/" + p + "/subscriptions/" + labels["subscription_id"])
	case "gcs_bucket":
		add("//storage.googleapis.com/" + labels["bucket_name"])
	case "cloudsql_database":
		// database_id is PROJECT:INSTANCE.
		if _, inst, ok := strings.Cut(labels["database_id"], ":"); ok {
			add("//cloudsql.googleapis.com/projects/" + p + "/instances/" + inst)
		}
	case "cloud_tasks_queue":
		add("//cloudtasks.googleapis.com/projects/" + number + "/locations/" + loc + "/queues/" + labels["queue_id"])
	case "workflows.googleapis.com/Workflow":
		add("//workflows.googleapis.com/projects/" + p + "/locations/" + loc + "/workflows/" + labels["workflow_id"])
	case "spanner_instance":
		add("//spanner.googleapis.com/projects/" + p + "/instances/" + labels["instance_id"])
	case "redis_instance":
		add("//redis.googleapis.com/projects/" + p + "/locations/" + labels["region"] + "/instances/" + labels["instance_id"])
	}
	return out
}
