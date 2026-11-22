package main

// GKE's kube state metrics packages (#2100): the curated kube_* metrics of
// how-to/kube-state-metrics (re-read 2026-11-22), each a GAUGE DOUBLE on
// prometheus_target, read from the cluster's objects as kube-state-metrics
// reads them. "Kube state metrics does not collect from system
// namespaces."

import (
	"encoding/json"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/gke"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// ksmKinds are the resources each package reads.
var ksmKinds = map[string]string{
	"STORAGE": "persistentvolumes,persistentvolumeclaims", "POD": "pods", "DEPLOYMENT": "deployments.apps",
	"STATEFULSET": "statefulsets.apps", "DAEMONSET": "daemonsets.apps", "HPA": "horizontalpodautoscalers.autoscaling",
	"JOBSET": "jobsets.jobset.x-k8s.io",
}

// ksmObject is what is read of an object.
type ksmObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		UID       string `json:"uid"`
	} `json:"metadata"`
	Spec   json.RawMessage `json:"spec"`
	Status json.RawMessage `json:"status"`
}

// ksmPoints are the package's metrics of the objects in raw (a kubectl
// List), for target t at now.
func ksmPoints(t gke.TelemetryTarget, raw []byte, now time.Time) []monitoring.PromPoint {
	var list struct {
		Items []ksmObject `json:"items"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	var out []monitoring.PromPoint
	g := func(o ksmObject, name string, v float64, ls map[string]string) {
		res := map[string]string{"project_id": t.Project, "location": t.Location, "cluster": t.Cluster,
			"namespace": o.Metadata.Namespace, "job": "kube-state-metrics", "instance": "kube-state-metrics:8080"}
		out = append(out, monitoring.PromPoint{Type: monitoring.PromMetricType(name, "gauge"), Labels: ls, Resource: res, End: now, Value: v})
	}
	b := func(ok bool) float64 {
		if ok {
			return 1
		}
		return 0
	}
	for _, o := range list.Items {
		if gke.SystemNamespaces[o.Metadata.Namespace] || o.Metadata.Namespace == "gmp-system" || o.Metadata.Namespace == "gmp-public" {
			continue
		}
		name := o.Metadata.Name
		switch o.Kind {
		case "Pod":
			var st struct {
				Phase      string `json:"phase"`
				Conditions []struct {
					Type, Status, Reason string
				} `json:"conditions"`
				ContainerStatuses []struct {
					Name  string `json:"name"`
					Ready bool   `json:"ready"`
					State struct {
						Waiting *struct {
							Reason string `json:"reason"`
						} `json:"waiting"`
					} `json:"state"`
				} `json:"containerStatuses"`
			}
			_ = json.Unmarshal(o.Status, &st)
			pod := map[string]string{"pod": name, "uid": o.Metadata.UID}
			for _, ph := range []string{"Pending", "Succeeded", "Failed", "Unknown", "Running"} {
				g(o, "kube_pod_status_phase", b(st.Phase == ph), map[string]string{"pod": name, "uid": o.Metadata.UID, "phase": ph})
			}
			unsched := false
			for _, c := range st.Conditions {
				if c.Type == "PodScheduled" && c.Status == "False" && c.Reason == "Unschedulable" {
					unsched = true
				}
			}
			if unsched {
				g(o, "kube_pod_status_unschedulable", 1, pod)
			}
			for _, c := range st.ContainerStatuses {
				g(o, "kube_pod_container_status_ready", b(c.Ready), map[string]string{"pod": name, "uid": o.Metadata.UID, "container": c.Name})
				if w := c.State.Waiting; w != nil && w.Reason != "" {
					g(o, "kube_pod_container_status_waiting_reason", 1,
						map[string]string{"pod": name, "uid": o.Metadata.UID, "container": c.Name, "reason": w.Reason})
				}
			}
		case "Deployment":
			var sp struct {
				Replicas *int `json:"replicas"`
			}
			var st struct {
				AvailableReplicas int `json:"availableReplicas"`
				UpdatedReplicas   int `json:"updatedReplicas"`
			}
			_ = json.Unmarshal(o.Spec, &sp)
			_ = json.Unmarshal(o.Status, &st)
			l := map[string]string{"deployment": name}
			r := 1
			if sp.Replicas != nil {
				r = *sp.Replicas
			}
			g(o, "kube_deployment_spec_replicas", float64(r), l)
			g(o, "kube_deployment_status_replicas_available", float64(st.AvailableReplicas), l)
			g(o, "kube_deployment_status_replicas_updated", float64(st.UpdatedReplicas), l)
		case "StatefulSet":
			var st struct {
				Replicas        int `json:"replicas"`
				ReadyReplicas   int `json:"readyReplicas"`
				UpdatedReplicas int `json:"updatedReplicas"`
			}
			_ = json.Unmarshal(o.Status, &st)
			l := map[string]string{"statefulset": name}
			g(o, "kube_statefulset_replicas", float64(st.Replicas), l)
			g(o, "kube_statefulset_status_replicas_ready", float64(st.ReadyReplicas), l)
			g(o, "kube_statefulset_status_replicas_updated", float64(st.UpdatedReplicas), l)
		case "DaemonSet":
			var st struct {
				Desired    int `json:"desiredNumberScheduled"`
				Misplaced  int `json:"numberMisscheduled"`
				Ready      int `json:"numberReady"`
				UpdatedNum int `json:"updatedNumberScheduled"`
			}
			_ = json.Unmarshal(o.Status, &st)
			l := map[string]string{"daemonset": name}
			g(o, "kube_daemonset_status_desired_number_scheduled", float64(st.Desired), l)
			g(o, "kube_daemonset_status_number_misscheduled", float64(st.Misplaced), l)
			g(o, "kube_daemonset_status_number_ready", float64(st.Ready), l)
			g(o, "kube_daemonset_status_updated_number_scheduled", float64(st.UpdatedNum), l)
		case "HorizontalPodAutoscaler":
			var sp struct {
				MinReplicas *int `json:"minReplicas"`
				MaxReplicas int  `json:"maxReplicas"`
				Metrics     []struct {
					Type     string `json:"type"`
					Resource *struct {
						Name   string `json:"name"`
						Target struct {
							Type               string `json:"type"`
							AverageUtilization *int   `json:"averageUtilization"`
							AverageValue       string `json:"averageValue"`
							Value              string `json:"value"`
						} `json:"target"`
					} `json:"resource"`
				} `json:"metrics"`
			}
			var st struct {
				CurrentReplicas int `json:"currentReplicas"`
				DesiredReplicas int `json:"desiredReplicas"`
				Conditions      []struct {
					Type, Status string
				} `json:"conditions"`
			}
			_ = json.Unmarshal(o.Spec, &sp)
			_ = json.Unmarshal(o.Status, &st)
			l := map[string]string{"horizontalpodautoscaler": name}
			minR := 1
			if sp.MinReplicas != nil {
				minR = *sp.MinReplicas
			}
			g(o, "kube_horizontalpodautoscaler_spec_max_replicas", float64(sp.MaxReplicas), l)
			g(o, "kube_horizontalpodautoscaler_spec_min_replicas", float64(minR), l)
			g(o, "kube_horizontalpodautoscaler_status_current_replicas", float64(st.CurrentReplicas), l)
			g(o, "kube_horizontalpodautoscaler_status_desired_replicas", float64(st.DesiredReplicas), l)
			for _, m := range sp.Metrics {
				if m.Resource == nil {
					continue
				}
				tg := m.Resource.Target
				switch {
				case tg.AverageUtilization != nil:
					g(o, "kube_horizontalpodautoscaler_spec_target_metric", float64(*tg.AverageUtilization),
						map[string]string{"horizontalpodautoscaler": name, "metric_name": m.Resource.Name, "metric_target_type": "utilization"})
				case tg.AverageValue != "":
					if v := parseQuantity(tg.AverageValue); v >= 0 {
						g(o, "kube_horizontalpodautoscaler_spec_target_metric", float64(v),
							map[string]string{"horizontalpodautoscaler": name, "metric_name": m.Resource.Name, "metric_target_type": "average"})
					}
				}
			}
			for _, c := range st.Conditions {
				for _, s := range []string{"true", "false", "unknown"} {
					g(o, "kube_horizontalpodautoscaler_status_condition", b(c.Status == map[string]string{"true": "True", "false": "False", "unknown": "Unknown"}[s]),
						map[string]string{"horizontalpodautoscaler": name, "condition": c.Type, "status": s})
				}
			}
		case "PersistentVolume":
			var sp struct {
				Capacity struct {
					Storage string `json:"storage"`
				} `json:"capacity"`
				StorageClassName string `json:"storageClassName"`
				ClaimRef         *struct {
					Name string `json:"name"`
				} `json:"claimRef"`
				CSI *struct {
					Driver       string `json:"driver"`
					VolumeHandle string `json:"volumeHandle"`
				} `json:"csi"`
				Local *struct {
					Path   string `json:"path"`
					FSType string `json:"fsType"`
				} `json:"local"`
				HostPath *struct {
					Path string `json:"path"`
				} `json:"hostPath"`
			}
			var st struct {
				Phase string `json:"phase"`
			}
			_ = json.Unmarshal(o.Spec, &sp)
			_ = json.Unmarshal(o.Status, &st)
			l := map[string]string{"persistentvolume": name}
			if v := parseQuantity(sp.Capacity.Storage); v >= 0 {
				g(o, "kube_persistentvolume_capacity_bytes", float64(v), l)
			}
			if sp.ClaimRef != nil {
				g(o, "kube_persistentvolume_claim_ref", 1, map[string]string{"persistentvolume": name, "claim_name": sp.ClaimRef.Name, "name": sp.ClaimRef.Name})
			}
			info := map[string]string{"persistentvolume": name, "storageclass": sp.StorageClassName, "csi_driver": "", "csi_volume_handle": "", "local_path": "", "local_fs": ""}
			if sp.CSI != nil {
				info["csi_driver"], info["csi_volume_handle"] = sp.CSI.Driver, sp.CSI.VolumeHandle
			}
			if sp.Local != nil {
				info["local_path"], info["local_fs"] = sp.Local.Path, sp.Local.FSType
			}
			g(o, "kube_persistentvolume_info", 1, info)
			for _, ph := range []string{"Pending", "Available", "Bound", "Released", "Failed"} {
				g(o, "kube_persistentvolume_status_phase", b(st.Phase == ph), map[string]string{"persistentvolume": name, "phase": ph})
			}
		case "PersistentVolumeClaim":
			var sp struct {
				StorageClassName *string `json:"storageClassName"`
				VolumeName       string  `json:"volumeName"`
				Resources        struct {
					Requests struct {
						Storage string `json:"storage"`
					} `json:"requests"`
				} `json:"resources"`
			}
			var st struct {
				Phase string `json:"phase"`
			}
			_ = json.Unmarshal(o.Spec, &sp)
			_ = json.Unmarshal(o.Status, &st)
			sc := ""
			if sp.StorageClassName != nil {
				sc = *sp.StorageClassName
			}
			g(o, "kube_persistentvolumeclaim_info", 1, map[string]string{"persistentvolumeclaim": name, "storageclass": sc, "volumename": sp.VolumeName})
			if v := parseQuantity(sp.Resources.Requests.Storage); v >= 0 {
				g(o, "kube_persistentvolumeclaim_resource_requests_storage_bytes", float64(v), map[string]string{"persistentvolumeclaim": name})
			}
			for _, ph := range []string{"Lost", "Bound", "Pending"} {
				g(o, "kube_persistentvolumeclaim_status_phase", b(st.Phase == ph), map[string]string{"persistentvolumeclaim": name, "phase": ph})
			}
		case "JobSet":
			var sp struct {
				ReplicatedJobs []struct {
					Name     string `json:"name"`
					Replicas *int   `json:"replicas"`
				} `json:"replicatedJobs"`
			}
			var st struct {
				Restarts           int `json:"restarts"`
				ReplicatedJobsStat []struct {
					Name      string `json:"name"`
					Ready     int    `json:"ready"`
					Succeeded int    `json:"succeeded"`
					Failed    int    `json:"failed"`
					Active    int    `json:"active"`
					Suspended int    `json:"suspended"`
				} `json:"replicatedJobsStatus"`
				Conditions []struct {
					Type, Status string
				} `json:"conditions"`
			}
			_ = json.Unmarshal(o.Spec, &sp)
			_ = json.Unmarshal(o.Status, &st)
			for _, rj := range sp.ReplicatedJobs {
				r := 1
				if rj.Replicas != nil {
					r = *rj.Replicas
				}
				g(o, "kube_jobset_specified_replicas", float64(r), map[string]string{"jobset_name": name, "replicated_job_name": rj.Name})
			}
			for _, rs := range st.ReplicatedJobsStat {
				l := func() map[string]string {
					return map[string]string{"jobset_name": name, "replicated_job_name": rs.Name}
				}
				g(o, "kube_jobset_ready_replicas", float64(rs.Ready), l())
				g(o, "kube_jobset_succeeded_replicas", float64(rs.Succeeded), l())
				g(o, "kube_jobset_failed_replicas", float64(rs.Failed), l())
				g(o, "kube_jobset_active_replicas", float64(rs.Active), l())
				g(o, "kube_jobset_suspended_replicas", float64(rs.Suspended), l())
			}
			for _, c := range st.Conditions {
				g(o, "kube_jobset_status_condition", b(c.Status == "True"), map[string]string{"jobset_name": name, "condition": c.Type})
			}
			g(o, "kube_jobset_restarts", float64(st.Restarts), map[string]string{"jobset_name": name})
		}
	}
	return out
}

// ksmResources are the resources the packages on read, comma-joined for
// one kubectl get; JobSet's apart, its resource being a CRD a cluster may
// lack.
func ksmResources(on map[string]bool) (core, jobset string) {
	for _, p := range []string{"STORAGE", "POD", "DEPLOYMENT", "STATEFULSET", "DAEMONSET", "HPA"} {
		if on[p] {
			if core != "" {
				core += ","
			}
			core += ksmKinds[p]
		}
	}
	if on["JOBSET"] {
		jobset = ksmKinds["JOBSET"]
	}
	return core, jobset
}
