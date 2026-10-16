package main

// The Permissions tab on a Pub/Sub topic's page and a subscription's
// (#1367), through the official pubsub/v2 TopicAdminClient's and
// SubscriptionAdminClient's GetIamPolicy, SetIamPolicy and
// TestIamPermissions against the Pub/Sub front, which stores the policies
// and enforces none (ADR-0006, internal/pubsubfront/iam.go). A subscription
// row on the topic page has no page, so no tab either.

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const pubsubPermissionsLink = compatibilityDoc + "pubsub--googlepubsubv1"

// pubsubIAM runs fn with the IAM half of the official client for a
// resource of collection ("topics" or "subscriptions") in project.
func (p pubsubProvider) pubsubIAM(ctx context.Context, project, collection, name string, fn func(iamServer) error) error {
	if project == "" {
		return fmt.Errorf("choose a project in the toolbar")
	}
	if !strings.HasPrefix(name, "projects/"+project+"/"+collection+"/") {
		return fmt.Errorf("%s is not one of project %s's %s", name, project, collection)
	}
	c, err := p.client(ctx, project)
	if err != nil {
		return fmt.Errorf("connect to Pub/Sub: %w", err)
	}
	defer func() { _ = c.Close() }()
	if collection == "topics" {
		return fn(clientIAM{c.TopicAdminClient})
	}
	return fn(clientIAM{c.SubscriptionAdminClient})
}

func pubsubPolicyOn(path []string, collection, example string) *console.PolicyTarget {
	if len(path) != 1 || !strings.Contains(path[0], "/"+collection+"/") {
		return nil
	}
	return &console.PolicyTarget{Link: pubsubPermissionsLink, RoleHelp: roleHelp("Pub/Sub", example)}
}

func (pubsubProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	return pubsubPolicyOn(path, "topics", "roles/pubsub.publisher")
}

func (p pubsubProvider) GetPolicy(ctx context.Context, project string, path []string) (out console.Policy, err error) {
	err = p.pubsubIAM(ctx, project, "topics", path[0], func(api iamServer) error {
		out, err = getProtoPolicy(ctx, api, path[0])
		return err
	})
	return out, err
}

func (p pubsubProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.pubsubIAM(ctx, project, "topics", path[0], func(api iamServer) error {
		return setProtoPolicy(ctx, api, path[0], pol)
	})
}

func (p pubsubProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) (out []string, err error) {
	err = p.pubsubIAM(ctx, project, "topics", path[0], func(api iamServer) error {
		out, err = testProtoPermissions(ctx, api, path[0], perms)
		return err
	})
	return out, err
}

// Subscriptions: a subscription's page.

func (pubsubSubscriptionsProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	return pubsubPolicyOn(path, "subscriptions", "roles/pubsub.subscriber")
}

func (p pubsubSubscriptionsProvider) GetPolicy(ctx context.Context, project string, path []string) (out console.Policy, err error) {
	err = p.topics().pubsubIAM(ctx, project, "subscriptions", path[0], func(api iamServer) error {
		out, err = getProtoPolicy(ctx, api, path[0])
		return err
	})
	return out, err
}

func (p pubsubSubscriptionsProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.topics().pubsubIAM(ctx, project, "subscriptions", path[0], func(api iamServer) error {
		return setProtoPolicy(ctx, api, path[0], pol)
	})
}

func (p pubsubSubscriptionsProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) (out []string, err error) {
	err = p.topics().pubsubIAM(ctx, project, "subscriptions", path[0], func(api iamServer) error {
		out, err = testProtoPermissions(ctx, api, path[0], perms)
		return err
	})
	return out, err
}
