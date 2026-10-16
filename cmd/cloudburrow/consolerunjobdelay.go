package main

// Delayed execution on a Cloud Run job's page (#1450): v1's
// spec.template.spec.delayExecution, which v2 (the API the Jobs forms use)
// has no field for. Set delayed execution reads the job through Google's
// generated v1 client at the region's {region}- host, as gcloud does, and
// replaces it with the flag changed, as gcloud beta run jobs update
// --[no-]delay-execution does.

import (
	"context"
	"errors"
	"net/http"

	"google.golang.org/api/option"
	runv1 "google.golang.org/api/run/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	actSetDelay   = "setdelay"
	runDelayField = "delayExecution"
	runDelayHelp  = "Cloud Run may start a delayed execution within the next 12 hours, depending on available capacity. " +
		"Locally capacity is never waited for, so an execution starts at once either way."
)

// regionalTransport sends every request with the region's {region}-
// host, which is how the v1 API is told the region.
type regionalTransport struct{ host string }

func (t regionalTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = t.host
	return http.DefaultTransport.RoundTrip(r)
}

// v1Jobs is the v1 client against the adapter, in the page's region.
func (p runJobsProvider) v1Jobs(ctx context.Context) (*runv1.APIService, error) {
	if p.endpoint() == "" {
		return nil, errors.New("the Cloud Run adapter is not running")
	}
	return runv1.NewService(ctx, option.WithEndpoint("http://"+p.endpoint()+"/"), option.WithoutAuthentication(),
		option.WithHTTPClient(&http.Client{Transport: regionalTransport{p.location() + "-run.localhost"}}))
}

func (p runJobsProvider) v1Job(ctx context.Context, project, job string) (*runv1.APIService, *runv1.Job, error) {
	c, err := p.v1Jobs(ctx)
	if err != nil {
		return nil, nil, err
	}
	j, err := c.Namespaces.Jobs.Get("namespaces/" + p.project(project) + "/jobs/" + lastSegment(job)).Context(ctx).Do()
	return c, j, err
}

// delayAction is a job page's Set delayed execution, its box ticked when
// the job's executions are delayed now; none when the job cannot be read.
func (p runJobsProvider) delayAction(ctx context.Context, project, job string) []console.Action {
	_, j, err := p.v1Job(ctx, project, job)
	if err != nil || j.Spec == nil || j.Spec.Template == nil || j.Spec.Template.Spec == nil {
		return nil
	}
	current := "false"
	if j.Spec.Template.Spec.DelayExecution {
		current = "true"
	}
	return []console.Action{{ID: actSetDelay, Label: "Set delayed execution", Fields: []console.Field{
		{Name: runDelayField, Label: "Delay executions", Type: "checkbox", Default: current, Help: runDelayHelp},
	}}}
}

// setDelay replaces the job with its delayExecution set as the form says.
func (p runJobsProvider) setDelay(ctx context.Context, project, job string, values map[string]string) error {
	c, j, err := p.v1Job(ctx, project, job)
	if err != nil {
		return err
	}
	if j.Spec == nil || j.Spec.Template == nil || j.Spec.Template.Spec == nil {
		return errors.New("the job has no execution template")
	}
	j.Spec.Template.Spec.DelayExecution = values[runDelayField] == "true"
	_, err = c.Namespaces.Jobs.ReplaceJob("namespaces/"+p.project(project)+"/jobs/"+j.Metadata.Name, j).Context(ctx).Do()
	return err
}
