package run

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// delayExecution, v1's ExecutionSpec field (#1450): "If true, the system
// will start the execution within the next 12 hours depending on available
// capacity" (the v1 discovery document). It is on a job's execution
// template (spec.template.spec.delayExecution, gcloud beta run jobs create
// --delay-execution), on a run's overrides, and on each execution's spec.
//
// v2 has the field nowhere, so the v2 Job and Execution protos this adapter
// stores cannot hold it. It is kept beside them instead, as an annotation on
// the Kubernetes object each is stored in: the job's ConfigMap and the
// execution's batch Job. The v2 mapping reads neither annotation, so v2
// clients never see it, as they never see it on Google. A v2 UpdateJob
// rewrites the job's ConfigMap from v2's fields alone, so it clears the
// job's flag, as a v2 client that cannot send the field replaces the job
// without it (Google's behaviour here is UNVERIFIED).
//
// Locally there is never a wait for capacity, so a delayed execution starts
// at once: "within the next 12 hours" holds.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/run/docs/reference/rest/v1/namespaces.jobs
const annV1DelayExecution = "cloudburrow.dev/run-v1-delay-execution"

// setV1Delay sets or clears the delayExecution annotation on one object.
func (s *Server) setV1Delay(ctx context.Context, resource, name string, on bool) error {
	var v any // null removes the annotation in a merge patch
	if on {
		v = "true"
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{annV1DelayExecution: v}}})
	if err != nil {
		return apierror.Internal(err, "encode delayExecution patch")
	}
	return s.kn.patchObject(ctx, resource, name, string(patch))
}

// setV1JobDelay stores a job's own delayExecution on its ConfigMap.
func (s *Server) setV1JobDelay(ctx context.Context, jobName string, on bool) error {
	_, id, _ := strings.Cut(jobName, "/jobs/")
	cm, err := s.loadJob(ctx, jobName, id)
	if err != nil {
		return err
	}
	if (cm.Metadata.Annotations[annV1DelayExecution] == "true") == on {
		return nil
	}
	return s.setV1Delay(ctx, resConfigMap, cm.Metadata.Name, on)
}

// v1JobDelay is a job's own delayExecution.
func (s *Server) v1JobDelay(ctx context.Context, jobName string) (bool, error) {
	_, id, _ := strings.Cut(jobName, "/jobs/")
	cm, err := s.loadJob(ctx, jobName, id)
	if err != nil {
		return false, err
	}
	return cm.Metadata.Annotations[annV1DelayExecution] == "true", nil
}

// v1JobConfigMaps are every job's ConfigMap, by the job's v2 name.
func (s *Server) v1JobConfigMaps(ctx context.Context) (map[string]kconfigMap, error) {
	cms, err := listObjects[kconfigMap](ctx, s.kn, resConfigMap, labelOwned+"=true,"+labelKind+"="+kindRunJob)
	if err != nil {
		return nil, err
	}
	out := make(map[string]kconfigMap, len(cms))
	for _, cm := range cms {
		if n := cm.Metadata.Annotations[annRunName]; n != "" {
			out[n] = cm
		}
	}
	return out, nil
}

// v1ExecutionDelays is every delayed execution's v2 name.
func (s *Server) v1ExecutionDelays(ctx context.Context) (map[string]bool, error) {
	execs, err := s.jobExecutions(ctx, "")
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, j := range execs {
		if j.Metadata.Annotations[annV1DelayExecution] == "true" {
			out[j.Metadata.Annotations[annRunName]] = true
		}
	}
	return out, nil
}

// v1ExecutionDelay is one execution's delayExecution.
func (s *Server) v1ExecutionDelay(ctx context.Context, execName string) (bool, error) {
	_, id, _ := strings.Cut(execName, "/executions/")
	var j kjob
	if err := s.kn.getObject(ctx, &j, resBatchJob, id, "execution "+execName); err != nil {
		return false, err
	}
	return j.Metadata.Annotations[annV1DelayExecution] == "true", nil
}

// v1JobLocations are the regions a project has jobs in, sorted: where a
// region-less v1 jobs call (the global endpoint) looks.
func v1JobLocations(cms map[string]kconfigMap, project string) []string {
	seen := map[string]bool{}
	var out []string
	for name := range cms {
		if projectOf(name) != project {
			continue
		}
		if loc := locationOf(name); loc != "" && !seen[loc] {
			seen[loc] = true
			out = append(out, loc)
		}
	}
	sort.Strings(out)
	return out
}
