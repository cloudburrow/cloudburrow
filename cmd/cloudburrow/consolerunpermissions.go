package main

// The Permissions tab on a Cloud Run service's page and a Cloud Run job's
// (#1216), through the official ServicesClient's and JobsClient's
// GetIamPolicy and SetIamPolicy against the adapter, the calls an SDK makes.
// A revision and an execution have no policy of their own.

import (
	"context"
	"errors"

	"cloud.google.com/go/iam/apiv1/iampb"
	runclient "cloud.google.com/go/run/apiv2"
	"github.com/googleapis/gax-go/v2"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// runIAMClient is the IAM half of the official Cloud Run clients.
type runIAMClient interface {
	GetIamPolicy(context.Context, *iampb.GetIamPolicyRequest, ...gax.CallOption) (*iampb.Policy, error)
	SetIamPolicy(context.Context, *iampb.SetIamPolicyRequest, ...gax.CallOption) (*iampb.Policy, error)
	TestIamPermissions(context.Context, *iampb.TestIamPermissionsRequest, ...gax.CallOption) (*iampb.TestIamPermissionsResponse, error)
}

// clientIAM adapts a client to iamServer.
type clientIAM struct{ c runIAMClient }

func (a clientIAM) GetIamPolicy(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return a.c.GetIamPolicy(ctx, r)
}

func (a clientIAM) SetIamPolicy(ctx context.Context, r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	return a.c.SetIamPolicy(ctx, r)
}

func (a clientIAM) TestIamPermissions(ctx context.Context, r *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return a.c.TestIamPermissions(ctx, r)
}

const runPermissionsLink = compatibilityDoc + "cloud-run--googlecloudrunv2"

// Cloud Run services: a service's page.

func (runProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: runPermissionsLink, RoleHelp: roleHelp("Cloud Run", "roles/run.invoker")}
}

func (p runProvider) withServiceIAM(ctx context.Context, project string, path []string, fn func(iamServer, string) error) error {
	if p.runEndpoint() == "" {
		return errors.New("the Cloud Run adapter is not running")
	}
	if project == "" {
		project = p.defaultProject
	}
	c, err := p.servicesClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return fn(clientIAM{c}, "projects/"+project+"/locations/"+p.location()+"/services/"+path[0])
}

func (p runProvider) GetPolicy(ctx context.Context, project string, path []string) (pol console.Policy, err error) {
	err = p.withServiceIAM(ctx, project, path, func(api iamServer, name string) error {
		pol, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return pol, err
}

func (p runProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.withServiceIAM(ctx, project, path, func(api iamServer, name string) error {
		return setProtoPolicy(ctx, api, name, pol)
	})
}

func (p runProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) (held []string, err error) {
	err = p.withServiceIAM(ctx, project, path, func(api iamServer, name string) error {
		held, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return held, err
}

// Cloud Run jobs: a job's page.

func (runJobsProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: runPermissionsLink, RoleHelp: roleHelp("Cloud Run", "roles/run.invoker")}
}

func (p runJobsProvider) withJobIAM(ctx context.Context, project string, path []string, fn func(iamServer, string) error) error {
	project = p.project(project)
	if project == "" {
		return errors.New("choose a project first")
	}
	return p.withClients(ctx, func(jc *runclient.JobsClient, _ *runclient.ExecutionsClient) error {
		return fn(clientIAM{jc}, p.jobName(project, path[0]))
	})
}

func (p runJobsProvider) GetPolicy(ctx context.Context, project string, path []string) (pol console.Policy, err error) {
	err = p.withJobIAM(ctx, project, path, func(api iamServer, name string) error {
		pol, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return pol, err
}

func (p runJobsProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.withJobIAM(ctx, project, path, func(api iamServer, name string) error {
		return setProtoPolicy(ctx, api, name, pol)
	})
}

func (p runJobsProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) (held []string, err error) {
	err = p.withJobIAM(ctx, project, path, func(api iamServer, name string) error {
		held, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return held, err
}

var _ console.PolicyEditor = runProvider{}
var _ console.PolicyEditor = runJobsProvider{}
