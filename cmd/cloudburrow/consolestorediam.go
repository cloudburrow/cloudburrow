package main

// Permissions tabs (#1486) on a Spanner instance's and database's page, a
// Bigtable table's page and a tag key's and value's page: the policies the
// Spanner and Bigtable fronts and Resource Manager store, not enforced
// (ADR-0006), through the same GetIamPolicy, SetIamPolicy and
// TestIamPermissions their APIs serve.

import (
	"context"

	btadmin "cloud.google.com/go/bigtable/admin/apiv2"
	"cloud.google.com/go/iam/apiv1/iampb"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"github.com/googleapis/gax-go/v2"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// gaxIAM is a generated client's three IAM methods, which take call options.
type gaxIAM interface {
	GetIamPolicy(context.Context, *iampb.GetIamPolicyRequest, ...gax.CallOption) (*iampb.Policy, error)
	SetIamPolicy(context.Context, *iampb.SetIamPolicyRequest, ...gax.CallOption) (*iampb.Policy, error)
	TestIamPermissions(context.Context, *iampb.TestIamPermissionsRequest, ...gax.CallOption) (*iampb.TestIamPermissionsResponse, error)
}

// gaxServer is a gaxIAM as an iamServer.
type gaxServer struct{ c gaxIAM }

func (g gaxServer) GetIamPolicy(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return g.c.GetIamPolicy(ctx, r)
}

func (g gaxServer) SetIamPolicy(ctx context.Context, r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	return g.c.SetIamPolicy(ctx, r)
}

func (g gaxServer) TestIamPermissions(ctx context.Context, r *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return g.c.TestIamPermissions(ctx, r)
}

// --- Spanner: an instance's page and a database's ----------------------

func (spannerProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if (len(path) != 1 && len(path) != 2) || isSpannerChildPath(path) {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "optional-services",
		RoleHelp: roleHelp("Spanner", "roles/spanner.databaseReader"), Conditions: true}
}

// withSpannerIAM calls fn with the instance or database admin client and
// the full name of the instance or database at path.
func (p spannerProvider) withSpannerIAM(ctx context.Context, project string, path []string, fn func(iamServer, string) error) error {
	name := "projects/" + project + "/instances/" + path[0]
	if len(path) == 2 {
		c, err := database.NewDatabaseAdminClient(ctx, localOpts(p.endpoint)...)
		if err != nil {
			return err
		}
		defer c.Close()
		return fn(gaxServer{c}, name+"/databases/"+path[1])
	}
	c, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(gaxServer{c}, name)
}

func (p spannerProvider) GetPolicy(ctx context.Context, project string, path []string) (pol console.Policy, err error) {
	err = p.withSpannerIAM(ctx, project, path, func(api iamServer, name string) error {
		pol, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return pol, err
}

func (p spannerProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.withSpannerIAM(ctx, project, path, func(api iamServer, name string) error {
		return setProtoPolicy(ctx, api, name, pol)
	})
}

func (p spannerProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) (held []string, err error) {
	err = p.withSpannerIAM(ctx, project, path, func(api iamServer, name string) error {
		held, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return held, err
}

// --- Bigtable: a table's page ------------------------------------------
//
// Through the generated table admin client's GetIamPolicy and SetIamPolicy
// at version 3 (#1748), as getProtoPolicy and setProtoPolicy call every
// other service's, so a conditional binding is shown and written back with
// its condition: Bigtable is among the services Google lists as allowing
// conditions. The handwritten client's TableIAM handle reads version 1.

func (bigtableProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "optional-services",
		RoleHelp: roleHelp("Bigtable", "roles/bigtable.reader"), Conditions: true}
}

// withTableIAM calls fn with the table admin API and the full name of the
// table at path, in the instance ctx's request addresses.
func (p bigtableProvider) withTableIAM(ctx context.Context, project string, path []string, fn func(iamServer, string) error) error {
	inst, err := p.instance(ctx, project)
	if err != nil {
		return err
	}
	c, err := btadmin.NewBigtableTableAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(gaxServer{c}, "projects/"+project+"/instances/"+inst+"/tables/"+path[0])
}

func (p bigtableProvider) GetPolicy(ctx context.Context, project string, path []string) (pol console.Policy, err error) {
	err = p.withTableIAM(ctx, project, path, func(api iamServer, name string) error {
		pol, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return pol, err
}

func (p bigtableProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.withTableIAM(ctx, project, path, func(api iamServer, name string) error {
		return setProtoPolicy(ctx, api, name, pol)
	})
}

func (p bigtableProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) (held []string, err error) {
	err = p.withTableIAM(ctx, project, path, func(api iamServer, name string) error {
		held, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return held, err
}

// --- Tags: a tag key's page and a tag value's ----------------------------

func (tagsProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 && len(path) != 2 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "resource-manager--googlecloudresourcemanagerv3-projects-folders-organizations-tags-liens",
		RoleHelp: roleHelp("Resource Manager", "roles/resourcemanager.tagUser")}
}

// withTagIAM calls fn with the TagKeys or TagValues API and the tag's name.
func (p tagsProvider) withTagIAM(path []string, fn func(iamServer, string) error) error {
	if len(path) == 2 {
		return fn(resourcemanager.NewTagValuesServer(p.registry), "tagValues/"+path[1])
	}
	return fn(resourcemanager.NewTagKeysServer(p.registry), "tagKeys/"+path[0])
}

func (p tagsProvider) GetPolicy(ctx context.Context, _ string, path []string) (pol console.Policy, err error) {
	err = p.withTagIAM(path, func(api iamServer, name string) error {
		pol, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return pol, err
}

func (p tagsProvider) SetPolicy(ctx context.Context, _ string, path []string, pol console.Policy) error {
	return p.withTagIAM(path, func(api iamServer, name string) error {
		return setProtoPolicy(ctx, api, name, pol)
	})
}

func (p tagsProvider) TestPermissions(ctx context.Context, _ string, path []string, perms []string) (held []string, err error) {
	err = p.withTagIAM(path, func(api iamServer, name string) error {
		held, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return held, err
}

var (
	_ console.PolicyEditor = spannerProvider{}
	_ console.PolicyEditor = bigtableProvider{}
	_ console.PolicyEditor = tagsProvider{}
)
