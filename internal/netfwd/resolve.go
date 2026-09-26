package netfwd

import (
	"context"
	"encoding/json"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// A Service's port-forward binds one pod, chosen by kubectl among the pods
// its selector matches, and kubectl does not skip a pod that is being
// deleted. During a rollout, such as `up --mode ephemeral` replacing a
// persistent backend's pod, the old pod stays Ready while it terminates, and
// a tunnel bound to it accepts connections that then hang until it dies
// (#381). So the forwarder picks the pod itself: the newest one that is
// Ready and not being deleted.

type svcJSON struct {
	Spec struct {
		Selector map[string]string `json:"selector"`
		Ports    []struct {
			Port       int             `json:"port"`
			TargetPort json.RawMessage `json:"targetPort"`
		} `json:"ports"`
	} `json:"spec"`
}

type podListJSON struct {
	Items []struct {
		Metadata struct {
			Name              string     `json:"name"`
			CreationTimestamp time.Time  `json:"creationTimestamp"`
			DeletionTimestamp *time.Time `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Ports []struct {
					Name          string `json:"name"`
					ContainerPort int    `json:"containerPort"`
				} `json:"ports"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// selector renders a Service's selector for `kubectl get -l`, sorted so the
// call is deterministic. It is empty when the Service selects nothing.
func selector(svc []byte) (string, error) {
	var s svcJSON
	if err := json.Unmarshal(svc, &s); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(s.Spec.Selector))
	for k, v := range s.Spec.Selector {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ","), nil
}

// choosePod returns the pod to forward to and the container port behind
// servicePort: the newest pod that is Ready and has no deletion timestamp.
// ok is false when no such pod exists or the port cannot be resolved, and
// the caller then forwards to the Service as before.
func choosePod(svc, pods []byte, servicePort int) (name string, port int, ok bool) {
	var s svcJSON
	var p podListJSON
	if json.Unmarshal(svc, &s) != nil || json.Unmarshal(pods, &p) != nil {
		return "", 0, false
	}
	var target json.RawMessage
	for _, sp := range s.Spec.Ports {
		if sp.Port == servicePort {
			target = sp.TargetPort
		}
	}
	items := p.Items
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Metadata.CreationTimestamp.After(items[j].Metadata.CreationTimestamp)
	})
	for _, pod := range items {
		if pod.Metadata.DeletionTimestamp != nil {
			continue
		}
		ready := false
		for _, c := range pod.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready = true
			}
		}
		if !ready {
			continue
		}
		// targetPort is a number, a named container port, or absent, in
		// which case it equals the Service port.
		var n int
		var named string
		switch {
		case len(target) == 0:
			n = servicePort
		case json.Unmarshal(target, &n) == nil:
		case json.Unmarshal(target, &named) == nil:
			for _, c := range pod.Spec.Containers {
				for _, cp := range c.Ports {
					if cp.Name == named {
						n = cp.ContainerPort
					}
				}
			}
		}
		if n == 0 {
			return "", 0, false
		}
		return pod.Metadata.Name, n, true
	}
	return "", 0, false
}

// forwardTarget returns the kubectl port-forward resource and remote port:
// a chosen pod when one can be found, with its name, else the Service and
// no name.
func (f *Forwarder) forwardTarget(ctx context.Context) (resource string, port int, pod string) {
	fallback := "svc/" + f.target.Name
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	kubectl := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", f.kubeconfig, "-n", f.target.Namespace}, args...)...).Output()
	}
	svc, err := kubectl("get", "svc", f.target.Name, "-o", "json")
	if err != nil {
		return fallback, f.target.ServicePort, ""
	}
	sel, err := selector(svc)
	if err != nil || sel == "" {
		return fallback, f.target.ServicePort, ""
	}
	pods, err := kubectl("get", "pods", "-l", sel, "-o", "json")
	if err != nil {
		return fallback, f.target.ServicePort, ""
	}
	name, p, ok := choosePod(svc, pods, f.target.ServicePort)
	if !ok {
		return fallback, f.target.ServicePort, ""
	}
	return "pod/" + name, p, name
}
