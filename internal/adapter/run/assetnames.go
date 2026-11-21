package run

import (
	"context"
	"sort"
	"strings"
)

// Cloud Asset's IAM policy analysis (#2112) enumerates a project's
// resources that keep allow policies, in every location: Cloud Run's
// services and jobs, Cloud Run functions and Artifact Registry
// repositories, named as their APIs name them (and as Cloud Asset
// Inventory's run.googleapis.com/Service and Job,
// cloudfunctions.googleapis.com/Function and
// artifactregistry.googleapis.com/Repository assets are). Worker pools
// keep policies too, but run.googleapis.com/WorkerPool is "Not available
// in the analysis APIs" (supported-asset-types, read 2026-11-21), so they
// are not listed.

// ProjectResourceNames are the project's services and jobs, in every
// location, by name.
func (s *Server) ProjectResourceNames(ctx context.Context, project string) []string {
	var out []string
	if svcs, _, err := s.v1Services(ctx, v1Scope{project: project}); err == nil {
		for _, svc := range svcs {
			out = append(out, svc.GetName())
		}
	}
	prefix := "projects/" + project + "/locations/"
	if cms, err := listObjects[kconfigMap](ctx, s.kn, resConfigMap, labelOwned+"=true,"+labelKind+"="+kindRunJob); err == nil {
		for _, cm := range cms {
			if n := cm.Metadata.Annotations[annRunName]; strings.HasPrefix(n, prefix) && strings.Contains(n, "/jobs/") {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// RepositoryNames are the project's Artifact Registry repositories, in
// every location, by name.
func RepositoryNames(project string) []string {
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	var out []string
	for name := range repositories.repos {
		if strings.HasPrefix(name, "projects/"+project+"/locations/") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
