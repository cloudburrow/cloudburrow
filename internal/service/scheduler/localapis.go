package scheduler

import "github.com/cloudburrow/cloudburrow/internal/egress"

// WithLocalAPIs sends an HTTP target on a googleapis.com host to this
// instance's own endpoint of the API it names, local(api) being its
// "http://host:port" by the host's first label ("" when not served). A job
// Google's Workflows page makes for a workflow's Cloud Scheduler trigger
// posts to https://workflowexecutions.googleapis.com/v1/{workflow}/executions;
// here it reaches this instance's Workflow Executions, and a googleapis.com
// API this instance does not serve, or any other Google host, is refused
// rather than sent to Google (internal/egress, #2015).
func (r *Runner) WithLocalAPIs(local func(api string) string) *Runner {
	r.local = local
	return r
}

// localTarget is uri with a googleapis.com host replaced by the local
// endpoint of its API; a Google host it cannot map is refused, and other
// URIs are returned as they are.
func (r *Runner) localTarget(uri string) (string, error) {
	var served egress.LocalAPIs
	if r.local != nil {
		served = r.local
	}
	return egress.ResolveWith(uri, served)
}
