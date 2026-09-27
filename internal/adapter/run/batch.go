package run

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The Kubernetes objects behind Cloud Run Jobs (#582), read and written
// through the same kubectl the Knative Services are.
//
// A Cloud Run job is configuration, and is kept as a ConfigMap the adapter
// owns; each execution is a batch/v1 Job, which the kind cluster runs to
// completion (docs/compatibility.md, Kubernetes: Jobs).

// kmeta is the object metadata this reads.
type kmeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Generation        int64             `json:"generation,omitempty"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
}

// kconfigMap is a ConfigMap holding one Cloud Run job.
type kconfigMap struct {
	Metadata kmeta             `json:"metadata"`
	Data     map[string]string `json:"data"`
}

// kjob is the subset of a batch/v1 Job an execution is read from.
type kjob struct {
	Metadata kmeta `json:"metadata"`
	Spec     struct {
		Completions *int32 `json:"completions"`
		Parallelism *int32 `json:"parallelism"`
		Suspend     bool   `json:"suspend"`
	} `json:"spec"`
	Status struct {
		StartTime      *time.Time `json:"startTime"`
		CompletionTime *time.Time `json:"completionTime"`
		Active         int32      `json:"active"`
		Succeeded      int32      `json:"succeeded"`
		// Failed counts failed pods, retries included.
		Failed int32 `json:"failed"`
		// Terminating is pods being deleted, as after a suspend.
		Terminating *int32 `json:"terminating"`
		// FailedIndexes is the tasks that exhausted their retries, as
		// "1,3-5", with backoffLimitPerIndex.
		FailedIndexes *string         `json:"failedIndexes"`
		Conditions    []ksvcCondition `json:"conditions"`
	} `json:"status"`
}

// kpod is the subset of a pod a failed task's message is read from.
type kpod struct {
	Metadata kmeta `json:"metadata"`
	Status   struct {
		Phase             string `json:"phase"`
		Reason            string `json:"reason"`
		Message           string `json:"message"`
		ContainerStatuses []struct {
			Name  string `json:"name"`
			State struct {
				Terminated *struct {
					ExitCode   int32     `json:"exitCode"`
					Reason     string    `json:"reason"`
					Message    string    `json:"message"`
					FinishedAt time.Time `json:"finishedAt"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// Kubernetes resource names, fully qualified so no custom resource called
// "jobs" can be read in place of batch/v1's.
const (
	resConfigMap = "configmaps"
	resBatchJob  = "jobs.batch"
	resPod       = "pods"
)

// isNotFound reports whether kubectl said the object does not exist.
func isNotFound(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "NotFound") || strings.Contains(msg, "not found")
}

// getObject reads one object into v. A missing object is NOT_FOUND, named by
// what, and anything else is Internal.
func (k *Knative) getObject(ctx context.Context, v any, resource, name, what string) error {
	out, err := k.kubectl(ctx, "", "get", resource, name, "-o", "json")
	if err != nil {
		if isNotFound(err) {
			return apierror.NotFound("%s not found", what)
		}
		return apierror.Internal(err, "read %s", what)
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		return apierror.Internal(err, "decode %s", what)
	}
	return nil
}

// listObjects reads every object of a resource matching a label selector.
func listObjects[T any](ctx context.Context, k *Knative, resource, selector string) ([]T, error) {
	out, err := k.kubectl(ctx, "", "get", resource, "-l", selector, "-o", "json")
	if err != nil {
		return nil, apierror.Internal(err, "list %s", resource)
	}
	var list struct {
		Items []T `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, apierror.Internal(err, "decode %s list", resource)
	}
	return list.Items, nil
}

// writeObject creates (verb "create") or replaces (verb "replace") an
// object. A duplicate create is ALREADY_EXISTS, a replace that lost a race
// is ABORTED, and a manifest the cluster refused is INVALID_ARGUMENT with the
// cluster's own message.
func (k *Knative) writeObject(ctx context.Context, verb, manifest, what string) error {
	out, err := k.kubectl(ctx, manifest, verb, "-f", "-")
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(err.Error())
	if detail == "" {
		detail = strings.TrimSpace(out)
	}
	switch {
	case strings.Contains(detail, "AlreadyExists") || strings.Contains(detail, "already exists"):
		return apierror.AlreadyExists("%s already exists", what)
	case strings.Contains(detail, "the object has been modified") || strings.Contains(detail, "Conflict"):
		return apierror.Aborted("%s was changed concurrently; read it again and retry", what)
	case isRejection(detail):
		return apierror.InvalidArgument("the cluster rejected %s: %s", what, detail)
	}
	return apierror.Internal(err, "%s %s: %s", verb, what, detail)
}

// deleteObject removes one object; one already gone is not an error.
func (k *Knative) deleteObject(ctx context.Context, resource, name string) error {
	if _, err := k.kubectl(ctx, "", "delete", resource, name, "--ignore-not-found"); err != nil {
		return apierror.Internal(err, "delete %s %s", resource, name)
	}
	return nil
}

// deleteSelected removes every object of a resource matching a selector.
// kubectl's default cascade is background, so a Job's pods go with it.
func (k *Knative) deleteSelected(ctx context.Context, resource, selector string) error {
	if _, err := k.kubectl(ctx, "", "delete", resource, "-l", selector, "--ignore-not-found"); err != nil {
		return apierror.Internal(err, "delete %s -l %s", resource, selector)
	}
	return nil
}

// patchObject applies a JSON merge patch.
func (k *Knative) patchObject(ctx context.Context, resource, name, patch string) error {
	if _, err := k.kubectl(ctx, "", "patch", resource, name, "--type", "merge", "-p", patch); err != nil {
		if isNotFound(err) {
			return apierror.NotFound("%s %s not found", resource, name)
		}
		return apierror.Internal(err, "patch %s %s", resource, name)
	}
	return nil
}
