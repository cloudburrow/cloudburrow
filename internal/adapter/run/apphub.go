package run

import (
	"context"
	"net/http"
)

// Names are the full names of every Cloud Run service, job and worker
// pool, in every project and location, for App Hub's discovery (#2110),
// which supported-resources lists all three of (services as services, jobs
// and worker pools as workloads): one listing of each, where ListServices,
// ListJobs and ListWorkerPools answer one parent.
func (s *Server) Names(ctx context.Context) (services, jobs, pools []string, err error) {
	items, err := s.kn.List(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, k := range items {
		if n := k.Metadata.Annotations["cloudburrow.dev/cloud-run-name"]; n != "" {
			services = append(services, n)
		}
	}
	cms, err := listObjects[kconfigMap](ctx, s.kn, resConfigMap, labelOwned+"=true,"+labelKind+"="+kindRunJob)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, cm := range cms {
		if n := cm.Metadata.Annotations[annRunName]; n != "" {
			jobs = append(jobs, n)
		}
	}
	wps, err := s.specsOfKind(ctx, kindWorkerPool)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, cm := range wps {
		if n := cm.Metadata.Annotations[annRunName]; n != "" {
			pools = append(pools, n)
		}
	}
	return services, jobs, pools, nil
}

// GKEClient is an HTTP client that reaches a Kubernetes Engine cluster's
// API server as its client certificate, for App Hub's discovery of the
// cluster's workloads and services (#2110).
func GKEClient(a GKEAccess) (*http.Client, error) { return gkeClient(a) }
