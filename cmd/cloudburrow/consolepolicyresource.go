package main

// The full resource name of each Permissions tab's policy whose service
// stores IAM Conditions (#1756): the condition editor's Run Linter gives it
// to the IAM API's LintPolicy as full_resource_name, the way Google's
// console lints the condition of the resource it is editing. Each is the
// service's API host and the resource name the tab's GetIamPolicy takes
// (docs.cloud.google.com/iam/docs/full-resource-names).

import (
	"context"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
)

// fullResourceName is //host/name, or "" when name is.
func fullResourceName(host, name string) string {
	if name == "" {
		return ""
	}
	return "//" + host + "/" + name
}

func (projectIAMProvider) PolicyResource(_ context.Context, _ string, path []string) string {
	if len(path) != 1 {
		return ""
	}
	return fullResourceName("cloudresourcemanager.googleapis.com", "projects/"+path[0])
}

func (p hierarchyProvider) PolicyResource(_ context.Context, _ string, path []string) string {
	_, name, err := p.iamTarget(path)
	if err != nil {
		return ""
	}
	return fullResourceName("cloudresourcemanager.googleapis.com", name)
}

func (secretsProvider) PolicyResource(_ context.Context, project string, path []string) string {
	if len(path) != 1 || project == "" {
		return ""
	}
	return fullResourceName("secretmanager.googleapis.com", secrets.SecretName(secretAt(project, path[0])))
}

func (kmsProvider) PolicyResource(_ context.Context, project string, path []string) string {
	if len(path) != 1 && len(path) != 2 {
		return ""
	}
	ring, err := kmsRing(project, path)
	if err != nil {
		return ""
	}
	if len(path) == 2 {
		ring += "/cryptoKeys/" + path[1]
	}
	return fullResourceName("cloudkms.googleapis.com", ring)
}

func (kmsEkmProvider) PolicyResource(_ context.Context, project string, path []string) string {
	if len(path) != 1 || kmsOwned(project, path[0]) != nil {
		return ""
	}
	return fullResourceName("cloudkms.googleapis.com", path[0])
}

func (p runProvider) PolicyResource(_ context.Context, project string, path []string) string {
	if len(path) != 1 {
		return ""
	}
	if project == "" {
		project = p.defaultProject
	}
	return fullResourceName("run.googleapis.com", "projects/"+project+"/locations/"+p.location()+"/services/"+path[0])
}

func (p runJobsProvider) PolicyResource(_ context.Context, project string, path []string) string {
	if project = p.project(project); len(path) != 1 || project == "" {
		return ""
	}
	return fullResourceName("run.googleapis.com", p.jobName(project, path[0]))
}

func (p runWorkerPoolsProvider) PolicyResource(_ context.Context, project string, path []string) string {
	if project = p.jobs().project(project); len(path) != 1 || project == "" {
		return ""
	}
	return fullResourceName("run.googleapis.com", p.poolName(project, path[0]))
}

func (p runInstancesProvider) PolicyResource(_ context.Context, project string, path []string) string {
	if project = p.jobs().project(project); len(path) != 1 || project == "" {
		return ""
	}
	return fullResourceName("run.googleapis.com", p.instanceName(project, path[0]))
}

func (loggingBucketsProvider) PolicyResource(_ context.Context, project string, path []string) string {
	if len(path) != 2 || !strings.Contains(path[0], "/buckets/") {
		return ""
	}
	return fullResourceName("logging.googleapis.com", path[0]+"/views/"+path[1])
}

func (p bigqueryProvider) PolicyResource(_ context.Context, project string, path []string) string {
	name, _, err := p.iamResource(project, path)
	if err != nil {
		return ""
	}
	return fullResourceName("bigquery.googleapis.com", name)
}

func (spannerProvider) PolicyResource(_ context.Context, project string, path []string) string {
	if project == "" || (len(path) != 1 && len(path) != 2) || isSpannerChildPath(path) {
		return ""
	}
	name := "projects/" + project + "/instances/" + path[0]
	if len(path) == 2 {
		name += "/databases/" + path[1]
	}
	return fullResourceName("spanner.googleapis.com", name)
}

func (p bigtableProvider) PolicyResource(ctx context.Context, project string, path []string) string {
	if len(path) != 1 || project == "" {
		return ""
	}
	inst, err := p.instance(ctx, project)
	if err != nil {
		return ""
	}
	return fullResourceName("bigtableadmin.googleapis.com", "projects/"+project+"/instances/"+inst+"/tables/"+path[0])
}

func (bigtableInstancesProvider) PolicyResource(_ context.Context, project string, path []string) string {
	if len(path) != 1 || project == "" {
		return ""
	}
	return fullResourceName("bigtableadmin.googleapis.com", "projects/"+project+"/instances/"+path[0])
}

var (
	_ console.PolicyResourcer = projectIAMProvider{}
	_ console.PolicyResourcer = hierarchyProvider{}
	_ console.PolicyResourcer = secretsProvider{}
	_ console.PolicyResourcer = kmsProvider{}
	_ console.PolicyResourcer = kmsEkmProvider{}
	_ console.PolicyResourcer = runProvider{}
	_ console.PolicyResourcer = runJobsProvider{}
	_ console.PolicyResourcer = runWorkerPoolsProvider{}
	_ console.PolicyResourcer = runInstancesProvider{}
	_ console.PolicyResourcer = loggingBucketsProvider{}
	_ console.PolicyResourcer = bigqueryProvider{}
	_ console.PolicyResourcer = spannerProvider{}
	_ console.PolicyResourcer = bigtableProvider{}
	_ console.PolicyResourcer = bigtableInstancesProvider{}
)
