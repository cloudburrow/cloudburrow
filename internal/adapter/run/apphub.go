package run

import "context"

// Names are the full names of every Cloud Run service and job, in every
// project and location, for App Hub's discovery (#2110), which
// supported-resources lists both of: one listing of each, where
// ListServices and ListJobs answer one parent.
func (s *Server) Names(ctx context.Context) (services, jobs []string, err error) {
	items, err := s.kn.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, k := range items {
		if n := k.Metadata.Annotations["cloudburrow.dev/cloud-run-name"]; n != "" {
			services = append(services, n)
		}
	}
	cms, err := listObjects[kconfigMap](ctx, s.kn, resConfigMap, labelOwned+"=true,"+labelKind+"="+kindRunJob)
	if err != nil {
		return nil, nil, err
	}
	for _, cm := range cms {
		if n := cm.Metadata.Annotations[annRunName]; n != "" {
			jobs = append(jobs, n)
		}
	}
	return services, jobs, nil
}
