package scheduler

import (
	"fmt"
	"net/url"
	"strings"
)

// WithLocalAPIs sends an HTTP target on a googleapis.com host to this
// instance's own endpoint of the API it names, local(api) being its
// "http://host:port" by the host's first label ("" when not served). A job
// Google's Workflows page makes for a workflow's Cloud Scheduler trigger
// posts to https://workflowexecutions.googleapis.com/v1/{workflow}/executions;
// here it reaches this instance's Workflow Executions, and a googleapis.com
// API this instance does not serve is refused rather than sent to Google.
func (r *Runner) WithLocalAPIs(local func(api string) string) *Runner {
	r.local = local
	return r
}

// localTarget is uri with a googleapis.com host replaced by the local
// endpoint of its API; other URIs are returned as they are.
func (r *Runner) localTarget(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return uri, nil
	}
	api, ok := strings.CutSuffix(strings.ToLower(u.Hostname()), ".googleapis.com")
	if !ok {
		return uri, nil
	}
	// A regional host such as us-central1-run.googleapis.com names run.
	if i := strings.LastIndex(api, "-"); i >= 0 && r.local != nil && r.local(api) == "" && r.local(api[i+1:]) != "" {
		api = api[i+1:]
	}
	base := ""
	if r.local != nil {
		base = r.local(api)
	}
	if base == "" {
		return "", fmt.Errorf("refused: %s.googleapis.com is not served by this CloudBurrow instance, which sends no request to Google", api)
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("bad local endpoint for %s: %v", api, err)
	}
	u.Scheme, u.Host = b.Scheme, b.Host
	return u.String(), nil
}
