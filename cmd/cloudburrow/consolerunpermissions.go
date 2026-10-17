package main

// The Permissions tab on a Cloud Run service's page and a Cloud Run job's
// (#1216), through the official ServicesClient's and JobsClient's
// GetIamPolicy and SetIamPolicy against the adapter, the calls an SDK makes;
// a worker pool's (#1300) through the WorkerPoolsClient's; and an
// instance's (#1556) through the v1 client, as v2's Instances has no IAM
// methods. A revision and an execution have no policy of their own.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"cloud.google.com/go/iam/apiv1/iampb"
	runclient "cloud.google.com/go/run/apiv2"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/option"
	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/genproto/googleapis/type/expr"

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

// Cloud Run worker pools: a pool's page (#1300).

func (runWorkerPoolsProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: runPermissionsLink, RoleHelp: roleHelp("Cloud Run", "roles/run.developer")}
}

func (p runWorkerPoolsProvider) withPoolIAM(ctx context.Context, project string, path []string, fn func(iamServer, string) error) error {
	project = p.jobs().project(project)
	if project == "" {
		return errors.New("choose a project first")
	}
	return p.withClients(ctx, func(wc *runclient.WorkerPoolsClient, _ *runclient.RevisionsClient) error {
		return fn(clientIAM{wc}, p.poolName(project, path[0]))
	})
}

func (p runWorkerPoolsProvider) GetPolicy(ctx context.Context, project string, path []string) (pol console.Policy, err error) {
	err = p.withPoolIAM(ctx, project, path, func(api iamServer, name string) error {
		pol, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return pol, err
}

func (p runWorkerPoolsProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.withPoolIAM(ctx, project, path, func(api iamServer, name string) error {
		return setProtoPolicy(ctx, api, name, pol)
	})
}

func (p runWorkerPoolsProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) (held []string, err error) {
	err = p.withPoolIAM(ctx, project, path, func(api iamServer, name string) error {
		held, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return held, err
}

var _ console.PolicyEditor = runWorkerPoolsProvider{}

// Cloud Run instances: an instance's page (#1556). v2's Instances API has
// no IAM methods, so the tab reads and writes the policy through Google's
// generated v1 client (google.golang.org/api/run/v1): the
// projects.locations.instances getIamPolicy, setIamPolicy and
// testIamPermissions that store it beside the instance.

const runInstancePermissionsLink = compatibilityDoc + "cloud-run-worker-pools-and-instances"

func (runInstancesProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: runInstancePermissionsLink, RoleHelp: roleHelp("Cloud Run", "roles/run.invoker")}
}

// v1InstanceIAM is the v1 client's instance IAM methods as iamServer, the
// v1 JSON policy translated to and from iampb's.
type v1InstanceIAM struct{ c *runv1.APIService }

func (i v1InstanceIAM) GetIamPolicy(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	p, err := i.c.Projects.Locations.Instances.GetIamPolicy(r.GetResource()).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return v1PolicyToProto(p)
}

func (i v1InstanceIAM) SetIamPolicy(ctx context.Context, r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	p, err := i.c.Projects.Locations.Instances.SetIamPolicy(r.GetResource(),
		&runv1.SetIamPolicyRequest{Policy: v1PolicyFromProto(r.GetPolicy())}).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return v1PolicyToProto(p)
}

func (i v1InstanceIAM) TestIamPermissions(ctx context.Context, r *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	resp, err := i.c.Projects.Locations.Instances.TestIamPermissions(r.GetResource(),
		&runv1.TestIamPermissionsRequest{Permissions: r.GetPermissions()}).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return &iampb.TestIamPermissionsResponse{Permissions: resp.Permissions}, nil
}

// v1PolicyToProto is a v1 JSON policy as iampb's, its etag decoded from the
// base64 the JSON API spells it in.
func v1PolicyToProto(p *runv1.Policy) (*iampb.Policy, error) {
	etag, err := base64.StdEncoding.DecodeString(p.Etag)
	if err != nil {
		return nil, fmt.Errorf("the policy's etag %q is not base64: %w", p.Etag, err)
	}
	out := &iampb.Policy{Version: int32(p.Version), Etag: etag}
	for _, b := range p.Bindings {
		pb := &iampb.Binding{Role: b.Role, Members: b.Members}
		if c := b.Condition; c != nil {
			pb.Condition = &expr.Expr{Title: c.Title, Expression: c.Expression, Description: c.Description, Location: c.Location}
		}
		out.Bindings = append(out.Bindings, pb)
	}
	return out, nil
}

// v1PolicyFromProto is the v1 JSON form of a policy to set.
func v1PolicyFromProto(p *iampb.Policy) *runv1.Policy {
	out := &runv1.Policy{Version: int64(p.GetVersion()), Etag: base64.StdEncoding.EncodeToString(p.GetEtag())}
	for _, b := range p.GetBindings() {
		out.Bindings = append(out.Bindings, &runv1.Binding{Role: b.GetRole(), Members: b.GetMembers()})
	}
	return out
}

func (p runInstancesProvider) withInstanceIAM(ctx context.Context, project string, path []string, fn func(iamServer, string) error) error {
	project = p.jobs().project(project)
	if project == "" {
		return errors.New("choose a project first")
	}
	addr := p.jobs().endpoint()
	if addr == "" {
		return errors.New("the Cloud Run adapter is not running")
	}
	c, err := runv1.NewService(ctx, option.WithEndpoint("http://"+addr+"/"), option.WithoutAuthentication())
	if err != nil {
		return fmt.Errorf("connect to Cloud Run: %w", err)
	}
	return fn(v1InstanceIAM{c}, p.instanceName(project, path[0]))
}

func (p runInstancesProvider) GetPolicy(ctx context.Context, project string, path []string) (pol console.Policy, err error) {
	err = p.withInstanceIAM(ctx, project, path, func(api iamServer, name string) error {
		pol, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return pol, err
}

func (p runInstancesProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.withInstanceIAM(ctx, project, path, func(api iamServer, name string) error {
		return setProtoPolicy(ctx, api, name, pol)
	})
}

func (p runInstancesProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) (held []string, err error) {
	err = p.withInstanceIAM(ctx, project, path, func(api iamServer, name string) error {
		held, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return held, err
}

var _ console.PolicyEditor = runInstancesProvider{}
