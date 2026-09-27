// Package run adapts the Cloud Run v2 API onto Knative Serving.
//
// The adapter is ours; the execution engine is not (ADR-0005). Knative is the
// closest available model for Cloud Run, not an equivalent one, so this
// package translates the subset that maps cleanly and reports the rest as
// unsupported rather than accepting configuration it would silently ignore.
package run

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// Knative applies and reads Knative Services in a cluster, through the
// internal/k8s runner. The runner's namespace is where the adapter's objects
// live; tests give it a fake Invoker, so the mapping logic runs without a
// cluster.
type Knative struct {
	Kube *k8s.Runner
}

// ksvc is the subset of a Knative Service we read back.
type ksvc struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		Annotations       map[string]string `json:"annotations"`
		Labels            map[string]string `json:"labels"`
		Generation        int64             `json:"generation"`
		UID               string            `json:"uid"`
		ResourceVersion   string            `json:"resourceVersion"`
		CreationTimestamp time.Time         `json:"creationTimestamp"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				ContainerConcurrency int   `json:"containerConcurrency"`
				TimeoutSeconds       int64 `json:"timeoutSeconds"`
				Containers           []struct {
					Image         string  `json:"image"`
					WorkingDir    string  `json:"workingDir"`
					StartupProbe  *kprobe `json:"startupProbe"`
					LivenessProbe *kprobe `json:"livenessProbe"`
					Env           []struct {
						Name      string    `json:"name"`
						Value     string    `json:"value"`
						ValueFrom *struct{} `json:"valueFrom"`
					} `json:"env"`
					Ports []struct {
						ContainerPort int `json:"containerPort"`
					} `json:"ports"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		// ObservedGeneration is the spec generation Knative has reconciled.
		// Until it reaches metadata.generation, the conditions describe the
		// previous spec, so Ready=True can be the old revision's.
		ObservedGeneration        int64           `json:"observedGeneration"`
		URL                       string          `json:"url"`
		Conditions                []ksvcCondition `json:"conditions"`
		LatestReadyRevisionName   string          `json:"latestReadyRevisionName"`
		LatestCreatedRevisionName string          `json:"latestCreatedRevisionName"`
		// Traffic is where the Service routes requests: which revisions
		// serve, and what share each takes.
		Traffic []struct {
			RevisionName   string `json:"revisionName"`
			Percent        int    `json:"percent"`
			LatestRevision bool   `json:"latestRevision"`
		} `json:"traffic"`
	} `json:"status"`
}

// ksvcCondition is one Knative status condition. Named rather than inline so
// a test can construct one without restating the whole anonymous type.
type ksvcCondition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	Message            string    `json:"message"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// lastTransition is the most recent condition change, or since when there
// is none: Knative keeps no update time, and a condition changes whenever a
// new spec is reconciled.
func (k ksvc) lastTransition(since time.Time) time.Time {
	latest := since
	for _, c := range k.Status.Conditions {
		if c.LastTransitionTime.After(latest) {
			latest = c.LastTransitionTime
		}
	}
	return latest
}

// Ready reports whether the Knative Service is serving, and why if not.
//
// Knative reports readiness through conditions rather than a single field, so
// a caller that only looked at status.url would treat a failed revision as
// merely slow.
//
// When a revision fails, the top-level Ready condition says only that the
// Configuration "does not have any ready Revision", while ConfigurationsReady
// carries the revision name and the container's own output. The more specific
// message is preferred: "Container failed with: ..." tells a developer what to
// fix, and "does not have any ready Revision" does not.
func (k ksvc) Ready() (bool, string) {
	var readyMsg string
	failed := false
	for _, c := range k.Status.Conditions {
		if c.Type != "Ready" {
			continue
		}
		switch c.Status {
		case "True":
			return true, ""
		case "False":
			failed = true
			readyMsg = c.Message
			if readyMsg == "" {
				readyMsg = c.Reason
			}
		default:
			return false, "" // Unknown: still reconciling
		}
	}
	if !failed {
		return false, ""
	}
	for _, c := range k.Status.Conditions {
		if c.Type == "ConfigurationsReady" && c.Status == "False" && c.Message != "" {
			return false, c.Message
		}
	}
	return false, readyMsg
}

// latestCreatedFailure is the reason the newest revision failed, when it has.
//
// After an update the Service stays Ready on the previous revision while the
// new one fails, so Ready alone never reports the failure; the
// ConfigurationsReady condition does, naming the new revision.
func (k ksvc) latestCreatedFailure() string {
	for _, c := range k.Status.Conditions {
		if c.Type == "ConfigurationsReady" && c.Status == "False" {
			if c.Message != "" {
				return c.Message
			}
			return c.Reason
		}
	}
	return ""
}

// Apply creates or updates a Knative Service from a manifest.
func (k *Knative) Apply(ctx context.Context, manifest string) error {
	err := k.Kube.Apply(ctx, manifest, k8s.ApplyOptions{})
	if err == nil {
		return nil
	}
	// The cluster's own message is what says why. Reporting only "apply
	// failed" leaves a caller with nothing to act on, and reporting it as
	// Internal when the cluster rejected the request says the fault is ours
	// when it is theirs.
	detail := strings.TrimSpace(err.Error())
	if isRejection(detail) {
		return apierror.InvalidArgument("the cluster rejected the service: %s", detail)
	}
	return apierror.Internal(err, "apply Knative Service: %s", detail)
}

// isRejection reports whether the cluster refused the request because of what
// it contained, rather than failing to process it.
func isRejection(message string) bool {
	for _, marker := range []string{
		"is invalid", "admission webhook", "validation failed",
		"must consist of", "Invalid value", "field is immutable",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// Get reads a Knative Service.
func (k *Knative) Get(ctx context.Context, name string) (ksvc, error) {
	out, err := k.Kube.Get(ctx, "ksvc", name, "json")
	if err != nil {
		return ksvc{}, apierror.NotFound("service %s not found", name)
	}
	var s ksvc
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		return ksvc{}, apierror.Internal(err, "decode Knative Service")
	}
	return s, nil
}

// List reads every Knative Service CloudBurrow manages in the namespace.
func (k *Knative) List(ctx context.Context) ([]ksvc, error) {
	out, err := k.Kube.List(ctx, "ksvc", k8s.OwnedSelector, "json")
	if err != nil {
		return nil, apierror.Internal(err, "list Knative Services")
	}
	var list struct {
		Items []ksvc `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, apierror.Internal(err, "decode Knative Service list")
	}
	return list.Items, nil
}

// Delete removes a Knative Service.
//
// It deletes only Services carrying our ownership label, so a Knative Service
// a developer created by hand in the same namespace is never removed.
func (k *Knative) Delete(ctx context.Context, name string) error {
	s, err := k.Get(ctx, name)
	if err != nil {
		return err
	}
	if s.Metadata.Labels[k8s.OwnedLabel] != k8s.OwnedValue {
		return apierror.FailedPrecondition(
			"service %s was not created by CloudBurrow and will not be deleted", name)
	}
	if err := k.Kube.Delete(ctx, "ksvc", name, false); err != nil {
		return apierror.Internal(err, "delete Knative Service")
	}
	return nil
}
